import { createHash } from "node:crypto";

import { address, type Address } from "@solana/kit";

import { PARTNER_ROUTE } from "./route-spec.js";

export type RwaMultiplyRouteSpec = Readonly<{
  schemaVersion: 1;
  id: "loyal-voltr-rwa-multiply-usdc-v1";
  cluster: "mainnet-beta";
  genesisHash: string;
  previousBackyardVault: Address;
  previousBackyardVaultDataSha256: "7d76d132efcef768017bd2e1c7402c5e459b2137765c150ef4511fd5f67fbc95";
  setupAdmin: Address;
  vault: Readonly<{
    address: Address;
    derivationDomain: string;
    name: "Loyal RWA Multiply USDC";
    description: "Voltr accounting over policy-constrained Squads RWA Multiply";
    withdrawalWaitingPeriodSeconds: 600n;
    capRaw: 1_000_000_000_000n;
    proofAmountRaw: 1_000_000n;
  }>;
  squads: Readonly<{
    program: Address;
    settings: Address;
    vaultIndex: 0;
    vault: Address;
    delegatedExecutor: Address;
    assetAta: Address;
    collateralAta: Address;
    customPolicySeeds: Readonly<{
      allocation: 53n;
      navRefresh: 54n;
      stageWithdrawal: 55n;
      withdraw: 56n;
    }>;
  }>;
  /** Inactive fallback retained for forensic comparison only. */
  trustfulBridge: Readonly<{
    adaptorProgram: Address;
    strategySeed: "loyal-rwa-multiply-squads-v1";
    strategy: Address;
    withdrawalHoldingAuth: Address;
    withdrawalHoldingAta: Address;
    maxReportedNavRaw: 2_000_000_000_000n;
    maxSnapshotAgeSlots: 32n;
  }>;
  customAdaptor: Readonly<{
    program: Address;
    strategyConfig: Address;
    strategyDerivationDomain: string;
    settingsSigner: Address;
    maxReportedNavRaw: 2_000_000_000_000n;
    maxReportAgeSlots: 32n;
  }>;
  voltrAdmission: Readonly<{
    protocolAdmin: Address;
    squadsV4Program: Address;
    multisig: Address;
    vaultIndex: 0;
  }>;
  kamino: Readonly<{
    program: Address;
    farmsProgram: Address;
    market: Address;
    obligation: Address;
    collateralReserve: Address;
    debtReserve: Address;
  }>;
  assets: Readonly<{
    assetMint: Address;
    collateralMint: Address;
    tokenProgram: Address;
    associatedTokenProgram: Address;
    decimals: 6;
    jupiterProofInputRaw: 1_000n;
    maxSlippageBps: 50;
  }>;
  programs: Readonly<{
    voltr: Address;
    trustfulAdaptor: Address;
    jupiter: Address;
    system: Address;
  }>;
}>;

export const RWA_MULTIPLY_ROUTE = {
  schemaVersion: 1,
  id: "loyal-voltr-rwa-multiply-usdc-v1",
  cluster: "mainnet-beta",
  genesisHash: "5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d",
  previousBackyardVault: PARTNER_ROUTE.vault,
  previousBackyardVaultDataSha256:
    "7d76d132efcef768017bd2e1c7402c5e459b2137765c150ef4511fd5f67fbc95",
  setupAdmin: address("BAqgbERmvUViqDSx961xpRBHGt68SpACiWL4t9696qZZ"),
  vault: {
    address: address("HXtk15EA5pBg3rSKxBm8sWPExScPkTknSRp37fXNHgNA"),
    derivationDomain:
      "loyal-voltr-rwa-multiply-mainnet-v1:5YQ78RwqukvCcykpmjmgRFmbEUeAgLpuVDxx1xNZnHD6:ST999VUTo5QExYEX9bz1oDDoKGkjXG9zpphy4Hj7VWh",
    name: "Loyal RWA Multiply USDC",
    description: "Voltr accounting over policy-constrained Squads RWA Multiply",
    withdrawalWaitingPeriodSeconds: 600n,
    capRaw: 1_000_000_000_000n,
    proofAmountRaw: 1_000_000n,
  },
  squads: {
    program: address("SMRTzfY6DfH5ik3TKiyLFfXexV8uSG3d2UksSCYdunG"),
    settings: address("5YQ78RwqukvCcykpmjmgRFmbEUeAgLpuVDxx1xNZnHD6"),
    vaultIndex: 0,
    vault: address("ST999VUTo5QExYEX9bz1oDDoKGkjXG9zpphy4Hj7VWh"),
    delegatedExecutor: address("62JLkPeE4oG65LRB3W3m52RVicmYq3xFHdv7TecCsPj5"),
    assetAta: address("EBG2iYrcXttDy9FpWDeNVL8uaCLRCkevrpRyrAhvVYKe"),
    collateralAta: address("CYwM28WSoYp85HrQGuaVpWy2JhKH6JJah4m65DSWUNiN"),
    customPolicySeeds: {
      allocation: 53n,
      navRefresh: 54n,
      stageWithdrawal: 55n,
      withdraw: 56n,
    },
  },
  // Voltr's deployed generic bridge. The strategy PDA is derived from the
  // fixed seed under this adaptor; deposits land directly in the Squads USDC
  // ATA and withdrawals are sourced from the adaptor's deterministic holding
  // ATA after Squads stages liquidity there.
  trustfulBridge: {
    adaptorProgram: address("3pnpK9nrs1R65eMV1wqCXkDkhSgN18xb1G5pgYPwoZjJ"),
    strategySeed: "loyal-rwa-multiply-squads-v1",
    strategy: address("4MetvifzuZShQ5zUhff4mVvwpu3kfKcqYfuY8pW7Zy9B"),
    withdrawalHoldingAuth: address("915qDo3X21eUc6RyX3Qx7KPW8qHJhUuJBSD9YNaVQwwa"),
    withdrawalHoldingAta: address("BsyRSvD5vfrE9VKhaZqvBt5nHAbPA4omAv3eePNXbQyN"),
    // The vault cap is 1,000,000 USDC. The NAV ceiling leaves room for yield
    // while bounding the manager-supplied Trustful accounting field.
    maxReportedNavRaw: 2_000_000_000_000n,
    // All Kamino valuation accounts must share one refresh slot no more than
    // this many slots behind the finalized snapshot used for the attestation.
    maxSnapshotAgeSlots: 32n,
  },
  customAdaptor: {
    program: address("FSj27QT2PtP7365pQRtgSAwSwk5h2m2ATCBoXQjwTSxW"),
    strategyConfig: address("9hDH4acTDrSjg9d5n8c1g53jMTonaDAUesp1diCWuuhj"),
    strategyDerivationDomain:
      "loyal-voltr-rwa-multiply-strategy-config-mainnet-v2:HXtk15EA5pBg3rSKxBm8sWPExScPkTknSRp37fXNHgNA:5YQ78RwqukvCcykpmjmgRFmbEUeAgLpuVDxx1xNZnHD6:ST999VUTo5QExYEX9bz1oDDoKGkjXG9zpphy4Hj7VWh",
    settingsSigner: address("BAqgbERmvUViqDSx961xpRBHGt68SpACiWL4t9696qZZ"),
    maxReportedNavRaw: 2_000_000_000_000n,
    maxReportAgeSlots: 32n,
  },
  // This is Ranger's independent Voltr protocol-administration authority. It is
  // deliberately separate from the Loyal Squads Smart Account above.
  voltrAdmission: {
    protocolAdmin: address("G2FCNGgQQ7MYyJvkXw1du86YGR6vXXejuQG9LsjX1kEs"),
    squadsV4Program: address("SQDS4ep65T869zMMBKyuUq6aD6EgTu8psMjkvj52pCf"),
    multisig: address("7szuzpoZzah95BsAu2LQm3bpor5ofiAV4HuinyfFEdse"),
    vaultIndex: 0,
  },
  kamino: {
    program: address("KLend2g3cP87fffoy8q1mQqGKjrxjC8boSyAYavgmjD"),
    farmsProgram: address("FarmsPZpWu9i7Kky8tPN37rs2TpmMrAZrC7S7vJa91Hr"),
    market: address("6WEGfej9B9wjxRs6t4BYpb9iCXd8CpTpJ8fVSNzHCC5y"),
    obligation: address("Gtwj2FNuiPoV2mGLC5SpHZ9PCmDrHHKaHXtacRaqm8vT"),
    collateralReserve: address("AwCyCPZYJSZ93xcVKNK7jR8e1BHzJXq1D4bReNuh9woY"),
    debtReserve: address("Atj6UREVWa7WxbF2EMKNyfmYUY1U1txughe2gjhcPDCo"),
  },
  assets: {
    assetMint: address("EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"),
    collateralMint: address("AvZZF1YaZDziPY2RCK4oJrRVrbN3mTD9NL24hPeaZeUj"),
    tokenProgram: address("TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"),
    associatedTokenProgram: address("ATokenGPvbdGVxr1b2hvZbsiqW5xWH25efTNsLJA8knL"),
    decimals: 6,
    jupiterProofInputRaw: 1_000n,
    maxSlippageBps: 50,
  },
  programs: {
    voltr: address("vVoLTRjQmtFpiYoegx285Ze4gsLJ8ZxgFKVcuvmG1a8"),
    trustfulAdaptor: address("3pnpK9nrs1R65eMV1wqCXkDkhSgN18xb1G5pgYPwoZjJ"),
    jupiter: address("JUP6LkbZbjS1jKKwapdHNy74zcZ3tLUZoi5QNyVTaV4"),
    system: address("11111111111111111111111111111111"),
  },
} as const satisfies RwaMultiplyRouteSpec;

function canonicalJson(value: unknown): string {
  if (typeof value === "bigint") return JSON.stringify(value.toString());
  if (Array.isArray(value)) return `[${value.map(canonicalJson).join(",")}]`;
  if (value && typeof value === "object") {
    return `{${Object.entries(value)
      .sort(([left], [right]) => left.localeCompare(right))
      .map(([key, entry]) => `${JSON.stringify(key)}:${canonicalJson(entry)}`)
      .join(",")}}`;
  }
  return JSON.stringify(value);
}

export function rwaMultiplyRouteSpecSha256(
  route: RwaMultiplyRouteSpec = RWA_MULTIPLY_ROUTE,
): string {
  // Trustful is retained only as an inactive fallback. It cannot influence the
  // identity hash of the active custom-adaptor route.
  const { trustfulBridge: _trustfulBridge, ...activeRoute } = route;
  return createHash("sha256").update(canonicalJson(activeRoute)).digest("hex");
}
