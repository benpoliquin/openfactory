import { z } from "zod";

const nonempty = z.string().min(1);
const integer = z.number().int().nonnegative();
const strings = z.array(z.string());
const record = z.record(z.string(), z.unknown());
const sha256 = z.string().regex(/^[a-f0-9]{64}$/);
const timestamp = z.iso.datetime({offset:true});
const fields = <T extends z.ZodRawShape>(shape:T) => z.object(shape).passthrough();
export const disclosureSchema = z.object({context_sha:nonempty,policy_sha:nonempty,model_provider_ids:strings,bundle_manifest_shas:strings.optional(),private_material_to_models:z.boolean()}).strict();
export type Disclosure = z.infer<typeof disclosureSchema>;
export const sessionSchema = z.object({operator_id:nonempty,label:z.string(),credential_id:nonempty,project_scope:strings,csrf_token:nonempty,installation_id:nonempty,auth_generation:nonempty}).strict();
export type Session = z.infer<typeof sessionSchema>;
export const projectSchema = fields({id:nonempty,checks:strings,completion_targets:strings,model_provider_ids:strings,limits:record});
export type Project = z.infer<typeof projectSchema>;
export const requestSchema = fields({id:nonempty,operator_id:nonempty,project:nonempty,state:nonempty,content_version:integer,latest_draft_revision:integer,creation_policy_sha:z.string(),event_sequence:integer,lifetime_dispatches:integer.optional(),max_draft_dispatches:integer.optional(),wait:record.optional()});
export type Request = z.infer<typeof requestSchema>;
export const questionSchema = fields({id:nonempty,request_id:nonempty,content_version:integer,input_manifest_sha:nonempty,output_sha:nonempty,text:nonempty,consequence:z.string(),answer_type:nonempty,choices:strings.nullable().optional(),answered_by_sequence:integer.optional()});
export type Question = z.infer<typeof questionSchema>;
export const draftSchema = fields({request_id:nonempty,revision:integer,content_version:integer,route:z.enum(["feature","plan","program"]),format:z.enum(["feature_markdown","program_json"]),canonical_source:z.string(),source_sha:nonempty,canonical_sha:nonempty,domain_revision:nonempty,input_manifest_sha:z.string(),output_sha:z.string(),policy_sha:nonempty,bundle_manifest_sha:z.string().regex(/^[a-f0-9]{64}$/).optional(),assumptions:strings.nullable(),unresolved_risks:strings.nullable(),findings:z.array(record).nullable()});
export type Draft = z.infer<typeof draftSchema>;
export const linkSchema = fields({request_id:nonempty,draft_revision:z.number().int().positive(),subject_kind:z.enum(["feature","plan","program"]),subject_id:nonempty,subject_revision:nonempty,command_id:z.uuid(),created_at:timestamp});
export type IntakeLink = z.infer<typeof linkSchema>;
export const viewSchema = fields({request:requestSchema,questions:z.array(questionSchema).nullable(),draft:draftSchema.optional(),steps:z.array(fields({id:nonempty,content_version:integer,state:nonempty,thread_id:z.string().optional()})).nullable(),links:z.array(linkSchema).nullable()});
export type RequestView = z.infer<typeof viewSchema>;
export const turnSchema = fields({request_id:nonempty,sequence:integer,role:nonempty,text:z.string(),producer_step_id:z.string().optional(),audit:record,created_at:nonempty});
export type Turn = z.infer<typeof turnSchema>;
const questionRef = z.object({question_id:nonempty,content_version:integer,input_manifest_sha:nonempty,output_sha:nonempty}).strict();
const subjectPayload = z.object({kind:z.enum(["feature","plan","program"]),id:nonempty,revision:nonempty}).strict();
const artifactPayload = z.object({program_id:nonempty,revision:nonempty,artifact_id:nonempty,artifact_kind:nonempty,artifact_sha:nonempty,subject_sha:nonempty}).strict();
const base = {command_id:z.uuid(),request_id:nonempty.optional(),expected_content_version:integer.optional()};
const command = <T extends z.ZodType>(action:string,payload:T) => z.object({...base,action:z.literal(action),payload}).strict();
export const commandSchema = z.union([
 command("request.create",z.object({project:nonempty,text:nonempty,client_message_id:z.uuid(),parent_request_id:nonempty.optional()}).strict()),
 command("turn.append",z.object({client_message_id:z.uuid(),text:nonempty,question:questionRef.optional()}).strict()),
 z.object({...base,action:z.literal("intake.draft"),request_id:nonempty,expected_content_version:integer,payload:z.object({disclosure:disclosureSchema}).strict()}).strict(),
 command("draft.replace",z.union([z.object({route:z.literal("program"),format:z.literal("program_json"),canonical_source:nonempty,bundle_manifest_sha:z.string().regex(/^[a-f0-9]{64}$/).optional()}).strict(),z.object({route:z.enum(["feature","plan"]),format:z.literal("feature_markdown"),canonical_source:nonempty}).strict()])),
 command("intake.cancel",z.object({}).strict()),command("intake.retry",z.object({}).strict()),
 command("intake.approve_queue",z.object({draft_revision:integer,source_sha:nonempty,domain_revision:nonempty}).strict()),
 ...["feature.approve","program.approve_revision","subject.retry","program.resume","subject.cancel"].map(action=>command(action,subjectPayload)),
 ...["program.approve_architecture","program.approve_experiment"].map(action=>command(action,artifactPayload)),
 command("program.answer",z.object({program_id:nonempty,revision_hash:nonempty,question_artifact_id:nonempty,question_artifact_sha:nonempty,question_id:nonempty,text:nonempty,supporting_refs:z.array(record)}).strict()),
 command("planning.answer",z.object({feature_id:nonempty,feature_revision:nonempty,attempt_id:nonempty,output_sha:nonempty,question_id:nonempty,text:nonempty}).strict()),
]);
// Shape stays explicit to consumers; runtime commandSchema rejects unknown authority fields.
export type Command = {command_id:string;action:string;request_id?:string;expected_content_version?:number;payload:Record<string,unknown>};
export const reviewClaimsSchema = z.object({
 version:z.literal(1),installation_id:nonempty,operator_id:nonempty,credential_id:nonempty,
 action:z.enum(["intake.approve_queue","feature.approve","program.approve_revision","program.approve_architecture","program.approve_experiment","subject.retry","program.resume","intake.retry"]),
 request_id:nonempty.optional(),content_version:integer.optional(),draft_revision:integer.optional(),
 subject_kind:z.enum(["intake","feature","plan","program"]),subject_id:nonempty,subject_revision:sha256,
 canonical_payload_sha:sha256,review_manifest_sha:sha256,policy_sha:sha256,execution_policy_sha:sha256.optional(),input_manifest_sha:sha256.optional(),
 evidence:z.array(z.object({kind:nonempty,id:nonempty,sha:sha256,revision:integer.optional()}).strict()),
 bundle_manifest_shas:z.array(sha256),command_id:z.uuid(),challenge_id:nonempty,issued_at:timestamp,expires_at:timestamp,
}).strict();
export const programResumeWorkSchema = z.object({
 step_id:nonempty,step_revision:z.number().int().positive(),step_kind:nonempty,step_state:nonempty,iteration:integer,input_manifest_sha:sha256,
 feature_id:nonempty,feature_revision:sha256,feature_state:nonempty,feature_attempts:integer,feature_max_attempts:z.number().int().positive(),
 attempt_id:nonempty,attempt_state:z.string(),attempt_stage:z.string(),attempt_active:z.literal(false),attempt_output_sha:sha256,attempt_updated_at:timestamp,
 subject_id:nonempty.optional(),execution_policy_sha:sha256.optional(),bundle_manifest_sha:sha256.optional(),
 prior_run_id:nonempty.optional(),prior_request_sha:sha256.optional(),prior_receipt_sha:sha256.optional(),prior_receipt_state:nonempty.optional(),prior_receipt_conclusive:z.boolean().optional(),
 prior_execution_outcome:nonempty.optional(),prior_observation_outcome:nonempty.optional(),prior_hypothesis_outcome:nonempty.optional(),
 prior_launch_absent:z.boolean().optional(),
}).strict();
export type ProgramResumeWork = z.infer<typeof programResumeWorkSchema>;
export const reviewSchema = z.object({
 id:nonempty,command:commandSchema,claims:reviewClaimsSchema,claims_sha:sha256,
 claims_canonical:nonempty,manifest_canonical:nonempty,canonical_payload:nonempty,command_canonical:nonempty,selected_artifact_canonical:nonempty.optional(),
 canonical_source:z.string(),feature:record.optional(),program_revision:record.optional(),answers:z.array(record).optional(),seed_manifest:record.optional(),artifacts:z.array(record).optional(),
 resume_work:programResumeWorkSchema.optional(),
 findings:z.array(z.object({code:z.string(),severity:z.string(),detail:z.string()}).strict()).nullable(),created_at:timestamp,
}).strict();
export type Review = z.infer<typeof reviewSchema>;
export const receiptSchema = fields({operator_id:nonempty,command_id:z.uuid(),digest:nonempty,action:nonempty,state:nonempty,result:z.unknown(),approval_id:z.string().optional()});
export type Receipt = z.infer<typeof receiptSchema>;
export const subjectSchema = fields({kind:nonempty,id:nonempty,project:nonempty,revision:nonempty,feature:record.optional(),program:record.optional(),program_revision:record.optional(),answers:z.array(record).optional(),planning_questions:z.array(record).optional(),planning_answers:z.array(record).optional(),seed_manifest:record.optional(),artifacts:z.array(record).optional()});
export type Subject = z.infer<typeof subjectSchema>;
export const errorSchema = z.object({error:z.object({code:nonempty,message:nonempty,retryable:z.boolean(),current:z.unknown().optional()}).strict()}).strict();
export const page = <T extends z.ZodType>(item:T) => fields({items:z.array(item).nullable(),next:z.string().optional()});
export const sequencePage = <T extends z.ZodType>(item:T) => fields({items:z.array(item).nullable(),next:integer.optional()});
export const ceremonySchema = z.object({challenge_id:nonempty,public_key:record}).strict();
export const authorizationOptionsSchema = z.object({challenge_id:nonempty,public_key:record,review_id:nonempty,claims_sha:sha256,command_digest:sha256}).strict();
export const enrollmentSchema = z.object({pairing_id:nonempty,credential_id:nonempty,fingerprint:z.string().regex(/^[a-f0-9]{64}$/),pending_confirmation:z.literal(true)}).strict();
export type Enrollment = z.infer<typeof enrollmentSchema>;
export const bootstrapSchema = z.object({enabled:z.boolean(),external_origin:z.string(),public_path:z.string(),rp_name:z.string()}).strict();
export function questionBinding(question:Question) { return {question_id:question.id,content_version:question.content_version,input_manifest_sha:question.input_manifest_sha,output_sha:question.output_sha}; }
export type DraftDisposition =
 | {kind:"handed_off";canApprove:false;link:IntakeLink}
 | {kind:"submitted_unresolved"|"superseded"|"unrelated"|"historical";canApprove:false}
 | {kind:"current";canApprove:boolean};
export function draftDisposition(draft:Draft,request:Request,links:IntakeLink[]):DraftDisposition {
 if(draft.request_id!==request.id)return {kind:"unrelated",canApprove:false};
 // The durable request/revision association identifies the immutable source;
 // handoff itself may advance request content version without changing that draft.
 const link=links.find(link=>link.request_id===request.id&&link.draft_revision===draft.revision&&link.subject_kind===draft.route&&link.subject_revision===draft.domain_revision);
 if(link)return {kind:"handed_off",canApprove:false,link};
 if(request.state==="submitted")return {kind:"submitted_unresolved",canApprove:false};
 if(draft.content_version!==request.content_version)return {kind:"superseded",canApprove:false};
 if(draft.revision!==request.latest_draft_revision)return {kind:"historical",canApprove:false};
 return {kind:"current",canApprove:request.state==="review_ready"&&!request.active_step_id};
}
export function reviewMatches(review:Review, command:Command):boolean {
 const claims=review.claims;
 return claims.version===1 && claims.action===command.action && claims.command_id===command.command_id && (!command.request_id||claims.request_id===command.request_id) && (command.expected_content_version===undefined||claims.content_version===command.expected_content_version) && (command.payload.draft_revision===undefined||claims.draft_revision===command.payload.draft_revision) && review.command.command_id===command.command_id && review.command.action===command.action && review.command.request_id===command.request_id && review.command.expected_content_version===command.expected_content_version && stableJSON(review.command.payload)===stableJSON(command.payload);
}
export function stableJSON(value:unknown):string {
 if (Array.isArray(value)) return `[${value.map(stableJSON).join(",")}]`;
 if (value!==null && typeof value==="object") return `{${Object.keys(value).sort().map(key=>`${JSON.stringify(key)}:${stableJSON((value as Record<string,unknown>)[key])}`).join(",")}}`;
 return JSON.stringify(value) ?? "null";
}
export function sourceDiff(before:string,after:string):string {
 const a=before.split("\n"), b=after.split("\n"); let start=0;
 while(start<a.length && start<b.length && a[start]===b[start]) start++;
 if(start===a.length && start===b.length) return "No source changes.";
 let endA=a.length,endB=b.length;
 while(endA>start && endB>start && a[endA-1]===b[endB-1]) {endA--;endB--;}
 return [`Lines ${start+1}–${endA} → ${start+1}–${endB}`, ...a.slice(start,endA).map(line=>`− ${line}`), ...b.slice(start,endB).map(line=>`+ ${line}`)].join("\n");
}
