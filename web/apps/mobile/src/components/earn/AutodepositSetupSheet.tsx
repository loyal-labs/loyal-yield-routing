import {
  BottomSheetBackdrop,
  BottomSheetModal,
  BottomSheetTextInput,
  BottomSheetView,
} from "@gorhom/bottom-sheet";
import * as Haptics from "expo-haptics";
import { Info, Trash2, Wallet, X } from "lucide-react-native";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  ActivityIndicator,
  Keyboard,
  Pressable,
  StyleSheet,
  Text,
  View,
} from "react-native";
import Animated, {
  useAnimatedKeyboard,
  useAnimatedStyle,
} from "react-native-reanimated";
import { useSafeAreaInsets } from "react-native-safe-area-context";
import {
  amountFieldStyles,
  amountToUsd,
  computeSolShortfall,
  fitUsdAmountFontSize,
  focusInputAfterSheetSettles,
  formatAmountDisplay,
  formatSolAmountCompact,
  formatSolTopUpCta,
  presentSheetWhenOpen,
  sanitizeAmount,
  solShortfallBlocks,
  useCaretBlink,
} from "@/components/sheets/sheetFacts";
import { useFixedSheetLayout } from "@/hooks/useFixedSheetLayout";
import { useKeyboardRescueFocus } from "@/hooks/useKeyboardRescueFocus";

import AutodepositIcon from "../../../assets/images/earn/autodeposit.svg";

const COLOR_BLACK = "#000";
const COLOR_LABEL_DIM = "rgba(60, 60, 67, 0.6)";
const COLOR_CENTS_DIM = "rgba(60, 60, 67, 0.4)";
const COLOR_CHIP_BG = "#F2F2F7";
const COLOR_RED = "#F9363C";
const COLOR_WALLET_GREEN = "#32B67C";
const COLOR_CONNECTOR = "rgba(0, 0, 0, 0.14)";

function splitDollars(value: number): { whole: string; cents: string } {
  const [whole, cents] = value.toFixed(2).split(".");
  return {
    whole: `$${Number(whole).toLocaleString("en-US")}`,
    cents: `.${cents}`,
  };
}

function shortenAddress(address: string | null): string {
  if (!address || address.length <= 9) {
    return address ?? "";
  }
  return `${address.slice(0, 4)}...${address.slice(-4)}`;
}

// Creating an Autodeposit stands up on-chain accounts (subscription authority,
// policy, recurring delegation) whose rent + fees total ~0.014 SOL measured on
// mainnet; gate at 0.02 for headroom. Most of it comes back as a rent refund
// when the Autodeposit is deleted.
const SETUP_MIN_SOL = 0.02;

export function computeAutodepositSetupSolShortfall(
  solBalance: number | null,
): number | null {
  return computeSolShortfall(SETUP_MIN_SOL, solBalance);
}

export function AutodepositSetupSheet({
  open,
  onClose,
  onConfirm,
  onDelete,
  mode,
  initialThresholdUsd,
  availableUsdc,
  earnBalanceUsd,
  walletAddress,
  setupSolShortfall,
}: {
  open: boolean;
  onClose: () => void;
  onConfirm: (thresholdUsd: number) => void | Promise<void>;
  onDelete?: () => void | Promise<void>;
  mode: "create" | "edit";
  initialThresholdUsd: number | null;
  availableUsdc: number | null;
  earnBalanceUsd: number | null;
  walletAddress: string | null;
  setupSolShortfall?: number | null;
}) {
  const sheetRef = useRef<BottomSheetModal>(null);
  const inputRef = useRef<{ focus: () => void; blur?: () => void } | null>(
    null,
  );
  const { openKeyboard } = useKeyboardRescueFocus(inputRef);
  const insets = useSafeAreaInsets();
  const [amount, setAmount] = useState("");
  const [isFocused, setIsFocused] = useState(false);
  const caretOn = useCaretBlink(isFocused);
  const [submitting, setSubmitting] = useState(false);
  const [submitError, setSubmitError] = useState<string | null>(null);

  const { sheetHeight, snapPoints } = useFixedSheetLayout();
  const balance = Number.isFinite(availableUsdc ?? NaN)
    ? (availableUsdc as number)
    : 0;
  const earnBalance = Number.isFinite(earnBalanceUsd ?? NaN)
    ? (earnBalanceUsd as number)
    : 0;

  // Present/dismiss follows `open` ALONE. Deleting an autodeposit refreshes
  // state so `initialThresholdUsd` drops to null mid-close; if that re-ran this
  // effect while `open` were still true, it would re-present a sheet the user
  // just closed (the delete dismisses imperatively before onClose propagates).
  useEffect(() => {
    presentSheetWhenOpen(sheetRef.current, open);
  }, [open]);

  useEffect(() => {
    if (!open) return;
    setAmount(
      mode === "edit" && initialThresholdUsd != null && initialThresholdUsd > 0
        ? String(initialThresholdUsd)
        : "",
    );
    setSubmitError(null);
    setSubmitting(false);
  }, [open, mode, initialThresholdUsd]);

  const handleSheetChange = useCallback((index: number) => {
    focusInputAfterSheetSettles(() => inputRef.current?.focus(), index);
  }, []);

  const handleAmountChange = useCallback((text: string) => {
    setAmount(sanitizeAmount(text));
  }, []);

  const handleClose = useCallback(() => {
    Keyboard.dismiss();
    sheetRef.current?.dismiss();
  }, []);

  const handleDelete = useCallback(async () => {
    if (submitting) {
      return;
    }
    void Haptics.notificationAsync(Haptics.NotificationFeedbackType.Warning);
    Keyboard.dismiss();
    setSubmitError(null);
    const result = onDelete?.();
    if (!(result instanceof Promise)) {
      sheetRef.current?.dismiss();
      return;
    }
    setSubmitting(true);
    try {
      await result;
      sheetRef.current?.dismiss();
    } catch (error) {
      setSubmitError(
        error instanceof Error
          ? error.message
          : "Couldn't remove Autodeposit. Please try again.",
      );
    } finally {
      setSubmitting(false);
    }
  }, [submitting, onDelete]);

  const thresholdUsd = amountToUsd(amount);
  // Creating without enough SOL for the setup accounts' rent blocks the CTA
  // outright (independent of the typed threshold) — same UX as DepositSheet's
  // first-deposit gate. Otherwise the design's only validation: a $0 threshold
  // can't create an autodeposit.
  const needsSolTopUp = mode === "create" && solShortfallBlocks(setupSolShortfall);
  const disabled = needsSolTopUp || thresholdUsd <= 0;

  const handleConfirm = useCallback(async () => {
    if (disabled || submitting) {
      return;
    }
    void Haptics.notificationAsync(Haptics.NotificationFeedbackType.Success);
    Keyboard.dismiss();
    setSubmitError(null);
    const result = onConfirm(thresholdUsd);
    if (!(result instanceof Promise)) {
      sheetRef.current?.dismiss();
      return;
    }
    setSubmitting(true);
    try {
      await result;
      sheetRef.current?.dismiss();
    } catch (error) {
      setSubmitError(
        error instanceof Error
          ? error.message
          : "Something went wrong. Please try again.",
      );
    } finally {
      setSubmitting(false);
    }
  }, [disabled, submitting, onConfirm, thresholdUsd]);

  const renderBackdrop = useCallback(
    (props: React.ComponentProps<typeof BottomSheetBackdrop>) => (
      <BottomSheetBackdrop
        {...props}
        disappearsOnIndex={-1}
        appearsOnIndex={0}
        opacity={0.2}
      />
    ),
    [],
  );

  const displayValue = formatAmountDisplay(amount);
  const fromParts = splitDollars(balance);
  const toParts = splitDollars(earnBalance);
  const ctaLabel = needsSolTopUp
    ? formatSolTopUpCta(setupSolShortfall as number, "create Autodeposit")
    : mode === "edit"
      ? "Confirm"
      : "Create Autodeposit";

  const amountFontSize = useMemo(
    () => fitUsdAmountFontSize(displayValue),
    [displayValue],
  );

  const keyboard = useAnimatedKeyboard();
  const animatedFooterStyle = useAnimatedStyle(() => ({
    transform: [{ translateY: -keyboard.height.value }],
  }));

  return (
    <BottomSheetModal
      ref={sheetRef}
      snapPoints={snapPoints}
      enableDynamicSizing={false}
      enablePanDownToClose
      backdropComponent={renderBackdrop}
      onDismiss={onClose}
      onChange={handleSheetChange}
      handleComponent={null}
      backgroundStyle={styles.sheetBackground}
      keyboardBehavior="extend"
      keyboardBlurBehavior="restore"
      android_keyboardInputMode="adjustResize"
    >
      <BottomSheetView style={[styles.container, { height: sheetHeight }]}>
        <View style={styles.toolbar}>
          <Pressable
            onPress={handleClose}
            accessibilityRole="button"
            accessibilityLabel="Close"
            style={({ pressed }) => [
              styles.iconButton,
              { opacity: pressed ? 0.7 : 1 },
            ]}
            hitSlop={12}
          >
            <X size={24} color="#1C1C1E" strokeWidth={2} />
          </Pressable>
          <Text style={styles.toolbarTitle}>Autodeposit</Text>
          {mode === "edit" ? (
            <Pressable
              onPress={handleDelete}
              accessibilityRole="button"
              accessibilityLabel="Delete Autodeposit"
              disabled={submitting}
              style={({ pressed }) => [
                styles.iconButton,
                styles.deleteButton,
                { opacity: pressed ? 0.7 : 1 },
              ]}
              hitSlop={12}
            >
              <Trash2 size={24} color={COLOR_RED} strokeWidth={2} />
            </Pressable>
          ) : (
            <View style={styles.iconButtonSpacer} />
          )}
        </View>

        <View style={styles.body}>
          <Text style={styles.inputLabel}>Deposit anything above</Text>
          <Pressable style={[amountFieldStyles.row, { marginTop: 4 }]} onPress={openKeyboard}>
            <View style={amountFieldStyles.visual} pointerEvents="none">
              <Text style={[amountFieldStyles.text, { fontSize: amountFontSize }]}>
                $
              </Text>
              <Text style={[amountFieldStyles.text, { fontSize: amountFontSize }]}>
                {displayValue || "0"}
              </Text>
              <View
                style={[
                  amountFieldStyles.caret,
                  { opacity: isFocused && caretOn ? 1 : 0 },
                ]}
              />
            </View>
            <BottomSheetTextInput
              ref={inputRef as unknown as React.Ref<never>}
              value={displayValue}
              onChangeText={handleAmountChange}
              onFocus={() => setIsFocused(true)}
              onBlur={() => setIsFocused(false)}
              keyboardType="decimal-pad"
              inputMode="decimal"
              maxLength={15}
              caretHidden
              pointerEvents="none"
              style={amountFieldStyles.overlayInput}
              accessibilityLabel="Autodeposit threshold"
            />
          </Pressable>
          <View style={styles.caption}>
            <Info size={20} color={COLOR_RED} strokeWidth={2} />
            <Text style={styles.captionText}>
              Any stablecoins above this amount will automatically go to Earn
            </Text>
          </View>
        </View>

        <Animated.View
          style={[
            styles.footerAbsolute,
            { paddingBottom: insets.bottom + 12 },
            animatedFooterStyle,
          ]}
        >
          <View style={styles.flow}>
            <View style={styles.flowCell}>
              <View style={styles.walletIcon}>
                <Wallet size={24} color="#FFF" strokeWidth={2} />
              </View>
              <View style={styles.flowMiddle}>
                <Text
                  style={styles.flowSubtitle}
                  numberOfLines={1}
                  ellipsizeMode="middle"
                >
                  from {shortenAddress(walletAddress)}
                </Text>
                <Text style={styles.flowAmount}>
                  <Text style={styles.flowAmountWhole}>{fromParts.whole}</Text>
                  <Text style={styles.flowAmountCents}>{fromParts.cents}</Text>
                </Text>
              </View>
            </View>
            <View style={styles.connector} />
            <View style={styles.flowCell}>
              <AutodepositIcon width={48} height={48} />
              <View style={styles.flowMiddle}>
                <Text style={styles.flowSubtitle} numberOfLines={1}>
                  to Earn
                </Text>
                <Text style={styles.flowAmount}>
                  <Text style={styles.flowAmountWhole}>{toParts.whole}</Text>
                  <Text style={styles.flowAmountCents}>{toParts.cents}</Text>
                </Text>
              </View>
            </View>
          </View>

          {submitError ? (
            <Text style={styles.submitError}>{submitError}</Text>
          ) : null}

          {mode === "create" ? (
            <Text style={styles.solHint}>
              Setting up Autodeposit takes ~{formatSolAmountCompact(SETUP_MIN_SOL)}{" "}
              SOL from your wallet for Solana account rent — it is returned
              when you remove Autodeposit.
            </Text>
          ) : null}

          <Pressable
            onPress={handleConfirm}
            accessibilityRole="button"
            accessibilityLabel={ctaLabel}
            disabled={disabled || submitting}
            style={({ pressed }) => [
              styles.cta,
              needsSolTopUp
                ? styles.ctaError
                : disabled
                  ? styles.ctaDisabled
                  : styles.ctaEnabled,
              !disabled && !submitting && pressed && styles.ctaPressed,
            ]}
          >
            {submitting ? (
              <ActivityIndicator color="#FFF" />
            ) : needsSolTopUp ? (
              <Text style={styles.ctaLabelError}>{ctaLabel}</Text>
            ) : (
              <Text
                style={
                  disabled ? styles.ctaLabelDisabled : styles.ctaLabelEnabled
                }
              >
                {ctaLabel}
              </Text>
            )}
          </Pressable>
        </Animated.View>
      </BottomSheetView>
    </BottomSheetModal>
  );
}

const styles = StyleSheet.create({
  sheetBackground: {
    backgroundColor: "#FFF",
    borderTopLeftRadius: 38,
    borderTopRightRadius: 38,
  },
  container: {
    backgroundColor: "#FFF",
    borderTopLeftRadius: 38,
    borderTopRightRadius: 38,
    overflow: "hidden",
  },
  toolbar: {
    flexDirection: "row",
    alignItems: "center",
    justifyContent: "space-between",
    paddingHorizontal: 16,
    paddingVertical: 16,
  },
  iconButton: {
    width: 44,
    height: 44,
    borderRadius: 22,
    backgroundColor: COLOR_CHIP_BG,
    alignItems: "center",
    justifyContent: "center",
  },
  deleteButton: {
    backgroundColor: "rgba(249, 54, 60, 0.14)",
  },
  iconButtonSpacer: {
    width: 44,
    height: 44,
  },
  toolbarTitle: {
    fontFamily: "Geist_600SemiBold",
    fontSize: 17,
    lineHeight: 22,
    color: COLOR_BLACK,
    textAlign: "center",
  },
  body: {
    paddingHorizontal: 16,
    paddingTop: 10,
  },
  inputLabel: {
    fontFamily: "Geist_400Regular",
    fontSize: 15,
    lineHeight: 20,
    color: COLOR_LABEL_DIM,
  },
  caption: {
    flexDirection: "row",
    alignItems: "center",
    gap: 8,
    paddingTop: 8,
  },
  captionText: {
    flex: 1,
    fontFamily: "Geist_400Regular",
    fontSize: 15,
    lineHeight: 20,
    color: COLOR_RED,
  },
  submitError: {
    fontFamily: "Geist_400Regular",
    fontSize: 13,
    lineHeight: 18,
    color: COLOR_RED,
    textAlign: "center",
    marginBottom: 8,
  },
  solHint: {
    fontFamily: "Geist_400Regular",
    fontSize: 14,
    lineHeight: 20,
    color: COLOR_LABEL_DIM,
    textAlign: "center",
    marginBottom: 12,
  },
  footerAbsolute: {
    position: "absolute",
    left: 0,
    right: 0,
    bottom: 0,
    backgroundColor: "#FFF",
    paddingHorizontal: 20,
    paddingTop: 16,
  },
  flow: {
    flexDirection: "row",
    alignItems: "center",
    gap: 8,
    marginBottom: 16,
  },
  flowCell: {
    flex: 1,
    flexDirection: "row",
    alignItems: "center",
    gap: 12,
  },
  walletIcon: {
    width: 48,
    height: 48,
    borderRadius: 12,
    backgroundColor: COLOR_WALLET_GREEN,
    alignItems: "center",
    justifyContent: "center",
  },
  flowMiddle: {
    flex: 1,
    paddingVertical: 9,
  },
  flowSubtitle: {
    fontFamily: "Geist_400Regular",
    fontSize: 15,
    lineHeight: 20,
    color: COLOR_LABEL_DIM,
  },
  flowAmount: {
    fontFamily: "Geist_600SemiBold",
    fontSize: 20,
    lineHeight: 24,
  },
  flowAmountWhole: {
    fontFamily: "Geist_600SemiBold",
    fontSize: 20,
    lineHeight: 24,
    color: COLOR_BLACK,
  },
  flowAmountCents: {
    fontFamily: "Geist_600SemiBold",
    fontSize: 20,
    lineHeight: 24,
    color: COLOR_CENTS_DIM,
  },
  connector: {
    width: 14,
    height: 2,
    borderRadius: 2,
    backgroundColor: COLOR_CONNECTOR,
  },
  cta: {
    height: 50,
    borderRadius: 78,
    alignItems: "center",
    justifyContent: "center",
  },
  ctaEnabled: {
    backgroundColor: COLOR_BLACK,
  },
  ctaDisabled: {
    backgroundColor: COLOR_CHIP_BG,
  },
  ctaError: {
    backgroundColor: "rgba(249, 54, 60, 0.14)",
  },
  ctaPressed: {
    opacity: 0.85,
  },
  ctaLabelEnabled: {
    fontFamily: "Geist_500Medium",
    fontSize: 16,
    lineHeight: 20,
    color: "#FFF",
  },
  ctaLabelDisabled: {
    fontFamily: "Geist_500Medium",
    fontSize: 16,
    lineHeight: 20,
    color: "rgba(60, 60, 67, 0.3)",
  },
  ctaLabelError: {
    fontFamily: "Geist_500Medium",
    fontSize: 16,
    lineHeight: 20,
    color: COLOR_RED,
    textAlign: "center",
  },
});
