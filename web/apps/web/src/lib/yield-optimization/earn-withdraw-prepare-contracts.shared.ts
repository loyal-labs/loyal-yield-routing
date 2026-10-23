import type { WireSmartAccountPreparedEarnUsdcWithdraw } from "@loyal-labs/smart-account-vaults";

export type EarnWithdrawPrepareRequestBody = {
  amountRaw: string | "max";
  sourceId: string;
};

// Legacy wire shape: `{ amountRaw, mode, source }`. Every mobile binary and
// OTA shipped before the sourceId contract still sends it, and the change
// silently 400'd the whole mobile withdraw fleet (ASK-2099) — so the parser
// accepts both shapes and resolution maps legacy requests with the matcher
// that contract used. Delete once the mobile fleet speaks sourceId.
export type EarnWithdrawLegacySourceRequest = {
  amountRaw?: string;
  id: string;
  liquidityMint?: string;
  market?: string | null;
  mint?: string;
  reserve?: string;
  tokenAccount?: string;
  type: "reserve" | "idle";
} | null;

export type EarnWithdrawLegacyPrepareRequest = {
  mode: "partial" | "full";
  source: EarnWithdrawLegacySourceRequest;
};

export type EarnWithdrawPrepareResponse = {
  preparedWithdraw: WireSmartAccountPreparedEarnUsdcWithdraw;
};

type EarnWithdrawPrepareRecord = Record<string, unknown>;

function assertRequestObject(body: unknown): EarnWithdrawPrepareRecord {
  if (!body || typeof body !== "object") {
    throw new Error("Request body must be an object.");
  }

  return body as EarnWithdrawPrepareRecord;
}

function readUnsignedIntegerString(
  body: EarnWithdrawPrepareRecord,
  key: string
): string {
  const value = body[key];

  if (typeof value !== "string" || !/^\d+$/.test(value.trim())) {
    throw new Error(`${key} must be an unsigned integer string.`);
  }

  return value.trim();
}

export function parseEarnWithdrawPrepareRequestBody(body: unknown): {
  amountRaw: bigint | "max";
  sourceId: string | null;
  legacy: EarnWithdrawLegacyPrepareRequest | null;
} {
  const record = assertRequestObject(body);
  const amountValue = record.amountRaw;
  const amountRaw =
    amountValue === "max"
      ? "max"
      : BigInt(readUnsignedIntegerString(record, "amountRaw"));

  if (amountRaw !== "max" && amountRaw <= BigInt(0)) {
    throw new Error("amountRaw must be greater than 0.");
  }

  if (
    record.sourceId === undefined &&
    (record.mode === "partial" || record.mode === "full")
  ) {
    if (amountRaw === "max") {
      throw new Error("amountRaw must be an unsigned integer string.");
    }
    return {
      amountRaw,
      sourceId: null,
      legacy: { mode: record.mode, source: readLegacyWithdrawSource(record) },
    };
  }

  const sourceId = record.sourceId;
  if (typeof sourceId !== "string" || sourceId.trim().length === 0) {
    throw new Error("sourceId must be a non-empty string.");
  }

  return {
    amountRaw,
    sourceId: sourceId.trim(),
    legacy: null,
  };
}

function readLegacyWithdrawSource(
  body: EarnWithdrawPrepareRecord
): EarnWithdrawLegacySourceRequest {
  const value = body.source;
  if (value === undefined || value === null) {
    return null;
  }
  if (typeof value !== "object") {
    throw new Error("source must be an object when provided.");
  }
  const source = value as EarnWithdrawPrepareRecord;
  const type = source.type;
  if (type !== "reserve" && type !== "idle") {
    throw new Error("source.type must be reserve or idle.");
  }
  const id = source.id;
  if (typeof id !== "string" || id.trim().length === 0) {
    throw new Error("source.id must be a non-empty string.");
  }

  return {
    amountRaw:
      typeof source.amountRaw === "string" && /^\d+$/.test(source.amountRaw)
        ? source.amountRaw
        : undefined,
    id: id.trim(),
    liquidityMint:
      typeof source.liquidityMint === "string"
        ? source.liquidityMint.trim()
        : undefined,
    market:
      typeof source.market === "string"
        ? source.market.trim()
        : source.market === null
        ? null
        : undefined,
    mint: typeof source.mint === "string" ? source.mint.trim() : undefined,
    reserve:
      typeof source.reserve === "string" ? source.reserve.trim() : undefined,
    tokenAccount:
      typeof source.tokenAccount === "string"
        ? source.tokenAccount.trim()
        : undefined,
    type,
  };
}
