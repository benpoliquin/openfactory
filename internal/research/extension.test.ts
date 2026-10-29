// Run with Bun in the packaged BB image (OMP 18.10.3 installed). These are
// custom-tool boundary tests, not proof of LLM use or public-source truth.
import { afterEach, expect, test } from "bun:test";
import type { ExtensionAPI, ExtensionContext } from "@oh-my-pi/pi-coding-agent";
import { createRequire } from "node:module";
import { createHash } from "node:crypto";
import * as fs from "node:fs/promises";
import * as os from "node:os";
import * as path from "node:path";
import register from "../../deploy/research-extension";

const ompPackage = process.env.OPENFACTORY_OMP_PACKAGE ?? "/usr/local/lib/node_modules/@oh-my-pi/pi-coding-agent";
const requireOMP = createRequire(path.join(ompPackage, "package.json"));
const zod: ExtensionAPI["zod"] = requireOMP("@oh-my-pi/omptype/zod");
const cleanup: string[] = [];
afterEach(async () => { for (const directory of cleanup.splice(0)) await fs.rm(directory, { recursive: true, force: true }); });
type ToolResult = { content: Array<{ text?: string }>; isError?: boolean };
type Registered = { name: string; parameters: { parse(value: unknown): unknown } };
// The runtime adapter is imported from the same pinned installation as Zod.
// This narrow module boundary avoids substituting its dispatch implementation.
type AdapterModule = {
 RegisteredToolAdapter: new (tool: { definition: Registered; extensionPath: string }, runner: { createContext(): ExtensionContext }) => {
  execute(id: string, args: unknown, signal?: AbortSignal, update?: undefined): Promise<ToolResult>;
 };
};
// OPENFACTORY_OMP_PACKAGE selects the installed module at runtime, so a static import cannot resolve it.
const { RegisteredToolAdapter } = await import(path.join(ompPackage, "src/extensibility/extensions/wrapper.ts")) as AdapterModule;
type Hook = (event: { toolName?: string }, ctx: ExtensionContext) => Promise<unknown>;
function registry() {
 const registered = new Map<string, Registered>(); const hooks = new Map<string, Hook>(); let active: string[] = [];
 const api = { zod, registerTool(tool: Registered) { registered.set(tool.name, tool); }, on(event: string, hook: Hook) { hooks.set(event, hook); }, async setActiveTools(names: string[]) { active = names; } };
 // Only the observed registerTool/on/setActiveTools surface is needed by this
 // test host. Actual schema parsing comes from the pinned OMP dependency.
 register(api as unknown as ExtensionAPI);
 return { registered, hooks, active: () => active };
}
async function fixture(role: string, brokerURL = "http://127.0.0.1:38888/") {
 const temporary = await fs.realpath(await fs.mkdtemp(path.join(os.tmpdir(), "openfactory-extension-"))); cleanup.push(temporary);
 const digest = (s: string) => createHash("sha256").update(s).digest("hex").slice(0, 32);
 const cwd = path.join(temporary, "discovery", digest("project"), digest(role), digest("step"), digest("attempt"));
 await fs.mkdir(path.join(cwd, ".openfactory"), { recursive: true, mode: 0o700 });
 const control = { program_id: "program", revision: 1, role, step_id: "step", attempt_id: "attempt", broker_url: brokerURL, capability_token: "test-opaque-capability-never-model-output", deadline: new Date(Date.now() + 60000).toISOString(), read_only: !["architecture", "contracts"].includes(role), allowed_write_paths: ["experiments", "artifacts"] };
 await fs.writeFile(path.join(cwd, ".openfactory", "discovery-session.json"), JSON.stringify(control), { mode: 0o600 });
 // Tools in this extension use only ctx.cwd; model/session internals are not
 // substituted for any authority decision in the test.
 const ctx = { cwd } as unknown as ExtensionContext;
 return { cwd, ctx, control, temporary };
}
async function invoke(tool: Registered | undefined, ctx: ExtensionContext, input: unknown, id = "call") {
 if (!tool) throw new Error("tool not registered");
 const adapter = new RegisteredToolAdapter({ definition: tool, extensionPath: path.resolve("deploy/research-extension.ts") }, { createContext: () => ctx });
 return adapter.execute(id, tool.parameters.parse(input), undefined, undefined);
}

test("discovery registers and activates exactly six tools and blocks native capabilities", async () => {
 const host = registry(); const { ctx } = await fixture("research");
 await host.hooks.get("session_start")!({}, ctx);
 expect(host.active().sort()).toEqual(["research_search", "research_capture", "research_github", "research_source", "discovery_read", "discovery_write"].sort());
 for (const toolName of ["bash", "eval", "task", "web_search", "read", "write"]) expect(await host.hooks.get("tool_call")!({ toolName }, ctx)).toEqual({ block: true, reason: "discovery_tool_not_permitted" });
});

test("workspace tools reject controlfiles, symlinks, traversal, URLs, oversize, and evaluator writes", async () => {
 const host = registry(); const { cwd, ctx, temporary, control } = await fixture("plan_evaluation");
 await fs.writeFile(path.join(temporary, "outside"), "outside secret");
 await fs.symlink(path.join(temporary, "outside"), path.join(cwd, "link"));
 await fs.writeFile(path.join(cwd, "oversize"), Buffer.alloc((1 << 20) + 1));
 for (const name of [".openfactory/discovery-session.json", "../outside", path.join(temporary, "outside"), "https://example.com/", "link", "oversize"]) {
  const result = await invoke(host.registered.get("discovery_read"), ctx, { path: name });
  expect(result.isError).toBe(true); expect(JSON.stringify(result)).not.toContain(control.capability_token); expect(JSON.stringify(result)).not.toContain("outside secret");
 }
 const write = await invoke(host.registered.get("discovery_write"), ctx, { path: "experiments/run.py", content: "print('not executed')" }); expect(write.isError).toBe(true);
 const listing = await invoke(host.registered.get("discovery_read"), ctx, { path: "." }); expect(JSON.stringify(listing)).not.toContain(".openfactory"); expect(JSON.stringify(listing)).not.toContain('"name":"link"');
});

test("architecture writes only controller-approved preparation prefixes", async () => {
 const host = registry(); const { cwd, ctx } = await fixture("architecture");
 const allowed = await invoke(host.registered.get("discovery_write"), ctx, { path: "experiments/run.py", content: "print('prepared only')\n" }); expect(allowed.isError).not.toBe(true);
 expect(await fs.readFile(path.join(cwd, "experiments", "run.py"), "utf8")).toBe("print('prepared only')\n");
 for (const name of ["src/product.go", ".openfactory/discovery-session.json", "experiments/../escape", "artifacts/.secret"]) expect((await invoke(host.registered.get("discovery_write"), ctx, { path: name, content: "denied" })).isError).toBe(true);
});

test("broker uses opaque bearer and stable controller request ID without exposing capability", async () => {
 const requests: Array<{ authorization: string | null; body: Record<string, unknown> }> = [];
 const server = Bun.serve({ port: 0, async fetch(request) { requests.push({ authorization: request.headers.get("authorization"), body: await request.json() as Record<string, unknown> }); return Response.json({ search: { query_id: "approved-topic", leads: [], status: "completed" } }); } });
 try {
  const host = registry(); const { ctx, control } = await fixture("research", `http://127.0.0.1:${server.port}/`);
  const first = await invoke(host.registered.get("research_search"), ctx, { topic_id: "approved-topic" }, "stable-call");
  await invoke(host.registered.get("research_search"), ctx, { topic_id: "approved-topic" }, "stable-call");
  expect(requests).toHaveLength(2); expect(requests[0].authorization).toBe(`Bearer ${control.capability_token}`); expect(requests[0].body).toEqual(requests[1].body); expect(Object.keys(requests[0].body).sort()).toEqual(["request_id", "topic_id"]); expect(requests[0].body.request_id).toMatch(/^[0-9a-f]{64}$/); expect(JSON.stringify(first)).not.toContain(control.capability_token);
 } finally { server.stop(true); }
});

function payload(result: { content: Array<{ text?: string }> }) {
 expect(result.content).toHaveLength(1);
 return JSON.parse(result.content[0].text!);
}

test("discovery byte pages preserve UTF-8 boundaries and return the exact final page", async () => {
 const host = registry(); const { cwd, ctx } = await fixture("research");
 const name = "unicode.txt"; const text = "Aé€😀Z";
 await fs.writeFile(path.join(cwd, name), text);
 const tool = host.registered.get("discovery_read");
 // Two bytes cannot include the two-byte é after A: back off, never replace it.
 const first = await invoke(tool, ctx, { path: name, start_byte: 0, max_bytes: 2 });
 expect(first.isError).not.toBe(true);
 expect(payload(first)).toEqual({ path: name, text: "A", start_byte: 0, next_byte: 1, total_bytes: 11 });
 const second = await invoke(tool, ctx, { path: name, start_byte: 1, max_bytes: 5 });
 expect(second.isError).not.toBe(true);
 expect(payload(second)).toEqual({ path: name, text: "é€", start_byte: 1, next_byte: 6, total_bytes: 11 });
 const last = await invoke(tool, ctx, { path: name, start_byte: 6, max_bytes: 5 });
 expect(last.isError).not.toBe(true);
 expect(payload(last)).toEqual({ path: name, text: "😀Z", start_byte: 6, total_bytes: 11 });
 expect([first, second, last].map(page => payload(page).text).join("")).toBe(text);
 // Every continuation byte is an invalid starting offset, not replacement text.
 for (const start_byte of [2, 4, 5, 7, 8, 9]) {
  const invalid = await invoke(tool, ctx, { path: name, start_byte, max_bytes: 5 });
  expect(invalid.isError).toBe(true);
 }
 for (const [start_byte, max_bytes] of [[1, 1], [3, 2], [6, 3]]) {
  const small = await invoke(tool, ctx, { path: name, start_byte, max_bytes });
  expect(small.isError).toBe(true);
  expect(payload(small).error.code).toBe("discovery_page_too_small");
 }
});

test("discovery byte paging reconstructs inputs above 1 MiB and line reads advertise a retry", async () => {
 const host = registry(); const { cwd, ctx } = await fixture("research");
 await fs.mkdir(path.join(cwd, "inputs"));
 const name = "inputs/large.txt"; const text = "é😀\n".repeat(160000);
 const total_bytes = Buffer.byteLength(text);
 expect(total_bytes).toBeGreaterThan(1 << 20);
 await fs.writeFile(path.join(cwd, name), text);
 const tool = host.registered.get("discovery_read");
 const line = await invoke(tool, ctx, { path: name, start_line: 1, limit: 1 });
 expect(line.isError).toBe(true);
 expect(payload(line)).toEqual(expect.objectContaining({ error: { code: "discovery_byte_paging_required" }, start_byte: 0, max_bytes: 65536, total_bytes }));
 const pages: string[] = []; let start_byte = 0;
 while (start_byte < total_bytes) {
  const result = await invoke(tool, ctx, { path: name, start_byte, max_bytes: 65536 });
  expect(result.isError).not.toBe(true);
  const page = payload(result); const length = Buffer.byteLength(page.text);
  expect(page.path).toBe(name); expect(page.start_byte).toBe(start_byte); expect(page.total_bytes).toBe(total_bytes);
  expect(length).toBeGreaterThan(0); expect(length).toBeLessThanOrEqual(65536);
  pages.push(page.text); start_byte += length;
  if (start_byte === total_bytes) expect(page).not.toHaveProperty("next_byte");
  else expect(page.next_byte).toBe(start_byte);
 }
 expect(pages.join("")).toBe(text);
 await fs.writeFile(path.join(cwd, "outside-inputs.txt"), text);
 const outside = await invoke(tool, ctx, { path: "outside-inputs.txt", start_byte: 0, max_bytes: 65536 });
 expect(outside.isError).toBe(true);
 expect(payload(outside).error.code).toBe("discovery_access_denied");
});

test("discovery long single lines advertise byte paging instead of truncating", async () => {
 const host = registry(); const { cwd, ctx } = await fixture("research");
 const text = "€".repeat(24000); const name = "long-line.txt";
 await fs.writeFile(path.join(cwd, name), text);
 const tool = host.registered.get("discovery_read");
 const line = await invoke(tool, ctx, { path: name });
 expect(line.isError).toBe(true);
 expect(payload(line)).toEqual(expect.objectContaining({ error: { code: "discovery_byte_paging_required" }, start_byte: 0, max_bytes: 65536, total_bytes: Buffer.byteLength(text) }));
 const first = await invoke(tool, ctx, { path: name, start_byte: 0, max_bytes: 65536 });
 expect(first.isError).not.toBe(true);
 expect(payload(first)).toEqual({ path: name, text: "€".repeat(21845), start_byte: 0, next_byte: 65535, total_bytes: 72000 });
 const last = await invoke(tool, ctx, { path: name, start_byte: 65535, max_bytes: 65536 });
 expect(last.isError).not.toBe(true);
 expect(payload(first).text + payload(last).text).toBe(text);
 expect(payload(last)).not.toHaveProperty("next_byte");
});

test("discovery read schema rejects mixed paging modes and invalid byte ranges", async () => {
 const tool = registry().registered.get("discovery_read")!;
 for (const byte of [{ start_byte: 0 }, { max_bytes: 8 }, { start_byte: 0, max_bytes: 8 }]) {
  for (const line of [{ start_line: 1 }, { limit: 1 }, { start_line: 1, limit: 1 }]) {
   expect(() => tool.parameters.parse({ path: "file.txt", ...byte, ...line })).toThrow();
  }
 }
 for (const byte of [{ start_byte: -1 }, { start_byte: 0.5 }, { max_bytes: 0 }, { max_bytes: 65537 }, { max_bytes: 1.5 }]) {
  expect(() => tool.parameters.parse({ path: "file.txt", ...byte })).toThrow();
 }
});

test("discovery byte paging preserves a UTF-8 BOM and exact EOF offsets", async () => {
 const host = registry(); const { cwd, ctx } = await fixture("research");
 await fs.writeFile(path.join(cwd, "bom.txt"), "\ufeffZ");
 const tool = host.registered.get("discovery_read");
 const first = await invoke(tool, ctx, { path: "bom.txt", start_byte: 0, max_bytes: 3 });
 expect(payload(first)).toEqual({ path: "bom.txt", text: "\ufeff", start_byte: 0, next_byte: 3, total_bytes: 4 });
 const last = await invoke(tool, ctx, { path: "bom.txt", start_byte: 3, max_bytes: 1 });
 expect(payload(last)).toEqual({ path: "bom.txt", text: "Z", start_byte: 3, total_bytes: 4 });
 const eof = await invoke(tool, ctx, { path: "bom.txt", start_byte: 4, max_bytes: 1 });
 expect(payload(eof)).toEqual({ path: "bom.txt", text: "", start_byte: 4, total_bytes: 4 });
});
