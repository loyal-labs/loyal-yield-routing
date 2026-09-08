import { createHash } from "node:crypto";
import {
  chmodSync,
  existsSync,
  lstatSync,
  mkdirSync,
  readFileSync,
} from "node:fs";
import { join, resolve } from "node:path";

export type JsonRecord = Record<string, unknown>;

export const PENDING_BINDING_MISMATCH =
  "RECONCILE_MISMATCH: pending journal does not match the canonical binding";

const VOLATILE_PENDING_FIELDS = new Set([
  "verdict",
  "sent",
  "signed",
  "broadcast",
  "broadcastPreMark",
  "signature",
  "abortReason",
  "pendingBindingSha256",
]);

function canonicalValue(value: unknown): unknown {
  if (typeof value === "bigint") return value.toString();
  if (Array.isArray(value)) return value.map((entry) => canonicalValue(entry));
  if (value && typeof value === "object") {
    return Object.fromEntries(
      Object.keys(value as JsonRecord)
        .sort()
        .map((key) => [key, canonicalValue((value as JsonRecord)[key])]),
    );
  }
  return value;
}

export function canonicalJson(value: unknown): string {
  return JSON.stringify(canonicalValue(value)) ?? "null";
}

export function sha256Hex(value: string | Uint8Array): string {
  return createHash("sha256").update(value).digest("hex");
}

function pendingBindingValue(value: unknown): unknown {
  if (!value || typeof value !== "object" || Array.isArray(value)) return value;
  const record = value as JsonRecord;
  return Object.fromEntries(
    Object.entries(record).filter(([key]) => !VOLATILE_PENDING_FIELDS.has(key)),
  );
}

export function pendingBindingSha256(pending: unknown): string {
  return sha256Hex(canonicalJson(pendingBindingValue(pending)));
}

export function finalizedJournalSha256(path: string): string {
  return sha256Hex(readFileSync(path));
}

export type CanonicalLegStateStatus =
  | "pending"
  | "attempted"
  | "finalized"
  | "aborted-pre-send";

type CanonicalLegBase = Readonly<{
  step: string;
  vault: string;
  journal: string;
  expectedSignature: string;
  messageSha256: string;
  wireSha256: string;
  pendingBindingSha256: string;
  checkoutRoot: string;
  stateRoot: string;
  repeatable: boolean;
  allowRepeat: boolean;
  journalExists?: boolean;
  nowUnixMs?: number;
}>;

function historyEntry(existing: JsonRecord): JsonRecord {
  const { history: _history, ...entry } = existing;
  return entry;
}

function nextHistory(existing: JsonRecord): unknown[] {
  const history = Array.isArray(existing.history) ? existing.history : [];
  return [...history, historyEntry(existing)];
}

function assertAllowRepeatFlag(step: string, repeatable: boolean, allowRepeat: boolean) {
  if (allowRepeat && !repeatable) {
    throw new Error(`--allow-repeat is not accepted for one-shot HXtk leg ${step}`);
  }
}

export function assertCanonicalLegAvailable(input: Readonly<{
  step: string;
  journal: string;
  existing: JsonRecord | null;
  repeatable: boolean;
  allowRepeat: boolean;
  journalExists?: boolean;
}>): void {
  const { step, journal, existing, repeatable, allowRepeat } = input;
  assertAllowRepeatFlag(step, repeatable, allowRepeat);
  if (existing === null) return;

  if (existing.status === "pending" || existing.status === "attempted") {
    throw new Error(
      `${step} canonical replay fence is ${existing.status}; use the same journal's --reconcile path after checking finalized status`,
    );
  }
  if (existing.status === "aborted-pre-send") return;
  if (existing.status !== "finalized") {
    throw new Error(`${step} canonical replay fence has invalid status ${String(existing.status)}`);
  }
  if (!allowRepeat) {
    throw new Error(`${step} canonical replay fence is finalized; rerun requires explicit --allow-repeat`);
  }
  if (String(existing.journal ?? "") === journal) {
    throw new Error(`${step} --allow-repeat requires a new journal path`);
  }
  if (input.journalExists === true) {
    throw new Error(`${step} --allow-repeat requires a new journal path that does not exist`);
  }
}

export function beginCanonicalLegRecord(input: CanonicalLegBase & Readonly<{
  existing: JsonRecord | null;
}>): JsonRecord {
  const {
    step,
    vault,
    journal,
    expectedSignature,
    messageSha256,
    wireSha256,
    pendingBindingSha256: binding,
    checkoutRoot,
    stateRoot,
    repeatable,
    allowRepeat,
    journalExists,
    existing,
    nowUnixMs = Date.now(),
  } = input;
  assertAllowRepeatFlag(step, repeatable, allowRepeat);

  if (existing !== null
    && existing.status !== "finalized"
    && existing.status !== "aborted-pre-send") {
    throw new Error(
      `${step} canonical replay fence is ${String(existing.status)}; use the same journal's --reconcile path after checking finalized status`,
    );
  }

  if (existing?.status === "finalized") {
    if (!allowRepeat) {
      throw new Error(`${step} canonical replay fence is finalized; rerun requires explicit --allow-repeat`);
    }
    if (String(existing.journal ?? "") === journal) {
      throw new Error(`${step} --allow-repeat requires a new journal path`);
    }
    if (journalExists === true) {
      throw new Error(`${step} --allow-repeat requires a new journal path that does not exist`);
    }
  }

  const rearm = existing !== null;
  return {
    schema: "loyal-voltr-hxtk-reset-state/v2",
    vault,
    step,
    status: "pending" satisfies CanonicalLegStateStatus,
    broadcast: false,
    journal,
    expectedSignature,
    messageSha256,
    wireSha256,
    pendingBindingSha256: binding,
    checkoutRoot,
    stateRoot,
    attempt: Number(existing?.attempt ?? 0) + 1,
    ...(rearm ? { history: nextHistory(existing) } : {}),
    updatedAtUnixMs: nowUnixMs,
  };
}

export function assertPendingJournalBinding(input: Readonly<{
  pending: JsonRecord;
  canonicalState: JsonRecord;
  expectedSignature: string;
  messageSha256: string;
  wireSha256: string;
}>): void {
  const { pending, canonicalState, expectedSignature, messageSha256, wireSha256 } = input;
  const transaction = pending.transaction && typeof pending.transaction === "object"
    ? pending.transaction as JsonRecord
    : null;
  const binding = pendingBindingSha256(pending);
  const matches = transaction !== null
    && String(pending.pendingBindingSha256 ?? "") === binding
    && String(canonicalState.pendingBindingSha256 ?? "") === binding
    && String(canonicalState.expectedSignature ?? "") === expectedSignature
    && String(canonicalState.messageSha256 ?? "") === messageSha256
    && String(canonicalState.wireSha256 ?? "") === wireSha256
    && String(transaction.expectedSignature ?? "") === expectedSignature
    && String(transaction.messageSha256 ?? "") === messageSha256
    && String(transaction.wireSha256 ?? "") === wireSha256
    && String(pending.canonicalStateRoot ?? "") === String(canonicalState.stateRoot ?? "");
  if (!matches) throw new Error(PENDING_BINDING_MISMATCH);
}

export function assertFinalizedJournalBinding(input: Readonly<{
  step: string;
  journal: string;
  canonicalState: JsonRecord | null;
  journalSha256: string;
}>): void {
  const { step, journal, canonicalState, journalSha256 } = input;
  const matches = canonicalState !== null
    && canonicalState.status === "finalized"
    && resolve(String(canonicalState.journal ?? "")) === resolve(journal)
    && String(canonicalState.finalizedJournalSha256 ?? "") === journalSha256;
  if (!matches) {
    throw new Error(`RECONCILE_MISMATCH: finalized ${step} journal is not bound to the canonical fence`);
  }
}

export function repairPostFinalizationStatus(status: unknown):
  | "REPAIR_FINALIZED_POLICY_STILL_PRESENT"
  | "REPAIR_ATTEMPTED_POLICY_PRESENT"
  | null {
  if (status === "finalized") return "REPAIR_FINALIZED_POLICY_STILL_PRESENT";
  if (status === "attempted") return "REPAIR_ATTEMPTED_POLICY_PRESENT";
  return null;
}

function currentUid(uid?: number): number {
  const resolved = uid ?? (typeof process.getuid === "function" ? process.getuid() : undefined);
  if (resolved === undefined) throw new Error("HXTK state root cannot determine the current uid");
  return resolved;
}

function assertPrivateDirectory(path: string, uid: number): void {
  const stat = lstatSync(path);
  if (!stat.isDirectory() || stat.isSymbolicLink()) {
    throw new Error(`HXTK state root component ${path} is not a real directory`);
  }
  if (stat.uid !== uid) {
    throw new Error(`HXTK state root component ${path} is not owned by the current uid`);
  }
  if ((stat.mode & 0o077) !== 0) {
    throw new Error(`HXTK state root component ${path} is group/other-accessible`);
  }
}

function ensurePrivateDirectory(path: string, uid: number): void {
  if (!existsSync(path)) mkdirSync(path, { recursive: true, mode: 0o700 });
  assertPrivateDirectory(path, uid);
  chmodSync(path, 0o700);
  assertPrivateDirectory(path, uid);
}

function ensureDefaultRoot(root: string, base: string, uid: number, create: boolean): void {
  const relative = root.slice(base.length).split("/").filter(Boolean);
  let current = base;
  if (!existsSync(current)) {
    if (!create) return;
    ensurePrivateDirectory(current, uid);
  } else {
    assertPrivateDirectory(current, uid);
  }
  for (const component of relative) {
    current = join(current, component);
    if (!existsSync(current)) {
      if (!create) return;
      ensurePrivateDirectory(current, uid);
    } else {
      assertPrivateDirectory(current, uid);
    }
  }
}

export function resolveCanonicalStateRoot(input: Readonly<{
  vault: string;
  env?: Readonly<Record<string, string | undefined>>;
  uid?: number;
  create?: boolean;
}>): string {
  const env = input.env ?? process.env;
  const override = env.HXTK_RESET_STATE_ROOT?.trim();
  const create = input.create === true;
  const uid = currentUid(input.uid);
  if (override) {
    if (!override.startsWith("/")) {
      throw new Error("HXTK_RESET_STATE_ROOT must be an absolute path");
    }
    const root = resolve(override);
    if (!existsSync(root)) {
      if (!create) return root;
      ensurePrivateDirectory(root, uid);
    } else {
      assertPrivateDirectory(root, uid);
    }
    return root;
  }

  const home = env.HOME?.trim();
  if (!home) throw new Error("HXTK state root requires HOME when HXTK_RESET_STATE_ROOT is unset");
  if (!home.startsWith("/")) throw new Error("HOME must be an absolute path for the HXTK state root");
  const base = join(resolve(home), ".loyal");
  const root = join(base, "hxtk-reset", input.vault);
  ensureDefaultRoot(root, base, uid, create);
  return root;
}
