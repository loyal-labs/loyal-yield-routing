import type { Connection, TransactionError } from "@solana/web3.js";

const STATUS_POLL_MS = 2_000;
const POLL_DEADLINE_MS = 120_000;

type Landing = { err: TransactionError | null };

export class TransactionStatusUnknownError extends Error {
  readonly signature: string;

  constructor(signature: string) {
    super("Do not retry. Check the transaction below first.");
    this.name = "TransactionStatusUnknownError";
    this.signature = signature;
  }
}

async function readLanding(
  connection: Connection,
  signature: string
): Promise<Landing | null> {
  try {
    const { value } = await connection.getSignatureStatuses([signature], {
      searchTransactionHistory: true,
    });
    const status = value[0];
    return status?.confirmationStatus === "confirmed" ||
      status?.confirmationStatus === "finalized"
      ? { err: status.err }
      : null;
  } catch {
    return null;
  }
}

async function pollForLanding(
  connection: Connection,
  signature: string,
  stop: AbortSignal
): Promise<Landing | null> {
  const deadline = Date.now() + POLL_DEADLINE_MS;
  while (!stop.aborted && Date.now() < deadline) {
    await new Promise((resolve) => setTimeout(resolve, STATUS_POLL_MS));
    const landing = stop.aborted
      ? null
      : await readLanding(connection, signature);
    if (landing) {
      return landing;
    }
  }
  return null;
}

/**
 * Resolves once the transaction is confirmed and throws if it failed on chain.
 * confirmTransaction alone misses a transaction that lands while its socket is
 * down: it reads the status once, after the first subscribe, so it reports the
 * blockhash as expired and a manual retry moves the money twice. When the chain
 * still has no answer at expiry it throws TransactionStatusUnknownError, and the
 * caller must not offer a retry that signs again.
 */
export async function confirmSentTransaction(
  connection: Connection,
  sent: { blockhash: string; lastValidBlockHeight: number; signature: string }
): Promise<void> {
  const stop = new AbortController();
  const subscribed = connection
    .confirmTransaction({ ...sent, abortSignal: stop.signal }, "confirmed")
    .then((confirmation): Landing => ({ err: confirmation.value.err }))
    .catch(() => null);
  const landing =
    (await Promise.race([
      subscribed,
      pollForLanding(connection, sent.signature, stop.signal),
    ])) ?? (await readLanding(connection, sent.signature));
  stop.abort();
  if (!landing) {
    throw new TransactionStatusUnknownError(sent.signature);
  }
  if (landing.err) {
    throw new Error(`Transaction failed: ${JSON.stringify(landing.err)}`);
  }
}
