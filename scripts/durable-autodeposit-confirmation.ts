export type AutodepositOperationKind = "pull" | "top_up";

export type AutodepositAttemptState =
  | "prepared"
  | "submitted"
  | "confirmed"
  | "failed"
  | "expired"
  | "unknown"
  | "ambiguous";

export type DurableAutodepositAttempt = {
  id: string;
  claimToken: string;
  operationKind: AutodepositOperationKind;
  executionId: string | null;
  amountRaw: bigint;
  sourcePreBalanceRaw: bigint;
  destinationPreBalanceRaw: bigint;
  signature: string;
  signedTransactionBase64: string;
  signedTransactionSha256: string;
  blockhash: string;
  lastValidBlockHeight: bigint;
  state: AutodepositAttemptState;
  broadcastCount: number;
  confirmedSlot: bigint | null;
};

export type AttemptObservation = {
  state: "confirmed" | "failed" | "expired" | "unknown" | "ambiguous";
  confirmedSlot: bigint | null;
  error: unknown | null;
};

export type DurableAttemptSettlement = {
  attempt: DurableAutodepositAttempt;
  observation: AttemptObservation;
  broadcasted: boolean;
};

export type DurableAttemptDependencies = {
  observe: (
    attempt: DurableAutodepositAttempt
  ) => Promise<AttemptObservation>;
  broadcastExact: (
    attempt: DurableAutodepositAttempt
  ) => Promise<string>;
  recordBroadcast: (
    attempt: DurableAutodepositAttempt
  ) => Promise<DurableAutodepositAttempt>;
  recordObservation: (
    attempt: DurableAutodepositAttempt,
    observation: AttemptObservation
  ) => Promise<DurableAutodepositAttempt>;
};

export type AutodepositAttemptAlert = {
  code: "autodeposit_transaction_effect_ambiguous";
  operation: "reconcile_autodeposit_transaction";
  summary: "autodeposit transaction effect remains ambiguous after blockhash expiry";
  retryable: false;
  recoveryRequired: true;
};

export function attemptHoldsClaim(
  state: AutodepositAttemptState
): boolean {
  return (
    state === "prepared" ||
    state === "submitted" ||
    state === "confirmed" ||
    state === "unknown" ||
    state === "ambiguous"
  );
}

export function attemptAllowsSafeRequeue(
  state: AutodepositAttemptState
): boolean {
  return state === "failed" || state === "expired";
}

export function operationalAlertForAttempt(
  state: AutodepositAttemptState
): AutodepositAttemptAlert | null {
  if (state !== "ambiguous") {
    return null;
  }
  return {
    code: "autodeposit_transaction_effect_ambiguous",
    operation: "reconcile_autodeposit_transaction",
    summary:
      "autodeposit transaction effect remains ambiguous after blockhash expiry",
    retryable: false,
    recoveryRequired: true,
  };
}

function isTerminalObservation(observation: AttemptObservation): boolean {
  return observation.state !== "unknown";
}

/**
 * Resolves one persisted signed attempt without ever constructing a replacement.
 * Re-broadcasts are safe because they use the immutable bytes whose deterministic
 * signature is already stored on the attempt.
 */
export async function settleDurableAutodepositAttempt(args: {
  attempt: DurableAutodepositAttempt;
  dependencies: DurableAttemptDependencies;
}): Promise<DurableAttemptSettlement> {
  let attempt = args.attempt;
  let observation = await args.dependencies.observe(attempt);
  if (isTerminalObservation(observation)) {
    attempt = await args.dependencies.recordObservation(attempt, observation);
    return { attempt, observation, broadcasted: false };
  }

  let broadcasted = false;
  try {
    const returnedSignature = await args.dependencies.broadcastExact(attempt);
    if (returnedSignature !== attempt.signature) {
      throw new Error(
        `RPC returned signature ${returnedSignature}, expected persisted signature ${attempt.signature}.`
      );
    }
    attempt = await args.dependencies.recordBroadcast(attempt);
    broadcasted = true;
  } catch (error) {
    observation = await args.dependencies.observe(attempt);
    if (observation.state === "unknown" && observation.error === null) {
      observation = { ...observation, error };
    }
    attempt = await args.dependencies.recordObservation(attempt, observation);
    return { attempt, observation, broadcasted };
  }

  observation = await args.dependencies.observe(attempt);
  attempt = await args.dependencies.recordObservation(attempt, observation);
  return { attempt, observation, broadcasted };
}
