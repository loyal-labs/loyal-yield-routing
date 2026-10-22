import { useMemo } from "react";
import { StyleSheet, type StyleProp, type ViewStyle } from "react-native";
import { ArrowLeft, ArrowUp, Plus } from "lucide-react-native";
import { Gesture, GestureDetector } from "react-native-gesture-handler";
import Animated from "react-native-reanimated";
import { useSafeAreaInsets } from "react-native-safe-area-context";
import { Skeleton } from "@/components/Skeleton";
import { Pressable, Text, View } from "@/tw";
import EarnFlash from "../../../assets/images/earn/flash.svg";
const COLOR_BADGE_GREEN = "#32B67C";
const COLOR_LABEL_DIM = "rgba(60, 60, 67, 0.6)";
const COLOR_BALANCE_DIM = "rgba(60, 60, 67, 0.4)";
const COLOR_WITHDRAW_BG = "#F5F5F5";
const TAB_BAR_RESERVED_HEIGHT = 96;

export function EarnHeader({
  title = "Earn",
  apyLabel,
  onBack,
}: {
  title?: string;
  apyLabel: string;
  onBack?: () => void;
}) {
  return (
    <View style={[styles.header, onBack && { paddingVertical: 8 }]}>
      {onBack && (
        <Pressable
          onPress={onBack}
          accessibilityRole="button"
          accessibilityLabel="Back to Home"
          style={{
            width: 44,
            height: 44,
            marginRight: 4,
            alignItems: "center",
            justifyContent: "center",
            borderRadius: 999,
            backgroundColor: "rgba(255,255,255,0.1)",
          }}
        >
          <ArrowLeft size={28} color="#FFF" />
        </Pressable>
      )}
      <Text style={[styles.title, onBack && { fontSize: 22, lineHeight: 28 }]}>
        {title}
      </Text>
      <View style={styles.badge}>
        {!onBack && <EarnFlash width={12} height={16} />}
        <Text style={styles.badgeText}>{apyLabel}</Text>
      </View>
    </View>
  );
}

export function EarnBalanceCard({
  label = "Earn Balance",
  whole,
  cents,
  hasDeposit,
  balanceLoading,
  onPositions,
  onDeposit,
  onWithdraw,
  onRefund,
  style,
  fullWidthActions = false,
  depositDisabled = false,
  withdrawDisabled = false,
}: {
  label?: string;
  whole: string;
  cents: string;
  hasDeposit: boolean;
  balanceLoading: boolean;
  onPositions: () => void;
  onDeposit: () => void;
  onWithdraw: () => void;
  onRefund?: () => void;
  style?: StyleProp<ViewStyle>;
  fullWidthActions?: boolean;
  depositDisabled?: boolean;
  withdrawDisabled?: boolean;
}) {
  const insets = useSafeAreaInsets();
  const footerSwipeGesture = useMemo(
    () =>
      Gesture.Pan()
        .enabled(hasDeposit)
        .activeOffsetY(-12)
        .runOnJS(true)
        .onStart(onPositions),
    [hasDeposit, onPositions]
  );
  return (
    <Animated.View
      style={[
        styles.bottomCard,
        { borderTopLeftRadius: 26, borderTopRightRadius: 26 },
        style,
        { paddingBottom: TAB_BAR_RESERVED_HEIGHT + insets.bottom },
      ]}
    >
      <GestureDetector gesture={footerSwipeGesture}>
        <Animated.View style={styles.footerInner}>
          {hasDeposit ? (
            <Pressable
              onPress={onPositions}
              accessibilityRole="button"
              accessibilityLabel="View current positions"
              hitSlop={8}
              style={styles.handleHit}
            >
              <View style={styles.dragHandle} />
            </Pressable>
          ) : null}

          <View style={styles.balanceRow}>
            <Text style={styles.balanceLabel}>{label}</Text>
            {balanceLoading ? (
              <Skeleton style={styles.balanceSkeleton} />
            ) : (
              <Text style={styles.balanceValue}>
                <Text style={styles.balanceValueStrong}>{whole}</Text>
                <Text style={styles.balanceValueDim}>{cents}</Text>
              </Text>
            )}
          </View>

          <View style={styles.actionRow}>
            <Pressable
              onPress={onDeposit}
              accessibilityRole="button"
              accessibilityLabel="Deposit"
              disabled={depositDisabled}
              style={({ pressed }) => [
                styles.depositButton,
                !hasDeposit || fullWidthActions
                  ? styles.depositButtonFull
                  : null,
                { opacity: depositDisabled ? 0.4 : pressed ? 0.8 : 1 },
              ]}
            >
              <View style={styles.iconWrap}>
                <Plus size={20} color="#FFF" strokeWidth={2.2} />
              </View>
              <Text style={styles.depositLabel}>Deposit</Text>
            </Pressable>
            {!hasDeposit && !!onRefund ? (
              <Pressable
                onPress={onRefund}
                accessibilityRole="button"
                accessibilityLabel="View refundable accounts"
                style={({ pressed }) => [
                  styles.refundActionButton,
                  { opacity: pressed ? 0.8 : 1 },
                ]}
              >
                <Text style={styles.withdrawLabel}>Refund SOL</Text>
              </Pressable>
            ) : null}
            {hasDeposit ? (
              <Pressable
                onPress={onWithdraw}
                accessibilityRole="button"
                accessibilityLabel="Withdraw"
                disabled={withdrawDisabled}
                style={({ pressed }) => [
                  styles.withdrawButton,
                  fullWidthActions && styles.depositButtonFull,
                  { opacity: withdrawDisabled ? 0.4 : pressed ? 0.8 : 1 },
                ]}
              >
                <View style={styles.iconWrap}>
                  <ArrowUp
                    size={20}
                    color="#000"
                    strokeWidth={2.2}
                    opacity={0.6}
                  />
                </View>
                <Text style={styles.withdrawLabel}>Withdraw</Text>
              </Pressable>
            ) : null}
          </View>
        </Animated.View>
      </GestureDetector>
    </Animated.View>
  );
}
const styles = StyleSheet.create({
  header: {
    flexDirection: "row",
    alignItems: "center",
    gap: 8,
    paddingVertical: 4,
    paddingHorizontal: 16,
  },
  title: {
    fontFamily: "Geist_600SemiBold",
    fontSize: 28,
    lineHeight: 32,
    color: "#FFF",
  },
  badge: {
    flexDirection: "row",
    alignItems: "center",
    justifyContent: "center",
    gap: 2,
    paddingHorizontal: 6,
    paddingVertical: 1,
    borderRadius: 8,
    backgroundColor: COLOR_BADGE_GREEN,
  },
  badgeText: {
    fontFamily: "Geist_400Regular",
    fontSize: 15,
    lineHeight: 20,
    color: "#FFF",
    letterSpacing: 0.06,
  },
  bottomCard: {
    backgroundColor: "#FFF",
    paddingHorizontal: 16,
    paddingTop: 8,
  },
  footerInner: {
    gap: 8,
  },
  handleHit: {
    alignItems: "center",
    justifyContent: "center",
    paddingTop: 8,
    paddingBottom: 10,
  },
  dragHandle: {
    width: 32,
    height: 4,
    borderRadius: 4,
    backgroundColor: "rgba(0, 0, 0, 0.24)",
  },
  balanceRow: {
    flexDirection: "column",
    gap: 2,
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
    color: COLOR_BALANCE_DIM,
  },
  balanceSkeleton: {
    height: 36,
    width: 150,
    marginVertical: 6,
    borderRadius: 10,
  },
  actionRow: {
    flexDirection: "row",
    alignItems: "center",
    gap: 7,
  },
  depositButton: {
    flexDirection: "row",
    alignItems: "center",
    justifyContent: "center",
    gap: 4,
    height: 44,
    paddingLeft: 12,
    paddingRight: 20,
    borderRadius: 78,
    backgroundColor: "#000",
    overflow: "hidden",
  },
  depositButtonFull: {
    flex: 1,
  },
  withdrawButton: {
    flexDirection: "row",
    alignItems: "center",
    justifyContent: "center",
    gap: 4,
    height: 44,
    paddingLeft: 12,
    paddingRight: 20,
    borderRadius: 78,
    backgroundColor: COLOR_WITHDRAW_BG,
    overflow: "hidden",
  },
  refundActionButton: {
    flex: 1,
    alignItems: "center",
    justifyContent: "center",
    height: 44,
    borderRadius: 78,
    backgroundColor: COLOR_WITHDRAW_BG,
  },
  iconWrap: {
    width: 28,
    height: 28,
    alignItems: "center",
    justifyContent: "center",
  },
  depositLabel: {
    fontFamily: "Geist_400Regular",
    fontSize: 17,
    lineHeight: 22,
    color: "#FFF",
  },
  withdrawLabel: {
    fontFamily: "Geist_400Regular",
    fontSize: 17,
    lineHeight: 22,
    color: "#000",
  },
});
