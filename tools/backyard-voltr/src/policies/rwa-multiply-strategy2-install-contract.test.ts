import assert from "node:assert/strict";
import { Buffer } from "node:buffer";
import { createHash } from "node:crypto";
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
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
  assertStrategyTwoAnchorObservation,
  assertStrategyTwoFirstInvocation,
  STRATEGY_TWO_ANCHOR_POLICY_ADDRESS,
  STRATEGY_TWO_ANCHOR_POLICY_DATA_SHA256,
  STRATEGY_TWO_FIRST_POLICY_SEED_BEFORE,
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
 * seam an operator install at seeds 145-148 relies on: a derived base seed of
 * 144 produces the exact four expected seeds in the compiler output. The test
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

test("the first installer invocation is pinned to the basic policy anchor at seed 144", () => {
  // The anchor is the seed-144 policy of the installed basic set, as recorded
  // in docs/evidence/backyard-rwa-basic/policy-install-readback-v1.json.
  assert.equal(STRATEGY_TWO_ANCHOR_POLICY_ADDRESS, "Z9jqB9pWDf1L1yFKVzXU1XnX8eKLndFP37FUwZMfWyz");
  assert.equal(customPolicyAddress(STRATEGY_TWO_FIRST_POLICY_SEED_BEFORE), STRATEGY_TWO_ANCHOR_POLICY_ADDRESS);
  assert.doesNotThrow(() => assertStrategyTwoFirstInvocation(
    STRATEGY_TWO_FIRST_POLICY_SEED_BEFORE, true));
  assert.throws(() => assertStrategyTwoFirstInvocation(143n, true),
    /requires finalized Settings counter 144/);
  assert.throws(() => assertStrategyTwoFirstInvocation(
    STRATEGY_TWO_FIRST_POLICY_SEED_BEFORE, false),
  new RegExp(`basic policy anchor at seed 144.*${STRATEGY_TWO_ANCHOR_POLICY_ADDRESS}`));
});

test("seed journal identity and anchor hash are revalidated against the live anchor", () => {
  const identity = throwawayIdentity();
  const expectation: StrategyTwoSeedExpectation = {
    policySeedBefore: STRATEGY_TWO_FIRST_POLICY_SEED_BEFORE,
    expectedSeeds: Object.values(deriveStrategyTwoPolicySeeds(STRATEGY_TWO_FIRST_POLICY_SEED_BEFORE)),
    settingsAddress: RWA_MULTIPLY_ROUTE.squads.settings,
    genesisHash: RWA_MULTIPLY_ROUTE.genesisHash,
    strategyTwoConfig: identity.config,
    delegatedSigner: identity.delegatedSigner,
    anchorPolicy: STRATEGY_TWO_ANCHOR_POLICY_ADDRESS,
    anchorPolicyDataSha256: STRATEGY_TWO_ANCHOR_POLICY_DATA_SHA256,
    observationSlot: 1,
  };
  const directory = mkdtempSync("/tmp/rwa-multiply-seed-journal-");
  const journal = join(directory, "strategy-two-seeds.json");
  try {
    writeStrategyTwoSeedExpectation(journal, expectation);
    const journaled = readStrategyTwoSeedExpectation(journal);
    const observation: StrategyTwoSeedAnchorObservation = {
      ...journaled,
      anchorPolicyPresent: true,
    };
    assert.doesNotThrow(() => assertStrategyTwoSeedJournalAnchor(journaled, observation));
    for (const [field, value, message] of [
      ["settingsAddress", "different-settings", "Settings address"],
      ["genesisHash", "different-genesis", "genesis"],
      ["anchorPolicyDataSha256", "b".repeat(64), "anchor policy data hash differs"],
    ] as const) {
      assert.throws(() => assertStrategyTwoSeedJournalAnchor(journaled, {
        ...observation,
        [field]: value,
      }), new RegExp(message));
    }
    assert.throws(() => assertStrategyTwoSeedJournalAnchor(journaled, {
      ...observation,
      anchorPolicyPresent: false,
    }), /basic policy anchor/);
    // Anchor bytes are pinned to the finalized install readback, so a journal
    // claiming any other hash is rejected when it is written or read.
    assert.throws(() => writeStrategyTwoSeedExpectation(journal, {
      ...expectation,
      anchorPolicyDataSha256: "b".repeat(64),
    }), /anchor policy data hash is not the finalized seed-144 install readback/);
    // First-invocation shape: the expectation is built from the live anchor
    // observation, so matching-but-drifted bytes still fail the readback pin.
    assert.throws(() => assertStrategyTwoSeedJournalAnchor({
      ...journaled,
      anchorPolicyDataSha256: "b".repeat(64),
    }, { ...observation, anchorPolicyDataSha256: "b".repeat(64) }),
    /anchor policy bytes differ from the seed-144 install readback/);
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
      anchorPolicy: STRATEGY_TWO_ANCHOR_POLICY_ADDRESS,
      anchorPolicyDataSha256: STRATEGY_TWO_ANCHOR_POLICY_DATA_SHA256,
      observationSlot: 1,
    }));
    const worker = await buildStrategyTwoWorkerPolicyConfig(journal, identity);
    const artifact = await compileCustomPolicyArtifact(
      policySeedBefore,
      await rwaMultiplyStrategyTwoTarget(identity, policySeedBefore),
    );
    assert.deepEqual(worker.policies.map(({ seed, policy }) => ({ seed, policy })),
      artifact.policies.map(({ seed, policy }) => ({ seed, policy })));
    assert.equal(worker.source.policySeedBefore, "144");
    assert.deepEqual(worker.legacyGate.policySeeds, ["62", "63", "64", "65"]);
    assert.deepEqual(worker.anchorGate, {
      seed: "144",
      policy: STRATEGY_TWO_ANCHOR_POLICY_ADDRESS,
      dataSha256: STRATEGY_TWO_ANCHOR_POLICY_DATA_SHA256,
      observed: "skipped-no-connection",
    });
  } finally {
    rmSync(directory, { recursive: true, force: true });
  }
});

test("the bootstrap rehearsal seed base is pinned to the seed-journal constant", async () => {
  // The rehearsal tool must compile the exact seed set the installer journals,
  // so it may not carry its own seed-base literal that can drift from 144.
  const source = readFileSync(
    new URL("../activation/rwa-multiply-strategy2-bootstrap.ts", import.meta.url), "utf8");
  assert.match(source,
    /const SIMULATED_POLICY_SEED_BEFORE = STRATEGY_TWO_FIRST_POLICY_SEED_BEFORE;/);
  assert.doesNotMatch(source, /SIMULATED_POLICY_SEED_BEFORE = \d+n/);
  // The shared live-anchor gate skips loudly when no connection is available
  // and rejects drifted anchor bytes otherwise.
  assert.deepEqual(assertStrategyTwoAnchorObservation(null), { observed: "skipped-no-connection" });
  assert.throws(() => assertStrategyTwoAnchorObservation({
    anchorPolicyPresent: true,
    anchorPolicyOwnerMatches: true,
    anchorPolicyDataSha256: "b".repeat(64),
  }), /anchor policy bytes differ from the seed-144 install readback/);
  assert.throws(() => assertStrategyTwoAnchorObservation({
    anchorPolicyPresent: false,
    anchorPolicyOwnerMatches: false,
    anchorPolicyDataSha256: null,
  }), /basic policy anchor at seed 144/);
});

/**
 * Wire C must be state-neutral in the runbook sense — manager restored to the
 * Squads vault and no economic/config field changed — not byte-identical:
 * Voltr stamps lastUpdatedTs on every config write, so a raw sha256 equality
 * could never hold and would deadlock the install.
 */
test("wire C state neutrality is decided on decoded vault fields, not raw bytes", () => {
  const bootstrap = readFileSync(
    new URL("../activation/rwa-multiply-strategy2-bootstrap.ts", import.meta.url), "utf8");
  const reconcile = readFileSync(
    new URL("../activation/rwa-multiply-strategy2-bootstrap-reconcile.ts", import.meta.url), "utf8");
  assert.doesNotMatch(bootstrap, /sha256\(vaultPost\.data\) === chainGates\.vaultSha256/);
  assert.doesNotMatch(reconcile, /sha256\(vault\.data\) === input\.expectedVaultSha256/);
  assert.match(bootstrap, /vaultNeutralityVerdict\(/);
  assert.match(reconcile, /vaultNeutralityVerdict\(/);
  // The relaxation and the simulated post-state are journaled explicitly.
  assert.match(bootstrap, /vaultSha256Before: chainGates\.vaultSha256/);
  assert.match(bootstrap, /vaultSha256AfterSimulation:/);
  assert.match(bootstrap, /vaultNeutralityIgnoredFields: VAULT_NEUTRALITY_IGNORED_FIELDS/);
  assert.match(bootstrap, /vaultNeutralityBefore: phase === "C" \? chainGates\.vaultNeutralityBefore : null/);
  // Reconciliation replays the same decoded rule from the journaled prestate.
  assert.match(reconcile, /pending journal lacks the decoded wire C vault prestate/);
  assert.match(reconcile, /expectedManager: input\.route\.squads\.vault/);
});
