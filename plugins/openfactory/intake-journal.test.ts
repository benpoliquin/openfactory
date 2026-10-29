import assert from "node:assert/strict";
import test from "node:test";
import { browserJournal, type JournalScope } from "./intake-journal.ts";
import { disclosureSchema, type Command } from "./intake-contract.ts";

const scope:JournalScope={origin:"https://openfactory.example",operator_id:"operator-one",installation_id:"installation-one",auth_generation:"generation-one"};
const id="00000000-0000-4000-8000-000000000001";
function memory() {
 const data=new Map<string,string>();
 return {data,getItem(key:string){return data.get(key)??null;},setItem(key:string,value:string){data.set(key,value);},removeItem(key:string){data.delete(key);}};
}
function draft() {
 return {command_id:id,action:"intake.draft",request_id:"request-one",expected_content_version:3,payload:{disclosure:{context_sha:"a".repeat(64),policy_sha:"b".repeat(64),model_provider_ids:["provider-one"],bundle_manifest_shas:["c".repeat(64)],private_material_to_models:false}}};
}
test("journal namespaces isolate actors, origins, installations and restore generations",()=>{
 const storage=memory(), journal=browserJournal(storage), original=journal.bind(scope);
 original.pending.add(id); journal.rememberDraft(scope,draft());
 const alternate=[{...scope,operator_id:"operator-two"},{...scope,origin:"https://other.example"},{...scope,installation_id:"installation-two"},{...scope,auth_generation:"generation-two"}];
 const namespaces=new Set([original.namespace]);
 for(const value of alternate) {
  const bound=journal.bind(value);namespaces.add(bound.namespace);
  assert.deepEqual(bound.pending.list(),[]);assert.deepEqual(journal.drafts(value),[]);
 }
 assert.equal(namespaces.size,5);
 assert.deepEqual(journal.previous(alternate[0]),[]);
 assert.deepEqual(journal.previous(alternate[1]),[]);
 const restored=browserJournal(storage).previous(alternate[3]);
 assert.ok(restored.some(value=>value.namespace===original.namespace));
 assert.deepEqual(restored.find(value=>value.namespace===original.namespace)?.pending.list(),[id]);
 assert.equal(browserJournal(storage).drafts(scope)[0].command_id,id,"old scope remains unresolved and recoverable, never migrated into new scope");
});
test("draft metadata is cloned before storage and after reload with exact-ID conflicts rejected",()=>{
 const storage=memory(),journal=browserJournal(storage),value=draft();
 journal.rememberDraft(scope,value);
 value.payload.disclosure.model_provider_ids.push("later-provider");
 const reloaded=browserJournal(storage).drafts(scope);
 assert.deepEqual(disclosureSchema.parse(reloaded[0].payload.disclosure).model_provider_ids,["provider-one"]);
 reloaded[0].request_id="mutated";
 assert.equal(journal.drafts(scope)[0].request_id,"request-one");
 journal.rememberDraft(scope,draft());
 assert.throws(()=>journal.rememberDraft(scope,{...draft(),expected_content_version:4}));
 journal.forgetDraft(scope,id);assert.deepEqual(journal.drafts(scope),[]);
});
test("journal rejects private text, auth, assertions, non-draft commands and malformed disclosure",()=>{
 const storage=memory(),journal=browserJournal(storage);
 const unsafe:unknown[]=[{...draft(),action:"turn.append",payload:{text:"private text"}},{...draft(),text:"private"},{...draft(),assertion:{signature:"secret"}},{...draft(),payload:{...draft().payload,text:"private"}},{...draft(),payload:{disclosure:{context_sha:"not-a-hash",policy_sha:"b".repeat(64),model_provider_ids:[],private_material_to_models:false}}},{...draft(),payload:{disclosure:{context_sha:"a".repeat(64),policy_sha:"b".repeat(64),model_provider_ids:[],private_material_to_models:false,csrf_token:"secret"}}}];
 for(const value of unsafe) assert.throws(()=>journal.rememberDraft(scope,value as Command));
 assert.equal(storage.data.size,0,"unsafe metadata must never create a persisted record");
});
test("registry stores only strict identity scope and deterministic namespace",()=>{
 const storage=memory(),journal=browserJournal(storage);journal.bind(scope);
 const raw=storage.getItem("openfactory-intake.journal-scopes.v1")!;
 assert.deepEqual(JSON.parse(raw),[{scope,namespace:encodeURIComponent(JSON.stringify([scope.origin,scope.operator_id,scope.installation_id,scope.auth_generation]))}]);
 for(const invalid of [{...scope,origin:"https://openfactory.example/"},{...scope,origin:"http://remote.example"},{...scope,operator_id:""},{...scope,auth_generation:"x".repeat(257)},{...scope,csrf_token:"secret"}]) assert.throws(()=>journal.bind(invalid as JournalScope));
 assert.doesNotThrow(()=>journal.bind({...scope,origin:"http://localhost:38886"}));
});
test("corrupt registry and corrupt draft records fail closed without clearing unresolved data",()=>{
 for(const raw of ["not json","{}","[{\"scope\":{},\"namespace\":\"forged\"}]",JSON.stringify([{scope,namespace:"forged"}])]) {
  const storage=memory();storage.setItem("openfactory-intake.journal-scopes.v1",raw);
  assert.throws(()=>browserJournal(storage).bind(scope));assert.throws(()=>browserJournal(storage).previous(scope));assert.equal(storage.getItem("openfactory-intake.journal-scopes.v1"),raw);
 }
 const storage=memory(),journal=browserJournal(storage),bound=journal.bind(scope);
 const key=`openfactory-intake.draft-metadata.v1.${bound.namespace}`;
 storage.setItem(key,JSON.stringify([{...draft(),payload:{text:"private"}}]));
 assert.throws(()=>journal.drafts(scope));assert.ok(storage.getItem(key));
});
test("scope registry has a hard limit with no eviction",()=>{
 const storage=memory(),journal=browserJournal(storage);
 for(let index=0;index<64;index++) journal.bind({...scope,auth_generation:`generation-${index}`});
 const saved=storage.getItem("openfactory-intake.journal-scopes.v1");
 assert.throws(()=>journal.bind({...scope,auth_generation:"one-too-many"}));
 assert.equal(storage.getItem("openfactory-intake.journal-scopes.v1"),saved);
 assert.equal(journal.previous({...scope,auth_generation:"unbound-generation"}).length,64);
});
test("storage read and write failures block binding and metadata dispatch preparation",()=>{
 const unreadable={getItem(){throw new Error("denied");},setItem(){throw new Error("denied");},removeItem(){throw new Error("denied");}};
 assert.throws(()=>browserJournal(unreadable).bind(scope));assert.throws(()=>browserJournal(unreadable).rememberDraft(scope,draft()));
 const unwritable={getItem(){return null;},setItem(){throw new Error("full");},removeItem(){}};
 assert.throws(()=>browserJournal(unwritable).bind(scope));
 const storage=memory(),journal=browserJournal(storage);journal.bind(scope);
 const bound=browserJournal({getItem:storage.getItem,setItem(){throw new Error("full");},removeItem:storage.removeItem});
 assert.throws(()=>bound.rememberDraft(scope,draft()));assert.deepEqual(journal.drafts(scope),[]);
});
test("configured provider model paths roundtrip exactly without control characters",()=>{
 const storage=memory(),journal=browserJournal(storage),value=draft();
 value.payload.disclosure.model_provider_ids=["openrouter/qwen/qwen3-coder:exact","provider/model.with+symbols"];
 journal.rememberDraft(scope,value);
 assert.deepEqual(disclosureSchema.parse(journal.drafts(scope)[0].payload.disclosure).model_provider_ids,value.payload.disclosure.model_provider_ids);
 for(const provider of [""," \t ","provider\nmodel","x".repeat(513)]) {
  const invalid=draft();invalid.command_id="00000000-0000-4000-8000-000000000002";invalid.payload.disclosure.model_provider_ids=[provider];
  assert.throws(()=>journal.rememberDraft(scope,invalid));
 }
});
test("forgetting absent metadata is write-free and final removal deletes its key",()=>{
 const storage=memory(),journal=browserJournal(storage),bound=journal.bind(scope);
 const readOnly=browserJournal({getItem:storage.getItem,setItem(){throw new Error("must not write");},removeItem(){throw new Error("must not remove");}});
 assert.doesNotThrow(()=>readOnly.forgetDraft(scope,id));
 journal.rememberDraft(scope,draft());
 const key=`openfactory-intake.draft-metadata.v1.${bound.namespace}`;
 assert.ok(storage.getItem(key));
 assert.throws(()=>readOnly.forgetDraft(scope,id));
 assert.ok(storage.getItem(key),"failed deletion retains recovery metadata");
 journal.forgetDraft(scope,id);assert.equal(storage.getItem(key),null);
});
