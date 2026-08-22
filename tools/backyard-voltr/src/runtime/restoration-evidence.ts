import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import { dirname, relative, resolve } from "node:path";

import type { PartnerStrategyId } from "../domain/route-spec.js";
import {
  PARTNER_FOUR_MARKET_ROUTE,
  PARTNER_ROUTE,
  fourMarketRouteSpecSha256,
} from "../domain/route-spec.js";
import {
  planWithdrawalRestoration,
  parseWithdrawalRestorationPlanFile,
  parseWithdrawalRestorationScanFile,
  type WithdrawalRestorationPlan,
  type WithdrawalRestorationScan,
  type WithdrawalRestorationSource,
} from "./withdrawal-restoration.js";

type Sha256 = string;

/** The verifier's canonical transaction reference, without any RPC-derived fields. */
export type RestorationTransactionRef = Readonly<{
  path: string;
  fileSha256: Sha256;
  signature: string;
  intentSha256: Sha256;
  messageSha256: Sha256;
  slot: number;
  protectedAddressSetSha256: Sha256;
  protectedPrestateSha256: Sha256;
  protectedPoststateSha256: Sha256;
  protectedBeforeContextSlot: number;
  protectedAfterContextSlot: number;
  protectedPreAttestationSha256: Sha256;
  protectedSettlementAttestationSha256: Sha256;
}>;

/** Normalized fields extracted from the confirmed manager command output. */
export type RestorationManagerConfirmation = Readonly<{
  broadcast: true;
  commitment: "confirmed";
  strategyId: "main";
  reserve: string;
  amountRaw: bigint;
  originId: Sha256;
  legId: Sha256;
  managerIntentId: Sha256;
  lifecycleId: Sha256;
  routeAuthorizationSha256: Sha256;
  signature: string;
  confirmedSlot: number;
  readbackContextSlot: number;
  idleRawAfter: bigint;
  remainingShortfallRaw: bigint;
  protectedAddressSetSha256: Sha256;
  protectedPrestateSha256: Sha256;
  protectedPoststateSha256: Sha256;
}>;

export type RestorationDurableOutboxRow = Readonly<{
  eventId: number;
  legId: string;
  dedupeKey: string;
  state: string;
  leaseFence: number;
  managerIntentId: string;
  expectedSignature: string;
  confirmedSignature: string;
  confirmedSlot: number;
  readbackContextSlot: number;
  oneSendOnly: true;
}>;

export type RestorationDurableOutbox = Readonly<{
  eventKind: "backyard_voltr_manager_withdraw";
  aggregateKind: "voltr_withdrawal_restoration";
  originId: string;
  generation: number;
  insertedLegCount: number;
  duplicateLegCount: number;
  rows: readonly RestorationDurableOutboxRow[];
  ackCondition: "confirmed_manager_readback_and_recomputed_idle_shortfall";
}>;

export type RestorationDurableReadback = Readonly<{
  verdict: "BACKYARD_VOLTR_RESTORATION_DURABLE_READBACK_PASS";
  broadcast: false;
  signerLoaded: false;
  source: "loyal_yield.orchestration_outbox";
  durableOutbox: RestorationDurableOutbox;
}>;

export type RestorationEvidenceInput = Readonly<{
  scan: WithdrawalRestorationScan;
  plan: WithdrawalRestorationPlan;
  manager: RestorationManagerConfirmation;
  durableReadback: RestorationDurableReadback | RestorationDurableOutbox;
  transaction: RestorationTransactionRef;
  /** Optional full source set; omitted sources are reconstructed from plan legs. */
  sources?: readonly WithdrawalRestorationSource[];
}>;

export type RestorationEvidenceArtifact = Readonly<{
  schemaVersion: 1;
  evidenceType: "backyard-voltr-withdrawal-restoration-confirmed";
  broadcast: true;
  routeId: string;
  routeSpecSha256: string;
  scanGenerationFingerprint: string;
  requestOrigin: WithdrawalRestorationScan["requestOrigin"];
  rawQuerySha256: string;
  queryConfigSha256: string;
  generation: number;
  sources: readonly Readonly<{
    strategyId: PartnerStrategyId;
    reserve: string;
    availableRaw: string;
    netYieldLossBps: string;
    unwindCostLamports: string;
    observedContextSlot: number;
    positionFingerprint: string;
  }>[];
  plan: Record<string, unknown>;
  durableOutbox: RestorationDurableOutbox;
  confirmedLegs: readonly Readonly<{
    legId: string;
    strategyId: PartnerStrategyId;
    reserve: string;
    amountRaw: string;
    originId: string;
    transaction: RestorationTransactionRef;
    readbackContextSlot: number;
    idleRawAfter: string;
    remainingShortfallRaw: string;
  }>[];
  shortfallRecomputations: readonly Readonly<{
    afterLegId: string | null;
    contextSlot: number;
    confirmedIdleRaw: string;
    remainingShortfallRaw: string;
  }>[];
}>;

function fail(message: string): never {
  throw new Error(`restoration evidence: ${message}`);
}

function record(value: unknown, label: string): Readonly<Record<string, unknown>> {
  if (value === null || typeof value !== "object" || Array.isArray(value)) fail(`${label} must be an object`);
  return value as Readonly<Record<string, unknown>>;
}

function text(value: unknown, label: string): string {
  if (typeof value !== "string" || value.length === 0) fail(`${label} must be a non-empty string`);
  return value;
}

function bigintField(value: unknown, label: string): bigint {
  if (typeof value === "bigint") return value;
  if (typeof value === "number" && Number.isSafeInteger(value)) return BigInt(value);
  if (typeof value === "string" && /^(0|[1-9][0-9]*)$/.test(value)) return BigInt(value);
  fail(`${label} must be a non-negative integer`);
}

function sha(value: string, label: string): string {
  if (!/^[0-9a-f]{64}$/.test(value)) fail(`${label} must be a lowercase SHA-256`);
  return value;
}

function positiveSlot(value: number, label: string): number {
  if (!Number.isSafeInteger(value) || value <= 0) fail(`${label} must be a positive safe integer`);
  return value;
}

function canonicalJson(value: unknown): string {
  if (typeof value === "bigint") return JSON.stringify(value.toString());
  if (Array.isArray(value)) return `[${value.map(canonicalJson).join(",")}]`;
  if (value && typeof value === "object") {
    return `{${Object.entries(value as Record<string, unknown>)
      .sort(([left], [right]) => left.localeCompare(right))
      .map(([key, entry]) => `${JSON.stringify(key)}:${canonicalJson(entry)}`)
      .join(",")}}`;
  }
  return JSON.stringify(value);
}

function jsonPlan(plan: WithdrawalRestorationPlan): Record<string, unknown> {
  return JSON.parse(canonicalJson(plan)) as Record<string, unknown>;
}

function sameOrigin(left: WithdrawalRestorationScan["requestOrigin"], right: WithdrawalRestorationScan["requestOrigin"]): boolean {
  return left.signature === right.signature
    && left.eventIndex === right.eventIndex
    && left.receipt === right.receipt
    && left.rawAccountSha256 === right.rawAccountSha256
    && left.generationFingerprint === right.generationFingerprint;
}

/**
 * Extract the exact restoration fields from the successful manager command.
 * The CLI's JSON replacer serializes bigint values as decimal strings, while
 * in-process callers may still hold bigint values; both forms are accepted
 * and normalized here. No durable acknowledgement is inferred from missing
 * Phase B or a non-zero manager readback gate count.
 */
export function extractRestorationManagerConfirmation(value: unknown): RestorationManagerConfirmation {
  const root = record(value, "manager output");
  if (root.verdict !== "PARTNER_MANAGER_OPERATION_FINALIZED_AND_VERIFIED" || root.broadcast !== true) fail("manager output is not the exact confirmed restoration pass");
  const finalized = record(root.finalized, "manager output.finalized");
  if (finalized.err !== null) fail("manager transaction has a chain error");
  const signature = text(finalized.signature, "manager output.finalized.signature");
  const confirmedSlot = positiveSlot(finalized.confirmedSlot as number, "manager output.finalized.confirmedSlot");
  const readback = record(root.readback, "manager output.readback");
  if (readback.failedGateCount !== 0) fail("manager readback contains failed gates");
  const idleRawAfter = bigintField(readback.idleAfter, "manager output.readback.idleAfter");
  const protectedState = record(root.protectedState, "manager output.protectedState");
  const restoration = record(root.restorationBridge, "manager output.restorationBridge");
  const phaseA = record(restoration.phaseA, "manager output.restorationBridge.phaseA");
  const token = record(phaseA.token, "manager output.restorationBridge.phaseA.token");
  const phaseB = record(restoration.phaseB, "manager output.restorationBridge.phaseB");
  const completion = record(phaseB.completion, "manager output.restorationBridge.phaseB.completion");
  if (phaseB.verdict !== "BACKYARD_VOLTR_RESTORATION_BRIDGE_PHASE_B_PASS" || phaseB.broadcast !== false || phaseB.signerLoaded !== false || phaseB.phase !== "confirm" || completion.acknowledged !== true) fail("manager output does not contain an acknowledged Phase-B restoration fence");
  const intent = record(root.intent, "manager output.intent");
  const strategyId = text(token.strategyId, "manager output.restorationBridge.phaseA.token.strategyId");
  const reserve = text(token.reserve, "manager output.restorationBridge.phaseA.token.reserve");
  if (strategyId !== "main" || reserve !== PARTNER_ROUTE.strategy.reserve) fail("manager restoration output is not the exact Main reserve leg");
  const readbackContextSlot = positiveSlot(root.readbackContextSlot as number, "manager output.readbackContextSlot");
  const amountRaw = bigintField(token.amountRaw, "manager output.restorationBridge.phaseA.token.amountRaw");
  return {
    broadcast: true,
    commitment: "confirmed",
    strategyId: "main",
    reserve,
    amountRaw,
    originId: sha(text(token.originId, "manager output.restorationBridge.phaseA.token.originId"), "manager output.originId"),
    legId: sha(text(token.legId, "manager output.restorationBridge.phaseA.token.legId"), "manager output.legId"),
    managerIntentId: sha(text(token.managerIntentId, "manager output.restorationBridge.phaseA.token.managerIntentId"), "manager output.managerIntentId"),
    lifecycleId: sha(text(intent.lifecycleId, "manager output.intent.lifecycleId"), "manager output.lifecycleId"),
    routeAuthorizationSha256: sha(text(intent.routeAuthorizationSha256, "manager output.intent.routeAuthorizationSha256"), "manager output.routeAuthorizationSha256"),
    signature,
    confirmedSlot,
    readbackContextSlot,
    idleRawAfter,
    remainingShortfallRaw: bigintField(restoration.remainingShortfallRaw, "manager output.restorationBridge.remainingShortfallRaw"),
    protectedAddressSetSha256: sha(text(protectedState.addressSetSha256, "manager output.protectedState.addressSetSha256"), "manager output.protectedState.addressSetSha256"),
    protectedPrestateSha256: sha(text(protectedState.beforeSha256, "manager output.protectedState.beforeSha256"), "manager output.protectedState.beforeSha256"),
    protectedPoststateSha256: sha(text(protectedState.afterSha256, "manager output.protectedState.afterSha256"), "manager output.protectedState.afterSha256"),
  };
}

function validateTransaction(transaction: RestorationTransactionRef): void {
  if (!transaction.path.trim()) fail("transaction.path is required");
  sha(transaction.fileSha256, "transaction.fileSha256");
  if (transaction.signature.trim().length < 80 || transaction.signature.trim().length > 90) fail("transaction.signature is malformed");
  sha(transaction.intentSha256, "transaction.intentSha256");
  sha(transaction.messageSha256, "transaction.messageSha256");
  positiveSlot(transaction.slot, "transaction.slot");
  sha(transaction.protectedAddressSetSha256, "transaction.protectedAddressSetSha256");
  sha(transaction.protectedPrestateSha256, "transaction.protectedPrestateSha256");
  sha(transaction.protectedPoststateSha256, "transaction.protectedPoststateSha256");
  sha(transaction.protectedPreAttestationSha256, "transaction.protectedPreAttestationSha256");
  sha(transaction.protectedSettlementAttestationSha256, "transaction.protectedSettlementAttestationSha256");
  positiveSlot(transaction.protectedBeforeContextSlot, "transaction.protectedBeforeContextSlot");
  positiveSlot(transaction.protectedAfterContextSlot, "transaction.protectedAfterContextSlot");
  if (transaction.protectedBeforeContextSlot > transaction.slot || transaction.protectedAfterContextSlot < transaction.slot || transaction.protectedAfterContextSlot < transaction.protectedBeforeContextSlot) {
    fail("transaction protected contexts are not ordered around the confirmed slot");
  }
}

function validatePlanAndSources(input: RestorationEvidenceInput): readonly WithdrawalRestorationSource[] {
  const { scan, plan, manager } = input;
  if (scan.verdict !== "PARTNER_WITHDRAWAL_DEMAND_SCAN_PASS" || scan.routeId !== PARTNER_FOUR_MARKET_ROUTE.id || scan.routeSpecSha256 !== fourMarketRouteSpecSha256() || scan.vault !== PARTNER_ROUTE.vault) fail("scan is not the exact confirmed four-market pass");
  if (plan.routeId !== scan.routeId || plan.routeSpecSha256 !== scan.routeSpecSha256 || plan.vault !== scan.vault || plan.generation <= 0) fail("plan route or generation does not equal scan");
  if (plan.requestedRaw <= 0n || plan.plannedRaw !== plan.requestedRaw) fail("plan amount is not a positive fully covered shortfall");
  if (!plan.durability) fail("plan has no durable lifecycle binding");
  if (!sameOrigin(plan.durability.requestOrigin, scan.requestOrigin)) fail("plan request origin differs from scan");
  if (plan.durability.protectedCheckpoint.contextSlot > scan.observationContextSlot) fail("plan protected checkpoint is newer than scan");
  sha(plan.durability.lifecycleId, "plan durability.lifecycleId");
  sha(plan.durability.routeAuthorizationSha256, "plan durability.routeAuthorizationSha256");
  if (plan.legs.length !== 1 || plan.legs[0]?.strategyId !== "main" || plan.legs[0]?.reserve !== PARTNER_ROUTE.strategy.reserve) fail("restoration must contain exactly one Main leg");
  if (manager.strategyId !== "main" || manager.reserve !== plan.legs[0]!.reserve || manager.amountRaw !== plan.legs[0]!.amountRaw || manager.originId !== plan.originId || manager.legId !== plan.legs[0]!.legId) fail("manager confirmation does not equal the named Main plan leg");
  if (manager.lifecycleId !== plan.durability.lifecycleId || manager.routeAuthorizationSha256 !== plan.durability.routeAuthorizationSha256) fail("manager confirmation lifecycle or route authorization differs from plan");
  const sources = input.sources ?? plan.legs.map((leg) => ({
    strategyId: leg.strategyId,
    reserve: leg.reserve,
    availableRaw: leg.sourceAvailableRaw,
    netYieldLossBps: leg.netYieldLossBps,
    unwindCostLamports: leg.unwindCostLamports,
    observedContextSlot: leg.sourceObservedContextSlot,
    positionFingerprint: leg.positionFingerprint,
  }));
  const rebuilt = planWithdrawalRestoration(scan, sources, plan.generation, plan.durability);
  if (canonicalJson(rebuilt) !== canonicalJson(plan)) fail("supplied plan differs from deterministic planner output");
  return sources;
}

function validateManager(manager: RestorationManagerConfirmation, plan: WithdrawalRestorationPlan, transaction: RestorationTransactionRef): void {
  if (manager.broadcast !== true || manager.commitment !== "confirmed") fail("manager confirmation is not a successful confirmed broadcast");
  if (manager.strategyId !== "main" || manager.amountRaw !== plan.legs[0]!.amountRaw || manager.confirmedSlot !== transaction.slot) fail("manager confirmation amount/strategy/slot differs from canonical transaction");
  if (manager.amountRaw <= 0n || manager.signature.trim().length < 80 || manager.signature.trim().length > 90) fail("manager confirmation amount or signature is malformed");
  sha(manager.originId, "manager.originId");
  sha(manager.legId, "manager.legId");
  sha(manager.managerIntentId, "manager.managerIntentId");
  sha(manager.protectedAddressSetSha256, "manager.protectedAddressSetSha256");
  sha(manager.protectedPrestateSha256, "manager.protectedPrestateSha256");
  sha(manager.protectedPoststateSha256, "manager.protectedPoststateSha256");
  if (manager.idleRawAfter < 0n || manager.remainingShortfallRaw < 0n || manager.readbackContextSlot < manager.confirmedSlot || manager.confirmedSlot <= plan.durability!.protectedCheckpoint.contextSlot) fail("manager confirmed/readback contexts or balances are not ordered after request checkpoint");
  if (manager.remainingShortfallRaw !== 0n) fail("manager confirmation does not close the restoration shortfall");
  if (manager.protectedAddressSetSha256 !== transaction.protectedAddressSetSha256 || manager.protectedPrestateSha256 !== transaction.protectedPrestateSha256 || manager.protectedPoststateSha256 !== transaction.protectedPoststateSha256) fail("manager protected hashes differ from canonical transaction facts");
}

function validateDurable(readback: RestorationDurableReadback | RestorationDurableOutbox, plan: WithdrawalRestorationPlan, manager: RestorationManagerConfirmation): RestorationDurableOutbox {
  if ("durableOutbox" in readback && (readback.verdict !== "BACKYARD_VOLTR_RESTORATION_DURABLE_READBACK_PASS" || readback.broadcast !== false || readback.signerLoaded !== false)) fail("durable readback wrapper is not the signer-free readback pass");
  const outbox = "durableOutbox" in readback
    ? readback.durableOutbox
    : readback;
  if (!outbox) fail("durable readback has no durableOutbox");
  const exactOutbox = outbox as RestorationDurableOutbox;
  if (("broadcast" in readback && readback.broadcast !== false) || ("signerLoaded" in readback && readback.signerLoaded !== false)) fail("durable readback crossed the signer/broadcast boundary");
  if (exactOutbox.eventKind !== "backyard_voltr_manager_withdraw" || exactOutbox.aggregateKind !== "voltr_withdrawal_restoration" || exactOutbox.originId !== plan.originId || exactOutbox.generation !== plan.generation || exactOutbox.insertedLegCount !== 1 || exactOutbox.duplicateLegCount !== 0 || exactOutbox.ackCondition !== "confirmed_manager_readback_and_recomputed_idle_shortfall" || exactOutbox.rows.length !== 1) fail("durable outbox contract is not exact");
  const row = exactOutbox.rows[0]!;
  const leg = plan.legs[0]!;
  if (!Number.isSafeInteger(row.eventId) || row.eventId <= 0 || !Number.isSafeInteger(row.confirmedSlot) || row.confirmedSlot <= 0 || !Number.isSafeInteger(row.readbackContextSlot) || row.readbackContextSlot < row.confirmedSlot || row.legId !== leg.legId || row.dedupeKey !== `backyard-voltr:${plan.originId}:${plan.generation}:${leg.legId}` || row.state !== "acknowledged" || row.leaseFence <= 0 || row.managerIntentId !== manager.managerIntentId || row.expectedSignature !== manager.signature || row.confirmedSignature !== manager.signature || row.confirmedSlot !== manager.confirmedSlot || row.readbackContextSlot !== manager.readbackContextSlot || row.oneSendOnly !== true) fail("durable row is not bound to the exact confirmed manager leg");
  return exactOutbox;
}

/** Assemble the exact restoration artifact consumed by `verify four-market`. */
export function assembleRestorationEvidence(input: RestorationEvidenceInput): RestorationEvidenceArtifact {
  const { scan, plan, manager, transaction } = input;
  const sources = validatePlanAndSources(input);
  validateTransaction(transaction);
  validateManager(manager, plan, transaction);
  if (transaction.protectedAddressSetSha256 !== plan.durability!.protectedCheckpoint.addressSetSha256 || transaction.protectedPrestateSha256 !== plan.durability!.protectedCheckpoint.stateSha256 || manager.protectedAddressSetSha256 !== plan.durability!.protectedCheckpoint.addressSetSha256) fail("request checkpoint and restoration prestate hashes do not form the protected chain");
  const durableOutbox = validateDurable(input.durableReadback, plan, manager);
  const leg = plan.legs[0]!;
  const sourceRows = sources.map((source) => ({ ...source, availableRaw: source.availableRaw.toString(), netYieldLossBps: source.netYieldLossBps.toString(), unwindCostLamports: source.unwindCostLamports.toString() }));
  return {
    schemaVersion: 1,
    evidenceType: "backyard-voltr-withdrawal-restoration-confirmed",
    broadcast: true,
    routeId: scan.routeId,
    routeSpecSha256: scan.routeSpecSha256,
    scanGenerationFingerprint: scan.generationFingerprint,
    requestOrigin: scan.requestOrigin,
    rawQuerySha256: scan.rawQuerySha256,
    queryConfigSha256: scan.queryConfigSha256,
    generation: plan.generation,
    sources: sourceRows,
    plan: jsonPlan(plan),
    durableOutbox,
    confirmedLegs: [{
      legId: leg.legId,
      strategyId: leg.strategyId,
      reserve: leg.reserve,
      amountRaw: leg.amountRaw.toString(),
      originId: plan.originId,
      transaction,
      readbackContextSlot: manager.readbackContextSlot,
      idleRawAfter: manager.idleRawAfter.toString(),
      remainingShortfallRaw: manager.remainingShortfallRaw.toString(),
    }],
    shortfallRecomputations: [
      { afterLegId: null, contextSlot: scan.observationContextSlot, confirmedIdleRaw: scan.demand.confirmedIdleRaw.toString(), remainingShortfallRaw: scan.demand.idleShortfallRaw.toString() },
      { afterLegId: leg.legId, contextSlot: manager.readbackContextSlot, confirmedIdleRaw: manager.idleRawAfter.toString(), remainingShortfallRaw: manager.remainingShortfallRaw.toString() },
    ],
  };
}

export const buildRestorationEvidence = assembleRestorationEvidence;

/**
 * File adapter used by the CLI. Every transaction-reference field is derived
 * from the maintained manager command output and its actual file bytes; the
 * operator supplies paths only, never signatures, hashes, slots, or balances.
 */
export function assembleRestorationEvidenceFromFiles(input: Readonly<{
  scanPath: string;
  planPath: string;
  managerPath: string;
  durableReadbackPath: string;
  manifestPath: string;
}>): RestorationEvidenceArtifact {
  const scan = parseWithdrawalRestorationScanFile(resolve(input.scanPath));
  const plan = parseWithdrawalRestorationPlanFile(resolve(input.planPath));
  const managerAbsolute = resolve(input.managerPath);
  const managerBytes = readFileSync(managerAbsolute);
  const managerOutput = record(JSON.parse(managerBytes.toString("utf8")), "manager output");
  const manager = extractRestorationManagerConfirmation(managerOutput);
  const durableReadback = JSON.parse(readFileSync(resolve(input.durableReadbackPath), "utf8")) as RestorationDurableReadback;
  const manifestRoot = resolve(dirname(resolve(input.manifestPath)));
  const transactionPath = relative(manifestRoot, managerAbsolute);
  if (transactionPath.length === 0 || transactionPath === ".." || transactionPath.startsWith("../") || transactionPath.startsWith("/")) fail("manager transaction path escapes the lifecycle manifest directory");
  const intent = record(managerOutput.intent, "manager output.intent");
  const senderProof = record(managerOutput.senderProof, "manager output.senderProof");
  const protectedState = record(managerOutput.protectedState, "manager output.protectedState");
  const preSendAttestation = record(managerOutput.preSendAttestation, "manager output.preSendAttestation");
  const settlementAttestation = record(managerOutput.settlementAttestation, "manager output.settlementAttestation");
  const transaction: RestorationTransactionRef = {
    path: transactionPath,
    fileSha256: createHash("sha256").update(managerBytes).digest("hex"),
    signature: manager.signature,
    intentSha256: sha(text(managerOutput.intentSha256, "manager output.intentSha256"), "manager output.intentSha256"),
    messageSha256: sha(text(senderProof.serializedMessageSha256, "manager output.senderProof.serializedMessageSha256"), "manager output.senderProof.serializedMessageSha256"),
    slot: manager.confirmedSlot,
    protectedAddressSetSha256: manager.protectedAddressSetSha256,
    protectedPrestateSha256: manager.protectedPrestateSha256,
    protectedPoststateSha256: manager.protectedPoststateSha256,
    protectedBeforeContextSlot: positiveSlot(protectedState.beforeContextSlot as number, "manager output.protectedState.beforeContextSlot"),
    protectedAfterContextSlot: positiveSlot(protectedState.afterContextSlot as number, "manager output.protectedState.afterContextSlot"),
    protectedPreAttestationSha256: sha(text(preSendAttestation.attestationSha256, "manager output.preSendAttestation.attestationSha256"), "manager output.preSendAttestation.attestationSha256"),
    protectedSettlementAttestationSha256: sha(text(settlementAttestation.attestationSha256, "manager output.settlementAttestation.attestationSha256"), "manager output.settlementAttestation.attestationSha256"),
  };
  if (intent.lifecycleId !== manager.lifecycleId || intent.routeAuthorizationSha256 !== manager.routeAuthorizationSha256) fail("manager intent differs from the extracted restoration lifecycle");
  return assembleRestorationEvidence({ scan, plan, manager, durableReadback, transaction });
}
