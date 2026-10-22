import type {
  EarnMaxActivity,
  EarnMaxSummaryResponse,
} from "@loyal-labs/api";

import {
  EarnMaxError,
  fetchEarnMaxActivity,
  fetchEarnMaxInvite,
  fetchEarnMaxSummary,
} from "./earn-max";

export type EarnMaxRead =
  | { kind: "invite_required" }
  | {
      kind: "ready";
      summary: EarnMaxSummaryResponse;
      activity: EarnMaxActivity;
    }
  | { kind: "failed"; message: string };

export async function readEarnMax(walletAddress: string): Promise<EarnMaxRead> {
  try {
    const invite = await fetchEarnMaxInvite(walletAddress);
    if (!invite.redeemed) {
      return { kind: "invite_required" };
    }
    const [summary, activity] = await Promise.all([
      fetchEarnMaxSummary(walletAddress),
      fetchEarnMaxActivity(walletAddress),
    ]);
    return { kind: "ready", summary, activity };
  } catch (error) {
    if (error instanceof EarnMaxError && error.code === "invite_required") {
      return { kind: "invite_required" };
    }
    return {
      kind: "failed",
      message:
        error instanceof Error ? error.message : "Earn MAX failed to load.",
    };
  }
}
