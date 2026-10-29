import assert from "node:assert/strict";
import test from "node:test";
import {pairingCapture} from "./intake-pairing.ts";
import {takePairingFragment} from "./intake-client.ts";

test("same-page links synchronously clear every fragment and replace old enrollment and invalid-link errors",()=>{
 const events=new EventTarget(),location={pathname:"/plugins/openfactory/intake/pair",search:"?panel=1",hash:`#pairing=${"A".repeat(43)}`};
 const urls:string[]=[];const history={state:null,replaceState(_data:unknown,_unused:string,url?:string|URL|null){urls.push(String(url));location.hash="";}};
 let secret="",enrollment:string|null="old fingerprint";const causes:unknown[]=[];
 const capture=pairingCapture(()=>takePairingFragment(location,history),(next,cause)=>{assert.equal(location.hash,"");secret=next;causes.push(cause);enrollment=null;});
 const cleanup=capture.listen(events);capture.capture();assert.equal(secret,"A".repeat(43));assert.equal(enrollment,null);assert.equal(causes.at(-1),undefined);
 enrollment="first fingerprint";location.hash=`#pairing=${"B".repeat(43)}`;events.dispatchEvent(new Event("hashchange"));
 assert.equal(secret,"B".repeat(43));assert.equal(enrollment,null);assert.equal(causes.at(-1),undefined);
 location.hash="#pairing=invalid&private=secret";events.dispatchEvent(new Event("hashchange"));assert.equal(secret,"");assert.ok(causes.at(-1) instanceof Error);assert.equal(enrollment,null);
 location.hash=`#pairing=${"C".repeat(43)}`;events.dispatchEvent(new Event("hashchange"));assert.equal(secret,"C".repeat(43));assert.equal(causes.at(-1),undefined);
 assert.deepEqual(urls,Array(4).fill("/plugins/openfactory/intake/pair?panel=1"));cleanup();
});

test("listener cleanup and re-setup retain same-page handling without a bootstrap guard",()=>{
 const events=new EventTarget();let captures=0;
 const pairing=pairingCapture(()=>{captures++;return "secret";},()=>{});
 const first=pairing.listen(events);first();const second=pairing.listen(events);
 events.dispatchEvent(new Event("hashchange"));assert.equal(captures,1);second();events.dispatchEvent(new Event("hashchange"));assert.equal(captures,1);
});

test("a registration result from an older link cannot clear a newer secret or show its stale fingerprint",async()=>{
 const events=new EventTarget();let fragment="first",secret="",fingerprint:string|null=null;
 const pairing=pairingCapture(()=>fragment,(next)=>{secret=next;fingerprint=null;});const stop=pairing.listen(events);pairing.capture();
 let resolveRegistered!:(value:string)=>void;const registered=new Promise<string>((resolve)=>{resolveRegistered=resolve;});const epoch=pairing.epoch();
 const finish=(async()=>{const result=await registered;if(pairing.isCurrent(epoch)){fingerprint=result;secret="";}})();
 fragment="second";events.dispatchEvent(new Event("hashchange"));resolveRegistered("old fingerprint");await finish;
 assert.equal(secret,"second");assert.equal(fingerprint,null);assert.equal(pairing.isCurrent(epoch),false);
 const newEpoch=pairing.epoch();assert.equal(pairing.isCurrent(newEpoch),true);stop();
});
