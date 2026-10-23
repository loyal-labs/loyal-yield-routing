export const MOBILE_DEVICE_PLATFORMS = ["android", "ios"] as const;

export type MobileDevicePlatform = typeof MOBILE_DEVICE_PLATFORMS[number];

const UUID_V4_PATTERN =
  /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;

export function isCanonicalUuidV4(value: unknown): value is string {
  return typeof value === "string" && UUID_V4_PATTERN.test(value);
}

export type TelemetryDeviceIdentity = {
  deviceId?: string;
  devicePlatform?: MobileDevicePlatform;
};

export type MobileTelemetryReleaseContext = {
  environment: string;
  release: string;
};

export const MAX_ERROR_NAME_LENGTH = 80;
export const MAX_ERROR_MESSAGE_LENGTH = 512;
export const MAX_ERROR_STACK_LENGTH = 4096;

export const MOBILE_ERROR_OPERATIONS = [
  "mobile.global_error",
  "mobile.fatal_error",
  "mobile.unhandled_rejection",
] as const;

export type MobileErrorOperation = typeof MOBILE_ERROR_OPERATIONS[number];

export type MobileErrorEnvelope = TelemetryDeviceIdentity &
  MobileTelemetryReleaseContext & {
    message: string;
    name: string;
    operation: MobileErrorOperation;
    pathname: string;
    stack?: string;
    timestamp: string;
  };

export function mobileChannelToEnvironment(channel: string): string {
  if (channel === "production") return "prod";
  if (channel === "preview") return "preview";
  if (channel === "dapp-store") return "prod";
  return "dev";
}

export function isTelemetryHttpStatus(value: unknown): value is number {
  return (
    typeof value === "number" &&
    Number.isInteger(value) &&
    value >= 100 &&
    value <= 599
  );
}

export const LIFECYCLE_ERROR_CODES = [
  "unexpected_error",
  "invalid_request",
  "unauthenticated",
  "captcha_verification_failed",
  "wallet_selection_timeout",
  "wallet_connection_timeout",
  "wallet_connection_failed",
  "wallet_authorization_expired",
  "wallet_account_mismatch",
  // The wallet could not produce a signature. Distinct from
  // `invalid_wallet_signature`, which means a signature was produced and the
  // backend rejected it during verification.
  "wallet_signing_failed",
  "wallet_signing_unsupported",
  "wallet_rejected",
  "invalid_wallet_origin",
  "invalid_wallet_proof_kind",
  "invalid_wallet_signature",
  "invalid_wallet_signin",
  "wallet_auth_completion_in_progress",
  "smart_account_sponsor_not_configured",
  "smart_account_provisioning_failed",
  "smart_account_reservation_conflict",
  "smart_account_signer_mismatch",
  "smart_account_principal_mismatch",
  "state_not_ready",
  "policy_signer_missing",
  "invalid_source",
  "insufficient_usdc",
  "insufficient_native_sol",
  "earn_max_liquidity_unavailable",
  "instruction_fetch_failed",
  "instruction_validation_failed",
  "transaction_too_large",
  "cluster_mismatch",
  "wallet_unavailable",
  "wallet_mismatch",
  "simulation_failed",
  // Kamino KTX could not provide enough collateral to preserve the exact
  // partial Earn withdrawal output after bounded rounding adjustments.
  "earn_withdraw_underfilled",
  // The route's output fell below the quote's minimum-out (Jupiter 6001).
  "slippage_exceeded",
  "send_failed",
  "chain_confirmation_failed",
  "slot_resolution_failed",
  "backend_confirmation_failed",
  "record_failed",
  "full_exit_verification_retryable",
  "autodeposit_not_found",
  "autodeposit_not_active",
  "earn_position_required",
  "no_scheduled_sweeps",
  "request_failed",
  "progress_read_failed",
  "execution_failed",
  "kamino_top_up_failed",
  "autodeposit_target_closed",
  "unconfirmed_signature",
  "metadata_mismatch",
  "principal_mismatch",
  "slot_mismatch",
  "artifact_missing",
  "token_approval_missing",
  "realtime_unresolved",
] as const;
export type LifecycleErrorCode = typeof LIFECYCLE_ERROR_CODES[number];

// Underlying causes behind a broad `errorCode`. Every value is a token this
// repo defines — never a string forwarded from a vendor SDK — so nothing a
// client holds can reach `loyal.error.detail`. Emitters map their native error
// onto one of these; add a token here when a new cause needs telling apart.
//
// `mwa_*` are Mobile Wallet Adapter session failures, whose native codes are
// EUNSPECIFIED (the module's catch-all), ERROR_WALLET_NOT_FOUND,
// ERROR_SESSION_TIMEOUT, ERROR_SESSION_CLOSED, "Timed out waiting for local
// association to be ready", and "Failed to end session".
//
// The rest name what went wrong behind a `request_failed` that carries no
// `httpStatus` — meaning no response ever arrived, so the status that usually
// explains a failure is absent. Four unrelated incidents look identical
// without them: the device is offline, our own client timeout elapsed, Kamino
// is down, or an RPC answered with an error (ASK-2018).
export const LIFECYCLE_ERROR_DETAILS = [
  "mwa_unspecified",
  "mwa_wallet_not_found",
  "mwa_session_timeout",
  "mwa_association_timeout",
  "mwa_session_closed",
  "mwa_end_session_failed",
  // Connection-level fetch failure: DNS, TLS or a reset socket. React Native
  // surfaces all of them as a bare TypeError("Network request failed").
  "network_unreachable",
  "request_timeout",
  "kamino_upstream_unavailable",
  "rpc_request_failed",
] as const;
export type LifecycleErrorDetail = typeof LIFECYCLE_ERROR_DETAILS[number];
