import { fromRaw } from "@loyal-labs/api";
import type { ReactNode } from "react";

import { receiveIconSource } from "@/features/receive/ReceiveAddress";
import { Pressable, Text, View } from "@/tw";
import { Image } from "@/tw/image";

import Scan from "../../../assets/images/bridge-out/scan.svg";
import Wallet from "../../../assets/images/bridge-out/wallet.svg";
import Placeholder from "../../../assets/images/receive/asset-placeholder.svg";
import Back from "../../../assets/images/receive/back.svg";
import Close from "../../../assets/images/receive/close.svg";

const usdc = require("../../../assets/images/earn/usdc.png");
const sol = require("../../../assets/images/receive/sol.png");

// Hermes may omit thousands separators for BigInt.toLocaleString.
export function bridgeAmountLabel(raw: bigint | string, decimals: number) {
  const [whole, fraction] = fromRaw(raw, decimals).split(".");
  const grouped = whole.replace(/,/g, "").replace(/\B(?=(\d{3})+(?!\d))/g, ",");
  return fraction ? `${grouped}.${fraction}` : grouped;
}

export function BridgeHeader({
  title,
  onBack,
  close,
  onScan,
  disabled,
}: {
  title: string;
  onBack: () => void;
  close?: boolean;
  onScan?: () => void;
  disabled?: boolean;
}) {
  return (
    <View className="flex-row items-center justify-between px-[16px] py-[16px]">
      <Pressable
        accessibilityRole="button"
        accessibilityLabel={close ? "Close Bridge" : "Back"}
        accessibilityState={{ disabled }}
        disabled={disabled}
        onPress={onBack}
        className="h-[44px] w-[44px] items-center justify-center rounded-full bg-[#F2F2F7] active:opacity-60"
      >
        {close ? <Close /> : <Back />}
      </Pressable>
      <Text
        accessibilityRole="header"
        className="flex-1 text-center text-[17px] leading-[22px] font-semibold text-black"
      >
        {title}
      </Text>
      {onScan ? (
        <Pressable
          accessibilityRole="button"
          accessibilityLabel="Scan QR code"
          onPress={onScan}
          className="h-[44px] w-[44px] items-center justify-center rounded-full bg-[#F2F2F7] active:opacity-60"
        >
          <Scan />
        </Pressable>
      ) : (
        <View className="w-[44px]" />
      )}
    </View>
  );
}

export function BridgeButton({
  label,
  onPress,
  tone = "primary",
}: {
  label: string;
  onPress?: () => void;
  tone?: "primary" | "secondary" | "attention";
}) {
  return (
    <Pressable
      accessibilityRole="button"
      accessibilityState={{ disabled: !onPress }}
      disabled={!onPress}
      onPress={onPress}
      className={`min-h-[50px] items-center justify-center rounded-full px-[16px] active:opacity-60 ${
        tone === "primary"
          ? "bg-black"
          : tone === "secondary"
          ? "bg-[#F5F5F5]"
          : "bg-[#FEE3E4]"
      }`}
    >
      <Text
        className={`text-center font-medium ${
          tone === "primary"
            ? "text-[16px] leading-[20px] text-white"
            : tone === "secondary"
            ? "text-[17px] leading-[22px] text-black"
            : "text-[16px] leading-[20px] text-[#F9363C]"
        }`}
      >
        {label}
      </Text>
    </Pressable>
  );
}

function coinSource(file: string) {
  if (file === "usdc.png") return usdc;
  if (file === "sol.png") return sol;
  return receiveIconSource(file);
}

export function BridgeCoin({
  file,
  badge,
  size = 48,
}: {
  file: string;
  badge?: string;
  size?: 36 | 48;
}) {
  const badgeSize = size === 36 ? 18 : 22;
  const icon = (name: string, diameter: number) =>
    name && name !== "asset-placeholder.svg" ? (
      <Image
        source={coinSource(name)}
        recyclingKey={name}
        style={{
          width: diameter,
          height: diameter,
          borderRadius: diameter / 2,
        }}
      />
    ) : (
      <Placeholder width={diameter} height={diameter} />
    );
  return (
    <View style={{ width: size, height: size }}>
      {icon(file, size)}
      {badge !== undefined && (
        <View
          className="absolute -right-px -bottom-px items-center justify-center rounded-full"
          style={{
            width: badgeSize,
            height: badgeSize,
            backgroundColor: size === 36 ? "#F5F5F5" : "#FFF",
          }}
        >
          {icon(badge, badgeSize - 4)}
        </View>
      )}
    </View>
  );
}

export function WalletGlyph() {
  return (
    <View className="h-[48px] w-[48px] items-center justify-center rounded-xl bg-black/[0.04]">
      <Wallet />
    </View>
  );
}

export function BridgeRow({
  title,
  detail,
  leading,
  trailing,
  onPress,
  label,
}: {
  title: string;
  detail?: string;
  leading: ReactNode;
  trailing?: ReactNode;
  onPress: () => void;
  label?: string;
}) {
  return (
    <Pressable
      accessibilityRole="button"
      accessibilityLabel={label ?? [title, detail].filter(Boolean).join(", ")}
      onPress={onPress}
      className="min-h-[60px] flex-row items-center gap-[12px] px-[16px] py-[6px] active:opacity-60"
    >
      {leading}
      <View className="min-w-0 flex-1 gap-[2px]">
        <Text
          numberOfLines={1}
          className="text-[17px] leading-[22px] font-medium text-black"
        >
          {title}
        </Text>
        {detail && (
          <Text
            numberOfLines={1}
            className="text-[15px] leading-[20px] text-[#8A8A8E]"
          >
            {detail}
          </Text>
        )}
      </View>
      {trailing}
    </Pressable>
  );
}
