import { createHash } from "node:crypto";

import { address, type Address } from "@solana/kit";

export type DeploymentIdentity = Readonly<{
  programId: Address;
  programDataAddress: Address;
  deployedSlot: bigint;
  executableSha256: string;
}>;

export type PartnerStrategyId = "main" | "onre" | "prime" | "maple";

export type PartnerStrategyCandidate = Readonly<{
  id: PartnerStrategyId;
  reserve: Address;
}>;

export type PartnerStrategyGraphIdentity = Readonly<{
  id: PartnerStrategyId;
  reserve: Address;
  graph: Readonly<{
    lendingMarket: Address;
    lendingMarketAuthority: Address;
    obligation: Address;
    userMetadata: Address;
    reserveLiquiditySupply: Address;
    reserveCollateralMint: Address;
    reserveCollateralSupplyVault: Address;
    scope: Address;
    reserveFarmState: Address;
    obligationFarm: Address;
  }>;
  voltr: Readonly<{
    strategyAuth: Address;
    strategyInitReceipt: Address;
    strategyAssetAta: Address;
  }>;
}>;

/**
 * The product-level four-reserve allowlist. Market, farm, Scope, vault and
 * obligation identities are deliberately not copied here: Chunk 0 resolves
 * them from confirmed KLend account bytes and freezes them only after the live
 * compatibility verifier passes.
 */
export const PARTNER_STRATEGY_CANDIDATES = [
  {
    id: "main",
    reserve: address("D6q6wuQSrifJKZYpR1M8R4YawnLDtDsMmWM1NbBmgJ59"),
  },
  {
    id: "onre",
    reserve: address("AYL4LMc4ZCVyq3Z7XPJGWDM4H9PiWjqXAAuuHBEGVR2Z"),
  },
  {
    id: "prime",
    reserve: address("9GJ9GBRwCp4pHmWrQ43L5xpc9Vykg7jnfwcFGN8FoHYu"),
  },
  {
    id: "maple",
    reserve: address("Atj6UREVWa7WxbF2EMKNyfmYUY1U1txughe2gjhcPDCo"),
  },
] as const satisfies readonly PartnerStrategyCandidate[];

/** Discovery-time identities frozen after the first confirmed four-market probe. */
export const PARTNER_SCOPE_PROGRAM = address(
  "HFn8GnPADiny6XqUoWE8uRPPxb29ikn4yTuPa9MF2fWJ",
);

export const PARTNER_SCOPE_ORACLE_MAPPINGS = address(
  "4zh6bmb77qX2CL7t5AJYCqa6YqFafbz3QJNeFvZjLowg",
);

export const PARTNER_LOOKUP_TABLE_COMPATIBILITY_IDENTITY = {
  address: address("HSmmBwB7ZRWEsWf4q47w65hXfmqNrfP67KDtpuVrHK7T"),
  authority: address("BAqgbERmvUViqDSx961xpRBHGt68SpACiWL4t9696qZZ"),
  addressCount: 185,
  orderedAddressesSha256:
    "901173cf1cc0bafa9152c66425eb5a4c05819cbdfa742bc9c489d4fa167157c5",
} as const;

/**
 * Immutable graph identities discovered once and frozen before any four-market
 * policy generation. Mutable reserve economics (rates, balances, refresh slots,
 * oracle values, and account-data hashes) deliberately do not belong here.
 */
export const PARTNER_FOUR_MARKET_STRATEGIES = [
  {
    id: "main",
    reserve: address("D6q6wuQSrifJKZYpR1M8R4YawnLDtDsMmWM1NbBmgJ59"),
    graph: {
      lendingMarket: address("7u3HeHxYDLhnCoErrtycNokbQYbWGzLs6JSDqGAv5PfF"),
      lendingMarketAuthority: address("9DrvZvyWh1HuAoZxvYWMvkf2XCzryCpGgHqrMjyDWpmo"),
      obligation: address("51YGhznwYD4pPfdwyGA1cqL2KPDtWUr4pJNyaLa8NwHj"),
      userMetadata: address("9pgc2m2YehaUP13rZRiFsC6UkV1nRJVEv3EjQTMzQSpD"),
      reserveLiquiditySupply: address("Bgq7trRgVMeq33yt235zM2onQ4bRDBsY5EWiTetF4qw6"),
      reserveCollateralMint: address("B8V6WVjPxW1UGwVDfxH2d2r8SyT4cqn7dQRK6XneVa7D"),
      reserveCollateralSupplyVault: address("3DzjXRfxRm6iejfyyMynR4tScddaanrePJ1NJU2XnPPL"),
      scope: address("3t4JZcueEzTbVP6kLxXrL3VpWx45jDer4eqysweBchNH"),
      reserveFarmState: address("JAvnB9AKtgPsTEoKmn24Bq64UMoYcrtWtq42HHBdsPkh"),
      obligationFarm: address("98dATwikz9cioKBvbvSMC95XwSqS4Eht2EeBmdChHoWu"),
    },
    voltr: {
      strategyAuth: address("FMrNcfz8eRHzwAGwh1XHJxTjYebojcdfi7viEw8BdWfZ"),
      strategyInitReceipt: address("8TrCAoobPV9cygRG59LforafAmVE5QLa9HBS76GEG2gh"),
      strategyAssetAta: address("BjBuUPgRB6fNeLDWbSJ61owT1sRspmXP46sypLXAGbwn"),
    },
  },
  {
    id: "onre",
    reserve: address("AYL4LMc4ZCVyq3Z7XPJGWDM4H9PiWjqXAAuuHBEGVR2Z"),
    graph: {
      lendingMarket: address("47tfyEG9SsdEnUm9cw5kY9BXngQGqu3LBoop9j5uTAv8"),
      lendingMarketAuthority: address("FsvTiXTUFDc4aLbrov4PrvDTjXCWCniL1dxTUkZ1T2ss"),
      obligation: address("DD5s1EtffBhcWwpUAeTYfnKptU7ZHmRpMw7gGzBdUt8i"),
      userMetadata: address("DNbjPmFrGRs3e1rdz8BJEWDWdS1stFHdbxvaLrjQtm5M"),
      reserveLiquiditySupply: address("8BkQTZsT8ssKMU643De4iiV5Wf3pENdUFTsdtHPueKjB"),
      reserveCollateralMint: address("DBieuGmP1xh36oZRwTtw722yJ8pzZ8wycYD9nY2BSxwn"),
      reserveCollateralSupplyVault: address("6aTxdy7Hg7MHtE8NDBZWxxw9tnpj56XmdoCpkf8g2hxZ"),
      scope: address("3t4JZcueEzTbVP6kLxXrL3VpWx45jDer4eqysweBchNH"),
      reserveFarmState: address("GNcywqL6AZajsyyitxGQUvbihPgAzGZUqKfjYcvTj2pi"),
      obligationFarm: address("DRw4nRsRB3uqT6qei7fVPCZpdhqmzTMdEencoha2H8HW"),
    },
    voltr: {
      strategyAuth: address("JC3ijTWLpzvU18H9j8Q7pWa1LBAUGx5arzoBVcy6cmZp"),
      strategyInitReceipt: address("2dhBPLc2s69FBck4Kdz5PzGanV4YsRL5M4nY3KdVUJpo"),
      strategyAssetAta: address("Bp7ngTwvHmP69xPzNKHGje6matu4vMTchzLLTu1sPZY5"),
    },
  },
  {
    id: "prime",
    reserve: address("9GJ9GBRwCp4pHmWrQ43L5xpc9Vykg7jnfwcFGN8FoHYu"),
    graph: {
      lendingMarket: address("CqAoLuqWtavaVE8deBjMKe8ZfSt9ghR6Vb8nfsyabyHA"),
      lendingMarketAuthority: address("9SLBVnPz8dRGvafST6zNBZYSSt3HtdU68XQLGR13t3uM"),
      obligation: address("2qKSuV1CsyH1oQWdMhW2bo5ULtSXQ9erfJ9MyAtXgVam"),
      userMetadata: address("91bWDMTFZXRR8NASHamNCa9napdNpqX4HM34Bqu8kSLR"),
      reserveLiquiditySupply: address("H6JUwz8c61eQnYUx8avGXydKztKPyGvgWAUjmZUPS3BC"),
      reserveCollateralMint: address("DKaVQFXD6Qz4USTkRWyPun3oU6r1RfYsWJ8YqLpnSnN5"),
      reserveCollateralSupplyVault: address("CtgiQTkAQp8h1ayqdE21Cr56qekdqeQ19da3j1KLgSUn"),
      scope: address("3t4JZcueEzTbVP6kLxXrL3VpWx45jDer4eqysweBchNH"),
      reserveFarmState: address("HqEqwkTmqCAVEQQaEBuSSGD2EAvcorFogqhZz46TYJyz"),
      obligationFarm: address("7qJnuaE45Mp8gW7aizS14XKggNEAvBmcMQVCb377kZBu"),
    },
    voltr: {
      strategyAuth: address("82DNksF4PDEmp7RqJbCvJDbyyXSfTqFxT3f6tyNkQsF1"),
      strategyInitReceipt: address("Gyhy9cX1fyjxhYKbRbt3rpswRg8JZRPvMgGorMWdHJ9U"),
      strategyAssetAta: address("3nspEGSXtY2zKTnuY7SujPPb8bCnfCJ5e99Wf9pBuH17"),
    },
  },
  {
    id: "maple",
    reserve: address("Atj6UREVWa7WxbF2EMKNyfmYUY1U1txughe2gjhcPDCo"),
    graph: {
      lendingMarket: address("6WEGfej9B9wjxRs6t4BYpb9iCXd8CpTpJ8fVSNzHCC5y"),
      lendingMarketAuthority: address("6QbtpY2jDNcncRFmVf343NThnCdaY8gCAsYATPnYQR9g"),
      obligation: address("C3YtVXgwyoKcqi5cqb3d2evhoJjmBWrLF28SUcMSFdo1"),
      userMetadata: address("8WKv88bcBcVskigBaRSLacs8C1vhAV9sBaFQy6ufDLnD"),
      reserveLiquiditySupply: address("BBcwMNSMyhhBnYE9pevEvkxKHGzTafMP9v3j7Kk7nAWM"),
      reserveCollateralMint: address("6M89FWrQaqcy3domy85J1a1wVMnviL86WeUqbqTXf1qb"),
      reserveCollateralSupplyVault: address("25x4aEFoJE3bk4sdNLgHrrmchyop1JvcmGA4ccA6tWWT"),
      scope: address("3t4JZcueEzTbVP6kLxXrL3VpWx45jDer4eqysweBchNH"),
      reserveFarmState: address("6Y9fzrWzGZaxdAJ2eWRg9UZpL3kqPDiVXAb67KJpWdUg"),
      obligationFarm: address("WkLYcygLTU8kdHRQnVeiqhG9dukKeo7YUxfd4Ymrm4g"),
    },
    voltr: {
      strategyAuth: address("DCNxVeadKhtB965n515k5utKyAzgBGjYm2S9oy92H9Jm"),
      strategyInitReceipt: address("9MajR4HSgdRkWiFsAum83P5F6EYNGjgscLGk97eEN6dC"),
      strategyAssetAta: address("5vjmiwoH9g8kJRatHM5gsBAw8eZVjjvjXjVTn6GfhWon"),
    },
  },
] as const satisfies readonly PartnerStrategyGraphIdentity[];

export const PARTNER_FOUR_MARKET_ROUTE = {
  schemaVersion: 1,
  id: "loyal-backyard-four-market-usdc-v1",
  baseRouteId: "loyal-backyard-main-usdc-v1",
  baseMainRouteSpecSha256:
    "31e2de6705ccaa64df4625bc747c4fb9a6f9ff3142fd05b1132aa0ca2d90d234",
  withdrawalWaitingPeriodSeconds: 600n,
  normalOptimizationIntervalSeconds: 3_600n,
  normalOptimizationIdleFloorRaw: 0n,
  withdrawalRestorationPriority: true,
  scope: {
    program: PARTNER_SCOPE_PROGRAM,
    oracleMappings: PARTNER_SCOPE_ORACLE_MAPPINGS,
  },
  lookupTable: PARTNER_LOOKUP_TABLE_COMPATIBILITY_IDENTITY,
  commonVoltr: {
    protocol: address("4sycXz9Xwevedo6eiXR8QEhY8yrQrkNS4G1deY9tAD2Y"),
    idleAuth: address("C8geyt5kKSDoXYPrSvDee6Rv9ooBzXLiQLmCSUjamcfo"),
    idleAta: address("9LHpTxtFDYb8xJAruX9uTrceohFms2KyRvkXREj3iV9P"),
    lpMint: address("dbQkLsUYE7ADHHv8XEottANAa773K4xM4nyPjVdutka"),
    lpMintAuth: address("BqKLmhKUy4Q7iHHGKKPSzenPL7LbEed9ZW3pTBofxuZn"),
    adaptorAddReceipt: address("Gaq7gNF3CyZucQS9XRKWz44fRfJuAVbs6pckJbuksDHt"),
  },
  strategies: PARTNER_FOUR_MARKET_STRATEGIES,
} as const;

export type PartnerRouteSpec = Readonly<{
  schemaVersion: 1;
  id: "loyal-backyard-main-usdc-v1";
  cluster: "mainnet-beta";
  genesisHash: string;
  sdk: Readonly<{
    voltrVault: "2.1.1";
    kaminoKlend: "7.3.9";
  }>;
  vault: Address;
  setupAdmin: Address;
  squads: Readonly<{
    program: Address;
    settings: Address;
    vaultIndex: 1;
    manager: Address;
    guardian: Address;
    guardianPermissionsMask: 7;
    threshold: 1;
    policySeedBefore: bigint;
    depositPolicySeed: bigint;
    withdrawPolicySeed: bigint;
  }>;
  asset: Readonly<{
    mint: Address;
    tokenProgram: Address;
    decimals: 6;
    vaultCapRaw: bigint;
    proofAmountRaw: bigint;
    maxManagerOperationRaw: bigint;
  }>;
  vaultConfiguration: Readonly<{
    name: "Backyard Loyal USDC";
    description: "Policy-constrained Voltr vault managed by Loyal Squads";
    withdrawalWaitingPeriodSeconds: 600n;
    lockedProfitDegradationDurationSeconds: 86_400n;
    startAtTs: 0n;
    managerPerformanceFeeBps: number;
    adminPerformanceFeeBps: number;
    managerManagementFeeBps: number;
    adminManagementFeeBps: number;
    redemptionFeeBps: number;
    issuanceFeeBps: number;
    disabledOperations: 0;
    allowAnyAdaptor: 0;
  }>;
  programs: Readonly<{
    voltrVault: Address;
    kaminoAdaptor: Address;
    klend: Address;
    farms: Address;
    token: Address;
    associatedToken: Address;
    system: Address;
  }>;
  strategy: Readonly<{
    reserve: Address;
    lendingMarket: Address;
    collateralFarm: Address;
  }>;
  lookupTable: Readonly<{
    address: Address;
    authority: Address;
  }>;
  deployments: readonly DeploymentIdentity[];
}>;

export const PARTNER_ROUTE = {
  schemaVersion: 1,
  id: "loyal-backyard-main-usdc-v1",
  cluster: "mainnet-beta",
  genesisHash: "5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d",
  sdk: { voltrVault: "2.1.1", kaminoKlend: "7.3.9" },
  vault: address("AdwKLBQWKxNewpkjMFMz4NyKit7qXygGpjkqHBCWcriK"),
  setupAdmin: address("BAqgbERmvUViqDSx961xpRBHGt68SpACiWL4t9696qZZ"),
  squads: {
    program: address("SMRTzfY6DfH5ik3TKiyLFfXexV8uSG3d2UksSCYdunG"),
    settings: address("5YQ78RwqukvCcykpmjmgRFmbEUeAgLpuVDxx1xNZnHD6"),
    vaultIndex: 1,
    manager: address("DMPn3d7G2rcVVhvRbpSyEeq3cBW7bygiGjSgrLci5FYK"),
    guardian: address("oz8skK9o2N5w85rrkMfBVdeg6wnjAqMzriVSupERo3C"),
    guardianPermissionsMask: 7,
    threshold: 1,
    policySeedBefore: 42n,
    depositPolicySeed: 43n,
    withdrawPolicySeed: 44n,
  },
  asset: {
    mint: address("EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"),
    tokenProgram: address("TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"),
    decimals: 6,
    vaultCapRaw: 1_000_000_000_000n,
    proofAmountRaw: 1_000_000n,
    maxManagerOperationRaw: 200_000_000_000n,
  },
  vaultConfiguration: {
    name: "Backyard Loyal USDC",
    description: "Policy-constrained Voltr vault managed by Loyal Squads",
    withdrawalWaitingPeriodSeconds: 600n,
    lockedProfitDegradationDurationSeconds: 86_400n,
    startAtTs: 0n,
    managerPerformanceFeeBps: 0,
    adminPerformanceFeeBps: 500,
    managerManagementFeeBps: 0,
    adminManagementFeeBps: 0,
    redemptionFeeBps: 0,
    issuanceFeeBps: 0,
    disabledOperations: 0,
    allowAnyAdaptor: 0,
  },
  programs: {
    voltrVault: address("vVoLTRjQmtFpiYoegx285Ze4gsLJ8ZxgFKVcuvmG1a8"),
    kaminoAdaptor: address("to6Eti9CsC5FGkAtqiPphvKD2hiQiLsS8zWiDBqBPKR"),
    klend: address("KLend2g3cP87fffoy8q1mQqGKjrxjC8boSyAYavgmjD"),
    farms: address("FarmsPZpWu9i7Kky8tPN37rs2TpmMrAZrC7S7vJa91Hr"),
    token: address("TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"),
    associatedToken: address("ATokenGPvbdGVxr1b2hvZbsiqW5xWH25efTNsLJA8knL"),
    system: address("11111111111111111111111111111111"),
  },
  strategy: {
    reserve: address("D6q6wuQSrifJKZYpR1M8R4YawnLDtDsMmWM1NbBmgJ59"),
    lendingMarket: address("7u3HeHxYDLhnCoErrtycNokbQYbWGzLs6JSDqGAv5PfF"),
    collateralFarm: address("JAvnB9AKtgPsTEoKmn24Bq64UMoYcrtWtq42HHBdsPkh"),
  },
  lookupTable: {
    address: address("HSmmBwB7ZRWEsWf4q47w65hXfmqNrfP67KDtpuVrHK7T"),
    authority: address("BAqgbERmvUViqDSx961xpRBHGt68SpACiWL4t9696qZZ"),
  },
  deployments: [
    {
      programId: address("vVoLTRjQmtFpiYoegx285Ze4gsLJ8ZxgFKVcuvmG1a8"),
      programDataAddress: address("3fiAyUjktZkZf6hcbBPy6U6UdkMdEFoToS4sjtzAd5az"),
      deployedSlot: 433_299_444n,
      executableSha256: "674066350ecf33114c430604bee65b23c7a4d65a01c606fa3863c93327ce9fbd",
    },
    {
      programId: address("to6Eti9CsC5FGkAtqiPphvKD2hiQiLsS8zWiDBqBPKR"),
      programDataAddress: address("9JAuHw1UPaKdpa1QhtzfSEQAGAP1NmTMdcuxgqiz1ovR"),
      deployedSlot: 406_317_018n,
      executableSha256: "933f17362069cc09f60e4267a5dbb929893a54068a913bd293ec7deee90129e0",
    },
    {
      programId: address("KLend2g3cP87fffoy8q1mQqGKjrxjC8boSyAYavgmjD"),
      programDataAddress: address("9uSbGW1y9H5Av6H5TKxQ1wnFApSq2t3oEpfF2YfjDQGA"),
      deployedSlot: 440_486_775n,
      executableSha256: "9db16dd4b7bbfe4f13df850bf880bfc4522fcece06717c0626d625746a3cc85b",
    },
    {
      programId: address("FarmsPZpWu9i7Kky8tPN37rs2TpmMrAZrC7S7vJa91Hr"),
      programDataAddress: address("5Fz4tY19ihWJAW1j9RNoEQJKSdn5mQHjcVrcZ5BRK5E9"),
      deployedSlot: 379_693_642n,
      executableSha256: "5e42feeccadeb5dcd80e11a62b15141c4e127b21db2ae8ca2afa6c281505f564",
    },
    {
      programId: address("SMRTzfY6DfH5ik3TKiyLFfXexV8uSG3d2UksSCYdunG"),
      programDataAddress: address("2g3u9qgz4adKQVN1TUoh7bbBKqaSsjXtz1yX2ptagW5T"),
      deployedSlot: 383_815_455n,
      executableSha256: "49cf27024d211ab827eadc11219a935abf9a5138ece1c0b0631c26790fd4f3c0",
    },
  ],
} as const satisfies PartnerRouteSpec;

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

export function routeSpecSha256(route: PartnerRouteSpec = PARTNER_ROUTE): string {
  return createHash("sha256").update(canonicalJson(route)).digest("hex");
}

export function fourMarketRouteSpecSha256(): string {
  return createHash("sha256")
    .update(canonicalJson(PARTNER_FOUR_MARKET_ROUTE))
    .digest("hex");
}

export function partnerStrategyIdentity(
  strategyId: PartnerStrategyId,
): PartnerStrategyGraphIdentity {
  const strategy = PARTNER_FOUR_MARKET_STRATEGIES.find(
    ({ id }) => id === strategyId,
  );
  if (!strategy) {
    throw new Error(`unsupported Backyard strategy id ${strategyId}`);
  }
  return strategy;
}

/**
 * Adapter for the pinned Voltr SDK, whose builder accepts one strategy graph at
 * a time. Authorization always binds fourMarketRouteSpecSha256(); this
 * strategy-specific view is never treated as the product route manifest.
 */
export function partnerBuilderRoute(
  strategyId: PartnerStrategyId,
): PartnerRouteSpec {
  const strategy = partnerStrategyIdentity(strategyId);
  return {
    ...PARTNER_ROUTE,
    strategy: {
      reserve: strategy.reserve,
      lendingMarket: strategy.graph.lendingMarket,
      collateralFarm: strategy.graph.reserveFarmState,
    },
  };
}

export function partnerStrategyGraphSha256(
  strategyId: PartnerStrategyId,
): string {
  return createHash("sha256")
    .update(canonicalJson(partnerStrategyIdentity(strategyId)))
    .digest("hex");
}
