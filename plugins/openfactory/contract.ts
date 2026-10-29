import { defineRpcContract } from "@get-bb/plugin-sdk";
import { z } from "zod";
import { programQuery, programID, programPageInput, artifactInput, programListSchema, programStatusSchema, programPageSchema, artifactDetailSchema } from "./program-contract.ts";

const timestamp = z.iso.datetime({ offset: true });
const count = z.number().int().nonnegative();
const capacity = z.object({ active: count, limit: count });
export const snapshotSchema = z.object({
  observed_at: timestamp,
  paused: z.boolean(),
  limits: z.object({ global: capacity, projects: z.array(capacity.extend({ name: z.string() })) }),
  calls: z.object({ day: z.string(), used: count, limit: count }),
  intake_step_count: count.optional(),
  features: z.array(z.object({
    feature: z.object({
      id: z.string().min(1), title: z.string(), project: z.string(), state: z.string(),
      reason: z.string().optional(), completion_target: z.string(), receipt: z.string().optional(),
      created_at: timestamp, updated_at: timestamp,
      discovery_step_id: z.string().optional(),
      intake_step_id: z.string().optional(),
      program_evidence_gate: z.object({ program_id: z.string(), revision: count, manifest_sha: z.string(), evaluation_artifact_id: z.string() }).strict().optional(),
    }),
    attempts: z.array(z.object({
      id: z.string(), feature_id: z.string(), stage: z.string(), state: z.string(), active: z.boolean(),
      started_at: timestamp, stage_started_at: timestamp, updated_at: timestamp, deadline: timestamp,
      base_sha: z.string(), candidate_sha: z.string().optional(), candidate_generation: count,
      source_base_sha: z.string().optional(), source_candidate_sha: z.string().optional(),
    })),
    sessions: z.array(z.object({
      attempt_id: z.string(), stage: z.string(), dispatch_key: z.string(), thread_id: z.string(),
      environment_id: z.string(), created_at: timestamp,
    })),
    checks: z.array(z.object({
      attempt_id: z.string(), name: z.string(), candidate_generation: count, candidate_sha: z.string(),
      state: z.string(), detail: z.string(), started_at: timestamp, updated_at: timestamp,
    })),
    integration: z.object({
      resources: z.array(z.string()), state: z.string(), position: count,
      owner_attempt_id: z.string().optional(), sequence: count,
    }),
    wait: z.object({
      code: z.string(), detail: z.string().optional(), feature_id: z.string().optional(),
      resource: z.string().optional(), owner_attempt_id: z.string().optional(),
    }).optional(),
    current_evidence: z.array(z.object({
      id: z.string(), feature_id: z.string(), attempt_id: z.string(), kind: z.string(),
      candidate_sha: z.string(), candidate_generation: count, passed: z.boolean(), detail: z.string(), created_at: timestamp,
    })),
    receipt: z.string().optional(),
    dependencies: z.array(z.object({ id: z.string(), state: z.string(), satisfied: z.boolean() })),
    timeline: z.array(z.object({
      stage: z.string(), state: z.string(), started_at: timestamp,
      finished_at: timestamp.optional(), attempt_id: z.string().optional(),
    })),
  })),
  next_cursor: z.string().optional(),
});
export type Snapshot = z.infer<typeof snapshotSchema>;
export type FeatureStatus = Snapshot["features"][number];
export const rpcContract = defineRpcContract({
  snapshot: { input: z.null(), output: snapshotSchema },
  programList: { input: programQuery, output: programListSchema },
  programStatus: { input: programID, output: programStatusSchema },
  programPage: { input: programPageInput, output: programPageSchema },
  programArtifact: { input: artifactInput, output: artifactDetailSchema },
});
