"use client";

import type { Get } from "@loyal-labs/api";
import { useEffect, useState } from "react";

import { copyTextToClipboard } from "@/components/wallet-workspace/facelift/copy-text";
import { useAuthSession } from "@/contexts/auth-session-context";

export function useInviteLink(enabled: boolean) {
  const { user } = useAuthSession();
  const walletAddress = user?.walletAddress;
  const [result, setResult] = useState<{
    data: Get["/api/referrals"];
    walletAddress: string;
  } | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [copied, setCopied] = useState(false);
  const [attempt, setAttempt] = useState(0);
  const data =
    enabled && result?.walletAddress === walletAddress
      ? result?.data
      : undefined;
  const link = data?.link;

  useEffect(() => {
    setResult(null);
    setError(null);
    setCopied(false);
    if (!enabled || !walletAddress) return;
    const controller = new AbortController();
    fetch("/api/referrals", { cache: "no-store", signal: controller.signal })
      .then(async (response) => {
        if (!response.ok) throw new Error("Could not load invitation link");
        const data: Get["/api/referrals"] = await response.json();
        if (typeof data.link !== "string" || !data.link) {
          throw new Error("Missing invitation link");
        }
        if (!controller.signal.aborted)
          setResult({ data, walletAddress });
      })
      .catch(() => {
        if (!controller.signal.aborted)
          setError("Couldn’t load your invitation link.");
      });
    return () => controller.abort();
  }, [attempt, enabled, walletAddress]);

  useEffect(() => {
    if (!copied) return;
    const timer = setTimeout(() => setCopied(false), 1800);
    return () => clearTimeout(timer);
  }, [copied]);

  async function copy() {
    if (!link) return;
    try {
      if (!(await copyTextToClipboard(link)))
        throw new Error("Clipboard unavailable");
      setError(null);
      setCopied(true);
    } catch {
      setCopied(false);
      setError("Couldn’t copy the link. Please try again.");
    }
  }

  return {
    link,
    rewardsActive: data?.rewards_active === true,
    friends: data?.friends,
    error,
    copied,
    copy,
    retry: () => setAttempt((value) => value + 1),
  };
}
