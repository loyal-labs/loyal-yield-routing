#!/usr/bin/env bun
/**
 * End-to-end verification for the ASK-2051 stale current-reserve reconciliation.
 *
 * Reproduces the production retry loop in an isolated world and proves the fix stops it.
 * The `same-mint-reserve-swap` binary is a stub in a throwaway directory that answers
 * with the real production JSON shapes, Neon is an in-memory projection plus pointer row
 * that records every statement, and there is no RPC, signer, or chain access.
 *
 * The production condition: `user_yield_positions.current_reserve` names a reserve the
 * vault no longer holds, so the deposit obligation for it does not exist, the policy plan
 * never builds, and the dry run returns `lookupTableResolution: null` with the real
 * reason sitting unread in `preflightBlockers`.
 */
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

import {
  assertNoTopUpPreflightBlockers,
  assertReconciliationPersisted,
  assertResolvedCurrentReserve,
  autodepositExecutorFailureExitCode,
  autodepositFailureDisposition,
  AUTODEPOSIT_FEE_PAYER_LOW_MARKER,
  readMissingDepositObligation,
  isDrainedVaultFailure,
  isFeePayerExhaustedFailure,
  isPullResolvedTopUpBlocker,
  isTopUpPreflightBlockedFailure,
  isUnresolvedCurrentReserveFailure,
  loadLiveVaultPositions,
  persistReconciledCurrentReserve,
  releaseAutodepositLotClaim,
  reportFeePayerBalance,
  readTopUpLookupTableCoverage,
  readTopUpPreflightBlockers,
  resolveCurrentReserve,
  runSameMintReserveTopUp,
  type EligibleTarget,
  type LiveVaultPosition,
} from "./execute-autodeposit-policy";

/** The reserve the stale pointer names; the vault has no obligation on its market. */
const STALE_RESERVE = "D6q6wuQSrifJKZYpR1M8R4YawnLDtDsMmWM1NbBmgJ59";
const STALE_MARKET = "7u3HeHxYDLhnCoErrtycNokbQYbWGzLs6JSDqGAv5PfF";
/** The reserve the vault actually holds after the rebalance. */
const LIVE_RESERVE = "AYL4LMc4ZCVyq3Z7XPJGWDM4H9PiWjqXAAuuHBEGVR2Z";
const LIVE_MARKET = "47tfyEG9SsdEnUm9cw5kY9BXngQGqu3LBoop9j5uTAv8";
const OTHER_RESERVE = "GTzdvEf7bAosdzCjgcA2Nxs3fMVMMK139P1SW3rYgqTs";
/** A route whose plan builds, but whose wallet-funding leg is unmet at dry-run time. */
const FUNDING_DEFERRED_RESERVE = "5cjp3WNfEGghXJx5rvqF1SNCxETq2Cr1CudFNiGpoqKy";
/** Verbatim from production after the fix deployed. */
const FUNDING_BLOCKER =
  "wallet USDC balance 870715 is below needed funding amount 1256712";
/** A route whose plan builds but whose funding-wallet ATA the pull will never create. */
const WALLET_ATA_MISSING_RESERVE = "GTzdvEf7bAosdzCjgcA2Nxs3fMVMMK139P1SW3rYgqTt";
const WALLET_ATA_BLOCKER =
  "wallet USDC ATA 8SoPTUZ4tcSXHJ3M4WMV6sMCAiAb26S2H51h34Pb24Ce does not exist for " +
  "BAqgbERmvUViqDSx961xpRBHGt68SpACiWL4t9696qZZ";
const USDC_MINT = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v";
const OTHER_MINT = "Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB";
const MISSING_OBLIGATION = "HfwZYDSnbmqCDj73j417ERft53uoTqw8djsEoaXYwhf3";
const DATABASE_URL = "postgres://verification-stub/none";
const PRODUCTION_STUCK_TARGET_COUNT = 11;
const PRODUCTION_TRIGGER_TICKS = 20;

/** Verbatim from the captured production dry run for target 4994. */
const PRODUCTION_BLOCKER =
  `deposit obligation ${MISSING_OBLIGATION} is missing for reserve ` +
  `${STALE_RESERVE}; run the missing-obligation setup transaction before policy deposit`;

const target = {
  id: BigInt(4994),
  settings: "6scgzFo55CS94QNmgmuNtZQUCLZKzsUUG8YT1PcMuW27",
  vaultIndex: 1,
  wallet: "5n8ovVj2rmErdgReeWihoeMVVqB2jAGx3GaYVbDk8PH1",
  walletUsdcAta: "8SoPTUZ4tcSXHJ3M4WMV6sMCAiAb26S2H51h34Pb24Ce",
  walletTokenAta: "8SoPTUZ4tcSXHJ3M4WMV6sMCAiAb26S2H51h34Pb24Ce",
  vaultPubkey: "5ZnT5CVJpd3SNFoXX9kFHY1Ur2HbK95QYX7B5wfhT1y5",
  vaultUsdcAta: "APrV6SXX5KxTdvtqfSqxsby8p2d9PATVoBKyc7EKar8f",
  vaultTokenAta: "APrV6SXX5KxTdvtqfSqxsby8p2d9PATVoBKyc7EKar8f",
  tokenMint: USDC_MINT,
  sweepPolicyAccount: "9dcG2DMpy57UsEG39p1cDEcDgZgDV1ebJNpLTt17WwBi",
  routePolicyId: BigInt(8867),
  routePolicyAccount: "EaV8NXzW3mG7nQqMtiD7mPFqvsifiemfdfvmTLeHqoGz",
  routePolicyLastSeenSlot: BigInt(437_168_327),
  routePolicySeed: BigInt(2),
  routeModes: ["same_mint_kamino"],
  recurringDelegation: "Cby9L7a9TttgXobyLmGZV8tutEV3B8kh2e6YVDF381TG",
  walletBalanceFloorRaw: BigInt(0),
  maxAmountPerPeriodRaw: null,
  periodLengthSeconds: null,
  startTimestamp: null,
  currentReserve: STALE_RESERVE,
  currentMarket: STALE_MARKET,
  currentLiquidityMint: USDC_MINT,
} satisfies EligibleTarget;

const failures: string[] = [];
let checks = 0;

function check(name: string, condition: boolean, detail?: unknown): void {
  checks += 1;
  if (condition) {
    console.log(`  ok   ${name}`);
    return;
  }
  const suffix =
    detail === undefined
      ? ""
      : ` -> ${JSON.stringify(detail, (_key, value) =>
          typeof value === "bigint" ? value.toString() : value
        )}`;
  console.log(`  FAIL ${name}${suffix}`);
  failures.push(`${name}${suffix}`);
}

function position(
  overrides: Partial<LiveVaultPosition> & Pick<LiveVaultPosition, "reserve">
): LiveVaultPosition {
  return {
    market: LIVE_MARKET,
    liquidityMint: USDC_MINT,
    amountRaw: BigInt(353_092_985),
    observedSlot: BigInt(437_579_499),
    observedAt: new Date(),
    ...overrides,
  };
}

type PointerRow = {
  id: string;
  current_reserve: string;
  current_market: string;
  current_liquidity_mint: string;
  status: string;
};

function createFakeNeon(state: {
  positions: LiveVaultPosition[];
  pointer: PointerRow;
}) {
  const statements: { sql: string; values: unknown[] }[] = [];
  const neon =
    () =>
    async (strings: TemplateStringsArray, ...values: unknown[]) => {
      const sql = strings.raw.join(" ? ").replace(/\s+/g, " ").trim();
      statements.push({ sql, values });

      if (/FROM loyal_yield\.vault_reserve_positions_current/i.test(sql)) {
        // The projection table holds a row per known reserve whether or not it carries a
        // balance, and the query no longer filters them out. Returning zero rows here is
        // what lets the drained-vault case be told apart from a silent projector.
        return state.positions.map((entry) => ({
          reserve: entry.reserve,
          market: entry.market,
          liquidity_mint: entry.liquidityMint,
          amount_raw: entry.amountRaw.toString(),
          observed_slot: entry.observedSlot?.toString() ?? null,
          observed_at: entry.observedAt?.toISOString() ?? null,
        }));
      }

      if (/UPDATE loyal_yield\.user_yield_positions/i.test(sql)) {
        const [reserve, market, liquidityMint] = values as string[];
        const guardedFrom = values[values.length - 1];
        if (
          state.pointer.status !== "active" ||
          state.pointer.current_reserve !== guardedFrom
        ) {
          return [];
        }
        state.pointer.current_reserve = reserve;
        state.pointer.current_market = market;
        state.pointer.current_liquidity_mint = liquidityMint;
        return [{ id: state.pointer.id }];
      }

      return [];
    };
  return { neon: neon as never, statements };
}

function updateStatements(
  statements: { sql: string; values: unknown[] }[]
): { sql: string; values: unknown[] }[] {
  return statements.filter((statement) =>
    /UPDATE loyal_yield\.user_yield_positions/i.test(statement.sql)
  );
}

function writeStub(directory: string): string {
  const stubPath = join(directory, "same-mint-reserve-swap-stub.ts");
  writeFileSync(stubPath, STUB_SOURCE, "utf8");
  return stubPath;
}

function installPlan(directory: string): void {
  writeFileSync(
    join(directory, "plan.json"),
    JSON.stringify({
      blockedReserve: STALE_RESERVE,
      blocker: PRODUCTION_BLOCKER,
      healthyReserve: LIVE_RESERVE,
      fundingDeferredReserve: FUNDING_DEFERRED_RESERVE,
      fundingBlocker: FUNDING_BLOCKER,
      walletAtaMissingReserve: WALLET_ATA_MISSING_RESERVE,
      walletAtaBlocker: WALLET_ATA_BLOCKER,
    }),
    "utf8"
  );
  writeFileSync(join(directory, "invocations.log"), "", "utf8");
}

function spawnCount(directory: string): number {
  return readFileSync(join(directory, "invocations.log"), "utf8")
    .split("\n")
    .filter(Boolean).length;
}

function dryRun(reserve: string, workerTarget: EligibleTarget = target) {
  return runSameMintReserveTopUp({
    amountRaw: BigInt(715_370),
    execute: false,
    reserve,
    rpcUrl: "https://verification.invalid",
    target: workerTarget,
  });
}

/**
 * One pass of the production loop, in the same order `main` runs it: resolve the
 * destination, then dry run, then gate on blockers before anything can pull.
 */
async function simulateTick(args: {
  neon: never;
  positions: LiveVaultPosition[];
  pointer: PointerRow;
  reconcile: boolean;
  execute: boolean;
}): Promise<{ ok: boolean; reserve: string | null; error?: unknown }> {
  const workerTarget: EligibleTarget = {
    ...target,
    currentReserve: args.pointer.current_reserve,
    currentMarket: args.pointer.current_market,
    currentLiquidityMint: args.pointer.current_liquidity_mint,
  };
  try {
    let reserve = workerTarget.currentReserve ?? STALE_RESERVE;
    if (args.reconcile) {
      const resolution = resolveCurrentReserve({
        positions: await loadLiveVaultPositions({
          databaseUrl: DATABASE_URL,
          neon: args.neon,
          target: workerTarget,
        }),
        target: workerTarget,
      });
      assertResolvedCurrentReserve(resolution);
      if (resolution.status === "reconciled") {
        reserve = resolution.to.reserve;
        if (args.execute) {
          await persistReconciledCurrentReserve({
            databaseUrl: DATABASE_URL,
            from: resolution.from,
            neon: args.neon,
            target: workerTarget,
            to: resolution.to,
          });
        }
      }
    }
    const result = await dryRun(reserve, workerTarget);
    assertNoTopUpPreflightBlockers(result);
    return { ok: true, reserve };
  } catch (error) {
    return { ok: false, reserve: null, error };
  }
}

async function scenarioProductionMisreportIsFixed(
  directory: string
): Promise<void> {
  console.log("\nthe blocked route is no longer reported as unknown ALT coverage");
  installPlan(directory);
  const blocked = await dryRun(STALE_RESERVE);
  blockedFixture = blocked;

  check(
    "the dry run reproduces production: exit 0, null resolution",
    blocked.exitCode === 0 && blocked.json?.lookupTableResolution === null,
    { exitCode: blocked.exitCode, resolution: blocked.json?.lookupTableResolution }
  );
  check(
    "the real reason is present in preflightBlockers",
    readTopUpPreflightBlockers(blocked).includes(PRODUCTION_BLOCKER),
    readTopUpPreflightBlockers(blocked)
  );

  const coverage = readTopUpLookupTableCoverage(blocked, STALE_RESERVE);
  check(
    "coverage is classified as a blocked route, not unknown coverage",
    coverage.status === "blocked" &&
      coverage.reason !== "missing_lookup_table_resolution" &&
      coverage.reason?.startsWith("route_") === true,
    coverage
  );
  check(
    "coverage carries the blocker text",
    coverage.blocker === PRODUCTION_BLOCKER,
    coverage.blocker
  );

  let gateError: unknown = null;
  try {
    assertNoTopUpPreflightBlockers(blocked);
  } catch (error) {
    gateError = error;
  }
  const message = gateError instanceof Error ? gateError.message : "";
  check("the preflight gate rejects the blocked route", gateError !== null);
  check(
    "the thrown error names the missing obligation",
    message.includes(MISSING_OBLIGATION) && message.includes(STALE_RESERVE),
    message.slice(0, 200)
  );
  check(
    "the failure is classified as preflight blocked",
    isTopUpPreflightBlockedFailure(gateError),
    message.slice(0, 120)
  );
}

async function scenarioUnreadableResolutionStillReportsUnknown(
  directory: string
): Promise<void> {
  console.log("\nan unreadable resolution with no blockers still reports unknown");
  installPlan(directory);
  const result = await dryRun("unreadable");
  const coverage = readTopUpLookupTableCoverage(result, "unreadable");
  check(
    "reason stays missing_lookup_table_resolution",
    coverage.status === "unknown" &&
      coverage.reason === "missing_lookup_table_resolution",
    coverage
  );
}

async function scenarioLoopReproduced(directory: string): Promise<void> {
  console.log("\nwithout reconciliation: the loop runs forever");
  installPlan(directory);
  const state = {
    positions: [position({ reserve: LIVE_RESERVE })],
    pointer: {
      id: "4016",
      current_reserve: STALE_RESERVE,
      current_market: STALE_MARKET,
      current_liquidity_mint: USDC_MINT,
      status: "active",
    },
  };
  const { neon, statements } = createFakeNeon(state);

  let failedTicks = 0;
  for (let tick = 0; tick < 10; tick += 1) {
    const outcome = await simulateTick({
      execute: true,
      neon,
      positions: state.positions,
      pointer: state.pointer,
      reconcile: false,
    });
    if (!outcome.ok) {
      failedTicks += 1;
    }
  }

  check("every tick fails", failedTicks === 10, failedTicks);
  check("every tick really ran the binary", spawnCount(directory) === 10);
  check(
    "the stale pointer is never corrected",
    state.pointer.current_reserve === STALE_RESERVE
  );
  check("nothing was written", updateStatements(statements).length === 0);
}

async function scenarioLoopStopped(directory: string): Promise<void> {
  console.log("\nwith reconciliation: the loop stops on the first tick");
  installPlan(directory);
  const state = {
    positions: [position({ reserve: LIVE_RESERVE })],
    pointer: {
      id: "4016",
      current_reserve: STALE_RESERVE,
      current_market: STALE_MARKET,
      current_liquidity_mint: USDC_MINT,
      status: "active",
    },
  };
  const { neon, statements } = createFakeNeon(state);

  const outcomes: { ok: boolean; reserve: string | null }[] = [];
  for (let tick = 0; tick < 10; tick += 1) {
    outcomes.push(
      await simulateTick({
        execute: true,
        neon,
        positions: state.positions,
        pointer: state.pointer,
        reconcile: true,
      })
    );
  }

  check(
    "every tick succeeds",
    outcomes.every((outcome) => outcome.ok),
    outcomes.filter((outcome) => !outcome.ok).length
  );
  check(
    "the deposit targets the reserve the vault actually holds",
    outcomes.every((outcome) => outcome.reserve === LIVE_RESERVE),
    outcomes[0]
  );
  check(
    "the pointer is corrected to chain truth",
    state.pointer.current_reserve === LIVE_RESERVE &&
      state.pointer.current_market === LIVE_MARKET,
    state.pointer
  );
  check(
    "exactly one correcting write was issued",
    updateStatements(statements).length === 1,
    updateStatements(statements).length
  );
  check(
    "the write is guarded on the stale value and an active row",
    /position\.status = 'active'/i.test(updateStatements(statements)[0]?.sql ?? "") &&
      updateStatements(statements)[0]?.values.includes(STALE_RESERVE),
    updateStatements(statements)[0]
  );
}

async function scenarioDryRunNeverMutates(directory: string): Promise<void> {
  console.log("\nsafety: a dry run resolves but never writes");
  installPlan(directory);
  const state = {
    positions: [position({ reserve: LIVE_RESERVE })],
    pointer: {
      id: "4016",
      current_reserve: STALE_RESERVE,
      current_market: STALE_MARKET,
      current_liquidity_mint: USDC_MINT,
      status: "active",
    },
  };
  const { neon, statements } = createFakeNeon(state);

  const outcome = await simulateTick({
    execute: false,
    neon,
    positions: state.positions,
    pointer: state.pointer,
    reconcile: true,
  });

  check("the dry run still reaches the live reserve", outcome.reserve === LIVE_RESERVE);
  check("no write was issued", updateStatements(statements).length === 0);
  check(
    "the pointer is untouched",
    state.pointer.current_reserve === STALE_RESERVE
  );
}

function scenarioGuardsNeverRedirect(): void {
  console.log("\nsafety: ambiguous or unverified observations never redirect funds");
  const stale = new Date(Date.now() - 3_600_000);

  const cases: {
    name: string;
    positions: LiveVaultPosition[];
    reason: string;
  }[] = [
    { name: "no live position", positions: [], reason: "no_live_position" },
    {
      name: "every reserve zero but never freshly observed",
      positions: [
        position({ reserve: LIVE_RESERVE, amountRaw: BigInt(0), observedAt: stale }),
      ],
      reason: "no_live_position",
    },
    {
      name: "two live positions",
      positions: [
        position({ reserve: LIVE_RESERVE }),
        position({ reserve: OTHER_RESERVE }),
      ],
      reason: "multiple_live_positions",
    },
    {
      name: "stale projection",
      positions: [position({ reserve: LIVE_RESERVE, observedAt: stale })],
      reason: "stale_projection",
    },
    {
      name: "unobserved projection",
      positions: [position({ reserve: LIVE_RESERVE, observedAt: null })],
      reason: "stale_projection",
    },
    {
      name: "different liquidity mint",
      positions: [
        position({ reserve: LIVE_RESERVE, liquidityMint: OTHER_MINT }),
      ],
      reason: "liquidity_mint_mismatch",
    },
  ];

  for (const testCase of cases) {
    const resolution = resolveCurrentReserve({
      positions: testCase.positions,
      target,
    });
    check(
      `${testCase.name} stays unresolved (${testCase.reason})`,
      resolution.status === "unresolved" && resolution.reason === testCase.reason,
      resolution
    );
    let thrown: unknown = null;
    try {
      assertResolvedCurrentReserve(resolution);
    } catch (error) {
      thrown = error;
    }
    check(`${testCase.name} refuses to pull`, thrown !== null);
    check(
      `${testCase.name} is classified as an unresolved reserve`,
      isUnresolvedCurrentReserveFailure(thrown),
      thrown instanceof Error ? thrown.message.slice(0, 120) : thrown
    );
  }
}

function scenarioHealthyTargetsAreUntouched(): void {
  console.log("\nsafety: healthy targets and first deposits are unaffected");

  const live = resolveCurrentReserve({
    positions: [
      position({ reserve: STALE_RESERVE, market: STALE_MARKET }),
      position({ reserve: LIVE_RESERVE }),
    ],
    target,
  });
  check(
    "a pointer backed by a live position is left alone",
    live.status === "unchanged" && live.reason === "current_reserve_is_live",
    live
  );

  const firstDeposit = resolveCurrentReserve({
    positions: [],
    target: { ...target, currentReserve: null },
  });
  check(
    "a target with no pointer falls through to the default earn target",
    firstDeposit.status === "unchanged" &&
      firstDeposit.reason === "no_current_reserve",
    firstDeposit
  );
}

/**
 * Regression guard for the production incident of 2026-08-06 14:11 UTC: the first fix
 * rejected every preflight blocker, including the wallet-funding leg that is unmet on
 * every autodeposit dry run by construction. Targets whose coverage was already `ready`
 * started failing as preflight_blocked.
 */
async function scenarioDeferrableBlockersStillProceed(
  directory: string
): Promise<void> {
  console.log("\nregression: a deferrable funding blocker must not stop the pull");
  installPlan(directory);
  const deferred = await dryRun(FUNDING_DEFERRED_RESERVE);

  check(
    "the dry run reports the funding blocker",
    readTopUpPreflightBlockers(deferred).includes(FUNDING_BLOCKER),
    readTopUpPreflightBlockers(deferred)
  );
  check(
    "but it still carries a built lookup-table resolution",
    deferred.json?.lookupTableResolution !== null &&
      deferred.json?.lookupTableResolution !== undefined
  );

  let thrown: unknown = null;
  try {
    assertNoTopUpPreflightBlockers(deferred);
  } catch (error) {
    thrown = error;
  }
  check(
    "the preflight gate lets it through",
    thrown === null,
    thrown instanceof Error ? thrown.message.slice(0, 200) : thrown
  );

  const coverage = readTopUpLookupTableCoverage(deferred, FUNDING_DEFERRED_RESERVE);
  check(
    "coverage still resolves as ready so the pull can proceed",
    coverage.status === "ready",
    coverage
  );
  // The pull funds the vault ATA, not the funding wallet's, so this blocker outlives it
  // and would fail the execute leg with the user's funds already in the vault.
  const walletAtaMissing = await dryRun(WALLET_ATA_MISSING_RESERVE);
  let walletAtaError: unknown = null;
  try {
    assertNoTopUpPreflightBlockers(walletAtaMissing);
  } catch (error) {
    walletAtaError = error;
  }
  const walletAtaMessage =
    walletAtaError instanceof Error ? walletAtaError.message : "";
  check(
    "a missing funding-wallet ATA is rejected before the pull",
    walletAtaError !== null,
    readTopUpPreflightBlockers(walletAtaMissing)
  );
  check(
    "it is rejected even though the resolution is built and coverage is ready",
    walletAtaMissing.json?.lookupTableResolution !== null &&
      readTopUpLookupTableCoverage(walletAtaMissing, WALLET_ATA_MISSING_RESERVE)
        .status === "ready",
    readTopUpLookupTableCoverage(walletAtaMissing, WALLET_ATA_MISSING_RESERVE)
  );
  check(
    "the error separates the blocker the pull cannot clear",
    walletAtaMessage.includes("unresolvableBlockers") &&
      walletAtaMessage.includes("does not exist for") &&
      !walletAtaMessage.includes('unresolvableBlockers=["wallet USDC balance'),
    walletAtaMessage.slice(0, 240)
  );
  check(
    "the missing-ATA failure is classified as preflight blocked",
    isTopUpPreflightBlockedFailure(walletAtaError)
  );
  check(
    "only the exact low-balance blocker is treated as pull-resolved",
    isPullResolvedTopUpBlocker(FUNDING_BLOCKER) &&
      !isPullResolvedTopUpBlocker(WALLET_ATA_BLOCKER) &&
      !isPullResolvedTopUpBlocker(PRODUCTION_BLOCKER) &&
      !isPullResolvedTopUpBlocker(
        `${FUNDING_BLOCKER} and wallet USDC ATA missing`
      )
  );
  check(
    "a plan that never built is still rejected",
    (() => {
      try {
        assertNoTopUpPreflightBlockers(blockedFixture!);
        return false;
      } catch {
        return true;
      }
    })(),
    "missing-obligation case must still throw"
  );
}

let blockedFixture: Awaited<ReturnType<typeof dryRun>> | null = null;

function scenarioMatchingRowIsValidated(): void {
  console.log("\nreview 3: a matching pointer row is validated, not trusted blindly");
  const stale = new Date(Date.now() - 3_600_000);

  const staleMatch = resolveCurrentReserve({
    positions: [
      position({
        reserve: STALE_RESERVE,
        market: STALE_MARKET,
        observedAt: stale,
      }),
    ],
    target,
  });
  check(
    "a stale matching row is reported as stale rather than silently trusted",
    staleMatch.status === "unchanged" &&
      staleMatch.reason === "current_reserve_is_live" &&
      staleMatch.projectionStale === true,
    staleMatch
  );
  check(
    "a stale matching row still does not redirect funds",
    staleMatch.status === "unchanged",
    staleMatch
  );

  const freshMatch = resolveCurrentReserve({
    positions: [position({ reserve: STALE_RESERVE, market: STALE_MARKET })],
    target,
  });
  check(
    "a fresh matching row is not flagged stale",
    freshMatch.status === "unchanged" && freshMatch.projectionStale === false,
    freshMatch
  );

  const wrongMintMatch = resolveCurrentReserve({
    positions: [
      position({
        reserve: STALE_RESERVE,
        market: STALE_MARKET,
        liquidityMint: OTHER_MINT,
      }),
    ],
    target,
  });
  check(
    "a matching row in the wrong mint is rejected",
    wrongMintMatch.status === "unresolved" &&
      wrongMintMatch.reason === "liquidity_mint_mismatch",
    wrongMintMatch
  );

  const dustPlusLive = resolveCurrentReserve({
    positions: [
      position({
        reserve: STALE_RESERVE,
        market: STALE_MARKET,
        amountRaw: BigInt(1),
      }),
      position({ reserve: LIVE_RESERVE }),
    ],
    target,
  });
  check(
    "a matching dust row keeps the pointer and reports the other live reserve",
    dustPlusLive.status === "unchanged" &&
      dustPlusLive.liveReserves?.includes(LIVE_RESERVE) === true,
    dustPlusLive
  );
}

async function scenarioPositionsAreScopedToVault(
  directory: string
): Promise<void> {
  console.log("\nreview 2: positions and writes are scoped to the target vault");
  installPlan(directory);
  const state = {
    positions: [position({ reserve: LIVE_RESERVE })],
    pointer: {
      id: "4016",
      current_reserve: STALE_RESERVE,
      current_market: STALE_MARKET,
      current_liquidity_mint: USDC_MINT,
      status: "active",
    },
  };
  const { neon, statements } = createFakeNeon(state);

  await loadLiveVaultPositions({
    databaseUrl: DATABASE_URL,
    neon,
    target,
  });
  const selectSql = statements[0]?.sql ?? "";
  check(
    "the position query binds the full vault identity",
    /vault\.settings = \?/i.test(selectSql) &&
      /vault\.vault_index = \?/i.test(selectSql) &&
      /vault\.vault_pubkey = \?/i.test(selectSql) &&
      /vault\.active/i.test(selectSql),
    selectSql
  );
  check(
    "the position query binds the target's vault pubkey value",
    statements[0]?.values.includes(target.vaultPubkey),
    statements[0]?.values
  );

  await persistReconciledCurrentReserve({
    databaseUrl: DATABASE_URL,
    from: STALE_RESERVE,
    neon,
    target,
    to: position({ reserve: LIVE_RESERVE }),
  });
  const update = updateStatements(statements)[0];
  check(
    "the pointer update binds the full position identity",
    /position\.vault_pubkey = \?/i.test(update?.sql ?? "") &&
      /position\.wallet_address = \?/i.test(update?.sql ?? "") &&
      /position\.status = 'active'/i.test(update?.sql ?? ""),
    update?.sql
  );
  check(
    "the pointer update binds the target's vault pubkey value",
    update?.values.includes(target.vaultPubkey),
    update?.values
  );
}

function scenarioLostRaceStopsTheAttempt(): void {
  console.log("\nreview 4: a lost compare-and-set stops the attempt");
  let thrown: unknown = null;
  try {
    assertReconciliationPersisted({
      from: STALE_RESERVE,
      persistedPositionIds: [],
      to: LIVE_RESERVE,
    });
  } catch (error) {
    thrown = error;
  }
  check("an empty compare-and-set result refuses to pull", thrown !== null);
  check(
    "the lost race is classified as an unresolved reserve",
    isUnresolvedCurrentReserveFailure(thrown),
    thrown instanceof Error ? thrown.message.slice(0, 140) : thrown
  );
  check(
    "the error names the lost race",
    thrown instanceof Error &&
      thrown.message.includes("lost_reconciliation_race"),
    thrown instanceof Error ? thrown.message.slice(0, 140) : thrown
  );

  let ok = true;
  try {
    assertReconciliationPersisted({
      from: STALE_RESERVE,
      persistedPositionIds: ["4016"],
      to: LIVE_RESERVE,
    });
  } catch {
    ok = false;
  }
  check("a single updated row proceeds", ok);
}

async function scenarioMissingObligationIsCountable(
  directory: string
): Promise<void> {
  console.log("\nreview 1: the recoverable missing-obligation case is separable");
  installPlan(directory);
  const blocked = await dryRun(STALE_RESERVE);

  const missing = readMissingDepositObligation(blocked);
  check(
    "the missing obligation and its reserve are extracted",
    missing?.obligation === MISSING_OBLIGATION &&
      missing?.reserve === STALE_RESERVE,
    missing
  );
  const coverage = readTopUpLookupTableCoverage(blocked, STALE_RESERVE);
  check(
    "coverage names it as a missing deposit obligation",
    coverage.reason === "route_deposit_obligation_missing",
    coverage.reason
  );

  let message = "";
  try {
    assertNoTopUpPreflightBlockers(blocked);
  } catch (error) {
    message = error instanceof Error ? error.message : String(error);
  }
  check(
    "the thrown error carries the structured missing obligation",
    message.includes("missingDepositObligation") &&
      message.includes(MISSING_OBLIGATION),
    message.slice(0, 200)
  );
}

/**
 * The condition target 4542 sat in for 700 consecutive attempts: the user withdrew
 * everything, so no reserve can be reconciled toward and no retry can ever succeed. The
 * refusal to pull is correct; retrying it every five minutes and paging each time is not.
 */
async function scenarioDrainedVaultBacksOffWithoutPaging(): Promise<void> {
  console.log("\nASK-2051 follow-up: a drained vault backs off instead of paging");

  const drained = resolveCurrentReserve({
    positions: [
      position({ reserve: STALE_RESERVE, market: STALE_MARKET, amountRaw: BigInt(0) }),
      position({ reserve: LIVE_RESERVE, amountRaw: BigInt(0) }),
    ],
    target,
  });
  check(
    "a freshly observed empty vault is classified as drained",
    drained.status === "unresolved" && drained.reason === "vault_drained",
    drained
  );

  let thrown: unknown = null;
  try {
    assertResolvedCurrentReserve(drained);
  } catch (error) {
    thrown = error;
  }
  check("a drained vault still refuses to pull", thrown !== null);
  check(
    "the drained vault is classified as not actionable",
    isDrainedVaultFailure(thrown),
    thrown instanceof Error ? thrown.message.slice(0, 160) : thrown
  );

  // The separation has to hold in both directions, or the quiet exit code would start
  // swallowing genuine faults that also stop before funds move.
  let missingProjection: unknown = null;
  try {
    assertResolvedCurrentReserve(
      resolveCurrentReserve({ positions: [], target })
    );
  } catch (error) {
    missingProjection = error;
  }
  check(
    "a silent projector is not treated as a drained vault",
    isUnresolvedCurrentReserveFailure(missingProjection) &&
      !isDrainedVaultFailure(missingProjection),
    missingProjection instanceof Error
      ? missingProjection.message.slice(0, 160)
      : missingProjection
  );
  // The loader returns every mint the vault touches, so drain evidence has to name the
  // target's own mint. A fresh row for a different mint vouching for a missing USDC
  // projection would silence a real projector failure as a drained vault.
  const foreignMintOnly = resolveCurrentReserve({
    positions: [
      position({
        amountRaw: BigInt(0),
        liquidityMint: OTHER_MINT,
        reserve: OTHER_RESERVE,
      }),
    ],
    target,
  });
  check(
    "a fresh zero row for another mint does not prove this vault is drained",
    foreignMintOnly.status === "unresolved" &&
      foreignMintOnly.reason === "no_live_position",
    foreignMintOnly
  );
  const staleTargetMint = resolveCurrentReserve({
    positions: [
      position({
        amountRaw: BigInt(0),
        observedAt: new Date(Date.now() - 3_600_000),
        reserve: STALE_RESERVE,
      }),
      position({
        amountRaw: BigInt(0),
        liquidityMint: OTHER_MINT,
        reserve: OTHER_RESERVE,
      }),
    ],
    target,
  });
  check(
    "a stale target-mint row is not rescued by a fresh foreign-mint row",
    staleTargetMint.status === "unresolved" &&
      staleTargetMint.reason === "no_live_position",
    staleTargetMint
  );

  check(
    "an ambiguous vault is not treated as a drained vault",
    !isDrainedVaultFailure(
      new Error(
        'Autodeposit target reserve could not be resolved against chain truth; ' +
          'refusing to pull. resolution={"reason":"multiple_live_positions"}'
      )
    )
  );
  check(
    "a top-up preflight block is not treated as a drained vault",
    !isDrainedVaultFailure(new Error(PRODUCTION_BLOCKER))
  );

  check(
    "the drained vault reports the quiet exit code, not the blocked-route one",
    autodepositExecutorFailureExitCode("not_actionable", {
      AUTODEPOSIT_NOT_ACTIONABLE_EXIT_CODE: "23",
      AUTODEPOSIT_PREFLIGHT_BLOCKED_EXIT_CODE: "22",
    }) === 23
  );

  // The exit code and the retry cadence are decided together, so the pairing itself is
  // asserted rather than only its two halves.
  const drainedDisposition = autodepositFailureDisposition(thrown);
  check(
    "a drained vault exits quietly and waits six hours",
    drainedDisposition.failureCode === "not_actionable" &&
      drainedDisposition.retryDelaySeconds === 6 * 60 * 60,
    drainedDisposition
  );
  const blockedDisposition = autodepositFailureDisposition(
    new Error(
      "Kamino top-up dry run reported preflight blockers; refusing to pull. " +
        `blockers=${JSON.stringify([PRODUCTION_BLOCKER])}`
    )
  );
  check(
    "a blocked route still pages and keeps the five-minute cadence",
    blockedDisposition.failureCode === "preflight_blocked" &&
      blockedDisposition.retryDelaySeconds === 5 * 60,
    blockedDisposition
  );
  const unclassifiedDisposition = autodepositFailureDisposition(
    new Error("BlockhashNotFound")
  );
  check(
    "an unclassified failure keeps the generic exit and the fast cadence",
    unclassifiedDisposition.failureCode === null &&
      unclassifiedDisposition.retryDelaySeconds === 5 * 60,
    unclassifiedDisposition
  );

  const state = {
    positions: [],
    pointer: {
      id: "4542",
      current_reserve: STALE_RESERVE,
      current_market: STALE_MARKET,
      current_liquidity_mint: USDC_MINT,
      status: "active",
    },
  };
  const { neon, statements } = createFakeNeon(state);
  await releaseAutodepositLotClaim({
    claimToken: "autodeposit-trigger:4542:234781:1",
    databaseUrl: DATABASE_URL,
    lastError: thrown instanceof Error ? thrown.message : String(thrown),
    neon,
    pauseTargetForMissingDelegate: false,
    retryDelaySeconds: 6 * 60 * 60,
  });
  const release = statements.at(-1);
  check(
    "the release backs the lots off by the long delay",
    release?.values.includes(6 * 60 * 60) === true,
    release?.values
  );
  check(
    "the backoff is bound into eligible_after rather than hard-coded",
    /eligible_after = now\(\) \+ \( \? \* interval '1 second'\)/i.test(
      release?.sql ?? ""
    ),
    release?.sql
  );
  // Only the slot deadline gates the next attempt, so delaying the lot alone would let
  // the replacement slot fire on its old, short deadline and make the backoff arrive a
  // full cycle late.
  check(
    "the replacement scheduled slot is pushed out by the same delay",
    /UPDATE loyal_yield\.balance_sweep_scheduled_slots AS slot/i.test(
      release?.sql ?? ""
    ) &&
      /SET eligible_after = GREATEST\(/i.test(release?.sql ?? "") &&
      /slot\.status IN \('scheduled', 'requested'\)/i.test(release?.sql ?? ""),
    release?.sql
  );

  const { neon: defaultNeon, statements: defaultStatements } =
    createFakeNeon(state);
  await releaseAutodepositLotClaim({
    claimToken: "autodeposit-trigger:4542:234782:1",
    databaseUrl: DATABASE_URL,
    lastError: "BlockhashNotFound",
    neon: defaultNeon,
    pauseTargetForMissingDelegate: false,
  });
  check(
    "an ordinary failure keeps the five-minute retry cadence",
    defaultStatements.at(-1)?.values.includes(5 * 60) === true,
    defaultStatements.at(-1)?.values
  );
}

/**
 * The 2026-08-07 outage: the shared signer ran out of SOL and every target stopped at
 * once, but the alert read "executor exited unsuccessfully" — the same bucket as any
 * unclassified fault. The one failure an operator can clear in two minutes was the one
 * the alert did not name.
 */
function scenarioFeePayerExhaustionIsNamed(): void {
  console.log("\n2026-08-07 outage: an empty fee payer names its own remedy");

  // Verbatim from production.
  const pullFailure = new Error(
    "Autodeposit pull fee payer 62JLkPeE4oG65LRB3W3m52RVicmYq3xFHdv7TecCsPj5 " +
      "has 1004469 lamports; 50000000 required."
  );
  const topUpFailure = new Error(
    "Kamino top-up fee payer 62JLkPeE4oG65LRB3W3m52RVicmYq3xFHdv7TecCsPj5 " +
      "has 1004469 lamports; 50000000 required. Refusing to pull user funds."
  );

  for (const [name, failure] of [
    ["the pull guard", pullFailure],
    ["the top-up guard", topUpFailure],
  ] as const) {
    check(
      `${name} is recognised as an exhausted fee payer`,
      isFeePayerExhaustedFailure(failure)
    );
    const disposition = autodepositFailureDisposition(failure);
    check(
      `${name} reports the fee-payer code and keeps the fast retry`,
      disposition.failureCode === "fee_payer_exhausted" &&
        disposition.retryDelaySeconds === 5 * 60,
      disposition
    );
  }

  check(
    "it no longer falls into the generic unclassified bucket",
    autodepositFailureDisposition(pullFailure).failureCode !== null
  );
  check(
    "the exhausted fee payer is not mistaken for a drained vault",
    !isDrainedVaultFailure(pullFailure)
  );
  check(
    "an unrelated funds error is not classified as an exhausted fee payer",
    !isFeePayerExhaustedFailure(
      new Error("Transfer: insufficient lamports 100, need 200")
    ) &&
      !isFeePayerExhaustedFailure(
        new Error("wallet USDC balance 870715 is below needed funding amount 1256712")
      )
  );
  check(
    "the exit code is distinct from every other classified failure",
    autodepositExecutorFailureExitCode("fee_payer_exhausted", {
      AUTODEPOSIT_FEE_PAYER_EXHAUSTED_EXIT_CODE: "24",
    }) === 24
  );

  // The warning has to fire while the signer still works, or it is just a second way of
  // learning the fleet already stopped.
  const warned: string[] = [];
  const originalWarn = console.warn;
  console.warn = (line: string) => warned.push(line);
  try {
    const low = reportFeePayerBalance({
      balanceLamports: 60_000_000,
      feePayer: "62JLkPeE4oG65LRB3W3m52RVicmYq3xFHdv7TecCsPj5",
      lowLamports: 150_000_000,
      minimumLamports: 50_000_000,
      role: "Autodeposit pull fee payer",
    });
    const healthy = reportFeePayerBalance({
      balanceLamports: 400_000_000,
      feePayer: "62JLkPeE4oG65LRB3W3m52RVicmYq3xFHdv7TecCsPj5",
      lowLamports: 150_000_000,
      minimumLamports: 50_000_000,
      role: "Autodeposit pull fee payer",
    });
    check("a signer above the floor but running low still warns", low);
    check("a healthy signer stays silent", !healthy && warned.length === 1);
    check(
      "the warning carries the balance and the remaining headroom",
      warned[0]?.includes(AUTODEPOSIT_FEE_PAYER_LOW_MARKER) === true &&
        warned[0]?.includes('"balanceLamports":60000000') === true &&
        warned[0]?.includes('"remainingTransactions":1') === true,
      warned[0]
    );
  } finally {
    console.warn = originalWarn;
  }
}

function scenarioExitCodeIsDistinct(): void {
  console.log("\nthe blocked route reports its own exit code");
  check(
    "preflight_blocked maps to the trigger's configured code",
    autodepositExecutorFailureExitCode("preflight_blocked", {
      AUTODEPOSIT_PREFLIGHT_BLOCKED_EXIT_CODE: "22",
    }) === 22
  );
  check(
    "it does not collide with the top-up or persistence codes",
    autodepositExecutorFailureExitCode("kamino_top_up_failed", {
      AUTODEPOSIT_KAMINO_TOP_UP_FAILED_EXIT_CODE: "20",
    }) === 20 &&
      autodepositExecutorFailureExitCode("yield_persistence_failed", {
        AUTODEPOSIT_YIELD_PERSISTENCE_FAILED_EXIT_CODE: "21",
      }) === 21
  );
  check(
    "an unset code falls back to the generic failure",
    autodepositExecutorFailureExitCode("preflight_blocked", {}) === 1
  );
  check(
    "unrelated errors are not classified as blocked or unresolved",
    !isTopUpPreflightBlockedFailure(new Error("BlockhashNotFound")) &&
      !isUnresolvedCurrentReserveFailure(new Error("BlockhashNotFound"))
  );
}

async function scenarioProductionShapedLoad(directory: string): Promise<void> {
  console.log("\nproduction-shaped load: 11 stuck targets over 20 trigger ticks");
  installPlan(directory);
  const before = spawnCount(directory);

  let reconciledTargets = 0;
  let totalFailures = 0;
  for (let index = 0; index < PRODUCTION_STUCK_TARGET_COUNT; index += 1) {
    const state = {
      positions: [position({ reserve: LIVE_RESERVE })],
      pointer: {
        id: `pointer-${index}`,
        current_reserve: STALE_RESERVE,
        current_market: STALE_MARKET,
        current_liquidity_mint: USDC_MINT,
        status: "active",
      },
    };
    const { neon } = createFakeNeon(state);
    for (let tick = 0; tick < PRODUCTION_TRIGGER_TICKS; tick += 1) {
      const outcome = await simulateTick({
        execute: true,
        neon,
        positions: state.positions,
        pointer: state.pointer,
        reconcile: true,
      });
      if (!outcome.ok) {
        totalFailures += 1;
      }
    }
    if (state.pointer.current_reserve === LIVE_RESERVE) {
      reconciledTargets += 1;
    }
  }

  check(
    "every stuck target is reconciled",
    reconciledTargets === PRODUCTION_STUCK_TARGET_COUNT,
    reconciledTargets
  );
  check("no tick fails after reconciliation", totalFailures === 0, totalFailures);
  check(
    "each spawn now targets the live reserve",
    spawnCount(directory) - before ===
      PRODUCTION_STUCK_TARGET_COUNT * PRODUCTION_TRIGGER_TICKS,
    spawnCount(directory) - before
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
const reserve = argValue("--deposit-reserve");

// The vault has no obligation on the stale reserve's market, so the policy plan never
// builds: the binary emits the blockers and a null lookup-table resolution, and exits 0.
if (reserve === plan.blockedReserve) {
  console.log(
    JSON.stringify({
      status: "initial_deposit_dry_run",
      sendsTransactions: false,
      preflightBlockers: [plan.blocker],
      policyDeposit: null,
      policyDepositTransaction: null,
      lookupTableResolution: null,
    })
  );
  process.exit(0);
}

// The plan builds and the resolution is present, but the borrowed initial-deposit mode
// reports its wallet-funding leg as unmet because the pull has not run yet. Production
// treats this as funding_deferred and proceeds.
// The plan builds, but the funding wallet has no USDC ATA. The pull funds the vault ATA
// and never this one, so the blocker survives into execute mode, which rejects it after
// the user's funds have already moved.
if (reserve === plan.walletAtaMissingReserve) {
  console.log(
    JSON.stringify({
      status: "initial_deposit_dry_run",
      sendsTransactions: false,
      preflightBlockers: [plan.walletAtaBlocker, plan.fundingBlocker],
      policyDeposit: { signer: "BAqgbERmvUViqDSx961xpRBHGt68SpACiWL4t9696qZZ" },
      policyDepositTransaction: {
        feePayer: "BAqgbERmvUViqDSx961xpRBHGt68SpACiWL4t9696qZZ",
        simulationError: null,
      },
      lookupTableResolution: {
        reusable: {
          ready: true,
          missingAddresses: [],
          packetFits: true,
          tables: [{ tableId: 140 }],
          transaction: { packetSizeBytes: 435 },
          simulationError: null,
        },
        rollout: { mode: "reusable_only", forceLegacy: false },
        selection: { blocker: "route_funding_required:vault_usdc" },
        sharedMarketCatalog: { state: "covered" },
      },
    })
  );
  process.exit(0);
}

if (reserve === plan.fundingDeferredReserve) {
  console.log(
    JSON.stringify({
      status: "initial_deposit_dry_run",
      sendsTransactions: false,
      preflightBlockers: [plan.fundingBlocker],
      policyDeposit: { signer: "BAqgbERmvUViqDSx961xpRBHGt68SpACiWL4t9696qZZ" },
      policyDepositTransaction: {
        feePayer: "BAqgbERmvUViqDSx961xpRBHGt68SpACiWL4t9696qZZ",
        simulationError: null,
      },
      lookupTableResolution: {
        reusable: {
          ready: true,
          missingAddresses: [],
          packetFits: true,
          tables: [{ tableId: 140 }],
          transaction: { packetSizeBytes: 435 },
          simulationError: null,
        },
        rollout: { mode: "reusable_only", forceLegacy: false },
        selection: { blocker: "route_funding_required:vault_usdc" },
        sharedMarketCatalog: { state: "covered" },
      },
    })
  );
  process.exit(0);
}

// An unreadable resolution with no blockers: the genuinely unknown case.
if (reserve !== plan.healthyReserve) {
  console.log(
    JSON.stringify({
      status: "initial_deposit_dry_run",
      sendsTransactions: false,
      preflightBlockers: [],
      lookupTableResolution: null,
    })
  );
  process.exit(0);
}

console.log(
  JSON.stringify({
    status: "initial_deposit_dry_run",
    sendsTransactions: false,
    preflightBlockers: [],
    policyDeposit: { signer: "BAqgbERmvUViqDSx961xpRBHGt68SpACiWL4t9696qZZ" },
    policyDepositTransaction: {
      feePayer: "BAqgbERmvUViqDSx961xpRBHGt68SpACiWL4t9696qZZ",
      simulationError: null,
    },
    lookupTableResolution: {
      reusable: {
        ready: true,
        missingAddresses: [],
        packetFits: true,
        tables: [{ tableId: 140 }],
        transaction: { packetSizeBytes: 435 },
        simulationError: null,
      },
      rollout: { mode: "reusable_only", forceLegacy: false },
      selection: { blocker: null },
      sharedMarketCatalog: { state: "covered" },
    },
  })
);
process.exit(0);
`;

async function main(): Promise<void> {
  const directory = mkdtempSync(join(tmpdir(), "autodeposit-reserve-verify-"));
  const stubPath = writeStub(directory);
  process.env.VERIFY_STUB_DIR = directory;
  process.env.SAME_MINT_RESERVE_SWAP_COMMAND = `bun ${stubPath}`;
  process.env.POLICY_KEYPAIR ??= "verification-stub-no-signer";

  console.log("autodeposit stale current-reserve verification (ASK-2051)");
  console.log(`isolated stub directory: ${directory}`);

  try {
    await scenarioProductionMisreportIsFixed(directory);
    await scenarioUnreadableResolutionStillReportsUnknown(directory);
    await scenarioLoopReproduced(directory);
    await scenarioLoopStopped(directory);
    await scenarioDryRunNeverMutates(directory);
    scenarioGuardsNeverRedirect();
    scenarioHealthyTargetsAreUntouched();
    await scenarioDeferrableBlockersStillProceed(directory);
    scenarioMatchingRowIsValidated();
    await scenarioPositionsAreScopedToVault(directory);
    scenarioLostRaceStopsTheAttempt();
    await scenarioMissingObligationIsCountable(directory);
    await scenarioDrainedVaultBacksOffWithoutPaging();
    scenarioFeePayerExhaustionIsNamed();
    scenarioExitCodeIsDistinct();
    await scenarioProductionShapedLoad(directory);
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
