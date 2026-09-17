/** Exact, sequential pilot initializer installation. Never retries a saved wire. */
import { createHash } from "node:crypto";
import { existsSync, readFileSync, writeFileSync } from "node:fs";
import { resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { generated } from "@loyal-labs/loyal-smart-accounts-core";
import { Connection, Keypair, PublicKey, Transaction, TransactionMessage, VersionedTransaction } from "@solana/web3.js";
import bs58 from "bs58";
import { signingMaterialFromEnvironment } from "../integrations/signer.js";
import { createPolicyInstruction } from "./rwa-basic-policy-set.js";
import { INITIALIZER_ADMIN as ADMIN, INITIALIZER_SETTINGS as SETTINGS, INITIALIZER_PROGRAM as PROGRAM, INITIALIZER_ARTIFACT_SHA256, readInitializerArtifact, verifyInitializerPolicy } from "./multiply-initializer-artifact.js";

const schema="pilot-initializer-install-attempt/v1";
const sha=(b:Uint8Array)=>createHash("sha256").update(b).digest("hex");
function fail(ok:unknown,message:string):asserts ok {if(!ok)throw new Error(message);}
type SettingsState={policySeed:{toString():string}|null;threshold:number;timeLock:number;signers:readonly {key:PublicKey;permissions:{mask:number}}[]};
const Settings=(generated as unknown as {Settings:{deserialize(b:Buffer):readonly [SettingsState,number]}}).Settings;

export function verifyInitializerAttempt(value:unknown,artifactBytes:Buffer,index:number) {
 const artifact=readInitializerArtifact(artifactBytes),policy=artifact.policies[index];
 if(!policy)throw new Error("Invalid initializer index");
 const row=value as Record<string,unknown>;
 fail(row?.schema===schema&&row.artifactSha256===INITIALIZER_ARTIFACT_SHA256&&row.seed===policy.seed,"Initializer journal identity mismatch");
 fail(typeof row.wireBase64==="string"&&typeof row.signature==="string","Missing initializer signed wire");
 const wire=Buffer.from(row.wireBase64,"base64");
 fail(wire.toString("base64")===row.wireBase64&&sha(wire)===row.wireSha256,"Initializer wire hash mismatch");
 const tx=VersionedTransaction.deserialize(wire);
 const expected=new TransactionMessage({payerKey:new PublicKey(ADMIN),recentBlockhash:tx.message.recentBlockhash,instructions:[createPolicyInstruction(policy)]}).compileToLegacyMessage();
 fail(Buffer.from(expected.serialize()).equals(Buffer.from(tx.message.serialize())),"Initializer wire is not the exact reviewed installation");
 fail(tx.signatures.length===1&&bs58.encode(tx.signatures[0]!)===row.signature&&Transaction.from(wire).verifySignatures(),"Initializer signature verification failed");
 return {policy,tx,wire,signature:row.signature};
}

export function initializerJournalPath(index:number):string {
 fail(Number.isInteger(index)&&index>=0&&index<3,"Choose initializer index 0, 1 or 2");
 return fileURLToPath(new URL(`../../../../docs/evidence/voltr-selector-2026-09-16/initializer-install-seed-${149+index}.json`,import.meta.url));
}

export function verifyInitializerNativeEffects(accountKeys:readonly PublicKey[],policyAddress:string,meta:{fee:number;preBalances:readonly number[];postBalances:readonly number[]}) {
 const payerIndex=accountKeys.findIndex(k=>k.toBase58()===ADMIN),policyIndex=accountKeys.findIndex(k=>k.toBase58()===policyAddress);
 fail(payerIndex>=0&&policyIndex>=0&&payerIndex!==policyIndex&&meta.preBalances.length===accountKeys.length&&meta.postBalances.length===accountKeys.length&&[...meta.preBalances,...meta.postBalances,meta.fee].every(v=>Number.isSafeInteger(v)&&v>=0),"Incomplete finalized initializer balance evidence");
 const rent=meta.postBalances[policyIndex]!;
 fail(meta.fee<=50000&&meta.preBalances[policyIndex]===0&&rent>0&&meta.preBalances[payerIndex]! - meta.postBalances[payerIndex]! === rent+meta.fee&&meta.preBalances.every((v,i)=>i===payerIndex||i===policyIndex||v===meta.postBalances[i]),"Finalized initializer native effects mismatch");
}

export async function installMultiplyInitializer(artifactPath:string,index:number,journalPath:string,execute=false) {
 fail(Number.isInteger(index)&&index>=0&&index<3,"Choose initializer index 0, 1 or 2");
 fail(journalPath===resolve(journalPath)&&journalPath===initializerJournalPath(index),"Use the canonical initializer journal path; alternate paths cannot bypass pending recovery");
 const bytes=readFileSync(artifactPath),artifact=readInitializerArtifact(bytes),policy=artifact.policies[index]!;
 const connection=new Connection(process.env.SOLANA_RPC_URL??"https://api.mainnet-beta.solana.com",{commitment:"finalized",disableRetryOnRateLimit:true,fetch:(url,options)=>fetch(url,{...options,signal:AbortSignal.timeout(15000)})});
 fail(await connection.getGenesisHash()==="5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d","Wrong genesis");
 // Recovery does not need signer access and cannot submit or replace a wire.
 if(existsSync(journalPath)) {
  const saved=verifyInitializerAttempt(JSON.parse(readFileSync(journalPath,"utf8")),bytes,index);
  const status=(await connection.getSignatureStatuses([saved.signature],{searchTransactionHistory:true})).value[0];
  if(!status) {
   const validity=await connection.isBlockhashValid(saved.tx.message.recentBlockhash,{commitment:"finalized"});
   if(!validity.value) {
    const absent=await connection.getMultipleAccountsInfoAndContext([new PublicKey(SETTINGS),new PublicKey(policy.account)],{commitment:"finalized",minContextSlot:validity.context.slot});
    const state=absent.value[0];
    const repeated=(await connection.getSignatureStatuses([saved.signature],{searchTransactionHistory:true})).value[0];
    const transaction=await connection.getTransaction(saved.signature,{commitment:"finalized",maxSupportedTransactionVersion:0});
    if(repeated===null&&transaction===null&&absent.value[1]===null&&state?.owner.toBase58()===PROGRAM&&!state.executable&&Settings.deserialize(state.data)[0].policySeed?.toString()===String(148+index))return {verdict:"EXPIRED_UNSPENT_NO_AUTOMATIC_RETRY",signature:saved.signature,broadcast:false,finalizedSlot:absent.context.slot,blockhashInvalidAtSlot:validity.context.slot};
   }
  }
  if(!status||status.confirmationStatus!=="finalized")return {verdict:"PENDING_RECONCILIATION",signature:saved.signature,broadcast:"unknown"};
  if(status.err!==null)return {verdict:"FINALIZED_FAILED",signature:saved.signature,broadcast:true};
  const landed=await connection.getTransaction(saved.signature,{commitment:"finalized",maxSupportedTransactionVersion:0});
  fail(landed&&landed.slot===status.slot&&landed.meta?.err===null&&landed.blockTime!==null&&landed.blockTime!==undefined&&Buffer.from(landed.transaction.message.serialize()).equals(Buffer.from(saved.tx.message.serialize())),"Finalized initializer transaction mismatch");
  const current=await connection.getAccountInfoAndContext(new PublicKey(policy.account),{commitment:"finalized",minContextSlot:status.slot});
  fail(current.value,"Finalized initializer policy missing");
  const accountDataSHA256=verifyInitializerPolicy(policy,current.value,landed.blockTime);
  verifyInitializerNativeEffects(saved.tx.message.staticAccountKeys,policy.account,landed.meta!);
  return {verdict:"FINALIZED_POLICY_INSTALLED_NOT_ACTIVATED",signature:saved.signature,broadcast:true,slot:status.slot,manifestBinding:{lane:policy.family,policySeed:policy.seed,policy:policy.account,accountDataSHA256}};
 }
 fail(execute&&process.env.CONFIRM_MAINNET==="1","Fresh installation requires --execute and CONFIRM_MAINNET=1");
 const program=await connection.getAccountInfo(new PublicKey(PROGRAM),"finalized");
 const loader="BPFLoaderUpgradeab1e11111111111111111111111";
 fail(program&&program.owner.toBase58()===loader&&program.executable&&program.data.length===36&&program.data.readUInt32LE(0)===2,"Squads executable identity mismatch");
 const programData=new PublicKey(program.data.subarray(4)).toBase58();
 const keys=[SETTINGS,ADMIN,...artifact.policies.map(p=>p.account),PROGRAM,programData].map(x=>new PublicKey(x));
 const before=await connection.getMultipleAccountsInfoAndContext(keys,{commitment:"confirmed"});
 const settings=before.value[0],admin=before.value[1],image=before.value[6];
 fail(before.value[5]?.data.equals(program.data)&&image&&image.owner.toBase58()===loader&&!image.executable&&image.data.length>45&&image.data.readUInt32LE(0)===3&&sha(image.data.subarray(45))==="1c95bd7be140589d2aec38a85d7ecfe70ec69639277f622c898f821ab1d636fa","Squads differs from captured policy proof binary");
 fail(settings&&settings.owner.toBase58()===PROGRAM&&!settings.executable&&admin,"Missing Settings or admin");
 const [decoded]=Settings.deserialize(settings.data);
 fail(decoded.policySeed?.toString()===String(148+index)&&decoded.threshold===1&&decoded.timeLock===0&&decoded.signers.length===1&&decoded.signers[0]?.key.toBase58()===ADMIN&&decoded.signers[0]?.permissions.mask===7,"Settings authority or sequential seed changed");
 for(let i=0;i<3;i++) {
  if(i<index){const prior=before.value[2+i];fail(prior,"Preceding initializer not installed");verifyInitializerPolicy(artifact.policies[i]!,prior);}
  else fail(before.value[2+i]===null,"Candidate initializer address already exists");
 }
 const material=await signingMaterialFromEnvironment("SOLANA_TESTING_PK");
 fail(material.signer.address===ADMIN,"Wrong initializer admin signer");
 const lifetime=await connection.getLatestBlockhashAndContext({commitment:"confirmed",minContextSlot:before.context.slot});
 const latest=lifetime.value;
 const tx=new VersionedTransaction(new TransactionMessage({payerKey:new PublicKey(ADMIN),recentBlockhash:latest.blockhash,instructions:[createPolicyInstruction(policy)]}).compileToLegacyMessage());
 tx.sign([Keypair.fromSeed(material.privateSeed)]);
 fail(tx.serialize().length<=1232,"Initializer packet too large");
 const simulation=await connection.simulateTransaction(tx,{sigVerify:true,commitment:"confirmed",minContextSlot:Math.max(before.context.slot,lifetime.context.slot),accounts:{encoding:"base64",addresses:[SETTINGS,ADMIN,policy.account]}});
 if(simulation.value.err!==null)return {verdict:"SIGNED_SIMULATION_FAILED",broadcast:false,slot:simulation.context.slot,error:simulation.value.err,logs:simulation.value.logs};
 const post=simulation.value.accounts?.[2],postAdmin=simulation.value.accounts?.[1],postSettings=simulation.value.accounts?.[0];
 const time=await connection.getBlockTime(simulation.context.slot);
 fail(post&&typeof post.data[0]==="string"&&post.data[1]==="base64"&&time!==null&&postAdmin&&postSettings,"Incomplete initializer simulation");
 verifyInitializerPolicy(policy,{...post,data:Buffer.from(post.data[0],"base64")},time);
 const fee=(await connection.getFeeForMessage(tx.message,"confirmed")).value;
 const rent=await connection.getMinimumBalanceForRentExemption(934,"finalized");
 if(!(fee!==null&&fee<=50000&&post.lamports===rent&&admin.lamports-postAdmin.lamports===rent+fee&&postSettings.lamports===settings.lamports))return {verdict:"SIGNED_SIMULATION_COST_MISMATCH",broadcast:false,fee,rent,policyLamports:post.lamports,adminDebit:admin.lamports-postAdmin.lamports,settingsDelta:postSettings.lamports-settings.lamports};
 const fresh=await connection.getMultipleAccountsInfoAndContext(keys,{commitment:"confirmed",minContextSlot:simulation.context.slot});
 fail(before.value.every((a,i)=>{const b=fresh.value[i];return a===null?b===null:!!b&&a.owner.equals(b.owner)&&a.executable===b.executable&&a.lamports===b.lamports&&a.data.equals(b.data);}),"Initializer protected state changed after simulation");
 const wire=tx.serialize(),signature=bs58.encode(tx.signatures[0]!);
 const journal={schema,artifactSha256:INITIALIZER_ARTIFACT_SHA256,seed:policy.seed,signature,wireBase64:Buffer.from(wire).toString("base64"),wireSha256:sha(wire),lastValidBlockHeight:latest.lastValidBlockHeight,simulationSlot:simulation.context.slot,rentLamports:rent,feeLamports:fee};
 verifyInitializerAttempt(journal,bytes,index);
 writeFileSync(journalPath,JSON.stringify(journal,null,2)+"\n",{flag:"wx",mode:0o600,flush:true});
 try {
  const sent=await connection.sendRawTransaction(wire,{skipPreflight:false,preflightCommitment:"confirmed",maxRetries:0,minContextSlot:fresh.context.slot});
  fail(sent===signature,"RPC returned a different initializer signature");
  return {verdict:"SUBMITTED_PENDING_FINALITY",signature,broadcast:true};
 } catch {return {verdict:"PENDING_RECONCILIATION",signature,broadcast:"unknown"};}
}

if(process.argv[1]===fileURLToPath(import.meta.url)) {
 const [artifact,index,journal,...flags]=process.argv.slice(2);
 if(!artifact||!index||!journal||flags.some(f=>f!=="--execute"))throw new Error("usage: install-multiply-initializers.ts <artifact> <index 0..2> <absolute-journal> [--execute]");
 console.log(JSON.stringify(await installMultiplyInitializer(artifact,Number(index),journal,flags.includes("--execute"))));
}
