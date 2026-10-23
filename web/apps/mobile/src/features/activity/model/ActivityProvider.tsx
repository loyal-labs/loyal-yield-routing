import {
  createContext,
  type ReactNode,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
} from "react";
import { Alert } from "react-native";
import {
  EARN_AUTODEPOSIT_MIN_VISIBLE_SCHEDULED_SWEEP_RAW,
  getVisibleEarnAutodepositScheduledSweeps,
  type EarnAutodepositProgressState,
  type LoadedEarnAutodepositScheduledSweep,
} from "@loyal-labs/shared";

import { nudgeQuestProgressCheck } from "@/components/quests/QuestCompletionWatcher";
import { useEarnActivity } from "@/hooks/wallet/useEarnActivity";
import { useEarnAutodeposit } from "@/hooks/wallet/useEarnAutodeposit";
import { useTokenHoldings } from "@/hooks/wallet/useTokenHoldings";
import { useWalletAutoRefresh } from "@/hooks/wallet/useWalletAutoRefresh";
import { useWalletTransactions } from "@/hooks/wallet/useWalletTransactions";
import {
  SOLANA_USDC_MINT_DEVNET,
  SOLANA_USDC_MINT_MAINNET,
} from "@/lib/solana/constants";
import {
  executeEarnAutodepositClose,
  executeEarnAutodepositScheduledSweep,
} from "@/lib/solana/earn/autodeposit";
import {
  EarnApiError,
  fetchEarnAutodepositSweepProgress,
  fetchEarnRefundScan,
  type EarnAutodepositState,
  type EarnRefundPrepareRequest,
  type EarnTransactionItem,
} from "@/lib/solana/earn/earn-api";
import { executeEarnRefund } from "@/lib/solana/earn/refund";
import { isScheduledSweepDue } from "@/lib/solana/earn/earn-scheduled-sweep";
import { earnTransactionTimestampMs } from "@/lib/solana/earn/earn-tx-display";
import { isWalletUnlocked, useWallet } from "@/lib/wallet/wallet-provider";
import type { LifecycleFlow } from "@/services/observability";
import { startMobileLoadingMetric } from "@/services/loading-metrics";
import type { Transaction } from "@/types/wallet";

import {
  type ActivitySection,
  getActivityLastSeen,
  setActivityLastSeen,
} from "./activity-unread";

type ActivityContextValue = {
  walletAddress: string | null;
  walletTransactions: Transaction[];
  isFetchingTransactions: boolean;
  loadWalletTransactions: (options?: { force?: boolean }) => Promise<void>;
  earnTransactions: EarnTransactionItem[];
  isFetchingEarn: boolean;
  hasLoadedEarn: boolean;
  refreshEarnTransactions: () => Promise<void>;
  autodeposit: EarnAutodepositState | null;
  autodepositLoaded: boolean;
  refreshAutodeposit: (options?: {
    throwOnError?: boolean;
  }) => Promise<EarnAutodepositState | null>;
  refreshAutodepositAndHoldings: () => Promise<unknown>;
  earnScheduledSweeps: LoadedEarnAutodepositScheduledSweep[];
  autodepositPausedMissingPosition: boolean;
  isExecutingSweep: boolean;
  executeScheduledSweep: () => Promise<void>;
  sweepMorph: SweepMorph | null;
  clearSweepMorph: () => void;
  sweepProgressState: EarnAutodepositProgressState | null;
  walletUnread: boolean;
  earnUnread: boolean;
  anyUnread: boolean;
  markSeen: (section: ActivitySection) => void;
  earnRefunds: EarnRefundItem[];
  earnLockedRefundLamports: number;
  refreshEarnRefunds: () => Promise<EarnRefundItem[]>;
  refundingAccount: string | null;
  executeRefund: (item: EarnRefundItem) => Promise<void>;
  /**
   * The locked-rent row can offer "Delete Autodeposit": the Autodeposit is
   * paused (not running, not mid-setup) and both close accounts are known.
   */
  canDeleteLockedRefundAutodeposit: boolean;
  deletingAutodeposit: boolean;
  deleteAutodepositForRefund: () => void;
};

export type EarnRefundItem = {
  kind: "policy" | "recurring_delegation" | "vault";
  account: string;
  lamports: number;
};

export type SweepMorph = {
  sweeps: LoadedEarnAutodepositScheduledSweep[];
  scheduledSlotId: string;
  resultTx: EarnTransactionItem | null;
  baselineTxIds: ReadonlySet<string>;
  startedAtMs: number;
};

const ActivityContext = createContext<ActivityContextValue | null>(null);

const isBalanceSweepTx = (tx: EarnTransactionItem): boolean =>
  tx.kind === "balance_sweep" || tx.eventType === "balance_sweep";

const DUE_SWEEP_POLL_MS = 2000;
const DUE_SWEEP_POLL_BUDGET_MS = 40_000;
const MAX_DUE_SWEEP_POLLS = DUE_SWEEP_POLL_BUDGET_MS / DUE_SWEEP_POLL_MS;

const SWEEP_RESULT_POLL_MS = 2500;
const SWEEP_RESULT_POLL_BUDGET_MS = 60_000;
const MAX_SWEEP_RESULT_POLLS =
  SWEEP_RESULT_POLL_BUDGET_MS / SWEEP_RESULT_POLL_MS;
const SWEEP_MORPH_MAX_PINNED_MS = 120_000;

const SWEEP_PROGRESS_POLL_MS = 1000;
const MAX_SWEEP_PROGRESS_POLLS = 45;
const MAX_SWEEP_PROGRESS_MISSES = 3;

const SWEEP_PROGRESS_END_STATES = new Set<EarnAutodepositProgressState>([
  "completed",
  "failed",
  "canceled",
  "released",
]);

export function ActivityProvider({ children }: { children: ReactNode }) {
  const { publicKey, signer, state } = useWallet();
  const { walletTransactions, isFetchingTransactions, loadWalletTransactions } =
    useWalletTransactions(publicKey);
  const {
    earnTransactions,
    isLoading: isFetchingEarn,
    hasLoaded: hasLoadedEarn,
    refresh: refreshEarnTransactions,
  } = useEarnActivity(publicKey);
  const {
    autodeposit,
    hasLoaded: autodepositLoaded,
    refreshAutodeposit: refreshAutodepositState,
  } = useEarnAutodeposit(publicKey);

  useWalletAutoRefresh({
    walletAddress: publicKey,
    refresh: refreshEarnTransactions,
    intervalMs: 15_000,
  });

  // The visibility cap needs the live wallet USDC balance (see
  // getVisibleEarnAutodepositScheduledSweeps) so a scheduled slot whose surplus was already
  // swept into Earn hides instead of lingering as a phantom row. Only read
  // holdings when there's actually a candidate sweep to evaluate, so passive
  // Activity views don't pay for it. Read-only (no signer) — never prompts Seed
  // Vault.
  const scheduledSweepFloorRaw =
    autodeposit?.walletBalanceFloorRaw != null &&
    /^\d+$/.test(autodeposit.walletBalanceFloorRaw)
      ? BigInt(autodeposit.walletBalanceFloorRaw)
      : null;
  const hasScheduledSweepCandidate =
    scheduledSweepFloorRaw != null &&
    (autodeposit?.scheduledSweeps ?? []).some(
      (sweep) =>
        /^\d+$/.test(sweep.remainingAmountRaw) &&
        BigInt(sweep.remainingAmountRaw) >= EARN_AUTODEPOSIT_MIN_VISIBLE_SCHEDULED_SWEEP_RAW,
    );
  const { tokenHoldings, refreshTokenHoldings } = useTokenHoldings(
    hasScheduledSweepCandidate ? publicKey : null,
  );
  const walletUsdcRaw = useMemo<bigint | null>(() => {
    const holding = tokenHoldings.find(
      (h) =>
        h.mint === SOLANA_USDC_MINT_MAINNET ||
        h.mint === SOLANA_USDC_MINT_DEVNET,
    );
    if (!holding || !Number.isFinite(holding.balance)) {
      return null;
    }
    // ponytail: float UI balance -> raw; sub-cent rounding error sits far below
    // the 0.01 USDC visibility threshold, so it can't flip the surplus gate.
    return BigInt(Math.round(holding.balance * 10 ** holding.decimals));
  }, [tokenHoldings]);

  const earnScheduledSweeps = useMemo(
    () =>
      getVisibleEarnAutodepositScheduledSweeps({
        scheduledSweeps: autodeposit?.scheduledSweeps ?? [],
        walletBalanceFloorRaw: scheduledSweepFloorRaw,
        walletBalanceRaw: walletUsdcRaw,
      }),
    [autodeposit, walletUsdcRaw, scheduledSweepFloorRaw],
  );

  const autodepositPausedMissingPosition =
    autodeposit?.lifecycleStatus === "paused_missing_position";

  // Re-read Autodeposit state AND the wallet balance the surplus cap depends on.
  // The force holdings refresh is the point: after an execute the scheduled slot
  // can linger server-side, so only a fresh balance (now back at the floor) lets
  // the cap drop the row — a stale cached balance keeps showing it. No-op when
  // there's no candidate sweep (the holdings hook is idle with a null address).
  const refreshAutodepositAndHoldings = useCallback(async () => {
    await Promise.all([refreshAutodepositState(), refreshTokenHoldings(true)]);
  }, [refreshAutodepositState, refreshTokenHoldings]);

  const hasDueScheduledSweep = earnScheduledSweeps.some(isScheduledSweepDue);
  useEffect(() => {
    if (!hasDueScheduledSweep) {
      return;
    }
    let polls = 0;
    const intervalId = setInterval(() => {
      void refreshAutodepositAndHoldings();
      polls += 1;
      if (polls >= MAX_DUE_SWEEP_POLLS) {
        clearInterval(intervalId);
      }
    }, DUE_SWEEP_POLL_MS);
    return () => clearInterval(intervalId);
  }, [hasDueScheduledSweep, refreshAutodepositAndHoldings]);

  const [isExecutingSweep, setIsExecutingSweep] = useState(false);
  const [sweepMorph, setSweepMorph] = useState<SweepMorph | null>(null);
  const pendingSweepFlowRef =
    useRef<LifecycleFlow<"earn.autodeposit.execute_now"> | null>(null);
  const pendingSweepMetricRef = useRef<ReturnType<
    typeof startMobileLoadingMetric
  > | null>(null);
  const sweepAttemptIdRef = useRef(0);
  const failPendingSweep = useCallback(() => {
    sweepAttemptIdRef.current += 1;
    pendingSweepFlowRef.current?.fail("ui_commit", {
      executeNowState: "failed",
    });
    pendingSweepFlowRef.current = null;
    pendingSweepMetricRef.current?.failAfterPaint();
    pendingSweepMetricRef.current = null;
  }, []);
  const clearSweepMorph = useCallback(() => {
    failPendingSweep();
    setSweepMorph(null);
  }, [failPendingSweep]);

  const executeScheduledSweep = useCallback(async () => {
    if (!signer || !isWalletUnlocked(state) || isExecutingSweep) {
      return;
    }
    const sweeps = earnScheduledSweeps;
    const baselineTxIds = new Set(
      earnTransactions.filter(isBalanceSweepTx).map((tx) => tx.id),
    );
    failPendingSweep();
    const attemptId = ++sweepAttemptIdRef.current;
    pendingSweepMetricRef.current = startMobileLoadingMetric(
      "earn.autodeposit.execute_now",
    );
    setIsExecutingSweep(true);
    try {
      const execution = await executeEarnAutodepositScheduledSweep({
        signer,
        flowId: pendingSweepMetricRef.current.flowId,
      });
      if (attemptId !== sweepAttemptIdRef.current) {
        execution.flow.fail("ui_commit", { executeNowState: "failed" });
        return;
      }
      pendingSweepFlowRef.current = execution.flow;
      if (sweeps.length > 0) {
        setSweepMorph({
          sweeps,
          scheduledSlotId: execution.scheduledSlotId,
          resultTx: null,
          baselineTxIds,
          startedAtMs: Date.now(),
        });
      }
      await Promise.all([
        refreshAutodepositAndHoldings(),
        refreshEarnTransactions(),
      ]);
    } catch (error) {
      failPendingSweep();
      console.warn("[autodeposit] execute scheduled sweep failed", error);
      const refusal =
        error instanceof EarnApiError &&
        (error.code === "earn_position_required" ||
          error.code === "autodeposit_not_active" ||
          error.code === "no_scheduled_sweeps")
          ? error.message
          : null;
      Alert.alert(
        refusal ? "Deposit can't run" : "Deposit didn't complete",
        refusal ??
          "The deposit didn't complete this time. Please wait 1 minute and try again.",
      );
    } finally {
      setIsExecutingSweep(false);
    }
  }, [
    signer,
    state,
    isExecutingSweep,
    earnScheduledSweeps,
    earnTransactions,
    refreshAutodepositAndHoldings,
    refreshEarnTransactions,
    failPendingSweep,
  ]);

  useEffect(() => {
    if (!sweepMorph || sweepMorph.resultTx) {
      return;
    }
    const fresh = earnTransactions.find(
      (tx) => isBalanceSweepTx(tx) && !sweepMorph.baselineTxIds.has(tx.id),
    );
    if (fresh) {
      setSweepMorph((m) => (m && !m.resultTx ? { ...m, resultTx: fresh } : m));
      pendingSweepFlowRef.current?.complete("state_observed", {
        executeNowState: "completed",
      });
      pendingSweepFlowRef.current = null;
      pendingSweepMetricRef.current?.completeAfterPaint();
      pendingSweepMetricRef.current = null;
      nudgeQuestProgressCheck();
      setTimeout(nudgeQuestProgressCheck, 8_000);
    }
  }, [earnTransactions, sweepMorph]);

  // While the morph is unresolved, poll the Earn feed so the result tx is picked
  // up without waiting for the 15s background tick. Bounded. Deliberately only
  // the feed (a backend read) — NOT `refreshAutodepositAndHoldings`, which
  // force-refreshes token holdings over the Solana RPC: the morph resolves
  // purely off the feed, and adding holdings reads here (on top of the
  // due-sweep poll) was pressuring the rate-limited RPC into 429s.
  useEffect(() => {
    if (!sweepMorph || sweepMorph.resultTx) {
      return;
    }
    let polls = 0;
    const intervalId = setInterval(() => {
      void refreshEarnTransactions();
      polls += 1;
      if (polls >= MAX_SWEEP_RESULT_POLLS) {
        clearInterval(intervalId);
      }
    }, SWEEP_RESULT_POLL_MS);
    return () => clearInterval(intervalId);
  }, [sweepMorph, refreshEarnTransactions]);

  useEffect(() => {
    if (!sweepMorph) {
      return;
    }
    const timeoutId = setTimeout(() => {
      failPendingSweep();
      setSweepMorph(null);
    }, SWEEP_MORPH_MAX_PINNED_MS);
    return () => clearTimeout(timeoutId);
  }, [sweepMorph, failPendingSweep]);

  useEffect(() => {
    failPendingSweep();
    setSweepMorph(null);
  }, [publicKey, failPendingSweep]);

  const [sweepProgressState, setSweepProgressState] =
    useState<EarnAutodepositProgressState | null>(null);
  const dueSweepSlotId =
    earnScheduledSweeps.find(isScheduledSweepDue)?.id ?? null;
  const watchedSweepSlotId = sweepMorph
    ? sweepMorph.resultTx
      ? null
      : sweepMorph.scheduledSlotId
    : dueSweepSlotId;

  useEffect(() => {
    if (!publicKey || !watchedSweepSlotId) {
      setSweepProgressState(null);
      return;
    }
    let cancelled = false;
    let fetching = false;
    let sawInFlight = false;
    let polls = 0;
    let misses = 0;
    const intervalId = setInterval(() => {
      if (fetching) {
        return;
      }
      polls += 1;
      if (polls > MAX_SWEEP_PROGRESS_POLLS) {
        clearInterval(intervalId);
        return;
      }
      fetching = true;
      void fetchEarnAutodepositSweepProgress(publicKey, watchedSweepSlotId)
        .then((progress) => {
          if (cancelled) {
            return;
          }
          if (!progress) {
            misses += 1;
            if (misses >= MAX_SWEEP_PROGRESS_MISSES) {
              clearInterval(intervalId);
            }
            return;
          }
          misses = 0;
          setSweepProgressState(progress.state);
          const bouncedBack = sawInFlight && progress.state === "scheduled";
          if (progress.state !== "scheduled") {
            sawInFlight = true;
          }
          if (
            progress.state === "failed" ||
            progress.state === "released" ||
            progress.state === "canceled"
          ) {
            if (progress.state === "canceled") {
              pendingSweepFlowRef.current?.cancel("state_observed", {
                executeNowState: progress.state,
              });
            } else {
              pendingSweepFlowRef.current?.fail("state_observed", {
                executeNowState: progress.state,
              });
            }
            pendingSweepFlowRef.current = null;
            pendingSweepMetricRef.current?.failAfterPaint();
            pendingSweepMetricRef.current = null;
            setSweepMorph(null);
          }
          if (SWEEP_PROGRESS_END_STATES.has(progress.state) || bouncedBack) {
            clearInterval(intervalId);
            void refreshAutodepositAndHoldings();
            void refreshEarnTransactions();
          }
        })
        .finally(() => {
          fetching = false;
        });
    }, SWEEP_PROGRESS_POLL_MS);
    return () => {
      cancelled = true;
      clearInterval(intervalId);
    };
  }, [
    publicKey,
    watchedSweepSlotId,
    refreshAutodepositAndHoldings,
    refreshEarnTransactions,
  ]);

  const [earnRefunds, setEarnRefunds] = useState<EarnRefundItem[]>([]);
  const [earnLockedRefundLamports, setEarnLockedRefundLamports] = useState(0);
  const [refundingAccount, setRefundingAccount] = useState<string | null>(null);

  const refreshEarnRefunds = useCallback(async (): Promise<EarnRefundItem[]> => {
    if (!publicKey) {
      setEarnRefunds([]);
      setEarnLockedRefundLamports(0);
      return [];
    }
    try {
      const { scan } = await fetchEarnRefundScan(publicKey);
      const items: EarnRefundItem[] = [
        ...(scan?.policies ?? [])
          .filter((policy) => policy.canRefund && (policy.lamports ?? 0) > 0)
          .map((policy) => ({
            kind: "policy" as const,
            account: policy.account,
            lamports: policy.lamports ?? 0,
          })),
        ...(scan?.recurringDelegations ?? [])
          .filter(
            (delegation) =>
              delegation.canRefund && (delegation.lamports ?? 0) > 0,
          )
          .map((delegation) => ({
            kind: "recurring_delegation" as const,
            account: delegation.account,
            lamports: delegation.lamports ?? 0,
          })),
        ...(scan?.vault?.canRefund && scan.vault.totalRefundableLamports > 0
          ? [
              {
                kind: "vault" as const,
                account: scan.vault.account,
                lamports: scan.vault.totalRefundableLamports,
              },
            ]
          : []),
      ];
      const isAutodepositBlock = (reason: string | null | undefined) =>
        typeof reason === "string" &&
        /autodeposit|recurring delegation/i.test(reason);
      const lockedLamports =
        (scan?.policies ?? [])
          .filter((p) => !p.canRefund && isAutodepositBlock(p.blockedReason))
          .reduce((sum, p) => sum + (p.lamports ?? 0), 0) +
        (scan?.recurringDelegations ?? [])
          .filter((d) => !d.canRefund && isAutodepositBlock(d.blockedReason))
          .reduce((sum, d) => sum + (d.lamports ?? 0), 0) +
        (scan?.vault &&
        !scan.vault.canRefund &&
        isAutodepositBlock(scan.vault.blockedReason)
          ? scan.vault.totalRefundableLamports
          : 0);
      setEarnRefunds(items);
      setEarnLockedRefundLamports(lockedLamports);
      return items;
    } catch (error) {
      console.warn("[earn-refunds] scan failed", error);
      return [];
    }
  }, [publicKey]);

  useEffect(() => {
    void refreshEarnRefunds();
  }, [refreshEarnRefunds]);

  const executeRefund = useCallback(
    async (item: EarnRefundItem) => {
      if (!signer || !isWalletUnlocked(state) || refundingAccount) {
        return;
      }
      setRefundingAccount(item.account);
      const metric = startMobileLoadingMetric("earn.refund");
      try {
        const request: EarnRefundPrepareRequest =
          item.kind === "policy"
            ? { kind: "policy", policyAccount: item.account }
            : item.kind === "recurring_delegation"
              ? { kind: "recurring_delegation", recurringDelegation: item.account }
              : { kind: "vault" };
        await executeEarnRefund({ signer, request });
        setEarnRefunds((current) =>
          current.filter((refund) => refund.account !== item.account),
        );
        void refreshEarnRefunds();
        metric.completeAfterPaint();
      } catch (error) {
        metric.failAfterPaint();
        console.warn("[earn-refunds] refund failed", error);
        Alert.alert(
          "Refund didn't complete",
          "Refund didn't complete this time. Please wait 1 minute and try again.",
        );
      } finally {
        setRefundingAccount(null);
      }
    },
    [signer, state, refundingAccount, refreshEarnRefunds],
  );

  const canDeleteLockedRefundAutodeposit =
    autodeposit != null &&
    !autodeposit.active &&
    autodeposit.status !== "pending" &&
    autodeposit.recurringDelegation != null;
  const [deletingAutodeposit, setDeletingAutodeposit] = useState(false);

  const deleteAutodepositForRefund = useCallback(() => {
    if (
      !canDeleteLockedRefundAutodeposit ||
      !autodeposit?.recurringDelegation ||
      !signer ||
      !isWalletUnlocked(state) ||
      deletingAutodeposit
    ) {
      return;
    }
    const { policyAccount, recurringDelegation } = autodeposit;
    Alert.alert(
      "Delete Autodeposit?",
      "This removes Autodeposit and returns its reserved SOL to your wallet.",
      [
        { text: "Cancel", style: "cancel" },
        {
          text: "Delete",
          style: "destructive",
          onPress: () => {
            setDeletingAutodeposit(true);
            void (async () => {
              try {
                await executeEarnAutodepositClose({
                  signer,
                  policy: policyAccount,
                  recurringDelegation,
                });
                // The close tx already refunded the rent — clear the row now,
                // then rescan (the chain read can lag a beat behind).
                setEarnLockedRefundLamports(0);
                void refreshAutodepositState();
                void refreshEarnRefunds();
              } catch (error) {
                console.warn("[earn-refunds] autodeposit delete failed", error);
                Alert.alert(
                  "Delete didn't complete",
                  "Autodeposit wasn't deleted this time. Please wait 1 minute and try again.",
                );
              } finally {
                setDeletingAutodeposit(false);
              }
            })();
          },
        },
      ],
    );
  }, [
    canDeleteLockedRefundAutodeposit,
    autodeposit,
    signer,
    state,
    deletingAutodeposit,
    refreshAutodepositState,
    refreshEarnRefunds,
  ]);

  const newestWalletTs = useMemo(() => {
    let newest = 0;
    for (const tx of walletTransactions) {
      if (tx.timestamp > newest) newest = tx.timestamp;
    }
    return newest;
  }, [walletTransactions]);

  const newestEarnTs = useMemo(() => {
    let newest = 0;
    for (const tx of earnTransactions) {
      const ts = earnTransactionTimestampMs(tx);
      if (ts > newest) newest = ts;
    }
    return newest;
  }, [earnTransactions]);

  const [seenWallet, setSeenWallet] = useState<number | undefined>(() =>
    getActivityLastSeen("wallet"),
  );
  const [seenEarn, setSeenEarn] = useState<number | undefined>(() =>
    getActivityLastSeen("earn"),
  );

  useEffect(() => {
    if (newestWalletTs <= 0 || seenWallet !== undefined) return;
    setActivityLastSeen("wallet", newestWalletTs);
    setSeenWallet(newestWalletTs);
  }, [newestWalletTs, seenWallet]);

  useEffect(() => {
    if (newestEarnTs <= 0 || seenEarn !== undefined) return;
    setActivityLastSeen("earn", newestEarnTs);
    setSeenEarn(newestEarnTs);
  }, [newestEarnTs, seenEarn]);

  const markSeen = useCallback(
    (section: ActivitySection) => {
      if (section === "wallet" && newestWalletTs > 0) {
        setActivityLastSeen("wallet", newestWalletTs);
        setSeenWallet(newestWalletTs);
      }
      if (section === "earn" && newestEarnTs > 0) {
        setActivityLastSeen("earn", newestEarnTs);
        setSeenEarn(newestEarnTs);
      }
    },
    [newestWalletTs, newestEarnTs],
  );

  const walletUnread = seenWallet !== undefined && newestWalletTs > seenWallet;
  const earnUnread = seenEarn !== undefined && newestEarnTs > seenEarn;

  const value = useMemo<ActivityContextValue>(
    () => ({
      walletAddress: publicKey,
      walletTransactions,
      isFetchingTransactions,
      loadWalletTransactions,
      earnTransactions,
      isFetchingEarn,
      hasLoadedEarn,
      refreshEarnTransactions,
      autodeposit,
      autodepositLoaded,
      refreshAutodeposit: refreshAutodepositState,
      refreshAutodepositAndHoldings,
      earnScheduledSweeps,
      isExecutingSweep,
      executeScheduledSweep,
      autodepositPausedMissingPosition,
      sweepMorph,
      clearSweepMorph,
      sweepProgressState,
      walletUnread,
      earnUnread,
      anyUnread: walletUnread || earnUnread,
      markSeen,
      earnRefunds,
      earnLockedRefundLamports,
      refreshEarnRefunds,
      refundingAccount,
      executeRefund,
      canDeleteLockedRefundAutodeposit,
      deletingAutodeposit,
      deleteAutodepositForRefund,
    }),
    [
      publicKey,
      walletTransactions,
      isFetchingTransactions,
      loadWalletTransactions,
      earnTransactions,
      isFetchingEarn,
      hasLoadedEarn,
      refreshEarnTransactions,
      autodeposit,
      autodepositLoaded,
      refreshAutodepositState,
      refreshAutodepositAndHoldings,
      earnScheduledSweeps,
      isExecutingSweep,
      executeScheduledSweep,
      autodepositPausedMissingPosition,
      sweepMorph,
      clearSweepMorph,
      sweepProgressState,
      walletUnread,
      earnUnread,
      markSeen,
      earnRefunds,
      earnLockedRefundLamports,
      refreshEarnRefunds,
      refundingAccount,
      executeRefund,
      canDeleteLockedRefundAutodeposit,
      deletingAutodeposit,
      deleteAutodepositForRefund,
    ],
  );

  return (
    <ActivityContext.Provider value={value}>
      {children}
    </ActivityContext.Provider>
  );
}

export function useActivity(): ActivityContextValue {
  const ctx = useContext(ActivityContext);
  if (!ctx) {
    throw new Error("useActivity must be used within an ActivityProvider");
  }
  return ctx;
}
