import { z } from "zod";
import { programQuery, programID, programPageInput, artifactInput, programListSchema, programStatusSchema, sectionSchemas, artifactDetailSchema } from "./program-contract.ts";
import { dashboardGET, dashboardURL } from "./http-get.ts";

export async function fetchProgram<T>(signal: AbortSignal, path: string, query: Record<string, string | number | undefined>, schema: z.ZodType<T>, endpoint = process.env.OPENFACTORY_VIEW_URL || "http://openfactory:38887/api/status"): Promise<T> {
 const url = dashboardURL(endpoint);
 url.pathname = `/api/programs${path}`; url.search = "";
 for (const [key,value] of Object.entries(query)) if (value !== undefined) url.searchParams.set(key,String(value));
 const response = await dashboardGET(url, signal);
 if (!response.ok) {await response.body?.cancel();throw new Error(`OpenFactory API returned HTTP ${response.status}.`);}
 if (!/^application\/json(?:\s*;|$)/i.test(response.headers.get("content-type") || "")) {await response.body?.cancel();throw new Error("OpenFactory API returned non-JSON Program data.");}
 const max = 16 * 1024 * 1024;
 if (Number(response.headers.get("content-length")) > max) {await response.body?.cancel();throw new Error("OpenFactory Program response exceeded 16 MiB.");}
 if (!response.body) throw new Error("OpenFactory API returned empty Program data.");
 const reader = response.body.getReader(); const decoder = new TextDecoder("utf-8",{fatal:true}); let bytes = 0; let json = "";
 try {while (true) {signal.throwIfAborted();const chunk = await reader.read();if (chunk.done) break;bytes += chunk.value.byteLength;if (bytes > max) throw new Error("OpenFactory Program response exceeded 16 MiB.");json += decoder.decode(chunk.value,{stream:true});}json += decoder.decode();}
 finally {await reader.cancel();reader.releaseLock();}
 try {return schema.parse(JSON.parse(json));} catch {throw new Error("OpenFactory API returned invalid canonical Program presentation data.");}
}
export function programHandlers(signal: () => AbortSignal) {
 return {
  programList: async (input: z.infer<typeof programQuery>) => {const query = programQuery.parse(input);return fetchProgram(signal(),"",{limit:100,...query},programListSchema);},
  programStatus: async (input: z.infer<typeof programID>) => {const {id} = programID.parse(input);return fetchProgram(signal(),`/${encodeURIComponent(id)}`,{},programStatusSchema);},
  programPage: async (input: z.infer<typeof programPageInput>) => {const {id,section,query} = programPageInput.parse(input);return fetchProgram(signal(),`/${encodeURIComponent(id)}/${section}`,{limit:100,...query},sectionSchemas[section] as z.ZodType<z.infer<(typeof sectionSchemas)[typeof section]>>);},
  programArtifact: async (input: z.infer<typeof artifactInput>) => {const {id,artifact_id} = artifactInput.parse(input);return fetchProgram(signal(),`/${encodeURIComponent(id)}/artifacts/${encodeURIComponent(artifact_id)}`,{},artifactDetailSchema);},
 };
}
