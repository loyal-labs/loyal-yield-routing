import type { EarnMaxActivity, EarnMaxSummary } from "@loyal-labs/api";
import { earnMaxControls } from "@loyal-labs/shared";
import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useRef,
  useState,
  type ReactNode,
} from "react";
import { mmkv } from "@/lib/storage";
import { AppState } from "react-native";
import {
  executeMobileEarnMax,
  type MobileEarnMaxOperation,
} from "@/lib/solana/earn/mobile-earn-max";
import { deliberateSession } from "@/lib/solana/earn/earn-session";
import { getSolanaEnv } from "@/lib/solana/rpc/connection";
import { isWalletUnlocked, useWallet } from "@/lib/wallet/wallet-provider";
import { fetchEarnMax } from "@/services/api";

const NO_ACTIVITY: EarnMaxActivity = {
  earnings: null,
  operations: [],
  performance: [],
};

function useModel() {
  const { publicKey, signer, state } = useWallet();
  const key =
    isWalletUnlocked(state) && getSolanaEnv() === "mainnet" ? publicKey : null;
  const current = useRef(key);
  current.current = key;
  const signerRef = useRef(signer);
  signerRef.current = signer;
  const [data, setData] = useState<{
    key: string;
    summary: EarnMaxSummary | null;
    activity: EarnMaxActivity;
  } | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const locked = useRef(false);
  const readVersion = useRef(0);
  const [loadedKey, setLoadedKey] = useState<string | null>(null);
  const refresh = useCallback(async (options?: { signIn?: boolean }) => {
    if (!key || getSolanaEnv() !== "mainnet") return false;
    if (options?.signIn && signerRef.current)
      await deliberateSession(signerRef.current);
    const version = ++readVersion.current;
    const isCurrent = () =>
      current.current === key && readVersion.current === version;
    try {
      const [summary, activity] = await Promise.all([
        fetchEarnMax(key, "/api/smart-accounts/earn-max/summary"),
        fetchEarnMax(key, "/api/smart-accounts/earn-max/activity"),
      ]);
      if (isCurrent()) {
        setData({
          key,
          summary: summary?.summary ?? null,
          activity: activity ?? NO_ACTIVITY,
        });
        setError(null);
        return true;
      }
    } catch (e) {
      if (isCurrent()) {
        setError(e instanceof Error ? e.message : "Unable to update Earn MAX.");
      }
    } finally {
      if (isCurrent()) setLoadedKey(key);
    }
    return false;
  }, [key]);
  useEffect(() => {
    readVersion.current += 1;
    setData(null);
    setError(null);
    setLoadedKey(null);
    void refresh();
    const timer = setInterval(() => {
      if (AppState.currentState === "active") void refresh();
    }, 15000);
    const subscription = AppState.addEventListener("change", (value) => {
      if (value === "active") void refresh();
    });
    return () => {
      clearInterval(timer);
      subscription.remove();
    };
  }, [refresh]);
  const run = useCallback(
    async (operation: MobileEarnMaxOperation) => {
      if (!key || !signer)
        throw new Error("Unlock your wallet on mainnet to continue.");
      if (locked.current)
        throw new Error("An Earn MAX transaction is already in progress.");
      locked.current = true;
      setBusy(true);
      try {
        const result = await executeMobileEarnMax(signer, operation);
        if (current.current === key) {
          readVersion.current += 1;
          setData(null);
          await refresh();
        }
        return result;
      } finally {
        locked.current = false;
        setBusy(false);
      }
    },
    [key, signer, refresh]
  );
  const visible = data?.key === key ? data : null;
  const newest = Math.max(
    0,
    ...(visible?.activity.operations ?? []).map(
      (item) => Date.parse(item.timestamp) || 0
    ),
    visible?.summary?.withdrawal?.canClaim
      ? Date.parse(visible.summary.withdrawal.readyBy) || 0
      : 0
  );
  const [seen, setSeen] = useState(0);
  const seenKey = `activity:earn-max:${key}`;
  useEffect(() => {
    setSeen(mmkv.getNumber(seenKey) ?? 0);
  }, [seenKey]);
  const markSeen = useCallback(() => {
    mmkv.setNumber(seenKey, newest);
    setSeen(newest);
  }, [seenKey, newest]);
  return {
    unread: newest > seen,
    markSeen,
    controls: earnMaxControls({
      busy,
      summary: visible?.summary ?? null,
    }),
    summary: visible?.summary ?? null,
    activity: visible?.activity ?? null,
    error,
    busy,
    refresh,
    run,
    supported: !!key,
    hasLoaded: key !== null && loadedKey === key,
  };
}
const Context = createContext<ReturnType<typeof useModel> | null>(null);
export function EarnMaxProvider({ children }: { children: ReactNode }) {
  return <Context.Provider value={useModel()}>{children}</Context.Provider>;
}
export function useEarnMax() {
  const value = useContext(Context);
  if (!value) throw new Error("EarnMaxProvider missing");
  return value;
}
