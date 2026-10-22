"use client";

import {
  createSmartAccountVaultsClient,
  EarnMaxVoltrPrepareError,
  isVoltrOperationVisible,
  prepareEarnMaxVoltr,
  sendPreparedWithWallet,
  VoltrClaimLiquidityError,
  voltrClaimPlan,
  voltrDepositPlan,
  type VoltrPlanBuilder,
  voltrRequestWithdrawalPlan,
} from "@loyal-labs/smart-account-vaults";
import { useConnection, useWallet } from "@solana/wallet-adapter-react";
import { PublicKey } from "@solana/web3.js";
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
import { readJson, viewModel, walletBridge } from "./use-earn-max";

export function useEarnMaxVoltr(input: {
  settingsPda: string | null | undefined;
  walletAddress: string | null;
}): { actions: EarnMaxActions; view: EarnMaxViewModel } {
  const { connection } = useConnection();
  const wallet = useWallet();
  const [summary, setSummary] = useState<EarnMaxSummaryResponse | null>(null);
  const [activity, setActivity] = useState<EarnMaxActivityResponse | null>(
    null
  );
  const [isLoading, setIsLoading] = useState(true);
  const [isBusy, setIsBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

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
    >()
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
      setIsLoading(false);
      return;
    }
    try {
      const [nextSummary, nextActivity] = await Promise.all([
        readJson<EarnMaxSummaryResponse>(
          "/api/smart-accounts/earn-max/summary"
        ),
        readJson<EarnMaxActivityResponse>(
          "/api/smart-accounts/earn-max/activity"
        ),
      ]);
      if (accountRef.current !== account) return;
      setSummary(nextSummary);
      setActivity(nextActivity);
      setError(null);
      for (const [signature, item] of pendingVisibility.current) {
        if (
          item.account !== account ||
          isVoltrOperationVisible(
            signature,
            nextSummary.summary,
            nextActivity.operations
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
          : "Earn MAX failed to load."
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
      variant: "claim" | "deposit" | "request_withdrawal",
      build: VoltrPlanBuilder
    ) => {
      setIsBusy(true);
      setError(null);
      const tracker = createBrowserLifecycleTracker({
        flowName: "earn_max.vault",
        flowVariant: variant,
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
          confirm: true,
          connection,
          prepared,
          wallet: walletBridge(wallet),
        });
        tracker.observe("wallet_submit_confirm", { chainState: "confirmed" });
        const account = accountRef.current;
        pendingVisibility.current.set(signature, {
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
        return true;
      } catch (nextError) {
        if (nextError instanceof VoltrClaimLiquidityError) {
          tracker.fail("prepare", {
            chainState: "not_submitted",
            errorCode: "earn_max_liquidity_unavailable",
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
            : "Earn MAX transaction failed."
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
    ]
  );

  const actions = useMemo<EarnMaxActions>(
    () => ({
      refresh,
      install: async () => true,
      close: async () => true,
      cancelWithdrawal: async () => false,
      deposit: (amountRaw) =>
        run("deposit", async (context) => voltrDepositPlan(context, amountRaw)),
      requestWithdrawal: (amountRaw) =>
        run("request_withdrawal", (context) =>
          voltrRequestWithdrawalPlan(connection, context, amountRaw)
        ),
      claim: () =>
        run("claim", (context) => voltrClaimPlan(connection, context)),
    }),
    [connection, refresh, run]
  );

  return {
    actions,
    view: viewModel({
      activity,
      busy: isBusy,
      error,
      loading: isLoading,
      summary,
    }),
  };
}
