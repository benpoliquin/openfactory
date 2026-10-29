// Run only in the packaged pinned OMP 18.10.3 environment.
import { afterEach, expect, test } from "bun:test";
import type { ExtensionAPI, ExtensionContext } from "@oh-my-pi/pi-coding-agent";
import { createRequire } from "node:module";
import { createHash } from "node:crypto";
import * as fs from "node:fs/promises";
import * as os from "node:os";
import * as path from "node:path";
import register from "../../deploy/intake-extension";
const ompPackage = process.env.OPENFACTORY_OMP_PACKAGE ?? "/usr/local/lib/node_modules/@oh-my-pi/pi-coding-agent";
const requireOMP = createRequire(path.join(ompPackage, "package.json"));
const zod: ExtensionAPI["zod"] = requireOMP("@oh-my-pi/omptype/zod");
type Result = { content: Array<{ text?: string }>; isError?: boolean };
type Tool = { name: string; parameters: { parse(value: unknown): unknown } };
// The installed module location is runtime-selected; static resolution cannot work.
const { RegisteredToolAdapter } = await import(path.join(ompPackage, "src/extensibility/extensions/wrapper.ts")) as {
 RegisteredToolAdapter: new (tool: { definition: Tool; extensionPath: string }, runner: { createContext(): ExtensionContext }) => { execute(id: string, args: unknown, signal?: AbortSignal, update?: undefined): Promise<Result> };
};
const cleanup: string[] = [];
afterEach(async () => { for (const root of cleanup.splice(0)) await fs.rm(root, { recursive: true, force: true }); });
async function fixture(bytes = Buffer.from("Aé€😀Z")) {
 const temporary = await fs.realpath(await fs.mkdtemp(path.join(os.tmpdir(), "openfactory-intake-extension-"))); cleanup.push(temporary);
 const identity = { project: "project", request_id: "request", step_id: "step", attempt_id: "attempt" };
 const cwd = path.join(temporary, "intake", ...Object.values(identity).map(s => createHash("sha256").update(s).digest("hex").slice(0, 32)));
 await fs.mkdir(path.join(cwd, ".openfactory"), { recursive: true }); await fs.mkdir(path.join(cwd, "inputs"));
 const name = "inputs/context.txt";
 await fs.writeFile(path.join(cwd, name), bytes, { mode: 0o444 });
 const control = { ...identity, deadline: new Date(Date.now() + 60000).toISOString(), input_files: { [name]: createHash("sha256").update(bytes).digest("hex") } };
 const controlFile = path.join(cwd, ".openfactory/intake-session.json");
 await fs.writeFile(controlFile, JSON.stringify(control), { mode: 0o600 });
 const tools = new Map<string, Tool>(); const hooks = new Map<string, (e: { toolName?: string }, c: ExtensionContext) => Promise<unknown>>(); let active: string[] = [];
 // Test host implements only the extension registration surface; dispatch uses OMP itself.
 register({ zod, registerTool(t: Tool) { tools.set(t.name, t); }, on(e: string, h: (event: { toolName?: string }, ctx: ExtensionContext) => Promise<unknown>) { hooks.set(e, h); }, async setActiveTools(names: string[]) { active = names; } } as unknown as ExtensionAPI);
 const ctx = { cwd } as ExtensionContext; const tool = tools.get("intake_read")!;
 const adapter = new RegisteredToolAdapter({ definition: tool, extensionPath: path.resolve("deploy/intake-extension.ts") }, { createContext: () => ctx });
 const invoke = (args: unknown) => adapter.execute("call", tool.parameters.parse(args), undefined, undefined);
 return { cwd, temporary, name, bytes, control, controlFile, ctx, tools, hooks, active: () => active, tool, invoke };
}
function payload(result: Result) { return JSON.parse(result.content[0].text!); }
test("intake activates one tool and rejects all native/research capabilities", async () => {
 const f = await fixture(); await f.hooks.get("session_start")!({}, f.ctx);
 expect([...f.tools.keys()]).toEqual(["intake_read"]); expect(f.active()).toEqual(["intake_read"]);
 for (const toolName of ["bash", "read", "write", "eval", "task", "research_search", "discovery_read", "web_search", "mcp_secret"]) expect(await f.hooks.get("tool_call")!({ toolName }, f.ctx)).toEqual({ block: true, reason: "intake_tool_not_permitted" });
 for (const fields of [{ start_line: 1 }, { limit: 1 }, { max_bytes: 65537 }, { start_byte: -1 }, { start_byte: 0.5 }, { unknown: true }]) expect(() => f.tool.parameters.parse({ path: f.name, ...fields })).toThrow();
});
test("actual adapter reconstructs large UTF8 input with exact bounded byte offsets", async () => {
 const text = "\ufeffAé€😀\n".repeat(100000); const f = await fixture(Buffer.from(text)); let start = 0; const pages: string[] = [];
 while (start < f.bytes.length) {
  const result = await f.invoke({ path: f.name, start_byte: start, max_bytes: 65536 }); expect(result.isError).not.toBe(true);
  const page = payload(result); expect(page.start_byte).toBe(start); expect(page.total_bytes).toBe(f.bytes.length);
  const size = Buffer.byteLength(page.text); expect(size).toBeGreaterThan(0); expect(size).toBeLessThanOrEqual(65536); pages.push(page.text); start += size;
  if (start < f.bytes.length) expect(page.next_byte).toBe(start); else expect(page).not.toHaveProperty("next_byte");
 }
 expect(pages.join("")).toBe(text);
 expect(payload(await f.invoke({ path: f.name, start_byte: start }))).toEqual({ path: f.name, text: "", start_byte: start, total_bytes: start });
});
test("UTF8 split starts and insufficient pages fail without replacement", async () => {
 const f = await fixture();
 expect(payload(await f.invoke({ path: f.name, max_bytes: 2 }))).toEqual({ path: f.name, text: "A", start_byte: 0, next_byte: 1, total_bytes: 11 });
 for (const start_byte of [2, 4, 5, 7, 8, 9, 12]) expect((await f.invoke({ path: f.name, start_byte })).isError).toBe(true);
 expect(payload(await f.invoke({ path: f.name, start_byte: 6, max_bytes: 3 })).error.code).toBe("intake_page_too_small");
});
test("control full identity, expiry, permissions and schema fail closed", async () => {
 const f = await fixture();
 for (const field of ["project", "request_id", "step_id", "attempt_id"]) {
  await fs.writeFile(f.controlFile, JSON.stringify({ ...f.control, [field]: "wrong" })); expect((await f.invoke({ path: f.name })).isError).toBe(true);
 }
 for (const control of [{ ...f.control, deadline: "2000-01-01T00:00:00Z" }, { ...f.control, credential: "secret" }, { ...f.control, input_files: {} }]) {
  await fs.writeFile(f.controlFile, JSON.stringify(control)); expect((await f.invoke({ path: f.name })).isError).toBe(true);
 }
 await fs.writeFile(f.controlFile, JSON.stringify(f.control)); await fs.chmod(f.controlFile, 0o644); expect((await f.invoke({ path: f.name })).isError).toBe(true);
});
test("declared hashes do not authorize links, traversal, hidden files or invalid text", async () => {
 const f = await fixture();
 for (const name of [".", ".openfactory/intake-session.json", "inputs/../context.txt", "inputs/.hidden", "inputs//context.txt", "/etc/passwd", "inputs/\\bad", "inputs/\x00bad", "inputs/\ud800bad", "inputs/undeclared"]) expect((await f.invoke({ path: name })).isError).toBe(true);
 const target = path.join(f.cwd, f.name);
 await fs.link(target, path.join(f.temporary, "hardlink")); expect((await f.invoke({ path: f.name })).isError).toBe(true); await fs.unlink(path.join(f.temporary, "hardlink"));
 await fs.unlink(target); await fs.symlink(f.controlFile, target); expect((await f.invoke({ path: f.name })).isError).toBe(true); await fs.unlink(target);
 for (const bytes of [Buffer.from("wrong hash"), Buffer.from([0xff]), Buffer.from("NUL\x00")]) {
  await fs.writeFile(target, bytes, { mode: 0o444 });
  await fs.writeFile(f.controlFile, JSON.stringify({ ...f.control, input_files: { [f.name]: createHash("sha256").update(bytes).digest("hex") } }));
  if (bytes.equals(Buffer.from("wrong hash"))) await fs.writeFile(f.controlFile, JSON.stringify(f.control));
  expect((await f.invoke({ path: f.name })).isError).toBe(true); await fs.unlink(target);
 }
 const handle = await fs.open(target, "w", 0o444); await handle.truncate((64 << 20) + 1); await handle.close(); expect((await f.invoke({ path: f.name })).isError).toBe(true);
});
test("cached verification cannot conceal subsequent file mutation", async () => {
 const f = await fixture(); expect((await f.invoke({ path: f.name })).isError).not.toBe(true);
 await fs.chmod(path.join(f.cwd, f.name), 0o644); await fs.writeFile(path.join(f.cwd, f.name), "changed"); await fs.chmod(path.join(f.cwd, f.name), 0o444);
 expect((await f.invoke({ path: f.name })).isError).toBe(true);
});
test("wrapper executes fixed intake policy and rejects appended launch flags", async () => {
 const f = await fixture();
 const bin = path.join(f.temporary, "bin"); await fs.mkdir(bin);
 // This executable only captures argv/environment; it never launches OMP.
 await fs.writeFile(path.join(bin, "bun"), "#!/bin/sh\nprintf 'ARG:%s\\n' \"$@\"\nprintf 'PI_CONFIG_FILES:%s\\n' \"${PI_CONFIG_FILES-unset}\"\nprintf 'HTTPS_PROXY:%s\\n' \"${HTTPS_PROXY-unset}\"\n", { mode: 0o755 });
 const script = path.resolve("deploy/omp.sh");
 const env = { ...process.env, HOME: path.join(f.temporary, "home"), PATH: `${bin}:${process.env.PATH}`, PI_CONFIG_FILES: "/ambient/config", HTTPS_PROXY: "http://ambient-proxy", OPENFACTORY_INTAKE_CONFIG: "/attacker/config", OPENFACTORY_INTAKE_EXTENSION: "/attacker/extension", OPENFACTORY_AGENT_MAX_TIME: "7m" };
 const launch = async (cwd: string, args: string[]) => {
  const child = Bun.spawn(["/bin/sh", script, "acp", ...args], { cwd, env, stdout: "pipe", stderr: "pipe" });
  const [stdout, stderr, code] = await Promise.all([new Response(child.stdout).text(), new Response(child.stderr).text(), child.exited]);
  return { stdout, stderr, code };
 };
 const result = await launch(f.cwd, []);
 expect(result.code).toBe(0); expect(result.stderr).toBe("");
 expect(result.stdout.split("\n").filter(line => line.startsWith("ARG:")).map(line => line.slice(4))).toEqual([
  "/usr/local/lib/node_modules/@oh-my-pi/pi-coding-agent/dist/cli.js", "acp",
  "--approval-mode", "yolo", "--no-title", "--max-time", "7m",
  "--no-extensions", "--no-rules", "--no-skills",
  "--config", "/opt/openfactory/intake/intake-config.yaml",
  "--extension", "/usr/local/lib/node_modules/@oh-my-pi/pi-coding-agent/openfactory/intake-extension.ts",
  "--tools", "intake_read",
 ]);
 expect(result.stdout).toContain("PI_CONFIG_FILES:unset"); expect(result.stdout).toContain("HTTPS_PROXY:unset"); expect(result.stdout).not.toContain("/attacker/");
 for (const args of [["--tools", "bash"], ["--extension", "/ambient"], ["--config", "/ambient"], ["--", "--tools", "write"]]) {
  const denied = await launch(f.cwd, args); expect(denied.code).toBe(1); expect(denied.stdout).toBe(""); expect(denied.stderr).toContain("intake_launch_rejected");
 }
 const ordinary = await launch(f.temporary, ["--model", "chosen"]);
 expect(ordinary.code).toBe(0); expect(ordinary.stdout).toContain("ARG:read,bash,edit,write,grep,find,glob,eval,todo,task"); expect(ordinary.stdout).toContain("ARG:chosen");
 const discovery = path.join(f.temporary, "discovery", "p", "r", "s", "a"); await fs.mkdir(path.join(discovery, ".openfactory"), { recursive: true });
 await fs.writeFile(path.join(discovery, ".openfactory/discovery-session.json"), "{}");
 const research = await launch(discovery, []); expect(research.code).toBe(0); expect(research.stdout).toContain("ARG:research_search,research_capture,research_github,research_source,discovery_read,discovery_write");
 const config = await fs.readFile(path.resolve("deploy/intake-config.yaml"), "utf8"); expect(config).toContain("enabled: false"); expect(config).toContain("default: []"); expect(config).toContain("web: []");
});
