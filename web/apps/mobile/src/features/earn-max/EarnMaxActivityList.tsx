import {
  earnMaxPending,
  earnMaxPendingMessage,
  earnMaxWithdrawalPending,
  earnMaxWithdrawalCostView,
  formatEarnMaxWithdrawalAmount,
} from "@loyal-labs/shared";
import { Fragment, useEffect, useState } from "react";
import { useIsFocused } from "@react-navigation/native";
import {
  ActivityIndicator,
  Linking,
  Pressable,
  StyleSheet,
  Text,
  TextInput,
  View,
} from "react-native";
import { EarnMaxWithdrawalIcon } from "./EarnMaxTransferDestination";
import { ActivityListSkeleton } from "@/features/activity/ui/ActivityListSkeleton";
import { useEarnMax } from "./EarnMaxProvider";
export function EarnMaxActivityList() {
  const {
    summary,
    activity,
    error,
    busy,
    run,
    refresh,
    approval,
    approveWithdrawal,
  } = useEarnMax();
  const [minimum, setMinimum] = useState("");
  const [maxFee, setMaxFee] = useState("");
  const focused = useIsFocused();
  useEffect(() => {
    if (focused) void refresh().catch(() => {});
  }, [focused, refresh]);
  const [actionError, setActionError] = useState<string | null>(null);
  const [checking, setChecking] = useState(false);
  const [checkedAt, setCheckedAt] = useState<Date | null>(null);
  const withdrawal = summary?.withdrawal ?? null;
  const costs = earnMaxWithdrawalCostView({status:approval?.costStatus??null,approved:approval?.approved??null});
  const healthReason = withdrawal?.health?.reason;
  const recoverable = summary && "recoverableUsdcRaw" in summary && typeof summary.recoverableUsdcRaw === "string" ? summary.recoverableUsdcRaw : null;
  const pending = earnMaxPending(withdrawal);
  useEffect(() => {
    setCheckedAt(null);
    setActionError(null);
    setMinimum("");setMaxFee("");
  }, [withdrawal?.requestId,approval?.draft.requestSignature,approval?.draft.policySeed]);
  return (
    <View>
      {recoverable && BigInt(recoverable) > BigInt(0) ? <View style={styles.pendingRow}><View style={{flex:1,gap:8}}><Text style={styles.subtitle}>{formatEarnMaxWithdrawalAmount(recoverable,"USDC")} remains available in your Earn MAX account.</Text><Pressable accessibilityRole="button" disabled={busy} onPress={async()=>{try{await run({action:"recover_assets"})}catch(error){setActionError(error instanceof Error ? error.message : "Unable to move remaining USDC.")}}}><Text style={styles.title}>Move remaining USDC to wallet</Text></Pressable></View></View> : null}
      {(error || actionError) && (
        <Text accessibilityRole="alert" style={{ color: "#F9363C" }}>
          {actionError ?? error}
        </Text>
      )}
      {withdrawal && earnMaxWithdrawalPending(withdrawal) && (
        <View style={styles.pendingRow}>
          <EarnMaxWithdrawalIcon />
          <View style={{ flex: 1, gap: 8 }}>
            <Text style={styles.title}>
              Withdrawal · ${(Number(withdrawal.amountRaw) / 1e6).toFixed(2)}
            </Text>
            <Text style={styles.subtitle}>
              {healthReason === "certified_nav_unavailable" ? "Waiting for an updated vault value. Your request remains pending."
                : healthReason === "withdrawal_price_limit" ? "Waiting for a price within the approved withdrawal limit."
                : approval?.reason === "needs_consent" ? "Review and approve a minimum USDC amount to continue."
                : !approval?.approved
                ? "Approve a minimum USDC amount to continue."
                : approval.reason === "waiting_price"
                  ? "Waiting for your approved minimum. You can wait or approve a lower amount."
                  : approval.reason === "expired"
                    ? "Your approval expired. A claim you already signed may still finish. Review and approve before signing again."
                    : withdrawal.canClaim
                ? "Ready to claim to your wallet."
                : pending
                ? earnMaxPendingMessage(pending)
                : `Available after ${new Date(
                            withdrawal.readyBy,
                  ).toLocaleString()}, subject to vault liquidity.`}
            </Text>
            {approval?.approved ? (
              <>
              <Text style={styles.subtitle}>
                Approved minimum:{" "}
                {formatEarnMaxWithdrawalAmount(approval.approved.minClaimUsdcRaw,"USDC")}.
              </Text>
              <View accessibilityLabel="Withdrawal costs" style={{gap:4}}>{costs.rows.map(({label,value})=><View key={label} style={{flexDirection:"row",justifyContent:"space-between",gap:12}}><Text style={[styles.subtitle,{flex:1}]}>{label}</Text><Text style={styles.subtitle}>{value??"Unavailable"}</Text></View>)}</View>
              <Text style={styles.subtitle}>The operator pays its own network fees. Your claim fees are shown separately.</Text>
              {costs.unknownReason ? <Text style={styles.subtitle}>Some cost details are unavailable or awaiting confirmation. Check status before signing again.</Text>:null}
              {costs.pending ? <Text style={styles.subtitle}>A claim you signed is pending and may still finish under that approval.</Text>:null}
              </>
            ) : null}
            <Text style={styles.subtitle}>Minimum USDC to receive</Text>
            <TextInput
              accessibilityLabel="Minimum USDC to receive"
              keyboardType="decimal-pad"
              onChangeText={setMinimum}
              value={minimum}
              style={styles.subtitle}
            />
            <Text style={styles.subtitle}>Maximum network fees (SOL)</Text>
            <TextInput
              accessibilityLabel="Maximum network fees in SOL"
              keyboardType="decimal-pad"
              onChangeText={setMaxFee}
              value={maxFee}
              style={styles.subtitle}
            />
            <Text style={styles.subtitle}>
              Failed attempts can charge network fees. A claim you already signed
              may finish under its earlier approval. A new minimum needs a new wallet approval.
            </Text>
            <Pressable
              accessibilityRole="button"
              disabled={busy}
              onPress={async () => {
                try {
                  const parse = (value: string, decimals: number) => {
                    if (
                      !new RegExp(`^\\d+(?:\\.\\d{1,${decimals}})?$`).test(
                        value,
                      )
                    )
                      throw new Error("Enter valid amounts.");
                    const [whole, fraction = ""] = value.split(".");
                    return (
                      BigInt(whole) * BigInt(10) ** BigInt(decimals) +
                      BigInt(fraction.padEnd(decimals, "0"))
                    );
                  };
                  await approveWithdrawal(parse(minimum, 6), parse(maxFee, 9));
                } catch (error) {
                  setActionError(
                    error instanceof Error ? error.message : "Approval failed.",
                  );
                }
              }}
            >
              <Text style={styles.title}>Approve minimum</Text>
            </Pressable>
            <Pressable
              accessibilityRole="button"
              accessibilityState={{
                busy: busy || checking,
                disabled: busy || checking,
              }}
              disabled={busy || checking}
              onPress={async () => {
                setActionError(null);
                try {
                  if (
                    withdrawal.canClaim &&
                    approval?.approved &&
                    !approval.reason
                  )
                    await run({ action: "claim" });
                  else {
                    setChecking(true);
                    setCheckedAt(null);
                    if (await refresh()) setCheckedAt(new Date());
                  }
                } catch (e) {
                  setActionError(
                    e instanceof Error
                      ? e.message
                      : "Unable to update withdrawal.",
                  );
                } finally {
                  setChecking(false);
                }
              }}
              style={{
                paddingHorizontal: 16,
                paddingVertical: 8,
                alignSelf: "flex-start",
                backgroundColor: "#000",
                borderRadius: 30,
                flexDirection: "row",
                alignItems: "center",
                gap: 8,
                opacity: busy || checking ? 0.5 : 1,
              }}
            >
              {checking && <ActivityIndicator size="small" color="#FFF" />}
              <Text
                style={{
                  fontFamily: "Geist_500Medium",
                  fontSize: 15,
                  lineHeight: 20,
                  color: "white",
                  textAlign: "center",
                }}
              >
                {checking
                  ? "Checking…"
                  : busy
                  ? "Confirming…"
                    : withdrawal.canClaim &&
                        approval?.approved &&
                        !approval.reason
                  ? "Claim"
                  : "Check status"}
              </Text>
            </Pressable>
            {checkedAt && !error && !actionError && (
              <Text accessibilityLiveRegion="polite" style={styles.subtitle}>
                Last checked {checkedAt.toLocaleTimeString()}.
              </Text>
            )}
          </View>
        </View>
      )}
      {!activity && !error && <ActivityListSkeleton />}
      {activity?.operations.length === 0 && (
        <Text style={[styles.subtitle, { padding: 16 }]}>
          No Earn MAX activity yet.
        </Text>
      )}
      {activity?.operations.map((item, index, operations) => {
        const date = new Date(item.timestamp);
        const showDate =
          index === 0 ||
          new Date(operations[index - 1].timestamp).toDateString() !==
            date.toDateString();
        const positive = item.action === "deposit" || item.action === "claim";
        return (
          <Fragment key={item.id}>
            {showDate && (
              <Text style={styles.dateHeader}>
                {date.toLocaleDateString("en-US", {
                  month: "short",
                  day: "numeric",
                })}
              </Text>
            )}
            <Pressable
              accessibilityRole="link"
              disabled={!item.signature}
              onPress={() => {
                if (item.signature)
                  void Linking.openURL(
                    `https://solscan.io/tx/${encodeURIComponent(
                      item.signature,
                    )}`,
                  );
              }}
              style={styles.row}
            >
              <EarnMaxWithdrawalIcon />
              <View style={{ flex: 1 }}>
                <Text style={styles.title}>
                  {item.action === "deposit"
                    ? "Deposit"
                    : item.action === "withdraw_request"
                    ? "Withdrawal requested"
                    : item.action === "claim"
                    ? "Claimed"
                    : item.action.replaceAll("_", " ")}
                </Text>
                <Text style={styles.subtitle}>
                  {item.action === "deposit"
                    ? "USDC to RWA Loop"
                    : item.action === "claim"
                    ? "USDC to wallet"
                    : "RWA Loop USDC"}
                </Text>
              </View>
              <View style={{ alignItems: "flex-end" }}>
                <Text style={[styles.amount, positive && { color: "#34C759" }]}>
                  {item.amountRaw === null
                    ? "—"
                    : `${positive ? "+" : "−"}${(
                        Number(item.amountRaw) / 1e6
                      ).toLocaleString("en-US", {
                        maximumFractionDigits: 6,
                      })} USDC`}
                </Text>
                <Text style={styles.subtitle}>
                  {date.toLocaleTimeString([], {
                    hour: "numeric",
                    minute: "2-digit",
                  })}
                </Text>
              </View>
            </Pressable>
          </Fragment>
        );
      })}
    </View>
  );
}
const styles = StyleSheet.create({
  row: {
    flexDirection: "row",
    alignItems: "center",
    paddingHorizontal: 16,
    paddingVertical: 6,
    gap: 12,
    minHeight: 60,
  },
  pendingRow: {
    flexDirection: "row",
    alignItems: "flex-start",
    padding: 16,
    gap: 12,
  },
  dateHeader: {
    fontFamily: "Geist_400Regular",
    fontSize: 17,
    lineHeight: 22,
    letterSpacing: -0.187,
    color: "rgba(60,60,67,0.6)",
    paddingHorizontal: 16,
    paddingTop: 13,
    paddingBottom: 9,
  },
  title: {
    fontFamily: "Geist_500Medium",
    fontSize: 17,
    lineHeight: 22,
    color: "#000",
  },
  subtitle: {
    fontFamily: "Geist_400Regular",
    fontSize: 15,
    lineHeight: 20,
    color: "rgba(60,60,67,0.6)",
  },
  amount: {
    fontFamily: "Geist_400Regular",
    fontSize: 17,
    lineHeight: 22,
    color: "#000",
  },
});
