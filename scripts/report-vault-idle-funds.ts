import { neon } from "@neondatabase/serverless";
import { Connection, PublicKey } from "@solana/web3.js";

const USDC_MINT = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v";
const PYUSD_MINT = "2b1kV6DkPAnxd5ixfnxCpjxmKwqjjaYmCZfHsFu24GXo";
const TOKEN_PROGRAMS = new Set([
  "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA",
  "TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb",
]);

const MINT_METADATA = new Map([
  [USDC_MINT, { decimals: 6, symbol: "USDC" }],
  [PYUSD_MINT, { decimals: 6, symbol: "PYUSD" }],
]);

type RawVaultRow = {
  vault_id: string;
  vault_pubkey: string;
  managed_vault_active: boolean;
  mint: string;
  amount_raw: string;
  owner: string;
  token_account: string;
  observed_slot: string;
  observed_at: string;
  source_commitment: string;
  updated_at: string;
  active_position_count: string;
  active_position_current_amount_raw: string;
  active_autodeposit_target_count: string;
  top_up_failure_count: string;
  last_top_up_failure_at: string | null;
};

export type VaultIdleBalance = {
  vaultId: string;
  vaultPubkey: string;
  managedVaultActive: boolean;
  mint: string;
  symbol: string;
  decimals: number | null;
  neonAmountRaw: bigint;
  owner: string;
  tokenAccount: string;
  observedSlot: bigint;
  observedAt: string;
  sourceCommitment: string;
  updatedAt: string;
  activePositionCount: number;
  activePositionCurrentAmountRaw: bigint;
  activeAutodepositTargetCount: number;
  topUpFailureCount: number;
  lastTopUpFailureAt: string | null;
  chainAmountRaw?: bigint;
  chainStatus?: ChainStatus;
  chainSlot?: number;
};

type ChainStatus =
  | "ok"
  | "missing_account"
  | "invalid_token_account"
  | "mint_mismatch"
  | "owner_mismatch"
  | "program_mismatch";

export type ReportOptions = {
  chainBatchSize: number;
  csvPath: string | null;
  json: boolean;
  jsonPath: string | null;
  mint: string | null;
  top: number;
  verifyChain: boolean;
};

type AmountBucket = {
  label: string;
  maxExclusive: bigint | null;
};

const AMOUNT_BUCKETS: AmountBucket[] = [
  { label: "0-0.000010", maxExclusive: 11n },
  { label: "0.000011-0.001000", maxExclusive: 1_001n },
  { label: "0.001001-0.009999", maxExclusive: 10_000n },
  { label: "0.010000-0.099999", maxExclusive: 100_000n },
  { label: "0.100000-0.999999", maxExclusive: 1_000_000n },
  { label: "1.000000-9.999999", maxExclusive: 10_000_000n },
  { label: "10.000000-99.999999", maxExclusive: 100_000_000n },
  { label: "100+", maxExclusive: null },
];

export function parseOptions(argv: string[]): ReportOptions {
  const options: ReportOptions = {
    chainBatchSize: 100,
    csvPath: null,
    json: false,
    jsonPath: null,
    mint: null,
    top: 25,
    verifyChain: false,
  };

  for (let index = 0; index < argv.length; index += 1) {
    const argument = argv[index];
    const value = argv[index + 1];
    switch (argument) {
      case "--chain-batch-size":
        options.chainBatchSize = readPositiveInteger(
          value,
          "--chain-batch-size",
        );
        index += 1;
        break;
      case "--csv":
        options.csvPath = readValue(value, "--csv");
        index += 1;
        break;
      case "--json":
        options.json = true;
        break;
      case "--json-output":
        options.jsonPath = readValue(value, "--json-output");
        index += 1;
        break;
      case "--mint":
        options.mint = new PublicKey(readValue(value, "--mint")).toBase58();
        index += 1;
        break;
      case "--top":
        options.top = readNonNegativeInteger(value, "--top");
        index += 1;
        break;
      case "--verify-chain":
        options.verifyChain = true;
        break;
      case "--help":
      case "-h":
        printHelp();
        process.exit(0);
        break;
      default:
        throw new Error(`Unknown argument: ${argument}`);
    }
  }

  if (options.chainBatchSize > 100) {
    throw new Error(
      "--chain-batch-size cannot exceed Solana's 100-account limit",
    );
  }
  return options;
}

function readValue(value: string | undefined, option: string): string {
  if (!value || value.startsWith("--")) {
    throw new Error(`${option} requires a value`);
  }
  return value;
}

function readPositiveInteger(
  value: string | undefined,
  option: string,
): number {
  const parsed = readNonNegativeInteger(value, option);
  if (parsed === 0) {
    throw new Error(`${option} must be greater than zero`);
  }
  return parsed;
}

function readNonNegativeInteger(
  value: string | undefined,
  option: string,
): number {
  const parsed = Number(readValue(value, option));
  if (!Number.isSafeInteger(parsed) || parsed < 0) {
    throw new Error(`${option} must be a non-negative integer`);
  }
  return parsed;
}

function printHelp(): void {
  console.log(`Usage: bun scripts/report-vault-idle-funds.ts [options]

Read-only audit of user funds held in vault token accounts instead of Kamino.

Options:
  --verify-chain           Re-read every reported token account at finalized commitment
  --chain-batch-size N     Accounts per finalized RPC request (default 100, max 100)
  --mint ADDRESS           Limit the report to one mint
  --top N                  Number of largest balances to print (default 25)
  --json                   Print the complete report as JSON
  --json-output PATH       Write the complete report as JSON
  --csv PATH               Write every non-zero or discrepant vault row as CSV
  -h, --help               Show this help

Environment:
  NEON_DATABASE_URL        Required
  SOLANA_RPC_URL           Required only with --verify-chain`);
}

function requireEnv(name: string): string {
  const value = process.env[name]?.trim();
  if (!value) {
    throw new Error(`${name} is required`);
  }
  return value;
}

function normalizeRow(row: RawVaultRow): VaultIdleBalance {
  const metadata = MINT_METADATA.get(row.mint);
  return {
    vaultId: row.vault_id,
    vaultPubkey: row.vault_pubkey,
    managedVaultActive: row.managed_vault_active,
    mint: row.mint,
    symbol: metadata?.symbol ?? row.mint,
    decimals: metadata?.decimals ?? null,
    neonAmountRaw: BigInt(row.amount_raw),
    owner: row.owner,
    tokenAccount: row.token_account,
    observedSlot: BigInt(row.observed_slot),
    observedAt: row.observed_at,
    sourceCommitment: row.source_commitment,
    updatedAt: row.updated_at,
    activePositionCount: Number(row.active_position_count),
    activePositionCurrentAmountRaw: BigInt(
      row.active_position_current_amount_raw,
    ),
    activeAutodepositTargetCount: Number(row.active_autodeposit_target_count),
    topUpFailureCount: Number(row.top_up_failure_count),
    lastTopUpFailureAt: row.last_top_up_failure_at,
  };
}

async function loadVaultRows(databaseUrl: string): Promise<VaultIdleBalance[]> {
  const sql = neon(databaseUrl);
  const rows = await sql`
    WITH position_rollup AS (
      SELECT
        vault_pubkey,
        COUNT(*) FILTER (WHERE status::text = 'active') AS active_position_count,
        COALESCE(
          SUM(current_amount_raw) FILTER (WHERE status::text = 'active'),
          0
        ) AS active_position_current_amount_raw
      FROM loyal_yield.user_yield_positions
      GROUP BY vault_pubkey
    ),
    target_rollup AS (
      SELECT
        vault_pubkey,
        COUNT(*) FILTER (
          WHERE active = true AND lifecycle_status = 'active'
        ) AS active_autodeposit_target_count
      FROM loyal_yield.balance_sweep_targets
      GROUP BY vault_pubkey
    ),
    failure_rollup AS (
      SELECT
        destination_vault_ata AS token_account,
        COUNT(*) FILTER (
          WHERE completion_failure_code = 'kamino_top_up_failed'
        ) AS top_up_failure_count,
        MAX(inserted_at) FILTER (
          WHERE completion_failure_code = 'kamino_top_up_failed'
        ) AS last_top_up_failure_at
      FROM loyal_yield.balance_sweep_executions
      GROUP BY destination_vault_ata
    )
    SELECT
      balance.vault_id::text,
      vault.vault_pubkey,
      vault.active AS managed_vault_active,
      balance.mint,
      balance.amount_raw::text,
      balance.owner,
      balance.token_account,
      balance.observed_slot::text,
      balance.observed_at::text,
      balance.source_commitment,
      balance.updated_at::text,
      COALESCE(position.active_position_count, 0)::text AS active_position_count,
      COALESCE(position.active_position_current_amount_raw, 0)::text
        AS active_position_current_amount_raw,
      COALESCE(target.active_autodeposit_target_count, 0)::text
        AS active_autodeposit_target_count,
      COALESCE(failure.top_up_failure_count, 0)::text AS top_up_failure_count,
      failure.last_top_up_failure_at::text
    FROM loyal_yield.vault_idle_token_balances_current AS balance
    JOIN loyal_yield.managed_vaults AS vault
      ON vault.id = balance.vault_id
    LEFT JOIN position_rollup AS position
      ON position.vault_pubkey = vault.vault_pubkey
    LEFT JOIN target_rollup AS target
      ON target.vault_pubkey = vault.vault_pubkey
    LEFT JOIN failure_rollup AS failure
      ON failure.token_account = balance.token_account
    ORDER BY balance.vault_id, balance.mint
  `;
  return (rows as RawVaultRow[]).map(normalizeRow);
}

type ManagedVaultCoverage = {
  managedVaultCount: number;
  representedVaultCount: number;
  missingVaultCount: number;
  missingVaultIds: string[];
};

async function loadManagedVaultCoverage(
  databaseUrl: string,
): Promise<ManagedVaultCoverage> {
  const sql = neon(databaseUrl);
  const rows = await sql`
    SELECT
      COUNT(*)::text AS managed_vault_count,
      COUNT(*) FILTER (WHERE idle.vault_id IS NOT NULL)::text
        AS represented_vault_count,
      COUNT(*) FILTER (WHERE idle.vault_id IS NULL)::text AS missing_vault_count,
      COALESCE(
        ARRAY_AGG(vault.id::text ORDER BY vault.id)
          FILTER (WHERE idle.vault_id IS NULL),
        ARRAY[]::text[]
      ) AS missing_vault_ids
    FROM loyal_yield.managed_vaults AS vault
    LEFT JOIN (
      SELECT DISTINCT vault_id
      FROM loyal_yield.vault_idle_token_balances_current
    ) AS idle
      ON idle.vault_id = vault.id
  `;
  const row = rows[0] as {
    managed_vault_count: string;
    represented_vault_count: string;
    missing_vault_count: string;
    missing_vault_ids: string[];
  };
  return {
    managedVaultCount: Number(row.managed_vault_count),
    representedVaultCount: Number(row.represented_vault_count),
    missingVaultCount: Number(row.missing_vault_count),
    missingVaultIds: row.missing_vault_ids,
  };
}

export function parseTokenAccountData(data: Uint8Array): {
  amountRaw: bigint;
  mint: string;
  owner: string;
} {
  if (data.byteLength < 72) {
    throw new Error(`token account data is only ${data.byteLength} bytes`);
  }
  const buffer = Buffer.from(data.buffer, data.byteOffset, data.byteLength);
  return {
    mint: new PublicKey(buffer.subarray(0, 32)).toBase58(),
    owner: new PublicKey(buffer.subarray(32, 64)).toBase58(),
    amountRaw: buffer.readBigUInt64LE(64),
  };
}

async function verifyRowsOnChain(
  rows: VaultIdleBalance[],
  rpcUrl: string,
  batchSize: number,
): Promise<{ maxSlot: number; minSlot: number }> {
  const connection = new Connection(rpcUrl, "finalized");
  let minSlot = Number.MAX_SAFE_INTEGER;
  let maxSlot = 0;

  for (let offset = 0; offset < rows.length; offset += batchSize) {
    const batch = rows.slice(offset, offset + batchSize);
    const publicKeys = batch.map((row) => new PublicKey(row.tokenAccount));
    const response = await retryRpc(() =>
      connection.getMultipleAccountsInfoAndContext(publicKeys, {
        commitment: "finalized",
      }),
    );
    minSlot = Math.min(minSlot, response.context.slot);
    maxSlot = Math.max(maxSlot, response.context.slot);

    response.value.forEach((account, index) => {
      const row = batch[index];
      row.chainSlot = response.context.slot;
      if (!account) {
        row.chainAmountRaw = 0n;
        row.chainStatus = "missing_account";
        return;
      }
      if (!TOKEN_PROGRAMS.has(account.owner.toBase58())) {
        row.chainAmountRaw = 0n;
        row.chainStatus = "program_mismatch";
        return;
      }
      try {
        const decoded = parseTokenAccountData(account.data);
        row.chainAmountRaw = decoded.amountRaw;
        if (decoded.mint !== row.mint) {
          row.chainStatus = "mint_mismatch";
        } else if (decoded.owner !== row.owner) {
          row.chainStatus = "owner_mismatch";
        } else {
          row.chainStatus = "ok";
        }
      } catch {
        row.chainAmountRaw = 0n;
        row.chainStatus = "invalid_token_account";
      }
    });
  }

  return {
    minSlot: rows.length === 0 ? 0 : minSlot,
    maxSlot,
  };
}

async function retryRpc<T>(operation: () => Promise<T>): Promise<T> {
  let lastError: unknown;
  for (let attempt = 1; attempt <= 3; attempt += 1) {
    try {
      return await operation();
    } catch (error) {
      lastError = error;
      if (attempt < 3) {
        await Bun.sleep(attempt * 500);
      }
    }
  }
  throw lastError;
}

type Cohort = {
  count: number;
  amountRaw: bigint;
};

type MintSummary = {
  mint: string;
  symbol: string;
  decimals: number | null;
  rows: number;
  positive: Cohort;
  neonTotalRaw: bigint;
  chainPositive?: Cohort;
  chainTotalRaw?: bigint;
  exactChainMatches?: number;
  chainMismatches?: Cohort;
  chainStatusCounts?: Record<string, number>;
  chainAboveNeon?: Cohort;
  chainBelowNeon?: Cohort;
  neonZeroChainPositive?: Cohort;
  neonPositiveChainZero?: Cohort;
  chainActiveManagedVaults?: Cohort;
  chainInactiveManagedVaults?: Cohort;
  chainActivePositions?: Cohort;
  chainNoActivePosition?: Cohort;
  chainActiveAutodeposit?: Cohort;
  chainNoActiveAutodeposit?: Cohort;
  chainHistoricalTopUpFailure?: Cohort;
  chainNoHistoricalTopUpFailure?: Cohort;
  chainAmountBuckets?: Record<string, Cohort>;
  activeManagedVaults: Cohort;
  inactiveManagedVaults: Cohort;
  activePositions: Cohort;
  noActivePosition: Cohort;
  activeAutodeposit: Cohort;
  noActiveAutodeposit: Cohort;
  historicalTopUpFailure: Cohort;
  noHistoricalTopUpFailure: Cohort;
  amountBuckets: Record<string, Cohort>;
  commitmentCounts: Record<string, Cohort>;
  freshness: Record<string, Cohort>;
};

function emptyCohort(): Cohort {
  return { count: 0, amountRaw: 0n };
}

function add(cohort: Cohort, amountRaw: bigint): void {
  cohort.count += 1;
  cohort.amountRaw += amountRaw;
}

function bucketFor(amountRaw: bigint): string {
  return (
    AMOUNT_BUCKETS.find(
      (bucket) =>
        bucket.maxExclusive === null || amountRaw < bucket.maxExclusive,
    )?.label ?? "unknown"
  );
}

function freshnessFor(observedAt: string, generatedAt: Date): string {
  const ageMs = Math.max(0, generatedAt.getTime() - Date.parse(observedAt));
  if (ageMs <= 5 * 60_000) return "0-5m";
  if (ageMs <= 60 * 60_000) return "5m-1h";
  if (ageMs <= 24 * 60 * 60_000) return "1h-24h";
  return "24h+";
}

export function summarizeRows(
  rows: VaultIdleBalance[],
  generatedAt: Date,
): MintSummary[] {
  const byMint = new Map<string, MintSummary>();
  for (const row of rows) {
    let summary = byMint.get(row.mint);
    if (!summary) {
      summary = {
        mint: row.mint,
        symbol: row.symbol,
        decimals: row.decimals,
        rows: 0,
        positive: emptyCohort(),
        neonTotalRaw: 0n,
        activeManagedVaults: emptyCohort(),
        inactiveManagedVaults: emptyCohort(),
        activePositions: emptyCohort(),
        noActivePosition: emptyCohort(),
        activeAutodeposit: emptyCohort(),
        noActiveAutodeposit: emptyCohort(),
        historicalTopUpFailure: emptyCohort(),
        noHistoricalTopUpFailure: emptyCohort(),
        amountBuckets: {},
        commitmentCounts: {},
        freshness: {},
      };
      if (row.chainAmountRaw !== undefined) {
        summary.chainPositive = emptyCohort();
        summary.chainTotalRaw = 0n;
        summary.exactChainMatches = 0;
        summary.chainMismatches = emptyCohort();
        summary.chainStatusCounts = {};
        summary.chainAboveNeon = emptyCohort();
        summary.chainBelowNeon = emptyCohort();
        summary.neonZeroChainPositive = emptyCohort();
        summary.neonPositiveChainZero = emptyCohort();
        summary.chainActiveManagedVaults = emptyCohort();
        summary.chainInactiveManagedVaults = emptyCohort();
        summary.chainActivePositions = emptyCohort();
        summary.chainNoActivePosition = emptyCohort();
        summary.chainActiveAutodeposit = emptyCohort();
        summary.chainNoActiveAutodeposit = emptyCohort();
        summary.chainHistoricalTopUpFailure = emptyCohort();
        summary.chainNoHistoricalTopUpFailure = emptyCohort();
        summary.chainAmountBuckets = {};
      }
      byMint.set(row.mint, summary);
    }

    summary.rows += 1;
    summary.neonTotalRaw += row.neonAmountRaw;
    if (row.neonAmountRaw > 0n) {
      add(summary.positive, row.neonAmountRaw);
      add(
        row.managedVaultActive
          ? summary.activeManagedVaults
          : summary.inactiveManagedVaults,
        row.neonAmountRaw,
      );
      add(
        row.activePositionCount > 0
          ? summary.activePositions
          : summary.noActivePosition,
        row.neonAmountRaw,
      );
      add(
        row.activeAutodepositTargetCount > 0
          ? summary.activeAutodeposit
          : summary.noActiveAutodeposit,
        row.neonAmountRaw,
      );
      add(
        row.topUpFailureCount > 0
          ? summary.historicalTopUpFailure
          : summary.noHistoricalTopUpFailure,
        row.neonAmountRaw,
      );
      const amountBucket = bucketFor(row.neonAmountRaw);
      summary.amountBuckets[amountBucket] ??= emptyCohort();
      add(summary.amountBuckets[amountBucket], row.neonAmountRaw);
      const freshness = freshnessFor(row.observedAt, generatedAt);
      summary.freshness[freshness] ??= emptyCohort();
      add(summary.freshness[freshness], row.neonAmountRaw);
    }

    summary.commitmentCounts[row.sourceCommitment] ??= emptyCohort();
    add(summary.commitmentCounts[row.sourceCommitment], row.neonAmountRaw);

    if (row.chainAmountRaw !== undefined) {
      summary.chainTotalRaw =
        (summary.chainTotalRaw ?? 0n) + row.chainAmountRaw;
      if (row.chainAmountRaw > 0n) {
        add(summary.chainPositive!, row.chainAmountRaw);
        add(
          row.managedVaultActive
            ? summary.chainActiveManagedVaults!
            : summary.chainInactiveManagedVaults!,
          row.chainAmountRaw,
        );
        add(
          row.activePositionCount > 0
            ? summary.chainActivePositions!
            : summary.chainNoActivePosition!,
          row.chainAmountRaw,
        );
        add(
          row.activeAutodepositTargetCount > 0
            ? summary.chainActiveAutodeposit!
            : summary.chainNoActiveAutodeposit!,
          row.chainAmountRaw,
        );
        add(
          row.topUpFailureCount > 0
            ? summary.chainHistoricalTopUpFailure!
            : summary.chainNoHistoricalTopUpFailure!,
          row.chainAmountRaw,
        );
        const chainAmountBucket = bucketFor(row.chainAmountRaw);
        summary.chainAmountBuckets![chainAmountBucket] ??= emptyCohort();
        add(summary.chainAmountBuckets![chainAmountBucket], row.chainAmountRaw);
      }
      if (row.chainAmountRaw === row.neonAmountRaw) {
        summary.exactChainMatches = (summary.exactChainMatches ?? 0) + 1;
      } else {
        add(summary.chainMismatches!, row.chainAmountRaw - row.neonAmountRaw);
        if (row.chainAmountRaw > row.neonAmountRaw) {
          add(summary.chainAboveNeon!, row.chainAmountRaw - row.neonAmountRaw);
        } else {
          add(summary.chainBelowNeon!, row.neonAmountRaw - row.chainAmountRaw);
        }
      }
      if (row.neonAmountRaw === 0n && row.chainAmountRaw > 0n) {
        add(summary.neonZeroChainPositive!, row.chainAmountRaw);
      }
      if (row.neonAmountRaw > 0n && row.chainAmountRaw === 0n) {
        add(summary.neonPositiveChainZero!, row.neonAmountRaw);
      }
      const status = row.chainStatus ?? "unknown";
      summary.chainStatusCounts![status] =
        (summary.chainStatusCounts![status] ?? 0) + 1;
    }
  }
  return [...byMint.values()].sort((left, right) =>
    left.symbol.localeCompare(right.symbol),
  );
}

function formatAmount(amountRaw: bigint, decimals: number | null): string {
  if (decimals === null) return amountRaw.toString();
  const negative = amountRaw < 0n;
  const absolute = negative ? -amountRaw : amountRaw;
  const scale = 10n ** BigInt(decimals);
  const whole = absolute / scale;
  const fraction = (absolute % scale)
    .toString()
    .padStart(decimals, "0")
    .replace(/0+$/, "");
  return `${negative ? "-" : ""}${whole}${fraction ? `.${fraction}` : ""}`;
}

function printCohort(
  label: string,
  cohort: Cohort,
  decimals: number | null,
  symbol: string,
): void {
  console.log(
    `  ${label.padEnd(32)} ${String(cohort.count).padStart(6)}  ${formatAmount(cohort.amountRaw, decimals)} ${symbol}`,
  );
}

function printTextReport(args: {
  chainSlots: { minSlot: number; maxSlot: number } | null;
  coverage: ManagedVaultCoverage;
  generatedAt: string;
  rows: VaultIdleBalance[];
  summaries: MintSummary[];
  top: number;
}): void {
  console.log("Vault idle-funds audit (read-only)");
  console.log(`Generated at (UTC): ${args.generatedAt}`);
  console.log(
    `Coverage: ${args.coverage.representedVaultCount}/${args.coverage.managedVaultCount} managed vaults represented; ${args.coverage.missingVaultCount} missing current-balance rows`,
  );
  if (args.coverage.missingVaultCount > 0) {
    console.log(
      `Missing vault ids: ${args.coverage.missingVaultIds.join(", ")}`,
    );
  }
  if (args.chainSlots) {
    console.log(
      `Finalized chain verification slots: ${args.chainSlots.minSlot}-${args.chainSlots.maxSlot}`,
    );
  }

  for (const summary of args.summaries) {
    console.log(`\n${summary.symbol} (${summary.mint})`);
    console.log(`  Neon rows: ${summary.rows}`);
    printCohort(
      "positive Neon balances",
      summary.positive,
      summary.decimals,
      summary.symbol,
    );
    if (summary.chainPositive) {
      printCohort(
        "positive finalized balances",
        summary.chainPositive,
        summary.decimals,
        summary.symbol,
      );
      printCohort(
        "net finalized minus Neon",
        summary.chainMismatches!,
        summary.decimals,
        summary.symbol,
      );
      printCohort(
        "finalized above Neon (delta)",
        summary.chainAboveNeon!,
        summary.decimals,
        summary.symbol,
      );
      printCohort(
        "finalized below Neon (delta)",
        summary.chainBelowNeon!,
        summary.decimals,
        summary.symbol,
      );
      printCohort(
        "Neon zero, finalized positive",
        summary.neonZeroChainPositive!,
        summary.decimals,
        summary.symbol,
      );
      printCohort(
        "Neon positive, finalized zero",
        summary.neonPositiveChainZero!,
        summary.decimals,
        summary.symbol,
      );
      console.log(
        `  exact Neon/finalized matches: ${summary.exactChainMatches}/${summary.rows}`,
      );
      console.log(
        `  chain account statuses: ${JSON.stringify(summary.chainStatusCounts)}`,
      );
      console.log("  finalized balance cohorts:");
      printCohort(
        "    active managed vaults",
        summary.chainActiveManagedVaults!,
        summary.decimals,
        summary.symbol,
      );
      printCohort(
        "    inactive managed vaults",
        summary.chainInactiveManagedVaults!,
        summary.decimals,
        summary.symbol,
      );
      printCohort(
        "    with active Earn position",
        summary.chainActivePositions!,
        summary.decimals,
        summary.symbol,
      );
      printCohort(
        "    without active Earn position",
        summary.chainNoActivePosition!,
        summary.decimals,
        summary.symbol,
      );
      printCohort(
        "    active autodeposit target",
        summary.chainActiveAutodeposit!,
        summary.decimals,
        summary.symbol,
      );
      printCohort(
        "    no active autodeposit target",
        summary.chainNoActiveAutodeposit!,
        summary.decimals,
        summary.symbol,
      );
      printCohort(
        "    linked to top-up failure",
        summary.chainHistoricalTopUpFailure!,
        summary.decimals,
        summary.symbol,
      );
      console.log("  finalized amount buckets:");
      for (const bucket of AMOUNT_BUCKETS) {
        const cohort = summary.chainAmountBuckets![bucket.label];
        if (cohort) {
          printCohort(
            `    ${bucket.label}`,
            cohort,
            summary.decimals,
            summary.symbol,
          );
        }
      }
    }
    printCohort(
      "active managed vaults",
      summary.activeManagedVaults,
      summary.decimals,
      summary.symbol,
    );
    printCohort(
      "inactive managed vaults",
      summary.inactiveManagedVaults,
      summary.decimals,
      summary.symbol,
    );
    printCohort(
      "with active Earn position",
      summary.activePositions,
      summary.decimals,
      summary.symbol,
    );
    printCohort(
      "without active Earn position",
      summary.noActivePosition,
      summary.decimals,
      summary.symbol,
    );
    printCohort(
      "active autodeposit target",
      summary.activeAutodeposit,
      summary.decimals,
      summary.symbol,
    );
    printCohort(
      "no active autodeposit target",
      summary.noActiveAutodeposit,
      summary.decimals,
      summary.symbol,
    );
    printCohort(
      "linked to prior top-up failure",
      summary.historicalTopUpFailure,
      summary.decimals,
      summary.symbol,
    );
    console.log("  amount buckets:");
    for (const bucket of AMOUNT_BUCKETS) {
      const cohort = summary.amountBuckets[bucket.label];
      if (cohort) {
        printCohort(
          `    ${bucket.label}`,
          cohort,
          summary.decimals,
          summary.symbol,
        );
      }
    }
    console.log("  observation freshness (positive Neon rows):");
    for (const label of ["0-5m", "5m-1h", "1h-24h", "24h+"]) {
      const cohort = summary.freshness[label];
      if (cohort) {
        printCohort(`    ${label}`, cohort, summary.decimals, summary.symbol);
      }
    }
  }

  const relevant = args.rows
    .filter(
      (row) =>
        row.neonAmountRaw > 0n ||
        (row.chainAmountRaw !== undefined && row.chainAmountRaw > 0n) ||
        (row.chainAmountRaw !== undefined &&
          row.chainAmountRaw !== row.neonAmountRaw),
    )
    .sort((left, right) => {
      const leftAmount = left.chainAmountRaw ?? left.neonAmountRaw;
      const rightAmount = right.chainAmountRaw ?? right.neonAmountRaw;
      return leftAmount === rightAmount ? 0 : leftAmount > rightAmount ? -1 : 1;
    })
    .slice(0, args.top);
  if (relevant.length > 0) {
    console.log(`\nLargest ${relevant.length} balances:`);
    console.log(
      "  vault_id | symbol | Neon | finalized | active position | active autodeposit | failures | token account",
    );
    for (const row of relevant) {
      console.log(
        `  ${row.vaultId} | ${row.symbol} | ${formatAmount(row.neonAmountRaw, row.decimals)} | ${row.chainAmountRaw === undefined ? "not checked" : formatAmount(row.chainAmountRaw, row.decimals)} | ${row.activePositionCount} | ${row.activeAutodepositTargetCount} | ${row.topUpFailureCount} | ${row.tokenAccount}`,
      );
    }
  }
}

function jsonReplacer(_key: string, value: unknown): unknown {
  return typeof value === "bigint" ? value.toString() : value;
}

export function rowsToCsv(rows: VaultIdleBalance[]): string {
  const columns = [
    "vaultId",
    "vaultPubkey",
    "managedVaultActive",
    "mint",
    "symbol",
    "neonAmountRaw",
    "chainAmountRaw",
    "chainStatus",
    "tokenAccount",
    "owner",
    "observedSlot",
    "observedAt",
    "sourceCommitment",
    "activePositionCount",
    "activePositionCurrentAmountRaw",
    "activeAutodepositTargetCount",
    "topUpFailureCount",
    "lastTopUpFailureAt",
  ] as const;
  const escape = (value: unknown): string => {
    const text = value === undefined || value === null ? "" : String(value);
    return /[",\n]/.test(text) ? `"${text.replaceAll('"', '""')}"` : text;
  };
  return [
    columns.join(","),
    ...rows.map((row) =>
      columns.map((column) => escape(row[column])).join(","),
    ),
  ].join("\n");
}

async function main(): Promise<void> {
  const options = parseOptions(Bun.argv.slice(2));
  const databaseUrl = requireEnv("NEON_DATABASE_URL");
  const generatedAt = new Date();
  const [allRows, coverage] = await Promise.all([
    loadVaultRows(databaseUrl),
    loadManagedVaultCoverage(databaseUrl),
  ]);
  const rows = options.mint
    ? allRows.filter((row) => row.mint === options.mint)
    : allRows;
  let chainSlots: { minSlot: number; maxSlot: number } | null = null;
  if (options.verifyChain) {
    chainSlots = await verifyRowsOnChain(
      rows,
      requireEnv("SOLANA_RPC_URL"),
      options.chainBatchSize,
    );
  }
  const summaries = summarizeRows(rows, generatedAt);
  const report = {
    generatedAt: generatedAt.toISOString(),
    source: "loyal_yield.vault_idle_token_balances_current",
    sourceOfTruthNote:
      "Neon rows are the current projection. With --verify-chain, finalized Solana balances are the authoritative current amounts.",
    filters: { mint: options.mint },
    coverage,
    chainSlots,
    summaries,
    vaults: rows,
  };
  const json = JSON.stringify(report, jsonReplacer, 2);

  if (options.jsonPath) {
    await Bun.write(options.jsonPath, `${json}\n`);
  }
  if (options.csvPath) {
    const relevantRows = rows.filter(
      (row) =>
        row.neonAmountRaw > 0n ||
        (row.chainAmountRaw !== undefined && row.chainAmountRaw > 0n) ||
        (row.chainAmountRaw !== undefined &&
          row.chainAmountRaw !== row.neonAmountRaw),
    );
    await Bun.write(options.csvPath, `${rowsToCsv(relevantRows)}\n`);
  }
  if (options.json) {
    console.log(json);
  } else {
    printTextReport({
      chainSlots,
      coverage,
      generatedAt: generatedAt.toISOString(),
      rows,
      summaries,
      top: options.top,
    });
  }
}

if (import.meta.main) {
  main().catch((error) => {
    console.error(error instanceof Error ? error.message : String(error));
    process.exitCode = 1;
  });
}
