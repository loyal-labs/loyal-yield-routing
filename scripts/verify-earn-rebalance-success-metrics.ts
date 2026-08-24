#!/usr/bin/env bun

import { existsSync, readFileSync } from "node:fs";
import { resolve } from "node:path";

const ROOT = resolve(import.meta.dir, "..");
const PASS = "PASS_EARN_REBALANCE_SUCCESS_METRICS";
const FAIL = "FAIL_EARN_REBALANCE_SUCCESS_METRICS";

type Check = {
  name: string;
  passed: boolean;
  detail: string;
};

const checks: Check[] = [];

function record(name: string, passed: boolean, detail: string): void {
  checks.push({ name, passed, detail });
  process.stdout.write(`${passed ? "PASS" : "FAIL"} ${name}: ${detail}\n`);
}

function file(relative: string): string | null {
  const absolute = resolve(ROOT, relative);
  if (!existsSync(absolute)) return null;
  return readFileSync(absolute, "utf8");
}

function requireOrdered(
  relative: string,
  before: string,
  after: string,
  name: string,
): void {
  const source = file(relative);
  if (source === null) {
    record(name, false, `${relative} is missing`);
    return;
  }
  const beforeIndex = source.indexOf(before);
  const afterIndex = source.indexOf(after, Math.max(0, beforeIndex));
  record(
    name,
    beforeIndex >= 0 && afterIndex > beforeIndex,
    beforeIndex < 0
      ? `durable marker ${JSON.stringify(before)} is missing`
      : afterIndex < 0
        ? `success marker ${JSON.stringify(after)} is missing after the durable marker`
        : `${after} follows ${before}`,
  );
}

async function run(
  name: string,
  command: string[],
  requiredOutput?: RegExp,
): Promise<void> {
  const child = Bun.spawn(command, {
    cwd: ROOT,
    env: { ...process.env, NO_DNA: "1" },
    stdout: "pipe",
    stderr: "pipe",
  });
  const [exitCode, stdout, stderr] = await Promise.all([
    child.exited,
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
  ]);
  const output = `${stdout}\n${stderr}`;
  const outputMatched = requiredOutput === undefined || requiredOutput.test(output);
  record(
    name,
    exitCode === 0 && outputMatched,
    exitCode !== 0
      ? `exit ${exitCode}: ${stderr.split(/\r?\n/).slice(-8).join(" | ")}`
      : !outputMatched
        ? `required behavioral test output was absent`
        : command.join(" "),
  );
}

const packageSource = file("package.json");
let packageEntrypoint = false;
if (packageSource !== null) {
  const parsed = JSON.parse(packageSource) as { scripts?: Record<string, string> };
  packageEntrypoint =
    parsed.scripts?.["verify:earn-rebalance-success-metrics"] ===
    "bun scripts/verify-earn-rebalance-success-metrics.ts";
}
record(
  "verifier_entrypoint",
  packageEntrypoint,
  packageEntrypoint ? "package script is authoritative" : "package script is missing or incorrect",
);

const metricSource = file("crates/loyal-observability/src/earn_rebalance.rs");
const operations = [
  "ata.observation_persisted",
  "opportunity.published",
  "route.revalidated",
  "route.execution_handoff_persisted",
  "route.confirmed",
  "route.reconciled",
];
const metricContractPresent =
  metricSource !== null && operations.every((operation) => metricSource.includes(operation));
record(
  "typed_metric_contract",
  metricContractPresent,
  metricContractPresent ? "all six stable operations exist" : "typed six-stage contract is missing",
);

const forbiddenMetricAttributes = [
  "loyal.wallet",
  "loyal.vault",
  "loyal.opportunity",
  "loyal.route.id",
  "transaction.signature",
  "transaction.payload",
  "error.detail",
];
const boundedAttributes =
  metricSource !== null &&
  forbiddenMetricAttributes.every((attribute) => !metricSource.includes(attribute));
record(
  "bounded_metric_attributes",
  boundedAttributes,
  boundedAttributes
    ? "no forbidden runtime-valued metric attributes"
    : "a forbidden runtime-valued metric attribute is present",
);

requireOrdered(
  "crates/balance-sweep-ata-monitor/src/lib.rs",
  "sink.record_observation(observation).await?",
  "EarnRebalanceStage::AtaObservationPersisted",
  "ata_success_after_persistence",
);
requireOrdered(
  "crates/loyal-yield-orchestrator/src/bin/fleet-opportunity-planner.rs",
  "upsert_rebalance_opportunity(input).await?",
  "EarnRebalanceStage::OpportunityPublished",
  "planner_success_after_publication",
);
requireOrdered(
  "crates/loyal-fleet-worker/src/lib.rs",
  "finish_fleet_worker_task(&client, result).await",
  "EarnRebalanceStage::RouteRevalidated",
  "revalidator_success_after_durable_transition",
);
requireOrdered(
  "crates/loyal-fleet-worker/src/lib.rs",
  "finish_fleet_worker_task(&client, result).await",
  "EarnRebalanceStage::RouteExecutionHandoffPersisted",
  "executor_success_after_durable_handoff",
);
requireOrdered(
  "crates/loyal-yield-orchestrator/src/bin/fleet-route-confirmer.rs",
  "SignedRouteSubmissionAdvance::ReconciliationPending",
  "EarnRebalanceStage::RouteConfirmed",
  "confirmer_success_after_reconciliation_handoff",
);
requireOrdered(
  "crates/loyal-fleet-worker/src/lib.rs",
  "SignedRouteSubmissionAdvance::Reconciled",
  "EarnRebalanceStage::RouteReconciled",
  "reconciler_success_after_durable_reconciliation",
);

await run(
  "metric_export_behavior",
  ["cargo", "test", "-p", "loyal-observability", "earn_rebalance_success_metrics_contract", "--", "--nocapture"],
  /earn_rebalance_success_metrics_contract[^\n]*ok/,
);
await run(
  "worker_outcome_classification",
  ["cargo", "test", "-p", "loyal-fleet-worker", "earn_rebalance_stage_success_classification", "--", "--nocapture"],
  /earn_rebalance_stage_success_classification[^\n]*ok/,
);
await run("observability_and_ata_compile", [
  "cargo",
  "check",
  "--locked",
  "-p",
  "loyal-observability",
  "-p",
  "balance-sweep-ata-monitor",
  "--bin",
  "balance-sweep-ata-monitor",
]);
await run("planner_and_confirmer_compile", [
  "cargo",
  "check",
  "--locked",
  "-p",
  "loyal-yield-orchestrator",
  "--bin",
  "fleet-opportunity-planner",
  "--bin",
  "fleet-route-confirmer",
]);
await run("revalidator_executor_reconciler_compile", [
  "cargo",
  "check",
  "--locked",
  "-p",
  "loyal-fleet-worker",
  "--bin",
  "same-mint-reserve-swap",
]);
await run("rust_format", ["cargo", "fmt", "--all", "--", "--check"]);
await run("diff_hygiene", ["git", "diff", "--check"]);

const failed = checks.filter((check) => !check.passed);
process.stdout.write(`\n${failed.length === 0 ? PASS : FAIL}\n`);
if (failed.length > 0) {
  process.stdout.write(`failed_conditions=${failed.map((check) => check.name).join(",")}\n`);
  process.exit(1);
}
