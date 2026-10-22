import {
  BottomSheetModal,
  BottomSheetScrollView,
  BottomSheetTextInput,
  BottomSheetView,
} from "@gorhom/bottom-sheet";
import {
  CameraView,
  type BarcodeScanningResult,
  useCameraPermissions,
} from "expo-camera";
import { LAMPORTS_PER_SOL } from "@solana/web3.js";
import * as Clipboard from "expo-clipboard";
import * as Haptics from "expo-haptics";
import {
  ArrowLeft,
  ChevronDown,
  ScanLine,
  Search,
  Wallet,
  X,
} from "lucide-react-native";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  ActivityIndicator,
  Image,
  Keyboard,
  Linking,
  Modal,
  StatusBar,
  StyleSheet,
  useWindowDimensions,
} from "react-native";
import Animated, {
  useAnimatedKeyboard,
  useAnimatedStyle,
} from "react-native-reanimated";
import { useSafeAreaInsets } from "react-native-safe-area-context";
import { useFixedSheetLayout } from "@/hooks/useFixedSheetLayout";
import { useKeyboardRescueFocus } from "@/hooks/useKeyboardRescueFocus";

import {
  amountFieldStyles,
  BASE58_ADDRESS_PATTERN,
  fitAmountFontSize,
  focusInputAfterSheetSettles,
  formatAmountInputDisplay,
  formatSolAmount,
  formatTokenAmount,
  formatUsdAmount,
  isValidSolanaAddress,
  MascotReveal,
  SCREEN_WIDTH,
  presentSheetWhenOpen,
  SendingSpinner,
  SheetBackdrop,
  solscanExplorerUrl,
  stripAmountInput,
  truncateAmountTo6Decimals,
  useCaretBlink,
  useSheetSettledRefocus,
} from "@/components/sheets/sheetFacts";
import { useWalletTransactions } from "@/hooks/wallet/useWalletTransactions";
import { track } from "@/lib/analytics/analytics";
import { SEND_EVENTS } from "@/lib/analytics/send-events";
import {
  MAX_RECENT_RECIPIENTS,
  NATIVE_SOL_MINT,
  SOLANA_FEE_SOL,
} from "@/lib/solana/constants";
import type { TokenDetailsByMint } from "@/hooks/wallet/useTokenDetails";
import { resolveTokenIcon } from "@/lib/solana/token-holdings/resolve-token-info";
import type { TokenHolding } from "@/lib/solana/token-holdings/types";
import { formatSenderAddress } from "@/lib/solana/wallet/formatters";
import { sendSolTransaction, sendSplTokenTransaction } from "@/lib/solana/wallet/wallet-details";
import { useWallet } from "@/lib/wallet/wallet-provider";
import { Pressable, Text, View } from "@/tw";
import type { Transaction } from "@/types/wallet";

import SwapCurrencyIcon from "../../../assets/images/icons/swap_currency_28.svg";
import XCloseIcon from "../../../assets/images/icons/x_close_24.svg";
import SendErrorDog from "../../../assets/images/wallet/send_error_dog.svg";
import SendSuccessDog from "../../../assets/images/wallet/send_success_dog.svg";

function formatRecipientLabel(value: string): string {
  return formatSenderAddress(value.trim());
}

const INVALID_RECIPIENT_HINT = "Invalid Solana address";

function formatFeeUsd(feeUsd: number | null): string {
  if (feeUsd === null) return "";
  if (feeUsd < 0.01) return "<$0.01";
  return `≈ ${formatUsdAmount(feeUsd)}`;
}

function getFriendlyError(raw: string): string {
  const lower = raw.toLowerCase();
  if (lower.includes("insufficient lamports") || lower.includes("not enough sol"))
    return "You don't have enough SOL to complete this transaction.";
  if (lower.includes("insufficient funds"))
    return "Insufficient funds for this transaction.";
  if (lower.includes("blockhash not found") || lower.includes("block height exceeded"))
    return "The transaction expired. Please try again.";
  if (lower.includes("timeout") || lower.includes("timed out"))
    return "The transaction timed out. Please try again.";
  if (raw.length > 120) return "Something went wrong. Please try again.";
  return raw;
}

type SendStep = "recipient" | "form" | "confirm" | "result";

type RecentRecipient = {
  address: string;
  sendCount: number;
};

function deriveRecentRecipients(transactions: Transaction[]): RecentRecipient[] {
  const byRecipient = new Map<
    string,
    { address: string; sendCount: number; lastTimestamp: number }
  >();

  for (const tx of transactions) {
    if (tx.type !== "outgoing") continue;
    const address = tx.recipient?.trim();
    if (!address) continue;
    const key = address.toLowerCase();
    const existing = byRecipient.get(key);
    byRecipient.set(key, {
      address: existing?.address ?? address,
      sendCount: (existing?.sendCount ?? 0) + 1,
      lastTimestamp: Math.max(existing?.lastTimestamp ?? 0, tx.timestamp ?? 0),
    });
  }

  return Array.from(byRecipient.values())
    .sort((a, b) => b.lastTimestamp - a.lastTimestamp)
    .slice(0, MAX_RECENT_RECIPIENTS)
    .map(({ address, sendCount }) => ({ address, sendCount }));
}

type SendSheetProps = {
  open: boolean;
  onClose: () => void;
  solBalanceLamports: number | null;
  solPriceUsd: number | null;
  tokenHoldings: TokenHolding[];
  tokenDetailsByMint?: TokenDetailsByMint;
  onSendComplete?: () => void;
  initialMint?: string;
  initialShowScanner?: boolean;
};

type SendAsset = {
  key: string;
  mint: string;
  symbol: string;
  name: string;
  decimals: number;
  balance: number;
  priceUsd: number | null;
  imageUrl: string | null;
};

const SOL_ADDRESS_CANDIDATE_REGEX = new RegExp(BASE58_ADDRESS_PATTERN.source.slice(1, -1), "g");

function safeDecodeUriComponent(value: string): string {
  try {
    return decodeURIComponent(value);
  } catch {
    return value;
  }
}

function extractSolanaAddressFromScan(rawData: string): string | null {
  const trimmed = rawData.trim();
  if (!trimmed) return null;

  const possibleInputs = [trimmed, safeDecodeUriComponent(trimmed)];
  const queryKeys = ["to", "address", "recipient", "pubkey"];

  for (const input of possibleInputs) {
    const solanaPrefixed = input
      .replace(/^solana:(\/\/)?/i, "")
      .split("?")[0]
      .split("#")[0];
    if (isValidSolanaAddress(solanaPrefixed)) return solanaPrefixed;

    try {
      const parsed = new URL(input);
      for (const key of queryKeys) {
        const paramValue = parsed.searchParams.get(key);
        if (!paramValue) continue;
        const normalizedValue = safeDecodeUriComponent(paramValue).trim();
        if (isValidSolanaAddress(normalizedValue)) return normalizedValue;
      }
    } catch {
    }

    const candidates = input.match(SOL_ADDRESS_CANDIDATE_REGEX) ?? [];
    for (const candidate of candidates) {
      if (isValidSolanaAddress(candidate)) return candidate;
    }
  }

  return null;
}

function toRawAmount(amount: number, decimals: number): bigint {
  const scale = 10 ** decimals;
  const scaled = Math.floor(amount * scale);
  if (!Number.isFinite(scaled) || scaled <= 0) {
    throw new Error("Enter a valid amount");
  }
  return BigInt(scaled);
}

const SEND_FEE_RESERVE_SOL = SOLANA_FEE_SOL;

function buildSendAssets(
  tokenHoldings: TokenHolding[],
  solBalanceLamports: number | null,
  solPriceUsd: number | null,
): SendAsset[] {
  const assetsByKey = new Map<string, SendAsset>();
  const eligibleHoldings = tokenHoldings.filter(
    (holding) => holding.balance > 0,
  );

  for (const holding of eligibleHoldings) {
    const key = holding.mint;
    const existing = assetsByKey.get(key);
    const candidate: SendAsset = {
      key,
      mint: holding.mint,
      symbol: holding.symbol || "TOKEN",
      name: holding.name || holding.symbol || "Token",
      decimals: holding.decimals,
      balance: holding.balance,
      priceUsd: holding.priceUsd,
      imageUrl: holding.imageUrl,
    };

    if (!existing || candidate.balance > existing.balance) {
      assetsByKey.set(key, candidate);
    }
  }

  const solBalance = solBalanceLamports ? solBalanceLamports / LAMPORTS_PER_SOL : 0;
  if (solBalance > 0) {
    const existingSol = assetsByKey.get(NATIVE_SOL_MINT);
    assetsByKey.set(NATIVE_SOL_MINT, {
      key: NATIVE_SOL_MINT,
      mint: NATIVE_SOL_MINT,
      symbol: existingSol?.symbol || "SOL",
      name: existingSol?.name || "Solana",
      decimals: 9,
      balance: solBalance,
      priceUsd: existingSol?.priceUsd ?? solPriceUsd,
      imageUrl: existingSol?.imageUrl ?? null,
    });
  }

  return [...assetsByKey.values()].sort((a, b) => {
    const aUsd = (a.priceUsd ?? 0) * a.balance;
    const bUsd = (b.priceUsd ?? 0) * b.balance;
    if (bUsd !== aUsd) return bUsd - aUsd;
    return a.symbol.localeCompare(b.symbol);
  });
}

function resolveInitialSendKey(
  sendAssets: SendAsset[],
  initialMint?: string,
): string {
  if (initialMint) {
    const anyMatch = sendAssets.find((asset) => asset.mint === initialMint);
    if (anyMatch) return anyMatch.key;
  }

  return sendAssets[0]?.key ?? NATIVE_SOL_MINT;
}

export function SendSheet({
  open,
  onClose,
  solBalanceLamports,
  solPriceUsd,
  tokenHoldings,
  tokenDetailsByMint,
  onSendComplete,
  initialMint,
  initialShowScanner,
}: SendSheetProps) {
  const { signer, publicKey } = useWallet();
  const bottomSheetRef = useRef<BottomSheetModal>(null);
  const { sheetHeight, snapPoints } = useFixedSheetLayout();
  const recipientInputRef = useRef<{ focus: () => void } | null>(null);
  const amountInputRef = useRef<{ focus: () => void } | null>(null);
  const sheetSettledRef = useRef(false);
  const [cameraPermission, requestCameraPermission] = useCameraPermissions();
  const scanLockRef = useRef(false);
  const scanUnlockTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const [step, setStep] = useState<SendStep>("recipient");
  const [showQrScanner, setShowQrScanner] = useState(false);
  const [showTokenPicker, setShowTokenPicker] = useState(false);
  const [selectedAssetKey, setSelectedAssetKey] = useState<string>(NATIVE_SOL_MINT);
  const [recipient, setRecipient] = useState("");
  const [scanError, setScanError] = useState<string | null>(null);
  const [amountStr, setAmountStr] = useState("");
  const [currencyMode, setCurrencyMode] = useState<"TOKEN" | "USD">("TOKEN");
  const [isSending, setIsSending] = useState(false);
  const [sendError, setSendError] = useState<string | null>(null);
  const [txSignature, setTxSignature] = useState<string | null>(null);

  const { walletTransactions } = useWalletTransactions(open ? publicKey : null);
  const recentRecipients = useMemo(
    () => deriveRecentRecipients(walletTransactions),
    [walletTransactions],
  );

  const sendAssets = useMemo(
    () => buildSendAssets(tokenHoldings, solBalanceLamports, solPriceUsd),
    [tokenHoldings, solBalanceLamports, solPriceUsd],
  );
  const selectedAsset = useMemo(() => {
    return (
      sendAssets.find((asset) => asset.key === selectedAssetKey) ??
      sendAssets[0] ??
      null
    );
  }, [sendAssets, selectedAssetKey]);
  const tokenPriceUsd = selectedAsset?.priceUsd ?? null;
  const balanceInToken = selectedAsset?.balance ?? 0;

  const amountNum = parseFloat(amountStr) || 0;
  const amountInToken =
    currencyMode === "TOKEN"
      ? amountNum
      : tokenPriceUsd
        ? amountNum / tokenPriceUsd
        : 0;
  const amountInUsd =
    currencyMode === "USD"
      ? amountNum
      : tokenPriceUsd
        ? amountNum * tokenPriceUsd
        : 0;

  const recipientTrimmed = recipient.trim();
  const isValidRecipient = isValidSolanaAddress(recipientTrimmed);
  const recipientSummaryLabel = formatRecipientLabel(recipientTrimmed);
  const sendFeeReserveSol = SEND_FEE_RESERVE_SOL;
  const maxSpendableInToken =
    selectedAsset?.mint === NATIVE_SOL_MINT
      ? Math.max(0, balanceInToken - sendFeeReserveSol)
      : balanceInToken;
  const minAmountInToken = selectedAsset ? 1 / (10 ** selectedAsset.decimals) : 0;
  const isValidAmount =
    !!selectedAsset &&
    amountInToken >= minAmountInToken &&
    amountInToken <= maxSpendableInToken;
  const isFormValid =
    !!selectedAsset && isValidRecipient && isValidAmount;

  useEffect(() => {
    if (sendAssets.length === 0) return;
    if (!sendAssets.some((asset) => asset.key === selectedAssetKey)) {
      setSelectedAssetKey(resolveInitialSendKey(sendAssets, initialMint));
    }
  }, [initialMint, selectedAssetKey, sendAssets]);

  useEffect(() => {
    return () => {
      if (scanUnlockTimerRef.current) {
        clearTimeout(scanUnlockTimerRef.current);
      }
    };
  }, []);

  const handleOpenQrScanner = useCallback(async () => {
    Keyboard.dismiss();
    setScanError(null);
    setShowTokenPicker(false);
    setShowQrScanner(true);

    let granted = cameraPermission?.granted ?? false;
    if (!granted) {
      const response = await requestCameraPermission();
      granted = response.granted;
    }

    if (!granted) {
      Haptics.notificationAsync(Haptics.NotificationFeedbackType.Error);
      return;
    }

    scanLockRef.current = false;
    if (scanUnlockTimerRef.current) {
      clearTimeout(scanUnlockTimerRef.current);
      scanUnlockTimerRef.current = null;
    }
    Haptics.impactAsync(Haptics.ImpactFeedbackStyle.Light);
  }, [cameraPermission, requestCameraPermission]);

  const prevOpenRef = useRef<boolean | null>(null);
  useEffect(() => {
    const wasOpen = prevOpenRef.current;
    prevOpenRef.current = open;
    if (wasOpen === open) return;
    presentSheetWhenOpen(bottomSheetRef.current, open);
    if (open) {
      setStep("recipient");
      setShowQrScanner(false);
      setShowTokenPicker(false);
      setSelectedAssetKey(resolveInitialSendKey(sendAssets, initialMint));
      setRecipient("");
      setScanError(null);
      setAmountStr("");
      setCurrencyMode("TOKEN");
      setSendError(null);
      setTxSignature(null);
      setIsSending(false);
      scanLockRef.current = false;
      if (scanUnlockTimerRef.current) {
        clearTimeout(scanUnlockTimerRef.current);
        scanUnlockTimerRef.current = null;
      }
      if (initialShowScanner) {
        void handleOpenQrScanner();
      }
    }
  }, [open, sendAssets, initialMint, initialShowScanner, handleOpenQrScanner]);

  const handleSend = useCallback(async () => {
    if (!isFormValid || isSending) return;

    Keyboard.dismiss();
    setIsSending(true);
    setSendError(null);
    setStep("result");

    try {
      if (!selectedAsset) {
        throw new Error("No available token balance");
      }

      if (!signer) {
        throw new Error("Wallet signer is not available");
      }
      let sig: string;
      if (selectedAsset.mint === NATIVE_SOL_MINT) {
        sig = await sendSolTransaction(
          recipientTrimmed,
          Math.floor(amountInToken * LAMPORTS_PER_SOL),
          signer,
        );
      } else {
        sig = await sendSplTokenTransaction(
          recipientTrimmed,
          selectedAsset.mint,
          toRawAmount(amountInToken, selectedAsset.decimals),
          selectedAsset.decimals,
          signer,
        );
      }

      setTxSignature(sig);
      Haptics.notificationAsync(Haptics.NotificationFeedbackType.Success);
      track(SEND_EVENTS.sendFunds, {
        method: "wallet_address",
      });
      onSendComplete?.();
    } catch (error) {
      const msg =
        error instanceof Error ? error.message : "Transaction failed";
      setSendError(getFriendlyError(msg));
      Haptics.notificationAsync(Haptics.NotificationFeedbackType.Error);
      track(SEND_EVENTS.sendFundsFailed, {
        method: "wallet_address",
      });
    } finally {
      setIsSending(false);
    }
  }, [
    isFormValid,
    isSending,
    selectedAsset,
    recipientTrimmed,
    amountInToken,
    onSendComplete,
    signer,
  ]);

  const handleClose = useCallback(() => {
    setShowQrScanner(false);
    setShowTokenPicker(false);
    bottomSheetRef.current?.dismiss();
    onClose();
  }, [onClose]);

  const toggleCurrency = useCallback(() => {
    if (!tokenPriceUsd) return;
    Haptics.impactAsync(Haptics.ImpactFeedbackStyle.Light);
    if (currencyMode === "TOKEN") {
      const usd = amountNum * tokenPriceUsd;
      setCurrencyMode("USD");
      setAmountStr(usd > 0 ? usd.toFixed(2) : "");
    } else {
      const tokenAmount = amountNum / tokenPriceUsd;
      setCurrencyMode("TOKEN");
      setAmountStr(tokenAmount > 0 ? String(Number(tokenAmount.toFixed(6))) : "");
    }
  }, [currencyMode, amountNum, tokenPriceUsd]);

  const handlePasteRecipient = useCallback(async () => {
    const pasted = await Clipboard.getStringAsync();
    const normalized = pasted.replace(/\s+/g, "");
    if (!normalized) return;
    setRecipient(normalized);
    Haptics.impactAsync(Haptics.ImpactFeedbackStyle.Light);
  }, []);

  const handleBarcodeScanned = useCallback(
    (event: BarcodeScanningResult) => {
      if (scanLockRef.current) return;
      scanLockRef.current = true;

      const scannedAddress = extractSolanaAddressFromScan(event.data);
      if (!scannedAddress) {
        setScanError("No valid Solana address found in that QR code.");
        Haptics.notificationAsync(Haptics.NotificationFeedbackType.Error);
        scanUnlockTimerRef.current = setTimeout(() => {
          scanLockRef.current = false;
          scanUnlockTimerRef.current = null;
        }, 800);
        return;
      }

      setRecipient(scannedAddress);
      setScanError(null);
      setShowQrScanner(false);
      Haptics.notificationAsync(Haptics.NotificationFeedbackType.Success);
    },
    [],
  );

  const handleSelectAsset = useCallback(
    (key: string) => {
      setSelectedAssetKey(key);
      setShowTokenPicker(false);
      setAmountStr("");
      setCurrencyMode("TOKEN");
      Haptics.impactAsync(Haptics.ImpactFeedbackStyle.Light);
    },
    [],
  );

  const handlePercentage = useCallback(
    (pct: number) => {
      if (!selectedAsset) return;

      const maxAmount = maxSpendableInToken;
      const raw = pct === 100 ? maxAmount : maxAmount * (pct / 100);
      const truncatedTokens = truncateAmountTo6Decimals(raw);

      if (currencyMode === "TOKEN") {
        setAmountStr(truncatedTokens > 0 ? String(truncatedTokens) : "");
      } else if (tokenPriceUsd) {
        const usd = truncatedTokens * tokenPriceUsd;
        setAmountStr(
          usd > 0 ? (Math.floor(usd * 100) / 100).toFixed(2) : "",
        );
      }
      Haptics.impactAsync(Haptics.ImpactFeedbackStyle.Light);
    },
    [selectedAsset, maxSpendableInToken, currencyMode, tokenPriceUsd],
  );

  const focusActiveInput = useCallback(() => {
    if (step === "recipient") {
      recipientInputRef.current?.focus();
    } else if (step === "form" && !showTokenPicker) {
      amountInputRef.current?.focus();
    }
  }, [step, showTokenPicker]);

  const handleSheetChange = useCallback(
    (index: number) => {
      sheetSettledRef.current = index >= 0;
      focusInputAfterSheetSettles(focusActiveInput, index);
    },
    [focusActiveInput],
  );
  useSheetSettledRefocus(sheetSettledRef, focusActiveInput);

  return (
    <>
    <BottomSheetModal
      ref={bottomSheetRef}
      snapPoints={snapPoints}
      enableDynamicSizing={false}
      enablePanDownToClose={step !== "result" || !isSending}
      backdropComponent={SheetBackdrop}
      onDismiss={onClose}
      onChange={handleSheetChange}
      handleComponent={null}
      backgroundStyle={{ borderTopLeftRadius: 38, borderTopRightRadius: 38 }}
      keyboardBehavior="extend"
      keyboardBlurBehavior="restore"
      android_keyboardInputMode="adjustResize"
    >
      <BottomSheetView
        style={{
          height: sheetHeight,
          backgroundColor: "#fff",
          borderTopLeftRadius: 38,
          borderTopRightRadius: 38,
          overflow: "hidden",
        }}
      >
        {step === "recipient" && (
          <RecipientStep
            recipient={recipient}
            onRecipientChange={setRecipient}
            onPasteRecipient={handlePasteRecipient}
            onScanRecipient={handleOpenQrScanner}
            onClose={handleClose}
            onPickRecipient={setRecipient}
            onProceed={() => setStep("form")}
            recentRecipients={recentRecipients}
            inputRef={recipientInputRef}
          />
        )}

        {step === "form" && !showTokenPicker && (
          <AmountStep
            recipientLabel={recipientSummaryLabel}
            onBack={() => setStep("recipient")}
            selectedAsset={selectedAsset}
            tokenDetailsByMint={tokenDetailsByMint}
            onAssetPress={() => {
              Keyboard.dismiss();
              setShowTokenPicker(true);
            }}
            amountStr={amountStr}
            onAmountChange={setAmountStr}
            currencyMode={currencyMode}
            onToggleCurrency={toggleCurrency}
            onMax={() => handlePercentage(100)}
            amountInToken={amountInToken}
            amountInUsd={amountInUsd}
            maxSpendableInToken={maxSpendableInToken}
            tokenPriceUsd={tokenPriceUsd}
            isValidAmount={amountStr.length > 0 ? isValidAmount : true}
            isFormValid={isFormValid}
            onNext={() => {
              Keyboard.dismiss();
              setStep("confirm");
            }}
            inputRef={amountInputRef}
          />
        )}

        {step === "form" && showTokenPicker && (
          <BottomSheetScrollView
            style={{ flex: 1 }}
            keyboardShouldPersistTaps="handled"
          >
            <AssetPickerStep
              assets={sendAssets}
              tokenDetailsByMint={tokenDetailsByMint}
              onSelect={handleSelectAsset}
              onBack={() => setShowTokenPicker(false)}
            />
          </BottomSheetScrollView>
        )}

        {step === "confirm" && (
          <ConfirmStep
            recipientLabel={recipientSummaryLabel}
            selectedAsset={selectedAsset}
            tokenDetailsByMint={tokenDetailsByMint}
            amountInToken={amountInToken}
            amountInUsd={amountInUsd}
            feeSol={sendFeeReserveSol}
            solPriceUsd={solPriceUsd}
            isSending={isSending}
            onBack={() => setStep("form")}
            onConfirm={handleSend}
          />
        )}

        {step === "result" && (
          <ResultStep
            isSending={isSending}
            sendError={sendError}
            txSignature={txSignature}
            amountInToken={amountInToken}
            tokenSymbol={selectedAsset?.symbol ?? "TOKEN"}
            recipientLabel={recipientSummaryLabel}
            onClose={handleClose}
          />
        )}
      </BottomSheetView>
    </BottomSheetModal>
    <QrScannerModal
      visible={showQrScanner}
      onClose={() => {
        setShowQrScanner(false);
        setScanError(null);
      }}
      onScan={handleBarcodeScanned}
      scanError={scanError}
      permissionGranted={cameraPermission?.granted === true}
      canAskPermissionAgain={cameraPermission?.canAskAgain !== false}
      onRequestPermission={requestCameraPermission}
    />
    </>
  );
}

function RecipientGroupHeader({ title }: { title: string }) {
  return (
    <View style={{ paddingHorizontal: 16 }}>
      <View style={{ paddingTop: 12, paddingBottom: 8 }}>
        <Text
          className="text-[17px] font-medium"
          style={{
            color: "rgba(60,60,67,0.6)",
            letterSpacing: -0.187,
            lineHeight: 22,
          }}
        >
          {title}
        </Text>
      </View>
    </View>
  );
}

function RecipientCell({
  label,
  subtitle,
  onPress,
}: {
  label: string;
  subtitle: string;
  onPress: () => void;
}) {
  return (
    <Pressable
      className="w-full flex-row items-center"
      style={{ paddingHorizontal: 16 }}
      onPress={onPress}
      accessibilityRole="button"
      accessibilityLabel={`Send to ${label}`}
    >
      <View style={{ paddingRight: 12, paddingVertical: 6 }}>
        <View
          className="items-center justify-center rounded-xl"
          style={{ width: 48, height: 48, backgroundColor: "rgba(0,0,0,0.04)" }}
        >
          <Wallet size={28} color="rgba(60,60,67,0.4)" strokeWidth={1.6} />
        </View>
      </View>
      <View className="flex-1" style={{ paddingVertical: 9 }}>
        <Text
          className="text-[17px] font-medium text-black"
          style={{ lineHeight: 22 }}
          numberOfLines={1}
        >
          {label}
        </Text>
        <Text
          className="text-[15px] font-normal"
          style={{ color: "rgba(60,60,67,0.6)", lineHeight: 20 }}
          numberOfLines={1}
        >
          {subtitle}
        </Text>
      </View>
    </Pressable>
  );
}

function formatSendCount(count: number): string {
  return `${count} ${count === 1 ? "send" : "sends"}`;
}

function RecipientStep({
  recipient,
  onRecipientChange,
  onPasteRecipient,
  onScanRecipient,
  onClose,
  onPickRecipient,
  onProceed,
  recentRecipients,
  inputRef,
}: {
  recipient: string;
  onRecipientChange: (value: string) => void;
  onPasteRecipient: () => void;
  onScanRecipient: () => void;
  onClose: () => void;
  onPickRecipient: (address: string) => void;
  onProceed: () => void;
  recentRecipients: RecentRecipient[];
  inputRef: React.MutableRefObject<{
    focus: () => void;
    blur?: () => void;
  } | null>;
}) {
  const insets = useSafeAreaInsets();
  const keyboard = useAnimatedKeyboard();
  const footerStyle = useAnimatedStyle(() => ({
    transform: [{ translateY: -keyboard.height.value }],
  }));

  const trimmed = recipient.trim();
  const hasInput = trimmed.length > 0;
  const isValid = isValidSolanaAddress(trimmed);
  const showError = hasInput && !isValid;
  const matchedRecent = recentRecipients.find(
    (entry) => entry.address.toLowerCase() === trimmed.toLowerCase(),
  );

  return (
    <View className="w-full" style={{ flex: 1 }}>
      <View className="w-full" style={{ paddingVertical: 16 }}>
        <View className="flex-row items-center justify-between px-4">
          <Pressable
            className="h-11 w-11 items-center justify-center rounded-full bg-[#f2f2f7]"
            hitSlop={6}
            onPress={onClose}
            accessibilityRole="button"
            accessibilityLabel="Close"
          >
            <X size={24} color="rgba(60,60,67,0.6)" strokeWidth={2} />
          </Pressable>
          <View
            style={{
              position: "absolute",
              left: 0,
              right: 0,
              top: 0,
              bottom: 0,
              alignItems: "center",
              justifyContent: "center",
              pointerEvents: "none",
            }}
          >
            <Text
              className="text-[17px] font-semibold text-black"
              style={{ lineHeight: 22 }}
            >
              Recipient
            </Text>
          </View>
          <Pressable
            className="h-11 w-11 items-center justify-center rounded-full bg-[#f2f2f7]"
            hitSlop={6}
            onPress={onScanRecipient}
            accessibilityRole="button"
            accessibilityLabel="Scan QR code"
          >
            <ScanLine size={24} color="rgba(60,60,67,0.6)" strokeWidth={1.8} />
          </Pressable>
        </View>
      </View>

      <View style={{ paddingTop: 36, paddingHorizontal: 16, paddingBottom: 16 }}>
        <View className="flex-row items-start" style={{ minHeight: 48 }}>
          <BottomSheetTextInput
            ref={inputRef as unknown as React.Ref<never>}
            style={{
              flex: 1,
              minHeight: 48,
              paddingVertical: 12,
              fontFamily: "Geist_500Medium",
              fontSize: 20,
              lineHeight: 24,
              color: "#000",
            }}
            placeholder="Solana address"
            placeholderTextColor="rgba(60,60,67,0.4)"
            value={recipient}
            onChangeText={onRecipientChange}
            autoCapitalize="none"
            autoComplete="off"
            autoCorrect={false}
            multiline
            scrollEnabled={false}
          />
          {hasInput ? (
            <Pressable
              className="items-center justify-center"
              style={{ paddingLeft: 12, paddingVertical: 12 }}
              hitSlop={6}
              onPress={() => onRecipientChange("")}
              accessibilityRole="button"
              accessibilityLabel="Clear recipient"
            >
              <XCloseIcon width={24} height={24} />
            </Pressable>
          ) : (
            <View
              className="justify-center"
              style={{ paddingLeft: 12, height: 48 }}
            >
              <Pressable
                className="items-center justify-center rounded-full"
                style={{
                  minWidth: 64,
                  paddingHorizontal: 16,
                  paddingVertical: 8,
                  backgroundColor: "rgba(0,0,0,0.04)",
                }}
                onPress={onPasteRecipient}
                accessibilityRole="button"
                accessibilityLabel="Paste from clipboard"
              >
                <Text
                  className="text-[15px] font-medium text-black"
                  style={{ lineHeight: 20 }}
                >
                  Paste
                </Text>
              </Pressable>
            </View>
          )}
        </View>
        {showError ? (
          <Text
            className="text-[15px] font-normal"
            style={{ color: "#f9363c", lineHeight: 20, marginTop: 4 }}
          >
            {INVALID_RECIPIENT_HINT}
          </Text>
        ) : null}
      </View>

      <BottomSheetScrollView
        style={{ flex: 1 }}
        contentContainerStyle={{ paddingBottom: 96 }}
        keyboardShouldPersistTaps="handled"
      >
        {isValid ? (
          <View className="w-full">
            <RecipientGroupHeader title="Exact match" />
            <RecipientCell
              label={formatRecipientLabel(trimmed)}
              subtitle={
                matchedRecent
                  ? formatSendCount(matchedRecent.sendCount)
                  : "New address"
              }
              onPress={() => onPickRecipient(trimmed)}
            />
          </View>
        ) : recentRecipients.length > 0 ? (
          <View className="w-full">
            <RecipientGroupHeader title="Recently used" />
            {recentRecipients.map((entry) => (
              <RecipientCell
                key={entry.address}
                label={formatRecipientLabel(entry.address)}
                subtitle={formatSendCount(entry.sendCount)}
                onPress={() => onPickRecipient(entry.address)}
              />
            ))}
          </View>
        ) : null}
      </BottomSheetScrollView>

      <Animated.View
        style={[
          {
            position: "absolute",
            left: 0,
            right: 0,
            bottom: 0,
            backgroundColor: "#fff",
            paddingHorizontal: 20,
            paddingTop: 16,
            paddingBottom: insets.bottom + 12,
          },
          footerStyle,
        ]}
      >
        <Pressable
          onPress={() => {
            if (isValid) onProceed();
          }}
          disabled={!isValid}
          accessibilityRole="button"
          accessibilityLabel="Proceed"
          style={{
            height: 50,
            borderRadius: 78,
            alignItems: "center",
            justifyContent: "center",
            backgroundColor: "#000",
            opacity: isValid ? 1 : 0.4,
          }}
        >
          <Text
            className="text-[16px] font-medium text-white"
            style={{ lineHeight: 20 }}
          >
            Proceed
          </Text>
        </Pressable>
      </Animated.View>
    </View>
  );
}

function AmountStep({
  recipientLabel,
  onBack,
  selectedAsset,
  tokenDetailsByMint,
  onAssetPress,
  amountStr,
  onAmountChange,
  currencyMode,
  onToggleCurrency,
  onMax,
  amountInToken,
  amountInUsd,
  maxSpendableInToken,
  tokenPriceUsd,
  isValidAmount,
  isFormValid,
  onNext,
  inputRef,
}: {
  recipientLabel: string;
  onBack: () => void;
  selectedAsset: SendAsset | null;
  tokenDetailsByMint?: TokenDetailsByMint;
  onAssetPress: () => void;
  amountStr: string;
  onAmountChange: (v: string) => void;
  currencyMode: "TOKEN" | "USD";
  onToggleCurrency: () => void;
  onMax: () => void;
  amountInToken: number;
  amountInUsd: number;
  maxSpendableInToken: number;
  tokenPriceUsd: number | null;
  isValidAmount: boolean;
  isFormValid: boolean;
  onNext: () => void;
  inputRef: React.MutableRefObject<{
    focus: () => void;
    blur?: () => void;
  } | null>;
}) {
  const { openKeyboard } = useKeyboardRescueFocus(inputRef);
  const insets = useSafeAreaInsets();
  const keyboard = useAnimatedKeyboard();
  const footerStyle = useAnimatedStyle(() => ({
    transform: [{ translateY: -keyboard.height.value }],
  }));

  const [isFocused, setIsFocused] = useState(false);
  const caretOn = useCaretBlink(isFocused);

  const symbol = selectedAsset?.symbol ?? "TOKEN";
  const selectedAssetMint = selectedAsset?.mint ?? NATIVE_SOL_MINT;
  const assetIcon = resolveTokenIcon({
    mint: selectedAssetMint,
    imageUrl: selectedAsset?.imageUrl,
    detailLogoUrl: tokenDetailsByMint?.[selectedAssetMint]?.token.logoUrl,
  });
  const balance = selectedAsset?.balance ?? 0;

  const displayAmount = formatAmountInputDisplay(amountStr);
  const showAmountError = amountStr.length > 0 && !isValidAmount;
  const amountErrorText =
    amountInToken > maxSpendableInToken
      ? "Insufficient balance"
      : "Enter a valid amount";
  const conversionText =
    currencyMode === "USD"
      ? `${formatTokenAmount(amountInToken, 0)} ${symbol}`
      : formatUsdAmount(amountInUsd);

  const amountFontSize = useMemo(
    () =>
      fitAmountFontSize(
        `${currencyMode === "USD" ? "$" : ""}${displayAmount || "0"}`,
        SCREEN_WIDTH - 32 - 52, // px-16*2 + swap button
      ),
    [currencyMode, displayAmount],
  );

  const amountTextStyle = {
    ...amountFieldStyles.text,
    fontSize: amountFontSize,
  };

  return (
    <View className="w-full" style={{ flex: 1 }}>
      <View className="w-full" style={{ paddingVertical: 16 }}>
        <View className="flex-row items-center justify-between px-4">
          <Pressable
            className="h-11 w-11 items-center justify-center rounded-full bg-[#f2f2f7]"
            hitSlop={6}
            onPress={onBack}
            accessibilityRole="button"
            accessibilityLabel="Back"
          >
            <ArrowLeft size={24} color="rgba(60,60,67,0.6)" strokeWidth={2} />
          </Pressable>
          <View
            style={{
              position: "absolute",
              left: 0,
              right: 0,
              top: 0,
              bottom: 0,
              alignItems: "center",
              justifyContent: "center",
              pointerEvents: "none",
            }}
          >
            <Text
              className="text-[17px] font-semibold text-black"
              style={{ lineHeight: 22 }}
            >
              Send
            </Text>
            <Text
              className="text-[15px] font-normal"
              style={{ color: "rgba(60,60,67,0.6)", lineHeight: 20, marginTop: 2 }}
            >
              to {recipientLabel}
            </Text>
          </View>
          <View className="h-11 w-11" style={{ opacity: 0 }} />
        </View>
      </View>

      <View style={{ paddingTop: 36, paddingHorizontal: 16 }}>
        <Pressable style={amountFieldStyles.row} onPress={openKeyboard}>
          <View
            style={[
              amountFieldStyles.visual,
              { paddingRight: 52, pointerEvents: "none" },
            ]}
          >
            {currencyMode === "USD" ? (
              <Text style={amountTextStyle}>$</Text>
            ) : null}
            <Text style={amountTextStyle}>{displayAmount || "0"}</Text>
            <View
              style={[
                amountFieldStyles.caret,
                { opacity: isFocused && caretOn ? 1 : 0 },
              ]}
            />
          </View>
          <BottomSheetTextInput
            ref={inputRef as unknown as React.Ref<never>}
            value={displayAmount}
            onChangeText={(t) => onAmountChange(stripAmountInput(t))}
            onFocus={() => setIsFocused(true)}
            onBlur={() => setIsFocused(false)}
            keyboardType="decimal-pad"
            inputMode="decimal"
            maxLength={15}
            caretHidden
            pointerEvents="none"
            style={amountFieldStyles.overlayInput}
            accessibilityLabel="Send amount"
          />
          {tokenPriceUsd ? (
            <Pressable
              onPress={onToggleCurrency}
              hitSlop={6}
              accessibilityRole="button"
              accessibilityLabel="Switch between token and USD"
              style={{
                position: "absolute",
                right: 0,
                top: 0,
                bottom: 0,
                width: 44,
                alignItems: "center",
                justifyContent: "center",
              }}
            >
              <SwapCurrencyIcon width={28} height={28} />
            </Pressable>
          ) : null}
        </Pressable>
        <Text
          className="text-[15px] font-normal"
          style={{
            color: showAmountError ? "#f9363c" : "rgba(60,60,67,0.6)",
            lineHeight: 20,
            marginTop: 4,
          }}
        >
          {showAmountError ? amountErrorText : conversionText}
        </Text>
      </View>

      <Animated.View
        style={[
          {
            position: "absolute",
            left: 0,
            right: 0,
            bottom: 0,
            backgroundColor: "#fff",
            paddingTop: 16,
            paddingBottom: insets.bottom + 12,
          },
          footerStyle,
        ]}
      >
        <View
          className="w-full flex-row items-center"
          style={{ paddingHorizontal: 16, marginBottom: 16 }}
        >
          <Pressable
            style={{ paddingRight: 12, paddingVertical: 2 }}
            onPress={onAssetPress}
            accessibilityRole="button"
            accessibilityLabel="Select token"
          >
            <View
              className="flex-row items-center"
              style={{
                backgroundColor: "rgba(0,0,0,0.04)",
                borderRadius: 61,
                padding: 4,
              }}
            >
              <View style={{ position: "relative" }}>
                <Image
                  source={{ uri: assetIcon }}
                  style={{ width: 40, height: 40, borderRadius: 20 }}
                />
              </View>
              <View
                style={{
                  width: 24,
                  height: 16,
                  alignItems: "center",
                  justifyContent: "center",
                }}
              >
                <ChevronDown size={18} color="rgba(60,60,67,0.4)" />
              </View>
            </View>
          </Pressable>
          <View className="flex-1" style={{ paddingVertical: 8 }}>
            <Text
              className="text-[20px] font-semibold text-black"
              style={{ lineHeight: 24 }}
              numberOfLines={1}
            >
              {formatUsdAmount(balance * (tokenPriceUsd ?? 0))}
            </Text>
            <Text
              className="text-[15px] font-normal"
              style={{ color: "rgba(60,60,67,0.6)", lineHeight: 20 }}
              numberOfLines={1}
            >
              {formatTokenAmount(balance)} {symbol}
            </Text>
          </View>
          <View style={{ paddingLeft: 12 }}>
            <Pressable
              className="items-center justify-center"
              style={{
                minWidth: 64,
                paddingHorizontal: 16,
                paddingVertical: 8,
                borderRadius: 40,
                backgroundColor: "rgba(0,0,0,0.04)",
              }}
              onPress={onMax}
              accessibilityRole="button"
              accessibilityLabel="Use max balance"
            >
              <Text
                className="text-[15px] font-medium text-black"
                style={{ lineHeight: 20 }}
              >
                MAX
              </Text>
            </Pressable>
          </View>
        </View>

        <View style={{ paddingHorizontal: 20 }}>
          <Pressable
            className="items-center justify-center"
            style={{
              height: 50,
              borderRadius: 78,
              backgroundColor: "#000",
              opacity: isFormValid ? 1 : 0.4,
            }}
            onPress={onNext}
            disabled={!isFormValid}
            accessibilityRole="button"
            accessibilityLabel="Review send"
          >
            <Text
              className="text-[16px] font-medium text-white"
              style={{ lineHeight: 20 }}
            >
              Review
            </Text>
          </Pressable>
        </View>
      </Animated.View>
    </View>
  );
}

function AssetPickerStep({
  assets,
  tokenDetailsByMint,
  onSelect,
  onBack,
}: {
  assets: SendAsset[];
  tokenDetailsByMint?: TokenDetailsByMint;
  onSelect: (key: string) => void;
  onBack: () => void;
}) {
  const [search, setSearch] = useState("");

  const filteredAssets = useMemo(() => {
    const query = search.trim().toLowerCase();
    if (!query) return assets;
    return assets.filter(
      (asset) =>
        asset.symbol.toLowerCase().includes(query) ||
        asset.name.toLowerCase().includes(query) ||
        asset.mint.toLowerCase().includes(query),
    );
  }, [assets, search]);

  return (
    <View className="w-full">
      <View className="w-full" style={{ paddingVertical: 16 }}>
        <View className="flex-row items-center justify-between px-4">
          <Pressable
            className="h-11 w-11 items-center justify-center rounded-full bg-[#f2f2f7]"
            hitSlop={6}
            onPress={onBack}
            accessibilityRole="button"
            accessibilityLabel="Back"
          >
            <ArrowLeft size={24} color="rgba(60,60,67,0.6)" strokeWidth={2} />
          </Pressable>
          <View
            style={{
              position: "absolute",
              left: 0,
              right: 0,
              top: 0,
              bottom: 0,
              alignItems: "center",
              justifyContent: "center",
              pointerEvents: "none",
            }}
          >
            <Text
              className="text-[17px] font-semibold text-black"
              style={{ lineHeight: 22 }}
            >
              Select asset
            </Text>
          </View>
          <View className="h-11 w-11" style={{ opacity: 0 }} />
        </View>
      </View>

      <View style={{ paddingHorizontal: 16, paddingBottom: 16 }}>
        <View
          className="flex-row items-center"
          style={{
            backgroundColor: "#f2f2f7",
            borderRadius: 47,
            paddingHorizontal: 16,
          }}
        >
          <View style={{ paddingRight: 12, paddingVertical: 14 }}>
            <Search size={24} color="rgba(60,60,67,0.6)" strokeWidth={2} />
          </View>
          <BottomSheetTextInput
            style={{
              flex: 1,
              paddingVertical: 15,
              fontFamily: "Geist_400Regular",
              fontSize: 17,
              color: "#000",
            }}
            placeholder="Search"
            placeholderTextColor="rgba(60,60,67,0.6)"
            value={search}
            onChangeText={setSearch}
            autoCapitalize="none"
            autoCorrect={false}
          />
        </View>
      </View>

      {filteredAssets.map((asset) => {
        const icon = resolveTokenIcon({
          mint: asset.mint,
          imageUrl: asset.imageUrl,
          detailLogoUrl: tokenDetailsByMint?.[asset.mint]?.token.logoUrl,
        });
        return (
          <Pressable
            key={asset.key}
            className="w-full flex-row items-center"
            style={{ paddingHorizontal: 16 }}
            onPress={() => onSelect(asset.key)}
            accessibilityRole="button"
            accessibilityLabel={`Select ${asset.name}`}
          >
            <View style={{ paddingRight: 12, paddingVertical: 6 }}>
              <View style={{ position: "relative" }}>
                <Image
                  source={{ uri: icon }}
                  style={{
                    width: 48,
                    height: 48,
                    borderRadius: 24,
                    borderWidth: 0.5,
                    borderColor: "rgba(0,0,0,0.08)",
                  }}
                />
              </View>
            </View>
            <View className="flex-1" style={{ paddingVertical: 8, gap: 2 }}>
              <Text
                className="text-[17px] font-medium text-black"
                style={{ lineHeight: 22, letterSpacing: -0.187 }}
                numberOfLines={1}
              >
                {asset.name}
              </Text>
              <Text
                className="text-[15px] font-normal"
                style={{ color: "rgba(60,60,67,0.6)", lineHeight: 20 }}
                numberOfLines={1}
              >
                {formatTokenAmount(asset.balance)} {asset.symbol}
              </Text>
            </View>
            <View style={{ paddingLeft: 12 }}>
              <Text
                className="text-[17px] font-medium text-black"
                style={{ lineHeight: 22 }}
              >
                {formatUsdAmount(asset.balance * (asset.priceUsd ?? 0))}
              </Text>
            </View>
          </Pressable>
        );
      })}

      {filteredAssets.length === 0 ? (
        <Text
          className="text-[15px] font-normal"
          style={{
            color: "rgba(60,60,67,0.6)",
            textAlign: "center",
            paddingVertical: 32,
          }}
        >
          No tokens found
        </Text>
      ) : null}
    </View>
  );
}

function QrScannerModal({
  visible,
  onClose,
  onScan,
  scanError,
  permissionGranted,
  canAskPermissionAgain,
  onRequestPermission,
}: {
  visible: boolean;
  onClose: () => void;
  onScan: (event: BarcodeScanningResult) => void;
  scanError: string | null;
  permissionGranted: boolean;
  canAskPermissionAgain: boolean;
  onRequestPermission: () => Promise<{ granted: boolean }>;
}) {
  const insets = useSafeAreaInsets();
  const { width: screenWidth, height: screenHeight } = useWindowDimensions();
  const frameSize = Math.min(280, Math.round(screenWidth * 0.72));
  const sideStripHeight = Math.max(0, (screenHeight - frameSize) / 2 - 40);

  const handleGrantPermission = useCallback(async () => {
    await onRequestPermission();
  }, [onRequestPermission]);

  return (
    <Modal
      visible={visible}
      animationType="fade"
      presentationStyle="overFullScreen"
      transparent
      statusBarTranslucent
      onRequestClose={onClose}
    >
      <StatusBar barStyle="light-content" />
      <View style={qrScannerStyles.root}>
        {permissionGranted ? (
          <CameraView
            style={StyleSheet.absoluteFillObject}
            facing="back"
            barcodeScannerSettings={{ barcodeTypes: ["qr"] }}
            onBarcodeScanned={onScan}
          />
        ) : null}

        <View style={StyleSheet.absoluteFillObject} pointerEvents="none">
          <View style={[qrScannerStyles.dim, { height: sideStripHeight }]} />
          <View style={[qrScannerStyles.middleRow, { height: frameSize }]}>
            <View style={[qrScannerStyles.dim, qrScannerStyles.flex1]} />
            <View style={{ width: frameSize, height: frameSize }}>
              <View style={[qrScannerStyles.bracket, qrScannerStyles.bracketTopLeftH]} />
              <View style={[qrScannerStyles.bracket, qrScannerStyles.bracketTopLeftV]} />
              <View style={[qrScannerStyles.bracket, qrScannerStyles.bracketTopRightH]} />
              <View style={[qrScannerStyles.bracket, qrScannerStyles.bracketTopRightV]} />
              <View style={[qrScannerStyles.bracket, qrScannerStyles.bracketBottomLeftH]} />
              <View style={[qrScannerStyles.bracket, qrScannerStyles.bracketBottomLeftV]} />
              <View style={[qrScannerStyles.bracket, qrScannerStyles.bracketBottomRightH]} />
              <View style={[qrScannerStyles.bracket, qrScannerStyles.bracketBottomRightV]} />
            </View>
            <View style={[qrScannerStyles.dim, qrScannerStyles.flex1]} />
          </View>
          <View style={[qrScannerStyles.dim, qrScannerStyles.flex1]} />
        </View>

        <View
          style={{
            position: "absolute",
            top: insets.top + 12,
            left: 0,
            right: 0,
            flexDirection: "row",
            alignItems: "center",
            justifyContent: "center",
            paddingHorizontal: 16,
          }}
        >
          <Text className="text-[17px] font-semibold text-white" style={{ lineHeight: 22 }}>
            Scan QR
          </Text>
          <Pressable
            onPress={onClose}
            accessibilityRole="button"
            accessibilityLabel="Close scanner"
            hitSlop={12}
            style={{
              position: "absolute",
              right: 16,
              top: -6,
              width: 36,
              height: 36,
              borderRadius: 18,
              alignItems: "center",
              justifyContent: "center",
              backgroundColor: "rgba(0,0,0,0.45)",
            }}
          >
            <X size={20} color="#fff" strokeWidth={2} />
          </Pressable>
        </View>

        <View
          style={{
            position: "absolute",
            bottom: insets.bottom + 28,
            left: 0,
            right: 0,
            paddingHorizontal: 32,
            alignItems: "center",
          }}
        >
          {permissionGranted ? (
            <Text className="text-center text-[14px] text-white" style={{ opacity: 0.85 }}>
              Align a wallet QR code inside the frame
            </Text>
          ) : canAskPermissionAgain ? (
            <>
              <Text className="mb-3 text-center text-[14px] text-white" style={{ opacity: 0.85 }}>
                Camera access is required to scan QR codes
              </Text>
              <Pressable
                onPress={handleGrantPermission}
                className="rounded-full bg-white px-5 py-2.5"
              >
                <Text className="text-[14px] font-semibold text-black">
                  Grant Camera Access
                </Text>
              </Pressable>
            </>
          ) : (
            <Text className="text-center text-[14px] text-white" style={{ opacity: 0.85 }}>
              Enable camera permission in device settings
            </Text>
          )}
          {scanError ? (
            <Text className="mt-3 text-center text-[13px] text-red-400">
              {scanError}
            </Text>
          ) : null}
        </View>
      </View>
    </Modal>
  );
}

const SCANNER_DIM = "rgba(0, 0, 0, 0.65)";
const BRACKET_THICKNESS = 3;
const BRACKET_LENGTH = 26;
const BRACKET_OFFSET = -BRACKET_THICKNESS;

const qrScannerStyles = StyleSheet.create({
  root: { flex: 1, backgroundColor: "#000" },
  flex1: { flex: 1 },
  dim: { backgroundColor: SCANNER_DIM },
  middleRow: { flexDirection: "row" },
  bracket: { position: "absolute", backgroundColor: "#fff", borderRadius: 1.5 },
  bracketTopLeftH: {
    top: BRACKET_OFFSET,
    left: BRACKET_OFFSET,
    width: BRACKET_LENGTH,
    height: BRACKET_THICKNESS,
  },
  bracketTopLeftV: {
    top: BRACKET_OFFSET,
    left: BRACKET_OFFSET,
    width: BRACKET_THICKNESS,
    height: BRACKET_LENGTH,
  },
  bracketTopRightH: {
    top: BRACKET_OFFSET,
    right: BRACKET_OFFSET,
    width: BRACKET_LENGTH,
    height: BRACKET_THICKNESS,
  },
  bracketTopRightV: {
    top: BRACKET_OFFSET,
    right: BRACKET_OFFSET,
    width: BRACKET_THICKNESS,
    height: BRACKET_LENGTH,
  },
  bracketBottomLeftH: {
    bottom: BRACKET_OFFSET,
    left: BRACKET_OFFSET,
    width: BRACKET_LENGTH,
    height: BRACKET_THICKNESS,
  },
  bracketBottomLeftV: {
    bottom: BRACKET_OFFSET,
    left: BRACKET_OFFSET,
    width: BRACKET_THICKNESS,
    height: BRACKET_LENGTH,
  },
  bracketBottomRightH: {
    bottom: BRACKET_OFFSET,
    right: BRACKET_OFFSET,
    width: BRACKET_LENGTH,
    height: BRACKET_THICKNESS,
  },
  bracketBottomRightV: {
    bottom: BRACKET_OFFSET,
    right: BRACKET_OFFSET,
    width: BRACKET_THICKNESS,
    height: BRACKET_LENGTH,
  },
});

function ConfirmRow({
  label,
  value,
  valueWeight = "normal",
  secondary,
}: {
  label: string;
  value: string;
  valueWeight?: "normal" | "medium";
  secondary?: string;
}) {
  return (
    <View
      className="w-full flex-row items-center"
      style={{ paddingHorizontal: 16, overflow: "hidden" }}
    >
      <View className="flex-1" style={{ paddingVertical: 10 }}>
        <Text
          className="text-[14px] font-normal"
          style={{ color: "rgba(60,60,67,0.6)", lineHeight: 18 }}
        >
          {label}
        </Text>
        <View
          className="w-full flex-row items-center"
          style={{ gap: 4 }}
        >
          <Text
            className={`text-[17px] text-black ${
              valueWeight === "medium" ? "font-medium" : "font-normal"
            }`}
            style={{ lineHeight: 22, letterSpacing: -0.187 }}
            numberOfLines={1}
          >
            {value}
          </Text>
          {secondary ? (
            <Text
              className="text-[17px] font-normal"
              style={{
                color: "rgba(60,60,67,0.6)",
                lineHeight: 22,
                letterSpacing: -0.187,
              }}
              numberOfLines={1}
            >
              {secondary}
            </Text>
          ) : null}
        </View>
      </View>
    </View>
  );
}

function ConfirmStep({
  recipientLabel,
  selectedAsset,
  tokenDetailsByMint,
  amountInToken,
  amountInUsd,
  feeSol,
  solPriceUsd,
  isSending,
  onBack,
  onConfirm,
}: {
  recipientLabel: string;
  selectedAsset: SendAsset | null;
  tokenDetailsByMint?: TokenDetailsByMint;
  amountInToken: number;
  amountInUsd: number;
  feeSol: number;
  solPriceUsd: number | null;
  isSending: boolean;
  onBack: () => void;
  onConfirm: () => void;
}) {
  const insets = useSafeAreaInsets();

  const symbol = selectedAsset?.symbol ?? "TOKEN";
  const assetMint = selectedAsset?.mint ?? NATIVE_SOL_MINT;
  const isNativeSol = assetMint === NATIVE_SOL_MINT;
  const assetIcon = resolveTokenIcon({
    mint: assetMint,
    imageUrl: selectedAsset?.imageUrl,
    detailLogoUrl: tokenDetailsByMint?.[assetMint]?.token.logoUrl,
  });

  const formatAssetAmount = (value: number) =>
    isNativeSol ? formatSolAmount(value) : formatTokenAmount(value, 0);

  const feeUsd = solPriceUsd !== null ? feeSol * solPriceUsd : null;
  const totalToken = isNativeSol ? amountInToken + feeSol : amountInToken;
  const totalUsd = isNativeSol
    ? totalToken * (solPriceUsd ?? 0)
    : amountInUsd;

  const confirmDisabled = isSending;

  return (
    <View className="w-full" style={{ flex: 1 }}>
      <View className="w-full" style={{ paddingVertical: 16 }}>
        <View className="flex-row items-center justify-between px-4">
          <Pressable
            className="h-11 w-11 items-center justify-center rounded-full bg-[#f2f2f7]"
            hitSlop={6}
            onPress={onBack}
            accessibilityRole="button"
            accessibilityLabel="Back"
          >
            <ArrowLeft size={24} color="rgba(60,60,67,0.6)" strokeWidth={2} />
          </Pressable>
          <View
            style={{
              position: "absolute",
              left: 0,
              right: 0,
              top: 0,
              bottom: 0,
              alignItems: "center",
              justifyContent: "center",
              pointerEvents: "none",
            }}
          >
            <Text
              className="text-[17px] font-semibold text-black"
              style={{ lineHeight: 22 }}
            >
              Send
            </Text>
            <Text
              className="text-[15px] font-normal"
              style={{ color: "rgba(60,60,67,0.6)", lineHeight: 20, marginTop: 2 }}
            >
              to {recipientLabel}
            </Text>
          </View>
          <View className="h-11 w-11" style={{ opacity: 0 }} />
        </View>
      </View>

      <View
        className="w-full flex-row items-start"
        style={{ padding: 16 }}
      >
        <View style={{ position: "relative" }}>
          <Image
            source={{ uri: assetIcon }}
            style={{ width: 64, height: 64, borderRadius: 32 }}
          />
        </View>
      </View>

      <View style={{ paddingTop: 16, paddingHorizontal: 16 }}>
        <View style={{ paddingBottom: 20, paddingHorizontal: 8 }}>
          <View className="flex-row items-baseline" style={{ gap: 8 }}>
            <Text
              style={{
                fontFamily: "Geist_600SemiBold",
                fontSize: 40,
                lineHeight: 48,
                color: "#000",
              }}
            >
              −{formatAssetAmount(amountInToken)}
            </Text>
            <Text
              style={{
                fontFamily: "Geist_600SemiBold",
                fontSize: 28,
                lineHeight: 32,
                letterSpacing: 0.4,
                color: "rgba(60,60,67,0.4)",
              }}
            >
              {symbol}
            </Text>
          </View>
          {amountInUsd > 0 ? (
            <Text
              className="text-[17px] font-normal"
              style={{
                color: "rgba(60,60,67,0.6)",
                lineHeight: 22,
                marginTop: 4,
              }}
            >
              ≈{formatUsdAmount(amountInUsd)}
            </Text>
          ) : null}
        </View>
      </View>

      <BottomSheetScrollView
        style={{ flex: 1 }}
        contentContainerStyle={{ paddingHorizontal: 16, paddingBottom: 8 }}
        keyboardShouldPersistTaps="handled"
      >
        <View
          className="w-full"
          style={{ backgroundColor: "#f2f2f7", borderRadius: 20, paddingVertical: 4 }}
        >
          <ConfirmRow label="Recipient" value={recipientLabel} />
          <ConfirmRow
            label="Transfer fee"
            value={`${formatSolAmount(feeSol)} SOL`}
            secondary={formatFeeUsd(feeUsd)}
          />
          <ConfirmRow
            label="Total amount"
            value={`${formatAssetAmount(totalToken)} ${symbol}`}
            valueWeight="medium"
            secondary={totalUsd > 0 ? `≈ ${formatUsdAmount(totalUsd)}` : undefined}
          />
        </View>

      </BottomSheetScrollView>

      <View
        style={{
          backgroundColor: "#fff",
          paddingTop: 16,
          paddingBottom: insets.bottom + 12,
          paddingHorizontal: 20,
        }}
      >
        <Pressable
          className="items-center justify-center"
          style={{
            height: 50,
            borderRadius: 78,
            backgroundColor: "#000",
            opacity: confirmDisabled ? 0.4 : 1,
          }}
          onPress={onConfirm}
          disabled={confirmDisabled}
          accessibilityRole="button"
          accessibilityLabel="Confirm send"
        >
          {isSending ? (
            <ActivityIndicator color="#fff" />
          ) : (
            <Text
              className="text-[16px] font-medium text-white"
              style={{ lineHeight: 20 }}
            >
              Confirm
            </Text>
          )}
        </Pressable>
      </View>
    </View>
  );
}

function ResultStep({
  isSending,
  sendError,
  txSignature,
  amountInToken,
  tokenSymbol,
  recipientLabel,
  onClose,
}: {
  isSending: boolean;
  sendError: string | null;
  txSignature: string | null;
  amountInToken: number;
  tokenSymbol: string;
  recipientLabel: string;
  onClose: () => void;
}) {
  const insets = useSafeAreaInsets();

  const status = isSending ? "sending" : sendError ? "error" : "success";

  const explorerUrl = txSignature
    ? solscanExplorerUrl(txSignature)
    : null;

  return (
    <View className="w-full" style={{ flex: 1 }}>
      <View className="w-full" style={{ paddingVertical: 16 }}>
        <View className="flex-row items-center justify-between px-4">
          <Pressable
            className="h-11 w-11 items-center justify-center rounded-full bg-[#f2f2f7]"
            hitSlop={6}
            onPress={onClose}
            accessibilityRole="button"
            accessibilityLabel="Close"
          >
            <X size={24} color="rgba(60,60,67,0.6)" strokeWidth={2} />
          </Pressable>
          <View
            style={{
              position: "absolute",
              left: 0,
              right: 0,
              top: 0,
              bottom: 0,
              alignItems: "center",
              justifyContent: "center",
              pointerEvents: "none",
            }}
          >
            <Text
              className="text-[17px] font-semibold text-black"
              style={{ lineHeight: 22 }}
            >
              Send
            </Text>
            <Text
              className="text-[15px] font-normal"
              style={{ color: "rgba(60,60,67,0.6)", lineHeight: 20, marginTop: 2 }}
            >
              to {recipientLabel}
            </Text>
          </View>
          <View className="h-11 w-11" style={{ opacity: 0 }} />
        </View>
      </View>

      <View
        className="flex-1 items-center justify-center"
        style={{ paddingHorizontal: 32, paddingVertical: 24 }}
      >
        <View className="w-full items-center" style={{ gap: 20 }}>
          {status === "sending" ? (
            <SendingSpinner />
          ) : status === "success" ? (
            <MascotReveal>
              <SendSuccessDog width={100} height={80} />
            </MascotReveal>
          ) : (
            <MascotReveal>
              <SendErrorDog width={100} height={80} />
            </MascotReveal>
          )}

          <View className="w-full items-center" style={{ gap: 4 }}>
            <Text
              className="text-[22px] font-semibold text-black"
              style={{ lineHeight: 28, textAlign: "center" }}
            >
              {status === "sending"
                ? "Sending..."
                : status === "success"
                  ? "Sent"
                  : "Transaction failed"}
            </Text>
            {status === "success" ? (
              <Text
                className="text-[17px] font-normal"
                style={{ lineHeight: 22, textAlign: "center", maxWidth: 300 }}
              >
                <Text className="text-black">
                  {formatTokenAmount(amountInToken)} {tokenSymbol}
                </Text>
                <Text style={{ color: "rgba(60,60,67,0.6)" }}>
                  {" successfully sent to "}
                </Text>
                <Text className="text-black">{recipientLabel}</Text>
              </Text>
            ) : (
              <Text
                className="text-[17px] font-normal"
                style={{
                  color: "rgba(60,60,67,0.6)",
                  lineHeight: 22,
                  textAlign: "center",
                  maxWidth: 300,
                }}
              >
                {status === "sending"
                  ? "You can close this screen and continue using the app"
                  : (sendError ?? "Something went wrong. Please try again.")}
              </Text>
            )}
          </View>
        </View>
      </View>

      <View
        style={{
          backgroundColor: "#fff",
          paddingTop: 16,
          paddingBottom: insets.bottom + 12,
          paddingHorizontal: 20,
          gap: 10,
        }}
      >
        <Pressable
          className="items-center justify-center"
          style={{ height: 50, borderRadius: 78, backgroundColor: "#000" }}
          onPress={onClose}
          accessibilityRole="button"
          accessibilityLabel="Done"
        >
          <Text
            className="text-[16px] font-medium text-white"
            style={{ lineHeight: 20 }}
          >
            Done
          </Text>
        </Pressable>
        {explorerUrl ? (
          <Pressable
            className="items-center justify-center"
            style={{ height: 50, borderRadius: 78, backgroundColor: "#f5f5f5" }}
            onPress={() => Linking.openURL(explorerUrl)}
            accessibilityRole="button"
            accessibilityLabel="View transaction"
          >
            <Text
              className="text-[17px] font-medium text-black"
              style={{ lineHeight: 22 }}
            >
              View transaction
            </Text>
          </Pressable>
        ) : null}
      </View>
    </View>
  );
}
