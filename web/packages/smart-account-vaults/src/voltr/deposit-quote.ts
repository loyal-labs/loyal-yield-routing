import { PublicKey } from "@solana/web3.js";
import { Buffer } from "buffer";

import type { BytePin } from "./lighthouse";
import { VOLTR_VAULT_SIZE } from "./program";

export const EARN_MAX_DEPOSIT_QUOTE_PATH =
  "/api/smart-accounts/earn-max/deposit-quote";

/** One certified pricing basis for one official deposit of `amountRaw`. */
export type EarnMaxDepositQuote = {
  amountRaw: bigint;
  authority: PublicKey;
  certificateId: string;
  certifiedSlot: bigint;
  expiresAt: bigint;
  lpAta: PublicKey;
  lpBaselineRaw: bigint;
  lpMaxRaw: bigint;
  lpMinRaw: bigint;
  lpMint: PublicKey;
  lpSupplyRaw: bigint;
  notAfterSlot: bigint;
  supplyMaxRaw: bigint;
  vault: PublicKey;
  vaultPins: BytePin[];
};

const HELD_MESSAGES = {
  release_control_held:
    "Earn MAX deposits are paused right now. Try again later.",
  owner_not_admitted: "This account cannot deposit into Earn MAX yet.",
  amount_over_canary_limit:
    "This amount is above the current Earn MAX deposit limit. Try a smaller amount.",
  nav_uncertified:
    "Earn MAX cannot confirm the vault price right now. Try again in a few minutes.",
  deposit_waiting_nav_publication:
    "Earn MAX is waiting for the next vault price update. Try again in a few minutes.",
} as const;

export type EarnMaxDepositHeldReason = keyof typeof HELD_MESSAGES;

export type EarnMaxDepositQuoteErrorCode =
  | EarnMaxDepositHeldReason
  | "deposit_unavailable"
  | "invalid_quote"
  | "quote_expired";

const OTHER_MESSAGES = {
  deposit_unavailable:
    "Earn MAX deposits are unavailable right now. Try again later.",
  invalid_quote: "Earn MAX could not verify the deposit price. Try again.",
  quote_expired: "The deposit price expired. Try again.",
} as const;

export class EarnMaxDepositQuoteError extends Error {
  readonly code: EarnMaxDepositQuoteErrorCode;

  constructor(code: EarnMaxDepositQuoteErrorCode) {
    super(
      code in HELD_MESSAGES
        ? HELD_MESSAGES[code as EarnMaxDepositHeldReason]
        : OTHER_MESSAGES[code as keyof typeof OTHER_MESSAGES],
    );
    this.name = "EarnMaxDepositQuoteError";
    this.code = code;
  }
}

const MAX_U64 = (BigInt(1) << BigInt(64)) - BigInt(1);

function u64(value: unknown): bigint {
  if (typeof value !== "string" || !/^(0|[1-9][0-9]{0,19})$/.test(value))
    throw new EarnMaxDepositQuoteError("invalid_quote");
  const parsed = BigInt(value);
  if (parsed > MAX_U64) throw new EarnMaxDepositQuoteError("invalid_quote");
  return parsed;
}

function key(value: unknown): PublicKey {
  if (typeof value !== "string")
    throw new EarnMaxDepositQuoteError("invalid_quote");
  try {
    return new PublicKey(value);
  } catch {
    throw new EarnMaxDepositQuoteError("invalid_quote");
  }
}

function pins(value: unknown): BytePin[] {
  if (!Array.isArray(value) || value.length === 0)
    throw new EarnMaxDepositQuoteError("invalid_quote");
  let end = 0;
  return value.map((pin: unknown) => {
    const { offset, hex } = (pin ?? {}) as { offset?: unknown; hex?: unknown };
    if (
      typeof offset !== "number" ||
      !Number.isSafeInteger(offset) ||
      offset < end ||
      typeof hex !== "string" ||
      hex.length === 0 ||
      !/^([0-9a-f]{2})+$/.test(hex) ||
      offset + hex.length / 2 > VOLTR_VAULT_SIZE
    )
      throw new EarnMaxDepositQuoteError("invalid_quote");
    end = offset + hex.length / 2;
    return { offset, bytes: Uint8Array.from(Buffer.from(hex, "hex")) };
  });
}

export function parseEarnMaxDepositQuote(body: unknown): EarnMaxDepositQuote {
  if (typeof body !== "object" || body === null)
    throw new EarnMaxDepositQuoteError("invalid_quote");
  const b = body as Record<string, unknown>;
  if (typeof b.certificateId !== "string" || b.certificateId.length === 0)
    throw new EarnMaxDepositQuoteError("invalid_quote");
  return {
    amountRaw: u64(b.amountRaw),
    authority: key(b.authority),
    certificateId: b.certificateId,
    certifiedSlot: u64(b.certifiedSlot),
    expiresAt: u64(b.expiresAt),
    lpAta: key(b.lpAta),
    lpBaselineRaw: u64(b.lpBaselineRaw),
    lpMaxRaw: u64(b.lpMaxRaw),
    lpMinRaw: u64(b.lpMinRaw),
    lpMint: key(b.lpMint),
    lpSupplyRaw: u64(b.lpSupplyRaw),
    notAfterSlot: u64(b.notAfterSlot),
    supplyMaxRaw: u64(b.supplyMaxRaw),
    vault: key(b.vault),
    vaultPins: pins(b.vaultPins),
  };
}

/** The base client: web passes a credentialed fetch, mobile its Earn bearer read. */
export type EarnMaxApiRequest = (
  path: string,
  init: { body: string; headers: Record<string, string>; method: "POST" },
) => Promise<Response>;

export async function fetchEarnMaxDepositQuote(
  request: EarnMaxApiRequest,
  amountRaw: bigint,
): Promise<EarnMaxDepositQuote> {
  const response = await request(EARN_MAX_DEPOSIT_QUOTE_PATH, {
    body: JSON.stringify({ amountRaw: amountRaw.toString() }),
    headers: { "Content-Type": "application/json" },
    method: "POST",
  });
  if (response.status === 409) {
    const payload = (await response.json().catch(() => null)) as {
      reason?: unknown;
    } | null;
    const reason = payload?.reason;
    throw new EarnMaxDepositQuoteError(
      typeof reason === "string" && reason in HELD_MESSAGES
        ? (reason as EarnMaxDepositHeldReason)
        : "deposit_unavailable",
    );
  }
  if (!response.ok) throw new EarnMaxDepositQuoteError("deposit_unavailable");
  const quote = parseEarnMaxDepositQuote(
    await response.json().catch(() => null),
  );
  if (quote.amountRaw !== amountRaw)
    throw new EarnMaxDepositQuoteError("invalid_quote");
  return quote;
}
