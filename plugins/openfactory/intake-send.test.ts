import assert from "node:assert/strict";
import test from "node:test";
import {sendJournal} from "./intake-send.ts";
import {IntakeBrowserError} from "./intake-browser.ts";
const saveID="baf9b067-9ca4-4cbd-a5b4-7fc6584a5e6b";
const draftID="baf9b067-9ca4-4cbd-a5b4-7fc6584a5e6c";
const thirdID="baf9b067-9ca4-4cbd-a5b4-7fc6584a5e6d";
const key="openfactory-intake.send-lifecycle.v1";
function fixture() {
  const data=new Map<string,string>();
  return {data,getItem(name:string){return data.get(name)??null;},setItem(name:string,value:string){data.set(name,value);},removeItem(name:string){data.delete(name);}};
}

test("lost save response survives reload without generating another save or draft ID",()=>{
  const storage=fixture();const first=sendJournal(storage);
  assert.equal(first.get(),null);
  assert.deepEqual(first.begin(saveID,draftID),{save_command_id:saveID,draft_command_id:draftID,phase:"saving"});
  const reloaded=sendJournal(storage);
  assert.deepEqual(reloaded.get(),first.get());
  assert.throws(()=>reloaded.drafting(),IntakeBrowserError);
  assert.throws(()=>reloaded.begin(thirdID,draftID),IntakeBrowserError);
  const recovered=reloaded.saved("openfactory-intake-request-1",7);
  assert.deepEqual(recovered,{save_command_id:saveID,draft_command_id:draftID,request_id:"openfactory-intake-request-1",content_version:7,phase:"saved"});
  assert.deepEqual(sendJournal(storage).begin(saveID,draftID),recovered,"same IDs never reset recovered phase");
  const drafting=sendJournal(storage).drafting();assert.equal(drafting.phase,"drafting");assert.equal(drafting.draft_command_id,draftID);
  assert.deepEqual(sendJournal(storage).drafting(),drafting,"reconnect cannot generate new model grant ID");
  assert.deepEqual(sendJournal(storage).saved("openfactory-intake-request-1",7),drafting);
  assert.throws(()=>sendJournal(storage).saved("openfactory-intake-request-1",8),IntakeBrowserError);
  sendJournal(storage).clear();assert.equal(sendJournal(storage).get(),null);
});

test("only lifecycle IDs and numeric fences persist; caller-added private fields cannot leak",()=>{
  const storage=fixture(),journal=sendJournal(storage);
  const state=journal.begin(saveID,draftID);
  Object.assign(state,{text:"private transcript",token:"auth secret",project:"private project",providers:["private-provider"]});
  journal.saved("request_1",0);journal.drafting();
  const raw=storage.getItem(key)!;
  assert.deepEqual(JSON.parse(raw),{save_command_id:saveID,draft_command_id:draftID,phase:"drafting",request_id:"request_1",content_version:0});
  for(const secret of ["private transcript","auth secret","private project","private-provider"]) assert.equal(raw.includes(secret),false);
});

test("malformed, unknown-key and impossible persisted lifecycle states are removed",()=>{
  const valid={save_command_id:saveID,draft_command_id:draftID,phase:"saved",request_id:"request",content_version:1};
  const storage=fixture();
  for(const bad of [null,[],{...valid,token:"secret"},{...valid,save_command_id:"not-uuid"},{...valid,draft_command_id:saveID},{...valid,phase:"approved"},{...valid,request_id:"../private"},{...valid,content_version:-1},{...valid,content_version:1.5},{...valid,content_version:Number.MAX_SAFE_INTEGER+1},{...valid,phase:"saving"},{save_command_id:saveID,draft_command_id:draftID,phase:"drafting"}]) {
    storage.setItem(key,JSON.stringify(bad));assert.equal(sendJournal(storage).get(),null);assert.equal(storage.data.has(key),false);
  }
  storage.setItem(key,"{broken");assert.equal(sendJournal(storage).get(),null);assert.equal(storage.data.size,0);
});

test("invalid transition inputs do not replace the last durable state",()=>{
  const storage=fixture(),journal=sendJournal(storage);
  assert.throws(()=>journal.saved("request",1),IntakeBrowserError);
  assert.throws(()=>journal.begin(saveID,saveID),IntakeBrowserError);
  assert.equal(storage.data.size,0);journal.begin(saveID,draftID);
  const original=storage.getItem(key);
  for(const [request,version] of [["request",-1],["/private",1],["request",1.2]] as const) assert.throws(()=>journal.saved(request,version),IntakeBrowserError);
  assert.equal(storage.getItem(key),original);
});

test("unavailable journal storage fails closed before advancing phases",()=>{
  const storage=fixture();let rejectWrites=false;
  const journal=sendJournal({...storage,setItem(name:string,value:string){if(rejectWrites)throw new Error("quota");storage.setItem(name,value);}});
  rejectWrites=true;assert.throws(()=>journal.begin(saveID,draftID),error=>error instanceof IntakeBrowserError&&error.code==="storage_unavailable");assert.equal(storage.data.size,0);
  rejectWrites=false;journal.begin(saveID,draftID);rejectWrites=true;
  assert.throws(()=>journal.saved("request",1),IntakeBrowserError);assert.equal(journal.get()?.phase,"saving");
  const inaccessible=sendJournal({getItem(){throw new Error("denied");},setItem(){throw new Error("denied");},removeItem(){throw new Error("denied");}});
  assert.throws(()=>inaccessible.begin(saveID,draftID),IntakeBrowserError);assert.throws(()=>inaccessible.clear(),IntakeBrowserError);
  const corrupt=sendJournal({getItem(){return "broken";},setItem(){},removeItem(){throw new Error("denied");}});
  assert.throws(()=>corrupt.get(),IntakeBrowserError);
});

test("send lifecycle namespaces keep old generation unresolved and new generation independent",()=>{
  const storage=fixture(),old=sendJournal(storage,"scope-generation-1"),fresh=sendJournal(storage,"scope-generation-2"),legacy=sendJournal(storage);
  old.begin(saveID,draftID);old.saved("request",4);old.drafting();
  assert.equal(fresh.get(),null);assert.equal(legacy.get(),null);
  fresh.begin(thirdID,saveID);fresh.saved("new-request",1);
  assert.equal(old.get()?.phase,"drafting");assert.equal(fresh.get()?.phase,"saved");
  fresh.clear();assert.equal(fresh.get(),null);assert.equal(old.get()?.draft_command_id,draftID);
  assert.equal(storage.data.has("openfactory-intake.send-lifecycle.v1.scope-generation-1"),true);
  storage.setItem("openfactory-intake.send-lifecycle.v1.scope-generation-2","corrupt");
  assert.equal(fresh.get(),null);assert.equal(old.get()?.phase,"drafting");
});
