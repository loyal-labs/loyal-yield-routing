import type {
  EarnMaxActivityItem,
  EarnMaxCoverage,
  EarnMaxEarnings,
  EarnMaxPerformancePoint,
  EarnMaxWithdrawalView,
} from "@loyal-labs/shared";

export type {
  EarnMaxActivityItem,
  EarnMaxActivityResponse,
  EarnMaxApyPoint,
  EarnMaxCoverage,
  EarnMaxPerformancePoint,
  EarnMaxPolicyBinding,
  EarnMaxSummary,
  EarnMaxSummaryResponse,
  EarnMaxWithdrawalHealth,
  EarnMaxWithdrawalView,
} from "@loyal-labs/shared";

export type EarnMaxViewModel = {
  activity: EarnMaxActivityItem[];
  balanceUsd: number;
  canWithdraw: boolean;
  coverage: EarnMaxCoverage;
  earnedUsd: number | null;
  earnings?: EarnMaxEarnings | null;
  error: string | null;
  forecastApyBps: number | null;
  isBusy: boolean;
  isLoading: boolean;
  performance: EarnMaxPerformancePoint[];
  policyStatus: string | null;
  realizedApyBps: number | null;
  status: string;
  strategyLabel: string;
  withdrawal: EarnMaxWithdrawalView | null;
};

export type EarnMaxActions = {
  cancelWithdrawal: () => Promise<boolean>;
  claim: () => Promise<boolean>;
  close: () => Promise<boolean>;
  deposit: (amountRaw: bigint) => Promise<boolean>;
  install: () => Promise<boolean>;
  refresh: () => Promise<void>;
  requestWithdrawal: (amountRaw: bigint | "max") => Promise<boolean>;
};
