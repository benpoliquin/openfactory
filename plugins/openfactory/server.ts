import type { BbPluginApi } from "@get-bb/plugin-sdk";
import { rpcContract, snapshotSchema, type Snapshot } from "./contract.ts";
import { programHandlers } from "./program-server.ts";
import { dashboardGET, dashboardURL } from "./http-get.ts";

const maxBytes = 16 * 1024 * 1024;
const pageSize = 500;
function pageLimit(value: string | undefined) {
  if (value === undefined) return 20;
  if (!/^[1-9]\d*$/.test(value) || Number(value) > 100) throw new Error("OPENFACTORY_VIEW_MAX_PAGES must be an integer from 1 to 100.");
  return Number(value);
}

// The BB server is the trusted bridge to the read-only dashboard; browser requests
// never receive a dashboard credential or mutation authority.
export async function fetchSnapshot(signal: AbortSignal, endpoint = process.env.OPENFACTORY_VIEW_URL || "http://openfactory:38887/api/status", maxPages = pageLimit(process.env.OPENFACTORY_VIEW_MAX_PAGES)): Promise<Snapshot> {
  const url = dashboardURL(endpoint);
  url.searchParams.set("limit", String(pageSize));
  url.searchParams.delete("after");
  let snapshot: Snapshot | undefined;
  let bytes = 0;
  const cursors = new Set<string>();
  const ids = new Set<string>();
  for (let page = 0; page < maxPages; page++) {
    const response = await dashboardGET(url, signal);
    if (!response.ok) {
      await response.body?.cancel();
      throw new Error(`OpenFactory API returned HTTP ${response.status}.`);
    }
    if (!/^application\/json(?:\s*;|$)/i.test(response.headers.get("content-type") || "")) {
      await response.body?.cancel();
      throw new Error("OpenFactory API returned a non-JSON snapshot.");
    }
    const length = response.headers.get("content-length");
    if (length && Number(length) > maxBytes - bytes) {
      await response.body?.cancel();
      throw new Error("OpenFactory snapshot exceeded the 16 MiB response bound.");
    }
    if (!response.body) throw new Error("OpenFactory API returned an empty snapshot.");
    const reader = response.body.getReader();
    const decoder = new TextDecoder("utf-8", { fatal: true });
    let json = "";
    try {
      while (true) {
        signal.throwIfAborted();
        const chunk = await reader.read();
        if (chunk.done) break;
        bytes += chunk.value.byteLength;
        if (bytes > maxBytes) throw new Error("OpenFactory snapshot exceeded the 16 MiB response bound.");
        json += decoder.decode(chunk.value, { stream: true });
      }
      json += decoder.decode();
    } finally {
      await reader.cancel();
      reader.releaseLock();
    }
    let data: Snapshot;
    try {
      data = snapshotSchema.parse(JSON.parse(json));
    } catch {
      throw new Error("OpenFactory API returned an invalid status snapshot.");
    }
    for (const entry of data.features) {
      if (ids.has(entry.feature.id)) throw new Error("OpenFactory pagination repeated a feature; refresh to retry the snapshot.");
      ids.add(entry.feature.id);
    }
    if (!snapshot) snapshot = data;
    else snapshot.features.push(...data.features);
    if (!data.next_cursor) {
      delete snapshot.next_cursor;
      return snapshot;
    }
    if (cursors.has(data.next_cursor)) throw new Error("OpenFactory pagination repeated a cursor.");
    cursors.add(data.next_cursor);
    url.searchParams.set("after", data.next_cursor);
  }
  throw new Error(`OpenFactory snapshot exceeds the ${maxPages}-page bound; increase OPENFACTORY_VIEW_MAX_PAGES (maximum 100). No partial board is shown.`);
}

export default function plugin(bb: BbPluginApi) {
  const disposed = new AbortController();
  bb.onDispose(() => disposed.abort());
  bb.rpc.register(rpcContract, {
    ...programHandlers(() => AbortSignal.any([disposed.signal, AbortSignal.timeout(8000)])),
    async snapshot() {
      const signal = AbortSignal.any([disposed.signal, AbortSignal.timeout(8000)]);
      try {
        return await fetchSnapshot(signal);
      } catch (error) {
        if (signal.aborted) throw new Error("OpenFactory snapshot request timed out or the plugin stopped.");
        throw new Error(error instanceof Error ? error.message : "OpenFactory API is unreachable.");
      }
    },
  });
}
