// ClickStack error reporting (ASK-1804) — posts unhandled JS errors, fatal JS
// crashes, and unhandled promise rejections to the askloyal.com observability
// ingest.
//
// Best-effort by design: every path here swallows its own failures. Telemetry
// must never crash the app or block a user flow. Native crashes (JS VM dies
// before this code runs) stay with Datadog — this covers the JS layer only.

import {
  type LifecycleErrorDetail,
  type LifecycleErrorCode,
  MAX_ERROR_MESSAGE_LENGTH,
  MAX_ERROR_NAME_LENGTH,
  MAX_ERROR_STACK_LENGTH,
  mobileChannelToEnvironment,
  type MobileErrorEnvelope,
  type MobileErrorOperation,
  isTelemetryHttpStatus,
} from "@loyal-labs/shared";
import * as Updates from "expo-updates";

import { env } from "@/config/env";
import {
  isConnectionFailure,
  isConnectionTimeout,
  isRpcError,
} from "@/lib/network/connection-failure";
import { isInsufficientSolError } from "@/lib/wallet/insufficient-sol-error";
import { hasLandedProgress, isWalletRejection } from "@/lib/wallet/rejection";
import {
  isWalletSessionError,
  type WalletSessionFailure,
} from "@/lib/wallet/wallet-session-error";
import {
  getObservabilityDeviceId,
  getObservabilityDevicePlatform,
} from "@/services/device-identity";

const REPORT_TIMEOUT_MS = 1250;
const DEDUP_WINDOW_MS = 5000;
const DEDUP_MAX_ENTRIES = 128;

type GlobalErrorHandler = (error: unknown, isFatal?: boolean) => void;

type ErrorUtilsLike = {
  getGlobalHandler?: () => GlobalErrorHandler | null;
  setGlobalHandler?: (handler: GlobalErrorHandler) => void;
};

type HermesInternalLike = {
  enablePromiseRejectionTracker?: (options: {
    allRejections: boolean;
    onUnhandled: (id: number, rejection: unknown) => void;
  }) => void;
};

let initialized = false;
let currentPathname = "/";
const recentReports = new Map<string, number>();

export function setObservabilityPathname(pathname: string): void {
  if (pathname.startsWith("/")) {
    currentPathname = pathname;
  }
}

let deviceIdentity: {
  deviceId: string;
  devicePlatform: "android" | "ios";
} | null = null;

function currentDeviceIdentity(): {
  deviceId: string;
  devicePlatform: "android" | "ios";
} {
  deviceIdentity ??= {
    deviceId: getObservabilityDeviceId(),
    devicePlatform: getObservabilityDevicePlatform(),
  };
  return deviceIdentity;
}

export function getObservabilityEnvironment(): string {
  return mobileChannelToEnvironment(Updates.channel ?? "");
}

export function getObservabilityRelease(): string {
  const updatePrefix = Updates.updateId?.split("-")[0];
  return (
    [Updates.runtimeVersion, updatePrefix].filter(Boolean).join("_") || "dev"
  );
}

function truncate(value: string, maxLength: number): string {
  return value.length <= maxLength ? value : value.slice(0, maxLength);
}

function normalizeError(error: unknown): {
  message: string;
  name: string;
  stack?: string;
} {
  if (error instanceof Error) {
    return {
      message: truncate(
        error.message || "Unknown error.",
        MAX_ERROR_MESSAGE_LENGTH
      ),
      name: truncate(error.name || "Error", MAX_ERROR_NAME_LENGTH),
      ...(error.stack
        ? { stack: truncate(error.stack, MAX_ERROR_STACK_LENGTH) }
        : {}),
    };
  }

  if (typeof error === "string") {
    return {
      message: truncate(error, MAX_ERROR_MESSAGE_LENGTH),
      name: "NonErrorException",
    };
  }

  return {
    message: "Unhandled non-Error exception.",
    name: "NonErrorException",
  };
}

function isDuplicate(envelope: MobileErrorEnvelope): boolean {
  const now = Date.now();
  const fingerprint = [
    envelope.pathname,
    envelope.name,
    envelope.message,
    envelope.stack ?? "",
  ].join("\u0000");

  for (const [key, reportedAt] of recentReports) {
    if (now - reportedAt > DEDUP_WINDOW_MS) {
      recentReports.delete(key);
    }
  }

  const previous = recentReports.get(fingerprint);
  if (previous !== undefined && now - previous <= DEDUP_WINDOW_MS) {
    return true;
  }

  if (recentReports.size >= DEDUP_MAX_ENTRIES) {
    const oldestKey = recentReports.keys().next().value;
    if (typeof oldestKey === "string") {
      recentReports.delete(oldestKey);
    }
  }
  recentReports.set(fingerprint, now);
  return false;
}

export async function postObservabilityJson(
  path: string,
  payload: unknown
): Promise<void> {
  const controller = new AbortController();
  const timeout = setTimeout(() => controller.abort(), REPORT_TIMEOUT_MS);

  try {
    const headers: Record<string, string> = {
      "content-type": "application/json",
    };
    if (env.vercelProtectionBypass) {
      headers["x-vercel-protection-bypass"] = env.vercelProtectionBypass;
    }
    await fetch(`${env.apiBaseUrl}${path}`, {
      body: JSON.stringify(payload),
      headers,
      method: "POST",
      signal: controller.signal,
    });
  } catch {
  } finally {
    clearTimeout(timeout);
  }
}

function postEnvelope(envelope: MobileErrorEnvelope): Promise<void> {
  return postObservabilityJson("/api/observability/errors", envelope);
}

function report(
  error: unknown,
  operation: MobileErrorOperation,
  messagePrefix?: string
): void {
  try {
    const normalized = normalizeError(error);
    const envelope: MobileErrorEnvelope = {
      ...normalized,
      ...currentDeviceIdentity(),
      environment: getObservabilityEnvironment(),
      ...(messagePrefix
        ? {
            message: truncate(
              `${messagePrefix} ${normalized.message}`,
              MAX_ERROR_MESSAGE_LENGTH
            ),
          }
        : {}),
      operation,
      pathname: currentPathname,
      release: getObservabilityRelease(),
      timestamp: new Date().toISOString(),
    };
    if (isDuplicate(envelope)) {
      return;
    }
    void postEnvelope(envelope);
  } catch {
  }
}

export function initObservability(): void {
  if (initialized) {
    return;
  }
  initialized = true;

  try {
    const errorUtils = (globalThis as { ErrorUtils?: ErrorUtilsLike })
      .ErrorUtils;
    const previousHandler = errorUtils?.getGlobalHandler?.() ?? null;
    errorUtils?.setGlobalHandler?.((error, isFatal) => {
      report(error, isFatal ? "mobile.fatal_error" : "mobile.global_error");
      previousHandler?.(error, isFatal);
    });

    const hermes = (globalThis as { HermesInternal?: HermesInternalLike })
      .HermesInternal;
    hermes?.enablePromiseRejectionTracker?.({
      allRejections: true,
      onUnhandled: (_id, rejection) => {
        report(rejection, "mobile.unhandled_rejection");
        if (__DEV__) {
          // Enabling the tracker replaces RN's default dev warning — keep it.
          console.warn("Possible unhandled promise rejection:", rejection);
        }
      },
    });
  } catch (error) {
    console.warn("[observability] init failed", error);
  }
}

// ---------------------------------------------------------------------------
// Lifecycle flow tracing — twin of `createLifecycleTracker` in
// `frontend/src/features/observability/lifecycle-contract.ts`, posting to the
// mobile events ingest. Flow/variant/stage names and every diagnostics field
// are validated server-side against that contract; an envelope that drifts
// from it is silently dropped, so keep the vocabularies below in sync.

type LifecycleVariantMap = {
  "earn_max.vault": "deposit" | "request_withdrawal" | "claim";
  "auth.sign_in":
    | "seed_vault"
    | "wallet_adapter"
    | "import_wallet"
    | "new_wallet"
    | "privy_email"
    | "privy_oauth";
  "earn.deposit": "initial" | "top_up";
  "earn.withdrawal": "partial" | "full";
  "earn.autodeposit.configuration":
    | "setup"
    | "floor_update"
    | "pause"
    | "resume"
    | "close";
  "earn.autodeposit.execute_now": "execute_now";
};

type LifecycleStageMap = {
  "earn_max.vault": "intent" | "prepare" | "wallet_submit_confirm";
  "auth.sign_in":
    | "intent"
    | "wallet_connect"
    | "challenge"
    | "completion"
    | "ui_commit";
  "earn.deposit":
    | "intent"
    | "prepare"
    | "policy"
    | "policy_finalize"
    | "wallet_submit_confirm"
    | "backend_confirm"
    | "ui_commit";
  "earn.withdrawal":
    | "intent"
    | "prepare"
    | "autodeposit_close"
    | "wallet_submit_confirm"
    | "backend_confirm"
    | "full_exit_verify"
    | "cleanup_prepare"
    | "cleanup_wallet_submit_confirm"
    | "cleanup_backend_confirm"
    | "cleanup"
    | "ui_commit";
  "earn.autodeposit.configuration":
    | "intent"
    | "prepare"
    | "wallet_approval"
    | "create_policy"
    | "create_recurring_delegation"
    | "backend_confirm"
    | "bootstrap"
    | "ui_commit";
  "earn.autodeposit.execute_now":
    | "intent"
    | "request"
    | "state_observed"
    | "ui_commit";
};

type LifecycleFlowName = keyof LifecycleVariantMap;

const LIFECYCLE_ERROR_DETAILS: readonly LifecycleErrorDetail[] = [
  "network_unreachable",
  "request_timeout",
  "kamino_upstream_unavailable",
  "rpc_request_failed",
];

export type { LifecycleErrorDetail };

export type LifecycleDiagnostics = {
  autodepositCloseRequired?: boolean;
  chainState?: "not_submitted" | "submitted" | "confirmed" | "failed";
  cleanupRequired?: boolean;
  errorCode?: LifecycleErrorCode;
  errorDetail?: LifecycleErrorDetail;
  executeNowState?:
    | "requested"
    | "selected"
    | "pull_confirmed"
    | "completed"
    | "failed"
    | "released"
    | "canceled";
  executionMode?: "batch" | "sequential" | "single";
  httpStatus?: number;
  persistenceState?: "not_started" | "recorded" | "failed";
  policyMode?: "create" | "reuse";
  recoveryRequired?: boolean;
  /**
   * Substage coordinates inside a coarse stage — which ordered sub-call a
   * `prepare` failure died in (ASK-2095). The ingest contract bounds them
   * (index 0–15, count 1–16); an out-of-range value drops the whole event.
   */
  stageCount?: number;
  stageIndex?: number;
};

export type LifecycleFlow<F extends LifecycleFlowName> = {
  cancel: (
    stage: LifecycleStageMap[F],
    diagnostics?: LifecycleDiagnostics
  ) => void;
  complete: (
    stage: LifecycleStageMap[F],
    diagnostics?: LifecycleDiagnostics
  ) => void;
  fail: (
    stage: LifecycleStageMap[F],
    diagnostics?: LifecycleDiagnostics
  ) => void;
  failFrom: (
    stage: LifecycleStageMap[F],
    error: unknown,
    diagnostics?: LifecycleDiagnostics
  ) => void;
  flowId: string;
  observe: (
    stage: LifecycleStageMap[F],
    diagnostics?: LifecycleDiagnostics
  ) => void;
  setVariant: (variant: LifecycleVariantMap[F]) => void;
  setWalletAddress: (walletAddress: string) => void;
  start: (
    stage: LifecycleStageMap[F],
    diagnostics?: LifecycleDiagnostics
  ) => void;
};

const WALLET_SESSION_ERROR_CODES: Record<
  WalletSessionFailure,
  LifecycleErrorCode
> = {
  account_mismatch: "wallet_account_mismatch",
  authorization_expired: "wallet_authorization_expired",
  connection_failed: "wallet_connection_failed",
  signing_failed: "wallet_signing_failed",
  timeout: "wallet_connection_timeout",
  unavailable: "wallet_unavailable",
};

export function mapLifecycleErrorCode(error: unknown): LifecycleErrorCode {
  if (isWalletRejection(error)) return "wallet_rejected";
  if (isWalletSessionError(error)) {
    return WALLET_SESSION_ERROR_CODES[error.failure];
  }
  if (isInsufficientSolError(error)) return "insufficient_native_sol";
  if (error && typeof error === "object" && "code" in error) {
    const code = (error as { code?: unknown }).code;
    if (code === "unconfirmed_signature") return "unconfirmed_signature";
    if (code === "earn_withdraw_underfilled") {
      return "earn_withdraw_underfilled";
    }
    return "request_failed";
  }
  if (isConnectionFailure(error)) return "request_failed";
  return "unexpected_error";
}

function declaredErrorDetail(value: unknown): LifecycleErrorDetail | undefined {
  return LIFECYCLE_ERROR_DETAILS.includes(value as LifecycleErrorDetail)
    ? (value as LifecycleErrorDetail)
    : undefined;
}

export function mapLifecycleErrorDetail(
  error: unknown
): LifecycleErrorDetail | undefined {
  const carried = declaredErrorDetail(
    error && typeof error === "object"
      ? (error as { detail?: unknown }).detail
      : undefined
  );
  if (carried) return carried;
  if (error instanceof Error && error.name === "FetchTimeoutError") {
    return "request_timeout";
  }
  if (isConnectionTimeout(error)) return "request_timeout";
  if (isConnectionFailure(error)) return "network_unreachable";
  if (isRpcError(error)) return "rpc_request_failed";
  return undefined;
}

function httpStatusOf(error: unknown): number | undefined {
  if (!error || typeof error !== "object") return undefined;
  const { status } = error as { status?: unknown };
  return isTelemetryHttpStatus(status) ? status : undefined;
}

function generateFlowId(): string {
  const cryptoApi = (globalThis as { crypto?: { randomUUID?: () => string } })
    .crypto;
  if (cryptoApi?.randomUUID) return cryptoApi.randomUUID();
  return "xxxxxxxx-xxxx-4xxx-yxxx-xxxxxxxxxxxx".replace(/[xy]/g, (char) => {
    const random = Math.trunc(Math.random() * 16);
    const value = char === "x" ? random : (random % 4) + 8;
    return value.toString(16);
  });
}

/**
 * Start a traced flow. Emissions are fire-and-forget; a terminal outcome
 * (complete/fail) latches the flow and later emissions become no-ops, so a
 * blanket `fail` in an outer catch never overwrites a more precise inner one.
 */
// The ingest drops any envelope whose walletAddress isn't base58 — guard here
// so one bad address never costs the whole event.
const WALLET_ADDRESS_PATTERN = /^[1-9A-HJ-NP-Za-km-z]{32,44}$/;
// The lifecycle ingest drops any envelope whose flowId isn't a canonical v4,
// so an adopted id is checked before it can cost the whole flow's events.
const FLOW_ID_PATTERN =
  /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i;

export function startLifecycleFlow<F extends LifecycleFlowName>(args: {
  flowId?: string;
  flowName: F;
  flowVariant: LifecycleVariantMap[F];
  reportUnexpectedErrors?: boolean;
  walletAddress?: string;
}): LifecycleFlow<F> {
  const flowId =
    args.flowId && FLOW_ID_PATTERN.test(args.flowId)
      ? args.flowId
      : generateFlowId();
  const startedAt = Date.now();
  let lastAt = startedAt;
  let variant: LifecycleVariantMap[F] = args.flowVariant;
  let walletAddress =
    args.walletAddress && WALLET_ADDRESS_PATTERN.test(args.walletAddress)
      ? args.walletAddress
      : undefined;
  let terminal = false;

  const emit = (
    outcome: "started" | "observed" | "completed" | "failed" | "cancelled",
    stage: LifecycleStageMap[F],
    diagnostics: LifecycleDiagnostics = {}
  ): void => {
    try {
      if (terminal) return;
      const current = Date.now();
      const envelope = {
        ...diagnostics,
        ...currentDeviceIdentity(),
        durationMs: Math.min(900_000, Math.max(0, current - lastAt)),
        elapsedMs: Math.min(86_400_000, Math.max(0, current - startedAt)),
        environment: getObservabilityEnvironment(),
        flowId,
        flowName: args.flowName,
        flowVariant: variant,
        outcome,
        pathname: currentPathname,
        release: getObservabilityRelease(),
        runtime: "mobile",
        source: "mobile_app",
        stage,
        timestamp: new Date(current).toISOString(),
        ...(walletAddress ? { walletAddress } : {}),
      };
      lastAt = current;
      if (
        outcome === "completed" ||
        outcome === "failed" ||
        outcome === "cancelled"
      ) {
        terminal = true;
      }
      void postObservabilityJson("/api/observability/events", envelope);
    } catch {
    }
  };

  return {
    cancel: (stage, diagnostics) => emit("cancelled", stage, diagnostics),
    complete: (stage, diagnostics) => emit("completed", stage, diagnostics),
    fail: (stage, diagnostics) => emit("failed", stage, diagnostics),
    failFrom: (stage, error, diagnostics) => {
      if (terminal) return;
      const errorCode = mapLifecycleErrorCode(error);
      const cancelled =
        (errorCode === "wallet_rejected" ||
          errorCode === "insufficient_native_sol") &&
        !hasLandedProgress(error);
      const httpStatus = httpStatusOf(error);
      const errorDetail =
        diagnostics?.errorDetail ?? mapLifecycleErrorDetail(error);
      emit(cancelled ? "cancelled" : "failed", stage, {
        ...diagnostics,
        errorCode,
        ...(errorDetail !== undefined ? { errorDetail } : {}),
        ...(httpStatus !== undefined ? { httpStatus } : {}),
      });
      if (errorCode === "unexpected_error" && args.reportUnexpectedErrors) {
        report(
          error,
          "mobile.global_error",
          `[flow_id=${flowId} flow_name=${args.flowName} flow_stage=${stage}]`
        );
      }
    },
    flowId,
    observe: (stage, diagnostics) => emit("observed", stage, diagnostics),
    setVariant: (next) => {
      variant = next;
    },
    setWalletAddress: (next) => {
      if (WALLET_ADDRESS_PATTERN.test(next)) {
        walletAddress = next;
      }
    },
    start: (stage, diagnostics) => emit("started", stage, diagnostics),
  };
}
