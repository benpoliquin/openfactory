import { commandSchema, reviewSchema, type Command, type Review, type Session } from "./intake-contract.ts";
import { isAlias, parseDocument, visit } from "yaml";

export class IntakeReviewError extends Error {
  readonly code: "invalid_contract" | "stale_policy" | "stale_subject" | "challenge_expired";
  constructor(code: "invalid_contract" | "stale_policy" | "stale_subject" | "challenge_expired", message: string) { super(message); this.code = code; this.name = "IntakeReviewError"; }
}
function requireValue(ok: unknown, message: string): asserts ok { if (!ok) throw new IntakeReviewError("invalid_contract", message); }
function object(value: unknown): Record<string, unknown> { requireValue(value !== null && typeof value === "object" && !Array.isArray(value), "Expected review object"); return Object.fromEntries(Object.entries(value)); }
function text(value: unknown): string { requireValue(typeof value === "string" && value.length > 0, "Missing review string"); return value; }
function list(value: unknown): unknown[] { requireValue(Array.isArray(value), "Missing review array"); return value; }
function equal(a: unknown, b: unknown): boolean {
  if (a === b) return true;
  if (Array.isArray(a) || Array.isArray(b)) return Array.isArray(a) && Array.isArray(b) && a.length === b.length && a.every((v, i) => equal(v, b[i]));
  if (a === null || b === null || typeof a !== "object" || typeof b !== "object") return false;
  const x = object(a), y = object(b), keys = Object.keys(x);
  return keys.length === Object.keys(y).length && keys.every(k => Object.hasOwn(y, k) && equal(x[k], y[k]));
}
function same(a: unknown, b: unknown, label: string): void { requireValue(equal(a, b), `${label} differs from bound review`); }
export async function canonicalSHA256(raw: string): Promise<string> { return Array.from(new Uint8Array(await crypto.subtle.digest("SHA-256", new TextEncoder().encode(raw))), b => b.toString(16).padStart(2, "0")).join(""); }
function parse(raw: string): unknown {
  let decoded: unknown;
  try { decoded = JSON.parse(raw); } catch { throw new IntakeReviewError("invalid_contract", "Invalid canonical JSON"); }
  // JSON.parse silently overwrites duplicate keys. Scan the already-valid JSON
  // tokens as well, so ambiguous material never reaches the displayed contract.
  const tokens = raw.match(/"(?:[^"\\]|\\.)*"|[{}\[\]:,]|-?(?:0|[1-9]\d*)(?:\.\d+)?(?:[eE][+-]?\d+)?|true|false|null/g) ?? [];
  let index = 0;
  function visit(): void {
    const token = tokens[index++];
    if (token === "{") {
      const keys = new Set<string>();
      while (tokens[index] !== "}") {
        const key: unknown = JSON.parse(tokens[index++]);
        requireValue(typeof key === "string" && !keys.has(key), "Duplicate JSON key");
        keys.add(key); index++; visit();
        if (tokens[index] !== ",") break;
        index++;
      }
      index++;
    } else if (token === "[") {
      while (tokens[index] !== "]") { visit(); if (tokens[index] !== ",") break; index++; }
      index++;
    }
  }
  visit();
  return decoded;
}
const manifestKeys = new Set("command_sha source_sha canonical_sha domain_revision request draft turns questions feature program program_revision answers seed_manifest artifacts planning_questions planning_answers selected_artifact selected_experiment sources evidence bundle_manifest_shas policy_sha execution_policy_sha resume_work".split(" "));
const immutableFeatureKeys = "id project title kind risk priority depends_on completion_target checks acceptance body discovery_step_id program_evidence_gate intake_step_id revision".split(" ");
function scope(project: unknown, session: Session): void { requireValue(session.project_scope.includes(text(project)), "Project outside session scope"); }
function optionalArray(value: unknown): unknown { return value === undefined || value === null ? [] : value; }

function featureSource(raw: string): Record<string, unknown> {
  requireValue(new TextEncoder().encode(raw).length <= 256 * 1024, "Feature source exceeds review bound");
  const normalized = raw.replace(/\r\n/g, "\n");
  requireValue(normalized.startsWith("---\n"), "Missing Feature front matter");
  const end = normalized.indexOf("\n---\n", 4);
  requireValue(end >= 0, "Unterminated Feature front matter");
  const document = parseDocument(normalized.slice(4, end), {version:"1.2",schema:"core",uniqueKeys:true,strict:true});
  requireValue(document.errors.length === 0, "Invalid Feature YAML");
  visit(document, (_key, node) => {
    requireValue(!isAlias(node), "YAML aliases forbidden");
    if (node && typeof node === "object") requireValue(!("anchor" in node && node.anchor) && !("tag" in node && node.tag), "YAML anchors and tags forbidden");
  });
  const source = object(document.toJS({maxAliasCount:0}));
  function rejectPrototypeKeys(value: unknown): void {
    if (value === null || typeof value !== "object") return;
    for (const [key, nested] of Object.entries(value)) { requireValue(!["__proto__", "prototype", "constructor"].includes(key), "Prototype YAML key forbidden"); rejectPrototypeKeys(nested); }
  }
  rejectPrototypeKeys(source);
  const fields = new Set("id project title kind risk priority depends_on completion_target checks acceptance program_evidence_gate".split(" "));
  requireValue(Object.keys(source).every(k => fields.has(k)), "Unknown Feature source field");
  for (const key of ["id", "project", "title"]) text(source[key]);
  for (const key of ["kind", "risk", "completion_target"]) requireValue(source[key] === undefined || source[key] === null || typeof source[key] === "string", `Invalid Feature ${key}`);
  source.kind = source.kind || "feature"; source.risk = source.risk || "normal"; source.completion_target = source.completion_target || "integrated"; source.priority ??= 0;
  requireValue(Number.isSafeInteger(source.priority), "Invalid Feature priority");
  for (const key of ["depends_on", "checks"]) { source[key] ??= null; requireValue(source[key] === null || list(source[key]).every(v => typeof v === "string"), `Invalid Feature ${key}`); }
  source.acceptance ??= null;
  if (source.acceptance !== null) for (const item of list(source.acceptance)) { const acceptance = object(item); requireValue(Object.keys(acceptance).every(k => ["id", "scenario", "expected"].includes(k)), "Unknown acceptance field"); for (const key of ["id", "scenario", "expected"]) text(acceptance[key]); }
  if (source.program_evidence_gate !== undefined && source.program_evidence_gate !== null) {
    const gate = object(source.program_evidence_gate);
    requireValue(Object.keys(gate).every(k => ["program_id", "revision", "manifest_sha", "evaluation_artifact_id"].includes(k)), "Unknown Feature evidence gate field");
    text(gate.program_id); text(gate.evaluation_artifact_id);
    requireValue(Number.isSafeInteger(gate.revision) && Number(gate.revision) > 0 && /^[a-f0-9]{64}$/.test(text(gate.manifest_sha)), "Invalid Feature evidence gate");
  }
  if (source.program_evidence_gate === null) delete source.program_evidence_gate;
  source.body = normalized.slice(end + 5).replace(/^[\u0009-\u000d\u0020\u0085\u00a0\u1680\u2000-\u200a\u2028\u2029\u202f\u205f\u3000]+|[\u0009-\u000d\u0020\u0085\u00a0\u1680\u2000-\u200a\u2028\u2029\u202f\u205f\u3000]+$/g, "");
  return source;
}

function normalizedProgramSource(source: unknown, normalized: unknown, path = ""): void {
  if (path.startsWith("limits.") && (source === 0 || source === undefined)) {
    requireValue(typeof normalized === "number" && Number.isSafeInteger(normalized) && normalized >= 0, "Invalid normalized Program limit");
    return;
  }
  if (source === undefined || source === null) {
    if (normalized !== null && typeof normalized === "object" && !Array.isArray(normalized)) {
      for (const [key, value] of Object.entries(normalized)) normalizedProgramSource(undefined, value, path ? `${path}.${key}` : key);
    } else requireValue(normalized === undefined || normalized === null || normalized === "" || normalized === false || normalized === 0, "Program source omits non-default material");
    return;
  }
  if (typeof source === "object" && !Array.isArray(source)) {
    const original = object(source), target = object(normalized);
    requireValue(Object.keys(original).every(key => Object.hasOwn(target, key)), "Unknown Program source field");
    for (const key of Object.keys(target)) normalizedProgramSource(original[key], target[key], path ? `${path}.${key}` : key);
    return;
  }
  same(source, normalized, "Normalized Program source");
}

/** Validate exact server-hashed bytes before any native credential operation. Server verification remains authoritative. */
export async function validateReview(review: Review, command: Command, session: Session, expectedPolicySHA?: string): Promise<Review> {
  const result = reviewSchema.safeParse(review);
  requireValue(result.success, "Incomplete review contract");
  const r = object(result.data), c = object(r.claims), p = object(command.payload);
  commandSchema.parse(command);
  const claimsRaw = text(r.claims_canonical), manifestRaw = text(r.manifest_canonical), commandRaw = text(r.command_canonical), payloadRaw = text(r.canonical_payload);
  same(await canonicalSHA256(claimsRaw), r.claims_sha, "Claims hash");
  same(await canonicalSHA256(manifestRaw), c.review_manifest_sha, "Manifest hash");
  same(await canonicalSHA256(payloadRaw), c.canonical_payload_sha, "Payload hash");
  same(parse(claimsRaw), c, "Claims");
  const m = object(parse(manifestRaw));
  requireValue(Object.keys(m).every(k => manifestKeys.has(k)), "Unknown manifest field");
  same(await canonicalSHA256(commandRaw), m.command_sha, "Command hash");
  const boundCommand = commandSchema.parse(parse(commandRaw));
  same(boundCommand, r.command, "Displayed command"); same(boundCommand, command, "Requested command");
  same(c.version, 1, "Claims version");
  const requiredClaims = "version installation_id operator_id credential_id action subject_kind subject_id subject_revision canonical_payload_sha review_manifest_sha policy_sha evidence bundle_manifest_shas command_id challenge_id issued_at expires_at".split(" ");
  const allowedClaims = new Set([...requiredClaims, "request_id", "content_version", "draft_revision", "input_manifest_sha", "execution_policy_sha"]);
  requireValue(requiredClaims.every(k => Object.hasOwn(c, k)) && Object.keys(c).every(k => allowedClaims.has(k)), "Incomplete or unknown claims");
  for (const key of ["canonical_payload_sha", "review_manifest_sha", "policy_sha"]) requireValue(/^[a-f0-9]{64}$/.test(text(c[key])), `Invalid ${key}`);
  for (const key of ["command_sha", "source_sha", "canonical_sha", "domain_revision", "evidence", "bundle_manifest_shas", "policy_sha"]) requireValue(Object.hasOwn(m, key), `Missing manifest ${key}`);
  for (const key of ["installation_id", "operator_id", "credential_id"] as const) same(c[key], text(session[key]), key);
  same(c.action, command.action, "Action"); same(c.command_id, command.command_id, "Command ID"); text(c.challenge_id); text(r.id);
  const issued = Date.parse(text(c.issued_at)), expires = Date.parse(text(c.expires_at)), now = Date.now();
  if (!Number.isFinite(issued) || !Number.isFinite(expires) || expires <= issued || expires - issued > 120000 || issued > now + 5000 || expires <= now) throw new IntakeReviewError("challenge_expired", "Review is not fresh");
  same(c.policy_sha, text(m.policy_sha), "Policy");
  if (expectedPolicySHA !== undefined && c.policy_sha !== expectedPolicySHA) throw new IntakeReviewError("stale_policy", "Project policy changed");
  const evidence = list(m.evidence), bundles = list(m.bundle_manifest_shas);
  same(c.evidence, evidence, "Evidence"); same(c.bundle_manifest_shas, bundles, "Bundles");
  let previous: {key: string; revision: number} | undefined;
  for (const value of evidence) {
    const ref = object(value);
    requireValue(Object.keys(ref).every(k => ["kind", "id", "sha", "revision"].includes(k)), "Unknown evidence field");
    const key = `${text(ref.kind)}\u0000${text(ref.id)}\u0000${text(ref.sha)}`, revision = ref.revision ?? 0;
    requireValue(/^[a-f0-9]{64}$/.test(text(ref.sha)) && typeof revision === "number" && Number.isSafeInteger(revision) && revision >= 0, "Invalid evidence identity");
    requireValue(!previous || previous.key < key || (previous.key === key && previous.revision < revision), "Evidence must be sorted and unique");
    previous = {key, revision};
  }
  requireValue(bundles.every((v, i) => typeof v === "string" && /^[a-f0-9]{64}$/.test(v) && (i === 0 || String(bundles[i - 1]) < v)), "Bundles must be sorted and unique");
  for (const key of ["program_revision", "seed_manifest", "resume_work"]) same(r[key], m[key], key);
  if (command.action !== "program.resume") requireValue(m.resume_work === undefined, "Unexpected resume work");
  const resumeExecution = command.action === "program.resume" && m.resume_work !== undefined && object(m.resume_work).step_kind === "experiment_run";
  if (command.action === "program.resume") {
    const program = object(m.program);
    requireValue(program.state === "active" || program.state === "waiting", "Program is not resumable");
    same(program.wait, {code:"prerequisite_missing",detail:m.resume_work === undefined ? "No failed worker is selected; resume continues only previously approved Program work and prerequisites." : "One current settled failed worker is selected for retry under its existing authority."}, "Safe resume presentation");
    if (m.resume_work === undefined) requireValue(c.input_manifest_sha === undefined, "Prerequisite-only resume has unexpected input authority");
  }
  if (m.resume_work !== undefined) {
    const work = object(m.resume_work), program = object(m.program), revision = object(m.program_revision);
    const required = "step_id step_revision step_kind step_state iteration input_manifest_sha feature_id feature_revision feature_state feature_attempts feature_max_attempts attempt_id attempt_state attempt_stage attempt_active attempt_output_sha attempt_updated_at".split(" ");
    const priorFields = ["prior_run_id", "prior_request_sha", "prior_receipt_sha", "prior_receipt_state", "prior_receipt_conclusive", "prior_execution_outcome", "prior_observation_outcome", "prior_hypothesis_outcome"];
    const executionFields = ["subject_id", "execution_policy_sha", "bundle_manifest_sha", "prior_launch_absent", ...priorFields];
    const allowed = new Set([...required, ...executionFields]);
    requireValue(required.every(key => Object.hasOwn(work, key)) && Object.keys(work).every(key => allowed.has(key)), "Incomplete or unknown resume work");
    same(work.step_revision, revision.number, "Resume step revision");
    requireValue(Number.isSafeInteger(program.iteration) && Number(work.iteration) >= Number(program.iteration), "Resume iteration predates current Program");
    same(work.feature_id, `openfactory-discovery-${text(work.step_id)}`, "Resume internal Feature ID");
    same(work.attempt_active, false, "Resume attempt ownership");
    text(work.attempt_id);
    requireValue(work.step_state === "ready" || work.step_state === "blocked", "Resume step is not retryable");
    same(work.feature_state, "blocked", "Resume Feature state");
    requireValue(
      (work.attempt_state === "blocked" && work.attempt_stage === "failed") ||
      (work.step_kind === "experiment_run" && work.attempt_state === "failed" && work.attempt_stage === "operator_reconciled_failure"),
      "Resume latest attempt is not a settled failure",
    );
    requireValue(["research", "architecture", "plan_evaluation", "experiment_run", "result_evaluation", "contracts", "contract_evaluation"].includes(text(work.step_kind)), "Unknown resume step kind");
    requireValue(Number.isSafeInteger(work.feature_max_attempts) && Number(work.feature_attempts) >= 1 && Number(work.feature_max_attempts) > Number(work.feature_attempts), "Resume Feature attempt allowance exhausted");
    for (const key of ["input_manifest_sha", "feature_revision", "attempt_output_sha"]) requireValue(/^[a-f0-9]{64}$/.test(text(work[key])), `Invalid resume ${key}`);
    for (const key of ["iteration", "feature_attempts"]) requireValue(typeof work[key] === "number" && Number.isSafeInteger(work[key]) && Number(work[key]) >= 0, `Invalid resume ${key}`);
    requireValue(Number.isSafeInteger(work.step_revision) && Number(work.step_revision) > 0 && Number.isFinite(Date.parse(text(work.attempt_updated_at))), "Invalid resume revision or timestamp");
    same(c.input_manifest_sha, work.input_manifest_sha, "Resume input manifest");
    requireValue(evidence.some(value => equal(value, {kind:"program_revision",id:c.subject_revision,sha:c.subject_revision,revision:work.step_revision})), "Resume input lacks current Program revision evidence");
    if (!resumeExecution) requireValue(executionFields.every(key => work[key] === undefined), "Unexpected resume execution metadata");
    else {
      for (const key of ["subject_id", "execution_policy_sha", "bundle_manifest_sha"]) requireValue(/^[a-f0-9]{64}$/.test(text(work[key])), `Invalid execution resume ${key}`);
      same(work.execution_policy_sha, c.execution_policy_sha, "Resume claims execution policy");
      same(work.execution_policy_sha, m.execution_policy_sha, "Resume manifest execution policy");
      requireValue(bundles.includes(work.bundle_manifest_sha), "Resume experiment bundle missing");
      requireValue(evidence.some(value => equal(value, {kind:"bundle",id:work.bundle_manifest_sha,sha:work.bundle_manifest_sha,revision:work.step_revision})), "Resume exact bundle missing from evidence");
      requireValue(work.prior_launch_absent === undefined || typeof work.prior_launch_absent === "boolean", "Invalid prior launch absence proof");
      if (work.prior_launch_absent === true) {
        requireValue(priorFields.every(key => work[key] === undefined), "No-launch proof contradicts prior runner receipt");
        same(work.attempt_state, "blocked", "No-launch attempt state");
        same(work.attempt_stage, "failed", "No-launch attempt stage");
      } else {
        text(work.prior_run_id);
        for (const key of ["prior_request_sha", "prior_receipt_sha"]) requireValue(/^[a-f0-9]{64}$/.test(text(work[key])), `Invalid resume ${key}`);
        requireValue(work.prior_receipt_conclusive === undefined || typeof work.prior_receipt_conclusive === "boolean", "Invalid resume receipt conclusion");
        requireValue(["failed", "cancelled", "resolved_failed", "waiting_prerequisite", "completed"].includes(text(work.prior_receipt_state)) && work.prior_receipt_conclusive !== true, "Resume runner receipt is not a retryable terminal state");
        if (work.attempt_stage === "operator_reconciled_failure") same(work.prior_receipt_state, "resolved_failed", "Resume reconciled runner receipt");
        if (work.prior_receipt_state === "resolved_failed") requireValue(work.attempt_state === "failed" && work.attempt_stage === "operator_reconciled_failure", "Resolved runner receipt lacks reconciled attempt");
        requireValue(["pending", "running", "completed", "nonzero", "infrastructure", "prerequisite_missing", "cancelled", "uncertain"].includes(text(work.prior_execution_outcome)), "Invalid prior execution outcome");
        requireValue(["pending", "complete", "incomplete", "truncated", "not_evaluated"].includes(text(work.prior_observation_outcome)), "Invalid prior observation outcome");
        requireValue(["pending", "inconclusive"].includes(text(work.prior_hypothesis_outcome)), "Prior hypothesis is already conclusive");
        requireValue(evidence.some(value => equal(value, {kind:"experiment_receipt",id:work.prior_run_id,sha:work.prior_receipt_sha,revision:work.step_revision})), "Prior runner receipt missing from evidence");
      }
    }
  }
  for (const key of ["answers", "artifacts"]) same(optionalArray(r[key]), optionalArray(m[key]), key);
  if (m.seed_manifest !== undefined) { const seed = object(m.seed_manifest); requireValue(bundles.includes(text(seed.manifest_sha)), "Seed bundle not bound"); for (const value of list(seed.files)) { const file = object(value); text(file.path); requireValue(/^[a-f0-9]{64}$/.test(text(file.content_sha)) && Number.isSafeInteger(file.bytes) && Number(file.bytes) >= 0, "Invalid seed file"); } }
  const source = r.canonical_source ?? ""; requireValue(typeof source === "string", "Invalid source");
  if (source) same(await canonicalSHA256(source), m.source_sha, "Exact source hash");
  const requestAction = command.action === "intake.retry" || command.action === "intake.approve_queue";
  if (requestAction) {
    const request = object(m.request); same(request.id, command.request_id, "Request ID"); same(c.request_id, request.id, "Claims request"); same(request.operator_id, session.operator_id, "Request operator"); scope(request.project, session);
    same(request.content_version, command.expected_content_version, "Request version"); same(c.content_version, request.content_version, "Claims content version");
    const context = object(request.context); same(c.input_manifest_sha, context.sha === "" ? undefined : context.sha, "Request input manifest");
    if (command.action === "intake.retry") { same(c.subject_kind, "intake", "Retry kind"); same(c.subject_id, request.id, "Retry subject"); same(c.subject_revision, c.canonical_payload_sha, "Retry revision"); same(parse(payloadRaw), request, "Retry content"); requireValue(!m.draft && !r.feature && !r.program_revision && !source, "Unexpected retry material"); }
    else {
      const draft = object(m.draft); same(draft.request_id, request.id, "Draft request"); same(draft.content_version, request.content_version, "Draft content version"); same(draft.revision, p.draft_revision, "Draft revision"); same(c.draft_revision, draft.revision, "Claims draft revision"); same(request.latest_draft_revision, draft.revision, "Current draft");
      for (const key of ["source_sha", "domain_revision"]) same(draft[key], p[key], `Draft ${key}`);
      same(draft.policy_sha, c.policy_sha, "Draft policy"); same(draft.canonical_source, source, "Draft source"); same(draft.source_sha, m.source_sha, "Draft source hash"); same(draft.canonical_sha, m.canonical_sha, "Draft canonical hash"); same(draft.canonical_sha, c.canonical_payload_sha, "Draft payload"); same(draft.domain_revision, c.subject_revision, "Draft domain"); same(c.subject_kind, draft.route, "Draft route"); same(optionalArray(r.findings), optionalArray(draft.findings), "Findings");
    }
  } else { requireValue(!m.request && !m.draft && c.request_id === undefined && c.content_version === undefined && c.draft_revision === undefined, "Unexpected request binding"); same(c.subject_id, p.id ?? p.program_id, "Subject ID"); same(c.subject_revision, p.revision, "Subject revision"); same(c.subject_kind, p.kind ?? "program", "Subject kind"); }
  if (command.action !== "intake.retry") {
    text(source);
    same(m.domain_revision, c.subject_revision, "Domain revision");
    if (c.subject_kind === "feature" || c.subject_kind === "plan") {
      requireValue(command.action === "intake.approve_queue" || command.action === "feature.approve" || command.action === "subject.retry", "Invalid feature action");
      requireValue(!m.program && !r.program_revision && !m.program_revision, "Unexpected program material");
      const f = object(r.feature), mf = object(m.feature); for (const key of immutableFeatureKeys) same(f[key], mf[key], `Feature ${key}`);
      const canonicalFeature = featureSource(payloadRaw), originalFeature = featureSource(source);
      same(originalFeature, canonicalFeature, "Normalized Feature source");
      for (const key of Object.keys(canonicalFeature)) same(canonicalFeature[key], f[key] === undefined && ["depends_on", "checks", "acceptance"].includes(key) ? null : f[key], `Canonical Feature ${key}`);
      same(canonicalFeature.program_evidence_gate, f.program_evidence_gate, "Canonical Feature evidence gate");
      requireValue(!f.discovery_step_id && !f.intake_step_id, "Internal Feature cannot be granted");
      if (command.action !== "intake.approve_queue") same(f, mf, "Feature");
      same(f.id, c.subject_id, "Feature ID"); same(f.revision, c.subject_revision, "Feature revision"); same(f.kind === "plan" ? "plan" : "feature", c.subject_kind, "Feature route"); scope(f.project, session);
      if (m.request) same(f.project, object(m.request).project, "Feature project");
      if (command.action === "subject.retry") same(f.policy_hash, c.policy_sha, "Retry policy");
      same(c.canonical_payload_sha, m.canonical_sha, "Feature payload hash"); if (command.action !== "intake.approve_queue") same(payloadRaw, source, "Feature canonical source");
      if (!requestAction && f.program_evidence_gate) same(c.input_manifest_sha, object(f.program_evidence_gate).manifest_sha, "Feature evidence input");
    } else {
      same(c.subject_kind, "program", "Program kind"); requireValue(!r.feature && !m.feature, "Unexpected feature material");
      const revision = object(r.program_revision), spec = object(revision.spec); same(revision.program_id, c.subject_id, "Program ID"); same(spec.id, c.subject_id, "Spec ID"); same(revision.revision_hash, c.subject_revision, "Program revision"); scope(spec.project, session);
      requireValue(Number.isSafeInteger(revision.number) && Number(revision.number) > 0, "Invalid Program revision number");
      if (revision.seed_manifest_sha) same(revision.seed_manifest_sha, object(m.seed_manifest).manifest_sha, "Program revision seed");
      if (command.action === "intake.approve_queue") normalizedProgramSource(parse(source), spec);
      if (m.request) same(spec.project, object(m.request).project, "Program request project");
      if (m.program) { const program = object(m.program); same(program.id, c.subject_id, "Current program ID"); same(program.project, spec.project, "Program project"); same(program.current_revision, revision.number, "Current program revision"); }
      else requireValue(command.action === "intake.approve_queue", "Missing current program");
      const artifactAction = command.action === "program.approve_architecture" || command.action === "program.approve_experiment";
      if (artifactAction) {
        const selected = object(m.selected_artifact), raw = text(r.selected_artifact_canonical);
        same(selected.kind, command.action === "program.approve_architecture" ? "architecture" : "experiment_contracts", "Action artifact kind");
        same(selected.id, p.artifact_id, "Artifact ID"); same(selected.kind, p.artifact_kind, "Artifact kind"); same(selected.content_sha, p.artifact_sha, "Artifact hash"); same(selected.program_id, c.subject_id, "Artifact program"); same(selected.revision, revision.number, "Artifact revision"); same(await canonicalSHA256(raw), selected.content_sha, "Exact artifact hash"); same(c.input_manifest_sha, selected.input_manifest_sha, "Artifact input");
        requireValue(evidence.some(v => equal(v, {kind:selected.kind,id:selected.id,sha:selected.content_sha,revision:selected.revision})), "Selected artifact missing from evidence");
        same(await canonicalSHA256(source), m.canonical_sha, "Program source canonical hash"); same(parse(source), spec, "Program source spec"); same(p.subject_sha, c.canonical_payload_sha, "Selected subject hash");
        if (command.action === "program.approve_architecture") { same(payloadRaw, raw, "Architecture bytes"); same(p.subject_sha, p.artifact_sha, "Architecture subject"); requireValue(!m.selected_experiment, "Unexpected experiment"); parse(raw); }
        else { const experiment = object(m.selected_experiment); same(parse(payloadRaw), experiment, "Selected experiment"); const contracts = object(parse(raw)); requireValue(list(contracts.experiments).filter(v => equal(v, experiment)).length === 1, "Experiment not uniquely present in artifact"); requireValue(bundles.includes(text(experiment.bundle_manifest_sha)), "Experiment bundle missing"); same(text(c.execution_policy_sha), text(m.execution_policy_sha), "Execution policy"); }
      } else {
        requireValue(["intake.approve_queue", "program.approve_revision", "program.resume", "subject.retry"].includes(command.action), "Invalid program action");
        if (!resumeExecution) {
          same(parse(payloadRaw), spec, "Program spec"); same(c.canonical_payload_sha, m.canonical_sha, "Program payload");
          if (command.action !== "intake.approve_queue") same(payloadRaw, source, "Program canonical source");
        }
        if (resumeExecution) {
          const work = object(m.resume_work), artifact = object(m.selected_artifact), experiment = object(m.selected_experiment), raw = text(r.selected_artifact_canonical);
          same(artifact.kind, "experiment_contracts", "Resume artifact kind");
          same(artifact.program_id, c.subject_id, "Resume artifact Program");
          same(artifact.revision, revision.number, "Resume artifact revision");
          text(artifact.id); same(artifact.iteration, work.iteration, "Resume artifact iteration");
          same(await canonicalSHA256(raw), artifact.content_sha, "Resume exact artifact hash");
          requireValue(evidence.some(value => equal(value, {kind:artifact.kind,id:artifact.id,sha:artifact.content_sha,revision:artifact.revision})), "Resume artifact missing from evidence");
          requireValue(list(object(parse(raw)).experiments).filter(value => equal(value, experiment)).length === 1, "Resume experiment not uniquely present in artifact");
          same(experiment.bundle_manifest_sha, work.bundle_manifest_sha, "Resume frozen experiment bundle");
          same(parse(source), spec, "Resume Program source spec");
          same(await canonicalSHA256(source), m.canonical_sha, "Resume Program canonical source hash");
          same(c.canonical_payload_sha, work.subject_id, "Resume frozen experiment subject");
          same(parse(payloadRaw), experiment, "Resume selected experiment");
        }
      }
    }
  }
  if (command.action !== "program.approve_experiment" && !resumeExecution) requireValue(c.execution_policy_sha === undefined && m.execution_policy_sha === undefined && m.selected_experiment === undefined, "Unexpected execution policy");
  if (!["program.approve_architecture", "program.approve_experiment"].includes(command.action) && !resumeExecution) requireValue(r.selected_artifact_canonical === undefined && m.selected_artifact === undefined, "Unexpected selected artifact");
  return result.data;
}

function base64url(value: unknown): Uint8Array {
  const encoded = text(value); requireValue(/^[A-Za-z0-9_-]+$/.test(encoded) && encoded.length % 4 !== 1, "Invalid base64url");
  let decoded: string; try { decoded = atob(encoded.replace(/-/g, "+").replace(/_/g, "/")); } catch { throw new IntakeReviewError("invalid_contract", "Invalid base64url"); }
  const bytes = Uint8Array.from(decoded, c => c.charCodeAt(0)); same(btoa(decoded).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, ""), encoded, "Canonical base64url"); return bytes;
}
export function validateAuthorizationOptions(review: Review, options: {challenge_id:string;public_key:Record<string,unknown>;review_id:string;claims_sha:string;command_digest:string}, commandDigest: string): void {
  const now = Date.now(), issued = Date.parse(review.claims.issued_at), expires = Date.parse(review.claims.expires_at);
  if (!Number.isFinite(issued) || !Number.isFinite(expires) || issued > now || expires <= now || expires <= issued || expires - issued > 120000) throw new IntakeReviewError("challenge_expired", "Review is no longer fresh for authorization");
  same(options.challenge_id, review.claims.challenge_id, "Authorization challenge ID"); same(options.review_id, review.id, "Authorization review ID"); same(options.claims_sha, review.claims_sha, "Authorization claims"); same(options.command_digest, commandDigest, "Authorization command"); requireValue(/^[a-f0-9]{64}$/.test(commandDigest), "Invalid command digest"); requireValue(base64url(options.public_key.challenge).length >= 32, "Authorization challenge too short"); same(options.public_key.userVerification, "required", "User verification");
  const allowed = list(options.public_key.allowCredentials);
  requireValue(allowed.length === 1, "Authorization must select exactly the reviewed credential");
  const selected = object(allowed[0]);
  same(selected.type, "public-key", "Authorization credential type");
  same(selected.id, review.claims.credential_id, "Authorization credential ID");
}
export function validateAssertionBinding(credential: object, challenge: string, origin: string): void {
  base64url(challenge); const response = object(object(credential).response), bytes = base64url(response.clientDataJSON);
  let raw: string; try { raw = new TextDecoder("utf-8", {fatal:true}).decode(bytes); } catch { throw new IntakeReviewError("invalid_contract", "Invalid client data UTF-8"); }
  const data = object(parse(raw)); same(data.type, "webauthn.get", "Assertion type"); same(data.origin, origin, "Assertion origin"); base64url(data.challenge); same(data.challenge, challenge, "Assertion challenge"); requireValue(data.crossOrigin === undefined || data.crossOrigin === false, "Cross-origin assertion forbidden");
}
