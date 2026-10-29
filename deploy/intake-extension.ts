// Pinned OMP 18.10.3 capability boundary, not a hostile-code OS sandbox.
import type { ExtensionAPI } from "@oh-my-pi/pi-coding-agent";
import { constants, type Stats } from "node:fs";
import * as fs from "node:fs/promises";
import * as path from "node:path";
import { createHash } from "node:crypto";

const ceiling = 64 << 20;
const decode = () => new TextDecoder("utf-8", { fatal: true, ignoreBOM: true });
const deny = () => new Error("intake_access_denied");
const digest = (s: string) => createHash("sha256").update(s).digest("hex").slice(0, 32);
function parts(name: string) {
 if (!name || name.length > 4096 || /[\x00-\x20\x7f:\\]/.test(name) || decode().decode(Buffer.from(name)) !== name) throw deny();
 const result = name.split("/");
 if (result.some(p => !p || p.startsWith(".")) || result[0] !== "inputs" || result.length < 2) throw deny();
 return result;
}
async function confined(root: string, name: string) {
 let current = root;
 for (const segment of parts(name)) {
  current = path.join(current, segment);
  const stat = await fs.lstat(current);
  if (stat.isSymbolicLink() || await fs.realpath(current) !== current) throw deny();
 }
 return current;
}
function stamp(stat: Stats) { return [stat.dev, stat.ino, stat.size, stat.mtimeMs, stat.ctimeMs, stat.mode, stat.nlink].join(":"); }
function validFile(stat: Stats) { if (!stat.isFile() || stat.nlink !== 1 || stat.size > ceiling || (stat.mode & 0o222)) throw deny(); }
async function session(cwd: string) {
 const root = await fs.realpath(cwd);
 if (root !== path.resolve(cwd) || !/\/intake\/(?:[0-9a-f]{32}\/){3}[0-9a-f]{32}$/.test(root)) throw deny();
 const directory = path.join(root, ".openfactory");
 if (!(await fs.lstat(directory)).isDirectory() || await fs.realpath(directory) !== directory) throw deny();
 const file = await fs.open(path.join(directory, "intake-session.json"), constants.O_RDONLY | constants.O_NOFOLLOW);
 let control;
 try {
  const stat = await file.stat();
  if (!stat.isFile() || stat.nlink !== 1 || (stat.mode & 0o777) !== 0o600 || stat.size > 1 << 20) throw deny();
  control = JSON.parse(decode().decode(await file.readFile()));
 } finally { await file.close(); }
 const keys = ["request_id", "step_id", "attempt_id", "project", "deadline", "input_files"];
 if (!control || Array.isArray(control) || Object.keys(control).sort().join() !== keys.sort().join()) throw deny();
 for (const key of keys.slice().filter(k => k !== "input_files")) if (typeof control[key] !== "string" || !control[key] || /[\x00-\x1f\x7f]/.test(control[key])) throw deny();
 if (!Number.isFinite(Date.parse(control.deadline)) || Date.parse(control.deadline) <= Date.now()) throw deny();
 if ([control.project, control.request_id, control.step_id, control.attempt_id].map(digest).join("/") !== root.split("/").slice(-4).join("/")) throw deny();
 if (!control.input_files || typeof control.input_files !== "object" || Array.isArray(control.input_files)) throw deny();
 for (const [name, hash] of Object.entries(control.input_files)) { parts(name); if (typeof hash !== "string" || !/^[0-9a-f]{64}$/.test(hash)) throw deny(); }
 return { root, control };
}
function output(value: unknown) { return { content: [{ type: "text" as const, text: JSON.stringify(value) }], details: {} }; }
export default function (pi: ExtensionAPI) {
 const verified = new Map<string, string>();
 pi.on("session_start", async (_event, ctx) => { verified.clear(); await session(ctx.cwd); await pi.setActiveTools(["intake_read"]); });
 pi.on("tool_call", async event => { if (event.toolName !== "intake_read") return { block: true, reason: "intake_tool_not_permitted" }; });
 const z = pi.zod;
 pi.registerTool({ name: "intake_read", label: "Read disclosed intake input", loadMode: "essential", approval: "read",
  description: "Read only declared immutable inputs/. UTF-8 byte pages, at most 65536 bytes; follow next_byte until omitted. No listing or line mode.",
  parameters: z.object({ path: z.string().min(1).max(4096), start_byte: z.number().int().min(0).max(Number.MAX_SAFE_INTEGER).optional(), max_bytes: z.number().int().min(1).max(65536).optional() }).strict(),
  async execute(_id, params, signal, _update, ctx) {
   try {
    const { root, control } = await session(ctx.cwd);
    if (!Object.hasOwn(control.input_files, params.path)) throw deny();
    const name = await confined(root, params.path);
    const file = await fs.open(name, constants.O_RDONLY | constants.O_NOFOLLOW);
    try {
     const stat = await file.stat(); validFile(stat);
     const identity = stamp(stat); const expectedHash = control.input_files[params.path];
     const cacheKey = name + ":" + expectedHash;
     if (verified.get(cacheKey) !== identity) {
      const hash = createHash("sha256"); const decoder = decode(); const buffer = Buffer.alloc(65536); let offset = 0;
      while (offset < stat.size) {
       signal?.throwIfAborted();
       const { bytesRead } = await file.read(buffer, 0, Math.min(buffer.length, stat.size - offset), offset);
       if (!bytesRead) throw deny();
       const chunk = buffer.subarray(0, bytesRead); hash.update(chunk);
       if (decoder.decode(chunk, { stream: true }).includes("\x00")) throw deny();
       offset += bytesRead;
      }
      if (decoder.decode().includes("\x00") || hash.digest("hex") !== expectedHash || stamp(await file.stat()) !== identity) throw deny();
      verified.set(cacheKey, identity);
     }
     const start = params.start_byte ?? 0, maximum = params.max_bytes ?? 65536;
     if (start > stat.size) throw deny();
     const buffer = Buffer.alloc(Math.min(maximum + 1, stat.size - start)); let read = 0;
     while (read < buffer.length) { const result = await file.read(buffer, read, buffer.length - read, start + read); if (!result.bytesRead) throw deny(); read += result.bytesRead; }
     if (stamp(await file.stat()) !== identity || stamp(await fs.lstat(name)) !== identity || Date.parse(control.deadline) <= Date.now()) throw deny();
     if (read && (buffer[0] & 0xc0) === 0x80) throw deny();
     let end = Math.min(maximum, read);
     if (start + end < stat.size) while (end > 0 && (buffer[end] & 0xc0) === 0x80) end--;
     if (read && !end) return { ...output({ error: { code: "intake_page_too_small" } }), isError: true };
     const text = decode().decode(buffer.subarray(0, end));
     return output({ path: params.path, text, start_byte: start, total_bytes: stat.size, ...(start + end < stat.size ? { next_byte: start + end } : {}) });
    } finally { await file.close(); }
   } catch { return { ...output({ error: { code: "intake_access_denied" } }), isError: true }; }
  }
 });
}
