import { useEffect, useState } from "react";
import { Pressable, StyleSheet } from "react-native";
import Animated, {
  LinearTransition,
  runOnJS,
  useAnimatedStyle,
  useSharedValue,
  withTiming,
} from "react-native-reanimated";

import {
  formatLoadedScheduledSweepAvailableIn,
  getEarnTransactionRowLabel,
  getLoadedScheduledSweepExecuteNowAvailableAtMs,
  isScheduledSweepExecuting,
  type LoadedEarnAutodepositScheduledSweep,
} from "@loyal-labs/shared";

import type { EarnTransactionItem } from "@/lib/solana/earn/earn-api";
import {
  formatScheduledSweepAmount,
  formatScheduledSweepTime,
  getScheduledSweepStatusLabels,
} from "@/lib/solana/earn/earn-scheduled-sweep";
import {
  formatEarnRowTime,
  getEarnTransactionAmountColor,
  groupEarnTransactions,
} from "@/lib/solana/earn/earn-tx-display";
import { Text, View } from "@/tw";

import {
  type EarnRefundItem,
  type SweepMorph,
  useActivity,
} from "../model/ActivityProvider";
import { EarnTxAccountIcon, EarnTxCompoundIcon } from "./EarnTxIcon";

const SECONDARY = "rgba(60, 60, 67, 0.6)";
const BRAND_RED = "#F9363C";

function EarnTransactionRow({ item }: { item: EarnTransactionItem }) {
  const label = getEarnTransactionRowLabel(item);
  const amountColor = getEarnTransactionAmountColor(item.kind);

  return (
    <View className="flex-row items-center px-4 py-2.5">
      <EarnTxCompoundIcon item={item} />
      <View className="ml-3 flex-1">
        <Text className="text-[17px] font-medium text-black">{label}</Text>
        <View className="mt-0.5 flex-row items-center gap-1">
          <EarnTxAccountIcon account={item.source} />
          <Text
            className="text-[13px]"
            style={{ color: SECONDARY }}
            numberOfLines={1}
          >
            {item.source.label}
          </Text>
          <Text className="text-[13px]" style={{ color: SECONDARY }}>
            →
          </Text>
          <EarnTxAccountIcon account={item.destination} />
          <Text
            className="text-[13px]"
            style={{ color: SECONDARY }}
            numberOfLines={1}
          >
            {item.destination.label}
          </Text>
        </View>
      </View>
      <View className="items-end">
        <Text className="text-[17px]" style={{ color: amountColor }}>
          {item.amount}
        </Text>
        <Text className="text-[13px]" style={{ color: SECONDARY }}>
          {formatEarnRowTime(item)}
        </Text>
      </View>
    </View>
  );
}

function EarnScheduledRow({
  sweep,
  isExecuting,
  onExecute,
  forceExecuting = false,
  progressState = null,
}: {
  sweep: LoadedEarnAutodepositScheduledSweep;
  isExecuting: boolean;
  onExecute: () => void;
  forceExecuting?: boolean;
  progressState?: string | null;
}) {
  const awaiting = forceExecuting || isScheduledSweepExecuting(sweep);

  const availableAtMs = getLoadedScheduledSweepExecuteNowAvailableAtMs(sweep);
  const [nowMs, setNowMs] = useState(() => Date.now());
  useEffect(() => {
    if (availableAtMs === null || availableAtMs <= Date.now()) {
      return;
    }
    const intervalId = setInterval(() => {
      setNowMs(Date.now());
      if (Date.now() >= availableAtMs) {
        clearInterval(intervalId);
      }
    }, 1000);
    return () => clearInterval(intervalId);
  }, [availableAtMs]);
  const availableInLabel =
    availableAtMs === null
      ? null
      : formatLoadedScheduledSweepAvailableIn(availableAtMs, nowMs);

  const statusLabels = getScheduledSweepStatusLabels(sweep, progressState);
  const progressLocked =
    progressState === "requested" ||
    progressState === "selected" ||
    progressState === "pull_confirmed" ||
    progressState === "completed";
  const disabled =
    isExecuting || awaiting || progressLocked || availableInLabel !== null;
  const buttonLabel = isExecuting
    ? "Requesting…"
    : (availableInLabel ??
      statusLabels.button ??
      (awaiting ? "Executing…" : "Execute now"));

  return (
    <View className="flex-row items-start px-4 py-2.5">
      <EarnTxCompoundIcon item={{ kind: "balance_sweep" }} />
      <View className="ml-3 flex-1">
        <Text className="text-[17px] font-medium text-black">Autodeposit</Text>
        <Text className="text-[15px]" style={{ color: SECONDARY }}>
          {statusLabels.subtitle ??
            formatScheduledSweepTime(sweep.eligibleAfter)}
        </Text>
        <Pressable
          accessibilityRole="button"
          disabled={disabled}
          onPress={onExecute}
          style={({ pressed }) => [
            styles.executeButton,
            (pressed || disabled) && styles.executeButtonMuted,
          ]}
        >
          <Text className="text-[14px] font-medium text-white">
            {buttonLabel}
          </Text>
        </Pressable>
      </View>
      <View className="ml-3 items-end">
        <Text className="text-[17px] text-black">
          {formatScheduledSweepAmount(sweep.remainingAmountRaw)}
        </Text>
        <Text className="text-[13px]" style={{ color: SECONDARY }}>
          Main → Earn
        </Text>
      </View>
    </View>
  );
}

function formatRefundSol(lamports: number): string {
  return (lamports / 1e9).toFixed(4).replace(/\.?0+$/, "");
}

const REFUND_SUBTITLES: Record<EarnRefundItem["kind"], string> = {
  policy: "Earn policy rent",
  recurring_delegation: "Earn allowance rent",
  vault: "Earn account rent",
};

function EarnRefundRow({
  item,
  isRefunding,
  onRefund,
}: {
  item: EarnRefundItem;
  isRefunding: boolean;
  onRefund: () => void;
}) {
  return (
    <View className="flex-row items-start px-4 py-2.5">
      <EarnTxCompoundIcon item={{ kind: "withdraw" }} />
      <View className="ml-3 flex-1">
        <Text className="text-[17px] font-medium text-black">Refund</Text>
        <Text className="text-[15px]" style={{ color: SECONDARY }}>
          {REFUND_SUBTITLES[item.kind]}
        </Text>
        <Pressable
          accessibilityRole="button"
          disabled={isRefunding}
          onPress={onRefund}
          style={({ pressed }) => [
            styles.executeButton,
            (pressed || isRefunding) && styles.executeButtonMuted,
          ]}
        >
          <Text className="text-[14px] font-medium text-white">
            {isRefunding ? "Refunding…" : "Refund"}
          </Text>
        </Pressable>
      </View>
      <View className="ml-3 items-end">
        <Text className="text-[17px] text-black">
          +{formatRefundSol(item.lamports)} SOL
        </Text>
        <Text className="text-[13px]" style={{ color: SECONDARY }}>
          Earn → Main
        </Text>
      </View>
    </View>
  );
}

function EarnLockedRefundRow({
  lamports,
  canDelete,
  isDeleting,
  onDelete,
}: {
  lamports: number;
  canDelete: boolean;
  isDeleting: boolean;
  onDelete: () => void;
}) {
  return (
    <View className="flex-row items-start px-4 py-2.5">
      <EarnTxCompoundIcon item={{ kind: "withdraw" }} />
      <View className="ml-3 flex-1">
        <Text className="text-[17px] font-medium text-black">Reserved</Text>
        <Text className="text-[15px]" style={{ color: SECONDARY }}>
          Held while Autodeposit is set up. Delete Autodeposit to refund it.
        </Text>
        {canDelete ? (
          <Pressable
            accessibilityRole="button"
            disabled={isDeleting}
            onPress={onDelete}
            style={({ pressed }) => [
              styles.executeButton,
              (pressed || isDeleting) && styles.executeButtonMuted,
            ]}
          >
            <Text className="text-[14px] font-medium text-white">
              {isDeleting ? "Deleting…" : "Delete Autodeposit"}
            </Text>
          </Pressable>
        ) : null}
      </View>
      <View className="ml-3 items-end">
        <Text className="text-[17px] text-black">
          {formatRefundSol(lamports)} SOL
        </Text>
        <Text className="text-[13px]" style={{ color: SECONDARY }}>
          Locked
        </Text>
      </View>
    </View>
  );
}

function EarnAutodepositPausedRow() {
  return (
    <View className="flex-row items-start px-4 py-2.5">
      <EarnTxCompoundIcon item={{ kind: "deposit" }} />
      <View className="ml-3 flex-1">
        <Text className="text-[17px] font-medium text-black">Autodeposit</Text>
        <Text className="text-[15px]" style={{ color: SECONDARY }}>
          Paused — your Earn account was closed. Make a deposit and Autodeposit
          turns back on automatically.
        </Text>
      </View>
      <View className="ml-3 items-end">
        <Text className="text-[13px]" style={{ color: SECONDARY }}>
          Paused
        </Text>
      </View>
    </View>
  );
}

const noop = () => {};

function SweepMorphSection({
  morph,
  isRequesting,
  progressState,
}: {
  morph: SweepMorph;
  isRequesting: boolean;
  progressState: string | null;
}) {
  const opacity = useSharedValue(1);
  const [showResult, setShowResult] = useState(false);

  useEffect(() => {
    if (morph.resultTx && !showResult) {
      opacity.value = withTiming(0, { duration: 220 }, (finished) => {
        if (finished) {
          runOnJS(setShowResult)(true);
        }
      });
    }
  }, [morph.resultTx, showResult, opacity]);

  useEffect(() => {
    if (showResult) {
      opacity.value = withTiming(1, { duration: 260 });
    }
  }, [showResult, opacity]);

  const animatedStyle = useAnimatedStyle(() => ({ opacity: opacity.value }));

  return (
    <Animated.View style={animatedStyle} layout={LinearTransition.duration(260)}>

      {showResult && morph.resultTx ? (
        <EarnTransactionRow item={morph.resultTx} />
      ) : (
        <View>
          <Text
            className="px-4 pb-2 pt-3 text-[17px]"
            style={{ color: SECONDARY, letterSpacing: -0.187 }}
          >
            Scheduled
          </Text>
          {morph.sweeps.map((sweep) => (
            <EarnScheduledRow
              key={sweep.id}
              sweep={sweep}
              isExecuting={isRequesting}
              onExecute={noop}
              forceExecuting
              progressState={progressState}
            />
          ))}
        </View>
      )}
    </Animated.View>
  );
}

export function EarnActivityList({ limit }: { limit: number }) {
  const {
    earnTransactions,
    hasLoadedEarn,
    earnScheduledSweeps,
    isExecutingSweep,
    executeScheduledSweep,
    autodepositPausedMissingPosition,
    sweepMorph,
    sweepProgressState,
    earnRefunds,
    earnLockedRefundLamports,
    refundingAccount,
    executeRefund,
    canDeleteLockedRefundAutodeposit,
    deletingAutodeposit,
    deleteAutodepositForRefund,
  } = useActivity();
  const hasScheduled = !sweepMorph && earnScheduledSweeps.length > 0;
  const hasRefunds = earnRefunds.length > 0;
  const hasLockedRefunds = earnLockedRefundLamports > 0;

  if (
    !hasLoadedEarn &&
    earnTransactions.length === 0 &&
    !hasScheduled &&
    !sweepMorph &&
    !hasRefunds &&
    !hasLockedRefunds &&
    !autodepositPausedMissingPosition
  ) {
    return (
      <View className="px-4">
        {[1, 2, 3].map((i) => (
          <View key={i} className="flex-row items-center px-4 py-2.5">
            <View
              className="h-12 w-12 rounded-full"
              style={{ backgroundColor: "#f2f2f7" }}
            />
            <View className="ml-3 flex-1">
              <View
                className="mb-1 h-4 w-24 rounded"
                style={{ backgroundColor: "#f2f2f7" }}
              />
              <View
                className="h-3 w-28 rounded"
                style={{ backgroundColor: "#f2f2f7" }}
              />
            </View>
            <View className="items-end">
              <View
                className="mb-1 h-4 w-20 rounded"
                style={{ backgroundColor: "#f2f2f7" }}
              />
              <View
                className="h-3 w-12 rounded"
                style={{ backgroundColor: "#f2f2f7" }}
              />
            </View>
          </View>
        ))}
      </View>
    );
  }

  if (
    earnTransactions.length === 0 &&
    !hasScheduled &&
    !sweepMorph &&
    !hasRefunds &&
    !hasLockedRefunds &&
    !autodepositPausedMissingPosition
  ) {
    return (
      <View className="items-center px-4 py-12">
        <Text className="text-[17px] font-medium text-black">
          No transactions yet
        </Text>
        <Text
          className="mt-1 text-[15px]"
          style={{ color: SECONDARY, textAlign: "center" }}
        >
          Earn deposits and withdrawals will appear here.
        </Text>
      </View>
    );
  }

  const morphResultId = sweepMorph?.resultTx?.id;
  const groupedTxns = morphResultId
    ? earnTransactions.filter((tx) => tx.id !== morphResultId)
    : earnTransactions;
  // Only render up to `limit` rows — the parent grows this on scroll. Rendering
  // the full history at once blocks the tab switch (non-virtualized ScrollView).
  const groups = groupEarnTransactions(groupedTxns.slice(0, limit));

  return (
    <View>
      {hasRefunds || hasLockedRefunds ? (
        <View>
          <Text
            className="px-4 pb-2 pt-3 text-[17px]"
            style={{ color: SECONDARY, letterSpacing: -0.187 }}
          >
            Refunds
          </Text>
          {earnRefunds.map((refund) => (
            <EarnRefundRow
              key={refund.account}
              item={refund}
              isRefunding={refundingAccount === refund.account}
              onRefund={() => void executeRefund(refund)}
            />
          ))}
          {hasLockedRefunds ? (
            <EarnLockedRefundRow
              lamports={earnLockedRefundLamports}
              canDelete={canDeleteLockedRefundAutodeposit}
              isDeleting={deletingAutodeposit}
              onDelete={deleteAutodepositForRefund}
            />
          ) : null}
        </View>
      ) : null}
      {autodepositPausedMissingPosition ? <EarnAutodepositPausedRow /> : null}
      {sweepMorph ? (
        <SweepMorphSection
          key={sweepMorph.startedAtMs}
          morph={sweepMorph}
          isRequesting={isExecutingSweep}
          progressState={sweepProgressState}
        />
      ) : hasScheduled ? (
        <View>
          <Text
            className="px-4 pb-2 pt-3 text-[17px]"
            style={{ color: SECONDARY, letterSpacing: -0.187 }}
          >
            Scheduled
          </Text>
          {earnScheduledSweeps.map((sweep) => (
            <EarnScheduledRow
              key={sweep.id}
              sweep={sweep}
              isExecuting={isExecutingSweep}
              onExecute={executeScheduledSweep}
              progressState={sweepProgressState}
            />
          ))}
        </View>
      ) : null}
      {groups.map((group) => (
        <View key={group.date}>
          <Text
            className="px-4 pb-2 pt-3 text-[17px]"
            style={{ color: SECONDARY, letterSpacing: -0.187 }}
          >
            {group.date}
          </Text>
          {group.items.map((item) => (
            <EarnTransactionRow key={item.id} item={item} />
          ))}
        </View>
      ))}
    </View>
  );
}

const styles = StyleSheet.create({
  executeButton: {
    alignSelf: "flex-start",
    marginTop: 8,
    backgroundColor: BRAND_RED,
    borderRadius: 9999,
    paddingHorizontal: 16,
    paddingVertical: 6,
  },
  executeButtonMuted: {
    opacity: 0.6,
  },
});
