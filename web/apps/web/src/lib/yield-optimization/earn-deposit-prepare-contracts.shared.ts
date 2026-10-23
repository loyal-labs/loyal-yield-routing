import type { WireSmartAccountPreparedEarnUsdcDeposit } from "@loyal-labs/smart-account-vaults";
import { PublicKey } from "@solana/web3.js";

export type EarnDepositPrepareRequestBody = {
  amountRaw: string;
  mint: string;
};

export type EarnDepositPrepareResponse = {
  preparedDeposit: WireSmartAccountPreparedEarnUsdcDeposit;
};

type EarnDepositPrepareRecord = Record<string, unknown>;

function assertRequestObject(body: unknown): EarnDepositPrepareRecord {
  if (!body || typeof body !== "object") {
    throw new Error("Request body must be an object.");
  }

  return body as EarnDepositPrepareRecord;
}

function readUnsignedIntegerString(
  body: EarnDepositPrepareRecord,
  key: string
): string {
  const value = body[key];

  if (typeof value !== "string" || !/^\d+$/.test(value.trim())) {
    throw new Error(`${key} must be an unsigned integer string.`);
  }

  return value.trim();
}

export function parseEarnDepositPrepareRequestBody(body: unknown): {
  amountRaw: bigint;
  mint: string;
} {
  const record = assertRequestObject(body);
  const amountRaw = BigInt(readUnsignedIntegerString(record, "amountRaw"));

  if (amountRaw <= BigInt(0)) {
    throw new Error("amountRaw must be greater than 0.");
  }

  const mintValue = record.mint;
  if (typeof mintValue !== "string") {
    throw new Error("mint must be a mint public key.");
  }
  const mint = new PublicKey(mintValue).toBase58();

  return { amountRaw, mint };
}
