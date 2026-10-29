export interface PairingCapture {
 capture(): void;
 epoch(): number;
 isCurrent(epoch: number): boolean;
 listen(target: Pick<EventTarget,"addEventListener"|"removeEventListener">): () => void;
}
// The supplied capture must synchronously remove fragment bytes before returning or throwing.
export function pairingCapture(capture:()=>string,onCapture:(secret:string,error:unknown,epoch:number)=>void):PairingCapture {
 let generation=0;
 const refresh=()=>{
  const epoch=++generation;
  let secret="",error:unknown;
  try {secret=capture();} catch(cause) {error=cause;}
  onCapture(secret,error,epoch);
 };
 return {
  capture:refresh,
  epoch:()=>generation,
  isCurrent:epoch=>epoch===generation,
  listen(target) {
   target.addEventListener("hashchange",refresh);
   return ()=>target.removeEventListener("hashchange",refresh);
  },
 };
}
