import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";

import { getRequestWithdrawVaultReceiptDiscriminatorBytes } from "@voltr/vault-sdk";
import bs58 from "bs58";

import type { PartnerStrategyId } from "../domain/route-spec.js";
import { PARTNER_FOUR_MARKET_ROUTE, PARTNER_FOUR_MARKET_STRATEGIES, PARTNER_ROUTE, fourMarketRouteSpecSha256 } from "../domain/route-spec.js";

/** The manager API is deliberately the only execution boundary exposed here. */
export type LogicalManagerWithdraw = Readonly<{
  strategyId: PartnerStrategyId;
  reserve: string;
  amountRaw: bigint;
  operation: "manager-withdraw";
  originId: string;
}>;

export type WithdrawalRestorationSource = Readonly<{
  strategyId: PartnerStrategyId;
  reserve: string;
  availableRaw: bigint;
  /** Lower is safer: this is the expected net yield sacrificed by unwinding. */
  netYieldLossBps: bigint;
  /** Lower is preferred when yield loss is equal. */
  unwindCostLamports: bigint;
  observedContextSlot: number;
  positionFingerprint: string;
}>;

export type WithdrawalRestorationScan = Readonly<{
  verdict: "PARTNER_WITHDRAWAL_DEMAND_SCAN_PASS";
  routeId: string;
  routeSpecSha256: string;
  vault: string;
  observationContextSlot: number;
  generationFingerprint: string;
  rawQuerySha256: string;
  queryConfigSha256: string;
  requestOrigin: WithdrawalRestorationRequestOrigin;
  receipts: readonly Readonly<{
    receipt: string;
    user: string;
    upperBoundAssetRaw: bigint;
    generationFingerprint: string;
  }>[];
  demand: Readonly<{
    configuredIdleFloorRaw: bigint;
    confirmedIdleRaw: bigint;
    pendingWithdrawalUpperBoundRaw: bigint;
    requiredIdleRaw: bigint;
    idleShortfallRaw: bigint;
  }>;
}>;

/** Immutable request-generation tuple carried into the durable outbox. */
export type WithdrawalRestorationRequestOrigin = Readonly<{
  signature: string;
  eventIndex: number;
  receipt: string;
  rawAccountSha256: string;
  /** Fingerprint of the receipt-generation account, distinct from the scan aggregate fingerprint. */
  generationFingerprint: string;
}>;

/** Route-owned checkpoint used to attribute a restoration to one lifecycle. */
export type WithdrawalRestorationProtectedCheckpoint = Readonly<{
  addressSetSha256: string;
  stateSha256: string;
  contextSlot: number;
}>;

export type WithdrawalRestorationDurabilityContext = Readonly<{
  lifecycleId: string;
  routeAuthorizationSha256: string;
  requestOrigin: WithdrawalRestorationRequestOrigin;
  protectedCheckpoint: WithdrawalRestorationProtectedCheckpoint;
}>;

export type WithdrawalRestorationLeg = Readonly<{
  legId: string;
  strategyId: PartnerStrategyId;
  reserve: string;
  amountRaw: bigint;
  sourceAvailableRaw: bigint;
  netYieldLossBps: bigint;
  unwindCostLamports: bigint;
  sourceObservedContextSlot: number;
  positionFingerprint: string;
  managerRequest: LogicalManagerWithdraw;
}>;

export type WithdrawalRestorationPlan = Readonly<{
  schemaVersion: 1;
  routeId: string;
  routeSpecSha256: string;
  vault: string;
  generation: number;
  originId: string;
  origin: Readonly<{
    kind: "voltr-withdrawal-demand";
    scanGenerationFingerprint: string;
    observationContextSlot: number;
    receiptIds: readonly string[];
  }>;
  requestedRaw: bigint;
  plannedRaw: bigint;
  /** Null means this is an offline plan and cannot cross the store boundary. */
  durability: WithdrawalRestorationDurabilityContext | null;
  legs: readonly WithdrawalRestorationLeg[];
  outbox: Readonly<{
    idempotencyKey: string;
    eventType: "backyard_voltr_manager_withdraw";
    pendingLegIds: readonly string[];
  }>;
}>;

type JsonObject = Readonly<Record<string, unknown>>;

function object(value: unknown, label: string): JsonObject {
  if (value === null || typeof value !== "object" || Array.isArray(value)) throw new Error(`${label} must be an object`);
  return value as JsonObject;
}

function exactKeys(value: JsonObject, keys: readonly string[], label: string): void {
  const expected = new Set(keys);
  for (const key of Object.keys(value)) if (!expected.has(key)) throw new Error(`${label} contains unknown field ${key}`);
  for (const key of keys) if (!(key in value)) throw new Error(`${label} is missing ${key}`);
}

function stringField(value: JsonObject, key: string, label: string): string {
  if (typeof value[key] !== "string" || value[key].trim() === "") throw new Error(`${label}.${key} must be a non-empty string`);
  return value[key] as string;
}

function bigintField(value: JsonObject, key: string, label: string): bigint {
  const raw = value[key];
  if (typeof raw !== "string" || !/^(0|[1-9][0-9]*)$/.test(raw)) throw new Error(`${label}.${key} must be a canonical non-negative bigint string`);
  return BigInt(raw);
}

function numberField(value: JsonObject, key: string, label: string): number {
  const raw = value[key];
  if (typeof raw !== "number" || !Number.isSafeInteger(raw) || raw <= 0) throw new Error(`${label}.${key} must be a positive safe integer`);
  return raw;
}

function shaField(value: JsonObject, key: string, label: string): string {
  const raw = stringField(value, key, label);
  if (!/^[0-9a-f]{64}$/.test(raw)) throw new Error(`${label}.${key} must be a lowercase SHA-256`);
  return raw;
}

function canonicalJson(value: unknown): string {
  if (typeof value === "bigint") return JSON.stringify(value.toString());
  if (Array.isArray(value)) return `[${value.map(canonicalJson).join(",")}]`;
  if (value && typeof value === "object") return `{${Object.entries(value as JsonObject).sort(([left], [right]) => left.localeCompare(right)).map(([key, entry]) => `${JSON.stringify(key)}:${canonicalJson(entry)}`).join(",")}}`;
  return JSON.stringify(value);
}

function scanQueryProof(root: JsonObject): Readonly<{ rawQuerySha256: string; queryConfigSha256: string }> {
  const rawQuery = object(root.rawQuery, "withdrawal scan.rawQuery");
  exactKeys(rawQuery, ["method", "params"], "withdrawal scan.rawQuery");
  if (rawQuery.method !== "getProgramAccounts" || !Array.isArray(rawQuery.params) || rawQuery.params.length !== 2 || rawQuery.params[0] !== PARTNER_ROUTE.programs.voltrVault) throw new Error("withdrawal scan raw query is not the exact Voltr getProgramAccounts call");
  const config = object(rawQuery.params[1], "withdrawal scan.rawQuery.params[1]");
  const allowed = new Set(["commitment", "encoding", "withContext", "filters", "minContextSlot"]);
  if (Object.keys(config).some((key) => !allowed.has(key)) || config.commitment !== "confirmed" || config.encoding !== "base64" || config.withContext !== true) throw new Error("withdrawal scan query config is not exact confirmed/base64/withContext");
  if ("minContextSlot" in config && (typeof config.minContextSlot !== "number" || !Number.isSafeInteger(config.minContextSlot) || config.minContextSlot <= 0)) throw new Error("withdrawal scan minContextSlot is malformed");
  const discriminator = bs58.encode(Buffer.from(getRequestWithdrawVaultReceiptDiscriminatorBytes()));
  const expectedFilters = [{ memcmp: { offset: 0, bytes: discriminator } }, { memcmp: { offset: 8, bytes: PARTNER_ROUTE.vault } }];
  if (canonicalJson(config.filters) !== canonicalJson(expectedFilters)) throw new Error("withdrawal scan filters are not the exact receipt discriminator/vault filters");
  const rawQuerySha256 = shaField(root, "rawQuerySha256", "withdrawal scan");
  const queryConfigSha256 = shaField(root, "queryConfigSha256", "withdrawal scan");
  if (rawQuerySha256 !== sha256(canonicalJson(rawQuery)) || queryConfigSha256 !== sha256(canonicalJson(config))) throw new Error("withdrawal scan query hashes do not match canonical query bytes");
  return { rawQuerySha256, queryConfigSha256 };
}

/** Parse scanner output without coercing JSON numbers into bigint values. */
export function parseWithdrawalRestorationScanFile(path: string): WithdrawalRestorationScan {
  let parsed: unknown;
  try { parsed = JSON.parse(readFileSync(path, "utf8")); } catch (error) { throw new Error(`cannot read withdrawal scan ${path}: ${error instanceof Error ? error.message : String(error)}`); }
  const root = object(parsed, "withdrawal scan");
  exactKeys(root, ["verdict", "broadcast", "signerLoaded", "commitment", "routeId", "routeSpecSha256", "vault", "receiptProgram", "observationContextSlot", "receiptContextSlot", "idleContextSlot", "contextSlotsAligned", "rawQuery", "rawQuerySha256", "queryConfigSha256", "requestOrigin", "generationFingerprint", "receipts", "demand"], "withdrawal scan");
  if (root.verdict !== "PARTNER_WITHDRAWAL_DEMAND_SCAN_PASS" || root.broadcast !== false || root.signerLoaded !== false || root.commitment !== "confirmed" || root.contextSlotsAligned !== true) throw new Error("withdrawal scan is not a signer-free confirmed aligned pass");
  if (root.routeId !== PARTNER_FOUR_MARKET_ROUTE.id || root.routeSpecSha256 !== fourMarketRouteSpecSha256() || root.vault !== PARTNER_ROUTE.vault || root.receiptProgram !== PARTNER_ROUTE.programs.voltrVault) throw new Error("withdrawal scan is not bound to the exact four-market route");
  const observationContextSlot = numberField(root, "observationContextSlot", "withdrawal scan");
  if (numberField(root, "receiptContextSlot", "withdrawal scan") !== observationContextSlot || numberField(root, "idleContextSlot", "withdrawal scan") !== observationContextSlot) throw new Error("withdrawal scan account contexts are not aligned");
  const generationFingerprint = stringField(root, "generationFingerprint", "withdrawal scan");
  const queryProof = scanQueryProof(root);
  const originRoot = object(root.requestOrigin, "withdrawal scan.requestOrigin");
  exactKeys(originRoot, ["signature", "eventIndex", "receipt", "rawAccountSha256", "generationFingerprint"], "withdrawal scan.requestOrigin");
  const eventIndex = originRoot.eventIndex;
  if (typeof eventIndex !== "number" || !Number.isSafeInteger(eventIndex) || eventIndex < 0) throw new Error("withdrawal scan.requestOrigin.eventIndex must be a non-negative safe integer");
  const requestOrigin: WithdrawalRestorationRequestOrigin = {
    signature: stringField(originRoot, "signature", "withdrawal scan.requestOrigin"),
    eventIndex,
    receipt: stringField(originRoot, "receipt", "withdrawal scan.requestOrigin"),
    rawAccountSha256: shaField(originRoot, "rawAccountSha256", "withdrawal scan.requestOrigin"),
    generationFingerprint: shaField(originRoot, "generationFingerprint", "withdrawal scan.requestOrigin"),
  };
  if (!Array.isArray(root.receipts)) throw new Error("withdrawal scan.receipts must be an array");
  const receipts = root.receipts.map((raw, index) => {
    const row = object(raw, `withdrawal scan.receipts[${index}]`);
    exactKeys(row, ["receipt", "owner", "lamports", "dataBase64", "dataSha256", "vault", "user", "amountLpEscrowed", "amountAssetToWithdrawDecimalBits", "upperBoundAssetRaw", "withdrawableFromTs", "bump", "version", "observedContextSlot", "generationFingerprint"], `withdrawal scan.receipts[${index}]`);
    if (row.vault !== PARTNER_ROUTE.vault || numberField(row, "observedContextSlot", `withdrawal scan.receipts[${index}]`) < observationContextSlot) throw new Error(`withdrawal scan receipt ${index} is not bound to its observation slot`);
    const receipt = stringField(row, "receipt", `withdrawal scan.receipts[${index}]`);
    const dataBase64 = stringField(row, "dataBase64", `withdrawal scan.receipts[${index}]`);
    const data = Buffer.from(dataBase64, "base64");
    if (data.toString("base64") !== dataBase64 || sha256(data) !== shaField(row, "dataSha256", `withdrawal scan.receipts[${index}]`) || row.owner !== PARTNER_ROUTE.programs.voltrVault || typeof row.lamports !== "number" || !Number.isSafeInteger(row.lamports) || row.lamports <= 0) throw new Error(`withdrawal scan receipt ${index} raw account envelope is not canonical`);
    const user = stringField(row, "user", `withdrawal scan.receipts[${index}]`);
    return { receipt, user, upperBoundAssetRaw: bigintField(row, "upperBoundAssetRaw", `withdrawal scan.receipts[${index}]`), generationFingerprint: stringField(row, "generationFingerprint", `withdrawal scan.receipts[${index}]`) };
  });
  const demand = object(root.demand, "withdrawal scan.demand");
  exactKeys(demand, ["configuredIdleFloorRaw", "confirmedIdleRaw", "pendingWithdrawalUpperBoundRaw", "requiredIdleRaw", "idleShortfallRaw", "rounding"], "withdrawal scan.demand");
  if (typeof demand.rounding !== "string") throw new Error("withdrawal scan.demand.rounding must be present");
  const parsedDemand = {
    configuredIdleFloorRaw: bigintField(demand, "configuredIdleFloorRaw", "withdrawal scan.demand"),
    confirmedIdleRaw: bigintField(demand, "confirmedIdleRaw", "withdrawal scan.demand"),
    pendingWithdrawalUpperBoundRaw: bigintField(demand, "pendingWithdrawalUpperBoundRaw", "withdrawal scan.demand"),
    requiredIdleRaw: bigintField(demand, "requiredIdleRaw", "withdrawal scan.demand"),
    idleShortfallRaw: bigintField(demand, "idleShortfallRaw", "withdrawal scan.demand"),
  };
  if (receipts.reduce((sum, row) => sum + row.upperBoundAssetRaw, 0n) !== parsedDemand.pendingWithdrawalUpperBoundRaw) throw new Error("withdrawal scan demand does not equal receipt rows");
  const originReceipt = root.receipts.find((raw) => object(raw, "withdrawal scan receipt").receipt === requestOrigin.receipt);
  if (!originReceipt) throw new Error("withdrawal scan request origin is not an active receipt");
  const originRow = object(originReceipt, "withdrawal scan request receipt");
  const expectedOriginFingerprint = sha256(canonicalJson({ signature: requestOrigin.signature, eventIndex: requestOrigin.eventIndex, receipt: requestOrigin.receipt, rawAccountSha256: requestOrigin.rawAccountSha256 }));
  if (shaField(originRow, "dataSha256", "withdrawal scan request receipt") !== requestOrigin.rawAccountSha256 || requestOrigin.generationFingerprint !== expectedOriginFingerprint) throw new Error("withdrawal scan request origin does not match the exact transaction/receipt generation");
  return { verdict: "PARTNER_WITHDRAWAL_DEMAND_SCAN_PASS", routeId: root.routeId as string, routeSpecSha256: root.routeSpecSha256 as string, vault: root.vault as string, observationContextSlot, generationFingerprint, rawQuerySha256: queryProof.rawQuerySha256, queryConfigSha256: queryProof.queryConfigSha256, requestOrigin, receipts, demand: parsedDemand };
}

/** Strictly parse the deterministic plan retained by the maintained CLI. */
export function parseWithdrawalRestorationPlanFile(path: string): WithdrawalRestorationPlan {
  let parsed: unknown;
  try { parsed = JSON.parse(readFileSync(path, "utf8")); } catch (error) { throw new Error(`cannot read withdrawal restoration plan ${path}: ${error instanceof Error ? error.message : String(error)}`); }
  const envelope = object(parsed, "withdrawal restoration plan envelope");
  const root = "plan" in envelope ? object(envelope.plan, "withdrawal restoration plan") : envelope;
  exactKeys(root, ["schemaVersion", "routeId", "routeSpecSha256", "vault", "generation", "originId", "origin", "requestedRaw", "plannedRaw", "durability", "legs", "outbox"], "withdrawal restoration plan");
  if (root.schemaVersion !== 1 || root.routeId !== PARTNER_FOUR_MARKET_ROUTE.id || root.routeSpecSha256 !== fourMarketRouteSpecSha256() || root.vault !== PARTNER_ROUTE.vault) throw new Error("withdrawal restoration plan is not bound to the exact four-market route");
  const generation = numberField(root, "generation", "withdrawal restoration plan");
  const originId = shaField(root, "originId", "withdrawal restoration plan");
  const origin = object(root.origin, "withdrawal restoration plan.origin");
  exactKeys(origin, ["kind", "scanGenerationFingerprint", "observationContextSlot", "receiptIds"], "withdrawal restoration plan.origin");
  if (origin.kind !== "voltr-withdrawal-demand" || !Array.isArray(origin.receiptIds) || origin.receiptIds.length === 0 || origin.receiptIds.some((receipt) => typeof receipt !== "string" || receipt.length === 0)) throw new Error("withdrawal restoration plan origin is malformed");
  const parsedOrigin = { kind: "voltr-withdrawal-demand" as const, scanGenerationFingerprint: shaField(origin, "scanGenerationFingerprint", "withdrawal restoration plan.origin"), observationContextSlot: numberField(origin, "observationContextSlot", "withdrawal restoration plan.origin"), receiptIds: [...origin.receiptIds] as string[] };
  const durabilityRoot = object(root.durability, "withdrawal restoration plan.durability");
  exactKeys(durabilityRoot, ["lifecycleId", "routeAuthorizationSha256", "requestOrigin", "protectedCheckpoint"], "withdrawal restoration plan.durability");
  const requestOriginRoot = object(durabilityRoot.requestOrigin, "withdrawal restoration plan.durability.requestOrigin");
  exactKeys(requestOriginRoot, ["signature", "eventIndex", "receipt", "rawAccountSha256", "generationFingerprint"], "withdrawal restoration plan.durability.requestOrigin");
  const eventIndex = requestOriginRoot.eventIndex;
  if (typeof eventIndex !== "number" || !Number.isSafeInteger(eventIndex) || eventIndex < 0) throw new Error("withdrawal restoration plan request event index is malformed");
  const requestOrigin: WithdrawalRestorationRequestOrigin = { signature: stringField(requestOriginRoot, "signature", "withdrawal restoration plan.durability.requestOrigin"), eventIndex, receipt: stringField(requestOriginRoot, "receipt", "withdrawal restoration plan.durability.requestOrigin"), rawAccountSha256: shaField(requestOriginRoot, "rawAccountSha256", "withdrawal restoration plan.durability.requestOrigin"), generationFingerprint: shaField(requestOriginRoot, "generationFingerprint", "withdrawal restoration plan.durability.requestOrigin") };
  const checkpoint = object(durabilityRoot.protectedCheckpoint, "withdrawal restoration plan.durability.protectedCheckpoint");
  exactKeys(checkpoint, ["addressSetSha256", "stateSha256", "contextSlot"], "withdrawal restoration plan.durability.protectedCheckpoint");
  const durability: WithdrawalRestorationDurabilityContext = { lifecycleId: shaField(durabilityRoot, "lifecycleId", "withdrawal restoration plan.durability"), routeAuthorizationSha256: shaField(durabilityRoot, "routeAuthorizationSha256", "withdrawal restoration plan.durability"), requestOrigin, protectedCheckpoint: { addressSetSha256: shaField(checkpoint, "addressSetSha256", "withdrawal restoration plan.durability.protectedCheckpoint"), stateSha256: shaField(checkpoint, "stateSha256", "withdrawal restoration plan.durability.protectedCheckpoint"), contextSlot: numberField(checkpoint, "contextSlot", "withdrawal restoration plan.durability.protectedCheckpoint") } };
  if (!Array.isArray(root.legs) || root.legs.length === 0) throw new Error("withdrawal restoration plan must contain at least one leg");
  const legs = root.legs.map((raw, index): WithdrawalRestorationLeg => {
    const row = object(raw, `withdrawal restoration plan.legs[${index}]`);
    exactKeys(row, ["legId", "strategyId", "reserve", "amountRaw", "sourceAvailableRaw", "netYieldLossBps", "unwindCostLamports", "sourceObservedContextSlot", "positionFingerprint", "managerRequest"], `withdrawal restoration plan.legs[${index}]`);
    const strategyId = stringField(row, "strategyId", `withdrawal restoration plan.legs[${index}]`) as PartnerStrategyId;
    const identity = PARTNER_FOUR_MARKET_STRATEGIES.find(({ id }) => id === strategyId);
    if (!identity || row.reserve !== identity.reserve) throw new Error(`withdrawal restoration plan leg ${index} is not an approved strategy/reserve`);
    const legId = shaField(row, "legId", `withdrawal restoration plan.legs[${index}]`);
    const amountRaw = bigintField(row, "amountRaw", `withdrawal restoration plan.legs[${index}]`);
    const managerRequest = object(row.managerRequest, `withdrawal restoration plan.legs[${index}].managerRequest`);
    exactKeys(managerRequest, ["strategyId", "reserve", "amountRaw", "operation", "originId"], `withdrawal restoration plan.legs[${index}].managerRequest`);
    if (managerRequest.strategyId !== strategyId || managerRequest.reserve !== identity.reserve || bigintField(managerRequest, "amountRaw", `withdrawal restoration plan.legs[${index}].managerRequest`) !== amountRaw || managerRequest.operation !== "manager-withdraw" || managerRequest.originId !== originId) throw new Error(`withdrawal restoration plan leg ${index} manager request differs from its logical leg`);
    return { legId, strategyId, reserve: identity.reserve, amountRaw, sourceAvailableRaw: bigintField(row, "sourceAvailableRaw", `withdrawal restoration plan.legs[${index}]`), netYieldLossBps: bigintField(row, "netYieldLossBps", `withdrawal restoration plan.legs[${index}]`), unwindCostLamports: bigintField(row, "unwindCostLamports", `withdrawal restoration plan.legs[${index}]`), sourceObservedContextSlot: numberField(row, "sourceObservedContextSlot", `withdrawal restoration plan.legs[${index}]`), positionFingerprint: shaField(row, "positionFingerprint", `withdrawal restoration plan.legs[${index}]`), managerRequest: { strategyId, reserve: identity.reserve, amountRaw, operation: "manager-withdraw", originId } };
  });
  const outbox = object(root.outbox, "withdrawal restoration plan.outbox");
  exactKeys(outbox, ["idempotencyKey", "eventType", "pendingLegIds"], "withdrawal restoration plan.outbox");
  if (outbox.idempotencyKey !== `backyard-voltr:${originId}:${generation}` || outbox.eventType !== "backyard_voltr_manager_withdraw" || !Array.isArray(outbox.pendingLegIds) || JSON.stringify(outbox.pendingLegIds) !== JSON.stringify(legs.map(({ legId }) => legId))) throw new Error("withdrawal restoration plan outbox identity differs from its legs");
  return { schemaVersion: 1, routeId: PARTNER_FOUR_MARKET_ROUTE.id, routeSpecSha256: fourMarketRouteSpecSha256(), vault: PARTNER_ROUTE.vault, generation, originId, origin: parsedOrigin, requestedRaw: bigintField(root, "requestedRaw", "withdrawal restoration plan"), plannedRaw: bigintField(root, "plannedRaw", "withdrawal restoration plan"), durability, legs, outbox: { idempotencyKey: outbox.idempotencyKey as string, eventType: "backyard_voltr_manager_withdraw", pendingLegIds: [...outbox.pendingLegIds] as string[] } };
}

/** Strictly parse position evidence emitted by loadFourMarketRestorationSources. */
export function parseFourMarketPositionEvidenceFile(path: string, minimumContextSlot: number): readonly WithdrawalRestorationSource[] {
  let parsed: unknown;
  try { parsed = JSON.parse(readFileSync(path, "utf8")); } catch (error) { throw new Error(`cannot read position evidence ${path}: ${error instanceof Error ? error.message : String(error)}`); }
  const root = object(parsed, "position evidence");
  exactKeys(root, ["verdict", "broadcast", "signerLoaded", "commitment", "routeId", "routeSpecSha256", "vault", "observationContextSlot", "minimumContextSlot", "sources"], "position evidence");
  if (root.verdict !== "PARTNER_FOUR_MARKET_POSITION_EVIDENCE_PASS" || root.broadcast !== false || root.signerLoaded !== false || root.commitment !== "confirmed" || root.routeId !== PARTNER_FOUR_MARKET_ROUTE.id || root.routeSpecSha256 !== fourMarketRouteSpecSha256() || root.vault !== PARTNER_ROUTE.vault) throw new Error("position evidence is not an exact confirmed four-market pass");
  const observed = numberField(root, "observationContextSlot", "position evidence");
  if (observed < minimumContextSlot || numberField(root, "minimumContextSlot", "position evidence") !== minimumContextSlot) throw new Error("position evidence predates the withdrawal scan");
  if (!Array.isArray(root.sources) || root.sources.length !== PARTNER_FOUR_MARKET_STRATEGIES.length) throw new Error("position evidence must contain exactly four sources");
  const expected = PARTNER_FOUR_MARKET_STRATEGIES;
  return root.sources.map((raw, index) => {
    const row = object(raw, `position evidence.sources[${index}]`);
    exactKeys(row, ["strategyId", "reserve", "availableRaw", "netYieldLossBps", "unwindCostLamports", "observedContextSlot", "positionFingerprint", "strategyReceipt", "strategyAssetAta"], `position evidence.sources[${index}]`);
    const identity = expected[index]!;
    if (row.strategyId !== identity.id || row.reserve !== identity.reserve || row.strategyReceipt !== identity.voltr.strategyInitReceipt || row.strategyAssetAta !== identity.voltr.strategyAssetAta) throw new Error(`position evidence source ${index} does not match the frozen ${identity.id} identity`);
    const slot = numberField(row, "observedContextSlot", `position evidence.sources[${index}]`);
    if (slot < minimumContextSlot) throw new Error(`position evidence source ${index} predates the withdrawal scan`);
    return { strategyId: identity.id, reserve: identity.reserve, availableRaw: bigintField(row, "availableRaw", `position evidence.sources[${index}]`), netYieldLossBps: bigintField(row, "netYieldLossBps", `position evidence.sources[${index}]`), unwindCostLamports: bigintField(row, "unwindCostLamports", `position evidence.sources[${index}]`), observedContextSlot: slot, positionFingerprint: stringField(row, "positionFingerprint", `position evidence.sources[${index}]`) };
  });
}

export function restorationPlanAsOutboxInput(plan: WithdrawalRestorationPlan, cluster: string): Readonly<Record<string, unknown>> {
  const durability = plan.durability;
  if (!durability) throw new Error("outbox input requires lifecycle, route authorization, request origin, and protected checkpoint bindings");
  return { cluster, vault: plan.vault, routeId: plan.routeId, routeSpecSha256: plan.routeSpecSha256, routeAuthorizationSha256: durability.routeAuthorizationSha256, lifecycleId: durability.lifecycleId, requestOrigin: durability.requestOrigin, protectedCheckpoint: durability.protectedCheckpoint, originId: plan.originId, generation: plan.generation, scanGenerationFingerprint: plan.origin.scanGenerationFingerprint, observationContextSlot: plan.origin.observationContextSlot, requestedRaw: plan.requestedRaw, legs: plan.legs.map(({ legId, strategyId, reserve, amountRaw, sourceAvailableRaw, sourceObservedContextSlot, positionFingerprint }) => ({ legId, strategyId, reserve, amountRaw, sourceAvailableRaw, sourceObservedContextSlot, positionFingerprint })) };
}

/**
 * The existing Earn/Neon implementation owns SQL transactions, leases, and
 * fencing. This adapter is intentionally an interface, rather than a second
 * database or scheduler. `upsertPlan` must atomically get-or-create by
 * originId/generation and enqueue each manager request in the existing
 * orchestration outbox. Replaying the same scan must return duplicate=true
 * and must not append another movement or outbox event.
 */
export interface WithdrawalRestorationPersistence {
  upsertPlan(input: Readonly<{
    plan: WithdrawalRestorationPlan;
    duplicateOfOriginId?: string;
  }>): Promise<Readonly<{ plan: WithdrawalRestorationPlan; duplicate: boolean }> | Readonly<{ duplicate: true; plan: WithdrawalRestorationPlan }>>;
}

const MAX_RESTORATION_RAW = PARTNER_ROUTE.asset.maxManagerOperationRaw;

function sha256(value: string | ArrayLike<number>): string {
  return createHash("sha256")
    .update(typeof value === "string" ? value : Uint8Array.from(value))
    .digest("hex");
}

function canonicalOrigin(scan: WithdrawalRestorationScan): string {
  return JSON.stringify({
    kind: "voltr-withdrawal-demand",
    routeId: scan.routeId,
    routeSpecSha256: scan.routeSpecSha256,
    vault: scan.vault,
    generationFingerprint: scan.generationFingerprint,
    observationContextSlot: scan.observationContextSlot,
    receipts: scan.receipts
      .map(({ receipt, user, upperBoundAssetRaw, generationFingerprint }) => ({
        receipt,
        user,
        upperBoundAssetRaw: upperBoundAssetRaw.toString(),
        generationFingerprint,
      }))
      .slice()
      .sort((left, right) => left.receipt.localeCompare(right.receipt)),
  });
}

function assertScan(scan: WithdrawalRestorationScan): void {
  if (scan.verdict !== "PARTNER_WITHDRAWAL_DEMAND_SCAN_PASS") throw new Error("restoration requires a passing confirmed withdrawal scan");
  if (scan.routeId !== PARTNER_FOUR_MARKET_ROUTE.id || scan.routeSpecSha256 !== fourMarketRouteSpecSha256()) throw new Error("restoration scan is not bound to the four-market route");
  if (scan.vault !== PARTNER_ROUTE.vault || !Number.isSafeInteger(scan.observationContextSlot) || scan.observationContextSlot <= 0) throw new Error("restoration scan vault or context slot is not exact");
  if (scan.generationFingerprint.trim() === "") throw new Error("restoration scan has no generation fingerprint");
  if (scan.demand.idleShortfallRaw <= 0n) throw new Error("restoration is only created for a positive idle shortfall");
  if (scan.demand.pendingWithdrawalUpperBoundRaw < 0n || scan.demand.requiredIdleRaw !== scan.demand.configuredIdleFloorRaw + scan.demand.pendingWithdrawalUpperBoundRaw) throw new Error("restoration scan demand is inconsistent");
  const expectedShortfall = scan.demand.requiredIdleRaw > scan.demand.confirmedIdleRaw ? scan.demand.requiredIdleRaw - scan.demand.confirmedIdleRaw : 0n;
  if (scan.demand.idleShortfallRaw !== expectedShortfall) throw new Error("restoration scan shortfall is not recomputed from the confirmed idle balance");
  const receiptDemand = scan.receipts.reduce((sum, receipt) => sum + receipt.upperBoundAssetRaw, 0n);
  if (receiptDemand !== scan.demand.pendingWithdrawalUpperBoundRaw) throw new Error("restoration scan receipt demand does not match its aggregate demand");
  const receipts = new Set<string>();
  for (const receipt of scan.receipts) {
    if (receipts.has(receipt.receipt) || receipt.upperBoundAssetRaw <= 0n || receipt.generationFingerprint.trim() === "") throw new Error("restoration scan contains a duplicate or invalid receipt");
    receipts.add(receipt.receipt);
  }
}

function compareSources(left: WithdrawalRestorationSource, right: WithdrawalRestorationSource): number {
  const compareBigint = (a: bigint, b: bigint): number => a < b ? -1 : a > b ? 1 : 0;
  return compareBigint(left.netYieldLossBps, right.netYieldLossBps)
    || compareBigint(left.unwindCostLamports, right.unwindCostLamports)
    || (left.availableRaw > right.availableRaw ? -1 : left.availableRaw < right.availableRaw ? 1 : 0)
    || left.strategyId.localeCompare(right.strategyId)
    || left.reserve.localeCompare(right.reserve);
}

function assertSource(source: WithdrawalRestorationSource): void {
  const strategy = PARTNER_FOUR_MARKET_ROUTE.strategies.find(({ id }) => id === source.strategyId);
  if (!strategy || strategy.reserve !== source.reserve) throw new Error(`restoration source ${source.strategyId} is not an approved reserve`);
  if (source.availableRaw < 0n || source.netYieldLossBps < 0n || source.unwindCostLamports < 0n) throw new Error(`restoration source ${source.strategyId} has a negative bound`);
  if (!Number.isSafeInteger(source.observedContextSlot) || source.observedContextSlot <= 0 || source.positionFingerprint.trim() === "") throw new Error(`restoration source ${source.strategyId} lacks fresh position identity`);
}

function assertDurabilityContext(
  scan: WithdrawalRestorationScan,
  durability: WithdrawalRestorationDurabilityContext,
): void {
  const sha = (value: string, label: string) => {
    if (!/^[0-9a-f]{64}$/.test(value)) throw new Error(`${label} must be a lowercase SHA-256 digest`);
  };
  sha(durability.lifecycleId, "restoration lifecycleId");
  sha(durability.routeAuthorizationSha256, "restoration routeAuthorizationSha256");
  sha(durability.requestOrigin.rawAccountSha256, "restoration requestOrigin.rawAccountSha256");
  sha(durability.requestOrigin.generationFingerprint, "restoration requestOrigin.generationFingerprint");
  sha(durability.protectedCheckpoint.addressSetSha256, "restoration protectedCheckpoint.addressSetSha256");
  sha(durability.protectedCheckpoint.stateSha256, "restoration protectedCheckpoint.stateSha256");
  if (durability.requestOrigin.signature.trim() === "" || durability.requestOrigin.receipt.trim() === "" || !Number.isSafeInteger(durability.requestOrigin.eventIndex) || durability.requestOrigin.eventIndex < 0) throw new Error("restoration request origin tuple is malformed");
  if (!Number.isSafeInteger(durability.protectedCheckpoint.contextSlot) || durability.protectedCheckpoint.contextSlot <= 0 || durability.protectedCheckpoint.contextSlot > scan.observationContextSlot) throw new Error("restoration protected checkpoint must be the positive request poststate at or before the confirmed scan");
}

/**
 * Selects the smallest deterministic set of approved reserve withdrawals that
 * restores the requested idle amount. It never builds a Kamino instruction;
 * each result is a logical manager-withdraw request for the existing manager
 * executor/policy path.
 */
export function planWithdrawalRestoration(
  scan: WithdrawalRestorationScan,
  sources: readonly WithdrawalRestorationSource[],
  generation: number,
  durability: WithdrawalRestorationDurabilityContext | null = null,
): WithdrawalRestorationPlan {
  assertScan(scan);
  if (!Number.isSafeInteger(generation) || generation <= 0) throw new Error("restoration generation must be a positive safe integer");
  if (durability) assertDurabilityContext(scan, durability);
  const unique = new Set<string>();
  for (const source of sources) {
    assertSource(source);
    if (source.observedContextSlot < scan.observationContextSlot) throw new Error(`restoration source ${source.strategyId} is older than the withdrawal-demand snapshot`);
    if (unique.has(source.strategyId)) throw new Error(`restoration contains duplicate strategy ${source.strategyId}`);
    unique.add(source.strategyId);
  }
  const requestedRaw = scan.demand.idleShortfallRaw;
  let remaining = requestedRaw;
  const originId = sha256(canonicalOrigin(scan));
  const legs: WithdrawalRestorationLeg[] = [];
  for (const source of sources.slice().sort(compareSources)) {
    let sourceRemaining = source.availableRaw;
    let chunk = 0;
    while (remaining > 0n && sourceRemaining > 0n) {
      const amountRaw = [remaining, sourceRemaining, MAX_RESTORATION_RAW].reduce((minimum, value) => value < minimum ? value : minimum);
      if (amountRaw <= 0n) break;
      const legId = sha256(JSON.stringify({ originId, generation, chunk, strategyId: source.strategyId, reserve: source.reserve, amountRaw: amountRaw.toString(), positionFingerprint: source.positionFingerprint }));
      legs.push({
        legId,
        strategyId: source.strategyId,
        reserve: source.reserve,
        amountRaw,
        sourceAvailableRaw: source.availableRaw,
        netYieldLossBps: source.netYieldLossBps,
        unwindCostLamports: source.unwindCostLamports,
        sourceObservedContextSlot: source.observedContextSlot,
        positionFingerprint: source.positionFingerprint,
        managerRequest: { strategyId: source.strategyId, reserve: source.reserve, amountRaw, operation: "manager-withdraw", originId },
      });
      remaining -= amountRaw;
      sourceRemaining -= amountRaw;
      chunk += 1;
    }
  }
  if (remaining !== 0n) throw new Error(`approved reserve liquidity cannot restore the full idle shortfall; missing ${remaining} raw USDC`);
  const plannedRaw = requestedRaw - remaining;
  const pendingLegIds = legs.map(({ legId }) => legId);
  const outbox = {
    idempotencyKey: `backyard-voltr:${originId}:${generation}`,
    eventType: "backyard_voltr_manager_withdraw" as const,
    pendingLegIds,
  };
  return {
    schemaVersion: 1,
    routeId: scan.routeId,
    routeSpecSha256: scan.routeSpecSha256,
    vault: scan.vault,
    generation,
    originId,
    origin: {
      kind: "voltr-withdrawal-demand",
      scanGenerationFingerprint: scan.generationFingerprint,
      observationContextSlot: scan.observationContextSlot,
      receiptIds: scan.receipts.map(({ receipt }) => receipt).slice().sort(),
    },
    requestedRaw,
    plannedRaw,
    durability,
    legs,
    outbox,
  };
}

/** Persist through the existing Earn store/outbox; no local scheduler or DB is permitted. */
export async function persistWithdrawalRestoration(
  persistence: WithdrawalRestorationPersistence,
  plan: WithdrawalRestorationPlan,
): Promise<Readonly<{ plan: WithdrawalRestorationPlan; duplicate: boolean }>> {
  if (plan.routeId !== PARTNER_FOUR_MARKET_ROUTE.id || plan.routeSpecSha256 !== fourMarketRouteSpecSha256() || plan.vault !== PARTNER_ROUTE.vault) throw new Error("refusing to persist a restoration plan for a different route");
  if (!plan.durability) throw new Error("refusing to persist an offline restoration plan without durable lifecycle bindings");
  if (plan.legs.length === 0 || plan.plannedRaw !== plan.requestedRaw || plan.outbox.pendingLegIds.length !== plan.legs.length) throw new Error("restoration plan is incomplete");
  return persistence.upsertPlan({ plan });
}

/** Narrow offline verifier used by the partner proof; it exercises fail-closed invariants without RPC or secrets. */
export function verifyWithdrawalRestorationPlanner(): Readonly<{ verdict: "BACKYARD_VOLTR_WITHDRAWAL_RESTORATION_PLANNER_PASS" | "BACKYARD_VOLTR_WITHDRAWAL_RESTORATION_PLANNER_FAIL"; failedGateCount: number; gates: readonly Readonly<{ name: string; pass: boolean }>[] }> {
  const scan: WithdrawalRestorationScan = {
    verdict: "PARTNER_WITHDRAWAL_DEMAND_SCAN_PASS",
    routeId: PARTNER_FOUR_MARKET_ROUTE.id,
    routeSpecSha256: fourMarketRouteSpecSha256(),
    vault: PARTNER_ROUTE.vault,
    observationContextSlot: 440623056,
    generationFingerprint: "confirmed-scan-fixture",
    rawQuerySha256: "1".repeat(64),
    queryConfigSha256: "2".repeat(64),
    requestOrigin: {
      signature: "request-signature-fixture",
      eventIndex: 0,
      receipt: "receipt-fixture",
      rawAccountSha256: "3".repeat(64),
      generationFingerprint: "4".repeat(64),
    },
    receipts: [{ receipt: "receipt-fixture", user: PARTNER_ROUTE.setupAdmin, upperBoundAssetRaw: 700_000n, generationFingerprint: "receipt-fixture" }],
    demand: { configuredIdleFloorRaw: 0n, confirmedIdleRaw: 0n, pendingWithdrawalUpperBoundRaw: 700_000n, requiredIdleRaw: 700_000n, idleShortfallRaw: 700_000n },
  };
  const sources: readonly WithdrawalRestorationSource[] = [
    { strategyId: "prime", reserve: "9GJ9GBRwCp4pHmWrQ43L5xpc9Vykg7jnfwcFGN8FoHYu", availableRaw: 500_000n, netYieldLossBps: 30n, unwindCostLamports: 20_000n, observedContextSlot: 440623056, positionFingerprint: "prime-position" },
    { strategyId: "main", reserve: "D6q6wuQSrifJKZYpR1M8R4YawnLDtDsMmWM1NbBmgJ59", availableRaw: 500_000n, netYieldLossBps: 10n, unwindCostLamports: 50_000n, observedContextSlot: 440623056, positionFingerprint: "main-position" },
  ];
  const plan = planWithdrawalRestoration(scan, sources, 1);
  const gates = [
    { name: "selects lowest yield-loss source first", pass: plan.legs[0]?.strategyId === "main" },
    { name: "restores exact shortfall", pass: plan.plannedRaw === 700_000n && plan.legs.reduce((sum, leg) => sum + leg.amountRaw, 0n) === 700_000n },
    { name: "manager-only logical requests with no direct Kamino executor", pass: plan.legs.every(({ managerRequest }) => managerRequest.operation === "manager-withdraw" && JSON.stringify(Object.keys(managerRequest).sort()) === JSON.stringify(["amountRaw", "operation", "originId", "reserve", "strategyId"]) && !("instructions" in managerRequest) && !("programId" in managerRequest)) },
    { name: "each manager leg respects cap", pass: plan.legs.every(({ amountRaw }) => amountRaw <= MAX_RESTORATION_RAW) },
    { name: "origin and outbox are bound", pass: plan.originId.length === 64 && plan.outbox.idempotencyKey.includes(plan.originId) },
    { name: "large shortfalls are chunked", pass: (() => {
      const largeScan: WithdrawalRestorationScan = {
        ...scan,
        demand: { ...scan.demand, pendingWithdrawalUpperBoundRaw: 2_100_000n, requiredIdleRaw: 2_100_000n, idleShortfallRaw: 2_100_000n },
        receipts: [{ receipt: scan.receipts[0]!.receipt, user: scan.receipts[0]!.user, upperBoundAssetRaw: 2_100_000n, generationFingerprint: scan.receipts[0]!.generationFingerprint }],
      };
      const largePlan = planWithdrawalRestoration(largeScan, [{
        strategyId: sources[0]!.strategyId,
        reserve: sources[0]!.reserve,
        availableRaw: 2_100_000n,
        netYieldLossBps: sources[0]!.netYieldLossBps,
        unwindCostLamports: sources[0]!.unwindCostLamports,
        observedContextSlot: sources[0]!.observedContextSlot,
        positionFingerprint: sources[0]!.positionFingerprint,
      }], 2);
      return largePlan.legs.length === 3 && largePlan.legs.every(({ amountRaw }) => amountRaw <= MAX_RESTORATION_RAW);
    })() },
  ] as const;
  const failedGateCount = gates.filter(({ pass }) => !pass).length;
  return { verdict: failedGateCount === 0 ? "BACKYARD_VOLTR_WITHDRAWAL_RESTORATION_PLANNER_PASS" : "BACKYARD_VOLTR_WITHDRAWAL_RESTORATION_PLANNER_FAIL", failedGateCount, gates };
}
