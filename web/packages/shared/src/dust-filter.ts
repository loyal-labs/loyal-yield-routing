export const SOL_DUST_THRESHOLD_LAMPORTS = 100_000;

export const TOKEN_DUST_NORMALIZED_THRESHOLD = 0.0001;

export type SolDustInput = {
  isUserSigned: boolean;
  hasTokenChange: boolean;
  lamports: number;
};

export function isDustSolTransfer(input: SolDustInput): boolean {
  if (input.isUserSigned) return false;
  if (input.hasTokenChange) return false;
  return Math.abs(input.lamports) < SOL_DUST_THRESHOLD_LAMPORTS;
}

export type TokenDustInput = {
  isUserSigned: boolean;
  direction: "in" | "out";
  /** Absolute raw token amount (post-decimal). Must be non-negative. */
  rawAmount: bigint;
  decimals: number;
};

export function isDustTokenTransfer(input: TokenDustInput): boolean {
  if (input.isUserSigned) return false;
  if (input.direction !== "in") return false;
  if (input.rawAmount <= BigInt(0)) return false;

  const decimals = Math.max(0, input.decimals);
  const divisor = 10 ** decimals;
  const normalized = Number(input.rawAmount) / divisor;
  return normalized < TOKEN_DUST_NORMALIZED_THRESHOLD;
}
