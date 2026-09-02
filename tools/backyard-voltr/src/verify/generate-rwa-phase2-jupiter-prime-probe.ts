/**
 * One meaningful pre-install Jupiter proof: create the packed prefix policy,
 * then execute its exact USDC->PRIME v0+ALT Squads payload in Helius's
 * stateful signed-unsent simulator.  This never sends a transaction.
 */
import { createHash } from "node:crypto";
import { existsSync, readFileSync, writeFileSync } from "node:fs";
import { resolve } from "node:path";
import { fileURLToPath } from "node:url";

import bs58 from "bs58";
import { Connection, Keypair, PublicKey, TransactionInstruction, TransactionMessage, VersionedTransaction, type AccountInfo } from "@solana/web3.js";

import { RWA_MULTIPLY_ROUTE } from "../domain/rwa-multiply-route-spec.js";
import { signingMaterialFromEnvironment } from "../integrations/signer.js";
import { buildExactJupiterSquadsExecution, signExactJupiterSquadsExecution } from "./rwa-phase2-jupiter-execution.js";

type Json = Record<string, unknown>;
const ROOT = resolve(fileURLToPath(new URL("../../../..", import.meta.url)));
const COMPILED_PATH = resolve(ROOT, "docs/evidence/backyard-rwa-go/policy-compiled-v1.json");
const HEADERS_PATH = resolve(ROOT, "docs/evidence/backyard-rwa-go/policy-jupiter-headers-v1.json");
const OUTPUT_PATH = resolve(ROOT, "docs/evidence/backyard-rwa-go/policy-helius-jupiter-prime-probe-v1.json");
const PACKET_LIMIT = 1_232;

function invariant(value: unknown, message: string): asserts value { if (!value) throw new Error(message); }
function object(value: unknown, label: string): Json { invariant(value !== null && typeof value === "object" && !Array.isArray(value), `${label} is not an object`); return value as Json; }
function text(value: unknown, label: string): string { invariant(typeof value === "string" && value.length > 0, `${label} is missing`); return value; }
function sha256(value: Uint8Array | string): string { return createHash("sha256").update(value).digest("hex"); }
function stateSha256(infos: readonly (AccountInfo<Buffer> | null)[]): string { return sha256(JSON.stringify(infos.map((info) => info === null ? null : ({ owner: info.owner.toBase58(), executable: info.executable, lamports: info.lamports, data: info.data.toString("base64") })))); }

function createInstruction(value: unknown): TransactionInstruction {
  const raw = object(value, "compiled PolicyCreate instruction");
  const dataBase64 = text(raw.dataBase64, "compiled PolicyCreate data");
  const data = Buffer.from(dataBase64, "base64");
  invariant(data.toString("base64") === dataBase64 && sha256(data) === text(raw.dataSha256, "compiled PolicyCreate data hash"), "compiled PolicyCreate data drifted");
  const accounts = raw.accounts;
  invariant(Array.isArray(accounts) && accounts.length > 0, "compiled PolicyCreate accounts are absent");
  return new TransactionInstruction({ programId: new PublicKey(text(raw.programId, "compiled PolicyCreate program")), data,
    keys: accounts.map((entry, index) => { const account = object(entry, `compiled PolicyCreate account ${index}`); invariant(typeof account.signer === "boolean" && typeof account.writable === "boolean", `compiled PolicyCreate account ${index} roles are malformed`); return { pubkey: new PublicKey(text(account.address, `compiled PolicyCreate account ${index} address`)), isSigner: account.signer, isWritable: account.writable }; }),
  });
}

function responseSummary(value: unknown): Json {
  const root = object(value, "Helius response");
  if (root.error) return { kind: "rpc-error", error: root.error };
  const result = object(root.result, "Helius result");
  const body = object(result.value, "Helius result value");
  const rows = Array.isArray(body.transactionResults) ? body.transactionResults.map((entry) => {
    const row = object(entry, "Helius transaction result");
    return { err: row.err ?? null, logsSha256: sha256(JSON.stringify(row.logs ?? [])), capturedPre: Array.isArray(row.preExecutionAccounts), capturedPost: Array.isArray(row.postExecutionAccounts) };
  }) : [];
  return { kind: "result", contextSlot: object(result.context ?? {}, "Helius context").slot ?? null, summary: body.summary ?? null, transactionResults: rows };
}

async function main() {
  invariant(!existsSync(OUTPUT_PATH), `Jupiter PRIME probe already exists at ${OUTPUT_PATH}; refusing to replace evidence`);
  const rpcUrl = process.env.SOLANA_RPC_URL?.trim();
  invariant(rpcUrl && new URL(rpcUrl).hostname.includes("helius"), "Helius SOLANA_RPC_URL is required");
  const artifactBytes = readFileSync(COMPILED_PATH);
  const artifact = object(JSON.parse(artifactBytes.toString("utf8")), "compiled Phase-2 artifact");
  invariant(artifact.phase === "phase2" && Array.isArray(artifact.policies) && artifact.policies.length === 70, "frozen 70-policy artifact is absent");
  const headers = object(JSON.parse(readFileSync(HEADERS_PATH, "utf8")), "resolved Jupiter headers");
  invariant(headers.verdict === "PASS_HEADERS_RESOLVED" && Array.isArray(headers.rows), "exact Jupiter headers are absent");
  const policy = artifact.policies.find((entry) => {
    const row = object(entry, "compiled policy");
    return row.logicalName === "swap/packed/15" && Array.isArray(row.swapEdges)
      && row.swapEdges.some((edge) => { const value = object(edge, "packed swap edge"); return value.from === "USDC" && value.to === "PRIME"; });
  });
  invariant(policy, "USDC->PRIME prefix policy is absent");
  const header = headers.rows.find((entry) => object(entry, "Jupiter header").key === "USDC->PRIME");
  invariant(header, "USDC->PRIME resolved header is absent");
  const adminMaterial = await signingMaterialFromEnvironment("SOLANA_TESTING_PK");
  const delegatedMaterial = await signingMaterialFromEnvironment("POLICY_KEYPAIR");
  const admin = Keypair.fromSecretKey(adminMaterial.secretKey);
  const delegated = Keypair.fromSecretKey(delegatedMaterial.secretKey);
  invariant(admin.publicKey.toBase58() === RWA_MULTIPLY_ROUTE.setupAdmin, "SOLANA_TESTING_PK is not the Settings authority");
  invariant(delegated.publicKey.toBase58() === RWA_MULTIPLY_ROUTE.squads.delegatedExecutor, "POLICY_KEYPAIR is not the delegated executor");
  const connection = new Connection(rpcUrl, "confirmed");
  invariant(await connection.getGenesisHash() === RWA_MULTIPLY_ROUTE.genesisHash, "RPC is not mainnet-beta");
  const latest = await connection.getLatestBlockhashAndContext("confirmed");
  const execution = await buildExactJupiterSquadsExecution({ connection, compiledPolicy: policy, headerRow: header, delegatedSigner: delegated.publicKey });
  const create = createInstruction(object(policy, "compiled prefix policy").createInstruction);
  const createTransaction = new VersionedTransaction(new TransactionMessage({ payerKey: admin.publicKey, recentBlockhash: latest.value.blockhash, instructions: [create] }).compileToV0Message());
  createTransaction.sign([admin]);
  const createWire = createTransaction.serialize();
  invariant(createWire.length <= PACKET_LIMIT, `PolicyCreate packet is ${createWire.length} bytes; limit is ${PACKET_LIMIT}`);
  const execute = signExactJupiterSquadsExecution({ execution, payer: delegated, recentBlockhash: latest.value.blockhash });
  const wires = [
    { role: "create-packed-usdc-prime-policy", wire: createWire, signature: bs58.encode(createTransaction.signatures[0]!) },
    { role: "execute-usdc-prime-v0-alt", wire: execute.wire, signature: bs58.encode(execute.transaction.signatures[0]!) },
  ] as const;
  const inspected = [RWA_MULTIPLY_ROUTE.squads.settings, RWA_MULTIPLY_ROUTE.squads.vault, execution.policy,
    execution.innerInstruction.keys[3]?.pubkey.toBase58(), execution.innerInstruction.keys[6]?.pubkey.toBase58()];
  invariant(inspected.every((entry): entry is string => typeof entry === "string"), "USDC->PRIME header custody indexes drifted");
  const before = await connection.getMultipleAccountsInfoAndContext(inspected.map((entry) => new PublicKey(entry)), { commitment: "confirmed", minContextSlot: latest.context.slot });
  const beforeStatuses = await connection.getSignatureStatuses(wires.map((entry) => entry.signature), { searchTransactionHistory: true });
  invariant(beforeStatuses.value.every((entry) => entry === null), "signed-unsent wire already landed before simulation");
  const http = await fetch(rpcUrl, { method: "POST", headers: { "content-type": "application/json" }, body: JSON.stringify({
    jsonrpc: "2.0", id: "rwa-phase2-jupiter-prime-probe", method: "simulateBundle",
    params: [{ encodedTransactions: wires.map((entry) => Buffer.from(entry.wire).toString("base64")) }, {
      preExecutionAccountsConfigs: wires.map(() => ({ addresses: inspected, encoding: "base64" })),
      postExecutionAccountsConfigs: wires.map(() => ({ addresses: inspected, encoding: "base64" })),
      skipSigVerify: false, simulationBank: { commitment: { commitment: "confirmed" } }, transactionEncoding: "base64", replaceRecentBlockhash: false,
    }],
  }) });
  const result = await http.json() as unknown;
  const afterStatuses = await connection.getSignatureStatuses(wires.map((entry) => entry.signature), { searchTransactionHistory: true });
  invariant(afterStatuses.value.every((entry) => entry === null), "Helius signed-unsent simulation unexpectedly landed a wire");
  const after = await connection.getMultipleAccountsInfoAndContext(inspected.map((entry) => new PublicKey(entry)), { commitment: "confirmed", minContextSlot: before.context.slot });
  invariant(stateSha256(before.value) === stateSha256(after.value), "confirmed state changed across signed-unsent Jupiter proof");
  const provider = responseSummary(result);
  const rows = Array.isArray(provider.transactionResults) ? provider.transactionResults as Json[] : [];
  const pass = http.ok && provider.kind === "result" && provider.summary === "succeeded" && rows.length === 2
    && rows.every((entry) => entry.err === null && entry.capturedPre === true && entry.capturedPost === true);
  const output = { schema: "loyal-backyard-rwa-phase2-jupiter-prime-probe/v1", verdict: pass ? "PASS" : "REJECTED", broadcast: false, signedUnsent: true, cluster: "mainnet-beta", commitment: "confirmed", compiledArtifactSha256: sha256(artifactBytes), representative: { edge: execution.edgeKey, policy: execution.policy, seed: execution.policySeed, constraintIndex: execution.constraintIndex, lookupTables: execution.lookupTableAddresses, compiledPayloadSha256: execution.compiledPayloadSha256 }, simulationRequest: { method: "simulateBundle", skipSigVerify: false, simulationBankCommitment: "confirmed", replaceRecentBlockhash: false }, transactions: wires.map((entry) => ({ role: entry.role, signature: entry.signature, packetBytes: entry.wire.length, transactionBase64: Buffer.from(entry.wire).toString("base64"), transactionSha256: sha256(entry.wire) })), signatureAbsentOnChain: beforeStatuses.value.every((entry) => entry === null) && afterStatuses.value.every((entry) => entry === null), chainPreStateSha256: stateSha256(before.value), chainPostStateSha256: stateSha256(after.value), confirmedReadbackSlot: after.context.slot, provider: { httpStatus: http.status, response: provider }, conclusion: pass ? "Exact v0+ALT USDC->PRIME Squads execution passed a sequential signed-unsent confirmed Helius simulation." : "The exact v0+ALT construction was signed and simulated but did not pass; inspect the provider result before any further edge work." };
  writeFileSync(OUTPUT_PATH, `${JSON.stringify(output, null, 2)}\n`, { flag: "wx", mode: 0o600 });
  console.log(JSON.stringify({ verdict: output.verdict, output: OUTPUT_PATH, packets: output.transactions.map(({ role, packetBytes }) => ({ role, packetBytes })) }));
}

main().catch((error) => { const rpcUrl = process.env.SOLANA_RPC_URL?.trim(); const message = error instanceof Error ? error.message : String(error); console.error(rpcUrl ? message.replaceAll(rpcUrl, "<rpc>") : message); process.exitCode = 1; });
