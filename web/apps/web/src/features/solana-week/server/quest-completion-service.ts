import "server-only";

import { pushTokens } from "@loyal-labs/db-core/schema";
import { and, eq, gte, inArray, ne, notExists, sql } from "drizzle-orm";

import { getDatabase } from "@/lib/core/database";
import {
  quest1DonePush,
  sendWalletPush,
} from "@/lib/push-notifications/wallet-push.server";
import { findWalletAddressesWithBalanceSweepsSince } from "@/lib/yield-optimization/earn-autodeposit-repository.server";
import {
  getYieldOptimizationClient,
  solanaWeekQuestCompletions,
  userYieldPositionDeposits,
} from "@/lib/yield-optimization/yield-neon-client.server";

import {
  reportQuestCompletion,
  type QuestCompletionMetadata,
  type QuestCompletionResult,
  type QuestKind,
} from "./quest-completion-reporter";

type CompletionRow = typeof solanaWeekQuestCompletions.$inferSelect;

export type RecordAndReportStatus =
  | "reported"
  | "pending"
  | "failed"
  | "skipped";

function db() {
  return getYieldOptimizationClient().db;
}

async function findCompletionRow(
  walletId: string,
  kind: QuestKind
): Promise<CompletionRow | null> {
  const [row] = await db()
    .select()
    .from(solanaWeekQuestCompletions)
    .where(
      and(
        eq(solanaWeekQuestCompletions.walletAddress, walletId),
        eq(solanaWeekQuestCompletions.questKind, kind)
      )
    )
    .limit(1);
  return row ?? null;
}

async function ensureCompletionRow(
  walletId: string,
  kind: QuestKind,
  metadata: QuestCompletionMetadata | undefined
): Promise<CompletionRow> {
  await db()
    .insert(solanaWeekQuestCompletions)
    .values({
      walletAddress: walletId,
      questKind: kind,
      status: "pending",
      metadata: metadata ?? null,
    })
    .onConflictDoNothing({
      target: [
        solanaWeekQuestCompletions.walletAddress,
        solanaWeekQuestCompletions.questKind,
      ],
    });

  const row = await findCompletionRow(walletId, kind);
  if (!row) {
    throw new Error("Failed to persist Solana Week quest completion row.");
  }
  return row;
}

async function applyReportResult(
  rowId: bigint,
  result: QuestCompletionResult
): Promise<RecordAndReportStatus> {
  const now = new Date();
  const base = {
    attempts: sql`${solanaWeekQuestCompletions.attempts} + 1`,
    updatedAt: now,
  };

  switch (result.status) {
    case "completed":
    case "already_completed":
      await db()
        .update(solanaWeekQuestCompletions)
        .set({
          ...base,
          status: "reported",
          solanaStatus: result.status,
          reportedAt: now,
          lastErrorCode: null,
          lastErrorMessage: null,
        })
        .where(eq(solanaWeekQuestCompletions.id, rowId));
      return "reported";
    case "permanent_error":
      await db()
        .update(solanaWeekQuestCompletions)
        .set({
          ...base,
          status: "failed",
          lastErrorCode: result.error,
          lastErrorMessage: result.message,
        })
        .where(eq(solanaWeekQuestCompletions.id, rowId));
      return "failed";
    case "disabled":
    case "skipped":
      await db()
        .update(solanaWeekQuestCompletions)
        .set({ ...base, status: "pending", lastErrorCode: result.reason })
        .where(eq(solanaWeekQuestCompletions.id, rowId));
      return "pending";
    case "retryable_error":
      await db()
        .update(solanaWeekQuestCompletions)
        .set({ ...base, status: "pending", lastErrorCode: result.error })
        .where(eq(solanaWeekQuestCompletions.id, rowId));
      return "pending";
  }
}

export async function recordAndReportQuestCompletion(args: {
  walletId: string;
  kind: QuestKind;
  metadata?: QuestCompletionMetadata;
}): Promise<RecordAndReportStatus> {
  const walletId = args.walletId.trim();
  if (!walletId) {
    return "skipped";
  }

  const row = await ensureCompletionRow(walletId, args.kind, args.metadata);
  if (row.status === "reported") {
    return "reported";
  }

  const result = await reportQuestCompletion();
  const status = await applyReportResult(row.id, result);
  if (status === "reported" && args.kind === "earn_deposit") {
    await sendWalletPush(walletId, quest1DonePush());
  }
  return status;
}

async function reportBestEffort(
  kind: QuestKind,
  walletId: string,
  metadata: QuestCompletionMetadata | undefined
): Promise<void> {
  try {
    const status = await recordAndReportQuestCompletion({
      kind,
      walletId,
      metadata,
    });
    if (status === "failed") {
      console.error("[solana-week] quest completion permanently failed", {
        kind,
        walletId,
      });
    }
  } catch (error) {
    console.error("[solana-week] quest completion threw", {
      kind,
      walletId,
      errorName: error instanceof Error ? error.name : typeof error,
      errorMessage:
        error instanceof Error ? error.message : "Unknown reporting error.",
    });
  }
}

/**
 * Minimum qualifying deposit for Quest 1: $5 USDC (6 decimals). Deposits below
 * this don't earn the badge, so we neither create a completion row nor report.
 */
export const MIN_EARN_DEPOSIT_QUEST_USDC_RAW = BigInt(5_000_000);

export function reportEarnDepositQuestCompletion(
  walletId: string,
  depositedUsdcRaw: bigint,
  metadata?: QuestCompletionMetadata
): Promise<void> {
  if (depositedUsdcRaw < MIN_EARN_DEPOSIT_QUEST_USDC_RAW) {
    return Promise.resolve();
  }
  return reportBestEffort("earn_deposit", walletId, metadata);
}

export function reportFirstAutodepositSweepQuestCompletion(
  walletId: string,
  metadata?: QuestCompletionMetadata
): Promise<void> {
  return reportBestEffort("first_autodeposit_sweep", walletId, metadata);
}

export type ReconcileSummary = {
  retriedRows: number;
  backfilledWallets: number;
  backfilledDepositWallets: number;
  reported: number;
  stillPending: number;
  failed: number;
};

const RETRY_BATCH_LIMIT = 500;

async function findQualifyingDepositWalletsWithoutQuestRowSince(
  since: Date
): Promise<string[]> {
  const rows = await db()
    .selectDistinct({ wallet: userYieldPositionDeposits.walletAddress })
    .from(userYieldPositionDeposits)
    .where(
      and(
        gte(userYieldPositionDeposits.createdAt, since),
        gte(
          userYieldPositionDeposits.principalAmountRaw,
          MIN_EARN_DEPOSIT_QUEST_USDC_RAW
        ),
        notExists(
          db()
            .select({ one: sql`1` })
            .from(solanaWeekQuestCompletions)
            .where(
              and(
                eq(
                  solanaWeekQuestCompletions.walletAddress,
                  userYieldPositionDeposits.walletAddress
                ),
                eq(solanaWeekQuestCompletions.questKind, "earn_deposit")
              )
            )
        )
      )
    );
  return rows.map((row) => row.wallet);
}

// Quest 1 is mobile-only attribution and deposits don't record their surface;
// a push-token registration (app DB) is the mobile discriminator, so web
// deposits stay excluded by design.
async function filterToMobileWallets(wallets: string[]): Promise<string[]> {
  if (wallets.length === 0) {
    return [];
  }
  const rows = await getDatabase()
    .selectDistinct({ wallet: pushTokens.walletPublicKey })
    .from(pushTokens)
    .where(inArray(pushTokens.walletPublicKey, wallets));
  return rows
    .map((row) => row.wallet)
    .filter((wallet): wallet is string => Boolean(wallet));
}

export async function reconcileQuestCompletions(args: {
  sweepLookbackMs: number;
}): Promise<ReconcileSummary> {
  const summary: ReconcileSummary = {
    retriedRows: 0,
    backfilledWallets: 0,
    backfilledDepositWallets: 0,
    reported: 0,
    stillPending: 0,
    failed: 0,
  };

  const tally = (status: RecordAndReportStatus) => {
    if (status === "reported") {
      summary.reported += 1;
    } else if (status === "failed") {
      summary.failed += 1;
    } else if (status === "pending") {
      summary.stillPending += 1;
    }
  };

  const pendingRows = await db()
    .select()
    .from(solanaWeekQuestCompletions)
    .where(ne(solanaWeekQuestCompletions.status, "reported"))
    .limit(RETRY_BATCH_LIMIT);

  for (const row of pendingRows) {
    summary.retriedRows += 1;
    try {
      const result = await reportQuestCompletion();
      tally(await applyReportResult(row.id, result));
    } catch (error) {
      summary.stillPending += 1;
      console.error("[solana-week] reconcile retry threw", {
        rowId: row.id.toString(),
        errorMessage:
          error instanceof Error ? error.message : "Unknown retry error.",
      });
    }
  }

  const since = new Date(Date.now() - args.sweepLookbackMs);
  const sweptWallets = await findWalletAddressesWithBalanceSweepsSince(since);
  for (const wallet of sweptWallets) {
    summary.backfilledWallets += 1;
    try {
      tally(
        await recordAndReportQuestCompletion({
          kind: "first_autodeposit_sweep",
          walletId: wallet,
          metadata: { source: "cron:reconcile-backfill" },
        })
      );
    } catch (error) {
      summary.stillPending += 1;
      console.error("[solana-week] reconcile backfill threw", {
        wallet,
        errorMessage:
          error instanceof Error ? error.message : "Unknown backfill error.",
      });
    }
  }

  const depositWallets = await filterToMobileWallets(
    await findQualifyingDepositWalletsWithoutQuestRowSince(since)
  );
  for (const wallet of depositWallets) {
    summary.backfilledDepositWallets += 1;
    try {
      tally(
        await recordAndReportQuestCompletion({
          kind: "earn_deposit",
          walletId: wallet,
          metadata: { source: "cron:deposit-backfill" },
        })
      );
    } catch (error) {
      summary.stillPending += 1;
      console.error("[solana-week] deposit backfill threw", {
        wallet,
        errorMessage:
          error instanceof Error ? error.message : "Unknown backfill error.",
      });
    }
  }

  return summary;
}

const QUEST_KINDS: QuestKind[] = ["earn_deposit", "first_autodeposit_sweep"];

export type QuestProgressStatus = CompletionRow["status"] | "not_started";

export type QuestProgressItem = {
  kind: QuestKind;
  status: QuestProgressStatus;
  solanaStatus: CompletionRow["solanaStatus"];
  reportedAt: string | null;
  attempts: number;
};

// Persisted rows are only ever pending/reported/failed (see applyReportResult);
// anything unexpected collapses to "pending" so the UI never shows a raw value.
function normalizeRowStatus(status: string): QuestProgressStatus {
  return status === "reported" || status === "failed" ? status : "pending";
}

export async function getQuestProgress(
  walletId: string
): Promise<QuestProgressItem[]> {
  const wallet = walletId.trim();
  const rowByKind = new Map<QuestKind, CompletionRow>();
  if (wallet) {
    const rows = await db()
      .select()
      .from(solanaWeekQuestCompletions)
      .where(eq(solanaWeekQuestCompletions.walletAddress, wallet));
    for (const row of rows) {
      rowByKind.set(row.questKind, row);
    }
  }

  return QUEST_KINDS.map((kind) => {
    const row = rowByKind.get(kind);
    if (!row) {
      return {
        kind,
        status: "not_started" as const,
        solanaStatus: null,
        reportedAt: null,
        attempts: 0,
      };
    }
    return {
      kind,
      status: normalizeRowStatus(row.status),
      solanaStatus: row.solanaStatus,
      reportedAt: row.reportedAt ? row.reportedAt.toISOString() : null,
      attempts: row.attempts,
    };
  });
}
