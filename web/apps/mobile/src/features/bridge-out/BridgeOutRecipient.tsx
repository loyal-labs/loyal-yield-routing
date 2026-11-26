import {
  BottomSheetScrollView,
  BottomSheetTextInput,
} from "@gorhom/bottom-sheet";
import type { BridgeDestination, BridgeRecipient } from "@loyal-labs/api";
import { useCameraPermissions } from "expo-camera";
import * as Clipboard from "expo-clipboard";
import { type ComponentRef, useEffect, useRef, useState } from "react";
import { Keyboard } from "react-native";

import { QrScannerModal } from "@/components/wallet/QrScannerModal";
import { formatSenderAddress } from "@/lib/solana/wallet/formatters";
import { Pressable, Text, View } from "@/tw";

import Clear from "../../../assets/images/bridge-out/clear.svg";
import { BridgeHeader, BridgeRow, WalletGlyph } from "./BridgeOutParts";
import { bridgeRecipientState } from "./recipient-address";

export function BridgeOutRecipient({
  addressKind,
  network,
  value,
  rejected,
  recents,
  onBack,
  onSelect,
}: {
  addressKind: BridgeDestination["addressKind"];
  network: string;
  value: string;
  rejected: boolean;
  recents: BridgeRecipient[] | null;
  onBack: () => void;
  onSelect: (address: string) => void;
}) {
  const [input, setInput] = useState(value);
  const [submitted, setSubmitted] = useState(false);
  const [pasteError, setPasteError] = useState(false);
  const [scanning, setScanning] = useState(false);
  const [scanError, setScanError] = useState<string | null>(null);
  const [permission, requestPermission] = useCameraPermissions();
  const field = useRef<ComponentRef<typeof BottomSheetTextInput>>(null);
  useEffect(() => {
    if (scanning) return;
    const timer = setTimeout(() => field.current?.focus(), 120);
    return () => clearTimeout(timer);
  }, [scanning]);

  const address = input.trim();
  const tron = addressKind === "tron";
  const state =
    rejected && address === value
      ? "invalid"
      : bridgeRecipientState(addressKind, address, submitted);
  const valid = state === "valid";
  const invalid = state === "invalid";
  const recent = (recents ?? []).filter(
    (entry) =>
      !(rejected && value === entry.address) &&
      bridgeRecipientState(addressKind, entry.address) === "valid",
  );
  const match =
    valid &&
    recent.find((entry) =>
      tron
        ? entry.address === address
        : entry.address.toLowerCase() === address.toLowerCase(),
    );
  const showRecents = !address || invalid;
  const rows = valid
    ? [{ address, sends: match ? match.sends : 0 }]
    : showRecents
    ? recent
    : [];
  const select = (picked: string) => {
    Keyboard.dismiss();
    onSelect(picked);
  };
  function change(text: string, complete = false) {
    setInput(
      text
        .replace(/[\r\n]/g, "")
        .trim()
        .slice(0, 128),
    );
    setSubmitted(complete);
    setPasteError(false);
  }
  async function paste() {
    try {
      change(await Clipboard.getStringAsync(), true);
    } catch {
      setPasteError(true);
    }
    field.current?.focus();
  }

  return (
    <>
      <BridgeHeader
        title="Recipient"
        onBack={onBack}
        onScan={() => {
          Keyboard.dismiss();
          setScanError(null);
          setScanning(true);
          if (!permission?.granted && permission?.canAskAgain !== false)
            void requestPermission().catch(() =>
              setScanError("Could not access the camera."),
            );
        }}
      />
      <View className="px-[16px] pt-[36px] pb-[16px]">
        <View className="min-h-[48px] flex-row items-center gap-[12px]">
          <BottomSheetTextInput
            ref={field}
            accessibilityLabel={`${network} address`}
            placeholder={`${network} address`}
            placeholderTextColor="#AFADB3"
            value={input}
            onChangeText={change}
            autoCapitalize="none"
            autoComplete="off"
            autoCorrect={false}
            multiline
            maxLength={128}
            scrollEnabled={false}
            returnKeyType="done"
            submitBehavior="submit"
            onSubmitEditing={() => {
              if (valid) select(address);
              else setSubmitted(true);
            }}
            style={{
              flex: 1,
              minHeight: 48,
              paddingVertical: 10,
              fontFamily: "Geist_500Medium",
              fontSize: 20,
              lineHeight: 24,
              color: "#000",
            }}
          />
          <Pressable
            accessibilityRole="button"
            accessibilityLabel={input ? "Clear address" : "Paste"}
            hitSlop={4}
            onPress={
              input
                ? () => {
                    field.current?.clear();
                    change("");
                    field.current?.focus();
                  }
                : paste
            }
            className={
              input
                ? "h-[44px] w-[44px] items-center justify-center active:opacity-60"
                : "min-w-[64px] items-center justify-center rounded-full bg-black/[0.04] px-[16px] py-[8px] active:opacity-60"
            }
          >
            {input ? (
              <Clear />
            ) : (
              <Text className="text-[15px] leading-[20px] font-medium text-black">
                Paste
              </Text>
            )}
          </Pressable>
        </View>
        {invalid && (
          <Text
            accessibilityRole="alert"
            className="mt-[4px] text-[15px] leading-[20px] text-[#F9363C]"
          >
            Invalid {network} address
          </Text>
        )}
        {pasteError && (
          <Text
            accessibilityLiveRegion="polite"
            className="mt-[4px] text-[15px] leading-[20px] text-[#8A8A8E]"
          >
            Paste into the address field using your keyboard.
          </Text>
        )}
      </View>
      <BottomSheetScrollView
        keyboardShouldPersistTaps="handled"
        contentContainerStyle={{ paddingBottom: 16 }}
      >
        <Text className="px-[16px] pt-[12px] pb-[8px] text-[17px] leading-[22px] font-medium text-[#8A8A8E]">
          {showRecents ? "Recently used" : "Exact match"}
        </Text>
        {rows.map((row) => {
          const history =
            row.sends > 0
              ? `${row.sends} ${row.sends === 1 ? "send" : "sends"}`
              : "New address";
          return (
            <BridgeRow
              key={row.address}
              leading={<WalletGlyph />}
              label={`Use ${row.address} on ${network}, ${history}`}
              title={formatSenderAddress(row.address)}
              detail={`${network} · ${history}`}
              onPress={() => select(row.address)}
            />
          );
        })}
        {!rows.length && (!showRecents || recents !== null) && (
          <View className="min-h-[60px] flex-row items-center gap-[12px] px-[16px]">
            <View className="h-[48px] w-[48px] rounded-full border-2 border-dashed border-black/[0.12]" />
            <Text
              accessibilityLiveRegion="polite"
              className="text-[15px] leading-[20px] text-[#8A8A8E]"
            >
              No results found
            </Text>
          </View>
        )}
      </BottomSheetScrollView>
      {scanning && (
        <QrScannerModal
          visible
          permissionGranted={permission?.granted ?? false}
          canAskPermissionAgain={permission?.canAskAgain ?? true}
          onRequestPermission={requestPermission}
          scanError={scanError}
          onClose={() => setScanning(false)}
          onScan={({ data }) => {
            const candidate = data
              .trim()
              .replace(tron ? /^tron:/i : /^ethereum:/i, "");
            if (
              bridgeRecipientState(addressKind, candidate, true) !== "valid"
            ) {
              setScanError(
                `No valid ${network} address found in that QR code.`,
              );
              return;
            }
            change(candidate, true);
            setScanning(false);
          }}
        />
      )}
    </>
  );
}
