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

type EligibleTarget = {
  id: bigint;
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

export function isMissingAutodepositTokenDelegateFailure(
  error: unknown
): boolean {
  const message = error instanceof Error ? error.message : String(error);
  return (
    message.includes("Autodeposit pull simulation failed") &&
    message.includes("Program log: Error: owner does not match")
  );
}
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
      rp.policy_seed AS route_policy_seed,
      rp.route_modes AS route_modes,
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

async function completeAutodepositLotClaim(args: {
  neon: AppModules["neon"];
  databaseUrl: string;
  claimToken: string;
  executionId: string;
}) {
  const sql = args.neon(args.databaseUrl);
  await sql`
    WITH matched_lots AS (
      SELECT i.lot_id, i.amount_raw
      FROM loyal_yield.balance_sweep_lot_claim_items i
      JOIN loyal_yield.balance_sweep_surplus_lots l
        ON l.id = i.lot_id
      JOIN loyal_yield.balance_sweep_wallet_balance_events e
        ON e.event_id = l.source_event_id
      JOIN loyal_yield.balance_sweep_lot_claims c
        ON c.claim_token = i.claim_token
      JOIN loyal_yield.balance_sweep_targets t
        ON t.id = c.target_id
      WHERE i.claim_token = ${args.claimToken}
        AND l.target_id = c.target_id
        AND e.mint = t.token_mint
        AND t.token_mint = ${USDC_MINT_ADDRESS}
    ),
    inserted AS (
      INSERT INTO loyal_yield.balance_sweep_execution_lots
        (execution_id, lot_id, amount_raw)
      SELECT ${args.executionId}, lot_id, amount_raw
      FROM matched_lots
      ON CONFLICT (execution_id, lot_id) DO NOTHING
      RETURNING lot_id
    ),
    updated_claim AS (
      UPDATE loyal_yield.balance_sweep_lot_claims
      SET status = 'executed',
          execution_id = ${args.executionId},
          updated_at = now()
      WHERE claim_token = ${args.claimToken}
        AND status = 'selected'
        AND EXISTS (SELECT 1 FROM matched_lots)
      RETURNING claim_token
    )
    UPDATE loyal_yield.balance_sweep_scheduled_slots
    SET status = 'executed',
        execution_id = ${args.executionId},
        updated_at = now()
    WHERE claim_token IN (SELECT claim_token FROM updated_claim)
  `;
}

async function releaseAutodepositLotClaim(args: {
  neon: AppModules["neon"];
  databaseUrl: string;
  claimToken: string;
  lastError: string;
  pauseTargetForMissingDelegate: boolean;
}) {
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
      SET remaining_amount_raw = LEAST(
            l.original_amount_raw,
            l.remaining_amount_raw + i.amount_raw
          ),
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
          updated_at = now()
      WHERE claim_token = (SELECT claim_token FROM selected_claim)
        AND EXISTS (SELECT 1 FROM restored)
      RETURNING claim_token
    )
    UPDATE loyal_yield.balance_sweep_scheduled_slots
    SET status = 'failed',
        claim_token = NULL,
        last_error = ${args.lastError},
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

async function sendPreparedOperation(args: {
  compilePreparedOperation: AppModules["compilePreparedOperation"];
  connection: Connection;
  prepared: PreparedOperation;
  signers: Keypair[];
}): Promise<{ signature: string; slot: bigint }> {
  const latestBlockhash = await args.connection.getLatestBlockhash(
    DEFAULT_COMMITMENT
  );
  const transaction = args.compilePreparedOperation({
    prepared: args.prepared,
    blockhash: latestBlockhash.blockhash,
  });
  transaction.sign(args.signers);
  const signature = await args.connection.sendRawTransaction(
    transaction.serialize()
  );
  const confirmation = await args.connection.confirmTransaction(
    {
      signature,
      blockhash: latestBlockhash.blockhash,
      lastValidBlockHeight: latestBlockhash.lastValidBlockHeight,
    },
    DEFAULT_COMMITMENT
  );
  if (confirmation.value.err) {
    throw new Error(
      `Transaction ${signature} failed: ${JSON.stringify(confirmation.value.err)}`
    );
  }
  const parsed = await args.connection.getTransaction(signature, {
    commitment: DEFAULT_COMMITMENT,
    maxSupportedTransactionVersion: 0,
  });
  return { signature, slot: BigInt(parsed?.slot ?? 0) };
}

type SameMintTopUpResult = {
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

function requireTopUpExecution(result: SameMintTopUpResult): {
  signature: string;
  confirmedSlot: bigint;
} {
  if (result.json?.status !== "initial_deposit_executed") {
    throw new Error(
      `same-mint Kamino top-up did not report execution: ${JSON.stringify(
        summarizeTopUpResult(result)
      )}`
    );
  }
  const policyDepositTransaction = readRecord(
    result.json?.policyDepositTransaction
  );
  const signature = policyDepositTransaction?.signature?.toString();
  const confirmedSlot = policyDepositTransaction?.confirmedSlot?.toString();
  if (!signature || !confirmedSlot) {
    throw new Error(
      `same-mint Kamino top-up result is missing policy deposit signature/slot: ${JSON.stringify(
        summarizeTopUpResult(result)
      )}`
    );
  }
  return { signature, confirmedSlot: BigInt(confirmedSlot) };
}

function readTopUpObservedPosition(
  result: SameMintTopUpResult,
  reserve: string
): { amountRaw: bigint; observedSlot: bigint | null } | null {
  const reconcile = readRecord(result.json?.postChainReconcile);
  const positions = reconcile?.positions;
  if (!Array.isArray(positions)) {
    return null;
  }
  const position = positions
    .map((item) => readRecord(item))
    .find((item) => item?.reserve?.toString() === reserve);
  const amountRaw = position?.amountRaw?.toString();
  if (!amountRaw) {
    return null;
  }
  const observedSlot = reconcile?.observedSlot?.toString();
  return {
    amountRaw: BigInt(amountRaw),
    observedSlot: observedSlot ? BigInt(observedSlot) : null,
  };
}

function readRecord(value: unknown): Record<string, unknown> | null {
  return value && typeof value === "object" && !Array.isArray(value)
    ? (value as Record<string, unknown>)
    : null;
}

function tailLines(value: string, count: number): string[] {
  return value.trim().split(/\r?\n/).filter(Boolean).slice(-count);
}

async function recordPullExecution(args: {
  neon: AppModules["neon"];
  databaseUrl: string;
  target: EligibleTarget;
  signature: string;
  slot: bigint;
  amountRaw: bigint;
  sourcePreBalanceRaw: bigint;
  sourcePostBalanceRaw: bigint;
  destinationPreBalanceRaw: bigint;
  destinationPostBalanceRaw: bigint;
}): Promise<{ dedupeKey: string; executionId: string }> {
  const sql = args.neon(args.databaseUrl);
  const dedupeKey = `${args.target.id.toString()}:autodeposit-pull:${args.signature}`;
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
        ${JSON.stringify({ source: "single-vault-autodeposit-executor" })}::jsonb,
        ${JSON.stringify({ sequence: "subscription_pull_then_kamino_deposit" })}::jsonb,
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

async function updateExecutionEvidence(args: {
  decodedEvidence: Record<string, unknown>;
  dedupeKey: string;
  neon: AppModules["neon"];
  databaseUrl: string;
}) {
  const sql = args.neon(args.databaseUrl);
  await sql`
    UPDATE loyal_yield.balance_sweep_executions
    SET
      decoded_evidence = COALESCE(decoded_evidence, '{}'::jsonb) ||
        ${JSON.stringify(args.decodedEvidence)}::jsonb,
      decoded_at = now()
    WHERE dedupe_key = ${args.dedupeKey}
  `;
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
  scheduledSlotId: bigint | null;
  target: EligibleTarget;
  targetReserve: string;
}): Promise<{
  status: "duplicate" | "inserted";
  depositId: string;
  positionId: string | null;
}> {
  const sql = args.appModules.neon(args.databaseUrl);
  const now = new Date();
  const depositRows = await sql`
    INSERT INTO loyal_yield.user_yield_position_deposits (
      deposit_signature,
      policy_signature,
      confirmed_slot,
      wallet_address,
      smart_account_address,
      settings,
      vault_index,
      vault_pubkey,
      policy_id,
      policy_account,
      policy_seed,
      target_reserve,
      market,
      liquidity_mint,
      target_supply_apy_bps,
      deposit_mint,
      principal_amount_raw,
      balance_sweep_execution_id,
      balance_sweep_scheduled_slot_id,
      confirmed_at,
      created_at
    )
    VALUES (
      ${args.depositSignature},
      ${args.policySignature},
      ${args.depositSlot.toString()},
      ${args.target.wallet},
      ${args.target.vaultPubkey},
      ${args.target.settings},
      ${args.target.vaultIndex},
      ${args.target.vaultPubkey},
      ${args.target.routePolicySeed.toString()},
      ${args.target.routePolicyAccount},
      ${args.target.routePolicySeed.toString()},
      ${args.targetReserve},
      ${args.market},
      ${args.liquidityMint},
      ${null},
      ${args.liquidityMint},
      ${args.amountRaw.toString()},
      ${args.balanceSweepExecutionId},
      ${args.scheduledSlotId?.toString() ?? null},
      ${now},
      ${now}
    )
    ON CONFLICT (deposit_signature) DO NOTHING
    RETURNING id
  `;

  const insertedDeposit = depositRows[0] as Record<string, unknown> | undefined;
  if (!insertedDeposit) {
    const depositRows = await sql`
      UPDATE loyal_yield.user_yield_position_deposits
      SET
        balance_sweep_execution_id = COALESCE(
          balance_sweep_execution_id,
          ${args.balanceSweepExecutionId}
        ),
        balance_sweep_scheduled_slot_id = COALESCE(
          balance_sweep_scheduled_slot_id,
          ${args.scheduledSlotId?.toString() ?? null}
        )
      WHERE deposit_signature = ${args.depositSignature}
        AND (
          balance_sweep_execution_id IS NULL
          OR balance_sweep_execution_id = ${args.balanceSweepExecutionId}
        )
      RETURNING id
    `;
    const deposit = depositRows[0] as Record<string, unknown> | undefined;
    if (!deposit) {
      throw new Error(
        "Autodeposit yield deposit is already linked to another sweep execution."
      );
    }
    const existingRows = await sql`
      SELECT position_id
      FROM loyal_yield.user_yield_position_holding_events
      WHERE source_signature = ${args.depositSignature}
      ORDER BY id DESC
      LIMIT 1
    `;
    const existing = existingRows[0] as Record<string, unknown> | undefined;
    return {
      status: "duplicate",
      depositId: readRequiredString(deposit.id, "deposit.id"),
      positionId: existing?.position_id?.toString() ?? null,
    };
  }

  const existingRows = await sql`
    SELECT *
    FROM loyal_yield.user_yield_positions
    WHERE settings = ${args.target.settings}
      AND vault_index = ${args.target.vaultIndex}
      AND wallet_address = ${args.target.wallet}
      AND status = 'active'
    ORDER BY updated_at DESC, id DESC
    LIMIT 1
  `;
  const existing = existingRows[0] as Record<string, unknown> | undefined;
  let positionId: string;
  let eventType: "deposit_initialized" | "deposit_top_up";
  let nextAmountRaw: bigint;
  let nextPrincipalRaw: bigint;
  let holdingDeltaRaw: bigint | null;

  if (existing) {
    positionId = readRequiredString(existing.id, "position.id");
    if (readRequiredString(existing.status, "position.status") !== "active") {
      throw new Error("Autodeposit top-up requires an active yield position.");
    }
    const currentAmountRaw = BigInt(
      readRequiredString(existing.current_amount_raw, "current_amount_raw")
    );
    const principalAmountRaw = BigInt(
      readRequiredString(existing.principal_amount_raw, "principal_amount_raw")
    );
    const sameCurrentHolding =
      readRequiredString(existing.current_reserve, "current_reserve") ===
        args.targetReserve &&
      readRequiredString(
        existing.current_liquidity_mint,
        "current_liquidity_mint"
      ) === args.liquidityMint;
    eventType = "deposit_top_up";
    nextAmountRaw =
      sameCurrentHolding && args.observedCurrentAmountRaw !== null
        ? args.observedCurrentAmountRaw
        : sameCurrentHolding
          ? currentAmountRaw + args.amountRaw
          : currentAmountRaw;
    nextPrincipalRaw = principalAmountRaw + args.amountRaw;
    holdingDeltaRaw = sameCurrentHolding ? nextAmountRaw - currentAmountRaw : null;

    await sql`
      UPDATE loyal_yield.user_yield_positions
      SET
        deposit_mint = ${args.liquidityMint},
        initial_liquidity_mint = ${args.liquidityMint},
        initial_market = ${args.market},
        last_confirmed_slot = ${args.depositSlot.toString()},
        last_deposit_signature = ${args.depositSignature},
        policy_account = ${args.target.routePolicyAccount},
        policy_id = ${args.target.routePolicySeed.toString()},
        policy_seed = ${args.target.routePolicySeed.toString()},
        principal_amount_raw = ${nextPrincipalRaw.toString()},
        smart_account_address = ${args.target.vaultPubkey},
        status = 'active',
        updated_at = ${now},
        vault_pubkey = ${args.target.vaultPubkey},
        wallet_address = ${args.target.wallet}
      WHERE id = ${positionId}
    `;
  } else {
    eventType = "deposit_initialized";
    nextAmountRaw = args.observedCurrentAmountRaw ?? args.amountRaw;
    nextPrincipalRaw = args.amountRaw;
    holdingDeltaRaw = args.amountRaw;
    const positionRows = await sql`
      INSERT INTO loyal_yield.user_yield_positions (
        wallet_address,
        smart_account_address,
        settings,
        vault_index,
        vault_pubkey,
        policy_id,
        policy_account,
        policy_seed,
        initial_reserve,
        initial_market,
        initial_liquidity_mint,
        initial_supply_apy_bps,
        deposit_mint,
        principal_amount_raw,
        current_reserve,
        current_market,
        current_liquidity_mint,
        current_amount_raw,
        current_observed_slot,
        current_observed_at,
        first_deposit_signature,
        last_deposit_signature,
        last_confirmed_slot,
        status,
        created_at,
        updated_at
      )
      VALUES (
        ${args.target.wallet},
        ${args.target.vaultPubkey},
        ${args.target.settings},
        ${args.target.vaultIndex},
        ${args.target.vaultPubkey},
        ${args.target.routePolicySeed.toString()},
        ${args.target.routePolicyAccount},
        ${args.target.routePolicySeed.toString()},
        ${args.targetReserve},
        ${args.market},
        ${args.liquidityMint},
        ${null},
        ${args.liquidityMint},
        ${args.amountRaw.toString()},
        ${args.targetReserve},
        ${args.market},
        ${args.liquidityMint},
        ${args.amountRaw.toString()},
        ${args.depositSlot.toString()},
        ${now},
        ${args.depositSignature},
        ${args.depositSignature},
        ${args.depositSlot.toString()},
        'active',
        ${now},
        ${now}
      )
      RETURNING id
    `;
    positionId = readRequiredString(
      (positionRows[0] as Record<string, unknown> | undefined)?.id,
      "position.id"
    );
  }

  const eventRows = await sql`
    INSERT INTO loyal_yield.user_yield_position_holding_events (
      position_id,
      event_type,
      reserve,
      market,
      liquidity_mint,
      amount_raw,
      principal_delta_raw,
      holding_delta_raw,
      observed_slot,
      observed_at,
      source_signature,
      source_deposit_id,
      created_at
    )
    VALUES (
      ${positionId},
      ${eventType},
      ${args.targetReserve},
      ${args.market},
      ${args.liquidityMint},
      ${nextAmountRaw.toString()},
      ${args.amountRaw.toString()},
      ${holdingDeltaRaw?.toString() ?? null},
      ${(args.observedSlot ?? args.depositSlot).toString()},
      ${now},
      ${args.depositSignature},
      ${readRequiredString(insertedDeposit.id, "deposit.id")},
      ${now}
    )
    RETURNING id
  `;
  const eventId = readRequiredString(
    (eventRows[0] as Record<string, unknown> | undefined)?.id,
    "holding_event.id"
  );

  await sql`
    UPDATE loyal_yield.user_yield_positions
    SET
      current_amount_raw = ${nextAmountRaw.toString()},
      current_liquidity_mint = ${args.liquidityMint},
      current_market = ${args.market},
      current_observed_at = ${now},
      current_observed_slot = ${(args.observedSlot ?? args.depositSlot).toString()},
      current_reserve = ${args.targetReserve},
      last_holding_event_id = ${eventId},
      last_confirmed_slot = ${args.depositSlot.toString()},
      last_deposit_signature = ${args.depositSignature},
      principal_amount_raw = ${nextPrincipalRaw.toString()},
      status = 'active',
      updated_at = ${now}
    WHERE id = ${positionId}
  `;

  return {
    status: "inserted",
    depositId: readRequiredString(insertedDeposit.id, "deposit.id"),
    positionId,
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

async function markAutodepositExecutionFailed(args: {
  neon: AppModules["neon"];
  databaseUrl: string;
  executionId: string;
  scheduledSlotId: bigint | null;
  failureCode: "kamino_top_up_failed" | "yield_persistence_failed";
}) {
  if (args.scheduledSlotId === null) {
    return;
  }
  const sql = args.neon(args.databaseUrl);
  await sql`
    SELECT loyal_yield.mark_autodeposit_execution_failed(
      ${args.executionId},
      ${args.scheduledSlotId.toString()},
      ${args.failureCode}
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

function assertExecutablePreflight(args: {
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
}

async function main() {
  const appModules = await loadAppModules();
  const PublicKeyCtor = appModules.PublicKey;
  const options = parseOptions(Bun.argv.slice(2));
  const databaseUrl = requireEnv("NEON_DATABASE_URL");
  const rpcUrl = requireEnv("SOLANA_RPC_URL");
  const policyKeypair = parseKeypairSecretWith(
    appModules.Keypair,
    requireEnv("POLICY_KEYPAIR")
  );
  const programId = new PublicKeyCtor(
    process.env.LOYAL_SMART_ACCOUNTS_PROGRAM_ID ?? appModules.PROGRAM_ADDRESS
  );
  const connection = new Connection(rpcUrl, DEFAULT_COMMITMENT);

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
          persistedWalletBalanceFloorRaw: target.walletBalanceFloorRaw.toString(),
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
        throw new Error("--claim-token is required when executing with lot claims.");
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

  let pullSent = false;
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
    const topUpReserve =
      target.currentReserve ?? defaultEarnTarget.reserve.toBase58();
    const topUpMarket =
      target.currentMarket ?? defaultEarnTarget.market.toBase58();
    const topUpLiquidityMint =
      target.currentLiquidityMint ?? pull.persistence.liquidityMint;
    if (topUpLiquidityMint !== pull.persistence.liquidityMint) {
      throw new Error(
        `Autodeposit top-up liquidity mint ${topUpLiquidityMint} does not match pulled mint ${pull.persistence.liquidityMint}.`
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
        source: target.currentReserve ? "active_yield_position" : "default_earn_target",
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

    assertExecutablePreflight({ pullSimulation, topUpDryRun });

    const { result: pullSend, safety: topUpFeePayerSafety } =
      await runAfterFeePayerSolSafety({
        connection,
        feePayer: topUpFeePayer,
        run: () =>
          sendPreparedOperation({
            compilePreparedOperation: appModules.compilePreparedOperation,
            connection,
            prepared: pull.prepared,
            signers: [policyKeypair],
          }),
      });
    pullSent = true;
    const walletPostPullRaw = await getTokenBalanceRaw(connection, walletUsdcAta);
    const vaultPostPullRaw = await getTokenBalanceRaw(connection, vaultUsdcAta);
    const executionRecord = await recordPullExecution({
      neon: appModules.neon,
      databaseUrl,
      target,
      signature: pullSend.signature,
      slot: pullSend.slot,
      amountRaw: executionAmountRaw,
      sourcePreBalanceRaw: walletBalanceRaw,
      sourcePostBalanceRaw: walletPostPullRaw,
      destinationPreBalanceRaw: vaultPreBalanceRaw,
      destinationPostBalanceRaw: vaultPostPullRaw,
    });
    if (lotClaim?.status === "selected" && lotClaim.claimToken) {
      await completeAutodepositLotClaim({
        neon: appModules.neon,
        databaseUrl,
        claimToken: lotClaim.claimToken,
        executionId: executionRecord.executionId,
      });
    }

    let topUpExecute: SameMintTopUpResult;
    let topUpExecution: { signature: string; confirmedSlot: bigint };
    try {
      topUpExecute = await runSameMintReserveTopUp({
        amountRaw: executionAmountRaw,
        execute: true,
        reserve: topUpReserve,
        rpcUrl,
        target,
      });
      topUpExecution = requireTopUpExecution(topUpExecute);
    } catch (error) {
      await markAutodepositExecutionFailed({
        neon: appModules.neon,
        databaseUrl,
        executionId: executionRecord.executionId,
        scheduledSlotId: options.scheduledSlotId,
        failureCode: "kamino_top_up_failed",
      });
      await updateExecutionEvidence({
        neon: appModules.neon,
        databaseUrl,
        dedupeKey: executionRecord.dedupeKey,
        decodedEvidence: {
          status: "partial_executed_pull_top_up_blocked",
          kaminoTopUpError:
            error instanceof Error ? error.message : String(error),
          kaminoTopUpDryRun: summarizeTopUpResult(topUpDryRun),
          topUpFeePayerSafety,
          vaultPostPullRaw: vaultPostPullRaw.toString(),
        },
      });
      console.log(
        JSON.stringify(
          {
            ...plan,
            status: "partial_executed_pull_top_up_blocked",
            signatures: {
              pull: pullSend.signature,
            },
            confirmedSlots: {
              pull: pullSend.slot.toString(),
            },
            walletPostPullRaw: walletPostPullRaw.toString(),
            vaultPostPullRaw: vaultPostPullRaw.toString(),
            topUpFeePayerSafety,
            kaminoTopUpError:
              error instanceof Error ? error.message : String(error),
          },
          null,
          2
        )
      );
      process.exitCode = 1;
      return;
    }

    const topUpObservedPosition = readTopUpObservedPosition(
      topUpExecute,
      topUpReserve
    );
    const vaultPostDepositRaw = await getTokenBalanceRaw(connection, vaultUsdcAta);
    let yieldDepositRecord: Awaited<
      ReturnType<typeof recordAutodepositYieldDeposit>
    >;
    try {
      yieldDepositRecord = await recordAutodepositYieldDeposit({
        amountRaw: executionAmountRaw,
        appModules,
        balanceSweepExecutionId: executionRecord.executionId,
        databaseUrl,
        depositSignature: topUpExecution.signature,
        depositSlot: topUpExecution.confirmedSlot,
        liquidityMint: topUpLiquidityMint,
        market: topUpMarket,
        observedCurrentAmountRaw: topUpObservedPosition?.amountRaw ?? null,
        observedSlot: topUpObservedPosition?.observedSlot ?? null,
        policySignature: topUpExecution.signature,
        scheduledSlotId: options.scheduledSlotId,
        target,
        targetReserve: topUpReserve,
      });
      if (options.scheduledSlotId !== null) {
        await markAutodepositExecutionCompleted({
          neon: appModules.neon,
          databaseUrl,
          executionId: executionRecord.executionId,
          scheduledSlotId: options.scheduledSlotId,
          kaminoDepositSignature: topUpExecution.signature,
        });
      }
    } catch (error) {
      await markAutodepositExecutionFailed({
        neon: appModules.neon,
        databaseUrl,
        executionId: executionRecord.executionId,
        scheduledSlotId: options.scheduledSlotId,
        failureCode: "yield_persistence_failed",
      });
      await updateExecutionEvidence({
        neon: appModules.neon,
        databaseUrl,
        dedupeKey: executionRecord.dedupeKey,
        decodedEvidence: {
          status: "partial_executed_pull_yield_persistence_failed",
          kaminoDepositSignature: topUpExecution.signature,
          kaminoDepositSlot: topUpExecution.confirmedSlot.toString(),
        },
      });
      throw error;
    }
    await updateExecutionEvidence({
      neon: appModules.neon,
      databaseUrl,
      dedupeKey: executionRecord.dedupeKey,
      decodedEvidence: {
        status: "executed",
        kaminoDepositSignature: topUpExecution.signature,
        kaminoDepositSlot: topUpExecution.confirmedSlot.toString(),
        kaminoTopUp: summarizeTopUpResult(topUpExecute),
        kaminoTopUpObservedPosition: topUpObservedPosition
          ? {
              amountRaw: topUpObservedPosition.amountRaw.toString(),
              observedSlot:
                topUpObservedPosition.observedSlot?.toString() ?? null,
            }
          : null,
        topUpFeePayerSafety,
        vaultPostDepositRaw: vaultPostDepositRaw.toString(),
        yieldDepositRecord,
      },
    });
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
          signatures: {
            pull: pullSend.signature,
            kaminoDeposit: topUpExecution.signature,
          },
          confirmedSlots: {
            pull: pullSend.slot.toString(),
            kaminoDeposit: topUpExecution.confirmedSlot.toString(),
          },
          walletPostPullRaw: walletPostPullRaw.toString(),
          vaultPostPullRaw: vaultPostPullRaw.toString(),
          vaultPostDepositRaw: vaultPostDepositRaw.toString(),
          topUpFeePayerSafety,
          kaminoTopUpExecution: summarizeTopUpResult(topUpExecute),
          yieldDepositRecord,
          solanaWeekNotify,
        },
        null,
        2
      )
    );
  } catch (error) {
    if (!pullSent && lotClaim?.status === "selected" && lotClaim.claimToken) {
      const lastError = error instanceof Error ? error.message : String(error);
      await releaseAutodepositLotClaim({
        neon: appModules.neon,
        databaseUrl,
        claimToken: lotClaim.claimToken,
        lastError: lastError.slice(0, 4_000),
        pauseTargetForMissingDelegate:
          isMissingAutodepositTokenDelegateFailure(error),
      });
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
    process.exitCode = 1;
  });
}
