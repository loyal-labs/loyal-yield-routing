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

export type EarnHoldingWithdrawSource = {
  amountRaw: string;
  id: string;
  label: string;
  liquidityMint: string;
  market: string | null;
  reserve: string | null;
  sourceId: string;
  tokenAccount: string | null;
  type: "reserve" | "idle";
};

function isPositiveAmountRaw(amountRaw: string): boolean {
  try {
    return BigInt(amountRaw) > BigInt(0);
  } catch {
    return false;
  }
}

function holdingToWithdrawSource(
  holding: EarnRpcHolding
): EarnHoldingWithdrawSource | null {
  if (!isPositiveAmountRaw(holding.amountRaw)) {
    return null;
  }
  if (holding.kind === "idle") {
    const tokenAccount = holding.provenance.tokenAccount;
    if (!tokenAccount) {
      return null;
    }
    return {
      amountRaw: holding.amountRaw,
      id: holding.sourceId,
      label: `${holding.label} ${holding.marketName}`,
      liquidityMint: holding.liquidityMint,
      market: null,
      reserve: null,
      sourceId: holding.sourceId,
      tokenAccount,
      type: "idle",
    };
  }
  if (!holding.reserve || !holding.market) {
    return null;
  }
  return {
    amountRaw: holding.amountRaw,
    id: holding.sourceId,
    label: holding.label,
    liquidityMint: holding.liquidityMint,
    market: holding.market,
    reserve: holding.reserve,
    sourceId: holding.sourceId,
    tokenAccount: null,
    type: "reserve",
  };
}

export function earnWithdrawSourcesFromHoldings(
  holdings: readonly EarnRpcHolding[]
): EarnHoldingWithdrawSource[] {
  return holdings.flatMap((holding) => {
    const source = holdingToWithdrawSource(holding);
    return source ? [source] : [];
  });
}
