import assert from "node:assert/strict";
import { test } from "node:test";

import { RWA_MULTIPLY_ROUTE } from "../domain/rwa-multiply-route-spec.js";
import {
  buildLegacyCustomPolicyRetirementInstruction,
  LEGACY_CUSTOM_POLICY_ADDRESSES,
  LEGACY_CUSTOM_POLICY_DATA_SHA256,
  LEGACY_CUSTOM_POLICY_SEEDS,
  REPLACEMENT_CUSTOM_POLICY_IDENTITIES,
} from "./rwa-multiply-legacy-retirement.js";

test("legacy retirement is one exact four-policy Settings action", () => {
  const instruction = buildLegacyCustomPolicyRetirementInstruction();
  assert.equal(instruction.programAddress, RWA_MULTIPLY_ROUTE.squads.program);
  assert.deepEqual(LEGACY_CUSTOM_POLICY_SEEDS, [53n, 54n, 55n, 56n]);
  assert.equal(LEGACY_CUSTOM_POLICY_DATA_SHA256.length, 4);
  assert.ok(LEGACY_CUSTOM_POLICY_DATA_SHA256.every((value) => /^[0-9a-f]{64}$/.test(value)));
  assert.deepEqual(REPLACEMENT_CUSTOM_POLICY_IDENTITIES.map(({ seed }) => seed), ["62", "63", "64", "65"]);
  assert.ok(REPLACEMENT_CUSTOM_POLICY_IDENTITIES.every(({ policy, dataSha256 }) =>
    policy.length >= 32 && /^[0-9a-f]{64}$/.test(dataSha256)));
  assert.deepEqual(instruction.accounts?.slice(5).map(({ address }) => address),
    LEGACY_CUSTOM_POLICY_ADDRESSES);
  assert.equal(instruction.accounts?.length, 9);
  assert.ok((instruction.data?.length ?? 0) > 8);
});
