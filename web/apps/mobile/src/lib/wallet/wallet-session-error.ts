// A wallet session that never carried our request is not a request failure
// (ASK-1872). The MWA native module rejects every session-level problem with a
// string `code`, and lifecycle telemetry maps *any* coded error to
// `request_failed` — so "the wallet app never connected back" reached ClickStack
// looking exactly like "the backend returned 500", and paged on-call for a
// failure that never left the device.
//
// The native module's rejection codes (SolanaMobileWalletAdapterModule.kt) are:
//
//   ERROR_WALLET_NOT_FOUND                                  no wallet installed
//   "Timed out waiting for local association to be ready"    10s association wait
//   "Session not established: Local association cancelled…"  user backed out
//   "Timed out waiting for response"                         90s in-session wait
//   EUNSPECIFIED                                             everything else,
//     including the ExecutionException raised when the wallet app never
//     connects back to the local association socket
//
// Every one of those throws `WalletSessionError` so callers can ask
// `isWalletSessionError` instead of matching codes, and telemetry can report
// the actual failure instead of a blanket `request_failed`.

import type { SolanaMobileWalletAdapterProtocolErrorCode } from "@solana-mobile/mobile-wallet-adapter-protocol";

import { DeeplinkResponseError } from "./deeplink-protocol";

export type WalletSessionFailure =
  | "authorization_expired"
  | "account_mismatch"
  | "unavailable"
  | "connection_failed"
  | "timeout"
  | "signing_failed";

const WALLET_SESSION_MESSAGES: Record<WalletSessionFailure, string> = {
  account_mismatch:
    "Wallet authorization is no longer valid. Reset your wallet in Settings and reconnect your wallet.",
  authorization_expired:
    "Wallet authorization is no longer valid. Reset your wallet in Settings and reconnect your wallet.",
  connection_failed:
    "Couldn't reach your wallet app. Open it once so it's running, then try again.",
  signing_failed:
    "Your wallet app couldn't sign the request. Try again, and update the wallet app if this keeps happening.",
  timeout:
    "Your wallet app didn't respond in time. Try again with it already open.",
  unavailable:
    "No compatible Solana wallet app was found. Install Phantom or Solflare and try again.",
};

export class WalletSessionError extends Error {
  readonly failure: WalletSessionFailure;
  /**
   * The wallet backend's own code, kept for local logs only. Telemetry cannot
   * carry it yet: the ingest rejects envelopes with unknown keys, so until the
   * backend contract gains a field for it the failure discriminant above is
   * what reaches ClickStack.
   */
  readonly walletCode?: string | number;

  constructor(
    failure: WalletSessionFailure,
    walletCode?: string | number,
    cause?: unknown,
  ) {
    super(
      WALLET_SESSION_MESSAGES[failure],
      cause === undefined ? undefined : { cause },
    );
    this.name = "WalletSessionError";
    this.failure = failure;
    this.walletCode = walletCode;
  }
}

export function isWalletSessionError(
  error: unknown,
): error is WalletSessionError {
  return error instanceof WalletSessionError;
}

// React Native can deliver a native rejection as a plain object, so do not
// require the value to inherit from Error (or from one particular JS realm).
export function errorCodeOf(error: unknown): string | number | undefined {
  if (!error || typeof error !== "object") return undefined;
  const code = (error as { code?: unknown }).code;
  return typeof code === "string" || typeof code === "number"
    ? code
    : undefined;
}

export function hasErrorCode(error: unknown, code: string | number): boolean {
  return errorCodeOf(error) === code;
}

export const MWA_ERROR = {
  ERROR_AUTHORIZATION_FAILED: -1,
  ERROR_NOT_SIGNED: -3,
} as const satisfies Pick<
  typeof SolanaMobileWalletAdapterProtocolErrorCode,
  "ERROR_AUTHORIZATION_FAILED" | "ERROR_NOT_SIGNED"
>;

/**
 * True when the wallet rejected the request because it no longer honors this
 * app's authorization — the user disconnected the app inside the wallet. Both
 * protocols express it: the deeplink protocol's 4100 unauthorized error
 * (`DeeplinkResponseError.isUnauthorized`) and MWA's
 * ERROR_AUTHORIZATION_FAILED. The stored authorization behind such a session
 * is dead and must be dropped (`forgetWalletAuthorization`).
 */
export function isDeadAuthorization(error: unknown): boolean {
  return (
    (error instanceof DeeplinkResponseError && error.isUnauthorized) ||
    hasErrorCode(error, MWA_ERROR.ERROR_AUTHORIZATION_FAILED)
  );
}

/**
 * Drop the stored authorization behind a dead wallet session and produce the
 * error to throw. Clearing is the half that matters: without it the app keeps
 * presenting a connected wallet whose every signature is refused.
 */
export async function forgetWalletAuthorization(
  clearStoredAuthorization: () => Promise<void>,
  failure: "authorization_expired" | "account_mismatch",
  walletCode: string | number | undefined,
  cause?: unknown,
): Promise<WalletSessionError> {
  await clearStoredAuthorization();
  return new WalletSessionError(failure, walletCode, cause);
}
