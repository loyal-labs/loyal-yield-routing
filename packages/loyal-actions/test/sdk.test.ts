import { describe, expect, test } from "bun:test";
import { PublicKey, SystemProgram } from "@solana/web3.js";
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
  LoyalCluster,
  MaxFeeBps,
  RISK_BASKET_MARKETS,
  RiskBasket,
  STABLECOIN_MINTS,
  Stablecoin,
  SwapLane,
  createLoyalActionsSdk,
} from "../src/index.js";
import {
  deriveKaminoUserMetadata,
  deriveKaminoVanillaObligation,
  kaminoInitObligationConstraint,
} from "../src/internal/protocols.js";
import {
  KAMINO_INIT_OBLIGATION_DISCRIMINATOR,
  KAMINO_VANILLA_OBLIGATION_ID,
  KAMINO_VANILLA_OBLIGATION_TAG,
} from "../src/constants.js";

const settings = new PublicKey("11111111111111111111111111111112");
const authority = new PublicKey("11111111111111111111111111111113");
const delegatedSigner = new PublicKey("11111111111111111111111111111114");
const vault = new PublicKey("11111111111111111111111111111115");

const squads = {
  settings,
  authority,
  delegatedSigner,
  accountIndex: 0,
  vault,
};

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
