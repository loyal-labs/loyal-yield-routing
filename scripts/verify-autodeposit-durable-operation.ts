import { readFile } from "node:fs/promises";
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

async function main() {
  let model: typeof import("./durable-autodeposit-operation");
  try {
    model = await import("./durable-autodeposit-operation");
  } catch (error) {
    check("durable operation model exists", false, String(error));
    return;
  }

  const initial = model.initialDurableAutodepositState();
  check(
    "deposit is preflighted before pull",
    model.nextDurableAutodepositAction(initial) === "preflight_deposit",
  );

  const ready = model.reduceDurableAutodepositState(initial, {
    type: "deposit_preflight_ready",
  });
  check(
    "pull starts only after deposit readiness",
    model.nextDurableAutodepositAction(ready) === "execute_pull",
  );

  const pulled = model.reduceDurableAutodepositState(ready, {
    type: "pull_confirmed",
    signature: "pull-signature",
  });
  check(
    "confirmed pull resumes deposit",
    model.nextDurableAutodepositAction(pulled) === "execute_deposit",
  );
  check("pull-only state cannot complete", !model.canCompleteDurableAutodeposit(pulled));
  check("pull-only state cannot notify success", !model.canNotifyDurableAutodepositSuccess(pulled));

  const retrying = model.reduceDurableAutodepositState(pulled, {
    type: "deposit_retryable_failure",
  });
  check(
    "retryable deposit resumes without another pull",
    model.nextDurableAutodepositAction(retrying) === "execute_deposit",
  );
  check(
    "retryable failure does not page",
    model.operationalAlertForDurableAutodeposit(retrying) === null,
  );

  const completed = model.reduceDurableAutodepositState(retrying, {
    type: "deposit_confirmed",
    signature: "deposit-signature",
  });
  check("deposit confirmation permits completion", model.canCompleteDurableAutodeposit(completed));
  check(
    "deposit confirmation permits success notification",
    model.canNotifyDurableAutodepositSuccess(completed),
  );

  const ambiguous = model.reduceDurableAutodepositState(pulled, {
    type: "deposit_ambiguous",
  });
  check(
    "ambiguous chain effect pages",
    model.operationalAlertForDurableAutodeposit(ambiguous) === "transaction_effect_ambiguous",
  );

  const ownershipLost = model.reduceDurableAutodepositState(pulled, {
    type: "durable_ownership_lost",
  });
  check(
    "lost durable ownership pages",
    model.operationalAlertForDurableAutodeposit(ownershipLost) === "durable_ownership_lost",
  );
  check(
    "a decreased post-pull source balance blocks another deposit",
    model.durableDepositRetryEffect({
      currentSourceBalanceRaw: 105n,
      postPullSourceBalanceRaw: 110n,
    }) === "ambiguous_prior_effect",
  );

  const [executor, trigger, fleetWorker, migration, dockerfile, workflow] = await Promise.all([
    source("scripts/execute-autodeposit-policy.ts"),
    source("crates/balance-sweep-autodeposit-trigger/src/main.rs"),
    source("crates/loyal-fleet-worker/src/lib.rs"),
    source("crates/loyal-yield-store/migrations/0040_durable_autodeposit_operation.sql"),
    source("Dockerfile.light-workers"),
    source(".github/workflows/worker-images.yml"),
  ]);

  check(
    "executor uses durable operation model",
    executor.includes("durable-autodeposit-operation"),
  );
  check(
    "executor preflights the direct Kamino deposit",
    executor.includes("preflightDurableKaminoDeposit"),
  );
  check(
    "confirmed pull resumes the direct Kamino deposit",
    executor.includes("resumeDurableKaminoDeposit"),
  );
  check(
    "the exact signed Kamino transaction is durable before broadcast",
    fleetWorker.includes("durablePolicyDepositTransaction") &&
      fleetWorker.includes("signedTransactionBase64") &&
      executor.includes('operationKind: "top_up"') &&
      executor.includes("sendPreparedTopUpOperation"),
  );
  check(
    "concurrent deposit runners are fenced",
    migration.includes("deposit_lease_token") &&
      migration.includes("deposit_lease_expires_at") &&
      executor.includes("another executor owns the active deposit lease"),
  );
  check(
    "a crash after an unrecorded deposit cannot consume unrelated idle funds",
    migration.includes("deposit_source_balance_raw") &&
      executor.includes("depositSourceBalanceRaw") &&
      executor.includes("durable post-pull baseline"),
  );
  check(
    "old idle-vault handoff is not the completion boundary",
    !executor.includes("publishConfirmedPullHandoff"),
  );
  check(
    "trigger no longer pages on recoverable idle-vault age",
    !trigger.includes("autodeposit_idle_vault_recovery_stalled"),
  );
  check(
    "runtime image includes operation model",
    dockerfile.includes("durable-autodeposit-operation.ts"),
  );
  check(
    "worker image workflow watches operation model",
    workflow.includes("durable-autodeposit-operation.ts"),
  );
}

await main();

for (const result of checks) {
  const prefix = result.pass ? "ok" : "not ok";
  console.log(`${prefix} - ${result.name}${result.detail ? `: ${result.detail}` : ""}`);
}

if (checks.length === 0 || checks.some((result) => !result.pass)) {
  console.log("FAIL_AUTODEPOSIT_DURABLE_OPERATION");
  process.exit(1);
}

console.log("PASS_AUTODEPOSIT_DURABLE_OPERATION");
