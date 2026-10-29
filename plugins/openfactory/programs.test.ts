import assert from "node:assert/strict";
import test from "node:test";
import { programHandlers, fetchProgram } from "./program-server.ts";
import { programListSchema, programQuery, artifactDetailSchema, sectionSchemas, artifactOutputLabel } from "./program-contract.ts";
import { ExperimentContract, ExperimentCommandReceipt, ExperimentObservationSpec, ExperimentReceipt, SourceCitation, Program, Feature } from "./program-schemas.ts";
import { cancelledSocket, configureDashboard, dashboardFixture, sendJSON } from "./http-fixture.ts";
const now = "2026-10-28T12:00:00Z";
const outputState = (artifact_id: string, producer_step_kind: string) => ({artifact_id,producer_step_kind,producer_subject_id:"",superseded_by:[] as string[],current_stage_output:true,historical_revision:false});
const program = {id:"program-1",project:"demo",title:"Discovery",current_revision:2,state:"waiting",phase:"experiments",iteration:1,created_at:now,updated_at:now};
const commandReceipt = {
 argv:["python","measure.py"],binary_sha:"binary",exit_code:0,termination:"exited",
 stdout_sha:"stdout",stderr_sha:"stderr",stdout_bytes:64,stderr_bytes:0,
 stdout_observed_bytes:128,stderr_observed_bytes:0,stdout_truncated:true,stderr_truncated:false,
};
const receipt = {
 run_id:"run",request_sha:"request",program_id:"program-1",revision:2,step_id:"step",attempt_id:"attempt",
 contract_sha:"contract",bundle_manifest_sha:"bundle",state:"completed",started_at:now,finished_at:now,
 commands:[commandReceipt],artifacts:{manifest_sha:"manifest",files:[{path:"results.json",content_sha:"sha",bytes:32}]},
 observations:[{name:"latency",type:"number",unit:"ms",value:200,source_path:"results.json",source_sha:"sha"}],
 accepted:false,conclusive:true,declared_image_identity:"python",attestation_scope:"binary",
 execution_outcome:"completed",observation_outcome:"complete",hypothesis_outcome:"falsified",
};
const experimentContract = {
 id:"experiment-1",question_refs:["question-1"],decision_refs:[],hypothesis:"Latency stays below 100 ms",
 falsification:"Observed latency is at least 100 ms",
 environment:{profile:"python",image_identity:"python",tools:["python"],prerequisites:[],dataset_refs:[]},
 bundle_manifest_sha:"bundle",bundle_files:["measure.py"],
 commands:[{profile:"python",args:["measure.py"]}],
 observations:[{name:"latency",type:"number",unit:"ms",output_path:"results.json",json_pointer:"/run/latency_ms"}],
 acceptance:[{observation:"latency",operator:"lt",value:100,unit:"ms"}],
 limits:{wall_seconds:30,max_output_bytes:64,max_artifact_bytes:1024,max_artifact_files:1,resource_description:"One measurement"},
 external_authority:{network:false,accounts:[],credentials:[],hardware:[],spend:0},limitations:[],
};

test("Program navigation fetches one bounded page, preserving opaque cursor and canonical filters",async t => {
 const calls: URL[] = [];
 const endpoint = await dashboardFixture(t, (request,response) => {
  const url = new URL(request.url!,endpoint);calls.push(url);
  assert.equal(request.method,"GET");
  assert.equal(request.headers.host,`127.0.0.1:${new URL(endpoint).port}`);
  assert.equal(request.headers.authorization,undefined);assert.equal(request.headers.cookie,undefined);
  assert.equal(request.headers.accept,"application/json");
  response.setHeader("Set-Cookie","private=must-not-return");
  sendJSON(response,{items:[program],next_cursor:"opaque+snapshot/2",snapshot:"snapshot"});
 });
 configureDashboard(t,endpoint);
 const handlers = programHandlers(() => new AbortController().signal);
 const first = await handlers.programList({project:"demo",limit:25});
 assert.equal(calls.length,1,"must not flatten all Program pages into a poll");
 assert.equal(calls[0].pathname,"/api/programs");assert.equal(calls[0].searchParams.get("limit"),"25");
 await handlers.programList({project:"demo",limit:25,after:first.next_cursor});
 assert.equal(calls[1].searchParams.get("after"),"opaque+snapshot/2");
 assert.throws(() => programQuery.parse({limit:501}));
 await assert.rejects(handlers.programArtifact({id:"../status",artifact_id:"artifact"}));
});

test("Program bridge rejects redirects, non JSON, streamed oversize and stale snapshot HTTP errors",async t => {
 let count = 0;
 const endpoint = await dashboardFixture(t, (_request,response) => {
  count++;
  if (count === 1) { response.writeHead(400);response.end("stale"); }
  else if (count === 2) { response.setHeader("Content-Type","text/html");response.end("html"); }
  else if (count === 3) {
   response.setHeader("Content-Type","application/json");
   const chunk = Buffer.alloc(64*1024,32);let remaining = 257;
   const pump = () => {
    while (remaining > 0 && !response.destroyed) {
     remaining--;
     if (!response.write(chunk)) {response.once("drain",pump);return;}
    }
    if (!response.destroyed) response.end();
   };
   pump();
  } else { response.writeHead(302,{Location:"/redirect-target"});response.end(); }
 });
 const signal = new AbortController().signal;
 await assert.rejects(fetchProgram(signal,"",{},programListSchema,endpoint),/HTTP 400/);
 await assert.rejects(fetchProgram(signal,"",{},programListSchema,endpoint),/non-JSON/);
 await assert.rejects(fetchProgram(signal,"",{},programListSchema,endpoint),/16 MiB/);
 await assert.rejects(fetchProgram(signal,"",{},programListSchema,endpoint),/HTTP 302/);
 assert.equal(count,4,"redirect target must never be requested");
 await assert.rejects(fetchProgram(signal,"",{},programListSchema,"https://user:secret@example.org/api/status"),/without credentials/);
});

test("Program socket responses retain fatal UTF-8, canonical schema and declared size validation",async t => {
 let count = 0;
 const endpoint = await dashboardFixture(t,(_request,response) => {
  response.setHeader("Content-Type","application/json");
  count++;
  if (count === 1) sendJSON(response,{items:[{...program,token:"not-canonical"}],snapshot:"snapshot"});
  else if (count === 2) response.end(Buffer.from([0x22,0xc3,0x28,0x22]));
  else {response.setHeader("Content-Length",String(17*1024*1024));response.flushHeaders();}
 });
 const signal = new AbortController().signal;
 await assert.rejects(fetchProgram(signal,"",{},programListSchema,endpoint),/invalid canonical/);
 await assert.rejects(fetchProgram(signal,"",{},programListSchema,endpoint),/encoded data|encoding/i);
 await assert.rejects(fetchProgram(signal,"",{},programListSchema,endpoint),/16 MiB/);
 for (const invalid of ["ftp://localhost/api/status",`${endpoint}#fragment`,endpoint.replace("/api/status","/api/private")]) {
  await assert.rejects(fetchProgram(signal,"",{},programListSchema,invalid),/HTTP\(S\).*without credentials/);
 }
});

test("Program abort closes its actual streaming response socket", {timeout:3000},async t => {
 let closed: Promise<void> | undefined;
 let started!: () => void;
 const ready = new Promise<void>(resolve => {started = resolve;});
 const endpoint = await dashboardFixture(t,(request,response) => {
  closed = cancelledSocket(request.socket, controller.signal);
  response.writeHead(200,{"Content-Type":"application/json"});
  response.write('{"items":[');started();
 });
 const controller = new AbortController();
 const rejected = assert.rejects(fetchProgram(controller.signal,"",{},programListSchema,endpoint),/abort/i);
 await ready;
 controller.abort();
 await rejected;
 assert.ok(closed);
 await closed;
});

test("strict Program presentation rejects aliases, unknown secrets and host paths",() => {
 assert.equal(Program.safeParse(program).success,true);
 assert.equal(Program.safeParse({...program,currentRevision:2}).success,false);
 assert.equal(Program.safeParse({...program,token:"secret"}).success,false);
 assert.equal(Program.safeParse({...program,title:"Read /Users/operator/private/input"}).success,false);
 assert.equal(Program.safeParse({...program,title:"<script>alert('text only')</script>"}).success,true);
});

test("artifact drilldown validates canonical payload and retains evaluation findings and supersession",() => {
 const artifact = {id:"evaluation",program_id:"program-1",revision:2,iteration:1,kind:"evaluation",schema_version:1,content_sha:"sha",input_manifest_sha:"manifest",producer_step_id:"step",producer_attempt_id:"attempt",producer_thread_id:"independent",supersedes_id:"old",created_at:now};
 const payload = {evaluated_manifest_sha:"manifest",findings:[{id:"f1",severity:"blocking",detail:"Missing durability evidence",refs:[]}],coverage:[],verdict:"revise",required_changes:["Measure recovery"]};
 const detail = artifactDetailSchema.parse({artifact,output_state:outputState(artifact.id,"result_evaluation"),payload,snapshot:"snapshot"});
 assert.equal(detail.artifact.supersedes_id,"old");assert.deepEqual(detail.payload,payload);
 assert.equal(artifactDetailSchema.safeParse({artifact,output_state:outputState(artifact.id,"result_evaluation"),payload:{...payload,prompt:"private"},snapshot:"snapshot"}).success,false);
});

test("receipt presentation preserves actual falsification versus infrastructure failure",() => {
 const base = receipt;
 assert.equal(ExperimentReceipt.parse(base).hypothesis_outcome,"falsified");
 const infra = ExperimentReceipt.parse({...base,state:"failed",conclusive:false,failure_kind:"infrastructure",execution_outcome:"infrastructure_failure",observation_outcome:"missing",hypothesis_outcome:"unknown",observations:[]});
 assert.equal(infra.conclusive,false);assert.equal(infra.hypothesis_outcome,"unknown");assert.equal(infra.failure_kind,"infrastructure");
});

test("implementation contract presentation retains behavior, acceptance and dependencies without policy metadata",() => {
 const feature = {id:"proposal",title:"Deduplicate receipts",project:"demo",state:"proposed",completion_target:"integrated",created_at:now,updated_at:now,kind:"feature",risk:"medium",priority:1,depends_on:["storage"],checks:["unit"],acceptance:[{id:"ac1",scenario:"Duplicate delivery",expected:"One committed receipt"}],body:"Persist the deduplication key atomically.",revision:"revision",reason:"Awaiting approval",attempts:0,policy_hash:"private-policy",token:"private-token"};
 const shown = Feature.parse(feature);
 assert.equal(shown.body,feature.body);
 assert.deepEqual(shown.acceptance,feature.acceptance);
 assert.deepEqual(shown.depends_on,["storage"]);
 assert.deepEqual(shown.checks,["unit"]);
 assert.equal("policy_hash" in shown,false);
 assert.equal("token" in shown,false);
});

test("accepted artifacts retain more than one page of nested requirements",() => {
 const artifact = {id:"requirements",program_id:"program-1",revision:2,iteration:1,kind:"requirements",schema_version:1,content_sha:"sha",input_manifest_sha:"manifest",producer_step_id:"step",producer_attempt_id:"attempt",producer_thread_id:"author",created_at:now};
 const requirements = Array.from({length:501},(_,index) => ({id:`r${index}`,behavior:"Record durable receipt",rationale:"Prevent duplicate commits",constraints:[],acceptance:[],priority:"must",claim_refs:[],question_refs:[]}));
 const shown = artifactDetailSchema.parse({artifact,output_state:outputState(artifact.id,"requirements"),payload:{requirements},snapshot:"snapshot"});
 assert.deepEqual(shown.payload,{requirements});
});

test("experiments page accepts canonical contracts and nonempty command receipts through the bridge",async t => {
 const artifact = {id:"experiments",program_id:"program-1",revision:2,iteration:1,kind:"experiment_contracts",schema_version:1,content_sha:"sha",input_manifest_sha:"manifest",producer_step_id:"step",producer_attempt_id:"attempt",producer_thread_id:"author",created_at:now};
 const item = {artifact,contract:experimentContract,contract_sha:"contract",grants:[],receipts:[receipt]};
 const endpoint = await dashboardFixture(t,(request,response) => {
  assert.equal(new URL(request.url!,endpoint).pathname,"/api/programs/program-1/experiments");
  sendJSON(response,{items:[item],snapshot:"snapshot",revision_hash:"revision"});
 });
 configureDashboard(t,endpoint);
 const shown = await programHandlers(() => new AbortController().signal).programPage({id:"program-1",section:"experiments",query:{}});
 assert.deepEqual(shown.items,[item]);
 const detail = artifactDetailSchema.parse({artifact,output_state:outputState(artifact.id,"experiment_contracts"),payload:{experiments:[experimentContract]},snapshot:"snapshot"});
 assert.deepEqual(detail.payload,{experiments:[experimentContract]});
 const {bundle_files: _bundleFiles,...missingBundleFiles} = experimentContract;
 assert.equal(ExperimentContract.safeParse(missingBundleFiles).success,false);
 for (const field of ["stdout_observed_bytes","stderr_observed_bytes"] as const) {
  const {[field]: _observed,...missingObserved} = commandReceipt;
  assert.equal(ExperimentCommandReceipt.safeParse(missingObserved).success,false);
  for (const invalid of [-1,0.5]) assert.equal(ExperimentCommandReceipt.safeParse({...commandReceipt,[field]:invalid}).success,false);
 }
});

test("exact observation and citation JSON pointers survive without weakening private text guards",() => {
 const observation = experimentContract.observations[0];
 const citation = {source_id:"source",text_sha:"sha",quote:"Public measurement",json_pointer:"/users/0/login"};
 const artifact = {id:"claims",program_id:"program-1",revision:2,iteration:1,kind:"claims",schema_version:1,content_sha:"sha",input_manifest_sha:"manifest",producer_step_id:"step",producer_attempt_id:"attempt",producer_thread_id:"author",created_at:now};
 const claims = [{id:"claim",statement:"Public measurement is reproducible",classification:"fact",citations:[citation],inference_premises:[],scope:[],limitations:[]}];
 assert.deepEqual(artifactDetailSchema.parse({artifact,output_state:outputState(artifact.id,"claims"),payload:{claims},snapshot:"snapshot"}).payload,{claims});
 for (const pointer of ["","/run/latency_ms","/users/0/login","/a~1b/~0key","/ space /Bearer abc","/"+"a".repeat(4095)]) {
  assert.equal(ExperimentObservationSpec.parse({...observation,json_pointer:pointer}).json_pointer,pointer);
  assert.equal(SourceCitation.parse({...citation,json_pointer:pointer}).json_pointer,pointer);
 }
 for (const pointer of ["/"+"a".repeat(4096),"/run/\u0000latency","/users/\nlogin","/users/\u007flogin","/users/\u0085login"]) {
  assert.equal(ExperimentObservationSpec.safeParse({...observation,json_pointer:pointer}).success,false);
  assert.equal(SourceCitation.safeParse({...citation,json_pointer:pointer}).success,false);
 }
 for (const privateText of ["/run/private/input","/Users/operator/private/input","Bearer private-token","-----BEGIN RSA PRIVATE KEY-----"]) {
  assert.equal(ExperimentObservationSpec.safeParse({...observation,output_path:privateText}).success,false);
  assert.equal(SourceCitation.safeParse({...citation,quote:privateText}).success,false);
  assert.equal(ExperimentContract.safeParse({...experimentContract,hypothesis:privateText}).success,false);
  assert.equal(ExperimentContract.safeParse({...experimentContract,bundle_files:[privateText]}).success,false);
  assert.equal(ExperimentCommandReceipt.safeParse({...commandReceipt,argv:[privateText]}).success,false);
  assert.equal(ExperimentReceipt.safeParse({...receipt,observations:[{...receipt.observations[0],value:privateText}]}).success,false);
 }
});

test("artifact history preserves separate evaluation stages and explicit output states",async t => {
 const kinds = ["plan_evaluation","result_evaluation","contract_evaluation"];
 const items = kinds.map((kind,index) => ({id:`evaluation-${index}`,program_id:"program-1",revision:2,iteration:1,kind:"evaluation",schema_version:1,content_sha:`sha-${index}`,input_manifest_sha:"manifest",producer_step_id:`step-${kind}`,producer_attempt_id:"attempt",producer_thread_id:"evaluator",created_at:now}));
 const output_states = kinds.map((kind,index) => ({...outputState(items[index].id,kind),producer_subject_id:`subject-${index}`}));
 output_states[0] = {...output_states[0],current_stage_output:false,superseded_by:["new-plan-evaluation"]};
 output_states[2] = {...output_states[2],current_stage_output:false,historical_revision:true};
 const page = {items,output_states,snapshot:"snapshot",next_cursor:"older-row"};
 const endpoint = await dashboardFixture(t,(_request,response) => sendJSON(response,page));
 configureDashboard(t,endpoint);
 const shown = await programHandlers(() => new AbortController().signal).programPage({id:"program-1",section:"artifacts",query:{}});
 assert.deepEqual(shown,page,"retain canonical artifact items and companion states in server order");
 const labels = output_states.map(artifactOutputLabel);
 assert.match(labels[0],/Historical \/ non-current output.*plan_evaluation.*subject-0.*superseded by new-plan-evaluation/);
 assert.match(labels[1],/Current stage output.*result_evaluation.*subject-1/);
 assert.match(labels[2],/Historical revision.*contract_evaluation.*subject-2/);
 assert.equal(new Set(labels).size,3,"evaluation payload kind must not replace the producer stage");
 assert.equal(sectionSchemas.artifacts.safeParse({items,snapshot:"snapshot"}).success,false);
 const payload = {evaluated_manifest_sha:"manifest",findings:[],coverage:[],verdict:"accept",required_changes:[]};
 assert.equal(artifactDetailSchema.safeParse({artifact:items[0],payload,snapshot:"snapshot"}).success,false);
 for (const [index,state] of output_states.entries()) {
  const detail = artifactDetailSchema.parse({artifact:items[index],output_state:state,payload,snapshot:"snapshot"});
  assert.equal(artifactOutputLabel(detail.output_state),labels[index]);
 }
});
