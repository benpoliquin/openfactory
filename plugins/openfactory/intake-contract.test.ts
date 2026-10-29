import assert from "node:assert/strict";
import test from "node:test";
import {commandSchema, reviewSchema, reviewMatches, questionBinding, sourceDiff, draftSchema, requestSchema, linkSchema, draftDisposition, type Command, type Question, type Draft} from "./intake-contract.ts";

const id="baf9b067-9ca4-4cbd-a5b4-7fc6584a5e6b";
const other="baf9b067-9ca4-4cbd-a5b4-7fc6584a5e6c";
const approval:Command={command_id:id,action:"intake.approve_queue",request_id:"request",expected_content_version:7,payload:{draft_revision:3,source_sha:"source-sha",domain_revision:"domain-revision"}};
function reviewFixture() {
  const sha="a".repeat(64),created="2026-10-28T12:00:00Z";
  return {id:"review",command:approval,claims:{version:1,installation_id:"installation",operator_id:"operator",credential_id:"credential",action:approval.action,subject_kind:"intake",subject_id:"request",subject_revision:sha,canonical_payload_sha:sha,review_manifest_sha:sha,policy_sha:sha,evidence:[],bundle_manifest_shas:[],command_id:id,challenge_id:"challenge",issued_at:created,expires_at:"2026-10-28T12:02:00Z",request_id:"request",content_version:7,draft_revision:3},claims_sha:sha,claims_canonical:"{}",manifest_canonical:"{}",canonical_payload:"{}",command_canonical:"{}",canonical_source:"# Exact source",findings:null,created_at:created};
}

test("review comparison binds exact command identity, content version, action and payload digests",()=>{
  const review=reviewSchema.parse(reviewFixture());
  assert.equal(reviewMatches(review,approval),true);
  for(const changed of [
    {...approval,command_id:other},
    {...approval,request_id:"other-request"},
    {...approval,request_id:undefined},
    {...approval,action:"intake.retry"},
    {...approval,expected_content_version:8},
    {...approval,payload:{...approval.payload,source_sha:"different-sha"}},
    {...approval,payload:{...approval.payload,domain_revision:"different-domain"}},
    {...approval,payload:{...approval.payload,draft_revision:4}},
  ]) assert.equal(reviewMatches(review,changed),false);
  const reordered:Command={...approval,payload:{domain_revision:"domain-revision",source_sha:"source-sha",draft_revision:3}};
  assert.equal(reviewMatches(review,reordered),true);
  assert.equal(reviewSchema.safeParse({...review,claims:{...review.claims,event_sequence:999}}).success,false,"progress cannot be injected into strict authority claims");
  assert.equal(reviewSchema.safeParse({...review,claims:{...review.claims,version:2}}).success,false);
  for(const changedClaims of [
    {...review.claims,action:"intake.retry"},
    {...review.claims,command_id:other},
    {...review.claims,request_id:"different-request"},
    {...review.claims,content_version:8},
    {...review.claims,draft_revision:4},
  ]) {
    const changed=reviewSchema.parse({...review,claims:changedClaims});
    assert.equal(reviewMatches(changed,approval),false,"claim authority must match the reviewed command");
  }
});

test("command schemas reject extra authority at both envelope and payload boundaries",()=>{
  assert.equal(commandSchema.safeParse(approval).success,true);
  for(const [key,value] of Object.entries({actor:"operator",approved:true,shell:"run",model:"provider",policy:{allow:true},policy_override:true,operator_id:"spoof"})) {
    assert.equal(commandSchema.safeParse({...approval,[key]:value}).success,false,key);
    assert.equal(commandSchema.safeParse({...approval,payload:{...approval.payload,[key]:value}}).success,false,`payload.${key}`);
  }
  assert.equal(commandSchema.safeParse({...approval,command_id:"not-a-uuid"}).success,false);
});

test("artifact authority requires both containing artifact and exact subject hashes",()=>{
  for(const action of ["program.approve_architecture","program.approve_experiment"]) {
    const payload={program_id:"program",revision:"revision",artifact_id:"artifact",artifact_kind:"experiment_contracts",artifact_sha:"artifact-hash",subject_sha:"subject-hash"};
    assert.equal(commandSchema.safeParse({command_id:id,action,payload}).success,true);
    const {subject_sha:subjectSHA,...missingSubject}=payload;
    assert.equal(subjectSHA,"subject-hash");
    assert.equal(commandSchema.safeParse({command_id:id,action,payload:missingSubject}).success,false);
    assert.equal(commandSchema.safeParse({command_id:id,action,payload:{...payload,subject_sha:""}}).success,false);
  }
});

test("question binding retains the exact content, input and output fence without unrelated state",()=>{
  const question:Question={id:"question",request_id:"request",content_version:7,input_manifest_sha:"input-sha",output_sha:"output-sha",text:"Which target?",consequence:"Changes deployment",answer_type:"text",choices:null,answered_by_sequence:12};
  assert.deepEqual(questionBinding(question),{question_id:"question",content_version:7,input_manifest_sha:"input-sha",output_sha:"output-sha"});
  for(const changed of [{...question,content_version:8},{...question,input_manifest_sha:"new-input"},{...question,output_sha:"new-output"}]) assert.notDeepEqual(questionBinding(changed),questionBinding(question));
  assert.deepEqual(questionBinding({...question,text:"Display changed",answered_by_sequence:20,event_sequence:999}),questionBinding(question));
});

test("source diff retains exact Unicode additions, deletions and all changed lines",()=>{
  assert.equal(sourceDiff("header\nold café 🦜\n尾\nfooter","header\nnew café 🦜\n新\nfooter"),"Lines 2–3 → 2–3\n− old café 🦜\n− 尾\n+ new café 🦜\n+ 新");
  assert.equal(sourceDiff("same","same"),"No source changes.");
  const before=Array.from({length:600},(_,i)=>`old-${i}-é`).join("\n");
  const after=Array.from({length:600},(_,i)=>`new-${i}-🦜`).join("\n");
  const diff=sourceDiff(before,after);
  const lines=diff.split("\n");assert.equal(lines.length,1201);
  assert.equal(lines[1],"− old-0-é");assert.equal(lines[600],"− old-599-é");assert.equal(lines[601],"+ new-0-🦜");assert.equal(lines[1200],"+ new-599-🦜");
  assert.equal(sourceDiff("a\nc","a\nb\nc"),"Lines 2–1 → 2–2\n+ b");
});

test("canonical imports bind only valid Program seeds and exact route-format pairs",()=>{
  const base={command_id:id,action:"draft.replace",request_id:"request",expected_content_version:7};
  const seed="abcdef0123456789".repeat(4);
  assert.equal(commandSchema.safeParse({...base,payload:{route:"program",format:"program_json",canonical_source:"{}",bundle_manifest_sha:seed}}).success,true);
  assert.equal(commandSchema.safeParse({...base,payload:{route:"program",format:"program_json",canonical_source:"{}"}}).success,true);
  for(const route of ["feature","plan"]) {
    assert.equal(commandSchema.safeParse({...base,payload:{route,format:"feature_markdown",canonical_source:"# Exact source"}}).success,true);
    assert.equal(commandSchema.safeParse({...base,payload:{route,format:"feature_markdown",canonical_source:"# Exact source",bundle_manifest_sha:seed}}).success,false);
    assert.equal(commandSchema.safeParse({...base,payload:{route,format:"program_json",canonical_source:"{}"}}).success,false);
  }
  assert.equal(commandSchema.safeParse({...base,payload:{route:"program",format:"feature_markdown",canonical_source:"# Source"}}).success,false);
  for(const bad of [seed.toUpperCase(),"",seed.slice(1),`${seed}0`,"z".repeat(64),"../bundle"]) {
    assert.equal(commandSchema.safeParse({...base,payload:{route:"program",format:"program_json",canonical_source:"{}",bundle_manifest_sha:bad}}).success,false);
  }
});

test("durable text saves cannot carry model consent while drafting requires an exact saved fence",()=>{
  const disclosure={context_sha:"context",policy_sha:"policy",model_provider_ids:["provider"],private_material_to_models:true};
  const create={command_id:id,action:"request.create",payload:{project:"demo",text:"Save this without a model call",client_message_id:other}};
  const append={command_id:id,action:"turn.append",request_id:"request",expected_content_version:7,payload:{client_message_id:other,text:"Exact saved follow-up"}};
  for(const save of [create,append]) {
    assert.equal(commandSchema.safeParse(save).success,true);
    assert.equal(commandSchema.safeParse({...save,payload:{...save.payload,disclosure}}).success,false);
    assert.equal(commandSchema.safeParse({...save,disclosure}).success,false);
  }
  const draft={command_id:id,action:"intake.draft",request_id:"request",expected_content_version:7,payload:{disclosure}};
  assert.equal(commandSchema.safeParse(draft).success,true);
  const {request_id:requestID,...withoutRequest}=draft;
  const {expected_content_version:version,...withoutVersion}=draft;
  assert.equal(requestID,"request");assert.equal(version,7);
  for(const invalid of [withoutRequest,withoutVersion,{...draft,payload:{}},{...draft,payload:{disclosure,text:"new unsaved input"}},{...draft,expected_content_version:-1},{...draft,expected_content_version:1.5}]) {
    assert.equal(commandSchema.safeParse(invalid).success,false);
  }
});

function draftStatusFixture(route:Draft["route"]="feature") {
 const hash="a".repeat(64),created="2026-10-28T12:00:00Z";
 const draft=draftSchema.parse({request_id:"request",revision:1,content_version:2,route,format:route==="program"?"program_json":"feature_markdown",canonical_source:"Exact immutable proposal",source_sha:hash,canonical_sha:hash,domain_revision:hash,input_manifest_sha:"",output_sha:"",policy_sha:hash,assumptions:[],unresolved_risks:[],findings:[]});
 const request=requestSchema.parse({id:"request",operator_id:"operator",project:"demo",state:"review_ready",content_version:2,latest_draft_revision:1,creation_policy_sha:hash,event_sequence:10,created_at:created,updated_at:created});
 const link=linkSchema.parse({request_id:"request",draft_revision:1,subject_kind:route,subject_id:"accepted-product",subject_revision:hash,command_id:id,created_at:created});
 return {draft,request,link};
}

test("durable exact handoff remains submitted when handoff advances request content version",()=>{
 for(const route of ["feature","plan","program"] as const) {
  const {draft,request,link}=draftStatusFixture(route);
  const result=draftDisposition(draft,{...request,state:"submitted",content_version:3},[link]);
  assert.equal(result.kind,"handed_off");assert.equal(result.canApprove,false);
  assert.ok(result.kind==="handed_off");assert.equal(result.link.subject_kind,route);assert.equal(result.link.subject_id,"accepted-product");assert.equal(result.link.subject_revision,draft.domain_revision);
  assert.equal(draftDisposition(draft,request,[link]).kind,"handed_off","a committed link also fences an older request snapshot");
  assert.equal(draftDisposition(draft,request,[link]).canApprove,false);
 }
});

test("submitted state without an exact durable link is unresolved, never inferred superseded or approvable",()=>{
 const {draft,request,link}=draftStatusFixture();
 const submitted={...request,state:"submitted",content_version:3};
 for(const links of [[],[{...link,request_id:"other-request"}],[{...link,draft_revision:2}],[{...link,subject_kind:"plan" as const}],[{...link,subject_revision:"b".repeat(64)}]]) {
  const result=draftDisposition(draft,submitted,links);
  assert.equal(result.kind,"submitted_unresolved");assert.equal(result.canApprove,false);
 }
 assert.equal(linkSchema.safeParse({...link,draft_revision:undefined}).success,false,"a subject link without its immutable draft revision cannot prove handoff");
});

test("only current unsubmitted review-ready content is approvable; progress alone does not stale it",()=>{
 const {draft,request}=draftStatusFixture();
 assert.deepEqual(draftDisposition(draft,request,[]),{kind:"current",canApprove:true});
 assert.deepEqual(draftDisposition(draft,{...request,event_sequence:999},[]),{kind:"current",canApprove:true});
 assert.deepEqual(draftDisposition(draft,{...request,content_version:3},[]),{kind:"superseded",canApprove:false});
 assert.deepEqual(draftDisposition(draft,{...request,latest_draft_revision:2},[]),{kind:"historical",canApprove:false});
 assert.deepEqual(draftDisposition({...draft,request_id:"different"},request,[]),{kind:"unrelated",canApprove:false});
 assert.equal(draftDisposition(draft,{...request,active_step_id:"active-step"},[]).canApprove,false);
 for(const state of ["collecting","draft_queued","drafting","blocked","cancelling","cancelled"])assert.equal(draftDisposition(draft,{...request,state},[]).canApprove,false,state);
});
