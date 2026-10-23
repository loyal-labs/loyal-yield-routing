import {
  earnMaxPending,
  earnMaxPendingMessage,
  earnMaxWithdrawalPending,
} from "@loyal-labs/shared";
import { Fragment, useEffect, useState } from "react";
import { useIsFocused } from "@react-navigation/native";
import {
  ActivityIndicator,
  Linking,
  Pressable,
  StyleSheet,
  Text,
  View,
} from "react-native";
import { EarnMaxWithdrawalIcon } from "./EarnMaxTransferDestination";
import { ActivityListSkeleton } from "@/features/activity/ui/ActivityListSkeleton";
import { useEarnMax } from "./EarnMaxProvider";
export function EarnMaxActivityList() {
  const { summary, activity, error, busy, run, refresh } = useEarnMax();
  const focused = useIsFocused();
  useEffect(() => {
    if (focused) void refresh().catch(() => {});
  }, [focused, refresh]);
  const [actionError, setActionError] = useState<string | null>(null);
  const [checking, setChecking] = useState(false);
  const [checkedAt, setCheckedAt] = useState<Date | null>(null);
  const withdrawal = summary?.withdrawal ?? null;
  const pending = earnMaxPending(withdrawal);
  useEffect(() => {
    setCheckedAt(null);
    setActionError(null);
  }, [withdrawal?.requestId]);
  return (
    <View>
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
              {withdrawal.canClaim
                ? "Ready to claim to your wallet."
                : pending
                ? earnMaxPendingMessage(pending)
                : `Available after ${new Date(
                    withdrawal.readyBy
                  ).toLocaleString()}, subject to vault liquidity.`}
            </Text>
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
                  if (withdrawal.canClaim) await run({ action: "claim" });
                  else {
                    setChecking(true);
                    setCheckedAt(null);
                    if (await refresh()) setCheckedAt(new Date());
                  }
                } catch (e) {
                  setActionError(
                    e instanceof Error
                      ? e.message
                      : "Unable to update withdrawal."
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
                  : withdrawal.canClaim
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
                      item.signature
                    )}`
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
