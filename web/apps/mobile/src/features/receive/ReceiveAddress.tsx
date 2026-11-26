import { PostHogMaskView } from "posthog-react-native";
import * as Clipboard from "expo-clipboard";
import * as Haptics from "expo-haptics";
import { useEffect, useState } from "react";
import { Alert, Text as NativeText } from "react-native";
import QRCode from "react-native-qrcode-svg";

import { Pressable, Text, View } from "@/tw";
import Copy from "../../../assets/images/receive/copy.svg";

// Red Loyal mascot rendered in the center of the QR. Passed to the QR as a raw
// SVG string so react-native-qrcode-svg renders it via SvgXml (preserving the
// mark's own fills) instead of going through the asset/transformer path.
// Mask-free on purpose: the original asset clipped the pupil with an SVG
// <mask> (mask-type:alpha, userSpaceOnUse), which react-native-svg renders on
// Android but silently drops on iOS — the pupil disappeared and the dog lost
// its eye. The mask only trimmed a sub-pixel of pupil overflow, so a slightly
// smaller pupil nudged into the eye white needs no clipping at all.
const QR_MARK_SVG = `<svg width="35" height="28" viewBox="0 0 35 28" fill="none" xmlns="http://www.w3.org/2000/svg">
<path d="M5.24998 24.4999L0 12.25H15.75V0L22.7499 12.25L24.4999 0L34.9999 27.9999L5.24998 24.4999Z" fill="#F9363C"/>
<path d="M19.369 15.141C22.2645 15.2927 24.5052 17.45 24.3737 19.9595L13.8881 19.41C14.0196 16.9005 16.4735 14.9892 19.369 15.141Z" fill="white"/>
<circle cx="19.2498" cy="17.7" r="2.1" transform="rotate(3 19.2498 17.7)" fill="black"/>
</svg>`;

export function receiveIconSource(file: string | null) {
  return file
    ? {
        uri: `https://app.askloyal.com/wallet-workspace/facelift/receive/${encodeURIComponent(file)}`,
      }
    : undefined;
}

export function useCopyAddress(address: string | null, onCopied?: () => void) {
  const [copiedAddress, setCopiedAddress] = useState<string | null>(null);
  useEffect(() => {
    if (!copiedAddress) return;
    const timer = setTimeout(() => setCopiedAddress(null), 2000);
    return () => clearTimeout(timer);
  }, [copiedAddress]);

  async function copy() {
    if (!address) return;
    const success = await Clipboard.setStringAsync(address).catch(() => false);
    if (!success) {
      Alert.alert("Could not copy address", "Please try again.");
      return;
    }
    setCopiedAddress(address);
    onCopied?.();
    void Haptics.notificationAsync(Haptics.NotificationFeedbackType.Success);
  }

  return { copied: !!address && copiedAddress === address, copy };
}

export function ReceiveButton({
  label,
  onPress,
  disabled,
}: {
  label: string;
  onPress: () => void;
  disabled?: boolean;
}) {
  return (
    <Pressable
      accessibilityRole="button"
      accessibilityState={{ disabled: !!disabled }}
      disabled={disabled}
      onPress={onPress}
      className="min-h-[50px] flex-1 items-center justify-center rounded-full bg-[#F5F5F5] active:opacity-60"
      style={{ opacity: disabled ? 0.4 : 1 }}
    >
      <Text className="text-[17px] leading-[22px] font-medium text-black">
        {label}
      </Text>
    </Pressable>
  );
}

export function ReceiveAddressCard({
  address,
  bridge,
}: {
  address: string;
  bridge?: {
    symbol: string;
    chainName: string;
    qrIcon: string | null;
    onCopy: () => void;
  };
}) {
  const backgroundColor = bridge ? "#F5F5F5" : "#F9F9FB";
  return (
    <PostHogMaskView
      style={{
        alignItems: "center",
        paddingTop: 32,
        paddingBottom: 16,
        backgroundColor,
        borderRadius: bridge ? 16 : 24,
        width: bridge ? "100%" : 256,
      }}
    >
      <View
        accessible
        accessibilityRole="image"
        accessibilityLabel={`${bridge?.chainName ?? "Solana"} receive QR code`}
      >
        <QRCode
          value={address}
          size={192}
          ecl="H"
          backgroundColor="transparent"
          logoSVG={bridge ? undefined : QR_MARK_SVG}
          logo={bridge ? receiveIconSource(bridge.qrIcon) : undefined}
          logoSize={bridge ? 32 : 40}
          logoMargin={6}
          logoBackgroundColor={backgroundColor}
          logoBorderRadius={8}
        />
      </View>
      {bridge && (
        <Text className="mt-5 text-center text-[16px] leading-5 font-semibold text-black">
          Send {bridge.symbol} to this address
        </Text>
      )}
      <View
        className={`w-full flex-row items-center px-4 ${
          bridge ? "mt-1" : "mt-5"
        }`}
      >
        {bridge && <View className="w-10" />}
        <Text className="flex-1 text-center font-mono text-[15px] leading-5 text-[#8A8A8E]">
          <NativeText style={{ color: "#000" }}>
            {address.slice(0, 4)}
          </NativeText>
          {address.slice(4, -4)}
          <NativeText style={{ color: "#000" }}>{address.slice(-4)}</NativeText>
        </Text>
        {bridge && (
          <Pressable
            accessibilityRole="button"
            accessibilityLabel="Copy bridge address"
            hitSlop={4}
            onPress={bridge.onCopy}
            className="h-11 w-10 items-center justify-center rounded-full active:opacity-60"
          >
            <Copy width={24} height={24} />
          </Pressable>
        )}
      </View>
    </PostHogMaskView>
  );
}
