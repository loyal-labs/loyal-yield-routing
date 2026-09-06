// A local six-leg subclaim, not production authority or full lifecycle proof.
import {createHash} from "node:crypto";
import {isDeepStrictEqual} from "node:util";
import {VersionedTransaction} from "@solana/web3.js";

type Json=Record<string,any>;
const hash=(b:Uint8Array)=>createHash("sha256").update(b).digest("hex");
const sameSet=(a:string[],b:string[])=>a.length===b.length&&new Set(a).size===a.length&&a.every(x=>b.includes(x));
const tokenProgram="TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA";
const kamino="KLend2g3cP87fffoy8q1mQqGKjrxjC8boSyAYavgmjD";
const squads="SMRTzfY6DfH5ik3TKiyLFfXexV8uSG3d2UksSCYdunG";
const finite=(n:unknown):n is number=>Number.isSafeInteger(n)&&Number(n)>0&&Number(n)<=1_000_000_000_000;

export function onreSetupStagingProof(e:Json):boolean {
  try {
    return e.schema==="phase3-onre-lending-roundtrip-result/v1"&&e.broadcast===false&&e.installedPolicyProof===false&&e.candidateCreation.length===4&&
      e.candidateCreation.every((c:Json,i:number)=>{
        const s=c.stagedComparison;
        const account=c.after.find((a:Json)=>a.address===c.policy);
        return s?.broadcast===false&&s.installed===false&&s.comparisonOnly===true&&s.samePolicyBytesAndBalance===true&&s.sameSettings===true&&
          s.allocatedBytes===[1383,1383,1400,1250][i]&&account?.present===true&&account.owner===squads&&
          Buffer.from(account.dataBase64,"base64").length===s.allocatedBytes&&hash(Buffer.from(account.dataBase64,"base64"))===account.dataSha256&&
          finite(s.localRentLamports)&&account.lamports===s.localRentLamports&&s.firstFundingLamports===Math.floor(s.localRentLamports/2)&&
          s.firstPayerDebitLamports===s.firstFundingLamports+5000&&s.secondPayerDebitLamports===s.localRentLamports-s.firstFundingLamports+5000&&
          finite(s.fundingPacketBytes)&&s.fundingPacketBytes<=1232&&/^[a-f0-9]{64}$/.test(s.fundingWireSha256);
      });
  } catch {return false;}
}

function wire(step:Json,instructions:number) {
  const b=Buffer.from(step.wireBase64,"base64");
  if(b.length>1232||b.length<65||b[0]!==1||b.subarray(1,65).some(x=>x!==0)||hash(b)!==step.wireSha256)throw Error("wire");
  const m=VersionedTransaction.deserialize(b).message;
  if(m.compiledInstructions.length!==instructions)throw Error("instruction count");
  if(m.staticAccountKeys[m.compiledInstructions[instructions-1]!.programIdIndex]!.toBase58()!==squads)throw Error("wrapper");
  return b;
}
function resize(template:Json,inner:Buffer,instructions:number) {
  const b=wire(template,instructions),original=Buffer.from(template.instructionDataBase64,"base64");
  const offset=b.indexOf(original);
  if(inner.length!==original.length||offset<0||b.indexOf(original,offset+1)!==-1)throw Error("ambiguous inner");
  inner.copy(b,offset);return b;
}

export function onreConnectedProof(e:Json,p:Json,snapshot:Json,planHash:string,snapshotHash:string,exitCode:number|null):boolean {
  try {
    const lending=p.onreLending,legs=e.lendingSteps;
    if(exitCode!==0||e.schema!=="phase3-onre-lending-roundtrip-result/v1"||p.profile!=="ONRE_ROUNDTRIP"||p.lane!=="OnRe/ONyc/USDC"||
      p.compiler!=="TYPESCRIPT_CANDIDATE_NOT_INSTALLED_GO"||p.broadcast!==false||
      ["broadcast","signatureProof","installedPolicyProof","goCompilerProof"].some(k=>e[k]!==false)||
      e.planSha256!==planHash||e.snapshotSha256!==snapshotHash||!isDeepStrictEqual(e.programs,snapshot.programs)||e.slot!==snapshot.slot||
      lending?.schema!=="phase3-onre-linked-lending-plan/v1"||lending.broadcast!==false||lending.signatureProof!==false||lending.lane!==p.lane||
      lending.obligation!=="4LnCFir7Qc99GhjGHLcwtkfweyAMu37u5QE1zTupKsei"||
      p.inputCustody!=="EBG2iYrcXttDy9FpWDeNVL8uaCLRCkevrpRyrAhvVYKe"||p.collateralCustody!=="AVX9wxDTk639eZ4KaiMA7LrLhXe7Lg6DaDDVRa1Q7Ji3"||
      p.debtCustody!==p.inputCustody||lending.debtCustody!==p.debtCustody||lending.collateralCustody!==p.collateralCustody||
      p.steps.length!==2||lending.steps.length!==4||legs.length!==4||e.steps.length!==2||e.initialCashBufferRaw!==1000||
      e.twoSwapsPassed!==true||e.onreCollateralCleared!==true)return false;
    const edges=["USDC->ONyc","ONyc->USDC","OnRe/borrow","OnRe/repay"];
    if(!sameSet(p.candidate.policies.map((c:Json)=>c.edge),edges)||e.candidateCreation.length!==4||
      !e.candidateCreation.every((c:Json,i:number)=>c.candidateOnly===true&&c.policy===p.candidate.policies[i].policy&&c.seed===Number(p.candidate.policies[i].seed)&&finite(c.packetBytes)&&c.packetBytes<=1232))return false;
    const overrides=[
      ["62JLkPeE4oG65LRB3W3m52RVicmYq3xFHdv7TecCsPj5","lamports",1_000_000_000],
      [p.inputCustody,"tokenAmount",p.steps[0].amountRaw+1000],
      [p.collateralCustody,"tokenAmount",0],
      ["BAqgbERmvUViqDSx961xpRBHGt68SpACiWL4t9696qZZ","lamports",1_000_000_000],
    ];
    if(e.overrides.length!==overrides.length||!e.overrides.every((o:Json,i:number)=>{
      const [address,field,after]=overrides[i]!,a=snapshot.accounts.find((a:Json)=>a.address===address);
      const before=field==="lamports"?a.lamports:Number(Buffer.from(a.dataBase64,"base64").readBigUInt64LE(64));
      return o.address===address&&o.field===field&&o.after===after&&o.before===before;
    }))return false;
    const programs=new Set(snapshot.accounts.filter((a:Json)=>a.executable===true).map((a:Json)=>a.address));
    const addresses=p.addresses.filter((a:string)=>!programs.has(a));
    if(!sameSet(e.stateAddresses,addresses))return false;
    const data=(rows:Json[],address:string,owner:string)=>{
      const a=rows.find(a=>a.address===address);
      if(a?.present!==true||a.owner!==owner)throw Error("account identity");
      return Buffer.from(a.dataBase64,"base64");
    };
    const token=(rows:Json[],address:string)=>data(rows,address,tokenProgram).readBigUInt64LE(64);
    const position=(rows:Json[])=>{
      const a=rows.find(a=>a.address===lending.obligation);
      if(a?.present===false)return [0n,0n];
      const b=data(rows,lending.obligation,kamino);if(b.length!==3344)throw Error("obligation layout");
      let receipts=0n,debt=0n;
      for(let i=0;i<8;i++)receipts+=b.readBigUInt64LE(128+i*136);
      for(let i=0;i<5;i++)debt+=b.readBigUInt64LE(1296+i*200)+(b.readBigUInt64LE(1304+i*200)<<64n);
      return [receipts,debt];
    };
    const chain=[e.steps[0],...legs,e.steps[1]];
    for(const [i,s] of chain.entries()) {
      for(const rows of [s.before,s.after])if(!sameSet(rows.map((a:Json)=>a.address),addresses)||rows.some((a:Json)=>a.present!==false&&(a.present!==true||hash(Buffer.from(a.dataBase64,"base64"))!==a.dataSha256)))return false;
      if(i>0&&!isDeepStrictEqual(chain[i-1].after,s.before))return false;
      if(s.error!==null||!finite(s.computeUnits)||s.computeUnits>(i===0||i===5?200_000:800_000))return false;
    }
    // The deployed full withdrawal closes the empty obligation and returns its
    // rent to this existing vault. Observing it does not authorize a live close.
    const withdrawal=legs[3],obligationBefore=withdrawal.before.find((a:Json)=>a.address===lending.obligation);
    const obligationAfter=withdrawal.after.find((a:Json)=>a.address===lending.obligation);
    const vault="ST999VUTo5QExYEX9bz1oDDoKGkjXG9zpphy4Hj7VWh";
    const vaultBefore=withdrawal.before.find((a:Json)=>a.address===vault),vaultAfter=withdrawal.after.find((a:Json)=>a.address===vault);
    if(obligationBefore?.present!==true||obligationAfter?.present!==false||!finite(obligationBefore.lamports)||
      vaultBefore?.present!==true||vaultAfter?.present!==true||vaultAfter.lamports-vaultBefore.lamports!==obligationBefore.lamports)return false;
    if(!position(chain[0].before).every(n=>n===0n)||!position(chain[5].after).every(n=>n===0n)||
      token(chain[0].before,p.inputCustody)!==BigInt(p.steps[0].amountRaw)+1000n||token(chain[0].before,p.collateralCustody)!==0n||
      token(chain[5].after,p.collateralCustody)!==0n||token(chain[5].after,p.inputCustody)!==BigInt(e.terminalUSDCRaw))return false;
    for(const [i,s] of legs.entries()) {
      const template=lending.steps[i],inner=Buffer.from(template.instructionDataBase64,"base64");
      const [receipts,debt]=position(s.before),[afterReceipts,afterDebt]=position(s.after);
      const amount=i===0?token(s.before,p.collateralCustody):i===1?1000n:i===2?(debt!+(1n<<60n)-1n)>>60n:receipts!;
      if(!finite(s.amountRaw)||BigInt(s.amountRaw)!==amount||inner.length!==16||s.leg!==["deposit","borrow","repay","withdraw"][i]||s.leg!==template.leg||s.templateSha256!==template.wireSha256)return false;
      inner.writeBigUInt64LE(amount,8);
      if(!resize(template,inner,4).equals(wire(s,4))||s.negative?.rejectedBeforeKaminoCPI!==true||s.negative?.custodyUnchanged!==true||
        !String(s.negative.error).startsWith("InstructionError(3, Custom(")||s.pass!==true||
        (afterReceipts!>0n)!==(i<3)||(afterDebt!>0n)!==(i===1)||BigInt(s.receiptRaw)!==afterReceipts||BigInt(s.debtSF)!==afterDebt)return false;
      const cashDelta=token(s.after,p.inputCustody)-token(s.before,p.inputCustody),collateralDelta=token(s.after,p.collateralCustody)-token(s.before,p.collateralCustody);
      if(i===0&&(collateralDelta!==-amount||cashDelta!==0n)||i===1&&(collateralDelta!==0n||cashDelta<=0n||cashDelta>amount||afterDebt!==amount<<60n)||
        i===2&&(collateralDelta!==0n||cashDelta!==-amount)||i===3&&(collateralDelta<=0n||cashDelta!==0n||debt!==0n))return false;
    }
    for(const [i,s] of e.steps.entries()) {
      const t=p.steps[i],r=s.executedRequest,inner=Buffer.from(t.instructionDataBase64,"base64");
      const amount=i===0?BigInt(t.amountRaw):token(s.before,p.collateralCustody);
      const quoted=inner.readBigUInt64LE(17)*amount/BigInt(t.amountRaw),minimum=BigInt(t.minimumOutputRaw)*amount/BigInt(t.amountRaw);
      inner.writeBigUInt64LE(amount,9);inner.writeBigUInt64LE(quoted,17);
      const expected={...t,amountRaw:Number(amount),minimumOutputRaw:Number(minimum),instructionDataBase64:inner.toString("base64")};
      const bytes=resize(t,inner,1);Object.assign(expected,{wireBase64:bytes.toString("base64"),wireSha256:hash(bytes)});
      if(!isDeepStrictEqual(r,expected)||!finite(r.amountRaw)||!finite(r.minimumOutputRaw)||!bytes.equals(wire(r,1))||s.wireSha256!==r.wireSha256||
        s.action!==["SWAP_STABLE_TO_COLLATERAL_STEP","SWAP_COLLATERAL_TO_STABLE_STEP"][i]||r.source!==(i===0?p.inputCustody:p.collateralCustody)||r.destination!==(i===0?p.collateralCustody:p.inputCustody)||
        token(s.before,r.source)-token(s.after,r.source)!==amount||token(s.after,r.destination)-token(s.before,r.destination)<minimum||
        s.economicPass!==true||s.negative?.rejectedBeforeJupiterCPI!==true||s.negative?.custodyUnchanged!==true||
        !sameSet(s.additionalNegatives.map((n:Json)=>n.mutation),["slippage_above_50_bps","platform_fee_low_byte","platform_fee_high_byte","positive_slippage_fee_low_byte","positive_slippage_fee_high_byte","destination_replaced_by_source"])||
        s.additionalNegatives.some((n:Json)=>n.rejectedBeforeJupiterCPI!==true||n.custodyUnchanged!==true))return false;
    }
    return finite(e.terminalUSDCRaw);
  } catch {return false;}
}
