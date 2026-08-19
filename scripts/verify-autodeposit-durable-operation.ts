import { access, readFile } from "node:fs/promises";
import { join } from "node:path";

type Check = { name: string; pass: boolean; detail?: string };

const root = join(import.meta.dir, "..");
const checks: Check[] = [];

function check(name: string, pass: boolean, detail?: string) {
  checks.push({ name, pass, detail });
}

async function source(path: string) {
  return readFile(join(root, path), "utf8");
}

async function exists(path: string) {
  try {
    await access(join(root, path));
    return true;
  } catch {
    return false;
  }
}

async function main() {
  const [
    executor,
    trigger,
    fleetWorker,
    store,
    migration,
    productionMigrations,
    packageJson,
    staleCurrentReserveVerifier,
    dockerfile,
    workflow,
  ] = await Promise.all([
      source("scripts/execute-autodeposit-policy.ts"),
      source("crates/balance-sweep-autodeposit-trigger/src/main.rs"),
      source("crates/loyal-fleet-worker/src/lib.rs"),
      source("crates/loyal-yield-store/src/store.rs"),
      source(
        "crates/loyal-yield-store/migrations/0040_durable_autodeposit_operation.sql",
      ),
      source("crates/loyal-yield-orchestrator/src/bin/yield-migrations.rs"),
      source("package.json"),
      source("scripts/verify-autodeposit-stale-current-reserve.ts"),
      source("Dockerfile.light-workers"),
      source(".github/workflows/worker-images.yml"),
    ]);

  const duplicateModelExists = await exists(
    "scripts/durable-autodeposit-operation.ts",
  );
  const duplicateModelTestExists = await exists(
    "scripts/durable-autodeposit-operation.test.ts",
  );
  const obsoleteIdleHandoffVerifierExists = await exists(
    "scripts/verify-autodeposit-idle-vault-handoff.ts",
  );

  check(
    "the existing lot claim is the sole durable job owner",
    migration.includes("ALTER TABLE loyal_yield.balance_sweep_lot_claims") &&
      migration.includes("autodeposit_executor_lease_token") &&
      migration.includes("autodeposit_executor_lease_expires_at") &&
      migration.includes("autodeposit_deposit_plan"),
  );
  check(
    "the production migration runner applies migration 40",
    productionMigrations.includes("version: 40") &&
      productionMigrations.includes('name: "durable_autodeposit_operation"') &&
      productionMigrations.includes(
        'migrations/0040_durable_autodeposit_operation.sql',
      ),
  );
  check(
    "no parallel autodeposit operation table exists",
    !migration.includes("balance_sweep_autodeposit_operations") &&
      !executor.includes("balance_sweep_autodeposit_operations"),
  );
  check(
    "no duplicate operation state model exists",
    !duplicateModelExists &&
      !duplicateModelTestExists &&
      !executor.includes("durable-autodeposit-operation"),
  );
  check(
    "claim lease is acquired and checked by the executor",
    executor.includes("acquireAutodepositClaimLease") &&
      executor.includes("renewAutodepositClaimLease") &&
      executor.includes("releaseAutodepositClaimLease") &&
      executor.includes("autodeposit_executor_lease_token"),
  );
  check(
    "an immutable deposit plan is persisted before pull",
    executor.includes("persistAutodepositDepositPlan") &&
      executor.includes("autodeposit_deposit_plan") &&
      executor.includes("deposit plan conflicts with this execution"),
  );
  const recoveryQueryStart = trigger.indexOf("let recovery_rows = sqlx::query");
  const recoveryQuery = trigger.slice(
    recoveryQueryStart,
    trigger.indexOf(".fetch_all(pool)", recoveryQueryStart),
  );
  check(
    "restart recovery does not depend on the target remaining active",
    recoveryQuery.includes("attempt.attempt_state = ANY($3::text[])") &&
      !recoveryQuery.includes("target.active = true") &&
      !recoveryQuery.includes("target.lifecycle_status = 'active'"),
  );
  check(
    "signed attempts remain the only transaction ledger",
    executor.includes("balance_sweep_transaction_attempts") &&
      executor.includes('operationKind: "pull"') &&
      executor.includes('operationKind: "top_up"') &&
      executor.includes("persistPreparedAutodepositAttempt"),
  );
  check(
    "the route helper exposes exact signed top-up bytes but does not execute them",
    fleetWorker.includes("durablePolicyDepositTransaction") &&
      fleetWorker.includes("signedTransactionBase64") &&
      executor.includes("prepareSameMintReserveTopUp") &&
      !executor.includes("runSameMintReserveTopUp"),
  );
  check(
    "the direct path never publishes a fleet idle-balance handoff",
    !executor.includes(
      "INSERT INTO loyal_yield.vault_idle_token_balances_current",
    ) && !executor.includes("publishConfirmedPullHandoff"),
  );
  check(
    "a pre-existing idle balance defers the direct pull",
    executor.includes("assertEmptyVaultBeforeDirectAutodeposit") &&
      executor.includes("existing idle vault balance must drain before direct autodeposit"),
  );
  check(
    "fleet idle candidates exclude an in-flight direct deposit",
    store.includes("balance_sweep_transaction_attempts AS direct_pull") &&
      store.includes("direct_pull.operation_kind = 'pull'") &&
      store.includes("direct_pull.attempt_state = 'confirmed'") &&
      store.includes("direct_top_up.operation_kind = 'top_up'") &&
      store.includes("direct_top_up.attempt_state = 'confirmed'") &&
      store.includes("direct_top_up.id IS NULL") &&
      store.includes("direct_target.token_mint = idle.mint"),
  );
  const completionStart = executor.indexOf("async function completeAutodepositClaim");
  const completion = executor.slice(
    completionStart,
    executor.indexOf("async function resumeDirectKaminoDeposit", completionStart),
  );
  check(
    "claim completion locks and validates ownership before accounting",
    completion.includes("owned_claim AS MATERIALIZED") &&
      completion.includes("autodeposit_executor_lease_token") &&
      completion.includes("autodeposit_executor_lease_expires_at > now()") &&
      completion.includes("FOR UPDATE") &&
      completion.includes("JOIN owned_claim ON TRUE") &&
      completion.indexOf("owned_claim AS MATERIALIZED") <
        completion.indexOf("inserted_deposit AS"),
  );
  check(
    "claim completion is gated by the confirmed top-up attempt",
    completion.includes("operation_kind = 'top_up'") &&
      completion.includes("attempt_state = 'confirmed'") &&
      completion.includes("completed_claim") &&
      completion.includes("completed_slot"),
  );
  check(
    "ambiguous top-ups retain a typed operator-action failure",
    executor.includes("throwIfAutodepositAttemptRequiresOperator") &&
      executor.includes("throw new AutodepositEffectAmbiguousError") &&
      executor.includes("error instanceof AutodepositEffectAmbiguousError"),
  );
  check(
    "app accounting completes with the claim from the reconciled total",
    executor.includes("reconcileDirectDepositPosition") &&
      executor.includes("completed_deposit") &&
      executor.includes("completed_position") &&
      executor.includes("completed_holding_event") &&
      executor.includes("principal_amount_raw +") &&
      executor.includes("postConfirmPositionAmountRaw"),
  );
  check(
    "the removed idle-age operational alert stays absent",
    !trigger.includes("autodeposit_idle_vault_recovery_stalled"),
  );
  check(
    "the obsolete idle-handoff verifier is retired",
    !obsoleteIdleHandoffVerifierExists &&
      !packageJson.includes('"verify:autodeposit-idle-vault-handoff"'),
  );
  check(
    "the stale-reserve verifier uses the prepare-only top-up helper",
    staleCurrentReserveVerifier.includes("prepareSameMintReserveTopUp") &&
      !staleCurrentReserveVerifier.includes("runSameMintReserveTopUp"),
  );
  check(
    "runtime packaging does not carry the removed duplicate model",
    !dockerfile.includes("durable-autodeposit-operation.ts") &&
      !workflow.includes("durable-autodeposit-operation.ts"),
  );
}

await main();

for (const result of checks) {
  const prefix = result.pass ? "ok" : "not ok";
  console.log(
    `${prefix} - ${result.name}${result.detail ? `: ${result.detail}` : ""}`,
  );
}

if (checks.length === 0 || checks.some((result) => !result.pass)) {
  console.log("FAIL_AUTODEPOSIT_DURABLE_OPERATION");
  process.exit(1);
}

console.log("PASS_AUTODEPOSIT_DURABLE_OPERATION");
