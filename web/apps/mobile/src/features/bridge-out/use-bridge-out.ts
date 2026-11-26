import {
  isBridgeDestinations,
  isBridgeRecipients,
  type BridgeDestinationToken,
} from "@loyal-labs/api";
import {
  useEffect,
  useLayoutEffect,
  useReducer,
  useRef,
  useState,
  useSyncExternalStore,
} from "react";
import { AppState } from "react-native";

import { useWalletUsdcRaw } from "@/hooks/wallet/useWalletUsdcRaw";
import { fundingRequest } from "@/features/receive/funding-api";
import {
  getConnection,
  getSolanaEnv,
  onSolanaEnvChange,
} from "@/lib/solana/rpc/connection";
import { getWalletSigner } from "@/lib/solana/wallet/wallet-details";
import { mmkv } from "@/lib/storage";
import { isWalletRejection } from "@/lib/wallet/rejection";

import { usdcRaw } from "./amount";
import { bridgeRecipientState } from "./recipient-address";
import {
  inFlightTransfer,
  outgoingReceipt,
  QuoteExpired,
  quoteOut,
  readTransfers,
  reconcileReceipt,
  sendReviewedQuote,
  transferStep,
  type OutStep,
  type Request,
  type ReviewedQuote,
} from "./transfer";

export { relayTransactionUrl, type OutStep } from "./transfer";

const sending = new Set<string>();
const message = (error: unknown) =>
  error instanceof Error
    ? error.message
    : "Could not check the bridge. Please try again.";

function useMainnetWallet(wallet: string | null) {
  const env = useSyncExternalStore(
    onSolanaEnvChange,
    getSolanaEnv,
    getSolanaEnv
  );
  return env === "mainnet" ? wallet : null;
}

export function useBridgeUsdcBalance(wallet: string | null) {
  return useWalletUsdcRaw(useMainnetWallet(wallet));
}

function useBridgeRead<T>(
  wallet: string | null,
  path: string | null,
  valid: (body: unknown) => body is T,
  interactive: boolean
) {
  const owner = useMainnetWallet(wallet);
  const key = owner && path ? JSON.stringify([owner, path]) : null;
  const [read, setRead] = useState<{
    key: string;
    value?: T;
    error?: string;
  } | null>(null);
  const [attempt, again] = useReducer((i: number) => i + 1, 0);
  useEffect(() => {
    if (!owner || !path || !key) return;
    const controller = new AbortController();
    setRead(null);
    void fundingRequest<unknown>(owner, path, {
      interactive,
      signal: controller.signal,
    })
      .then((value) => {
        if (!valid(value))
          throw new Error("Could not read the bridge. Please try again.");
        if (!controller.signal.aborted) setRead({ key, value });
      })
      .catch((e) => {
        if (!controller.signal.aborted) setRead({ key, error: message(e) });
      });
    return () => controller.abort();
  }, [owner, path, key, valid, attempt, interactive]);
  const result = read?.key === key ? read : null;
  return {
    value: result?.value,
    error:
      wallet && !owner
        ? "Switch to Solana mainnet to bridge USDC."
        : result?.error ?? null,
    retry: again,
  };
}

export function useBridgeDestinations(walletAddress: string | null) {
  const { value, error, retry } = useBridgeRead(
    walletAddress,
    "/api/funding/bridge/destinations",
    isBridgeDestinations,
    true // The catalog is mounted by the wallet's Bridge tap.
  );
  return {
    destinations: value ? value.destinations ?? [] : null,
    error,
    retry,
  };
}

export function useBridgeRecipients(
  walletAddress: string | null,
  chain: string,
  enabled: boolean
) {
  const path =
    enabled && chain
      ? `/api/funding/bridge/out/recipients?chain=${encodeURIComponent(chain)}`
      : null;
  const { value } = useBridgeRead(walletAddress, path, isBridgeRecipients, false);
  return value ? value.recipients ?? [] : null;
}

type View = {
  owner: string | null;
  step: OutStep;
  checked: boolean;
  notice: string | null;
};
const empty = (owner: string | null): View => ({
  owner,
  step: { kind: "form" },
  checked: false,
  notice: null,
});
type Session = {
  wallet: string;
  signal: AbortSignal;
  receipts: ReturnType<typeof outgoingReceipt>;
  request: Request;
  ignored: Set<string>;
  tracking: string | null;
  revision: number;
  refresh: () => Promise<void>;
};
type QuoteError = { code: string | null; message: string; retryable: boolean };
type QuoteRead = { key: string; review?: ReviewedQuote; error?: QuoteError };

export function useBridgeOut({
  walletAddress,
  catalogPending,
  chain,
  token,
  recipient,
  amountInput,
  balanceRaw,
}: {
  walletAddress: string | null;
  catalogPending: boolean;
  chain: string;
  token: BridgeDestinationToken | null;
  recipient: string;
  amountInput: string;
  balanceRaw: bigint | null;
}) {
  const owner = useMainnetWallet(walletAddress);
  const amountRaw = usdcRaw(amountInput);
  const asset = token?.asset ?? "";
  const insufficient = balanceRaw !== null && amountRaw > balanceRaw;
  const belowMinimum = Boolean(
    token &&
      amountRaw > BigInt(0) &&
      amountRaw < BigInt(token.minUsdc) * BigInt(1_000_000)
  );
  const address = recipient.trim();
  const validAddress =
    bridgeRecipientState(chain === "tron" ? "tron" : "evm", address) ===
    "valid";
  const eligible = Boolean(
    owner &&
      chain &&
      asset &&
      validAddress &&
      amountRaw > BigInt(0) &&
      balanceRaw !== null &&
      !insufficient &&
      !belowMinimum
  );
  const key = JSON.stringify([
    owner,
    chain,
    asset,
    address,
    amountRaw.toString(),
  ]);
  const generation = useRef(0);
  useLayoutEffect(() => {
    generation.current++;
  }, [key, eligible]);
  const active = useRef<Session | null>(null);
  const [view, setView] = useState<View>(() => empty(null));
  const shown = view.owner === owner ? view : empty(owner);

  useLayoutEffect(() => {
    setView(empty(owner));
    if (!owner) return;
    const controller = new AbortController();
    const { signal } = controller;
    const scope: Session = {
      wallet: owner,
      signal,
      receipts: outgoingReceipt(mmkv, owner),
      request: (path, options) => fundingRequest(owner, path, options),
      ignored: new Set(),
      tracking: null,
      revision: 0,
      refresh,
    };
    active.current = scope;
    let checking = false;
    let checked = false;
    async function refresh() {
      if (
        checking ||
        signal.aborted ||
        sending.has(scope.wallet) ||
        AppState.currentState === "background"
      )
        return;
      checking = true;
      const revision = scope.revision;
      const current = () =>
        !signal.aborted &&
        revision === scope.revision &&
        getSolanaEnv() === "mainnet";
      try {
        const receipt = scope.receipts.read();
        if (receipt && current()) {
          scope.tracking = receipt.requestId;
          setView({
            owner,
            checked: false,
            notice: null,
            step: { kind: "bridging", requestId: receipt.requestId },
          });
        }
        // The catalog signs in on Bridge entry. Show a saved receipt while it opens.
        if (catalogPending) return;
        const transfers = await readTransfers(scope.request, signal);
        if (!current()) return;
        const row = scope.tracking
          ? transfers.find((t) => t.requestId === scope.tracking)
          : inFlightTransfer(transfers, scope.ignored);
        const step: OutStep | null = receipt
          ? await reconcileReceipt(
              scope.receipts,
              receipt,
              transfers,
              scope.request,
              getConnection(),
              signal
            )
          : row
          ? transferStep(row)
          : scope.tracking
          ? {
              kind: "failed",
              requestId: scope.tracking,
              message:
                "The transfer did not finish. Check your wallet before trying again.",
            }
          : null;
        if (!current()) return;
        checked = true;
        if (step?.kind === "failed" && step.requestId)
          scope.ignored.add(step.requestId);
        scope.tracking = step?.kind === "bridging" ? step.requestId : null;
        if (step) setView({ owner, checked, step, notice: null });
        else
          setView((prev) => ({
            ...prev,
            checked,
            notice: prev.checked ? prev.notice : null,
          }));
      } catch (e) {
        if (current()) {
          checked = false;
          setView((prev) => ({ ...prev, checked: false, notice: message(e) }));
        }
      } finally {
        checking = false;
      }
    }
    void refresh();
    const timer = setInterval(() => {
      if (scope.tracking || !checked) void refresh();
    }, 4000);
    const foreground = AppState.addEventListener("change", (state) => {
      if (state === "active") void refresh();
    });
    return () => {
      controller.abort();
      clearInterval(timer);
      foreground.remove();
      active.current = null;
    };
  }, [owner, catalogPending]);

  const [attempt, refreshQuote] = useReducer((i: number) => i + 1, 0);
  const authorizeQuote = useRef<string | null>(null);
  function retry() {
    authorizeQuote.current = key;
    refreshQuote();
  }
  const [read, setRead] = useState<QuoteRead | null>(null);
  const form = shown.step.kind === "form";
  useEffect(() => {
    const scope = active.current;
    if (!eligible || !owner || scope?.wallet !== owner || !form) return;
    const controller = new AbortController();
    const { signal } = controller;
    const input = {
      chain,
      asset,
      recipient: address,
      amountRaw: amountRaw.toString(),
    };
    setRead({ key });
    const timer = setTimeout(() => {
      const interactive = authorizeQuote.current === key;
      authorizeQuote.current = null;
      const request: Request = (path, options) =>
        fundingRequest(owner, path, { ...options, interactive });
      void quoteOut(request, owner, input, signal)
        .then((review) => {
          if (!signal.aborted) {
            setRead({ key, review });
            if (interactive) void scope.refresh();
          }
        })
        .catch((e) => {
          if (!signal.aborted)
            setRead({
              key,
              error: {
                code: typeof e?.code === "string" ? e.code : null,
                message: message(e),
                retryable: !e?.status || [401, 429, 503].includes(e.status),
              },
            });
        });
    }, 500);
    return () => {
      controller.abort();
      clearTimeout(timer);
    };
  }, [form, eligible, owner, key, chain, asset, address, amountRaw, attempt]);
  const current = eligible && read?.key === key ? read : null;
  const ready = Boolean(form && current?.review && shown.checked);
  const approved = useRef<ReviewedQuote | null>(null);
  useLayoutEffect(() => {
    approved.current = ready && current?.review ? current.review : null;
  }, [ready, current]);
  async function send() {
    const scope = active.current;
    const review = current?.review;
    if (
      !owner ||
      scope?.wallet !== owner ||
      !ready ||
      !review ||
      approved.current !== review ||
      scope.tracking ||
      sending.has(owner)
    )
      return;
    approved.current = null;
    const at = generation.current;
    const assertCurrent = () => {
      if (
        scope.signal.aborted ||
        at !== generation.current ||
        getSolanaEnv() !== "mainnet"
      )
        throw new Error(
          "The wallet or transfer details changed. Review the transfer again."
        );
    };
    sending.add(owner);
    scope.revision++;
    setView({ owner, checked: false, notice: null, step: { kind: "sending" } });
    try {
      const step = await sendReviewedQuote({
        review,
        request: (path, options) =>
          fundingRequest(owner, path, { ...options, interactive: true }),
        connection: getConnection(),
        currentSigner: getWalletSigner,
        assertCurrent,
        receipts: scope.receipts,
        signal: scope.signal,
        ignored: scope.ignored,
      });
      if (scope.signal.aborted) return;
      scope.tracking = step.kind === "bridging" ? step.requestId : null;
      setView({ owner, checked: false, step, notice: null });
    } catch (e) {
      if (scope.signal.aborted) return;
      // signTransaction cannot broadcast. Only a saved receipt can represent a send.
      scope.ignored.add(review.response.requestId);
      if (e instanceof QuoteExpired || isWalletRejection(e)) {
        setRead(null);
        refreshQuote();
        setView({
          owner,
          checked: true,
          step: { kind: "form" },
          notice:
            e instanceof QuoteExpired ? e.message : "Wallet approval canceled.",
        });
      } else {
        setView({
          owner,
          checked: true,
          step: { kind: "failed", requestId: null, message: message(e) },
          notice: null,
        });
      }
    } finally {
      sending.delete(owner);
      if (!scope.signal.aborted) void scope.refresh();
    }
  }
  return {
    amountRaw,
    insufficient,
    belowMinimum,
    quote: current?.review?.response ?? null,
    ready,
    quoting: eligible && form && !current?.review && !current?.error,
    quoteError:
      current?.error ??
      (form && shown.notice && !shown.checked
        ? { code: null, message: shown.notice, retryable: true }
        : null),
    notice: shown.notice,
    step: shown.step,
    send,
    retry,
  };
}
