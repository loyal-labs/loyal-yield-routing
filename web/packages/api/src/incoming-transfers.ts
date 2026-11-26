import type { BridgeBalance, BridgeTransfer, EvmDepositNetwork } from "./generated/types.gen";

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

export const lane = (chain: string, asset: string) => `${chain}:${asset}`;

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

export type Incoming = {
  wallet: string | null;
  balances: BridgeBalance[];
  balancesAt: number;
  transfers: BridgeTransfer[];
  transfersAt: number;
  error: string | null;
  runs: Run[];
};

export type IncomingEvent =
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

export const emptyIncoming = (wallet: string | null): Incoming => ({
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

export function reduceIncoming(state: Incoming, action: IncomingAction): Incoming {
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

export function incomingItems(
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
