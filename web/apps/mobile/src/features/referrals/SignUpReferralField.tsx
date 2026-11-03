import { useEffect, useRef, useState } from "react";

import { lookupReferralCode } from "@/services/api";

import { REFUSAL_COPY } from "./copy";
import { ReferralCodeField } from "./ReferralCodeField";
import { clearPending, parseCode, readPending, storePending } from "./pending-code";

export function SignUpReferralField({ editable }: { editable: boolean }) {
  const [value, setValue] = useState("");
  const [message, setMessage] = useState<string | null>(null);
  const edits = useRef(0);

  useEffect(() => {
    void readPending().then((pending) => {
      if (pending) setValue((typed) => typed || pending.code);
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

  return (
    <ReferralCodeField
      value={value}
      onChangeText={onChangeText}
      editable={editable}
      message={message}
    />
  );
}
