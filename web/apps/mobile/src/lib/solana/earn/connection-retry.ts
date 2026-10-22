import { KaminoUpstreamError } from "@loyal-labs/smart-account-vaults";

import {
  isConnectionFailure,
  isConnectionTimeout,
  isRpcError,
} from "@/lib/network/connection-failure";
import type { LifecycleErrorDetail } from "@/services/observability";

import { earnNetworkError } from "./earn-api";

// RN surfaces connection-level fetch failures (DNS/TLS/socket reset) as a bare
// TypeError("Network request failed") — which used to reach the Earn sheets
// verbatim (ASK-1801). Prepare stages are read-only on both the backend and
// the RPC, so retrying them is always safe; send/confirm stages keep their
// own semantics and are deliberately NOT wrapped.
const NETWORK_RETRY_ATTEMPTS = 3;
const NETWORK_RETRY_DELAY_MS = 1_000;

function isRetryableKaminoStatus(status: number): boolean {
  return status >= 500 || status === 429;
}

// A Solana JSON-RPC error response during a read-only prepare (node lag, a
// slot the node hasn't reached) heals on retry the same way a transport blip
// does.
function isRetryableNetworkError(error: unknown): error is Error {
  if (isConnectionFailure(error)) {
    return true;
  }
  if (isRpcError(error)) {
    return true;
  }
  return (
    error instanceof KaminoUpstreamError &&
    isRetryableKaminoStatus(error.status)
  );
}

function exhaustedErrorDetail(
  lastError: unknown,
): LifecycleErrorDetail | undefined {
  if (lastError instanceof KaminoUpstreamError) {
    return "kamino_upstream_unavailable";
  }
  if (isRpcError(lastError)) return "rpc_request_failed";
  if (isConnectionTimeout(lastError)) return "request_timeout";
  if (isConnectionFailure(lastError)) return "network_unreachable";
  return undefined;
}

export async function withConnectionRetry<T>(
  label: string,
  exhaustedMessage: string,
  run: () => Promise<T>,
): Promise<T> {
  let lastError: unknown;
  for (let attempt = 0; attempt < NETWORK_RETRY_ATTEMPTS; attempt++) {
    if (attempt > 0) {
      await new Promise((resolve) =>
        setTimeout(resolve, NETWORK_RETRY_DELAY_MS),
      );
    }
    try {
      return await run();
    } catch (error) {
      if (!isRetryableNetworkError(error)) {
        throw error;
      }
      lastError = error;
      console.warn(`[earn] ${label}: network failure`, {
        attempt: attempt + 1,
        errorMessage: error.message,
      });
    }
  }
  console.warn(`[earn] ${label}: giving up after network failures`, {
    errorMessage:
      lastError instanceof Error ? lastError.message : String(lastError),
  });
  throw earnNetworkError(exhaustedMessage, exhaustedErrorDetail(lastError));
}
