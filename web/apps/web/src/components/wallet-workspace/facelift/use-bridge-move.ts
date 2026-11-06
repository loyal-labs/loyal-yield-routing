"use client";

import type {
  BridgeBalance,
  BridgeBalancesResponse,
  BridgeQuoteResponse,
  BridgeReleaseRequest,
  BridgeTransfer,
  BridgeTransfersResponse,
  EvmDepositNetwork,
  RouteError,
} from "@loyal-labs/api";
import {
  useSendTransaction,
  useUser,
  useWallets,
} from "@privy-io/react-auth";
import {
  useCallback,
  useEffect,
  useMemo,
  useReducer,
  useRef,
} from "react";

import { useEvmDepositAddress } from "./use-evm-deposit-address";

type IncomingStatus =
  | { kind: "waiting" }
  | { kind: "bridging" }
  | { kind: "arrived" }
  | { kind: "failed"; message: string; requestId: string | null };

export type IncomingItem = {
  key: string;
  chain: string;
  asset: string;
  amountRaw: bigint;
  decimals: number;
  minUsd: number;
  symbol: string;
  shortName: string;
  icon: string | null;
  chainIcon: string | null;
  transfer?: {
    requestId: string;
    state: BridgeTransfer["state"];
    submittedAt: string | null;
    inFlight: boolean;
  };
  status: IncomingStatus;
};

const POLL_MS = 3000;
const ARRIVED_MS = 5000;
const FOCUS_READ_MS = 30_000;
const RECEIPT_TIMEOUT_MS = 120_000;
const SEND_TIMEOUT_MS = 120_000;
const SEND_TIMEOUT = "move_send_timeout";
const APPROVAL_REVERTED = "move_approval_reverted";
const RELEASE_TIMEOUT_MS = 10_000;

const SPONSOR_REFUSAL_CODES = new Set([
  "insufficient_funds",
  "policy_violation",
  "feature_not_enabled",
]);

export function toRaw(input: string, decimals: number): bigint {
  const [whole = "", fraction = ""] = input.replace(/,/g, "").split(".");
  const digits = `${whole}${fraction.slice(0, decimals).padEnd(decimals, "0")}`;
  return /^\d+$/.test(digits) ? BigInt(digits) : BigInt(0);
}

export function fromRaw(raw: bigint | string, decimals: number): string {
  const value = typeof raw === "bigint" ? raw : BigInt(raw);
  const base = BigInt(10) ** BigInt(decimals);
  const fraction = (value % base).toString().padStart(decimals, "0");
  const trimmed = fraction.slice(0, 6).replace(/0+$/, "");
  const whole = (value / base).toLocaleString("en-US");
  return trimmed ? `${whole}.${trimmed}` : whole;
}

async function readJSON<T>(res: Response, fallback: string): Promise<T> {
  const body = (await res.json().catch(() => null)) as
    | T
    | Partial<RouteError>
    | null;
  if (res.ok && body) return body as T;
  const detail =
    body && typeof body === "object" && "error" in body ? body.error : undefined;
  throw Object.assign(new Error(detail?.message || `${fallback} (${res.status}).`), {
    code: detail?.code,
    requestId: detail?.requestId,
  });
}

function inFlightRefusal(e: unknown): MoveOutcome | null {
  const { code, requestId } = (e ?? {}) as { code?: unknown; requestId?: unknown };
  return code === "bridge_in_flight" && typeof requestId === "string"
    ? { kind: "inFlight", requestId }
    : null;
}

const post = (path: string, body: unknown, signal?: AbortSignal) =>
  fetch(path, {
    method: "POST",
    credentials: "include",
    headers: { "content-type": "application/json" },
    body: JSON.stringify(body),
    signal,
  });

const transferPath = (requestId: string, step: string) =>
  `/api/funding/bridge/transfers/${encodeURIComponent(requestId)}/${step}`;

const pause = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms));

const errorText = (e: unknown) => (e instanceof Error ? e.message : String(e));

type SendFailure = "rejected" | "refused" | "reverted" | "timeout" | "other";

export function classifySendError(e: unknown): SendFailure {
  const { code, privyErrorCode } = (e ?? {}) as {
    code?: unknown;
    privyErrorCode?: unknown;
  };
  if (code === 4001) return "rejected";
  if (code === SEND_TIMEOUT) return "timeout";
  if (code === APPROVAL_REVERTED) return "reverted";
  return [code, privyErrorCode].some(
    (c) => typeof c === "string" && SPONSOR_REFUSAL_CODES.has(c)
  )
    ? "refused"
    : "other";
}

export function settleWithin<T>(pending: Promise<T>, ms = SEND_TIMEOUT_MS) {
  let timer: ReturnType<typeof setTimeout>;
  const expired = new Promise<never>((_, reject) => {
    timer = setTimeout(
      () => reject(Object.assign(new Error("Timed out"), { code: SEND_TIMEOUT })),
      ms
    );
  });
  return Promise.race([pending, expired]).finally(() => clearTimeout(timer));
}

function sendFailureCopy(failure: SendFailure, symbol: string, sent: boolean) {
  if (failure === "timeout")
    return "This is taking too long. Check your wallet before trying again.";
  if (failure === "refused")
    return sent
      ? "Not sponsored. Please try again."
      : `Not sponsored. ${symbol} stays in your wallet.`;
  return sent
    ? "The move did not finish. Check your wallet before trying again."
    : "The move did not finish. Please try again.";
}

const readBalances = async (signal?: AbortSignal) =>
  (
    await readJSON<BridgeBalancesResponse>(
      await fetch("/api/funding/bridge/balances", { credentials: "include", signal }),
      "Could not read your EVM balances"
    )
  ).balances ?? [];

const readTransfers = async (signal?: AbortSignal) =>
  (
    await readJSON<BridgeTransfersResponse>(
      await fetch("/api/funding/bridge/transfers", { credentials: "include", signal }),
      "Could not read the bridge"
    )
  ).transfers ?? [];

const lane = (chain: string, asset: string) => `${chain}:${asset}`;

const unsettled = (t: BridgeTransfer) =>
  t.state === "quoted" || t.state === "submitted";

type Run = {
  key: string;
  chain: string;
  asset: string;
  amountRaw: bigint;
  requestId: string | null;
  running: boolean;
  failure: { message: string; requestId: string | null } | null;
};

type Incoming = {
  wallet: string | null;
  balances: BridgeBalance[];
  balancesAt: number;
  transfers: BridgeTransfer[];
  transfersAt: number;
  error: string | null;
  runs: Run[];
};

type IncomingEvent =
  | {
      type: "read";
      at: number;
      balances?: BridgeBalance[];
      transfers?: BridgeTransfer[];
      error?: string | null;
    }
  | { type: "run"; run: Run }
  | { type: "patch"; key: string; patch: Partial<Run> }
  | { type: "drop"; key: string }
  | { type: "settle" };

type IncomingAction = IncomingEvent & { wallet: string };

const emptyIncoming = (wallet: string | null): Incoming => ({
  wallet,
  balances: [],
  balancesAt: 0,
  transfers: [],
  transfersAt: 0,
  error: null,
  runs: [],
});

function adoptSettled(prev: Incoming, transfers: BridgeTransfer[]): Run[] {
  const tracked = new Set(prev.runs.map((r) => r.requestId));
  const watched = new Set(
    prev.transfers.filter((t) => t.inFlight).map((t) => t.requestId)
  );
  return transfers
    .filter(
      (t) => watched.has(t.requestId) && !tracked.has(t.requestId) && !unsettled(t)
    )
    .map((t) => ({
      key: t.requestId,
      chain: t.chain,
      asset: t.asset,
      amountRaw: BigInt(t.amountInRaw),
      requestId: t.requestId,
      running: false,
      failure: null,
    }));
}

function stillListed(runs: Run[], transfers: BridgeTransfer[]): Run[] {
  const listed = new Set(transfers.map((t) => t.requestId));
  return runs.filter(
    (r) => r.running || r.failure || (r.requestId !== null && listed.has(r.requestId))
  );
}

function reduceIncoming(state: Incoming, action: IncomingAction): Incoming {
  const prev = state.wallet === action.wallet ? state : emptyIncoming(action.wallet);
  switch (action.type) {
    case "read": {
      const next = { ...prev };
      if (action.balances && action.at > prev.balancesAt) {
        next.balances = action.balances;
        next.balancesAt = action.at;
      }
      if (action.transfers && action.at > prev.transfersAt) {
        next.runs = [
          ...stillListed(prev.runs, action.transfers),
          ...adoptSettled(prev, action.transfers),
        ];
        next.transfers = action.transfers;
        next.transfersAt = action.at;
      }
      if (action.error !== undefined) next.error = action.error;
      return next;
    }
    case "run":
      return {
        ...prev,
        runs: [
          ...prev.runs.filter(
            (r) =>
              r.key !== action.run.key &&
              lane(r.chain, r.asset) !== lane(action.run.chain, action.run.asset)
          ),
          action.run,
        ],
      };
    case "patch":
      return {
        ...prev,
        runs: prev.runs.map((r) =>
          r.key === action.key ? { ...r, ...action.patch } : r
        ),
      };
    case "drop":
      return { ...prev, runs: prev.runs.filter((r) => r.key !== action.key) };
    case "settle": {
      const filled = new Set(
        prev.transfers.filter((t) => t.state === "filled").map((t) => t.requestId)
      );
      return {
        ...prev,
        runs: prev.runs.filter((r) => !r.requestId || !filled.has(r.requestId)),
      };
    }
  }
}

function serverStatus(
  t: BridgeTransfer,
  symbol: string,
  shortName: string
): IncomingStatus {
  if (t.state === "filled") return { kind: "arrived" };
  if (t.state === "refunded")
    return {
      kind: "failed",
      message: `Relay refunded ${symbol} to your ${shortName} wallet.`,
      requestId: t.requestId,
    };
  if (t.state === "failed")
    return { kind: "failed", message: "The bridge failed.", requestId: t.requestId };
  return { kind: "bridging" };
}

function incomingItems(
  state: Incoming,
  networks: EvmDepositNetwork[]
): IncomingItem[] {
  const order = new Map(
    networks.flatMap((n) => (n.tokens ?? []).map((t) => lane(n.chain, t.asset)))
      .map((key, index) => [key, index] as const)
  );
  const rank = (item: IncomingItem) =>
    order.get(lane(item.chain, item.asset)) ?? order.size;
  const describe = (chain: string, asset: string) => {
    const network = networks.find((n) => n.chain === chain);
    const token = network?.tokens?.find((t) => t.asset === asset);
    return network && token
      ? {
          chain,
          asset,
          decimals: token.decimals,
          minUsd: network.minUsd,
          symbol: token.symbol,
          shortName: network.shortName,
          icon: token.icon,
          chainIcon: network.icon,
        }
      : null;
  };
  const byRequest = new Map(state.transfers.map((t) => [t.requestId, t]));
  const covered = new Set<string>();
  const items: IncomingItem[] = [];

  for (const run of state.runs) {
    const shown = describe(run.chain, run.asset);
    if (!shown) continue;
    const server = run.requestId ? byRequest.get(run.requestId) : undefined;
    if (server && unsettled(server) && !server.inFlight) continue;
    const status: IncomingStatus =
      server && !unsettled(server)
        ? serverStatus(server, shown.symbol, shown.shortName)
        : run.failure
        ? { kind: "failed", ...run.failure }
        : { kind: "bridging" };
    covered.add(lane(run.chain, run.asset));
    items.push({
      ...shown,
      key: run.key,
      amountRaw: server ? BigInt(server.amountInRaw) : run.amountRaw,
      transfer: server && {
        requestId: server.requestId,
        state: server.state,
        submittedAt: server.submittedAt,
        inFlight: server.inFlight,
      },
      status,
    });
  }

  const claimed = new Set(state.runs.map((r) => r.requestId));
  for (const t of state.transfers) {
    if (!t.inFlight || claimed.has(t.requestId)) continue;
    const shown = describe(t.chain, t.asset);
    if (!shown) continue;
    covered.add(lane(t.chain, t.asset));
    items.push({
      ...shown,
      key: t.requestId,
      amountRaw: BigInt(t.amountInRaw),
      transfer: {
        requestId: t.requestId,
        state: t.state,
        submittedAt: t.submittedAt,
        inFlight: t.inFlight,
      },
      status: { kind: "bridging" },
    });
  }

  for (const b of state.balances) {
    const amountRaw = BigInt(b.amountRaw);
    const shown = describe(b.chain, b.asset);
    if (!shown || amountRaw === BigInt(0) || covered.has(lane(b.chain, b.asset)))
      continue;
    items.push({
      ...shown,
      key: `${state.wallet}:${b.chain}:${b.asset}:${amountRaw}`,
      amountRaw,
      status: { kind: "waiting" },
    });
  }
  return items.sort((a, b) => rank(a) - rank(b));
}

type Receipt = "landed" | "reverted" | "unknown";

async function waitForReceipt(
  provider: { request: (args: { method: string; params?: unknown[] }) => Promise<unknown> },
  hash: string,
  signal: AbortSignal
): Promise<Receipt> {
  const deadline = Date.now() + RECEIPT_TIMEOUT_MS;
  while (Date.now() < deadline && !signal.aborted) {
    let receipt: { status?: string } | null;
    try {
      receipt = (await settleWithin(
        provider.request({
          method: "eth_getTransactionReceipt",
          params: [hash],
        }),
        deadline - Date.now()
      )) as { status?: string } | null;
    } catch {
      return "unknown";
    }
    if (receipt?.status === "0x1") return "landed";
    if (receipt?.status === "0x0") return "reverted";
    await pause(1500);
  }
  return "unknown";
}

type ReleaseReason = BridgeReleaseRequest["reason"];

function releaseReason(
  failure: SendFailure | "aborted",
  depositHandedToWallet: boolean
): ReleaseReason | null {
  if (failure === "rejected" || failure === "refused") return failure;
  if (depositHandedToWallet) return null;
  return failure === "reverted" ? "reverted" : "aborted";
}

async function release(requestId: string, reason: ReleaseReason) {
  for (let attempt = 0; attempt < 2; attempt++) {
    const res = await post(
      transferPath(requestId, "release"),
      { reason } satisfies BridgeReleaseRequest,
      AbortSignal.timeout(RELEASE_TIMEOUT_MS)
    ).catch(() => null);
    if (res && res.status < 500) return;
  }
}

type SendTransaction = ReturnType<typeof useSendTransaction>["sendTransaction"];
type EvmWallet = ReturnType<typeof useWallets>["wallets"][number];

type MoveOutcome =
  | { kind: "nothing" }
  | { kind: "aborted" }
  | { kind: "rejected" }
  | { kind: "inFlight"; requestId: string }
  | { kind: "failed"; message: string; requestId: string | null }
  | { kind: "submitted"; requestId: string; recorded: boolean };

type Move = {
  sender: string;
  chain: string;
  asset: string;
  symbol: string;
  wallet: EvmWallet | undefined;
  sendTransaction: SendTransaction;
  signal: AbortSignal;
  onRead: (balances: BridgeBalance[], transfers: BridgeTransfer[]) => void;
  onQuoted: (requestId: string, amountRaw: bigint) => void;
};

export async function moveIncoming(move: Move): Promise<MoveOutcome> {
  const lockName = `loyal.bridge.move:${move.sender.toLowerCase()}:${move.chain}`;
  try {
    return await (navigator.locks
      ? navigator.locks.request(lockName, { signal: move.signal }, () =>
          moveLocked(move)
        )
      : moveLocked(move));
  } catch (e) {
    if (move.signal.aborted) return { kind: "aborted" };
    return { kind: "failed", message: errorText(e), requestId: null };
  }
}

async function moveLocked(move: Move): Promise<MoveOutcome> {
  const { signal } = move;
  let quoted: BridgeQuoteResponse;
  try {
    const [balances, transfers] = await Promise.all([
      readBalances(signal),
      readTransfers(signal),
    ]);
    move.onRead(balances, transfers);
    const here = lane(move.chain, move.asset);
    const moving = transfers.find((t) => t.inFlight && lane(t.chain, t.asset) === here);
    if (moving) return { kind: "inFlight", requestId: moving.requestId };
    const balance = balances.find((b) => lane(b.chain, b.asset) === here);
    const amountRaw = balance ? BigInt(balance.amountRaw) : BigInt(0);
    if (amountRaw === BigInt(0)) return { kind: "nothing" };
    quoted = await readJSON<BridgeQuoteResponse>(
      await post(
        "/api/funding/bridge/quote",
        { chain: move.chain, asset: move.asset, amountRaw: amountRaw.toString() },
        signal
      ),
      "Could not get a quote"
    );
    move.onQuoted(quoted.requestId, amountRaw);
  } catch (e) {
    if (signal.aborted) return { kind: "aborted" };
    return inFlightRefusal(e) ?? { kind: "failed", message: errorText(e), requestId: null };
  }

  const calls = quoted.calls ?? [];
  const { wallet } = move;
  let confirming = false;
  let depositSent = false;
  let sent = false;
  let originTx: string | null = null;
  try {
    if (!wallet || calls.length === 0) {
      throw new Error("Your EVM wallet is not ready. Please try again.");
    }
    await settleWithin(wallet.switchChain(quoted.chainId));
    const provider = await settleWithin(wallet.getEthereumProvider());
    confirming = true;
    await settleWithin(
      post(transferPath(quoted.requestId, "confirm"), {}, signal).then((res) =>
        readJSON<BridgeTransfer>(res, "Could not start the move")
      )
    );
    for (const [index, call] of calls.entries()) {
      signal.throwIfAborted();
      const last = index === calls.length - 1;
      depositSent = last;
      const { hash } = await settleWithin(
        move.sendTransaction(
          {
            to: call.to,
            data: call.data,
            value: BigInt(call.value),
            chainId: quoted.chainId,
          },
          {
            sponsor: true,
            address: wallet.address,
            uiOptions: { showWalletUIs: index === 0 },
          }
        )
      );
      sent = true;
      signal.throwIfAborted();
      const receipt = await waitForReceipt(provider, hash, signal);
      signal.throwIfAborted();
      if (receipt === "reverted" && last) {
        await release(quoted.requestId, "reverted");
        return {
          kind: "failed",
          message: `The transfer reverted. ${move.symbol} stays in your wallet.`,
          requestId: quoted.requestId,
        };
      }
      if (receipt === "reverted")
        throw Object.assign(new Error("The approval transaction reverted."), {
          code: APPROVAL_REVERTED,
        });
      if (last) originTx = hash;
      else if (receipt === "unknown") throw new Error("Transaction confirmation timed out");
    }
  } catch (e) {
    const refused = inFlightRefusal(e);
    if (refused && !signal.aborted) return refused;
    const failure = classifySendError(e);
    const reason = confirming
      ? releaseReason(signal.aborted ? "aborted" : failure, depositSent)
      : null;
    if (reason) await release(quoted.requestId, reason);
    if (signal.aborted) return { kind: "aborted" };
    if (failure === "rejected") return { kind: "rejected" };
    if (failure === "refused")
      void post(
        transferPath(quoted.requestId, "sponsor-refusal"),
        { message: errorText(e).slice(0, 500) },
        signal
      ).catch(() => undefined);
    return {
      kind: "failed",
      message: sendFailureCopy(failure, move.symbol, sent),
      requestId: sent ? quoted.requestId : null,
    };
  }
  if (!originTx)
    return {
      kind: "failed",
      message: sendFailureCopy("other", move.symbol, sent),
      requestId: null,
    };

  for (let attempt = 0; attempt < 3; attempt++) {
    if (signal.aborted) return { kind: "aborted" };
    try {
      await readJSON<BridgeTransfer>(
        await post(transferPath(quoted.requestId, "submit"), { originTx }, signal),
        "Could not record the bridge"
      );
      return { kind: "submitted", requestId: quoted.requestId, recorded: true };
    } catch {
      if (attempt < 2) await pause(2000);
    }
  }
  if (signal.aborted) return { kind: "aborted" };
  return { kind: "submitted", requestId: quoted.requestId, recorded: false };
}

const isExternalSolanaWallet = (
  user: ReturnType<typeof useUser>["user"],
  address: string | null
) =>
  user?.linkedAccounts.some(
    (a) =>
      a.type === "wallet" &&
      a.chainType === "solana" &&
      a.address === address &&
      a.walletClientType !== "privy" &&
      a.walletClientType !== "privy-v2"
  ) ?? false;

export function useIncomingTransfers(walletAddress: string | null) {
  const { user } = useUser();
  const { sendTransaction } = useSendTransaction();
  const { wallets } = useWallets();
  const external = isExternalSolanaWallet(user, walletAddress);
  const evm = useEvmDepositAddress(external ? walletAddress : null);
  const { info, load, networks, retryOnNextLoad } = evm;
  const [peeks, peek] = useReducer((n: number) => n + 1, 0);
  useEffect(() => {
    void load(false);
  }, [load, peeks]);
  const sender =
    external && info?.kind === "relay_signed" ? info.address : null;
  const enabled = sender !== null && networks.length > 0;

  const [state, dispatch] = useReducer(reduceIncoming, null, () =>
    emptyIncoming(null)
  );
  const walletRef = useRef(walletAddress);
  walletRef.current = walletAddress;
  const apply = useCallback(
    (wallet: string, event: IncomingEvent) => {
      if (walletRef.current === wallet) dispatch({ ...event, wallet });
    },
    []
  );
  const readAt = useRef(0);
  const balancesReadAt = useRef(0);
  const read = useCallback(
    async (wallet: string, withBalances: boolean) => {
      const at = ++readAt.current;
      if (withBalances) balancesReadAt.current = Date.now();
      const [balances, transfers] = await Promise.allSettled([
        withBalances ? readBalances() : Promise.resolve(undefined),
        readTransfers(),
      ]);
      apply(wallet, {
        type: "read",
        at,
        balances: balances.status === "fulfilled" ? balances.value : undefined,
        transfers: transfers.status === "fulfilled" ? transfers.value : undefined,
        error: !withBalances
          ? undefined
          : balances.status === "rejected"
          ? errorText(balances.reason)
          : null,
      });
    },
    [apply]
  );

  const [reads, reread] = useReducer((n: number) => n + 1, 0);
  useEffect(() => {
    if (enabled && walletAddress) void read(walletAddress, true);
  }, [enabled, read, reads, walletAddress]);

  useEffect(() => {
    if (!enabled || !walletAddress) return;
    const wallet = walletAddress;
    const peekBalances = () => {
      if (document.visibilityState !== "visible") return;
      if (Date.now() - balancesReadAt.current < FOCUS_READ_MS) return;
      void read(wallet, true);
    };
    window.addEventListener("focus", peekBalances);
    document.addEventListener("visibilitychange", peekBalances);
    return () => {
      window.removeEventListener("focus", peekBalances);
      document.removeEventListener("visibilitychange", peekBalances);
    };
  }, [enabled, read, walletAddress]);

  const shown = enabled && state.wallet === walletAddress ? state : null;
  const items = useMemo(
    () => (shown ? incomingItems(shown, networks) : []),
    [networks, shown]
  );

  const polling = items.some(
    (i) => i.status.kind === "bridging" || i.transfer?.inFlight
  );
  useEffect(() => {
    if (!polling || !walletAddress) return;
    const timer = setInterval(() => void read(walletAddress, false), POLL_MS);
    return () => clearInterval(timer);
  }, [polling, read, walletAddress]);

  const arrived = items.some((i) => i.status.kind === "arrived");
  useEffect(() => {
    if (!arrived || !walletAddress) return;
    const timer = setTimeout(() => {
      apply(walletAddress, { type: "settle" });
      reread();
    }, ARRIVED_MS);
    return () => clearTimeout(timer);
  }, [apply, arrived, walletAddress]);

  const runSignal = useRef<AbortSignal | null>(null);
  useEffect(() => {
    const controller = new AbortController();
    runSignal.current = controller.signal;
    return () => controller.abort();
  }, [walletAddress]);

  const active = useRef(new Set<string>());
  const confirm = useCallback(
    async (item: IncomingItem) => {
      const signal = runSignal.current;
      if (!sender || !walletAddress || !signal || signal.aborted) return;
      const wallet = walletAddress;
      const busy = `${wallet}:${lane(item.chain, item.asset)}`;
      if (active.current.has(busy)) return;
      active.current.add(busy);
      const { key } = item;
      apply(wallet, {
        type: "run",
        run: {
          key,
          chain: item.chain,
          asset: item.asset,
          amountRaw: item.amountRaw,
          requestId: null,
          running: true,
          failure: null,
        },
      });
      try {
        const outcome = await moveIncoming({
          sender,
          chain: item.chain,
          asset: item.asset,
          symbol: item.symbol,
          wallet: wallets.find(
            (w) => w.address.toLowerCase() === sender.toLowerCase()
          ),
          sendTransaction,
          signal,
          onRead: (balances, transfers) => {
            balancesReadAt.current = Date.now();
            apply(wallet, {
              type: "read",
              at: ++readAt.current,
              balances,
              transfers,
              error: null,
            });
          },
          onQuoted: (requestId, amountRaw) =>
            apply(wallet, { type: "patch", key, patch: { requestId, amountRaw } }),
        });
        if (outcome.kind === "aborted") return;
        if (outcome.kind === "nothing" || outcome.kind === "rejected") {
          apply(wallet, { type: "drop", key });
          return;
        }
        apply(wallet, {
          type: "patch",
          key,
          patch:
            outcome.kind === "failed"
              ? {
                  running: false,
                  failure: { message: outcome.message, requestId: outcome.requestId },
                }
              : { running: false, requestId: outcome.requestId },
        });
        void read(wallet, false);
      } finally {
        active.current.delete(busy);
      }
    },
    [apply, read, sendTransaction, sender, walletAddress, wallets]
  );

  const reload = useCallback(() => {
    retryOnNextLoad();
    peek();
    reread();
  }, [retryOnNextLoad]);

  const discoveryError = external && !info ? evm.error : null;

  return {
    items,
    error: discoveryError ?? shown?.error ?? null,
    confirm,
    reload,
  };
}

export type IncomingTransfersState = ReturnType<typeof useIncomingTransfers>;
