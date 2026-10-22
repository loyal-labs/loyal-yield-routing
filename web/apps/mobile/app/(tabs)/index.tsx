import { useEarnEarnings } from "@/hooks/wallet/useEarnEarnings";
import { useEarnMax } from "@/features/earn-max/EarnMaxProvider";
import { useUnshield } from "@loyal-labs/wallet-core/hooks";
import { useLocalSearchParams, useRouter } from "expo-router";
import { ArrowDown, ArrowUp } from "lucide-react-native";
import {
  type ComponentRef,
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
} from "react";
import {
  ActivityIndicator,
  RefreshControl,
  View as MeasureView,
} from "react-native";
import { useSafeAreaInsets } from "react-native-safe-area-context";

import { LogoHeader } from "@/components/LogoHeader";
import {
  computeFirstDepositSolShortfall,
  DepositSheet,
} from "@/components/earn/DepositSheet";
import { getLoyalApyBps } from "@/components/earn/earnForecastModel";
import { nudgeQuestProgressCheck } from "@/components/quests/QuestCompletionWatcher";
import { AddEmailNudge } from "@/components/wallet/AddEmailNudge";
import { BalanceBackgroundPicker } from "@/components/wallet/BalanceBackgroundPicker";
import { BalanceCard } from "@/components/wallet/BalanceCard";
import { ReceiveSheet } from "@/components/wallet/ReceiveSheet";
import { SendSheet } from "@/components/wallet/SendSheet";
import { SwapSheet } from "@/components/wallet/SwapSheet";
import { UnshieldSheet } from "@/components/wallet/UnshieldSheet";
import { shouldShowWalletTopUp } from "@/components/wallet/wallet-screen-helpers";
import {
  filterHoldingsByCategory,
  sumHoldingsUsd,
} from "@/features/wallet-categories/model/categorize";
import {
  buildCryptoHref,
  buildEarnHref,
  buildStablecoinsHref,
} from "@/features/wallet-categories/routes";
import { ActionBarButton } from "@/features/wallet-categories/ui/ActionBarButton";
import {
  type MoreActionsAnchor,
  MoreActionsSheet,
} from "@/features/wallet-categories/ui/MoreActionsSheet";
import { WalletCategoryGrid } from "@/features/wallet-categories/ui/WalletCategoryGrid";
import { useEarnForecast } from "@/hooks/wallet/useEarnForecast";
import { useEarnPosition } from "@/hooks/wallet/useEarnPosition";
import { useSolPrice } from "@/hooks/wallet/useSolPrice";
import { useTokenDetails } from "@/hooks/wallet/useTokenDetails";
import { useTokenHoldings } from "@/hooks/wallet/useTokenHoldings";
import {
  useWalletAutoRefresh,
  type WalletRefreshReason,
} from "@/hooks/wallet/useWalletAutoRefresh";
import { useWalletBalance } from "@/hooks/wallet/useWalletBalance";
import { useWalletInit } from "@/hooks/wallet/useWalletInit";
import { track } from "@/lib/analytics/analytics";
import { PORTFOLIO_EVENTS } from "@/lib/analytics/portfolio-events";
import {
  LOYAL_TOKEN_MINT,
  NATIVE_SOL_MINT,
  SOLANA_USDC_MINT_DEVNET,
  SOLANA_USDC_MINT_MAINNET,
} from "@/lib/solana/constants";
import { env } from "@/config/env";
import { executeEarnDeposit } from "@/lib/solana/earn/deposit";
import { getEarnProductAssets } from "@/lib/solana/earn/earn-product-mints";
import { getSolanaEnv, onSolanaEnvChange } from "@/lib/solana/rpc/connection";
import { clearHoldingsCache } from "@/lib/solana/token-holdings/fetch-token-holdings";
import {
  getCachedBalanceBg,
  setCachedBalanceBg,
} from "@/lib/solana/wallet-cache";
import { DEFAULT_BALANCE_BACKGROUND_ID } from "@/lib/wallet/balance-backgrounds";
import { isWalletUnlocked, useWallet } from "@/lib/wallet/wallet-provider";
import { ScrollView, Text, View } from "@/tw";

import EllipsisIcon from "../../assets/images/icons/ellipsis.svg";

export default function WalletScreen() {
  const {
    summary: earnMaxSummary,
    publicApyBps: earnMaxApyBps,
    hasLoaded: earnMaxLoaded,
  } = useEarnMax();
  const insets = useSafeAreaInsets();
  const router = useRouter();
  const params = useLocalSearchParams<{ scan?: string }>();
  const { walletAddress, isLoading, walletError, retryWalletInit } =
    useWalletInit();
  const { earnings } = useEarnEarnings(walletAddress);
  const { solBalanceLamports, refreshBalance } =
    useWalletBalance(walletAddress);
  const { solPriceUsd } = useSolPrice();

  const { tokenHoldings, refreshTokenHoldings } =
    useTokenHoldings(walletAddress);
  const {
    position: earnPosition,
    policyMissing: earnPolicyMissing,
    isLoading: isEarnLoading,
    hasLoaded: earnLoaded,
    refreshEarnPosition,
  } = useEarnPosition(walletAddress);
  const earnLoading = walletAddress != null && (isEarnLoading || !earnLoaded);
  // Loyal APY forecast — the Earn card shows the same headline rate as the Earn
  // screen / APY chart / web, not the position's raw reserve supply APY.
  const forecastSummary = useEarnForecast();
  const { signer, state } = useWallet();
  const {
    balances: shieldedBalances,
    executeUnshield,
    refreshBalances: refreshShieldedBalances,
  } = useUnshield(signer, getSolanaEnv());

  const doFullRefresh = useCallback(
    async (reason: WalletRefreshReason) => {
      const forceOnChainState =
        reason === "manual" ||
        reason === "mutation" ||
        reason === "network-switch";

      await Promise.allSettled([
        refreshBalance(forceOnChainState),
        refreshTokenHoldings(forceOnChainState),
        refreshEarnPosition(),
      ]);
    },
    [refreshBalance, refreshTokenHoldings, refreshEarnPosition]
  );

  const { requestRefresh } = useWalletAutoRefresh({
    walletAddress,
    refresh: doFullRefresh,
  });

  const [networkLoading, setNetworkLoading] = useState(false);
  const [networkKey, setNetworkKey] = useState(0);
  const [isRefreshing, setIsRefreshing] = useState(false);
  const [tokenMarketRefreshKey, setTokenMarketRefreshKey] = useState(0);

  const tokenDetailMints = useMemo(() => {
    const mints = new Set<string>();
    mints.add(NATIVE_SOL_MINT);
    mints.add(LOYAL_TOKEN_MINT);
    mints.add(
      getSolanaEnv() === "mainnet"
        ? SOLANA_USDC_MINT_MAINNET
        : SOLANA_USDC_MINT_DEVNET
    );
    for (const holding of tokenHoldings) mints.add(holding.mint);
    for (const balance of shieldedBalances) mints.add(balance.tokenMint);
    return Array.from(mints);
  }, [tokenHoldings, shieldedBalances]);
  const tokenDetailsByMint = useTokenDetails(
    tokenDetailMints,
    tokenMarketRefreshKey
  );

  useEffect(() => {
    return onSolanaEnvChange(() => {
      clearHoldingsCache();
      setNetworkLoading(true);
      setNetworkKey((k) => k + 1);
      setTokenMarketRefreshKey((k) => k + 1);

      Promise.resolve(requestRefresh("network-switch")).finally(() =>
        setNetworkLoading(false)
      );
    });
  }, [requestRefresh]);

  const totalSolLamports = solBalanceLamports ?? 0;
  const totalPortfolioUsd = useMemo(() => {
    let total = 0;
    let hasValuation = false;

    for (const holding of tokenHoldings) {
      if (
        typeof holding.valueUsd === "number" &&
        Number.isFinite(holding.valueUsd)
      ) {
        total += holding.valueUsd;
        hasValuation = true;
        continue;
      }
      if (
        typeof holding.priceUsd === "number" &&
        Number.isFinite(holding.priceUsd) &&
        holding.priceUsd > 0
      ) {
        total += holding.balance * holding.priceUsd;
        hasValuation = true;
      }
    }

    // Earn lives in the vault, not in tokenHoldings — add it explicitly so the
    // headline balance equals the sum of the three overview buckets shown below
    // (Earn + Stablecoins + Crypto). Without this the headline silently omits
    // the Earn deposit and no longer reconciles with the category cards.
    const earnRaw = Number(earnPosition?.currentAmountRaw);
    if (Number.isFinite(earnRaw) && earnRaw > 0) {
      total += earnRaw / 1e6;
      hasValuation = true;
    }

    if (earnMaxSummary) {
      total +=
        earnMaxSummary.balanceUsd +
        Number(earnMaxSummary.withdrawal?.amountRaw ?? "0") / 1e6;
      hasValuation = true;
    }
    return hasValuation ? total : null;
  }, [tokenHoldings, earnPosition, earnMaxSummary]);

  const stablecoinsUsd = useMemo(
    () =>
      sumHoldingsUsd(filterHoldingsByCategory(tokenHoldings, "stablecoins")),
    [tokenHoldings]
  );
  const cryptoUsd = useMemo(
    () => sumHoldingsUsd(filterHoldingsByCategory(tokenHoldings, "crypto")),
    [tokenHoldings]
  );
  const earnUsd = useMemo(() => {
    const raw = Number(earnPosition?.currentAmountRaw);
    return Number.isFinite(raw) ? raw / 1e6 : 0;
  }, [earnPosition]);
  const earnApyBps = useMemo(() => {
    if (!earnPosition) {
      return null;
    }
    return getLoyalApyBps(forecastSummary);
  }, [earnPosition, forecastSummary]);

  // First-deposit SOL gate: opening the first Earn position costs SOL
  // (rent/fees), so with no existing position the wallet must hold
  // FIRST_DEPOSIT_MIN_SOL. Null while the position/balance are loading — the
  // gate fails open.
  const firstDepositSolShortfall = useMemo(() => {
    // A balance alone doesn't prove the cheap top-up path applies: a missing
    // route-policy pair (post-full-exit orphans) sends even a top-up down
    // first-time setup with its SOL cost — gate on the policy read too.
    if (!earnLoaded || (earnUsd > 0 && !earnPolicyMissing)) return null;
    return computeFirstDepositSolShortfall(
      solBalanceLamports != null ? solBalanceLamports / 1e9 : null
    );
  }, [earnLoaded, earnUsd, earnPolicyMissing, solBalanceLamports]);

  // Wallet stablecoin balances feed the Deposit sheet's coin selector and its
  // available/insufficient state (token units ≈ dollars for every 6-decimal
  // Earn product stablecoin).
  const stableBalancesByMint = useMemo(() => {
    const balances: Record<string, number> = {};
    for (const asset of getEarnProductAssets(env.solanaEnv)) {
      const holding = tokenHoldings.find((h) => h.mint === asset.mint);
      if (holding && Number.isFinite(holding.balance)) {
        balances[asset.mint] = holding.balance;
      }
    }
    return balances;
  }, [tokenHoldings]);

  const [isSendOpen, setIsSendOpen] = useState(false);
  const [sendWithScanner, setSendWithScanner] = useState(false);
  const [isReceiveOpen, setIsReceiveOpen] = useState(false);
  const [isSwapOpen, setIsSwapOpen] = useState(false);
  const [isUnshieldOpen, setIsUnshieldOpen] = useState(false);
  const [isMoreOpen, setIsMoreOpen] = useState(false);
  const [moreAnchor, setMoreAnchor] = useState<MoreActionsAnchor | null>(null);
  const moreButtonRef = useRef<ComponentRef<typeof MeasureView>>(null);
  const [isBgPickerOpen, setIsBgPickerOpen] = useState(false);
  const [isDepositOpen, setIsDepositOpen] = useState(false);
  const [balanceBg, setBalanceBg] = useState<string | null>(() => {
    const cached = getCachedBalanceBg();
    return cached !== undefined ? cached : DEFAULT_BALANCE_BACKGROUND_ID;
  });

  const openSend = useCallback((withScanner: boolean) => {
    setSendWithScanner(withScanner);
    setIsSendOpen(true);
  }, []);

  const handledScanRef = useRef<string | null>(null);
  useEffect(() => {
    const token = params.scan;
    if (!token || token === handledScanRef.current) return;
    handledScanRef.current = token;
    openSend(true);
    router.setParams({ scan: "" });
  }, [params.scan, openSend, router]);

  const handleRefresh = useCallback(async () => {
    setIsRefreshing(true);
    setTokenMarketRefreshKey((k) => k + 1);

    const REFRESH_DEADLINE_MS = 15_000;
    const work = requestRefresh("manual");
    const deadline = new Promise<"deadline">((resolve) =>
      setTimeout(() => resolve("deadline"), REFRESH_DEADLINE_MS)
    );

    try {
      const outcome = await Promise.race([
        Promise.resolve(work).then(() => "done" as const),
        deadline,
      ]);
      if (outcome === "deadline") {
        console.warn(
          "[wallet-refresh] deadline hit; some requests still pending"
        );
      }
    } finally {
      setIsRefreshing(false);
    }
  }, [requestRefresh]);

  const handleSendComplete = useCallback(() => {
    void requestRefresh("mutation");
  }, [requestRefresh]);

  const handleSwapComplete = useCallback(() => {
    void requestRefresh("mutation");
  }, [requestRefresh]);

  const handleBgSelect = useCallback((bg: string | null) => {
    setBalanceBg(bg);
    setCachedBalanceBg(bg);
  }, []);

  const handleDepositConfirmed = useCallback(
    async (amountUsd: number, mint: string) => {
      if (!signer || !isWalletUnlocked(state)) {
        throw new Error("Unlock your wallet to deposit.");
      }
      await executeEarnDeposit({ signer, amountUsd, mint });
      void refreshEarnPosition();
      nudgeQuestProgressCheck();
    },
    [signer, state, refreshEarnPosition]
  );

  const showTopUpAction = useMemo(
    () =>
      shouldShowWalletTopUp({
        totalSolLamports,
        holdings: tokenHoldings,
        isLoading,
        networkLoading,
        walletError,
      }),
    [totalSolLamports, tokenHoldings, isLoading, networkLoading, walletError]
  );

  if (isLoading && !walletAddress) {
    return (
      <View className="flex-1 bg-white">
        <LogoHeader />
        <View className="flex-1 items-center justify-center py-20">
          <ActivityIndicator size="large" color="#000" />
          <Text
            className="mt-3 text-[15px]"
            style={{ color: "rgba(60, 60, 67, 0.6)" }}
          >
            Loading wallet...
          </Text>
        </View>
      </View>
    );
  }

  return (
    <View className="flex-1 bg-white">
      <LogoHeader showLibrary={false} onScanPress={() => openSend(true)} />
      {/* Fits the viewport and stretches (grid rows compress before anything
          scrolls); the ScrollView stays for pull-to-refresh and as overflow
          fallback on screens shorter than the grid's compression floor.
          alwaysBounceVertical keeps the refresh gesture working when content
          fits exactly and there is nothing to scroll. */}
      <ScrollView
        className="flex-1"
        contentContainerStyle={{
          flexGrow: 1,
          paddingBottom: insets.bottom + 78,
        }}
        alwaysBounceVertical
        showsVerticalScrollIndicator={false}
        refreshControl={
          <RefreshControl refreshing={isRefreshing} onRefresh={handleRefresh} />
        }
      >
        <View className="flex-1">
          <AddEmailNudge />
          <BalanceCard
            key={networkKey}
            walletAddress={walletAddress}
            solBalanceLamports={totalSolLamports}
            totalPortfolioUsd={totalPortfolioUsd}
            isLoading={isLoading || networkLoading}
            walletError={walletError}
            onRetry={retryWalletInit}
            showTopUpAction={showTopUpAction}
            onTopUpPress={() => setIsReceiveOpen(true)}
            balanceBg={balanceBg}
            onOpenBgPicker={() => setIsBgPickerOpen(true)}
          />

          <View style={{ flex: 1, marginTop: 16 }}>
            <WalletCategoryGrid
              earnUsd={earnUsd}
              earnMonthUsd={earnings?.rangeEarnedUsd ?? null}
              earnLoading={earnLoading}
              earnApyBps={earnApyBps}
              earnMaxUsd={earnMaxSummary?.balanceUsd ?? null}
              earnMaxLoading={walletAddress != null && !earnMaxLoaded}
              earnMaxApyBps={earnMaxSummary?.forecastApyBps ?? earnMaxApyBps}
              onPressEarnMax={() => router.navigate("/(tabs)/earn-max")}
              stablecoinsUsd={stablecoinsUsd}
              cryptoUsd={cryptoUsd}
              onPressEarn={() => router.navigate(buildEarnHref())}
              onPressStablecoins={(rect) =>
                router.push(buildStablecoinsHref(rect))
              }
              onPressCrypto={(rect) => router.push(buildCryptoHref(rect))}
            />
          </View>

          <View
            className="flex-row items-center gap-2 px-4"
            style={{ paddingTop: 16 }}
          >
            <ActionBarButton
              variant="primary"
              label="Send"
              icon={<ArrowUp size={28} color="#FFFFFF" strokeWidth={2} />}
              onPress={() => {
                track(PORTFOLIO_EVENTS.openSend);
                openSend(false);
              }}
            />
            <ActionBarButton
              variant="secondary"
              label="Receive"
              icon={
                <ArrowDown
                  size={28}
                  color="#3C3C43"
                  strokeWidth={2}
                  opacity={0.6}
                />
              }
              onPress={() => {
                track(PORTFOLIO_EVENTS.openReceive);
                setIsReceiveOpen(true);
              }}
            />
            <MeasureView ref={moreButtonRef} collapsable={false}>
              <ActionBarButton
                variant="secondary"
                icon={<EllipsisIcon width={28} height={28} />}
                onPress={() => {
                  moreButtonRef.current?.measureInWindow(
                    (x, y, width, height) => {
                      setMoreAnchor({ x, y, width, height });
                      setIsMoreOpen(true);
                    }
                  );
                }}
              />
            </MeasureView>
          </View>
        </View>
      </ScrollView>

      <SendSheet
        open={isSendOpen}
        onClose={() => setIsSendOpen(false)}
        solBalanceLamports={solBalanceLamports}
        solPriceUsd={solPriceUsd}
        tokenHoldings={tokenHoldings}
        tokenDetailsByMint={tokenDetailsByMint}
        onSendComplete={handleSendComplete}
        initialShowScanner={sendWithScanner}
      />

      <ReceiveSheet
        open={isReceiveOpen}
        onClose={() => setIsReceiveOpen(false)}
        walletAddress={walletAddress}
      />

      <SwapSheet
        open={isSwapOpen}
        onClose={() => setIsSwapOpen(false)}
        walletAddress={walletAddress}
        tokenHoldings={tokenHoldings}
        tokenDetailsByMint={tokenDetailsByMint}
        onSwapComplete={handleSwapComplete}
      />

      <UnshieldSheet
        open={isUnshieldOpen}
        onClose={() => setIsUnshieldOpen(false)}
        balances={shieldedBalances}
        executeUnshield={executeUnshield}
        tokenHoldings={tokenHoldings}
        tokenDetailsByMint={tokenDetailsByMint}
        onUnshieldComplete={() => void requestRefresh("mutation")}
      />

      <MoreActionsSheet
        open={isMoreOpen}
        onClose={() => setIsMoreOpen(false)}
        anchor={moreAnchor}
        onSend={() => {
          track(PORTFOLIO_EVENTS.openSend);
          openSend(false);
        }}
        onReceive={() => {
          track(PORTFOLIO_EVENTS.openReceive);
          setIsReceiveOpen(true);
        }}
        onSwap={() => {
          track(PORTFOLIO_EVENTS.openSwap);
          setIsSwapOpen(true);
        }}
        onUnshield={
          shieldedBalances.length > 0
            ? () => {
                void refreshShieldedBalances();
                setIsUnshieldOpen(true);
              }
            : undefined
        }
      />

      <BalanceBackgroundPicker
        open={isBgPickerOpen}
        onClose={() => setIsBgPickerOpen(false)}
        selectedBg={balanceBg}
        onSelect={handleBgSelect}
      />

      <DepositSheet
        open={isDepositOpen}
        onClose={() => setIsDepositOpen(false)}
        onDeposit={handleDepositConfirmed}
        stableBalancesByMint={stableBalancesByMint}
        firstDepositSolShortfall={firstDepositSolShortfall}
        isFirstDeposit={earnLoaded && (earnUsd <= 0 || earnPolicyMissing)}
      />
    </View>
  );
}
