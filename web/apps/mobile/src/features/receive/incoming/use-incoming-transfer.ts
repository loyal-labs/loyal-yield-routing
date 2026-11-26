import {
  emptyIncoming,
  executeIncomingQuote,
  finishIncomingDeposit,
  incomingItems,
  prepareIncomingMove,
  readIncomingBalances,
  readIncomingTransfers,
  reduceIncoming,
  settleWithin,
  type BridgeTransfer,
  type EvmDepositAddressResponse,
  type IncomingItem,
  type IncomingRequest,
  type MoveOutcome,
  type ReviewedMove,
} from "@loyal-labs/api";
import type { ConnectedEthereumWallet, usePrivyClient } from "@privy-io/expo";
import { useCallback, useEffect, useLayoutEffect, useMemo, useReducer, useRef, useState } from "react";
import { AppState } from "react-native";

import { fundingRequest } from "@/features/receive/funding-api";
import { getSolanaEnv, onSolanaEnvChange } from "@/lib/solana/rpc/connection";
import { getWalletSigner } from "@/lib/solana/wallet/wallet-details";
import { mmkv } from "@/lib/storage";

import {
  isExternalOwner,
  linkedSender,
  receiptProvider,
  sendSponsoredCall,
  validateQuote,
} from "./privy-transfer";
import { receiptStore, recordedReceipt } from "./receipt";

const sending = new Set<string>();
const routePath = "/api/funding/evm-deposit-address";

type Identity = {
  solana: string;
  userId: string;
  senderId: string;
  wallet: ConnectedEthereumWallet;
  route: EvmDepositAddressResponse;
  client: ReturnType<typeof usePrivyClient>;
};
export type TransferReview = ReviewedMove & { item: IncomingItem };
type Notice = { message: string; requestId?: string | null };

export function useIncomingTransfer(identity: Identity) {
  const { solana, wallet, route } = identity;
  const sender = wallet.address.toLowerCase();
  const live = useRef(identity);
  live.current = identity;
  const alive = useRef(false);
  const active = useRef<AbortController | null>(null);
  const [busy, setBusy] = useState<"quoting" | "sending" | null>(null);
  const [review, setReview] = useState<TransferReview | null>(null);
  const reviewed = useRef<TransferReview | null>(null);
  function showReview(next: TransferReview | null) {
    reviewed.current = next;
    setReview(next);
  }
  const [notice, setNotice] = useState<Notice | null>(null);
  const [state, dispatch] = useReducer(reduceIncoming, solana, emptyIncoming);
  const [refresh, reload] = useReducer((i: number) => i + 1, 0);
  const sequence = useRef(0);
  const receipts = useMemo(() => receiptStore(mmkv, solana, sender), [solana, sender]);
  const [pendingCount, setPendingCount] = useState(0);
  const items = useMemo(() => incomingItems(state, route.networks ?? []), [state, route.networks]);
  const pending = pendingCount > 0 || busy === "sending" || state.transfers.some((t) => t.inFlight);

  useLayoutEffect(() => {
    alive.current = true;
    const unsubscribe = onSolanaEnvChange(() => active.current?.abort());
    return () => {
      alive.current = false;
      active.current?.abort();
      unsubscribe();
    };
  }, []);

  const request = useCallback(
    <T>(path: string, options: Parameters<IncomingRequest>[1]) =>
      fundingRequest<T>(solana, path, options),
    [solana],
  );

  async function assertActive(signal: AbortSignal) {
    const signer = await getWalletSigner();
    if (
      signal.aborted ||
      !alive.current ||
      getSolanaEnv() !== "mainnet" ||
      signer.kind === "privy" ||
      signer.publicKey.toBase58() !== solana
    ) {
      throw new Error("The active wallet or network changed. Review the transfer again.");
    }
  }

  function handleOutcome(outcome: MoveOutcome) {
    switch (outcome.kind) {
      case "submitted":
        setNotice({
          message: outcome.recorded
            ? "Transfer submitted. Waiting for Solana."
            : "Checking the submitted transfer.",
          requestId: outcome.requestId,
        });
        break;
      case "inFlight":
        setNotice({ message: "This transfer is already in progress.", requestId: outcome.requestId });
        break;
      case "failed":
        setNotice(
          outcome.uncertainRequestId
            ? {
                message: "Checking whether the transfer was sent.",
                requestId: outcome.uncertainRequestId,
              }
            : { message: outcome.message, requestId: outcome.requestId },
        );
        break;
      case "rejected":
        setNotice({ message: "Wallet approval canceled." });
        break;
      case "nothing":
        setNotice({ message: "There is no balance left to transfer." });
        break;
    }
  }

  useEffect(() => {
    const controller = new AbortController();
    const { signal } = controller;
    let reading = false;
    async function read(withBalances: boolean) {
      if (
        reading ||
        signal.aborted ||
        AppState.currentState === "background" ||
        getSolanaEnv() !== "mainnet"
      )
        return;
      reading = true;
      const at = ++sequence.current;
      try {
        const [balances, transfers] = await Promise.all([
          withBalances ? readIncomingBalances(request, signal) : undefined,
          readIncomingTransfers(request, signal),
        ]);
        if (signal.aborted) return;
        dispatch({ type: "read", wallet: solana, at, balances, transfers, error: null });
        for (const saved of receipts.read()) {
          if (recordedReceipt(transfers, saved.requestId)) {
            receipts.forget(saved.requestId);
            continue;
          }
          if (sending.has(sender)) continue;
          sending.add(sender);
          try {
            const provider = await settleWithin(receiptProvider(live.current.wallet, saved.chainId));
            if (signal.aborted) return;
            const result = await finishIncomingDeposit(
              request,
              saved.requestId,
              saved.symbol,
              provider,
              saved.hash,
              signal,
            );
            if (signal.aborted) return;
            if ((result.kind === "submitted" && result.recorded) || result.kind === "failed") {
              receipts.forget(saved.requestId);
              handleOutcome(result);
              reload();
            }
          } finally {
            sending.delete(sender);
          }
        }
        setPendingCount(receipts.read().length);
      } catch (e) {
        if (!signal.aborted) dispatch({ type: "read", wallet: solana, at, error: message(e) });
      } finally {
        reading = false;
      }
    }
    void read(true);
    const timer = setInterval(() => void read(!pending), pending ? 3000 : 30_000);
    const subscription = AppState.addEventListener("change", (next) => {
      // An external wallet may foreground its own app while authorizing this read.
      if (next === "active") void read(true);
    });
    return () => {
      controller.abort();
      clearInterval(timer);
      subscription.remove();
    };
  }, [pending, receipts, refresh, request, sender, solana]);

  function cancel() {
    if (busy === "sending") return;
    active.current?.abort();
    showReview(null);
  }

  async function act(item: IncomingItem, approved?: TransferReview) {
    if (active.current || sending.has(sender) || (approved && approved !== reviewed.current)) return;
    const controller = new AbortController();
    const { signal } = controller;
    active.current = controller;
    if (approved) sending.add(sender);
    setBusy(approved ? "sending" : "quoting");
    setNotice(null);
    showReview(null);
    const { client, userId, senderId } = live.current;
    const deliberate: IncomingRequest = (path, options) =>
      fundingRequest(solana, path, { ...options, interactive: true });
    try {
      await assertActive(signal);
      const [currentRoute, { user }] = await Promise.all([
        deliberate<EvmDepositAddressResponse>(
          routePath,
          { signal },
          "Could not check the deposit address",
        ),
        client.user.refreshUser(),
      ]);
      await assertActive(signal);
      const owner = linkedSender(user, currentRoute);
      if (
        user.id !== userId ||
        !isExternalOwner(user, solana) ||
        !owner ||
        owner.id !== senderId ||
        owner.address.toLowerCase() !== sender
      ) {
        throw new Error("The linked wallet changed. Reopen the transfer to review it again.");
      }
      if (receipts.read().some((r) => r.chain === item.chain && r.asset === item.asset)) {
        throw new Error("Checking the previous transfer before this balance can move again.");
      }
      if (!approved) {
        const at = ++sequence.current;
        const result = await prepareIncomingMove({
          request: deliberate,
          chain: item.chain,
          asset: item.asset,
          signal,
          onRead: (balances, transfers) => {
            if (!signal.aborted)
              dispatch({ type: "read", wallet: solana, at, balances, transfers, error: null });
          },
        });
        if (signal.aborted) return;
        if (result.kind !== "quoted") {
          handleOutcome(result);
          return;
        }
        validateQuote(item.chain, result.quote);
        showReview({ ...result, item });
        return;
      }

      const [balances, transfers] = await Promise.all([
        readIncomingBalances(deliberate, signal),
        readIncomingTransfers(deliberate, signal),
      ]);
      const moving = transfers.find(
        (t) => t.inFlight && t.chain === item.chain && t.asset === item.asset,
      );
      if (moving) {
        handleOutcome({ kind: "inFlight", requestId: moving.requestId });
        return;
      }
      const balance = balances.find((b) => b.chain === item.chain && b.asset === item.asset);
      if (
        !balance ||
        BigInt(balance.amountRaw) !== approved.amountRaw ||
        balance.decimals !== item.decimals
      ) {
        throw new Error("Your balance changed. Review the updated amount.");
      }
      await assertActive(signal);
      const { quote } = approved;
      const result = await executeIncomingQuote({
        request,
        quote,
        signal,
        symbol: item.symbol,
        checkConfirmation(confirmed: BridgeTransfer) {
          if (
            confirmed.requestId !== quote.requestId ||
            !confirmed.inFlight ||
            confirmed.direction !== "in" ||
            confirmed.state !== "quoted" ||
            confirmed.chain !== item.chain ||
            confirmed.asset !== item.asset ||
            confirmed.amountInRaw !== approved.amountRaw.toString() ||
            confirmed.amountOutRaw !== quote.amountOutRaw
          ) {
            throw new Error("The bridge confirmation does not match your review.");
          }
        },
        async connect(chainId) {
          const provider = await settleWithin(receiptProvider(wallet, chainId));
          async function assertSigningOwner() {
            await assertActive(signal);
            const current = (await client.user.refreshUser()).user;
            const account = linkedSender(current, currentRoute);
            if (
              current.id !== userId ||
              !isExternalOwner(current, solana) ||
              account?.id !== senderId ||
              account.address.toLowerCase() !== sender
            ) {
              throw new Error("The signing account changed.");
            }
            await assertActive(signal);
          }
          return {
            provider,
            async send(call, index) {
              const { hash } = await sendSponsoredCall(client, owner, chainId, call, assertSigningOwner);
              if (index === (quote.calls?.length ?? 0) - 1) {
                receipts.remember({
                  requestId: quote.requestId,
                  hash,
                  chainId,
                  chain: item.chain,
                  asset: item.asset,
                  symbol: item.symbol,
                });
              }
              return { hash };
            },
          };
        },
      });
      if (result.kind === "submitted" && result.recorded) receipts.forget(quote.requestId);
      if (!signal.aborted) handleOutcome(result);
    } catch (error) {
      if (!signal.aborted) setNotice({ message: message(error) });
    } finally {
      if (approved) sending.delete(sender);
      controller.abort();
      active.current = null;
      if (alive.current) {
        setBusy(null);
        reload();
      }
    }
  }

  return {
    items,
    transfers: state.transfers,
    error: state.error,
    pending,
    busy,
    review,
    notice,
    reload,
    cancel,
    act,
  };
}

function message(error: unknown) {
  return error instanceof Error ? error.message : "Could not check the transfer. Please try again.";
}
