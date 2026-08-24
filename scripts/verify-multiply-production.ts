import { createHash } from "node:crypto";
import { existsSync, readFileSync, readdirSync, statSync } from "node:fs";
import { resolve } from "node:path";

import { neon } from "@neondatabase/serverless";
import { Connection, PublicKey } from "@solana/web3.js";

const CONTRACT_VERSION = "earn-max-v2";
const POLICY_MANIFEST_VERSION = "earn-max-v1";
const CONTRACT_SHA256 = "1ecb25c88473a316532012c7ee5cff727b55860cd6cd3106c281ebdfa4ba80fa";
const PASS = "PASS_EARN_MAX_PRODUCTION_READY";
const FAIL = "FAIL_EARN_MAX_PRODUCTION_READY";
const BLOCKED = "BLOCKED_EARN_MAX_PRODUCTION_READY";
const MAINNET_GENESIS = "5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d";
const VERIFY_SETTINGS = "6jgkucnbz1RuHq6NULqACQY3r2XegHaWhgPpaCEGPCA3";
const APP_URL = "https://askloyal.com";
const POLICY_MONITOR_SERVICE = "srv-d8j87m6q1p3s73ff8n8g";
const MULTIPLY_WORKER_SERVICE = "srv-da56asrncjis73fu9psg";
const ROOT = resolve(import.meta.dir, "..");
const APPS_ROOT = resolve(ROOT, "../loyal-apps");
const CONTRACT = "docs/plans/multiply-rwa-looping-policy-architecture.md";
const CONFIG = "crates/loyal-fleet-worker/src/multiply/config.rs";
const POLICY_MONITOR = "crates/loyal-squads-policy-monitor/src/lib.rs";
const POLICY_MONITOR_MANIFEST = "crates/loyal-squads-policy-monitor/Cargo.toml";
const LASERSTREAM_MONITOR = "crates/balance-sweep-ata-monitor/src/main.rs";
const LASERSTREAM_SOURCE = "crates/balance-sweep-ata-monitor/src/lib.rs";
const LASERSTREAM_RECONCILIATION = "crates/balance-sweep-ata-monitor/src/earn_reconciliation.rs";
const WORKER = "crates/loyal-fleet-worker/src/bin/multiply-route-worker.rs";
const MIGRATIONS = "crates/loyal-yield-store/migrations";
const RENDER_BLUEPRINT = "render.yaml";
const APP_API_ROOT = "apps/web/src/app/api/smart-accounts/earn-max";
const APP_FEATURE_ROOT = "apps/web/src/features/earn-max";
const APP_ACTIONS = "packages/loyal-actions/src/earn-max.ts";
const APP_UI = "apps/web/src/components/wallet-workspace/facelift/earn-max-pane.tsx";
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

function record(value: unknown): Record<string, unknown> | null {
  return typeof value === "object" && value !== null && !Array.isArray(value)
    ? value as Record<string, unknown>
    : null;
}

function array(value: unknown): unknown[] {
  return Array.isArray(value) ? value : [];
}

function integer(value: unknown): bigint | null {
  if (typeof value === "bigint") return value;
  if (typeof value === "number" && Number.isSafeInteger(value)) return BigInt(value);
  if (typeof value === "string" && /^-?\d+$/.test(value)) return BigInt(value);
  return null;
}

function timestamp(value: unknown): number | null {
  if (typeof value !== "string" && !(value instanceof Date)) return null;
  const parsed = new Date(value).getTime();
  return Number.isFinite(parsed) ? parsed : null;
}

async function commandJson(command: string[], cwd = ROOT): Promise<unknown> {
  const child = Bun.spawn(command, { cwd, stdout: "pipe", stderr: "pipe" });
  const [exitCode, stdout, stderr] = await Promise.all([
    child.exited,
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
  ]);
  if (exitCode !== 0) {
    fail("read_only_command_failed", {
      command: command.join(" "),
      exitCode,
      stderrTail: stderr.split(/\r?\n/).slice(-12).join("\n"),
    });
  }
  try {
    return JSON.parse(stdout);
  } catch {
    fail("read_only_command_returned_invalid_json", {
      command: command.join(" "),
      stdoutSha256: sha256(stdout),
    });
  }
}

async function commandText(command: string[], cwd = ROOT): Promise<string> {
  const child = Bun.spawn(command, { cwd, stdout: "pipe", stderr: "pipe" });
  const [exitCode, stdout, stderr] = await Promise.all([
    child.exited,
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
  ]);
  if (exitCode !== 0) {
    fail("read_only_command_failed", {
      command: command.join(" "),
      exitCode,
      stderrTail: stderr.split(/\r?\n/).slice(-12).join("\n"),
    });
  }
  return stdout.trim();
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
  for (const required of [
    "0064_earn_max_partial_lifecycle.sql",
    "source_instruction_index",
    "request_withdrawal",
    "cancel_withdrawal",
  ]) {
    requireText(
      `${migrations.join("\n")}\n${source}`,
      required,
      "earn_max_partial_lifecycle_schema_missing",
      MIGRATIONS,
    );
  }
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
  const laserstreamSource = file(ROOT, LASERSTREAM_SOURCE);
  const reconciliation = file(ROOT, LASERSTREAM_RECONCILIATION);
  requireText(manifest, "loyal-fleet-worker", "policy_projection_manifest_contract_missing", POLICY_MONITOR_MANIFEST);
  for (const required of [
    "UpdateSourceKind::Laserstream",
    "with_earn_max_projection",
    "PolicyCommitment::Confirmed",
    "EARN_MAX_DELEGATE",
    "LaserstreamPolicyUpdateSource",
    "earn_max_policy_replay_start_slot",
  ]) {
    requireText(laserstream, required, "earn_max_projection_not_owned_by_existing_laserstream", LASERSTREAM_MONITOR);
  }
  for (const required of [
    "SubscribeRequestFilterTransactions",
    "CommitmentLevel::Confirmed",
    "SQUADS_SMART_ACCOUNT_PROGRAM_ID.to_string()",
    "process_earn_max_policy_update",
  ]) {
    requireText(laserstreamSource, required, "earn_max_confirmed_transaction_stream_missing", LASERSTREAM_SOURCE);
  }
  requireText(
    reconciliation,
    "read_confirmed_squads_policy_transaction",
    "laserstream_policy_reconciliation_bridge_missing",
    LASERSTREAM_RECONCILIATION,
  );
  for (const required of [
    "parse_earn_max_intent",
    "project_earn_max_intent",
    "source_instruction_index",
    "CommitmentConfig::confirmed()",
    "memo.accounts.contains(&vault_pubkey)",
    "has_squads_execution",
    '["loyal", "earn-max", "v1"]',
  ]) {
    requireText(
      reconciliation,
      required,
      "earn_max_intent_projection_missing",
      LASERSTREAM_RECONCILIATION,
    );
  }
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
    laserstreamSourceSha256: sha256(laserstreamSource),
    reconciliationSha256: sha256(reconciliation),
  };
}

function checkAppSource(): Json {
  if (!existsSync(APPS_ROOT)) fail("loyal_apps_checkout_missing", { path: APPS_ROOT });
  const apiRoot = resolve(APPS_ROOT, APP_API_ROOT);
  const files = relativeFiles(apiRoot).filter((path) => path.endsWith("route.ts"));
  const expected = [
    "activity/route.ts",
    "performance/route.ts",
    "state/route.ts",
  ];
  if (JSON.stringify(files) !== JSON.stringify(expected)) {
    fail("earn_max_endpoint_inventory_drift", { expected, actual: files });
  }
  const featureRoot = resolve(APPS_ROOT, APP_FEATURE_ROOT);
  const featureFiles = relativeFiles(featureRoot).filter((path) => /\.tsx?$/.test(path));
  const source = [
    ...files.map((path) => file(apiRoot, path)),
    ...featureFiles.map((path) => file(featureRoot, path)),
    file(APPS_ROOT, APP_ACTIONS),
    file(APPS_ROOT, APP_UI),
  ].join("\n");
  for (const forbidden of [
    "programId", "policySeed", "claimDestination", "/confirm",
    "prepareTransaction", "requestWithdrawal", "requestEarnMaxWithdrawal",
  ] as const) {
    rejectText(files.map((path) => file(apiRoot, path)).join("\n"), forbidden, "earn_max_arbitrary_or_confirmation_surface", APP_API_ROOT);
  }
  for (const required of [
    "buildEarnMaxInstallInstructions",
    "buildEarnMaxDepositInstructions",
    "buildEarnMaxWithdrawalRequestInstructions",
    "buildEarnMaxWithdrawalCancelInstructions",
    "buildEarnMaxClaimInstructions",
    "buildEarnMaxCloseInstructions",
    "createEarnMaxPolicyManifest",
    "resolveEarnMaxInstallSeedBase",
    "EarnMaxViewModel",
    "history_incomplete",
    "realized_apy_bps",
    "forecast_apy_bps",
    "x-loyal-deployment-revision",
    "loyal:earn-max:v1:withdraw:",
    "loyal:earn-max:v1:cancel:",
    "confirm: true",
  ]) {
    requireText(source, required, "earn_max_application_contract_missing", APP_FEATURE_ROOT);
  }
  for (const forbidden of [
    "preparation_pending", "SOLANA_TESTING_PK", "flashBorrow", "flash_borrow",
    "guard", "hook", "EARN_MAX_BALANCE_USD", "EARN_MAX_APY_LABEL",
    "NOOP_EXECUTE_NOW", "mock no-op", "Mocked Earn MAX",
  ]) {
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
  const state = file(ROOT, "crates/loyal-yield-store/src/fleet_orchestration/multiply.rs");
  const planner = file(ROOT, "crates/loyal-fleet-worker/src/multiply/planner.rs");
  const store = file(ROOT, MULTIPLY_STORE);
  for (const required of [
    "bootstrap_ready_route",
    "admit_next_confirmed_deposit",
    "admit_next_confirmed_claim",
    "record_multiply_position_snapshot",
    "confirmed_kamino_reserve_curve_500ms",
    "forecast_apy_bps",
    "redeploy_after_partial_claim",
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
  requireText(planner, "if observed.claim.amount_raw > 0", "active_position_top_up_not_deployed", "crates/loyal-fleet-worker/src/multiply/planner.rs");
  requireText(state, "ready_by", "withdrawal_sla_not_explicit", "crates/loyal-yield-store/src/fleet_orchestration/multiply.rs");
  requireText(state, "cancel_withdrawal", "withdrawal_cancellation_state_missing", "crates/loyal-yield-store/src/fleet_orchestration/multiply.rs");
  requireText(state, "roll_terminal_policy_seed_base", "repeated_policy_install_state_transition_missing", "crates/loyal-yield-store/src/fleet_orchestration/multiply.rs");
  requireText(store, "interval '30 seconds'", "withdrawal_cancel_grace_missing", MULTIPLY_STORE);
  return {
    workerSha256: sha256(worker),
    stateSha256: sha256(state),
    plannerSha256: sha256(planner),
    storeSha256: sha256(store),
  };
}

async function targetedChecks(): Promise<Json> {
  const commands: Array<{ command: string[]; cwd: string }> = [
    { command: ["cargo", "check", "-q", "-p", "loyal-squads-policy-monitor"], cwd: ROOT },
    { command: ["cargo", "check", "-q", "-p", "balance-sweep-ata-monitor", "--bin", "balance-sweep-ata-monitor"], cwd: ROOT },
    { command: ["cargo", "check", "-q", "-p", "loyal-fleet-worker", "--bin", "multiply-route-worker"], cwd: ROOT },
    {
      command: [
        "bunx", "turbo", "run", "build",
        "--filter=@loyal-labs/smart-account-vaults...",
        "--filter=@loyal-labs/actions...",
        "--filter=@loyal-labs/auth-core...",
        "--filter=@loyal-labs/db-adapter-neon...",
      ],
      cwd: APPS_ROOT,
    },
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
        stdoutTail: stdout.split(/\r?\n/).slice(-20).join("\n"),
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
    WHERE version IN (54, 55, 56, 64)
    ORDER BY version
  `;
  if (
    migrations.length !== 4 ||
    String(migrations[0]?.name) !== "earn_max_per_user" ||
    String(migrations[1]?.name) !== "earn_max_repeated_lifecycle" ||
    String(migrations[2]?.name) !== "earn_max_dynamic_policy_seeds" ||
    String(migrations[3]?.name) !== "earn_max_partial_lifecycle"
  ) {
    fail("deployed_earn_max_migration_missing", { migrations });
  }
  const liveRoutes = ["state", "performance", "activity"];
  const routeEvidence: Json[] = [];
  let deployedRevision: string | null = null;
  for (const route of liveRoutes) {
    const response = await fetch(`${APP_URL}/api/smart-accounts/earn-max/${route}`, {
      redirect: "manual",
    });
    const contractHeader = response.headers.get("x-loyal-earn-max-contract");
    const revision = response.headers.get("x-loyal-deployment-revision");
    if (
      response.status !== 401 ||
      contractHeader !== CONTRACT_VERSION ||
      !revision?.match(/^[0-9a-f]{40}$/) ||
      (deployedRevision !== null && revision !== deployedRevision)
    ) {
      fail("deployed_earn_max_application_identity_missing", {
        route,
        status: response.status,
        contractHeader,
        revision,
        deployedRevision,
      });
    }
    deployedRevision = revision;
    routeEvidence.push({ route, status: response.status, contractHeader, revision });
  }
  for (const removed of ["history", "withdrawals", "transactions/prepare"]) {
    const response = await fetch(`${APP_URL}/api/smart-accounts/earn-max/${removed}`, {
      redirect: "manual",
    });
    if (response.status !== 404) {
      fail("deployed_earn_max_mutation_or_obsolete_endpoint_survived", {
        route: removed,
        status: response.status,
      });
    }
  }
  const appRevision = await commandText(["git", "rev-parse", "HEAD"], APPS_ROOT);
  if (deployedRevision !== appRevision) {
    fail("deployed_earn_max_application_revision_drift", { deployedRevision, appRevision });
  }
  return {
    genesisHash,
    tables,
    migrations: migrations.map((migration) => ({
      version: migration.version,
      name: migration.name,
      checksum: migration.checksum,
    })),
    deployedRevision,
    routes: routeEvidence,
  };
}

async function checkDeployedWorkers(
  monitorImageRevision: string,
  workerImageRevision: string,
): Promise<Json> {
  const expected = {
    [POLICY_MONITOR_SERVICE]: `ghcr.io/loyal-labs/loyal-yield-routing/laserstream-workers:sha-${monitorImageRevision}`,
    [MULTIPLY_WORKER_SERVICE]: `ghcr.io/loyal-labs/loyal-yield-routing/light-workers:sha-${workerImageRevision}`,
  };
  const evidence: Json[] = [];
  for (const [service, image] of Object.entries(expected)) {
    const deploys = array(await commandJson(["render", "deploys", "list", service, "-o", "json"]));
    const latest = record(deploys[0]);
    const deployedImage = record(latest?.image)?.ref;
    if (latest?.status !== "live" || deployedImage !== image) {
      fail("earn_max_worker_deployment_drift", {
        service,
        expectedImage: image,
        actualStatus: latest?.status,
        actualImage: deployedImage,
      });
    }
    evidence.push({
      service,
      deployId: latest.id,
      image: deployedImage,
      status: latest.status,
      createdAt: latest.createdAt,
      finishedAt: latest.finishedAt,
    });
  }
  return { services: evidence };
}

async function confirmedSignatures(
  connection: Connection,
  signatures: string[],
): Promise<Json> {
  const invalidFormat = signatures.filter((value) => !/^[1-9A-HJ-NP-Za-km-z]{80,90}$/.test(value));
  const unique = [...new Set(signatures.filter((value) => /^[1-9A-HJ-NP-Za-km-z]{80,90}$/.test(value)))];
  if (invalidFormat.length > 0 || unique.length === 0) {
    fail("earn_max_lifecycle_signature_inventory_invalid", {
      supplied: signatures.length,
      unique: unique.length,
      invalidFormatCount: invalidFormat.length,
    });
  }
  const statuses = await connection.getSignatureStatuses(unique, { searchTransactionHistory: true });
  const invalid = unique.flatMap((signature, index) => {
    const status = statuses.value[index];
    return status && status.err === null && ["confirmed", "finalized"].includes(status.confirmationStatus ?? "")
      ? []
      : [{ signature, status }];
  });
  if (invalid.length > 0) fail("earn_max_lifecycle_signature_not_confirmed", { invalid });
  return { count: unique.length, slots: statuses.value.map((status) => status?.slot ?? null) };
}

async function checkFreshLifecycle(): Promise<Json> {
  const rpcUrl = requiredEnv("SOLANA_RPC_URL");
  const databaseUrl = requiredEnv("NEON_DATABASE_URL");
  const sql = neon(databaseUrl);
  const connection = new Connection(rpcUrl, { commitment: "confirmed", httpAgent: false });
  const policies = await sql`
    SELECT * FROM loyal_yield.earn_max_policy_sets
    WHERE settings = ${VERIFY_SETTINGS} AND vault_index = 0
    LIMIT 1
  `;
  const policy = record(policies[0]);
  const bindings = array(policy?.policy_accounts).map(record).filter((value): value is Record<string, unknown> => value !== null);
  const seeds = bindings.map((binding) => integer(binding.seed));
  const accounts = bindings.map((binding) => String(binding.account ?? ""));
  const base = integer(policy?.policy_seed_base);
  const actualSeeds = seeds.filter((seed): seed is bigint => seed !== null);
  const expectedSeeds = base === null
    ? []
    : Array.from({ length: 6 }, (_, index) => base + BigInt(index));
  if (
    policy?.manifest_version !== POLICY_MANIFEST_VERSION ||
    policy?.status !== "removed" ||
    base === null || base <= 0n ||
    bindings.length !== 6 ||
    actualSeeds.length !== 6 ||
    new Set(actualSeeds).size !== 6 ||
    expectedSeeds.some((seed) => !actualSeeds.includes(seed)) ||
    new Set(accounts).size !== 6 ||
    bindings.some((binding) => binding.matches !== false)
  ) {
    blocked("fresh_laserstream_policy_removal_missing", {
      settings: VERIFY_SETTINGS,
      policyStatus: policy?.status,
      policySeedBase: policy?.policy_seed_base,
      policySeeds: actualSeeds.map(String),
      policyCount: bindings.length,
      resume: "complete the fresh confirmed install-to-removal product lifecycle",
    });
  }

  const routes = await sql`
    SELECT route_key, settings, vault_index, vault, state, state_version, updated_at
    FROM loyal_yield.multiply_route_states
    WHERE settings = ${VERIFY_SETTINGS} AND vault_index = 0
    LIMIT 1
  `;
  const route = record(routes[0]);
  const state = record(route?.state);
  const deposit = record(state?.deposit);
  const withdrawal = record(state?.withdrawal);
  const frontend = record(state?.frontend);
  const depositAmount = integer(deposit?.amountRaw);
  const walletPre = integer(deposit?.walletPreAmountRaw);
  const walletPost = integer(deposit?.walletPostAmountRaw);
  const vaultPre = integer(deposit?.vaultPreAmountRaw);
  const vaultPost = integer(deposit?.vaultPostAmountRaw);
  const requestedAt = timestamp(withdrawal?.requestedAt);
  const readyBy = timestamp(withdrawal?.readyBy);
  const unwindAt = timestamp(withdrawal?.unwindCompletedAt);
  if (
    state?.schemaVersion !== 7 ||
    state?.engineVersion !== "earn_max_v1" ||
    integer(state?.policySeedBase) !== base ||
    state?.goal !== "claimed" ||
    state?.currentOperationId !== null ||
    state?.manualRecoveryReason !== null ||
    withdrawal?.status !== "claimed" ||
    frontend?.status !== "claimed" ||
    depositAmount === null || depositAmount <= 0n ||
    walletPre === null || walletPost === null || walletPre - walletPost !== depositAmount ||
    vaultPre === null || vaultPost === null || vaultPost - vaultPre !== depositAmount ||
    requestedAt === null || readyBy === null || unwindAt === null ||
    readyBy - requestedAt !== 600_000 || unwindAt < requestedAt || unwindAt > readyBy
  ) {
    blocked("fresh_claimed_route_reconciliation_missing", {
      settings: VERIFY_SETTINGS,
      goal: state?.goal,
      withdrawalStatus: withdrawal?.status,
      readyBy,
      unwindMilliseconds: requestedAt !== null && unwindAt !== null ? unwindAt - requestedAt : null,
      resume: "complete and reconcile the fresh deposit, unwind, and claim lifecycle within 600 seconds",
    });
  }

  const operations = await sql`
    SELECT operation_id, action, status, transaction_signature,
           source_instruction_index, confirmed_slot, expected_effects,
           created_at, updated_at
    FROM loyal_yield.multiply_operations
    WHERE route_key = ${String(route?.route_key ?? "")}
    ORDER BY created_at, operation_id
  `;
  const requiredActions = [
    "request_withdrawal",
    "cancel_withdrawal",
    "deposit_claim_asset",
    "swap_claim_to_collateral",
    "deposit_collateral",
    "borrow_debt",
    "withdraw_collateral",
    "repay_debt",
    "withdraw_remaining_collateral",
    "swap_collateral_to_claim",
    "claim",
  ];
  const actionSet = new Set(operations.map((operation) => String(operation.action)));
  const badOperations = operations.filter((operation) => operation.status !== "reconciled");
  const actionCount = (action: string) => operations.filter((operation) => operation.action === action).length;
  const chainLocations = operations
    .filter((operation) => operation.source_instruction_index !== null)
    .map((operation) => `${operation.transaction_signature}:${operation.source_instruction_index}`);
  const intentLocationsUnique = new Set(chainLocations).size === chainLocations.length;
  if (
    operations.length === 0 ||
    requiredActions.some((action) => !actionSet.has(action)) ||
    badOperations.length > 0 ||
    actionCount("deposit_claim_asset") < 2 ||
    actionCount("request_withdrawal") < 3 ||
    actionCount("cancel_withdrawal") < 1 ||
    actionCount("claim") < 2 ||
    !intentLocationsUnique
  ) {
    blocked("fresh_hookless_operation_graph_incomplete", {
      actions: [...actionSet].sort(),
      counts: Object.fromEntries(requiredActions.map((action) => [action, actionCount(action)])),
      intentLocationsUnique,
      nonReconciled: badOperations.map((operation) => ({ id: operation.operation_id, status: operation.status })),
      resume: "complete the confirmed deposit, top-up, cancel, partial/full claim, and hookless open/unwind graph",
    });
  }

  const operationSlot = (operation: Record<string, unknown>) => integer(operation.confirmed_slot);
  const intent = (operation: Record<string, unknown>) =>
    record(record(operation.expected_effects)?.intent);
  const claimSourcePost = (operation: Record<string, unknown>): bigint | null => {
    const effects = record(operation.expected_effects);
    const before = array(effects?.tokenAmountsBefore)
      .map(record)
      .filter((value): value is Record<string, unknown> => value !== null);
    const deltas = array(effects?.tokenDeltas)
      .map(record)
      .filter((value): value is Record<string, unknown> => value !== null);
    const sourceDelta = deltas.find((delta) => (integer(delta.rawDelta) ?? 0n) < 0n);
    const sourceBefore = before.find((amount) => amount.account === sourceDelta?.account);
    const amount = integer(sourceBefore?.amountRaw);
    const delta = integer(sourceDelta?.rawDelta);
    return amount === null || delta === null ? null : amount + delta;
  };
  const bySlot = (left: Record<string, unknown>, right: Record<string, unknown>) =>
    Number((operationSlot(left) ?? 0n) - (operationSlot(right) ?? 0n));
  const deposits = operations.filter((operation) => operation.action === "deposit_claim_asset").sort(bySlot);
  const borrows = operations.filter((operation) => operation.action === "borrow_debt").sort(bySlot);
  const requests = operations.filter((operation) => operation.action === "request_withdrawal").sort(bySlot);
  const cancels = operations.filter((operation) => operation.action === "cancel_withdrawal").sort(bySlot);
  const claims = operations.filter((operation) => operation.action === "claim").sort(bySlot);
  const partialClaim = claims.find((operation) => (claimSourcePost(operation) ?? 0n) > 0n);
  const fullClaim = [...claims].reverse().find((operation) => claimSourcePost(operation) === 0n);
  const cancel = cancels.find((candidate) => requests.some((request) =>
    intent(request)?.requestId === intent(candidate)?.requestId &&
    (operationSlot(request) ?? 0n) <= (operationSlot(candidate) ?? 0n)
  ));
  const partialClaimSlot = partialClaim ? operationSlot(partialClaim) : null;
  const fullClaimSlot = fullClaim ? operationSlot(fullClaim) : null;
  const cancelSlot = cancel ? operationSlot(cancel) : null;
  const partialRequest = partialClaimSlot === null ? undefined : [...requests].reverse().find((request) =>
    intent(request)?.requestId !== intent(cancel ?? {})?.requestId &&
    (operationSlot(request) ?? 0n) <= partialClaimSlot
  );
  const redeploy = partialClaimSlot === null ? undefined : operations.find((operation) =>
    operation.action === "swap_claim_to_collateral" &&
    (operationSlot(operation) ?? 0n) > partialClaimSlot
  );
  const redeploySlot = redeploy ? operationSlot(redeploy) : null;
  const fullRequest = fullClaimSlot === null || redeploySlot === null ? undefined : requests.find((request) =>
    (operationSlot(request) ?? 0n) > redeploySlot &&
    (operationSlot(request) ?? 0n) <= fullClaimSlot
  );
  const firstDepositSlot = operationSlot(deposits[0] ?? {});
  const topUpSlot = operationSlot(deposits[1] ?? {});
  const initialBorrow = firstDepositSlot === null || topUpSlot === null ? undefined : borrows.find((borrow) =>
    (operationSlot(borrow) ?? 0n) > firstDepositSlot &&
    (operationSlot(borrow) ?? 0n) < topUpSlot
  );
  const lifecycleSlots = [
    firstDepositSlot,
    initialBorrow ? operationSlot(initialBorrow) : null,
    topUpSlot,
    cancelSlot,
    partialRequest ? operationSlot(partialRequest) : null,
    partialClaimSlot,
    redeploySlot,
    fullRequest ? operationSlot(fullRequest) : null,
    fullClaimSlot,
  ];
  const lifecycleOrdered = lifecycleSlots.every((slot) => slot !== null) &&
    lifecycleSlots.every((slot, index) =>
      index === 0 || (slot ?? 0n) >= (lifecycleSlots[index - 1] ?? 0n)
    );
  if (!lifecycleOrdered) {
    blocked("fresh_partial_and_repeated_lifecycle_missing", {
      firstDepositSlot: firstDepositSlot?.toString(),
      initialBorrowSlot: initialBorrow ? operationSlot(initialBorrow)?.toString() : null,
      topUpSlot: topUpSlot?.toString(),
      cancelSlot: cancelSlot?.toString(),
      partialRequestSlot: partialRequest ? operationSlot(partialRequest)?.toString() : null,
      partialClaimSlot: partialClaimSlot?.toString(),
      partialClaimSourcePost: partialClaim ? claimSourcePost(partialClaim)?.toString() : null,
      redeploySlot: redeploySlot?.toString(),
      fullRequestSlot: fullRequest ? operationSlot(fullRequest)?.toString() : null,
      fullClaimSlot: fullClaimSlot?.toString(),
      resume: "complete the ordered initial-open, top-up, cancel, partial-claim/redeploy, and full-claim lifecycle",
    });
  }

  const snapshots = await sql`
    SELECT * FROM loyal_yield.multiply_position_snapshots
    WHERE route_key = ${String(route?.route_key ?? "")}
    ORDER BY observed_slot, id
  `;
  const open = snapshots.find((snapshot) => (integer(snapshot.collateral_raw) ?? 0n) > 0n && (integer(snapshot.debt_raw) ?? 0n) > 0n);
  const finalSnapshot = snapshots.at(-1);
  if (
    !open || !finalSnapshot ||
    integer(finalSnapshot.claim_raw) !== 0n ||
    integer(finalSnapshot.collateral_raw) !== 0n ||
    integer(finalSnapshot.debt_raw) !== 0n ||
    integer(finalSnapshot.equity_usd_micros) !== 0n ||
    !finalSnapshot.valuation_source || !finalSnapshot.valuation_slot
  ) {
    blocked("fresh_position_history_or_final_zero_missing", {
      snapshotCount: snapshots.length,
      hasRealOpen: Boolean(open),
      final: finalSnapshot ? {
        claimRaw: finalSnapshot.claim_raw,
        collateralRaw: finalSnapshot.collateral_raw,
        debtRaw: finalSnapshot.debt_raw,
        equityUsdMicros: finalSnapshot.equity_usd_micros,
      } : null,
      resume: "observe one nonzero real position and a reconciled zero final position",
    });
  }

  const policyAccounts = await connection.getMultipleAccountsInfo(
    accounts.map((account) => new PublicKey(account)),
    { commitment: "confirmed" },
  );
  if (policyAccounts.some(Boolean)) fail("removed_earn_max_policy_still_exists_on_chain", { accounts });
  const signatures = [
    String(policy?.observed_signature ?? ""),
    String(deposit?.transactionSignature ?? ""),
    String(withdrawal?.claimSignature ?? ""),
    ...operations.map((operation) => String(operation.transaction_signature ?? "")),
  ];
  const chain = await confirmedSignatures(connection, signatures);
  const latestOperationUpdatedAt = Math.max(
    ...operations.map((operation) => timestamp(operation.updated_at) ?? 0),
  );
  return {
    settings: VERIFY_SETTINGS,
    routeKey: route?.route_key,
    vault: route?.vault,
    policySeedBase: base.toString(),
    policyAccounts: accounts,
    operationCount: operations.length,
    actionCounts: Object.fromEntries(requiredActions.map((action) => [action, actionCount(action)])),
    intentLocationCount: chainLocations.length,
    partialClaimSourcePost: partialClaim ? claimSourcePost(partialClaim)?.toString() : null,
    latestOperationUpdatedAt: new Date(latestOperationUpdatedAt).toISOString(),
    snapshotCount: snapshots.length,
    openSnapshotSlot: open.observed_slot,
    finalSnapshotSlot: finalSnapshot.observed_slot,
    unwindMilliseconds: unwindAt - requestedAt,
    chain,
  };
}

function checkReplayEvidence(deployedWorkers: Json, lifecycle: Json): Json {
  const projector = array(deployedWorkers.services)
    .map(record)
    .find((service) => service?.service === POLICY_MONITOR_SERVICE);
  const replayStartedAt = timestamp(projector?.createdAt);
  const latestOperationAt = timestamp(lifecycle.latestOperationUpdatedAt);
  if (
    replayStartedAt === null ||
    latestOperationAt === null ||
    replayStartedAt <= latestOperationAt ||
    projector?.status !== "live"
  ) {
    blocked("post_lifecycle_projector_replay_missing", {
      service: POLICY_MONITOR_SERVICE,
      replayStartedAt: projector?.createdAt,
      latestOperationUpdatedAt: lifecycle.latestOperationUpdatedAt,
      status: projector?.status,
      resume: "restart the exact pinned LaserStream worker after the lifecycle and rerun the read-only verifier",
    });
  }
  return {
    service: POLICY_MONITOR_SERVICE,
    deployId: projector?.deployId,
    replayStartedAt: projector?.createdAt,
    latestOperationUpdatedAt: lifecycle.latestOperationUpdatedAt,
    operationCountAfterReplay: lifecycle.operationCount,
    intentLocationCountAfterReplay: lifecycle.intentLocationCount,
  };
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
  if (!monitorImage || !workerImage) {
    fail("earn_max_worker_image_pin_missing", { monitorImage, workerImage });
  }
  const imageBuild = file(ROOT, "scripts/build-rust-image-binaries.sh");
  requireText(imageBuild, "laserstream-workers)\n    packages=(balance-sweep-ata-monitor kamino-reserve-monitor", "policy_projection_wrong_image_family", "scripts/build-rust-image-binaries.sh");
  requireText(imageBuild, "multiply-route-worker", "multiply_worker_missing_from_image", "scripts/build-rust-image-binaries.sh");
  return {
    workerSha256: sha256(worker),
    renderSha256: sha256(render),
    monitorImageRevision: monitorImage,
    workerImageRevision: workerImage,
  };
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
const deployedWorkers = await checkDeployedWorkers(
  String(release.monitorImageRevision),
  String(release.workerImageRevision),
);
const lifecycle = await checkFreshLifecycle();
const replay = checkReplayEvidence(deployedWorkers, lifecycle);

emit(PASS, "earn_max_production_ready", {
  contract,
  topology,
  schema,
  laserStream,
  app,
  engine,
  release,
  targeted,
  live,
  deployedWorkers,
  lifecycle,
  replay,
}, 0);
