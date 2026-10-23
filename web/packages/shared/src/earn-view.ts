import {
  formatAutodepositUsdLabel,
  type LoadedEarnAutodepositConfig,
} from "./earn-autodeposit-loaded-state";
import type { EarnForecastResponse } from "./earn-forecast";

export const EARN_FORECAST_REALIZED_MAX_AGE_MS = 3 * 60 * 60 * 1000;

export type EarnForecastApy = Pick<
  EarnForecastResponse,
  "apyBps" | "availability" | "rangeHighBps" | "rangeLowBps" | "source"
>;

export function toForecastApy(
  payload: EarnForecastResponse,
  nowMs: number = Date.now()
): EarnForecastApy {
  const expired =
    payload.strategy === "realized_7d_share_price" &&
    (!Number.isFinite(Date.parse(payload.updatedAt)) ||
      nowMs - Date.parse(payload.updatedAt) >=
        EARN_FORECAST_REALIZED_MAX_AGE_MS);
  return {
    apyBps: payload.apyBps,
    availability: expired
      ? "unavailable"
      : payload.availability ??
        (payload.strategy === "safe_no_fees" ? "unavailable" : "available"),
    rangeHighBps: payload.rangeHighBps,
    rangeLowBps: payload.rangeLowBps,
    source: payload.source,
  };
}

export function formatEarnApyPercent(apyBps: number): string {
  return `${(apyBps / 100).toFixed(2)}%`;
}

export function formatEarnApyLabel(apy: EarnForecastApy | undefined): string {
  if (!apy || apy.availability === "unavailable") {
    return "APY unavailable";
  }
  const qualifiers = [
    ...(apy.source === "live" ? ["6–24h live"] : []),
    ...(apy.availability === "stale" ? ["stale"] : []),
  ];
  const label = `${formatEarnApyPercent(apy.apyBps)} APY`;
  return qualifiers.length > 0 ? `${label} (${qualifiers.join(", ")})` : label;
}

export type EarnAutodepositViewState =
  | LoadedEarnAutodepositConfig["state"]
  | "closing"
  | "pausing"
  | "resuming";

export function earnAutodepositLabel(
  autodeposit: { keepAmount: string; state: EarnAutodepositViewState } | null
): string {
  if (!autodeposit) {
    return "Start earning the moment your money arrives";
  }
  switch (autodeposit.state) {
    case "pausing":
      return "Pausing…";
    case "resuming":
      return "Resuming…";
    case "paused":
      return "Paused";
    default:
      return `Anything above ${formatAutodepositUsdLabel(autodeposit.keepAmount)}`;
  }
}

export type EarnHoldingInput = {
  amountRaw: string;
  kind: "idle" | "kamino";
  liquidityMint: string;
  market: string | null;
  marketName: string;
  provenance: Record<string, unknown>;
  reserve: string | null;
  sourceId?: string;
  supplyApyBps: string | null;
  tokenProgramId: string;
};

export type EarnWithdrawSourceModel = {
  amountRaw: string;
  balance: number;
  id: string;
  label: string;
  liquidityMint: string;
  market: string | null;
  reserve: string | null;
  sourceId: string;
  supplyApyBps: string | null;
  tokenAccount: string | null;
  tokenProgramId: string;
  type: "reserve" | "idle";
};

function holdingTokenAccount(holding: EarnHoldingInput): string | null {
  return typeof holding.provenance.tokenAccount === "string"
    ? holding.provenance.tokenAccount
    : null;
}

export function earnWithdrawSourceKey(holding: EarnHoldingInput): string {
  if (typeof holding.sourceId === "string") {
    return holding.sourceId;
  }
  const sourceId =
    holding.kind === "idle"
      ? holdingTokenAccount(holding) ?? holding.liquidityMint
      : holding.reserve ?? holding.liquidityMint;
  return `${holding.kind}:${sourceId}`;
}

function hasPositiveAmount(holding: EarnHoldingInput): boolean {
  try {
    return BigInt(holding.amountRaw) > BigInt(0);
  } catch {
    return false;
  }
}

export function earnWithdrawSources(
  holdings: readonly EarnHoldingInput[] | undefined
): EarnWithdrawSourceModel[] {
  return (holdings ?? []).filter(hasPositiveAmount).map((holding) => {
    const sourceId = earnWithdrawSourceKey(holding);
    return {
      amountRaw: holding.amountRaw,
      balance: Number(BigInt(holding.amountRaw)) / 1_000_000,
      id: sourceId,
      label:
        holding.kind === "idle"
          ? `Idle vault ${holding.marketName}`
          : `${holding.marketName} reserve`,
      liquidityMint: holding.liquidityMint,
      market: holding.market,
      reserve: holding.reserve,
      sourceId,
      supplyApyBps: holding.supplyApyBps,
      tokenAccount: holdingTokenAccount(holding),
      tokenProgramId: holding.tokenProgramId,
      type: holding.kind === "idle" ? "idle" : "reserve",
    };
  });
}
