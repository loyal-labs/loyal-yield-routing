import { PublicKey } from "@solana/web3.js";
import { LoyalCluster, RiskBasket, Stablecoin, SwapLane, createLoyalActionsSdk } from "../src/index.js";

const sdk = createLoyalActionsSdk({ cluster: LoyalCluster.MainnetBeta });
const key = new PublicKey("11111111111111111111111111111112");

const policy = sdk.initYieldRoutePolicy({
  risk: RiskBasket.Safe,
  swapLanes: [SwapLane.Jupiter] as const,
  squads: {
    settings: key,
    authority: key,
    delegatedSigner: key,
    accountIndex: 0,
    vault: key,
  },
});

const jupiterIndexes = policy.routes.jupiter.instructionConstraintIndexes;
void jupiterIndexes;

// @ts-expect-error Loyal route metadata is absent when the Loyal lane is not enabled.
const loyalIndexes = policy.routes.loyal.instructionConstraintIndexes;
void loyalIndexes;

sdk.initYieldRoutePolicy({
  risk: RiskBasket.Safe,
  stablecoins: [Stablecoin.USDC, Stablecoin.PYUSD],
  swapLanes: [SwapLane.Jupiter] as const,
  squads: {
    settings: key,
    authority: key,
    delegatedSigner: key,
    accountIndex: 0,
    vault: key,
  },
});

sdk.initYieldRoutePolicy({
  risk: RiskBasket.Safe,
  swapLanes: [SwapLane.Jupiter] as const,
  squads: {
    settings: key,
    authority: key,
    delegatedSigner: key,
    accountIndex: 0,
    vault: key,
  },
  // @ts-expect-error Kamino markets are derived from RiskBasket.
  kaminoMarkets: [],
});
