import { useEffect, useRef, useState } from "react";
import { useBbNavigate } from "@get-bb/plugin-sdk/app";
import { Presentation } from "./programs";
import type { IntakeClient } from "./intake-client";
import type { Command, Subject } from "./intake-contract";

type RecordValue = Record<string, unknown>;
const object = (value: unknown): RecordValue | undefined => value !== null && typeof value === "object" && !Array.isArray(value) ? value as RecordValue : undefined;
const text = (value: unknown) => typeof value === "string" ? value : "";
const records = (value: unknown): RecordValue[] => Array.isArray(value) ? value.flatMap(item => object(item) ? [item as RecordValue] : []) : [];
const command = (action: string, payload: RecordValue): Command => ({command_id: crypto.randomUUID(), action, payload});
const message = (error: unknown) => error instanceof Error ? error.message : "The operation failed. Refresh before trying again.";
function download(source: string, name: string) {
 const url = URL.createObjectURL(new Blob([source], {type:"application/json;charset=utf-8"}));
 const link = document.createElement("a");
 link.href = url; link.download = name.replace(/[^a-zA-Z0-9._-]/g,"_") + ".json";
 document.body.appendChild(link); link.click(); link.remove(); window.setTimeout(() => URL.revokeObjectURL(url), 1000);
}
export function IntakeSubject({kind,id,client,onError,onAuthorize,onChanged,answerDrafts}: {
 kind:string; id:string; client:IntakeClient; onError:(error:unknown)=>void; onAuthorize:(command:Command)=>void; onChanged:()=>void; answerDrafts:Map<string,{text:string;refs:string}>;
}) {
 const [subject,setSubject] = useState<Subject>();
 const [artifacts,setArtifacts] = useState<RecordValue[]>([]);
 const [artifactPages,setArtifactPages] = useState<RecordValue[]>([]);
 const [next,setNext] = useState<string>();
 const [busy,setBusy] = useState(false);
 const [error,setError] = useState("");
 const [notice,setNotice] = useState("");
 const generation = useRef(0);
 const locked = useRef(false);
 const navigate = useBbNavigate();
 const fail = (cause:unknown) => { setError(message(cause)); onError(cause); };
 async function refresh() {
  if (locked.current) return;
  locked.current = true; setBusy(true); setError("");
  const current = ++generation.current;
  try {
   const value = await client.getSubject(kind,id);
   const page = await client.getSubjectArtifacts(kind,id);
   if (current !== generation.current) return;
   setSubject(value); setArtifacts(page.items ?? []); setNext(page.next);
   const {items: _items,next: _next,...metadata} = page; setArtifactPages([metadata]);
  } catch (cause) { if (current === generation.current) { setSubject(undefined); fail(cause); } }
  finally { if (current === generation.current) {locked.current = false; setBusy(false);} }
 }
 useEffect(() => { void refresh(); return () => {generation.current++; locked.current = false;}; }, [kind,id,client]);
 async function mutate(value:Command) {
  if (locked.current) throw new Error("Another subject operation is in progress. Your answer has been retained.");
  locked.current = true; setBusy(true); setError("");
  try { await client.command(value); setNotice(value.action.endsWith("answer") ? "Answer saved as a new unapproved revision. Review the new revision separately; no execution authority was granted." : "Cancellation requested. Active ownership may still be settling; inspect the refreshed server state."); onChanged(); }
  catch (cause) { fail(cause); throw cause; }
  finally {locked.current = false; setBusy(false); await refresh();}
 }
 async function more() {
  if (!next || locked.current) return;
  locked.current = true; setBusy(true); setError("");
  try {
   const page = await client.getSubjectArtifacts(kind,id,next);
   setArtifacts(previous => [...previous,...(page.items ?? []).filter(item => !previous.some(old => old.id === item.id))]); setNext(page.next);
   const {items: _items,next: _next,...metadata} = page; setArtifactPages(previous => [...previous,metadata]);
  } catch(cause) {setError(`${message(cause)} If the artifact snapshot changed or its cursor expired, refresh the subject to restart history at the current snapshot.`); onError(cause);} finally {locked.current = false; setBusy(false);}
 }
 const payload = subject ? {kind:subject.kind,id:subject.id,revision:subject.revision} : undefined;
 const revisionHash = text(subject?.program_revision?.revision_hash);
 return <section aria-label="Intake subject" className="intake-subject">
  <h1>{kind === "program" ? "Program" : kind === "plan" ? "Planning feature" : "Feature"}: {id}</h1>
  <button type="button" disabled={busy} onClick={() => void refresh()}>Refresh subject and current revision</button>
  <div aria-live="polite">{busy && <p>Loading or saving…</p>}{notice && <p>{notice}</p>}</div>
  {error && <p role="alert">{error}</p>}
  {subject && <>
   <p>Project {subject.project} · exact revision <strong>{subject.revision}</strong></p>
   <p>Actions bind this displayed version. The server checks current revision, policy, evidence and ownership. A review is not an approval; each execution grant requires a separate exact-command passkey ceremony.</p>
   <h2>Complete subject, specification, answers, seed lineage and server state</h2>
   <Presentation value={subject}/>
   <h2>{subject.kind === "program" ? "Program revision authority" : "Feature / planning authority"}</h2>
   {payload && <div>
    <button type="button" disabled={busy} onClick={() => onAuthorize(command(subject.kind === "program" ? "program.approve_revision" : "feature.approve",payload))}>{subject.kind === "program" ? "Review approval of this Program revision only" : "Review approval of this feature revision"}</button>
    <button type="button" disabled={busy} onClick={() => onAuthorize(command(subject.kind === "program" ? "program.resume" : "subject.retry",payload))}>{subject.kind === "program" ? "Review resume of this Program revision" : "Review retry of this feature revision"}</button>
    <button type="button" disabled={busy} onClick={() => {void mutate(command("subject.cancel",payload)).catch(()=>{});}}>Cancel this subject revision</button>
   </div>}
   <p>Program revision approval does not approve architecture, experiments or published feature proposals. Cancellation is a request, not proof that a worker has stopped. Waits and missing prerequisites remain server-authoritative.</p>
   {records(subject.planning_questions).map(question => <AnswerForm key={`${subject.revision}:${text(question.id)}`} draftKey={JSON.stringify(["planning",subject.id,subject.revision,question.feature_revision,question.attempt_id,question.output_sha,question.id])} answerDrafts={answerDrafts} title={text(question.question)} disabled={busy} binding={question} onSubmit={async answer => {
    const featureID = text(question.feature_id), featureRevision = text(question.feature_revision), attemptID = text(question.attempt_id), outputSHA = text(question.output_sha), questionID = text(question.id);
    if (!featureID || !featureRevision || !attemptID || !outputSHA || !questionID || featureRevision !== subject.revision) throw new Error("This planning question is not bound to the displayed revision. Refresh the subject.");
    await mutate(command("planning.answer",{feature_id:featureID,feature_revision:featureRevision,attempt_id:attemptID,output_sha:outputSHA,question_id:questionID,text:answer}));
   }}/>) }
   <h2>Artifact history and separate exact grants</h2>
   <p>History is paged. Earlier artifacts are not automatically current evidence or effective authority. Canonical detail is fetched in full on demand; no shortened presentation is offered as a complete download.</p>
   <section aria-label="Artifact history read snapshots"><h3>Server page snapshots and output positions</h3><p>These are the server's classifications for each loaded history page, not grants. Sharing the displayed Program revision does not make an artifact effective evidence or approved authority.</p>{artifactPages.map((metadata,index)=><section key={index} aria-label={`Artifact page ${index+1} metadata`}><h4>Page {index+1}: snapshot, revision hash and output states</h4><Presentation value={metadata}/></section>)}</section>
   {artifacts.map((artifact,index) => <SubjectArtifact key={`${subject.revision}:${text(artifact.id)}:${index}`} artifact={artifact} subject={subject} revisionHash={revisionHash} client={client} answerDrafts={answerDrafts} disabled={busy} onError={fail} onAuthorize={onAuthorize} onAnswer={mutate} onFeature={featureID => navigate.toPluginPanel("intake",{subPath:"subjects/feature/"+encodeURIComponent(featureID)})}/>) }
   {!artifacts.length && <p>No artifacts recorded on this page.</p>}
   {next ? <button type="button" disabled={busy} onClick={() => void more()}>Load more artifact history</button> : <p>End of loaded artifact history.</p>}
  </>}
 </section>;
}
function AnswerForm({title,binding,disabled,supporting=false,onSubmit,answerDrafts,draftKey}: {title:string;binding:unknown;disabled:boolean;supporting?:boolean;onSubmit:(answer:string,refs:RecordValue[])=>Promise<void>;answerDrafts:Map<string,{text:string;refs:string}>;draftKey:string}) {
 const [answer,setAnswer] = useState(() => answerDrafts.get(draftKey)?.text ?? ""); const [refs,setRefs] = useState(() => answerDrafts.get(draftKey)?.refs ?? "[]"); const [error,setError] = useState(""); const [saving,setSaving] = useState(false);
 return <form onSubmit={event => {event.preventDefault(); if(saving || disabled) return; setError(""); void (async () => {
  try {
   if (!answer.trim()) throw new Error("Enter an answer.");
   const parsed:unknown = supporting ? JSON.parse(refs) : [];
   if (!Array.isArray(parsed) || parsed.some(value => {const item=object(value); return !item || Object.keys(item).some(key => !["kind","id","sha","revision"].includes(key)) || !text(item.kind) || !text(item.id) || !/^[a-fA-F0-9]{64}$/.test(text(item.sha)) || (item.revision !== undefined && (typeof item.revision !== "number" || !Number.isSafeInteger(item.revision) || item.revision < 0));})) throw new Error("Supporting references must be a JSON array of objects containing nonempty kind, id, a 64-character SHA-256 sha, and optionally a nonnegative integer revision. Unknown fields are not accepted.");
   setSaving(true); await onSubmit(answer,parsed as RecordValue[]); answerDrafts.delete(draftKey); setAnswer(""); setRefs("[]");
  } catch(cause) {setError(message(cause));} finally {setSaving(false);}
 })();}}>
  <h3>{title || "Consequential question"}</h3><Presentation value={binding}/>
  <label>Answer<textarea required value={answer} disabled={disabled || saving} onChange={event=>{const value=event.target.value;setAnswer(value);answerDrafts.set(draftKey,{text:value,refs});}}/></label>
  {supporting && <label>Supporting references (JSON array of kind, id, sha and optional revision; empty array allowed)<textarea value={refs} disabled={disabled || saving} onChange={event=>{const value=event.target.value;setRefs(value);answerDrafts.set(draftKey,{text:answer,refs:value});}} spellCheck={false}/></label>}
  <p>Saving creates a new unapproved revision. Review and approve it separately.</p>
  {error && <p role="alert">{error}</p>}<button type="submit" disabled={disabled || saving}>{saving ? "Saving answer…" : "Save answer as new unapproved revision"}</button>
 </form>;
}
function SubjectArtifact({artifact,subject,revisionHash,client,disabled,onError,onAuthorize,onAnswer,onFeature,answerDrafts}: {
 artifact:RecordValue;subject:Subject;revisionHash:string;client:IntakeClient;disabled:boolean;onError:(error:unknown)=>void;onAuthorize:(command:Command)=>void;onAnswer:(command:Command)=>Promise<void>;onFeature:(id:string)=>void;answerDrafts:Map<string,{text:string;refs:string}>;
}) {
 const [detail,setDetail] = useState<RecordValue>(); const [loading,setLoading] = useState(false); const [error,setError] = useState("");
 const navigate = useBbNavigate();
 const id=text(artifact.id), kind=text(artifact.kind), sha=text(artifact.content_sha);
 async function load() {
  if(loading) return; setLoading(true); setError("");
  try {
   const value = await client.getSubjectArtifact(subject.kind,subject.id,id);
   const metadata = object(value.artifact);
   if (!metadata || metadata.id !== id || metadata.kind !== kind || metadata.content_sha !== sha || metadata.revision !== artifact.revision || typeof value.canonical_source !== "string" || !object(value.payload)) throw new Error("Canonical artifact response does not match the displayed artifact binding. Refresh the subject.");
   setDetail(value);
  } catch(cause) {setError(message(cause));onError(cause);} finally {setLoading(false);}
 }
 const canonical = typeof detail?.canonical_source === "string" ? detail.canonical_source : undefined;
 const payload = object(detail?.payload);
 const current = !!revisionHash && artifact.revision === subject.program_revision?.number;
 const threadID=text(artifact.producer_thread_id);
 return <article>
  <h3>{kind || "Artifact"} · {id}</h3><Presentation value={artifact}/>
  {threadID && <button type="button" onClick={()=>navigate.toThread(threadID)}>Open recorded worker thread {threadID}</button>}
  <button type="button" disabled={loading || disabled} onClick={()=>void load()}>{loading ? "Loading canonical artifact…" : "Read full canonical artifact"}</button>
  {error && <p role="alert">{error}</p>}
  {detail && <section aria-label="Server output position"><h4>Server-recorded current output / historical receipt</h4><p>Output position is not evidence authority. A historical receipt records earlier work; it is not the current iteration's output. Even current output is not automatically approved evidence or an execution grant.</p>{detail.output_state !== undefined ? <Presentation value={detail.output_state}/> : <p>No output-position classification was supplied by the server.</p>}{detail.snapshot !== undefined && <><h4>Read snapshot</h4><Presentation value={detail.snapshot}/></>}</section>}
  {detail && <><h4>Full canonical detail</h4><Presentation value={detail}/>{canonical !== undefined ? <button type="button" onClick={()=>download(canonical,`${subject.id}-${id}-${sha}`)}>Download exact canonical artifact</button> : <p>Exact source bytes were not supplied; no canonical download is available.</p>}</>}
  {kind === "architecture" && <><p>This grants only this exact architecture artifact, not experiments or product features.</p><button type="button" disabled={disabled || !current || !id || !sha || !detail} onClick={()=>onAuthorize(command("program.approve_architecture",{program_id:subject.id,revision:revisionHash,artifact_id:id,artifact_kind:kind,artifact_sha:sha,subject_sha:sha}))}>Review separate exact architecture grant</button></>}
  {kind === "experiment_contracts" && records(payload?.experiments).map((experiment,index) => {
   const experimentID=text(experiment.id);
   const subjectSHA=text(records(detail?.experiment_subjects).find(item=>item.id===experimentID)?.subject_sha);
   return <section key={`${experimentID}:${index}`} aria-label={`Experiment ${experimentID}`}>
    <h4>Separate experiment grant: {experimentID}</h4><Presentation value={experiment}/>
    <p>This grant covers only this exact experiment contract, not other experiments, architecture or product features.</p>
    {subjectSHA && <p>Server-bound experiment contract SHA: {subjectSHA}</p>}
    <button type="button" disabled={disabled || !current || !id || !sha || !/^[a-fA-F0-9]{64}$/.test(subjectSHA)} onClick={()=>onAuthorize(command("program.approve_experiment",{program_id:subject.id,revision:revisionHash,artifact_id:id,artifact_kind:kind,artifact_sha:sha,subject_sha:subjectSHA}))}>Review separate exact experiment {experimentID} grant</button>
    {!subjectSHA && <p>The server has not supplied this experiment's canonical contract hash. No grant can be requested.</p>}
   </section>;
  })}
  {(kind === "architecture" || kind === "experiment_contracts") && !current && <p>Historical revision: no grant is offered. Refresh for the current revision.</p>}
  {kind === "questions" && current && records(payload?.questions).map(question=><AnswerForm key={`${revisionHash}:${id}:${text(question.id)}`} draftKey={JSON.stringify(["program",subject.id,revisionHash,id,sha,question.id])} answerDrafts={answerDrafts} title={text(question.text)} binding={{artifact,question,revision_hash:revisionHash}} disabled={disabled} supporting onSubmit={async (answer,refs)=>{
   if(!id || !sha || !text(question.id)) throw new Error("The server did not supply a complete question binding.");
   await onAnswer(command("program.answer",{program_id:subject.id,revision_hash:revisionHash,question_artifact_id:id,question_artifact_sha:sha,question_id:text(question.id),text:answer,supporting_refs:refs}));
  }}/>) }
  {kind === "implementation_contracts" && records(payload?.contracts).map((contract,index)=>{const feature=object(contract.feature),featureID=text(feature?.id);return featureID ? <div key={`${featureID}:${index}`}><Presentation value={contract}/><p>This is a proposal, not delivered software or an execution grant. Open the feature to inspect its own current revision and separate approval.</p><button type="button" onClick={()=>onFeature(featureID)}>Open separate feature {featureID}</button></div>:null;})}
 </article>;
}
