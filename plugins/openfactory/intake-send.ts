import {IntakeBrowserError} from "./intake-browser.ts";

export interface SendLifecycle {
  save_command_id: string;
  draft_command_id: string;
  request_id?: string;
  content_version?: number;
  phase: "saving" | "saved" | "drafting";
}
export interface SendJournal {
  get(): SendLifecycle | null;
  begin(saveID: string, draftID: string): SendLifecycle;
  saved(requestID: string, version: number): SendLifecycle;
  drafting(): SendLifecycle;
  clear(): void;
}
const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i;
function invalid(message: string): never {throw new IntakeBrowserError("invalid_contract",message);}
function validate(value: unknown): SendLifecycle {
  if (!value || typeof value !== "object" || Array.isArray(value)) invalid("Invalid send lifecycle");
  const input=value as Record<string,unknown>;
  if (Object.keys(input).some(name=>!["save_command_id","draft_command_id","request_id","content_version","phase"].includes(name))) invalid("Unexpected send lifecycle fields");
  if (typeof input.save_command_id!=="string" || !uuid.test(input.save_command_id) || typeof input.draft_command_id!=="string" || !uuid.test(input.draft_command_id) || input.save_command_id===input.draft_command_id) invalid("Send requires distinct UUID command IDs");
  if (!["saving","saved","drafting"].includes(String(input.phase))) invalid("Invalid send phase");
  const hasRequest=Object.hasOwn(input,"request_id"), hasVersion=Object.hasOwn(input,"content_version");
  if (hasRequest!==hasVersion || (input.phase!=="saving" && !hasRequest)) invalid("Send requires the saved content fence");
  if (input.phase==="saving" && hasRequest) invalid("Saving cannot claim a saved content fence");
  if (hasRequest && (typeof input.request_id!=="string" || !/^[A-Za-z0-9_.-]{1,200}$/.test(input.request_id) || typeof input.content_version!=="number" || !Number.isSafeInteger(input.content_version) || input.content_version<0)) invalid("Invalid saved content fence");
  return {save_command_id:input.save_command_id,draft_command_id:input.draft_command_id,phase:input.phase as SendLifecycle["phase"],...(hasRequest?{request_id:input.request_id as string,content_version:input.content_version as number}:{})};
}
export function sendJournal(storage: Pick<Storage,"getItem"|"setItem"|"removeItem">, namespace?: string): SendJournal {
  const key = "openfactory-intake.send-lifecycle.v1" + (namespace === undefined ? "" : `.${namespace}`);
  const unavailable=()=>new IntakeBrowserError("storage_unavailable","Send recovery storage is unavailable; no command was dispatched");
  const clear=()=>{try {storage.removeItem(key);} catch {throw unavailable();}};
  const get=():SendLifecycle|null=>{
    let raw:string|null;
    try {raw=storage.getItem(key);} catch {throw unavailable();}
    if (raw===null) return null;
    try {return validate(JSON.parse(raw));} catch {clear();return null;}
  };
  const persist=(state:SendLifecycle):SendLifecycle=>{
    const safe=validate(state);
    try {storage.setItem(key,JSON.stringify(safe));} catch {throw unavailable();}
    return safe;
  };
  return {
    get,clear,
    begin(saveID,draftID) {
      const candidate=validate({save_command_id:saveID,draft_command_id:draftID,phase:"saving"});
      const current=get();
      if (current) {
        if (current.save_command_id!==saveID || current.draft_command_id!==draftID) invalid("Reconcile the existing send before starting another");
        return current;
      }
      return persist(candidate);
    },
    saved(requestID,version) {
      const current=get();if (!current) invalid("No send is awaiting a save receipt");
      if (current.phase!=="saving") {
        if (current.request_id!==requestID || current.content_version!==version) invalid("Save receipt does not match the recorded content fence");
        return current;
      }
      return persist({...current,request_id:requestID,content_version:version,phase:"saved"});
    },
    drafting() {
      const current=get();if (!current || current.phase==="saving") invalid("A durable save receipt is required before drafting");
      if (current.phase==="drafting") return current;
      return persist({...current,phase:"drafting"});
    },
  };
}
