import { createHash } from "node:crypto";
import { existsSync, readFileSync, readdirSync } from "node:fs";
import { resolve } from "node:path";

import { neon } from "@neondatabase/serverless";
import { Connection } from "@solana/web3.js";

const PASS = "PASS_RWA_MULTIPLY_RELEASE_CANDIDATE";
const FAIL = "FAIL_RWA_MULTIPLY_RELEASE_CANDIDATE";
const BLOCKED = "BLOCKED_RWA_MULTIPLY_RELEASE_CANDIDATE";
const MAINNET_GENESIS = "5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d";
const ROOT = resolve(import.meta.dir, "..");
const WORKER = "crates/loyal-fleet-worker/src/bin/multiply-route-worker.rs";
const DOMAIN = "crates/loyal-yield-store/src/fleet_orchestration/multiply.rs";
const ENGINE = "crates/loyal-fleet-worker/src/multiply";
const MIGRATION = "crates/loyal-yield-store/migrations/0053_multiply_production_engine.sql";
const BUILD_INVENTORY = "scripts/build-rust-image-binaries.sh";
const LIGHT_WORKERS_IMAGE = "Dockerfile.light-workers";
const IMAGE_WORKFLOW = ".github/workflows/rust-image-build.yml";
const RENDER_BLUEPRINT = "render.yaml";
const MIGRATION_RUNNER = "crates/loyal-yield-orchestrator/src/bin/yield-migrations.rs";
const EXPECTED_ENGINE_FILES = [
  "mod.rs",
  "builder.rs",
  "config.rs",
  "executor.rs",
  "observe.rs",
  "planner.rs",
  "policy.rs",
  "view.rs",
] as const;

type Json = Record<string, unknown>;

function emit(verdict: string, condition: string, evidence: Json, exitCode: number): never {
  process.stdout.write(`${JSON.stringify({ verdict, condition, evidence }, null, 2)}\n`);
  process.exit(exitCode);
}

function fail(condition: string, evidence: Json = {}): never {
  return emit(FAIL, condition, evidence, 2);
}

function blocked(condition: string, evidence: Json = {}): never {
  return emit(BLOCKED, condition, evidence, 2);
}

function file(relative: string): string {
  const path = resolve(ROOT, relative);
  if (!existsSync(path)) fail("required_source_missing", { path: relative });
  return readFileSync(path, "utf8");
}

function nonblankLines(source: string): number {
  return source.split(/\r?\n/).filter((line) => line.trim().length > 0).length;
}

function sha256(value: string | Uint8Array): string {
  return createHash("sha256").update(value).digest("hex");
}

function occurrences(source: string, pattern: RegExp): number {
  return [...source.matchAll(new RegExp(pattern.source, `${pattern.flags.replace("g", "")}g`))].length;
}

function checkArchitecture(): Json {
  const worker = file(WORKER);
  const workerLines = nonblankLines(worker);
  if (workerLines > 250) {
    fail("production_cli_not_thin", { path: WORKER, nonblankLines: workerLines, maximum: 250 });
  }
  for (const forbidden of ["repair-", "RepairSource", "RepairTarget", "rerun --once", "--once"]) {
    if (worker.includes(forbidden)) {
      fail("manual_operator_state_machine_survived", { path: WORKER, forbidden });
    }
  }

  const domain = file(DOMAIN);
  const domainLines = nonblankLines(domain);
  if (domainLines > 1_000) {
    fail("persisted_domain_exceeds_simplicity_budget", {
      path: DOMAIN,
      nonblankLines: domainLines,
      maximum: 1_000,
    });
  }
  for (const forbidden of [
    "MultiplyQuoteFence",
    "MultiplyCapacityReservation",
    "MultiplyAccountConflictLease",
    "MultiplyExecutionFences",
    "submission_history",
    "optimizer",
  ]) {
    if (domain.includes(forbidden)) {
      fail("dormant_architecture_survived", { path: DOMAIN, forbidden });
    }
  }

  const engineDirectory = resolve(ROOT, ENGINE);
  if (!existsSync(engineDirectory)) fail("production_engine_missing", { path: ENGINE });
  const actualFiles = new Set(readdirSync(engineDirectory).filter((name) => name.endsWith(".rs")));
  for (const expected of EXPECTED_ENGINE_FILES) {
    if (!actualFiles.has(expected)) fail("production_engine_module_missing", { module: expected });
  }
  const engineSources = EXPECTED_ENGINE_FILES.map((name) => ({
    name,
    source: file(`${ENGINE}/${name}`),
  }));
  const moduleLines = engineSources.map(({ name, source }) => ({ name, lines: nonblankLines(source) }));
  const engineLines = moduleLines.reduce((sum, entry) => sum + entry.lines, 0);
  if (engineLines > 3_500 || moduleLines.some((entry) => entry.lines > 900)) {
    fail("production_engine_exceeds_simplicity_budget", {
      totalNonblankLines: engineLines,
      maximumTotal: 3_500,
      maximumModule: 900,
      modules: moduleLines,
    });
  }
  const engine = engineSources.map(({ source }) => source).join("\n");
  for (const forbidden of [
    "SOURCE_WITHDRAW_FOR_REPAY_TRANCHES_RAW",
    "TARGET_WITHDRAW_FOR_REPAY_TRANCHES_RAW",
    "TARGET_INITIAL_DEPOSIT_RAW",
    "TARGET_BORROW_RAW",
    "TARGET_LOOP_DEPOSIT_RAW",
    "SOURCE_SWAP_ALT",
    "TARGET_SWAP_ALTS",
    "wSCbM0HWnI",
    "amount_value_usd_micros",
    "value_usd_micros: source_delta",
    "value_usd_micros: destination_delta",
  ]) {
    if (engine.includes(forbidden)) {
      fail("canary_or_false_value_semantics_survived", { forbidden });
    }
  }
  const nextActionCount = occurrences(engine, /(?:pub\s+)?fn\s+next_action\s*\(/g);
  if (nextActionCount !== 1) fail("planner_not_singular", { nextActionCount });
  if (!engine.includes("pub struct StrategyConfig") || !engine.includes("SYRUP_USDC_USDC") || !engine.includes("SYRUP_USDC_PYUSD")) {
    fail("two_data_only_strategy_configs_missing");
  }
  if (!engine.includes("ensure_exact_policy") || !engine.includes("persist_signed_operation") || !engine.includes("reconcile_operation")) {
    fail("single_operation_pipeline_missing");
  }
  for (const recoveryContract of [
    "max_retries: Some(0)",
    "get_signature_statuses_with_history",
    "get_block_height",
    "expire_multiply_operation",
    "mark_multiply_manual_recovery",
  ]) {
    if (!engine.includes(recoveryContract)) fail("crash_recovery_contract_missing", { recoveryContract });
  }

  const migration = file(MIGRATION);
  for (const required of [
    "CREATE TABLE loyal_yield.multiply_operations",
    "engine_version",
    "signed_wire",
    "broadcast_intent_at",
    "reconciliation_sha256",
    "CREATE UNIQUE INDEX multiply_operations_one_nonterminal_per_route",
    "WHERE status IN",
  ]) {
    if (!migration.includes(required)) fail("operation_migration_contract_missing", { required });
  }
  const verifier = readFileSync(import.meta.path, "utf8");
  for (const forbidden of ["from \"../crates/"]) {
    if (verifier.includes(forbidden)) fail("verifier_not_independent", { forbidden });
  }
  return { workerLines, domainLines, engineLines, modules: moduleLines, migrationSha256: sha256(migration) };
}

function sourceSection(source: string, start: string, end: string): string {
  const startIndex = source.indexOf(start);
  if (startIndex < 0) fail("release_topology_section_missing", { start });
  const endIndex = source.indexOf(end, startIndex + start.length);
  return source.slice(startIndex, endIndex < 0 ? source.length : endIndex);
}

function requireText(source: string, required: string, condition: string, path: string): void {
  if (!source.includes(required)) fail(condition, { path, required });
}

function checkReleaseTopology(): Json {
  const buildInventory = file(BUILD_INVENTORY);
  const lightInventory = sourceSection(buildInventory, "  light-workers)", "  operator-tools)");
  requireText(lightInventory, "multiply-route-worker", "multiply_worker_missing_from_build_inventory", BUILD_INVENTORY);

  const dockerfile = file(LIGHT_WORKERS_IMAGE);
  requireText(
    dockerfile,
    "COPY --chmod=0755 build-artifacts/rust/multiply-route-worker /usr/local/bin/multiply-route-worker",
    "multiply_worker_missing_from_runtime_image",
    LIGHT_WORKERS_IMAGE,
  );

  const workflow = file(IMAGE_WORKFLOW);
  const lightProbeLine = workflow.split(/\r?\n/).find((line) => line.includes("LIGHT_WORKER_PROBE_BINARIES")) ?? "";
  requireText(lightProbeLine, "multiply-route-worker", "multiply_worker_missing_from_image_probe_inventory", IMAGE_WORKFLOW);
  requireText(workflow, "probe_role multiply_route_worker /usr/local/bin/multiply-route-worker --role-probe", "multiply_worker_role_probe_missing", IMAGE_WORKFLOW);

  const worker = file(WORKER);
  if (worker.includes("solana_testing_keypair_from_env")) {
    fail("multiply_runtime_loads_setup_authority", { path: WORKER });
  }
  for (const required of [
    "Command::RoleProbe",
    "fleet_worker_role_probe",
    "networkAccessed",
  ]) {
    requireText(worker, required, "multiply_worker_operational_contract_missing", WORKER);
  }
  const engineLoop = file(`${ENGINE}/mod.rs`);
  for (const required of ["shutdown_signal", "multiply_worker_drained"]) {
    requireText(engineLoop, required, "multiply_worker_operational_contract_missing", `${ENGINE}/mod.rs`);
  }

  const domain = file(DOMAIN);
  for (const required of ["is_same_request", "has_pending_withdrawal"]) {
    requireText(domain, required, "withdrawal_serialization_contract_missing", DOMAIN);
  }
  const executor = file(`${ENGINE}/executor.rs`);
  for (const required of ["after.claim.amount_raw > 0", "RouteGoal::Move"]) {
    requireText(executor, required, "residual_claim_redeployment_missing", `${ENGINE}/executor.rs`);
  }
  for (const required of ["MAX_TRANSACTION_FEE_LAMPORTS", "get_fee_for_message", "fee_lamports > MAX_TRANSACTION_FEE_LAMPORTS"]) {
    requireText(executor, required, "multiply_fee_payer_bound_missing", `${ENGINE}/executor.rs`);
  }

  const render = file(RENDER_BLUEPRINT);
  if (!render.includes("name: loyal-multiply-route-worker")) {
    blocked("immutable_multiply_release_image_unpublished", {
      path: RENDER_BLUEPRINT,
      resume: "publish a linux/amd64 light-workers:sha-<commit> image containing multiply-route-worker, then declare loyal-multiply-route-worker pinned to that exact image",
    });
  }
  const renderService = sourceSection(render, "name: loyal-multiply-route-worker", "          - type:");
  for (const required of [
    "runtime: image",
    "autoDeploy: false",
    "light-workers:sha-",
    "preDeployCommand: /usr/local/bin/yield-migrations --apply",
    "dockerCommand: /usr/local/bin/multiply-route-worker run",
    "maxShutdownDelaySeconds: 60",
    "key: NEON_DATABASE_URL",
    "key: SOLANA_RPC_URL",
    "key: POLICY_KEYPAIR",
    "key: OBSERVABILITY_INGESTION_API_KEY",
  ]) {
    requireText(renderService, required, "multiply_render_service_contract_missing", RENDER_BLUEPRINT);
  }
  if (renderService.includes("SOLANA_TESTING_PK")) {
    fail("multiply_render_service_exposes_setup_authority", { path: RENDER_BLUEPRINT });
  }

  const migrations = file(MIGRATION_RUNNER);
  if (!/version:\s*53,[\s\S]{0,160}name:\s*"multiply_production_engine"/.test(migrations)) {
    fail("multiply_production_migration_not_registered", { path: MIGRATION_RUNNER });
  }
  return {
    buildInventorySha256: sha256(lightInventory),
    imageSha256: sha256(dockerfile),
    workflowSha256: sha256(workflow),
    renderServiceSha256: sha256(renderService),
  };
}

async function probeWorkerRole(): Promise<Json> {
  const child = Bun.spawn(
    ["cargo", "run", "-q", "-p", "loyal-fleet-worker", "--bin", "multiply-route-worker", "--", "--role-probe"],
    { cwd: ROOT, stdout: "pipe", stderr: "pipe", env: {} },
  );
  const [exitCode, stdout, stderr] = await Promise.all([
    child.exited,
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
  ]);
  if (exitCode !== 0) {
    fail("multiply_worker_role_probe_failed", {
      exitCode,
      stderrTail: stderr.split(/\r?\n/).slice(-20).join("\n"),
    });
  }
  let probe: Record<string, unknown>;
  try {
    probe = JSON.parse(stdout);
  } catch {
    fail("multiply_worker_role_probe_not_json", { stdoutSha256: sha256(stdout) });
  }
  const expected = {
    schemaVersion: 1,
    event: "fleet_worker_role_probe",
    status: "pass",
    role: "multiply_route_worker",
    networkAccessed: false,
    secretsLoaded: false,
    databaseMutated: false,
    transactionSent: false,
  };
  if (
    Object.keys(probe).length !== Object.keys(expected).length
    || Object.entries(expected).some(([key, value]) => probe[key] !== value)
  ) {
    fail("multiply_worker_role_probe_contract_drift", { probe });
  }
  return expected;
}

async function cargoCheck(): Promise<Json> {
  const child = Bun.spawn(
    ["cargo", "check", "-q", "-p", "loyal-fleet-worker", "--bin", "multiply-route-worker"],
    { cwd: ROOT, stdout: "pipe", stderr: "pipe" },
  );
  const [exitCode, stdout, stderr] = await Promise.all([
    child.exited,
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
  ]);
  if (exitCode !== 0) {
    fail("production_engine_does_not_compile", {
      exitCode,
      stdoutSha256: sha256(stdout),
      stderrSha256: sha256(stderr),
      stderrTail: stderr.split(/\r?\n/).slice(-20).join("\n"),
    });
  }
  return { command: "cargo check -q -p loyal-fleet-worker --bin multiply-route-worker", exitCode };
}

function requiredEnv(name: string): string {
  const value = process.env[name]?.trim();
  if (!value) blocked("terminal_environment_missing", { variable: name, resume: "run through op run --env-file=.env.1password" });
  return value;
}

async function verifyLive(): Promise<Json> {
  const rpcUrl = requiredEnv("SOLANA_RPC_URL");
  const databaseUrl = requiredEnv("NEON_DATABASE_URL");
  const connection = new Connection(rpcUrl, { commitment: "confirmed", httpAgent: false });
  const genesisHash = await connection.getGenesisHash();
  if (genesisHash !== MAINNET_GENESIS) fail("rpc_not_mainnet_beta", { genesisHash });
  const sql = neon(databaseUrl);

  const tableRows = await sql`
    SELECT table_name
    FROM information_schema.tables
    WHERE table_schema = 'loyal_yield'
      AND table_name LIKE 'multiply_%'
    ORDER BY table_name
  `;
  const tables = tableRows.map((row) => String(row.table_name));
  if (JSON.stringify(tables) !== JSON.stringify(["multiply_operations", "multiply_route_states"])) {
    fail("multiply_table_topology_drift", { tables });
  }
  const legacyColumnRows = await sql`
    SELECT column_name
    FROM information_schema.columns
    WHERE table_schema = 'loyal_yield'
      AND table_name = 'multiply_route_states'
      AND column_name IN (
        'pending_signed_wire',
        'pending_signed_wire_sha256',
        'pending_transaction_signature',
        'pending_recent_blockhash',
        'pending_last_valid_block_height',
        'pending_broadcast_intent_at'
      )
    ORDER BY column_name
  `;
  const legacyColumns = legacyColumnRows.map((row) => String(row.column_name));
  if (legacyColumns.length !== 0) fail("legacy_route_wire_survived", { legacyColumns });
  const routeRows = await sql`
    SELECT route_key, vault_id, state, state_version
    FROM loyal_yield.multiply_route_states
    ORDER BY route_key
  `;
  if (routeRows.length !== 1) fail("one_route_row_per_vault_drift", { routeCount: routeRows.length });
  const route = routeRows[0] as Record<string, unknown>;
  const state = route.state as Record<string, unknown>;
  if (Number(state.schemaVersion) !== 4 || Number(state.generation) !== Number(route.state_version) || state.routeKey !== route.route_key || Number(state.vaultId) !== Number(route.vault_id)) {
    fail("schema_v4_route_identity_drift", { routeKey: route.route_key, schemaVersion: state.schemaVersion });
  }
  const operationRows = await sql`
    SELECT operation_id, route_key, cycle, engine_version, action, strategy_key,
           status, idempotency_key, expected_effects, policy_account,
           policy_data_sha256, message_sha256, signed_wire,
           signed_wire_sha256, transaction_signature, recent_blockhash,
           last_valid_block_height, broadcast_intent_at, confirmed_slot,
           reconciliation_sha256, created_at, updated_at
    FROM loyal_yield.multiply_operations
    WHERE route_key = ${String(route.route_key)}
    ORDER BY created_at, operation_id
  `;
  const nonterminal = operationRows.filter((row) => !["reconciled", "expired", "manual_recovery"].includes(String(row.status)));
  if (nonterminal.length > 1) fail("more_than_one_nonterminal_operation", { count: nonterminal.length });
  const cycle = Number(state.cycle);
  const current = operationRows.filter((row) => Number(row.cycle) === cycle && row.engine_version === "linus_v1");
  if (current.length === 0) {
    blocked("fresh_linus_v1_lifecycle_missing", {
      routeKey: route.route_key,
      cycle,
      resume: "run one authorized fresh mainnet lifecycle with the production worker, then rerun this verifier",
    });
  }
  const reconciled = current.filter((row) => row.status === "reconciled");
  for (const operation of reconciled) {
    const deposit = operation.action === "deposit_claim_asset";
    const common = operation.signed_wire === null
      && typeof operation.signed_wire_sha256 === "string"
      && typeof operation.transaction_signature === "string"
      && typeof operation.recent_blockhash === "string"
      && Number(operation.confirmed_slot) > 0
      && typeof operation.reconciliation_sha256 === "string"
      && typeof operation.message_sha256 === "string";
    const complete = common && (deposit
      ? operation.last_valid_block_height === null
        && operation.broadcast_intent_at === null
        && operation.policy_account === null
        && operation.policy_data_sha256 === null
      : Number(operation.last_valid_block_height) > 0
        && operation.broadcast_intent_at !== null
        && typeof operation.policy_account === "string"
        && typeof operation.policy_data_sha256 === "string");
    if (!complete) fail("terminal_operation_evidence_incomplete", { operationId: operation.operation_id });
  }
  const signatures = reconciled.map((row) => String(row.transaction_signature));
  if (new Set(signatures).size !== signatures.length) fail("deterministic_signature_reused");
  const statusResponse = await connection.getSignatureStatuses(signatures, { searchTransactionHistory: true });
  if (statusResponse.value.some((status) => !status || status.err || !["confirmed", "finalized"].includes(String(status.confirmationStatus)))) {
    fail("reconciled_signature_not_confirmed", { signatureCount: signatures.length });
  }

  const actions = reconciled.map((row) => String(row.action));
  const firstSourceClose = reconciled.findIndex((row) => row.action === "withdraw_remaining_collateral" && row.strategy_key === "syrup_usdc_usdc");
  const firstTargetOpen = reconciled.findIndex((row) => row.action === "deposit_collateral" && row.strategy_key === "syrup_usdc_pyusd");
  if (firstSourceClose < 0 || firstTargetOpen <= firstSourceClose) {
    fail("generic_two_strategy_move_missing", { actions });
  }
  const reverse = reconciled.filter((row) => row.action === "swap_collateral_to_debt");
  const repays = reconciled.filter((row) => row.action === "repay_debt");
  if (reverse.length === 0 || repays.length === 0) fail("user_capital_down_missing");
  const sumDelta = (row: Record<string, unknown>, mint: string) => {
    const effects = row.expected_effects as { tokenDeltas?: Array<{ mint: string; rawDelta: number }> };
    return (effects.tokenDeltas ?? []).filter((delta) => delta.mint === mint).reduce((sum, delta) => sum + Number(delta.rawDelta), 0);
  };
  const debtMint = "2b1kV6DkPAnxd5ixfnxCpjxmKwqjjaYmCZfHsFu24GXo";
  const reverseDebt = reverse.reduce((sum, row) => sum + Math.max(0, sumDelta(row, debtMint)), 0);
  const repaidDebt = -repays.reduce((sum, row) => sum + Math.min(0, sumDelta(row, debtMint)), 0);
  if (reverseDebt < repaidDebt || repaidDebt <= 0) fail("down_used_unproven_debt_capital", { reverseDebt, repaidDebt });

  const expired = current.filter((row) => row.status === "expired");
  const recoveredExpiry = expired.some((failed) => current.some((retry) => retry.status === "reconciled" && retry.action === failed.action && retry.transaction_signature !== failed.transaction_signature && retry.message_sha256 !== failed.message_sha256));
  if (expired.length > 0 && !recoveredExpiry) fail("deterministic_expiry_recovery_missing", { expiredCount: expired.length });

  const deposit = reconciled.find((row) => row.action === "deposit_claim_asset");
  const claim = reconciled.find((row) => row.action === "claim");
  const withdrawal = state.withdrawal as Record<string, unknown> | null;
  if (!deposit || !claim || !withdrawal) fail("deposit_withdraw_claim_contract_missing");
  const deltas = (row: Record<string, unknown>) =>
    ((row.expected_effects as { tokenDeltas?: Array<{ account: string; mint: string; rawDelta: number }> }).tokenDeltas ?? []);
  const balancedTransfer = (row: Record<string, unknown>) => {
    const values = deltas(row);
    return values.length === 2
      && values[0]?.mint === values[1]?.mint
      && Number(values[0]?.rawDelta) + Number(values[1]?.rawDelta) === 0
      && Number(values[0]?.rawDelta) !== 0;
  };
  if (!balancedTransfer(deposit) || !balancedTransfer(claim)) {
    fail("deposit_or_claim_deltas_not_equal_and_opposite");
  }
  const requestedAt = new Date(String(withdrawal.requestedAt));
  const claimableAt = new Date(String(withdrawal.claimableAt));
  const unwindCompletedAt = new Date(String(withdrawal.unwindCompletedAt));
  if (![requestedAt, claimableAt, unwindCompletedAt].every((value) => Number.isFinite(value.getTime())) || claimableAt.getTime() - requestedAt.getTime() > 600_000 || unwindCompletedAt.getTime() - requestedAt.getTime() > 600_000) {
    fail("ten_minute_contract_drift", { requestedAt, claimableAt, unwindCompletedAt });
  }
  const view = state.frontend as Record<string, unknown> | null;
  if (!view || Number(view.generation) !== Number(state.generation) || view.observedSlot !== state.observedSlot) {
    fail("frontend_generation_or_observation_drift", { generation: state.generation, view });
  }
  return {
    genesisHash,
    routeKey: route.route_key,
    cycle,
    operationCount: current.length,
    reconciledCount: reconciled.length,
    expiredCount: expired.length,
    sourceCloseIndex: firstSourceClose,
    targetOpenIndex: firstTargetOpen,
    reverseDebtRaw: reverseDebt,
    repaidDebtRaw: repaidDebt,
    claimSignature: claim.transaction_signature,
  };
}

const architecture = checkArchitecture();
const releaseTopology = checkReleaseTopology();
const roleProbe = await probeWorkerRole();
const compilation = await cargoCheck();
const live = await verifyLive();
process.stdout.write(`${JSON.stringify({
  verdict: PASS,
  condition: "production_release_candidate_static_and_mainnet_contract_reconciled",
  evidence: { marker: PASS, architecture, releaseTopology, roleProbe, compilation, live },
}, null, 2)}\n`);
