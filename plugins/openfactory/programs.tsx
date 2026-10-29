import { useState } from "react";
import { experimental_useRpcQuery, useBbNavigate } from "@get-bb/plugin-sdk/app";
import { rpcContract } from "./contract";
import type { z } from "zod";
import { artifactOutputLabel, programPageInput } from "./program-contract";

type Section = z.infer<typeof programPageInput>["section"];
const sections: Section[] = ["artifacts","questions","experiments","evidence","features","events"];
const label = (value: string) => value.replaceAll("_"," ");
// React text nodes only: captured HTML, Markdown and URLs are never executed.
export function Presentation({value}: {value: unknown}) {
 if (value === null || value === undefined) return <span>None</span>;
 if (typeof value !== "object") return <span style={{whiteSpace:"pre-wrap",overflowWrap:"anywhere"}}>{String(value)}</span>;
 if (Array.isArray(value)) return value.length ? <ol>{value.map((item,index) => <li key={index}><Presentation value={item}/></li>)}</ol> : <span>None recorded</span>;
 return <dl>{Object.entries(value).map(([key,item]) => <div key={key}><dt><strong>{label(key)}</strong></dt><dd><Presentation value={item}/></dd></div>)}</dl>;
}
export function Programs() {
 const [id,setID] = useState<string | null>(null);
 const [after,setAfter] = useState<string | undefined>();
 const [project,setProject] = useState("");
 const query = experimental_useRpcQuery({contract:rpcContract,method:"programList",input:{limit:25,...(after ? {after}:{}),...(project ? {project}:{})}});
 if (id) return <ProgramDetail key={id} id={id} back={() => setID(null)}/>;
 return <main className="openfactory-board"><h1>Discovery Programs</h1><p>Read-only · discovery proposals are not delivered software. Version-bound answers and separate authority grants are available through authenticated OpenFactory Intake or the trusted CLI.</p><label>Project<input value={project} onChange={event => {setProject(event.target.value);setAfter(undefined);}}/></label>
 {query.error && <p role="alert">{query.error.message}</p>}{query.isLoading && <p>Loading Programs…</p>}
 <ul>{query.data?.items.map(program => <li key={program.id}><button type="button" onClick={() => setID(program.id)}>{program.title} · {program.id}</button><p>{program.project} · revision {program.current_revision} · iteration {program.iteration} · {label(program.phase)} · {label(program.state)}</p>{program.wait && <Presentation value={program.wait}/>}</li>)}</ul>
 {after && <button type="button" onClick={() => setAfter(undefined)}>First page</button>}{query.data?.next_cursor && <button type="button" disabled={query.isFetching} onClick={() => setAfter(query.data!.next_cursor)}>Next Programs</button>}
 </main>;
}
function ProgramDetail({id,back}: {id:string;back:()=>void}) {
 const navigate = useBbNavigate();
 const query = experimental_useRpcQuery({contract:rpcContract,method:"programStatus",input:{id}});
 const [section,setSection] = useState<Section>("artifacts");
 const [revision,setRevision] = useState<number | undefined>();
 return <main className="openfactory-board"><button type="button" onClick={back}>Programs</button><h1>{query.data?.program.title ?? id}</h1>{query.error && <p role="alert">{query.error.message}</p>}
 <button type="button" onClick={() => navigate.toPluginPanel("intake", { subPath: `subjects/program/${encodeURIComponent(id)}` })}>Open authenticated Intake controls</button>
 {query.data && <><h2>Revision, phase and waits</h2><Presentation value={query.data.program}/><h2>Lifetime budget and revision limits</h2><Presentation value={query.data.budget}/><Presentation value={query.data.revision}/><h2>Recorded exact grant history (not current authority)</h2><p>Grants may belong to superseded revisions or policies. Invalidations or expired evidence can invalidate authority; inspect the current wait and invalidations below.</p><Presentation value={query.data.grants}/><h2>Invalidations / stale evidence gates</h2><Presentation value={query.data.invalidations}/><details><summary>Internal steps ({query.data.steps.length})</summary><Presentation value={query.data.steps}/></details>{query.data.truncated && <p>Status lists are truncated at 500 entries.</p>}</>}
 <label>Artifact revision (blank = current)<input type="number" min="1" value={revision ?? ""} onChange={event => setRevision(event.target.value ? Number(event.target.value):undefined)}/></label>
 <nav aria-label="Program evidence sections">{sections.map(value => <button type="button" key={value} aria-pressed={section === value} onClick={() => setSection(value)}>{label(value)}</button>)}</nav>
 <ProgramPage key={`${id}:${section}:${revision}`} id={id} section={section} revision={revision}/></main>;
}
function ProgramPage({id,section,revision}: {id:string;section:Section;revision?:number}) {
 const [after,setAfter] = useState<string | undefined>();
 const [artifact,setArtifact] = useState<string | null>(null);
 const query = experimental_useRpcQuery({contract:rpcContract,method:"programPage",input:{id,section,query:{limit:25,...(after ? {after}:{}),...(revision ? {revision}:{})}}});
 return <section><h2>{label(section)}</h2>{section === "evidence" && <p>Verified citations establish quote occurrence in the captured source, not publisher truth or semantic entailment. Search leads alone are not evidence.</p>}{section === "experiments" && <p>Conclusive rejected observations may falsify a hypothesis. Infrastructure failure, missing prerequisites and inconclusive output are not falsification. Recorded grants bind exact contract hashes but are history, not automatically effective permission: superseded revisions or policies, invalidations and expired evidence can invalidate authority. Inspect current waits and invalidations.</p>}{section === "features" && <p>Published proposals remain unapproved until separately approved; invalidated Program evidence blocks stale gates.</p>}
 {query.error && <p role="alert">{query.error.message} Return to the first page if the snapshot changed.</p>}{query.isLoading && <p>Loading…</p>}
 {query.data?.items.map((item,index) => {
  const state = section === "artifacts" && "id" in item && query.data && "output_states" in query.data ? query.data.output_states.find(state => state.artifact_id === item.id) : undefined;
  return <article key={index}>{state && <p>{artifactOutputLabel(state)} · Output position is not evidence authority.</p>}<Presentation value={item}/>{section === "artifacts" && "id" in item && <button type="button" onClick={() => setArtifact(String(item.id))}>Read artifact {String(item.id)}</button>}</article>;
 })}
 {after && <button type="button" onClick={() => setAfter(undefined)}>First page</button>}{query.data?.next_cursor && <button type="button" disabled={query.isFetching} onClick={() => setAfter(query.data!.next_cursor)}>Next {label(section)}</button>}
 {artifact && <Artifact key={artifact} id={id} artifact_id={artifact} close={() => setArtifact(null)}/>}</section>;
}
function Artifact({id,artifact_id,close}: {id:string;artifact_id:string;close:()=>void}) {
 const query = experimental_useRpcQuery({contract:rpcContract,method:"programArtifact",input:{id,artifact_id}});
 return <section aria-label="Artifact detail"><h2>Canonical artifact detail</h2><p>content_sha identifies the original immutable artifact, not this sanitized presentation. Supersession retains prior evidence; evaluator thread identity identifies the independent evaluation session. Current stage output describes output position, not evidence authority.</p><button type="button" onClick={close}>Close artifact</button>{query.error && <p role="alert">{query.error.message}</p>}{query.data && <p>{artifactOutputLabel(query.data.output_state)}</p>}<Presentation value={query.data}/></section>;
}
