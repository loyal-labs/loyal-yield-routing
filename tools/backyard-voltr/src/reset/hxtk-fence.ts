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
  readdirSync,
  readSync,
  unlinkSync,
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
  "attemptGeneration",
  "journalBindingSha256",
  "pendingBindingSha256",
  "sendStatus",
  // Recovery-only metadata may be added before a pending inode is renamed to
  // its abort artifact; it is not part of the immutable transaction binding.
  "rearmable",
  "attemptedExpiryProof",
  "lastValidBlockHeight",
  "finalizedBlockHeight",
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

type FileIdentity = Readonly<{
  dev: number;
  ino: number;
}>;

function sameFileIdentity(left: FileIdentity, right: FileIdentity): boolean {
  return left.dev === right.dev && left.ino === right.ino;
}

function assertPrivateDescriptor(path: string, fd: number, stat = fstatSync(fd)) {
  const uid = currentUid();
  if (!stat.isFile()) throw new Error(`HXTK journal ${path} is not a regular file`);
  if (stat.uid !== uid) throw new Error(`HXTK journal ${path} is not owned by the current uid`);
  if ((stat.mode & 0o077) !== 0) throw new Error(`HXTK journal ${path} is group/other-accessible`);
  return stat;
}

function readPrivateBytesFromDescriptor(path: string, fd: number, initialStat = fstatSync(fd)): Uint8Array {
  const stat = assertPrivateDescriptor(path, fd, initialStat);
  const bytes = new Uint8Array(stat.size);
  let offset = 0;
  while (offset < bytes.length) {
    const read = readSync(fd, bytes, offset, bytes.length - offset, offset);
    if (read === 0) throw new Error(`HXTK journal ${path} changed while reading`);
    offset += read;
  }
  return bytes;
}

function readPrivateBytes(path: string): Uint8Array {
  const fd = openSync(path, constants.O_RDONLY | constants.O_NOFOLLOW);
  try {
    return readPrivateBytesFromDescriptor(path, fd);
  } finally {
    closeSync(fd);
  }
}

function openPrivateIdentity(path: string): Readonly<{ fd: number; stat: ReturnType<typeof fstatSync> }> {
  const fd = openSync(path, constants.O_RDONLY | constants.O_NOFOLLOW);
  try {
    const stat = assertPrivateDescriptor(path, fd);
    return { fd, stat };
  } catch (error) {
    closeSync(fd);
    throw error;
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
  "reportSlotAgeAtSend",
  "reportSlotCurrentSlotAtSend",
  "reportSlotObservedSlot",
  "reportSlotMarginSlots",
  "reportSlotMaxAgeSlots",
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

function generationPaths(path: string): Map<number, string> {
  const directory = dirname(path);
  const prefix = `${path}.gen-`;
  const generations = new Map<number, string>();
  let entries: ReturnType<typeof readdirSync>;
  try {
    entries = readdirSync(directory, { withFileTypes: true });
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") return generations;
    throw error;
  }
  for (const entry of entries) {
    const candidate = join(directory, entry.name);
    if (!candidate.startsWith(prefix)) continue;
    const suffix = candidate.slice(prefix.length);
    if (!/^\d+$/.test(suffix)) continue;
    const generation = Number(suffix);
    if (!Number.isSafeInteger(generation) || generation < 0) {
      throw new Error(`STATE_GENERATION_CONFLICT: invalid generation filename ${candidate}`);
    }
    const stat = lstatSync(candidate);
    if (stat.isSymbolicLink() || !stat.isFile()) {
      throw new Error(`STATE_GENERATION_CONFLICT: generation audit inode ${candidate} is not a regular file`);
    }
    generations.set(generation, candidate);
  }
  return generations;
}

function highestContiguousGeneration(path: string): number | null {
  const generations = generationPaths(path);
  if (!generations.has(0)) return null;
  let highest = 0;
  while (generations.has(highest + 1)) highest += 1;
  return highest;
}

function statePointerBehind(path: string, pointerGeneration: number | null, highest: number): Error {
  return new Error(
    `STATE_POINTER_BEHIND: ${path} pointer generation ${pointerGeneration === null ? "absent" : pointerGeneration} `
    + `is behind highest contiguous generation ${highest}`,
  );
}

function assertCanonicalStateBytes(
  path: string,
  result: Readonly<{ record: JsonRecord; sha256: string; bytes: Uint8Array }>,
): Readonly<{ record: JsonRecord; sha256: string; bytes: Uint8Array }> {
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

export function readCanonicalState(
  path: string,
  options: Readonly<{ allowRollForward?: boolean }> = {},
): Readonly<{
  record: JsonRecord;
  sha256: string;
  bytes: Uint8Array;
}> {
  let result: Readonly<{ record: JsonRecord; sha256: string; bytes: Uint8Array }>;
  try {
    result = readBoundJournal(path);
  } catch (error) {
    const highest = highestContiguousGeneration(path);
    if (highest === null) throw error;
    if (!options.allowRollForward) throw statePointerBehind(path, null, highest);
    writePrivateAtomic(path, readPrivateBytes(`${path}.gen-${highest}`));
    result = readBoundJournal(path);
  }

  let pointerGeneration = canonicalStateGeneration(result.record);
  const highest = highestContiguousGeneration(path);
  if (highest === null || pointerGeneration > highest) {
    throw new Error(
      `STATE_GENERATION_CONFLICT: canonical state ${path} generation ${pointerGeneration} `
      + `has no contiguous generation audit chain`,
    );
  }
  if (pointerGeneration < highest) {
    if (!options.allowRollForward) throw statePointerBehind(path, pointerGeneration, highest);
    // This is the only read-side repair. It is used only by a caller that
    // already holds the leg claim; simulation and other read-only paths leave
    // the pointer untouched and report STATE_POINTER_BEHIND instead.
    writePrivateAtomic(path, readPrivateBytes(`${path}.gen-${highest}`));
    result = readBoundJournal(path);
    pointerGeneration = canonicalStateGeneration(result.record);
    if (pointerGeneration !== highest) {
      throw new Error(
        `STATE_GENERATION_CONFLICT: canonical state ${path} roll-forward published generation `
        + `${pointerGeneration}, expected ${highest}`,
      );
    }
  }
  return assertCanonicalStateBytes(path, result);
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
    current = readCanonicalState(path, { allowRollForward: true }).record;
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
  dev: number;
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
  // Open once with O_NOFOLLOW and keep this descriptor's fstat identity for
  // every comparison below. A separate lstat/open pair permits an ABA swap.
  const fd = openSync(path, constants.O_RDONLY | constants.O_NOFOLLOW);
  try {
    const pointer = assertPrivateDescriptor(path, fd);
    const parsed = JSON.parse(Buffer.from(
      readPrivateBytesFromDescriptor(path, fd, pointer),
    ).toString("utf8")) as Partial<CanonicalLegClaim>;
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
      dev: pointer.dev,
      inode: pointer.ino,
      pid: parsed.pid!,
      startedAtUnixMs: parsed.startedAtUnixMs,
      processStartTime: parsed.processStartTime,
      journal: parsed.journal,
      hostname: parsed.hostname,
      token: parsed.token,
    };
  } finally {
    closeSync(fd);
  }
}

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
  if (!processIsDeadLocal(existing.pid, existing.processStartTime)) {
    throw claimOccupied(step, existing);
  }
}

function processIsDeadLocal(pid: number, recordedStartTime: string): boolean {
  try {
    process.kill(pid, 0);
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ESRCH") return true;
    throw error;
  }
  try {
    // A sandbox may permit pid existence checks while denying the process
    // table lookup. Unknown start time is conservatively live; only a
    // positively different recorded start time permits takeover.
    return processStartTimeForPid(pid) !== recordedStartTime;
  } catch (error) {
    if (error instanceof Error && error.message.startsWith("cannot determine process start time")) {
      return false;
    }
    throw error;
  }
}

function refuseAfterBreakRace(path: string, step: string): never {
  try {
    throw claimOccupied(step, readClaim(path));
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error;
  }
  throw new Error(`another hxtk-reset process won breaking the ${step} claim; refusing to acquire it`);
}

export type ClaimBreakTransitionStep = "breaking-marker" | "breaking-renamed" | "breaking-pointer";

type ClaimBreakMarker = Readonly<{
  path: string;
  targetToken: string;
  targetTokenPath: string;
  targetDev: number;
  targetIno: number;
  targetPid: number;
  targetProcessStartTime: string;
}>;

function breakingMarkerPath(claimPath: string, token: string): string {
  if (!/^[0-9a-f]{32}$/.test(token)) throw new Error(`invalid HXTK breaking token ${token}`);
  return `${claimPath}.breaking.${token}`;
}

function readBreakingMarker(path: string): ClaimBreakMarker {
  const parsed = JSON.parse(Buffer.from(readPrivateBytes(path)).toString("utf8")) as Partial<ClaimBreakMarker>;
  if (typeof parsed.targetToken !== "string"
    || !/^[0-9a-f]{32}$/.test(parsed.targetToken)
    || typeof parsed.targetTokenPath !== "string"
    || !Number.isSafeInteger(parsed.targetDev)
    || !Number.isSafeInteger(parsed.targetIno)
    || !Number.isSafeInteger(parsed.targetPid)
    || typeof parsed.targetProcessStartTime !== "string") {
    throw new Error(`HXTK claim break marker ${path} is malformed; refusing to continue`);
  }
  return {
    path,
    targetToken: parsed.targetToken,
    targetTokenPath: parsed.targetTokenPath,
    targetDev: parsed.targetDev!,
    targetIno: parsed.targetIno!,
    targetPid: parsed.targetPid!,
    targetProcessStartTime: parsed.targetProcessStartTime,
  };
}

function electBreakingMarker(existing: CanonicalLegClaim): ClaimBreakMarker {
  const path = breakingMarkerPath(existing.path, existing.token);
  const marker: ClaimBreakMarker = {
    path,
    targetToken: existing.token,
    targetTokenPath: existing.tokenPath,
    targetDev: existing.dev,
    targetIno: existing.inode,
    targetPid: existing.pid,
    targetProcessStartTime: existing.processStartTime,
  };
  try {
    writeExclusivePrivate(path, Buffer.from(`${canonicalJson({
      targetToken: marker.targetToken,
      targetTokenPath: marker.targetTokenPath,
      targetDev: marker.targetDev,
      targetIno: marker.targetIno,
      targetPid: marker.targetPid,
      targetProcessStartTime: marker.targetProcessStartTime,
    })}\n`));
    return marker;
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code !== "EEXIST") throw error;
    const occupied = readBreakingMarker(path);
    if (occupied.targetToken === marker.targetToken
      && occupied.targetDev === marker.targetDev
      && occupied.targetIno === marker.targetIno) return occupied;
    throw new Error(`HXTK ${existing.path} has a foreign claim-break marker`, { cause: error });
  }
}

function unlinkPointerIfIdentity(path: string, expected: FileIdentity): boolean {
  const fd = openSync(path, constants.O_RDONLY | constants.O_NOFOLLOW);
  try {
    const stat = assertPrivateDescriptor(path, fd);
    if (!sameFileIdentity({ dev: stat.dev, ino: stat.ino }, expected)) return false;
    unlinkSync(path);
    syncDirectory(dirname(path));
    return true;
  } finally {
    closeSync(fd);
  }
}

function finishBreakingMarker(
  claimPath: string,
  marker: ClaimBreakMarker,
  afterPointerUnlink?: () => void,
): boolean {
  let pointer: CanonicalLegClaim;
  try {
    pointer = readClaim(claimPath);
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") {
      unlinkPrivateTemporary(marker.path);
      return false;
    }
    throw error;
  }
  if (pointer.token !== marker.targetToken
    || pointer.dev !== marker.targetDev
    || pointer.inode !== marker.targetIno) {
    // A replacement claim owns the pointer. The marker is no longer useful,
    // but its mismatch must never remove the replacement inode.
    unlinkPrivateTemporary(marker.path);
    return false;
  }
  if (!unlinkPointerIfIdentity(claimPath, { dev: marker.targetDev, ino: marker.targetIno })) {
    unlinkPrivateTemporary(marker.path);
    return false;
  }
  afterPointerUnlink?.();
  unlinkPrivateTemporary(marker.path);
  return true;
}

function resumeBreakingMarkers(claimPath: string): boolean {
  const directory = dirname(claimPath);
  const prefix = `${claimPath}.breaking.`;
  let entries: ReturnType<typeof readdirSync>;
  try {
    entries = readdirSync(directory, { withFileTypes: true });
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") return false;
    throw error;
  }
  let finished = false;
  for (const entry of entries) {
    const candidate = join(directory, entry.name);
    if (!candidate.startsWith(prefix) || !entry.isFile()) continue;
    const marker = readBreakingMarker(candidate);
    if (finishBreakingMarker(claimPath, marker)) finished = true;
  }
  return finished;
}

type BreakLease = Readonly<{
  path: string;
  tokenPath: string;
  dev: number;
  ino: number;
  pid: number;
  startedAtUnixMs: number;
  hostname: string;
  processStartTime: string;
  token: string;
}>;

const OWNED_BREAK_LEASE_TOKENS = new Set<string>();

function breakLeasePath(claimPath: string): string {
  return `${claimPath}.break-lease`;
}

function breakLeaseTokenPath(path: string, token: string): string {
  if (!/^[0-9a-f]{32}$/.test(token)) throw new Error(`invalid HXTK break lease token ${token}`);
  return `${path}.${token}`;
}

function readBreakLease(path: string): BreakLease {
  const fd = openSync(path, constants.O_RDONLY | constants.O_NOFOLLOW);
  try {
    const pointer = assertPrivateDescriptor(path, fd);
    const parsed = JSON.parse(Buffer.from(
      readPrivateBytesFromDescriptor(path, fd, pointer),
    ).toString("utf8")) as Partial<BreakLease>;
    if (!Number.isSafeInteger(parsed.pid) || (parsed.pid ?? 0) <= 0
      || typeof parsed.startedAtUnixMs !== "number"
      || typeof parsed.hostname !== "string"
      || typeof parsed.processStartTime !== "string"
      || typeof parsed.token !== "string"
      || !/^[0-9a-f]{32}$/.test(parsed.token)) {
      throw new Error(`HXTK break lease ${path} is malformed; refusing to break it`);
    }
    return {
      path,
      tokenPath: breakLeaseTokenPath(path, parsed.token),
      dev: pointer.dev,
      ino: pointer.ino,
      pid: parsed.pid!,
      startedAtUnixMs: parsed.startedAtUnixMs,
      hostname: parsed.hostname,
      processStartTime: parsed.processStartTime,
      token: parsed.token,
    };
  } finally {
    closeSync(fd);
  }
}

function newBreakLease(path: string): Omit<BreakLease, "dev" | "ino"> {
  const token = randomBytes(16).toString("hex");
  return {
    path,
    tokenPath: breakLeaseTokenPath(path, token),
    pid: process.pid,
    startedAtUnixMs: Date.now(),
    hostname: hostname(),
    processStartTime: processStartTimeForPid(process.pid),
    token,
  };
}

function createBreakLease(lease: Omit<BreakLease, "dev" | "ino">): BreakLease {
  writeExclusivePrivate(lease.tokenPath, Buffer.from(`${canonicalJson({
    pid: lease.pid,
    startedAtUnixMs: lease.startedAtUnixMs,
    hostname: lease.hostname,
    processStartTime: lease.processStartTime,
    token: lease.token,
  })}\n`));
  try {
    linkSync(lease.tokenPath, lease.path);
    syncDirectory(dirname(lease.path));
  } catch (error) {
    unlinkPrivateTemporary(lease.tokenPath);
    throw error;
  }
  const identity = openPrivateIdentity(lease.tokenPath);
  try {
    const created = { ...lease, dev: Number(identity.stat.dev), ino: Number(identity.stat.ino) };
    OWNED_BREAK_LEASE_TOKENS.add(lease.token);
    return created;
  } finally {
    closeSync(identity.fd);
  }
}

function finishBreakLease(lease: BreakLease): void {
  let existing: BreakLease;
  try {
    existing = readBreakLease(lease.path);
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") return;
    throw error;
  }
  if (existing.token !== lease.token
    || !sameFileIdentity(existing, lease)
    || !OWNED_BREAK_LEASE_TOKENS.has(lease.token)) return;
  if (unlinkPointerIfIdentity(lease.path, lease)) {
    unlinkPrivateTemporary(lease.tokenPath);
  }
  OWNED_BREAK_LEASE_TOKENS.delete(lease.token);
}

function acquireBreakLease(claimPath: string, step: string): BreakLease {
  const path = breakLeasePath(claimPath);
  const lease = newBreakLease(path);
  try {
    return createBreakLease(lease);
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
  if (!processIsDeadLocal(existing.pid, existing.processStartTime)) {
    return refuseAfterBreakRace(claimPath, step);
  }

  const stalePath = `${existing.tokenPath}.stale-${Date.now()}-${process.pid}`;
  try {
    renamePrivateFile(existing.tokenPath, stalePath);
  } catch (renameError) {
    const code = (renameError as NodeJS.ErrnoException).code;
    if (code !== "ENOENT" && code !== "EEXIST") throw renameError;
  }
  let pointer: FileIdentity;
  try {
    const opened = openPrivateIdentity(path);
    try {
      pointer = { dev: Number(opened.stat.dev), ino: Number(opened.stat.ino) };
    } finally {
      closeSync(opened.fd);
    }
  } catch (pointerError) {
    if ((pointerError as NodeJS.ErrnoException).code === "ENOENT") {
      return refuseAfterBreakRace(claimPath, step);
    }
    throw pointerError;
  }
  if (!sameFileIdentity(pointer, existing)) return refuseAfterBreakRace(claimPath, step);
  if (!unlinkPointerIfIdentity(path, existing)) return refuseAfterBreakRace(claimPath, step);
  try {
    return createBreakLease(lease);
  } catch (createError) {
    if ((createError as NodeJS.ErrnoException).code !== "EEXIST") throw createError;
    return refuseAfterBreakRace(claimPath, step);
  }
}

function newClaim(path: string, journal: string): Omit<CanonicalLegClaim, "dev" | "inode"> {
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

function createClaim(claim: Omit<CanonicalLegClaim, "dev" | "inode">): CanonicalLegClaim {
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
  const identity = openPrivateIdentity(claim.tokenPath);
  const owned = { ...claim, dev: Number(identity.stat.dev), inode: Number(identity.stat.ino) };
  closeSync(identity.fd);
  OWNED_CLAIM_TOKENS.add(claim.token);
  return owned;
}

export function acquireCanonicalLegClaim(input: Readonly<{
  stateRoot: string;
  step: string;
  journal: string;
  breakClaim?: boolean;
  faultAfterBreakStep?: (step: ClaimBreakTransitionStep) => void;
}>): CanonicalLegClaim {
  const path = claimPath(input.stateRoot, input.step);
  const claim = newClaim(path, input.journal);
  const breakLease = input.breakClaim ? acquireBreakLease(path, input.step) : null;
  try {
    resumeBreakingMarkers(path);
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
        // A marker proves that a previous breaker already proved this local
        // claim dead. If an older interrupted break left only the pointer's
        // hard link, a dead recorded process is still required before cleanup.
        if (!existsSync(existing.tokenPath) && processIsDeadLocal(existing.pid, existing.processStartTime)) {
          if (!unlinkPointerIfIdentity(path, { dev: existing.dev, ino: existing.inode })) {
            return refuseAfterBreakRace(path, input.step);
          }
          return createClaim(claim);
        }
        throw claimOccupied(input.step, existing);
      }

      assertDeadLocalClaim(input.step, existing);
      const marker = electBreakingMarker(existing);
      input.faultAfterBreakStep?.("breaking-marker");
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
          if (resumeBreakingMarkers(path)) return createClaim(claim);
          return refuseAfterBreakRace(path, input.step);
        }
        throw renameError;
      }
      input.faultAfterBreakStep?.("breaking-renamed");
      const finished = finishBreakingMarker(path, marker, () => {
        input.faultAfterBreakStep?.("breaking-pointer");
      });
      if (!finished) return refuseAfterBreakRace(path, input.step);
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
  let tokenIdentity: FileIdentity;
  try {
    const opened = openPrivateIdentity(claim.tokenPath);
    try {
      tokenIdentity = { dev: Number(opened.stat.dev), ino: Number(opened.stat.ino) };
    } finally {
      closeSync(opened.fd);
    }
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") {
      OWNED_CLAIM_TOKENS.delete(claim.token);
      return;
    }
    throw error;
  }
  let pointerIdentity: FileIdentity;
  try {
    const opened = openPrivateIdentity(claim.path);
    try {
      pointerIdentity = { dev: Number(opened.stat.dev), ino: Number(opened.stat.ino) };
    } finally {
      closeSync(opened.fd);
    }
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") {
      unlinkSync(claim.tokenPath);
      syncDirectory(dirname(claim.path));
      OWNED_CLAIM_TOKENS.delete(claim.token);
      return;
    }
    throw error;
  }
  if (!sameFileIdentity(pointerIdentity, tokenIdentity)) {
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
  attemptToken?: string;
  signedWireBase64?: string;
  messageBase64?: string;
  lastValidBlockHeight?: number;
  pendingRecord?: JsonRecord;
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
    attemptToken: providedAttemptToken,
    signedWireBase64,
    messageBase64,
    lastValidBlockHeight,
    pendingRecord,
    journalExists,
    existing,
    nowUnixMs = Date.now(),
  } = input;
  assertAllowRepeatFlag(step, repeatable, allowRepeat);

  const attemptToken = providedAttemptToken ?? randomBytes(16).toString("hex");
  if (!/^[0-9a-f]{32}$/.test(attemptToken)) {
    throw new Error(`${step} canonical state has an invalid attemptToken`);
  }

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
    attemptToken,
    ...(signedWireBase64 === undefined ? {} : { signedWireBase64 }),
    ...(messageBase64 === undefined ? {} : { messageBase64 }),
    ...(lastValidBlockHeight === undefined ? {} : { lastValidBlockHeight }),
    ...(pendingRecord === undefined ? {} : { pendingRecord }),
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
  const attemptToken = pending.attemptToken;
  const attemptMatches = typeof attemptToken !== "string"
    || String(canonicalState.attemptToken ?? "") === attemptToken;
  const matches = transaction !== null
    && String(pending.pendingBindingSha256 ?? "") === binding
    && attemptMatches
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
