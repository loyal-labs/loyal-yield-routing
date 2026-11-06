import type {
  EarnMaxActivityItem,
  EarnMaxCoverage,
  EarnMaxEarnings,
  EarnMaxPerformancePoint,
  EarnMaxWithdrawalView,
} from "@loyal-labs/shared";
import type { WithdrawalCostStatus } from "@loyal-labs/api";

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
  recoverableUsdcRaw?: string | null;
  withdrawalApproval?: {
    costStatus?: WithdrawalCostStatus | null;
    approvedReferenceRaw?: string | null;
    approvedFeeLamports?: string | null;
    generationKey?: string;
    floorRaw: string | null;
    reason: string | null;
    referenceRaw: string;
  };
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
  issuanceFeeBps: number;
  performance: EarnMaxPerformancePoint[];
  policyStatus: string | null;
  realizedApyBps: number | null;
  status: string;
  strategyLabel: string;
  withdrawal: EarnMaxWithdrawalView | null;
};

export type EarnMaxActions = {
  recoverClaimRemainder?: () => Promise<boolean>;
  approveWithdrawal?: (
    minClaimRaw: bigint,
    maxFeeLamports: bigint,
  ) => Promise<boolean>;
  cancelWithdrawal: () => Promise<boolean>;
  claim: () => Promise<boolean>;
  close: () => Promise<boolean>;
  deposit: (amountRaw: bigint) => Promise<boolean>;
  install: () => Promise<boolean>;
  refresh: () => Promise<void>;
  requestWithdrawal: (amountRaw: bigint | "max") => Promise<boolean>;
};
