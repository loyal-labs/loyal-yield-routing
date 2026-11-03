import { useCallback, useEffect, useRef } from "react";
import { AppState } from "react-native";

import { useWallet } from "@/lib/wallet/wallet-provider";
import { claimReferral } from "@/services/api";

import { referralsEnabled } from "./enabled";
import { clearPending, readPending, subscribePending } from "./pending-code";

export function useClaimPendingCode(): void {
  const { publicKey, appUserReady } = useWallet();
  const wallet = appUserReady ? publicKey : null;
  const running = useRef(false);
  const again = useRef(false);

  const run = useCallback(async () => {
    if (!wallet || !referralsEnabled()) return;
    if (running.current) {
      again.current = true;
      return;
    }
    running.current = true;
    try {
      do {
        again.current = false;
        const pending = await readPending();
        if (!pending) break;
        const answer = await claimReferral(wallet, pending);
        if (!answer.answered) break;
        await clearPending(pending);
      } while (again.current);
    } finally {
      running.current = false;
    }
  }, [wallet]);

  useEffect(() => {
    void run();
    const unsubscribe = subscribePending(() => void run());
    const appState = AppState.addEventListener("change", (next) => {
      if (next === "active") void run();
    });
    return () => {
      unsubscribe();
      appState.remove();
    };
  }, [run]);
}
