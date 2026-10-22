import type { Signer } from "@/lib/wallet/signer";

import { EarnApiError, type EarnAuthFields } from "./earn-api";
import { maybeMintEarnSession, signEarnAuthFields } from "./earn-session";

export type EarnAuthPurpose =
  | "earn-deposit-prepare"
  | "earn-deposit-confirm"
  | "earn-withdraw-prepare"
  | "earn-withdraw-confirm"
  | "earn-autodeposit-setup-prepare"
  | "earn-autodeposit-setup-confirm"
  | "earn-autodeposit-floor-confirm"
  | "earn-autodeposit-toggle-confirm"
  | "earn-autodeposit-close-prepare"
  | "earn-autodeposit-close-confirm"
  | "earn-autodeposit-sweep-execute"
  | "earn-refund-prepare"
  | "earn-max";

export async function signEarnAuth(
  signer: Signer,
  purpose: EarnAuthPurpose,
): Promise<EarnAuthFields> {
  const fields = await signEarnAuthFields(signer, purpose);
  maybeMintEarnSession(fields);
  return fields;
}

// Auth-failure codes worth one retry with a freshly signed message: the reused
// flow auth aged past the backend's 5-minute freshness window, or the backend
// predates flow-auth reuse and only accepts the exact purpose.
const RETRYABLE_AUTH_CODES = new Set([
  "stale_mobile_auth",
  "invalid_mobile_signature",
]);

// Runs an Earn API call with the flow's already-signed auth message — the
// backend's confirm endpoints accept the flow's prepare signature, so one
// wallet prompt covers prepare + confirms. If the backend rejects the reuse,
// signs a fresh `purpose`-scoped auth once (one extra prompt) and retries.
export async function withEarnAuth<T>(
  signer: Signer,
  flowAuth: EarnAuthFields,
  purpose: EarnAuthPurpose,
  call: (auth: EarnAuthFields) => Promise<T>,
): Promise<T> {
  try {
    return await call(flowAuth);
  } catch (error) {
    if (
      error instanceof EarnApiError &&
      error.code !== undefined &&
      RETRYABLE_AUTH_CODES.has(error.code)
    ) {
      return call(await signEarnAuth(signer, purpose));
    }
    throw error;
  }
}
