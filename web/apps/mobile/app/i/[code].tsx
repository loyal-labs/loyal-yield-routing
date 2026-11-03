import { Redirect, useLocalSearchParams } from "expo-router";
import { useEffect } from "react";

import { storePending } from "@/features/referrals/pending-code";

export default function InviteLink() {
  const { code } = useLocalSearchParams<{ code: string }>();

  useEffect(() => {
    if (code) void storePending(code, "link");
  }, [code]);

  return <Redirect href="/" />;
}
