// Closed local OnRe extension of the existing swap probe. No signer or send API.
import {createHash} from "node:crypto";
import {readFileSync} from "node:fs";
import {Reserve,refreshReserve,refreshObligation} from "@kamino-finance/klend-sdk";
import {UserState} from "@kamino-finance/farms-sdk/dist/@codegen/farms/accounts/UserState.js";
import {AccountRole,address,none,some,type Address} from "@solana/kit";
import {Connection,PublicKey,TransactionInstruction,TransactionMessage,VersionedTransaction} from "@solana/web3.js";
import {RWA_MULTIPLY_ROUTE as route} from "../domain/rwa-multiply-route-spec.js";
import {resolveCurrentRwaMultiplyCatalog} from "../policies/rwa-multiply-catalog-resolver.js";
import {buildPhaseTwoKaminoLaneOperations,hasConfiguredKaminoOracle} from "../policies/rwa-multiply-phase2-kamino.js";
import {toWeb3Instruction} from "../integrations/solana-compat.js";
import {buildExactKaminoSquadsExecution} from "./rwa-phase2-kamino-execution.js";

const sha=(b:Uint8Array)=>createHash("sha256").update(b).digest("hex");
function require(v:unknown):asserts v {if(!v)throw new Error("OnRe local lending boundary mismatch");}
const read=(name:string)=>JSON.parse(readFileSync(new URL(`../../../../docs/evidence/backyard-rwa-go/${name}`,import.meta.url),"utf8"));

export async function prepareOnReLending(rpc:Connection,seedBefore:bigint,collateralAmount:bigint) {
  const resolution=await resolveCurrentRwaMultiplyCatalog(rpc,"finalized");
  require(BigInt(resolution.policySeedBefore)===seedBefore);
  const lane=resolution.lanes.find(l=>l.key==="OnRe/ONyc/USDC");require(lane?.exact);
  const g=lane.resolved;
  const farm="7vNfe1qX8iDxP5p3A4fosrjLqdn1YjmmGcZZkG2b4APF",user="nMqFZFPQsNwot49QAD1B76LxNV7qRG1tnbkXyTjbUAD";
  require(g.obligation==="4LnCFir7Qc99GhjGHLcwtkfweyAMu37u5QE1zTupKsei"&&g.vault===route.squads.vault&&
    g.debtReserve.address==="AYL4LMc4ZCVyq3Z7XPJGWDM4H9PiWjqXAAuuHBEGVR2Z"&&g.debtReserve.farmDebt===farm);
  const reserveKeys=[g.collateralReserve.address,g.debtReserve.address];
  const batch=await rpc.getMultipleAccountsInfoAndContext([...reserveKeys,user].map(a=>new PublicKey(a)),{commitment:"finalized",minContextSlot:resolution.contextSlot});
  require(batch.value[0]?.owner.toBase58()===route.kamino.program&&batch.value[1]?.owner.toBase58()===route.kamino.program&&batch.value[2]?.owner.toBase58()===route.kamino.farmsProgram);
  const u=UserState.decode(batch.value[2]!.data);
  require(String(u.owner)===g.vault&&String(u.delegatee)===g.obligation&&String(u.farmState)===farm);
  const optional=(v:string)=>hasConfiguredKaminoOracle(v)?some(address(v)):none<Address>();
  const refreshes=batch.value.slice(0,2).map((a,i)=>{
    const t=Reserve.decode(a!.data).config.tokenInfo;
    return toWeb3Instruction(refreshReserve({reserve:address(reserveKeys[i]!),lendingMarket:address(g.lendingMarket),pythOracle:optional(String(t.pythConfiguration.price)),
      switchboardPriceOracle:optional(String(t.switchboardConfiguration.priceAggregator)),switchboardTwapOracle:optional(String(t.switchboardConfiguration.twapAggregator)),scopePrices:optional(String(t.scopeConfiguration.priceFeed))},[],address(route.kamino.program)));
  });
  const compiled=read("policy-compiled-v1.json"),installed=read("policy-install-readback-v1.json");
  const groups:any[]=[],candidates:any[]=[],steps:any[]=[];
  const policies:Record<string,string>={};const addresses=new Set<string>([user,farm,g.obligation]);
  for(const [i,operation] of ["deposit","borrow","repay","withdraw"].entries()) {
    const matches=compiled.policies.filter((p:any)=>p.logicalName===`lane/${lane.key}`&&p.operations?.includes(operation));require(matches.length===1);
    const original=matches[0];const receipt=installed.operations.find((p:any)=>p.policyAddress===original.policy);
    require(receipt?.active===true&&sha(Buffer.from(receipt.dataBase64,"base64"))===receipt.dataSha256);
    policies[original.policy]=receipt.dataSha256;addresses.add(original.policy);
    let policy=original;
    if(operation==="borrow"||operation==="repay") {
      const positions=operation==="borrow"?[12,13]:[9,10];
      const constraints=structuredClone(original.constraints);require(constraints.length===1);
      for(const [n,index] of positions.entries()) {
        const a=constraints[0].accountPubkeys.find((a:any)=>a.index===index);
        require(a?.pubkeys.length===1&&a.pubkeys[0]===route.kamino.program);a.pubkeys=[n===0?user:farm];
      }
      const seed=seedBefore+3n+BigInt(candidates.length),b=Buffer.alloc(8);b.writeBigUInt64LE(seed);
      const p=PublicKey.findProgramAddressSync([Buffer.from("smart_account"),Buffer.from("policy"),new PublicKey(route.squads.settings).toBuffer(),b],new PublicKey(route.squads.program))[0].toBase58();
      policy={...original,seed:seed.toString(),policy:p,constraints};
      const edge=`OnRe/${operation}`;
      groups.push({edge,originalPolicy:original.policy,originalPolicyDataSha256:receipt.dataSha256,originalConstraints:original.constraints,replacedConstraintIndex:0,replacementConstraints:constraints});
      candidates.push({edge,seed:seed.toString(),policy:p});addresses.add(p);
    }
    const amount=i===0?collateralAmount:1000n; // finite repay/withdraw templates sized from executed state by the runner
    const shape=buildPhaseTwoKaminoLaneOperations(lane,amount,{debt:{farm,user}}).find(s=>s.operation===operation)!;
    const inner=new TransactionInstruction({programId:new PublicKey(shape.programId),data:Buffer.from(shape.dataBase64,"base64"),keys:shape.accounts.map(a=>({pubkey:new PublicKey(a.address),isSigner:a.signer,isWritable:a.writable}))});
    const execution=buildExactKaminoSquadsExecution({compiledPolicy:policy,operation,innerInstruction:inner,delegatedSigner:new PublicKey(route.squads.delegatedExecutor)});
    const remaining=i===0?[]:i===2?reserveKeys:[reserveKeys[0]!];
    const refresh=toWeb3Instruction(refreshObligation({lendingMarket:address(g.lendingMarket),obligation:address(g.obligation)},remaining.map(a=>({address:address(a),role:AccountRole.WRITABLE})),address(route.kamino.program)));
    const ixs=[...refreshes,refresh,execution.outerInstruction];
    const message=new TransactionMessage({payerKey:new PublicKey(route.squads.delegatedExecutor),recentBlockhash:route.squads.vault,instructions:ixs}).compileToLegacyMessage();
    const wire=new VersionedTransaction(message).serialize();require(wire.length<=1232&&wire.slice(1,65).every(b=>b===0));
    for(const ix of ixs){addresses.add(ix.programId.toBase58());for(const a of ix.keys)addresses.add(a.pubkey.toBase58());}
    steps.push({leg:operation,wireBase64:Buffer.from(wire).toString("base64"),wireSha256:sha(wire),instructionDataBase64:shape.dataBase64,amountRaw:Number(amount)});
  }
  return {schema:"phase3-onre-linked-lending-plan/v1",broadcast:false,signatureProof:false,lane:lane.key,obligation:g.obligation,
    collateralCustody:g.collateralCustody.address,debtCustody:g.debtCustody.address,steps,groups,candidates,policies,addresses:[...addresses],
    observedSlot:batch.context.slot,farmUserSha256:sha(batch.value[2]!.data)};
}
