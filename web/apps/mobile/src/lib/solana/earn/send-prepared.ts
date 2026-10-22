import {
  ComputeBudgetProgram,
  type Connection,
  type PublicKey,
  TransactionMessage,
  VersionedTransaction,
} from "@solana/web3.js";
import { Buffer } from "buffer";

import {
  WalletRejectedError,
  withLandedSignatures,
} from "@/lib/wallet/rejection";
import type { Signer } from "@/lib/wallet/signer";

import type { HydratedPreparedOperation } from "./wire";

export type SentTransaction = { signature: string; confirmedSlot: string };

// `confirmTransaction` (WebSocket/blockheight strategy) and a follow-up
// `getSignatureStatuses` read can land on different load-balanced RPC nodes, so
// the status read can briefly lag behind ("processed"/null) for a tx that is
// already confirmed. Re-read a few times before giving up so an already-landed
// transaction is never reported as failed.
const CONFIRMED_SLOT_MAX_ATTEMPTS = 8;
const CONFIRMED_SLOT_RETRY_MS = 600;
// Same lag can follow a (possibly false) blockheight-expiry — give the status a
// few quick reads before surfacing the expiry.
const POST_EXPIRY_ATTEMPTS = 3;

const PRIORITY_FEE_MICRO_LAMPORTS = 100_000;
// Solana's max serialized transaction size. The backend prepares txs up to this
// limit, so prepending the priority-fee instruction (~44 bytes) can push a fat
// first-deposit tx over it — Seed Vault then refuses to sign (result=1007) and
// the RPC would reject it anyway. When that happens, send without the fee:
// landing slowly beats not signing at all.
const MAX_TRANSACTION_BYTES = 1232;
// Rebroadcast cadence and hard ceilings. Every resend is a paid Helius
// `sendTransactionWithStake` call, and an uncapped 2s loop burned ~269K
// credits/day (ASK-2258). With the priority fee above, a 4s cadence still
// keeps the tx in front of leaders; the count/time caps guarantee a stuck
// confirmation (or an orphaned batch loop) can never resend forever. The
// 60s deadline matches blockhash validity — past it, resends are dead weight.
export const REBROADCAST_INTERVAL_MS = 4000;
export const REBROADCAST_MAX_RESENDS = 10;
export const REBROADCAST_MAX_ELAPSED_MS = 60_000;

const delay = (ms: number): Promise<void> =>
  new Promise((resolve) => setTimeout(resolve, ms));

async function readConfirmedSlot(
  connection: Connection,
  signature: string,
): Promise<string | null> {
  const { value } = await connection.getSignatureStatuses([signature], {
    searchTransactionHistory: true,
  });
  const status = value[0];
  if (status?.err) {
    throw new Error("Transaction failed on-chain.");
  }
  if (
    status &&
    typeof status.slot === "number" &&
    (status.confirmationStatus === "confirmed" ||
      status.confirmationStatus === "finalized")
  ) {
    return String(status.slot);
  }
  return null;
}

async function pollConfirmedSlot(
  connection: Connection,
  signature: string,
  attempts: number,
): Promise<string | null> {
  for (let attempt = 0; attempt < attempts; attempt++) {
    const slot = await readConfirmedSlot(connection, signature);
    if (slot !== null) {
      return slot;
    }
    if (attempt < attempts - 1) {
      await delay(CONFIRMED_SLOT_RETRY_MS);
    }
  }
  return null;
}

async function confirmSentTransaction(
  connection: Connection,
  signature: string,
  blockhash: string,
  lastValidBlockHeight: number,
): Promise<SentTransaction> {
  let contextSlot: number | null = null;
  try {
    const confirmation = await connection.confirmTransaction(
      { blockhash, lastValidBlockHeight, signature },
      "confirmed",
    );
    if (confirmation.value.err) {
      throw new Error("Transaction failed on-chain.");
    }
    contextSlot = confirmation.context.slot;
  } catch (error) {
    // A real on-chain failure must surface as-is. Otherwise the blockheight
    // strategy gave up — the signature status is the source of truth, so check
    // it before failing.
    if (
      error instanceof Error &&
      error.message === "Transaction failed on-chain."
    ) {
      throw error;
    }
    const slot = await pollConfirmedSlot(
      connection,
      signature,
      POST_EXPIRY_ATTEMPTS,
    );
    if (slot !== null) {
      return { signature, confirmedSlot: slot };
    }
    throw error;
  }

  const slot = await pollConfirmedSlot(
    connection,
    signature,
    CONFIRMED_SLOT_MAX_ATTEMPTS,
  );
  return { signature, confirmedSlot: slot ?? String(contextSlot) };
}

function compilePreparedOperation(
  operation: HydratedPreparedOperation,
  blockhash: string,
): VersionedTransaction {
  const compile = (withPriorityFee: boolean) =>
    new VersionedTransaction(
      new TransactionMessage({
        payerKey: operation.payer,
        recentBlockhash: blockhash,
        instructions: withPriorityFee
          ? [
              ComputeBudgetProgram.setComputeUnitPrice({
                microLamports: PRIORITY_FEE_MICRO_LAMPORTS,
              }),
              ...operation.instructions,
            ]
          : [...operation.instructions],
      }).compileToV0Message([...operation.lookupTableAccounts]),
    );
  const transaction = compile(true);
  if (transaction.serialize().length > MAX_TRANSACTION_BYTES) {
    return compile(false);
  }
  return transaction;
}

type InFlightTransaction = {
  signature: string;
  stop: () => Promise<void>;
};

// Sends an already-signed transaction and re-broadcasts it until `stop()` or
// the resend caps — under load, RPCs drop a pending tx and `confirmTransaction`
// never re-sends; it just waits out the blockhash and reports expiry.
// Re-broadcasting the signed raw tx keeps it in front of leaders until it
// lands. Callers stop the loop on confirmation, on-chain failure, or expiry.
async function startSendingSignedTransaction(
  connection: Connection,
  transaction: VersionedTransaction,
): Promise<InFlightTransaction> {
  const rawTransaction = transaction.serialize();
  const signature = await connection.sendRawTransaction(rawTransaction, {
    skipPreflight: false,
    maxRetries: 0,
  });

  let rebroadcasting = true;
  const deadline = Date.now() + REBROADCAST_MAX_ELAPSED_MS;
  let wakeRebroadcast: (() => void) | null = null;
  const waitForRebroadcast = (): Promise<void> =>
    new Promise((resolve) => {
      const timer = setTimeout(() => {
        wakeRebroadcast = null;
        resolve();
      }, REBROADCAST_INTERVAL_MS);
      wakeRebroadcast = () => {
        clearTimeout(timer);
        wakeRebroadcast = null;
        resolve();
      };
    });
  const rebroadcast = (async () => {
    for (let resend = 0; resend < REBROADCAST_MAX_RESENDS; resend++) {
      await waitForRebroadcast();
      if (!rebroadcasting || Date.now() >= deadline) {
        return;
      }
      try {
        await connection.sendRawTransaction(rawTransaction, {
          skipPreflight: true,
          maxRetries: 0,
        });
      } catch {
      }
    }
  })();

  return {
    signature,
    stop: async () => {
      rebroadcasting = false;
      wakeRebroadcast?.();
      await rebroadcast.catch(() => undefined);
    },
  };
}

async function sendSignedTransaction(
  connection: Connection,
  transaction: VersionedTransaction,
  blockhash: string,
  lastValidBlockHeight: number,
): Promise<SentTransaction> {
  const inFlight = await startSendingSignedTransaction(connection, transaction);
  try {
    return await confirmSentTransaction(
      connection,
      inFlight.signature,
      blockhash,
      lastValidBlockHeight,
    );
  } finally {
    await inFlight.stop();
  }
}

export type SendPreparedMode = "confirm-each" | "send-all-before-confirm";

// Signs a sequence of prepared operations in as few wallet prompts as the
// signer allows (Seed Vault batches them into a single authorization), then
// sends + confirms them strictly in order. All stages share one blockhash —
// its ~60s validity comfortably covers a few sequential confirmed-commitment
// landings, and `confirmSentTransaction` already tolerates false expiries. A
// stage failure aborts the remainder; the backend's prepare is chain-driven
// and resumes from whatever landed.
//
// `sendMode` (default "confirm-each") mirrors the web SDK's
// `sendPreparedBatchWithWallet`: "send-all-before-confirm" broadcasts every
// signed tx in order BEFORE waiting on any confirmation, so a later tx doesn't
// spend the shared blockhash window queued behind an earlier tx's
// confirmation. Only safe when the txs don't depend on each other's state
// (e.g. the autodeposit policy + delegation pair) — deposit/withdraw steps
// build on the previous tx's state and must stay on "confirm-each".
export async function signAndSendPreparedOperations(args: {
  connection: Connection;
  signer: Signer;
  operations: HydratedPreparedOperation[];
  sendMode?: SendPreparedMode;
}): Promise<SentTransaction[]> {
  const { connection, signer, operations, sendMode = "confirm-each" } = args;
  if (operations.length === 0) {
    return [];
  }
  // MWA wallet apps with transaction protection (Solflare, Seeker Wallet)
  // simulate each tx at signing time and append Lighthouse guard instructions
  // asserting the expected post-state. Batch signing computes every stage's
  // guards against the SAME pre-state, so once stage 1 lands, stage 2's
  // guards are stale and it fails on-chain with Lighthouse AssertionFailed
  // (custom error 0x1900). Sign each stage in its own wallet session, after
  // the previous stage confirmed, so the wallet simulates against real state.
  // Deeplink signers (Solflare on iOS) have the same transaction-protection
  // behavior, and each of their sign calls is a full app switch anyway.
  if (
    (signer.kind === "mwa" || signer.kind === "deeplink") &&
    operations.length > 1
  ) {
    const sent: SentTransaction[] = [];
    for (const operation of operations) {
      try {
        const [confirmed] = await signAndSendPreparedOperations({
          connection,
          signer,
          operations: [operation],
        });
        sent.push(confirmed);
      } catch (error) {
        if (sent.length > 0 && error instanceof WalletRejectedError) {
          throw withLandedSignatures(
            error,
            sent.map((tx) => tx.signature),
          );
        }
        throw error;
      }
    }
    return sent;
  }
  const { blockhash, lastValidBlockHeight } =
    await connection.getLatestBlockhash("confirmed");
  const transactions = operations.map((operation) =>
    compilePreparedOperation(operation, blockhash),
  );
  await signer.signAllTransactions(transactions);

  if (sendMode === "send-all-before-confirm" && transactions.length > 1) {
    const inFlight: InFlightTransaction[] = [];
    try {
      for (const transaction of transactions) {
        inFlight.push(
          await startSendingSignedTransaction(connection, transaction),
        );
      }
      const sent: SentTransaction[] = [];
      for (const flight of inFlight) {
        sent.push(
          await confirmSentTransaction(
            connection,
            flight.signature,
            blockhash,
            lastValidBlockHeight,
          ),
        );
      }
      return sent;
    } finally {
      for (const flight of inFlight) {
        await flight.stop();
      }
    }
  }

  const sent: SentTransaction[] = [];
  for (const transaction of transactions) {
    sent.push(
      await sendSignedTransaction(
        connection,
        transaction,
        blockhash,
        lastValidBlockHeight,
      ),
    );
  }
  return sent;
}

export async function signAndSendPreparedOperation(args: {
  connection: Connection;
  signer: Signer;
  operation: HydratedPreparedOperation;
}): Promise<SentTransaction> {
  const [sent] = await signAndSendPreparedOperations({
    connection: args.connection,
    signer: args.signer,
    operations: [args.operation],
  });
  return sent;
}
