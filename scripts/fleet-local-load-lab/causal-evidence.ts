export type CausalEvidence = {
  raw: {
    rpcTotalRequests: number;
    rpcSyntheticDriverRequests: number;
    outboxTotalRowsAdded: number;
    localUserOutboxRowsAdded: number;
  };
  attribution: {
    realWorker: { rpcRequests: number; completed: number; outboxRows: number };
    syntheticSql: { outboxRows: number };
    syntheticRpc: { requests: number };
  };
  checks: {
    allStartedProcessesSurvived: boolean;
    realWorkerProgress: { passes: boolean };
  };
  verdicts: { fullChainE2e: "PASS" | "FAIL" | "NOT_RUN" };
  chain?: {
    reconciled: number;
    balanceDeltaRaw: number;
    roleCompletions: Record<string, number>;
  };
};

const finiteNonnegative = (value: number) =>
  Number.isFinite(value) && value >= 0;

export function causalEvidenceFailures(value: CausalEvidence): string[] {
  const failures: string[] = [];
  const numbers = [
    value.raw.rpcTotalRequests,
    value.raw.rpcSyntheticDriverRequests,
    value.raw.outboxTotalRowsAdded,
    value.raw.localUserOutboxRowsAdded,
    value.attribution.realWorker.rpcRequests,
    value.attribution.realWorker.completed,
    value.attribution.realWorker.outboxRows,
    value.attribution.syntheticSql.outboxRows,
    value.attribution.syntheticRpc.requests,
  ];
  if (!numbers.every(finiteNonnegative)) failures.push("counters must be finite and nonnegative");

  const expectedRealRpc = value.raw.rpcTotalRequests
    - value.raw.rpcSyntheticDriverRequests;
  if (expectedRealRpc < 0
    || value.attribution.realWorker.rpcRequests !== expectedRealRpc
    || value.attribution.syntheticRpc.requests !== value.raw.rpcSyntheticDriverRequests) {
    failures.push("RPC source attribution does not reconcile with raw logs");
  }

  const expectedWorkerOutbox = value.raw.outboxTotalRowsAdded
    - value.raw.localUserOutboxRowsAdded;
  if (expectedWorkerOutbox < 0
    || value.attribution.realWorker.outboxRows !== expectedWorkerOutbox
    || value.attribution.syntheticSql.outboxRows !== value.raw.localUserOutboxRowsAdded) {
    failures.push("outbox source attribution does not reconcile with event kinds");
  }

  const progress = value.attribution.realWorker.completed > 0;
  if (value.checks.realWorkerProgress.passes !== progress) {
    failures.push("real-worker progress verdict is not derived from completed work");
  }

  if (value.verdicts.fullChainE2e === "PASS") {
    const chain = value.chain;
    const everyRoleCompleted = chain
      && ["planner", "revalidator", "executor", "confirmer", "reconciler"]
        .every((role) => (chain.roleCompletions[role] ?? 0) > 0);
    if (!progress || !chain || chain.reconciled <= 0
      || chain.balanceDeltaRaw === 0 || !everyRoleCompleted) {
      failures.push("full-chain PASS lacks causal pipeline and balance evidence");
    }
  }
  return failures;
}
