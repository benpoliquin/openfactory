import { createServer, type RequestListener, type ServerResponse } from "node:http";
import { once } from "node:events";
import type { TestContext } from "node:test";
import assert from "node:assert/strict";
import type { Socket } from "node:net";

export async function dashboardFixture(t: TestContext, handler: RequestListener): Promise<string> {
  const server = createServer(handler);
  server.listen(0);
  await once(server, "listening");
  t.after(() => {
    server.close();
    server.closeAllConnections();
  });
  const address = server.address();
  if (!address || typeof address === "string") throw new Error("Expected a TCP test server.");
  return `http://localhost:${address.port}/api/status`;
}

export function configureDashboard(t: TestContext, endpoint: string): void {
  const previous = process.env.OPENFACTORY_VIEW_URL;
  process.env.OPENFACTORY_VIEW_URL = endpoint;
  t.after(() => {
    if (previous === undefined) delete process.env.OPENFACTORY_VIEW_URL;
    else process.env.OPENFACTORY_VIEW_URL = previous;
  });
}

export function sendJSON(response: ServerResponse, data: unknown): void {
  response.setHeader("Content-Type", "application/json");
  response.end(JSON.stringify(data));
}

export async function cancelledSocket(socket: Socket, signal: AbortSignal): Promise<void> {
  const errors: { error: NodeJS.ErrnoException; cancelled: boolean }[] = [];
  socket.on("error", error => { errors.push({ error, cancelled: signal.aborted }); });
  // events.once rejects on error, but a peer's intentional abort may reset TCP.
  // Observe closure independently, then validate every error rather than hiding it.
  await new Promise<void>(resolve => { socket.once("close", () => resolve()); });
  assert.equal(socket.destroyed, true, "cancellation must destroy the connected socket");
  for (const { error, cancelled } of errors) {
    assert.equal(cancelled, true, "socket errors before intentional cancellation are unexpected");
    assert.equal(error.code, "ECONNRESET", error.message);
  }
}
