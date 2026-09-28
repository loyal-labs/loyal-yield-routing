// Public reads and zero-signature local candidate wires only. No signer loads,
// policy installation, simulation RPC or send RPC is available in this script.
import {createHash} from "node:crypto";
import {spawnSync} from "node:child_process";
import {readFileSync,writeFileSync} from "node:fs";
import {resolve} from "node:path";
import {generated} from "@loyal-labs/loyal-smart-accounts-core";
import {Connection,PublicKey,TransactionInstruction,TransactionMessage,VersionedTransaction} from "@solana/web3.js";
import {RWA_MULTIPLY_ROUTE as route} from "../domain/rwa-multiply-route-spec.js";
import {catalogSwapEdges,validateJupiterHeader} from "../policies/rwa-multiply-jupiter-headers.js";
import {exactJupiterConstraint} from "../policies/rwa-multiply-jupiter-constraint.js";
import {buildExactJupiterSquadsExecution} from "./rwa-phase2-jupiter-execution.js";
import {prepareOnReLending} from "./prepare-phase3-onre-lending.js";

const sha=(data:Uint8Array|string)=>createHash("sha256").update(data).digest("hex");
class ProbeBoundaryError extends Error {}
function require(value:unknown,message:string):asserts value {if(!value)throw new ProbeBoundaryError(message);}
let stage="arguments";
const publicRows:any[]=[];
try {
  const directory=process.argv[2];
  require(directory&&/^\/private\/tmp\/backyard-phase3-jupiter-probe\.[A-Za-z0-9]+$/.test(directory),"explicit local probe directory required");
  require(process.argv[3]===undefined||["--return","--lending-return","--onre-roundtrip","--onre-lending-roundtrip","--onre-leverage-roundtrip","--onre-bridge-roundtrip"].includes(process.argv[3]),"unknown probe mode");
  const onreBridgeRequested=process.argv[3]==="--onre-bridge-roundtrip";
  const onreLeverageRequested=process.argv[3]==="--onre-leverage-roundtrip"||onreBridgeRequested;
  const onreLendingRequested=process.argv[3]==="--onre-lending-roundtrip"||onreLeverageRequested;
  const onre=process.argv[3]==="--onre-roundtrip"||onreLendingRequested;
  const returning=process.argv[3]!==undefined&&!onre;
  const linked=process.argv[3]==="--lending-return";
  const lendingPrelude=linked?JSON.parse(readFileSync(resolve(directory,"kamino-plan.json"),"utf8")):undefined;
  if(linked)require(lendingPrelude.schema==="phase3-kamino-controlled-probe/v1"&&lendingPrelude.broadcast===false&&lendingPrelude.signatureProof===false&&lendingPrelude.lane==="Ethena/USDe/PYUSD"&&JSON.stringify(lendingPrelude.steps.map((s:any)=>s.leg))===JSON.stringify(["deposit","borrow","repay","withdraw"]),"invalid local Go lending prelude");
  require(process.env.SOLANA_RPC_URL,"RPC missing");
  stage="finalized Settings identity";
  const rpc=new Connection(process.env.SOLANA_RPC_URL,{commitment:"finalized",disableRetryOnRateLimit:true,fetch:(input,init)=>fetch(input,{...init,signal:AbortSignal.timeout(30_000)})});
  require(await rpc.getGenesisHash()===route.genesisHash,"genesis drift");
  const settings=await rpc.getAccountInfoAndContext(new PublicKey(route.squads.settings),{commitment:"finalized"});
  require(settings.value?.owner.toBase58()===route.squads.program,"Settings identity drift");
  const [state]=(generated as any).Settings.fromAccountInfo(settings.value);
  require(state.threshold===1&&state.timeLock===0&&state.signers.length===1&&state.signers[0].key.toBase58()===route.setupAdmin&&state.signers[0].permissions.mask===7,"Settings authority drift");
  const seedBefore=BigInt(state.policySeed.toString());
  let artifactBytes=onre?Buffer.alloc(0):readFileSync(new URL(returning?"../../../../docs/evidence/backyard-rwa-go/phase3/jupiter-v2-return-repair-candidates-2026-09-05.json":"../../../../docs/evidence/backyard-rwa-go/phase3/jupiter-v2-repair-candidates-2026-09-04.json",import.meta.url));
  const artifact=onre?{schema:"phase3-jupiter-v2-repair-candidates/v1",broadcast:false,installed:false,groups:[] as any[]}:JSON.parse(artifactBytes.toString());
  require(artifact.schema==="phase3-jupiter-v2-repair-candidates/v1"&&artifact.broadcast===false&&artifact.installed===false&&artifact.groups.length===(onre?0:returning?3:2),"candidate scope drift");
  const installedCatalog=JSON.parse(readFileSync(new URL("../../../../docs/evidence/backyard-rwa-go/policy-compiled-v1.json",import.meta.url),"utf8"));
  const installedReadback=JSON.parse(readFileSync(new URL("../../../../docs/evidence/backyard-rwa-go/policy-install-readback-v1.json",import.meta.url),"utf8"));
  const addresses=new Set<string>([route.squads.settings,route.setupAdmin,route.squads.delegatedExecutor,route.squads.program,"11111111111111111111111111111111","SysvarC1ock11111111111111111111111111111111"]);
  const policies:Record<string,string>={};const steps=[];const candidatePolicies:Array<{edge:string;seed:string;policy:string}>=[];
  let amount=100_000n;
  for(const [i,key] of (onre?["USDC->ONyc","ONyc->USDC"]:returning?["USDe->USDC","PYUSD->USDC"]:["USDC->USDe","USDe->PYUSD"]).entries()) {
    stage=key+" public quote";
    const edge=catalogSwapEdges().find(e=>e.key===key)!;let group=artifact.groups.find((g:any)=>g.edge===key);
    const installedOnly=returning&&i===1;
    require(edge&&(group||installedOnly||onre),"candidate edge absent");
    // Sizing assumptions must match actual lending poststate. The runner never
    // patches custody to make these quotes fit.
    if(returning)amount=linked?(i===0?99_999_999n:2_000n):(i===0?2_224_590n:96_256n);
    const query=new URLSearchParams({inputMint:edge.source.mint,outputMint:edge.destination.mint,amount:amount.toString(),slippageBps:"50",swapMode:"ExactIn",maxAccounts:"32",instructionVersion:installedOnly?"V1":"V2"});
    // The first OnRe multi-hop sample exhausted the existing 200k envelope.
    // Test the complete direct roundtrip before adding compute machinery.
    // This request filter changes no policy constraints or production routing.
    if(onre)query.set("onlyDirectRoutes","true");
    const q=await fetch("https://lite-api.jup.ag/swap/v1/quote?"+query,{signal:AbortSignal.timeout(20_000)});require(q.ok,"quote HTTP failure");const quote=await q.json() as any;
    require(quote.inputMint===edge.source.mint&&quote.outputMint===edge.destination.mint&&quote.inAmount===amount.toString()&&quote.swapMode==="ExactIn"&&quote.slippageBps===50&&quote.routePlan.length>0&&quote.routePlan.length<=4,"quote identity drift");
    stage=key+" public instructions";
    const s=await fetch("https://lite-api.jup.ag/swap/v1/swap-instructions",{method:"POST",headers:{"content-type":"application/json"},body:JSON.stringify({userPublicKey:route.squads.vault,quoteResponse:quote,wrapAndUnwrapSol:false,useSharedAccounts:true,dynamicComputeUnitLimit:false}),signal:AbortSignal.timeout(20_000)});
    require(s.ok,"instruction HTTP failure");const response=await s.json() as any;
    publicRows.push({edge,quote,response,observedAt:new Date().toISOString()});
    stage=key+" header and policy layout";
    require(!response.setupInstructions?.length&&!response.otherInstructions?.length&&!response.cleanupInstruction&&!response.tokenLedgerInstruction,"unapproved auxiliary instructions");
    const data=Buffer.from(response.swapInstruction.data,"base64");
    const instruction=new TransactionInstruction({programId:new PublicKey(response.swapInstruction.programId),data,keys:response.swapInstruction.accounts.map((a:any)=>({...a,pubkey:new PublicKey(a.pubkey)}))});
    const header=validateJupiterHeader({instruction,sourceMint:edge.source.mint,destinationMint:edge.destination.mint,sourceAta:edge.source.ata,destinationAta:edge.destination.ata,sourceTokenProgram:edge.source.tokenProgram,destinationTokenProgram:edge.destination.tokenProgram,amountRaw:amount,outAmountRaw:BigInt(quote.outAmount)});
    require(installedOnly?header.dialect==="shared-accounts-route-cross-program-reverse":header.dialect==="shared-accounts-route-v2","unexpected policy dialect");
    const row={pass:true,key,source:edge.source,destination:edge.destination,quote:{inAmountRaw:quote.inAmount,outAmountRaw:quote.outAmount},header,
      instruction:{...response.swapInstruction,dataBase64:response.swapInstruction.data,dataSha256:sha(data)},lookupTables:response.addressLookupTableAddresses};
    const exact=exactJupiterConstraint(row);
    if(onre) {
      // Closed OnRe diagnostic: retain the original logical group and every
      // sibling constraint. This does not install policies or register a lane.
      const originals=installedCatalog.policies.filter((p:any)=>p.swapEdges?.some((e:any)=>e.from===edge.from&&e.to===edge.to));
      require(originals.length===1,"OnRe original group missing or duplicate");
      const original=originals[0];
      const oldEdge=original.swapEdges.find((e:any)=>e.from===edge.from&&e.to===edge.to);
      const old=original.constraints[oldEdge.constraintIndex];
      const readback=installedReadback.operations.find((p:any)=>p.policyAddress===original.policy);
      require(readback?.active===true&&sha(Buffer.from(readback.dataBase64,"base64"))===readback.dataSha256,"OnRe original policy provenance drift");
      for(const field of ["authority","sourceCustody","destinationCustody","sourceMint","destinationMint","sourceTokenProgram","destinationTokenProgram"])
        require(oldEdge[field]===exact.edge[field],"OnRe replacement changes an authority or asset boundary");
      require(old.programId===exact.constraint.programId&&old.data.find((d:any)=>d.kind==="u64-less-than-or-equal")?.value===1_000_000_000_000&&
        old.data.find((d:any)=>d.kind==="u16-less-than-or-equal")?.value===50&&old.data.find((d:any)=>d.kind==="u8-equals")?.value===0,"OnRe replacement changes economic authority");
      group={edge:key,originalPolicy:original.policy,originalPolicyDataSha256:readback.dataSha256,
        originalConstraints:original.constraints,replacedConstraintIndex:oldEdge.constraintIndex,
        replacementConstraints:original.constraints.map((c:any,index:number)=>index===oldEdge.constraintIndex?{operation:null,...exact.constraint}:c)};
      artifact.groups.push(group);
    }
    let compiledPolicy:any;
    if(installedOnly) {
      const matches=installedCatalog.policies.filter((p:any)=>p.swapEdges?.some((e:any)=>e.from===edge.from&&e.to===edge.to));
      require(matches.length===1,"installed return policy absent or duplicate");
      compiledPolicy=matches[0];
      const installedEdge=compiledPolicy.swapEdges.find((e:any)=>e.from===edge.from&&e.to===edge.to);
      require(JSON.stringify({operation:null,...exact.constraint})===JSON.stringify(compiledPolicy.constraints[installedEdge.constraintIndex]),"fresh return quote differs from installed policy semantics");
      const readback=installedReadback.operations.find((p:any)=>p.policyAddress===compiledPolicy.policy);
      require(readback?.active===true&&sha(Buffer.from(readback.dataBase64,"base64"))===readback.dataSha256,"installed return provenance drift");
      addresses.add(compiledPolicy.policy);policies[compiledPolicy.policy]=readback.dataSha256;
    } else {
      require(JSON.stringify({operation:null,...exact.constraint})===JSON.stringify(group.replacementConstraints[group.replacedConstraintIndex]),"fresh instruction differs from candidate semantics");
      const seed=seedBefore+BigInt(candidatePolicies.length+1);const seedBytes=Buffer.alloc(8);seedBytes.writeBigUInt64LE(seed);
      const policy=PublicKey.findProgramAddressSync([Buffer.from("smart_account"),Buffer.from("policy"),new PublicKey(route.squads.settings).toBuffer(),seedBytes],new PublicKey(route.squads.program))[0];
      compiledPolicy={logicalName:"swap/"+key,seed:seed.toString(),policy:policy.toBase58(),constraints:group.replacementConstraints,constraintCount:group.replacementConstraints.length,swapEdges:[{...exact.edge,constraintIndex:group.replacedConstraintIndex}]};
      addresses.add(group.originalPolicy);policies[group.originalPolicy]=group.originalPolicyDataSha256;
      candidatePolicies.push({edge:key,seed:seed.toString(),policy:policy.toBase58()});
    }
    stage=key+" SDK compilation and lookup reads";
    const execution=await buildExactJupiterSquadsExecution({connection:rpc,delegatedSigner:new PublicKey(route.squads.delegatedExecutor),headerRow:row,
      compiledPolicy});
    const builder=new TransactionMessage({payerKey:new PublicKey(route.squads.delegatedExecutor),recentBlockhash:route.squads.vault,instructions:[execution.outerInstruction]});
    let message:ReturnType<typeof builder.compileToLegacyMessage>|ReturnType<typeof builder.compileToV0Message>;
    if(returning) {
      // Match production preparation: use legacy when it fits. The installed
      // PYUSD return currently has no v0 binding; do not invent one for a probe.
      const legacy=builder.compileToLegacyMessage();
      let fits=false;
      try {fits=new VersionedTransaction(legacy).serialize().length<=1232;} catch {}
      require(fits||!installedOnly,"installed return requires unsupported packet encoding");
      message=fits?legacy:builder.compileToV0Message([...execution.lookupTables]);
    } else message=builder.compileToV0Message([...execution.lookupTables]);
    const wire=new VersionedTransaction(message).serialize();require(wire.length<=1232&&wire[0]===1&&wire.slice(1,65).every(b=>b===0),"unsigned packet envelope drift");
    for(const a of [...message.staticAccountKeys,...instruction.keys.map(a=>a.pubkey),...execution.lookupTables.map(t=>t.key)])addresses.add(a.toBase58());
    const minimum=BigInt(quote.otherAmountThreshold);require(minimum>0n&&minimum<=BigInt(quote.outAmount),"invalid minimum output");
    steps.push({action:onre?(i===0?"SWAP_STABLE_TO_COLLATERAL_STEP":"SWAP_COLLATERAL_TO_STABLE_STEP"):returning?(i===0?"SWAP_COLLATERAL_TO_STABLE_STEP":"SWAP_DEBT_TO_USDC_STEP"):(i===0?"SWAP_STABLE_TO_COLLATERAL_STEP":"SWAP_COLLATERAL_TO_DEBT_STEP"),source:edge.source.ata,destination:edge.destination.ata,amountRaw:Number(amount),minimumOutputRaw:Number(minimum),instructionDataBase64:row.instruction.dataBase64,amountOffset:installedOnly?data.length-19:9,policyMaximumInputRaw:1_000_000_000_000,wireBase64:Buffer.from(wire).toString("base64"),wireSha256:sha(wire),headerRow:row});
    // The OnRe runner must consume the actual entry output, with no custody
    // patch between swaps. A stale/mismatching quote fails that assertion.
    amount=onre?BigInt(quote.outAmount):minimum;
  }
  if(linked) {
    require(lendingPrelude.delegate===route.squads.delegatedExecutor&&lendingPrelude.collateralCustody===steps[0]!.source&&lendingPrelude.debtCustody===steps[1]!.source,"lending custody differs from return custody");
    for(const a of lendingPrelude.addresses)addresses.add(a);
    for(const [a,hash] of Object.entries(lendingPrelude.policies)) {
      require(!policies[a]||policies[a]===hash,"conflicting installed policy hashes");
      policies[a]=hash as string;
    }
  }
  let onreLending;
  if(onreLendingRequested) {
    stage="OnRe linked lending construction";
    const prepared=await prepareOnReLending(rpc,seedBefore,BigInt(steps[1]!.amountRaw),onreLeverageRequested);
    const {groups,candidates,policies:originalPolicies,addresses:extraAddresses,...lending}=prepared;
    artifact.groups.push(...groups);candidatePolicies.push(...candidates);
    for(const a of extraAddresses)addresses.add(a);
    for(const [a,hash] of Object.entries(originalPolicies)){require(!policies[a]||policies[a]===hash,"lending policy conflict");policies[a]=hash;}
    onreLending=lending;
  }
  let onreBridge;
  if(onreBridgeRequested) {
    stage="existing Go bridge account discovery";
    const binary=process.env.PHASE3_ONRE_BRIDGE_COMPILER;
    require(binary==="/private/tmp/phase3-onre-bridge-test","explicit local test compiler required");
    const result=spawnSync(binary,["-test.run=^TestExportOnReBridgeProbe$"],{env:{...process.env,PHASE3_ONRE_BRIDGE_INPUT:JSON.stringify({discover:true})},timeout:30_000});
    require(result.status===0,"local Go bridge discovery failed");
    const line=result.stdout.toString().split("\n").find(s=>s.startsWith("ONRE_BRIDGE_JSON="));
    require(line,"local Go bridge discovery absent");
    onreBridge=JSON.parse(line.slice("ONRE_BRIDGE_JSON=".length));
    require(onreBridge.broadcast===false&&onreBridge.signatureProof===false&&onreBridge.discoveryOnly===true&&onreBridge.steps.length===4,"bridge discovery scope drift");
    for(const a of onreBridge.addresses)addresses.add(a);
    for(const [a,hash] of Object.entries(onreBridge.policies)){require(!policies[a]||policies[a]===hash,"bridge policy conflict");policies[a]=hash as string;}
  }
  require(addresses.size<=100,"coherent batch too large");
  if(onre) {
    artifactBytes=Buffer.from(JSON.stringify(artifact,null,2)+"\n");
    writeFileSync(resolve(directory,"candidate.json"),artifactBytes,{flag:"wx",mode:0o600});
    writeFileSync(resolve(directory,"public-quotes.json"),JSON.stringify({broadcast:false,signatureProof:false,rows:publicRows},null,2)+"\n",{flag:"wx",mode:0o600});
  }
  const plan={schema:"phase3-jupiter-controlled-probe/v1",broadcast:false,compiler:"TYPESCRIPT_CANDIDATE_NOT_INSTALLED_GO",...(onre?{profile:"ONRE_ROUNDTRIP"}:returning?{profile:"RETURN_CONVERSIONS",initialBalancesProof:"LOCAL_POST_PAYOFF_SIZING_PRECONDITIONS_NOT_EXECUTED_LENDING_OR_BRIDGE"}:{}),lane:onre?"OnRe/ONyc/USDC":"Ethena/USDe/PYUSD",delegate:route.squads.delegatedExecutor,inputCustody:returning?steps[0]!.destination:steps[0]!.source,collateralCustody:returning?steps[0]!.source:steps[0]!.destination,debtCustody:returning?steps[1]!.source:steps[1]!.destination,steps,policies,addresses:[...addresses].sort(),
    ...(onreLending?{onreLending}:{}),
    ...(onreBridge?{onreBridge}:{}),
    ...(linked?{lendingPrelude,initialBalancesProof:"LOCAL_FUNDED_LENDING_PRECONDITION_WITH_CONTINUOUS_EXECUTED_RETURNS_NOT_BRIDGE_ENTRY"}:{}),
    candidate:{artifactSha256:sha(artifactBytes),settings:route.squads.settings,settingsDataSha256:sha(settings.value.data),settingsSlot:settings.context.slot,setupAdmin:route.setupAdmin,seedBefore:seedBefore.toString(),policies:candidatePolicies}};
  writeFileSync(resolve(directory,"plan.json"),JSON.stringify(plan,null,2)+"\n",{flag:"wx",mode:0o600});
  console.log(JSON.stringify({broadcast:false,compiler:plan.compiler,steps:steps.map(s=>({action:s.action,packetBytes:Buffer.from(s.wireBase64,"base64").length})),accounts:addresses.size}));
} catch(error) {
  // Only explicitly authored boundary messages are safe to print. RPC errors
  // may contain credential-bearing URLs; never forward their message or stack.
  const reason=error instanceof ProbeBoundaryError?error.message:"PUBLIC_RPC_OR_CONSTRUCTION_ERROR";
  const directory=process.argv[2];
  if(directory&&/^\/private\/tmp\/backyard-phase3-jupiter-probe\.[A-Za-z0-9]+$/.test(directory)&&publicRows.length>0) {
    writeFileSync(resolve(directory,"preparation-diagnostic.json"),JSON.stringify({broadcast:false,signatureProof:false,stage,reason,publicRows},null,2)+"\n",{flag:"wx",mode:0o600});
  }
  console.error(JSON.stringify({broadcast:false,signatureProof:false,stage,reason}));process.exitCode=1;
}
