export type BackyardStrategyId = "main" | "onre" | "prime" | "maple";

export type VaultPosition = Readonly<{
  id: BackyardStrategyId;
  label: string;
  reserve: string;
  valueRaw: bigint;
}>;
export type VaultSnapshot = Readonly<{
  contextSlot: number;
  observedAt: string;
  totalValueRaw: bigint;
  idleRaw: bigint;
  lpSupplyRaw: bigint;
  positions: readonly VaultPosition[];
}>;

export type VaultFlow = Readonly<{
  kind: "deposit" | "withdrawal";
  signature: string;
  timestamp: number;
  amountRaw: bigint;
  totalValueBeforeRaw: bigint;
  totalValueAfterRaw: bigint;
}>;

export type VaultHistory = Readonly<{
  cutoffTimestamp: number;
  depositsRaw: bigint;
  withdrawalsRaw: bigint;
  flows: readonly VaultFlow[];
  scannedSignatureCount: number;
}>;

export type ReserveRate = Readonly<{
  reserve: string;
  supplyApy: number;
  observedAt: string;
  slot: number;
}>;

export type VaultApy = Readonly<{
  grossSupplyApy: number;
  netSupplyApy: number;
  performanceFeeBps: number;
  observedAt: string;
}>;

export type VaultBalancePoint = Readonly<{
  date: string;
  balanceRaw: bigint;
  depositsRaw: bigint;
  withdrawalsRaw: bigint;
}>;
