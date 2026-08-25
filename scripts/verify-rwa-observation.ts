import { createHash } from "node:crypto";
import { existsSync, readFileSync, readdirSync } from "node:fs";
import { resolve } from "node:path";

const PASS = "PASS_RWA_OBSERVATION_V1";
const FAIL = "FAIL_RWA_OBSERVATION_V1";
const BLOCKED = "BLOCKED_RWA_OBSERVATION_V1";
const ROOT = resolve(import.meta.dir, "..");
const SERVICE_ID = process.env.KAMINO_RENDER_SERVICE_ID ?? "srv-d8h4i9a8pkls73bver00";
const SERVICE_NAME = "loyal-kamino-reserve-monitor";
const IMAGE_PREFIX = "ghcr.io/loyal-labs/loyal-yield-routing/laserstream-workers:sha-";
const MIGRATION = "crates/loyal-timescale-migrations/migrations/0007_kamino_rwa_decision_observations.sql";
const FRESHNESS_MINUTES = Number(process.env.RWA_OBSERVATION_FRESHNESS_MINUTES ?? "10");

type Json = Record<string, unknown>;

function emit(verdict: string, condition: string, evidence: Json, exitCode: number): never {
  process.stdout.write(`${JSON.stringify({ verdict, condition, evidence }, null, 2)}\n`);
  process.stdout.write(`${verdict} ${condition}\n`);
  process.exit(exitCode);
}

function fail(condition: string, evidence: Json = {}): never {
  return emit(FAIL, condition, evidence, 2);
}

function blocked(condition: string, evidence: Json = {}): never {
  return emit(BLOCKED, condition, evidence, 2);
}

function requireFile(path: string): string {
  const absolute = resolve(ROOT, path);
  if (!existsSync(absolute)) fail("required_source_missing", { path });
  return readFileSync(absolute, "utf8");
}

function requireText(source: string, text: string, path: string): void {
  if (!source.includes(text)) fail("source_contract_missing", { path, text });
}

function sha256(value: string): string {
  return createHash("sha256").update(value).digest("hex");
}

function earnMaxReserves(): string[] {
  const actions = requireFile("crates/loyal-actions/src/earn_max.rs");
  const reserves = [...actions.matchAll(
    /pub const EARN_MAX_[A-Z_]+_RESERVE: &str =\s*"([1-9A-HJ-NP-Za-km-z]{32,44})";/g,
  )].map((match) => match[1]);
  if (reserves.length !== 10 || new Set(reserves).size !== 10) {
    fail("earn_max_observation_manifest_invalid", { reserveCount: reserves.length });
  }
  return reserves;
}

async function command(
  argv: string[],
  kind: "local" | "external",
  env: Record<string, string | undefined> = process.env,
): Promise<string> {
  const child = Bun.spawn(argv, { cwd: ROOT, env, stdout: "pipe", stderr: "pipe" });
  const [exitCode, stdout, stderr] = await Promise.all([
    child.exited,
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
  ]);
  if (exitCode !== 0) {
    const evidence = {
      command: argv[0],
      exitCode,
      stderrTail: stderr.split(/\r?\n/).slice(-12).join("\n"),
    };
    if (kind === "external") blocked("external_read_unavailable", evidence);
    fail("local_verification_failed", evidence);
  }
  return stdout.trim();
}

function parseJson(value: string, condition: string): unknown {
  try {
    return JSON.parse(value);
  } catch {
    fail(condition, { outputSha256: sha256(value) });
  }
}

function object(value: unknown): Json {
  if (typeof value !== "object" || value === null || Array.isArray(value)) {
    fail("unexpected_external_shape");
  }
  return value as Json;
}

function staticContract(): void {
  const pkg = JSON.parse(requireFile("package.json")) as { scripts?: Record<string, string> };
  if (pkg.scripts?.["verify:rwa-observation-v1"] !== "bun scripts/verify-rwa-observation.ts") {
    fail("verifier_entrypoint_mismatch");
  }
  const competing = readdirSync(resolve(ROOT, "scripts"))
    .filter((name) => name.startsWith("verify-rwa-observation") && name !== "verify-rwa-observation.ts");
  if (competing.length > 0) fail("competing_verifier_found", { competing });

  const codecPath = "crates/loyal-kamino-codec/src/apy.rs";
  const codec = requireFile(codecPath);
  for (const field of [
    "reserve_status", "emergency_mode", "loan_to_value_pct",
    "liquidation_threshold_pct", "borrow_factor_pct", "deposit_limit",
    "borrow_limit", "utilization_limit_block_borrowing_above_pct",
    "disable_usage_as_coll_outside_emode", "borrow_limit_outside_elevation_group",
    "borrowed_amount_outside_elevation_group", "origination_fee_sf",
    "flash_loan_fee_sf", "borrow_rate_curve", "deposit_withdrawal_cap",
    "debt_withdrawal_cap",
  ]) requireText(codec, `pub ${field}:`, codecPath);
  requireText(codec, "previous.borrow_rate_curve != current.borrow_rate_curve", codecPath);

  const actionsPath = "crates/loyal-actions/src/earn_max.rs";
  requireText(requireFile(actionsPath), "EARN_MAX_OBSERVATION_RESERVES", actionsPath);
  const manifestReserves = earnMaxReserves();
  const configPath = "crates/loyal-fleet-worker/src/multiply/config.rs";
  const config = requireFile(configPath);
  for (const reserve of manifestReserves) {
    if (config.includes(reserve)) fail("duplicate_rwa_reserve_manifest", { path: configPath });
  }
  requireText(config, "EARN_MAX_ONYC_COLLATERAL_RESERVE", configPath);
  requireText(config, "EARN_MAX_PRIME_COLLATERAL_RESERVE", configPath);
  requireText(config, "EARN_MAX_SYRUP_COLLATERAL_RESERVE", configPath);
  const targetsPath = "crates/loyal-kamino-data/src/targets.rs";
  requireText(requireFile(targetsPath), "fetch_earn_max_observation_targets", targetsPath);
  const writerPath = "crates/loyal-kamino-data/src/timescale.rs";
  requireText(requireFile(writerPath), "serde_json::to_value(record.snapshot)", writerPath);
  const monitorPath = "crates/kamino-reserve-monitor/src/main.rs";
  requireText(requireFile(monitorPath), "merge_observation_targets", monitorPath);

  const migration = requireFile(MIGRATION);
  for (const field of [
    "reserve_status", "emergency_mode", "loan_to_value_pct",
    "liquidation_threshold_pct", "borrow_factor_pct", "deposit_limit",
    "borrow_limit", "utilization_limit_block_borrowing_above_pct",
    "disable_usage_as_coll_outside_emode", "borrow_limit_outside_elevation_group",
    "borrowed_amount_outside_elevation_group", "origination_fee_sf",
    "flash_loan_fee_sf", "borrow_rate_curve", "deposit_withdrawal_cap",
    "debt_withdrawal_cap",
  ]) requireText(migration, field, MIGRATION);
  const runnerPath = "crates/loyal-timescale-migrations/src/main.rs";
  const runner = requireFile(runnerPath);
  requireText(runner, "version: 7", runnerPath);
  requireText(runner, "0007_kamino_rwa_decision_observations.sql", runnerPath);

  const renderPath = "render.yaml";
  const render = requireFile(renderPath);
  requireText(render, `name: ${SERVICE_NAME}`, renderPath);
  requireText(render, "runtime: image", renderPath);
  requireText(render, "preDeployCommand: /usr/local/bin/kamino-monitor-predeploy", renderPath);
  requireText(render, "dockerCommand: /usr/local/bin/kamino-reserve-monitor", renderPath);
  if (!/laserstream-workers:sha-[0-9a-f]{40}/.test(render)) fail("render_image_not_immutable");
  const dockerfile = requireFile("Dockerfile.laserstream-workers");
  requireText(dockerfile, "kamino-reserve-monitor", "Dockerfile.laserstream-workers");
  requireText(dockerfile, "loyal-timescale-migrations", "Dockerfile.laserstream-workers");
  requireText(dockerfile, "kamino-monitor-predeploy", "Dockerfile.laserstream-workers");
}

async function localContract(): Promise<void> {
  for (const argv of [
    ["cargo", "fmt", "--all", "--", "--check"],
    ["cargo", "test", "-p", "loyal-kamino-codec"],
    ["cargo", "test", "-p", "loyal-kamino-data", "targets"],
    ["cargo", "test", "-p", "kamino-reserve-monitor", "--lib"],
    ["cargo", "check", "-p", "kamino-reserve-monitor"],
    ["cargo", "check", "-p", "loyal-timescale-migrations"],
  ]) await command(argv, "local");
}

async function renderContract(head: string): Promise<{ deployedAt: string; image: string }> {
  if (!process.env.RENDER_API_KEY) blocked("render_environment_missing", { missing: ["RENDER_API_KEY"] });
  const services = parseJson(await command(["render", "services", "--output", "json"], "external"), "render_services_invalid_json");
  if (!Array.isArray(services)) fail("render_services_invalid_shape");
  const row = services.map(object).find((entry) => object(entry.service).id === SERVICE_ID);
  if (!row) fail("render_service_missing", { serviceId: SERVICE_ID });
  const service = object(row.service);
  const details = object(service.serviceDetails);
  const envDetails = object(details.envSpecificDetails);
  const image = String(service.imagePath ?? "");
  const expectedImage = `${IMAGE_PREFIX}${head}`;
  if (
    service.name !== SERVICE_NAME || service.type !== "background_worker" ||
    details.runtime !== "image" || envDetails.dockerCommand !== "/usr/local/bin/kamino-reserve-monitor" ||
    envDetails.preDeployCommand !== "/usr/local/bin/kamino-monitor-predeploy" ||
    object(service.registryCredential).name !== "loyal-ghcr" || image !== expectedImage
  ) fail("render_service_contract_mismatch", { serviceId: SERVICE_ID, image, expectedImage });

  const deploys = parseJson(
    await command(["render", "deploys", "list", SERVICE_ID, "--output", "json"], "external"),
    "render_deploys_invalid_json",
  );
  if (!Array.isArray(deploys) || deploys.length === 0) fail("render_deploy_missing");
  const latest = object(deploys[0]);
  if (latest.status !== "live") fail("render_deploy_not_live", { deployId: latest.id, status: latest.status });

  let response: Response;
  try {
    response = await fetch(`https://api.render.com/v1/services/${SERVICE_ID}/env-vars`, {
      headers: { Authorization: `Bearer ${process.env.RENDER_API_KEY}` },
      signal: AbortSignal.timeout(20_000),
    });
  } catch (error) {
    blocked("render_env_read_unavailable", { error: String(error) });
  }
  if (!response.ok) blocked("render_env_read_unavailable", { status: response.status });
  const envRows = await response.json();
  if (!Array.isArray(envRows)) fail("render_env_invalid_shape");
  const vars = new Map<string, string>();
  for (const raw of envRows) {
    const envVar = object(object(raw).envVar);
    vars.set(String(envVar.key ?? ""), String(envVar.value ?? ""));
  }
  for (const key of ["TIMESCALEDB_URL", "SOLANA_RPC_URL", "HELIUS_API_KEY"]) {
    if (!vars.get(key)?.trim()) fail("render_secret_binding_missing", { key });
  }
  for (const [key, value] of [
    ["KAMINO_UPDATE_SOURCE", "laserstream"],
    ["LASERSTREAM_ENDPOINT", "https://laserstream-mainnet-ewr.helius-rpc.com"],
    ["KAMINO_API_BASE", "https://api.kamino.finance"],
  ]) if (vars.get(key) !== value) fail("render_env_contract_mismatch", { key });

  return { deployedAt: String(latest.finishedAt ?? latest.createdAt), image };
}

async function timescaleContract(deployedAt: string): Promise<Json> {
  if (!process.env.TIMESCALEDB_URL) blocked("timescale_environment_missing", { missing: ["TIMESCALEDB_URL"] });
  if (!Number.isFinite(FRESHNESS_MINUTES) || FRESHNESS_MINUTES < 1 || FRESHNESS_MINUTES > 60) {
    fail("invalid_freshness_window", { freshnessMinutes: FRESHNESS_MINUTES });
  }
  const migrationChecksum = sha256(requireFile(MIGRATION));
  const rwaReserves = earnMaxReserves();
  const values = rwaReserves.map((reserve) => `('${reserve}')`).join(",");
  const sql = `
WITH required(reserve) AS (VALUES ${values}),
covered AS (
  SELECT required.reserve, latest.*
  FROM required
  LEFT JOIN kamino.latest_verified_reserve_updates latest USING (reserve)
),
live_stream AS (
  SELECT count(DISTINCT updates.reserve)::int AS reserve_count,
         count(*)::int AS row_count
  FROM kamino.reserve_updates updates
  JOIN required USING (reserve)
  WHERE updates.source = 'laserstream_grpc'
    AND updates.received_at >= now() - interval '${FRESHNESS_MINUTES} minutes'
    AND updates.observed_at >= '${deployedAt}'::timestamptz
),
invalid_floors AS (
  SELECT count(*)::int AS row_count
  FROM kamino.reserve_confirmed_observation_floors floors
  JOIN required USING (reserve)
  WHERE NOT floors.state_valid
),
migration AS (
  SELECT count(*)::int AS row_count
  FROM loyal.timescale_schema_migrations
  WHERE version = 7
    AND name = 'kamino_rwa_decision_observations'
    AND checksum = '${migrationChecksum}'
)
SELECT json_build_object(
  'requiredReserveCount', (SELECT count(*) FROM required),
  'coveredReserveCount', count(event_id),
  'freshVerifiedReserveCount', count(*) FILTER (
    WHERE event_id IS NOT NULL
      AND verified_at >= now() - interval '${FRESHNESS_MINUTES} minutes'
      AND observed_at >= '${deployedAt}'::timestamptz
  ),
  'decisionFieldReserveCount', count(*) FILTER (
    WHERE reserve_status IS NOT NULL
      AND emergency_mode IS NOT NULL
      AND loan_to_value_pct IS NOT NULL
      AND liquidation_threshold_pct IS NOT NULL
      AND borrow_factor_pct IS NOT NULL
      AND deposit_limit IS NOT NULL
      AND borrow_limit IS NOT NULL
      AND utilization_limit_block_borrowing_above_pct IS NOT NULL
      AND disable_usage_as_coll_outside_emode IS NOT NULL
      AND borrow_limit_outside_elevation_group IS NOT NULL
      AND borrowed_amount_outside_elevation_group IS NOT NULL
      AND origination_fee_sf IS NOT NULL
      AND flash_loan_fee_sf IS NOT NULL
      AND jsonb_array_length(borrow_rate_curve) = 11
      AND deposit_withdrawal_cap IS NOT NULL
      AND debt_withdrawal_cap IS NOT NULL
  ),
  'confirmedCommitmentReserveCount', count(*) FILTER (
    WHERE source_commitment = 'confirmed'
      AND verification_commitment = 'confirmed'
      AND account_data_hash IS NOT NULL
  ),
  'recentLaserstreamReserveCount', (SELECT reserve_count FROM live_stream),
  'recentLaserstreamRowCount', (SELECT row_count FROM live_stream),
  'invalidFloorCount', (SELECT row_count FROM invalid_floors),
  'migrationCount', (SELECT row_count FROM migration),
  'oldestVerifiedAt', min(verified_at),
  'newestObservedAt', max(observed_at)
) FROM covered;`;
  const output = await command(
    ["sh", "-c", "exec psql \"$TIMESCALEDB_URL\" -X -A -t -v ON_ERROR_STOP=1 -c \"$RWA_SQL\""],
    "external",
    { ...process.env, RWA_SQL: sql },
  );
  const evidence = object(parseJson(output, "timescale_invalid_json"));
  for (const key of [
    "coveredReserveCount", "freshVerifiedReserveCount", "decisionFieldReserveCount",
    "confirmedCommitmentReserveCount",
  ]) if (Number(evidence[key]) !== rwaReserves.length) fail("timescale_rwa_coverage_incomplete", { key, ...evidence });
  if (Number(evidence.recentLaserstreamRowCount) < 1) fail("timescale_laserstream_not_collecting", evidence);
  if (Number(evidence.invalidFloorCount) !== 0) fail("timescale_invalid_observation_floor", evidence);
  if (Number(evidence.migrationCount) !== 1) fail("timescale_migration_identity_mismatch", evidence);
  return evidence;
}

async function main(): Promise<void> {
  staticContract();
  await localContract();
  const dirty = await command(["git", "status", "--porcelain"], "local");
  if (dirty) fail("release_worktree_not_clean", { changedPathCount: dirty.split(/\r?\n/).length });
  const head = await command(["git", "rev-parse", "HEAD"], "local");
  const originMain = await command(["git", "rev-parse", "origin/main"], "local");
  if (head !== originMain) fail("release_revision_not_origin_main", { head, originMain });
  const render = await renderContract(head);
  const timescale = await timescaleContract(render.deployedAt);
  emit(PASS, "deployed_and_collecting", {
    revision: head,
    serviceId: SERVICE_ID,
    image: render.image,
    deployedAt: render.deployedAt,
    freshnessMinutes: FRESHNESS_MINUTES,
    timescale,
  }, 0);
}

await main();
