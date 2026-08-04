#!/usr/bin/env bun
/**
 * End-to-end verification for the ASK-2006 autodeposit lookup-table readiness gate.
 *
 * Runs the real executor helpers against a stub `same-mint-reserve-swap` binary in a
 * throwaway directory, so no Neon, RPC, signer, or chain access is involved. Each
 * scenario drives the same code path production uses: a spawned subprocess, the real
 * stdout JSON extraction, the real exit-code-to-Error mapping, and the real gate.
 */
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

import {
  awaitTopUpLookupTableReadiness,
  classifyTopUpFailure,
  readTopUpLookupTableCoverage,
  runSameMintReserveTopUp,
  runTopUpWithLookupTableRetry,
} from "./execute-autodeposit-policy";

type StubDryRunMode =
  | "covered"
  | "incomplete"
  | "funding_required"
  | "no_resolution";
type StubExecuteMode = "alt_coverage_error" | "confirm_timeout" | "executed";
type StubPlan = { dryRuns: StubDryRunMode[]; executes: StubExecuteMode[] };

const DEPOSIT_RESERVE = "D6q6wuQSrifJKZYpR1M8R4YawnLDtDsMmWM1NbBmgJ59";
const ALT_COVERAGE_STDERR =
  "initial reserve deposit ALT coverage is incomplete before wallet funding: " +
  "reusable lookup-table coverage is incomplete or the exact simulation failure is not " +
  "the expected missing-token-account prerequisite";
const CONFIRM_TIMEOUT_STDERR =
  "unable to confirm transaction. This can happen in situations such as transaction " +
  "expiration and insufficient fee-payer funds";

const target = {
  id: BigInt(7957),
  settings: "Cauk6KoDbsa3WduikBFsDBnaZKLDKQpXeURe89xcmKSU",
  vaultIndex: 1,
  wallet: "5n8ovVj2rmErdgReeWihoeMVVqB2jAGx3GaYVbDk8PH1",
  walletUsdcAta: "8SoPTUZ4tcSXHJ3M4WMV6sMCAiAb26S2H51h34Pb24Ce",
  walletTokenAta: "8SoPTUZ4tcSXHJ3M4WMV6sMCAiAb26S2H51h34Pb24Ce",
  vaultPubkey: "GSSNmpnnWvJdkx1TgUKEUw1csANd5tAuBUNQXySCx2Kk",
  vaultUsdcAta: "APrV6SXX5KxTdvtqfSqxsby8p2d9PATVoBKyc7EKar8f",
  vaultTokenAta: "APrV6SXX5KxTdvtqfSqxsby8p2d9PATVoBKyc7EKar8f",
  tokenMint: "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v",
  sweepPolicyAccount: "9dcG2DMpy57UsEG39p1cDEcDgZgDV1ebJNpLTt17WwBi",
  routePolicyAccount: "9dcG2DMpy57UsEG39p1cDEcDgZgDV1ebJNpLTt17WwBi",
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
} as unknown as Parameters<typeof runSameMintReserveTopUp>[0]["target"];

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

function writeStub(directory: string): string {
  const stubPath = join(directory, "same-mint-reserve-swap-stub.ts");
  writeFileSync(stubPath, STUB_SOURCE, "utf8");
  return stubPath;
}

function installPlan(directory: string, plan: StubPlan): void {
  writeFileSync(join(directory, "plan.json"), JSON.stringify(plan), "utf8");
  writeFileSync(join(directory, "invocations.log"), "", "utf8");
}

function readInvocations(directory: string): string[] {
  return readFileSync(join(directory, "invocations.log"), "utf8")
    .split("\n")
    .filter(Boolean);
}

function dryRun(): Promise<Awaited<ReturnType<typeof runSameMintReserveTopUp>>> {
  return runSameMintReserveTopUp({
    amountRaw: BigInt(1_000_000),
    execute: false,
    reserve: DEPOSIT_RESERVE,
    rpcUrl: "https://verification.invalid",
    target,
  });
}

function executeRun(): Promise<
  Awaited<ReturnType<typeof runSameMintReserveTopUp>>
> {
  return runSameMintReserveTopUp({
    amountRaw: BigInt(1_000_000),
    execute: true,
    reserve: DEPOSIT_RESERVE,
    rpcUrl: "https://verification.invalid",
    target,
  });
}

const noWait = async (): Promise<void> => {};

async function scenarioSteadyStateStaysFast(directory: string): Promise<void> {
  console.log("\nsteady state: covered coverage never delays the pull");
  installPlan(directory, { dryRuns: ["covered"], executes: ["executed"] });

  const readiness = await awaitTopUpLookupTableReadiness({
    dryRun: await dryRun(),
    pollIntervalMs: 10_000,
    refreshDryRun: dryRun,
    sleep: noWait,
    timeoutMs: 240_000,
  });

  check("gate reports ready", readiness.status === "ready", readiness.status);
  check("gate does not re-run the dry run", readiness.attempts === 1, readiness.attempts);
  check(
    "only the caller's dry run was spawned",
    readInvocations(directory).length === 1,
    readInvocations(directory)
  );
  check("no execute leg was reached", !readInvocations(directory).includes("execute"));
}

async function scenarioFundingBlockerIsReady(directory: string): Promise<void> {
  console.log(
    "\npre-pull funding blocker: an empty vault ATA must not be mistaken for missing coverage"
  );
  installPlan(directory, {
    dryRuns: ["funding_required"],
    executes: ["executed"],
  });

  const result = await dryRun();
  const coverage = readTopUpLookupTableCoverage(result);
  const readiness = await awaitTopUpLookupTableReadiness({
    dryRun: result,
    pollIntervalMs: 10_000,
    refreshDryRun: dryRun,
    sleep: noWait,
    timeoutMs: 240_000,
  });

  check(
    "route_funding_required blocker still counts as complete coverage",
    coverage.status === "ready",
    coverage
  );
  check("gate does not stall the normal first deposit", readiness.status === "ready");
  check("gate returns on the first dry run", readiness.attempts === 1, readiness.attempts);
}

async function scenarioIncidentRaceWaits(directory: string): Promise<void> {
  console.log("\nASK-2006 race: gate waits for the provisioner instead of pulling");
  installPlan(directory, {
    dryRuns: ["incomplete", "incomplete", "covered"],
    executes: ["executed"],
  });

  const readiness = await awaitTopUpLookupTableReadiness({
    dryRun: await dryRun(),
    pollIntervalMs: 10_000,
    refreshDryRun: dryRun,
    sleep: noWait,
    timeoutMs: 240_000,
  });

  check("gate becomes ready once coverage lands", readiness.status === "ready", readiness.status);
  check("gate polled until coverage was satisfied", readiness.attempts === 3, readiness.attempts);
  check(
    "no execute leg ran while coverage was incomplete",
    !readInvocations(directory).includes("execute"),
    readInvocations(directory)
  );
}

async function scenarioNeverReadyAborts(directory: string): Promise<void> {
  console.log("\nprovisioner stalled: gate times out rather than stranding funds");
  installPlan(directory, { dryRuns: ["incomplete"], executes: ["executed"] });

  const readiness = await awaitTopUpLookupTableReadiness({
    dryRun: await dryRun(),
    pollIntervalMs: 10_000,
    refreshDryRun: dryRun,
    sleep: noWait,
    timeoutMs: 25_000,
  });

  check("gate times out", readiness.status === "timed_out", readiness.status);
  check("timeout is bounded", readiness.attempts <= 4, readiness.attempts);
  check(
    "coverage is reported as incomplete, not unknown",
    readiness.coverage.status === "incomplete",
    readiness.coverage
  );
  check(
    "no execute leg ran",
    !readInvocations(directory).includes("execute"),
    readInvocations(directory)
  );
}

async function scenarioUnknownShapeDoesNotBlock(directory: string): Promise<void> {
  console.log("\nunreadable resolution: availability is preserved");
  installPlan(directory, { dryRuns: ["no_resolution"], executes: ["executed"] });

  const readiness = await awaitTopUpLookupTableReadiness({
    dryRun: await dryRun(),
    pollIntervalMs: 10_000,
    refreshDryRun: dryRun,
    sleep: noWait,
    timeoutMs: 25_000,
  });

  check("gate reports unknown", readiness.status === "unknown", readiness.status);
  check("gate does not block", readiness.attempts === 1, readiness.attempts);
  check(
    "reason is recorded for the evidence trail",
    readiness.coverage.status === "unknown" &&
      readiness.coverage.reason === "missing_lookup_table_resolution",
    readiness.coverage
  );
}

async function scenarioExecuteRetryRecovers(directory: string): Promise<void> {
  console.log("\nlate coverage lapse: the execute leg retries instead of failing");
  installPlan(directory, {
    dryRuns: ["covered"],
    executes: ["alt_coverage_error", "executed"],
  });

  const result = await runTopUpWithLookupTableRetry({
    attempt: executeRun,
    attempts: 3,
    delayMs: 20_000,
    sleep: noWait,
  });

  check(
    "retry produced an executed top-up",
    result.json?.status === "initial_deposit_executed",
    result.json?.status
  );
  check(
    "exactly two execute attempts were spawned",
    readInvocations(directory).filter((line) => line === "execute").length === 2,
    readInvocations(directory)
  );
}

async function scenarioRetryDoesNotMaskOtherFailures(
  directory: string
): Promise<void> {
  console.log("\nunrelated failure: retry must not swallow a confirm timeout");
  installPlan(directory, { dryRuns: ["covered"], executes: ["confirm_timeout"] });

  let caught: unknown;
  try {
    await runTopUpWithLookupTableRetry({
      attempt: executeRun,
      attempts: 3,
      delayMs: 20_000,
      sleep: noWait,
    });
  } catch (error) {
    caught = error;
  }

  check("the failure propagates", caught !== undefined);
  check(
    "it is not retried",
    readInvocations(directory).filter((line) => line === "execute").length === 1,
    readInvocations(directory)
  );
  check(
    "it is classified as a confirm timeout",
    classifyTopUpFailure(caught) === "confirm_timeout",
    classifyTopUpFailure(caught)
  );
}

function scenarioClassifiesProductionFailures(): void {
  console.log("\nclassification: real failure strings from the incident");
  check(
    "exec 9659 stderr classifies as alt_coverage_pending",
    classifyTopUpFailure(new Error(ALT_COVERAGE_STDERR)) === "alt_coverage_pending"
  );
  check(
    "exec 9660 stderr classifies as confirm_timeout",
    classifyTopUpFailure(new Error(CONFIRM_TIMEOUT_STDERR)) === "confirm_timeout"
  );
  check(
    "blockhash failures stay distinct",
    classifyTopUpFailure(
      new Error("initial reserve funding simulation failed: BlockhashNotFound")
    ) === "blockhash_not_found"
  );
  check(
    "lease races stay distinct",
    classifyTopUpFailure(
      new Error(
        "unexpected store state: lookup-table usage lease races with a nonterminal mutation operation"
      )
    ) === "lookup_table_lease_race"
  );
}

function scenarioGatePrecedesThePull(): void {
  console.log("\nordering: the gate runs before any user funds move");
  const source = readFileSync(
    join(import.meta.dir, "execute-autodeposit-policy.ts"),
    "utf8"
  );
  const gateIndex = source.indexOf("await awaitTopUpLookupTableReadiness({");
  const sendIndex = source.indexOf("sendPreparedOperation({");
  const recordIndex = source.indexOf("await recordPullExecution({");

  check("the gate is wired into the executor", gateIndex > 0, gateIndex);
  check(
    "the gate runs before the pull is sent",
    gateIndex > 0 && sendIndex > gateIndex,
    { gateIndex, sendIndex }
  );
  check(
    "the gate runs before the pull is recorded",
    gateIndex > 0 && recordIndex > gateIndex,
    { gateIndex, recordIndex }
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
const logPath = join(directory, "invocations.log");
const isExecute = process.argv.includes("--execute");
const previous = readFileSync(logPath, "utf8").split("\\n").filter(Boolean);
const kind = isExecute ? "execute" : "dry";
const index = previous.filter((line) => line === kind).length;
appendFileSync(logPath, kind + "\\n");

const modes = isExecute ? plan.executes : plan.dryRuns;
const mode = modes[Math.min(index, modes.length - 1)];

const coveredResolution = {
  mode: "active_reusable_resolver",
  cluster: "mainnet-beta",
  rollout: { mode: "reusable_only", forceLegacy: false },
  selection: { kind: "reusable", blocker: null, tableIds: [38, 34, 35] },
  reusable: {
    kind: "reusable",
    ready: true,
    packetFits: true,
    simulationSucceeded: true,
    simulationError: null,
    missingAddresses: [],
    tables: [{ tableId: 38 }, { tableId: 34 }, { tableId: 35 }],
    transaction: { packetSizeBytes: 483, fitsPacketDataSize: true },
  },
  sharedMarketCatalog: { state: "covered" },
  requirementsFingerprint: "645fed50d392aaa580b8dc4b4b9bd4467e42ece65896d44c143c60b2c06289c3",
};

const fundingRequiredResolution = {
  ...coveredResolution,
  selection: {
    kind: "blocked",
    blocker:
      "route_funding_required: exact route simulation failed: Transfer: insufficient funds",
    tableIds: [38, 34, 35],
  },
  reusable: {
    ...coveredResolution.reusable,
    ready: false,
    simulationSucceeded: false,
    simulationError: "Transfer: insufficient funds",
  },
};

const incompleteResolution = {
  ...coveredResolution,
  selection: {
    kind: "blocked",
    blocker:
      "reusable-only runtime requires complete reusable ALT coverage and simulation",
    tableIds: [],
  },
  reusable: {
    kind: "reusable",
    ready: false,
    packetFits: false,
    simulationSucceeded: false,
    simulationError: null,
    missingAddresses: [
      "5YjZj3dk61a8HNYK2jSUWGv3tBBzTp29sfPqewJbra9h",
      "9dcG2DMpy57UsEG39p1cDEcDgZgDV1ebJNpLTt17WwBi",
    ],
    tables: [],
    transaction: null,
  },
};

const resolutions = {
  covered: coveredResolution,
  funding_required: fundingRequiredResolution,
  incomplete: incompleteResolution,
};

if (!isExecute) {
  const body = {
    status: "initial_deposit_dry_run",
    writesDecision: false,
    sendsTransactions: false,
    wallet: {
      signer: "BAqgbERmvUViqDSx961xpRBHGt68SpACiWL4t9696qZZ",
      usdcAmountRaw: "870715",
      usdcAtaExists: true,
    },
    preflightBlockers: [],
    fundingTransaction: { simulationError: null },
    policyDeposit: { signer: "62JLkPeE4oG65LRB3W3m52RVicmYq3xFHdv7TecCsPj5" },
    policyDepositTransaction: { simulationError: null },
  };
  if (mode !== "no_resolution") {
    body.lookupTableResolution = resolutions[mode];
  }
  console.log(JSON.stringify(body, null, 2));
  process.exit(0);
}

if (mode === "alt_coverage_error") {
  console.error(
    JSON.stringify({
      error:
        "initial reserve deposit ALT coverage is incomplete before wallet funding: reusable lookup-table coverage is incomplete or the exact simulation failure is not the expected missing-token-account prerequisite",
      event: "same_mint_route_worker_fatal",
    })
  );
  process.exit(1);
}

if (mode === "confirm_timeout") {
  console.error(
    JSON.stringify({
      error:
        "unable to confirm transaction. This can happen in situations such as transaction expiration and insufficient fee-payer funds",
      event: "same_mint_route_worker_fatal",
    })
  );
  process.exit(1);
}

console.log(
  JSON.stringify(
    {
      status: "initial_deposit_executed",
      policyDepositTransaction: {
        signature:
          "3MxU4wwwYs9xtrJCWnZ2n4R1TAuLEkb9Af3v4ErL53syLgXGawBAxKP9i7RyqTyVtHNkeLZBxZsBmxLrDRm7anMc",
        confirmedSlot: "437209512",
        simulationError: null,
      },
      lookupTableResolution: coveredResolution,
    },
    null,
    2
  )
);
process.exit(0);
`;

async function main(): Promise<void> {
  const directory = mkdtempSync(join(tmpdir(), "autodeposit-alt-verify-"));
  const stubPath = writeStub(directory);
  process.env.VERIFY_STUB_DIR = directory;
  process.env.SAME_MINT_RESERVE_SWAP_COMMAND = `bun ${stubPath}`;
  process.env.POLICY_KEYPAIR ??= "verification-stub-no-signer";

  console.log(`autodeposit ALT readiness verification (ASK-2006)`);
  console.log(`isolated stub directory: ${directory}`);

  try {
    await scenarioSteadyStateStaysFast(directory);
    await scenarioFundingBlockerIsReady(directory);
    await scenarioIncidentRaceWaits(directory);
    await scenarioNeverReadyAborts(directory);
    await scenarioUnknownShapeDoesNotBlock(directory);
    await scenarioExecuteRetryRecovers(directory);
    await scenarioRetryDoesNotMaskOtherFailures(directory);
    scenarioClassifiesProductionFailures();
    scenarioGatePrecedesThePull();
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
