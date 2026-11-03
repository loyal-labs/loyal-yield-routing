import { useCallback, useEffect, useRef } from "react";
import { AppState } from "react-native";

import { isWalletUnlocked, useWallet } from "@/lib/wallet/wallet-provider";
import { claimReferral } from "@/services/api";

import { clearPending, readPending, subscribePending } from "./pending-code";

const RETRY_DELAY_MS = 5_000;
const MAX_RETRIES = 6;

export function useClaimPendingCode(): void {
  const { publicKey, state } = useWallet();
  const wallet = isWalletUnlocked(state) ? publicKey : null;
  const running = useRef(false);
  const again = useRef(false);
  const retries = useRef(0);
  const retryTimer = useRef<ReturnType<typeof setTimeout> | null>(null);

  const run = useCallback(async () => {
    if (!wallet) return;
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
        if (!answer.answered) {
          if (retries.current++ < MAX_RETRIES) {
            if (retryTimer.current) clearTimeout(retryTimer.current);
            retryTimer.current = setTimeout(() => void run(), RETRY_DELAY_MS);
          }
          break;
        }
        retries.current = 0;
        await clearPending(pending);
      } while (again.current);
    } finally {
      running.current = false;
    }
  }, [wallet]);

  useEffect(() => {
    retries.current = 0;
    void run();
    const unsubscribe = subscribePending(() => {
      retries.current = 0;
      void run();
    });
    const appState = AppState.addEventListener("change", (next) => {
      if (next !== "active") return;
      retries.current = 0;
      void run();
    });
    return () => {
      unsubscribe();
      appState.remove();
      if (retryTimer.current) clearTimeout(retryTimer.current);
    };
  }, [run]);
}
