import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

import type { PartnerStrategyId } from "./route-spec.js";

/**
 * A semantic bootstrap authorization.  It deliberately does not contain a
 * blockhash, signature, or packet bytes: those are rebuilt and checked by the
 * strategy/ATA preparation code immediately before the one send.  It does
 * contain every value that is allowed to survive a blockhash refresh.
 *
 * This is intentionally separate from the compatibility approval.  The latter
 * proves that the four-market graph is usable; this envelope is the explicit
 * authorization to mutate six exact accounts using the current source.
 */
export type BootstrapExecutionOperation = Readonly<{
  operation: "initialize-strategy" | "initialize-strategy-asset-ata";
  strategyId: Exclude<PartnerStrategyId, "main">;
  reserve: string;
  vault: string;
  setupAdmin: string;
  strategyAuth: string;
  strategyInitReceipt: string;
  strategyAssetAta: string;
  fourMarketRouteSpecSha256: string;
  strategyGraphSha256: string;
  builderRouteSpecSha256: string;
  instructionDataSha256: Readonly<Record<string, string>>;
  maxTotalLamports: string;
}>;

export type BootstrapExecutionAuthorization = Readonly<{
  schemaVersion: 1;
  evidenceType: "backyard-voltr-four-market-bootstrap-execution-authorization";
  approvalId: string;
  expiresAtUnix: string;
  routeId: string;
  cluster: "mainnet-beta";
  genesisHash: string;
  compatibilityApproval: Readonly<{
    path: string;
    fileSha256: string;
    artifactPath: string;
    artifactFileSha256: string;
  }>;
  sourceBinding: Readonly<{
    algorithm: "sha256";
    files: readonly Readonly<{ path: string; sha256: string }>[];
    aggregateSha256: string;
  }>;
  operations: readonly BootstrapExecutionOperation[];
}>;

export const BOOTSTRAP_EXECUTION_AUTHORIZATION_PATH =
  "docs/evidence/backyard-voltr-four-market/bootstrap-execution-authorization-v1.json";
export const COMPATIBILITY_APPROVAL_PATH =
  "docs/evidence/backyard-voltr-four-market/compatibility-verifier-approval-v1.json";
export const COMPATIBILITY_ARTIFACT_PATH =
  "docs/evidence/backyard-voltr-four-market/compatibility-v1.json";
export const BOOTSTRAP_EXECUTION_SOURCE_PATHS = [
  "tools/backyard-voltr/bun.lock",
  "tools/backyard-voltr/package.json",
  "tools/backyard-voltr/src/bootstrap/authorization.ts",
  "tools/backyard-voltr/src/bootstrap/strategy.ts",
  "tools/backyard-voltr/src/bootstrap/strategy-asset.ts",
  "tools/backyard-voltr/src/cli.ts",
  "tools/backyard-voltr/src/domain/bootstrap-execution-authorization.ts",
  "tools/backyard-voltr/src/domain/execution-intent.ts",
  "tools/backyard-voltr/src/domain/route-spec.ts",
  "tools/backyard-voltr/src/integrations/signer.ts",
  "tools/backyard-voltr/src/integrations/solana-compat.ts",
  "tools/backyard-voltr/src/integrations/voltr.ts",
  "tools/backyard-voltr/src/verify/current.ts",
  "tools/backyard-voltr/tsconfig.json",
] as const;

const REPOSITORY_ROOT = resolve(dirname(fileURLToPath(import.meta.url)), "../../../..");

type JsonRecord = Record<string, unknown>;

function canonicalJson(value: unknown): string {
  if (Array.isArray(value)) return `[${value.map(canonicalJson).join(",")}]`;
  if (value && typeof value === "object") {
    return `{${Object.entries(value as JsonRecord)
      .sort(([left], [right]) => left.localeCompare(right))
      .map(([key, entry]) => `${JSON.stringify(key)}:${canonicalJson(entry)}`)
      .join(",")}}`;
  }
  return JSON.stringify(value);
}

function canonicalFileText(value: unknown): string {
  return `${JSON.stringify(value, null, 2)}\n`;
}

function sha256(bytes: Uint8Array): string {
  return createHash("sha256").update(bytes).digest("hex");
}

function exactKeys(value: JsonRecord, expected: readonly string[], label: string): void {
  const actual = Object.keys(value).sort().join("\0");
  if (actual !== [...expected].sort().join("\0")) throw new Error(`${label} keys are not exact`);
}

function record(value: unknown, label: string): JsonRecord {
  if (!value || typeof value !== "object" || Array.isArray(value)) throw new Error(`${label} must be an object`);
  return value as JsonRecord;
}

function stringField(value: JsonRecord, key: string, label: string): string {
  const result = value[key];
  if (typeof result !== "string" || result.length === 0) throw new Error(`${label}.${key} must be a non-empty string`);
  return result;
}

function shaField(value: JsonRecord, key: string, label: string): string {
  const result = stringField(value, key, label);
  if (!/^[0-9a-f]{64}$/.test(result)) throw new Error(`${label}.${key} must be a lowercase SHA-256 digest`);
  return result;
}

function addressField(value: JsonRecord, key: string, label: string): string {
  // Address syntax is checked against the frozen route identities by the
  // caller.  Here we only reject missing/non-string values so this module has
  // no Solana dependency and cannot accidentally accept a partial envelope.
  return stringField(value, key, label);
}

function parseOperation(value: unknown, index: number): BootstrapExecutionOperation {
  const label = `bootstrap authorization operations[${index}]`;
  const root = record(value, label);
  exactKeys(root, [
    "operation", "strategyId", "reserve", "vault", "setupAdmin", "strategyAuth",
    "strategyInitReceipt", "strategyAssetAta", "fourMarketRouteSpecSha256", "strategyGraphSha256",
    "builderRouteSpecSha256", "instructionDataSha256", "maxTotalLamports",
  ], label);
  const operation = stringField(root, "operation", label);
  if (operation !== "initialize-strategy" && operation !== "initialize-strategy-asset-ata") throw new Error(`${label}.operation is invalid`);
  const strategyId = stringField(root, "strategyId", label);
  if (strategyId !== "onre" && strategyId !== "prime" && strategyId !== "maple") throw new Error(`${label}.strategyId is invalid`);
  const data = record(root.instructionDataSha256, `${label}.instructionDataSha256`);
  if (operation === "initialize-strategy") exactKeys(data, ["setManager", "initializeStrategy", "restoreManager"], `${label}.instructionDataSha256`);
  else exactKeys(data, ["createAta"], `${label}.instructionDataSha256`);
  const instructionDataSha256 = Object.fromEntries(Object.entries(data).map(([key]) => [key, shaField(data, key, `${label}.instructionDataSha256`)]));
  const maxTotalLamports = stringField(root, "maxTotalLamports", label);
  if (!/^[1-9][0-9]*$/.test(maxTotalLamports)) throw new Error(`${label}.maxTotalLamports must be a positive decimal integer`);
  return {
    operation: operation as BootstrapExecutionOperation["operation"],
    strategyId: strategyId as BootstrapExecutionOperation["strategyId"],
    reserve: addressField(root, "reserve", label),
    vault: addressField(root, "vault", label),
    setupAdmin: addressField(root, "setupAdmin", label),
    strategyAuth: addressField(root, "strategyAuth", label),
    strategyInitReceipt: addressField(root, "strategyInitReceipt", label),
    strategyAssetAta: addressField(root, "strategyAssetAta", label),
    fourMarketRouteSpecSha256: shaField(root, "fourMarketRouteSpecSha256", label),
    strategyGraphSha256: shaField(root, "strategyGraphSha256", label),
    builderRouteSpecSha256: shaField(root, "builderRouteSpecSha256", label),
    instructionDataSha256,
    maxTotalLamports,
  };
}

function parseAuthorization(value: unknown): BootstrapExecutionAuthorization {
  const root = record(value, "bootstrap execution authorization");
  exactKeys(root, ["schemaVersion", "evidenceType", "approvalId", "expiresAtUnix", "routeId", "cluster", "genesisHash", "compatibilityApproval", "sourceBinding", "operations"], "bootstrap execution authorization");
  if (root.schemaVersion !== 1) throw new Error("bootstrap execution authorization schemaVersion must be 1");
  if (root.evidenceType !== "backyard-voltr-four-market-bootstrap-execution-authorization") throw new Error("bootstrap execution authorization evidenceType is not exact");
  const cluster = stringField(root, "cluster", "bootstrap execution authorization");
  if (cluster !== "mainnet-beta") throw new Error("bootstrap execution authorization cluster is not mainnet-beta");
  const compatibility = record(root.compatibilityApproval, "bootstrap authorization compatibilityApproval");
  exactKeys(compatibility, ["path", "fileSha256", "artifactPath", "artifactFileSha256"], "bootstrap authorization compatibilityApproval");
  const source = record(root.sourceBinding, "bootstrap authorization sourceBinding");
  exactKeys(source, ["algorithm", "files", "aggregateSha256"], "bootstrap authorization sourceBinding");
  if (source.algorithm !== "sha256" || !Array.isArray(source.files) || source.files.length === 0) throw new Error("bootstrap authorization sourceBinding is invalid");
  const files = source.files.map((entry, index) => {
    const item = record(entry, `bootstrap authorization sourceBinding.files[${index}]`);
    exactKeys(item, ["path", "sha256"], `bootstrap authorization sourceBinding.files[${index}]`);
    const path = stringField(item, "path", `bootstrap authorization sourceBinding.files[${index}]`);
    if (path.startsWith("/") || path.split("/").includes("..")) throw new Error(`bootstrap authorization source path escapes repository: ${path}`);
    return { path, sha256: shaField(item, "sha256", `bootstrap authorization sourceBinding.files[${index}]`) };
  });
  if (new Set(files.map(({ path }) => path)).size !== files.length) throw new Error("bootstrap authorization source paths are duplicated");
  const operations = root.operations;
  if (!Array.isArray(operations) || operations.length !== 6) throw new Error("bootstrap authorization must contain exactly six OnRe/Prime/Maple operations");
  const parsedOperations = operations.map(parseOperation);
  const operationKeys = parsedOperations.map(({ operation, strategyId }) => `${operation}:${strategyId}`);
  const expectedOperationKeys = ["initialize-strategy:onre", "initialize-strategy:prime", "initialize-strategy:maple", "initialize-strategy-asset-ata:onre", "initialize-strategy-asset-ata:prime", "initialize-strategy-asset-ata:maple"];
  if (operationKeys.join("\0") !== expectedOperationKeys.join("\0")) throw new Error("bootstrap authorization operation order/scope is not exact");
  const expiresAtUnix = stringField(root, "expiresAtUnix", "bootstrap execution authorization");
  if (!/^[1-9][0-9]*$/.test(expiresAtUnix)) throw new Error("bootstrap execution authorization expiresAtUnix must be a positive decimal integer");
  return {
    schemaVersion: 1,
    evidenceType: "backyard-voltr-four-market-bootstrap-execution-authorization",
    approvalId: stringField(root, "approvalId", "bootstrap execution authorization"),
    expiresAtUnix,
    routeId: stringField(root, "routeId", "bootstrap execution authorization"),
    cluster: "mainnet-beta",
    genesisHash: stringField(root, "genesisHash", "bootstrap execution authorization"),
    compatibilityApproval: {
      path: stringField(compatibility, "path", "bootstrap authorization compatibilityApproval"),
      fileSha256: shaField(compatibility, "fileSha256", "bootstrap authorization compatibilityApproval"),
      artifactPath: stringField(compatibility, "artifactPath", "bootstrap authorization compatibilityApproval"),
      artifactFileSha256: shaField(compatibility, "artifactFileSha256", "bootstrap authorization compatibilityApproval"),
    },
    sourceBinding: { algorithm: "sha256", files, aggregateSha256: shaField(source, "aggregateSha256", "bootstrap authorization sourceBinding") },
    operations: parsedOperations,
  };
}

/** Load and validate a canonical authorization file without loading a signer. */
export function loadBootstrapExecutionAuthorization(
  path: string,
  confirmedFileSha256: string | null,
  repositoryRoot = REPOSITORY_ROOT,
): BootstrapExecutionAuthorization {
  if (path !== BOOTSTRAP_EXECUTION_AUTHORIZATION_PATH) throw new Error(`bootstrap execution requires --authorization ${BOOTSTRAP_EXECUTION_AUTHORIZATION_PATH}`);
  const absolutePath = resolve(repositoryRoot, path);
  if (absolutePath !== resolve(repositoryRoot, BOOTSTRAP_EXECUTION_AUTHORIZATION_PATH)) throw new Error("bootstrap authorization path escapes repository");
  if (!confirmedFileSha256 || !/^[0-9a-f]{64}$/.test(confirmedFileSha256)) throw new Error("bootstrap execution requires --confirm-authorization-sha256");
  const bytes = readFileSync(absolutePath);
  const observedFileSha256 = sha256(bytes);
  if (observedFileSha256 !== confirmedFileSha256) throw new Error(`bootstrap authorization SHA-256 mismatch: observed ${observedFileSha256}, confirmed ${confirmedFileSha256}`);
  const text = bytes.toString("utf8");
  const parsed = JSON.parse(text) as unknown;
  if (canonicalFileText(parsed) !== text) throw new Error("bootstrap authorization must use canonical two-space JSON with one trailing newline");
  const authorization = parseAuthorization(parsed);
  if (BigInt(authorization.expiresAtUnix) < BigInt(Math.floor(Date.now() / 1_000))) throw new Error("bootstrap execution authorization is expired");
  if (authorization.compatibilityApproval.path !== COMPATIBILITY_APPROVAL_PATH || authorization.compatibilityApproval.artifactPath !== COMPATIBILITY_ARTIFACT_PATH) throw new Error("bootstrap authorization compatibility paths are not exact");
  const compatibilityApprovalBytes = readFileSync(resolve(repositoryRoot, COMPATIBILITY_APPROVAL_PATH));
  const compatibilityArtifactBytes = readFileSync(resolve(repositoryRoot, COMPATIBILITY_ARTIFACT_PATH));
  if (sha256(compatibilityApprovalBytes) !== authorization.compatibilityApproval.fileSha256 || sha256(compatibilityArtifactBytes) !== authorization.compatibilityApproval.artifactFileSha256) throw new Error("bootstrap authorization compatibility evidence hashes do not match current files");
  const compatibilityApproval = record(JSON.parse(compatibilityApprovalBytes.toString("utf8")), "bound compatibility approval");
  const compatibilityRouteSpec = record(compatibilityApproval.routeSpec, "bound compatibility approval routeSpec");
  const compatibilityArtifact = record(JSON.parse(compatibilityArtifactBytes.toString("utf8")), "bound compatibility artifact");
  const approvedFourMarketHash = shaField(compatibilityRouteSpec, "fourMarketRouteSpecSha256", "bound compatibility approval routeSpec");
  if (
    compatibilityArtifact.verdict !== "BACKYARD_VOLTR_FOUR_MARKET_COMPATIBILITY_PASS"
    || compatibilityArtifact.failedGateCount !== 0
    || compatibilityArtifact.fourMarketRouteSpecSha256 !== approvedFourMarketHash
    || !authorization.operations.every(({ fourMarketRouteSpecSha256 }) => fourMarketRouteSpecSha256 === approvedFourMarketHash)
  ) throw new Error("bootstrap authorization is not bound to a current PASS compatibility artifact and route hash");
  if (authorization.sourceBinding.files.map(({ path: sourcePath }) => sourcePath).join("\0") !== BOOTSTRAP_EXECUTION_SOURCE_PATHS.join("\0")) throw new Error("bootstrap authorization source file set/order is not exact");
  const observedFiles = authorization.sourceBinding.files.map(({ path: sourcePath }) => ({ path: sourcePath, sha256: sha256(readFileSync(resolve(repositoryRoot, sourcePath))) }));
  const observedAggregateSha256 = sha256(Buffer.from(canonicalJson(observedFiles)));
  if (canonicalJson(observedFiles) !== canonicalJson(authorization.sourceBinding.files) || observedAggregateSha256 !== authorization.sourceBinding.aggregateSha256) throw new Error("bootstrap authorization source binding does not match checked-out source");
  return authorization;
}

export function bootstrapExecutionSourceBinding(repositoryRoot = REPOSITORY_ROOT) {
  const files = BOOTSTRAP_EXECUTION_SOURCE_PATHS.map((path) => ({
    path,
    sha256: sha256(readFileSync(resolve(repositoryRoot, path))),
  }));
  return {
    algorithm: "sha256" as const,
    files,
    aggregateSha256: sha256(Buffer.from(canonicalJson(files))),
  } as const;
}

export function operationAuthorization(
  authorization: BootstrapExecutionAuthorization,
  operation: BootstrapExecutionOperation["operation"],
  strategyId: BootstrapExecutionOperation["strategyId"],
): BootstrapExecutionOperation {
  const result = authorization.operations.find((entry) => entry.operation === operation && entry.strategyId === strategyId);
  if (!result) throw new Error(`bootstrap authorization does not authorize ${operation}:${strategyId}`);
  return result;
}
