import { request as httpRequest } from "node:http";
import { request as httpsRequest } from "node:https";
import { isIP } from "node:net";
import { Readable } from "node:stream";

export function dashboardURL(endpoint: string): URL {
  const url = new URL(endpoint);
  if (!/^https?:$/.test(url.protocol) || url.username || url.password || url.hash || url.pathname !== "/api/status") {
    throw new Error("OPENFACTORY_VIEW_URL must be an HTTP(S) /api/status URL without credentials or a fragment.");
  }
  return url;
}

type DashboardResponse = {
  ok: boolean;
  status: number;
  headers: Headers;
  body: ReadableStream<Uint8Array>;
};

// Keep the network destination and TLS identity from the configured URL. Only
// dashboard HTTP authority is loopback: Node's native fetch ignores Host overrides.
// No agent, proxy, cookie jar, redirect handling, or ambient credentials are used.
export function dashboardGET(url: URL, signal: AbortSignal): Promise<DashboardResponse> {
  // The plugin targets ES2022, whose Promise API predates withResolvers.
  return new Promise((resolve, reject) => {
    signal.throwIfAborted();
    const secure = url.protocol === "https:";
    const hostname = url.hostname.replace(/^\[|\]$/g, "");
    const request = (secure ? httpsRequest : httpRequest)(url, {
      method: "GET",
      signal,
      agent: false,
      // https otherwise derives SNI from the overridden Host header. For an IP,
      // omit SNI; Node still verifies the original destination IP certificate.
      ...(secure ? { servername: isIP(hostname) ? "" : hostname } : {}),
      headers: {
        Accept: "application/json",
        Host: `127.0.0.1:${url.port || (secure ? "443" : "80")}`,
      },
    }, response => {
      const headers = new Headers();
      for (let index = 0; index < response.rawHeaders.length; index += 2) {
        headers.append(response.rawHeaders[index], response.rawHeaders[index + 1]);
      }
      const status = response.statusCode ?? 0;
      resolve({
        ok: status >= 200 && status < 300,
        status,
        headers,
        // Bound queued bytes as well as the callers' streamed JSON limits.
        body: Readable.toWeb(response, {
          strategy: { highWaterMark: 64 * 1024, size: chunk => chunk.byteLength },
        }) as ReadableStream<Uint8Array>,
      });
    });
    request.on("error", reject);
    request.end();
  });
}
