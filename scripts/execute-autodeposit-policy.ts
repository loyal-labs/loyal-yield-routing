import {
  Connection,
  Keypair,
  PublicKey,
  VersionedTransaction,
  type AddressLookupTableAccount,
  type TransactionInstruction,
} from "@solana/web3.js";
import bs58 from "bs58";
import { existsSync } from "node:fs";

import {
  runDurableAutodepositExecution,
  type AttemptOutcome,
  type DurableAutodepositExecution,
  type DurableAutodepositStore,
  type LandedAttemptEvidence,
  type LeaseFence,
  type PersistedAttempt,
  type PreparedSignedAttempt,
  type PullChainEvidence,
  type TopUpChainEvidence,
} from "./durable-autodeposit-execution";

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
  scheduledSlotId: bigint;
  targetId: bigint | null;
};

type EligibleTarget = {
  id: bigint;
  cluster: string;
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
  routePolicyAccount: string;
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
    super(`Autodeposit target ${targetId} does not have an active Earn route policy.`);
    this.name = "MissingActiveEarnRoutePolicyError";
    this.targetId = targetId;
  }
}

const DEFAULT_COMMITMENT = "confirmed";
const DEFAULT_LOCAL_SAME_MINT_COMMAND = ["bun", "run", "same-mint:swap", "--"] as const;
const SAME_MINT_ROUTE_MODE = "same_mint_kamino";
const USDC_MINT_ADDRESS = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v";
const USDC_DECIMALS = 6;
const PRE_SEND_FAILURE_RETRY_DELAY_SECONDS = 5 * 60;
const AUTODEPOSIT_TOP_UP_FEE_PAYER_MIN_LAMPORTS = 50_000_000;
const SOLANA_WEEK_NOTIFY_ENDPOINT_ENV = "SOLANA_WEEK_NOTIFY_ENDPOINT";
const SOLANA_WEEK_NOTIFY_SECRET_ENV = "SOLANA_WEEK_NOTIFY_SECRET";
const SOLANA_WEEK_NOTIFY_TIMEOUT_MS = 5_000;

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

  if (input.remainingAllowanceRaw !== null && input.remainingAllowanceRaw !== undefined) {
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
        throw new Error("--scheduled-slot-id requires an unsigned integer value.");
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

export function assertDurableExecuteIdentity(
  options: Pick<CliOptions, "claimToken" | "execute" | "requireLotClaim" | "scheduledSlotId">,
): void {
  if (
    options.execute &&
    (!options.requireLotClaim || options.claimToken === null || options.scheduledSlotId === null)
  ) {
    throw new Error(
      "--execute requires --require-lot-claim, --claim-token, and --scheduled-slot-id for durable recovery.",
    );
  }
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
  options: Pick<CliOptions, "claimToken" | "scheduledSlotId" | "targetId">
): Promise<EligibleTarget | null> {
  const sql = neon(databaseUrl);
  const rows = await sql`
    SELECT
      t.id,
      COALESCE(NULLIF(t.cluster, ''), 'mainnet-beta') AS cluster,
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
      COALESCE(recovery.top_up_policy_account, rp.policy_account) AS route_policy_account,
      COALESCE(recovery.top_up_policy_seed, rp.policy_seed) AS route_policy_seed,
      COALESCE(recovery.top_up_route_modes, rp.route_modes) AS route_modes,
      recovery.id AS recovery_execution_id,
      yp.current_reserve,
      yp.current_market,
      yp.current_liquidity_mint
    FROM loyal_yield.balance_sweep_targets t
    LEFT JOIN LATERAL (
      SELECT policy_account, policy_seed, route_modes
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
      SELECT
        execution.id,
        execution.top_up_policy_account,
        execution.top_up_policy_seed,
        execution.top_up_route_modes
      FROM loyal_yield.balance_sweep_executions AS execution
      WHERE execution.target_id = t.id
        AND execution.lifecycle_state <> 'completed'
        AND (
          (${options.claimToken !== null}
            AND execution.claim_token = ${options.claimToken})
          OR
          (${options.scheduledSlotId !== null}
            AND execution.scheduled_slot_id = ${options.scheduledSlotId?.toString() ?? null})
        )
      ORDER BY execution.id DESC
      LIMIT 1
    ) recovery ON TRUE
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
    WHERE (
        (
          t.active
          AND t.lifecycle_status = 'active'
          AND t.wallet_balance_floor_raw IS NOT NULL
          AND t.recurring_delegation IS NOT NULL
        )
        OR recovery.id IS NOT NULL
      )
      AND (${options.targetId === null} OR t.id = ${options.targetId?.toString() ?? null})
    ORDER BY t.id
    LIMIT 2
  `;

  if (rows.length === 0) {
    return null;
  }
  if (rows.length > 1 && options.targetId === null) {
    throw new Error("Multiple eligible autodeposit targets found; pass --target-id.");
  }

  const row = rows[0] as Record<string, unknown>;
  const id = BigInt(readRequiredString(row.id, "id"));
  const routePolicyAccount = readNullableString(row.route_policy_account);
  if (!routePolicyAccount) {
    throw new MissingActiveEarnRoutePolicyError(id);
  }

  return {
    id,
    cluster: readRequiredString(row.cluster, "cluster"),
    settings: readRequiredString(row.settings, "settings"),
    vaultIndex: Number(readRequiredString(row.vault_index, "vault_index")),
    wallet: readRequiredString(row.wallet, "wallet"),
    walletUsdcAta: readRequiredString(row.wallet_usdc_ata, "wallet_usdc_ata"),
    walletTokenAta: readRequiredString(row.wallet_token_ata, "wallet_token_ata"),
    vaultPubkey: readRequiredString(row.vault_pubkey, "vault_pubkey"),
    vaultUsdcAta: readRequiredString(row.vault_usdc_ata, "vault_usdc_ata"),
    vaultTokenAta: readRequiredString(row.vault_token_ata, "vault_token_ata"),
    tokenMint: readRequiredString(row.token_mint, "token_mint"),
    sweepPolicyAccount: readRequiredString(
      row.sweep_policy_account,
      "sweep_policy_account"
    ),
    routePolicyAccount,
    routePolicySeed: BigInt(readRequiredString(row.route_policy_seed, "route_policy_seed")),
    routeModes: readStringArray(row.route_modes, "route_modes"),
    recurringDelegation: readRequiredString(
      row.recurring_delegation,
      "recurring_delegation"
    ),
    walletBalanceFloorRaw: BigInt(
      readRequiredString(row.wallet_balance_floor_raw, "wallet_balance_floor_raw")
    ),
    maxAmountPerPeriodRaw: row.max_amount_per_period
      ? BigInt(readRequiredString(row.max_amount_per_period, "max_amount_per_period"))
      : null,
    periodLengthSeconds: row.period_length_seconds
      ? BigInt(readRequiredString(row.period_length_seconds, "period_length_seconds"))
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
          amountRaw: BigInt(readRequiredString(lot.amount_raw, "claim.amount_raw")),
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
            WHEN COALESCE(${args.maxAmountPerPeriodRaw?.toString() ?? null}::bigint, 0) > 0
            THEN ${args.maxAmountPerPeriodRaw?.toString() ?? null}::bigint
            ELSE 9223372036854775807
          END,
          COALESCE(${args.remainingAllowanceRaw?.toString() ?? null}::bigint, 9223372036854775807)
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
        WHEN ${args.scheduledSlotId !== null} AND NOT EXISTS (SELECT 1 FROM slot_guard) THEN 'scheduled_slot_not_available'
        WHEN COALESCE((SELECT event_id FROM processed), 0) < (SELECT event_id FROM stale) THEN 'newer_unprocessed_wallet_event'
        WHEN ${args.walletBalanceRaw.toString()}::bigint - ${args.walletBalanceFloorRaw.toString()}::bigint <= 0 THEN 'wallet_balance_not_above_floor'
        WHEN COALESCE(${args.remainingAllowanceRaw?.toString() ?? null}::bigint, 1) <= 0 THEN 'allowance_exhausted'
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

async function releaseAutodepositLotClaim(args: { neon: AppModules["neon"]; databaseUrl: string; claimToken: string }) {
  const sql = args.neon(args.databaseUrl);
  await sql`
    WITH selected_claim AS (
      SELECT c.claim_token
      FROM loyal_yield.balance_sweep_lot_claims c
      JOIN loyal_yield.balance_sweep_targets t
        ON t.id = c.target_id
      WHERE c.claim_token = ${args.claimToken}
        AND c.status = 'selected'
        AND t.token_mint = ${USDC_MINT_ADDRESS}
    ),
    restored AS (
      UPDATE loyal_yield.balance_sweep_surplus_lots l
      SET remaining_amount_raw = l.remaining_amount_raw + i.amount_raw,
          status = 'open',
          eligible_after = now() + (${PRE_SEND_FAILURE_RETRY_DELAY_SECONDS} * interval '1 second'),
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
    updated_claim AS (
      UPDATE loyal_yield.balance_sweep_lot_claims
      SET status = 'released',
          updated_at = now()
      WHERE claim_token = (SELECT claim_token FROM selected_claim)
        AND EXISTS (SELECT 1 FROM restored)
      RETURNING claim_token
    )
    UPDATE loyal_yield.balance_sweep_scheduled_slots
    SET status = 'failed',
        claim_token = NULL,
        last_error = 'claim released before autodeposit pull',
        updated_at = now()
    WHERE claim_token IN (SELECT claim_token FROM updated_claim)
  `;
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
      `Recurring delegation account ${args.recurringDelegation.toBase58()} has unexpected data length ${account.data.length}.`
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
  return new DataView(
    data.buffer,
    data.byteOffset + offset,
    8
  ).getBigUint64(0, true);
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
    remainingAmountInPeriodRaw:
      allowance.remainingAmountInPeriodRaw.toString(),
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
  const latestBlockhash =
    await args.connection.getLatestBlockhash(DEFAULT_COMMITMENT);
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

async function prepareSignedOperation(args: {
  compilePreparedOperation: AppModules["compilePreparedOperation"];
  connection: Connection;
  prepared: PreparedOperation;
  signers: Keypair[];
}): Promise<PreparedSignedAttempt> {
  const latestBlockhash =
    await args.connection.getLatestBlockhash(DEFAULT_COMMITMENT);
  const transaction = args.compilePreparedOperation({
    prepared: args.prepared,
    blockhash: latestBlockhash.blockhash,
  });
  transaction.sign(args.signers);
  const signatureBytes = transaction.signatures[0];
  if (!signatureBytes) {
    throw new Error(
      "Prepared transaction is missing its deterministic signature.",
    );
  }
  return {
    signature: bs58.encode(signatureBytes),
    blockhash: latestBlockhash.blockhash,
    lastValidBlockHeight: BigInt(latestBlockhash.lastValidBlockHeight),
    signedTransactionBase64: Buffer.from(transaction.serialize()).toString(
      "base64",
    ),
  };
}

function preparedTopUpAttempt(
  result: SameMintTopUpResult,
  options: { allowExpectedPrePullFundingSkip?: boolean } = {},
): PreparedSignedAttempt {
  const policyDepositTransaction = readRecord(
    result.json?.policyDepositTransaction,
  );
  const prepared = readRecord(policyDepositTransaction?.preparedTransaction);
  const signature = readRequiredString(
    prepared?.signature,
    "prepared Kamino top-up signature",
  );
  const blockhash = readRequiredString(
    prepared?.blockhash,
    "prepared Kamino top-up blockhash",
  );
  const lastValidBlockHeight = BigInt(
    readRequiredString(
      prepared?.lastValidBlockHeight,
      "prepared Kamino top-up lastValidBlockHeight",
    ),
  );
  const signedTransactionBase64 = readRequiredString(
    prepared?.signedTransactionBase64,
    "prepared Kamino top-up signed transaction",
  );
  const transaction = VersionedTransaction.deserialize(
    Buffer.from(signedTransactionBase64, "base64"),
  );
  const derivedSignature = transaction.signatures[0]
    ? bs58.encode(transaction.signatures[0])
    : null;
  if (derivedSignature !== signature) {
    throw new Error(
      `Prepared Kamino transaction signature ${derivedSignature} does not match ${signature}.`,
    );
  }
  if (transaction.message.recentBlockhash !== blockhash) {
    throw new Error(
      `Prepared Kamino transaction blockhash ${transaction.message.recentBlockhash} does not match ${blockhash}.`,
    );
  }
  if (policyDepositTransaction?.simulationError) {
    throw new Error(
      `Prepared Kamino top-up simulation failed: ${policyDepositTransaction.simulationError}`,
    );
  }
  if (
    policyDepositTransaction?.simulationSkippedReason &&
    !(
      options.allowExpectedPrePullFundingSkip &&
      policyDepositTransaction.simulationSkippedReason ===
        PRE_PULL_FUNDING_SIMULATION_SKIP
    )
  ) {
    throw new Error(
      `Prepared Kamino top-up was not simulated: ${policyDepositTransaction.simulationSkippedReason}`,
    );
  }
  return {
    signature,
    blockhash,
    lastValidBlockHeight,
    signedTransactionBase64,
  };
}

async function broadcastExactSignedTransaction(
  connection: Connection,
  attempt: PersistedAttempt,
): Promise<string> {
  const bytes = Buffer.from(attempt.signedTransactionBase64, "base64");
  const transaction = VersionedTransaction.deserialize(bytes);
  const derivedSignature = transaction.signatures[0]
    ? bs58.encode(transaction.signatures[0])
    : null;
  if (derivedSignature !== attempt.signature) {
    throw new Error(
      `Persisted signed transaction derives ${derivedSignature}, expected ${attempt.signature}.`,
    );
  }
  return connection.sendRawTransaction(bytes, {
    maxRetries: 0,
    skipPreflight: true,
  });
}

export async function reconcilePersistedAttempt(args: {
  attempt: PersistedAttempt;
  connection: Connection;
  waitForConfirmation: boolean;
}): Promise<AttemptOutcome> {
  if (args.waitForConfirmation) {
    try {
      const confirmation = await args.connection.confirmTransaction(
        {
          signature: args.attempt.signature,
          blockhash: args.attempt.blockhash,
          lastValidBlockHeight: Number(args.attempt.lastValidBlockHeight),
        },
        DEFAULT_COMMITMENT,
      );
      if (confirmation.value.err) {
        return {
          classification: "failed",
          error: confirmation.value.err,
        };
      }
    } catch {
      // A confirmation timeout or BlockhashNotFound is ambiguous until the
      // persisted signature and block height are reconciled below.
    }
  }

  const statuses = await args.connection.getSignatureStatuses(
    [args.attempt.signature],
    {
      searchTransactionHistory: true,
    },
  );
  const status = statuses.value[0];
  if (status?.err) {
    return { classification: "failed", error: status.err };
  }
  if (status) {
    if (
      status.confirmationStatus === "confirmed" ||
      status.confirmationStatus === "finalized"
    ) {
      return {
        classification: "landed",
        confirmedSlot: BigInt(status.slot),
      };
    }
    // A processed status proves the signature reached a fork. It is not safe
    // to call the attempt non-landed merely because its blockhash later
    // expires; reconciliation must retain the signature as unknown.
    return { classification: "unknown", error: null };
  }

  const currentBlockHeight =
    await args.connection.getBlockHeight(DEFAULT_COMMITMENT);
  if (BigInt(currentBlockHeight) > args.attempt.lastValidBlockHeight) {
    return {
      classification: "expired_not_landed",
      error: null,
    };
  }
  return { classification: "unknown", error: null };
}

type ParsedTokenBalance = {
  accountIndex: number;
  mint: string;
  uiTokenAmount: { amount: string };
};

async function readTokenBalanceEvidence(args: {
  connection: Connection;
  signature: string;
  tokenMint: string;
  sourceTokenAccount: string;
  destinationTokenAccount?: string;
}) {
  const transaction = await args.connection.getParsedTransaction(
    args.signature,
    {
      commitment: DEFAULT_COMMITMENT,
      maxSupportedTransactionVersion: 0,
    },
  );
  if (!transaction?.meta || transaction.meta.err) {
    throw new Error(
      `Confirmed transaction ${args.signature} is unavailable or failed.`,
    );
  }
  const accountKeys = transaction.transaction.message.accountKeys.map((key) =>
    key.pubkey.toBase58(),
  );
  const sourceIndex = accountKeys.indexOf(args.sourceTokenAccount);
  const destinationIndex = args.destinationTokenAccount
    ? accountKeys.indexOf(args.destinationTokenAccount)
    : -1;
  if (
    sourceIndex < 0 ||
    (args.destinationTokenAccount && destinationIndex < 0)
  ) {
    throw new Error(
      `Transaction ${args.signature} does not contain the expected token accounts.`,
    );
  }
  const pre = (transaction.meta.preTokenBalances ?? []) as ParsedTokenBalance[];
  const post = (transaction.meta.postTokenBalances ??
    []) as ParsedTokenBalance[];
  const amountAt = (
    balances: ParsedTokenBalance[],
    accountIndex: number,
  ): bigint => {
    const balance = balances.find(
      (item) =>
        item.accountIndex === accountIndex && item.mint === args.tokenMint,
    );
    return balance ? BigInt(balance.uiTokenAmount.amount) : BigInt(0);
  };
  return {
    slot: BigInt(transaction.slot),
    sourcePreBalanceRaw: amountAt(pre, sourceIndex),
    sourcePostBalanceRaw: amountAt(post, sourceIndex),
    destinationPreBalanceRaw:
      destinationIndex >= 0 ? amountAt(pre, destinationIndex) : BigInt(0),
    destinationPostBalanceRaw:
      destinationIndex >= 0 ? amountAt(post, destinationIndex) : BigInt(0),
  };
}

async function readConfirmedPullEvidence(args: {
  connection: Connection;
  signature: string;
  target: EligibleTarget;
}): Promise<PullChainEvidence> {
  const balances = await readTokenBalanceEvidence({
    connection: args.connection,
    signature: args.signature,
    tokenMint: args.target.tokenMint,
    sourceTokenAccount: args.target.walletTokenAta,
    destinationTokenAccount: args.target.vaultTokenAta,
  });
  const sourceDebitRaw =
    balances.sourcePreBalanceRaw - balances.sourcePostBalanceRaw;
  const destinationCreditRaw =
    balances.destinationPostBalanceRaw - balances.destinationPreBalanceRaw;
  if (sourceDebitRaw <= BigInt(0) || sourceDebitRaw !== destinationCreditRaw) {
    throw new Error(
      `Pull ${args.signature} token evidence is inconsistent: source debit ${sourceDebitRaw}, vault credit ${destinationCreditRaw}.`,
    );
  }
  return {
    confirmedSlot: balances.slot,
    ...balances,
    destinationCreditRaw,
  };
}

async function readConfirmedTopUpEvidence(args: {
  amountRaw: bigint;
  connection: Connection;
  signature: string;
  target: EligibleTarget;
}): Promise<TopUpChainEvidence> {
  const balances = await readTokenBalanceEvidence({
    connection: args.connection,
    signature: args.signature,
    tokenMint: args.target.tokenMint,
    sourceTokenAccount: args.target.vaultTokenAta,
  });
  const vaultDebitRaw =
    balances.sourcePreBalanceRaw - balances.sourcePostBalanceRaw;
  if (vaultDebitRaw !== args.amountRaw) {
    throw new Error(
      `Kamino top-up ${args.signature} debited ${vaultDebitRaw}, expected confirmed pull ${args.amountRaw}.`,
    );
  }
  return { confirmedSlot: balances.slot, vaultDebitRaw };
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
  | { status: "skipped"; reason: "missing_endpoint" | "missing_secret" }
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

async function runSameMintReserveTopUp(args: {
  amountRaw: bigint;
  execute: boolean;
  reserve: string;
  rpcUrl: string;
  target: EligibleTarget;
}): Promise<SameMintTopUpResult> {
  const command = [
    ...sameMintReserveSwapCommand(),
    "--settings",
    args.target.settings,
    "--vault-index",
    args.target.vaultIndex.toString(),
    "--route-policy-account",
    args.target.routePolicyAccount,
    "--emit-prepared-transaction",
    "--deposit-reserve",
    args.reserve,
    args.amountRaw.toString(),
    "--rpc-url",
    args.rpcUrl,
  ];
  if (args.execute) {
    command.push("--execute");
  }

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

function requireTopUpFeePayer(
  result: SameMintTopUpResult,
  PublicKeyCtor: typeof PublicKey
): PublicKey {
  const wallet = readRecord(result.json?.wallet);
  return new PublicKeyCtor(
    readRequiredString(wallet?.signer, "Kamino top-up fee payer")
  );
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

export function redactSensitiveText(value: string): string {
  let redacted = value;
  const rpcUrl = process.env.SOLANA_RPC_URL;
  if (rpcUrl) {
    redacted = redacted.split(rpcUrl).join("[redacted SOLANA_RPC_URL]");
  }
  return redacted
    .replace(/api-key=[^'"\s]+/gi, "api-key=[redacted]")
    .replace(
      /("signedTransactionBase64"\s*:\s*")[^"]+(")/g,
      "$1[redacted signed transaction]$2"
    );
}

function serializedRedactedError(error: unknown): string {
  if (error === null || error === undefined) return "null";
  const value =
    error instanceof Error
      ? { name: error.name, message: error.message }
      : error;
  let serialized: string | undefined;
  try {
    serialized = JSON.stringify(value, (_key, item) =>
      typeof item === "bigint" ? item.toString() : item
    );
  } catch {
    serialized = undefined;
  }
  return redactSensitiveText(serialized ?? JSON.stringify(String(error)));
}

async function notifySolanaWeekSweep(args: {
  PublicKeyCtor: typeof PublicKey;
  ownerWalletAddress: string;
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
      body: JSON.stringify({ walletAddress }),
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
  return value && typeof value === "object" && !Array.isArray(value) ? (value as Record<string, unknown>) : null;
}

function tailLines(value: string, count: number): string[] {
  return value.trim().split(/\r?\n/).filter(Boolean).slice(-count);
}

async function recordAutodepositYieldDeposit(args: {
  amountRaw: bigint;
  appModules: AppModules;
  balanceSweepExecutionId: string;
  databaseUrl: string;
  depositSignature: string;
  depositSlot: bigint;
  liquidityMint: string;
  market: string;
  observedCurrentAmountRaw: bigint | null;
  observedSlot: bigint | null;
  policySignature: string;
  scheduledSlotId: bigint;
  target: EligibleTarget;
  targetReserve: string;
}): Promise<{
  status: "duplicate" | "inserted";
  depositId: string;
  positionId: string;
}> {
  const sql = args.appModules.neon(args.databaseUrl);
  const rows = await sql`
    SELECT *
    FROM loyal_yield.record_durable_autodeposit_yield_deposit(
      ${args.amountRaw.toString()},
      ${args.balanceSweepExecutionId},
      ${args.depositSignature},
      ${args.depositSlot.toString()},
      ${args.liquidityMint},
      ${args.market},
      ${args.observedCurrentAmountRaw?.toString() ?? null},
      ${args.observedSlot?.toString() ?? null},
      ${args.policySignature},
      ${args.scheduledSlotId.toString()},
      ${args.target.wallet},
      ${args.target.vaultPubkey},
      ${args.target.settings},
      ${args.target.vaultIndex},
      ${args.target.routePolicySeed.toString()},
      ${args.target.routePolicyAccount},
      ${args.targetReserve}
    )
  `;
  const result = rows[0] as Record<string, unknown> | undefined;
  if (!result) {
    throw new Error("Atomic autodeposit application persistence returned no result.");
  }
  const status = readRequiredString(result.result_status, "result_status");
  if (status !== "inserted" && status !== "duplicate") {
    throw new Error(`Unexpected atomic autodeposit persistence status ${status}.`);
  }
  return {
    status,
    depositId: readRequiredString(result.result_deposit_id, "result_deposit_id"),
    positionId: readRequiredString(result.result_position_id, "result_position_id"),
  };
}

async function markAutodepositExecutionCompleted(args: {
  neon: AppModules["neon"];
  databaseUrl: string;
  executionId: string;
  scheduledSlotId: bigint;
  kaminoDepositSignature: string;
}) {
  const sql = args.neon(args.databaseUrl);
  await sql`
    SELECT loyal_yield.mark_autodeposit_execution_completed(
      ${args.executionId},
      ${args.scheduledSlotId.toString()},
      ${args.kaminoDepositSignature}
    )
  `;
}

function summarizeSimulation(summary: SimulationSummary) {
  return {
    err: summary.err,
    unitsConsumed: summary.unitsConsumed,
    lastLog: summary.logs.at(-1) ?? null,
    errorLogTail: summary.err ? summary.logs.slice(-12) : [],
  };
}

function summarizeSimulationFailure(summary: SimulationSummary): string {
  return JSON.stringify(summarizeSimulation(summary));
}

function topUpPolicySimulationError(result: SameMintTopUpResult): string | null {
  const policyDepositTransaction = readRecord(result.json?.policyDepositTransaction);
  return policyDepositTransaction?.simulationError?.toString() ?? null;
}

function topUpPolicySimulationSkippedReason(result: SameMintTopUpResult): string | null {
  const policyDepositTransaction = readRecord(result.json?.policyDepositTransaction);
  return policyDepositTransaction?.simulationSkippedReason?.toString() ?? null;
}

const PRE_PULL_FUNDING_SIMULATION_SKIP =
  "policy deposit simulation requires the wallet funding transaction to land first";

function isReplacedByAutodepositPullBlocker(blocker: unknown): boolean {
  if (typeof blocker !== "string") return false;
  return (
    /^wallet USDC ATA .+ does not exist for .+$/.test(blocker) ||
    /^wallet USDC balance \d+ is below needed funding amount \d+$/.test(blocker)
  );
}

export function assertExecutablePreflight(args: {
  pullSimulation: SimulationSummary;
  topUpDryRun: SameMintTopUpResult;
}) {
  if (args.pullSimulation.err) {
    throw new Error(
      `Autodeposit pull simulation failed; refusing to execute. simulation=${summarizeSimulationFailure(
        args.pullSimulation
      )}`
    );
  }
  const topUpError = topUpPolicySimulationError(args.topUpDryRun);
  if (topUpError) {
    throw new Error(
      `Kamino route-policy top-up dry-run simulation failed; refusing to execute. topUp=${JSON.stringify(
        summarizeTopUpResult(args.topUpDryRun)
      )}`
    );
  }
  const topUpSkippedReason = topUpPolicySimulationSkippedReason(args.topUpDryRun);
  const preflightBlockers = Array.isArray(args.topUpDryRun.json?.preflightBlockers)
    ? args.topUpDryRun.json.preflightBlockers.filter((value) => value !== null)
    : [];
  const missingObligationSetup = args.topUpDryRun.json?.missingObligationSetup ?? null;
  const unexpectedBlockers = preflightBlockers.filter(
    (blocker) => !isReplacedByAutodepositPullBlocker(blocker)
  );
  const expectedFundingSkip =
    topUpSkippedReason === PRE_PULL_FUNDING_SIMULATION_SKIP;
  if (
    missingObligationSetup ||
    unexpectedBlockers.length > 0 ||
    (preflightBlockers.length > 0 && !expectedFundingSkip) ||
    (topUpSkippedReason !== null && !expectedFundingSkip)
  ) {
    throw new Error(
      `Kamino top-up is not fully preparable; refusing to pull user funds. topUp=${JSON.stringify(
        summarizeTopUpResult(args.topUpDryRun)
      )}`
    );
  }
  preparedTopUpAttempt(args.topUpDryRun, {
    allowExpectedPrePullFundingSkip: expectedFundingSkip,
  });
}

export async function runAfterExecutablePreflight<T>(args: {
  pullSimulation: SimulationSummary;
  topUpDryRun: SameMintTopUpResult;
  run: () => Promise<T>;
}): Promise<T> {
  assertExecutablePreflight(args);
  return args.run();
}

const VAULT_LEASE_TTL_SECONDS = 90;
const VAULT_LEASE_RENEW_INTERVAL_MS = 30_000;

async function acquireVaultLease(args: {
  neon: AppModules["neon"];
  databaseUrl: string;
  cluster: string;
  vaultPubkey: string;
  claimToken: string | null;
  scheduledSlotId: bigint | null;
}): Promise<LeaseFence> {
  const sql = args.neon(args.databaseUrl);
  const ownerToken = crypto.randomUUID();
  const rows = await sql`
    WITH execution_state AS (
      SELECT
        COUNT(*) > 0 AS has_nonterminal,
        COALESCE(BOOL_OR(
          (${args.claimToken !== null}
            AND execution.claim_token = ${args.claimToken})
          OR
          (${args.scheduledSlotId !== null}
            AND execution.scheduled_slot_id = ${args.scheduledSlotId?.toString() ?? null})
        ), false) AS has_matching_execution,
        COALESCE(BOOL_OR(
          execution.active_attempt_kind IS NOT NULL
          AND NOT (
              (${args.claimToken !== null}
                AND execution.claim_token = ${args.claimToken})
              OR
              (${args.scheduledSlotId !== null}
                AND execution.scheduled_slot_id = ${args.scheduledSlotId?.toString() ?? null})
          )
        ), false) AS has_competing_active_attempt
      FROM loyal_yield.balance_sweep_executions AS execution
      JOIN loyal_yield.balance_sweep_targets AS target
        ON target.id = execution.target_id
      WHERE COALESCE(NULLIF(target.cluster, ''), 'mainnet-beta') = ${args.cluster}
        AND target.vault_pubkey = ${args.vaultPubkey}
        AND execution.lifecycle_state <> 'completed'
    ),
    recovery_guard AS (
      SELECT
        NOT has_nonterminal
        OR (has_matching_execution AND NOT has_competing_active_attempt)
          AS allowed
      FROM execution_state
    )
    INSERT INTO loyal_yield.vault_operation_leases (
      cluster,
      vault_pubkey,
      owner_token,
      fence,
      expires_at,
      updated_at
    )
    SELECT
      ${args.cluster},
      ${args.vaultPubkey},
      ${ownerToken},
      1,
      now() + (${VAULT_LEASE_TTL_SECONDS} * interval '1 second'),
      now()
    FROM recovery_guard
    WHERE allowed
    ON CONFLICT (cluster, vault_pubkey) DO UPDATE
    SET
      owner_token = EXCLUDED.owner_token,
      fence = loyal_yield.vault_operation_leases.fence + 1,
      expires_at = EXCLUDED.expires_at,
      updated_at = now()
    WHERE loyal_yield.vault_operation_leases.expires_at <= now()
      AND loyal_yield.vault_operation_leases.blocking_signature IS NULL
      AND (SELECT allowed FROM recovery_guard)
    RETURNING owner_token, fence
  `;
  const row = rows[0] as Record<string, unknown> | undefined;
  if (!row) {
    throw new Error(`Vault ${args.vaultPubkey} already has an active durable operation lease.`);
  }
  return {
    cluster: args.cluster,
    vaultPubkey: args.vaultPubkey,
    ownerToken: readRequiredString(row.owner_token, "lease.owner_token"),
    fence: BigInt(readRequiredString(row.fence, "lease.fence")),
  };
}

async function renewVaultLease(args: {
  neon: AppModules["neon"];
  databaseUrl: string;
  lease: LeaseFence;
}): Promise<boolean> {
  const sql = args.neon(args.databaseUrl);
  const rows = await sql`
    UPDATE loyal_yield.vault_operation_leases
    SET
      expires_at = now() + (${VAULT_LEASE_TTL_SECONDS} * interval '1 second'),
      updated_at = now()
    WHERE cluster = ${args.lease.cluster}
      AND vault_pubkey = ${args.lease.vaultPubkey}
      AND owner_token = ${args.lease.ownerToken}
      AND fence = ${args.lease.fence.toString()}
      AND expires_at > now()
    RETURNING fence
  `;
  return rows.length === 1;
}

async function releaseVaultLease(args: { neon: AppModules["neon"]; databaseUrl: string; lease: LeaseFence }) {
  const sql = args.neon(args.databaseUrl);
  await sql`
    DELETE FROM loyal_yield.vault_operation_leases
    WHERE cluster = ${args.lease.cluster}
      AND vault_pubkey = ${args.lease.vaultPubkey}
      AND owner_token = ${args.lease.ownerToken}
      AND fence = ${args.lease.fence.toString()}
  `;
}

async function withVaultLease<T>(args: {
  neon: AppModules["neon"];
  databaseUrl: string;
  cluster: string;
  vaultPubkey: string;
  claimToken: string | null;
  scheduledSlotId: bigint | null;
  run: (lease: LeaseFence) => Promise<T>;
}): Promise<T> {
  const lease = await acquireVaultLease(args);
  let leaseLost = false;
  const heartbeat = setInterval(() => {
    void renewVaultLease({
      neon: args.neon,
      databaseUrl: args.databaseUrl,
      lease,
    })
      .then((renewed) => {
        if (!renewed) leaseLost = true;
      })
      .catch(() => {
        leaseLost = true;
      });
  }, VAULT_LEASE_RENEW_INTERVAL_MS);
  try {
    const result = await args.run(lease);
    if (leaseLost) {
      throw new Error("Vault operation lease was lost during execution.");
    }
    return result;
  } finally {
    clearInterval(heartbeat);
    await releaseVaultLease({
      neon: args.neon,
      databaseUrl: args.databaseUrl,
      lease,
    });
  }
}

type DurableStoreContext = {
  appModules: AppModules;
  databaseUrl: string;
  target: EligibleTarget;
  claimToken: string | null;
  scheduledSlotId: bigint | null;
  requestedAmountRaw: bigint;
  topUpReserve: string;
  topUpMarket: string;
  topUpLiquidityMint: string;
};

class NeonDurableAutodepositStore implements DurableAutodepositStore {
  constructor(private readonly context: DurableStoreContext) {}

  private sql() {
    return this.context.appModules.neon(this.context.databaseUrl);
  }

  private async reload(): Promise<DurableAutodepositExecution> {
    const execution = await this.loadExecution();
    if (!execution) {
      throw new Error("Durable autodeposit execution disappeared after persistence.");
    }
    return execution;
  }

  async loadExecution(): Promise<DurableAutodepositExecution | null> {
    if (!this.context.claimToken && this.context.scheduledSlotId === null) {
      return null;
    }
    const sql = this.sql();
    const rows = await sql`
      SELECT
        execution.id,
        execution.lifecycle_state,
        execution.requested_amount_raw,
        execution.confirmed_pull_amount_raw,
        execution.reserved_amount_raw,
        execution.signature AS pull_signature,
        execution.kamino_deposit_signature,
        execution.top_up_reserve,
        execution.top_up_market,
        execution.top_up_liquidity_mint,
        attempt.id AS attempt_id,
        attempt.operation_kind,
        attempt.attempt_number,
        attempt.signature AS attempt_signature,
        attempt.blockhash,
        attempt.last_valid_block_height,
        attempt.signed_transaction_base64,
        attempt.classification,
        attempt.broadcast_at
      FROM loyal_yield.balance_sweep_executions AS execution
      LEFT JOIN LATERAL (
        SELECT candidate.*
        FROM loyal_yield.balance_sweep_execution_attempts AS candidate
        WHERE candidate.execution_id = execution.id
          AND candidate.operation_kind = execution.active_attempt_kind
          AND candidate.classification IN ('prepared', 'unknown')
        ORDER BY candidate.attempt_number DESC
        LIMIT 1
      ) AS attempt ON TRUE
      WHERE execution.target_id = ${this.context.target.id.toString()}
        AND (
          (${this.context.claimToken !== null}
            AND execution.claim_token = ${this.context.claimToken})
          OR
          (${this.context.scheduledSlotId !== null}
            AND execution.scheduled_slot_id = ${this.context.scheduledSlotId?.toString() ?? null})
        )
      ORDER BY execution.id DESC
      LIMIT 1
    `;
    const row = rows[0] as Record<string, unknown> | undefined;
    if (!row) return null;
    const hasReplayableAttempt =
      row.attempt_id !== null &&
      row.attempt_id !== undefined &&
      row.blockhash !== null &&
      row.last_valid_block_height !== null &&
      row.signed_transaction_base64 !== null;
    const activeAttempt: PersistedAttempt | null = hasReplayableAttempt
      ? {
          id: readRequiredString(row.attempt_id, "attempt.id"),
          executionId: readRequiredString(row.id, "execution.id"),
          operationKind: readRequiredString(
            row.operation_kind,
            "attempt.operation_kind"
          ) as PersistedAttempt["operationKind"],
          attemptNumber: Number(readRequiredString(row.attempt_number, "attempt.attempt_number")),
          signature: readRequiredString(row.attempt_signature, "attempt.signature"),
          blockhash: readRequiredString(row.blockhash, "attempt.blockhash"),
          lastValidBlockHeight: BigInt(
            readRequiredString(row.last_valid_block_height, "attempt.last_valid_block_height")
          ),
          signedTransactionBase64: readRequiredString(
            row.signed_transaction_base64,
            "attempt.signed_transaction_base64"
          ),
          classification: readRequiredString(
            row.classification,
            "attempt.classification"
          ) as PersistedAttempt["classification"],
          broadcastAt: row.broadcast_at ? readRequiredString(row.broadcast_at, "attempt.broadcast_at") : null,
        }
      : null;
    return {
      id: readRequiredString(row.id, "execution.id"),
      lifecycleState: readRequiredString(
        row.lifecycle_state,
        "execution.lifecycle_state"
      ) as DurableAutodepositExecution["lifecycleState"],
      requestedAmountRaw: BigInt(readRequiredString(row.requested_amount_raw, "execution.requested_amount_raw")),
      confirmedPullAmountRaw: row.confirmed_pull_amount_raw ? BigInt(row.confirmed_pull_amount_raw.toString()) : null,
      reservedAmountRaw: BigInt(readRequiredString(row.reserved_amount_raw, "execution.reserved_amount_raw")),
      activeAttempt,
      pullSignature: row.pull_signature?.toString() ?? null,
      successfulTopUpSignature: row.kamino_deposit_signature?.toString() ?? null,
      topUpReserve: row.top_up_reserve?.toString() ?? this.context.topUpReserve,
      topUpMarket: row.top_up_market?.toString() ?? this.context.topUpMarket,
      topUpLiquidityMint: row.top_up_liquidity_mint?.toString() ?? this.context.topUpLiquidityMint,
    };
  }

  async assertLease(lease: LeaseFence): Promise<void> {
    const sql = this.sql();
    const rows = await sql`
      SELECT 1
      FROM loyal_yield.vault_operation_leases
      WHERE cluster = ${lease.cluster}
        AND vault_pubkey = ${lease.vaultPubkey}
        AND owner_token = ${lease.ownerToken}
        AND fence = ${lease.fence.toString()}
        AND expires_at > now()
    `;
    if (rows.length !== 1) {
      throw new Error(`Vault lease ${lease.cluster}:${lease.vaultPubkey}:${lease.fence} is stale.`);
    }
  }

  async createWithPreparedPull(
    lease: LeaseFence,
    attempt: PreparedSignedAttempt
  ): Promise<DurableAutodepositExecution> {
    const sql = this.sql();
    const dedupeIdentity = this.context.claimToken ?? this.context.scheduledSlotId?.toString() ?? attempt.signature;
    const dedupeKey = `${this.context.target.id}:durable-autodeposit:${dedupeIdentity}`;
    const rows = await sql`
      WITH lease_guard AS (
        SELECT 1
        FROM loyal_yield.vault_operation_leases
        WHERE cluster = ${lease.cluster}
          AND vault_pubkey = ${lease.vaultPubkey}
          AND owner_token = ${lease.ownerToken}
          AND fence = ${lease.fence.toString()}
          AND expires_at > now()
      ),
      claim_guard AS (
        SELECT claim_token
        FROM loyal_yield.balance_sweep_lot_claims
        WHERE claim_token = ${this.context.claimToken}
          AND status = 'selected'
          AND execution_id IS NULL
        FOR UPDATE
      ),
      slot_guard AS (
        SELECT id
        FROM loyal_yield.balance_sweep_scheduled_slots
        WHERE id = ${this.context.scheduledSlotId?.toString() ?? null}
          AND status = 'selected'
          AND execution_id IS NULL
        FOR UPDATE
      ),
      inserted_execution AS (
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
          source_commitment,
          raw_evidence,
          decoded_evidence,
          dedupe_key,
          scheduled_slot_id,
          claim_token,
          lifecycle_state,
          requested_amount_raw,
          confirmed_pull_amount_raw,
          reserved_amount_raw,
          top_up_reserve,
          top_up_market,
          top_up_liquidity_mint,
          top_up_policy_account,
          top_up_policy_seed,
          top_up_route_modes,
          active_attempt_kind
        )
        SELECT
          ${this.context.target.id.toString()},
          ${attempt.signature},
          NULL,
          ${this.context.target.walletUsdcAta},
          ${this.context.target.vaultUsdcAta},
          ${this.context.target.tokenMint},
          ${this.context.target.walletTokenAta},
          ${this.context.target.vaultTokenAta},
          ${this.context.requestedAmountRaw.toString()},
          'prepared',
          ${JSON.stringify({ source: "durable-autodeposit-executor" })}::jsonb,
          ${JSON.stringify({
            status: "durable_pull_confirmation_pending",
          })}::jsonb,
          ${dedupeKey},
          ${this.context.scheduledSlotId?.toString() ?? null},
          ${this.context.claimToken},
          'pull_confirmation_pending',
          ${this.context.requestedAmountRaw.toString()},
          NULL,
          0,
          ${this.context.topUpReserve},
          ${this.context.topUpMarket},
          ${this.context.topUpLiquidityMint},
          ${this.context.target.routePolicyAccount},
          ${this.context.target.routePolicySeed.toString()},
          ${this.context.target.routeModes},
          'pull'
        FROM lease_guard
        WHERE (${this.context.claimToken === null} OR EXISTS (SELECT 1 FROM claim_guard))
          AND (${this.context.scheduledSlotId === null} OR EXISTS (SELECT 1 FROM slot_guard))
        ON CONFLICT (dedupe_key) DO NOTHING
        RETURNING id
      ),
      linked_claim AS (
        UPDATE loyal_yield.balance_sweep_lot_claims AS claim
        SET
          execution_id = inserted_execution.id,
          updated_at = now()
        FROM inserted_execution
        WHERE claim.claim_token = ${this.context.claimToken}
          AND claim.status = 'selected'
          AND claim.execution_id IS NULL
        RETURNING claim.claim_token
      ),
      linked_slot AS (
        UPDATE loyal_yield.balance_sweep_scheduled_slots AS slot
        SET
          execution_id = inserted_execution.id,
          updated_at = now()
        FROM inserted_execution
        WHERE slot.id = ${this.context.scheduledSlotId?.toString() ?? null}
          AND slot.status = 'selected'
          AND slot.execution_id IS NULL
        RETURNING slot.id
      ),
      inserted_attempt AS (
        INSERT INTO loyal_yield.balance_sweep_execution_attempts (
          execution_id,
          operation_kind,
          attempt_number,
          signature,
          blockhash,
          last_valid_block_height,
          signed_transaction_base64,
          classification,
          lease_owner_token,
          lease_fence
        )
        SELECT
          id,
          'pull',
          1,
          ${attempt.signature},
          ${attempt.blockhash},
          ${attempt.lastValidBlockHeight.toString()},
          ${attempt.signedTransactionBase64},
          'prepared',
          ${lease.ownerToken},
          ${lease.fence.toString()}
        FROM inserted_execution
        WHERE (${this.context.claimToken === null} OR EXISTS (SELECT 1 FROM linked_claim))
          AND (${this.context.scheduledSlotId === null} OR EXISTS (SELECT 1 FROM linked_slot))
        RETURNING id
      )
      SELECT id FROM inserted_attempt
    `;
    if (rows.length !== 1) {
      const existing = await this.loadExecution();
      if (existing) return existing;
      throw new Error("Fenced pull-attempt persistence did not acquire ownership.");
    }
    return this.reload();
  }

  async appendPreparedTopUp(
    lease: LeaseFence,
    executionId: string,
    attempt: PreparedSignedAttempt
  ): Promise<DurableAutodepositExecution> {
    const sql = this.sql();
    const rows = await sql`
      WITH lease_guard AS (
        SELECT 1
        FROM loyal_yield.vault_operation_leases
        WHERE cluster = ${lease.cluster}
          AND vault_pubkey = ${lease.vaultPubkey}
          AND owner_token = ${lease.ownerToken}
          AND fence = ${lease.fence.toString()}
          AND expires_at > now()
      ),
      owned_execution AS (
        UPDATE loyal_yield.balance_sweep_executions AS execution
        SET
          lifecycle_state = 'deposit_confirmation_pending',
          active_attempt_kind = 'top_up',
          completion_failure_code = NULL,
          decoded_evidence = COALESCE(execution.decoded_evidence, '{}'::jsonb) ||
            ${JSON.stringify({
              status: "durable_deposit_confirmation_pending",
            })}::jsonb,
          decoded_at = now()
        FROM lease_guard
        WHERE execution.id = ${executionId}
          AND execution.lifecycle_state = 'deposit_pending'
          AND NOT EXISTS (
            SELECT 1
            FROM loyal_yield.balance_sweep_execution_attempts AS pending
            WHERE pending.execution_id = execution.id
              AND pending.operation_kind = 'top_up'
              AND pending.classification IN ('prepared', 'unknown')
          )
        RETURNING execution.id
      ),
      inserted_attempt AS (
        INSERT INTO loyal_yield.balance_sweep_execution_attempts (
          execution_id,
          operation_kind,
          attempt_number,
          signature,
          blockhash,
          last_valid_block_height,
          signed_transaction_base64,
          classification,
          lease_owner_token,
          lease_fence
        )
        SELECT
          owned_execution.id,
          'top_up',
          COALESCE((
            SELECT MAX(previous.attempt_number) + 1
            FROM loyal_yield.balance_sweep_execution_attempts AS previous
            WHERE previous.execution_id = owned_execution.id
              AND previous.operation_kind = 'top_up'
          ), 1),
          ${attempt.signature},
          ${attempt.blockhash},
          ${attempt.lastValidBlockHeight.toString()},
          ${attempt.signedTransactionBase64},
          'prepared',
          ${lease.ownerToken},
          ${lease.fence.toString()}
        FROM owned_execution
        RETURNING id
      )
      SELECT id FROM inserted_attempt
    `;
    if (rows.length !== 1) {
      throw new Error(`Execution ${executionId} refused a competing fenced top-up attempt.`);
    }
    return this.reload();
  }

  async recordBroadcast(lease: LeaseFence, attempt: PersistedAttempt) {
    const sql = this.sql();
    const rows = await sql`
      UPDATE loyal_yield.balance_sweep_execution_attempts AS attempt
      SET broadcast_at = COALESCE(attempt.broadcast_at, now())
      WHERE attempt.id = ${attempt.id}
        AND EXISTS (
          SELECT 1
          FROM loyal_yield.vault_operation_leases AS operation_lease
          WHERE operation_lease.cluster = ${lease.cluster}
            AND operation_lease.vault_pubkey = ${lease.vaultPubkey}
            AND operation_lease.owner_token = ${lease.ownerToken}
            AND operation_lease.fence = ${lease.fence.toString()}
            AND operation_lease.expires_at > now()
        )
      RETURNING attempt.id
    `;
    if (rows.length !== 1) {
      throw new Error("Stale lease could not record the broadcast boundary.");
    }
  }

  async recordNonLandedAttempt(
    lease: LeaseFence,
    execution: DurableAutodepositExecution,
    attempt: PersistedAttempt,
    outcome: Exclude<AttemptOutcome, LandedAttemptEvidence>
  ): Promise<DurableAutodepositExecution> {
    const sql = this.sql();
    const nextState =
      attempt.operationKind === "top_up" &&
      (outcome.classification === "failed" || outcome.classification === "expired_not_landed")
        ? "deposit_pending"
        : "needs_reconciliation";
    const keepActive = outcome.classification === "unknown";
    const rows = await sql`
      WITH lease_guard AS (
        SELECT 1
        FROM loyal_yield.vault_operation_leases
        WHERE cluster = ${lease.cluster}
          AND vault_pubkey = ${lease.vaultPubkey}
          AND owner_token = ${lease.ownerToken}
          AND fence = ${lease.fence.toString()}
          AND expires_at > now()
      ),
      classified AS (
        UPDATE loyal_yield.balance_sweep_execution_attempts AS attempt_row
        SET
          classification = ${outcome.classification},
          classified_at = now(),
          chain_error = ${serializedRedactedError(outcome.error)}::jsonb
        FROM lease_guard
        WHERE attempt_row.id = ${attempt.id}
          AND attempt_row.execution_id = ${execution.id}
          AND attempt_row.classification IN ('prepared', 'unknown')
        RETURNING attempt_row.id
      )
      UPDATE loyal_yield.balance_sweep_executions AS execution_row
      SET
        lifecycle_state = ${nextState},
        active_attempt_kind = ${keepActive ? attempt.operationKind : null},
        completion_failure_code = NULL,
        decoded_evidence = COALESCE(execution_row.decoded_evidence, '{}'::jsonb) ||
          jsonb_build_object(
            'status', ${nextState === "deposit_pending" ? "durable_deposit_pending" : "durable_needs_reconciliation"},
            'lastAttemptClassification', ${outcome.classification}
          ),
        decoded_at = now()
      WHERE execution_row.id = ${execution.id}
        AND EXISTS (SELECT 1 FROM classified)
      RETURNING execution_row.id
    `;
    if (rows.length !== 1) {
      throw new Error("Fenced attempt classification lost ownership.");
    }
    return this.reload();
  }

  async recordConfirmedPull(
    lease: LeaseFence,
    execution: DurableAutodepositExecution,
    attempt: PersistedAttempt,
    evidence: PullChainEvidence
  ): Promise<DurableAutodepositExecution> {
    const sql = this.sql();
    const rows = await sql`
      WITH lease_guard AS (
        SELECT 1
        FROM loyal_yield.vault_operation_leases
        WHERE cluster = ${lease.cluster}
          AND vault_pubkey = ${lease.vaultPubkey}
          AND owner_token = ${lease.ownerToken}
          AND fence = ${lease.fence.toString()}
          AND expires_at > now()
      ),
      classified AS (
        UPDATE loyal_yield.balance_sweep_execution_attempts AS attempt_row
        SET
          classification = 'landed',
          classified_at = now(),
          confirmed_slot = ${evidence.confirmedSlot.toString()},
          chain_error = NULL,
          evidence = attempt_row.evidence ||
            ${JSON.stringify({
              source: "confirmed_transaction_token_balances",
            })}::jsonb
        FROM lease_guard
        WHERE attempt_row.id = ${attempt.id}
          AND attempt_row.execution_id = ${execution.id}
          AND attempt_row.operation_kind = 'pull'
          AND attempt_row.classification IN ('prepared', 'unknown')
        RETURNING attempt_row.id
      ),
      advanced AS (
        UPDATE loyal_yield.balance_sweep_executions AS execution_row
        SET
          lifecycle_state = 'deposit_pending',
          active_attempt_kind = NULL,
          slot = ${evidence.confirmedSlot.toString()},
          amount_raw = ${evidence.destinationCreditRaw.toString()},
          confirmed_pull_amount_raw = ${evidence.destinationCreditRaw.toString()},
          reserved_amount_raw = ${evidence.destinationCreditRaw.toString()},
          source_pre_balance_raw = ${evidence.sourcePreBalanceRaw.toString()},
          source_post_balance_raw = ${evidence.sourcePostBalanceRaw.toString()},
          destination_pre_balance_raw = ${evidence.destinationPreBalanceRaw.toString()},
          destination_post_balance_raw = ${evidence.destinationPostBalanceRaw.toString()},
          source_commitment = 'confirmed',
          received_at = COALESCE(execution_row.received_at, now()),
          decoded_at = now(),
          completion_failure_code = NULL,
          decoded_evidence = COALESCE(execution_row.decoded_evidence, '{}'::jsonb) ||
            jsonb_build_object(
              'status', 'durable_deposit_pending',
              'confirmedPullAmountRaw', ${evidence.destinationCreditRaw.toString()}::text,
              'pullEvidenceSource', 'confirmed_transaction_token_balances'
            )
        WHERE execution_row.id = ${execution.id}
          AND EXISTS (SELECT 1 FROM classified)
        RETURNING execution_row.id, execution_row.claim_token, execution_row.scheduled_slot_id
      ),
      matched_lots AS (
        SELECT item.lot_id, item.amount_raw
        FROM loyal_yield.balance_sweep_lot_claim_items AS item
        JOIN advanced ON advanced.claim_token = item.claim_token
      ),
      inserted_lots AS (
        INSERT INTO loyal_yield.balance_sweep_execution_lots (
          execution_id,
          lot_id,
          amount_raw
        )
        SELECT ${execution.id}, lot_id, amount_raw
        FROM matched_lots
        ON CONFLICT (execution_id, lot_id) DO NOTHING
      ),
      completed_claim AS (
        UPDATE loyal_yield.balance_sweep_lot_claims AS claim
        SET
          status = 'executed',
          execution_id = ${execution.id},
          updated_at = now()
        FROM advanced
        WHERE claim.claim_token = advanced.claim_token
          AND claim.status IN ('selected', 'executed')
        RETURNING claim.claim_token
      )
      UPDATE loyal_yield.balance_sweep_scheduled_slots AS slot
      SET
        status = 'executed',
        execution_id = ${execution.id},
        updated_at = now()
      FROM advanced
      WHERE slot.id = advanced.scheduled_slot_id
        AND slot.status IN ('selected', 'executed')
      RETURNING slot.id
    `;
    if (rows.length !== 1 && this.context.scheduledSlotId !== null) {
      throw new Error("Confirmed pull did not atomically consume its claim and slot.");
    }
    return this.reload();
  }

  async recordConfirmedTopUp(
    lease: LeaseFence,
    execution: DurableAutodepositExecution,
    attempt: PersistedAttempt,
    evidence: TopUpChainEvidence
  ): Promise<DurableAutodepositExecution> {
    const sql = this.sql();
    const rows = await sql`
      WITH lease_guard AS (
        SELECT 1
        FROM loyal_yield.vault_operation_leases
        WHERE cluster = ${lease.cluster}
          AND vault_pubkey = ${lease.vaultPubkey}
          AND owner_token = ${lease.ownerToken}
          AND fence = ${lease.fence.toString()}
          AND expires_at > now()
      ),
      classified AS (
        UPDATE loyal_yield.balance_sweep_execution_attempts AS attempt_row
        SET
          classification = 'landed',
          classified_at = now(),
          confirmed_slot = ${evidence.confirmedSlot.toString()},
          chain_error = NULL,
          evidence = attempt_row.evidence || jsonb_build_object(
            'vaultDebitRaw', ${evidence.vaultDebitRaw.toString()}::text,
            'source', 'confirmed_transaction_token_balances'
          )
        FROM lease_guard
        WHERE attempt_row.id = ${attempt.id}
          AND attempt_row.execution_id = ${execution.id}
          AND attempt_row.operation_kind = 'top_up'
          AND attempt_row.classification IN ('prepared', 'unknown')
        RETURNING attempt_row.id
      )
      UPDATE loyal_yield.balance_sweep_executions AS execution_row
      SET
        lifecycle_state = 'deposit_confirmed',
        active_attempt_kind = NULL,
        successful_top_up_attempt_id = classified.id,
        kamino_deposit_signature = ${attempt.signature},
        reserved_amount_raw = 0,
        completion_failure_code = NULL,
        decoded_evidence = COALESCE(execution_row.decoded_evidence, '{}'::jsonb) ||
          jsonb_build_object(
            'status', 'durable_deposit_confirmed',
            'kaminoDepositSignature', ${attempt.signature},
            'kaminoDepositSlot', ${evidence.confirmedSlot.toString()}::text
          ),
        decoded_at = now()
      FROM classified
      WHERE execution_row.id = ${execution.id}
        AND execution_row.confirmed_pull_amount_raw = ${evidence.vaultDebitRaw.toString()}
      RETURNING execution_row.id
    `;
    if (rows.length !== 1) {
      throw new Error("Fenced top-up confirmation did not match the pull reservation.");
    }
    return this.reload();
  }

  async persistCompletion(
    lease: LeaseFence,
    execution: DurableAutodepositExecution
  ): Promise<DurableAutodepositExecution> {
    await this.assertLease(lease);
    if (
      this.context.scheduledSlotId === null ||
      !execution.successfulTopUpSignature ||
      !execution.confirmedPullAmountRaw
    ) {
      throw new Error(`Execution ${execution.id} lacks the slot, signature, or amount needed for completion.`);
    }
    const sql = this.sql();
    const attemptRows = await sql`
      SELECT confirmed_slot
      FROM loyal_yield.balance_sweep_execution_attempts
      WHERE execution_id = ${execution.id}
        AND operation_kind = 'top_up'
        AND signature = ${execution.successfulTopUpSignature}
        AND classification = 'landed'
      LIMIT 1
    `;
    const depositSlot = BigInt(
      readRequiredString(
        (attemptRows[0] as Record<string, unknown> | undefined)?.confirmed_slot,
        "successful top-up confirmed_slot"
      )
    );
    await recordAutodepositYieldDeposit({
      amountRaw: execution.confirmedPullAmountRaw,
      appModules: this.context.appModules,
      balanceSweepExecutionId: execution.id,
      databaseUrl: this.context.databaseUrl,
      depositSignature: execution.successfulTopUpSignature,
      depositSlot,
      liquidityMint: execution.topUpLiquidityMint,
      market: execution.topUpMarket,
      observedCurrentAmountRaw: null,
      observedSlot: depositSlot,
      policySignature: execution.successfulTopUpSignature,
      scheduledSlotId: this.context.scheduledSlotId,
      target: this.context.target,
      targetReserve: execution.topUpReserve,
    });
    await markAutodepositExecutionCompleted({
      neon: this.context.appModules.neon,
      databaseUrl: this.context.databaseUrl,
      executionId: execution.id,
      scheduledSlotId: this.context.scheduledSlotId,
      kaminoDepositSignature: execution.successfulTopUpSignature,
    });
    const rows = await sql`
      UPDATE loyal_yield.balance_sweep_executions AS execution_row
      SET
        lifecycle_state = 'completed',
        reserved_amount_raw = 0,
        active_attempt_kind = NULL,
        completion_failure_code = NULL,
        decoded_evidence = COALESCE(execution_row.decoded_evidence, '{}'::jsonb) ||
          ${JSON.stringify({ status: "executed" })}::jsonb,
        decoded_at = now()
      WHERE execution_row.id = ${execution.id}
        AND execution_row.completed_at IS NOT NULL
        AND EXISTS (
          SELECT 1
          FROM loyal_yield.vault_operation_leases AS operation_lease
          WHERE operation_lease.cluster = ${lease.cluster}
            AND operation_lease.vault_pubkey = ${lease.vaultPubkey}
            AND operation_lease.owner_token = ${lease.ownerToken}
            AND operation_lease.fence = ${lease.fence.toString()}
            AND operation_lease.expires_at > now()
        )
      RETURNING execution_row.id
    `;
    if (rows.length !== 1) {
      throw new Error("Completion persistence lost its fenced lease.");
    }
    return this.reload();
  }
}

async function executeDurableSaga(args: {
  connection: Connection;
  lease: LeaseFence;
  pullPrepared: PreparedOperation | null;
  policyKeypair: Keypair;
  rpcUrl: string;
  store: NeonDurableAutodepositStore;
  target: EligibleTarget;
  appModules: AppModules;
}): Promise<DurableAutodepositExecution> {
  return runDurableAutodepositExecution({
    store: args.store,
    lease: args.lease,
    preparePull: async () => {
      if (!args.pullPrepared) {
        throw new Error(
          "Recovery attempted to construct a replacement pull instead of reconciling persisted evidence."
        );
      }
      return prepareSignedOperation({
        compilePreparedOperation: args.appModules.compilePreparedOperation,
        connection: args.connection,
        prepared: args.pullPrepared,
        signers: [args.policyKeypair],
      });
    },
    prepareTopUp: async (amountRaw, execution) => {
      const prepared = await runSameMintReserveTopUp({
        amountRaw,
        execute: false,
        reserve: execution.topUpReserve,
        rpcUrl: args.rpcUrl,
        target: args.target,
      });
      return preparedTopUpAttempt(prepared);
    },
    broadcastExactSignedTransaction: (attempt) => broadcastExactSignedTransaction(args.connection, attempt),
    reconcileAttempt: (attempt, waitForConfirmation) =>
      reconcilePersistedAttempt({
        attempt,
        connection: args.connection,
        waitForConfirmation,
      }),
    readConfirmedPullEvidence: (attempt) =>
      readConfirmedPullEvidence({
        connection: args.connection,
        signature: attempt.signature,
        target: args.target,
      }),
    readConfirmedTopUpEvidence: (attempt) => {
      const executionAmountRaw = args.store.loadExecution().then((execution) => {
        if (!execution?.confirmedPullAmountRaw) {
          throw new Error("Top-up evidence requires confirmed pull amount.");
        }
        return execution.confirmedPullAmountRaw;
      });
      return executionAmountRaw.then((amountRaw) =>
        readConfirmedTopUpEvidence({
          amountRaw,
          connection: args.connection,
          signature: attempt.signature,
          target: args.target,
        })
      );
    },
  });
}

async function main() {
  const appModules = await loadAppModules();
  const PublicKeyCtor = appModules.PublicKey;
  const options = parseOptions(Bun.argv.slice(2));
  assertDurableExecuteIdentity(options);
  const databaseUrl = requireEnv("NEON_DATABASE_URL");
  const rpcUrl = requireEnv("SOLANA_RPC_URL");
  const policyKeypair = parseKeypairSecretWith(
    appModules.Keypair,
    requireEnv("POLICY_KEYPAIR"),
  );
  const programId = new PublicKeyCtor(
    process.env.LOYAL_SMART_ACCOUNTS_PROGRAM_ID ?? appModules.PROGRAM_ADDRESS,
  );
  const connection = new Connection(rpcUrl, DEFAULT_COMMITMENT);

  let target: EligibleTarget | null;
  try {
    target = await loadEligibleTarget(appModules.neon, databaseUrl, options);
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
          2,
        ),
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
        2,
      ),
    );
    return;
  }

  const runFlow = async (lease: LeaseFence | null) => {
    const client = appModules.createSmartAccountVaultsClient({
      connection: createPrepareConnection(connection),
      programId,
    });
    assertAutodepositPullSupport(client);
    const defaultEarnTarget = appModules.getKaminoUsdcEarnTargetForCluster(
      appModules.LoyalCluster.MainnetBeta,
    );
    const expectedUsdcMint = defaultEarnTarget.liquidityMint.toBase58();
    if (expectedUsdcMint !== USDC_MINT_ADDRESS) {
      throw new Error(
        `SDK USDC mint ${expectedUsdcMint} does not match executor USDC guard ${USDC_MINT_ADDRESS}.`,
      );
    }
    if (target.tokenMint !== expectedUsdcMint) {
      throw new Error(
        `Autodeposit target mint ${target.tokenMint} is not supported; USDC-only executor expected ${expectedUsdcMint}.`,
      );
    }
    const defaultTopUpReserve =
      target.currentReserve ?? defaultEarnTarget.reserve.toBase58();
    const defaultTopUpMarket =
      target.currentMarket ?? defaultEarnTarget.market.toBase58();
    const defaultTopUpLiquidityMint =
      target.currentLiquidityMint ?? expectedUsdcMint;
    const recoveryStore = new NeonDurableAutodepositStore({
      appModules,
      databaseUrl,
      target,
      claimToken: options.claimToken,
      scheduledSlotId: options.scheduledSlotId,
      requestedAmountRaw: BigInt(0),
      topUpReserve: defaultTopUpReserve,
      topUpMarket: defaultTopUpMarket,
      topUpLiquidityMint: defaultTopUpLiquidityMint,
    });
    const existingExecution = lease
      ? await recoveryStore.loadExecution()
      : null;
    if (existingExecution) {
      const recovered = await executeDurableSaga({
        connection,
        lease,
        pullPrepared: null,
        policyKeypair,
        rpcUrl,
        store: recoveryStore,
        target,
        appModules,
      });
      console.log(
        JSON.stringify(
          {
            status:
              recovered.lifecycleState === "completed"
                ? "executed"
                : recovered.lifecycleState,
            executionId: recovered.id,
            lifecycleState: recovered.lifecycleState,
            confirmedPullAmountRaw:
              recovered.confirmedPullAmountRaw?.toString() ?? null,
            reservedAmountRaw: recovered.reservedAmountRaw.toString(),
            signatures: {
              pull: recovered.pullSignature,
              kaminoDeposit: recovered.successfulTopUpSignature,
            },
            recovery: true,
          },
          null,
          2,
        ),
      );
      if (recovered.lifecycleState !== "completed") {
        process.exitCode = 1;
      }
      return;
    }

    const walletUsdcAta = new PublicKeyCtor(target.walletUsdcAta);
    const vaultUsdcAta = new PublicKeyCtor(target.vaultUsdcAta);
    const walletBalanceRaw = await getTokenBalanceRaw(
      connection,
      walletUsdcAta,
    );
    const vaultPreBalanceRaw = await getTokenBalanceRaw(
      connection,
      vaultUsdcAta,
    );
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

    if (
      sweepDecision.kind === "no_excess" ||
      sweepDecision.kind === "allowance_exhausted"
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
          2,
        ),
      );
      return;
    }

    let lotClaim: LotClaimResult | null = null;
    let executionAmountRaw = sweepDecision.amountRaw;
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
            "--claim-token is required when executing with lot claims.",
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
              2,
            ),
          );
          return;
        }
        executionAmountRaw = lotClaim.amountRaw;
      }
    }

    let pullSent = false;
    let durableStore: NeonDurableAutodepositStore | null = null;
    try {
      const pull = await client.prepareEarnUsdcAutodepositPull({
        policy: new PublicKeyCtor(target.sweepPolicyAccount),
        walletAddress: new PublicKeyCtor(target.wallet),
        feePayer: policyKeypair.publicKey,
        policySigner: policyKeypair.publicKey,
        recurringDelegation: new PublicKeyCtor(target.recurringDelegation),
        amountRaw: executionAmountRaw,
        cluster: appModules.LoyalCluster.MainnetBeta,
      });
      const topUpReserve = defaultTopUpReserve;
      const topUpMarket = defaultTopUpMarket;
      const topUpLiquidityMint =
        target.currentLiquidityMint ?? pull.persistence.liquidityMint;
      if (topUpLiquidityMint !== pull.persistence.liquidityMint) {
        throw new Error(
          `Autodeposit top-up liquidity mint ${topUpLiquidityMint} does not match pulled mint ${pull.persistence.liquidityMint}.`,
        );
      }

      const pullSimulation = await simulatePreparedOperation({
        compilePreparedOperation: appModules.compilePreparedOperation,
        connection,
        prepared: pull.prepared,
        signers: [policyKeypair],
      });
      const topUpDryRun = await runSameMintReserveTopUp({
        amountRaw: executionAmountRaw,
        execute: false,
        reserve: topUpReserve,
        rpcUrl,
        target,
      });
      const topUpFeePayer = requireTopUpFeePayer(topUpDryRun, PublicKeyCtor);

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
          reserve: topUpReserve,
          market: topUpMarket,
          liquidityMint: topUpLiquidityMint,
          source: target.currentReserve
            ? "active_yield_position"
            : "default_earn_target",
        },
        excessRaw: sweepDecision.excessRaw.toString(),
        amountRaw: executionAmountRaw.toString(),
        amountUi: Number(executionAmountRaw) / 10 ** USDC_DECIMALS,
        cappedByMaxPerPeriod: sweepDecision.cappedByMaxPerPeriod,
        cappedByRemainingAllowance: sweepDecision.cappedByRemainingAllowance,
        subscriptionAllowance: summarizeAllowance(allowance),
        transactionOrder: [
          "subscription_pull_wallet_to_earn_vault",
          "kamino_route_policy_top_up_from_earn_vault",
        ],
        signers: {
          pull: policyKeypair.publicKey.toBase58(),
          kaminoTopUpFeePayer: topUpFeePayer.toBase58(),
          kaminoTopUp:
            readRecord(topUpDryRun.json?.policyDeposit)?.signer?.toString() ??
            null,
        },
        topUpFeePayerSafety: {
          feePayer: topUpFeePayer.toBase58(),
          balanceLamports: null,
          minimumLamports: AUTODEPOSIT_TOP_UP_FEE_PAYER_MIN_LAMPORTS,
          commitment: DEFAULT_COMMITMENT,
          checked: false,
        },
        policies: {
          sweep: target.sweepPolicyAccount,
          kaminoTopUp: target.routePolicyAccount,
          kaminoTopUpSeed: target.routePolicySeed.toString(),
          kaminoTopUpRouteModes: target.routeModes,
        },
        simulations: {
          pull: summarizeSimulation(pullSimulation),
          kaminoTopUp: summarizeTopUpResult(topUpDryRun),
        },
        lotClaim: lotClaim ? summarizeLotClaim(lotClaim) : null,
        sendsTransactions: options.execute,
      };

      if (!options.execute) {
        console.log(JSON.stringify(plan, null, 2));
        return;
      }
      if (!lease) {
        throw new Error(
          "Durable autodeposit execution requires a fenced vault lease.",
        );
      }

      const topUpFeePayerSafety = await runAfterExecutablePreflight({
        pullSimulation,
        topUpDryRun,
        run: () =>
          assertFeePayerSol({
            connection,
            feePayer: topUpFeePayer,
          }),
      });
      durableStore = new NeonDurableAutodepositStore({
        appModules,
        databaseUrl,
        target,
        claimToken: lotClaim?.claimToken ?? options.claimToken,
        scheduledSlotId: options.scheduledSlotId,
        requestedAmountRaw: executionAmountRaw,
        topUpReserve,
        topUpMarket,
        topUpLiquidityMint,
      });
      const durableExecution = await executeDurableSaga({
        connection,
        lease,
        pullPrepared: pull.prepared,
        policyKeypair,
        rpcUrl,
        store: durableStore,
        target,
        appModules,
      });
      pullSent = durableExecution.pullSignature !== null;
      const walletPostPullRaw = await getTokenBalanceRaw(
        connection,
        walletUsdcAta,
      );
      const vaultPostExecutionRaw = await getTokenBalanceRaw(
        connection,
        vaultUsdcAta,
      );
      if (durableExecution.lifecycleState !== "completed") {
        console.log(
          JSON.stringify(
            {
              ...plan,
              status: durableExecution.lifecycleState,
              executionId: durableExecution.id,
              confirmedPullAmountRaw:
                durableExecution.confirmedPullAmountRaw?.toString() ?? null,
              reservedAmountRaw: durableExecution.reservedAmountRaw.toString(),
              signatures: {
                pull: durableExecution.pullSignature,
                kaminoDeposit: durableExecution.successfulTopUpSignature,
              },
              walletPostPullRaw: walletPostPullRaw.toString(),
              vaultPostExecutionRaw: vaultPostExecutionRaw.toString(),
              topUpFeePayerSafety,
            },
            null,
            2,
          ),
        );
        process.exitCode = 1;
        return;
      }
      const solanaWeekNotify = await notifySolanaWeekSweep({
        PublicKeyCtor,
        ownerWalletAddress: target.wallet,
      });
      logSolanaWeekNotifyResult(solanaWeekNotify);

      console.log(
        JSON.stringify(
          {
            ...plan,
            status: "executed",
            executionId: durableExecution.id,
            signatures: {
              pull: durableExecution.pullSignature,
              kaminoDeposit: durableExecution.successfulTopUpSignature,
            },
            walletPostPullRaw: walletPostPullRaw.toString(),
            vaultPostExecutionRaw: vaultPostExecutionRaw.toString(),
            confirmedPullAmountRaw:
              durableExecution.confirmedPullAmountRaw?.toString() ?? null,
            topUpFeePayerSafety,
            solanaWeekNotify,
          },
          null,
          2,
        ),
      );
    } catch (error) {
      const persistedExecution = durableStore
        ? await durableStore.loadExecution()
        : null;
      if (
        !pullSent &&
        !persistedExecution &&
        lotClaim?.status === "selected" &&
        lotClaim.claimToken
      ) {
        await releaseAutodepositLotClaim({
          neon: appModules.neon,
          databaseUrl,
          claimToken: lotClaim.claimToken,
        });
      }
      throw error;
    }
  };

  if (options.execute) {
    await withVaultLease({
      neon: appModules.neon,
      databaseUrl,
      cluster: target.cluster,
      vaultPubkey: target.vaultPubkey,
      claimToken: options.claimToken,
      scheduledSlotId: options.scheduledSlotId,
      run: runFlow,
    });
  } else {
    await runFlow(null);
  }
}

if (import.meta.main) {
  main().catch((error) => {
    console.error(
      JSON.stringify(
        {
          status: "error",
          error: redactSensitiveText(
            error instanceof Error ? error.message : String(error),
          ),
        },
        null,
        2
      )
    );
    process.exitCode = 1;
  });
}
