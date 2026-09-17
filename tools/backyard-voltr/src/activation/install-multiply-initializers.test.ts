import { test, expect } from "bun:test";
import { readFileSync } from "node:fs";
import { createHash } from "node:crypto";
import { PublicKey, TransactionMessage, VersionedTransaction } from "@solana/web3.js";
import bs58 from "bs58";
import { verifyInitializerAttempt } from "./install-multiply-initializers.js";
import { INITIALIZER_ADMIN, INITIALIZER_ARTIFACT_SHA256, readInitializerArtifact } from "./multiply-initializer-artifact.js";
import { createPolicyInstruction } from "./rwa-basic-policy-set.js";
const bytes=readFileSync(new URL("../../../../docs/evidence/voltr-selector-2026-09-16/initializer-install-candidates-149-151.json",import.meta.url));
const policy=readInitializerArtifact(bytes).policies[0]!;
const tx=new VersionedTransaction(new TransactionMessage({payerKey:new PublicKey(INITIALIZER_ADMIN),recentBlockhash:"11111111111111111111111111111111",instructions:[createPolicyInstruction(policy)]}).compileToLegacyMessage());
const record=(value:VersionedTransaction)=>({schema:"pilot-initializer-install-attempt/v1",artifactSha256:INITIALIZER_ARTIFACT_SHA256,seed:"149",wireBase64:Buffer.from(value.serialize()).toString("base64"),wireSha256:createHash("sha256").update(value.serialize()).digest("hex"),signature:bs58.encode(value.signatures[0]!)});
test("unsigned and forged retained transactions cannot count as signed attempts",()=>{
 expect(()=>verifyInitializerAttempt(record(tx),bytes,0)).toThrow("signature verification");
 tx.signatures[0]!.fill(1);
 expect(()=>verifyInitializerAttempt(record(tx),bytes,0)).toThrow("signature verification");
});
test("wire mutation and lane substitution cannot enter recovery",()=>{
 const row=record(tx);
 expect(()=>verifyInitializerAttempt({...row,wireSha256:"0".repeat(64)},bytes,0)).toThrow("hash mismatch");
 expect(()=>verifyInitializerAttempt(row,bytes,1)).toThrow("identity mismatch");
 expect(()=>verifyInitializerAttempt({...row,seed:"150"},bytes,1)).toThrow("exact reviewed installation");
});

import { initializerJournalPath, installMultiplyInitializer, verifyInitializerNativeEffects } from "./install-multiply-initializers.js";
test("alternate journal paths cannot bypass recovery",async()=>{
 expect(initializerJournalPath(0)).toEndWith("initializer-install-seed-149.json");
 expect(initializerJournalPath(2)).toEndWith("initializer-install-seed-151.json");
 expect(()=>initializerJournalPath(3)).toThrow();
 await expect(installMultiplyInitializer("unused",0,"/private/tmp/alternate-initializer.json",true)).rejects.toThrow("canonical initializer journal path");
});
test("finalized creation conserves native funds and bounds fees",()=>{
 const keys=[new PublicKey(INITIALIZER_ADMIN),new PublicKey(policy.account),new PublicKey("11111111111111111111111111111111")];
 const meta={fee:5000,preBalances:[10000000,0,1],postBalances:[4600040,5394960,1]};
 expect(()=>verifyInitializerNativeEffects(keys,policy.account,meta)).not.toThrow();
 for(const changed of [
  {...meta,fee:50001},
  {...meta,preBalances:[10000000,1,1]},
  {...meta,postBalances:[4600039,5394960,1]},
  {...meta,postBalances:[4600040,5394960,2]},
  {...meta,postBalances:[4600040,5394960]},
  {...meta,postBalances:[Number.NaN,5394960,1]},
 ])expect(()=>verifyInitializerNativeEffects(keys,policy.account,changed)).toThrow();
});
