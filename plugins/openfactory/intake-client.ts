import { z } from "zod";
import { bootstrapSchema,sessionSchema,projectSchema,viewSchema,requestSchema,turnSchema,draftSchema,disclosureSchema,reviewSchema,receiptSchema,subjectSchema,errorSchema,page,sequencePage,ceremonySchema,authorizationOptionsSchema,enrollmentSchema,commandSchema,reviewMatches,type Session,type Command,type Review,type Receipt,type Disclosure,type Turn } from "./intake-contract.ts";
import { creationOptions,requestOptions,credentialJSON,pendingCommands,type PendingCommands } from "./intake-browser.ts";
import { sendJournal,type SendJournal } from "./intake-send.ts";
import { browserJournal,type BrowserJournal,type JournalScope } from "./intake-journal.ts";
import { canonicalSHA256,validateReview,validateAuthorizationOptions,validateAssertionBinding } from "./intake-review.ts";

export class IntakeError extends Error {
 code:string;retryable:boolean;current?:unknown;status:number;
 constructor(code:string,message:string,retryable=false,current?:unknown,status=0) { super(message);this.name="IntakeError";this.code=code;this.retryable=retryable;this.current=current;this.status=status; }
}
export type PendingResult = {id:string;receipt?:Receipt;error?:IntakeError;scope_changed?:boolean;unreplayable?:boolean};
type ClientEnvironment = {fetch?:typeof fetch;storage?:Pick<Storage,"getItem"|"setItem"|"removeItem">;credentials?:Pick<CredentialsContainer,"create"|"get">;origin?:string};
const authority:Record<string,true> = {"intake.approve_queue":true,"feature.approve":true,"program.approve_revision":true,"program.approve_architecture":true,"program.approve_experiment":true,"subject.retry":true,"program.resume":true,"intake.retry":true};
const object = z.record(z.string(),z.unknown());
const artifacts = page(object);
export class IntakeClient {
 session:Session|null=null;
 private transport:typeof fetch;
 private credentials:Pick<CredentialsContainer,"create"|"get">;
 private pending:PendingCommands;
 private sendState:SendJournal;
 private journal:BrowserJournal;
 private journalScope?:JournalScope;
 private origin:string;
 private liveRetries=new Map<string,{path:string;body:unknown}>();
 private unavailableBodies=new Set<string>();
 constructor(environment:ClientEnvironment={}) {
  this.transport=environment.fetch??globalThis.fetch.bind(globalThis);
  this.credentials=environment.credentials??navigator.credentials;
  // Persist only opaque command IDs and send lifecycle/version fences, never private text or auth.
  const storage=environment.storage??{
   getItem:(key:string)=>window.localStorage.getItem(key),setItem:(key:string,value:string)=>window.localStorage.setItem(key,value),removeItem:(key:string)=>window.localStorage.removeItem(key),
  };
  this.origin=environment.origin??globalThis.location?.origin??"http://localhost";
  this.journal=browserJournal(storage);this.pending=pendingCommands(storage,"unbound");this.sendState=sendJournal(storage,"unbound");
 }
 private ensureScope():JournalScope {
  if(!this.session)throw new IntakeError("auth_required","Sign in before using an authenticated command journal.");
  const scope:JournalScope={origin:this.origin,operator_id:this.session.operator_id,installation_id:this.session.installation_id,auth_generation:this.session.auth_generation};
  const old=this.journalScope;
  if(!old||old.origin!==scope.origin||old.operator_id!==scope.operator_id||old.installation_id!==scope.installation_id||old.auth_generation!==scope.auth_generation){
   const records=this.journal.bind(scope);const drafts=this.journal.drafts(scope);
   this.pending=records.pending;this.sendState=records.send;this.liveRetries.clear();this.unavailableBodies.clear();
   for(const command of drafts)this.liveRetries.set(command.command_id,{path:"/commands",body:command});
   this.journalScope=scope;
  }
  return scope;
 }
 private assertCurrentScope(scope:JournalScope) {
  const session=this.session;
  if(!session||scope.origin!==this.origin||scope.operator_id!==session.operator_id||scope.installation_id!==session.installation_id||scope.auth_generation!==session.auth_generation)throw new IntakeError("auth_generation_changed","Authentication scope changed. The previous command remains unresolved and will not be replayed.");
 }
 private settleReceipt(receipt:Receipt) {
  const scope=this.ensureScope(),state=this.sendState.get(),id=receipt.command_id;
  if(state?.save_command_id===id)this.recordSave(receipt);
  if(state?.draft_command_id===id)this.sendState.clear();
  this.pending.remove(id);this.journal.forgetDraft(scope,id);this.liveRetries.delete(id);this.unavailableBodies.delete(id);
 }
 async read<T>(path:string,schema:z.ZodType<T>,signal?:AbortSignal):Promise<T> { return this.call(path,schema,undefined,signal); }
 private async call<T>(path:string,schema:z.ZodType<T>,body?:unknown,signal?:AbortSignal):Promise<T> {
  if(!path.startsWith("/")||path.startsWith("//")||path.includes("..")) throw new IntakeError("invalid_contract","Invalid intake endpoint.");
  const headers:Record<string,string>={Accept:"application/json"};
  if(body!==undefined) {headers["Content-Type"]="application/json";if(this.session) headers["X-OpenFactory-CSRF"]=this.session.csrf_token;}
  let response:Response;
  try {response=await this.transport(`/openfactory-intake/v1${path}`,{method:body===undefined?"GET":"POST",credentials:"same-origin",mode:"same-origin",redirect:"error",cache:"no-store",headers,...(body===undefined?{}:{body:JSON.stringify(body)}),signal});}
  catch(error) {if(signal?.aborted) throw error;throw new IntakeError("connection_uncertain","Connection interrupted. A submitted command may already have committed; check its receipt before retrying.",true);}
  if(!/^application\/json(?:;|$)/i.test(response.headers.get("content-type")??"")) throw new IntakeError("invalid_contract","Controller returned an unexpected response.",false,undefined,response.status);
  const limit=4*1024*1024;
  if(Number(response.headers.get("content-length")??0)>limit) throw new IntakeError("bundle_limit","Controller response exceeds the safe display limit; use paged history.");
  const reader=response.body?.getReader();
  if(!reader)throw new IntakeError("invalid_contract","Controller response has no JSON body.");
  const decoder=new TextDecoder("utf-8",{fatal:true});let text="",count=0;
  try {
   for(;;){const part=await reader.read();if(part.done)break;count+=part.value.byteLength;if(count>limit){await reader.cancel();throw new IntakeError("bundle_limit","Controller response exceeds the safe display limit; use paged history.");}text+=decoder.decode(part.value,{stream:true});}
   text+=decoder.decode();
  }catch(error){if(error instanceof IntakeError)throw error;if(signal?.aborted)throw error;throw new IntakeError("connection_uncertain","Controller response was interrupted or invalid. Reconcile the command receipt before retrying.",true);}
  let value:unknown;
  try {value=JSON.parse(text);} catch {throw new IntakeError("invalid_contract","Controller response is not valid JSON.");}
  if(!response.ok) {
   const parsed=errorSchema.safeParse(value);
   if(!parsed.success) throw new IntakeError("invalid_contract","Controller returned an invalid error response.",false,undefined,response.status);
   const error=parsed.data.error;
   if(response.status===401) this.session=null;
   throw new IntakeError(error.code,error.message,error.retryable,error.current,response.status);
  }
  const parsed=schema.safeParse(value);
  if(!parsed.success) throw new IntakeError("invalid_contract","Controller response does not match the intake protocol.");
  return parsed.data;
 }
 bootstrap() {return this.read("/bootstrap",bootstrapSchema);}
 async getSession() {this.session=await this.read("/session",sessionSchema);this.ensureScope();return this.session;}
 projects() {return this.read("/projects",z.object({items:z.array(projectSchema),policy_sha:z.string().regex(/^[a-f0-9]{64}$/)}).passthrough());}
 async register(pairingSecret:string) {
  const options=await this.call("/auth/register/options",ceremonySchema,{pairing_secret:pairingSecret});
  const credential=credentialJSON(await this.credentials.create({publicKey:creationOptions(options.public_key)}));
  return this.call("/auth/register/finish",enrollmentSchema,{pairing_secret:pairingSecret,challenge_id:options.challenge_id,credential});
 }
 async login() {
  const options=await this.call("/auth/login/options",ceremonySchema,{});
  const credential=credentialJSON(await this.credentials.get({publicKey:requestOptions(options.public_key)}));
  this.session=await this.call("/auth/login/finish",sessionSchema,{challenge_id:options.challenge_id,credential});
  this.ensureScope();
  return this.session;
 }
 async logout() {await this.call("/auth/logout",z.unknown(),{});this.session=null;this.journalScope=undefined;this.liveRetries.clear();}
 requests(project?:string,after?:string) {const q=new URLSearchParams({limit:"50",...(project?{project}:{}),...(after?{after}:{})});return this.read(`/requests?${q}`,page(requestSchema));}
 request(id:string,signal?:AbortSignal) {return this.read(`/requests/${encodeURIComponent(id)}`,viewSchema,signal);}
 turns(id:string,after=0,signal?:AbortSignal) {return this.read(`/requests/${encodeURIComponent(id)}/turns?after=${after}&limit=50`,sequencePage(turnSchema),signal);}
 async completeTurns(id:string,signal?:AbortSignal):Promise<Turn[]> {
  const result:Turn[]=[];let after=0;
  for(;;){const page=await this.turns(id,after,signal);result.push(...(page.items??[]));if(result.length>256)throw new IntakeError("input_limit","Transcript exceeds the supported complete context display limit; no partial context is offered for drafting.");if(!page.next)return result;if(page.next<=after)throw new IntakeError("invalid_contract","Transcript cursor did not advance.");after=page.next;}
 }
 events(id:string,after=0,signal?:AbortSignal) {return this.read(`/requests/${encodeURIComponent(id)}/events?after=${after}&limit=50`,sequencePage(object),signal);}
 draft(id:string,revision:number) {return this.read(`/requests/${encodeURIComponent(id)}/drafts/${revision}`,draftSchema);}
 disclosure(requestID:string,bundleSHAs:string[]=[]) {const query=new URLSearchParams();for(const sha of bundleSHAs)query.append("bundle_manifest_sha",sha);return this.read(`/requests/${encodeURIComponent(requestID)}/disclosure${query.size?`?${query}`:""}`,z.object({content_version:z.number().int().nonnegative(),disclosure:disclosureSchema}).strict());}
 async command(command:Command):Promise<Receipt> {
  const parsed=commandSchema.parse(command);
  if(authority[parsed.action]) throw new IntakeError("human_verification_required","This action requires an exact review and a fresh passkey confirmation.");
  return this.dispatch(command.command_id,"/commands",parsed);
 }
 sendLifecycle() {if(this.session)this.ensureScope();return this.sendState.get();}
 isUnreplayable(id:string) {return this.unavailableBodies.has(id);}
 beginNewIntention() {
  this.ensureScope();const state=this.sendState.get();
  if(this.pendingIDs().some(id=>this.liveRetries.has(id)))throw new IntakeError("receipt_pending","An exact retryable envelope remains available. Reconcile it before declaring another intention.");
  if(state){if(state.phase!=="saving"||!this.unavailableBodies.has(state.save_command_id))throw new IntakeError("receipt_pending","The current send must be reconciled or explicitly kept without drafting first.");this.sendState.clear();}
  // Earlier unresolved IDs remain journaled. No old envelope is rebuilt or dispatched.
 }
 cancelSavedSend() {this.ensureScope();const state=this.sendState.get();if(state?.phase!=="saved")throw new IntakeError("receipt_pending","A dispatched or unresolved send cannot be discarded.");this.sendState.clear();}
 private recordSave(receipt:Receipt) {
  const request=requestSchema.parse(receipt.result);this.sendState.saved(request.id,request.content_version);return request;
 }
 async saveForSend(command:Command) {
  if(!["request.create","turn.append"].includes(command.action))throw new IntakeError("invalid_contract","Send must first save an exact text turn.");
  commandSchema.parse(command);
  const scope=this.ensureScope();
  const send=this.sendState,previous=send.get();
  send.begin(command.command_id,previous?.save_command_id===command.command_id?previous.draft_command_id:crypto.randomUUID());
  try {const receipt=await this.command(command);this.assertCurrentScope(scope);return this.recordSave(receipt);}
  catch(error){if(error instanceof IntakeError&&error.status>=400&&error.status<500&&![401,408,429].includes(error.status))send.clear();throw error;}
 }
 async draftSaved(requestID:string,contentVersion:number,disclosure:Disclosure) {
  const scope=this.ensureScope();
  const send=this.sendState,state=send.get();
  if(state && (state.phase!=="saved"||state.request_id!==requestID||state.content_version!==contentVersion))throw new IntakeError("receipt_pending","Recover the previous send lifecycle before generating another proposal.");
  const id=state?.draft_command_id??crypto.randomUUID();
  if(state)send.drafting();
  try {const receipt=await this.command({command_id:id,action:"intake.draft",request_id:requestID,expected_content_version:contentVersion,payload:{disclosure}});this.assertCurrentScope(scope);if(state)send.clear();return receipt;}
  catch(error){if(state&&error instanceof IntakeError&&error.status>=400&&error.status<500&&![401,408,429].includes(error.status))send.clear();throw error;}
 }
 async review(command:Command):Promise<Review> {
  const parsed=commandSchema.parse(command);
  const scope=this.ensureScope();
  if(!authority[parsed.action]) throw new IntakeError("unsupported_action","This action does not grant execution authority.");
  const current=await this.projects();this.assertCurrentScope(scope);
  const review=await this.call("/reviews",reviewSchema,{command:parsed});this.assertCurrentScope(scope);
  if(!reviewMatches(review,command)) throw new IntakeError("invalid_contract","The returned review does not bind the requested command.");
  const checked=await validateReview(review,command,this.session!,current.policy_sha);this.assertCurrentScope(scope);
  if(this.session!.credential_id!==checked.claims.credential_id)throw new IntakeError("stale_subject","Sign-in credential changed while preparing the review.");
  return checked;
 }
 async authorize(review:Review):Promise<Receipt> {
  const scope=this.ensureScope();
  const current=await this.projects();this.assertCurrentScope(scope);
  const checked=await validateReview(review,review.command,this.session!,current.policy_sha);this.assertCurrentScope(scope);
  const options=await this.call("/authorizations/options",authorizationOptionsSchema,{review_id:checked.id});this.assertCurrentScope(scope);
  validateAuthorizationOptions(checked,options,await canonicalSHA256(checked.command_canonical));
  if(this.session!.credential_id!==checked.claims.credential_id)throw new IntakeError("stale_subject","Sign-in credential changed. Fetch and review a fresh exact command.");
  const challenge=z.string().parse(options.public_key.challenge);
  // Native cancellation commits nothing; the returned assertion must still bind the reviewed ceremony.
  const assertion=credentialJSON(await this.credentials.get({publicKey:requestOptions(options.public_key)}));
  this.assertCurrentScope(scope);
  if(this.session!.credential_id!==checked.claims.credential_id)throw new IntakeError("stale_subject","Sign-in credential changed during confirmation.");
  validateAssertionBinding(assertion,challenge,this.origin);
  return this.dispatch(checked.command.command_id,"/authorizations/commit",{command_id:checked.command.command_id,challenge_id:options.challenge_id,assertion});
 }
 private async dispatch(id:string,path:string,body:unknown):Promise<Receipt> {
  const scope=this.ensureScope();
  if(path==="/commands"&&(body as Command).action==="intake.draft"){
   this.journal.rememberDraft(scope,body as Command);
   // First dispatch and reconnect share one scope-bound immutable envelope;
   // neither can reconstruct authority from the current mutable UI state.
   const saved=this.journal.drafts(scope).find(command=>command.command_id===id);
   if(!saved)throw new IntakeError("invalid_contract","The exact durable drafting envelope is unavailable; no command was dispatched.");
   body=saved;
  }
  // Persist the ID before sending: inability to recover a receipt must fail closed.
  this.pending.add(id);this.liveRetries.set(id,{path,body});
  const pendingAtDispatch=this.pending;
  try {
   const receipt=await this.call(path,receiptSchema,body);
   if(receipt.command_id!==id) throw new IntakeError("invalid_contract","Controller returned a receipt for another command.");
   this.assertCurrentScope(scope);
   this.settleReceipt(receipt);return receipt;
  } catch(error) {
   // Typed definitive rejection is not an uncertain write. Keep IDs across transient errors.
   if(error instanceof IntakeError && error.status>=400 && error.status<500 && ![401,408,429].includes(error.status)) {pendingAtDispatch.remove(id);this.journal.forgetDraft(scope,id);this.liveRetries.delete(id);}
   throw error;
  }
 }
 pendingIDs() {if(this.session)this.ensureScope();const ids=this.pending.list();if(this.journalScope)for(const command of this.journal.drafts(this.journalScope))if(!ids.includes(command.command_id))ids.push(command.command_id);return ids;}
 async recover():Promise<PendingResult[]> {
  const scope=this.ensureScope(),result:PendingResult[]=[],state=this.sendState.get();
  const ids=this.pendingIDs(),lifecycleID=state?.phase==="saving"?state.save_command_id:state?.phase==="drafting"?state.draft_command_id:undefined;
  if(lifecycleID&&!ids.includes(lifecycleID))ids.push(lifecycleID);
  for(const id of ids) {
   try {const receipt=await this.read(`/commands/${encodeURIComponent(id)}`,receiptSchema);this.assertCurrentScope(scope);if(receipt.command_id!==id)throw new IntakeError("invalid_contract","Receipt command ID mismatch.");this.settleReceipt(receipt);result.push({id,receipt});}
   catch(error){const unavailable=error instanceof IntakeError&&error.status===404&&!this.liveRetries.has(id);if(unavailable)this.unavailableBodies.add(id);result.push({id,unreplayable:unavailable,error:unavailable?new IntakeError("receipt_pending","Only the command ID survived reconnect and no committed receipt is visible. Its exact private envelope is unavailable. It remains unresolved and will never be rebuilt from the current composer."):error instanceof IntakeError?error:new IntakeError("connection_uncertain","Receipt lookup interrupted.",true)});}
  }
  if(!this.session)return result;
  this.assertCurrentScope(scope);
  for(const previous of this.journal.previous(scope)){
   const oldState=previous.send.get(),oldIDs=previous.pending.list();
   for(const command of this.journal.drafts(previous.scope))if(!oldIDs.includes(command.command_id))oldIDs.push(command.command_id);
   if(oldState){const needed=oldState.phase==="saving"?[oldState.save_command_id,oldState.draft_command_id]:[oldState.draft_command_id];for(const id of needed)if(!oldIDs.includes(id))oldIDs.push(id);}
   for(const id of oldIDs){
    if(previous.scope.installation_id===scope.installation_id){
     try {const receipt=await this.read(`/commands/${encodeURIComponent(id)}`,receiptSchema);this.assertCurrentScope(scope);if(receipt.command_id!==id)throw new IntakeError("invalid_contract","Receipt command ID mismatch.");previous.pending.remove(id);this.journal.forgetDraft(previous.scope,id);if(oldState?.draft_command_id===id)previous.send.clear();result.push({id,receipt,scope_changed:true});continue;}catch(error){if(!this.session)return result;this.assertCurrentScope(scope);}
    }
    result.push({id,scope_changed:true,error:new IntakeError("auth_generation_changed","This command belongs to a different authentication generation or installation. Its outcome is unresolved. It will never be replayed or rebuilt; only committed-receipt lookup is allowed.")});
   }
  }
  return result;
 }
 canRetry(id:string) {if(!this.session)return false;this.ensureScope();return this.liveRetries.has(id);}
 async retry(id:string) {
  const scope=this.ensureScope();
  const original=this.liveRetries.get(id);
  if(!original) throw new IntakeError("receipt_pending","Only the command ID survived reconnect. Check its receipt; do not recreate the action with a new ID.");
  try {
   const receipt=await this.read(`/commands/${encodeURIComponent(id)}`,receiptSchema);
   if(receipt.command_id!==id)throw new IntakeError("invalid_contract","Receipt command ID mismatch.");
   this.assertCurrentScope(scope);this.settleReceipt(receipt);return receipt;
  }catch(error){if(!(error instanceof IntakeError&&error.status===404&&error.code==="not_found"))throw error;}
  // The exact original body and command ID are replayed, never re-created from current UI state.
  this.assertCurrentScope(scope);
  return this.dispatch(id,original.path,original.body);
 }
 getSubject(kind:string,id:string) {return this.read(`/subjects/${encodeURIComponent(kind)}/${encodeURIComponent(id)}`,subjectSchema);}
 getSubjectArtifacts(kind:string,id:string,after?:string) {return this.read(`/subjects/${encodeURIComponent(kind)}/${encodeURIComponent(id)}/artifacts?limit=50${after?`&after=${encodeURIComponent(after)}`:""}`,artifacts).then(page=>({...page,items:page.items??[]}));}
 getSubjectArtifact(kind:string,id:string,artifactID:string) {return this.read(`/subjects/${encodeURIComponent(kind)}/${encodeURIComponent(id)}/artifacts/${encodeURIComponent(artifactID)}`,object);}
 async uploadBundle(project:string,files:File[]) {
  const manifestFiles=await Promise.all(files.map(async(file,index)=>({id:String(index+1),path:file.webkitRelativePath||file.name,bytes:file.size,content_sha:Array.from(new Uint8Array(await crypto.subtle.digest("SHA-256",await file.arrayBuffer()))).map(byte=>byte.toString(16).padStart(2,"0")).join("")})));
  const upload=await this.call("/bundles",z.object({id:z.string()}).passthrough(),{project,files:manifestFiles});
  for(let index=0;index<files.length;index++) {
   const response=await this.transport(`/openfactory-intake/v1/bundles/${encodeURIComponent(upload.id)}/files/${manifestFiles[index].id}`,{method:"PUT",credentials:"same-origin",mode:"same-origin",redirect:"error",cache:"no-store",headers:{"Content-Type":"application/octet-stream","X-OpenFactory-CSRF":this.session?.csrf_token??""},body:files[index]});
   if(!response.ok) {let value:unknown;try{value=await response.json();}catch{}const parsed=errorSchema.safeParse(value);throw new IntakeError(parsed.success?parsed.data.error.code:"invalid_contract",parsed.success?parsed.data.error.message:"Bundle upload failed.",false,undefined,response.status);}
  }
  return this.call(`/bundles/${encodeURIComponent(upload.id)}/finalize`,object,{});
 }
}

// Capture enrollment secrets before any await/network call and remove all fragment bytes.
export function takePairingFragment(location:Pick<Location,"hash"|"pathname"|"search">,history:Pick<History,"replaceState"|"state">):string {
 const fragment=location.hash;
 if(!fragment) return "";
 history.replaceState(history.state,"",`${location.pathname}${location.search}`);
 const values=new URLSearchParams(fragment.slice(1));
 const secret=values.get("pairing")??"";
 if(!/^[A-Za-z0-9_-]{43}$/.test(secret)) throw new IntakeError("invalid_request","Pairing link is invalid. Ask the operator CLI for a new one-time link.");
 return secret;
}
