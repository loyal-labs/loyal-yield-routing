import { createHash } from "node:crypto";
import { execFileSync } from "node:child_process";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { fileURLToPath } from "node:url";

import {
  PARTNER_FOUR_MARKET_ROUTE,
  fourMarketRouteSpecSha256,
  partnerStrategyIdentity,
  type PartnerStrategyId,
} from "../domain/route-spec.js";

/**
 * The adapter is deliberately not an economic planner.  Earn owns market
 * observation, ranking, and durable outbox publication; this module only
 * validates the replay envelope that Earn emits for the Voltr handoff.
 * Keeping this boundary projection-only prevents a second TypeScript planner
 * from silently diverging from `fleet_orchestration::{observation,planner}`.
 */
export const EARN_ADAPTER_REPLAY_KIND = "loyal-earn-shared-observation-planner-replay-v1" as const;
export const EARN_ADAPTER_REPLAY_SOURCE =
  "crates/loyal-yield-orchestrator/src/bin/backyard-voltr-earn-replay.rs + crates/loyal-yield-orchestrator/src/fleet_orchestration/{mod,observation,planner}.rs + crates/loyal-yield-store/src/fleet_orchestration/{domain,queue}.rs" as const;

const EARN_ADAPTER_SOURCE_PATHS = [
  "crates/loyal-yield-orchestrator/src/fleet_orchestration/observation.rs",
  "crates/loyal-yield-orchestrator/src/fleet_orchestration/planner.rs",
  "crates/loyal-yield-store/src/fleet_orchestration/queue.rs",
  "crates/loyal-yield-orchestrator/src/bin/backyard-voltr-earn-replay.rs",
  "crates/loyal-yield-orchestrator/src/fleet_orchestration/mod.rs",
  "crates/loyal-yield-store/src/fleet_orchestration/domain.rs",
  "tools/backyard-voltr/src/domain/route-spec.ts",
  "tools/backyard-voltr/src/runtime/earn-adapter.ts",
] as const;

type JsonObject = Readonly<Record<string, unknown>>;
const REPOSITORY_ROOT = resolve(fileURLToPath(new URL("../../../..", import.meta.url)));

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

function bigintString(value: unknown, label: string): bigint {
  if (typeof value !== "string" || !/^(0|[1-9][0-9]*)$/.test(value)) throw new Error(`${label} must be a canonical non-negative integer string`);
  return BigInt(value);
}

export type EarnSharedReplay = Readonly<{
  kind: typeof EARN_ADAPTER_REPLAY_KIND;
  observation: Readonly<{
    contextSlot: number;
    inputSha256: string;
    configuredIdleFloorRaw?: string;
    confirmedIdleRaw: string;
    withdrawalDemandRaw: string;
    requiredIdleRaw: string;
    idleShortfallRaw: string;
  }>;
  planner: Readonly<{
    implementation: "loyal-yield-orchestrator::fleet_orchestration::{observation,planner}";
    inputSha256: string;
    outputSha256: string;
    recomputed: true;
    selectedOpportunityId?: number;
    selectedCount?: 1;
    decision: "normal-optimization";
    selectedSourceStrategyId: string;
    selectedSourceReserve: string;
    selectedTargetReserve: string;
    selectedAmountRaw: string;
    selectedNotionalUsdMicros: string;
    target: string;
    path: readonly string[];
  }>;
  normalOptimization: Readonly<{
    status: "eligible";
    withdrawalDemandRaw: string;
    sourceReserve: string;
    targetReserve: string;
    path: readonly string[];
    selectedOpportunityId: number;
    selectedNotionalUsdMicros: string;
    semanticSha256: string;
  }>;
  priorityProbe: Readonly<{
    inputSha256: string;
    outputSha256: string;
    withdrawalDemandRaw: string;
    normalOptimization: Readonly<{ status: "blocked"; reason: "positive-withdrawal-demand"; candidateCount: number; selectedCount: 0; deferredCount: number }>;
    preRequestManagerPair: Readonly<{ present: boolean; restoresLaterRequest: false; semantic: "not-a-restoration-proof" }>;
  }>;
  durable: Readonly<{
    implementation: "loyal-yield-store::fleet_orchestration::queue";
    eventKind: "rebalance_opportunity";
    aggregateKind: "rebalance_opportunity";
    originId: string;
    generation: number;
    movementId: string;
    outboxRows: number;
    replayed: true;
    duplicateRows: number;
    leaseFenced: true;
    idempotencyKeySha256: string;
    movementPath: readonly string[];
  }>;
  rustReplay: Readonly<{
    input: JsonObject;
    outputSha256: string;
    sourceBindings: readonly Readonly<{ path: string; sha256: string }>[];
  }>;
}>;

/**
 * Validate and normalize an Earn replay.  This checks only identities and
 * arithmetic that can be checked from the persisted replay; it intentionally
 * does not claim to recreate the Rust planner in TypeScript.
 */
export function validateEarnSharedReplay(
  value: unknown,
  expected: Readonly<{ movementId: string; sourceStrategyId: string; destinationStrategyId?: string; sourceReserve: string; targetReserve?: string; amountRaw: bigint; expectedContextSlot?: number; /** @deprecated source-tx lower bound is intentionally ignored; use expectedContextSlot. */ minimumContextSlot?: number; expectedObservation: Readonly<{ configuredIdleFloorRaw?: bigint; confirmedIdleRaw: bigint; withdrawalDemandRaw: bigint; requiredIdleRaw: bigint; idleShortfallRaw: bigint }>; rustSourceBindings: readonly Readonly<{ path: string; sha256: string }>[] }>,
): EarnSharedReplay {
  const root = object(value, "earnAdapter.sharedReplay");
  exactKeys(root, ["kind", "observation", "planner", "normalOptimization", "priorityProbe", "durable", "rustReplay"], "earnAdapter.sharedReplay");
  if (root.kind !== EARN_ADAPTER_REPLAY_KIND) throw new Error("Earn replay kind is not the maintained shared replay contract");
  const observation = object(root.observation, "earnAdapter.sharedReplay.observation");
  exactKeys(observation, ["contextSlot", "inputSha256", "configuredIdleFloorRaw", "confirmedIdleRaw", "withdrawalDemandRaw", "requiredIdleRaw", "idleShortfallRaw"], "earnAdapter.sharedReplay.observation");
  const contextSlot = positiveInteger(observation.contextSlot, "earnAdapter.sharedReplay.observation.contextSlot");
  if (expected.expectedContextSlot === undefined || contextSlot !== expected.expectedContextSlot) throw new Error("Earn replay observation is not the exact protected-before context");
  const inputSha256 = sha(observation.inputSha256, "earnAdapter.sharedReplay.observation.inputSha256");
  const configuredIdleFloorRaw = bigintString(observation.configuredIdleFloorRaw, "earnAdapter.sharedReplay.observation.configuredIdleFloorRaw");
  const confirmedIdleRaw = bigintString(observation.confirmedIdleRaw, "earnAdapter.sharedReplay.observation.confirmedIdleRaw");
  const withdrawalDemandRaw = bigintString(observation.withdrawalDemandRaw, "earnAdapter.sharedReplay.observation.withdrawalDemandRaw");
  const requiredIdleRaw = bigintString(observation.requiredIdleRaw, "earnAdapter.sharedReplay.observation.requiredIdleRaw");
  const idleShortfallRaw = bigintString(observation.idleShortfallRaw, "earnAdapter.sharedReplay.observation.idleShortfallRaw");
  if (configuredIdleFloorRaw !== 0n || requiredIdleRaw !== configuredIdleFloorRaw + withdrawalDemandRaw || idleShortfallRaw !== (requiredIdleRaw > confirmedIdleRaw ? requiredIdleRaw - confirmedIdleRaw : 0n)) throw new Error("Earn replay observation demand arithmetic is inconsistent with the frozen idle floor");
  if (expected.expectedObservation.configuredIdleFloorRaw === undefined || configuredIdleFloorRaw !== expected.expectedObservation.configuredIdleFloorRaw || confirmedIdleRaw !== expected.expectedObservation.confirmedIdleRaw || withdrawalDemandRaw !== expected.expectedObservation.withdrawalDemandRaw || requiredIdleRaw !== expected.expectedObservation.requiredIdleRaw || idleShortfallRaw !== expected.expectedObservation.idleShortfallRaw) throw new Error("Earn replay observation does not equal the exact confirmed withdrawal scanner demand");

  const planner = object(root.planner, "earnAdapter.sharedReplay.planner");
  exactKeys(planner, ["implementation", "inputSha256", "outputSha256", "recomputed", "selectedOpportunityId", "selectedSourceStrategyId", "selectedSourceReserve", "selectedTargetReserve", "selectedAmountRaw", "selectedNotionalUsdMicros", "selectedCount", "decision", "target", "path"], "earnAdapter.sharedReplay.planner");
  if (planner.implementation !== "loyal-yield-orchestrator::fleet_orchestration::{observation,planner}" || planner.recomputed !== true || planner.decision !== "normal-optimization" || typeof planner.target !== "string" || planner.target === "voltr-idle") throw new Error("Earn replay is not produced by the normal shared planner boundary");
  const plannerInputSha256 = sha(planner.inputSha256, "earnAdapter.sharedReplay.planner.inputSha256");
  const outputSha256 = sha(planner.outputSha256, "earnAdapter.sharedReplay.planner.outputSha256");
  if (planner.selectedSourceStrategyId !== expected.sourceStrategyId || planner.selectedCount !== 1 || typeof planner.selectedOpportunityId !== "number" || !Number.isSafeInteger(planner.selectedOpportunityId) || planner.selectedOpportunityId <= 0 || bigintString(planner.selectedAmountRaw, "earnAdapter.sharedReplay.planner.selectedAmountRaw") !== expected.amountRaw || bigintString(planner.selectedNotionalUsdMicros, "earnAdapter.sharedReplay.planner.selectedNotionalUsdMicros") !== expected.amountRaw || planner.selectedSourceReserve !== expected.sourceReserve || (expected.targetReserve !== undefined && planner.selectedTargetReserve !== expected.targetReserve) || planner.target !== planner.selectedTargetReserve || !Array.isArray(planner.path) || JSON.stringify(planner.path) !== JSON.stringify([expected.sourceReserve, "voltr-idle", planner.selectedTargetReserve])) throw new Error("Earn planner decision is not the exact normal Voltr movement");
  const normalOptimization = object(root.normalOptimization, "earnAdapter.sharedReplay.normalOptimization");
  exactKeys(normalOptimization, ["status", "withdrawalDemandRaw", "sourceReserve", "targetReserve", "path", "selectedOpportunityId", "selectedNotionalUsdMicros", "semanticSha256"], "earnAdapter.sharedReplay.normalOptimization");
  if (normalOptimization.status !== "eligible" || bigintString(normalOptimization.withdrawalDemandRaw, "earnAdapter.sharedReplay.normalOptimization.withdrawalDemandRaw") !== 0n || normalOptimization.sourceReserve !== expected.sourceReserve || normalOptimization.targetReserve !== planner.selectedTargetReserve || JSON.stringify(normalOptimization.path) !== JSON.stringify([expected.sourceReserve, "voltr-idle", planner.selectedTargetReserve]) || normalOptimization.selectedOpportunityId !== planner.selectedOpportunityId || bigintString(normalOptimization.selectedNotionalUsdMicros, "earnAdapter.sharedReplay.normalOptimization.selectedNotionalUsdMicros") !== expected.amountRaw) throw new Error("Earn normal optimization path is not source reserve -> idle -> exact target reserve");
  const normalSemantic = { sourceReserve: expected.sourceReserve, targetReserve: planner.selectedTargetReserve, path: [expected.sourceReserve, "voltr-idle", planner.selectedTargetReserve], withdrawalDemandRaw: 0, selectedNotionalUsdMicros: Number(expected.amountRaw) };
  if (sha(normalOptimization.semanticSha256, "earnAdapter.sharedReplay.normalOptimization.semanticSha256") !== sha256RustJson(normalSemantic)) throw new Error("Earn normal optimization semantic hash does not bind the exact path and notional");
  const priorityProbe = object(root.priorityProbe, "earnAdapter.sharedReplay.priorityProbe");
  exactKeys(priorityProbe, ["inputSha256", "outputSha256", "withdrawalDemandRaw", "normalOptimization", "preRequestManagerPair"], "earnAdapter.sharedReplay.priorityProbe");
  if (bigintString(priorityProbe.withdrawalDemandRaw, "earnAdapter.sharedReplay.priorityProbe.withdrawalDemandRaw") <= 0n) throw new Error("Earn priority probe must use positive withdrawal demand");
  const probeNormal = object(priorityProbe.normalOptimization, "earnAdapter.sharedReplay.priorityProbe.normalOptimization");
  exactKeys(probeNormal, ["status", "reason", "candidateCount", "selectedCount", "deferredCount"], "earnAdapter.sharedReplay.priorityProbe.normalOptimization");
  if (probeNormal.status !== "blocked" || probeNormal.reason !== "positive-withdrawal-demand" || probeNormal.selectedCount !== 0 || typeof probeNormal.candidateCount !== "number" || !Number.isSafeInteger(probeNormal.candidateCount) || probeNormal.candidateCount <= 0 || probeNormal.deferredCount !== probeNormal.candidateCount) throw new Error("Earn positive-demand priority probe does not block/defer normal optimization");
  const preRequestPair = object(priorityProbe.preRequestManagerPair, "earnAdapter.sharedReplay.priorityProbe.preRequestManagerPair");
  exactKeys(preRequestPair, ["present", "restoresLaterRequest", "semantic"], "earnAdapter.sharedReplay.priorityProbe.preRequestManagerPair");
  if (typeof preRequestPair.present !== "boolean" || preRequestPair.restoresLaterRequest !== false || preRequestPair.semantic !== "not-a-restoration-proof") throw new Error("Earn priority probe makes an invalid pre-request manager restoration claim");

  const durable = object(root.durable, "earnAdapter.sharedReplay.durable");
  exactKeys(durable, ["implementation", "eventKind", "aggregateKind", "originId", "generation", "movementId", "outboxRows", "replayed", "duplicateRows", "leaseFenced", "idempotencyKeySha256", "movementPath"], "earnAdapter.sharedReplay.durable");
  if (durable.implementation !== "loyal-yield-store::fleet_orchestration::queue" || durable.eventKind !== "rebalance_opportunity" || durable.aggregateKind !== "rebalance_opportunity" || durable.movementId !== expected.movementId || durable.replayed !== true || durable.leaseFenced !== true || positiveInteger(durable.generation, "earnAdapter.sharedReplay.durable.generation") !== durable.generation || positiveInteger(durable.outboxRows, "earnAdapter.sharedReplay.durable.outboxRows") !== durable.outboxRows || durable.outboxRows !== 1 || durable.duplicateRows !== 1 || !Array.isArray(durable.movementPath) || JSON.stringify(durable.movementPath) !== JSON.stringify([expected.sourceReserve, "voltr-idle", planner.selectedTargetReserve])) throw new Error("Earn durable replay is not the exact idempotent normal optimization outbox movement");
  const originId = sha(durable.originId, "earnAdapter.sharedReplay.durable.originId");
  if (typeof durable.duplicateRows !== "number" || !Number.isSafeInteger(durable.duplicateRows) || durable.duplicateRows !== 1) throw new Error("Earn durable replay must prove one duplicate replay was absorbed");
  const rustReplay = object(root.rustReplay, "earnAdapter.sharedReplay.rustReplay");
  exactKeys(rustReplay, ["input", "outputSha256", "sourceBindings"], "earnAdapter.sharedReplay.rustReplay");
  if (!Array.isArray(rustReplay.sourceBindings)) throw new Error("earnAdapter.sharedReplay.rustReplay.sourceBindings must be an array");
  const persistedSources = rustReplay.sourceBindings.map((raw, index) => {
    const row = object(raw, `earnAdapter.sharedReplay.rustReplay.sourceBindings[${index}]`);
    exactKeys(row, ["path", "sha256"], `earnAdapter.sharedReplay.rustReplay.sourceBindings[${index}]`);
    return { path: stringField(row, "path", `earnAdapter.sharedReplay.rustReplay.sourceBindings[${index}]`), sha256: sha(row.sha256, `earnAdapter.sharedReplay.rustReplay.sourceBindings[${index}]`) };
  });
  if (JSON.stringify(persistedSources) !== JSON.stringify(expected.rustSourceBindings)) throw new Error("persisted Rust replay source bindings do not match the maintained executable/dependency source set");
  const rustInput = object(rustReplay.input, "earnAdapter.sharedReplay.rustReplay.input");
  const rustOutput = runRustReplay(rustInput);
  const expectedOutputSha256 = sha(rustReplay.outputSha256, "earnAdapter.sharedReplay.rustReplay.outputSha256");
  if (rustOutput.outputSha256 !== expectedOutputSha256) throw new Error("Rust replay output hash differs from the persisted adapter evidence");
  if (rustOutput.kind !== EARN_ADAPTER_REPLAY_KIND || rustOutput.routeId !== "loyal-backyard-four-market-usdc-v1" || rustOutput.movementId !== expected.movementId || rustOutput.sourceStrategyId !== expected.sourceStrategyId || (expected.destinationStrategyId !== undefined && rustOutput.destinationStrategyId !== expected.destinationStrategyId) || rustOutput.sourceReserve !== expected.sourceReserve || (expected.targetReserve !== undefined && rustOutput.targetReserve !== expected.targetReserve) || rustOutput.amountRaw !== Number(expected.amountRaw)) throw new Error("Rust replay output is not bound to the exact Voltr movement");
  const rustPriorityInput = object(rustInput.priorityProbe, "Rust replay input priorityProbe");
  if (sha(priorityProbe.inputSha256, "earnAdapter.sharedReplay.priorityProbe.inputSha256") !== sha256CanonicalReplay(rustPriorityInput)) throw new Error("Earn priority probe input hash does not bind the Rust replay input");
  const rustObservation = object(rustOutput.observation, "Rust replay observation");
  const rustPlanner = object(rustOutput.planner, "Rust replay planner");
  const rustDurable = object(rustOutput.durable, "Rust replay durable");
  if (rustObservation.contextSlot !== contextSlot || rustObservation.inputSha256 !== inputSha256 || rustPlanner.inputSha256 !== rustInputPlannerSha256(rustInput) || rustPlanner.inputSha256 !== plannerInputSha256 || rustPlanner.outputSha256 !== outputSha256 || rustPlanner.selectedSourceReserve !== expected.sourceReserve || rustPlanner.selectedTargetReserve !== planner.selectedTargetReserve || rustPlanner.selectedAmountRaw !== Number(expected.amountRaw) || rustPlanner.selectedNotionalUsdMicros !== Number(expected.amountRaw) || rustPlanner.decision !== "normal-optimization" || rustPlanner.target !== planner.selectedTargetReserve || rustDurable.movementId !== expected.movementId || rustDurable.replayed !== true || rustDurable.duplicateRows !== 1 || rustDurable.leaseFenced !== true) throw new Error("Rust replay does not reproduce the persisted normal optimization/outbox decision");
  const rustPriorityOutput = { withdrawalDemandRaw: Number(bigintString(priorityProbe.withdrawalDemandRaw, "earnAdapter.sharedReplay.priorityProbe.withdrawalDemandRaw")), normalOptimization: priorityProbe.normalOptimization, preRequestManagerPair: priorityProbe.preRequestManagerPair };
  if (sha(priorityProbe.outputSha256, "earnAdapter.sharedReplay.priorityProbe.outputSha256") !== sha256RustJson(rustPriorityOutput)) throw new Error("Earn priority probe output hash does not bind its blocked/deferred semantics");
  const generatedSources = Array.isArray(rustOutput.sourceBindings) ? rustOutput.sourceBindings : [];
  const normalizedSources = generatedSources.map((raw, index) => { const row = object(raw, `Rust replay sourceBindings[${index}]`); exactKeys(row, ["path", "sha256"], `Rust replay sourceBindings[${index}]`); return { path: stringField(row, "path", `Rust replay sourceBindings[${index}]`), sha256: sha(row.sha256, `Rust replay sourceBindings[${index}]`) }; });
  if (JSON.stringify(normalizedSources) !== JSON.stringify(expected.rustSourceBindings) || JSON.stringify(normalizedSources) !== JSON.stringify(persistedSources)) throw new Error("Rust replay source bindings do not match the current maintained executable/dependency source set");
  return { kind: EARN_ADAPTER_REPLAY_KIND, observation: { contextSlot, inputSha256, confirmedIdleRaw: confirmedIdleRaw.toString(), withdrawalDemandRaw: withdrawalDemandRaw.toString(), requiredIdleRaw: requiredIdleRaw.toString(), idleShortfallRaw: idleShortfallRaw.toString() }, planner: { implementation: planner.implementation as EarnSharedReplay["planner"]["implementation"], inputSha256: plannerInputSha256, outputSha256, recomputed: true, decision: "normal-optimization", selectedSourceStrategyId: planner.selectedSourceStrategyId as string, selectedSourceReserve: planner.selectedSourceReserve as string, selectedTargetReserve: planner.selectedTargetReserve as string, selectedAmountRaw: expected.amountRaw.toString(), selectedNotionalUsdMicros: expected.amountRaw.toString(), target: planner.target as string, path: planner.path as string[] }, normalOptimization: { status: "eligible", withdrawalDemandRaw: "0", sourceReserve: normalOptimization.sourceReserve as string, targetReserve: normalOptimization.targetReserve as string, path: normalOptimization.path as string[], selectedOpportunityId: normalOptimization.selectedOpportunityId as number, selectedNotionalUsdMicros: expected.amountRaw.toString(), semanticSha256: normalOptimization.semanticSha256 as string }, priorityProbe: { inputSha256: sha(priorityProbe.inputSha256, "earnAdapter.sharedReplay.priorityProbe.inputSha256"), outputSha256: sha(priorityProbe.outputSha256, "earnAdapter.sharedReplay.priorityProbe.outputSha256"), withdrawalDemandRaw: priorityProbe.withdrawalDemandRaw as string, normalOptimization: { status: "blocked", reason: "positive-withdrawal-demand", candidateCount: probeNormal.candidateCount as number, selectedCount: 0, deferredCount: probeNormal.deferredCount as number }, preRequestManagerPair: { present: preRequestPair.present as boolean, restoresLaterRequest: false, semantic: "not-a-restoration-proof" } }, durable: { implementation: durable.implementation as EarnSharedReplay["durable"]["implementation"], eventKind: "rebalance_opportunity", aggregateKind: "rebalance_opportunity", originId, generation: durable.generation as number, movementId: durable.movementId as string, outboxRows: durable.outboxRows as number, replayed: true, duplicateRows: 1, leaseFenced: true, idempotencyKeySha256: sha(durable.idempotencyKeySha256, "earnAdapter.sharedReplay.durable.idempotencyKeySha256"), movementPath: durable.movementPath as string[] }, rustReplay: { input: rustInput, outputSha256: expectedOutputSha256, sourceBindings: persistedSources } };
}

function rustInputPlannerSha256(input: JsonObject): string {
  const planner = input.planner;
  if (planner === null || typeof planner !== "object" || Array.isArray(planner)) throw new Error("Rust replay input is missing planner");
  return sha256CanonicalReplay(planner);
}

function runRustReplay(input: JsonObject): JsonObject & { outputSha256: string } {
  let stdout: string;
  try {
    stdout = execFileSync("cargo", ["run", "--offline", "--quiet", "--manifest-path", `${REPOSITORY_ROOT}/crates/loyal-yield-orchestrator/Cargo.toml`, "--bin", "backyard-voltr-earn-replay"], { cwd: REPOSITORY_ROOT, input: `${JSON.stringify(input)}\n`, encoding: "utf8", maxBuffer: 8 * 1024 * 1024 });
  } catch (error) {
    throw new Error(`maintained Rust Earn replay command failed: ${error instanceof Error ? error.message : String(error)}`);
  }
  const parsed = JSON.parse(stdout) as unknown;
  const output = object(parsed, "maintained Rust Earn replay output");
  const outputSha256 = sha(output.outputSha256, "maintained Rust Earn replay output.outputSha256");
  return { ...output, outputSha256 };
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
  /** Exact JSON input accepted by backyard-voltr-earn-replay. */
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

export type EarnAdapterReplayFacts = Readonly<{
  movementId: string;
  sourceStrategyId: PartnerStrategyId;
  destinationStrategyId: PartnerStrategyId;
  amountRaw: bigint;
  protectedBeforeContextSlot: number;
  confirmedIdleRaw: bigint;
  movementOpportunityId: number;
  planner: Readonly<{
    opportunities: readonly JsonObject[];
    economicPolicy: JsonObject;
    capacityCurves: readonly JsonObject[];
    waveLimits: JsonObject;
  }>;
  durable: Readonly<{
    originId: string;
    generation: number;
    outboxRows: number;
    duplicateRows: number;
    leaseFenced: true;
  }>;
  priorityProbe: Readonly<{
    withdrawalDemandRaw: bigint;
    preRequestManagerPairPresent: true;
  }>;
}>;

/** Build the exact, ordered input envelope consumed by the maintained Rust replay. */
export function buildEarnAdapterReplayInput(facts: EarnAdapterReplayFacts): JsonObject {
  const amountRaw = safeRaw(facts.amountRaw, "Earn replay facts amountRaw", true);
  const contextSlot = positiveInteger(facts.protectedBeforeContextSlot, "Earn replay facts protectedBeforeContextSlot");
  const confirmedIdleRaw = safeRaw(facts.confirmedIdleRaw, "Earn replay facts confirmedIdleRaw");
  const priorityWithdrawalDemandRaw = safeRaw(facts.priorityProbe.withdrawalDemandRaw, "Earn replay facts priorityWithdrawalDemandRaw", true);
  if (facts.sourceStrategyId !== "main" || facts.destinationStrategyId !== "onre") throw new Error("Earn replay facts are intentionally bound to Main-withdraw -> OnRe-deposit");
  if (!/^[0-9a-f]{64}$/.test(facts.movementId) || !/^[0-9a-f]{64}$/.test(facts.durable.originId)) throw new Error("Earn replay facts movement and origin ids must be lowercase SHA-256 values");
  const sourceReserve = partnerStrategyIdentity(facts.sourceStrategyId).reserve;
  const targetReserve = partnerStrategyIdentity(facts.destinationStrategyId).reserve;
  if (!Number.isSafeInteger(facts.movementOpportunityId) || facts.movementOpportunityId <= 0) throw new Error("Earn replay facts movementOpportunityId must be positive");
  if (facts.durable.generation <= 0 || facts.durable.outboxRows !== 1 || facts.durable.duplicateRows !== 1 || facts.durable.leaseFenced !== true) throw new Error("Earn replay facts durable contract is not exact");
  return {
    schemaVersion: 1,
    routeId: PARTNER_FOUR_MARKET_ROUTE.id,
    movementId: facts.movementId,
    sourceStrategyId: facts.sourceStrategyId,
    destinationStrategyId: facts.destinationStrategyId,
    sourceReserve,
    targetReserve,
    amountRaw,
    movementOpportunityId: facts.movementOpportunityId,
    observation: {
      contextSlot,
      configuredIdleFloorRaw: Number(PARTNER_FOUR_MARKET_ROUTE.normalOptimizationIdleFloorRaw),
      confirmedIdleRaw,
      withdrawalDemandRaw: 0,
      requiredIdleRaw: Number(PARTNER_FOUR_MARKET_ROUTE.normalOptimizationIdleFloorRaw),
      idleShortfallRaw: 0,
    },
    planner: {
      opportunities: facts.planner.opportunities,
      economicPolicy: facts.planner.economicPolicy,
      capacityCurves: facts.planner.capacityCurves,
      waveLimits: facts.planner.waveLimits,
    },
    durable: {
      originId: facts.durable.originId,
      generation: facts.durable.generation,
      outboxRows: facts.durable.outboxRows,
      duplicateRows: facts.durable.duplicateRows,
      leaseFenced: facts.durable.leaseFenced,
    },
    priorityProbe: {
      withdrawalDemandRaw: priorityWithdrawalDemandRaw,
      preRequestManagerPairPresent: facts.priorityProbe.preRequestManagerPairPresent,
    },
  };
}

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
  const movement = replayObjectField(root, "movement", "earnAdapter producer input");
  const replayInput = replayObjectField(root, "replayInput", "earnAdapter producer input");
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
    replayInput,
  };
}

function replayObjectField(root: JsonObject, key: string, label: string): JsonObject {
  return object(root[key], `${label}.${key}`);
}

function requireString(value: unknown, expected: string, label: string): void {
  if (value !== expected) throw new Error(`${label} must equal ${expected}`);
}

function requireNumber(value: unknown, expected: number, label: string): void {
  if (value !== expected) throw new Error(`${label} must equal ${expected}`);
}

function requireBoolean(value: unknown, expected: boolean, label: string): void {
  if (value !== expected) throw new Error(`${label} must equal ${expected}`);
}

function safeRaw(value: bigint, label: string, positive = false): number {
  if (value < 0n || (positive && value === 0n) || value > BigInt(Number.MAX_SAFE_INTEGER)) {
    throw new Error(`${label} must be a ${positive ? "positive " : "non-negative "}safe integer`);
  }
  return Number(value);
}

function replayBigintString(value: unknown, label: string): string {
  if (typeof value === "number") {
    if (!Number.isSafeInteger(value) || value < 0) throw new Error(`${label} must be a non-negative safe integer`);
    return String(value);
  }
  return bigintString(value, label).toString();
}

function currentEarnSourceBindings(): readonly Readonly<{ path: string; sha256: string }>[] {
  return EARN_ADAPTER_SOURCE_PATHS.map((path) => ({
    path,
    sha256: createHash("sha256").update(readFileSync(resolve(REPOSITORY_ROOT, path))).digest("hex"),
  }));
}

function rustJson(value: unknown): string {
  if (Array.isArray(value)) return `[${value.map(rustJson).join(",")}]`;
  if (value && typeof value === "object") {
    return `{${Object.entries(value as JsonObject).sort(([left], [right]) => left.localeCompare(right)).map(([key, entry]) => `${JSON.stringify(key)}:${rustJson(entry)}`).join(",")}}`;
  }
  return JSON.stringify(value);
}

function sha256RustJson(value: unknown): string {
  return createHash("sha256").update(rustJson(value)).digest("hex");
}

/**
 * Produce the no-broadcast outer Earn evidence artifact from already-confirmed
 * lifecycle facts and an exact Rust replay input. This function never contacts
 * RPC, loads a signer, writes an outbox row, or broadcasts a transaction. It
 * fails closed if the replay is not the normal zero-demand source -> idle ->
 * destination movement or if the separate positive-demand probe is absent.
 */
export function produceEarnAdapterEvidence(input: EarnAdapterProducerInput): EarnAdapterEvidenceArtifact {
  requireString(input.routeId, PARTNER_FOUR_MARKET_ROUTE.id, "Earn adapter routeId");
  requireString(input.routeSpecSha256, fourMarketRouteSpecSha256(), "Earn adapter routeSpecSha256");
  const protectedBeforeContextSlot = positiveInteger(input.protectedBeforeContextSlot, "Earn adapter protectedBeforeContextSlot");
  const confirmedIdleRaw = BigInt(input.confirmedIdleRaw);
  const priorityWithdrawalDemandRaw = BigInt(input.priorityWithdrawalDemandRaw);
  safeRaw(confirmedIdleRaw, "Earn adapter confirmedIdleRaw");
  safeRaw(priorityWithdrawalDemandRaw, "Earn adapter priorityWithdrawalDemandRaw", true);

  const movement = input.movement;
  const amountRaw = safeRaw(BigInt(movement.amountRaw), "Earn adapter movement.amountRaw", true);
  if (movement.sourceStrategyId !== "main" || movement.destinationStrategyId !== "onre") throw new Error("Earn adapter producer is intentionally bound to Main-withdraw -> OnRe-deposit");
  const sourceIdentity = partnerStrategyIdentity(movement.sourceStrategyId);
  const destinationIdentity = partnerStrategyIdentity(movement.destinationStrategyId);
  if (typeof movement.movementId !== "string" || !/^[0-9a-f]{64}$/.test(movement.movementId)) throw new Error("Earn adapter movementId must be a lowercase SHA-256");
  for (const [label, value] of [["sourceWithdrawSignature", movement.sourceWithdrawSignature], ["destinationDepositSignature", movement.destinationDepositSignature]] as const) {
    if (typeof value !== "string" || value.trim() === "") throw new Error(`Earn adapter ${label} must be non-empty`);
  }
  if (movement.sourceWithdrawSignature === movement.destinationDepositSignature) throw new Error("Earn adapter source and destination signatures must differ");
  const sourceWithdrawSlot = positiveInteger(movement.sourceWithdrawSlot, "Earn adapter sourceWithdrawSlot");
  const destinationDepositSlot = positiveInteger(movement.destinationDepositSlot, "Earn adapter destinationDepositSlot");
  const idleReadbackContextSlot = positiveInteger(movement.idleReadbackContextSlot, "Earn adapter idleReadbackContextSlot");
  if (sourceWithdrawSlot < protectedBeforeContextSlot || destinationDepositSlot <= sourceWithdrawSlot || idleReadbackContextSlot < sourceWithdrawSlot || idleReadbackContextSlot > destinationDepositSlot) throw new Error("Earn adapter confirmed movement slots are not ordered protected-before <= source <= idle readback <= destination");
  if (movement.timerDecisionCount !== 1) throw new Error("Earn adapter must contain exactly one timer decision");
  if (movement.withdrawalDemandReservedRaw !== 0n) throw new Error("Earn adapter normal movement must reserve zero withdrawal demand");
  if (movement.sourceIdleDeltaRaw <= 0n || movement.sourceIdleDeltaRaw > BigInt(amountRaw)) throw new Error("Earn adapter source idle delta is not a bounded positive movement");
  if (movement.destinationIdleDeltaRaw !== -BigInt(amountRaw)) throw new Error("Earn adapter destination idle delta is not the exact negative movement");

  const replayInput = input.replayInput;
  requireNumber(replayInput.schemaVersion, 1, "Earn replay input schemaVersion");
  requireString(replayInput.routeId, PARTNER_FOUR_MARKET_ROUTE.id, "Earn replay input routeId");
  requireString(replayInput.movementId, movement.movementId, "Earn replay input movementId");
  requireString(replayInput.sourceStrategyId, movement.sourceStrategyId, "Earn replay input sourceStrategyId");
  requireString(replayInput.destinationStrategyId, movement.destinationStrategyId, "Earn replay input destinationStrategyId");
  requireString(replayInput.sourceReserve, sourceIdentity.reserve, "Earn replay input sourceReserve");
  requireString(replayInput.targetReserve, destinationIdentity.reserve, "Earn replay input targetReserve");
  requireNumber(replayInput.amountRaw, amountRaw, "Earn replay input amountRaw");
  const observationInput = replayObjectField(replayInput, "observation", "Earn replay input");
  requireNumber(observationInput.contextSlot, protectedBeforeContextSlot, "Earn replay observation contextSlot");
  requireNumber(observationInput.confirmedIdleRaw, safeRaw(confirmedIdleRaw, "Earn adapter confirmedIdleRaw"), "Earn replay observation confirmedIdleRaw");
  requireNumber(observationInput.withdrawalDemandRaw, 0, "Earn replay observation withdrawalDemandRaw");
  const priorityInput = replayObjectField(replayInput, "priorityProbe", "Earn replay input");
  requireNumber(priorityInput.withdrawalDemandRaw, safeRaw(priorityWithdrawalDemandRaw, "Earn adapter priorityWithdrawalDemandRaw", true), "Earn replay priorityProbe withdrawalDemandRaw");
  requireBoolean(priorityInput.preRequestManagerPairPresent, true, "Earn replay priorityProbe preRequestManagerPairPresent");

  const rustOutput = runRustReplay(replayInput);
  const rustObservation = replayObjectField(rustOutput, "observation", "Rust replay output");
  const rustPlanner = replayObjectField(rustOutput, "planner", "Rust replay output");
  const rustNormal = replayObjectField(rustOutput, "normalOptimization", "Rust replay output");
  const rustPriority = replayObjectField(rustOutput, "priorityProbe", "Rust replay output");
  const rustPriorityNormal = replayObjectField(rustPriority, "normalOptimization", "Rust replay priorityProbe");
  const rustPriorityPair = replayObjectField(rustPriority, "preRequestManagerPair", "Rust replay priorityProbe");
  const rustDurable = replayObjectField(rustOutput, "durable", "Rust replay output");
  if (rustPriorityPair.present !== true) throw new Error("Rust replay priority probe lacks the required pre-request manager pair observation");
  const sourceBindings = currentEarnSourceBindings();
  const sharedReplay = {
    kind: EARN_ADAPTER_REPLAY_KIND,
    observation: {
      contextSlot: positiveInteger(rustObservation.contextSlot, "Rust replay observation.contextSlot"),
      inputSha256: sha(rustObservation.inputSha256, "Rust replay observation.inputSha256"),
      configuredIdleFloorRaw: replayBigintString(rustObservation.configuredIdleFloorRaw, "Rust replay observation.configuredIdleFloorRaw"),
      confirmedIdleRaw: replayBigintString(rustObservation.confirmedIdleRaw, "Rust replay observation.confirmedIdleRaw"),
      withdrawalDemandRaw: replayBigintString(rustObservation.withdrawalDemandRaw, "Rust replay observation.withdrawalDemandRaw"),
      requiredIdleRaw: replayBigintString(rustObservation.requiredIdleRaw, "Rust replay observation.requiredIdleRaw"),
      idleShortfallRaw: replayBigintString(rustObservation.idleShortfallRaw, "Rust replay observation.idleShortfallRaw"),
    },
    planner: {
      implementation: "loyal-yield-orchestrator::fleet_orchestration::{observation,planner}" as const,
      inputSha256: sha(rustPlanner.inputSha256, "Rust replay planner.inputSha256"),
      outputSha256: sha(rustPlanner.outputSha256, "Rust replay planner.outputSha256"),
      recomputed: true as const,
      selectedOpportunityId: positiveInteger(rustPlanner.selectedOpportunityId, "Rust replay planner.selectedOpportunityId"),
      selectedSourceStrategyId: String(rustPlanner.selectedSourceStrategyId),
      selectedSourceReserve: String(rustPlanner.selectedSourceReserve),
      selectedTargetReserve: String(rustPlanner.selectedTargetReserve),
      selectedAmountRaw: BigInt(Number(rustPlanner.selectedAmountRaw)).toString(),
      selectedNotionalUsdMicros: BigInt(Number(rustPlanner.selectedNotionalUsdMicros)).toString(),
      selectedCount: 1 as const,
      decision: "normal-optimization" as const,
      target: String(rustPlanner.target),
      path: rustPlanner.path as string[],
    },
    normalOptimization: {
      status: "eligible" as const,
      withdrawalDemandRaw: "0",
      sourceReserve: String(rustNormal.sourceReserve),
      targetReserve: String(rustNormal.targetReserve),
      path: rustNormal.path as string[],
      selectedOpportunityId: positiveInteger(rustNormal.selectedOpportunityId, "Rust replay normalOptimization.selectedOpportunityId"),
      selectedNotionalUsdMicros: BigInt(Number(rustNormal.selectedNotionalUsdMicros)).toString(),
      semanticSha256: sha(rustNormal.semanticSha256, "Rust replay normalOptimization.semanticSha256"),
    },
    priorityProbe: {
      inputSha256: sha(rustPriority.inputSha256, "Rust replay priorityProbe.inputSha256"),
      outputSha256: sha(rustPriority.outputSha256, "Rust replay priorityProbe.outputSha256"),
      withdrawalDemandRaw: replayBigintString(rustPriority.withdrawalDemandRaw, "Rust replay priorityProbe.withdrawalDemandRaw"),
      normalOptimization: {
        status: "blocked" as const,
        reason: "positive-withdrawal-demand" as const,
        candidateCount: positiveInteger(rustPriorityNormal.candidateCount, "Rust replay priorityProbe.normalOptimization.candidateCount"),
        selectedCount: 0 as const,
        deferredCount: positiveInteger(rustPriorityNormal.deferredCount, "Rust replay priorityProbe.normalOptimization.deferredCount"),
      },
      preRequestManagerPair: {
        present: rustPriorityPair.present === true,
        restoresLaterRequest: false as const,
        semantic: "not-a-restoration-proof" as const,
      },
    },
    durable: {
      implementation: "loyal-yield-store::fleet_orchestration::queue" as const,
      eventKind: "rebalance_opportunity" as const,
      aggregateKind: "rebalance_opportunity" as const,
      originId: sha(rustDurable.originId, "Rust replay durable.originId"),
      generation: positiveInteger(rustDurable.generation, "Rust replay durable.generation"),
      movementId: String(rustDurable.movementId),
      outboxRows: positiveInteger(rustDurable.outboxRows, "Rust replay durable.outboxRows"),
      replayed: true as const,
      duplicateRows: 1,
      leaseFenced: true as const,
      idempotencyKeySha256: sha(rustDurable.idempotencyKeySha256, "Rust replay durable.idempotencyKeySha256"),
      movementPath: rustDurable.movementPath as string[],
    },
    rustReplay: { input: replayInput, outputSha256: sha(rustOutput.outputSha256, "Rust replay outputSha256"), sourceBindings: sourceBindings.filter(({ path }) => path.startsWith("crates/")) },
  };
  const expectedObservation = {
    configuredIdleFloorRaw: PARTNER_FOUR_MARKET_ROUTE.normalOptimizationIdleFloorRaw,
    confirmedIdleRaw,
    withdrawalDemandRaw: 0n,
    requiredIdleRaw: PARTNER_FOUR_MARKET_ROUTE.normalOptimizationIdleFloorRaw,
    idleShortfallRaw: 0n,
  };
  validateEarnSharedReplay(sharedReplay, {
    movementId: movement.movementId,
    sourceStrategyId: movement.sourceStrategyId,
    destinationStrategyId: movement.destinationStrategyId,
    sourceReserve: sourceIdentity.reserve,
    targetReserve: destinationIdentity.reserve,
    amountRaw: BigInt(movement.amountRaw),
    expectedContextSlot: protectedBeforeContextSlot,
    expectedObservation,
    rustSourceBindings: sourceBindings.filter(({ path }) => path.startsWith("crates/")),
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
    sourceBindings,
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
      amountRaw: BigInt(movement.amountRaw).toString(),
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

export function sha256CanonicalReplay(value: unknown): string {
  return createHash("sha256").update(JSON.stringify(value)).digest("hex");
}
