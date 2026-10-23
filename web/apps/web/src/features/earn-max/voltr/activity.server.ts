import "server-only";

import {
  type Connection,
  type ParsedTransactionWithMeta,
  type PartiallyDecodedInstruction,
  PublicKey,
  type TokenBalance,
} from "@solana/web3.js";
import bs58 from "bs58";

import type { EarnMaxActivityItem, EarnMaxPerformancePoint } from "../types";
import {
  VOLTR_IDLE_ATA,
  VOLTR_LP_MINT,
  VOLTR_PROGRAM_ID,
  VOLTR_VAULT,
  type VoltrPosition,
} from "@loyal-labs/smart-account-vaults";

const HISTORY_LIMIT = 100;

// First 8 data bytes of the Voltr instructions a user signs (program.ts).
const ACTIONS: [
  action: VoltrHistoryEntry["action"],
  discriminator: number[]
][] = [
  ["deposit", [126, 224, 21, 255, 228, 53, 117, 33]],
  ["withdraw_request", [248, 225, 47, 22, 116, 144, 23, 143]],
  ["claim", [135, 7, 237, 120, 149, 94, 95, 7]],
];

type VoltrHistoryEntry = {
  confirmedSlot?: string;
  lpDeltaRaw: bigint;
  signature: string;
  timestamp: string;
} & (
  | { action: "deposit" | "claim"; amountRaw: bigint }
  | {
      action: "withdraw_request";
      amountRaw: bigint | null;
      requestedRaw: bigint | null;
    }
);

export type EarnMaxVoltrHistory = {
  complete: boolean;
  entries: VoltrHistoryEntry[];
};

function voltrAction(
  instruction: PartiallyDecodedInstruction,
  authority: PublicKey
): { action: VoltrHistoryEntry["action"]; data: Buffer } | null {
  if (
    !instruction.programId.equals(VOLTR_PROGRAM_ID) ||
    !instruction.accounts.some((key) => key.equals(VOLTR_VAULT)) ||
    !instruction.accounts.some((key) => key.equals(authority))
  ) {
    return null;
  }
  const data = Buffer.from(bs58.decode(instruction.data));
  const match = ACTIONS.find(([, discriminator]) =>
    discriminator.every((byte, index) => data[index] === byte)
  );
  return match ? { action: match[0], data } : null;
}

function idleDeltaRaw(transaction: ParsedTransactionWithMeta): bigint {
  const idle = VOLTR_IDLE_ATA.toBase58();
  const keys = transaction.transaction.message.accountKeys;
  const amount = (balances: TokenBalance[] | null | undefined) =>
    BigInt(
      balances?.find((b) => keys[b.accountIndex]?.pubkey.toBase58() === idle)
        ?.uiTokenAmount.amount ?? "0"
    );
  return (
    amount(transaction.meta?.postTokenBalances) -
    amount(transaction.meta?.preTokenBalances)
  );
}

function lpDeltaRaw(
  transaction: ParsedTransactionWithMeta,
  authority: PublicKey
): bigint {
  const owner = authority.toBase58();
  const mint = VOLTR_LP_MINT.toBase58();
  const amount = (balances: TokenBalance[] | null | undefined) =>
    BigInt(
      balances?.find((b) => b.owner === owner && b.mint === mint)?.uiTokenAmount
        .amount ?? "0"
    );
  return (
    amount(transaction.meta?.postTokenBalances) -
    amount(transaction.meta?.preTokenBalances)
  );
}

// RequestWithdrawVault event (Anchor "Program data:" log): escrowed LP at
// byte 114, requested asset value as U80F48 at byte 122. Only trusted when
// the LP matches this transaction's own LP move.
const REQUEST_EVENT = Buffer.from([59, 94, 26, 38, 47, 131, 158, 162]);

function requestedRawFromLogs(
  transaction: ParsedTransactionWithMeta,
  escrowedLp: bigint
): bigint | null {
  for (const line of transaction.meta?.logMessages ?? []) {
    if (!line.startsWith("Program data: ")) continue;
    const data = Buffer.from(line.slice(14), "base64");
    if (data.length < 138 || !data.subarray(0, 8).equals(REQUEST_EVENT))
      continue;
    if (data.readBigUInt64LE(114) !== escrowedLp) continue;
    const bits =
      data.readBigUInt64LE(122) | (data.readBigUInt64LE(130) << BigInt(64));
    return bits >> BigInt(48);
  }
  return null;
}

function decode(
  transaction: ParsedTransactionWithMeta,
  signature: string,
  authority: PublicKey
): VoltrHistoryEntry[] {
  const instructions = [
    ...transaction.transaction.message.instructions,
    ...(transaction.meta?.innerInstructions ?? []).flatMap(
      (i) => i.instructions
    ),
  ];
  const timestamp = new Date((transaction.blockTime ?? 0) * 1000).toISOString();
  const entries: VoltrHistoryEntry[] = [];
  for (const instruction of instructions) {
    if (!("data" in instruction)) continue;
    const match = voltrAction(instruction, authority);
    if (!match) continue;
    const lpDelta =
      entries.length === 0 ? lpDeltaRaw(transaction, authority) : BigInt(0);
    const base = {
      confirmedSlot: String(transaction.slot),
      lpDeltaRaw: lpDelta,
      signature,
      timestamp,
    };
    entries.push(
      match.action === "withdraw_request"
        ? {
            ...base,
            action: match.action,
            amountRaw: null,
            requestedRaw:
              lpDelta < BigInt(0)
                ? requestedRawFromLogs(transaction, -lpDelta)
                : null,
          }
        : {
            ...base,
            action: match.action,
            amountRaw:
              match.action === "deposit"
                ? match.data.readBigUInt64LE(8)
                : -idleDeltaRaw(transaction),
          }
    );
  }
  return entries;
}

const MAX_CACHED_HISTORIES = 256;
const cache = new Map<string, { newest: string; value: EarnMaxVoltrHistory }>();

export async function readVoltrHistory(
  connection: Connection,
  authority: PublicKey
): Promise<EarnMaxVoltrHistory> {
  const key = authority.toBase58();
  const pages: Awaited<ReturnType<Connection["getSignaturesForAddress"]>>[] =
    [];
  let before: string | undefined;
  let complete = false;
  do {
    const page = await connection.getSignaturesForAddress(authority, {
      limit: HISTORY_LIMIT,
      before,
    });
    pages.push(page);
    complete = page.length < HISTORY_LIMIT;
    before = page.at(-1)?.signature;
  } while (!complete && before !== undefined);
  const signatures = pages.flat().filter((s) => s.err === null);
  const newest = signatures[0]?.signature ?? "";
  const hit = cache.get(key);
  if (hit && hit.newest === newest) {
    cache.delete(key);
    cache.set(key, hit);
    return hit.value;
  }
  const transactions = [];
  for (let start = 0; start < signatures.length; start += HISTORY_LIMIT) {
    transactions.push(
      ...(await connection.getParsedTransactions(
        signatures.slice(start, start + HISTORY_LIMIT).map((s) => s.signature),
        { maxSupportedTransactionVersion: 0 }
      ))
    );
  }
  const entries = transactions.flatMap((transaction, index) =>
    transaction
      ? decode(transaction, signatures[index]!.signature, authority)
      : []
  );
  // A request moves LP, not USDC: show what its claim paid (or the pending
  // payout). Entries are newest first, so the claim precedes its request.
  let laterClaimRaw: bigint | null = null;
  for (const entry of entries) {
    if (entry.action === "claim") laterClaimRaw = entry.amountRaw;
    if (entry.action === "withdraw_request") {
      entry.amountRaw = laterClaimRaw;
      laterClaimRaw = null;
    }
  }
  const value = { complete, entries };
  cache.delete(key);
  cache.set(key, { newest, value });
  if (cache.size > MAX_CACHED_HISTORIES) {
    const oldest = cache.keys().next().value;
    if (oldest !== undefined) cache.delete(oldest);
  }
  return value;
}

const DUST_RAW = BigInt(10_000); // 1 cent
const withoutDust = (raw: bigint) =>
  raw < BigInt(0) && -raw < DUST_RAW ? BigInt(0) : raw;

export function voltrActivity(
  history: EarnMaxVoltrHistory,
  position: Pick<VoltrPosition, "lpRaw" | "valueRaw" | "withdrawal">,
  dayEnd: { at: string; price: number }[] = []
): {
  earnedRaw: bigint | null;
  operations: EarnMaxActivityItem[];
  performance: EarnMaxPerformancePoint[];
} {
  const entries = history.entries.map((entry) =>
    entry.action === "withdraw_request" &&
    entry.amountRaw === null &&
    position.withdrawal
      ? { ...entry, amountRaw: position.withdrawal.payoutRaw }
      : entry
  );
  const operations = entries.map((entry) => ({
    confirmedSlot: entry.confirmedSlot,
    action: entry.action,
    amountRaw: entry.amountRaw === null ? null : entry.amountRaw.toString(),
    id: `${entry.signature}:${entry.action}`,
    requestedRaw:
      entry.action === "withdraw_request" && entry.requestedRaw !== null
        ? entry.requestedRaw.toString()
        : null,
    signature: entry.signature,
    status: "confirmed",
    timestamp: entry.timestamp,
  }));
  const performance: EarnMaxPerformancePoint[] = [];
  let principal = BigInt(0);
  for (const entry of [...entries].reverse()) {
    if (entry.action === "deposit") principal += entry.amountRaw;
    else if (entry.action === "withdraw_request")
      principal -= entry.amountRaw ?? BigInt(0);
    else continue;
    performance.push({
      equityUsd: Number(principal < 0 ? 0 : principal) / 1_000_000,
      timestamp: entry.timestamp,
    });
  }
  if (performance.length > 0) {
    const opened = performance[0]!.timestamp;
    for (const day of history.complete ? dayEnd : []) {
      if (day.at <= opened) continue;
      const lpThen = entries
        .filter((e) => e.timestamp > day.at)
        .reduce((lp, e) => lp - e.lpDeltaRaw, position.lpRaw);
      if (lpThen < BigInt(0)) continue;
      performance.push({
        equityUsd: (Number(lpThen) * day.price) / 1_000_000,
        timestamp: day.at,
      });
    }
    performance.push({
      equityUsd: Number(position.valueRaw) / 1_000_000,
      timestamp: new Date().toISOString(),
    });
    performance.sort((a, b) => a.timestamp.localeCompare(b.timestamp));
  }
  const sum = (action: VoltrHistoryEntry["action"]) =>
    entries
      .filter((e) => e.action === action)
      .reduce((total, e) => total + (e.amountRaw ?? BigInt(0)), BigInt(0));
  const pendingRaw = position.withdrawal?.payoutRaw ?? BigInt(0);
  return {
    // Lifetime: what the user holds + is owed + got back, minus what went in.
    // Voltr rounds deposit LP down, so a fresh position reads a few raw
    // units under its deposit; hide that dust (< 1 cent), keep real losses.
    earnedRaw: history.complete
      ? withoutDust(
          position.valueRaw + pendingRaw + sum("claim") - sum("deposit")
        )
      : null,
    operations,
    performance,
  };
}
