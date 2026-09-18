/**
 * Production bridge policy rollover: fresh-seed install of the strategy-two
 * custom policy set at seeds 152-155 (base 151) with the approved raised
 * ceilings (200k USDC per execution and per day - stricter than the 1m family
 * envelope; reported-NAV ceiling unchanged at the 1m vault maxCap). The old
 * set is never edited: replacement is forward-only, and retirement of seeds
 * 145-148 is a separate root-owned action.
 *
 * One journal per index under the root-chosen absolute directory. The exact
 * signed wire is journaled (verified, exclusive, flushed, 0600) before the
 * single send; any later invocation with an existing journal is a READONLY
 * finality and exact-wire reconcile that can never resend.
 */
import { createHash, createPublicKey, verify } from "node:crypto";
import { chmodSync, existsSync, readFileSync, writeFileSync } from "node:fs";
import { isAbsolute, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

import { generated as squadsGenerated } from "@loyal-labs/loyal-smart-accounts-core";
import { address } from "@solana/kit";
import { Connection, MessageV0, PublicKey, TransactionInstruction, TransactionMessage, VersionedTransaction, type AccountInfo } from "@solana/web3.js";
import bs58 from "bs58";
import BN from "bn.js";

import { type CustomPolicyTarget } from "../domain/custom-policy-target.js";
import { rwaMultiplyStrategyTwoTarget, type StrategyTwoIdentity } from "../domain/rwa-multiply-strategy2-route-spec.js";
import { fromWeb3Instruction, prepareSignedV0Transaction } from "../integrations/solana-compat.js";
import { signingMaterialFromEnvironment } from "../integrations/signer.js";
import {
  buildInstalledRowExpectations,
  compileCustomPolicyArtifact,
  installedPolicyRow,
  readFinalizedCustomPolicySeed,
  type CustomPolicyArtifact,
  type InstalledRowExpectations,
} from "../policies/rwa-multiply-custom.js";

export const JOURNAL_SCHEMA = "production-bridge-policy-install-attempt/v1";
const MAINNET_GENESIS_HASH = "5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d";
const BRIDGE_IDENTITY: StrategyTwoIdentity = {
  config: address("DCpR24Eb6xCWxDyaZvCBTkadkxCB2vkqJN1EfYNWtLxY"),
  delegatedSigner: address("62JLkPeE4oG65LRB3W3m52RVicmYq3xFHdv7TecCsPj5"),
};
const BRIDGE_SEED_BASE = 151n;
const BRIDGE_SEED_CEILING = 155n;
const BRIDGE_AMOUNT_CAP_RAW = 200_000_000_000n;
const BRIDGE_DAILY_LIMIT_RAW = 200_000_000_000n;
const PACKET_LIMIT = 1_232;
const FEE_LIMIT = 50_000;
const MAX_POLICY_COST_LAMPORTS = 20_000_000;
const SQUADS_LOADER = "BPFLoaderUpgradeab1e11111111111111111111111";
const SQUADS_ELF_SHA256 = "1c95bd7be140589d2aec38a85d7ecfe70ec69639277f622c898f821ab1d636fa";

type SettingsState = {
  policySeed: { toString(): string } | null;
  threshold: number;
  timeLock: number;
  signers: readonly { key: PublicKey; permissions: { mask: number } }[];
};
const Settings = (squadsGenerated as unknown as {
  Settings: {
    deserialize(buffer: Buffer): readonly [SettingsState, number];
    fromArgs(args: Record<string, unknown>): { serialize(): [Buffer, number] };
  };
}).Settings;
const Policy = (squadsGenerated as unknown as {
  Policy: {
    deserialize(buffer: Buffer): readonly [Record<string, unknown>, number];
    fromArgs(args: Record<string, unknown>): { serialize(): [Buffer, number] };
  };
}).Policy;

type PolicySnapshot = { owner: string; lamports: number; executable: boolean; data: Uint8Array };

function fail(value: unknown, message: string): asserts value {
  if (!value) throw new Error(message);
}
function sha256(bytes: Uint8Array): string {
  return createHash("sha256").update(bytes).digest("hex");
}
function bridgeConnection(): Connection {
  const rpcUrl = process.env.SOLANA_RPC_URL?.trim();
  fail(rpcUrl, "SOLANA_RPC_URL is required");
  return new Connection(rpcUrl, "finalized");
}
function bridgeInstruction(create: CustomPolicyArtifact["policies"][number]["createInstruction"]): TransactionInstruction {
  return new TransactionInstruction({
    programId: new PublicKey(create.programId),
    keys: create.accounts.map((account) => ({
      pubkey: new PublicKey(account.address),
      isSigner: account.signer,
      isWritable: account.writable,
    })),
    data: Buffer.from(create.dataBase64, "base64"),
  });
}
/** Canonical account bytes: exact serialization plus zero padding to the stored length. */
function isCanonicalPaddedBytes(data: Buffer, canonical: Buffer): boolean {
  return data.length >= canonical.length && data.subarray(0, canonical.length).equals(canonical)
    && data.subarray(canonical.length).every((byte) => byte === 0);
}

/** The stored policy must be the canonical codec serialization plus zero padding. */
function assertCanonicalPolicyBytes(data: Uint8Array, context: string) {
  const [decoded] = Policy.deserialize(Buffer.from(data));
  fail(isCanonicalPaddedBytes(Buffer.from(data), Policy.fromArgs({ ...decoded }).serialize()[0]!),
    `${context}: policy bytes are not the canonical codec serialization plus zero padding`);
}

/** The approved bridge target: strategy-two identities, seeds 152-155, raised ceilings. */
export async function productionBridgeTarget(): Promise<CustomPolicyTarget> {
  const base = await rwaMultiplyStrategyTwoTarget(BRIDGE_IDENTITY, BRIDGE_SEED_BASE);
  return {
    ...base,
    caps: {
      ...base.caps,
      amountRaw: BRIDGE_AMOUNT_CAP_RAW,
      dailySpendingLimit: { ...base.caps.dailySpendingLimit!, maxPerPeriodRaw: BRIDGE_DAILY_LIMIT_RAW },
    },
  };
}

/** Read-only compile of the bridge artifact against the finalized Settings counter. */
export async function compileProductionBridgePolicies() {
  const connection = bridgeConnection();
  const target = await productionBridgeTarget();
  const { contextSlot, policySeedBefore } = await readFinalizedCustomPolicySeed(connection);
  fail(policySeedBefore >= BRIDGE_SEED_BASE && policySeedBefore <= BRIDGE_SEED_CEILING,
    `Finalized Settings counter ${policySeedBefore} escaped the bridge rollover window ${BRIDGE_SEED_BASE}-${BRIDGE_SEED_CEILING}`);
  return { connection, contextSlot, policySeedBefore, target, artifact: await compileCustomPolicyArtifact(BRIDGE_SEED_BASE, target) };
}

/** Canonical journal path: a function of the root-chosen directory and index only. */
export function bridgeJournalPath(journalDir: string, index: number): string {
  fail(Number.isInteger(index) && index >= 0 && index < 4, "Choose bridge policy index 0..3");
  fail(journalDir.length > 0 && isAbsolute(journalDir) && resolve(journalDir) === journalDir,
    "journal directory must be absolute and canonical");
  return join(journalDir, `production-bridge-policy-install-seed-${152 + index}.json`);
}

/**
 * Settings may only advance the policy seed. Decode-compares the authority
 * fields and encode-compares both buffers against the canonical serialization
 * with the seed swapped to `expectedSeed` (account data may carry zero
 * padding, never assumed to be exactly one buffer length).
 */
export function assertSettingsOnlySeedAdvanced(preData: Buffer, postData: Buffer, expectedSeed: bigint, context: string) {
  const [pre] = Settings.deserialize(preData);
  const [post] = Settings.deserialize(postData);
  const sameAuthority = pre.threshold === post.threshold && pre.timeLock === post.timeLock
    && pre.signers.length === post.signers.length
    && pre.signers.every((signer, i) => signer.key.equals(post.signers[i]!.key)
      && signer.permissions.mask === post.signers[i]!.permissions.mask);
  fail(sameAuthority && post.policySeed !== null && BigInt(post.policySeed.toString()) === expectedSeed,
    `${context}: Settings changed beyond the policy seed`);
  const withSeed = (seed: bigint) => Settings.fromArgs({
    ...pre, policySeed: pre.policySeed === null ? null : new BN(String(seed)),
  }).serialize()[0]!;
  fail(isCanonicalPaddedBytes(preData, withSeed(BigInt(post.policySeed!.toString()) - 1n))
    && isCanonicalPaddedBytes(postData, withSeed(expectedSeed)) && preData.length === postData.length,
  `${context}: Settings bytes are not the canonical seed-only advance`);
}

/**
 * Recovery-side Settings evolution check: the journaled pre-send bytes must
 * be canonical, and the current bytes must be exactly the original Settings
 * serialization with only policySeed replaced by the actual current seed,
 * which must lie in [seedFloor, seedCeiling]. Returns the current seed.
 */
export function assertSettingsEvolvedFromJournal(preData: Buffer, currentData: Buffer, seedFloor: bigint,
  seedCeiling: bigint, context: string): bigint {
  const [pre] = Settings.deserialize(preData);
  const [current] = Settings.deserialize(currentData);
  fail(current.policySeed !== null, `${context}: current Settings has no policy seed`);
  const currentSeed = BigInt(current.policySeed.toString());
  fail(currentSeed >= seedFloor && currentSeed <= seedCeiling,
    `${context}: Settings seed ${currentSeed} escaped the verified window ${seedFloor}-${seedCeiling}`);
  fail(pre.threshold === current.threshold && pre.timeLock === current.timeLock
    && pre.signers.length === current.signers.length
    && pre.signers.every((signer, i) => signer.key.equals(current.signers[i]!.key)
      && signer.permissions.mask === current.signers[i]!.permissions.mask),
  `${context}: Settings authority drifted from the journaled pre-send bytes`);
  fail(isCanonicalPaddedBytes(preData, Settings.fromArgs({ ...pre }).serialize()[0]!),
    `${context}: journaled pre-settings bytes are not canonical`);
  const evolved = Settings.fromArgs({
    ...pre, policySeed: pre.policySeed === null ? null : new BN(String(currentSeed)),
  }).serialize()[0]!;
  fail(isCanonicalPaddedBytes(currentData, evolved) && preData.length === currentData.length,
    `${context}: Settings bytes are not the journaled pre-state with only the policy seed advanced`);
  return currentSeed;
}

function assertBridgeNativeEffects(accountKeys: readonly PublicKey[], policyAddress: string, admin: string,
  meta: { fee: number; preBalances: readonly number[]; postBalances: readonly number[] }, rent: number) {
  const payerIndex = accountKeys.findIndex((key) => key.toBase58() === admin);
  const policyIndex = accountKeys.findIndex((key) => key.toBase58() === policyAddress);
  fail(payerIndex >= 0 && policyIndex >= 0 && payerIndex !== policyIndex
    && meta.preBalances.length === accountKeys.length && meta.postBalances.length === accountKeys.length
    && [...meta.preBalances, ...meta.postBalances, meta.fee].every((value) => Number.isSafeInteger(value) && value >= 0),
  "Incomplete bridge balance evidence");
  fail(Number.isSafeInteger(rent) && rent > 0 && meta.fee > 0 && meta.fee <= FEE_LIMIT
    && meta.preBalances[policyIndex] === 0 && meta.postBalances[policyIndex] === rent
    && meta.preBalances[payerIndex]! - meta.postBalances[payerIndex]! === rent + meta.fee
    && meta.preBalances.every((value, i) => i === payerIndex || i === policyIndex || value === meta.postBalances[i]),
  "Bridge native effects are not exactly fee plus policy rent from the admin");
}

/** Semantic + byte-level projection checks shared by the unsigned and signed simulations. */
async function assertBridgeProjection(connection: Connection, target: CustomPolicyTarget, artifact: CustomPolicyArtifact,
  index: number, pre: { settings: Buffer; adminLamports: number; settingsLamports: number },
  post: { policy: PolicySnapshot; settings: PolicySnapshot; adminLamports: number }, fee: number, slot: number) {
  const policy = artifact.policies[index]!;
  fail(post.policy.owner === target.route.squads.program && !post.policy.executable
    && post.settings.owner === target.route.squads.program && !post.settings.executable,
  "Simulation projected a wrong owner or an executable account");
  fail(Number.isSafeInteger(fee) && fee > 0 && fee <= FEE_LIMIT, `Bridge fee ${fee} exceeds ${FEE_LIMIT}`);
  fail([pre.adminLamports, pre.settingsLamports, post.adminLamports, post.policy.lamports, post.settings.lamports]
    .every((value) => Number.isSafeInteger(value) && value >= 0), "Bridge projection balances are not safe nonnegative integers");
  const rent = await connection.getMinimumBalanceForRentExemption(post.policy.data.length, "confirmed");
  fail(Number.isSafeInteger(rent) && rent > 0 && post.policy.lamports === rent,
    `Bridge policy rent ${post.policy.lamports} is not the data-length exemption ${rent}`);
  fail(rent + fee <= MAX_POLICY_COST_LAMPORTS, "Bridge policy cost exceeds the 20m lamport bound");
  fail(post.adminLamports === pre.adminLamports - rent - fee && post.settings.lamports === pre.settingsLamports,
    "Bridge lamport effects are not exactly rent plus fee from the admin");
  assertSettingsOnlySeedAdvanced(pre.settings, Buffer.from(post.settings.data), BigInt(policy.seed), "Bridge simulation");
  assertCanonicalPolicyBytes(post.policy.data, "Simulated bridge policy");
  const landingBlockTime = await connection.getBlockTime(slot);
  fail(landingBlockTime !== null, "Simulation slot block time is unavailable");
  const expectations = await buildInstalledRowExpectations(target, { landingBlockTime, requireUntouchedUsage: true });
  const row = installedPolicyRow({
    target, expected: policy, index,
    info: { owner: new PublicKey(post.policy.owner), data: post.policy.data },
    expectations,
  });
  fail(row.pass, `Simulated bridge ${row.operation} policy drifted from the contract: ${row.reason ?? ""}`);
  return { rent, landingBlockTime, row, policyDataSha256: sha256(post.policy.data), settingsDataSha256: sha256(post.settings.data) };
}

async function readBridgePreflightAccounts(connection: Connection, keys: readonly PublicKey[], minContextSlot?: number) {
  return connection.getMultipleAccountsInfoAndContext([...keys],
    minContextSlot === undefined ? { commitment: "confirmed" } : { commitment: "confirmed", minContextSlot });
}

/** One drift line per changed key: address plus equality kind. Never raw data bytes. */
function describeBridgeAccountDiff(label: string, before: AccountInfo<Buffer> | null, after: AccountInfo<Buffer> | null): string | null {
  if (before === null || after === null) {
    return before === after ? null
      : `${label}: existence ${before === null ? "absent" : "present"} -> ${after === null ? "absent" : "present"}`;
  }
  const kinds: string[] = [];
  if (!before.owner.equals(after.owner)) kinds.push(`owner ${before.owner.toBase58()} -> ${after.owner.toBase58()}`);
  if (before.executable !== after.executable) kinds.push(`executable ${before.executable} -> ${after.executable}`);
  if (before.lamports !== after.lamports) kinds.push(`lamports ${before.lamports} -> ${after.lamports}`);
  if (sha256(before.data) !== sha256(after.data)) kinds.push(`dataSha256 ${sha256(before.data)} -> ${sha256(after.data)}`);
  return kinds.length === 0 ? null : `${label}: ${kinds.join("; ")}`;
}

export function assertBridgeStateUnchanged(keys: readonly PublicKey[], first: readonly (AccountInfo<Buffer> | null)[],
  second: readonly (AccountInfo<Buffer> | null)[]) {
  fail(first.length === second.length && first.every((account, i) => {
    const fresh = second[i];
    return account === null ? fresh === null : !!fresh && account.owner.equals(fresh.owner)
      && account.executable === fresh.executable && account.lamports === fresh.lamports && account.data.equals(fresh.data);
  }), `Bridge protected state changed before journaling: ${keys.map((key, i) =>
    describeBridgeAccountDiff(key.toBase58(), first[i] ?? null, second[i] ?? null))
    .filter((diff): diff is string => diff !== null).join(" | ") || "unknown drift"}`);
}

async function assertBridgePreflight(connection: Connection, target: CustomPolicyTarget, artifact: CustomPolicyArtifact, index: number) {
  fail(await connection.getGenesisHash() === MAINNET_GENESIS_HASH, "RPC is not mainnet-beta");
  const program = await connection.getAccountInfo(new PublicKey(target.route.squads.program), "finalized");
  fail(program && program.owner.toBase58() === SQUADS_LOADER && program.executable
    && program.data.length === 36 && program.data.readUInt32LE(0) === 2, "Squads executable identity mismatch");
  // settings, admin, four candidate policies, program, program data.
  const keys = [target.route.squads.settings, target.route.setupAdmin,
    ...artifact.policies.map((policy) => policy.policy), target.route.squads.program,
    new PublicKey(program!.data.subarray(4)).toBase58()].map((value) => new PublicKey(value));
  const before = await readBridgePreflightAccounts(connection, keys);
  const image = before.value[7];
  fail(before.value[6]?.data.equals(program!.data) && image && image.owner.toBase58() === SQUADS_LOADER
    && !image.executable && image.data.length > 45 && image.data.readUInt32LE(0) === 3
    && sha256(image.data.subarray(45)) === SQUADS_ELF_SHA256, "Squads differs from the pinned policy proof binary");
  const [settings, admin] = [before.value[0], before.value[1]];
  fail(settings && settings.owner.toBase58() === target.route.squads.program && !settings.executable && admin,
    "Squads Settings or admin is absent");
  const [decoded] = Settings.deserialize(settings!.data);
  fail(decoded.policySeed?.toString() === String(BRIDGE_SEED_BASE + BigInt(index)) && decoded.threshold === 1
    && decoded.timeLock === 0 && decoded.signers.length === 1
    && decoded.signers[0]!.key.toBase58() === target.route.setupAdmin
    && decoded.signers[0]!.permissions.mask === 7, "Settings authority or sequential bridge seed drifted");
  const expectations = await buildInstalledRowExpectations(target);
  for (let i = 0; i < 4; i++) {
    if (i < index) {
      const prior = before.value[2 + i]!;
      fail(prior, "Preceding bridge policy is absent");
      assertCanonicalPolicyBytes(prior.data, "Preceding bridge policy");
      fail(installedPolicyRow({ target, expected: artifact.policies[i]!, index: i, info: prior, expectations }).pass,
        "Preceding bridge policy drifted from the contract");
    } else {
      fail(before.value[2 + i] === null, i === index ? "Candidate bridge policy already exists" : "Future bridge policy already exists");
    }
  }
  return {
    keys, beforeAccounts: before.value, settings: settings!, contextSlot: before.context.slot,
    adminLamports: admin!.lamports, settingsLamports: settings!.lamports,
  };
}

/** Journal replay barrier: identity, admin signature, and the full canonical v0 message. */
export function verifyBridgeJournalEntry(value: unknown, target: CustomPolicyTarget, artifact: CustomPolicyArtifact, index: number) {
  const policy = artifact.policies[index]!;
  const row = value as Record<string, unknown>;
  fail(row && row.schema === JOURNAL_SCHEMA && row.seed === policy.seed && row.policy === policy.policy
    && row.operation === policy.operation && row.index === index, "Bridge journal identity mismatch");
  fail(typeof row.wireBase64 === "string" && typeof row.signature === "string"
    && typeof row.preSettingsDataBase64 === "string", "Missing bridge signed wire or pre-settings bytes");
  const wire = Buffer.from(row.wireBase64, "base64");
  fail(wire.toString("base64") === row.wireBase64 && sha256(wire) === row.wireSha256, "Bridge wire hash mismatch");
  const transaction = VersionedTransaction.deserialize(wire);
  const admin = new PublicKey(target.route.setupAdmin);
  fail(transaction.message.version === 0 && transaction.message.staticAccountKeys[0]!.equals(admin)
    && transaction.signatures.length === 1 && bs58.encode(transaction.signatures[0]!) === row.signature,
  "Bridge journal signature identity mismatch");
  fail(verify(null, transaction.message.serialize(), createPublicKey({
    key: { kty: "OKP", crv: "Ed25519", x: admin.toBuffer().toString("base64url") }, format: "jwk",
  }), transaction.signatures[0]!), "Bridge journal signature verification failed");
  // Recompile the canonical v0 message from the exact create instruction and
  // the saved blockhash: header, key ordering, writable/signer flags, and
  // instruction bytes must all be byte-identical to the saved message.
  const message = transaction.message as MessageV0;
  const canonical = MessageV0.compile({
    payerKey: admin,
    instructions: [bridgeInstruction(policy.createInstruction)],
    addressLookupTableAccounts: [],
    recentBlockhash: message.recentBlockhash,
  });
  fail(Buffer.from(canonical.serialize()).equals(Buffer.from(message.serialize())),
    "Bridge wire is not the exact reviewed create transaction");
  return { policy, wire, signature: row.signature as string, transaction, journal: row };
}

/**
 * READONLY reconcile of a journaled wire: finality, exact-wire and message
 * equality, native effects, and on-chain semantics. Never resends. On-chain
 * Settings authority must still match the journaled pre-send bytes and its
 * seed must be the policy's seed or a later verified one (up to the ceiling),
 * so recovering an early index after later policies landed still passes.
 */
async function reconcileJournaledBridgePolicy(connection: Connection, target: CustomPolicyTarget,
  artifact: CustomPolicyArtifact, index: number, journalPath: string) {
  const saved = verifyBridgeJournalEntry(JSON.parse(readFileSync(journalPath, "utf8")), target, artifact, index);
  const status = (await connection.getSignatureStatuses([saved.signature], { searchTransactionHistory: true })).value[0];
  if (!status || status.confirmationStatus !== "finalized") {
    return { verdict: "PENDING_RECONCILIATION", signature: saved.signature, broadcast: "unknown" as const, journal: journalPath };
  }
  if (status.err !== null) return { verdict: "FINALIZED_FAILED", signature: saved.signature, broadcast: true, journal: journalPath };
  const landed = await connection.getTransaction(saved.signature, { commitment: "finalized", maxSupportedTransactionVersion: 0 });
  fail(landed && landed.slot === status.slot && landed.meta?.err === null && landed.blockTime != null
    && Buffer.from(landed.transaction.message.serialize()).equals(Buffer.from(saved.transaction.message.serialize())),
  "Finalized bridge transaction mismatches the journaled wire");
  const [policyInfo, settingsInfo, ...packet] = await connection.getMultipleAccountsInfo(
    [new PublicKey(saved.policy.policy), new PublicKey(target.route.squads.settings),
      ...artifact.policies.map((candidate) => new PublicKey(candidate.policy))], "finalized");
  fail(policyInfo && policyInfo.owner.toBase58() === target.route.squads.program && !policyInfo.executable,
    "Finalized bridge policy is absent, executable, or wrongly owned");
  assertCanonicalPolicyBytes(policyInfo!.data, "Finalized bridge policy");
  const rent = await connection.getMinimumBalanceForRentExemption(policyInfo!.data.length, "finalized");
  assertBridgeNativeEffects(saved.transaction.message.staticAccountKeys, saved.policy.policy, target.route.setupAdmin, landed!.meta!, rent);
  fail(settingsInfo && settingsInfo.owner.toBase58() === target.route.squads.program && !settingsInfo.executable,
    "Finalized Settings is absent, executable, or wrongly owned");
  const currentSeed = assertSettingsEvolvedFromJournal(
    Buffer.from(saved.journal.preSettingsDataBase64 as string, "base64"), settingsInfo!.data,
    BigInt(saved.policy.seed), BRIDGE_SEED_CEILING, "Finalized bridge settings");
  // Every packet policy the seed counter says was created later must itself
  // verify; nothing beyond the current seed may exist inside the packet.
  const plainExpectations = await buildInstalledRowExpectations(target);
  for (let i = 0; i < 4; i++) {
    if (i === index) continue;
    const candidate = artifact.policies[i]!;
    const info = packet[i];
    if (BigInt(candidate.seed) <= currentSeed) {
      fail(info && info.owner.toBase58() === target.route.squads.program && !info.executable,
        `Later bridge policy ${candidate.seed} is absent, executable, or wrongly owned`);
      assertCanonicalPolicyBytes(info!.data, `Later bridge policy ${candidate.seed}`);
      fail(installedPolicyRow({ target, expected: candidate, index: i, info: info!, expectations: plainExpectations }).pass,
        `Later bridge ${candidate.operation} policy drifted from the contract`);
    } else {
      fail(info === null, `Future bridge policy ${candidate.seed} already exists during recovery`);
    }
  }
  const expectations = await buildInstalledRowExpectations(target,
    { landingBlockTime: landed!.blockTime!, requireUntouchedUsage: true });
  const row = installedPolicyRow({ target, expected: saved.policy, index, info: policyInfo!, expectations });
  fail(row.pass, `Finalized bridge ${row.operation} policy drifted: ${row.reason ?? ""}`);
  return {
    verdict: "FINALIZED_BRIDGE_POLICY_INSTALLED", signature: saved.signature, broadcast: true,
    slot: status.slot, journal: journalPath, rentLamports: rent, installed: row,
  };
}

/** Verify the entry, then write it exclusively and flushed at 0600, then re-verify from disk. */
function writeJournaledWire(journalPath: string, entry: Record<string, unknown>, target: CustomPolicyTarget,
  artifact: CustomPolicyArtifact, index: number) {
  verifyBridgeJournalEntry(entry, target, artifact, index);
  writeFileSync(journalPath, `${JSON.stringify(entry, (_key, item) => typeof item === "bigint" ? item.toString() : item, 2)}\n`,
    { flag: "wx", mode: 0o600, flush: true });
  chmodSync(journalPath, 0o600);
  verifyBridgeJournalEntry(JSON.parse(readFileSync(journalPath, "utf8")), target, artifact, index);
}

/** Install (or READONLY-reconcile) bridge policy `index` of 0..3 under `journalDir`. */
export async function installProductionBridgePolicy(index: number, journalDir: string, execute = false) {
  const journalPath = bridgeJournalPath(journalDir, index);
  const { connection, target, artifact } = await compileProductionBridgePolicies();
  const policy = artifact.policies[index]!;
  if (existsSync(journalPath)) {
    return reconcileJournaledBridgePolicy(connection, target, artifact, index, journalPath);
  }
  fail(!execute || process.env.CONFIRM_MAINNET === "1", "Bridge installation requires --execute and CONFIRM_MAINNET=1");
  const preflight = await assertBridgePreflight(connection, target, artifact, index);
  if (!execute) {
    const { blockhash } = await connection.getLatestBlockhash("confirmed");
    const transaction = new VersionedTransaction(new TransactionMessage({
      payerKey: new PublicKey(target.route.setupAdmin),
      recentBlockhash: blockhash,
      instructions: [bridgeInstruction(policy.createInstruction)],
    }).compileToV0Message());
    fail(transaction.serialize().length <= PACKET_LIMIT, `Bridge packet ${transaction.serialize().length} exceeds ${PACKET_LIMIT} bytes`);
    const simulation = await connection.simulateTransaction(transaction, {
      sigVerify: false, commitment: "confirmed", minContextSlot: preflight.contextSlot,
      accounts: { encoding: "base64", addresses: [target.route.squads.settings, target.route.setupAdmin, policy.policy] },
    });
    fail(simulation.value.err === null, `Bridge simulation failed: ${JSON.stringify({ err: simulation.value.err, logs: simulation.value.logs })}`);
    const [postSettings, postAdmin, postPolicy] = simulation.value.accounts ?? [];
    fail(postSettings && postAdmin && postPolicy && typeof postPolicy.data[0] === "string", "Incomplete bridge simulation accounts");
    const fee = (await connection.getFeeForMessage(transaction.message, "confirmed")).value;
    const projection = await assertBridgeProjection(connection, target, artifact, index, {
      settings: preflight.settings.data, adminLamports: preflight.adminLamports, settingsLamports: preflight.settingsLamports,
    }, {
      policy: { owner: postPolicy!.owner, lamports: postPolicy!.lamports, executable: postPolicy!.executable, data: Buffer.from(postPolicy!.data[0]!, "base64") },
      settings: { owner: postSettings!.owner, lamports: postSettings!.lamports, executable: postSettings!.executable, data: Buffer.from(postSettings!.data[0]!, "base64") },
      adminLamports: postAdmin!.lamports,
    }, fee!, simulation.context.slot);
    return {
      verdict: "BRIDGE_POLICY_SIMULATION_PASS_NOT_SENT", broadcast: false, index, operation: policy.operation,
      seed: policy.seed, policy: policy.policy, policySeedBefore: String(BRIDGE_SEED_BASE + BigInt(index)),
      packetBytes: transaction.serialize().length, feeLamports: fee, rentLamports: projection.rent,
      projectedPolicyDataSha256: projection.policyDataSha256, projectedSettingsDataSha256: projection.settingsDataSha256,
      journalPath,
    };
  }
  const material = await signingMaterialFromEnvironment("SOLANA_TESTING_PK");
  fail(material.signer.address === target.route.setupAdmin, "Bridge admin signer drifted from the approved setup admin");
  const prepared = await prepareSignedV0Transaction({
    rpcUrl: process.env.SOLANA_RPC_URL!.trim(),
    feePayer: material,
    instructions: [fromWeb3Instruction(bridgeInstruction(policy.createInstruction))],
    inspectedAddresses: [policy.policy, target.route.squads.settings, target.route.setupAdmin],
    minimumContextSlot: preflight.contextSlot,
    // Confirmed prestate/simulation matches the preflight and reread banks,
    // removing the avoidable finalization wait inside the optimistic window;
    // terminal reconciliation stays finalized.
    commitment: "confirmed",
  });
  fail(prepared.packetBytes <= PACKET_LIMIT, `Bridge packet ${prepared.packetBytes} exceeds ${PACKET_LIMIT} bytes`);
  fail(prepared.simulation.err === null, `Bridge signed simulation failed: ${JSON.stringify({ err: prepared.simulation.err, logs: prepared.simulation.logs })}`);
  const [postPolicy, postSettings, postAdmin] = prepared.simulation.postAccounts;
  fail(postPolicy && postSettings && postAdmin, "Incomplete bridge signed simulation accounts");
  const projection = await assertBridgeProjection(connection, target, artifact, index, {
    settings: preflight.settings.data, adminLamports: preflight.adminLamports, settingsLamports: preflight.settingsLamports,
  }, {
    policy: { owner: postPolicy!.owner, lamports: postPolicy!.lamports, executable: postPolicy!.executable, data: postPolicy!.data },
    settings: { owner: postSettings!.owner, lamports: postSettings!.lamports, executable: postSettings!.executable, data: postSettings!.data },
    adminLamports: postAdmin!.lamports,
  }, prepared.feeLamports, prepared.simulationSlot);
  // Re-read every protected preflight account (Settings, admin, candidates,
  // program, program data) and require byte-for-byte stability before the
  // journal barrier, then confirm the sequential seed one last time.
  // Confirmed prepare (same commitment as preflight and reread, slot-chained)
  // keeps the optimistic pre-journal window short; terminal reconciliation
  // below still reads finalized.
  const reread = await readBridgePreflightAccounts(connection, preflight.keys,
    Math.max(preflight.contextSlot, prepared.simulationSlot));
  assertBridgeStateUnchanged(preflight.keys, preflight.beforeAccounts, reread.value);
  const [rereadSettings] = Settings.deserialize(reread.value[0]!.data);
  fail(rereadSettings.policySeed?.toString() === String(BRIDGE_SEED_BASE + BigInt(index)),
    "Finalized Settings seed moved before the bridge journal barrier; refusing");
  writeJournaledWire(journalPath, {
    schema: JOURNAL_SCHEMA, index, operation: policy.operation, seed: policy.seed, policy: policy.policy,
    signature: prepared.expectedSignature, wireBase64: Buffer.from(prepared.serializedTransaction).toString("base64"),
    wireSha256: sha256(prepared.serializedTransaction), packetBytes: prepared.packetBytes,
    feeLamports: prepared.feeLamports, rentLamports: projection.rent,
    projectedPolicyDataSha256: projection.policyDataSha256, projectedSettingsDataSha256: projection.settingsDataSha256,
    preSettingsDataBase64: Buffer.from(reread.value[0]!.data).toString("base64"),
    lastValidBlockHeight: prepared.latestBlockhash.lastValidBlockHeight, simulationSlot: prepared.simulationSlot,
  }, target, artifact, index);
  try {
    const sent = await connection.sendRawTransaction(prepared.serializedTransaction, {
      skipPreflight: false, preflightCommitment: "confirmed", maxRetries: 0,
    });
    fail(sent === prepared.expectedSignature, "RPC returned a different bridge signature");
    const confirmation = await connection.confirmTransaction({ signature: sent, ...prepared.latestBlockhash }, "finalized");
    fail(confirmation.value.err === null, `Bridge transaction finalized with ${JSON.stringify(confirmation.value.err)}`);
    return await reconcileJournaledBridgePolicy(connection, target, artifact, index, journalPath);
  } catch {
    return { verdict: "PENDING_RECONCILIATION", signature: prepared.expectedSignature, broadcast: "unknown" as const, journal: journalPath };
  }
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  const [index, journalDir, ...flags] = process.argv.slice(2);
  if (index === undefined || journalDir === undefined || flags.some((flag) => flag !== "--execute")) {
    throw new Error("usage: production-bridge-policies.ts <index 0..3> <absolute-journal-dir> [--execute]");
  }
  console.log(JSON.stringify(await installProductionBridgePolicy(Number(index), resolve(journalDir), flags.includes("--execute")), null, 2));
}
