import type { EarnStablecoinSymbol } from "@/lib/earn/stablecoin-monitor.shared";

export type EarnFlowPoint = {
  date: string;
  depositedRaw: bigint;
  liquidityMint: string;
  netRaw: bigint;
  withdrawnRaw: bigint;
};

export type EarnPositionRow = {
  collateralReserveRows: number;
  collateralStoredAmountRaw: bigint;
  currentObservedAt: string;
  currentPointerDeltaRaw: bigint;
  currentReserve: string;
  depositMint: string;
  excludedReserveRaw: bigint;
  idleAmountRaw: bigint;
  missingManagedVaultRows: number;
  missingRedeemableMetadataRows: number;
  normalizedAumRaw: bigint;
  normalizedReserveRaw: bigint;
  principalAmountRaw: bigint;
  redeemableReserveRows: number;
  settings: string;
  storedCurrentPointerRaw: bigint;
  unknownReserveSemanticsRows: number;
  walletAddress: string;
};

export type EarnStablecoinSummary = {
  activeAumRaw: bigint;
  activeExcludedReserveRaw: bigint;
  activeIdleRaw: bigint;
  activePositionCount: number;
  activePrincipalRaw: bigint;
  activeReserveRaw: bigint;
  activeStoredCurrentPointerRaw: bigint;
  currentPointerDeltaRaw: bigint;
  deposited30dRaw: bigint;
  latestRebalanceAt: string | null;
  liquidityMint: string;
  symbol: EarnStablecoinSymbol;
  withdrawn30dRaw: bigint;
};

export type EarnFreshnessMetric = {
  label: string;
  timestamp: string | null;
};

export type EarnData = {
  activeAumRaw: bigint;
  activeAutodepositPolicies: number;
  activeCollateralStoredAmountRaw: bigint;
  activeCurrentPointerDeltaRaw: bigint;
  activeExcludedReserveRaw: bigint;
  activeIdleRaw: bigint;
  activeMissingManagedVaultRows: number;
  activeMissingRedeemableMetadataRows: number;
  activePrincipalRaw: bigint;
  activeRedeemableReserveRows: number;
  activeCollateralReserveRows: number;
  activeReserveRaw: bigint;
  activeStoredCurrentPointerRaw: bigint;
  activeUnknownReserveSemanticsRows: number;
  autodepositExecutionAmount30dRaw: bigint;
  autodepositExecutionAmountRaw: bigint;
  autodepositExecutionCount30d: number;
  autodepositExecutionCount: number;
  autodepositStatusCounts: {
    active: number;
    closed: number;
    inconsistent: number;
    paused: number;
    pending: number;
  };
  flow30d: EarnFlowPoint[];
  freshness: EarnFreshnessMetric[];
  scheduledEligibleAmountRaw: bigint;
  scheduledEligibleLotCount: number;
  scheduledOpenAmountRaw: bigint;
  scheduledOpenLotCount: number;
  stablecoins: EarnStablecoinSummary[];
  topPositions: EarnPositionRow[];
  totalDeposited30dRaw: bigint;
  totalWithdrawn30dRaw: bigint;
  uniqueEarnPolicies: number;
  uniqueEarnUsers: number;
};
