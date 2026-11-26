import {
  BottomSheetModal,
  BottomSheetScrollView,
  BottomSheetView,
} from "@gorhom/bottom-sheet";
import * as Haptics from "expo-haptics";
import { useCallback, useEffect, useRef, useState } from "react";
import { Alert, BackHandler, Keyboard, Share } from "react-native";
import { useSafeAreaInsets } from "react-native-safe-area-context";

import {
  presentSheetWhenOpen,
  SheetBackdrop,
} from "@/components/sheets/sheetFacts";
import {
  DepositPicker,
  type DepositSelection,
  OtherChains,
} from "@/features/receive/OtherChains";
import {
  ReceiveAddressCard,
  ReceiveButton,
  useCopyAddress,
} from "@/features/receive/ReceiveAddress";
import { useReceiveBridge } from "@/features/receive/useReceiveBridge";
import { useFixedSheetLayout } from "@/hooks/useFixedSheetLayout";
import { Pressable, Text, View } from "@/tw";

import Back from "../../../assets/images/receive/back.svg";
import Close from "../../../assets/images/receive/close.svg";

export type ReceiveTab = "Solana" | "Other chains";
type Page = "receive" | "assets" | "chains";
type ReceiveSheetProps = {
  open: boolean;
  onClose: () => void;
  walletAddress: string | null;
};
type Bridge = ReturnType<typeof useReceiveBridge>;

export function useReceiveSheetBridge(
  walletAddress: string | null,
  tab: ReceiveTab | null,
) {
  const otherChainsOpen = tab === "Other chains";
  const bridge = useReceiveBridge(walletAddress, otherChainsOpen);
  const { launched, load } = bridge;
  useEffect(() => {
    if (otherChainsOpen && launched) void load();
  }, [otherChainsOpen, launched, load]);
  return bridge;
}

export function ReceiveSheet(props: ReceiveSheetProps) {
  const wallet = props.open ? props.walletAddress : null;
  const [otherChainsWallet, setOtherChainsWallet] = useState<string | null>(
    null,
  );
  useEffect(() => setOtherChainsWallet(null), [wallet]);
  const tab =
    wallet && otherChainsWallet === wallet ? "Other chains" : "Solana";
  const bridge = useReceiveSheetBridge(wallet, props.open ? tab : null);
  return (
    <ReceiveSheetModal
      {...props}
      bridge={bridge}
      tab={tab}
      onTabChange={(next) =>
        setOtherChainsWallet(next === "Other chains" ? wallet : null)
      }
    />
  );
}

export function ReceiveSheetModal({
  open,
  onClose,
  walletAddress,
  tab,
  onTabChange,
  bridge,
}: ReceiveSheetProps & {
  tab: ReceiveTab;
  onTabChange: (tab: ReceiveTab) => void;
  bridge: Bridge;
}) {
  const { sheetHeight, snapPoints } = useFixedSheetLayout();
  const ref = useRef<BottomSheetModal>(null);
  useEffect(() => presentSheetWhenOpen(ref.current, open), [open]);

  return (
    <BottomSheetModal
      ref={ref}
      snapPoints={snapPoints}
      enableDynamicSizing={false}
      enablePanDownToClose
      backdropComponent={SheetBackdrop}
      onDismiss={onClose}
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
          overflow: "hidden",
          borderTopLeftRadius: 38,
          borderTopRightRadius: 38,
        }}
      >
        {open && (
          <ReceiveContent
            key={walletAddress}
            walletAddress={walletAddress}
            tab={tab}
            onTabChange={onTabChange}
            bridge={bridge}
            dismiss={onClose}
          />
        )}
      </BottomSheetView>
    </BottomSheetModal>
  );
}

function ReceiveContent({
  walletAddress,
  tab,
  onTabChange,
  bridge,
  dismiss,
}: {
  walletAddress: string | null;
  tab: ReceiveTab;
  onTabChange: (tab: ReceiveTab) => void;
  bridge: Bridge;
  dismiss: () => void;
}) {
  const insets = useSafeAreaInsets();
  const [page, setPage] = useState<Page>("receive");
  const [selection, setSelection] = useState<DepositSelection | null>(null);
  const { launched } = bridge;

  const back = useCallback(() => {
    if (page === "receive") {
      dismiss();
    } else {
      Keyboard.dismiss();
      setPage(page === "chains" && !selection?.chain ? "assets" : "receive");
    }
  }, [dismiss, page, selection?.chain]);

  useEffect(() => {
    const listener = BackHandler.addEventListener("hardwareBackPress", () => {
      back();
      return true;
    });
    return () => listener.remove();
  }, [back]);

  const title =
    page === "assets"
      ? "Select asset to deposit"
      : page === "chains"
      ? "Select chain"
      : "Receive";

  return (
    <View className="flex-1 bg-white" onAccessibilityEscape={back}>
      <View className="flex-row items-center justify-between px-4 py-4">
        <Pressable
          accessibilityRole="button"
          accessibilityLabel={page === "receive" ? "Close Receive" : "Back"}
          onPress={back}
          hitSlop={4}
          className="h-11 w-11 items-center justify-center rounded-full bg-[#F2F2F7] active:opacity-60"
        >
          {page === "receive" ? (
            <Close width={28} height={28} />
          ) : (
            <Back width={28} height={28} />
          )}
        </Pressable>
        <Text
          accessibilityRole="header"
          className="flex-1 text-center text-[17px] leading-[22px] font-semibold text-black"
        >
          {title}
        </Text>
        <View className="w-11" />
      </View>
      {page === "receive" ? (
        <>
          <View className="mx-4 mb-2 flex-row rounded-full bg-[#F2F2F7] p-1">
            {(["Solana", "Other chains"] as const).map((label) => {
              const disabled = label === "Other chains" && !launched;
              return (
                <Pressable
                  key={label}
                  accessibilityRole="tab"
                  accessibilityLabel={label}
                  accessibilityState={{ selected: tab === label, disabled }}
                  disabled={disabled}
                  onPress={() => onTabChange(label)}
                  className={`flex-1 flex-row items-center justify-center gap-[6px] rounded-full py-2 ${
                    tab === label ? "bg-white" : ""
                  }`}
                  style={{ opacity: disabled ? 0.45 : 1 }}
                >
                  <Text
                    className={`text-[15px] leading-5 font-medium ${
                      tab === label ? "text-black" : "text-[#8A8A8E]"
                    }`}
                  >
                    {label}
                  </Text>
                  {label === "Other chains" && launched && (
                    <View className="rounded-full bg-[#9946FC] px-[6px] py-px">
                      <Text className="text-[11px] leading-[14px] font-medium text-white">
                        NEW
                      </Text>
                    </View>
                  )}
                </Pressable>
              );
            })}
          </View>
          {tab === "Solana" ? (
            <SolanaReceive
              walletAddress={walletAddress}
              bottomInset={insets.bottom}
            />
          ) : launched ? (
            <OtherChains
              bridge={bridge}
              selection={selection}
              bottomInset={insets.bottom}
              onAssets={() => setPage("assets")}
              onChains={() => setPage("chains")}
            />
          ) : (
            <Text
              accessibilityLiveRegion="polite"
              className="px-4 py-4 text-center text-[#8A8A8E]"
            >
              Other chains are unavailable for this wallet.
            </Text>
          )}
        </>
      ) : (
        <DepositPicker
          key={page}
          bridge={bridge}
          bottomInset={insets.bottom}
          chainAsset={page === "chains" ? selection?.asset ?? null : null}
          onAsset={(asset) => {
            setSelection({ asset, chain: null });
            setPage("chains");
          }}
          onNetwork={(network) => {
            if (!selection) return;
            setSelection({ asset: selection.asset, chain: network.chain });
            setPage("receive");
          }}
        />
      )}
    </View>
  );
}

function SolanaReceive({
  walletAddress,
  bottomInset,
}: {
  walletAddress: string | null;
  bottomInset: number;
}) {
  const { copy, copied } = useCopyAddress(walletAddress);
  async function share() {
    if (!walletAddress) return;
    void Haptics.impactAsync(Haptics.ImpactFeedbackStyle.Light);
    try {
      await Share.share({
        message: `My Solana wallet address:\n${walletAddress}`,
      });
    } catch {
      Alert.alert("Could not share address", "Please try again.");
    }
  }

  return (
    <>
      <BottomSheetScrollView
        style={{ flex: 1 }}
        contentContainerStyle={{ flexGrow: 1 }}
      >
        <View className="min-h-[310px] flex-1 items-center justify-center p-4">
          {walletAddress ? (
            <ReceiveAddressCard address={walletAddress} />
          ) : (
            <Text className="text-[#8A8A8E]">No address</Text>
          )}
        </View>
        <Text
          className="self-center px-8 pb-3 text-center text-[15px] leading-5 text-[#8A8A8E]"
          style={{ maxWidth: 382 }}
        >
          Use to receive assets on the Solana network only. Other assets will be
          lost forever.
        </Text>
      </BottomSheetScrollView>
      <View
        className="flex-row gap-2 px-4 pt-2"
        style={{ paddingBottom: bottomInset + 16 }}
      >
        <ReceiveButton
          label={copied ? "Copied!" : "Copy"}
          onPress={copy}
          disabled={!walletAddress}
        />
        <ReceiveButton
          label="Share"
          onPress={share}
          disabled={!walletAddress}
        />
      </View>
    </>
  );
}
