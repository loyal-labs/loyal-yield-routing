import { test, expect } from "bun:test";
import { readFileSync } from "node:fs";
import { readInitializerArtifact, verifyInitializerPolicy } from "./multiply-initializer-artifact.js";
const root = new URL("../../../../docs/evidence/voltr-selector-2026-09-16/",import.meta.url);
const bytes=readFileSync(new URL("initializer-install-candidates-149-151.json",root));
const artifact=readInitializerArtifact(bytes);
const simulation=JSON.parse(readFileSync(new URL("initializer-first-policy-simulation-447652263.json",root),"utf8"));
const account={...simulation.accounts[1],data:Buffer.from(simulation.accounts[1].data[0],"base64")};
// Captured simulation start time; controlled readback matching, not live authority.
const time=1789602843;
test("reviewed artifact and actual simulated policy agree",()=>{
 expect(artifact.policies.length).toBe(3);
 expect(verifyInitializerPolicy(artifact.policies[0]!,account,time)).toBe("2ed6ba7ff124b71ef20df17371bb7f5842f904733d970f1e79c9d4edf77f1eca");
});
test("altered installation authority cannot enter the signing path",()=>{
 const changed=JSON.parse(bytes.toString());changed.policies[0].instruction.accounts[1].address=changed.delegate;
 expect(()=>readInitializerArtifact(Buffer.from(JSON.stringify(changed)))).toThrow("reviewed compiler output");
});
test("wrong identity, lane, padding and time refuse policy evidence",()=>{
 for(const changed of [{...account,owner:artifact.policies[0]!.account},{...account,executable:true},{...account,lamports:0},{...account,data:account.data.subarray(0,576)}])expect(()=>verifyInitializerPolicy(artifact.policies[0]!,changed,time)).toThrow();
 expect(()=>verifyInitializerPolicy(artifact.policies[1]!,account,time)).toThrow();
 const padding=Buffer.from(account.data);padding[933]=1;
 expect(()=>verifyInitializerPolicy(artifact.policies[0]!,{...account,data:padding},time)).toThrow();
 expect(()=>verifyInitializerPolicy(artifact.policies[0]!,account,time+121)).toThrow();
});
