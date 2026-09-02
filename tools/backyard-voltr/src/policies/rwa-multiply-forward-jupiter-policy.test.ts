import { strict as assert } from "node:assert";
import { test } from "node:test";

import { RWA_MULTIPLY_ROUTE } from "../domain/rwa-multiply-route-spec.js";
import { PHASE_ONE_FORWARD_ROUTE_PREFIX_HEX } from "./rwa-multiply-jupiter-headers.js";
import {
  FORWARD_JUPITER_AMOUNT_OFFSET,
  FORWARD_JUPITER_DATA_LENGTH,
  FORWARD_JUPITER_FEE_OFFSET,
  FORWARD_JUPITER_OUT_AMOUNT_OFFSET,
  FORWARD_JUPITER_POLICY_SEED,
  FORWARD_JUPITER_POLICY_SEED_BEFORE,
  FORWARD_JUPITER_SLIPPAGE_OFFSET,
  forwardJupiterConstraints,
  forwardJupiterPolicyAddress,
} from "./rwa-multiply-forward-jupiter-policy.js";

test("forward Jupiter rollover is one seed with exactly two legacy len37 prefixes", () => {
  assert.equal(FORWARD_JUPITER_POLICY_SEED_BEFORE, 65n);
  assert.equal(FORWARD_JUPITER_POLICY_SEED, 66n);
  assert.equal(FORWARD_JUPITER_DATA_LENGTH, 37);
  assert.equal(FORWARD_JUPITER_AMOUNT_OFFSET, 18);
  assert.equal(FORWARD_JUPITER_OUT_AMOUNT_OFFSET, 26);
  assert.equal(FORWARD_JUPITER_SLIPPAGE_OFFSET, 34);
  assert.equal(FORWARD_JUPITER_FEE_OFFSET, 36);
  assert.equal(forwardJupiterPolicyAddress(), "FZjjJScy689WWSwhwr2HZPy2aevZukq75niD6gW3b1TG");
  const expected = (routePlanPrefixHex: string) => ({
    programId: RWA_MULTIPLY_ROUTE.programs.jupiter,
    accountPubkeys: [
      { index: 0, pubkeys: [RWA_MULTIPLY_ROUTE.assets.tokenProgram] },
      { index: 2, pubkeys: [RWA_MULTIPLY_ROUTE.squads.vault] },
      { index: 3, pubkeys: [RWA_MULTIPLY_ROUTE.squads.assetAta] },
      { index: 6, pubkeys: [RWA_MULTIPLY_ROUTE.squads.collateralAta] },
      { index: 7, pubkeys: [RWA_MULTIPLY_ROUTE.assets.assetMint] },
      { index: 8, pubkeys: [RWA_MULTIPLY_ROUTE.assets.collateralMint] },
    ],
    data: [
      { kind: "slice-equals", offset: 0, valueHex: "c1209b3341d69c81" },
      { kind: "slice-equals", offset: 8, valueHex: routePlanPrefixHex },
      { kind: "u64-less-than-or-equal", offset: 18,
        value: Number(RWA_MULTIPLY_ROUTE.vault.capRaw) },
      { kind: "u16-less-than-or-equal", offset: 34,
        value: RWA_MULTIPLY_ROUTE.assets.maxSlippageBps },
      { kind: "u8-equals", offset: 36, value: 0 },
    ],
  });
  assert.deepEqual(forwardJupiterConstraints(), [
    expected("01010000007400640001"),
    expected("02010000007400640001"),
  ]);
  assert.deepEqual(PHASE_ONE_FORWARD_ROUTE_PREFIX_HEX, [
    "01010000007400640001", "02010000007400640001",
  ]);
});
