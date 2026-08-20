#!/usr/bin/env bun
/**
 * End-to-end verification for the ASK-2020 closed-route-policy reconciliation.
 *
 * Reproduces the production retry storm in an isolated world and proves the fix stops
 * it. The `same-mint-reserve-swap` binary is a stub in a throwaway directory, Neon is an
 * in-memory `route_policies` table that records every statement, and the chain is a set
 * of live account addresses. No real database, RPC, signer, or chain access.
 *
 * The trigger's own guard is modelled, not executed: `balance-sweep-autodeposit-trigger`
 * reaps a scheduled slot without spawning the executor when the target has no
 * `route_policies` row with `active = true`, so eligibility here is exactly that
 * predicate.
 */
import { createHash } from "node:crypto";
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

import { PublicKey } from "@solana/web3.js";

import {
  closedRoutePolicyReconciliationIsNotActionable,
  readClosedRoutePolicyAccount,
  reconcileClosedRoutePolicy,
  reconcileClosedRoutePolicyFailure,
  prepareSameMintReserveTopUp,
  type EligibleTarget,
} from "./execute-autodeposit-policy";

const ROUTE_POLICY = "8csLZG4MsVtKJRbYvVULcYYhHq7BfbwwEnVFyZMipHUs";
const SETUP_POLICY = "9dcG2DMpy57UsEG39p1cDEcDgZgDV1ebJNpLTt17WwBi";
const DEPOSIT_RESERVE = "D6q6wuQSrifJKZYpR1M8R4YawnLDtDsMmWM1NbBmgJ59";
const DATABASE_URL = "postgres://verification-stub/none";
const PRODUCTION_FLEET_TARGET_COUNT = 2_500;
const PRODUCTION_CLOSED_POLICY_COUNT = 25;
const PRODUCTION_TRIGGER_TICKS = 20;

const ALT_COVERAGE_ERROR =
  "initial reserve deposit ALT coverage is incomplete before wallet funding";
const CONFIRM_TIMEOUT_ERROR =
  "unable to confirm transaction. This can happen in situations such as transaction expiration";

const target = {
  id: BigInt(1286),
  settings: "5XTtJAGTPdnz7T7Hnvpwv4A8NHxUCqtKuMQjnYTkRqdW",
  vaultIndex: 1,
  wallet: "5n8ovVj2rmErdgReeWihoeMVVqB2jAGx3GaYVbDk8PH1",
  walletUsdcAta: "8SoPTUZ4tcSXHJ3M4WMV6sMCAiAb26S2H51h34Pb24Ce",
  walletTokenAta: "8SoPTUZ4tcSXHJ3M4WMV6sMCAiAb26S2H51h34Pb24Ce",
  vaultPubkey: "FHGs2nuvk1UsJxHuTZCsMCEc2TqpypCoZYedSXCLmSpm",
  vaultUsdcAta: "APrV6SXX5KxTdvtqfSqxsby8p2d9PATVoBKyc7EKar8f",
  vaultTokenAta: "APrV6SXX5KxTdvtqfSqxsby8p2d9PATVoBKyc7EKar8f",
  tokenMint: "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v",
  sweepPolicyAccount: SETUP_POLICY,
  routePolicyId: BigInt(4471),
  routePolicyAccount: ROUTE_POLICY,
  routePolicyLastSeenSlot: BigInt(437_168_327),
  routePolicySeed: BigInt(2),
  routeModes: ["same_mint_kamino"],
  recurringDelegation: "Cby9L7a9TttgXobyLmGZV8tutEV3B8kh2e6YVDF381TG",
  walletBalanceFloorRaw: BigInt(0),
  maxAmountPerPeriodRaw: null,
  periodLengthSeconds: null,
  startTimestamp: null,
  currentReserve: null,
  currentMarket: null,
  currentLiquidityMint: null,
} satisfies EligibleTarget;

const failures: string[] = [];
let checks = 0;

function check(name: string, condition: boolean, detail?: unknown): void {
  checks += 1;
  if (condition) {
    console.log(`  ok   ${name}`);
    return;
  }
  const suffix = detail === undefined ? "" : ` -> ${JSON.stringify(detail)}`;
  console.log(`  FAIL ${name}${suffix}`);
  failures.push(`${name}${suffix}`);
}

type RoutePolicyRow = {
  id: string;
  policy_account: string;
  active: boolean;
  finalized_eligible?: boolean;
  last_seen_slot?: string;
  settings?: string;
  vault_index?: number;
  vault_pubkey?: string;
  active_policy_id?: string;
  vault_active?: boolean;
};

function createFakeNeon(rows: RoutePolicyRow[]) {
  const statements: { sql: string; values: unknown[] }[] = [];
  const neon =
    () =>
    async (strings: TemplateStringsArray, ...values: unknown[]) => {
      const sql = strings.raw.join(" ? ").replace(/\s+/g, " ").trim();
      statements.push({ sql, values });
      const deactivates =
        /UPDATE loyal_yield\.route_policies/i.test(sql) &&
        /SET active = false/i.test(sql);
      if (!deactivates) {
        return [];
      }
      const settings = String(values[0]);
      const vaultIndex = Number(values[1]);
      const vaultPubkey = String(values[2]);
      const policyId = String(values[3]);
      const account = String(values[4]);
      const lastSeenSlot = String(values[5]);
      const updated = rows.filter(
        (row) =>
          row.id === policyId &&
          row.policy_account === account &&
          row.active &&
          (row.last_seen_slot ?? target.routePolicyLastSeenSlot.toString()) ===
            lastSeenSlot &&
          (row.settings ?? target.settings) === settings &&
          (row.vault_index ?? target.vaultIndex) === vaultIndex &&
          (row.vault_pubkey ?? target.vaultPubkey) === vaultPubkey &&
          (row.active_policy_id ?? row.id) === row.id &&
          (row.vault_active ?? true)
      );
      const clearsFinalizedEligibility = /finalized_eligible\s*=\s*false/i.test(
        sql
      );
      for (const row of updated) {
        if ((row.finalized_eligible ?? true) && !clearsFinalizedEligibility) {
          throw new Error(
            'new row for relation "route_policies" violates check constraint "route_policies_finalized_eligible_check"'
          );
        }
        row.active = false;
        row.finalized_eligible = false;
      }
      return updated.map((row) => ({ id: row.id }));
    };
  return { neon: neon as never, statements };
}

function createFakeChain(probeAccountExists: boolean[]) {
  let reads = 0;
  const configs: unknown[] = [];
  const connection = {
    getAccountInfoAndContext: async (key: PublicKey, config: unknown) => {
      configs.push(config);
      const probeIndex = reads;
      reads += 1;
      const exists =
        probeAccountExists[
          Math.min(probeIndex, probeAccountExists.length - 1)
        ] ?? false;
      return {
        context: { slot: 437_300_000 + probeIndex },
        value: exists
          ? {
              lamports: 2_616_960,
              data: Buffer.alloc(0),
              owner: key,
              executable: false,
              rentEpoch: 0,
            }
          : null,
      };
    },
  };
  return {
    connection: connection as never,
    configs,
    reads: () => reads,
  };
}

function writeStub(directory: string): string {
  const stubPath = join(directory, "same-mint-reserve-swap-stub.ts");
  writeFileSync(stubPath, STUB_SOURCE, "utf8");
  return stubPath;
}

function installPlan(
  directory: string,
  missingAccount: string,
  missingAccountsByTarget: Record<string, string> = {}
): void {
  writeFileSync(
    join(directory, "plan.json"),
    JSON.stringify({ missingAccount, missingAccountsByTarget }),
    "utf8"
  );
  writeFileSync(join(directory, "invocations.log"), "", "utf8");
}

function spawnCount(directory: string): number {
  return readFileSync(join(directory, "invocations.log"), "utf8")
    .split("\n")
    .filter(Boolean).length;
}

function dryRun(): Promise<unknown> {
  return dryRunTarget(target);
}

function dryRunTarget(workerTarget: EligibleTarget): Promise<unknown> {
  return prepareSameMintReserveTopUp({
    amountRaw: BigInt(609_765),
    reserve: DEPOSIT_RESERVE,
    rpcUrl: "https://verification.invalid",
    target: workerTarget,
  });
}

/**
 * One pass of the production loop: the trigger only spawns the executor while the target
 * still has an active route policy, and the executor reconciles on the way out.
 */
async function simulateTicks(args: {
  directory: string;
  rows: RoutePolicyRow[];
  neon: never;
  connection: never;
  reconcile: boolean;
  ticks: number;
}): Promise<{ alerts: number; spawns: number }> {
  let spawns = 0;
  let alerts = 0;
  for (let tick = 0; tick < args.ticks; tick += 1) {
    const eligible = args.rows.some(
      (row) => row.policy_account === ROUTE_POLICY && row.active
    );
    if (!eligible) {
      continue;
    }
    spawns += 1;
    try {
      await dryRun();
    } catch (error) {
      if (!args.reconcile) {
        alerts += 1;
        continue;
      }
      const reconciliation = await reconcileClosedRoutePolicyFailure({
        connection: args.connection,
        databaseUrl: DATABASE_URL,
        error,
        execute: true,
        neon: args.neon,
        target,
      });
      if (
        !reconciliation ||
        !closedRoutePolicyReconciliationIsNotActionable(reconciliation)
      ) {
        alerts += 1;
      }
    }
  }
  return { alerts, spawns };
}

async function scenarioStormReproduced(directory: string): Promise<void> {
  console.log("\nwithout the fix: the storm runs forever");
  installPlan(directory, ROUTE_POLICY);
  const rows: RoutePolicyRow[] = [
    { id: "4471", policy_account: ROUTE_POLICY, active: true },
  ];
  const { neon } = createFakeNeon(rows);
  const { connection } = createFakeChain([false, false]);

  const outcome = await simulateTicks({
    connection,
    directory,
    neon,
    reconcile: false,
    rows,
    ticks: 10,
  });

  check("every tick spawns the executor", outcome.spawns === 10, outcome);
  check(
    "every spawn really ran the binary",
    spawnCount(directory) === 10,
    spawnCount(directory)
  );
  check("the policy stays active forever", rows[0].active === true);
}

async function scenarioStormStopped(directory: string): Promise<void> {
  console.log("\nwith the fix: the storm stops after one tick");
  installPlan(directory, ROUTE_POLICY);
  const rows: RoutePolicyRow[] = [
    { id: "4471", policy_account: ROUTE_POLICY, active: true },
  ];
  const { neon, statements } = createFakeNeon(rows);
  const { connection, configs, reads } = createFakeChain([false, false]);

  const outcome = await simulateTicks({
    connection,
    directory,
    neon,
    reconcile: true,
    rows,
    ticks: 10,
  });

  check("only the first tick spawns the executor", outcome.spawns === 1, outcome);
  check("expected closure emits no operational alert", outcome.alerts === 0, outcome);
  check(
    "no further binary invocations",
    spawnCount(directory) === 1,
    spawnCount(directory)
  );
  check("the policy is deactivated", rows[0].active === false);
  check(
    "the policy is no longer finalized-eligible",
    rows[0].finalized_eligible === false,
    rows[0]
  );
  check("chain was consulted twice before writing", reads() === 2, reads());
  check(
    "second finalized read is fenced to the first context",
    (configs[1] as { commitment?: string; minContextSlot?: number })
      ?.commitment === "finalized" &&
      (configs[1] as { minContextSlot?: number })?.minContextSlot ===
        437_300_000,
    configs
  );
  check(
    "exactly one deactivation statement was issued",
    statements.length === 1,
    statements.map((statement) => statement.sql)
  );
  check(
    "the statement scopes to the closed policy and only while active",
    statements[0]?.values[3] === target.routePolicyId.toString() &&
      statements[0]?.values[4] === ROUTE_POLICY &&
      statements[0]?.values[5] === target.routePolicyLastSeenSlot.toString() &&
      /vault\.active_policy_id = policy\.id/i.test(statements[0]?.sql ?? "") &&
      /FOR UPDATE OF policy/i.test(statements[0]?.sql ?? ""),
    statements[0]
  );
}

async function scenarioLivePolicyIsNeverDeactivated(
  directory: string
): Promise<void> {
  console.log(
    "\nsafety: a policy that still exists on chain is never deactivated"
  );
  installPlan(directory, ROUTE_POLICY);
  const rows: RoutePolicyRow[] = [
    { id: "4471", policy_account: ROUTE_POLICY, active: true },
  ];
  const { neon, statements } = createFakeNeon(rows);
  const { connection } = createFakeChain([true]);

  let outcome: unknown;
  try {
    await dryRun();
  } catch {
    outcome = await reconcileClosedRoutePolicy({
      connection,
      databaseUrl: DATABASE_URL,
      neon,
      target,
    });
  }

  check(
    "reconciliation is skipped",
    (outcome as { status?: string })?.status === "skipped",
    outcome
  );
  check("the policy stays active", rows[0].active === true);
  check("no statement was issued", statements.length === 0, statements);
}

async function scenarioOtherPolicyIsIgnored(directory: string): Promise<void> {
  console.log("\nscope: an error naming a different policy is ignored");
  installPlan(directory, SETUP_POLICY);

  let closed: string | null = "unset";
  try {
    await dryRun();
  } catch (error) {
    closed = readClosedRoutePolicyAccount(error, target.routePolicyAccount);
  }

  check(
    "the setup policy does not trigger reconciliation",
    closed === null,
    closed
  );
}

function scenarioUnrelatedErrorsAreIgnored(): void {
  console.log("\nscope: unrelated failures never reconcile");
  for (const [name, message] of [
    ["ALT coverage", ALT_COVERAGE_ERROR],
    ["confirm timeout", CONFIRM_TIMEOUT_ERROR],
    [
      "obligation missing",
      "deposit obligation 5YjZj3dk is missing for reserve",
    ],
  ] as const) {
    check(
      `${name} is not treated as a closed policy`,
      readClosedRoutePolicyAccount(new Error(message), ROUTE_POLICY) === null
    );
  }
  check(
    "the exact production error is recognised",
    readClosedRoutePolicyAccount(
      new Error(
        `same-mint Kamino top-up command failed with exit code 1: {"stderrTail":["{\\"error\\":\\"policy account ${ROUTE_POLICY} does not exist\\",\\"event\\":\\"same_mint_route_worker_fatal\\"}"]}`
      ),
      ROUTE_POLICY
    ) === ROUTE_POLICY
  );
}

async function scenarioReconciliationIsIdempotent(): Promise<void> {
  console.log("\nidempotence: a second pass is a no-op");
  const rows: RoutePolicyRow[] = [
    { id: "4471", policy_account: ROUTE_POLICY, active: true },
  ];
  const { neon } = createFakeNeon(rows);
  const { connection } = createFakeChain([false, false, false, false]);

  const first = await reconcileClosedRoutePolicy({
    connection,
    databaseUrl: DATABASE_URL,
    neon,
    target,
  });
  const second = await reconcileClosedRoutePolicy({
    connection,
    databaseUrl: DATABASE_URL,
    neon,
    target,
  });

  check(
    "the first pass deactivates one row",
    (first as { deactivatedPolicyIds?: string[] }).deactivatedPolicyIds
      ?.length === 1,
    first
  );
  check(
    "the second pass deactivates nothing",
    (second as { status?: string; reason?: string }).status === "skipped" &&
      (second as { reason?: string }).reason === "policy_binding_changed",
    second
  );
}

async function scenarioDryRunNeverMutates(directory: string): Promise<void> {
  console.log("\ndry-run safety: closed-policy simulation remains read-only");
  installPlan(directory, ROUTE_POLICY);
  const rows: RoutePolicyRow[] = [
    { id: "4471", policy_account: ROUTE_POLICY, active: true },
  ];
  const { neon, statements } = createFakeNeon(rows);
  const { connection, reads } = createFakeChain([false, false]);
  let outcome: unknown = "not-called";
  try {
    await dryRun();
  } catch (error) {
    outcome = await reconcileClosedRoutePolicyFailure({
      connection,
      databaseUrl: DATABASE_URL,
      error,
      execute: false,
      neon,
      target,
    });
  }

  check("dry run does not reconcile", outcome === null, outcome);
  check("dry run performs no chain proof", reads() === 0, reads());
  check(
    "dry run performs no database statement",
    statements.length === 0,
    statements
  );
  check("dry run leaves policy active", rows[0].active);
}

async function scenarioTransientNullFailsClosed(): Promise<void> {
  console.log(
    "\nRPC safety: one finalized null cannot deactivate a live policy"
  );
  const rows: RoutePolicyRow[] = [
    { id: "4471", policy_account: ROUTE_POLICY, active: true },
  ];
  const { neon, statements } = createFakeNeon(rows);
  const { connection, reads } = createFakeChain([false, true]);
  const outcome = await reconcileClosedRoutePolicy({
    connection,
    databaseUrl: DATABASE_URL,
    neon,
    target,
  });

  check(
    "second finalized probe rejects the transient null",
    outcome.status === "skipped" &&
      outcome.reason === "policy_account_exists_at_second_finalized_probe",
    outcome
  );
  check("both finalized probes ran", reads() === 2, reads());
  check(
    "transient null issues no database statement",
    statements.length === 0,
    statements
  );
  check("transient null leaves policy active", rows[0].active);
}

async function scenarioBindingCompareAndSetFailsClosed(): Promise<void> {
  console.log(
    "\ndatabase safety: newer policy evidence wins the reconciliation race"
  );
  const rows: RoutePolicyRow[] = [
    {
      id: "4471",
      policy_account: ROUTE_POLICY,
      active: true,
      last_seen_slot: (target.routePolicyLastSeenSlot + BigInt(1)).toString(),
    },
  ];
  const { neon, statements } = createFakeNeon(rows);
  const { connection } = createFakeChain([false, false]);
  const outcome = await reconcileClosedRoutePolicy({
    connection,
    databaseUrl: DATABASE_URL,
    neon,
    target,
  });

  check(
    "stale executor snapshot is rejected",
    outcome.status === "skipped" && outcome.reason === "policy_binding_changed",
    outcome
  );
  check("newer policy evidence remains active", rows[0].active);
  check(
    "compare-and-set statement was attempted once",
    statements.length === 1,
    statements
  );
}

function deterministicPublicKey(label: string, index: number): string {
  return new PublicKey(
    createHash("sha256").update(`${label}:${index}`).digest()
  ).toBase58();
}

function productionTarget(index: number): EligibleTarget {
  return {
    ...target,
    id: BigInt(10_000 + index),
    settings: deterministicPublicKey("settings", index),
    vaultIndex: index,
    vaultPubkey: deterministicPublicKey("vault", index),
    routePolicyId: BigInt(50_000 + index),
    routePolicyAccount: deterministicPublicKey("route-policy", index),
    routePolicyLastSeenSlot: BigInt(437_300_000 + index),
    routePolicySeed: BigInt(index + 1),
  };
}

async function scenarioProductionShapedWorkerLoad(
  directory: string
): Promise<void> {
  console.log(
    `\nproduction-shaped load: ${PRODUCTION_FLEET_TARGET_COUNT} targets, ` +
      `${PRODUCTION_CLOSED_POLICY_COUNT} concurrent closed-policy workers`
  );
  const targets = Array.from(
    { length: PRODUCTION_FLEET_TARGET_COUNT },
    (_, index) => productionTarget(index)
  );
  const scheduledTargets = targets.slice(0, PRODUCTION_CLOSED_POLICY_COUNT);
  const rows: RoutePolicyRow[] = targets.map((workerTarget) => ({
    id: workerTarget.routePolicyId.toString(),
    policy_account: workerTarget.routePolicyAccount,
    active: true,
    last_seen_slot: workerTarget.routePolicyLastSeenSlot.toString(),
    settings: workerTarget.settings,
    vault_index: workerTarget.vaultIndex,
    vault_pubkey: workerTarget.vaultPubkey,
    active_policy_id: workerTarget.routePolicyId.toString(),
    vault_active: true,
  }));
  installPlan(
    directory,
    ROUTE_POLICY,
    Object.fromEntries(
      scheduledTargets.map((workerTarget) => [
        `${workerTarget.settings}:${workerTarget.vaultIndex}`,
        workerTarget.routePolicyAccount,
      ])
    )
  );
  const { neon, statements } = createFakeNeon(rows);
  const { connection, reads } = createFakeChain(
    Array(PRODUCTION_CLOSED_POLICY_COUNT * 2).fill(false)
  );

  let spawnedWorkers = 0;
  let operationalAlerts = 0;
  let fleetRowsScanned = 0;
  for (let tick = 0; tick < PRODUCTION_TRIGGER_TICKS; tick += 1) {
    fleetRowsScanned += rows.length;
    const runnable = scheduledTargets.filter((workerTarget) =>
      rows.some(
        (row) => row.id === workerTarget.routePolicyId.toString() && row.active
      )
    );
    spawnedWorkers += runnable.length;
    await Promise.all(
      runnable.map(async (workerTarget) => {
        try {
          await dryRunTarget(workerTarget);
        } catch (error) {
          const reconciliation = await reconcileClosedRoutePolicyFailure({
            connection,
            databaseUrl: DATABASE_URL,
            error,
            execute: true,
            neon,
            target: workerTarget,
          });
          if (
            !reconciliation ||
            !closedRoutePolicyReconciliationIsNotActionable(reconciliation)
          ) {
            operationalAlerts += 1;
          }
        }
      })
    );
  }

  check(
    "production fleet scan volume is exercised",
    fleetRowsScanned ===
      PRODUCTION_FLEET_TARGET_COUNT * PRODUCTION_TRIGGER_TICKS,
    fleetRowsScanned
  );
  check(
    "each closed target spawns exactly one worker",
    spawnedWorkers === PRODUCTION_CLOSED_POLICY_COUNT,
    spawnedWorkers
  );
  check(
    "child-process worker count matches scheduler count",
    spawnCount(directory) === PRODUCTION_CLOSED_POLICY_COUNT,
    spawnCount(directory)
  );
  check(
    "every closed target is reconciled",
    rows.filter((row) => !row.active).length === PRODUCTION_CLOSED_POLICY_COUNT,
    rows.filter((row) => !row.active).length
  );
  check(
    "every reconciled policy is no longer finalized-eligible",
    rows
      .filter((row) => !row.active)
      .every((row) => row.finalized_eligible === false)
  );
  check(
    "expected closures emit no operational alerts",
    operationalAlerts === 0,
    operationalAlerts
  );
  check(
    "healthy fleet remains active",
    rows.filter((row) => row.active).length ===
      PRODUCTION_FLEET_TARGET_COUNT - PRODUCTION_CLOSED_POLICY_COUNT,
    rows.filter((row) => row.active).length
  );
  check(
    "every mutation has two finalized proofs",
    reads() === PRODUCTION_CLOSED_POLICY_COUNT * 2,
    reads()
  );
  check(
    "one compare-and-set statement is issued per closed target",
    statements.length === PRODUCTION_CLOSED_POLICY_COUNT,
    statements.length
  );
}

async function scenarioProductionWorkerUsesTerminalMissingPolicyResult(): Promise<void> {
  console.log(
    "\nworker contract: autodeposit policy absence is terminal, not fatal"
  );
  const subprocess = Bun.spawn(
    [
      "cargo",
      "test",
      "-p",
      "loyal-fleet-worker",
      "tests::autodeposit_deposit_missing_policy_is_terminal_not_fatal",
      "--",
      "--exact",
    ],
    { stdout: "pipe", stderr: "pipe" }
  );
  const [stdout, stderr, exitCode] = await Promise.all([
    new Response(subprocess.stdout).text(),
    new Response(subprocess.stderr).text(),
    subprocess.exited,
  ]);
  const output = `${stdout}\n${stderr}`;
  check(
    "the real worker returns a typed missing-policy result without the fatal path",
    exitCode === 0 && /test result: ok\. 1 passed/.test(output),
    { exitCode, output: output.slice(-2_000) }
  );
}

const STUB_SOURCE = `#!/usr/bin/env bun
import { appendFileSync, readFileSync } from "node:fs";
import { join } from "node:path";

const directory = process.env.VERIFY_STUB_DIR;
if (!directory) {
  throw new Error("VERIFY_STUB_DIR is required");
}
const plan = JSON.parse(readFileSync(join(directory, "plan.json"), "utf8"));
appendFileSync(join(directory, "invocations.log"), "spawn\\n");

function argValue(name) {
  const index = process.argv.indexOf(name);
  return index >= 0 ? process.argv[index + 1] : null;
}
const targetKey = argValue("--settings") + ":" + argValue("--vault-index");
const missingAccount =
  plan.missingAccountsByTarget?.[targetKey] ?? plan.missingAccount;

console.error(
  JSON.stringify({
    error: "policy account " + missingAccount + " does not exist",
    event: "same_mint_route_policy_missing",
  })
);
process.exit(1);
`;

async function main(): Promise<void> {
  const directory = mkdtempSync(join(tmpdir(), "autodeposit-policy-verify-"));
  const stubPath = writeStub(directory);
  process.env.VERIFY_STUB_DIR = directory;
  process.env.SAME_MINT_RESERVE_SWAP_COMMAND = `bun ${stubPath}`;
  process.env.POLICY_KEYPAIR ??= "verification-stub-no-signer";

  console.log("autodeposit closed-route-policy verification (ASK-2020)");
  console.log(`isolated stub directory: ${directory}`);

  try {
    await scenarioStormReproduced(directory);
    await scenarioStormStopped(directory);
    await scenarioDryRunNeverMutates(directory);
    await scenarioLivePolicyIsNeverDeactivated(directory);
    await scenarioTransientNullFailsClosed();
    await scenarioBindingCompareAndSetFailsClosed();
    await scenarioOtherPolicyIsIgnored(directory);
    scenarioUnrelatedErrorsAreIgnored();
    await scenarioReconciliationIsIdempotent();
    await scenarioProductionShapedWorkerLoad(directory);
    await scenarioProductionWorkerUsesTerminalMissingPolicyResult();
  } finally {
    rmSync(directory, { force: true, recursive: true });
  }

  console.log("");
  if (failures.length > 0) {
    console.log(`FAILED ${failures.length}/${checks} checks`);
    for (const failure of failures) {
      console.log(`  - ${failure}`);
    }
    process.exitCode = 1;
    return;
  }
  console.log(`PASSED ${checks}/${checks} checks`);
}

await main();
