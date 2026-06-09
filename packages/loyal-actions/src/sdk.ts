import { PublicKey } from "@solana/web3.js";
import { DEFAULT_MAX_FEE_BPS, RISK_BASKET_MARKETS, STABLECOIN_MINTS, STABLECOINS } from "./constants.js";
import { clusterConfigFor } from "./cluster.js";
import { kaminoDepositConstraint, kaminoWithdrawConstraint, jupiterConstraint, loyalHubConstraint, uniquePubkeys } from "./internal/protocols.js";
import { createProgramInteractionPolicyInstruction, deriveActionAccount } from "./internal/squads.js";
import { LoyalCluster, MaxFeeBps, RiskBasket, Stablecoin, SwapLane } from "./types.js";
import type { InitYieldRoutePolicyInput, InitYieldRoutePolicyResult, LoyalActionsSdk, LoyalActionRoute3 } from "./types.js";

const VALID_MAX_FEE_BPS = new Set<number>([
  MaxFeeBps.Bps50,
  MaxFeeBps.Bps75,
  MaxFeeBps.Bps100,
  MaxFeeBps.Bps125,
  MaxFeeBps.Bps150,
]);

export function createLoyalActionsSdk(config: { cluster: LoyalCluster }): LoyalActionsSdk {
  if (!Object.values(LoyalCluster).includes(config.cluster)) {
    throw new Error(`unsupported Loyal cluster: ${String(config.cluster)}`);
  }
  const clusterConfig = clusterConfigFor(config.cluster);

  return {
    initYieldRoutePolicy<const Lanes extends readonly SwapLane[]>(
      input: InitYieldRoutePolicyInput<Lanes>,
    ): InitYieldRoutePolicyResult<Lanes> {
      validateInput(input);

      const maxFeeBps = input.maxFeeBps ?? DEFAULT_MAX_FEE_BPS;
      const stablecoins = input.stablecoins ?? STABLECOINS;
      const stableMints = stablecoins.map((stablecoin) => STABLECOIN_MINTS[stablecoin]);
      const kaminoMarkets = [...RISK_BASKET_MARKETS[input.risk]];
      const kaminoLiquidityMints = [...stableMints];
      const actionAccount = deriveActionAccount(clusterConfig, input.squads.settings);
      const constraints = [
        kaminoWithdrawConstraint(clusterConfig, input.squads.vault, kaminoMarkets, kaminoLiquidityMints),
        ...input.swapLanes.map((lane) =>
          lane === SwapLane.Jupiter
            ? jupiterConstraint(clusterConfig, input.squads.vault, stableMints, maxFeeBps)
            : loyalHubConstraint(clusterConfig, input.squads.vault, stableMints, maxFeeBps),
        ),
        kaminoDepositConstraint(clusterConfig, input.squads.vault, kaminoMarkets, kaminoLiquidityMints),
      ];

      const instruction = createProgramInteractionPolicyInstruction(clusterConfig, input.squads, constraints);
      const depositIndex = 1 + input.swapLanes.length;
      const routes: Record<string, unknown> = {
        sameMint: {
          actionAccount,
          instructionConstraintIndexes: [0, depositIndex] as const,
        },
      };

      for (const [offset, lane] of input.swapLanes.entries()) {
        const route: LoyalActionRoute3 = {
          actionAccount,
          instructionConstraintIndexes: [0, offset + 1, depositIndex] as const,
        };
        if (lane === SwapLane.Jupiter) {
          routes.jupiter = route;
        } else {
          routes.loyal = route;
        }
      }

      return {
        instructions: [instruction],
        actionAccount,
        routes: routes as InitYieldRoutePolicyResult<Lanes>["routes"],
        spec: {
          risk: input.risk,
          stablecoins: [...stablecoins],
          stableMints,
          kaminoMarkets,
          kaminoLiquidityMints,
          swapLanes: [...input.swapLanes],
          maxFeeBps,
        },
      };
    },
  };
}

function validateInput(input: InitYieldRoutePolicyInput): void {
  if (!Object.values(RiskBasket).includes(input.risk)) {
    throw new Error(`unsupported risk basket: ${String(input.risk)}`);
  }
  if (input.stablecoins !== undefined) {
    if (!Array.isArray(input.stablecoins) || input.stablecoins.length === 0) {
      throw new Error("at least one stablecoin is required");
    }
    const seenStablecoins = new Set<Stablecoin>();
    for (const stablecoin of input.stablecoins) {
      if (!Object.values(Stablecoin).includes(stablecoin)) {
        throw new Error(`unsupported stablecoin: ${String(stablecoin)}`);
      }
      if (seenStablecoins.has(stablecoin)) {
        throw new Error(`duplicate stablecoin: ${stablecoin}`);
      }
      seenStablecoins.add(stablecoin);
    }
  }
  if (!Array.isArray(input.swapLanes) || input.swapLanes.length === 0) {
    throw new Error("at least one swap lane is required");
  }
  const seen = new Set<SwapLane>();
  for (const lane of input.swapLanes) {
    if (!Object.values(SwapLane).includes(lane)) {
      throw new Error(`unsupported swap lane: ${String(lane)}`);
    }
    if (seen.has(lane)) {
      throw new Error(`duplicate swap lane: ${lane}`);
    }
    seen.add(lane);
  }
  const maxFeeBps = input.maxFeeBps ?? DEFAULT_MAX_FEE_BPS;
  if (!VALID_MAX_FEE_BPS.has(maxFeeBps)) {
    throw new Error(`unsupported maxFeeBps: ${String(maxFeeBps)}`);
  }
  if (!Number.isInteger(input.squads.accountIndex) || input.squads.accountIndex < 0 || input.squads.accountIndex > 255) {
    throw new Error("squads.accountIndex must be a u8");
  }
  for (const [name, value] of Object.entries(input.squads)) {
    if (name === "accountIndex") {
      continue;
    }
    if (!(value instanceof PublicKey)) {
      throw new Error(`squads.${name} must be a PublicKey`);
    }
  }
  const stablecoins: readonly Stablecoin[] = input.stablecoins ?? STABLECOINS;
  const stableMints = stablecoins.map((stablecoin) => STABLECOIN_MINTS[stablecoin]);
  if (uniquePubkeys(stableMints).length !== stableMints.length) {
    throw new Error("stablecoin mint registry contains duplicates");
  }
}
