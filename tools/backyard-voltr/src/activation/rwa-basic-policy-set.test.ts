import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { existsSync, mkdtempSync, readFileSync, rmSync } from "node:fs";
import { join } from "node:path";

import { test } from "bun:test";
import { generated, Policy } from "@loyal-labs/loyal-smart-accounts-core";
import { Keypair, PublicKey, Transaction } from "@solana/web3.js";

import { RWA_MULTIPLY_ROUTE } from "../domain/rwa-multiply-route-spec.js";
import {
  BASIC_POLICY_ARTIFACT,
  PACKET_LIMIT,
  POLICY_SEEDS,
  assertExecuteAuthorization,
  assertPolicyMatchesArtifact,
  assertPolicyPayloadMatchesArtifact,
  acquireInstallLock,
  canonicalPolicyBump,
  classifyInstallStart,
  classifyJournalDrift,
  classifyJournalLeg,
  createPolicyInstruction,
  derivePolicyAddress,
  parseArtifact,
  policyExpectationFromArtifact,
  releaseInstallLock,
  reconcileInstallJournal,
  validateInstallJournal,
} from "./rwa-basic-policy-set.js";

const artifact = parseArtifact(JSON.parse(readFileSync(BASIC_POLICY_ARTIFACT, "utf8")) as unknown);
const PROGRAM = "SMRTzfY6DfH5ik3TKiyLFfXexV8uSG3d2UksSCYdunG";
const SIGNATURE = "1".repeat(64);
const ARTIFACT_SHA256 = "b".repeat(64);

const artifactRow = (seed: string) => {
  const row = artifact.policies.find((policy) => policy.seed === seed);
  assert.ok(row !== undefined, `artifact has no policy ${seed}`);
  return row;
};

const sha256 = (value: Uint8Array | string): string => createHash("sha256").update(value).digest("hex");

test("parses the four-policy artifact and preserves stable fingerprints", () => {
  assert.deepEqual(artifact.policies.map((policy) => policy.seed), [...POLICY_SEEDS]);
  assert.deepEqual(artifact.policies.map((policy) => policy.account), artifact.policies.map((policy) => derivePolicyAddress(artifact.settings, policy.seed)));
  for (const row of artifact.policies) {
    const data = Buffer.from(row.instruction.dataBase64, "base64");
    // dataSha256 digests the PolicyCreate instruction data, not the policy account data.
    assert.equal(sha256(data), row.dataSha256);
    // legacyPacketBytes is the serialized legacy wire length for one fee-payer signature.
    const wire = new Transaction({ feePayer: new PublicKey(artifact.authority), recentBlockhash: PublicKey.default.toBase58() })
      .add(createPolicyInstruction(row));
    assert.equal(wire.serialize({ requireAllSignatures: false, verifySignatures: false }).length, row.legacyPacketBytes);
    assert.equal(row.legacyPacketBytes <= PACKET_LIMIT, true);
  }
});

test("derives every policy PDA from the settings and consecutive seed", () => {
  assert.deepEqual(artifact.policies.map((policy) => derivePolicyAddress(artifact.settings, policy.seed)), artifact.policies.map((policy) => policy.account));
});

test("builds legacy PolicyCreate instructions within the packet limit", () => {
  for (const policy of artifact.policies) {
    const instruction = createPolicyInstruction(policy);
    assert.equal(instruction.programId.toBase58(), PROGRAM);
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
  assert.doesNotThrow(() => validateInstallJournal({ ...journalFixture([legFixture("141")]) }));
  assert.throws(
    () => validateInstallJournal(journalFixture([{ ...legFixture("141"), seed: "143", account: artifactRow("143").account, family: artifactRow("143").family }])),
    /next policy seed/,
  );
});

test("reconcile classifier verifies a finalized leg only with its PDA present", () => {
  const present = classifyJournalLeg({ state: "finalized", seed: "141", livePolicySeed: "141", pdaPresent: true });
  assert.equal(present.action, "verify-finalized");
  assert.equal(present.resimulate, false);

  assert.equal(classifyJournalLeg({ state: "finalized", seed: "141", livePolicySeed: "140", pdaPresent: false }).action, "refuse");
  assert.equal(classifyJournalLeg({ state: "finalized", seed: "141", livePolicySeed: "140", pdaPresent: true }).action, "refuse");
});

test("reconcile classifier promotes a planned leg whose PDA landed on chain", () => {
  const decision = classifyJournalLeg({ state: "planned", seed: "142", livePolicySeed: "142", pdaPresent: true });
  assert.equal(decision.action, "promote-finalized");
  assert.equal(decision.resimulate, false);
});

test("reconcile classifier retries a planned leg that never landed only once the wire cannot land", () => {
  const failed = classifyJournalLeg({
    state: "planned", seed: "142", livePolicySeed: "141", pdaPresent: false,
    evidence: { signatureStatus: "failed", blockhashValid: null, statusSlot: 500 },
  });
  assert.equal(failed.action, "abandon-and-retry");
  assert.equal(failed.resimulate, false);

  const expired = classifyJournalLeg({
    state: "planned", seed: "142", livePolicySeed: "141", pdaPresent: false,
    evidence: { signatureStatus: "unknown", blockhashValid: false, statusSlot: null },
  });
  assert.equal(expired.action, "abandon-and-retry");

  assert.equal(classifyJournalLeg({ state: "planned", seed: "142", livePolicySeed: "141", pdaPresent: false }).action, "refuse");
});

test("reconcile classifier refuses a leg that may still land or landed without a PDA", () => {
  const inFlight = classifyJournalLeg({
    state: "planned", seed: "142", livePolicySeed: "141", pdaPresent: false,
    evidence: { signatureStatus: "unknown", blockhashValid: true, statusSlot: null },
  });
  assert.equal(inFlight.action, "refuse");
  assert.match(inFlight.reason, /leg 142 may still land; re-run reconcile after the blockhash expires/);

  const landed = classifyJournalLeg({
    state: "planned", seed: "142", livePolicySeed: "141", pdaPresent: false,
    evidence: { signatureStatus: "landed", blockhashValid: null, statusSlot: 500 },
  });
  assert.equal(landed.action, "refuse");
  assert.match(landed.reason, /landed but its PDA is absent/);
});

test("reconcile classifier treats a non-finalized error as replayable until the blockhash expires", () => {
  const live = classifyJournalLeg({
    state: "planned", seed: "142", livePolicySeed: "141", pdaPresent: false,
    evidence: { signatureStatus: "replayable-failure", blockhashValid: true, statusSlot: 500, confirmationStatus: "confirmed" },
  });
  assert.equal(live.action, "refuse");
  assert.match(live.reason, /leg 142 failed at confirmed but may still be replayed; re-run reconcile after the blockhash expires/);

  const expired = classifyJournalLeg({
    state: "planned", seed: "142", livePolicySeed: "141", pdaPresent: false,
    evidence: { signatureStatus: "replayable-failure", blockhashValid: false, statusSlot: 500, confirmationStatus: "processed" },
  });
  assert.equal(expired.action, "abandon-and-retry");
  assert.match(expired.reason, /leg 142|failed at processed without finalizing/);
});

test("reconcile classifier refuses a planned seed consumed elsewhere or ahead of the chain", () => {
  const consumed = classifyJournalLeg({
    state: "planned", seed: "142", livePolicySeed: "142", pdaPresent: false,
    evidence: { signatureStatus: "unknown", blockhashValid: false, statusSlot: null },
  });
  assert.equal(consumed.action, "refuse");
  assert.match(consumed.reason, /manual review/);

  assert.equal(classifyJournalLeg({
    state: "planned", seed: "143", livePolicySeed: "141", pdaPresent: false,
    evidence: { signatureStatus: "failed", blockhashValid: null, statusSlot: 500 },
  }).action, "refuse");
});

test("reconcile classifier re-simulates a blocked leg before retrying it", () => {
  const retry = classifyJournalLeg({
    state: "blocked", seed: "143", livePolicySeed: "142", pdaPresent: false,
    evidence: { signatureStatus: "failed", blockhashValid: null, statusSlot: 500 },
  });
  assert.equal(retry.action, "abandon-and-retry");
  assert.equal(retry.resimulate, true);

  const blockedButLive = classifyJournalLeg({
    state: "blocked", seed: "143", livePolicySeed: "142", pdaPresent: false,
    evidence: { signatureStatus: "unknown", blockhashValid: true, statusSlot: null },
  });
  assert.equal(blockedButLive.action, "refuse");
  assert.equal(classifyJournalLeg({
    state: "blocked", seed: "143", livePolicySeed: "142", pdaPresent: true,
  }).action, "refuse");
  assert.equal(classifyJournalLeg({
    state: "blocked", seed: "143", livePolicySeed: "143", pdaPresent: false,
    evidence: { signatureStatus: "failed", blockhashValid: null, statusSlot: 500 },
  }).action, "refuse");
});

test("start gate resumes a journal, refuses a completed install, and starts only a clean slate", () => {
  assert.equal(classifyInstallStart({ journalExists: false, readbackExists: false }), "fresh");
  assert.equal(classifyInstallStart({ journalExists: true, readbackExists: false }), "reconcile");
  assert.equal(classifyInstallStart({ journalExists: false, readbackExists: true }), "complete");
  assert.equal(classifyInstallStart({ journalExists: true, readbackExists: true }), "complete");
});

test("start gate refuses a journal drifted from the current artifact", () => {
  const base = {
    journalArtifactSha256: ARTIFACT_SHA256,
    journalSettings: artifact.settings,
    journalAuthority: artifact.authority,
    artifactSha256: ARTIFACT_SHA256,
    settings: artifact.settings,
    authority: artifact.authority,
  };
  assert.equal(classifyJournalDrift(base), "match");
  assert.equal(classifyJournalDrift({ ...base, journalSettings: undefined, journalAuthority: undefined }), "match");
  assert.equal(classifyJournalDrift({ ...base, journalArtifactSha256: "c".repeat(64) }), "drift");
  assert.equal(classifyJournalDrift({ ...base, journalSettings: "other-settings" }), "drift");
  assert.equal(classifyJournalDrift({ ...base, journalAuthority: "other-authority" }), "drift");
});

test("journal accepts an abandoned leg only when a same-seed retry follows it", () => {
  const abandoned = { ...legFixture("141"), state: "abandoned", abandonReason: "never landed; blockhash expired" };
  const resumed = {
    ...journalFixture([abandoned, legFixture("141")]),
    reconciliations: [{
      at: "2026-09-09T00:00:00.000Z",
      finalizedSlot: 1_000,
      livePolicySeed: "140",
      verdicts: [{ seed: "141", recordedState: "planned", verdict: "abandon-and-retry" }],
    }],
  };
  assert.doesNotThrow(() => validateInstallJournal(resumed));
  // blocked and planned attempts may follow abandoned attempts, in any count.
  assert.doesNotThrow(() => validateInstallJournal(journalFixture([abandoned, {
    ...legFixture("141"),
    state: "blocked",
    preSendSimulation: { contextSlot: 456, unitsConsumed: 789, err: "BlockhashNotFound" },
  }])));
  assert.doesNotThrow(() => validateInstallJournal(journalFixture([
    abandoned,
    { ...legFixture("141"), state: "abandoned", abandonReason: "never landed; blockhash expired again" },
    legFixture("141"),
  ])));
  // A second unbroken attempt is not a retry.
  assert.throws(() => validateInstallJournal(journalFixture([legFixture("141"), legFixture("141")])), /second unbroken attempt/);
  assert.throws(() => validateInstallJournal(journalFixture([
    abandoned,
    legFixture("141"),
    { ...legFixture("141"), state: "blocked", preSendSimulation: { contextSlot: 456, unitsConsumed: 789, err: "BlockhashNotFound" } },
  ])), /second unbroken attempt/);
  // Nothing follows a finalized leg.
  assert.throws(
    () => validateInstallJournal(journalFixture([{ ...legFixture("141"), state: "finalized", readback: finalizedReadback("141") }, legFixture("141")])),
    /already finalized/,
  );
  // Seeds never repeat after the chain moved on, and no seed is skipped.
  assert.throws(
    () => validateInstallJournal(journalFixture([legFixture("141"), legFixture("143", { account: artifactRow("143").account, family: artifactRow("143").family })])),
    /not the next policy seed/,
  );
  assert.throws(
    () => validateInstallJournal({ ...resumed, reconciliations: [{ at: "2026-09-09T00:00:00.000Z", finalizedSlot: 1_000, livePolicySeed: "140", verdicts: [{}] }] }),
    /verdict/,
  );
});

test("journal enforces per-state required fields and leg identity", () => {
  const planned = legFixture("141");
  assert.doesNotThrow(() => validateInstallJournal(journalFixture([planned]), artifact));
  assert.doesNotThrow(() => validateInstallJournal(journalFixture([planned])));

  assert.throws(() => validateInstallJournal(journalFixture([{ ...planned, account: artifactRow("142").account }]), artifact), /PDA drifted/);
  assert.throws(() => validateInstallJournal(journalFixture([{ ...planned, family: "SomeoneElse" }]), artifact), /family drifted/);
  assert.throws(() => validateInstallJournal(journalFixture([{ ...planned, signature: "test-signature" }]), artifact), /signature is not base58/);
  assert.throws(
    () => validateInstallJournal(journalFixture([{ ...planned, preSendSimulation: { contextSlot: 456, unitsConsumed: 789, err: "BlockhashNotFound" } }]), artifact),
    /planned without a passing pre-send simulation/,
  );

  const blocked = { ...planned, state: "blocked", preSendSimulation: { contextSlot: 456, unitsConsumed: 789, err: "BlockhashNotFound" } };
  assert.doesNotThrow(() => validateInstallJournal(journalFixture([blocked]), artifact));
  assert.throws(() => validateInstallJournal(journalFixture([{ ...planned, state: "blocked" }]), artifact), /blocked without a pre-send simulation error/);

  assert.throws(() => validateInstallJournal(journalFixture([{ ...planned, state: "abandoned" }]), artifact), /abandonReason/);
  assert.doesNotThrow(() => validateInstallJournal(journalFixture([{ ...planned, state: "abandoned", abandonReason: "blockhash expired" }]), artifact));

  const finalized = { ...planned, state: "finalized", readback: finalizedReadback("141") };
  assert.doesNotThrow(() => validateInstallJournal(journalFixture([finalized]), artifact));
  assert.throws(() => validateInstallJournal(journalFixture([{ ...planned, state: "finalized" }]), artifact), /readback/);
  // Every field the readback writes is required, and the recorded hash must be the artifact's.
  assert.throws(() => validateInstallJournal(journalFixture([{ ...planned, state: "finalized", readback: { finalizedSlot: "900" } }]), artifact), /readback seed drifted/);
  assert.throws(
    () => validateInstallJournal(journalFixture([{ ...planned, state: "finalized", readback: { ...finalizedReadback("141"), accountDataSha256: undefined } }]), artifact),
    /readback account hash is invalid/,
  );
  assert.throws(
    () => validateInstallJournal(journalFixture([{ ...planned, state: "finalized", readback: { ...finalizedReadback("141"), dataSha256: "c".repeat(64) } }]), artifact),
    /readback data hash drifted from the artifact/,
  );

  // Slots are nonnegative integers.
  assert.throws(
    () => validateInstallJournal(journalFixture([{ ...planned, preSendSimulation: { contextSlot: -1, unitsConsumed: 789, err: null } }]), artifact),
    /simulation slot is invalid/,
  );
  assert.throws(
    () => validateInstallJournal(journalFixture([{ ...planned, state: "finalized", readback: { ...finalizedReadback("141"), finalizedSlot: -5 } }]), artifact),
    /readback slot is invalid/,
  );
  assert.throws(() => validateInstallJournal(journalFixture([{ ...planned, lastValidBlockHeight: 0 }]), artifact), /block height is invalid/);
});

test("install lock is exclusive and leaves no file behind on release", () => {
  const directory = mkdtempSync("/tmp/rwa-basic-policy-lock-");
  const lock = join(directory, "policy-install.lock");
  try {
    acquireInstallLock(lock, () => "2026-09-09T00:00:00.000Z");
    const recorded = JSON.parse(readFileSync(lock, "utf8")) as { pid: unknown; startedAt: unknown };
    assert.equal(typeof recorded.pid, "number");
    assert.equal(recorded.startedAt, "2026-09-09T00:00:00.000Z");
    assert.throws(() => acquireInstallLock(lock), /already exists/);
    releaseInstallLock(lock);
    assert.equal(existsSync(lock), false);
  } finally {
    rmSync(directory, { recursive: true, force: true });
  }
});

test("full policy readback matches the artifact instruction field by field", () => {
  for (const row of artifact.policies) {
    const expected = policyExpectationFromArtifact(row);
    assert.equal(expected.settings, artifact.settings);
    assert.deepEqual(expected.signers, [{ key: artifact.delegate, permissionsMask: 7 }]);
    assert.equal(expected.threshold, 1);
    assert.equal(expected.timeLock, 0);
    assert.equal(expected.start, "0");
    assert.equal(expected.rentCollector, PublicKey.default.toBase58());
    assert.equal(expected.accountIndex, 0);
    assert.doesNotThrow(() => assertPolicyMatchesArtifact(row, decodePolicyAccount(policyAccount(row.seed, expected.constraints))));
    assert.doesNotThrow(() => assertPolicyPayloadMatchesArtifact(row, expected.constraints));
  }
});

test("full policy readback refuses any altered on-chain field", () => {
  const row = artifactRow("141");
  const constraints = policyExpectationFromArtifact(row).constraints;
  const mutate = (apply: (decoded: DecodedPolicy) => void): DecodedPolicy => {
    const decoded = decodePolicyAccount(policyAccount("141", constraints));
    apply(decoded);
    return decoded;
  };
  assert.throws(() => assertPolicyMatchesArtifact(row, mutate((d) => { d.timeLock = 5; })), /time lock 5 does not match the artifact 0/);
  assert.throws(() => assertPolicyMatchesArtifact(row, mutate((d) => { d.threshold = 2; })), /threshold 2 does not match the artifact 1/);
  assert.throws(() => assertPolicyMatchesArtifact(row, mutate((d) => { d.start = 7n; })), /on-chain start 7 does not match the artifact 0/);
  assert.throws(() => assertPolicyMatchesArtifact(row, mutate((d) => { d.expiration = { __kind: "Timestamp", fields: [123n] }; })), /expires, but the artifact sets no expiration/);
  assert.throws(() => assertPolicyMatchesArtifact(row, mutate((d) => { d.rentCollector = Keypair.generate().publicKey; })), /rent collector/);
  assert.throws(() => assertPolicyMatchesArtifact(row, mutate((d) => { d.signers[0].key = Keypair.generate().publicKey; })), /delegated signer/);
  assert.throws(() => assertPolicyMatchesArtifact(row, mutate((d) => { d.signers[0].permissions.mask = 3; })), /permissions 3 do not match the artifact 7/);
  assert.throws(() => assertPolicyMatchesArtifact(row, mutate((d) => { d.seed = 142n; })), /on-chain seed 142 does not match the artifact 141/);
  assert.throws(() => assertPolicyMatchesArtifact(row, mutate((d) => { d.policyState.fields[0].preHook = { __kind: "Hook" }; })), /hooks, but the artifact sets none/);
  assert.throws(() => assertPolicyMatchesArtifact(row, mutate((d) => { d.policyState.fields[0].postHook = { __kind: "Hook" }; })), /hooks, but the artifact sets none/);
  assert.throws(() => assertPolicyMatchesArtifact(row, mutate((d) => { d.policyState.fields[0].spendingLimits = [{}]; })), /spending limits, but the artifact sets none/);
  assert.throws(() => assertPolicyMatchesArtifact(row, mutate((d) => { d.policyState.fields[0].accountIndex = 1; })), /account index 1 does not match the artifact 0/);
  assert.throws(() => assertPolicyMatchesArtifact(row, mutate((d) => { d.policyState.__kind = "SpendingLimit"; })), /policy kind/);
  // Fresh-install account state: the canonical PDA bump and zeroed transaction counters.
  assert.throws(() => assertPolicyMatchesArtifact(row, mutate((d) => { d.bump = (d.bump + 1) % 256; })), /does not match the canonical PDA bump/);
  assert.throws(() => assertPolicyMatchesArtifact(row, mutate((d) => { d.transactionIndex = 1n; })), /transaction index 1\/0 is not the fresh-install zero state/);
  assert.throws(() => assertPolicyMatchesArtifact(row, mutate((d) => { d.staleTransactionIndex = 1n; })), /transaction index 0\/1 is not the fresh-install zero state/);
});

test("payload check accepts the artifact constraints and rejects any other lane", () => {
  for (const row of artifact.policies) {
    assert.doesNotThrow(() => assertPolicyPayloadMatchesArtifact(row, decodeConstraints(row.seed)));
  }
  assert.throws(() => assertPolicyPayloadMatchesArtifact(artifactRow("141"), decodeConstraints("142")), /do not match the artifact payload/);
  assert.throws(() => assertPolicyPayloadMatchesArtifact(artifactRow("141"), tamperAccountConstraint("141")), /do not match the artifact payload/);
});

test("reconcile refuses a drifted journal before reading any chain state", async () => {
  const journal = journalFixture([legFixture("141")]);
  const outcome = await reconcileInstallJournal({
    connection: fakeConnection({ liveSeed: "140", present: [] }),
    artifact,
    journal,
    artifactSha256: "c".repeat(64),
  });
  assert.equal(outcome.status, "refused");
  assert.match(outcome.reason ?? "", /drifted from the current artifact/);
  assert.equal(outcome.reconciliation, null);
});

test("reconcile refuses an unsent leg while its blockhash can still land", async () => {
  const journal = journalFixture([legFixture("141")]);
  const outcome = await reconcileInstallJournal({
    connection: fakeConnection({ liveSeed: "140", present: [], blockhashValid: true }),
    artifact,
    journal,
    artifactSha256: ARTIFACT_SHA256,
  });
  assert.equal(outcome.status, "refused");
  assert.match(outcome.reason ?? "", /leg 141 may still land; re-run reconcile after the blockhash expires/);
  assert.equal((journalLeg(journal, 0).state as string), "planned");
  assert.deepEqual(outcome.pendingSeeds, []);
  const verdict = outcome.reconciliation?.verdicts[0] as Record<string, unknown>;
  assert.equal(verdict.signatureStatus, "unknown");
  assert.equal(verdict.blockhashValid, true);
});

test("reconcile refuses a leg whose signature landed but whose PDA is still absent", async () => {
  const journal = journalFixture([legFixture("141")]);
  const outcome = await reconcileInstallJournal({
    connection: fakeConnection({
      liveSeed: "140",
      present: [],
      signatures: { [SIGNATURE]: { err: null, confirmationStatus: "confirmed", slot: 500 } },
    }),
    artifact,
    journal,
    artifactSha256: ARTIFACT_SHA256,
  });
  assert.equal(outcome.status, "refused");
  assert.match(outcome.reason ?? "", /landed but its PDA is absent/);
});

test("reconcile refuses a confirmed failure while its blockhash can still be replayed", async () => {
  const journal = journalFixture([legFixture("141")]);
  const outcome = await reconcileInstallJournal({
    connection: fakeConnection({
      liveSeed: "140",
      present: [],
      blockhashValid: true,
      signatures: { [SIGNATURE]: { err: "BlockhashNotFound", confirmationStatus: "confirmed", slot: 500 } },
    }),
    artifact,
    journal,
    artifactSha256: ARTIFACT_SHA256,
  });
  assert.equal(outcome.status, "refused");
  assert.match(outcome.reason ?? "", /leg 141 failed at confirmed but may still be replayed; re-run reconcile after the blockhash expires/);
  assert.equal(journalLeg(journal, 0).state, "planned");
  const verdict = outcome.reconciliation?.verdicts[0] as Record<string, unknown>;
  assert.equal(verdict.signatureStatus, "replayable-failure");
  assert.equal(verdict.blockhashValid, true);
});

test("reconcile refuses a processed failure while its blockhash can still be replayed", async () => {
  const journal = journalFixture([legFixture("141")]);
  const outcome = await reconcileInstallJournal({
    connection: fakeConnection({
      liveSeed: "140",
      present: [],
      blockhashValid: true,
      signatures: { [SIGNATURE]: { err: "AccountInUse", confirmationStatus: "processed", slot: 500 } },
    }),
    artifact,
    journal,
    artifactSha256: ARTIFACT_SHA256,
  });
  assert.equal(outcome.status, "refused");
  assert.match(outcome.reason ?? "", /leg 141 failed at processed but may still be replayed/);
});

test("reconcile abandons a planned leg once its send failed on chain", async () => {
  const journal = journalFixture([legFixture("141")]);
  const outcome = await reconcileInstallJournal({
    connection: fakeConnection({
      liveSeed: "140",
      present: [],
      signatures: { [SIGNATURE]: { err: "AccountInUse", confirmationStatus: "finalized", slot: 500 } },
    }),
    artifact,
    journal,
    artifactSha256: ARTIFACT_SHA256,
  });
  assert.equal(outcome.status, "reconciled");
  const leg = journalLeg(journal, 0);
  assert.equal(leg.state, "abandoned");
  assert.equal(leg.signature, SIGNATURE);
  assert.match(leg.abandonReason as string, /failed on chain at finalized/);
  assert.deepEqual(outcome.pendingSeeds, [...POLICY_SEEDS]);
  const verdict = outcome.reconciliation?.verdicts[0] as Record<string, unknown>;
  assert.equal(verdict.signatureStatus, "failed");
  assert.equal(verdict.statusSlot, 500);
  assert.deepEqual(outcome.reconciliation?.livePolicySeed, "140");
});

test("reconcile abandons an unseen leg only after its blockhash expires", async () => {
  const journal = journalFixture([legFixture("141")]);
  const outcome = await reconcileInstallJournal({
    connection: fakeConnection({ liveSeed: "140", present: [], blockhashValid: false }),
    artifact,
    journal,
    artifactSha256: ARTIFACT_SHA256,
  });
  assert.equal(outcome.status, "reconciled");
  const leg = journalLeg(journal, 0);
  assert.equal(leg.state, "abandoned");
  assert.match(leg.abandonReason as string, /never landed and its blockhash expired/);
  assert.deepEqual(outcome.pendingSeeds, [...POLICY_SEEDS]);
});

test("reconcile abandons a processed failure only after its blockhash expires", async () => {
  const journal = journalFixture([legFixture("141")]);
  const outcome = await reconcileInstallJournal({
    connection: fakeConnection({
      liveSeed: "140",
      present: [],
      blockhashValid: false,
      signatures: { [SIGNATURE]: { err: "AccountInUse", confirmationStatus: "processed", slot: 500 } },
    }),
    artifact,
    journal,
    artifactSha256: ARTIFACT_SHA256,
  });
  assert.equal(outcome.status, "reconciled");
  const leg = journalLeg(journal, 0);
  assert.equal(leg.state, "abandoned");
  assert.match(leg.abandonReason as string, /failed at processed without finalizing and its blockhash expired/);
  const verdict = outcome.reconciliation?.verdicts[0] as Record<string, unknown>;
  assert.equal(verdict.signatureStatus, "replayable-failure");
  assert.equal(verdict.confirmationStatus, "processed");
  assert.equal(verdict.blockhashValid, false);
});

test("reconcile re-simulates a blocked leg before retrying it", async () => {
  const journal = journalFixture([legFixture("141", {
    state: "blocked",
    preSendSimulation: { contextSlot: 456, unitsConsumed: 789, err: "BlockhashNotFound" },
  })]);
  const outcome = await reconcileInstallJournal({
    connection: fakeConnection({
      liveSeed: "140",
      present: [],
      signatures: { [SIGNATURE]: { err: "BlockhashNotFound", confirmationStatus: "finalized", slot: 500 } },
    }),
    artifact,
    journal,
    artifactSha256: ARTIFACT_SHA256,
  });
  assert.equal(outcome.status, "reconciled");
  assert.equal(journalLeg(journal, 0).state, "abandoned");
  const verdict = outcome.reconciliation?.verdicts[0] as Record<string, unknown>;
  assert.equal(verdict.resimulated, true);
});

test("reconcile re-verifies a recorded abandoned leg and refuses a still-replayable wire", async () => {
  const abandoned = { ...legFixture("141"), state: "abandoned", abandonReason: "never landed; blockhash expired" };
  const replayable = await reconcileInstallJournal({
    connection: fakeConnection({ liveSeed: "140", present: [], blockhashValid: true }),
    artifact,
    journal: journalFixture([abandoned, legFixture("141")]),
    artifactSha256: ARTIFACT_SHA256,
  });
  assert.equal(replayable.status, "refused");
  assert.match(replayable.reason ?? "", /abandoned leg 141 attempt 1 is still replayable; manual review/);
  assert.deepEqual(replayable.pendingSeeds, []);
  const verdict = replayable.reconciliation?.verdicts[0] as Record<string, unknown>;
  assert.equal(verdict.attempt, 1);
  assert.equal(verdict.verdict, "refuse");
  assert.equal(verdict.blockhashValid, true);

  const landed = await reconcileInstallJournal({
    connection: fakeConnection({
      liveSeed: "140",
      present: [],
      signatures: { [SIGNATURE]: { err: null, confirmationStatus: "confirmed", slot: 500 } },
    }),
    artifact,
    journal: journalFixture([abandoned, legFixture("141")]),
    artifactSha256: ARTIFACT_SHA256,
  });
  assert.equal(landed.status, "refused");
  assert.match(landed.reason ?? "", /abandoned leg 141 attempt 1 is still replayable; manual review/);
});

test("reconcile keeps an abandoned leg only with a finalized error or an expired blockhash", async () => {
  const abandoned = { ...legFixture("141"), state: "abandoned", abandonReason: "never landed; blockhash expired" };
  const finalizedFailure = await reconcileInstallJournal({
    connection: fakeConnection({
      liveSeed: "140",
      present: [],
      blockhashValid: true,
      signatures: { [SIGNATURE]: { err: "AccountInUse", confirmationStatus: "finalized", slot: 500 } },
    }),
    artifact,
    journal: journalFixture([abandoned, legFixture("141")]),
    artifactSha256: ARTIFACT_SHA256,
  });
  assert.equal(finalizedFailure.status, "reconciled");
  assert.deepEqual(finalizedFailure.pendingSeeds, [...POLICY_SEEDS]);
  const verdict = finalizedFailure.reconciliation?.verdicts[0] as Record<string, unknown>;
  assert.equal(verdict.attempt, 1);
  assert.equal(verdict.verdict, "skipped");
  assert.equal(verdict.signatureStatus, "failed");

  const expired = await reconcileInstallJournal({
    connection: fakeConnection({ liveSeed: "140", present: [], blockhashValid: false }),
    artifact,
    journal: journalFixture([abandoned, legFixture("141")]),
    artifactSha256: ARTIFACT_SHA256,
  });
  assert.equal(expired.status, "reconciled");
  assert.deepEqual(expired.pendingSeeds, [...POLICY_SEEDS]);
});

test("reconcile re-verifies every abandoned attempt per seed and numbers them", async () => {
  // Attempt 1 provably failed at finalized; attempt 2 still holds a live blockhash.
  const first = { ...legFixture("141"), state: "abandoned", abandonReason: "failed at finalized", signature: "2".repeat(64) };
  const second = { ...legFixture("141"), state: "abandoned", abandonReason: "never landed; blockhash expired" };
  const outcome = await reconcileInstallJournal({
    connection: fakeConnection({
      liveSeed: "140",
      present: [],
      blockhashValid: true,
      signatures: { ["2".repeat(64)]: { err: "AccountInUse", confirmationStatus: "finalized", slot: 500 } },
    }),
    artifact,
    journal: journalFixture([first, second, legFixture("141")]),
    artifactSha256: ARTIFACT_SHA256,
  });
  assert.equal(outcome.status, "refused");
  assert.match(outcome.reason ?? "", /abandoned leg 141 attempt 2 is still replayable; manual review/);
  const verdicts = outcome.reconciliation?.verdicts as Array<Record<string, unknown>>;
  assert.equal(verdicts[0].attempt, 1);
  assert.equal(verdicts[0].verdict, "skipped");
  assert.equal(verdicts[1].attempt, 2);
  assert.equal(verdicts[1].verdict, "refuse");
});

test("reconcile promotes a planned leg and verifies its full payload against the artifact", async () => {
  const journal = journalFixture([legFixture("141")]);
  const outcome = await reconcileInstallJournal({
    connection: fakeConnection({ liveSeed: "141", present: ["141"] }),
    artifact,
    journal,
    artifactSha256: ARTIFACT_SHA256,
  });
  assert.equal(outcome.status, "reconciled");
  const leg = journalLeg(journal, 0);
  assert.equal(leg.state, "finalized");
  const readback = leg.readback as Record<string, unknown>;
  assert.equal(readback.finalizedSlot, 900);
  assert.equal(readback.dataSha256, artifactRow("141").dataSha256);
  assert.equal(readback.accountDataSha256, sha256(policyAccount("141", policyExpectationFromArtifact(artifactRow("141")).constraints)));
  assert.deepEqual(outcome.pendingSeeds, ["142", "143", "144"]);
});

test("reconcile refuses a policy account whose discriminator does not match the generated codec", async () => {
  const corrupted = policyAccount("141", decodeConstraints("141"));
  corrupted[3] ^= 0xff;
  const journal = journalFixture([legFixture("141", { state: "finalized", readback: finalizedReadback("141") })]);
  await assert.rejects(
    () => reconcileInstallJournal({
      connection: fakeConnection({ liveSeed: "141", present: ["141"], policyData: () => corrupted }),
      artifact,
      journal,
      artifactSha256: ARTIFACT_SHA256,
    }),
    /account discriminator does not match the generated Policy discriminator/,
  );
});

test("reconcile refuses an on-chain policy payload that the artifact does not specify", async () => {
  const mismatched = policyAccount("141", decodeConstraints("142"));
  const journal = journalFixture([legFixture("141", {
    state: "finalized",
    readback: { seed: "141", account: artifactRow("141").account, dataSha256: "c".repeat(64), finalizedSlot: 800 },
  })]);
  await assert.rejects(
    () => reconcileInstallJournal({
      connection: fakeConnection({ liveSeed: "141", present: ["141"], policyData: () => mismatched }),
      artifact,
      journal,
      artifactSha256: ARTIFACT_SHA256,
    }),
    /do not match the artifact payload/,
  );
});

function legFixture(seed: string, overrides: Record<string, unknown> = {}): Record<string, unknown> {
  const row = artifactRow(seed);
  return {
    seed,
    account: row.account,
    family: row.family,
    state: "planned",
    wireSha256: "a".repeat(64),
    blockhash: "test-blockhash",
    lastValidBlockHeight: 123,
    packetBytes: row.legacyPacketBytes,
    signature: SIGNATURE,
    preSendSimulation: { contextSlot: 456, unitsConsumed: 789, err: null },
    ...overrides,
  };
}

function journalFixture(legs: readonly unknown[]): Record<string, unknown> {
  return {
    schema: "loyal-backyard-rwa-basic-policy-install-journal/v1",
    broadcast: true,
    artifactSha256: ARTIFACT_SHA256,
    settings: artifact.settings,
    authority: artifact.authority,
    legs: [...legs],
  };
}

function journalLeg(journal: Record<string, unknown>, index: number): Record<string, unknown> {
  return (journal.legs as Array<Record<string, unknown>>)[index] as Record<string, unknown>;
}

/** The readback exactly as `finalizedPolicyReadback` records it for a fixture policy account. */
function finalizedReadback(seed: string): Record<string, unknown> {
  return {
    seed,
    account: artifactRow(seed).account,
    dataSha256: artifactRow(seed).dataSha256,
    accountDataSha256: sha256(policyAccount(seed, decodeConstraints(seed))),
    finalizedSlot: 900,
  };
}

type StructBeet = { serialize(args: Record<string, unknown>): [Buffer, number] };
type ConstraintBeet = {
  toFixedFromData(data: Buffer, offset: number): { read(data: Buffer, offset: number): unknown; byteSize: number };
  toFixedFromValue(value: unknown): { write(buffer: Buffer, offset: number, value: unknown): void; byteSize: number };
};

const { settingsBeet, policyBeet, instructionConstraintBeet } = generated as unknown as {
  settingsBeet: StructBeet;
  policyBeet: StructBeet;
  instructionConstraintBeet: ConstraintBeet;
};

/** Legacy PolicyCreate payload: the ProgramInteraction constraint vector starts at this fixed offset. */
const CONSTRAINTS_OFFSET = 28;

function decodeConstraints(seed: string): readonly unknown[] {
  const row = artifactRow(seed);
  const data = Buffer.from(row.instruction.dataBase64, "base64");
  const constraints: unknown[] = [];
  let offset = CONSTRAINTS_OFFSET;
  for (let index = 0; index < row.constraints.length; index += 1) {
    const fixed = instructionConstraintBeet.toFixedFromData(data, offset);
    constraints.push(fixed.read(data, offset));
    offset += fixed.byteSize;
  }
  return constraints;
}

function tamperAccountConstraint(seed: string): readonly unknown[] {
  return (decodeConstraints(seed) as Array<Record<string, unknown>>).map((constraint, index) => {
    if (index !== 0) return constraint;
    const accountConstraints = constraint.accountConstraints as Array<Record<string, unknown>>;
    return {
      ...constraint,
      accountConstraints: accountConstraints.map((entry, entryIndex) => entryIndex === 1
        ? { ...entry, accountConstraint: { __kind: "Pubkey", fields: [[PublicKey.default]] } }
        : entry),
    };
  });
}

function policyAccount(seed: string, constraints: readonly unknown[]): Buffer {
  const expected = policyExpectationFromArtifact(artifactRow(seed));
  const [data] = policyBeet.serialize({
    accountDiscriminator: (generated as unknown as { policyDiscriminator: number[] }).policyDiscriminator,
    settings: new PublicKey(expected.settings),
    seed: BigInt(expected.seed),
    bump: canonicalPolicyBump(artifact.settings, seed),
    transactionIndex: 0n,
    staleTransactionIndex: 0n,
    signers: expected.signers.map((signer) => ({ key: new PublicKey(signer.key), permissions: { mask: signer.permissionsMask } })),
    threshold: expected.threshold,
    timeLock: expected.timeLock,
    policyState: {
      __kind: expected.policyKind,
      fields: [{ accountIndex: expected.accountIndex, instructionsConstraints: constraints, preHook: null, postHook: null, spendingLimits: [] }],
    },
    start: BigInt(expected.start),
    expiration: null,
    rentCollector: new PublicKey(expected.rentCollector),
  });
  return data;
}

type DecodedPolicy = Record<string, any>;

function decodePolicyAccount(data: Buffer): DecodedPolicy {
  return (Policy as unknown as {
    fromAccountInfo(info: { owner: PublicKey; data: Buffer; lamports: number; executable: boolean; rentEpoch: number }): readonly [DecodedPolicy, number];
  }).fromAccountInfo({ owner: new PublicKey(PROGRAM), data, lamports: 1, executable: false, rentEpoch: 0 })[0];
}

function settingsAccount(policySeed: string): Buffer {
  const [data] = settingsBeet.serialize({
    accountDiscriminator: (generated as unknown as { settingsDiscriminator: number[] }).settingsDiscriminator,
    seed: 140n,
    settingsAuthority: new PublicKey(artifact.authority),
    threshold: 1,
    timeLock: 0,
    transactionIndex: 0n,
    staleTransactionIndex: 0n,
    archivalAuthority: null,
    archivableAfter: 0n,
    bump: 255,
    signers: [{ key: new PublicKey(artifact.delegate), permissions: { mask: 3 } }],
    accountUtilization: 0,
    policySeed: BigInt(policySeed),
    reserved2: 0,
  });
  return data;
}

function fakeConnection(input: Readonly<{
  liveSeed: string;
  present: readonly string[];
  policyData?: (seed: string) => Buffer | null;
  signatures?: Record<string, { err: unknown; confirmationStatus: string; slot: number } | null>;
  blockhashValid?: boolean;
  slot?: number;
}>) {
  const slot = input.slot ?? 900;
  const settingsInfo = () => ({ owner: new PublicKey(PROGRAM), data: settingsAccount(input.liveSeed), lamports: 1, executable: false, rentEpoch: 0 });
  const account = (address: string | PublicKey) => {
    const key = typeof address === "string" ? address : address.toBase58();
    if (key === RWA_MULTIPLY_ROUTE.squads.settings) return settingsInfo();
    const row = artifact.policies.find((policy) => policy.account === key);
    if (row === undefined) return null;
    const data = input.policyData !== undefined
      ? input.policyData(row.seed)
      : input.present.includes(row.seed) ? policyAccount(row.seed, decodeConstraints(row.seed)) : null;
    return data === null ? null : { owner: new PublicKey(PROGRAM), data, lamports: 1, executable: false, rentEpoch: 0 };
  };
  return {
    getAccountInfoAndContext: async (address: string) => ({
      context: { slot },
      value: account(address),
    }),
    getAccountInfo: async (address: string) => account(address),
    getMultipleAccountsInfoAndContext: async (addresses: readonly string[]) => ({
      context: { slot },
      value: addresses.map((address) => account(address)),
    }),
    getSignatureStatuses: async (signatures: readonly string[]) => ({
      context: { slot },
      value: signatures.map((signature) => input.signatures?.[signature] ?? null),
    }),
    isBlockhashValid: async () => ({ context: { slot }, value: input.blockhashValid === true }),
    simulateTransaction: async () => ({ context: { slot }, value: { err: null, logs: [], unitsConsumed: 1_000n } }),
    getLatestBlockhashAndContext: async () => ({ context: { slot }, value: { blockhash: "fake-blockhash", lastValidBlockHeight: 4_242 } }),
  } as unknown as Parameters<typeof reconcileInstallJournal>[0]["connection"];
}
