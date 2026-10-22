import { timingSafeEqual } from "node:crypto";
import { NextResponse } from "next/server";

import { reportFirstAutodepositSweepQuestCompletion } from "@/features/solana-week/server/quest-completion-service";
import { getOptionalEnv } from "@/lib/core/config/shared";
import {
  autodepositSweepExecutedPush,
  autodepositSweepFailedPush,
  autodepositSweepScheduledPush,
  sendWalletPush,
} from "@/lib/push-notifications/wallet-push.server";
import { claimPushCampaignSend } from "@/lib/push-notifications/push-campaign-send-log.server";

// Internal backend-to-backend endpoint the autodeposit sweep worker calls the
// moment it records a confirmed sweep, so Quest 2 ("first Earn deposit via
// autodeposit") is reported in real time instead of waiting for the cron, and
// the wallet's devices get the transactional push (ASK-1651).
// Authenticated with SOLANA_WEEK_NOTIFY_SECRET (Bearer).
// Body: { walletAddress, kind?: "scheduled" | "executed", amountRaw?: string }.
// `kind` defaults to "executed" (the only event the worker sent historically);
// "scheduled" and "failed" are push-only and skip quest reporting. A failed
// sweep is retried every cycle, so failures carry `dedupeKey` (the scheduled
// slot id) and the sent-log keeps the push at-most-once per sweep (ASK-2091).
function isAuthorized(request: Request): boolean {
  const secret = getOptionalEnv(process.env, "SOLANA_WEEK_NOTIFY_SECRET");
  if (!secret) {
    return false;
  }
  const header = request.headers.get("authorization");
  if (!header) {
    return false;
  }
  const expected = Buffer.from(`Bearer ${secret}`);
  const provided = Buffer.from(header);
  return (
    expected.length === provided.length && timingSafeEqual(expected, provided)
  );
}

export async function POST(request: Request) {
  if (!isAuthorized(request)) {
    return NextResponse.json({ error: "unauthorized" }, { status: 401 });
  }

  let body: unknown;
  try {
    body = await request.json();
  } catch {
    return NextResponse.json({ error: "invalid_request" }, { status: 400 });
  }

  const record =
    typeof body === "object" && body !== null
      ? (body as Record<string, unknown>)
      : {};
  const walletAddress = record.walletAddress;
  if (typeof walletAddress !== "string" || !walletAddress.trim()) {
    return NextResponse.json(
      { error: "invalid_request", message: "walletAddress is required." },
      { status: 400 }
    );
  }

  const kind =
    record.kind === "scheduled" || record.kind === "failed"
      ? record.kind
      : "executed";
  const amountRaw = parseAmountRaw(record.amountRaw);

  if (kind === "scheduled") {
    await sendWalletPush(
      walletAddress,
      autodepositSweepScheduledPush(amountRaw)
    );
    return NextResponse.json({ status: "accepted" });
  }

  if (kind === "failed") {
    if (!(await claimFailurePush(walletAddress, record.dedupeKey))) {
      return NextResponse.json({ status: "deduped" });
    }
    await sendWalletPush(walletAddress, autodepositSweepFailedPush(amountRaw));
    return NextResponse.json({ status: "accepted" });
  }

  await reportFirstAutodepositSweepQuestCompletion(walletAddress, {
    source: "sweep-worker-notify",
  });

  await sendWalletPush(walletAddress, autodepositSweepExecutedPush(amountRaw));

  return NextResponse.json({ status: "accepted" });
}

// Without a `dedupeKey` the campaign falls back to a per-day key, so an older
// worker build can still only wake the user once a day.
async function claimFailurePush(
  walletAddress: string,
  rawDedupeKey: unknown
): Promise<boolean> {
  const dedupeKey =
    typeof rawDedupeKey === "string" && rawDedupeKey.trim()
      ? rawDedupeKey.trim().slice(0, 120)
      : new Date().toISOString().slice(0, 10);
  try {
    return await claimPushCampaignSend({
      campaign: `autodeposit-failed:${dedupeKey}`,
      walletAddress,
    });
  } catch (error) {
    console.warn("[sweep-notify] failure sent-log write failed", {
      errorMessage:
        error instanceof Error ? error.message : "Unknown write error.",
      walletAddress,
    });
    return true;
  }
}

function parseAmountRaw(value: unknown): bigint | null {
  if (typeof value !== "string" || !/^\d+$/.test(value)) {
    return null;
  }
  const parsed = BigInt(value);
  return parsed > BigInt(0) ? parsed : null;
}
