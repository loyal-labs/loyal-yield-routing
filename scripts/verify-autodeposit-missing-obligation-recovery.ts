#!/usr/bin/env bun
/**
 * Verifier for pre-pull recovery of a missing Kamino deposit obligation.
 *
 * This is deliberately isolated: it uses production JSON shapes and the real
 * executor helpers, but never connects to Neon, Solana RPC, Render, or a signer.
 * PASS means every required safety property below held. Any failed property
 * exits non-zero.
 */
import {
  assertLookupTableReadinessBeforePull,
  assertNoTopUpPreflightBlockers,
  awaitTopUpLookupTableReadiness,
  readMissingDepositObligation,
  recoverMissingObligationBeforePull,
  runMissingObligationSetup,
  type EligibleTarget,
} from "./execute-autodeposit-policy";

const RESERVE = "D6q6wuQSrifJKZYpR1M8R4YawnLDtDsMmWM1NbBmgJ59";
const OBLIGATION = "HfwZYDSnbmqCDj73j417ERft53uoTqw8djsEoaXYwhf3";
const BLOCKER =
  `deposit obligation ${OBLIGATION} is missing for reserve ${RESERVE}; ` +
  "run the missing-obligation setup transaction before policy deposit";

const target = {
  id: 7694n,
  settings: "6scgzFo55CS94QNmgmuNtZQUCLZKzsUUG8YT1PcMuW27",
  vaultIndex: 1,
  wallet: "5n8ovVj2rmErdgReeWihoeMVVqB2jAGx3GaYVbDk8PH1",
  walletUsdcAta: "8SoPTUZ4tcSXHJ3M4WMV6sMCAiAb26S2H51h34Pb24Ce",
  walletTokenAta: "8SoPTUZ4tcSXHJ3M4WMV6sMCAiAb26S2H51h34Pb24Ce",
  vaultPubkey: "5ZnT5CVJpd3SNFoXX9kFHY1Ur2HbK95QYX7B5wfhT1y5",
  vaultUsdcAta: "APrV6SXX5KxTdvtqfSqxsby8p2d9PATVoBKyc7EKar8f",
  vaultTokenAta: "APrV6SXX5KxTdvtqfSqxsby8p2d9PATVoBKyc7EKar8f",
  tokenMint: "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v",
  sweepPolicyAccount: "9dcG2DMpy57UsEG39p1cDEcDgZgDV1ebJNpLTt17WwBi",
  routePolicyId: 8867n,
  routePolicyAccount: "EaV8NXzW3mG7nQqMtiD7mPFqvsifiemfdfvmTLeHqoGz",
  routePolicyLastSeenSlot: 437_168_327n,
  routePolicySeed: 2n,
  routeModes: ["same_mint_kamino"],
  recurringDelegation: "Cby9L7a9TttgXobyLmGZV8tutEV3B8kh2e6YVDF381TG",
  walletBalanceFloorRaw: 0n,
  maxAmountPerPeriodRaw: null,
  periodLengthSeconds: null,
  startTimestamp: null,
  currentReserve: RESERVE,
  currentMarket: "7u3HeHxYDLhnCoErrtycNokbQYbWGzLs6JSDqGAv5PfF",
  currentLiquidityMint: "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v",
} satisfies EligibleTarget;

type Result = Awaited<ReturnType<typeof runMissingObligationSetup>>;

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

function result(json: Record<string, unknown>): Result {
  return { command: [], exitCode: 0, stdout: "", stderr: "", json };
}

function reusableReadyResolution(): Record<string, unknown> {
  return {
    rollout: { mode: "reusable_only" },
    reusable: {
      ready: true,
      staticCoverage: true,
      packetFits: true,
      missingAddresses: [],
      tables: ["9xQeWvG816bUx9EPfEZGdeP6A1u8B23D7uX9QyeQz3d"],
      simulationError: null,
      transaction: { packetSizeBytes: 900 },
    },
    sharedMarketCatalog: { state: "covered" },
    selection: { blocker: null },
  };
}

function missingTopUp(): Result {
  return result({
    status: "initial_deposit_dry_run",
    preflightBlockers: [BLOCKER],
    missingObligationSetup: {
      targetObligation: OBLIGATION,
      targetReserve: RESERVE,
      policySource: "setup_policy",
    },
    lookupTableResolution: null,
  });
}

function readyTopUp(): Result {
  return result({
    status: "initial_deposit_dry_run",
    preflightBlockers: [
      "wallet USDC balance 0 is below needed funding amount 1000000",
    ],
    missingObligationSetup: null,
    lookupTableResolution: reusableReadyResolution(),
    policyDepositTransaction: { simulationError: null },
  });
}

function setupDryRun(): Result {
  return result({
    status: "setup_obligation_reserve_dry_run",
    sendsTransactions: false,
    target: {
      reserve: RESERVE,
      obligation: OBLIGATION,
      obligationExists: false,
    },
    missingObligationSetup: {
      targetObligation: OBLIGATION,
      targetReserve: RESERVE,
      policySource: "setup_policy",
      initExecution: { simulationError: null },
    },
    lookupTableResolution: reusableReadyResolution(),
  });
}

function setupExecuted(overrides: Record<string, unknown> = {}): Result {
  return result({
    status: "setup_obligation_reserve_executed",
    sendsTransactions: true,
    target: {
      reserve: RESERVE,
      obligation: OBLIGATION,
      obligationExists: true,
    },
    setup: {
      targetObligation: OBLIGATION,
      targetReserve: RESERVE,
      policySource: "setup_policy",
      initExecution: {
        signature: "4Nd1mYfYcD7K7vLRj2xP3rxA3Ckb2eB8xWmYwX8jv1a",
        confirmedSlot: "437579599",
      },
    },
    ...overrides,
  });
}

async function callerAllowsPullAfterRecovery(args: {
  initial: Result;
  execute: boolean;
  runSetup: (execute: boolean) => Promise<Result>;
  refreshTopUp: () => Promise<Result>;
  events: string[];
}): Promise<ReturnType<typeof recoverMissingObligationBeforePull>> {
  const recovered = await recoverMissingObligationBeforePull({
    dryRun: args.initial,
    execute: args.execute,
    reserve: RESERVE,
    pollIntervalMs: 1,
    timeoutMs: 2,
    runSetup: args.runSetup,
    refreshTopUp: args.refreshTopUp,
    sleep: async () => {},
  });
  if (!args.execute) {
    return recovered;
  }
  assertNoTopUpPreflightBlockers(recovered.topUpDryRun);
  const readiness = await awaitTopUpLookupTableReadiness({
    dryRun: recovered.topUpDryRun,
    refreshDryRun: args.refreshTopUp,
    reserve: RESERVE,
    pollIntervalMs: 1,
    timeoutMs: 2,
    sleep: async () => {},
  });
  assertLookupTableReadinessBeforePull(readiness);
  args.events.push("pull");
  return recovered;
}

async function expectRefusal(name: string, operation: () => Promise<unknown>) {
  try {
    await operation();
    check(name, false, "operation unexpectedly succeeded");
  } catch {
    check(name, true);
  }
}

async function main() {
  console.log("autodeposit missing-obligation recovery verifier");

  const executorSource = await Bun.file(
    new URL("./execute-autodeposit-policy.ts", import.meta.url)
  ).text();
  const preflightStart = executorSource.indexOf(
    "export async function preflightDurableKaminoDeposit"
  );
  const initialDryRunIndex = executorSource.indexOf(
    "const initialDryRun = await refreshTopUp();",
    preflightStart
  );
  const recoveryIndex = executorSource.indexOf(
    "const recovered = await recoverMissingObligationBeforePull({",
    preflightStart
  );
  const mainPreflightIndex = executorSource.indexOf(
    "const depositPreflight = await preflightDurableKaminoDeposit({"
  );
  const pullIndex = executorSource.indexOf(
    "const { result: durablePullSend } =",
    mainPreflightIndex
  );
  check(
    "production executor wires recovery after dry-run and before pull",
    initialDryRunIndex >= 0 &&
      initialDryRunIndex < recoveryIndex &&
      mainPreflightIndex >= 0 &&
      mainPreflightIndex < pullIndex
  );
  const recoveryWiring = executorSource.slice(
    recoveryIndex,
    executorSource.indexOf("assertNoTopUpPreflightBlockers", recoveryIndex)
  );
  check(
    "production recovery invokes the setup-only runner",
    recoveryWiring.includes("runSetup: (execute) =>") &&
      recoveryWiring.includes("runMissingObligationSetup({")
  );

  const originalCommand = process.env.SAME_MINT_RESERVE_SWAP_COMMAND;
  process.env.SAME_MINT_RESERVE_SWAP_COMMAND = "/usr/bin/true";
  try {
    const dryCommand = await runMissingObligationSetup({
      execute: false,
      reserve: RESERVE,
      rpcUrl: "http://verification.invalid",
      target,
    });
    const liveCommand = await runMissingObligationSetup({
      execute: true,
      reserve: RESERVE,
      rpcUrl: "http://verification.invalid",
      target,
    });
    check(
      "setup runner uses the setup-only reserve mode",
      dryCommand.command.includes("--setup-obligation-reserve") &&
        !dryCommand.command.includes("--deposit-reserve")
    );
    check(
      "dry setup command sends nothing",
      !dryCommand.command.includes("--execute")
    );
    check(
      "live setup command opts into execution explicitly",
      liveCommand.command.includes("--execute")
    );
  } finally {
    if (originalCommand === undefined) {
      delete process.env.SAME_MINT_RESERVE_SWAP_COMMAND;
    } else {
      process.env.SAME_MINT_RESERVE_SWAP_COMMAND = originalCommand;
    }
  }

  const events: string[] = [];
  const recovered = await callerAllowsPullAfterRecovery({
    initial: missingTopUp(),
    execute: true,
    events,
    runSetup: async (execute) => {
      events.push(execute ? "setup:execute" : "setup:dry-run");
      return execute ? setupExecuted() : setupDryRun();
    },
    refreshTopUp: async () => {
      events.push("top-up:refresh");
      return readyTopUp();
    },
  });
  check(
    "setup dry-run, setup confirmation, refreshed top-up, then pull are ordered",
    events.join(",") ===
      "setup:dry-run,setup:execute,top-up:refresh,pull",
    events
  );
  check(
    "recovered top-up no longer reports the missing obligation",
    readMissingDepositObligation(recovered.topUpDryRun) === null
  );
  check(
    "executed recovery records the confirmed setup result",
    recovered.recovery.status === "executed" &&
      recovered.recovery.setupExecution !== null
  );

  const dryEvents: string[] = [];
  const dryRecovery = await callerAllowsPullAfterRecovery({
    initial: missingTopUp(),
    execute: false,
    events: dryEvents,
    runSetup: async (execute) => {
      dryEvents.push(execute ? "setup:execute" : "setup:dry-run");
      return setupDryRun();
    },
    refreshTopUp: async () => {
      dryEvents.push("top-up:refresh");
      return readyTopUp();
    },
  });
  check(
    "non-execute mode simulates setup without setup execution or pull",
    dryEvents.join(",") === "setup:dry-run",
    dryEvents
  );
  check(
    "non-execute mode reports setup readiness",
    dryRecovery.recovery.status === "dry_run_ready"
  );

  const fastPathEvents: string[] = [];
  await callerAllowsPullAfterRecovery({
    initial: readyTopUp(),
    execute: true,
    events: fastPathEvents,
    runSetup: async () => {
      fastPathEvents.push("setup");
      return setupDryRun();
    },
    refreshTopUp: async () => {
      fastPathEvents.push("top-up:refresh");
      return readyTopUp();
    },
  });
  check(
    "existing-obligation fast path performs no setup",
    fastPathEvents.join(",") === "pull",
    fastPathEvents
  );

  for (const scenario of [
    {
      name: "setup simulation failure refuses the pull",
      runSetup: async (execute: boolean) =>
        execute
          ? setupExecuted()
          : setupDryRunWith({ initExecution: { simulationError: "custom(1)" } }),
      refresh: async () => readyTopUp(),
    },
    {
      name: "setup lookup-table unreadiness refuses the pull",
      runSetup: async (execute: boolean) =>
        execute
          ? setupExecuted()
          : result({
              ...setupDryRun().json,
              lookupTableResolution: {
                rollout: { mode: "reusable_only" },
                reusable: {
                  ready: false,
                  missingAddresses: [RESERVE],
                  packetFits: true,
                  tables: [],
                },
                sharedMarketCatalog: { state: "pending" },
              },
            }),
      refresh: async () => readyTopUp(),
    },
    {
      name: "setup execution without confirmation refuses the pull",
      runSetup: async (execute: boolean) =>
        execute
          ? setupExecuted({ setup: { initExecution: { signature: null } } })
          : setupDryRun(),
      refresh: async () => readyTopUp(),
    },
    {
      name: "persistent missing obligation refuses the pull",
      runSetup: async (execute: boolean) =>
        execute ? setupExecuted() : setupDryRun(),
      refresh: async () => missingTopUp(),
    },
    {
      name: "refreshed blocked route refuses the pull",
      runSetup: async (execute: boolean) =>
        execute ? setupExecuted() : setupDryRun(),
      refresh: async () =>
        result({
          status: "initial_deposit_dry_run",
          preflightBlockers: ["route policy constraint mismatch"],
          missingObligationSetup: null,
          lookupTableResolution: null,
        }),
    },
  ]) {
    const refusalEvents: string[] = [];
    await expectRefusal(scenario.name, () =>
      callerAllowsPullAfterRecovery({
        initial: missingTopUp(),
        execute: true,
        events: refusalEvents,
        runSetup: scenario.runSetup,
        refreshTopUp: scenario.refresh,
      })
    );
    check(
      `${scenario.name}: pull remained unsent`,
      !refusalEvents.includes("pull"),
      refusalEvents
    );
  }

  if (failures.length > 0) {
    console.error(`FAIL: ${failures.length}/${checks} required checks failed`);
    for (const failure of failures) console.error(`- ${failure}`);
    process.exitCode = 1;
    return;
  }
  console.log(`PASS: ${checks}/${checks} required checks passed`);
}

function setupDryRunWith(
  missingSetupOverrides: Record<string, unknown>
): Result {
  const base = setupDryRun();
  return result({
    ...base.json,
    missingObligationSetup: {
      ...((base.json?.missingObligationSetup as Record<string, unknown>) ?? {}),
      ...missingSetupOverrides,
    },
  });
}

await main();
