import {
  BottomSheetScrollView,
  BottomSheetTextInput,
} from "@gorhom/bottom-sheet";
import type { EvmDepositNetwork, EvmDepositToken } from "@loyal-labs/api";
import { type ReactNode, useEffect, useState } from "react";
import { ActivityIndicator, Alert, Keyboard } from "react-native";

import { track } from "@/lib/analytics/analytics";
import { BRIDGE_EVENTS } from "@/lib/analytics/bridge-events";
import { Pressable, Text, View } from "@/tw";
import { Image } from "@/tw/image";

import AssetPlaceholder from "../../../assets/images/receive/asset-placeholder.svg";
import Chevron from "../../../assets/images/receive/chevron.svg";
import Info from "../../../assets/images/receive/info.svg";
import Search from "../../../assets/images/receive/search.svg";
import Warning from "../../../assets/images/receive/warning.svg";
import {
  ReceiveAddressCard,
  ReceiveButton,
  receiveIconSource,
  useCopyAddress,
} from "./ReceiveAddress";
import type { useReceiveBridge } from "./useReceiveBridge";

type Bridge = ReturnType<typeof useReceiveBridge>;

export type DepositSelection = { asset: string; chain: string | null };

const usdcIcon = require("../../../assets/images/earn/usdc.png");
const solIcon = require("../../../assets/images/receive/sol.png");

function minimumDeposit(network: EvmDepositNetwork, token: EvmDepositToken) {
  const usd = network.minUsd.toFixed(2);
  return token.stable ? `${usd} ${token.symbol}` : `$${usd}`;
}

function Coin({ file, label }: { file: string | null; label: string }) {
  const source = file === "usdc.png" ? usdcIcon : receiveIconSource(file);
  return source ? (
    <Image
      source={source}
      recyclingKey={file}
      className="h-12 w-12 rounded-full bg-white"
    />
  ) : (
    <View className="h-12 w-12 items-center justify-center rounded-full bg-[#F5F5F5]">
      <Text className="text-[17px] font-medium text-[#8A8A8E]">
        {label.slice(0, 1)}
      </Text>
    </View>
  );
}

function Notice({
  children,
  onRetry,
}: {
  children: string;
  onRetry?: () => void;
}) {
  return (
    <View className="flex-row items-start gap-2 rounded-2xl bg-[#FFE4E5] px-4 py-2">
      <Warning width={24} height={24} />
      <Text
        accessibilityLiveRegion="polite"
        className="flex-1 text-[13px] leading-4 text-[#CF2129]"
      >
        {children}
      </Text>
      {onRetry && (
        <Pressable
          accessibilityRole="button"
          onPress={onRetry}
          className="min-h-11 justify-center pl-2 active:opacity-60"
        >
          <Text className="font-medium text-black">Try again</Text>
        </Pressable>
      )}
    </View>
  );
}

function Loading({ label }: { label: string }) {
  return (
    <View className="flex-row items-center justify-center gap-2 py-4">
      <ActivityIndicator color="#8A8A8E" />
      <Text
        accessibilityLiveRegion="polite"
        className="text-[15px] text-[#8A8A8E]"
      >
        {label}
      </Text>
    </View>
  );
}

function StepRow({
  caption,
  value,
  icon,
  placeholder,
  destination,
  onPress,
}: {
  caption: string;
  value: string;
  icon?: ReactNode;
  placeholder?: boolean;
  destination?: boolean;
  onPress: () => void;
}) {
  return (
    <Pressable
      accessibilityRole="button"
      accessibilityLabel={`${caption}, ${value}${
        destination ? " on Solana, more information" : ""
      }`}
      onPress={onPress}
      className="min-h-[60px] flex-row items-center gap-3 px-4 py-[6px] active:opacity-60"
    >
      {icon ?? (
        <View className="h-12 w-12 items-center justify-center rounded-full bg-[#F5F5F5]">
          <AssetPlaceholder width={28} height={28} />
        </View>
      )}
      {!destination && (
        <View
          pointerEvents="none"
          className="absolute bottom-[-4px] left-[39px] h-2 w-[2px] rounded-full bg-[#CBCBCD]"
        />
      )}
      <View className="flex-1 gap-[2px]">
        <Text className="text-[15px] leading-5 text-[#8A8A8E]">{caption}</Text>
        <View className="flex-row items-center gap-1">
          <Text
            className={`shrink text-[17px] leading-[22px] font-medium ${
              placeholder ? "text-[#AFADB3]" : "text-black"
            }`}
          >
            {value}
          </Text>
          {destination && (
            <Text className="rounded-md bg-[#F5F5F5] px-1 py-px text-[11px] leading-[14px] font-medium text-[#8A8A8E]">
              Solana
            </Text>
          )}
        </View>
      </View>
      {destination ? (
        <Info width={24} height={24} />
      ) : (
        <Chevron width={24} height={24} />
      )}
    </Pressable>
  );
}

function RouteAmounts({
  network,
  token,
}: {
  network: EvmDepositNetwork;
  token: EvmDepositToken;
}) {
  const rows: [string, string][] = [];
  if (network.minUsd > 0)
    rows.push(["Minimum deposit amount", minimumDeposit(network, token)]);
  if (network.route === "relay_deposit_address") {
    if (token.feeUsd != null)
      rows.push(["Fee", `~$${Number(token.feeUsd).toFixed(2)}`]);
    if (token.receiveUsdc != null) {
      const [whole, fraction = ""] = token.receiveUsdc.split(".");
      rows.push([
        "You receive about",
        `${whole}.${fraction.padEnd(2, "0").slice(0, 2)} USDC`,
      ]);
    }
  } else if (token.feePercent != null) {
    rows.push(["Fee", `${Number(token.feePercent)}%`]);
  }
  if (!rows.length) return null;
  return (
    <View className="rounded-2xl bg-[#F5F5F5] px-4 py-1">
      {rows.map(([label, value]) => (
        <View
          key={label}
          className="flex-row items-center justify-between gap-2 py-[10px]"
        >
          <Text className="flex-1 text-[13px] leading-4 text-[#8A8A8E]">
            {label}
          </Text>
          <Text className="text-[13px] leading-4 text-black">{value}</Text>
        </View>
      ))}
    </View>
  );
}

export function OtherChains({
  bridge,
  selection,
  onAssets,
  onChains,
  bottomInset,
}: {
  bridge: Bridge;
  selection: DepositSelection | null;
  onAssets: () => void;
  onChains: () => void;
  bottomInset: number;
}) {
  const asset = bridge.networks
    .flatMap((n) => n.tokens ?? [])
    .find((t) => t.asset === selection?.asset);
  const network = bridge.networks.find((n) => n.chain === selection?.chain);
  const token = network?.tokens?.find((t) => t.asset === selection?.asset);
  const address = token ? network?.address ?? null : null;
  const tron = network?.route === "relay_deposit_address";
  const { copy, copied } = useCopyAddress(address, () =>
    track(BRIDGE_EVENTS.receiveAddressCopied, { network_family: tron ? "tron" : "evm" }),
  );
  const selectedTron = tron && !!token;
  const { loadTron } = bridge;
  useEffect(() => {
    if (selectedTron) void loadTron();
  }, [selectedTron, loadTron]);
  const loading = tron ? bridge.tronLoading : bridge.loading;
  const error = tron ? bridge.tronError : bridge.error;
  const retry = tron ? bridge.retryTron : bridge.retry;
  const confirmOnWallet =
    network?.route === "evm" && bridge.kind === "relay_signed";

  return (
    <>
      <BottomSheetScrollView
        style={{ flex: 1 }}
        contentContainerStyle={{ paddingTop: 8, paddingBottom: 8 }}
      >
        <View className="gap-1">
          <StepRow
            caption="Asset to deposit"
            value={asset?.symbol ?? "Select asset"}
            placeholder={!asset}
            icon={asset && <Coin file={asset.icon} label={asset.symbol} />}
            onPress={onAssets}
          />
          {asset && (
            <StepRow
              caption="Sending from chain"
              value={network?.name ?? "Select chain"}
              placeholder={!network}
              icon={
                network && <Coin file={network.icon} label={network.name} />
              }
              onPress={onChains}
            />
          )}
          <StepRow
            caption="Asset to receive"
            value="USDC"
            destination
            onPress={() =>
              Alert.alert(
                "Asset to receive",
                confirmOnWallet
                  ? "Confirm the incoming transfer on Wallet to receive USDC on Solana."
                  : "Your bridged deposit arrives as USDC on Solana.",
              )
            }
            icon={
              <View className="h-12 w-12">
                <Image source={usdcIcon} className="h-12 w-12 rounded-full" />
                <View className="absolute -right-px -bottom-px rounded-full bg-white p-[2px]">
                  <Image
                    source={solIcon}
                    className="h-[19px] w-[19px] rounded-full"
                  />
                </View>
              </View>
            }
          />
        </View>

        <View className="gap-2 px-4 pt-4">
          {network && token && (
            <Notice>
              {`Send ${tron ? token.symbol : "funds"} only via the ${
                network.shortName
              } network${tron ? "" : " and not from an exchange"}.${
                network.minUsd > 0
                  ? ` The minimum amount is $${network.minUsd} in the selected asset.`
                  : ""
              } Sending an unsupported token will result in permanent loss of funds.`}
            </Notice>
          )}
          {error && <Notice onRetry={retry}>{error}</Notice>}
          {loading && !address && (
            <Loading
              label={network ? "Loading address…" : "Loading deposit assets…"}
            />
          )}
          {!error && !loading && !address && network && token && (
            <Notice onRetry={retry}>
              No deposit address is available yet.
            </Notice>
          )}
          {network && token && (
            <>
              {address && (
                <ReceiveAddressCard
                  key={`${network.chain}:${address}`}
                  address={address}
                  bridge={{
                    symbol: token.symbol,
                    chainName: network.name,
                    qrIcon: network.qrIcon ?? network.icon,
                    onCopy: copy,
                  }}
                />
              )}
              <RouteAmounts network={network} token={token} />
            </>
          )}
        </View>
      </BottomSheetScrollView>
      {network && token && (
        <View
          className="flex-row px-4 pt-2"
          style={{ paddingBottom: bottomInset + 16 }}
        >
          <ReceiveButton
            label={copied ? "Copied!" : "Copy address"}
            onPress={copy}
            disabled={!address}
          />
        </View>
      )}
    </>
  );
}

export function DepositPicker({
  bridge,
  chainAsset,
  onAsset,
  onNetwork,
  bottomInset,
}: {
  bridge: Bridge;
  chainAsset: string | null;
  onAsset: (asset: string) => void;
  onNetwork: (network: EvmDepositNetwork) => void;
  bottomInset: number;
}) {
  const [query, setQuery] = useState("");
  const needle = query.trim().toLowerCase();
  const options =
    chainAsset === null
      ? [
          ...new Map(
            bridge.networks.flatMap((n) =>
              (n.tokens ?? []).map((t) => [t.asset, t] as const),
            ),
          ).values(),
        ].map((t) => ({
          id: t.asset,
          name: t.name,
          detail: t.symbol,
          icon: t.icon,
          search: [t.name, t.symbol, t.asset],
          choose: () => onAsset(t.asset),
        }))
      : bridge.networks.flatMap((n) => {
          const token = n.tokens?.find((t) => t.asset === chainAsset);
          return token
            ? [
                {
                  id: n.chain,
                  name: n.name,
                  icon: n.icon,
                  search: [n.name, n.shortName, n.chain],
                  detail:
                    n.minUsd > 0
                      ? `Min deposit amount: ${minimumDeposit(n, token)}`
                      : null,
                  choose: () => onNetwork(n),
                },
              ]
            : [];
        });
  const rows = options.filter((row) =>
    row.search.some((text) => text.toLowerCase().includes(needle)),
  );
  const noun = chainAsset === null ? "assets" : "chains";

  return (
    <>
      <View className="mx-4 mb-2 min-h-[52px] flex-row items-center gap-3 rounded-full bg-[#F2F2F7] px-4">
        <Search width={24} height={24} />
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
            paddingVertical: 8,
            fontFamily: "Geist_400Regular",
            fontSize: 17,
            color: "#000",
          }}
        />
      </View>
      <BottomSheetScrollView
        style={{ flex: 1 }}
        keyboardShouldPersistTaps="handled"
        contentContainerStyle={{
          paddingTop: 8,
          paddingBottom: bottomInset + 16,
        }}
      >
        {rows.map((row) => (
          <Pressable
            key={row.id}
            accessibilityRole="button"
            accessibilityLabel={row.name}
            accessibilityHint={row.detail ?? undefined}
            onPress={() => {
              Keyboard.dismiss();
              row.choose();
            }}
            className="min-h-[60px] flex-row items-center gap-3 px-4 py-[6px] active:opacity-60"
          >
            <Coin file={row.icon} label={row.name} />
            <View className="flex-1 gap-[2px]">
              <Text className="text-[17px] leading-[22px] font-medium text-black">
                {row.name}
              </Text>
              {row.detail && (
                <Text className="text-[15px] leading-5 text-[#8A8A8E]">
                  {row.detail}
                </Text>
              )}
            </View>
          </Pressable>
        ))}
        {rows.length === 0 && (
          <View className="px-4 py-3">
            {bridge.networks.length === 0 && bridge.error ? (
              <Notice onRetry={bridge.retry}>{bridge.error}</Notice>
            ) : bridge.networks.length === 0 && bridge.loading ? (
              <Loading label="Loading deposit assets…" />
            ) : options.length === 0 && chainAsset === null ? (
              <Notice onRetry={bridge.retry}>
                No deposit assets are available.
              </Notice>
            ) : (
              <Text
                accessibilityLiveRegion="polite"
                className="py-4 text-center text-[15px] text-[#8A8A8E]"
              >
                No {noun} found
              </Text>
            )}
          </View>
        )}
      </BottomSheetScrollView>
    </>
  );
}
