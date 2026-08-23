import { createHash } from "node:crypto";
import { existsSync, readFileSync, readdirSync, statSync } from "node:fs";
import { resolve } from "node:path";

import { neon } from "@neondatabase/serverless";
import { Connection } from "@solana/web3.js";

const CONTRACT_VERSION = "earn-max-v1";
const CONTRACT_SHA256 = "50d25c214d1c813da09f20b8e1c187c756ce31261bf0b645c0795be1058cb3e3";
const PASS = "PASS_EARN_MAX_PRODUCTION_READY";
const FAIL = "FAIL_EARN_MAX_PRODUCTION_READY";
const BLOCKED = "BLOCKED_EARN_MAX_PRODUCTION_READY";
const MAINNET_GENESIS = "5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d";
const ROOT = resolve(import.meta.dir, "..");
const APPS_ROOT = resolve(ROOT, "../loyal-apps");
const CONTRACT = "docs/plans/multiply-rwa-looping-policy-architecture.md";
const CONFIG = "crates/loyal-fleet-worker/src/multiply/config.rs";
const POLICY_MONITOR = "crates/loyal-squads-policy-monitor/src/lib.rs";
const POLICY_MONITOR_MANIFEST = "crates/loyal-squads-policy-monitor/Cargo.toml";
const LASERSTREAM_MONITOR = "crates/balance-sweep-ata-monitor/src/main.rs";
const LASERSTREAM_RECONCILIATION = "crates/balance-sweep-ata-monitor/src/earn_reconciliation.rs";
const WORKER = "crates/loyal-fleet-worker/src/bin/multiply-route-worker.rs";
const MIGRATIONS = "crates/loyal-yield-store/migrations";
const RENDER_BLUEPRINT = "render.yaml";
const APP_API_ROOT = "apps/web/src/app/api/smart-accounts/earn-max";
const APP_FEATURE_ROOT = "apps/web/src/features/earn-max";
const APP_ACTIONS = "packages/loyal-actions/src/earn-max.ts";
const MULTIPLY_STORE = "crates/loyal-yield-store/src/multiply_state_store.rs";

type Json = Record<string, unknown>;

function emit(verdict: string, condition: string, evidence: Json, exitCode: number): never {
  process.stdout.write(`${JSON.stringify({
    contractVersion: CONTRACT_VERSION,
    verdict,
    condition,
    evidence,
  }, null, 2)}\n`);
  process.stdout.write(`${verdict} ${condition}\n`);
  process.exit(exitCode);
}

function fail(condition: string, evidence: Json = {}): never {
  return emit(FAIL, condition, evidence, 2);
}

function blocked(condition: string, evidence: Json = {}): never {
  return emit(BLOCKED, condition, evidence, 2);
}

function sha256(value: string | Uint8Array): string {
  return createHash("sha256").update(value).digest("hex");
}

function file(root: string, relative: string): string {
  const path = resolve(root, relative);
  if (!existsSync(path) || !statSync(path).isFile()) {
    fail("required_source_missing", { path });
  }
  return readFileSync(path, "utf8");
}

function relativeFiles(root: string): string[] {
  if (!existsSync(root)) return [];
  const result: string[] = [];
  const visit = (directory: string) => {
    for (const entry of readdirSync(directory, { withFileTypes: true })) {
      const path = resolve(directory, entry.name);
      if (entry.isDirectory()) visit(path);
      else if (entry.isFile()) result.push(path.slice(root.length + 1));
    }
  };
  visit(root);
  return result.sort();
}

function requireText(source: string, expected: string, condition: string, path: string): void {
  if (!source.includes(expected)) fail(condition, { path, expected });
}

function rejectText(source: string, forbidden: string, condition: string, path: string): void {
  if (source.includes(forbidden)) fail(condition, { path, forbidden });
}

function checkContractIdentity(): Json {
  const contract = file(ROOT, CONTRACT);
  if (sha256(contract) !== CONTRACT_SHA256) {
    fail("authoritative_contract_hash_drift", {
      expected: CONTRACT_SHA256,
      actual: sha256(contract),
      path: CONTRACT,
    });
  }
  for (const expected of [
    `**Version:** \`${CONTRACT_VERSION}\``,
    "op run --env-file=.env.1password -- bun run verify:earn-max:production",
    PASS,
    FAIL,
    BLOCKED,
    "the only product-readiness authority",
  ]) {
    requireText(contract, expected, "authoritative_contract_drift", CONTRACT);
  }
  const obsoleteMarkers = ["PASS", "FAIL", "BLOCKED"].map(
    (prefix) => `${prefix}_RWA_${"MULTIPLY_RELEASE_CANDIDATE"}`,
  );
  for (const obsolete of [...obsoleteMarkers, `One fixed ${"pooled Squads vault"}`]) {
    rejectText(contract, obsolete, "obsolete_product_contract_survived", CONTRACT);
  }

  const packageJson = file(ROOT, "package.json");
  const packageData = JSON.parse(packageJson) as { scripts?: Record<string, string> };
  const scripts = packageData.scripts ?? {};
  if (scripts["verify:earn-max:production"] !== "bun scripts/verify-multiply-production.ts") {
    fail("authoritative_verifier_entrypoint_missing", { path: "package.json" });
  }
  if (scripts[`verify:${"multiply:production"}`]) {
    fail("competing_product_verifier_entrypoint_survived", { path: "package.json" });
  }

  return { contract: CONTRACT, version: CONTRACT_VERSION, sha256: sha256(contract) };
}

function checkPerUserTopology(): Json {
  const config = file(ROOT, CONFIG);
  const fixedIdentityPatterns = [
    /pub const SETTINGS\s*:/,
    /pub const VAULT\s*:/,
    /pub const DELEGATE\s*:/,
    /pub const (?:SYRUP|USDC|PYUSD)_CUSTODY\s*:/,
    /pub const CLAIM_POLICY\s*:/,
    /obligation:\s*"[1-9A-HJ-NP-Za-km-z]+"/,
    /account:\s*"[1-9A-HJ-NP-Za-km-z]+"/,
  ];
  const matches = fixedIdentityPatterns
    .map((pattern) => config.match(pattern)?.[0])
    .filter((value): value is string => Boolean(value));
  if (matches.length > 0) {
    fail("multiply_topology_still_fixed", {
      path: CONFIG,
      matches,
      resume: "derive user-owned accounts and policy PDAs from Settings plus the earn-max-v1 manifest",
    });
  }
  for (const required of ["EarnMaxTopology", "derive_earn_max_topology", "manifest_version"] as const) {
    requireText(config, required, "deterministic_per_user_topology_missing", CONFIG);
  }
  for (const forbidden of ["USX", "guard", "flashBorrow", "flash_borrow"] as const) {
    rejectText(config, forbidden, "forbidden_strategy_or_program_survived", CONFIG);
  }
  return { path: CONFIG, sha256: sha256(config) };
}

function checkMinimalSchemaSource(): Json {
  const migrationRoot = resolve(ROOT, MIGRATIONS);
  const migrations = relativeFiles(migrationRoot).filter((path) => path.endsWith(".sql"));
  const source = migrations.map((path) => file(migrationRoot, path)).join("\n");
  for (const table of [
    "earn_max_policy_sets",
    "multiply_route_states",
    "multiply_operations",
    "multiply_position_snapshots",
  ]) {
    requireText(source, table, "earn_max_schema_table_missing", MIGRATIONS);
  }
  requireText(
    source,
    "multiply_operations_one_nonterminal_per_route",
    "multiply_one_nonterminal_constraint_missing",
    MIGRATIONS,
  );
  for (const forbidden of [
    "earn_max_policy_events",
    "earn_max_decisions",
    "earn_max_commands",
    "earn_max_jobs",
    "earn_max_sagas",
    "earn_max_outbox",
    "earn_max_registry",
    "earn_max_confirmations",
  ]) {
    rejectText(source, forbidden, "forbidden_earn_max_table_survived", MIGRATIONS);
  }
  return { migrations, sha256: sha256(source) };
}

function checkLaserStreamSource(): Json {
  const monitor = file(ROOT, POLICY_MONITOR);
  const manifest = file(ROOT, POLICY_MONITOR_MANIFEST);
  const laserstream = file(ROOT, LASERSTREAM_MONITOR);
  const reconciliation = file(ROOT, LASERSTREAM_RECONCILIATION);
  requireText(manifest, "loyal-fleet-worker", "policy_projection_manifest_contract_missing", POLICY_MONITOR_MANIFEST);
  for (const required of [
    "UpdateSourceKind::Laserstream",
    "with_earn_max_projection",
    "PolicyCommitment::Confirmed",
    "EARN_MAX_DELEGATE",
  ]) {
    requireText(laserstream, required, "earn_max_projection_not_owned_by_existing_laserstream", LASERSTREAM_MONITOR);
  }
  requireText(
    reconciliation,
    "process_policy_instructions",
    "laserstream_policy_reconciliation_bridge_missing",
    LASERSTREAM_RECONCILIATION,
  );
  for (const required of [
    "project_earn_max_policy_set",
    "current_policy_matches",
    "get_multiple_accounts_with_commitment",
  ]) {
    requireText(monitor, required, "policy_projection_contract_missing", POLICY_MONITOR);
  }
  return {
    policyMonitorSha256: sha256(monitor),
    laserstreamOwnerSha256: sha256(laserstream),
    reconciliationSha256: sha256(reconciliation),
  };
}

function checkAppSource(): Json {
  if (!existsSync(APPS_ROOT)) fail("loyal_apps_checkout_missing", { path: APPS_ROOT });
  const apiRoot = resolve(APPS_ROOT, APP_API_ROOT);
  const files = relativeFiles(apiRoot).filter((path) => path.endsWith("route.ts"));
  const expected = [
    "history/route.ts",
    "state/route.ts",
    "transactions/prepare/route.ts",
    "withdrawals/route.ts",
  ];
  if (JSON.stringify(files) !== JSON.stringify(expected)) {
    fail("earn_max_endpoint_inventory_drift", { expected, actual: files });
  }
  const featureRoot = resolve(APPS_ROOT, APP_FEATURE_ROOT);
  const featureFiles = relativeFiles(featureRoot).filter((path) => path.endsWith(".ts"));
  const source = [
    ...files.map((path) => file(apiRoot, path)),
    ...featureFiles.map((path) => file(featureRoot, path)),
    file(APPS_ROOT, APP_ACTIONS),
  ].join("\n");
  for (const action of ["install_policies", "deposit", "claim", "close_policies"] as const) {
    requireText(source, action, "earn_max_prepare_action_missing", APP_API_ROOT);
  }
  for (const forbidden of ["programId", "policySeed", "claimDestination", "/confirm"] as const) {
    rejectText(files.map((path) => file(apiRoot, path)).join("\n"), forbidden, "earn_max_arbitrary_or_confirmation_surface", APP_API_ROOT);
  }
  for (const required of [
    "prepareEarnMaxInstall",
    "prepareEarnMaxDeposit",
    "prepareEarnMaxClaim",
    "prepareEarnMaxClose",
    "createEarnMaxPolicyManifest",
    "serializePreparedOperation",
    "history_incomplete",
    "realized_apy_bps",
    "forecast_apy_bps",
    "x-loyal-deployment-revision",
  ]) {
    requireText(source, required, "earn_max_application_contract_missing", APP_FEATURE_ROOT);
  }
  for (const forbidden of ["preparation_pending", "SOLANA_TESTING_PK", "flashBorrow", "flash_borrow", "guard", "hook"]) {
    rejectText(source, forbidden, "earn_max_application_placeholder_or_forbidden_graph", APP_FEATURE_ROOT);
  }
  requireText(
    file(APPS_ROOT, "apps/web/tsconfig.earn-max.json"),
    "src/features/earn-max/**/*.ts",
    "earn_max_scoped_typecheck_missing",
    "apps/web/tsconfig.earn-max.json",
  );
  return { root: APP_API_ROOT, files, featureFiles, sha256: sha256(source) };
}

function checkWorkerAndStoreSource(): Json {
  const worker = file(ROOT, "crates/loyal-fleet-worker/src/multiply/mod.rs");
  const store = file(ROOT, MULTIPLY_STORE);
  for (const required of [
    "bootstrap_ready_route",
    "admit_next_confirmed_deposit",
    "admit_next_confirmed_claim",
    "record_multiply_position_snapshot",
    "confirmed_kamino_reserve_curve_500ms",
    "forecast_apy_bps",
  ]) {
    requireText(worker, required, "earn_max_worker_bridge_missing", "crates/loyal-fleet-worker/src/multiply/mod.rs");
  }
  for (const required of [
    "load_unbootstrapped_earn_max_policy_set",
    "load_unadmitted_multiply_route_state",
    "load_claimable_multiply_route_state",
    "admit_external_multiply_operation",
  ]) {
    requireText(store, required, "earn_max_store_contract_missing", MULTIPLY_STORE);
  }
  rejectText(worker, "build_operation(.*MultiplyAction::Claim", "delegate_claim_execution_survived", "crates/loyal-fleet-worker/src/multiply/mod.rs");
  return { workerSha256: sha256(worker), storeSha256: sha256(store) };
}

async function targetedChecks(): Promise<Json> {
  const commands: Array<{ command: string[]; cwd: string }> = [
    { command: ["cargo", "check", "-q", "-p", "loyal-squads-policy-monitor"], cwd: ROOT },
    { command: ["cargo", "check", "-q", "-p", "balance-sweep-ata-monitor", "--bin", "balance-sweep-ata-monitor"], cwd: ROOT },
    { command: ["cargo", "check", "-q", "-p", "loyal-fleet-worker", "--bin", "multiply-route-worker"], cwd: ROOT },
    { command: ["bun", "run", "typecheck"], cwd: resolve(APPS_ROOT, "packages/loyal-actions") },
    { command: ["bunx", "tsc", "-p", "apps/web/tsconfig.earn-max.json", "--pretty", "false"], cwd: APPS_ROOT },
  ];
  const results: Json[] = [];
  for (const check of commands) {
    const child = Bun.spawn(check.command, { cwd: check.cwd, stdout: "pipe", stderr: "pipe" });
    const [exitCode, stdout, stderr] = await Promise.all([
      child.exited,
      new Response(child.stdout).text(),
      new Response(child.stderr).text(),
    ]);
    if (exitCode !== 0) {
      fail("targeted_compile_failed", {
        command: check.command.join(" "),
        cwd: check.cwd,
        exitCode,
        stdoutSha256: sha256(stdout),
        stderrSha256: sha256(stderr),
        stderrTail: stderr.split(/\r?\n/).slice(-20).join("\n"),
      });
    }
    results.push({ command: check.command.join(" "), cwd: check.cwd, exitCode });
  }
  return { results };
}

function requiredEnv(name: string): string {
  const value = process.env[name]?.trim();
  if (!value) {
    blocked("terminal_environment_missing", {
      variable: name,
      resume: "run the sole verifier through op run --env-file=.env.1password",
    });
  }
  return value;
}

async function checkLivePrerequisites(): Promise<Json> {
  const rpcUrl = requiredEnv("SOLANA_RPC_URL");
  const databaseUrl = requiredEnv("NEON_DATABASE_URL");
  requiredEnv("EARN_MAX_VERIFY_SETTINGS");
  requiredEnv("EARN_MAX_APP_URL");

  const connection = new Connection(rpcUrl, { commitment: "confirmed", httpAgent: false });
  const genesisHash = await connection.getGenesisHash();
  if (genesisHash !== MAINNET_GENESIS) fail("rpc_not_mainnet_beta", { genesisHash });

  const sql = neon(databaseUrl);
  const rows = await sql`
    SELECT table_name
    FROM information_schema.tables
    WHERE table_schema = 'loyal_yield'
      AND table_name IN (
        'earn_max_policy_sets',
        'multiply_route_states',
        'multiply_operations',
        'multiply_position_snapshots',
        'projection_offsets'
      )
    ORDER BY table_name
  `;
  const tables = rows.map((row) => String(row.table_name));
  const expected = [
    "earn_max_policy_sets",
    "multiply_operations",
    "multiply_position_snapshots",
    "multiply_route_states",
    "projection_offsets",
  ];
  if (JSON.stringify(tables) !== JSON.stringify(expected)) {
    fail("deployed_earn_max_schema_incomplete", { expected, actual: tables });
  }
  const migrations = await sql`
    SELECT version, name, checksum
    FROM loyal_yield.schema_migrations
    WHERE version IN (54, 55)
    ORDER BY version
  `;
  if (
    migrations.length !== 2 ||
    String(migrations[0]?.name) !== "earn_max_per_user" ||
    String(migrations[1]?.name) !== "earn_max_repeated_lifecycle"
  ) {
    fail("deployed_earn_max_migration_missing", { migrations });
  }
  const appUrl = requiredEnv("EARN_MAX_APP_URL").replace(/\/$/, "");
  const response = await fetch(`${appUrl}/api/smart-accounts/earn-max/state`, {
    redirect: "manual",
  });
  const contractHeader = response.headers.get("x-loyal-earn-max-contract");
  const deployedRevision = response.headers.get("x-loyal-deployment-revision");
  if (contractHeader !== CONTRACT_VERSION || !deployedRevision?.match(/^[0-9a-f]{40}$/)) {
    fail("deployed_earn_max_application_identity_missing", {
      status: response.status,
      contractHeader,
      deployedRevision,
    });
  }
  return { genesisHash, tables, migration: migrations[0], deployedRevision };
}

function checkReleaseSource(): Json {
  const worker = file(ROOT, WORKER);
  const render = file(ROOT, RENDER_BLUEPRINT);
  for (const forbidden of ["SOLANA_TESTING_PK", "guard", "flashBorrow", "flash_borrow"] as const) {
    rejectText(worker, forbidden, "multiply_runtime_authority_or_graph_drift", WORKER);
  }
  requireText(worker, "CommitmentConfig::confirmed()", "multiply_runtime_not_confirmed", WORKER);
  for (const required of ["loyal-multiply-route-worker", "multiply-route-worker run", "POLICY_KEYPAIR"] as const) {
    requireText(render, required, "earn_max_release_topology_missing", RENDER_BLUEPRINT);
  }
  const monitorImage = render.match(/name: loyal-balance-sweep-ata-monitor[\s\S]*?laserstream-workers:sha-([0-9a-f]{40})/)?.[1];
  const workerImage = render.match(/name: loyal-multiply-route-worker[\s\S]*?light-workers:sha-([0-9a-f]{40})/)?.[1];
  if (!monitorImage || !workerImage || monitorImage !== workerImage) {
    fail("earn_max_worker_image_pin_drift", { monitorImage, workerImage });
  }
  const imageBuild = file(ROOT, "scripts/build-rust-image-binaries.sh");
  requireText(imageBuild, "laserstream-workers)\n    packages=(balance-sweep-ata-monitor kamino-reserve-monitor", "policy_projection_wrong_image_family", "scripts/build-rust-image-binaries.sh");
  requireText(imageBuild, "multiply-route-worker", "multiply_worker_missing_from_image", "scripts/build-rust-image-binaries.sh");
  return { workerSha256: sha256(worker), renderSha256: sha256(render), imageRevision: monitorImage };
}

const contract = checkContractIdentity();
const topology = checkPerUserTopology();
const schema = checkMinimalSchemaSource();
const laserStream = checkLaserStreamSource();
const app = checkAppSource();
const engine = checkWorkerAndStoreSource();
const release = checkReleaseSource();
const targeted = await targetedChecks();
const live = await checkLivePrerequisites();

blocked("fresh_deployed_earn_max_lifecycle_missing", {
  evidence: { contract, topology, schema, laserStream, app, engine, release, targeted, live },
  resume: "deploy the exact inspected revisions, execute one funded authenticated confirmed-mainnet lifecycle through the product, then rerun this verifier",
});
