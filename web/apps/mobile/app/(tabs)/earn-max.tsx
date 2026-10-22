import { useIsFocused } from "@react-navigation/native";
import { StatusBar } from "expo-status-bar";
import { EarnChartTabs } from "@/components/earn/EarnChartTabs";
import {
  EarnBalanceCard,
  EarnHeader,
} from "@/components/earn/EarnScreenControls";
import * as Haptics from "expo-haptics";
import { buildEarnMaxDailySeries } from "@loyal-labs/shared";
import { router } from "expo-router";
import { useCallback, useEffect, useMemo, useState } from "react";
import { Pressable, Text, View } from "react-native";
import { useSafeAreaInsets } from "react-native-safe-area-context";
import { DepositSheet } from "@/components/earn/DepositSheet";
import { PositionsSheet } from "@/components/earn/PositionsSheet";
import { WithdrawSheet } from "@/components/earn/WithdrawSheet";
import { useEarnMax } from "@/features/earn-max/EarnMaxProvider";
import { useTokenHoldings } from "@/hooks/wallet/useTokenHoldings";
import { useWallet } from "@/lib/wallet/wallet-provider";
import type { EarnEarningsResponse } from "@/lib/solana/earn/earn-api";
import { SOLANA_USDC_MINT_MAINNET as USDC } from "@/lib/solana/constants";
export default function EarnMaxScreen() {
  const insets = useSafeAreaInsets();
  const isFocused = useIsFocused();
  const { publicKey } = useWallet();
  const { tokenHoldings, refreshTokenHoldings } = useTokenHoldings(publicKey);
  const {
    summary,
    activity,
    busy,
    run,
    error,
    publicApyBps,
    refresh,
    hasLoaded,
  } = useEarnMax();
  useEffect(() => {
    if (isFocused) {
      void refresh().catch(() => {});
      void refreshTokenHoldings(true);
    }
  }, [isFocused, refresh, refreshTokenHoldings]);
  const [sheet, setSheet] = useState<
    "deposit" | "withdraw" | "positions" | null
  >(null);
  const openDeposit = useCallback(() => {
    void Haptics.selectionAsync();
    void refreshTokenHoldings(true);
    setSheet("deposit");
  }, [refreshTokenHoldings]);
  useEffect(() => {
    if (!isFocused) setSheet(null);
  }, [isFocused]);
  const apy = summary?.forecastApyBps ?? publicApyBps;
  const balance = summary?.balanceUsd ?? (hasLoaded && !error ? 0 : undefined);
  const split = (balance ?? 0).toFixed(2).split(".");
  const walletUsdc = tokenHoldings.find((h) => h.mint === USDC)?.balance ?? 0;
  const earnings = useMemo<EarnEarningsResponse | null>(() => {
    if (
      !summary ||
      !activity ||
      summary.earnedUsd === null ||
      summary.coverage !== "complete"
    )
      return null;
    const days = buildEarnMaxDailySeries({
      performance: activity.performance,
      activity: activity.operations,
    }).days;
    return {
      bars: days.map((day) => ({
        apyBps: null,
        avgPrincipalUsd: 0,
        earnedUsd: day.earnedUsd,
        endAt: new Date(day.date.getTime() + 86400000).toISOString(),
        isCurrent: day.isCurrent,
        label: String(day.date.getUTCDate()),
        principalAmountRaw: "0",
        principalUsd: 0,
        startAt: day.date.toISOString(),
      })),
      currentApyBps: null,
      lastDepositAt: null,
      lifetimeEarnedUsd: summary.earnedUsd,
      principalAmountRaw: "0",
      principalUsd: 0,
      rangeEarnedUsd: days.reduce((sum, day) => sum + day.earnedUsd, 0),
      sinceLastDepositEarnedUsd: summary.earnedUsd,
      todayEarnedUsd: days.at(-1)?.earnedUsd ?? 0,
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
          apyLabel={apy == null ? "—" : `${(apy / 100).toFixed(2)}% APY`}
          onBack={() => router.navigate("/(tabs)")}
        />
        {error && (
          <Pressable
            onPress={() => void refresh()}
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
            history: summary?.apyHistory ?? [],
            apyBps: apy ?? null,
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
        depositDisabled={busy || !hasLoaded || !!error}
        withdrawDisabled={
          busy || !hasLoaded || !!error || (!summary?.withdrawal && !balance)
        }
        onPositions={() => {
          void Haptics.selectionAsync();
          setSheet("positions");
        }}
        onDeposit={openDeposit}
        onWithdraw={() => {
          void Haptics.selectionAsync();
          if (summary?.withdrawal) activityLink();
          else setSheet("withdraw");
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
          void refreshTokenHoldings(true);
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
          if (summary?.withdrawal)
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
          apyBps: apy ?? null,
          earnedUsd: summary?.earnedUsd ?? null,
          withdrawDisabled: busy || !!summary?.withdrawal || !balance,
        }}
        onDeposit={openDeposit}
        onWithdraw={() => setSheet("withdraw")}
      />
    </View>
  );
}
