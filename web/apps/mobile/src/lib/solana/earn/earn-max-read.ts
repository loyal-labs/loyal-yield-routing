import type { EarnMaxActivity, EarnMaxSummaryResponse } from "@loyal-labs/api";

import type { Signer } from "@/lib/wallet/signer";

import { deliberateSession } from "./earn-session";
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
  | { kind: "session_required"; notice: string | null }
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
    if (error instanceof EarnMaxError && error.code === "session_required") {
      return { kind: "session_required", notice: null };
    }
    return {
      kind: "failed",
      message:
        error instanceof Error ? error.message : "Earn MAX failed to load.",
    };
  }
}

export async function connectEarnMax(
  walletAddress: string,
  signer: Signer,
): Promise<EarnMaxRead> {
  const token = await deliberateSession(signer);
  if (!token) {
    return {
      kind: "session_required",
      notice: "Could not sign in. Try again in a minute.",
    };
  }
  return readEarnMax(walletAddress);
}
