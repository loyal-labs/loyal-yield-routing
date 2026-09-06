import {createHash} from "node:crypto";
import {isDeepStrictEqual} from "node:util";
import {PublicKey,VersionedTransaction} from "@solana/web3.js";

const sha=(v:Uint8Array)=>createHash("sha256").update(v).digest("hex");
const vault="HXtk15EA5pBg3rSKxBm8sWPExScPkTknSRp37fXNHgNA",receipt="3GHLmyTTGH9ZfQqb3YCo9xKjpPhMLvHsq2JSYzCnk9U6";
const voltr="vVoLTRjQmtFpiYoegx285Ze4gsLJ8ZxgFKVcuvmG1a8",squads="SMRTzfY6DfH5ik3TKiyLFfXexV8uSG3d2UksSCYdunG";
const cash=["6LATwaB4yRwGURCBDyFeJGqofaXxb6xXws9wBGbr3RBh","FTDWN5Ay8tzYPJBJT4s2oZaHRQ7jKPo8XP2ZRWb5GP3M","EBG2iYrcXttDy9FpWDeNVL8uaCLRCkevrpRyrAhvVYKe"];

// Evaluate captured RPC behavior, not a local success label. A valid failure is
// useful evidence but cannot satisfy even this narrow unsigned bridge subclaim.
export function onreBridgeEntryProof(e:any,manifest:any) {
  const invalid={validObservation:false,pass:false,proofLevel:"INVALID_OR_MISSING_UNSIGNED_BRIDGE_OBSERVATION"};
  try {
    if(e.schema!=="phase3-onre-bridge-rpc-inspection/v1"||e.broadcast!==false||e.signatureProof!==false||e.sequentialProof!==false||!Number.isSafeInteger(e.slot)||e.slot<=0||!Array.isArray(e.accounts)||!Array.isArray(e.rows)||e.rows.length!==2)return invalid;
    const expected=Object.fromEntries(manifest.runtimeBindings.bridgePolicies.map((p:any)=>[p.account,p.dataSha256]));
    if(!isDeepStrictEqual(e.policies,expected)||new Set(e.accounts.map((a:any)=>a.address)).size!==e.accounts.length)return invalid;
    for(const a of e.accounts)if(a.present&&(a.executable===true||sha(Buffer.from(a.dataBase64,"base64"))!==a.dataSha256))return invalid;
    const account=(address:string)=>e.accounts.find((a:any)=>a.address===address);
    for(const [address,hash] of Object.entries(expected))if(account(address)?.owner!==squads||account(address)?.dataSha256!==hash)return invalid;
    if(account(vault)?.owner!==voltr||account(receipt)?.owner!==voltr)return invalid;
    const v=Buffer.from(account(vault).dataBase64,"base64"),r=Buffer.from(account(receipt).dataBase64,"base64");
    if(v.length!==928||r.length!==192||v.subarray(0,8).toString("hex")!=="d308e82b02987577"||r.subarray(0,8).toString("hex")!=="3308c0fd734e70d6")return invalid;
    const total=v.readBigUInt64LE(168),position=r.readBigUInt64LE(104);
    if(e.vaultTotalValueRaw!==total.toString()||e.strategyReceiptValueRaw!==position.toString())return invalid;
    const authorities=["EoHz6FHTL34F6HjuJmb5EceaRqxRG1RMYwYWKtWkGBFb","8fLTf2ufePttZW3Es1xVoW3ows3WjXcuHQkkBCVvHsdH",manifest.identities.squadsVault];
    for(const [i,address] of cash.entries()) {
      const a=account(address),data=Buffer.from(a.dataBase64,"base64");
      if(!a.present||a.owner!==manifest.identities.classicTokenProgram||data.length!==165||data[108]!==1||
        !data.subarray(0,32).equals(new PublicKey(manifest.identities.usdcMint).toBuffer())||!data.subarray(32,64).equals(new PublicKey(authorities[i]).toBuffer())||
        (i===0?data.readBigUInt64LE(64)<101000n:data.readBigUInt64LE(64)!==0n))return invalid;
    }
    const actions=["REPORT_NAV","VOLTR_ALLOCATE_TO_SQUADS"];
    for(const [i,row] of e.rows.entries()) {
      if(row.input.action!==actions[i]||row.input.amountRaw!==(i===0?0:101000)||row.input.slot!==e.slot||
        !isDeepStrictEqual(row.input.accounts,e.accounts.filter((a:any)=>cash.includes(a.address)))||row.input.accounts.length!==3||
        row.compiled.broadcast!==false||row.compiled.signatureProof!==false||row.compiled.discoveryOnly!==false||!isDeepStrictEqual(row.compiled.policies,expected)||row.compiled.steps.length!==1)return invalid;
      const step=row.compiled.steps[0],request=step.request,wire=Buffer.from(step.wireBase64,"base64");
      if(request.Action!==actions[i]||request.AmountRaw!==row.input.amountRaw||request.Report.Sequence!==e.slot||request.Report.ObservedSlot!==e.slot||request.Report.NAVAfterRaw!==row.input.amountRaw||
        request.AdaptorConfig!==manifest.identities.v2StrategyConfig||request.Settings!==manifest.identities.squadsSettings||
        !/^[0-9a-f]{64}$/.test(request.Report.SnapshotDigest)||wire.length>1232||wire[0]!==1||!wire.subarray(1,65).every(b=>b===0)||sha(wire)!==step.wireSha256)return invalid;
      const tx=VersionedTransaction.deserialize(wire),message=tx.message,keys=message.staticAccountKeys.map(k=>k.toBase58());
      const policy=manifest.runtimeBindings.bridgePolicies.find((p:any)=>p.action===actions[i]).account;
      if(message.version!=="legacy"||keys[0]!==manifest.identities.delegatedExecutor||message.compiledInstructions.length!==1||
        keys[message.compiledInstructions[0]!.programIdIndex]!==squads||!keys.includes(policy)||Object.keys(expected).some(p=>p!==policy&&keys.includes(p)))return invalid;
      const s=row.simulation;
      if(!Number.isSafeInteger(s.context.slot)||s.context.slot<e.slot||s.context.slot>e.slot+128||!Array.isArray(s.value.logs)||!s.value.logs.includes(`Program ${voltr} invoke [2]`)||!Number.isSafeInteger(s.value.unitsConsumed)||s.value.unitsConsumed<=0||!Object.hasOwn(s.value,"err"))return invalid;
      if(s.value.err===null&&(s.value.logs.some((l:string)=>l.includes(" failed:"))||!s.value.logs.includes(`Program ${voltr} success`)||s.value.logs.at(-1)!==`Program ${squads} success`))return invalid;
    }
    return {validObservation:true,pass:e.rows.every((row:any)=>row.simulation.value.err===null),slot:e.slot,
      vaultTotalValueRaw:total.toString(),strategyReceiptValueRaw:position.toString(),receiptExceedsVaultTotalRaw:(position>total?position-total:0n).toString(),
      errors:e.rows.map((row:any)=>({action:row.input.action,error:row.simulation.value.err})),
      proofLevel:"CAPTURED_UNSIGNED_INDEPENDENT_RPC_BRIDGE_SIMULATIONS_NOT_SIGNATURE_SEQUENTIAL_NAV_COMPLETENESS_OR_LIVE_PROOF"};
  }catch{return invalid;}
}
