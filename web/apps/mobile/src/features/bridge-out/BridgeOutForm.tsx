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
import Animated, {
  Easing,
  FadeIn,
  useReducedMotion,
} from "react-native-reanimated";

import { Skeleton } from "@/components/Skeleton";
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
import Warning from "../../../assets/images/receive/warning.svg";
import {
  bridgeAmountLabel,
  BridgeButton,
  BridgeCoin,
  WalletGlyph,
} from "./BridgeOutParts";
import type { useBridgeOut } from "./use-bridge-out";
import { usdcForDollars } from "./amount";

export type BridgeAmount = { usdc: string; dollars: string | null };

const receiveFade = FadeIn.duration(150).easing(Easing.bezier(0.22, 1, 0.36, 1));
const amountSkeleton = {
  width: 92,
  maxWidth: "100%",
  height: 18,
  borderRadius: 9,
  backgroundColor: "#E5E5EA",
} as const;

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
  const reducedMotion = useReducedMotion();
  const PendingAmount = reducedMotion ? View : Skeleton;
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
  const quote = hasAmount && recipient && !out.insufficient ? out.quote : null;
  const estimate = out.estimate;

  function receiveProblem() {
    if (out.insufficient) return "Insufficient USDC balance.";
    if (out.belowMinimum)
      return `Minimum ${token.minUsdc} USDC on ${network}`;
    if (estimate && estimate.kind !== "priced") return estimate.reason;
    if (rejected) return `Invalid ${network} address`;
    return out.quoteError?.message;
  }
  function receiveDisplay(): { amount: string | null; detail?: string } {
    if (!hasAmount) return { amount: "0", detail: "$0" };
    const priced = quote ?? (estimate?.kind === "priced" ? estimate : null);
    if (!priced) return { amount: null };
    const value = bridgeAmountLabel(priced.amountOutRaw, token.decimals);
    const usd =
      priced.amountOutUsd && Number.isFinite(Number(priced.amountOutUsd))
        ? formatUsdAmount(Number(priced.amountOutUsd))
        : null;
    const amount = value === "0" ? "<0.000001" : value;
    if (quote) return { amount, detail: usd ? `≈${usd}` : undefined };
    return { amount, detail: usd ? `Estimated · ${usd}` : "Estimated" };
  }
  const receive = receiveDisplay();
  const problem = receiveProblem();
  const pending = receive.amount === null && !problem;
  const pendingLabel =
    balanceRaw === null ? "Checking balance…" : recipient ? "Getting quote…" : "Estimating…";

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
    if (sending) return { label: "Sending…", tone: "secondary" };
    if (!hasAmount) return { label: "Enter amount", onPress: openKeyboard };
    if (
      out.insufficient ||
      out.belowMinimum ||
      estimate?.kind === "below-minimum"
    )
      return { label: "Change amount", onPress: openKeyboard };
    if (balanceRaw === null)
      return { label: recipient ? "Get quote" : "Enter recipient", tone: "secondary" };
    if (!recipient || rejected)
      return {
        label: rejected ? "Edit recipient" : "Enter recipient",
        onPress: () => openPage("recipient"),
      };
    if (out.quoteError?.retryable)
      return { label: "Try again", onPress: out.retry, tone: "primary" };
    if (out.quoteError)
      return { label: "Change amount", onPress: openKeyboard };
    if (!out.ready || !quote)
      return { label: quote ? "Checking transfer…" : "Get quote", tone: "secondary" };
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
              <View
                accessible
                accessibilityRole="text"
                accessibilityLabel="You receive amount"
                accessibilityLiveRegion="polite"
                accessibilityState={{ busy: pending }}
                accessibilityValue={{
                  text: receive.amount !== null
                    ? `${receive.amount} ${token.symbol}`
                    : pending ? pendingLabel : "Unavailable",
                }}
                className="min-w-0 flex-1 justify-center"
              >
                {pending ? (
                  <PendingAmount style={amountSkeleton} />
                ) : (
                  <Animated.View
                    key={receive.amount}
                    entering={!reducedMotion && hasAmount ? receiveFade : undefined}
                  >
                    <Text
                      numberOfLines={1}
                      adjustsFontSizeToFit
                      className={`text-[32px] leading-[38px] font-semibold ${receive.amount === null ? "text-[#8A8A8E]" : "text-black"}`}
                    >
                      {receive.amount ?? "—"}
                    </Text>
                  </Animated.View>
                )}
              </View>
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
            {(receive.detail || !problem) && (
              <Text className="min-h-[20px] text-[15px] leading-[20px] text-[#8A8A8E]">
                {receive.detail}
              </Text>
            )}
            {!!problem && (
              <Animated.View
                key={problem}
                entering={reducedMotion ? undefined : receiveFade}
              >
                <View className="min-h-[20px] flex-row items-center gap-[6px]">
                  <Warning width={16} height={16} accessible={false} />
                  <Text
                    accessibilityRole="alert"
                    accessibilityLiveRegion="polite"
                    className="shrink text-[13px] leading-[18px] text-[#CF2129]"
                  >
                    {problem}
                  </Text>
                </View>
              </Animated.View>
            )}
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
        {!!out.notice && !out.quoteError && (
          <Text
            accessibilityLiveRegion="polite"
            className="text-center text-[13px] leading-[16px] text-[#8A8A8E]"
          >
            {out.notice}
          </Text>
        )}
        <BridgeButton tone="attention" {...primaryAction()} />
      </View>
    </>
  );
}
