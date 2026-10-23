import { PublicKey } from "@solana/web3.js";

import type { EarnRpcHolding } from "./rpc-holdings";

function publicKeyFromMetadata(
  metadata: Record<string, unknown> | null | undefined,
  keys: string[]
): PublicKey | null {
  if (!metadata) {
    return null;
  }

  for (const key of keys) {
    const value = metadata[key];
    if (typeof value !== "string" || value.trim().length === 0) {
      continue;
    }
    try {
      return new PublicKey(value);
    } catch {
      continue;
    }
  }

  return null;
}

export function snapshotFullWithdrawalTargets(holdings: EarnRpcHolding[]): {
  amountRaw: bigint;
  liquidityMint: PublicKey;
  liquidityTokenProgram: PublicKey;
  market: PublicKey;
  reserve: PublicKey;
  reserveCollateralMint?: PublicKey;
  supplyApyBps: bigint | null;
}[] {
  const targets = [];
  for (const holding of holdings) {
    if (holding.kind !== "kamino" || !holding.reserve || !holding.market) {
      continue;
    }
    let amountRaw: bigint;
    try {
      amountRaw = BigInt(holding.amountRaw);
    } catch {
      continue;
    }
    if (amountRaw <= BigInt(0)) {
      continue;
    }
    let supplyApyBps: bigint | null = null;
    if (holding.supplyApyBps) {
      try {
        supplyApyBps = BigInt(holding.supplyApyBps);
      } catch {
        supplyApyBps = null;
      }
    }
    const reserveCollateralMint = publicKeyFromMetadata(holding.provenance, [
      "reserveCollateralMint",
    ]);
    targets.push({
      amountRaw,
      liquidityMint: new PublicKey(holding.liquidityMint),
      liquidityTokenProgram: new PublicKey(holding.tokenProgramId),
      market: new PublicKey(holding.market),
      reserve: new PublicKey(holding.reserve),
      ...(reserveCollateralMint ? { reserveCollateralMint } : {}),
      supplyApyBps,
    });
  }
  return targets;
}
