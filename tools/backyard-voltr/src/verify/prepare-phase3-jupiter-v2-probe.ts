// Public reads and zero-signature local candidate wires only. No signer loads,
// policy installation, simulation RPC or send RPC is available in this script.
import {createHash} from "node:crypto";
import {readFileSync,writeFileSync} from "node:fs";
import {resolve} from "node:path";
import {generated} from "@loyal-labs/loyal-smart-accounts-core";
import {Connection,PublicKey,TransactionInstruction,TransactionMessage,VersionedTransaction} from "@solana/web3.js";
import {RWA_MULTIPLY_ROUTE as route} from "../domain/rwa-multiply-route-spec.js";
import {catalogSwapEdges,validateJupiterHeader} from "../policies/rwa-multiply-jupiter-headers.js";
import {exactJupiterConstraint} from "../policies/rwa-multiply-jupiter-constraint.js";
import {buildExactJupiterSquadsExecution} from "./rwa-phase2-jupiter-execution.js";

const sha=(data:Uint8Array|string)=>createHash("sha256").update(data).digest("hex");
function require(value:unknown,message:string):asserts value {if(!value)throw new Error(message);}
try {
  const directory=process.argv[2];
  require(directory&&/^\/private\/tmp\/backyard-phase3-jupiter-probe\.[A-Za-z0-9]+$/.test(directory),"explicit local probe directory required");
  require(process.env.SOLANA_RPC_URL,"RPC missing");
  const rpc=new Connection(process.env.SOLANA_RPC_URL,{commitment:"finalized",disableRetryOnRateLimit:true,fetch:(input,init)=>fetch(input,{...init,signal:AbortSignal.timeout(30_000)})});
  require(await rpc.getGenesisHash()===route.genesisHash,"genesis drift");
  const settings=await rpc.getAccountInfoAndContext(new PublicKey(route.squads.settings),{commitment:"finalized"});
  require(settings.value?.owner.toBase58()===route.squads.program,"Settings identity drift");
  const [state]=(generated as any).Settings.fromAccountInfo(settings.value);
  require(state.threshold===1&&state.timeLock===0&&state.signers.length===1&&state.signers[0].key.toBase58()===route.setupAdmin&&state.signers[0].permissions.mask===7,"Settings authority drift");
  const seedBefore=BigInt(state.policySeed.toString());
  const artifactBytes=readFileSync(new URL("../../../../docs/evidence/backyard-rwa-go/phase3/jupiter-v2-repair-candidates-2026-09-04.json",import.meta.url));
  const artifact=JSON.parse(artifactBytes.toString());
  require(artifact.schema==="phase3-jupiter-v2-repair-candidates/v1"&&artifact.broadcast===false&&artifact.installed===false&&artifact.groups.length===2,"candidate scope drift");
  const addresses=new Set<string>([route.squads.settings,route.setupAdmin,route.squads.delegatedExecutor,route.squads.program,"11111111111111111111111111111111","SysvarC1ock11111111111111111111111111111111"]);
  const policies:Record<string,string>={};const steps=[];const candidatePolicies=[];
  let amount=100_000n;
  for(const [i,key] of ["USDC->USDe","USDe->PYUSD"].entries()) {
    const edge=catalogSwapEdges().find(e=>e.key===key)!;const group=artifact.groups.find((g:any)=>g.edge===key);
    require(edge&&group,"candidate edge absent");
    const query=new URLSearchParams({inputMint:edge.source.mint,outputMint:edge.destination.mint,amount:amount.toString(),slippageBps:"50",swapMode:"ExactIn",maxAccounts:"32",instructionVersion:"V2"});
    const q=await fetch("https://lite-api.jup.ag/swap/v1/quote?"+query,{signal:AbortSignal.timeout(20_000)});require(q.ok,"quote HTTP failure");const quote=await q.json() as any;
    require(quote.inputMint===edge.source.mint&&quote.outputMint===edge.destination.mint&&quote.inAmount===amount.toString()&&quote.swapMode==="ExactIn"&&quote.slippageBps===50&&quote.routePlan.length>0&&quote.routePlan.length<=4,"quote identity drift");
    const s=await fetch("https://lite-api.jup.ag/swap/v1/swap-instructions",{method:"POST",headers:{"content-type":"application/json"},body:JSON.stringify({userPublicKey:route.squads.vault,quoteResponse:quote,wrapAndUnwrapSol:false,useSharedAccounts:true,dynamicComputeUnitLimit:false}),signal:AbortSignal.timeout(20_000)});
    require(s.ok,"instruction HTTP failure");const response=await s.json() as any;
    require(!response.setupInstructions?.length&&!response.otherInstructions?.length&&!response.cleanupInstruction&&!response.tokenLedgerInstruction,"unapproved auxiliary instructions");
    const data=Buffer.from(response.swapInstruction.data,"base64");
    const instruction=new TransactionInstruction({programId:new PublicKey(response.swapInstruction.programId),data,keys:response.swapInstruction.accounts.map((a:any)=>({...a,pubkey:new PublicKey(a.pubkey)}))});
    const header=validateJupiterHeader({instruction,sourceMint:edge.source.mint,destinationMint:edge.destination.mint,sourceAta:edge.source.ata,destinationAta:edge.destination.ata,sourceTokenProgram:edge.source.tokenProgram,destinationTokenProgram:edge.destination.tokenProgram,amountRaw:amount,outAmountRaw:BigInt(quote.outAmount)});
    require(header.dialect==="shared-accounts-route-v2","not V2");
    const row={pass:true,key,source:edge.source,destination:edge.destination,quote:{inAmountRaw:quote.inAmount,outAmountRaw:quote.outAmount},header,
      instruction:{...response.swapInstruction,dataBase64:response.swapInstruction.data,dataSha256:sha(data)},lookupTables:response.addressLookupTableAddresses};
    const exact=exactJupiterConstraint(row);
    require(JSON.stringify({operation:null,...exact.constraint})===JSON.stringify(group.replacementConstraints[group.replacedConstraintIndex]),"fresh instruction differs from candidate semantics");
    const seed=seedBefore+BigInt(i+1);const seedBytes=Buffer.alloc(8);seedBytes.writeBigUInt64LE(seed);
    const policy=PublicKey.findProgramAddressSync([Buffer.from("smart_account"),Buffer.from("policy"),new PublicKey(route.squads.settings).toBuffer(),seedBytes],new PublicKey(route.squads.program))[0];
    const execution=await buildExactJupiterSquadsExecution({connection:rpc,delegatedSigner:new PublicKey(route.squads.delegatedExecutor),headerRow:row,
      compiledPolicy:{logicalName:"swap/"+key,seed:seed.toString(),policy:policy.toBase58(),constraints:group.replacementConstraints,constraintCount:group.replacementConstraints.length,swapEdges:[{...exact.edge,constraintIndex:group.replacedConstraintIndex}]}});
    const message=new TransactionMessage({payerKey:new PublicKey(route.squads.delegatedExecutor),recentBlockhash:route.squads.vault,instructions:[execution.outerInstruction]}).compileToV0Message([...execution.lookupTables]);
    const wire=new VersionedTransaction(message).serialize();require(wire.length<=1232&&wire[0]===1&&wire.slice(1,65).every(b=>b===0),"unsigned packet envelope drift");
    for(const a of [...message.staticAccountKeys,...instruction.keys.map(a=>a.pubkey),...execution.lookupTables.map(t=>t.key)])addresses.add(a.toBase58());
    addresses.add(group.originalPolicy);policies[group.originalPolicy]=group.originalPolicyDataSha256;
    candidatePolicies.push({edge:key,seed:seed.toString(),policy:policy.toBase58()});
    const minimum=BigInt(quote.otherAmountThreshold);require(minimum>0n&&minimum<=BigInt(quote.outAmount),"invalid minimum output");
    steps.push({action:i===0?"SWAP_STABLE_TO_COLLATERAL_STEP":"SWAP_COLLATERAL_TO_DEBT_STEP",source:edge.source.ata,destination:edge.destination.ata,amountRaw:Number(amount),minimumOutputRaw:Number(minimum),instructionDataBase64:row.instruction.dataBase64,amountOffset:9,policyMaximumInputRaw:1_000_000_000_000,wireBase64:Buffer.from(wire).toString("base64"),wireSha256:sha(wire),headerRow:row});
    amount=minimum;
  }
  require(addresses.size<=100,"coherent batch too large");
  const plan={schema:"phase3-jupiter-controlled-probe/v1",broadcast:false,compiler:"TYPESCRIPT_CANDIDATE_NOT_INSTALLED_GO",lane:"Ethena/USDe/PYUSD",delegate:route.squads.delegatedExecutor,inputCustody:steps[0]!.source,collateralCustody:steps[0]!.destination,debtCustody:steps[1]!.destination,steps,policies,addresses:[...addresses].sort(),
    candidate:{artifactSha256:sha(artifactBytes),settings:route.squads.settings,settingsDataSha256:sha(settings.value.data),settingsSlot:settings.context.slot,setupAdmin:route.setupAdmin,seedBefore:seedBefore.toString(),policies:candidatePolicies}};
  writeFileSync(resolve(directory,"plan.json"),JSON.stringify(plan,null,2)+"\n",{flag:"wx",mode:0o600});
  console.log(JSON.stringify({broadcast:false,compiler:plan.compiler,steps:steps.map(s=>({action:s.action,packetBytes:Buffer.from(s.wireBase64,"base64").length})),accounts:addresses.size}));
} catch {console.error("Read-only V2 candidate construction failed; no signer or send was used");process.exitCode=1;}
