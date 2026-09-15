import {expect,test} from "bun:test";
import {readFileSync} from "node:fs";
import {gunzipSync} from "node:zlib";
import {createHash} from "node:crypto";
import {onreConnectedProof,onreSetupStagingProof} from "./onre-connected-proof.js";

test("OnRe leverage requires the actual borrowed-cash swap and debt-bearing redeposit before payoff",()=>{
  const retained=JSON.parse(gunzipSync(readFileSync(new URL("../../../../docs/evidence/backyard-rwa-go/phase3/onre-connected-leverage-2026-09-06.json.gz",import.meta.url))).toString());
  const {inputs,execution}=retained.preflight.localOnReLeverage.data;
  const verify=(e:any,p=inputs.plan,leverage=true)=>onreConnectedProof(e,p,inputs.snapshot,execution.planSha256,execution.snapshotSha256,0,leverage);
  expect(verify(execution)).toBe(true);
  expect(verify(execution,inputs.plan,false)).toBe(false);
  expect(onreSetupStagingProof(execution,true)).toBe(true);
  for(const mutate of [
    (e:any)=>e.schema="phase3-onre-lending-roundtrip-result/v1",
    (e:any)=>e.lendingSteps.splice(2,2),
    (e:any)=>e.lendingSteps[2].leg="deposit",
    (e:any)=>e.lendingSteps[2].amountRaw++,
    (e:any)=>e.lendingSteps[2].executedRequest.amountRaw=2000,
    (e:any)=>e.lendingSteps[2].executedRequest.minimumOutputRaw=1,
    (e:any)=>e.lendingSteps[2].executedRequest.source=inputs.plan.collateralCustody,
    (e:any)=>e.lendingSteps[2].negative.rejectedBeforeJupiterCPI=false,
    (e:any)=>e.lendingSteps[2].computeUnits=200001,
    (e:any)=>e.lendingSteps[3].leg="deposit",
    (e:any)=>e.lendingSteps[3].wireBase64=e.lendingSteps[0].wireBase64,
    (e:any)=>e.lendingSteps[3].receiptRaw=e.lendingSteps[1].receiptRaw,
    (e:any)=>e.lendingSteps[3].debtSF="0",
    (e:any)=>e.lendingSteps[3].negative.custodyUnchanged=false,
    (e:any)=>e.lendingSteps[3].before=e.lendingSteps[1].after,
    (e:any)=>e.lendingSteps[4].amountRaw=999,
    (e:any)=>e.overrides[1].after++,
    (e:any)=>e.terminalUSDCRaw++,
  ]){const e=structuredClone(execution);mutate(e);expect(verify(e)).toBe(false);}
  const p=structuredClone(inputs.plan);delete p.onreLending.redeposit;expect(verify(execution,p)).toBe(false);
});

test("OnRe staged setup proof requires every exact shape, unchanged authority and both payer fees",()=>{
  const retained=JSON.parse(gunzipSync(readFileSync(new URL("../../../../docs/evidence/backyard-rwa-go/phase3/onre-staged-setup-2026-09-06.json.gz",import.meta.url))).toString());
  const e=retained.preflight.localOnReConnected.data.execution;
  expect(onreSetupStagingProof(e)).toBe(true);
  for(const mutate of [
    (e:any)=>e.candidateCreation.pop(),
    (e:any)=>e.candidateCreation[0].stagedComparison.broadcast=true,
    (e:any)=>e.candidateCreation[1].stagedComparison.sameSettings=false,
    (e:any)=>e.candidateCreation[2].stagedComparison.samePolicyBytesAndBalance=false,
    (e:any)=>e.candidateCreation[0].stagedComparison.firstPayerDebitLamports-=5000,
    (e:any)=>e.candidateCreation[3].stagedComparison.secondPayerDebitLamports-=5000,
    (e:any)=>e.candidateCreation[0].stagedComparison.allocatedBytes=1400,
    (e:any)=>e.candidateCreation[0].stagedComparison.fundingPacketBytes=1233,
    (e:any)=>e.candidateCreation[0].stagedComparison.localRentLamports++,
  ]){const changed=structuredClone(e);mutate(changed);expect(onreSetupStagingProof(changed)).toBe(false);}
});

test("OnRe connected verifier rejects altered real-program witnesses, not just failure labels",()=>{
  const retained=JSON.parse(gunzipSync(readFileSync(new URL("../../../../docs/evidence/backyard-rwa-go/phase3/onre-connected-lending-2026-09-06.json.gz",import.meta.url))).toString());
  const {inputs,execution}=retained.preflight.localOnReConnected.data;
  const verify=(e:any,p=inputs.plan,s=inputs.snapshot,code:number|null=0)=>onreConnectedProof(e,p,s,execution.planSha256,execution.snapshotSha256,code);
  expect(verify(execution)).toBe(true);
  const terminal=(e:any,address:string,offset:number,value:bigint)=>{
    const a=e.steps[1].after.find((a:any)=>a.address===address);
    const b=a.present===false?Buffer.alloc(3344):Buffer.from(a.dataBase64,"base64");
    if(a.present===false)Object.assign(a,{present:true,owner:"KLend2g3cP87fffoy8q1mQqGKjrxjC8boSyAYavgmjD",lamports:1});
    b.writeBigUInt64LE(value,offset);
    a.dataBase64=b.toString("base64");a.dataSha256=createHash("sha256").update(b).digest("hex");
  };
  for(const mutate of [
    (e:any)=>e.broadcast=true,
    (e:any)=>e.installedPolicyProof=true,
    (e:any)=>e.goCompilerProof=true,
    (e:any)=>e.schema="phase3-onre-swap-roundtrip-result/v1",
    (e:any)=>e.planSha256="forged",
    (e:any)=>e.programs[0].elfSha256="forged",
    (e:any)=>e.slot++,
    (e:any)=>e.stateAddresses.pop(),
    (e:any)=>e.overrides.push(e.overrides[0]),
    (e:any)=>e.overrides[1].after++,
    (e:any)=>e.candidateCreation[2].policy=e.candidateCreation[0].policy,
    (e:any)=>e.candidateCreation[3].packetBytes=1233,
    (e:any)=>e.lendingSteps.pop(),
    (e:any)=>e.lendingSteps[0].before[0].lamports++,
    (e:any)=>e.lendingSteps[1].amountRaw++,
    (e:any)=>e.lendingSteps[2].wireBase64=e.lendingSteps[1].wireBase64,
    (e:any)=>e.lendingSteps[2].negative.rejectedBeforeKaminoCPI=false,
    (e:any)=>e.lendingSteps[3].templateSha256="forged",
    (e:any)=>e.lendingSteps[3].after.find((a:any)=>a.address==="ST999VUTo5QExYEX9bz1oDDoKGkjXG9zpphy4Hj7VWh").lamports++,
    (e:any)=>e.steps[0].computeUnits=200001,
    (e:any)=>e.steps[1].additionalNegatives.pop(),
    (e:any)=>e.steps[1].executedRequest.minimumOutputRaw=1,
    (e:any)=>e.steps[1].executedRequest.source=inputs.plan.inputCustody,
    (e:any)=>e.steps[1].after[0].dataSha256="forged",
    (e:any)=>terminal(e,inputs.plan.collateralCustody,64,1n),
    (e:any)=>terminal(e,inputs.plan.onreLending.obligation,1296,1n),
    (e:any)=>e.terminalUSDCRaw++,
  ]){const e=structuredClone(execution);mutate(e);expect(verify(e)).toBe(false);}
  expect(verify(execution,inputs.plan,inputs.snapshot,1)).toBe(false);
  expect(verify(execution,{...inputs.plan,lane:"AUTO/AUTO/PYUSD"})).toBe(false);
});
