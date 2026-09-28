import assert from "node:assert/strict";
import { test } from "node:test";

import { RWA_MULTIPLY_ROUTE } from "../domain/rwa-multiply-route-spec.js";
import {
  buildLegacyCustomPolicyRetirementInstruction,
  customPolicyAddress,
  deriveStrategyTwoReplacementSeeds,
  assertStrategyTwoInstallMayCoexist,
  assertStrategyTwoReplacementPoliciesFinalized,
  LEGACY_CUSTOM_POLICY_ADDRESSES,
  LEGACY_CUSTOM_POLICY_DATA_SHA256,
  LEGACY_CUSTOM_POLICY_SEEDS,
} from "./rwa-multiply-legacy-retirement.js";

test("legacy retirement removes the v2 bridge policies at seeds 62-65", () => {
  const instruction = buildLegacyCustomPolicyRetirementInstruction();
  assert.equal(instruction.programAddress, RWA_MULTIPLY_ROUTE.squads.program);
  assert.deepEqual(LEGACY_CUSTOM_POLICY_SEEDS, [62n, 63n, 64n, 65n]);
  assert.deepEqual(LEGACY_CUSTOM_POLICY_ADDRESSES, LEGACY_CUSTOM_POLICY_SEEDS.map(customPolicyAddress));
  assert.equal(LEGACY_CUSTOM_POLICY_DATA_SHA256.length, 4);
  assert.ok(LEGACY_CUSTOM_POLICY_DATA_SHA256.every((value) => /^[0-9a-f]{64}$/.test(value)));
  // The retirement wire carries one Settings action per legacy policy.
  assert.deepEqual(instruction.accounts?.slice(5).map(({ address }) => address),
    LEGACY_CUSTOM_POLICY_ADDRESSES);
  assert.equal(instruction.accounts?.length, 9);
  assert.ok((instruction.data?.length ?? 0) > 8);
});

test("legacy data hashes are frozen before the removal wire can be built", () => {
  const [allocation, navRefresh, stage, withdraw] = LEGACY_CUSTOM_POLICY_DATA_SHA256;
  assert.equal(allocation, "bda72932f474064fa3cd60ce91633acba35b2730e86b82f4352aa96a6738e2f4");
  assert.equal(navRefresh, "bf34a3e9c9c635c79a0d30e096b639a86d52e300ad113c81161e3486832d97ca");
  assert.equal(stage, "ef8c231497fb2620b5930cfe5d329c871f103db6512781eb5487534db8b1291b");
  assert.equal(withdraw, "84e8f6f881758cff1714ef743603c016024104f9834392c6fba693c3651b719c");
});

test("strategy-two replacement seeds land past every installed policy", () => {
  const seeds = deriveStrategyTwoReplacementSeeds(144n);
  assert.deepEqual(seeds, [145n, 146n, 147n, 148n]);
  const legacy = new Set(LEGACY_CUSTOM_POLICY_ADDRESSES);
  for (const seed of seeds) {
    const policy = customPolicyAddress(seed);
    assert.ok(!legacy.has(policy), `replacement seed ${seed} collides with a legacy policy`);
    assert.ok(!LEGACY_CUSTOM_POLICY_SEEDS.includes(seed));
    // The installed basic set consumed seeds 141-144, so the replacements must
    // land strictly past it.
    assert.ok(seed > 144n, `replacement seed ${seed} does not clear the installed basic set`);
  }
});

test("strategy-two installation permits legacy 62-65 to coexist", () => {
  assert.doesNotThrow(() => assertStrategyTwoInstallMayCoexist([
    { address: LEGACY_CUSTOM_POLICY_ADDRESSES[0] },
    { address: LEGACY_CUSTOM_POLICY_ADDRESSES[1] },
    { address: LEGACY_CUSTOM_POLICY_ADDRESSES[2] },
    { address: LEGACY_CUSTOM_POLICY_ADDRESSES[3] },
  ]));
  assert.throws(() => assertStrategyTwoInstallMayCoexist([]), /coexistence read is incomplete/);
});

test("legacy retirement refuses without all four finalized replacements", () => {
  const expected = deriveStrategyTwoReplacementSeeds(144n);
  assert.throws(() => assertStrategyTwoReplacementPoliciesFinalized({ pass: false, rows: [] }, expected),
    /four finalized strategy-two replacement policies/);
  assert.doesNotThrow(() => assertStrategyTwoReplacementPoliciesFinalized({
    pass: true,
    rows: expected.map((seed) => ({ seed: seed.toString() })),
  }, expected));
});
