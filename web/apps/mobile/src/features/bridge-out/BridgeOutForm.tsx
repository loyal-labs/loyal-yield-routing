import {
  BottomSheetScrollView,
  BottomSheetTextInput,
} from "@gorhom/bottom-sheet";
import {
  type BridgeDestination,
  type BridgeDestinationToken,
  type BridgeOutQuoteResponse,
} from "@loyal-labs/api";
import {
  type ComponentProps,
  useEffect,
  useRef,
  useState,
} from "react";
import { Keyboard } from "react-native";
import type { TextInput } from "react-native-gesture-handler";

import {
  formatAmountInputDisplay,
  formatUsdAmount,
  SHEET_SETTLE_FOCUS_DELAY_MS,
} from "@/components/sheets/sheetFacts";
import { useKeyboardRescueFocus } from "@/hooks/useKeyboardRescueFocus";
import { formatSenderAddress } from "@/lib/solana/wallet/formatters";
import { Pressable, Text, View } from "@/tw";

import Clear from "../../../assets/images/bridge-out/clear.svg";
import Currency from "../../../assets/images/bridge-out/currency.svg";
import Down from "../../../assets/images/bridge-out/down.svg";
import PillChevron from "../../../assets/images/bridge-out/pill-chevron.svg";
import Chevron from "../../../assets/images/receive/chevron.svg";
import {
  bridgeAmountLabel,
  BridgeButton,
  BridgeCoin,
  WalletGlyph,
} from "./BridgeOutParts";
import type { useBridgeOut } from "./use-bridge-out";
import { usdcForDollars } from "./amount";

export type BridgeAmount = { usdc: string; dollars: string | null };

type FormProps = {
  destination: BridgeDestination;
  token: BridgeDestinationToken;
  network: string;
  balanceRaw: bigint | null;
  priceUsd: number | null;
  amount: BridgeAmount;
  setAmount: (amount: BridgeAmount) => void;
  settled: boolean;
  recipient: string;
  clearRecipient: () => void;
  openPage: (page: "send" | "assets" | "recipient") => void;
  out: ReturnType<typeof useBridgeOut>;
};

function TokenPill({
  symbol,
  network,
  file,
  badge,
  label,
  disabled,
  onPress,
}: {
  symbol: string;
  network: string;
  file: string;
  badge: string;
  label: string;
  disabled: boolean;
  onPress: () => void;
}) {
  return (
    <Pressable
      accessibilityRole="button"
      accessibilityLabel={label}
      accessibilityHint={`${symbol} on ${network}`}
      accessibilityState={{ disabled }}
      disabled={disabled}
      onPress={onPress}
      className="max-w-[56%] flex-row items-center rounded-full bg-black/[0.04] py-[4px] pl-[4px] pr-[10px] active:opacity-60"
    >
      <BridgeCoin file={file} badge={badge} size={36} />
      <View className="ml-[8px] shrink">
        <Text
          numberOfLines={1}
          className="text-[15px] leading-[20px] font-medium text-black"
        >
          {symbol}
        </Text>
        <Text
          numberOfLines={1}
          className="text-[13px] leading-[16px] text-[#8A8A8E]"
        >
          {network}
        </Text>
      </View>
      <PillChevron />
    </Pressable>
  );
}

function QuoteDetails({
  quote,
  token,
}: {
  quote: BridgeOutQuoteResponse;
  token: BridgeDestinationToken;
}) {
  const fee = quote.networkFeeUsd ? Number(quote.networkFeeUsd) : null;
  const rows = [
    ["Rate", quote.rate ? `1 USDC ≈ ${quote.rate} ${token.symbol}` : "—"],
    [
      "Minimum received",
      `${bridgeAmountLabel(quote.amountOutMinRaw, token.decimals)} ${
        token.symbol
      }`,
    ],
    ["Max slippage", `${quote.slippageBps / 100}%`],
    ["Loyal fee", `${quote.loyalFeeBps / 100}%`],
    [
      "Network fee",
      fee === null ? "—" : fee < 0.01 ? "<$0.01" : formatUsdAmount(fee),
    ],
  ];
  return (
    <View className="mx-[16px] mt-[8px] rounded-2xl bg-[#F5F5F5] px-[16px] py-[4px]">
      {rows.map(([name, value]) => (
        <View
          key={name}
          className="flex-row items-center justify-between gap-[8px] py-[8px]"
        >
          <Text className="shrink text-[13px] leading-[16px] text-[#8A8A8E]">
            {name}
          </Text>
          <Text className="shrink text-right text-[13px] leading-[16px] text-black">
            {value}
          </Text>
        </View>
      ))}
    </View>
  );
}

export function BridgeOutForm({
  destination,
  token,
  network,
  balanceRaw,
  priceUsd,
  amount,
  setAmount,
  recipient,
  clearRecipient,
  openPage,
  settled,
  out,
}: FormProps) {
  const field = useRef<TextInput>(null);
  const { openKeyboard } = useKeyboardRescueFocus(field);
  const [focused, setFocused] = useState(false);
  const focusOnMount = useRef(!amount.usdc);
  useEffect(() => {
    if (!settled || !focusOnMount.current) return;
    const timer = setTimeout(
      () => field.current?.focus(),
      SHEET_SETTLE_FOCUS_DELAY_MS,
    );
    return () => clearTimeout(timer);
  }, [settled]);

  const sending = out.step.kind === "sending";
  const hasAmount = out.amountRaw > BigInt(0);
  const rejected = out.quoteError?.code === "invalid_recipient";
  const inDollars = amount.dollars !== null;
  const entered = amount.dollars ?? amount.usdc;
  const usdcAmount = Number(amount.usdc) || 0;
  const balance = balanceRaw === null ? null : bridgeAmountLabel(balanceRaw, 6);
  const error = rejected
    ? `Invalid ${network} address`
    : out.quoteError?.message;
  const quote = hasAmount && recipient && !out.insufficient ? out.quote : null;
  const estimate = out.estimate;

  function receiveDisplay(): { amount: string; detail?: string } {
    if (!hasAmount) return { amount: "0", detail: "$0" };
    if (out.belowMinimum)
      return {
        amount: "Below minimum",
        detail: `Minimum ${token.minUsdc} USDC for ${token.symbol} on ${network}.`,
      };
    if (out.insufficient)
      return { amount: "Unavailable", detail: "Amount exceeds your USDC balance." };
    const priced = quote ?? (estimate?.kind === "priced" ? estimate : null);
    if (priced) {
      const value = bridgeAmountLabel(priced.amountOutRaw, token.decimals);
      const usd =
        priced.amountOutUsd && Number.isFinite(Number(priced.amountOutUsd))
          ? formatUsdAmount(Number(priced.amountOutUsd))
          : null;
      const amount = value === "0" ? "<0.000001" : value;
      if (quote) return { amount, detail: usd ? `≈${usd}` : undefined };
      return { amount, detail: usd ? `Estimated · ${usd}` : "Estimated" };
    }
    if (estimate && estimate.kind !== "priced")
      return {
        amount: estimate.kind === "below-minimum" ? "Below minimum" : "Unavailable",
        detail: estimate.reason,
      };
    if (balanceRaw === null) return { amount: "Checking balance…" };
    if (recipient && out.quoteError) return { amount: "Unavailable" };
    return { amount: recipient ? "Getting quote…" : "Estimating…" };
  }
  const receive = receiveDisplay();

  function changeAmount(input: string) {
    const value = input.replace(",", ".");
    if (!/^\d{0,14}(\.\d{0,6})?$/.test(value)) return;
    const usdc = inDollars ? usdcForDollars(value, priceUsd) : value;
    setAmount({ usdc, dollars: inDollars ? value : null });
  }
  function toggleCurrency() {
    if (inDollars) setAmount({ ...amount, dollars: null });
    else if (priceUsd)
      setAmount({
        ...amount,
        dollars: amount.usdc ? (usdcAmount * priceUsd).toFixed(2) : "",
      });
  }
  function setMax() {
    if (balance === null) return;
    const usdc = balance.replace(/,/g, "");
    setAmount({
      usdc,
      dollars:
        inDollars && priceUsd ? (Number(usdc) * priceUsd).toFixed(2) : null,
    });
  }

  function primaryAction(): ComponentProps<typeof BridgeButton> {
    if (sending) return { label: "Sending…" };
    if (!hasAmount) return { label: "Enter amount", onPress: openKeyboard };
    if (balanceRaw === null) return { label: "Loading balance…" };
    if (out.insufficient)
      return { label: "Insufficient balance", onPress: openKeyboard };
    if (out.belowMinimum || estimate?.kind === "below-minimum")
      return { label: "Change amount", onPress: openKeyboard };
    if (!recipient || rejected)
      return { label: "Enter recipient", onPress: () => openPage("recipient") };
    if (out.quoteError?.retryable)
      return { label: "Try again", onPress: out.retry, tone: "primary" };
    if (out.quoteError)
      return { label: "Change amount", onPress: openKeyboard };
    if (!out.ready || !quote)
      return { label: quote ? "Checking transfer…" : "Getting quote…" };
    return {
      label: "Send now",
      tone: "primary",
      onPress: () => {
        Keyboard.dismiss();
        void out.send();
      },
    };
  }

  return (
    <>
      <BottomSheetScrollView
        style={{ flex: 1 }}
        keyboardShouldPersistTaps="handled"
        contentContainerStyle={{ flexGrow: 1 }}
      >
        <View className="px-[16px]">
          <View className="gap-[2px] py-[8px]">
            <Text className="text-[15px] leading-[20px] text-[#8A8A8E]">
              You send
            </Text>
            <View className="h-[48px] flex-row items-center gap-[8px]">
              <Pressable
                accessible={false}
                disabled={sending}
                onPress={openKeyboard}
                className="h-[48px] min-w-0 flex-1 flex-row items-center"
              >
                {inDollars && (
                  <Text className="text-[32px] leading-[38px] font-semibold text-black">
                    $
                  </Text>
                )}
                <BottomSheetTextInput
                  ref={field}
                  accessibilityLabel="You send amount"
                  editable={!sending}
                  pointerEvents="none"
                  placeholder="0"
                  placeholderTextColor="#000"
                  keyboardType="decimal-pad"
                  inputMode="decimal"
                  returnKeyType="done"
                  onSubmitEditing={Keyboard.dismiss}
                  onFocus={() => setFocused(true)}
                  onBlur={() => setFocused(false)}
                  value={focused ? entered : formatAmountInputDisplay(entered)}
                  onChangeText={changeAmount}
                  style={{
                    flex: 1,
                    minWidth: 0,
                    padding: 0,
                    fontSize: 32,
                    fontFamily: "Geist_600SemiBold",
                    color: "#000",
                    includeFontPadding: false,
                  }}
                />
              </Pressable>
              <TokenPill
                symbol="USDC"
                network="Solana"
                file="usdc.png"
                badge="sol.png"
                label="Select asset to send"
                disabled={sending}
                onPress={() => openPage("send")}
              />
            </View>
            <View className="flex-row items-center justify-between gap-[8px]">
              <Pressable
                accessibilityRole="button"
                accessibilityLabel="Switch amount currency"
                hitSlop={6}
                disabled={sending || (!priceUsd && !inDollars)}
                onPress={toggleCurrency}
                className="shrink flex-row items-center gap-[4px]"
              >
                <Text
                  numberOfLines={1}
                  className="shrink text-[15px] leading-[20px] text-[#8A8A8E]"
                >
                  {inDollars
                    ? `${bridgeAmountLabel(out.amountRaw, 6)} USDC`
                    : !hasAmount || priceUsd
                    ? formatUsdAmount(usdcAmount * (priceUsd ?? 0))
                    : "—"}
                </Text>
                <Currency />
              </Pressable>
              <Pressable
                accessibilityRole="button"
                accessibilityLabel="Use maximum balance"
                hitSlop={6}
                disabled={sending || balance === null}
                onPress={setMax}
                className="shrink flex-row items-center gap-[4px]"
              >
                <Text className="text-[15px] leading-[20px] text-black">
                  MAX
                </Text>
                <Text
                  numberOfLines={1}
                  className="shrink text-[15px] leading-[20px] text-[#8A8A8E]"
                >
                  {balance ?? "—"}
                </Text>
              </Pressable>
            </View>
          </View>
          <View className="h-[28px] items-center justify-center">
            <View className="absolute right-0 left-0 h-px bg-black/[0.08]" />
            <View className="h-[28px] w-[28px] items-center justify-center rounded-full bg-[#F5F5F5]">
              <Down />
            </View>
          </View>
          <View className="gap-[2px] py-[8px]">
            <Text className="text-[15px] leading-[20px] text-[#8A8A8E]">
              You receive
            </Text>
            <View className="h-[48px] flex-row items-center gap-[8px]">
              <Text
                accessibilityLabel="You receive amount"
                accessibilityValue={{ text: receive.amount }}
                numberOfLines={1}
                adjustsFontSizeToFit
                className="min-w-0 flex-1 text-[32px] leading-[38px] font-semibold text-black"
              >
                {receive.amount}
              </Text>
              <TokenPill
                symbol={token.symbol}
                network={network}
                file={token.icon}
                badge={destination.icon}
                label="Select asset to receive"
                disabled={sending}
                onPress={() => openPage("assets")}
              />
            </View>
            <Text
              accessibilityLiveRegion="polite"
              className="min-h-[20px] text-[15px] leading-[20px] text-[#8A8A8E]"
            >
              {receive.detail}
            </Text>
          </View>
        </View>

        <View className="min-h-[16px] grow" />
        <View className="min-h-[60px] flex-row items-center px-[16px]">
          <Pressable
            accessibilityRole="button"
            accessibilityLabel={
              recipient
                ? `Edit ${network} recipient, ${recipient}`
                : `Enter ${network} address`
            }
            disabled={sending}
            onPress={() => openPage("recipient")}
            className="flex-1 flex-row items-center gap-[12px] py-[6px] active:opacity-60"
          >
            <WalletGlyph />
            <View className="flex-1">
              <Text className="text-[15px] leading-[20px] text-[#8A8A8E]">
                {network} recipient
              </Text>
              <Text
                numberOfLines={1}
                className={`text-[20px] leading-[24px] font-medium ${
                  recipient ? "text-black" : "text-[#AFADB3]"
                }`}
              >
                {recipient
                  ? formatSenderAddress(recipient)
                  : `Enter ${network} address`}
              </Text>
            </View>
            {!recipient && <Chevron />}
          </Pressable>
          {!!recipient && (
            <Pressable
              accessibilityRole="button"
              accessibilityLabel="Clear recipient"
              disabled={sending}
              onPress={clearRecipient}
              className="h-[44px] w-[44px] items-center justify-center"
            >
              <Clear />
            </Pressable>
          )}
        </View>
        {quote && <QuoteDetails quote={quote} token={token} />}
      </BottomSheetScrollView>
      <View className="gap-[8px] px-[20px] pt-[16px] pb-[16px]">
        {!!(error || out.notice) && (
          <Text
            accessibilityLiveRegion="polite"
            className={`text-center text-[13px] leading-[16px] ${
              error ? "text-[#CF2129]" : "text-[#8A8A8E]"
            }`}
          >
            {error || out.notice}
          </Text>
        )}
        <BridgeButton tone="attention" {...primaryAction()} />
      </View>
    </>
  );
}
