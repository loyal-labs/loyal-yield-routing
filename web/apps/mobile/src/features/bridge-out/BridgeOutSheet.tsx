import {
  BottomSheetBackdrop,
  type BottomSheetBackdropProps,
  BottomSheetModal,
  BottomSheetView,
} from "@gorhom/bottom-sheet";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { ActivityIndicator, BackHandler, Keyboard } from "react-native";
import Animated, {
  useAnimatedKeyboard,
  useAnimatedStyle,
} from "react-native-reanimated";
import {
  useSafeAreaFrame,
  useSafeAreaInsets,
} from "react-native-safe-area-context";

import { Text, View } from "@/tw";
import {
  useBridgeDestinations,
  useBridgeOut,
  useBridgeRecipients,
  useBridgeUsdcBalance,
} from "@/features/bridge-out/use-bridge-out";

import { type BridgeAmount, BridgeOutForm } from "./BridgeOutForm";
import { BridgeButton, BridgeHeader } from "./BridgeOutParts";
import { BridgeDestinationPicker, BridgeSource } from "./BridgeOutPicker";
import { BridgeOutRecipient } from "./BridgeOutRecipient";
import { BridgeOutStatus } from "./BridgeOutStatus";

type Page =
  | { kind: "form" | "send" | "assets" | "recipient" }
  | { kind: "chains"; asset: string };
type Props = {
  walletAddress: string;
  priceUsd: number | null;
  onClose: (submitted: boolean) => void;
};

export function BridgeOutSheet({
  open,
  walletAddress,
  priceUsd,
  onClose,
}: Omit<Props, "walletAddress"> & {
  open: boolean;
  walletAddress: string | null;
}) {
  if (!open || !walletAddress) return null;
  const price =
    priceUsd !== null && Number.isFinite(priceUsd) && priceUsd > 0
      ? priceUsd
      : null;
  return (
    <BridgeFlow
      key={walletAddress}
      walletAddress={walletAddress}
      priceUsd={price}
      onClose={onClose}
    />
  );
}

function BridgeFlow({ walletAddress, priceUsd, onClose }: Props) {
  const sheet = useRef<BottomSheetModal>(null);
  const { height } = useSafeAreaFrame();
  const { top, bottom } = useSafeAreaInsets();
  // The modal fills its host, including Android's navigation bar.
  const sheetHeight = Math.floor(height - Math.max(height * 0.06, top + 12));
  const snapPoints = useMemo(() => [sheetHeight], [sheetHeight]);
  const keyboard = useAnimatedKeyboard();
  const keyboardPadding = useAnimatedStyle(() => ({
    flex: 1,
    paddingBottom: Math.max(bottom, keyboard.height.value),
  }));
  const [settled, setSettled] = useState(false);
  const [page, setPage] = useState<Page>({ kind: "form" });
  const [selection, setSelection] = useState<{
    chain: string;
    asset: string;
  } | null>(null);
  const [to, setTo] = useState<{ chain: string; address: string } | null>(null);
  const [amount, setAmount] = useState<BridgeAmount>({
    usdc: "",
    dollars: null,
  });

  const catalog = useBridgeDestinations(walletAddress);
  const destinations = (catalog.destinations ?? []).filter(
    (item) => item.tokens?.length,
  );
  const destination =
    destinations.find((item) => item.chain === selection?.chain) ??
    destinations
      .filter((item) => item.popularRank > 0)
      .sort((a, b) => a.popularRank - b.popularRank)[0] ??
    destinations[0];
  const token =
    destination?.tokens?.find((t) => t.asset === selection?.asset) ??
    destination?.tokens?.find((t) => t.symbol === "USDT") ??
    destination?.tokens?.[0] ??
    null;
  const network = destination?.name.replace(/\s*\(.*\)$/, "") ?? "";
  const chain = destination?.chain ?? "";
  const recipient = to?.chain === chain ? to.address : "";
  const balanceRaw = useBridgeUsdcBalance(walletAddress);
  const recents = useBridgeRecipients(
    walletAddress,
    chain,
    page.kind === "recipient",
  );
  const out = useBridgeOut({
    walletAddress,
    chain,
    token,
    recipient,
    amountInput: amount.usdc,
    balanceRaw,
  });
  const { step } = out;
  const result = step.kind === "form" || step.kind === "sending" ? null : step;
  const sending = step.kind === "sending";

  useEffect(() => {
    sheet.current?.present();
  }, []);

  const back = useCallback(() => {
    if (sending) return;
    Keyboard.dismiss();
    if (result || page.kind === "form") sheet.current?.dismiss();
    else setPage({ kind: page.kind === "chains" ? "assets" : "form" });
  }, [sending, result, page.kind]);

  useEffect(() => {
    const listener = BackHandler.addEventListener("hardwareBackPress", () => {
      back();
      return true;
    });
    return () => listener.remove();
  }, [back]);

  const backdrop = useCallback(
    (props: BottomSheetBackdropProps) => (
      <BottomSheetBackdrop
        {...props}
        appearsOnIndex={0}
        disappearsOnIndex={-1}
        opacity={0.4}
        pressBehavior={sending ? "none" : "close"}
      />
    ),
    [sending],
  );
  const openPage = (kind: "form" | "assets" | "send" | "recipient") => {
    Keyboard.dismiss();
    setPage({ kind });
  };
  const showRecipient =
    !result && page.kind === "recipient" && !!destination && !!token;
  const title =
    result || page.kind === "form"
      ? "Bridge"
      : page.kind === "send"
      ? "You send"
      : page.kind === "chains"
      ? "Select chain"
      : "You receive";
  return (
    <BottomSheetModal
      ref={sheet}
      snapPoints={snapPoints}
      enableDynamicSizing={false}
      enablePanDownToClose={!sending}
      backdropComponent={backdrop}
      onChange={(index) => setSettled(index >= 0)}
      onDismiss={() =>
        onClose(step.kind === "bridging" || step.kind === "filled")
      }
      handleComponent={null}
      keyboardBehavior="extend"
      keyboardBlurBehavior="restore"
      android_keyboardInputMode="adjustResize"
      backgroundStyle={{
        backgroundColor: "#FFF",
        borderTopLeftRadius: 38,
        borderTopRightRadius: 38,
      }}
    >
      <BottomSheetView
        style={{
          height: sheetHeight,
          backgroundColor: "#FFF",
          borderTopLeftRadius: 38,
          borderTopRightRadius: 38,
          overflow: "hidden",
        }}
      >
        <Animated.View onAccessibilityEscape={back} style={keyboardPadding}>
          {!showRecipient && (
            <BridgeHeader
              title={title}
              close={result ? result.kind !== "bridging" : page.kind === "form"}
              disabled={sending}
              onBack={back}
            />
          )}
          {result ? (
            <BridgeOutStatus step={result} onDone={back} />
          ) : !destination || !token ? (
            <View className="items-center gap-[12px] px-[16px] py-[24px]">
              {!catalog.error && catalog.destinations === null && (
                <ActivityIndicator color="#8A8A8E" />
              )}
              <Text
                accessibilityLiveRegion="polite"
                className="text-center text-[15px] leading-[20px] text-[#8A8A8E]"
              >
                {catalog.error ??
                  (catalog.destinations === null
                    ? "Loading chains…"
                    : "No destinations are available.")}
              </Text>
              {(catalog.error || catalog.destinations !== null) && (
                <BridgeButton
                  label="Try again"
                  tone="secondary"
                  onPress={catalog.retry}
                />
              )}
            </View>
          ) : page.kind === "form" ? (
            <BridgeOutForm
              destination={destination}
              token={token}
              network={network}
              balanceRaw={balanceRaw}
              priceUsd={priceUsd}
              amount={amount}
              setAmount={setAmount}
              settled={settled}
              recipient={recipient}
              clearRecipient={() => setTo(null)}
              openPage={openPage}
              out={out}
            />
          ) : page.kind === "send" ? (
            <BridgeSource
              balanceRaw={balanceRaw}
              priceUsd={priceUsd}
              onSelect={() => openPage("form")}
            />
          ) : page.kind === "recipient" ? (
            <BridgeOutRecipient
              key={chain}
              addressKind={destination.addressKind}
              network={network}
              value={recipient}
              rejected={out.quoteError?.code === "invalid_recipient"}
              recents={recents}
              onBack={back}
              onSelect={(address) => {
                setTo({ chain, address });
                openPage("form");
              }}
            />
          ) : (
            <BridgeDestinationPicker
              key={page.kind}
              destinations={destinations}
              asset={page.kind === "chains" ? page.asset : null}
              onSelect={(id) => {
                if (page.kind === "chains") {
                  if (id !== chain) setTo(null);
                  setSelection({ chain: id, asset: page.asset });
                  openPage("form");
                } else {
                  setPage({ kind: "chains", asset: id });
                }
              }}
            />
          )}
        </Animated.View>
      </BottomSheetView>
    </BottomSheetModal>
  );
}
