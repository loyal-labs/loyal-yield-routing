import { readFileSync } from "node:fs";
import { join } from "node:path";

const ROOT = join(import.meta.dir, "..");

type Check = {
  name: string;
  passed: boolean;
  detail?: unknown;
};

const checks: Check[] = [];

function check(name: string, passed: boolean, detail?: unknown): void {
  checks.push({ name, passed, ...(detail === undefined ? {} : { detail }) });
}

function read(path: string): string {
  return readFileSync(join(ROOT, path), "utf8");
}

function between(source: string, start: string, end: string): string {
  const startIndex = source.indexOf(start);
  const endIndex = source.indexOf(end, startIndex + start.length);
  if (startIndex < 0 || endIndex < 0) return "";
  return source.slice(startIndex, endIndex);
}

function hasReservationPredicate(source: string): boolean {
  const required = [
    "FROM loyal_yield.balance_sweep_lot_claims AS direct_claim",
    "direct_claim.status = 'selected'",
    "direct_target.token_mint = idle.mint",
    "direct_vault.id = idle.vault_id",
    "direct_pull.operation_kind = 'pull'",
    "direct_pull.attempt_state IN (",
    "'prepared'",
    "'submitted'",
    "'confirmed'",
    "'unknown'",
    "'ambiguous'",
    "direct_top_up.operation_kind = 'top_up'",
    "direct_top_up.attempt_state = 'confirmed'",
    "direct_top_up.id IS NULL",
  ];
  return required.every((token) => source.includes(token));
}

const observation = read(
  "crates/loyal-yield-orchestrator/src/fleet_orchestration/observation.rs"
);
const store = read("crates/loyal-yield-store/src/store.rs");
const executor = read("scripts/execute-autodeposit-policy.ts");
const databaseTest = read(
  "crates/loyal-yield-store/tests/autodeposit_fleet_idle_ownership_db.rs"
);
const agents = read("AGENTS.md");
const packageJson = JSON.parse(read("package.json")) as {
  scripts?: Record<string, string>;
};

const idlePlannerBranches = observation
  .split("'idle_vault_usdc'::TEXT AS source_kind")
  .slice(1)
  .map((branch) => branch.slice(0, branch.indexOf("\n        )\n        SELECT")))
  .filter((branch) => branch.length > 0);

check(
  "both Fleet planner query variants expose an idle source branch",
  idlePlannerBranches.length === 2,
  { count: idlePlannerBranches.length }
);
check(
  "current-schema Fleet planner excludes active Autodeposit pulls",
  hasReservationPredicate(idlePlannerBranches[0] ?? "")
);
check(
  "migration-22 fallback does not name post-migration transaction attempts",
  !(idlePlannerBranches[1] ?? "").includes(
    "balance_sweep_transaction_attempts"
  )
);

const singleIdleReader = between(
  store,
  "pub async fn current_idle_token_balance(",
  "pub async fn current_idle_token_balances_for_vaults("
);
const batchIdleReader = between(
  store,
  "pub async fn current_idle_token_balances_for_vaults(",
  "pub async fn record_current_idle_token_balance("
);

check(
  "Fleet executor single-vault idle reader excludes active Autodeposit pulls",
  hasReservationPredicate(singleIdleReader)
);
check(
  "Fleet batch idle reader excludes active Autodeposit pulls",
  hasReservationPredicate(batchIdleReader)
);

const idleDecisionWriter = between(
  store,
  "pub async fn record_idle_vault_deposit_decision(",
  "pub async fn record_idle_vault_deposit_decision_with_signed_submission("
);
check(
  "Fleet atomically arbitrates ownership before creating an idle decision",
  idleDecisionWriter.includes("acquire_idle_vault_handoff_lock(") &&
    idleDecisionWriter.includes("active_autodeposit_pull_exists(")
);

const preparedAttemptWriter = between(
  executor,
  "async function persistPreparedAutodepositAttempt(",
  "function attemptErrorDetail("
);
check(
  "Autodeposit uses the same transaction lock and refuses a new pull owned by Fleet",
  preparedAttemptWriter.includes("sql.transaction([") &&
    preparedAttemptWriter.includes("idle-vault-handoff:%s:%s") &&
    preparedAttemptWriter.includes("fleet_decision.execution_plan ->> 'kind'") &&
    preparedAttemptWriter.includes("'idle_vault_deposit'")
);
check(
  "the database test forces competing ownership through the shared lock",
  databaseTest.includes("!fleet_attempt.is_finished()") &&
    databaseTest.includes("!autodeposit_attempt.is_finished()") &&
    databaseTest.includes("Fleet must lose when Autodeposit prepares the pull first") &&
    databaseTest.includes("Autodeposit must refuse to prepare a pull while Fleet owns")
);

for (const reader of [
  singleIdleReader,
  batchIdleReader,
  idlePlannerBranches[0] ?? "",
]) {
  const activeStateStart = reader.indexOf("direct_pull.attempt_state IN (");
  const activeStateEnd = reader.indexOf(")", activeStateStart);
  const activeStates =
    activeStateStart >= 0 && activeStateEnd >= 0
      ? reader.slice(activeStateStart, activeStateEnd)
      : "";
  check(
    "terminal failed and expired pulls do not reserve idle funds",
    !activeStates.includes("'failed'") && !activeStates.includes("'expired'")
  );
}

check(
  "the project records the two-transaction size constraint",
  agents.includes("Autodeposit transaction boundary") &&
    agents.includes("exceeds Solana's transaction size limit") &&
    agents.includes("must remain separate transactions")
);
check(
  "the fix does not add a generic movement ownership table",
  !`${store}\n${observation}`.includes("idle_vault_movement_claims")
);
check(
  "package.json exposes the authoritative verifier",
  packageJson.scripts?.["verify:autodeposit-fleet-idle-ownership"] ===
    "bash scripts/verify-autodeposit-fleet-idle-ownership.sh"
);

for (const result of checks) {
  console.log(
    JSON.stringify({
      status: result.passed ? "PASS" : "FAIL",
      check: result.name,
      ...(result.detail === undefined ? {} : { detail: result.detail }),
    })
  );
}

const failed = checks.filter((result) => !result.passed);
console.log(
  JSON.stringify({
    verifier: "autodeposit-fleet-idle-ownership",
    requiredChecks: checks.length,
    failedChecks: failed.length,
    verdict: failed.length === 0 ? "PASS" : "FAIL",
  })
);

if (failed.length > 0) process.exit(1);
