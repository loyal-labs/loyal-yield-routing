import { describe, expect, test } from "bun:test";

import {
  runDurableAutodepositExecution,
  type AttemptOutcome,
  type DurableAutodepositDependencies,
  type DurableAutodepositExecution,
  type DurableAutodepositHooks,
  type DurableAutodepositStore,
  type LandedAttemptEvidence,
  type LeaseFence,
  type PersistedAttempt,
  type PreparedSignedAttempt,
  type PullChainEvidence,
} from "./durable-autodeposit-execution";

const LEASE: LeaseFence = {
  cluster: "mainnet-beta",
  vaultPubkey: "vault",
  ownerToken: "owner",
  fence: BigInt(7),
};

class MemoryStore implements DurableAutodepositStore {
  execution: DurableAutodepositExecution | null = null;
  attempts: PersistedAttempt[] = [];
  broadcastWrites = 0;
  completionWrites = 0;
  failCompletionOnce = false;
  validFence = LEASE.fence;

  async loadExecution() {
    return this.execution;
  }

  async assertLease(lease: LeaseFence) {
    if (lease.fence !== this.validFence) {
      throw new Error("stale vault operation lease");
    }
  }

  private persistAttempt(operationKind: "pull" | "top_up", prepared: PreparedSignedAttempt): PersistedAttempt {
    const attempt: PersistedAttempt = {
      ...prepared,
      id: String(this.attempts.length + 1),
      executionId: "execution-1",
      operationKind,
      attemptNumber: this.attempts.filter((candidate) => candidate.operationKind === operationKind).length + 1,
      classification: "prepared",
      broadcastAt: null,
    };
    this.attempts.push(attempt);
    return attempt;
  }

  async createWithPreparedPull(lease: LeaseFence, prepared: PreparedSignedAttempt) {
    await this.assertLease(lease);
    if (this.execution) return this.execution;
    const attempt = this.persistAttempt("pull", prepared);
    this.execution = {
      id: "execution-1",
      lifecycleState: "pull_confirmation_pending",
      requestedAmountRaw: BigInt(100),
      confirmedPullAmountRaw: null,
      reservedAmountRaw: BigInt(0),
      activeAttempt: attempt,
      pullSignature: attempt.signature,
      successfulTopUpSignature: null,
      topUpReserve: "reserve",
      topUpMarket: "market",
      topUpLiquidityMint: "mint",
    };
    return this.execution;
  }

  async appendPreparedTopUp(lease: LeaseFence, executionId: string, prepared: PreparedSignedAttempt) {
    await this.assertLease(lease);
    if (!this.execution || this.execution.id !== executionId || this.execution.lifecycleState !== "deposit_pending") {
      throw new Error("top-up prepare CAS rejected");
    }
    const attempt = this.persistAttempt("top_up", prepared);
    this.execution.lifecycleState = "deposit_confirmation_pending";
    this.execution.activeAttempt = attempt;
    return this.execution;
  }

  async recordBroadcast(lease: LeaseFence, attempt: PersistedAttempt) {
    await this.assertLease(lease);
    attempt.broadcastAt = new Date(0).toISOString();
    this.broadcastWrites += 1;
  }

  async recordNonLandedAttempt(
    lease: LeaseFence,
    execution: DurableAutodepositExecution,
    attempt: PersistedAttempt,
    outcome: Exclude<AttemptOutcome, LandedAttemptEvidence>
  ) {
    await this.assertLease(lease);
    attempt.classification = outcome.classification;
    if (outcome.classification === "unknown") {
      execution.lifecycleState = "needs_reconciliation";
      execution.activeAttempt = attempt;
    } else {
      execution.lifecycleState = attempt.operationKind === "top_up" ? "deposit_pending" : "needs_reconciliation";
      execution.activeAttempt = null;
    }
    return execution;
  }

  async recordConfirmedPull(
    lease: LeaseFence,
    execution: DurableAutodepositExecution,
    attempt: PersistedAttempt,
    evidence: PullChainEvidence
  ) {
    await this.assertLease(lease);
    attempt.classification = "landed";
    execution.lifecycleState = "deposit_pending";
    execution.confirmedPullAmountRaw = evidence.destinationCreditRaw;
    execution.reservedAmountRaw = evidence.destinationCreditRaw;
    execution.pullSignature = attempt.signature;
    execution.activeAttempt = null;
    return execution;
  }

  async recordConfirmedTopUp(lease: LeaseFence, execution: DurableAutodepositExecution, attempt: PersistedAttempt) {
    await this.assertLease(lease);
    attempt.classification = "landed";
    execution.lifecycleState = "deposit_confirmed";
    execution.reservedAmountRaw = BigInt(0);
    execution.successfulTopUpSignature = attempt.signature;
    execution.activeAttempt = null;
    return execution;
  }

  async persistCompletion(lease: LeaseFence, execution: DurableAutodepositExecution) {
    await this.assertLease(lease);
    this.completionWrites += 1;
    if (this.failCompletionOnce) {
      this.failCompletionOnce = false;
      throw new Error("injected application persistence failure");
    }
    execution.lifecycleState = "completed";
    return execution;
  }
}

function prepared(signature: string): PreparedSignedAttempt {
  return {
    signature,
    blockhash: `blockhash-${signature}`,
    lastValidBlockHeight: BigInt(123),
    signedTransactionBase64: `signed-${signature}`,
  };
}

function createHarness(options?: { pullAmountRaw?: bigint; hooks?: DurableAutodepositHooks }) {
  const store = new MemoryStore();
  const outcomes = new Map<string, AttemptOutcome>();
  const broadcasts: string[] = [];
  const topUpAmounts: bigint[] = [];
  let pullBuilds = 0;
  let topUpBuilds = 0;
  let confirmationTimeoutSignature: string | null = null;
  let blockhashNotFoundSignature: string | null = null;
  let expiredAfterBroadcastSignature: string | null = null;

  const dependencies: DurableAutodepositDependencies = {
    store,
    lease: LEASE,
    hooks: options?.hooks,
    preparePull: async () => prepared(`pull-${++pullBuilds}`),
    prepareTopUp: async (amountRaw) => {
      topUpAmounts.push(amountRaw);
      return prepared(`top-up-${++topUpBuilds}`);
    },
    broadcastExactSignedTransaction: async (attempt) => {
      broadcasts.push(attempt.signature);
      outcomes.set(attempt.signature, {
        classification: "landed",
        confirmedSlot: BigInt(42),
      });
      if (attempt.signature === expiredAfterBroadcastSignature) {
        outcomes.set(attempt.signature, {
          classification: "expired_not_landed",
          error: "last valid block height passed with no signature status",
        });
        return attempt.signature;
      }
      if (attempt.signature === blockhashNotFoundSignature) {
        outcomes.set(attempt.signature, {
          classification: "expired_not_landed",
          error: "BlockhashNotFound",
        });
        throw new Error("BlockhashNotFound");
      }
      return attempt.signature;
    },
    reconcileAttempt: async (attempt, waitForConfirmation) => {
      const outcome = outcomes.get(attempt.signature) ?? {
        classification: "unknown" as const,
        error: null,
      };
      if (
        waitForConfirmation &&
        attempt.signature === confirmationTimeoutSignature &&
        outcome.classification === "landed"
      ) {
        confirmationTimeoutSignature = null;
        return { classification: "unknown", error: "confirmation timeout" };
      }
      return outcome;
    },
    readConfirmedPullEvidence: async (_attempt, landed) => {
      const amount = options?.pullAmountRaw ?? BigInt(73);
      return {
        confirmedSlot: landed.confirmedSlot,
        sourcePreBalanceRaw: BigInt(1000),
        sourcePostBalanceRaw: BigInt(1000) - amount,
        destinationPreBalanceRaw: BigInt(11),
        destinationPostBalanceRaw: BigInt(11) + amount,
        destinationCreditRaw: amount,
      };
    },
    readConfirmedTopUpEvidence: async (_attempt, landed) => ({
      confirmedSlot: landed.confirmedSlot,
      vaultDebitRaw: store.execution?.confirmedPullAmountRaw ?? BigInt(0),
    }),
  };

  return {
    store,
    outcomes,
    broadcasts,
    topUpAmounts,
    dependencies,
    setConfirmationTimeout(signature: string) {
      confirmationTimeoutSignature = signature;
    },
    setBlockhashNotFound(signature: string) {
      blockhashNotFoundSignature = signature;
    },
    setExpiredAfterBroadcast(signature: string) {
      expiredAfterBroadcastSignature = signature;
    },
  };
}

async function runUntilTerminal(dependencies: DurableAutodepositDependencies, attempts = 8) {
  let execution: DurableAutodepositExecution | null = null;
  for (let index = 0; index < attempts; index += 1) {
    execution = await runDurableAutodepositExecution(dependencies);
    if (
      execution.lifecycleState === "completed" ||
      (execution.lifecycleState === "needs_reconciliation" && !execution.activeAttempt)
    ) {
      return execution;
    }
  }
  throw new Error(`execution did not settle: ${execution?.lifecycleState}`);
}

function crashOnce(message: string) {
  let crashed = false;
  return () => {
    if (!crashed) {
      crashed = true;
      throw new Error(message);
    }
  };
}

describe("durable autodeposit execution", () => {
  const crashBoundaries: Array<keyof DurableAutodepositHooks> = [
    "after_pull_constructed",
    "after_pull_attempt_persisted",
    "after_pull_broadcast",
    "after_pull_chain_landed",
    "after_pull_confirmed_persisted",
    "after_top_up_constructed",
    "after_top_up_attempt_persisted",
    "after_top_up_broadcast",
    "after_top_up_chain_landed",
    "after_top_up_confirmed_persisted",
    "before_completion_persistence",
  ];

  for (const boundary of crashBoundaries) {
    test(`recovers a crash at ${boundary} without a second pull`, async () => {
      const harness = createHarness({
        hooks: { [boundary]: crashOnce(`crash at ${boundary}`) },
      });

      await expect(runDurableAutodepositExecution(harness.dependencies)).rejects.toThrow(`crash at ${boundary}`);
      const completed = await runUntilTerminal(harness.dependencies);

      expect(completed.lifecycleState).toBe("completed");
      expect(harness.broadcasts.filter((signature) => signature.startsWith("pull-"))).toHaveLength(1);
      expect(
        harness.store.attempts.filter(
          (attempt) => attempt.operationKind === "pull" && attempt.classification === "landed"
        )
      ).toHaveLength(1);
    });
  }

  test("uses confirmed pull evidence instead of the requested amount", async () => {
    const harness = createHarness({ pullAmountRaw: BigInt(61) });
    const completed = await runUntilTerminal(harness.dependencies);

    expect(completed.requestedAmountRaw).toBe(BigInt(100));
    expect(completed.confirmedPullAmountRaw).toBe(BigInt(61));
    expect(harness.topUpAmounts).toEqual([BigInt(61)]);
  });

  test("recognizes a landed confirmation timeout on recovery", async () => {
    const harness = createHarness();
    harness.setConfirmationTimeout("pull-1");

    const timedOut = await runDurableAutodepositExecution(harness.dependencies);
    expect(timedOut.lifecycleState).toBe("needs_reconciliation");
    expect(timedOut.activeAttempt?.signature).toBe("pull-1");

    const completed = await runUntilTerminal(harness.dependencies);
    expect(completed.lifecycleState).toBe("completed");
    expect(harness.broadcasts.filter((signature) => signature === "pull-1")).toHaveLength(1);
  });

  test("replaces only a definitively expired top-up and retains its evidence", async () => {
    const harness = createHarness();
    harness.setExpiredAfterBroadcast("top-up-1");

    const first = await runDurableAutodepositExecution(harness.dependencies);
    expect(first.lifecycleState).toBe("deposit_pending");
    expect(harness.store.attempts[1]?.classification).toBe("expired_not_landed");

    const completed = await runUntilTerminal(harness.dependencies);
    expect(completed.lifecycleState).toBe("completed");
    expect(harness.store.attempts.map((attempt) => attempt.classification)).toEqual([
      "landed",
      "expired_not_landed",
      "landed",
    ]);
  });

  test("BlockhashNotFound retries a fresh top-up without re-pulling", async () => {
    const harness = createHarness();
    harness.setBlockhashNotFound("top-up-1");

    const first = await runDurableAutodepositExecution(harness.dependencies);
    expect(first.lifecycleState).toBe("deposit_pending");
    expect(harness.store.attempts[1]?.classification).toBe("expired_not_landed");

    const completed = await runUntilTerminal(harness.dependencies);
    expect(completed.lifecycleState).toBe("completed");
    expect(harness.broadcasts.filter((signature) => signature.startsWith("pull-"))).toEqual(["pull-1"]);
    expect(harness.broadcasts.filter((signature) => signature.startsWith("top-up-"))).toEqual(["top-up-1", "top-up-2"]);
    expect(harness.store.attempts.map((attempt) => attempt.classification)).toEqual([
      "landed",
      "expired_not_landed",
      "landed",
    ]);
  });

  test("retries application persistence only after chain success", async () => {
    const harness = createHarness();
    harness.store.failCompletionOnce = true;

    await expect(runDurableAutodepositExecution(harness.dependencies)).rejects.toThrow(
      "injected application persistence failure"
    );
    expect(harness.store.execution?.lifecycleState).toBe("deposit_confirmed");
    const broadcastsAfterChainSuccess = [...harness.broadcasts];

    const completed = await runDurableAutodepositExecution(harness.dependencies);
    expect(completed.lifecycleState).toBe("completed");
    expect(harness.broadcasts).toEqual(broadcastsAfterChainSuccess);
    expect(harness.store.completionWrites).toBe(2);
  });

  test("a stale lease fence cannot append or broadcast a competing top-up", async () => {
    const harness = createHarness();
    harness.dependencies.hooks = {
      after_pull_confirmed_persisted: crashOnce("stop after pull persistence"),
    };
    await expect(runDurableAutodepositExecution(harness.dependencies)).rejects.toThrow("stop after pull persistence");
    expect(harness.store.execution?.lifecycleState).toBe("deposit_pending");
    const broadcastsBeforeStaleHolder = [...harness.broadcasts];

    harness.store.validFence = BigInt(8);
    await expect(runDurableAutodepositExecution(harness.dependencies)).rejects.toThrow("stale vault operation lease");
    expect(harness.broadcasts).toEqual(broadcastsBeforeStaleHolder);
    expect(harness.store.attempts.filter((attempt) => attempt.operationKind === "top_up")).toHaveLength(0);
  });
});
