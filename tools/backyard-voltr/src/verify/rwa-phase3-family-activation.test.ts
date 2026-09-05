import { describe, expect, test } from "bun:test";
import {createHash} from "node:crypto";
import { exactSet, measuredCondition, localCapTestProof, catalogJupiterPacketProof, candidateJupiterExecutionProof, returnQuoteCompatibilityProof, linkedLendingReturnProof, prefundedPolicyProof } from "./rwa-phase3-family-activation.js";

describe("Phase 3 measured verifier", () => {
  test("prefunded setup proof requires exact policy preservation and both real debits including fees",()=>{
    const rows=[{accountCount:15,allocatedBytes:1400},{accountCount:13,allocatedBytes:1250}].map(r=>({...r,broadcast:false,installed:false,accepted:true,error:null,samePolicyBytesAndBalance:true,firstFundingLamports:50000,localRentLamports:100000,firstPayerDebitLamports:55000,secondPayerDebitLamports:55000}));
    const encode=(r:unknown[])=>r.map(v=>"PHASE3_PREFUNDED_POLICY "+JSON.stringify(v)).join("\n");
    expect(prefundedPolicyProof(encode(rows),0)).toMatchObject({complete:true,supported:true});
    for(const mutate of [(r:any)=>r.broadcast=true,(r:any)=>r.installed=true,(r:any)=>r.samePolicyBytesAndBalance=false,(r:any)=>r.firstPayerDebitLamports=50000,(r:any)=>r.secondPayerDebitLamports=50000,(r:any)=>r.secondPayerDebitLamports=100000,(r:any)=>r.allocatedBytes=860,(r:any)=>r.error="rejected"]) {
      const bad=structuredClone(rows);mutate(bad[0]);expect(prefundedPolicyProof(encode(bad),0).supported).toBe(false);
    }
    expect(prefundedPolicyProof(encode(rows.slice(1)),0).complete).toBe(false);
    expect(prefundedPolicyProof(encode([...rows,rows[0]]),0).complete).toBe(false);
    expect(prefundedPolicyProof(encode(rows),1).complete).toBe(false);
    const rejected=rows.map(r=>({...r,accepted:false,error:"program rejected prefunding",samePolicyBytesAndBalance:false,secondPayerDebitLamports:5000}));
    expect(prefundedPolicyProof(encode(rejected),0)).toMatchObject({complete:true,supported:false});
  });
  test("linked lending proof rejects custody resets, discontinuity and raw terminal debt despite success labels",()=>{
    const sha=(b:Buffer)=>createHash("sha256").update(b).digest("hex");
    const row=(address:string,data:Buffer)=>({address,present:true,lamports:1,dataBase64:data.toString("base64"),dataSha256:sha(data)});
    const state=(c:number,d:number,u:number,receipts:number,debt:number)=>{
      const tokens=[["c",c],["d",d],["u",u]].map(([address,n])=>{const b=Buffer.alloc(165);b.writeBigUInt64LE(BigInt(n!),64);return row(address as string,b);});
      const obligation=Buffer.alloc(3344);obligation.writeBigUInt64LE(BigInt(receipts),128);obligation.writeBigUInt64LE(BigInt(debt),1296);
      return [...tokens,row("o",obligation)];
    };
    const states=[state(100_000_000,2000,0,0,0),state(0,2000,0,100,0),state(0,3000,0,100,1000),state(0,2000,0,100,0),state(99_999_999,2000,0,0,0),state(0,2000,100000,0,0),state(0,0,102000,0,0)];
    const legs=["deposit","borrow","repay","withdraw"];
    const plan={lane:"Ethena/USDe/PYUSD",delegate:"manager",collateralCustody:"c",debtCustody:"d",inputCustody:"u",addresses:["c","d","u","o"],
      lendingPrelude:{schema:"phase3-kamino-controlled-probe/v1",broadcast:false,signatureProof:false,lane:"Ethena/USDe/PYUSD",delegate:"manager",collateralCustody:"c",debtCustody:"d",obligation:"o",steps:legs.map((leg,i)=>({leg,wireSha256:"wire"+i}))},
      steps:[{source:"c",amountRaw:99_999_999},{source:"d",amountRaw:2000}]};
    const chain=Array.from({length:6},(_,i)=>({leg:legs[i],wireSha256:"wire"+i,before:states[i],after:states[i+1],error:null,computeUnits:100000}));
    const evidence={lendingSteps:chain.slice(0,4),steps:chain.slice(4),terminalUSDCRaw:102000,stateAddresses:plan.addresses};
    const snapshot={accounts:plan.addresses.map(address=>({address,executable:false}))};
    expect(linkedLendingReturnProof(evidence,plan,snapshot)).toBe(true);
    const mutations=[
      (e:any)=>e.lendingSteps.pop(),
      (e:any)=>e.lendingSteps[1].wireSha256="different",
      (e:any)=>e.steps[0].before=state(99_999_998,2000,0,0,0),
      (e:any)=>e.steps[1].before[0].lamports++,
      (e:any)=>e.steps[1].after=state(0,0,102000,0,1),
      (e:any)=>e.steps[1].after=state(0,1,102000,0,0),
      (e:any)=>e.steps[1].after[0].dataSha256="forged",
      (e:any)=>e.steps[1].after.pop(),
      (e:any)=>e.terminalUSDCRaw++,
      (e:any)=>e.lendingSteps[0].before=state(99_999_999,2000,0,0,0),
    ];
    // Serialized evidence has independent before/after objects, not the shared
    // fixture state references used above.
    for(const mutate of mutations) { const bad=JSON.parse(JSON.stringify(evidence));mutate(bad);expect(linkedLendingReturnProof(bad,plan,snapshot)).toBe(false); }
    expect(linkedLendingReturnProof(evidence,{...plan,lendingPrelude:undefined},snapshot)).toBe(false);
    expect(linkedLendingReturnProof({...evidence,stateAddresses:plan.addresses.slice(1)},plan,snapshot)).toBe(false);
    expect(linkedLendingReturnProof(evidence,plan,{accounts:[{address:"c",executable:true}]})).toBe(false);
  });
  test("return proof requires full source sweeps and linked USDC balances, not a claimed flat flag",()=>{
    const mutations=["slippage_above_50_bps","platform_fee_low_byte","platform_fee_high_byte","positive_slippage_fee_low_byte","positive_slippage_fee_high_byte","destination_replaced_by_source"];
    const plan={compiler:"TYPESCRIPT_CANDIDATE_NOT_INSTALLED_GO",broadcast:false,profile:"RETURN_CONVERSIONS",lane:"Ethena/USDe/PYUSD",candidate:{policies:[{edge:"USDe->USDC",policy:"p1",seed:"140"}]},steps:[{wireSha256:"w1",amountRaw:10,minimumOutputRaw:8},{wireSha256:"w2",amountRaw:20,minimumOutputRaw:18}]};
    const evidence={schema:"phase3-jupiter-return-controlled-result/v1",broadcast:false,signatureProof:false,installedPolicyProof:false,goCompilerProof:false,planSha256:"plan",snapshotSha256:"snapshot",twoSwapsPassed:true,returnCustodyCleared:true,terminalUSDCRaw:26,
      candidateCreation:[{candidateOnly:true,policy:"p1",seed:140,packetBytes:1100}],
      steps:["SWAP_COLLATERAL_TO_STABLE_STEP","SWAP_DEBT_TO_USDC_STEP"].map((action,i)=>({action,economicPass:true,error:null,wireSha256:plan.steps[i]!.wireSha256,computeUnits:190000,custodyBefore:i===0?[10,0]:[20,8],custodyAfter:i===0?[0,8]:[0,26],negative:{rejectedBeforeJupiterCPI:true,custodyUnchanged:true},additionalNegatives:(i===0?mutations:["slippage_above_50_bps","platform_fee_low_byte","destination_replaced_by_source"]).map(mutation=>({mutation,rejectedBeforeJupiterCPI:true,custodyUnchanged:true}))}))};
    const proof=(e:any,p:any=plan)=>candidateJupiterExecutionProof(e,p,"plan","snapshot",0,true);
    expect(proof(evidence)).toBe(true);
    expect(candidateJupiterExecutionProof(evidence,plan,"plan","snapshot",0)).toBe(false);
    for(const mutate of [(e:any)=>e.returnCustodyCleared=false,(e:any)=>e.terminalUSDCRaw++,(e:any)=>e.steps[0].custodyAfter[0]=1,(e:any)=>e.steps[1].custodyBefore[1]++,(e:any)=>e.steps[0].custodyAfter[1]=7,(e:any)=>e.steps[1].additionalNegatives.pop(),(e:any)=>e.installedPolicyProof=true,(e:any)=>e.candidateCreation.push(e.candidateCreation[0])]) {
      const bad=structuredClone(evidence);mutate(bad);expect(proof(bad)).toBe(false);
    }
    expect(proof(evidence,{...plan,profile:undefined})).toBe(false);
  });
  test("sound quote diagnostics cannot be reported as compatible when a return edge rejects",()=>{
    const name="TestPhase3ReturnQuoteCompatibility";
    const rows=["USDe->PYUSD","PYUSD->USDC","USDe->USDC"].map(edge=>({edge,artifactSha256:"artifact",accepted:edge==="PYUSD->USDC",reason:edge==="PYUSD->USDC"?"":"layout mismatch"}));
    const encode=(values:any[])=>[...values.map(row=>({Action:"output",Test:name,Output:"probe.go:1: PHASE3_RETURN_QUOTE "+JSON.stringify(row)+"\n"})),{Action:"pass",Test:name},{Action:"pass"}].map(e=>JSON.stringify(e)).join("\n");
    const proof=(values:any[],code=0,hash="artifact")=>returnQuoteCompatibilityProof(encode(values),code,hash);
    expect(proof(rows)).toMatchObject({complete:true,allAccepted:false});
    expect(proof(rows.map(r=>({...r,accepted:true,reason:""})))).toMatchObject({complete:true,allAccepted:true});
    for(const bad of [rows.slice(1),[...rows,rows[0]],rows.map(r=>({...r,accepted:true})),rows.map(r=>({...r,artifactSha256:"stale"}))])expect(proof(bad).complete).toBe(false);
    expect(proof(rows,1).complete).toBe(false);
    expect(returnQuoteCompatibilityProof("",0,"artifact").complete).toBe(false);
  });
  test("candidate evidence cannot impersonate installed proof or omit a rejecting mutation",()=>{
    const mutations=["slippage_above_50_bps","platform_fee_low_byte","platform_fee_high_byte","positive_slippage_fee_low_byte","positive_slippage_fee_high_byte","destination_replaced_by_source"];
    const plan={compiler:"TYPESCRIPT_CANDIDATE_NOT_INSTALLED_GO",broadcast:false,lane:"Ethena/USDe/PYUSD",candidate:{policies:[{policy:"p1",seed:"140"},{policy:"p2",seed:"141"}]},steps:[{wireSha256:"w1"},{wireSha256:"w2"}]};
    const evidence={schema:"phase3-jupiter-candidate-controlled-result/v1",broadcast:false,signatureProof:false,installedPolicyProof:false,goCompilerProof:false,planSha256:"plan",snapshotSha256:"snapshot",twoSwapsPassed:true,
      candidateCreation:plan.candidate.policies.map(p=>({candidateOnly:true,policy:p.policy,seed:Number(p.seed),packetBytes:1100})),
      steps:["SWAP_STABLE_TO_COLLATERAL_STEP","SWAP_COLLATERAL_TO_DEBT_STEP"].map((action,i)=>({action,economicPass:true,error:null,wireSha256:plan.steps[i]!.wireSha256,computeUnits:190000,negative:{rejectedBeforeJupiterCPI:true,custodyUnchanged:true},additionalNegatives:mutations.map(mutation=>({mutation,rejectedBeforeJupiterCPI:true,custodyUnchanged:true}))}))};
    const proof=(e:any)=>candidateJupiterExecutionProof(e,plan,"plan","snapshot",0);
    expect(proof(evidence)).toBe(true);
    for(const change of [{schema:"phase3-jupiter-controlled-result/v1"},{broadcast:true},{signatureProof:true},{installedPolicyProof:true},{goCompilerProof:true},{planSha256:"stale"},{snapshotSha256:"other"},{candidateCreation:[]},{steps:[]}])expect(proof({...evidence,...change})).toBe(false);
    for(const mutate of [(e:any)=>e.steps[0].additionalNegatives.pop(),(e:any)=>e.steps[0].additionalNegatives.push(e.steps[0].additionalNegatives[0]),(e:any)=>e.steps[1].negative.rejectedBeforeJupiterCPI=false,(e:any)=>e.steps[0].additionalNegatives[0].custodyUnchanged=false,(e:any)=>e.steps[1].computeUnits=200001,(e:any)=>e.candidateCreation[0].packetBytes=1233]) {
      const bad=structuredClone(evidence);mutate(bad);expect(proof(bad)).toBe(false);
    }
    expect(candidateJupiterExecutionProof(evidence,plan,"plan","snapshot",1)).toBe(false);
  });
  test("packet coverage distinguishes an oversized witnessed exit from missing or forged measurements",()=>{
    const rows=[...["USDC->AUTO","AUTO->USDC","PYUSD->AUTO","AUTO->PYUSD","USDC->PYUSD","PYUSD->USDC"].map(edge=>({lane:"AUTO/AUTO/PYUSD",edge,packetBytes:900,fits:true})),
      ...["USDC->USDe","USDe->USDC","PYUSD->USDe","USDe->PYUSD","USDC->PYUSD","PYUSD->USDC"].map(edge=>({lane:"Ethena/USDe/PYUSD",edge,packetBytes:edge==="USDe->PYUSD"?1399:900,fits:edge!=="USDe->PYUSD"})),
      ...["USDC->PRIME","PRIME->USDC","PYUSD->PRIME","PRIME->PYUSD","USDC->PYUSD","PYUSD->USDC"].map(edge=>({lane:"Prime/PRIME/PYUSD",edge,packetBytes:900,fits:true})),
      ...["USDC->PRIME","PRIME->USDC","USDS->PRIME","PRIME->USDS","USDC->USDS","USDS->USDC"].map(edge=>({lane:"Prime/PRIME/USDS",edge,packetBytes:900,fits:true}))];
    const encode=(values:unknown[])=>values.map(row=>JSON.stringify({Action:"output",Test:"TestCatalogJupiterInstructionsMatchInstalledEdgesAndRejectMutations/lane/edge",Output:"file.go:1: PHASE3_JUPITER_PACKET "+JSON.stringify(row)+"\n"})).join("\n");
    expect(catalogJupiterPacketProof(encode(rows))).toMatchObject({complete:true,allFit:false});
    expect(catalogJupiterPacketProof(encode(rows.slice(1))).complete).toBe(false);
    expect(catalogJupiterPacketProof(encode([...rows,rows[0]])).complete).toBe(false);
    expect(catalogJupiterPacketProof(encode(rows.map(r=>({...r,fits:true})))).complete).toBe(false);
    expect(catalogJupiterPacketProof("").allFit).toBe(false);
  });
  test("rejects missing, duplicate, extra and substituted lane identities", () => {
    expect(exactSet(["a","b"],["b","a"])).toBe(true);
    for (const candidate of [[],["a"],["a","a"],["a","b","c"],["a","c"],null])
      expect(exactSet(candidate,["a","b"])).toBe(false);
  });
  test("passing observations cannot conceal missing behavioral proof", () => {
    const result = measuredCondition("R01","cap gate",[{claim:"compiled caps",verdict:"PASS",evidence:[1,20,60]}],["production admission"]);
    expect(result.verdict).toBe("FAIL");
    expect(result.measurementStatus).toBe("MEASURED");
    expect(result.missingProof).toEqual(["production admission"]);
  });
  test("distinguishes unavailable observation from a falsified condition", () => {
    const unavailable = {claim:"current state",verdict:"BLOCKED" as const,evidence:"credentials unavailable"};
    expect(measuredCondition("R05","live state",[unavailable],[]).verdict).toBe("BLOCKED");
    expect(measuredCondition("R05","live state",[unavailable,{claim:"image",verdict:"FAIL",evidence:"wrong source"}],[]).verdict).toBe("FAIL");
  });
  test("requires nonempty complete passing measurements", () => {
    expect(measuredCondition("R04","lifecycle",[],[]).verdict).toBe("FAIL");
    expect(measuredCondition("R04","lifecycle",[{claim:"execution",verdict:"PASS",evidence:{}}],[]).verdict).toBe("PASS");
  });
  test("local cap evidence requires all named behavioral tests and a successful package result", () => {
    const names=["TestProductionBridgeRejectsFreshOverCapCostBeforeSignerOrDatabase",
      "TestProductionKaminoAndJupiterRejectFreshOverCapCostBeforeSigner",
      "TestKnownBuildCostRejectsStaleObservationAndDoesNotGrantAdmission",
      "TestPersistedSendInputRevaluesWithoutRebuildingWire",
      "TestPersistedSendInputRejectsIdentityDriftBeforeRPC"];
    const events=[...names.map(Test=>({Action:"pass",Test})),{Action:"pass"}];
    const encode=(rows:unknown[])=>rows.map(row=>JSON.stringify(row)).join("\n");
    expect(localCapTestProof(encode(events),0).pass).toBe(true);
    expect(localCapTestProof(encode(events),1).pass).toBe(false);
    expect(localCapTestProof(encode(events.slice(1)),0).pass).toBe(false);
    expect(localCapTestProof(encode(events.slice(0,-1)),0).pass).toBe(false);
    expect(localCapTestProof(encode([...events,{Action:"pass",Test:names[0]}]),0).pass).toBe(false);
    expect(localCapTestProof(encode([{Action:"skip",Test:names[0]},...events.slice(1)]),0).pass).toBe(false);
    expect(localCapTestProof(encode([...events,{Action:"fail",Test:"nested/negative"}]),0).pass).toBe(false);
    expect(localCapTestProof("",0).pass).toBe(false);
  });
});
