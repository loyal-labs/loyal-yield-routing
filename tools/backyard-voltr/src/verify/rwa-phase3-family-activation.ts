import { createHash, randomUUID } from "node:crypto";
import { spawn, spawnSync } from "node:child_process";
import { readFileSync, writeFileSync } from "node:fs";
import { resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { gzipSync } from "node:zlib";
import { Reserve } from "@kamino-finance/klend-sdk";
import { ExtensionType, getExtensionTypes, getTransferFeeConfig, getTransferHook, unpackMint } from "@solana/spl-token";
import { Connection, PublicKey } from "@solana/web3.js";
import { reviewPhase3Bindings } from "./rwa-phase3-binding-review.js";

const ROOT = resolve(fileURLToPath(new URL("../../../..", import.meta.url)));
const CONTRACT = "docs/plans/backyard-rwa-phase3-family-activation-verifier.md";
const ROUTE = "rwa-multiply:ST999VUTo5QExYEX9bz1oDDoKGkjXG9zpphy4Hj7VWh";
const SERVICE = "srv-dabkt0ojo6nc7381o9fg";
const GOAL = "01a06b6c-8023-72b1-ad5d-c97c0662820e";
const CAP_TESTS = [
  "TestProductionBridgeRejectsFreshOverCapCostBeforeSignerOrDatabase",
  "TestProductionKaminoAndJupiterRejectFreshOverCapCostBeforeSigner",
  "TestKnownBuildCostRejectsStaleObservationAndDoesNotGrantAdmission",
  "TestPersistedSendInputRevaluesWithoutRebuildingWire",
  "TestPersistedSendInputRejectsIdentityDriftBeforeRPC",
];
export const EXPECTED_LANES = [
  "Prime/PRIME/USDC", "Prime/PRIME/PYUSD", "Prime/PRIME/USDS",
  "Maple/syrupUSDC/USDC", "Maple/syrupUSDC/USDG", "Maple/syrupUSDC/PYUSD",
  "OnRe/ONyc/USDC", "OnRe/ONyc/USDG", "OnRe/ONyc/USDS",
  "AUTO/AUTO/PYUSD", "Ethena/USDe/PYUSD",
].sort();
type Json = Record<string, any>;
type Observation = { status: "OBSERVED" | "BLOCKED"; source: string; data?: Json; reason?: string };
export type Measurement = { claim: string; verdict: "PASS" | "FAIL" | "BLOCKED"; evidence: unknown };
export function measuredCondition(id: string, condition: string, measurements: Measurement[], missingProof: string[]) {
  // An empty check set is never successful; missing behavior remains visible
  // even when all available observations pass. No percentage implies readiness.
  const failed = measurements.some(m => m.verdict === "FAIL");
  const blocked = measurements.some(m => m.verdict === "BLOCKED");
  const verdict = failed || missingProof.length > 0 || measurements.length === 0 ? "FAIL" : blocked ? "BLOCKED" : "PASS";
  return {id, condition, verdict, measurementStatus: measurements.length ? "MEASURED" : "MISSING_PROOF",
    measurements, missingProof};
}
const measured = (claim: string, pass: boolean, evidence: unknown): Measurement =>
  ({claim, verdict: pass ? "PASS" : "FAIL", evidence});
function observedCheck(observation: Observation, claim: string, predicate: (data: Json) => boolean): Measurement {
  if (observation.status !== "OBSERVED" || !observation.data)
    return {claim, verdict:"BLOCKED", evidence:{source:observation.source,reason:observation.reason}};
  return measured(claim,predicate(observation.data),{source:observation.source,
    reference:"preflight observation with matching source",slot:observation.data.slot});
}
const read = (p: string) => readFileSync(resolve(ROOT, p), "utf8");
const json = (p: string): Json => JSON.parse(read(p));
const sha = (s: string | Uint8Array) => createHash("sha256").update(s).digest("hex");
export function exactSet(actual: unknown, expected: string[]): boolean {
  return Array.isArray(actual) && actual.every((x) => typeof x === "string") &&
    actual.length === expected.length && new Set(actual).size === actual.length &&
    [...actual].sort().every((x, i) => x === [...expected].sort()[i]);
}
export function localCapTestProof(output: string, exitCode: number | null, expectedTests: string[] = CAP_TESTS) {
  const events=output.trim().split("\n").filter(Boolean).map(line=>JSON.parse(line) as Json);
  const terminal=events.filter(e=>expectedTests.includes(e.Test) && ["pass","fail","skip"].includes(e.Action));
  const passed=terminal.filter(e=>e.Action==="pass").map(e=>e.Test);
  return {passedTests:passed,expectedTests,
    pass:expectedTests.length>0 && exitCode===0 && exactSet(passed,expectedTests) && terminal.length===expectedTests.length &&
      !events.some(e=>e.Action==="fail") && events.some(e=>e.Action==="pass" && e.Test===undefined),
    proofLevel:"LOCAL_PRODUCTION_BUILDERS_CONTROLLED_RPC_INPUTS_NOT_LIVE_ADMISSION"};
}
async function localCapObservation(names: string[] = CAP_TESTS, source="local production-builder cap witnesses", packetWitnesses=false, extraEnv: Record<string,string> = {}): Promise<Observation> {
  try {
    const child=spawn("go",["test","./internal/backyardrwa","-json","-race","-count=1","-timeout=60s",
      "-run","^("+names.join("|")+")$"],{
      cwd:resolve(ROOT,"go/backyard-rwa-worker"),stdio:["ignore","pipe","pipe"],env:{...process.env,...extraEnv},
    });
    let output="";
    child.stdout.setEncoding("utf8"); child.stdout.on("data",chunk=>{output+=chunk;});
    child.stderr.resume();
    const deadline=setTimeout(()=>child.kill("SIGKILL"),90_000);
    try {
      const code=await new Promise<number|null>((resolve,reject)=>{child.once("error",reject);child.once("close",resolve);});
      return {status:"OBSERVED",source,data:{...localCapTestProof(output,code,names),
        ...(packetWitnesses?{packets:catalogJupiterPacketProof(output)}:{})}};
    } finally {clearTimeout(deadline);}
  } catch {return {status:"BLOCKED",source,reason:"LOCAL_CAP_WITNESSES_UNAVAILABLE"};}
}
export function catalogJupiterPacketProof(output:string) {
  const expected=[...["USDC->AUTO","AUTO->USDC","PYUSD->AUTO","AUTO->PYUSD","USDC->PYUSD","PYUSD->USDC"].map(e=>"AUTO/AUTO/PYUSD|"+e),
    ...["USDC->USDe","USDe->USDC","PYUSD->USDe","USDe->PYUSD","USDC->PYUSD","PYUSD->USDC"].map(e=>"Ethena/USDe/PYUSD|"+e)];
  const rows=output.trim().split("\n").filter(Boolean).map(line=>JSON.parse(line) as Json)
    .filter(e=>e.Action==="output"&&typeof e.Test==="string"&&e.Test.startsWith("TestCatalogJupiterInstructionsMatchInstalledEdgesAndRejectMutations/")&&typeof e.Output==="string"&&e.Output.includes("PHASE3_JUPITER_PACKET "))
    .map(e=>JSON.parse(e.Output.slice(e.Output.indexOf("PHASE3_JUPITER_PACKET ")+"PHASE3_JUPITER_PACKET ".length).trim()) as Json);
  const complete=exactSet(rows.map(r=>r.lane+"|"+r.edge),expected)&&rows.every(r=>Number.isSafeInteger(r.packetBytes)&&r.packetBytes>65&&r.fits===(r.packetBytes<=1232));
  return {complete,allFit:complete&&rows.every(r=>r.fits),rows,proofLevel:"LOCAL_UNSIGNED_SQUADS_LEGACY_OR_VALIDATED_V0_PACKET_SAMPLES"};
}
async function localJupiterObservation(): Promise<Observation> {
  const result=await localCapObservation(["TestCatalogJupiterInstructionsMatchInstalledEdgesAndRejectMutations","TestWorkerDispatchesNonUSDCConversionsWithoutChangingTheirIdentity",
    "TestVersionedMessageMatchesSDKAndRejectsInvalidLookupAccounts","TestJupiterLookupPreparationAndFinalSendRejectChangedAccounts",
    "TestFreshJupiterLookupHintsPreservePolicyAndPersistedMapping"],
    "AUTO/Ethena installed Jupiter layouts, controlled client/dispatch and packet measurements",true);
  if(result.data)result.data.proofLevel="LOCAL_CONSTRUCTION_CONTROLLED_API_AND_DISPATCH_NOT_PROGRAM_EXECUTION";
  return result;
}
async function localSequentialKaminoObservation(): Promise<Observation> {
  const source="Go production messages executed sequentially through cloned deployed Ethena programs and installed policies";
  const directory=process.env.PHASE3_KAMINO_PROBE_DIR;
  if(!directory)return {status:"BLOCKED",source,reason:"EXPLICIT_PUBLIC_SVM_SNAPSHOT_NOT_CONFIGURED"};
  if(!/^\/private\/tmp\/backyard-phase3-kamino-probe\.[A-Za-z0-9]+$/.test(directory))
    return {status:"BLOCKED",source,reason:"INVALID_PUBLIC_SVM_SNAPSHOT_DIRECTORY"};
  const resultName=`verifier-${randomUUID()}.json`;
  const run=async(command:string,args:string[],cwd:string)=>{
    const child=spawn(command,args,{cwd,stdio:"ignore",env:{...process.env,PHASE3_KAMINO_PROBE_RESULT:resultName}});
    const timeout=setTimeout(()=>child.kill("SIGKILL"),120_000);
    try{return await new Promise<number|null>((resolve,reject)=>{child.once("error",reject);child.once("close",resolve);});}
    finally{clearTimeout(timeout);}
  };
  try{
    const inputs={plan:JSON.parse(readFileSync(resolve(directory,"plan.json"),"utf8")),snapshot:JSON.parse(readFileSync(resolve(directory,"snapshot.json"),"utf8"))};
    const currentCompiler=await localCapObservation(["TestPhase3KaminoProbeMatchesProduction"],"probe messages compared with the current Go compiler");
    if(currentCompiler.status!=="OBSERVED"||currentCompiler.data?.pass!==true)return {status:"OBSERVED",source,data:{pass:false,reason:"PROBE_DOES_NOT_MATCH_CURRENT_GO_COMPILER"}};
    const exitCode=await run("cargo",["test","-p","squads-test-harness","--test","rwa_kamino_controlled_probe","--","--ignored","--nocapture"],ROOT);
    let execution:Json;
    try{execution=JSON.parse(readFileSync(resolve(directory,resultName),"utf8"));}
    catch{return {status:"OBSERVED",source,data:{pass:false,reason:"SVM_EXECUTION_DID_NOT_PRODUCE_EVIDENCE",exitCode}};}
    const repayment=await localCapObservation(["TestPhase3KaminoRepaymentProbeMatchesProduction"],
      "executed finite repayment and dust-rejection wires compared with current Go compiler, maximum-debit pricing and reconciliation",false,
      {PHASE3_KAMINO_PROBE_RESULT:resultName});
    const pass=exitCode===0&&execution.fourKaminoLegsPassed===true&&execution.negative?.rejectedBeforeKaminoCPI===true&&
      execution.boundedRepaymentProofPassed===true&&repayment.status==="OBSERVED"&&repayment.data?.pass===true&&
      execution.planSha256===sha(readFileSync(resolve(directory,"plan.json")))&&execution.snapshotSha256===sha(readFileSync(resolve(directory,"snapshot.json")));
    return {status:"OBSERVED",source,data:{pass,slot:execution.slot,proofLevel:"CONTROLLED_FOUR_KAMINO_LEGS_NOT_FULL_LIFECYCLE_OR_SIGNER_PROOF",inputs,execution,repayment}};
  }catch{return {status:"BLOCKED",source,reason:"LOCAL_SEQUENTIAL_KAMINO_PROBE_UNAVAILABLE"};}
}
async function localSendJournalObservation(): Promise<Observation> {
  const source="disposable PostgreSQL final-send and expiry witnesses";
  if (!process.env.PHASE3_TEST_DATABASE_URL) return {status:"BLOCKED",source,reason:"DISPOSABLE_TEST_DATABASE_NOT_CONFIGURED"};
  // The selected test itself rejects any non-disposable connection. Production
  // DB credentials are never used to initialize or mutate this test fixture.
  const result=await localCapObservation(["TestPhase3DatabaseAdmissionAndSendFence"],source);
  if (result.data) result.data.proofLevel="LOCAL_DATABASE_AND_CONTROLLED_RPC_UNSIGNED_FIXTURE_NO_SIGNER_OR_SEND";
  return result;
}

async function localSequentialJupiterObservation(): Promise<Observation> {
  const source="two Go Jupiter swaps executed sequentially through cloned deployed programs and installed policies";
  const directory=process.env.PHASE3_JUPITER_PROBE_DIR;
  if(!directory)return {status:"BLOCKED",source,reason:"EXPLICIT_PUBLIC_JUPITER_SNAPSHOT_NOT_CONFIGURED"};
  if(!/^\/private\/tmp\/backyard-phase3-jupiter-probe\.[A-Za-z0-9]+$/.test(directory))return {status:"BLOCKED",source,reason:"INVALID_PUBLIC_JUPITER_SNAPSHOT_DIRECTORY"};
  try {
    const inputs={plan:JSON.parse(readFileSync(resolve(directory,"plan.json"),"utf8")),snapshot:JSON.parse(readFileSync(resolve(directory,"snapshot.json"),"utf8"))};
    const compiler=await localCapObservation(["TestPhase3JupiterProbeMatchesProduction"],"Jupiter probe compared with current Go compiler");
    if(compiler.status!=="OBSERVED"||compiler.data?.pass!==true)return {status:"OBSERVED",source,data:{pass:false,reason:"JUPITER_PROBE_COMPILER_MISMATCH"}};
    const resultName=`verifier-${randomUUID()}.json`;
    const child=spawn("cargo",["test","-p","squads-test-harness","--test","rwa_jupiter_controlled_probe","ethena_go_swaps_execute_sequentially_under_deployed_policies","--","--ignored","--nocapture"],{
      cwd:ROOT,stdio:"ignore",env:{...process.env,PHASE3_JUPITER_PROBE_RESULT:resultName},
    });
    const deadline=setTimeout(()=>child.kill("SIGKILL"),120_000);
    let code:number|null;
    try {code=await new Promise<number|null>((resolve,reject)=>{child.once("error",reject);child.once("close",resolve);});}
    finally {clearTimeout(deadline);}
    let execution:Json;
    try {execution=JSON.parse(readFileSync(resolve(directory,resultName),"utf8"));}
    catch {return {status:"OBSERVED",source,data:{pass:false,reason:"JUPITER_EXECUTION_DID_NOT_PRODUCE_EVIDENCE",exitCode:code}};}
    const pass=code===0&&execution.schema==="phase3-jupiter-controlled-result/v1"&&execution.broadcast===false&&execution.signatureProof===false&&
      execution.twoSwapsPassed===true&&execution.steps?.length===2&&execution.steps.every((s:Json)=>s.economicPass===true&&s.error===null&&s.negative?.rejectedBeforeJupiterCPI===true)&&
      execution.planSha256===sha(readFileSync(resolve(directory,"plan.json")))&&execution.snapshotSha256===sha(readFileSync(resolve(directory,"snapshot.json")));
    return {status:"OBSERVED",source,data:{pass,proofLevel:"TWO_SWAP_PROGRAM_EXECUTION_NOT_FULL_R04_LIFECYCLE",inputs,execution}};
  } catch {return {status:"BLOCKED",source,reason:"LOCAL_SEQUENTIAL_JUPITER_PROBE_UNAVAILABLE"};}
}

async function localJupiterRepairObservation(): Promise<Observation> {
  const source="current TypeScript Jupiter constraint compiler and deployed Squads candidate PolicyCreate";
  try {
    const candidates=json("docs/evidence/backyard-rwa-go/phase3/jupiter-v2-repair-candidates-2026-09-04.json");
    const quotes=json("docs/evidence/backyard-rwa-go/phase3/jupiter-v2-public-quotes-2026-09-04.json");
    const run=async(command:string,args:string[],cwd:string)=>{
      const child=spawn(command,args,{cwd,stdio:["ignore","pipe","pipe"],env:process.env});
      let output="";
      for(const stream of [child.stdout,child.stderr]) {stream.setEncoding("utf8");stream.on("data",chunk=>{output+=chunk;});}
      const deadline=setTimeout(()=>child.kill("SIGKILL"),90_000);
      try{return {code:await new Promise<number|null>((resolve,reject)=>{child.once("error",reject);child.once("close",resolve);}),output};}
      finally{clearTimeout(deadline);}
    };
    const tests=await run("bun",["test","src/policies/rwa-multiply-jupiter-constraint.test.ts","src/policies/rwa-multiply-custodies.test.ts"],resolve(ROOT,"tools/backyard-voltr"));
    const compilerPass=tests.code===0&&/\b10 pass\b/.test(tests.output)&&/\b0 fail\b/.test(tests.output);
    if(!compilerPass)return {status:"OBSERVED",source,data:{pass:false,reason:"JUPITER_V2_COMPILER_REGRESSION",compilerPass}};
    if(!process.env.SQUADS_SMART_ACCOUNT_PROGRAM_SO)return {status:"OBSERVED",source,data:{pass:false,compilerPass,reason:"DEPLOYED_SQUADS_BINARY_NOT_CONFIGURED",candidates,quotes}};
    const result=await run("cargo",["test","-p","squads-test-harness","--test","rwa_policy_creation_rent","jupiter_v2_repair_groups","--","--ignored","--nocapture"],ROOT);
    const line=result.output.split("\n").find(l=>l.startsWith("PHASE3_JUPITER_V2_POLICY "));
    const execution=line?JSON.parse(line.slice("PHASE3_JUPITER_V2_POLICY ".length)):null;
    const pass=result.code===0&&execution?.broadcast===false&&execution?.installed===false&&execution?.rows?.length===2&&
      execution.candidateSha256===sha(read("docs/evidence/backyard-rwa-go/phase3/jupiter-v2-repair-candidates-2026-09-04.json"));
    return {status:"OBSERVED",source,data:{pass,compilerPass,candidates,quotes,execution,proofLevel:"LOCAL_CANDIDATE_COMPILATION_AND_CREATION_NOT_SWAP_EXECUTION_INSTALLATION_OR_MAINNET_RENT"}};
  } catch {return {status:"BLOCKED",source,reason:"JUPITER_V2_REPAIR_PROBE_UNAVAILABLE"};}
}

export function candidateJupiterExecutionProof(execution:Json,plan:Json,planHash:string,snapshotHash:string,exitCode:number|null):boolean {
  const mutations=["slippage_above_50_bps","platform_fee_low_byte","platform_fee_high_byte",
    "positive_slippage_fee_low_byte","positive_slippage_fee_high_byte","destination_replaced_by_source"];
  const actions=["SWAP_STABLE_TO_COLLATERAL_STEP","SWAP_COLLATERAL_TO_DEBT_STEP"];
  return exitCode===0&&execution.schema==="phase3-jupiter-candidate-controlled-result/v1"&&
    execution.broadcast===false&&execution.signatureProof===false&&execution.installedPolicyProof===false&&execution.goCompilerProof===false&&
    plan.compiler==="TYPESCRIPT_CANDIDATE_NOT_INSTALLED_GO"&&plan.broadcast===false&&plan.lane==="Ethena/USDe/PYUSD"&&
    execution.planSha256===planHash&&execution.snapshotSha256===snapshotHash&&execution.twoSwapsPassed===true&&
    Array.isArray(plan.candidate?.policies)&&plan.candidate.policies.length===2&&
    Array.isArray(execution.candidateCreation)&&execution.candidateCreation.length===2&&execution.candidateCreation.every((c:Json,i:number)=>
      c.candidateOnly===true&&c.policy===plan.candidate.policies[i].policy&&c.seed===Number(plan.candidate.policies[i].seed)&&
      Number.isSafeInteger(c.packetBytes)&&c.packetBytes>65&&c.packetBytes<=1232)&&
    Array.isArray(execution.steps)&&execution.steps.length===2&&execution.steps.every((s:Json,i:number)=>
      s.action===actions[i]&&s.economicPass===true&&s.error===null&&s.wireSha256===plan.steps?.[i]?.wireSha256&&
      Number.isSafeInteger(s.computeUnits)&&s.computeUnits>0&&s.computeUnits<=200_000&&
      s.negative?.rejectedBeforeJupiterCPI===true&&s.negative?.custodyUnchanged===true&&Array.isArray(s.additionalNegatives)&&
      exactSet(s.additionalNegatives.map((n:Json)=>n.mutation),mutations)&&s.additionalNegatives.every((n:Json)=>n.rejectedBeforeJupiterCPI===true&&n.custodyUnchanged===true));
}

async function localCandidateJupiterObservation():Promise<Observation> {
  const source="local V2 candidate PolicyCreate and sequential swaps, with conditional current-Go wire comparison";
  const directory=process.env.PHASE3_JUPITER_CANDIDATE_PROBE_DIR;
  if(!directory)return {status:"BLOCKED",source,reason:"EXPLICIT_PUBLIC_JUPITER_CANDIDATE_SNAPSHOT_NOT_CONFIGURED"};
  if(!/^\/private\/tmp\/backyard-phase3-jupiter-probe\.[A-Za-z0-9]+$/.test(directory))return {status:"BLOCKED",source,reason:"INVALID_CANDIDATE_SNAPSHOT_DIRECTORY"};
  try {
    const planBytes=readFileSync(resolve(directory,"plan.json"));const snapshotBytes=readFileSync(resolve(directory,"snapshot.json"));
    const inputs={plan:JSON.parse(planBytes.toString()),snapshot:JSON.parse(snapshotBytes.toString())};
    const resultName=`verifier-candidate-${randomUUID()}.json`;
    const run=async(command:string,args:string[],cwd:string)=>{
      const child=spawn(command,args,{cwd,stdio:["ignore","pipe","ignore"],env:{...process.env,PHASE3_JUPITER_PROBE_RESULT:resultName}});
      let output="";child.stdout.setEncoding("utf8");child.stdout.on("data",chunk=>{output+=chunk;});
      const deadline=setTimeout(()=>child.kill("SIGKILL"),120_000);
      try {const code=await new Promise<number|null>((resolve,reject)=>{child.once("error",reject);child.once("close",resolve);});return {code,output};}
      finally {clearTimeout(deadline);}
    };
    const rust=await run("cargo",["test","-p","squads-test-harness","--test","rwa_jupiter_controlled_probe","ethena_v2_candidate_swaps_execute_sequentially","--","--ignored","--nocapture"],ROOT);
    let execution:Json;
    try {execution=JSON.parse(readFileSync(resolve(directory,resultName),"utf8"));}
    catch {return {status:"OBSERVED",source,data:{pass:false,reason:"CANDIDATE_EXECUTION_EVIDENCE_MISSING",exitCode:rust.code}};}
    const executionPass=candidateJupiterExecutionProof(execution,inputs.plan,sha(planBytes),sha(snapshotBytes),rust.code)&&
      inputs.plan.candidate.artifactSha256===sha(read("docs/evidence/backyard-rwa-go/phase3/jupiter-v2-repair-candidates-2026-09-04.json"));
    const names=["TestCandidateJupiterV2FixedPrefixAndClient","TestPhase3JupiterCandidateMatchesGo"];
    const go=await run("go",["test","./internal/backyardrwa","-json","-race","-count=1","-timeout=60s","-run","^("+names.join("|")+")$"],resolve(ROOT,"go/backyard-rwa-worker"));
    const compiler={...localCapTestProof(go.output,go.code,names),proofLevel:"CURRENT_GO_BYTES_WITH_TEST_ONLY_CANDIDATE_BINDINGS_NOT_INSTALLED_POLICY_PROOF"};
    return {status:"OBSERVED",source,data:{pass:executionPass&&compiler.pass,executionPass,compiler,slot:execution.slot,inputs,execution,
      proofLevel:"LOCAL_CANDIDATE_TWO_SWAP_EXECUTION_AND_CONDITIONAL_GO_PARITY_NOT_INSTALLED_OR_FULL_R04"}};
  } catch {return {status:"BLOCKED",source,reason:"LOCAL_CANDIDATE_JUPITER_PROBE_UNAVAILABLE"};}
}
async function localBridgeAdmissionObservation(): Promise<Observation> {
  const result=await localCapObservation([
    "TestBridgeAdmissionMeasuresCompleteCashReturnWithoutSigner",
    "TestBridgeAdmissionRejectsUnpricedExposureAndFullSweepCap",
    "TestBridgeAdmissionReturnGraphConsumesReservedBudget",
    "TestTickRecordsBeforeBridgeBuildAndDispatchesExactAction",
  ],"production bridge admission, cash-only return pricing and worker rejection ordering");
  if(result.data)result.data.proofLevel="LOCAL_CONTROLLED_RPC_BRIDGE_RETURN_ACCOUNTING_NOT_EXECUTED_LIFECYCLE";
  return result;
}
async function localWithdrawalAdmissionObservation(): Promise<Observation> {
  const result=await localCapObservation([
    "TestWithdrawalAdmissionPricesCompleteCrossProtocolReturn",
    "TestWithdrawalAdmissionRejectsUnsafeOrIncompleteReturn",
    "TestWithdrawalReturnAdmissionContinuesThroughNAVSwapAndBridge",
    "TestDebtFreeReturnReservesBothCollateralAndDebtResidue",
    "TestDebtResidueAdmissionContinuesFromNAVThroughActualSwap",
    "TestTickDispatchesKaminoAndReobservesAfterReconciliation",
  ],"debt-free withdrawal, collateral/debt-residue quotes and complete return admission through production paths");
  if(result.data)result.data.proofLevel="CONTROLLED_RPC_QUOTE_AND_POSTSTATE_ACCOUNTING_NOT_EXECUTED_RETURN";
  return result;
}
async function localKaminoConstructionObservation(): Promise<Observation> {
  const result=await localCapObservation(["TestCatalogKaminoConstructionMatchesRetainedAUTOAndEthena"],
    "AUTO/Ethena unsigned Kamino construction against retained SDK account vectors");
  if (result.data) {
    result.data.proofLevel="LOCAL_UNSIGNED_CONSTRUCTION_AND_MUTATIONS_NOT_PROGRAM_EXECUTION";
    result.data.lanes=["AUTO/AUTO/PYUSD","Ethena/USDe/PYUSD"];
    result.data.operations=["deposit","borrow","repay","withdraw"];
    result.data.retainedEvidence="docs/evidence/backyard-rwa-go/phase3/setup-feasibility-2026-09-04.json";
  }
  return result;
}
async function localDebtDecisionObservation(): Promise<Observation> {
  const result=await localCapObservation([
    "TestNonUSDCLifecycleDecisionsKeepDebtAndBridgeCashSeparate",
    "TestNonUSDCLifecycleSafetyPrecedence",
    "TestFixedAccountObservationPreservesDecimalsAndUSDCEntryCapacity",
    "TestKaminoAccruedDebtUsesUnroundedFractionAndFullRateLimbs",
    "TestAccruedDebtFlowsThroughObservationNAVAndRepayment",
    "TestBoundedKaminoRepaymentReconcilesActualDebitWithoutClaimingPayoff",
    "TestBoundedKaminoRepaymentReservesWireMaximumAndRejectsWeakenedEffects",
  ],"non-USDC lifecycle decisions and fixed-account valuation with controlled inputs");
  if (result.data) result.data.proofLevel="LOCAL_PRODUCTION_DECISIONS_AND_ACCOUNT_DECODING_NOT_EXECUTED_LIFECYCLE";
  return result;
}
async function rpc(method: string, params: unknown[] = []): Promise<any> {
  const endpoint = process.env.SOLANA_RPC_URL;
  if (!endpoint) throw new Error("RPC_CREDENTIAL_MISSING");
  const response = await fetch(endpoint, {
    method: "POST", headers: { "content-type": "application/json" },
    body: JSON.stringify({ jsonrpc: "2.0", id: 1, method, params }),
    signal: AbortSignal.timeout(30_000),
  });
  if (!response.ok) throw new Error("RPC_HTTP_" + response.status);
  const payload = await response.json() as Json;
  if (payload.error || payload.result === undefined) throw new Error("RPC_RESPONSE_FAILED");
  return payload.result;
}
async function chainObservation(catalog: Json, manifest: Json): Promise<Observation> {
  try {
    const genesis = await rpc("getGenesisHash");
    if (genesis !== manifest.genesisHash) return {status:"BLOCKED", source:"Solana RPC", reason:"GENESIS_MISMATCH"};
    const mintAddresses = [...new Set<string>(catalog.lanes.flatMap((l: Json) =>
      [l.candidateIdentity.collateralMint, l.candidateIdentity.debtMint]))];
    const addresses = [...new Set<string>([
      ...mintAddresses, manifest.identities.squadsSettings,
      manifest.identities.voltrVault, manifest.identities.v2StrategyConfig,
      manifest.identities.squadsUsdcAta, manifest.identities.delegatedExecutor,
      ...catalog.lanes.flatMap((l: Json) => [l.candidateIdentity.collateralReserve,l.candidateIdentity.debtReserve]),
    ])];
    const result = await rpc("getMultipleAccounts", [addresses, {commitment:"finalized",encoding:"base64"}]);
    if (result.value.length !== addresses.length) throw new Error("RPC_ACCOUNT_COUNT");
    const reserveAddresses = catalog.lanes.flatMap((l: Json) => [l.candidateIdentity.collateralReserve,l.candidateIdentity.debtReserve]);
    const accounts = result.value.map((account: Json | null, i: number) => {
      const address = addresses[i];
      if (!address) throw new Error("RPC_ACCOUNT_ADDRESS_MISSING");
      if (!account) return {address:addresses[i], present:false};
      const bytes = Buffer.from(account.data[0], "base64");
      let mint: Json = {};
      let reserve: Json = {};
      if (mintAddresses.includes(address)) {
        const decoded = unpackMint(new PublicKey(address), {
          data:bytes,owner:new PublicKey(account.owner),executable:account.executable,lamports:account.lamports,
        },new PublicKey(account.owner));
        const hook = getTransferHook(decoded);
        const fees = getTransferFeeConfig(decoded);
        const feeSummary = (fee: NonNullable<typeof fees>["olderTransferFee"]) => ({
          epoch:fee.epoch.toString(),maximumFeeRaw:fee.maximumFee.toString(),basisPoints:fee.transferFeeBasisPoints,
        });
        mint = {mintDecimals:decoded.decimals,mintInitialized:decoded.isInitialized,
          extensions:getExtensionTypes(decoded.tlvData).map(type => ({type,name:ExtensionType[type] ?? "UNMAPPED_SDK_ENUM"})),
          transferHook:hook ? {authority:hook.authority.toBase58(),programId:hook.programId.toBase58(),
            enabled:!hook.programId.equals(PublicKey.default)} : null,
          transferFees:fees ? {older:feeSummary(fees.olderTransferFee),newer:feeSummary(fees.newerTransferFee)} : null};
      }
      if (reserveAddresses.includes(addresses[i])) {
        if (account.owner !== "KLend2g3cP87fffoy8q1mQqGKjrxjC8boSyAYavgmjD") throw new Error("RESERVE_OWNER_MISMATCH");
        const decoded = Reserve.decode(bytes);
        const sf = 1n << 60n;
        const borrowed = (BigInt(decoded.liquidity.borrowedAmountSf.toString())+sf-1n)/sf;
        const available = BigInt(decoded.liquidity.availableAmount.toString());
        const depositLimit = BigInt(decoded.config.depositLimit.toString());
        const borrowLimit = BigInt(decoded.config.borrowLimit.toString());
        reserve = {reserve:{status:Number(decoded.config.status),mint:String(decoded.liquidity.mintPubkey),
          mintDecimals:String(decoded.liquidity.mintDecimals),marketPriceSf:decoded.liquidity.marketPriceSf.toString(),
          availableRaw:available.toString(),borrowedRawCeil:borrowed.toString(),
          depositHeadroomRaw:(depositLimit>available+borrowed?depositLimit-available-borrowed:0n).toString(),
          borrowHeadroomRaw:(borrowLimit>borrowed?borrowLimit-borrowed:0n).toString(),
          loanToValuePct:Number(decoded.config.loanToValuePct),liquidationThresholdPct:Number(decoded.config.liquidationThresholdPct),
          utilizationLimitBlockBorrowingAbovePct:Number(decoded.config.utilizationLimitBlockBorrowingAbovePct)}};
      }
      return {
        address:addresses[i], present:true, owner:account.owner, executable:account.executable,
        lamports:String(account.lamports), dataLength:bytes.length, dataSha256:sha(bytes),
        ...mint,...reserve,
        ...(addresses[i] === manifest.identities.squadsUsdcAta && bytes.length >= 165
          ? {tokenAmountRaw:bytes.readBigUInt64LE(64).toString()} : {}),
      };
    });
    return {status:"OBSERVED",source:"fresh finalized Solana accounts",data:{genesis,slot:result.context.slot,accounts}};
  } catch {
    // Fetch/driver errors can contain credential-bearing URLs; never print them.
    return {status:"BLOCKED",source:"Solana RPC",reason:"RPC_READ_UNAVAILABLE"};
  }
}
async function databaseObservation(): Promise<Observation> {
  const connection = process.env.NEON_DATABASE_URL;
  if (!connection) return {status:"BLOCKED",source:"Postgres",reason:"DATABASE_CREDENTIAL_MISSING"};
  try {
    const u = new URL(connection);
    const sql = `SELECT json_build_object(
      'route', (SELECT json_build_object('routeKey',route_key,'leaseOwner',lease_owner,
        'leaseLive',lease_expires_at>clock_timestamp(),'fencingToken',fencing_token,
        'stateVersion',state_version,'phase3',state->'phase3')
        FROM loyal_yield.multiply_route_states WHERE route_key='${ROUTE}'),
      'nonterminal', (SELECT COALESCE(json_agg(json_build_object(
        'operationId',operation_id,'status',status,'action',action,'strategyKey',strategy_key,
        'signature',transaction_signature,'createdAt',created_at)), '[]'::json)
        FROM loyal_yield.multiply_operations WHERE route_key='${ROUTE}'
        AND status IN ('decided','built','simulated','signed','broadcast_intent','submitted','confirmed','reconciling')),
      'phase3OperationCount', (SELECT count(*) FROM loyal_yield.multiply_operations
        WHERE route_key='${ROUTE}' AND expected_effects->'phase3'->>'goalId'='${GOAL}'),
      'latestOperation', (SELECT json_build_object('operationId',operation_id,
        'status',status,'action',action,'createdAt',created_at)
        FROM loyal_yield.multiply_operations WHERE route_key='${ROUTE}'
        ORDER BY created_at DESC LIMIT 1)
    )`;
    const child = spawn("psql",["-X","-q","-A","-t","-v","ON_ERROR_STOP=1","-c",sql], {
      cwd:ROOT, stdio:["ignore","pipe","pipe"],
      env:{...process.env,PGHOST:u.hostname,PGPORT:u.port||"5432",PGUSER:decodeURIComponent(u.username),
        PGPASSWORD:decodeURIComponent(u.password),PGDATABASE:u.pathname.slice(1),
        PGSSLMODE:"require",PGCONNECT_TIMEOUT:"10",
        PGOPTIONS:"-c default_transaction_read_only=on -c statement_timeout=30000"},
    });
    let output = "";
    child.stdout.setEncoding("utf8");
    child.stdout.on("data", chunk => { output += chunk; });
    // Drain but never expose credential-bearing driver errors.
    child.stderr.resume();
    const deadline = setTimeout(() => child.kill(), 35_000);
    let code: number | null;
    try {
      code = await new Promise<number | null>((resolve, reject) => {
        child.once("error", reject);
        child.once("close", resolve);
      });
    } finally { clearTimeout(deadline); }
    if (code !== 0) throw new Error("DATABASE_READ_FAILED");
    return {status:"OBSERVED",source:"fresh read-only Postgres snapshot",data:JSON.parse(output)};
  } catch {
    return {status:"BLOCKED",source:"Postgres",reason:"DATABASE_READ_UNAVAILABLE"};
  }
}
async function deploymentObservation(): Promise<Observation> {
  const key = process.env.RENDER_API_KEY;
  if (!key) return {status:"BLOCKED",source:"Render",reason:"RENDER_CREDENTIAL_MISSING"};
  try {
    const r = await fetch("https://api.render.com/v1/services/"+SERVICE+"/deploys?limit=5", {
      headers:{Authorization:"Bearer "+key},signal:AbortSignal.timeout(30_000),
    });
    if (!r.ok) throw new Error("RENDER_READ_FAILED");
    const payload = await r.json() as Json[];
    return {status:"OBSERVED",source:"Render deploy API",data:{serviceId:SERVICE,
      deploys:payload.map((row) => {const d=row.deploy ?? row;return {
        id:d.id,status:d.status,createdAt:d.createdAt,finishedAt:d.finishedAt,
        commit:d.commit?.id,image:d.image?.ref,digest:d.image?.digest,
      };}),
    }};
  } catch {
    return {status:"BLOCKED",source:"Render",reason:"RENDER_READ_UNAVAILABLE"};
  }
}
async function runtimeObservation(): Promise<Observation> {
  return workerInspection(["--inspect-phase3",...EXPECTED_LANES],
    "loyal-backyard-rwa-phase3-runtime-inspection/v1", "local compiled production route resolver",
    data => exactSet(data.lanes?.map((row: Json) => row.lane),EXPECTED_LANES));
}
async function setupRentObservation(): Promise<Observation> {
  if (!process.env.SOLANA_RPC_URL) return {status:"BLOCKED",source:"setup rent feasibility",reason:"RPC_CREDENTIAL_MISSING"};
  return workerInspection(["--inspect-phase3-setup-rent"],
    "loyal-backyard-rwa-phase3-setup-rent-inspection/v1", "setup rent feasibility",
    data => Number.isSafeInteger(data.slot) && data.slot > 0 && Array.isArray(data.policies) && data.policies.length === 2);
}
async function workerInspection(args: string[], schema: string, source: string, validate: (data: Json) => boolean): Promise<Observation> {
  // Run the same command package as the worker, in an explicitly read-only
  // mode before runtime config/signers are loaded. Do not inspect source text
  // and call the presence of a function proof that production invokes it.
  try {
    const child = spawn("go",["run","./cmd/backyard-rwa-worker",...args], {
      cwd:resolve(ROOT,"go/backyard-rwa-worker"),stdio:["ignore","pipe","pipe"],
    });
    let output = "";
    child.stdout.setEncoding("utf8");
    child.stdout.on("data",chunk => { output += chunk; });
    child.stderr.resume();
    const deadline = setTimeout(() => child.kill("SIGKILL"),180_000);
    try {
      const code = await new Promise<number | null>((resolve,reject) => {
        child.once("error",reject); child.once("close",resolve);
      });
      if (code !== 0) throw new Error("RUNTIME_INSPECTION_FAILED");
      const data = JSON.parse(output) as Json;
      if (data.schema !== schema || data.readOnly !== true || !validate(data)) throw new Error("RUNTIME_INSPECTION_INVALID");
      return {status:"OBSERVED",source,data};
    } finally { clearTimeout(deadline); }
  } catch { return {status:"BLOCKED",source,reason:"RUNTIME_INSPECTION_UNAVAILABLE"}; }
}
async function bindingObservation(): Promise<Observation> {
  if (!process.env.SOLANA_RPC_URL) return {status:"BLOCKED",source:"binding review",reason:"RPC_CREDENTIAL_MISSING"};
  try {
    const connection=new Connection(process.env.SOLANA_RPC_URL,{
      commitment:"finalized",disableRetryOnRateLimit:true,
      fetch:(input,init)=>fetch(input,{...init,signal:AbortSignal.timeout(30_000)}),
    });
    return {status:"OBSERVED",source:"fresh accounts compared with retained installed-policy bindings",data:await reviewPhase3Bindings(connection)};
  } catch {
    return {status:"BLOCKED",source:"binding review",reason:"BINDING_REVIEW_UNAVAILABLE_OR_INVALID"};
  }
}
function sourceIdentity() {
  const git = (args: string[]) => {
    const result=spawnSync("git",args,{cwd:ROOT,encoding:"utf8",timeout:10_000,maxBuffer:2*1024*1024});
    if (result.status!==0 || result.error) throw new Error("SOURCE_IDENTITY_UNAVAILABLE");
    return result.stdout;
  };
  const paths=[...new Set(git(["ls-files","-z","--cached","--others","--exclude-standard","--",
    "go/backyard-rwa-worker","tools/backyard-voltr/src","tools/backyard-voltr/package.json",
    "docs/manifests/backyard-rwa-v1.json","crates/loyal-actions/fixtures/backyard_rwa_policy_catalog_v1.json","bun.lock",
    "docs/evidence/backyard-rwa-go/phase3/setup-feasibility-2026-09-04.json",
    "docs/evidence/backyard-rwa-go/policy-compiled-v1.json","docs/evidence/backyard-rwa-go/policy-install-readback-v1.json",
    "docs/evidence/backyard-rwa-go/policy-jupiter-headers-v1.json",
    "docs/evidence/backyard-rwa-go/phase3/jupiter-lookup-accounts-2026-09-04.json",
    "docs/evidence/backyard-rwa-go/phase3/jupiter-v2-public-quotes-2026-09-04.json",
    "docs/evidence/backyard-rwa-go/phase3/jupiter-v2-repair-candidates-2026-09-04.json",
    "crates/squads-test-harness/tests/rwa_policy_creation_rent.rs",
    "crates/squads-test-harness/tests/support/rwa_jupiter_candidate.rs",
    "crates/squads-test-harness/tests/rwa_kamino_controlled_probe.rs","crates/squads-test-harness/tests/rwa_jupiter_controlled_probe.rs","crates/squads-test-harness/Cargo.toml","Cargo.lock",
  ]).split("\0").filter(Boolean))].sort();
  const files=paths.map(path=>({path,sha256:sha(read(path))}));
  return {head:git(["rev-parse","HEAD"]).trim(),
    dirty:git(["status","--porcelain"]).trim().length>0,
    implementationSha256:sha(JSON.stringify(files)),fileCount:files.length,
    proofLevel:"LOCAL_SOURCE_SNAPSHOT_NOT_DEPLOYMENT"};
}
export async function verify() {
  const source=sourceIdentity();
  const manifest = json("docs/manifests/backyard-rwa-v1.json");
  const catalog = json("crates/loyal-actions/fixtures/backyard_rwa_policy_catalog_v1.json");
  const catalogLanes = catalog.lanes.map((l: Json) => [l.market,l.collateral,l.debt].join("/"));
  const active = manifest.runtimeActivation?.runtimeRoutes ?? [];
  const offline = process.argv.includes("--offline");
  const [runtime,localCaps,localSendJournal,localBridgeAdmission,localWithdrawalAdmission,localKaminoConstruction,localDebtDecisions,localJupiter,localSequentialKamino,localSequentialJupiter,localJupiterRepair,localCandidateJupiter] = await Promise.all([
    runtimeObservation(),localCapObservation(),localSendJournalObservation(),localBridgeAdmissionObservation(),localWithdrawalAdmissionObservation(),localKaminoConstructionObservation(),localDebtDecisionObservation(),localJupiterObservation(),localSequentialKaminoObservation(),localSequentialJupiterObservation(),localJupiterRepairObservation(),localCandidateJupiterObservation()]);
  const bindings: Observation = offline ? {status:"BLOCKED",source:"binding review",reason:"OFFLINE_DIAGNOSTIC"} : await bindingObservation();
  const [chain,database,deployment,setupRent] = offline
    ? ["Solana RPC","Postgres","Render","setup rent feasibility"].map(source => ({status:"BLOCKED" as const,source,reason:"OFFLINE_DIAGNOSTIC"}))
    : await Promise.all([chainObservation(catalog,manifest),databaseObservation(),deploymentObservation(),setupRentObservation()]);
  const retained = json("docs/evidence/backyard-rwa-go/phase2-runtime/lifecycle-v1.json");
  const conditions = [
    measuredCondition("R01","Production admission/send cap enforcement and reserved exits",[
      observedCheck(runtime,"compiled goal identity and cap constants",d => d.goalId === GOAL &&
        JSON.stringify(d.capsMicros) === JSON.stringify([1_000_000,20_000_000,60_000_000])),
      observedCheck(database,"durable budget exists for this goal",d => d.route?.phase3?.goalId === GOAL),
      observedCheck(localCaps,"local production builders reject fresh over-cap costs before signing and reject stale valuation",d=>d.pass===true),
      observedCheck(localBridgeAdmission,"cash-only bridge admission prices staging, full restoration and each required NAV; rejects unsupported exposure and prevents build after HOLD",d=>d.pass===true),
      observedCheck(localWithdrawalAdmission,"debt-free withdrawal, intermediate NAV, full collateral/debt-residue conversions and aggregate bridge return receive measured admission; outstanding debt and over-cap full returns reject",d=>d.pass===true),
      observedCheck(localSendJournal,"local journal persists producer-measured bridge and debt-free withdrawal costs, preserves concurrency/restart binding, reprices before send, and releases signed HOLD only after proven expiry/absence",d=>d.pass===true),
    ],[
      "Production admission for entry, borrowing, debt-bearing repayment/conversion graphs, setup and safe one-time budget initialization; debt-free return admission does not cover those shapes.",
      "Complete admission/send witnesses beyond local controlled-input build rejection: concurrency, restart, ambiguity, final-send freshness and successful reserved unwind.",
      "Independent reconciliation of deployed spent/reserved accounting, including setup and full-custody restore.",
    ]),
    measuredCondition("R02","Exact eleven-lane runtime allowlist and frozen three-family queue",[
      measured("exact catalog lane set",exactSet(catalogLanes,EXPECTED_LANES),{catalogLanes}),
      measured("exact manifest runtime lane set",exactSet(active,EXPECTED_LANES),{runtimeRoutes:active}),
      observedCheck(runtime,"every catalogued lane resolves through production",d => d.lanes.every((row: Json) => row.resolved === true)),
    ],["Durable three-family queue with reviewed identity bindings and in-family-only flat substitution behavior."]),
    measuredCondition("R03","Shared debt/token/valuation/exit runtime and existing policy authority",[
      observedCheck(localJupiterRepair,"V2 candidate compiler rejects economic/custody mutations, preserves all 52 legacy constraints and sibling edges, and full replacement groups create under the deployed Squads binary",d=>d.pass===true),
      observedCheck(localKaminoConstruction,"AUTO/Ethena four-leg unsigned construction matches retained SDK vectors and rejects account/policy substitutions",d=>d.pass===true),
      observedCheck(localDebtDecisions,"non-USDC planner separates debt/bridge custody and observes accrued debt; finite repayment bounds preserve custody conservation and charge the wire maximum without claiming payoff",d=>d.pass===true),
      observedCheck(localJupiter,"AUTO/Ethena Jupiter layouts match installed edge constraints and controlled worker dispatch preserves conversion identities",d=>d.pass===true),
      observedCheck(localJupiter,"all AUTO/Ethena retained conversion samples fit the actual Squads packet envelope (legacy or validated v0)",d=>d.pass===true&&d.packets?.allFit===true),
      measured("catalog operation and swap-edge cardinality",catalog.operations.length === 44 && catalog.swapEdges.length === 52,
        {operations:catalog.operations.length,swapEdges:catalog.swapEdges.length}),
      observedCheck(chain,"required observed accounts are present",d => Array.isArray(d.accounts) && d.accounts.length > 0 && d.accounts.every((a: Json) => a.present === true)),
      observedCheck(bindings,"current Kamino account vectors match retained installed policies",d => d.lanes.every((lane:Json)=>
        lane.operations.every((op:Json)=>op.accountVectorMatches && op.retainedPolicyBytesMatch))),
      observedCheck(bindings,"resolved custody, obligation and farm setup identities are initialized",d => d.lanes.every((lane:Json)=>
        lane.custodiesExact && lane.obligation.exact && lane.farmAccounts.every((farm:Json)=>farm.exact))),
    ],[
      "Installed policy bytes and exact account derivation/ownership/setup compared against each proposed binding.",
      "All-lane production observation, construction, non-USDC valuation, exit and reconciliation behavior; catalog counts alone prove none of these.",
    ]),
    measuredCondition("R04","All-lane positives/negatives and full stateful lifecycle",[
      observedCheck(localCandidateJupiter,"V2 candidates create on cloned Settings and execute two sequential swaps plus fourteen rejecting mutations; current Go matches SDK wires with test-only candidate bindings, not installed authority or a full lifecycle",d=>d.pass===true),
      observedCheck(localSequentialKamino,"Ethena Go deposit/borrow/repay/withdraw execute sequentially against deployed binaries with captured custody effects, flat terminal obligation and an installed-policy amount rejection",d=>d.pass===true),
      observedCheck(localSequentialJupiter,"Ethena USDC/collateral and collateral/debt Go swaps execute sequentially against deployed binaries with measured debit/min-output and installed-policy rejection before Jupiter CPI",d=>d.pass===true),
    ],[
      "Complete sequential bridge/swap/Kamino/return/NAV lifecycle with signer proof, fee/exit admission and explicit controlled-capacity overrides where required; the four-leg local Kamino probe is only a subclaim.",
      "Batched exact eleven-lane behavioral coverage with identity-bound retained equivalence and dangerous mutations.",
    ]),
    measuredCondition("R05","Immutable deployment, one fenced writer and flat queue transitions",[
      observedCheck(deployment,"live immutable image tag observed",d => d.deploys.some((row: Json) =>
        row.status === "live" && /^ghcr\.io\/loyal-labs\/loyal-yield-routing\/backyard-rwa-worker:sha-[0-9a-f]{40}$/.test(row.image ?? ""))),
      observedCheck(database,"no unresolved journal operations at snapshot",d => Array.isArray(d.nonterminal) && d.nonterminal.length === 0),
    ],[
      "Image digest/source/service command bound to verified Phase 3 build, not merely any immutable old image.",
      "Fenced queue advancement with independently flat shared custody and touched obligations; held family cannot stall eligible siblings.",
    ]),
    measuredCondition("R06","Three Go-originated accepted canary outcomes",[
      observedCheck(database,"at least one Phase 3 journal operation exists",d => Number.isSafeInteger(d.phase3OperationCount) && d.phase3OperationCount > 0),
    ],["OnRe, AUTO and Ethena each have complete LIVE_VALIDATED or R04-backed unfunded CAPACITY_PENDING proof and finalized terminal custody."]),
    measuredCondition("R07","Retained prior routes, withdrawal, NAV and recovery",[
      measured("retained Phase 2 artifact reports PASS",retained.verdict === "PASS",{
        path:"docs/evidence/backyard-rwa-go/phase2-runtime/lifecycle-v1.json",
        sha256:sha(read("docs/evidence/backyard-rwa-go/phase2-runtime/lifecycle-v1.json")),verdict:retained.verdict,
        proofLevel:"HISTORICAL_ARTIFACT_ONLY"}),
    ],["Match retained dependency identities and execute affected withdrawal/recovery/NAV regressions, including actual reserved-exit admission and execution."]),
    measuredCondition("R08","Deployed readiness agrees with operational handoff and standing coverage",[
      measured("source and embedded manifest bytes agree",read("docs/manifests/backyard-rwa-v1.json") ===
        read("go/backyard-rwa-worker/internal/backyardrwa/manifest/backyard-rwa-v1.json"),{proofLevel:"LOCAL_MANIFEST_PARITY"}),
    ],["Deployed eleven-lane readiness and existing handoff agree with proven statuses, policy bindings, image, budgets and recovery; promoted CI checks pass."]),
  ];
  const verdict = conditions.some(c => c.verdict === "FAIL") ? "FAIL" : conditions.some(c => c.verdict === "BLOCKED") ? "BLOCKED" : "PASS";
  if (sourceIdentity().implementationSha256!==source.implementationSha256) throw new Error("SOURCE_CHANGED_DURING_VERIFICATION");
  return {
    schema:"loyal-backyard-rwa-phase3-family-activation-verifier/v2",
    verdict,implementationStatus:verdict === "PASS" ? "VERIFIED" : "INCOMPLETE",broadcast:false,readOnly:true,
    generatedAt:new Date().toISOString(),goalId:GOAL,
    source,
    contract:{path:CONTRACT,sha256:sha(read(CONTRACT))},
    // Existing policy allocations are a diagnostic sample, not a fabricated
    // pass/fail for the complete proposed setup graph. Retain prices, hashes,
    // rent and the explicit new-allocation proof limitation in the snapshot.
    preflight:{chain,database,deployment,runtime,bindings,setupRent,localCaps,localSendJournal,localBridgeAdmission,localWithdrawalAdmission,localKaminoConstruction,localDebtDecisions,localJupiter,localSequentialKamino,localSequentialJupiter,localJupiterRepair,localCandidateJupiter},
    conditions,
  };
}
if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  try {
    const result = await verify();
    const serialized=JSON.stringify(result,null,2)+"\n";
    const outputIndex=process.argv.indexOf("--output");
    if (outputIndex!==-1) {
      const argument=process.argv[outputIndex+1];
      if (!argument || argument.startsWith("--")) throw new Error("OUTPUT_PATH_MISSING");
      const output=resolve(ROOT,argument);
      // Evidence snapshots are immutable. Never replace earlier proof silently.
      const bytes=output.endsWith(".gz")?gzipSync(serialized):Buffer.from(serialized);
      writeFileSync(output,bytes,{flag:"wx",mode:0o600});
      console.log(JSON.stringify({verdict:result.verdict,output,sha256:sha(bytes),jsonSha256:sha(serialized),
        conditions:result.conditions.map(c=>({id:c.id,verdict:c.verdict,measurements:c.measurements.map(m=>({claim:m.claim,verdict:m.verdict})),missingProof:c.missingProof}))},null,2));
    } else console.log(serialized.trimEnd());
    process.exitCode=result.verdict === "PASS" ? 0 : 1;
  }
  catch { console.log(JSON.stringify({verdict:"FAIL",reason:"VERIFIER_INPUT_INVALID",broadcast:false})); process.exitCode=1; }
}
