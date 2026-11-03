import { useState } from "react";
import { ActivityIndicator, StyleSheet } from "react-native";

import { useWallet } from "@/lib/wallet/wallet-provider";
import { claimReferral } from "@/services/api";
import { Pressable, Text, View } from "@/tw";

import { formatClosesAt, REFERRAL_COPY, REFUSAL_COPY } from "./copy";
import { parseCode } from "./pending-code";
import { ReferralCodeField } from "./ReferralCodeField";
import { useReferrals } from "./use-referrals";

export function ReferralClaimSetting() {
  const { publicKey } = useWallet();
  const { data, refresh } = useReferrals();
  const [value, setValue] = useState("");
  const [message, setMessage] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [joined, setJoined] = useState(false);

  const code = parseCode(value);

  const apply = async () => {
    if (!code || !publicKey || busy) return;
    setBusy(true);
    setMessage(null);
    const answer = await claimReferral(publicKey, { code, source: "typed" });
    setBusy(false);
    if (!answer.answered) {
      setMessage(REFERRAL_COPY.unavailable);
    } else if (answer.result.bound) {
      setJoined(true);
      void refresh();
    } else if (answer.result.refusal) {
      setMessage(REFUSAL_COPY[answer.result.refusal]);
    }
  };

  if (joined) {
    return (
      <View style={styles.wrap}>
        <Text style={styles.title}>{REFERRAL_COPY.joinedAnInvite}</Text>
      </View>
    );
  }
  if (!data?.can_claim) return null;

  return (
    <View style={styles.wrap}>
      <Text style={styles.title}>{REFERRAL_COPY.settingsTitle}</Text>
      <ReferralCodeField
        value={value}
        onChangeText={(next) => {
          setValue(next);
          setMessage(null);
        }}
        editable={!busy}
        message={message}
        detail={formatClosesAt(data.closes_at)}
      />
      <Pressable
        onPress={() => void apply()}
        disabled={!code || busy}
        style={[styles.button, (!code || busy) && styles.buttonDisabled]}
      >
        {busy ? (
          <ActivityIndicator color="#fff" />
        ) : (
          <Text style={styles.buttonText}>{REFERRAL_COPY.settingsApply}</Text>
        )}
      </Pressable>
    </View>
  );
}

const styles = StyleSheet.create({
  wrap: { paddingHorizontal: 16, paddingVertical: 12, gap: 10 },
  title: {
    fontFamily: "Geist_500Medium",
    fontSize: 17,
    lineHeight: 22,
    color: "#000",
  },
  button: {
    backgroundColor: "#f9363c",
    borderRadius: 12,
    paddingVertical: 10,
    alignItems: "center",
  },
  buttonDisabled: { opacity: 0.5 },
  buttonText: {
    fontFamily: "Geist_500Medium",
    fontSize: 15,
    lineHeight: 22,
    color: "#fff",
  },
});
