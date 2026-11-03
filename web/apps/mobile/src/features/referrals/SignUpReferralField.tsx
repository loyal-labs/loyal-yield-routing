import { useEffect, useRef, useState } from "react";
import { StyleSheet } from "react-native";

import { lookupReferralCode } from "@/services/api";
import { Pressable, Text } from "@/tw";

import { REFERRAL_COPY, REFUSAL_COPY } from "./copy";
import { ReferralCodeField } from "./ReferralCodeField";
import { clearPending, parseCode, readPending, storePending } from "./pending-code";

export function SignUpReferralField({ editable }: { editable: boolean }) {
  const [value, setValue] = useState("");
  const [message, setMessage] = useState<string | null>(null);
  const [open, setOpen] = useState(false);
  const [pressed, setPressed] = useState(false);
  const edits = useRef(0);

  useEffect(() => {
    void readPending().then((pending) => {
      if (!pending) return;
      setValue((typed) => typed || pending.code);
      setOpen(true);
    });
  }, []);

  const onChangeText = (next: string) => {
    setValue(next);
    setMessage(null);
    const edit = ++edits.current;
    const code = parseCode(next);
    if (!code) {
      void clearPending();
      return;
    }
    void lookupReferralCode(code).then(async (valid) => {
      if (edits.current !== edit) return;
      if (valid === false) {
        setMessage(REFUSAL_COPY.unknown_code);
        await clearPending();
        return;
      }
      await storePending(code, "typed");
    });
  };

  if (!open) {
    return (
      <Pressable
        onPress={() => {
          setPressed(true);
          setOpen(true);
        }}
        disabled={!editable}
        hitSlop={8}
        style={styles.row}
      >
        <Text style={styles.rowText}>{REFERRAL_COPY.haveCode}</Text>
      </Pressable>
    );
  }

  return (
    <ReferralCodeField
      value={value}
      onChangeText={onChangeText}
      editable={editable}
      message={message}
      autoFocus={pressed}
    />
  );
}

const styles = StyleSheet.create({
  row: { alignSelf: "flex-start", paddingHorizontal: 12, paddingVertical: 6 },
  rowText: {
    fontFamily: "Geist_500Medium",
    fontSize: 14,
    lineHeight: 20,
    color: "rgba(60,60,67,0.6)",
  },
});
