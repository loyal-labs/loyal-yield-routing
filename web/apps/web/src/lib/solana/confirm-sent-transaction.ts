import type { Connection, TransactionError } from "@solana/web3.js";

const STATUS_POLL_MS = 2000;
const READ_TIMEOUT_MS = 10_000;
const SETTLE_DEADLINE_MS = 120_000;

export type SentTransaction = {
  lastValidBlockHeight: number;
  signature: string;
};

export type SentOutcome =
  | { kind: "landed" }
  | { kind: "failed"; err: TransactionError }
  | { kind: "status-unknown" };

const STATUS_UNKNOWN: SentOutcome = { kind: "status-unknown" };

function outcomeOf(err: TransactionError | null): SentOutcome {
  return err ? { kind: "failed", err } : { kind: "landed" };
}

function waitFor(ms: number, stop: AbortSignal): Promise<void> {
  return new Promise((resolve) => {
    const finish = () => {
      clearTimeout(timer);
      stop.removeEventListener("abort", finish);
      resolve();
    };
    const timer = setTimeout(finish, ms);
    stop.addEventListener("abort", finish);
  });
}

async function readOrNull<T>(read: () => Promise<T>): Promise<T | null> {
  let timer: ReturnType<typeof setTimeout> | undefined;
  try {
    return await Promise.race([
      read(),
      new Promise<null>((resolve) => {
        timer = setTimeout(() => resolve(null), READ_TIMEOUT_MS);
      }),
    ]);
  } catch {
    return null;
  } finally {
    clearTimeout(timer);
  }
}

// A `processed` status, err included, can still change on another fork, so only
// the confirmed and finalized ones answer.
async function readSettledStatus(
  connection: Connection,
  signature: string
): Promise<SentOutcome | null> {
  const response = await readOrNull(() =>
    connection.getSignatureStatuses([signature], {
      searchTransactionHistory: true,
    })
  );
  const status = response?.value[0];
  return status?.confirmationStatus === "confirmed" ||
    status?.confirmationStatus === "finalized"
    ? outcomeOf(status.err)
    : null;
}

async function pollUntilSettled(
  connection: Connection,
  { lastValidBlockHeight, signature }: SentTransaction,
  stop: AbortSignal
): Promise<SentOutcome> {
  const deadline = Date.now() + SETTLE_DEADLINE_MS;
  while (!stop.aborted && Date.now() < deadline) {
    const settled = await readSettledStatus(connection, signature);
    if (settled) {
      return settled;
    }
    const height = await readOrNull(() =>
      connection.getBlockHeight("confirmed")
    );
    if (height !== null && height > lastValidBlockHeight) {
      break;
    }
    await waitFor(STATUS_POLL_MS, stop);
  }
  if (stop.aborted) {
    return STATUS_UNKNOWN;
  }
  return (await readSettledStatus(connection, signature)) ?? STATUS_UNKNOWN;
}

/**
 * Answers what became of a transaction that has already been broadcast, and
 * never rejects. status-unknown means the chain gave no answer before expiry, so
 * the money may have moved and the caller must not offer to sign again.
 *
 * This replaces confirmTransaction, which reads the status once after the first
 * subscribe: a transaction that lands while its socket is down, or behind a 429
 * on that one read, comes back as expired. Its first read also rejects on a
 * `processed` err with a bare object, and a 429 there is an unhandled rejection.
 */
export async function settleSentTransaction(
  connection: Connection,
  sent: SentTransaction
): Promise<SentOutcome> {
  const stop = new AbortController();
  let subscription: number | undefined;
  try {
    const notified = new Promise<SentOutcome>((resolve) => {
      subscription = connection.onSignature(
        sent.signature,
        ({ err }) => {
          subscription = undefined;
          resolve(outcomeOf(err));
        },
        "confirmed"
      );
    });
    return await Promise.race([
      notified,
      pollUntilSettled(connection, sent, stop.signal),
    ]);
  } catch {
    return STATUS_UNKNOWN;
  } finally {
    stop.abort();
    if (subscription !== undefined) {
      connection.removeSignatureListener(subscription).catch(() => undefined);
    }
  }
}

export function transactionFailedMessage(err: TransactionError): string {
  return `Transaction failed: ${JSON.stringify(err)}`;
}

// The same two field names the errors from sendPreparedWithWallet carry and
// classifyTransactionFailure reads, so "submitted, unresolved" has one spelling.
export type TransactionStatusUnknown = {
  error: string;
  success: false;
  transactionSignature: string;
  transactionWasSubmitted: true;
};

export function transactionStatusUnknown(
  transactionSignature: string
): TransactionStatusUnknown {
  return {
    error: "Do not retry. Check the transaction below first.",
    success: false,
    transactionSignature,
    transactionWasSubmitted: true,
  };
}
