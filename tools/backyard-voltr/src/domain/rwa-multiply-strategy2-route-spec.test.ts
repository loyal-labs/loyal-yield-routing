import assert from "node:assert/strict";
import { test } from "node:test";

import { Keypair } from "@solana/web3.js";

import { V2_CUSTOM_POLICY_TARGET } from "./custom-policy-target.js";
import { RWA_MULTIPLY_ROUTE } from "./rwa-multiply-route-spec.js";
import {
  deriveStrategyTwoVoltrAccounts,
  rwaMultiplyStrategyTwoRoute,
  rwaMultiplyStrategyTwoTarget,
  deriveStrategyTwoPolicySeeds,
  STRATEGY_TWO_DERIVATION_DOMAIN,
  STRATEGY_TWO_OPERATIONAL_AMOUNT_CAP_RAW,
  STRATEGY_TWO_REPORT_NAV_CAP_RAW,
  STRATEGY_TWO_DAILY_SPENDING_LIMIT_RAW,
  type StrategyTwoIdentity,
} from "./rwa-multiply-strategy2-route-spec.js";

function throwawayIdentity(): StrategyTwoIdentity {
  return {
    config: Keypair.generate().publicKey.toBase58() as StrategyTwoIdentity["config"],
    delegatedSigner: Keypair.generate().publicKey.toBase58() as StrategyTwoIdentity["delegatedSigner"],
  };
}

test("strategy-two seeds derive from the finalized Settings counter", () => {
  assert.deepEqual(deriveStrategyTwoPolicySeeds(144n), {
    allocation: 145n, navRefresh: 146n, stageWithdrawal: 147n, withdraw: 148n,
  });
  assert.equal(STRATEGY_TWO_OPERATIONAL_AMOUNT_CAP_RAW, 100_000_000n);
  assert.equal(STRATEGY_TWO_REPORT_NAV_CAP_RAW, RWA_MULTIPLY_ROUTE.vault.capRaw);
  assert.ok(STRATEGY_TWO_REPORT_NAV_CAP_RAW > STRATEGY_TWO_OPERATIONAL_AMOUNT_CAP_RAW);
  assert.notEqual(STRATEGY_TWO_DERIVATION_DOMAIN, RWA_MULTIPLY_ROUTE.customAdaptor.strategyDerivationDomain);
});

test("strategy-two route swaps only the rotated identities", () => {
  const identity = throwawayIdentity();
  const route = rwaMultiplyStrategyTwoRoute(identity);
  assert.equal(route.squads.delegatedExecutor, identity.delegatedSigner);
  assert.equal(route.customAdaptor.strategyConfig, identity.config);
  assert.equal(route.customAdaptor.maxReportedNavRaw, STRATEGY_TWO_REPORT_NAV_CAP_RAW);
  assert.equal(route.customAdaptor.strategyDerivationDomain, STRATEGY_TWO_DERIVATION_DOMAIN);
  assert.equal(route.vault.address, RWA_MULTIPLY_ROUTE.vault.address);
  assert.equal(route.squads.settings, RWA_MULTIPLY_ROUTE.squads.settings);
  assert.equal(route.programs.voltr, RWA_MULTIPLY_ROUTE.programs.voltr);
  assert.equal(route.customAdaptor.program, RWA_MULTIPLY_ROUTE.customAdaptor.program);
  assert.equal(route.assets.assetMint, RWA_MULTIPLY_ROUTE.assets.assetMint);
  assert.throws(() => rwaMultiplyStrategyTwoRoute({
    ...identity,
    config: RWA_MULTIPLY_ROUTE.customAdaptor.strategyConfig,
  }), /must not reuse the v2 adaptor config/);
  // One hot key this week by owner decision: the v2 executor is an accepted
  // strategy-two delegated signer; only the adaptor config must be fresh.
  assert.doesNotThrow(() => rwaMultiplyStrategyTwoRoute({
    ...identity,
    delegatedSigner: RWA_MULTIPLY_ROUTE.squads.delegatedExecutor,
  }));
});

test("strategy-two target moves the policy and Voltr surface off v2", async () => {
  const identity = throwawayIdentity();
  const target = await rwaMultiplyStrategyTwoTarget(identity, 144n);
  assert.equal(target.route.squads.delegatedExecutor, identity.delegatedSigner);
  assert.deepEqual(target.seeds, deriveStrategyTwoPolicySeeds(144n));
  assert.deepEqual(target.caps, {
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
  });
  // The installed v2 set stays the uncapped regression baseline.
  assert.deepEqual(V2_CUSTOM_POLICY_TARGET.seeds, RWA_MULTIPLY_ROUTE.squads.customPolicySeeds);
  assert.equal(V2_CUSTOM_POLICY_TARGET.caps.amountRaw, RWA_MULTIPLY_ROUTE.vault.capRaw);
  assert.equal(V2_CUSTOM_POLICY_TARGET.caps.reportNavRaw, null);
  assert.equal(V2_CUSTOM_POLICY_TARGET.caps.dailySpendingLimit, null);

  const accounts = await deriveStrategyTwoVoltrAccounts(identity.config);
  const v2 = await deriveStrategyTwoVoltrAccounts(RWA_MULTIPLY_ROUTE.customAdaptor.strategyConfig);
  assert.deepEqual(Object.keys(accounts), Object.keys(v2));
  const derived = Object.values(accounts);
  assert.equal(new Set(derived).size, 4);
  for (const [key, value] of Object.entries(accounts)) {
    assert.notEqual(value, v2[key as keyof typeof v2], `${key} must move with the config rotation`);
  }
});
