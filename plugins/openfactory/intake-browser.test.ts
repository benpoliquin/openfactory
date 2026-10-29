import assert from "node:assert/strict";
import test from "node:test";
import {decodeBase64URL, encodeBase64URL, creationOptions, requestOptions, credentialJSON, pendingCommands, IntakeBrowserError} from "./intake-browser.ts";

const encoded = "AP_-gA";
const binary = () => new Uint8Array([0, 255, 254, 128]).buffer;
const registration = () => ({challenge: encoded, rp: {name: "OpenFactory", id: "openfactory.example"}, user: {id: encoded, name: "operator", displayName: "Operator"}, pubKeyCredParams: [{type: "public-key", alg: -7}]});

test("canonical base64url preserves arbitrary bytes and view offsets", () => {
  assert.deepEqual(new Uint8Array(decodeBase64URL(encoded)), new Uint8Array(binary()));
  assert.equal(encodeBase64URL(binary()), encoded);
  assert.equal(encodeBase64URL(new Uint8Array([99, 0, 255, 254, 128, 99]).subarray(1, 5)), encoded);
  assert.equal(encodeBase64URL(new DataView(new Uint8Array([99, 0, 255, 254, 128]).buffer, 1)), encoded);
  assert.equal(decodeBase64URL("").byteLength, 0);
  for (const malformed of ["A", "AB", "AAF", "AA==", "a+b/", " AA", "AA\n", "é"]) assert.throws(() => decodeBase64URL(malformed), IntakeBrowserError);
});

test("native creation and request options decode all credential byte fields and require UV", () => {
  const source = {...registration(), authenticatorSelection: {residentKey: "required", authenticatorAttachment: "platform"}, excludeCredentials: [{type: "public-key", id: encoded, transports: ["internal", "hybrid"]}], extensions: {credProps: true, prf: {eval: {first: encoded}}}};
  const created = creationOptions(source);
  assert.ok(created.challenge instanceof ArrayBuffer);
  assert.deepEqual(new Uint8Array(created.user.id as ArrayBuffer), new Uint8Array(binary()));
  assert.equal(created.authenticatorSelection?.userVerification, "required");
  assert.equal(created.authenticatorSelection?.residentKey, "required");
  assert.deepEqual(created.excludeCredentials?.[0].transports, ["internal", "hybrid"]);
  assert.deepEqual(created.rp, {name: "OpenFactory", id: "openfactory.example"});
  assert.equal(created.user.name, "operator");
  assert.equal(created.user.displayName, "Operator");
  assert.deepEqual(created.pubKeyCredParams, [{type: "public-key", alg: -7}]);
  assert.equal(source.user.id, encoded, "input is not mutated");
  const requested = requestOptions({challenge: encoded, rpId: "openfactory.example", allowCredentials: source.excludeCredentials, extensions: {largeBlob: {write: encoded}}});
  assert.equal(requested.userVerification, "required");
  assert.ok(requested.allowCredentials?.[0].id instanceof ArrayBuffer);
  assert.equal(requested.rpId, "openfactory.example");
  for (const userVerification of ["preferred", "discouraged", null]) {
    assert.throws(() => creationOptions({...registration(), authenticatorSelection: {userVerification}}), IntakeBrowserError);
    assert.throws(() => requestOptions({challenge: encoded, userVerification}), IntakeBrowserError);
  }
});

test("creation preserves native algorithm choices and ceremony preferences without coercing invalid parameters", () => {
  const created = creationOptions({...registration(), timeout: 60000, attestation: "none", pubKeyCredParams: [{type: "public-key", alg: -7}, {type: "public-key", alg: -257}], authenticatorSelection: {residentKey: "required", requireResidentKey: true, userVerification: "required"}});
  assert.deepEqual(created.pubKeyCredParams, [{type: "public-key", alg: -7}, {type: "public-key", alg: -257}]);
  assert.equal(created.timeout, 60000);
  assert.equal(created.attestation, "none");
  assert.deepEqual(created.authenticatorSelection, {residentKey: "required", requireResidentKey: true, userVerification: "required"});
  for (const pubKeyCredParams of [undefined, [], [{type: "public-key"}], [{type: "public-key", alg: "-7"}], [{type: "public-key", alg: -7.5}], [{type: "password", alg: -7}], [{type: "public-key", alg: Infinity}]]) assert.throws(() => creationOptions({...registration(), pubKeyCredParams}), IntakeBrowserError);
  for (const user of [{id: encoded, name: "", displayName: "Operator"}, {id: encoded, name: "operator"}, {id: "AB", name: "operator", displayName: "Operator"}]) assert.throws(() => creationOptions({...registration(), user}), IntakeBrowserError);
});

test("invalid option and extension contracts fail before native credential invocation", () => {
  for (const extensions of [{credProps: "yes"}, {appid: 42}, {largeBlob: {read: true, write: encoded}}, {prf: {eval: {second: encoded}}}, {prf: {eval: {first: "AB"}}}, {unknown: () => true}]) {
    assert.throws(() => creationOptions({...registration(), extensions}), IntakeBrowserError);
  }
  assert.throws(() => creationOptions({...registration(), pubKeyCredParams: [{type: "public-key", alg: "-7"}]}));
  assert.throws(() => requestOptions({challenge: encoded, allowCredentials: [{type: "password", id: encoded}]}));
  assert.throws(() => requestOptions({challenge: ""}));
});

test("registration and assertion serialize actual response bytes and extension outputs", () => {
  const base = {id: encoded, rawId: binary(), type: "public-key", authenticatorAttachment: "platform", getClientExtensionResults: () => ({credProps: {rk: true}, prf: {results: {first: binary()}}})};
  const registered = credentialJSON({...base, response: {clientDataJSON: binary(), attestationObject: binary(), getTransports: () => ["internal"]}} as unknown as PublicKeyCredential) as {response: object; rawId: string; clientExtensionResults: {prf: {results: {first: string}}}};
  assert.deepEqual(registered.response, {clientDataJSON: encoded, attestationObject: encoded, transports: ["internal"]});
  assert.equal(registered.clientExtensionResults.prf.results.first, encoded);
  assert.equal(registered.rawId, encoded);
  const assertion = credentialJSON({...base, response: {clientDataJSON: binary(), authenticatorData: binary(), signature: binary(), userHandle: null}} as unknown as PublicKeyCredential) as {response: object};
  assert.deepEqual(assertion.response, {clientDataJSON: encoded, authenticatorData: encoded, signature: encoded, userHandle: null});
  const identified = credentialJSON({...base, response: {clientDataJSON: binary(), authenticatorData: binary(), signature: binary(), userHandle: binary()}} as unknown as PublicKeyCredential) as {response: {userHandle: string}};
  assert.equal(identified.response.userHandle, encoded);
  assert.throws(() => credentialJSON(null), (error: unknown) => error instanceof IntakeBrowserError && error.code === "credential_unavailable");
  assert.throws(() => credentialJSON({type: "password"} as Credential), IntakeBrowserError);
  assert.throws(() => credentialJSON({...base, id: "wrong", response: {clientDataJSON: binary(), attestationObject: binary()}} as unknown as PublicKeyCredential), IntakeBrowserError);
});

const id = "baf9b067-9ca4-4cbd-a5b4-7fc6584a5e6b";
test("durable reconnect storage contains only UUIDs, rejects payloads, and removes corruption", () => {
  const data = new Map<string, string>();
  const storage = {getItem: (key: string) => data.get(key) ?? null, setItem: (key: string, value: string) => { data.set(key, value); }, removeItem: (key: string) => { data.delete(key); }};
  const pending = pendingCommands(storage);
  pending.add(id); pending.add(id);
  assert.deepEqual(pendingCommands(storage).list(), [id]);
  assert.deepEqual([...data], [["openfactory-intake.pending-command-ids.v1", JSON.stringify([id])]]);
  for (const secret of ["transcript text", JSON.stringify({auth: "secret"}), "../command", "00000000-0000-0000-0000-000000000000"]) assert.throws(() => pending.add(secret), IntakeBrowserError);
  for (const corrupt of ["not json", JSON.stringify({token: "secret"}), JSON.stringify([id, "secret"]), JSON.stringify([id, id])]) {
    data.set("openfactory-intake.pending-command-ids.v1", corrupt);
    assert.deepEqual(pending.list(), []);
    assert.equal(data.size, 0);
  }
  pending.add(id); pending.remove(id); assert.deepEqual(pending.list(), []);
  for (let i = 0; i < 64; i++) pending.add(`00000000-0000-4000-8000-${i.toString(16).padStart(12, "0")}`);
  assert.throws(() => pending.add(id), IntakeBrowserError);
  assert.equal(pending.list().length, 64, "never evict unresolved commands");
  pending.clear(); assert.equal(data.size, 0);
});

test("unavailable storage explicitly prevents a command from being treated as durable", () => {
  const broken = pendingCommands({getItem: () => null, setItem: () => { throw new Error("quota"); }, removeItem: () => { throw new Error("disabled"); }});
  assert.throws(() => broken.add(id), (error: unknown) => error instanceof IntakeBrowserError && error.code === "storage_unavailable");
  assert.throws(() => broken.clear(), IntakeBrowserError);
  assert.throws(() => pendingCommands({getItem: () => { throw new Error("disabled"); }, setItem() {}, removeItem() {}}).list(), IntakeBrowserError);
});

test("pending command namespaces isolate operators, installations and restored generations",()=>{
  const data=new Map<string,string>();
  const storage={getItem:(key:string)=>data.get(key)??null,setItem:(key:string,value:string)=>{data.set(key,value);},removeItem:(key:string)=>{data.delete(key);}};
  const legacy=pendingCommands(storage),original=pendingCommands(storage,"scope-generation-1"),restored=pendingCommands(storage,"scope-generation-2");
  legacy.add(id);original.add(id);
  assert.deepEqual(restored.list(),[]);assert.deepEqual(original.list(),[id]);
  restored.add("baf9b067-9ca4-4cbd-a5b4-7fc6584a5e6c");original.clear();
  assert.deepEqual(legacy.list(),[id]);assert.equal(restored.list().length,1);
  assert.equal(data.has("openfactory-intake.pending-command-ids.v1.scope-generation-2"),true);
  data.set("openfactory-intake.pending-command-ids.v1.scope-generation-1","corrupt");
  assert.deepEqual(original.list(),[]);assert.equal(restored.list().length,1);
});
