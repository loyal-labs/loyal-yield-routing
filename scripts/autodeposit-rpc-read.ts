import { Connection, type FetchFn } from "@solana/web3.js";

const MAX_ATTEMPTS = 3;
const TOTAL_BUDGET_MS = 8_000;
const RETRY_DELAY_MS = 250;

// Inject only the HTTP call signature, not Bun's optional runtime extensions.
export type AutodepositRpcFetch = (...args: Parameters<FetchFn>) => ReturnType<FetchFn>;

export type AutodepositRpcReadContext = {
  executorStage: "read_wallet_balance" | "read_vault_balance";
  targetId: string;
  scheduledSlotId: string | null;
};

export type AutodepositRpcReadDiagnostics = AutodepositRpcReadContext & {
  operation: "getTokenAccountBalance";
  httpStatus: number | null;
  attempts: number;
  retries: number;
  elapsedMs: number;
  deadlineExceeded: boolean;
};

export class AutodepositRpcReadError extends Error {
  constructor(readonly diagnostics: AutodepositRpcReadDiagnostics) {
    // Never retain a provider URL, response body, or the original error as cause.
    super("Autodeposit pre-send balance read failed");
    this.name = "AutodepositRpcReadError";
  }
}

export function isTransientRpcHttpStatus(status: number | null): boolean {
  return status === 500 || status === 502 || status === 503 || status === 504;
}

/** Legacy web3 HTTP failures contain status + reason; extract only that allowlist. */
export function autodepositDependencyHttpStatus(error: unknown): number | null {
  if (error instanceof AutodepositRpcReadError) {
    return error.diagnostics.httpStatus;
  }
  const message = error instanceof Error ? error.message : String(error);
  const match = /\b(500 Internal Server Error|502 Bad Gateway|503 Service Unavailable|504 Gateway Timeout)\b/i.exec(message);
  return match ? Number(match[1].slice(0, 3)) : null;
}

/**
 * Only the two PRE-SEND balance reads use this connection. Never pass it to a send
 * or recovery path. A shared deadline covers requests, body reads, and backoff;
 * the abort signal and fetch guard also prevent SDK work beyond that deadline.
 * The callback preserves the caller's existing balance/missing-account semantics.
 */
export async function readAutodepositPreSendBalance(args: {
  rpcUrl: string;
  context: AutodepositRpcReadContext;
  read: (connection: Connection) => Promise<bigint>;
}, options: {
  fetch?: AutodepositRpcFetch;
  totalBudgetMs?: number;
  retryDelayMs?: number;
} = {}): Promise<bigint> {
  const totalBudgetMs = options.totalBudgetMs ?? TOTAL_BUDGET_MS;
  const retryDelayMs = options.retryDelayMs ?? RETRY_DELAY_MS;
  if (!Number.isFinite(totalBudgetMs) || totalBudgetMs <= 0 ||
      !Number.isFinite(retryDelayMs) || retryDelayMs < 0) {
    throw new Error("Invalid autodeposit RPC read budget");
  }
  const startedAt = performance.now();
  const deadline = startedAt + totalBudgetMs;
  const controller = new AbortController();
  let attempts = 0;
  let httpStatus: number | null = null;
  const expired = () => controller.signal.aborted || performance.now() >= deadline;
  const failure = (deadlineExceeded = expired()) => new AutodepositRpcReadError({
    ...args.context,
    operation: "getTokenAccountBalance",
    httpStatus,
    attempts,
    retries: Math.max(0, attempts - 1),
    elapsedMs: Math.round(performance.now() - startedAt),
    deadlineExceeded,
  });
  const fetchRpc = options.fetch ?? globalThis.fetch;
  const connection = new Connection(args.rpcUrl, {
    commitment: "confirmed",
    // web3's otherwise-hidden 429 retry loop must not extend our deadline or
    // retry errors outside the explicit transient-server allowlist.
    disableRetryOnRateLimit: true,
    fetch: Object.assign(async (...[url, init]: Parameters<FetchFn>) => {
      if (expired()) throw failure(true);
      const response = await fetchRpc(url, { ...init, signal: controller.signal });
      httpStatus = response.status;
      if (!response.ok) {
        // Do not read/retain untrusted provider error bodies (URLs may be echoed).
        void response.body?.cancel().catch(() => {});
        throw failure();
      }
      return response;
    }, {
      // Bun's FetchFn includes preconnect. Never open speculative connections
      // outside this read's deadline; web3 itself uses only the call signature.
      preconnect: () => {},
    }),
  });
  let deadlineTimer: ReturnType<typeof setTimeout> | undefined;
  let backoffTimer: ReturnType<typeof setTimeout> | undefined;
  const deadlineReached = new Promise<never>((_, reject) => {
    deadlineTimer = setTimeout(() => {
      controller.abort();
      reject(failure(true));
    }, Math.max(0, deadline - performance.now()));
  });
  const readWithRetries = async () => {
    for (;;) {
      if (expired()) throw failure(true);
      attempts += 1;
      httpStatus = null;
      try {
        const balance = await args.read(connection);
        if (expired()) throw failure(true);
        return balance;
      } catch {
        if (expired()) throw failure(true);
        if (!isTransientRpcHttpStatus(httpStatus) || attempts >= MAX_ATTEMPTS) {
          throw failure();
        }
        const delay = retryDelayMs * 2 ** (attempts - 1);
        if (performance.now() + delay >= deadline) {
          // The next attempt cannot start within budget. Fail now, not after
          // sleeping beyond it, retaining the last observed server status.
          throw failure(true);
        }
        await new Promise<void>((resolve) => {
          backoffTimer = setTimeout(resolve, delay);
        });
      }
    }
  };
  try {
    return await Promise.race([readWithRetries(), deadlineReached]);
  } finally {
    clearTimeout(deadlineTimer);
    clearTimeout(backoffTimer);
    controller.abort();
  }
}
