"use client";

import type { Get } from "@loyal-labs/api";
import { useEffect, useState } from "react";

export type EarnMaxApy = Get["/api/earn-max/apy"];

const REFRESH_MS = 60_000;
const FAILURE_BACKOFF_MS = 30_000;

let cached: { fetchedAt: number; value: EarnMaxApy } | null = null;
let inflight: Promise<EarnMaxApy> | null = null;
let failedAt = 0;

async function fetchEarnMaxApy(): Promise<EarnMaxApy> {
  const now = Date.now();
  if (cached && now - cached.fetchedAt < REFRESH_MS) {
    return cached.value;
  }
  if (inflight) {
    return inflight;
  }
  if (now - failedAt < FAILURE_BACKOFF_MS) {
    throw new Error("Earn MAX APY request failed recently");
  }
  inflight = fetch("/api/earn-max/apy", {
    cache: "no-store",
    signal: AbortSignal.timeout(30_000),
  })
    .then(async (response) => {
      if (!response.ok) {
        throw new Error(`Earn MAX APY request failed: ${response.status}`);
      }
      return (await response.json()) as EarnMaxApy;
    })
    .then((value) => {
      cached = { fetchedAt: Date.now(), value };
      return value;
    })
    .catch((error: unknown) => {
      failedAt = Date.now();
      throw error;
    })
    .finally(() => {
      inflight = null;
    });
  return inflight;
}

export function useEarnMaxApy({
  enabled = true,
}: {
  enabled?: boolean;
} = {}): {
  apy: EarnMaxApy | null;
  isLoaded: boolean;
} {
  const [state, setState] = useState<{
    apy: EarnMaxApy | null;
    isLoaded: boolean;
  }>({ apy: cached?.value ?? null, isLoaded: cached !== null });

  useEffect(() => {
    if (!enabled) {
      return;
    }
    let mounted = true;
    const refresh = () => {
      fetchEarnMaxApy().then(
        (apy) => mounted && setState({ apy, isLoaded: true }),
        () => mounted && setState((s) => ({ ...s, isLoaded: true }))
      );
    };
    refresh();
    const interval = window.setInterval(refresh, REFRESH_MS);
    return () => {
      mounted = false;
      window.clearInterval(interval);
    };
  }, [enabled]);

  return state;
}
