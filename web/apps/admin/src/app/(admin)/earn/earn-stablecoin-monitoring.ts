import type { StablecoinHealthWarning } from "@/lib/earn/stablecoin-monitor.shared";
import type { SafeReserveApyStatus } from "@/lib/kamino/timescale-reserve-monitor.shared";

import type { EarnData } from "./earn-data";

export type EarnStablecoinHealthRow = EarnData["stablecoins"][number] & {
  bestSupplyApyPercent: number | null;
  eligibleReserveCount: number;
  eligibilityReason: string;
  eligibilityStatus: SafeReserveApyStatus;
  warnings: StablecoinHealthWarning[];
};
