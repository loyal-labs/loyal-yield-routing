/**
 * Helius signed-unsent proof for the four installed Phase 1 bridge policies.
 * Two exact existing-policy prelude payloads stage and restore one raw USDC in
 * the ephemeral confirmed bank; no admin funding or broadcast occurs.
 */
import { createHash } from "node:crypto";
import { existsSync, writeFileSync } from "node:fs";
import { resolve } from "node:path";
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";
import { AccountRole, address, createNoopSigner, type Instruction } from "@solana/kit";
import bs58 from "bs58";
import { Connection, Keypair, PublicKey, TransactionMessage, VersionedTransaction, type AccountInfo, type TransactionInstruction } from "@solana/web3.js";
import { RWA_MULTIPLY_ROUTE } from "../domain/rwa-multiply-route-spec.js";
import { toWeb3Instruction } from "../integrations/solana-compat.js";
import { signingMaterialFromEnvironment } from "../integrations/signer.js";
import { buildRwaMultiplyArmReportInstruction, buildRwaMultiplyManagerInstructions, buildRwaMultiplyWithdrawalStagingInstruction, deriveRwaMultiplyVoltrAccounts, type RwaReportV1 } from "../integrations/rwa-multiply-voltr.js";

type Json = Record<string, unknown>;
type Name = "allocate" | "nav-refresh" | "stage-withdrawal" | "restore";
type Wire = Readonly<{ role: "prelude-stage" | "prelude-restore" | Name; wire: Uint8Array; signature: string }>;
const ROOT = resolve(fileURLToPath(new URL("../../../..", import.meta.url)));
const OUT = resolve(ROOT, "docs/evidence/backyard-rwa-go/policy-helius-bridge-lifecycle-v2.json");
const AMOUNT = 1n;
const PACKET_LIMIT = 1_232;
const POLICY = {
  allocate: { seed: "62", account: "HoDV7mtsb2u1VARZLYuGByW7cCsGWL9NFxHZs7WHjdzz", dataSha256: "bda72932f474064fa3cd60ce91633acba35b2730e86b82f4352aa96a6738e2f4" },
  "nav-refresh": { seed: "63", account: "41nzu42c3KPgJfWhnV5jbfxjHbvVU6HXaiJmzzYNqvBP", dataSha256: "bf34a3e9c9c635c79a0d30e096b639a86d52e300ad113c81161e3486832d97ca" },
  "stage-withdrawal": { seed: "64", account: "ALz5Wkt82GhGFH1LfzbnAovkZ6t85ErovbxHUH3yY1wY", dataSha256: "ef8c231497fb2620b5930cfe5d329c871f103db6512781eb5487534db8b1291b" },
  restore: { seed: "65", account: "DjYYkQWb4zYbySfEndjVdg2NwZ8i77Fb9P1UFVbebc5t", dataSha256: "84e8f6f881758cff1714ef743603c016024104f9834392c6fba693c3651b719c" },
} as const;
const EVIDENCE_ORDER: readonly Name[] = ["allocate", "stage-withdrawal", "restore", "nav-refresh"];
function invariant(value: unknown, message: string): asserts value { if (!value) throw new Error(message); }
function sha(value: Uint8Array | string): string { return createHash("sha256").update(value).digest("hex"); }
function object(value: unknown, label: string): Json { invariant(value !== null && typeof value === "object" && !Array.isArray(value), `${label} is not an object`); return value as Json; }
function tokenRaw(data: Buffer): bigint { invariant(data.length >= 72, "token account is too short"); return data.readBigUInt64LE(64); }
function snapshotHash(values: readonly (AccountInfo<Buffer> | null)[]): string { return sha(JSON.stringify(values.map((value) => value === null ? null : ({ owner: value.owner.toBase58(), executable: value.executable, lamports: value.lamports, dataBase64: value.data.toString("base64") })))); }
function innerWire(instruction: Instruction) { return { programId: instruction.programAddress, accounts: (instruction.accounts ?? []).map((account) => ({ address: account.address, signer: account.role === AccountRole.READONLY_SIGNER || account.role === AccountRole.WRITABLE_SIGNER, writable: account.role === AccountRole.WRITABLE || account.role === AccountRole.WRITABLE_SIGNER })), dataBase64: Buffer.from(instruction.data ?? []).toString("base64") }; }
function wrapper(policy: string, inner: readonly Instruction[], constraintIndices: readonly number[]): Instruction {
  const result = spawnSync("cargo", ["run", "--quiet", "-p", "loyal-actions", "--bin", "compile-voltr-custom-execution"], { cwd: ROOT, input: JSON.stringify({ policy, delegatedSigner: RWA_MULTIPLY_ROUTE.squads.delegatedExecutor, accountIndex: 0, constraintIndices, inner: inner.map(innerWire) }), encoding: "utf8", maxBuffer: 16 * 1024 * 1024 });
  invariant(result.status === 0, `bridge wrapper compiler failed: ${(result.stderr || result.stdout).trim()}`);
  const output = JSON.parse(result.stdout) as { schema?: unknown; instruction?: ReturnType<typeof innerWire> };
  invariant(output.schema === "loyal-voltr-custom-execution/v2" && output.instruction, "bridge wrapper output drifted");
  return { programAddress: address(output.instruction.programId), accounts: output.instruction.accounts.map((account) => ({ address: address(account.address), role: account.signer ? account.writable ? AccountRole.WRITABLE_SIGNER : AccountRole.READONLY_SIGNER : account.writable ? AccountRole.WRITABLE : AccountRole.READONLY })), data: Buffer.from(output.instruction.dataBase64, "base64") };
}
function sign(role: Wire["role"], payer: Keypair, instruction: TransactionInstruction, blockhash: string): Wire {
  const tx = new VersionedTransaction(new TransactionMessage({ payerKey: payer.publicKey, recentBlockhash: blockhash, instructions: [instruction] }).compileToV0Message()); tx.sign([payer]); const wire = tx.serialize(); invariant(wire.length <= PACKET_LIMIT, `${role} packet is ${wire.length} bytes; limit is ${PACKET_LIMIT}`); return { role, wire, signature: bs58.encode(tx.signatures[0]!) };
}
function resultSummary(value: unknown): Json {
  const root = object(value, "Helius response"); if (root.error) return { kind: "rpc-error", error: root.error };
  const result = object(root.result, "Helius result"); const response = object(result.value, "Helius result value");
  const transactionResults = Array.isArray(response.transactionResults) ? response.transactionResults.map((entry) => { const row = object(entry, "Helius transaction result"); return { err: row.err ?? null, capturedPre: Array.isArray(row.preExecutionAccounts), capturedPost: Array.isArray(row.postExecutionAccounts), preExecutionAccountsSha256: sha(JSON.stringify(row.preExecutionAccounts ?? null)), postExecutionAccountsSha256: sha(JSON.stringify(row.postExecutionAccounts ?? null)), logsSha256: sha(JSON.stringify(row.logs ?? [])) }; }) : [];
  return { kind: "result", contextSlot: object(result.context ?? {}, "Helius context").slot ?? null, summary: response.summary ?? null, transactionResults };
}
async function main() {
  invariant(!process.argv.includes("--execute"), "this generator has no broadcast mode"); invariant(!existsSync(OUT), `${OUT} already exists; evidence artifacts are immutable`);
  const rpcUrl = process.env.SOLANA_RPC_URL?.trim(); invariant(rpcUrl && new URL(rpcUrl).hostname.includes("helius"), "Helius SOLANA_RPC_URL is required");
  const delegated = Keypair.fromSecretKey((await signingMaterialFromEnvironment("POLICY_KEYPAIR")).secretKey); invariant(delegated.publicKey.toBase58() === RWA_MULTIPLY_ROUTE.squads.delegatedExecutor, "POLICY_KEYPAIR is not the delegated executor");
  const connection = new Connection(rpcUrl, "confirmed"); invariant(await connection.getGenesisHash() === RWA_MULTIPLY_ROUTE.genesisHash, "RPC is not Solana mainnet-beta"); const latest = await connection.getLatestBlockhashAndContext("confirmed"); const accounts = await deriveRwaMultiplyVoltrAccounts();
  const protectedAddresses = [RWA_MULTIPLY_ROUTE.customAdaptor.strategyConfig, accounts.reportTicket, accounts.strategyInitReceipt, accounts.idleAta, accounts.strategyAssetAta, RWA_MULTIPLY_ROUTE.squads.assetAta, ...Object.values(POLICY).map(({ account }) => account)];
  const before = await connection.getMultipleAccountsInfoAndContext(protectedAddresses.map((value) => new PublicKey(value)), { commitment: "confirmed", minContextSlot: latest.context.slot }); invariant(before.value.every((value) => value !== null), "protected account missing");
  const [config, ticket, receipt, _idle, _strategy, squads, ...policies] = before.value as (AccountInfo<Buffer> | null)[]; invariant(config && ticket && receipt && squads && policies.length === EVIDENCE_ORDER.length && receipt.data.length >= 112 && ticket.data.length === 96, "protected account layout drifted"); invariant(tokenRaw(squads.data) >= AMOUNT, "Squads idle USDC lacks the 1-raw prelude amount");
  for (const [index, expected] of Object.values(POLICY).entries()) invariant(sha(policies[index]?.data ?? Buffer.alloc(0)) === expected.dataSha256, `Phase 1 policy ${expected.seed} bytes drifted`);
  const baseSequence = BigInt(latest.context.slot) - 3n; invariant(baseSequence > ticket.data.readBigUInt64LE(48), "confirmed bank is too close to last consumed report sequence"); let reportIndex = 0;
  const manager = createNoopSigner(RWA_MULTIPLY_ROUTE.squads.vault);
  const managerPayload = async (name: "allocate" | "nav-refresh" | "restore", amount: bigint): Promise<Instruction> => {
    const sequence = baseSequence + BigInt(reportIndex++); const report: RwaReportV1 = { sequence, observedSlot: sequence, navAfterRaw: receipt.data.readBigUInt64LE(104), snapshotDigest: createHash("sha256").update(config.data).update(ticket.data).update(receipt.data).update(Buffer.concat(policies.map((policy) => policy!.data))).update(`${name}:${sequence}`).digest() }; const operation = name === "restore" ? "withdraw" : "deposit"; const capital = await buildRwaMultiplyManagerInstructions(manager, amount, report); return wrapper(POLICY[name].account, [await buildRwaMultiplyArmReportInstruction(manager, operation, amount, report), operation === "withdraw" ? capital.withdraw : capital.deposit], [0, 1]);
  };
  const stage = async () => wrapper(POLICY["stage-withdrawal"].account, [await buildRwaMultiplyWithdrawalStagingInstruction(manager, AMOUNT)], [0]);
  const payloads: readonly Readonly<{ role: Wire["role"]; instruction: Instruction }>[] = [
    { role: "prelude-stage", instruction: await stage() },
    { role: "prelude-restore", instruction: await managerPayload("restore", AMOUNT) },
    { role: "allocate", instruction: await managerPayload("allocate", AMOUNT) },
    { role: "stage-withdrawal", instruction: await stage() },
    { role: "restore", instruction: await managerPayload("restore", AMOUNT) },
    { role: "nav-refresh", instruction: await managerPayload("nav-refresh", 0n) },
  ];
  // Helius rejects duplicate signed transactions. Reusing the exact stage and
  // restore payloads is intentional, so give every bundle member a distinct,
  // still-live confirmed blockhash rather than adding an unrelated instruction.
  const blockhashes = [latest.value.blockhash];
  for (let attempt = 0; blockhashes.length < payloads.length && attempt < 40; attempt += 1) {
    await new Promise((resolve) => setTimeout(resolve, 350));
    const next = await connection.getLatestBlockhashAndContext("confirmed");
    if (!blockhashes.includes(next.value.blockhash)) blockhashes.push(next.value.blockhash);
  }
  invariant(blockhashes.length === payloads.length, "could not obtain distinct live blockhashes for repeated exact policy payloads");
  const wires = payloads.map(({ role, instruction }, index) => sign(role, delegated, toWeb3Instruction(instruction), blockhashes[index]!)); invariant(wires.length === 6, "expected two prep and four evidence wires");
  const statusBefore = await connection.getSignatureStatuses(wires.map((entry) => entry.signature), { searchTransactionHistory: true }); invariant(statusBefore.value.every((status) => status === null), "a signed-unsent bridge wire already landed before simulation");
  const configs = wires.map(() => ({ addresses: protectedAddresses, encoding: "base64" })); const response = await fetch(rpcUrl, { method: "POST", headers: { "content-type": "application/json" }, body: JSON.stringify({ jsonrpc: "2.0", id: "rwa-phase2-bridge-lifecycle", method: "simulateBundle", params: [{ encodedTransactions: wires.map((entry) => Buffer.from(entry.wire).toString("base64")) }, { preExecutionAccountsConfigs: configs, postExecutionAccountsConfigs: configs, skipSigVerify: false, simulationBank: { commitment: { commitment: "confirmed" } }, transactionEncoding: "base64", replaceRecentBlockhash: false }] }) });
  const provider = resultSummary(await response.json() as unknown); const statusAfter = await connection.getSignatureStatuses(wires.map((entry) => entry.signature), { searchTransactionHistory: true }); invariant(statusAfter.value.every((status) => status === null), "Helius simulation unexpectedly landed a signed bridge wire"); const after = await connection.getMultipleAccountsInfoAndContext(protectedAddresses.map((value) => new PublicKey(value)), { commitment: "confirmed", minContextSlot: latest.context.slot });
  const rows = Array.isArray(provider.transactionResults) ? provider.transactionResults as Json[] : []; const pass = response.ok && provider.kind === "result" && provider.summary === "succeeded" && rows.length === wires.length && rows.every((row) => row.err === null && row.capturedPre === true && row.capturedPost === true) && rows.slice(1).every((row, index) => rows[index]?.postExecutionAccountsSha256 === row.preExecutionAccountsSha256); invariant(pass, `Helius rejected exact all-policy bridge lifecycle: ${JSON.stringify(provider)}`); invariant(snapshotHash(before.value) === snapshotHash(after.value), "confirmed protected state changed across signed-unsent bridge simulation");
  const evidenceWires = wires.slice(2); const bundles = EVIDENCE_ORDER.map((name, index) => { const action = evidenceWires[index]!; return { name, policyScope: "phase1-external-existing", externalPolicy: POLICY[name], compiledArtifactSha256: null, probeAmountRaw: String(name === "nav-refresh" ? 0n : AMOUNT), broadcast: false, signature: action.signature, packetBytes: action.wire.length, transactionBase64: Buffer.from(action.wire).toString("base64"), transactionSha256: sha(action.wire), simulation: { method: "simulateBundle", skipSigVerify: false, replaceRecentBlockhash: false, err: null, contextSlot: provider.contextSlot }, signatureAbsentOnChain: true, chainPreStateSha256: snapshotHash(before.value), chainPostStateSha256: snapshotHash(after.value), confirmedReadbackSlot: after.context.slot }; });
  writeFileSync(OUT, `${JSON.stringify({ schema: "loyal-backyard-rwa-phase2-bridge-lifecycle-positive/v2", verdict: "PASS", broadcast: false, signedUnsent: true, cluster: "mainnet-beta", commitment: "confirmed", policyScope: "phase1-external-existing", bundles, preparation: { policyScope: "phase1-external-existing", roles: ["prelude-stage", "prelude-restore"], exactPolicies: [POLICY["stage-withdrawal"], POLICY.restore], amountRaw: String(AMOUNT), purpose: "simulate-only state setup from existing Squads idle USDC; not an additional permission" }, simulationRequest: { method: "simulateBundle", skipSigVerify: false, simulationBankCommitment: "confirmed", replaceRecentBlockhash: false }, transactions: wires.map((entry) => ({ role: entry.role, signature: entry.signature, packetBytes: entry.wire.length, transactionBase64: Buffer.from(entry.wire).toString("base64"), transactionSha256: sha(entry.wire) })), signatureAbsentOnChain: true, chainPreStateSha256: snapshotHash(before.value), chainPostStateSha256: snapshotHash(after.value), confirmedReadbackSlot: after.context.slot, provider, conclusion: "Exact existing policies staged and restored 1 raw USDC only in Helius's confirmed simulation bank, then exact policies 62 allocate, 64 stage, 65 restore, and 63 nav-refresh passed. Every signed wire remained absent from mainnet and confirmed protected-state readback was unchanged." }, null, 2)}\n`, { flag: "wx", mode: 0o600 }); console.log(JSON.stringify({ verdict: "PASS", output: OUT, packets: wires.map(({ role, wire }) => ({ role, packetBytes: wire.length })) }));
}
main().catch((error) => { const message = error instanceof Error ? error.message : String(error); console.error(message.replaceAll(process.env.SOLANA_RPC_URL ?? "", "<rpc>")); process.exitCode = 1; });
