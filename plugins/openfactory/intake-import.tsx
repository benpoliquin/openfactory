import {useRef,useState} from "react";
import type {IntakeClient} from "./intake-client";

type CanonicalEdit = {source:string;route:"feature"|"plan"|"program";format:"feature_markdown"|"program_json";bundle_manifest_sha?:string};
export function IntakeImport({requestID,contentVersion,client,maxBytes,availableBundles,edit,onEdit,onChanged,onError}: {
 requestID:string;
 contentVersion:number;
 client:IntakeClient;
 maxBytes:number;
 availableBundles:string[];
 edit?:CanonicalEdit;
 onEdit:(edit:CanonicalEdit)=>void;
 onChanged:()=>void;
 onError:(error:unknown)=>void;
}) {
 const [local,setLocal]=useState<CanonicalEdit>(()=>edit??{source:"",route:"feature",format:"feature_markdown"});
 const [busy,setBusy]=useState(false);
 const [error,setError]=useState("");
 const [notice,setNotice]=useState("");
 const locked=useRef(false);
 const current=edit??local;
 function update(value:CanonicalEdit) {setLocal(value);onEdit(value);setNotice("");}
 function fail(cause:unknown) {setError(cause instanceof Error?cause.message:"Canonical source could not be saved. Refresh before retrying.");onError(cause);}
 function validate(source:string) {
  if (!Number.isSafeInteger(maxBytes)||maxBytes<1) throw new Error("The server has not supplied a valid canonical source byte limit.");
  if (!source.trim()) throw new Error("Enter or import nonempty canonical source.");
  if (new TextEncoder().encode(source).byteLength>maxBytes) throw new Error(`Canonical source exceeds the ${maxBytes}-byte limit.`);
 }
 async function importFile(file:File) {
  if (locked.current)return;
  locked.current=true;setBusy(true);setError("");setNotice("");
  try {
   if (!Number.isSafeInteger(maxBytes)||maxBytes<1||file.size>maxBytes) throw new Error(`Canonical source file exceeds the permitted byte limit (${maxBytes}).`);
   const bytes=await file.arrayBuffer();
   if (bytes.byteLength>maxBytes) throw new Error(`Canonical source exceeds the ${maxBytes}-byte limit.`);
   const source=new TextDecoder("utf-8",{fatal:true,ignoreBOM:true}).decode(bytes);
   validate(source);update({...current,source});setNotice("Exact UTF-8 source imported into memory. Save it below to create an unapproved draft.");
  } catch(cause) {fail(cause);}
  finally {locked.current=false;setBusy(false);}
 }
 async function save() {
  if (locked.current)return;
  locked.current=true;setBusy(true);setError("");setNotice("");
  try {
   validate(current.source);
   const seed=current.bundle_manifest_sha;
   if(seed && (current.route!=="program"||!/^[a-f0-9]{64}$/.test(seed)||!availableBundles.includes(seed))) throw new Error("Select an available finalized Program seed bundle before saving.");
   await client.command({command_id:crypto.randomUUID(),action:"draft.replace",request_id:requestID,expected_content_version:contentVersion,payload:{route:current.route,format:current.format,canonical_source:current.source,...(current.route==="program"&&seed?{bundle_manifest_sha:seed}:{})}});
   setNotice("Canonical source saved as a new unapproved draft. Review the server's immutable draft before requesting any separate approval.");onChanged();
  } catch(cause) {fail(cause);}
  finally {locked.current=false;setBusy(false);}
 }
 return <form onSubmit={event=>{event.preventDefault();void save();}}>
  <h3>Import an existing canonical contract</h3>
  <p>This saves exact source as a new unapproved draft. It does not call a model, approve work, or grant execution authority.</p>
  <label>Contract route<select value={current.route} disabled={busy} onChange={event=>{
   const route=event.target.value;
   if(route!=="feature"&&route!=="plan"&&route!=="program")return;
   update({source:current.source,route,format:route==="program"?"program_json":"feature_markdown"});
  }}><option value="feature">Feature · Markdown</option><option value="plan">Plan · Markdown</option><option value="program">Program · JSON</option></select></label>
  {current.route==="program"&&<><label>Exact Program seed bundle<select value={current.bundle_manifest_sha??""} disabled={busy} onChange={event=>{
   const seed=event.target.value;
   update({source:current.source,route:current.route,format:current.format,...(seed?{bundle_manifest_sha:seed}:{})});
  }}><option value="">No seed bundle</option>{Array.from(new Set(availableBundles)).filter(sha=>/^[a-f0-9]{64}$/.test(sha)).map(sha=><option key={sha} value={sha}>{sha}</option>)}</select></label>
  <p>This binds only the exact finalized Program seed bundle to the imported contract. Uploading or importing it is not consent to share it with a model.</p></>}
  <label>Import UTF-8 source file<input type="file" disabled={busy} accept={current.route==="program"?".json,application/json":".md,.markdown,text/markdown,text/plain"} onChange={event=>{
   const file=event.target.files?.[0];event.target.value="";if(file)void importFile(file);
  }}/></label>
  <label>Exact canonical source<textarea value={current.source} disabled={busy} spellCheck={false} onChange={event=>update({...current,source:event.target.value})}/></label>
  <p>Maximum {maxBytes} UTF-8 bytes. Source remains in this page's memory until saved; no transcript or source is stored in browser storage.</p>
  {error&&<p role="alert">{error}</p>}
  {notice&&<p role="status">{notice}</p>}
  <button type="submit" disabled={busy||!current.source.trim()}>{busy?"Processing canonical source…":"Save as new unapproved draft"}</button>
 </form>;
}
