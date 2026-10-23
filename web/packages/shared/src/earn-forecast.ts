export type EarnForecastResponse = {
  strategy:
    | "safe_no_fees"
    | "safe_fee_aware_1bps"
    | "medium_fee_aware_1bps"
    | "realized_7d_share_price";
  source?: "realized_7d" | "live";
  availability?: "available" | "stale" | "unavailable";
  apyBps: number;
  rangeLowBps: number;
  rangeHighBps: number;
  window: { startedAt: string; endedAt: string };
  updatedAt: string;
};

export type EarnForecastApyHistorySample = {
  observedAt: string;
  apyBps: number;
};

export type EarnForecastApyHistorySeries = {
  key: "loyal" | "mainUsdcReserve";
  label: string;
  metadata?: Record<string, string | number | boolean | null>;
  samples: EarnForecastApyHistorySample[];
};

export type EarnForecastApyHistoryResponse = {
  feeBps: 1;
  generatedAt: string;
  riskProfile: "safe" | "medium";
  samples: EarnForecastApyHistorySample[];
  series?: EarnForecastApyHistorySeries[];
  window: { startedAt: string; endedAt: string };
};

export type EarnForecastSummaryResponse = {
  forecast: EarnForecastResponse;
  history: EarnForecastApyHistoryResponse;
};
