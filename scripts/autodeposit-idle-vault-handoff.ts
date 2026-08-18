export const AUTODEPOSIT_IDLE_HANDOFF_STATUS =
  "partial_executed_pull_idle_vault_handoff";

export type IdleVaultObservation = {
  amountRaw: bigint;
  observedSlot: bigint;
};

export type IdleVaultProjection = IdleVaultObservation & {
  mint: string;
  owner: string;
  tokenAccount: string;
  vaultId: bigint;
};

export type HistoricalIdleVaultCandidate = {
  alreadyRecovered: boolean;
  amountRaw: bigint;
  executionId: bigint;
};

export async function observeIdleVaultAtOrAfter(args: {
  minimumSlot: bigint;
  maxAttempts: number;
  pollIntervalMs: number;
  read: () => Promise<IdleVaultObservation>;
  sleep?: (milliseconds: number) => Promise<void>;
}): Promise<IdleVaultObservation> {
  if (!Number.isInteger(args.maxAttempts) || args.maxAttempts < 1) {
    throw new Error("maxAttempts must be a positive integer");
  }
  const sleep =
    args.sleep ??
    ((milliseconds: number) =>
      new Promise<void>((resolve) => setTimeout(resolve, milliseconds)));
  let last: IdleVaultObservation | null = null;
  for (let attempt = 1; attempt <= args.maxAttempts; attempt += 1) {
    last = await args.read();
    if (last.observedSlot >= args.minimumSlot) {
      return last;
    }
    if (attempt < args.maxAttempts) {
      await sleep(args.pollIntervalMs);
    }
  }
  throw new Error(
    `idle-vault observation remained at slot ${last?.observedSlot ?? "unknown"}, before confirmed pull slot ${args.minimumSlot}`,
  );
}

export function projectIdleVaultBalance(
  current: IdleVaultProjection | null,
  incoming: IdleVaultProjection,
): IdleVaultProjection {
  if (!current) {
    return incoming;
  }
  if (current.vaultId !== incoming.vaultId || current.mint !== incoming.mint) {
    throw new Error("idle-vault projection keys do not match");
  }
  if (incoming.observedSlot < current.observedSlot) {
    return current;
  }
  if (
    incoming.observedSlot === current.observedSlot &&
    (incoming.amountRaw !== current.amountRaw ||
      incoming.owner !== current.owner ||
      incoming.tokenAccount !== current.tokenAccount)
  ) {
    throw new Error(
      `conflicting idle-vault observation at slot ${incoming.observedSlot}`,
    );
  }
  return incoming;
}

export function historicalIdleVaultRecoveryAction(
  candidate: HistoricalIdleVaultCandidate,
): "skip_already_recovered" | "skip_zero" | "project" {
  if (candidate.alreadyRecovered) {
    return "skip_already_recovered";
  }
  return candidate.amountRaw > 0n ? "project" : "skip_zero";
}

export class OncePerKeyAlertLatch {
  readonly #claimed = new Set<string>();

  claim(key: string): boolean {
    if (this.#claimed.has(key)) {
      return false;
    }
    this.#claimed.add(key);
    return true;
  }
}

export function idleVaultRecoveryAlert(args: {
  alertAlreadyClaimed: boolean;
  handoffPersistenceFailed: boolean;
  idleSinceMs: number;
  nowMs: number;
  recoverySlaMs: number;
}): "handoff_persistence_failed" | "recovery_sla_exceeded" | null {
  if (args.alertAlreadyClaimed) {
    return null;
  }
  if (args.handoffPersistenceFailed) {
    return "handoff_persistence_failed";
  }
  return args.nowMs - args.idleSinceMs >= args.recoverySlaMs
    ? "recovery_sla_exceeded"
    : null;
}

export function shouldNotifyIdleVaultFailure(args: {
  finalFailure: boolean;
  notificationAlreadySent: boolean;
}): boolean {
  return args.finalFailure && !args.notificationAlreadySent;
}
