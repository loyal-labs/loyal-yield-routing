import {
  BottomSheetScrollView,
  BottomSheetTextInput,
} from "@gorhom/bottom-sheet";
import type { BridgeDestination } from "@loyal-labs/api";
import { useState } from "react";
import { Keyboard } from "react-native";

import { formatUsdAmount } from "@/components/sheets/sheetFacts";
import { Text, View } from "@/tw";

import Search from "../../../assets/images/receive/search.svg";
import { bridgeAmountLabel, BridgeCoin, BridgeRow } from "./BridgeOutParts";

const assetOrder = ["usdc", "usdt", "usdg", "pyusd", "usds"];
const chainOrder = [
  "ethereum",
  "tron",
  "bsc",
  "base",
  "arbitrum",
  "polygon",
  "avalanche",
];
function rank(value: string, order: string[]) {
  const rank = order.indexOf(value);
  return rank === -1 ? order.length : rank;
}

export function BridgeSource({
  balanceRaw,
  priceUsd,
  onSelect,
}: {
  balanceRaw: bigint | null;
  priceUsd: number | null;
  onSelect: () => void;
}) {
  const value =
    balanceRaw !== null && priceUsd !== null
      ? formatUsdAmount((Number(balanceRaw) / 1e6) * priceUsd)
      : "—";
  const [whole, cents] = value.split(".");
  const balance = balanceRaw === null ? "—" : bridgeAmountLabel(balanceRaw, 6);
  return (
    <BridgeRow
      title="USD Coin"
      detail="USDC"
      label={`USD Coin, ${balance} USDC on Solana`}
      leading={<BridgeCoin file="usdc.png" badge="sol.png" />}
      trailing={
        <View className="max-w-[50%] items-end gap-[2px]">
          <Text
            numberOfLines={1}
            className="text-[17px] leading-[22px] font-medium text-black"
          >
            {whole}
            {cents && <Text className="text-[#AFADB3]">.{cents}</Text>}
          </Text>
          <Text
            numberOfLines={1}
            className="text-[15px] leading-[20px] text-[#8A8A8E]"
          >
            {balance} USDC
          </Text>
        </View>
      }
      onPress={onSelect}
    />
  );
}

export function BridgeDestinationPicker({
  destinations,
  asset,
  onSelect,
}: {
  destinations: BridgeDestination[];
  asset: string | null;
  onSelect: (id: string) => void;
}) {
  const [query, setQuery] = useState("");
  const needle = query.trim().toLowerCase();
  const options =
    asset === null
      ? [
          ...new Map(
            destinations.flatMap((d) =>
              (d.tokens ?? []).map((t) => [t.asset, t] as const),
            ),
          ).values(),
        ].map((t) => ({
          id: t.asset,
          name: t.name,
          detail: t.symbol,
          icon: t.icon,
        }))
      : destinations.flatMap((d) => {
          const token = d.tokens?.find((t) => t.asset === asset);
          return token
            ? [
                {
                  id: d.chain,
                  name: d.name,
                  detail:
                    token.minUsdc > 0
                      ? `Minimum ${token.minUsdc} USDC`
                      : undefined,
                  icon: d.icon,
                },
              ]
            : [];
        });
  const order = asset === null ? assetOrder : chainOrder;
  const matches = options
    .filter((o) =>
      `${o.name} ${o.detail ?? ""} ${o.id}`.toLowerCase().includes(needle),
    )
    .sort((a, b) => rank(a.id, order) - rank(b.id, order));
  const noun = asset === null ? "assets" : "chains";
  return (
    <>
      <View className="mx-[16px] mb-[16px] min-h-[52px] flex-row items-center gap-[12px] rounded-full bg-[#F2F2F7] px-[16px]">
        <Search />
        <BottomSheetTextInput
          accessibilityLabel={`Search ${noun}`}
          placeholder="Search"
          placeholderTextColor="#8A8A8E"
          autoCapitalize="none"
          autoCorrect={false}
          returnKeyType="search"
          value={query}
          onChangeText={setQuery}
          style={{
            flex: 1,
            fontFamily: "Geist_400Regular",
            fontSize: 17,
            color: "#000",
            paddingVertical: 8,
          }}
        />
      </View>
      <BottomSheetScrollView
        keyboardShouldPersistTaps="handled"
        contentContainerStyle={{ paddingBottom: 16 }}
      >
        {matches.map((o) => (
          <BridgeRow
            key={o.id}
            title={o.name}
            detail={o.detail}
            leading={<BridgeCoin file={o.icon} />}
            onPress={() => {
              Keyboard.dismiss();
              onSelect(o.id);
            }}
          />
        ))}
        {matches.length === 0 && (
          <Text
            accessibilityLiveRegion="polite"
            className="px-[16px] py-[16px] text-center text-[15px] text-[#8A8A8E]"
          >
            No {noun} found
          </Text>
        )}
      </BottomSheetScrollView>
    </>
  );
}
