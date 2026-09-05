// Public account/program capture for zero-signature local execution only.
import {createHash} from "node:crypto";
import {readFileSync,writeFileSync} from "node:fs";
import {resolve} from "node:path";
import {Connection,PublicKey} from "@solana/web3.js";

const sha=(b:Uint8Array)=>createHash("sha256").update(b).digest("hex");
try {
  const directory=process.argv[2];
  if(!directory||!/^\/private\/tmp\/backyard-phase3-jupiter-probe\.[A-Za-z0-9]+$/.test(directory)||!process.env.SOLANA_RPC_URL)throw new Error();
  const plan=JSON.parse(readFileSync(resolve(directory,"plan.json"),"utf8"));
  if(plan.schema!=="phase3-jupiter-controlled-probe/v1"||plan.lane!=="Ethena/USDe/PYUSD"||plan.broadcast!==false||plan.steps.length!==2||plan.addresses.length>100)throw new Error();
  const rpc=new Connection(process.env.SOLANA_RPC_URL,{commitment:"finalized",disableRetryOnRateLimit:true,
    fetch:(input,init)=>fetch(input,{...init,signal:AbortSignal.timeout(30_000)})});
  const genesis=await rpc.getGenesisHash();if(genesis!=="5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d")throw new Error();
  const batch=await rpc.getMultipleAccountsInfoAndContext(plan.addresses.map((a:string)=>new PublicKey(a)),{commitment:"finalized"});
  const accounts=batch.value.map((a,i)=>({address:plan.addresses[i],present:!!a,...(a?{owner:a.owner.toBase58(),lamports:a.lamports,executable:a.executable,dataBase64:a.data.toString("base64"),dataSha256:sha(a.data)}:{})}));
  for(const [address,hash] of Object.entries(plan.policies)){
    const a=accounts.find(a=>a.address===address);if(!a||a.dataSha256!==hash||a.owner!=="SMRTzfY6DfH5ik3TKiyLFfXexV8uSG3d2UksSCYdunG")throw new Error();
  }
  const programs=[];
  for(let i=0;i<batch.value.length;i++){
    const p=batch.value[i];if(!p?.executable)continue;
    const program=plan.addresses[i];
    if(p.owner.toBase58()==="NativeLoader1111111111111111111111111111111"){
      if(!["11111111111111111111111111111111","ComputeBudget111111111111111111111111111111","AddressLookupTab1e1111111111111111111111111"].includes(program))throw new Error();
      continue;
    }
    let elf:Buffer;let programData:string|null=null;let deploymentSlot:string|null=null;
    if(p.owner.toBase58()==="BPFLoaderUpgradeab1e11111111111111111111111"){
      if(p.data.length!==36||p.data.readUInt32LE(0)!==2)throw new Error();
      programData=new PublicKey(p.data.subarray(4,36)).toBase58();
      const d=await rpc.getAccountInfoAndContext(new PublicKey(programData),{commitment:"finalized",minContextSlot:batch.context.slot});
      if(!d.value||d.value.executable||!d.value.owner.equals(p.owner)||d.value.data.readUInt32LE(0)!==3)throw new Error();
      deploymentSlot=d.value.data.readBigUInt64LE(4).toString();
      if(BigInt(deploymentSlot)>BigInt(batch.context.slot))throw new Error();
      elf=d.value.data.subarray(45);
    }else if(p.owner.toBase58()==="BPFLoader2111111111111111111111111111111111")elf=p.data;
    else throw new Error();
    if(elf.subarray(0,4).toString("hex")!=="7f454c46")throw new Error();
    const file=program+".so";writeFileSync(resolve(directory,file),elf,{flag:"wx",mode:0o600});
    programs.push({program,programData,deploymentSlot,file,elfSha256:sha(elf)});
  }
  if(!programs.some(p=>p.program==="JUP6LkbZbjS1jKKwapdHNy74zcZ3tLUZoi5QNyVTaV4")||!programs.some(p=>p.program==="SMRTzfY6DfH5ik3TKiyLFfXexV8uSG3d2UksSCYdunG"))throw new Error();
  const snapshot={schema:"phase3-jupiter-svm-snapshot/v1",broadcast:false,genesis,slot:batch.context.slot,programs,accounts};
  writeFileSync(resolve(directory,"snapshot.json"),JSON.stringify(snapshot,null,2)+"\n",{flag:"wx",mode:0o600});
  console.log(JSON.stringify({slot:snapshot.slot,programs,accounts:accounts.length,absent:accounts.filter(a=>!a.present).map(a=>a.address)}));
}catch{console.error("Read-only Jupiter snapshot unavailable, incomplete or changed identity");process.exitCode=1;}
