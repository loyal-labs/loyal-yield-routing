"use client";

import { useCallback, useEffect, useRef, useState } from "react";
import { useAuthSession } from "@/contexts/auth-session-context";
import type { EarnPositionData } from "./use-earn-position-data";

type Compatibility = {
  account: string;
  seed: string;
  generation: "compatible" | "legacy_token_program";
};

export function usePolicyUpdates(data: EarnPositionData) {
  const { user } = useAuthSession();
  const fetchCompatibility = data.smartAccountData.fetchEarnPolicyCompatibility;
  const scope = user?.settingsPda ?? null;
  const scopeRef = useRef(scope);
  scopeRef.current = scope;
  const requestRef = useRef(0);
  const updateLock = useRef(false);
  const [result, setResult] = useState<{
    scope: string;
    policy: Compatibility | null;
  } | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [isLoading, setIsLoading] = useState(false);
  const [isUpdating, setIsUpdating] = useState(false);
  const refresh = useCallback(async () => {
    if (!scope) return;
    const request = ++requestRef.current;
    setIsLoading(true);
    setError(null);
    try {
      const policy = await fetchCompatibility();
      if (scopeRef.current === scope && requestRef.current === request) {
        setResult({ scope, policy });
        return policy;
      }
    } catch (cause) {
      if (scopeRef.current === scope && requestRef.current === request) {
        setResult(null);
        setError(
          cause instanceof Error
            ? cause.message
            : "Could not check policy updates."
        );
      }
    } finally {
      if (scopeRef.current === scope && requestRef.current === request)
        setIsLoading(false);
    }
  }, [scope, fetchCompatibility]);
  useEffect(() => {
    setResult(null);
    setError(null);
    setIsUpdating(false);
    updateLock.current = false;
    void refresh();
    return () => {
      requestRef.current += 1;
    };
  }, [refresh]);
  const policy = result?.scope === scope ? result.policy : null;
  const requiredAccount =
    policy?.generation === "legacy_token_program" ? policy.account : null;
  const update = async () => {
    if (
      !scope ||
      !requiredAccount ||
      updateLock.current ||
      !data.actions.ensureCanSignAccountAction()
    )
      return;
    updateLock.current = true;
    setIsUpdating(true);
    setError(null);
    try {
      const current = await refresh();
      if (
        scopeRef.current !== scope ||
        current?.generation !== "legacy_token_program"
      )
        return;
      const outcome = await data.smartAccountData.executeEarnPolicySetup({
        force: true,
      });
      if (scopeRef.current !== scope) return;
      if (!outcome.success)
        throw new Error(outcome.error ?? "Policy update was not completed.");
      await data.smartAccountData.refreshAfterTx({
        accountIndex: 1,
        groups: ["policies"],
      });
      await refresh();
    } catch (cause) {
      if (scopeRef.current === scope)
        setError(
          cause instanceof Error ? cause.message : "Policy update failed."
        );
    } finally {
      if (scopeRef.current === scope) {
        updateLock.current = false;
        setIsUpdating(false);
      }
    }
  };
  return {
    requiredAccount,
    earnCount: requiredAccount ? 1 : 0,
    earnMaxCount: 0,
    error,
    isLoading,
    isUpdating,
    refresh,
    update,
  };
}

export type PolicyUpdates = ReturnType<typeof usePolicyUpdates>;
