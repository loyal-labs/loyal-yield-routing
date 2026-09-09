/**
 * Phase 1 reset tooling for Voltr vault HXtk… (Loyal RWA Multiply USDC).
 *
 * Every subcommand follows the same contract:
 *   1. fetch fresh chain state (one getAccountInfo per address, rate limited),
 *   2. build the exact transaction,
 *   3. simulate it over RPC with sigVerify:false + replaceRecentBlockhash:true,
 *   4. decode the emitted Voltr event(s) from the program logs and assert the
 *      expected post-state,
 *   5. write docs/evidence/hxtk-reset-2026-09-08/<step>.simulated.json with
 *      "sent": false.
 *
 * The signing/sending path is only reachable when the operator passes
 * --execute --journal PATH with CONFIRM_MAINNET=1 and the matching signer env
 * var. --simulate (the default) never reads key material, because a simulated
 * transaction carries zero-filled signature slots and sigVerify is false.
 *
 * Reference shapes: go/backyard-rwa-worker/internal/backyardrwa/build.go,
 * report_ticket.go, crates/squads-test-harness/tests/voltr_reset_sequence.rs
 * and docs/plans/backyard-rwa-adaptor-strategy2-audit-2026-09-08.md §7.
 */

import { createHash, randomBytes } from "node:crypto";
import {
  existsSync,
  mkdirSync,
  readdirSync,
  writeFileSync,
} from "node:fs";
import { basename, dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

import { generated as squadsGenerated } from "@loyal-labs/loyal-smart-accounts-core";
import { executeSettingsTransactionSync } from "@loyal-labs/loyal-smart-accounts-core/internal";
import {
  findAssociatedTokenPda,
  getCreateAssociatedTokenIdempotentInstruction,
} from "@solana-program/token";
import {
  AccountRole,
  address,
  createNoopSigner,
  type Address,
  type Instruction,
} from "@solana/kit";
import {
  ComputeBudgetProgram,
  PublicKey,
  TransactionInstruction,
  TransactionMessage,
  VersionedTransaction,
} from "@solana/web3.js";
import {
  findRequestWithdrawVaultReceiptPda,
  getCancelRequestWithdrawVaultInstructionAsync,
  getHarvestFeeInstructionAsync,
  getRequestWithdrawVaultInstructionAsync,
  getRequestWithdrawVaultReceiptDecoder,
  getStrategyInitReceiptDecoder,
  getUpdateVaultConfigInstructionAsync,
  getVaultDecoder,
  getWithdrawVaultInstructionAsync,
  VaultConfigField,
} from "@voltr/vault-sdk";
import {
  getCancelRequestWithdrawVaultEventDecoder,
  getHarvestFeeEventDecoder,
  getRequestWithdrawVaultEventDecoder,
  getUpdateVaultConfigEventDecoder,
  getWithdrawVaultEventDecoder,
} from "@voltr/vault-sdk";
import bs58 from "bs58";

import { RWA_MULTIPLY_ROUTE } from "../domain/rwa-multiply-route-spec.js";
import {
  compileCustomPolicyArtifact,
  type CustomPolicyArtifact,
} from "../policies/rwa-multiply-custom.js";
import type { CustomPolicyTarget } from "../domain/custom-policy-target.js";
import {
  compilerSourceTreeSha256,
  runRustCompiler,
  type CompilerProvenance,
} from "../policies/compiler-build.js";
import {
  buildRwaMultiplyArmReportInstruction,
  buildRwaMultiplyManagerInstructions,
} from "../integrations/rwa-multiply-voltr.js";
import {
  prepareSignedV0Transaction,
  finalizedTransaction,
  readFinalizedSignatureStatus,
  PreparedTransactionSendError,
  sendPreparedOnce,
  sendPreparedConfirmedOnce,
  fromWeb3Instruction,
  toWeb3Instruction,
  type PreparedTransaction,
} from "../integrations/solana-compat.js";
import { signingMaterialFromEnvironment } from "../integrations/signer.js";
import {
  assertCanonicalLegAvailable as assertCanonicalLegAvailableFence,
  assertFinalizedJournalBinding,
  assertPendingJournalBinding,
  acquireCanonicalLegClaim,
  canonicalStateGeneration,
  beginCanonicalLegRecord,
  finalizedJournalSha256,
  pendingBindingSha256,
  PENDING_BINDING_MISMATCH,
  readCanonicalState,
  readBoundJournal,
  readBoundPending,
  releaseCanonicalLegClaim,
  renamePrivateFile,
  repairPostFinalizationStatus,
  resolveCanonicalStateRoot,
  rewritePendingStatus,
  writePrivateAtomic,
  writePrivateExclusive,
  writeCanonicalStateCas,
  type CanonicalLegClaim,
  type CanonicalLegStateStatus,
} from "./hxtk-fence.js";
import { parseHxtkCli } from "./hxtk-cli.js";

const REPOSITORY_ROOT = resolve(fileURLToPath(new URL("../../../..", import.meta.url)));
const EVIDENCE_DIR = resolve(REPOSITORY_ROOT, "docs/evidence/hxtk-reset-2026-09-08");
const SCHEMA = "loyal-voltr-hxtk-reset-step/v1";

// ---- identities (all public keys; no secrets) --------------------------------

const VOLTR = address(RWA_MULTIPLY_ROUTE.programs.voltr);
const SQUADS_PROGRAM = RWA_MULTIPLY_ROUTE.squads.program;
const SQUADS_SETTINGS = RWA_MULTIPLY_ROUTE.squads.settings;
const VAULT = address(RWA_MULTIPLY_ROUTE.vault.address);
const ADMIN = RWA_MULTIPLY_ROUTE.setupAdmin; // BAqg…  vault admin
const PROTOCOL = address("4sycXz9Xwevedo6eiXR8QEhY8yrQrkNS4G1deY9tAD2Y");
const RENT_SYSVAR = address("SysvarRent111111111111111111111111111111111");
const LP_MINT = address("6tNheTBYSpQkfMLhcczKgmTLSGffK54npKMG1WQR2tvb");
const IDLE_ATA = address("6LATwaB4yRwGURCBDyFeJGqofaXxb6xXws9wBGbr3RBh");
const RECEIPT1 = address("3GHLmyTTGH9ZfQqb3YCo9xKjpPhMLvHsq2JSYzCnk9U6");
const CUSTODY1 = address("FTDWN5Ay8tzYPJBJT4s2oZaHRQ7jKPo8XP2ZRWb5GP3M");
const REPORT_TICKET = address("C71BFjq6PfgcWV4geoRudheupKnQBv6yN6uzYKthgAt5");
const REPORT_NAV_POLICY = address("41nzu42c3KPgJfWhnV5jbfxjHbvVU6HXaiJmzzYNqvBP");
/** Pending withdraw request receipt PDA: seeds { vault, userTransferAuthority: ADMIN }. */
const REQUEST_RECEIPT = address("8eufrxGC9Djf7ekcoWnyewKvYz4GgjtmLLpB8HBji99e");
/** Escrow LP ATA of REQUEST_RECEIPT; holds the entire LP supply pre-reset. */
const PENDING_ESCROW = address("C35aUCiMtQa7Zou8Jag5qRbMHqvnVarPwkSwYRgshcC1");
const ADMIN_LP_ATA = address("ZvsW29zAXZwMayzP9jAVBryRi5rt6X7em5vYdhKGbvZ");
const PROTOCOL_TREASURY = address("C7sE3MjSAqqF7TgXn1VsNQPWem1gdhqv3ZYV9TNfSjY9");
const SQUADS_VAULT = RWA_MULTIPLY_ROUTE.squads.vault; // ST999…  (also vault.manager)
const SQUADS_USDC_ATA = RWA_MULTIPLY_ROUTE.squads.assetAta; // EBG2…
const DELEGATED_EXECUTOR = RWA_MULTIPLY_ROUTE.squads.delegatedExecutor; // 62JL…
const USDC = RWA_MULTIPLY_ROUTE.assets.assetMint;
const TOKEN_PROGRAM = RWA_MULTIPLY_ROUTE.assets.tokenProgram;
const ATOKEN_PROGRAM = RWA_MULTIPLY_ROUTE.assets.associatedTokenProgram;
const IDLE_AUTH = address("EoHz6FHTL34F6HjuJmb5EceaRqxRG1RMYwYWKtWkGBFb");
const LP_MINT_AUTH = address("HHM86gQUM7rN8bz2VPWhqNn7ZcfznLC5KyT7kLs569yq");
const SYS_PROGRAM = address("11111111111111111111111111111111");

/** idle + receipt1 − tv. Never crank receipt1 below this (audit §6 underflow gate). */
const PHANTOM_NAV_RAW = 3_793_536n;
const REQUEST_WAITING_PERIOD_SECONDS = 600n;
const REPAIR_POLICY_SEED_MIN = 0n;
const REPAIR_POLICY_SEED_MAX = 255n;
const REPAIR_POLICY_SEED_BATCH_SIZE = 64;
const REPAIR_POLICY_LEGACY_RANGES = ["62-65"] as const;
const REPAIR_POLICY_SCHEMA = "loyal-voltr-hxtk-repair-policy/v1";
const REPAIR_EXECUTION_SCHEMA = "loyal-voltr-hxtk-repair-execution/v1";
const REPAIR_POLICY_REMOVE_SCHEMA = "loyal-voltr-hxtk-repair-policy-remove/v1";
const REPAIR_POLICY_OPERATION = "nav-refresh" as const;
const REPORT_DIGEST = new Uint8Array(32).fill(7);
const REPAIRED_BOOK_RAW = 3_793_417n;
/**
 * HXtk reset proof pins from audit §7 line 90 and the
 * voltr-reset-litesvm-2026-09-08 evidence `cancel` block. These values are
 * valid only for the exact frozen pre-cancel state below.
 */
export const CANCEL_EXPECTED_REFUND_LP = 78_196_265n;
export const CANCEL_EXPECTED_BURN_LP = 21_745_257n;
export const CANCEL_EXPECTED_SUPPLY_AFTER = 167_797_415n;
export const REQUEST_EXPECTED_LP = CANCEL_EXPECTED_SUPPLY_AFTER;
export const CLAIM_EXPECTED_PAYOUT_RAW = 3_793_394n;
export const CLAIM_EXPECTED_RESIDUAL_RAW = 23n;
export const HXTK_RESET_PROOF_PRESTATE = {
  amountLpEscrowed: 99_941_522n,
  amountAssetToWithdrawRaw: 1_767_782n,
  totalValue: REPAIRED_BOOK_RAW,
  idleBalance: REPAIRED_BOOK_RAW,
  lpSupply: 189_542_672n,
  adminLpBalance: 89_601_150n,
  receipt1PositionValue: PHANTOM_NAV_RAW,
} as const;
/** Keep eight slots of headroom inside the adaptor's 32-slot ReportSlot bound. */
export const REPORT_AGE_MARGIN_SLOTS = 8;
/**
 * Hard-pinned to crates/loyal-voltr-rwa-nav-adaptor/src/config.rs:307. The
 * deployed config and LiteSVM reset evidence both prove maxReportAgeSlots=32.
 */
export const ADAPTOR_MAX_REPORT_AGE_SLOTS = 32;
const EXECUTION_COMPILER_BIN = "compile-voltr-custom-execution";
const POLICY_JOURNAL_FLAG = "--policy-journal";
const REPAIR_JOURNAL_FLAG = "--repair-journal";
const CANCEL_JOURNAL_FLAG = "--cancel-journal";
const REQUEST_JOURNAL_FLAG = "--request-journal";
const CLAIM_JOURNAL_FLAG = "--claim-journal";
const REPAIR_POLICY_EXPECTED_SEED = 140n;
const REPAIR_POLICY_EXPECTED_SETTINGS_SEED = 139n;
const REPAIR_POLICY_EXPECTED_PDA = address("7vqKymJ4RcP9TUR9jT6G2ruuRp3j6rVhTzoYJWYTe2dR");
const REPAIR_POLICY_CREATE_DATA_BYTES = 837;
const REPAIR_POLICY_CREATE_DATA_SHA256 = "796624dfef068d71db889913de3527f36c23e19e11aafc9e370f021b649f153e";
const RESTORED_DEGRADATION_SECONDS = 86_400n;
const POLICY_PROVENANCE_STATEMENT =
  "Live policy bytes are compared to the hash recorded in the finalized PolicyCreate journal as a dynamic continuity pin, alongside decoded semantic checks.";
const HXTK_STATE_SCHEMA = "loyal-voltr-hxtk-reset-state/v2";
const REPEATABLE_LEGS = new Set(["config", "harvest", "restore-degradation"]);

type ParsedHxtkCli = ReturnType<typeof parseHxtkCli>;
let activeCli: ParsedHxtkCli | null = null;

function currentCli(): ParsedHxtkCli {
  return activeCli ?? parseHxtkCli([]);
}

const REPAIR_FROZEN = {
  totalValue: 2_793_298n,
  idleBalance: 3_793_417n,
  receipt1PositionValue: 2_793_417n,
  custody1Balance: 0n,
  lpSupply: 99_941_522n,
  degradation: 0n,
  adminPerformanceFeeBps: 0,
  waitingPeriod: REQUEST_WAITING_PERIOD_SECONDS,
  requestReceipt: REQUEST_RECEIPT,
  requestEscrowLpBalance: 99_941_522n,
  ticketLastConsumedSequence: 444_157_930n,
} as const;

// ---- tiny JSON-RPC layer (read + simulate only, no key material) -------------

const DEFAULT_RPC_URL = "https://api.mainnet-beta.solana.com";
const URL_PATTERN = /\bhttps?:\/\/[^\s"'`<>)}\]]+/gi;

function redactUrl(value: string): string {
  try {
    const parsed = new URL(value);
    return `${parsed.protocol}//${parsed.host}`;
  } catch {
    return value.replace(/([?#\/]).*$/, "");
  }
}

function sanitizeText(value: string): string {
  const configuredRpc = process.env.SOLANA_RPC_URL?.trim();
  const replaced = configuredRpc
    ? value.split(configuredRpc).join(redactUrl(configuredRpc))
    : value;
  return replaced.replace(URL_PATTERN, (candidate) => redactUrl(candidate));
}

function sanitizeError(error: unknown): string {
  return sanitizeText(error instanceof Error ? error.message : String(error));
}

let lastRpcAt = 0;
async function rpc<T = unknown>(method: string, params: unknown): Promise<T> {
  const url = process.env.SOLANA_RPC_URL?.trim() || DEFAULT_RPC_URL;
  const gap = 400 - (Date.now() - lastRpcAt);
  if (gap > 0) await new Promise((resolve_) => setTimeout(resolve_, gap));
  lastRpcAt = Date.now();
  const response = await fetch(url, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify({ jsonrpc: "2.0", id: 1, method, params }),
  });
  const json = await response.json() as { result?: T; error?: unknown };
  if (json.error !== undefined) throw new Error(sanitizeText(`rpc ${method}: ${JSON.stringify(json.error)}`));
  return json.result as T;
}

async function rpcWithRetry<T>(method: string, params: unknown, tries = 5): Promise<T> {
  let lastError: unknown = null;
  for (let attempt = 0; attempt < tries; attempt += 1) {
    try {
      return await rpc<T>(method, params);
    } catch (error) {
      lastError = error;
      if (attempt === tries - 1) break;
      await new Promise((resolve_) => setTimeout(resolve_, 800 * (attempt + 1)));
    }
  }
  throw new Error(sanitizeError(lastError));
}

async function readConfirmedSlot(): Promise<number> {
  return rpcWithRetry<number>("getSlot", [{ commitment: "confirmed" }]);
}

// ---- account decoding --------------------------------------------------------

type RawAccount = Readonly<{
  address: Address;
  owner: string;
  lamports: number;
  data: Uint8Array;
} | null>;

type Commitment = "confirmed" | "finalized";

export type ReportSlotAge = Readonly<{
  observedSlot: number;
  currentSlot: number;
  ageSlots: number;
  marginSlots: number;
  maxAgeSlots: number;
}>;

export class ReportSlotStaleError extends Error {
  readonly verdict: "REPORT_SLOT_STALE_PRE_SIMULATE" | "REPORT_SLOT_STALE_PRE_SEND";
  readonly age: ReportSlotAge;

  constructor(
    verdict: "REPORT_SLOT_STALE_PRE_SIMULATE" | "REPORT_SLOT_STALE_PRE_SEND",
    age: ReportSlotAge,
  ) {
    super(
      `${verdict}: observedSlot=${age.observedSlot} currentSlot=${age.currentSlot} `
      + `reportSlotAge=${age.ageSlots} margin=${age.marginSlots} maxAge=${age.maxAgeSlots}; `
      + "wait for a fresher confirmed slot and rerun the same repair leg",
    );
    this.name = "ReportSlotStaleError";
    this.verdict = verdict;
    this.age = age;
  }
}

export function reportSlotAge(observedSlot: bigint | number, currentSlot: number): ReportSlotAge {
  const observed = typeof observedSlot === "bigint" ? Number(observedSlot) : observedSlot;
  if (!Number.isSafeInteger(observed) || observed < 0) {
    throw new Error(`report observed slot ${String(observedSlot)} is not a safe non-negative integer`);
  }
  if (!Number.isSafeInteger(currentSlot) || currentSlot < 0) {
    throw new Error(`current confirmed slot ${currentSlot} is not a safe non-negative integer`);
  }
  return {
    observedSlot: observed,
    currentSlot,
    ageSlots: currentSlot - observed,
    marginSlots: REPORT_AGE_MARGIN_SLOTS,
    maxAgeSlots: ADAPTOR_MAX_REPORT_AGE_SLOTS,
  };
}

export function assertReportSlotFresh(
  observedSlot: bigint | number,
  currentSlot: number,
  phase: "pre-simulate" | "pre-send",
): ReportSlotAge {
  const age = reportSlotAge(observedSlot, currentSlot);
  const verdict = phase === "pre-send"
    ? "REPORT_SLOT_STALE_PRE_SEND"
    : "REPORT_SLOT_STALE_PRE_SIMULATE";
  if (age.currentSlot < age.observedSlot
    || age.ageSlots + age.marginSlots > age.maxAgeSlots) {
    throw new ReportSlotStaleError(verdict, age);
  }
  return age;
}

type RpcAccountValue = Readonly<{
  owner: string;
  lamports: number;
  data: readonly [string, string];
} | null>;

async function getAccount(pubkey: Address, commitment: Commitment = "confirmed"): Promise<RawAccount> {
  const result = await rpcWithRetry<{
    value: RpcAccountValue;
  }>("getAccountInfo", [pubkey, { encoding: "base64", commitment }]);
  const value = result?.value ?? null;
  if (!value) return null;
  if (value.data[1] !== "base64") throw new Error(`unexpected encoding for ${pubkey}`);
  return {
    address: pubkey,
    owner: value.owner,
    lamports: value.lamports,
    data: new Uint8Array(Buffer.from(value.data[0], "base64")),
  };
}

function decodeRpcAccount(addressValue: Address, value: RpcAccountValue): RawAccount {
  if (!value) return null;
  if (value.data[1] !== "base64") throw new Error(`unexpected encoding for ${addressValue}`);
  return {
    address: addressValue,
    owner: value.owner,
    lamports: value.lamports,
    data: new Uint8Array(Buffer.from(value.data[0], "base64")),
  };
}

function u64Le(bytes: Uint8Array, offset: number): bigint {
  let out = 0n;
  for (let index = 7; index >= 0; index -= 1) out = (out << 8n) | BigInt(bytes[offset + index] ?? 0);
  return out;
}

function u128Le(bytes: Uint8Array, offset: number): bigint {
  let out = 0n;
  for (let index = 15; index >= 0; index -= 1) out = (out << 8n) | BigInt(bytes[offset + index] ?? 0);
  return out;
}

/** SPL Token standard layout: account amount at 64. */
function tokenAmount(account: RawAccount): bigint | null {
  return account ? u64Le(account.data, 64) : null;
}

/** SPL Mint standard layout: supply at 36. */
function mintSupply(account: RawAccount): bigint | null {
  return account ? u64Le(account.data, 36) : null;
}

async function associatedToken(owner: Address, mint: Address): Promise<Address> {
  const [ata] = await findAssociatedTokenPda(
    { owner, mint, tokenProgram: TOKEN_PROGRAM },
    { programAddress: ATOKEN_PROGRAM },
  );
  return address(ata);
}

// ---- Voltr decoders ----------------------------------------------------------

function decodeVault(account: RawAccount) {
  if (!account) return null;
  const vault = getVaultDecoder().decode(account.data);
  return {
    manager: vault.manager,
    admin: vault.admin,
    pendingAdmin: vault.pendingAdmin,
    allowAnyAdaptor: vault.allowAnyAdaptor,
    totalValue: vault.asset.totalValue,
    idleAta: vault.asset.idleAta,
    lpMint: vault.lp.mint,
    maxCap: vault.vaultConfiguration.maxCap,
    lockedProfitDegradationDuration: vault.vaultConfiguration.lockedProfitDegradationDuration,
    withdrawalWaitingPeriod: vault.vaultConfiguration.withdrawalWaitingPeriod,
    managerPerformanceFeeBps: vault.feeConfiguration.managerPerformanceFee,
    adminPerformanceFeeBps: vault.feeConfiguration.adminPerformanceFee,
    accumulatedLpManagerFees: vault.feeState.accumulatedLpManagerFees,
    accumulatedLpAdminFees: vault.feeState.accumulatedLpAdminFees,
    accumulatedLpProtocolFees: vault.feeState.accumulatedLpProtocolFees,
    deadWeight: vault.deadWeight,
    highWaterMarkBits: vault.highWaterMark.highestAssetPerLpDecimalBits,
    lastUpdatedLockedProfit: vault.lockedProfitState.lastUpdatedLockedProfit,
    lockedProfitLastReport: vault.lockedProfitState.lastReport,
    version: vault.version,
  };
}

function decodeStrategyReceipt(account: RawAccount) {
  if (!account) return null;
  const receipt = getStrategyInitReceiptDecoder().decode(account.data);
  return {
    vault: receipt.vault,
    strategy: receipt.strategy,
    adaptorProgram: receipt.adaptorProgram,
    positionValue: receipt.positionValue,
    /** Post-upgrade the binary tracks the custody balance here (audit §1.3). */
    custodyTrackedRaw: u64Le(account.data, 128),
  };
}

/** Report ticket of the custom adaptor (96 bytes, report_ticket.go). */
function decodeReportTicket(account: RawAccount) {
  if (!account) return null;
  const data = account.data;
  return {
    version: data[8],
    bump: data[9],
    armed: data[10] === 1,
    strategy: bs58.encode(data.subarray(16, 48)),
    lastConsumedSequence: u64Le(data, 48),
    activeSequence: u64Le(data, 56),
    activeHashIsZero: data.subarray(64, 96).every((byte) => byte === 0),
  };
}

type SquadsPolicyState = Readonly<{
  settings: PublicKey;
  seed: { toString(): string };
  threshold: number;
  timeLock: number;
  signers: readonly Readonly<{
    key: PublicKey;
    permissions: Readonly<{ mask: number }>;
  }>[];
  policyState: Readonly<{ __kind: string; fields?: readonly unknown[] }>;
}>;

const SquadsPolicy = (squadsGenerated as unknown as {
  Policy: {
    fromAccountInfo(account: Readonly<{
      owner: PublicKey;
      lamports: number;
      executable: boolean;
      rentEpoch: number;
      data: Buffer;
    }>): readonly [SquadsPolicyState, number];
  };
}).Policy;

type SquadsProgramInteraction = Readonly<{
  accountIndex?: number;
  preHook?: unknown;
  postHook?: unknown;
  spendingLimits?: readonly unknown[];
  instructionsConstraints?: readonly Readonly<{
    programId: PublicKey;
    accountConstraints?: readonly unknown[];
    dataConstraints?: readonly unknown[];
  }>[];
}>;

function policyPda(seed: bigint): Address {
  if (seed < 0n || seed > (1n << 64n) - 1n) {
    throw new Error(`policy seed ${seed} is outside u64`);
  }
  const seedBytes = Buffer.alloc(8);
  seedBytes.writeBigUInt64LE(seed);
  const [pda] = PublicKey.findProgramAddressSync([
    Buffer.from("smart_account"),
    Buffer.from("policy"),
    new PublicKey(SQUADS_SETTINGS).toBuffer(),
    seedBytes,
  ], new PublicKey(SQUADS_PROGRAM));
  return address(pda.toBase58());
}

function dataValue(value: Readonly<{ __kind?: unknown; fields?: readonly unknown[] }>) {
  const kind = String(value.__kind ?? "");
  const raw = value.fields?.[0];
  if (kind === "U8Slice") {
    const bytes = raw instanceof Uint8Array
      ? raw
      : raw && typeof raw === "object"
        ? Uint8Array.from(Object.values(raw as Record<string, number>))
        : new Uint8Array();
    return { kind, value: Buffer.from(bytes).toString("hex") };
  }
  return {
    kind,
    value: typeof raw === "number"
      ? String(raw)
      : String((raw as { toString?: () => string })?.toString?.() ?? raw),
  };
}

type StableDecodedConstraint = Readonly<{
  index: number | null;
  kind: string;
  keys: readonly string[];
} | {
  offset: string;
  operator: string;
  kind: string;
  value: string;
}>;

type StableDecodedPolicy = Readonly<{
  constraints: readonly Readonly<{
    index: number;
    programId: string;
    accountConstraints: readonly StableDecodedConstraint[];
    dataConstraints: readonly StableDecodedConstraint[];
  }>[];
}>;

function decodedConstraint(value: unknown): StableDecodedConstraint {
  const constraint = value as {
    accountIndex?: number;
    accountConstraint?: { __kind?: unknown; fields?: readonly unknown[] };
    dataOffset?: { toString(): string } | number;
    dataValue?: { __kind?: unknown; fields?: readonly unknown[] };
    operator?: { __kind?: unknown } | number;
  };
  if (constraint.accountConstraint) {
    const keys = constraint.accountConstraint.fields?.[0] as readonly PublicKey[] | undefined;
    return {
      index: constraint.accountIndex ?? null,
      kind: String(constraint.accountConstraint.__kind ?? ""),
      keys: keys?.map((key) => key.toBase58()) ?? [],
    };
  }
  return {
    offset: String((constraint.dataOffset as { toString?: () => string })?.toString?.() ?? constraint.dataOffset),
    operator: typeof constraint.operator === "number"
      ? (["Equals", "NotEquals", "GreaterThan", "GreaterThanOrEqualTo", "LessThan", "LessThanOrEqualTo"] as const)[constraint.operator]
        ?? `Unknown(${constraint.operator})`
      : String(constraint.operator?.__kind ?? ""),
    ...dataValue(constraint.dataValue ?? {}),
  };
}

function decodeSquadsPolicy(account: RawAccount) {
  if (!account) return null;
  if (account.owner !== SQUADS_PROGRAM) throw new Error(`policy ${account.address} has owner ${account.owner}`);
  const [policy] = SquadsPolicy.fromAccountInfo({
    owner: new PublicKey(account.owner),
    lamports: account.lamports,
    executable: false,
    rentEpoch: 0,
    data: Buffer.from(account.data),
  });
  const body = policy.policyState.fields?.[0] as SquadsProgramInteraction | undefined;
  return {
    settings: policy.settings.toBase58(),
    seed: policy.seed.toString(),
    threshold: policy.threshold,
    timeLock: policy.timeLock,
    signers: policy.signers.map((signer) => ({
      address: signer.key.toBase58(),
      permissionsMask: signer.permissions.mask,
    })),
    policyState: policy.policyState.__kind,
    accountIndex: body?.accountIndex ?? null,
    preHook: body?.preHook ?? null,
    postHook: body?.postHook ?? null,
    spendingLimitCount: body?.spendingLimits?.length ?? 0,
    constraints: body?.instructionsConstraints?.map((constraint, index) => ({
      index,
      programId: constraint.programId.toBase58(),
      accountConstraints: constraint.accountConstraints?.map(decodedConstraint) ?? [],
      dataConstraints: constraint.dataConstraints?.map(decodedConstraint) ?? [],
    })) ?? [],
  };
}

function decodeSquadsSettingsPolicySeed(account: RawAccount): string | null {
  if (!account || account.owner !== SQUADS_PROGRAM) return null;
  const Settings = (squadsGenerated as unknown as {
    Settings: {
      fromAccountInfo(info: Readonly<{
        owner: PublicKey;
        lamports: number;
        executable: boolean;
        rentEpoch: number;
        data: Buffer;
      }>): readonly [{ policySeed?: { toString(): string } | null }, number];
    };
  }).Settings;
  const [settings] = Settings.fromAccountInfo({
    owner: new PublicKey(account.owner),
    lamports: account.lamports,
    executable: false,
    rentEpoch: 0,
    data: Buffer.from(account.data),
  });
  return settings.policySeed?.toString() ?? null;
}

function stablePolicyConstraints(decoded: StableDecodedPolicy | null) {
  return (decoded?.constraints ?? []).map((constraint) => ({
    index: constraint.index,
    programId: constraint.programId,
    pinnedPubkeys: constraint.accountConstraints
      .filter((accountConstraint): accountConstraint is { index: number | null; kind: string; keys: readonly string[] } => "keys" in accountConstraint)
      .flatMap((accountConstraint) => accountConstraint.keys),
    accountConstraints: constraint.accountConstraints
      .filter((accountConstraint): accountConstraint is { index: number | null; kind: string; keys: readonly string[] } => "keys" in accountConstraint)
      .map((accountConstraint) => ({
      index: accountConstraint.index,
      kind: accountConstraint.kind,
      pinnedPubkeys: accountConstraint.keys,
      })),
    dataConstraints: constraint.dataConstraints
      .filter((dataConstraint): dataConstraint is {
        offset: string;
        operator: string;
        kind: string;
        value: string;
      } => "offset" in dataConstraint)
      .map(({ offset, operator, kind, value }) => ({ offset, operator, kind, value })),
  }));
}

function decodePolicyCreateWire(instruction: Instruction) {
  const SyncSettingsArgs = (squadsGenerated as unknown as {
    syncSettingsTransactionArgsBeet: {
      deserialize(data: Buffer): readonly [{
        numSigners: number;
        actions: readonly unknown[];
        memo: string | null;
      }, number];
    };
  }).syncSettingsTransactionArgsBeet;
  const [args] = SyncSettingsArgs.deserialize(Buffer.from(instruction.data ?? []).subarray(8));
  const action = args.actions[0] as {
    __kind?: unknown;
    seed?: unknown;
    signers?: readonly unknown[];
    threshold?: unknown;
    timeLock?: unknown;
    policyCreationPayload?: {
      __kind?: unknown;
      fields?: readonly unknown[];
    };
  } | undefined;
  const payload = action?.policyCreationPayload;
  const body = payload?.__kind === "ProgramInteraction"
    ? payload.fields?.[0] as {
        accountIndex?: unknown;
        instructionsConstraints?: readonly unknown[];
        preHook?: unknown;
        postHook?: unknown;
        spendingLimits?: readonly unknown[];
      } | undefined
    : undefined;
  return {
    seed: action?.seed === undefined || action.seed === null
      ? null
      : String((action.seed as { toString?: () => string }).toString?.() ?? action.seed),
    policyState: String(payload?.__kind ?? ""),
    threshold: typeof action?.threshold === "number"
      ? action.threshold
      : typeof action?.threshold === "bigint"
        ? Number(action.threshold)
        : null,
    timeLock: typeof action?.timeLock === "number"
      ? action.timeLock
      : typeof action?.timeLock === "bigint"
        ? Number(action.timeLock)
        : null,
    signers: action?.signers?.map((signer) => {
      const decoded = signer as {
        key?: PublicKey;
        permissions?: { mask?: number };
      };
      return {
        address: decoded.key?.toBase58() ?? "",
        permissionsMask: decoded.permissions?.mask ?? null,
      };
    }) ?? [],
    accountIndex: typeof body?.accountIndex === "number" ? body.accountIndex : null,
    preHook: body?.preHook ?? null,
    postHook: body?.postHook ?? null,
    spendingLimitCount: body?.spendingLimits?.length ?? 0,
    constraints: body?.instructionsConstraints?.map((constraint, index) => {
      const decoded = constraint as {
        programId?: PublicKey;
        accountConstraints?: readonly unknown[];
        dataConstraints?: readonly unknown[];
      };
      return {
        index,
        programId: decoded.programId?.toBase58() ?? "",
        accountConstraints: decoded.accountConstraints?.map(decodedConstraint) ?? [],
        dataConstraints: decoded.dataConstraints?.map(decodedConstraint) ?? [],
      };
    }) ?? [],
  };
}

type RepairPolicySeedRow = Readonly<{
  seed: string;
  policy: Address;
  present: boolean;
  owner: string | null;
  dataBytes: number;
  dataSha256: string | null;
}>;

function seedRanges(seeds: readonly bigint[]): readonly string[] {
  const ordered = [...seeds].sort((left, right) => (left < right ? -1 : left > right ? 1 : 0));
  const ranges: string[] = [];
  let start: bigint | null = null;
  let previous: bigint | null = null;
  for (const seed of ordered) {
    if (start === null) {
      start = seed;
      previous = seed;
      continue;
    }
    if (seed === previous! + 1n) {
      previous = seed;
      continue;
    }
    ranges.push(start === previous ? start.toString() : `${start}-${previous}`);
    start = seed;
    previous = seed;
  }
  if (start !== null) {
    ranges.push(start === previous ? start.toString() : `${start}-${previous}`);
  }
  return ranges;
}

async function readRepairPolicySeeds(): Promise<Readonly<{
  contextSlot: number;
  contextSlots: readonly number[];
  scan: Readonly<{
    minSeed: string;
    maxSeed: string;
    batchSize: number;
    batchCount: number;
    legacyPolicyRanges: readonly string[];
    seedRule: string;
  }>;
  occupiedCount: number;
  occupiedSeeds: readonly string[];
  occupiedRanges: readonly string[];
  rows: readonly RepairPolicySeedRow[];
}>> {
  const seeds = Array.from(
    { length: Number(REPAIR_POLICY_SEED_MAX - REPAIR_POLICY_SEED_MIN + 1n) },
    (_unused, index) => REPAIR_POLICY_SEED_MIN + BigInt(index),
  );
  const rows: RepairPolicySeedRow[] = [];
  const contextSlots: number[] = [];
  for (let offset = 0; offset < seeds.length; offset += REPAIR_POLICY_SEED_BATCH_SIZE) {
    const batchSeeds = seeds.slice(offset, offset + REPAIR_POLICY_SEED_BATCH_SIZE);
    const policyAddresses = batchSeeds.map(policyPda);
    const response = await rpcWithRetry<{
      context: { slot: number };
      value: readonly RpcAccountValue[];
    }>("getMultipleAccounts", [
      policyAddresses,
      { encoding: "base64", commitment: "finalized" },
    ]);
    if (response.value.length !== policyAddresses.length) {
      throw new Error("finalized repair-policy seed readback omitted an account slot");
    }
    contextSlots.push(response.context.slot);
    rows.push(...policyAddresses.map((policy, index) => {
      const account = decodeRpcAccount(policy, response.value[index] ?? null);
      return {
        seed: batchSeeds[index]!.toString(),
        policy,
        present: account !== null,
        owner: account?.owner ?? null,
        dataBytes: account?.data.length ?? 0,
        dataSha256: account ? createHash("sha256").update(account.data).digest("hex") : null,
      } satisfies RepairPolicySeedRow;
    }));
  }
  const occupied = rows.filter((row) => row.present).map((row) => BigInt(row.seed));
  return {
    contextSlot: Math.max(...contextSlots),
    contextSlots,
    scan: {
      minSeed: REPAIR_POLICY_SEED_MIN.toString(),
      maxSeed: REPAIR_POLICY_SEED_MAX.toString(),
      batchSize: REPAIR_POLICY_SEED_BATCH_SIZE,
      batchCount: contextSlots.length,
      legacyPolicyRanges: REPAIR_POLICY_LEGACY_RANGES,
      seedRule: "PolicyCreate derives Settings.policySeed + 1; an arbitrary action seed is ignored by the deployed program",
    },
    occupiedCount: occupied.length,
    occupiedSeeds: occupied.map((seed) => seed.toString()),
    occupiedRanges: seedRanges(occupied),
    rows,
  };
}

type SettingsPolicyCounter = Readonly<{
  account: Exclude<RawAccount, null>;
  policySeed: bigint;
  expectedSeed: bigint;
}>;

async function readSettingsPolicyCounter(): Promise<SettingsPolicyCounter> {
  const account = await getAccount(SQUADS_SETTINGS, "finalized");
  const policySeedText = decodeSquadsSettingsPolicySeed(account);
  if (!account || policySeedText === null) {
    throw new Error("finalized Squads Settings is absent or has no policy seed");
  }
  const policySeed = BigInt(policySeedText);
  if (policySeed >= (1n << 64n) - 1n) {
    throw new Error("finalized Squads Settings policy seed cannot advance");
  }
  return { account, policySeed, expectedSeed: policySeed + 1n };
}

function settingsPolicyReadback(counter: SettingsPolicyCounter) {
  return {
    commitment: "finalized" as const,
    policySeed: counter.policySeed.toString(),
    expectedSeed: counter.expectedSeed.toString(),
    seedRule: "PolicyCreate derives Settings.policySeed + 1; an arbitrary action seed is ignored by the deployed program",
  };
}

// ---- live state --------------------------------------------------------------

type LiveState = Awaited<ReturnType<typeof readState>>;

async function readState(commitment: Commitment = "confirmed", atomic = false) {
  const [managerLpAta, treasuryLpAta, adminUsdcAta] = await Promise.all([
    associatedToken(SQUADS_VAULT, LP_MINT),
    associatedToken(PROTOCOL_TREASURY, LP_MINT),
    associatedToken(ADMIN, USDC),
  ]);
  // SDK seeds: { vault, userTransferAuthority } — the pending request's authority
  // is the admin hot key, so the PDA must land on REQUEST_RECEIPT (8eufrx…).
  const derivedRequestReceipt = (await findRequestWithdrawVaultReceiptPda({
    vault: VAULT,
    userTransferAuthority: ADMIN,
  }))[0];
  if (derivedRequestReceipt !== REQUEST_RECEIPT) {
    throw new Error(
      `request receipt PDA ${derivedRequestReceipt} does not match recorded ${REQUEST_RECEIPT}`,
    );
  }
  const [escrowAta] = await Promise.all([
    associatedToken(REQUEST_RECEIPT, LP_MINT),
  ]);
  if (escrowAta !== PENDING_ESCROW) {
    throw new Error(`request escrow ATA ${escrowAta} does not match recorded ${PENDING_ESCROW}`);
  }

  const addresses = [
    VAULT, IDLE_ATA, LP_MINT, RECEIPT1, CUSTODY1, REPORT_TICKET,
    PENDING_ESCROW, ADMIN_LP_ATA, managerLpAta, treasuryLpAta, adminUsdcAta,
    REQUEST_RECEIPT, SQUADS_USDC_ATA,
  ] as const;
  const names = [
    "vault", "idle", "lpMint", "receipt1", "custody1", "ticket",
    "pendingEscrow", "adminLp", "managerLp", "treasuryLp", "adminUsdc",
    "requestReceipt", "squadsUsdc",
  ] as const;
  const accounts = {} as Record<typeof names[number], RawAccount>;
  let contextSlot: number;
  let epoch: number | null;
  if (atomic) {
    const response = await rpcWithRetry<{
      context: { slot: number };
      value: readonly RpcAccountValue[];
    }>("getMultipleAccounts", [addresses, { encoding: "base64", commitment }]);
    if (response.value.length !== addresses.length) {
      throw new Error(`atomic ${commitment} snapshot returned ${response.value.length} accounts; expected ${addresses.length}`);
    }
    for (let index = 0; index < addresses.length; index += 1) {
      accounts[names[index]!] = decodeRpcAccount(addresses[index]!, response.value[index] ?? null);
    }
    contextSlot = response.context.slot;
    epoch = null;
  } else {
    for (let index = 0; index < addresses.length; index += 1) {
      accounts[names[index]!] = await getAccount(addresses[index]!, commitment);
    }
    const epochInfo = await rpcWithRetry<{ absoluteSlot: number; epoch: number }>("getEpochInfo", [{ commitment }]);
    contextSlot = epochInfo.absoluteSlot;
    epoch = epochInfo.epoch;
  }
  return {
    contextSlot,
    epoch,
    identity: {
      managerLpAta,
      treasuryLpAta,
      requestReceiptAddress: REQUEST_RECEIPT,
      requestReceiptPdaSeeds: { vault: VAULT, userTransferAuthority: ADMIN },
      escrowAta: PENDING_ESCROW,
      adminUsdcAta,
    },
    vault: decodeVault(accounts.vault),
    idleBalance: tokenAmount(accounts.idle),
    lpSupply: mintSupply(accounts.lpMint),
    receipt1: decodeStrategyReceipt(accounts.receipt1),
    custody1Balance: tokenAmount(accounts.custody1),
    reportTicket: decodeReportTicket(accounts.ticket),
    requestEscrowLpBalance: tokenAmount(accounts.pendingEscrow),
    adminLpBalance: tokenAmount(accounts.adminLp),
    managerLpBalance: tokenAmount(accounts.managerLp),
    treasuryLpBalance: tokenAmount(accounts.treasuryLp),
    adminUsdcBalance: tokenAmount(accounts.adminUsdc),
    requestReceipt: accounts.requestReceipt
      ? {
          vault: bs58.encode(accounts.requestReceipt.data.subarray(8, 40)),
          userTransferAuthority: bs58.encode(accounts.requestReceipt.data.subarray(40, 72)),
          amountLpEscrowed: u64Le(accounts.requestReceipt.data, 72),
          amountAssetBits: u128Le(accounts.requestReceipt.data, 80),
          amountAssetToWithdrawRaw: u128Le(accounts.requestReceipt.data, 80) >> 48n,
          withdrawableFromTs: u64Le(accounts.requestReceipt.data, 96),
          bump: accounts.requestReceipt.data[104] ?? null,
        }
      : null,
    squadsUsdcBalance: tokenAmount(accounts.squadsUsdc),
  };
}

/** Parked value no instruction can remove (audit §6 D invariant, evidence-file sign). */
function conservedGap(state: LiveState): bigint | null {
  if (!state.vault || state.idleBalance === null || state.receipt1 === null) return null;
  return state.idleBalance + state.receipt1.positionValue - state.vault.totalValue;
}

function summarize(state: LiveState) {
  const gap = conservedGap(state);
  return {
    contextSlot: state.contextSlot,
    epoch: state.epoch,
    totalValue: state.vault?.totalValue.toString() ?? null,
    idleBalance: state.idleBalance?.toString() ?? null,
    custody1Balance: state.custody1Balance?.toString() ?? null,
    receipt1PositionValue: state.receipt1?.positionValue.toString() ?? null,
    receipt1CustodyTracked: state.receipt1?.custodyTrackedRaw.toString() ?? null,
    lpSupply: state.lpSupply?.toString() ?? null,
    deadWeight: state.vault?.deadWeight.toString() ?? null,
    feeAccumulatedLpManager: state.vault?.accumulatedLpManagerFees.toString() ?? null,
    feeAccumulatedLpAdmin: state.vault?.accumulatedLpAdminFees.toString() ?? null,
    feeAccumulatedLpProtocol: state.vault?.accumulatedLpProtocolFees.toString() ?? null,
    lockedProfitDegradationDuration: state.vault?.lockedProfitDegradationDuration.toString() ?? null,
    withdrawalWaitingPeriod: state.vault?.withdrawalWaitingPeriod.toString() ?? null,
    adminPerformanceFeeBps: state.vault?.adminPerformanceFeeBps ?? null,
    managerPerformanceFeeBps: state.vault?.managerPerformanceFeeBps ?? null,
    maxCap: state.vault?.maxCap.toString() ?? null,
    lastUpdatedLockedProfit: state.vault?.lastUpdatedLockedProfit.toString() ?? null,
    lockedProfitLastReport: state.vault?.lockedProfitLastReport.toString() ?? null,
    highWaterMarkBits: state.vault?.highWaterMarkBits.toString() ?? null,
    managerLpBalance: state.managerLpBalance?.toString() ?? null,
    adminLpBalance: state.adminLpBalance?.toString() ?? null,
    treasuryLpBalance: state.treasuryLpBalance?.toString() ?? null,
    requestEscrowLpBalance: state.requestEscrowLpBalance?.toString() ?? null,
    adminUsdcBalance: state.adminUsdcBalance?.toString() ?? null,
    squadsUsdcBalance: state.squadsUsdcBalance?.toString() ?? null,
    requestReceipt: state.requestReceipt
      ? {
          vault: state.requestReceipt.vault,
          userTransferAuthority: state.requestReceipt.userTransferAuthority,
          amountLpEscrowed: state.requestReceipt.amountLpEscrowed.toString(),
          amountAssetBits: state.requestReceipt.amountAssetBits.toString(),
          amountAssetToWithdrawRaw: state.requestReceipt.amountAssetToWithdrawRaw.toString(),
          withdrawableFromTs: state.requestReceipt.withdrawableFromTs.toString(),
          bump: state.requestReceipt.bump,
        }
      : null,
    reportTicket: state.reportTicket
      ? {
          version: state.reportTicket.version,
          armed: state.reportTicket.armed,
          lastConsumedSequence: state.reportTicket.lastConsumedSequence.toString(),
          activeSequence: state.reportTicket.activeSequence.toString(),
          activeHashIsZero: state.reportTicket.activeHashIsZero,
        }
      : null,
    conservedGap_idlePlusReceiptsMinusTv: gap === null ? null : gap.toString(),
    tvEqualsIdle: state.vault && state.idleBalance !== null
      ? state.vault.totalValue === state.idleBalance
      : null,
  };
}

/**
 * Fingerprint the repair account snapshot without its RPC context metadata.
 * The context slot is expected to advance between the initial read and the
 * pre-send read; every decoded account/state value must remain identical.
 */
function repairStateFingerprint(state: LiveState): string {
  const summary = summarize(state);
  return toJson({ ...summary, contextSlot: undefined, epoch: undefined });
}

// ---- event decoding ----------------------------------------------------------

const EVENT_DECODERS = {
  UpdateVaultConfig: getUpdateVaultConfigEventDecoder(),
  HarvestFee: getHarvestFeeEventDecoder(),
  CancelRequestWithdrawVault: getCancelRequestWithdrawVaultEventDecoder(),
  RequestWithdrawVault: getRequestWithdrawVaultEventDecoder(),
  WithdrawVault: getWithdrawVaultEventDecoder(),
} as const;

type EventName = keyof typeof EVENT_DECODERS;

/**
 * Anchor events arrive as `Program data: <base64(disc(8) + borsh)>`. The
 * deployed binary uses sha256("event:<Name>Event")[..8]; the value below was
 * confirmed byte-for-byte against the LiteSVM capture of the same instruction.
 */
function eventDiscriminator(name: EventName): Uint8Array {
  const suffix = name.endsWith("Event") ? name : `${name}Event`;
  return new Uint8Array(createHash("sha256").update(`event:${suffix}`).digest().subarray(0, 8));
}

function decodeEvents(name: EventName, logs: readonly string[]): unknown[] {
  const marker = "Program data: ";
  const prefix = Buffer.from(eventDiscriminator(name)).toString("base64").slice(0, 10);
  const decoder = EVENT_DECODERS[name];
  const found: unknown[] = [];
  for (const line of logs) {
    const at = line.indexOf(marker);
    if (at < 0) continue;
    const payload = line.slice(at + marker.length).trim();
    if (!payload.startsWith(prefix)) continue;
    const bytes = new Uint8Array(Buffer.from(payload, "base64"));
    try {
      found.push(decoder.decode(bytes.subarray(8)));
    } catch {
      // Truncated or foreign event; report it rather than guessing.
      found.push({ undecodable: payload });
    }
  }
  return found;
}

// ---- build + simulate --------------------------------------------------------

/**
 * Simulated wires are never sent. They are paid by the step's real fee payer
 * address (which must be funded, exactly as in the live transaction) but every
 * signature slot is zero-filled: legal here only because sigVerify is false.
 * No key material is read or required by this path.
 */
const PACKET_LIMIT = 1_232;
const MAX_SIMULATION_ACCOUNTS = 31;

type Simulation = Readonly<{
  err: unknown;
  logs: readonly string[];
  unitsConsumed: number | null;
  packetBytes: number;
  contextSlot: number | null;
  postAccounts: readonly RawAccount[];
}>;

async function simulate(
  feePayerAddress: Address,
  instructions: readonly Instruction[],
  watchedAddresses: readonly Address[],
): Promise<Simulation> {
  const distinct = [...new Set(watchedAddresses)].slice(0, MAX_SIMULATION_ACCOUNTS);
  const web3Instructions: TransactionInstruction[] = [
    fromWeb3Instruction(ComputeBudgetProgram.setComputeUnitLimit({ units: 400_000 })),
    ...instructions,
  ].map(toWeb3Instruction);
  const blockhash = await rpcWithRetry<{ value: { blockhash: string } }>("getLatestBlockhash", [
    { commitment: "confirmed" },
  ]);
  const message = new TransactionMessage({
    payerKey: new PublicKey(feePayerAddress),
    recentBlockhash: blockhash.value.blockhash,
    instructions: web3Instructions,
  }).compileToV0Message();
  const transaction = new VersionedTransaction(message);
  // Zero-filled signature slots: legal here only because sigVerify is false.
  transaction.signatures = Array.from(
    { length: message.header.numRequiredSignatures },
    () => new Uint8Array(64),
  );
  const wire = transaction.serialize();
  if (wire.length > PACKET_LIMIT) {
    throw new Error(`simulated packet is ${wire.length} bytes; limit ${PACKET_LIMIT}`);
  }
  const response = await rpcWithRetry<{
    context?: { slot: number };
    value: {
      err: unknown;
      logs: readonly string[] | null;
      unitsConsumed?: number;
      accounts?: readonly ({ owner: string; lamports: number; data: [string, string] } | null)[];
    };
  }>("simulateTransaction", [
    Buffer.from(wire).toString("base64"),
    {
      commitment: "confirmed",
      encoding: "base64",
      sigVerify: false,
      replaceRecentBlockhash: true,
      innerInstructions: true,
      accounts: { encoding: "base64", addresses: [...distinct] },
    },
  ]);
  const result = response?.value;
  return {
    err: result?.err ?? null,
    logs: result?.logs ?? [],
    unitsConsumed: result?.unitsConsumed ?? null,
    packetBytes: wire.length,
    contextSlot: response?.context?.slot ?? null,
    postAccounts: distinct.map((target, index) => {
      const value = result?.accounts?.[index];
      if (!value) return null;
      return {
        address: target,
        owner: value.owner,
        lamports: value.lamports,
        data: new Uint8Array(Buffer.from(value.data[0], "base64")),
      };
    }),
  };
}

function postAccount(postAccounts: readonly RawAccount[], target: Address): RawAccount {
  return postAccounts.find((account) => account?.address === target) ?? null;
}

// ---- output ------------------------------------------------------------------

/** JSON.stringify rejects BigInt; every decoded field is emitted as a string. */
function toJson(value: unknown, pretty = 0): string {
  const withRoot = value && typeof value === "object" && !Array.isArray(value)
    ? {
        canonicalStateRoot: canonicalStateRootForOutput(),
        ...(value as Record<string, unknown>),
      }
    : value;
  return JSON.stringify(withRoot, (_key, entry) =>
    typeof entry === "bigint"
      ? entry.toString()
      : typeof entry === "string"
        ? sanitizeText(entry)
        : entry, pretty) ?? "{}";
}

function canonicalStateRootForOutput(): string {
  return resolveCanonicalStateRoot({ vault: VAULT.toString(), create: false });
}

// ---- evidence ----------------------------------------------------------------

function writeEvidence(step: string, evidence: Record<string, unknown>): string {
  mkdirSync(EVIDENCE_DIR, { recursive: true });
  const path = resolve(EVIDENCE_DIR, `${step}.simulated.json`);
  // Simulation evidence is never a send authorization. Keep all three
  // lifecycle flags explicit and false even if a caller accidentally passes a
  // stale flag from an operator plan.
  writeFileSync(path, `${toJson({ schema: SCHEMA, step, ...evidence, sent: false, signed: false, broadcast: false }, 2)}\n`);
  return path;
}

// ---- one-shot repair policy -------------------------------------------------

type PolicyWireInstruction = CustomPolicyArtifact["policies"][number]["createInstruction"];

function wireInstruction(value: PolicyWireInstruction): Instruction {
  return {
    programAddress: address(value.programId),
    accounts: value.accounts.map((account) => ({
      address: address(account.address),
      role: account.signer
        ? account.writable ? AccountRole.WRITABLE_SIGNER : AccountRole.READONLY_SIGNER
        : account.writable ? AccountRole.WRITABLE : AccountRole.READONLY,
    })),
    data: new Uint8Array(Buffer.from(value.dataBase64, "base64")),
  };
}

function wireFromInstruction(instruction: Instruction): PolicyWireInstruction {
  return {
    programId: instruction.programAddress,
    accounts: (instruction.accounts ?? []).map((account) => ({
      address: account.address,
      signer: account.role === AccountRole.READONLY_SIGNER
        || account.role === AccountRole.WRITABLE_SIGNER,
      writable: account.role === AccountRole.WRITABLE
        || account.role === AccountRole.WRITABLE_SIGNER,
    })),
    dataBase64: Buffer.from(instruction.data ?? []).toString("base64"),
  };
}

function parseSeed(value: string, label: string): bigint {
  if (!/^[0-9]+$/.test(value)) throw new Error(`${label} must be a decimal u64 seed`);
  const seed = BigInt(value);
  if (seed < REPAIR_POLICY_SEED_MIN || seed > REPAIR_POLICY_SEED_MAX) {
    throw new Error(`${label} must be in the scanned one-shot repair range 0..255`);
  }
  return seed;
}

function cliValue(flag: string): string {
  return currentCli().value(flag) ?? "";
}

function assertNoArbitraryRepairSeed() {
  if (currentCli().has("--seed")) {
    throw new Error("--seed is not accepted; the HXtk repair policy is hard-bound to Settings seed 140");
  }
  const requested = cliValue("--expect-seed");
  if (requested.length > 0 && parseSeed(requested, "--expect-seed") !== REPAIR_POLICY_EXPECTED_SEED) {
    throw new Error(`--expect-seed ${requested} does not equal the hard-bound repair policy seed ${REPAIR_POLICY_EXPECTED_SEED}`);
  }
}

function assertRepairPolicySettingsCounter(counter: SettingsPolicyCounter, phase: string) {
  if (counter.policySeed !== REPAIR_POLICY_EXPECTED_SETTINGS_SEED
    || counter.expectedSeed !== REPAIR_POLICY_EXPECTED_SEED) {
    throw new Error(
      `${phase}: finalized Settings counter is ${counter.policySeed} (next ${counter.expectedSeed}); `
      + `expected policySeed ${REPAIR_POLICY_EXPECTED_SETTINGS_SEED} (next ${REPAIR_POLICY_EXPECTED_SEED})`,
    );
  }
}

function assertRepairPolicyHardBinding(seed: bigint, policy: Address, createData?: Uint8Array) {
  if (seed !== REPAIR_POLICY_EXPECTED_SEED) {
    throw new Error(`repair policy seed ${seed} is not the hard-bound seed ${REPAIR_POLICY_EXPECTED_SEED}`);
  }
  if (policy !== REPAIR_POLICY_EXPECTED_PDA || policyPda(seed) !== REPAIR_POLICY_EXPECTED_PDA) {
    throw new Error(`repair policy PDA ${policy} is not the hard-bound ${REPAIR_POLICY_EXPECTED_PDA}`);
  }
  if (createData !== undefined) {
    const dataSha256 = createHash("sha256").update(createData).digest("hex");
    if (createData.length !== REPAIR_POLICY_CREATE_DATA_BYTES
      || dataSha256 !== REPAIR_POLICY_CREATE_DATA_SHA256) {
      throw new Error(
        `PolicyCreate data ${createData.length} bytes/${dataSha256} does not match the reviewed `
        + `${REPAIR_POLICY_CREATE_DATA_BYTES} bytes/${REPAIR_POLICY_CREATE_DATA_SHA256}`,
      );
    }
  }
}

type RepairPolicyJournalRecord = Readonly<{
  schema?: unknown;
  verdict?: unknown;
  expectedSeed?: unknown;
  seed?: unknown;
  policy?: unknown;
  settingsReadback?: Readonly<{
    policySeed?: unknown;
    expectedSeed?: unknown;
  }>;
}>;

type RepairPolicyJournalTarget = Readonly<{
  path: string;
  expectedSeed: bigint;
  policy: Address;
}>;

function optionalPolicyJournalPath(): string | null {
  const raw = currentCli().value(POLICY_JOURNAL_FLAG);
  if (raw === undefined) return null;
  const path = resolve(raw);
  if (!path.endsWith(".json") || !existsSync(path)) {
    throw new Error(`${POLICY_JOURNAL_FLAG} requires an existing JSON journal`);
  }
  return path;
}

function optionalRepairJournalPath(): string | null {
  const raw = currentCli().value(REPAIR_JOURNAL_FLAG);
  if (raw === undefined) return null;
  const path = resolve(raw);
  if (!path.endsWith(".json") || !existsSync(path)) {
    throw new Error(`${REPAIR_JOURNAL_FLAG} requires an existing JSON journal`);
  }
  return path;
}

function requiredJournalPath(flag: string, description: string): string {
  const raw = currentCli().value(flag);
  if (raw === undefined) throw new Error(`${flag} is required: ${description}`);
  const path = resolve(raw);
  if (!path.endsWith(".json") || !existsSync(path)) {
    throw new Error(`${flag} requires an existing finalized JSON journal`);
  }
  return path;
}

async function readRepairConsumedJournal(
  rpcUrl: string,
): Promise<Readonly<{ path: string; seed: bigint; policy: Address }> | null> {
  const path = optionalRepairJournalPath();
  if (path === null) return null;
  const parsed = assertFinalizedJournalBound("repair", path).record;
  if (parsed.schema !== REPAIR_EXECUTION_SCHEMA || parsed.verdict !== "FINALIZED_RECONCILED"
    || parsed.sent !== true || parsed.signed !== true || parsed.broadcast !== true) {
    throw new Error(`${REPAIR_JOURNAL_FLAG} must be a finalized repair journal`);
  }
  if (typeof parsed.policyJournal !== "string" || parsed.policyJournal.length === 0) {
    throw new Error(`${REPAIR_JOURNAL_FLAG} must be policy-linked`);
  }
  const policyJournalPath = resolve(parsed.policyJournal);
  if (!existsSync(policyJournalPath)) {
    throw new Error(`${REPAIR_JOURNAL_FLAG} policy-linked PolicyCreate journal is absent`);
  }
  const policyJournal = assertFinalizedJournalBound("repair-policy", policyJournalPath).record;
  if (policyJournal.schema !== REPAIR_POLICY_SCHEMA || policyJournal.verdict !== "FINALIZED_RECONCILED"
    || policyJournal.sent !== true || policyJournal.signed !== true || policyJournal.broadcast !== true) {
    throw new Error(`${REPAIR_JOURNAL_FLAG} policy link is not a finalized PolicyCreate journal`);
  }
  const seed = parseSeed(String(parsed.seed ?? parsed.expectedSeed ?? ""), `${REPAIR_JOURNAL_FLAG} seed`);
  const policy = address(stringAt(parsed.policy, `${REPAIR_JOURNAL_FLAG}.policy`));
  assertRepairPolicyHardBinding(seed, policy);
  const linkedSeed = parseSeed(String(policyJournal.seed ?? policyJournal.expectedSeed ?? ""), `${REPAIR_JOURNAL_FLAG} linked policy seed`);
  const linkedPolicy = address(stringAt(policyJournal.policy, `${REPAIR_JOURNAL_FLAG} linked policy`));
  if (linkedSeed !== seed || linkedPolicy !== policy) {
    throw new Error(`${REPAIR_JOURNAL_FLAG} policy link does not match the repair seed/PDA`);
  }
  const finalized = await verifyFinalizedRepairJournal(rpcUrl, path, policyJournalPath);
  if (finalized.seed !== seed || finalized.policy !== policy) {
    throw new Error(`${REPAIR_JOURNAL_FLAG} finalized provenance does not match the repair journal`);
  }
  return { path, seed, policy };
}

async function assertRepairNotAlreadyApplied(state: LiveState, rpcUrl: string) {
  const consumed = await readRepairConsumedJournal(rpcUrl);
  if (state.vault?.totalValue === state.idleBalance) {
    throw new Error(
      `REPAIR_ALREADY_APPLIED: ${consumed === null ? "on-chain tv == idle" : `finalized repair journal ${consumed.path} is policy-linked and `}`
      + `on-chain tv ${state.vault?.totalValue} already equals idle ${state.idleBalance}`,
    );
  }
}

function readPolicyJournalTarget(): RepairPolicyJournalTarget | null {
  const path = optionalPolicyJournalPath();
  if (path === null) return null;
  const parsed = assertFinalizedJournalBound("repair-policy", path).record as RepairPolicyJournalRecord;
  if (parsed.schema !== REPAIR_POLICY_SCHEMA) {
    throw new Error(`${POLICY_JOURNAL_FLAG} must be the finalized repair-policy creation journal`);
  }
  if (parsed.verdict !== "FINALIZED_RECONCILED") {
    throw new Error(`${POLICY_JOURNAL_FLAG} must be the finalized repair-policy journal`);
  }
  const raw = parsed.expectedSeed ?? parsed.seed ?? parsed.settingsReadback?.expectedSeed;
  if (typeof raw !== "string") {
    throw new Error(`${POLICY_JOURNAL_FLAG} has no expectedSeed`);
  }
  const expectedSeed = parseSeed(raw, "repair-policy journal expectedSeed");
  assertRepairPolicyHardBinding(expectedSeed, address(String(parsed.policy ?? REPAIR_POLICY_EXPECTED_PDA)));
  if (parsed.seed !== undefined && String(parsed.seed) !== expectedSeed.toString()) {
    throw new Error(`${POLICY_JOURNAL_FLAG} seed differs from expectedSeed`);
  }
  if (parsed.policy !== undefined && String(parsed.policy) !== policyPda(expectedSeed)) {
    throw new Error(`${POLICY_JOURNAL_FLAG} policy PDA does not match expectedSeed`);
  }
  if (parsed.settingsReadback?.expectedSeed !== undefined
    && String(parsed.settingsReadback.expectedSeed) !== expectedSeed.toString()) {
    throw new Error(`${POLICY_JOURNAL_FLAG} Settings readback disagrees with expectedSeed`);
  }
  return {
    path,
    expectedSeed,
    policy: address(String(parsed.policy ?? REPAIR_POLICY_EXPECTED_PDA)),
  };
}

type RepairPolicyTarget = Readonly<{
  seed: bigint;
  source: "policy-journal";
  settings: SettingsPolicyCounter;
  policyJournal: string;
}>;

async function resolveRepairPolicyTarget(
  _readback: Awaited<ReturnType<typeof readRepairPolicySeeds>>,
): Promise<RepairPolicyTarget> {
  const settings = await readSettingsPolicyCounter();
  const journal = readPolicyJournalTarget();
  assertNoArbitraryRepairSeed();
  if (journal === null) {
    throw new Error(`${POLICY_JOURNAL_FLAG} is required; repair and PolicyRemove cannot use a journal-less policy target`);
  }
  return {
    seed: journal.expectedSeed,
    source: "policy-journal",
    settings,
    policyJournal: journal.path,
  };
}

type FinalizedPolicyProvenance = Readonly<{
  path: string;
  seed: bigint;
  policy: Address;
  creationSignature: string;
  creationMessageSha256: string;
  creationMessageBase64: string;
  compiledPolicy: ReturnType<typeof decodePolicyCreateWire>;
  policyDataBytes: number;
  policyDataSha256: string;
  settingsIdentity: Readonly<{
    address: Address;
    owner: string;
    dataBytes: number;
    dataSha256: string;
    policySeed: string;
  }>;
}>;

function assertPolicyCompilerSourceTree(record: JsonRecord): void {
  const compiler = recordAt(record.compiler, "repair-policy journal compiler");
  if (String(compiler.compilerSourceTreeSha256 ?? "") !== compilerSourceTreeSha256(REPOSITORY_ROOT)) {
    throw new Error(
      "RECONCILE_MISMATCH: policy compiler source tree drifted; operator must use the same checkout revision",
    );
  }
}

async function verifyFinalizedPolicyCreationJournal(
  rpcUrl: string,
  path: string,
): Promise<FinalizedPolicyProvenance> {
  const { record, wire, finalized } = await readFinalizedJournal(
    rpcUrl,
    path,
    REPAIR_POLICY_SCHEMA,
    "repair-policy",
  );
  assertPolicyCompilerSourceTree(record);
  const seed = parseSeed(String(record.expectedSeed ?? record.seed ?? ""), "repair-policy journal expectedSeed");
  const policy = address(stringAt(record.policy, "repair-policy journal policy"));
  assertRepairPolicyHardBinding(seed, policy);
  const hardBinding = recordAt(record.hardBinding, "repair-policy journal hardBinding");
  if (String(hardBinding.expectedSeed) !== REPAIR_POLICY_EXPECTED_SEED.toString()
    || String(hardBinding.expectedPolicy) !== REPAIR_POLICY_EXPECTED_PDA
    || Number(hardBinding.createDataBytes) !== REPAIR_POLICY_CREATE_DATA_BYTES
    || String(hardBinding.createDataSha256) !== REPAIR_POLICY_CREATE_DATA_SHA256) {
    throw new Error("repair-policy journal hard binding is not the reviewed seed-140 contract");
  }
  const transaction = wire.transaction;
  const policyCreate = recordAt(transaction.policyCreate, "repair-policy transaction.policyCreate");
  const policyCreateData = assertBase64Bytes(
    stringAt(policyCreate.dataBase64, "repair-policy transaction.policyCreate.dataBase64"),
    "repair-policy transaction.policyCreate.dataBase64",
  );
  const policyCreateDataSha256 = createHash("sha256").update(policyCreateData).digest("hex");
  if (policyCreateData.length !== REPAIR_POLICY_CREATE_DATA_BYTES
    || policyCreateDataSha256 !== REPAIR_POLICY_CREATE_DATA_SHA256
    || String(policyCreate.programId) !== SQUADS_PROGRAM) {
    throw new Error("repair-policy journal PolicyCreate wire is not the reviewed exact-NAV message");
  }
  const compiledPolicy = decodePolicyCreateWire(
    wireInstruction(policyCreate as unknown as PolicyWireInstruction),
  );
  const policyAccount = await getAccount(policy, "finalized");
  const policyDataSha256 = policyAccount
    ? createHash("sha256").update(policyAccount.data).digest("hex")
    : null;
  const semantics = repairPolicySemantics(
    decodeSquadsPolicy(policyAccount),
    seed,
    compiledPolicy,
  );
  if (!policyAccount || !semantics.identityPass || !semantics.payloadPass
    || !semantics.digestUnconstrained || !semantics.navExact.pass || !semantics.compiledMatch) {
    throw new Error("finalized repair policy journal points to a policy with drifted compiled semantics");
  }
  const settings = await getAccount(SQUADS_SETTINGS, "finalized");
  const settingsIdentity = recordAt(record.settingsIdentity, "repair-policy journal settingsIdentity");
  const settingsOwner = stringAt(settingsIdentity.owner, "settingsIdentity.owner");
  const settingsAddress = address(stringAt(settingsIdentity.address, "settingsIdentity.address"));
  const settingsDataBytes = Number(settingsIdentity.dataBytes);
  const settingsDataSha256 = stringAt(settingsIdentity.dataSha256, "settingsIdentity.dataSha256");
  const recordedSettingsPolicySeed = BigInt(stringAt(settingsIdentity.policySeed, "settingsIdentity.policySeed"));
  const settingsPolicySeed = decodeSquadsSettingsPolicySeed(settings);
  if (settingsAddress !== SQUADS_SETTINGS
    || settingsOwner !== SQUADS_PROGRAM
    || recordedSettingsPolicySeed !== seed
    || !settings
    || settings.owner !== settingsOwner
    || settingsPolicySeed === null
    || BigInt(settingsPolicySeed) < seed
    || !Number.isInteger(settingsDataBytes)
    || settingsDataBytes <= 0
    || !/^[0-9a-f]{64}$/.test(settingsDataSha256)) {
    throw new Error("finalized repair-policy journal Settings identity does not match chain");
  }
  if (String(record.finalizedPolicyDataSha256 ?? "") !== policyDataSha256
    || Number(record.finalizedPolicyDataBytes) !== policyAccount.data.length) {
    throw new Error("repair-policy journal finalized policy hash/length disagrees with chain");
  }
  if (wire.signature !== finalized.transaction.signatures[0]) {
    throw new Error("finalized PolicyCreate signature differs from the journal");
  }
  return {
    path,
    seed,
    policy,
    creationSignature: wire.signature,
    creationMessageSha256: wire.messageSha256,
    creationMessageBase64: Buffer.from(wire.message).toString("base64"),
    compiledPolicy,
    policyDataBytes: policyAccount.data.length,
    policyDataSha256,
    settingsIdentity: {
      address: settingsAddress,
      owner: settings.owner,
      dataBytes: settings.data.length,
      dataSha256: createHash("sha256").update(settings.data).digest("hex"),
      policySeed: settingsPolicySeed,
    },
  };
}

type FinalizedRepairJournal = Readonly<{
  path: string;
  record: JsonRecord;
  finalized: FinalizedTransaction;
  seed: bigint;
  policy: Address;
}>;

async function verifyFinalizedRepairJournal(
  rpcUrl: string,
  path: string,
  policyJournal: string,
): Promise<FinalizedRepairJournal> {
  const { record, finalized } = await readFinalizedJournal(
    rpcUrl,
    path,
    REPAIR_EXECUTION_SCHEMA,
    "repair",
  );
  const seed = parseSeed(String(record.seed ?? record.expectedSeed ?? ""), "repair journal seed");
  const policy = address(stringAt(record.policy, "repair journal policy"));
  assertRepairPolicyHardBinding(seed, policy);
  if (resolve(String(record.policyJournal ?? "")) !== resolve(policyJournal)) {
    throw new Error("repair journal is not linked to the supplied finalized PolicyCreate journal");
  }
  // The one-shot policy may already be closed by PolicyRemove when this
  // journal is checked from the later restore gate. Re-read the immutable
  // PolicyCreate transaction and its decoded semantic provenance here.
  const policyCreation = await readFinalizedJournal(
    rpcUrl,
    resolve(policyJournal),
    REPAIR_POLICY_SCHEMA,
    "repair-policy",
  );
  assertPolicyCompilerSourceTree(policyCreation.record);
  const creationSeed = parseSeed(
    String(policyCreation.record.expectedSeed ?? policyCreation.record.seed ?? ""),
    "repair-policy journal expectedSeed",
  );
  const creationPolicy = address(stringAt(policyCreation.record.policy, "repair-policy journal policy"));
  if (creationSeed !== seed || creationPolicy !== policy) {
    throw new Error("repair journal policy provenance does not match the finalized PolicyCreate");
  }
  const policyCreate = recordAt(policyCreation.wire.transaction.policyCreate, "repair journal PolicyCreate");
  const policyCreateData = assertBase64Bytes(
    stringAt(policyCreate.dataBase64, "repair journal PolicyCreate.dataBase64"),
    "repair journal PolicyCreate.dataBase64",
  );
  if (String(policyCreate.programId) !== SQUADS_PROGRAM
    || policyCreateData.length !== REPAIR_POLICY_CREATE_DATA_BYTES
    || createHash("sha256").update(policyCreateData).digest("hex") !== REPAIR_POLICY_CREATE_DATA_SHA256) {
    throw new Error("repair journal is not linked to the reviewed seed-140 PolicyCreate wire");
  }
  const compiledPolicy = decodePolicyCreateWire(
    wireInstruction(policyCreate as unknown as PolicyWireInstruction),
  );
  const recordedPolicy = policyCreation.record.decodedPolicy === null
    || typeof policyCreation.record.decodedPolicy !== "object"
    ? null
    : policyCreation.record.decodedPolicy as ReturnType<typeof decodeSquadsPolicy>;
  const policySemantics = repairPolicySemantics(recordedPolicy, seed, compiledPolicy);
  if (!policySemantics.identityPass || !policySemantics.payloadPass
    || !policySemantics.digestUnconstrained || !policySemantics.navExact.pass
    || !policySemantics.compiledMatch) {
    throw new Error("repair journal PolicyCreate provenance does not carry the exact compiled policy semantics");
  }
  const policyHardBinding = recordAt(policyCreation.record.hardBinding, "repair journal PolicyCreate.hardBinding");
  if (String(policyHardBinding.expectedSeed) !== REPAIR_POLICY_EXPECTED_SEED.toString()
    || String(policyHardBinding.expectedPolicy) !== REPAIR_POLICY_EXPECTED_PDA) {
    throw new Error("repair journal PolicyCreate hard binding is not seed 140");
  }
  const postState = recordAt(record.finalizedPostState, "repair journal finalizedPostState");
  const postChecks = record.expectedPostState ?? record.postState;
  if (postChecks === undefined) throw new Error("repair journal has no expected post-state");
  const policyDataBytes = Number(record.finalizedPolicyDataBytes);
  const policyDataSha256 = String(record.finalizedPolicyDataSha256 ?? "");
  if (!Number.isInteger(policyDataBytes) || policyDataBytes <= 0
    || !/^[0-9a-f]{64}$/.test(policyDataSha256)
    || policyDataBytes !== Number(policyCreation.record.finalizedPolicyDataBytes)
    || policyDataSha256 !== String(policyCreation.record.finalizedPolicyDataSha256 ?? "")) {
    throw new Error("repair journal finalized policy observation differs from the PolicyCreate journal");
  }
  const requiredPostState: Readonly<Record<string, string>> = {
    totalValue: REPAIRED_BOOK_RAW.toString(),
    idleBalance: REPAIRED_BOOK_RAW.toString(),
    receipt1PositionValue: PHANTOM_NAV_RAW.toString(),
    custody1Balance: "0",
    lpSupply: REPAIR_FROZEN.lpSupply.toString(),
    lockedProfitDegradationDuration: REPAIR_FROZEN.degradation.toString(),
  };
  for (const [field, expected] of Object.entries(requiredPostState)) {
    if (String(postState[field] ?? "") !== expected) {
      throw new Error(`repair journal finalized post-state ${field} is ${String(postState[field] ?? "absent")}; expected ${expected}`);
    }
  }
  const recordedProvenance = recordAt(record.policyProvenance, "repair journal policyProvenance");
  if (String(recordedProvenance.creationSignature ?? "") !== policyCreation.wire.signature
    || String(recordedProvenance.creationMessageSha256 ?? "") !== policyCreation.wire.messageSha256
    || Number(recordedProvenance.policyDataBytes) !== policyDataBytes
    || String(recordedProvenance.policyDataSha256 ?? "") !== policyDataSha256) {
    throw new Error("repair journal policy provenance fields differ from the finalized PolicyCreate");
  }
  return { path, record, finalized, seed, policy };
}

async function compileRepairPolicy(seed: bigint) {
  assertRepairPolicyHardBinding(seed, REPAIR_POLICY_EXPECTED_PDA);
  const repairTarget: CustomPolicyTarget = {
    route: RWA_MULTIPLY_ROUTE,
    seeds: {
      allocation: seed - 1n,
      navRefresh: seed,
      stageWithdrawal: seed + 1n,
      withdraw: seed + 2n,
    },
    caps: {
      amountRaw: RWA_MULTIPLY_ROUTE.vault.capRaw,
      reportNavRaw: null,
      dailySpendingLimit: null,
    },
  };
  const artifact = await compileCustomPolicyArtifact(
    seed - 2n,
    repairTarget,
    { navExactRaw: PHANTOM_NAV_RAW },
  );
  const target = artifact.policies.find((entry) =>
    entry.operation === REPAIR_POLICY_OPERATION && BigInt(entry.seed) === seed);
  if (!target) throw new Error(`compiler did not emit nav-refresh policy seed ${seed}`);
  if (target.policy !== policyPda(seed)) {
    throw new Error(`compiler policy PDA for seed ${seed} drifted from the local derivation`);
  }
  assertRepairPolicyHardBinding(
    seed,
    address(target.policy),
    Buffer.from(target.createInstruction.dataBase64, "base64"),
  );
  if (JSON.stringify(target.constraintIndices) !== JSON.stringify([0, 1])) {
    throw new Error("nav-refresh policy must constrain arm_report and deposit_strategy with [0, 1]");
  }
  return { artifact, target } as const;
}

type CustomExecutionArtifact = Readonly<{
  schema: "loyal-voltr-custom-execution/v2";
  sourceSha256: string;
  instruction: PolicyWireInstruction;
  compiler: CompilerProvenance;
}>;

function customExecutionSource(
  policy: Address,
  inner: readonly Instruction[],
  constraintIndices: readonly number[],
): string {
  return JSON.stringify({
    policy,
    delegatedSigner: DELEGATED_EXECUTOR,
    accountIndex: 0,
    constraintIndices,
    inner: inner.map(wireFromInstruction),
  });
}

function compileCustomExecution(
  policy: Address,
  inner: readonly Instruction[],
  constraintIndices: readonly number[],
): CustomExecutionArtifact {
  if (inner.length === 0 || inner.length !== constraintIndices.length) {
    throw new Error("repair execution inner instructions and constraints must be nonempty and aligned");
  }
  const source = customExecutionSource(policy, inner, constraintIndices);
  const result = runRustCompiler<{
    schema?: unknown;
    sourceSha256?: unknown;
    instruction?: PolicyWireInstruction;
  }>({
    compilerBinary: EXECUTION_COMPILER_BIN,
    cwd: REPOSITORY_ROOT,
    input: source,
    maxBuffer: 16 * 1024 * 1024,
    label: "repair execution compiler",
  });
  const output = result.output;
  if (output.schema !== "loyal-voltr-custom-execution/v2"
    || typeof output.sourceSha256 !== "string"
    || !output.instruction
    || typeof output.instruction.programId !== "string"
    || !Array.isArray(output.instruction.accounts)
    || typeof output.instruction.dataBase64 !== "string") {
    throw new Error("repair execution compiler escaped its exact v2 wrapper contract");
  }
  const expectedSourceSha256 = createHash("sha256").update(source).digest("hex");
  if (output.sourceSha256 !== expectedSourceSha256) {
    throw new Error("repair execution compiler source hash drifted");
  }
  if (output.instruction.programId !== SQUADS_PROGRAM
    || output.instruction.accounts[0]?.address !== policy
    || output.instruction.accounts[2]?.address !== DELEGATED_EXECUTOR
    || !output.instruction.accounts[2]?.signer) {
    throw new Error("repair execution wrapper account graph drifted from policy/delegated signer");
  }
  return {
    schema: "loyal-voltr-custom-execution/v2",
    sourceSha256: output.sourceSha256,
    instruction: output.instruction,
    compiler: result.compiler,
  };
}

function findUniqueSubarray(haystack: Uint8Array, needle: ArrayLike<number>, label: string): number {
  let found = -1;
  for (let offset = 0; offset <= haystack.length - needle.length; offset += 1) {
    let matches = true;
    for (let index = 0; index < needle.length; index += 1) {
      if (haystack[offset + index] !== needle[index]) {
        matches = false;
        break;
      }
    }
    if (matches) {
      if (found !== -1) throw new Error(`${label} appears more than once in compiled ExecuteSync data`);
      found = offset;
    }
  }
  if (found === -1) throw new Error(`${label} is absent from compiled ExecuteSync data`);
  return found;
}

/**
 * The Rust wrapper compiler is the slow step. Compile once with a slot-free
 * placeholder, then replace only the two equal-length inner report payloads.
 * Account ordering and all wrapper framing remain compiler-owned; a final
 * source hash is emitted for the exact slot-bearing inner instructions.
 */
function rebindCustomExecutionArtifact(
  artifact: CustomExecutionArtifact,
  policy: Address,
  previousInner: readonly Instruction[],
  nextInner: readonly Instruction[],
  constraintIndices: readonly number[],
): CustomExecutionArtifact {
  if (previousInner.length !== nextInner.length || previousInner.length !== constraintIndices.length) {
    throw new Error("compiled ExecuteSync rebind inputs are not aligned");
  }
  const data = new Uint8Array(Buffer.from(artifact.instruction.dataBase64, "base64"));
  for (let index = 0; index < previousInner.length; index += 1) {
    const previous = previousInner[index]!.data ?? new Uint8Array();
    const next = nextInner[index]!.data ?? new Uint8Array();
    if (previous.length !== next.length) {
      throw new Error(`compiled ExecuteSync rebind changed inner instruction ${index} length`);
    }
    const offset = findUniqueSubarray(data, previous, `inner instruction ${index}`);
    data.set(next, offset);
  }
  return {
    ...artifact,
    sourceSha256: createHash("sha256")
      .update(customExecutionSource(policy, nextInner, constraintIndices))
      .digest("hex"),
    instruction: {
      ...artifact.instruction,
      dataBase64: Buffer.from(data).toString("base64"),
    },
  };
}

function buildPolicyRemoveInstruction(policy: Address): Instruction {
  const web3Instruction = executeSettingsTransactionSync({
    settingsPda: new PublicKey(SQUADS_SETTINGS),
    signers: [new PublicKey(ADMIN)],
    actions: [{ __kind: "PolicyRemove" as const, policy: new PublicKey(policy) }],
    feePayer: new PublicKey(ADMIN),
    programId: new PublicKey(SQUADS_PROGRAM),
    remainingAccounts: [{ pubkey: new PublicKey(policy), isSigner: false, isWritable: true }],
  });
  if (web3Instruction.programId.toBase58() !== SQUADS_PROGRAM) {
    throw new Error("PolicyRemove escaped the Squads program");
  }
  const instruction = fromWeb3Instruction(web3Instruction);
  const accounts = instruction.accounts ?? [];
  if (accounts.length !== 6
    || accounts[0]?.address !== SQUADS_SETTINGS
    || accounts[0]?.role !== AccountRole.WRITABLE
    || accounts[1]?.address !== ADMIN
    || accounts[1]?.role !== AccountRole.WRITABLE_SIGNER
    || accounts[4]?.address !== ADMIN
    || accounts[4]?.role !== AccountRole.READONLY_SIGNER
    || accounts[5]?.address !== policy
    || accounts[5]?.role !== AccountRole.WRITABLE) {
    throw new Error("PolicyRemove account graph drifted from Settings/admin/policy");
  }
  return instruction;
}

function repairPolicySemantics(
  decoded: ReturnType<typeof decodeSquadsPolicy>,
  seed: bigint,
  compiled: ReturnType<typeof decodePolicyCreateWire> | null = null,
) {
  const constraints = decoded?.constraints ?? [];
  const dataConstraints = constraints
    .flatMap((constraint) => constraint.dataConstraints)
    .filter((constraint): constraint is {
      offset: string;
      operator: string;
      kind: string;
      value: string;
    } => "offset" in constraint);
  const digestConstraints = dataConstraints.filter((constraint) =>
    constraint.offset === "47" || constraint.offset === "59");
  const navExactConstraints = dataConstraints.filter((constraint) =>
    constraint.offset === "39" || constraint.offset === "51");
  const navExactPass = navExactConstraints.length === 2
    && ["39", "51"].every((offset) => {
      const matches = navExactConstraints.filter((constraint) => constraint.offset === offset);
      return matches.length === 1
        && matches[0]?.kind === "U64Le"
        && matches[0]?.operator === "Equals"
        && BigInt(matches[0].value) === PHANTOM_NAV_RAW;
    });
  const compiledMatch = decoded !== null && compiled !== null
    && compiled.seed === decoded.seed
    && compiled.threshold === decoded.threshold
    && compiled.timeLock === decoded.timeLock
    && toJson(compiled.signers) === toJson(decoded.signers)
    && compiled.policyState === decoded.policyState
    && compiled.accountIndex === decoded.accountIndex
    && toJson(stablePolicyConstraints(compiled)) === toJson(stablePolicyConstraints(decoded));
  return {
    identityPass: decoded !== null
      && decoded.settings === SQUADS_SETTINGS
      && decoded.seed === seed.toString()
      && decoded.threshold === 1
      && decoded.timeLock === 0
      && decoded.signers.length === 1
      && decoded.signers[0]?.address === DELEGATED_EXECUTOR
      && decoded.signers[0]?.permissionsMask === 7,
    payloadPass: decoded !== null
      && decoded.policyState === "ProgramInteraction"
      && decoded.accountIndex === 0
      && decoded.preHook === null
      && decoded.postHook === null
      && decoded.spendingLimitCount === 0
      && constraints.length === 2
      && constraints[0]?.programId === RWA_MULTIPLY_ROUTE.customAdaptor.program
      && constraints[1]?.programId === VOLTR,
    digestUnconstrained: digestConstraints.length === 0,
    navExact: {
      present: navExactConstraints.length > 0,
      pass: navExactPass,
      constraints: navExactConstraints,
    },
    compiledMatch,
    compiledPolicy: compiled,
    decoded,
  } as const;
}

function writePrivate(path: string, value: Record<string, unknown>, flag: "w" | "wx") {
  const bytes = Buffer.from(`${toJson(value, 2)}\n`);
  if (flag === "wx") writePrivateExclusive(path, bytes);
  else writePrivateAtomic(path, bytes);
}

type RepairPolicyOperatorMode = "execute" | "reconcile";

function operatorMode(): RepairPolicyOperatorMode | null {
  return currentCli().mode;
}

function operatorJournal(): string {
  const journal = resolve(cliValue("--journal"));
  if (!journal.endsWith(".json") || !existsSync(dirname(journal))) {
    throw new Error("--execute/--reconcile requires --journal PATH.json under an existing directory");
  }
  return journal;
}

function canonicalLegStatePath(step: string, create = false, stateRootOverride?: string): string {
  if (!/^[a-z0-9-]+$/.test(step)) throw new Error(`invalid canonical HXtk leg name ${step}`);
  return resolve(canonicalStateRoot(create, stateRootOverride), `${step}.state`);
}

function canonicalStateRoot(create = false, stateRootOverride?: string): string {
  return stateRootOverride ?? resolveCanonicalStateRoot({ vault: VAULT.toString(), create });
}

function readCanonicalLegState(
  step: string,
  create = false,
  stateRootOverride?: string,
  options: Readonly<{ allowRollForward?: boolean }> = {},
): JsonRecord | null {
  const path = canonicalLegStatePath(step, create, stateRootOverride);
  let parsed: JsonRecord;
  try {
    parsed = readCanonicalState(path, options).record;
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT"
      && !existsSync(path + ".gen-0")) return null;
    throw error;
  }
  if (parsed.schema !== HXTK_STATE_SCHEMA
    || parsed.step !== step
    || parsed.vault !== VAULT.toString()
    || parsed.checkoutRoot !== REPOSITORY_ROOT
    || parsed.stateRoot !== canonicalStateRoot(false, stateRootOverride)) {
    throw new Error(`canonical HXtk state ${path} has an unexpected schema, leg, or vault`);
  }
  if (parsed.status !== "pending" && parsed.status !== "attempted"
    && parsed.status !== "finalized" && parsed.status !== "aborted-pre-send") {
    throw new Error(`canonical HXtk state ${path} has an invalid status`);
  }
  canonicalStateGeneration(parsed);
  return parsed;
}

function stateGeneration(state: JsonRecord | null): number | null {
  return state === null ? null : canonicalStateGeneration(state);
}

function assertCanonicalLegAvailable(
  step: string,
  journal: string,
  stateRootOverride?: string,
  existingOverride?: JsonRecord | null,
): JsonRecord | null {
  const existing = existingOverride === undefined
    ? readCanonicalLegState(step, false, stateRootOverride)
    : existingOverride;
  const currentAttemptToken = String(existing?.attemptToken ?? "");
  if (existing?.status !== "aborted-pre-send") {
    const foreign = interruptedAbortedJournalPaths(journal).filter((artifact) =>
      artifact.filenameAttemptToken !== currentAttemptToken
      || abortArtifactAttemptToken(artifact) !== currentAttemptToken);
    if (foreign.length > 0) {
      throw new Error(
        `JOURNAL_HAS_FOREIGN_ABORT_ARTIFACT: ${foreign.map((artifact) => artifact.path).join(", ")}`,
      );
    }
  }
  assertCanonicalLegAvailableFence({
    step,
    journal,
    existing,
    repeatable: REPEATABLE_LEGS.has(step),
    allowRepeat: currentCli().has("--allow-repeat"),
    journalExists: existsSync(journal) || existsSync(`${journal}.pending`),
  });
  return existing;
}

function beginCanonicalLegState(
  step: string,
  journal: string,
  pending: JsonRecord,
  existing: JsonRecord | null,
  expectedGeneration: number | null,
  stateRootOverride?: string,
): Readonly<{ path: string; generation: number }> {
  const path = canonicalLegStatePath(step, true, stateRootOverride);
  const transaction = recordAt(pending.transaction, "pending transaction");
  const attemptToken = stringAt(pending.attemptToken, "pending attemptToken");
  if (!/^[0-9a-f]{32}$/.test(attemptToken)) {
    throw new Error("pending attemptToken must be a 16-byte lowercase hex token");
  }
  const lastValidBlockHeight = Number(transaction.lastValidBlockHeight);
  if (!Number.isSafeInteger(lastValidBlockHeight) || lastValidBlockHeight < 0) {
    throw new Error("pending transaction.lastValidBlockHeight is missing or invalid");
  }
  const state = beginCanonicalLegRecord({
    step,
    vault: VAULT.toString(),
    journal,
    expectedSignature: stringAt(transaction.expectedSignature, "pending transaction.expectedSignature"),
    messageSha256: stringAt(transaction.messageSha256, "pending transaction.messageSha256"),
    wireSha256: stringAt(transaction.wireSha256, "pending transaction.wireSha256"),
    pendingBindingSha256: stringAt(pending.pendingBindingSha256, "pending pendingBindingSha256"),
    attemptToken,
    signedWireBase64: stringAt(pending.signedWireBase64, "pending signedWireBase64"),
    messageBase64: stringAt(transaction.messageBase64, "pending transaction.messageBase64"),
    lastValidBlockHeight,
    pendingRecord: pending,
    checkoutRoot: REPOSITORY_ROOT,
    stateRoot: canonicalStateRoot(false, stateRootOverride),
    repeatable: REPEATABLE_LEGS.has(step),
    allowRepeat: currentCli().has("--allow-repeat"),
    journalExists: existsSync(journal) || existsSync(`${journal}.pending`),
    existing,
  });
  const written = writeCanonicalStateCas(path, state, expectedGeneration);
  return { path, generation: canonicalStateGeneration(written) };
}

function markCanonicalLegState(
  step: string,
  journal: string,
  patch: Readonly<{
    status: CanonicalLegStateStatus;
    broadcast: false | "attempted" | true;
    signature?: string;
    error?: string;
    abortReason?: string;
    abortedJournal?: string;
    attemptGeneration?: number;
    lastValidBlockHeight?: number;
    rearmable?: boolean;
    attemptedExpiryProof?: AttemptedExpiryProof;
    finalizedJournalSha256?: string;
  }>,
  expectedGeneration: number | null,
  stateRootOverride?: string,
): number {
  const path = canonicalLegStatePath(step, false, stateRootOverride);
  const current = readCanonicalLegState(step, false, stateRootOverride, { allowRollForward: true });
  if (current === null) throw new Error(`${step} canonical state disappeared during execution`);
  if (stateGeneration(current) !== expectedGeneration) {
    throw new Error(
      `STATE_GENERATION_CONFLICT: expected generation ${expectedGeneration ?? "absent"}, `
      + `found ${stateGeneration(current) ?? "absent"}`,
    );
  }
  if (String(current.journal ?? "") !== journal) {
    throw new Error(
      `${step} canonical replay fence belongs to journal ${sanitizeText(String(current.journal ?? ""))}; `
      + `refusing journal ${sanitizeText(journal)}`,
    );
  }
  const written = writeCanonicalStateCas(path, {
    ...current,
    ...patch,
    ...(patch.error === undefined ? {} : { error: sanitizeText(patch.error) }),
    updatedAtUnixMs: Date.now(),
  }, expectedGeneration);
  return canonicalStateGeneration(written);
}

function ensureCanonicalLegStateForReconcile(
  step: string,
  journal: string,
  pending: JsonRecord,
  wire: Readonly<{ signature: string; messageSha256: string; wireSha256: string }>,
  existing: JsonRecord | null,
  expectedGeneration: number | null,
  stateRootOverride?: string,
): Readonly<{ path: string; generation: number }> {
  const path = canonicalLegStatePath(step, false, stateRootOverride);
  if (existing === null) throw new Error("RECONCILE_MISMATCH: pending journal does not match the canonical binding");
  assertPendingJournalBinding({
    pending,
    canonicalState: existing,
    expectedSignature: wire.signature,
    messageSha256: wire.messageSha256,
    wireSha256: wire.wireSha256,
  });
  if (existing.status === "finalized") {
    throw new Error(`${step} canonical state is already finalized; refusing a second reconciliation`);
  }
  if (existing.status !== "attempted"
    || String(existing.journal ?? "") !== journal) {
    throw new Error(`${step} pending journal does not match the canonical replay state`);
  }
  if (String(existing.attemptToken ?? "") !== String(pending.attemptToken ?? "")
    || String(existing.signature ?? wire.signature) !== wire.signature) {
    throw new Error(`${step} pending journal does not match the attempted canonical record`);
  }
  if (stateGeneration(existing) !== expectedGeneration) {
    throw new Error(
      `STATE_GENERATION_CONFLICT: expected generation ${expectedGeneration ?? "absent"}, `
      + `found ${stateGeneration(existing) ?? "absent"}`,
    );
  }
  // Recovery never creates an attempted record. The sending process wrote it
  // exactly once immediately before the sole raw submission; reconciliation
  // only proves that this already-attempted record finalized or expired.
  return { path, generation: expectedGeneration! };
}

function repairPolicyRemoveJournalPath(repairJournal: string): string {
  return repairJournal.replace(/\.json$/i, ".policy-remove.json");
}

export const HXTK_RECOVERY_LEGS = [
  "verify",
  "config",
  "repair-policy",
  "repair",
  "repair-policy-remove",
  "harvest",
  "cancel",
  "request",
  "claim",
  "restore-degradation",
] as const;

export type HxtkRecoveryLeg = typeof HXTK_RECOVERY_LEGS[number];

export function buildHxtkRecoveryCommand(input: Readonly<{
  step: HxtkRecoveryLeg | string;
  mode: "simulate" | "execute" | "reconcile";
  journal?: string;
  flags?: readonly string[];
  finalized?: boolean;
  breakClaim?: boolean;
  allowRepeat?: boolean;
}>): string {
  const repeatable = REPEATABLE_LEGS.has(input.step);
  if (input.allowRepeat && !repeatable) {
    throw new Error(`--allow-repeat is not accepted for one-shot HXtk leg ${input.step}`);
  }
  if (input.finalized) return `${input.step} already finalized; verify with bun run reset:hxtk verify`;
  const prefix = input.mode === "execute"
    ? "op run --env-file=.env.1password -- env CONFIRM_MAINNET=1 bun run reset:hxtk"
    : "bun run reset:hxtk";
  const args = [prefix, input.step, ...(input.flags ?? []), `--${input.mode}`];
  if (input.allowRepeat) args.push("--allow-repeat");
  if (input.breakClaim) args.push("--break-claim");
  if (input.journal !== undefined) args.push("--journal", resolve(input.journal));
  return args.join(" ");
}

export function buildRepairRecoveryCommands(input: Readonly<{
  repairJournal: string;
  policyJournal: string;
  policyRemoveJournal: string;
  repairStatus?: CanonicalLegStateStatus | null | undefined;
  removalStatus?: CanonicalLegStateStatus | null | undefined;
}>): string[] {
  const repairState = input.repairStatus;
  const removalState = input.removalStatus;
  const removalMode = removalState !== null
    && removalState !== undefined
    && removalState !== "aborted-pre-send"
    ? "reconcile"
    : "execute";
  const repairJournal = resolve(input.repairJournal);
  const policyJournal = resolve(input.policyJournal);
  const policyRemoveJournal = resolve(input.policyRemoveJournal);
  const command = (step: HxtkRecoveryLeg, mode: "execute" | "reconcile", flags: readonly string[], journal: string) =>
    buildHxtkRecoveryCommand({ step, mode, flags, journal });
  const repairRecovery = repairState === "finalized"
    ? buildHxtkRecoveryCommand({ step: "repair", mode: "reconcile", finalized: true })
    : command("repair", "reconcile", ["--expect-seed", "140", "--policy-journal", policyJournal], repairJournal);
  const removalRecovery = removalState === "finalized"
    ? buildHxtkRecoveryCommand({ step: "repair-policy-remove", mode: "reconcile", finalized: true })
    : command(
        "repair-policy-remove",
        removalMode,
        ["--expect-seed", "140", "--policy-journal", policyJournal, "--repair-journal", repairJournal],
        policyRemoveJournal,
      );
  return [
    repairRecovery,
    removalRecovery,
  ];
}

function repairRecoveryCommands(input: Readonly<{
  repairJournal: string;
  policyJournal: string;
  policyRemoveJournal: string;
}>): string[] {
  return buildRepairRecoveryCommands({
    ...input,
    repairStatus: readCanonicalLegState("repair")?.status as CanonicalLegStateStatus | null | undefined,
    removalStatus: readCanonicalLegState("repair-policy-remove")?.status as CanonicalLegStateStatus | null | undefined,
  });
}

function abortedJournalPath(journal: string, attemptToken: string): string {
  if (!/^[0-9a-f]{32}$/.test(attemptToken)) {
    throw new Error("abort artifact requires a 16-byte lowercase hex attemptToken");
  }
  let timestamp = Date.now();
  let path = `${journal}.aborted-${timestamp}-${attemptToken}.json`;
  while (existsSync(path)) {
    timestamp += 1;
    path = `${journal}.aborted-${timestamp}-${attemptToken}.json`;
  }
  return path;
}

type AbortArtifact = Readonly<{
  path: string;
  filenameAttemptToken: string | null;
  record: JsonRecord | null;
}>;

function interruptedAbortedJournalPaths(journal: string): AbortArtifact[] {
  const directory = dirname(journal);
  const prefix = `${basename(journal)}.aborted-`;
  try {
    return readdirSync(directory, { withFileTypes: true })
      .filter((entry) => entry.isFile() && entry.name.startsWith(prefix) && entry.name.endsWith(".json"))
      .map((entry) => {
        const path = resolve(directory, entry.name);
        const suffix = entry.name.slice(prefix.length, -".json".length);
        const filenameAttemptToken = /^\d+-([0-9a-f]{32})$/.exec(suffix)?.[1] ?? null;
        try {
          return {
            path,
            filenameAttemptToken,
            record: readBoundJournal(path).record,
          };
        } catch {
          // A malformed or legacy artifact is stale by definition; keep it
          // visible to the caller instead of allowing it to drive recovery.
          return { path, filenameAttemptToken, record: null };
        }
      })
      .sort((left, right) => left.path.localeCompare(right.path));
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") return [];
    throw error;
  }
}

function abortArtifactAttemptToken(artifact: AbortArtifact): string | null {
  const contentToken = artifact.record?.attemptToken;
  if (typeof contentToken !== "string" || !/^[0-9a-f]{32}$/.test(contentToken)) return null;
  return contentToken;
}

function abortArtifactMatchesState(artifact: AbortArtifact, state: JsonRecord): boolean {
  const attemptToken = String(state.attemptToken ?? "");
  const binding = String(state.pendingBindingSha256 ?? "");
  return state.status === "pending"
    && attemptToken.length > 0
    && artifact.filenameAttemptToken === attemptToken
    && abortArtifactAttemptToken(artifact) === attemptToken
    && Number(artifact.record?.attemptGeneration) === Number(state.generation)
    && String(artifact.record?.pendingBindingSha256 ?? "") === binding;
}

function abortRecordForState(
  state: JsonRecord,
  pending: JsonRecord,
  reason: string,
  broadcast: false | "attempted" = false,
  extra: JsonRecord = {},
): JsonRecord {
  const attemptToken = String(state.attemptToken ?? pending.attemptToken ?? "");
  const binding = String(state.pendingBindingSha256 ?? pending.pendingBindingSha256 ?? "");
  if (!/^[0-9a-f]{32}$/.test(attemptToken) || binding.length === 0
    || String(pending.attemptToken ?? "") !== attemptToken
    || String(pending.pendingBindingSha256 ?? "") !== binding) {
    throw new Error("STATE_GENERATION_CONFLICT: pending recovery record is not bound to the canonical attempt");
  }
  return {
    ...pending,
    ...extra,
    verdict: broadcast === "attempted" ? "ABORTED_AFTER_BLOCKHASH_EXPIRY" : "ABORTED_PRE_SEND",
    sent: false,
    signed: true,
    broadcast,
    abortReason: reason,
    attemptGeneration: Number(state.attemptGeneration ?? state.generation),
    // pendingBindingSha256 is the journal binding hash; retain it in the
    // artifact under its canonical name as well as the attempt token.
    journalBindingSha256: binding,
    sendStatus: {
      ...(pending.sendStatus && typeof pending.sendStatus === "object" ? pending.sendStatus as JsonRecord : {}),
      verdict: broadcast === "attempted" ? "ABORTED_AFTER_BLOCKHASH_EXPIRY" : "ABORTED_PRE_SEND",
      sendError: reason,
      signature: pending.transaction && typeof pending.transaction === "object"
        ? (pending.transaction as JsonRecord).expectedSignature
        : undefined,
    },
  };
}

function publishAbortArtifact(
  journal: string,
  state: JsonRecord,
  pending: JsonRecord,
  reason: string,
  pendingPath?: string,
  broadcast: false | "attempted" = false,
  extra: JsonRecord = {},
): string {
  const existingArtifact = interruptedAbortedJournalPaths(journal).find((artifact) =>
    artifact.filenameAttemptToken === String(state.attemptToken)
      && abortArtifactAttemptToken(artifact) === String(state.attemptToken)
      && String(artifact.record?.pendingBindingSha256 ?? "") === String(state.pendingBindingSha256 ?? "")
      && Number(artifact.record?.attemptGeneration) === Number(state.attemptGeneration ?? state.generation)
      && (broadcast !== "attempted" || artifact.record?.broadcast === "attempted"),
  );
  if (existingArtifact !== undefined) return existingArtifact.path;
  const artifactPath = abortedJournalPath(journal, String(state.attemptToken));
  if (pendingPath !== undefined && existsSync(pendingPath)) {
    // The renamed file is the durable abort artifact. Rewrite the complete
    // record, including proven-expiry proof and rearmable fields; a
    // volatile-only pending rewrite would drop those fields at the rename.
    const currentPending = readBoundPending(pendingPath).record;
    const aborted = abortRecordForState(state, currentPending, reason, broadcast, extra);
    writePrivate(pendingPath, aborted, "w");
    renamePrivateFile(pendingPath, artifactPath);
  } else {
    const aborted = abortRecordForState(state, pending, reason, broadcast, extra);
    writePrivate(artifactPath, aborted, "wx");
  }
  return artifactPath;
}

export type InterruptedTransitionRecovery = Readonly<{
  state: JsonRecord | null;
  staleAbortArtifacts: readonly string[];
  autoAbortedPreSend: boolean;
  attemptedExpiryRecovered: boolean;
}>;

export function resumeInterruptedTransition(input: Readonly<{
  step: string;
  journal: string;
  stateRoot: string;
  mode?: RepairPolicyOperatorMode;
}>): InterruptedTransitionRecovery {
  const pendingPath = `${input.journal}.pending`;
  let state = readCanonicalLegState(input.step, false, input.stateRoot, { allowRollForward: false });
  const artifacts = interruptedAbortedJournalPaths(input.journal);
  if (state !== null
    && typeof state.journal === "string"
    && state.journal !== input.journal
    && (state.status === "attempted" || state.status === "finalized"
      || (state.status === "aborted-pre-send" && state.abortReason === ATTEMPTED_EXPIRY_REASON))) {
    if (state.status === "aborted-pre-send" && state.abortReason === ATTEMPTED_EXPIRY_REASON) {
      throw new Error(`JOURNAL_MISMATCH_ATTEMPTED_EXPIRED: reconcile the original journal ${state.journal}`);
    }
    throw new Error(`JOURNAL_MISMATCH_CANONICAL_STATE: ${state.journal}`);
  }
  if (state?.status === "aborted-pre-send"
    && state.abortReason === PROVEN_ATTEMPTED_EXPIRY_REASON
    && state.journal !== input.journal
    && input.mode === "execute"
    && (existsSync(input.journal) || existsSync(pendingPath))) {
    throw new Error(
      `JOURNAL_EXISTS_ATTEMPTED_EXPIRY_PROVEN: new re-arm journal must not already exist: ${input.journal}`,
    );
  }
  state = readCanonicalLegState(input.step, false, input.stateRoot, { allowRollForward: true });
  const currentAttemptToken = state?.status === "aborted-pre-send" ? null : String(state?.attemptToken ?? "");
  const foreignArtifacts = state?.status === "aborted-pre-send"
    ? []
    : artifacts.filter((artifact) => artifact.filenameAttemptToken !== currentAttemptToken
      || abortArtifactAttemptToken(artifact) !== currentAttemptToken);
  // A new execute must not reuse a journal whose directory contains an abort
  // artifact from another attempt. Reconcile is the read-only chain check for
  // the current attempted record, so it carries foreign artifacts forward as
  // stale evidence instead of letting them shadow that attempt.
  if (foreignArtifacts.length > 0 && input.mode !== "reconcile") {
    throw new Error(
      `JOURNAL_HAS_FOREIGN_ABORT_ARTIFACT: ${foreignArtifacts.map((artifact) => artifact.path).join(", ")}`,
    );
  }

  // Finalization is ordered as: publish the final journal, publish the
  // sent-wire rename, then advance canonical state. Each check is safe to
  // repeat after a crash at any boundary.
  if (existsSync(input.journal)) {
    if (existsSync(pendingPath)) {
      renamePrivateFile(pendingPath, `${input.journal}.sent-wire`);
    }
    if (state === null) {
      throw new Error(`STATE_GENERATION_CONFLICT: ${input.step} finalized journal has no canonical state`);
    }
    if (String(state.journal ?? "") !== input.journal) {
      throw new Error(`STATE_GENERATION_CONFLICT: ${input.step} finalized journal is not bound to canonical state`);
    }
    if (state.status === "finalized"
      && String(state.finalizedJournalSha256 ?? "") !== finalizedJournalSha256(input.journal)) {
      throw new Error(`RECONCILE_MISMATCH: finalized ${input.step} journal hash is not bound to canonical state`);
    }
    if (state.status !== "attempted" && state.status !== "finalized") {
      throw new Error(`STATE_GENERATION_CONFLICT: ${input.step} finalized journal is not backed by an attempted canonical record`);
    }
    // A published final journal is evidence to be checked by the async
    // reconcile path; it is never allowed to promote attempted -> finalized by
    // filename presence alone.
    return {
      state,
      staleAbortArtifacts: artifacts.map((artifact) => artifact.path),
      autoAbortedPreSend: false,
      attemptedExpiryRecovered: false,
    };
  }

  let appliedAbortPath: string | null = null;
  let attemptedExpiryRecovered = false;
  if (state?.status === "aborted-pre-send" && state.abortReason === ATTEMPTED_EXPIRY_REASON) {
    // Do not complete or publish an attempted-expired recovery artifact until
    // the async caller has re-read the expected signature at finalized
    // commitment. RPC lag can make an already-landed signature appear absent.
    // The caller owns the chain read and may finalize this attempt instead.
    attemptedExpiryRecovered = true;
  }
  if (existsSync(pendingPath)) {
    const pending = readBoundPending(pendingPath).record;
    if (state?.status === "pending") {
      // No attempted canonical state means no raw send was reachable. Convert
      // every pre-mark pending recovery to an attempt-bound abort artifact.
      appliedAbortPath = publishAbortArtifact(
        input.journal,
        state,
        pending,
        "recovered pending journal before the canonical attempted mark; no send was reachable",
        pendingPath,
      );
      markCanonicalLegState(
        input.step,
        input.journal,
        {
          status: "aborted-pre-send",
          broadcast: false,
          abortReason: "recovered pending journal before the canonical attempted mark; no send was reachable",
          abortedJournal: appliedAbortPath,
        },
        stateGeneration(state),
        input.stateRoot,
      );
      state = readCanonicalLegState(input.step, false, input.stateRoot, { allowRollForward: true });
    } else if (pending.verdict === "ABORTED_PRE_SEND" && state?.status === "aborted-pre-send") {
      // A previous process may have completed the volatile rewrite but crashed
      // before the rename. Only the already-aborted canonical attempt can adopt
      // this artifact; attempted records are never touched by it.
      const attemptToken = String(state.attemptToken ?? "");
      if (String(pending.attemptToken ?? "") !== attemptToken) {
        throw new Error(`JOURNAL_HAS_FOREIGN_ABORT_ARTIFACT: ${pendingPath}`);
      }
      appliedAbortPath = publishAbortArtifact(
        input.journal,
        state,
        pending,
        String(pending.abortReason ?? "pre-send abort"),
        pendingPath,
      );
    }
  }

  let autoAbortedPreSend = false;
  const hasMatchingAbortArtifact = state?.status === "pending"
    && artifacts.some((artifact) => abortArtifactMatchesState(artifact, state!));
  if (state?.status === "pending" && !existsSync(pendingPath) && !hasMatchingAbortArtifact) {
    const pending = recordAt(state.pendingRecord, `${input.step} canonical pendingRecord`);
    appliedAbortPath = publishAbortArtifact(
      input.journal,
      state,
      pending,
      "recovered canonical pending state before journal publication; no send was reachable",
    );
    markCanonicalLegState(
      input.step,
      input.journal,
      {
        status: "aborted-pre-send",
        broadcast: false,
        abortReason: "recovered canonical pending state before journal publication; no send was reachable",
        abortedJournal: appliedAbortPath,
      },
      stateGeneration(state),
      input.stateRoot,
    );
    state = readCanonicalLegState(input.step, false, input.stateRoot, { allowRollForward: true });
    autoAbortedPreSend = true;
  }

  if (state?.status === "pending") {
    const matching = artifacts.find((artifact) => abortArtifactMatchesState(artifact, state!));
    if (matching !== undefined) {
      const aborted = matching.record!;
      const abortReason = typeof aborted.abortReason === "string" ? aborted.abortReason : "pre-send abort";
      markCanonicalLegState(
        input.step,
        input.journal,
        {
          status: "aborted-pre-send",
          broadcast: false,
          abortReason,
          error: abortReason,
          abortedJournal: matching.path,
        },
        stateGeneration(state),
        input.stateRoot,
      );
      state = readCanonicalLegState(input.step, false, input.stateRoot, { allowRollForward: true });
      appliedAbortPath = matching.path;
    }
  }

  const staleAbortArtifacts = artifacts
    .filter((artifact) => artifact.path !== appliedAbortPath
      && artifact.path !== String(state?.abortedJournal ?? ""))
    .map((artifact) => artifact.path);
  return { state, staleAbortArtifacts, autoAbortedPreSend, attemptedExpiryRecovered };
}

type JsonRecord = Record<string, unknown>;
type FinalizedTransaction = Awaited<ReturnType<typeof finalizedTransaction>>;
type BeforeSendContext = Readonly<{
  pendingJournal: string;
}>;

type JournaledExecutionBuild = Readonly<{
  prepared: PreparedTransaction;
  plan: JsonRecord;
  beforeSend?: (context: BeforeSendContext) => Promise<Readonly<{
    sendStatus?: JsonRecord;
  }> | void>;
}>;

type PreparedSettlement =
  | Awaited<ReturnType<typeof sendPreparedOnce>>
  | Awaited<ReturnType<typeof sendPreparedConfirmedOnce>>;
type PreparedSendOnce = (
  rpcUrl: string,
  prepared: PreparedTransaction,
  authorizedContextSlot: number,
) => Promise<PreparedSettlement>;

type JournaledStepDependencies = Readonly<{
  stateRoot?: string;
  allowExecuteWithoutConfirmation?: boolean;
  sendPreparedOnce?: PreparedSendOnce;
  finalizedTransaction?: typeof finalizedTransaction;
  readFinalizedSignatureStatus?: typeof readFinalizedSignatureStatus;
  currentBlockHeight?: (rpcUrl: string) => Promise<number>;
  sleep?: (milliseconds: number) => Promise<void>;
  faultAfterTransitionStep?: (step: JournalTransitionStep) => void | Promise<void>;
}>;

export type JournalTransitionStep =
  | "canonical-pending"
  | "pending-journal"
  | "attempted-state"
  | "final-journal"
  | "sent-wire"
  | "finalized-state"
  | "aborted-pending"
  | "aborted-journal"
  | "aborted-state"
  | "attempted-expiry-state"
  | "attempted-expiry-artifact"
  | "attempted-expiry-marker";

// A node may lag indexing a signature that was included before expiry. Keep
// this margin explicit: expiry is only decided after finalized height is at
// least 64 blocks beyond the transaction's last-valid height.
export const ATTEMPTED_EXPIRY_RECHECK_MARGIN_BLOCKS = 64;
/** Finalization normally trails confirmed by about 31 slots; wait in bounded 2s polls. */
export const FINALIZED_WAIT_POLL_INTERVAL_MS = 2_000;
export const FINALIZED_WAIT_MAX_POLLS = 30;
export const FINALIZED_WAIT_ERROR_POLLS = 3;
const ATTEMPTED_EXPIRY_REASON = "attempted-expired" as const;
const PROVEN_ATTEMPTED_EXPIRY_REASON = "attempted-expired-proven" as const;

function provenExpiryRearmInstruction(step: string, originalJournal: string): string {
  return `re-arm ${step} with --execute --journal <new-journal>.json after reviewing the proven expiry artifact; original journal is ${originalJournal}`;
}

export class JournalTransitionFault extends Error {
  readonly transitionStep: JournalTransitionStep;

  constructor(step: JournalTransitionStep) {
    super(`HXTK_TEST_INTERRUPTED: ${step}`);
    this.name = "JournalTransitionFault";
    this.transitionStep = step;
  }
}

async function faultAfterTransitionStep(
  dependencies: JournaledStepDependencies,
  step: JournalTransitionStep,
): Promise<void> {
  await dependencies.faultAfterTransitionStep?.(step);
}

type AttemptedExpiryProof = Readonly<{
  lastValidBlockHeight: number;
  finalizedBlockHeight: number;
  absentReads: readonly [
    Readonly<{ kind: "absent"; poll: number; observedAtUnixMs: number }>,
    Readonly<{ kind: "absent"; poll: number; observedAtUnixMs: number }>,
  ];
}>;
type FinalizedAbsentRead = AttemptedExpiryProof["absentReads"][number];

function provenExpiryProof(value: unknown, label: string): AttemptedExpiryProof {
  const record = value && typeof value === "object" && !Array.isArray(value)
    ? value as JsonRecord
    : null;
  const absentReads = record?.absentReads;
  const lastValidBlockHeight = Number(record?.lastValidBlockHeight);
  const finalizedBlockHeight = Number(record?.finalizedBlockHeight);
  if (!Number.isSafeInteger(lastValidBlockHeight)
    || lastValidBlockHeight < 0
    || !Number.isSafeInteger(finalizedBlockHeight)
    || finalizedBlockHeight < 0
    || !Array.isArray(absentReads)
    || absentReads.length !== 2) {
    throw new Error(`PROVEN_ATTEMPTED_EXPIRY_RECORD_INVALID: ${label} is missing both expiry heights or absent reads`);
  }
  const normalizedReads = absentReads.map((value, index) => {
    const read = value && typeof value === "object" && !Array.isArray(value)
      ? value as JsonRecord
      : null;
    const poll = Number(read?.poll);
    const observedAtUnixMs = Number(read?.observedAtUnixMs);
    if (read?.kind !== "absent"
      || !Number.isSafeInteger(poll)
      || poll < 1
      || !Number.isSafeInteger(observedAtUnixMs)
      || observedAtUnixMs < 0) {
      throw new Error(`PROVEN_ATTEMPTED_EXPIRY_RECORD_INVALID: ${label} absent read ${index + 1} is invalid`);
    }
    return { kind: "absent" as const, poll, observedAtUnixMs };
  });
  return {
    lastValidBlockHeight,
    finalizedBlockHeight,
    absentReads: [normalizedReads[0]!, normalizedReads[1]!],
  };
}

function sameProvenExpiryProof(left: AttemptedExpiryProof, right: AttemptedExpiryProof): boolean {
  return left.lastValidBlockHeight === right.lastValidBlockHeight
    && left.finalizedBlockHeight === right.finalizedBlockHeight
    && left.absentReads.every((read, index) => {
      const expected = right.absentReads[index]!;
      return read.kind === expected.kind
        && read.poll === expected.poll
        && read.observedAtUnixMs === expected.observedAtUnixMs;
    });
}

function assertProvenExpiryPublication(
  state: JsonRecord,
  artifact: JsonRecord,
  artifactPath: string,
  canonicalJournal: string,
  expectedAttemptToken: string,
  expectedProof: AttemptedExpiryProof,
): void {
  if (state.status !== "aborted-pre-send"
    || state.abortReason !== PROVEN_ATTEMPTED_EXPIRY_REASON
    || state.rearmable !== true
    || String(state.journal ?? "") !== canonicalJournal
    || String(state.attemptToken ?? "") !== expectedAttemptToken
    || String(state.abortedJournal ?? "") !== artifactPath) {
    throw new Error("PROVEN_ATTEMPTED_EXPIRY_RECORD_INVALID: canonical publication is not complete");
  }
  const stateProof = provenExpiryProof(state.attemptedExpiryProof, "canonical state");
  const artifactProof = provenExpiryProof(artifact.attemptedExpiryProof, "abort artifact");
  if (!sameProvenExpiryProof(stateProof, expectedProof)
    || !sameProvenExpiryProof(artifactProof, expectedProof)
    || artifact.abortReason !== PROVEN_ATTEMPTED_EXPIRY_REASON
    || artifact.broadcast !== "attempted"
    || artifact.rearmable !== true
    || String(artifact.attemptToken ?? "") !== expectedAttemptToken
    || Number(artifact.lastValidBlockHeight) !== expectedProof.lastValidBlockHeight
    || Number(artifact.finalizedBlockHeight) !== expectedProof.finalizedBlockHeight
    || Number(artifact.attemptGeneration) !== Number(state.attemptGeneration)
    || String(artifact.pendingBindingSha256 ?? "") !== String(state.pendingBindingSha256 ?? "")) {
    throw new Error("PROVEN_ATTEMPTED_EXPIRY_RECORD_INVALID: artifact does not preserve the proven attempt proof");
  }
}

type FinalizedWaitResult =
  | Readonly<{ kind: "finalized"; transaction: FinalizedTransaction }>
  | Readonly<{ kind: "expired"; proof: AttemptedExpiryProof }>;

async function waitForFinalizedSignature(input: Readonly<{
  rpcUrl: string;
  signature: string;
  lastValidBlockHeight: number;
}>, dependencies: JournaledStepDependencies): Promise<FinalizedWaitResult> {
  const readSignature = dependencies.readFinalizedSignatureStatus ?? readFinalizedSignatureStatus;
  const currentBlockHeight = dependencies.currentBlockHeight
    ?? (async (rpcUrl: string) => rpcWithRetry<number>("getBlockHeight", [{ commitment: "finalized" }]));
  const sleep = dependencies.sleep
    ?? ((milliseconds: number) => new Promise<void>((resolve) => setTimeout(resolve, milliseconds)));
  let consecutiveErrors = 0;
  let consecutiveAbsent: FinalizedAbsentRead[] = [];

  for (let poll = 1; poll <= FINALIZED_WAIT_MAX_POLLS; poll += 1) {
    const status = await readSignature(input.rpcUrl, input.signature);
    if (status.kind === "finalized") {
      if (status.err !== null && status.err !== undefined) {
        throw new Error(`FINALIZED_WAIT_SIGNATURE_ERROR: finalized transaction ${input.signature} has error ${JSON.stringify(status.err)}`);
      }
      return { kind: "finalized", transaction: status.transaction };
    }
    if (status.kind === "error") {
      consecutiveErrors += 1;
      consecutiveAbsent = [];
      if (consecutiveErrors >= FINALIZED_WAIT_ERROR_POLLS) {
        throw new Error(
          `FINALIZED_WAIT_UNREADABLE: finalized signature ${input.signature} returned ${consecutiveErrors} consecutive errors; reconcile the attempted journal read-only (${status.message})`,
        );
      }
    } else {
      consecutiveErrors = 0;
      const absentRead = {
        kind: "absent" as const,
        poll,
        observedAtUnixMs: Date.now(),
      };
      consecutiveAbsent = [...consecutiveAbsent, absentRead].slice(-2);
      let finalizedBlockHeight: number;
      try {
        finalizedBlockHeight = await currentBlockHeight(input.rpcUrl);
      } catch (error) {
        throw new Error(
          `FINALIZED_WAIT_UNREADABLE: finalized block height read failed while waiting for ${input.signature}: ${sanitizeError(error)}`,
        );
      }
      if (consecutiveAbsent.length === 2
        && finalizedBlockHeight > input.lastValidBlockHeight + ATTEMPTED_EXPIRY_RECHECK_MARGIN_BLOCKS) {
        return {
          kind: "expired",
          proof: {
            lastValidBlockHeight: input.lastValidBlockHeight,
            finalizedBlockHeight,
            absentReads: consecutiveAbsent as unknown as AttemptedExpiryProof["absentReads"],
          },
        };
      }
    }
    if (poll < FINALIZED_WAIT_MAX_POLLS) {
      await sleep(FINALIZED_WAIT_POLL_INTERVAL_MS);
    }
  }
  throw new Error(
    `FINALIZED_WAIT_TIMEOUT: finalized signature ${input.signature} did not finalize within ${FINALIZED_WAIT_MAX_POLLS} polls; reconcile the attempted journal read-only`,
  );
}

async function completeAttemptedExpiryAbort(input: Readonly<{
  step: string;
  journal: string;
}>, dependencies: JournaledStepDependencies, stateRoot: string,
pending: JsonRecord, expectedGeneration: number, proof: AttemptedExpiryProof,
pendingPath?: string): Promise<Readonly<{
  state: JsonRecord;
  generation: number;
  abortedJournal: string;
}>> {
  const existingLegacyArtifact = interruptedAbortedJournalPaths(input.journal).find((artifact) =>
    artifact.filenameAttemptToken === String(pending.attemptToken)
      && artifact.record?.abortReason === ATTEMPTED_EXPIRY_REASON,
  );
  const rearmable = existingLegacyArtifact === undefined;
  const reason = rearmable ? PROVEN_ATTEMPTED_EXPIRY_REASON : ATTEMPTED_EXPIRY_REASON;
  const attemptedGeneration = expectedGeneration;
  const abortedGeneration = markCanonicalLegState(input.step, input.journal, {
    status: "aborted-pre-send",
    broadcast: "attempted",
    abortReason: reason,
    ...(rearmable ? { rearmable: true, attemptedExpiryProof: proof } : {}),
    attemptGeneration: attemptedGeneration,
  }, expectedGeneration, stateRoot);
  await faultAfterTransitionStep(dependencies, "attempted-expiry-state");
  const abortedState = readCanonicalLegState(input.step, false, stateRoot, { allowRollForward: true })!;
  const abortedPath = publishAbortArtifact(
    input.journal,
    abortedState,
    pending,
    reason,
    pendingPath,
    "attempted",
    rearmable ? {
      rearmable: true,
      attemptedExpiryProof: proof,
      lastValidBlockHeight: proof.lastValidBlockHeight,
      finalizedBlockHeight: proof.finalizedBlockHeight,
    } : {},
  );
  await faultAfterTransitionStep(dependencies, "attempted-expiry-artifact");
  const markedState = readCanonicalLegState(input.step, false, stateRoot, { allowRollForward: true })!;
  const generation = String(markedState.abortedJournal ?? "") === abortedPath
    ? canonicalStateGeneration(markedState)
    : markCanonicalLegState(input.step, input.journal, {
        status: "aborted-pre-send",
        broadcast: "attempted",
        abortReason: reason,
        ...(rearmable ? { rearmable: true, attemptedExpiryProof: proof } : {}),
        attemptGeneration: attemptedGeneration,
        abortedJournal: abortedPath,
      }, abortedGeneration, stateRoot);
  await faultAfterTransitionStep(dependencies, "attempted-expiry-marker");
  return {
    state: readCanonicalLegState(input.step, false, stateRoot, { allowRollForward: true })!,
    generation,
    abortedJournal: abortedPath,
  };
}

async function completeProvenExpiryPublication(input: Readonly<{
  step: string;
  canonicalJournal: string;
  state: JsonRecord;
  expectedGeneration: number;
}>, dependencies: JournaledStepDependencies, stateRoot: string): Promise<Readonly<{
  state: JsonRecord;
  generation: number;
  abortedJournal: string;
}>> {
  const canonicalJournal = input.canonicalJournal;
  const state = input.state;
  const proof = provenExpiryProof(state.attemptedExpiryProof, "canonical state");
  const pending = recordAt(state.pendingRecord, `${input.step} canonical pendingRecord`);
  const attemptToken = stringAt(state.attemptToken, `${input.step} proven attemptToken`);
  if (String(pending.attemptToken ?? "") !== attemptToken) {
    throw new Error("PROVEN_ATTEMPTED_EXPIRY_RECORD_INVALID: pending record has a different attempt token");
  }
  if (String(state.journal ?? "") !== canonicalJournal) {
    throw new Error("PROVEN_ATTEMPTED_EXPIRY_RECORD_INVALID: canonical journal changed during publication recovery");
  }
  const abortedPath = publishAbortArtifact(
    canonicalJournal,
    state,
    pending,
    PROVEN_ATTEMPTED_EXPIRY_REASON,
    existsSync(`${canonicalJournal}.pending`) ? `${canonicalJournal}.pending` : undefined,
    "attempted",
    {
      rearmable: true,
      attemptedExpiryProof: proof,
      lastValidBlockHeight: proof.lastValidBlockHeight,
      finalizedBlockHeight: proof.finalizedBlockHeight,
    },
  );
  await faultAfterTransitionStep(dependencies, "attempted-expiry-artifact");
  const markedState = readCanonicalLegState(input.step, false, stateRoot, { allowRollForward: true })!;
  const generation = String(markedState.abortedJournal ?? "") === abortedPath
    ? canonicalStateGeneration(markedState)
    : markCanonicalLegState(input.step, canonicalJournal, {
        status: "aborted-pre-send",
        broadcast: "attempted",
        abortReason: PROVEN_ATTEMPTED_EXPIRY_REASON,
        rearmable: true,
        attemptedExpiryProof: proof,
        attemptGeneration: Number(state.attemptGeneration ?? state.generation),
        abortedJournal: abortedPath,
      }, input.expectedGeneration, stateRoot);
  await faultAfterTransitionStep(dependencies, "attempted-expiry-marker");
  const completedState = readCanonicalLegState(input.step, false, stateRoot, { allowRollForward: true })!;
  const artifact = readBoundJournal(abortedPath).record;
  assertProvenExpiryPublication(
    completedState,
    artifact,
    abortedPath,
    canonicalJournal,
    attemptToken,
    proof,
  );
  return {
    state: completedState,
    generation,
    abortedJournal: abortedPath,
  };
}

async function reconcilePublishedJournal(
  input: Parameters<typeof runJournaledStep>[0],
  dependencies: JournaledStepDependencies,
  stateRoot: string,
  state: JsonRecord,
  loadFinalized: typeof finalizedTransaction,
): Promise<Readonly<{ state: JsonRecord; generation: number }>> {
  if (String(state.journal ?? "") !== input.journal) {
    throw new Error(`STATE_GENERATION_CONFLICT: ${input.step} finalized journal is not bound to canonical state`);
  }
  const journalHash = finalizedJournalSha256(input.journal);
  if (state.status === "finalized") {
    if (String(state.finalizedJournalSha256 ?? "") !== journalHash) {
      throw new Error(`RECONCILE_MISMATCH: finalized ${input.step} journal hash is not bound to canonical state`);
    }
    return { state, generation: stateGeneration(state)! };
  }
  if (state.status !== "attempted") {
    throw new Error(`STATE_GENERATION_CONFLICT: ${input.step} finalized journal is not backed by an attempted canonical record`);
  }
  const finalizedRecord = readBoundJournal(input.journal).record;
  const wire = journalWire(finalizedRecord, input.schema, input.step, "finalized");
  const finalized = await loadFinalized(input.rpcUrl, wire.signature);
  assertFinalizedJournalMessage(wire, finalized);
  await input.reconcile({ pending: finalizedRecord, finalized });
  const generation = markCanonicalLegState(input.step, input.journal, {
    status: "finalized",
    broadcast: true,
    signature: wire.signature,
    finalizedJournalSha256: journalHash,
  }, stateGeneration(state), stateRoot);
  await faultAfterTransitionStep(dependencies, "finalized-state");
  return {
    state: readCanonicalLegState(input.step, false, stateRoot, { allowRollForward: true })!,
    generation,
  };
}

function recordAt(value: unknown, key: string): JsonRecord {
  const record = value && typeof value === "object" ? value as JsonRecord : null;
  if (!record) throw new Error(`journal field ${key} is not an object`);
  return record;
}

function stringAt(value: unknown, label: string): string {
  if (typeof value !== "string" || value.length === 0) {
    throw new Error(`journal field ${label} is missing`);
  }
  return value;
}

function optionalBigintAt(value: unknown, label: string): bigint | null {
  return value === null || value === undefined ? null : BigInt(stringAt(value, label));
}

function cancelProofPreStateFromRecord(before: JsonRecord): HxtkCancelProofPreState {
  const requestReceipt = before.requestReceipt === null
    ? null
    : recordAt(before.requestReceipt, "cancel journal before.requestReceipt");
  return {
    amountLpEscrowed: optionalBigintAt(requestReceipt?.amountLpEscrowed, "cancel journal before.requestReceipt.amountLpEscrowed"),
    amountAssetToWithdrawRaw: optionalBigintAt(
      requestReceipt?.amountAssetToWithdrawRaw,
      "cancel journal before.requestReceipt.amountAssetToWithdrawRaw",
    ),
    totalValue: optionalBigintAt(before.totalValue, "cancel journal before.totalValue"),
    idleBalance: optionalBigintAt(before.idleBalance, "cancel journal before.idleBalance"),
    lpSupply: optionalBigintAt(before.lpSupply, "cancel journal before.lpSupply"),
    adminLpBalance: optionalBigintAt(before.adminLpBalance, "cancel journal before.adminLpBalance"),
    receipt1PositionValue: optionalBigintAt(
      before.receipt1PositionValue,
      "cancel journal before.receipt1PositionValue",
    ),
  };
}

function assertBase64Bytes(value: string, label: string): Uint8Array {
  const bytes = Uint8Array.from(Buffer.from(value, "base64"));
  if (Buffer.from(bytes).toString("base64") !== value) {
    throw new Error(`journal field ${label} is not canonical base64`);
  }
  return bytes;
}

function journalWire(
  journal: JsonRecord,
  expectedSchema: string,
  expectedStep: string,
  lifecycle: "pending" | "finalized",
) {
  if (journal.schema !== expectedSchema || journal.step !== expectedStep) {
    throw new Error(`journal is not the ${expectedStep} ${expectedSchema} schema`);
  }
  const transaction = recordAt(journal.transaction, "transaction");
  const signature = stringAt(transaction.expectedSignature, "transaction.expectedSignature");
  const signedWireBase64 = stringAt(journal.signedWireBase64, "signedWireBase64");
  const messageBase64 = stringAt(transaction.messageBase64, "transaction.messageBase64");
  const wire = assertBase64Bytes(signedWireBase64, "signedWireBase64");
  const message = assertBase64Bytes(messageBase64, "transaction.messageBase64");
  const wireSha256 = createHash("sha256").update(wire).digest("hex");
  const messageSha256 = createHash("sha256").update(message).digest("hex");
  if (transaction.wireSha256 !== wireSha256) {
    throw new Error("pending journal signed wire hash does not match signedWireBase64");
  }
  if (transaction.messageSha256 !== messageSha256) {
    throw new Error("pending journal message hash does not match messageBase64");
  }
  const signedTransaction = VersionedTransaction.deserialize(Buffer.from(wire));
  const signedMessage = signedTransaction.message.serialize();
  if (!Buffer.from(signedMessage).equals(Buffer.from(message))) {
    throw new Error("journal signed wire message is not byte-identical to transaction.messageBase64");
  }
  const signedSignature = signedTransaction.signatures[0]
    ? bs58.encode(signedTransaction.signatures[0])
    : null;
  if (signedSignature !== signature) {
    throw new Error(`journal signed wire signature ${signedSignature ?? "absent"} differs from ${signature}`);
  }
  if (lifecycle === "pending"
    && (journal.sent !== false
      || (journal.broadcast !== false && journal.broadcast !== "attempted")
      || journal.signed !== true)) {
    throw new Error("pending journal lifecycle flags are not signed-but-unsent");
  }
  if (lifecycle === "finalized"
    && (journal.verdict !== "FINALIZED_RECONCILED"
      || journal.sent !== true || journal.signed !== true || journal.broadcast !== true)) {
    throw new Error("finalized journal lifecycle flags are not finalized-reconciled");
  }
  if (lifecycle === "finalized" && journal.signature !== signature) {
    throw new Error("finalized journal top-level signature differs from transaction.expectedSignature");
  }
  if (lifecycle === "pending" && journal.signature !== undefined && journal.signature !== signature) {
    throw new Error("pending journal top-level signature differs from transaction.expectedSignature");
  }
  return { transaction, signature, wire, message, wireSha256, messageSha256, signedTransaction } as const;
}

function assertFinalizedJournalMessage(
  wire: ReturnType<typeof journalWire>,
  finalized: FinalizedTransaction,
) {
  const observedSignature = finalized.transaction.signatures[0];
  if (observedSignature !== wire.signature) {
    throw new Error(`finalized transaction signature ${observedSignature ?? "absent"} differs from journal ${wire.signature}`);
  }
  const finalizedMessage = finalized.transaction.message.serialize();
  if (!Buffer.from(finalizedMessage).equals(Buffer.from(wire.message))) {
    throw new Error("finalized transaction message is not byte-identical to the journal message");
  }
  const finalizedMessageSha256 = createHash("sha256").update(finalizedMessage).digest("hex");
  if (finalizedMessageSha256 !== wire.messageSha256) {
    throw new Error("finalized transaction message hash differs from the journal");
  }
}

function assertFinalizedJournalBound(step: string, path: string): Readonly<{
  record: JsonRecord;
  sha256: string;
}> {
  let bound: ReturnType<typeof readBoundJournal>;
  try {
    bound = readBoundJournal(path);
  } catch {
    throw new Error(`RECONCILE_MISMATCH: finalized ${step} journal is not bound to the canonical fence`);
  }
  assertFinalizedJournalBinding({
    step,
    journal: path,
    canonicalState: readCanonicalLegState(step),
    journalSha256: bound.sha256,
  });
  return bound;
}

async function readFinalizedJournal(
  rpcUrl: string,
  path: string,
  schema: string,
  step: string,
) {
  const parsed = assertFinalizedJournalBound(step, path).record;
  const wire = journalWire(parsed, schema, step, "finalized");
  const finalized = await finalizedTransaction(rpcUrl, wire.signature);
  assertFinalizedJournalMessage(wire, finalized);
  if (parsed.finalizedSlot !== undefined && Number(parsed.finalizedSlot) !== finalized.slot) {
    throw new Error(`${step} journal finalizedSlot differs from the chain transaction slot`);
  }
  if (parsed.finalizedBlockTime !== undefined
    && parsed.finalizedBlockTime !== null
    && Number(parsed.finalizedBlockTime) !== finalized.blockTime) {
    throw new Error(`${step} journal finalizedBlockTime differs from the chain transaction`);
  }
  return { record: parsed, wire, finalized } as const;
}

type FinalizedCancelJournal = Readonly<{
  path: string;
  record: JsonRecord;
  finalized: FinalizedTransaction;
  expectedRefundLp: bigint;
  expectedBurnLp: bigint;
  expectedSupplyAfter: bigint;
  beforeAdminLp: bigint;
  adminLpDelta: bigint;
}>;

async function verifyFinalizedCancelJournal(
  rpcUrl: string,
  path: string,
): Promise<FinalizedCancelJournal> {
  const result = await readFinalizedJournal(rpcUrl, path, SCHEMA, "cancel");
  const before = recordAt(result.record.before, "cancel journal before");
  const cancel = recordAt(result.record.cancel, "cancel journal cancel");
  const finalizedState = recordAt(result.record.finalizedState, "cancel journal finalizedState");
  const preState = cancelProofPreStateFromRecord(before);
  assertHxtkCancelProofPreState(preState, "finalized cancel journal before");
  const expectedRefundLp = BigInt(stringAt(
    cancel.expectedRefundLp ?? cancel.escrowRefundLp,
    "cancel.expectedRefundLp",
  ));
  const expectedBurnLp = BigInt(stringAt(cancel.expectedBurnLp, "cancel.expectedBurnLp"));
  const expectedSupplyAfter = BigInt(stringAt(cancel.expectedSupplyAfter, "cancel.expectedSupplyAfter"));
  if (String(result.record.requestReceiptPda ?? cancel.requestReceipt ?? "") !== REQUEST_RECEIPT
    || cancel.originalFrozenReceipt !== true
    || expectedRefundLp !== CANCEL_EXPECTED_REFUND_LP
    || expectedBurnLp !== CANCEL_EXPECTED_BURN_LP
    || expectedSupplyAfter !== CANCEL_EXPECTED_SUPPLY_AFTER) {
    throw new Error("RECONCILE_MISMATCH: finalized cancel journal is not pinned to the reset proof receipt and numbers");
  }
  const beforeAdminLp = BigInt(String(before.adminLpBalance ?? "0"));
  const finalizedAdminLp = BigInt(stringAt(finalizedState.adminLpBalance, "cancel journal finalizedState.adminLpBalance"));
  const adminLpDelta = finalizedAdminLp - beforeAdminLp;
  const finalizedReceipt = finalizedState.requestReceipt === null
    ? null
    : recordAt(finalizedState.requestReceipt, "cancel journal finalized requestReceipt");
  if (finalizedReceipt !== null
    && (String(finalizedReceipt.vault) !== VAULT
      || String(finalizedReceipt.userTransferAuthority) !== ADMIN)) {
    throw new Error("RECONCILE_MISMATCH: finalized cancel receipt identity drifted");
  }
  const events = result.finalized.meta?.logMessages
    ? decodeEvents("CancelRequestWithdrawVault", result.finalized.meta.logMessages)
    : [];
  const event = events.length === 1
    ? events[0] as { amountLpRefunded?: bigint; amountLpBurned?: bigint }
    : null;
  assertHxtkCancelSimulation({
    preState,
    simulationSucceeded: true,
    cancelEventCount: events.length,
    eventRefundLp: event?.amountLpRefunded ?? null,
    eventBurnLp: event?.amountLpBurned ?? null,
    escrowAfter: optionalBigintAt(finalizedState.requestEscrowLpBalance, "cancel journal finalizedState.requestEscrowLpBalance"),
    requestReceiptLpAfter: optionalBigintAt(
      finalizedReceipt?.amountLpEscrowed,
      "cancel journal finalized requestReceipt.amountLpEscrowed",
    ),
    adminLpDelta,
    adminLpAfter: optionalBigintAt(finalizedState.adminLpBalance, "cancel journal finalizedState.adminLpBalance"),
    supplyAfter: optionalBigintAt(finalizedState.lpSupply, "cancel journal finalizedState.lpSupply"),
    totalValueAfter: optionalBigintAt(finalizedState.totalValue, "cancel journal finalizedState.totalValue"),
    idleBalanceAfter: optionalBigintAt(finalizedState.idleBalance, "cancel journal finalizedState.idleBalance"),
  }, "finalized cancel journal");
  return {
    path,
    record: result.record,
    finalized: result.finalized,
    expectedRefundLp,
    expectedBurnLp,
    expectedSupplyAfter,
    beforeAdminLp,
    adminLpDelta,
  };
}

type FinalizedRequestJournal = Readonly<{
  path: string;
  record: JsonRecord;
  finalized: FinalizedTransaction;
  requestBlockTime: number;
  chainExpectedWithdrawableFromTs: bigint;
  requestReceipt: JsonRecord;
  amountLpEscrowed: bigint;
  withdrawableFromTs: bigint;
}>;

async function verifyFinalizedRequestJournal(
  rpcUrl: string,
  path: string,
): Promise<FinalizedRequestJournal> {
  const result = await readFinalizedJournal(rpcUrl, path, SCHEMA, "request");
  const before = recordAt(result.record.before, "request journal before");
  const beforeReceipt = before.requestReceipt === null
    ? null
    : recordAt(before.requestReceipt, "request journal before.requestReceipt");
  assertHxtkPostCancelState({
    adminLpBalance: optionalBigintAt(before.adminLpBalance, "request journal before.adminLpBalance"),
    lpSupply: optionalBigintAt(before.lpSupply, "request journal before.lpSupply"),
    totalValue: optionalBigintAt(before.totalValue, "request journal before.totalValue"),
    idleBalance: optionalBigintAt(before.idleBalance, "request journal before.idleBalance"),
    receipt1PositionValue: optionalBigintAt(before.receipt1PositionValue, "request journal before.receipt1PositionValue"),
    requestEscrowLpBalance: optionalBigintAt(before.requestEscrowLpBalance, "request journal before.requestEscrowLpBalance"),
    requestReceiptLp: optionalBigintAt(beforeReceipt?.amountLpEscrowed, "request journal before.requestReceipt.amountLpEscrowed"),
  }, "finalized request journal before");
  if (String(result.record.requestReceiptPda ?? "") !== REQUEST_RECEIPT) {
    throw new Error("finalized request journal is not bound to the HXtk request receipt PDA");
  }
  const requestReceipt = recordAt(result.record.requestReceipt, "request journal requestReceipt");
  if (String(requestReceipt.vault) !== VAULT
    || String(requestReceipt.userTransferAuthority) !== ADMIN) {
    throw new Error("finalized request journal receipt is not the HXtk admin request receipt");
  }
  const amountLpEscrowed = BigInt(stringAt(requestReceipt.amountLpEscrowed, "requestReceipt.amountLpEscrowed"));
  if (amountLpEscrowed !== REQUEST_EXPECTED_LP) {
    throw new Error(
      `RECONCILE_MISMATCH: finalized request amount ${amountLpEscrowed} is not the proof-pinned ${REQUEST_EXPECTED_LP}`,
    );
  }
  const withdrawableFromTs = BigInt(stringAt(requestReceipt.withdrawableFromTs, "requestReceipt.withdrawableFromTs"));
  const requestBlockTime = result.finalized.blockTime;
  if (requestBlockTime === null || requestBlockTime === undefined) {
    throw new Error("finalized request transaction has no blockTime; cannot reconcile the chain waiting period");
  }
  const chainExpectedWithdrawableFromTs = BigInt(requestBlockTime) + REQUEST_WAITING_PERIOD_SECONDS;
  const waitingPeriodDelta = withdrawableFromTs - chainExpectedWithdrawableFromTs;
  if (waitingPeriodDelta < -1n || waitingPeriodDelta > 1n) {
    throw new Error(
      `finalized request withdrawableFromTs ${withdrawableFromTs} does not equal request blockTime ${requestBlockTime} + `
      + `${REQUEST_WAITING_PERIOD_SECONDS} (${chainExpectedWithdrawableFromTs}); observed delta ${waitingPeriodDelta}`,
    );
  }
  if (amountLpEscrowed <= 0n) throw new Error("finalized request journal has no escrowed LP");
  const events = result.finalized.meta?.logMessages
    ? decodeEvents("RequestWithdrawVault", result.finalized.meta.logMessages)
    : [];
  const event = events.length === 1
    ? events[0] as {
        vault?: Address;
        user?: Address;
        requestedAmount?: bigint;
        isAmountInLp?: boolean;
        isWithdrawAll?: boolean;
        requestWithdrawVaultReceipt?: Address;
        amountLpEscrowed?: bigint;
        withdrawableFromTs?: bigint;
      }
    : null;
  if (events.length !== 1
    || event?.vault?.toString() !== VAULT
    || event.user?.toString() !== ADMIN
    || event.requestedAmount !== REQUEST_EXPECTED_LP
    || event.isAmountInLp !== true
    || event.isWithdrawAll !== true
    || event.requestWithdrawVaultReceipt?.toString() !== REQUEST_RECEIPT
    || event.amountLpEscrowed !== REQUEST_EXPECTED_LP
    || event.withdrawableFromTs !== withdrawableFromTs) {
    throw new Error("RECONCILE_MISMATCH: finalized request event is not the proof-pinned all-LP request");
  }
  const finalizedState = recordAt(result.record.finalizedState, "request journal finalizedState");
  if (String(finalizedState.adminLpBalance) !== "0"
    || String(finalizedState.requestEscrowLpBalance) !== amountLpEscrowed.toString()
    || String(finalizedState.lpSupply) !== REQUEST_EXPECTED_LP.toString()) {
    throw new Error("finalized request journal post-state does not bind the new request receipt");
  }
  return {
    path,
    record: result.record,
    finalized: result.finalized,
    requestBlockTime,
    chainExpectedWithdrawableFromTs,
    requestReceipt,
    amountLpEscrowed,
    withdrawableFromTs,
  };
}

function claimExpectedRaw(value: unknown, label: string): bigint {
  try {
    return BigInt(stringAt(value, label));
  } catch (error) {
    throw new Error(`RECONCILE_MISMATCH: ${sanitizeError(error)}`);
  }
}

async function verifyFinalizedClaimJournal(
  rpcUrl: string,
  path: string,
) {
  const result = await readFinalizedJournal(rpcUrl, path, SCHEMA, "claim");
  const finalizedState = recordAt(result.record.finalizedState, "claim journal finalizedState");
  if (finalizedState.requestReceipt !== null || String(finalizedState.requestEscrowLpBalance) !== "0") {
    throw new Error("RECONCILE_MISMATCH: finalized claim journal does not prove request closure and escrow drain");
  }
  const before = recordAt(result.record.before, "claim journal before");
  const claim = recordAt(result.record.claim, "claim journal claim");
  const reconciliation = recordAt(result.record.claimReconciliation, "claim journal claimReconciliation");
  if (String(result.record.requestReceiptPda ?? claim.requestReceipt ?? "") !== REQUEST_RECEIPT) {
    throw new Error("RECONCILE_MISMATCH: finalized claim journal is not bound to the request receipt in the request journal");
  }
  const expectedPayoutRaw = claimExpectedRaw(
    result.record.expectedPayoutRaw ?? claim.expectedPayoutRaw,
    "claim.expectedPayoutRaw",
  );
  const expectedLpBurnRaw = claimExpectedRaw(
    result.record.expectedLpBurnRaw ?? claim.expectedLpBurnRaw,
    "claim.expectedLpBurnRaw",
  );
  if (expectedPayoutRaw !== CLAIM_EXPECTED_PAYOUT_RAW || expectedLpBurnRaw !== REQUEST_EXPECTED_LP) {
    throw new Error("RECONCILE_MISMATCH: finalized claim journal expected payout or burn is not proof-pinned");
  }
  const payoutRaw = BigInt(stringAt(reconciliation.payoutRaw, "claimReconciliation.payoutRaw"));
  const idleDeltaRaw = BigInt(stringAt(reconciliation.idleDeltaRaw, "claimReconciliation.idleDeltaRaw"));
  const tvAfter = BigInt(stringAt(reconciliation.tvAfter, "claimReconciliation.tvAfter"));
  const lpBurnedRaw = BigInt(stringAt(reconciliation.lpBurnedRaw, "claimReconciliation.lpBurnedRaw"));
  const lpSupplyAfter = BigInt(stringAt(reconciliation.lpSupplyAfter, "claimReconciliation.lpSupplyAfter"));
  const requestAmountLp = BigInt(stringAt(claim.requestAmountLp, "claim.requestAmountLp"));
  const amountAssetToWithdrawRaw = BigInt(stringAt(
    claim.amountAssetToWithdrawRaw,
    "claim.amountAssetToWithdrawRaw",
  ));
  const beforeAdminUsdc = BigInt(stringAt(before.adminUsdcBalance, "claim journal before.adminUsdcBalance"));
  const finalizedAdminUsdc = BigInt(stringAt(finalizedState.adminUsdcBalance, "claim journal finalizedState.adminUsdcBalance"));
  const beforeIdle = BigInt(stringAt(before.idleBalance, "claim journal before.idleBalance"));
  const finalizedIdle = BigInt(stringAt(finalizedState.idleBalance, "claim journal finalizedState.idleBalance"));
  const beforeTv = BigInt(stringAt(before.totalValue, "claim journal before.totalValue"));
  const finalizedTv = BigInt(stringAt(finalizedState.totalValue, "claim journal finalizedState.totalValue"));
  const beforeSupply = BigInt(stringAt(before.lpSupply, "claim journal before.lpSupply"));
  const finalizedSupply = BigInt(stringAt(finalizedState.lpSupply, "claim journal finalizedState.lpSupply"));
  const beforeReceipt1 = optionalBigintAt(before.receipt1PositionValue, "claim journal before.receipt1PositionValue");
  const finalizedReceipt1 = optionalBigintAt(
    finalizedState.receipt1PositionValue,
    "claim journal finalizedState.receipt1PositionValue",
  );
  const beforeTicket = before.reportTicket;
  const finalizedTicket = finalizedState.reportTicket;
  assertHxtkClaimProof({
    preState: {
      requestAmountLp,
      totalValue: beforeTv,
      idleBalance: beforeIdle,
      lpSupply: beforeSupply,
      receipt1PositionValue: beforeReceipt1,
    },
    payout: payoutRaw,
    lpBurned: lpBurnedRaw,
    totalValueAfter: finalizedTv,
    idleBalanceAfter: finalizedIdle,
    receipt1PositionValueAfter: finalizedReceipt1,
    requestReceiptClosed: reconciliation.requestReceiptClosed === true,
    escrowAfter: reconciliation.escrowLpBalanceAfter === "0" ? 0n : null,
  }, "finalized claim journal");
  const events = result.finalized.meta?.logMessages
    ? decodeEvents("WithdrawVault", result.finalized.meta.logMessages)
    : [];
  const event = events.length === 1
    ? events[0] as {
        user?: Address;
        userAmountAssetWithdrawn?: bigint;
        userAmountLpBurned?: bigint;
        vault?: Address;
        vaultAssetTotalValueAfter?: bigint;
      }
    : null;
  if (payoutRaw < 1n
    || payoutRaw !== expectedPayoutRaw
    || finalizedAdminUsdc - beforeAdminUsdc !== payoutRaw
    || payoutRaw > amountAssetToWithdrawRaw
    || idleDeltaRaw !== payoutRaw
    || finalizedIdle !== beforeIdle - payoutRaw
    || tvAfter !== beforeTv - payoutRaw
    || finalizedTv !== tvAfter
    || lpBurnedRaw !== expectedLpBurnRaw
    || lpBurnedRaw !== requestAmountLp
    || finalizedSupply !== beforeSupply - lpBurnedRaw
    || lpSupplyAfter !== finalizedSupply
    || reconciliation.requestReceiptClosed !== true
    || reconciliation.escrowLpBalanceAfter !== "0"
    || reconciliation.ticketUnchanged !== true
    || toJson(beforeTicket) !== toJson(finalizedTicket)
    || events.length !== 1
    || event?.user?.toString() !== ADMIN
    || event.userAmountAssetWithdrawn !== CLAIM_EXPECTED_PAYOUT_RAW
    || event.userAmountLpBurned !== REQUEST_EXPECTED_LP
    || event.vault?.toString() !== VAULT
    || event.vaultAssetTotalValueAfter !== CLAIM_EXPECTED_RESIDUAL_RAW) {
    throw new Error("RECONCILE_MISMATCH: finalized claim journal does not prove the exact simulated payout, book, LP burn, closure, and ticket invariants");
  }
  return { ...result, path } as const;
}

type RestorePrerequisites = Readonly<{
  repairJournal: FinalizedRepairJournal;
  claimJournal: Awaited<ReturnType<typeof verifyFinalizedClaimJournal>>;
  repairBlockTime: number;
  eligibleAt: number;
  eligibilityObservationSlot: number;
  eligibilityChainTime: number;
}>;

async function latestFinalizedChainTime(rpcUrl: string): Promise<Readonly<{
  slot: number;
  blockTime: number;
}>> {
  // Eligibility is a chain-time decision. Read the latest finalized slot and
  // its bank timestamp together; wall-clock Date.now() is not authoritative
  // for a transaction that will be admitted by the cluster.
  const slot = await rpcWithRetry<number>("getSlot", [{ commitment: "finalized" }]);
  const blockTime = await rpcWithRetry<number | null>("getBlockTime", [slot]);
  if (blockTime === null || blockTime === undefined) {
    throw new Error(`latest finalized slot ${slot} has no blockTime`);
  }
  return { slot, blockTime };
}

async function readRestorePrerequisites(
  rpcUrl: string,
  paths?: Readonly<{
    repairJournal?: string;
    claimJournal?: string;
    enforceEligibility?: boolean;
  }>,
): Promise<RestorePrerequisites> {
  await assertRepairPolicyRetired("restore-degradation");
  const repairPath = paths?.repairJournal ?? requiredJournalPath(
    REPAIR_JOURNAL_FLAG,
    "restore-degradation requires the finalized repair journal",
  );
  const repairRecord = assertFinalizedJournalBound("repair", repairPath).record;
  const policyJournal = resolve(stringAt(repairRecord.policyJournal, "repair journal policyJournal"));
  const repairJournal = await verifyFinalizedRepairJournal(rpcUrl, repairPath, policyJournal);
  const claimPath = paths?.claimJournal ?? requiredJournalPath(
    CLAIM_JOURNAL_FLAG,
    "restore-degradation requires the finalized claim journal",
  );
  const claimJournal = await verifyFinalizedClaimJournal(rpcUrl, claimPath);
  if (claimJournal.finalized.slot <= repairJournal.finalized.slot) {
    throw new Error("restore-degradation claim journal must finalize after the repair journal");
  }
  const blockTime = repairJournal.finalized.blockTime;
  if (blockTime === null || blockTime === undefined) {
    throw new Error("RESTORE_DEGRADATION_BLOCKED: finalized repair transaction has no blockTime");
  }
  const repairBlockTime: number = blockTime;
  const eligibleAt = repairBlockTime + Number(RESTORED_DEGRADATION_SECONDS);
  const eligibility = await latestFinalizedChainTime(rpcUrl);
  if (paths?.enforceEligibility !== false && eligibleAt > eligibility.blockTime) {
    throw new Error(
      `RESTORE_DEGRADATION_WAIT: repair blockTime ${repairBlockTime} + ${RESTORED_DEGRADATION_SECONDS}s `
      + `= ${eligibleAt}, latest finalized slot ${eligibility.slot} blockTime ${eligibility.blockTime}`,
    );
  }
  return {
    repairJournal,
    claimJournal,
    repairBlockTime,
    eligibleAt,
    eligibilityObservationSlot: eligibility.slot,
    eligibilityChainTime: eligibility.blockTime,
  };
}

async function runJournaledStep(input: Readonly<{
  mode: RepairPolicyOperatorMode;
  step: string;
  schema: string;
  journal: string;
  rpcUrl: string;
  build: () => Promise<JournaledExecutionBuild>;
  reconcile: (input: Readonly<{
    pending: JsonRecord;
    finalized: FinalizedTransaction;
  }>) => Promise<JsonRecord>;
}>, dependencies: JournaledStepDependencies = {}): Promise<number> {
  const stateRoot = dependencies.stateRoot
    ?? resolveCanonicalStateRoot({ vault: VAULT.toString(), create: true });
  const preClaimState = readCanonicalLegState(input.step, false, stateRoot, { allowRollForward: false });
  if (input.mode === "execute"
    && preClaimState?.status === "aborted-pre-send"
    && preClaimState.abortReason === ATTEMPTED_EXPIRY_REASON
    && String(preClaimState.journal ?? "") !== input.journal) {
    console.log(toJson({
      verdict: "JOURNAL_MISMATCH_ATTEMPTED_EXPIRED",
      rearmable: false,
      journal: input.journal,
      canonicalJournal: String(preClaimState.journal ?? ""),
      recoveryInstruction: buildHxtkRecoveryCommand({
        step: input.step,
        mode: "reconcile",
        journal: String(preClaimState.journal ?? ""),
        finalized: false,
      }),
      reason: "the attempted-expired leg remains bound to its original journal; reconcile that journal before any new execute",
    }, 2));
    return 0;
  }
  // The attempted-expired ownership precheck above is the sole canonical
  // pointer read allowed before the claim; after it, the claim remains held
  // through barrier reads, build, simulation, send, reconciliation, and every
  // canonical state write.
  const claim = acquireCanonicalLegClaim({
    stateRoot,
    step: input.step,
    journal: input.journal,
    breakClaim: currentCli().has("--break-claim"),
  });
  try {
    return await runJournaledStepHeld(input, dependencies);
  } finally {
    releaseCanonicalLegClaim(claim);
  }
}

async function runJournaledStepHeld(input: Readonly<{
  mode: RepairPolicyOperatorMode;
  step: string;
  schema: string;
  journal: string;
  rpcUrl: string;
  build: () => Promise<JournaledExecutionBuild>;
  reconcile: (input: Readonly<{
    pending: JsonRecord;
    finalized: FinalizedTransaction;
  }>) => Promise<JsonRecord>;
}>, dependencies: JournaledStepDependencies = {}): Promise<number> {
  const stateRoot = dependencies.stateRoot ?? canonicalStateRoot(true);
  const sendOnce = dependencies.sendPreparedOnce ?? sendPreparedOnce;
  const loadFinalized = dependencies.finalizedTransaction ?? finalizedTransaction;
  const readSignature = dependencies.readFinalizedSignatureStatus ?? readFinalizedSignatureStatus;
  const currentBlockHeight = dependencies.currentBlockHeight
    ?? (async (rpcUrl: string) => rpcWithRetry<number>("getBlockHeight", [{ commitment: "finalized" }]));
  const preResumeState = readCanonicalLegState(input.step, false, stateRoot, { allowRollForward: false });
  if (input.mode === "execute"
    && preResumeState?.status === "aborted-pre-send"
    && preResumeState.abortReason === ATTEMPTED_EXPIRY_REASON
    && String(preResumeState.journal ?? "") !== input.journal) {
    throw new Error(`JOURNAL_MISMATCH_ATTEMPTED_EXPIRED: reconcile the original journal ${preResumeState.journal}`);
  }
  const resumed = resumeInterruptedTransition({
    step: input.step,
    journal: input.journal,
    stateRoot,
    mode: input.mode,
  });
  let sectionState = readCanonicalLegState(input.step, false, stateRoot, { allowRollForward: true });
  const staleAbortArtifacts = [...resumed.staleAbortArtifacts];
  let expectedGeneration = stateGeneration(sectionState);
  if (sectionState?.status === "aborted-pre-send" && sectionState.abortReason === ATTEMPTED_EXPIRY_REASON) {
    const canonicalJournal = String(sectionState.journal ?? "");
    // An attempted-expired record remains bound to its original journal until
    // the expected signature has been reconciled. Refuse a different execute
    // journal before constructing records, reading the chain, or publishing an
    // artifact, so a failed re-arm cannot mutate the new journal.
    if (input.mode === "execute" && canonicalJournal !== input.journal) {
      throw new Error(
        `JOURNAL_MISMATCH_ATTEMPTED_EXPIRED: reconcile the original journal ${canonicalJournal} with ${buildHxtkRecoveryCommand({ step: input.step, mode: "reconcile", finalized: false })}`,
      );
    }
    const pending = recordAt(sectionState.pendingRecord, `${input.step} canonical pendingRecord`);
    const wire = journalWire(pending, input.schema, input.step, "pending");
    const signatureStatus = await readSignature(input.rpcUrl, wire.signature);
    const landed = signatureStatus.kind === "finalized" ? signatureStatus.transaction : undefined;
    if (landed !== undefined) {
      assertFinalizedJournalMessage(wire, landed);
      if (canonicalJournal !== input.journal) {
        console.log(toJson({
          schema: input.schema,
          step: input.step,
          verdict: "REARM_REFUSED_ATTEMPTED_EXPIRY_SIGNATURE_LANDED",
          reason: "the original attempted-expired signature is finalized; reconcile the original journal and do not create a second send",
          journal: input.journal,
          canonicalState: canonicalLegStatePath(input.step, false, stateRoot),
          finalizeInstruction: buildHxtkRecoveryCommand({ step: input.step, mode: "reconcile", finalized: true }),
          staleAbortArtifacts,
        }, 2));
        return 0;
      }
      const reconciliation = await input.reconcile({ pending, finalized: landed });
      writePrivate(input.journal, {
        ...pending,
        verdict: "FINALIZED_RECONCILED",
        sent: true,
        signed: true,
        broadcast: true,
        signature: wire.signature,
        finalizedSlot: landed.slot,
        finalizedBlockTime: landed.blockTime ?? null,
        ...reconciliation,
      }, "wx");
      expectedGeneration = markCanonicalLegState(input.step, input.journal, {
        status: "finalized",
        broadcast: true,
        signature: wire.signature,
        finalizedJournalSha256: finalizedJournalSha256(input.journal),
      }, expectedGeneration, stateRoot);
      console.log(toJson({
        schema: input.schema,
        step: input.step,
        verdict: "FINALIZED_RECONCILED",
        signature: wire.signature,
        finalizedSlot: landed.slot,
        journal: input.journal,
        canonicalState: canonicalLegStatePath(input.step, false, stateRoot),
        staleAbortArtifacts,
        recoveryCommand: buildHxtkRecoveryCommand({ step: input.step, mode: "reconcile", finalized: true }),
      }, 2));
      return 0;
    }
    if (signatureStatus.kind === "error" && input.mode === "execute" && String(sectionState.journal ?? "") !== input.journal) {
      console.log(toJson({
        schema: input.schema,
        step: input.step,
        verdict: "REARM_REFUSED_ATTEMPTED_EXPIRY_SIGNATURE_UNREADABLE",
        reason: "the attempted-expired signature remains unreadable after finalized-commitment recheck; reconcile the original journal before re-arming",
        journal: input.journal,
        canonicalState: canonicalLegStatePath(input.step, false, stateRoot),
        finalizeInstruction: buildHxtkRecoveryCommand({ step: input.step, mode: "reconcile", finalized: false }),
        staleAbortArtifacts,
      }, 2));
      return 0;
    }
    if (resumed.attemptedExpiryRecovered) {
      const pendingPath = `${input.journal}.pending`;
      const applied = publishAbortArtifact(
        input.journal,
        sectionState,
        pending,
        ATTEMPTED_EXPIRY_REASON,
        existsSync(pendingPath) ? pendingPath : undefined,
        "attempted",
      );
      if (String(sectionState.abortedJournal ?? "") !== applied) {
        markCanonicalLegState(input.step, input.journal, {
          status: "aborted-pre-send",
          broadcast: "attempted",
          abortReason: ATTEMPTED_EXPIRY_REASON,
          attemptGeneration: Number(sectionState.attemptGeneration ?? sectionState.generation),
          abortedJournal: applied,
        }, expectedGeneration, stateRoot);
      }
    }
  }
  if (new Set(["harvest", "cancel", "request", "claim", "restore-degradation"]).has(input.step)) {
    await assertRepairPolicyRetired(input.step);
  }
  if (resumed.autoAbortedPreSend) {
    console.log(toJson({
      schema: input.schema,
      step: input.step,
      verdict: "ABORTED_PRE_SEND_REARMABLE",
      reason: "canonical pending state was recovered before the attempted mark; rerun after reviewing the attempt-bound abort artifact",
      journal: input.journal,
      canonicalState: canonicalLegStatePath(input.step, false, stateRoot),
      abortedJournal: resumed.state?.abortedJournal ?? null,
      staleAbortArtifacts,
    }, 2));
    return 0;
  }
  if (resumed.attemptedExpiryRecovered) {
    console.log(toJson({
      schema: input.schema,
      step: input.step,
      verdict: "ABORTED_AFTER_BLOCKHASH_EXPIRY",
      rearmable: false,
      recoveryInstruction: buildHxtkRecoveryCommand({ step: input.step, mode: "reconcile", finalized: false }),
      journal: input.journal,
      canonicalState: canonicalLegStatePath(input.step, false, stateRoot),
      abortedJournal: resumed.state?.abortedJournal ?? null,
      staleAbortArtifacts,
    }, 2));
    return 0;
  }
  if (sectionState?.status === "aborted-pre-send"
    && sectionState.abortReason === ATTEMPTED_EXPIRY_REASON
    && String(sectionState.journal ?? "") === input.journal) {
    // This path does not permit a new journal; report that truthfully.
    console.log(toJson({
      schema: input.schema,
      step: input.step,
      verdict: "ABORTED_AFTER_BLOCKHASH_EXPIRY",
      rearmable: false,
      recoveryInstruction: buildHxtkRecoveryCommand({ step: input.step, mode: "reconcile", finalized: false }),
      journal: input.journal,
      canonicalState: canonicalLegStatePath(input.step, false, stateRoot),
      abortedJournal: sectionState.abortedJournal ?? null,
      staleAbortArtifacts,
    }, 2));
    return 0;
  }
  if (sectionState?.status === "aborted-pre-send"
    && sectionState.abortReason === PROVEN_ATTEMPTED_EXPIRY_REASON) {
    const canonicalJournal = String(sectionState.journal ?? "");
    const sameJournal = canonicalJournal === input.journal;
    const recoveryInstruction = provenExpiryRearmInstruction(input.step, canonicalJournal);
    const completesPreviousAttempt = sameJournal || input.mode === "execute";
    if (completesPreviousAttempt) {
      let completed: Readonly<{
        state: JsonRecord;
        generation: number;
        abortedJournal: string;
      }>;
      try {
        completed = await completeProvenExpiryPublication({
          step: input.step,
          canonicalJournal,
          state: sectionState,
          expectedGeneration: expectedGeneration!,
        }, dependencies, stateRoot);
      } catch (error) {
        if (!sameJournal) {
          throw new Error(
            `PROVEN_ATTEMPTED_EXPIRY_PUBLICATION_INCOMPLETE: ${sanitizeError(error)}; `
            + `reconcile the original journal with ${buildHxtkRecoveryCommand({
              step: input.step,
              mode: "reconcile",
              journal: canonicalJournal,
              finalized: false,
            })}`,
          );
        }
        throw error;
      }
      if (sameJournal) {
        if (input.mode === "execute") {
          throw new Error(
            `JOURNAL_MISMATCH_ATTEMPTED_EXPIRY_PROVEN: proven expiry is re-armable only with a new journal; ${recoveryInstruction}`,
          );
        }
        console.log(toJson({
          schema: input.schema,
          step: input.step,
          verdict: "ABORTED_AFTER_BLOCKHASH_EXPIRY_PROVEN",
          rearmable: true,
          abortReason: PROVEN_ATTEMPTED_EXPIRY_REASON,
          attemptedExpiryProof: sectionState.attemptedExpiryProof,
          recoveryInstruction,
          journal: input.journal,
          canonicalState: canonicalLegStatePath(input.step, false, stateRoot),
          abortedJournal: completed.abortedJournal,
          staleAbortArtifacts,
        }, 2));
        return 0;
      }
      // A new attempt may be elected only after the old proven record is fully
      // published and verified under this claim.
      sectionState = completed.state;
      expectedGeneration = completed.generation;
    }
    if (sameJournal && input.mode === "reconcile") {
      console.log(toJson({
        schema: input.schema,
        step: input.step,
        verdict: "ABORTED_AFTER_BLOCKHASH_EXPIRY_PROVEN",
        rearmable: true,
        abortReason: PROVEN_ATTEMPTED_EXPIRY_REASON,
        attemptedExpiryProof: sectionState.attemptedExpiryProof ?? null,
        recoveryInstruction,
        journal: input.journal,
        canonicalState: canonicalLegStatePath(input.step, false, stateRoot),
        abortedJournal: sectionState.abortedJournal ?? null,
        staleAbortArtifacts,
      }, 2));
      return 0;
    }
    if (sameJournal) {
      throw new Error(
        `JOURNAL_MISMATCH_ATTEMPTED_EXPIRY_PROVEN: proven expiry is re-armable only with a new journal; ${recoveryInstruction}`,
      );
    }
    if (input.mode !== "execute") {
      throw new Error(
        `PROVEN_ATTEMPTED_EXPIRY_REARM_REQUIRED: ${recoveryInstruction}`,
      );
    }
    if (existsSync(input.journal) || existsSync(`${input.journal}.pending`)) {
      throw new Error(
        `JOURNAL_EXISTS_ATTEMPTED_EXPIRY_PROVEN: new re-arm journal must not already exist: ${input.journal}`,
      );
    }
  }
  if (existsSync(input.journal)) {
    if (sectionState === null) {
      throw new Error(`STATE_GENERATION_CONFLICT: ${input.step} finalized journal has no canonical state`);
    }
    const completed = await reconcilePublishedJournal(
      input,
      dependencies,
      stateRoot,
      sectionState,
      loadFinalized,
    );
    console.log(toJson({
      schema: input.schema,
      step: input.step,
      verdict: "FINALIZED_RECONCILED",
      signature: completed.state.signature ?? null,
      journal: input.journal,
      canonicalState: canonicalLegStatePath(input.step, false, stateRoot),
      staleAbortArtifacts,
      recoveryCommand: buildHxtkRecoveryCommand({ step: input.step, mode: "reconcile", finalized: true }),
    }, 2));
    return 0;
  }
  if (sectionState?.status === "attempted" && !existsSync(`${input.journal}.pending`)) {
    // The canonical attempted record is self-sufficient. A crash after an
    // expiry artifact rename can remove `.pending` before the canonical abort
    // election; recover from the canonical wire instead of refusing merely
    // because the journal barrier is gone. This path never sends.
    const pending = recordAt(sectionState.pendingRecord, `${input.step} canonical pendingRecord`);
    const wire = journalWire(pending, input.schema, input.step, "pending");
    if (wire.signature !== String(sectionState.expectedSignature ?? "")) {
      throw new Error(PENDING_BINDING_MISMATCH);
    }
    let finalized: FinalizedTransaction;
    const initialStatus = await readSignature(input.rpcUrl, wire.signature);
    if (initialStatus.kind === "finalized") {
      finalized = initialStatus.transaction;
    } else if (initialStatus.kind === "error") {
      throw new Error(`finalized signature unreadable-error: ${initialStatus.message}`);
    } else {
      const lastValidBlockHeight = Number(
        sectionState.lastValidBlockHeight
          ?? recordAt(pending.transaction, "pending transaction").lastValidBlockHeight,
      );
      if (!Number.isSafeInteger(lastValidBlockHeight) || lastValidBlockHeight < 0) {
        throw new Error("invalid lastValidBlockHeight while checking finalized signature absence");
      }
      const observedBlockHeight = await currentBlockHeight(input.rpcUrl);
      if (observedBlockHeight <= lastValidBlockHeight + ATTEMPTED_EXPIRY_RECHECK_MARGIN_BLOCKS) {
        throw new Error("finalized signature absence is not beyond the expiry recheck margin");
      }
      // The signature may land after the first lookup and before the expiry
      // decision. Re-read it once before declaring the attempt expired.
      const secondStatus = await readSignature(input.rpcUrl, wire.signature);
      if (secondStatus.kind === "finalized") {
        finalized = secondStatus.transaction;
      } else if (secondStatus.kind === "error") {
        throw new Error(`finalized signature unreadable-error: ${secondStatus.message}`);
      } else {
        const proof: AttemptedExpiryProof = {
          lastValidBlockHeight,
          finalizedBlockHeight: observedBlockHeight,
          absentReads: [
            { kind: "absent", poll: 1, observedAtUnixMs: Date.now() - 1 },
            { kind: "absent", poll: 2, observedAtUnixMs: Date.now() },
          ],
        };
        const completed = await completeAttemptedExpiryAbort(
          input,
          dependencies,
          stateRoot,
          pending,
          expectedGeneration!,
          proof,
        );
        console.log(toJson({
          schema: input.schema,
          step: input.step,
          verdict: "ABORTED_AFTER_BLOCKHASH_EXPIRY_PROVEN",
          rearmable: true,
          abortReason: PROVEN_ATTEMPTED_EXPIRY_REASON,
          attemptedExpiryProof: proof,
          recoveryInstruction: `rerun ${input.step} with a new journal after reviewing the proven expiry artifact`,
          journal: input.journal,
          canonicalState: canonicalLegStatePath(input.step, false, stateRoot),
          abortedJournal: completed.abortedJournal,
          staleAbortArtifacts,
        }, 2));
        return 0;
      }
    }
    assertFinalizedJournalMessage(wire, finalized);
    const reconciliation = await input.reconcile({ pending, finalized });
    writePrivate(input.journal, {
      ...pending,
      verdict: "FINALIZED_RECONCILED",
      sent: true,
      signed: true,
      broadcast: true,
      signature: wire.signature,
      finalizedSlot: finalized.slot,
      finalizedBlockTime: finalized.blockTime ?? null,
      sendStatus: {
        ...(pending.sendStatus && typeof pending.sendStatus === "object" ? pending.sendStatus as JsonRecord : {}),
        verdict: "FINALIZED_RECONCILED",
        signature: wire.signature,
      },
      ...reconciliation,
    }, "wx");
    const finalizedJournalHash = finalizedJournalSha256(input.journal);
    expectedGeneration = markCanonicalLegState(input.step, input.journal, {
      status: "finalized",
      broadcast: true,
      signature: wire.signature,
      finalizedJournalSha256: finalizedJournalHash,
    }, expectedGeneration, stateRoot);
    console.log(toJson({
      schema: input.schema,
      step: input.step,
      verdict: "FINALIZED_RECONCILED",
      signature: wire.signature,
      finalizedSlot: finalized!.slot,
      journal: input.journal,
      canonicalState: canonicalLegStatePath(input.step, false, stateRoot),
      staleAbortArtifacts,
      recoveryCommand: buildHxtkRecoveryCommand({ step: input.step, mode: "reconcile", finalized: true }),
    }, 2));
    return 0;
  }
  if (input.mode === "reconcile") {
    if (!existsSync(`${input.journal}.pending`)) {
      throw new Error(`${input.step} --reconcile requires one pending journal and no finalized journal`);
    }
    const pending = readBoundPending(`${input.journal}.pending`).record;
    if (pending.verdict === "ABORTED_PRE_SEND") {
      throw new Error(`${input.step} pending journal was aborted before send; rebuild with a new journal after rechecking state`);
    }
    let wire: ReturnType<typeof journalWire>;
    try {
      wire = journalWire(pending, input.schema, input.step, "pending");
    } catch (error) {
      throw new Error(PENDING_BINDING_MISMATCH, { cause: error });
    }
    const reconciledState = ensureCanonicalLegStateForReconcile(
      input.step,
      input.journal,
      pending,
      wire,
      sectionState,
      expectedGeneration,
      stateRoot,
    );
    const canonicalStatePath = reconciledState.path;
    expectedGeneration = reconciledState.generation;
    let finalized: FinalizedTransaction;
    const initialStatus = await readSignature(input.rpcUrl, wire.signature);
    if (initialStatus.kind === "finalized") {
      finalized = initialStatus.transaction;
    } else if (initialStatus.kind === "error") {
      throw new Error(`finalized signature unreadable-error: ${initialStatus.message}`);
    } else {
      const lastValidBlockHeight = Number(
        sectionState?.lastValidBlockHeight
          ?? recordAt(pending.transaction, "pending transaction").lastValidBlockHeight,
      );
      if (!Number.isSafeInteger(lastValidBlockHeight) || lastValidBlockHeight < 0) {
        throw new Error("invalid lastValidBlockHeight while checking finalized signature absence");
      }
      const observedBlockHeight = await currentBlockHeight(input.rpcUrl);
      if (observedBlockHeight <= lastValidBlockHeight + ATTEMPTED_EXPIRY_RECHECK_MARGIN_BLOCKS) {
        throw new Error("finalized signature absence is not beyond the expiry recheck margin");
      }
      // The signature may land after the first lookup and before the expiry
      // decision. Re-read it once before declaring the attempt expired.
      const secondStatus = await readSignature(input.rpcUrl, wire.signature);
      if (secondStatus.kind === "finalized") {
        finalized = secondStatus.transaction;
      } else if (secondStatus.kind === "error") {
        throw new Error(`finalized signature unreadable-error: ${secondStatus.message}`);
      } else {
        const proof: AttemptedExpiryProof = {
          lastValidBlockHeight,
          finalizedBlockHeight: observedBlockHeight,
          absentReads: [
            { kind: "absent", poll: 1, observedAtUnixMs: Date.now() - 1 },
            { kind: "absent", poll: 2, observedAtUnixMs: Date.now() },
          ],
        };
        const completed = await completeAttemptedExpiryAbort(
          input,
          dependencies,
          stateRoot,
          pending,
          expectedGeneration!,
          proof,
          `${input.journal}.pending`,
        );
        console.log(toJson({
          schema: input.schema,
          step: input.step,
          verdict: "ABORTED_AFTER_BLOCKHASH_EXPIRY_PROVEN",
          rearmable: true,
          abortReason: PROVEN_ATTEMPTED_EXPIRY_REASON,
          attemptedExpiryProof: proof,
          recoveryInstruction: provenExpiryRearmInstruction(input.step, input.journal),
          lastValidBlockHeight,
          finalizedBlockHeight: observedBlockHeight,
          journal: input.journal,
          canonicalState: canonicalStatePath,
          abortedJournal: completed.abortedJournal,
          staleAbortArtifacts,
        }, 2));
        return 0;
      }
    }
    assertFinalizedJournalMessage(wire, finalized);
    const reconciliation = await input.reconcile({ pending, finalized });
    writePrivate(input.journal, {
      ...pending,
      verdict: "FINALIZED_RECONCILED",
      sent: true,
      signed: true,
      broadcast: true,
      signature: wire.signature,
      finalizedSlot: finalized.slot,
      finalizedBlockTime: finalized.blockTime ?? null,
      sendStatus: {
        ...(pending.sendStatus && typeof pending.sendStatus === "object" ? pending.sendStatus as JsonRecord : {}),
        verdict: "FINALIZED_RECONCILED",
        signature: wire.signature,
      },
      ...reconciliation,
    }, "wx");
    await faultAfterTransitionStep(dependencies, "final-journal");
    const finalizedJournalHash = finalizedJournalSha256(input.journal);
    renamePrivateFile(`${input.journal}.pending`, `${input.journal}.sent-wire`);
    await faultAfterTransitionStep(dependencies, "sent-wire");
    expectedGeneration = markCanonicalLegState(input.step, input.journal, {
      status: "finalized",
      broadcast: true,
      signature: wire.signature,
      finalizedJournalSha256: finalizedJournalHash,
    }, expectedGeneration, stateRoot);
    await faultAfterTransitionStep(dependencies, "finalized-state");
    console.log(toJson({
      schema: input.schema,
      step: input.step,
      verdict: "FINALIZED_RECONCILED",
      signature: wire.signature,
      finalizedSlot: finalized.slot,
      journal: input.journal,
      canonicalState: canonicalStatePath,
      staleAbortArtifacts,
      recoveryCommand: buildHxtkRecoveryCommand({ step: input.step, mode: "reconcile", finalized: true }),
    }, 2));
    return 0;
  }
  if (!dependencies.allowExecuteWithoutConfirmation && process.env.CONFIRM_MAINNET !== "1") {
    throw new Error(`${input.step} --execute requires CONFIRM_MAINNET=1`);
  }
  // The canonical replay fence is consulted while the claim is held and
  // before the requested journal barrier, build, or simulation.
  if (existsSync(input.journal)
    && sectionState?.status === "finalized"
    && String(sectionState.journal ?? "") === input.journal) {
    console.log(toJson({
      schema: input.schema,
      step: input.step,
      verdict: "FINALIZED_RECONCILED",
      signature: sectionState.signature ?? null,
      journal: input.journal,
      canonicalState: canonicalLegStatePath(input.step, false, stateRoot),
      staleAbortArtifacts,
      recoveryCommand: buildHxtkRecoveryCommand({ step: input.step, mode: "reconcile", finalized: true }),
    }, 2));
    return 0;
  }
  assertCanonicalLegAvailable(input.step, input.journal, stateRoot, sectionState);
  if (existsSync(input.journal) || existsSync(`${input.journal}.pending`)) {
    throw new Error(`${input.step} journal replay barrier already exists`);
  }
  const built = await input.build();
  if (built.prepared.packetBytes > PACKET_LIMIT) {
    throw new Error(`${input.step} packet is ${built.prepared.packetBytes} bytes; limit ${PACKET_LIMIT}`);
  }
  if (built.prepared.simulation.err !== null) {
    throw new Error(`${input.step} signed simulation failed: ${JSON.stringify({
      err: built.prepared.simulation.err,
      logs: built.prepared.simulation.logs,
    })}`);
  }
  const wireSha256 = createHash("sha256").update(built.prepared.serializedTransaction).digest("hex");
  const messageSha256 = createHash("sha256").update(built.prepared.serializedMessage).digest("hex");
  const attemptToken = randomBytes(16).toString("hex");
  const planTransaction = recordAt(built.plan.transaction, "plan.transaction");
  const pendingWithoutBinding: JsonRecord = {
    ...built.plan,
    schema: input.schema,
    step: input.step,
    verdict: "SIGNED_SIMULATION_PASS_PENDING_SEND",
    sent: false,
    signed: true,
    broadcast: false,
    attemptToken,
    expectedPreState: built.plan.expectedPreState ?? built.plan.before ?? null,
    expectedPostState: built.plan.expectedPostState ?? built.plan.postState ?? null,
    signedWireBase64: Buffer.from(built.prepared.serializedTransaction).toString("base64"),
    canonicalStateRoot: canonicalStateRoot(false, stateRoot),
    transaction: {
      ...planTransaction,
      expectedSignature: built.prepared.expectedSignature,
      wireSha256,
      messageSha256,
      messageBase64: Buffer.from(built.prepared.serializedMessage).toString("base64"),
      lastValidBlockHeight: built.prepared.latestBlockhash.lastValidBlockHeight,
      packetBytes: built.prepared.packetBytes,
      prestateSlot: built.prepared.prestateSlot,
      simulationSlot: built.prepared.simulationSlot,
    },
  };
  const pending: JsonRecord = {
    ...pendingWithoutBinding,
    pendingBindingSha256: pendingBindingSha256(pendingWithoutBinding),
  };
  const begunState = beginCanonicalLegState(
    input.step,
    input.journal,
    pending,
    sectionState,
    expectedGeneration,
    stateRoot,
  );
  const canonicalStatePath = begunState.path;
  expectedGeneration = begunState.generation;
  await faultAfterTransitionStep(dependencies, "canonical-pending");
  writePrivate(`${input.journal}.pending`, pending, "wx");
  await faultAfterTransitionStep(dependencies, "pending-journal");
  try {
    const beforeSendEvidence = await built.beforeSend?.({
      pendingJournal: `${input.journal}.pending`,
    });
    if (beforeSendEvidence?.sendStatus !== undefined) {
      const currentPending = readBoundPending(`${input.journal}.pending`).record;
      const currentSendStatus = currentPending.sendStatus && typeof currentPending.sendStatus === "object"
        ? currentPending.sendStatus as JsonRecord
        : {};
      rewritePendingStatus(`${input.journal}.pending`, {
        sendStatus: {
          ...currentSendStatus,
          ...beforeSendEvidence.sendStatus,
        },
      });
    }
  } catch (error) {
    const abortReason = sanitizeError(error);
    const abortedPendingPath = abortedJournalPath(input.journal, attemptToken);
    const staleSendStatus = error instanceof ReportSlotStaleError
      ? {
          reportSlotAgeAtSend: error.age.ageSlots,
          reportSlotCurrentSlotAtSend: error.age.currentSlot,
          reportSlotObservedSlot: error.age.observedSlot,
          reportSlotMarginSlots: error.age.marginSlots,
          reportSlotMaxAgeSlots: error.age.maxAgeSlots,
        }
      : {};
    const currentPending = readBoundPending(`${input.journal}.pending`).record;
    const currentSendStatus = currentPending.sendStatus && typeof currentPending.sendStatus === "object"
      ? currentPending.sendStatus as JsonRecord
      : {};
    rewritePendingStatus(`${input.journal}.pending`, {
      verdict: "ABORTED_PRE_SEND",
      abortReason,
      sent: false,
      signed: true,
      broadcast: false,
      attemptGeneration: expectedGeneration,
      journalBindingSha256: String(pending.pendingBindingSha256),
      sendStatus: {
        ...currentSendStatus,
        ...staleSendStatus,
        verdict: "ABORTED_PRE_SEND",
        sendError: abortReason,
      },
    });
    await faultAfterTransitionStep(dependencies, "aborted-pending");
    renamePrivateFile(`${input.journal}.pending`, abortedPendingPath);
    await faultAfterTransitionStep(dependencies, "aborted-journal");
    // This abort path can only run before the attempted mark and before the sole raw send.
    expectedGeneration = markCanonicalLegState(input.step, input.journal, {
      status: "aborted-pre-send",
      broadcast: false,
      error: abortReason,
      abortReason,
      abortedJournal: abortedPendingPath,
    }, expectedGeneration, stateRoot);
    await faultAfterTransitionStep(dependencies, "aborted-state");
    if (error instanceof ReportSlotStaleError) {
      console.log(toJson({
        schema: input.schema,
        step: input.step,
        verdict: error.verdict,
        reason: error.message,
        reportSlot: error.age,
        journal: input.journal,
        abortedJournal: abortedPendingPath,
        canonicalState: canonicalLegStatePath(input.step, false, stateRoot),
        recoveryInstruction: `rerun the same ${input.step} leg after the confirmed slot catches up`,
      }, 2));
    }
    throw error;
  }
  // Mark the exact expected signature as attempted before the raw RPC call.
  // A process crash after submission and before catch therefore remains
  // fenced as attempted instead of looking like an unsent transaction.
  const attemptedPending = rewritePendingStatus(`${input.journal}.pending`, {
    verdict: "SEND_ATTEMPTED_PENDING_RECONCILE",
    sent: false,
    signed: true,
    broadcast: "attempted",
    signature: built.prepared.expectedSignature,
    broadcastPreMark: "before-raw-submission",
    sendStatus: {
      ...(readBoundPending(`${input.journal}.pending`).record.sendStatus
        && typeof readBoundPending(`${input.journal}.pending`).record.sendStatus === "object"
        ? readBoundPending(`${input.journal}.pending`).record.sendStatus as JsonRecord
        : {}),
      verdict: "SEND_ATTEMPTED_PENDING_RECONCILE",
      attemptedAtUnixMs: Date.now(),
      signature: built.prepared.expectedSignature,
    },
  });
  expectedGeneration = markCanonicalLegState(input.step, input.journal, {
    status: "attempted",
    broadcast: "attempted",
    signature: built.prepared.expectedSignature,
    lastValidBlockHeight: built.prepared.latestBlockhash.lastValidBlockHeight,
  }, expectedGeneration, stateRoot);
  await faultAfterTransitionStep(dependencies, "attempted-state");
  // sendPreparedOnce has MAX_IDENTICAL_SUBMISSION_ATTEMPTS=1. It is the only
  // raw-send call in this shared flow; a failure leaves the pending wire for
  // the read-only --reconcile path and is never retried here.
  let settled: PreparedSettlement | null = null;
  try {
    settled = await sendOnce(input.rpcUrl, built.prepared, built.prepared.simulationSlot);
    if (settled.err !== null) {
      throw new Error(sanitizeText(`${input.step} settled with ${JSON.stringify(settled.err)}`));
    }
    const finalizedWait = await waitForFinalizedSignature({
      rpcUrl: input.rpcUrl,
      signature: settled.signature,
      lastValidBlockHeight: built.prepared.latestBlockhash.lastValidBlockHeight,
    }, dependencies);
    if (finalizedWait.kind === "expired") {
      const completed = await completeAttemptedExpiryAbort(
        input,
        dependencies,
        stateRoot,
        attemptedPending,
        expectedGeneration!,
        finalizedWait.proof,
        `${input.journal}.pending`,
      );
      console.log(toJson({
        schema: input.schema,
        step: input.step,
        verdict: "ABORTED_AFTER_BLOCKHASH_EXPIRY_PROVEN",
        rearmable: true,
        abortReason: PROVEN_ATTEMPTED_EXPIRY_REASON,
        attemptedExpiryProof: finalizedWait.proof,
        journal: input.journal,
        canonicalState: canonicalStatePath,
        abortedJournal: completed.abortedJournal,
        recoveryInstruction: `rerun ${input.step} with a new journal after reviewing the proven expiry artifact`,
      }, 2));
      // This is a completed safety abort, not a finalized repair milestone;
      // keep cmdRepairOperator from auto-removing the one-shot policy.
      return 1;
    }
    const finalized = finalizedWait.transaction;
    const wire = journalWire(attemptedPending, input.schema, input.step, "pending");
    assertFinalizedJournalMessage(wire, finalized);
    const reconciliation = await input.reconcile({ pending: attemptedPending, finalized });
    writePrivate(input.journal, {
      ...attemptedPending,
      verdict: "FINALIZED_RECONCILED",
      sent: true,
      signed: true,
      broadcast: true,
      signature: settled.signature,
      finalizedSlot: finalized.slot,
      confirmedSettledSlot: "confirmedSlot" in settled ? settled.confirmedSlot : null,
      confirmedContextSlot: "confirmedSlot" in settled ? settled.confirmationSlot : null,
      finalizedContextSlot: "confirmedSlot" in settled ? null : settled.confirmationSlot,
      finalizedBlockTime: finalized.blockTime ?? null,
      sendStatus: {
        ...(attemptedPending.sendStatus && typeof attemptedPending.sendStatus === "object" ? attemptedPending.sendStatus as JsonRecord : {}),
        verdict: "FINALIZED_RECONCILED",
        signature: settled.signature,
      },
      ...reconciliation,
    }, "wx");
    await faultAfterTransitionStep(dependencies, "final-journal");
    const finalizedJournalHash = finalizedJournalSha256(input.journal);
    renamePrivateFile(`${input.journal}.pending`, `${input.journal}.sent-wire`);
    await faultAfterTransitionStep(dependencies, "sent-wire");
    expectedGeneration = markCanonicalLegState(input.step, input.journal, {
      status: "finalized",
      broadcast: true,
      signature: settled.signature,
      finalizedJournalSha256: finalizedJournalHash,
    }, expectedGeneration, stateRoot);
    await faultAfterTransitionStep(dependencies, "finalized-state");
    console.log(toJson({
      schema: input.schema,
      step: input.step,
      verdict: "FINALIZED_RECONCILED",
      signature: settled.signature,
      finalizedSlot: finalized.slot,
      confirmedSettledSlot: "confirmedSlot" in settled ? settled.confirmedSlot : null,
      confirmedContextSlot: "confirmedSlot" in settled ? settled.confirmationSlot : null,
      reportSlot: attemptedPending.reportSlot ?? null,
      reportSlotAgeAtSend: attemptedPending.sendStatus
        && typeof attemptedPending.sendStatus === "object"
        ? (attemptedPending.sendStatus as JsonRecord).reportSlotAgeAtSend ?? null
        : null,
      journal: input.journal,
      canonicalState: canonicalStatePath,
      recoveryCommand: buildHxtkRecoveryCommand({ step: input.step, mode: "reconcile", finalized: true }),
    }, 2));
    return 0;
  } catch (error) {
    if (error instanceof JournalTransitionFault) throw error;
    if (existsSync(`${input.journal}.pending`)) {
      const signature = settled?.signature
        ?? (error instanceof PreparedTransactionSendError
          ? error.expectedSignature
          : built.prepared.expectedSignature);
      const submission = error instanceof PreparedTransactionSendError
        ? {
            submissionAttemptCount: error.submissionAttemptCount,
            submissionWireSha256: error.submissionWireSha256,
            submissionAttempts: error.submissionAttempts,
          }
        : null;
      rewritePendingStatus(`${input.journal}.pending`, {
        verdict: "SEND_ATTEMPTED_PENDING_RECONCILE",
        sent: false,
        signed: true,
        broadcast: "attempted",
        signature,
        sendStatus: {
          verdict: "SEND_ATTEMPTED_PENDING_RECONCILE",
          sendError: sanitizeError(error),
          submission,
          attemptedAtUnixMs: Number(recordAt(attemptedPending.sendStatus, "sendStatus").attemptedAtUnixMs ?? Date.now()),
          signature,
        },
      });
      // The canonical attempted mark is written exactly once, immediately
      // before sendOnce. Error details remain in the pending journal; a catch
      // or recovery path never writes another attempted state.
    }
    throw error;
  }
}

/** Offline flow hook used by reset tests; production paths use runJournaledStep. */
export async function runJournaledStepForTest(
  input: Parameters<typeof runJournaledStep>[0],
  dependencies: Readonly<{
    stateRoot: string;
    sendPreparedOnce: PreparedSendOnce;
    finalizedTransaction: typeof finalizedTransaction;
    readFinalizedSignatureStatus?: typeof readFinalizedSignatureStatus;
    currentBlockHeight?: (rpcUrl: string) => Promise<number>;
    sleep?: (milliseconds: number) => Promise<void>;
    claim?: CanonicalLegClaim;
    breakClaim?: boolean;
    faultAfterTransitionStep?: (step: JournalTransitionStep) => void | Promise<void>;
  }>,
): Promise<number> {
  const preClaimState = readCanonicalLegState(input.step, false, dependencies.stateRoot, { allowRollForward: false });
  if (input.mode === "execute"
    && preClaimState?.status === "aborted-pre-send"
    && preClaimState.abortReason === ATTEMPTED_EXPIRY_REASON
    && String(preClaimState.journal ?? "") !== input.journal) {
    throw new Error(`JOURNAL_MISMATCH_ATTEMPTED_EXPIRED: reconcile the original journal ${preClaimState.journal}`);
  }
  const claim = dependencies.claim ?? acquireCanonicalLegClaim({
    stateRoot: dependencies.stateRoot,
    step: input.step,
    journal: input.journal,
    ...(dependencies.breakClaim === undefined ? {} : { breakClaim: dependencies.breakClaim }),
  });
  try {
    return await runJournaledStepHeld(input, {
      ...dependencies,
      allowExecuteWithoutConfirmation: true,
    });
  } finally {
    if (dependencies.claim === undefined) releaseCanonicalLegClaim(claim);
  }
}

function operatorRpcUrl(): string {
  const rpcUrl = process.env.SOLANA_RPC_URL?.trim();
  if (!rpcUrl) throw new Error("SOLANA_RPC_URL is required for the operator path");
  return rpcUrl;
}

function configPostState(account: RawAccount) {
  const vault = decodeVault(account);
  return vault ? {
    lockedProfitDegradationDuration: vault.lockedProfitDegradationDuration.toString(),
    adminPerformanceFeeBps: vault.adminPerformanceFeeBps,
    totalValue: vault.totalValue.toString(),
    admin: vault.admin,
    manager: vault.manager,
  } : null;
}

async function cmdConfigOperator(mode: RepairPolicyOperatorMode): Promise<number> {
  const journal = operatorJournal();
  const rpcUrl = operatorRpcUrl();
  return runJournaledStep({
    mode,
    step: "config",
    schema: SCHEMA,
    journal,
    rpcUrl,
    build: async () => {
      const state = await readState("finalized");
      if (!state.vault) throw new Error("vault account is absent");
      if (state.vault.admin !== ADMIN) throw new Error(`vault admin ${state.vault.admin} is not ${ADMIN}`);
      const noopAdmin = createNoopSigner(ADMIN);
      const degradation = await getUpdateVaultConfigInstructionAsync({
        admin: noopAdmin, protocol: PROTOCOL, vault: VAULT, rent: RENT_SYSVAR,
        field: VaultConfigField.LockedProfitDegradationDuration, data: new Uint8Array(8),
      }, { programAddress: VOLTR });
      const adminFee = await getUpdateVaultConfigInstructionAsync({
        admin: noopAdmin, protocol: PROTOCOL, vault: VAULT, rent: RENT_SYSVAR,
        field: VaultConfigField.AdminPerformanceFee, data: new Uint8Array(2),
      }, { programAddress: VOLTR });
      const instructions = [degradation, adminFee];
      for (const instruction of instructions) {
        const accounts = instruction.accounts ?? [];
        if (accounts.length !== 4 || accounts[0]?.address !== ADMIN || accounts[2]?.address !== VAULT) {
          throw new Error("updateVaultConfig account list drifted from admin/protocol/vault/rent");
        }
      }
      const admin = await signingMaterialFromEnvironment("SOLANA_TESTING_PK");
      if (admin.signer.address !== ADMIN) throw new Error("SOLANA_TESTING_PK is not the HXtk admin signer");
      const prepared = await prepareSignedV0Transaction({
        rpcUrl,
        feePayer: admin,
        instructions,
        inspectedAddresses: [VAULT],
        prestateAddresses: [VAULT],
        minimumContextSlot: state.contextSlot,
        commitment: "finalized",
      });
      const postVault = decodeVault(rawFromPreparedSnapshot(VAULT, prepared.simulation.postAccounts[0]));
      if (!postVault || postVault.lockedProfitDegradationDuration !== 0n || postVault.adminPerformanceFeeBps !== 0) {
        throw new Error("signed config simulation did not project degradation and admin fee to zero");
      }
      return {
        prepared,
        plan: {
          before: summarize(state),
          expectedPreState: summarize(state),
          expectedPostState: configPostState(rawFromPreparedSnapshot(VAULT, prepared.simulation.postAccounts[0])),
          transaction: {
            feePayer: ADMIN,
            signer: ADMIN,
            signerEnvVar: "SOLANA_TESTING_PK",
            instructionCount: 2,
            instructions: [
              "updateVaultConfig(LockedProfitDegradationDuration, u64 0)",
              "updateVaultConfig(AdminPerformanceFee, u16 0)",
            ],
          },
        },
      };
    },
    reconcile: async ({ pending }) => {
      const state = await readState("finalized");
      if (!state.vault || state.vault.lockedProfitDegradationDuration !== 0n || state.vault.adminPerformanceFeeBps !== 0) {
        throw new Error("finalized config did not reconcile degradation and admin fee to zero");
      }
      const before = recordAt(pending.before, "before");
      if (String(before.totalValue) !== state.vault.totalValue.toString()) {
        throw new Error("finalized config changed vault total value");
      }
      return { finalizedState: summarize(state) };
    },
  });
}

function rawFromPreparedSnapshot(
  target: Address,
  account: Readonly<{ owner: string; lamports: number; data: Uint8Array }> | null | undefined,
): RawAccount {
  if (!account) return null;
  return { address: target, owner: account.owner, lamports: account.lamports, data: account.data };
}

async function cmdRepairPolicyOperator(mode: RepairPolicyOperatorMode): Promise<number> {
  assertNoArbitraryRepairSeed();
  const journal = operatorJournal();
  const rpcUrl = operatorRpcUrl();
  return runJournaledStep({
    mode,
    step: "repair-policy",
    schema: REPAIR_POLICY_SCHEMA,
    journal,
    rpcUrl,
    build: async () => {
      const settingsBefore = await readSettingsPolicyCounter();
      assertRepairPolicySettingsCounter(settingsBefore, "repair-policy initial read");
      const readback = await readRepairPolicySeeds();
      const seed = REPAIR_POLICY_EXPECTED_SEED;
      const row = readback.rows.find((candidate) => BigInt(candidate.seed) === seed);
      if (row?.present) throw new Error(`repair-policy seed ${seed} is already present on finalized chain`);
      const { artifact, target } = await compileRepairPolicy(seed);
      const policyCreate = wireInstruction(target.createInstruction);
      const createData = Uint8Array.from(policyCreate.data ?? []);
      assertRepairPolicyHardBinding(seed, address(target.policy), createData);
      const compiledPolicy = decodePolicyCreateWire(policyCreate);
      const admin = await signingMaterialFromEnvironment("SOLANA_TESTING_PK");
      if (admin.signer.address !== ADMIN) throw new Error("SOLANA_TESTING_PK is not the HXtk admin signer");
      const prepared = await prepareSignedV0Transaction({
        rpcUrl,
        feePayer: admin,
        instructions: [policyCreate],
        inspectedAddresses: [target.policy, SQUADS_SETTINGS, ADMIN],
        prestateAddresses: [target.policy, SQUADS_SETTINGS, ADMIN],
        minimumContextSlot: readback.contextSlot,
        commitment: "finalized",
      });
      const projectedPolicy = rawFromPreparedSnapshot(address(target.policy), prepared.simulation.postAccounts[0]);
      const projectedSettings = rawFromPreparedSnapshot(SQUADS_SETTINGS, prepared.simulation.postAccounts[1]);
      const decoded = decodeSquadsPolicy(projectedPolicy);
      const projectedPolicyDataSha256 = projectedPolicy
        ? createHash("sha256").update(projectedPolicy.data).digest("hex")
        : null;
      const semantics = repairPolicySemantics(decoded, seed, compiledPolicy);
      if (!semantics.identityPass || !semantics.payloadPass || !semantics.digestUnconstrained
        || !semantics.navExact.pass || !semantics.compiledMatch) {
        throw new Error("signed repair-policy simulation did not project the exact one-shot contract");
      }
      if (!projectedPolicy) throw new Error("signed repair-policy simulation omitted the projected policy account");
      if (!projectedSettings || projectedSettings.owner !== SQUADS_PROGRAM) {
        throw new Error("signed PolicyCreate simulation did not preserve the Squads Settings identity");
      }
      const settingsIdentity = {
        address: SQUADS_SETTINGS,
        owner: projectedSettings.owner,
        dataBytes: projectedSettings.data.length,
        dataSha256: createHash("sha256").update(projectedSettings.data).digest("hex"),
        policySeed: decodeSquadsSettingsPolicySeed(projectedSettings),
      };
      if (settingsIdentity.policySeed !== seed.toString()) {
        throw new Error("signed PolicyCreate simulation did not advance Settings to seed 140");
      }
      const plan: JsonRecord = {
        seed: seed.toString(),
        expectedSeed: seed.toString(),
        policy: target.policy,
        compiler: {
          ...artifact.compiler,
          compilerArtifactSchema: artifact.schema,
          sourceSha256: artifact.sourceSha256,
          compilerPolicySeedBefore: (seed - 2n).toString(),
          operation: target.operation,
          constraintIndices: target.constraintIndices,
          createDataBytes: createData.length,
          createDataSha256: createHash("sha256").update(createData).digest("hex"),
          navExact: semantics.navExact,
          navConstraint: "Equals",
          digest: "unconstrained",
          compiledPolicy,
        },
        hardBinding: {
          expectedSeed: REPAIR_POLICY_EXPECTED_SEED.toString(),
          expectedPolicy: REPAIR_POLICY_EXPECTED_PDA,
          createDataBytes: REPAIR_POLICY_CREATE_DATA_BYTES,
          createDataSha256: REPAIR_POLICY_CREATE_DATA_SHA256,
          simulatedPolicyDataBytes: projectedPolicy.data.length,
          simulatedPolicyDataSha256: projectedPolicyDataSha256,
          settingsPolicySeed: REPAIR_POLICY_EXPECTED_SETTINGS_SEED.toString(),
        },
        settingsReadback: settingsPolicyReadback(settingsBefore),
        settingsIdentity,
        seedReadback: readback,
        expectedPreState: {
          settingsPolicySeed: settingsBefore.policySeed.toString(),
          expectedSeed: seed.toString(),
          policy: "absent",
        },
        expectedPostState: {
          settingsPolicySeed: seed.toString(),
          policy: target.policy,
          policyDataBytes: projectedPolicy.data.length,
        },
        constraints: stablePolicyConstraints(semantics.decoded),
        decodedPolicy: semantics.decoded,
        transaction: {
          feePayer: ADMIN,
          signer: ADMIN,
          signerEnvVar: "SOLANA_TESTING_PK",
          instructionCount: 1,
          policyCreate: wireFromInstruction(policyCreate),
        },
      };
      return {
        prepared,
        plan,
        beforeSend: async () => {
          const settingsAtSend = await readSettingsPolicyCounter();
          if (settingsAtSend.policySeed !== settingsBefore.policySeed
            || settingsAtSend.expectedSeed !== seed) {
            throw new Error(`Settings policy seed moved during repair-policy preparation (${settingsBefore.policySeed} -> ${settingsAtSend.policySeed}); refusing send`);
          }
          assertRepairPolicySettingsCounter(settingsAtSend, "repair-policy pre-send read");
          assertRepairPolicyHardBinding(seed, address(target.policy), createData);
          if (await getAccount(address(target.policy), "finalized")) {
            throw new Error("repair-policy target PDA appeared before send; refusing duplicate creation");
          }
        },
      };
    },
    reconcile: async ({ pending }) => {
      const seed = REPAIR_POLICY_EXPECTED_SEED;
      const policy = address(stringAt(pending.policy, "policy"));
      assertRepairPolicyHardBinding(seed, policy);
      const settingsCounter = await readSettingsPolicyCounter();
      if (settingsCounter.policySeed !== seed) {
        throw new Error(`finalized Settings policy seed ${settingsCounter.policySeed} does not equal created seed ${seed}`);
      }
      const policyAccount = await getAccount(policy, "finalized");
      const policyCreate = recordAt(
        recordAt(pending.transaction, "repair-policy transaction").policyCreate,
        "repair-policy transaction.policyCreate",
      );
      const compiledPolicy = decodePolicyCreateWire(
        wireInstruction(policyCreate as unknown as PolicyWireInstruction),
      );
      const semantics = repairPolicySemantics(decodeSquadsPolicy(policyAccount), seed, compiledPolicy);
      const dataSha256 = policyAccount
        ? createHash("sha256").update(policyAccount.data).digest("hex")
        : null;
      if (!policyAccount || !semantics.identityPass || !semantics.payloadPass
        || !semantics.digestUnconstrained || !semantics.navExact.pass || !semantics.compiledMatch) {
        throw new Error("finalized repair-policy readback is not the exact one-shot contract");
      }
      const settings = await getAccount(SQUADS_SETTINGS, "finalized");
      const settingsIdentity = recordAt(pending.settingsIdentity, "settingsIdentity");
      const expectedSettingsHash = stringAt(settingsIdentity.dataSha256, "settingsIdentity.dataSha256");
      if (!settings || settings.owner !== SQUADS_PROGRAM
        || settings.data.length !== Number(settingsIdentity.dataBytes)
        || createHash("sha256").update(settings.data).digest("hex") !== expectedSettingsHash
        || decodeSquadsSettingsPolicySeed(settings) !== seed.toString()
        || String(settingsIdentity.address) !== SQUADS_SETTINGS) {
        throw new Error("finalized PolicyCreate changed or omitted the reviewed Settings identity");
      }
      return {
        finalizedPolicyDataBytes: policyAccount.data.length,
        finalizedPolicyDataSha256: dataSha256,
        finalizedSettingsIdentity: {
          address: SQUADS_SETTINGS,
          owner: settings.owner,
          dataBytes: settings.data.length,
          dataSha256: createHash("sha256").update(settings.data).digest("hex"),
          policySeed: decodeSquadsSettingsPolicySeed(settings),
        },
        finalizedSettingsReadback: settingsPolicyReadback(settingsCounter),
        finalizedPolicy: semantics.decoded,
        finalizedCompiledPolicy: compiledPolicy,
        finalizedConstraints: stablePolicyConstraints(semantics.decoded),
      };
    },
  });
}

async function cmdRepairPolicy(): Promise<number> {
  const mode = operatorMode();
  if (mode) return cmdRepairPolicyOperator(mode);
  assertNoArbitraryRepairSeed();
  if (currentCli().has("--journal")) {
    // --journal without an operator mode is almost certainly an operator typo
    // and must not be ignored.
    if (currentCli().has("--journal")) {
      throw new Error("repair-policy --journal requires --execute or --reconcile");
    }
  }
  const settingsBefore = await readSettingsPolicyCounter();
  assertRepairPolicySettingsCounter(settingsBefore, "repair-policy initial read");
  const readback = await readRepairPolicySeeds();
  const seed = REPAIR_POLICY_EXPECTED_SEED;
  const row = readback.rows.find((candidate) => BigInt(candidate.seed) === seed);
  if (row?.present) throw new Error(`repair-policy seed ${seed} is already present on finalized chain`);
  const { artifact, target } = await compileRepairPolicy(seed);
  const policyCreate = wireInstruction(target.createInstruction);
  assertRepairPolicyHardBinding(seed, address(target.policy), Uint8Array.from(policyCreate.data ?? []));
  const compiledPolicy = decodePolicyCreateWire(policyCreate);
  const targetPolicy = address(target.policy);
  const policyCreateAccounts = policyCreate.accounts ?? [];
  const simulation = await simulate(ADMIN, [policyCreate], [targetPolicy, SQUADS_SETTINGS, ADMIN]);
  const projectedPolicy = simulation.err === null
    ? postAccount(simulation.postAccounts, targetPolicy)
    : null;
  const decoded = simulation.err === null ? decodeSquadsPolicy(projectedPolicy) : null;
  const projectedPolicyDataSha256 = projectedPolicy
    ? createHash("sha256").update(projectedPolicy.data).digest("hex")
    : null;
  const semantics = repairPolicySemantics(decoded, seed, compiledPolicy);
  const checks = [
    checkRow("simulation succeeds", simulation.err === null, null,
      simulation.err === null ? null : JSON.stringify(simulation.err)),
    checkRow("packet <= 1,232 bytes", simulation.packetBytes <= PACKET_LIMIT,
      `<= ${PACKET_LIMIT}`, simulation.packetBytes),
    checkRow("PolicyCreate uses the HXtk admin as fee payer/signer",
      policyCreateAccounts.some((account) => account.address === ADMIN && (
        account.role === AccountRole.READONLY_SIGNER || account.role === AccountRole.WRITABLE_SIGNER)),
      ADMIN, policyCreateAccounts.map((account) => account.address)),
    checkRow("PolicyCreate action seed equals Settings expectedSeed",
      target.seed === seed.toString(), seed.toString(), target.seed),
    checkRow("PolicyCreate seed is the hard-bound 140", seed === REPAIR_POLICY_EXPECTED_SEED,
      REPAIR_POLICY_EXPECTED_SEED.toString(), seed.toString()),
    checkRow("PolicyCreate PDA is the hard-bound PDA", targetPolicy === REPAIR_POLICY_EXPECTED_PDA,
      REPAIR_POLICY_EXPECTED_PDA, targetPolicy),
    checkRow("PolicyCreate data is the reviewed exact-NAV wire",
      (policyCreate.data?.length ?? 0) === REPAIR_POLICY_CREATE_DATA_BYTES,
      REPAIR_POLICY_CREATE_DATA_BYTES, policyCreate.data?.length ?? null),
    checkRow("PolicyCreate data hash is the reviewed hash",
      createHash("sha256").update(Uint8Array.from(policyCreate.data ?? [])).digest("hex") === REPAIR_POLICY_CREATE_DATA_SHA256,
      REPAIR_POLICY_CREATE_DATA_SHA256,
      createHash("sha256").update(Uint8Array.from(policyCreate.data ?? [])).digest("hex")),
    checkRow("simulated policy owner is Squads",
      projectedPolicy?.owner === SQUADS_PROGRAM, SQUADS_PROGRAM, projectedPolicy?.owner ?? null),
    checkRow("simulated policy account is present",
      projectedPolicy !== null,
      "present",
      projectedPolicy ? `${projectedPolicy.data.length} bytes/${projectedPolicyDataSha256}` : null),
    checkRow("simulated policy identity and delegated signer are exact",
      semantics.identityPass, true, semantics.identityPass),
    checkRow("simulated policy is ProgramInteraction with account index 0 and no spending limit",
      semantics.payloadPass, true, semantics.payloadPass),
    checkRow("report digest is unconstrained", semantics.digestUnconstrained, true,
      semantics.digestUnconstrained),
    checkRow("exact NAV Equals constraints are present at offsets 39 and 51", semantics.navExact.pass, true,
      semantics.navExact),
    checkRow("decoded live policy matches the compiled PolicyCreate", semantics.compiledMatch, true,
      semantics.compiledMatch),
  ];
  const pass = checks.every((check) => check.pass);
  const output = {
    schema: REPAIR_POLICY_SCHEMA,
    step: "repair-policy",
    sent: false,
    broadcast: false,
    verdict: pass ? "SIMULATION_PASS_UNSENT" : "SIMULATION_FAILED",
    seed: seed.toString(),
    expectedSeed: seed.toString(),
    policy: target.policy,
    hardBinding: {
      expectedSeed: REPAIR_POLICY_EXPECTED_SEED.toString(),
      expectedPolicy: REPAIR_POLICY_EXPECTED_PDA,
      createDataBytes: REPAIR_POLICY_CREATE_DATA_BYTES,
      createDataSha256: REPAIR_POLICY_CREATE_DATA_SHA256,
      settingsPolicySeed: REPAIR_POLICY_EXPECTED_SETTINGS_SEED.toString(),
      sequentialSeedRule: "PolicyCreate uses Settings.policySeed + 1; seed 140 is the only permitted repair policy target",
    },
    seedReadback: readback,
    settingsReadback: settingsPolicyReadback(settingsBefore),
    compiler: {
      ...artifact.compiler,
      compilerArtifactSchema: artifact.schema,
      sourceSha256: artifact.sourceSha256,
      compilerPolicySeedBefore: (seed - 2n).toString(),
      operation: target.operation,
      constraintIndices: target.constraintIndices,
      createDataBytes: Buffer.from(target.createInstruction.dataBase64, "base64").length,
      createDataSha256: createHash("sha256").update(Buffer.from(target.createInstruction.dataBase64, "base64")).digest("hex"),
      navExact: semantics.navExact,
      navConstraint: "Equals",
      digest: "unconstrained",
      compiledPolicy,
    },
    simulatedPolicyDataBytes: projectedPolicy?.data.length ?? null,
    simulatedPolicyDataSha256: projectedPolicyDataSha256,
    policyProvenanceStatement: POLICY_PROVENANCE_STATEMENT,
    transaction: {
      feePayer: ADMIN,
      signer: ADMIN,
      signerEnvVar: "SOLANA_TESTING_PK",
      packetBytes: simulation.packetBytes,
      instructionCount: 1,
      unitsConsumed: simulation.unitsConsumed,
      policyCreate: wireFromInstruction(policyCreate),
    },
    constraintSource: simulation.err === null ? "simulated_policy_account" : "compiled_policy_create_payload",
    constraints: stablePolicyConstraints(simulation.err === null ? semantics.decoded : compiledPolicy),
    compiledConstraints: stablePolicyConstraints(compiledPolicy),
    decodedPolicy: semantics.decoded,
    simulationBlocker: simulation.err !== null
      ? {
          err: simulation.err,
          reason: "PolicyCreate simulation failed for the exact finalized Settings next seed",
        }
      : null,
    checks,
    ...(simulation.err === null ? {} : { logs: simulation.logs }),
  };
  console.log(toJson(output, 2));
  writeEvidence("repair-policy", output);
  return pass ? 0 : 1;
}

function repairPrecondition(state: LiveState): Readonly<{
  repairNavRaw: bigint;
}> {
  if (!state.vault || !state.receipt1 || !state.reportTicket || state.idleBalance === null) {
    throw new Error("repair requires the HXtk vault, strategy receipt, idle ATA, and report ticket");
  }
  const frozenChecks = [
    checkRow("frozen tv", state.vault.totalValue === REPAIR_FROZEN.totalValue,
      REPAIR_FROZEN.totalValue.toString(), state.vault.totalValue.toString()),
    checkRow("frozen idle", state.idleBalance === REPAIR_FROZEN.idleBalance,
      REPAIR_FROZEN.idleBalance.toString(), state.idleBalance.toString()),
    checkRow("frozen strategy-one receipt", state.receipt1.positionValue === REPAIR_FROZEN.receipt1PositionValue,
      REPAIR_FROZEN.receipt1PositionValue.toString(), state.receipt1.positionValue.toString()),
    checkRow("frozen strategy custody", state.custody1Balance === REPAIR_FROZEN.custody1Balance,
      REPAIR_FROZEN.custody1Balance.toString(), state.custody1Balance?.toString() ?? null),
    checkRow("frozen LP supply", state.lpSupply === REPAIR_FROZEN.lpSupply,
      REPAIR_FROZEN.lpSupply.toString(), state.lpSupply?.toString() ?? null),
    checkRow("frozen degradation", state.vault.lockedProfitDegradationDuration === REPAIR_FROZEN.degradation,
      REPAIR_FROZEN.degradation.toString(), state.vault.lockedProfitDegradationDuration.toString()),
    checkRow("frozen admin performance fee", state.vault.adminPerformanceFeeBps === REPAIR_FROZEN.adminPerformanceFeeBps,
      REPAIR_FROZEN.adminPerformanceFeeBps, state.vault.adminPerformanceFeeBps),
    checkRow("frozen waiting period", state.vault.withdrawalWaitingPeriod === REPAIR_FROZEN.waitingPeriod,
      REPAIR_FROZEN.waitingPeriod.toString(), state.vault.withdrawalWaitingPeriod.toString()),
    checkRow("frozen pending request receipt is present", state.requestReceipt !== null,
      REPAIR_FROZEN.requestReceipt, state.requestReceipt ? REPAIR_FROZEN.requestReceipt : null),
    checkRow("frozen request receipt identity", state.requestReceipt?.vault === VAULT.toString()
      && state.requestReceipt.userTransferAuthority === ADMIN.toString(),
    `${VAULT}/${ADMIN}`, state.requestReceipt ? `${state.requestReceipt.vault}/${state.requestReceipt.userTransferAuthority}` : null),
    checkRow("frozen request escrow", state.requestEscrowLpBalance === REPAIR_FROZEN.requestEscrowLpBalance,
      REPAIR_FROZEN.requestEscrowLpBalance.toString(), state.requestEscrowLpBalance?.toString() ?? null),
    checkRow("frozen request amount", state.requestReceipt?.amountLpEscrowed === REPAIR_FROZEN.requestEscrowLpBalance,
      REPAIR_FROZEN.requestEscrowLpBalance.toString(), state.requestReceipt?.amountLpEscrowed.toString() ?? null),
    checkRow("frozen ticket last consumed sequence", state.reportTicket.lastConsumedSequence === REPAIR_FROZEN.ticketLastConsumedSequence,
      REPAIR_FROZEN.ticketLastConsumedSequence.toString(), state.reportTicket.lastConsumedSequence.toString()),
  ];
  if (!frozenChecks.every((check) => check.pass)) {
    throw new Error(`REPAIR_FROZEN_STATE_MISMATCH ${toJson({ observationSlot: state.contextSlot, checks: frozenChecks })}`);
  }
  if (state.vault.admin !== ADMIN || state.vault.manager !== SQUADS_VAULT) {
    throw new Error("repair authority boundary drifted from the HXtk admin and Squads manager");
  }
  if (state.receipt1.vault !== VAULT
    || state.receipt1.strategy !== RWA_MULTIPLY_ROUTE.customAdaptor.strategyConfig
    || state.receipt1.adaptorProgram !== RWA_MULTIPLY_ROUTE.customAdaptor.program) {
    throw new Error("strategy-one receipt identity drifted from the HXtk custom adaptor");
  }
  if (state.custody1Balance !== 0n) {
    throw new Error(`repair requires zero strategy custody; observed ${state.custody1Balance ?? "null"}`);
  }
  if (state.reportTicket.armed || state.reportTicket.activeSequence !== 0n || !state.reportTicket.activeHashIsZero) {
    throw new Error("repair requires an idle report ticket with no active report");
  }
  const repairNavRaw = state.idleBalance + state.receipt1.positionValue - state.vault.totalValue;
  if (repairNavRaw !== PHANTOM_NAV_RAW) {
    throw new Error(`repair NAV changed from the frozen phantom ${PHANTOM_NAV_RAW}; observed ${repairNavRaw}`);
  }
  return { repairNavRaw };
}

export async function buildFreshRepairReport(input: Readonly<{
  snapshot: LiveState;
  readConfirmedSlot: () => Promise<number>;
}>): Promise<Readonly<{
  report: Readonly<{
    sequence: bigint;
    observedSlot: bigint;
    navAfterRaw: bigint;
    snapshotDigest: Uint8Array;
  }>;
  slot: number;
  repairNavRaw: bigint;
}>> {
  const precondition = repairPrecondition(input.snapshot);
  // This is intentionally the last chain read and the first slot-dependent
  // value before the arm/capital instruction builders run. NAV remains bound
  // to the consistent snapshot above; only the report sequence is fresh.
  const slot = await input.readConfirmedSlot();
  const sequence = BigInt(slot);
  if (sequence <= input.snapshot.reportTicket!.lastConsumedSequence) {
    throw new Error(
      `repair sequence ${sequence} is not above last consumed ${input.snapshot.reportTicket!.lastConsumedSequence}`,
    );
  }
  return {
    slot,
    repairNavRaw: precondition.repairNavRaw,
    report: {
      sequence,
      observedSlot: sequence,
      navAfterRaw: PHANTOM_NAV_RAW,
      snapshotDigest: REPORT_DIGEST,
    },
  };
}

async function buildRepairExecution(
  policy: Address,
  state: LiveState,
  readSlot: () => Promise<number> = readConfirmedSlot,
) {
  const precondition = repairPrecondition(state);
  const manager = createNoopSigner(SQUADS_VAULT);
  const placeholderReport = {
    sequence: 0n,
    observedSlot: 0n,
    navAfterRaw: PHANTOM_NAV_RAW,
    snapshotDigest: REPORT_DIGEST,
  } as const;
  const placeholderArm = await buildRwaMultiplyArmReportInstruction(
    manager,
    "deposit",
    0n,
    placeholderReport,
  );
  const placeholderCapital = await buildRwaMultiplyManagerInstructions(manager, 0n, placeholderReport);
  const compiledPlaceholder = compileCustomExecution(
    policy,
    [placeholderArm, placeholderCapital.deposit],
    [0, 1],
  );
  // All readbacks and the slow execution-artifact compiler are complete before
  // this fresh slot read. Only the final slot-bearing instruction bytes are
  // built/rebound afterward, leaving a short age window before simulation.
  const fresh = await buildFreshRepairReport({ snapshot: state, readConfirmedSlot: readSlot });
  const report = fresh.report;
  const arm = await buildRwaMultiplyArmReportInstruction(
    manager,
    "deposit",
    0n,
    report,
  );
  const capital = await buildRwaMultiplyManagerInstructions(manager, 0n, report);
  const executionArtifact = rebindCustomExecutionArtifact(
    compiledPlaceholder,
    policy,
    [placeholderArm, placeholderCapital.deposit],
    [arm, capital.deposit],
    [0, 1],
  );
  return {
    precondition,
    freshSlot: fresh.slot,
    report,
    arm,
    capital: capital.deposit,
    executionArtifact,
    execution: wireInstruction(executionArtifact.instruction),
  } as const;
}

type RepairObservedState = Readonly<{
  totalValue: bigint | null;
  idleBalance: bigint | null;
  receipt1PositionValue: bigint | null;
  custody1Balance: bigint | null;
  lpSupply: bigint | null;
  lockedProfitDegradationDuration: bigint | null;
  reportTicket: ReturnType<typeof decodeReportTicket>;
}>;

function repairObservedState(state: LiveState): RepairObservedState {
  return {
    totalValue: state.vault?.totalValue ?? null,
    idleBalance: state.idleBalance,
    receipt1PositionValue: state.receipt1?.positionValue ?? null,
    custody1Balance: state.custody1Balance,
    lpSupply: state.lpSupply,
    lockedProfitDegradationDuration: state.vault?.lockedProfitDegradationDuration ?? null,
    reportTicket: state.reportTicket,
  };
}

function repairChecks(
  observed: RepairObservedState,
  before: LiveState,
  sequence: bigint,
) {
  return repairChecksAgainst(
    observed,
    before.lpSupply,
    before.vault?.lockedProfitDegradationDuration ?? null,
    sequence,
  );
}

function repairChecksAgainst(
  observed: RepairObservedState,
  expectedLpSupply: bigint | null,
  expectedDegradation: bigint | null,
  sequence: bigint,
) {
  const checks = [
    checkRow("post tv == repaired idle 3,793,417", observed.totalValue === REPAIRED_BOOK_RAW,
      REPAIRED_BOOK_RAW.toString(), observed.totalValue?.toString() ?? null),
    checkRow("post idle == 3,793,417", observed.idleBalance === REPAIRED_BOOK_RAW,
      REPAIRED_BOOK_RAW.toString(), observed.idleBalance?.toString() ?? null),
    checkRow("post strategy-one receipt == 3,793,536", observed.receipt1PositionValue === PHANTOM_NAV_RAW,
      PHANTOM_NAV_RAW.toString(), observed.receipt1PositionValue?.toString() ?? null),
    checkRow("post strategy custody == 0", observed.custody1Balance === 0n, "0", observed.custody1Balance?.toString() ?? null),
    checkRow("LP supply unchanged", observed.lpSupply === expectedLpSupply,
      expectedLpSupply?.toString() ?? null, observed.lpSupply?.toString() ?? null),
    checkRow("locked-profit degradation is unchanged from the read", observed.lockedProfitDegradationDuration === expectedDegradation,
      expectedDegradation?.toString() ?? null,
      observed.lockedProfitDegradationDuration?.toString() ?? null),
    checkRow("report ticket consumed the repair sequence", observed.reportTicket?.lastConsumedSequence === sequence,
      sequence.toString(), observed.reportTicket?.lastConsumedSequence.toString() ?? null),
    checkRow("report ticket is idle after consume", observed.reportTicket?.armed === false
      && observed.reportTicket.activeSequence === 0n && observed.reportTicket.activeHashIsZero,
    "false/0/zero", observed.reportTicket ? `${observed.reportTicket.armed}/${observed.reportTicket.activeSequence}/${observed.reportTicket.activeHashIsZero}` : null),
  ];
  return {
    postState: {
      totalValue: observed.totalValue?.toString() ?? null,
      idleBalance: observed.idleBalance?.toString() ?? null,
      receipt1PositionValue: observed.receipt1PositionValue?.toString() ?? null,
      custody1Balance: observed.custody1Balance?.toString() ?? null,
      lpSupply: observed.lpSupply?.toString() ?? null,
      lockedProfitDegradationDuration: observed.lockedProfitDegradationDuration?.toString() ?? null,
      reportTicket: observed.reportTicket ? {
        armed: observed.reportTicket.armed,
        lastConsumedSequence: observed.reportTicket.lastConsumedSequence.toString(),
        activeSequence: observed.reportTicket.activeSequence.toString(),
        activeHashIsZero: observed.reportTicket.activeHashIsZero,
      } : null,
    },
    checks,
  } as const;
}

function repairPoststate(
  postAccounts: readonly RawAccount[],
  before: LiveState,
  sequence: bigint,
) {
  return repairChecks({
    totalValue: decodeVault(postAccount(postAccounts, VAULT))?.totalValue ?? null,
    idleBalance: tokenAmount(postAccount(postAccounts, IDLE_ATA)),
    receipt1PositionValue: decodeStrategyReceipt(postAccount(postAccounts, RECEIPT1))?.positionValue ?? null,
    custody1Balance: tokenAmount(postAccount(postAccounts, CUSTODY1)),
    lpSupply: mintSupply(postAccount(postAccounts, LP_MINT)),
    lockedProfitDegradationDuration: decodeVault(postAccount(postAccounts, VAULT))?.lockedProfitDegradationDuration ?? null,
    reportTicket: decodeReportTicket(postAccount(postAccounts, REPORT_TICKET)),
  }, before, sequence);
}

async function cmdRepair(): Promise<number> {
  const mode = operatorMode();
  if (mode) return cmdRepairOperator(mode);
  assertNoArbitraryRepairSeed();
  if (currentCli().has("--journal")) {
    throw new Error("repair --journal requires --execute or --reconcile");
  }
  const state = await readState("finalized", true);
  const rpcUrl = process.env.SOLANA_RPC_URL?.trim() || DEFAULT_RPC_URL;
  await assertRepairNotAlreadyApplied(state, rpcUrl);
  try {
    repairPrecondition(state);
  } catch (error) {
    const blocker = error instanceof Error ? error.message : String(error);
    if (!blocker.startsWith("REPAIR_FROZEN_STATE_MISMATCH")) throw error;
    const output = {
      schema: REPAIR_EXECUTION_SCHEMA,
      step: "repair",
      policyProvenanceStatement: POLICY_PROVENANCE_STATEMENT,
      sent: false,
      signed: false,
      broadcast: false,
      verdict: "REPAIR_FROZEN_STATE_MISMATCH",
      observationSlot: state.contextSlot,
      frozenPrestate: {
        totalValue: REPAIR_FROZEN.totalValue.toString(),
        idleBalance: REPAIR_FROZEN.idleBalance.toString(),
        receipt1PositionValue: REPAIR_FROZEN.receipt1PositionValue.toString(),
        custody1Balance: REPAIR_FROZEN.custody1Balance.toString(),
        lpSupply: REPAIR_FROZEN.lpSupply.toString(),
        degradation: REPAIR_FROZEN.degradation.toString(),
        adminPerformanceFeeBps: REPAIR_FROZEN.adminPerformanceFeeBps,
        withdrawalWaitingPeriod: REPAIR_FROZEN.waitingPeriod.toString(),
        requestReceipt: REPAIR_FROZEN.requestReceipt,
        requestEscrowLpBalance: REPAIR_FROZEN.requestEscrowLpBalance.toString(),
        ticketLastConsumedSequence: REPAIR_FROZEN.ticketLastConsumedSequence.toString(),
      },
      state: summarize(state),
      reason: blocker,
    };
    console.log(toJson(output, 2));
    writeEvidence("repair", output);
    return 2;
  }
  const readback = await readRepairPolicySeeds();
  const target = await resolveRepairPolicyTarget(readback);
  const seed = target.seed;
  const policy = policyPda(seed);
  const policyAccount = await getAccount(policy, "finalized");
  if (!policyAccount) {
    const output = {
      schema: REPAIR_EXECUTION_SCHEMA,
      step: "repair",
      policyProvenanceStatement: POLICY_PROVENANCE_STATEMENT,
      sent: false,
      broadcast: false,
      verdict: "PENDING_REPAIR_POLICY",
      seed: seed.toString(),
      expectedSeed: seed.toString(),
      policy,
      reason: `finalized repair policy seed ${seed} (${policy}) is absent; run repair-policy, await finalized readback, then rerun repair`,
      targetSource: target.source,
      settingsReadback: settingsPolicyReadback(target.settings),
      policyJournal: target.policyJournal,
      seedReadback: readback,
      state: summarize(state),
    };
    console.log(toJson(output, 2));
    writeEvidence("repair", output);
    return 2;
  }
  const provenance = await verifyFinalizedPolicyCreationJournal(rpcUrl, target.policyJournal);
  if (provenance.seed !== seed || provenance.policy !== policy) {
    throw new Error("repair policy provenance does not match the derived seed-140 target");
  }
  const decoded = decodeSquadsPolicy(policyAccount);
  const semantics = repairPolicySemantics(decoded, seed, provenance.compiledPolicy);
  if (!semantics.identityPass || !semantics.payloadPass || !semantics.digestUnconstrained
    || !semantics.navExact.pass || !semantics.compiledMatch) {
    throw new Error(`finalized repair policy seed ${seed} is present but is not the exact one-shot contract`);
  }
  const built = await buildRepairExecution(policy, state);
  let preSimulationSlot: number;
  let preSimulationAge: ReportSlotAge;
  let simulation: Simulation;
  let reportSlotAgeAtSimulate: ReportSlotAge;
  try {
    preSimulationSlot = await readConfirmedSlot();
    preSimulationAge = assertReportSlotFresh(
      built.report.observedSlot,
      preSimulationSlot,
      "pre-simulate",
    );
    simulation = await simulate(DELEGATED_EXECUTOR, [built.execution], [
      policy, SQUADS_SETTINGS, VAULT, IDLE_ATA, RECEIPT1, CUSTODY1, LP_MINT, REPORT_TICKET, SQUADS_USDC_ATA,
    ]);
    const simulationContextSlot = simulation.contextSlot ?? preSimulationSlot;
    reportSlotAgeAtSimulate = assertReportSlotFresh(
      built.report.observedSlot,
      simulationContextSlot,
      "pre-simulate",
    );
  } catch (error) {
    if (!(error instanceof ReportSlotStaleError)) throw error;
    const output = {
      schema: REPAIR_EXECUTION_SCHEMA,
      step: "repair",
      sent: false,
      signed: false,
      broadcast: false,
      verdict: error.verdict,
      reason: error.message,
      reportSlot: error.age,
      observationSlot: state.contextSlot,
      report: {
        sequence: built.report.sequence.toString(),
        observedSlot: built.report.observedSlot.toString(),
        navAfterRaw: built.report.navAfterRaw.toString(),
      },
    };
    console.log(toJson(output, 2));
    writeEvidence("repair", output);
    return 1;
  }
  const post = simulation.err === null
    ? repairPoststate(simulation.postAccounts, state, built.report.sequence)
    : { postState: null, checks: [] } as const;
  const checks = [
    checkRow("policy is the exact one-shot ProgramInteraction", semantics.payloadPass, true, semantics.payloadPass),
    checkRow("outer ExecuteSync uses the Squads program", built.execution.programAddress === SQUADS_PROGRAM,
      SQUADS_PROGRAM, built.execution.programAddress),
    checkRow("simulation succeeds", simulation.err === null, null,
      simulation.err === null ? null : JSON.stringify(simulation.err)),
    ...post.checks,
  ];
  const pass = checks.every((check) => check.pass);
  const output = {
    schema: REPAIR_EXECUTION_SCHEMA,
    step: "repair",
    sent: false,
    broadcast: false,
    verdict: pass ? "SIMULATION_PASS_UNSENT" : "SIMULATION_FAILED",
    seed: seed.toString(),
    expectedSeed: seed.toString(),
    policy,
    targetSource: target.source,
    settingsReadback: settingsPolicyReadback(target.settings),
    policyJournal: target.policyJournal,
    seedReadback: readback,
    observationSlot: state.contextSlot,
    reportSlot: {
      maxAgeSlots: ADAPTOR_MAX_REPORT_AGE_SLOTS,
      marginSlots: REPORT_AGE_MARGIN_SLOTS,
      preSimulationCurrentSlot: preSimulationSlot,
      simulationContextSlot: simulation.contextSlot,
      reportSlotAgeAtSimulate: reportSlotAgeAtSimulate.ageSlots,
      reportSlotAgeAtSend: null,
      preSimulationAge: preSimulationAge.ageSlots,
    },
    frozenPrestate: {
      totalValue: REPAIR_FROZEN.totalValue.toString(),
      idleBalance: REPAIR_FROZEN.idleBalance.toString(),
      receipt1PositionValue: REPAIR_FROZEN.receipt1PositionValue.toString(),
      custody1Balance: REPAIR_FROZEN.custody1Balance.toString(),
      lpSupply: REPAIR_FROZEN.lpSupply.toString(),
      degradation: REPAIR_FROZEN.degradation.toString(),
      adminPerformanceFeeBps: REPAIR_FROZEN.adminPerformanceFeeBps,
      withdrawalWaitingPeriod: REPAIR_FROZEN.waitingPeriod.toString(),
      requestReceipt: REPAIR_FROZEN.requestReceipt,
      ticketLastConsumedSequence: REPAIR_FROZEN.ticketLastConsumedSequence.toString(),
    },
    policyReadback: {
      finalizedDataBytes: policyAccount.data.length,
      finalizedDataSha256: createHash("sha256").update(policyAccount.data).digest("hex"),
      constraints: stablePolicyConstraints(semantics.decoded),
      compiledConstraints: stablePolicyConstraints(provenance.compiledPolicy),
      decodedPolicy: semantics.decoded,
    },
    policyProvenance: {
      continuityPin: POLICY_PROVENANCE_STATEMENT,
      creationSignature: provenance.creationSignature,
      creationMessageSha256: provenance.creationMessageSha256,
      policyDataBytes: provenance.policyDataBytes,
      policyDataSha256: provenance.policyDataSha256,
      settingsIdentity: provenance.settingsIdentity,
      compiledPolicy: provenance.compiledPolicy,
    },
    before: summarize(state),
    report: {
      sequence: built.report.sequence.toString(),
      observedSlot: built.report.observedSlot.toString(),
      navAfterRaw: built.report.navAfterRaw.toString(),
      snapshotDigest: Buffer.from(built.report.snapshotDigest).toString("hex"),
    },
    compiler: {
      ...built.executionArtifact.compiler,
      schema: built.executionArtifact.schema,
      sourceSha256: built.executionArtifact.sourceSha256,
      constraintIndices: [0, 1],
    },
    transaction: {
      feePayer: DELEGATED_EXECUTOR,
      delegatedSigner: DELEGATED_EXECUTOR,
      signerEnvVar: "POLICY_KEYPAIR",
      packetBytes: simulation.packetBytes,
      simulationContextSlot: simulation.contextSlot,
      instructionCount: 1,
      unitsConsumed: simulation.unitsConsumed,
      outer: wireFromInstruction(built.execution),
      inner: [wireFromInstruction(built.arm), wireFromInstruction(built.capital)],
    },
    checks,
    postState: post.postState,
    ...(simulation.err === null ? {} : { logs: simulation.logs }),
  };
  console.log(toJson(output, 2));
  writeEvidence("repair", output);
  return pass ? 0 : 1;
}

async function cmdRepairOperator(mode: RepairPolicyOperatorMode): Promise<number> {
  assertNoArbitraryRepairSeed();
  const journal = operatorJournal();
  const rpcUrl = operatorRpcUrl();
  const policyJournal = optionalPolicyJournalPath();
  if (policyJournal === null) throw new Error(`${POLICY_JOURNAL_FLAG} is required for repair`);
  const policyRemoveJournal = repairPolicyRemoveJournalPath(journal);
  const recoveryCommands = () => repairRecoveryCommands({
    repairJournal: journal,
    policyJournal,
    policyRemoveJournal,
  });
  let repairResult: number;
  try {
    repairResult = await runJournaledStep({
      mode,
      step: "repair",
      schema: REPAIR_EXECUTION_SCHEMA,
      journal,
      rpcUrl,
      build: async () => {
      const state = await readState("finalized", true);
      await assertRepairNotAlreadyApplied(state, rpcUrl);
      repairPrecondition(state);
      const readback = await readRepairPolicySeeds();
      const target = await resolveRepairPolicyTarget(readback);
      const seed = target.seed;
      assertRepairPolicyHardBinding(seed, policyPda(seed));
      const policy = policyPda(seed);
      const policyJournal = target.policyJournal;
      if (policyJournal === null) {
        throw new Error(`${POLICY_JOURNAL_FLAG} is required for repair`);
      }
      const provenance = await verifyFinalizedPolicyCreationJournal(rpcUrl, policyJournal);
      if (provenance.seed !== seed || provenance.policy !== policy) {
        throw new Error("repair policy provenance does not match the derived seed-140 target");
      }
      const policyAccount = await getAccount(policy, "finalized");
      if (!policyAccount) {
        throw new Error(`PENDING_REPAIR_POLICY: finalized repair policy seed ${seed} (${policy}) is absent; run repair-policy, await finalized readback, then rerun repair`);
      }
      const policyDataSha256 = createHash("sha256").update(policyAccount.data).digest("hex");
      const semantics = repairPolicySemantics(
        decodeSquadsPolicy(policyAccount),
        seed,
        provenance.compiledPolicy,
      );
      if (!semantics.identityPass || !semantics.payloadPass || !semantics.digestUnconstrained
        || !semantics.navExact.pass || !semantics.compiledMatch) {
        throw new Error(`finalized repair policy seed ${seed} is present but is not the exact one-shot contract`);
      }
      const built = await buildRepairExecution(policy, state);
      // Read the confirmed tip after every policy/readback/compiler step and
      // immediately before the signed simulation. The adaptor evaluates the
      // confirmed simulation bank using the prepared blockhash.
      const preSimulationSlot = await readConfirmedSlot();
      const preSimulationAge = assertReportSlotFresh(
        built.report.observedSlot,
        preSimulationSlot,
        "pre-simulate",
      );
      const delegated = await signingMaterialFromEnvironment("POLICY_KEYPAIR");
      if (delegated.signer.address !== DELEGATED_EXECUTOR) {
        throw new Error("POLICY_KEYPAIR is not the HXtk delegated executor signer");
      }
      const inspectedAddresses = [
        policy, SQUADS_SETTINGS, VAULT, IDLE_ATA, RECEIPT1, CUSTODY1, LP_MINT, REPORT_TICKET, SQUADS_USDC_ATA,
      ];
      const prepared = await prepareSignedV0Transaction({
        rpcUrl,
        feePayer: delegated,
        instructions: [built.execution],
        inspectedAddresses,
        prestateAddresses: inspectedAddresses,
        minimumContextSlot: state.contextSlot,
        // The report is stamped from the confirmed tip. Prepare and simulate
        // at confirmed too: finalized banks can lag by about 31 slots, which
        // is incompatible with the adaptor's 32-slot report-age window.
        commitment: "confirmed",
      });
      const reportSlotAgeAtSimulate = assertReportSlotFresh(
        built.report.observedSlot,
        prepared.simulationSlot,
        "pre-simulate",
      );
      const postAccounts = prepared.simulation.postAccounts.map((account, index) =>
        rawFromPreparedSnapshot(inspectedAddresses[index]!, account
          ? { owner: account.owner, lamports: account.lamports, data: account.data }
          : null));
      const post = repairPoststate(postAccounts, state, built.report.sequence);
      if (!post.checks.every((check) => check.pass)) {
        throw new Error(`signed repair simulation did not project the exact post-state: ${JSON.stringify(post.checks)}`);
      }
      const plan: JsonRecord = {
        seed: seed.toString(),
        expectedSeed: seed.toString(),
        policy,
        targetSource: target.source,
        settingsReadback: settingsPolicyReadback(target.settings),
        policyJournal: target.policyJournal,
        policyRemoveJournal: repairPolicyRemoveJournalPath(journal),
        seedReadback: readback,
        policyReadback: {
          finalizedDataBytes: policyAccount.data.length,
          finalizedDataSha256: policyDataSha256,
          constraints: stablePolicyConstraints(semantics.decoded),
          compiledConstraints: stablePolicyConstraints(provenance.compiledPolicy),
          decodedPolicy: semantics.decoded,
        },
        policyProvenance: {
          continuityPin: POLICY_PROVENANCE_STATEMENT,
          creationSignature: provenance.creationSignature,
          creationMessageSha256: provenance.creationMessageSha256,
          policyDataBytes: provenance.policyDataBytes,
          policyDataSha256: provenance.policyDataSha256,
          settingsIdentity: provenance.settingsIdentity,
          compiledPolicy: provenance.compiledPolicy,
        },
        before: summarize(state),
        observationSlot: state.contextSlot,
        reportSlot: {
          maxAgeSlots: ADAPTOR_MAX_REPORT_AGE_SLOTS,
          marginSlots: REPORT_AGE_MARGIN_SLOTS,
          preSimulationCurrentSlot: preSimulationSlot,
          simulationContextSlot: prepared.simulationSlot,
          reportSlotAgeAtSimulate: reportSlotAgeAtSimulate.ageSlots,
          reportSlotAgeAtSend: null,
          preSimulationAge: preSimulationAge.ageSlots,
        },
        expectedPreState: summarize(state),
        prestateExpectations: {
          totalValue: REPAIR_FROZEN.totalValue.toString(),
          idleBalance: REPAIR_FROZEN.idleBalance.toString(),
          receipt1PositionValue: REPAIR_FROZEN.receipt1PositionValue.toString(),
          custody1Balance: REPAIR_FROZEN.custody1Balance.toString(),
          lpSupply: REPAIR_FROZEN.lpSupply.toString(),
          lockedProfitDegradationDuration: REPAIR_FROZEN.degradation.toString(),
          adminPerformanceFeeBps: REPAIR_FROZEN.adminPerformanceFeeBps,
          withdrawalWaitingPeriod: REPAIR_FROZEN.waitingPeriod.toString(),
          requestReceipt: REPAIR_FROZEN.requestReceipt,
          requestEscrowLpBalance: REPAIR_FROZEN.requestEscrowLpBalance.toString(),
          ticketLastConsumedSequence: REPAIR_FROZEN.ticketLastConsumedSequence.toString(),
        },
        report: {
          sequence: built.report.sequence.toString(),
          observedSlot: built.report.observedSlot.toString(),
          navAfterRaw: built.report.navAfterRaw.toString(),
          snapshotDigest: Buffer.from(built.report.snapshotDigest).toString("hex"),
        },
        compiler: {
          ...built.executionArtifact.compiler,
          schema: built.executionArtifact.schema,
          sourceSha256: built.executionArtifact.sourceSha256,
          constraintIndices: [0, 1],
        },
        expectedPostState: post.postState,
        postState: post.postState,
        transaction: {
          feePayer: DELEGATED_EXECUTOR,
          delegatedSigner: DELEGATED_EXECUTOR,
          signerEnvVar: "POLICY_KEYPAIR",
          instructionCount: 1,
          outer: wireFromInstruction(built.execution),
          inner: [wireFromInstruction(built.arm), wireFromInstruction(built.capital)],
        },
        checks: post.checks,
      };
      return {
        prepared,
        plan,
        beforeSend: async ({ pendingJournal }) => {
          // Re-run the same atomic finalized account snapshot immediately
          // before the only raw send. A changed vault/book/ticket/request
          // value aborts the pending journal rather than sending a stale NAV.
          const current = await readState("finalized", true);
          if (repairStateFingerprint(current) !== repairStateFingerprint(state)) {
            throw new Error(
              `ABORTED_PRE_SEND: finalized repair snapshot changed after signed simulation (initial slot ${state.contextSlot}, `
              + `pre-send slot ${current.contextSlot})`,
            );
          }
          // Persist the send-age evidence before the final freshness assertion
          // so the healthy path has no journal/fsync work between that check
          // and the attempted mark. A stale report uses the normal elected
          // aborted-pre-send path, so its signed wire is re-runnable with the
          // same journal.
          const currentSlot = await readConfirmedSlot();
          const reportSlotAgeAtSend = reportSlotAge(
            built.report.observedSlot,
            currentSlot,
          );
          rewritePendingStatus(pendingJournal, {
            sendStatus: {
              reportSlotAgeAtSend: reportSlotAgeAtSend.ageSlots,
              reportSlotCurrentSlotAtSend: currentSlot,
              reportSlotObservedSlot: built.report.observedSlot,
              reportSlotMarginSlots: REPORT_AGE_MARGIN_SLOTS,
              reportSlotMaxAgeSlots: ADAPTOR_MAX_REPORT_AGE_SLOTS,
            },
          });
          assertReportSlotFresh(built.report.observedSlot, currentSlot, "pre-send");
        },
      };
      },
      reconcile: async ({ pending }) => {
      const seed = parseSeed(String(pending.seed ?? pending.expectedSeed ?? ""), "repair journal seed");
      const policy = address(stringAt(pending.policy, "repair journal policy"));
      assertRepairPolicyHardBinding(seed, policy);
      const policyJournal = stringAt(pending.policyJournal, "repair journal policyJournal");
      const provenance = await verifyFinalizedPolicyCreationJournal(rpcUrl, resolve(policyJournal));
      if (provenance.seed !== seed || provenance.policy !== policy) {
        throw new Error("finalized repair journal policy provenance does not match its policy target");
      }
      const policyAccount = await getAccount(policy, "finalized");
      const semantics = repairPolicySemantics(
        decodeSquadsPolicy(policyAccount),
        seed,
        provenance.compiledPolicy,
      );
      const policyDataSha256 = policyAccount
        ? createHash("sha256").update(policyAccount.data).digest("hex")
        : null;
      if (!policyAccount || !semantics.identityPass || !semantics.payloadPass
        || !semantics.digestUnconstrained || !semantics.navExact.pass || !semantics.compiledMatch) {
        throw new Error("finalized repair policy is not the exact one-shot contract");
      }
      const state = await readState("finalized", true);
      const report = recordAt(pending.report, "report");
      const expectations = recordAt(pending.prestateExpectations, "prestateExpectations");
      const sequence = BigInt(stringAt(report.sequence, "report.sequence"));
      const post = repairChecksAgainst(
        repairObservedState(state),
        BigInt(stringAt(expectations.lpSupply, "prestateExpectations.lpSupply")),
        BigInt(stringAt(expectations.lockedProfitDegradationDuration, "prestateExpectations.lockedProfitDegradationDuration")),
        sequence,
      );
      if (!post.checks.every((check) => check.pass)) {
        throw new Error(`finalized repair post-state did not reconcile: ${JSON.stringify(post.checks)}`);
      }
      return {
        finalizedPolicyDataBytes: policyAccount.data.length,
        finalizedPolicyDataSha256: policyDataSha256,
        policyProvenance: {
          continuityPin: POLICY_PROVENANCE_STATEMENT,
          creationSignature: provenance.creationSignature,
          creationMessageSha256: provenance.creationMessageSha256,
          policyDataBytes: provenance.policyDataBytes,
          policyDataSha256: provenance.policyDataSha256,
          settingsIdentity: provenance.settingsIdentity,
          compiledPolicy: provenance.compiledPolicy,
        },
        finalizedPostState: post.postState,
        finalizedState: summarize(state),
      };
      },
    }, {
      // Repair alone settles at confirmed; its existing finalized readback,
      // reconciliation, and expiry path remains the completion boundary.
      sendPreparedOnce: sendPreparedConfirmedOnce,
    });
  } catch (error) {
    const canonicalRepairState = readCanonicalLegState("repair");
    const status = repairPostFinalizationStatus(canonicalRepairState?.status);
    if (status === null) throw error;
    console.error(toJson({
      schema: REPAIR_EXECUTION_SCHEMA,
      step: "repair",
      status,
      verdict: status,
      error: sanitizeError(error),
      repairJournal: journal,
      policyRemoveJournal,
      recoveryCommands: recoveryCommands(),
    }, 2));
    return 1;
  }
  if (mode !== "execute" || repairResult !== 0) return repairResult;
  try {
    // A finalized repair is not a usable reset milestone until its one-shot
    // policy is retired. Keep removal as a separate journaled leg while
    // making it inseparable from the normal --execute repair invocation.
    return await cmdRepairPolicyRemoveOperator("execute", {
      journal: policyRemoveJournal,
      repairJournal: journal,
    });
  } catch (error) {
    console.error(toJson({
      schema: REPAIR_EXECUTION_SCHEMA,
      step: "repair",
      status: "REPAIR_FINALIZED_POLICY_STILL_PRESENT",
      verdict: "REPAIR_FINALIZED_POLICY_STILL_PRESENT",
      repairJournal: journal,
      policyRemoveJournal,
      error: sanitizeError(error),
      recoveryCommands: recoveryCommands(),
    }, 2));
    return 1;
  }
}

function policyClosed(account: RawAccount): boolean {
  return account === null
    || (account.lamports === 0 && account.owner === SYS_PROGRAM && account.data.length === 0);
}

async function assertRepairPolicyRetired(step: string) {
  const policy = await getAccount(REPAIR_POLICY_EXPECTED_PDA, "finalized");
  if (!policyClosed(policy)) {
    throw new Error(
      `REPAIR_FINALIZED_POLICY_STILL_PRESENT: ${step} refuses while finalized seed-140 policy `
      + `${REPAIR_POLICY_EXPECTED_PDA} exists`,
    );
  }
}

function assertOriginalFrozenReceipt(state: LiveState, step: string) {
  if (state.identity.requestReceiptAddress.toString() !== REQUEST_RECEIPT.toString()
    || state.identity.requestReceiptPdaSeeds.vault.toString() !== VAULT.toString()
    || state.identity.requestReceiptPdaSeeds.userTransferAuthority.toString() !== ADMIN.toString()) {
    throw new Error(
      `RECONCILE_MISMATCH: ${step} is not bound to the original HXtk frozen request receipt ${REQUEST_RECEIPT}`,
    );
  }
  if (state.requestReceipt !== null
    && (state.requestReceipt.vault !== VAULT.toString()
      || state.requestReceipt.userTransferAuthority !== ADMIN.toString())) {
    throw new Error(
      `RECONCILE_MISMATCH: ${step} observed a request receipt with the wrong vault or authority`,
    );
  }
}

function assertOriginalFrozenCancelReceipt(state: LiveState, step: string) {
  assertOriginalFrozenReceipt(state, step);
  if (!state.requestReceipt
    || state.requestReceipt.amountLpEscrowed !== REPAIR_FROZEN.requestEscrowLpBalance
    || state.requestEscrowLpBalance !== REPAIR_FROZEN.requestEscrowLpBalance) {
    throw new Error(
      `RECONCILE_MISMATCH: ${step} is not bound to the original frozen receipt amount `
      + `${REPAIR_FROZEN.requestEscrowLpBalance} LP`,
    );
  }
}

async function cmdRepairPolicyRemoveOperator(
  mode: RepairPolicyOperatorMode,
  overrides: Readonly<{ journal?: string; repairJournal?: string }> = {},
): Promise<number> {
  assertNoArbitraryRepairSeed();
  const journal = overrides.journal ?? operatorJournal();
  const rpcUrl = operatorRpcUrl();
  return runJournaledStep({
    mode,
    step: "repair-policy-remove",
    schema: REPAIR_POLICY_REMOVE_SCHEMA,
    journal,
    rpcUrl,
    build: async () => {
      const readback = await readRepairPolicySeeds();
      const target = await resolveRepairPolicyTarget(readback);
      const seed = target.seed;
      assertRepairPolicyHardBinding(seed, policyPda(seed));
      const policy = policyPda(seed);
      const policyJournal = target.policyJournal;
      if (policyJournal === null) throw new Error(`${POLICY_JOURNAL_FLAG} is required for PolicyRemove`);
      const provenance = await verifyFinalizedPolicyCreationJournal(rpcUrl, policyJournal);
      if (provenance.seed !== seed || provenance.policy !== policy) {
        throw new Error("PolicyRemove policy provenance does not match the seed-140 target");
      }
      const repairJournal = overrides.repairJournal ?? requiredJournalPath(
        REPAIR_JOURNAL_FLAG,
        "PolicyRemove requires the finalized repair journal",
      );
      const repair = await verifyFinalizedRepairJournal(rpcUrl, repairJournal, policyJournal);
      if (repair.seed !== seed || repair.policy !== policy) {
        throw new Error("PolicyRemove repair journal does not match the proven repair policy");
      }
      const policyAccount = await getAccount(policy, "finalized");
      if (!policyAccount) throw new Error(`PENDING_REPAIR_POLICY: finalized repair policy ${policy} is absent; there is no policy to remove`);
      const policyDataSha256 = createHash("sha256").update(policyAccount.data).digest("hex");
      const semantics = repairPolicySemantics(
        decodeSquadsPolicy(policyAccount),
        seed,
        provenance.compiledPolicy,
      );
      if (!semantics.identityPass || !semantics.payloadPass || !semantics.digestUnconstrained
        || !semantics.navExact.pass || !semantics.compiledMatch) {
        throw new Error(`finalized repair policy seed ${seed} is not the exact one-shot contract; refusing removal`);
      }
      const settingsBefore = await getAccount(SQUADS_SETTINGS, "finalized");
      const adminBefore = await getAccount(ADMIN, "finalized");
      if (!settingsBefore || !adminBefore) throw new Error("PolicyRemove prestate omitted Settings or admin");
      const remove = buildPolicyRemoveInstruction(policy);
      const admin = await signingMaterialFromEnvironment("SOLANA_TESTING_PK");
      if (admin.signer.address !== ADMIN) throw new Error("SOLANA_TESTING_PK is not the HXtk admin signer");
      const prepared = await prepareSignedV0Transaction({
        rpcUrl,
        feePayer: admin,
        instructions: [remove],
        inspectedAddresses: [policy, SQUADS_SETTINGS, ADMIN],
        prestateAddresses: [policy, SQUADS_SETTINGS, ADMIN],
        minimumContextSlot: readback.contextSlot,
        commitment: "finalized",
      });
      const projectedPolicy = rawFromPreparedSnapshot(policy, prepared.simulation.postAccounts[0]);
      const projectedSettings = rawFromPreparedSnapshot(SQUADS_SETTINGS, prepared.simulation.postAccounts[1]);
      const projectedAdmin = rawFromPreparedSnapshot(ADMIN, prepared.simulation.postAccounts[2]);
      const projectedSettingsDataSha256 = projectedSettings
        ? createHash("sha256").update(projectedSettings.data).digest("hex")
        : null;
      if (!policyClosed(projectedPolicy) || !projectedSettings || !projectedAdmin
        || projectedSettingsDataSha256 !== createHash("sha256").update(settingsBefore.data).digest("hex")) {
        throw new Error("signed PolicyRemove simulation did not close only the repair policy");
      }
      return {
        prepared,
        plan: {
          expectedPreState: {
            policy: policy,
            policyDataBytes: policyAccount.data.length,
            policyDataSha256,
            settingsDataSha256: createHash("sha256").update(settingsBefore.data).digest("hex"),
          },
          expectedPostState: { policy: "closed", settingsUnchanged: true, adminPresent: true },
          expectedSeed: seed.toString(),
          seed: seed.toString(),
          policy,
          targetSource: target.source,
          policyJournal: target.policyJournal,
          repairJournal,
          policyProvenance: {
            continuityPin: POLICY_PROVENANCE_STATEMENT,
            creationSignature: provenance.creationSignature,
            creationMessageSha256: provenance.creationMessageSha256,
            policyDataBytes: provenance.policyDataBytes,
            policyDataSha256: provenance.policyDataSha256,
            settingsIdentity: provenance.settingsIdentity,
          },
          repairProvenance: {
            finalizedSignature: repair.finalized.transaction.signatures[0] ?? null,
            finalizedSlot: repair.finalized.slot,
            finalizedBlockTime: repair.finalized.blockTime ?? null,
          },
          settingsReadback: settingsPolicyReadback(target.settings),
          seedReadback: readback,
          before: {
            policyDataBytes: policyAccount.data.length,
            policyDataSha256: createHash("sha256").update(policyAccount.data).digest("hex"),
            constraints: stablePolicyConstraints(semantics.decoded),
            decodedPolicy: semantics.decoded,
          },
          transaction: {
            feePayer: ADMIN,
            signer: ADMIN,
            signerEnvVar: "SOLANA_TESTING_PK",
            instructionCount: 1,
            policyRemove: wireFromInstruction(remove),
            projectedSettingsDataSha256,
          },
        },
      };
    },
    reconcile: async ({ pending }) => {
      const seed = parseSeed(String(pending.seed ?? pending.expectedSeed ?? ""), "removal journal seed");
      const policy = address(stringAt(pending.policy, "removal journal policy"));
      assertRepairPolicyHardBinding(seed, policy);
      const policyJournal = stringAt(pending.policyJournal, "removal journal policyJournal");
      const policyCreation = await readFinalizedJournal(
        rpcUrl,
        resolve(policyJournal),
        REPAIR_POLICY_SCHEMA,
        "repair-policy",
      );
      if (address(stringAt(policyCreation.record.policy, "repair-policy journal policy")) !== policy
        || parseSeed(String(policyCreation.record.seed ?? policyCreation.record.expectedSeed ?? ""), "repair-policy journal seed") !== seed) {
        throw new Error("finalized PolicyRemove policy provenance does not match the removal target");
      }
      const policyProvenance = recordAt(pending.policyProvenance, "removal journal policyProvenance");
      if (String(policyProvenance.creationSignature) !== policyCreation.wire.signature
        || String(policyProvenance.creationMessageSha256) !== policyCreation.wire.messageSha256) {
        throw new Error("removal journal policy provenance signature/hash differs from the finalized PolicyCreate");
      }
      const repairJournal = stringAt(pending.repairJournal, "removal journal repairJournal");
      const repair = await verifyFinalizedRepairJournal(rpcUrl, resolve(repairJournal), resolve(policyJournal));
      if (repair.seed !== seed || repair.policy !== policy) {
        throw new Error("finalized PolicyRemove repair journal does not match the policy target");
      }
      const finalizedPolicy = await getAccount(policy, "finalized");
      const finalizedSettings = await getAccount(SQUADS_SETTINGS, "finalized");
      const finalizedAdmin = await getAccount(ADMIN, "finalized");
      const settingsHash = finalizedSettings
        ? createHash("sha256").update(finalizedSettings.data).digest("hex")
        : null;
      const transaction = recordAt(pending.transaction, "transaction");
      if (!policyClosed(finalizedPolicy) || !finalizedSettings || !finalizedAdmin
        || settingsHash !== transaction.projectedSettingsDataSha256) {
        throw new Error("finalized PolicyRemove did not close the policy without changing Settings/admin");
      }
      return {
        finalizedPolicyClosed: true,
        finalizedSettingsDataSha256: settingsHash,
      };
    },
  });
}

async function cmdRepairPolicyRemove(): Promise<number> {
  const mode = operatorMode();
  if (mode) return cmdRepairPolicyRemoveOperator(mode);
  assertNoArbitraryRepairSeed();
  if (currentCli().has("--journal")) {
    throw new Error("repair-policy-remove --journal is not supported");
  }
  if (!currentCli().has(POLICY_JOURNAL_FLAG) || !currentCli().has(REPAIR_JOURNAL_FLAG)) {
    const output = {
      schema: REPAIR_POLICY_REMOVE_SCHEMA,
      step: "repair-policy-remove",
      policyProvenanceStatement: POLICY_PROVENANCE_STATEMENT,
      sent: false,
      signed: false,
      broadcast: false,
      verdict: "PENDING_FINALIZED_POLICY_AND_REPAIR",
      reason: `${POLICY_JOURNAL_FLAG} and ${REPAIR_JOURNAL_FLAG} are required; removal follows the proven PolicyCreate and finalized repair journals`,
    };
    console.log(toJson(output, 2));
    writeEvidence("repair-policy-remove", output);
    return 2;
  }
  const rpcUrl = process.env.SOLANA_RPC_URL?.trim() || DEFAULT_RPC_URL;
  const readback = await readRepairPolicySeeds();
  const target = await resolveRepairPolicyTarget(readback);
  const seed = target.seed;
  const policy = policyPda(seed);
  const policyJournal = target.policyJournal;
  if (policyJournal === null) throw new Error(`${POLICY_JOURNAL_FLAG} is required for PolicyRemove`);
  const provenance = await verifyFinalizedPolicyCreationJournal(rpcUrl, policyJournal);
  if (provenance.seed !== seed || provenance.policy !== policy) {
    throw new Error("PolicyRemove policy provenance does not match the seed-140 target");
  }
  const repairJournalPath = requiredJournalPath(
    REPAIR_JOURNAL_FLAG,
    "PolicyRemove requires the finalized repair journal",
  );
  const repair = await verifyFinalizedRepairJournal(rpcUrl, repairJournalPath, policyJournal);
  if (repair.seed !== seed || repair.policy !== policy) {
    throw new Error("PolicyRemove repair journal does not match the proven policy");
  }
  const policyAccount = await getAccount(policy, "finalized");
  if (!policyAccount) {
    const output = {
      schema: REPAIR_POLICY_REMOVE_SCHEMA,
      step: "repair-policy-remove",
      policyProvenanceStatement: POLICY_PROVENANCE_STATEMENT,
      sent: false,
      broadcast: false,
      verdict: "PENDING_REPAIR_POLICY",
      seed: seed.toString(),
      expectedSeed: seed.toString(),
      policy,
      reason: `finalized repair policy seed ${seed} (${policy}) is absent; there is no policy to remove`,
      targetSource: target.source,
      settingsReadback: settingsPolicyReadback(target.settings),
      policyJournal: target.policyJournal,
      seedReadback: readback,
    };
    console.log(toJson(output, 2));
    writeEvidence("repair-policy-remove", output);
    return 2;
  }
  const policyDataSha256 = createHash("sha256").update(policyAccount.data).digest("hex");
  const decoded = decodeSquadsPolicy(policyAccount);
  const semantics = repairPolicySemantics(decoded, seed, provenance.compiledPolicy);
  if (!semantics.identityPass || !semantics.payloadPass || !semantics.digestUnconstrained
    || !semantics.navExact.pass || !semantics.compiledMatch) {
    throw new Error(`finalized repair policy seed ${seed} is not the exact one-shot contract; refusing removal`);
  }
  const settingsBefore = await getAccount(SQUADS_SETTINGS, "finalized");
  const adminBefore = await getAccount(ADMIN, "finalized");
  if (!settingsBefore || !adminBefore) throw new Error("PolicyRemove prestate omitted Settings or admin");
  const remove = buildPolicyRemoveInstruction(policy);
  const simulation = await simulate(ADMIN, [remove], [policy, SQUADS_SETTINGS, ADMIN]);
  const postPolicy = postAccount(simulation.postAccounts, policy);
  const postSettings = postAccount(simulation.postAccounts, SQUADS_SETTINGS);
  const postAdmin = postAccount(simulation.postAccounts, ADMIN);
  const checks = [
    checkRow("simulation succeeds", simulation.err === null, null,
      simulation.err === null ? null : JSON.stringify(simulation.err)),
    checkRow("PolicyRemove closes the one-shot policy", simulation.err === null && postPolicy === null,
      "closed", postPolicy === null ? "closed" : "present"),
    checkRow("Settings owner and bytes remain unchanged", postSettings?.owner === settingsBefore.owner
      && postSettings !== null
      && createHash("sha256").update(postSettings.data).digest("hex")
        === createHash("sha256").update(settingsBefore.data).digest("hex"),
    settingsBefore.owner, postSettings?.owner ?? null),
    checkRow("admin remains present after rent refund", postAdmin?.owner === adminBefore.owner,
      adminBefore.owner, postAdmin?.owner ?? null),
  ];
  const pass = checks.every((check) => check.pass);
  const output = {
    schema: REPAIR_POLICY_REMOVE_SCHEMA,
    step: "repair-policy-remove",
    sent: false,
    broadcast: false,
    verdict: pass ? "SIMULATION_PASS_UNSENT" : "SIMULATION_FAILED",
    seed: seed.toString(),
    expectedSeed: seed.toString(),
    policy,
    targetSource: target.source,
    settingsReadback: settingsPolicyReadback(target.settings),
    policyJournal: target.policyJournal,
    repairJournal: repairJournalPath,
    policyProvenance: {
      continuityPin: POLICY_PROVENANCE_STATEMENT,
      creationSignature: provenance.creationSignature,
      creationMessageSha256: provenance.creationMessageSha256,
      policyDataBytes: provenance.policyDataBytes,
      policyDataSha256: provenance.policyDataSha256,
      settingsIdentity: provenance.settingsIdentity,
    },
    repairProvenance: {
      finalizedSignature: repair.finalized.transaction.signatures[0] ?? null,
      finalizedSlot: repair.finalized.slot,
      finalizedBlockTime: repair.finalized.blockTime ?? null,
    },
    seedReadback: readback,
    before: {
      policyDataBytes: policyAccount.data.length,
      policyDataSha256,
      constraints: stablePolicyConstraints(semantics.decoded),
      decodedPolicy: semantics.decoded,
    },
    transaction: {
      feePayer: ADMIN,
      signer: ADMIN,
      signerEnvVar: "SOLANA_TESTING_PK",
      packetBytes: simulation.packetBytes,
      instructionCount: 1,
      unitsConsumed: simulation.unitsConsumed,
      policyRemove: wireFromInstruction(remove),
    },
    checks,
    ...(simulation.err === null ? {} : { logs: simulation.logs }),
  };
  console.log(toJson(output, 2));
  writeEvidence("repair-policy-remove", output);
  return pass ? 0 : 1;
}

// ---- verify ------------------------------------------------------------------

async function cmdVerify(): Promise<number> {
  const state = await readState();
  const summary = summarize(state);
  const navRepairRaw = state.vault && state.idleBalance !== null && state.receipt1
    ? state.idleBalance + state.receipt1.positionValue - state.vault.totalValue
    : null;
  const ticket = state.reportTicket;
  const endState = {
    tvEqualsIdle: summary.tvEqualsIdle,
    receipt1NotBelowPhantom: (state.receipt1?.positionValue ?? 0n) >= PHANTOM_NAV_RAW,
    receipt1EqualsPhantom: state.receipt1?.positionValue === PHANTOM_NAV_RAW,
    feeAccumulatorsZero: (state.vault?.accumulatedLpAdminFees ?? 1n) === 0n
      && (state.vault?.accumulatedLpManagerFees ?? 1n) === 0n
      && (state.vault?.accumulatedLpProtocolFees ?? 1n) === 0n,
    custody1Zero: (state.custody1Balance ?? 1n) === 0n,
    degradationZero: (state.vault?.lockedProfitDegradationDuration ?? 1n) === 0n,
    performanceFeesZero: (state.vault?.adminPerformanceFeeBps ?? 1) === 0
      && (state.vault?.managerPerformanceFeeBps ?? 1) === 0,
    waitingPeriodIs600: (state.vault?.withdrawalWaitingPeriod ?? 0n) === REQUEST_WAITING_PERIOD_SECONDS,
  };
  const phase1Complete = endState.tvEqualsIdle === true
    && endState.receipt1NotBelowPhantom
    && endState.feeAccumulatorsZero
    && endState.custody1Zero;
  const output = {
    step: "verify",
    sent: false,
    verdict: phase1Complete ? "PHASE1_END_STATE_OK" : "PRE_RESET_OR_PARTIAL",
    phase1Complete,
    endStateChecks: endState,
    preconditions: {
      reportTicketArmed: ticket?.armed ?? null,
      reportTicketCoherent: ticket
        ? (!ticket.armed && ticket.activeSequence === 0n && ticket.activeHashIsZero)
          || (ticket.armed && ticket.activeSequence !== 0n && !ticket.activeHashIsZero)
        : null,
      ticketLastConsumedSequence: ticket?.lastConsumedSequence.toString() ?? null,
      computedRepairNavRaw: navRepairRaw === null ? null : navRepairRaw.toString(),
      computedRepairNavMatchesPhantom: navRepairRaw === PHANTOM_NAV_RAW,
      openWithdrawRequest: state.requestReceipt,
    },
    aborts: {
      reportTicketArmed: ticket?.armed === true,
      custody1NonZero: (state.custody1Balance ?? 0n) !== 0n,
      // The gate that matters: the NAV the repair reports must never be cranked
      // below the phantom. receipt1 itself may legitimately sit below it (§6).
      repairNavBelowPhantom: navRepairRaw === null || navRepairRaw < PHANTOM_NAV_RAW,
    },
    state: summary,
  };
  console.log(toJson(output, 2));
  writeEvidence("verify", {
    verdict: output.verdict,
    phase1Complete,
    endStateChecks: endState,
    preconditions: output.preconditions,
    aborts: output.aborts,
    state: summary,
  });
  return 0;
}

// ---- config ------------------------------------------------------------------

async function cmdConfig(): Promise<number> {
  const mode = operatorMode();
  if (mode) return cmdConfigOperator(mode);
  const state = await readState();
  if (!state.vault) throw new Error("vault account is absent");
  if (state.vault.admin !== ADMIN) {
    throw new Error(`vault admin ${state.vault.admin} is not ${ADMIN}`);
  }
  const before = summarize(state);

  const noopAdmin = createNoopSigner(ADMIN);
  const degradation = await getUpdateVaultConfigInstructionAsync({
    admin: noopAdmin,
    protocol: PROTOCOL,
    vault: VAULT,
    rent: RENT_SYSVAR,
    field: VaultConfigField.LockedProfitDegradationDuration,
    data: new Uint8Array(8),
  }, { programAddress: VOLTR });
  const adminFee = await getUpdateVaultConfigInstructionAsync({
    admin: noopAdmin,
    protocol: PROTOCOL,
    vault: VAULT,
    rent: RENT_SYSVAR,
    field: VaultConfigField.AdminPerformanceFee,
    data: new Uint8Array(2),
  }, { programAddress: VOLTR });
  for (const instruction of [degradation, adminFee]) {
    const accounts = instruction.accounts ?? [];
    if (accounts.length !== 4) {
      throw new Error(`updateVaultConfig has ${accounts.length} accounts; expected admin/protocol/vault/rent`);
    }
    if (accounts[0]?.address !== ADMIN || accounts[2]?.address !== VAULT) {
      throw new Error("updateVaultConfig account list drifted from admin/protocol/vault/rent");
    }
  }

  const simulation = await simulate(ADMIN, [degradation, adminFee], [VAULT]);
  const events = simulation.err === null ? decodeEvents("UpdateVaultConfig", simulation.logs) : [];

  type ConfigEvent = { field?: string; oldValue?: string; newValue?: string };
  const degradationEvent = events.find((event) => (event as ConfigEvent).field === "LockedProfitDegradationDuration") as ConfigEvent | undefined;
  const feeEvent = events.find((event) => (event as ConfigEvent).field === "AdminPerformanceFee") as ConfigEvent | undefined;
  const postVault = simulation.err === null ? decodeVault(postAccount(simulation.postAccounts, VAULT)) : null;
  const checks = [
    {
      check: "simulation succeeds",
      pass: simulation.err === null,
      expected: null,
      actual: simulation.err === null ? null : JSON.stringify(simulation.err),
    },
    {
      check: "lockedProfitDegradationDuration -> 0",
      pass: postVault?.lockedProfitDegradationDuration === 0n,
      expected: "0",
      actual: postVault?.lockedProfitDegradationDuration.toString() ?? null,
    },
    {
      check: "adminPerformanceFeeBps -> 0",
      pass: postVault?.adminPerformanceFeeBps === 0,
      expected: 0,
      actual: postVault?.adminPerformanceFeeBps ?? null,
    },
    {
      check: "books untouched (tv, idle, lp supply, receipt1, custody1)",
      pass: postVault?.totalValue === state.vault.totalValue
        && state.idleBalance !== null
        && state.receipt1 !== null,
      expected: state.vault.totalValue.toString(),
      actual: postVault?.totalValue.toString() ?? null,
    },
    {
      check: "event 1: LockedProfitDegradationDuration 86400 -> 0",
      pass: degradationEvent?.oldValue === "86400" && degradationEvent?.newValue === "0",
      expected: "86400 -> 0",
      actual: degradationEvent ? `${degradationEvent.oldValue} -> ${degradationEvent.newValue}` : null,
    },
    {
      check: "event 2: AdminPerformanceFee 500 -> 0",
      pass: feeEvent?.oldValue === "500" && feeEvent?.newValue === "0",
      expected: "500 -> 0",
      actual: feeEvent ? `${feeEvent.oldValue} -> ${feeEvent.newValue}` : null,
    },
  ];
  const pass = checks.every((row) => row.pass);
  const output = {
    step: "config",
    sent: false,
    verdict: pass ? "SIMULATION_PASS_UNSENT" : "SIMULATION_FAILED",
    broadcast: false,
    before,
    transaction: {
      packetBytes: simulation.packetBytes,
      unitsConsumed: simulation.unitsConsumed,
      instructions: [
        "updateVaultConfig(LockedProfitDegradationDuration, u64 0)",
        "updateVaultConfig(AdminPerformanceFee, u16 0)",
      ],
      accounts: { admin: ADMIN, protocol: PROTOCOL, vault: VAULT, rent: RENT_SYSVAR },
      signer: ADMIN,
      signerEnvVar: "SOLANA_TESTING_PK",
      executeCommand:
        'op run --env-file=.env.1password -- env CONFIRM_MAINNET=1 bun run reset:hxtk config --execute --journal /absolute/path/hxtk-config.json',
    },
    checks,
    events,
    postState: postVault
      ? {
          lockedProfitDegradationDuration: postVault.lockedProfitDegradationDuration.toString(),
          adminPerformanceFeeBps: postVault.adminPerformanceFeeBps,
          totalValue: postVault.totalValue.toString(),
          admin: postVault.admin,
          manager: postVault.manager,
        }
      : null,
    ...(simulation.err === null ? {} : { logs: simulation.logs }),
  };
  console.log(toJson(output, 2));
  writeEvidence("config", output);
  return pass ? 0 : 1;
}

// ---- phase 2: harvest / cancel / request / claim -----------------------------

function checkRow(label: string, pass: boolean, expected: unknown, actual: unknown) {
  return { check: label, pass, expected, actual };
}

function postToken(postAccounts: readonly RawAccount[], target: Address): bigint | null {
  return tokenAmount(postAccount(postAccounts, target));
}

export type HxtkCancelProofPreState = Readonly<{
  amountLpEscrowed: bigint | null;
  amountAssetToWithdrawRaw: bigint | null;
  totalValue: bigint | null;
  idleBalance: bigint | null;
  lpSupply: bigint | null;
  adminLpBalance: bigint | null;
  receipt1PositionValue: bigint | null;
}>;

export type HxtkCancelSimulationObservation = Readonly<{
  preState: HxtkCancelProofPreState;
  simulationSucceeded: boolean;
  cancelEventCount: number;
  eventRefundLp: bigint | null;
  eventBurnLp: bigint | null;
  escrowAfter: bigint | null;
  requestReceiptLpAfter: bigint | null;
  adminLpDelta: bigint | null;
  adminLpAfter: bigint | null;
  supplyAfter: bigint | null;
  totalValueAfter: bigint | null;
  idleBalanceAfter: bigint | null;
}>;

export function assertHxtkCancelProofPreState(
  actual: HxtkCancelProofPreState,
  context = "cancel",
): void {
  const fields = Object.keys(HXTK_RESET_PROOF_PRESTATE) as Array<keyof HxtkCancelProofPreState>;
  const mismatches = fields
    .filter((field) => actual[field] !== HXTK_RESET_PROOF_PRESTATE[field])
    .map((field) => `${field} expected ${HXTK_RESET_PROOF_PRESTATE[field]} actual ${actual[field] ?? "missing"}`);
  if (mismatches.length > 0) {
    throw new Error(
      `HXTK_RESET_PROOF_STATE_DRIFT: ${context} refuses proof-pinned reset state; ${mismatches.join(", ")}`,
    );
  }
}

export type HxtkPostCancelState = Readonly<{
  adminLpBalance: bigint | null;
  lpSupply: bigint | null;
  totalValue: bigint | null;
  idleBalance: bigint | null;
  receipt1PositionValue: bigint | null;
  requestEscrowLpBalance: bigint | null;
  requestReceiptLp: bigint | null;
}>;

export function assertHxtkPostCancelState(
  actual: HxtkPostCancelState,
  context = "post-cancel",
): void {
  const mismatches = [
    actual.adminLpBalance !== CANCEL_EXPECTED_SUPPLY_AFTER
      ? `adminLpBalance expected ${CANCEL_EXPECTED_SUPPLY_AFTER} actual ${actual.adminLpBalance ?? "missing"}` : null,
    actual.lpSupply !== CANCEL_EXPECTED_SUPPLY_AFTER
      ? `lpSupply expected ${CANCEL_EXPECTED_SUPPLY_AFTER} actual ${actual.lpSupply ?? "missing"}` : null,
    actual.totalValue !== REPAIRED_BOOK_RAW
      ? `totalValue expected ${REPAIRED_BOOK_RAW} actual ${actual.totalValue ?? "missing"}` : null,
    actual.idleBalance !== REPAIRED_BOOK_RAW
      ? `idleBalance expected ${REPAIRED_BOOK_RAW} actual ${actual.idleBalance ?? "missing"}` : null,
    actual.receipt1PositionValue !== PHANTOM_NAV_RAW
      ? `receipt1PositionValue expected ${PHANTOM_NAV_RAW} actual ${actual.receipt1PositionValue ?? "missing"}` : null,
    actual.requestEscrowLpBalance !== 0n
      ? `requestEscrowLpBalance expected 0 actual ${actual.requestEscrowLpBalance ?? "missing"}` : null,
    actual.requestReceiptLp !== null && actual.requestReceiptLp !== 0n
      ? `requestReceiptLp expected closed|0 actual ${actual.requestReceiptLp}` : null,
  ].filter((mismatch): mismatch is string => mismatch !== null);
  if (mismatches.length > 0) {
    throw new Error(`HXTK_RESET_POST_CANCEL_DRIFT: ${context} refuses non-proof state; ${mismatches.join(", ")}`);
  }
}

function hxtkCancelSimulationChecks(input: HxtkCancelSimulationObservation) {
  const expectedSupply = input.preState.lpSupply === null
    ? null
    : input.preState.lpSupply - CANCEL_EXPECTED_BURN_LP;
  return [
    checkRow("simulation succeeds", input.simulationSucceeded, true, input.simulationSucceeded),
    checkRow("exactly one cancel event emitted", input.cancelEventCount === 1, 1, input.cancelEventCount),
    checkRow("cancel event refund equals the proof pin",
      input.eventRefundLp === CANCEL_EXPECTED_REFUND_LP,
      CANCEL_EXPECTED_REFUND_LP.toString(), input.eventRefundLp?.toString() ?? null),
    checkRow("cancel event burn equals the proof pin",
      input.eventBurnLp === CANCEL_EXPECTED_BURN_LP,
      CANCEL_EXPECTED_BURN_LP.toString(), input.eventBurnLp?.toString() ?? null),
    checkRow("escrow drained", input.escrowAfter === 0n, "0", input.escrowAfter?.toString() ?? null),
    checkRow("receipt cleared (closed or amountLpEscrowed 0)",
      input.requestReceiptLpAfter === null || input.requestReceiptLpAfter === 0n,
      "closed|0",
      input.requestReceiptLpAfter === null ? "closed" : input.requestReceiptLpAfter.toString()),
    checkRow("admin LP balance delta equals the documented refund",
      input.adminLpDelta === CANCEL_EXPECTED_REFUND_LP,
      CANCEL_EXPECTED_REFUND_LP.toString(), input.adminLpDelta?.toString() ?? null),
    checkRow("LP supply after equals before minus the documented burn",
      input.supplyAfter === CANCEL_EXPECTED_SUPPLY_AFTER && input.supplyAfter === expectedSupply,
      `${CANCEL_EXPECTED_SUPPLY_AFTER} (${input.preState.lpSupply ?? "missing"} - ${CANCEL_EXPECTED_BURN_LP})`,
      input.supplyAfter?.toString() ?? null),
    checkRow("admin LP after equals 100% of LP supply",
      input.adminLpAfter === CANCEL_EXPECTED_SUPPLY_AFTER && input.adminLpAfter === input.supplyAfter,
      CANCEL_EXPECTED_SUPPLY_AFTER.toString(), input.adminLpAfter?.toString() ?? null),
    checkRow("tv and idle are unchanged",
      input.totalValueAfter !== null && input.totalValueAfter === input.preState.totalValue
        && input.idleBalanceAfter !== null && input.idleBalanceAfter === input.preState.idleBalance,
      `${input.preState.totalValue ?? "missing"}/${input.preState.idleBalance ?? "missing"}`,
      `${input.totalValueAfter?.toString() ?? "missing"}/${input.idleBalanceAfter?.toString() ?? "missing"}`),
  ] as const;
}

export function assertHxtkCancelSimulation(
  input: HxtkCancelSimulationObservation,
  context = "cancel simulation",
) {
  assertHxtkCancelProofPreState(input.preState, context);
  const checks = hxtkCancelSimulationChecks(input);
  const failures = checks.filter((check) => !check.pass).map((check) => check.check);
  if (failures.length > 0) {
    throw new Error(`HXTK_CANCEL_PROOF_MISMATCH: ${context}; failed checks: ${failures.join(", ")}`);
  }
  return checks;
}

export type HxtkRequestSimulationObservation = Readonly<{
  preState: HxtkPostCancelState;
  simulationSucceeded: boolean;
  requestEventCount: number;
  eventRequestedAmount: bigint | null;
  eventIsAmountInLp: boolean | null;
  eventIsWithdrawAll: boolean | null;
  eventReceipt: string | null;
  escrowAfter: bigint | null;
  adminLpAfter: bigint | null;
  supplyAfter: bigint | null;
}>;

function hxtkRequestSimulationChecks(input: HxtkRequestSimulationObservation) {
  return [
    checkRow("simulation succeeds", input.simulationSucceeded, true, input.simulationSucceeded),
    checkRow("exactly one request event emitted", input.requestEventCount === 1, 1, input.requestEventCount),
    checkRow("request event amount equals all post-cancel admin LP",
      input.eventRequestedAmount === REQUEST_EXPECTED_LP,
      REQUEST_EXPECTED_LP.toString(), input.eventRequestedAmount?.toString() ?? null),
    checkRow("request event uses LP amount", input.eventIsAmountInLp === true, true, input.eventIsAmountInLp),
    checkRow("request event uses isWithdrawAll", input.eventIsWithdrawAll === true, true, input.eventIsWithdrawAll),
    checkRow("request event receipt is HXtk's receipt",
      input.eventReceipt === REQUEST_RECEIPT, REQUEST_RECEIPT, input.eventReceipt),
    checkRow("escrow contains all post-cancel LP",
      input.escrowAfter === REQUEST_EXPECTED_LP,
      REQUEST_EXPECTED_LP.toString(), input.escrowAfter?.toString() ?? null),
    checkRow("admin LP ATA emptied", input.adminLpAfter === 0n, "0", input.adminLpAfter?.toString() ?? null),
    checkRow("supply unchanged by request",
      input.supplyAfter === REQUEST_EXPECTED_LP,
      REQUEST_EXPECTED_LP.toString(), input.supplyAfter?.toString() ?? null),
  ] as const;
}

export function assertHxtkRequestSimulation(
  input: HxtkRequestSimulationObservation,
  context = "request simulation",
) {
  assertHxtkPostCancelState(input.preState, context);
  const checks = hxtkRequestSimulationChecks(input);
  const failures = checks.filter((check) => !check.pass).map((check) => check.check);
  if (failures.length > 0) {
    throw new Error(`HXTK_REQUEST_PROOF_MISMATCH: ${context}; failed checks: ${failures.join(", ")}`);
  }
  return checks;
}

export type HxtkClaimProofPreState = Readonly<{
  requestAmountLp: bigint | null;
  totalValue: bigint | null;
  idleBalance: bigint | null;
  lpSupply: bigint | null;
  receipt1PositionValue: bigint | null;
}>;

export function assertHxtkClaimProofPreState(
  actual: HxtkClaimProofPreState,
  context = "claim",
): void {
  const mismatches = [
    actual.requestAmountLp !== REQUEST_EXPECTED_LP
      ? `requestAmountLp expected ${REQUEST_EXPECTED_LP} actual ${actual.requestAmountLp ?? "missing"}` : null,
    actual.totalValue !== REPAIRED_BOOK_RAW
      ? `totalValue expected ${REPAIRED_BOOK_RAW} actual ${actual.totalValue ?? "missing"}` : null,
    actual.idleBalance !== REPAIRED_BOOK_RAW
      ? `idleBalance expected ${REPAIRED_BOOK_RAW} actual ${actual.idleBalance ?? "missing"}` : null,
    actual.lpSupply !== REQUEST_EXPECTED_LP
      ? `lpSupply expected ${REQUEST_EXPECTED_LP} actual ${actual.lpSupply ?? "missing"}` : null,
    actual.receipt1PositionValue !== PHANTOM_NAV_RAW
      ? `receipt1PositionValue expected ${PHANTOM_NAV_RAW} actual ${actual.receipt1PositionValue ?? "missing"}` : null,
  ].filter((mismatch): mismatch is string => mismatch !== null);
  if (mismatches.length > 0) {
    throw new Error(`HXTK_CLAIM_PROOF_STATE_DRIFT: ${context} refuses non-proof state; ${mismatches.join(", ")}`);
  }
}

export type HxtkClaimProofObservation = Readonly<{
  preState: HxtkClaimProofPreState;
  payout: bigint | null;
  lpBurned: bigint | null;
  totalValueAfter: bigint | null;
  idleBalanceAfter: bigint | null;
  receipt1PositionValueAfter: bigint | null;
  requestReceiptClosed: boolean;
  escrowAfter: bigint | null;
}>;

export function assertHxtkClaimProof(
  input: HxtkClaimProofObservation,
  context = "claim",
): void {
  assertHxtkClaimProofPreState(input.preState, context);
  const mismatches = [
    input.payout !== CLAIM_EXPECTED_PAYOUT_RAW
      ? `payout expected ${CLAIM_EXPECTED_PAYOUT_RAW} actual ${input.payout ?? "missing"}` : null,
    input.lpBurned !== REQUEST_EXPECTED_LP
      ? `lpBurned expected ${REQUEST_EXPECTED_LP} actual ${input.lpBurned ?? "missing"}` : null,
    input.totalValueAfter !== CLAIM_EXPECTED_RESIDUAL_RAW
      ? `totalValueAfter expected ${CLAIM_EXPECTED_RESIDUAL_RAW} actual ${input.totalValueAfter ?? "missing"}` : null,
    input.idleBalanceAfter !== CLAIM_EXPECTED_RESIDUAL_RAW
      ? `idleBalanceAfter expected ${CLAIM_EXPECTED_RESIDUAL_RAW} actual ${input.idleBalanceAfter ?? "missing"}` : null,
    input.receipt1PositionValueAfter !== PHANTOM_NAV_RAW
      ? `receipt1PositionValueAfter expected ${PHANTOM_NAV_RAW} actual ${input.receipt1PositionValueAfter ?? "missing"}` : null,
    input.requestReceiptClosed !== true ? "request receipt expected closed" : null,
    input.escrowAfter !== 0n ? `escrowAfter expected 0 actual ${input.escrowAfter ?? "missing"}` : null,
  ].filter((mismatch): mismatch is string => mismatch !== null);
  if (mismatches.length > 0) {
    throw new Error(`HXTK_CLAIM_PROOF_MISMATCH: ${context}; ${mismatches.join(", ")}`);
  }
}

function cancelProofPreStateFromLiveState(state: LiveState): HxtkCancelProofPreState {
  return {
    amountLpEscrowed: state.requestReceipt?.amountLpEscrowed ?? null,
    amountAssetToWithdrawRaw: state.requestReceipt?.amountAssetToWithdrawRaw ?? null,
    totalValue: state.vault?.totalValue ?? null,
    idleBalance: state.idleBalance,
    lpSupply: state.lpSupply,
    adminLpBalance: state.adminLpBalance,
    receipt1PositionValue: state.receipt1?.positionValue ?? null,
  };
}

function postCancelStateFromLiveState(state: LiveState): HxtkPostCancelState {
  return {
    adminLpBalance: state.adminLpBalance,
    lpSupply: state.lpSupply,
    totalValue: state.vault?.totalValue ?? null,
    idleBalance: state.idleBalance,
    receipt1PositionValue: state.receipt1?.positionValue ?? null,
    requestEscrowLpBalance: state.requestEscrowLpBalance,
    requestReceiptLp: state.requestReceipt?.amountLpEscrowed ?? null,
  };
}

function claimProofPreStateFromLiveState(state: LiveState): HxtkClaimProofPreState {
  return {
    requestAmountLp: state.requestReceipt?.amountLpEscrowed ?? null,
    totalValue: state.vault?.totalValue ?? null,
    idleBalance: state.idleBalance,
    lpSupply: state.lpSupply,
    receipt1PositionValue: state.receipt1?.positionValue ?? null,
  };
}

async function createAtaIdempotent(payer: Address, ata: Address, owner: Address, mint: Address): Promise<Instruction> {
  return getCreateAssociatedTokenIdempotentInstruction({
    payer: createNoopSigner(payer),
    ata,
    owner,
    mint,
    tokenProgram: TOKEN_PROGRAM,
  });
}

async function cmdHarvestOperator(mode: RepairPolicyOperatorMode): Promise<number> {
  const journal = operatorJournal();
  const rpcUrl = operatorRpcUrl();
  return runJournaledStep({
    mode,
    step: "harvest",
    schema: SCHEMA,
    journal,
    rpcUrl,
    build: async () => {
      await assertRepairPolicyRetired("harvest");
      const state = await readState("finalized");
      if (!state.vault) throw new Error("vault account is absent");
      const { managerLpAta, treasuryLpAta } = state.identity;
      const noopAdmin = createNoopSigner(ADMIN);
      const adminFeeAccrued = state.vault.accumulatedLpAdminFees;
      const instructions = [
        await createAtaIdempotent(ADMIN, managerLpAta, SQUADS_VAULT, LP_MINT),
        await createAtaIdempotent(ADMIN, treasuryLpAta, PROTOCOL_TREASURY, LP_MINT),
        await getHarvestFeeInstructionAsync({
          harvester: noopAdmin,
          vaultManager: SQUADS_VAULT,
          vaultAdmin: ADMIN,
          protocolTreasury: PROTOCOL_TREASURY,
          protocol: PROTOCOL,
          vault: VAULT,
          vaultLpMint: LP_MINT,
          vaultLpMintAuth: LP_MINT_AUTH,
          vaultManagerLpAta: managerLpAta,
          vaultAdminLpAta: ADMIN_LP_ATA,
          protocolTreasuryLpAta: treasuryLpAta,
          lpTokenProgram: TOKEN_PROGRAM,
        }, { programAddress: VOLTR }),
      ];
      if ((instructions[2]?.accounts ?? []).length !== 12) throw new Error("harvestFee account list drifted from the 12-account wire");
      const admin = await signingMaterialFromEnvironment("SOLANA_TESTING_PK");
      if (admin.signer.address !== ADMIN) throw new Error("SOLANA_TESTING_PK is not the HXtk admin signer");
      const inspectedAddresses = [VAULT, LP_MINT, ADMIN_LP_ATA, managerLpAta, treasuryLpAta];
      const prepared = await prepareSignedV0Transaction({
        rpcUrl,
        feePayer: admin,
        instructions,
        inspectedAddresses,
        prestateAddresses: inspectedAddresses,
        minimumContextSlot: state.contextSlot,
        commitment: "finalized",
      });
      const postVault = decodeVault(rawFromPreparedSnapshot(VAULT, prepared.simulation.postAccounts[0]));
      const postAdminLp = tokenAmount(rawFromPreparedSnapshot(ADMIN_LP_ATA, prepared.simulation.postAccounts[2]));
      const postSupply = mintSupply(rawFromPreparedSnapshot(LP_MINT, prepared.simulation.postAccounts[1]));
      const postAdminLpDelta = postAdminLp === null ? null : postAdminLp - (state.adminLpBalance ?? 0n);
      if (!postVault || postVault.accumulatedLpAdminFees !== 0n || postVault.accumulatedLpManagerFees !== 0n
        || postVault.accumulatedLpProtocolFees !== 0n
        || postAdminLpDelta !== adminFeeAccrued
        || postSupply !== (state.lpSupply ?? 0n) + adminFeeAccrued) {
        throw new Error("signed harvest simulation did not project the accrued fee transfer exactly");
      }
      return {
        prepared,
        plan: {
          before: summarize(state),
          expectedPreState: summarize(state),
          expectedPostState: {
            adminLpBalance: postAdminLp?.toString() ?? null,
            adminLpDelta: adminFeeAccrued.toString(),
            lpSupply: postSupply?.toString() ?? null,
            feeAccumulators: "0",
          },
          harvest: { adminFeeAccrued: adminFeeAccrued.toString() },
          transaction: {
            feePayer: ADMIN,
            signer: ADMIN,
            signerEnvVar: "SOLANA_TESTING_PK",
            instructionCount: instructions.length,
            instructions: [
              "createAssociatedTokenAccountIdempotent(manager LP ATA)",
              "createAssociatedTokenAccountIdempotent(treasury LP ATA)",
              "harvestFee",
            ],
          },
        },
      };
    },
    reconcile: async ({ pending }) => {
      const state = await readState("finalized");
      if (!state.vault) throw new Error("finalized harvest omitted the vault");
      const before = recordAt(pending.before, "before");
      const harvest = recordAt(pending.harvest, "harvest");
      const accrued = BigInt(stringAt(harvest.adminFeeAccrued, "harvest.adminFeeAccrued"));
      const beforeAdmin = BigInt(String(before.adminLpBalance ?? "0"));
      const beforeSupply = BigInt(stringAt(before.lpSupply, "before.lpSupply"));
      const adminLpDelta = (state.adminLpBalance ?? 0n) - beforeAdmin;
      if (adminLpDelta !== accrued || state.lpSupply !== beforeSupply + accrued
        || state.vault.accumulatedLpAdminFees !== 0n || state.vault.accumulatedLpManagerFees !== 0n
        || state.vault.accumulatedLpProtocolFees !== 0n) {
        throw new Error("finalized harvest did not reconcile the fee delta and cleared accumulators");
      }
      return { finalizedAdminLpDelta: adminLpDelta.toString(), finalizedState: summarize(state) };
    },
  });
}

function buildCancelInstruction(noopUser: ReturnType<typeof createNoopSigner>) {
  return getCancelRequestWithdrawVaultInstructionAsync({
    userTransferAuthority: noopUser,
    protocol: PROTOCOL,
    vault: VAULT,
    vaultLpMint: LP_MINT,
    userLpAta: ADMIN_LP_ATA,
    requestWithdrawLpAta: PENDING_ESCROW,
    requestWithdrawVaultReceipt: REQUEST_RECEIPT,
    lpTokenProgram: TOKEN_PROGRAM,
    systemProgram: SYS_PROGRAM,
  }, { programAddress: VOLTR });
}

/** Receipt stays live after cancel/request; decode the tracked LP straight from bytes. */
function requestReceiptLp(account: RawAccount): bigint | null {
  return account ? u64Le(account.data, 72) : null;
}

function requestReceiptWithdrawableFromTs(account: RawAccount): bigint | null {
  return account ? u64Le(account.data, 96) : null;
}

async function cmdHarvest(): Promise<number> {
  const mode = operatorMode();
  if (mode) return cmdHarvestOperator(mode);
  await assertRepairPolicyRetired("harvest");
  const state = await readState();
  if (!state.vault) throw new Error("vault account is absent");
  const before = summarize(state);
  const { managerLpAta, treasuryLpAta } = state.identity;
  const noopAdmin = createNoopSigner(ADMIN);
  const adminFeeAccrued = state.vault.accumulatedLpAdminFees;
  const instructions = [
    await createAtaIdempotent(ADMIN, managerLpAta, SQUADS_VAULT, LP_MINT),
    await createAtaIdempotent(ADMIN, treasuryLpAta, PROTOCOL_TREASURY, LP_MINT),
    await getHarvestFeeInstructionAsync({
      harvester: noopAdmin,
      vaultManager: SQUADS_VAULT,
      vaultAdmin: ADMIN,
      protocolTreasury: PROTOCOL_TREASURY,
      protocol: PROTOCOL,
      vault: VAULT,
      vaultLpMint: LP_MINT,
      vaultLpMintAuth: LP_MINT_AUTH,
      vaultManagerLpAta: managerLpAta,
      vaultAdminLpAta: ADMIN_LP_ATA,
      protocolTreasuryLpAta: treasuryLpAta,
      lpTokenProgram: TOKEN_PROGRAM,
    }, { programAddress: VOLTR }),
  ];
  if ((instructions[2]?.accounts ?? []).length !== 12) {
    throw new Error("harvestFee account list drifted from the 12-account wire (harvester/manager/admin/treasury/protocol/vault/mint/mintAuth/3 atas/token)");
  }

  const simulation = await simulate(ADMIN, instructions, [
    VAULT, LP_MINT, ADMIN_LP_ATA, managerLpAta, treasuryLpAta,
  ]);
  const events = simulation.err === null ? decodeEvents("HarvestFee", simulation.logs) : [];
  const postVault = simulation.err === null ? decodeVault(postAccount(simulation.postAccounts, VAULT)) : null;
  const supplyAfter = mintSupply(postAccount(simulation.postAccounts, LP_MINT));
  const adminLpBefore = state.adminLpBalance ?? 0n;
  const adminLpAfter = postToken(simulation.postAccounts, ADMIN_LP_ATA);
  const adminLpDelta = adminLpAfter === null ? null : adminLpAfter - adminLpBefore;
  const checks = [
    checkRow("simulation succeeds", simulation.err === null, null,
      simulation.err === null ? null : JSON.stringify(simulation.err)),
    checkRow("harvest event emitted", events.length > 0, ">=1", events.length),
    checkRow("admin LP ATA delta equals the accrued admin fee",
      adminLpDelta === adminFeeAccrued,
      adminFeeAccrued.toString(), adminLpDelta?.toString() ?? null),
    checkRow("manager LP ATA credited 0 (accumulator empty)",
      postToken(simulation.postAccounts, managerLpAta) === 0n, "0",
      postToken(simulation.postAccounts, managerLpAta)?.toString() ?? null),
    checkRow("treasury LP ATA credited 0 (accumulator empty)",
      postToken(simulation.postAccounts, treasuryLpAta) === 0n, "0",
      postToken(simulation.postAccounts, treasuryLpAta)?.toString() ?? null),
    checkRow("lp supply grows by exactly the admin fee",
      supplyAfter === (state.lpSupply ?? 0n) + adminFeeAccrued,
      ((state.lpSupply ?? 0n) + adminFeeAccrued).toString(),
      supplyAfter?.toString() ?? null),
    checkRow("fee accumulators reset to 0",
      postVault?.accumulatedLpAdminFees === 0n && postVault?.accumulatedLpManagerFees === 0n,
      "0/0",
      postVault ? `${postVault.accumulatedLpAdminFees}/${postVault.accumulatedLpManagerFees}` : null),
    checkRow("books untouched (tv)",
      postVault?.totalValue === state.vault.totalValue,
      state.vault.totalValue.toString(), postVault?.totalValue.toString() ?? null),
  ];
  const pass = checks.every((row) => row.pass);
  console.log(toJson({
    step: "harvest", sent: false, verdict: pass ? "SIMULATION_PASS_UNSENT" : "SIMULATION_FAILED",
    adminLpDelta: adminLpDelta?.toString() ?? null,
    before, transaction: {
      packetBytes: simulation.packetBytes, unitsConsumed: simulation.unitsConsumed,
      instructions: [
        "createAssociatedTokenAccountIdempotent(manager ST999… LP)",
        "createAssociatedTokenAccountIdempotent(treasury C7sE… LP)",
        "harvestFee",
      ],
      signer: ADMIN, signerEnvVar: "SOLANA_TESTING_PK",
      executeCommand: "op run --env-file=.env.1password -- env CONFIRM_MAINNET=1 bun run reset:hxtk harvest --execute --journal /absolute/path/hxtk-harvest.json",
    }, checks, events,
  }, 2));
  writeEvidence("harvest", {
    verdict: pass ? "SIMULATION_PASS_UNSENT" : "SIMULATION_FAILED",
    adminLpDelta: adminLpDelta?.toString() ?? null,
    adminFeeAccrued: adminFeeAccrued.toString(),
    before, checks, events,
  });
  return pass ? 0 : 1;
}

async function cmdCancelOperator(mode: RepairPolicyOperatorMode): Promise<number> {
  const journal = operatorJournal();
  const rpcUrl = operatorRpcUrl();
  return runJournaledStep({
    mode,
    step: "cancel",
    schema: SCHEMA,
    journal,
    rpcUrl,
    build: async () => {
      await assertRepairPolicyRetired("cancel");
      const state = await readState("finalized");
      assertOriginalFrozenCancelReceipt(state, "cancel");
      assertHxtkCancelProofPreState(cancelProofPreStateFromLiveState(state), "cancel pre-sign");
      if (!state.requestReceipt) throw new Error("no pending withdraw request receipt on chain");
      if (state.requestReceipt.userTransferAuthority !== ADMIN) throw new Error("pending request authority is not the HXtk admin");
      if (state.requestReceipt.amountLpEscrowed !== (state.requestEscrowLpBalance ?? 0n)) {
        throw new Error("escrow balance disagrees with the receipt's amountLpEscrowed");
      }
      const noopAdmin = createNoopSigner(ADMIN);
      const cancel = await buildCancelInstruction(noopAdmin);
      if ((cancel.accounts ?? []).length !== 9) throw new Error("cancelRequestWithdrawVault account list drifted from the 9-account wire");
      const admin = await signingMaterialFromEnvironment("SOLANA_TESTING_PK");
      if (admin.signer.address !== ADMIN) throw new Error("SOLANA_TESTING_PK is not the HXtk admin signer");
      const inspectedAddresses = [VAULT, IDLE_ATA, LP_MINT, ADMIN_LP_ATA, PENDING_ESCROW, REQUEST_RECEIPT];
      const prepared = await prepareSignedV0Transaction({
        rpcUrl,
        feePayer: admin,
        instructions: [cancel],
        inspectedAddresses,
        prestateAddresses: inspectedAddresses,
        minimumContextSlot: state.contextSlot,
        commitment: "finalized",
      });
      const postVault = decodeVault(rawFromPreparedSnapshot(VAULT, prepared.simulation.postAccounts[0]));
      const postIdle = tokenAmount(rawFromPreparedSnapshot(IDLE_ATA, prepared.simulation.postAccounts[1]));
      const postSupply = mintSupply(rawFromPreparedSnapshot(LP_MINT, prepared.simulation.postAccounts[2]));
      const postAdminLp = tokenAmount(rawFromPreparedSnapshot(ADMIN_LP_ATA, prepared.simulation.postAccounts[3]));
      const postEscrow = tokenAmount(rawFromPreparedSnapshot(PENDING_ESCROW, prepared.simulation.postAccounts[4]));
      const postReceipt = rawFromPreparedSnapshot(REQUEST_RECEIPT, prepared.simulation.postAccounts[5]);
      const events = prepared.simulation.err === null
        ? decodeEvents("CancelRequestWithdrawVault", prepared.simulation.logs)
        : [];
      const event = events.length === 1
        ? events[0] as { amountLpRefunded?: bigint; amountLpBurned?: bigint }
        : null;
      assertHxtkCancelSimulation({
        preState: cancelProofPreStateFromLiveState(state),
        simulationSucceeded: prepared.simulation.err === null,
        cancelEventCount: events.length,
        eventRefundLp: event?.amountLpRefunded ?? null,
        eventBurnLp: event?.amountLpBurned ?? null,
        escrowAfter: postEscrow,
        requestReceiptLpAfter: postReceipt === null ? null : requestReceiptLp(postReceipt),
        adminLpDelta: postAdminLp === null ? null : postAdminLp - (state.adminLpBalance ?? 0n),
        adminLpAfter: postAdminLp,
        supplyAfter: postSupply,
        totalValueAfter: postVault?.totalValue ?? null,
        idleBalanceAfter: postIdle,
      }, "signed cancel simulation");
      return {
        prepared,
        plan: {
          before: summarize(state),
          expectedPreState: summarize(state),
          expectedPostState: {
            escrowLpBalance: "0",
            expectedRefundLp: CANCEL_EXPECTED_REFUND_LP.toString(),
            expectedBurnLp: CANCEL_EXPECTED_BURN_LP.toString(),
            expectedSupplyAfter: CANCEL_EXPECTED_SUPPLY_AFTER.toString(),
            adminLpDelta: CANCEL_EXPECTED_REFUND_LP.toString(),
            adminLpBalance: CANCEL_EXPECTED_SUPPLY_AFTER.toString(),
            requestReceipt: "closed|0",
          },
          requestReceiptPda: REQUEST_RECEIPT,
          originalFrozenReceipt: true,
          cancel: {
            expectedRefundLp: CANCEL_EXPECTED_REFUND_LP.toString(),
            expectedBurnLp: CANCEL_EXPECTED_BURN_LP.toString(),
            expectedSupplyAfter: CANCEL_EXPECTED_SUPPLY_AFTER.toString(),
            requestReceipt: REQUEST_RECEIPT,
            originalFrozenReceipt: true,
          },
          transaction: {
            feePayer: ADMIN,
            signer: ADMIN,
            signerEnvVar: "SOLANA_TESTING_PK",
            instructionCount: 1,
            instructions: ["cancelRequestWithdrawVault"],
          },
        },
      };
    },
    reconcile: async ({ pending, finalized }) => {
      const state = await readState("finalized");
      const before = recordAt(pending.before, "before");
      const cancel = recordAt(pending.cancel, "cancel");
      const expectedRefundLp = BigInt(stringAt(
        cancel.expectedRefundLp ?? cancel.escrowRefundLp,
        "cancel.expectedRefundLp",
      ));
      const expectedBurnLp = BigInt(stringAt(cancel.expectedBurnLp, "cancel.expectedBurnLp"));
      const expectedSupplyAfter = BigInt(stringAt(cancel.expectedSupplyAfter, "cancel.expectedSupplyAfter"));
      const beforeAdmin = BigInt(String(before.adminLpBalance ?? "0"));
      const adminLpDelta = (state.adminLpBalance ?? 0n) - beforeAdmin;
      if (expectedRefundLp !== CANCEL_EXPECTED_REFUND_LP
        || expectedBurnLp !== CANCEL_EXPECTED_BURN_LP
        || expectedSupplyAfter !== CANCEL_EXPECTED_SUPPLY_AFTER) {
        throw new Error("RECONCILE_MISMATCH: finalized cancel journal is not pinned to the reset proof numbers");
      }
      const events = finalized.meta?.logMessages
        ? decodeEvents("CancelRequestWithdrawVault", finalized.meta.logMessages)
        : [];
      const event = events.length === 1
        ? events[0] as { amountLpRefunded?: bigint; amountLpBurned?: bigint }
        : null;
      const post = postCancelStateFromLiveState(state);
      assertHxtkCancelSimulation({
        preState: cancelProofPreStateFromRecord(before),
        simulationSucceeded: true,
        cancelEventCount: events.length,
        eventRefundLp: event?.amountLpRefunded ?? null,
        eventBurnLp: event?.amountLpBurned ?? null,
        escrowAfter: post.requestEscrowLpBalance,
        requestReceiptLpAfter: post.requestReceiptLp,
        adminLpDelta,
        adminLpAfter: post.adminLpBalance,
        supplyAfter: post.lpSupply,
        totalValueAfter: post.totalValue,
        idleBalanceAfter: post.idleBalance,
      }, "finalized cancel reconcile");
      return { finalizedAdminLpDelta: adminLpDelta.toString(), finalizedState: summarize(state) };
    },
  });
}

async function cmdCancel(): Promise<number> {
  const mode = operatorMode();
  if (mode) return cmdCancelOperator(mode);
  await assertRepairPolicyRetired("cancel");
  const state = await readState();
  assertOriginalFrozenCancelReceipt(state, "cancel");
  const preState = cancelProofPreStateFromLiveState(state);
  assertHxtkCancelProofPreState(preState, "cancel pre-simulate");
  if (!state.requestReceipt) throw new Error("no pending withdraw request receipt on chain");
  if (state.requestReceipt.userTransferAuthority !== ADMIN) {
    throw new Error(`request authority ${state.requestReceipt.userTransferAuthority} is not ${ADMIN}`);
  }
  if (state.requestReceipt.amountLpEscrowed !== (state.requestEscrowLpBalance ?? 0n)) {
    throw new Error("escrow balance disagrees with the receipt's amountLpEscrowed");
  }
  const before = summarize(state);
  const noopAdmin = createNoopSigner(ADMIN);
  const cancel = await buildCancelInstruction(noopAdmin);
  if ((cancel.accounts ?? []).length !== 9) {
    throw new Error("cancelRequestWithdrawVault account list drifted from the 9-account wire");
  }
  const simulation = await simulate(ADMIN, [cancel], [
    VAULT, IDLE_ATA, LP_MINT, ADMIN_LP_ATA, PENDING_ESCROW, REQUEST_RECEIPT,
  ]);
  const events = simulation.err === null ? decodeEvents("CancelRequestWithdrawVault", simulation.logs) : [];
  const event = events.length === 1
    ? events[0] as { amountLpRefunded?: bigint; amountLpBurned?: bigint }
    : null;
  const postVault = simulation.err === null ? decodeVault(postAccount(simulation.postAccounts, VAULT)) : null;
  const supplyAfter = mintSupply(postAccount(simulation.postAccounts, LP_MINT));
  const adminLpBefore = state.adminLpBalance ?? 0n;
  const adminLpAfter = postToken(simulation.postAccounts, ADMIN_LP_ATA);
  const refund = adminLpAfter === null ? null : adminLpAfter - adminLpBefore;
  const escrowAfter = postToken(simulation.postAccounts, PENDING_ESCROW);
  const checks = hxtkCancelSimulationChecks({
    preState,
    simulationSucceeded: simulation.err === null,
    cancelEventCount: events.length,
    eventRefundLp: event?.amountLpRefunded ?? null,
    eventBurnLp: event?.amountLpBurned ?? null,
    escrowAfter,
    requestReceiptLpAfter: postAccount(simulation.postAccounts, REQUEST_RECEIPT) === null
      ? null : requestReceiptLp(postAccount(simulation.postAccounts, REQUEST_RECEIPT)),
    adminLpDelta: refund,
    adminLpAfter,
    supplyAfter,
    totalValueAfter: postVault?.totalValue ?? null,
    idleBalanceAfter: postToken(simulation.postAccounts, IDLE_ATA),
  });
  const pass = checks.every((row) => row.pass);
  console.log(toJson({
    step: "cancel", sent: false, verdict: pass ? "SIMULATION_PASS_UNSENT" : "SIMULATION_FAILED",
    adminLpDelta: refund?.toString() ?? null,
    requestReceiptPda: REQUEST_RECEIPT,
    originalFrozenReceipt: true,
    before, transaction: {
      packetBytes: simulation.packetBytes, unitsConsumed: simulation.unitsConsumed,
      instructions: ["cancelRequestWithdrawVault"], signer: ADMIN, signerEnvVar: "SOLANA_TESTING_PK",
      executeCommand: "op run --env-file=.env.1password -- env CONFIRM_MAINNET=1 bun run reset:hxtk cancel --execute --journal /absolute/path/hxtk-cancel.json",
    }, checks, events,
  }, 2));
  writeEvidence("cancel", {
    verdict: pass ? "SIMULATION_PASS_UNSENT" : "SIMULATION_FAILED",
    adminLpDelta: refund?.toString() ?? null,
    expectedRefundLp: CANCEL_EXPECTED_REFUND_LP.toString(),
    expectedBurnLp: CANCEL_EXPECTED_BURN_LP.toString(),
    expectedSupplyAfter: CANCEL_EXPECTED_SUPPLY_AFTER.toString(),
    requestReceiptPda: REQUEST_RECEIPT,
    originalFrozenReceipt: true,
    before, checks, events,
  });
  return pass ? 0 : 1;
}

type RequestAfterCancel = Readonly<{
  cancelJournal: FinalizedCancelJournal;
  state: LiveState;
  amountLp: bigint;
}>;

async function readRequestAfterFinalizedCancel(rpcUrl: string): Promise<RequestAfterCancel> {
  await assertRepairPolicyRetired("request");
  const cancelPath = requiredJournalPath(
    CANCEL_JOURNAL_FLAG,
    "request must follow a finalized cancel journal",
  );
  const cancelJournal = await verifyFinalizedCancelJournal(rpcUrl, cancelPath);
  const state = await readState("finalized", true);
  assertOriginalFrozenReceipt(state, "request");
  assertHxtkPostCancelState(postCancelStateFromLiveState(state), "request after finalized cancel");
  const amountLp = state.adminLpBalance ?? 0n;
  if (amountLp !== REQUEST_EXPECTED_LP) {
    throw new Error(
      `HXTK_RESET_POST_CANCEL_DRIFT: request amount expected ${REQUEST_EXPECTED_LP} post-cancel admin LP, actual ${amountLp}`,
    );
  }
  return { cancelJournal, state, amountLp };
}

async function cmdRequestOperator(mode: RepairPolicyOperatorMode): Promise<number> {
  const journal = operatorJournal();
  const rpcUrl = operatorRpcUrl();
  return runJournaledStep({
    mode,
    step: "request",
    schema: SCHEMA,
    journal,
    rpcUrl,
    build: async () => {
      const afterCancel = await readRequestAfterFinalizedCancel(rpcUrl);
      const { state, amountLp } = afterCancel;
      const noopAdmin = createNoopSigner(ADMIN);
      const request = await getRequestWithdrawVaultInstructionAsync({
        payer: noopAdmin,
        userTransferAuthority: noopAdmin,
        protocol: PROTOCOL,
        vault: VAULT,
        vaultLpMint: LP_MINT,
        userLpAta: ADMIN_LP_ATA,
        requestWithdrawLpAta: PENDING_ESCROW,
        requestWithdrawVaultReceipt: REQUEST_RECEIPT,
        amount: amountLp,
        isAmountInLp: true,
        isWithdrawAll: true,
        lpTokenProgram: TOKEN_PROGRAM,
        systemProgram: SYS_PROGRAM,
      }, { programAddress: VOLTR });
      const admin = await signingMaterialFromEnvironment("SOLANA_TESTING_PK");
      if (admin.signer.address !== ADMIN) throw new Error("SOLANA_TESTING_PK is not the HXtk admin signer");
      const instructions = [request];
      const inspectedAddresses = [VAULT, LP_MINT, ADMIN_LP_ATA, PENDING_ESCROW, REQUEST_RECEIPT];
      const prepared = await prepareSignedV0Transaction({
        rpcUrl,
        feePayer: admin,
        instructions,
        inspectedAddresses,
        prestateAddresses: inspectedAddresses,
        minimumContextSlot: state.contextSlot,
        commitment: "finalized",
      });
      const postReceipt = rawFromPreparedSnapshot(REQUEST_RECEIPT, prepared.simulation.postAccounts[4]);
      const postEscrow = tokenAmount(rawFromPreparedSnapshot(PENDING_ESCROW, prepared.simulation.postAccounts[3]));
      const postAdminLp = tokenAmount(rawFromPreparedSnapshot(ADMIN_LP_ATA, prepared.simulation.postAccounts[2]));
      const postSupply = mintSupply(rawFromPreparedSnapshot(LP_MINT, prepared.simulation.postAccounts[1]));
      const postWithdrawable = requestReceiptWithdrawableFromTs(postReceipt);
      const events = prepared.simulation.err === null
        ? decodeEvents("RequestWithdrawVault", prepared.simulation.logs)
        : [];
      const event = events.length === 1
        ? events[0] as {
            requestedAmount?: bigint;
            isAmountInLp?: boolean;
            isWithdrawAll?: boolean;
            requestWithdrawVaultReceipt?: Address;
          }
        : null;
      assertHxtkRequestSimulation({
        preState: postCancelStateFromLiveState(state),
        simulationSucceeded: prepared.simulation.err === null,
        requestEventCount: events.length,
        eventRequestedAmount: event?.requestedAmount ?? null,
        eventIsAmountInLp: event?.isAmountInLp ?? null,
        eventIsWithdrawAll: event?.isWithdrawAll ?? null,
        eventReceipt: event?.requestWithdrawVaultReceipt?.toString() ?? null,
        escrowAfter: postEscrow,
        adminLpAfter: postAdminLp,
        supplyAfter: postSupply,
      }, "signed request simulation");
      if (postReceipt === null || postWithdrawable === null
        || postWithdrawable < BigInt(Math.floor(Date.now() / 1000)) + REQUEST_WAITING_PERIOD_SECONDS) {
        throw new Error("signed request-only simulation did not project the post-cancel request and waiting period");
      }
      return {
        prepared,
        plan: {
          cancelJournal: afterCancel.cancelJournal.path,
          before: summarize(state),
          expectedPreState: summarize(state),
          expectedPostState: {
            escrowLpBalance: amountLp.toString(),
            adminLpBalance: "0",
            waitingPeriodSeconds: REQUEST_WAITING_PERIOD_SECONDS.toString(),
            expectedClaimPayoutRaw: CLAIM_EXPECTED_PAYOUT_RAW.toString(),
            expectedResidualTv: CLAIM_EXPECTED_RESIDUAL_RAW.toString(),
            expectedResidualIdle: CLAIM_EXPECTED_RESIDUAL_RAW.toString(),
          },
          requestAmountLp: amountLp.toString(),
          expectedClaimPayoutRaw: CLAIM_EXPECTED_PAYOUT_RAW.toString(),
          expectedResidualTv: CLAIM_EXPECTED_RESIDUAL_RAW.toString(),
          expectedResidualIdle: CLAIM_EXPECTED_RESIDUAL_RAW.toString(),
          isAmountInLp: true,
          isWithdrawAll: true,
          requestReceipt: REQUEST_RECEIPT,
          requestReceiptPda: REQUEST_RECEIPT,
          simulatedWithCancelPrefix: false,
          transaction: {
            feePayer: ADMIN,
            signer: ADMIN,
            signerEnvVar: "SOLANA_TESTING_PK",
            instructionCount: 1,
            instructions: ["requestWithdrawVault(all post-cancel admin LP, isAmountInLp, isWithdrawAll)"],
          },
        },
      };
    },
    reconcile: async ({ pending, finalized }) => {
      const cancelJournal = await verifyFinalizedCancelJournal(
        rpcUrl,
        resolve(stringAt(pending.cancelJournal, "request journal cancelJournal")),
      );
      const state = await readState("finalized", true);
      const request = state.requestReceipt;
      const expectedPostState = recordAt(pending.expectedPostState, "expectedPostState");
      const expectedAmount = BigInt(stringAt(expectedPostState.escrowLpBalance, "expectedPostState.escrowLpBalance"));
      if (finalized.slot <= cancelJournal.finalized.slot) {
        throw new Error("finalized request transaction predates or equals the finalized cancel transaction");
      }
      if (!request || request.amountLpEscrowed === 0n || state.adminLpBalance !== 0n
        || (state.requestEscrowLpBalance ?? 0n) !== expectedAmount
        || expectedAmount !== REQUEST_EXPECTED_LP
        || cancelJournal.adminLpDelta !== cancelJournal.expectedRefundLp) {
        throw new Error("finalized request did not reconcile the new escrowed request");
      }
      const requestBlockTime = finalized.blockTime;
      if (requestBlockTime === null || requestBlockTime === undefined) {
        throw new Error("finalized request transaction has no blockTime; cannot reconcile the chain waiting period");
      }
      const chainExpectedWithdrawableFromTs = BigInt(requestBlockTime) + REQUEST_WAITING_PERIOD_SECONDS;
      const waitingPeriodDelta = request.withdrawableFromTs - chainExpectedWithdrawableFromTs;
      if (waitingPeriodDelta < -1n || waitingPeriodDelta > 1n) {
        throw new Error(
          `finalized request withdrawableFromTs ${request.withdrawableFromTs} does not equal request blockTime ${requestBlockTime} + `
          + `${REQUEST_WAITING_PERIOD_SECONDS} (${chainExpectedWithdrawableFromTs}); observed delta ${waitingPeriodDelta}`,
        );
      }
      return {
        cancelJournal: cancelJournal.path,
        requestReceipt: request,
        claimableAt: request.withdrawableFromTs.toString(),
        requestBlockTime,
        chainExpectedWithdrawableFromTs: chainExpectedWithdrawableFromTs.toString(),
        waitingPeriodDelta: waitingPeriodDelta.toString(),
        finalizedState: summarize(state),
      };
    },
  });
}

async function cmdRequest(): Promise<number> {
  const mode = operatorMode();
  if (mode) return cmdRequestOperator(mode);
  await assertRepairPolicyRetired("request");
  if (!currentCli().has(CANCEL_JOURNAL_FLAG)) {
    const output = {
      schema: SCHEMA,
      step: "request",
      sent: false,
      signed: false,
      broadcast: false,
      verdict: "PENDING_FINALIZED_CANCEL",
      reason: `${CANCEL_JOURNAL_FLAG} is required; request is request-only after finalized cancel`,
    };
    console.log(toJson(output, 2));
    writeEvidence("request", output);
    return 2;
  }
  const afterCancel = await readRequestAfterFinalizedCancel(
    process.env.SOLANA_RPC_URL?.trim() || DEFAULT_RPC_URL,
  );
  const { state, amountLp } = afterCancel;
  const before = summarize(state);
  const noopAdmin = createNoopSigner(ADMIN);
  const request = await getRequestWithdrawVaultInstructionAsync({
    payer: noopAdmin,
    userTransferAuthority: noopAdmin,
    protocol: PROTOCOL,
    vault: VAULT,
    vaultLpMint: LP_MINT,
    userLpAta: ADMIN_LP_ATA,
    requestWithdrawLpAta: PENDING_ESCROW,
    requestWithdrawVaultReceipt: REQUEST_RECEIPT,
    amount: amountLp,
    isAmountInLp: true,
    isWithdrawAll: true,
    lpTokenProgram: TOKEN_PROGRAM,
    systemProgram: SYS_PROGRAM,
  }, { programAddress: VOLTR });
  const simulation = await simulate(ADMIN, [request], [
    VAULT, LP_MINT, ADMIN_LP_ATA, PENDING_ESCROW, REQUEST_RECEIPT,
  ]);
  const events = simulation.err === null ? decodeEvents("RequestWithdrawVault", simulation.logs) : [];
  const event = events.length === 1
    ? events[0] as {
        requestedAmount?: bigint;
        isAmountInLp?: boolean;
        isWithdrawAll?: boolean;
        requestWithdrawVaultReceipt?: Address;
      }
    : null;
  const postReceipt = postAccount(simulation.postAccounts, REQUEST_RECEIPT);
  const nowSeconds = BigInt(Math.floor(Date.now() / 1000));
  const withdrawableFromTs = requestReceiptWithdrawableFromTs(postReceipt);
  const postBits = postReceipt ? u128Le(postReceipt.data, 80) : null;
  const escrowAfter = postToken(simulation.postAccounts, PENDING_ESCROW);
  const checks = [
    checkRow("simulation succeeds", simulation.err === null, null,
      simulation.err === null ? null : JSON.stringify(simulation.err)),
    checkRow("exactly one request event emitted", events.length === 1, 1, events.length),
    checkRow("request event amount equals all post-cancel admin LP",
      event?.requestedAmount === REQUEST_EXPECTED_LP,
      REQUEST_EXPECTED_LP.toString(), event?.requestedAmount?.toString() ?? null),
    checkRow("request event uses LP amount", event?.isAmountInLp === true, true, event?.isAmountInLp ?? null),
    checkRow("request event uses isWithdrawAll", event?.isWithdrawAll === true, true, event?.isWithdrawAll ?? null),
    checkRow("request event receipt is HXtk's receipt",
      event?.requestWithdrawVaultReceipt?.toString() === REQUEST_RECEIPT,
      REQUEST_RECEIPT, event?.requestWithdrawVaultReceipt?.toString() ?? null),
    checkRow("escrow re-funded with the entire admin LP balance (plan: request all)",
      escrowAfter === amountLp,
      amountLp.toString(), escrowAfter?.toString() ?? null),
    checkRow("admin LP ATA emptied", postToken(simulation.postAccounts, ADMIN_LP_ATA) === 0n, "0",
      postToken(simulation.postAccounts, ADMIN_LP_ATA)?.toString() ?? null),
    checkRow("supply unchanged by request",
      mintSupply(postAccount(simulation.postAccounts, LP_MINT)) === (state.lpSupply ?? 0n),
      (state.lpSupply ?? 0n).toString(),
      mintSupply(postAccount(simulation.postAccounts, LP_MINT))?.toString() ?? null),
    checkRow(`withdrawableFromTs >= now + ${REQUEST_WAITING_PERIOD_SECONDS}s waiting period`,
      withdrawableFromTs !== null && withdrawableFromTs >= nowSeconds + REQUEST_WAITING_PERIOD_SECONDS,
      `>= ${nowSeconds + REQUEST_WAITING_PERIOD_SECONDS} (now + 600)`, withdrawableFromTs?.toString() ?? null),
    checkRow("amountAssetToWithdraw reported (informational)", true, "n/a",
      postBits === null ? null : (postBits >> 48n).toString()),
  ];
  const pass = checks.slice(0, -1).every((row) => row.pass);
  console.log(toJson({
    step: "request", sent: false, verdict: pass ? "SIMULATION_PASS_UNSENT" : "SIMULATION_FAILED",
    cancelJournal: afterCancel.cancelJournal.path,
    simulatedWithCancelPrefix: false, before, transaction: {
      packetBytes: simulation.packetBytes, unitsConsumed: simulation.unitsConsumed,
      instructions: ["requestWithdrawVault(all post-cancel admin LP, isAmountInLp, isWithdrawAll)"],
      signer: ADMIN, signerEnvVar: "SOLANA_TESTING_PK",
      executeCommand: "op run --env-file=.env.1password -- env CONFIRM_MAINNET=1 bun run reset:hxtk request --cancel-journal /absolute/path/hxtk-cancel.json --execute --journal /absolute/path/hxtk-request.json",
    }, checks, events,
    expectedClaimPayoutRaw: CLAIM_EXPECTED_PAYOUT_RAW.toString(),
    expectedResidualTv: CLAIM_EXPECTED_RESIDUAL_RAW.toString(),
    expectedResidualIdle: CLAIM_EXPECTED_RESIDUAL_RAW.toString(),
  }, 2));
  writeEvidence("request", {
    verdict: pass ? "SIMULATION_PASS_UNSENT" : "SIMULATION_FAILED",
    cancelJournal: afterCancel.cancelJournal.path,
    simulatedWithCancelPrefix: false, before, checks, events,
    requestAmountLp: REQUEST_EXPECTED_LP.toString(),
    expectedClaimPayoutRaw: CLAIM_EXPECTED_PAYOUT_RAW.toString(),
    expectedResidualTv: CLAIM_EXPECTED_RESIDUAL_RAW.toString(),
    expectedResidualIdle: CLAIM_EXPECTED_RESIDUAL_RAW.toString(),
  });
  return pass ? 0 : 1;
}

type ClaimBeforeObservation = Readonly<{
  adminUsdcBalance: bigint | null;
  idleBalance: bigint | null;
  totalValue: bigint | null;
  lpSupply: bigint | null;
  receipt1PositionValue: bigint | null;
  reportTicket: ReturnType<typeof decodeReportTicket>;
  amountAssetToWithdrawRaw: bigint | null;
}>;

type ClaimPostObservation = Readonly<{
  adminUsdcBalance: bigint | null;
  idleBalance: bigint | null;
  totalValue: bigint | null;
  lpSupply: bigint | null;
  receipt1PositionValue: bigint | null;
  requestReceiptClosed: boolean;
  escrowLpBalance: bigint | null;
  reportTicket: ReturnType<typeof decodeReportTicket>;
}>;

function claimReconciliationFromObservations(
  before: ClaimBeforeObservation,
  post: ClaimPostObservation,
  requestAmountLp: bigint,
  expectedPayoutRaw?: bigint,
  expectedLpBurnRaw?: bigint,
) {
  const payout = before.adminUsdcBalance === null || post.adminUsdcBalance === null
    ? null
    : post.adminUsdcBalance - before.adminUsdcBalance;
  const idleDelta = before.idleBalance === null || post.idleBalance === null
    ? null
    : before.idleBalance - post.idleBalance;
  const tvAfter = post.totalValue;
  const lpBurned = before.lpSupply === null || post.lpSupply === null
    ? null
    : before.lpSupply - post.lpSupply;
  const ticketUnchanged = before.reportTicket !== null
    && post.reportTicket !== null
    && toJson(before.reportTicket) === toJson(post.reportTicket);
  const checks = [
    checkRow("payout balance delta is available", payout !== null, "non-null", payout?.toString() ?? null),
    checkRow("payout is positive", payout !== null && payout >= 1n, ">= 1", payout?.toString() ?? null),
    checkRow("payout equals the pre-send simulation", expectedPayoutRaw === undefined
      || (payout !== null && payout === expectedPayoutRaw),
    expectedPayoutRaw?.toString() ?? "bound after simulation", payout?.toString() ?? null),
    checkRow("payout <= the receipt's amountAssetToWithdrawRaw (redeemable-value cap)",
      payout !== null && before.amountAssetToWithdrawRaw !== null
        && payout <= before.amountAssetToWithdrawRaw,
      before.amountAssetToWithdrawRaw === null ? "receipt amountAssetToWithdrawRaw" : `<= ${before.amountAssetToWithdrawRaw}`,
      payout?.toString() ?? null),
    checkRow("idle delta equals payout", idleDelta !== null && payout !== null && idleDelta === payout,
      payout?.toString() ?? null, idleDelta?.toString() ?? null),
    checkRow("tv after equals tv before minus payout",
      tvAfter !== null && before.totalValue !== null && payout !== null && tvAfter === before.totalValue - payout,
      before.totalValue === null || payout === null ? "tv before - payout" : (before.totalValue - payout).toString(),
      tvAfter?.toString() ?? null),
    checkRow("LP burned equals the request amount", lpBurned !== null && lpBurned === requestAmountLp,
      requestAmountLp.toString(), lpBurned?.toString() ?? null),
    checkRow("LP burned equals the pre-send simulation", expectedLpBurnRaw === undefined
      || (lpBurned !== null && lpBurned === expectedLpBurnRaw),
    expectedLpBurnRaw?.toString() ?? "bound after simulation", lpBurned?.toString() ?? null),
    checkRow("LP supply after matches the burn", before.lpSupply !== null && lpBurned !== null
      && post.lpSupply === before.lpSupply - lpBurned,
    before.lpSupply === null || lpBurned === null ? "lp supply before - burn" : (before.lpSupply - lpBurned).toString(),
    post.lpSupply?.toString() ?? null),
    checkRow("request receipt is closed", post.requestReceiptClosed, true, post.requestReceiptClosed),
    checkRow("escrow drained", post.escrowLpBalance === 0n, "0", post.escrowLpBalance?.toString() ?? null),
    checkRow("report ticket is unchanged", ticketUnchanged, true, ticketUnchanged),
    checkRow("proof payout equals 3,793,394 raw", payout === CLAIM_EXPECTED_PAYOUT_RAW,
      CLAIM_EXPECTED_PAYOUT_RAW.toString(), payout?.toString() ?? null),
    checkRow("proof LP burn equals all post-cancel admin LP",
      lpBurned === REQUEST_EXPECTED_LP,
      REQUEST_EXPECTED_LP.toString(), lpBurned?.toString() ?? null),
    checkRow("proof residual tv == idle == 23",
      tvAfter === CLAIM_EXPECTED_RESIDUAL_RAW && post.idleBalance === CLAIM_EXPECTED_RESIDUAL_RAW,
      `${CLAIM_EXPECTED_RESIDUAL_RAW}/${CLAIM_EXPECTED_RESIDUAL_RAW}`,
      `${tvAfter?.toString() ?? "missing"}/${post.idleBalance?.toString() ?? "missing"}`),
    checkRow("strategy-one receipt remains 3,793,536",
      before.receipt1PositionValue === PHANTOM_NAV_RAW
        && post.receipt1PositionValue === PHANTOM_NAV_RAW,
      PHANTOM_NAV_RAW.toString(),
      `${before.receipt1PositionValue?.toString() ?? "missing"}/${post.receipt1PositionValue?.toString() ?? "missing"}`),
  ];
  return {
    payout,
    idleDelta,
    tvAfter,
    idleAfter: post.idleBalance,
    lpBurned,
    lpSupplyAfter: post.lpSupply,
    receipt1PositionValueAfter: post.receipt1PositionValue,
    requestReceiptClosed: post.requestReceiptClosed,
    escrowLpBalanceAfter: post.escrowLpBalance,
    ticketUnchanged,
    checks,
    postState: {
      payoutRaw: payout?.toString() ?? null,
      idleDeltaRaw: idleDelta?.toString() ?? null,
      tvAfter: tvAfter?.toString() ?? null,
      idleAfter: post.idleBalance?.toString() ?? null,
      lpBurnedRaw: lpBurned?.toString() ?? null,
      lpSupplyAfter: post.lpSupply?.toString() ?? null,
      receipt1PositionValueAfter: post.receipt1PositionValue?.toString() ?? null,
      requestReceiptClosed: post.requestReceiptClosed,
      escrowLpBalanceAfter: post.escrowLpBalance?.toString() ?? null,
      ticketUnchanged,
    },
  } as const;
}

async function cmdClaimOperator(mode: RepairPolicyOperatorMode): Promise<number> {
  const journal = operatorJournal();
  const rpcUrl = operatorRpcUrl();
  return runJournaledStep({
    mode,
    step: "claim",
    schema: SCHEMA,
    journal,
    rpcUrl,
    build: async () => {
      await assertRepairPolicyRetired("claim");
      const requestJournalPath = requiredJournalPath(
        REQUEST_JOURNAL_FLAG,
        "claim must follow a finalized request journal",
      );
      const requestJournal = await verifyFinalizedRequestJournal(rpcUrl, requestJournalPath);
      const state = await readState("finalized", true);
      if (!state.requestReceipt) throw new Error("no pending withdraw request receipt on chain");
      if (!state.vault) throw new Error("vault account is absent");
      if (state.requestReceipt.amountLpEscrowed !== requestJournal.amountLpEscrowed
        || state.requestReceipt.withdrawableFromTs !== requestJournal.withdrawableFromTs) {
        throw new Error("claim request receipt differs from the finalized request journal");
      }
      assertHxtkClaimProofPreState(claimProofPreStateFromLiveState(state), "claim pre-sign");
      const eligibility = await latestFinalizedChainTime(rpcUrl);
      if (state.requestReceipt.withdrawableFromTs > BigInt(eligibility.blockTime)) {
        throw new Error(
          `withdrawableFromTs ${state.requestReceipt.withdrawableFromTs} is after latest finalized chain time `
          + `${eligibility.blockTime} at slot ${eligibility.slot}`,
        );
      }
      const noopAdmin = createNoopSigner(ADMIN);
      const { adminUsdcAta } = state.identity;
      const withdraw = await getWithdrawVaultInstructionAsync({
        userTransferAuthority: noopAdmin,
        protocol: PROTOCOL,
        vault: VAULT,
        vaultAssetMint: USDC,
        vaultLpMint: LP_MINT,
        requestWithdrawLpAta: PENDING_ESCROW,
        vaultAssetIdleAta: IDLE_ATA,
        vaultAssetIdleAuth: IDLE_AUTH,
        userAssetAta: adminUsdcAta,
        requestWithdrawVaultReceipt: REQUEST_RECEIPT,
        assetTokenProgram: TOKEN_PROGRAM,
        lpTokenProgram: TOKEN_PROGRAM,
        systemProgram: SYS_PROGRAM,
      }, { programAddress: VOLTR });
      if ((withdraw.accounts ?? []).length !== 13) throw new Error("withdrawVault account list drifted from the 13-account wire");
      const admin = await signingMaterialFromEnvironment("SOLANA_TESTING_PK");
      if (admin.signer.address !== ADMIN) throw new Error("SOLANA_TESTING_PK is not the HXtk admin signer");
      const inspectedAddresses = [VAULT, IDLE_ATA, LP_MINT, PENDING_ESCROW, adminUsdcAta, REQUEST_RECEIPT, REPORT_TICKET, RECEIPT1];
      const prepared = await prepareSignedV0Transaction({
        rpcUrl,
        feePayer: admin,
        instructions: [withdraw],
        inspectedAddresses,
        prestateAddresses: inspectedAddresses,
        minimumContextSlot: state.contextSlot,
        commitment: "finalized",
      });
      const postUserAsset = tokenAmount(rawFromPreparedSnapshot(adminUsdcAta, prepared.simulation.postAccounts[4]));
      const postIdle = tokenAmount(rawFromPreparedSnapshot(IDLE_ATA, prepared.simulation.postAccounts[1]));
      const postSupply = mintSupply(rawFromPreparedSnapshot(LP_MINT, prepared.simulation.postAccounts[2]));
      const postVault = decodeVault(rawFromPreparedSnapshot(VAULT, prepared.simulation.postAccounts[0]));
      const postReceipt = rawFromPreparedSnapshot(REQUEST_RECEIPT, prepared.simulation.postAccounts[5]);
      const postEscrow = tokenAmount(rawFromPreparedSnapshot(PENDING_ESCROW, prepared.simulation.postAccounts[3]));
      const postTicket = decodeReportTicket(rawFromPreparedSnapshot(REPORT_TICKET, prepared.simulation.postAccounts[6]));
      const postReceipt1 = decodeStrategyReceipt(rawFromPreparedSnapshot(RECEIPT1, prepared.simulation.postAccounts[7]));
      const claim = claimReconciliationFromObservations(
        {
          adminUsdcBalance: state.adminUsdcBalance,
          idleBalance: state.idleBalance,
          totalValue: state.vault.totalValue,
          lpSupply: state.lpSupply,
          receipt1PositionValue: state.receipt1?.positionValue ?? null,
          reportTicket: state.reportTicket,
          amountAssetToWithdrawRaw: state.requestReceipt.amountAssetToWithdrawRaw,
        },
        {
          adminUsdcBalance: postUserAsset,
          idleBalance: postIdle,
          totalValue: postVault?.totalValue ?? null,
          lpSupply: postSupply,
          receipt1PositionValue: postReceipt1?.positionValue ?? null,
          requestReceiptClosed: postReceipt === null,
          escrowLpBalance: postEscrow,
          reportTicket: postTicket,
        },
        requestJournal.amountLpEscrowed,
      );
      assertHxtkClaimProof({
        preState: claimProofPreStateFromLiveState(state),
        payout: claim.payout,
        lpBurned: claim.lpBurned,
        totalValueAfter: claim.tvAfter,
        idleBalanceAfter: postIdle,
        receipt1PositionValueAfter: postReceipt1?.positionValue ?? null,
        requestReceiptClosed: claim.requestReceiptClosed,
        escrowAfter: claim.escrowLpBalanceAfter,
      }, "signed claim simulation");
      if (!claim.checks.every((check) => check.pass)) {
        throw new Error(`signed claim simulation did not project the complete payout and closure state: ${JSON.stringify(claim.checks)}`);
      }
      if (claim.payout === null || claim.lpBurned === null) {
        throw new Error("signed claim simulation did not produce exact payout and LP-burn expectations");
      }
      const expectedPayoutRaw = claim.payout;
      const expectedLpBurnRaw = claim.lpBurned;
      return {
        prepared,
        plan: {
          before: summarize(state),
          expectedPreState: summarize(state),
          expectedPostState: claim.postState,
          requestJournal: requestJournal.path,
          requestReceiptPda: REQUEST_RECEIPT,
          expectedPayoutRaw: expectedPayoutRaw.toString(),
          expectedLpBurnRaw: expectedLpBurnRaw.toString(),
          expectedResidualTv: CLAIM_EXPECTED_RESIDUAL_RAW.toString(),
          expectedResidualIdle: CLAIM_EXPECTED_RESIDUAL_RAW.toString(),
          claim: {
            requestReceipt: REQUEST_RECEIPT,
            requestAmountLp: requestJournal.amountLpEscrowed.toString(),
            amountAssetToWithdrawRaw: requestJournal.requestReceipt.amountAssetToWithdrawRaw,
            expectedPayoutRaw: expectedPayoutRaw.toString(),
            expectedLpBurnRaw: expectedLpBurnRaw.toString(),
            expectedResidualTv: CLAIM_EXPECTED_RESIDUAL_RAW.toString(),
            expectedResidualIdle: CLAIM_EXPECTED_RESIDUAL_RAW.toString(),
          },
          eligibilityObservationSlot: eligibility.slot,
          eligibilityChainTime: eligibility.blockTime,
          claimReconciliation: claim.postState,
          transaction: {
            feePayer: ADMIN,
            signer: ADMIN,
            signerEnvVar: "SOLANA_TESTING_PK",
            instructionCount: 1,
            instructions: ["withdrawVault"],
          },
        },
      };
    },
    reconcile: async ({ pending, finalized }) => {
      const requestJournal = await verifyFinalizedRequestJournal(
        rpcUrl,
        resolve(stringAt(pending.requestJournal, "claim journal requestJournal")),
      );
      const state = await readState("finalized", true);
      if (finalized.slot <= requestJournal.finalized.slot) {
        throw new Error("finalized claim transaction predates or equals the finalized request transaction");
      }
      const before = recordAt(pending.before, "claim before");
      const claim = recordAt(pending.claim, "claim");
      if (String(pending.requestReceiptPda ?? claim.requestReceipt ?? "") !== REQUEST_RECEIPT) {
        throw new Error("RECONCILE_MISMATCH: claim journal is not bound to the request receipt PDA");
      }
      const amountAssetToWithdrawRaw = BigInt(stringAt(claim.amountAssetToWithdrawRaw, "claim.amountAssetToWithdrawRaw"));
      const expectedPayoutRaw = claimExpectedRaw(
        pending.expectedPayoutRaw ?? claim.expectedPayoutRaw,
        "claim.expectedPayoutRaw",
      );
      const expectedLpBurnRaw = claimExpectedRaw(
        pending.expectedLpBurnRaw ?? claim.expectedLpBurnRaw,
        "claim.expectedLpBurnRaw",
      );
      const claimReconciliation = claimReconciliationFromObservations(
        {
          adminUsdcBalance: BigInt(stringAt(before.adminUsdcBalance, "claim before.adminUsdcBalance")),
          idleBalance: BigInt(stringAt(before.idleBalance, "claim before.idleBalance")),
          totalValue: BigInt(stringAt(before.totalValue, "claim before.totalValue")),
          lpSupply: BigInt(stringAt(before.lpSupply, "claim before.lpSupply")),
          receipt1PositionValue: optionalBigintAt(before.receipt1PositionValue, "claim before.receipt1PositionValue"),
          reportTicket: before.reportTicket as ReturnType<typeof decodeReportTicket>,
          amountAssetToWithdrawRaw,
        },
        {
          adminUsdcBalance: state.adminUsdcBalance,
          idleBalance: state.idleBalance,
          totalValue: state.vault?.totalValue ?? null,
          lpSupply: state.lpSupply,
          receipt1PositionValue: state.receipt1?.positionValue ?? null,
          requestReceiptClosed: state.requestReceipt === null,
          escrowLpBalance: state.requestEscrowLpBalance,
          reportTicket: state.reportTicket,
        },
        requestJournal.amountLpEscrowed,
        expectedPayoutRaw,
        expectedLpBurnRaw,
      );
      assertHxtkClaimProof({
        preState: {
          requestAmountLp: requestJournal.amountLpEscrowed,
          totalValue: BigInt(stringAt(before.totalValue, "claim before.totalValue")),
          idleBalance: BigInt(stringAt(before.idleBalance, "claim before.idleBalance")),
          lpSupply: BigInt(stringAt(before.lpSupply, "claim before.lpSupply")),
          receipt1PositionValue: optionalBigintAt(before.receipt1PositionValue, "claim before.receipt1PositionValue"),
        },
        payout: claimReconciliation.payout,
        lpBurned: claimReconciliation.lpBurned,
        totalValueAfter: claimReconciliation.tvAfter,
        idleBalanceAfter: claimReconciliation.idleAfter,
        receipt1PositionValueAfter: claimReconciliation.receipt1PositionValueAfter === null
          ? null : BigInt(claimReconciliation.receipt1PositionValueAfter),
        requestReceiptClosed: claimReconciliation.requestReceiptClosed,
        escrowAfter: claimReconciliation.escrowLpBalanceAfter === null
          ? null : BigInt(claimReconciliation.escrowLpBalanceAfter),
      }, "finalized claim reconcile");
      if (!claimReconciliation.checks.every((check) => check.pass)) {
        throw new Error(`RECONCILE_MISMATCH: finalized claim did not reconcile the exact payout and closure state: ${JSON.stringify(claimReconciliation.checks)}`);
      }
      return {
        requestJournal: requestJournal.path,
        claimReconciliation: claimReconciliation.postState,
        finalizedState: summarize(state),
      };
    },
  });
}

async function cmdClaim(): Promise<number> {
  const mode = operatorMode();
  if (mode) return cmdClaimOperator(mode);
  await assertRepairPolicyRetired("claim");
  if (!currentCli().has(REQUEST_JOURNAL_FLAG)) {
    const output = {
      schema: SCHEMA,
      step: "claim",
      sent: false,
      signed: false,
      broadcast: false,
      verdict: "PENDING_FINALIZED_REQUEST",
      reason: `${REQUEST_JOURNAL_FLAG} is required; claim must bind to the new request receipt`,
    };
    console.log(toJson(output, 2));
    writeEvidence("claim", output);
    return 2;
  }
  const requestJournal = await verifyFinalizedRequestJournal(
    process.env.SOLANA_RPC_URL?.trim() || DEFAULT_RPC_URL,
    resolve(requiredJournalPath(REQUEST_JOURNAL_FLAG, "claim must follow a finalized request journal")),
  );
  const state = await readState("finalized", true);
  if (!state.requestReceipt) throw new Error("no pending withdraw request receipt on chain");
  if (!state.vault) throw new Error("vault account is absent");
  if (state.requestReceipt.amountLpEscrowed !== requestJournal.amountLpEscrowed
    || state.requestReceipt.withdrawableFromTs !== requestJournal.withdrawableFromTs) {
    throw new Error("claim request receipt differs from the finalized request journal");
  }
  assertHxtkClaimProofPreState(claimProofPreStateFromLiveState(state), "claim pre-simulate");
  const eligibility = await latestFinalizedChainTime(
    process.env.SOLANA_RPC_URL?.trim() || DEFAULT_RPC_URL,
  );
  if (state.requestReceipt.withdrawableFromTs > BigInt(eligibility.blockTime)) {
    throw new Error(
      `withdrawableFromTs ${state.requestReceipt.withdrawableFromTs} is after latest finalized chain time `
      + `${eligibility.blockTime} at slot ${eligibility.slot}`,
    );
  }
  const before = summarize(state);
  const noopAdmin = createNoopSigner(ADMIN);
  const { adminUsdcAta } = state.identity;
  const withdraw = await getWithdrawVaultInstructionAsync({
    userTransferAuthority: noopAdmin,
    protocol: PROTOCOL,
    vault: VAULT,
    vaultAssetMint: USDC,
    vaultLpMint: LP_MINT,
    requestWithdrawLpAta: PENDING_ESCROW,
    vaultAssetIdleAta: IDLE_ATA,
    vaultAssetIdleAuth: IDLE_AUTH,
    userAssetAta: adminUsdcAta,
    requestWithdrawVaultReceipt: REQUEST_RECEIPT,
    assetTokenProgram: TOKEN_PROGRAM,
    lpTokenProgram: TOKEN_PROGRAM,
    systemProgram: SYS_PROGRAM,
  }, { programAddress: VOLTR });
  if ((withdraw.accounts ?? []).length !== 13) {
    throw new Error("withdrawVault account list drifted from the 13-account wire");
  }
  const simulation = await simulate(ADMIN, [withdraw], [
    VAULT, IDLE_ATA, LP_MINT, PENDING_ESCROW, adminUsdcAta, REQUEST_RECEIPT, REPORT_TICKET, RECEIPT1,
  ]);
  const events = simulation.err === null ? decodeEvents("WithdrawVault", simulation.logs) : [];
  const event = events.length === 1
    ? events[0] as {
        user?: Address;
        userAmountAssetWithdrawn?: bigint;
        userAmountLpBurned?: bigint;
        vault?: Address;
        vaultAssetTotalValueAfter?: bigint;
      }
    : null;
  const postVault = simulation.err === null ? decodeVault(postAccount(simulation.postAccounts, VAULT)) : null;
  const postReceipt = postAccount(simulation.postAccounts, REQUEST_RECEIPT);
  const postReceipt1 = decodeStrategyReceipt(postAccount(simulation.postAccounts, RECEIPT1));
  const simulatedClaim = claimReconciliationFromObservations(
    {
      adminUsdcBalance: state.adminUsdcBalance,
      idleBalance: state.idleBalance,
      totalValue: state.vault.totalValue,
      lpSupply: state.lpSupply,
      receipt1PositionValue: state.receipt1?.positionValue ?? null,
      reportTicket: state.reportTicket,
      amountAssetToWithdrawRaw: state.requestReceipt.amountAssetToWithdrawRaw,
    },
    {
      adminUsdcBalance: postToken(simulation.postAccounts, adminUsdcAta),
      idleBalance: postToken(simulation.postAccounts, IDLE_ATA),
      totalValue: postVault?.totalValue ?? null,
      lpSupply: mintSupply(postAccount(simulation.postAccounts, LP_MINT)),
      receipt1PositionValue: postReceipt1?.positionValue ?? null,
      requestReceiptClosed: postReceipt === null,
      escrowLpBalance: postToken(simulation.postAccounts, PENDING_ESCROW),
      reportTicket: decodeReportTicket(postAccount(simulation.postAccounts, REPORT_TICKET)),
    },
    state.requestReceipt.amountLpEscrowed,
  );
  assertHxtkClaimProof({
    preState: claimProofPreStateFromLiveState(state),
    payout: simulatedClaim.payout,
    lpBurned: simulatedClaim.lpBurned,
    totalValueAfter: simulatedClaim.tvAfter,
    idleBalanceAfter: simulatedClaim.idleAfter,
    receipt1PositionValueAfter: simulatedClaim.receipt1PositionValueAfter,
    requestReceiptClosed: simulatedClaim.requestReceiptClosed,
    escrowAfter: simulatedClaim.escrowLpBalanceAfter,
  }, "claim pre-send simulation");
  if (simulatedClaim.payout === null || simulatedClaim.lpBurned === null) {
    throw new Error("claim simulation did not produce exact payout and LP-burn expectations");
  }
  const expectedPayoutRaw = simulatedClaim.payout;
  const expectedLpBurnRaw = simulatedClaim.lpBurned;
  const claim = claimReconciliationFromObservations(
    {
      adminUsdcBalance: state.adminUsdcBalance,
      idleBalance: state.idleBalance,
      totalValue: state.vault.totalValue,
      lpSupply: state.lpSupply,
      receipt1PositionValue: state.receipt1?.positionValue ?? null,
      reportTicket: state.reportTicket,
      amountAssetToWithdrawRaw: state.requestReceipt.amountAssetToWithdrawRaw,
    },
    {
      adminUsdcBalance: postToken(simulation.postAccounts, adminUsdcAta),
      idleBalance: postToken(simulation.postAccounts, IDLE_ATA),
      totalValue: postVault?.totalValue ?? null,
      lpSupply: mintSupply(postAccount(simulation.postAccounts, LP_MINT)),
      receipt1PositionValue: postReceipt1?.positionValue ?? null,
      requestReceiptClosed: postReceipt === null,
      escrowLpBalance: postToken(simulation.postAccounts, PENDING_ESCROW),
      reportTicket: decodeReportTicket(postAccount(simulation.postAccounts, REPORT_TICKET)),
    },
    state.requestReceipt.amountLpEscrowed,
    expectedPayoutRaw,
    expectedLpBurnRaw,
  );
  const checks = [
    checkRow("simulation succeeds", simulation.err === null, null,
      simulation.err === null ? null : JSON.stringify(simulation.err)),
    checkRow("exactly one withdraw event emitted", events.length === 1, 1, events.length),
    checkRow("withdraw event payout equals the proof pin",
      event?.userAmountAssetWithdrawn === CLAIM_EXPECTED_PAYOUT_RAW,
      CLAIM_EXPECTED_PAYOUT_RAW.toString(), event?.userAmountAssetWithdrawn?.toString() ?? null),
    checkRow("withdraw event LP burn equals the proof pin",
      event?.userAmountLpBurned === REQUEST_EXPECTED_LP,
      REQUEST_EXPECTED_LP.toString(), event?.userAmountLpBurned?.toString() ?? null),
    checkRow("withdraw event binds the HXtk vault",
      event?.vault?.toString() === VAULT, VAULT, event?.vault?.toString() ?? null),
    checkRow("withdraw event residual tv equals the proof pin",
      event?.vaultAssetTotalValueAfter === CLAIM_EXPECTED_RESIDUAL_RAW,
      CLAIM_EXPECTED_RESIDUAL_RAW.toString(), event?.vaultAssetTotalValueAfter?.toString() ?? null),
    ...claim.checks,
  ];
  const pass = checks.every((row) => row.pass);
  const output = {
    step: "claim", sent: false, signed: false, broadcast: false,
    verdict: pass ? "SIMULATION_PASS_UNSENT" : "SIMULATION_FAILED",
    requestJournal: requestJournal.path,
    requestReceiptPda: REQUEST_RECEIPT,
    expectedPayoutRaw: expectedPayoutRaw.toString(),
    expectedLpBurnRaw: expectedLpBurnRaw.toString(),
    expectedResidualTv: CLAIM_EXPECTED_RESIDUAL_RAW.toString(),
    expectedResidualIdle: CLAIM_EXPECTED_RESIDUAL_RAW.toString(),
    eligibilityObservationSlot: eligibility.slot,
    eligibilityChainTime: eligibility.blockTime,
    claimStage: "post-repair",
    note: "claim is bound to the finalized request journal; the simulation proves payout, book, LP-burn, closure, and ticket invariants.",
    before,
    transaction: {
      packetBytes: simulation.packetBytes, unitsConsumed: simulation.unitsConsumed,
      instructions: ["withdrawVault"], signer: ADMIN, signerEnvVar: "SOLANA_TESTING_PK",
      executeCommand: "op run --env-file=.env.1password -- env CONFIRM_MAINNET=1 bun run reset:hxtk claim --request-journal /absolute/path/hxtk-request.json --execute --journal /absolute/path/hxtk-claim.json",
    },
    checks, events,
    postState: claim.postState,
  };
  console.log(toJson(output, 2));
  writeEvidence("claim", { ...output });
  return pass ? 0 : 1;
}

async function buildRestoreDegradationInstruction(noopAdmin: ReturnType<typeof createNoopSigner>) {
  const data = Buffer.alloc(8);
  data.writeBigUInt64LE(RESTORED_DEGRADATION_SECONDS);
  return getUpdateVaultConfigInstructionAsync({
    admin: noopAdmin,
    protocol: PROTOCOL,
    vault: VAULT,
    rent: RENT_SYSVAR,
    field: VaultConfigField.LockedProfitDegradationDuration,
    data: new Uint8Array(data),
  }, { programAddress: VOLTR });
}

function assertPostClaimState(state: LiveState) {
  if (state.requestReceipt !== null || (state.requestEscrowLpBalance ?? 0n) !== 0n) {
    throw new Error("restore-degradation requires finalized claim assertions: request receipt closed and escrow drained");
  }
  if (!state.vault || state.vault.admin !== ADMIN || state.vault.manager !== SQUADS_VAULT) {
    throw new Error("restore-degradation vault authority identity drifted");
  }
  if (state.vault.lockedProfitDegradationDuration !== 0n
    || state.vault.adminPerformanceFeeBps !== 0
    || state.vault.withdrawalWaitingPeriod !== REQUEST_WAITING_PERIOD_SECONDS) {
    throw new Error("restore-degradation requires degradation 0, admin fee 0, and waiting period 600 after claim");
  }
  if (state.vault.totalValue !== CLAIM_EXPECTED_RESIDUAL_RAW
    || state.idleBalance !== CLAIM_EXPECTED_RESIDUAL_RAW
    || state.receipt1?.positionValue !== PHANTOM_NAV_RAW) {
    throw new Error(
      `restore-degradation requires proof residual tv == idle == ${CLAIM_EXPECTED_RESIDUAL_RAW} and receipt1 == ${PHANTOM_NAV_RAW}`,
    );
  }
}

function assertRestoredState(state: LiveState, expectedAdminPerformanceFeeBps: number) {
  if (state.requestReceipt !== null || (state.requestEscrowLpBalance ?? 0n) !== 0n) {
    throw new Error("finalized restore-degradation did not preserve the finalized claim closure");
  }
  if (!state.vault || state.vault.admin !== ADMIN || state.vault.manager !== SQUADS_VAULT) {
    throw new Error("finalized restore-degradation vault authority identity drifted");
  }
  if (state.vault.lockedProfitDegradationDuration !== RESTORED_DEGRADATION_SECONDS
    || state.vault.adminPerformanceFeeBps !== expectedAdminPerformanceFeeBps
    || state.vault.withdrawalWaitingPeriod !== REQUEST_WAITING_PERIOD_SECONDS) {
    throw new Error(
      `finalized restore-degradation state is not restored: degradation=${state.vault.lockedProfitDegradationDuration}, `
      + `adminPerformanceFeeBps=${state.vault.adminPerformanceFeeBps}, waitingPeriod=${state.vault.withdrawalWaitingPeriod}; `
      + `expected degradation=${RESTORED_DEGRADATION_SECONDS}, adminPerformanceFeeBps=${expectedAdminPerformanceFeeBps}, `
      + `waitingPeriod=${REQUEST_WAITING_PERIOD_SECONDS}`,
    );
  }
  if (state.vault.totalValue !== CLAIM_EXPECTED_RESIDUAL_RAW
    || state.idleBalance !== CLAIM_EXPECTED_RESIDUAL_RAW
    || state.receipt1?.positionValue !== PHANTOM_NAV_RAW) {
    throw new Error(
      `finalized restore-degradation residual drifted: tv=${state.vault.totalValue}, idle=${state.idleBalance}, `
      + `receipt1=${state.receipt1?.positionValue ?? "missing"}; expected ${CLAIM_EXPECTED_RESIDUAL_RAW}/${CLAIM_EXPECTED_RESIDUAL_RAW}/${PHANTOM_NAV_RAW}`,
    );
  }
}

async function cmdRestoreDegradationOperator(mode: RepairPolicyOperatorMode): Promise<number> {
  const journal = operatorJournal();
  const rpcUrl = operatorRpcUrl();
  return runJournaledStep({
    mode,
    step: "restore-degradation",
    schema: SCHEMA,
    journal,
    rpcUrl,
    build: async () => {
      const prerequisites = await readRestorePrerequisites(rpcUrl);
      const state = await readState("finalized", true);
      assertPostClaimState(state);
      if (!state.vault) throw new Error("vault account is absent");
      const noopAdmin = createNoopSigner(ADMIN);
      const restore = await buildRestoreDegradationInstruction(noopAdmin);
      const accounts = restore.accounts ?? [];
      if (accounts.length !== 4 || accounts[0]?.address !== ADMIN || accounts[2]?.address !== VAULT) {
        throw new Error("restore-degradation account list drifted from admin/protocol/vault/rent");
      }
      const admin = await signingMaterialFromEnvironment("SOLANA_TESTING_PK");
      if (admin.signer.address !== ADMIN) throw new Error("SOLANA_TESTING_PK is not the HXtk admin signer");
      const prepared = await prepareSignedV0Transaction({
        rpcUrl,
        feePayer: admin,
        instructions: [restore],
        inspectedAddresses: [VAULT],
        prestateAddresses: [VAULT],
        minimumContextSlot: state.contextSlot,
        commitment: "finalized",
      });
      const projectedVault = decodeVault(rawFromPreparedSnapshot(VAULT, prepared.simulation.postAccounts[0]));
      if (!projectedVault || projectedVault.lockedProfitDegradationDuration !== RESTORED_DEGRADATION_SECONDS
        || projectedVault.adminPerformanceFeeBps !== 0
        || projectedVault.withdrawalWaitingPeriod !== REQUEST_WAITING_PERIOD_SECONDS) {
        throw new Error("signed restore-degradation simulation did not project the reviewed post-claim config");
      }
      return {
        prepared,
        plan: {
          repairJournal: prerequisites.repairJournal.path,
          claimJournal: prerequisites.claimJournal.path,
          repairBlockTime: prerequisites.repairBlockTime,
          eligibleAt: prerequisites.eligibleAt,
          before: summarize(state),
          expectedPreState: summarize(state),
          expectedPostState: configPostState(rawFromPreparedSnapshot(VAULT, prepared.simulation.postAccounts[0])),
          transaction: {
            feePayer: ADMIN,
            signer: ADMIN,
            signerEnvVar: "SOLANA_TESTING_PK",
            instructionCount: 1,
            instructions: ["updateVaultConfig(LockedProfitDegradationDuration, u64 86400)"],
          },
        },
      };
    },
    reconcile: async ({ pending, finalized }) => {
      const prerequisites = await readRestorePrerequisites(rpcUrl, {
        repairJournal: resolve(stringAt(pending.repairJournal, "restore-degradation repairJournal")),
        claimJournal: resolve(stringAt(pending.claimJournal, "restore-degradation claimJournal")),
        enforceEligibility: false,
      });
      if (Number(pending.repairBlockTime) !== prerequisites.repairBlockTime
        || Number(pending.eligibleAt) !== prerequisites.eligibleAt) {
        throw new Error("restore-degradation journal timing fields differ from the finalized repair transaction");
      }
      const restoreBlockTime = finalized.blockTime;
      if (restoreBlockTime === null || restoreBlockTime === undefined) {
        throw new Error("finalized restore-degradation transaction has no blockTime");
      }
      if (restoreBlockTime - prerequisites.repairBlockTime < Number(RESTORED_DEGRADATION_SECONDS)) {
        throw new Error(
          `finalized restore-degradation transaction is too early: restore blockTime ${restoreBlockTime} - `
          + `repair blockTime ${prerequisites.repairBlockTime} < ${RESTORED_DEGRADATION_SECONDS}`,
        );
      }
      const expectedPreState = recordAt(pending.expectedPreState, "restore-degradation expectedPreState");
      const expectedAdminPerformanceFeeBps = Number(expectedPreState.adminPerformanceFeeBps);
      if (!Number.isInteger(expectedAdminPerformanceFeeBps) || expectedAdminPerformanceFeeBps < 0) {
        throw new Error("restore-degradation journal has no valid configured admin performance fee");
      }
      const state = await readState("finalized", true);
      assertRestoredState(state, expectedAdminPerformanceFeeBps);
      const expected = recordAt(pending.expectedPostState, "restore-degradation expectedPostState");
      if (String(expected.lockedProfitDegradationDuration) !== RESTORED_DEGRADATION_SECONDS.toString()
        || Number(expected.adminPerformanceFeeBps) !== expectedAdminPerformanceFeeBps) {
        throw new Error("restore-degradation journal expected post-state does not match the configured admin fee and degradation");
      }
      return {
        repairJournal: prerequisites.repairJournal.path,
        repairBlockTime: prerequisites.repairBlockTime,
        eligibleAt: prerequisites.eligibleAt,
        eligibilityObservationSlot: prerequisites.eligibilityObservationSlot,
        eligibilityChainTime: prerequisites.eligibilityChainTime,
        restoreBlockTime,
        restoreElapsedSeconds: restoreBlockTime - prerequisites.repairBlockTime,
        finalizedState: summarize(state),
      };
    },
  });
}

async function cmdRestoreDegradation(): Promise<number> {
  const mode = operatorMode();
  if (mode) return cmdRestoreDegradationOperator(mode);
  await assertRepairPolicyRetired("restore-degradation");
  const rpcUrl = process.env.SOLANA_RPC_URL?.trim() || DEFAULT_RPC_URL;
  if (!currentCli().has(REPAIR_JOURNAL_FLAG) || !currentCli().has(CLAIM_JOURNAL_FLAG)) {
    const output = {
      schema: SCHEMA,
      step: "restore-degradation",
      sent: false,
      signed: false,
      broadcast: false,
      verdict: "PENDING_FINALIZED_REPAIR_AND_CLAIM",
      reason: `${REPAIR_JOURNAL_FLAG} and ${CLAIM_JOURNAL_FLAG} are required before restoring degradation`,
    };
    console.log(toJson(output, 2));
    writeEvidence("restore-degradation", output);
    return 2;
  }
  let prerequisites: RestorePrerequisites;
  try {
    prerequisites = await readRestorePrerequisites(rpcUrl);
  } catch (error) {
    const blocker = error instanceof Error ? error.message : String(error);
    if (!blocker.startsWith("RESTORE_DEGRADATION_WAIT:")) throw error;
    const output = {
      schema: SCHEMA,
      step: "restore-degradation",
      sent: false,
      signed: false,
      broadcast: false,
      verdict: "RESTORE_DEGRADATION_WAIT",
      reason: blocker,
    };
    console.log(toJson(output, 2));
    writeEvidence("restore-degradation", output);
    return 2;
  }
  const state = await readState("finalized", true);
  assertPostClaimState(state);
  if (!state.vault) throw new Error("vault account is absent");
  const noopAdmin = createNoopSigner(ADMIN);
  const restore = await buildRestoreDegradationInstruction(noopAdmin);
  const simulation = await simulate(ADMIN, [restore], [VAULT]);
  const projectedVault = simulation.err === null ? decodeVault(postAccount(simulation.postAccounts, VAULT)) : null;
  const checks = [
    checkRow("simulation succeeds", simulation.err === null, null,
      simulation.err === null ? null : JSON.stringify(simulation.err)),
    checkRow("post-claim request receipt is closed", state.requestReceipt === null, null,
      state.requestReceipt ? "present" : null),
    checkRow("post-claim request escrow is drained", state.requestEscrowLpBalance === 0n, "0",
      state.requestEscrowLpBalance?.toString() ?? null),
    checkRow("post-claim residual tv == idle == 23",
      state.vault.totalValue === CLAIM_EXPECTED_RESIDUAL_RAW && state.idleBalance === CLAIM_EXPECTED_RESIDUAL_RAW,
      `${CLAIM_EXPECTED_RESIDUAL_RAW}/${CLAIM_EXPECTED_RESIDUAL_RAW}`,
      `${state.vault.totalValue}/${state.idleBalance}`),
    checkRow("strategy-one receipt remains 3,793,536",
      state.receipt1?.positionValue === PHANTOM_NAV_RAW,
      PHANTOM_NAV_RAW.toString(), state.receipt1?.positionValue.toString() ?? null),
    checkRow("degradation -> 86,400", projectedVault?.lockedProfitDegradationDuration === RESTORED_DEGRADATION_SECONDS,
      RESTORED_DEGRADATION_SECONDS.toString(), projectedVault?.lockedProfitDegradationDuration.toString() ?? null),
    checkRow("admin performance fee remains 0", projectedVault?.adminPerformanceFeeBps === 0, 0,
      projectedVault?.adminPerformanceFeeBps ?? null),
    checkRow("waiting period remains 600", projectedVault?.withdrawalWaitingPeriod === REQUEST_WAITING_PERIOD_SECONDS,
      REQUEST_WAITING_PERIOD_SECONDS.toString(), projectedVault?.withdrawalWaitingPeriod.toString() ?? null),
    checkRow("packet <= 1,232 bytes", simulation.packetBytes <= PACKET_LIMIT, `<= ${PACKET_LIMIT}`, simulation.packetBytes),
  ];
  const pass = checks.every((check) => check.pass);
  const output = {
    schema: SCHEMA,
    step: "restore-degradation",
    sent: false,
    signed: false,
    broadcast: false,
    verdict: pass ? "SIMULATION_PASS_UNSENT" : "SIMULATION_FAILED",
    repairJournal: prerequisites.repairJournal.path,
    claimJournal: resolve(requiredJournalPath(CLAIM_JOURNAL_FLAG, "claim journal")),
    repairBlockTime: prerequisites.repairBlockTime,
    eligibleAt: prerequisites.eligibleAt,
    before: summarize(state),
    transaction: {
      packetBytes: simulation.packetBytes,
      unitsConsumed: simulation.unitsConsumed,
      instructions: ["updateVaultConfig(LockedProfitDegradationDuration, u64 86400)"],
      signer: ADMIN,
      signerEnvVar: "SOLANA_TESTING_PK",
      executeCommand: "op run --env-file=.env.1password -- env CONFIRM_MAINNET=1 bun run reset:hxtk restore-degradation --repair-journal /absolute/path/hxtk-repair.json --claim-journal /absolute/path/hxtk-claim.json --execute --journal /absolute/path/hxtk-restore-degradation.json",
    },
    checks,
    postState: projectedVault ? configPostState(postAccount(simulation.postAccounts, VAULT)) : null,
    ...(simulation.err === null ? {} : { logs: simulation.logs }),
  };
  console.log(toJson(output, 2));
  writeEvidence("restore-degradation", output);
  return pass ? 0 : 1;
}

// ---- entrypoint --------------------------------------------------------------

const USAGE = `usage: bun run reset:hxtk <step> [--simulate|--execute --journal PATH.json|--reconcile --journal PATH.json]

steps:
  verify   read live state; evaluate the Phase 1 end-state assertions
  config   updateVaultConfig: LockedProfitDegradationDuration = 0, AdminPerformanceFee = 0
  harvest  createIdempotent manager/treasury LP ATAs + harvestFee                (Phase 2)
  cancel   cancelRequestWithdrawVault: partial refund 78,196,265 LP, burn 21,745,257 (Phase 2)
  request  requestWithdrawVault(167,797,415 LP, isAmountInLp, isWithdrawAll)     (Phase 2)
  claim    withdrawVault: payout 3,793,394, residual tv == idle == 23          (Phase 2)
  repair-policy       create the one-shot nav-refresh policy (simulate by default)
  repair              ExecuteSync arm_report + deposit_strategy(0) through it, then retire policy
  repair-policy-remove standalone recovery for the one-shot policy after repair finalization
  restore-degradation restore locked-profit degradation to 86,400 after claim + 24h

Order: verify -> config -> repair-policy -> repair-policy finalized readback -> repair ->
repair-policy-remove -> harvest -> cancel -> request -> (600 s wait) -> claim.
--simulate (default) never reads a key: the fee payer is the step's real signer
address with an empty signature slot, legal because sigVerify is false and the
RPC fee-payer check only needs a funded address.
--execute requires CONFIRM_MAINNET=1, --journal PATH.json, and the step's signer
environment. --reconcile only reads a pending journal and finalized chain state.
Both paths take an exclusive per-leg claim; use --break-claim only after
proving the recorded pid is gone.
repair requires --policy-journal; PolicyRemove additionally requires
--repair-journal; request requires --cancel-journal; claim requires
--request-journal; restore-degradation requires --repair-journal and
--claim-journal.
--send is removed and rejected; use --execute with a journal instead.`;

const PENDING_STEPS: Readonly<Record<string, string>> = {};

async function main(): Promise<number> {
  activeCli = parseHxtkCli(process.argv.slice(2));
  const step = activeCli.step;
  if (activeCli.has("--send")) {
    throw new Error("--send is removed; use --execute --journal PATH.json");
  }
  // Apply mode validation before dispatch so even read-only commands cannot
  // accidentally treat a mixed --simulate/--execute invocation as execute.
  operatorMode();
  switch (step) {
    case "verify": return cmdVerify();
    case "config": return cmdConfig();
    case "harvest": return cmdHarvest();
    case "cancel": return cmdCancel();
    case "request": return cmdRequest();
    case "claim": return cmdClaim();
    case "repair-policy": return cmdRepairPolicy();
    case "repair": return cmdRepair();
    case "repair-policy-remove": return cmdRepairPolicyRemove();
    case "restore-degradation": return cmdRestoreDegradation();
    case "":
      console.error(USAGE);
      return 2;
    default: {
      const phase = PENDING_STEPS[step];
      if (phase) {
        console.error(JSON.stringify({ verdict: "BLOCKED", blocker: `${step} is built in ${phase}` }));
        return 2;
      }
      console.error(USAGE);
      return 2;
    }
  }
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  try {
    process.exitCode = await main();
  } catch (error) {
    console.error(JSON.stringify({
      verdict: "BLOCKED",
      blocker: sanitizeError(error),
    }));
    process.exitCode = 1;
  }
}
