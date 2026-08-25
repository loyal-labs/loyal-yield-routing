import { createHash } from "node:crypto";
import { existsSync, readFileSync, readdirSync } from "node:fs";
import { resolve } from "node:path";

const PASS = "PASS_RWA_OBSERVATION_BACKFILL_V1";
const FAIL = "FAIL_RWA_OBSERVATION_BACKFILL_V1";
const BLOCKED = "BLOCKED_RWA_OBSERVATION_BACKFILL_V1";
const ROOT = resolve(import.meta.dir, "..");
const SOURCE_FILE = resolve(process.env.RWA_BACKFILL_DIR ?? "/private/tmp/rwa-observation-backfill-v1", "history.jsonl");
const START_ISO = "2026-06-24T00:00:00.000Z";
const END_ISO = "2026-08-24T00:00:00.000Z";
const MIGRATION = "crates/loyal-timescale-migrations/migrations/0008_kamino_historic_backfill_semantics.sql";
const SOURCE = "kamino_api_history";

const RESERVES = [
  ["47tfyEG9SsdEnUm9cw5kY9BXngQGqu3LBoop9j5uTAv8", "6ZxkBSJEqsXA3Kdm2PDAzHLUdPTPUK93Lf4bAezec1UQ", "5Y8NV33Vv7WbnLfq3zBcKSdYPrk7g2KoiQoe7M2tcxp5"],
  ["47tfyEG9SsdEnUm9cw5kY9BXngQGqu3LBoop9j5uTAv8", "AYL4LMc4ZCVyq3Z7XPJGWDM4H9PiWjqXAAuuHBEGVR2Z", "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"],
  ["47tfyEG9SsdEnUm9cw5kY9BXngQGqu3LBoop9j5uTAv8", "3yDc9ARvtPLhYxZLgucZGuBtZ9bHshBvXTwHxGe3nhmC", "USDSwr9ApdHk5bvJKMjzff41FfuX8bSxdKcR81vTwcA"],
  ["CqAoLuqWtavaVE8deBjMKe8ZfSt9ghR6Vb8nfsyabyHA", "BUTND9T7Ux4KR8RAEgd4WoZwnP7xA279oA1y3iPVcvSh", "3b8X44fLF9ooXaUm3hhSgjpmVs6rZZ3pPoGnGahc3Uu7"],
  ["CqAoLuqWtavaVE8deBjMKe8ZfSt9ghR6Vb8nfsyabyHA", "9GJ9GBRwCp4pHmWrQ43L5xpc9Vykg7jnfwcFGN8FoHYu", "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"],
  ["CqAoLuqWtavaVE8deBjMKe8ZfSt9ghR6Vb8nfsyabyHA", "3ZUAwhEtK8XWfK4fy98z4yoptm4GeyeAu21L11HPXaZ5", "2b1kV6DkPAnxd5ixfnxCpjxmKwqjjaYmCZfHsFu24GXo"],
  ["CqAoLuqWtavaVE8deBjMKe8ZfSt9ghR6Vb8nfsyabyHA", "7SzMWArC8WAenndXFmRyfvcvrNPodqUFkmPrmmoRZvn4", "USDSwr9ApdHk5bvJKMjzff41FfuX8bSxdKcR81vTwcA"],
  ["6WEGfej9B9wjxRs6t4BYpb9iCXd8CpTpJ8fVSNzHCC5y", "AwCyCPZYJSZ93xcVKNK7jR8e1BHzJXq1D4bReNuh9woY", "AvZZF1YaZDziPY2RCK4oJrRVrbN3mTD9NL24hPeaZeUj"],
  ["6WEGfej9B9wjxRs6t4BYpb9iCXd8CpTpJ8fVSNzHCC5y", "Atj6UREVWa7WxbF2EMKNyfmYUY1U1txughe2gjhcPDCo", "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"],
  ["6WEGfej9B9wjxRs6t4BYpb9iCXd8CpTpJ8fVSNzHCC5y", "92qeAka3ZzCGPfJriDXrE7tiNqfATVCAM6ZjjctR3TrS", "2b1kV6DkPAnxd5ixfnxCpjxmKwqjjaYmCZfHsFu24GXo"],
] as const;

type Json = Record<string, unknown>;
type SourceEvidence = { rows: number; counts: Record<string, number>; digest: string };

function emit(verdict: string, condition: string, evidence: Json, exitCode: number): never {
  console.log(JSON.stringify({ verdict, condition, evidence }, null, 2));
  console.log(`${verdict} ${condition}`);
  process.exit(exitCode);
}
function fail(condition: string, evidence: Json = {}): never { return emit(FAIL, condition, evidence, 2); }
function blocked(condition: string, evidence: Json = {}): never { return emit(BLOCKED, condition, evidence, 2); }
function file(path: string): string {
  const absolute = resolve(ROOT, path);
  if (!existsSync(absolute)) fail("required_source_missing", { path });
  return readFileSync(absolute, "utf8");
}
function requireText(source: string, value: string, path: string): void {
  if (!source.includes(value)) fail("source_contract_missing", { path, value });
}
function hash(algorithm: string, value: string): string { return createHash(algorithm).update(value).digest("hex"); }
function object(value: unknown, condition = "unexpected_json_shape"): Json {
  if (typeof value !== "object" || value === null || Array.isArray(value)) fail(condition);
  return value as Json;
}
async function command(argv: string[], kind: "local" | "external", env = process.env): Promise<string> {
  const child = Bun.spawn(argv, { cwd: ROOT, env, stdout: "pipe", stderr: "pipe" });
  const [exitCode, stdout, stderr] = await Promise.all([child.exited, new Response(child.stdout).text(), new Response(child.stderr).text()]);
  if (exitCode !== 0) {
    const evidence = { command: argv[0], exitCode, stderrTail: stderr.split(/\r?\n/).slice(-12).join("\n") };
    if (kind === "external") blocked("external_read_unavailable", evidence);
    fail("local_verification_failed", evidence);
  }
  return stdout.trim();
}

function staticContract(): void {
  const pkg = JSON.parse(file("package.json")) as { scripts?: Record<string, string> };
  if (pkg.scripts?.["verify:rwa-observation-backfill-v1"] !== "bun scripts/verify-rwa-observation-backfill.ts") fail("verifier_entrypoint_mismatch");
  if (pkg.scripts?.["backfill:rwa-observation-history"] !== "bun scripts/backfill-rwa-observation-history.ts") fail("backfill_entrypoint_mismatch");
  const competitors = readdirSync(resolve(ROOT, "scripts")).filter((name) => name.startsWith("verify-rwa-observation-backfill") && name !== "verify-rwa-observation-backfill.ts");
  if (competitors.length) fail("competing_verifier_found", { competitors });
  const backfillPath = "scripts/backfill-rwa-observation-history.ts";
  const backfill = file(backfillPath);
  for (const value of [SOURCE, "/metrics/history", "pg_advisory_xact_lock", "--execute", ...RESERVES.flat()]) requireText(backfill, value, backfillPath);
  if (file("crates/kamino-historic-data/src/cli.rs").includes("earn_max_observation_reserves")) fail("costly_raw_replay_entrypoint_forbidden");
  const migration = file(MIGRATION);
  for (const value of ["substreams_backfill", SOURCE, "ORDER BY reserve, observed_at DESC, event_id DESC"]) requireText(migration, value, MIGRATION);
  if (/\bCREATE\s+TABLE\b/i.test(migration)) fail("duplicate_history_table_forbidden");
  const runnerPath = "crates/loyal-timescale-migrations/src/main.rs";
  const runner = file(runnerPath);
  requireText(runner, "version: 8", runnerPath);
  requireText(runner, "0008_kamino_historic_backfill_semantics.sql", runnerPath);
  if (file("render.yaml").includes("backfill-rwa-observation-history")) fail("historic_service_forbidden");
}

async function localContract(): Promise<void> {
  for (const argv of [
    ["cargo", "fmt", "--all", "--", "--check"],
    ["cargo", "check", "-p", "loyal-timescale-migrations"],
    ["cargo", "test", "-p", "loyal-kamino-data", "targets"],
    ["bun", "scripts/backfill-rwa-observation-history.ts", "--help"],
  ]) await command(argv, "local");
}

function sourceContract(): SourceEvidence {
  if (!existsSync(SOURCE_FILE)) fail("backfill_source_missing", { sourceFile: SOURCE_FILE });
  const identity = new Map(RESERVES.map(([market, reserve, mint]) => [reserve, { market, mint }]));
  const counts = Object.fromEntries(RESERVES.map(([, reserve]) => [reserve, 0]));
  const timestamps = new Map<string, number[]>();
  const keys: string[] = [];
  const rows = readFileSync(SOURCE_FILE, "utf8").split(/\r?\n/).filter(Boolean).map((line) => JSON.parse(line) as Json);
  for (const row of rows) {
    const reserve = String(row.reserve ?? "");
    const expected = identity.get(reserve);
    const snapshot = object(row.snapshot, "backfill_snapshot_invalid");
    if (!expected) fail("backfill_reserve_outside_manifest", { reserve });
    const observedAt = Date.parse(String(row.observed_at));
    if (observedAt < Date.parse(START_ISO) || observedAt >= Date.parse(END_ISO)) fail("backfill_time_outside_window", { reserve, observedAt: row.observed_at });
    if (row.source !== SOURCE || row.source_commitment !== "api_daily") fail("backfill_provenance_invalid", { reserve });
    if (row.market !== expected.market || row.liquidity_mint !== expected.mint) fail("backfill_identity_invalid", { reserve });
    if (snapshot.observation_schema_version !== "kamino_api_history_v1") fail("backfill_schema_invalid", { reserve });
    for (const field of ["supply_apy", "borrow_apy", "total_supply_usd_estimate", "total_borrow_usd_estimate", "available_amount", "utilization", "market_price_usd"]) {
      if (!Number.isFinite(Number(row[field]))) fail("backfill_economic_field_invalid", { reserve, field });
    }
    for (const field of ["loan_to_value_pct", "liquidation_threshold_pct", "borrow_factor_pct", "deposit_limit", "borrow_limit", "borrow_limit_outside_elevation_group", "borrowed_amount_outside_elevation_group", "exchange_rate"]) {
      if (snapshot[field] === undefined || snapshot[field] === null) fail("backfill_decision_field_missing", { reserve, field });
    }
    if (!Array.isArray(snapshot.borrow_rate_curve) || snapshot.borrow_rate_curve.length < 2) fail("backfill_borrow_curve_invalid", { reserve });
    const key = String(row.dedupe_key ?? "");
    if (key !== `api_history:${reserve}:${row.observed_at}`) fail("backfill_dedupe_key_invalid", { reserve });
    keys.push(key);
    counts[reserve] += 1;
    timestamps.set(reserve, [...(timestamps.get(reserve) ?? []), observedAt]);
  }
  if (new Set(keys).size !== keys.length) fail("backfill_source_duplicates", { rows: rows.length });
  for (const [, reserve] of RESERVES) {
    const points = (timestamps.get(reserve) ?? []).sort((a, b) => a - b);
    if (points.length < 1_400 || points[0] !== Date.parse(START_ISO) || Date.parse(END_ISO) - points.at(-1)! !== 3_600_000) fail("backfill_hourly_coverage_invalid", { reserve, points: points.length });
    if (points.some((point, index) => index > 0 && point - points[index - 1]! > 86_400_000)) fail("backfill_history_gap", { reserve });
  }
  keys.sort();
  return { rows: rows.length, counts, digest: hash("md5", keys.join("\n")) };
}

async function databaseContract(source: SourceEvidence): Promise<Json> {
  if (!process.env.TIMESCALEDB_URL) blocked("timescale_environment_missing", { missing: ["TIMESCALEDB_URL"] });
  const values = RESERVES.map(([, reserve]) => `('${reserve}')`).join(",");
  const checksum = hash("sha256", file(MIGRATION));
  const sql = `
WITH required(reserve) AS (VALUES ${values}), scoped AS (
  SELECT updates.*, dedupe.dedupe_key FROM kamino.reserve_updates updates
  JOIN kamino.reserve_update_dedupe dedupe USING (event_id) JOIN required USING (reserve)
  WHERE updates.source = '${SOURCE}' AND observed_at >= '${START_ISO}'::timestamptz AND observed_at < '${END_ISO}'::timestamptz
), counts AS (SELECT reserve, count(*)::int AS row_count FROM scoped GROUP BY reserve), latest AS (
  SELECT count(*)::int AS row_count, count(*) FILTER (WHERE source = '${SOURCE}')::int AS historic_count,
         count(*) FILTER (WHERE observed_at >= '${END_ISO}'::timestamptz)::int AS chronological_count
  FROM kamino.latest_reserve_updates JOIN required USING (reserve)
), live AS (
  SELECT count(*)::int AS row_count, count(*) FILTER (WHERE source = '${SOURCE}')::int AS historic_count
  FROM kamino.latest_verified_reserve_updates JOIN required USING (reserve) WHERE verified_at >= now() - interval '10 minutes'
)
SELECT json_build_object(
  'rows', (SELECT count(*) FROM scoped), 'reserves', (SELECT count(DISTINCT reserve) FROM scoped),
  'digest', (SELECT md5(string_agg(dedupe_key, E'\\n' ORDER BY dedupe_key)) FROM scoped),
  'reserveCounts', (SELECT json_object_agg(reserve, row_count) FROM counts),
  'decisionRows', (SELECT count(*) FROM scoped WHERE snapshot ->> 'observation_schema_version' = 'kamino_api_history_v1'
    AND snapshot -> 'loan_to_value_pct' IS NOT NULL AND snapshot -> 'liquidation_threshold_pct' IS NOT NULL
    AND snapshot -> 'borrow_factor_pct' IS NOT NULL AND snapshot -> 'deposit_limit' IS NOT NULL
    AND snapshot -> 'borrow_limit' IS NOT NULL AND jsonb_array_length(snapshot -> 'borrow_rate_curve') >= 2),
  'currentRefs', (SELECT count(*) FROM kamino.reserve_current_states state JOIN scoped ON scoped.event_id = state.state_event_id),
  'verificationRefs', (SELECT count(*) FROM kamino.reserve_confirmed_verifications verification JOIN scoped ON scoped.event_id = verification.state_event_id),
  'floorRefs', (SELECT count(*) FROM kamino.reserve_confirmed_observation_floors WHERE source = '${SOURCE}'),
  'latestRows', (SELECT row_count FROM latest), 'latestHistoricRows', (SELECT historic_count FROM latest),
  'latestChronologicalRows', (SELECT chronological_count FROM latest), 'freshLiveRows', (SELECT row_count FROM live),
  'freshLiveHistoricRows', (SELECT historic_count FROM live),
  'migrationCount', (SELECT count(*) FROM loyal.timescale_schema_migrations WHERE version = 8 AND name = 'kamino_historic_backfill_semantics' AND checksum = '${checksum}'),
  'notifyGuarded', position('${SOURCE}' in pg_get_functiondef('kamino.notify_reserve_update()'::regprocedure)) > 0,
  'latestViewChronological', position('observed_at DESC' in pg_get_viewdef('kamino.latest_reserve_updates'::regclass, true)) > 0
);`;
  const output = await command(["sh", "-c", "exec psql \"$TIMESCALEDB_URL\" -X -A -t -v ON_ERROR_STOP=1 -c \"$RWA_SQL\""], "external", { ...process.env, RWA_SQL: sql });
  let parsed: unknown;
  try { parsed = JSON.parse(output); } catch { blocked("timescale_invalid_json", { outputSha256: hash("sha256", output) }); }
  const db = object(parsed);
  const expectedCounts = JSON.stringify(Object.fromEntries(Object.entries(source.counts).sort()));
  const actualCounts = JSON.stringify(Object.fromEntries(Object.entries(object(db.reserveCounts)).sort()));
  for (const [key, expected] of [["rows", source.rows], ["reserves", 10], ["decisionRows", source.rows], ["currentRefs", 0], ["verificationRefs", 0], ["floorRefs", 0], ["latestRows", 10], ["latestHistoricRows", 0], ["latestChronologicalRows", 10], ["freshLiveRows", 10], ["freshLiveHistoricRows", 0], ["migrationCount", 1]] as const) {
    if (Number(db[key]) !== expected) fail("timescale_backfill_contract_mismatch", { key, expected, actual: db[key] });
  }
  if (db.digest !== source.digest || actualCounts !== expectedCounts) fail("timescale_source_reconciliation_failed", { sourceDigest: source.digest, databaseDigest: db.digest });
  if (db.notifyGuarded !== true || db.latestViewChronological !== true) fail("timescale_live_isolation_missing", db);
  return db;
}

async function main(): Promise<void> {
  staticContract();
  await localContract();
  const source = sourceContract();
  const dirty = await command(["git", "status", "--porcelain"], "local");
  if (dirty) fail("release_worktree_not_clean", { changedPathCount: dirty.split(/\r?\n/).length });
  const head = await command(["git", "rev-parse", "HEAD"], "local");
  const origin = await command(["git", "rev-parse", "origin/main"], "local");
  if (head !== origin) fail("release_revision_not_origin_main", { head, origin });
  const database = await databaseContract(source);
  emit(PASS, "exact_api_history_reconciled_and_live_state_isolated", {
    revision: head, window: { start: START_ISO, end: END_ISO },
    source: { file: SOURCE_FILE, rows: source.rows, counts: source.counts, digest: source.digest }, database,
  }, 0);
}

await main();
