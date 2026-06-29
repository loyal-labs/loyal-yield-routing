import { describe, expect, test } from "bun:test";
import {
  PublicKey,
  SystemProgram,
  TransactionInstruction,
  TransactionMessage,
  VersionedTransaction,
} from "@solana/web3.js";
import {
  DEFAULT_MAX_FEE_BPS,
  KAMINO_ALTCOINS_MARKET,
  KAMINO_BITCOIN_MARKET,
  KAMINO_HUMA_MARKET,
  KAMINO_JLP_MARKET,
  KAMINO_MAIN_MARKET,
  KAMINO_SOLSTICE_MARKET,
  KAMINO_SUPERSTATE_OPENING_BELL_MARKET,
  KAMINO_XSTOCKS_MARKET,
  LOYAL_CLUSTER_CONFIGS,
  LoyalCluster,
  MaxFeeBps,
  RISK_BASKET_MARKETS,
  RiskBasket,
  STABLECOIN_MINTS,
  Stablecoin,
  TREASURY_JUPITER_SWAP_ACTION_SEED,
  TREASURY_REBALANCE_ACTION_SEED,
  TREASURY_TOP_UP_ACTION_SEED,
  SwapLane,
  assertRebalanceAvoidsActiveLanes,
  compileSquadsTransactionInstructions,
  createLoyalActionsSdk,
  createProgramInteractionPolicyUpdateInstruction,
  createSquadsProgramInteractionExecutionInstruction,
  createSquadsSmartAccountInstruction,
  createSquadsSyncTransactionInstruction,
  deriveSquadsPolicy,
  deriveSquadsProgramConfig,
  deriveSquadsSettings,
  deriveSquadsVault,
} from "../src/index.js";
import type { AccountConstraint, DataConstraint, InstructionConstraint } from "../src/index.js";
import {
  deriveAssociatedTokenAddress,
  deriveLoyalHubAuthority,
  deriveKaminoUserMetadata,
  deriveKaminoVanillaObligation,
  kaminoInitObligationConstraint,
  loyalHubConstraint,
} from "../src/internal/protocols.js";
import {
  WITHDRAW_INVENTORY,
  WITHDRAW_INVENTORY_AMOUNT_DATA_OFFSET,
  WITHDRAW_INVENTORY_LANE_ID_DATA_OFFSET,
  WITHDRAW_INVENTORY_TAG_OFFSET,
  swapExactInAccounts,
  withdrawInventoryAccounts,
} from "../src/generated/loyal-hub-abi.js";
import {
  KAMINO_INIT_OBLIGATION_DISCRIMINATOR,
  KAMINO_VANILLA_OBLIGATION_ID,
  KAMINO_VANILLA_OBLIGATION_TAG,
} from "../src/constants.js";

const settings = new PublicKey("11111111111111111111111111111112");
const authority = new PublicKey("11111111111111111111111111111113");
const delegatedSigner = new PublicKey("11111111111111111111111111111114");
const vault = new PublicKey("11111111111111111111111111111115");
const spendingLimitMint = new PublicKey("11111111111111111111111111111116");

const squads = {
  settings,
  authority,
  delegatedSigner,
  accountIndex: 0,
  vault,
};
const SOLANA_PACKET_DATA_SIZE = 1232;

function i64Le(value: bigint): number[] {
  const bytes: number[] = [];
  let remaining = BigInt.asUintN(64, value);
  for (let index = 0; index < 8; index += 1) {
    bytes.push(Number(remaining & 0xffn));
    remaining >>= 8n;
  }
  return bytes;
}

function findBytes(haystack: readonly number[], needle: readonly number[]): number {
  return haystack.findIndex((_, index) =>
    needle.every((byte, offset) => haystack[index + offset] === byte)
  );
}

describe("initYieldRoutePolicy", () => {
  test("builds one all-in-one policy instruction and route indexes for Jupiter", () => {
    const sdk = createLoyalActionsSdk({ cluster: LoyalCluster.MainnetBeta });
    const policy = sdk.initYieldRoutePolicy({
      risk: RiskBasket.Safe,
      swapLanes: [SwapLane.Jupiter] as const,
      squads,
    });

    expect(policy.instructions).toHaveLength(1);
    expect(policy.instructions[0]?.programId.toBase58()).toBe("SMRTzfY6DfH5ik3TKiyLFfXexV8uSG3d2UksSCYdunG");
    expect(policy.instructions[0]?.keys.map((key) => [key.pubkey.toBase58(), key.isSigner, key.isWritable])).toEqual([
      [settings.toBase58(), false, true],
      [authority.toBase58(), true, true],
      ["11111111111111111111111111111111", false, false],
      ["SMRTzfY6DfH5ik3TKiyLFfXexV8uSG3d2UksSCYdunG", false, false],
      [authority.toBase58(), true, false],
      [policy.actionAccount.toBase58(), false, true],
    ]);
    expect(policy.instructions[0]?.data.subarray(0, 8).toJSON().data).toEqual([138, 209, 64, 163, 79, 67, 233, 76]);
    expect(policy.routes.sameMint.instructionConstraintIndexes).toEqual([0, 2]);
    expect(policy.routes.jupiter.instructionConstraintIndexes).toEqual([0, 1, 2]);
    expect(policy.routes.loyal).toBeUndefined();
    expect(policy.spec.maxFeeBps).toBe(DEFAULT_MAX_FEE_BPS);
  });

  test("computes route indexes for Loyal and combined lane order", () => {
    const sdk = createLoyalActionsSdk({ cluster: LoyalCluster.MainnetBeta });
    const loyalOnly = sdk.initYieldRoutePolicy({
      risk: RiskBasket.Safe,
      swapLanes: [SwapLane.Loyal] as const,
      squads,
    });
    const both = sdk.initYieldRoutePolicy({
      risk: RiskBasket.Safe,
      swapLanes: [SwapLane.Jupiter, SwapLane.Loyal] as const,
      maxFeeBps: MaxFeeBps.Bps150,
      squads,
    });

    expect(loyalOnly.routes.sameMint.instructionConstraintIndexes).toEqual([0, 2]);
    expect(loyalOnly.routes.loyal.instructionConstraintIndexes).toEqual([0, 1, 2]);
    expect(loyalOnly.routes.jupiter).toBeUndefined();
    expect(both.routes.sameMint.instructionConstraintIndexes).toEqual([0, 3]);
    expect(both.routes.jupiter.instructionConstraintIndexes).toEqual([0, 1, 3]);
    expect(both.routes.loyal.instructionConstraintIndexes).toEqual([0, 2, 3]);
    expect(both.spec.maxFeeBps).toBe(MaxFeeBps.Bps150);
  });

  test("builds same-mint-only route indexes without swap lanes", () => {
    const sdk = createLoyalActionsSdk({ cluster: LoyalCluster.MainnetBeta });
    const policy = sdk.initYieldRoutePolicy({
      risk: RiskBasket.Safe,
      swapLanes: [] as const,
      squads,
    });

    expect(policy.instructions).toHaveLength(1);
    expect(policy.routes.sameMint.instructionConstraintIndexes).toEqual([0, 1]);
    expect(policy.routes.jupiter).toBeUndefined();
    expect(policy.routes.loyal).toBeUndefined();
    expect(policy.spec.swapLanes).toEqual([]);
  });

  test("builds market-scoped init obligation constraint with KLend vanilla seeds", () => {
    const constraint = kaminoInitObligationConstraint(vault, [KAMINO_MAIN_MARKET]);
    const pubkeyAt = (index: number) => {
      const accountConstraint = constraint.accountConstraints.find((account) => account.accountIndex === index);
      expect(accountConstraint?.kind.type).toBe("pubkey");
      if (accountConstraint?.kind.type !== "pubkey") {
        throw new Error(`account ${index} is not a pubkey constraint`);
      }
      return accountConstraint.kind.pubkeys.map((pubkey) => pubkey.toBase58());
    };

    expect(pubkeyAt(0)).toEqual([vault.toBase58()]);
    expect(pubkeyAt(1)).toEqual([vault.toBase58()]);
    expect(pubkeyAt(2)).toEqual([deriveKaminoVanillaObligation(vault, KAMINO_MAIN_MARKET).toBase58()]);
    expect(pubkeyAt(3)).toEqual([KAMINO_MAIN_MARKET.toBase58()]);
    expect(pubkeyAt(4)).toEqual([PublicKey.default.toBase58()]);
    expect(pubkeyAt(5)).toEqual([PublicKey.default.toBase58()]);
    expect(pubkeyAt(6)).toEqual([deriveKaminoUserMetadata(vault).toBase58()]);
    expect(pubkeyAt(7)).toEqual(["SysvarRent111111111111111111111111111111111"]);
    expect(pubkeyAt(8)).toEqual([SystemProgram.programId.toBase58()]);
    expect(constraint.dataConstraints).toEqual([
      {
        dataOffset: 0n,
        dataValue: {
          type: "u8Slice",
          value: [...KAMINO_INIT_OBLIGATION_DISCRIMINATOR, KAMINO_VANILLA_OBLIGATION_TAG, KAMINO_VANILLA_OBLIGATION_ID],
        },
        operator: "equals",
      },
    ]);
  });

  test("keeps Loyal Hub policy constraints compatible with Token-2022 swap accounts", () => {
    const config = LOYAL_CLUSTER_CONFIGS[LoyalCluster.MainnetBeta];
    const constraint = loyalHubConstraint(
      config,
      vault,
      [STABLECOIN_MINTS[Stablecoin.USDC], STABLECOIN_MINTS[Stablecoin.PYUSD]],
      DEFAULT_MAX_FEE_BPS,
    );
    const accountAt = (index: number) => {
      const accountConstraint = constraint.accountConstraints.find((account) => account.accountIndex === index);
      expect(accountConstraint).toBeDefined();
      return accountConstraint;
    };

    expect(accountAt(swapExactInAccounts.INPUT_MINT)?.owner).toBeUndefined();
    expect(accountAt(swapExactInAccounts.OUTPUT_MINT)?.owner).toBeUndefined();
    expect(accountAt(swapExactInAccounts.USER_INPUT)?.owner).toBeUndefined();
    expect(accountAt(swapExactInAccounts.USER_OUTPUT)?.owner).toBeUndefined();
    const token2022 = accountAt(swapExactInAccounts.TOKEN_2022_PROGRAM);
    expect(token2022?.kind.type).toBe("pubkey");
    if (token2022?.kind.type !== "pubkey") {
      throw new Error("TOKEN_2022_PROGRAM should be a pubkey constraint");
    }
    expect(token2022.kind.pubkeys.map((pubkey) => pubkey.toBase58())).toEqual([
      config.token2022ProgramId.toBase58(),
    ]);
  });

  test("builds a treasury Loyal Hub rebalance policy with exact Hub Jupiter and top-up constraints", () => {
    const sdk = createLoyalActionsSdk({ cluster: LoyalCluster.MainnetBeta });
    const config = LOYAL_CLUSTER_CONFIGS[LoyalCluster.MainnetBeta];
    const laneId = 2;
    const inputMint = STABLECOIN_MINTS[Stablecoin.USDC];
    const outputMint = STABLECOIN_MINTS[Stablecoin.PYUSD];
    const maxWithdrawAmount = 500000n;
    const maxTopUpAmount = 495000n;
    const policy = sdk.initTreasuryLoyalHubRebalancePolicy({
      laneId,
      inputMint,
      outputMint,
      inputTokenProgram: config.tokenProgramId,
      outputTokenProgram: config.token2022ProgramId,
      outputMintDecimals: 6,
      maxWithdrawAmount,
      maxTopUpAmount,
      maxSlippageBps: 50,
      squads,
    });
    const hubAuthority = deriveLoyalHubAuthority(config.loyalHubSwapProgramId, laneId);
    const treasuryInput = deriveAssociatedTokenAddress(inputMint, vault, config.tokenProgramId);
    const treasuryOutput = deriveAssociatedTokenAddress(outputMint, vault, config.token2022ProgramId);
    const hubInput = deriveAssociatedTokenAddress(inputMint, hubAuthority, config.tokenProgramId);
    const hubOutput = deriveAssociatedTokenAddress(outputMint, hubAuthority, config.token2022ProgramId);

    expect(policy.instructions).toHaveLength(3);
    expect(policy.policies.withdraw.instructions[0]?.data[22]).toBe(4);
    expect(policy.policies.jupiter.instructions[0]?.data[22]).toBe(4);
    expect(policy.policies.topUp.instructions[0]?.data[22]).toBe(4);
    expect(policy.policies.withdraw.actionAccount.toBase58()).toBe(
      deriveSquadsPolicy(config, settings, TREASURY_REBALANCE_ACTION_SEED).address.toBase58(),
    );
    expect(policy.policies.jupiter.actionAccount.toBase58()).toBe(
      deriveSquadsPolicy(config, settings, TREASURY_JUPITER_SWAP_ACTION_SEED).address.toBase58(),
    );
    expect(policy.policies.topUp.actionAccount.toBase58()).toBe(
      deriveSquadsPolicy(config, settings, TREASURY_TOP_UP_ACTION_SEED).address.toBase58(),
    );
    expect(policy.policies.withdraw.route.instructionConstraintIndexes).toEqual([0]);
    expect(policy.policies.jupiter.route.instructionConstraintIndexes).toEqual([0]);
    expect(policy.policies.topUp.route.instructionConstraintIndexes).toEqual([0]);
    expect(policy.policies.withdraw.constraints).toHaveLength(1);
    expect(policy.policies.jupiter.constraints).toHaveLength(1);
    expect(policy.policies.topUp.constraints).toHaveLength(1);

    const [withdraw] = policy.policies.withdraw.constraints;
    const [jupiter] = policy.policies.jupiter.constraints;
    const [topUp] = policy.policies.topUp.constraints;
    expect(withdraw?.programId.toBase58()).toBe(config.loyalHubSwapProgramId.toBase58());
    expectPubkeyConstraint(withdraw, withdrawInventoryAccounts.ADMIN, [vault]);
    expectPubkeyConstraint(withdraw, withdrawInventoryAccounts.HUB_SOURCE, [hubInput], config.tokenProgramId);
    expectAuthorityConstraint(withdraw, withdrawInventoryAccounts.HUB_SOURCE, hubAuthority);
    expectPubkeyConstraint(withdraw, withdrawInventoryAccounts.DESTINATION, [treasuryInput], config.tokenProgramId);
    expectAuthorityConstraint(withdraw, withdrawInventoryAccounts.DESTINATION, vault);
    expectPubkeyConstraint(withdraw, withdrawInventoryAccounts.MINT, [inputMint], config.tokenProgramId);
    expectPubkeyConstraint(withdraw, withdrawInventoryAccounts.HUB_AUTHORITY, [hubAuthority]);
    expectPubkeyConstraint(withdraw, withdrawInventoryAccounts.TOKEN_PROGRAM, [config.tokenProgramId]);
    expectDataU8(withdraw?.dataConstraints, BigInt(WITHDRAW_INVENTORY_TAG_OFFSET), WITHDRAW_INVENTORY);
    expectDataU64Lte(withdraw?.dataConstraints, BigInt(WITHDRAW_INVENTORY_AMOUNT_DATA_OFFSET), maxWithdrawAmount);
    expectDataU8(withdraw?.dataConstraints, BigInt(WITHDRAW_INVENTORY_LANE_ID_DATA_OFFSET), laneId);

    expect(jupiter?.programId.toBase58()).toBe(config.jupiterV6ProgramId.toBase58());
    expectPubkeyConstraint(jupiter, 1, [vault]);
    expectPubkeyConstraint(jupiter, 2, [treasuryInput], config.tokenProgramId);
    expectAuthorityConstraint(jupiter, 2, vault);
    expectPubkeyConstraint(jupiter, 5, [treasuryOutput], config.token2022ProgramId);
    expectAuthorityConstraint(jupiter, 5, vault);
    expectPubkeyConstraint(jupiter, 6, [inputMint], config.tokenProgramId);
    expectPubkeyConstraint(jupiter, 7, [outputMint], config.token2022ProgramId);
    expectPubkeyConstraint(jupiter, 8, [config.tokenProgramId]);
    expectPubkeyConstraint(jupiter, 9, [config.token2022ProgramId]);
    expectDataU16Lte(jupiter?.dataConstraints, 25n, 50);

    expect(topUp?.programId.toBase58()).toBe(config.token2022ProgramId.toBase58());
    expectPubkeyConstraint(topUp, 0, [treasuryOutput], config.token2022ProgramId);
    expectAuthorityConstraint(topUp, 0, vault);
    expectPubkeyConstraint(topUp, 1, [outputMint], config.token2022ProgramId);
    expectPubkeyConstraint(topUp, 2, [hubOutput], config.token2022ProgramId);
    expectAuthorityConstraint(topUp, 2, hubAuthority);
    expectPubkeyConstraint(topUp, 3, [vault]);
    expectDataU8(topUp?.dataConstraints, 0n, 12);
    expectDataU64Lte(topUp?.dataConstraints, 1n, maxTopUpAmount);
    expectDataU8(topUp?.dataConstraints, 9n, 6);
  });

  test("builds split treasury policy create transactions that fit the packet limit without lookup tables", () => {
    const sdk = createLoyalActionsSdk({ cluster: LoyalCluster.MainnetBeta });
    const config = LOYAL_CLUSTER_CONFIGS[LoyalCluster.MainnetBeta];
    const policy = sdk.initTreasuryLoyalHubRebalancePolicy({
      laneId: 0,
      inputMint: STABLECOIN_MINTS[Stablecoin.USDC],
      outputMint: STABLECOIN_MINTS[Stablecoin.PYUSD],
      inputTokenProgram: config.tokenProgramId,
      outputTokenProgram: config.token2022ProgramId,
      outputMintDecimals: 6,
      maxWithdrawAmount: 500000n,
      maxTopUpAmount: 495000n,
      maxSlippageBps: 50,
      squads,
    });

    for (const plan of [policy.policies.withdraw, policy.policies.jupiter, policy.policies.topUp]) {
      const message = new TransactionMessage({
        payerKey: authority,
        recentBlockhash: PublicKey.default.toBase58(),
        instructions: plan.instructions,
      }).compileToV0Message([]);
      const transaction = new VersionedTransaction(message);
      expect(transaction.serialize().length).toBeLessThanOrEqual(SOLANA_PACKET_DATA_SIZE);
    }
  });

  test("rejects invalid treasury rebalance policy inputs before instruction creation", () => {
    const sdk = createLoyalActionsSdk({ cluster: LoyalCluster.MainnetBeta });
    const config = LOYAL_CLUSTER_CONFIGS[LoyalCluster.MainnetBeta];
    const valid = {
      laneId: 0,
      inputMint: STABLECOIN_MINTS[Stablecoin.USDC],
      outputMint: STABLECOIN_MINTS[Stablecoin.PYUSD],
      inputTokenProgram: config.tokenProgramId,
      outputTokenProgram: config.token2022ProgramId,
      outputMintDecimals: 6,
      maxWithdrawAmount: 1n,
      maxTopUpAmount: 1n,
      maxSlippageBps: 50,
      squads,
    };

    expect(() => sdk.initTreasuryLoyalHubRebalancePolicy({ ...valid, laneId: 256 })).toThrow("laneId");
    expect(() => sdk.initTreasuryLoyalHubRebalancePolicy({ ...valid, outputMintDecimals: 256 })).toThrow("outputMintDecimals");
    expect(() => sdk.initTreasuryLoyalHubRebalancePolicy({ ...valid, maxSlippageBps: 10001 })).toThrow("maxSlippageBps");
    expect(() => sdk.initTreasuryLoyalHubRebalancePolicy({ ...valid, maxWithdrawAmount: 0n })).toThrow("maxWithdrawAmount");
    expect(() => sdk.initTreasuryLoyalHubRebalancePolicy({ ...valid, maxTopUpAmount: 0n })).toThrow("maxTopUpAmount");
  });

  test("derives stable exposure internally from the approved seven symbols", () => {
    const sdk = createLoyalActionsSdk({ cluster: LoyalCluster.MainnetBeta });
    const policy = sdk.initYieldRoutePolicy({
      risk: RiskBasket.Safe,
      swapLanes: [SwapLane.Jupiter] as const,
      squads,
    });

    expect(Object.values(Stablecoin).map(String)).toEqual(["USDC", "USDT", "PYUSD", "USDS", "USDG", "USDE", "SUSDE"]);
    expect(Object.keys(STABLECOIN_MINTS)).toEqual(Object.values(Stablecoin));
    expect(policy.spec.stablecoins).toEqual(Object.values(Stablecoin));
    expect(policy.spec.stableMints.map((mint) => mint.toBase58())).toEqual([
      "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v",
      "Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB",
      "2b1kV6DkPAnxd5ixfnxCpjxmKwqjjaYmCZfHsFu24GXo",
      "USDSwr9ApdHk5bvJKMjzff41FfuX8bSxdKcR81vTwcA",
      "2u1tszSeqZ3qBWF3uNGPFc8TzMk2tdiwknnRMWGWjGWH",
      "DEkqHyPN7GMRJ5cArtQFAWefqbZb33Hyf6s5iCwjEonT",
      "Eh6XEPhSwoLv5wFApukmnaVSHQ6sAnoD9BmgmwQoN2sN",
    ]);
    expect(policy.spec.kaminoLiquidityMints).toEqual(policy.spec.stableMints);
  });

  test("keeps risk baskets cumulative and curated", () => {
    const safe = RISK_BASKET_MARKETS[RiskBasket.Safe];
    const medium = RISK_BASKET_MARKETS[RiskBasket.Medium];
    const aggressive = RISK_BASKET_MARKETS[RiskBasket.Aggressive];

    expect(safe.every((market) => medium.includes(market))).toBe(true);
    expect(medium.every((market) => aggressive.includes(market))).toBe(true);
    for (const market of [KAMINO_JLP_MARKET, KAMINO_HUMA_MARKET, KAMINO_XSTOCKS_MARKET, KAMINO_SOLSTICE_MARKET, KAMINO_ALTCOINS_MARKET]) {
      expect(safe).not.toContain(market);
    }
    for (const market of [KAMINO_JLP_MARKET, KAMINO_BITCOIN_MARKET, KAMINO_SUPERSTATE_OPENING_BELL_MARKET]) {
      expect(medium).toContain(market);
    }
    expect(medium).not.toContain(KAMINO_ALTCOINS_MARKET);
    expect(aggressive).toContain(KAMINO_ALTCOINS_MARKET);
  });

  test("rejects invalid inputs before instruction creation", () => {
    const sdk = createLoyalActionsSdk({ cluster: LoyalCluster.MainnetBeta });

    expect(() =>
      sdk.initYieldRoutePolicy({
        risk: RiskBasket.Safe,
        swapLanes: "jupiter" as unknown as SwapLane[],
        squads,
      }),
    ).toThrow("swapLanes must be an array");
    expect(() =>
      sdk.initYieldRoutePolicy({
        risk: RiskBasket.Safe,
        swapLanes: [SwapLane.Jupiter, SwapLane.Jupiter],
        squads,
      }),
    ).toThrow("duplicate swap lane");
    expect(() =>
      sdk.initYieldRoutePolicy({
        risk: RiskBasket.Safe,
        swapLanes: [SwapLane.Jupiter],
        maxFeeBps: 99 as MaxFeeBps,
        squads,
      }),
    ).toThrow("unsupported maxFeeBps");
    expect(() => createLoyalActionsSdk({ cluster: "localnet" as LoyalCluster })).toThrow("unsupported Loyal cluster");
  });
});

function expectPubkeyConstraint(
  constraint: InstructionConstraint | undefined,
  accountIndex: number,
  pubkeys: PublicKey[],
  owner?: PublicKey,
): void {
  const account = findAccountConstraint(constraint, accountIndex, "pubkey", owner);
  expect(account?.kind.type).toBe("pubkey");
  if (account?.kind.type !== "pubkey") {
    throw new Error(`account ${accountIndex} is not a pubkey constraint`);
  }
  expect(account.kind.pubkeys.map((pubkey) => pubkey.toBase58())).toEqual(pubkeys.map((pubkey) => pubkey.toBase58()));
}

function expectAuthorityConstraint(
  constraint: InstructionConstraint | undefined,
  accountIndex: number,
  authority: PublicKey,
  owner?: PublicKey,
): void {
  const account = findAccountConstraint(constraint, accountIndex, "accountData", owner);
  expect(account?.kind.type).toBe("accountData");
  if (account?.kind.type !== "accountData") {
    throw new Error(`account ${accountIndex} is not an account-data constraint`);
  }
  expect(account.kind.dataConstraints).toContainEqual({
    dataOffset: 32n,
    dataValue: { type: "u8Slice", value: [...authority.toBytes()] },
    operator: "equals",
  });
}

function expectDataU8(constraints: readonly DataConstraint[] | undefined, offset: bigint, value: number): void {
  expect(constraints).toContainEqual({
    dataOffset: offset,
    dataValue: { type: "u8", value },
    operator: "equals",
  });
}

function expectDataU16Lte(constraints: readonly DataConstraint[] | undefined, offset: bigint, value: number): void {
  expect(constraints).toContainEqual({
    dataOffset: offset,
    dataValue: { type: "u16Le", value },
    operator: "lessThanOrEqualTo",
  });
}

function expectDataU64Lte(constraints: readonly DataConstraint[] | undefined, offset: bigint, value: bigint): void {
  expect(constraints).toContainEqual({
    dataOffset: offset,
    dataValue: { type: "u64Le", value },
    operator: "lessThanOrEqualTo",
  });
}

function findAccountConstraint(
  constraint: InstructionConstraint | undefined,
  accountIndex: number,
  kind: AccountConstraint["kind"]["type"],
  owner?: PublicKey,
): AccountConstraint | undefined {
  return constraint?.accountConstraints.find((account) => {
    if (account.accountIndex !== accountIndex || account.kind.type !== kind) {
      return false;
    }
    if (owner === undefined) {
      return account.owner === undefined;
    }
    return account.owner?.equals(owner) ?? false;
  });
}

describe("Squads operational helpers", () => {
  const config = LOYAL_CLUSTER_CONFIGS[LoyalCluster.MainnetBeta];

  test("derives settings, vault, policy, and program config PDAs", () => {
    const settingsPda = deriveSquadsSettings(config, 1n);
    const vaultPda = deriveSquadsVault(config, settingsPda.address, 0);
    const policyPda = deriveSquadsPolicy(config, settingsPda.address, 1n);
    const programConfig = deriveSquadsProgramConfig(config);

    expect(settingsPda.address.toBase58()).toBe("41gqrPgijYycTaCCzKyLfvqikMEH9fzGCwZYAKQHvMbd");
    expect(vaultPda.address.toBase58()).toBe("EdMSvMoHfsemd2s7eHCrRnuM1dzPu2CpUrHJN98XYC9y");
    expect(policyPda.address.toBase58()).toBe("622E3Qc1AU49sDhtPaYmSZeBN3dwBKGU8RmnvCp2rR3i");
    expect(programConfig.toBase58()).toBe("GmY9kVi3FhrCUn2MJkzzpE6C5618YoHuGsgqHU78cKus");
  });

  test("builds a live Squads smart-account creation instruction with caller-supplied treasury", () => {
    const payer = new PublicKey("11111111111111111111111111111116");
    const verifier = new PublicKey("11111111111111111111111111111117");
    const treasury = new PublicKey("11111111111111111111111111111118");
    const instruction = createSquadsSmartAccountInstruction(config, {
      payer,
      verifier,
      seed: 1n,
      treasury,
    });
    const settingsPda = deriveSquadsSettings(config, 1n);

    expect(instruction.programId.toBase58()).toBe(config.squadsSmartAccountProgramId.toBase58());
    expect(instruction.keys.map((key) => [key.pubkey.toBase58(), key.isSigner, key.isWritable])).toEqual([
      [deriveSquadsProgramConfig(config).toBase58(), false, true],
      [treasury.toBase58(), false, true],
      [payer.toBase58(), true, true],
      [SystemProgram.programId.toBase58(), false, false],
      [config.squadsSmartAccountProgramId.toBase58(), false, false],
      [settingsPda.address.toBase58(), false, true],
    ]);
    expect(instruction.data.subarray(0, 8).toJSON().data).toEqual([197, 102, 253, 231, 77, 84, 50, 17]);
    expect(instruction.data.subarray(8, 13).toJSON().data).toEqual([0, 1, 0, 1, 0]);
  });

  test("compiles arbitrary inner instructions for Squads sync execution", () => {
    const program = new PublicKey("11111111111111111111111111111119");
    const writable = new PublicKey("1111111111111111111111111111111A");
    const signerAccount = new PublicKey("1111111111111111111111111111111B");
    const instruction = new TransactionInstruction({
      programId: program,
      keys: [
        { pubkey: writable, isSigner: false, isWritable: true },
        { pubkey: signerAccount, isSigner: true, isWritable: false },
      ],
      data: Buffer.from([1, 2, 3]),
    });

    const compiled = compileSquadsTransactionInstructions([instruction]);
    expect(compiled.transactionAccounts.map((account) => [account.pubkey.toBase58(), account.isSigner, account.isWritable])).toEqual([
      [writable.toBase58(), false, true],
      [signerAccount.toBase58(), true, false],
      [program.toBase58(), false, false],
    ]);
    expect(compiled.compiledInstructions).toEqual([
      {
        programIdIndex: 2,
        accounts: [0, 1],
        data: new Uint8Array([1, 2, 3]),
      },
    ]);
  });

  test("builds Squads sync and ProgramInteraction execution wrappers", () => {
    const program = new PublicKey("11111111111111111111111111111119");
    const signerAccount = new PublicKey("1111111111111111111111111111111B");
    const inner = new TransactionInstruction({
      programId: program,
      keys: [{ pubkey: signerAccount, isSigner: true, isWritable: false }],
      data: Buffer.from([9]),
    });

    const sync = createSquadsSyncTransactionInstruction(config, {
      settings,
      signer: delegatedSigner,
      accountIndex: 0,
      instructions: [inner],
    });
    const policyExecution = createSquadsProgramInteractionExecutionInstruction(config, {
      policy: deriveSquadsPolicy(config, settings, 1n).address,
      signer: delegatedSigner,
      accountIndex: 0,
      instructions: [inner],
      instructionConstraintIndexes: [0, 2, 3],
    });

    expect(sync.keys.slice(0, 3).map((key) => [key.pubkey.toBase58(), key.isSigner, key.isWritable])).toEqual([
      [settings.toBase58(), false, true],
      [config.squadsSmartAccountProgramId.toBase58(), false, false],
      [delegatedSigner.toBase58(), true, false],
    ]);
    expect(sync.data.subarray(0, 11).toJSON().data).toEqual([90, 81, 187, 81, 39, 70, 128, 78, 0, 1, 0]);
    expect(policyExecution.keys.slice(0, 3).map((key) => [key.pubkey.toBase58(), key.isSigner, key.isWritable])).toEqual([
      [deriveSquadsPolicy(config, settings, 1n).address.toBase58(), false, true],
      [config.squadsSmartAccountProgramId.toBase58(), false, false],
      [delegatedSigner.toBase58(), true, false],
    ]);
    expect(policyExecution.data.subarray(0, 12).toJSON().data).toEqual([90, 81, 187, 81, 39, 70, 128, 78, 0, 1, 1, 1]);
  });

  test("builds a compact ProgramInteraction policy update settings action", () => {
    const innerConstraint = {
      programId: delegatedSigner,
      accountConstraints: [],
      dataConstraints: [],
    };
    const policyUpdate = createProgramInteractionPolicyUpdateInstruction(
      config,
      {
        settings,
        authority,
        delegatedSigner,
        accountIndex: 0,
      },
      deriveSquadsPolicy(config, settings, TREASURY_REBALANCE_ACTION_SEED).address,
      [innerConstraint],
    );

    expect(policyUpdate.keys.map((key) => [key.pubkey.toBase58(), key.isSigner, key.isWritable])).toEqual([
      [settings.toBase58(), false, true],
      [authority.toBase58(), true, true],
      ["11111111111111111111111111111111", false, false],
      [config.squadsSmartAccountProgramId.toBase58(), false, false],
      [authority.toBase58(), true, false],
      [deriveSquadsPolicy(config, settings, TREASURY_REBALANCE_ACTION_SEED).address.toBase58(), false, true],
    ]);
    expect(policyUpdate.data.subarray(0, 14).toJSON().data).toEqual([
      138, 209, 64, 163, 79, 67, 233, 76, 1, 1, 0, 0, 0, 8,
    ]);
  });

  test("builds compact ProgramInteraction policy updates with an hourly spending limit", () => {
    const innerConstraint = {
      programId: delegatedSigner,
      accountConstraints: [],
      dataConstraints: [],
    };
    const policyUpdate = createProgramInteractionPolicyUpdateInstruction(
      config,
      {
        settings,
        authority,
        delegatedSigner,
        accountIndex: 0,
        spendingLimits: [
          {
            mint: spendingLimitMint,
            timeConstraints: {
              start: 0n,
              expiration: null,
              period: { type: "custom", seconds: 3600n },
            },
            quantityConstraints: {
              maxPerPeriod: 1_000_000n,
            },
          },
        ],
      },
      deriveSquadsPolicy(config, settings, TREASURY_REBALANCE_ACTION_SEED).address,
      [innerConstraint],
    );

    const bytes = Array.from(policyUpdate.data);
    const spendingLimitBytes = [
      1,
      1,
      ...i64Le(0n),
      0,
      4,
      ...i64Le(3600n),
      ...i64Le(1_000_000n),
    ];
    const spendingLimitOffset = findBytes(bytes, spendingLimitBytes);

    expect(spendingLimitOffset).toBeGreaterThan(1);
    expect(bytes.slice(spendingLimitOffset - 2, spendingLimitOffset)).toEqual([0, 0]);
  });

  test("rejects planned rebalances that touch active lanes", () => {
    expect(() =>
      assertRebalanceAvoidsActiveLanes([1, 3], [
        {
          fromLaneId: 0,
          toLaneId: 3,
        },
      ]),
    ).toThrow("active lane");

    expect(() =>
      assertRebalanceAvoidsActiveLanes([1, 3], [
        {
          fromLaneId: 0,
          toLaneId: 2,
        },
      ]),
    ).not.toThrow();
  });
});
