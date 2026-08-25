import { createHash } from "node:crypto";
import { mkdirSync, readFileSync } from "node:fs";
import { dirname, resolve } from "node:path";

const SOURCE = "kamino_api_history";
const COMMITMENT = "api_daily";
const DEFAULT_START = "2026-06-24T00:00:00.000Z";
const DEFAULT_END = "2026-08-24T00:00:00.000Z";
const DEFAULT_OUTPUT = "/private/tmp/rwa-observation-backfill-v1/history.jsonl";
const API_BASE = "https://api.kamino.finance";

const RESERVES = [
  { market: "47tfyEG9SsdEnUm9cw5kY9BXngQGqu3LBoop9j5uTAv8", marketName: "OnRe Market", reserve: "6ZxkBSJEqsXA3Kdm2PDAzHLUdPTPUK93Lf4bAezec1UQ", mint: "5Y8NV33Vv7WbnLfq3zBcKSdYPrk7g2KoiQoe7M2tcxp5" },
  { market: "47tfyEG9SsdEnUm9cw5kY9BXngQGqu3LBoop9j5uTAv8", marketName: "OnRe Market", reserve: "AYL4LMc4ZCVyq3Z7XPJGWDM4H9PiWjqXAAuuHBEGVR2Z", mint: "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v" },
  { market: "47tfyEG9SsdEnUm9cw5kY9BXngQGqu3LBoop9j5uTAv8", marketName: "OnRe Market", reserve: "3yDc9ARvtPLhYxZLgucZGuBtZ9bHshBvXTwHxGe3nhmC", mint: "USDSwr9ApdHk5bvJKMjzff41FfuX8bSxdKcR81vTwcA" },
  { market: "CqAoLuqWtavaVE8deBjMKe8ZfSt9ghR6Vb8nfsyabyHA", marketName: "Figure Market", reserve: "BUTND9T7Ux4KR8RAEgd4WoZwnP7xA279oA1y3iPVcvSh", mint: "3b8X44fLF9ooXaUm3hhSgjpmVs6rZZ3pPoGnGahc3Uu7" },
  { market: "CqAoLuqWtavaVE8deBjMKe8ZfSt9ghR6Vb8nfsyabyHA", marketName: "Figure Market", reserve: "9GJ9GBRwCp4pHmWrQ43L5xpc9Vykg7jnfwcFGN8FoHYu", mint: "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v" },
  { market: "CqAoLuqWtavaVE8deBjMKe8ZfSt9ghR6Vb8nfsyabyHA", marketName: "Figure Market", reserve: "3ZUAwhEtK8XWfK4fy98z4yoptm4GeyeAu21L11HPXaZ5", mint: "2b1kV6DkPAnxd5ixfnxCpjxmKwqjjaYmCZfHsFu24GXo" },
  { market: "CqAoLuqWtavaVE8deBjMKe8ZfSt9ghR6Vb8nfsyabyHA", marketName: "Figure Market", reserve: "7SzMWArC8WAenndXFmRyfvcvrNPodqUFkmPrmmoRZvn4", mint: "USDSwr9ApdHk5bvJKMjzff41FfuX8bSxdKcR81vTwcA" },
  { market: "6WEGfej9B9wjxRs6t4BYpb9iCXd8CpTpJ8fVSNzHCC5y", marketName: "Maple Market", reserve: "AwCyCPZYJSZ93xcVKNK7jR8e1BHzJXq1D4bReNuh9woY", mint: "AvZZF1YaZDziPY2RCK4oJrRVrbN3mTD9NL24hPeaZeUj" },
  { market: "6WEGfej9B9wjxRs6t4BYpb9iCXd8CpTpJ8fVSNzHCC5y", marketName: "Maple Market", reserve: "Atj6UREVWa7WxbF2EMKNyfmYUY1U1txughe2gjhcPDCo", mint: "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v" },
  { market: "6WEGfej9B9wjxRs6t4BYpb9iCXd8CpTpJ8fVSNzHCC5y", marketName: "Maple Market", reserve: "92qeAka3ZzCGPfJriDXrE7tiNqfATVCAM6ZjjctR3TrS", mint: "2b1kV6DkPAnxd5ixfnxCpjxmKwqjjaYmCZfHsFu24GXo" },
] as const;

type Metrics = Record<string, unknown>;
type SourceRecord = { timestamp?: unknown; metrics?: Metrics };
type Observation = Record<string, unknown> & {
  account_data_hash: string;
  dedupe_key: string;
  observed_at: string;
  reserve: string;
  snapshot: Record<string, unknown>;
};

function usage(): never {
  console.log(`Usage:
  bun scripts/backfill-rwa-observation-history.ts [--start ISO] [--end ISO] [--output PATH]
  bun scripts/backfill-rwa-observation-history.ts --input PATH --execute

Fetch mode writes validated local JSONL only. Import mode requires TIMESCALEDB_URL and imports that artifact idempotently.`);
  process.exit(0);
}

function parseArgs(argv = process.argv.slice(2)) {
  let start = DEFAULT_START;
  let end = DEFAULT_END;
  let output = DEFAULT_OUTPUT;
  let input: string | undefined;
  let execute = false;
  for (let index = 0; index < argv.length; index += 1) {
    const flag = argv[index]!;
    if (flag === "--help" || flag === "-h") usage();
    if (flag === "--execute") { execute = true; continue; }
    const value = argv[++index];
    if (!value) throw new Error(`${flag} requires a value`);
    if (flag === "--start") start = new Date(value).toISOString();
    else if (flag === "--end") end = new Date(value).toISOString();
    else if (flag === "--output") output = resolve(value);
    else if (flag === "--input") input = resolve(value);
    else throw new Error(`unknown argument: ${flag}`);
  }
  if (Date.parse(start) >= Date.parse(end)) throw new Error("--start must be before --end");
  if (execute && !input) throw new Error("--execute requires a previously validated --input artifact");
  if (input && !execute) throw new Error("--input is only valid with --execute");
  return { end, execute, input, output, start };
}

function number(metrics: Metrics, key: string): number {
  const value = Number(metrics[key]);
  if (!Number.isFinite(value)) throw new Error(`invalid ${key}: ${String(metrics[key])}`);
  return value;
}

function normalize(
  identity: (typeof RESERVES)[number],
  timestamp: string,
  metrics: Metrics,
): Observation {
  if (String(metrics.mintAddress) !== identity.mint) throw new Error(`mint mismatch for ${identity.reserve}`);
  const supplyApy = number(metrics, "supplyInterestAPY");
  const borrowApy = number(metrics, "borrowInterestAPY");
  const totalSupply = number(metrics, "totalSupply");
  const totalBorrows = number(metrics, "totalBorrows");
  const rawHash = createHash("sha256").update(JSON.stringify(metrics)).digest("hex");
  const snapshot = {
    observation_schema_version: "kamino_api_history_v1",
    reserve_status_text: String(metrics.status),
    loan_to_value_pct: number(metrics, "loanToValue") * 100,
    liquidation_threshold_pct: number(metrics, "liquidationThreshold") * 100,
    borrow_factor_pct: number(metrics, "borrowFactor"),
    deposit_limit: String(metrics.reserveDepositLimit),
    borrow_limit: String(metrics.reserveBorrowLimit),
    borrow_limit_outside_elevation_group: String(metrics.borrowLimitOutsideElevationGroup),
    borrowed_amount_outside_elevation_group: String(metrics.borrowOutsideElevationGroup),
    borrow_rate_curve: metrics.borrowCurve,
    exchange_rate: String(metrics.exchangeRate),
    api_metrics: metrics,
  };
  return {
    account_data_hash: rawHash,
    available_amount: number(metrics, "totalLiquidity"),
    borrow_apr: Math.log1p(borrowApy),
    borrow_apy: borrowApy,
    borrowed_amount: totalBorrows,
    borrowed_amount_sf: String(metrics.totalBorrows),
    cumulative_borrow_rate_bsf: "0",
    dedupe_key: `api_history:${identity.reserve}:${timestamp}`,
    host_fixed_interest_rate_bps: number(metrics, "hostFixedInterestRate") * 10_000,
    liquidity_mint: identity.mint,
    market: identity.market,
    market_name: identity.marketName,
    market_price_last_updated_ts: Math.floor(Date.parse(timestamp) / 1000),
    market_price_usd: number(metrics, "assetPriceUSD"),
    mint_decimals: number(metrics, "decimals"),
    observed_at: timestamp,
    protocol_take_rate_pct: number(metrics, "protocolTakeRate") * 100,
    reserve: identity.reserve,
    snapshot,
    source: SOURCE,
    source_commitment: COMMITMENT,
    supply_apr: Math.log1p(supplyApy),
    supply_apy: supplyApy,
    symbol: String(metrics.symbol),
    target: { market: identity.market, market_name: identity.marketName, reserve: identity.reserve, liquidity_mint: identity.mint, symbol: String(metrics.symbol) },
    total_borrow_usd_estimate: number(metrics, "borrowTvl"),
    total_supply_amount: totalSupply,
    total_supply_usd_estimate: number(metrics, "depositTvl"),
    utilization: totalSupply === 0 ? 0 : totalBorrows / totalSupply,
  };
}

function validateRows(rows: Observation[], start: string, end: string): void {
  const startMs = Date.parse(start);
  const endMs = Date.parse(end);
  const identities = new Map(RESERVES.map((entry) => [entry.reserve, entry]));
  const perReserve = new Map<string, number[]>();
  const dedupe = new Set<string>();
  for (const row of rows) {
    const identity = identities.get(row.reserve);
    if (!identity || row.source !== SOURCE || row.source_commitment !== COMMITMENT) throw new Error("artifact provenance mismatch");
    if (row.market !== identity.market || row.liquidity_mint !== identity.mint) throw new Error(`artifact identity mismatch for ${row.reserve}`);
    const timestamp = Date.parse(row.observed_at);
    if (timestamp < startMs || timestamp >= endMs) throw new Error(`timestamp outside window: ${row.observed_at}`);
    if (row.snapshot.observation_schema_version !== "kamino_api_history_v1") throw new Error("artifact schema mismatch");
    if (dedupe.has(row.dedupe_key)) throw new Error(`duplicate ${row.dedupe_key}`);
    dedupe.add(row.dedupe_key);
    const points = perReserve.get(row.reserve) ?? [];
    points.push(timestamp);
    perReserve.set(row.reserve, points);
  }
  for (const identity of RESERVES) {
    const points = (perReserve.get(identity.reserve) ?? []).sort((a, b) => a - b);
    if (points.length === 0) throw new Error(`no history for ${identity.reserve}`);
    if (points[0]! !== startMs || endMs - points.at(-1)! > 24 * 60 * 60 * 1000) throw new Error(`incomplete boundary coverage for ${identity.reserve}`);
    for (let index = 1; index < points.length; index += 1) {
      if (points[index]! - points[index - 1]! > 24 * 60 * 60 * 1000) throw new Error(`history gap for ${identity.reserve}`);
    }
  }
}

async function fetchArtifact(start: string, end: string): Promise<Observation[]> {
  const batches = await Promise.all(RESERVES.map(async (identity) => {
    const url = `${API_BASE}/kamino-market/${identity.market}/reserves/${identity.reserve}/metrics/history?env=mainnet-beta&start=${encodeURIComponent(start)}&end=${encodeURIComponent(end)}`;
    const response = await fetch(url);
    if (!response.ok) throw new Error(`Kamino history HTTP ${response.status} for ${identity.reserve}`);
    const body = await response.json() as { reserve?: unknown; history?: SourceRecord[] };
    if (body.reserve !== identity.reserve || !Array.isArray(body.history)) throw new Error(`unexpected Kamino history response for ${identity.reserve}`);
    return body.history
      .filter((entry): entry is Required<SourceRecord> => typeof entry.timestamp === "string" && !!entry.metrics)
      .map((entry) => ({ timestamp: new Date(String(entry.timestamp)).toISOString(), metrics: entry.metrics }))
      .filter((entry) => Date.parse(entry.timestamp) >= Date.parse(start) && Date.parse(entry.timestamp) < Date.parse(end))
      .map((entry) => normalize(identity, entry.timestamp, entry.metrics));
  }));
  const rows = batches.flat().sort((a, b) => a.observed_at.localeCompare(b.observed_at) || a.reserve.localeCompare(b.reserve));
  validateRows(rows, start, end);
  return rows;
}

function loadArtifact(path: string): Observation[] {
  const rows = readFileSync(path, "utf8").split(/\r?\n/).filter(Boolean).map((line) => JSON.parse(line) as Observation);
  validateRows(rows, DEFAULT_START, DEFAULT_END);
  return rows;
}

async function importArtifact(rows: Observation[]): Promise<void> {
  if (!process.env.TIMESCALEDB_URL) throw new Error("TIMESCALEDB_URL is required with --execute");
  const payload = JSON.stringify(rows);
  if (payload.includes("$rwa_history$")) throw new Error("unexpected SQL delimiter in payload");
  const sql = `
BEGIN;
SELECT pg_advisory_xact_lock(hashtextextended('kamino_api_history_import_v1', 0));
WITH incoming AS (
  SELECT value AS data FROM jsonb_array_elements($rwa_history$${payload}$rwa_history$::jsonb)
), candidates AS (
  SELECT data FROM incoming
  WHERE NOT EXISTS (
    SELECT 1 FROM kamino.reserve_update_dedupe dedupe
    WHERE dedupe.dedupe_key = data ->> 'dedupe_key'
  )
), inserted AS (
  INSERT INTO kamino.reserve_updates (
    observed_at, slot, kind, source, source_commitment, reserve, market, market_name, symbol,
    liquidity_mint, mint_decimals, reserve_last_update_slot, reserve_last_update_stale,
    reserve_price_status, available_amount, borrowed_amount, borrowed_amount_sf,
    total_supply_amount, market_price_usd, market_price_last_updated_ts, cumulative_borrow_rate_bsf,
    total_supply_usd_estimate, total_borrow_usd_estimate, utilization, borrow_apr, supply_apr,
    borrow_apy, supply_apy, protocol_take_rate_pct, host_fixed_interest_rate_bps, diff_changed,
    changed_fields, diff_summary, diff, target, snapshot, record, account_data_hash,
    received_at, decoded_at, receive_to_decode_ms, decode_to_insert_ms
  )
  SELECT
    (data ->> 'observed_at')::timestamptz, 0, 'reserve_metric_history', '${SOURCE}', '${COMMITMENT}',
    data ->> 'reserve', data ->> 'market', data ->> 'market_name', data ->> 'symbol',
    data ->> 'liquidity_mint', (data ->> 'mint_decimals')::int, 0, true, 0,
    (data ->> 'available_amount')::double precision, (data ->> 'borrowed_amount')::double precision,
    data ->> 'borrowed_amount_sf', (data ->> 'total_supply_amount')::double precision,
    (data ->> 'market_price_usd')::double precision, (data ->> 'market_price_last_updated_ts')::bigint,
    data ->> 'cumulative_borrow_rate_bsf', (data ->> 'total_supply_usd_estimate')::double precision,
    (data ->> 'total_borrow_usd_estimate')::double precision, (data ->> 'utilization')::double precision,
    (data ->> 'borrow_apr')::double precision, (data ->> 'supply_apr')::double precision,
    (data ->> 'borrow_apy')::double precision, (data ->> 'supply_apy')::double precision,
    round((data ->> 'protocol_take_rate_pct')::numeric)::smallint,
    round((data ->> 'host_fixed_interest_rate_bps')::numeric)::int, true,
    ARRAY['api_daily_metrics'], data ->> 'dedupe_key', '{}'::jsonb, data -> 'target', data -> 'snapshot', data,
    data ->> 'account_data_hash', now(), now(), 0, 0
  FROM candidates
  RETURNING event_id, reserve, observed_at, account_data_hash
), deduped AS (
  INSERT INTO kamino.reserve_update_dedupe (dedupe_key, event_id, reserve, slot, account_data_hash)
  SELECT 'api_history:' || reserve || ':' || to_char(observed_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'),
         event_id, reserve, 0, account_data_hash
  FROM inserted
  RETURNING 1
)
SELECT count(*) AS inserted_rows FROM deduped;
COMMIT;
`;
  const child = Bun.spawn(["sh", "-c", "exec psql \"$TIMESCALEDB_URL\" -X -A -t -v ON_ERROR_STOP=1"], {
    env: process.env,
    stdin: new Blob([sql]),
    stdout: "pipe",
    stderr: "pipe",
  });
  const [exitCode, stdout, stderr] = await Promise.all([child.exited, new Response(child.stdout).text(), new Response(child.stderr).text()]);
  if (exitCode !== 0) throw new Error(`Timescale import failed: ${stderr.split(/\r?\n/).slice(-8).join("\n")}`);
  console.log(stdout.trim());
}

const args = parseArgs();
if (args.execute) {
  const rows = loadArtifact(args.input!);
  await importArtifact(rows);
  console.log(`validated and imported ${rows.length} source rows from ${args.input}`);
} else {
  const rows = await fetchArtifact(args.start, args.end);
  mkdirSync(dirname(args.output), { recursive: true });
  await Bun.write(args.output, `${rows.map((row) => JSON.stringify(row)).join("\n")}\n`);
  console.log(JSON.stringify({ source: SOURCE, output: args.output, rows: rows.length, reserves: RESERVES.length, start: args.start, end: args.end }));
}
