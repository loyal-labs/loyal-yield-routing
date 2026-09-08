import assert from "node:assert/strict";
import { Buffer } from "node:buffer";
import { createHash } from "node:crypto";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { test } from "node:test";

import { Keypair, PublicKey } from "@solana/web3.js";

import {
  deriveStrategyTwoPolicySeeds,
  rwaMultiplyStrategyTwoTarget,
  STRATEGY_TWO_DAILY_SPENDING_LIMIT_RAW,
  type StrategyTwoIdentity,
} from "../domain/rwa-multiply-strategy2-route-spec.js";
import { RWA_MULTIPLY_ROUTE } from "../domain/rwa-multiply-route-spec.js";
import {
  assertStrategyTwoSeedJournalAnchor,
  assertStrategyTwoFirstInvocation,
  STRATEGY_TWO_FIRST_POLICY_SEED_BEFORE,
  STRATEGY_TWO_REPAIR_POLICY_ADDRESS,
  readStrategyTwoSeedExpectation,
  type StrategyTwoSeedExpectation,
  type StrategyTwoSeedAnchorObservation,
  writeStrategyTwoSeedExpectation,
} from "./rwa-multiply-strategy2-seed-journal.js";
import { buildStrategyTwoWorkerPolicyConfig } from "./rwa-multiply-strategy2-worker-config.js";
import { compileCustomPolicyArtifact, type CustomPolicyArtifact } from "./rwa-multiply-custom.js";
import { customPolicyAddress } from "./rwa-multiply-legacy-retirement.js";

function throwawayIdentity(): StrategyTwoIdentity {
  return {
    config: Keypair.generate().publicKey.toBase58() as StrategyTwoIdentity["config"],
    delegatedSigner: Keypair.generate().publicKey.toBase58() as StrategyTwoIdentity["delegatedSigner"],
  };
}

function createData(row: CustomPolicyArtifact["policies"][number]): Buffer {
  return Buffer.from(row.createInstruction.dataBase64, "base64");
}

function contains(haystack: Buffer, needle: Buffer): boolean {
  return haystack.indexOf(needle) >= 0;
}

function carriesBudget(data: Buffer, maxPerPeriodRaw: bigint): boolean {
  const budget = Buffer.alloc(8);
  budget.writeBigUInt64LE(maxPerPeriodRaw);
  return contains(data, budget);
}

/**
 * The installer and the verifier both derive their expectations from
 * `compileCustomPolicyArtifact(seedBefore, target)`, so this pins the exact
 * seam an operator install at seeds 141-144 relies on: a derived base seed of
 * 140 produces the exact four expected seeds in the compiler output. The test
 * uses a throwaway identity because the authoritative config keypair only
 * exists in the operator's derivation step and never in this repository.
 */
test("strategy-two install expectations equal the compiler output for a throwaway identity",
  { timeout: 120_000 },
  async () => {
  const policySeedBefore = STRATEGY_TWO_FIRST_POLICY_SEED_BEFORE;
  const expectedSeeds = deriveStrategyTwoPolicySeeds(policySeedBefore);
  const identity = throwawayIdentity();
  const target = await rwaMultiplyStrategyTwoTarget(identity, policySeedBefore);
  const artifact = await compileCustomPolicyArtifact(policySeedBefore, target);

  assert.deepEqual(artifact.policies.map(({ seed }) => seed),
    Object.values(expectedSeeds).map(String));
  assert.deepEqual(artifact.policies.map(({ policy }) => policy),
    Object.values(expectedSeeds).map((seed) => customPolicyAddress(seed)));

  // The daily USDC spending limit rides on EVERY policy (deposit-side lanes
  // admit the Squads USDC ATA writable too, and Squads charges the limit only
  // on decreases), so outflow per period is bounded regardless of which policy
  // executes or how many instructions one delegated execution packs.
  const mint = new PublicKey(RWA_MULTIPLY_ROUTE.assets.assetMint).toBuffer();
  for (const row of artifact.policies) {
    assert.ok(contains(createData(row), mint),
      `${row.operation} create data omits the spending-limit mint`);
    assert.ok(carriesBudget(createData(row), STRATEGY_TWO_DAILY_SPENDING_LIMIT_RAW),
      `${row.operation} create data does not carry the 300M daily budget`);
  }

  // The limit is genuinely parameterized end to end: removing it changes every
  // policy's bytes, so an operator can never install a set whose spending
  // limit silently differs from the route target.
  const withoutLimit = await compileCustomPolicyArtifact(policySeedBefore, {
    ...target,
    caps: { ...target.caps, dailySpendingLimit: null },
  });
  const sha = (row: CustomPolicyArtifact["policies"][number]) =>
    createHash("sha256").update(createData(row)).digest("hex");
  for (const [index, row] of artifact.policies.entries()) {
    assert.notEqual(sha(row), sha(withoutLimit.policies[index]!),
      `${row.operation} bytes did not respond to the spending limit input`);
  }
});

test("the first installer invocation is pinned to repair seed 140", () => {
  assert.equal(customPolicyAddress(STRATEGY_TWO_FIRST_POLICY_SEED_BEFORE), STRATEGY_TWO_REPAIR_POLICY_ADDRESS);
  assert.doesNotThrow(() => assertStrategyTwoFirstInvocation(
    STRATEGY_TWO_FIRST_POLICY_SEED_BEFORE, true));
  assert.throws(() => assertStrategyTwoFirstInvocation(139n, true),
    /requires finalized Settings counter 140/);
  assert.throws(() => assertStrategyTwoFirstInvocation(
    STRATEGY_TWO_FIRST_POLICY_SEED_BEFORE, false),
  new RegExp(`one-shot repair policy at seed 140.*${STRATEGY_TWO_REPAIR_POLICY_ADDRESS}`));
});

test("seed journal identity and repair hash are revalidated against the live anchor", () => {
  const identity = throwawayIdentity();
  const expectation: StrategyTwoSeedExpectation = {
    policySeedBefore: STRATEGY_TWO_FIRST_POLICY_SEED_BEFORE,
    expectedSeeds: Object.values(deriveStrategyTwoPolicySeeds(STRATEGY_TWO_FIRST_POLICY_SEED_BEFORE)),
    settingsAddress: RWA_MULTIPLY_ROUTE.squads.settings,
    genesisHash: RWA_MULTIPLY_ROUTE.genesisHash,
    strategyTwoConfig: identity.config,
    delegatedSigner: identity.delegatedSigner,
    repairPolicy: STRATEGY_TWO_REPAIR_POLICY_ADDRESS,
    repairPolicyDataSha256: "a".repeat(64),
    observationSlot: 1,
  };
  const directory = mkdtempSync("/tmp/rwa-multiply-seed-journal-");
  const journal = join(directory, "strategy-two-seeds.json");
  try {
    writeStrategyTwoSeedExpectation(journal, expectation);
    const journaled = readStrategyTwoSeedExpectation(journal);
    const observation: StrategyTwoSeedAnchorObservation = {
      ...journaled,
      repairPolicyPresent: true,
    };
    assert.doesNotThrow(() => assertStrategyTwoSeedJournalAnchor(journaled, observation));
    for (const [field, value] of [
      ["settingsAddress", "different-settings"],
      ["genesisHash", "different-genesis"],
      ["repairPolicyDataSha256", "b".repeat(64)],
    ] as const) {
      assert.throws(() => assertStrategyTwoSeedJournalAnchor(journaled, {
        ...observation,
        [field]: value,
      }), new RegExp(field === "repairPolicyDataSha256" ? "data hash" : field === "genesisHash" ? "genesis" : "Settings address"));
    }
    assert.throws(() => assertStrategyTwoSeedJournalAnchor(journaled, {
      ...observation,
      repairPolicyPresent: false,
    }), /repair policy/);
  } finally {
    rmSync(directory, { recursive: true, force: true });
  }
});

test("worker bindings derive their policy set from the seed journal", async () => {
  const policySeedBefore = STRATEGY_TWO_FIRST_POLICY_SEED_BEFORE;
  const identity = throwawayIdentity();
  const directory = mkdtempSync("/tmp/rwa-multiply-worker-config-");
  const journal = join(directory, "strategy-two-seeds.json");
  try {
    writeFileSync(journal, JSON.stringify({
      schema: "loyal-rwa-multiply-strategy-two-seed-expectation/v1",
      policySeedBefore: policySeedBefore.toString(),
      expectedSeeds: Object.values(deriveStrategyTwoPolicySeeds(policySeedBefore)).map(String),
      settingsAddress: RWA_MULTIPLY_ROUTE.squads.settings,
      genesisHash: RWA_MULTIPLY_ROUTE.genesisHash,
      strategyTwoConfig: identity.config,
      delegatedSigner: identity.delegatedSigner,
      repairPolicy: STRATEGY_TWO_REPAIR_POLICY_ADDRESS,
      repairPolicyDataSha256: "a".repeat(64),
      observationSlot: 1,
    }));
    const worker = await buildStrategyTwoWorkerPolicyConfig(journal, identity);
    const artifact = await compileCustomPolicyArtifact(
      policySeedBefore,
      await rwaMultiplyStrategyTwoTarget(identity, policySeedBefore),
    );
    assert.deepEqual(worker.policies.map(({ seed, policy }) => ({ seed, policy })),
      artifact.policies.map(({ seed, policy }) => ({ seed, policy })));
    assert.equal(worker.source.policySeedBefore, "140");
    assert.deepEqual(worker.legacyGate.policySeeds, ["62", "63", "64", "65"]);
  } finally {
    rmSync(directory, { recursive: true, force: true });
  }
});
