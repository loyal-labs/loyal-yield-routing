import assert from "node:assert/strict";
import { readFileSync } from "node:fs";

import { test } from "bun:test";

import {
  BASIC_POLICY_ARTIFACT,
  PACKET_LIMIT,
  POLICY_SEEDS,
  assertExecuteAuthorization,
  createPolicyInstruction,
  derivePolicyAddress,
  parseArtifact,
  validateInstallJournal,
} from "./rwa-basic-policy-set.js";

const artifact = parseArtifact(JSON.parse(readFileSync(BASIC_POLICY_ARTIFACT, "utf8")) as unknown);

test("parses the four-policy artifact and preserves stable fingerprints", () => {
  assert.deepEqual(artifact.policies.map((policy) => policy.seed), [...POLICY_SEEDS]);
  assert.deepEqual(artifact.policies.map((policy) => policy.account), [
    "2Wn69xc4ntC2aTjQNi4nnfmTCAqHngWYVfLSyeRbkkKh",
    "BmWgjEMgfYpfAYJCUSUkBmQqRKVxodjioob1gtc8ekuA",
    "9Z9cCwWbh6ygM6zrw5peG6VABYtNufjkwqitE9pdd3aA",
    "Z9jqB9pWDf1L1yFKVzXU1XnX8eKLndFP37FUwZMfWyz",
  ]);
  assert.deepEqual(artifact.policies.map((policy) => policy.legacyPacketBytes), [1136, 1136, 818, 818]);
  assert.deepEqual(artifact.policies.map((policy) => policy.dataSha256), [
    "bd75abb8acb28180e2fbcd66814e21d32267a447cf3dac50c928d2d2cc0dcc83",
    "d49cd14712056de39fb06c6fea3120dc851bf0f5b0bfa0d87413697457b30479",
    "e56888dd2b3ac821c16933e59ae31ec3f6fc38a0770dd79905933dde9885fe22",
    "44e8beecf05e6032df92878685d10521c30fb3a4d3df28c561fd45d6e92fcab7",
  ]);
});

test("derives every policy PDA from the settings and consecutive seed", () => {
  assert.deepEqual(artifact.policies.map((policy) => derivePolicyAddress(artifact.settings, policy.seed)), artifact.policies.map((policy) => policy.account));
});

test("builds legacy PolicyCreate instructions within the packet limit", () => {
  for (const policy of artifact.policies) {
    const instruction = createPolicyInstruction(policy);
    assert.equal(instruction.programId.toBase58(), "SMRTzfY6DfH5ik3TKiyLFfXexV8uSG3d2UksSCYdunG");
    assert.equal(instruction.data.length > 0, true);
    assert.equal(policy.legacyPacketBytes <= PACKET_LIMIT, true);
    assert.equal(instruction.keys.length, 6);
    assert.equal(instruction.keys.filter((key) => key.isSigner).length, 2);
  }
});

test("fails closed on execute authorization and validates the pre-send journal shape", () => {
  assert.throws(() => assertExecuteAuthorization({}), /CONFIRM_MAINNET/);
  assert.throws(() => assertExecuteAuthorization({ CONFIRM_MAINNET: "1" }), /SOLANA_TESTING_PK/);
  assert.doesNotThrow(() => assertExecuteAuthorization({ CONFIRM_MAINNET: "1", SOLANA_TESTING_PK: "test-keypair" }));

  const journal = {
    schema: "loyal-backyard-rwa-basic-policy-install-journal/v1",
    broadcast: true,
    legs: [{
      seed: "141",
      account: artifact.policies[0]!.account,
      state: "planned",
      wireSha256: "a".repeat(64),
      blockhash: "test-blockhash",
      lastValidBlockHeight: 123,
      packetBytes: 1136,
      signature: "test-signature",
      preSendSimulation: { contextSlot: 456, unitsConsumed: 789, err: null },
    }],
  };
  assert.doesNotThrow(() => validateInstallJournal(journal));
  assert.throws(() => validateInstallJournal({ ...journal, legs: [{ ...journal.legs[0], seed: "143" }] }), /next policy seed/);
});
