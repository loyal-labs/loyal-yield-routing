import { useCallback, useEffect, useRef } from "react";
import { AppState } from "react-native";

import { isWalletUnlocked, useWallet } from "@/lib/wallet/wallet-provider";
import { claimReferral } from "@/services/api";

import { referralsEnabled } from "./enabled";
import { clearPending, readPending, subscribePending } from "./pending-code";

export function useClaimPendingCode(): void {
  const { state, publicKey } = useWallet();
  const wallet = isWalletUnlocked(state) ? publicKey : null;
  const current = useRef(wallet);
  current.current = wallet;
  const running = useRef(false);
  const again = useRef(false);

  const run = useCallback(async () => {
    if (!referralsEnabled()) return;
    if (running.current) {
      again.current = true;
      return;
    }
    running.current = true;
    try {
      do {
        again.current = false;
        const claimant = current.current;
        if (!claimant) break;
        const pending = await readPending();
        if (!pending) break;
        const answer = await claimReferral(claimant, pending);
        if (!answer.answered) break;
        if (current.current !== claimant) continue;
        await clearPending(pending);
      } while (again.current);
    } finally {
      running.current = false;
    }
  }, []);

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
  }, [run, wallet]);
}
