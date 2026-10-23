import Loop from "../../../assets/images/earn-max/loop.svg";
import MaxPlus from "../../../assets/images/earn-max/plus.svg";
import MaxWithdraw from "../../../assets/images/earn-max/withdraw.svg";
import {
  BottomSheetBackdrop,
  BottomSheetModal,
  BottomSheetScrollView,
} from "@gorhom/bottom-sheet";
import * as Haptics from "expo-haptics";
import { ArrowUp, Plus } from "lucide-react-native";
import { useCallback, useEffect, useMemo, useRef } from "react";
import {
  Image,
  Pressable,
  StyleSheet,
  Text,
  View,
  useWindowDimensions,
} from "react-native";
import { useSafeAreaInsets } from "react-native-safe-area-context";
import { useFixedSheetLayout } from "@/hooks/useFixedSheetLayout";

import { splitUsdCents } from "@/lib/format/usd";
import type { EarnWithdrawSourceInfo } from "@/lib/solana/earn/earn-api";
import { resolveEarnPositionDisplay } from "@/lib/solana/earn/earn-position-display";
import { resolveTokenIcon } from "@/lib/solana/token-holdings/resolve-token-info";

import { VenueBadge } from "./venueBadge";

// Reveals the wallet's current Earn positions (Figma 74-21132). Opens from the
// drag handle on the Earn balance footer with the same mechanics as the
// Deposit/Withdraw sheets (a fixed-snap `BottomSheetModal` — dynamic sizing is
// avoided because it measures to zero on the New Architecture). Reproduces the
// footer (balance + Deposit/Withdraw) above the positions list so the sheet
// reads as the footer expanded upward. Each position pairs the underlying token
// icon with its Kamino market venue badge.
const COLOR_LABEL_DIM = "rgba(60, 60, 67, 0.6)";
const COLOR_VALUE_DIM = "rgba(60, 60, 67, 0.4)";
const COLOR_WITHDRAW_BG = "#F5F5F5";

function sourceUsd(source: EarnWithdrawSourceInfo): number {
  const raw = Number(source.amountRaw);
  return Number.isFinite(raw) ? raw / 1_000_000 : 0;
}

type PositionsSheetProps = {
  open: boolean;
  earnMax?: {
    apyBps: number | null;
    earnedUsd: number | null;
    withdrawDisabled: boolean;
  };
  onClose: () => void;
  balanceWhole: string;
  balanceCents: string;
  sources: EarnWithdrawSourceInfo[];
  onDeposit: () => void;
  onWithdraw: () => void;
};

export function PositionsSheet({
  open,
  earnMax,
  onClose,
  balanceWhole,
  balanceCents,
  sources,
  onDeposit,
  onWithdraw,
}: PositionsSheetProps) {
  const sheetRef = useRef<BottomSheetModal>(null);
  const insets = useSafeAreaInsets();
  const regularLayout = useFixedSheetLayout(0.92);
  const { height } = useWindowDimensions();
  const bottomInset = earnMax ? 70 + Math.max(insets.bottom, 12) : 0;
  const sheetHeight = earnMax
    ? height - insets.top - 60 - bottomInset
    : regularLayout.sheetHeight;
  const snapPoints = useMemo(() => [sheetHeight], [sheetHeight]);

  useEffect(() => {
    if (open) {
      sheetRef.current?.present();
    } else {
      sheetRef.current?.dismiss();
    }
  }, [open]);

  const positions = useMemo(
    () => [...sources].sort((a, b) => sourceUsd(b) - sourceUsd(a)),
    [sources]
  );

  const handleDeposit = useCallback(() => {
    void Haptics.selectionAsync();
    onDeposit();
  }, [onDeposit]);

  const handleWithdraw = useCallback(() => {
    void Haptics.selectionAsync();
    onWithdraw();
  }, [onWithdraw]);

  const renderBackdrop = useCallback(
    (props: React.ComponentProps<typeof BottomSheetBackdrop>) => (
      <BottomSheetBackdrop
        {...props}
        disappearsOnIndex={-1}
        appearsOnIndex={0}
        opacity={0.2}
      />
    ),
    []
  );

  return (
    <BottomSheetModal
      ref={sheetRef}
      snapPoints={snapPoints}
      enableDynamicSizing={false}
      enablePanDownToClose
      backdropComponent={earnMax ? undefined : renderBackdrop}
      bottomInset={bottomInset}
      onDismiss={onClose}
      handleComponent={null}
      backgroundStyle={styles.sheetBackground}
    >
      <BottomSheetScrollView
        style={{ height: sheetHeight }}
        contentContainerStyle={{ paddingBottom: insets.bottom + 16 }}
        showsVerticalScrollIndicator={false}
      >
        <View style={styles.handleRow}>
          <View style={styles.dragHandle} />
        </View>

        <View
          style={[styles.balanceCell, earnMax && { paddingTop: 0, gap: 0 }]}
        >
          <Text style={styles.balanceLabel}>Earn Balance</Text>
          <Text style={styles.balanceValue}>
            <Text style={styles.balanceValueStrong}>{balanceWhole}</Text>
            <Text style={styles.balanceValueDim}>{balanceCents}</Text>
          </Text>
        </View>

        <View style={styles.actionRow}>
          <Pressable
            onPress={handleDeposit}
            accessibilityRole="button"
            accessibilityLabel="Deposit"
            style={({ pressed }) => [
              styles.depositButton,
              { opacity: pressed ? 0.8 : 1 },
            ]}
          >
            <View style={styles.iconWrap}>
              {earnMax ? (
                <MaxPlus />
              ) : (
                <Plus size={20} color="#FFF" strokeWidth={2.2} />
              )}
            </View>
            <Text style={styles.depositLabel}>Deposit</Text>
          </Pressable>
          <Pressable
            disabled={earnMax?.withdrawDisabled}
            onPress={handleWithdraw}
            accessibilityRole="button"
            accessibilityLabel="Withdraw"
            style={({ pressed }) => [
              styles.withdrawButton,
              { opacity: pressed ? 0.8 : 1 },
            ]}
          >
            <View style={styles.iconWrap}>
              {earnMax ? (
                <MaxWithdraw />
              ) : (
                <ArrowUp
                  size={20}
                  color="#000"
                  strokeWidth={2.2}
                  opacity={0.6}
                />
              )}
            </View>
            <Text style={styles.withdrawLabel}>Withdraw</Text>
          </Pressable>
        </View>

        <View
          style={[
            styles.groupHeader,
            earnMax && { paddingTop: 13, paddingBottom: 9 },
          ]}
        >
          <Text style={styles.groupHeaderText}>Current positions</Text>
        </View>

        {earnMax ? (
          <View style={styles.positionCell}>
            <View
              style={{
                width: 48,
                height: 48,
                marginRight: 12,
                borderRadius: 12,
                backgroundColor: "rgba(0,0,0,0.04)",
                alignItems: "center",
                justifyContent: "center",
              }}
            >
              <Loop />
            </View>
            <View
              style={{
                flex: 1,
                flexDirection: "row",
                alignItems: "center",
                gap: 4,
              }}
            >
              <Text style={styles.positionTitle}>Looping</Text>
              <View style={styles.maxApyBadge}>
                <Text style={styles.maxApyText}>
                  {earnMax.apyBps == null
                    ? "—"
                    : `${(earnMax.apyBps / 100).toFixed(2)}% APY`}
                </Text>
              </View>
            </View>
            <View style={{ alignItems: "flex-end", paddingVertical: 11 }}>
              <Text style={styles.positionValue}>
                {balanceWhole}
                <Text style={styles.positionValueDim}>{balanceCents}</Text>
              </Text>
              <Text
                style={[
                  styles.positionSubtitle,
                  {
                    color:
                      earnMax.earnedUsd != null && earnMax.earnedUsd < 0
                        ? "#F9363C"
                        : "#4BB34B",
                  },
                ]}
              >
                {earnMax.earnedUsd == null
                  ? "—"
                  : `${earnMax.earnedUsd < 0 ? "−" : "+"}$${Math.abs(
                      earnMax.earnedUsd
                    ).toFixed(2)}`}
              </Text>
            </View>
          </View>
        ) : null}
        {positions.map((source) => {
          const { marketName, mintSymbol, venue } = resolveEarnPositionDisplay({
            market: source.market,
            liquidityMint: source.liquidityMint,
          });
          const value = splitUsdCents(sourceUsd(source));
          const tokenIcon = resolveTokenIcon({ mint: source.liquidityMint });
          return (
            <View key={source.id} style={styles.positionCell}>
              <View style={styles.iconStack}>
                <Image source={{ uri: tokenIcon }} style={styles.tokenIcon} />
                <View style={styles.venueAnchor}>
                  <VenueBadge venue={venue} />
                </View>
              </View>
              <View style={styles.positionMiddle}>
                <Text style={styles.positionTitle} numberOfLines={1}>
                  {marketName}
                </Text>
                <Text style={styles.positionSubtitle} numberOfLines={1}>
                  {mintSymbol}
                </Text>
              </View>
              <Text style={styles.positionValue} numberOfLines={1}>
                {value.whole}
                <Text style={styles.positionValueDim}>{value.cents}</Text>
              </Text>
            </View>
          );
        })}
      </BottomSheetScrollView>
    </BottomSheetModal>
  );
}

const styles = StyleSheet.create({
  maxApyBadge: {
    paddingHorizontal: 4,
    paddingTop: 2,
    paddingBottom: 1,
    borderRadius: 6,
    backgroundColor: "rgba(52,199,89,0.14)",
  },
  maxApyText: {
    fontFamily: "Geist_500Medium",
    fontSize: 11,
    lineHeight: 13,
    letterSpacing: 0.06,
    color: "#4BB34B",
  },
  sheetBackground: {
    backgroundColor: "#FFF",
    borderTopLeftRadius: 26,
    borderTopRightRadius: 26,
  },
  handleRow: {
    alignItems: "center",
    paddingTop: 6,
    paddingBottom: 6,
  },
  dragHandle: {
    width: 32,
    height: 4,
    borderRadius: 4,
    backgroundColor: "rgba(0, 0, 0, 0.24)",
  },
  balanceCell: {
    flexDirection: "column",
    gap: 2,
    paddingHorizontal: 16,
    paddingTop: 10,
    paddingBottom: 8,
  },
  balanceLabel: {
    fontFamily: "Geist_400Regular",
    fontSize: 15,
    lineHeight: 20,
    color: COLOR_LABEL_DIM,
  },
  balanceValue: {
    fontFamily: "Geist_600SemiBold",
    fontSize: 36,
    lineHeight: 48,
    letterSpacing: -0.4,
  },
  balanceValueStrong: {
    fontFamily: "Geist_600SemiBold",
    fontSize: 36,
    lineHeight: 48,
    color: "#000",
  },
  balanceValueDim: {
    fontFamily: "Geist_600SemiBold",
    fontSize: 36,
    lineHeight: 48,
    color: COLOR_VALUE_DIM,
  },
  actionRow: {
    flexDirection: "row",
    alignItems: "center",
    gap: 8,
    paddingHorizontal: 16,
    paddingTop: 8,
    paddingBottom: 16,
  },
  depositButton: {
    flex: 1,
    flexDirection: "row",
    alignItems: "center",
    justifyContent: "center",
    gap: 4,
    height: 50,
    paddingLeft: 8,
    paddingRight: 20,
    borderRadius: 78,
    backgroundColor: "#000",
    overflow: "hidden",
  },
  withdrawButton: {
    flex: 1,
    flexDirection: "row",
    alignItems: "center",
    justifyContent: "center",
    gap: 4,
    height: 50,
    paddingLeft: 8,
    paddingRight: 20,
    borderRadius: 78,
    backgroundColor: COLOR_WITHDRAW_BG,
    overflow: "hidden",
  },
  iconWrap: {
    width: 28,
    height: 28,
    alignItems: "center",
    justifyContent: "center",
  },
  depositLabel: {
    fontFamily: "Geist_500Medium",
    fontSize: 17,
    lineHeight: 22,
    color: "#FFF",
  },
  withdrawLabel: {
    fontFamily: "Geist_500Medium",
    fontSize: 17,
    lineHeight: 22,
    color: "#000",
  },
  groupHeader: {
    paddingHorizontal: 16,
    paddingTop: 12,
    paddingBottom: 8,
  },
  groupHeaderText: {
    fontFamily: "Geist_600SemiBold",
    fontSize: 17,
    lineHeight: 22,
    letterSpacing: -0.187,
    color: "#000",
  },
  positionCell: {
    flexDirection: "row",
    alignItems: "center",
    paddingHorizontal: 16,
  },
  iconStack: {
    width: 48,
    height: 48,
    marginRight: 12,
  },
  tokenIcon: {
    position: "absolute",
    top: 0,
    left: 0,
    width: 32,
    height: 32,
    borderRadius: 16,
  },
  venueAnchor: {
    position: "absolute",
    bottom: 0,
    right: 0,
  },
  positionMiddle: {
    flex: 1,
    paddingVertical: 8,
  },
  positionTitle: {
    fontFamily: "Geist_500Medium",
    fontSize: 17,
    lineHeight: 22,
    letterSpacing: -0.187,
    color: "#000",
  },
  positionSubtitle: {
    fontFamily: "Geist_400Regular",
    fontSize: 15,
    lineHeight: 20,
    color: COLOR_LABEL_DIM,
  },
  positionValue: {
    fontFamily: "Geist_500Medium",
    fontSize: 17,
    lineHeight: 22,
    letterSpacing: -0.187,
    color: "#000",
    textAlign: "right",
    paddingLeft: 12,
  },
  positionValueDim: {
    fontFamily: "Geist_500Medium",
    fontSize: 17,
    lineHeight: 22,
    color: COLOR_VALUE_DIM,
  },
});
