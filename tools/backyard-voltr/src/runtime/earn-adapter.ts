import { createHash } from "node:crypto";
import { execFileSync } from "node:child_process";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { fileURLToPath } from "node:url";

import {
  PARTNER_FOUR_MARKET_ROUTE,
  PARTNER_ROUTE,
  fourMarketRouteSpecSha256,
  partnerStrategyIdentity,
  type PartnerStrategyId,
} from "../domain/route-spec.js";

/**
 * The adapter is deliberately not an economic planner. The Go engine's fleet
 * Voltr family owns observation, planning, and durable opportunity
 * publication; this module replays saved planner inputs through that exact Go
 * planner (`loyal-evidence --kind voltr` -> `fleet.PlanVoltr`) and validates
 * the result. Keeping this boundary projection-only prevents a second
 * TypeScript planner from silently diverging from the engine.
 */
export const EARN_ADAPTER_REPLAY_KIND = "loyal-earn-go-voltr-planner-replay-v2" as const;
export const EARN_ADAPTER_REPLAY_IMPLEMENTATION = "go/workers/cmd/loyal-evidence --kind voltr -> fleet.PlanVoltr" as const;

const EARN_ADAPTER_SOURCE_PATHS = [
  "go/workers/cmd/loyal-evidence/main.go",
  "go/workers/internal/fleet/types.go",
  "go/workers/internal/fleet/voltr.go",
  "go/workers/internal/fleet/voltr_plan.go",
  "go/workers/internal/fleet/voltr_route.json",
  "tools/backyard-voltr/src/domain/route-spec.ts",
  "tools/backyard-voltr/src/runtime/earn-adapter.ts",
] as const;

type JsonObject = Readonly<Record<string, unknown>>;
const REPOSITORY_ROOT = resolve(fileURLToPath(new URL("../../../..", import.meta.url)));
const GO_MODULE_ROOT = resolve(REPOSITORY_ROOT, "go/workers");
const VOLTR_IDLE_TARGET = `voltr_idle:${PARTNER_ROUTE.vault}`;

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

function stringField(value: JsonObject, key: string, label: string): string {
  if (typeof value[key] !== "string" || value[key] === "") throw new Error(`${label}.${key} must be a non-empty string`);
  return value[key] as string;
}

function positiveInteger(value: unknown, label: string): number {
  if (typeof value !== "number" || !Number.isSafeInteger(value) || value <= 0) throw new Error(`${label} must be a positive safe integer`);
  return value;
}

function nonNegativeInteger(value: unknown, label: string): number {
  if (typeof value !== "number" || !Number.isSafeInteger(value) || value < 0) throw new Error(`${label} must be a non-negative safe integer`);
  return value;
}

function canonicalJson(value: unknown): string {
  if (Array.isArray(value)) return `[${value.map(canonicalJson).join(",")}]`;
  if (value && typeof value === "object") {
    return `{${Object.entries(value as JsonObject).sort(([left], [right]) => left.localeCompare(right)).map(([key, entry]) => `${JSON.stringify(key)}:${canonicalJson(entry)}`).join(",")}}`;
  }
  return JSON.stringify(value);
}

function sha256Canonical(value: unknown): string {
  return createHash("sha256").update(canonicalJson(value)).digest("hex");
}

/** One saved `loyal-evidence --kind voltr` input, passed to Go verbatim. */
export type GoVoltrReplayCall = Readonly<{
  Observation: JsonObject;
  Epoch: JsonObject;
  OptimizerEpochRowID: number;
  VaultID: number;
  LastOptimization: string | null;
  EvaluatedAt: string;
}>;

/**
 * The confirmed Earn movement is two planner decisions on two confirmed
 * observations: a zero-demand yield leg out of the source strategy, then an
 * idle allocation into the destination. The priority call replays the same
 * vault with the scanner's positive withdrawal demand.
 */
export type EarnReplayInput = Readonly<{
  schemaVersion: 2;
  routeId: string;
  movementId: string;
  sourceStrategyId: PartnerStrategyId;
  destinationStrategyId: PartnerStrategyId;
  amountRaw: number;
  source: GoVoltrReplayCall;
  destination: GoVoltrReplayCall;
  priority: GoVoltrReplayCall;
}>;

export type EarnReplayLeg = Readonly<{
  class: string;
  operation: string;
  strategyId: string;
  sourceReserve: string | null;
  targetReserve: string;
  amountRaw: string;
  sourceApyBps: number;
  targetApyBps: number;
  protectedContextSlot: number;
  intentSha256: string;
  planSha256: string;
  opportunityKey: string;
}>;

export type EarnSharedReplay = Readonly<{
  kind: typeof EARN_ADAPTER_REPLAY_KIND;
  implementation: typeof EARN_ADAPTER_REPLAY_IMPLEMENTATION;
  input: EarnReplayInput;
  inputSha256: string;
  sourceLeg: EarnReplayLeg;
  destinationLeg: EarnReplayLeg;
  priorityProbe: Readonly<{
    withdrawalDemandRaw: string;
    decision: string;
    normalOptimization: "blocked";
  }>;
  outputSha256: string;
  sourceBindings: readonly Readonly<{ path: string; sha256: string }>[];
}>;

function goCall(value: unknown, label: string): GoVoltrReplayCall {
  const call = object(value, label);
  exactKeys(call, ["Observation", "Epoch", "OptimizerEpochRowID", "VaultID", "LastOptimization", "EvaluatedAt"], label);
  const observation = object(call.Observation, `${label}.Observation`);
  exactKeys(observation, ["ContextSlot", "TotalValueRaw", "IdleRaw", "PositionsRaw", "PendingRaw", "EarliestRedeem", "Receipts", "State", "Addresses"], `${label}.Observation`);
  positiveInteger(observation.ContextSlot, `${label}.Observation.ContextSlot`);
  for (const key of ["TotalValueRaw", "IdleRaw", "PendingRaw", "EarliestRedeem"] as const) nonNegativeInteger(observation[key], `${label}.Observation.${key}`);
  if (!Array.isArray(observation.PositionsRaw) || observation.PositionsRaw.length !== 4) throw new Error(`${label}.Observation.PositionsRaw must hold the four strategy positions`);
  observation.PositionsRaw.forEach((raw, index) => nonNegativeInteger(raw, `${label}.Observation.PositionsRaw[${index}]`));
  object(call.Epoch, `${label}.Epoch`);
  positiveInteger(call.OptimizerEpochRowID, `${label}.OptimizerEpochRowID`);
  positiveInteger(call.VaultID, `${label}.VaultID`);
  if (call.LastOptimization !== null && (typeof call.LastOptimization !== "string" || Number.isNaN(Date.parse(call.LastOptimization)))) throw new Error(`${label}.LastOptimization must be null or an RFC 3339 time`);
  if (typeof call.EvaluatedAt !== "string" || Number.isNaN(Date.parse(call.EvaluatedAt))) throw new Error(`${label}.EvaluatedAt must be an RFC 3339 time`);
  return call as GoVoltrReplayCall;
}

function replayInput(value: unknown, label: string): EarnReplayInput {
  const root = object(value, label);
  exactKeys(root, ["schemaVersion", "routeId", "movementId", "sourceStrategyId", "destinationStrategyId", "amountRaw", "source", "destination", "priority"], label);
  if (root.schemaVersion !== 2 || root.routeId !== PARTNER_FOUR_MARKET_ROUTE.id) throw new Error(`${label} is not the schema-2 four-market replay input`);
  sha(root.movementId, `${label}.movementId`);
  const sourceStrategyId = stringField(root, "sourceStrategyId", label) as PartnerStrategyId;
  const destinationStrategyId = stringField(root, "destinationStrategyId", label) as PartnerStrategyId;
  partnerStrategyIdentity(sourceStrategyId);
  partnerStrategyIdentity(destinationStrategyId);
  if (sourceStrategyId === destinationStrategyId) throw new Error(`${label} source and destination strategies must differ`);
  positiveInteger(root.amountRaw, `${label}.amountRaw`);
  goCall(root.source, `${label}.source`);
  goCall(root.destination, `${label}.destination`);
  goCall(root.priority, `${label}.priority`);
  return root as EarnReplayInput;
}

/** Run one saved planner call through the Go engine. No DB, RPC, or signer. */
function runGoVoltrReplay(call: GoVoltrReplayCall): Readonly<{ opportunity: JsonObject | null; opportunityKey: string | null }> {
  let stdout: string;
  try {
    stdout = execFileSync("go", ["run", "./cmd/loyal-evidence", "-kind", "voltr", "-snapshot", "/dev/stdin"], { cwd: GO_MODULE_ROOT, input: `${JSON.stringify(call)}\n`, encoding: "utf8", maxBuffer: 8 * 1024 * 1024 });
  } catch (error) {
    throw new Error(`Go Voltr planner replay failed: ${error instanceof Error ? error.message : String(error)}`);
  }
  const output = object(JSON.parse(stdout) as unknown, "Go Voltr planner replay output");
  exactKeys(output, ["Opportunity", "OpportunityKey"], "Go Voltr planner replay output");
  if (output.Opportunity === null) {
    if (output.OpportunityKey !== null) throw new Error("Go Voltr planner replay keyed an empty decision");
    return { opportunity: null, opportunityKey: null };
  }
  return { opportunity: object(output.Opportunity, "Go Voltr planner replay opportunity"), opportunityKey: sha(output.OpportunityKey, "Go Voltr planner replay opportunity key") };
}

function replayLeg(call: GoVoltrReplayCall, label: string): EarnReplayLeg {
  const { opportunity, opportunityKey } = runGoVoltrReplay(call);
  if (!opportunity || !opportunityKey) throw new Error(`${label} planned no Voltr leg`);
  const plan = object(opportunity.Plan, `${label}.Plan`);
  const amount = positiveInteger(opportunity.AmountRaw, `${label}.AmountRaw`);
  return {
    class: stringField(opportunity, "Class", label),
    operation: stringField(plan, "operation", `${label}.Plan`),
    strategyId: stringField(plan, "strategy_id", `${label}.Plan`),
    sourceReserve: opportunity.SourceReserve === null ? null : stringField(opportunity, "SourceReserve", label),
    targetReserve: stringField(opportunity, "TargetReserve", label),
    amountRaw: amount.toString(),
    sourceApyBps: nonNegativeInteger(opportunity.SourceAPYBPS, `${label}.SourceAPYBPS`),
    targetApyBps: nonNegativeInteger(opportunity.TargetAPYBPS, `${label}.TargetAPYBPS`),
    protectedContextSlot: positiveInteger(plan.protected_context_slot, `${label}.Plan.protected_context_slot`),
    intentSha256: sha(plan.intent_sha256, `${label}.Plan.intent_sha256`),
    planSha256: sha256Canonical(plan),
    opportunityKey,
  };
}

function currentEarnSourceBindings(): readonly Readonly<{ path: string; sha256: string }>[] {
  return EARN_ADAPTER_SOURCE_PATHS.map((path) => ({
    path,
    sha256: createHash("sha256").update(readFileSync(resolve(REPOSITORY_ROOT, path))).digest("hex"),
  }));
}

/** Recompute the complete replay from its saved input through the Go planner. */
function recomputeEarnSharedReplay(input: EarnReplayInput): EarnSharedReplay {
  const sourceLeg = replayLeg(input.source, "Earn replay source leg");
  const destinationLeg = replayLeg(input.destination, "Earn replay destination leg");
  const probe = runGoVoltrReplay(input.priority);
  const decision = probe.opportunity === null ? "none" : stringField(probe.opportunity, "Class", "Earn replay priority probe");
  if (decision === "yield_optimization") throw new Error("positive withdrawal demand did not block normal Voltr optimization");
  const priorityProbe = {
    withdrawalDemandRaw: String(input.priority.Observation.PendingRaw),
    decision,
    normalOptimization: "blocked" as const,
  };
  return {
    kind: EARN_ADAPTER_REPLAY_KIND,
    implementation: EARN_ADAPTER_REPLAY_IMPLEMENTATION,
    input,
    inputSha256: sha256Canonical(input),
    sourceLeg,
    destinationLeg,
    priorityProbe,
    outputSha256: sha256Canonical({ sourceLeg, destinationLeg, priorityProbe }),
    sourceBindings: currentEarnSourceBindings(),
  };
}

export type EarnReplayExpectation = Readonly<{
  movementId: string;
  sourceStrategyId: PartnerStrategyId;
  destinationStrategyId: PartnerStrategyId;
  amountRaw: bigint;
  /** Protected-before context of the confirmed source withdrawal. */
  sourceContextSlot: number;
  /** Confirmed idle readback context between the two legs. */
  destinationContextSlot: number;
  /** Idle balance in the source leg's confirmed prestate. */
  confirmedIdleRaw: bigint;
  /** Positive demand from the separately captured withdrawal scanner. */
  priorityWithdrawalDemandRaw: bigint;
}>;

/** The replay must be the exact confirmed movement and priority probe. */
function assertReplayIsMovement(replay: EarnSharedReplay, expected: EarnReplayExpectation): EarnSharedReplay {
  const { input, sourceLeg, destinationLeg, priorityProbe } = replay;
  const sourceReserve = partnerStrategyIdentity(expected.sourceStrategyId).reserve;
  const destinationReserve = partnerStrategyIdentity(expected.destinationStrategyId).reserve;
  const amountRaw = expected.amountRaw.toString();
  if (input.movementId !== expected.movementId || input.sourceStrategyId !== expected.sourceStrategyId || input.destinationStrategyId !== expected.destinationStrategyId || BigInt(input.amountRaw) !== expected.amountRaw) throw new Error("Earn replay input is not the exact confirmed movement");
  if (sourceLeg.class !== "yield_optimization" || sourceLeg.operation !== "withdraw" || sourceLeg.strategyId !== expected.sourceStrategyId || sourceLeg.sourceReserve !== sourceReserve || sourceLeg.targetReserve !== VOLTR_IDLE_TARGET || sourceLeg.amountRaw !== amountRaw || sourceLeg.targetApyBps <= sourceLeg.sourceApyBps || sourceLeg.protectedContextSlot !== expected.sourceContextSlot) throw new Error("Earn source leg is not the planner's exact zero-demand yield withdrawal into Voltr idle");
  if (input.source.Observation.PendingRaw !== 0 || BigInt(input.source.Observation.IdleRaw as number) !== expected.confirmedIdleRaw) throw new Error("Earn source observation is not the confirmed zero-demand prestate");
  if (destinationLeg.class !== "idle_allocation" || destinationLeg.operation !== "deposit" || destinationLeg.strategyId !== expected.destinationStrategyId || destinationLeg.sourceReserve !== null || destinationLeg.targetReserve !== destinationReserve || destinationLeg.amountRaw !== amountRaw || destinationLeg.protectedContextSlot !== expected.destinationContextSlot || input.destination.Observation.PendingRaw !== 0) throw new Error("Earn destination leg is not the planner's exact idle allocation into the destination strategy");
  if (expected.priorityWithdrawalDemandRaw <= 0n || priorityProbe.withdrawalDemandRaw !== expected.priorityWithdrawalDemandRaw.toString()) throw new Error("Earn priority probe does not replay the exact positive scanner demand");
  return replay;
}

/**
 * Validate a persisted Earn replay: it must equal a fresh Go replay of its own
 * input and the current sources, and that replay must be the exact confirmed
 * movement.
 */
export function validateEarnSharedReplay(value: unknown, expected: EarnReplayExpectation): EarnSharedReplay {
  const root = object(value, "earnAdapter.sharedReplay");
  exactKeys(root, ["kind", "implementation", "input", "inputSha256", "sourceLeg", "destinationLeg", "priorityProbe", "outputSha256", "sourceBindings"], "earnAdapter.sharedReplay");
  if (root.kind !== EARN_ADAPTER_REPLAY_KIND || root.implementation !== EARN_ADAPTER_REPLAY_IMPLEMENTATION) throw new Error("Earn replay is not the maintained Go Voltr planner replay contract");
  const replay = recomputeEarnSharedReplay(replayInput(root.input, "earnAdapter.sharedReplay.input"));
  if (canonicalJson(root) !== canonicalJson(replay)) throw new Error("persisted Earn replay differs from a fresh Go planner replay of its input and current sources");
  return assertReplayIsMovement(replay, expected);
}

export type EarnAdapterProducerInput = Readonly<{
  /** The manifest-bound route identity. No route identity is inferred. */
  routeId: string;
  routeSpecSha256: string;
  /** Protected-before context for the confirmed source withdrawal. */
  protectedBeforeContextSlot: number;
  /** Confirmed idle balance observed in that same protected-before context. */
  confirmedIdleRaw: bigint;
  /** Positive demand from the separately captured withdrawal scanner. */
  priorityWithdrawalDemandRaw: bigint;
  movement: Readonly<{
    movementId: string;
    sourceStrategyId: PartnerStrategyId;
    destinationStrategyId: PartnerStrategyId;
    amountRaw: bigint;
    sourceWithdrawSignature: string;
    sourceWithdrawSlot: number;
    idleReadbackContextSlot: number;
    destinationDepositSignature: string;
    destinationDepositSlot: number;
    sourceIdleDeltaRaw: bigint;
    destinationIdleDeltaRaw: bigint;
    timerDecisionCount: number;
    withdrawalDemandReservedRaw: bigint;
  }>;
  /** Exact saved planner inputs replayed through `loyal-evidence --kind voltr`. */
  replayInput: JsonObject;
}>;

export type EarnAdapterEvidenceArtifact = Readonly<{
  schemaVersion: 1;
  evidenceType: "backyard-voltr-shared-earn-adapter-confirmed";
  broadcast: false;
  routeId: string;
  routeSpecSha256: string;
  executionKind: "voltr-manager";
  priority: "withdrawal-restoration-first";
  normalOptimizationIntervalSeconds: string;
  sourceBindings: readonly Readonly<{ path: string; sha256: string }>[];
  outboxContract: Readonly<{
    oneDurableMovement: true;
    sourceWithdrawThenDestinationDeposit: true;
    leaseFencing: true;
    oneSend: true;
    confirmedReconciliation: true;
    recoveryKeepsMovementIdentity: true;
    directKaminoExecutorUsed: false;
  }>;
  movement: Readonly<{
    movementId: string;
    sourceStrategyId: PartnerStrategyId;
    destinationStrategyId: PartnerStrategyId;
    amountRaw: string;
    sourceWithdrawSignature: string;
    sourceWithdrawSlot: number;
    idleReadbackContextSlot: number;
    destinationDepositSignature: string;
    destinationDepositSlot: number;
    timerDecisionCount: number;
    withdrawalDemandReservedRaw: string;
  }>;
  sharedReplay: EarnSharedReplay;
}>;

function parsedBigint(value: unknown, label: string): bigint {
  if (typeof value === "string" && /^(0|[1-9][0-9]*)$/.test(value)) return BigInt(value);
  if (typeof value === "number" && Number.isSafeInteger(value) && value >= 0) return BigInt(value);
  throw new Error(`${label} must be a canonical non-negative integer`);
}

function parsedSignedBigint(value: unknown, label: string): bigint {
  if (typeof value === "string" && /^-?(0|[1-9][0-9]*)$/.test(value)) return BigInt(value);
  if (typeof value === "number" && Number.isSafeInteger(value)) return BigInt(value);
  throw new Error(`${label} must be a canonical integer`);
}

/** Parse the JSON-safe CLI form, converting raw integer strings to bigint. */
export function parseEarnAdapterProducerInput(value: unknown): EarnAdapterProducerInput {
  const root = object(value, "earnAdapter producer input");
  const movement = object(root.movement, "earnAdapter producer input.movement");
  const replay = object(root.replayInput, "earnAdapter producer input.replayInput");
  const sourceStrategyId = stringField(movement, "sourceStrategyId", "earnAdapter producer input.movement") as PartnerStrategyId;
  const destinationStrategyId = stringField(movement, "destinationStrategyId", "earnAdapter producer input.movement") as PartnerStrategyId;
  return {
    routeId: stringField(root, "routeId", "earnAdapter producer input"),
    routeSpecSha256: stringField(root, "routeSpecSha256", "earnAdapter producer input"),
    protectedBeforeContextSlot: positiveInteger(root.protectedBeforeContextSlot, "earnAdapter producer input.protectedBeforeContextSlot"),
    confirmedIdleRaw: parsedBigint(root.confirmedIdleRaw, "earnAdapter producer input.confirmedIdleRaw"),
    priorityWithdrawalDemandRaw: parsedBigint(root.priorityWithdrawalDemandRaw, "earnAdapter producer input.priorityWithdrawalDemandRaw"),
    movement: {
      movementId: stringField(movement, "movementId", "earnAdapter producer input.movement"),
      sourceStrategyId,
      destinationStrategyId,
      amountRaw: parsedBigint(movement.amountRaw, "earnAdapter producer input.movement.amountRaw"),
      sourceWithdrawSignature: stringField(movement, "sourceWithdrawSignature", "earnAdapter producer input.movement"),
      sourceWithdrawSlot: positiveInteger(movement.sourceWithdrawSlot, "earnAdapter producer input.movement.sourceWithdrawSlot"),
      idleReadbackContextSlot: positiveInteger(movement.idleReadbackContextSlot, "earnAdapter producer input.movement.idleReadbackContextSlot"),
      destinationDepositSignature: stringField(movement, "destinationDepositSignature", "earnAdapter producer input.movement"),
      destinationDepositSlot: positiveInteger(movement.destinationDepositSlot, "earnAdapter producer input.movement.destinationDepositSlot"),
      sourceIdleDeltaRaw: parsedBigint(movement.sourceIdleDeltaRaw, "earnAdapter producer input.movement.sourceIdleDeltaRaw"),
      destinationIdleDeltaRaw: parsedSignedBigint(movement.destinationIdleDeltaRaw, "earnAdapter producer input.movement.destinationIdleDeltaRaw"),
      timerDecisionCount: positiveInteger(movement.timerDecisionCount, "earnAdapter producer input.movement.timerDecisionCount"),
      withdrawalDemandReservedRaw: parsedBigint(movement.withdrawalDemandReservedRaw, "earnAdapter producer input.movement.withdrawalDemandReservedRaw"),
    },
    replayInput: replay,
  };
}

function safeRaw(value: bigint, label: string, positive = false): number {
  if (value < 0n || (positive && value === 0n) || value > BigInt(Number.MAX_SAFE_INTEGER)) {
    throw new Error(`${label} must be a ${positive ? "positive " : "non-negative "}safe integer`);
  }
  return Number(value);
}

/**
 * Produce the no-broadcast outer Earn evidence artifact from already-confirmed
 * lifecycle facts and saved Go planner inputs. This function never contacts
 * RPC, loads a signer, writes a database row, or broadcasts a transaction. It
 * fails closed unless the Go planner chooses the exact zero-demand source ->
 * idle -> destination movement and positive demand blocks optimization.
 */
export function produceEarnAdapterEvidence(input: EarnAdapterProducerInput): EarnAdapterEvidenceArtifact {
  if (input.routeId !== PARTNER_FOUR_MARKET_ROUTE.id) throw new Error(`Earn adapter routeId must equal ${PARTNER_FOUR_MARKET_ROUTE.id}`);
  if (input.routeSpecSha256 !== fourMarketRouteSpecSha256()) throw new Error(`Earn adapter routeSpecSha256 must equal ${fourMarketRouteSpecSha256()}`);
  const protectedBeforeContextSlot = positiveInteger(input.protectedBeforeContextSlot, "Earn adapter protectedBeforeContextSlot");
  safeRaw(input.confirmedIdleRaw, "Earn adapter confirmedIdleRaw");
  safeRaw(input.priorityWithdrawalDemandRaw, "Earn adapter priorityWithdrawalDemandRaw", true);

  const movement = input.movement;
  safeRaw(movement.amountRaw, "Earn adapter movement.amountRaw", true);
  if (movement.sourceStrategyId !== "main" || movement.destinationStrategyId !== "onre") throw new Error("Earn adapter producer is intentionally bound to Main-withdraw -> OnRe-deposit");
  if (!/^[0-9a-f]{64}$/.test(movement.movementId)) throw new Error("Earn adapter movementId must be a lowercase SHA-256");
  for (const [label, value] of [["sourceWithdrawSignature", movement.sourceWithdrawSignature], ["destinationDepositSignature", movement.destinationDepositSignature]] as const) {
    if (value.trim() === "") throw new Error(`Earn adapter ${label} must be non-empty`);
  }
  if (movement.sourceWithdrawSignature === movement.destinationDepositSignature) throw new Error("Earn adapter source and destination signatures must differ");
  const sourceWithdrawSlot = positiveInteger(movement.sourceWithdrawSlot, "Earn adapter sourceWithdrawSlot");
  const destinationDepositSlot = positiveInteger(movement.destinationDepositSlot, "Earn adapter destinationDepositSlot");
  const idleReadbackContextSlot = positiveInteger(movement.idleReadbackContextSlot, "Earn adapter idleReadbackContextSlot");
  if (sourceWithdrawSlot < protectedBeforeContextSlot || destinationDepositSlot <= sourceWithdrawSlot || idleReadbackContextSlot < sourceWithdrawSlot || idleReadbackContextSlot > destinationDepositSlot) throw new Error("Earn adapter confirmed movement slots are not ordered protected-before <= source <= idle readback <= destination");
  if (movement.timerDecisionCount !== 1) throw new Error("Earn adapter must contain exactly one timer decision");
  if (movement.withdrawalDemandReservedRaw !== 0n) throw new Error("Earn adapter normal movement must reserve zero withdrawal demand");
  if (movement.sourceIdleDeltaRaw <= 0n || movement.sourceIdleDeltaRaw > movement.amountRaw) throw new Error("Earn adapter source idle delta is not a bounded positive movement");
  if (movement.destinationIdleDeltaRaw !== -movement.amountRaw) throw new Error("Earn adapter destination idle delta is not the exact negative movement");

  const sharedReplay = assertReplayIsMovement(recomputeEarnSharedReplay(replayInput(input.replayInput, "Earn replay input")), {
    movementId: movement.movementId,
    sourceStrategyId: movement.sourceStrategyId,
    destinationStrategyId: movement.destinationStrategyId,
    amountRaw: movement.amountRaw,
    sourceContextSlot: protectedBeforeContextSlot,
    destinationContextSlot: idleReadbackContextSlot,
    confirmedIdleRaw: input.confirmedIdleRaw,
    priorityWithdrawalDemandRaw: input.priorityWithdrawalDemandRaw,
  });
  return {
    schemaVersion: 1,
    evidenceType: "backyard-voltr-shared-earn-adapter-confirmed",
    broadcast: false,
    routeId: input.routeId,
    routeSpecSha256: input.routeSpecSha256,
    executionKind: "voltr-manager",
    priority: "withdrawal-restoration-first",
    normalOptimizationIntervalSeconds: PARTNER_FOUR_MARKET_ROUTE.normalOptimizationIntervalSeconds.toString(),
    sourceBindings: sharedReplay.sourceBindings,
    outboxContract: {
      oneDurableMovement: true,
      sourceWithdrawThenDestinationDeposit: true,
      leaseFencing: true,
      oneSend: true,
      confirmedReconciliation: true,
      recoveryKeepsMovementIdentity: true,
      directKaminoExecutorUsed: false,
    },
    movement: {
      movementId: movement.movementId,
      sourceStrategyId: movement.sourceStrategyId,
      destinationStrategyId: movement.destinationStrategyId,
      amountRaw: movement.amountRaw.toString(),
      sourceWithdrawSignature: movement.sourceWithdrawSignature,
      sourceWithdrawSlot,
      idleReadbackContextSlot,
      destinationDepositSignature: movement.destinationDepositSignature,
      destinationDepositSlot,
      timerDecisionCount: 1,
      withdrawalDemandReservedRaw: "0",
    },
    sharedReplay,
  };
}

export function earnAdapterSourcePaths(): readonly string[] {
  return EARN_ADAPTER_SOURCE_PATHS;
}
