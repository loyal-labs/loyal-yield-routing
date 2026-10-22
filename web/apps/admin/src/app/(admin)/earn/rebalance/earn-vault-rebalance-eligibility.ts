import type { SafeReserveApyStatusRow } from "@/lib/kamino/timescale-reserve-monitor.shared";

export const REBALANCE_ECONOMIC_POLICY = {
  fixedSafetyMarginUsdMicros: 50_000,
  holdingHorizonSeconds: 30 * 24 * 60 * 60,
  minimumNetGainUsdMicros: 100_000,
  minimumNotionalUsdMicros: 1_000_000,
} as const;

const SECONDS_PER_YEAR = 31_536_000;
const USD_MICROS = 1_000_000;

export function computeRebalanceEligibilityFloorRaw(
  reserveStatuses: readonly SafeReserveApyStatusRow[],
  decimals: number
): bigint | null {
  // Only reserves the Safe monitor still considers eligible can anchor a
  // spread. Stale or below-liquidity reserves routinely report 0%, which would
  // otherwise invent an enormous edge and collapse the floor to nothing.
  const apys = reserveStatuses
    .filter((status) => status.status === "eligible")
    .map((status) => status.supplyApyPercent)
    .filter((apy): apy is number => apy !== null && Number.isFinite(apy) && apy > 0);
  if (apys.length < 2) {
    return null;
  }

  const bestEdgeFraction = (Math.max(...apys) - Math.min(...apys)) / 100;
  if (bestEdgeFraction <= 0) {
    return null;
  }

  const horizonFraction =
    REBALANCE_ECONOMIC_POLICY.holdingHorizonSeconds / SECONDS_PER_YEAR;
  const requiredGainUsd =
    (REBALANCE_ECONOMIC_POLICY.minimumNetGainUsdMicros +
      REBALANCE_ECONOMIC_POLICY.fixedSafetyMarginUsdMicros) /
    USD_MICROS;
  const gainFloorUsd = requiredGainUsd / (bestEdgeFraction * horizonFraction);
  const notionalFloorUsd =
    REBALANCE_ECONOMIC_POLICY.minimumNotionalUsdMicros / USD_MICROS;

  return toRawAmount(Math.max(gainFloorUsd, notionalFloorUsd), decimals);
}

function toRawAmount(amount: number, decimals: number): bigint {
  const scale = 10 ** decimals;
  return BigInt(Math.ceil(amount * scale));
}

export function summarizeRebalanceEligibility<
  Vault extends {
    currentDepositRaw: string;
    opportunityCount?: number;
    rebalanceCount: number;
  },
>(
  vaults: readonly Vault[],
  floorRaw: bigint | null
): {
  eligibleCount: number;
  eligibleRebalancedCount: number;
  ineligibleCount: number;
} {
  let eligibleCount = 0;
  let eligibleRebalancedCount = 0;

  for (const vault of vaults) {
    const hasEvidence =
      vault.rebalanceCount > 0 || (vault.opportunityCount ?? 0) > 0;
    const clearsFloor =
      floorRaw === null || BigInt(vault.currentDepositRaw) >= floorRaw;
    if (!hasEvidence && !clearsFloor) {
      continue;
    }
    eligibleCount += 1;
    if (vault.rebalanceCount > 0) {
      eligibleRebalancedCount += 1;
    }
  }

  return {
    eligibleCount,
    eligibleRebalancedCount,
    ineligibleCount: vaults.length - eligibleCount,
  };
}
