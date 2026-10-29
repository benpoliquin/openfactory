import assert from "node:assert/strict";
import { describe, it } from "node:test";
import { commandSchema, reviewSchema, type Command, type Review, type Session } from "./intake-contract.ts";
import { validateReview, validateAuthorizationOptions, validateAssertionBinding } from "./intake-review.ts";
import { IntakeClient } from "./intake-client.ts";

// Small assertion adapter keeps synchronous and asynchronous boundary cases uniform.
function expect(value: unknown) {
  return {
    toThrow() { assert.equal(typeof value, "function"); if (typeof value === "function") assert.throws(() => value()); },
    not: { toThrow() { assert.equal(typeof value, "function"); if (typeof value === "function") assert.doesNotThrow(() => value()); } },
    resolves: { async toEqual(expected: unknown) { assert.deepEqual(await value, expected); } },
    rejects: {
      async toThrow() { await assert.rejects(Promise.resolve(value)); },
      async toMatchObject(expected: {code: string}) { await assert.rejects(Promise.resolve(value), (error: unknown) => error instanceof Error && "code" in error && error.code === expected.code); },
    },
  };
}

async function rebind(review: Review, manifest: Record<string, unknown>, claims: Record<string, unknown>) {
  const manifestRaw = JSON.stringify(manifest);
  const nextClaims = {...claims,review_manifest_sha:await hash(manifestRaw)};
  const claimsRaw = JSON.stringify(nextClaims);
  return reviewSchema.parse({...review,manifest_canonical:manifestRaw,claims:nextClaims,claims_canonical:claimsRaw,claims_sha:await hash(claimsRaw)});
}

const hash = async (raw: string) => Array.from(new Uint8Array(await crypto.subtle.digest("SHA-256", new TextEncoder().encode(raw))), b => b.toString(16).padStart(2,"0")).join("");
const session: Session = {operator_id:"operator",credential_id:"Y3JlZGVudGlhbA",installation_id:"installation",project_scope:["project"],label:"Operator",csrf_token:"csrf",auth_generation:"generation"};
const id = "9b5d201a-4bb8-4298-b833-e17f8322c073";
const policy = "a".repeat(64), revision = "b".repeat(64), input = "c".repeat(64);
const encode = (raw:string) => btoa(raw).replace(/\+/g,"-").replace(/\//g,"_").replace(/=+$/,"");

async function fixture(action:string, kind = "program", contextSHA = input) {
  const requestAction = action.startsWith("intake.");
  const artifactAction = action === "program.approve_architecture" || action === "program.approve_experiment";
  const feature = {id:"subject",project:"project",kind,title:"Title",revision,body:"Goal",priority:0,risk:"normal",completion_target:"integrated",depends_on:null,checks:null,acceptance:null,policy_hash:policy};
  const spec = {id:"subject",project:"project",title:"Program",limits:{max_iterations:3}};
  const pr = {program_id:"subject",number:1,revision_hash:revision,spec,seed_manifest_sha:"",created_at:new Date().toISOString()};
  const request = {id:"request",operator_id:"operator",project:"project",state:action === "intake.retry" ? "blocked":"review_ready",content_version:2,latest_draft_revision:1,creation_policy_sha:policy,lifetime_dispatches:1,max_draft_dispatches:12,context:{sha:contextSHA}};
  let source = kind === "program" ? JSON.stringify(spec) : `---\nid: subject\nproject: project\ntitle: Title\nkind: ${kind}\nrisk: normal\npriority: 0\ncompletion_target: integrated\nacceptance: null\n---\nGoal`;
  let payload = source;
  const manifest: Record<string,unknown> = {command_sha:"",source_sha:await hash(source),canonical_sha:await hash(payload),domain_revision:revision,policy_sha:policy,evidence:[],bundle_manifest_shas:[]};
  const command: Command = {command_id:id,action,payload:{kind,id:"subject",revision}};
  const material: Record<string,unknown> = {canonical_source:source,findings:[]};
  if (kind === "program") { material.program_revision = pr; manifest.program_revision = pr; manifest.program = {id:"subject",project:"project",state:"active",current_revision:1,iteration:1,...(action === "program.resume" ? {wait:{code:"prerequisite_missing",detail:"No failed worker is selected; resume continues only previously approved Program work and prerequisites."}} : {})}; }
  else { material.feature = feature; manifest.feature = feature; }
  if (requestAction) {
    command.request_id = "request"; command.expected_content_version = 2; manifest.request = request;
    if (action === "intake.retry") { command.payload = {}; payload = JSON.stringify(request); source = ""; material.canonical_source = ""; delete material.program_revision; delete manifest.program_revision; delete manifest.program; manifest.source_sha = ""; manifest.canonical_sha = ""; manifest.domain_revision = ""; }
    else { command.payload = {draft_revision:1,source_sha:await hash(source),domain_revision:revision}; manifest.draft = {request_id:"request",content_version:2,revision:1,route:kind,canonical_source:source,source_sha:await hash(source),canonical_sha:await hash(payload),domain_revision:revision,policy_sha:policy,findings:[]}; }
  }
  const bundle = "d".repeat(64);
  const experiment = {id:"experiment",bundle_manifest_sha:bundle};
  if (artifactAction) {
    const raw = action === "program.approve_experiment" ? JSON.stringify({experiments:[experiment]}) : '{"architecture":"reviewed"}';
    const artifactSHA = await hash(raw);
    const selected = {id:"artifact",kind:action === "program.approve_experiment" ? "experiment_contracts":"architecture",program_id:"subject",revision:1,content_sha:artifactSHA,input_manifest_sha:input};
    manifest.selected_artifact = selected; material.selected_artifact_canonical = raw;
    manifest.evidence = [{kind:selected.kind,id:selected.id,sha:artifactSHA,revision:1}];
    payload = action === "program.approve_experiment" ? JSON.stringify(experiment) : raw;
    command.payload = {program_id:"subject",revision,artifact_id:selected.id,artifact_kind:selected.kind,artifact_sha:artifactSHA,subject_sha:await hash(payload)};
    if (action === "program.approve_experiment") { manifest.selected_experiment = experiment; manifest.bundle_manifest_shas = [bundle]; manifest.execution_policy_sha = policy; }
  }
  const claims: Record<string,unknown> = {version:1,installation_id:session.installation_id,operator_id:session.operator_id,credential_id:session.credential_id,action,command_id:id,challenge_id:"challenge",subject_kind:action === "intake.retry" ? "intake":kind,subject_id:action === "intake.retry" ? "request":"subject",subject_revision:action === "intake.retry" ? await hash(payload):revision,canonical_payload_sha:await hash(payload),review_manifest_sha:"",policy_sha:policy,evidence:manifest.evidence,bundle_manifest_shas:manifest.bundle_manifest_shas,issued_at:new Date(Date.now()-1000).toISOString(),expires_at:new Date(Date.now()+60000).toISOString()};
  if (requestAction) { claims.request_id = "request"; claims.content_version = 2; if (contextSHA) claims.input_manifest_sha = contextSHA; if (action !== "intake.retry") claims.draft_revision = 1; }
  if (artifactAction) claims.input_manifest_sha = input;
  if (action === "program.approve_experiment") claims.execution_policy_sha = policy;
  const commandRaw = JSON.stringify(command); manifest.command_sha = await hash(commandRaw);
  const manifestRaw = JSON.stringify(manifest); claims.review_manifest_sha = await hash(manifestRaw);
  const claimsRaw = JSON.stringify(claims);
  const review = reviewSchema.parse({id:"review",command,claims,claims_sha:await hash(claimsRaw),claims_canonical:claimsRaw,manifest_canonical:manifestRaw,canonical_payload:payload,command_canonical:commandRaw,created_at:new Date().toISOString(),...material});
  return {review,command,manifest,claims,material};
}

describe("review authority consumer boundary", () => {
  for (const [action,kind] of [["intake.retry","program"],["intake.approve_queue","feature"],["intake.approve_queue","plan"],["intake.approve_queue","program"],["feature.approve","feature"],["subject.retry","feature"],["subject.retry","plan"],["subject.retry","program"],["program.approve_revision","program"],["program.resume","program"],["program.approve_architecture","program"],["program.approve_experiment","program"]]) {
    it(`accepts complete ${action}/${kind} and rejects altered source or claims`, async () => {
      const {review,command} = await fixture(action,kind);
      await expect(validateReview(review,command,session,policy)).resolves.toEqual(review);
      await expect(validateReview({...review,canonical_source:"altered"},command,session)).rejects.toThrow();
      await expect(validateReview({...review,claims:{...review.claims,subject_id:"other"}},command,session)).rejects.toThrow();
      await expect(validateReview(review,{...command,command_id:"a57d200a-4bb8-4298-b833-e17f8322c073"},session)).rejects.toThrow();
    });
  }
  it("rejects missing material, wrong actor, project policy, and bound manifest tampering", async () => {
    const {review,command} = await fixture("program.approve_revision");
    for (const mutation of [{claims_canonical:""},{manifest_canonical:"{}"},{canonical_payload:"{}"},{program_revision:undefined},{answers:[{text:"unbound"}]},{seed_manifest:{manifest_sha:input,files:[]}},{artifacts:[{id:"unbound"}]}]) await expect(validateReview({...review,...mutation},command,session)).rejects.toThrow();
    await expect(validateReview(review,command,{...session,installation_id:"other"})).rejects.toThrow();
    await expect(validateReview(review,command,{...session,project_scope:[]})).rejects.toThrow();
    await expect(validateReview(review,command,session,input)).rejects.toMatchObject({code:"stale_policy"});
  });
  it("rejects recomputed transport proofs with inconsistent action material", async () => {
    for (const action of ["program.approve_architecture", "program.approve_experiment"]) {
      const {review,command,manifest,claims} = await fixture(action);
      const artifact = Object.fromEntries(Object.entries(manifest.selected_artifact ?? {}));
      for (const change of [{program_id:"other"}, {content_sha:input}, {revision:2}, {input_manifest_sha:policy}]) {
        const changed = await rebind(review,{...manifest,selected_artifact:{...artifact,...change}},claims);
        await expect(validateReview(changed,command,session)).rejects.toThrow();
      }
      const unknown = await rebind(review,{...manifest,unknown_authority:true},claims);
      await expect(validateReview(unknown,command,session)).rejects.toThrow();
      const evidence = await rebind(review,{...manifest,evidence:[]},{...claims,evidence:[]});
      await expect(validateReview(evidence,command,session)).rejects.toThrow();
      if (action === "program.approve_experiment") {
        const bundles = await rebind(review,{...manifest,bundle_manifest_shas:[]},{...claims,bundle_manifest_shas:[]});
        await expect(validateReview(bundles,command,session)).rejects.toThrow();
        const execution = await rebind(review,{...manifest,execution_policy_sha:input},claims);
        await expect(validateReview(execution,command,session)).rejects.toThrow();
      }
    }
    const {review,command,manifest,claims} = await fixture("program.approve_revision");
    for (const change of [{subject_id:"other"}, {installation_id:"other"}, {expires_at:new Date(Date.now()-1).toISOString()}]) {
      await expect(validateReview(await rebind(review,manifest,{...claims,...change}),command,session)).rejects.toThrow();
    }
  });
  it("accepts a manually imported draft without disclosed model context", async () => {
    const {review,command,manifest,claims} = await fixture("intake.approve_queue","feature","");
    await expect(validateReview(review,command,session)).resolves.toEqual(review);
    const wrong = await rebind(review,manifest,{...claims,input_manifest_sha:input});
    await expect(validateReview(wrong,command,session)).rejects.toThrow();
  });
  it("matches Go body whitespace semantics for NEL and preserves BOM", async () => {
    const {review,command,manifest,claims} = await fixture("intake.approve_queue","feature");
    for (const [prefix, valid] of [["\u0085",true],["\ufeff",false]] as const) {
      const source = String(review.canonical_source).replace("\nGoal",`\n${prefix}Goal${prefix}`);
      const sourceSHA = await hash(source), nextCommand = commandSchema.parse({...command,action:"intake.approve_queue",payload:{draft_revision:command.payload.draft_revision,source_sha:sourceSHA,domain_revision:command.payload.domain_revision}});
      const commandRaw = JSON.stringify(nextCommand), draft = Object.fromEntries(Object.entries(manifest.draft ?? {}));
      const changed = await rebind(review,{...manifest,source_sha:sourceSHA,command_sha:await hash(commandRaw),draft:{...draft,source_sha:sourceSHA,canonical_source:source}},claims);
      const candidate = {...changed,command:nextCommand,command_canonical:commandRaw,canonical_source:source};
      if (valid) await expect(validateReview(candidate,nextCommand,session)).resolves.toEqual(candidate);
      else await expect(validateReview(candidate,nextCommand,session)).rejects.toThrow();
    }
  });
  it("rejects rehashed Feature source contradictions and unsafe YAML", async () => {
    const {review,command,manifest,claims} = await fixture("intake.approve_queue","feature");
    for (const source of [
      String(review.canonical_source).replace("title: Title", "title: Different"),
      String(review.canonical_source).replace("title: Title", "title: Title\ntitle: Other"),
      String(review.canonical_source).replace("title: Title", "title: &title Title"),
      String(review.canonical_source).replace("title: Title", "title: !!str Title"),
      String(review.canonical_source).replace("title: Title", "title: Title\nprogram_evidence_gate:\n  __proto__: forbidden"),
      String(review.canonical_source).replace("title: Title", "title: Title\nprogram_evidence_gate:\n  program_id: program\n  revision: 0\n  manifest_sha: bad\n  evaluation_artifact_id: evaluation"),
    ]) {
      const sourceSHA = await hash(source), nextCommand = commandSchema.parse({...command,action:"intake.approve_queue",payload:{draft_revision:command.payload.draft_revision,source_sha:sourceSHA,domain_revision:command.payload.domain_revision}});
      const commandRaw = JSON.stringify(nextCommand);
      const draft = Object.fromEntries(Object.entries(manifest.draft ?? {}));
      const changedManifest = {...manifest,source_sha:sourceSHA,command_sha:await hash(commandRaw),draft:{...draft,source_sha:sourceSHA,canonical_source:source}};
      const changed = await rebind(review,changedManifest,claims);
      await expect(validateReview({...changed,command:nextCommand,command_canonical:commandRaw,canonical_source:source},nextCommand,session)).rejects.toThrow();
    }
  });
  it("accepts existing immutable Feature despite mutable ledger differences", async () => {
    const {review,command,manifest,claims} = await fixture("intake.approve_queue","feature");
    manifest.feature = {...Object.fromEntries(Object.entries(review.feature ?? {})),state:"running",attempts:3};
    const manifestRaw = JSON.stringify(manifest); claims.review_manifest_sha = await hash(manifestRaw); const raw = JSON.stringify(claims);
    const changed = reviewSchema.parse({...review,manifest_canonical:manifestRaw,claims,claims_canonical:raw,claims_sha:await hash(raw)});
    await expect(validateReview(changed,command,session)).resolves.toEqual(changed);
  });
  it("does not equate raw draft source SHA with normalized canonical payload SHA", async () => {
    const {review,command,manifest,claims} = await fixture("intake.approve_queue","program");
    const source = '{"id":"subject","project":"project","title":"Program","limits":{"max_iterations":0}}';
    const sourceSHA = await hash(source); const changedCommand = commandSchema.parse({...command,action:"intake.approve_queue",payload:{draft_revision:command.payload.draft_revision,source_sha:sourceSHA,domain_revision:command.payload.domain_revision}});
    const draft = Object.fromEntries(Object.entries(manifest.draft ?? {})); manifest.draft = {...draft,canonical_source:source,source_sha:sourceSHA}; manifest.source_sha = sourceSHA;
    const commandRaw = JSON.stringify(changedCommand); manifest.command_sha = await hash(commandRaw);
    const manifestRaw = JSON.stringify(manifest); claims.review_manifest_sha = await hash(manifestRaw); const raw = JSON.stringify(claims);
    const changed = reviewSchema.parse({...review,command:changedCommand,command_canonical:commandRaw,canonical_source:source,manifest_canonical:manifestRaw,claims,claims_canonical:raw,claims_sha:await hash(raw)});
    await expect(validateReview(changed,changedCommand,session)).resolves.toEqual(changed);
  });
});

describe("native ceremony boundaries", () => {
  it("binds options IDs and digest independently from the random challenge", async () => {
    const {review} = await fixture("feature.approve","feature"); const digest = await hash(review.command_canonical);
    const options = {challenge_id:"challenge",review_id:review.id,claims_sha:review.claims_sha,command_digest:digest,public_key:{challenge:encode("r".repeat(32)),userVerification:"required",allowCredentials:[{type:"public-key",id:review.claims.credential_id}]}};
    expect(() => validateAuthorizationOptions(review,options,digest)).not.toThrow();
    for (const claims of [
      {...review.claims,expires_at:new Date(Date.now()-1).toISOString()},
      {...review.claims,issued_at:new Date(Date.now()+30000).toISOString()},
    ]) expect(() => validateAuthorizationOptions({...review,claims},options,digest)).toThrow();
    for (const mutation of [{challenge_id:"wrong"},{review_id:"wrong"},{claims_sha:input},{command_digest:input},{public_key:{...options.public_key,challenge:"x"}},{public_key:{...options.public_key,userVerification:"preferred"}}]) expect(() => validateAuthorizationOptions(review,{...options,...mutation},digest)).toThrow();
    for (const allowCredentials of [[],[{type:"public-key",id:"other"}],[{type:"other",id:review.claims.credential_id}], [...options.public_key.allowCredentials,{type:"public-key",id:"other"}]]) expect(() => validateAuthorizationOptions(review,{...options,public_key:{...options.public_key,allowCredentials}},digest)).toThrow();
  });
  it("rejects assertion challenge, origin, type, crossOrigin, and encoding mismatches", () => {
    const challenge = encode("r".repeat(32)), origin = "https://openfactory.example";
    const clientData = {type:"webauthn.get",origin,challenge,crossOrigin:false};
    const credential = (data:object) => ({response:{clientDataJSON:encode(JSON.stringify(data))}});
    expect(() => validateAssertionBinding(credential(clientData),challenge,origin)).not.toThrow();
    for (const mutation of [{type:"webauthn.create"},{origin:"https://attacker.example"},{challenge:encode("s".repeat(32))},{crossOrigin:true},{challenge:challenge+"="}]) expect(() => validateAssertionBinding(credential({...clientData,...mutation}),challenge,origin)).toThrow();
  });
});

describe("client trusted review boundary", () => {
  async function clientFixture(reviewMutation: Record<string,unknown> = {}, optionsMutation: Record<string,unknown> = {}, clientDataMutation: Record<string,unknown> = {}) {
    const {review,command} = await fixture("feature.approve","feature");
    const origin = "https://openfactory.example", challenge = encode("q".repeat(32));
    let gets = 0, commits = 0;
    const storageData = new Map<string,string>();
    const options = {challenge_id:review.claims.challenge_id,review_id:review.id,claims_sha:review.claims_sha,command_digest:await hash(review.command_canonical),public_key:{challenge,userVerification:"required",allowCredentials:[{type:"public-key",id:review.claims.credential_id}]},...optionsMutation};
    class AssertionCredential implements Credential {
      readonly type = "public-key";
      readonly id = encode("credential");
      readonly rawId = new TextEncoder().encode("credential").buffer;
      readonly authenticatorAttachment = null;
      readonly response = {
        clientDataJSON:new TextEncoder().encode(JSON.stringify({type:"webauthn.get",origin,challenge,...clientDataMutation})).buffer,
        authenticatorData:new Uint8Array(37).buffer,
        signature:new Uint8Array([48,6,2,1,1,2,1,1]).buffer,
        userHandle:null,
      };
      getClientExtensionResults() { return {}; }
    }
    const client = new IntakeClient({
      origin,
      storage:{getItem:key=>storageData.get(key)??null,setItem:(key,value)=>{storageData.set(key,value);},removeItem:key=>{storageData.delete(key);}},
      credentials:{create:async()=>null,get:async()=>{gets++;return new AssertionCredential();}},
      fetch:async input=>{
        const path = String(input);
        if (path.endsWith("/projects")) return Response.json({items:[],policy_sha:policy});
        if (path.endsWith("/reviews")) return Response.json({...review,...reviewMutation});
        if (path.endsWith("/authorizations/options")) return Response.json(options);
        if (path.endsWith("/authorizations/commit")) { commits++; throw new Error("Unexpected authorization commit"); }
        throw new Error(`Unexpected test endpoint ${path}`);
      },
    });
    client.session = {...session};
    return {client,review,command,counts:()=>({gets,commits})};
  }
  it("never returns an incomplete or contradictory review card", async () => {
    for (const mutation of [{claims_canonical:undefined},{canonical_payload:"altered"},{feature:{id:"different"}}]) {
      const state = await clientFixture(mutation);
      await assert.rejects(state.client.review(state.command));
      assert.deepEqual(state.counts(),{gets:0,commits:0});
    }
  });
  it("rejects authorization metadata before opening the native ceremony", async () => {
    for (const mutation of [{challenge_id:"other"},{review_id:"other"},{claims_sha:input},{command_digest:input},{public_key:{challenge:encode("q".repeat(32)),userVerification:"required",allowCredentials:[{type:"public-key",id:"other"}]}}]) {
      const state = await clientFixture({},mutation);
      await assert.rejects(state.client.authorize(state.review));
      assert.deepEqual(state.counts(),{gets:0,commits:0});
    }
  });
  it("serializes a binary native assertion but refuses mismatched client data before commit", async () => {
    for (const mutation of [{challenge:encode("wrong".repeat(8))},{origin:"https://attacker.example"}]) {
      const state = await clientFixture({}, {}, mutation);
      await assert.rejects(state.client.authorize(state.review), /Assertion (challenge|origin)/);
      assert.deepEqual(state.counts(),{gets:1,commits:0});
    }
  });
});

describe("Program resume work authority", () => {
  async function resumeFixture() {
    const base = await fixture("program.resume");
    const work: Record<string,unknown> = {
      step_id:"step",step_revision:1,step_kind:"research",step_state:"ready",iteration:1,
      input_manifest_sha:input,feature_id:"openfactory-discovery-step",feature_revision:revision,
      feature_state:"blocked",feature_attempts:1,feature_max_attempts:3,attempt_id:"attempt",attempt_state:"blocked",
      attempt_stage:"failed",attempt_active:false,attempt_output_sha:await hash(""),attempt_updated_at:new Date().toISOString(),
    };
    const evidence = [{kind:"program_revision",id:revision,sha:revision,revision:1}];
    const program = {...Object.fromEntries(Object.entries(base.manifest.program ?? {})),wait:{code:"prerequisite_missing",detail:"One current settled failed worker is selected for retry under its existing authority."}};
    const claims = {...base.claims,input_manifest_sha:input,evidence}, manifest = {...base.manifest,program,resume_work:work,evidence};
    const review = await rebind(reviewSchema.parse({...base.review,resume_work:work}),manifest,claims);
    return {...base,review,manifest,claims,work};
  }
  async function executionResumeFixture() {
    const base = await resumeFixture(), grant = await fixture("program.approve_experiment");
    const bundle = "d".repeat(64), receiptSHA = "e".repeat(64);
    const artifact: Record<string,unknown> = {...Object.fromEntries(Object.entries(grant.manifest.selected_artifact ?? {})),iteration:1};
    const work: Record<string,unknown> = {
      ...base.work,step_kind:"experiment_run",subject_id:await hash(grant.review.canonical_payload),
      execution_policy_sha:policy,bundle_manifest_sha:bundle,prior_run_id:"prior-run",
      prior_request_sha:policy,prior_receipt_sha:receiptSHA,prior_receipt_state:"failed",
      prior_execution_outcome:"infrastructure",prior_observation_outcome:"not_evaluated",prior_hypothesis_outcome:"inconclusive",
    };
    const evidence = [
      {kind:"bundle",id:bundle,sha:bundle,revision:1},
      {kind:"experiment_contracts",id:"artifact",sha:artifact.content_sha,revision:1},
      {kind:"experiment_receipt",id:"prior-run",sha:receiptSHA,revision:1},
      {kind:"program_revision",id:revision,sha:revision,revision:1},
    ];
    const claims = {...base.claims,execution_policy_sha:policy,canonical_payload_sha:work.subject_id,evidence,bundle_manifest_shas:[bundle]};
    const manifest = {...base.manifest,resume_work:work,selected_artifact:artifact,selected_experiment:grant.manifest.selected_experiment,execution_policy_sha:policy,evidence,bundle_manifest_shas:[bundle]};
    const review = await rebind(reviewSchema.parse({...base.review,resume_work:work,canonical_payload:grant.review.canonical_payload,selected_artifact_canonical:grant.review.selected_artifact_canonical}),manifest,claims);
    return {...base,review,manifest,claims,work};
  }
  it("accepts exact frozen execution resume including reconciled failure and inconclusive completion", async () => {
    const state = await executionResumeFixture();
    await assert.doesNotReject(validateReview(state.review,state.command,session));
    for (const mutation of [
      {prior_receipt_state:"completed",prior_receipt_conclusive:false},
      {attempt_state:"failed",attempt_stage:"operator_reconciled_failure",prior_receipt_state:"resolved_failed"},
    ]) {
      const work = {...state.work,...mutation};
      const review = await rebind(reviewSchema.parse({...state.review,resume_work:work}),{...state.manifest,resume_work:work},state.claims);
      await assert.doesNotReject(validateReview(review,state.command,session));
    }
  });
  it("accepts only an explicit controller no-launch proof without inventing a missing receipt fallback", async () => {
    const state = await executionResumeFixture();
    const work = {...Object.fromEntries(Object.entries(state.work).filter(([key]) => !key.startsWith("prior_"))),prior_launch_absent:true};
    const evidence = state.claims.evidence.filter(ref => ref.kind !== "experiment_receipt");
    const claims = {...state.claims,evidence}, manifest = {...state.manifest,resume_work:work,evidence};
    const review = await rebind(reviewSchema.parse({...state.review,resume_work:work}),manifest,claims);
    await assert.doesNotReject(validateReview(review,state.command,session));
    for (const mutation of [
      {prior_launch_absent:false},{prior_launch_absent:undefined},
      {prior_run_id:"unexpected"},{prior_receipt_sha:policy},{prior_receipt_conclusive:false},
      {attempt_state:"failed",attempt_stage:"operator_reconciled_failure"},
    ]) {
      const changedWork = {...work,...mutation};
      const changed = await rebind(reviewSchema.parse({...review,resume_work:changedWork}),{...manifest,resume_work:changedWork},claims);
      await assert.rejects(validateReview(changed,state.command,session));
    }
  });
  it("rejects rehashed execution resume contract, ownership, receipt, and opaque policy mismatches", async () => {
    const state = await executionResumeFixture();
    for (const mutation of [
      {subject_id:input},{execution_policy_sha:input},{bundle_manifest_sha:input},
      {prior_run_id:"other"},{prior_receipt_sha:input},{prior_request_sha:"invalid"},
      {prior_receipt_state:"uncertain"},{prior_receipt_state:"completed",prior_receipt_conclusive:true},
      {prior_receipt_state:"resolved_failed"},
      {prior_execution_outcome:"unknown"},{prior_observation_outcome:"unknown"},{prior_hypothesis_outcome:"unknown"},
      {prior_hypothesis_outcome:"supported"},{prior_hypothesis_outcome:"falsified"},
      {attempt_state:"failed",attempt_stage:"operator_reconciled_failure",prior_receipt_state:"failed"},
    ]) {
      await assert.rejects(async () => {
        const work = {...state.work,...mutation};
        const review = await rebind(reviewSchema.parse({...state.review,resume_work:work}),{...state.manifest,resume_work:work},state.claims);
        await validateReview(review,state.command,session);
      });
    }
    for (const mutation of [{selected_artifact_canonical:"{}"},{canonical_payload:state.review.canonical_source}]) await assert.rejects(validateReview({...state.review,...mutation},state.command,session));
    for (const mutation of [{selected_experiment:{id:"other"}},{selected_artifact:{id:"other"}},{bundle_manifest_shas:[]}]) {
      const review = await rebind(state.review,{...state.manifest,...mutation},state.claims);
      await assert.rejects(validateReview(review,state.command,session));
    }
  });
  it("accepts one settled failed worker and prerequisite-only resume without inventing work", async () => {
    const {review,command} = await resumeFixture();
    await assert.doesNotReject(validateReview(review,command,session));
    const prerequisite = await fixture("program.resume");
    await assert.doesNotReject(validateReview(prerequisite.review,prerequisite.command,session));
  });
  it("accepts a failed current iteration newer than the root Program iteration", async () => {
    const state = await resumeFixture(), work = {...state.work,iteration:2};
    const review = await rebind(reviewSchema.parse({...state.review,resume_work:work}),{...state.manifest,resume_work:work},state.claims);
    await assert.doesNotReject(validateReview(review,state.command,session));
  });
  it("rejects rehashed contradictory targets and unsafe worker ownership", async () => {
    const state = await resumeFixture();
    for (const mutation of [
      {step_id:"other"},{step_revision:2},{step_kind:"unknown"},{step_state:"running"},
      {iteration:0},{input_manifest_sha:policy},{feature_id:"ordinary-feature"},
      {feature_revision:"invalid"},{feature_state:"running"},{feature_attempts:-1},
      {feature_attempts:0},{feature_attempts:3},{feature_max_attempts:0},{feature_max_attempts:1},
      {attempt_id:""},{attempt_state:"running"},{attempt_stage:"uncertain"},{attempt_active:true},
      {attempt_state:"failed",attempt_stage:"operator_reconciled_failure"},
      {attempt_output_sha:"not-a-sha"},{attempt_updated_at:"invalid"},{execution_policy_sha:policy},
      {subject_id:input},{bundle_manifest_sha:policy},
    ]) {
      const work = {...state.work,...mutation};
      await assert.rejects(async () => {
        const review = await rebind(reviewSchema.parse({...state.review,resume_work:work}),{...state.manifest,resume_work:work},state.claims);
        await validateReview(review,state.command,session);
      });
    }
  });
  it("rejects displayed work changes even when all scalar fields remain well-formed", async () => {
    const state = await resumeFixture();
    for (const mutation of [{attempt_id:"other"},{feature_attempts:2},{attempt_output_sha:policy},{feature_revision:policy},{step_id:"other",feature_id:"openfactory-discovery-other"}]) {
      const review = reviewSchema.parse({...state.review,resume_work:{...state.work,...mutation}});
      await assert.rejects(validateReview(review,state.command,session));
    }
  });
  it("rejects unrelated actions, cross-project revisions, and canonical source substitution", async () => {
    const state = await resumeFixture();
    for (const mutation of [{state:"cancelled"},{wait:{code:"prerequisite_missing",detail:"/private/runtime/path/token"}}]) {
      const program = {...state.manifest.program,...mutation};
      const review = await rebind(state.review,{...state.manifest,program},state.claims);
      await assert.rejects(validateReview(review,state.command,session));
    }
    const approval = await fixture("program.approve_revision");
    const wrongAction = await rebind(reviewSchema.parse({...approval.review,resume_work:state.work}),{...approval.manifest,resume_work:state.work},approval.claims);
    await assert.rejects(validateReview(wrongAction,approval.command,session));
    await assert.rejects(validateReview(state.review,state.command,{...session,project_scope:["other"]}));
    await assert.rejects(validateReview({...state.review,canonical_payload:"{}"},state.command,session));
  });
});
