import assert from "node:assert/strict";
import test from "node:test";
import { fetchSnapshot } from "./server.ts";
import type { Snapshot } from "./contract.ts";
import { featureStage, prLink, stageColumn, waiting } from "./presentation.ts";
import { cancelledSocket, dashboardFixture, sendJSON } from "./http-fixture.ts";

const now = "2026-10-28T12:00:00Z";
function fixture(id = "feature-1"): Snapshot {
  return {
    observed_at: now, paused: false,
    limits: { global: { active: 2, limit: 3 }, projects: [{ name: "example", active: 2, limit: 2 }] },
    calls: { day: "2026-10-28", used: 4, limit: 10 },
    features: [{
      feature: { id, title: "Concurrent lane", project: "example", state: "running", completion_target: "pull_request", created_at: now, updated_at: now },
      attempts: [{ id: "attempt-1", feature_id: id, stage: "reconciling", state: "running", active: true, started_at: now, stage_started_at: now, updated_at: now, deadline: now, base_sha: "base", candidate_sha: "candidate", candidate_generation: 2 }],
      sessions: [{ attempt_id: "attempt-1", stage: "review", dispatch_key: "attempt-1:review:2", thread_id: "thread-1", environment_id: "environment-1", created_at: now }],
      checks: [{ attempt_id: "attempt-1", name: "unit", candidate_generation: 2, candidate_sha: "candidate", state: "passed", detail: "", started_at: now, updated_at: now }],
      integration: { resources: ["git:example:main"], state: "owned", position: 0, sequence: 1 },
      wait: { code: "integration_queue", owner_attempt_id: "attempt-2", resource: "git:example:main" },
      current_evidence: [{ id: "proof-1", feature_id: id, attempt_id: "attempt-1", kind: "review", candidate_sha: "candidate", candidate_generation: 2, passed: true, detail: "", created_at: now }],
      receipt: "https://github.com/example/repo/pull/12",
      dependencies: [{ id: "dependency-1", state: "accepted", satisfied: true }],
      timeline: [{ stage: "reconciling", state: "running", attempt_id: "attempt-1", started_at: now }],
    }],
  };
}
test("bridge fetches every page and sends only trusted read-only request authority", async t => {
  const calls: URL[] = [];
  const endpoint = await dashboardFixture(t, (request, response) => {
    const url = new URL(request.url!, endpoint);
    calls.push(url);
    assert.equal(request.method, "GET");
    assert.equal(request.headers.host, `127.0.0.1:${new URL(endpoint).port}`);
    assert.equal(request.headers.authorization, undefined);
    assert.equal(request.headers.cookie, undefined);
    assert.equal(url.searchParams.get("limit"), "500");
    assert.equal(url.searchParams.get("public"), "a+b/c");
    const page = fixture(calls.length === 1 ? "feature-1" : "feature-101");
    if (calls.length === 1) {
      page.features = Array.from({ length: 100 }, (_, index) => fixture(`feature-${index + 1}`).features[0]);
      page.next_cursor = "cursor-1";
      response.setHeader("Set-Cookie", "private=must-not-return");
    }
    sendJSON(response, page);
  });
  const data = await fetchSnapshot(new AbortController().signal, `${endpoint}?public=a%2Bb%2Fc&after=discard`);
  assert.equal(calls[0].searchParams.has("after"), false);
  assert.equal(data.features.length, 101);
  assert.equal(data.features[100].feature.id, "feature-101");
  assert.equal(calls[1].searchParams.get("after"), "cursor-1");
  assert.equal(data.next_cursor, undefined);
  assert.deepEqual(data, JSON.parse(JSON.stringify(data)), "RPC results must contain only JSON values, not explicit undefined properties");
});

test("pagination bounds and cursor cycles fail explicitly instead of returning a partial board", async t => {
  let count = 0;
  const endpoint = await dashboardFixture(t, (_request, response) => sendJSON(response, { ...fixture(`feature-${++count}`), next_cursor: "repeated" }));
  await assert.rejects(fetchSnapshot(new AbortController().signal, endpoint, 1), /No partial board/);
  await assert.rejects(fetchSnapshot(new AbortController().signal, endpoint), /repeated a cursor/);
});

test("invalid status, HTTP errors and oversized bodies are rejected", async t => {
  let count = 0;
  const endpoint = await dashboardFixture(t, (_request, response) => {
    if (count++ === 0) sendJSON(response, { paused: false });
    else if (count === 2) { response.writeHead(503); response.end("unavailable"); }
    else { response.writeHead(200, { "content-type": "application/json", "content-length": String(17 * 1024 * 1024) }); response.flushHeaders(); }
  });
  const signal = new AbortController().signal;
  await assert.rejects(fetchSnapshot(signal, endpoint), /invalid status snapshot/);
  await assert.rejects(fetchSnapshot(signal, endpoint), /HTTP 503/);
  await assert.rejects(fetchSnapshot(signal, endpoint), /16 MiB/);
});

test("streamed bodies cannot bypass the response bound by omitting Content-Length", async t => {
  const endpoint = await dashboardFixture(t, (_request, response) => {
    response.setHeader("Content-Type", "application/json");
    const chunk = Buffer.alloc(64 * 1024, 32);
    let remaining = 257;
    const pump = () => {
      while (remaining > 0 && !response.destroyed) {
        remaining--;
        if (!response.write(chunk)) { response.once("drain", pump); return; }
      }
      if (!response.destroyed) response.end();
    };
    pump();
  });
  await assert.rejects(fetchSnapshot(new AbortController().signal, endpoint), /16 MiB/);
});

test("snapshot rejects malformed UTF-8 and every invalid endpoint before connecting", async t => {
  const endpoint = await dashboardFixture(t, (_request, response) => {
    response.setHeader("Content-Type", "application/json");
    response.end(Buffer.from([0x22, 0xc3, 0x28, 0x22]));
  });
  await assert.rejects(fetchSnapshot(new AbortController().signal, endpoint), /encoded data|encoding/i);
  for (const invalid of ["ftp://localhost/api/status", `${endpoint}#fragment`, endpoint.replace("/api/status", "/api/private"), endpoint.replace("localhost", "user:secret@localhost")]) {
    await assert.rejects(fetchSnapshot(new AbortController().signal, invalid), /HTTP\(S\).*without credentials/);
  }
});

test("snapshot does not follow socket redirects", async t => {
  let calls = 0;
  const endpoint = await dashboardFixture(t, (_request, response) => {
    calls++;
    response.writeHead(307, { Location: "/api/status?redirected=true" });
    response.end();
  });
  await assert.rejects(fetchSnapshot(new AbortController().signal, endpoint), /HTTP 307/);
  assert.equal(calls, 1);
});

test("snapshot abort cancels an in-flight body and closes the actual socket", { timeout: 3000 }, async t => {
  let closed: Promise<void> | undefined;
  let started!: () => void;
  const ready = new Promise<void>(resolve => { started = resolve; });
  const endpoint = await dashboardFixture(t, (request, response) => {
    closed = cancelledSocket(request.socket, controller.signal);
    response.writeHead(200, { "Content-Type": "application/json" });
    response.write('{"observed_at":');
    started();
  });
  const controller = new AbortController();
  const rejected = assert.rejects(fetchSnapshot(controller.signal, endpoint), /abort/i);
  await ready;
  controller.abort();
  await rejected;
  assert.ok(closed);
  await closed;
});

test("snapshot deadline aborts before headers and closes the actual socket", { timeout: 5000 }, async t => {
  let closed: Promise<void> | undefined;
  const endpoint = await dashboardFixture(t, request => { closed = cancelledSocket(request.socket, signal); });
  const signal = AbortSignal.timeout(1000);
  await assert.rejects(fetchSnapshot(signal, endpoint), /abort|timeout/i);
  assert.ok(closed, "the deadline must cancel an actual connected request");
  await closed;
});

test("new lane stages and refreshed evidence remain real stages, not invented progress", () => {
  for (const [stage, column] of [["integration_queued", "waiting_to_merge"], ["reconciliation_pending", "waiting_to_merge"], ["reconciling", "waiting_to_merge"], ["checks", "checking"], ["review_pending", "reviewing"], ["integrating", "waiting_to_merge"], ["uncertain", undefined], ["unknown", undefined]] as const) assert.equal(stageColumn(stage), column);
  const status = fixture().features[0];
  assert.equal(featureStage(status), "reconciling");
  status.feature.state = "accepted";
  assert.equal(featureStage(status), "waiting_to_merge");
  status.feature.state = "cancelled";
  assert.equal(featureStage(status), "reconciling");
  status.feature.state = "blocked";
  assert.equal(featureStage(status), "reconciling");
  assert.equal(waiting("integration_queued", "running"), true);
  assert.equal(waiting("build", "uncertain"), true);
});

test("accepted receipts distinguish PR, reviewed, and integrated outcomes", () => {
  const status = fixture().features[0];
  status.feature.state = "accepted";
  status.feature.completion_target = "pull_request";
  status.receipt = "https://github.com/example/repo/pull/12";
  assert.equal(featureStage(status), "waiting_to_merge");
  assert.equal(stageColumn(featureStage(status)), "waiting_to_merge");
  status.feature.completion_target = "reviewed";
  status.receipt = "reviewed:candidate-sha";
  assert.equal(featureStage(status), "reviewed");
  assert.equal(stageColumn(featureStage(status)), "reviewing");
  for (const target of ["integrated", "staging", "production"]) {
    status.feature.completion_target = target;
    status.receipt = "commit-sha";
    assert.equal(featureStage(status), "integrated");
    status.receipt = "";
    assert.equal(featureStage(status), "accepted");
    status.receipt = "https://github.com/example/repo/pull/12";
    assert.equal(featureStage(status), "accepted");
    status.receipt = "reviewed:candidate-sha";
    assert.equal(featureStage(status), "reviewed");
  }
  status.feature.completion_target = "unknown";
  status.receipt = "commit-sha";
  assert.equal(featureStage(status), "accepted");
  assert.equal(stageColumn(featureStage(status)), undefined);
  status.feature.state = "uncertain";
  status.attempts[0].stage = "unknown";
  assert.equal(featureStage(status), "reconciling");
  status.timeline = [];
  assert.equal(featureStage(status), "unknown");
  assert.equal(stageColumn(featureStage(status)), undefined);
});

test("PR navigation permits only credential-free GitHub HTTPS pull requests", () => {
  assert.equal(prLink("https://github.com/example/repo/pull/12"), "https://github.com/example/repo/pull/12");
  for (const value of ["javascript:alert(1)", "https://evil.example/example/repo/pull/12", "https://user@github.com/example/repo/pull/12", "https://github.com/example/repo/pull/12?redirect=evil", "https://github.com/example/repo/pull/12#fragment", "https://github.com/example/repo/issues/12"]) assert.equal(prLink(value), null);
});
