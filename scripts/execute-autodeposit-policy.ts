import {
  Connection,
  Keypair,
  PublicKey,
  VersionedTransaction,
  type AddressLookupTableAccount,
  type TransactionInstruction,
} from "@solana/web3.js";
import bs58 from "bs58";

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

type PreparedEarnDeposit = {
  prepared: PreparedOperation;
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
  prepareEarnUsdcDeposit: (args: {
    amountRaw: bigint;
    cluster: string;
    feePayer: PublicKey;
    initializeYieldRoutingPolicy: boolean;
    policySigner: PublicKey;
    settingsPda: PublicKey;
    walletAddress: PublicKey;
    yieldRoutingPolicy: {
      account: PublicKey;
      seed: bigint;
    };
  }) => Promise<PreparedEarnDeposit>;
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
  createAssociatedTokenAccountIdempotentInstruction: (
    payer: PublicKey,
    associatedToken: PublicKey,
    owner: PublicKey,
    mint: PublicKey,
    programId?: PublicKey
  ) => TransactionInstruction;
  createSmartAccountVaultsClient: (config: {
    connection: Connection;
    programId: PublicKey;
  }) => SmartAccountVaultsClient;
  decodeTransferCheckedInstruction: (
    instruction: TransactionInstruction,
    programId?: PublicKey
  ) => {
    keys: {
      source: { pubkey: PublicKey };
      destination: { pubkey: PublicKey };
      owner: { pubkey: PublicKey };
    };
  };
  getAssociatedTokenAddressSync: (
    mint: PublicKey,
    owner: PublicKey,
    allowOwnerOffCurve?: boolean,
    programId?: PublicKey
  ) => PublicKey;
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
  TOKEN_PROGRAM_ID: PublicKey;
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
  targetId: bigint | null;
};

type EligibleTarget = {
  id: bigint;
  settings: string;
  wallet: string;
  walletUsdcAta: string;
  vaultPubkey: string;
  vaultUsdcAta: string;
  sweepPolicyAccount: string;
  routePolicyAccount: string;
  routePolicySeed: bigint;
  recurringDelegation: string;
  walletBalanceFloorRaw: bigint;
  maxAmountPerPeriodRaw: bigint | null;
  periodLengthSeconds: bigint | null;
  startTimestamp: bigint | null;
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

const DEFAULT_COMMITMENT = "confirmed";
const USDC_DECIMALS = 6;
const PRE_SEND_FAILURE_RETRY_DELAY_SECONDS = 5 * 60;

async function loadAppModules(): Promise<AppModules> {
  const [
    neonModule,
    splTokenModule,
    smartAccountVaultsModule,
    smartAccountsCoreModule,
    smartAccountsModule,
    loyalActionsModule,
  ] = await Promise.all([
    import("@neondatabase/serverless"),
    import("@solana/spl-token"),
    import("@loyal-labs/smart-account-vaults"),
    import("@loyal-labs/loyal-smart-accounts-core"),
    import("@loyal-labs/loyal-smart-accounts"),
    import("@loyal/actions"),
  ]);

  return {
    Keypair,
    PublicKey,
    compilePreparedOperation: smartAccountsCoreModule.compilePreparedOperation,
    createAssociatedTokenAccountIdempotentInstruction:
      splTokenModule.createAssociatedTokenAccountIdempotentInstruction,
    createSmartAccountVaultsClient:
      smartAccountVaultsModule.createSmartAccountVaultsClient as unknown as AppModules["createSmartAccountVaultsClient"],
    decodeTransferCheckedInstruction: splTokenModule.decodeTransferCheckedInstruction,
    getAssociatedTokenAddressSync: splTokenModule.getAssociatedTokenAddressSync,
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
    TOKEN_PROGRAM_ID: splTokenModule.TOKEN_PROGRAM_ID,
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

  return { claimToken, execute, overrideFloorRaw, requireLotClaim, targetId };
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
      t.wallet,
      t.wallet_usdc_ata,
      t.vault_pubkey,
      t.vault_usdc_ata,
      t.policy_account AS sweep_policy_account,
      t.recurring_delegation,
      t.wallet_balance_floor_raw,
      t.max_amount_per_period,
      t.period_length_seconds,
      t.start_timestamp,
      rp.policy_account AS route_policy_account,
      rp.policy_seed AS route_policy_seed
    FROM loyal_yield.balance_sweep_targets t
    LEFT JOIN LATERAL (
      SELECT policy_account, policy_seed
      FROM loyal_yield.route_policies rp
      WHERE rp.settings = t.settings
        AND rp.vault_index = t.vault_index
        AND rp.active
      ORDER BY rp.last_seen_slot DESC, rp.id DESC
      LIMIT 1
    ) rp ON TRUE
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
  const routePolicyAccount = readNullableString(row.route_policy_account);
  if (!routePolicyAccount) {
    throw new Error(
      `Autodeposit target ${row.id} does not have an active Earn route policy.`
    );
  }

  return {
    id: BigInt(readRequiredString(row.id, "id")),
    settings: readRequiredString(row.settings, "settings"),
    wallet: readRequiredString(row.wallet, "wallet"),
    walletUsdcAta: readRequiredString(row.wallet_usdc_ata, "wallet_usdc_ata"),
    vaultPubkey: readRequiredString(row.vault_pubkey, "vault_pubkey"),
    vaultUsdcAta: readRequiredString(row.vault_usdc_ata, "vault_usdc_ata"),
    sweepPolicyAccount: readRequiredString(
      row.sweep_policy_account,
      "sweep_policy_account"
    ),
    routePolicyAccount,
    routePolicySeed: BigInt(readRequiredString(row.route_policy_seed, "route_policy_seed")),
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
  };
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
  claimToken: string;
  walletBalanceRaw: bigint;
  walletBalanceFloorRaw: bigint;
  remainingAllowanceRaw: bigint | null;
}): Promise<LotClaimResult> {
  const sql = args.neon(args.databaseUrl);
  const rows = await sql`
    WITH existing_claim AS (
      SELECT claim_token, target_id, amount_raw, status::text AS status, stale_check_event_id
      FROM loyal_yield.balance_sweep_lot_claims
      WHERE claim_token = ${args.claimToken}
      FOR UPDATE
    ),
    target_guard AS (
      SELECT id
      FROM loyal_yield.balance_sweep_targets
      WHERE id = ${args.targetId.toString()}
        AND active
        AND lifecycle_status = 'active'
      FOR UPDATE
    ),
    stale AS (
      SELECT COALESCE(MAX(event_id), 0)::bigint AS event_id
      FROM loyal_yield.balance_sweep_wallet_balance_events
      WHERE target_id = ${args.targetId.toString()}
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
      WHERE l.target_id = ${args.targetId.toString()}
        AND l.status = 'open'
        AND l.remaining_amount_raw > 0
        AND l.eligible_after <= now()
        AND EXISTS (SELECT 1 FROM target_guard)
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
    active_claim AS (
      SELECT * FROM existing_claim
      UNION ALL
      SELECT * FROM inserted_claim
      LIMIT 1
    ),
    active_items AS (
      SELECT lot_id, amount_raw
      FROM loyal_yield.balance_sweep_lot_claim_items
      WHERE claim_token = (SELECT claim_token FROM active_claim)
      ORDER BY lot_id ASC
    ),
    noop_reason AS (
      SELECT CASE
        WHEN NOT EXISTS (SELECT 1 FROM target_guard) THEN 'target_not_active'
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
    WITH inserted AS (
      INSERT INTO loyal_yield.balance_sweep_execution_lots
        (execution_id, lot_id, amount_raw)
      SELECT ${args.executionId}, lot_id, amount_raw
      FROM loyal_yield.balance_sweep_lot_claim_items
      WHERE claim_token = ${args.claimToken}
      ON CONFLICT (execution_id, lot_id) DO NOTHING
      RETURNING lot_id
    )
    UPDATE loyal_yield.balance_sweep_lot_claims
    SET status = 'executed',
        execution_id = ${args.executionId},
        updated_at = now()
    WHERE claim_token = ${args.claimToken}
      AND status = 'selected'
  `;
}

async function releaseAutodepositLotClaim(args: {
  neon: AppModules["neon"];
  databaseUrl: string;
  claimToken: string;
}) {
  const sql = args.neon(args.databaseUrl);
  await sql`
    WITH selected_claim AS (
      SELECT claim_token
      FROM loyal_yield.balance_sweep_lot_claims
      WHERE claim_token = ${args.claimToken}
        AND status = 'selected'
    ),
    restored AS (
      UPDATE loyal_yield.balance_sweep_surplus_lots l
      SET remaining_amount_raw = l.remaining_amount_raw + i.amount_raw,
          status = 'open',
          eligible_after = now() + (${PRE_SEND_FAILURE_RETRY_DELAY_SECONDS} * interval '1 second'),
          updated_at = now()
      FROM loyal_yield.balance_sweep_lot_claim_items i
      WHERE i.claim_token = (SELECT claim_token FROM selected_claim)
        AND l.id = i.lot_id
      RETURNING l.id
    )
    UPDATE loyal_yield.balance_sweep_lot_claims
    SET status = 'released',
        updated_at = now()
    WHERE claim_token = (SELECT claim_token FROM selected_claim)
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

function removeWalletToVaultTransfer(args: {
  decodeTransferCheckedInstruction: AppModules["decodeTransferCheckedInstruction"];
  getAssociatedTokenAddressSync: AppModules["getAssociatedTokenAddressSync"];
  instructions: readonly TransactionInstruction[];
  signer: PublicKey;
  tokenProgramId: PublicKey;
  usdcMint: PublicKey;
  vaultUsdcAta: PublicKey;
}): TransactionInstruction[] {
  const signerUsdcAta = args.getAssociatedTokenAddressSync(
    args.usdcMint,
    args.signer,
    false,
    args.tokenProgramId
  );

  return args.instructions.filter((instruction) => {
    if (!instruction.programId.equals(args.tokenProgramId)) {
      return true;
    }
    try {
      const decoded = args.decodeTransferCheckedInstruction(
        instruction,
        args.tokenProgramId
      );
      return !(
        decoded.keys.source.pubkey.equals(signerUsdcAta) &&
        decoded.keys.destination.pubkey.equals(args.vaultUsdcAta) &&
        decoded.keys.owner.pubkey.equals(args.signer)
      );
    } catch {
      return true;
    }
  });
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
  databaseUrl: string;
  depositSignature: string;
  depositSlot: bigint;
  policySignature: string;
  target: EligibleTarget;
}): Promise<{ status: "duplicate" | "inserted"; positionId: string | null }> {
  const sql = args.appModules.neon(args.databaseUrl);
  const earnTarget = args.appModules.getKaminoUsdcEarnTargetForCluster(
    args.appModules.LoyalCluster.MainnetBeta
  );
  const now = new Date();
  const targetReserve = earnTarget.reserve.toBase58();
  const market = earnTarget.market.toBase58();
  const liquidityMint = earnTarget.liquidityMint.toBase58();
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
      1,
      ${args.target.vaultPubkey},
      ${args.target.routePolicySeed.toString()},
      ${args.target.routePolicyAccount},
      ${args.target.routePolicySeed.toString()},
      ${targetReserve},
      ${market},
      ${liquidityMint},
      ${null},
      ${liquidityMint},
      ${args.amountRaw.toString()},
      ${now},
      ${now}
    )
    ON CONFLICT (deposit_signature) DO NOTHING
    RETURNING id
  `;

  const insertedDeposit = depositRows[0] as Record<string, unknown> | undefined;
  if (!insertedDeposit) {
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
      positionId: existing?.position_id?.toString() ?? null,
    };
  }

  const existingRows = await sql`
    SELECT *
    FROM loyal_yield.user_yield_positions
    WHERE settings = ${args.target.settings}
      AND vault_index = 1
      AND initial_reserve = ${targetReserve}
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
        targetReserve &&
      readRequiredString(
        existing.current_liquidity_mint,
        "current_liquidity_mint"
      ) === liquidityMint;
    eventType = "deposit_top_up";
    nextAmountRaw = sameCurrentHolding
      ? currentAmountRaw + args.amountRaw
      : currentAmountRaw;
    nextPrincipalRaw = principalAmountRaw + args.amountRaw;
    holdingDeltaRaw = sameCurrentHolding ? args.amountRaw : null;

    await sql`
      UPDATE loyal_yield.user_yield_positions
      SET
        deposit_mint = ${liquidityMint},
        initial_liquidity_mint = ${liquidityMint},
        initial_market = ${market},
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
    nextAmountRaw = args.amountRaw;
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
        1,
        ${args.target.vaultPubkey},
        ${args.target.routePolicySeed.toString()},
        ${args.target.routePolicyAccount},
        ${args.target.routePolicySeed.toString()},
        ${targetReserve},
        ${market},
        ${liquidityMint},
        ${null},
        ${liquidityMint},
        ${args.amountRaw.toString()},
        ${targetReserve},
        ${market},
        ${liquidityMint},
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
      ${targetReserve},
      ${market},
      ${liquidityMint},
      ${nextAmountRaw.toString()},
      ${args.amountRaw.toString()},
      ${holdingDeltaRaw?.toString() ?? null},
      ${args.depositSlot.toString()},
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
      current_liquidity_mint = ${liquidityMint},
      current_market = ${market},
      current_observed_at = ${now},
      current_observed_slot = ${args.depositSlot.toString()},
      current_reserve = ${targetReserve},
      last_holding_event_id = ${eventId},
      last_confirmed_slot = ${args.depositSlot.toString()},
      last_deposit_signature = ${args.depositSignature},
      principal_amount_raw = ${nextPrincipalRaw.toString()},
      status = 'active',
      updated_at = ${now}
    WHERE id = ${positionId}
  `;

  return { status: "inserted", positionId };
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

function isKnownPrefundDepositFailure(summary: SimulationSummary): boolean {
  return (
    summary.err !== null &&
    summary.logs.some((log) => log.includes("Error: insufficient funds")) &&
    summary.logs.some((log) =>
      log.includes("DepositReserveLiquidityAndObligationCollateral")
    )
  );
}

function assertExecutablePreflight(args: {
  depositSimulation: SimulationSummary;
  pullSimulation: SimulationSummary;
}) {
  if (args.pullSimulation.err) {
    throw new Error(
      `Autodeposit pull simulation failed; refusing to execute. simulation=${summarizeSimulationFailure(
        args.pullSimulation
      )}`
    );
  }
  if (
    args.depositSimulation.err &&
    !isKnownPrefundDepositFailure(args.depositSimulation)
  ) {
    throw new Error(
      `Kamino deposit simulation failed for an unexpected reason; refusing to execute. simulation=${summarizeSimulationFailure(
        args.depositSimulation
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
  const legacyKaminoKeypair = parseKeypairSecretWith(
    appModules.Keypair,
    requireEnv("SOLANA_TESTING_PK")
  );
  const programId = new PublicKeyCtor(
    process.env.LOYAL_SMART_ACCOUNTS_PROGRAM_ID ?? appModules.PROGRAM_ADDRESS
  );
  const connection = new Connection(rpcUrl, DEFAULT_COMMITMENT);

  const target = await loadEligibleTarget(
    appModules.neon,
    databaseUrl,
    options.targetId
  );
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
        claimToken: options.claimToken,
        walletBalanceRaw,
        walletBalanceFloorRaw: effectiveFloorRaw,
        remainingAllowanceRaw: allowance.remainingAmountInPeriodRaw,
      });
      if (lotClaim.status !== "selected" && lotClaim.status !== "executed") {
        console.log(
          JSON.stringify(
            {
              status: "noop",
              reason: `lot_claim_${lotClaim.reason ?? lotClaim.status}`,
              targetId: target.id.toString(),
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
    const usdcMint = new PublicKeyCtor(pull.persistence.liquidityMint);

    const depositDraft = await client.prepareEarnUsdcDeposit({
      amountRaw: executionAmountRaw,
      cluster: appModules.LoyalCluster.MainnetBeta,
      feePayer: legacyKaminoKeypair.publicKey,
      initializeYieldRoutingPolicy: false,
      policySigner: legacyKaminoKeypair.publicKey,
      settingsPda: new PublicKeyCtor(target.settings),
      walletAddress: legacyKaminoKeypair.publicKey,
      yieldRoutingPolicy: {
        account: new PublicKeyCtor(target.routePolicyAccount),
        seed: target.routePolicySeed,
      },
    });
    const depositOnlyInstructions = removeWalletToVaultTransfer({
      decodeTransferCheckedInstruction: appModules.decodeTransferCheckedInstruction,
      getAssociatedTokenAddressSync: appModules.getAssociatedTokenAddressSync,
      instructions: depositDraft.prepared.instructions,
      signer: legacyKaminoKeypair.publicKey,
      tokenProgramId: appModules.TOKEN_PROGRAM_ID,
      usdcMint,
      vaultUsdcAta,
    });
    const depositPrepared = {
      ...depositDraft.prepared,
      instructions: [
        appModules.createAssociatedTokenAccountIdempotentInstruction(
          legacyKaminoKeypair.publicKey,
          vaultUsdcAta,
          new PublicKeyCtor(target.vaultPubkey),
          usdcMint,
          appModules.TOKEN_PROGRAM_ID
        ),
        ...depositOnlyInstructions.filter(
          (instruction) =>
            !instruction.keys.some((key) => key.pubkey.equals(walletUsdcAta))
        ),
      ],
    };

    const pullSimulation = await simulatePreparedOperation({
      compilePreparedOperation: appModules.compilePreparedOperation,
      connection,
      prepared: pull.prepared,
      signers: [policyKeypair],
    });
    const depositSimulation = await simulatePreparedOperation({
      compilePreparedOperation: appModules.compilePreparedOperation,
      connection,
      prepared: depositPrepared,
      signers: [legacyKaminoKeypair],
    });

    const plan = {
      status: options.execute ? "execute_requested" : "dry_run",
      targetId: target.id.toString(),
      wallet: target.wallet,
      vault: target.vaultPubkey,
      walletUsdcAta: target.walletUsdcAta,
      vaultUsdcAta: target.vaultUsdcAta,
      walletBalanceRaw: walletBalanceRaw.toString(),
      walletBalanceFloorRaw: effectiveFloorRaw.toString(),
      persistedWalletBalanceFloorRaw: target.walletBalanceFloorRaw.toString(),
      overrideFloorRaw: options.overrideFloorRaw?.toString() ?? null,
      vaultPreBalanceRaw: vaultPreBalanceRaw.toString(),
      excessRaw: sweepDecision.excessRaw.toString(),
      amountRaw: executionAmountRaw.toString(),
      amountUi: Number(executionAmountRaw) / 10 ** USDC_DECIMALS,
      cappedByMaxPerPeriod: sweepDecision.cappedByMaxPerPeriod,
      cappedByRemainingAllowance: sweepDecision.cappedByRemainingAllowance,
      subscriptionAllowance: summarizeAllowance(allowance),
      transactionOrder: [
        "subscription_pull_wallet_to_earn_vault",
        "kamino_main_usdc_deposit_from_earn_vault",
      ],
      signers: {
        pull: policyKeypair.publicKey.toBase58(),
        kaminoDeposit: legacyKaminoKeypair.publicKey.toBase58(),
      },
      policies: {
        sweep: target.sweepPolicyAccount,
        kaminoDeposit: target.routePolicyAccount,
      },
      simulations: {
        pull: summarizeSimulation(pullSimulation),
        kaminoDeposit: summarizeSimulation(depositSimulation),
      },
      simulationNotes: {
        kaminoDepositRequiresPostPullResimulation:
          isKnownPrefundDepositFailure(depositSimulation),
      },
      lotClaim: lotClaim ? summarizeLotClaim(lotClaim) : null,
      sendsTransactions: options.execute,
    };

    if (!options.execute) {
      console.log(JSON.stringify(plan, null, 2));
      return;
    }

    assertExecutablePreflight({ depositSimulation, pullSimulation });

    const pullSend = await sendPreparedOperation({
      compilePreparedOperation: appModules.compilePreparedOperation,
      connection,
      prepared: pull.prepared,
      signers: [policyKeypair],
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

    const depositPostPullSimulation = await simulatePreparedOperation({
      compilePreparedOperation: appModules.compilePreparedOperation,
      connection,
      prepared: depositPrepared,
      signers: [legacyKaminoKeypair],
    });
    if (depositPostPullSimulation.err) {
      await updateExecutionEvidence({
        neon: appModules.neon,
        databaseUrl,
        dedupeKey: executionRecord.dedupeKey,
        decodedEvidence: {
          status: "partial_executed_pull_deposit_blocked",
          kaminoDepositPostPullSimulation: summarizeSimulation(
            depositPostPullSimulation
          ),
          vaultPostPullRaw: vaultPostPullRaw.toString(),
        },
      });
      console.log(
        JSON.stringify(
          {
            ...plan,
            status: "partial_executed_pull_deposit_blocked",
            signatures: {
              pull: pullSend.signature,
            },
            confirmedSlots: {
              pull: pullSend.slot.toString(),
            },
            walletPostPullRaw: walletPostPullRaw.toString(),
            vaultPostPullRaw: vaultPostPullRaw.toString(),
            postPullSimulations: {
              kaminoDeposit: summarizeSimulation(depositPostPullSimulation),
            },
          },
          null,
          2
        )
      );
      process.exitCode = 1;
      return;
    }

    const depositSend = await sendPreparedOperation({
      compilePreparedOperation: appModules.compilePreparedOperation,
      connection,
      prepared: depositPrepared,
      signers: [legacyKaminoKeypair],
    });
    const vaultPostDepositRaw = await getTokenBalanceRaw(connection, vaultUsdcAta);
    const yieldDepositRecord = await recordAutodepositYieldDeposit({
      amountRaw: executionAmountRaw,
      appModules,
      databaseUrl,
      depositSignature: depositSend.signature,
      depositSlot: depositSend.slot,
      policySignature: depositSend.signature,
      target,
    });
    await updateExecutionEvidence({
      neon: appModules.neon,
      databaseUrl,
      dedupeKey: executionRecord.dedupeKey,
      decodedEvidence: {
        status: "executed",
        kaminoDepositSignature: depositSend.signature,
        kaminoDepositSlot: depositSend.slot.toString(),
        kaminoDepositPostPullSimulation:
          summarizeSimulation(depositPostPullSimulation),
        vaultPostDepositRaw: vaultPostDepositRaw.toString(),
        yieldDepositRecord,
      },
    });

    console.log(
      JSON.stringify(
        {
          ...plan,
          status: "executed",
          signatures: {
            pull: pullSend.signature,
            kaminoDeposit: depositSend.signature,
          },
          confirmedSlots: {
            pull: pullSend.slot.toString(),
            kaminoDeposit: depositSend.slot.toString(),
          },
          walletPostPullRaw: walletPostPullRaw.toString(),
          vaultPostPullRaw: vaultPostPullRaw.toString(),
          vaultPostDepositRaw: vaultPostDepositRaw.toString(),
          postPullSimulations: {
            kaminoDeposit: summarizeSimulation(depositPostPullSimulation),
          },
          yieldDepositRecord,
        },
        null,
        2
      )
    );
  } catch (error) {
    if (!pullSent && lotClaim?.status === "selected" && lotClaim.claimToken) {
      await releaseAutodepositLotClaim({
        neon: appModules.neon,
        databaseUrl,
        claimToken: lotClaim.claimToken,
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
