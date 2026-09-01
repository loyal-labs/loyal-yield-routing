import assert from "node:assert/strict";
import { test } from "node:test";

import {
  selectCustomPolicyMutation,
  type CustomPolicyArtifact,
  type CustomPolicyVerificationRow,
} from "./rwa-multiply-custom.js";

const operations = ["allocation", "nav-refresh", "stage-withdrawal", "withdraw"] as const;

function artifact(): CustomPolicyArtifact {
  return {
    schema: "loyal-voltr-custom-policy-artifact/v2",
    verdict: "VOLTR_CUSTOM_POLICY_ARTIFACT_COMPILED_NOT_DEPLOYED",
    physicalPolicyCount: 4,
    deploymentReady: false,
    sourceSha256: "0".repeat(64),
    policies: operations.map((operation, index) => {
      const seed = String(53 + index);
      const policy = `policy-${seed}`;
      const base = {
        programId: "squads",
        accounts: [],
        dataBase64: operation,
      } as const;
      return {
        operation,
        seed,
        policy,
        constraintIndex: 0 as const,
        constraintIndices: operation === "stage-withdrawal" ? [0] : [0, 1],
        createInstruction: { ...base, dataBase64: `create-${operation}` },
        updateInstruction: { ...base, dataBase64: `update-${operation}` },
      };
    }),
  };
}

function exactRows(value: CustomPolicyArtifact): CustomPolicyVerificationRow[] {
  return value.policies.map(({ operation, seed, policy }) => ({
    operation,
    seed,
    policy,
    pass: true,
    updateEligible: true,
    dataSha256: "1".repeat(64),
  }));
}

test("exact installed custom policies are a no-op", () => {
  const value = artifact();
  assert.deepEqual(selectCustomPolicyMutation({
    policySeedBefore: 56n,
    artifact: value,
    rows: exactRows(value),
  }), { kind: "noop" });
});

test("inexact policy with an exact authority boundary updates in place", () => {
  const value = artifact();
  const rows = exactRows(value);
  rows[1] = { ...rows[1]!, pass: false, reason: "inexact policy payload" };
  const selected = selectCustomPolicyMutation({ policySeedBefore: 56n, artifact: value, rows });
  assert.equal(selected.kind, "update");
  if (selected.kind !== "update") throw new Error("expected update");
  assert.equal(selected.target.seed, "54");
  assert.equal(selected.instruction.dataBase64, "update-nav-refresh");
});

test("absent next seed creates while absent historical seed fails closed", () => {
  const value = artifact();
  const rows = exactRows(value);
  const { dataSha256: _, ...row } = rows[0]!;
  rows[0] = { ...row, pass: false, updateEligible: false, reason: "absent" };
  const selected = selectCustomPolicyMutation({ policySeedBefore: 52n, artifact: value, rows });
  assert.equal(selected.kind, "create");
  if (selected.kind !== "create") throw new Error("expected create");
  assert.equal(selected.instruction.dataBase64, "create-allocation");
  assert.throws(() => selectCustomPolicyMutation({ policySeedBefore: 56n, artifact: value, rows }),
    /not the next finalized Settings seed/);
});

test("inexact policy with a mismatched authority boundary cannot update", () => {
  const value = artifact();
  const rows = exactRows(value);
  rows[2] = {
    ...rows[2]!,
    pass: false,
    updateEligible: false,
    reason: "authority boundary mismatch",
  };
  assert.throws(() => selectCustomPolicyMutation({ policySeedBefore: 56n, artifact: value, rows }),
    /failed its owner\/settings\/seed\/authority boundary/);
});
