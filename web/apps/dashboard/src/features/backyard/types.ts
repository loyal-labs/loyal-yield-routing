// The JSON GET /api/backyard returns (go/workers/internal/stats/backyard.go).
// Each source loads on its own; a failed one is { ok: false, value: null }.

export type Part<T> = { ok: true; value: T } | { ok: false; value: null };

export type LeverageLane = {
  apyPct: Record<string, number>;
  enterable: boolean;
  lane: string;
  levels: number[];
  older: boolean;
  spreadPct: number;
};

export type Route = {
  aumUsd: number | null;
  collateralUsd: number | null;
  currentApy: {
    apyPct: number;
    flat: boolean;
    lane: string | null;
    level: number | null;
    observedAt: string | null;
  } | null;
  currentOperationId: string | null;
  custodyUsd: number | null;
  debtUsd: number | null;
  lastHoldReason: string | null;
  leverageTarget: number | null;
  leverageWatch: { lanes: LeverageLane[]; observedAt: string } | null;
  ltvRatio: number | null;
  navFresh: boolean | null;
  navUsd: number | null;
  reportAgeS: number | null;
  reportObservedAt: string | null;
  routeStatus: string | null;
  selectorAction: string | null;
  selectorReason: string | null;
  upApproved: boolean | null;
  updatedAgeS: number;
  voltrIdleUsd: number | null;
};

export type MoneyMove = {
  action: string;
  amountRaw: string | null;
  at: string;
  reason: string | null;
  signature: string | null;
  status: string;
  strategyKey: string | null;
};

export type BackyardApi = {
  borrowing: Part<
    Array<{
      borrowApyPct: number;
      lane: string;
      roomTo90Usd: number;
      supplyUsd: number;
      token: string;
      utilization: number;
    }>
  >;
  breakdown: Part<{
    lane: string;
    points: Array<{ at: string; borrowPct: number; supplyPct: number }>;
  }>;
  health: Part<{
    navAll: number;
    navReconciled: number;
    openStops: string[];
    refusedBeforeSending: number;
  }>;
  history: Part<
    Array<{ at: string; equityUsd: number | null; ltvPct: number | null }>
  >;
  loadedAt: string;
  moveDays: Part<string[]>;
  moves: Part<MoneyMove[]>;
  navSeries: Part<Array<{ at: string; navUsd: number }>>;
  route: Part<Route>;
  voltr: Part<{
    daily: Array<{ day: string; price: number }>;
    /** Voltr's share price: USDC per 1,000 LP units (per LP token). */
    sharePrice: number;
    totalValueUsd: number | null;
    tvl: Array<{ at: string; value: number }>;
  }>;
};
