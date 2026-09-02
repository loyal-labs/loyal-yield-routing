/**
 * Creates only absent exact Phase-2 K-Lend obligation PDAs through the current
 * Squads Settings authority.  Every write is independently signed, simulated,
 * persisted before send, confirmed, and decoded back.  It never installs or
 * changes a policy.
 */
import { createHash } from "node:crypto";
import { chmodSync, existsSync, readFileSync, renameSync, writeFileSync } from "node:fs";
import { resolve } from "node:path";
import { fileURLToPath } from "node:url";

import { initObligation, Obligation, userMetadataPda } from "@kamino-finance/klend-sdk";
import { generated as squadsGenerated } from "@loyal-labs/loyal-smart-accounts-core";
import { executeTransactionSyncV2 } from "@loyal-labs/loyal-smart-accounts-core/internal";
import { AccountRole, address, createNoopSigner, type Instruction } from "@solana/kit";
import { Connection, ComputeBudgetProgram, Keypair, PublicKey, SystemProgram, TransactionInstruction, TransactionMessage, VersionedTransaction, type AccountInfo } from "@solana/web3.js";
import bs58 from "bs58";

import { RWA_MULTIPLY_ROUTE } from "../domain/rwa-multiply-route-spec.js";
import { resolutionLanes } from "../policies/rwa-multiply-phase2-kamino.js";

type Json = Record<string, unknown>;
type SettingsState = Readonly<{ policySeed: { toString(): string } | null; threshold: number; timeLock: number; signers: readonly Readonly<{ key: PublicKey; permissions: Readonly<{ mask: number }> }>[] }>;
const ROOT = resolve(fileURLToPath(new URL("../../../..", import.meta.url)));
const RESOLUTION_PATH = resolve(ROOT, "docs/evidence/backyard-rwa-go/policy-resolution-v1.json");
const JOURNAL_PATH = resolve(ROOT, "docs/evidence/backyard-rwa-go/policy-phase2-obligation-init-v1.json");
const OBLIGATION_BYTES = 3_344;
const PACKET_LIMIT = 1_232;
const RENT_SYSVAR = address("SysvarRent111111111111111111111111111111111");
const Settings = (squadsGenerated as unknown as { Settings: { fromAccountInfo(account: AccountInfo<Buffer>): readonly [SettingsState, number] } }).Settings;

function invariant(value: unknown, message: string): asserts value { if (!value) throw new Error(message); }
function object(value: unknown, label: string): Json { invariant(value !== null && typeof value === "object" && !Array.isArray(value), `${label} is not an object`); return value as Json; }
function sha256(value: Uint8Array | string): string { return createHash("sha256").update(value).digest("hex"); }
function writePrivate(path: string, value: Json, flag: "wx" | "w") { writeFileSync(path, `${JSON.stringify(value, null, 2)}\n`, { flag, mode: 0o600 }); chmodSync(path, 0o600); }
function loadAdmin(): Keypair { const encoded = process.env.SOLANA_TESTING_PK?.trim(); invariant(encoded, "SOLANA_TESTING_PK is required"); const bytes = encoded.startsWith("[") ? Uint8Array.from(JSON.parse(encoded) as number[]) : bs58.decode(encoded); invariant(bytes.length === 64, "SOLANA_TESTING_PK is not a 64-byte keypair"); const admin = Keypair.fromSecretKey(bytes); invariant(admin.publicKey.toBase58() === RWA_MULTIPLY_ROUTE.setupAdmin, "SOLANA_TESTING_PK is not the exact Settings authority"); return admin; }
function kitToWeb3(instruction: Instruction): TransactionInstruction { return new TransactionInstruction({ programId: new PublicKey(instruction.programAddress), data: Buffer.from(instruction.data ?? []), keys: (instruction.accounts ?? []).map((account) => ({ pubkey: new PublicKey(account.address), isSigner: account.role === AccountRole.READONLY_SIGNER || account.role === AccountRole.WRITABLE_SIGNER, isWritable: account.role === AccountRole.WRITABLE || account.role === AccountRole.WRITABLE_SIGNER })) }); }
function compileInner(instruction: TransactionInstruction) {
  const accounts: Array<{ pubkey: PublicKey; isWritable: boolean; isSigner: false }> = [];
  const indexOf = (pubkey: PublicKey, writable: boolean) => { const prior = accounts.findIndex((account) => account.pubkey.equals(pubkey)); if (prior >= 0) { const account = accounts[prior]; invariant(account, "inner account disappeared"); account.isWritable ||= writable; return prior; } invariant(accounts.length < 255, "Squads inner account table exceeds u8"); accounts.push({ pubkey, isWritable: writable, isSigner: false }); return accounts.length - 1; };
  invariant(instruction.keys.every((key) => !key.isSigner || key.pubkey.toBase58() === RWA_MULTIPLY_ROUTE.squads.vault), "obligation initializer has a signer other than the Squads vault");
  const keyIndexes = instruction.keys.map((key) => indexOf(key.pubkey, key.isWritable)); const dataLength = Buffer.alloc(2); dataLength.writeUInt16LE(instruction.data.length);
  return { accounts, instructions: Uint8Array.from(Buffer.concat([Buffer.from([1, indexOf(instruction.programId, false), keyIndexes.length, ...keyIndexes]), dataLength, instruction.data])) } as const;
}
async function readMultipleAtMinSlot(connection: Connection, addresses: readonly PublicKey[], minContextSlot: number) {
  let lastError: unknown = null;
  for (let attempt = 0; attempt < 24; attempt += 1) {
    try {
      return await connection.getMultipleAccountsInfoAndContext([...addresses], { commitment: "confirmed", minContextSlot });
    } catch (error) {
      lastError = error;
      const message = error instanceof Error ? error.message : String(error);
      if (!message.includes("Minimum context slot has not been reached") || attempt === 23) throw error;
      await new Promise((resolve) => setTimeout(resolve, 250));
    }
  }
  throw lastError;
}
function obligationState(info: AccountInfo<Buffer> | null, lane: ReturnType<typeof resolutionLanes>[number]) {
  if (!info) return null;
  invariant(info.owner.toBase58() === RWA_MULTIPLY_ROUTE.kamino.program && info.data.length === OBLIGATION_BYTES, `${lane.key} obligation owner or size drifted`);
  const obligation = Obligation.decode(info.data);
  invariant(obligation.tag.toNumber() === 1 && String(obligation.owner) === RWA_MULTIPLY_ROUTE.squads.vault && String(obligation.lendingMarket) === lane.resolved.lendingMarket, `${lane.key} obligation tag/owner/market drifted`);
  return { tag: obligation.tag.toNumber(), owner: String(obligation.owner), market: String(obligation.lendingMarket), dataSha256: sha256(info.data) };
}

async function main() {
  invariant(process.env.CONFIRM_MAINNET === "1", "CONFIRM_MAINNET=1 is required");
  invariant(!existsSync(JOURNAL_PATH), `final journal already exists at ${JOURNAL_PATH}`);
  const rpcUrl = process.env.SOLANA_RPC_URL?.trim(); invariant(rpcUrl, "SOLANA_RPC_URL is required");
  const connection = new Connection(rpcUrl, "confirmed"); invariant(await connection.getGenesisHash() === RWA_MULTIPLY_ROUTE.genesisHash, "RPC is not mainnet-beta");
  const admin = loadAdmin();
  const resolutionBytes = readFileSync(RESOLUTION_PATH); const resolution = object(JSON.parse(resolutionBytes.toString("utf8")), "Phase-2 resolution");
  invariant(resolution.schema === "loyal-backyard-rwa-policy-resolution/v1" && resolution.commitment === "confirmed" && resolution.laneGraphExact === true, "exact confirmed Phase-2 resolution is absent");
  const lanes = resolutionLanes(resolution); invariant(lanes.length === 11, "resolution does not contain 11 exact lanes");
  const recovered: Json[] = [];
  const pendingPath = `${JOURNAL_PATH}.pending`;
  if (existsSync(pendingPath)) {
    const pending = object(JSON.parse(readFileSync(pendingPath, "utf8")), "pending obligation journal");
    const lane = lanes.find((candidate) => candidate.key === pending.lane);
    const operation = object(pending.operation, "pending obligation operation");
    const signature = String(operation.expectedSignature ?? "");
    invariant(lane && signature.length > 0 && operation.obligation === lane.resolved.obligation, "pending journal does not bind one exact resolved obligation");
    const status = await connection.getSignatureStatuses([signature], { searchTransactionHistory: true });
    invariant(status.value[0]?.err === null && (status.value[0]?.confirmationStatus === "confirmed" || status.value[0]?.confirmationStatus === "finalized"), "pending signed wire has not landed successfully; refusing replacement or resend");
    const readback = await readMultipleAtMinSlot(connection, [new PublicKey(lane.resolved.obligation)], status.context.slot);
    const state = obligationState(readback.value[0] ?? null, lane); invariant(state !== null, "pending signed wire did not create its exact obligation");
    recovered.push({ lane: lane.key, action: "init-obligation", signature, confirmedSlot: readback.context.slot, policyChanges: 0, packetBytes: operation.packetBytes ?? null, fundingLamports: operation.fundingLamports ?? null, beforeObligationAbsent: true, after: state, recoveredFromPendingJournal: true });
    renameSync(pendingPath, `${JOURNAL_PATH}.${lane.key.replaceAll("/", "-")}.sent-wire`);
  }
  const settingsRead = await connection.getAccountInfoAndContext(new PublicKey(RWA_MULTIPLY_ROUTE.squads.settings), { commitment: "confirmed" });
  invariant(settingsRead.value?.owner.toBase58() === RWA_MULTIPLY_ROUTE.squads.program, "Squads Settings is absent or wrong owner");
  const [settings] = Settings.fromAccountInfo(settingsRead.value);
  invariant(settings.threshold === 1 && settings.timeLock === 0 && settings.signers.length === 1 && settings.signers[0]?.key.toBase58() === admin.publicKey.toBase58() && settings.signers[0]?.permissions.mask === 7, "current Settings authority boundary drifted");
  const addresses = lanes.map((lane) => new PublicKey(lane.resolved.obligation));
  const existing = await readMultipleAtMinSlot(connection, addresses, settingsRead.context.slot);
  const missing = lanes.filter((lane, index) => existing.value[index] === null);
  const present = lanes.filter((lane, index) => existing.value[index] !== null).map((lane, index) => ({ lane: lane.key, state: obligationState(existing.value[lanes.findIndex((candidate) => candidate.key === lane.key)] ?? null, lane) }));
  if (missing.length === 0) { writePrivate(JOURNAL_PATH, { schema: "loyal-backyard-rwa-phase2-obligation-init/v1", verdict: "PASS_ALREADY_RECONCILED", broadcast: false, commitment: "confirmed", resolutionSha256: sha256(resolutionBytes), settings: RWA_MULTIPLY_ROUTE.squads.settings, present }, "wx"); console.log(JSON.stringify({ verdict: "PASS_ALREADY_RECONCILED", journal: JOURNAL_PATH })); return; }
  const [metadataAddress] = await userMetadataPda(address(RWA_MULTIPLY_ROUTE.squads.vault), address(RWA_MULTIPLY_ROUTE.kamino.program));
  const metadata = new PublicKey(metadataAddress); const metadataInfo = await connection.getAccountInfo(metadata, "confirmed"); invariant(metadataInfo?.owner.toBase58() === RWA_MULTIPLY_ROUTE.kamino.program, "Kamino user metadata is absent or wrong owner");
  const rent = await connection.getMinimumBalanceForRentExemption(OBLIGATION_BYTES, "confirmed");
  const operations: Json[] = [...recovered];
  for (const lane of missing) {
    const [derived] = PublicKey.findProgramAddressSync([Buffer.from([1]), Buffer.from([0]), new PublicKey(RWA_MULTIPLY_ROUTE.squads.vault).toBuffer(), new PublicKey(lane.resolved.lendingMarket).toBuffer(), new PublicKey(lane.resolved.collateralReserve.liquidityMint).toBuffer(), new PublicKey(lane.resolved.debtReserve.liquidityMint).toBuffer()], new PublicKey(RWA_MULTIPLY_ROUTE.kamino.program));
    invariant(derived.toBase58() === lane.resolved.obligation, `${lane.key} resolved obligation PDA does not derive exactly`);
    const vaultBefore = await connection.getBalance(new PublicKey(RWA_MULTIPLY_ROUTE.squads.vault), "confirmed");
    const fundingLamports = Math.max(0, rent - vaultBefore);
    const inner = kitToWeb3(initObligation({ args: { tag: 1, id: 0 } }, { obligationOwner: createNoopSigner(RWA_MULTIPLY_ROUTE.squads.vault), feePayer: createNoopSigner(RWA_MULTIPLY_ROUTE.squads.vault), obligation: address(lane.resolved.obligation), lendingMarket: address(lane.resolved.lendingMarket), seed1Account: address(lane.resolved.collateralReserve.liquidityMint), seed2Account: address(lane.resolved.debtReserve.liquidityMint), ownerUserMetadata: address(metadata.toBase58()), rent: RENT_SYSVAR, systemProgram: RWA_MULTIPLY_ROUTE.programs.system }, [], RWA_MULTIPLY_ROUTE.kamino.program));
    const compiled = compileInner(inner);
    const execute = executeTransactionSyncV2({ feePayer: admin.publicKey, settingsPda: new PublicKey(RWA_MULTIPLY_ROUTE.squads.settings), accountIndex: RWA_MULTIPLY_ROUTE.squads.vaultIndex, numSigners: 1, instructions: compiled.instructions, instruction_accounts: [{ pubkey: admin.publicKey, isSigner: true, isWritable: false }, ...compiled.accounts] });
    const latest = await connection.getLatestBlockhashAndContext({ commitment: "confirmed", minContextSlot: settingsRead.context.slot });
    const transaction = new VersionedTransaction(new TransactionMessage({ payerKey: admin.publicKey, recentBlockhash: latest.value.blockhash, instructions: [ComputeBudgetProgram.setComputeUnitLimit({ units: 100_000 }), ComputeBudgetProgram.setComputeUnitPrice({ microLamports: 50_000 }), ...(fundingLamports > 0 ? [SystemProgram.transfer({ fromPubkey: admin.publicKey, toPubkey: new PublicKey(RWA_MULTIPLY_ROUTE.squads.vault), lamports: fundingLamports })] : []), execute] }).compileToV0Message());
    transaction.sign([admin]); const wire = transaction.serialize(); invariant(wire.length <= PACKET_LIMIT, `${lane.key} initializer packet is ${wire.length} bytes`); const signature = bs58.encode(transaction.signatures[0]!);
    const before = await readMultipleAtMinSlot(connection, [new PublicKey(lane.resolved.obligation), new PublicKey(RWA_MULTIPLY_ROUTE.squads.vault)], latest.context.slot); invariant(before.value[0] === null, `${lane.key} obligation appeared before the bounded send`);
    const simulation = await connection.simulateTransaction(VersionedTransaction.deserialize(wire), { commitment: "confirmed", sigVerify: true, replaceRecentBlockhash: false, minContextSlot: before.context.slot, accounts: { encoding: "base64", addresses: [lane.resolved.obligation, RWA_MULTIPLY_ROUTE.squads.vault] } });
    invariant(simulation.value.err === null, `${lane.key} signed initializer simulation failed: ${JSON.stringify(simulation.value.err)}`);
    writePrivate(`${JOURNAL_PATH}.pending`, { schema: "loyal-backyard-rwa-phase2-obligation-init/v1", verdict: "SIGNED_SIMULATION_PASS_PENDING_SEND", broadcast: true, commitment: "confirmed", resolutionSha256: sha256(resolutionBytes), lane: lane.key, policyChanges: 0, operation: { obligation: lane.resolved.obligation, packetBytes: wire.length, expectedSignature: signature, wireSha256: sha256(wire), fundingLamports, innerInstructionCount: 1, unitsConsumed: simulation.value.unitsConsumed ?? null, signedWireBase64: Buffer.from(wire).toString("base64") } }, "wx");
    const sent = await connection.sendRawTransaction(wire, { skipPreflight: true, maxRetries: 0 }); invariant(sent === signature, `${lane.key} RPC returned a different signature than the signed wire`);
    const confirmation = await connection.confirmTransaction({ signature: sent, blockhash: latest.value.blockhash, lastValidBlockHeight: latest.value.lastValidBlockHeight }, "confirmed"); invariant(confirmation.value.err === null, `${lane.key} initializer confirmed with error: ${JSON.stringify(confirmation.value.err)}`);
    const after = await readMultipleAtMinSlot(connection, [new PublicKey(lane.resolved.obligation)], confirmation.context.slot); const state = obligationState(after.value[0] ?? null, lane); invariant(state !== null, `${lane.key} obligation is absent after confirmed initializer`);
    operations.push({ lane: lane.key, action: "init-obligation", signature: sent, confirmedSlot: after.context.slot, policyChanges: 0, packetBytes: wire.length, fundingLamports, beforeObligationAbsent: true, after: state });
    renameSync(`${JOURNAL_PATH}.pending`, `${JOURNAL_PATH}.${lane.key.replaceAll("/", "-")}.sent-wire`);
  }
  writePrivate(JOURNAL_PATH, { schema: "loyal-backyard-rwa-phase2-obligation-init/v1", verdict: "CONFIRMED_RECONCILED", broadcast: true, commitment: "confirmed", resolutionSha256: sha256(resolutionBytes), settings: { address: RWA_MULTIPLY_ROUTE.squads.settings, contextSlot: settingsRead.context.slot, policySeed: settings.policySeed?.toString() ?? null }, operations, unchangedPolicySurface: true }, "wx");
  console.log(JSON.stringify({ verdict: "CONFIRMED_RECONCILED", journal: JOURNAL_PATH, initialized: operations.map((entry) => ({ lane: entry.lane, signature: entry.signature })) }));
}

main().catch((error) => { const rpcUrl = process.env.SOLANA_RPC_URL?.trim(); const message = error instanceof Error ? error.message : String(error); console.error(rpcUrl ? message.replaceAll(rpcUrl, "<rpc>") : message); process.exitCode = 1; });
