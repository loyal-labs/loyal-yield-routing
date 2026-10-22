// A user declining or backing out of a wallet prompt is a choice, not a
// failure — telemetry must not report it as an error (ASK-1859).
//
// Each wallet backend signals a decline differently: MWA raises coded protocol
// errors, a torn-down MWA session arrives as a bare native
// CancellationException, Seed Vault returns an Android activity result, and our
// own approval sheet just resolves false. Every one of those paths throws
// `WalletRejectedError` (or a subclass) so callers can ask `isWalletRejection`
// instead of matching error messages.

export class WalletRejectedError extends Error {
  readonly landedSignatures: readonly string[];

  constructor(
    message = "The signing request was declined.",
    landedSignatures: readonly string[] = [],
  ) {
    super(message);
    this.name = "WalletRejectedError";
    this.landedSignatures = landedSignatures;
  }
}

export function isWalletRejection(error: unknown): boolean {
  return error instanceof WalletRejectedError;
}

export function hasLandedProgress(error: unknown): boolean {
  return error instanceof WalletRejectedError && error.landedSignatures.length > 0;
}

export function withLandedSignatures(
  error: WalletRejectedError,
  landedSignatures: readonly string[],
): WalletRejectedError {
  const tagged = new WalletRejectedError(error.message, landedSignatures);
  tagged.name = error.name;
  tagged.stack = error.stack;
  return tagged;
}
