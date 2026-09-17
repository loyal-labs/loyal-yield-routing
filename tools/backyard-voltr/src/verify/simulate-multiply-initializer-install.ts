/** Read-only first-policy simulation. Later seeds need the preceding installed state. */
import { readInitializerArtifact, verifyInitializerPolicy } from "../activation/multiply-initializer-artifact.js";
import { createHash } from "node:crypto";
import { readFileSync, writeFileSync } from "node:fs";
import { Connection, PublicKey, TransactionInstruction, TransactionMessage, VersionedTransaction } from "@solana/web3.js";
import { generated } from "@loyal-labs/loyal-smart-accounts-core";

const [artifactPath, outputPath] = process.argv.slice(2);
if (!artifactPath || !outputPath || process.argv.length !== 4) throw new Error("usage: simulate-multiply-initializer-install.ts <compiler-artifact> <new-output>");
const artifactBytes = readFileSync(artifactPath);
const verifiedArtifact = readInitializerArtifact(artifactBytes);
const artifact = JSON.parse(artifactBytes.toString("utf8"));
const settings = "5YQ78RwqukvCcykpmjmgRFmbEUeAgLpuVDxx1xNZnHD6";
const authority = "BAqgbERmvUViqDSx961xpRBHGt68SpACiWL4t9696qZZ";
const program = "SMRTzfY6DfH5ik3TKiyLFfXexV8uSG3d2UksSCYdunG";
if (artifact.schema !== "loyal-backyard-multiply-initializer-install/v1" || artifact.settings !== settings || artifact.authority !== authority || artifact.policies?.length !== 3) throw new Error("Wrong compiler artifact");
const connection = new Connection(process.env.SOLANA_RPC_URL ?? "https://api.mainnet-beta.solana.com", { commitment: "finalized", disableRetryOnRateLimit: true, fetch: (url, options) => fetch(url, { ...options, signal: AbortSignal.timeout(15000) }) });
if (await connection.getGenesisHash() !== "5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d") throw new Error("Wrong cluster");
const before = await connection.getAccountInfoAndContext(new PublicKey(settings));
if (!before.value || before.value.owner.toBase58() !== program || before.value.executable) throw new Error("Settings identity mismatch");
type SettingsState = { policySeed: {toString():string}|null; threshold:number; timeLock:number; signers:readonly {key:PublicKey;permissions:{mask:number}}[] };
const Settings = (generated as unknown as {Settings:{deserialize(data:Buffer):readonly [SettingsState,number]}}).Settings;
const [decoded] = Settings.deserialize(before.value.data);
if (decoded.policySeed?.toString() !== artifact.policySeedBefore || decoded.threshold !== 1 || decoded.timeLock !== 0 || decoded.signers.length !== 1 || decoded.signers[0]?.key.toBase58() !== authority || decoded.signers[0]?.permissions.mask !== 7) throw new Error("Settings seed or authority changed");
const policy = artifact.policies[0];
if (policy.seed !== (BigInt(artifact.policySeedBefore) + 1n).toString() || policy.instruction.programId !== program) throw new Error("First policy identity mismatch");
const ix = new TransactionInstruction({ programId: new PublicKey(program), keys: policy.instruction.accounts.map((a: {address:string;signer:boolean;writable:boolean}) => ({ pubkey: new PublicKey(a.address), isSigner:a.signer, isWritable:a.writable })), data: Buffer.from(policy.instruction.dataBase64,"base64") });
const latest = await connection.getLatestBlockhash("finalized");
const tx = new VersionedTransaction(new TransactionMessage({ payerKey:new PublicKey(authority), recentBlockhash:latest.blockhash, instructions:[ix] }).compileToLegacyMessage());
if (tx.serialize().length > 1232) throw new Error("Oversized packet");
const simulation = await connection.simulateTransaction(tx, { sigVerify:false, commitment:"finalized", minContextSlot:before.context.slot, accounts:{encoding:"base64",addresses:[settings,policy.account]} });
let policyDataSha256:string|null=null;
let blockTime:number|null=null;
if(simulation.value.err===null) {
 const post=simulation.value.accounts?.[1];
 blockTime=await connection.getBlockTime(simulation.context.slot);
 if(!post||blockTime===null||typeof post.data[0]!=="string"||post.data[1]!=="base64")throw new Error("Missing simulation policy or block time");
 policyDataSha256=verifyInitializerPolicy(verifiedArtifact.policies[0]!,{...post,data:Buffer.from(post.data[0],"base64")},blockTime);
 const postSettings=simulation.value.accounts?.[0];
 if(!postSettings||typeof postSettings.data[0]!=="string"||postSettings.owner!==program||postSettings.executable||postSettings.data[1]!=="base64")throw new Error("Missing simulated Settings");
 const [afterSettings]=Settings.deserialize(Buffer.from(postSettings.data[0],"base64"));
 if(afterSettings.policySeed?.toString()!==policy.seed||afterSettings.threshold!==decoded.threshold||afterSettings.timeLock!==decoded.timeLock||afterSettings.signers.length!==1||afterSettings.signers[0]?.key.toBase58()!==authority||afterSettings.signers[0]?.permissions.mask!==7)throw new Error("Simulated Settings authority or seed mismatch");
}
const result = {policyDataSha256,blockTime, schema:"initializer-first-policy-unsigned-simulation/v1", broadcast:false, artifactSha256:createHash("sha256").update(artifactBytes).digest("hex"), settingsSlot:before.context.slot, simulationSlot:simulation.context.slot, seed:policy.seed, policy:policy.account, packetBytes:tx.serialize().length, err:simulation.value.err, unitsConsumed:simulation.value.unitsConsumed, logs:simulation.value.logs, accounts:simulation.value.accounts };
writeFileSync(outputPath, JSON.stringify(result,null,2)+"\n", {flag:"wx"});
console.log(JSON.stringify({...result,logs:undefined,accounts:undefined}));
if (simulation.value.err !== null) process.exitCode = 1;
