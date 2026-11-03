import type { Referrals } from "@loyal-labs/api";
import { useCallback, useEffect, useRef, useState } from "react";

import { isWalletUnlocked, useWallet } from "@/lib/wallet/wallet-provider";
import { fetchReferrals } from "@/services/api";

export function useReferrals() {
  const { publicKey, state } = useWallet();
  const wallet = isWalletUnlocked(state) ? publicKey : null;
  const current = useRef(wallet);
  current.current = wallet;
  const [data, setData] = useState<Referrals | null>(null);
  const [failed, setFailed] = useState(false);
  const [loading, setLoading] = useState(false);

  const refresh = useCallback(async () => {
    if (!wallet) return;
    setLoading(true);
    try {
      const next = await fetchReferrals(wallet);
      if (current.current === wallet) {
        setData(next);
        setFailed(false);
      }
    } catch {
      if (current.current === wallet) setFailed(true);
    } finally {
      if (current.current === wallet) setLoading(false);
    }
  }, [wallet]);

  useEffect(() => {
    setData(null);
    setFailed(false);
    void refresh();
  }, [refresh]);

  return { data, failed, loading, refresh };
}
