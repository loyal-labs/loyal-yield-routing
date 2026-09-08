import { createHash, randomBytes } from "node:crypto";
import {
  chmodSync,
  closeSync,
  constants,
  existsSync,
  fsyncSync,
  fstatSync,
  openSync,
  lstatSync,
  mkdirSync,
  renameSync,
  readSync,
  unlinkSync,
  writeSync,
} from "node:fs";
import { hostname, userInfo } from "node:os";
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
  "sendStatus",
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

function readPrivateBytes(path: string): Uint8Array {
  const flags = constants.O_RDONLY | constants.O_NOFOLLOW;
  const fd = openSync(path, flags);
  try {
    const stat = fstatSync(fd);
    const uid = currentUid();
    if (!stat.isFile()) throw new Error(`HXTK journal ${path} is not a regular file`);
    if (stat.uid !== uid) throw new Error(`HXTK journal ${path} is not owned by the current uid`);
    if ((stat.mode & 0o077) !== 0) throw new Error(`HXTK journal ${path} is group/other-accessible`);
    const bytes = new Uint8Array(stat.size);
    let offset = 0;
    while (offset < bytes.length) {
      const read = readSync(fd, bytes, offset, bytes.length - offset, offset);
      if (read === 0) throw new Error(`HXTK journal ${path} changed while reading`);
      offset += read;
    }
    return bytes;
  } finally {
    closeSync(fd);
  }
}

export function readBoundJournal(path: string, expectedSha256?: string): Readonly<{
  record: JsonRecord;
  sha256: string;
  bytes: Uint8Array;
}> {
  const bytes = readPrivateBytes(path);
  const sha256 = sha256Hex(bytes);
  if (expectedSha256 !== undefined && sha256 !== expectedSha256) {
    throw new Error(`HXTK journal ${path} hash does not match the canonical fence`);
  }
  const parsed = JSON.parse(Buffer.from(bytes).toString("utf8")) as JsonRecord;
  return { record: parsed, sha256, bytes };
}

export function readBoundPending(path: string): Readonly<{
  record: JsonRecord;
  sha256: string;
  bytes: Uint8Array;
}> {
  const result = readBoundJournal(path);
  const binding = result.record.pendingBindingSha256;
  if (typeof binding !== "string" || binding !== pendingBindingSha256(result.record)) {
    throw new Error(PENDING_BINDING_MISMATCH);
  }
  return result;
}

export function finalizedJournalSha256(path: string): string {
  return readBoundJournal(path).sha256;
}

const SEND_STATUS_FIELDS = new Set([
  "verdict",
  "sendError",
  "submission",
  "attemptedAtUnixMs",
  "signature",
]);

function writeAll(fd: number, bytes: Uint8Array): void {
  let offset = 0;
  while (offset < bytes.length) {
    const written = writeSync(fd, bytes, offset, bytes.length - offset);
    if (written === 0) throw new Error("private HXTK file write made no progress");
    offset += written;
  }
}

function writeExclusivePrivate(path: string, bytes: Uint8Array): void {
  const fd = openSync(
    path,
    constants.O_WRONLY | constants.O_CREAT | constants.O_EXCL | constants.O_NOFOLLOW,
    0o600,
  );
  try {
    writeAll(fd, bytes);
    fsyncSync(fd);
  } finally {
    closeSync(fd);
  }
}

function atomicWritePrivate(path: string, value: JsonRecord): void {
  const temporary = `${path}.tmp-${process.pid}-${Date.now()}`;
  writeExclusivePrivate(temporary, Buffer.from(`${canonicalJson(value)}\n`));
  renameSync(temporary, path);
}

export function rewritePendingStatus(path: string, statusFields: Readonly<JsonRecord>): JsonRecord {
  for (const [key, value] of Object.entries(statusFields)) {
    if (!VOLATILE_PENDING_FIELDS.has(key)) {
      throw new Error(`pending status rewrite refuses non-volatile field ${key}`);
    }
    if (key === "sendStatus") {
      if (!value || typeof value !== "object" || Array.isArray(value)) {
        throw new Error("pending status rewrite sendStatus must be an object");
      }
      for (const nestedKey of Object.keys(value as JsonRecord)) {
        if (!SEND_STATUS_FIELDS.has(nestedKey)) {
          throw new Error(`pending status rewrite refuses non-volatile sendStatus field ${nestedKey}`);
        }
      }
    }
  }
  const current = readBoundPending(path).record;
  const binding = pendingBindingSha256(current);
  if (String(current.pendingBindingSha256 ?? "") !== binding) {
    throw new Error(PENDING_BINDING_MISMATCH);
  }
  const next = { ...current, ...statusFields };
  if (String(next.pendingBindingSha256 ?? "") !== binding || pendingBindingSha256(next) !== binding) {
    throw new Error(PENDING_BINDING_MISMATCH);
  }
  atomicWritePrivate(path, next);
  return next;
}

export function canonicalStateGeneration(value: JsonRecord): number {
  const generation = value.generation;
  if (!Number.isSafeInteger(generation) || (generation as number) < 0) {
    throw new Error("STATE_GENERATION_CONFLICT: canonical state has no valid integer generation");
  }
  return generation as number;
}

/**
 * Replace one canonical state record with a compare-and-swap generation bump.
 * The caller owns the section claim and must carry the expected generation
 * forward after each successful write.
 */
export function writeCanonicalStateCas(
  path: string,
  value: JsonRecord,
  expectedGeneration: number | null,
): JsonRecord {
  if (expectedGeneration !== null
    && (!Number.isSafeInteger(expectedGeneration) || expectedGeneration < 0)) {
    throw new Error("STATE_GENERATION_CONFLICT: expected generation is invalid");
  }

  let current: JsonRecord | null = null;
  try {
    current = readBoundJournal(path).record;
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error;
  }
  const actualGeneration = current === null ? null : canonicalStateGeneration(current);
  if (actualGeneration !== expectedGeneration) {
    throw new Error(
      `STATE_GENERATION_CONFLICT: expected generation ${expectedGeneration ?? "absent"}, `
      + `found ${actualGeneration ?? "absent"}`,
    );
  }

  const next = {
    ...value,
    generation: (expectedGeneration ?? -1) + 1,
  };
  try {
    if (current === null) {
      writeExclusivePrivate(path, Buffer.from(`${canonicalJson(next)}\n`));
    } else {
      atomicWritePrivate(path, next);
    }
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "EEXIST"
      || (error as NodeJS.ErrnoException).code === "ENOENT") {
      throw new Error("STATE_GENERATION_CONFLICT: canonical state changed during write", { cause: error });
    }
    throw error;
  }
  return next;
}

function validLegName(step: string): void {
  if (!/^[a-z0-9-]+$/.test(step)) throw new Error(`invalid canonical HXtk leg name ${step}`);
}

export type CanonicalLegClaim = Readonly<{
  path: string;
  pid: number;
  startedAtUnixMs: number;
  journal: string;
  hostname: string;
  token: string;
}>;

function claimPath(stateRoot: string, step: string): string {
  validLegName(step);
  return join(stateRoot, `${step}.claim`);
}

function readClaim(path: string): CanonicalLegClaim {
  const parsed = JSON.parse(Buffer.from(readPrivateBytes(path)).toString("utf8")) as Partial<CanonicalLegClaim>;
  if (!Number.isSafeInteger(parsed.pid) || (parsed.pid ?? 0) <= 0
    || typeof parsed.startedAtUnixMs !== "number"
    || typeof parsed.journal !== "string"
    || typeof parsed.hostname !== "string"
    || typeof parsed.token !== "string"
    || !/^[0-9a-f]{32}$/.test(parsed.token)) {
    throw new Error(`HXTK claim ${path} is malformed; refusing to break it`);
  }
  return {
    path,
    pid: parsed.pid!,
    startedAtUnixMs: parsed.startedAtUnixMs,
    journal: parsed.journal,
    hostname: parsed.hostname,
    token: parsed.token,
  };
}

const PROCESS_CLAIM_TOKEN = randomBytes(16).toString("hex");

function claimOccupied(step: string, existing: CanonicalLegClaim): Error {
  return new Error(`another hxtk-reset process holds the ${step} claim: pid ${existing.pid}`);
}

function assertDeadLocalClaim(step: string, existing: CanonicalLegClaim): void {
  const localHostname = hostname();
  if (existing.hostname !== localHostname) {
    throw new Error(
      `cannot break ${step} claim from hostname ${existing.hostname}; expected ${localHostname}`,
    );
  }
  try {
    process.kill(existing.pid, 0);
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ESRCH") return;
    throw error;
  }
  throw claimOccupied(step, existing);
}

function refuseAfterBreakRace(path: string, step: string): never {
  try {
    throw claimOccupied(step, readClaim(path));
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error;
  }
  throw new Error(`another hxtk-reset process won breaking the ${step} claim; refusing to acquire it`);
}

type BreakLease = Readonly<{
  path: string;
  pid: number;
  startedAtUnixMs: number;
  hostname: string;
  token: string;
}>;

function breakLeasePath(claimPath: string): string {
  return `${claimPath}.break`;
}

function readBreakLease(path: string): BreakLease {
  const parsed = JSON.parse(Buffer.from(readPrivateBytes(path)).toString("utf8")) as Partial<BreakLease>;
  if (!Number.isSafeInteger(parsed.pid) || (parsed.pid ?? 0) <= 0
    || typeof parsed.startedAtUnixMs !== "number"
    || typeof parsed.hostname !== "string"
    || typeof parsed.token !== "string"
    || !/^[0-9a-f]{32}$/.test(parsed.token)) {
    throw new Error(`HXTK break lease ${path} is malformed; refusing to break it`);
  }
  return {
    path,
    pid: parsed.pid!,
    startedAtUnixMs: parsed.startedAtUnixMs,
    hostname: parsed.hostname,
    token: parsed.token,
  };
}

function newBreakLease(path: string): BreakLease {
  return {
    path,
    pid: process.pid,
    startedAtUnixMs: Date.now(),
    hostname: hostname(),
    token: PROCESS_CLAIM_TOKEN,
  };
}

function createBreakLease(lease: BreakLease): void {
  writeExclusivePrivate(lease.path, Buffer.from(`${canonicalJson({
    pid: lease.pid,
    startedAtUnixMs: lease.startedAtUnixMs,
    hostname: lease.hostname,
    token: lease.token,
  })}\n`));
}

function finishBreakLease(lease: BreakLease): void {
  let existing: BreakLease;
  try {
    existing = readBreakLease(lease.path);
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") return;
    throw error;
  }
  if (existing.token !== PROCESS_CLAIM_TOKEN || lease.token !== PROCESS_CLAIM_TOKEN) return;
  try {
    // Keep break cleanup rename-only: no break path is unlinked or reused.
    renameSync(lease.path, `${lease.path}.done-${Date.now()}-${process.pid}`);
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error;
  }
}

function acquireBreakLease(claimPath: string, step: string): BreakLease {
  const path = breakLeasePath(claimPath);
  const lease = newBreakLease(path);
  try {
    createBreakLease(lease);
    return lease;
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code !== "EEXIST") throw error;
  }

  let existing: BreakLease;
  try {
    existing = readBreakLease(path);
  } catch (readError) {
    if ((readError as NodeJS.ErrnoException).code === "ENOENT") {
      throw new Error(`HXTK ${step} break lease disappeared while acquiring; retry`, { cause: readError });
    }
    throw readError;
  }
  if (existing.hostname !== hostname()) {
    throw new Error(
      `cannot take ${step} break lease from hostname ${existing.hostname}; expected ${hostname()}`,
    );
  }
  try {
    process.kill(existing.pid, 0);
    return refuseAfterBreakRace(claimPath, step);
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code !== "ESRCH") throw error;
  }

  const stalePath = `${path}.stale-${Date.now()}-${process.pid}`;
  try {
    renameSync(path, stalePath);
  } catch (renameError) {
    const code = (renameError as NodeJS.ErrnoException).code;
    if (code === "ENOENT" || code === "EEXIST") return refuseAfterBreakRace(claimPath, step);
    throw renameError;
  }
  try {
    createBreakLease(lease);
    return lease;
  } catch (createError) {
    if ((createError as NodeJS.ErrnoException).code !== "EEXIST") throw createError;
    return refuseAfterBreakRace(claimPath, step);
  }
}

function newClaim(path: string, journal: string): CanonicalLegClaim {
  return {
    path,
    pid: process.pid,
    startedAtUnixMs: Date.now(),
    journal,
    hostname: hostname(),
    token: PROCESS_CLAIM_TOKEN,
  };
}

function createClaim(claim: CanonicalLegClaim): void {
  writeExclusivePrivate(claim.path, Buffer.from(`${canonicalJson({
    pid: claim.pid,
    startedAtUnixMs: claim.startedAtUnixMs,
    hostname: claim.hostname,
    journal: claim.journal,
    token: claim.token,
  })}\n`));
}

export function acquireCanonicalLegClaim(input: Readonly<{
  stateRoot: string;
  step: string;
  journal: string;
  breakClaim?: boolean;
}>): CanonicalLegClaim {
  const path = claimPath(input.stateRoot, input.step);
  const claim = newClaim(path, input.journal);
  const breakLease = input.breakClaim ? acquireBreakLease(path, input.step) : null;
  try {
    try {
      createClaim(claim);
      return claim;
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code !== "EEXIST") throw error;
      let existing: CanonicalLegClaim;
      try {
        existing = readClaim(path);
      } catch (readError) {
        if ((readError as NodeJS.ErrnoException).code === "ENOENT") {
          throw new Error(`HXTK ${input.step} claim disappeared while acquiring; retry`, { cause: readError });
        }
        throw readError;
      }
      if (!input.breakClaim) {
        throw claimOccupied(input.step, existing);
      }

      assertDeadLocalClaim(input.step, existing);
      const brokenPath = `${path}.broken-${Date.now()}-${process.pid}`;
      try {
        renameSync(path, brokenPath);
      } catch (renameError) {
        const code = (renameError as NodeJS.ErrnoException).code;
        if (code === "ENOENT" || code === "EEXIST") {
          return refuseAfterBreakRace(path, input.step);
        }
        throw renameError;
      }
      try {
        createClaim(claim);
        return claim;
      } catch (createError) {
        if ((createError as NodeJS.ErrnoException).code !== "EEXIST") throw createError;
        let replacement: CanonicalLegClaim;
        try {
          replacement = readClaim(path);
        } catch (readError) {
          if ((readError as NodeJS.ErrnoException).code === "ENOENT") {
            throw new Error(`another hxtk-reset process won breaking the ${input.step} claim; refusing to acquire it`, { cause: readError });
          }
          throw readError;
        }
        throw claimOccupied(input.step, replacement);
      }
    }
  } finally {
    if (breakLease !== null) finishBreakLease(breakLease);
  }
}

export function releaseCanonicalLegClaim(claim: CanonicalLegClaim): void {
  let existing: CanonicalLegClaim;
  try {
    existing = readClaim(claim.path);
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") return;
    throw error;
  }
  if (existing.token !== PROCESS_CLAIM_TOKEN || claim.token !== PROCESS_CLAIM_TOKEN) {
    console.warn("claim owned by another process; not released");
    return;
  }
  try {
    unlinkSync(claim.path);
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error;
  }
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
  if (!existsSync(path)) mkdirSync(path, { mode: 0o700 });
  assertPrivateDirectory(path, uid);
  chmodSync(path, 0o700);
  assertPrivateDirectory(path, uid);
}

function assertHomeDirectory(path: string): void {
  const stat = lstatSync(path);
  if (!stat.isDirectory() || stat.isSymbolicLink()) {
    throw new Error(`HXTK state root home component ${path} is not a real directory`);
  }
  if ((stat.mode & 0o022) !== 0) {
    throw new Error(`HXTK state root home component ${path} is group/world-writable`);
  }
}

function ensureDefaultRoot(root: string, home: string, uid: number, create: boolean): void {
  assertHomeDirectory(home);
  const relative = root.slice(home.length).split("/").filter(Boolean);
  let current = home;
  for (const component of relative) {
    current = join(current, component);
    try {
      lstatSync(current);
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error;
      if (!create) return;
      ensurePrivateDirectory(current, uid);
      continue;
    }
    assertPrivateDirectory(current, uid);
  }
}

export function resolveCanonicalStateRoot(input: Readonly<{
  vault: string;
  homeDir?: string;
  uid?: number;
  create?: boolean;
}>): string {
  const create = input.create === true;
  const uid = currentUid(input.uid);
  const home = resolve(input.homeDir ?? userInfo().homedir);
  if (!home.startsWith("/")) throw new Error("HXTK state root home must be absolute");
  const root = join(home, ".loyal", "hxtk-reset", input.vault);
  ensureDefaultRoot(root, home, uid, create);
  return root;
}
