import { EarnMaxTransferDestination } from "@/features/earn-max/EarnMaxTransferDestination";
import DepositClose from "../../../assets/images/earn-max/deposit-close.svg";
import DepositInfo from "../../../assets/images/earn-max/info.svg";
import DepositSelect from "../../../assets/images/earn-max/select.svg";
import {
  BottomSheetBackdrop,
  BottomSheetModal,
  BottomSheetTextInput,
  BottomSheetView,
} from "@gorhom/bottom-sheet";
import * as Haptics from "expo-haptics";
import { Check, ChevronDown, X } from "lucide-react-native";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  ActivityIndicator,
  Image,
  type ImageSourcePropType,
  Keyboard,
  Platform,
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
  usdBalanceFormatter,
  useCaretBlink,
} from "@/components/sheets/sheetFacts";
import { useFixedSheetLayout } from "@/hooks/useFixedSheetLayout";
import { useKeyboardRescueFocus } from "@/hooks/useKeyboardRescueFocus";

import { env } from "@/config/env";
import { getEarnProductAssets } from "@/lib/solana/earn/earn-product-mints";
import {
  DEFAULT_TOKEN_ICON,
  KNOWN_TOKEN_ICONS,
} from "@/lib/solana/token-holdings/constants";

const usdcLogo = require("../../../assets/images/earn/usdc.png");

// Which stablecoin funds the deposit — one row per Earn product asset
// (CASH/USDG/PYUSD/USDC/USDT/USDS on mainnet). Mirrors the web deposit
// pane's DepositSourceOption; the picker chrome is the withdraw sheet's
// source selector.
type DepositSourceOption = {
  symbol: string;
  mint: string;
  logo: ImageSourcePropType;
  usd: number;
};

const MIN_DEPOSIT_USD = 1;

// The first Earn deposit creates the position's on-chain accounts, which costs
// SOL (rent + fees, refunded when the position closes). Measured on mainnet
// for a virgin wallet (2026-07-08): policy stages + fees ≈ 0.0178 SOL, then
// the deposit stage transfers exactly 39,532,800 lamports (≈0.0395 SOL) of
// Kamino obligation setup rent — ≈0.0574 SOL total, so 0.05 was not enough.
const FIRST_DEPOSIT_MIN_SOL = 0.06;

export function computeFirstDepositSolShortfall(
  solBalance: number | null
): number | null {
  return computeSolShortfall(FIRST_DEPOSIT_MIN_SOL, solBalance);
}

const COLOR_LABEL_DIM = "rgba(60, 60, 67, 0.6)";
const COLOR_CHIP_BG = "#F2F2F7";
const COLOR_CHIP_SOFT = "rgba(0, 0, 0, 0.04)";
const COLOR_BLACK = "#000";
const COLOR_ERROR_BG = "rgba(249, 54, 60, 0.14)";
const COLOR_ERROR_TEXT = "#F9363C";

type DepositSheetProps = {
  open: boolean;
  onClose: () => void;
  onDeposit?: (amountUsd: number, mint: string) => void | Promise<void>;
  // The wallet's spendable balance per Earn product mint (token units ≈
  // dollars). `null`/missing entries render as $0.00 while holdings load.
  stableBalancesByMint?: Record<string, number> | null;
  firstDepositSolShortfall?: number | null;
  // True when the next deposit runs first-time setup — no position yet, or
  // the route-policy pair is missing (torn down by a full exit) — shows the
  // always-visible note that it takes ~FIRST_DEPOSIT_MIN_SOL of SOL and that
  // it comes back on full withdrawal.
  isFirstDeposit?: boolean;
  earnMax?: boolean;
  earnMaxApyBps?: number | null;
};

export function DepositSheet({
  open,
  onClose,
  onDeposit,
  stableBalancesByMint,
  firstDepositSolShortfall,
  isFirstDeposit,
  earnMax = false,
  earnMaxApyBps,
}: DepositSheetProps) {
  const sheetRef = useRef<BottomSheetModal>(null);
  const sourceSheetRef = useRef<BottomSheetModal>(null);
  const inputRef = useRef<{ focus: () => void; blur?: () => void } | null>(
    null
  );
  const { openKeyboard } = useKeyboardRescueFocus(inputRef);
  const insets = useSafeAreaInsets();
  const [amount, setAmount] = useState("");
  const [selectedSymbol, setSelectedSymbol] = useState("USDC");
  const [isFocused, setIsFocused] = useState(false);
  const caretOn = useCaretBlink(isFocused);
  const [submitting, setSubmitting] = useState(false);
  const [submitError, setSubmitError] = useState<string | null>(null);
  const [slowSetupHint, setSlowSetupHint] = useState(false);

  useEffect(() => {
    if (!submitting || !isFirstDeposit) {
      setSlowSetupHint(false);
      return;
    }
    const timer = setTimeout(() => setSlowSetupHint(true), 8_000);
    return () => clearTimeout(timer);
  }, [submitting, isFirstDeposit]);

  const { sheetHeight, snapPoints } = useFixedSheetLayout(
    earnMax ? 1 : 0.94,
    earnMax ? 1 : 12
  );
  // One row per Earn product asset, funded coins first (the sheet opens
  // downward, so funded rows sit nearest the trigger); product order within
  // each group. Empty rows stay visible but disabled so the coin set reads
  // complete — mirrors the web deposit selector.
  const sourceOptions = useMemo<DepositSourceOption[]>(() => {
    const options = getEarnProductAssets(earnMax ? "mainnet" : env.solanaEnv)
      .filter((asset) => !earnMax || asset.symbol === "USDC")
      .map((asset) => {
        const balance = stableBalancesByMint?.[asset.mint];
        return {
          symbol: asset.symbol,
          mint: asset.mint,
          logo:
            asset.symbol === "USDC"
              ? usdcLogo
              : { uri: KNOWN_TOKEN_ICONS[asset.mint] ?? DEFAULT_TOKEN_ICON },
          usd: Number.isFinite(balance ?? NaN) ? (balance as number) : 0,
        };
      });
    return options.sort((a, b) => (b.usd > 0 ? 1 : 0) - (a.usd > 0 ? 1 : 0));
  }, [stableBalancesByMint, earnMax]);
  const selectedSource =
    sourceOptions.find((option) => option.symbol === selectedSymbol) ??
    sourceOptions[0];
  const available = selectedSource.usd;

  const prevOpenRef = useRef<boolean | null>(null);
  useEffect(() => {
    const wasOpen = prevOpenRef.current;
    prevOpenRef.current = open;
    if (wasOpen === open) return;
    presentSheetWhenOpen(sheetRef.current, open);
    if (open) {
      setAmount("");
      setSubmitError(null);
      setSubmitting(false);
      const usdc = sourceOptions.find((option) => option.symbol === "USDC");
      const richest = [...sourceOptions].sort((a, b) => b.usd - a.usd)[0];
      setSelectedSymbol(
        usdc && usdc.usd > 0
          ? "USDC"
          : richest && richest.usd > 0
          ? richest.symbol
          : "USDC"
      );
    }
  }, [open, sourceOptions]);

  const handleSheetChange = useCallback((index: number) => {
    focusInputAfterSheetSettles(() => inputRef.current?.focus(), index);
  }, []);

  const handleAmountChange = useCallback((text: string) => {
    setAmount(sanitizeAmount(text));
  }, []);

  const handleFocus = useCallback(() => setIsFocused(true), []);
  const handleBlur = useCallback(() => setIsFocused(false), []);

  const handleClose = useCallback(() => {
    Keyboard.dismiss();
    sheetRef.current?.dismiss();
  }, []);

  const handleMax = useCallback(() => {
    void Haptics.selectionAsync();
    setAmount((Math.floor(available * 100) / 100).toFixed(2));
  }, [available]);

  const openSourceList = useCallback(() => {
    void Haptics.selectionAsync();
    Keyboard.dismiss();
    sourceSheetRef.current?.present();
  }, []);

  const selectSource = useCallback((symbol: string) => {
    void Haptics.selectionAsync();
    setSelectedSymbol(symbol);
    setAmount("");
    sourceSheetRef.current?.dismiss();
  }, []);

  const handleDeposit = useCallback(async () => {
    const usd = amountToUsd(amount);
    if (usd < MIN_DEPOSIT_USD || usd > available) {
      return;
    }
    void Haptics.notificationAsync(Haptics.NotificationFeedbackType.Success);
    Keyboard.dismiss();
    setSubmitError(null);
    const result = onDeposit?.(usd, selectedSource.mint);
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
          : "Deposit failed. Please try again."
      );
    } finally {
      setSubmitting(false);
    }
  }, [amount, available, onDeposit, selectedSource.mint]);

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

  const displayValue = formatAmountDisplay(amount);
  const enteredUsd = amountToUsd(amount);
  const needsSolTopUp = solShortfallBlocks(firstDepositSolShortfall);
  const belowMinimum = enteredUsd < MIN_DEPOSIT_USD;
  const insufficientFunds = !belowMinimum && enteredUsd > available;
  const hasError = needsSolTopUp || belowMinimum || insufficientFunds;
  const ctaLabel = needsSolTopUp
    ? formatSolTopUpCta(firstDepositSolShortfall as number, "open deposit")
    : belowMinimum
    ? `Minimum deposit is $${MIN_DEPOSIT_USD}`
    : insufficientFunds
    ? "Insufficient balance"
    : "Deposit";

  const amountFontSize = useMemo(
    () => fitUsdAmountFontSize(displayValue),
    [displayValue]
  );

  // gorhom's BottomSheetFooter zeroes out keyboard height on Android with
  // `adjustResize` (BottomSheet.js:1168), assuming the portal container will
  // shrink — it often doesn't. Drive the footer position with reanimated's
  // `useAnimatedKeyboard` instead, which reads the actual keyboard height
  // from the system regardless of input mode. The footer sits absolute at
  // the bottom of the sheet, then translates up by the keyboard height.
  const keyboard = useAnimatedKeyboard();
  const animatedFooterStyle = useAnimatedStyle(() => ({
    transform: [{ translateY: -keyboard.height.value }],
    ...(earnMax
      ? {
          paddingBottom:
            keyboard.height.value > 0 ? 24 : Math.max(insets.bottom + 12, 24),
        }
      : {}),
  }));

  return (
    <>
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
              {earnMax ? (
                <DepositClose />
              ) : (
                <X size={24} color="#1C1C1E" strokeWidth={2} />
              )}
            </Pressable>
            <Text style={styles.toolbarTitle}>Deposit</Text>
            <View style={styles.iconButtonSpacer} />
          </View>

          <View style={styles.body}>
            <View style={styles.amountInputWrap}>
              <Pressable
                style={[amountFieldStyles.row, earnMax && styles.maxAmountRow]}
                onPress={openKeyboard}
              >
                <View style={amountFieldStyles.visual} pointerEvents="none">
                  <Text
                    style={[
                      amountFieldStyles.text,
                      earnMax && styles.maxAmountText,
                      { fontSize: amountFontSize },
                    ]}
                  >
                    $
                  </Text>
                  <Text
                    style={[
                      amountFieldStyles.text,
                      earnMax && styles.maxAmountText,
                      { fontSize: amountFontSize },
                    ]}
                  >
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
                  onFocus={handleFocus}
                  onBlur={handleBlur}
                  keyboardType="decimal-pad"
                  inputMode="decimal"
                  maxLength={15}
                  caretHidden
                  pointerEvents="none"
                  style={amountFieldStyles.overlayInput}
                  accessibilityLabel="Deposit amount"
                />
              </Pressable>
              {earnMax && isFirstDeposit ? (
                <View style={styles.maxRentCaption}>
                  <DepositInfo />
                  <Text style={styles.maxRentText}>
                    Your first deposit creates Solana accounts and requires SOL
                    in your wallet for account rent and transaction fees.
                  </Text>
                </View>
              ) : null}
            </View>
          </View>

          <Animated.View
            style={[
              styles.footerAbsolute,
              { paddingBottom: insets.bottom + 12 },
              earnMax && styles.maxFooter,
              animatedFooterStyle,
            ]}
          >
            <View style={earnMax && styles.maxTransferRows}>
              <View
                style={[styles.balanceCell, earnMax && styles.maxBalanceCell]}
              >
                <Pressable
                  onPress={openSourceList}
                  accessibilityRole="button"
                  accessibilityLabel="Choose deposit stablecoin"
                  style={({ pressed }) => [
                    styles.selectorPill,
                    earnMax && styles.maxSourceIcon,
                    { opacity: pressed ? 0.8 : 1 },
                  ]}
                  hitSlop={8}
                >
                  <Image
                    source={earnMax ? usdcLogo : selectedSource.logo}
                    style={[styles.tokenLogo, earnMax && styles.maxTokenLogo]}
                    accessibilityLabel={selectedSource.symbol}
                  />
                  {!earnMax && (
                    <ChevronDown
                      size={16}
                      color={COLOR_LABEL_DIM}
                      strokeWidth={2}
                    />
                  )}
                </Pressable>
                <View style={styles.balanceMiddle}>
                  {earnMax && (
                    <Text style={styles.balanceAvailable}>
                      from USDC balance
                    </Text>
                  )}
                  <Text style={styles.balanceAmount}>
                    {earnMax ? (
                      <>
                        {usdBalanceFormatter.format(available).split(".")[0]}
                        <Text style={styles.maxCents}>
                          .{usdBalanceFormatter.format(available).split(".")[1]}
                        </Text>
                      </>
                    ) : (
                      usdBalanceFormatter.format(available)
                    )}
                  </Text>
                  {!earnMax && (
                    <Text style={styles.balanceAvailable} numberOfLines={1}>
                      {`${selectedSource.symbol} available`}
                    </Text>
                  )}
                </View>
                <Pressable
                  onPress={handleMax}
                  accessibilityRole="button"
                  accessibilityLabel="Use maximum balance"
                  style={({ pressed }) => [
                    styles.maxBadge,
                    { opacity: pressed ? 0.8 : 1 },
                  ]}
                  hitSlop={8}
                >
                  <Text style={styles.maxBadgeText}>MAX</Text>
                </Pressable>
                {earnMax && (
                  <Pressable
                    onPress={openSourceList}
                    accessibilityRole="button"
                    accessibilityLabel="Choose deposit stablecoin"
                    hitSlop={8}
                  >
                    <DepositSelect />
                  </Pressable>
                )}
              </View>

              {earnMax ? (
                <EarnMaxTransferDestination apyBps={earnMaxApyBps} />
              ) : null}
              {earnMax && <View style={styles.maxConnector} />}
            </View>
            {submitError ? (
              <Text style={styles.submitError}>{submitError}</Text>
            ) : null}

            {slowSetupHint ? (
              <Text style={styles.solHint}>
                Setting up your Earn account — a first deposit can take up to a
                minute…
              </Text>
            ) : null}

            {isFirstDeposit && !earnMax ? (
              <Text style={styles.solHint}>
                This deposit sets up your Earn account and takes ~
                {formatSolAmountCompact(FIRST_DEPOSIT_MIN_SOL)} SOL from your wallet
                for Solana account rent — it is returned when you fully
                withdraw.
              </Text>
            ) : null}

            <Pressable
              onPress={handleDeposit}
              accessibilityRole="button"
              accessibilityLabel={ctaLabel}
              disabled={hasError || submitting}
              style={({ pressed }) => [
                styles.cta,
                earnMax && styles.maxCta,
                hasError ? styles.ctaError : styles.ctaEnabled,
                !hasError && !submitting && pressed && styles.ctaPressed,
              ]}
            >
              {submitting ? (
                <ActivityIndicator color="#FFF" />
              ) : (
                <Text
                  style={
                    hasError ? styles.ctaLabelError : styles.ctaLabelEnabled
                  }
                >
                  {ctaLabel}
                </Text>
              )}
            </Pressable>
          </Animated.View>
        </BottomSheetView>
      </BottomSheetModal>

      <BottomSheetModal
        ref={sourceSheetRef}
        enableDynamicSizing
        enablePanDownToClose
        backdropComponent={renderBackdrop}
        backgroundStyle={styles.sheetBackground}
        handleIndicatorStyle={styles.sourceHandle}
      >
        <BottomSheetView
          style={{
            paddingHorizontal: 12,
            paddingBottom: insets.bottom + 12,
            paddingTop: 4,
          }}
        >
          <Text style={styles.sourceListTitle}>Stablecoins</Text>
          {sourceOptions.map((option) => {
            const isEmpty = option.usd <= 0;
            return (
              <Pressable
                key={option.symbol}
                onPress={() => selectSource(option.symbol)}
                disabled={isEmpty}
                accessibilityRole="button"
                accessibilityLabel={option.symbol}
                accessibilityState={{ disabled: isEmpty }}
                style={({ pressed }) => [
                  styles.sourceRow,
                  { backgroundColor: pressed ? COLOR_CHIP_BG : "transparent" },
                  isEmpty && styles.sourceRowDisabled,
                ]}
              >
                <Image
                  source={option.logo}
                  style={styles.sourceRowLogo}
                  accessibilityLabel={option.symbol}
                />
                <View style={styles.sourceRowMiddle}>
                  <Text style={styles.sourceRowLabel}>{option.symbol}</Text>
                  <Text style={styles.sourceRowBalance}>
                    {usdBalanceFormatter.format(option.usd)}
                  </Text>
                </View>
                {option.symbol === selectedSource.symbol ? (
                  <Check size={22} color={COLOR_BLACK} strokeWidth={2} />
                ) : null}
              </Pressable>
            );
          })}
        </BottomSheetView>
      </BottomSheetModal>
    </>
  );
}

const styles = StyleSheet.create({
  maxFooter: { paddingHorizontal: 0 },
  maxAmountRow: { height: Platform.OS === "ios" ? 58 : 48 },
  maxAmountText: { lineHeight: Platform.OS === "ios" ? 58 : 48 },
  maxTransferRows: { gap: 8 },
  maxBalanceCell: { marginHorizontal: 16, marginBottom: 0, height: 60 },
  maxSourceIcon: {
    padding: 0,
    paddingLeft: 0,
    paddingRight: 0,
    paddingVertical: 0,
    backgroundColor: "transparent",
  },
  maxTokenLogo: { width: 48, height: 48, borderRadius: 24 },
  maxCents: { color: "rgba(60,60,67,0.4)" },
  maxConnector: {
    position: "absolute",
    width: 2,
    height: 17,
    left: 39,
    top: 57,
    borderRadius: 2,
    backgroundColor: "rgba(0,0,0,0.14)",
  },
  maxCta: { marginHorizontal: 20, marginTop: 16 },
  maxRentCaption: {
    flexDirection: "row",
    gap: 8,
    paddingVertical: 8,
    marginTop: 4,
  },
  maxRentText: {
    flex: 1,
    fontFamily: "Geist_400Regular",
    fontSize: 15,
    lineHeight: 20,
    color: "#8A8A8E",
  },
  sheetBackground: {
    backgroundColor: "#FFF",
    borderTopLeftRadius: 38,
    borderTopRightRadius: 38,
  },
  container: {
    // Definite height matching the modal's snap point. Without an explicit
    // height, BottomSheetView sizes to content and absolute `bottom: 0`
    // children land at the content's bottom, not the sheet's bottom.
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
    paddingTop: 36,
  },
  amountInputWrap: {
    paddingBottom: 24,
  },
  balanceCell: {
    flexDirection: "row",
    alignItems: "center",
    gap: 12,
    marginBottom: 16,
  },
  selectorPill: {
    flexDirection: "row",
    alignItems: "center",
    gap: 4,
    paddingLeft: 4,
    paddingRight: 8,
    paddingVertical: 4,
    borderRadius: 61,
    backgroundColor: COLOR_CHIP_SOFT,
  },
  tokenLogo: {
    width: 40,
    height: 40,
    borderRadius: 20,
  },
  balanceMiddle: {
    flex: 1,
    paddingVertical: 8,
  },
  balanceAmount: {
    fontFamily: "Geist_600SemiBold",
    fontSize: 20,
    lineHeight: 24,
    color: COLOR_BLACK,
  },
  balanceAvailable: {
    fontFamily: "Geist_400Regular",
    fontSize: 15,
    lineHeight: 20,
    color: COLOR_LABEL_DIM,
  },
  maxBadge: {
    minWidth: 64,
    backgroundColor: COLOR_CHIP_SOFT,
    borderRadius: 40,
    paddingHorizontal: 16,
    paddingVertical: 8,
    alignItems: "center",
    justifyContent: "center",
  },
  maxBadgeText: {
    fontFamily: "Geist_500Medium",
    fontSize: 15,
    lineHeight: 20,
    color: COLOR_BLACK,
    textAlign: "center",
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
  cta: {
    height: 50,
    borderRadius: 9999,
    alignItems: "center",
    justifyContent: "center",
  },
  ctaEnabled: {
    backgroundColor: COLOR_BLACK,
  },
  ctaError: {
    backgroundColor: COLOR_ERROR_BG,
  },
  ctaPressed: {
    opacity: 0.85,
  },
  ctaLabelEnabled: {
    fontFamily: "Geist_400Regular",
    fontSize: 17,
    lineHeight: 22,
    color: "#FFF",
  },
  ctaLabelError: {
    fontFamily: "Geist_500Medium",
    fontSize: 16,
    lineHeight: 20,
    color: COLOR_ERROR_TEXT,
    textAlign: "center",
  },
  submitError: {
    fontFamily: "Geist_400Regular",
    fontSize: 14,
    lineHeight: 20,
    color: COLOR_ERROR_TEXT,
    textAlign: "center",
    marginBottom: 12,
  },
  solHint: {
    fontFamily: "Geist_400Regular",
    fontSize: 14,
    lineHeight: 20,
    color: COLOR_LABEL_DIM,
    textAlign: "center",
    marginBottom: 12,
  },
  sourceHandle: {
    backgroundColor: "rgba(60, 60, 67, 0.3)",
    width: 40,
  },
  sourceListTitle: {
    fontFamily: "Geist_600SemiBold",
    fontSize: 17,
    lineHeight: 22,
    color: COLOR_BLACK,
    paddingHorizontal: 8,
    paddingTop: 8,
    paddingBottom: 4,
  },
  sourceRow: {
    flexDirection: "row",
    alignItems: "center",
    gap: 12,
    paddingHorizontal: 8,
    paddingVertical: 10,
    borderRadius: 16,
  },
  sourceRowDisabled: {
    opacity: 0.4,
  },
  sourceRowLogo: {
    width: 32,
    height: 32,
    borderRadius: 16,
  },
  sourceRowMiddle: {
    flex: 1,
  },
  sourceRowLabel: {
    fontFamily: "Geist_500Medium",
    fontSize: 16,
    lineHeight: 20,
    color: COLOR_BLACK,
  },
  sourceRowBalance: {
    fontFamily: "Geist_400Regular",
    fontSize: 14,
    lineHeight: 18,
    color: COLOR_LABEL_DIM,
    marginTop: 2,
  },
});
