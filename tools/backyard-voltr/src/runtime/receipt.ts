import {
  getRequestWithdrawVaultReceiptDecoder,
  getRequestWithdrawVaultReceiptDiscriminatorBytes,
} from "@voltr/vault-sdk";

import { PARTNER_ROUTE } from "../domain/route-spec.js";
import type { AccountSnapshot } from "../integrations/solana-compat.js";

export const RECEIPT_DATA_LENGTH = 112;
const RECEIPT_SDK_PREFIX_LENGTH = 106;

export type DecodedWithdrawalReceipt = Readonly<{
  vault: string;
  user: string;
  amountLpEscrowed: bigint;
  amountAssetToWithdrawDecimalBits: bigint;
  withdrawableFromTs: bigint;
  bump: number;
  version: number;
}>;

/**
 * Strict deployed-account decoder. The SDK decoder only covers the 106-byte
 * generated prefix; the deployed receipt is 112 bytes and its trailing bytes
 * must remain zero. Callers must still bind the decoded vault/user/PDA to the
 * operation they are authorizing.
 */
export function decodeReceipt(
  snapshot: AccountSnapshot | null,
): DecodedWithdrawalReceipt | null {
  if (
    !snapshot
    || snapshot.owner !== PARTNER_ROUTE.programs.voltrVault
    || snapshot.data.length !== RECEIPT_DATA_LENGTH
  ) return null;
  if (!Buffer.from(snapshot.data.subarray(0, 8)).equals(Buffer.from(getRequestWithdrawVaultReceiptDiscriminatorBytes()))) return null;
  if (snapshot.data.subarray(RECEIPT_SDK_PREFIX_LENGTH).some((value) => value !== 0)) return null;
  return getRequestWithdrawVaultReceiptDecoder().decode(snapshot.data.subarray(0, RECEIPT_SDK_PREFIX_LENGTH));
}
