import type { FeatureStatus } from "./contract.ts";

export const stages = ["queued", "building", "checking", "reviewing", "waiting_to_merge", "integrated"] as const;
export function currentAttempt(status: FeatureStatus) {
  return status.attempts.find(attempt => attempt.active) ?? status.attempts[0];
}
export function featureStage(status: FeatureStatus) {
  if (status.feature.state === "accepted") {
    const receipt = (status.receipt ?? status.feature.receipt ?? "").trim();
    if (status.feature.completion_target === "pull_request") return "waiting_to_merge";
    if (status.feature.completion_target === "reviewed" || receipt === "reviewed" || receipt.startsWith("reviewed:")) return "reviewed";
    if (["integrated", "staging", "production"].includes(status.feature.completion_target) && receipt && !prLink(receipt)) return "integrated";
    return "accepted";
  }
  if (["proposed", "awaiting_approval", "ready"].includes(status.feature.state) && !status.attempts.some(attempt => attempt.active)) return "queued";
  const stage = currentAttempt(status)?.stage ?? status.timeline.at(-1)?.stage;
  if (stage && stageColumn(stage) && stageColumn(stage) !== "integrated") return stage;
  if (["blocked", "uncertain", "cancelled", "cancelling", "failed"].includes(status.feature.state)) {
    for (let index = status.timeline.length - 1; index >= 0; index--) {
      const recorded = status.timeline[index];
      const column = stageColumn(recorded.stage);
      if (column !== undefined && column !== "integrated") return recorded.stage;
    }
    return stage ?? status.feature.state;
  }
  return stage ?? status.feature.state;
}
export function stageColumn(stage: string): typeof stages[number] | undefined {
  if (["queued", "proposed", "awaiting_approval", "ready"].includes(stage)) return "queued";
  if (/^(prepare|scout|build|plan|committing)(_|$)/.test(stage) || stage === "building") return "building";
  if (["checks", "checking"].includes(stage)) return "checking";
  if (/^review(_|$)/.test(stage) || ["reviewing", "reviewed"].includes(stage)) return "reviewing";
  if (["integration_queued", "waiting_to_merge", "reconciliation_pending", "reconciling", "integrating", "opening_pr", "pr_opened", "releasing", "rollback", "rolling_back"].includes(stage)) return "waiting_to_merge";
  if (stage === "integrated") return "integrated";
  return undefined;
}
export function waiting(stage: string, state: string) {
  return ["queued", "integration_queued", "waiting_to_merge", "reconciliation_pending"].includes(stage) || /wait|block|pending|queue|pause|cancelling|uncertain/.test(state);
}
export function prLink(value?: string) {
  if (!value) return null;
  try {
    const url = new URL(value);
    return url.protocol === "https:" && url.hostname === "github.com" && !url.port && !url.username && !url.password && !url.search && !url.hash && /^\/[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+\/pull\/[1-9]\d*\/?$/.test(url.pathname) ? url.href : null;
  } catch { return null; }
}
