import { createHash } from "node:crypto";
import { execFileSync } from "node:child_process";
import { chmodSync, mkdirSync, readFileSync, statSync, writeFileSync } from "node:fs";
import { resolve } from "node:path";
import { fileURLToPath } from "node:url";

/**
 * The Rust bridge is the durable authority for restoration fencing.  This
 * module is intentionally only an adapter: it stages an input envelope and
 * validates the bridge's one-line result.  It does not load a signer, call
 * Solana, or submit a manager transaction.
 */

const REPOSITORY_ROOT = resolve(fileURLToPath(new URL("../../../..", import.meta.url)));
const DEFAULT_BINARY = resolve(REPOSITORY_ROOT, "target/debug/backyard-voltr-restoration-bridge");
const PHASE_A_VERDICT = "BACKYARD_VOLTR_RESTORATION_BRIDGE_PHASE_A_PASS";
const PHASE_B_VERDICT = "BACKYARD_VOLTR_RESTORATION_BRIDGE_PHASE_B_PASS";
const ROUTE_ID = "loyal-backyard-four-market-usdc-v1";
const ROUTE_SPEC_SHA256 = "a68ef28c8b9a9c8e34106cf78f1d10624d8bc9ebfd366cc15cbc5b273ecdf3e3";
const VAULT = "AdwKLBQWKxNewpkjMFMz4NyKit7qXygGpjkqHBCWcriK";
const CLUSTER = "mainnet-beta";
const STRATEGY_RESERVES: Readonly<Record<string, string>> = {
  main: "D6q6wuQSrifJKZYpR1M8R4YawnLDtDsMmWM1NbBmgJ59",
  onre: "AYL4LMc4ZCVyq3Z7XPJGWDM4H9PiWjqXAAuuHBEGVR2Z",
  prime: "9GJ9GBRwCp4pHmWrQ43L5xpc9Vykg7jnfwcFGN8FoHYu",
  maple: "Atj6UREVWa7WxbF2EMKNyfmYUY1U1txughe2gjhcPDCo",
};

type JsonObject = Readonly<Record<string, unknown>>;

export type RestorationManagerSignedIntent = Readonly<{
  managerIntentId: string;
  lifecycleId: string;
  strategyId: string;
  reserve: string;
  amountRaw: number;
  routeAuthorizationSha256: string;
  protectedPrestateSha256: string;
  protectedAddressSetSha256: string;
  protectedContextSlot: number;
  signedTransactionHex: string;
  signedTransactionSha256: string;
  messageSha256: string;
  expectedSignature: string;
  recentBlockhash: string;
  lastValidBlockHeight: number;
  feePayer: string;
  compiledFeeLamports: number;
  writableAccountKeys: readonly string[];
  logicalConflictKeys: readonly string[];
}>;

export type RestorationBridgePhaseAInput = Readonly<{
  schemaVersion: 1;
  phase: "prepare";
  cluster: typeof CLUSTER;
  routeId: typeof ROUTE_ID;
  routeSpecSha256: string;
  vault: typeof VAULT;
  owner: string;
  leaseSeconds: number;
  originId: string;
  generation: number;
  legId: string;
  signedIntent: RestorationManagerSignedIntent;
}>;

export type RestorationBridgeToken = Readonly<{
  schemaVersion: 1;
  eventId: number;
  cluster: string;
  owner: string;
  fencingToken: number;
  originId: string;
  generation: number;
  legId: string;
  managerIntentId: string;
  expectedSignature: string;
  signedTransactionSha256: string;
  messageSha256: string;
  strategyId: string;
  reserve: string;
  amountRaw: number;
  lifecycleId: string;
  routeAuthorizationSha256: string;
  protectedPrestateSha256: string;
  protectedAddressSetSha256: string;
  protectedContextSlot: number;
}>;

export type RestorationBridgeManagerHandoff = Readonly<{
  operation: "manager-withdraw";
  strategyId: string;
  reserve: string;
  amountRaw: number;
  eventId: number;
  fencingToken: number;
  leaseExpiresAt: string;
  expectedSignature: string;
}>;

export type RestorationBridgePhaseAOutput = Readonly<{
  verdict: typeof PHASE_A_VERDICT;
  broadcast: false;
  signerLoaded: false;
  phase: "prepare";
  token: RestorationBridgeToken;
  tokenSha256: string;
  managerHandoff: RestorationBridgeManagerHandoff;
  nextStep: string;
}>;

export type RestorationBridgeConfirmation = Readonly<{
  managerIntentId: string;
  lifecycleId: string;
  strategyId: string;
  reserve: string;
  amountRaw: number;
  routeAuthorizationSha256: string;
  signedTransactionSha256: string;
  messageSha256: string;
  expectedSignature: string;
  confirmedSlot: number;
  readbackContextSlot: number;
  commitment: "confirmed";
  managerTransactionSignature: string;
  idleRawAfter: number;
  remainingShortfallRaw: number;
  readbackFingerprint: string;
}>;

export type RestorationBridgeCompletion = Readonly<{
  eventId: number;
  originId: string;
  generation: number;
  legId: string;
  state: "acknowledged";
  acknowledged: true;
  canceledSiblingCount: number;
}>;

export type RestorationBridgePhaseBInput = Readonly<{
  schemaVersion: 1;
  phase: "confirm";
  token: RestorationBridgeToken;
  confirmation: RestorationBridgeConfirmation;
}>;

export type RestorationBridgeRunOptions = Readonly<{
  evidenceDirectory: string;
  binaryPath?: string;
}>;

export type RestorationBridgePhaseAResult = Readonly<RestorationBridgePhaseAOutput & { inputPath: string; inputFileSha256: string }>;
export type RestorationBridgePhaseBResult = Readonly<{
  verdict: typeof PHASE_B_VERDICT;
  broadcast: false;
  signerLoaded: false;
  phase: "confirm";
  completion: RestorationBridgeCompletion;
  tokenSha256: string;
  inputPath: string;
  inputFileSha256: string;
}>;

function object(value: unknown, label: string): JsonObject {
  if (value === null || typeof value !== "object" || Array.isArray(value)) throw new Error(`${label} must be an object`);
  return value as JsonObject;
}

function exactKeys(value: JsonObject, keys: readonly string[], label: string): void {
  const expected = new Set(keys);
  for (const key of Object.keys(value)) if (!expected.has(key)) throw new Error(`${label} contains unknown field ${key}`);
  for (const key of keys) if (!(key in value)) throw new Error(`${label} is missing ${key}`);
}

function sha(value: unknown, label: string): string {
  if (typeof value !== "string" || !/^[0-9a-f]{64}$/.test(value)) throw new Error(`${label} must be a lowercase SHA-256`);
  return value;
}

function nonEmpty(value: unknown, label: string): string {
  if (typeof value !== "string" || value.length === 0) throw new Error(`${label} must be non-empty`);
  return value;
}

function positiveInteger(value: unknown, label: string): number {
  if (typeof value !== "number" || !Number.isSafeInteger(value) || value <= 0) throw new Error(`${label} must be a positive safe integer`);
  return value;
}

function nonNegativeInteger(value: unknown, label: string): number {
  if (typeof value !== "number" || !Number.isSafeInteger(value) || value < 0) throw new Error(`${label} must be a non-negative safe integer`);
  return value;
}

function hashJson(value: unknown): string {
  return createHash("sha256").update(JSON.stringify(value)).digest("hex");
}

function validateShaFields(value: JsonObject, fields: readonly string[], label: string): void {
  for (const field of fields) sha(value[field], `${label}.${field}`);
}

function validateSignedIntent(value: unknown): RestorationManagerSignedIntent {
  const row = object(value, "signedIntent");
  exactKeys(row, ["managerIntentId", "lifecycleId", "strategyId", "reserve", "amountRaw", "routeAuthorizationSha256", "protectedPrestateSha256", "protectedAddressSetSha256", "protectedContextSlot", "signedTransactionHex", "signedTransactionSha256", "messageSha256", "expectedSignature", "recentBlockhash", "lastValidBlockHeight", "feePayer", "compiledFeeLamports", "writableAccountKeys", "logicalConflictKeys"], "signedIntent");
  const strategyId = nonEmpty(row.strategyId, "signedIntent.strategyId");
  if (!Object.prototype.hasOwnProperty.call(STRATEGY_RESERVES, strategyId) || row.reserve !== STRATEGY_RESERVES[strategyId]) throw new Error("signedIntent strategy/reserve is not an approved four-market pair");
  const signedTransactionHex = nonEmpty(row.signedTransactionHex, "signedIntent.signedTransactionHex");
  if (!/^[0-9a-f]+$/.test(signedTransactionHex) || signedTransactionHex.length < 130 || signedTransactionHex.length % 2 !== 0 || signedTransactionHex.slice(0, 2) !== "01") throw new Error("signedIntent.signedTransactionHex is not a canonical Solana wire");
  const writableAccountKeys = row.writableAccountKeys;
  const logicalConflictKeys = row.logicalConflictKeys;
  if (!Array.isArray(writableAccountKeys) || writableAccountKeys.length === 0 || writableAccountKeys.some((key) => typeof key !== "string" || key.length === 0)) throw new Error("signedIntent.writableAccountKeys is malformed");
  if (!Array.isArray(logicalConflictKeys) || logicalConflictKeys.length === 0 || logicalConflictKeys.some((key) => typeof key !== "string" || key.length === 0)) throw new Error("signedIntent.logicalConflictKeys is malformed");
  const expectedConflictKeys = [`kamino:reserve:${row.reserve as string}`, `voltr:vault:${VAULT}`].sort();
  if (JSON.stringify([...logicalConflictKeys].sort()) !== JSON.stringify(expectedConflictKeys)) throw new Error("signedIntent.logicalConflictKeys are not the exact vault/reserve fence");
  validateShaFields(row, ["lifecycleId", "routeAuthorizationSha256", "protectedPrestateSha256", "protectedAddressSetSha256", "signedTransactionSha256", "messageSha256"], "signedIntent");
  return {
    managerIntentId: sha(row.managerIntentId, "signedIntent.managerIntentId"), lifecycleId: row.lifecycleId as string, strategyId, reserve: row.reserve as string, amountRaw: positiveInteger(row.amountRaw, "signedIntent.amountRaw"), routeAuthorizationSha256: row.routeAuthorizationSha256 as string, protectedPrestateSha256: row.protectedPrestateSha256 as string, protectedAddressSetSha256: row.protectedAddressSetSha256 as string, protectedContextSlot: positiveInteger(row.protectedContextSlot, "signedIntent.protectedContextSlot"), signedTransactionHex, signedTransactionSha256: row.signedTransactionSha256 as string, messageSha256: row.messageSha256 as string, expectedSignature: nonEmpty(row.expectedSignature, "signedIntent.expectedSignature"), recentBlockhash: nonEmpty(row.recentBlockhash, "signedIntent.recentBlockhash"), lastValidBlockHeight: positiveInteger(row.lastValidBlockHeight, "signedIntent.lastValidBlockHeight"), feePayer: nonEmpty(row.feePayer, "signedIntent.feePayer"), compiledFeeLamports: nonNegativeInteger(row.compiledFeeLamports, "signedIntent.compiledFeeLamports"), writableAccountKeys: [...writableAccountKeys] as string[], logicalConflictKeys: [...logicalConflictKeys] as string[],
  };
}

function validatePhaseAInput(value: RestorationBridgePhaseAInput): RestorationBridgePhaseAInput {
  const row = object(value, "phaseA input");
  exactKeys(row, ["schemaVersion", "phase", "cluster", "routeId", "routeSpecSha256", "vault", "owner", "leaseSeconds", "originId", "generation", "legId", "signedIntent"], "phaseA input");
  if (row.schemaVersion !== 1 || row.phase !== "prepare" || row.cluster !== CLUSTER || row.routeId !== ROUTE_ID || row.routeSpecSha256 !== ROUTE_SPEC_SHA256 || row.vault !== VAULT) throw new Error("phaseA input is not bound to the approved four-market route");
  const leaseSeconds = positiveInteger(row.leaseSeconds, "phaseA.leaseSeconds");
  if (leaseSeconds < 60 || leaseSeconds > 900) throw new Error("phaseA.leaseSeconds is outside the Rust bridge bounds");
  const originId = sha(row.originId, "phaseA.originId");
  const legId = sha(row.legId, "phaseA.legId");
  const owner = nonEmpty(row.owner, "phaseA.owner");
  if (owner.length > 128) throw new Error("phaseA.owner must be at most 128 characters");
  return { schemaVersion: 1, phase: "prepare", cluster: CLUSTER, routeId: ROUTE_ID, routeSpecSha256: ROUTE_SPEC_SHA256, vault: VAULT, owner, leaseSeconds, originId, generation: positiveInteger(row.generation, "phaseA.generation"), legId, signedIntent: validateSignedIntent(row.signedIntent) };
}

const TOKEN_KEYS = ["schemaVersion", "eventId", "cluster", "owner", "fencingToken", "originId", "generation", "legId", "managerIntentId", "expectedSignature", "signedTransactionSha256", "messageSha256", "strategyId", "reserve", "amountRaw", "lifecycleId", "routeAuthorizationSha256", "protectedPrestateSha256", "protectedAddressSetSha256", "protectedContextSlot"] as const;

function validateToken(value: unknown, label = "token"): RestorationBridgeToken {
  const row = object(value, label);
  exactKeys(row, TOKEN_KEYS, label);
  if (row.schemaVersion !== 1 || row.cluster !== CLUSTER || !(typeof row.strategyId === "string" && Object.prototype.hasOwnProperty.call(STRATEGY_RESERVES, row.strategyId)) || row.reserve !== STRATEGY_RESERVES[row.strategyId as string]) throw new Error(`${label} is not an approved four-market token`);
  validateShaFields(row, ["originId", "legId", "managerIntentId", "signedTransactionSha256", "messageSha256", "lifecycleId", "routeAuthorizationSha256", "protectedPrestateSha256", "protectedAddressSetSha256"], label);
  return { schemaVersion: 1, eventId: positiveInteger(row.eventId, `${label}.eventId`), cluster: CLUSTER, owner: nonEmpty(row.owner, `${label}.owner`), fencingToken: positiveInteger(row.fencingToken, `${label}.fencingToken`), originId: row.originId as string, generation: positiveInteger(row.generation, `${label}.generation`), legId: row.legId as string, managerIntentId: row.managerIntentId as string, expectedSignature: nonEmpty(row.expectedSignature, `${label}.expectedSignature`), signedTransactionSha256: row.signedTransactionSha256 as string, messageSha256: row.messageSha256 as string, strategyId: row.strategyId as string, reserve: row.reserve as string, amountRaw: positiveInteger(row.amountRaw, `${label}.amountRaw`), lifecycleId: row.lifecycleId as string, routeAuthorizationSha256: row.routeAuthorizationSha256 as string, protectedPrestateSha256: row.protectedPrestateSha256 as string, protectedAddressSetSha256: row.protectedAddressSetSha256 as string, protectedContextSlot: positiveInteger(row.protectedContextSlot, `${label}.protectedContextSlot`) };
}

function canonicalToken(token: RestorationBridgeToken): RestorationBridgeToken {
  return { schemaVersion: token.schemaVersion, eventId: token.eventId, cluster: token.cluster, owner: token.owner, fencingToken: token.fencingToken, originId: token.originId, generation: token.generation, legId: token.legId, managerIntentId: token.managerIntentId, expectedSignature: token.expectedSignature, signedTransactionSha256: token.signedTransactionSha256, messageSha256: token.messageSha256, strategyId: token.strategyId, reserve: token.reserve, amountRaw: token.amountRaw, lifecycleId: token.lifecycleId, routeAuthorizationSha256: token.routeAuthorizationSha256, protectedPrestateSha256: token.protectedPrestateSha256, protectedAddressSetSha256: token.protectedAddressSetSha256, protectedContextSlot: token.protectedContextSlot };
}

function secureInput(value: JsonObject, options: RestorationBridgeRunOptions, phase: "a" | "b"): string {
  const directory = resolve(options.evidenceDirectory);
  if (directory === resolve("/") || directory.length < 2) throw new Error("restoration bridge evidence directory is unsafe");
  mkdirSync(directory, { recursive: true, mode: 0o700 });
  const path = resolve(directory, `restoration-bridge-phase-${phase}-input.json`);
  writeFileSync(path, JSON.stringify(value), { encoding: "utf8", mode: 0o600 });
  chmodSync(path, 0o600);
  return path;
}

function runBridge(value: JsonObject, options: RestorationBridgeRunOptions, phase: "a" | "b"): JsonObject & { inputPath: string; inputFileSha256: string } {
  const binary = resolve(options.binaryPath ?? DEFAULT_BINARY);
  try {
    const mode = statSync(binary).mode;
    if ((mode & 0o111) === 0) throw new Error("not executable");
  } catch {
    throw new Error("restoration bridge binary is absent or not executable");
  }
  const inputPath = secureInput(value, options, phase);
  let stdout: string;
  try {
    stdout = execFileSync(binary, ["--input", inputPath], { cwd: REPOSITORY_ROOT, encoding: "utf8", stdio: ["ignore", "pipe", "pipe"], maxBuffer: 8 * 1024 * 1024 });
  } catch {
    throw new Error("restoration bridge failed closed");
  }
  const lines = stdout.trim().split(/\r?\n/).filter((line) => line.length > 0);
  if (lines.length !== 1) throw new Error("restoration bridge output is not exactly one JSON line");
  let parsed: unknown;
  try { parsed = JSON.parse(lines[0]!); } catch { throw new Error("restoration bridge output is malformed JSON"); }
  const inputFileSha256 = createHash("sha256").update(readFileSync(inputPath)).digest("hex");
  return { ...object(parsed, "restoration bridge output"), inputPath, inputFileSha256 };
}

function validatePhaseAOutput(value: JsonObject, input: RestorationBridgePhaseAInput): RestorationBridgePhaseAResult {
  exactKeys(value, ["verdict", "broadcast", "signerLoaded", "phase", "token", "tokenSha256", "managerHandoff", "nextStep", "inputPath", "inputFileSha256"], "phaseA output");
  if (value.verdict !== PHASE_A_VERDICT || value.broadcast !== false || value.signerLoaded !== false || value.phase !== "prepare") throw new Error("phaseA bridge verdict is not a no-broadcast pass");
  const token = validateToken(value.token);
  if (token.cluster !== input.cluster || token.owner !== input.owner || token.originId !== input.originId || token.generation !== input.generation || token.legId !== input.legId || token.managerIntentId !== input.signedIntent.managerIntentId || token.expectedSignature !== input.signedIntent.expectedSignature || token.signedTransactionSha256 !== input.signedIntent.signedTransactionSha256 || token.messageSha256 !== input.signedIntent.messageSha256 || token.strategyId !== input.signedIntent.strategyId || token.reserve !== input.signedIntent.reserve || token.amountRaw !== input.signedIntent.amountRaw || token.lifecycleId !== input.signedIntent.lifecycleId || token.routeAuthorizationSha256 !== input.signedIntent.routeAuthorizationSha256 || token.protectedPrestateSha256 !== input.signedIntent.protectedPrestateSha256 || token.protectedAddressSetSha256 !== input.signedIntent.protectedAddressSetSha256 || token.protectedContextSlot !== input.signedIntent.protectedContextSlot) throw new Error("phaseA token is not bound to every caller-supplied identity");
  const tokenSha256 = sha(value.tokenSha256, "phaseA output.tokenSha256");
  if (tokenSha256 !== hashJson(canonicalToken(token))) throw new Error("phaseA token hash is inconsistent");
  const handoff = object(value.managerHandoff, "phaseA output.managerHandoff");
  exactKeys(handoff, ["operation", "strategyId", "reserve", "amountRaw", "eventId", "fencingToken", "leaseExpiresAt", "expectedSignature"], "phaseA output.managerHandoff");
  if (handoff.operation !== "manager-withdraw" || handoff.strategyId !== token.strategyId || handoff.reserve !== token.reserve || handoff.amountRaw !== token.amountRaw || handoff.eventId !== token.eventId || handoff.fencingToken !== token.fencingToken || handoff.expectedSignature !== token.expectedSignature || typeof handoff.leaseExpiresAt !== "string" || handoff.leaseExpiresAt.length === 0) throw new Error("phaseA manager handoff is not bound to the token");
  if (typeof value.nextStep !== "string" || value.nextStep.length === 0) throw new Error("phaseA nextStep is missing");
  return { verdict: PHASE_A_VERDICT, broadcast: false, signerLoaded: false, phase: "prepare", token, tokenSha256, managerHandoff: { operation: "manager-withdraw", strategyId: token.strategyId, reserve: token.reserve, amountRaw: token.amountRaw, eventId: token.eventId, fencingToken: token.fencingToken, leaseExpiresAt: handoff.leaseExpiresAt as string, expectedSignature: token.expectedSignature }, nextStep: value.nextStep as string, inputPath: nonEmpty(value.inputPath, "phaseA output.inputPath"), inputFileSha256: sha(value.inputFileSha256, "phaseA output.inputFileSha256") };
}

function validateConfirmation(value: unknown, token: RestorationBridgeToken): RestorationBridgeConfirmation {
  const row = object(value, "confirmation");
  exactKeys(row, ["managerIntentId", "lifecycleId", "strategyId", "reserve", "amountRaw", "routeAuthorizationSha256", "signedTransactionSha256", "messageSha256", "expectedSignature", "confirmedSlot", "readbackContextSlot", "commitment", "managerTransactionSignature", "idleRawAfter", "remainingShortfallRaw", "readbackFingerprint"], "confirmation");
  if (row.managerIntentId !== token.managerIntentId || row.lifecycleId !== token.lifecycleId || row.strategyId !== token.strategyId || row.reserve !== token.reserve || row.amountRaw !== token.amountRaw || row.routeAuthorizationSha256 !== token.routeAuthorizationSha256 || row.signedTransactionSha256 !== token.signedTransactionSha256 || row.messageSha256 !== token.messageSha256 || row.expectedSignature !== token.expectedSignature || row.managerTransactionSignature !== token.expectedSignature || row.commitment !== "confirmed") throw new Error("confirmation is not bound to the Phase-A token");
  validateShaFields(row, ["managerIntentId", "lifecycleId", "routeAuthorizationSha256", "signedTransactionSha256", "messageSha256", "readbackFingerprint"], "confirmation");
  const confirmedSlot = positiveInteger(row.confirmedSlot, "confirmation.confirmedSlot");
  const readbackContextSlot = positiveInteger(row.readbackContextSlot, "confirmation.readbackContextSlot");
  if (readbackContextSlot < confirmedSlot) throw new Error("confirmation.readbackContextSlot predates confirmedSlot");
  return { managerIntentId: token.managerIntentId, lifecycleId: token.lifecycleId, strategyId: token.strategyId, reserve: token.reserve, amountRaw: token.amountRaw, routeAuthorizationSha256: token.routeAuthorizationSha256, signedTransactionSha256: token.signedTransactionSha256, messageSha256: token.messageSha256, expectedSignature: token.expectedSignature, confirmedSlot, readbackContextSlot, commitment: "confirmed", managerTransactionSignature: token.expectedSignature, idleRawAfter: nonNegativeInteger(row.idleRawAfter, "confirmation.idleRawAfter"), remainingShortfallRaw: nonNegativeInteger(row.remainingShortfallRaw, "confirmation.remainingShortfallRaw"), readbackFingerprint: row.readbackFingerprint as string };
}

function validatePhaseBOutput(value: JsonObject, input: RestorationBridgePhaseBInput): RestorationBridgePhaseBResult {
  exactKeys(value, ["verdict", "broadcast", "signerLoaded", "phase", "completion", "tokenSha256", "inputPath", "inputFileSha256"], "phaseB output");
  if (value.verdict !== PHASE_B_VERDICT || value.broadcast !== false || value.signerLoaded !== false || value.phase !== "confirm") throw new Error("phaseB bridge verdict is not a no-broadcast pass");
  const tokenSha256 = sha(value.tokenSha256, "phaseB output.tokenSha256");
  if (tokenSha256 !== hashJson(canonicalToken(input.token))) throw new Error("phaseB token hash is inconsistent");
  const completion = object(value.completion, "phaseB output.completion");
  exactKeys(completion, ["eventId", "originId", "generation", "legId", "state", "acknowledged", "canceledSiblingCount"], "phaseB output.completion");
  if (completion.eventId !== input.token.eventId || completion.originId !== input.token.originId || completion.generation !== input.token.generation || completion.legId !== input.token.legId || completion.state !== "acknowledged" || completion.acknowledged !== true || !Number.isSafeInteger(completion.canceledSiblingCount) || (completion.canceledSiblingCount as number) < 0) throw new Error("phaseB completion is not bound to the fenced token");
  return { verdict: PHASE_B_VERDICT, broadcast: false, signerLoaded: false, phase: "confirm", completion: { eventId: input.token.eventId, originId: input.token.originId, generation: input.token.generation, legId: input.token.legId, state: "acknowledged", acknowledged: true, canceledSiblingCount: completion.canceledSiblingCount as number }, tokenSha256, inputPath: nonEmpty(value.inputPath, "phaseB output.inputPath"), inputFileSha256: sha(value.inputFileSha256, "phaseB output.inputFileSha256") };
}

export function prepareRestorationBridge(input: RestorationBridgePhaseAInput, options: RestorationBridgeRunOptions): RestorationBridgePhaseAResult {
  const normalized = validatePhaseAInput(input);
  const result = runBridge(normalized as unknown as JsonObject, options, "a");
  return validatePhaseAOutput(result, normalized);
}

export function confirmRestorationBridge(token: RestorationBridgeToken, confirmation: RestorationBridgeConfirmation, options: RestorationBridgeRunOptions): RestorationBridgePhaseBResult {
  const normalizedToken = validateToken(token);
  const normalizedConfirmation = validateConfirmation(confirmation, normalizedToken);
  const result = runBridge({ schemaVersion: 1, phase: "confirm", token: normalizedToken, confirmation: normalizedConfirmation }, options, "b");
  return validatePhaseBOutput(result, { schemaVersion: 1, phase: "confirm", token: normalizedToken, confirmation: normalizedConfirmation });
}

export const runRestorationBridgePhaseA = prepareRestorationBridge;
export const runRestorationBridgePhaseB = confirmRestorationBridge;
