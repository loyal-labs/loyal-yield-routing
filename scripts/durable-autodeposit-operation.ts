export type DurableAutodepositAction =
  | "preflight_deposit"
  | "execute_pull"
  | "execute_deposit"
  | "complete"
  | "wait";

export type DurableAutodepositOperationalAlert =
  | "transaction_effect_ambiguous"
  | "durable_ownership_lost";

export function durableDepositRetryEffect(args: {
  currentSourceBalanceRaw: bigint;
  postPullSourceBalanceRaw: bigint;
}): "safe_to_retry" | "ambiguous_prior_effect" {
  return args.currentSourceBalanceRaw < args.postPullSourceBalanceRaw
    ? "ambiguous_prior_effect"
    : "safe_to_retry";
}

export type DurableAutodepositState = {
  depositPreflight: "pending" | "ready" | "retryable_failure";
  pull: "not_started" | "confirmed" | "ambiguous";
  deposit: "not_started" | "retryable_failure" | "confirmed" | "ambiguous";
  durableOwnership: "held" | "lost";
  pullSignature: string | null;
  depositSignature: string | null;
};

export type DurableAutodepositEvent =
  | { type: "deposit_preflight_ready" }
  | { type: "deposit_preflight_retryable_failure" }
  | { type: "pull_confirmed"; signature: string }
  | { type: "pull_ambiguous" }
  | { type: "deposit_retryable_failure" }
  | { type: "deposit_confirmed"; signature: string }
  | { type: "deposit_ambiguous" }
  | { type: "durable_ownership_lost" };

export function initialDurableAutodepositState(): DurableAutodepositState {
  return {
    depositPreflight: "pending",
    pull: "not_started",
    deposit: "not_started",
    durableOwnership: "held",
    pullSignature: null,
    depositSignature: null,
  };
}

export function reduceDurableAutodepositState(
  state: DurableAutodepositState,
  event: DurableAutodepositEvent,
): DurableAutodepositState {
  switch (event.type) {
    case "deposit_preflight_ready":
      return { ...state, depositPreflight: "ready" };
    case "deposit_preflight_retryable_failure":
      return { ...state, depositPreflight: "retryable_failure" };
    case "pull_confirmed":
      return {
        ...state,
        pull: "confirmed",
        pullSignature: event.signature,
      };
    case "pull_ambiguous":
      return { ...state, pull: "ambiguous" };
    case "deposit_retryable_failure":
      return { ...state, deposit: "retryable_failure" };
    case "deposit_confirmed":
      return {
        ...state,
        deposit: "confirmed",
        depositSignature: event.signature,
      };
    case "deposit_ambiguous":
      return { ...state, deposit: "ambiguous" };
    case "durable_ownership_lost":
      return { ...state, durableOwnership: "lost" };
  }
}

export function operationalAlertForDurableAutodeposit(
  state: DurableAutodepositState,
): DurableAutodepositOperationalAlert | null {
  if (state.durableOwnership === "lost") {
    return "durable_ownership_lost";
  }
  if (state.pull === "ambiguous" || state.deposit === "ambiguous") {
    return "transaction_effect_ambiguous";
  }
  return null;
}

export function canCompleteDurableAutodeposit(
  state: DurableAutodepositState,
): boolean {
  return (
    state.durableOwnership === "held" &&
    state.pull === "confirmed" &&
    state.deposit === "confirmed" &&
    state.pullSignature !== null &&
    state.depositSignature !== null
  );
}

export function canNotifyDurableAutodepositSuccess(
  state: DurableAutodepositState,
): boolean {
  return canCompleteDurableAutodeposit(state);
}

export function nextDurableAutodepositAction(
  state: DurableAutodepositState,
): DurableAutodepositAction {
  if (operationalAlertForDurableAutodeposit(state)) {
    return "wait";
  }
  if (state.depositPreflight !== "ready") {
    return "preflight_deposit";
  }
  if (state.pull !== "confirmed") {
    return "execute_pull";
  }
  if (state.deposit !== "confirmed") {
    return "execute_deposit";
  }
  return "complete";
}
