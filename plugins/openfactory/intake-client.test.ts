import assert from "node:assert/strict";
import test from "node:test";
import {createHash} from "node:crypto";
import {IntakeClient, IntakeError, takePairingFragment} from "./intake-client.ts";
import {browserJournal} from "./intake-journal.ts";
import {commandSchema, reviewMatches, reviewSchema, stableJSON, type Command, type Receipt} from "./intake-contract.ts";

const id = "baf9b067-9ca4-4cbd-a5b4-7fc6584a5e6b";
const other = "baf9b067-9ca4-4cbd-a5b4-7fc6584a5e6c";
const session = {operator_id:"operator",label:"Operator",credential_id:"credential",project_scope:["demo"],csrf_token:"csrf-secret",installation_id:"installation",auth_generation:"generation"};
const scope={origin:"http://localhost",operator_id:session.operator_id,installation_id:session.installation_id,auth_generation:session.auth_generation};
function authenticatedClient(environment:ConstructorParameters<typeof IntakeClient>[0]) {
  const client=new IntakeClient(environment);client.session={...session};return client;
}
function storageFixture() {
  const entries = new Map<string,string>();
  return {entries, getItem(key:string) {return entries.get(key)??null;}, setItem(key:string,value:string) {entries.set(key,value);}, removeItem(key:string) {entries.delete(key);}};
}
function json(value:unknown,status=200) {return new Response(JSON.stringify(value),{status,headers:{"Content-Type":"application/json"}});}
function command():Command {return {command_id:id,action:"draft.replace",request_id:"request",expected_content_version:3,payload:{route:"feature",format:"feature_markdown",canonical_source:"private exact draft"}};}
function receipt(commandID=id) {return {operator_id:"operator",command_id:commandID,digest:"digest",action:"draft.replace",state:"committed",result:{request_id:"request"}};}
const credentials:Pick<CredentialsContainer,"create"|"get"> = {async create() {throw new Error("Unexpected registration");},async get() {throw new Error("Unexpected authorization");}};

test("unknown authority and direct execution commands are rejected before transport or persistence",async () => {
  const storage=storageFixture(); let calls=0;
  const client=new IntakeClient({storage,credentials,fetch:async()=>{calls++;throw new Error("Unexpected transport");}});
  await assert.rejects(client.command({...command(),approved:true} as Command));
  await assert.rejects(client.command({...command(),payload:{...command().payload,actor:"operator"}}));
  await assert.rejects(client.command({command_id:id,action:"intake.approve_queue",request_id:"request",expected_content_version:3,payload:{draft_revision:2,source_sha:"sha",domain_revision:"revision"}}),error=>error instanceof IntakeError&&error.code==="human_verification_required");
  assert.equal(calls,0); assert.equal(storage.entries.size,0);
});

test("authenticated commands use same-origin CSRF and never copy authentication into storage",async () => {
  const storage=storageFixture();
  const client=authenticatedClient({storage,credentials,fetch:async(input,init)=>{
    assert.equal(input,"/openfactory-intake/v1/commands");
    assert.equal(init?.method,"POST"); assert.equal(init?.credentials,"same-origin"); assert.equal(init?.mode,"same-origin"); assert.equal(init?.redirect,"error"); assert.equal(init?.cache,"no-store");
    const headers=new Headers(init?.headers); assert.equal(headers.get("X-OpenFactory-CSRF"),session.csrf_token); assert.equal(headers.get("Cookie"),null); assert.equal(headers.get("Authorization"),null);
    assert.deepEqual(JSON.parse(String(init?.body)),command());
    assert.deepEqual(browserJournal(storage).bind(scope).pending.list(),[id],"only command IDs and nonsecret scope are durable before dispatch");
    return json(receipt());
  }});
  client.session=session; await client.command(command());
  assert.deepEqual(client.pendingIDs(),[]);assert.equal([...storage.entries.values()].join("").includes("csrf-secret"),false);
});

test("lost response retains only ID and retries the original typed command before receipt recovery",async () => {
  const storage=storageFixture(); const sent:Command[]=[]; const order:string[]=[]; let posts=0;let lookups=0;
  const client=authenticatedClient({storage,credentials,fetch:async(input,init)=>{
    if(init?.method==="POST") {order.push("POST");posts++;sent.push(commandSchema.parse(JSON.parse(String(init.body))));throw new TypeError("connection closed after commit");}
    order.push("GET");lookups++;assert.equal(input,`/openfactory-intake/v1/commands/${id}`);
    return lookups===1?json({error:{code:"not_found",message:"No receipt",retryable:false}},404):json(receipt());
  }});
  const original=command(); await assert.rejects(client.command(original),error=>error instanceof IntakeError&&error.code==="connection_uncertain");
  original.payload.canonical_source="edited after uncertain send"; original.expected_content_version=4;
  await assert.rejects(client.retry(id));
  assert.equal(posts,2); assert.deepEqual(sent[1],sent[0]);
  assert.equal(sent[1].payload.canonical_source,"private exact draft");assert.equal(sent[1].expected_content_version,3);
  assert.deepEqual(order,["POST","GET","POST"],"retry must look up the receipt before replaying the original typed command");
  assert.deepEqual(browserJournal(storage).bind(scope).pending.list(),[id]);
  assert.equal([...storage.entries.values()].join("").includes("private"),false);
  const recovered=await client.recover();assert.equal(recovered[0].receipt?.command_id,id);assert.deepEqual(client.pendingIDs(),[]);
  const reconnected=authenticatedClient({storage,credentials,fetch:async()=>json(receipt())});
  await assert.rejects(reconnected.retry(id),error=>error instanceof IntakeError&&error.code==="receipt_pending");
});

test("401 drops the in-memory session while preserving uncertain command IDs",async () => {
  const storage=storageFixture();browserJournal(storage).bind(scope).pending.add(id);
  const client=authenticatedClient({storage,credentials,fetch:async()=>json({error:{code:"session_expired",message:"Session expired",retryable:false}},401)});
  client.session=session;
  const recovered=await client.recover(); assert.equal(recovered[0].error?.code,"session_expired");assert.equal(client.session,null);assert.deepEqual(client.pendingIDs(),[id]);
  client.session=session;await assert.rejects(client.command(command()));assert.equal(client.session,null);assert.deepEqual(client.pendingIDs(),[id]);
});

test("mismatched receipts and reviews cannot release pending IDs or authorize different bodies",async () => {
  const storage=storageFixture();const pending=browserJournal(storage).bind(scope).pending;pending.add(id);pending.add(other);
  const client=authenticatedClient({storage,credentials,fetch:async()=>json(receipt("baf9b067-9ca4-4cbd-a5b4-7fc6584a5e6d"))});
  const results=await client.recover();assert.equal(results.length,2);assert.ok(results.every(item=>item.error?.code==="invalid_contract"));assert.deepEqual(client.pendingIDs(),[id,other]);
  const approval=commandSchema.parse({command_id:id,action:"intake.approve_queue",request_id:"request",expected_content_version:3,payload:{draft_revision:2,source_sha:"sha",domain_revision:"revision"}});
  const sha="a".repeat(64),created="2026-10-28T12:00:00Z";
  const review=reviewSchema.parse({id:"review",command:{...approval,expected_content_version:4},claims:{version:1,installation_id:"installation",operator_id:"operator",credential_id:"credential",action:approval.action,subject_kind:"intake",subject_id:"request",subject_revision:sha,canonical_payload_sha:sha,review_manifest_sha:sha,policy_sha:sha,evidence:[],bundle_manifest_shas:[],command_id:id,challenge_id:"challenge",issued_at:created,expires_at:"2026-10-28T12:02:00Z",request_id:"request",content_version:4,draft_revision:2},claims_sha:sha,claims_canonical:"{}",manifest_canonical:"{}",canonical_payload:"{}",command_canonical:"{}",canonical_source:"# Exact source",findings:null,created_at:created});
  assert.equal(reviewMatches(review,approval),false);
  const reviewClient=authenticatedClient({storage:storageFixture(),credentials,fetch:async(input,init)=>{
    if(init?.method==="GET"){assert.equal(input,"/openfactory-intake/v1/projects");return json({items:[],policy_sha:sha});}
    assert.equal(input,"/openfactory-intake/v1/reviews");return json(review);
  }});
  await assert.rejects(reviewClient.review(approval),error=>error instanceof IntakeError&&error.code==="invalid_contract");
});

test("pairing fragment is removed synchronously even when secret validation fails",() => {
  const replaced:string[]=[];
  const history={state:{keep:true},replaceState(data:unknown,_unused:string,url?:string|URL|null) {assert.deepEqual(data,{keep:true});replaced.push(String(url));}};
  assert.throws(()=>takePairingFragment({hash:"#pairing=malformed&private=secret",pathname:"/plugins/openfactory/intake",search:"?project=demo"},history),IntakeError);
  assert.deepEqual(replaced,["/plugins/openfactory/intake?project=demo"]);
  assert.equal(takePairingFragment({hash:`#pairing=${"A".repeat(43)}`,pathname:"/intake",search:""},history),"A".repeat(43));
  assert.equal(takePairingFragment({hash:"",pathname:"/intake",search:""},history),"");assert.equal(replaced.length,2);
});

test("durability failure prevents network dispatch",async () => {
  let calls=0;
  const client=authenticatedClient({credentials,storage:{getItem:()=>null,setItem(){throw new Error("quota");},removeItem(){}},fetch:async()=>{calls++;return json(receipt());}});
  await assert.rejects(client.command(command()));assert.equal(calls,0);
});

const savedRequest={id:"openfactory-intake-request-1",operator_id:"operator",project:"demo",state:"collecting",content_version:1,latest_draft_revision:0,creation_policy_sha:"policy",event_sequence:1};
const disclosure={context_sha:"a".repeat(64),policy_sha:"b".repeat(64),model_provider_ids:["provider"],private_material_to_models:true};
function saveCommand():Command {return {command_id:id,action:"request.create",payload:{project:"demo",text:"private saved text",client_message_id:other}};}
function saveReceipt() {return {...receipt(),action:"request.create",result:savedRequest};}

test("lost save receipt recovers the exact send lifecycle before drafting with the stable second ID",async()=>{
  const storage=storageFixture();let savePosts=0;let draftPosts=0;
  const first=authenticatedClient({storage,credentials,fetch:async(_input,init)=>{
    const body=JSON.parse(String(init?.body)) as Command;
    assert.equal(body.action,"request.create");assert.equal("disclosure" in body.payload,false);
    savePosts++;throw new TypeError("save committed, response lost");
  }});
  await assert.rejects(first.saveForSend(saveCommand()));
  const pending=first.sendLifecycle();assert.equal(pending?.phase,"saving");assert.ok(pending?.draft_command_id);
  const draftID=pending!.draft_command_id;assert.notEqual(draftID,id);
  const reconnect=authenticatedClient({storage,credentials,fetch:async(input,init)=>{
    if(init?.method==="GET") {assert.equal(input,`/openfactory-intake/v1/commands/${id}`);return json(saveReceipt());}
    const body=JSON.parse(String(init?.body)) as Command;draftPosts++;
    assert.deepEqual(body,{command_id:draftID,action:"intake.draft",request_id:savedRequest.id,expected_content_version:1,payload:{disclosure}});
    return json({...receipt(draftID),action:"intake.draft",result:{queued:true}});
  }});
  const recovered=await reconnect.recover();assert.equal(recovered[0].receipt?.command_id,id);
  assert.deepEqual(reconnect.sendLifecycle(),{save_command_id:id,draft_command_id:draftID,phase:"saved",request_id:savedRequest.id,content_version:1});
  await reconnect.draftSaved(savedRequest.id,1,disclosure);
  assert.equal(savePosts,1);assert.equal(draftPosts,1);assert.equal(reconnect.sendLifecycle(),null);assert.deepEqual(reconnect.pendingIDs(),[]);
  assert.deepEqual(browserJournal(storage).drafts(scope),[]);
});

test("lost draft receipt recovers without queueing a second draft",async()=>{
  const storage=storageFixture();let draftPosts=0;
  const first=authenticatedClient({storage,credentials,fetch:async(_input,init)=>{
    const body=JSON.parse(String(init?.body)) as Command;
    if(body.action==="request.create")return json(saveReceipt());
    assert.equal(body.action,"intake.draft");draftPosts++;throw new TypeError("draft committed, response lost");
  }});
  await first.saveForSend(saveCommand());const draftID=first.sendLifecycle()!.draft_command_id;
  await assert.rejects(first.draftSaved(savedRequest.id,1,disclosure));
  assert.equal(first.sendLifecycle()?.phase,"drafting");
  const reconnect=authenticatedClient({storage,credentials,fetch:async(input,init)=>{
    assert.equal(init?.method,"GET");assert.equal(input,`/openfactory-intake/v1/commands/${draftID}`);
    return json({...receipt(draftID),action:"intake.draft",result:{queued:true}});
  }});
  const recovered=await reconnect.recover();assert.equal(recovered[0].receipt?.command_id,draftID);
  assert.equal(draftPosts,1);assert.equal(reconnect.sendLifecycle(),null);assert.deepEqual(reconnect.pendingIDs(),[]);
});

test("Save alone persists text without disclosure, draft dispatch or send lifecycle",async()=>{
  const storage=storageFixture();const actions:string[]=[];
  const client=authenticatedClient({storage,credentials,fetch:async(_input,init)=>{
    const body=JSON.parse(String(init?.body)) as Command;actions.push(body.action);
    assert.equal("disclosure" in body.payload,false);return json(saveReceipt());
  }});
  await client.command(saveCommand());
  assert.deepEqual(actions,["request.create"]);assert.equal(client.sendLifecycle(),null);assert.deepEqual(browserJournal(storage).drafts(scope),[]);
});

function turn(sequence:number) {return {request_id:"request",sequence,role:sequence%2?"user":"assistant",text:`Exact turn ${sequence}: café 🦜 尾\nsecond line`,audit:{kind:"verified_operator"},created_at:"2026-10-28T12:00:00Z"};}
test("completeTurns reads every sequence page with exact Unicode text and no mutation calls",async()=>{
  const calls:number[]=[];const expected=Array.from({length:113},(_,index)=>turn(index+1));
  const controller=new AbortController();
  const client=new IntakeClient({storage:storageFixture(),credentials,fetch:async(input,init)=>{
    assert.equal(init?.method,"GET");assert.equal(init?.body,undefined);assert.equal(init?.signal,controller.signal);
    const url=new URL(String(input),"https://openfactory.example");
    assert.equal(url.pathname,"/openfactory-intake/v1/requests/request/turns");assert.equal(url.searchParams.get("limit"),"50");
    const after=Number(url.searchParams.get("after"));calls.push(after);
    const items=expected.slice(after,after+50);
    return json({items,...(after+50<expected.length?{next:after+50}:{})});
  }});
  assert.deepEqual(await client.completeTurns("request",controller.signal),expected);
  assert.deepEqual(calls,[0,50,100]);assert.deepEqual(client.pendingIDs(),[]);
});

test("completeTurns rejects repeated cursors instead of returning partial transcript",async()=>{
  const calls:number[]=[];
  const client=new IntakeClient({storage:storageFixture(),credentials,fetch:async(input,init)=>{
    assert.equal(init?.method,"GET");assert.equal(init?.body,undefined);
    const after=Number(new URL(String(input),"https://openfactory.example").searchParams.get("after"));calls.push(after);
    return json({items:[turn(after+1)],next:5});
  }});
  await assert.rejects(client.completeTurns("request"),error=>error instanceof IntakeError&&error.code==="invalid_contract");
  assert.deepEqual(calls,[0,5]);
});

test("completeTurns rejects more than 256 turns without silently trimming context",async()=>{
  let calls=0;
  const client=new IntakeClient({storage:storageFixture(),credentials,fetch:async(input,init)=>{
    assert.equal(init?.method,"GET");assert.equal(init?.body,undefined);calls++;
    const after=Number(new URL(String(input),"https://openfactory.example").searchParams.get("after"));
    const count=after===250?7:50;
    return json({items:Array.from({length:count},(_,index)=>turn(after+index+1)),...(after<250?{next:after+50}:{})});
  }});
  await assert.rejects(client.completeTurns("request"),error=>error instanceof IntakeError&&error.code==="input_limit");
  assert.equal(calls,6);assert.deepEqual(client.pendingIDs(),[]);
});

function draftCommand(commandID=id):Command {return {command_id:commandID,action:"intake.draft",request_id:savedRequest.id,expected_content_version:7,payload:{disclosure:structuredClone(disclosure)}};}
const notFound=()=>json({error:{code:"not_found",message:"No committed receipt",retryable:false}},404);

test("same-generation reload preserves canonical draft authority and idempotency after receipt 404",async()=>{
  const storage=storageFixture(),original=draftCommand(),expected=commandSchema.parse(structuredClone(original));
  const expectedDigest=createHash("sha256").update(stableJSON(expected)).digest("hex");
  const first=authenticatedClient({storage,credentials,fetch:async(_input,init)=>{assert.deepEqual(commandSchema.parse(JSON.parse(String(init?.body))),expected);throw new TypeError("lost before commit");}});
  await assert.rejects(first.command(original));original.payload.disclosure={...disclosure,model_provider_ids:["different-current-provider"]};original.expected_content_version=99;
  const order:string[]=[],committed=new Map<string,{digest:string;receipt:Receipt}>();let queuedJobs=0;
  const reloaded=authenticatedClient({storage,credentials,fetch:async(_input,init)=>{
    order.push(init?.method??"");
    if(init?.method==="GET"){const saved=committed.get(id);return saved?json(saved.receipt):notFound();}
    const body=commandSchema.parse(JSON.parse(String(init?.body)));
    const digest=createHash("sha256").update(stableJSON(body)).digest("hex"),saved=committed.get(body.command_id);
    if(saved){return saved.digest===digest?json(saved.receipt):json({error:{code:"idempotency_conflict",message:"Command ID already binds different canonical data",retryable:false}},409);}
    assert.deepEqual(body,expected);assert.equal(digest,expectedDigest);
    queuedJobs++;const result={...receipt(body.command_id),action:"intake.draft",digest};committed.set(body.command_id,{digest,receipt:result});return json(result);
  }});
  assert.equal(reloaded.canRetry(id),true);await reloaded.retry(id);
  assert.deepEqual(order,["GET","POST"]);assert.equal(queuedJobs,1);assert.deepEqual(reloaded.pendingIDs(),[]);
  await reloaded.command(expected);assert.equal(queuedJobs,1,"same ID and canonical command cannot queue another job");
  await assert.rejects(reloaded.command(original),error=>error instanceof IntakeError&&error.code==="idempotency_conflict");
  assert.equal(queuedJobs,1,"mutated version or disclosure cannot replace committed authority");
  const durable=[...storage.entries.values()].join("");assert.equal(durable.includes("csrf-secret"),false);assert.equal(durable.includes("private saved text"),false);
});

test("generation change marks prior draft unresolved and cannot replay it; explicit fresh draft uses new ID",async()=>{
  const storage=storageFixture();const requests:Command[]=[];
  const client=authenticatedClient({storage,credentials,fetch:async(_input,init)=>{
    if(init?.method==="GET")return notFound();
    const body=JSON.parse(String(init?.body)) as Command;requests.push(body);
    if(requests.length===1)throw new TypeError("lost");return json({...receipt(body.command_id),action:"intake.draft"});
  }});
  await assert.rejects(client.command(draftCommand()));client.session={...session,auth_generation:"restored"};
  assert.equal(client.canRetry(id),false);
  await assert.rejects(client.retry(id),error=>error instanceof IntakeError&&error.code==="receipt_pending");
  const recovered=await client.recover();assert.equal(recovered[0].scope_changed,true);assert.equal(recovered[0].error?.code,"auth_generation_changed");assert.equal(requests.length,1);
  await client.draftSaved(savedRequest.id,8,{...disclosure,context_sha:"c".repeat(64)});
  assert.equal(requests.length,2);assert.notEqual(requests[1].command_id,id);assert.equal(requests[1].expected_content_version,8);
  assert.deepEqual(browserJournal(storage).bind(scope).pending.list(),[id],"old uncertain command remains unresolved");
});

test("different operator, installation or origin never hydrate prior draft bodies",async()=>{
  const storage=storageFixture();
  const old=authenticatedClient({storage,credentials,fetch:async()=>{throw new TypeError("lost");}});
  await assert.rejects(old.command(draftCommand()));
  for(const identity of [
    {session:{...session,operator_id:"different-operator"},origin:"http://localhost"},
    {session:{...session,installation_id:"different-installation"},origin:"http://localhost"},
    {session:{...session},origin:"https://different.example"},
  ]) {
    let posts=0;
    const fresh=authenticatedClient({storage,credentials,origin:identity.origin,fetch:async(_input,init)=>{if(init?.method==="POST")posts++;return notFound();}});
    fresh.session=identity.session;assert.equal(fresh.canRetry(id),false);
    await assert.rejects(fresh.retry(id));await fresh.recover();assert.equal(posts,0);
  }
  assert.deepEqual(browserJournal(storage).bind(scope).pending.list(),[id]);
});

test("current authentication may look up a committed old-generation receipt without replay",async()=>{
  const storage=storageFixture();
  const old=authenticatedClient({storage,credentials,fetch:async()=>{throw new TypeError("lost");}});
  await assert.rejects(old.command(draftCommand()));
  let lookups=0;
  const fresh=authenticatedClient({storage,credentials,fetch:async(input,init)=>{
    assert.equal(init?.method,"GET");assert.equal(input,`/openfactory-intake/v1/commands/${id}`);
    assert.equal(new Headers(init?.headers).get("Cookie"),null);lookups++;return json({...receipt(),action:"intake.draft"});
  }});
  fresh.session={...session,auth_generation:"restored"};
  const results=await fresh.recover();assert.equal(lookups,1);assert.equal(results[0].scope_changed,true);assert.equal(results[0].receipt?.command_id,id);
  assert.deepEqual(browserJournal(storage).bind(scope).pending.list(),[]);assert.deepEqual(browserJournal(storage).drafts(scope),[]);
});

test("scope change during an interrupted response cannot settle an old command in a new namespace",async()=>{
  const storage=storageFixture();let resolveResponse!:(value:Response)=>void;
  const response=new Promise<Response>(resolve=>{resolveResponse=resolve;});
  const client=authenticatedClient({storage,credentials,fetch:async()=>response});
  const pending=client.command(draftCommand());
  client.session={...session,auth_generation:"restored"};
  assert.deepEqual(client.pendingIDs(),[]);
  resolveResponse(json({...receipt(),action:"intake.draft"}));
  await assert.rejects(pending,error=>error instanceof IntakeError&&error.code==="auth_generation_changed");
  assert.deepEqual(browserJournal(storage).bind(scope).pending.list(),[id]);
  assert.deepEqual(client.pendingIDs(),[]);
});

test("lost private save envelope remains unresolved after explicit new intention; neither old ID is reused",async()=>{
  const storage=storageFixture();const old=authenticatedClient({storage,credentials,fetch:async()=>{throw new TypeError("save response lost");}});
  await assert.rejects(old.saveForSend(saveCommand()));
  const oldDraftID=old.sendLifecycle()!.draft_command_id;
  const posts:Command[]=[];
  const reconnect=authenticatedClient({storage,credentials,fetch:async(_input,init)=>{
    if(init?.method==="GET")return notFound();
    const body=JSON.parse(String(init?.body)) as Command;posts.push(body);
    return json({...saveReceipt(),command_id:body.command_id});
  }});
  const recovery=await reconnect.recover();
  assert.equal(recovery[0].id,id);assert.equal(recovery[0].unreplayable,true);
  assert.equal(reconnect.isUnreplayable(id),true);assert.equal(reconnect.canRetry(id),false);
  reconnect.beginNewIntention();assert.equal(reconnect.sendLifecycle(),null);
  assert.deepEqual(reconnect.pendingIDs(),[id]);assert.equal(posts.length,0);
  const freshID="baf9b067-9ca4-4cbd-a5b4-7fc6584a5e6d";
  await reconnect.saveForSend({...saveCommand(),command_id:freshID,payload:{...saveCommand().payload,text:"Explicit new private intention"}});
  assert.equal(posts.length,1);assert.equal(posts[0].command_id,freshID);
  const fresh=reconnect.sendLifecycle()!;assert.equal(fresh.save_command_id,freshID);
  assert.notEqual(fresh.draft_command_id,oldDraftID);assert.notEqual(fresh.draft_command_id,id);
  assert.deepEqual(reconnect.pendingIDs(),[id],"old uncertain save remains unresolved, never silently discarded");
});

test("explicit new intention cannot discard retryable draft metadata or an in-flight draft lifecycle",async()=>{
  const storage=storageFixture();
  const client=authenticatedClient({storage,credentials,fetch:async()=>{throw new TypeError("draft response lost");}});
  await assert.rejects(client.command(draftCommand()));
  assert.equal(client.canRetry(id),true);
  assert.throws(()=>client.beginNewIntention(),error=>error instanceof IntakeError&&error.code==="receipt_pending");
  assert.deepEqual(client.pendingIDs(),[id]);
  const storageWithLifecycle=storageFixture();
  const lifecycle=authenticatedClient({storage:storageWithLifecycle,credentials,fetch:async(_input,init)=>{
    const body=JSON.parse(String(init?.body)) as Command;
    if(body.action==="request.create")return json(saveReceipt());
    throw new TypeError("draft response lost");
  }});
  await lifecycle.saveForSend(saveCommand());await assert.rejects(lifecycle.draftSaved(savedRequest.id,1,disclosure));
  assert.equal(lifecycle.sendLifecycle()?.phase,"drafting");
  assert.throws(()=>lifecycle.beginNewIntention(),error=>error instanceof IntakeError&&error.code==="receipt_pending");
  assert.equal(lifecycle.sendLifecycle()?.phase,"drafting");
});
