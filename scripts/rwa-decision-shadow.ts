import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import {
  buildLeverageGrid,
  decideRwa,
  type CostQuote,
  type CurrentPosition,
  type DecisionPolicy,
  type NavPoint,
  type ReserveEvidence,
  type RwaDecisionInput,
  type StrategyDefinition,
} from "./rwa-decision";

const DEFAULT_POLICY: DecisionPolicy = {
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

// Keep five days of causal observations before the first decision. The
// forecaster may use up to seven days, but never requires future samples.
const BACKTEST_WARMUP_HOURS = 120;
const HOURS_PER_YEAR = 365.25 * 24;

type HistoryRow = Record<string, unknown> & {
  account_data_hash: string;
  dedupe_key: string;
  liquidity_mint: string;
  market: string;
  mint_decimals: number;
  observed_at: string;
  reserve: string;
  snapshot: Record<string, unknown>;
};

type PositionState = {
  strategyKey: string;
  leverageBps: number;
  openedAt: string;
  collateralUnits: number;
  debtUnits: number;
};

function snake(value: string): string {
  return value.replace(/([a-z0-9])([A-Z])/g, "$1_$2").toLowerCase();
}

function field(body: string, name: string): string {
  const match = body.match(new RegExp(`${name}:\\s*([A-Z][A-Z0-9_]*)`));
  if (!match) throw new Error(`missing ${name} in strategy template`);
  return match[1]!;
}

export function loadCanonicalStrategies(root: string): StrategyDefinition[] {
  const actions = readFileSync(resolve(root, "crates/loyal-actions/src/earn_max.rs"), "utf8");
  const config = readFileSync(resolve(root, "crates/loyal-fleet-worker/src/multiply/config.rs"), "utf8");
  const constants = new Map<string, string>();
  for (const match of actions.matchAll(/pub const (EARN_MAX_[A-Z0-9_]+): &str =\s*"([1-9A-HJ-NP-Za-km-z]{32,44})";/g)) {
    constants.set(match[1]!, match[2]!);
  }
  const aliases = new Map<string, string>();
  const importBlock = config.match(/use loyal_actions::\{([\s\S]*?)\};/)?.[1] ?? "";
  for (const token of importBlock.split(",").map((value) => value.trim()).filter(Boolean)) {
    const match = token.match(/^(EARN_MAX_[A-Z0-9_]+)(?:\s+as\s+([A-Z0-9_]+))?$/);
    if (match) aliases.set(match[2] ?? match[1]!, match[1]!);
  }
  const resolveReserve = (identifier: string): string => {
    const canonical = aliases.get(identifier) ?? identifier;
    const value = constants.get(canonical);
    if (!value) throw new Error(`unresolved canonical reserve ${identifier}`);
    return value;
  };
  const strategies: StrategyDefinition[] = [];
  for (const match of config.matchAll(/const [A-Z0-9_]+_TEMPLATE: StrategyTemplate = StrategyTemplate \{([\s\S]*?)\n\};/g)) {
    const body = match[1]!;
    const key = body.match(/key:\s*StrategyKey::([A-Za-z0-9_]+)/)?.[1];
    const target = body.match(/target_ltv_bps:\s*([0-9_]+)/)?.[1];
    if (!key || !target) throw new Error("incomplete canonical strategy template");
    strategies.push({
      key: snake(key),
      collateralReserve: resolveReserve(field(body, "collateral_reserve")),
      debtReserve: resolveReserve(field(body, "debt_reserve")),
      policyTargetLtvBps: Number(target.replaceAll("_", "")),
    });
  }
  strategies.sort((left, right) => left.key.localeCompare(right.key));
  if (strategies.length !== 7 || new Set(strategies.map((strategy) => strategy.key)).size !== 7) {
    throw new Error(`canonical strategy catalog must contain exactly seven unique rows, found ${strategies.length}`);
  }
  return strategies;
}

function numeric(value: unknown): number | null {
  const parsed = Number(value);
  return Number.isFinite(parsed) ? parsed : null;
}

function rawUsd(raw: unknown, decimals: unknown, price: unknown): number | null {
  const values = [numeric(raw), numeric(decimals), numeric(price)];
  if (values.some((value) => value === null)) return null;
  return values[0]! / 10 ** values[1]! * values[2]!;
}

function historyEvidence(row: HistoryRow): ReserveEvidence {
  const snapshot = row.snapshot;
  const metrics = (snapshot.api_metrics ?? {}) as Record<string, unknown>;
  const price = numeric(row.market_price_usd);
  const decimals = numeric(row.mint_decimals);
  const liquidationPct = numeric(snapshot.liquidation_threshold_pct);
  return {
    reserve: row.reserve,
    observedAt: row.observed_at,
    evidenceId: row.dedupe_key,
    schema: "kamino_api_history_v1",
    active: snapshot.reserve_status_text === "Active",
    emergencyMode: null,
    priceUsd: price,
    supplyApy: numeric(row.supply_apy),
    borrowApy: numeric(row.borrow_apy),
    availableLiquidityUsd: numeric(row.available_amount) !== null && price !== null
      ? numeric(row.available_amount)! * price
      : null,
    totalSupplyUsd: numeric(row.total_supply_usd_estimate),
    totalBorrowUsd: numeric(row.total_borrow_usd_estimate),
    depositLimitUsd: rawUsd(snapshot.deposit_limit, decimals, price),
    borrowLimitUsd: rawUsd(snapshot.borrow_limit, decimals, price),
    borrowLimitOutsideGroupUsd: rawUsd(snapshot.borrow_limit_outside_elevation_group, decimals, price),
    borrowedOutsideGroupUsd: rawUsd(snapshot.borrowed_amount_outside_elevation_group, decimals, price),
    debtWithdrawalHeadroomUsd: null,
    liquidationThresholdBps: liquidationPct === null ? null : Math.round(liquidationPct * 100),
  };
}

function candidateId(strategyKey: string, leverageBps: number): string {
  return `${strategyKey}@${leverageBps}`;
}

function modeledCost(
  strategy: StrategyDefinition,
  leverageBps: number,
  equityUsd: number,
  observedAt: string,
  fromCandidateId: string | null,
): CostQuote {
  const leverage = leverageBps / 10_000;
  const grossCollateral = equityUsd * leverage;
  const debt = equityUsd * (leverage - 1);
  const sourceLeverageBps = fromCandidateId === null
    ? null
    : Number(fromCandidateId.slice(fromCandidateId.lastIndexOf("@") + 1));
  if (sourceLeverageBps !== null && (!Number.isFinite(sourceLeverageBps) || sourceLeverageBps < 10_000)) {
    throw new Error(`invalid source candidate ${fromCandidateId}`);
  }
  return {
    candidateId: candidateId(strategy.key, leverageBps),
    fromCandidateId,
    amountUsd: equityUsd,
    observedAt,
    available: true,
    entryUsd: grossCollateral * 0.001,
    exitUsd: sourceLeverageBps === null ? 0 : equityUsd * (sourceLeverageBps / 10_000) * 0.001,
    flashUsd: debt * 0.00001,
    jupiterUsd: grossCollateral * 0.0005,
    fixedUsd: 0.06,
  };
}

function modeledExitCost(candidate: string, equityUsd: number): number {
  const leverageBps = Number(candidate.slice(candidate.lastIndexOf("@") + 1));
  if (!Number.isFinite(leverageBps) || leverageBps < 10_000) throw new Error(`invalid exit candidate ${candidate}`);
  return equityUsd * (leverageBps / 10_000) * 0.001 + 0.03;
}

function quoteTotal(quote: CostQuote): number {
  return quote.entryUsd + quote.exitUsd + quote.flashUsd + quote.jupiterUsd + quote.fixedUsd;
}

function allQuotes(
  strategies: StrategyDefinition[],
  equityUsd: number,
  observedAt: string,
  fromCandidateId: string | null,
  policy: DecisionPolicy,
): CostQuote[] {
  return strategies.flatMap((strategy) =>
    buildLeverageGrid(strategy.policyTargetLtvBps, policy.leverageStepBps)
      .map((leverage) => modeledCost(strategy, leverage, equityUsd, observedAt, fromCandidateId)),
  );
}

function parseHistory(path: string): { rows: HistoryRow[]; digest: string } {
  const rows = readFileSync(path, "utf8").split(/\r?\n/).filter(Boolean).map((line) => JSON.parse(line) as HistoryRow);
  const keys = rows.map((row) => row.dedupe_key).sort();
  return { rows, digest: createHash("md5").update(keys.join("\n")).digest("hex") };
}

function price(row: HistoryRow): number {
  const value = numeric(row.market_price_usd);
  if (value === null || value <= 0) throw new Error(`invalid price for ${row.reserve} at ${row.observed_at}`);
  return value;
}

function apyGrowth(apy: unknown, hours: number): number {
  const value = numeric(apy);
  if (value === null || value <= -1) throw new Error(`invalid APY ${String(apy)}`);
  return Math.pow(1 + value, hours / HOURS_PER_YEAR);
}

function markEquity(
  state: PositionState,
  strategy: StrategyDefinition,
  rows: Map<string, HistoryRow>,
): number {
  return state.collateralUnits * price(rows.get(strategy.collateralReserve)!)
    - state.debtUnits * price(rows.get(strategy.debtReserve)!);
}

function openPosition(
  strategy: StrategyDefinition,
  leverageBps: number,
  equityUsd: number,
  timestamp: string,
  rows: Map<string, HistoryRow>,
): PositionState {
  const leverage = leverageBps / 10_000;
  return {
    strategyKey: strategy.key,
    leverageBps,
    openedAt: timestamp,
    collateralUnits: equityUsd * leverage / price(rows.get(strategy.collateralReserve)!),
    debtUnits: equityUsd * (leverage - 1) / price(rows.get(strategy.debtReserve)!),
  };
}

function advancePosition(
  state: PositionState,
  strategy: StrategyDefinition,
  currentRows: Map<string, HistoryRow>,
  nextRows: Map<string, HistoryRow>,
  elapsedHours: number,
): { state: PositionState; equityUsd: number } {
  const collateral = currentRows.get(strategy.collateralReserve)!;
  const debt = currentRows.get(strategy.debtReserve)!;
  const advanced = {
    ...state,
    collateralUnits: state.collateralUnits * apyGrowth(collateral.supply_apy, elapsedHours),
    debtUnits: state.debtUnits * apyGrowth(debt.borrow_apy, elapsedHours),
  };
  return { state: advanced, equityUsd: markEquity(advanced, strategy, nextRows) };
}

function positionLtvBps(state: PositionState, strategy: StrategyDefinition, rows: Map<string, HistoryRow>): number {
  const collateral = state.collateralUnits * price(rows.get(strategy.collateralReserve)!);
  const debt = state.debtUnits * price(rows.get(strategy.debtReserve)!);
  return collateral <= 0 ? 10_000 : Math.round(debt / collateral * 10_000);
}

function staticBenchmark(
  strategy: StrategyDefinition,
  timestamps: string[],
  byTime: Map<string, Map<string, HistoryRow>>,
  initialEquityUsd: number,
  policy: DecisionPolicy,
): number {
  const leverage = buildLeverageGrid(strategy.policyTargetLtvBps, policy.leverageStepBps).at(-1)!;
  let equity = initialEquityUsd;
  const opening = modeledCost(strategy, leverage, equity, timestamps[0]!, null);
  equity -= quoteTotal(opening);
  let state = openPosition(strategy, leverage, equity, timestamps[0]!, byTime.get(timestamps[0]!)!);
  for (let index = 0; index < timestamps.length - 1; index += 1) {
    const current = timestamps[index]!;
    const next = timestamps[index + 1]!;
    const advanced = advancePosition(state, strategy, byTime.get(current)!, byTime.get(next)!, (Date.parse(next) - Date.parse(current)) / 3_600_000);
    state = advanced.state;
    equity = advanced.equityUsd;
  }
  return equity;
}

function perfectForesight(
  strategies: StrategyDefinition[],
  timestamps: string[],
  byTime: Map<string, Map<string, HistoryRow>>,
  initialEquityUsd: number,
  policy: DecisionPolicy,
): number {
  const candidates = strategies.flatMap((strategy) =>
    buildLeverageGrid(strategy.policyTargetLtvBps, policy.leverageStepBps)
      .map((leverageBps) => ({ strategy, leverageBps, id: candidateId(strategy.key, leverageBps) })),
  );
  let values = new Map<string, number>([["idle", initialEquityUsd]]);
  for (let index = 0; index < timestamps.length - 1; index += 1) {
    const current = timestamps[index]!;
    const next = timestamps[index + 1]!;
    const currentRows = byTime.get(current)!;
    const nextRows = byTime.get(next)!;
    const elapsedHours = (Date.parse(next) - Date.parse(current)) / 3_600_000;
    const nextValues = new Map<string, number>();
    let idleValue = values.get("idle") ?? Number.NEGATIVE_INFINITY;
    for (const [source, sourceValue] of values) {
      if (source !== "idle") idleValue = Math.max(idleValue, sourceValue - modeledExitCost(source, sourceValue));
    }
    nextValues.set("idle", idleValue);
    for (const destination of candidates) {
      let best = Number.NEGATIVE_INFINITY;
      for (const [source, sourceValue] of values) {
        const cost = source === destination.id
          ? 0
          : quoteTotal(modeledCost(destination.strategy, destination.leverageBps, sourceValue, current, source === "idle" ? null : source));
        const deployable = sourceValue - cost;
        if (deployable <= 0) continue;
        const state = openPosition(destination.strategy, destination.leverageBps, deployable, current, currentRows);
        const advanced = advancePosition(state, destination.strategy, currentRows, nextRows, elapsedHours);
        best = Math.max(best, advanced.equityUsd);
      }
      nextValues.set(destination.id, best);
    }
    values = nextValues;
  }
  return Math.max(...values.values());
}

export async function runWalkForwardBacktest(
  path: string,
  options: { initialEquityUsd?: number; policy?: Partial<DecisionPolicy> } = {},
): Promise<Record<string, unknown>> {
  const root = resolve(import.meta.dir, "..");
  const strategies = loadCanonicalStrategies(root);
  const policy = { ...DEFAULT_POLICY, ...options.policy };
  const initialEquityUsd = options.initialEquityUsd ?? 1_000;
  const { rows, digest } = parseHistory(path);
  const byTime = new Map<string, Map<string, HistoryRow>>();
  const historyByReserve = new Map<string, NavPoint[]>();
  for (const row of rows) {
    const at = byTime.get(row.observed_at) ?? new Map<string, HistoryRow>();
    at.set(row.reserve, row);
    byTime.set(row.observed_at, at);
    const nav = historyByReserve.get(row.reserve) ?? [];
    nav.push({ reserve: row.reserve, observedAt: row.observed_at, priceUsd: price(row) });
    historyByReserve.set(row.reserve, nav);
  }
  const allTimestamps = [...byTime.keys()].sort();
  const firstDecisionAt = Date.parse(allTimestamps[0]!) + Math.min(policy.forecastWindowHours, BACKTEST_WARMUP_HOURS) * 3_600_000;
  const timestamps = allTimestamps.filter((timestamp) => Date.parse(timestamp) >= firstDecisionAt);
  if (timestamps.length < 2) throw new Error("insufficient walk-forward history");
  let equity = initialEquityUsd;
  let position: PositionState | null = null;
  let accumulatedPnl = 0;
  let switchingCosts = 0;
  let switches = 0;
  let unsafeDecisionCount = 0;
  let explainedTurns = 0;
  let peak = equity;
  let maxDrawdown = 0;
  const actions: Record<string, number> = {};

  for (let index = 0; index < timestamps.length - 1; index += 1) {
    const timestamp = timestamps[index]!;
    const nextTimestamp = timestamps[index + 1]!;
    const currentRows = byTime.get(timestamp)!;
    const currentStrategy = position ? strategies.find((strategy) => strategy.key === position!.strategyKey)! : null;
    if (position && currentStrategy) equity = markEquity(position, currentStrategy, currentRows);
    const currentPosition: CurrentPosition | null = position && currentStrategy ? {
      strategyKey: position.strategyKey,
      leverageBps: position.leverageBps,
      currentLtvBps: positionLtvBps(position, currentStrategy, currentRows),
      openedAt: position.openedAt,
    } : null;
    const fromId = currentPosition ? candidateId(currentPosition.strategyKey, currentPosition.leverageBps) : null;
    const evidence = [...currentRows.values()].map(historyEvidence);
    const navHistory = [...historyByReserve.values()].flatMap((points) => points.filter((point) => point.observedAt <= timestamp));
    const input: RwaDecisionInput = {
      mode: "historical", asOf: timestamp, equityUsd: equity, strategies, reserves: evidence, navHistory,
      currentPosition, costQuotes: allQuotes(strategies, equity, timestamp, fromId, policy),
      currentExitCostUsd: position ? modeledExitCost(fromId!, equity) : null, policy,
    };
    const decision = decideRwa(input);
    actions[decision.action] = (actions[decision.action] ?? 0) + 1;
    const expectedCount = strategies.reduce((sum, strategy) => sum + buildLeverageGrid(strategy.policyTargetLtvBps, policy.leverageStepBps).length, 0);
    if (decision.rankedCandidates.length === expectedCount && decision.rankedCandidates.every((candidate) => candidate.reasonCodes.length > 0)) explainedTurns += 1;
    if (decision.chosen && (!decision.chosen.eligible || decision.chosen.projectedLtvBps > decision.chosen.liquidationThresholdBps! - policy.liquidationBufferBps || decision.chosen.capacityEquityUsd! + 1e-6 < equity)) unsafeDecisionCount += 1;

    let transitionCost = 0;
    if (["open", "switch", "delever"].includes(decision.action) && decision.chosen) {
      transitionCost = decision.chosen.transitionCostUsd ?? 0;
      equity -= transitionCost;
      switchingCosts += transitionCost;
      accumulatedPnl -= transitionCost;
      if (decision.action === "switch") switches += 1;
      const chosenStrategy = strategies.find((strategy) => strategy.key === decision.chosen!.strategyKey)!;
      position = openPosition(chosenStrategy, decision.chosen.leverageBps, equity, timestamp, currentRows);
    } else if (decision.action === "exit" && position) {
      transitionCost = input.currentExitCostUsd ?? 0;
      equity -= transitionCost;
      switchingCosts += transitionCost;
      accumulatedPnl -= transitionCost;
      position = null;
    }

    if (position) {
      const strategy = strategies.find((candidate) => candidate.key === position!.strategyKey)!;
      const before = equity;
      const advanced = advancePosition(position, strategy, currentRows, byTime.get(nextTimestamp)!, (Date.parse(nextTimestamp) - Date.parse(timestamp)) / 3_600_000);
      position = advanced.state;
      equity = advanced.equityUsd;
      accumulatedPnl += equity - before;
    }
    peak = Math.max(peak, equity);
    if (peak > 0) maxDrawdown = Math.max(maxDrawdown, (peak - equity) / peak);
  }

  const defaultKeyMatch = readFileSync(resolve(root, "crates/loyal-fleet-worker/src/multiply/planner.rs"), "utf8")
    .match(/RouteGoal::Deploy => up\(observed, StrategyKey::([A-Za-z0-9_]+), topology\)/)?.[1];
  if (!defaultKeyMatch) throw new Error("configured static strategy missing from planner owner");
  const defaultStrategy = strategies.find((strategy) => strategy.key === snake(defaultKeyMatch));
  if (!defaultStrategy) throw new Error("configured static strategy not in canonical catalog");
  const configuredStatic = staticBenchmark(defaultStrategy, timestamps, byTime, initialEquityUsd, policy);
  const oracle = perfectForesight(strategies, timestamps, byTime, initialEquityUsd, policy);
  const endingEquity = equity;
  return {
    artifactKind: "rwaShadowWalkForward",
    schemaVersion: 1,
    authorization: "shadow_only",
    evidenceSource: "kamino_api_history",
    sourceDigest: digest,
    sourceRows: rows.length,
    reserveCount: new Set(rows.map((row) => row.reserve)).size,
    turns: timestamps.length - 1,
    causal: true,
    initialEquityUsd,
    endingEquityUsd: endingEquity,
    netEarningsUsd: endingEquity - initialEquityUsd,
    switchingCostsUsd: switchingCosts,
    switches,
    actions,
    maxDrawdownFraction: maxDrawdown,
    unsafeDecisionCount,
    explanationCoverage: explainedTurns / (timestamps.length - 1),
    accountingDifferenceUsd: endingEquity - initialEquityUsd - accumulatedPnl,
    idleEndingEquityUsd: initialEquityUsd,
    configuredStaticStrategy: defaultStrategy.key,
    configuredStaticEndingEquityUsd: configuredStatic,
    perfectForesightEndingEquityUsd: oracle,
    regretUsd: oracle - endingEquity,
    window: { start: timestamps[0], end: timestamps.at(-1) },
  };
}

function usage(): never {
  console.log(`Usage:
  bun scripts/rwa-decision-shadow.ts --scenario <input.json>
  bun scripts/rwa-decision-shadow.ts --backtest <history.jsonl> [--equity-usd 1000]

Both modes are read-only and every result has authorization: "shadow_only".`);
  process.exit(0);
}

function argument(name: string): string | undefined {
  const index = Bun.argv.indexOf(name);
  return index >= 0 ? Bun.argv[index + 1] : undefined;
}

if (import.meta.main) {
  if (Bun.argv.includes("--help") || Bun.argv.includes("-h")) usage();
  const scenario = argument("--scenario");
  const backtest = argument("--backtest");
  if ((scenario ? 1 : 0) + (backtest ? 1 : 0) !== 1) usage();
  if (scenario) {
    const input = await Bun.file(scenario).json() as RwaDecisionInput;
    console.log(JSON.stringify(decideRwa(input), null, 2));
  } else {
    const equity = Number(argument("--equity-usd") ?? "1000");
    console.log(JSON.stringify(await runWalkForwardBacktest(resolve(backtest!), { initialEquityUsd: equity }), null, 2));
  }
}
