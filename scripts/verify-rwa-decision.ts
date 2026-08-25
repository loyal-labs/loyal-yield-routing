import { createHash } from "node:crypto";
import { existsSync, readFileSync, readdirSync } from "node:fs";
import { resolve } from "node:path";

const PASS = "PASS_RWA_DECISION_V1";
const FAIL = "FAIL_RWA_DECISION_V1";
const BLOCKED = "BLOCKED_RWA_DECISION_V1";
const ROOT = resolve(import.meta.dir, "..");
const ENGINE_PATH = "scripts/rwa-decision.ts";
const SHADOW_PATH = "scripts/rwa-decision-shadow.ts";
const SOURCE_PATH = process.env.RWA_BACKFILL_DIR
  ? resolve(process.env.RWA_BACKFILL_DIR, "history.jsonl")
  : "/private/tmp/rwa-observation-backfill-v1/history.jsonl";
const SOURCE_DIGEST = "3724d2d2fd74650e28feab7a085ccf83";

type Json = Record<string, unknown>;
const checks: Array<{ check: string; evidence: unknown }> = [];

function emit(verdict: string, condition: string, evidence: Json, code: number): never {
  console.log(JSON.stringify({ verdict, condition, evidence, checks }, null, 2));
  console.log(`${verdict} ${condition}`);
  process.exit(code);
}
function fail(condition: string, evidence: Json = {}): never { return emit(FAIL, condition, evidence, 2); }
function blocked(condition: string, evidence: Json = {}): never { return emit(BLOCKED, condition, evidence, 2); }
function check(condition: unknown, name: string, evidence: unknown = {}): asserts condition {
  if (!condition) fail(name, { evidence });
  checks.push({ check: name, evidence });
}
function file(path: string): string {
  const absolute = resolve(ROOT, path);
  if (!existsSync(absolute)) fail("required_source_missing", { path });
  return readFileSync(absolute, "utf8");
}
function near(actual: number, expected: number, name: string, epsilon = 1e-8): void {
  check(Number.isFinite(actual) && Math.abs(actual - expected) <= epsilon, name, { actual, expected, epsilon });
}
async function command(argv: string[], external = false): Promise<string> {
  const child = Bun.spawn(argv, { cwd: ROOT, env: process.env, stdout: "pipe", stderr: "pipe" });
  const [code, stdout, stderr] = await Promise.all([child.exited, new Response(child.stdout).text(), new Response(child.stderr).text()]);
  if (code !== 0) {
    const evidence = { command: argv.join(" "), code, stderrTail: stderr.split(/\r?\n/).slice(-12).join("\n") };
    if (external || stdout.includes(BLOCKED)) blocked("external_dependency_unavailable", evidence);
    fail("local_command_failed", evidence);
  }
  return stdout.trim();
}

function staticContract(): void {
  const pkg = JSON.parse(file("package.json")) as { scripts?: Record<string, string> };
  check(pkg.scripts?.["verify:rwa-decision-v1"] === "bun scripts/verify-rwa-decision.ts", "verifier_entrypoint", pkg.scripts?.["verify:rwa-decision-v1"]);
  check(pkg.scripts?.["rwa:decision-shadow"] === "bun scripts/rwa-decision-shadow.ts", "shadow_entrypoint", pkg.scripts?.["rwa:decision-shadow"]);
  const physical = readdirSync(resolve(ROOT, "scripts")).filter((name) => /^(verify-)?rwa-decision(?:-shadow)?\.ts$/.test(name)).sort();
  check(JSON.stringify(physical) === JSON.stringify(["rwa-decision-shadow.ts", "rwa-decision.ts", "verify-rwa-decision.ts"]), "exact_physical_surface", physical);
  const engine = file(ENGINE_PATH);
  const shadow = file(SHADOW_PATH);
  for (const token of ["Bun.spawn", "fetch(", "TIMESCALEDB_URL", "INSERT INTO", "UPDATE ", "DELETE FROM", "sendTransaction", "VersionedTransaction", "Keypair"])
    check(!engine.includes(token), "pure_engine_forbidden_dependency", { token });
  for (const token of ["INSERT INTO", "UPDATE ", "DELETE FROM", "sendTransaction", "VersionedTransaction", "Keypair", "render deploy", "--execute"])
    check(!shadow.includes(token), "shadow_write_surface_forbidden", { token });
  for (const token of ["authorization: \"shadow_only\"", "kamino_api_history", "loadCanonicalStrategies", "runWalkForwardBacktest"])
    check(shadow.includes(token), "shadow_contract_missing", { token });
  check(shadow.includes("point.observedAt <= timestamp"), "walk_forward_causal_cutoff_missing");
  check(shadow.includes('sourceValue - modeledExitCost(source, sourceValue)'), "oracle_exit_cost_missing");
  check(!shadow.includes('nextValues.set("idle", Math.max(...values.values()))'), "oracle_free_idle_transition_forbidden");
}

const policy = {
  freshnessSeconds: 600,
  quoteFreshnessSeconds: 300,
  forecastWindowHours: 168,
  minForecastSpanHours: 24,
  horizonHours: 168,
  leverageStepBps: 2_500,
  liquidationBufferBps: 1_000,
  cooldownHours: 24,
  minOpenNetUsd: 0,
  minSwitchEdgeUsd: 0.25,
  forecastApyFloor: -0.5,
  forecastApyCeiling: 0.5,
};

function reserve(overrides: Json = {}): Json {
  return {
    reserve: "collateral",
    observedAt: "2026-08-24T00:00:00.000Z",
    evidenceId: "event:1",
    schema: "live_v2",
    active: true,
    emergencyMode: false,
    priceUsd: 1,
    supplyApy: 0,
    borrowApy: 0.04,
    availableLiquidityUsd: 1_000_000,
    totalSupplyUsd: 100_000,
    totalBorrowUsd: 20_000,
    depositLimitUsd: 2_000_000,
    borrowLimitUsd: 1_000_000,
    borrowLimitOutsideGroupUsd: 1_000_000,
    borrowedOutsideGroupUsd: 20_000,
    debtWithdrawalHeadroomUsd: 1_000_000,
    liquidationThresholdBps: 8_000,
    ...overrides,
  };
}

function strategy(key: string, collateral = "collateral", debt = "debt", targetLtvBps = 5_000): Json {
  return { key, collateralReserve: collateral, debtReserve: debt, policyTargetLtvBps: targetLtvBps };
}

function nav(reserveKey: string, growth = 0.005): Json[] {
  return [
    { reserve: reserveKey, observedAt: "2026-08-17T00:00:00.000Z", priceUsd: 1 },
    { reserve: reserveKey, observedAt: "2026-08-24T00:00:00.000Z", priceUsd: 1 + growth },
  ];
}

function quotes(candidateIds: string[], fromCandidateId: string | null = null, cost = 0.01): Json[] {
  return candidateIds.map((candidateId) => ({
    candidateId, fromCandidateId, amountUsd: 1_000, observedAt: "2026-08-24T00:00:00.000Z",
    available: true, entryUsd: cost, exitUsd: cost, flashUsd: 0, jupiterUsd: 0, fixedUsd: 0,
  }));
}

function baseInput(): Json {
  const ids = ["alpha@10000", "alpha@12500", "alpha@15000", "alpha@17500", "alpha@20000"];
  return {
    mode: "live",
    asOf: "2026-08-24T00:00:00.000Z",
    equityUsd: 1_000,
    strategies: [strategy("alpha")],
    reserves: [reserve(), reserve({ reserve: "debt", evidenceId: "event:2" })],
    navHistory: nav("collateral"),
    currentPosition: null,
    costQuotes: quotes(ids),
    currentExitCostUsd: null,
    policy,
  };
}

async function implementationContract(): Promise<{ engine: Json; shadow: Json }> {
  const engine = await import(new URL("./rwa-decision.ts", import.meta.url).href) as Json;
  const shadow = await import(new URL("./rwa-decision-shadow.ts", import.meta.url).href) as Json;
  for (const name of ["decideRwa", "buildLeverageGrid", "estimateTrailingNavApy"])
    check(typeof engine[name] === "function", "engine_export_missing", { name });
  for (const name of ["loadCanonicalStrategies", "runWalkForwardBacktest"])
    check(typeof shadow[name] === "function", "shadow_export_missing", { name });
  return { engine, shadow };
}

function candidateIds(engine: Json, input: Json): string[] {
  return (engine.buildLeverageGrid as Function)(5_000, policy.leverageStepBps).map((bps: number) => `alpha@${bps}`);
}

function decisionFixtures(engine: Json): void {
  const decide = engine.decideRwa as Function;
  const input = baseInput();
  check(JSON.stringify(candidateIds(engine, input)) === JSON.stringify(["alpha@10000", "alpha@12500", "alpha@15000", "alpha@17500", "alpha@20000"]), "configured_leverage_grid", candidateIds(engine, input));
  const positive = decide(input);
  check(positive.action === "open" && positive.chosen?.leverageBps === 20_000, "positive_spread_chooses_highest_feasible", positive);
  const navApy = Math.pow(1.005, 365.25 / 7) - 1;
  const expectedGrossUsd = 1_000 * (2 * navApy - 0.04) * 168 / (365.25 * 24);
  near(positive.chosen.expectedGrossUsd, expectedGrossUsd, "independent_candidate_accounting");
  check(positive.authorization === "shadow_only" && positive.schemaVersion === 1, "shadow_versioned_result", positive);
  check(positive.rankedCandidates.length === 5 && positive.rankedCandidates.every((row: Json) => Array.isArray(row.reasonCodes) && row.reasonCodes.length > 0), "complete_candidate_explanations", { count: positive.rankedCandidates.length });
  check(new Set(positive.evidenceIds).size === 2, "evidence_identity_complete", positive.evidenceIds);

  const repeated = decide(structuredClone(input));
  check(JSON.stringify(repeated) === JSON.stringify(positive), "deterministic_result", { hash: createHash("sha256").update(JSON.stringify(positive)).digest("hex") });
  const future = structuredClone(input);
  (future.navHistory as Json[]).push({ reserve: "collateral", observedAt: "2026-08-25T00:00:00.000Z", priceUsd: 100 });
  check(JSON.stringify(decide(future)) === JSON.stringify(positive), "causal_no_lookahead", { asOf: input.asOf });

  const stale = structuredClone(input);
  (stale.reserves as Json[])[0]!.observedAt = "2026-08-23T00:00:00.000Z";
  check(decide(stale).action === "defer", "stale_live_evidence_defers", decide(stale));
  const missingQuote = structuredClone(input);
  missingQuote.costQuotes = [];
  check(decide(missingQuote).action === "defer", "missing_quotes_fail_closed", decide(missingQuote));
  const capacity = structuredClone(input);
  (capacity.reserves as Json[])[1]!.availableLiquidityUsd = 1;
  const capacityDecision = decide(capacity);
  check(capacityDecision.chosen?.leverageBps === 10_000, "capacity_bounds_leverage", capacityDecision);

  const negative = structuredClone(input);
  negative.navHistory = nav("collateral", 0);
  (negative.reserves as Json[])[0]!.supplyApy = 0;
  (negative.reserves as Json[])[1]!.borrowApy = 0.2;
  check(decide(negative).action === "defer", "negative_net_does_not_open", decide(negative));

  const currentId = "alpha@20000";
  const current = structuredClone(input);
  current.currentPosition = { strategyKey: "alpha", leverageBps: 20_000, currentLtvBps: 5_000, openedAt: "2026-08-20T00:00:00.000Z" };
  current.costQuotes = quotes(candidateIds(engine, current), currentId, 0.01);
  const emergency = structuredClone(current);
  (emergency.reserves as Json[])[0]!.emergencyMode = true;
  emergency.currentExitCostUsd = 0.1;
  check(decide(emergency).action === "exit", "known_emergency_exits", decide(emergency));
  const unsafe = structuredClone(current);
  (unsafe.reserves as Json[])[0]!.liquidationThresholdBps = 6_000;
  check(decide(unsafe).action === "delever" && decide(unsafe).chosen?.leverageBps < 20_000, "unsafe_position_delevers", decide(unsafe));

  const betaInput = structuredClone(current);
  betaInput.strategies = [strategy("alpha"), strategy("beta", "beta_collateral", "beta_debt")];
  betaInput.reserves.push(reserve({ reserve: "beta_collateral", evidenceId: "event:3", supplyApy: 0.25 }));
  betaInput.reserves.push(reserve({ reserve: "beta_debt", evidenceId: "event:4", borrowApy: 0.01 }));
  betaInput.navHistory = [...nav("collateral", 0.001), ...nav("beta_collateral", 0.006)];
  const betaIds = candidateIds(engine, betaInput).map((id) => id.replace("alpha", "beta"));
  betaInput.costQuotes = [...quotes(candidateIds(engine, betaInput), currentId, 0.01), ...quotes(betaIds, currentId, 0.01)];
  check(decide(betaInput).action === "switch" && decide(betaInput).chosen?.strategyKey === "beta", "positive_cost_adjusted_switch", decide(betaInput));
  const expensive = structuredClone(betaInput);
  expensive.costQuotes = [...quotes(candidateIds(engine, expensive), currentId, 0.01), ...quotes(betaIds, currentId, 500)];
  check(decide(expensive).action === "hold", "switch_cost_hysteresis_holds", decide(expensive));
}

async function catalogAndBacktest(shadow: Json): Promise<Json> {
  const strategies = await (shadow.loadCanonicalStrategies as Function)(ROOT);
  check(strategies.length === 7 && new Set(strategies.map((row: Json) => row.key)).size === 7, "exact_strategy_catalog", strategies);
  const reserves = new Set(strategies.flatMap((row: Json) => [row.collateralReserve, row.debtReserve]));
  check(reserves.size === 10, "exact_reserve_catalog", [...reserves]);
  check(strategies.filter((row: Json) => String(row.key).includes("usds")).length === 2, "approved_usds_lanes_only", strategies);
  check(strategies.filter((row: Json) => String(row.key).includes("pyusd")).length === 2, "approved_pyusd_lanes_only", strategies);
  if (!existsSync(SOURCE_PATH)) blocked("task1b_source_missing", { sourcePath: SOURCE_PATH });
  const report = await (shadow.runWalkForwardBacktest as Function)(SOURCE_PATH, { initialEquityUsd: 1_000 });
  check(report.sourceDigest === SOURCE_DIGEST && report.sourceRows === 14_440 && report.reserveCount === 10, "fixed_task1b_source", report);
  check(report.turns > 1_300 && report.causal === true && report.unsafeDecisionCount === 0, "causal_safe_walk_forward", report);
  check(report.explanationCoverage === 1 && Math.abs(report.accountingDifferenceUsd) < 1e-6, "walk_forward_reconciles", report);
  const actionCount = Object.values(report.actions as Record<string, number>).reduce((sum, count) => sum + count, 0);
  check(actionCount === report.turns, "walk_forward_action_count_reconciles", { actionCount, turns: report.turns });
  near(report.netEarningsUsd as number, (report.endingEquityUsd as number) - 1_000, "independent_net_earnings_reconciliation", 1e-6);
  near(report.regretUsd as number, (report.perfectForesightEndingEquityUsd as number) - (report.endingEquityUsd as number), "independent_regret_reconciliation", 1e-6);
  near(report.switchingCostsUsd as number, 1.56, "fixed_corpus_opening_cost", 1e-8);
  for (const field of ["endingEquityUsd", "netEarningsUsd", "switchingCostsUsd", "maxDrawdownFraction", "idleEndingEquityUsd", "configuredStaticEndingEquityUsd", "perfectForesightEndingEquityUsd", "regretUsd"])
    check(Number.isFinite(report[field]), "backtest_metric_missing", { field, value: report[field] });
  check(report.perfectForesightEndingEquityUsd + 1e-8 >= report.endingEquityUsd, "perfect_foresight_is_upper_bound", report);
  return report;
}

async function main(): Promise<void> {
  staticContract();
  await command(["bun", SHADOW_PATH, "--help"]);
  const { engine, shadow } = await implementationContract();
  decisionFixtures(engine);
  const backtest = await catalogAndBacktest(shadow);
  const dirty = await command(["git", "status", "--porcelain"]);
  check(dirty === "", "release_worktree_clean", { dirty });
  const [head, origin] = await Promise.all([command(["git", "rev-parse", "HEAD"]), command(["git", "rev-parse", "origin/main"])]);
  check(head === origin, "release_revision_is_origin_main", { head, origin });
  const task1 = await command(["bun", "run", "verify:rwa-observation-backfill-v1"], true);
  check(task1.includes("PASS_RWA_OBSERVATION_BACKFILL_V1"), "task1b_live_dependency", { outputHash: createHash("sha256").update(task1).digest("hex") });
  emit(PASS, "shadow_decision_layer_causal_safe_and_reconciled", {
    revision: head,
    strategyCount: 7,
    reserveCount: 10,
    backtest,
    proofBoundaries: { staticInspection: true, localSimulation: true, historicalReplay: true, liveReadOnly: true, submission: false, finalization: false, deployment: false },
  }, 0);
}

await main();
