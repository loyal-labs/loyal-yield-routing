import { useFocusEffect } from "expo-router";
import { useCallback, useEffect, useRef } from "react";
import { AppState, type AppStateStatus } from "react-native";

export type WalletRefreshReason =
  | "app-foreground"
  | "screen-focus"
  | "push-received"
  | "interval"
  | "mutation"
  | "network-switch"
  | "manual";

type RefreshFn = (reason: WalletRefreshReason) => Promise<void> | void;

type Options = {
  walletAddress: string | null;
  refresh: RefreshFn;
  intervalMs?: number;
  minIntervalMs?: number;
};

const BYPASS_THROTTLE_REASONS: ReadonlySet<WalletRefreshReason> = new Set([
  "manual",
  "mutation",
  "network-switch",
]);

export function useWalletAutoRefresh({
  walletAddress,
  refresh,
  intervalMs = 60_000,
  minIntervalMs = 3_000,
}: Options): {
  requestRefresh: RefreshFn;
} {
  const refreshRef = useRef<RefreshFn>(refresh);
  refreshRef.current = refresh;

  const inFlightRef = useRef(false);
  const pendingReasonRef = useRef<WalletRefreshReason | null>(null);
  const lastRefreshAtRef = useRef(0);

  const requestRefresh = useCallback<RefreshFn>(async (reason) => {
    if (!walletAddress) return;

    if (inFlightRef.current) {
      const current = pendingReasonRef.current;
      if (!current || BYPASS_THROTTLE_REASONS.has(reason)) {
        pendingReasonRef.current = reason;
      }
      return;
    }

    const now = Date.now();
    if (
      !BYPASS_THROTTLE_REASONS.has(reason) &&
      now - lastRefreshAtRef.current < minIntervalMs
    ) {
      return;
    }

    inFlightRef.current = true;
    lastRefreshAtRef.current = now;
    try {
      await refreshRef.current(reason);
    } catch (error) {
      console.error(`[wallet-auto-refresh] ${reason} refresh failed`, error);
    } finally {
      inFlightRef.current = false;
      const pending = pendingReasonRef.current;
      if (pending) {
        pendingReasonRef.current = null;
        void requestRefresh(pending);
      }
    }
  }, [walletAddress, minIntervalMs]);

  useEffect(() => {
    if (!walletAddress) return;
    let previous: AppStateStatus = AppState.currentState;
    const sub = AppState.addEventListener("change", (next) => {
      if (next === "active" && previous !== "active") {
        void requestRefresh("app-foreground");
      }
      previous = next;
    });
    return () => sub.remove();
  }, [walletAddress, requestRefresh]);

  useFocusEffect(
    useCallback(() => {
      void requestRefresh("screen-focus");
      return undefined;
    }, [requestRefresh]),
  );

  useEffect(() => {
    if (!walletAddress) return;
    let cancelled = false;
    let remove: (() => void) | null = null;
    void (async () => {
      try {
        const Notifications = await import("expo-notifications");
        if (cancelled) return;
        const sub = Notifications.addNotificationReceivedListener(() => {
          void requestRefresh("push-received");
        });
        remove = () => sub.remove();
      } catch {
        // expo-notifications unavailable (Expo Go) — silent.
      }
    })();
    return () => {
      cancelled = true;
      remove?.();
    };
  }, [walletAddress, requestRefresh]);

  useEffect(() => {
    if (!walletAddress) return;
    const handle = setInterval(() => {
      if (AppState.currentState !== "active") return;
      void requestRefresh("interval");
    }, intervalMs);
    return () => clearInterval(handle);
  }, [walletAddress, requestRefresh, intervalMs]);

  return { requestRefresh };
}
