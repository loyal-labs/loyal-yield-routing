import { address, type Address } from "@solana/kit";
import { findAssociatedTokenPda } from "@solana-program/token";
import { PublicKey } from "@solana/web3.js";
import {
  findStrategyInitReceiptPda,
  findVaultStrategyAuthPda,
} from "@voltr/vault-sdk";

import type { CustomPolicySeeds, CustomPolicyTarget } from "./custom-policy-target.js";
import { RWA_MULTIPLY_ROUTE, type RwaMultiplyRouteSpec } from "./rwa-multiply-route-spec.js";

/**
 * Strategy two runs on the same vault, custom adaptor, and Squads Settings as
 * v2, but with a fresh adaptor config keypair (its key IS the Voltr strategy
 * key), a delegated signer (the v2 executor is permitted by owner decision for
 * the single-hot-key week), and policies installed at fresh seeds. The
 * config keypair itself never lives in this repository: it is derived offline
 * by the operator from the setup admin over the derivation domain below (see
 * `tools/backyard-voltr/src/integrations/signer.ts`), so every address here is
 * a function of the two identities supplied at run time.
 */
export const STRATEGY_TWO_DERIVATION_DOMAIN = "loyal-rwa-multiply-mainnet-v3";

/**
 * Derive the four fresh policy seeds from the finalized Squads Settings
 * counter. PolicyCreate consumes the next counter value and does not honor a
 * caller-requested seed, so the compiler and installer must share this rule.
 */
export function deriveStrategyTwoPolicySeeds(policySeedBefore: bigint): CustomPolicySeeds {
  return {
    allocation: policySeedBefore + 1n,
    navRefresh: policySeedBefore + 2n,
    stageWithdrawal: policySeedBefore + 3n,
    withdraw: policySeedBefore + 4n,
  };
}

/**
 * Operational per-execution amount bound (100 USDC) replacing the v2 policy
 * cap of the whole vault. Raising it is a seed rollover, never an edit.
 */
export const STRATEGY_TWO_OPERATIONAL_AMOUNT_CAP_RAW = 100_000_000n;

/**
 * Daily USDC spending limit carried by every strategy-two policy: three
 * operational caps per 1-day period. This — not the per-instruction cap — is
 * what bounds a compromised delegate, because one delegated execution can
 * otherwise pack any number of transfers that each satisfy the per-instruction
 * cap. Squads charges the limit only on balance decreases, so it is inert on
 * the allocation/NAV-refresh lanes (the adaptor moves custody INTO the Squads
 * ATA there) and binding on every outflow lane.
 */
export const STRATEGY_TWO_DAILY_SPENDING_LIMIT_RAW = 300_000_000n;

/** Reported-NAV ceiling: the vault maxCap. Closes audit finding U3 on chain. */
export const STRATEGY_TWO_REPORT_NAV_CAP_RAW = RWA_MULTIPLY_ROUTE.vault.capRaw;

export type StrategyTwoIdentity = Readonly<{
  /** Adaptor config keypair address; the Voltr strategy key for strategy two. */
  config: Address;
  /** Delegated executor that signs policy-constrained Squads executions. */
  delegatedSigner: Address;
}>;

export type StrategyTwoVoltrAccounts = Readonly<{
  strategyInitReceipt: Address;
  strategyAuth: Address;
  strategyAssetAta: Address;
  reportTicket: Address;
}>;

export async function deriveStrategyTwoVoltrAccounts(
  config: Address,
): Promise<StrategyTwoVoltrAccounts> {
  const route = RWA_MULTIPLY_ROUTE;
  const [strategyInitReceipt] = await findStrategyInitReceiptPda(
    { vault: route.vault.address, strategy: config },
    { programAddress: route.programs.voltr },
  );
  const [strategyAuth] = await findVaultStrategyAuthPda(
    { vault: route.vault.address, strategy: config },
    { programAddress: route.programs.voltr },
  );
  const [strategyAssetAta] = await findAssociatedTokenPda({
    owner: strategyAuth,
    mint: route.assets.assetMint,
    tokenProgram: route.assets.tokenProgram,
  }, { programAddress: route.assets.associatedTokenProgram });
  const reportTicket = PublicKey.findProgramAddressSync([
    Buffer.from("report_ticket"),
    new PublicKey(config).toBuffer(),
  ], new PublicKey(route.customAdaptor.program))[0]!.toBase58();
  return {
    strategyInitReceipt: address(strategyInitReceipt),
    strategyAuth: address(strategyAuth),
    strategyAssetAta: address(strategyAssetAta),
    reportTicket: address(reportTicket),
  };
}

/**
 * The v2 route with strategy-two identities swapped in. Programs, mint,
 * token program, vault, Squads Settings, and the Squads vault are invariant;
 * the strategy config, delegated executor, and the adaptor's on-chain report
 * bound are the rotated values.
 */
export function rwaMultiplyStrategyTwoRoute(
  identity: StrategyTwoIdentity,
): RwaMultiplyRouteSpec {
  if (identity.config === RWA_MULTIPLY_ROUTE.customAdaptor.strategyConfig) {
    throw new Error("strategy-two config must not reuse the v2 adaptor config");
  }
  // Owner decision for this cutover week: one hot key, so the delegated
  // executor MAY equal the v2 executor. Only the adaptor config must be fresh.
  return {
    ...RWA_MULTIPLY_ROUTE,
    squads: {
      ...RWA_MULTIPLY_ROUTE.squads,
      delegatedExecutor: identity.delegatedSigner,
    },
    customAdaptor: {
      ...RWA_MULTIPLY_ROUTE.customAdaptor,
      strategyConfig: identity.config,
      strategyDerivationDomain: STRATEGY_TWO_DERIVATION_DOMAIN,
      maxReportedNavRaw: STRATEGY_TWO_REPORT_NAV_CAP_RAW,
    },
  };
}

export async function rwaMultiplyStrategyTwoTarget(
  identity: StrategyTwoIdentity,
  policySeedBefore: bigint,
): Promise<CustomPolicyTarget> {
  return {
    route: rwaMultiplyStrategyTwoRoute(identity),
    seeds: deriveStrategyTwoPolicySeeds(policySeedBefore),
    caps: {
      amountRaw: STRATEGY_TWO_OPERATIONAL_AMOUNT_CAP_RAW,
      reportNavRaw: STRATEGY_TWO_REPORT_NAV_CAP_RAW,
      dailySpendingLimit: {
        mint: RWA_MULTIPLY_ROUTE.assets.assetMint,
        maxPerPeriodRaw: STRATEGY_TWO_DAILY_SPENDING_LIMIT_RAW,
        start: 0n,
        expiration: null,
        period: "Daily",
        accumulateUnused: false,
      },
    },
  };
}
