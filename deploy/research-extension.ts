// OMP 18.10.3 extension API: extensibility/extensions/types.ts.
// Installed beneath the OMP package. No credentials are returned to the model,
// logged, passed in tool parameters, or placed in source/artifact payloads.
// This is capability reduction for trusted OMP, not a hostile-code sandbox.
import type { ExtensionAPI } from "@oh-my-pi/pi-coding-agent";
import { constants } from "node:fs";
import * as fs from "node:fs/promises";
import * as path from "node:path";
import { createHash } from "node:crypto";

const tools = ["research_search", "research_capture", "research_github", "research_source", "discovery_read", "discovery_write"];
const maxFile = 1 << 20;
const maxInputFile = 64 << 20;
type Session = { program_id: string; revision: number; step_id: string; attempt_id: string; role: string; broker_url: string; capability_token: string; deadline: string; read_only: boolean; allowed_write_paths: string[] };
class SafeError extends Error {
 constructor(code: string, readonly paging?: { start_byte: number; max_bytes: number; total_bytes: number }) { super(code); }
}
const denied = () => new SafeError("discovery_access_denied");

function relativeParts(name: string): string[] {
 if (!name || name.length > 4096 || path.isAbsolute(name) || name.includes("\\") || /[\x00-\x1f:]/.test(name)) throw denied();
 const parts = name.split("/");
 if (parts.some(p => !p || p === "." || p === ".." || p.startsWith("."))) throw denied();
 return parts;
}
async function rootFor(cwd: string): Promise<string> {
 const root = await fs.realpath(cwd);
 if (!/\/discovery\/[0-9a-f]{32}\/[0-9a-f]{32}\/[0-9a-f]{32}\/[0-9a-f]{32}$/.test(root) || root !== path.resolve(cwd)) throw denied();
 return root;
}
async function confined(root: string, name: string, createParents = false): Promise<string> {
 const parts = relativeParts(name); let current = root;
 for (let i = 0; i < parts.length; i++) {
  current = path.join(current, parts[i]);
  let stat;
  try { stat = await fs.lstat(current); } catch (e: unknown) {
   if (!(e instanceof Error) || !("code" in e) || e.code !== "ENOENT" || !createParents) throw denied();
   if (i < parts.length - 1) { await fs.mkdir(current, { mode: 0o700 }); stat = await fs.lstat(current); } else return current;
  }
  if (stat.isSymbolicLink() || i < parts.length - 1 && !stat.isDirectory()) throw denied();
  const actual = await fs.realpath(current);
  if (actual !== current || !actual.startsWith(root + path.sep)) throw denied();
 }
 return current;
}
async function boundedFile(file: string, max: number): Promise<Buffer> {
 const handle = await fs.open(file, constants.O_RDONLY | constants.O_NOFOLLOW);
 try {
  const stat = await handle.stat(); if (!stat.isFile() || stat.nlink !== 1 || stat.size > max) throw denied();
  const bytes = Buffer.alloc(Math.min(max + 1, Math.max(1, stat.size + 1))); let length = 0;
  while (length < bytes.length) { const chunk = await handle.read(bytes, length, bytes.length - length, length); if (!chunk.bytesRead) break; length += chunk.bytesRead; }
  if (length > max || length !== stat.size) throw denied();
  return bytes.subarray(0, length);
 } finally { await handle.close(); }
}

async function bytePage(file: string, name: string, start: number, maximum: number, ceiling: number) {
 if (!Number.isSafeInteger(start) || start < 0 || !Number.isSafeInteger(maximum) || maximum < 1 || maximum > 65536) throw denied();
 const handle = await fs.open(file, constants.O_RDONLY | constants.O_NOFOLLOW);
 try {
  const stat = await handle.stat();
  if (!stat.isFile() || stat.nlink !== 1 || stat.size > ceiling || start > stat.size) throw denied();
  if (start === stat.size) return { path: name, text: "", start_byte: start, total_bytes: stat.size };
  // One lookahead byte determines whether the requested end splits a UTF-8
  // character. Never read the full file merely to serve a bounded byte page.
  const expected = Math.min(maximum + 1, stat.size - start);
  const bytes = Buffer.alloc(expected); let read = 0;
  while (read < expected) {
   const chunk = await handle.read(bytes, read, expected - read, start + read);
   if (!chunk.bytesRead) break;
   read += chunk.bytesRead;
  }
  if (read !== expected || (await handle.stat()).size !== stat.size || (bytes[0] & 0xc0) === 0x80) throw denied();
  let end = Math.min(maximum, read);
  if (start + end < stat.size) while (end > 0 && (bytes[end] & 0xc0) === 0x80) end--;
  if (end === 0) throw new SafeError("discovery_page_too_small");
  const text = new TextDecoder("utf-8", { fatal: true, ignoreBOM: true }).decode(bytes.subarray(0, end));
  if (text.includes("\x00")) throw denied();
  return { path: name, text, start_byte: start, total_bytes: stat.size, ...(start + end < stat.size ? { next_byte: start + end } : {}) };
 } finally { await handle.close(); }
}
function namespaceDigest(value: string) { return createHash("sha256").update(value).digest("hex").slice(0, 32); }
async function session(cwd: string): Promise<{ root: string; control: Session }> {
 const root = await rootFor(cwd);
 const directory = path.join(root, ".openfactory"); const directoryStat = await fs.lstat(directory);
 if (!directoryStat.isDirectory() || directoryStat.isSymbolicLink()) throw denied();
 const file = path.join(directory, "discovery-session.json"); const stat = await fs.lstat(file);
 if (!stat.isFile() || stat.isSymbolicLink() || (stat.mode & 0o077) !== 0) throw denied();
 const control = JSON.parse((await boundedFile(file, 16384)).toString("utf8")) as Session;
 const segments = root.split("/");
 if (!control.program_id || !control.attempt_id || namespaceDigest(control.attempt_id) !== segments.at(-1) || namespaceDigest(control.step_id) !== segments.at(-2) || namespaceDigest(control.role) !== segments.at(-3) || typeof control.capability_token !== "string" || control.capability_token.length < 32 || !Number.isFinite(Date.parse(control.deadline)) || Date.parse(control.deadline) <= Date.now()) throw denied();
 const broker = new URL(control.broker_url);
 if (!["http:", "https:"].includes(broker.protocol) || broker.username || broker.password || broker.search || broker.hash || broker.pathname !== "/") throw denied();
 return { root, control };
}
function output(value: unknown) { return { content: [{ type: "text" as const, text: JSON.stringify(value) }], details: {} }; }
async function safe(fn: () => Promise<unknown>) {
 try { return output(await fn()); } catch (e) { return { ...output({ error: { code: e instanceof SafeError ? e.message : "discovery_operation_failed" }, ...(e instanceof SafeError && e.paging ? e.paging : {}) }), isError: true }; }
}
async function broker(cwd: string, toolCallID: string, route: string, params: Record<string, unknown>, signal?: AbortSignal) {
 const { control } = await session(cwd);
 const requestID = createHash("sha256").update(control.attempt_id + "\x00" + toolCallID + "\x00" + route + "\x00" + JSON.stringify(params)).digest("hex");
 const url = new URL(route, control.broker_url);
 const deadline = Math.min(35000, Date.parse(control.deadline) - Date.now()); if (deadline <= 0) throw denied();
 const timeout = AbortSignal.timeout(deadline); const combined = signal ? AbortSignal.any([signal, timeout]) : timeout;
 const response = await fetch(url, { method: "POST", headers: { "Content-Type": "application/json", Authorization: `Bearer ${control.capability_token}` }, body: JSON.stringify({ ...params, request_id: requestID }), signal: combined, redirect: "error" });
 return await responseJSON(response);
}
async function responseJSON(response: Response) {
 if (!response.body) throw new SafeError("research_unavailable");
 const reader = response.body.getReader(); const chunks: Uint8Array[] = []; let size = 0;
 try { while (true) { const { done, value } = await reader.read(); if (done) break; size += value.byteLength; if (size > 512 << 10) { await reader.cancel(); throw new SafeError("research_response_too_large"); } chunks.push(value); } } finally { reader.releaseLock(); }
 const text = Buffer.concat(chunks).toString("utf8"); const value = JSON.parse(text);
 if (!response.ok) { const code = value?.error?.code; throw new SafeError(typeof code === "string" && /^[a-z_]{1,64}$/.test(code) ? code : "research_unavailable"); }
 return value;
}

export default function (pi: ExtensionAPI) {
 const z = pi.zod;
 pi.on("session_start", async (_event, ctx) => { await session(ctx.cwd); await pi.setActiveTools(tools); });
 pi.on("tool_call", async (event) => { if (!tools.includes(event.toolName)) return { block: true, reason: "discovery_tool_not_permitted" }; });
 const string = () => z.string().min(1).max(8192);
 pi.registerTool({ name: "research_search", label: "Research search", loadMode: "essential", approval: "read", description: "Search only an operator-approved public topic ID. Results are leads, not captured evidence.", parameters: z.object({ topic_id: string() }), async execute(id, params, signal, _update, ctx) { return safe(() => broker(ctx.cwd, id, "/v1/research/search", params, signal)); } });
 const capture = z.object({ seed_id: string().optional(), lead_id: string().optional(), url: string().optional() });
 pi.registerTool({ name: "research_capture", label: "Capture public source", loadMode: "essential", approval: "read", description: "Capture the complete bounded public body from an approved seed, recorded search lead, or captured href. URL must match that authority exactly. Returns controller-assigned hashes; never supply invented source metadata.", parameters: capture, async execute(id, params, signal, _update, ctx) { return safe(() => broker(ctx.cwd, id, "/v1/research/capture", params, signal)); } });
 pi.registerTool({ name: "research_github", label: "Capture pinned GitHub source", loadMode: "essential", approval: "read", description: "Resolve an approved public GitHub blob URL to a full commit and capture pinned content. Only seed/lead URLs are accepted; no private repository credentials.", parameters: capture, async execute(id, params, signal, _update, ctx) { return safe(() => broker(ctx.cwd, id, "/v1/research/github", params, signal)); } });
 pi.registerTool({ name: "research_source", label: "Read captured source", loadMode: "essential", approval: "read", description: "Read immutable captured text and provenance. Lines are one-based; next_line is the next page. Quote occurrence is not proof of semantic truth.", parameters: z.object({ source_id: string(), start_line: z.number().int().min(1).optional(), limit: z.number().int().min(1).max(500).optional() }), async execute(_id, params, signal, _update, ctx) { return safe(async () => { const { control } = await session(ctx.cwd); if (!/^[A-Za-z0-9_-]{1,128}$/.test(params.source_id)) throw denied(); const url = new URL(`/v1/research/sources/${params.source_id}`, control.broker_url); if (params.start_line) url.searchParams.set("start_line", String(params.start_line)); if (params.limit) url.searchParams.set("limit", String(params.limit)); const timeout = AbortSignal.timeout(Math.max(1, Math.min(35000, Date.parse(control.deadline) - Date.now()))); return responseJSON(await fetch(url, { headers: { Authorization: `Bearer ${control.capability_token}` }, signal: signal ? AbortSignal.any([signal, timeout]) : timeout, redirect: "error" })); }); } });
 pi.registerTool({
  name: "discovery_read", label: "Read discovery workspace", loadMode: "essential", approval: "read",
  description: "Read only this discovery workspace. Byte mode: start_byte (default 0), max_bytes (default 65536), follow next_byte until omitted; exact UTF-8 boundaries, at most 64 KiB per page. Byte and line arguments are mutually exclusive. inputs/ permits controller-materialized files up to 64 MiB; other files at most 1 MiB. Large inputs/long lines return discovery_byte_paging_required with retry fields. Use path '.' to list root. No URLs, hidden files, symlinks, hardlinks, controlfiles or external filesystem.",
  parameters: z.object({ path: string(), start_line: z.number().int().min(1).optional(), limit: z.number().int().min(1).max(500).optional(), start_byte: z.number().int().min(0).optional(), max_bytes: z.number().int().min(1).max(65536).optional() }).refine(p => !((p.start_byte !== undefined || p.max_bytes !== undefined) && (p.start_line !== undefined || p.limit !== undefined)), { message: "byte and line paging arguments are mutually exclusive" }),
  async execute(_id, params, _signal, _update, ctx) {
   return safe(async () => {
    const { root } = await session(ctx.cwd);
    const byteMode = params.start_byte !== undefined || params.max_bytes !== undefined;
    if (byteMode && (params.start_line !== undefined || params.limit !== undefined)) throw denied();
    const file = params.path === "." ? root : await confined(root, params.path);
    const stat = await fs.lstat(file);
    const ceiling = params.path.startsWith("inputs/") ? maxInputFile : maxFile;
    if (byteMode) return bytePage(file, params.path, params.start_byte ?? 0, params.max_bytes ?? 65536, ceiling);
    if (stat.isDirectory()) {
     const names = await fs.readdir(file, { withFileTypes: true });
     const visible = names.filter(n => !n.name.startsWith(".") && !n.isSymbolicLink());
     if (visible.length > 200) throw new SafeError("discovery_directory_too_large");
     return { entries: visible.map(n => ({ name: n.name, directory: n.isDirectory() })) };
    }
    if (!stat.isFile() || stat.nlink !== 1 || stat.size > ceiling) throw denied();
    if (stat.size > maxFile) throw new SafeError("discovery_byte_paging_required", { start_byte: 0, max_bytes: 65536, total_bytes: stat.size });
    const data = await boundedFile(file, maxFile);
    const text = new TextDecoder("utf-8", { fatal: true, ignoreBOM: true }).decode(data);
    if (text.includes("\x00")) throw denied();
    const lines = text.split("\n"); const start = (params.start_line ?? 1) - 1;
    if (start >= lines.length) throw denied();
    let end = start, size = 0;
    while (end < lines.length && end - start < (params.limit ?? 100)) {
     const length = Buffer.byteLength(lines[end]) + 1;
     if (size + length > 65536) { if (end === start) throw new SafeError("discovery_byte_paging_required", { start_byte: start === 0 ? 0 : Buffer.byteLength(lines.slice(0, start).join("\n")) + 1, max_bytes: 65536, total_bytes: stat.size }); break; }
     size += length; end++;
    }
    return { path: params.path, start_line: start + 1, text: lines.slice(start, end).join("\n"), ...(end < lines.length ? { next_line: end + 1 } : {}) };
   });
  },
 });
 pi.registerTool({
  name: "discovery_write", label: "Write discovery preparation", loadMode: "essential", approval: "write",
  description: "Write a UTF-8 artifact/experiment preparation file only in controller-allowed relative paths. Available only to architecture/contracts roles; cannot modify product code, controlfiles or evaluator inputs. Does not execute experiments or establish evidence.",
  parameters: z.object({ path: string(), content: z.string().max(maxFile) }),
  async execute(_id, params, _signal, _update, ctx) {
   return safe(async () => {
    const { root, control } = await session(ctx.cwd); const parts = relativeParts(params.path);
    if (control.read_only || !["architecture", "contracts"].includes(control.role) || !["artifacts", "experiments"].includes(parts[0]) || !Array.isArray(control.allowed_write_paths) || !control.allowed_write_paths.some(prefix => { relativeParts(prefix); return params.path === prefix || params.path.startsWith(prefix + "/"); })) throw denied();
    const bytes = Buffer.from(params.content, "utf8"); if (bytes.length > maxFile) throw denied();
    const file = await confined(root, params.path, true);
    // Check hard-link identity BEFORE truncation. Hidden/control paths and
    // symlinks are rejected independently above and by O_NOFOLLOW here.
    const handle = await fs.open(file, constants.O_WRONLY | constants.O_CREAT | constants.O_NOFOLLOW, 0o600);
    try {
     const stat = await handle.stat(); if (!stat.isFile() || stat.nlink !== 1) throw denied();
     await handle.truncate(0); await handle.writeFile(bytes); await handle.sync();
    } finally { await handle.close(); }
    return { path: params.path, bytes: bytes.length };
   });
  },
 });
}
