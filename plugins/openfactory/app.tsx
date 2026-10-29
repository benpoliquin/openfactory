import { useEffect, useMemo, useRef, useState } from "react";
import { definePluginApp, experimental_useRpcQuery, useBbNavigate } from "@get-bb/plugin-sdk/app";
import { rpcContract, type FeatureStatus, type Snapshot } from "./contract";
import { currentAttempt, featureStage, prLink, stageColumn, stages, waiting } from "./presentation";
import "./openfactory.css";
import { Programs } from "./programs";
import { OpenFactoryIntake } from "./intake";

const label = (value: string) => value.replaceAll("_", " ");
const date = (value: string | number) => new Date(value).toLocaleString();
function duration(ms: number) {
  const seconds = Math.max(0, Math.floor(ms / 1000));
  if (seconds < 60) return `${seconds}s`;
  if (seconds < 3600) return `${Math.floor(seconds / 60)}m ${seconds % 60}s`;
  return `${Math.floor(seconds / 3600)}h ${Math.floor((seconds % 3600) / 60)}m`;
}

function OpenFactoryBoard() {
  const query = experimental_useRpcQuery({ contract: rpcContract, method: "snapshot", input: null });
  const queryRef = useRef(query);
  queryRef.current = query;
  const [clock, setClock] = useState(Date.now());
  const [search, setSearch] = useState("");
  const [project, setProject] = useState<string | null>(null);
  useEffect(() => {
    let stopped = false;
    let timer: number;
    const poll = async () => {
      if (!document.hidden && !queryRef.current.isFetching) await queryRef.current.refetch();
      if (!stopped) timer = window.setTimeout(poll, 4000);
    };
    timer = window.setTimeout(poll, 4000);
    const ticker = setInterval(() => setClock(Date.now()), 1000);
    return () => { stopped = true; clearTimeout(timer); clearInterval(ticker); };
  }, []);
  const data = query.data;
  const projects = useMemo(() => [...new Set(data?.features.map(status => status.feature.project) ?? [])].sort(), [data]);
  const features = useMemo(() => (data?.features ?? []).filter(({ feature }) => !feature.discovery_step_id && !feature.intake_step_id && (project === null || feature.project === project) && `${feature.id} ${feature.title} ${feature.project}`.toLowerCase().includes(search.toLowerCase())), [data, project, search]);
  const stale = data && clock - Date.parse(data.observed_at) > 15000;
  return <main className="openfactory-board">
    <header className="openfactory-top"><div><h1>OpenFactory</h1><p>Live feature flow <span aria-hidden="true">/</span> Read-only</p></div>
      <div className="openfactory-status"><span className={`openfactory-pill ${data?.paused ? "openfactory-wait" : ""}`}>{data ? data.paused ? "Paused" : "Live" : "Connecting"}</span>
      <button type="button" aria-disabled={query.isFetching} onClick={() => { if (!queryRef.current.isFetching) void queryRef.current.refetch(); }}>Refresh</button></div>
    </header>
    <div className="openfactory-toolbar"><label>Search<input type="search" placeholder="Feature title or ID" value={search} onChange={event => setSearch(event.target.value)} /></label>
      <label>Project<select value={project === null ? "all" : `project:${project}`} onChange={event => setProject(event.target.value === "all" ? null : event.target.value.slice(8))}><option value="all">All projects</option>{projects.map(name => <option key={name} value={`project:${name}`}>{name || "Unassigned"}</option>)}</select></label>
      <span className="openfactory-count">{features.length} feature{features.length === 1 ? "" : "s"}</span></div>
    <div className="openfactory-health">
      <div aria-live="polite" aria-atomic="true">
        {query.isLoading ? <p>Loading OpenFactory snapshot…</p> : query.error ? <p className="openfactory-error">{query.error.message} {data ? "Showing the last successful snapshot." : "Use Refresh to try again."}</p> : data && <p>{stale ? <strong className="openfactory-error">Stale data</strong> : data.paused ? "OpenFactory paused" : "OpenFactory live"}</p>}
      </div>
      {data && <><p>Snapshot <time dateTime={data.observed_at}>{date(data.observed_at)}</time> · refreshes every 4 seconds</p>
        <p>Global lanes {data.limits.global.active}/{data.limits.global.limit} · Calls {data.calls.used}/{data.calls.limit} ({data.calls.day})</p>
        <p>Internal discovery steps: {data.features.filter(status => status.feature.discovery_step_id).length} · included in global and project lane capacity above, hidden from the product board.</p>
        <p>{data.intake_step_count === undefined ? "Active internal intake count is unavailable from this controller" : `Active internal intake drafting steps: ${data.intake_step_count}`} · private intake rows are hidden; their capacity and calls are included in the same global and project limits above.</p>
        <p>{data.limits.projects.map(limit => `${limit.name}: ${limit.active}/${limit.limit} lanes`).join(" · ")}</p></>}
    </div>
    {data && <><section aria-labelledby="openfactory-lanes-title"><h2 id="openfactory-lanes-title">Feature lanes</h2><div className="openfactory-scroll" tabIndex={0} role="region" aria-label="Feature stage board, scroll horizontally">
      <table className="openfactory-lanes"><thead><tr><th scope="col">Feature</th>{stages.map(stage => <th scope="col" key={stage}>{label(stage)}</th>)}</tr></thead>
      <tbody>{features.map(status => <FeatureLane key={status.feature.id} status={status} />)}</tbody></table>
    </div>{features.length === 0 && <p className="openfactory-empty">{data.features.length ? "No features match these filters." : "No features yet. New work will appear here automatically."}</p>}</section>
    <History features={features} snapshot={data} /></>}
  </main>;
}
function FeatureLane({ status }: { status: FeatureStatus }) {
  const navigate = useBbNavigate();
  const { feature } = status;
  const attempt = currentAttempt(status);
  const stage = featureStage(status);
  const column = stageColumn(stage);
  const href = prLink(status.receipt);
  return <tr><th scope="row"><div className="openfactory-feature"><span className="openfactory-id">{feature.id} · {feature.project || "Unassigned"}</span><strong>{feature.title}</strong>
    <div className="openfactory-tags"><span className="openfactory-pill">{label(feature.state)}</span><span className="openfactory-pill">Stage: {label(stage)}</span>{attempt && attempt.stage !== stage && <span className="openfactory-pill">Attempt stage: {label(attempt.stage)}</span>}{attempt && <span className="openfactory-pill">Generation {attempt.candidate_generation}</span>}</div>
    {feature.reason && <p>{feature.reason}</p>}
    {status.wait && <p className="openfactory-wait">Waiting: {label(status.wait.code)}{status.wait.detail ? ` · ${status.wait.detail}` : ""}{status.wait.owner_attempt_id ? ` · owner ${status.wait.owner_attempt_id}` : ""}{status.wait.resource ? ` · ${status.wait.resource}` : ""}{status.wait.feature_id ? ` · feature ${status.wait.feature_id}` : ""}</p>}
    {status.dependencies.length > 0 && <div className="openfactory-dependencies">Depends on {status.dependencies.map((dependency, index) => <span key={dependency.id}>{index > 0 ? ", " : ""}<span title={dependency.state}>{dependency.satisfied ? "✓" : "○"} {dependency.id} ({label(dependency.state)})</span></span>)}</div>}
    {status.integration.state && <p>Integration: {label(status.integration.state)}{status.integration.position > 0 ? ` · queue position ${status.integration.position}` : ""}{status.integration.owner_attempt_id ? ` · owner ${status.integration.owner_attempt_id}` : ""}</p>}
    <details className="openfactory-details"><summary>Details & sessions ({status.sessions.length})</summary><dl><dt>Completion target</dt><dd>{feature.completion_target || "Not specified"}</dd><dt>Receipt</dt><dd>{status.receipt || "None"}</dd><dt>Created</dt><dd>{date(feature.created_at)}</dd><dt>Updated</dt><dd>{date(feature.updated_at)}</dd>
      {attempt && <><dt>Attempt</dt><dd>{attempt.id} · {attempt.active ? "active" : "inactive"}</dd><dt>Candidate</dt><dd>{attempt.candidate_sha || "Not recorded"} · generation {attempt.candidate_generation}</dd></>}
      {status.integration.resources.length > 0 && <><dt>Resources</dt><dd>{status.integration.resources.join(", ")}</dd></>}</dl>
      <div className="openfactory-session-list">{status.sessions.map((session, index) => session.thread_id && <button type="button" key={`${session.dispatch_key}-${index}`} onClick={() => navigate.toThread(session.thread_id)} title={`Open thread ${session.thread_id}`}>{label(session.stage)} · {session.attempt_id || session.thread_id}</button>)}</div>
      {status.checks.length > 0 && <><strong>Recorded checks</strong><ul>{status.checks.map((check, index) => <li key={`${check.attempt_id}-${check.name}-${index}`} title={check.detail}>{check.name}: {label(check.state)} · generation {check.candidate_generation}{check.attempt_id !== attempt?.id || check.candidate_generation !== attempt?.candidate_generation || check.candidate_sha !== attempt?.candidate_sha ? " (previous candidate)" : ""}</li>)}</ul></>}
      {status.current_evidence.length > 0 && <><strong>Current candidate evidence</strong><ul>{status.current_evidence.map(evidence => <li key={evidence.id} title={evidence.detail}>{label(evidence.kind)}: {evidence.passed ? "passed" : "failed"} · generation {evidence.candidate_generation}</li>)}</ul></>}
      {href && <a href={href} target="_blank" rel="noopener noreferrer">Open GitHub pull request ↗</a>}
      {feature.completion_target === "pull_request" && status.receipt && !href && <p>Pull request link unavailable: not a valid GitHub HTTPS pull request URL.</p>}
    </details></div></th>
    {stages.map(value => <td key={value} className={column === value ? `openfactory-current ${status.wait || waiting(stage, feature.state) ? "openfactory-wait" : ""}` : ""}>{column === value && <div className="openfactory-stage"><span className="openfactory-dot" aria-hidden="true" /><strong>{label(stage)}</strong><small>{label(feature.state)}</small>{attempt && <small>Generation {attempt.candidate_generation}</small>}</div>}</td>)}
  </tr>;
}
function History({ features, snapshot }: { features: FeatureStatus[]; snapshot: Snapshot }) {
  const end = Date.parse(snapshot.observed_at);
  const start = features.reduce((earliest, status) => status.timeline.reduce((value, segment) => {
    const time = Date.parse(segment.started_at);
    return Number.isFinite(time) && time <= end ? Math.min(value, time) : value;
  }, earliest), end);
  const span = Math.max(1, end - start);
  const position = (time: number) => `${Math.max(0, Math.min(100, (time - start) / span * 100))}%`;
  return <details className="openfactory-history"><summary>Recorded history <span>Shared time axis · no forecasts</span></summary><p>Actual timestamps only. Striped segments are waiting; solid segments are work. Open intervals end at the snapshot and are labeled ongoing. Parallel features share the same axis.</p>
    <div className="openfactory-scroll" tabIndex={0} role="region" aria-label="Feature history timeline, scroll horizontally"><div className="openfactory-gantt"><div className="openfactory-axis"><span>Feature / stage</span><div>{[0, .25, .5, .75, 1].map(fraction => <time key={fraction} style={{ left: `${fraction * 100}%`, transform: `translateX(-${fraction * 100}%)` }} dateTime={new Date(start + (end - start) * fraction).toISOString()}>{date(start + (end - start) * fraction)}</time>)}</div></div>
    {features.map(status => <section className="openfactory-history-feature" key={status.feature.id} aria-label={`${status.feature.title} history`}><h3>{status.feature.id} · {status.feature.title}</h3>{status.timeline.length === 0 && <p>No recorded stage intervals.</p>}{status.timeline.map((segment, index) => {
      const from = Date.parse(segment.started_at);
      const until = segment.finished_at ? Date.parse(segment.finished_at) : end;
      const valid = from <= end && until >= from;
      const description = `${label(segment.stage)} · ${label(segment.state)}${segment.attempt_id ? ` · attempt ${segment.attempt_id}` : ""} · ${date(from)} → ${segment.finished_at ? date(until) : "snapshot (ongoing)"} · ${duration(until - from)}`;
      return <div className="openfactory-history-row" key={index}><div><strong>{label(segment.stage)}</strong><small>{label(segment.state)} · {valid ? duration(until - from) : "timestamp unavailable"}{!segment.finished_at ? " · ongoing" : ""}</small></div><div className="openfactory-track">{valid && <span tabIndex={0} role="img" aria-label={description} title={description} className={`openfactory-bar ${waiting(segment.stage, segment.state) ? "openfactory-wait" : ""}`} style={{ left: position(from), width: `${Math.max(.25, (Math.min(until, end) - from) / span * 100)}%` }}><span>{label(segment.stage)}{!segment.finished_at ? " · ongoing" : ""}</span></span>}</div></div>;
    })}</section>)}
    </div></div></details>;
}
export default definePluginApp(app => {
  app.slots.navPanel({ id: "board", title: "OpenFactory", icon: "Columns2", path: "board", component: OpenFactoryBoard });
  app.slots.navPanel({ id: "programs", title: "Programs", icon: "Columns2", path: "programs", component: Programs });
  app.slots.navPanel({ id: "intake", title: "OpenFactory Intake", icon: "Columns2", path: "intake", component: OpenFactoryIntake });
});
