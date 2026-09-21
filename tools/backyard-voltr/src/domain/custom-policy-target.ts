import { RWA_MULTIPLY_ROUTE, type RwaMultiplyRouteSpec } from "./rwa-multiply-route-spec.js";

/**
 * The ceilings a compiled custom Voltr policy set enforces on every
 * report-bearing wire. `amountRaw` bounds each movement amount; `reportNavRaw`
 * bounds the manager-supplied `nav_after_raw` (audit finding U3).
 * `dailySpendingLimit` is the daily Squads spending limit carried by EVERY
 * policy in the set: the per-instruction `amountRaw` cannot bound how many
 * instructions a single delegated execution packs, and Squads charges a limit
 * only against balance decreases, so it is inert on the deposit-side lanes and
 * binding on every lane that can reduce the Squads vault balance. `null` leaves
 * a field unconstrained and is only correct for the installed v2 set.
 */
export type CustomPolicySpendingLimit = Readonly<{
  mint: string;
  maxPerPeriodRaw: bigint;
  start: bigint;
  expiration: bigint | null;
  period: "Daily";
  accumulateUnused: boolean;
}>;

export type CustomPolicyCaps = Readonly<{
  amountRaw: bigint | null;
  reportNavRaw: bigint | null;
  dailySpendingLimit: CustomPolicySpendingLimit | null;
}>;

export type CustomPolicySeeds = Readonly<{
  allocation: bigint;
  navRefresh: bigint;
  stageWithdrawal: bigint;
  withdraw: bigint;
}>;

/**
 * One compilable/verifiable custom policy set. Every Voltr account the
 * constraints pin derives from `route` (the strategy config key IS the Voltr
 * strategy key), so a target is the route plus the two rotation choices.
 */
export type CustomPolicyTarget = Readonly<{
  route: RwaMultiplyRouteSpec;
  seeds: CustomPolicySeeds;
  caps: CustomPolicyCaps;
}>;

/** The installed v2 set: vault-wide amount cap, deliberately uncapped NAV. */
export const V2_CUSTOM_POLICY_TARGET: CustomPolicyTarget = {
  route: RWA_MULTIPLY_ROUTE,
  seeds: RWA_MULTIPLY_ROUTE.squads.customPolicySeeds,
  caps: {
    amountRaw: RWA_MULTIPLY_ROUTE.vault.capRaw,
    reportNavRaw: null,
    dailySpendingLimit: null,
  },
};
