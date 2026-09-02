import { createHash } from "node:crypto";
import { chmodSync, existsSync, readFileSync, renameSync, writeFileSync } from "node:fs";
import { resolve } from "node:path";
import { fileURLToPath } from "node:url";

import { generated as squadsGenerated } from "@loyal-labs/loyal-smart-accounts-core";
import { Connection, Keypair, PublicKey, TransactionInstruction, TransactionMessage, VersionedTransaction, type AccountInfo, type SignatureStatus } from "@solana/web3.js";
import bs58 from "bs58";

import { RWA_MULTIPLY_ROUTE } from "../domain/rwa-multiply-route-spec.js";
import { signingMaterialFromEnvironment } from "../integrations/signer.js";

type Json = Record<string, unknown>;
type SettingsState = { policySeed: { toString(): string } | null };
type PolicyState = { settings: PublicKey; seed: { toString(): string }; signers: readonly { key: PublicKey }[];
  policyState: { __kind: string; fields?: readonly unknown[] } };
const ROOT = resolve(fileURLToPath(new URL("../../../..", import.meta.url)));
const COMPILED = resolve(ROOT, "docs/evidence/backyard-rwa-go/policy-compiled-v1.json");
const OUT = resolve(ROOT, "docs/evidence/backyard-rwa-go/policy-install-readback-v1.json");
const PROGRESS = `${OUT}.progress`;
const PACKET_LIMIT = 1_232;
const BATCH_SIZE = 5;
const Settings = (squadsGenerated as unknown as { Settings: { fromAccountInfo(info: AccountInfo<Buffer>): readonly [SettingsState, number] } }).Settings;
const Policy = (squadsGenerated as unknown as { Policy: { fromAccountInfo(info: AccountInfo<Buffer>): readonly [PolicyState, number] } }).Policy;
const sha256 = (value: Uint8Array | string) => createHash("sha256").update(value).digest("hex");
function invariant(value: unknown, message: string): asserts value { if (!value) throw new Error(message); }
function object(value: unknown, label: string): Json { invariant(value !== null && typeof value === "object" && !Array.isArray(value), `${label} is not an object`); return value as Json; }
function atomic(path: string, value: unknown) {
  const next = `${path}.next`; writeFileSync(next, `${JSON.stringify(value, null, 2)}\n`, { flag: "w", mode: 0o600 });
  chmodSync(next, 0o600); renameSync(next, path);
}
function createInstruction(value: unknown): TransactionInstruction {
  const row = object(value, "PolicyCreate"); const encoded = String(row.dataBase64 ?? ""); const data = Buffer.from(encoded, "base64");
  invariant(data.toString("base64") === encoded && sha256(data) === row.dataSha256 && Array.isArray(row.accounts), "PolicyCreate encoding drifted");
  return new TransactionInstruction({ programId: new PublicKey(String(row.programId)), data,
    keys: row.accounts.map((entry) => { const account = object(entry, "PolicyCreate account"); return { pubkey: new PublicKey(String(account.address)), isSigner: account.signer === true, isWritable: account.writable === true }; }) });
}
async function settingsSeed(connection: Connection): Promise<string> {
  const info = await connection.getAccountInfo(new PublicKey(RWA_MULTIPLY_ROUTE.squads.settings), "confirmed");
  invariant(info?.owner.toBase58() === RWA_MULTIPLY_ROUTE.squads.program, "Settings account is absent or has wrong owner");
  return Settings.fromAccountInfo(info)[0].policySeed?.toString() ?? "0";
}
async function confirmedStatus(connection: Connection, signature: string): Promise<SignatureStatus> {
  for (let attempt = 0; attempt < 20; attempt += 1) {
    const response = await connection.getSignatureStatuses([signature], { searchTransactionHistory: true });
    const status = response.value[0];
    if (status != null && status.confirmationStatus !== "processed") return status;
    if (attempt < 19) await new Promise((resolve) => setTimeout(resolve, 250));
  }
  throw new Error(`signature ${signature} did not reach confirmed status`);
}
async function readPolicy(connection: Connection, compiled: Json, minContextSlot?: number) {
  let response: Awaited<ReturnType<Connection["getAccountInfoAndContext"]>> | null = null;
  for (let attempt = 0; attempt < 20; attempt += 1) {
    try {
      response = await connection.getAccountInfoAndContext(new PublicKey(String(compiled.policy)), {
        commitment: "confirmed", ...(minContextSlot === undefined ? {} : { minContextSlot }),
      });
      break;
    } catch (error) {
      if (!(error instanceof Error && error.message.includes("Minimum context slot has not been reached")) || attempt === 19) throw error;
      await new Promise((resolve) => setTimeout(resolve, 250));
    }
  }
  invariant(response !== null, `policy ${String(compiled.seed)} readback did not reach its confirmed slot`);
  const info = response.value; invariant(info?.owner.toBase58() === RWA_MULTIPLY_ROUTE.squads.program, `policy ${String(compiled.seed)} readback absent`);
  const decoded = Policy.fromAccountInfo(info)[0];
  invariant(decoded.settings.toBase58() === RWA_MULTIPLY_ROUTE.squads.settings && decoded.seed.toString() === String(compiled.seed), `policy ${String(compiled.seed)} identity drifted`);
  invariant(decoded.signers.length === 1 && decoded.signers[0]?.key.toBase58() === RWA_MULTIPLY_ROUTE.squads.delegatedExecutor, `policy ${String(compiled.seed)} signer drifted`);
  invariant(decoded.policyState.__kind === "ProgramInteraction", `policy ${String(compiled.seed)} is not ProgramInteraction`);
  const body = object(decoded.policyState.fields?.[0], `policy ${String(compiled.seed)} state`);
  invariant(Array.isArray(body.instructionsConstraints) && body.instructionsConstraints.length === compiled.constraintCount,
    `policy ${String(compiled.seed)} constraint count drifted`);
  return { slot: response.context.slot, owner: info.owner.toBase58(), dataBase64: info.data.toString("base64"), dataSha256: sha256(info.data), active: true };
}

async function main() {
  invariant(process.env.CONFIRM_MAINNET === "1", "CONFIRM_MAINNET=1 is required");
  invariant(!existsSync(OUT), "final install artifact already exists");
  const rpc = process.env.SOLANA_RPC_URL?.trim(); invariant(rpc, "SOLANA_RPC_URL is required");
  const connection = new Connection(rpc, "confirmed");
  invariant(await connection.getGenesisHash() === RWA_MULTIPLY_ROUTE.genesisHash, "RPC is not mainnet-beta");
  const bytes = readFileSync(COMPILED); const artifact = object(JSON.parse(bytes.toString("utf8")), "compiled artifact");
  invariant(artifact.phase === "phase2" && artifact.verdict === "COMPILED_SIGNED_SIMULATION_REQUIRED" && Array.isArray(artifact.policies), "compiled Phase 2 artifact is not installable");
  const policies = artifact.policies.map((value) => object(value, "compiled policy"));
  invariant(policies.length === 70 && artifact.policySeedBefore === "66", "expected exact packed seeds 67-136");
  const compiledSha = sha256(bytes);
  const admin = await signingMaterialFromEnvironment("SOLANA_TESTING_PK");
  invariant(admin.signer.address === RWA_MULTIPLY_ROUTE.setupAdmin, "setup admin signer drifted");
  const adminKeypair = Keypair.fromSecretKey(admin.secretKey);
  const progress = existsSync(PROGRESS) ? object(JSON.parse(readFileSync(PROGRESS, "utf8")), "install progress") : {
    schema: "loyal-backyard-rwa-policy-install-progress/v1", compiledArtifactSha256: compiledSha, sent: {}, operations: [], batches: [],
  };
  invariant(progress.compiledArtifactSha256 === compiledSha, "progress belongs to another compiler artifact");
  const sent = object(progress.sent, "progress.sent"); const operations = progress.operations as Json[]; const batches = progress.batches as Json[];
  invariant(Array.isArray(operations) && Array.isArray(batches), "install progress arrays malformed");
  const initialSeed = await settingsSeed(connection);
  invariant(BigInt(initialSeed) >= 66n && BigInt(initialSeed) <= 136n, `live policy seed ${initialSeed} is outside this install`);

  for (let batchStart = 0; batchStart < policies.length; batchStart += BATCH_SIZE) {
    const batch = policies.slice(batchStart, batchStart + BATCH_SIZE); const batchOperations: Json[] = [];
    for (const policy of batch) {
      const seed = String(policy.seed); const address = String(policy.policy); const existing = await connection.getAccountInfo(new PublicKey(address), "confirmed");
      let signature = typeof sent[seed] === "string" ? String(sent[seed]) : ""; let confirmedSlot = 0;
      if (existing === null) {
        invariant((await settingsSeed(connection)) === String(BigInt(seed) - 1n), `seed ${seed} is not the unique forward successor`);
        const latest = await connection.getLatestBlockhashAndContext("confirmed");
        const tx = new VersionedTransaction(new TransactionMessage({ payerKey: new PublicKey(admin.signer.address), recentBlockhash: latest.value.blockhash,
          instructions: [createInstruction(policy.createInstruction)] }).compileToV0Message());
        tx.sign([adminKeypair]); const wire = tx.serialize(); invariant(wire.length <= PACKET_LIMIT, `seed ${seed} packet exceeds ${PACKET_LIMIT}`);
        signature = bs58.encode(tx.signatures[0]!); sent[seed] = signature; atomic(PROGRESS, progress);
        const returned = await connection.sendRawTransaction(wire, { skipPreflight: false, preflightCommitment: "confirmed", maxRetries: 0, minContextSlot: latest.context.slot });
        invariant(returned === signature, `seed ${seed} RPC signature mismatch`);
        const confirmation = await connection.confirmTransaction({ signature, blockhash: latest.value.blockhash, lastValidBlockHeight: latest.value.lastValidBlockHeight }, "confirmed");
        invariant(confirmation.value.err === null, `seed ${seed} failed: ${JSON.stringify(confirmation.value.err)}`);
        const status = await confirmedStatus(connection, signature);
        invariant(status.err === null, `seed ${seed} confirmed with an error`);
        confirmedSlot = status.slot;
      } else {
        invariant(signature.length > 0, `seed ${seed} exists without a recorded signed wire`);
        const status = await confirmedStatus(connection, signature);
        invariant(status.err === null, `seed ${seed} recorded signature failed`);
        confirmedSlot = status.slot;
      }
      const readback = await readPolicy(connection, policy, confirmedSlot);
      const operation = { action: "create", policyName: policy.name, seed, policyAddress: address, transactionSignature: signature,
        confirmedSlot, readbackSlot: readback.slot, owner: readback.owner, active: readback.active,
        dataBase64: readback.dataBase64, dataSha256: readback.dataSha256 };
      const prior = operations.findIndex((row) => row.seed === seed); if (prior >= 0) operations[prior] = operation; else operations.push(operation);
      batchOperations.push(operation); atomic(PROGRESS, progress);
    }
    const readback = await connection.getMultipleAccountsInfoAndContext(batch.map((policy) => new PublicKey(String(policy.policy))), { commitment: "confirmed" });
    invariant(readback.value.every((info) => info?.owner.toBase58() === RWA_MULTIPLY_ROUTE.squads.program), `batch ${batchStart / BATCH_SIZE + 1} readback failed`);
    const batchRow = { batch: batchStart / BATCH_SIZE + 1, seeds: batch.map((policy) => String(policy.seed)), confirmedReadbackSlot: readback.context.slot,
      dataSha256: readback.value.map((info) => sha256(info!.data)) };
    const priorBatch = batches.findIndex((row) => row.batch === batchRow.batch); if (priorBatch >= 0) batches[priorBatch] = batchRow; else batches.push(batchRow);
    atomic(PROGRESS, progress);
    console.log(JSON.stringify({ batch: batchRow.batch, seeds: batchRow.seeds, confirmedReadbackSlot: batchRow.confirmedReadbackSlot }));
  }
  invariant(await settingsSeed(connection) === "136" && operations.length === 70 && batches.length === 14, "forward install did not reconcile exactly");
  const result = { schema: "loyal-backyard-rwa-policy-install-readback/v1", verdict: "PASS", broadcast: true, cluster: "mainnet-beta",
    commitment: "confirmed", genesisHash: RWA_MULTIPLY_ROUTE.genesisHash, compiledArtifactSha256: compiledSha,
    policySeedBefore: "66", policySeedAfter: "136", retiredOrClosedSeeds: [], batchSize: BATCH_SIZE, batches, operations };
  atomic(OUT, result); renameSync(PROGRESS, `${OUT}.sent-wire`);
  console.log(JSON.stringify({ verdict: "PASS", output: OUT, policies: operations.length, batches: batches.length, policySeedAfter: "136" }));
}
main().catch((error) => { console.error(error instanceof Error ? error.message.replace(process.env.SOLANA_RPC_URL ?? "", "<rpc>") : String(error)); process.exitCode = 1; });
