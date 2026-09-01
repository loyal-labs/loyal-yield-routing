import { strict as assert } from "node:assert";
import { test } from "node:test";

import {
  DOWNSTREAM_ROLLBACK_MUTATION,
  downstreamRollbackLogProof,
  failedSimulationOverlayAccepted,
} from "./rwa-multiply-rollback-log-proof.js";

const arm = "Arm111111111111111111111111111111111111111";
const downstream = "Voltr111111111111111111111111111111111111";

test("accepts only Arm success followed by the dedicated downstream failure", () => {
  assert.deepEqual(downstreamRollbackLogProof([
    "Program Squads invoke [1]",
    `Program ${arm} invoke [2]`,
    `Program ${arm} success`,
    `Program ${downstream} invoke [2]`,
    `Program ${downstream} failed: custom program error: 0x1`,
    "Program Squads failed: custom program error: 0x1",
  ], arm, downstream), {
    armInvokeLogIndex: 1,
    armSuccessLogIndex: 2,
    downstreamInvokeLogIndex: 3,
    downstreamFailureLogIndex: 4,
  });
});

test("rejects missing Arm success, wrong downstream failure, and reversed order", () => {
  assert.equal(downstreamRollbackLogProof([
    `Program ${arm} invoke [2]`,
    `Program ${arm} failed: custom program error: 0x1`,
    `Program ${downstream} invoke [2]`,
    `Program ${downstream} failed: custom program error: 0x1`,
  ], arm, downstream), null);
  assert.equal(downstreamRollbackLogProof([
    `Program ${arm} invoke [2]`,
    `Program ${arm} success`,
    "Program Wrong11111111111111111111111111111111111 invoke [2]",
    "Program Wrong11111111111111111111111111111111111 failed: custom program error: 0x1",
  ], arm, downstream), null);
  assert.equal(downstreamRollbackLogProof([
    `Program ${downstream} invoke [2]`,
    `Program ${downstream} failed: custom program error: 0x1`,
    `Program ${arm} invoke [2]`,
    `Program ${arm} success`,
  ], arm, downstream), null);
});

const protectedAccounts = ["config", "ticket", "receipt"];

test("ordinary rejection cannot use an all-null simulation overlay", () => {
  assert.equal(failedSimulationOverlayAccepted({
    mutationName: "wrong_policy",
    inspectedAddresses: protectedAccounts,
    postAccountsAvailable: false,
    nullAddresses: protectedAccounts,
    changedAddresses: [],
    downstreamRollbackProven: true,
  }), false);
});

test("dedicated downstream rollback can use an all-null simulation overlay", () => {
  assert.equal(failedSimulationOverlayAccepted({
    mutationName: DOWNSTREAM_ROLLBACK_MUTATION,
    inspectedAddresses: protectedAccounts,
    postAccountsAvailable: false,
    nullAddresses: protectedAccounts,
    changedAddresses: [],
    downstreamRollbackProven: true,
  }), true);
});

test("dedicated downstream rollback rejects a partial-null simulation overlay", () => {
  assert.equal(failedSimulationOverlayAccepted({
    mutationName: DOWNSTREAM_ROLLBACK_MUTATION,
    inspectedAddresses: protectedAccounts,
    postAccountsAvailable: false,
    nullAddresses: [protectedAccounts[0]!],
    changedAddresses: [],
    downstreamRollbackProven: true,
  }), false);
});
