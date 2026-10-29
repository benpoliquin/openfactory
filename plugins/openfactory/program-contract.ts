import { z } from "zod";
import * as p from "./program-schemas.ts";
const id = z.string().min(1).max(256).refine(value => !/[\u0000-\u0020/\\?#]/.test(value) && value !== "." && value !== "..");
export const programQuery = z.object({ project: z.string().max(256).optional(), state: z.string().max(64).optional(), after: z.string().max(4096).optional(), kind: z.string().max(64).optional(), limit: z.number().int().min(1).max(500).optional(), revision: z.number().int().positive().optional() }).strict();
export const programID = z.object({ id }).strict();
export const programPageInput = programID.extend({ section: z.enum(["questions", "artifacts", "evidence", "experiments", "features", "events"]), query: programQuery });
export const artifactInput = programID.extend({ artifact_id: id });
const items = <T extends z.ZodType>(item: T) => z.array(item).max(500).nullable().transform(value => value ?? []);
const page = <T extends z.ZodType>(item: T) => z.object({items:items(item), next_cursor:z.string().max(4096).optional(), snapshot:z.string().max(4096), revision_hash:z.string().max(256).optional()}).strict();
export const programListSchema = page(p.Program);
export const programStatusSchema = z.object({program:p.Program,revision:p.ProgramRevision,budget:p.ProgramBudget,grants:items(p.ProgramGrant),invalidations:items(p.ProgramInvalidation),steps:items(p.ProgramStep),snapshot:z.string().max(4096),truncated:z.boolean().optional()}).strict();
export const sectionSchemas = {
 artifacts:page(p.ProgramArtifact).extend({output_states:items(p.ProgramArtifactOutputState)}), evidence:page(p.SourceCapture), events:page(p.ProgramEvent),
 questions:page(z.object({artifact:p.ProgramArtifact,question:p.ProgramQuestion,answers:items(p.QuestionAnswer)}).strict()),
 experiments:page(z.object({artifact:p.ProgramArtifact,contract:p.ExperimentContract,contract_sha:z.string(),grants:items(p.ProgramGrant),receipts:items(p.ExperimentReceipt)}).strict()),
 features:page(z.object({link:p.ProgramFeatureLink,feature:z.object({feature:p.Feature})}).strict()),
};
export const programPageSchema = z.union([sectionSchemas.artifacts,sectionSchemas.evidence,sectionSchemas.events,sectionSchemas.questions,sectionSchemas.experiments,sectionSchemas.features]);
export const artifactDetailSchema = z.object({artifact:p.ProgramArtifact,output_state:p.ProgramArtifactOutputState,payload:z.unknown(),snapshot:z.string().max(4096)}).strict().transform((detail,ctx) => {
 const schema = p.payloadSchemas[detail.artifact.kind as keyof typeof p.payloadSchemas];
 const parsed = schema?.safeParse(detail.payload);
 if (!parsed?.success) {ctx.addIssue({code:"custom",message:"Invalid canonical artifact payload"});return z.NEVER;}
 return {...detail,payload:parsed.data};
});

export function artifactOutputLabel(state: z.infer<typeof p.ProgramArtifactOutputState>): string {
 const status = state.historical_revision ? "Historical revision" : state.current_stage_output ? "Current stage output" : "Historical / non-current output";
 const supersession = state.superseded_by.length ? ` · superseded by ${state.superseded_by.join(", ")}` : "";
 return `${status} · producer step: ${state.producer_step_kind} · subject: ${state.producer_subject_id || "(none)"}${supersession}`;
}
