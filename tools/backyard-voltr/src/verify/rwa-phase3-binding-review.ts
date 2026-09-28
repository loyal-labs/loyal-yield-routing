import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { address } from "@solana/kit";
import { Connection, PublicKey } from "@solana/web3.js";
import { Obligation } from "@kamino-finance/klend-sdk";
import { getUserStatePDA } from "@kamino-finance/farms-sdk/dist/utils/utils.js";
import { UserState } from "@kamino-finance/farms-sdk/dist/@codegen/farms/accounts/UserState.js";
import { RWA_MULTIPLY_ROUTE } from "../domain/rwa-multiply-route-spec.js";
import { resolveCurrentRwaMultiplyCatalog } from "../policies/rwa-multiply-catalog-resolver.js";
import { buildPhaseTwoKaminoLaneOperations, hasConfiguredKaminoOracle, type KaminoLaneFarms } from "../policies/rwa-multiply-phase2-kamino.js";

type Json = Record<string, any>;
const ROOT = resolve(fileURLToPath(new URL("../../../..", import.meta.url)));
const sha = (data: string | Uint8Array) => createHash("sha256").update(data).digest("hex");
const read = (name: string) => readFileSync(resolve(ROOT,"docs/evidence/backyard-rwa-go",name),"utf8");

// Compare exact indexed account vectors, never set membership: swapping two
// same-mint destinations must remain a mismatch. This is source/byte review,
// not a replacement for Squads execution or deployed Go construction proof.
export function accountVectorDifferences(actual: string[], constraints: {index:number;pubkeys:string[]}[]) {
  const differences: {index:number;actual:string|null;allowed:string[]}[] = [];
  const indices = new Set(constraints.map(c => c.index));
  if (indices.size !== constraints.length || constraints.some(c => !Number.isSafeInteger(c.index) || c.index < 0))
    throw new Error("INVALID_CONSTRAINT_INDICES");
  for (const c of constraints) {
    if (!c.pubkeys.includes(actual[c.index] ?? "")) differences.push({index:c.index,actual:actual[c.index]??null,allowed:c.pubkeys});
  }
  for (let index=0;index<actual.length;index++)
    if (!indices.has(index)) differences.push({index,actual:actual[index]!,allowed:[]});
  return differences;
}

export async function reviewPhase3Bindings(connection: Connection) {
  const resolution = await resolveCurrentRwaMultiplyCatalog(connection,"finalized");
  const compiledText = read("policy-compiled-v1.json");
  const compiled = JSON.parse(compiledText) as Json;
  const installed = JSON.parse(read("policy-install-readback-v1.json")) as Json;
  const rollovers = JSON.parse(read("phase2-runtime/current-policy-rollovers-v1.json")) as Json;
  if (installed.compiledArtifactSha256 !== sha(compiledText)) throw new Error("COMPILED_INSTALL_PROVENANCE_MISMATCH");
  const farmsProgram = RWA_MULTIPLY_ROUTE.kamino.farmsProgram;
  const farms = new Map<string,KaminoLaneFarms>();
  for (const lane of resolution.lanes) {
    const bindings: {collateral?:{farm:string;user:string};debt?:{farm:string;user:string}} = {};
    for (const side of ["collateral","debt"] as const) {
      const farm = side === "collateral" ? lane.resolved.collateralReserve.farmCollateral : lane.resolved.debtReserve.farmDebt;
      if (hasConfiguredKaminoOracle(farm)) bindings[side] = {farm,user:String(await getUserStatePDA(
        address(farmsProgram),address(farm),address(lane.resolved.obligation)))};
    }
    farms.set(lane.key,bindings);
  }
  const policyRows: {address:string;hash:string}[] = [
    ...installed.operations.map((row: Json) => ({address:row.policyAddress,hash:row.dataSha256})),
    ...rollovers.policies.map((row: Json) => ({address:row.binding.policy,hash:row.liveAccountDataSha256})),
  ];
  const addresses = [...new Set([
    ...policyRows.map(p=>p.address),...resolution.lanes.map(l=>l.resolved.obligation),
    ...[...farms.values()].flatMap(f=>Object.values(f).flatMap(b=>[b.farm,b.user])),
  ])];
  // All current bindings fit one coherent read today; reject growth rather
  // than silently claim a mixed-slot snapshot.
  if (addresses.length > 100) throw new Error("BINDING_SNAPSHOT_EXCEEDS_RPC_BATCH");
  const snapshot = await connection.getMultipleAccountsInfoAndContext(addresses.map(a=>new PublicKey(a)),
    {commitment:"finalized",minContextSlot:resolution.contextSlot});
  if (snapshot.value.length !== addresses.length) throw new Error("INCOMPLETE_BINDING_SNAPSHOT");
  const accounts = new Map(addresses.map((a,i)=>[a,snapshot.value[i]]));
  const policies = policyRows.map(p=>{
    const info=accounts.get(p.address);
    return {...p,present:!!info,owner:info?.owner.toBase58(),observedSha256:info?sha(info.data):null,
      retainedBytesMatch:!!info && info.owner.toBase58()===RWA_MULTIPLY_ROUTE.squads.program && sha(info.data)===p.hash};
  });
  const lanes = resolution.lanes.map(lane=>{
    const binding=farms.get(lane.key)!;
    const obligationInfo=accounts.get(lane.resolved.obligation);
    let obligation: Json = {address:lane.resolved.obligation,present:!!obligationInfo,exact:false};
    if (obligationInfo?.owner.toBase58()===RWA_MULTIPLY_ROUTE.kamino.program) {
      const decoded=Obligation.decode(obligationInfo.data);
      obligation={...obligation,owner:String(decoded.owner),market:String(decoded.lendingMarket),
        exact:String(decoded.owner)===lane.resolved.vault && String(decoded.lendingMarket)===lane.resolved.lendingMarket,
        dataSha256:sha(obligationInfo.data)};
    }
    const farmAccounts=Object.entries(binding).map(([side,b])=>{
      const info=accounts.get(b.user), farmInfo=accounts.get(b.farm);
      let decoded: UserState|undefined;
      if (info?.owner.toBase58()===farmsProgram) decoded=UserState.decode(info.data);
      return {side,...b,present:!!info,farmOwner:farmInfo?.owner.toBase58(),owner:decoded?String(decoded.owner):null,
        delegatee:decoded?String(decoded.delegatee):null,
        exact:!!decoded && farmInfo?.owner.toBase58()===farmsProgram && String(decoded.farmState)===b.farm &&
          String(decoded.owner)===lane.resolved.vault && String(decoded.delegatee)===lane.resolved.obligation,
        dataSha256:info?sha(info.data):null};
    });
    const operations=buildPhaseTwoKaminoLaneOperations(lane,1n,binding).map(op=>{
      const original=compiled.policies.find((p:Json)=>p.logicalName===`lane/${lane.key}` && p.operations?.includes(op.operation));
      const rolled=lane.key===rollovers.selectedLane ? rollovers.policies.find((p:Json)=>p.binding.operation===op.operation) : undefined;
      const constraint=rolled ? {programId:rolled.binding.programId,accountPubkeys:rolled.binding.accountPubkeys.map((key:string,index:number)=>({index,pubkeys:[key]}))} : original?.constraints[0];
      if (!constraint) throw new Error("CATALOG_OPERATION_POLICY_MISSING");
      const policy=rolled?.binding.policy??original.policy;
      const differences=accountVectorDifferences(op.accounts.map(a=>a.address),constraint.accountPubkeys);
      return {operation:op.operation,policy,programMatches:op.programId===constraint.programId,
        retainedPolicyBytesMatch:policies.find(p=>p.address===policy)?.retainedBytesMatch===true,
        accounts:op.accounts,accountDifferences:differences,
        accountVectorMatches:op.programId===constraint.programId && differences.length===0};
    });
    return {lane:lane.key,custodiesExact:lane.exact,obligation,farmAccounts,operations};
  });
  return {schema:"loyal-backyard-rwa-phase3-binding-review/v1",broadcast:false,readOnly:true,
    proofLevel:"FRESH_ACCOUNT_AND_RETAINED_POLICY_VECTOR_REVIEW",slot:snapshot.context.slot,
    resolutionSlots:resolution.observationSlots,policySeed:resolution.policySeedBefore,
    compiledSha256:sha(compiledText),policies,lanes,
    limitations:["No runtime bindings installed or enabled; no transaction constructed for signing or broadcast.",
      "Account-vector comparison is not full policy evaluation, program execution, token-extension validation or capacity proof."]};
}
