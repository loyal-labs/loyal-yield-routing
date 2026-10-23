import { BottomSheetBackdrop } from "@gorhom/bottom-sheet";
import { PublicKey } from "@solana/web3.js";
import { useEffect, useState } from "react";
import type { ReactNode } from "react";
import { Dimensions, StyleSheet } from "react-native";
import Animated, {
  cancelAnimation,
  Easing,
  useAnimatedStyle,
  useSharedValue,
  withRepeat,
  withSpring,
  withTiming,
} from "react-native-reanimated";

import { getSolanaEnv } from "@/lib/solana/rpc/connection";

import SendSpinnerIcon from "../../../assets/images/wallet/send_spinner_80.svg";

export const BASE58_ADDRESS_PATTERN = /^[1-9A-HJ-NP-Za-km-z]{32,44}$/;

export function isValidSolanaAddress(address: string): boolean {
  if (!BASE58_ADDRESS_PATTERN.test(address)) return false;
  try {
    new PublicKey(address);
    return true;
  } catch {
    return false;
  }
}

// SOL amounts need more precision than tokens — the network fee is 0.000005
// SOL, so the review "Total amount" must keep up to 6 decimals to surface it.
export function formatSolAmount(value: number): string {
  return formatWithCommas(value, 0, 6);
}

export function formatWithCommas(
  value: number,
  minFrac: number,
  maxFrac: number
): string {
  const safe = Number.isFinite(value) ? value : 0;
  const [intPart, fracPartRaw = ""] = safe.toFixed(maxFrac).split(".");
  let fracPart = fracPartRaw;
  while (fracPart.length > minFrac && fracPart.endsWith("0")) {
    fracPart = fracPart.slice(0, -1);
  }
  const withCommas = intPart.replace(/\B(?=(\d{3})+(?!\d))/g, ",");
  return fracPart ? `${withCommas}.${fracPart}` : withCommas;
}

export function formatTokenAmount(value: number, minFrac = 2): string {
  return formatWithCommas(value, minFrac, 4);
}

export function formatUsdAmount(value: number): string {
  return `$${formatWithCommas(value, 2, 2)}`;
}

export function sanitizeAmount(input: string): string {
  let cleaned = input.replace(/[^0-9.]/g, "");
  const parts = cleaned.split(".");
  if (parts.length > 2) {
    cleaned = `${parts[0]}.${parts.slice(1).join("")}`;
  }
  const [intPart, decPart] = cleaned.split(".");
  const normalizedInt = intPart ? intPart.replace(/^0+(?=\d)/, "") : intPart;
  const trimmedDec = decPart !== undefined ? decPart.slice(0, 2) : undefined;
  return trimmedDec !== undefined
    ? `${normalizedInt || "0"}.${trimmedDec}`
    : normalizedInt;
}

// Strip an amount field to clean numeric text (no commas, single dot). Unlike
// sanitizeAmount we don't cap decimals — token amounts need more precision.
export function stripAmountInput(input: string): string {
  let cleaned = input.replace(/[^0-9.]/g, "");
  const parts = cleaned.split(".");
  if (parts.length > 2) {
    cleaned = `${parts[0]}.${parts.slice(1).join("")}`;
  }
  const [intPart, decPart] = cleaned.split(".");
  const normalizedInt = intPart ? intPart.replace(/^0+(?=\d)/, "") : intPart;
  return decPart !== undefined
    ? `${normalizedInt || "0"}.${decPart}`
    : normalizedInt;
}

export function formatAmountDisplay(raw: string): string {
  if (!raw) return "";
  const trailingDot = raw.endsWith(".") && !raw.slice(0, -1).includes(".");
  const [intPart, decPart] = raw.split(".");
  const intNum = Number(intPart || "0");
  const intText = Number.isFinite(intNum)
    ? intNum.toLocaleString("en-US")
    : intPart || "0";
  if (trailingDot) return `${intText}.`;
  return decPart !== undefined ? `${intText}.${decPart}` : intText;
}

// Thousands-separated display for the amount field, preserving a trailing dot
// while the user is mid-typing.
export function formatAmountInputDisplay(raw: string): string {
  if (!raw) return "";
  const trailingDot = raw.endsWith(".") && !raw.slice(0, -1).includes(".");
  const [intPart, decPart] = raw.split(".");
  const intText = formatWithCommas(Number(intPart || "0"), 0, 0);
  if (trailingDot) return `${intText}.`;
  return decPart !== undefined ? `${intText}.${decPart}` : intText;
}

export function amountToUsd(raw: string): number {
  if (!raw) return 0;
  const parsed = Number(raw);
  return Number.isFinite(parsed) ? parsed : 0;
}

export const usdBalanceFormatter = new Intl.NumberFormat("en-US", {
  style: "currency",
  currency: "USD",
  minimumFractionDigits: 2,
  maximumFractionDigits: 2,
});

// How much SOL the wallet is missing to cover the minimum. Returns null when
// covered — or when the balance hasn't loaded yet, so the gate fails open
// instead of blocking a funded wallet on slow data.
export function computeSolShortfall(
  minSol: number,
  solBalance: number | null
): number | null {
  if (solBalance == null || !Number.isFinite(solBalance)) {
    return null;
  }
  const shortfall = minSol - solBalance;
  if (shortfall <= 0) return null;
  // Ceil at 4 decimals so the ">" prompt stays true after rounding.
  return Math.ceil(shortfall * 1e4) / 1e4;
}

export function solShortfallBlocks(
  shortfall: number | null | undefined
): boolean {
  return (shortfall ?? 0) > 0;
}

export function formatSolAmountCompact(sol: number): string {
  return sol.toFixed(4).replace(/\.?0+$/, "");
}

export function formatSolTopUpCta(shortfall: number, purpose: string): string {
  return `Deposit > ${formatSolAmountCompact(shortfall)} SOL to ${purpose}`;
}

export const SCREEN_WIDTH = Dimensions.get("screen").width;

export const AMOUNT_MAX_FONT_SIZE = 48;
export const AMOUNT_MIN_FONT_SIZE = 22;
export const AMOUNT_CHAR_WIDTH_RATIO = 0.6;

export function fitAmountFontSize(
  text: string,
  availableWidth: number
): number {
  const fitted = Math.floor(
    availableWidth / (text.length * AMOUNT_CHAR_WIDTH_RATIO)
  );
  return Math.max(AMOUNT_MIN_FONT_SIZE, Math.min(AMOUNT_MAX_FONT_SIZE, fitted));
}

export function fitUsdAmountFontSize(displayValue: string): number {
  return fitAmountFontSize(`$${displayValue || "0"}`, SCREEN_WIDTH - 48);
}

export function truncateAmountTo6Decimals(value: number): number {
  return Math.floor(value * 1e6) / 1e6;
}

export const amountFieldStyles = StyleSheet.create({
  row: {
    position: "relative",
    // Matches the amount text's 58pt line box (see text).
    height: 58,
    justifyContent: "flex-end",
  },
  visual: {
    flexDirection: "row",
    alignItems: "flex-end",
  },
  text: {
    fontFamily: "Geist_600SemiBold",
    fontSize: 48,
    // iOS clips Geist's ascenders when lineHeight == fontSize (Android
    // forgave it via includeFontPadding). Bottom alignment keeps the
    // baseline and caret in place; the headroom goes to the top.
    lineHeight: 58,
    color: "#000",
    includeFontPadding: false,
  },
  caret: {
    width: 2,
    height: 40,
    marginHorizontal: 2,
    // Centered like the glyphs: both platforms center text in the 58pt line
    // box, so a bottom-pinned caret rendered below the digits on iOS.
    alignSelf: "center",
    borderRadius: 1,
    backgroundColor: "#000",
  },
  overlayInput: {
    position: "absolute",
    top: 0,
    left: 0,
    right: 0,
    bottom: 0,
    // `opacity: 0` (not `color: "transparent"`) — on Android the latter
    // doesn't reliably hide BottomSheetTextInput's text rendering, which
    // double-draws the digit on top of the visible <Text>. Opacity 0 hides
    // the whole input while still receiving focus/keyboard.
    opacity: 0,
    padding: 0,
    fontSize: 48,
  },
});

export function useCaretBlink(isFocused: boolean): boolean {
  const [caretOn, setCaretOn] = useState(true);
  useEffect(() => {
    if (!isFocused) {
      setCaretOn(true);
      return;
    }
    const id = setInterval(() => setCaretOn((v) => !v), 530);
    return () => clearInterval(id);
  }, [isFocused]);
  return caretOn;
}

export const SHEET_SETTLE_FOCUS_DELAY_MS = 120;

export function focusInputAfterSheetSettles(
  focus: () => void,
  index: number
): void {
  if (index < 0) return;
  // Focus once the sheet is at its snap point — earlier focus collides with
  // the present animation and the keyboard ends up above the sheet.
  setTimeout(focus, SHEET_SETTLE_FOCUS_DELAY_MS);
}

export function useSheetSettledRefocus(
  sheetSettledRef: { readonly current: boolean },
  focus: () => void
): void {
  useEffect(() => {
    if (!sheetSettledRef.current) return;
    const id = setTimeout(focus, 80);
    return () => clearTimeout(id);
  }, [sheetSettledRef, focus]);
}

export function presentSheetWhenOpen(
  sheet: { present: () => void; dismiss: () => void } | null,
  open: boolean
): void {
  if (open) {
    sheet?.present();
  } else {
    sheet?.dismiss();
  }
}

export function SheetBackdrop(
  props: React.ComponentProps<typeof BottomSheetBackdrop>
) {
  return (
    <BottomSheetBackdrop
      {...props}
      disappearsOnIndex={-1}
      appearsOnIndex={0}
      opacity={0.3}
    />
  );
}

export function solscanExplorerUrl(txSignature: string): string {
  return `https://solscan.io/tx/${txSignature}${
    getSolanaEnv() === "mainnet" ? "" : `?cluster=${getSolanaEnv()}`
  }`;
}

export function SendingSpinner() {
  const rotation = useSharedValue(0);
  useEffect(() => {
    rotation.value = withRepeat(
      withTiming(360, { duration: 900, easing: Easing.linear }),
      -1,
      false
    );
    return () => cancelAnimation(rotation);
  }, [rotation]);
  const style = useAnimatedStyle(() => ({
    transform: [{ rotate: `${rotation.value}deg` }],
  }));
  return (
    <Animated.View style={[{ width: 80, height: 80 }, style]}>
      <SendSpinnerIcon width={80} height={80} />
    </Animated.View>
  );
}

export function MascotReveal({ children }: { children: ReactNode }) {
  const scale = useSharedValue(0.3);
  const opacity = useSharedValue(0);
  useEffect(() => {
    opacity.value = withTiming(1, {
      duration: 160,
      easing: Easing.out(Easing.quad),
    });
    scale.value = withSpring(1, { damping: 8, stiffness: 220, mass: 0.6 });
    return () => {
      cancelAnimation(scale);
      cancelAnimation(opacity);
    };
  }, [scale, opacity]);
  const style = useAnimatedStyle(() => ({
    opacity: opacity.value,
    transform: [{ scale: scale.value }],
  }));
  return <Animated.View style={style}>{children}</Animated.View>;
}
