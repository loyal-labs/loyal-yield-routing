import { createHash } from "node:crypto";
import { readFileSync, writeFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { relative, resolve } from "node:path";

import { PublicKey } from "@solana/web3.js";

import {
  PARTNER_FOUR_MARKET_ROUTE,
  PARTNER_ROUTE,
  fourMarketRouteSpecSha256,
  partnerStrategyGraphSha256,
  partnerStrategyIdentity,
  type PartnerStrategyId,
} from "../domain/route-spec.js";
import {
  loadRuntimePolicyArtifact,
  type RuntimePolicyArtifact,
  type RuntimePolicyArtifactEntry,
} from "./compiler.js";

const REPOSITORY_ROOT = resolve(fileURLToPath(new URL("../../../..", import.meta.url)));
const AUTH_KIND = "backyard-voltr-four-market-policy-authorization";
const AUTH_PATH = "docs/evidence/backyard-voltr-four-market/policy-catalog-authorization-v23.json";
const SOURCE_PATHS = [
  "Cargo.toml",
  "Cargo.lock",
  "crates/loyal-actions/Cargo.toml",
  "crates/loyal-actions/src/lib.rs",
  "crates/loyal-actions/src/squads.rs",
  "crates/loyal-actions/src/autonomous_vaults/mod.rs",
  "crates/loyal-actions/src/autonomous_vaults/voltr_kamino.rs",
  "crates/loyal-actions/src/bin/compile_voltr_kamino_runtime_policy.rs",
  "crates/loyal-route-lookup-tables/Cargo.toml",
  "crates/loyal-route-lookup-tables/src/lib.rs",
  "crates/loyal-yield-store/Cargo.toml",
  "crates/loyal-yield-store/src/lib.rs",
  "crates/loyal-yield-store/src/store.rs",
  "crates/loyal-yield-store/src/types.rs",
  "crates/loyal-yield-store/src/fleet_orchestration/mod.rs",
  "crates/loyal-yield-store/src/fleet_orchestration/domain.rs",
  "crates/loyal-yield-store/src/fleet_orchestration/queue.rs",
  "crates/loyal-yield-store/src/fleet_orchestration/voltr_restoration.rs",
  "crates/loyal-yield-orchestrator/Cargo.toml",
  "crates/loyal-yield-orchestrator/src/lib.rs",
  "crates/loyal-yield-orchestrator/src/fleet_orchestration/mod.rs",
  "crates/loyal-yield-orchestrator/src/fleet_orchestration/observation.rs",
  "crates/loyal-yield-orchestrator/src/fleet_orchestration/planner.rs",
  "crates/loyal-yield-orchestrator/src/bin/fleet-opportunity-planner.rs",
  "crates/loyal-yield-orchestrator/src/bin/backyard-voltr-earn-replay.rs",
  "crates/loyal-yield-orchestrator/src/bin/backyard-voltr-restoration-bridge.rs",
  "crates/loyal-yield-orchestrator/src/bin/backyard-voltr-restoration-readback.rs",
  "tools/backyard-voltr/bun.lock",
  "tools/backyard-voltr/package.json",
  "tools/backyard-voltr/src/cli.ts",
  "tools/backyard-voltr/src/manager-cli.ts",
  "tools/backyard-voltr/src/bootstrap/authorization.ts",
  "tools/backyard-voltr/src/bootstrap/commands.ts",
  "tools/backyard-voltr/src/bootstrap/strategy-asset.ts",
  "tools/backyard-voltr/src/bootstrap/strategy.ts",
  "tools/backyard-voltr/src/activation/config.ts",
  "tools/backyard-voltr/src/domain/bootstrap-execution-authorization.ts",
  "tools/backyard-voltr/src/domain/execution-intent.ts",
  "tools/backyard-voltr/src/domain/route-spec.ts",
  "tools/backyard-voltr/src/integrations/signer.ts",
  "tools/backyard-voltr/src/integrations/solana-compat.ts",
  "tools/backyard-voltr/src/integrations/voltr.ts",
  "tools/backyard-voltr/src/runtime/manager.ts",
  "tools/backyard-voltr/src/runtime/protected-state.ts",
  "tools/backyard-voltr/src/runtime/restoration-bridge.ts",
  "tools/backyard-voltr/src/runtime/restoration-evidence.ts",
  "tools/backyard-voltr/src/runtime/commands.ts",
  "tools/backyard-voltr/src/runtime/earn-adapter.ts",
  "tools/backyard-voltr/src/runtime/final-reconciliation.ts",
  "tools/backyard-voltr/src/runtime/negative-mutations-mainnet.ts",
  "tools/backyard-voltr/src/runtime/receipt.ts",
  "tools/backyard-voltr/src/runtime/withdrawal-restoration.ts",
  "tools/backyard-voltr/src/runtime/withdrawal-scanner.ts",
  "tools/backyard-voltr/src/policies/authorization.ts",
  "tools/backyard-voltr/src/policies/commands.ts",
  "tools/backyard-voltr/src/policies/compiler.ts",
  "tools/backyard-voltr/src/verify/compatibility.ts",
  "tools/backyard-voltr/src/verify/current.ts",
  "tools/backyard-voltr/src/verify/finalized.ts",
  "tools/backyard-voltr/src/verify/four-market.ts",
  "tools/backyard-voltr/src/verify/integration-handoff.ts",
  "tools/backyard-voltr/src/verify/negative-mutations.ts",
  "tools/backyard-voltr/src/verify/squads.ts",
  "tools/backyard-voltr/src/verify/structure.ts",
  "tools/backyard-voltr/tsconfig.json",
] as const;
const SUFFIX: ReadonlyArray<{ strategyId: PartnerStrategyId; operation: "deposit" | "withdraw" }> = [
  { strategyId: "onre", operation: "deposit" },
  { strategyId: "onre", operation: "withdraw" },
  { strategyId: "prime", operation: "deposit" },
  { strategyId: "prime", operation: "withdraw" },
  { strategyId: "maple", operation: "deposit" },
  { strategyId: "maple", operation: "withdraw" },
];
const ALL: ReadonlyArray<{ strategyId: PartnerStrategyId; operation: "deposit" | "withdraw" }> = [
  { strategyId: "main", operation: "deposit" },
  { strategyId: "main", operation: "withdraw" },
  ...SUFFIX,
];

export type PolicyCatalogAuthorization = Readonly<{
  schemaVersion: 1;
  kind: typeof AUTH_KIND;
  routeId: string;
  routeSpecSha256: string;
  artifactPath: string;
  artifactFileSha256: string;
  artifactSha256: string;
  sourceManifestSha256: string;
  sourceAggregateSha256: string;
  sourceBinding: Readonly<{
    algorithm: "sha256";
    files: readonly Readonly<{ path: string; sha256: string }>[];
    aggregateSha256: string;
  }>;
  settings: string;
  manager: string;
  vault: string;
  admin: string;
  guardian: string;
  guardianPermissionsMask: 7;
  threshold: 1;
  catalogPolicySeedBefore: string;
  terminalPolicySeed: string;
  maxManagerOperationRaw: string;
  entries: readonly Readonly<{
    strategyId: PartnerStrategyId;
    operation: "deposit" | "withdraw";
    seed: string;
    policy: string;
    strategyGraphSha256: string;
    innerInstructionDataSha256: string;
    policyCreateDataSha256: string;
    managerExecutionDataSha256: string;
  }>[];
  authorizationSha256: string;
}>;

export type EffectiveRouteAuthorizationBody = Readonly<{
  schemaVersion: 1;
  kind: "backyard-voltr-four-market-effective-route-authorization";
  fourMarketRouteSpecSha256: string;
  runtimePolicyCatalog: Readonly<{
    fileSha256: string;
    artifactSha256: string;
    sourceManifestSha256: string;
  }>;
  policyCatalogAuthorization: Readonly<{
    fileSha256: string;
    authorizationSha256: string;
  }>;
  policies: readonly Readonly<{
    strategyId: PartnerStrategyId;
    operation: "deposit" | "withdraw";
    seed: string;
    policy: string;
    strategyGraphSha256: string;
    innerInstructionDataSha256: string;
    policyCreateProgramId: string;
    policyCreateDataSha256: string;
    managerExecutionProgramId: string;
    managerExecutionDataSha256: string;
    maxManagerOperationRaw: string;
  }>[];
}>;

export type EffectiveRouteAuthorizationDigest = Readonly<{
  body: EffectiveRouteAuthorizationBody;
  sha256: string;
}>;

function sha256(bytes: ArrayLike<number> | string): string {
  return createHash("sha256").update(typeof bytes === "string" ? bytes : Uint8Array.from(bytes)).digest("hex");
}

function canonicalJson(value: unknown): string {
  if (typeof value === "bigint") return JSON.stringify(value.toString());
  if (Array.isArray(value)) return `[${value.map(canonicalJson).join(",")}]`;
  if (value && typeof value === "object") {
    return `{${Object.entries(value)
      .sort(([left], [right]) => left.localeCompare(right))
      .map(([key, entry]) => `${JSON.stringify(key)}:${canonicalJson(entry)}`)
      .join(",")}}`;
  }
  return JSON.stringify(value);
}

function bodyWithoutHash(value: PolicyCatalogAuthorization): Omit<PolicyCatalogAuthorization, "authorizationSha256"> {
  const { authorizationSha256: _ignored, ...body } = value;
  return body;
}

function policyAddress(seed: bigint): string {
  const seedBytes = Buffer.alloc(8);
  seedBytes.writeBigUInt64LE(seed);
  return PublicKey.findProgramAddressSync(
    [
      Buffer.from("smart_account"),
      Buffer.from("policy"),
      new PublicKey(PARTNER_ROUTE.squads.settings).toBuffer(),
      seedBytes,
    ],
    new PublicKey(PARTNER_ROUTE.squads.program),
  )[0].toBase58();
}

function relativeRepositoryPath(path: string): string {
  const value = relative(REPOSITORY_ROOT, resolve(path));
  if (!value || value.startsWith("../") || value === ".." || value.startsWith("/")) {
    throw new Error("policy authorization path must remain inside the repository");
  }
  return value;
}

function sourceBinding(): PolicyCatalogAuthorization["sourceBinding"] {
  const files = SOURCE_PATHS.map((path) => ({ path, sha256: sha256(readFileSync(resolve(REPOSITORY_ROOT, path))) }));
  return { algorithm: "sha256", files, aggregateSha256: sha256(canonicalJson(files)) } as const;
}

function entryStrategy(entry: RuntimePolicyArtifactEntry): PartnerStrategyId {
  if (entry.strategyId === "main" || entry.strategyId === "onre" || entry.strategyId === "prime" || entry.strategyId === "maple") return entry.strategyId;
  throw new Error(`policy artifact entry ${entry.operation} is missing a four-market strategyId`);
}

function expectedEntry(entry: RuntimePolicyArtifactEntry, index: number): PolicyCatalogAuthorization["entries"][number] {
  const expected = ALL[index];
  if (!expected || entry.operation !== expected.operation || entryStrategy(entry) !== expected.strategyId) {
    throw new Error(`policy artifact entry ${index} is not the exact Main-retained/four-market catalog order`);
  }
  const seed = BigInt(entry.seed);
  const expectedSeed = PARTNER_ROUTE.squads.policySeedBefore + 1n + BigInt(index);
  const policy = policyAddress(seed);
  if (seed !== expectedSeed || entry.policy !== policy) {
    throw new Error(`policy artifact entry ${index} has an unexpected seed or PDA`);
  }
  for (const [label, value] of [
    ["innerInstructionDataSha256", entry.innerInstructionDataSha256],
    ["policyCreateDataSha256", entry.policyCreate.dataSha256],
    ["managerExecutionDataSha256", entry.managerExecution.dataSha256],
  ] as const) {
    if (!/^[0-9a-f]{64}$/.test(value)) throw new Error(`policy artifact entry ${index} ${label} is not a lowercase SHA-256`);
  }
  return {
    strategyId: expected.strategyId,
    operation: expected.operation,
    seed: seed.toString(),
    policy,
    strategyGraphSha256: partnerStrategyGraphSha256(expected.strategyId),
    innerInstructionDataSha256: entry.innerInstructionDataSha256,
    policyCreateDataSha256: entry.policyCreate.dataSha256,
    managerExecutionDataSha256: entry.managerExecution.dataSha256,
  };
}

function assertFourMarketArtifact(loaded: Readonly<{ artifact: RuntimePolicyArtifact; fileSha256: string }>): readonly PolicyCatalogAuthorization["entries"][number][] {
  const { artifact } = loaded;
  if (artifact.runtimePolicyCount !== 8 || artifact.routeId !== PARTNER_FOUR_MARKET_ROUTE.id || artifact.routeSpecSha256 !== fourMarketRouteSpecSha256()) {
    throw new Error("policy authorization requires the exact eight-entry four-market artifact");
  }
  if (!Array.isArray(artifact.sourceManifests) || artifact.sourceManifests.length !== 4) {
    throw new Error("policy authorization requires four strategy source manifests");
  }
  if (artifact.policies.length !== ALL.length) throw new Error("policy authorization requires exactly eight policy entries");
  const entries = artifact.policies.map((entry, index) => expectedEntry(entry, index));
  for (const [index, manifest] of artifact.sourceManifests.entries()) {
    if (manifest.routeId !== PARTNER_FOUR_MARKET_ROUTE.id || manifest.routeSpecSha256 !== fourMarketRouteSpecSha256()) {
      throw new Error(`policy authorization source manifest ${index} is not bound to the four-market route`);
    }
    const strategyId = manifest.strategyId;
    if (strategyId !== "main" && strategyId !== "onre" && strategyId !== "prime" && strategyId !== "maple") throw new Error(`policy authorization source manifest ${index} has no exact strategy id`);
    if (strategyId !== (["main", "onre", "prime", "maple"] as const)[index]) throw new Error(`policy authorization source manifest ${index} is out of canonical strategy order`);
    const strategy = partnerStrategyIdentity(strategyId);
    const expectedDepositSeed = (PARTNER_ROUTE.squads.policySeedBefore + 1n + BigInt(index * 2)).toString();
    const expectedWithdrawSeed = (PARTNER_ROUTE.squads.policySeedBefore + 2n + BigInt(index * 2)).toString();
    if (manifest.policySeeds.policySeedBefore !== PARTNER_ROUTE.squads.policySeedBefore.toString()
      || manifest.policySeeds.deposit !== expectedDepositSeed
      || manifest.policySeeds.withdraw !== expectedWithdrawSeed
      || manifest.ids.squadsSettings !== PARTNER_ROUTE.squads.settings
      || manifest.ids.manager !== PARTNER_ROUTE.squads.manager
      || manifest.ids.guardian !== PARTNER_ROUTE.squads.guardian
      || manifest.ids.admin !== PARTNER_ROUTE.setupAdmin
      || manifest.ids.vault !== PARTNER_ROUTE.vault
      || manifest.ids.reserve !== strategy.reserve
      || manifest.ids.lendingMarket !== strategy.graph.lendingMarket
      || manifest.ids.collateralFarm !== strategy.graph.reserveFarmState
      || manifest.limits.maxPerOperationRaw !== PARTNER_ROUTE.asset.maxManagerOperationRaw.toString()
      || manifest.vaultIndex !== PARTNER_ROUTE.squads.vaultIndex) throw new Error(`policy authorization source manifest ${index} identities/limit are not exact`);
  }
  const terminalSeed = (PARTNER_ROUTE.squads.policySeedBefore + BigInt(ALL.length)).toString();
  if (artifact.policySeedBefore !== PARTNER_ROUTE.squads.policySeedBefore.toString() || entries.at(-1)?.seed !== terminalSeed) {
    throw new Error(`policy authorization requires catalog seed base ${PARTNER_ROUTE.squads.policySeedBefore} and terminal seed ${terminalSeed}`);
  }
  return entries;
}

/**
 * One domain-separated identity for the complete manager execution boundary.
 * `runtimePolicyCatalog.fileSha256` is the digest of the exact artifact file
 * bytes; `artifactSha256` is the compiler's semantic artifact digest. The
 * intentionally redundant per-policy projection makes every installed PDA,
 * create-data hash, execution-data hash, and amount limit reviewable without
 * weakening the exact-file binding.
 */
export function effectiveRouteAuthorizationDigest(
  loaded: Readonly<{ artifact: RuntimePolicyArtifact; fileSha256: string }>,
  catalogAuthorization: Readonly<{ fileSha256: string; authorization: PolicyCatalogAuthorization }>,
): EffectiveRouteAuthorizationDigest {
  const canonicalEntries = assertFourMarketArtifact(loaded);
  const authorization = catalogAuthorization.authorization;
  for (const [label, value] of [
    ["runtime policy catalog file", loaded.fileSha256],
    ["runtime policy catalog artifact", loaded.artifact.artifactSha256],
    ["runtime policy catalog source manifest", loaded.artifact.sourceManifestSha256],
    ["policy catalog authorization file", catalogAuthorization.fileSha256],
    ["policy catalog authorization digest", authorization.authorizationSha256],
  ] as const) {
    if (!/^[0-9a-f]{64}$/.test(value)) throw new Error(`${label} SHA-256 is malformed`);
  }
  if (authorization.authorizationSha256 !== sha256(canonicalJson(bodyWithoutHash(authorization)))) {
    throw new Error("effective route authorization received an invalid authorization digest");
  }
  if (
    authorization.routeId !== PARTNER_FOUR_MARKET_ROUTE.id
    || authorization.routeSpecSha256 !== fourMarketRouteSpecSha256()
    || authorization.artifactFileSha256 !== loaded.fileSha256
    || authorization.artifactSha256 !== loaded.artifact.artifactSha256
    || authorization.sourceManifestSha256 !== loaded.artifact.sourceManifestSha256
    || authorization.maxManagerOperationRaw !== PARTNER_ROUTE.asset.maxManagerOperationRaw.toString()
    || JSON.stringify(authorization.entries) !== JSON.stringify(canonicalEntries)
  ) {
    throw new Error("effective route authorization catalog/authorization linkage is not exact");
  }
  const policies = loaded.artifact.policies.map((entry, index) => {
    const canonical = canonicalEntries[index];
    const manifest = loaded.artifact.sourceManifests?.find((value) => value.strategyId === canonical?.strategyId);
    if (
      !canonical
      || !manifest
      || entry.strategyId !== canonical.strategyId
      || entry.operation !== canonical.operation
      || entry.seed !== canonical.seed
      || entry.policy !== canonical.policy
      || entry.policyCreate.programId !== PARTNER_ROUTE.squads.program
      || entry.managerExecution.programId !== PARTNER_ROUTE.squads.program
      || entry.policyCreate.dataSha256 !== canonical.policyCreateDataSha256
      || entry.managerExecution.dataSha256 !== canonical.managerExecutionDataSha256
      || manifest.limits.maxPerOperationRaw !== authorization.maxManagerOperationRaw
    ) {
      throw new Error(`effective route authorization policy ${index} is not the exact authorized catalog entry`);
    }
    return {
      strategyId: canonical.strategyId,
      operation: canonical.operation,
      seed: canonical.seed,
      policy: canonical.policy,
      strategyGraphSha256: canonical.strategyGraphSha256,
      innerInstructionDataSha256: canonical.innerInstructionDataSha256,
      policyCreateProgramId: entry.policyCreate.programId,
      policyCreateDataSha256: canonical.policyCreateDataSha256,
      managerExecutionProgramId: entry.managerExecution.programId,
      managerExecutionDataSha256: canonical.managerExecutionDataSha256,
      maxManagerOperationRaw: manifest.limits.maxPerOperationRaw,
    } as const;
  });
  if (policies.length !== 8) throw new Error("effective route authorization requires exactly eight policies");
  const body: EffectiveRouteAuthorizationBody = {
    schemaVersion: 1,
    kind: "backyard-voltr-four-market-effective-route-authorization",
    fourMarketRouteSpecSha256: fourMarketRouteSpecSha256(),
    runtimePolicyCatalog: {
      fileSha256: loaded.fileSha256,
      artifactSha256: loaded.artifact.artifactSha256,
      sourceManifestSha256: loaded.artifact.sourceManifestSha256,
    },
    policyCatalogAuthorization: {
      fileSha256: catalogAuthorization.fileSha256,
      authorizationSha256: authorization.authorizationSha256,
    },
    policies,
  };
  return { body, sha256: sha256(canonicalJson(body)) } as const;
}

export function buildPolicyCatalogAuthorization(artifactPath: string, outputPath: string = AUTH_PATH): Readonly<{ path: string; fileSha256: string; authorizationSha256: string; effectiveRouteAuthorizationSha256: string; routeAuthorizationSha256: string; verdict: string }> {
  const loaded = loadRuntimePolicyArtifact(artifactPath);
  const entries = assertFourMarketArtifact(loaded);
  const sources = sourceBinding();
  const body = {
    schemaVersion: 1 as const,
    kind: AUTH_KIND as typeof AUTH_KIND,
    routeId: PARTNER_FOUR_MARKET_ROUTE.id,
    routeSpecSha256: fourMarketRouteSpecSha256(),
    artifactPath: relativeRepositoryPath(artifactPath),
    artifactFileSha256: loaded.fileSha256,
    artifactSha256: loaded.artifact.artifactSha256,
    sourceManifestSha256: loaded.artifact.sourceManifestSha256,
    sourceAggregateSha256: sources.aggregateSha256,
    sourceBinding: sources,
    settings: PARTNER_ROUTE.squads.settings,
    manager: PARTNER_ROUTE.squads.manager,
    vault: PARTNER_ROUTE.vault,
    admin: PARTNER_ROUTE.setupAdmin,
    guardian: PARTNER_ROUTE.squads.guardian,
    guardianPermissionsMask: 7 as const,
    threshold: 1 as const,
    catalogPolicySeedBefore: PARTNER_ROUTE.squads.policySeedBefore.toString(),
    terminalPolicySeed: (PARTNER_ROUTE.squads.policySeedBefore + BigInt(ALL.length)).toString(),
    maxManagerOperationRaw: PARTNER_ROUTE.asset.maxManagerOperationRaw.toString(),
    entries,
  };
  const authorizationSha256 = sha256(canonicalJson(body));
  const document = { ...body, authorizationSha256 };
  const path = resolve(outputPath);
  const serialized = `${JSON.stringify(document, null, 2)}\n`;
  writeFileSync(path, serialized, { mode: 0o600 });
  const fileSha256 = sha256(serialized);
  const effectiveRouteAuthorizationSha256 = effectiveRouteAuthorizationDigest(
    loaded,
    { fileSha256, authorization: document },
  ).sha256;
  return { path, fileSha256, authorizationSha256, effectiveRouteAuthorizationSha256, routeAuthorizationSha256: effectiveRouteAuthorizationSha256, verdict: "PARTNER_FOUR_MARKET_POLICY_AUTHORIZATION_BUILT" } as const;
}

export function loadPolicyCatalogAuthorization(
  authorizationPath: string,
  artifactPath: string,
  confirmAuthorizationSha256?: string | null,
): Readonly<{ path: string; fileSha256: string; authorization: PolicyCatalogAuthorization; artifact: RuntimePolicyArtifact }> {
  const path = resolve(authorizationPath);
  const bytes = readFileSync(path);
  const parsed = JSON.parse(bytes.toString("utf8")) as Partial<PolicyCatalogAuthorization>;
  const expectedKeys = [
    "schemaVersion", "kind", "routeId", "routeSpecSha256", "artifactPath", "artifactFileSha256", "artifactSha256",
    "sourceManifestSha256", "sourceAggregateSha256", "sourceBinding", "settings", "manager", "vault", "admin",
    "guardian", "guardianPermissionsMask", "threshold", "catalogPolicySeedBefore", "terminalPolicySeed",
    "maxManagerOperationRaw", "entries", "authorizationSha256",
  ];
  if (Object.keys(parsed).sort().join("\0") !== expectedKeys.sort().join("\0")) throw new Error("policy authorization envelope keys are not exact");
  if (parsed.schemaVersion !== 1 || parsed.kind !== AUTH_KIND) throw new Error("policy authorization envelope kind/schema is invalid");
  const authorization = parsed as PolicyCatalogAuthorization;
  const body = bodyWithoutHash(authorization);
  if (authorization.authorizationSha256 !== sha256(canonicalJson(body))) throw new Error("policy authorization envelope hash mismatch");
  const fileSha256 = sha256(bytes);
  if (confirmAuthorizationSha256 !== undefined && confirmAuthorizationSha256 !== null && confirmAuthorizationSha256 !== fileSha256) {
    throw new Error(`policy authorization confirmation SHA-256 mismatch: expected ${fileSha256}`);
  }
  if (authorization.routeId !== PARTNER_FOUR_MARKET_ROUTE.id || authorization.routeSpecSha256 !== fourMarketRouteSpecSha256()) throw new Error("policy authorization route binding is not exact");
  if (authorization.artifactPath !== relativeRepositoryPath(artifactPath)) throw new Error("policy authorization artifact path is not exact");
  const loaded = loadRuntimePolicyArtifact(artifactPath);
  const entries = assertFourMarketArtifact(loaded);
  if (authorization.artifactFileSha256 !== loaded.fileSha256 || authorization.artifactSha256 !== loaded.artifact.artifactSha256 || authorization.sourceManifestSha256 !== loaded.artifact.sourceManifestSha256) throw new Error("policy authorization artifact/source hash binding mismatch");
  const observedSources = sourceBinding();
  if (authorization.sourceBinding.algorithm !== "sha256" || JSON.stringify(authorization.sourceBinding.files) !== JSON.stringify(observedSources.files) || authorization.sourceBinding.aggregateSha256 !== observedSources.aggregateSha256 || authorization.sourceAggregateSha256 !== observedSources.aggregateSha256) throw new Error("policy authorization source binding does not match checked-out source");
  if (authorization.settings !== PARTNER_ROUTE.squads.settings || authorization.manager !== PARTNER_ROUTE.squads.manager || authorization.vault !== PARTNER_ROUTE.vault || authorization.admin !== PARTNER_ROUTE.setupAdmin || authorization.guardian !== PARTNER_ROUTE.squads.guardian || authorization.guardianPermissionsMask !== 7 || authorization.threshold !== 1 || authorization.catalogPolicySeedBefore !== PARTNER_ROUTE.squads.policySeedBefore.toString() || authorization.terminalPolicySeed !== (PARTNER_ROUTE.squads.policySeedBefore + BigInt(ALL.length)).toString() || authorization.maxManagerOperationRaw !== PARTNER_ROUTE.asset.maxManagerOperationRaw.toString()) throw new Error("policy authorization identity/limit/seed boundary is not exact");
  if (JSON.stringify(authorization.entries) !== JSON.stringify(entries)) throw new Error("policy authorization entries do not match the canonical artifact catalog");
  return { path, fileSha256, authorization, artifact: loaded.artifact } as const;
}

export function policyCatalogEntries(): typeof ALL {
  return ALL;
}

export function policyCatalogAuthorizationPath(): string {
  return resolve(REPOSITORY_ROOT, AUTH_PATH);
}
