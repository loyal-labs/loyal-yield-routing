"use client";

import {
  createSmartAccountVaultsClient,
  EarnMaxVoltrPrepareError,
  fetchEarnMaxDepositQuote,
  isVoltrOperationVisible,
  prepareEarnMaxVoltr,
  sendPreparedWithWallet,
  VoltrClaimHeldError,
  voltrClaimHeld,
  VoltrClaimLiquidityError,
  voltrClaimPlan,
  voltrDepositPlan,
  type VoltrPlanBuilder,
  voltrRequestWithdrawalPlan,
  voltrWithdrawalConsentMessage,
  voltrRecoverClaimRemainderPlan,
  type WalletAdapterLike,
} from "@loyal-labs/smart-account-vaults";
import { earnMaxControls } from "@loyal-labs/shared";
import { useConnection, useWallet } from "@solana/wallet-adapter-react";
import { PublicKey } from "@solana/web3.js";
import { Buffer } from "buffer";
import { z } from "zod";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";

import { isWalletCancellation } from "@/components/wallet-workspace/facelift/earn-actions-support";
import { createBrowserLifecycleTracker } from "@/features/observability/client";
import type { LifecycleTracker } from "@/features/observability/lifecycle-contract";
import { classifyTransactionFailure } from "@/features/observability/transaction-failure";

import type {
  EarnMaxActions,
  EarnMaxActivityResponse,
  EarnMaxSummaryResponse,
  EarnMaxViewModel,
} from "./types";

const consentSchema = z.object({
  policySeed: z.string(),
  authority: z.string(),
  cluster: z.string(),
  destination: z.string(),
  expiresAt: z.string(),
  feeBasisHash: z.string(),
  lpRaw: z.string(),
  maxUserFeeLamports: z.string(),
  minClaimUsdcRaw: z.string(),
  nonce: z.string(),
  owner: z.string(),
  ownerSignature: z.string(),
  program: z.string(),
  referenceSlot: z.string(),
  referenceUsdcRaw: z.string(),
  requestInstruction: z.number().int().nonnegative(),
  requestSignature: z.string(),
  settings: z.string(),
  vault: z.string(),
  version: z.string(),
});
const approvalSchema = z.object({
  costStatus: z.object({
    approvedCostUsdcRaw:z.string(),settledCostUsdcRaw:z.string().nullable(),reservedCostUsdcRaw:z.string().nullable(),remainingCostUsdcRaw:z.string().nullable(),
    approvedClaimFeeLamports:z.string(),spentClaimFeeLamports:z.string().nullable(),reservedClaimFeeLamports:z.string().nullable(),remainingClaimFeeLamports:z.string().nullable(),claimPending:z.boolean().nullable(),unknownReason:z.string().optional(),
  }).nullable(),
  draft: consentSchema,
  approved: consentSchema.nullable(),
  reason: z.string().optional(),
});
const consentPath = "/api/smart-accounts/earn-max/withdrawal-consent";
type Approval = z.infer<typeof approvalSchema>;

export async function readJson<T>(path: string): Promise<T> {
  const response = await fetch(path, {
    cache: "no-store",
    credentials: "include",
  });
  const body = (await response.json()) as T;
  if (!response.ok) {
    throw new Error(`Earn MAX request failed (${response.status}).`);
  }
  return body;
}

export function walletBridge(
  wallet: ReturnType<typeof useWallet>,
): WalletAdapterLike {
  if (!wallet.publicKey || !wallet.signTransaction) {
    throw new Error("Connected wallet cannot sign Earn MAX transactions.");
  }
  return {
    publicKey: wallet.publicKey,
    signTransaction: wallet.signTransaction,
    ...(wallet.signAllTransactions
      ? { signAllTransactions: wallet.signAllTransactions }
      : {}),
    ...(wallet.sendTransaction
      ? { sendTransaction: wallet.sendTransaction }
      : {}),
  };
}

export function viewModel(input: {
  activity: EarnMaxActivityResponse | null;
  busy: boolean;
  error: string | null;
  loading: boolean;
  summary: EarnMaxSummaryResponse | null;
}): EarnMaxViewModel {
  const summary = input.summary?.summary;
  const controls = earnMaxControls({
    busy: input.busy,
    summary: summary ?? null,
  });
  const strategyLabels: Record<string, string> = {
    onyc_usdc: "ONyc / USDC",
    onyc_usds: "ONyc / USDS",
    prime_usdc: "PRIME / USDC",
    prime_pyusd: "PRIME / PYUSD",
    prime_usds: "PRIME / USDS",
    syrup_usdc_usdc: "syrupUSDC / USDC",
    syrup_usdc_pyusd: "syrupUSDC / PYUSD",
  };
  return {
    recoverableUsdcRaw: summary && "recoverableUsdcRaw" in summary && typeof summary.recoverableUsdcRaw === "string" ? summary.recoverableUsdcRaw : null,
    activity: input.activity?.operations ?? [],
    balanceUsd: summary?.balanceUsd ?? 0,
    canWithdraw: controls.canWithdraw,
    coverage: summary?.coverage ?? "history_incomplete",
    earnedUsd: summary?.earnedUsd ?? null,
    earnings: input.activity?.earnings ?? null,
    error: input.error,
    forecastApyBps: summary?.forecastApyBps ?? null,
    isBusy: controls.busy,
    isLoading: input.loading,
    issuanceFeeBps: summary?.issuanceFeeBps ?? 0,
    performance: input.activity?.performance ?? [],
    policyStatus: summary?.policyStatus ?? null,
    realizedApyBps: summary?.realizedApyBps ?? null,
    status: summary?.goal ?? "not_installed",
    strategyLabel:
      (summary?.strategyKey && strategyLabels[summary.strategyKey]) ??
      "Earn MAX",
    withdrawal: summary?.withdrawal ?? null,
  };
}

export function useEarnMaxVoltr(input: {
  settingsPda: string | null | undefined;
  walletAddress: string | null;
}): { actions: EarnMaxActions; view: EarnMaxViewModel } {
  const { connection } = useConnection();
  const wallet = useWallet();
  const [summary, setSummary] = useState<EarnMaxSummaryResponse | null>(null);
  const [activity, setActivity] = useState<EarnMaxActivityResponse | null>(
    null,
  );
  const [isLoading, setIsLoading] = useState(true);
  const [isBusy, setIsBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [approval, setApproval] = useState<Approval | null>(null);

  // The account a response belongs to; a response for an account the user
  // switched away from mid-request is dropped.
  const accountRef = useRef<string | null>(null);
  accountRef.current =
    input.settingsPda && input.walletAddress
      ? `${input.walletAddress}:${input.settingsPda}`
      : null;

  const pendingVisibility = useRef(
    new Map<
      string,
      {
        account: string | null;
        tracker: LifecycleTracker;
        timer: ReturnType<typeof setTimeout>;
      }
    >(),
  );
  useEffect(() => {
    const pending = pendingVisibility.current;
    return () => {
      for (const item of pending.values()) clearTimeout(item.timer);
      pending.clear();
    };
  }, []);

  const refresh = useCallback(async () => {
    const account = accountRef.current;
    if (!(input.settingsPda && input.walletAddress)) {
      // Signed out, switched wallet, or not unlocked: never keep showing the
      // previous account's position.
      setSummary(null);
      setActivity(null);
      setApproval(null);
      setIsLoading(false);
      return;
    }
    try {
      const [nextSummary, nextActivity] = await Promise.all([
        readJson<EarnMaxSummaryResponse>(
          "/api/smart-accounts/earn-max/summary",
        ),
        readJson<EarnMaxActivityResponse>(
          "/api/smart-accounts/earn-max/activity",
        ),
      ]);
      if (accountRef.current !== account) return;
      setSummary(nextSummary);
      setActivity(nextActivity);
      if (nextSummary.summary?.withdrawal) {
        try {
          const nextApproval = approvalSchema.parse(
            await readJson<unknown>(consentPath),
          );
          if (accountRef.current !== account) return;
          setApproval(nextApproval);
        } catch {
          if (accountRef.current !== account) return;
          setApproval(null);
        }
      } else setApproval(null);
      setError(null);
      for (const [signature, item] of pendingVisibility.current) {
        if (
          item.account !== account ||
          isVoltrOperationVisible(
            signature,
            nextSummary.summary,
            nextActivity.operations,
          )
        ) {
          clearTimeout(item.timer);
          if (item.account === account)
            item.tracker.complete("position_visible", {
              chainState: "confirmed",
            });
          pendingVisibility.current.delete(signature);
        }
      }
    } catch (nextError) {
      if (accountRef.current !== account) return;
      setSummary(null);
      setError(
        nextError instanceof Error
          ? nextError.message
          : "Earn MAX failed to load.",
      );
    } finally {
      setIsLoading(false);
    }
  }, [input.settingsPda, input.walletAddress]);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  const readyBy = summary?.summary?.withdrawal?.readyBy;
  const canClaim = summary?.summary?.withdrawal?.canClaim;
  useEffect(() => {
    if (!readyBy || canClaim) return;
    const delay = Math.max(0, Date.parse(readyBy) - Date.now()) + 2_000;
    const timer = setTimeout(() => void refresh(), delay);
    return () => clearTimeout(timer);
  }, [canClaim, readyBy, refresh]);

  const run = useCallback(
    async (
      variant: "claim" | "deposit" | "request_withdrawal" | "recover_assets",
      build: VoltrPlanBuilder,
    ) => {
      setIsBusy(true);
      setError(null);
      const tracker = createBrowserLifecycleTracker({
        flowName: "earn_max.vault",
        flowVariant: variant === "recover_assets" ? "claim" : variant,
      });
      tracker.start("intent");
      let stage: "prepare" | "wallet_submit_confirm" = "prepare";
      try {
        const programId = summary?.config.programId;
        if (
          !(wallet.publicKey && input.settingsPda && programId) ||
          wallet.publicKey.toBase58() !== input.walletAddress
        ) {
          tracker.fail("prepare", {
            chainState: "not_submitted",
            errorCode: "wallet_unavailable",
          });
          throw new Error("Connect the authenticated wallet to use Earn MAX.");
        }
        const owner = wallet.publicKey;
        const settingsPda = new PublicKey(input.settingsPda);
        const smartAccountProgramId = new PublicKey(programId);
        const client = createSmartAccountVaultsClient({
          connection,
          programId: smartAccountProgramId,
        });
        const prepared = await prepareEarnMaxVoltr({
          connection,
          build,
          client,
          owner,
          programId: smartAccountProgramId,
          settingsPda,
        }).catch((prepareError: unknown) => {
          if (
            prepareError instanceof EarnMaxVoltrPrepareError &&
            prepareError.code === "transaction_too_large"
          ) {
            tracker.fail("prepare", {
              chainState: "not_submitted",
              errorCode: "transaction_too_large",
            });
          }
          throw prepareError;
        });
        stage = "wallet_submit_confirm";
        const signature = await sendPreparedWithWallet({
          beforeProtectedClaimBroadcast: async (claim) => {
            const response = await fetch("/api/smart-accounts/earn-max/withdrawal-claim-attempt", { method: "POST", credentials: "include", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ ...claim, wire: undefined, wireBase64: Buffer.from(claim.wire).toString("base64") }) });
            if (!response.ok) {
              const failure = z.object({ error: z.object({ code: z.string(), message: z.string().optional() }) }).safeParse(await response.json());
              const held = failure.success ? voltrClaimHeld(failure.data.error.code) : null;
              if (held) throw held;
              throw new Error(failure.success ? failure.data.error.message ?? "This withdrawal claim could not be reserved. Check status before signing another transaction." : "This withdrawal claim could not be reserved. Check status before signing another transaction.");
            }
          },
          confirm: true,
          connection,
          prepared,
          wallet: walletBridge(wallet),
        });
        tracker.observe("wallet_submit_confirm", { chainState: "confirmed" });
        const account = accountRef.current;
        if (variant !== "recover_assets") pendingVisibility.current.set(signature, {
          account,
          tracker,
          timer: setTimeout(() => {
            if (
              accountRef.current === account &&
              pendingVisibility.current.has(signature)
            )
              tracker.timeout("position_visible", {
                chainState: "confirmed",
                errorCode: "realtime_unresolved",
              });
          }, 60_000),
        });
        tracker.observe("ui_commit", { chainState: "confirmed" });
        await refresh();
        if (variant === "recover_assets") tracker.complete("position_visible", { chainState: "confirmed" });
        return true;
      } catch (nextError) {
        if (nextError instanceof VoltrClaimLiquidityError) {
          tracker.fail("prepare", {
            chainState: "not_submitted",
            errorCode: "earn_max_liquidity_unavailable",
          });
          await refresh();
        } else if (nextError instanceof VoltrClaimHeldError) {
          tracker.fail(stage, {
            chainState: "not_submitted",
            errorCode: "state_not_ready",
          });
          await refresh();
        } else if (
          stage === "wallet_submit_confirm" &&
          isWalletCancellation(nextError)
        ) {
          tracker.cancel(stage, {
            chainState: "not_submitted",
            errorCode: "wallet_rejected",
          });
        } else {
          tracker.fail(stage, {
            errorCode:
              stage === "prepare"
                ? "instruction_fetch_failed"
                : classifyTransactionFailure(nextError),
          });
        }
        setError(
          nextError instanceof Error
            ? nextError.message
            : "Earn MAX transaction failed.",
        );
        return false;
      } finally {
        setIsBusy(false);
      }
    },
    [
      connection,
      input.settingsPda,
      input.walletAddress,
      refresh,
      summary?.config.programId,
      wallet,
    ],
  );

  const actions = useMemo<EarnMaxActions>(
    () => ({
      approveWithdrawal: async (minClaimRaw, maxFeeLamports) => {
        if (isBusy || !wallet.signMessage) {
          setError("Connect a wallet that can sign withdrawal approvals.");
          return false;
        }
        const account = accountRef.current;
        setIsBusy(true);
        try {
          const latest = approvalSchema.parse(
            await readJson<unknown>(consentPath),
          );
          if (
            account !== accountRef.current ||
            latest.draft.owner !== wallet.publicKey?.toBase58()
          )
            throw new Error("Wallet changed. Review again.");
          if (
            minClaimRaw <= BigInt(0) ||
            minClaimRaw > BigInt(latest.draft.referenceUsdcRaw) ||
            maxFeeLamports < BigInt(0)
          )
            throw new Error(
              "Enter a positive minimum no higher than the current withdrawal value.",
            );
          const consent = {
            ...latest.draft,
            minClaimUsdcRaw: minClaimRaw.toString(),
            maxUserFeeLamports: maxFeeLamports.toString(),
          };
          const signature = await wallet.signMessage(
            voltrWithdrawalConsentMessage(consent),
          );
          if (account !== accountRef.current)
            throw new Error("Wallet changed. Review again.");
          const response = await fetch(consentPath, {
            method: "POST",
            credentials: "include",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({
              ...consent,
              ownerSignature: Buffer.from(signature).toString("base64"),
            }),
          });
          if (!response.ok)
            throw new Error(
              "Approval could not be saved. Refresh and approve again.",
            );
          await refresh();
          return true;
        } catch (error) {
          setError(error instanceof Error ? error.message : "Approval failed.");
          return false;
        } finally {
          setIsBusy(false);
        }
      },
      refresh,
      install: async () => true,
      close: async () => true,
      cancelWithdrawal: async () => false,
      deposit: (amountRaw) =>
        run("deposit", async (context) =>
          voltrDepositPlan(
            context,
            await fetchEarnMaxDepositQuote(
              (path, init) => fetch(path, { ...init, credentials: "include" }),
              amountRaw,
            ),
          ),
        ),
      requestWithdrawal: (amountRaw) =>
        run("request_withdrawal", (context) =>
          voltrRequestWithdrawalPlan(connection, context, amountRaw),
        ),
      claim: () =>
        run("claim", async (context) => {
          const latest = approvalSchema.parse(
            await readJson<unknown>(consentPath),
          );
          return voltrClaimPlan(
            connection,
            context,
            latest.approved ?? undefined,
          );
        }),
      recoverClaimRemainder: () => run("recover_assets", (context) => voltrRecoverClaimRemainderPlan(connection, context)),
    }),
    [connection, isBusy, refresh, run, wallet],
  );

  return {
    actions,
    view: {
      ...viewModel({
        activity,
        busy: isBusy,
        error,
        loading: isLoading,
        summary,
      }),
      ...(summary?.summary?.withdrawal
        ? {
            withdrawalApproval: {
              costStatus: approval?.costStatus ?? null,
              approvedReferenceRaw: approval?.approved?.referenceUsdcRaw ?? null,
              approvedFeeLamports: approval?.approved?.maxUserFeeLamports ?? null,
              generationKey: approval ? `${approval.draft.authority}:${approval.draft.requestSignature}:${approval.draft.requestInstruction}:${approval.draft.policySeed}` : undefined,
              floorRaw: approval?.approved?.minClaimUsdcRaw ?? null,
              referenceRaw:
                approval?.draft.referenceUsdcRaw ??
                  summary.summary?.withdrawal?.amountRaw ?? "0",
              reason:
                approval?.reason ??
                (approval?.approved ? null : "needs_consent"),
            },
          }
        : {}),
    },
  };
}
