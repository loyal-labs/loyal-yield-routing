import type {
  BridgeBalance,
  BridgeBalancesResponse,
  BridgeCall,
  BridgeQuoteResponse,
  BridgeReleaseRequest,
  BridgeTransfer,
  BridgeTransfersResponse,
} from "./generated/types.gen";
import { lane } from "./incoming-transfers";

export type IncomingRequest = <T>(
  path: string,
  options: { method?: "GET" | "POST"; body?: unknown; signal?: AbortSignal },
  fallback: string,
) => Promise<T>;

export const transferPath = (requestId: string, step: string) =>
  `/api/funding/bridge/transfers/${encodeURIComponent(requestId)}/${step}`;

export async function readIncomingBalances(request: IncomingRequest, signal?: AbortSignal) {
  const result = await request<BridgeBalancesResponse>(
    "/api/funding/bridge/balances",
    { signal },
    "Could not read your EVM balances",
  );
  return result.balances ?? [];
}

export async function readIncomingTransfers(request: IncomingRequest, signal?: AbortSignal) {
  const result = await request<BridgeTransfersResponse>(
    "/api/funding/bridge/transfers",
    { signal },
    "Could not read the bridge",
  );
  return result.transfers ?? [];
}

const RECEIPT_TIMEOUT_MS = 120_000;
const SEND_TIMEOUT_MS = 120_000;
const SEND_TIMEOUT = "move_send_timeout";
const APPROVAL_REVERTED = "move_approval_reverted";
const RELEASE_TIMEOUT_MS = 10_000;

const SPONSOR_REFUSAL_CODES = new Set(["insufficient_funds", "policy_violation", "feature_not_enabled"]);

function inFlightRefusal(e: unknown): Extract<MoveOutcome, { kind: "inFlight" }> | null {
  const { code, requestId } = (e ?? {}) as { code?: unknown; requestId?: unknown };
  return code === "bridge_in_flight" && typeof requestId === "string"
    ? { kind: "inFlight", requestId }
    : null;
}

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
  return [code, privyErrorCode].some((c) => typeof c === "string" && SPONSOR_REFUSAL_CODES.has(c))
    ? "refused"
    : "other";
}

export function settleWithin<T>(pending: Promise<T>, ms = SEND_TIMEOUT_MS) {
  let timer: ReturnType<typeof setTimeout>;
  const expired = new Promise<never>((_, reject) => {
    timer = setTimeout(() => reject(Object.assign(new Error("Timed out"), { code: SEND_TIMEOUT })), ms);
  });
  return Promise.race([pending, expired]).finally(() => clearTimeout(timer));
}

function sendFailureCopy(failure: SendFailure, symbol: string, sent: boolean) {
  if (failure === "timeout") return "This is taking too long. Check your wallet before trying again.";
  if (failure === "refused")
    return sent ? "Not sponsored. Please try again." : `Not sponsored. ${symbol} stays in your wallet.`;
  return sent
    ? "The move did not finish. Check your wallet before trying again."
    : "The move did not finish. Please try again.";
}

type Receipt = "landed" | "reverted" | "unknown";

async function waitForReceipt(
  provider: { request: (args: { method: string; params?: unknown[] }) => Promise<unknown> },
  hash: string,
  signal: AbortSignal,
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
        deadline - Date.now(),
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
  depositHandedToWallet: boolean,
): ReleaseReason | null {
  if (failure === "rejected" || failure === "refused") return failure;
  if (depositHandedToWallet) return null;
  return failure === "reverted" ? "reverted" : "aborted";
}

async function release(request: IncomingRequest, requestId: string, reason: ReleaseReason) {
  for (let attempt = 0; attempt < 2; attempt++) {
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), RELEASE_TIMEOUT_MS);
    try {
      await request(
        transferPath(requestId, "release"),
        {
          method: "POST",
          body: { reason } satisfies BridgeReleaseRequest,
          signal: controller.signal,
        },
        "Could not release the move",
      );
      return;
    } catch (error) {
      const status = (error as { status?: number } | null)?.status;
      if (status && status < 500) return;
    } finally {
      clearTimeout(timer);
    }
  }
}

export type MoveOutcome =
  | { kind: "nothing" }
  | { kind: "aborted" }
  | { kind: "rejected" }
  | { kind: "inFlight"; requestId: string }
  | { kind: "failed"; message: string; requestId: string | null; uncertainRequestId?: string }
  | { kind: "submitted"; requestId: string; recorded: boolean };

export type ReviewedMove = {
  kind: "quoted";
  quote: BridgeQuoteResponse;
  amountRaw: bigint;
};

type PrepareMove = {
  request: IncomingRequest;
  chain: string;
  asset: string;
  signal: AbortSignal;
  onRead: (balances: BridgeBalance[], transfers: BridgeTransfer[]) => void;
};

export async function prepareIncomingMove(
  move: PrepareMove,
): Promise<ReviewedMove | Exclude<MoveOutcome, { kind: "submitted" }>> {
  const { signal, request } = move;
  try {
    const [balances, transfers] = await Promise.all([
      readIncomingBalances(request, signal),
      readIncomingTransfers(request, signal),
    ]);
    move.onRead(balances, transfers);
    const here = lane(move.chain, move.asset);
    const moving = transfers.find((t) => t.inFlight && lane(t.chain, t.asset) === here);
    if (moving) return { kind: "inFlight", requestId: moving.requestId };
    const balance = balances.find((b) => lane(b.chain, b.asset) === here);
    const amountRaw = balance ? BigInt(balance.amountRaw) : BigInt(0);
    if (amountRaw === BigInt(0)) return { kind: "nothing" };
    const quote = await request<BridgeQuoteResponse>(
      "/api/funding/bridge/quote",
      {
        method: "POST",
        body: { chain: move.chain, asset: move.asset, amountRaw: amountRaw.toString() },
        signal,
      },
      "Could not get a quote",
    );
    return { kind: "quoted", quote, amountRaw };
  } catch (e) {
    if (signal.aborted) return { kind: "aborted" };
    return inFlightRefusal(e) ?? { kind: "failed", message: errorText(e), requestId: null };
  }
}

export type IncomingConnection = {
  provider: Parameters<typeof waitForReceipt>[0];
  send: (call: BridgeCall, index: number) => Promise<{ hash: string }>;
};

type ExecuteMove = {
  request: IncomingRequest;
  quote: BridgeQuoteResponse;
  symbol: string;
  signal: AbortSignal;
  connect: (chainId: number) => Promise<IncomingConnection>;
  checkConfirmation?: (transfer: BridgeTransfer) => void;
};

function checkSignal(signal: AbortSignal) {
  if (signal.aborted) throw new Error("Move canceled");
}

// A known deposit hash can be recorded again after reopening without calling a signer.
export async function finishIncomingDeposit(
  request: IncomingRequest,
  requestId: string,
  symbol: string,
  provider: IncomingConnection["provider"],
  hash: string,
  signal: AbortSignal,
): Promise<MoveOutcome> {
  const receipt = await waitForReceipt(provider, hash, signal);
  if (signal.aborted) return { kind: "aborted" };
  if (receipt === "reverted") {
    await release(request, requestId, "reverted");
    return {
      kind: "failed",
      message: `The transfer reverted. ${symbol} stays in your wallet.`,
      requestId,
    };
  }
  for (let attempt = 0; attempt < 3; attempt++) {
    if (signal.aborted) return { kind: "aborted" };
    try {
      await request<BridgeTransfer>(
        transferPath(requestId, "submit"),
        {
          method: "POST",
          body: { originTx: hash },
          signal,
        },
        "Could not record the bridge",
      );
      return { kind: "submitted", requestId, recorded: true };
    } catch {
      if (attempt < 2) await pause(2000);
    }
  }
  return signal.aborted ? { kind: "aborted" } : { kind: "submitted", requestId, recorded: false };
}

export async function executeIncomingQuote(move: ExecuteMove): Promise<MoveOutcome> {
  const { quote: quoted, signal, request } = move;
  const post = <T>(step: string, body: unknown, fallback: string) =>
    request<T>(transferPath(quoted.requestId, step), { method: "POST", body, signal }, fallback);
  const calls = quoted.calls ?? [];
  let confirming = false;
  let depositSent = false;
  let sent = false;
  try {
    checkSignal(signal);
    if (!calls.length) throw new Error("Your EVM wallet is not ready. Please try again.");
    const { provider, send } = await move.connect(quoted.chainId);
    checkSignal(signal);
    confirming = true;
    const confirmed = await settleWithin(
      post<BridgeTransfer>("confirm", {}, "Could not start the move"),
    );
    move.checkConfirmation?.(confirmed);
    for (const [index, call] of calls.entries()) {
      checkSignal(signal);
      const last = index === calls.length - 1;
      depositSent = last;
      const { hash } = await settleWithin(send(call, index));
      sent = true;
      checkSignal(signal);
      if (last)
        return await finishIncomingDeposit(
          request,
          quoted.requestId,
          move.symbol,
          provider,
          hash,
          signal,
        );
      const receipt = await waitForReceipt(provider, hash, signal);
      checkSignal(signal);
      if (receipt === "reverted")
        throw Object.assign(new Error("The approval transaction reverted."), {
          code: APPROVAL_REVERTED,
        });
      if (receipt === "unknown") throw new Error("Transaction confirmation timed out");
    }
  } catch (e) {
    const refused = inFlightRefusal(e);
    if (refused && !signal.aborted) return refused;
    const failure = classifySendError(e);
    const mayHaveBroadcast =
      depositSent && (e as { code?: string } | null)?.code !== "move_not_broadcast";
    const reason = confirming
      ? releaseReason(signal.aborted ? "aborted" : failure, mayHaveBroadcast)
      : null;
    if (reason) await release(request, quoted.requestId, reason);
    if (signal.aborted) return { kind: "aborted" };
    if (failure === "rejected") return { kind: "rejected" };
    if (failure === "refused")
      void post(
        "sponsor-refusal",
        { message: errorText(e).slice(0, 500) },
        "Could not record the sponsorship refusal",
      ).catch(() => undefined);
    return {
      kind: "failed",
      message: sendFailureCopy(failure, move.symbol, sent),
      requestId: sent ? quoted.requestId : null,
      ...(mayHaveBroadcast && !reason ? { uncertainRequestId: quoted.requestId } : {}),
    };
  }
  return { kind: "nothing" };
}
