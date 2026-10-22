import * as Haptics from "expo-haptics";
import { useCallback } from "react";
import { Pressable, StyleSheet, Text, View } from "react-native";

import type { ActivitySection } from "../model/activity-unread";

const SEGMENTS: { key: ActivitySection; label: string }[] = [
  { key: "earn", label: "Earn" },
  { key: "earn-max", label: "Earn MAX" },
  { key: "wallet", label: "Wallet" },
];

export function ActivitySegmentedControl({
  value,
  onChange,
  earnUnread,
  earnMaxUnread,
  walletUnread,
}: {
  value: ActivitySection;
  onChange: (section: ActivitySection) => void;
  earnUnread: boolean;
  earnMaxUnread: boolean;
  walletUnread: boolean;
}) {
  const handleSelect = useCallback(
    (section: ActivitySection) => {
      if (section === value) return;
      if (process.env.EXPO_OS !== "web") {
        void Haptics.selectionAsync();
      }
      onChange(section);
    },
    [value, onChange]
  );

  return (
    <View style={styles.track}>
      {SEGMENTS.map(({ key, label }) => {
        const active = value === key;
        const unread =
          key === "earn"
            ? earnUnread
            : key === "wallet"
            ? walletUnread
            : earnMaxUnread;
        return (
          <Pressable
            key={key}
            onPress={() => handleSelect(key)}
            accessibilityRole="tab"
            accessibilityState={{ selected: active }}
            accessibilityLabel={label}
            style={[styles.segment, active && styles.segmentActive]}
          >
            <Text
              style={[styles.segmentText, active && styles.segmentTextActive]}
            >
              {label}
            </Text>
            {unread ? <View style={styles.badge} /> : null}
          </Pressable>
        );
      })}
    </View>
  );
}

const styles = StyleSheet.create({
  track: {
    flexDirection: "row",
    alignItems: "center",
    padding: 4,
    borderRadius: 9999,
    backgroundColor: "#F2F2F7",
  },
  segment: {
    flex: 1,
    flexDirection: "row",
    alignItems: "center",
    justifyContent: "center",
    gap: 4,
    paddingHorizontal: 16,
    paddingVertical: 8,
    borderRadius: 9999,
  },
  segmentActive: {
    backgroundColor: "#FFFFFF",
  },
  segmentText: {
    fontFamily: "Geist_500Medium",
    fontSize: 15,
    lineHeight: 20,
    color: "rgba(60, 60, 67, 0.6)",
    textAlign: "center",
  },
  segmentTextActive: {
    color: "#000000",
  },
  badge: {
    width: 6,
    height: 6,
    borderRadius: 3,
    backgroundColor: "#F9363C",
  },
});
