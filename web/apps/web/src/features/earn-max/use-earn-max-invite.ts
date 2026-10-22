"use client";

import { useCallback, useEffect, useState } from "react";

import { createBrowserLifecycleTracker } from "@/features/observability/client";

const INVITE_PATH = "/api/smart-accounts/earn-max/invite";

export type EarnMaxInviteState = {
  redeemed: boolean | null;
  redeem: (code: string) => Promise<"redeemed" | "invalid" | "error">;
};

export function useEarnMaxInvite(walletAddress: string | null): EarnMaxInviteState {
  const [access, setAccess] = useState<{
    walletAddress: string;
    redeemed: boolean;
  } | null>(null);
  const redeemed =
    walletAddress && access?.walletAddress === walletAddress
      ? access.redeemed
      : null;

  useEffect(() => {
    setAccess(null);
    if (!walletAddress) return;
    let cancelled = false;
    void fetch(INVITE_PATH, { cache: "no-store", credentials: "include" })
      .then((response) => (response.ok ? response.json() : null))
      .then((body: { redeemed?: boolean } | null) => {
        if (!cancelled) {
          setAccess({ walletAddress, redeemed: body?.redeemed === true });
        }
      })
      .catch(() => {
        if (!cancelled) setAccess({ walletAddress, redeemed: false });
      });
    return () => {
      cancelled = true;
    };
  }, [walletAddress]);

  const redeem = useCallback(async (code: string) => {
    const tracker = createBrowserLifecycleTracker({
      flowName: "earn_max.invite",
      flowVariant: "redeem",
    });
    tracker.start("redeem");
    try {
      const response = await fetch(INVITE_PATH, {
        body: JSON.stringify({ code }),
        credentials: "include",
        headers: { "content-type": "application/json" },
        method: "POST",
      });
      if (response.ok) {
        tracker.complete("redeem", { httpStatus: response.status });
        setAccess((current) =>
          current?.walletAddress === walletAddress
            ? { ...current, redeemed: true }
            : current
        );
        return "redeemed" as const;
      }
      tracker.fail("redeem", {
        errorCode: "request_failed",
        httpStatus: response.status,
      });
      return response.status === 400 || response.status === 409
        ? ("invalid" as const)
        : ("error" as const);
    } catch {
      tracker.fail("redeem", { errorCode: "request_failed" });
      return "error" as const;
    }
  }, [walletAddress]);

  return { redeem, redeemed };
}
