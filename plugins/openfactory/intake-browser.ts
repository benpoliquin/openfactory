export class IntakeBrowserError extends Error {
  readonly code: "invalid_contract" | "credential_unavailable" | "storage_unavailable";
  constructor(code: "invalid_contract" | "credential_unavailable" | "storage_unavailable", message: string) {
    super(message); this.name = "IntakeBrowserError"; this.code = code;
  }
}
function invalid(message: string): never { throw new IntakeBrowserError("invalid_contract", message); }
export function encodeBase64URL(value: ArrayBuffer | ArrayBufferView): string {
  const bytes = ArrayBuffer.isView(value) ? new Uint8Array(value.buffer, value.byteOffset, value.byteLength) : new Uint8Array(value);
  let binary = "";
  for (const byte of bytes) binary += String.fromCharCode(byte);
  return btoa(binary).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}
export function decodeBase64URL(value: string): ArrayBuffer {
  if (typeof value !== "string" || !/^[A-Za-z0-9_-]*$/.test(value) || value.length % 4 === 1) invalid("Expected canonical base64url bytes");
  const binary = atob(value.replace(/-/g, "+").replace(/_/g, "/") + "=".repeat((4 - value.length % 4) % 4));
  const bytes = Uint8Array.from(binary, c => c.charCodeAt(0));
  if (encodeBase64URL(bytes) !== value) invalid("Expected canonical base64url bytes");
  return bytes.buffer;
}
function record(value: unknown): Record<string, unknown> {
  if (!value || typeof value !== "object" || Array.isArray(value) || (Object.getPrototypeOf(value) !== Object.prototype && Object.getPrototypeOf(value) !== null)) invalid("Expected an options object");
  return value as Record<string, unknown>;
}
function text(value: unknown): string { if (typeof value !== "string" || !value.length) invalid("Expected a nonempty string"); return value; }
function bytes(value: unknown): ArrayBuffer { const result = decodeBase64URL(text(value)); if (!result.byteLength) invalid("Expected nonempty bytes"); return result; }
function json(value: unknown): unknown {
  if (value === null || typeof value === "string" || typeof value === "boolean" || (typeof value === "number" && Number.isFinite(value))) return value;
  if (Array.isArray(value)) return value.map(json);
  const source = record(value); const result: Record<string, unknown> = {};
  for (const [key, item] of Object.entries(source)) {
    if (["__proto__", "constructor", "prototype"].includes(key)) invalid("Invalid extension field");
    Object.defineProperty(result, key, {value: json(item), enumerable: true, writable: true, configurable: true});
  }
  return result;
}
function extensions(value: unknown): AuthenticationExtensionsClientInputs | undefined {
  if (value === undefined) return undefined;
  const result = json(record(value)) as Record<string, unknown>;
  for (const key of ["appid", "appidExclude"]) if (key in result) text(result[key]);
  for (const key of ["credProps", "hmacCreateSecret"]) if (key in result && typeof result[key] !== "boolean") invalid("Invalid extension boolean");
  if ("credBlob" in result) result.credBlob = bytes(result.credBlob);
  if ("largeBlob" in result) {
    const blob = record(result.largeBlob);
    if ("read" in blob && typeof blob.read !== "boolean") invalid("Invalid largeBlob read");
    if ("support" in blob && !["required", "preferred"].includes(String(blob.support))) invalid("Invalid largeBlob support");
    if ("write" in blob) blob.write = bytes(blob.write);
    if (blob.read === true && "write" in blob) invalid("Conflicting largeBlob operation");
  }
  if ("prf" in result) {
    const prf = record(result.prf);
    const convert = (value: unknown) => { const input = record(value); if (!("first" in input) || Object.keys(input).some(k => !["first", "second"].includes(k))) invalid("Invalid PRF values"); return {first: bytes(input.first), ...("second" in input ? {second: bytes(input.second)} : {})}; };
    if ("eval" in prf) prf.eval = convert(prf.eval);
    if ("evalByCredential" in prf) { const entries = record(prf.evalByCredential); for (const [id, item] of Object.entries(entries)) { bytes(id); entries[id] = convert(item); } }
    if (Object.keys(prf).some(k => !["eval", "evalByCredential"].includes(k))) invalid("Invalid PRF extension");
  }
  return result as AuthenticationExtensionsClientInputs;
}
const transports = ["usb", "nfc", "ble", "internal", "hybrid", "smart-card"];
function descriptors(value: unknown): PublicKeyCredentialDescriptor[] | undefined {
  if (value === undefined) return undefined;
  if (!Array.isArray(value)) invalid("Expected credential descriptors");
  return value.map(item => {
    const input = record(item);
    if (input.type !== "public-key") invalid("Unsupported credential type");
    if (input.transports !== undefined && (!Array.isArray(input.transports) || input.transports.some(t => !transports.includes(t)))) invalid("Invalid credential transports");
    return {...input, type: "public-key", id: bytes(input.id)} as PublicKeyCredentialDescriptor;
  });
}
function common(input: Record<string, unknown>) {
  if (input.timeout !== undefined && (typeof input.timeout !== "number" || !Number.isFinite(input.timeout) || input.timeout <= 0)) invalid("Invalid ceremony timeout");
  return {...input, challenge: bytes(input.challenge), extensions: extensions(input.extensions)};
}
export function creationOptions(value: unknown): PublicKeyCredentialCreationOptions {
  const input = record(value), rp = record(input.rp), user = record(input.user);
  const rpName = text(rp.name), userName = text(user.name), displayName = text(user.displayName);
  const rpID = rp.id === undefined ? undefined : text(rp.id);
  if (!Array.isArray(input.pubKeyCredParams) || !input.pubKeyCredParams.length) invalid("Missing credential algorithms");
  const pubKeyCredParams = input.pubKeyCredParams.map((item): PublicKeyCredentialParameters => {
    const parameter = record(item), algorithm = parameter.alg;
    if (parameter.type !== "public-key" || typeof algorithm !== "number" || !Number.isSafeInteger(algorithm)) invalid("Invalid credential algorithm");
    return {...parameter, type: "public-key", alg: algorithm};
  });
  const selection = input.authenticatorSelection === undefined ? {} : record(input.authenticatorSelection);
  if (selection.userVerification !== undefined && selection.userVerification !== "required") invalid("User verification must be required");
  const authenticatorSelection: AuthenticatorSelectionCriteria = {userVerification: "required"};
  if (selection.authenticatorAttachment !== undefined) {
    const attachment = selection.authenticatorAttachment;
    if (attachment !== "platform" && attachment !== "cross-platform") invalid("Invalid authenticator attachment");
    authenticatorSelection.authenticatorAttachment = attachment;
  }
  if (selection.residentKey !== undefined) {
    const residentKey = selection.residentKey;
    if (residentKey !== "required" && residentKey !== "preferred" && residentKey !== "discouraged") invalid("Invalid resident key requirement");
    authenticatorSelection.residentKey = residentKey;
  }
  if (selection.requireResidentKey !== undefined) {
    if (typeof selection.requireResidentKey !== "boolean") invalid("Invalid resident key requirement");
    authenticatorSelection.requireResidentKey = selection.requireResidentKey;
  }
  let attestation: AttestationConveyancePreference | undefined;
  if (input.attestation !== undefined) {
    const preference = input.attestation;
    if (preference !== "none" && preference !== "indirect" && preference !== "direct" && preference !== "enterprise") invalid("Invalid attestation preference");
    attestation = preference;
  }
  return {
    ...common(input),
    rp: {...rp, name: rpName, ...(rpID === undefined ? {} : {id: rpID})},
    user: {...user, id: bytes(user.id), name: userName, displayName},
    pubKeyCredParams,
    excludeCredentials: descriptors(input.excludeCredentials),
    authenticatorSelection,
    ...(attestation === undefined ? {} : {attestation}),
  };
}
export function requestOptions(value: unknown): PublicKeyCredentialRequestOptions {
  const input = record(value);
  if (input.userVerification !== undefined && input.userVerification !== "required") invalid("User verification must be required");
  if (input.rpId !== undefined) text(input.rpId);
  return {...common(input), allowCredentials: descriptors(input.allowCredentials), userVerification: "required"} as PublicKeyCredentialRequestOptions;
}
function buffer(value: unknown): ArrayBuffer { if (!(value instanceof ArrayBuffer)) invalid("Invalid credential response bytes"); return value; }
function extensionOutput(value: unknown): unknown {
  if (value instanceof ArrayBuffer || ArrayBuffer.isView(value)) return encodeBase64URL(value);
  if (Array.isArray(value)) return value.map(extensionOutput);
  if (value && typeof value === "object") return Object.fromEntries(Object.entries(record(value)).map(([k,v]) => [k, extensionOutput(v)]));
  return json(value);
}
export function credentialJSON(credential: Credential | null): object {
  if (!credential || credential.type !== "public-key") throw new IntakeBrowserError("credential_unavailable", "No supported public-key credential was returned");
  const key = credential as PublicKeyCredential;
  if (typeof key.getClientExtensionResults !== "function") invalid("Invalid public-key credential");
  const response = key.response;
  const common = {clientDataJSON: encodeBase64URL(buffer(response.clientDataJSON))};
  let serialized: object;
  if ("attestationObject" in response) {
    const registration = response as AuthenticatorAttestationResponse;
    serialized = {...common, attestationObject: encodeBase64URL(buffer(registration.attestationObject)), transports: typeof registration.getTransports === "function" ? registration.getTransports() : []};
  } else if ("authenticatorData" in response && "signature" in response) {
    const assertion = response as AuthenticatorAssertionResponse;
    serialized = {...common, authenticatorData: encodeBase64URL(buffer(assertion.authenticatorData)), signature: encodeBase64URL(buffer(assertion.signature)), userHandle: assertion.userHandle === null ? null : encodeBase64URL(buffer(assertion.userHandle))};
  } else invalid("Unsupported authenticator response");
  const rawId = encodeBase64URL(buffer(key.rawId));
  if (text(key.id) !== rawId) invalid("Credential ID does not match raw ID");
  return {id: key.id, rawId, type: "public-key", authenticatorAttachment: key.authenticatorAttachment ?? null, response: serialized, clientExtensionResults: extensionOutput(key.getClientExtensionResults())};
}
const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i;
export interface PendingCommands {
  list(): string[];
  add(id: string): void;
  remove(id: string): void;
  clear(): void;
}
export function pendingCommands(storage: Pick<Storage, "getItem" | "setItem" | "removeItem">, namespace?: string): PendingCommands {
  const pendingKey = "openfactory-intake.pending-command-ids.v1" + (namespace === undefined ? "" : `.${namespace}`);
  const failure = () => new IntakeBrowserError("storage_unavailable", "Pending command storage is unavailable; command was not dispatched");
  const clear = () => { try { storage.removeItem(pendingKey); } catch { throw failure(); } };
  const list = (): string[] => {
    let raw: string | null;
    try { raw = storage.getItem(pendingKey); } catch { throw failure(); }
    if (raw === null) return [];
    try {
      const ids: unknown = JSON.parse(raw);
      if (!Array.isArray(ids) || ids.length > 64 || ids.some(id => typeof id !== "string" || !uuid.test(id)) || new Set(ids).size !== ids.length) { clear(); return []; }
      return ids;
    } catch (error) { if (error instanceof IntakeBrowserError) throw error; clear(); return []; }
  };
  const save = (ids: string[]) => { try { storage.setItem(pendingKey, JSON.stringify(ids)); } catch { throw failure(); } };
  return {list, clear, add(id: string) { if (typeof id !== "string" || !uuid.test(id)) invalid("Expected a UUID command ID"); const ids = list(); if (ids.includes(id)) return; if (ids.length === 64) invalid("Pending command limit reached; reconcile existing commands first"); save([...ids, id]); }, remove(id: string) { if (typeof id !== "string" || !uuid.test(id)) invalid("Expected a UUID command ID"); const ids = list().filter(item => item !== id); if (ids.length) save(ids); else clear(); }};
}
