import { describe, expect, test } from "bun:test";
import { exactSet, measuredCondition, localCapTestProof, catalogJupiterPacketProof, candidateJupiterExecutionProof } from "./rwa-phase3-family-activation.js";

describe("Phase 3 measured verifier", () => {
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
      ...["USDC->USDe","USDe->USDC","PYUSD->USDe","USDe->PYUSD","USDC->PYUSD","PYUSD->USDC"].map(edge=>({lane:"Ethena/USDe/PYUSD",edge,packetBytes:edge==="USDe->PYUSD"?1399:900,fits:edge!=="USDe->PYUSD"}))];
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
