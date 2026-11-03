import type { ReferralFriend, Referrals } from "@loyal-labs/api";
import * as Clipboard from "expo-clipboard";
import * as Haptics from "expo-haptics";
import { useCallback, useEffect, useState } from "react";
import {
  ActivityIndicator,
  FlatList,
  Platform,
  Share,
  StyleSheet,
} from "react-native";
import { useSafeAreaInsets } from "react-native-safe-area-context";

import { Pressable, Text, View } from "@/tw";

import {
  formatDeposited,
  formatJoinDate,
  REFERRAL_COPY,
  STATUS_LABEL,
} from "./copy";
import { useReferrals } from "./use-referrals";

function Summary({ data }: { data: Referrals }) {
  const { joined, deposited, withdrawn } = data.counts;
  const total = joined + deposited + withdrawn;
  if (total === 0) return null;
  return (
    <Text style={styles.summary}>
      {`${total} joined · ${deposited} deposited · ${formatDeposited(data.deposited_micro)} deposited in total`}
    </Text>
  );
}

function CodeBlock({ data }: { data: Referrals }) {
  const [copied, setCopied] = useState(false);

  useEffect(() => {
    if (!copied) return;
    const timer = setTimeout(() => setCopied(false), 1500);
    return () => clearTimeout(timer);
  }, [copied]);

  const copy = useCallback(async () => {
    await Clipboard.setStringAsync(data.code);
    void Haptics.notificationAsync(Haptics.NotificationFeedbackType.Success);
    setCopied(true);
  }, [data.code]);

  const share = useCallback(async () => {
    void Haptics.impactAsync(Haptics.ImpactFeedbackStyle.Light);
    await Share.share(
      Platform.OS === "ios" ? { url: data.link } : { message: data.link },
    );
  }, [data.link]);

  return (
    <View style={styles.codeBlock}>
      <Text style={styles.codeLabel}>{REFERRAL_COPY.yourCode}</Text>
      <Text style={styles.code} selectable>
        {data.code}
      </Text>
      <Text style={styles.link} numberOfLines={1} selectable>
        {data.link}
      </Text>
      <View style={styles.actions}>
        <Pressable onPress={() => void copy()} style={styles.secondaryButton}>
          <Text style={styles.secondaryButtonText}>
            {copied ? REFERRAL_COPY.copied : REFERRAL_COPY.copy}
          </Text>
        </Pressable>
        <Pressable onPress={() => void share()} style={styles.primaryButton}>
          <Text style={styles.primaryButtonText}>{REFERRAL_COPY.share}</Text>
        </Pressable>
      </View>
    </View>
  );
}

function FriendRow({ friend }: { friend: ReferralFriend }) {
  return (
    <View style={styles.row}>
      <View style={styles.rowMain}>
        <Text style={styles.rowLabel} numberOfLines={1}>
          {friend.label}
        </Text>
        <Text style={styles.rowDate}>{formatJoinDate(friend.bound_at)}</Text>
      </View>
      <View style={styles.rowSide}>
        <Text style={styles.rowAmount}>
          {formatDeposited(friend.deposited_micro)}
        </Text>
        <View style={[styles.chip, friend.status === "deposited" && styles.chipActive]}>
          <Text
            style={[
              styles.chipText,
              friend.status === "deposited" && styles.chipTextActive,
            ]}
          >
            {STATUS_LABEL[friend.status]}
          </Text>
        </View>
      </View>
    </View>
  );
}

export function InviteScreen() {
  const insets = useSafeAreaInsets();
  const { data, failed, loading, refresh } = useReferrals();

  return (
    <View className="flex-1 bg-white">
      {data ? (
        <FlatList
          data={data.friends}
          keyExtractor={(friend, index) => `${friend.bound_at}-${index}`}
          renderItem={({ item }) => <FriendRow friend={item} />}
          ListHeaderComponent={
            <View style={styles.header}>
              <CodeBlock data={data} />
              <Summary data={data} />
            </View>
          }
          ListEmptyComponent={
            <Text style={styles.empty}>{REFERRAL_COPY.emptyState}</Text>
          }
          refreshing={loading}
          onRefresh={() => void refresh()}
          contentContainerStyle={{ paddingBottom: Math.max(insets.bottom, 12) + 20 }}
        />
      ) : (
        <View style={styles.center}>
          {failed ? (
            <>
              <Text style={styles.empty}>{REFERRAL_COPY.unavailable}</Text>
              <Pressable onPress={() => void refresh()} style={styles.retryButton}>
                <Text style={styles.secondaryButtonText}>{REFERRAL_COPY.retry}</Text>
              </Pressable>
            </>
          ) : (
            <ActivityIndicator />
          )}
        </View>
      )}
    </View>
  );
}

const styles = StyleSheet.create({
  header: { padding: 16, gap: 16 },
  codeBlock: {
    backgroundColor: "#f2f2f7",
    borderRadius: 20,
    padding: 16,
    gap: 6,
  },
  codeLabel: {
    fontFamily: "Geist_400Regular",
    fontSize: 14,
    color: "rgba(60,60,67,0.6)",
  },
  code: {
    fontFamily: "Geist_600SemiBold",
    fontSize: 32,
    letterSpacing: 3,
    color: "#000",
  },
  link: {
    fontFamily: "Geist_400Regular",
    fontSize: 14,
    color: "rgba(60,60,67,0.6)",
  },
  actions: { flexDirection: "row", gap: 8, marginTop: 8 },
  primaryButton: {
    flex: 1,
    backgroundColor: "#f9363c",
    borderRadius: 12,
    paddingVertical: 12,
    alignItems: "center",
  },
  primaryButtonText: {
    fontFamily: "Geist_500Medium",
    fontSize: 15,
    color: "#fff",
  },
  secondaryButton: {
    flex: 1,
    backgroundColor: "#fff",
    borderRadius: 12,
    paddingVertical: 12,
    alignItems: "center",
  },
  secondaryButtonText: {
    fontFamily: "Geist_500Medium",
    fontSize: 15,
    color: "#000",
  },
  retryButton: {
    backgroundColor: "#f2f2f7",
    borderRadius: 12,
    paddingHorizontal: 24,
    paddingVertical: 12,
  },
  summary: {
    fontFamily: "Geist_500Medium",
    fontSize: 15,
    lineHeight: 20,
    color: "rgba(60,60,67,0.6)",
  },
  row: {
    flexDirection: "row",
    alignItems: "center",
    justifyContent: "space-between",
    paddingHorizontal: 16,
    paddingVertical: 12,
    gap: 12,
  },
  rowMain: { flex: 1, gap: 2 },
  rowLabel: {
    fontFamily: "Geist_500Medium",
    fontSize: 17,
    color: "#000",
  },
  rowDate: {
    fontFamily: "Geist_400Regular",
    fontSize: 13,
    color: "rgba(60,60,67,0.6)",
  },
  rowSide: { alignItems: "flex-end", gap: 4 },
  rowAmount: {
    fontFamily: "Geist_500Medium",
    fontSize: 17,
    color: "#000",
  },
  chip: {
    backgroundColor: "#f2f2f7",
    borderRadius: 10,
    paddingHorizontal: 8,
    paddingVertical: 2,
  },
  chipActive: { backgroundColor: "rgba(52,199,89,0.16)" },
  chipText: {
    fontFamily: "Geist_500Medium",
    fontSize: 12,
    color: "rgba(60,60,67,0.8)",
  },
  chipTextActive: { color: "#1c8a3b" },
  empty: {
    fontFamily: "Geist_400Regular",
    fontSize: 16,
    lineHeight: 22,
    color: "rgba(60,60,67,0.6)",
    textAlign: "center",
    paddingHorizontal: 32,
    paddingVertical: 24,
  },
  center: { flex: 1, alignItems: "center", justifyContent: "center", gap: 12 },
});
