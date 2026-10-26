import { useIsFocused } from "@react-navigation/native";
import { StatusBar } from "expo-status-bar";
import { EarnChartTabs } from "@/components/earn/EarnChartTabs";
import {
  EarnBalanceCard,
  EarnHeader,
} from "@/components/earn/EarnScreenControls";
import * as Haptics from "expo-haptics";
import { earnMaxApyFigure } from "@loyal-labs/shared";
import { router } from "expo-router";
import { useCallback, useEffect, useMemo, useState } from "react";
import { Pressable, Text, View } from "react-native";
import { useSafeAreaInsets } from "react-native-safe-area-context";
import { DepositSheet } from "@/components/earn/DepositSheet";
import { PositionsSheet } from "@/components/earn/PositionsSheet";
import { WithdrawSheet } from "@/components/earn/WithdrawSheet";
import { useEarnMax } from "@/features/earn-max/EarnMaxProvider";
import { useEarnMaxFundingUsdc } from "@/features/earn-max/useEarnMaxFundingUsdc";
import { useWallet } from "@/lib/wallet/wallet-provider";
import type { EarnEarningsResponse } from "@/lib/solana/earn/earn-api";
import { SOLANA_USDC_MINT_MAINNET as USDC } from "@/lib/solana/constants";
export default function EarnMaxScreen() {
  const insets = useSafeAreaInsets();
  const isFocused = useIsFocused();
  const { publicKey } = useWallet();
  const {
    summary,
    activity,
    controls,
    run,
    error,
    refresh,
    hasLoaded,
    supported,
  } = useEarnMax();
  const walletUsdc = useEarnMaxFundingUsdc(supported ? publicKey : null) ?? 0;
  useEffect(() => {
    if (isFocused) void refresh().catch(() => {});
  }, [isFocused, refresh]);
  const [sheet, setSheet] = useState<
    "deposit" | "withdraw" | "positions" | null
  >(null);
  const openDeposit = useCallback(() => {
    void Haptics.selectionAsync();
    setSheet("deposit");
  }, []);
  useEffect(() => {
    if (!isFocused) setSheet(null);
  }, [isFocused]);
  const apy = summary?.forecastApyBps ?? null;
  const balance = summary?.balanceUsd ?? (hasLoaded && !error ? 0 : undefined);
  const split = (balance ?? 0).toFixed(2).split(".");
  const earnings = useMemo<EarnEarningsResponse | null>(() => {
    if (
      !summary ||
      !activity?.earnings ||
      summary.coverage !== "complete"
    )
      return null;
    const { performanceUsd, days } = activity.earnings;
    const today = new Date().toISOString().slice(0, 10);
    return {
      bars: days.map((day) => ({
        apyBps: null,
        avgPrincipalUsd: 0,
        earnedUsd: day.performanceUsd,
        endAt: `${day.date}T23:59:59.000Z`,
        isCurrent: day.date === today,
        label: String(Number(day.date.slice(-2))),
        principalAmountRaw: "0",
        principalUsd: 0,
        startAt: `${day.date}T00:00:00.000Z`,
      })),
      currentApyBps: null,
      lastDepositAt: null,
      lifetimeEarnedUsd: performanceUsd,
      principalAmountRaw: "0",
      principalUsd: 0,
      rangeEarnedUsd: days.reduce((sum, day) => sum + day.performanceUsd, 0),
      sinceLastDepositEarnedUsd: performanceUsd,
      todayEarnedUsd: days.find((day) => day.date === today)?.performanceUsd ?? 0,
    };
  }, [summary, activity]);
  const activityLink = () =>
    router.navigate({
      pathname: "/(tabs)/activity",
      params: { section: "earn-max" },
    });
  return (
    <View style={{ flex: 1, backgroundColor: "#000", paddingTop: insets.top }}>
      {isFocused && <StatusBar style="light" />}
      <View style={{ flex: 1, overflow: "hidden" }}>
        <EarnHeader
          title="Earn MAX"
          apyLabel={earnMaxApyFigure(summary)?.label ?? "—"}
          onBack={() => router.navigate("/(tabs)")}
        />
        {error && (
          <Pressable
            onPress={() => void refresh({ signIn: true })}
            accessibilityRole="button"
            accessibilityLabel="Retry loading Earn MAX"
            style={{ paddingHorizontal: 16, paddingVertical: 8 }}
          >
            <Text style={{ color: "#F9363C" }}>
              Unable to update Earn MAX. Tap to retry.
            </Text>
          </Pressable>
        )}
        <EarnChartTabs
          principalUsd={balance ?? null}
          walletAddress={publicKey}
          earnMax={{
            earnings,
            status: earnings
              ? "ready"
              : !hasLoaded
              ? "loading"
              : error || summary
              ? "unavailable"
              : "ready",
            history: (summary?.apyHistory ?? []).flatMap((point) =>
              point.apyBps === null
                ? []
                : [{ date: point.date, apyBps: point.apyBps }]
            ),
            apyBps: apy,
          }}
        />
      </View>
      <EarnBalanceCard
        label="Earn MAX Balance"
        whole={`$${Number(split[0]).toLocaleString("en-US")}`}
        cents={`.${split[1]}`}
        hasDeposit
        fullWidthActions
        balanceLoading={balance == null}
        depositDisabled={!controls.canDeposit || !hasLoaded || !!error}
        withdrawDisabled={!controls.canWithdraw || !hasLoaded || !!error}
        onPositions={() => {
          void Haptics.selectionAsync();
          setSheet("positions");
        }}
        onDeposit={openDeposit}
        onWithdraw={() => {
          void Haptics.selectionAsync();
          setSheet("withdraw");
        }}
      />
      <DepositSheet
        earnMax
        earnMaxApyBps={apy}
        isFirstDeposit={!summary?.balanceUsd}
        open={sheet === "deposit"}
        onClose={() =>
          setSheet((current) => (current === "deposit" ? null : current))
        }
        stableBalancesByMint={{ [USDC]: walletUsdc }}
        onDeposit={async (amount) => {
          if (amount < 1) throw new Error("Minimum deposit is $1.");
          await run({
            action: "deposit",
            amountRaw: BigInt(Math.round(amount * 100)) * 10000n,
          });
        }}
      />
      <WithdrawSheet
        earnMax
        earnMaxWallet={{ address: publicKey, balance: walletUsdc }}
        open={sheet === "withdraw"}
        onClose={() =>
          setSheet((current) => (current === "withdraw" ? null : current))
        }
        availableUsdc={balance}
        onWithdraw={async (amount, _source, mode) => {
          if (!controls.canWithdraw)
            throw new Error("Claim your pending withdrawal first.");
          await run({
            action: "request_withdrawal",
            amountRaw:
              mode === "full"
                ? "max"
                : BigInt(Math.round(amount * 100)) * 10000n,
          });
          activityLink();
        }}
      />
      <PositionsSheet
        open={sheet === "positions"}
        onClose={() =>
          setSheet((current) => (current === "positions" ? null : current))
        }
        balanceWhole={`$${split[0]}`}
        balanceCents={`.${split[1]}`}
        sources={[]}
        earnMax={{
          apyBps: apy,
          earnedUsd: summary?.earnedUsd ?? null,
          withdrawDisabled: !controls.canWithdraw,
        }}
        onDeposit={openDeposit}
        onWithdraw={() => setSheet("withdraw")}
      />
    </View>
  );
}
