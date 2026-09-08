import { chmodSync, existsSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { afterEach, describe, expect, test } from "bun:test";

import {
  assertCanonicalLegAvailable,
  assertFinalizedJournalBinding,
  assertPendingJournalBinding,
  beginCanonicalLegRecord,
  canonicalJson,
  finalizedJournalSha256,
  pendingBindingSha256,
  repairPostFinalizationStatus,
  resolveCanonicalStateRoot,
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
    const root = tempRoot();
    const missing = join(root, "not-created");
    expect(resolveCanonicalStateRoot({
      vault: "HXtk",
      env: { HXTK_RESET_STATE_ROOT: missing },
      uid,
      create: false,
    })).toBe(missing);
    expect(existsSync(missing)).toBe(false);

    const secure = join(root, "secure");
    const resolved = resolveCanonicalStateRoot({
      vault: "HXtk",
      env: { HXTK_RESET_STATE_ROOT: secure },
      uid,
      create: true,
    });
    expect(resolved).toBe(secure);
    chmodSync(secure, 0o770);
    expect(() => resolveCanonicalStateRoot({
      vault: "HXtk",
      env: { HXTK_RESET_STATE_ROOT: secure },
      uid,
      create: false,
    })).toThrow("group/other-accessible");
    chmodSync(secure, 0o700);
    expect(() => resolveCanonicalStateRoot({
      vault: "HXtk",
      env: { HXTK_RESET_STATE_ROOT: secure },
      uid: uid + 1,
      create: false,
    })).toThrow("not owned by the current uid");
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
