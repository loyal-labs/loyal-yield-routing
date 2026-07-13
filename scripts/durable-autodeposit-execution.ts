export type AutodepositOperationKind = "pull" | "top_up";

export type AttemptClassification = "prepared" | "landed" | "failed" | "expired_not_landed" | "unknown";

export type AutodepositLifecycleState =
  | "pull_confirmation_pending"
  | "deposit_pending"
  | "deposit_confirmation_pending"
  | "deposit_confirmed"
  | "completed"
  | "needs_reconciliation";

export type PreparedSignedAttempt = {
  signature: string;
  blockhash: string;
  lastValidBlockHeight: bigint;
  signedTransactionBase64: string;
};

export type PersistedAttempt = PreparedSignedAttempt & {
  id: string;
  executionId: string;
  operationKind: AutodepositOperationKind;
  attemptNumber: number;
  classification: AttemptClassification;
  broadcastAt: string | null;
};

export type DurableAutodepositExecution = {
  id: string;
  lifecycleState: AutodepositLifecycleState;
  requestedAmountRaw: bigint;
  confirmedPullAmountRaw: bigint | null;
  reservedAmountRaw: bigint;
  activeAttempt: PersistedAttempt | null;
  pullSignature: string | null;
  successfulTopUpSignature: string | null;
  topUpReserve: string;
  topUpMarket: string;
  topUpLiquidityMint: string;
};

export type LandedAttemptEvidence = {
  classification: "landed";
  confirmedSlot: bigint;
};

export type AttemptOutcome =
  | LandedAttemptEvidence
  | {
      classification: "failed" | "expired_not_landed" | "unknown";
      error: unknown | null;
    };

export type PullChainEvidence = {
  confirmedSlot: bigint;
  sourcePreBalanceRaw: bigint;
  sourcePostBalanceRaw: bigint;
  destinationPreBalanceRaw: bigint;
  destinationPostBalanceRaw: bigint;
  destinationCreditRaw: bigint;
};

export type TopUpChainEvidence = {
  confirmedSlot: bigint;
  vaultDebitRaw: bigint;
};

export type LeaseFence = {
  cluster: string;
  vaultPubkey: string;
  ownerToken: string;
  fence: bigint;
};

export interface DurableAutodepositStore {
  loadExecution(): Promise<DurableAutodepositExecution | null>;
  assertLease(lease: LeaseFence): Promise<void>;
  createWithPreparedPull(lease: LeaseFence, attempt: PreparedSignedAttempt): Promise<DurableAutodepositExecution>;
  appendPreparedTopUp(
    lease: LeaseFence,
    executionId: string,
    attempt: PreparedSignedAttempt
  ): Promise<DurableAutodepositExecution>;
  recordBroadcast(lease: LeaseFence, attempt: PersistedAttempt): Promise<void>;
  recordNonLandedAttempt(
    lease: LeaseFence,
    execution: DurableAutodepositExecution,
    attempt: PersistedAttempt,
    outcome: Exclude<AttemptOutcome, LandedAttemptEvidence>
  ): Promise<DurableAutodepositExecution>;
  recordConfirmedPull(
    lease: LeaseFence,
    execution: DurableAutodepositExecution,
    attempt: PersistedAttempt,
    evidence: PullChainEvidence
  ): Promise<DurableAutodepositExecution>;
  recordConfirmedTopUp(
    lease: LeaseFence,
    execution: DurableAutodepositExecution,
    attempt: PersistedAttempt,
    evidence: TopUpChainEvidence
  ): Promise<DurableAutodepositExecution>;
  persistCompletion(lease: LeaseFence, execution: DurableAutodepositExecution): Promise<DurableAutodepositExecution>;
}

export type DurableAutodepositHooks = Partial<
  Record<
    | "after_pull_attempt_persisted"
    | "after_pull_constructed"
    | "after_pull_broadcast"
    | "after_pull_chain_landed"
    | "after_pull_confirmed_persisted"
    | "after_top_up_attempt_persisted"
    | "after_top_up_constructed"
    | "after_top_up_broadcast"
    | "after_top_up_chain_landed"
    | "after_top_up_confirmed_persisted"
    | "before_completion_persistence",
    () => Promise<void> | void
  >
>;

export type DurableAutodepositDependencies = {
  store: DurableAutodepositStore;
  lease: LeaseFence;
  preparePull: () => Promise<PreparedSignedAttempt>;
  prepareTopUp: (amountRaw: bigint, execution: DurableAutodepositExecution) => Promise<PreparedSignedAttempt>;
  broadcastExactSignedTransaction: (attempt: PersistedAttempt) => Promise<string>;
  reconcileAttempt: (attempt: PersistedAttempt, waitForConfirmation: boolean) => Promise<AttemptOutcome>;
  readConfirmedPullEvidence: (attempt: PersistedAttempt, landed: LandedAttemptEvidence) => Promise<PullChainEvidence>;
  readConfirmedTopUpEvidence: (attempt: PersistedAttempt, landed: LandedAttemptEvidence) => Promise<TopUpChainEvidence>;
  hooks?: DurableAutodepositHooks;
};

async function callHook(hooks: DurableAutodepositHooks | undefined, name: keyof DurableAutodepositHooks) {
  await hooks?.[name]?.();
}

function requireActiveAttempt(
  execution: DurableAutodepositExecution,
  operationKind: AutodepositOperationKind
): PersistedAttempt {
  const attempt = execution.activeAttempt;
  if (!attempt || attempt.operationKind !== operationKind) {
    throw new Error(
      `Execution ${execution.id} is ${execution.lifecycleState} without an active ${operationKind} attempt.`
    );
  }
  return attempt;
}

async function settlePersistedAttempt(args: {
  dependencies: DurableAutodepositDependencies;
  execution: DurableAutodepositExecution;
  operationKind: AutodepositOperationKind;
}): Promise<{
  execution: DurableAutodepositExecution;
  landed: LandedAttemptEvidence | null;
}> {
  const { dependencies, operationKind } = args;
  const attempt = requireActiveAttempt(args.execution, operationKind);

  // Reconciliation always precedes broadcast. This covers both a process that
  // died after broadcasting and one that died after persistence but before the
  // first send. Re-broadcasting the same signed bytes cannot create a distinct
  // Solana transaction because the signature is deterministic.
  let outcome = await dependencies.reconcileAttempt(attempt, false);
  if (outcome.classification === "landed") {
    return { execution: args.execution, landed: outcome };
  }
  if (outcome.classification === "failed" || outcome.classification === "expired_not_landed") {
    return {
      execution: await dependencies.store.recordNonLandedAttempt(dependencies.lease, args.execution, attempt, outcome),
      landed: null,
    };
  }

  await dependencies.store.assertLease(dependencies.lease);
  let submittedSignature: string;
  try {
    submittedSignature = await dependencies.broadcastExactSignedTransaction(attempt);
  } catch (error) {
    // A submission error is not evidence that the exact signature did not
    // land. Reconcile first; only the chain outcome may authorize a later
    // top-up attempt. Pull attempts are never replaced automatically.
    outcome = await dependencies.reconcileAttempt(attempt, false);
    if (outcome.classification === "landed") {
      return { execution: args.execution, landed: outcome };
    }
    return {
      execution: await dependencies.store.recordNonLandedAttempt(
        dependencies.lease,
        args.execution,
        attempt,
        outcome.classification === "unknown" ? { classification: "unknown", error } : outcome
      ),
      landed: null,
    };
  }
  if (submittedSignature !== attempt.signature) {
    throw new Error(`RPC returned signature ${submittedSignature}, expected persisted signature ${attempt.signature}.`);
  }
  await dependencies.store.recordBroadcast(dependencies.lease, attempt);
  await callHook(dependencies.hooks, operationKind === "pull" ? "after_pull_broadcast" : "after_top_up_broadcast");

  outcome = await dependencies.reconcileAttempt(attempt, true);
  if (outcome.classification === "landed") {
    return { execution: args.execution, landed: outcome };
  }
  return {
    execution: await dependencies.store.recordNonLandedAttempt(dependencies.lease, args.execution, attempt, outcome),
    landed: null,
  };
}

export async function runDurableAutodepositExecution(
  dependencies: DurableAutodepositDependencies
): Promise<DurableAutodepositExecution> {
  let execution = await dependencies.store.loadExecution();

  if (!execution) {
    const preparedPull = await dependencies.preparePull();
    await callHook(dependencies.hooks, "after_pull_constructed");
    execution = await dependencies.store.createWithPreparedPull(dependencies.lease, preparedPull);
    await callHook(dependencies.hooks, "after_pull_attempt_persisted");
  }

  if (execution.lifecycleState === "completed") {
    return execution;
  }

  if (
    execution.lifecycleState === "pull_confirmation_pending" ||
    (execution.lifecycleState === "needs_reconciliation" && execution.activeAttempt?.operationKind === "pull")
  ) {
    const settled = await settlePersistedAttempt({
      dependencies,
      execution,
      operationKind: "pull",
    });
    execution = settled.execution;
    if (!settled.landed) {
      return execution;
    }
    await callHook(dependencies.hooks, "after_pull_chain_landed");
    const pullEvidence = await dependencies.readConfirmedPullEvidence(
      requireActiveAttempt(execution, "pull"),
      settled.landed
    );
    if (pullEvidence.destinationCreditRaw <= BigInt(0)) {
      throw new Error("Confirmed pull evidence has no positive vault credit.");
    }
    execution = await dependencies.store.recordConfirmedPull(
      dependencies.lease,
      execution,
      requireActiveAttempt(execution, "pull"),
      pullEvidence
    );
    await callHook(dependencies.hooks, "after_pull_confirmed_persisted");
  }

  if (execution.lifecycleState === "needs_reconciliation") {
    if (execution.activeAttempt?.operationKind !== "top_up") {
      return execution;
    }
  }

  if (execution.lifecycleState === "deposit_pending") {
    const amountRaw = execution.confirmedPullAmountRaw;
    if (!amountRaw || amountRaw <= BigInt(0)) {
      throw new Error(`Execution ${execution.id} cannot prepare a top-up without confirmed pull evidence.`);
    }
    const preparedTopUp = await dependencies.prepareTopUp(amountRaw, execution);
    await callHook(dependencies.hooks, "after_top_up_constructed");
    execution = await dependencies.store.appendPreparedTopUp(dependencies.lease, execution.id, preparedTopUp);
    await callHook(dependencies.hooks, "after_top_up_attempt_persisted");
  }

  if (
    execution.lifecycleState === "deposit_confirmation_pending" ||
    (execution.lifecycleState === "needs_reconciliation" && execution.activeAttempt?.operationKind === "top_up")
  ) {
    const settled = await settlePersistedAttempt({
      dependencies,
      execution,
      operationKind: "top_up",
    });
    execution = settled.execution;
    if (!settled.landed) {
      return execution;
    }
    await callHook(dependencies.hooks, "after_top_up_chain_landed");
    const topUpEvidence = await dependencies.readConfirmedTopUpEvidence(
      requireActiveAttempt(execution, "top_up"),
      settled.landed
    );
    if (topUpEvidence.vaultDebitRaw !== execution.confirmedPullAmountRaw) {
      throw new Error(
        `Top-up vault debit ${topUpEvidence.vaultDebitRaw} does not match confirmed pull ${execution.confirmedPullAmountRaw}.`
      );
    }
    execution = await dependencies.store.recordConfirmedTopUp(
      dependencies.lease,
      execution,
      requireActiveAttempt(execution, "top_up"),
      topUpEvidence
    );
    await callHook(dependencies.hooks, "after_top_up_confirmed_persisted");
  }

  if (execution.lifecycleState === "deposit_confirmed") {
    await callHook(dependencies.hooks, "before_completion_persistence");
    execution = await dependencies.store.persistCompletion(dependencies.lease, execution);
  }

  return execution;
}
