import {
  isVoltrOperationVisible,
  VoltrClaimLiquidityError,
} from "@loyal-labs/smart-account-vaults";
import { useCallback, useEffect, useRef, useState } from "react";

import {
  EarnMaxError,
  fetchEarnMaxActivity,
  fetchEarnMaxInvite,
  fetchEarnMaxSummary,
  redeemEarnMaxInvite,
  resolveEarnMaxAccount,
  sendEarnMaxVoltr,
  type EarnMaxActivityResponse,
  type EarnMaxSummaryResponse,
  type EarnMaxVoltrAction,
} from "@/lib/solana/earn/earn-max";
import type { Signer } from "@/lib/wallet/signer";

export type EarnMaxRead =
  | { kind: "invite_required" }
  | {
      kind: "ready";
      summary: EarnMaxSummaryResponse;
      activity: EarnMaxActivityResponse;
    }
  | { kind: "failed"; message: string };

type PendingOperation = {
  signature: string;
  walletAddress: string;
  expiresAt: number;
};

const PENDING_VISIBILITY_MS = 60_000;
const PENDING_POLL_MS = 3_000;

async function readEarnMax(walletAddress: string): Promise<EarnMaxRead> {
  try {
    const invite = await fetchEarnMaxInvite(walletAddress);
    if (!invite.redeemed) {
      return { kind: "invite_required" };
    }
    const [summary, activity] = await Promise.all([
      fetchEarnMaxSummary(walletAddress),
      fetchEarnMaxActivity(walletAddress),
    ]);
    return { kind: "ready", summary, activity };
  } catch (error) {
    if (error instanceof EarnMaxError && error.code === "invite_required") {
      return { kind: "invite_required" };
    }
    return {
      kind: "failed",
      message:
        error instanceof Error ? error.message : "Earn MAX failed to load.",
    };
  }
}

export function useEarnMax(input: {
  walletAddress: string | null;
  signer: Signer | null;
}) {
  const { walletAddress, signer } = input;
  const [loaded, setLoaded] = useState<{
    walletAddress: string;
    read: EarnMaxRead;
  } | null>(null);
  const [pending, setPending] = useState<readonly PendingOperation[]>([]);
  const [isBusy, setIsBusy] = useState(false);
  const [actionError, setActionError] = useState<string | null>(null);

  const accountRef = useRef<string | null>(null);
  accountRef.current = walletAddress;

  const read =
    walletAddress && loaded?.walletAddress === walletAddress
      ? loaded.read
      : null;

  const refresh = useCallback(async () => {
    if (!walletAddress) {
      return;
    }
    const next = await readEarnMax(walletAddress);
    if (accountRef.current !== walletAddress) {
      return;
    }
    setLoaded({ walletAddress, read: next });
    if (next.kind === "ready") {
      setPending((items) =>
        items.filter(
          (item) =>
            item.walletAddress === walletAddress &&
            !isVoltrOperationVisible(
              item.signature,
              next.summary.summary,
              next.activity.operations,
            ),
        ),
      );
    }
  }, [walletAddress]);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  const isPending = pending.some(
    (item) => item.walletAddress === walletAddress,
  );
  useEffect(() => {
    if (!isPending) {
      return;
    }
    const timer = setInterval(() => {
      setPending((items) => items.filter((item) => item.expiresAt > Date.now()));
      void refresh();
    }, PENDING_POLL_MS);
    return () => clearInterval(timer);
  }, [isPending, refresh]);

  const withdrawal =
    read?.kind === "ready" ? read.summary.summary?.withdrawal : undefined;
  const readyBy = withdrawal?.readyBy;
  const canClaim = withdrawal?.canClaim;
  useEffect(() => {
    if (!readyBy || canClaim) {
      return;
    }
    const delay = Math.max(0, Date.parse(readyBy) - Date.now()) + 2_000;
    const timer = setTimeout(() => void refresh(), delay);
    return () => clearTimeout(timer);
  }, [canClaim, readyBy, refresh]);

  const config = read?.kind === "ready" ? read.summary.config : null;
  const run = useCallback(
    async (action: EarnMaxVoltrAction): Promise<boolean> => {
      if (
        !(walletAddress && signer && config) ||
        signer.publicKey.toBase58() !== walletAddress
      ) {
        setActionError("Connect the authenticated wallet to use Earn MAX.");
        return false;
      }
      setIsBusy(true);
      setActionError(null);
      try {
        const account = await resolveEarnMaxAccount(walletAddress, config);
        const sent = await sendEarnMaxVoltr({ account, action, signer });
        setPending((items) => [
          ...items,
          {
            signature: sent.signature,
            walletAddress,
            expiresAt: Date.now() + PENDING_VISIBILITY_MS,
          },
        ]);
        await refresh();
        return true;
      } catch (error) {
        if (error instanceof VoltrClaimLiquidityError) {
          await refresh();
        }
        if (accountRef.current === walletAddress) {
          setActionError(
            error instanceof Error
              ? error.message
              : "Earn MAX transaction failed.",
          );
        }
        return false;
      } finally {
        setIsBusy(false);
      }
    },
    [config, refresh, signer, walletAddress],
  );

  const redeem = useCallback(
    async (code: string): Promise<"redeemed" | "invalid" | "error"> => {
      if (!(signer && signer.publicKey.toBase58() === walletAddress)) {
        return "error";
      }
      try {
        const result = await redeemEarnMaxInvite(signer, code);
        if (result === "redeemed") {
          await refresh();
        }
        return result;
      } catch {
        return "error";
      }
    },
    [refresh, signer, walletAddress],
  );

  return {
    read,
    isBusy,
    isPending,
    actionError,
    refresh,
    redeem,
    deposit: (amountRaw: bigint) => run({ kind: "deposit", amountRaw }),
    requestWithdrawal: (amountRaw: bigint | "max") =>
      run({ kind: "request_withdrawal", amountRaw }),
    claim: () => run({ kind: "claim" }),
  };
}
