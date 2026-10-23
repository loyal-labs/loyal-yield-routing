import type { EarnMaxSummary, HealthView, WithdrawalView } from "@loyal-labs/api";

export { buildEarnMaxDailySeries as earnMaxDailySeries } from "./earn-max-history";

export type EarnMaxWithdrawalInput = Pick<
  WithdrawalView,
  "canClaim" | "claimBlockedReason" | "status"
> & { health?: Pick<HealthView, "lastKnownAttention" | "status"> };

export type EarnMaxPending =
  | "operator_attention"
  | "status_unavailable_after_attention"
  | "status_unavailable"
  | "insufficient_liquidity"
  | "liquidity_unverified";

const PENDING_COPY: Record<EarnMaxPending, { message: string; subtitle: string }> = {
  operator_attention: {
    message:
      "Vault withdrawals need operator attention. Your request is still pending and may take longer than usual. You do not need to submit it again.",
    subtitle: "Waiting for operator review",
  },
  status_unavailable_after_attention: {
    message:
      "The last vault status required operator attention. An updated status is unavailable. Your request remains pending.",
    subtitle: "Withdrawal status unavailable",
  },
  status_unavailable: {
    message:
      "An updated vault status is unavailable. Your request remains pending. Check status again before claiming.",
    subtitle: "Withdrawal status unavailable",
  },
  insufficient_liquidity: {
    message:
      "The vault does not have enough available USDC to pay this withdrawal yet. Check again later.",
    subtitle: "Waiting for vault liquidity",
  },
  liquidity_unverified: {
    message:
      "Vault liquidity could not be verified. Check status before claiming.",
    subtitle: "Liquidity not verified",
  },
};

export function earnMaxWithdrawalPending(
  withdrawal: Pick<WithdrawalView, "status"> | null
): boolean {
  return withdrawal !== null && withdrawal.status !== "claimed";
}

export function earnMaxPending(
  withdrawal: EarnMaxWithdrawalInput | null
): EarnMaxPending | null {
  if (!withdrawal || withdrawal.canClaim || withdrawal.status === "claimed") {
    return null;
  }
  if (withdrawal.health?.status === "operator_attention") {
    return "operator_attention";
  }
  if (withdrawal.health?.status === "unavailable") {
    return withdrawal.health.lastKnownAttention
      ? "status_unavailable_after_attention"
      : "status_unavailable";
  }
  if (withdrawal.claimBlockedReason === "insufficient_liquidity") {
    return "insufficient_liquidity";
  }
  return withdrawal.claimBlockedReason ? "liquidity_unverified" : null;
}

export function earnMaxPendingMessage(pending: EarnMaxPending): string {
  return PENDING_COPY[pending].message;
}

export function earnMaxPendingSubtitle(pending: EarnMaxPending): string {
  return PENDING_COPY[pending].subtitle;
}

export function earnMaxControls(input: {
  busy: boolean;
  summary:
    | (Pick<EarnMaxSummary, "balanceUsd" | "currentOperationId"> & {
        withdrawal: Pick<WithdrawalView, "status"> | null;
      })
    | null;
}): { busy: boolean; canDeposit: boolean; canWithdraw: boolean } {
  const busy = input.busy || Boolean(input.summary?.currentOperationId);
  const canWithdraw =
    (input.summary?.balanceUsd ?? 0) > 0 &&
    !earnMaxWithdrawalPending(input.summary?.withdrawal ?? null) &&
    !busy;
  return { busy, canDeposit: !busy, canWithdraw };
}

export function formatEarnMaxApyLabel(bps: number | null): string {
  return bps === null ? "— APY" : `${(bps / 100).toFixed(2)}% APY`;
}

export function earnMaxApyFigure(
  summary: Pick<EarnMaxSummary, "forecastApyBps"> | null
): { label: string } | null {
  return summary ? { label: formatEarnMaxApyLabel(summary.forecastApyBps) } : null;
}
