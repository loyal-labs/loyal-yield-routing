import { StyleSheet, TextInput } from "react-native";

import { Text, View } from "@/tw";

import { REFERRAL_COPY } from "./copy";

const CODE_LENGTH = 7;

export function sanitizeCodeInput(text: string): string {
  return text.toUpperCase().replace(/[^0-9A-Z]/g, "").slice(0, CODE_LENGTH);
}

type Props = {
  value: string;
  onChangeText: (value: string) => void;
  editable: boolean;
  message: string | null;
  detail?: string;
  autoFocus?: boolean;
};

export function ReferralCodeField({
  value,
  onChangeText,
  editable,
  message,
  detail,
  autoFocus,
}: Props) {
  return (
    <View style={styles.wrap}>
      <TextInput
        style={styles.input}
        value={value}
        onChangeText={(text) => onChangeText(sanitizeCodeInput(text))}
        placeholder={REFERRAL_COPY.fieldPlaceholder}
        placeholderTextColor="rgba(0,0,0,0.3)"
        autoCapitalize="characters"
        autoCorrect={false}
        editable={editable}
        autoFocus={autoFocus}
        maxLength={CODE_LENGTH}
        accessibilityLabel={REFERRAL_COPY.fieldLabel}
      />
      <Text style={styles.note}>{REFERRAL_COPY.depositVisibility}</Text>
      {detail ? <Text style={styles.note}>{detail}</Text> : null}
      {message ? <Text style={styles.error}>{message}</Text> : null}
    </View>
  );
}

const styles = StyleSheet.create({
  wrap: { gap: 6 },
  input: {
    fontFamily: "Geist_500Medium",
    fontSize: 17,
    letterSpacing: 2,
    color: "#000",
    backgroundColor: "rgba(0,0,0,0.04)",
    borderRadius: 16,
    height: 48,
    paddingHorizontal: 16,
  },
  note: {
    fontFamily: "Geist_400Regular",
    fontSize: 14,
    lineHeight: 18,
    color: "rgba(60,60,67,0.6)",
  },
  error: {
    fontFamily: "Geist_500Medium",
    fontSize: 13,
    color: "#FF3B30",
  },
});
