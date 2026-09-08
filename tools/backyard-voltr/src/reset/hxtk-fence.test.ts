import {
  chmodSync,
  existsSync,
  linkSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  renameSync,
  rmSync,
  statSync,
  symlinkSync,
  unlinkSync,
  writeFileSync,
} from "node:fs";
import { hostname } from "node:os";
import { join } from "node:path";
import { afterEach, describe, expect, test } from "bun:test";

import {
  assertCanonicalLegAvailable,
  assertFinalizedJournalBinding,
  assertPendingJournalBinding,
  acquireCanonicalLegClaim,
  beginCanonicalLegRecord,
  canonicalJson,
  canonicalStateGeneration,
  finalizedJournalSha256,
  pendingBindingSha256,
  readCanonicalState,
  readBoundPending,
  releaseCanonicalLegClaim,
  repairPostFinalizationStatus,
  resolveCanonicalStateRoot,
  rewritePendingStatus,
  writeCanonicalStateCas,
} from "./hxtk-fence.js";

const temporaryRoots: string[] = [];
const uid = typeof process.getuid === "function" ? process.getuid() : 0;

afterEach(() => {
  for (const root of temporaryRoots.splice(0)) rmSync(root, { recursive: true, force: true });
});

function tempRoot(): string {
  const root = mkdtempSync(join("/tmp", "hxtk-fence-"));
  temporaryRoots.push(root);
  return root;
}

function legInput(overrides: Partial<Parameters<typeof beginCanonicalLegRecord>[0]> = {}) {
  return {
    step: "harvest",
    vault: "HXtk",
    journal: "/tmp/harvest.json",
    expectedSignature: "signature",
    messageSha256: "message-hash",
    wireSha256: "wire-hash",
    pendingBindingSha256: "binding-hash",
    checkoutRoot: "/checkout",
    stateRoot: "/state",
    repeatable: true,
    allowRepeat: false,
    existing: null,
    ...overrides,
  };
}

describe("HXtk canonical fence", () => {
  test("canonical JSON sorts keys and renders BigInt as the journal does", () => {
    expect(canonicalJson({ z: 2, nested: { b: 3n, a: 1 }, a: 1 })).toBe(
      '{"a":1,"nested":{"a":1,"b":"3"},"z":2}',
    );
  });

  test("binding-hash mismatch refuses reconciliation while volatile fields do not change the hash", () => {
    const base = {
      schema: "schema",
      step: "claim",
      canonicalStateRoot: "/state",
      before: { amount: 3n },
      expectedPostState: { payout: "10" },
      transaction: {
        expectedSignature: "signature",
        messageSha256: "message-hash",
        wireSha256: "wire-hash",
      },
    };
    const binding = pendingBindingSha256(base);
    const pending = {
      ...base,
      pendingBindingSha256: binding,
      verdict: "SIGNED_SIMULATION_PASS_PENDING_SEND",
      sent: false,
      signed: true,
      broadcast: false,
    };
    const state = {
      status: "pending",
      stateRoot: "/state",
      pendingBindingSha256: binding,
      expectedSignature: "signature",
      messageSha256: "message-hash",
      wireSha256: "wire-hash",
    };
    expect(pendingBindingSha256({
      ...pending,
      verdict: "SEND_ATTEMPTED_PENDING_RECONCILE",
      sent: true,
      signed: false,
      broadcast: "attempted",
      broadcastPreMark: "before-raw-submission",
      signature: "different-status-signature",
      abortReason: "pre-send observation changed",
    })).toBe(binding);
    expect(() => assertPendingJournalBinding({
      pending,
      canonicalState: state,
      expectedSignature: "signature",
      messageSha256: "message-hash",
      wireSha256: "wire-hash",
    })).not.toThrow();
    expect(() => assertPendingJournalBinding({
      pending: { ...pending, before: { amount: 4n } },
      canonicalState: state,
      expectedSignature: "signature",
      messageSha256: "message-hash",
      wireSha256: "wire-hash",
    })).toThrow("RECONCILE_MISMATCH: pending journal does not match the canonical binding");
  });

  test("aborted pre-send state re-arms with the same or a new journal without allow-repeat", () => {
    const initial = beginCanonicalLegRecord(legInput());
    const aborted = { ...initial, status: "aborted-pre-send" };
    for (const journal of ["/tmp/harvest.json", "/tmp/new-harvest.json"]) {
      expect(() => assertCanonicalLegAvailable({
        step: "harvest",
        journal,
        existing: aborted,
        repeatable: true,
        allowRepeat: false,
      })).not.toThrow();
    }
    const rearmed = beginCanonicalLegRecord(legInput({ existing: aborted }));
    expect(rearmed.status).toBe("pending");
    expect(rearmed.attempt).toBe(2);
    expect(rearmed.history).toHaveLength(1);
    expect(rearmed.history?.[0]).toMatchObject({ status: "aborted-pre-send" });
  });

  test("allow-repeat accepts a new journal only for repeatable finalized legs", () => {
    const finalized = {
      ...beginCanonicalLegRecord(legInput()),
      status: "finalized",
      journal: "/tmp/old-harvest.json",
    };
    expect(() => assertCanonicalLegAvailable({
      step: "harvest",
      journal: "/tmp/new-harvest.json",
      existing: finalized,
      repeatable: true,
      allowRepeat: true,
      journalExists: false,
    })).not.toThrow();
    expect(() => assertCanonicalLegAvailable({
      step: "repair",
      journal: "/tmp/new-repair.json",
      existing: finalized,
      repeatable: false,
      allowRepeat: true,
      journalExists: false,
    })).toThrow("--allow-repeat is not accepted for one-shot HXtk leg repair");
    for (const status of ["pending", "attempted"] as const) {
      expect(() => assertCanonicalLegAvailable({
        step: "harvest",
        journal: "/tmp/new-harvest.json",
        existing: { ...finalized, status },
        repeatable: true,
        allowRepeat: true,
        journalExists: false,
      })).toThrow(`canonical replay fence is ${status}`);
    }
    expect(() => beginCanonicalLegRecord(legInput({
      existing: finalized,
      journal: "/tmp/new-harvest.json",
      allowRepeat: true,
      journalExists: false,
    }))).not.toThrow();
  });

  test("pending and attempted states can never start a new send", () => {
    for (const status of ["pending", "attempted"] as const) {
      expect(() => beginCanonicalLegRecord(legInput({
        existing: { ...beginCanonicalLegRecord(legInput()), status },
        journal: "/tmp/another.json",
      }))).toThrow(`canonical replay fence is ${status}`);
    }
  });

  test("state root is private, owned by the current user, and never created by read-only resolution", () => {
    const home = tempRoot();
    const missing = join(home, ".loyal");
    expect(resolveCanonicalStateRoot({
      vault: "HXtk",
      homeDir: home,
      uid,
      create: false,
    })).toBe(join(missing, "hxtk-reset", "HXtk"));
    expect(existsSync(missing)).toBe(false);

    const secure = join(home, "secure");
    mkdirSync(secure, { mode: 0o700 });
    const resolved = resolveCanonicalStateRoot({
      vault: "HXtk",
      homeDir: secure,
      uid,
      create: true,
    });
    expect(resolved).toBe(join(secure, ".loyal", "hxtk-reset", "HXtk"));
    const leaf = resolved;
    chmodSync(leaf, 0o770);
    expect(() => resolveCanonicalStateRoot({
      vault: "HXtk",
      homeDir: secure,
      uid,
      create: false,
    })).toThrow("group/other-accessible");
    chmodSync(leaf, 0o700);
    chmodSync(secure, 0o702);
    expect(() => resolveCanonicalStateRoot({
      vault: "HXtk",
      homeDir: secure,
      uid,
      create: false,
    })).toThrow("home component");
    chmodSync(secure, 0o700);
    expect(() => resolveCanonicalStateRoot({
      vault: "HXtk",
      homeDir: secure,
      uid: uid + 1,
      create: false,
    })).toThrow("not owned by the current uid");
  });

  test("state-root component symlinks are refused", () => {
    const home = tempRoot();
    const loyal = join(home, ".loyal");
    const target = join(home, "target");
    mkdirSync(target, { mode: 0o700 });
    symlinkSync(target, loyal);
    expect(() => resolveCanonicalStateRoot({
      vault: "HXtk",
      homeDir: home,
      uid,
      create: true,
    })).toThrow("not a real directory");
  });

  test("failed-send, pre-mark, and abort rewrites preserve the immutable binding", () => {
    const root = tempRoot();
    const path = join(root, "harvest.json.pending");
    const base = {
      schema: "schema",
      step: "harvest",
      canonicalStateRoot: "/state",
      before: { amount: 3 },
      expectedPayoutRaw: "10",
      transaction: {
        expectedSignature: "signature",
        messageSha256: "message-hash",
        wireSha256: "wire-hash",
      },
    };
    writeFileSync(path, `${JSON.stringify({ ...base, pendingBindingSha256: pendingBindingSha256(base) })}\n`, { mode: 0o600 });
    rewritePendingStatus(path, {
      verdict: "SEND_ATTEMPTED_PENDING_RECONCILE",
      sent: false,
      signed: true,
      broadcast: "attempted",
      signature: "signature",
      sendStatus: { verdict: "SEND_ATTEMPTED_PENDING_RECONCILE", attemptedAtUnixMs: 1, signature: "signature" },
    });
    rewritePendingStatus(path, {
      verdict: "SEND_ATTEMPTED_PENDING_RECONCILE",
      sendStatus: { verdict: "SEND_ATTEMPTED_PENDING_RECONCILE", sendError: "ambiguous", submission: null, attemptedAtUnixMs: 1, signature: "signature" },
    });
    const pending = readBoundPending(path).record;
    expect(pending.expectedPayoutRaw).toBe("10");
    expect(pending.before).toEqual({ amount: 3 });
    expect((pending.sendStatus as Record<string, unknown>).sendError).toBe("ambiguous");
    expect(() => rewritePendingStatus(path, { expectedPayoutRaw: "11" })).toThrow("non-volatile field expectedPayoutRaw");
    expect(() => rewritePendingStatus(path, { before: { amount: 4 } })).toThrow("non-volatile field before");
    expect(() => rewritePendingStatus(path, { transaction: { messageSha256: "changed" } })).toThrow("non-volatile field transaction");
    rewritePendingStatus(path, {
      verdict: "ABORTED_PRE_SEND",
      sent: false,
      signed: true,
      broadcast: false,
      abortReason: "state changed",
      sendStatus: { verdict: "ABORTED_PRE_SEND" },
    });
    expect(readBoundPending(path).record.abortReason).toBe("state changed");
  });

  test("exclusive leg claims allow one claimant and support only dead-pid breaking", () => {
    const home = tempRoot();
    const stateRoot = resolveCanonicalStateRoot({ vault: "HXtk", homeDir: home, uid, create: true });
    const first = acquireCanonicalLegClaim({ stateRoot, step: "repair", journal: "/tmp/one.json" });
    expect(() => acquireCanonicalLegClaim({ stateRoot, step: "repair", journal: "/tmp/two.json" }))
      .toThrow("another hxtk-reset process holds the repair claim: pid");
    expect(() => acquireCanonicalLegClaim({ stateRoot, step: "repair", journal: "/tmp/two.json", breakClaim: true }))
      .toThrow("another hxtk-reset process holds the repair claim: pid");
    releaseCanonicalLegClaim(first);

    const stale = join(stateRoot, "repair.claim");
    const staleToken = "a".repeat(32);
    const staleTokenPath = `${stale}.${staleToken}`;
    writeFileSync(staleTokenPath, `${JSON.stringify({
      pid: 99999999,
      startedAtUnixMs: 1,
      processStartTime: "dead-process",
      journal: "/tmp/old.json",
      hostname: hostname(),
      token: staleToken,
    })}\n`, { mode: 0o600 });
    linkSync(staleTokenPath, stale);
    const winner = acquireCanonicalLegClaim({
      stateRoot,
      step: "repair",
      journal: "/tmp/new.json",
      breakClaim: true,
    });
    expect(winner.journal).toBe("/tmp/new.json");
    expect(winner.tokenPath).toBe(`${winner.path}.${winner.token}`);
    expect(statSync(winner.path).ino).toBe(statSync(winner.tokenPath).ino);
    expect(JSON.parse(readFileSync(stale, "utf8"))).toMatchObject({ journal: "/tmp/new.json", token: winner.token });
    expect(() => acquireCanonicalLegClaim({
      stateRoot,
      step: "repair",
      journal: "/tmp/loser.json",
      breakClaim: true,
    })).toThrow("another hxtk-reset process holds the repair claim");
    expect(JSON.parse(readFileSync(stale, "utf8"))).toMatchObject({ journal: "/tmp/new.json", token: winner.token });
    releaseCanonicalLegClaim(winner);
  });

  test("foreign claim token is never unlinked by release", () => {
    const home = tempRoot();
    const stateRoot = resolveCanonicalStateRoot({ vault: "HXtk", homeDir: home, uid, create: true });
    const claim = acquireCanonicalLegClaim({ stateRoot, step: "repair", journal: "/tmp/owned.json" });
    const foreignToken = "f".repeat(32);
    const foreignTokenPath = `${claim.path}.${foreignToken}`;
    writeFileSync(foreignTokenPath, `${JSON.stringify({
      pid: process.pid,
      startedAtUnixMs: Date.now(),
      processStartTime: "replacement-live-claim",
      journal: "/tmp/foreign.json",
      hostname: hostname(),
      token: foreignToken,
    })}\n`, { mode: 0o600 });
    unlinkSync(claim.path);
    linkSync(foreignTokenPath, claim.path);
    releaseCanonicalLegClaim(claim);
    expect(existsSync(claim.path)).toBe(true);
    expect(JSON.parse(readFileSync(claim.path, "utf8")).journal).toBe("/tmp/foreign.json");
  });

  test("canonical state CAS rejects a stale generation", () => {
    const root = tempRoot();
    const path = join(root, "repair.state");
    const first = writeCanonicalStateCas(path, {
      schema: "loyal-voltr-hxtk-reset-state/v2",
      step: "repair",
      status: "pending",
    }, null);
    expect(canonicalStateGeneration(first)).toBe(0);
    expect(readCanonicalState(path).record).toEqual(first);
    expect(readFileSync(`${path}.gen-0`, "utf8")).toBe(readFileSync(path, "utf8"));
    const second = writeCanonicalStateCas(path, { ...first, status: "attempted" }, 0);
    expect(canonicalStateGeneration(second)).toBe(1);
    expect(readFileSync(`${path}.gen-1`, "utf8")).toBe(readFileSync(path, "utf8"));
    expect(() => writeCanonicalStateCas(path, { ...second, status: "finalized" }, 0))
      .toThrow("STATE_GENERATION_CONFLICT");
    expect(canonicalStateGeneration(JSON.parse(readFileSync(path, "utf8")))).toBe(1);
    const tampered = `${path}.tampered`;
    writeFileSync(tampered, `${JSON.stringify({ ...second, status: "pending" })}\n`, { mode: 0o600 });
    renameSync(tampered, path);
    expect(() => readCanonicalState(path)).toThrow("STATE_GENERATION_CONFLICT");
  });

  test("finalized journal hash mismatch refuses the later-leg load", () => {
    const root = tempRoot();
    const journal = join(root, "claim.json");
    writeFileSync(journal, "{\"verdict\":\"FINALIZED_RECONCILED\"}\n", { mode: 0o600 });
    const state = {
      status: "finalized",
      journal,
      finalizedJournalSha256: finalizedJournalSha256(journal),
    };
    expect(() => assertFinalizedJournalBinding({
      step: "claim",
      journal,
      canonicalState: state,
      journalSha256: finalizedJournalSha256(journal),
    })).not.toThrow();
    expect(() => assertFinalizedJournalBinding({
      step: "claim",
      journal,
      canonicalState: null,
      journalSha256: finalizedJournalSha256(journal),
    })).toThrow("RECONCILE_MISMATCH: finalized claim journal is not bound to the canonical fence");
    writeFileSync(journal, "{\"verdict\":\"edited\"}\n", { mode: 0o600 });
    expect(() => assertFinalizedJournalBinding({
      step: "claim",
      journal,
      canonicalState: state,
      journalSha256: finalizedJournalSha256(journal),
    })).toThrow("RECONCILE_MISMATCH: finalized claim journal is not bound to the canonical fence");
  });

  test("post-finalization exceptions map only attempted/finalized repair states", () => {
    expect(repairPostFinalizationStatus("finalized")).toBe("REPAIR_FINALIZED_POLICY_STILL_PRESENT");
    expect(repairPostFinalizationStatus("attempted")).toBe("REPAIR_ATTEMPTED_POLICY_PRESENT");
    for (const status of [null, "pending", "aborted-pre-send", undefined]) {
      expect(repairPostFinalizationStatus(status)).toBeNull();
    }
  });
});
