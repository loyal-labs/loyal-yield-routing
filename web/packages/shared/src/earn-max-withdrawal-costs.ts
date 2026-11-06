export type EarnMaxWithdrawalCostStatus = {
  approvedCostUsdcRaw: string;
  settledCostUsdcRaw: string | null;
  reservedCostUsdcRaw: string | null;
  remainingCostUsdcRaw: string | null;
  approvedClaimFeeLamports: string;
  spentClaimFeeLamports: string | null;
  reservedClaimFeeLamports: string | null;
  remainingClaimFeeLamports: string | null;
  claimPending: boolean | null;
  unknownReason?: string;
};

type ApprovedWithdrawalCost = {
  referenceUsdcRaw: string;
  minClaimUsdcRaw: string;
  maxUserFeeLamports: string;
};

function rawAmount(raw: string | null | undefined): bigint | null {
  return raw !== null && raw !== undefined && /^(0|[1-9]\d*)$/.test(raw)
    ? BigInt(raw)
    : null;
}

export function formatEarnMaxWithdrawalAmount(
  raw: string | null | undefined,
  unit: "USDC" | "SOL",
): string | null {
  const amount = rawAmount(raw);
  if (amount === null) return null;
  const decimals = unit === "USDC" ? 6 : 9;
  const scale = BigInt(10) ** BigInt(decimals);
  const fraction = (amount % scale)
    .toString()
    .padStart(decimals, "0")
    .replace(/0+$/, "");
  return `${amount / scale}${fraction ? `.${fraction}` : ""} ${unit}`;
}

// This is a display projection, not admission or a second expense ledger.
export function earnMaxWithdrawalCostView(input: {
  status: EarnMaxWithdrawalCostStatus | null;
  approved: ApprovedWithdrawalCost | null;
}) {
  const { status, approved } = input;
  const reference = rawAmount(approved?.referenceUsdcRaw);
  const floor = rawAmount(approved?.minClaimUsdcRaw);
  const budget =
    reference !== null && floor !== null && reference >= floor
      ? (reference - floor).toString()
      : null;
  const amounts: {
    label: string;
    raw: string | null | undefined;
    unit: "USDC" | "SOL";
  }[] = [
    {
      label: "Withdrawal cost limit",
      raw: status?.approvedCostUsdcRaw ?? budget,
      unit: "USDC",
    },
    { label: "Costs incurred", raw: status?.settledCostUsdcRaw, unit: "USDC" },
    {
      label: "Pending cost allowance",
      raw: status?.reservedCostUsdcRaw,
      unit: "USDC",
    },
    {
      label: "Remaining cost allowance",
      raw: status?.remainingCostUsdcRaw,
      unit: "USDC",
    },
    {
      label: "Claim fee limit",
      raw: status?.approvedClaimFeeLamports ?? approved?.maxUserFeeLamports,
      unit: "SOL",
    },
    {
      label: "Claim fees paid",
      raw: status?.spentClaimFeeLamports,
      unit: "SOL",
    },
    {
      label: "Pending claim fees",
      raw: status?.reservedClaimFeeLamports,
      unit: "SOL",
    },
    {
      label: "Remaining claim fees",
      raw: status?.remainingClaimFeeLamports,
      unit: "SOL",
    },
  ];
  return {
    rows: amounts.map(({ label, raw, unit }) => ({
      label,
      value: formatEarnMaxWithdrawalAmount(raw, unit),
      unit,
    })),
    pending: status?.claimPending ?? null,
    unknownReason:
      status?.unknownReason ?? (status ? null : "cost_status_unavailable"),
  };
}
