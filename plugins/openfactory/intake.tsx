import { useEffect,useRef,useState } from "react";
import { useBbNavigate } from "@get-bb/plugin-sdk/app";
import { IntakeClient,IntakeError,takePairingFragment,type PendingResult } from "./intake-client";
import { questionBinding,sourceDiff,requestSchema,stableJSON,draftDisposition,type Command,type Disclosure as IntakeDisclosure,type Draft,type Enrollment,type IntakeLink,type ProgramResumeWork,type Project,type Question,type Request,type RequestView,type Review,type Session,type Turn } from "./intake-contract";
import { Presentation } from "./programs";
import { IntakeSubject } from "./intake-subject";
import { IntakeImport } from "./intake-import";
import "./intake.css";
import type { SendLifecycle } from "./intake-send";
import { pairingCapture } from "./intake-pairing";
import { IntakeReviewError } from "./intake-review";

const label=(value:string)=>value.replaceAll("_"," ").replaceAll("."," · ");
type DraftEdit={source:string;route:Draft["route"];format:Draft["format"];bundle_manifest_sha?:string};
function download(source:string,name:string,format="text/plain") {
 const url=URL.createObjectURL(new Blob([source],{type:`${format};charset=utf-8`}));
 const anchor=document.createElement("a");anchor.href=url;anchor.download=name.replace(/[^a-zA-Z0-9._-]/g,"_");anchor.click();setTimeout(()=>URL.revokeObjectURL(url),1000);
}
export function OpenFactoryIntake({subPath}:{subPath:string}) {
 const [client]=useState(()=>new IntakeClient());
 const navigate=useBbNavigate();
 const [session,setSession]=useState<Session|null>(null);
 const [enabled,setEnabled]=useState<boolean|null>(null);
 const [projects,setProjects]=useState<Project[]>([]);
 const [policySHA,setPolicySHA]=useState("");
 const [project,setProject]=useState("");
 const [error,setError]=useState("");
 const [notice,setNotice]=useState("");
 const [busy,setBusy]=useState(false);
 const [pairing,setPairing]=useState("");
 const [enrollment,setEnrollment]=useState<Enrollment|null>(null);
 const [pairingLinks]=useState(()=>pairingCapture(()=>takePairingFragment(window.location,window.history),(secret,cause)=>{
  setPairing(secret);setEnrollment(null);setError("");setNotice("");
  if(cause)report(cause);
 }));
 const [pending,setPending]=useState<PendingResult[]>([]);
 const [newIntention,setNewIntention]=useState(false);
 const [sendLifecycle,setSendLifecycle]=useState<SendLifecycle|null>(null);
 const [review,setReview]=useState<Review|null>(null);
 const [reviewAccepted,setReviewAccepted]=useState(false);
 const [reviewClock,setReviewClock]=useState(Date.now());
 const reviewExpired=review!==null&&Date.parse(review.claims.expires_at)<=reviewClock;
 const reviewRef=useRef<HTMLDialogElement>(null);
 const [refresh,setRefresh]=useState(0);
 const [composer,setComposer]=useState("");
 const [requests,setRequests]=useState<Request[]>([]);
 const [historyCursor,setHistoryCursor]=useState<string>();
 const [nextHistory,setNextHistory]=useState<string>();
 const [current,setCurrent]=useState<RequestView|null>(null);
 const [turns,setTurns]=useState<Turn[]>([]);
 const [turnCursor,setTurnCursor]=useState<number|undefined>();
 const [question,setQuestion]=useState<Question|null>(null);
 const [disclosed,setDisclosed]=useState(false);
 const [preparedDisclosure,setPreparedDisclosure]=useState<{key:string;value:IntakeDisclosure}|null>(null);
 const [bundleSHAs,setBundleSHAs]=useState<string[]>([]);
 const errorRef=useRef<HTMLDivElement>(null);
 const initialized=useRef(false);
 const actionLocked=useRef(false);
 const draftEdits=useRef(new Map<string,DraftEdit>());
 const answerDrafts=useRef(new Map<string,{text:string;refs:string}>());
 const segments=subPath.split("/").filter(Boolean);
 const requestID=segments[0]==="requests"?segments[1]:undefined;
 const parentRequestID=segments[0]==="new"?segments[1]:undefined;
 const subject=segments[0]==="subjects"&&segments[1]&&segments[2]?{kind:segments[1],id:segments[2]}:null;
 const activeProject=current?.request.project??project;
 const selectedProject=projects.find(item=>item.id===activeProject);
 const disclosureKey=JSON.stringify([activeProject,composer,current?.request.content_version??0,bundleSHAs]);
 const pendingBlocksNewIntent=pending.some(item=>!item.receipt&&!item.scope_changed&&(!item.unreplayable||!newIntention));
 function report(error:unknown) {
  if(error instanceof DOMException && (error.name==="NotAllowedError"||error.name==="AbortError")) setError("Passkey confirmation was cancelled or unavailable. No authority was granted.");
  else if(error instanceof IntakeError||error instanceof IntakeReviewError) setError(`${error.message} (${error.code})${error instanceof IntakeError&&error.current?" Refresh and review the current revision before acting again.":""}`);
  else setError(error instanceof Error?error.message:"The action could not be completed.");
  if(!client.session) setSession(null);
  setDisclosed(false);setNewIntention(false);
  try {const unresolved=client.pendingIDs().map(id=>({id,unreplayable:client.isUnreplayable(id),error:new IntakeError("receipt_pending","This command outcome is unresolved. Look up its receipt or retry only its original command ID.",true)}));setPending(previous=>[...previous.filter(item=>item.scope_changed),...unresolved]);setSendLifecycle(client.sendLifecycle());} catch { /* The original error remains visible; no new write is dispatched. */ }
  setTimeout(()=>errorRef.current?.focus(),0);
 }
 async function run(action:()=>Promise<void>) {
  if(actionLocked.current) return;actionLocked.current=true;setBusy(true);setError("");
  try {await action();} catch(error) {report(error);} finally {actionLocked.current=false;setBusy(false);}
 }
 async function recover() {setNewIntention(false);setPending(await client.recover());setSendLifecycle(client.sendLifecycle());}
 async function loadSession() {
  const session=await client.getSession();setSession(session);
  const data=await client.projects();setProjects(data.items);setPolicySHA(data.policy_sha??"");setProject(previous=>data.items.some(item=>item.id===previous)?previous:data.items[0]?.id??"");
  await recover();
 }
 // This effect must remain independent of the one-time bootstrap guard: StrictMode
 // cleanup/re-setup must always install a fresh same-page link listener.
 useEffect(()=>pairingLinks.listen(window),[pairingLinks]);
 useEffect(()=>{
  if(initialized.current) return;initialized.current=true;
  // One-time secret disappears from browser history before bootstrap or any network work.
  pairingLinks.capture();
  void (async()=>{try {const bootstrap=await client.bootstrap();setEnabled(bootstrap.enabled);if(bootstrap.enabled) {try {await loadSession();} catch(error) {if(!(error instanceof IntakeError && error.status===401)) report(error);}}}catch(error){report(error);}})();
 },[]);
 useEffect(()=>{if(!session) return;let stopped=false;void client.requests(project||undefined,historyCursor).then(page=>{if(!stopped){setRequests(page.items??[]);setNextHistory(page.next);}}).catch(report);return()=>{stopped=true;};},[session,project,historyCursor,refresh]);
 useEffect(()=>{
  setCurrent(null);setTurns([]);setTurnCursor(undefined);setQuestion(null);setReview(null);setDisclosed(false);
  if(!session||!requestID) return;
  const controller=new AbortController();let timer:number|undefined;
  const poll=async()=>{
   if(document.hidden) {timer=window.setTimeout(poll,4000);return;}
   try {
    const view=await client.request(requestID,controller.signal);if(controller.signal.aborted) return;
    const allTurns=await client.completeTurns(requestID,controller.signal);if(controller.signal.aborted)return;
    setTurns(allTurns);setTurnCursor(undefined);
    setCurrent(previous=>{if(previous && previous.request.content_version!==view.request.content_version){setReview(null);setQuestion(null);setDisclosed(false);}return view;});
   }catch(error){if(!controller.signal.aborted) report(error);}
   if(!controller.signal.aborted) timer=window.setTimeout(poll,4000);
  };
  void poll();return()=>{controller.abort();clearTimeout(timer);};
 },[session,requestID,refresh]);
 useEffect(()=>{const online=()=>{if(client.session) void recover();setRefresh(value=>value+1);};window.addEventListener("online",online);return()=>window.removeEventListener("online",online);},[]);
 useEffect(()=>{const dialog=reviewRef.current;if(review&&session&&dialog&&!dialog.open)dialog.showModal();return()=>dialog?.close();},[review,session]);
 useEffect(()=>{if(!review)return;setReviewClock(Date.now());const timer=window.setInterval(()=>setReviewClock(Date.now()),1000);return()=>clearInterval(timer);},[review]);
 useEffect(()=>{setReview(null);setDisclosed(false);setQuestion(null);},[subPath]);
 async function authorize(command:Command) {await run(async()=>{if(pending.some(item=>!item.receipt&&!item.scope_changed))throw new IntakeError("receipt_pending","Resolve current-scope pending command receipts before creating another authority grant.");const review=await client.review(command);setReviewClock(Date.now());setReview(review);setReviewAccepted(false);});}
 async function changed() {setReview(null);setRefresh(value=>value+1);await recover();}
 async function saveText(generate:boolean) {
  if(pendingBlocksNewIntent||client.sendLifecycle())throw new IntakeError("receipt_pending","Resolve the earlier current-scope send, or explicitly acknowledge a separate new intention without rebuilding any old envelope.");
  if(requestID&&!current)throw new IntakeError("controller_unavailable","Wait for the complete saved request context before saving another turn.");
  if(!selectedProject)throw new IntakeError("project_forbidden","Choose an enrolled project.");
  if(!composer.trim())throw new IntakeError("invalid_request","Enter text or use your phone keyboard’s dictation first.");
  if(generate&&!disclosed)throw new IntakeError("disclosure_required","Read and accept the visible exact text, provider, policy and context disclosure first.");
  const command:Command=requestID&&current?{command_id:crypto.randomUUID(),action:"turn.append",request_id:requestID,expected_content_version:current.request.content_version,payload:{client_message_id:crypto.randomUUID(),text:composer,...(question?{question:questionBinding(question)}:{})}}:{command_id:crypto.randomUUID(),action:"request.create",payload:{project:activeProject,text:composer,client_message_id:crypto.randomUUID(),...(parentRequestID?{parent_request_id:parentRequestID}:{})}};
  const saved=generate?await client.saveForSend(command):requestSchema.parse((await client.command(command)).result);
  setSendLifecycle(client.sendLifecycle());setComposer("");setDisclosed(false);setQuestion(null);setNotice("Text saved durably. No drafting or model call has been granted by Save.");
  navigate.toPluginPanel("intake",{subPath:`requests/${encodeURIComponent(saved.id)}`});
  if(generate) {
   const offer=await client.disclosure(saved.id,bundleSHAs);
   const persistedTurns=await client.completeTurns(saved.id);
   const visibleTurns=requestID?turns:[];
   const prior=persistedTurns.slice(0,-1),last=persistedTurns.at(-1);
   const sameTranscript=prior.length===visibleTurns.length&&prior.every((turn,index)=>{const visible=visibleTurns[index];return turn.sequence===visible.sequence&&turn.role===visible.role&&turn.text===visible.text&&turn.producer_step_id===visible.producer_step_id&&stableJSON(turn.question??null)===stableJSON(visible.question??null);})&&last?.role==="user"&&last.text===command.payload.text&&last.client_message_id===command.payload.client_message_id;
   const sameDraftHistory=saved.latest_draft_revision===(requestID?current?.request.latest_draft_revision??0:0);
   const sameProviders=stableJSON([...offer.disclosure.model_provider_ids].sort())===stableJSON([...selectedProject.model_provider_ids].sort());
   const sameBundles=stableJSON([...(offer.disclosure.bundle_manifest_shas??[])].sort())===stableJSON([...bundleSHAs].sort());
   if(offer.content_version===saved.content_version&&!!policySHA&&offer.disclosure.policy_sha===policySHA&&sameProviders&&sameBundles&&sameTranscript&&sameDraftHistory) {
    await client.draftSaved(saved.id,saved.content_version,offer.disclosure);setNotice("Text saved, then exact bounded drafting queued under its separate stable command ID. Queued does not mean running.");
   } else setNotice("Text saved. The current disclosure changed or adds context/policy/provider material. Review the saved context disclosure explicitly before Generate proposal; no model call was queued.");
  }
  await changed();
 }
 async function generateProposal() {
  if(!current||composer.trim()||!disclosed||preparedDisclosure?.key!==disclosureKey)throw new IntakeError("disclosure_required","Save changes, then prepare and accept the exact current saved-context disclosure first.");
  await client.draftSaved(current.request.id,current.request.content_version,preparedDisclosure.value);
  setDisclosed(false);setNotice("A bounded proposal is durably queued from the already-saved context. No duplicate text turn was added.");await changed();
 }
 const maxTurn=Number(selectedProject?.limits.max_turn_bytes??16384);
 const textBytes=new TextEncoder().encode(composer).byteLength;
 const terminal=current && ["submitted","cancelled","cancelling"].includes(current.request.state);
 return <main className="openfactory-board openfactory-intake">
  <header className="intake-header"><div><h1>OpenFactory Intake</h1><p>Dictate with your phone keyboard. This composer sends text, never audio.</p></div><button type="button" onClick={()=>navigate.toPluginPanel("intake",{subPath:requestID?`new/${encodeURIComponent(requestID)}`:""})}>New intake</button></header>
  <div ref={errorRef} tabIndex={-1} className="openfactory-error intake-alert" role="alert">{error}</div>
  <p role="status" aria-live="polite">{busy?"Working…":notice}</p>
  {enabled===null&&<p>Connecting to the private intake controller…</p>}
  {enabled===false&&<p>OpenFactory Intake is disabled. A trusted operator must configure private same-origin access and CLI enrollment. Existing OpenFactory and Programs browsing remains read-only.</p>}
  {enabled&&!session&&<section aria-labelledby="intake-login"><h2 id="intake-login">Operator passkey</h2><p>BB sign-in is not execution authority. Enroll using a one-time link from the trusted OpenFactory CLI, confirm its fingerprint there, then sign in freshly.</p>
   {pairing&&<><p>One-time pairing secret captured and removed from this URL. It is held only in this page’s memory.</p><button type="button" disabled={busy} onClick={()=>void run(async()=>{
    const epoch=pairingLinks.epoch();
    try {const registered=await client.register(pairing);if(pairingLinks.isCurrent(epoch)){setEnrollment(registered);setPairing("");}}
    catch(cause){if(pairingLinks.isCurrent(epoch))throw cause;}
   })}>Register a passkey</button></>}
   {enrollment&&<article><h3>Pending trusted CLI confirmation</h3><p>Pairing {enrollment.pairing_id}</p><p>Compare this credential fingerprint in the CLI before confirming:</p><code>{enrollment.fingerprint}</code><p>Credential {enrollment.credential_id}. Registration alone does not create a session or grant authority.</p></article>}
   <button type="button" disabled={busy||!!pairing} onClick={()=>void run(async()=>{await client.login();await loadSession();setEnrollment(null);setNotice("Signed in. Execution still requires a fresh passkey review for each exact action.");})}>Sign in with passkey</button>
   <p>If your credential was revoked or your session expired, unsent text remains in this page. Recovery and enrollment revocation require the trusted CLI.</p>
  </section>}
  {session&&<>
   <section className="intake-session"><p>Signed in as {session.label} · {session.operator_id}</p><p>Device {session.credential_id} · projects {session.project_scope.join(", ")}</p><button type="button" disabled={busy} onClick={()=>void run(async()=>{await client.logout();setSession(null);setCurrent(null);setTurns([]);setRequests([]);setReview(null);setNotice("Signed out. Unsent composer text remains only in this page. Pending command IDs remain available for receipt recovery after sign-in.");})}>Sign out</button></section>
   <p>Installation {session.installation_id} · authentication generation {session.auth_generation}. Pending journals are isolated by origin, operator, installation and generation. Earlier-generation intents remain unresolved and are never automatically replayed.</p>
   <section aria-label="Command reconciliation"><h2>Receipts and reconnect</h2><p>Persisted data is limited to command IDs, scope/version/lifecycle fences, and immutable drafting disclosure hashes/provider IDs. Private transcript, source text, session authentication and passkey assertions are never copied into browser storage.</p><button type="button" disabled={busy} onClick={()=>void run(recover)}>Check pending receipts</button>
    {pending.map(item=>{const saved=item.receipt?requestSchema.safeParse(item.receipt.result):null;return <article key={`${item.scope_changed?"previous":"current"}:${item.id}`}><code>{item.id}</code>{item.scope_changed&&<p>Previous authentication generation or installation. This stored intent cannot be replayed; a fresh Send is a separate explicit intention.</p>}{item.receipt?<><p>Committed: {label(item.receipt.action)}. This receipt records its exact domain effect, not completed delivery.</p><Presentation value={item.receipt}/>{saved?.success&&<button type="button" onClick={()=>navigate.toPluginPanel("intake",{subPath:`requests/${encodeURIComponent(saved.data.id)}`})}>Open recovered saved request</button>}</>:<><p>{item.error?.message} {item.error?.code==="not_found"?"No committed receipt is visible yet; the outcome remains unresolved. Do not issue the same action under a replacement command ID.":""}</p>{!item.scope_changed&&client.canRetry(item.id)&&<button type="button" disabled={busy} onClick={()=>void run(async()=>{await client.retry(item.id);await changed();})}>Retry exact saved command ID</button>}</>}</article>;})}
    {sendLifecycle&&<article><h3>Two-stage send: {sendLifecycle.phase}</h3><p>Save command {sendLifecycle.save_command_id} · drafting command {sendLifecycle.draft_command_id}</p>{sendLifecycle.phase==="saved"?<><p>Text is saved as request {sendLifecycle.request_id}, content version {sendLifecycle.content_version}. Drafting has not been dispatched. Continue with an explicit current-context review, or keep this saved text without drafting.</p><button type="button" disabled={busy} onClick={()=>navigate.toPluginPanel("intake",{subPath:`requests/${encodeURIComponent(sendLifecycle.request_id!)}`})}>Continue saved send</button><button type="button" disabled={busy} onClick={()=>void run(async()=>{client.cancelSavedSend();setSendLifecycle(null);setNotice("Saved text retained on the controller. The unsent drafting intent was cancelled; no model call was granted.");})}>Keep saved text without drafting</button></>:<p>The dispatched stage may have committed. Check its receipt; do not create a replacement command ID.</p>}</article>}
    {pending.some(item=>!item.receipt&&!item.scope_changed&&item.unreplayable)&&<label><input type="checkbox" checked={newIntention} disabled={busy} onChange={event=>{if(!event.target.checked){setNewIntention(false);return;}void run(async()=>{client.beginNewIntention();setSendLifecycle(client.sendLifecycle());setNewIntention(true);setDisclosed(false);});}}/>I understand the earlier command remains unresolved and may have committed. My next Send is a separate new intention, never a reconstruction or retry of that old command.</label>}
   </section>
   <label>Enrolled project<select value={activeProject} disabled={!!requestID} onChange={event=>{setProject(event.target.value);setHistoryCursor(undefined);setDisclosed(false);setBundleSHAs([]);}}>{projects.map(item=><option key={item.id} value={item.id}>{item.id}</option>)}</select></label>
   {requestID&&<p>A request’s project is immutable. <button type="button" onClick={()=>navigate.toPluginPanel("intake",{subPath:`new/${encodeURIComponent(requestID)}`})}>Start another project’s request</button></p>}{parentRequestID&&<p>This new request will link to {parentRequestID}. Selecting another enrolled project creates a separate request; accepted work is never rewritten.</p>}
   {!subject&&<details open={!requestID}><summary>Request history</summary><ul>{requests.map(request=><li key={request.id}><button type="button" aria-current={request.id===requestID?"page":undefined} onClick={()=>navigate.toPluginPanel("intake",{subPath:`requests/${encodeURIComponent(request.id)}`})}>{request.id} · {request.project} · {label(request.state)} · content v{request.content_version}</button></li>)}</ul>{historyCursor&&<button type="button" onClick={()=>setHistoryCursor(undefined)}>First requests</button>}{nextHistory&&<button type="button" onClick={()=>setHistoryCursor(nextHistory)}>Next requests</button>}</details>}
   {subject?<IntakeSubject key={`${subject.kind}:${subject.id}:${refresh}`} {...subject} client={client} answerDrafts={answerDrafts.current} onError={report} onAuthorize={command=>void authorize(command)} onChanged={()=>void changed()}/>:<>
    {current&&<section aria-label="Current intake"><h2>{current.request.id}</h2><p><strong>{label(current.request.state)}</strong> · content version {current.request.content_version} · draft revision {current.request.latest_draft_revision}</p><p>Draft calls {current.request.lifetime_dispatches??0}/{current.request.max_draft_dispatches??"policy bound"}. Intake steps share actual OpenFactory lane and daily-call capacity.</p><Presentation value={current.request.wait}/>{current.request.state==="submitted"&&<p>Approved handoff committed. Delivery is governed by the linked Feature, Plan or Program and its independent gates below.</p>}{current.request.state==="cancelling"&&<p>Cancellation requested. Worker ownership and capacity remain held until reconciliation settles; this is not yet cancelled.</p>}
     <details><summary>Request context, policy and limits</summary><Presentation value={current.request}/></details>
     <ol className="intake-transcript" aria-label="Durable intake transcript">{turns.map(turn=><li key={turn.sequence}><article><header><strong>{label(turn.role)}</strong> · sequence {turn.sequence} · <time>{new Date(turn.created_at).toLocaleString()}</time></header><p className="intake-text">{turn.text}</p><details><summary>Recorded provenance</summary><Presentation value={{producer_step_id:turn.producer_step_id,audit:turn.audit}}/></details></article></li>)}</ol>
     {turnCursor!==undefined&&turnCursor>0&&<button type="button" disabled={busy} onClick={()=>void run(async()=>{const page=await client.turns(current.request.id,turnCursor);setTurns(previous=>[...new Map([...previous,...(page.items??[])].map(turn=>[turn.sequence,turn])).values()].sort((a,b)=>a.sequence-b.sequence));setTurnCursor(page.next);})}>More transcript</button>}
     {(current.questions??[]).filter(item=>!item.answered_by_sequence).map(item=><article key={item.id}><h3>{item.text}</h3><p>Consequence: {item.consequence}</p><p>Answer type: {item.answer_type}. Bound to content v{item.content_version} · output {item.output_sha}</p>{item.choices?.map(choice=><button type="button" key={choice} disabled={!!terminal} onClick={()=>{setQuestion(item);setComposer(choice);setDisclosed(false);}}>{choice}</button>)}<button type="button" disabled={!!terminal} onClick={()=>{setQuestion(item);document.getElementById("intake-composer")?.focus();}}>Answer this question</button></article>)}
     {!current.draft&&!terminal&&<IntakeImport requestID={current.request.id} contentVersion={current.request.content_version} client={client} maxBytes={Number(selectedProject?.limits.max_canonical_bytes??262144)} availableBundles={bundleSHAs} edit={draftEdits.current.get(`${current.request.id}:import`)} onEdit={edit=>draftEdits.current.set(`${current.request.id}:import`,edit)} onChanged={()=>void changed()} onError={report}/>}
     {current.draft&&<DraftCard key={`${current.draft.request_id}:${current.draft.revision}`} draft={current.draft} request={current.request} links={current.links??[]} availableBundles={bundleSHAs} edit={draftEdits.current.get(`${current.draft.request_id}:${current.draft.revision}`)} onEdit={edit=>draftEdits.current.set(`${current.draft!.request_id}:${current.draft!.revision}`,edit)} disabled={busy||!!terminal||!!composer} client={client} onError={report} onChange={()=>void changed()} onAuthorize={command=>void authorize(command)}/>}
     <div className="intake-actions"><button type="button" disabled={busy||!!terminal} onClick={()=>void run(async()=>{await client.command({command_id:crypto.randomUUID(),action:"intake.cancel",request_id:current.request.id,expected_content_version:current.request.content_version,payload:{}});setNotice("Cancellation requested; capacity is released only after ownership settles.");await changed();})}>Cancel intake</button>{["blocked","cancelled"].includes(current.request.state)&&<button type="button" disabled={busy||!!composer} onClick={()=>void authorize({command_id:crypto.randomUUID(),action:"intake.retry",request_id:current.request.id,expected_content_version:current.request.content_version,payload:{}})}>Review bounded retry</button>}</div>
     {(current.links??[]).map(link=><article key={`${link.subject_kind}:${link.subject_id}`}><h3>Linked {link.subject_kind}: {link.subject_id}</h3><p>Approved subject revision {link.subject_revision}</p><button type="button" onClick={()=>navigate.toPluginPanel("intake",{subPath:`subjects/${encodeURIComponent(link.subject_kind)}/${encodeURIComponent(link.subject_id)}`})}>Open current subject and independent gates</button></article>)}
     <details><summary>Draft worker ownership and native threads</summary>{(current.steps??[]).map(step=><article key={step.id}><Presentation value={step}/>{step.thread_id&&<button type="button" onClick={()=>navigate.toThread(step.thread_id!)}>Open recorded worker thread</button>}</article>)}<p>Worker threads are controller-owned. Do not use their native chat composer to bypass bounded Intake dispatch.</p></details>
    </section>}
    <section className="intake-compose" aria-label="Text intake composer"><h2>{current?"Refine or answer":"Describe the work"}</h2>{terminal?<p>Accepted or cancelling requests cannot be rewritten. Start a new request for an amendment; existing authority remains bound to its original revision.</p>:<>
     {question&&<p>Answering: {question.text} <button type="button" onClick={()=>setQuestion(null)}>Clear question binding</button></p>}
     <label htmlFor="intake-composer">Your exact text<textarea id="intake-composer" rows={6} value={composer} onChange={event=>{setComposer(event.target.value);setDisclosed(false);setNewIntention(false);setReview(null);}} placeholder="Use your phone keyboard’s microphone to dictate text. Nothing executes from voice intent."/></label><p>{textBytes}/{maxTurn} UTF-8 bytes. No silent trimming.</p>
     <p>Drafting context includes every saved transcript turn, complete prior model output records, every canonical draft revision and its metadata, the disclosed project checks/targets/limits, and only the explicitly selected private bundle manifests. Historical drafts and worker-output hashes remain available above; no history is silently trimmed.</p>
     <Disclosure project={selectedProject} policySHA={policySHA} text={composer} bundleSHAs={bundleSHAs} saved={!!current&&!composer.trim()} prepared={preparedDisclosure?.key===disclosureKey?preparedDisclosure.value:null} checked={disclosed} busy={busy||textBytes>maxTurn} onPrepare={()=>void run(async()=>{if(!current)throw new IntakeError("invalid_request","Save the request before preparing its canonical context disclosure.");setDisclosed(false);const offer=await client.disclosure(current.request.id,bundleSHAs);if(offer.content_version!==current.request.content_version)throw new IntakeError("stale_revision","Saved content changed. Refresh before reviewing its disclosure.");setPreparedDisclosure({key:disclosureKey,value:offer.disclosure});})} onChange={setDisclosed}/>
     <div className="intake-actions"><button type="button" disabled={busy||pendingBlocksNewIntent||!composer.trim()||textBytes>maxTurn||!!sendLifecycle} onClick={()=>void run(()=>saveText(false))}>Save draft</button><button type="button" disabled={busy||pendingBlocksNewIntent||!disclosed||!composer.trim()||textBytes>maxTurn||!!sendLifecycle} onClick={()=>void run(()=>saveText(true))}>Send for bounded drafting</button>{current&&!composer.trim()&&<button type="button" disabled={busy||pendingBlocksNewIntent||!disclosed||preparedDisclosure?.key!==disclosureKey||["draft_queued","drafting"].includes(current.request.state)} onClick={()=>void run(generateProposal)}>Generate proposal</button>}<button type="button" disabled={!composer.trim()} onClick={()=>download(composer,`openfactory-intake-unsent-${Date.now()}.txt`)}>Download unsent text</button></div>
    </>}</section>
    <details><summary>Private input bundles</summary><p>Upload is not model consent. Files are staged privately; only an explicitly disclosed manifest may be read by a bounded draft. Unsupported formats fail explicitly; nothing is silently extracted.</p><label>Upload files<input type="file" multiple disabled={busy||!activeProject} onChange={event=>{const files=Array.from(event.target.files??[]);if(!files.length)return;void run(async()=>{const upload=await client.uploadBundle(activeProject,files);const bundle=upload.bundle as Record<string,unknown>|undefined;const manifest=typeof bundle?.manifest_sha==="string"?bundle.manifest_sha:"";if(!manifest) throw new IntakeError("invalid_contract","Upload returned no canonical bundle manifest hash.");setBundleSHAs(previous=>[...previous,manifest]);setDisclosed(false);setNotice("Bundle staged. Review and explicitly disclose its manifest before model access.");});event.target.value="";}}/></label><Presentation value={bundleSHAs}/></details>
   </>}
   {review&&<dialog ref={reviewRef} className="intake-review" aria-labelledby="intake-review-title" onCancel={()=>setReview(null)}><h2 id="intake-review-title">Exact authority review: {label(review.command.action)}</h2><p>This grants only the exact scope, revision, hashes and policy below. A passkey verifies your fresh presence and verification; the authenticator does not display this contract. Program approval is not architecture, experiment or product approval.</p><p>Review expires <time dateTime={review.claims.expires_at}>{new Date(review.claims.expires_at).toLocaleTimeString()}</time>.</p>{reviewExpired&&<p role="alert">This exact review has expired. Close it and request a fresh review; it cannot grant authority.</p>}{error&&<p role="alert">{error}</p>}<ReviewMaterial review={review}/><label><input type="checkbox" checked={reviewAccepted} disabled={reviewExpired} onChange={event=>setReviewAccepted(event.target.checked)}/>I reviewed this exact command, canonical source, assumptions, evidence, limits and policy. I intend this authority grant.</label><div className="intake-actions"><button type="button" disabled={busy} onClick={()=>setReview(null)}>Close without granting</button><button type="button" disabled={busy||!reviewAccepted||!!composer||reviewExpired} onClick={()=>void run(async()=>{await client.authorize(review);setNotice("Exact command committed. This is authority handoff, not completed delivery.");await changed();})}>Confirm exact action with passkey</button></div></dialog>}
  </>}
 </main>;
}

function ReviewMaterial({review}:{review:Review}) {
 const json=review.claims.subject_kind!=="feature"&&review.claims.subject_kind!=="plan";
 const name=`${review.claims.subject_id}-${review.claims_sha}`;
 const executionResume=review.command.action==="program.resume"&&review.resume_work?.step_kind==="experiment_run";
 return <section aria-label="Verified exact review material"><h3>Bound authority claims</h3><p>Exact canonical bytes were checked locally against the displayed claims, source, normalized payload, command, manifest and selected evidence. Domain revisions and effective policy remain controller-authoritative.</p><Presentation value={{claims:review.claims,claims_sha:review.claims_sha,command:review.command,findings:review.findings,created_at:review.created_at}}/>
  {review.command.action==="program.resume"&&<ProgramResumeMaterial work={review.resume_work}/>}
  {review.claims.execution_policy_sha&&<p>Execution-policy fingerprint (opaque and controller-controlled): <code>{review.claims.execution_policy_sha}</code>. Its binding is verified, but this browser does not independently recompute or display the private global policy. Review the selected experiment’s exact profile and safe limits in the bound manifest.</p>}
  {review.canonical_source&&<details open><summary>Exact submitted canonical source</summary><pre>{review.canonical_source}</pre><button type="button" onClick={()=>download(review.canonical_source,`${name}-source.${json?"json":"md"}`,json?"application/json":"text/markdown")}>Download exact reviewed source</button></details>}
  <details open={executionResume}><summary>{executionResume?"Exact frozen experiment contract for this retry":"Normalized exact authority payload"}</summary><p>This is the exact payload behind canonical_payload_sha. It may differ from original source formatting; an experiment grant or execution retry binds only the selected frozen contract, not its whole containing artifact.</p>{executionResume?<Presentation value={JSON.parse(review.canonical_payload)}/>:<pre>{review.canonical_payload}</pre>}<button type="button" onClick={()=>download(review.canonical_payload,`${name}-payload.${json?"json":"md"}`,json?"application/json":"text/markdown")}>{executionResume?"Download exact frozen retry contract":"Download exact authority payload"}</button></details>
  {review.selected_artifact_canonical&&<details><summary>Exact containing artifact</summary><pre>{review.selected_artifact_canonical}</pre><button type="button" onClick={()=>download(review.selected_artifact_canonical!,`${name}-artifact.json`,"application/json")}>Download exact containing artifact</button></details>}
  <details open={!executionResume}><summary>Full bound context, evidence, lineage and selected contract</summary><Presentation value={JSON.parse(review.manifest_canonical)}/><button type="button" onClick={()=>download(review.manifest_canonical,`${name}-manifest.json`,"application/json")}>Download exact review manifest</button></details>
  <details><summary>Complete exact review envelope</summary><Presentation value={review}/></details>
 </section>;
}

function ProgramResumeMaterial({work}:{work?:ProgramResumeWork}) {
 if(!work)return <section aria-label="Prerequisite-only Program resume"><h3>Prerequisite-only resume</h3><p>No current failed worker was selected. This action clears the Program wait for prerequisite re-evaluation; it does not retry an internal execution, select historical work, or create additional authority. Later progress remains subject to existing grants and lifetime limits.</p></section>;
 return <section aria-label="Exact selected current resume work"><h3>Retry one current settled worker</h3><p>The controller selected only step {work.step_id} ({label(work.step_kind)}), iteration {work.iteration}, Program revision {work.step_revision}. This does not retry all blocked work or historical steps.</p>
  <p>Worker {work.feature_id} · immutable revision {work.feature_revision} · state {label(work.feature_state)} · attempts consumed {work.feature_attempts}/{work.feature_max_attempts}. The attempt cap is not reset.</p>
  <p>Latest attempt {work.attempt_id} · {label(work.attempt_state)} · stage {label(work.attempt_stage)||"not recorded"} · last recorded update <time dateTime={work.attempt_updated_at}>{new Date(work.attempt_updated_at).toLocaleString()}</time>. Controller ownership marker: not active; this timestamp is not an attestation that external work stopped. The controller rechecks settled ownership atomically before retry.</p>
  <p>Input manifest SHA {work.input_manifest_sha}. Persisted attempt-summary fingerprint {work.attempt_output_sha} is controller-bound and opaque; the private summary, raw worker output and internal paths are not exported or independently rehashed by this browser.</p>
  {work.prior_launch_absent&&<p>The controller recorded no prior launch for this terminal attempt, after checking that no dispatch, reserved call, execution intent or receipt exists. This is an explicit bound record—not an inference from a missing receipt.</p>}
  {work.prior_run_id&&<section aria-label="Bound previous execution"><h4>Previous settled execution</h4><p>Run {work.prior_run_id} · receipt state {label(work.prior_receipt_state??"")} · conclusive marker {String(work.prior_receipt_conclusive??false)}.</p><p>Execution outcome {label(work.prior_execution_outcome??"not_recorded")} · observation outcome {label(work.prior_observation_outcome??"not_recorded")} · hypothesis outcome {label(work.prior_hypothesis_outcome??"not_recorded")}. Infrastructure failure, missing prerequisites and inconclusive observations are not scientific falsification.</p><p>Prior request SHA {work.prior_request_sha} · receipt SHA {work.prior_receipt_sha}. These are opaque controller-bound record fingerprints; private receipt contents are not exported or independently rehashed by this browser. Unknown or unsettled execution cannot be retried.</p></section>}
  {work.execution_policy_sha&&<p>Existing execution-policy fingerprint (opaque, controller-controlled): {work.execution_policy_sha}. Only the exact previously authorized frozen subject and bundle below are covered; this is not new experiment authority.</p>}
  <Presentation value={work}/><p>Resume does not reset lifetime budgets or grant architecture, experiments, or final product approvals. Existing policy, evidence, capacity and settlement gates still apply.</p>
 </section>;
}

function Disclosure({project,policySHA,text,bundleSHAs,saved,prepared,checked,busy,onPrepare,onChange}:{project?:Project;policySHA:string;text:string;bundleSHAs:string[];saved:boolean;prepared:IntakeDisclosure|null;checked:boolean;busy:boolean;onPrepare:()=>void;onChange:(value:boolean)=>void}) {
 return <section className="intake-disclosure"><h3>Before sending to a model</h3><p>Selected enrolled project: <strong>{project?.id??"none"}</strong>. Configured providers/models: {(project?.model_provider_ids??[]).join(", ")||"No configured provider disclosed"}. Current project policy SHA: {policySHA||"must be reviewed after saving"}.</p><p>Save draft stores exact text without model authority or capacity. Send first saves, then separately requests bounded read-only drafting only if the saved offer matches this reviewed version, policy, providers and bundle scope. Unexpected changes stop for fresh review. No product execution, research, spending, accounts, hardware or production authority is granted.</p><details open><summary>Exact new text and disclosed manifests</summary>{text?<pre>{text}</pre>:<p>The complete saved transcript above is the context; no duplicate turn will be added.</p>}<Presentation value={bundleSHAs}/></details><details><summary>Project checks, completion targets and lifetime limits</summary><Presentation value={project}/></details>{saved&&<button type="button" disabled={busy||!project} onClick={onPrepare}>Prepare exact current disclosure</button>}{prepared&&<><h3>Server-bound saved context and provider disclosure</h3><Presentation value={prepared}/></>}<label><input type="checkbox" disabled={busy||(!text.trim()&&!prepared)} checked={checked} onChange={event=>onChange(event.target.checked)}/>I authorize only bounded read-only drafting of this exact disclosed context by these providers.</label></section>;
}
function DraftCard({draft,request,links,availableBundles,edit,onEdit,disabled,client,onError,onChange,onAuthorize}:{draft:Draft;request:Request;links:IntakeLink[];availableBundles:string[];edit?:DraftEdit;onEdit:(edit:DraftEdit)=>void;disabled:boolean;client:IntakeClient;onError:(error:unknown)=>void;onChange:()=>void;onAuthorize:(command:Command)=>void}) {
 const navigate=useBbNavigate(),contentVersion=request.content_version;
 const [source,setSource]=useState(edit?.source??draft.canonical_source);
 const [route,setRoute]=useState(edit?.route??draft.route);
 const [format,setFormat]=useState(edit?.format??draft.format);
 const [bundleManifestSHA,setBundleManifestSHA]=useState(edit?.bundle_manifest_sha??draft.bundle_manifest_sha??"");
 function change(next:DraftEdit) {setSource(next.source);setRoute(next.route);setFormat(next.format);setBundleManifestSHA(next.bundle_manifest_sha??"");onEdit(next);}
 const [previous,setPrevious]=useState<Draft|null>(null);
 const [working,setWorking]=useState(false);
 const replaceLocked=useRef(false);
 const dirty=source!==draft.canonical_source||route!==draft.route||format!==draft.format||bundleManifestSHA!==(draft.bundle_manifest_sha??"");
 const disposition=draftDisposition(draft,request,links);
 async function replace() {if(replaceLocked.current)return;replaceLocked.current=true;setWorking(true);try {await client.command({command_id:crypto.randomUUID(),action:"draft.replace",request_id:draft.request_id,expected_content_version:contentVersion,payload:{route,format,canonical_source:source,...(route==="program"&&bundleManifestSHA?{bundle_manifest_sha:bundleManifestSHA}:{})}});onChange();}catch(error){onError(error);}finally{replaceLocked.current=false;setWorking(false);}}
 return <article className="intake-draft"><h2>{label(draft.route)} canonical review · draft {draft.revision}</h2><p>Domain revision {draft.domain_revision}</p><p>Exact source SHA {draft.source_sha}</p><p>Canonical SHA {draft.canonical_sha} · policy SHA {draft.policy_sha} · input SHA {draft.input_manifest_sha}</p><h3>Assumptions</h3><Presentation value={draft.assumptions}/><h3>Unresolved risks</h3><Presentation value={draft.unresolved_risks}/><h3>Validation findings</h3><Presentation value={draft.findings}/><p>Full checks, dependencies, completion target and contract fields remain in the exact canonical source, not just this summary.</p><pre className="intake-source">{draft.canonical_source}</pre><button type="button" onClick={()=>download(draft.canonical_source,`${draft.request_id}-draft-${draft.revision}.${draft.format==="program_json"?"json":"md"}`,draft.format==="program_json"?"application/json":"text/markdown")}>Download exact canonical source</button>
  {disposition.kind==="handed_off"&&<section aria-label="Committed draft handoff"><h3>Submitted immutable proposal</h3><p>This exact draft revision was handed off to {disposition.link.subject_kind} {disposition.link.subject_id}, revision {disposition.link.subject_revision}, under command {disposition.link.command_id}. Submission is not completed delivery; follow the subject’s independent gates and evidence.</p><button type="button" onClick={()=>navigate.toPluginPanel("intake",{subPath:`subjects/${encodeURIComponent(disposition.link.subject_kind)}/${encodeURIComponent(disposition.link.subject_id)}`})}>Open handed-off subject</button></section>}
  {disposition.kind==="submitted_unresolved"&&<p role="status">The request is submitted, but this draft’s exact durable handoff link is unavailable. Refresh to reconcile its subject; no approval or supersession is inferred.</p>}
  {disposition.kind==="unrelated"&&<p role="alert">This draft does not belong to the displayed request. Refresh before reviewing any action.</p>}
  {disposition.kind==="historical"&&<p role="status">This is a historical draft revision, not the current proposal. It cannot be approved.</p>}
  <details><summary>Refine or import an existing canonical contract</summary><label>Route<select value={route} onChange={event=>{const route=event.target.value as Draft["route"];change({source,route,format:route==="program"?"program_json":"feature_markdown"});}}><option value="feature">Feature</option><option value="plan">Plan</option><option value="program">Program</option></select></label>{route==="program"&&<label>Exact Program seed bundle<select value={bundleManifestSHA} onChange={event=>change({source,route,format,bundle_manifest_sha:event.target.value})}><option value="">No seed bundle</option>{[...new Set([...availableBundles,...(draft.bundle_manifest_sha?[draft.bundle_manifest_sha]:[])])].map(sha=><option key={sha} value={sha}>{sha}</option>)}</select></label>}<label>Import exact source<input type="file" accept=".md,.markdown,.json,text/markdown,application/json,text/plain" onChange={event=>{const file=event.target.files?.[0];if(file){if(file.size>262144){onError(new IntakeError("input_limit","Canonical input exceeds 256 KiB."));return;}void file.arrayBuffer().then(bytes=>change({source:new TextDecoder("utf-8",{fatal:true,ignoreBOM:true}).decode(bytes),route,format,...(bundleManifestSHA?{bundle_manifest_sha:bundleManifestSHA}:{})})).catch(onError);}event.target.value="";}}/></label><label>Canonical {format}<textarea rows={12} value={source} onChange={event=>change({source:event.target.value,route,format,...(bundleManifestSHA?{bundle_manifest_sha:bundleManifestSHA}:{})})}/></label>{dirty&&<><p>Unsaved source or seed changes remain in this page’s memory after session expiry. Old approval is disabled. Save a new immutable draft, then review again.</p><pre>{sourceDiff(draft.canonical_source,source)}</pre><p>Exact seed: {draft.bundle_manifest_sha??"none"} → {bundleManifestSHA||"none"}. Selecting a seed for import is not model consent.</p></>}<button type="button" disabled={disabled||working||!dirty} onClick={()=>void replace()}>Validate and save new draft revision</button></details>
  {draft.revision>1&&<details><summary>Compare draft history</summary><label>Previous draft revision<input type="number" min={1} max={draft.revision-1} defaultValue={draft.revision-1} onChange={event=>{const revision=Number(event.target.value);if(Number.isInteger(revision)&&revision>0&&revision<draft.revision)void client.draft(draft.request_id,revision).then(setPrevious).catch(onError);}}/></label><button type="button" onClick={()=>void client.draft(draft.request_id,draft.revision-1).then(setPrevious).catch(onError)}>Load preceding draft</button>{previous&&<><p>Compared with revision {previous.revision} · domain {previous.domain_revision} · SHA {previous.source_sha}</p><pre>{sourceDiff(previous.canonical_source,draft.canonical_source)}</pre></>}</details>}
  {disposition.kind==="superseded"&&<p role="alert">This unsubmitted draft is superseded by newer content. It cannot be approved.</p>}
  <button type="button" disabled={disabled||working||dirty||!disposition.canApprove} onClick={()=>{if(disposition.canApprove)onAuthorize({command_id:crypto.randomUUID(),action:"intake.approve_queue",request_id:draft.request_id,expected_content_version:contentVersion,payload:{draft_revision:draft.revision,source_sha:draft.source_sha,domain_revision:draft.domain_revision}});}}>Review and approve exact {draft.route} for queue</button>
 </article>;
}
