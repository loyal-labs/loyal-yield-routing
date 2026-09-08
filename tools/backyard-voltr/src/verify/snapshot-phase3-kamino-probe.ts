// Read-only capture for a bounded local SVM experiment. Never imports a signer
// or builds/submits transactions; the input is exported by the Go compiler.
import {createHash} from "node:crypto";
import {readFileSync,writeFileSync} from "node:fs";
import {resolve} from "node:path";
import {Connection,PublicKey} from "@solana/web3.js";

const sha=(b:Uint8Array)=>createHash("sha256").update(b).digest("hex");
const programs=["SMRTzfY6DfH5ik3TKiyLFfXexV8uSG3d2UksSCYdunG","KLend2g3cP87fffoy8q1mQqGKjrxjC8boSyAYavgmjD",
  "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA","TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb","FarmsPZpWu9i7Kky8tPN37rs2TpmMrAZrC7S7vJa91Hr"];
try {
  const directory=process.argv[2];
  if(!directory||!/^\/private\/tmp\/backyard-phase3-kamino-probe\.[A-Za-z0-9]+$/.test(directory)||!process.env.SOLANA_RPC_URL)throw new Error();
  const plan=JSON.parse(readFileSync(resolve(directory,"plan.json"),"utf8"));
  if(plan.schema!=="phase3-kamino-controlled-probe/v1"||plan.lane!=="Ethena/USDe/PYUSD"||plan.broadcast!==false||plan.addresses.length>100)throw new Error();
  const rpc=new Connection(process.env.SOLANA_RPC_URL,{commitment:"finalized",disableRetryOnRateLimit:true,
    fetch:(input,init)=>fetch(input,{...init,signal:AbortSignal.timeout(30_000)})});
  const genesis=await rpc.getGenesisHash();if(genesis!=="5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d")throw new Error();
  let minSlot=await rpc.getSlot("finalized");
  const identities=[];
  for(const program of programs){
    const p=await rpc.getAccountInfoAndContext(new PublicKey(program),{commitment:"finalized",minContextSlot:minSlot});
    if(!p.value?.executable)throw new Error();
    let elf:Buffer;let deploymentSlot:string|null=null;let programData:string|null=null;
    if(p.value.owner.toBase58()==="BPFLoaderUpgradeab1e11111111111111111111111"){
      if(p.value.data.length!==36||p.value.data.readUInt32LE(0)!==2)throw new Error();
      programData=new PublicKey(p.value.data.subarray(4,36)).toBase58();
      const d=await rpc.getAccountInfoAndContext(new PublicKey(programData),{commitment:"finalized",minContextSlot:p.context.slot});
      if(!d.value||d.value.executable||!d.value.owner.equals(p.value.owner)||d.value.data.readUInt32LE(0)!==3)throw new Error();
      elf=d.value.data.subarray(45);deploymentSlot=d.value.data.readBigUInt64LE(4).toString();minSlot=d.context.slot;
    }else if(p.value.owner.toBase58()==="BPFLoader2111111111111111111111111111111111"){
      elf=p.value.data;minSlot=p.context.slot;
    }else throw new Error();
    if(elf.subarray(0,4).toString("hex")!=="7f454c46")throw new Error();
    const file=program+".so";writeFileSync(resolve(directory,file),elf,{flag:"wx",mode:0o600});
    identities.push({program,programData,deploymentSlot,slot:minSlot,file,elfSha256:sha(elf)});
  }
  const batch=await rpc.getMultipleAccountsInfoAndContext(plan.addresses.map((a:string)=>new PublicKey(a)),{commitment:"finalized",minContextSlot:minSlot});
  const accounts=batch.value.map((a,i)=>({address:plan.addresses[i],present:!!a,...(a?{owner:a.owner.toBase58(),lamports:a.lamports,executable:a.executable,dataBase64:a.data.toString("base64"),dataSha256:sha(a.data)}:{})}));
  for(const [address,hash] of Object.entries(plan.policies)){
    const a=accounts.find(a=>a.address===address);if(!a||a.dataSha256!==hash||a.owner!==programs[0])throw new Error();
  }
  const evidence={schema:"phase3-kamino-svm-snapshot/v1",broadcast:false,genesis,slot:batch.context.slot,programs:identities,accounts};
  writeFileSync(resolve(directory,"snapshot.json"),JSON.stringify(evidence,null,2)+"\n",{flag:"wx",mode:0o600});
  console.log(JSON.stringify({slot:evidence.slot,programs:identities,accounts:accounts.length,absent:accounts.filter(a=>!a.present).map(a=>a.address)}));
}catch{console.error("Read-only Kamino probe snapshot unavailable or identity changed");process.exitCode=1;}
