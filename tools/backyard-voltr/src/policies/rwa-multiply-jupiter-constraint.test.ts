import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import { test } from "node:test";
import { PublicKey, TransactionInstruction } from "@solana/web3.js";
import { exactJupiterConstraint } from "./rwa-multiply-jupiter-constraint.js";
import { validateJupiterHeader } from "./rwa-multiply-jupiter-headers.js";

const read = (name:string) => JSON.parse(readFileSync(new URL(`../../../../docs/evidence/backyard-rwa-go/${name}`,import.meta.url),"utf8"));
const sha = (data:Buffer) => createHash("sha256").update(data).digest("hex");

test("existing 52 legacy edge constraints remain byte-semantically identical", () => {
  const headers=read("policy-jupiter-headers-v1.json");
  const compiled=read("policy-compiled-v1.json");
  assert.equal(headers.rows.length,52);
  for(const row of headers.rows) {
    const {constraint}=exactJupiterConstraint(row);
    const prior=compiled.policies.flatMap((p:any)=>p.constraints).find((c:any)=>
      JSON.stringify(c.accountPubkeys)===JSON.stringify(constraint.accountPubkeys)&&
      JSON.stringify(c.data)===JSON.stringify(constraint.data));
    assert.ok(prior,`${row.key} missing equivalent installed-catalog constraint`);
    assert.equal(prior.operation,null);
    assert.deepEqual({...constraint,operation:null},prior);
  }
});

test("fresh V2 bytes compile fixed economic offsets and reject fee, amount and custody mutations", () => {
  const quotes=read("phase3/jupiter-v2-return-public-quotes-2026-09-05.json");
  const candidates=read("phase3/jupiter-v2-return-repair-candidates-2026-09-05.json");
  assert.equal(quotes.rows.length,3);
  assert.deepEqual(quotes.rows.slice(0,2),read("phase3/jupiter-v2-public-quotes-2026-09-04.json").rows);
  assert.deepEqual(candidates.groups.slice(0,2),read("phase3/jupiter-v2-repair-candidates-2026-09-04.json").groups);
  assert.deepEqual(candidates.groups.map((g:any)=>g.edge).sort(),["USDC->USDe","USDe->PYUSD","USDe->USDC"].sort());
  assert.equal(quotes.broadcast,false);
  const lengths=new Set<number>();
  for(const sample of quotes.rows) {
    const {edge,quote}=sample;
    const data=Buffer.from(sample.instruction.data,"base64");
    lengths.add(data.length);
    const keys=sample.instruction.accounts.map((a:any)=>({...a,pubkey:new PublicKey(a.pubkey)}));
    const input={instruction:new TransactionInstruction({programId:new PublicKey(sample.instruction.programId),keys,data}),
      sourceMint:edge.source.mint,destinationMint:edge.destination.mint,
      sourceAta:edge.source.ata,destinationAta:edge.destination.ata,
      sourceTokenProgram:edge.source.tokenProgram,destinationTokenProgram:edge.destination.tokenProgram,
      amountRaw:BigInt(quote.inAmount),outAmountRaw:BigInt(quote.outAmount)};
    const header=validateJupiterHeader(input);
    assert.equal(header.dialect,"shared-accounts-route-v2");
    const row={pass:true,key:edge.key,source:edge.source,destination:edge.destination,header,
      quote:{inAmountRaw:quote.inAmount,outAmountRaw:quote.outAmount},
      instruction:{...sample.instruction,dataBase64:data.toString("base64"),dataSha256:sha(data)}};
    const {constraint}=exactJupiterConstraint(row);
    const candidate=candidates.groups.find((g:any)=>g.edge===edge.key);
    const original=read("policy-compiled-v1.json").policies.find((p:any)=>p.policy===candidate.originalPolicy);
    const installed=read("policy-install-readback-v1.json").operations.find((p:any)=>p.policyAddress===candidate.originalPolicy);
    assert.equal(installed.active,true);
    assert.equal(installed.dataSha256,candidate.originalPolicyDataSha256);
    assert.equal(sha(Buffer.from(installed.dataBase64,"base64")),candidate.originalPolicyDataSha256);
    assert.deepEqual(candidate.originalConstraints,original.constraints);
    assert.equal(candidate.replacementConstraints.length,original.constraints.length);
    for(let i=0;i<original.constraints.length;i++) {
      assert.deepEqual(candidate.replacementConstraints[i],i===candidate.replacedConstraintIndex?{operation:null,...constraint}:original.constraints[i]);
    }
    assert.deepEqual(constraint.data,[
      {kind:"slice-equals",offset:0,valueHex:"d19853937cfed8e9"},
      {kind:"u64-less-than-or-equal",offset:9,value:1_000_000_000_000},
      {kind:"u16-less-than-or-equal",offset:25,value:50},
      {kind:"slice-equals",offset:27,valueHex:"00000000"},
    ]);
    for(const offset of [9,17,25,27,28,29,30]) {
      const changed=Buffer.from(data); changed[offset]=offset===25?51:changed[offset]!^1;
      assert.throws(()=>validateJupiterHeader({...input,instruction:new TransactionInstruction({...input.instruction,data:changed})}),`${edge.key} economic mutation ${offset}`);
    }
    for(const index of [1,2,5,6,7,8,9]) {
      const changed=keys.map((a:any,i:number)=>i===index?{...a,pubkey:PublicKey.default}:a);
      assert.throws(()=>validateJupiterHeader({...input,instruction:new TransactionInstruction({...input.instruction,keys:changed})}),`${edge.key} account mutation ${index}`);
    }
    const wrongHeader={...row,header:{...header,indexes:{...header.indexes,slippage:data.length-3}}};
    assert.throws(()=>exactJupiterConstraint(wrongHeader),/layout/);
  }
  assert.ok(lengths.size>=2,"exercise genuinely different public route layouts");
});
