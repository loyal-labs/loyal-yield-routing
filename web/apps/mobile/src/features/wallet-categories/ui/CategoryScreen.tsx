import { useUnshield } from "@loyal-labs/wallet-core/hooks";
import * as Haptics from "expo-haptics";
import { useLocalSearchParams, useRouter } from "expo-router";
import { ArrowDown, ArrowLeft, ArrowUp, ScanLine } from "lucide-react-native";
import {
  type ComponentRef,
  useCallback,
  useMemo,
  useRef,
  useState,
} from "react";
import { View as MeasureView } from "react-native";
import { Gesture, GestureDetector } from "react-native-gesture-handler";
import Animated, {
  useAnimatedScrollHandler,
  useSharedValue,
} from "react-native-reanimated";
import { useSafeAreaInsets } from "react-native-safe-area-context";

import { ReceiveSheet } from "@/components/wallet/ReceiveSheet";
import { SendSheet } from "@/components/wallet/SendSheet";
import { SwapSheet } from "@/components/wallet/SwapSheet";
import { resolveShieldedRow } from "@/components/wallet/shielded-balance";
import { UnshieldSheet } from "@/components/wallet/UnshieldSheet";
import { buildTokenDetailHref } from "@/features/token-details/routes";
import { useSolPrice } from "@/hooks/wallet/useSolPrice";
import {
  useTokenDetailMints,
  useTokenDetails,
} from "@/hooks/wallet/useTokenDetails";
import { useTokenHoldings } from "@/hooks/wallet/useTokenHoldings";
import { useWalletBalance } from "@/hooks/wallet/useWalletBalance";
import { useWalletInit } from "@/hooks/wallet/useWalletInit";
import { getSolanaEnv } from "@/lib/solana/rpc/connection";
import {
  getDisplayTokenHoldings,
  getPairPositions,
} from "@/lib/solana/token-holdings/display-holdings";
import { useWallet } from "@/lib/wallet/wallet-provider";
import { Pressable, Text, View } from "@/tw";
import EllipsisIcon from "../../../../assets/images/icons/ellipsis.svg";
import {
  filterHoldingsByCategory,
  isStablecoinHolding,
  sumHoldingsUsd,
  type WalletCategory,
} from "../model/categorize";
import { splitUsd } from "../model/format";
import type { CardSourceRect } from "../routes";
import { ActionBarButton } from "./ActionBarButton";
import { CardExpandTransition } from "./CardExpandTransition";
import { CategoryAssetRow } from "./CategoryAssetRow";
import { CategoryCardFace } from "./CategoryCardFace";
import { CryptoGlyph, StablecoinsGlyph } from "./CategoryGlyphs";
import { type MoreActionsAnchor, MoreActionsSheet } from "./MoreActionsSheet";
import { ShieldedAssetRow } from "./ShieldedAssetRow";

const MUTED = "rgba(60, 60, 67, 0.6)";
const CENTS_DIM = "rgba(60, 60, 67, 0.4)";
// Opaque so the morph box fully covers the real card behind the transparent
// modal at t=0, avoiding a double-image as it expands.
const GRID_CARD_GRAY_ALPHA = 0.03;
const toOpaqueOverWhite = (alpha: number): string => {
  const channel = Math.round(255 * (1 - alpha));
  return `#${((channel << 16) | (channel << 8) | channel)
    .toString(16)
    .padStart(6, "0")
    .toUpperCase()}`;
};
const CARD_COLOR = toOpaqueOverWhite(GRID_CARD_GRAY_ALPHA);

export function CategoryScreen({ category }: { category: WalletCategory }) {
  const insets = useSafeAreaInsets();
  const router = useRouter();
  const { walletAddress } = useWalletInit();
  const { solBalanceLamports, refreshBalance } =
    useWalletBalance(walletAddress);
  const { solPriceUsd } = useSolPrice();
  const { tokenHoldings, isHoldingsLoading, refreshTokenHoldings } =
    useTokenHoldings(walletAddress);
  const { signer } = useWallet();
  const {
    balances: shieldedBalances,
    executeUnshield,
    refreshBalances: refreshShieldedBalances,
  } = useUnshield(signer, getSolanaEnv());

  const params = useLocalSearchParams<{
    sx?: string;
    sy?: string;
    sw?: string;
    sh?: string;
    su?: string;
  }>();
  const sourceRect = useMemo<CardSourceRect | undefined>(() => {
    const w = Number(params.sw);
    const h = Number(params.sh);
    if (!Number.isFinite(w) || !Number.isFinite(h) || w <= 0 || h <= 0) {
      return undefined;
    }
    return {
      x: Number(params.sx) || 0,
      y: Number(params.sy) || 0,
      width: w,
      height: h,
      usd: Number(params.su) || 0,
    };
  }, [params.sx, params.sy, params.sw, params.sh, params.su]);

  const scrollOffset = useSharedValue(0);
  const scrollHandler = useAnimatedScrollHandler((e) => {
    scrollOffset.value = e.contentOffset.y;
  });
  const scrollGesture = useMemo(() => Gesture.Native(), []);

  const tokenDetailMints = useTokenDetailMints(tokenHoldings, shieldedBalances);
  const tokenDetailsByMint = useTokenDetails(tokenDetailMints);

  const shieldedRows = useMemo(
    () =>
      shieldedBalances
        .map((b) => resolveShieldedRow(b, tokenHoldings, tokenDetailsByMint))
        .filter(
          (row) => isStablecoinHolding(row) === (category === "stablecoins"),
        ),
    [shieldedBalances, tokenHoldings, tokenDetailsByMint, category],
  );

  const displayHoldings = useMemo(
    () =>
      filterHoldingsByCategory(
        getDisplayTokenHoldings(tokenHoldings),
        category,
      ),
    [tokenHoldings, category],
  );
  const pairPositions = useMemo(
    () => getPairPositions(displayHoldings),
    [displayHoldings],
  );
  const totalUsd = useMemo(
    () => sumHoldingsUsd(displayHoldings),
    [displayHoldings],
  );
  const balance = splitUsd(totalUsd);

  const [isSendOpen, setIsSendOpen] = useState(false);
  const [scanOnOpen, setScanOnOpen] = useState(false);
  const [isReceiveOpen, setIsReceiveOpen] = useState(false);
  const [isSwapOpen, setIsSwapOpen] = useState(false);
  const [unshieldMint, setUnshieldMint] = useState<string | null>(null);
  const [isUnshieldOpen, setIsUnshieldOpen] = useState(false);
  const openUnshield = useCallback(
    (mint: string | null) => {
      void refreshShieldedBalances();
      setUnshieldMint(mint);
      setIsUnshieldOpen(true);
    },
    [refreshShieldedBalances],
  );
  const [isMoreOpen, setIsMoreOpen] = useState(false);
  const [moreAnchor, setMoreAnchor] = useState<MoreActionsAnchor | null>(null);
  const moreButtonRef = useRef<ComponentRef<typeof MeasureView>>(null);

  const refresh = useCallback(() => {
    void refreshBalance(true);
    void refreshTokenHoldings(true);
  }, [refreshBalance, refreshTokenHoldings]);

  const handleTokenPress = useCallback(
    (mint: string) => {
      if (process.env.EXPO_OS !== "web") {
        void Haptics.impactAsync(Haptics.ImpactFeedbackStyle.Light);
      }
      router.push(buildTokenDetailHref(mint));
    },
    [router],
  );

  const title = category === "stablecoins" ? "Stablecoins" : "Crypto";
  const glyph =
    category === "stablecoins" ? (
      <StablecoinsGlyph size={64} />
    ) : (
      <CryptoGlyph size={64} />
    );
  const initialMint = displayHoldings[0]?.mint;

  // Collapsed "card face" drawn over the box at the start/end of the morph —
  // mirrors the wallet grid card (glyph top, label + value bottom).
  const face = (
    <CategoryCardFace
      category={category}
      title={title}
      usd={sourceRect?.usd ?? 0}
    />
  );

  return (
    <>
      <CardExpandTransition
        sourceRect={sourceRect}
        cardColor={CARD_COLOR}
        face={face}
        scrollOffset={scrollOffset}
        scrollGesture={scrollGesture}
      >
        {({ close }) => (
          <>
            <View
              className="flex-row items-center px-4 pb-2"
              style={{ paddingTop: insets.top + 8 }}
            >
              <Pressable
                onPress={close}
                accessibilityRole="button"
                accessibilityLabel="Back"
                className="h-11 w-11 items-center justify-center rounded-full"
                style={{ backgroundColor: "#f2f2f7" }}
                hitSlop={8}
              >
                <ArrowLeft size={24} color="#1C1C1E" strokeWidth={2} />
              </Pressable>
              <Text
                className="ml-3 flex-1 text-[22px] font-semibold text-black"
                style={{ letterSpacing: -0.44, lineHeight: 28 }}
                numberOfLines={1}
              >
                {title}
              </Text>
              <Pressable
                onPress={() => {
                  void Haptics.selectionAsync();
                  setScanOnOpen(true);
                  setIsSendOpen(true);
                }}
                accessibilityRole="button"
                accessibilityLabel="Scan QR code"
                className="h-11 w-11 items-center justify-center rounded-full"
                hitSlop={8}
              >
                <ScanLine
                  size={28}
                  color="#3C3C43"
                  strokeWidth={1.8}
                  opacity={0.6}
                />
              </Pressable>
            </View>

            <GestureDetector gesture={scrollGesture}>
              <Animated.ScrollView
                style={{ flex: 1 }}
                contentContainerStyle={{ paddingBottom: 16 }}
                onScroll={scrollHandler}
                scrollEventThrottle={16}
              >
                <View className="flex-row items-center px-4 py-2">
                  <View className="py-2 pr-3">{glyph}</View>
                  <View className="flex-1">
                    <Text
                      className="text-[14px]"
                      style={{ color: MUTED, lineHeight: 20 }}
                    >
                      Balance
                    </Text>
                    <Text
                      className="text-[40px] font-semibold text-black"
                      style={{ letterSpacing: -0.44, lineHeight: 48 }}
                    >
                      {balance.whole}
                      <Text style={{ color: CENTS_DIM }}>{balance.cents}</Text>
                    </Text>
                  </View>
                </View>

                <View className="px-4 pb-2 pt-3">
                  <Text
                    className="text-[17px] font-semibold text-black"
                    style={{ letterSpacing: -0.187, lineHeight: 22 }}
                  >
                    Assets
                  </Text>
                </View>

                {displayHoldings.length === 0 ? (
                  <View className="px-4 py-6">
                    <Text
                      className="text-center text-[15px]"
                      style={{ color: MUTED }}
                    >
                      {isHoldingsLoading ? "Loading assets…" : "No assets yet"}
                    </Text>
                  </View>
                ) : (
                  displayHoldings.map((holding, index) => (
                    <CategoryAssetRow
                      key={holding.mint}
                      holding={holding}
                      detail={tokenDetailsByMint[holding.mint]}
                      variant={category}
                      groupPosition={pairPositions[index]}
                      onPress={() => handleTokenPress(holding.mint)}
                    />
                  ))
                )}

                {shieldedRows.length > 0 ? (
                  <>
                    <View className="px-4 pb-2 pt-3">
                      <Text
                        className="text-[17px] font-semibold text-black"
                        style={{ letterSpacing: -0.187, lineHeight: 22 }}
                      >
                        Shielded
                      </Text>
                    </View>
                    {shieldedRows.map((row) => (
                      <ShieldedAssetRow
                        key={row.mint}
                        row={row}
                        onPress={() => {
                          void Haptics.selectionAsync();
                          openUnshield(row.mint);
                        }}
                      />
                    ))}
                  </>
                ) : null}
              </Animated.ScrollView>
            </GestureDetector>

            <View
              className="flex-row items-center gap-2 px-4 pt-2"
              style={{ paddingBottom: insets.bottom + 8 }}
            >
              <ActionBarButton
                variant="primary"
                label="Send"
                icon={<ArrowUp size={24} color="#FFFFFF" strokeWidth={2} />}
                onPress={() => {
                  void Haptics.selectionAsync();
                  setScanOnOpen(false);
                  setIsSendOpen(true);
                }}
              />
              <ActionBarButton
                variant="secondary"
                label="Receive"
                icon={
                  <ArrowDown
                    size={24}
                    color="#3C3C43"
                    strokeWidth={2}
                    opacity={0.6}
                  />
                }
                onPress={() => {
                  void Haptics.selectionAsync();
                  setIsReceiveOpen(true);
                }}
              />
              <MeasureView ref={moreButtonRef} collapsable={false}>
                <ActionBarButton
                  variant="secondary"
                  icon={<EllipsisIcon width={24} height={24} />}
                  onPress={() => {
                    moreButtonRef.current?.measureInWindow(
                      (x, y, width, height) => {
                        setMoreAnchor({ x, y, width, height });
                        setIsMoreOpen(true);
                      },
                    );
                  }}
                />
              </MeasureView>
            </View>
          </>
        )}
      </CardExpandTransition>

      <SendSheet
        open={isSendOpen}
        onClose={() => setIsSendOpen(false)}
        solBalanceLamports={solBalanceLamports}
        solPriceUsd={solPriceUsd}
        tokenHoldings={tokenHoldings}
        tokenDetailsByMint={tokenDetailsByMint}
        onSendComplete={refresh}
        initialMint={initialMint}
        initialShowScanner={scanOnOpen}
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
        onSwapComplete={refresh}
        initialFromMint={initialMint}
      />

      <UnshieldSheet
        open={isUnshieldOpen}
        onClose={() => setIsUnshieldOpen(false)}
        balances={shieldedBalances}
        executeUnshield={executeUnshield}
        tokenHoldings={tokenHoldings}
        tokenDetailsByMint={tokenDetailsByMint}
        initialMint={unshieldMint}
        onUnshieldComplete={refresh}
      />

      <MoreActionsSheet
        open={isMoreOpen}
        onClose={() => setIsMoreOpen(false)}
        anchor={moreAnchor}
        onSend={() => {
          setScanOnOpen(false);
          setIsSendOpen(true);
        }}
        onReceive={() => setIsReceiveOpen(true)}
        onSwap={() => setIsSwapOpen(true)}
        onUnshield={
          shieldedBalances.length > 0 ? () => openUnshield(null) : undefined
        }
      />
    </>
  );
}
