export type RebalanceRouteMode = "same_mint" | "cross_mint";

export type RebalanceAuditRange = "24h" | "7d" | "30d" | "all";

export type RebalanceAuditView =
  | "completed_rebalances"
  | "completed_deposits"
  | "errors";

export type RebalanceAuditErrorFilter =
  | "all"
  | "rebalance"
  | "deposit"
  | "needs_review";

export type RebalanceAuditLane = "rebalance" | "deposit" | "needs_review";

export type RebalanceAuditSource =
  | "autodeposit"
  | "idle_vault_deposit"
  | "manual_deposit"
  | "needs_review"
  | "rebalance";

export type EarnActiveReserveRouteRow = {
  activeAumRaw: bigint;
  currentReserve: string;
  latestObservedAt: string | null;
  liquidityMint: string;
  positionCount: number;
};

export type EarnRebalanceDecisionRow = {
  abandonReason: string | null;
  amountRaw: bigint | null;
  confirmedSlot: bigint | null;
  createdAt: string;
  decisionReason: string | null;
  decisionType: "rebalance" | null;
  estimatedEdgeBps: number | null;
  id: string;
  liquidityMint: string | null;
  routeMode: RebalanceRouteMode;
  signature: string | null;
  sourceLiquidityMint: string | null;
  sourceApyBps: number | null;
  sourceReserve: string | null;
  status: string;
  targetApyBps: number | null;
  targetLiquidityMint: string | null;
  targetReserve: string | null;
  updatedAt: string;
};

export type RebalanceActivityPoint = {
  bucketStartedAt: string;
  confirmed: number;
  finalizedSwapLegs: number;
  expiredSubmissions: number;
  failedDecisions: number;
  failedOpportunities: number;
  fleetClaims: number;
  maxSwapFeeLamports: bigint;
  routeMode: RebalanceRouteMode;
  swapFeeLamports: bigint;
  terminalAttempts: number;
};

export type AutodepositTimeSeriesRangeKey = "2d" | "7d" | "30d";

export type Last30DaysRebalancePoint = {
  confirmed: number;
  date: string;
  failed: number;
  routeMode: RebalanceRouteMode;
  terminalAttempts: number;
};

export type RebalanceAuditRow = {
  abandonReason: string | null;
  amountRaw: bigint | null;
  confirmedSlot: bigint | null;
  createdAt: string;
  decisionReason: string | null;
  estimatedEdgeBps: number | null;
  id: string;
  lane: RebalanceAuditLane;
  liquidityMint: string | null;
  movementKind: string | null;
  source: RebalanceAuditSource;
  sourceLiquidityMint: string | null;
  signature: string | null;
  secondarySignature: string | null;
  sourceApyBps: number | null;
  sourceReserve: string | null;
  status: string;
  submittedSlot: bigint | null;
  targetApyBps: number | null;
  targetLiquidityMint: string | null;
  targetReserve: string | null;
  updatedAt: string;
  vaultId: string | null;
  vaultIndex: number | null;
  vaultPubkey: string | null;
};

export type RebalanceAuditSummary = {
  active: number;
  completedDeposits: number;
  completedRebalances: number;
  depositErrors: number;
  errors: number;
  needsReview: number;
  rebalanceErrors: number;
  staleActive: number;
};
