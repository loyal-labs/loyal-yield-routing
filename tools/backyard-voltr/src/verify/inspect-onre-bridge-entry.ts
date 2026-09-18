// Fresh read-only confirmation of the existing bridge boundary. No signer,
// candidate installation, account override or send RPC. Simulations are unsent
// and do NOT prove signatures or sequential execution.
import {spawnSync} from "node:child_process";
import {createHash} from "node:crypto";
import {readFileSync,writeFileSync} from "node:fs";
import {resolve} from "node:path";
import {Connection,PublicKey,TransactionInstruction,TransactionMessage,VersionedTransaction} from "@solana/web3.js";
import {address,createNoopSigner,isSignerRole,isWritableRole} from "@solana/kit";
import {getDepositVaultInstructionAsync,getVaultDecoder,getStrategyInitReceiptDecoder} from "@voltr/vault-sdk";

const sha=(v:Uint8Array)=>createHash("sha256").update(v).digest("hex");
let stage="arguments";
try {
  const directory=process.argv[2];
  if(!directory||!/^\/private\/tmp\/backyard-phase3-jupiter-probe\.[A-Za-z0-9]+$/.test(directory)||!process.env.SOLANA_RPC_URL)throw new Error();
  const plan=JSON.parse(readFileSync(resolve(directory,"plan.json"),"utf8"));
  if(plan.lane!=="OnRe/ONyc/USDC"||plan.broadcast!==false||!plan.onreBridge||plan.addresses.length>100)throw new Error();
  const rpc=new Connection(process.env.SOLANA_RPC_URL,{commitment:"finalized",disableRetryOnRateLimit:true,fetch:(input,init)=>fetch(input,{...init,signal:AbortSignal.timeout(30_000)})});
  if(await rpc.getGenesisHash()!=="5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d")throw new Error();
  if(process.argv[3]==="--deposit-accounting-diagnostic") {
    // Explore the official ordinary-user deposit semantics, not a worker action
    // or authorization to fund it. The existing admin identity is unsigned.
    const admin="BAqgbERmvUViqDSx961xpRBHGt68SpACiWL4t9696qZZ",vault="HXtk15EA5pBg3rSKxBm8sWPExScPkTknSRp37fXNHgNA",receipt="3GHLmyTTGH9ZfQqb3YCo9xKjpPhMLvHsq2JSYzCnk9U6";
    const rows=[];
    for(const raw of [0n,119n]) {
      stage="SDK deposit construction";
      const ix=await getDepositVaultInstructionAsync({userTransferAuthority:createNoopSigner(address(admin)),vault:address(vault),vaultAssetMint:address("EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"),assetTokenProgram:address("TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"),amount:raw});
      const instruction=new TransactionInstruction({programId:new PublicKey(ix.programAddress),data:Buffer.from(ix.data),keys:ix.accounts.map(a=>({pubkey:new PublicKey(a.address),isSigner:isSignerRole(a.role),isWritable:isWritableRole(a.role)}))});
      const observed=[...new Set([vault,receipt,...ix.accounts.filter(a=>isWritableRole(a.role)).map(a=>a.address)])];
      stage="deposit account observation";
      const before=await rpc.getMultipleAccountsInfoAndContext(observed.map(a=>new PublicKey(a)),{commitment:"finalized"});
      stage="deposit unsigned wire";
      const tx=new VersionedTransaction(new TransactionMessage({payerKey:new PublicKey(admin),recentBlockhash:plan.inputCustody,instructions:[instruction]}).compileToLegacyMessage());
      const wire=Buffer.from(tx.serialize());
      if(wire.length>1232||wire[0]!==1||!wire.subarray(1,65).every(v=>v===0))throw new Error();
      stage="deposit unsigned simulation";
      const simulation=await rpc.simulateTransaction(tx,{sigVerify:false,replaceRecentBlockhash:true,commitment:"finalized",minContextSlot:before.context.slot,accounts:{encoding:"base64",addresses:observed}});
      rows.push({amountRaw:raw.toString(),instruction:{program:ix.programAddress,accounts:ix.accounts.map(a=>({address:a.address,role:a.role})),dataBase64:Buffer.from(ix.data).toString("base64")},wireBase64:wire.toString("base64"),wireSha256:sha(wire),observed,before:{slot:before.context.slot,accounts:before.value.map((a,i)=>({address:observed[i],present:!!a,...(a?{owner:a.owner.toBase58(),lamports:a.lamports,dataBase64:a.data.toString("base64"),dataSha256:sha(a.data)}:{})}))},simulation});
    }
    stage="deposit evidence retention";
    writeFileSync(resolve(directory,"deposit-accounting-rpc-diagnostic.json"),JSON.stringify({schema:"phase3-voltr-deposit-accounting-diagnostic/v1",broadcast:false,signatureProof:false,sequentialProof:false,liveRepairAuthorized:false,rows},null,2)+"\n",{flag:"wx",mode:0o600});
    console.log(JSON.stringify({broadcast:false,liveRepairAuthorized:false,rows:rows.map(r=>({amountRaw:r.amountRaw,slot:r.simulation.context.slot,error:r.simulation.value.err}))}));
    process.exit(0);
  }
  if(process.argv[3]!==undefined)throw new Error();
  const batch=await rpc.getMultipleAccountsInfoAndContext(plan.addresses.map((a:string)=>new PublicKey(a)),{commitment:"finalized"});
  const accounts=batch.value.map((a,i)=>({address:plan.addresses[i],present:!!a,...(a?{owner:a.owner.toBase58(),lamports:a.lamports,executable:a.executable,dataBase64:a.data.toString("base64"),dataSha256:sha(a.data)}:{})}));
  for(const [a,hash] of Object.entries(plan.onreBridge.policies))if(accounts.find(r=>r.address===a)?.dataSha256!==hash)throw new Error();
  const vaultAddress="HXtk15EA5pBg3rSKxBm8sWPExScPkTknSRp37fXNHgNA",receiptAddress="3GHLmyTTGH9ZfQqb3YCo9xKjpPhMLvHsq2JSYzCnk9U6";
  const data=(a:string)=>Buffer.from(accounts.find(r=>r.address===a)!.dataBase64!,"base64");
  const vault=getVaultDecoder().decode(data(vaultAddress)),receipt=getStrategyInitReceiptDecoder().decode(data(receiptAddress));
  const rows=[];
  for(const action of ["REPORT_NAV","VOLTR_ALLOCATE_TO_SQUADS"]) {
    const input={action,amountRaw:action==="REPORT_NAV"?0:101000,slot:batch.context.slot,accounts:accounts.filter(a=>["6LATwaB4yRwGURCBDyFeJGqofaXxb6xXws9wBGbr3RBh","FTDWN5Ay8tzYPJBJT4s2oZaHRQ7jKPo8XP2ZRWb5GP3M","EBG2iYrcXttDy9FpWDeNVL8uaCLRCkevrpRyrAhvVYKe"].includes(a.address))};
    const result=spawnSync("/private/tmp/phase3-onre-bridge-test",["-test.run=^TestExportOnReBridgeProbe$"],{env:{...process.env,PHASE3_ONRE_BRIDGE_INPUT:JSON.stringify(input)},timeout:30_000});
    if(result.status!==0)throw new Error();
    const line=result.stdout.toString().split("\n").find(s=>s.startsWith("ONRE_BRIDGE_JSON="));
    if(!line)throw new Error();
    const compiled=JSON.parse(line.slice("ONRE_BRIDGE_JSON=".length));
    if(compiled.broadcast!==false||compiled.signatureProof!==false||compiled.discoveryOnly!==false||compiled.steps.length!==1)throw new Error();
    const wire=Buffer.from(compiled.steps[0].wireBase64,"base64");
    if(wire.length>1232||wire[0]!==1||!wire.subarray(1,65).every(v=>v===0)||sha(wire)!==compiled.steps[0].wireSha256)throw new Error();
    const simulation=await rpc.simulateTransaction(VersionedTransaction.deserialize(wire),{sigVerify:false,replaceRecentBlockhash:true,commitment:"finalized",minContextSlot:batch.context.slot});
    rows.push({input,compiled,simulation});
  }
  const report={schema:"phase3-onre-bridge-rpc-inspection/v1",broadcast:false,signatureProof:false,sequentialProof:false,slot:batch.context.slot,policies:plan.onreBridge.policies,accounts:accounts.filter(a=>!a.executable),vaultTotalValueRaw:vault.asset.totalValue.toString(),strategyReceiptValueRaw:receipt.positionValue.toString(),rows};
  writeFileSync(resolve(directory,"bridge-rpc-simulation.json"),JSON.stringify(report,null,2)+"\n",{flag:"wx",mode:0o600});
  console.log(JSON.stringify({broadcast:false,slot:report.slot,vaultTotalValueRaw:report.vaultTotalValueRaw,strategyReceiptValueRaw:report.strategyReceiptValueRaw,simulations:rows.map(r=>({action:r.input.action,slot:r.simulation.context.slot,error:r.simulation.value.err,compute:r.simulation.value.unitsConsumed}))}));
}catch{console.error(JSON.stringify({broadcast:false,stage,reason:"READ_ONLY_BRIDGE_INSPECTION_UNAVAILABLE"}));process.exitCode=1;}
