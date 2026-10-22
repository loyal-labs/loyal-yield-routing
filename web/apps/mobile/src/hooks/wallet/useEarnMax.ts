import {
  isVoltrOperationVisible,
  VoltrClaimLiquidityError,
} from "@loyal-labs/smart-account-vaults";
import { useCallback, useEffect, useRef, useState } from "react";

import {
  EarnMaxWalletChangedError,
  redeemEarnMaxInvite,
  resolveEarnMaxAccount,
  sendEarnMaxVoltr,
  type EarnMaxVoltrAction,
} from "@/lib/solana/earn/earn-max";
import {
  connectEarnMax,
  type EarnMaxRead,
  readEarnMax,
} from "@/lib/solana/earn/earn-max-read";
import type { Signer } from "@/lib/wallet/signer";

type PendingOperation = {
  signature: string;
  walletAddress: string;
  expiresAt: number;
};

const PENDING_VISIBILITY_MS = 60_000;
const PENDING_POLL_MS = 3_000;

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
  const generationRef = useRef(0);
  if (accountRef.current !== walletAddress) {
    accountRef.current = walletAddress;
    generationRef.current += 1;
  }
  const busyRef = useRef(false);

  const read =
    walletAddress && loaded?.walletAddress === walletAddress
      ? loaded.read
      : null;

  const refresh = useCallback(async () => {
    if (!walletAddress) {
      setLoaded(null);
      return;
    }
    const generation = generationRef.current;
    const next = await readEarnMax(walletAddress);
    if (generationRef.current !== generation) {
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
    const timer = setInterval(() => void refresh(), PENDING_POLL_MS);
    return () => clearInterval(timer);
  }, [isPending, refresh]);

  useEffect(() => {
    const mine = pending.filter((item) => item.walletAddress === walletAddress);
    if (mine.length === 0) {
      return;
    }
    const nextExpiry = Math.min(...mine.map((item) => item.expiresAt));
    const timer = setTimeout(() => {
      const live = pending.filter((item) => item.expiresAt > Date.now());
      setPending(live);
      if (!live.some((item) => item.walletAddress === walletAddress)) {
        void refresh();
      }
    }, Math.max(0, nextExpiry - Date.now()));
    return () => clearTimeout(timer);
  }, [pending, refresh, walletAddress]);

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
      if (busyRef.current) {
        return false;
      }
      busyRef.current = true;
      setIsBusy(true);
      setActionError(null);
      const generation = generationRef.current;
      const assertActive = () => {
        if (generationRef.current !== generation) {
          throw new EarnMaxWalletChangedError();
        }
      };
      try {
        const account = await resolveEarnMaxAccount(walletAddress, config);
        const sent = await sendEarnMaxVoltr({
          account,
          action,
          assertActive,
          signer,
        });
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
        busyRef.current = false;
        setIsBusy(false);
      }
    },
    [config, refresh, signer, walletAddress],
  );

  const connect = useCallback(async () => {
    if (
      !(
        walletAddress &&
        signer &&
        signer.publicKey.toBase58() === walletAddress
      )
    ) {
      return;
    }
    const generation = generationRef.current;
    const next = await connectEarnMax(walletAddress, signer);
    if (generationRef.current === generation) {
      setLoaded({ walletAddress, read: next });
    }
  }, [signer, walletAddress]);

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
    connect,
    redeem,
    deposit: (amountRaw: bigint) => run({ kind: "deposit", amountRaw }),
    requestWithdrawal: (amountRaw: bigint | "max") =>
      run({ kind: "request_withdrawal", amountRaw }),
    claim: () => run({ kind: "claim" }),
  };
}
