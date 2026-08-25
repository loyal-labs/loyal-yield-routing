export const RWA_DECISION_SCHEMA_VERSION = 1;
export const RWA_DECISION_RULESET_VERSION = "rwa-shadow-causal-v1";
const HOURS_PER_YEAR = 365.25 * 24;
const LEVERAGE_DENOMINATOR = 10_000;

export type EvidenceMode = "live" | "historical";
export type DecisionAction = "open" | "hold" | "switch" | "delever" | "exit" | "defer";

export type StrategyDefinition = {
  key: string;
  collateralReserve: string;
  debtReserve: string;
  policyTargetLtvBps: number;
};

export type ReserveEvidence = {
  reserve: string;
  observedAt: string;
  evidenceId: string;
  schema: "live_v2" | "kamino_api_history_v1";
  active: boolean;
  emergencyMode: boolean | null;
  priceUsd: number | null;
  supplyApy: number | null;
  borrowApy: number | null;
  availableLiquidityUsd: number | null;
  totalSupplyUsd: number | null;
  totalBorrowUsd: number | null;
  depositLimitUsd: number | null;
  borrowLimitUsd: number | null;
  borrowLimitOutsideGroupUsd: number | null;
  borrowedOutsideGroupUsd: number | null;
  debtWithdrawalHeadroomUsd: number | null;
  liquidationThresholdBps: number | null;
};

export type NavPoint = { reserve: string; observedAt: string; priceUsd: number };

export type CurrentPosition = {
  strategyKey: string;
  leverageBps: number;
  currentLtvBps: number;
  openedAt: string;
};

export type CostQuote = {
  candidateId: string;
  fromCandidateId: string | null;
  amountUsd: number;
  observedAt: string;
  available: boolean;
  entryUsd: number;
  exitUsd: number;
  flashUsd: number;
  jupiterUsd: number;
  fixedUsd: number;
};

export type DecisionPolicy = {
  freshnessSeconds: number;
  quoteFreshnessSeconds: number;
  forecastWindowHours: number;
  minForecastSpanHours: number;
  horizonHours: number;
  leverageStepBps: number;
  liquidationBufferBps: number;
  cooldownHours: number;
  minOpenNetUsd: number;
  minSwitchEdgeUsd: number;
  forecastApyFloor: number;
  forecastApyCeiling: number;
};

export type RwaDecisionInput = {
  mode: EvidenceMode;
  asOf: string;
  equityUsd: number;
  strategies: StrategyDefinition[];
  reserves: ReserveEvidence[];
  navHistory: NavPoint[];
  currentPosition: CurrentPosition | null;
  costQuotes: CostQuote[];
  currentExitCostUsd: number | null;
  policy: DecisionPolicy;
};

export type CandidateEvaluation = {
  candidateId: string;
  strategyKey: string;
  leverageBps: number;
  eligible: boolean;
  reasonCodes: string[];
  forecastNavApy: number | null;
  grossApy: number | null;
  expectedGrossUsd: number | null;
  transitionCostUsd: number | null;
  expectedNetUsd: number | null;
  capacityEquityUsd: number | null;
  projectedLtvBps: number;
  liquidationThresholdBps: number | null;
  healthBufferBps: number | null;
};

export type RwaDecisionResult = {
  schemaVersion: 1;
  rulesetVersion: string;
  authorization: "shadow_only";
  evidenceMode: EvidenceMode;
  asOf: string;
  forecastCutoff: string;
  action: DecisionAction;
  reasonCode: string;
  chosen: CandidateEvaluation | null;
  current: CandidateEvaluation | null;
  rankedCandidates: CandidateEvaluation[];
  evidenceIds: string[];
};

function finite(value: unknown): value is number {
  return typeof value === "number" && Number.isFinite(value);
}

function nonNegative(value: unknown): value is number {
  return finite(value) && value >= 0;
}

function maxLeverageBpsForLtv(ltvBps: number): number {
  if (!Number.isInteger(ltvBps) || ltvBps < 0 || ltvBps >= LEVERAGE_DENOMINATOR) return 0;
  return Math.floor(LEVERAGE_DENOMINATOR / (1 - ltvBps / LEVERAGE_DENOMINATOR));
}

export function buildLeverageGrid(targetLtvBps: number, stepBps = 2_500): number[] {
  const maximum = maxLeverageBpsForLtv(targetLtvBps);
  if (maximum < LEVERAGE_DENOMINATOR || !Number.isInteger(stepBps) || stepBps <= 0) return [];
  const values = new Set<number>([LEVERAGE_DENOMINATOR, maximum]);
  for (let leverage = LEVERAGE_DENOMINATOR + stepBps; leverage < maximum; leverage += stepBps) values.add(leverage);
  return [...values].sort((left, right) => left - right);
}

export function estimateTrailingNavApy(
  points: NavPoint[],
  asOf: string,
  windowHours: number,
  minSpanHours: number,
  floor: number,
  ceiling: number,
): number | null {
  const cutoff = Date.parse(asOf);
  if (!finite(cutoff) || cutoff <= 0 || !finite(windowHours) || windowHours <= 0 || !finite(minSpanHours) || minSpanHours <= 0) return null;
  const start = cutoff - windowHours * 3_600_000;
  const eligible = points
    .filter((point) => {
      const timestamp = Date.parse(point.observedAt);
      return finite(point.priceUsd) && point.priceUsd > 0 && timestamp >= start && timestamp <= cutoff;
    })
    .sort((left, right) => Date.parse(left.observedAt) - Date.parse(right.observedAt));
  if (eligible.length < 2) return null;
  const first = eligible[0]!;
  const last = eligible.at(-1)!;
  const spanHours = (Date.parse(last.observedAt) - Date.parse(first.observedAt)) / 3_600_000;
  if (spanHours < minSpanHours || first.priceUsd <= 0 || last.priceUsd <= 0) return null;
  const annualized = Math.pow(last.priceUsd / first.priceUsd, HOURS_PER_YEAR / spanHours) - 1;
  if (!finite(annualized)) return null;
  return Math.min(ceiling, Math.max(floor, annualized));
}

function projectedLtvBps(leverageBps: number): number {
  return Math.round((1 - LEVERAGE_DENOMINATOR / leverageBps) * LEVERAGE_DENOMINATOR);
}

function quoteTotal(quote: CostQuote): number | null {
  const values = [quote.entryUsd, quote.exitUsd, quote.flashUsd, quote.jupiterUsd, quote.fixedUsd];
  return values.every(nonNegative) ? values.reduce((sum, value) => sum + value, 0) : null;
}

function freshnessProblem(observation: ReserveEvidence, input: RwaDecisionInput): string | null {
  const observedAt = Date.parse(observation.observedAt);
  const asOf = Date.parse(input.asOf);
  if (!finite(observedAt) || observedAt > asOf) return "observation_time_invalid";
  if (input.mode === "live") {
    if (observation.schema !== "live_v2") return "live_v2_required";
    if ((asOf - observedAt) / 1_000 > input.policy.freshnessSeconds) return "observation_stale";
  } else if (observation.schema !== "kamino_api_history_v1") {
    return "historical_schema_required";
  }
  return null;
}

function capacityFor(
  input: RwaDecisionInput,
  collateral: ReserveEvidence,
  debt: ReserveEvidence,
  leverageBps: number,
): { capacity: number | null; reason: string | null } {
  const leverage = leverageBps / LEVERAGE_DENOMINATOR;
  const collateralValues = [collateral.depositLimitUsd, collateral.totalSupplyUsd];
  if (input.mode === "live" && !collateralValues.every(nonNegative)) return { capacity: null, reason: "capacity_missing" };
  const collateralHeadroom = collateralValues.every(nonNegative)
    ? Math.max(0, collateral.depositLimitUsd! - collateral.totalSupplyUsd!)
    : Number.POSITIVE_INFINITY;
  let capacity = collateralHeadroom / leverage;
  if (leverage <= 1) return { capacity, reason: capacity + 1e-9 >= input.equityUsd ? null : "capacity_insufficient" };

  const requiredLive = [
    debt.availableLiquidityUsd, debt.borrowLimitUsd, debt.totalBorrowUsd,
    debt.borrowLimitOutsideGroupUsd, debt.borrowedOutsideGroupUsd, debt.debtWithdrawalHeadroomUsd,
  ];
  if (input.mode === "live" && !requiredLive.every(nonNegative)) return { capacity: null, reason: "capacity_missing" };
  const headrooms = [
    debt.availableLiquidityUsd,
    nonNegative(debt.borrowLimitUsd) && nonNegative(debt.totalBorrowUsd) ? Math.max(0, debt.borrowLimitUsd - debt.totalBorrowUsd) : null,
    nonNegative(debt.borrowLimitOutsideGroupUsd) && nonNegative(debt.borrowedOutsideGroupUsd)
      ? Math.max(0, debt.borrowLimitOutsideGroupUsd - debt.borrowedOutsideGroupUsd) : null,
    debt.debtWithdrawalHeadroomUsd,
  ].filter(nonNegative);
  const debtHeadroom = headrooms.length > 0 ? Math.min(...headrooms) : Number.POSITIVE_INFINITY;
  capacity = Math.min(capacity, debtHeadroom / (leverage - 1));
  return { capacity, reason: capacity + 1e-9 >= input.equityUsd ? null : "capacity_insufficient" };
}

function rejected(candidateId: string, strategyKey: string, leverageBps: number, reasons: string[]): CandidateEvaluation {
  return {
    candidateId, strategyKey, leverageBps, eligible: false, reasonCodes: [...new Set(reasons)].sort(),
    forecastNavApy: null, grossApy: null, expectedGrossUsd: null, transitionCostUsd: null,
    expectedNetUsd: null, capacityEquityUsd: null, projectedLtvBps: projectedLtvBps(leverageBps),
    liquidationThresholdBps: null, healthBufferBps: null,
  };
}

function rank(candidates: CandidateEvaluation[]): CandidateEvaluation[] {
  return [...candidates].sort((left, right) => {
    if (left.eligible !== right.eligible) return left.eligible ? -1 : 1;
    const score = (right.expectedNetUsd ?? Number.NEGATIVE_INFINITY) - (left.expectedNetUsd ?? Number.NEGATIVE_INFINITY);
    return Math.abs(score) > 1e-12 ? score : left.candidateId.localeCompare(right.candidateId);
  });
}

function result(
  input: RwaDecisionInput,
  action: DecisionAction,
  reasonCode: string,
  chosen: CandidateEvaluation | null,
  current: CandidateEvaluation | null,
  candidates: CandidateEvaluation[],
): RwaDecisionResult {
  return {
    schemaVersion: RWA_DECISION_SCHEMA_VERSION,
    rulesetVersion: RWA_DECISION_RULESET_VERSION,
    authorization: "shadow_only",
    evidenceMode: input.mode,
    asOf: input.asOf,
    forecastCutoff: input.asOf,
    action,
    reasonCode,
    chosen,
    current,
    rankedCandidates: rank(candidates),
    evidenceIds: [...new Set(input.reserves.map((observation) => observation.evidenceId).filter(Boolean))].sort(),
  };
}

export function decideRwa(input: RwaDecisionInput): RwaDecisionResult {
  if (!nonNegative(input.equityUsd) || input.equityUsd <= 0 || !finite(Date.parse(input.asOf))) {
    return result(input, "defer", "invalid_input", null, null, []);
  }
  const observations = new Map<string, ReserveEvidence>();
  const duplicateReserves = new Set<string>();
  for (const observation of input.reserves) {
    if (observations.has(observation.reserve)) duplicateReserves.add(observation.reserve);
    observations.set(observation.reserve, observation);
  }
  const currentId = input.currentPosition
    ? `${input.currentPosition.strategyKey}@${input.currentPosition.leverageBps}`
    : null;
  const candidates: CandidateEvaluation[] = [];

  for (const strategy of [...input.strategies].sort((left, right) => left.key.localeCompare(right.key))) {
    const collateral = observations.get(strategy.collateralReserve);
    const debt = observations.get(strategy.debtReserve);
    const initialReasons: string[] = [];
    if (duplicateReserves.size > 0) initialReasons.push("duplicate_reserve_evidence");
    if (!collateral || !debt) initialReasons.push("observation_missing");
    if (collateral) {
      const problem = freshnessProblem(collateral, input);
      if (problem) initialReasons.push(problem);
      if (!collateral.active) initialReasons.push("collateral_inactive");
      if (input.mode === "live" && collateral.emergencyMode === null) initialReasons.push("collateral_risk_observation_missing");
      if (collateral.emergencyMode === true) initialReasons.push("collateral_emergency");
    }
    if (debt) {
      const problem = freshnessProblem(debt, input);
      if (problem) initialReasons.push(problem);
      if (!debt.active) initialReasons.push("debt_inactive");
      if (input.mode === "live" && debt.emergencyMode === null) initialReasons.push("debt_risk_observation_missing");
      if (debt.emergencyMode === true) initialReasons.push("debt_emergency");
    }
    const liquidationThreshold = collateral?.liquidationThresholdBps;
    if (!finite(liquidationThreshold) || liquidationThreshold <= input.policy.liquidationBufferBps || liquidationThreshold >= 10_000) {
      initialReasons.push("liquidation_threshold_invalid");
    }
    const safetyLtv = finite(liquidationThreshold)
      ? liquidationThreshold - input.policy.liquidationBufferBps
      : 0;
    const effectiveTargetLtv = Math.min(strategy.policyTargetLtvBps, safetyLtv);
    const leverageGrid = buildLeverageGrid(effectiveTargetLtv, input.policy.leverageStepBps);
    if (leverageGrid.length === 0) leverageGrid.push(LEVERAGE_DENOMINATOR);
    const forecast = collateral
      ? estimateTrailingNavApy(
          input.navHistory.filter((point) => point.reserve === collateral.reserve), input.asOf,
          input.policy.forecastWindowHours, input.policy.minForecastSpanHours,
          input.policy.forecastApyFloor, input.policy.forecastApyCeiling,
        )
      : null;
    if (forecast === null) initialReasons.push("forecast_missing");

    for (const leverageBps of leverageGrid) {
      const candidateId = `${strategy.key}@${leverageBps}`;
      const reasons = [...initialReasons];
      const currentCandidate = candidateId === currentId;
      if (!collateral || !debt || forecast === null) {
        candidates.push(rejected(candidateId, strategy.key, leverageBps, reasons));
        continue;
      }
      if (![collateral.priceUsd, collateral.supplyApy, debt.priceUsd, debt.borrowApy].every(nonNegative)) reasons.push("economic_observation_missing");
      const projectedLtv = projectedLtvBps(leverageBps);
      if (projectedLtv > strategy.policyTargetLtvBps) reasons.push("policy_cap_exceeded");
      if (projectedLtv > safetyLtv) reasons.push("liquidation_buffer_exceeded");
      const capacity = currentCandidate
        ? { capacity: Number.POSITIVE_INFINITY, reason: null }
        : capacityFor(input, collateral, debt, leverageBps);
      if (capacity.reason) reasons.push(capacity.reason);

      let transitionCost = 0;
      if (!currentCandidate) {
        const quote = input.costQuotes.find((row) =>
          row.candidateId === candidateId && row.fromCandidateId === currentId && Math.abs(row.amountUsd - input.equityUsd) <= 1e-6,
        );
        if (!quote || !quote.available) reasons.push("quote_missing");
        else {
          const quoteAt = Date.parse(quote.observedAt);
          if (!finite(quoteAt) || quoteAt > Date.parse(input.asOf) || (Date.parse(input.asOf) - quoteAt) / 1_000 > input.policy.quoteFreshnessSeconds) reasons.push("quote_stale");
          const total = quoteTotal(quote);
          if (total === null) reasons.push("quote_invalid");
          else transitionCost = total;
        }
      }
      if (reasons.length > 0) {
        candidates.push(rejected(candidateId, strategy.key, leverageBps, reasons));
        continue;
      }
      const leverage = leverageBps / LEVERAGE_DENOMINATOR;
      const grossApy = leverage * (forecast + collateral.supplyApy!) - (leverage - 1) * debt.borrowApy!;
      const expectedGrossUsd = input.equityUsd * grossApy * input.policy.horizonHours / HOURS_PER_YEAR;
      candidates.push({
        candidateId, strategyKey: strategy.key, leverageBps, eligible: true, reasonCodes: ["eligible"],
        forecastNavApy: forecast, grossApy, expectedGrossUsd, transitionCostUsd: transitionCost,
        expectedNetUsd: expectedGrossUsd - transitionCost, capacityEquityUsd: capacity.capacity,
        projectedLtvBps: projectedLtv, liquidationThresholdBps: liquidationThreshold,
        healthBufferBps: liquidationThreshold - projectedLtv,
      });
    }
  }

  const ranked = rank(candidates);
  const eligible = ranked.filter((candidate) => candidate.eligible);
  const current = currentId
    ? candidates.find((candidate) => candidate.candidateId === currentId && candidate.eligible) ?? null
    : null;

  if (!input.currentPosition) {
    const best = eligible[0] ?? null;
    if (!best || best.expectedNetUsd === null || best.expectedNetUsd <= input.policy.minOpenNetUsd) {
      return result(input, "defer", best ? "open_edge_insufficient" : "no_eligible_candidate", null, null, candidates);
    }
    return result(input, "open", "best_net_candidate", best, null, candidates);
  }

  const currentStrategy = input.strategies.find((strategy) => strategy.key === input.currentPosition!.strategyKey);
  const currentCollateral = currentStrategy ? observations.get(currentStrategy.collateralReserve) : null;
  const currentDebt = currentStrategy ? observations.get(currentStrategy.debtReserve) : null;
  if (!currentStrategy) return result(input, "exit", "current_strategy_not_allowed", null, current, candidates);
  if (currentCollateral?.emergencyMode === true || currentDebt?.emergencyMode === true || currentCollateral?.active === false || currentDebt?.active === false) {
    return result(input, "exit", "known_market_risk", null, current, candidates);
  }
  if (input.mode === "live" && (currentCollateral?.emergencyMode === null || currentDebt?.emergencyMode === null)) {
    return result(input, "defer", "risk_observation_missing", null, current, candidates);
  }
  const currentFreshness = currentCollateral && currentDebt
    ? freshnessProblem(currentCollateral, input) ?? freshnessProblem(currentDebt, input)
    : "observation_missing";
  if (currentFreshness) return result(input, "defer", currentFreshness, null, current, candidates);

  const liquidationThreshold = currentCollateral?.liquidationThresholdBps ?? 0;
  const safeLtv = liquidationThreshold - input.policy.liquidationBufferBps;
  const currentUnsafe = input.currentPosition.currentLtvBps >= safeLtv
    || input.currentPosition.currentLtvBps > currentStrategy.policyTargetLtvBps;
  if (currentUnsafe) {
    const safer = eligible
      .filter((candidate) => candidate.strategyKey === currentStrategy.key && candidate.leverageBps < input.currentPosition!.leverageBps)
      .sort((left, right) => right.leverageBps - left.leverageBps)[0] ?? null;
    return safer
      ? result(input, "delever", "risk_limit_exceeded", safer, current, candidates)
      : result(input, "exit", "no_safe_deleverage_candidate", null, current, candidates);
  }
  if (!current) return result(input, "defer", "current_candidate_unscorable", null, null, candidates);

  if (nonNegative(input.currentExitCostUsd) && current.expectedGrossUsd! < -input.currentExitCostUsd) {
    return result(input, "exit", "holding_value_below_exit", null, current, candidates);
  }
  const best = eligible[0] ?? current;
  if (best.candidateId === current.candidateId) return result(input, "hold", "current_is_best", current, current, candidates);
  const ageHours = (Date.parse(input.asOf) - Date.parse(input.currentPosition.openedAt)) / 3_600_000;
  if (!finite(ageHours) || ageHours < input.policy.cooldownHours) return result(input, "hold", "cooldown_active", current, current, candidates);
  const edge = best.expectedNetUsd! - current.expectedNetUsd!;
  if (edge + 1e-12 < input.policy.minSwitchEdgeUsd) return result(input, "hold", "switch_edge_insufficient", current, current, candidates);
  return result(input, "switch", "better_net_candidate", best, current, candidates);
}
