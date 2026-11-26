import {
  isBridgeOutQuote,
  isBridgeTransfer,
  isBridgeTransfers,
  settleWithin,
  transferPath,
  type BridgeOutQuoteRequest,
  type BridgeOutQuoteResponse,
  type BridgeTransfer,
} from "@loyal-labs/api";
import { runBridgeOutPreflight } from "@loyal-labs/wallet-core/lib";
import {
  ASSOCIATED_TOKEN_PROGRAM_ID,
  getAssociatedTokenAddressSync,
  TOKEN_PROGRAM_ID,
} from "@solana/spl-token";
import {
  type Connection,
  PublicKey,
  SystemProgram,
  VersionedTransaction,
} from "@solana/web3.js";
import bs58 from "bs58";
import { Buffer } from "buffer";

import { SOLANA_USDC_MINT_MAINNET } from "@/lib/solana/constants";
import { assertOwnVersionedSignature, type Signer } from "@/lib/wallet/signer";

const QUOTE_AGE_MS = 30_000;
const USDC = new PublicKey(SOLANA_USDC_MINT_MAINNET);
// Pinned by the Go bridge's Relay deposit_token guard.
const DEPOSIT_PROGRAM = "99vQwtBwYtrqqD9YSXbdum3KBdxPAVxYTaQ3cfnJSrN2";
const DEPOSIT_STATE = "Dodg2HifwU8rmaVVyMyUZDGTRbqAJTyVYxXPwcbNpBKc";
const VAULT = new PublicKey("7uTT8Xi5RWXzy7h9XL244GRgEycDYDhLjr3ZyNdXi8pZ");
const LOOKUP_TABLE = "Hm9fUgcn7qwDaiNTFiGh6pNtVATgnaRcmK6Bbx6EMZfP";
const VAULT_ATA = getAssociatedTokenAddressSync(USDC, VAULT, true).toBase58();

export type OutStep =
  | { kind: "form" }
  | { kind: "sending" }
  | { kind: "bridging" | "filled"; requestId: string }
  | { kind: "failed"; requestId: string | null; message: string };

export type Request = (
  path: string,
  options?: { method?: "GET" | "POST"; body?: unknown; signal?: AbortSignal }
) => Promise<unknown>;

export type ReviewedQuote = {
  wallet: string;
  input: BridgeOutQuoteRequest;
  response: BridgeOutQuoteResponse;
  requestedAt: number;
};

export function relayTransactionUrl(requestId: string | null) {
  return typeof requestId === "string" &&
    /^[a-zA-Z0-9_-]{1,128}$/.test(requestId)
    ? `https://relay.link/transaction/${requestId}`
    : null;
}

export class QuoteExpired extends Error {
  constructor() {
    super("The quote expired. Review the new quote and send again.");
  }
}

function checkAge(review: ReviewedQuote) {
  const elapsed = Date.now() - review.requestedAt;
  if (elapsed < 0 || elapsed >= QUOTE_AGE_MS) throw new QuoteExpired();
}

function assertDeposit(review: ReviewedQuote) {
  const { response: q, wallet, input } = review;
  const ix = q.instructions?.[0];
  const keys = ix?.keys;
  const expected = [
    DEPOSIT_STATE,
    wallet,
    wallet,
    VAULT.toBase58(),
    SOLANA_USDC_MINT_MAINNET,
    getAssociatedTokenAddressSync(USDC, new PublicKey(wallet), true).toBase58(),
    VAULT_ATA,
    TOKEN_PROGRAM_ID.toBase58(),
    ASSOCIATED_TOKEN_PROGRAM_ID.toBase58(),
    SystemProgram.programId.toBase58(),
  ];
  if (
    q.instructions?.length !== 1 ||
    ix?.programId !== DEPOSIT_PROGRAM ||
    !keys ||
    keys.length !== expected.length ||
    !expected.every(
      (key, i) =>
        keys[i]?.pubkey === key &&
        keys[i].isSigner === (i === 1) &&
        keys[i].isWritable === (i === 1 || i === 5 || i === 6)
    ) ||
    (q.lookupTables ?? []).some(
      (table, i) => i !== 0 || table !== LOOKUP_TABLE
    ) ||
    !/^0b9c60da27a3b413[\da-f]{80}$/i.test(ix.data) ||
    Buffer.from(ix.data, "hex").readBigUInt64LE(8) !== BigInt(input.amountRaw)
  )
    throw new Error(
      "The quoted transaction does not match the requested Solana USDC deposit."
    );
}

export async function quoteOut(
  request: Request,
  wallet: string,
  input: BridgeOutQuoteRequest,
  signal: AbortSignal
) {
  const requestedAt = Date.now();
  const q = await request("/api/funding/bridge/out/quote", {
    method: "POST",
    body: input,
    signal,
  });
  if (
    !isBridgeOutQuote(q) ||
    !relayTransactionUrl(q.requestId) ||
    ![q.amountOutRaw, q.amountOutMinRaw, q.loyalFeeRaw].every((v) =>
      /^\d{1,78}$/.test(v)
    ) ||
    BigInt(q.amountOutMinRaw) <= BigInt(0) ||
    BigInt(q.amountOutMinRaw) > BigInt(q.amountOutRaw) ||
    BigInt(q.loyalFeeRaw) > BigInt(input.amountRaw) ||
    ![q.loyalFeeBps, q.slippageBps].every(
      (v) => Number.isInteger(v) && v >= 0 && v <= 10_000
    ) ||
    ![q.rate, q.amountInUsd, q.amountOutUsd, q.networkFeeUsd].every(
      (v) => typeof v === "string"
    ) ||
    !(
      q.etaSeconds === null ||
      (Number.isSafeInteger(q.etaSeconds) && q.etaSeconds >= 0)
    )
  )
    throw new Error("The bridge returned an invalid quote. Please try again.");
  const review = { wallet, input, response: q, requestedAt };
  assertDeposit(review);
  checkAge(review);
  return review;
}

export async function readTransfers(request: Request, signal: AbortSignal) {
  const result = await request("/api/funding/bridge/transfers?direction=out", {
    signal,
  });
  if (
    !isBridgeTransfers(result) ||
    result.transfers?.some((row) => row.direction !== "out")
  )
    throw new Error("Could not verify this wallet's outgoing transfers.");
  return result.transfers ?? [];
}

type Receipt = {
  requestId: string;
  signature: string;
  lastValidBlockHeight: number;
};
type Storage = {
  getString: (key: string) => string | undefined;
  setString: (key: string, value: string) => void;
  delete: (key: string) => void;
};

export function outgoingReceipt(storage: Storage, wallet: string) {
  const key = `loyal:bridge-out:mainnet:${wallet}`;
  function read(): Receipt | null {
    const value = storage.getString(key);
    if (value === undefined) return null;
    const row = JSON.parse(value) as Receipt;
    if (
      !row ||
      !relayTransactionUrl(row.requestId) ||
      typeof row.signature !== "string" ||
      bs58.decode(row.signature).length !== 64 ||
      !Number.isSafeInteger(row.lastValidBlockHeight) ||
      row.lastValidBlockHeight <= 0
    )
      throw new Error("Could not read the pending bridge transfer.");
    return row;
  }
  return {
    read,
    remember(row: Receipt) {
      if (read())
        throw new Error("This wallet already has a pending bridge transfer.");
      const value = JSON.stringify(row);
      storage.setString(key, value);
      if (storage.getString(key) !== value)
        throw new Error("Could not save the transfer before sending.");
    },
    forget(row: Receipt) {
      if (read()?.signature === row.signature) storage.delete(key);
    },
  };
}
type Receipts = ReturnType<typeof outgoingReceipt>;

export function inFlightTransfer(
  rows: BridgeTransfer[],
  ignored: ReadonlySet<string>
) {
  return rows.find(
    (row) =>
      row.inFlight && (row.state !== "quoted" || !ignored.has(row.requestId))
  );
}

export function transferStep(row: BridgeTransfer): OutStep {
  if (row.state === "quoted" && !row.inFlight)
    return {
      kind: "failed",
      requestId: row.requestId,
      message: "The bridge approval expired. Check your wallet activity before trying again.",
    };
  if (row.state === "failed" || row.state === "refunded") {
    const message =
      row.state === "refunded"
        ? "Refunded to your Solana wallet."
        : "The transfer failed.";
    return { kind: "failed", requestId: row.requestId, message };
  }
  return {
    kind: row.state === "filled" ? "filled" : "bridging",
    requestId: row.requestId,
  };
}

export async function sendReviewedQuote(options: {
  review: ReviewedQuote;
  request: Request;
  connection: Connection;
  currentSigner: () => Promise<Signer>;
  assertCurrent: () => void;
  receipts: Receipts;
  signal: AbortSignal;
  ignored: ReadonlySet<string>;
}): Promise<OutStep> {
  const {
    review,
    request,
    connection,
    currentSigner,
    assertCurrent,
    receipts,
    signal,
  } = options;
  const { wallet, input, response } = review;
  const pending = receipts.read();
  if (pending) return { kind: "bridging", requestId: pending.requestId };
  const check = () => {
    assertCurrent();
    checkAge(review);
  };
  const owner = new PublicKey(wallet);
  async function signer() {
    const active = await currentSigner();
    check();
    if (!active.publicKey.equals(owner))
      throw new Error("The active wallet changed. Review the transfer again.");
    return active;
  }
  check();
  assertDeposit(review);
  await signer();
  const verdict = await runBridgeOutPreflight({
    connection,
    owner,
    quote: response,
    amountRaw: BigInt(input.amountRaw),
  });
  check();
  if (!verdict.ok) {
    if (verdict.reason === "insufficient SOL for the network fee")
      throw new Error("Add SOL to your Solana wallet for the network fee.");
    throw new Error(
      verdict.retry
        ? "Could not verify the transfer yet. Please try again."
        : "The transfer did not pass simulation. Your USDC stays in your wallet."
    );
  }
  if (
    !Number.isSafeInteger(verdict.lastValidBlockHeight) ||
    verdict.lastValidBlockHeight <= 0
  )
    throw new Error("Could not check the transaction expiry.");
  const transfers = await readTransfers(request, signal);
  check();
  const blocking = inFlightTransfer(transfers, options.ignored);
  if (blocking) return transferStep(blocking);
  const confirmed = await request(transferPath(response.requestId, "confirm"), {
    method: "POST",
    body: {},
    signal,
  });
  check();
  if (
    !isBridgeTransfer(confirmed) ||
    confirmed.direction !== "out" ||
    confirmed.state !== "quoted" ||
    !confirmed.inFlight ||
    !confirmed.confirmedAt ||
    confirmed.originTx !== null ||
    confirmed.requestId !== response.requestId ||
    confirmed.chain !== input.chain ||
    confirmed.asset !== input.asset ||
    confirmed.amountInRaw !== input.amountRaw ||
    confirmed.amountOutRaw !== response.amountOutRaw
  )
    throw new Error("The bridge did not confirm the reviewed transfer.");

  const signingWallet = await signer();
  const message = Buffer.from(verdict.transaction.message.serialize());
  const returned = await settleWithin(
    signingWallet.signTransaction(verdict.transaction)
  );
  const wire = Uint8Array.from(returned.serialize());
  const signed = VersionedTransaction.deserialize(wire);
  if (
    !message.equals(Buffer.from(signed.message.serialize())) ||
    signed.message.header.numRequiredSignatures !== 1
  )
    throw new Error(
      "The wallet changed the checked transaction. Your USDC stays in your wallet."
    );
  await assertOwnVersionedSignature(signed, owner, 0);
  await signer();
  check();
  const signature = bs58.encode(signed.signatures[0]);
  receipts.remember({
    requestId: response.requestId,
    signature,
    lastValidBlockHeight: verdict.lastValidBlockHeight,
  });
  // After this boundary a timeout can hide a broadcast. Reconcile this signature, never sign a replacement.
  try {
    await settleWithin(
      connection.sendRawTransaction(wire, {
        skipPreflight: false,
        preflightCommitment: "confirmed",
        maxRetries: 3,
      }),
      15_000
    );
  } catch {
    // Reconciliation also handles a signature lost between persistence and the RPC call.
  }
  return { kind: "bridging", requestId: response.requestId };
}

export async function reconcileReceipt(
  receipts: Receipts,
  receipt: Receipt,
  transfers: BridgeTransfer[],
  request: Request,
  connection: Connection,
  signal: AbortSignal
): Promise<OutStep> {
  const { requestId, signature, lastValidBlockHeight } = receipt;
  function recordedStep(row: BridgeTransfer) {
    if (row.originTx !== signature)
      throw new Error(
        "The recorded bridge signature differs from this wallet's receipt."
      );
    // Keep the request through settlement so reopening after a fill restores its result.
    if (row.state !== "submitted" || !row.inFlight) receipts.forget(receipt);
    return transferStep(row);
  }
  const recorded = transfers.find(
    (row) => row.requestId === requestId && row.state !== "quoted"
  );
  if (recorded) return recordedStep(recorded);
  // Read the finalized height first. A missing status before the blockhash expires is inconclusive.
  const finalized = await connection.getEpochInfo("finalized");
  const {
    context,
    value: [status],
  } = await connection.getSignatureStatuses([signature], {
    searchTransactionHistory: true,
  });
  if (signal.aborted) throw new Error("Transfer check canceled.");
  if (
    status === null &&
    finalized.blockHeight !== undefined &&
    finalized.blockHeight > lastValidBlockHeight &&
    context.slot >= finalized.absoluteSlot
  ) {
    receipts.forget(receipt);
    return {
      kind: "failed",
      requestId,
      message: "The Solana transaction expired before it landed.",
    };
  }
  if (
    status?.confirmationStatus !== "confirmed" &&
    status?.confirmationStatus !== "finalized"
  )
    return { kind: "bridging", requestId };
  if (status.err) {
    receipts.forget(receipt);
    return {
      kind: "failed",
      requestId,
      message: "The Solana transaction failed.",
    };
  }
  const row = await request(transferPath(requestId, "submit"), {
    method: "POST",
    body: { originTx: signature },
    signal,
  });
  if (
    !isBridgeTransfer(row) ||
    row.requestId !== requestId ||
    row.direction !== "out" ||
    row.state === "quoted" ||
    row.originTx !== signature
  )
    throw new Error("Could not verify the recorded transfer.");
  return recordedStep(row);
}
