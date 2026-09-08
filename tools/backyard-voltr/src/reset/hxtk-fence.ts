import { createHash, randomBytes } from "node:crypto";
import { execFileSync } from "node:child_process";
import {
  chmodSync,
  closeSync,
  constants,
  existsSync,
  fsyncSync,
  fstatSync,
  linkSync,
  openSync,
  lstatSync,
  mkdirSync,
  renameSync,
  readSync,
  unlinkSync,
  statSync,
  writeSync,
} from "node:fs";
import { hostname, userInfo } from "node:os";
import { dirname, join, resolve } from "node:path";

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

function syncDirectory(path: string): void {
  const fd = openSync(path, constants.O_RDONLY);
  try {
    fsyncSync(fd);
  } finally {
    closeSync(fd);
  }
}

function privateTemporaryPath(path: string): string {
  return `${path}.tmp-${process.pid}-${Date.now()}-${randomBytes(8).toString("hex")}`;
}

function writePrivateTemporary(path: string, bytes: Uint8Array): string {
  const temporary = privateTemporaryPath(path);
  const fd = openSync(
    temporary,
    constants.O_WRONLY | constants.O_CREAT | constants.O_EXCL | constants.O_NOFOLLOW,
    0o600,
  );
  try {
    writeAll(fd, bytes);
    fsyncSync(fd);
  } finally {
    closeSync(fd);
  }
  return temporary;
}

function unlinkPrivateTemporary(path: string): void {
  try {
    unlinkSync(path);
    syncDirectory(dirname(path));
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error;
  }
}

function writeExclusivePrivate(path: string, bytes: Uint8Array): void {
  const temporary = writePrivateTemporary(path, bytes);
  try {
    // link(2) publishes the fully fsynced inode without exposing a partial file
    // at the final name and fails atomically when the name is occupied.
    linkSync(temporary, path);
    syncDirectory(dirname(path));
  } catch (error) {
    unlinkPrivateTemporary(temporary);
    throw error;
  }
  unlinkPrivateTemporary(temporary);
}

function atomicWritePrivate(path: string, value: JsonRecord): void {
  const temporary = writePrivateTemporary(path, Buffer.from(`${canonicalJson(value)}\n`));
  try {
    renameSync(temporary, path);
    syncDirectory(dirname(path));
  } catch (error) {
    unlinkPrivateTemporary(temporary);
    throw error;
  }
}

export function writePrivateExclusive(path: string, bytes: Uint8Array): void {
  writeExclusivePrivate(path, bytes);
}

export function writePrivateAtomic(path: string, bytes: Uint8Array): void {
  const temporary = writePrivateTemporary(path, bytes);
  try {
    renameSync(temporary, path);
    syncDirectory(dirname(path));
  } catch (error) {
    unlinkPrivateTemporary(temporary);
    throw error;
  }
}

export function renamePrivateFile(from: string, to: string): void {
  renameSync(from, to);
  syncDirectory(dirname(from));
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

export function readCanonicalState(path: string): Readonly<{
  record: JsonRecord;
  sha256: string;
  bytes: Uint8Array;
}> {
  const result = readBoundJournal(path);
  const generation = canonicalStateGeneration(result.record);
  const generationPath = `${path}.gen-${generation}`;
  let generationBytes: Uint8Array;
  try {
    generationBytes = readPrivateBytes(generationPath);
  } catch (error) {
    throw new Error(
      `STATE_GENERATION_CONFLICT: canonical state ${path} is missing its generation audit inode`,
      { cause: error },
    );
  }
  if (!Buffer.from(result.bytes).equals(Buffer.from(generationBytes))) {
    throw new Error(
      `STATE_GENERATION_CONFLICT: canonical state ${path} does not match ${generationPath}`,
    );
  }
  return result;
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
    current = readCanonicalState(path).record;
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
  const nextBytes = Buffer.from(`${canonicalJson(next)}\n`);
  const temporary = writePrivateTemporary(path, nextBytes);
  const generationPath = `${path}.gen-${next.generation}`;
  let elected = false;
  let renamed = false;
  try {
    // The raw send is reachable only after this process won the `attempted`
    // generation; no claim logic is relied upon for that guarantee.
    try {
      linkSync(temporary, generationPath);
      elected = true;
      syncDirectory(dirname(path));
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code === "EEXIST") {
        throw new Error(
          `STATE_GENERATION_CONFLICT: generation ${next.generation} is already elected`,
          { cause: error },
        );
      }
      throw error;
    }
    // Only the hard-link winner reaches the reader-pointer rename.
    renameSync(temporary, path);
    renamed = true;
    syncDirectory(dirname(path));
  } catch (error) {
    if (!elected) {
      unlinkPrivateTemporary(temporary);
    } else if (!renamed) {
      // A failed pointer publish must not strand the next generation as a
      // permanent conflict. A successful rename deliberately keeps the audit
      // inode and is never cleaned up here.
      try {
        unlinkSync(generationPath);
        syncDirectory(dirname(path));
      } catch (cleanupError) {
        if ((cleanupError as NodeJS.ErrnoException).code !== "ENOENT") throw cleanupError;
      }
    }
    if ((error as Error).message?.startsWith("STATE_GENERATION_CONFLICT")) throw error;
    if ((error as NodeJS.ErrnoException).code === "ENOENT") {
      throw new Error("STATE_GENERATION_CONFLICT: canonical state changed during publish", { cause: error });
    }
    throw error;
  } finally {
    try {
      unlinkSync(temporary);
      syncDirectory(dirname(path));
    } catch (cleanupError) {
      if ((cleanupError as NodeJS.ErrnoException).code !== "ENOENT") throw cleanupError;
    }
  }
  return next;
}

function validLegName(step: string): void {
  if (!/^[a-z0-9-]+$/.test(step)) throw new Error(`invalid canonical HXtk leg name ${step}`);
}

export type CanonicalLegClaim = Readonly<{
  path: string;
  tokenPath: string;
  inode: number;
  pid: number;
  startedAtUnixMs: number;
  processStartTime: string;
  journal: string;
  hostname: string;
  token: string;
}>;

function claimPath(stateRoot: string, step: string): string {
  validLegName(step);
  return join(stateRoot, `${step}.claim`);
}

function claimTokenPath(path: string, token: string): string {
  if (!/^[0-9a-f]{32}$/.test(token)) throw new Error(`invalid HXTK claim token ${token}`);
  return `${path}.${token}`;
}

function processStartTimeForPid(pid: number): string {
  try {
    const output = execFileSync("ps", ["-o", "lstart=", "-p", String(pid)], {
      encoding: "utf8",
      stdio: ["ignore", "pipe", "pipe"],
    }).trim();
    if (output.length === 0) throw new Error("ps returned no process start time");
    return output;
  } catch (error) {
    if (pid === process.pid) return CURRENT_PROCESS_START_TIME;
    throw new Error(`cannot determine process start time for pid ${pid}`, { cause: error });
  }
}

function readClaim(path: string): CanonicalLegClaim {
  const pointer = lstatSync(path);
  if (pointer.isSymbolicLink()) throw new Error(`HXTK claim ${path} is a symbolic link`);
  const parsed = JSON.parse(Buffer.from(readPrivateBytes(path)).toString("utf8")) as Partial<CanonicalLegClaim>;
  if (!Number.isSafeInteger(parsed.pid) || (parsed.pid ?? 0) <= 0
    || typeof parsed.startedAtUnixMs !== "number"
    || typeof parsed.processStartTime !== "string"
    || typeof parsed.journal !== "string"
    || typeof parsed.hostname !== "string"
    || typeof parsed.token !== "string"
    || !/^[0-9a-f]{32}$/.test(parsed.token)) {
    throw new Error(`HXTK claim ${path} is malformed; refusing to break it`);
  }
  return {
    path,
    tokenPath: claimTokenPath(path, parsed.token),
    inode: pointer.ino,
    pid: parsed.pid!,
    startedAtUnixMs: parsed.startedAtUnixMs,
    processStartTime: parsed.processStartTime,
    journal: parsed.journal,
    hostname: parsed.hostname,
    token: parsed.token,
  };
}

const PROCESS_CLAIM_TOKEN = randomBytes(16).toString("hex");
const OWNED_CLAIM_TOKENS = new Set<string>();
// macOS `ps` is the authoritative cross-process source. Sandboxed test
// runners can deny that utility even for the current process, so retain a
// process-local wall-clock start marker as the only same-process fallback.
const CURRENT_PROCESS_START_TIME = new Date(
  Date.now() - Math.round(process.uptime() * 1000),
).toISOString();

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
  if (processStartTimeForPid(existing.pid) !== existing.processStartTime) return;
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
    renamePrivateFile(lease.path, `${lease.path}.done-${Date.now()}-${process.pid}`);
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
    renamePrivateFile(path, stalePath);
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

function newClaim(path: string, journal: string): Omit<CanonicalLegClaim, "inode"> {
  const token = randomBytes(16).toString("hex");
  return {
    path,
    tokenPath: claimTokenPath(path, token),
    pid: process.pid,
    startedAtUnixMs: Date.now(),
    processStartTime: processStartTimeForPid(process.pid),
    journal,
    hostname: hostname(),
    token,
  };
}

function createClaim(claim: Omit<CanonicalLegClaim, "inode">): CanonicalLegClaim {
  writeExclusivePrivate(claim.tokenPath, Buffer.from(`${canonicalJson({
    pid: claim.pid,
    startedAtUnixMs: claim.startedAtUnixMs,
    processStartTime: claim.processStartTime,
    hostname: claim.hostname,
    journal: claim.journal,
    token: claim.token,
  })}\n`));
  try {
    linkSync(claim.tokenPath, claim.path);
    syncDirectory(dirname(claim.path));
  } catch (error) {
    try {
      unlinkSync(claim.tokenPath);
      syncDirectory(dirname(claim.path));
    } catch (cleanupError) {
      if ((cleanupError as NodeJS.ErrnoException).code !== "ENOENT") throw cleanupError;
    }
    throw error;
  }
  const inode = statSync(claim.tokenPath).ino;
  const owned = { ...claim, inode };
  OWNED_CLAIM_TOKENS.add(claim.token);
  return owned;
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
      return createClaim(claim);
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
      const brokenPath = `${existing.tokenPath}.broken-${Date.now()}-${process.pid}`;
      try {
        // Break the exact dead token inode inspected above. The fixed pointer
        // remains in place until its inode is checked immediately before the
        // unlink, so a replacement live claim cannot be removed by an ABA
        // breaker.
        renamePrivateFile(existing.tokenPath, brokenPath);
      } catch (renameError) {
        const code = (renameError as NodeJS.ErrnoException).code;
        if (code === "ENOENT" || code === "EEXIST") {
          return refuseAfterBreakRace(path, input.step);
        }
        throw renameError;
      }
      let pointer: ReturnType<typeof statSync>;
      try {
        pointer = statSync(path);
      } catch (statError) {
        if ((statError as NodeJS.ErrnoException).code === "ENOENT") {
          return refuseAfterBreakRace(path, input.step);
        }
        throw statError;
      }
      if (pointer.ino !== existing.inode) return refuseAfterBreakRace(path, input.step);
      try {
        unlinkSync(path);
        syncDirectory(dirname(path));
      } catch (unlinkError) {
        const code = (unlinkError as NodeJS.ErrnoException).code;
        if (code === "ENOENT" || code === "EEXIST") return refuseAfterBreakRace(path, input.step);
        throw unlinkError;
      }
      try {
        return createClaim(claim);
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
  if (!OWNED_CLAIM_TOKENS.has(claim.token)) {
    console.warn("claim token is not owned by this process; not released");
    return;
  }
  let tokenStat: ReturnType<typeof statSync>;
  try {
    tokenStat = statSync(claim.tokenPath);
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") {
      OWNED_CLAIM_TOKENS.delete(claim.token);
      return;
    }
    throw error;
  }
  let pointerStat: ReturnType<typeof statSync>;
  try {
    pointerStat = statSync(claim.path);
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") {
      unlinkSync(claim.tokenPath);
      syncDirectory(dirname(claim.path));
      OWNED_CLAIM_TOKENS.delete(claim.token);
      return;
    }
    throw error;
  }
  if (pointerStat.ino !== tokenStat.ino) {
    console.warn("claim pointer inode changed; not released");
    return;
  }
  unlinkSync(claim.path);
  syncDirectory(dirname(claim.path));
  unlinkSync(claim.tokenPath);
  syncDirectory(dirname(claim.path));
  OWNED_CLAIM_TOKENS.delete(claim.token);
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
