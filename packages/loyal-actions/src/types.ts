import type { PublicKey, TransactionInstruction } from "@solana/web3.js";

export type Address = PublicKey;
export type IInstruction = TransactionInstruction;

export enum LoyalCluster {
  Devnet = "devnet",
  MainnetBeta = "mainnet-beta",
}

export enum RiskBasket {
  Safe = "safe",
  Medium = "medium",
  Aggressive = "aggressive",
}

export enum SwapLane {
  Jupiter = "jupiter",
  Loyal = "loyal",
}

export enum MaxFeeBps {
  Bps50 = 50,
  Bps75 = 75,
  Bps100 = 100,
  Bps125 = 125,
  Bps150 = 150,
}

export enum Stablecoin {
  USDC = "USDC",
  USDT = "USDT",
  PYUSD = "PYUSD",
  USDS = "USDS",
  USDG = "USDG",
  USDE = "USDE",
  SUSDE = "SUSDE",
}

export type LoyalActionRoute2 = {
  actionAccount: Address;
  instructionConstraintIndexes: readonly [number, number];
};

export type LoyalActionRoute3 = {
  actionAccount: Address;
  instructionConstraintIndexes: readonly [number, number, number];
};

export type InitYieldRoutePolicyInput<
  Lanes extends readonly SwapLane[] = readonly SwapLane[],
> = {
  risk: RiskBasket;
  stablecoins?: readonly Stablecoin[];
  swapLanes: Lanes;
  maxFeeBps?: MaxFeeBps;
  squads: {
    settings: Address;
    authority: Address;
    delegatedSigner: Address;
    accountIndex: number;
    vault: Address;
  };
};

type JupiterRouteFor<Lanes extends readonly SwapLane[]> =
  Extract<Lanes[number], SwapLane.Jupiter> extends never
    ? { jupiter?: undefined }
    : { jupiter: LoyalActionRoute3 };

type LoyalRouteFor<Lanes extends readonly SwapLane[]> =
  Extract<Lanes[number], SwapLane.Loyal> extends never
    ? { loyal?: undefined }
    : { loyal: LoyalActionRoute3 };

export type InitYieldRoutePolicyResult<
  Lanes extends readonly SwapLane[] = readonly SwapLane[],
> = {
  instructions: IInstruction[];
  actionAccount: Address;
  routes: {
    sameMint: LoyalActionRoute2;
  } & JupiterRouteFor<Lanes> &
    LoyalRouteFor<Lanes>;
  spec: {
    risk: RiskBasket;
    stablecoins: Stablecoin[];
    stableMints: Address[];
    kaminoMarkets: Address[];
    kaminoLiquidityMints: Address[];
    swapLanes: SwapLane[];
    maxFeeBps: MaxFeeBps;
  };
};

export type LoyalActionsSdk = {
  initYieldRoutePolicy<const Lanes extends readonly SwapLane[]>(
    input: InitYieldRoutePolicyInput<Lanes>,
  ): InitYieldRoutePolicyResult<Lanes>;
};
