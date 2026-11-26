import { fromRaw, toRaw, type BridgeTransfer, type EvmDepositAddressResponse } from "@loyal-labs/api";
import { useEmbeddedEthereumWallet, usePrivy, usePrivyClient } from "@privy-io/expo";
import { useIsFocused } from "@react-navigation/native";
import { useEffect, useReducer, useState, useSyncExternalStore } from "react";
import { AppState, Linking, Modal, Pressable, ScrollView, StyleSheet, Text, View } from "react-native";
import { useSafeAreaInsets } from "react-native-safe-area-context";

import { isPrivyConfigured } from "@/components/wallet/PrivyProviderRoot";
import { fundingRequest } from "@/features/receive/funding-api";
import { withTapHaptic } from "@/lib/haptics";
import { getSolanaEnv, onSolanaEnvChange } from "@/lib/solana/rpc/connection";
import { isWalletUnlocked, useWallet } from "@/lib/wallet/wallet-provider";

import { isExternalOwner, linkedSender } from "./privy-transfer";
import { type TransferReview, useIncomingTransfer } from "./use-incoming-transfer";

export function IncomingBridge({ walletAddress }: { walletAddress: string | null }) {
  const mainnet = useSyncExternalStore(onSolanaEnvChange, getSolanaEnv, getSolanaEnv) === "mainnet";
  const { state, signer, publicKey } = useWallet();
  const focused = useIsFocused();
  if (
    !mainnet ||
    !focused ||
    !isPrivyConfigured() ||
    !walletAddress ||
    publicKey !== walletAddress ||
    !isWalletUnlocked(state) ||
    !signer ||
    signer.kind === "privy" ||
    signer.publicKey.toBase58() !== walletAddress
  )
    return null;
  return <PrivyIncoming key={walletAddress} solana={walletAddress} />;
}

function PrivyIncoming({ solana }: { solana: string }) {
  const { isReady, user } = usePrivy();
  if (!isReady || !user || !isExternalOwner(user, solana)) return null;
  return <IncomingRoute key={user.id} solana={solana} user={user} />;
}

function IncomingRoute({
  solana,
  user,
}: {
  solana: string;
  user: NonNullable<ReturnType<typeof usePrivy>["user"]>;
}) {
  const client = usePrivyClient();
  const { wallets } = useEmbeddedEthereumWallet();
  const [route, setRoute] = useState<EvmDepositAddressResponse | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [retry, refresh] = useReducer((n: number) => n + 1, 0);
  useEffect(() => {
    const controller = new AbortController();
    const { signal } = controller;
    let reading = false;
    async function discover(interactive = false) {
      if (
        reading ||
        signal.aborted ||
        getSolanaEnv() !== "mainnet" ||
        AppState.currentState === "background"
      )
        return;
      reading = true;
      try {
        // This GET enforces the launch gate and never creates a deposit address.
        const deposit = await fundingRequest<EvmDepositAddressResponse>(
          solana,
          "/api/funding/evm-deposit-address",
          { signal, interactive },
        );
        if (!signal.aborted) {
          setRoute(deposit);
          setError(null);
        }
      } catch (e) {
        if (signal.aborted) return;
        const failure = e as { code?: string; status?: number };
        if (failure.status === 401 || failure.status === 403) setRoute(null);
        if (failure.code === "not_open") setError(null);
        else if (failure.status === 401) setError("Authorize your wallet to check incoming transfers.");
        else setError(e instanceof Error ? e.message : "Could not check incoming transfers.");
      } finally {
        reading = false;
      }
    }
    void discover(retry > 0);
    const poll = setInterval(() => void discover(), 30_000);
    const subscription = AppState.addEventListener("change", (next) => {
      if (next === "active") void discover();
    });
    return () => {
      controller.abort();
      clearInterval(poll);
      subscription.remove();
    };
  }, [solana, retry]);

  const owner = route && linkedSender(user, route);
  const wallet = owner && wallets.find((w) => w.address.toLowerCase() === owner.address.toLowerCase());
  if (route && owner && wallet) {
    return (
      <Transfers
        key={owner.id + owner.address}
        {...{ route, wallet, client, solana }}
        userId={user.id}
        senderId={owner.id}
      />
    );
  }
  return error ? (
    <View style={styles.card}>
      <Text style={styles.body}>{error}</Text>
      <Action label="Retry incoming transfers" onPress={withTapHaptic(refresh)} />
    </View>
  ) : null;
}

function Transfers(identity: Parameters<typeof useIncomingTransfer>[0]) {
  const { items, transfers, pending, busy, error, notice, review, cancel, reload, act } =
    useIncomingTransfer(identity);
  const [linkError, setLinkError] = useState(false);
  const history = transfers.find(
    (t) => t.state === "filled" || t.state === "refunded" || t.state === "failed",
  );
  const historyNetwork = identity.route.networks?.find((n) => n.chain === history?.chain);
  const historyToken = historyNetwork?.tokens?.find((t) => t.asset === history?.asset);
  const latest = notice?.requestId && transfers.find((t) => t.requestId === notice.requestId);
  const showNotice = notice && !(latest && (latest.inFlight || latest.state !== "quoted"));
  function receipt(id: string) {
    setLinkError(false);
    void Linking.openURL(`https://relay.link/transaction/${encodeURIComponent(id)}`).catch(() =>
      setLinkError(true),
    );
  }
  if (!items.length && !history && !showNotice && !pending && !error && !busy && !review) return null;
  return (
    <View style={styles.group}>
      {items.map((item) => {
        const under = item.amountRaw < toRaw(String(item.minUsd), item.decimals);
        const retry =
          history?.chain === item.chain && history.asset === item.asset && history.state !== "filled";
        return (
          <View key={item.key} style={styles.card}>
            <Text style={styles.caption}>
              {item.transfer?.inFlight ? "Transfer in progress" : "Incoming transfer"}
            </Text>
            <Text style={styles.amount}>
              {fromRaw(item.amountRaw, item.decimals)} {item.symbol} from {item.shortName}
            </Text>
            {item.transfer?.inFlight ? (
              <Action label="View receipt" onPress={withTapHaptic(() => receipt(item.transfer!.requestId))} />
            ) : under ? (
              <Text style={styles.body}>Below the ${item.minUsd} minimum</Text>
            ) : (
              <Action
                label={retry ? "Retry transfer" : "Review transfer"}
                disabled={Boolean(busy || review)}
                onPress={withTapHaptic(() => void act(item))}
              />
            )}
          </View>
        );
      })}
      {busy === "quoting" && (
        <View style={styles.card}>
          <Text accessibilityLiveRegion="polite" style={styles.body}>
            Getting your quote…
          </Text>
          <Action label="Cancel" onPress={withTapHaptic(cancel)} />
        </View>
      )}
      {(busy === "sending" || (pending && !items.some((i) => i.transfer?.inFlight) && !busy)) && (
        <Text accessibilityLiveRegion="polite" style={styles.status}>
          Checking transfer status…
        </Text>
      )}
      {history && (
        <View style={styles.card}>
          <Text style={styles.amount} accessibilityLiveRegion="polite">
            {historyLabel(history)}
          </Text>
          <Text style={styles.body}>
            {historyNetwork && historyToken
              ? `${fromRaw(history.amountInRaw, historyToken.decimals)} ${historyToken.symbol} from ${
                  historyNetwork.shortName
                }`
              : history.chain + " · " + history.asset.toUpperCase()}
          </Text>
          <Action label="View receipt" onPress={withTapHaptic(() => receipt(history.requestId))} />
        </View>
      )}
      {(showNotice || error || linkError) && (
        <View style={styles.card}>
          <Text style={styles.body} accessibilityLiveRegion="polite">
            {linkError ? "Could not open the receipt." : error || notice?.message}
          </Text>
          {showNotice && notice.requestId && (
            <Action label="View receipt" onPress={withTapHaptic(() => receipt(notice.requestId!))} />
          )}
          <Action label="Refresh status" disabled={Boolean(busy)} onPress={withTapHaptic(reload)} />
        </View>
      )}
      {review && (
        <Review
          review={review}
          solana={identity.solana}
          sender={identity.wallet.address}
          cancel={cancel}
          approve={() => void act(review.item, review)}
        />
      )}
    </View>
  );
}

function historyLabel(t: BridgeTransfer) {
  if (t.state === "filled") return "USDC arrived on Solana";
  if (t.state === "refunded") return "Transfer refunded to your EVM wallet";
  return "Transfer failed";
}

function Review({
  review,
  solana,
  sender,
  approve,
  cancel,
}: {
  review: TransferReview;
  solana: string;
  sender: string;
  approve: () => void;
  cancel: () => void;
}) {
  const { quote, item, amountRaw } = review;
  const insets = useSafeAreaInsets();
  const raw = amountRaw.toString().padStart(item.decimals + 1, "0");
  const amount = item.decimals
    ? raw.slice(0, -item.decimals) +
      ("." + raw.slice(-item.decimals)).replace(/0+$/, "").replace(/\.$/, "")
    : raw;
  return (
    <Modal visible animationType="slide" transparent onRequestClose={cancel}>
      <View style={styles.overlay}>
        <View
          style={[styles.sheet, { paddingBottom: Math.max(insets.bottom, 16) }]}
          accessibilityViewIsModal
        >
          <ScrollView contentContainerStyle={styles.details} bounces={false}>
            <Text accessibilityRole="header" style={styles.title}>
              Review incoming transfer
            </Text>
            <Field label={`Send from ${item.shortName}`} value={`${amount} ${item.symbol}`} />
            <Field label="EVM sender" value={sender} />
            <Field label="Receive on Solana" value={`${fromRaw(quote.amountOutRaw, 6)} USDC`} />
            <Field label="Solana recipient" value={solana} />
            <Field label="Minimum received" value={`${fromRaw(quote.amountOutMinRaw, 6)} USDC`} />
            {quote.relayerFeeUsd && <Field label="Relay fee" value={`$${quote.relayerFeeUsd}`} />}
            <Text style={styles.body}>
              {quote.calls?.length === 2
                ? "Approving authorizes the token allowance and the bridge transfer. Network fees are sponsored."
                : "Approving sends the bridge transfer. Network fees are sponsored."}
            </Text>
            <Action label="Approve and send" primary onPress={approve} />
            <Action label="Cancel" onPress={cancel} />
          </ScrollView>
        </View>
      </View>
    </Modal>
  );
}

function Field({ label, value }: { label: string; value: string }) {
  return (
    <View style={styles.field}>
      <Text style={styles.caption}>{label}</Text>
      <Text selectable style={styles.body}>
        {value}
      </Text>
    </View>
  );
}

function Action({
  label,
  onPress,
  disabled,
  primary,
}: {
  label: string;
  onPress: () => void;
  disabled?: boolean;
  primary?: boolean;
}) {
  return (
    <Pressable
      accessibilityRole="button"
      accessibilityState={{ disabled: !!disabled }}
      disabled={disabled}
      onPress={onPress}
      style={[styles.button, primary && styles.primary, disabled && styles.disabled]}
    >
      <Text style={[styles.action, primary && styles.white]}>{label}</Text>
    </Pressable>
  );
}

const styles = StyleSheet.create({
  group: { gap: 8, marginVertical: 4 },
  card: { marginHorizontal: 16, padding: 16, gap: 4, borderRadius: 20, backgroundColor: "#F5F5F5" },
  caption: { fontFamily: "Geist_400Regular", fontSize: 13, color: "#595959" },
  amount: { fontFamily: "Geist_600SemiBold", fontSize: 17, color: "#171717" },
  body: { fontFamily: "Geist_400Regular", fontSize: 15, lineHeight: 21, color: "#242424" },
  status: { marginHorizontal: 24, fontFamily: "Geist_500Medium", color: "#444" },
  button: { minHeight: 44, justifyContent: "center", paddingHorizontal: 4 },
  action: { fontFamily: "Geist_600SemiBold", fontSize: 15, color: "#171717" },
  primary: { alignItems: "center", borderRadius: 28, backgroundColor: "#171717", minHeight: 50 },
  white: { color: "#FFF" },
  disabled: { opacity: 0.45 },
  overlay: { flex: 1, justifyContent: "flex-end", backgroundColor: "rgba(0,0,0,0.4)" },
  sheet: {
    maxHeight: "92%",
    backgroundColor: "#FFF",
    borderTopLeftRadius: 32,
    borderTopRightRadius: 32,
  },
  details: { paddingHorizontal: 24, paddingTop: 24, gap: 14 },
  title: { fontFamily: "Geist_600SemiBold", fontSize: 24, color: "#171717" },
  field: { gap: 3 },
});
