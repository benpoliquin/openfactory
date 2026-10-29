import { z } from "zod";
import { IntakeBrowserError, pendingCommands, type PendingCommands } from "./intake-browser.ts";
import { sendJournal, type SendJournal } from "./intake-send.ts";
import { commandSchema, disclosureSchema, stableJSON, type Command } from "./intake-contract.ts";

export interface JournalScope { origin:string; operator_id:string; installation_id:string; auth_generation:string }
export interface JournalRecords { scope:JournalScope; namespace:string; pending:PendingCommands; send:SendJournal }
export interface BrowserJournal {
 bind(scope:JournalScope):JournalRecords;
 previous(scope:JournalScope):JournalRecords[];
 rememberDraft(scope:JournalScope,command:Command):void;
 drafts(scope:JournalScope):Command[];
 forgetDraft(scope:JournalScope,id:string):void;
}
const registryKey = "openfactory-intake.journal-scopes.v1";
const identifier = z.string().min(1).max(256).regex(/^[A-Za-z0-9._:@+-]+$/);
const scopeSchema = z.object({origin:z.string().min(1).max(2048),operator_id:identifier,installation_id:identifier,auth_generation:identifier}).strict();
const entrySchema = z.object({scope:scopeSchema,namespace:z.string().min(1).max(8192)}).strict();
function validatedScope(value:unknown):JournalScope {
 const parsed=scopeSchema.safeParse(value);
 if(!parsed.success) throw new IntakeBrowserError("invalid_contract","Invalid journal identity scope");
 let url:URL;
 try {url=new URL(parsed.data.origin);} catch {throw new IntakeBrowserError("invalid_contract","Invalid journal origin");}
 if(url.origin!==parsed.data.origin || url.username || url.password || (url.protocol!=="https:" && !(url.protocol==="http:" && ["localhost","127.0.0.1","[::1]"].includes(url.hostname)))) throw new IntakeBrowserError("invalid_contract","Journal origin must be a normalized private HTTPS origin or local development origin");
 return {...parsed.data};
}
function namespaceFor(scope:JournalScope):string {
 return encodeURIComponent(JSON.stringify([scope.origin,scope.operator_id,scope.installation_id,scope.auth_generation]));
}
function safeDraft(value:unknown):Command {
 const parsed=commandSchema.safeParse(value);
 if(!parsed.success || parsed.data.action!=="intake.draft") throw new IntakeBrowserError("invalid_contract","Only exact intake.draft metadata can be journaled");
 const bounded=z.object({command_id:z.uuid(),action:z.literal("intake.draft"),request_id:identifier,expected_content_version:z.number().int().nonnegative().max(Number.MAX_SAFE_INTEGER),payload:z.object({disclosure:disclosureSchema}).strict()}).strict().safeParse(parsed.data);
 if(!bounded.success) throw new IntakeBrowserError("invalid_contract","Invalid exact draft command envelope");
 const draft=bounded.data;
 const disclosure=draft.payload.disclosure;
 if(!identifier.safeParse(draft.request_id).success || !Number.isSafeInteger(draft.expected_content_version) || !/^[a-f0-9]{64}$/.test(disclosure.context_sha) || !/^[a-f0-9]{64}$/.test(disclosure.policy_sha) || disclosure.model_provider_ids.length>64 || disclosure.model_provider_ids.some(id=>id.length>512 || !id.trim() || /[\u0000-\u001f\u007f]/.test(id)) || (disclosure.bundle_manifest_shas?.length ?? 0)>64 || disclosure.bundle_manifest_shas?.some(sha=>!/^[a-f0-9]{64}$/.test(sha))) throw new IntakeBrowserError("invalid_contract","Invalid bounded draft disclosure metadata");
 return JSON.parse(JSON.stringify(draft)) as Command;
}
export function browserJournal(storage:Pick<Storage,"getItem"|"setItem"|"removeItem">):BrowserJournal {
 function get(key:string):unknown {
  try {const value=storage.getItem(key);return value===null ? null : JSON.parse(value) as unknown;}
  catch {throw new IntakeBrowserError("storage_unavailable","Journal storage is unavailable or corrupt; no command was dispatched");}
 }
 function put(key:string,value:unknown) {
  try {storage.setItem(key,JSON.stringify(value));}
  catch {throw new IntakeBrowserError("storage_unavailable","Journal storage could not be saved; no command was dispatched");}
 }
 function registry():{scope:JournalScope;namespace:string}[] {
  const stored=get(registryKey);
  if(stored===null) return [];
  const parsed=z.array(entrySchema).max(64).safeParse(stored);
  if(!parsed.success) throw new IntakeBrowserError("storage_unavailable","Journal scope registry is corrupt; recovery is blocked");
  const seen=new Set<string>();
  for(const item of parsed.data) {
   const scope=validatedScope(item.scope);
   if(item.namespace!==namespaceFor(scope) || seen.has(item.namespace)) throw new IntakeBrowserError("storage_unavailable","Journal scope registry has conflicting identities; recovery is blocked");
   seen.add(item.namespace);
  }
  return parsed.data;
 }
 function records(scope:JournalScope,namespace:string):JournalRecords {
  return {scope:{...scope},namespace,pending:pendingCommands(storage,namespace),send:sendJournal(storage,namespace)};
 }
 function bind(value:JournalScope):JournalRecords {
  const scope=validatedScope(value), namespace=namespaceFor(scope), entries=registry();
  if(!entries.some(entry=>entry.namespace===namespace)) {
   if(entries.length>=64) throw new IntakeBrowserError("storage_unavailable","Journal scope limit reached; existing unresolved scopes were retained");
   put(registryKey,[...entries,{scope,namespace}]);
  }
  return records(scope,namespace);
 }
 function drafts(scope:JournalScope):Command[] {
  const bound=bind(scope), stored=get(`openfactory-intake.draft-metadata.v1.${bound.namespace}`);
  if(stored===null) return [];
  if(!Array.isArray(stored) || stored.length>64) throw new IntakeBrowserError("storage_unavailable","Draft metadata journal is corrupt");
  const values=stored.map(safeDraft), ids=new Set(values.map(value=>value.command_id));
  if(ids.size!==values.length) throw new IntakeBrowserError("storage_unavailable","Draft metadata journal contains duplicate command IDs");
  return values;
 }
 return {
  bind,
  previous(value) {
   const scope=validatedScope(value), current=namespaceFor(scope);
   return registry().filter(entry=>entry.scope.origin===scope.origin && entry.scope.operator_id===scope.operator_id && entry.namespace!==current).map(entry=>records(entry.scope,entry.namespace));
  },
  rememberDraft(scope,value) {
   const draft=safeDraft(value), bound=bind(scope), saved=drafts(scope), existing=saved.find(item=>item.command_id===draft.command_id);
   if(existing) {if(stableJSON(existing)!==stableJSON(draft)) throw new IntakeBrowserError("invalid_contract","Draft command ID already identifies different metadata");return;}
   if(saved.length>=64) throw new IntakeBrowserError("storage_unavailable","Draft metadata limit reached; reconcile saved commands first");
   put(`openfactory-intake.draft-metadata.v1.${bound.namespace}`,[...saved,draft]);
  },
  drafts,
  forgetDraft(scope,id) {
   if(!z.uuid().safeParse(id).success) throw new IntakeBrowserError("invalid_contract","Expected an exact command UUID");
   const bound=bind(scope), saved=drafts(scope);
   if(!saved.some(item=>item.command_id===id)) return;
   const remaining=saved.filter(item=>item.command_id!==id), key=`openfactory-intake.draft-metadata.v1.${bound.namespace}`;
   if(remaining.length) put(key,remaining);
   else {
    try {storage.removeItem(key);}
    catch {throw new IntakeBrowserError("storage_unavailable","Draft metadata could not be removed; recovery record was retained");}
   }
  },
 };
}
