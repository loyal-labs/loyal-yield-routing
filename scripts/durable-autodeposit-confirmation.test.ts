import { describe, expect, test } from "bun:test";

import {
  attemptAllowsSafeRequeue,
  attemptHoldsClaim,
  operationalAlertForAttempt,
  settleDurableAutodepositAttempt,
  type AttemptObservation,
  type DurableAutodepositAttempt,
} from "./durable-autodeposit-confirmation";

function attempt(
  overrides: Partial<DurableAutodepositAttempt> = {}
): DurableAutodepositAttempt {
  return {
    id: "1",
    claimToken: "claim-1",
    operationKind: "pull",
    executionId: null,
    amountRaw: BigInt(10),
    sourcePreBalanceRaw: BigInt(110),
    destinationPreBalanceRaw: BigInt(20),
    signature: "signature-1",
    signedTransactionBase64: "ZXhhY3Qtd2lyZS1pbWFnZQ==",
    signedTransactionSha256: "sha256",
    blockhash: "blockhash-1",
    lastValidBlockHeight: BigInt(100),
    state: "prepared",
    broadcastCount: 0,
    confirmedSlot: null,
    ...overrides,
  };
}

function harness(observations: AttemptObservation[]) {
  const broadcasts: Array<{
    signature: string;
    bytes: string;
  }> = [];
  const transitions: string[] = [];
  let observationIndex = 0;
  return {
    broadcasts,
    transitions,
    dependencies: {
      observe: async () =>
        observations[
          Math.min(observationIndex++, observations.length - 1)
        ]!,
      broadcastExact: async (candidate: DurableAutodepositAttempt) => {
        broadcasts.push({
          signature: candidate.signature,
          bytes: candidate.signedTransactionBase64,
        });
        return candidate.signature;
      },
      recordBroadcast: async (candidate: DurableAutodepositAttempt) => {
        transitions.push("submitted");
        return {
          ...candidate,
          state: "submitted" as const,
          broadcastCount: candidate.broadcastCount + 1,
        };
      },
      recordObservation: async (
        candidate: DurableAutodepositAttempt,
        observation: AttemptObservation
      ) => {
        transitions.push(observation.state);
        return {
          ...candidate,
          state: observation.state,
          confirmedSlot: observation.confirmedSlot,
        };
      },
    },
  };
}

const UNKNOWN: AttemptObservation = {
  state: "unknown",
  confirmedSlot: null,
  error: null,
};

describe("durable autodeposit confirmation", () => {
  test("records a confirmed signature exactly once without broadcasting a replacement", async () => {
    const confirmed: AttemptObservation = {
      state: "confirmed",
      confirmedSlot: BigInt(42),
      error: null,
    };
    const testHarness = harness([confirmed]);

    const settled = await settleDurableAutodepositAttempt({
      attempt: attempt({ state: "submitted", broadcastCount: 1 }),
      dependencies: testHarness.dependencies,
    });

    expect(settled.attempt.state).toBe("confirmed");
    expect(settled.attempt.confirmedSlot).toBe(BigInt(42));
    expect(attemptHoldsClaim(settled.attempt.state)).toBe(true);
    expect(testHarness.broadcasts).toEqual([]);
    expect(testHarness.transitions).toEqual(["confirmed"]);
  });

  test("rebroadcasts only the immutable bytes and signature while unresolved", async () => {
    const testHarness = harness([UNKNOWN, UNKNOWN]);
    const persisted = attempt({ state: "unknown", broadcastCount: 1 });

    const settled = await settleDurableAutodepositAttempt({
      attempt: persisted,
      dependencies: testHarness.dependencies,
    });

    expect(settled.attempt.state).toBe("unknown");
    expect(testHarness.broadcasts).toEqual([
      {
        signature: persisted.signature,
        bytes: persisted.signedTransactionBase64,
      },
    ]);
    expect(testHarness.transitions).toEqual(["submitted", "unknown"]);
  });

  test("does not broadcast an explicitly failed transaction", async () => {
    const failed: AttemptObservation = {
      state: "failed",
      confirmedSlot: null,
      error: { InstructionError: [0, "Custom"] },
    };
    const testHarness = harness([failed]);
    const settled = await settleDurableAutodepositAttempt({
      attempt: attempt({ state: "submitted" }),
      dependencies: testHarness.dependencies,
    });

    expect(settled.attempt.state).toBe("failed");
    expect(testHarness.broadcasts).toEqual([]);
    expect(attemptAllowsSafeRequeue(settled.attempt.state)).toBe(true);
  });

  test("safely requeues only a missing signature whose blockhash expired", async () => {
    const expired: AttemptObservation = {
      state: "expired",
      confirmedSlot: null,
      error: null,
    };
    const testHarness = harness([expired]);
    const settled = await settleDurableAutodepositAttempt({
      attempt: attempt({ state: "submitted" }),
      dependencies: testHarness.dependencies,
    });

    expect(settled.attempt.state).toBe("expired");
    expect(attemptAllowsSafeRequeue(settled.attempt.state)).toBe(true);
    expect(attemptHoldsClaim(settled.attempt.state)).toBe(false);
    expect(operationalAlertForAttempt(settled.attempt.state)).toBeNull();
  });

  test("holds and alerts an effect that remains ambiguous after expiry", async () => {
    const ambiguous: AttemptObservation = {
      state: "ambiguous",
      confirmedSlot: null,
      error: "processed signature did not reach confirmation before expiry",
    };
    const testHarness = harness([ambiguous]);
    const settled = await settleDurableAutodepositAttempt({
      attempt: attempt({ state: "submitted" }),
      dependencies: testHarness.dependencies,
    });

    expect(settled.attempt.state).toBe("ambiguous");
    expect(attemptHoldsClaim(settled.attempt.state)).toBe(true);
    expect(attemptAllowsSafeRequeue(settled.attempt.state)).toBe(false);
    expect(operationalAlertForAttempt(settled.attempt.state)).toEqual({
      code: "autodeposit_transaction_effect_ambiguous",
      operation: "reconcile_autodeposit_transaction",
      summary:
        "autodeposit transaction effect remains ambiguous after blockhash expiry",
      retryable: false,
      recoveryRequired: true,
    });
  });

  test("a confirmation timeout remains recoverable and does not page", async () => {
    const testHarness = harness([UNKNOWN, UNKNOWN]);
    const settled = await settleDurableAutodepositAttempt({
      attempt: attempt(),
      dependencies: testHarness.dependencies,
    });

    expect(settled.attempt.state).toBe("unknown");
    expect(attemptHoldsClaim(settled.attempt.state)).toBe(true);
    expect(operationalAlertForAttempt(settled.attempt.state)).toBeNull();
  });

  test("top-up recovery retains the confirmed pull execution identity", async () => {
    const testHarness = harness([UNKNOWN, UNKNOWN]);
    const topUp = attempt({
      id: "2",
      operationKind: "top_up",
      executionId: "execution-9",
      signature: "top-up-signature",
      state: "unknown",
    });

    const settled = await settleDurableAutodepositAttempt({
      attempt: topUp,
      dependencies: testHarness.dependencies,
    });

    expect(settled.attempt.operationKind).toBe("top_up");
    expect(settled.attempt.executionId).toBe("execution-9");
    expect(testHarness.broadcasts).toEqual([
      {
        signature: "top-up-signature",
        bytes: topUp.signedTransactionBase64,
      },
    ]);
  });
});
