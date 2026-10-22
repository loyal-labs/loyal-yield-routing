// The two — and only two — TypeErrors RN's fetch rejects with for a
// connection-level failure, from the whatwg-fetch polyfill's `xhr.onerror` and
// `xhr.ontimeout` handlers. `xhr.onabort` rejects with a DOMException instead,
// which `fetchWithTimeout` already turns into a FetchTimeoutError.
//
// Matching both matters: keying on "failed" alone would classify every stalled
// request as a bug in our own code and route it to the sanitized error ingest,
// which is exactly the flood that ingest is kept clear of.
const CONNECTION_FAILED = /network request failed/i;
const CONNECTION_TIMED_OUT = /network request timed out/i;

export function isConnectionFailure(error: unknown): boolean {
  if (!(error instanceof TypeError)) return false;
  return (
    CONNECTION_FAILED.test(error.message) ||
    CONNECTION_TIMED_OUT.test(error.message)
  );
}

export function isConnectionTimeout(error: unknown): boolean {
  return error instanceof TypeError && CONNECTION_TIMED_OUT.test(error.message);
}

export function isRpcError(error: unknown): boolean {
  return (
    error instanceof Error &&
    error.name === "SolanaJSONRPCError" &&
    typeof (error as { code?: unknown }).code === "number"
  );
}
