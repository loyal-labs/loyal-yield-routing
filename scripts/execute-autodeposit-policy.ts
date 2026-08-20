import {
  Connection,
  Keypair,
  PublicKey,
  Transaction,
  VersionedTransaction,
  sendAndConfirmTransaction,
  type AddressLookupTableAccount,
  type TransactionInstruction,
} from "@solana/web3.js";
import {
  ASSOCIATED_TOKEN_PROGRAM_ID,
  TOKEN_PROGRAM_ID,
  createAssociatedTokenAccountIdempotentInstruction,
  getAssociatedTokenAddressSync,
} from "@solana/spl-token";
import bs58 from "bs58";
import { createHash, randomUUID } from "node:crypto";
import { existsSync } from "node:fs";

import {
  attemptAllowsSafeRequeue,
  attemptHoldsClaim,
  operationalAlertForAttempt,
  settleDurableAutodepositAttempt,
  type AttemptObservation,
  type AutodepositAttemptState,
  type AutodepositOperationKind,
  type DurableAutodepositAttempt,
} from "./durable-autodeposit-confirmation";

type PreparedOperation = {
  operation: string;
  payer: PublicKey;
  instructions: readonly TransactionInstruction[];
  lookupTableAccounts: readonly AddressLookupTableAccount[];
  programId: PublicKey;
  requiresConfirmation: boolean;
  [key: string]: unknown;
};

type PreparedAutodepositPull = {
  prepared: PreparedOperation;
  persistence: {
    liquidityMint: string;
  };
};

type SmartAccountVaultsClient = {
  prepareEarnUsdcAutodepositPull?: (args: {
    policy: PublicKey;
    walletAddress: PublicKey;
    feePayer: PublicKey;
    policySigner: PublicKey;
    recurringDelegation: PublicKey;
    amountRaw: bigint;
    cluster: string;
  }) => Promise<PreparedAutodepositPull>;
};

type NeonQuery = (
  strings: TemplateStringsArray,
  ...values: unknown[]
) => Promise<unknown[]>;

type AppModules = {
  Keypair: typeof Keypair;
  PublicKey: typeof PublicKey;
  compilePreparedOperation: (args: {
    prepared: PreparedOperation;
    blockhash: string;
  }) => VersionedTransaction;
  createSmartAccountVaultsClient: (config: {
    connection: Connection;
    programId: PublicKey;
  }) => SmartAccountVaultsClient;
  getKaminoUsdcEarnTargetForCluster: (cluster: string) => {
    liquidityMint: PublicKey;
    market: PublicKey;
    reserve: PublicKey;
  };
  LoyalCluster: { MainnetBeta: string };
  neon: (databaseUrl: string) => NeonQuery;
  PROGRAM_ADDRESS: string;
  SUBSCRIPTIONS_PROGRAM_ID: PublicKey;
  SUBSCRIPTION_RECURRING_DELEGATION_AMOUNT_PER_PERIOD_OFFSET: number;
  SUBSCRIPTION_RECURRING_DELEGATION_AMOUNT_PULLED_OFFSET: number;
  SUBSCRIPTION_RECURRING_DELEGATION_DATA_LEN: number;
  SUBSCRIPTION_RECURRING_DELEGATION_DISCRIMINATOR: number;
  SUBSCRIPTION_RECURRING_DELEGATION_DISCRIMINATOR_OFFSET: number;
};

export type SweepAmountInput = {
  walletBalanceRaw: bigint;
  walletBalanceFloorRaw: bigint;
  maxAmountPerPeriodRaw: bigint | null;
  remainingAllowanceRaw?: bigint | null;
};

export type SweepAmountDecision =
  | { kind: "no_excess"; excessRaw: bigint }
  | {
      kind: "allowance_exhausted";
      excessRaw: bigint;
      remainingAllowanceRaw: bigint;
    }
  | {
      kind: "sweep";
      amountRaw: bigint;
      excessRaw: bigint;
      capped: boolean;
      cappedByMaxPerPeriod: boolean;
      cappedByRemainingAllowance: boolean;
    };

type CliOptions = {
  claimToken: string | null;
  execute: boolean;
  overrideFloorRaw: bigint | null;
  requireLotClaim: boolean;
  scheduledSlotId: bigint | null;
  targetId: bigint | null;
};

export type EligibleTarget = {
  id: bigint;
  managedVaultId?: bigint;
  settings: string;
  vaultIndex: number;
  wallet: string;
  walletUsdcAta: string;
  walletTokenAta: string;
  vaultPubkey: string;
  vaultUsdcAta: string;
  vaultTokenAta: string;
  tokenMint: string;
  sweepPolicyAccount: string;
  routePolicyId: bigint;
  routePolicyAccount: string;
  routePolicyLastSeenSlot: bigint;
  routePolicySeed: bigint;
  routeModes: string[];
  recurringDelegation: string;
  walletBalanceFloorRaw: bigint;
  maxAmountPerPeriodRaw: bigint | null;
  periodLengthSeconds: bigint | null;
  startTimestamp: bigint | null;
  currentReserve: string | null;
  currentMarket: string | null;
  currentLiquidityMint: string | null;
};

type ConfirmedPullHandoffTarget = Pick<
  EligibleTarget,
  | "id"
  | "managedVaultId"
  | "wallet"
  | "walletUsdcAta"
  | "walletTokenAta"
  | "vaultPubkey"
  | "vaultUsdcAta"
  | "vaultTokenAta"
  | "tokenMint"
>;

type DurableAutodepositTarget = ConfirmedPullHandoffTarget &
  Pick<
    EligibleTarget,
    | "settings"
    | "vaultIndex"
    | "routePolicyAccount"
    | "routePolicySeed"
    | "currentReserve"
    | "currentMarket"
    | "currentLiquidityMint"
  >;

type AutodepositDepositPlan = {
  version: 1;
  amountRaw: bigint;
  reserve: string;
  market: string;
  liquidityMint: string;
  target: DurableAutodepositTarget & { managedVaultId: bigint };
};

type AutodepositRecoveryContext = {
  attempt: DurableAutodepositAttempt;
  plan: AutodepositDepositPlan;
  target: DurableAutodepositTarget & { managedVaultId: bigint };
};

class AutodepositOwnershipLostError extends Error {}
export class AutodepositEffectAmbiguousError extends Error {}

export type DirectTopUpRecoveryAction =
  | "reconcile_persisted"
  | "prepare_or_requeue"
  | "effect_ambiguous";

export function classifyDirectTopUpRecovery(args: {
  existingAttemptState: AutodepositAttemptState | null;
  vaultAmountRaw: bigint;
  plannedAmountRaw: bigint;
  persistedSourcePreBalanceRaw: bigint | null;
}): DirectTopUpRecoveryAction {
  if (
    args.existingAttemptState !== null &&
    attemptHoldsClaim(args.existingAttemptState)
  ) {
    return "reconcile_persisted";
  }
  if (
    args.vaultAmountRaw < args.plannedAmountRaw ||
    (args.existingAttemptState !== null &&
      attemptAllowsSafeRequeue(args.existingAttemptState) &&
      args.persistedSourcePreBalanceRaw !== null &&
      args.vaultAmountRaw < args.persistedSourcePreBalanceRaw)
  ) {
    return "effect_ambiguous";
  }
  return "prepare_or_requeue";
}

export function throwIfAutodepositAttemptRequiresOperator(
  attempt: Pick<DurableAutodepositAttempt, "signature" | "state">
) {
  if (operationalAlertForAttempt(attempt.state)) {
    throw new AutodepositEffectAmbiguousError(
      `Durable Kamino top-up ${attempt.signature} has ambiguous chain effect.`
    );
  }
}

type SimulationSummary = {
  err: unknown;
  logs: string[];
  unitsConsumed: number | null;
};

type RecurringDelegationAllowance = {
  amountPerPeriodRaw: bigint;
  amountPulledInPeriodRaw: bigint;
  remainingAmountInPeriodRaw: bigint;
  periodLengthSeconds: bigint | null;
  startTimestamp: bigint | null;
  nextResetAt: string | null;
};

type ClaimedLot = {
  lotId: bigint;
  amountRaw: bigint;
};

type LotClaimResult =
  | {
      status: "selected" | "executed" | "released" | "failed";
      reason: null;
      claimToken: string;
      targetId: bigint;
      amountRaw: bigint;
      staleCheckEventId: bigint;
      lots: ClaimedLot[];
    }
  | {
      status: "noop";
      reason: string;
      claimToken: null;
      targetId: bigint;
      amountRaw: bigint;
      staleCheckEventId: bigint;
      lots: ClaimedLot[];
    };

class MissingActiveEarnRoutePolicyError extends Error {
  readonly targetId: bigint;

  constructor(targetId: bigint) {
    super(
      `Autodeposit target ${targetId} does not have an active Earn route policy.`
    );
    this.name = "MissingActiveEarnRoutePolicyError";
    this.targetId = targetId;
  }
}


const DEFAULT_COMMITMENT = "confirmed";
const DEFAULT_LOCAL_SAME_MINT_COMMAND = [
  "bun",
  "run",
  "same-mint:swap",
  "--",
] as const;
const SAME_MINT_ROUTE_MODE = "same_mint_kamino";
const USDC_MINT_ADDRESS = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v";
const USDC_DECIMALS = 6;
const PRE_SEND_FAILURE_RETRY_DELAY_SECONDS = 5 * 60;
/**
 * Backoff for a target that is correct but has nothing to act on. The five-minute
 * failure cadence exists to recover from transient faults; applying it to a vault the
 * user emptied burns a slot every five minutes forever. The lots stay claimable so a new
 * deposit still resumes the sweep, just on a cadence that matches how fast that can
 * plausibly change.
 */
const NOT_ACTIONABLE_RETRY_DELAY_SECONDS = 6 * 60 * 60;
const AUTODEPOSIT_PULL_FEE_PAYER_MIN_LAMPORTS = 50_000_000;
const AUTODEPOSIT_ALT_READINESS_TIMEOUT_MS = 240_000;
const AUTODEPOSIT_ALT_READINESS_POLL_INTERVAL_MS = 10_000;
const AUTODEPOSIT_ALT_READINESS_TIMEOUT_MS_ENV =
  "AUTODEPOSIT_ALT_READINESS_TIMEOUT_MS";
const AUTODEPOSIT_ALT_READINESS_POLL_INTERVAL_MS_ENV =
  "AUTODEPOSIT_ALT_READINESS_POLL_INTERVAL_MS";
/**
 * Balance at which the fee payer is reported as running low while it still works.
 *
 * Sized for roughly twelve hours of warning before the hard floor stops the fleet. Over
 * the 21 days to 2026-08-07 the signer spent 12.94 SOL, of which only 0.036 SOL was
 * transaction fees; the rest is rent for accounts the routes create. That makes the drain
 * bursty rather than steady, so the headroom is set from the 90th percentile of observed
 * rolling twelve-hour burn (0.494 SOL) instead of the mean (0.108 SOL), which would be
 * outrun by any ordinary busy period.
 *
 * A louder threshold is not free: sized to the 95th percentile it would sit at 1.95 SOL
 * and fire against almost every top-up the wallet has ever received.
 */
const AUTODEPOSIT_FEE_PAYER_LOW_LAMPORTS = 550_000_000;
const AUTODEPOSIT_FEE_PAYER_LOW_LAMPORTS_ENV =
  "AUTODEPOSIT_FEE_PAYER_LOW_LAMPORTS";
/** Grepped by the alerting pipeline; changing it silently disables the warning. */
export const AUTODEPOSIT_FEE_PAYER_LOW_MARKER = "autodeposit_fee_payer_low";
export const AUTODEPOSIT_FEE_PAYER_EXHAUSTED_MARKER =
  "autodeposit fee payer is out of SOL";

export function feePayerLowLamports(
  environment: Record<string, string | undefined> = process.env
): number {
  const raw = environment[AUTODEPOSIT_FEE_PAYER_LOW_LAMPORTS_ENV];
  if (!raw) {
    return AUTODEPOSIT_FEE_PAYER_LOW_LAMPORTS;
  }
  const parsed = Number(raw);
  return Number.isInteger(parsed) && parsed >= 0
    ? parsed
    : AUTODEPOSIT_FEE_PAYER_LOW_LAMPORTS;
}

/**
 * Reports a fee payer that still covers the next transaction but is heading for empty.
 *
 * Emitted on the way past rather than as a failure: the run continues normally. The
 * balance and the shortfall are structured so an operator can act without reconstructing
 * them from prose.
 */
export function reportFeePayerBalance(args: {
  feePayer: string;
  balanceLamports: number;
  minimumLamports: number;
  role: string;
  lowLamports?: number;
}): boolean {
  const lowLamports = args.lowLamports ?? feePayerLowLamports();
  if (args.balanceLamports >= lowLamports) {
    return false;
  }
  console.warn(
    JSON.stringify({
      status: AUTODEPOSIT_FEE_PAYER_LOW_MARKER,
      role: args.role,
      feePayer: args.feePayer,
      balanceLamports: args.balanceLamports,
      lowLamports,
      minimumLamports: args.minimumLamports,
      remainingTransactions: Math.floor(
        args.balanceLamports / args.minimumLamports
      ),
    })
  );
  return true;
}

export function isMissingAutodepositTokenDelegateFailure(
  error: unknown
): boolean {
  const message = error instanceof Error ? error.message : String(error);
  return (
    message.includes("Autodeposit pull simulation failed") &&
    message.includes("Program log: Error: owner does not match")
  );
}

const CLOSED_ROUTE_POLICY_PATTERN =
  /policy account ([1-9A-HJ-NP-Za-km-z]{32,44}) does not exist/;
const CLOSED_ROUTE_POLICY_COMMITMENT = "finalized";

/**
 * A full withdrawal closes the Squads route policy on chain and reclaims its rent, but
 * nothing tells this database. The stale `route_policies.active = true` keeps the target
 * eligible forever, so every scheduled slot spawns a dry run that can only fail. Only
 * treat the target's own route policy as closed; the setup policy and any other account
 * named in an error are out of scope here.
 */
export function readClosedRoutePolicyAccount(
  error: unknown,
  routePolicyAccount: string
): string | null {
  const message = error instanceof Error ? error.message : String(error);
  const match = message.match(CLOSED_ROUTE_POLICY_PATTERN);
  if (!match || match[1] !== routePolicyAccount) {
    return null;
  }
  return match[1];
}
const AUTODEPOSIT_TOP_UP_FEE_PAYER_MIN_LAMPORTS = 50_000_000;
const AUTODEPOSIT_KAMINO_TOP_UP_FAILED_EXIT_CODE_ENV =
  "AUTODEPOSIT_KAMINO_TOP_UP_FAILED_EXIT_CODE";
const AUTODEPOSIT_YIELD_PERSISTENCE_FAILED_EXIT_CODE_ENV =
  "AUTODEPOSIT_YIELD_PERSISTENCE_FAILED_EXIT_CODE";
const AUTODEPOSIT_PREFLIGHT_BLOCKED_EXIT_CODE_ENV =
  "AUTODEPOSIT_PREFLIGHT_BLOCKED_EXIT_CODE";
const AUTODEPOSIT_NOT_ACTIONABLE_EXIT_CODE_ENV =
  "AUTODEPOSIT_NOT_ACTIONABLE_EXIT_CODE";
const AUTODEPOSIT_FEE_PAYER_EXHAUSTED_EXIT_CODE_ENV =
  "AUTODEPOSIT_FEE_PAYER_EXHAUSTED_EXIT_CODE";
const AUTODEPOSIT_TRANSACTION_EFFECT_AMBIGUOUS_EXIT_CODE_ENV =
  "AUTODEPOSIT_TRANSACTION_EFFECT_AMBIGUOUS_EXIT_CODE";
const AUTODEPOSIT_IDLE_HANDOFF_FAILED_EXIT_CODE_ENV =
  "AUTODEPOSIT_IDLE_HANDOFF_FAILED_EXIT_CODE";
const SOLANA_WEEK_NOTIFY_ENDPOINT_ENV = "SOLANA_WEEK_NOTIFY_ENDPOINT";
const SOLANA_WEEK_NOTIFY_SECRET_ENV = "SOLANA_WEEK_NOTIFY_SECRET";
const SOLANA_WEEK_NOTIFY_TIMEOUT_MS = 5_000;

type AutodepositExecutorFailureCode =
  | "kamino_top_up_failed"
  | "yield_persistence_failed"
  | "preflight_blocked"
  | "not_actionable"
  | "fee_payer_exhausted"
  | "transaction_effect_ambiguous"
  | "idle_handoff_failed";

const AUTODEPOSIT_EXECUTOR_FAILURE_EXIT_CODE_ENVS: Record<
  AutodepositExecutorFailureCode,
  string
> = {
  kamino_top_up_failed: AUTODEPOSIT_KAMINO_TOP_UP_FAILED_EXIT_CODE_ENV,
  yield_persistence_failed: AUTODEPOSIT_YIELD_PERSISTENCE_FAILED_EXIT_CODE_ENV,
  preflight_blocked: AUTODEPOSIT_PREFLIGHT_BLOCKED_EXIT_CODE_ENV,
  not_actionable: AUTODEPOSIT_NOT_ACTIONABLE_EXIT_CODE_ENV,
  fee_payer_exhausted: AUTODEPOSIT_FEE_PAYER_EXHAUSTED_EXIT_CODE_ENV,
  transaction_effect_ambiguous:
    AUTODEPOSIT_TRANSACTION_EFFECT_AMBIGUOUS_EXIT_CODE_ENV,
  idle_handoff_failed: AUTODEPOSIT_IDLE_HANDOFF_FAILED_EXIT_CODE_ENV,
};

export function autodepositExecutorFailureExitCode(
  failureCode: AutodepositExecutorFailureCode,
  environment: Record<string, string | undefined> = process.env
): number {
  const raw =
    environment[AUTODEPOSIT_EXECUTOR_FAILURE_EXIT_CODE_ENVS[failureCode]];
  if (!raw) {
    return 1;
  }
  const parsed = Number(raw);
  return Number.isInteger(parsed) && parsed >= 2 && parsed <= 125 ? parsed : 1;
}

async function loadAppModules(): Promise<AppModules> {
  const [
    neonModule,
    smartAccountVaultsModule,
    smartAccountsCoreModule,
    smartAccountsModule,
    loyalActionsModule,
  ] = await Promise.all([
    import("@neondatabase/serverless"),
    import("@loyal-labs/smart-account-vaults"),
    import("@loyal-labs/loyal-smart-accounts-core"),
    import("@loyal-labs/loyal-smart-accounts"),
    import("@loyal/actions"),
  ]);

  return {
    Keypair,
    PublicKey,
    compilePreparedOperation: smartAccountsCoreModule.compilePreparedOperation,
    createSmartAccountVaultsClient:
      smartAccountVaultsModule.createSmartAccountVaultsClient as unknown as AppModules["createSmartAccountVaultsClient"],
    getKaminoUsdcEarnTargetForCluster:
      loyalActionsModule.getKaminoUsdcEarnTargetForCluster as AppModules["getKaminoUsdcEarnTargetForCluster"],
    LoyalCluster: loyalActionsModule.LoyalCluster,
    neon: neonModule.neon,
    PROGRAM_ADDRESS: smartAccountsModule.PROGRAM_ADDRESS,
    SUBSCRIPTIONS_PROGRAM_ID: loyalActionsModule.SUBSCRIPTIONS_PROGRAM_ID,
    SUBSCRIPTION_RECURRING_DELEGATION_AMOUNT_PER_PERIOD_OFFSET:
      loyalActionsModule.SUBSCRIPTION_RECURRING_DELEGATION_AMOUNT_PER_PERIOD_OFFSET,
    SUBSCRIPTION_RECURRING_DELEGATION_AMOUNT_PULLED_OFFSET:
      loyalActionsModule.SUBSCRIPTION_RECURRING_DELEGATION_AMOUNT_PULLED_OFFSET,
    SUBSCRIPTION_RECURRING_DELEGATION_DATA_LEN:
      loyalActionsModule.SUBSCRIPTION_RECURRING_DELEGATION_DATA_LEN,
    SUBSCRIPTION_RECURRING_DELEGATION_DISCRIMINATOR:
      loyalActionsModule.SUBSCRIPTION_RECURRING_DELEGATION_DISCRIMINATOR,
    SUBSCRIPTION_RECURRING_DELEGATION_DISCRIMINATOR_OFFSET:
      loyalActionsModule.SUBSCRIPTION_RECURRING_DELEGATION_DISCRIMINATOR_OFFSET,
  };
}

function createPrepareConnection(connection: Connection): Connection {
  return new Proxy(connection, {
    get(target, property, receiver) {
      if (property === "getTokenAccountBalance") {
        // The app deposit prepare helper has a wallet-balance guard for manual deposits.
        // Autodeposit funds are already in the vault after the pull transaction.
        return undefined;
      }
      const value = Reflect.get(target, property, receiver);
      return typeof value === "function" ? value.bind(target) : value;
    },
  }) as Connection;
}

function assertAutodepositPullSupport(
  client: SmartAccountVaultsClient
): asserts client is SmartAccountVaultsClient & {
  prepareEarnUsdcAutodepositPull: NonNullable<
    SmartAccountVaultsClient["prepareEarnUsdcAutodepositPull"]
  >;
} {
  if (typeof client.prepareEarnUsdcAutodepositPull !== "function") {
    throw new Error(
      "@loyal-labs/smart-account-vaults does not expose prepareEarnUsdcAutodepositPull; deploy a package/image with autodeposit pull support before claiming lots."
    );
  }
}

export function computeSweepAmount(
  input: SweepAmountInput
): SweepAmountDecision {
  const excessRaw = input.walletBalanceRaw - input.walletBalanceFloorRaw;
  if (excessRaw <= BigInt(0)) {
    return { kind: "no_excess", excessRaw };
  }

  let amountRaw = excessRaw;
  let cappedByMaxPerPeriod = false;
  let cappedByRemainingAllowance = false;

  if (
    input.maxAmountPerPeriodRaw !== null &&
    input.maxAmountPerPeriodRaw > BigInt(0) &&
    amountRaw > input.maxAmountPerPeriodRaw
  ) {
    amountRaw = input.maxAmountPerPeriodRaw;
    cappedByMaxPerPeriod = true;
  }

  if (
    input.remainingAllowanceRaw !== null &&
    input.remainingAllowanceRaw !== undefined
  ) {
    if (input.remainingAllowanceRaw <= BigInt(0)) {
      return {
        kind: "allowance_exhausted",
        excessRaw,
        remainingAllowanceRaw: input.remainingAllowanceRaw,
      };
    }
    if (amountRaw > input.remainingAllowanceRaw) {
      amountRaw = input.remainingAllowanceRaw;
      cappedByRemainingAllowance = true;
    }
  }

  return {
    kind: "sweep",
    amountRaw,
    excessRaw,
    capped: cappedByMaxPerPeriod || cappedByRemainingAllowance,
    cappedByMaxPerPeriod,
    cappedByRemainingAllowance,
  };
}

export function parseKeypairSecret(value: string): Keypair {
  return parseKeypairSecretWith(Keypair, value);
}

function parseKeypairSecretWith(
  KeypairCtor: typeof Keypair,
  value: string
): Keypair {
  const trimmed = value.trim();
  const decoded = decodeSecret(trimmed);

  if (decoded.length === 32) {
    return KeypairCtor.fromSeed(decoded);
  }
  if (decoded.length === 64) {
    return KeypairCtor.fromSecretKey(decoded);
  }

  throw new Error(`Keypair secret must decode to 32 or 64 bytes.`);
}

function decodeSecret(value: string): Uint8Array {
  if (value.startsWith("[")) {
    const parsed = JSON.parse(value);
    if (
      Array.isArray(parsed) &&
      parsed.every((item) => Number.isInteger(item) && item >= 0 && item <= 255)
    ) {
      return Uint8Array.from(parsed);
    }
    throw new Error("JSON keypair secret must be an array of bytes.");
  }

  const withoutPrefix = value.replace(/^0x/i, "");
  if (/^[0-9a-fA-F]+$/.test(withoutPrefix) && withoutPrefix.length % 2 === 0) {
    const bytes = new Uint8Array(withoutPrefix.length / 2);
    for (let index = 0; index < bytes.length; index += 1) {
      bytes[index] = Number.parseInt(
        withoutPrefix.slice(index * 2, index * 2 + 2),
        16
      );
    }
    return bytes;
  }

  return bs58.decode(value);
}

function parseOptions(argv: string[]): CliOptions {
  let claimToken: string | null = null;
  let execute = false;
  let overrideFloorRaw: bigint | null = null;
  let requireLotClaim = false;
  let scheduledSlotId: bigint | null = null;
  let targetId: bigint | null = null;

  for (let index = 0; index < argv.length; index += 1) {
    const arg = argv[index];
    if (arg === "--execute") {
      execute = true;
      continue;
    }
    if (arg === "--target-id") {
      const value = argv[index + 1];
      if (!value || !/^\d+$/.test(value)) {
        throw new Error("--target-id requires an unsigned integer value.");
      }
      targetId = BigInt(value);
      index += 1;
      continue;
    }
    if (arg === "--claim-token") {
      const value = argv[index + 1];
      if (!value || value.trim().length === 0) {
        throw new Error("--claim-token requires a non-empty value.");
      }
      claimToken = value;
      requireLotClaim = true;
      index += 1;
      continue;
    }
    if (arg === "--scheduled-slot-id") {
      const value = argv[index + 1];
      if (!value || !/^\d+$/.test(value)) {
        throw new Error(
          "--scheduled-slot-id requires an unsigned integer value."
        );
      }
      scheduledSlotId = BigInt(value);
      index += 1;
      continue;
    }
    if (arg === "--require-lot-claim") {
      requireLotClaim = true;
      continue;
    }
    if (arg === "--override-floor-raw" || arg === "--floor-raw") {
      const value = argv[index + 1];
      if (!value || !/^\d+$/.test(value)) {
        throw new Error(`${arg} requires an unsigned integer raw USDC value.`);
      }
      overrideFloorRaw = BigInt(value);
      index += 1;
      continue;
    }
    throw new Error(`Unknown argument: ${arg}`);
  }

  return {
    claimToken,
    execute,
    overrideFloorRaw,
    requireLotClaim,
    scheduledSlotId,
    targetId,
  };
}

function requireEnv(name: string): string {
  const value = process.env[name];
  if (!value || value.trim().length === 0) {
    throw new Error(`${name} is required.`);
  }
  return value;
}

async function loadEligibleTarget(
  neon: AppModules["neon"],
  databaseUrl: string,
  targetId: bigint | null
): Promise<EligibleTarget | null> {
  const sql = neon(databaseUrl);
  const rows = await sql`
    SELECT
      t.id,
      t.settings,
      t.vault_index,
      t.wallet,
      COALESCE(t.wallet_usdc_ata, t.wallet_token_ata) AS wallet_usdc_ata,
      t.wallet_token_ata,
      t.vault_pubkey,
      COALESCE(t.vault_usdc_ata, t.vault_token_ata) AS vault_usdc_ata,
      t.vault_token_ata,
      t.token_mint,
      t.policy_account AS sweep_policy_account,
      t.recurring_delegation,
      t.wallet_balance_floor_raw,
      t.max_amount_per_period,
      t.period_length_seconds,
      t.start_timestamp,
      rp.policy_account AS route_policy_account,
      rp.id AS route_policy_id,
      rp.last_seen_slot AS route_policy_last_seen_slot,
      rp.policy_seed AS route_policy_seed,
      rp.route_modes AS route_modes,
      rp.managed_vault_id,
      yp.current_reserve,
      yp.current_market,
      yp.current_liquidity_mint
    FROM loyal_yield.balance_sweep_targets t
    LEFT JOIN LATERAL (
      SELECT
        rp.id,
        rp.policy_account,
        rp.last_seen_slot,
        rp.policy_seed,
        rp.route_modes,
        mv.id AS managed_vault_id
      FROM loyal_yield.managed_vaults mv
      JOIN loyal_yield.route_policies rp
        ON mv.active_policy_id = rp.id
        AND rp.active
        AND rp.authority = t.authority
        AND rp.settings = t.settings
        AND rp.vault_index = t.vault_index
        AND rp.vault_pubkey = t.vault_pubkey
      WHERE mv.settings = t.settings
        AND mv.vault_index = t.vault_index
        AND mv.vault_pubkey = t.vault_pubkey
        AND mv.active
        AND ${SAME_MINT_ROUTE_MODE} = ANY(rp.route_modes)
      LIMIT 1
    ) rp ON TRUE
    LEFT JOIN LATERAL (
      SELECT current_reserve, current_market, current_liquidity_mint
      FROM loyal_yield.user_yield_positions yp
      WHERE yp.settings = t.settings
        AND yp.vault_index = t.vault_index
        AND yp.wallet_address = t.wallet
        AND yp.status = 'active'
      ORDER BY yp.updated_at DESC, yp.id DESC
      LIMIT 1
    ) yp ON TRUE
    WHERE t.active
      AND t.lifecycle_status = 'active'
      AND t.wallet_balance_floor_raw IS NOT NULL
      AND t.recurring_delegation IS NOT NULL
      AND (${targetId === null} OR t.id = ${targetId?.toString() ?? null})
    ORDER BY t.id
    LIMIT 2
  `;

  if (rows.length === 0) {
    return null;
  }
  if (rows.length > 1 && targetId === null) {
    throw new Error(
      "Multiple eligible autodeposit targets found; pass --target-id."
    );
  }

  const row = rows[0] as Record<string, unknown>;
  const id = BigInt(readRequiredString(row.id, "id"));
  const routePolicyAccount = readNullableString(row.route_policy_account);
  if (!routePolicyAccount) {
    throw new MissingActiveEarnRoutePolicyError(id);
  }

  return {
    id,
    managedVaultId: BigInt(
      readRequiredString(row.managed_vault_id, "managed_vault_id")
    ),
    settings: readRequiredString(row.settings, "settings"),
    vaultIndex: Number(readRequiredString(row.vault_index, "vault_index")),
    wallet: readRequiredString(row.wallet, "wallet"),
    walletUsdcAta: readRequiredString(row.wallet_usdc_ata, "wallet_usdc_ata"),
    walletTokenAta: readRequiredString(
      row.wallet_token_ata,
      "wallet_token_ata"
    ),
    vaultPubkey: readRequiredString(row.vault_pubkey, "vault_pubkey"),
    vaultUsdcAta: readRequiredString(row.vault_usdc_ata, "vault_usdc_ata"),
    vaultTokenAta: readRequiredString(row.vault_token_ata, "vault_token_ata"),
    tokenMint: readRequiredString(row.token_mint, "token_mint"),
    sweepPolicyAccount: readRequiredString(
      row.sweep_policy_account,
      "sweep_policy_account"
    ),
    routePolicyId: BigInt(
      readRequiredString(row.route_policy_id, "route_policy_id")
    ),
    routePolicyAccount,
    routePolicyLastSeenSlot: BigInt(
      readRequiredString(
        row.route_policy_last_seen_slot,
        "route_policy_last_seen_slot"
      )
    ),
    routePolicySeed: BigInt(
      readRequiredString(row.route_policy_seed, "route_policy_seed")
    ),
    routeModes: readStringArray(row.route_modes, "route_modes"),
    recurringDelegation: readRequiredString(
      row.recurring_delegation,
      "recurring_delegation"
    ),
    walletBalanceFloorRaw: BigInt(
      readRequiredString(
        row.wallet_balance_floor_raw,
        "wallet_balance_floor_raw"
      )
    ),
    maxAmountPerPeriodRaw: row.max_amount_per_period
      ? BigInt(
          readRequiredString(row.max_amount_per_period, "max_amount_per_period")
        )
      : null,
    periodLengthSeconds: row.period_length_seconds
      ? BigInt(
          readRequiredString(row.period_length_seconds, "period_length_seconds")
        )
      : null,
    startTimestamp: row.start_timestamp
      ? BigInt(readRequiredString(row.start_timestamp, "start_timestamp"))
      : null,
    currentReserve: readNullableString(row.current_reserve),
    currentMarket: readNullableString(row.current_market),
    currentLiquidityMint: readNullableString(row.current_liquidity_mint),
  };
}

function readStringArray(value: unknown, label: string): string[] {
  if (!Array.isArray(value)) {
    throw new Error(`Missing ${label}.`);
  }
  return value.map((item) => item.toString());
}

function readRequiredString(value: unknown, label: string): string {
  if (value === null || value === undefined) {
    throw new Error(`Missing ${label}.`);
  }
  return value.toString();
}

function readNullableString(value: unknown): string | null {
  if (value === null || value === undefined) {
    return null;
  }
  const text = value.toString();
  return text.length > 0 ? text : null;
}

function parseClaimRows(
  rows: unknown[],
  fallbackTargetId: bigint
): LotClaimResult {
  const row = rows[0] as Record<string, unknown> | undefined;
  if (!row) {
    return {
      status: "noop",
      reason: "claim_query_returned_no_rows",
      claimToken: null,
      targetId: fallbackTargetId,
      amountRaw: BigInt(0),
      staleCheckEventId: BigInt(0),
      lots: [],
    };
  }
  const status = readRequiredString(row.status, "claim.status");
  const lotsValue = row.lots;
  const lots = Array.isArray(lotsValue)
    ? lotsValue.map((item) => {
        const lot = item as Record<string, unknown>;
        return {
          lotId: BigInt(readRequiredString(lot.lot_id, "claim.lot_id")),
          amountRaw: BigInt(
            readRequiredString(lot.amount_raw, "claim.amount_raw")
          ),
        };
      })
    : [];
  const base = {
    targetId: BigInt(readRequiredString(row.target_id, "claim.target_id")),
    amountRaw: BigInt(readRequiredString(row.amount_raw, "claim.amount_raw")),
    staleCheckEventId: BigInt(
      readRequiredString(row.stale_check_event_id, "claim.stale_check_event_id")
    ),
    lots,
  };
  if (status === "noop") {
    return {
      status,
      reason: readRequiredString(row.reason, "claim.reason"),
      claimToken: null,
      ...base,
    };
  }
  if (
    status === "selected" ||
    status === "executed" ||
    status === "released" ||
    status === "failed"
  ) {
    return {
      status,
      reason: null,
      claimToken: readRequiredString(row.claim_token, "claim.claim_token"),
      ...base,
    };
  }
  throw new Error(`Unexpected lot claim status ${status}.`);
}

async function claimAutodepositLots(args: {
  neon: AppModules["neon"];
  databaseUrl: string;
  targetId: bigint;
  tokenMint: string;
  claimToken: string;
  scheduledSlotId: bigint | null;
  walletBalanceRaw: bigint;
  walletBalanceFloorRaw: bigint;
  maxAmountPerPeriodRaw: bigint | null;
  remainingAllowanceRaw: bigint | null;
}): Promise<LotClaimResult> {
  const sql = args.neon(args.databaseUrl);
  const rows = await sql`
    WITH existing_claim AS (
      SELECT c.claim_token, c.target_id, c.amount_raw, c.status::text AS status, c.stale_check_event_id
      FROM loyal_yield.balance_sweep_lot_claims c
      JOIN loyal_yield.balance_sweep_targets t
        ON t.id = c.target_id
      WHERE c.claim_token = ${args.claimToken}
        AND c.target_id = ${args.targetId.toString()}
        AND t.token_mint = ${args.tokenMint}
      FOR UPDATE
    ),
    target_guard AS (
      SELECT id, token_mint
      FROM loyal_yield.balance_sweep_targets
      WHERE id = ${args.targetId.toString()}
        AND active
        AND lifecycle_status = 'active'
        AND token_mint = ${args.tokenMint}
      FOR UPDATE
    ),
    slot_guard AS (
      SELECT id, status::text AS status
      FROM loyal_yield.balance_sweep_scheduled_slots
      WHERE id = ${args.scheduledSlotId?.toString() ?? null}
        AND target_id = ${args.targetId.toString()}
        AND token_mint = ${args.tokenMint}
        AND status IN ('scheduled', 'requested')
        AND eligible_after <= now()
      FOR UPDATE
    ),
    stale AS (
      SELECT COALESCE(MAX(event_id), 0)::bigint AS event_id
      FROM loyal_yield.balance_sweep_wallet_balance_events
      WHERE target_id = ${args.targetId.toString()}
        AND mint = (SELECT token_mint FROM target_guard)
    ),
    processed AS (
      SELECT COALESCE(last_event_id, 0)::bigint AS event_id
      FROM loyal_yield.projection_offsets
      WHERE consumer_name = 'balance_sweep_autodeposit_trigger'
    ),
    locked_lots AS (
      SELECT
        l.id,
        l.remaining_amount_raw,
        l.eligible_after,
        l.created_at
      FROM loyal_yield.balance_sweep_surplus_lots l
      JOIN loyal_yield.balance_sweep_wallet_balance_events e
        ON e.event_id = l.source_event_id
      WHERE l.target_id = ${args.targetId.toString()}
        AND e.mint = (SELECT token_mint FROM target_guard)
        AND (
          ${args.scheduledSlotId === null}
          OR l.scheduled_slot_id = ${args.scheduledSlotId?.toString() ?? null}
        )
        AND l.status = 'open'
        AND l.remaining_amount_raw > 0
        AND (
          ${args.scheduledSlotId !== null}
          OR l.eligible_after <= now()
        )
        AND EXISTS (SELECT 1 FROM target_guard)
        AND (
          ${args.scheduledSlotId === null}
          OR EXISTS (SELECT 1 FROM slot_guard)
        )
        AND COALESCE((SELECT event_id FROM processed), 0) >= (SELECT event_id FROM stale)
        AND NOT EXISTS (SELECT 1 FROM existing_claim)
      ORDER BY l.eligible_after ASC, l.created_at ASC, l.id ASC
      FOR UPDATE SKIP LOCKED
    ),
    eligible AS (
      SELECT
        locked_lots.id,
        locked_lots.remaining_amount_raw,
        COALESCE(
          SUM(locked_lots.remaining_amount_raw) OVER (
            ORDER BY locked_lots.eligible_after ASC, locked_lots.created_at ASC, locked_lots.id ASC
            ROWS BETWEEN UNBOUNDED PRECEDING AND 1 PRECEDING
          ),
          0
        ) AS running_before
      FROM locked_lots
    ),
    caps AS (
      SELECT
        LEAST(
          COALESCE((SELECT SUM(remaining_amount_raw) FROM eligible), 0),
          GREATEST(${args.walletBalanceRaw.toString()}::bigint - ${args.walletBalanceFloorRaw.toString()}::bigint, 0),
          CASE
            WHEN COALESCE(${
              args.maxAmountPerPeriodRaw?.toString() ?? null
            }::bigint, 0) > 0
            THEN ${args.maxAmountPerPeriodRaw?.toString() ?? null}::bigint
            ELSE 9223372036854775807
          END,
          COALESCE(${
            args.remainingAllowanceRaw?.toString() ?? null
          }::bigint, 9223372036854775807)
        ) AS amount_raw,
        (SELECT event_id FROM stale) AS stale_check_event_id
    ),
    selected AS (
      SELECT
        e.id AS lot_id,
        LEAST(e.remaining_amount_raw, (SELECT amount_raw FROM caps) - e.running_before) AS amount_raw
      FROM eligible e
      WHERE e.running_before < (SELECT amount_raw FROM caps)
    ),
    inserted_claim AS (
      INSERT INTO loyal_yield.balance_sweep_lot_claims
        (claim_token, target_id, amount_raw, status, stale_check_event_id)
      SELECT
        ${args.claimToken},
        ${args.targetId.toString()},
        amount_raw,
        'selected',
        stale_check_event_id
      FROM caps
      WHERE amount_raw > 0
      ON CONFLICT (claim_token) DO NOTHING
      RETURNING claim_token, target_id, amount_raw, status::text AS status, stale_check_event_id
    ),
    inserted_items AS (
      INSERT INTO loyal_yield.balance_sweep_lot_claim_items
        (claim_token, lot_id, amount_raw)
      SELECT ${args.claimToken}, lot_id, amount_raw
      FROM selected
      WHERE EXISTS (SELECT 1 FROM inserted_claim)
      ON CONFLICT (claim_token, lot_id) DO NOTHING
      RETURNING lot_id, amount_raw
    ),
    updated_lots AS (
      UPDATE loyal_yield.balance_sweep_surplus_lots l
      SET
        remaining_amount_raw = l.remaining_amount_raw - i.amount_raw,
        status = CASE
          WHEN l.remaining_amount_raw - i.amount_raw = 0
          THEN 'consumed'::loyal_yield.balance_sweep_surplus_lot_status
          ELSE 'open'::loyal_yield.balance_sweep_surplus_lot_status
        END,
        updated_at = now()
      FROM inserted_items i
      WHERE l.id = i.lot_id
        AND l.remaining_amount_raw >= i.amount_raw
      RETURNING l.id
    ),
    residual_slot AS (
      INSERT INTO loyal_yield.balance_sweep_scheduled_slots (
        target_id,
        token_mint,
        eligible_after,
        status
      )
      SELECT
        ${args.targetId.toString()},
        ${args.tokenMint},
        MAX(l.eligible_after),
        'scheduled'
      FROM loyal_yield.balance_sweep_surplus_lots l
      WHERE ${args.scheduledSlotId !== null}
        AND l.scheduled_slot_id = ${args.scheduledSlotId?.toString() ?? null}
        AND l.status = 'open'
        AND l.remaining_amount_raw > 0
        AND EXISTS (SELECT 1 FROM inserted_claim)
      HAVING COUNT(*) > 0
      RETURNING id
    ),
    moved_residual_lots AS (
      UPDATE loyal_yield.balance_sweep_surplus_lots l
      SET scheduled_slot_id = (SELECT id FROM residual_slot),
          updated_at = now()
      WHERE ${args.scheduledSlotId !== null}
        AND l.scheduled_slot_id = ${args.scheduledSlotId?.toString() ?? null}
        AND l.status = 'open'
        AND l.remaining_amount_raw > 0
        AND EXISTS (SELECT 1 FROM residual_slot)
      RETURNING l.id
    ),
    updated_slot AS (
      UPDATE loyal_yield.balance_sweep_scheduled_slots
      SET status = 'selected',
          claim_token = ${args.claimToken},
          last_error = NULL,
          updated_at = now()
      WHERE EXISTS (SELECT 1 FROM inserted_claim)
        AND (
          (${args.scheduledSlotId === null} AND id IN (
            SELECT DISTINCT l.scheduled_slot_id
            FROM loyal_yield.balance_sweep_surplus_lots l
            JOIN inserted_items i
              ON i.lot_id = l.id
            WHERE l.scheduled_slot_id IS NOT NULL
          ))
          OR id = ${args.scheduledSlotId?.toString() ?? null}
        )
      RETURNING id
    ),
    active_claim AS (
      SELECT * FROM existing_claim
      UNION ALL
      SELECT * FROM inserted_claim
      LIMIT 1
    ),
    active_items AS (
      SELECT i.lot_id, i.amount_raw
      FROM loyal_yield.balance_sweep_lot_claim_items i
      JOIN loyal_yield.balance_sweep_surplus_lots l
        ON l.id = i.lot_id
      JOIN loyal_yield.balance_sweep_wallet_balance_events e
        ON e.event_id = l.source_event_id
      WHERE i.claim_token = (SELECT claim_token FROM active_claim)
        AND l.target_id = (SELECT target_id FROM active_claim)
        AND e.mint = (SELECT token_mint FROM target_guard)
      ORDER BY i.lot_id ASC
    ),
    noop_reason AS (
      SELECT CASE
        WHEN NOT EXISTS (SELECT 1 FROM target_guard) THEN 'target_not_active'
        WHEN ${
          args.scheduledSlotId !== null
        } AND NOT EXISTS (SELECT 1 FROM slot_guard) THEN 'scheduled_slot_not_available'
        WHEN COALESCE((SELECT event_id FROM processed), 0) < (SELECT event_id FROM stale) THEN 'newer_unprocessed_wallet_event'
        WHEN ${args.walletBalanceRaw.toString()}::bigint - ${args.walletBalanceFloorRaw.toString()}::bigint <= 0 THEN 'wallet_balance_not_above_floor'
        WHEN COALESCE(${
          args.remainingAllowanceRaw?.toString() ?? null
        }::bigint, 1) <= 0 THEN 'allowance_exhausted'
        WHEN COALESCE((SELECT SUM(remaining_amount_raw) FROM eligible), 0) <= 0 THEN 'no_eligible_lots'
        ELSE 'claim_not_created'
      END AS reason
    )
    SELECT
      COALESCE((SELECT status FROM active_claim), 'noop') AS status,
      CASE WHEN EXISTS (SELECT 1 FROM active_claim) THEN NULL ELSE (SELECT reason FROM noop_reason) END AS reason,
      (SELECT claim_token FROM active_claim) AS claim_token,
      ${args.targetId.toString()}::bigint AS target_id,
      COALESCE((SELECT amount_raw FROM active_claim), 0)::bigint AS amount_raw,
      COALESCE((SELECT stale_check_event_id FROM active_claim), (SELECT event_id FROM stale), 0)::bigint AS stale_check_event_id,
      COALESCE(
        (SELECT jsonb_agg(jsonb_build_object('lot_id', lot_id, 'amount_raw', amount_raw) ORDER BY lot_id) FROM active_items),
        '[]'::jsonb
      ) AS lots
  `;
  return parseClaimRows(rows, args.targetId);
}

export async function releaseAutodepositLotClaim(args: {
  neon: AppModules["neon"];
  databaseUrl: string;
  claimToken: string;
  leaseToken?: string | null;
  lastError: string;
  pauseTargetForMissingDelegate: boolean;
  retryDelaySeconds?: number;
}): Promise<AutodepositLotClaimReleaseResult> {
  const sql = args.neon(args.databaseUrl);
  const retryDelaySeconds =
    args.retryDelaySeconds ?? PRE_SEND_FAILURE_RETRY_DELAY_SECONDS;
  const rows = await sql`
    WITH selected_claim AS (
      SELECT c.claim_token
      FROM loyal_yield.balance_sweep_lot_claims c
      JOIN loyal_yield.balance_sweep_targets t
        ON t.id = c.target_id
      WHERE c.claim_token = ${args.claimToken}
        AND c.status = 'selected'
        AND (
          ${args.leaseToken ?? null}::text IS NULL
          OR (
            c.autodeposit_executor_lease_token = ${args.leaseToken ?? null}
            AND c.autodeposit_executor_lease_expires_at > now()
          )
        )
        AND t.token_mint = ${USDC_MINT_ADDRESS}
    ),
    restored AS (
      UPDATE loyal_yield.balance_sweep_surplus_lots l
      SET remaining_amount_raw = LEAST(
            l.original_amount_raw,
            l.remaining_amount_raw + i.amount_raw
          ),
          status = 'open',
          eligible_after = now() + (${retryDelaySeconds} * interval '1 second'),
          updated_at = now()
      FROM loyal_yield.balance_sweep_lot_claim_items i
      JOIN loyal_yield.balance_sweep_wallet_balance_events e
        ON true
      JOIN loyal_yield.balance_sweep_lot_claims c
        ON c.claim_token = i.claim_token
      JOIN loyal_yield.balance_sweep_targets t
        ON t.id = c.target_id
      WHERE i.claim_token = (SELECT claim_token FROM selected_claim)
        AND l.id = i.lot_id
        AND l.target_id = c.target_id
        AND e.event_id = l.source_event_id
        AND e.mint = t.token_mint
        AND t.token_mint = ${USDC_MINT_ADDRESS}
      RETURNING l.id
    ),
    paused_target AS (
      UPDATE loyal_yield.balance_sweep_targets t
      SET active = false,
          lifecycle_status = 'pending_delegation',
          last_seen_at = now()
      FROM loyal_yield.balance_sweep_lot_claims c
      WHERE c.claim_token = (SELECT claim_token FROM selected_claim)
        AND c.target_id = t.id
        AND t.active
        AND t.lifecycle_status = 'active'
        AND ${args.pauseTargetForMissingDelegate}
        AND EXISTS (SELECT 1 FROM restored)
      RETURNING t.id
    ),
    updated_claim AS (
      UPDATE loyal_yield.balance_sweep_lot_claims
      SET status = 'released',
          autodeposit_executor_lease_token = NULL,
          autodeposit_executor_lease_expires_at = NULL,
          updated_at = now()
      WHERE claim_token = (SELECT claim_token FROM selected_claim)
        AND EXISTS (SELECT 1 FROM restored)
      RETURNING claim_token
    ),
    -- The replacement slot is created when the claim is taken, so it already carries the
    -- deadline the lots had *before* this failure. Only the slot deadline gates the next
    -- attempt: with a scheduled slot in hand the claim query skips the lot's own
    -- eligible_after entirely. Pushing the slot out here is what makes a long backoff
    -- take effect on the next attempt rather than one full cycle later.
    delayed_slot AS (
      UPDATE loyal_yield.balance_sweep_scheduled_slots AS slot
      SET eligible_after = GREATEST(
            slot.eligible_after,
            now() + (${retryDelaySeconds} * interval '1 second')
          ),
          updated_at = now()
      FROM loyal_yield.balance_sweep_surplus_lots AS lot
      WHERE lot.scheduled_slot_id = slot.id
        AND lot.id IN (SELECT id FROM restored)
        AND slot.status IN ('scheduled', 'requested')
      RETURNING slot.id
    ),
    updated_slot AS (
      UPDATE loyal_yield.balance_sweep_scheduled_slots
      SET status = 'scheduled',
          claim_token = NULL,
          eligible_after = now() + (${retryDelaySeconds} * interval '1 second'),
          last_error = ${args.lastError},
          updated_at = now()
      WHERE claim_token IN (SELECT claim_token FROM updated_claim)
      RETURNING id
    )
    SELECT
      EXISTS (SELECT 1 FROM updated_claim) AS claim_released,
      EXISTS (SELECT 1 FROM updated_slot) AS slot_released,
      EXISTS (SELECT 1 FROM paused_target) AS target_paused
  `;
  const row = (rows[0] ?? {}) as Record<string, unknown>;
  return {
    claimReleased: row.claim_released === true,
    slotReleased: row.slot_released === true,
    targetPaused: row.target_paused === true,
  };
}

export type AutodepositLotClaimReleaseResult = {
  claimReleased: boolean;
  slotReleased: boolean;
  targetPaused: boolean;
};

export type MissingDelegateQuarantineEvent = {
  status: "autodeposit_target_paused_missing_delegate";
  targetId: string;
  scheduledSlotId: string | null;
  recoveryOwner: "user";
  recoveryAction: "repair_autodeposit_token_delegate";
  retryable: false;
};

export type MissingDelegateQuarantineResult =
  | {
      status: "quarantined";
      release: AutodepositLotClaimReleaseResult;
      event: MissingDelegateQuarantineEvent;
    }
  | {
      status: "unproven";
      release: AutodepositLotClaimReleaseResult;
    };

/**
 * Completes the missing-delegate safety transition before allowing a caller to suppress
 * the executor alert. If the release throws, or if its atomic SQL statement cannot prove
 * every expected transition, `onQuarantined` is deliberately unreachable.
 */
export async function quarantineMissingAutodepositDelegate(args: {
  releaseClaim: () => Promise<AutodepositLotClaimReleaseResult>;
  targetId: bigint;
  scheduledSlotId: bigint | null;
  onQuarantined: (event: MissingDelegateQuarantineEvent) => void;
}): Promise<MissingDelegateQuarantineResult> {
  const release = await args.releaseClaim();
  if (!release.claimReleased || !release.slotReleased || !release.targetPaused) {
    return { status: "unproven", release };
  }

  const event: MissingDelegateQuarantineEvent = {
    status: "autodeposit_target_paused_missing_delegate",
    targetId: args.targetId.toString(),
    scheduledSlotId: args.scheduledSlotId?.toString() ?? null,
    recoveryOwner: "user",
    recoveryAction: "repair_autodeposit_token_delegate",
    retryable: false,
  };
  args.onQuarantined(event);
  return { status: "quarantined", release, event };
}

async function markScheduledSlotFailed(args: {
  neon: AppModules["neon"];
  databaseUrl: string;
  scheduledSlotId: bigint;
  targetId: bigint;
  lastError: string;
}) {
  const sql = args.neon(args.databaseUrl);
  await sql`
    UPDATE loyal_yield.balance_sweep_scheduled_slots
    SET status = 'failed',
        claim_token = NULL,
        last_error = ${args.lastError},
        updated_at = now()
    WHERE id = ${args.scheduledSlotId.toString()}
      AND target_id = ${args.targetId.toString()}
      AND status IN ('scheduled', 'requested')
  `;
}

export type ClosedRoutePolicyReconciliation =
  | {
      status: "skipped";
      reason:
        | "policy_account_exists_at_first_finalized_probe"
        | "policy_account_exists_at_second_finalized_probe"
        | "policy_binding_changed";
      firstFinalizedContextSlot: number;
      secondFinalizedContextSlot: number | null;
    }
  | {
      status: "reconciled";
      policyAccount: string;
      deactivatedPolicyIds: string[];
      firstFinalizedContextSlot: number;
      secondFinalizedContextSlot: number;
    };

type ClosedRoutePolicyTarget = Pick<
  EligibleTarget,
  | "routePolicyId"
  | "routePolicyAccount"
  | "routePolicyLastSeenSlot"
  | "settings"
  | "vaultIndex"
  | "vaultPubkey"
>;

/**
 * Reconciles a single stale route policy against chain truth. The on-chain read is the
 * authority: an error string alone must never be enough to deactivate a policy, because
 * a transient null from the worker's RPC would otherwise disable a live target.
 */
export async function reconcileClosedRoutePolicy(args: {
  connection: Pick<Connection, "getAccountInfoAndContext">;
  neon: AppModules["neon"];
  databaseUrl: string;
  target: ClosedRoutePolicyTarget;
}): Promise<ClosedRoutePolicyReconciliation> {
  const policyAccount = new PublicKey(args.target.routePolicyAccount);
  const firstProbe = await args.connection.getAccountInfoAndContext(
    policyAccount,
    CLOSED_ROUTE_POLICY_COMMITMENT
  );
  if (firstProbe.value !== null) {
    return {
      status: "skipped",
      reason: "policy_account_exists_at_first_finalized_probe",
      firstFinalizedContextSlot: firstProbe.context.slot,
      secondFinalizedContextSlot: null,
    };
  }

  // A second read fenced to the first finalized context prevents one transient
  // null from authorizing a destructive database transition. If the policy was
  // recreated after the first read, the second read observes it and fails shut.
  const secondProbe = await args.connection.getAccountInfoAndContext(
    policyAccount,
    {
      commitment: CLOSED_ROUTE_POLICY_COMMITMENT,
      minContextSlot: firstProbe.context.slot,
    }
  );
  if (secondProbe.value !== null) {
    return {
      status: "skipped",
      reason: "policy_account_exists_at_second_finalized_probe",
      firstFinalizedContextSlot: firstProbe.context.slot,
      secondFinalizedContextSlot: secondProbe.context.slot,
    };
  }

  const policyAccountText = policyAccount.toBase58();
  const sql = args.neon(args.databaseUrl);
  const rows = await sql`
    WITH locked_policy AS (
      SELECT policy.id
      FROM loyal_yield.route_policies policy
      JOIN loyal_yield.managed_vaults vault
        ON vault.active_policy_id = policy.id
       AND vault.active
       AND vault.settings = ${args.target.settings}
       AND vault.vault_index = ${args.target.vaultIndex}
       AND vault.vault_pubkey = ${args.target.vaultPubkey}
      WHERE policy.id = ${args.target.routePolicyId.toString()}
        AND policy.policy_account = ${policyAccountText}
        AND policy.last_seen_slot = ${args.target.routePolicyLastSeenSlot.toString()}
        AND policy.active
      FOR UPDATE OF policy
    )
    UPDATE loyal_yield.route_policies policy
    SET active = false,
        finalized_eligible = false,
        last_seen_at = now()
    FROM locked_policy
    WHERE policy.id = locked_policy.id
    RETURNING policy.id
  `;
  const deactivatedPolicyIds = rows.map((row) =>
    readRequiredString((row as Record<string, unknown>).id, "route_policy.id")
  );
  if (deactivatedPolicyIds.length === 0) {
    return {
      status: "skipped",
      reason: "policy_binding_changed",
      firstFinalizedContextSlot: firstProbe.context.slot,
      secondFinalizedContextSlot: secondProbe.context.slot,
    };
  }
  return {
    status: "reconciled",
    policyAccount: policyAccountText,
    deactivatedPolicyIds,
    firstFinalizedContextSlot: firstProbe.context.slot,
    secondFinalizedContextSlot: secondProbe.context.slot,
  };
}

/**
 * A finalized missing policy is an expected terminal lifecycle outcome once its
 * database binding is retired. A binding change is terminal for this stale
 * executor snapshot as well; the next scheduler read will use the new binding.
 * Contradictory finalized evidence stays actionable instead of being hidden.
 */
export function closedRoutePolicyReconciliationIsNotActionable(
  reconciliation: ClosedRoutePolicyReconciliation
): boolean {
  return (
    reconciliation.status === "reconciled" ||
    (reconciliation.status === "skipped" &&
      reconciliation.reason === "policy_binding_changed")
  );
}

/**
 * Handles the error-side reconciliation boundary shared by production and the
 * isolated worker verifier. A dry run is observational by contract and never
 * reaches either RPC or Neon, even when its simulation reports a closed policy.
 */
export async function reconcileClosedRoutePolicyFailure(args: {
  execute: boolean;
  error: unknown;
  connection: Pick<Connection, "getAccountInfoAndContext">;
  neon: AppModules["neon"];
  databaseUrl: string;
  target: ClosedRoutePolicyTarget;
}): Promise<ClosedRoutePolicyReconciliation | null> {
  if (!args.execute) {
    return null;
  }
  const closedRoutePolicy = readClosedRoutePolicyAccount(
    args.error,
    args.target.routePolicyAccount
  );
  if (!closedRoutePolicy) {
    return null;
  }
  return reconcileClosedRoutePolicy({
    connection: args.connection,
    databaseUrl: args.databaseUrl,
    neon: args.neon,
    target: args.target,
  });
}

const CURRENT_RESERVE_PROJECTION_MAX_AGE_SECONDS = 900;
const UNRESOLVED_CURRENT_RESERVE_MARKER =
  "Autodeposit target reserve could not be resolved against chain truth";

export function isUnresolvedCurrentReserveFailure(error: unknown): boolean {
  const message = error instanceof Error ? error.message : String(error);
  return message.includes(UNRESOLVED_CURRENT_RESERVE_MARKER);
}

/**
 * True when the refusal to pull was caused by a vault confirmed to hold nothing, rather
 * than by a fault. The distinction drives both the retry cadence and whether the exit
 * pages, so it is matched on the serialized reason the resolution itself emitted instead
 * of on prose that could drift.
 */
export function isDrainedVaultFailure(error: unknown): boolean {
  const message = error instanceof Error ? error.message : String(error);
  return (
    isUnresolvedCurrentReserveFailure(error) &&
    message.includes('"reason":"vault_drained"')
  );
}

/**
 * True when the run stopped because a fee payer cannot cover the next transaction.
 *
 * This is the fleet's most operationally actionable failure and its least self-evident
 * one: it stops every target at once, no code change can clear it, and the only fix is
 * sending SOL. Matched on the shared shape both balance guards emit — a role, the signer,
 * the observed lamports, and the requirement — so it cannot be confused with an ordinary
 * insufficient-funds error from somewhere else in the transaction.
 */
export function isFeePayerExhaustedFailure(error: unknown): boolean {
  const message = error instanceof Error ? error.message : String(error);
  return /fee payer \w+ has \d+ lamports; \d+ required\./.test(message);
}

/**
 * Whether a pre-send failure is worth waking the user for (ASK-2091).
 *
 * A drained vault promised nothing, and an unclassified failure is usually
 * gone by the next hourly cycle; pushing for either trains users to ignore
 * the channel that has to work when a sweep is genuinely stuck.
 * `yield_persistence_failed` never reaches here — the deposit landed and only
 * our bookkeeping failed, so the user has nothing to act on.
 */
export function shouldNotifyFailedSweep(
  failureCode: AutodepositExecutorFailureCode | null
): boolean {
  return failureCode === "fee_payer_exhausted";
}

export type AutodepositFailureDisposition = {
  /** Null when the failure is unclassified and keeps the generic exit code. */
  failureCode: AutodepositExecutorFailureCode | null;
  retryDelaySeconds: number;
};

/**
 * Maps a pre-send failure to how loudly it should exit and how soon it should be retried.
 *
 * These two answers have to be decided together. An exit that pages but backs off for
 * hours hides a live fault, and an exit that stays quiet but retries every five minutes
 * silently burns slots forever. Keeping the pair in one place is also what makes the
 * decision reachable from the verifier.
 */
export function autodepositFailureDisposition(
  error: unknown
): AutodepositFailureDisposition {
  // Checked first: an empty fee payer stops the run before any route reasoning, so
  // classifying it as anything else would bury a fleet-wide outage under a per-target
  // diagnosis. The fast retry cadence is kept deliberately, so the fleet resumes on its
  // own within minutes of a top-up rather than waiting out a backoff.
  if (isFeePayerExhaustedFailure(error)) {
    return {
      failureCode: "fee_payer_exhausted",
      retryDelaySeconds: PRE_SEND_FAILURE_RETRY_DELAY_SECONDS,
    };
  }
  if (isDrainedVaultFailure(error)) {
    return {
      failureCode: "not_actionable",
      retryDelaySeconds: NOT_ACTIONABLE_RETRY_DELAY_SECONDS,
    };
  }
  if (
    isTopUpPreflightBlockedFailure(error) ||
    isUnresolvedCurrentReserveFailure(error)
  ) {
    return {
      failureCode: "preflight_blocked",
      retryDelaySeconds: PRE_SEND_FAILURE_RETRY_DELAY_SECONDS,
    };
  }
  return {
    failureCode: null,
    retryDelaySeconds: PRE_SEND_FAILURE_RETRY_DELAY_SECONDS,
  };
}

export type LiveVaultPosition = {
  reserve: string;
  market: string;
  liquidityMint: string;
  amountRaw: bigint;
  observedSlot: bigint | null;
  observedAt: Date | null;
};

export type CurrentReserveResolution =
  | {
      status: "unchanged";
      reason: "no_current_reserve" | "current_reserve_is_live";
      /** True when the projection backing the kept pointer is older than the bound. */
      projectionStale?: boolean;
      liveReserves?: string[];
    }
  | {
      status: "unresolved";
      reason:
        | "no_live_position"
        | "vault_drained"
        | "multiple_live_positions"
        | "stale_projection"
        | "liquidity_mint_mismatch";
      currentReserve: string;
      liveReserves: string[];
    }
  | {
      status: "reconciled";
      from: string;
      to: LiveVaultPosition;
    };

type CurrentReserveTarget = Pick<
  EligibleTarget,
  "currentReserve" | "tokenMint" | "settings" | "vaultIndex" | "wallet"
>;

/**
 * Decides which reserve the top-up should target when the stored pointer disagrees with
 * the vault's observed holdings. The pointer is not authority: a full rebalance can move
 * every lot to another market without updating it, and depositing into the stale reserve
 * then fails because the vault has no obligation there.
 *
 * Redirecting moves user funds, so this only overrides on an unambiguous, fresh
 * observation of exactly one live position in the expected mint. Anything else is left
 * unresolved for the caller to fail on, because guessing a destination is worse than
 * stopping.
 */
export function resolveCurrentReserve(args: {
  target: CurrentReserveTarget;
  positions: LiveVaultPosition[];
  now?: Date;
  maxProjectionAgeSeconds?: number;
}): CurrentReserveResolution {
  const currentReserve = args.target.currentReserve;
  if (!currentReserve) {
    return { status: "unchanged", reason: "no_current_reserve" };
  }
  const live = args.positions.filter((position) => position.amountRaw > 0);
  const liveReserves = live.map((position) => position.reserve);
  const now = args.now ?? new Date();
  const maxAgeSeconds =
    args.maxProjectionAgeSeconds ?? CURRENT_RESERVE_PROJECTION_MAX_AGE_SECONDS;
  const isFresh = (observedAt: Date | null): boolean =>
    observedAt !== null &&
    now.getTime() - observedAt.getTime() <= maxAgeSeconds * 1_000;

  const matched = live.find((position) => position.reserve === currentReserve);
  if (matched) {
    if (matched.liquidityMint !== args.target.tokenMint) {
      return {
        status: "unresolved",
        reason: "liquidity_mint_mismatch",
        currentReserve,
        liveReserves,
      };
    }
    // Keeping the pointer is a no-op rather than a redirect, so a lagging projection
    // must not turn a healthy target into a failure: that would convert projector lag
    // into an outage. Report the staleness so it stays visible, and let the next pass
    // reconcile once the projection catches up.
    return {
      status: "unchanged",
      reason: "current_reserve_is_live",
      projectionStale: !isFresh(matched.observedAt),
      liveReserves,
    };
  }
  if (live.length === 0) {
    // "Every reserve reads zero" and "the projector gave us nothing" arrive here as the
    // same empty list, but they are opposite conditions. A fresh zero observation is
    // proof the user withdrew everything, which no retry can change; a silent projector
    // is a fault that must keep paging. Only a fresh row can tell them apart.
    //
    // The evidence must come from the target's own mint. The loader returns every mint
    // the vault touches, so a fresh row for an unrelated mint would otherwise vouch for
    // a target whose own projection is missing entirely — silencing a real projector
    // failure as a drained vault.
    const drainConfirmed = args.positions.some(
      (position) =>
        position.liquidityMint === args.target.tokenMint &&
        isFresh(position.observedAt)
    );
    return {
      status: "unresolved",
      reason: drainConfirmed ? "vault_drained" : "no_live_position",
      currentReserve,
      liveReserves,
    };
  }
  if (live.length > 1) {
    return {
      status: "unresolved",
      reason: "multiple_live_positions",
      currentReserve,
      liveReserves,
    };
  }
  const [only] = live;
  if (only.liquidityMint !== args.target.tokenMint) {
    return {
      status: "unresolved",
      reason: "liquidity_mint_mismatch",
      currentReserve,
      liveReserves,
    };
  }
  if (!isFresh(only.observedAt)) {
    return {
      status: "unresolved",
      reason: "stale_projection",
      currentReserve,
      liveReserves,
    };
  }
  return { status: "reconciled", from: currentReserve, to: only };
}

export function assertResolvedCurrentReserve(
  resolution: CurrentReserveResolution
): void {
  if (resolution.status !== "unresolved") {
    return;
  }
  throw new Error(
    `${UNRESOLVED_CURRENT_RESERVE_MARKER}; refusing to pull. ` +
      `resolution=${JSON.stringify({
        reason: resolution.reason,
        currentReserve: resolution.currentReserve,
        liveReserves: resolution.liveReserves,
      })}`
  );
}

/**
 * The pointer update is a compare-and-set, so an empty result means another writer moved
 * the pointer between the read and the write. Its observation is at least as fresh as
 * this one, so this attempt must stop rather than route on a projection that has already
 * been superseded. The next scheduled slot re-resolves from the winner's state.
 */
export function assertReconciliationPersisted(args: {
  persistedPositionIds: string[];
  from: string;
  to: string;
}): void {
  if (args.persistedPositionIds.length === 1) {
    return;
  }
  throw new Error(
    `${UNRESOLVED_CURRENT_RESERVE_MARKER}; refusing to pull. ` +
      `resolution=${JSON.stringify({
        reason: "lost_reconciliation_race",
        currentReserve: args.from,
        reconciledTo: args.to,
        persistedPositionIds: args.persistedPositionIds,
      })}`
  );
}

export async function loadLiveVaultPositions(args: {
  neon: AppModules["neon"];
  databaseUrl: string;
  target: Pick<EligibleTarget, "settings" | "vaultIndex" | "vaultPubkey">;
}): Promise<LiveVaultPosition[]> {
  const sql = args.neon(args.databaseUrl);
  // Nothing enforces one managed vault per (settings, vault_index), and a replaced or
  // deactivated vault keeps its position rows. Scoping on the full identity stops a
  // stale sibling from either redirecting the deposit or faking ambiguity.
  //
  // Zero-amount rows are kept deliberately. They carry the observation timestamp that
  // separates "the vault is confirmed empty" from "the projector told us nothing", and
  // those two cases deserve opposite handling. Callers filter for live amounts
  // themselves.
  const rows = await sql`
    SELECT position.reserve,
           position.market,
           position.liquidity_mint,
           position.amount_raw,
           position.observed_slot,
           position.observed_at
    FROM loyal_yield.vault_reserve_positions_current position
    JOIN loyal_yield.managed_vaults vault
      ON vault.id = position.vault_id
     AND vault.settings = ${args.target.settings}
     AND vault.vault_index = ${args.target.vaultIndex}
     AND vault.vault_pubkey = ${args.target.vaultPubkey}
     AND vault.active
    ORDER BY position.reserve
  `;
  return rows.map((row) => {
    const record = row as Record<string, unknown>;
    const observedAt = record.observed_at;
    const observedSlot = record.observed_slot;
    return {
      reserve: readRequiredString(record.reserve, "position.reserve"),
      market: readRequiredString(record.market, "position.market"),
      liquidityMint: readRequiredString(
        record.liquidity_mint,
        "position.liquidity_mint"
      ),
      amountRaw: BigInt(
        readRequiredString(record.amount_raw, "position.amount_raw")
      ),
      observedSlot:
        observedSlot === null || observedSlot === undefined
          ? null
          : BigInt(observedSlot.toString()),
      observedAt:
        observedAt === null || observedAt === undefined
          ? null
          : new Date(observedAt.toString()),
    };
  });
}

/**
 * Writes the corrected pointer back so the next scheduled slot starts from chain truth
 * instead of repeating the same resolution. Guarded on the stale value so a concurrent
 * writer that already fixed the row wins instead of being overwritten.
 */
export async function persistReconciledCurrentReserve(args: {
  neon: AppModules["neon"];
  databaseUrl: string;
  target: Pick<
    EligibleTarget,
    "settings" | "vaultIndex" | "wallet" | "vaultPubkey"
  >;
  from: string;
  to: LiveVaultPosition;
}): Promise<string[]> {
  const sql = args.neon(args.databaseUrl);
  const rows = await sql`
    UPDATE loyal_yield.user_yield_positions position
    SET current_reserve = ${args.to.reserve},
        current_market = ${args.to.market},
        current_liquidity_mint = ${args.to.liquidityMint},
        current_amount_raw = ${args.to.amountRaw.toString()},
        current_observed_slot = ${args.to.observedSlot?.toString() ?? null},
        current_observed_at = ${args.to.observedAt?.toISOString() ?? null},
        updated_at = now()
    WHERE position.settings = ${args.target.settings}
      AND position.vault_index = ${args.target.vaultIndex}
      AND position.vault_pubkey = ${args.target.vaultPubkey}
      AND position.wallet_address = ${args.target.wallet}
      AND position.status = 'active'
      AND position.current_reserve = ${args.from}
    RETURNING position.id
  `;
  return rows.map((row) =>
    readRequiredString(
      (row as Record<string, unknown>).id,
      "user_yield_position.id"
    )
  );
}

async function getTokenBalanceRaw(
  connection: Connection,
  tokenAccount: PublicKey
): Promise<bigint> {
  try {
    const balance = await connection.getTokenAccountBalance(
      tokenAccount,
      DEFAULT_COMMITMENT
    );
    return BigInt(balance.value.amount);
  } catch (error) {
    if (
      error instanceof Error &&
      error.message.toLowerCase().includes("could not find account")
    ) {
      return BigInt(0);
    }
    throw error;
  }
}

export function assertEmptyVaultBeforeDirectAutodeposit(
  vaultBalanceRaw: bigint
): void {
  if (vaultBalanceRaw > BigInt(0)) {
    throw new Error(
      `existing idle vault balance must drain before direct autodeposit: ${vaultBalanceRaw}`
    );
  }
}

async function ensureVaultTokenAccountBeforePull(args: {
  connection: Connection;
  execute: boolean;
  feePayer: Keypair;
  target: Pick<EligibleTarget, "tokenMint" | "vaultPubkey" | "vaultTokenAta">;
}) {
  const vault = new PublicKey(args.target.vaultPubkey);
  const mint = new PublicKey(args.target.tokenMint);
  const configuredTokenAccount = new PublicKey(args.target.vaultTokenAta);
  const derivedTokenAccount = getAssociatedTokenAddressSync(
    mint,
    vault,
    true,
    TOKEN_PROGRAM_ID,
    ASSOCIATED_TOKEN_PROGRAM_ID
  );
  if (!derivedTokenAccount.equals(configuredTokenAccount)) {
    throw new Error(
      `Configured vault token account ${configuredTokenAccount.toBase58()} does not match derived ATA ${derivedTokenAccount.toBase58()}.`
    );
  }
  if (
    await args.connection.getAccountInfo(
      configuredTokenAccount,
      DEFAULT_COMMITMENT
    )
  ) {
    return { status: "ready" as const, signature: null };
  }
  if (!args.execute) {
    return { status: "repair_required" as const, signature: null };
  }
  const transaction = new Transaction().add(
    createAssociatedTokenAccountIdempotentInstruction(
      args.feePayer.publicKey,
      configuredTokenAccount,
      vault,
      mint,
      TOKEN_PROGRAM_ID,
      ASSOCIATED_TOKEN_PROGRAM_ID
    )
  );
  const signature = await sendAndConfirmTransaction(
    args.connection,
    transaction,
    [args.feePayer],
    { commitment: DEFAULT_COMMITMENT }
  );
  const repaired = await args.connection.getAccountInfo(
    configuredTokenAccount,
    DEFAULT_COMMITMENT
  );
  if (!repaired) {
    throw new Error(
      `Vault ATA repair ${signature} confirmed without creating ${configuredTokenAccount.toBase58()}.`
    );
  }
  return { status: "repaired" as const, signature };
}

async function getContextFencedTokenBalance(args: {
  connection: Connection;
  minimumSlot: bigint;
  tokenAccount: PublicKey;
}) {
  let lastObservedSlot = BigInt(0);
  for (let attempt = 0; attempt < 20; attempt += 1) {
    const balance = await args.connection.getTokenAccountBalance(
      args.tokenAccount,
      DEFAULT_COMMITMENT
    );
    lastObservedSlot = BigInt(balance.context.slot);
    if (lastObservedSlot >= args.minimumSlot) {
      return {
        amountRaw: BigInt(balance.value.amount),
        observedSlot: lastObservedSlot,
      };
    }
    await new Promise<void>((resolve) => setTimeout(resolve, 250));
  }
  throw new Error(
    `vault token observation remained at slot ${lastObservedSlot}, before confirmed pull slot ${args.minimumSlot}`
  );
}

async function loadRecurringDelegationAllowance(args: {
  appModules: AppModules;
  connection: Connection;
  recurringDelegation: PublicKey;
  periodLengthSeconds: bigint | null;
  startTimestamp: bigint | null;
}): Promise<RecurringDelegationAllowance> {
  const account = await args.connection.getAccountInfo(
    args.recurringDelegation,
    DEFAULT_COMMITMENT
  );
  if (!account) {
    throw new Error(
      `Recurring delegation account ${args.recurringDelegation.toBase58()} was not found.`
    );
  }
  if (!account.owner.equals(args.appModules.SUBSCRIPTIONS_PROGRAM_ID)) {
    throw new Error(
      `Recurring delegation account ${args.recurringDelegation.toBase58()} is not owned by the Subscriptions program.`
    );
  }
  if (
    account.data.length <
    args.appModules.SUBSCRIPTION_RECURRING_DELEGATION_DATA_LEN
  ) {
    throw new Error(
      `Recurring delegation account ${args.recurringDelegation.toBase58()} has unexpected data length ${
        account.data.length
      }.`
    );
  }
  const discriminator =
    account.data[
      args.appModules.SUBSCRIPTION_RECURRING_DELEGATION_DISCRIMINATOR_OFFSET
    ];
  if (
    discriminator !==
    args.appModules.SUBSCRIPTION_RECURRING_DELEGATION_DISCRIMINATOR
  ) {
    throw new Error(
      `Recurring delegation account ${args.recurringDelegation.toBase58()} has unexpected discriminator ${discriminator}.`
    );
  }

  const amountPerPeriodRaw = readU64Le(
    account.data,
    args.appModules.SUBSCRIPTION_RECURRING_DELEGATION_AMOUNT_PER_PERIOD_OFFSET,
    "amount_per_period"
  );
  const amountPulledInPeriodRaw = readU64Le(
    account.data,
    args.appModules.SUBSCRIPTION_RECURRING_DELEGATION_AMOUNT_PULLED_OFFSET,
    "amount_pulled_in_period"
  );
  const remainingAmountInPeriodRaw =
    amountPerPeriodRaw > amountPulledInPeriodRaw
      ? amountPerPeriodRaw - amountPulledInPeriodRaw
      : BigInt(0);

  return {
    amountPerPeriodRaw,
    amountPulledInPeriodRaw,
    remainingAmountInPeriodRaw,
    periodLengthSeconds: args.periodLengthSeconds,
    startTimestamp: args.startTimestamp,
    nextResetAt: estimateNextResetAt(
      args.startTimestamp,
      args.periodLengthSeconds
    ),
  };
}

function readU64Le(data: Uint8Array, offset: number, label: string): bigint {
  if (offset < 0 || offset + 8 > data.length) {
    throw new Error(`Recurring delegation account is missing ${label}.`);
  }
  return new DataView(data.buffer, data.byteOffset + offset, 8).getBigUint64(
    0,
    true
  );
}

function estimateNextResetAt(
  startTimestamp: bigint | null,
  periodLengthSeconds: bigint | null
): string | null {
  if (
    startTimestamp === null ||
    periodLengthSeconds === null ||
    periodLengthSeconds <= BigInt(0)
  ) {
    return null;
  }
  const nowSeconds = BigInt(Math.floor(Date.now() / 1000));
  const nextResetSeconds =
    nowSeconds < startTimestamp
      ? startTimestamp
      : startTimestamp +
        ((nowSeconds - startTimestamp) / periodLengthSeconds + BigInt(1)) *
          periodLengthSeconds;
  return new Date(Number(nextResetSeconds) * 1000).toISOString();
}

function summarizeAllowance(allowance: RecurringDelegationAllowance) {
  return {
    amountPerPeriodRaw: allowance.amountPerPeriodRaw.toString(),
    amountPulledInPeriodRaw: allowance.amountPulledInPeriodRaw.toString(),
    remainingAmountInPeriodRaw: allowance.remainingAmountInPeriodRaw.toString(),
    amountPerPeriodUi:
      Number(allowance.amountPerPeriodRaw) / 10 ** USDC_DECIMALS,
    amountPulledInPeriodUi:
      Number(allowance.amountPulledInPeriodRaw) / 10 ** USDC_DECIMALS,
    remainingAmountInPeriodUi:
      Number(allowance.remainingAmountInPeriodRaw) / 10 ** USDC_DECIMALS,
    periodLengthSeconds: allowance.periodLengthSeconds?.toString() ?? null,
    startTimestamp: allowance.startTimestamp?.toString() ?? null,
    nextResetAt: allowance.nextResetAt,
  };
}

function summarizeLotClaim(claim: LotClaimResult) {
  return {
    status: claim.status,
    reason: claim.reason,
    claimToken: claim.claimToken,
    targetId: claim.targetId.toString(),
    amountRaw: claim.amountRaw.toString(),
    staleCheckEventId: claim.staleCheckEventId.toString(),
    lots: claim.lots.map((lot) => ({
      lotId: lot.lotId.toString(),
      amountRaw: lot.amountRaw.toString(),
    })),
  };
}

async function simulatePreparedOperation(args: {
  compilePreparedOperation: AppModules["compilePreparedOperation"];
  connection: Connection;
  prepared: PreparedOperation;
  signers: Keypair[];
}): Promise<SimulationSummary> {
  const latestBlockhash = await args.connection.getLatestBlockhash(
    DEFAULT_COMMITMENT
  );
  const transaction = args.compilePreparedOperation({
    prepared: args.prepared,
    blockhash: latestBlockhash.blockhash,
  });
  transaction.sign(args.signers);
  const result = await args.connection.simulateTransaction(transaction, {
    commitment: DEFAULT_COMMITMENT,
    replaceRecentBlockhash: true,
    sigVerify: false,
  });

  return {
    err: result.value.err ?? null,
    logs: result.value.logs ?? [],
    unitsConsumed: result.value.unitsConsumed ?? null,
  };
}

function parseDurableAutodepositAttempt(
  row: Record<string, unknown>
): DurableAutodepositAttempt {
  return {
    id: readRequiredString(row.id, "attempt.id"),
    claimToken: readRequiredString(row.claim_token, "attempt.claim_token"),
    operationKind: readRequiredString(
      row.operation_kind,
      "attempt.operation_kind"
    ) as AutodepositOperationKind,
    executionId: readNullableString(row.execution_id),
    amountRaw: BigInt(readRequiredString(row.amount_raw, "attempt.amount_raw")),
    sourcePreBalanceRaw: BigInt(
      readRequiredString(
        row.source_pre_balance_raw,
        "attempt.source_pre_balance_raw"
      )
    ),
    destinationPreBalanceRaw: BigInt(
      readRequiredString(
        row.destination_pre_balance_raw,
        "attempt.destination_pre_balance_raw"
      )
    ),
    signature: readRequiredString(row.signature, "attempt.signature"),
    signedTransactionBase64: readRequiredString(
      row.signed_transaction_base64,
      "attempt.signed_transaction_base64"
    ),
    signedTransactionSha256: readRequiredString(
      row.signed_transaction_sha256,
      "attempt.signed_transaction_sha256"
    ),
    blockhash: readRequiredString(
      row.recent_blockhash,
      "attempt.recent_blockhash"
    ),
    lastValidBlockHeight: BigInt(
      readRequiredString(
        row.last_valid_block_height,
        "attempt.last_valid_block_height"
      )
    ),
    state: readRequiredString(
      row.attempt_state,
      "attempt.attempt_state"
    ) as DurableAutodepositAttempt["state"],
    broadcastCount: Number(
      readRequiredString(row.broadcast_count, "attempt.broadcast_count")
    ),
    confirmedSlot:
      row.confirmed_slot === null || row.confirmed_slot === undefined
        ? null
        : BigInt(readRequiredString(row.confirmed_slot, "attempt.confirmed_slot")),
  };
}

async function loadDurableAutodepositAttempt(args: {
  neon: AppModules["neon"];
  databaseUrl: string;
  claimToken: string;
  operationKind: AutodepositOperationKind;
}): Promise<DurableAutodepositAttempt | null> {
  const sql = args.neon(args.databaseUrl);
  const rows = await sql`
    SELECT *
    FROM loyal_yield.balance_sweep_transaction_attempts
    WHERE claim_token = ${args.claimToken}
      AND operation_kind = ${args.operationKind}
    ORDER BY attempt_number DESC
    LIMIT 1
  `;
  const row = rows[0] as Record<string, unknown> | undefined;
  return row ? parseDurableAutodepositAttempt(row) : null;
}

function serializeAutodepositDepositPlan(plan: AutodepositDepositPlan) {
  return {
    version: plan.version,
    amountRaw: plan.amountRaw.toString(),
    reserve: plan.reserve,
    market: plan.market,
    liquidityMint: plan.liquidityMint,
    target: {
      id: plan.target.id.toString(),
      managedVaultId: plan.target.managedVaultId.toString(),
      settings: plan.target.settings,
      vaultIndex: plan.target.vaultIndex,
      wallet: plan.target.wallet,
      walletUsdcAta: plan.target.walletUsdcAta,
      walletTokenAta: plan.target.walletTokenAta,
      vaultPubkey: plan.target.vaultPubkey,
      vaultUsdcAta: plan.target.vaultUsdcAta,
      vaultTokenAta: plan.target.vaultTokenAta,
      tokenMint: plan.target.tokenMint,
      routePolicyAccount: plan.target.routePolicyAccount,
      routePolicySeed: plan.target.routePolicySeed.toString(),
      currentReserve: plan.target.currentReserve,
      currentMarket: plan.target.currentMarket,
      currentLiquidityMint: plan.target.currentLiquidityMint,
    },
  };
}

function parseAutodepositDepositPlan(value: unknown): AutodepositDepositPlan {
  const plan = readRecord(value);
  const target = readRecord(plan?.target);
  if (Number(plan?.version) !== 1 || !target) {
    throw new Error("Autodeposit claim has an invalid immutable deposit plan.");
  }
  return {
    version: 1,
    amountRaw: BigInt(readRequiredString(plan?.amountRaw, "deposit plan amountRaw")),
    reserve: readRequiredString(plan?.reserve, "deposit plan reserve"),
    market: readRequiredString(plan?.market, "deposit plan market"),
    liquidityMint: readRequiredString(
      plan?.liquidityMint,
      "deposit plan liquidityMint"
    ),
    target: {
      id: BigInt(readRequiredString(target.id, "deposit plan target id")),
      managedVaultId: BigInt(
        readRequiredString(
          target.managedVaultId,
          "deposit plan managed vault id"
        )
      ),
      settings: readRequiredString(target.settings, "deposit plan settings"),
      vaultIndex: Number(
        readRequiredString(target.vaultIndex, "deposit plan vault index")
      ),
      wallet: readRequiredString(target.wallet, "deposit plan wallet"),
      walletUsdcAta: readRequiredString(
        target.walletUsdcAta,
        "deposit plan wallet ATA"
      ),
      walletTokenAta: readRequiredString(
        target.walletTokenAta,
        "deposit plan wallet token ATA"
      ),
      vaultPubkey: readRequiredString(
        target.vaultPubkey,
        "deposit plan vault"
      ),
      vaultUsdcAta: readRequiredString(
        target.vaultUsdcAta,
        "deposit plan vault ATA"
      ),
      vaultTokenAta: readRequiredString(
        target.vaultTokenAta,
        "deposit plan vault token ATA"
      ),
      tokenMint: readRequiredString(target.tokenMint, "deposit plan mint"),
      routePolicyAccount: readRequiredString(
        target.routePolicyAccount,
        "deposit plan route policy"
      ),
      routePolicySeed: BigInt(
        readRequiredString(
          target.routePolicySeed,
          "deposit plan route policy seed"
        )
      ),
      currentReserve: readNullableString(target.currentReserve),
      currentMarket: readNullableString(target.currentMarket),
      currentLiquidityMint: readNullableString(target.currentLiquidityMint),
    },
  };
}

async function loadAutodepositRecoveryContext(args: {
  neon: AppModules["neon"];
  databaseUrl: string;
  claimToken: string;
  targetId: bigint;
  scheduledSlotId: bigint;
}): Promise<AutodepositRecoveryContext | null> {
  const sql = args.neon(args.databaseUrl);
  const rows = await sql`
    SELECT attempt.*, claim.autodeposit_deposit_plan
    FROM loyal_yield.balance_sweep_transaction_attempts AS attempt
    JOIN loyal_yield.balance_sweep_lot_claims AS claim
      ON claim.claim_token = attempt.claim_token
     AND claim.target_id = attempt.target_id
     AND claim.status = 'selected'
    JOIN loyal_yield.balance_sweep_scheduled_slots AS slot
      ON slot.id = attempt.scheduled_slot_id
     AND slot.target_id = attempt.target_id
     AND slot.claim_token = attempt.claim_token
    WHERE attempt.claim_token = ${args.claimToken}
      AND attempt.target_id = ${args.targetId.toString()}
      AND attempt.scheduled_slot_id = ${args.scheduledSlotId.toString()}
      AND attempt.operation_kind = 'pull'
      AND attempt.attempt_state IN (
        'prepared', 'submitted', 'confirmed', 'unknown', 'ambiguous'
      )
    ORDER BY attempt.attempt_number DESC
    LIMIT 1
  `;
  const row = rows[0] as Record<string, unknown> | undefined;
  if (!row) {
    return null;
  }
  const attempt = parseDurableAutodepositAttempt(row);
  if (attempt.state === "confirmed" && attempt.confirmedSlot === null) {
    throw new Error(
      `Confirmed autodeposit pull ${attempt.id} has no confirmed slot.`
    );
  }
  if (row.autodeposit_deposit_plan == null) {
    throw new Error(
      `Confirmed pull ${attempt.signature} lost its immutable deposit plan.`
    );
  }
  const plan = parseAutodepositDepositPlan(row.autodeposit_deposit_plan);
  if (
    plan.target.id !== args.targetId ||
    plan.amountRaw !== attempt.amountRaw
  ) {
    throw new Error(
      `Confirmed pull ${attempt.signature} does not match its immutable deposit plan.`
    );
  }
  return { attempt, plan, target: plan.target };
}

async function acquireAutodepositClaimLease(args: {
  neon: AppModules["neon"];
  databaseUrl: string;
  claimToken: string;
  targetId: bigint;
}): Promise<string | null> {
  const sql = args.neon(args.databaseUrl);
  const leaseToken = randomUUID();
  const rows = await sql`
    UPDATE loyal_yield.balance_sweep_lot_claims
    SET autodeposit_executor_lease_token = ${leaseToken},
        autodeposit_executor_lease_expires_at = now() + interval '10 minutes',
        updated_at = now()
    WHERE claim_token = ${args.claimToken}
      AND target_id = ${args.targetId.toString()}
      AND status = 'selected'
      AND (
        autodeposit_executor_lease_token IS NULL
        OR autodeposit_executor_lease_expires_at <= now()
      )
    RETURNING claim_token
  `;
  return rows.length === 1 ? leaseToken : null;
}

async function renewAutodepositClaimLease(args: {
  neon: AppModules["neon"];
  databaseUrl: string;
  claimToken: string;
  leaseToken: string;
}): Promise<void> {
  const sql = args.neon(args.databaseUrl);
  const rows = await sql`
    UPDATE loyal_yield.balance_sweep_lot_claims
    SET autodeposit_executor_lease_expires_at = now() + interval '10 minutes',
        updated_at = now()
    WHERE claim_token = ${args.claimToken}
      AND status = 'selected'
      AND autodeposit_executor_lease_token = ${args.leaseToken}
      AND autodeposit_executor_lease_expires_at > now()
    RETURNING claim_token
  `;
  if (rows.length !== 1) {
    throw new AutodepositOwnershipLostError(
      `Autodeposit claim ${args.claimToken} lost durable ownership.`
    );
  }
}

async function releaseAutodepositClaimLease(args: {
  neon: AppModules["neon"];
  databaseUrl: string;
  claimToken: string;
  leaseToken: string;
}): Promise<void> {
  const sql = args.neon(args.databaseUrl);
  await sql`
    UPDATE loyal_yield.balance_sweep_lot_claims
    SET autodeposit_executor_lease_token = NULL,
        autodeposit_executor_lease_expires_at = NULL,
        updated_at = now()
    WHERE claim_token = ${args.claimToken}
      AND status = 'selected'
      AND autodeposit_executor_lease_token = ${args.leaseToken}
  `;
}

async function persistAutodepositDepositPlan(args: {
  neon: AppModules["neon"];
  databaseUrl: string;
  claimToken: string;
  leaseToken: string;
  plan: AutodepositDepositPlan;
}): Promise<AutodepositDepositPlan> {
  const sql = args.neon(args.databaseUrl);
  const serialized = serializeAutodepositDepositPlan(args.plan);
  const rows = await sql`
    UPDATE loyal_yield.balance_sweep_lot_claims
    SET autodeposit_deposit_plan = COALESCE(
          autodeposit_deposit_plan,
          ${JSON.stringify(serialized)}::jsonb
        ),
        updated_at = now()
    WHERE claim_token = ${args.claimToken}
      AND status = 'selected'
      AND autodeposit_executor_lease_token = ${args.leaseToken}
      AND autodeposit_executor_lease_expires_at > now()
      AND (
        autodeposit_deposit_plan IS NULL
        OR autodeposit_deposit_plan = ${JSON.stringify(serialized)}::jsonb
      )
    RETURNING autodeposit_deposit_plan
  `;
  const row = rows[0] as Record<string, unknown> | undefined;
  if (!row) {
    throw new AutodepositOwnershipLostError(
      `Autodeposit claim ${args.claimToken} deposit plan conflicts with this execution or its lease was lost.`
    );
  }
  return parseAutodepositDepositPlan(row.autodeposit_deposit_plan);
}

async function persistPreparedAutodepositAttempt(args: {
  neon: AppModules["neon"];
  databaseUrl: string;
  claimToken: string;
  leaseToken: string;
  targetId: bigint;
  scheduledSlotId: bigint;
  operationKind: AutodepositOperationKind;
  executionId: bigint | null;
  amountRaw: bigint;
  sourcePreBalanceRaw: bigint;
  destinationPreBalanceRaw: bigint;
  signature: string;
  signedTransactionBase64: string;
  signedTransactionSha256: string;
  blockhash: string;
  lastValidBlockHeight: bigint;
}): Promise<DurableAutodepositAttempt> {
  const sql = args.neon(args.databaseUrl);
  const rows = await sql`
    WITH guarded_claim AS (
      UPDATE loyal_yield.balance_sweep_lot_claims
      SET updated_at = now()
      WHERE claim_token = ${args.claimToken}
        AND target_id = ${args.targetId.toString()}
        AND status = 'selected'
        AND autodeposit_executor_lease_token = ${args.leaseToken}
        AND autodeposit_executor_lease_expires_at > now()
      RETURNING claim_token
    ),
    existing_active AS (
      SELECT *
      FROM loyal_yield.balance_sweep_transaction_attempts
      WHERE claim_token = ${args.claimToken}
        AND operation_kind = ${args.operationKind}
        AND attempt_state IN (
          'prepared', 'submitted', 'confirmed', 'unknown', 'ambiguous'
        )
      ORDER BY attempt_number DESC
      LIMIT 1
    ),
    next_attempt AS (
      SELECT COALESCE(MAX(attempt_number), 0) + 1 AS attempt_number
      FROM loyal_yield.balance_sweep_transaction_attempts
      WHERE claim_token = ${args.claimToken}
        AND operation_kind = ${args.operationKind}
    ),
    inserted AS (
      INSERT INTO loyal_yield.balance_sweep_transaction_attempts (
        claim_token,
        target_id,
        scheduled_slot_id,
        execution_id,
        operation_kind,
        attempt_number,
        amount_raw,
        source_pre_balance_raw,
        destination_pre_balance_raw,
        signature,
        signed_transaction_base64,
        signed_transaction_sha256,
        recent_blockhash,
        last_valid_block_height,
        attempt_state
      )
      SELECT
        ${args.claimToken},
        ${args.targetId.toString()},
        ${args.scheduledSlotId.toString()},
        ${args.executionId?.toString() ?? null},
        ${args.operationKind},
        next_attempt.attempt_number,
        ${args.amountRaw.toString()},
        ${args.sourcePreBalanceRaw.toString()},
        ${args.destinationPreBalanceRaw.toString()},
        ${args.signature},
        ${args.signedTransactionBase64},
        ${args.signedTransactionSha256},
        ${args.blockhash},
        ${args.lastValidBlockHeight.toString()},
        'prepared'
      FROM next_attempt
      CROSS JOIN guarded_claim
      WHERE NOT EXISTS (SELECT 1 FROM existing_active)
      ON CONFLICT DO NOTHING
      RETURNING *
    )
    SELECT * FROM inserted
    UNION ALL
    SELECT * FROM existing_active
    LIMIT 1
  `;
  const row = rows[0] as Record<string, unknown> | undefined;
  if (!row) {
    throw new Error(
      `Could not persist or recover durable ${args.operationKind} attempt for claim ${args.claimToken}.`
    );
  }
  return parseDurableAutodepositAttempt(row);
}

function attemptErrorDetail(error: unknown): string | null {
  if (error === null || error === undefined) {
    return null;
  }
  const detail =
    error instanceof Error ? error.message : JSON.stringify(error) ?? String(error);
  return detail.slice(0, 4_000);
}

async function recordAutodepositAttemptBroadcast(args: {
  neon: AppModules["neon"];
  databaseUrl: string;
  attempt: DurableAutodepositAttempt;
  leaseToken: string;
}): Promise<DurableAutodepositAttempt> {
  const sql = args.neon(args.databaseUrl);
  const rows = await sql`
    UPDATE loyal_yield.balance_sweep_transaction_attempts
    SET attempt_state = 'submitted',
        broadcast_count = broadcast_count + 1,
        last_broadcast_at = now(),
        last_status_checked_at = now(),
        error_detail = NULL,
        updated_at = now()
    WHERE id = ${args.attempt.id}
      AND signature = ${args.attempt.signature}
      AND signed_transaction_sha256 = ${args.attempt.signedTransactionSha256}
      AND attempt_state IN ('prepared', 'submitted', 'unknown')
      AND EXISTS (
        SELECT 1
        FROM loyal_yield.balance_sweep_lot_claims AS claim
        WHERE claim.claim_token = ${args.attempt.claimToken}
          AND claim.status = 'selected'
          AND claim.autodeposit_executor_lease_token = ${args.leaseToken}
          AND claim.autodeposit_executor_lease_expires_at > now()
      )
    RETURNING *
  `;
  const row = rows[0] as Record<string, unknown> | undefined;
  if (!row) {
    throw new Error(
      `Durable attempt ${args.attempt.id} lost its immutable broadcast identity.`
    );
  }
  return parseDurableAutodepositAttempt(row);
}

async function recordAutodepositAttemptObservation(args: {
  neon: AppModules["neon"];
  databaseUrl: string;
  attempt: DurableAutodepositAttempt;
  observation: AttemptObservation;
  leaseToken: string;
}): Promise<DurableAutodepositAttempt> {
  const sql = args.neon(args.databaseUrl);
  const rows = await sql`
    UPDATE loyal_yield.balance_sweep_transaction_attempts
    SET attempt_state = ${args.observation.state},
        confirmed_slot = ${args.observation.confirmedSlot?.toString() ?? null},
        last_status_checked_at = now(),
        error_detail = ${attemptErrorDetail(args.observation.error)},
        updated_at = now()
    WHERE id = ${args.attempt.id}
      AND signature = ${args.attempt.signature}
      AND signed_transaction_sha256 = ${args.attempt.signedTransactionSha256}
      AND attempt_state IN ('prepared', 'submitted', 'unknown', 'ambiguous')
      AND EXISTS (
        SELECT 1
        FROM loyal_yield.balance_sweep_lot_claims AS claim
        WHERE claim.claim_token = ${args.attempt.claimToken}
          AND claim.status = 'selected'
          AND claim.autodeposit_executor_lease_token = ${args.leaseToken}
          AND claim.autodeposit_executor_lease_expires_at > now()
      )
    RETURNING *
  `;
  const row = rows[0] as Record<string, unknown> | undefined;
  if (!row) {
    const current = await loadDurableAutodepositAttempt({
      neon: args.neon,
      databaseUrl: args.databaseUrl,
      claimToken: args.attempt.claimToken,
      operationKind: args.attempt.operationKind,
    });
    if (current?.signature === args.attempt.signature) {
      return current;
    }
    throw new Error(
      `Durable attempt ${args.attempt.id} could not record signature observation.`
    );
  }
  return parseDurableAutodepositAttempt(row);
}

export async function observeDurableAutodepositAttempt(args: {
  connection: Connection;
  attempt: DurableAutodepositAttempt;
}): Promise<AttemptObservation> {
  let statusError: unknown = null;
  let status: Awaited<ReturnType<Connection["getSignatureStatuses"]>>["value"][number] =
    null;
  try {
    const statuses = await args.connection.getSignatureStatuses(
      [args.attempt.signature],
      { searchTransactionHistory: true }
    );
    status = statuses.value[0] ?? null;
  } catch (error) {
    statusError = error;
  }

  if (status?.err) {
    return { state: "failed", confirmedSlot: null, error: status.err };
  }
  if (
    status?.confirmationStatus === "confirmed" ||
    status?.confirmationStatus === "finalized"
  ) {
    return {
      state: "confirmed",
      confirmedSlot: BigInt(status.slot),
      error: null,
    };
  }

  let currentBlockHeight: bigint | null = null;
  let heightError: unknown = null;
  try {
    currentBlockHeight = BigInt(
      await args.connection.getBlockHeight("finalized")
    );
  } catch (error) {
    heightError = error;
  }
  const expired =
    currentBlockHeight !== null &&
    currentBlockHeight > args.attempt.lastValidBlockHeight;
  if (status || (expired && statusError !== null)) {
    return {
      state: expired ? "ambiguous" : "unknown",
      confirmedSlot: null,
      error: statusError ?? heightError,
    };
  }
  if (expired) {
    return { state: "expired", confirmedSlot: null, error: null };
  }
  return {
    state: "unknown",
    confirmedSlot: null,
    error: statusError ?? heightError,
  };
}

type DurablePreparedOperationResult =
  | {
      status: "confirmed";
      signature: string;
      slot: bigint;
      attempt: DurableAutodepositAttempt;
    }
  | {
      status: "pending" | "failed" | "expired" | "ambiguous";
      attempt: DurableAutodepositAttempt;
      error: unknown | null;
    };

async function sendPreparedOperation(args: {
  compilePreparedOperation: AppModules["compilePreparedOperation"];
  connection: Connection;
  prepared: PreparedOperation | null;
  signers: Keypair[];
  neon: AppModules["neon"];
  databaseUrl: string;
  claimToken: string;
  leaseToken: string;
  targetId: bigint;
  scheduledSlotId: bigint;
  amountRaw: bigint;
  sourcePreBalanceRaw: bigint;
  destinationPreBalanceRaw: bigint;
}): Promise<DurablePreparedOperationResult> {
  let attempt = await loadDurableAutodepositAttempt({
    neon: args.neon,
    databaseUrl: args.databaseUrl,
    claimToken: args.claimToken,
    operationKind: "pull",
  });
  if (!attempt) {
    if (!args.prepared) {
      throw new Error(
        `No persisted pull attempt or prepared operation exists for claim ${args.claimToken}.`
      );
    }
    const latestBlockhash = await args.connection.getLatestBlockhash(
      DEFAULT_COMMITMENT
    );
    const transaction = args.compilePreparedOperation({
      prepared: args.prepared,
      blockhash: latestBlockhash.blockhash,
    });
    transaction.sign(args.signers);
    const signedTransaction = transaction.serialize();
    const signatureBytes = transaction.signatures[0];
    if (!signatureBytes) {
      throw new Error("Prepared autodeposit pull has no deterministic signature.");
    }
    const signature = bs58.encode(signatureBytes);
    const signedTransactionBase64 =
      Buffer.from(signedTransaction).toString("base64");
    const signedTransactionSha256 = createHash("sha256")
      .update(signedTransaction)
      .digest("hex");
    attempt = await persistPreparedAutodepositAttempt({
      neon: args.neon,
      databaseUrl: args.databaseUrl,
      claimToken: args.claimToken,
      leaseToken: args.leaseToken,
      targetId: args.targetId,
      scheduledSlotId: args.scheduledSlotId,
      operationKind: "pull",
      executionId: null,
      amountRaw: args.amountRaw,
      sourcePreBalanceRaw: args.sourcePreBalanceRaw,
      destinationPreBalanceRaw: args.destinationPreBalanceRaw,
      signature,
      signedTransactionBase64,
      signedTransactionSha256,
      blockhash: latestBlockhash.blockhash,
      lastValidBlockHeight: BigInt(latestBlockhash.lastValidBlockHeight),
    });
  }

  const settlement = await settleDurableAutodepositAttempt({
    attempt,
    dependencies: {
      observe: (candidate) =>
        observeDurableAutodepositAttempt({
          connection: args.connection,
          attempt: candidate,
        }),
      broadcastExact: async (candidate) => {
        await renewAutodepositClaimLease({
          neon: args.neon,
          databaseUrl: args.databaseUrl,
          claimToken: args.claimToken,
          leaseToken: args.leaseToken,
        });
        const bytes = Buffer.from(
          candidate.signedTransactionBase64,
          "base64"
        );
        const transaction = VersionedTransaction.deserialize(bytes);
        const derivedSignature = transaction.signatures[0]
          ? bs58.encode(transaction.signatures[0])
          : null;
        if (derivedSignature !== candidate.signature) {
          throw new Error(
            `Persisted autodeposit transaction derives ${derivedSignature}, expected ${candidate.signature}.`
          );
        }
        return args.connection.sendRawTransaction(bytes, {
          maxRetries: 0,
          skipPreflight: true,
        });
      },
      recordBroadcast: (candidate) =>
        recordAutodepositAttemptBroadcast({
          neon: args.neon,
          databaseUrl: args.databaseUrl,
          attempt: candidate,
          leaseToken: args.leaseToken,
        }),
      recordObservation: (candidate, observation) =>
        recordAutodepositAttemptObservation({
          neon: args.neon,
          databaseUrl: args.databaseUrl,
          attempt: candidate,
          observation,
          leaseToken: args.leaseToken,
        }),
    },
  });

  if (settlement.observation.state === "confirmed") {
    if (settlement.observation.confirmedSlot === null) {
      throw new Error(
        `Confirmed autodeposit attempt ${settlement.attempt.signature} has no slot.`
      );
    }
    return {
      status: "confirmed",
      signature: settlement.attempt.signature,
      slot: settlement.observation.confirmedSlot,
      attempt: settlement.attempt,
    };
  }
  return {
    status:
      settlement.observation.state === "unknown"
        ? "pending"
        : settlement.observation.state,
    attempt: settlement.attempt,
    error: settlement.observation.error,
  };
}

async function sendPreparedTopUpOperation(args: {
  connection: Connection;
  neon: AppModules["neon"];
  databaseUrl: string;
  claimToken: string;
  leaseToken: string;
  targetId: bigint;
  scheduledSlotId: bigint;
  executionId: string;
  amountRaw: bigint;
  sourcePreBalanceRaw: bigint;
  prepare: () => Promise<SameMintTopUpResult>;
}): Promise<DurablePreparedOperationResult> {
  let attempt = await loadDurableAutodepositAttempt({
    neon: args.neon,
    databaseUrl: args.databaseUrl,
    claimToken: args.claimToken,
    operationKind: "top_up",
  });
  if (!attempt || attemptAllowsSafeRequeue(attempt.state)) {
    const dryRun = await args.prepare();
    assertNoTopUpPreflightBlockers(dryRun);
    const prepared = readRecord(
      dryRun.json?.durablePolicyDepositTransaction
    );
    const signature = readRequiredString(
      prepared?.signature,
      "durable Kamino top-up signature"
    );
    const signedTransactionBase64 = readRequiredString(
      prepared?.signedTransactionBase64,
      "durable Kamino top-up signed transaction"
    );
    const signedTransaction = Buffer.from(signedTransactionBase64, "base64");
    const signedTransactionSha256 = createHash("sha256")
      .update(signedTransaction)
      .digest("hex");
    if (
      signedTransactionSha256 !==
      readRequiredString(
        prepared?.signedTransactionSha256,
        "durable Kamino top-up transaction hash"
      )
    ) {
      throw new Error("Durable Kamino top-up transaction hash does not match its bytes.");
    }
    const transaction = VersionedTransaction.deserialize(signedTransaction);
    const derivedSignature = transaction.signatures[0]
      ? bs58.encode(transaction.signatures[0])
      : null;
    if (derivedSignature !== signature) {
      throw new Error(
        `Durable Kamino top-up bytes derive ${derivedSignature}, expected ${signature}.`
      );
    }
    attempt = await persistPreparedAutodepositAttempt({
      neon: args.neon,
      databaseUrl: args.databaseUrl,
      claimToken: args.claimToken,
      leaseToken: args.leaseToken,
      targetId: args.targetId,
      scheduledSlotId: args.scheduledSlotId,
      operationKind: "top_up",
      executionId: BigInt(args.executionId),
      amountRaw: args.amountRaw,
      sourcePreBalanceRaw: args.sourcePreBalanceRaw,
      destinationPreBalanceRaw: BigInt(0),
      signature,
      signedTransactionBase64,
      signedTransactionSha256,
      blockhash: readRequiredString(
        prepared?.recentBlockhash,
        "durable Kamino top-up recent blockhash"
      ),
      lastValidBlockHeight: BigInt(
        readRequiredString(
          prepared?.lastValidBlockHeight,
          "durable Kamino top-up last valid block height"
        )
      ),
    });
  }

  const settlement = await settleDurableAutodepositAttempt({
    attempt,
    dependencies: {
      observe: (candidate) =>
        observeDurableAutodepositAttempt({
          connection: args.connection,
          attempt: candidate,
        }),
      broadcastExact: async (candidate) => {
        await renewAutodepositClaimLease({
          neon: args.neon,
          databaseUrl: args.databaseUrl,
          claimToken: args.claimToken,
          leaseToken: args.leaseToken,
        });
        const bytes = Buffer.from(candidate.signedTransactionBase64, "base64");
        const transaction = VersionedTransaction.deserialize(bytes);
        const derivedSignature = transaction.signatures[0]
          ? bs58.encode(transaction.signatures[0])
          : null;
        if (derivedSignature !== candidate.signature) {
          throw new Error(
            `Persisted top-up transaction derives ${derivedSignature}, expected ${candidate.signature}.`
          );
        }
        return args.connection.sendRawTransaction(bytes, {
          maxRetries: 0,
          skipPreflight: true,
        });
      },
      recordBroadcast: (candidate) =>
        recordAutodepositAttemptBroadcast({
          neon: args.neon,
          databaseUrl: args.databaseUrl,
          attempt: candidate,
          leaseToken: args.leaseToken,
        }),
      recordObservation: (candidate, observation) =>
        recordAutodepositAttemptObservation({
          neon: args.neon,
          databaseUrl: args.databaseUrl,
          attempt: candidate,
          observation,
          leaseToken: args.leaseToken,
        }),
    },
  });
  if (settlement.observation.state === "confirmed") {
    if (settlement.observation.confirmedSlot === null) {
      throw new Error(
        `Confirmed top-up attempt ${settlement.attempt.signature} has no slot.`
      );
    }
    return {
      status: "confirmed",
      signature: settlement.attempt.signature,
      slot: settlement.observation.confirmedSlot,
      attempt: settlement.attempt,
    };
  }
  return {
    status:
      settlement.observation.state === "unknown"
        ? "pending"
        : settlement.observation.state,
    attempt: settlement.attempt,
    error: settlement.observation.error,
  };
}

export type SameMintTopUpResult = {
  command: string[];
  exitCode: number;
  stdout: string;
  stderr: string;
  json: Record<string, unknown> | null;
};

export type TopUpFeePayerSolSafety = {
  feePayer: string;
  balanceLamports: number;
  minimumLamports: number;
  commitment: typeof DEFAULT_COMMITMENT;
  checked: true;
};

type SolanaWeekNotifyResult =
  | {
      status: "skipped";
      reason: "missing_endpoint" | "missing_secret" | "no_scheduled_slot";
    }
  | { status: "sent"; httpStatus: number }
  | { status: "failed"; httpStatus: number | null; error: string };

function sameMintReserveSwapCommand(): string[] {
  const configured = process.env.SAME_MINT_RESERVE_SWAP_COMMAND;
  if (configured && configured.trim().length > 0) {
    return splitSimpleCommand(configured);
  }
  if (existsSync("/usr/local/bin/same-mint-reserve-swap")) {
    return ["/usr/local/bin/same-mint-reserve-swap"];
  }
  return [...DEFAULT_LOCAL_SAME_MINT_COMMAND];
}

function splitSimpleCommand(command: string): string[] {
  const parts = command.match(/(?:[^\s"]+|"[^"]*")+/g) ?? [];
  return parts.map((part) => part.replace(/^"|"$/g, ""));
}

export async function prepareSameMintReserveTopUp(args: {
  amountRaw: bigint;
  reserve: string;
  rpcUrl: string;
  target: Pick<EligibleTarget, "settings" | "vaultIndex">;
}): Promise<SameMintTopUpResult> {
  const command = [
    ...sameMintReserveSwapCommand(),
    "--settings",
    args.target.settings,
    "--vault-index",
    args.target.vaultIndex.toString(),
    "--deposit-reserve",
    args.reserve,
    args.amountRaw.toString(),
    "--rpc-url",
    args.rpcUrl,
  ];
  return runSameMintCommand(command);
}

async function reconcileDirectDepositPosition(args: {
  reserve: string;
  rpcUrl: string;
  target: Pick<EligibleTarget, "settings" | "vaultIndex">;
}): Promise<{ amountRaw: bigint; observedSlot: bigint }> {
  const result = await runSameMintCommand([
    ...sameMintReserveSwapCommand(),
    "--settings",
    args.target.settings,
    "--vault-index",
    args.target.vaultIndex.toString(),
    "--reconcile-from-chain",
    "--reconcile-current-positions",
    "--rpc-url",
    args.rpcUrl,
  ]);
  if (result.json?.status !== "current_positions_reconciled") {
    throw new Error(
      `Post-confirm Kamino reconciliation returned ${String(
        result.json?.status ?? "no status"
      )}.`
    );
  }
  const chainReconcile = readRecord(result.json.chainReconcile);
  const positions = Array.isArray(chainReconcile?.positions)
    ? chainReconcile.positions
    : [];
  const position = positions
    .map(readRecord)
    .find((candidate) => candidate?.reserve?.toString() === args.reserve);
  if (!position) {
    throw new Error(
      `Post-confirm Kamino reconciliation omitted reserve ${args.reserve}.`
    );
  }
  return {
    amountRaw: BigInt(
      readRequiredString(position.amountRaw, "post-confirm Kamino amount")
    ),
    observedSlot: BigInt(
      readRequiredString(
        chainReconcile?.observedSlot,
        "post-confirm Kamino observed slot"
      )
    ),
  };
}

export async function runMissingObligationSetup(args: {
  execute: boolean;
  reserve: string;
  rpcUrl: string;
  target: Pick<EligibleTarget, "settings" | "vaultIndex">;
}): Promise<SameMintTopUpResult> {
  const command = [
    ...sameMintReserveSwapCommand(),
    "--settings",
    args.target.settings,
    "--vault-index",
    args.target.vaultIndex.toString(),
    "--setup-obligation-reserve",
    args.reserve,
    "--rpc-url",
    args.rpcUrl,
  ];
  if (args.execute) {
    command.push("--execute");
  }

  return runSameMintCommand(command);
}

async function runSameMintCommand(
  command: string[]
): Promise<SameMintTopUpResult> {
  const subprocess = Bun.spawn(command, {
    stdout: "pipe",
    stderr: "pipe",
    env: {
      ...process.env,
      YIELD_ROUTER_KEYPAIR:
        process.env.POLICY_KEYPAIR ?? process.env.YIELD_ROUTER_KEYPAIR,
    },
  });
  const [stdout, stderr, exitCode] = await Promise.all([
    new Response(subprocess.stdout).text(),
    new Response(subprocess.stderr).text(),
    subprocess.exited,
  ]);
  const json = extractJsonObject(stdout);
  const result = { command, exitCode, stdout, stderr, json };

  if (exitCode !== 0) {
    throw new Error(
      `same-mint Kamino top-up command failed with exit code ${exitCode}: ${JSON.stringify(
        summarizeTopUpResult(result)
      )}`
    );
  }
  return result;
}

function extractJsonObject(text: string): Record<string, unknown> | null {
  const start = text.indexOf("{");
  const end = text.lastIndexOf("}");
  if (start < 0 || end < start) {
    return null;
  }
  try {
    const parsed = JSON.parse(text.slice(start, end + 1));
    return parsed && typeof parsed === "object" && !Array.isArray(parsed)
      ? (parsed as Record<string, unknown>)
      : null;
  } catch {
    return null;
  }
}

function summarizeTopUpResult(result: SameMintTopUpResult) {
  const policyDepositTransaction = readRecord(
    result.json?.policyDepositTransaction
  );
  const fundingTransaction = readRecord(result.json?.fundingTransaction);
  return {
    command: result.command.map(redactSensitiveText).join(" "),
    exitCode: result.exitCode,
    status: result.json?.status?.toString() ?? null,
    preflightBlockers: result.json?.preflightBlockers ?? null,
    missingObligationSetup: result.json?.missingObligationSetup ?? null,
    fundingSimulationError:
      fundingTransaction?.simulationError?.toString() ?? null,
    policyDepositSimulationError:
      policyDepositTransaction?.simulationError?.toString() ?? null,
    policyDepositSimulationSkippedReason:
      policyDepositTransaction?.simulationSkippedReason?.toString() ?? null,
    stdoutTail: tailLines(result.stdout, 16).map(redactSensitiveText),
    stderrTail: tailLines(result.stderr, 16).map(redactSensitiveText),
  };
}

export type TopUpLookupTableCoverage = {
  status: "ready" | "pending" | "blocked" | "unknown";
  reason: string | null;
  acceptedBy:
    | "reusable_ready"
    | "funding_deferred"
    | "account_creation_deferred"
    | null;
  blocker: string | null;
  missingAddressCount: number | null;
  packetFits: boolean | null;
  reusableReady: boolean | null;
  reusableSimulationError: string | null;
  rolloutMode: string | null;
  sharedCatalogState: string | null;
  staticCoverage: boolean | null;
  tableCount: number | null;
  vaultLiquidityTokenAccountMissing: boolean | null;
};

export function readTopUpPreflightBlockers(
  result: SameMintTopUpResult
): string[] {
  const blockers = result.json?.preflightBlockers;
  if (!Array.isArray(blockers)) {
    return [];
  }
  return blockers
    .map((blocker) => blocker?.toString() ?? "")
    .filter((blocker) => blocker.length > 0);
}

const MISSING_DEPOSIT_OBLIGATION_PATTERN =
  /deposit obligation ([1-9A-HJ-NP-Za-km-z]{32,44}) is missing for reserve ([1-9A-HJ-NP-Za-km-z]{32,44})/;

/**
 * A missing obligation is recoverable by a setup transaction, unlike the other preflight
 * blockers. Naming it separately keeps that population countable so it can be handled on
 * its own terms instead of hiding inside a generic blocked-route bucket.
 */
export function readMissingDepositObligation(
  result: SameMintTopUpResult
): { obligation: string; reserve: string } | null {
  for (const blocker of readTopUpPreflightBlockers(result)) {
    const match = blocker.match(MISSING_DEPOSIT_OBLIGATION_PATTERN);
    if (match) {
      return { obligation: match[1], reserve: match[2] };
    }
  }
  return null;
}

function blockedCoverage(
  reason: string,
  blocker: string | null
): TopUpLookupTableCoverage {
  return { ...unknownCoverage(reason), status: "blocked", blocker };
}

function unknownCoverage(reason: string): TopUpLookupTableCoverage {
  return {
    status: "unknown",
    reason,
    acceptedBy: null,
    blocker: null,
    missingAddressCount: null,
    packetFits: null,
    reusableReady: null,
    reusableSimulationError: null,
    rolloutMode: null,
    sharedCatalogState: null,
    staticCoverage: null,
    tableCount: null,
    vaultLiquidityTokenAccountMissing: null,
  };
}

function isAccountNotInitializedSimulationError(error: string): boolean {
  const normalized = error.toLowerCase();
  return (
    normalized.includes("accountnotinitialized") ||
    normalized.includes("account not initialized") ||
    normalized.includes("custom(3012)") ||
    normalized.includes("custom program error: 0xbc4")
  );
}

function readVaultLiquidityTokenAccountMissing(
  result: SameMintTopUpResult,
  reserve: string
): boolean | null {
  for (const key of ["activeChainReconcile", "chainReconcile"]) {
    const preview = readRecord(result.json?.[key]);
    const positions = Array.isArray(preview?.positions)
      ? preview.positions
      : [];
    for (const entry of positions) {
      const position = readRecord(entry);
      if (position?.reserve?.toString() !== reserve) {
        continue;
      }
      const exists = position.vaultLiquidityTokenAccountExists;
      if (typeof exists === "boolean") {
        return !exists;
      }
    }
  }
  return null;
}

/**
 * Mirrors `require_missing_token_account_deferred_simulation_coverage` in
 * same-mint-reserve-swap. `pending` means the reusable provisioner still has work to do
 * and waiting can clear it; `blocked` means the route itself is failing and waiting
 * cannot.
 */
export function readTopUpLookupTableCoverage(
  result: SameMintTopUpResult,
  reserve: string
): TopUpLookupTableCoverage {
  const resolution = readRecord(result.json?.lookupTableResolution);
  if (!resolution) {
    // The binary only builds a lookup-table phase once the policy plan exists, so a
    // plan that failed preflight leaves this field null. Reporting that as unknown ALT
    // coverage sends operators to the provisioner for a fault that lives in the route.
    const blockers = readTopUpPreflightBlockers(result);
    if (blockers.length > 0) {
      const missingObligation = readMissingDepositObligation(result);
      return blockedCoverage(
        missingObligation
          ? "route_deposit_obligation_missing"
          : "route_preflight_blocked",
        blockers[0]
      );
    }
    return unknownCoverage("missing_lookup_table_resolution");
  }
  const reusable = readRecord(resolution.reusable);
  const rollout = readRecord(resolution.rollout);
  if (!reusable || !rollout) {
    return unknownCoverage("incomplete_lookup_table_resolution");
  }

  const selection = readRecord(resolution.selection);
  const sharedCatalog = readRecord(resolution.sharedMarketCatalog);
  const missingAddresses = Array.isArray(reusable.missingAddresses)
    ? reusable.missingAddresses
    : [];
  const tables = Array.isArray(reusable.tables) ? reusable.tables : [];
  const packetFits =
    typeof reusable.packetFits === "boolean" ? reusable.packetFits : null;
  const compiledMessageSize = readRecord(reusable.transaction)?.packetSizeBytes;
  const rolloutMode = rollout.mode?.toString() ?? null;
  const sharedCatalogState = sharedCatalog?.state?.toString() ?? null;
  const blocker = selection?.blocker?.toString() ?? null;
  const reusableSimulationError = reusable.simulationError?.toString() ?? null;
  const vaultLiquidityTokenAccountMissing =
    readVaultLiquidityTokenAccountMissing(result, reserve);

  const runtimeEnabled =
    rolloutMode === "reusable_only" && rollout.forceLegacy !== true;
  const sharedCatalogCovered = sharedCatalogState === "covered";
  const staticCoverage =
    runtimeEnabled &&
    sharedCatalogCovered &&
    missingAddresses.length === 0 &&
    packetFits === true &&
    tables.length > 0 &&
    typeof compiledMessageSize === "number";
  const reusableReady = reusable.ready === true && sharedCatalogCovered;

  const acceptedBy =
    runtimeEnabled && reusableReady
      ? "reusable_ready"
      : staticCoverage && blocker?.startsWith("route_funding_required:")
      ? "funding_deferred"
      : staticCoverage &&
        vaultLiquidityTokenAccountMissing === true &&
        reusableSimulationError !== null &&
        isAccountNotInitializedSimulationError(reusableSimulationError)
      ? "account_creation_deferred"
      : null;

  const status = acceptedBy ? "ready" : staticCoverage ? "blocked" : "pending";

  return {
    status,
    reason: acceptedBy
      ? null
      : staticCoverage
      ? "reusable_coverage_is_complete_but_the_route_simulation_is_not_a_deferrable_prerequisite"
      : "reusable_coverage_is_incomplete",
    acceptedBy,
    blocker,
    missingAddressCount: missingAddresses.length,
    packetFits,
    reusableReady,
    reusableSimulationError,
    rolloutMode,
    sharedCatalogState,
    staticCoverage,
    tableCount: tables.length,
    vaultLiquidityTokenAccountMissing,
  };
}

export type TopUpLookupTableReadiness = {
  status: "ready" | "blocked" | "unknown" | "timed_out";
  attempts: number;
  waitedMs: number;
  coverage: TopUpLookupTableCoverage;
  dryRun: SameMintTopUpResult;
};

export async function awaitTopUpLookupTableReadiness(args: {
  dryRun: SameMintTopUpResult;
  refreshDryRun: () => Promise<SameMintTopUpResult>;
  reserve: string;
  timeoutMs: number;
  pollIntervalMs: number;
  now?: () => number;
  sleep?: (milliseconds: number) => Promise<void>;
}): Promise<TopUpLookupTableReadiness> {
  const now = args.now ?? (() => Date.now());
  const sleep = args.sleep ?? defaultSleep;
  const pollIntervalMs = Math.max(1, args.pollIntervalMs);
  const maxAttempts = Math.max(
    1,
    Math.floor(args.timeoutMs / pollIntervalMs) + 1
  );
  const startedAt = now();
  let dryRun = args.dryRun;
  let coverage = readTopUpLookupTableCoverage(dryRun, args.reserve);
  let attempts = 1;

  while (coverage.status === "pending") {
    const waitedMs = now() - startedAt;
    if (attempts >= maxAttempts || waitedMs + pollIntervalMs > args.timeoutMs) {
      return { status: "timed_out", attempts, waitedMs, coverage, dryRun };
    }
    await sleep(pollIntervalMs);
    dryRun = await args.refreshDryRun();
    attempts += 1;
    coverage = readTopUpLookupTableCoverage(dryRun, args.reserve);
  }

  return {
    status: coverage.status,
    attempts,
    waitedMs: now() - startedAt,
    coverage,
    dryRun,
  };
}

/**
 * The pull moves user funds, so anything short of a confirmed-ready resolution has to
 * stop here: a stalled provisioner, a route the reusable resolver rejects, and an
 * unreadable resolution all mean the top-up leg cannot be trusted to land.
 */
export function assertLookupTableReadinessBeforePull(
  readiness: TopUpLookupTableReadiness
): void {
  if (readiness.status === "ready") {
    return;
  }
  // The dry-run summary carries `preflightBlockers`, which is where a failed policy
  // plan records why. Serializing only the coverage hides that reason completely and
  // leaves the alert pointing at the lookup-table subsystem.
  throw new Error(
    `Kamino top-up reusable lookup-table coverage is ${readiness.status} after ` +
      `${readiness.waitedMs}ms across ${readiness.attempts} dry runs; refusing to pull. ` +
      `coverage=${JSON.stringify(readiness.coverage)} ` +
      `topUp=${JSON.stringify(summarizeTopUpResult(readiness.dryRun))}`
  );
}

export type MissingObligationRecovery = {
  status:
    | "not_needed"
    | "dry_run_ready"
    | "executed"
    | "concurrently_completed";
  missingObligation: { obligation: string; reserve: string } | null;
  setupDryRun: SameMintTopUpResult | null;
  setupExecution: SameMintTopUpResult | null;
  setupReadiness: TopUpLookupTableReadiness | null;
};

export async function recoverMissingObligationBeforePull(args: {
  dryRun: SameMintTopUpResult;
  execute: boolean;
  reserve: string;
  pollIntervalMs: number;
  timeoutMs: number;
  runSetup: (execute: boolean) => Promise<SameMintTopUpResult>;
  refreshTopUp: () => Promise<SameMintTopUpResult>;
  now?: () => number;
  sleep?: (milliseconds: number) => Promise<void>;
}): Promise<{
  topUpDryRun: SameMintTopUpResult;
  recovery: MissingObligationRecovery;
}> {
  const missingObligation = readMissingDepositObligation(args.dryRun);
  if (!missingObligation) {
    return {
      topUpDryRun: args.dryRun,
      recovery: {
        status: "not_needed",
        missingObligation: null,
        setupDryRun: null,
        setupExecution: null,
        setupReadiness: null,
      },
    };
  }
  if (missingObligation.reserve !== args.reserve) {
    throw new Error(
      `Kamino missing-obligation reserve ${missingObligation.reserve} does not match selected top-up reserve ${args.reserve}; refusing to pull.`
    );
  }

  const setupDryRun = await args.runSetup(false);
  const setupDryRunStatus = setupDryRun.json?.status?.toString() ?? null;
  if (setupDryRunStatus === "setup_obligation_reserve_skipped_existing") {
    const refreshedTopUp = await requireRecoveredTopUp(args.refreshTopUp);
    return {
      topUpDryRun: refreshedTopUp,
      recovery: {
        status: "concurrently_completed",
        missingObligation,
        setupDryRun,
        setupExecution: null,
        setupReadiness: null,
      },
    };
  }
  requireSetupDryRun(setupDryRun, missingObligation);

  const setupReadiness = await awaitTopUpLookupTableReadiness({
    dryRun: setupDryRun,
    refreshDryRun: () => args.runSetup(false),
    reserve: args.reserve,
    pollIntervalMs: args.pollIntervalMs,
    timeoutMs: args.timeoutMs,
    ...(args.now ? { now: args.now } : {}),
    ...(args.sleep ? { sleep: args.sleep } : {}),
  });
  if (
    setupReadiness.dryRun.json?.status ===
    "setup_obligation_reserve_skipped_existing"
  ) {
    const refreshedTopUp = await requireRecoveredTopUp(args.refreshTopUp);
    return {
      topUpDryRun: refreshedTopUp,
      recovery: {
        status: "concurrently_completed",
        missingObligation,
        setupDryRun: setupReadiness.dryRun,
        setupExecution: null,
        setupReadiness,
      },
    };
  }
  assertLookupTableReadinessBeforePull(setupReadiness);

  if (!args.execute) {
    return {
      topUpDryRun: args.dryRun,
      recovery: {
        status: "dry_run_ready",
        missingObligation,
        setupDryRun,
        setupExecution: null,
        setupReadiness,
      },
    };
  }

  let setupExecution: SameMintTopUpResult;
  try {
    setupExecution = await args.runSetup(true);
  } catch (setupError) {
    // Another executor may have won the setup race. Continue only if a fresh
    // deposit dry-run independently proves the obligation now exists and the
    // route plan builds; otherwise preserve the original setup failure.
    try {
      const refreshedTopUp = await requireRecoveredTopUp(args.refreshTopUp);
      return {
        topUpDryRun: refreshedTopUp,
        recovery: {
          status: "concurrently_completed",
          missingObligation,
          setupDryRun,
          setupExecution: null,
          setupReadiness,
        },
      };
    } catch {
      throw setupError;
    }
  }

  const setupStatus = setupExecution.json?.status?.toString() ?? null;
  const status =
    setupStatus === "setup_obligation_reserve_skipped_existing"
      ? "concurrently_completed"
      : "executed";
  if (status === "executed") {
    requireSetupExecution(setupExecution, missingObligation);
  }
  const refreshedTopUp = await requireRecoveredTopUp(args.refreshTopUp);
  return {
    topUpDryRun: refreshedTopUp,
    recovery: {
      status,
      missingObligation,
      setupDryRun,
      setupExecution,
      setupReadiness,
    },
  };
}

function requireSetupDryRun(
  result: SameMintTopUpResult,
  expected: { obligation: string; reserve: string }
): void {
  if (result.json?.status !== "setup_obligation_reserve_dry_run") {
    throw new Error(
      `Kamino obligation setup did not report a dry run: ${JSON.stringify(
        summarizeTopUpResult(result)
      )}`
    );
  }
  if (result.json.sendsTransactions !== false) {
    throw new Error("Kamino obligation setup dry run claimed it sends transactions.");
  }
  const target = readRecord(result.json.target);
  if (
    target?.reserve?.toString() !== expected.reserve ||
    target?.obligation?.toString() !== expected.obligation ||
    target?.obligationExists !== false
  ) {
    throw new Error(
      `Kamino obligation setup dry run target does not match the blocked top-up: ${JSON.stringify(
        target
      )}`
    );
  }
  const missingSetup = readRecord(result.json.missingObligationSetup);
  const initExecution = readRecord(missingSetup?.initExecution);
  if (initExecution?.simulationError != null) {
    throw new Error(
      `Kamino obligation setup dry-run simulation failed: ${initExecution.simulationError}`
    );
  }
}

function requireSetupExecution(
  result: SameMintTopUpResult,
  expected: { obligation: string; reserve: string }
): void {
  if (
    result.json?.status !== "setup_obligation_reserve_executed" ||
    result.json.sendsTransactions !== true
  ) {
    throw new Error(
      `Kamino obligation setup did not report execution: ${JSON.stringify(
        summarizeTopUpResult(result)
      )}`
    );
  }
  const target = readRecord(result.json.target);
  if (
    target?.reserve?.toString() !== expected.reserve ||
    target?.obligation?.toString() !== expected.obligation ||
    target?.obligationExists !== true
  ) {
    throw new Error(
      `Kamino obligation setup did not prove the expected account exists: ${JSON.stringify(
        target
      )}`
    );
  }
  const setup = readRecord(result.json.setup);
  const initExecution = readRecord(setup?.initExecution);
  if (
    !initExecution?.signature?.toString() ||
    !initExecution.confirmedSlot?.toString()
  ) {
    throw new Error(
      "Kamino obligation setup result is missing its signature or confirmed slot."
    );
  }
}

async function requireRecoveredTopUp(
  refreshTopUp: () => Promise<SameMintTopUpResult>
): Promise<SameMintTopUpResult> {
  const refreshedTopUp = await refreshTopUp();
  const stillMissing = readMissingDepositObligation(refreshedTopUp);
  if (stillMissing) {
    throw new Error(
      `Kamino deposit obligation ${stillMissing.obligation} is still missing after setup; refusing to pull.`
    );
  }
  assertNoTopUpPreflightBlockers(refreshedTopUp);
  return refreshedTopUp;
}

const TOP_UP_PREFLIGHT_BLOCKED_MARKER =
  "Kamino top-up dry run reported preflight blockers";

/**
 * The only blocker the pull clears. `funding_needed_raw` is computed against the vault
 * balance, so once the pull lands the shortfall is zero and this blocker disappears on
 * the execute pass.
 *
 * Every other funding blocker survives the pull. A missing funding-wallet ATA is the
 * dangerous one: the pull funds the vault ATA, never that wallet's, so the blocker is
 * still there when the execute leg re-runs preflight -- and the binary rejects any
 * non-empty blocker set before submitting. That failure lands after user funds have
 * already moved, stranding them in the vault ATA. Matching exactly means an unexpected
 * blocker fails closed.
 */
const PULL_RESOLVED_BLOCKER_PATTERN =
  /^wallet USDC balance \d+ is below needed funding amount \d+$/;

export function isPullResolvedTopUpBlocker(blocker: string): boolean {
  return PULL_RESOLVED_BLOCKER_PATTERN.test(blocker.trim());
}

export function isTopUpPreflightBlockedFailure(error: unknown): boolean {
  const message = error instanceof Error ? error.message : String(error);
  return message.includes(TOP_UP_PREFLIGHT_BLOCKED_MARKER);
}

/**
 * A blocked route cannot be waited out, so it must not reach the readiness gate: that
 * gate can only describe lookup-table coverage, and would report a route fault as an
 * ALT fault. Failing here keeps the blocker text attached to the failure.
 *
 * Blockers on their own are not fatal. The initial-deposit mode this flow borrows models
 * a wallet-funded deposit, so its wallet-funding leg is always unmet here: the vault is
 * funded by the pull, which has not run at dry-run time. The coverage gate already
 * accepts that as `funding_deferred`. What is unrecoverable is a policy plan that never
 * built, and the binary only omits the lookup-table resolution in exactly that case, so
 * the missing resolution -- not the presence of blockers -- is the signal to stop.
 */
export function assertNoTopUpPreflightBlockers(
  dryRun: SameMintTopUpResult
): void {
  const blockers = readTopUpPreflightBlockers(dryRun);
  if (blockers.length === 0) {
    return;
  }
  const unresolvableBlockers = blockers.filter(
    (blocker) => !isPullResolvedTopUpBlocker(blocker)
  );
  if (
    unresolvableBlockers.length === 0 &&
    readRecord(dryRun.json?.lookupTableResolution)
  ) {
    return;
  }
  const missingObligation = readMissingDepositObligation(dryRun);
  throw new Error(
    `${TOP_UP_PREFLIGHT_BLOCKED_MARKER}; refusing to pull. ` +
      `blockers=${JSON.stringify(blockers)} ` +
      `unresolvableBlockers=${JSON.stringify(unresolvableBlockers)} ` +
      `missingDepositObligation=${JSON.stringify(missingObligation)} ` +
      `topUp=${JSON.stringify(summarizeTopUpResult(dryRun))}`
  );
}

const TOP_UP_LOOKUP_TABLE_COVERAGE_MARKERS = [
  "initial reserve deposit ALT coverage is incomplete before wallet funding",
  "reusable lookup-table coverage is incomplete or the exact simulation failure",
  "reusable lookup-table coverage/packet/catalog gate failed before prerequisite transaction",
  "reusable-only runtime requires complete reusable ALT coverage and simulation",
] as const;

export function isLookupTableCoverageTopUpFailure(error: unknown): boolean {
  const message = error instanceof Error ? error.message : String(error);
  return TOP_UP_LOOKUP_TABLE_COVERAGE_MARKERS.some((marker) =>
    message.includes(marker)
  );
}

export function classifyTopUpFailure(error: unknown): string {
  const message = error instanceof Error ? error.message : String(error);
  if (isLookupTableCoverageTopUpFailure(error)) {
    return "alt_coverage_pending";
  }
  if (message.includes("unable to confirm transaction")) {
    return "confirm_timeout";
  }
  if (message.includes("BlockhashNotFound")) {
    return "blockhash_not_found";
  }
  if (message.includes("lookup-table usage lease races")) {
    return "lookup_table_lease_race";
  }
  if (message.includes("preflight blocked")) {
    return "preflight_blocked";
  }
  return "unclassified";
}

export async function runTopUpWithLookupTableRetry(args: {
  attempt: (context: {
    attempt: number;
    executionId: string;
    amountRaw: bigint;
  }) => Promise<SameMintTopUpResult>;
  attempts: number;
  executionId: string;
  amountRaw: bigint;
  delayMs: number;
  onRetry?: (info: {
    attempt: number;
    executionId: string;
    amountRaw: bigint;
    error: unknown;
  }) => void;
  sleep?: (milliseconds: number) => Promise<void>;
}): Promise<SameMintTopUpResult> {
  const sleep = args.sleep ?? defaultSleep;
  const maxAttempts = Math.max(1, args.attempts);
  let lastError: unknown;

  for (let attempt = 1; attempt <= maxAttempts; attempt += 1) {
    try {
      return await args.attempt({
        attempt,
        executionId: args.executionId,
        amountRaw: args.amountRaw,
      });
    } catch (error) {
      lastError = error;
      if (attempt >= maxAttempts || !isLookupTableCoverageTopUpFailure(error)) {
        throw error;
      }
      args.onRetry?.({
        attempt,
        executionId: args.executionId,
        amountRaw: args.amountRaw,
        error,
      });
      await sleep(args.delayMs);
    }
  }

  throw lastError;
}

function defaultSleep(milliseconds: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, milliseconds));
}

function readEnvInteger(name: string, fallback: number): number {
  const value = process.env[name]?.trim();
  if (!value || !/^\d+$/.test(value)) {
    return fallback;
  }
  return Number(value);
}

export async function assertSolBalance(args: {
  connection: Pick<Connection, "getBalance">;
  feePayer: PublicKey;
  minimumLamports: number;
  role: string;
}): Promise<void> {
  const balanceLamports = await args.connection.getBalance(
    args.feePayer,
    DEFAULT_COMMITMENT
  );
  if (balanceLamports < args.minimumLamports) {
    throw new Error(
      `${
        args.role
      } ${args.feePayer.toBase58()} has ${balanceLamports} lamports; ` +
        `${args.minimumLamports} required.`
    );
  }
  reportFeePayerBalance({
    balanceLamports,
    feePayer: args.feePayer.toBase58(),
    minimumLamports: args.minimumLamports,
    role: args.role,
  });
}

export async function assertFeePayerSol(args: {
  connection: Pick<Connection, "getBalance">;
  feePayer: PublicKey;
}): Promise<TopUpFeePayerSolSafety> {
  const balanceLamports = await args.connection.getBalance(
    args.feePayer,
    DEFAULT_COMMITMENT
  );
  if (balanceLamports < AUTODEPOSIT_TOP_UP_FEE_PAYER_MIN_LAMPORTS) {
    throw new Error(
      `Kamino top-up fee payer ${args.feePayer.toBase58()} has ${balanceLamports} lamports; ` +
        `${AUTODEPOSIT_TOP_UP_FEE_PAYER_MIN_LAMPORTS} required. Refusing to pull user funds.`
    );
  }
  reportFeePayerBalance({
    balanceLamports,
    feePayer: args.feePayer.toBase58(),
    minimumLamports: AUTODEPOSIT_TOP_UP_FEE_PAYER_MIN_LAMPORTS,
    role: "Kamino top-up fee payer",
  });
  return {
    feePayer: args.feePayer.toBase58(),
    balanceLamports,
    minimumLamports: AUTODEPOSIT_TOP_UP_FEE_PAYER_MIN_LAMPORTS,
    commitment: DEFAULT_COMMITMENT,
    checked: true,
  };
}

export async function runAfterFeePayerSolSafety<T>(args: {
  connection: Pick<Connection, "getBalance">;
  feePayer: PublicKey;
  run: () => Promise<T>;
}): Promise<{ result: T; safety: TopUpFeePayerSolSafety }> {
  const safety = await assertFeePayerSol(args);
  return { result: await args.run(), safety };
}

function redactSensitiveText(value: string): string {
  let redacted = value;
  const rpcUrl = process.env.SOLANA_RPC_URL;
  if (rpcUrl) {
    redacted = redacted.split(rpcUrl).join("[redacted SOLANA_RPC_URL]");
  }
  return redacted.replace(/api-key=[^'"\s]+/gi, "api-key=[redacted]");
}

async function notifySolanaWeekSweep(args: {
  PublicKeyCtor: typeof PublicKey;
  ownerWalletAddress: string;
  /** Defaults to "executed" on the app side when omitted. */
  kind?: "executed" | "failed";
  amountRaw?: bigint | null;
  /** At-most-once key for the app's push sent-log; one push per sweep. */
  dedupeKey?: string | null;
}): Promise<SolanaWeekNotifyResult> {
  const endpoint = process.env[SOLANA_WEEK_NOTIFY_ENDPOINT_ENV]?.trim();
  const secret = process.env[SOLANA_WEEK_NOTIFY_SECRET_ENV]?.trim();
  if (!endpoint) {
    return { status: "skipped", reason: "missing_endpoint" };
  }
  if (!secret) {
    return { status: "skipped", reason: "missing_secret" };
  }

  const abortController = new AbortController();
  const timeout = setTimeout(() => {
    abortController.abort();
  }, SOLANA_WEEK_NOTIFY_TIMEOUT_MS);
  const walletAddress = new args.PublicKeyCtor(
    args.ownerWalletAddress
  ).toBase58();

  try {
    const response = await fetch(endpoint, {
      method: "POST",
      headers: {
        Authorization: `Bearer ${secret}`,
        "Content-Type": "application/json",
      },
      signal: abortController.signal,
      body: JSON.stringify({
        walletAddress,
        ...(args.kind === "failed"
          ? {
              kind: "failed",
              ...(args.amountRaw !== null && args.amountRaw !== undefined
                ? { amountRaw: args.amountRaw.toString() }
                : {}),
              ...(args.dedupeKey ? { dedupeKey: args.dedupeKey } : {}),
            }
          : {}),
      }),
    });
    if (!response.ok) {
      const body = await response.text();
      return {
        status: "failed",
        httpStatus: response.status,
        error: body.trim().slice(0, 200) || response.statusText,
      };
    }
    return { status: "sent", httpStatus: response.status };
  } catch (error) {
    return {
      status: "failed",
      httpStatus: null,
      error: abortController.signal.aborted
        ? `notification timed out after ${SOLANA_WEEK_NOTIFY_TIMEOUT_MS}ms`
        : error instanceof Error
        ? error.message
        : String(error),
    };
  } finally {
    clearTimeout(timeout);
  }
}

/**
 * Report a scheduled sweep that did not land, so the app can push the user.
 *
 * Skipped when the run is not executing a scheduled slot: the "about to move"
 * push is sent when a slot is scheduled, so without one there is no promise to
 * break. The slot id doubles as the app-side at-most-once key, which is what
 * keeps the hourly retries of one stuck sweep down to a single push.
 */
async function notifyFailedSweep(args: {
  PublicKeyCtor: typeof PublicKey;
  amountRaw: bigint | null;
  ownerWalletAddress: string;
  scheduledSlotId: bigint | null;
}): Promise<SolanaWeekNotifyResult> {
  if (args.scheduledSlotId === null) {
    return { status: "skipped", reason: "no_scheduled_slot" };
  }
  return notifySolanaWeekSweep({
    PublicKeyCtor: args.PublicKeyCtor,
    amountRaw: args.amountRaw,
    dedupeKey: `slot-${args.scheduledSlotId.toString()}`,
    kind: "failed",
    ownerWalletAddress: args.ownerWalletAddress,
  });
}

function logSolanaWeekNotifyResult(result: SolanaWeekNotifyResult) {
  const level = result.status === "sent" ? "info" : "warn";
  console.warn(
    JSON.stringify({
      event: "solana_week_sweep_notify",
      level,
      ...result,
    })
  );
}

function readRecord(value: unknown): Record<string, unknown> | null {
  return value && typeof value === "object" && !Array.isArray(value)
    ? (value as Record<string, unknown>)
    : null;
}

function tailLines(value: string, count: number): string[] {
  return value.trim().split(/\r?\n/).filter(Boolean).slice(-count);
}

export async function preflightDurableKaminoDeposit(args: {
  amountRaw: bigint;
  execute: boolean;
  defaultMarket: string;
  defaultReserve: string;
  defaultLiquidityMint: string;
  rpcUrl: string;
  target: EligibleTarget;
}) {
  const reserve = args.target.currentReserve ?? args.defaultReserve;
  const market = args.target.currentMarket ?? args.defaultMarket;
  const liquidityMint =
    args.target.currentLiquidityMint ?? args.defaultLiquidityMint;
  if (liquidityMint !== args.target.tokenMint) {
    throw new Error(
      `Autodeposit top-up mint ${liquidityMint} does not match pulled mint ${args.target.tokenMint}.`
    );
  }
  const refreshTopUp = () =>
    prepareSameMintReserveTopUp({
      amountRaw: args.amountRaw,
      reserve,
      rpcUrl: args.rpcUrl,
      target: args.target,
    });
  const initialDryRun = await refreshTopUp();
  const recovered = await recoverMissingObligationBeforePull({
    dryRun: initialDryRun,
    execute: args.execute,
    reserve,
    pollIntervalMs: readEnvInteger(
      AUTODEPOSIT_ALT_READINESS_POLL_INTERVAL_MS_ENV,
      AUTODEPOSIT_ALT_READINESS_POLL_INTERVAL_MS
    ),
    timeoutMs: readEnvInteger(
      AUTODEPOSIT_ALT_READINESS_TIMEOUT_MS_ENV,
      AUTODEPOSIT_ALT_READINESS_TIMEOUT_MS
    ),
    runSetup: (execute) =>
      runMissingObligationSetup({
        execute,
        reserve,
        rpcUrl: args.rpcUrl,
        target: args.target,
      }),
    refreshTopUp,
  });
  assertNoTopUpPreflightBlockers(recovered.topUpDryRun);
  const policyDepositTransaction = readRecord(
    recovered.topUpDryRun.json?.policyDepositTransaction
  );
  if (policyDepositTransaction?.simulationError != null) {
    throw new Error(
      `Kamino top-up dry-run simulation failed; refusing to pull. topUp=${JSON.stringify(
        summarizeTopUpResult(recovered.topUpDryRun)
      )}`
    );
  }
  const readiness = await awaitTopUpLookupTableReadiness({
    dryRun: recovered.topUpDryRun,
    refreshDryRun: refreshTopUp,
    reserve,
    pollIntervalMs: readEnvInteger(
      AUTODEPOSIT_ALT_READINESS_POLL_INTERVAL_MS_ENV,
      AUTODEPOSIT_ALT_READINESS_POLL_INTERVAL_MS
    ),
    timeoutMs: readEnvInteger(
      AUTODEPOSIT_ALT_READINESS_TIMEOUT_MS_ENV,
      AUTODEPOSIT_ALT_READINESS_TIMEOUT_MS
    ),
  });
  assertLookupTableReadinessBeforePull(readiness);
  return {
    reserve,
    market,
    liquidityMint,
    dryRun: readiness.dryRun,
    evidence: {
      topUp: summarizeTopUpResult(readiness.dryRun),
      lookupTableCoverage: readiness.coverage,
      missingObligationRecovery: recovered.recovery.status,
    },
  };
}

async function recordPullExecution(args: {
  neon: AppModules["neon"];
  databaseUrl: string;
  target: ConfirmedPullHandoffTarget;
  signature: string;
  slot: bigint;
  amountRaw: bigint;
  sourcePreBalanceRaw: bigint;
  sourcePostBalanceRaw: bigint;
  destinationPreBalanceRaw: bigint;
  destinationPostBalanceRaw: bigint;
}): Promise<{ dedupeKey: string; executionId: string }> {
  const sql = args.neon(args.databaseUrl);
  const dedupeKey = `${args.target.id.toString()}:autodeposit-pull:${
    args.signature
  }`;
  const rows = await sql`
    WITH inserted AS (
      INSERT INTO loyal_yield.balance_sweep_executions (
        target_id,
        signature,
        slot,
        source_wallet_ata,
        destination_vault_ata,
        token_mint,
        source_token_ata,
        destination_token_ata,
        amount_raw,
        source_pre_balance_raw,
        source_post_balance_raw,
        destination_pre_balance_raw,
        destination_post_balance_raw,
        source_commitment,
        raw_evidence,
        decoded_evidence,
        received_at,
        decoded_at,
        dedupe_key
      )
      VALUES (
        ${args.target.id.toString()},
        ${args.signature},
        ${args.slot.toString()},
        ${args.target.walletUsdcAta},
        ${args.target.vaultUsdcAta},
        ${args.target.tokenMint},
        ${args.target.walletTokenAta},
        ${args.target.vaultTokenAta},
        ${args.amountRaw.toString()},
        ${args.sourcePreBalanceRaw.toString()},
        ${args.sourcePostBalanceRaw.toString()},
        ${args.destinationPreBalanceRaw.toString()},
        ${args.destinationPostBalanceRaw.toString()},
        'confirmed',
        ${JSON.stringify({
          source: "single-vault-autodeposit-executor",
        })}::jsonb,
        ${JSON.stringify({
          sequence: "subscription_pull_then_mandatory_kamino_deposit",
        })}::jsonb,
        now(),
        now(),
        ${dedupeKey}
      )
      ON CONFLICT (dedupe_key) DO NOTHING
      RETURNING id
    ),
    existing AS (
      SELECT id
      FROM loyal_yield.balance_sweep_executions
      WHERE dedupe_key = ${dedupeKey}
    )
    SELECT id FROM inserted
    UNION ALL
    SELECT id FROM existing
    LIMIT 1
  `;
  const row = rows[0] as Record<string, unknown> | undefined;
  return {
    dedupeKey,
    executionId: readRequiredString(row?.id, "balance_sweep_execution.id"),
  };
}

async function completeAutodepositClaim(args: {
  claimToken: string;
  databaseUrl: string;
  executionId: string;
  leaseToken: string;
  neon: AppModules["neon"];
  plan: AutodepositDepositPlan;
  postConfirmPositionAmountRaw: bigint;
  postConfirmObservedSlot: bigint;
  scheduledSlotId: bigint;
}) {
  const sql = args.neon(args.databaseUrl);
  const rows = await sql`
    WITH owned_claim AS MATERIALIZED (
      SELECT claim_token
      FROM loyal_yield.balance_sweep_lot_claims
      WHERE claim_token = ${args.claimToken}
        AND status = 'selected'
        AND autodeposit_executor_lease_token = ${args.leaseToken}
        AND autodeposit_executor_lease_expires_at > now()
      FOR UPDATE
    ),
    confirmed_top_up AS (
      SELECT attempt.signature, attempt.confirmed_slot
      FROM loyal_yield.balance_sweep_transaction_attempts AS attempt
      JOIN owned_claim ON TRUE
      WHERE attempt.claim_token = ${args.claimToken}
        AND attempt.execution_id = ${args.executionId}
        AND attempt.operation_kind = 'top_up'
        AND attempt.attempt_state = 'confirmed'
        AND attempt.confirmed_slot IS NOT NULL
      ORDER BY attempt.attempt_number DESC
      LIMIT 1
    ),
    existing_position AS MATERIALIZED (
      SELECT
        position.id, position.current_amount_raw, position.principal_amount_raw,
        position.current_reserve, position.current_liquidity_mint
      FROM loyal_yield.user_yield_positions AS position
      JOIN owned_claim ON TRUE
      WHERE position.settings = ${args.plan.target.settings}
        AND position.vault_index = ${args.plan.target.vaultIndex}
        AND position.wallet_address = ${args.plan.target.wallet}
        AND position.status = 'active'
      ORDER BY position.updated_at DESC, position.id DESC
      LIMIT 1
      FOR UPDATE OF position
    ),
    inserted_deposit AS (
      INSERT INTO loyal_yield.user_yield_position_deposits (
        deposit_signature, policy_signature, confirmed_slot,
        wallet_address, smart_account_address, settings, vault_index,
        vault_pubkey, policy_id, policy_account, policy_seed,
        target_reserve, market, liquidity_mint, target_supply_apy_bps,
        deposit_mint, principal_amount_raw, balance_sweep_execution_id,
        balance_sweep_scheduled_slot_id, confirmed_at, created_at
      )
      SELECT
        confirmed_top_up.signature, confirmed_top_up.signature,
        confirmed_top_up.confirmed_slot, ${args.plan.target.wallet},
        ${args.plan.target.vaultPubkey}, ${args.plan.target.settings},
        ${args.plan.target.vaultIndex}, ${args.plan.target.vaultPubkey},
        ${args.plan.target.routePolicySeed.toString()},
        ${args.plan.target.routePolicyAccount},
        ${args.plan.target.routePolicySeed.toString()}, ${args.plan.reserve},
        ${args.plan.market}, ${args.plan.liquidityMint}, NULL,
        ${args.plan.liquidityMint}, ${args.plan.amountRaw.toString()},
        ${args.executionId}, ${args.scheduledSlotId.toString()}, now(), now()
      FROM confirmed_top_up
      ON CONFLICT (deposit_signature) DO NOTHING
      RETURNING id, deposit_signature
    ),
    linked_existing_deposit AS (
      UPDATE loyal_yield.user_yield_position_deposits AS deposit
      SET
        balance_sweep_execution_id = COALESCE(
          deposit.balance_sweep_execution_id,
          ${args.executionId}
        ),
        balance_sweep_scheduled_slot_id = COALESCE(
          deposit.balance_sweep_scheduled_slot_id,
          ${args.scheduledSlotId.toString()}
        )
      FROM confirmed_top_up
      WHERE deposit.deposit_signature = confirmed_top_up.signature
        AND NOT EXISTS (SELECT 1 FROM inserted_deposit)
        AND (
          deposit.balance_sweep_execution_id IS NULL
          OR deposit.balance_sweep_execution_id = ${args.executionId}
        )
      RETURNING deposit.id, deposit.deposit_signature
    ),
    completed_deposit AS (
      SELECT id, deposit_signature, TRUE AS inserted FROM inserted_deposit
      UNION ALL
      SELECT id, deposit_signature, FALSE AS inserted
      FROM linked_existing_deposit
    ),
    updated_position AS (
      UPDATE loyal_yield.user_yield_positions AS position
      SET deposit_mint = ${args.plan.liquidityMint},
          initial_liquidity_mint = ${args.plan.liquidityMint},
          initial_market = ${args.plan.market},
          last_confirmed_slot = confirmed_top_up.confirmed_slot,
          last_deposit_signature = confirmed_top_up.signature,
          policy_account = ${args.plan.target.routePolicyAccount},
          policy_id = ${args.plan.target.routePolicySeed.toString()},
          policy_seed = ${args.plan.target.routePolicySeed.toString()},
          principal_amount_raw = position.principal_amount_raw +
            CASE WHEN completed_deposit.inserted
              THEN ${args.plan.amountRaw.toString()}::bigint ELSE 0 END,
          smart_account_address = ${args.plan.target.vaultPubkey},
          vault_pubkey = ${args.plan.target.vaultPubkey},
          wallet_address = ${args.plan.target.wallet},
          current_reserve = ${args.plan.reserve},
          current_market = ${args.plan.market},
          current_liquidity_mint = ${args.plan.liquidityMint},
          current_amount_raw = ${args.postConfirmPositionAmountRaw.toString()},
          current_observed_slot = ${args.postConfirmObservedSlot.toString()},
          current_observed_at = now(),
          status = 'active',
          updated_at = now()
      FROM existing_position, confirmed_top_up, completed_deposit
      WHERE position.id = existing_position.id
      RETURNING
        position.id,
        'deposit_top_up'::text AS event_type,
        CASE
          WHEN existing_position.current_reserve = ${args.plan.reserve}
           AND existing_position.current_liquidity_mint = ${args.plan.liquidityMint}
          THEN ${args.postConfirmPositionAmountRaw.toString()}::bigint -
            existing_position.current_amount_raw
          ELSE NULL
        END AS holding_delta_raw
    ),
    inserted_position AS (
      INSERT INTO loyal_yield.user_yield_positions (
        wallet_address, smart_account_address, settings, vault_index,
        vault_pubkey, policy_id, policy_account, policy_seed,
        initial_reserve, initial_market, initial_liquidity_mint,
        initial_supply_apy_bps, deposit_mint, principal_amount_raw,
        current_reserve, current_market, current_liquidity_mint,
        current_amount_raw, current_observed_slot, current_observed_at,
        first_deposit_signature, last_deposit_signature,
        last_confirmed_slot, status, created_at, updated_at
      )
      SELECT
        ${args.plan.target.wallet}, ${args.plan.target.vaultPubkey},
        ${args.plan.target.settings}, ${args.plan.target.vaultIndex},
        ${args.plan.target.vaultPubkey},
        ${args.plan.target.routePolicySeed.toString()},
        ${args.plan.target.routePolicyAccount},
        ${args.plan.target.routePolicySeed.toString()}, ${args.plan.reserve},
        ${args.plan.market}, ${args.plan.liquidityMint}, NULL,
        ${args.plan.liquidityMint}, ${args.plan.amountRaw.toString()},
        ${args.plan.reserve}, ${args.plan.market}, ${args.plan.liquidityMint},
        ${args.postConfirmPositionAmountRaw.toString()},
        ${args.postConfirmObservedSlot.toString()}, now(),
        confirmed_top_up.signature, confirmed_top_up.signature,
        confirmed_top_up.confirmed_slot, 'active', now(), now()
      FROM confirmed_top_up, completed_deposit
      WHERE NOT EXISTS (SELECT 1 FROM existing_position)
      RETURNING
        id,
        'deposit_initialized'::text AS event_type,
        ${args.postConfirmPositionAmountRaw.toString()}::bigint AS holding_delta_raw
    ),
    completed_position AS (
      SELECT * FROM updated_position
      UNION ALL
      SELECT * FROM inserted_position
    ),
    inserted_holding_event AS (
      INSERT INTO loyal_yield.user_yield_position_holding_events (
        position_id, event_type, reserve, market, liquidity_mint,
        amount_raw, principal_delta_raw, holding_delta_raw,
        observed_slot, observed_at, source_signature,
        source_deposit_id, created_at
      )
      SELECT
        completed_position.id,
        completed_position.event_type::loyal_yield.user_yield_holding_event_type,
        ${args.plan.reserve}, ${args.plan.market}, ${args.plan.liquidityMint},
        ${args.postConfirmPositionAmountRaw.toString()},
        CASE WHEN completed_deposit.inserted
          THEN ${args.plan.amountRaw.toString()}::bigint ELSE 0 END,
        completed_position.holding_delta_raw,
        ${args.postConfirmObservedSlot.toString()}, now(),
        confirmed_top_up.signature, completed_deposit.id, now()
      FROM completed_position, confirmed_top_up, completed_deposit
      WHERE NOT EXISTS (
        SELECT 1
        FROM loyal_yield.user_yield_position_holding_events AS existing_event
        WHERE existing_event.source_signature = confirmed_top_up.signature
      )
      RETURNING id, position_id
    ),
    completed_holding_event AS (
      SELECT id, position_id FROM inserted_holding_event
      UNION ALL
      SELECT existing_event.id, existing_event.position_id
      FROM loyal_yield.user_yield_position_holding_events AS existing_event
      JOIN confirmed_top_up
        ON existing_event.source_signature = confirmed_top_up.signature
      WHERE NOT EXISTS (SELECT 1 FROM inserted_holding_event)
      LIMIT 1
    ),
    finalized_position AS (
      UPDATE loyal_yield.user_yield_positions AS position
      SET last_holding_event_id = completed_holding_event.id,
          updated_at = now()
      FROM completed_holding_event
      WHERE position.id = completed_holding_event.position_id
      RETURNING position.id
    ),
    completed_execution AS (
      UPDATE loyal_yield.balance_sweep_executions
      SET kamino_deposit_signature = confirmed_top_up.signature,
          completed_at = now(),
          completion_failure_code = NULL,
          decoded_evidence = COALESCE(decoded_evidence, '{}'::jsonb) ||
            jsonb_build_object(
              'status', 'executed',
              'kaminoDepositSignature', confirmed_top_up.signature,
              'kaminoDepositSlot', confirmed_top_up.confirmed_slot::text
            ),
          decoded_at = now()
      FROM confirmed_top_up
      WHERE id = ${args.executionId}
        AND EXISTS (SELECT 1 FROM completed_deposit)
        AND EXISTS (SELECT 1 FROM finalized_position)
        AND EXISTS (
          SELECT 1
          FROM loyal_yield.balance_sweep_lot_claims AS claim
          WHERE claim.claim_token = ${args.claimToken}
            AND claim.status = 'selected'
            AND claim.autodeposit_executor_lease_token = ${args.leaseToken}
            AND claim.autodeposit_executor_lease_expires_at > now()
        )
      RETURNING id
    ),
    matched_lots AS (
      SELECT item.lot_id, item.amount_raw
      FROM loyal_yield.balance_sweep_lot_claim_items AS item
      WHERE item.claim_token = ${args.claimToken}
    ),
    inserted_lots AS (
      INSERT INTO loyal_yield.balance_sweep_execution_lots
        (execution_id, lot_id, amount_raw)
      SELECT ${args.executionId}, lot_id, amount_raw
      FROM matched_lots
      WHERE EXISTS (SELECT 1 FROM completed_execution)
      ON CONFLICT (execution_id, lot_id) DO NOTHING
    ),
    completed_claim AS (
      UPDATE loyal_yield.balance_sweep_lot_claims
      SET status = 'executed',
          execution_id = ${args.executionId},
          autodeposit_executor_lease_token = NULL,
          autodeposit_executor_lease_expires_at = NULL,
          updated_at = now()
      WHERE claim_token = ${args.claimToken}
        AND status = 'selected'
        AND autodeposit_executor_lease_token = ${args.leaseToken}
        AND autodeposit_executor_lease_expires_at > now()
        AND EXISTS (SELECT 1 FROM completed_execution)
      RETURNING claim_token
    ),
    completed_slot AS (
      UPDATE loyal_yield.balance_sweep_scheduled_slots
      SET status = 'executed', execution_id = ${args.executionId}, updated_at = now()
      WHERE claim_token IN (SELECT claim_token FROM completed_claim)
      RETURNING id
    )
    SELECT
      EXISTS (SELECT 1 FROM completed_deposit) AS deposit_completed,
      EXISTS (SELECT 1 FROM finalized_position) AS position_completed,
      EXISTS (SELECT 1 FROM completed_execution) AS execution_completed,
      EXISTS (SELECT 1 FROM completed_claim) AS claim_completed,
      EXISTS (SELECT 1 FROM completed_slot) AS slot_completed
  `;
  const row = rows[0] as Record<string, unknown> | undefined;
  if (
    row?.deposit_completed !== true ||
    row.position_completed !== true ||
    row.execution_completed !== true ||
    row.claim_completed !== true ||
    row.slot_completed !== true
  ) {
    throw new Error(
      `Autodeposit claim ${args.claimToken} did not complete atomically.`
    );
  }
}

async function resumeDirectKaminoDeposit(args: {
  attempt: DurableAutodepositAttempt;
  claimToken: string;
  connection: Connection;
  databaseUrl: string;
  leaseToken: string;
  neon: AppModules["neon"];
  plan: AutodepositDepositPlan;
  rpcUrl: string;
  scheduledSlotId: bigint;
  target: DurableAutodepositTarget & { managedVaultId: bigint };
}) {
  if (args.attempt.confirmedSlot === null) {
    throw new Error(
      `Confirmed autodeposit pull ${args.attempt.id} has no confirmed slot.`
    );
  }
  const walletPostPullRaw = await getTokenBalanceRaw(
    args.connection,
    new PublicKey(args.target.walletUsdcAta)
  );
  const vaultObservation = await getContextFencedTokenBalance({
    connection: args.connection,
    minimumSlot: args.attempt.confirmedSlot,
    tokenAccount: new PublicKey(args.target.vaultUsdcAta),
  });
  await renewAutodepositClaimLease({
    neon: args.neon,
    databaseUrl: args.databaseUrl,
    claimToken: args.claimToken,
    leaseToken: args.leaseToken,
  });
  const executionRecord = await recordPullExecution({
    neon: args.neon,
    databaseUrl: args.databaseUrl,
    target: args.target,
    signature: args.attempt.signature,
    slot: args.attempt.confirmedSlot,
    amountRaw: args.attempt.amountRaw,
    sourcePreBalanceRaw: args.attempt.sourcePreBalanceRaw,
    sourcePostBalanceRaw: walletPostPullRaw,
    destinationPreBalanceRaw: args.attempt.destinationPreBalanceRaw,
    destinationPostBalanceRaw: vaultObservation.amountRaw,
  });
  const existingTopUpAttempt = await loadDurableAutodepositAttempt({
    neon: args.neon,
    databaseUrl: args.databaseUrl,
    claimToken: args.claimToken,
    operationKind: "top_up",
  });
  const topUpRecovery = classifyDirectTopUpRecovery({
    existingAttemptState: existingTopUpAttempt?.state ?? null,
    vaultAmountRaw: vaultObservation.amountRaw,
    plannedAmountRaw: args.plan.amountRaw,
    persistedSourcePreBalanceRaw:
      existingTopUpAttempt?.sourcePreBalanceRaw ?? null,
  });
  if (topUpRecovery === "effect_ambiguous") {
    await releaseAutodepositClaimLease({
      neon: args.neon,
      databaseUrl: args.databaseUrl,
      claimToken: args.claimToken,
      leaseToken: args.leaseToken,
    });
    throw new AutodepositEffectAmbiguousError(
      `Autodeposit deposit effect is ambiguous for claim ${args.claimToken}; refusing to submit another deposit.`
    );
  }
  let topUpSend: DurablePreparedOperationResult;
  try {
    topUpSend = await sendPreparedTopUpOperation({
      connection: args.connection,
      neon: args.neon,
      databaseUrl: args.databaseUrl,
      claimToken: args.claimToken,
      leaseToken: args.leaseToken,
      targetId: args.target.id,
      scheduledSlotId: args.scheduledSlotId,
      executionId: executionRecord.executionId,
      amountRaw: args.plan.amountRaw,
      sourcePreBalanceRaw:
        existingTopUpAttempt?.sourcePreBalanceRaw ?? vaultObservation.amountRaw,
      prepare: () =>
        prepareSameMintReserveTopUp({
          amountRaw: args.plan.amountRaw,
          reserve: args.plan.reserve,
          rpcUrl: args.rpcUrl,
          target: args.target,
        }),
    });
  } catch (error) {
    if (error instanceof AutodepositOwnershipLostError) {
      throw error;
    }
    await releaseAutodepositClaimLease({
      neon: args.neon,
      databaseUrl: args.databaseUrl,
      claimToken: args.claimToken,
      leaseToken: args.leaseToken,
    });
    if (error instanceof AutodepositEffectAmbiguousError) {
      throw error;
    }
    return {
      status: "deposit_pending" as const,
      executionRecord,
      vaultObservation,
      walletPostPullRaw,
      error: error instanceof Error ? error.message : String(error),
    };
  }
  if (topUpSend.status !== "confirmed") {
    await releaseAutodepositClaimLease({
      neon: args.neon,
      databaseUrl: args.databaseUrl,
      claimToken: args.claimToken,
      leaseToken: args.leaseToken,
    });
    throwIfAutodepositAttemptRequiresOperator(topUpSend.attempt);
    return {
      status: "deposit_pending" as const,
      executionRecord,
      vaultObservation,
      walletPostPullRaw,
      error: attemptErrorDetail(topUpSend.error),
    };
  }
  const deposit = {
    signature: topUpSend.signature,
    confirmedSlot: topUpSend.slot,
  };
  const postConfirmPosition = await reconcileDirectDepositPosition({
    reserve: args.plan.reserve,
    rpcUrl: args.rpcUrl,
    target: args.target,
  });
  await completeAutodepositClaim({
    claimToken: args.claimToken,
    databaseUrl: args.databaseUrl,
    executionId: executionRecord.executionId,
    leaseToken: args.leaseToken,
    neon: args.neon,
    plan: args.plan,
    postConfirmPositionAmountRaw: postConfirmPosition.amountRaw,
    postConfirmObservedSlot: postConfirmPosition.observedSlot,
    scheduledSlotId: args.scheduledSlotId,
  });
  return {
    status: "completed" as const,
    deposit,
    executionRecord,
    vaultObservation,
    walletPostPullRaw,
  };
}

async function recoverAutodepositClaim(args: {
  context: AutodepositRecoveryContext;
  compilePreparedOperation: AppModules["compilePreparedOperation"];
  connection: Connection;
  databaseUrl: string;
  neon: AppModules["neon"];
  scheduledSlotId: bigint;
}): Promise<void> {
  const leaseToken = await acquireAutodepositClaimLease({
    neon: args.neon,
    databaseUrl: args.databaseUrl,
    claimToken: args.context.attempt.claimToken,
    targetId: args.context.target.id,
  });
  if (!leaseToken) {
    console.log(
      JSON.stringify({
        status: "autodeposit_deposit_pending",
        recoverySource: "persisted_confirmed_pull",
        targetId: args.context.target.id.toString(),
        scheduledSlotId: args.scheduledSlotId.toString(),
        retryable: true,
        alert: null,
        reason: "claim_owned_by_another_executor",
      })
    );
    return;
  }
  try {
    let pullAttempt = args.context.attempt;
    if (pullAttempt.state !== "confirmed") {
      const pullSend = await sendPreparedOperation({
        compilePreparedOperation: args.compilePreparedOperation,
        connection: args.connection,
        prepared: null,
        signers: [],
        neon: args.neon,
        databaseUrl: args.databaseUrl,
        claimToken: pullAttempt.claimToken,
        leaseToken,
        targetId: args.context.target.id,
        scheduledSlotId: args.scheduledSlotId,
        amountRaw: pullAttempt.amountRaw,
        sourcePreBalanceRaw: pullAttempt.sourcePreBalanceRaw,
        destinationPreBalanceRaw: pullAttempt.destinationPreBalanceRaw,
      });
      if (pullSend.status !== "confirmed") {
        const alert = operationalAlertForAttempt(pullSend.attempt.state);
        if (attemptAllowsSafeRequeue(pullSend.attempt.state)) {
          await releaseAutodepositLotClaim({
            neon: args.neon,
            databaseUrl: args.databaseUrl,
            claimToken: pullAttempt.claimToken,
            leaseToken,
            lastError: `durable pull attempt ${pullSend.attempt.signature} ${pullSend.status}`,
            pauseTargetForMissingDelegate: false,
            retryDelaySeconds: PRE_SEND_FAILURE_RETRY_DELAY_SECONDS,
          });
        } else {
          await releaseAutodepositClaimLease({
            neon: args.neon,
            databaseUrl: args.databaseUrl,
            claimToken: pullAttempt.claimToken,
            leaseToken,
          });
        }
        console.log(
          JSON.stringify({
            status: `autodeposit_pull_${pullSend.status}`,
            recoverySource: "persisted_signed_pull",
            targetId: args.context.target.id.toString(),
            scheduledSlotId: args.scheduledSlotId.toString(),
            signature: pullSend.attempt.signature,
            retryable: pullSend.status !== "ambiguous",
            alert,
          })
        );
        if (alert) {
          process.exitCode = autodepositExecutorFailureExitCode(
            "transaction_effect_ambiguous"
          );
        }
        return;
      }
      pullAttempt = pullSend.attempt;
    }
    const result = await resumeDirectKaminoDeposit({
      attempt: pullAttempt,
      claimToken: pullAttempt.claimToken,
      connection: args.connection,
      databaseUrl: args.databaseUrl,
      leaseToken,
      neon: args.neon,
      plan: args.context.plan,
      rpcUrl: requireEnv("SOLANA_RPC_URL"),
      scheduledSlotId: args.scheduledSlotId,
      target: args.context.target,
    });
    console.log(
      JSON.stringify(
        {
          status:
            result.status === "completed"
              ? "autodeposit_completed"
              : "autodeposit_deposit_pending",
          recoverySource: "persisted_confirmed_pull",
          targetId: args.context.target.id.toString(),
          scheduledSlotId: args.scheduledSlotId.toString(),
          signatures: {
            pull: pullAttempt.signature,
            kaminoDeposit:
              result.status === "completed" ? result.deposit.signature : null,
          },
          walletPostPullRaw: result.walletPostPullRaw.toString(),
          vaultPostPullRaw: result.vaultObservation.amountRaw.toString(),
          retryable: result.status === "deposit_pending",
          alert: null,
        },
        null,
        2
      )
    );
  } catch (error) {
    await releaseAutodepositClaimLease({
      neon: args.neon,
      databaseUrl: args.databaseUrl,
      claimToken: args.context.attempt.claimToken,
      leaseToken,
    });
    if (
      error instanceof AutodepositEffectAmbiguousError ||
      error instanceof AutodepositOwnershipLostError
    ) {
      process.exitCode = autodepositExecutorFailureExitCode(
        "transaction_effect_ambiguous"
      );
      throw error;
    }
    console.log(
      JSON.stringify({
        status: "autodeposit_deposit_pending",
        recoverySource: "persisted_confirmed_pull",
        targetId: args.context.target.id.toString(),
        scheduledSlotId: args.scheduledSlotId.toString(),
        retryable: true,
        alert: null,
        error: error instanceof Error ? error.message : String(error),
      })
    );
  }
}

function summarizeSimulation(summary: SimulationSummary) {
  return {
    err: summary.err,
    unitsConsumed: summary.unitsConsumed,
    lastLog: summary.logs.at(-1) ?? null,
    errorLogTail: summary.err ? summary.logs.slice(-12) : [],
  };
}

async function main() {
  const appModules = await loadAppModules();
  const PublicKeyCtor = appModules.PublicKey;
  const options = parseOptions(Bun.argv.slice(2));
  const databaseUrl = requireEnv("NEON_DATABASE_URL");
  const rpcUrl = requireEnv("SOLANA_RPC_URL");
  const connection = new Connection(rpcUrl, DEFAULT_COMMITMENT);
  const autodepositRecovery =
    options.execute &&
    options.claimToken !== null &&
    options.targetId !== null &&
    options.scheduledSlotId !== null
      ? await loadAutodepositRecoveryContext({
          neon: appModules.neon,
          databaseUrl,
          claimToken: options.claimToken,
          targetId: options.targetId,
          scheduledSlotId: options.scheduledSlotId,
        })
      : null;
  if (autodepositRecovery && options.scheduledSlotId !== null) {
    await recoverAutodepositClaim({
      context: autodepositRecovery,
      compilePreparedOperation: appModules.compilePreparedOperation,
      connection,
      databaseUrl,
      neon: appModules.neon,
      scheduledSlotId: options.scheduledSlotId,
    });
    return;
  }
  const policyKeypair = parseKeypairSecretWith(
    appModules.Keypair,
    requireEnv("POLICY_KEYPAIR")
  );
  const programId = new PublicKeyCtor(
    process.env.LOYAL_SMART_ACCOUNTS_PROGRAM_ID ?? appModules.PROGRAM_ADDRESS
  );

  let target: EligibleTarget | null;
  try {
    target = await loadEligibleTarget(
      appModules.neon,
      databaseUrl,
      options.targetId
    );
  } catch (error) {
    if (
      error instanceof MissingActiveEarnRoutePolicyError &&
      options.execute &&
      options.scheduledSlotId !== null
    ) {
      await markScheduledSlotFailed({
        neon: appModules.neon,
        databaseUrl,
        scheduledSlotId: options.scheduledSlotId,
        targetId: error.targetId,
        lastError: error.message,
      });
      console.log(
        JSON.stringify(
          {
            status: "failed",
            reason: "missing_active_earn_route_policy",
            targetId: error.targetId.toString(),
            scheduledSlotId: options.scheduledSlotId.toString(),
            error: error.message,
          },
          null,
          2
        )
      );
      process.exitCode = 1;
      return;
    }
    throw error;
  }
  if (!target) {
    console.log(
      JSON.stringify(
        { status: "noop", reason: "no_eligible_autodeposit_target" },
        null,
        2
      )
    );
    return;
  }

  const walletUsdcAta = new PublicKeyCtor(target.walletUsdcAta);
  const vaultUsdcAta = new PublicKeyCtor(target.vaultUsdcAta);
  const walletBalanceRaw = await getTokenBalanceRaw(connection, walletUsdcAta);
  const vaultPreBalanceRaw = await getTokenBalanceRaw(connection, vaultUsdcAta);
  const allowance = await loadRecurringDelegationAllowance({
    appModules,
    connection,
    recurringDelegation: new PublicKeyCtor(target.recurringDelegation),
    periodLengthSeconds: target.periodLengthSeconds,
    startTimestamp: target.startTimestamp,
  });
  const effectiveFloorRaw =
    options.overrideFloorRaw ?? target.walletBalanceFloorRaw;
  const sweepDecision = computeSweepAmount({
    walletBalanceRaw,
    walletBalanceFloorRaw: effectiveFloorRaw,
    maxAmountPerPeriodRaw: target.maxAmountPerPeriodRaw,
    remainingAllowanceRaw: allowance.remainingAmountInPeriodRaw,
  });

  const client = appModules.createSmartAccountVaultsClient({
    connection: createPrepareConnection(connection),
    programId,
  });
  assertAutodepositPullSupport(client);
  const defaultEarnTarget = appModules.getKaminoUsdcEarnTargetForCluster(
    appModules.LoyalCluster.MainnetBeta
  );
  const expectedUsdcMint = defaultEarnTarget.liquidityMint.toBase58();
  if (expectedUsdcMint !== USDC_MINT_ADDRESS) {
    throw new Error(
      `SDK USDC mint ${expectedUsdcMint} does not match executor USDC guard ${USDC_MINT_ADDRESS}.`
    );
  }
  if (target.tokenMint !== expectedUsdcMint) {
    throw new Error(
      `Autodeposit target mint ${target.tokenMint} is not supported; USDC-only executor expected ${expectedUsdcMint}.`
    );
  }
  if (target.managedVaultId === undefined) {
    throw new Error(
      `Autodeposit target ${target.id} is not linked to an active managed vault.`
    );
  }

  let lotClaim: LotClaimResult | null = null;
  let executionAmountRaw =
    sweepDecision.kind === "sweep" ? sweepDecision.amountRaw : BigInt(0);
  if (options.requireLotClaim) {
    if (!options.execute) {
      lotClaim = {
        status: "noop",
        reason: "lot_claim_skipped_for_dry_run",
        claimToken: null,
        targetId: target.id,
        amountRaw: BigInt(0),
        staleCheckEventId: BigInt(0),
        lots: [],
      };
    } else {
      if (!options.claimToken) {
        throw new Error(
          "--claim-token is required when executing with lot claims."
        );
      }
      lotClaim = await claimAutodepositLots({
        neon: appModules.neon,
        databaseUrl,
        targetId: target.id,
        tokenMint: target.tokenMint,
        claimToken: options.claimToken,
        scheduledSlotId: options.scheduledSlotId,
        walletBalanceRaw,
        walletBalanceFloorRaw: effectiveFloorRaw,
        maxAmountPerPeriodRaw: target.maxAmountPerPeriodRaw,
        remainingAllowanceRaw: allowance.remainingAmountInPeriodRaw,
      });
      if (lotClaim.status !== "selected") {
        console.log(
          JSON.stringify(
            {
              status: "noop",
              reason: `lot_claim_${lotClaim.reason ?? lotClaim.status}`,
              targetId: target.id.toString(),
              scheduledSlotId: options.scheduledSlotId?.toString() ?? null,
              lotClaim: summarizeLotClaim(lotClaim),
              walletBalanceRaw: walletBalanceRaw.toString(),
              walletBalanceFloorRaw: effectiveFloorRaw.toString(),
              subscriptionAllowance: summarizeAllowance(allowance),
            },
            null,
            2
          )
        );
        return;
      }
      executionAmountRaw = lotClaim.amountRaw;
    }
  }

  if (
    lotClaim?.status !== "selected" &&
    (sweepDecision.kind === "no_excess" ||
      sweepDecision.kind === "allowance_exhausted")
  ) {
    console.log(
      JSON.stringify(
        {
          status: "noop",
          reason:
            sweepDecision.kind === "no_excess"
              ? "wallet_balance_not_above_floor"
              : "subscription_allowance_exhausted",
          targetId: target.id.toString(),
          scheduledSlotId: options.scheduledSlotId?.toString() ?? null,
          walletBalanceRaw: walletBalanceRaw.toString(),
          walletBalanceFloorRaw: effectiveFloorRaw.toString(),
          persistedWalletBalanceFloorRaw:
            target.walletBalanceFloorRaw.toString(),
          overrideFloorRaw: options.overrideFloorRaw?.toString() ?? null,
          excessRaw: sweepDecision.excessRaw.toString(),
          subscriptionAllowance: summarizeAllowance(allowance),
        },
        null,
        2
      )
    );
    return;
  }

  let pullSent = false;
  let claimLeaseToken: string | null = null;
  try {
    if (
      options.execute &&
      lotClaim?.status === "selected" &&
      lotClaim.claimToken
    ) {
      claimLeaseToken = await acquireAutodepositClaimLease({
        neon: appModules.neon,
        databaseUrl,
        claimToken: lotClaim.claimToken,
        targetId: target.id,
      });
      if (!claimLeaseToken) {
        console.log(
          JSON.stringify({
            status: "autodeposit_claim_owned",
            targetId: target.id.toString(),
            scheduledSlotId: options.scheduledSlotId?.toString() ?? null,
            retryable: true,
            alert: null,
          })
        );
        return;
      }
    }
    const existingDurablePullAttempt =
      options.execute && lotClaim?.status === "selected" && lotClaim.claimToken
        ? await loadDurableAutodepositAttempt({
            neon: appModules.neon,
            databaseUrl,
            claimToken: lotClaim.claimToken,
            operationKind: "pull",
          })
        : null;
    if (
      existingDurablePullAttempt &&
      existingDurablePullAttempt.amountRaw !== executionAmountRaw
    ) {
      throw new Error(
        `Persisted pull amount ${existingDurablePullAttempt.amountRaw} does not match selected claim amount ${executionAmountRaw}.`
      );
    }
    if (!existingDurablePullAttempt) {
      assertEmptyVaultBeforeDirectAutodeposit(vaultPreBalanceRaw);
      await assertSolBalance({
        connection,
        feePayer: policyKeypair.publicKey,
        minimumLamports: AUTODEPOSIT_PULL_FEE_PAYER_MIN_LAMPORTS,
        role: "Autodeposit pull fee payer",
      });
    }
    const vaultTokenAccountReadiness = await ensureVaultTokenAccountBeforePull({
      connection,
      execute: options.execute,
      feePayer: policyKeypair,
      target,
    });

    const pull = existingDurablePullAttempt
      ? null
      : await client.prepareEarnUsdcAutodepositPull({
          policy: new PublicKeyCtor(target.sweepPolicyAccount),
          walletAddress: new PublicKeyCtor(target.wallet),
          feePayer: policyKeypair.publicKey,
          policySigner: policyKeypair.publicKey,
          recurringDelegation: new PublicKeyCtor(target.recurringDelegation),
          amountRaw: executionAmountRaw,
          cluster: appModules.LoyalCluster.MainnetBeta,
        });
    const pulledLiquidityMint =
      pull?.persistence.liquidityMint ?? target.tokenMint;
    if (pulledLiquidityMint !== target.tokenMint) {
      throw new Error(
        `Autodeposit pull mint ${pulledLiquidityMint} does not match target mint ${target.tokenMint}.`
      );
    }

    const pullSimulation: SimulationSummary = pull
      ? await simulatePreparedOperation({
          compilePreparedOperation: appModules.compilePreparedOperation,
          connection,
          prepared: pull.prepared,
          signers: [policyKeypair],
        })
      : {
          err: null,
          logs: ["persisted signed pull is reconciled instead of rebuilt"],
          unitsConsumed: null,
        };
    const depositPreflight = await preflightDurableKaminoDeposit({
      amountRaw: executionAmountRaw,
      execute: options.execute,
      defaultMarket: defaultEarnTarget.market.toBase58(),
      defaultReserve: defaultEarnTarget.reserve.toBase58(),
      defaultLiquidityMint: defaultEarnTarget.liquidityMint.toBase58(),
      rpcUrl,
      target,
    });
    const topUpFeePayer = new PublicKeyCtor(
      readRequiredString(
        readRecord(depositPreflight.dryRun.json?.wallet)?.signer,
        "Kamino top-up fee payer"
      )
    );
    if (pullSimulation.err) {
      throw new Error(
        `Autodeposit pull simulation failed; refusing to execute. pull=${JSON.stringify(
          summarizeSimulation(pullSimulation)
        )}`
      );
    }

    const plan = {
      status: options.execute ? "execute_requested" : "dry_run",
      targetId: target.id.toString(),
      scheduledSlotId: options.scheduledSlotId?.toString() ?? null,
      wallet: target.wallet,
      vault: target.vaultPubkey,
      walletUsdcAta: target.walletUsdcAta,
      vaultUsdcAta: target.vaultUsdcAta,
      walletBalanceRaw: walletBalanceRaw.toString(),
      walletBalanceFloorRaw: effectiveFloorRaw.toString(),
      persistedWalletBalanceFloorRaw: target.walletBalanceFloorRaw.toString(),
      overrideFloorRaw: options.overrideFloorRaw?.toString() ?? null,
      vaultPreBalanceRaw: vaultPreBalanceRaw.toString(),
      topUpTarget: {
        reserve: depositPreflight.reserve,
        market: depositPreflight.market,
        liquidityMint: depositPreflight.liquidityMint,
        owner: "autodeposit-claim",
      },
      excessRaw: sweepDecision.excessRaw.toString(),
      amountRaw: executionAmountRaw.toString(),
      amountUi: Number(executionAmountRaw) / 10 ** USDC_DECIMALS,
      cappedByMaxPerPeriod:
        sweepDecision.kind === "sweep"
          ? sweepDecision.cappedByMaxPerPeriod
          : false,
      cappedByRemainingAllowance:
        sweepDecision.kind === "sweep"
          ? sweepDecision.cappedByRemainingAllowance
          : false,
      subscriptionAllowance: summarizeAllowance(allowance),
      transactionOrder: [
        "subscription_pull_wallet_to_earn_vault",
        "kamino_route_policy_top_up_from_earn_vault",
      ],
      signers: {
        pull: policyKeypair.publicKey.toBase58(),
        kaminoTopUpFeePayer: topUpFeePayer.toBase58(),
      },
      policies: {
        sweep: target.sweepPolicyAccount,
      },
      simulations: {
        pull: summarizeSimulation(pullSimulation),
        kaminoTopUp: depositPreflight.evidence,
      },
      vaultTokenAccountReadiness,
      lotClaim: lotClaim ? summarizeLotClaim(lotClaim) : null,
      sendsTransactions: options.execute,
    };

    if (!options.execute) {
      console.log(JSON.stringify(plan, null, 2));
      return;
    }

    if (
      lotClaim?.status !== "selected" ||
      !lotClaim.claimToken ||
      options.scheduledSlotId === null
    ) {
      throw new Error(
        "Durable autodeposit execution requires a selected lot claim and scheduled slot."
      );
    }
    const durableClaimToken = lotClaim.claimToken;
    const durableScheduledSlotId = options.scheduledSlotId;
    if (!claimLeaseToken) {
      throw new AutodepositOwnershipLostError(
        `Autodeposit claim ${durableClaimToken} has no executor lease.`
      );
    }
    const durableLeaseToken = claimLeaseToken;
    await renewAutodepositClaimLease({
      neon: appModules.neon,
      databaseUrl,
      claimToken: durableClaimToken,
      leaseToken: durableLeaseToken,
    });
    const durableDepositPlan = await persistAutodepositDepositPlan({
      neon: appModules.neon,
      databaseUrl,
      claimToken: durableClaimToken,
      leaseToken: durableLeaseToken,
      plan: {
        version: 1,
        amountRaw: executionAmountRaw,
        reserve: depositPreflight.reserve,
        market: depositPreflight.market,
        liquidityMint: depositPreflight.liquidityMint,
        target: target as EligibleTarget & { managedVaultId: bigint },
      },
    });
    const { result: durablePullSend } = await runAfterFeePayerSolSafety({
      connection,
      feePayer: topUpFeePayer,
      run: () =>
        sendPreparedOperation({
          compilePreparedOperation: appModules.compilePreparedOperation,
          connection,
          prepared: pull?.prepared ?? null,
          signers: [policyKeypair],
          neon: appModules.neon,
          databaseUrl,
          claimToken: durableClaimToken,
          leaseToken: durableLeaseToken,
          targetId: target.id,
          scheduledSlotId: durableScheduledSlotId,
          amountRaw: executionAmountRaw,
          sourcePreBalanceRaw: walletBalanceRaw,
          destinationPreBalanceRaw: vaultPreBalanceRaw,
        }),
    });
    if (durablePullSend.status !== "confirmed") {
      const alert = operationalAlertForAttempt(durablePullSend.attempt.state);
      if (attemptAllowsSafeRequeue(durablePullSend.attempt.state)) {
        await releaseAutodepositLotClaim({
          neon: appModules.neon,
          databaseUrl,
          claimToken: durableClaimToken,
          leaseToken: durableLeaseToken,
          lastError: `durable pull attempt ${durablePullSend.attempt.signature} ${durablePullSend.status}`,
          pauseTargetForMissingDelegate: false,
          retryDelaySeconds: PRE_SEND_FAILURE_RETRY_DELAY_SECONDS,
        });
      } else {
        await releaseAutodepositClaimLease({
          neon: appModules.neon,
          databaseUrl,
          claimToken: durableClaimToken,
          leaseToken: durableLeaseToken,
        });
      }
      console.log(
        JSON.stringify({
          status: `autodeposit_pull_${durablePullSend.status}`,
          targetId: target.id.toString(),
          scheduledSlotId: durableScheduledSlotId.toString(),
          signature: durablePullSend.attempt.signature,
          attemptState: durablePullSend.attempt.state,
          retryable: durablePullSend.status !== "ambiguous",
          recoveryRequired: durablePullSend.status === "ambiguous",
          error: attemptErrorDetail(durablePullSend.error),
          alert,
        })
      );
      if (alert) {
        process.exitCode = autodepositExecutorFailureExitCode(
          "transaction_effect_ambiguous"
        );
      }
      return;
    }
    const pullSend = durablePullSend;
    pullSent = true;
    try {
      const result = await resumeDirectKaminoDeposit({
        attempt: pullSend.attempt,
        claimToken: durableClaimToken,
        connection,
        databaseUrl,
        leaseToken: durableLeaseToken,
        neon: appModules.neon,
        plan: durableDepositPlan,
        rpcUrl,
        scheduledSlotId: durableScheduledSlotId,
        target: target as EligibleTarget & { managedVaultId: bigint },
      });
      console.log(
        JSON.stringify(
          {
            ...plan,
            status:
              result.status === "completed"
                ? "autodeposit_completed"
                : "autodeposit_deposit_pending",
            signatures: {
              pull: pullSend.signature,
              kaminoDeposit:
                result.status === "completed" ? result.deposit.signature : null,
            },
            confirmedSlots: {
              pull: pullSend.slot.toString(),
              kaminoDeposit:
                result.status === "completed"
                  ? result.deposit.confirmedSlot.toString()
                  : null,
            },
            walletPostPullRaw: result.walletPostPullRaw.toString(),
            vaultPostPullRaw: result.vaultObservation.amountRaw.toString(),
            retryable: result.status === "deposit_pending",
            alert: null,
          },
          null,
          2
        )
      );
    } catch (error) {
      await releaseAutodepositClaimLease({
        neon: appModules.neon,
        databaseUrl,
        claimToken: durableClaimToken,
        leaseToken: durableLeaseToken,
      });
      if (
        !(error instanceof AutodepositEffectAmbiguousError) &&
        !(error instanceof AutodepositOwnershipLostError)
      ) {
        console.log(
          JSON.stringify({
            status: "autodeposit_deposit_pending",
            targetId: target.id.toString(),
            scheduledSlotId: durableScheduledSlotId.toString(),
            signature: pullSend.signature,
            retryable: true,
            alert: null,
            error: error instanceof Error ? error.message : String(error),
          })
        );
        return;
      }
      process.exitCode = autodepositExecutorFailureExitCode(
        "transaction_effect_ambiguous"
      );
      throw error;
    }
  } catch (error) {
    // A blocked route never moved funds, so it deserves its own exit code rather than
    // the generic 1 that every unclassified failure shares. A vault confirmed empty,
    // or a missing delegate whose quarantine completed, deserves neither an alert nor
    // the fast retry cadence.
    const disposition = autodepositFailureDisposition(error);
    const missingTokenDelegate =
      isMissingAutodepositTokenDelegateFailure(error);
    if (shouldNotifyFailedSweep(disposition.failureCode)) {
      logSolanaWeekNotifyResult(
        await notifyFailedSweep({
          PublicKeyCtor,
          amountRaw: null,
          ownerWalletAddress: target.wallet,
          scheduledSlotId: options.scheduledSlotId,
        })
      );
    }
    if (!process.exitCode && disposition.failureCode) {
      process.exitCode = autodepositExecutorFailureExitCode(
        disposition.failureCode
      );
    }
    let unresolvedPullAttempt: DurableAutodepositAttempt | null = null;
    let pullAttemptLookupFailed = false;
    if (!pullSent && lotClaim?.claimToken) {
      try {
        unresolvedPullAttempt = await loadDurableAutodepositAttempt({
          neon: appModules.neon,
          databaseUrl,
          claimToken: lotClaim.claimToken,
          operationKind: "pull",
        });
      } catch {
        // Fail closed. Losing the database read is not evidence that the exact
        // signed transaction was never persisted or broadcast.
        pullAttemptLookupFailed = true;
      }
    }
    if (
      !pullSent &&
      !unresolvedPullAttempt &&
      !pullAttemptLookupFailed &&
      lotClaim?.status === "selected" &&
      lotClaim.claimToken
    ) {
      const claimToken = lotClaim.claimToken;
      const lastError = error instanceof Error ? error.message : String(error);
      const releaseClaim = () =>
        releaseAutodepositLotClaim({
          neon: appModules.neon,
          databaseUrl,
          claimToken,
          leaseToken: claimLeaseToken,
          lastError: lastError.slice(0, 4_000),
          pauseTargetForMissingDelegate: missingTokenDelegate,
          retryDelaySeconds: disposition.retryDelaySeconds,
        });
      if (missingTokenDelegate) {
        await quarantineMissingAutodepositDelegate({
          releaseClaim,
          targetId: target.id,
          scheduledSlotId: options.scheduledSlotId,
          onQuarantined: (event) => {
            if (!process.exitCode) {
              process.exitCode = autodepositExecutorFailureExitCode(
                "not_actionable"
              );
            }
            console.log(JSON.stringify(event));
          },
        });
      } else {
        await releaseClaim();
      }
    }
    if (
      claimLeaseToken &&
      lotClaim?.status === "selected" &&
      lotClaim.claimToken
    ) {
      await releaseAutodepositClaimLease({
        neon: appModules.neon,
        databaseUrl,
        claimToken: lotClaim.claimToken,
        leaseToken: claimLeaseToken,
      });
    }
    const reconciliation = await reconcileClosedRoutePolicyFailure({
      connection,
      databaseUrl,
      error,
      execute: options.execute,
      neon: appModules.neon,
      target,
    });
    if (reconciliation) {
      console.log(
        JSON.stringify({
          status: "closed_route_policy_reconciliation",
          targetId: target.id.toString(),
          routePolicyAccount: target.routePolicyAccount,
          reconciliation,
        })
      );
    }
    if (
      reconciliation &&
      closedRoutePolicyReconciliationIsNotActionable(reconciliation)
    ) {
      process.exitCode = autodepositExecutorFailureExitCode("not_actionable");
      return;
    }
    if (
      !pullSent &&
      !unresolvedPullAttempt &&
      !pullAttemptLookupFailed &&
      disposition.failureCode !== "fee_payer_exhausted" &&
      !missingTokenDelegate
    ) {
      process.exitCode = 0;
      console.log(
        JSON.stringify({
          status: "autodeposit_preflight_retry_pending",
          targetId: target.id.toString(),
          scheduledSlotId: options.scheduledSlotId?.toString() ?? null,
          retryable: true,
          alert: null,
          error: error instanceof Error ? error.message : String(error),
        })
      );
      return;
    }
    throw error;
  }
}

if (import.meta.main) {
  main().catch((error) => {
    console.error(
      JSON.stringify(
        {
          status: "error",
          error: error instanceof Error ? error.message : String(error),
        },
        null,
        2
      )
    );
    if (!process.exitCode) {
      process.exitCode = 1;
    }
  });
}
