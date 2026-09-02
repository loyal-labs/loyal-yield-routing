/**
 * Phase 2 is deliberately verified independently from the Phase 1 lifecycle.
 *
 * This file does not compile policies, resolve accounts, sign packets, simulate,
 * or install anything.  It is the fail-closed consumer of those producers.  A
 * future producer may change implementation, but it must emit the evidence
 * shapes below before Phase 2 can be called ready.
 */
import { createHash } from "node:crypto";
import { existsSync, readFileSync } from "node:fs";
import { resolve } from "node:path";
import { fileURLToPath } from "node:url";

type Json = null | boolean | number | string | Json[] | { [key: string]: Json };
type RecordJson = { [key: string]: Json };
type Failure = Readonly<{ id: string; observed: Json; required: string }>;

const ROOT = resolve(fileURLToPath(new URL("../../../..", import.meta.url)));
const CATALOG = "crates/loyal-actions/fixtures/backyard_rwa_policy_catalog_v1.json";
const RESOLUTION = "docs/evidence/backyard-rwa-go/policy-resolution-v1.json";
const JUPITER_HEADERS = "docs/evidence/backyard-rwa-go/policy-jupiter-headers-v1.json";
const COMPILED = "docs/evidence/backyard-rwa-go/policy-compiled-v1.json";
const PACKETS = "docs/evidence/backyard-rwa-go/policy-packets-v1.json";
const SIMULATIONS = "docs/evidence/backyard-rwa-go/policy-signed-unsent-v1.json";
const INSTALL = "docs/evidence/backyard-rwa-go/policy-install-readback-v1.json";
const PACKET_LIMIT = 1_232;
const MAINNET_GENESIS = "5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d";
const LANE_KEYS = [
  "OnRe/ONyc/USDC", "OnRe/ONyc/USDG", "OnRe/ONyc/USDS",
  "Prime/PRIME/USDC", "Prime/PRIME/PYUSD", "Prime/PRIME/USDS",
  "Maple/syrupUSDC/USDC", "Maple/syrupUSDC/USDG", "Maple/syrupUSDC/PYUSD",
  "AUTO/AUTO/PYUSD", "Ethena/USDe/PYUSD",
] as const;
const KAMINO_OPERATIONS = ["Deposit", "Withdraw", "Borrow", "Repay"] as const;
const POSITIVE_GROUPS = [
  "three-lane-markets", "singleton-markets", "swap-graph", "bridge-lifecycle",
] as const;
const NEGATIVE_MUTATIONS = [
  "same-mint-wrong-reserve", "cross-lane-obligation", "unapproved-edge",
  "extra-instruction", "amount-cap-breach", "signer-substitution",
  "writable-role-substitution",
] as const;
const EXTERNAL_BRIDGE_POLICIES = {
  allocate: { seed: "62", account: "HoDV7mtsb2u1VARZLYuGByW7cCsGWL9NFxHZs7WHjdzz", dataSha256: "bda72932f474064fa3cd60ce91633acba35b2730e86b82f4352aa96a6738e2f4" },
  "nav-refresh": { seed: "63", account: "41nzu42c3KPgJfWhnV5jbfxjHbvVU6HXaiJmzzYNqvBP", dataSha256: "bf34a3e9c9c635c79a0d30e096b639a86d52e300ad113c81161e3486832d97ca" },
  "stage-withdrawal": { seed: "64", account: "ALz5Wkt82GhGFH1LfzbnAovkZ6t85ErovbxHUH3yY1wY", dataSha256: "ef8c231497fb2620b5930cfe5d329c871f103db6512781eb5487534db8b1291b" },
  restore: { seed: "65", account: "DjYYkQWb4zYbySfEndjVdg2NwZ8i77Fb9P1UFVbebc5t", dataSha256: "84e8f6f881758cff1714ef743603c016024104f9834392c6fba693c3651b719c" },
} as const;

function asRecord(value: Json | undefined): RecordJson | null {
  return value !== null && typeof value === "object" && !Array.isArray(value) ? value as RecordJson : null;
}

function asArray(value: Json | undefined): Json[] {
  return Array.isArray(value) ? value : [];
}

function sha256(value: Buffer | string): string {
  return createHash("sha256").update(value).digest("hex");
}

function isSha256(value: unknown): value is string {
  return typeof value === "string" && /^[0-9a-f]{64}$/i.test(value);
}

function exactSet(observed: readonly string[], expected: readonly string[]): boolean {
  return observed.length === expected.length && new Set(observed).size === observed.length
    && observed.every((value) => expected.includes(value));
}

function positiveInteger(value: Json | undefined): boolean {
  return typeof value === "number" && Number.isSafeInteger(value) && value > 0;
}

function readJson(path: string, failures: Failure[]): RecordJson | null {
  const absolute = resolve(ROOT, path);
  if (!existsSync(absolute)) {
    failures.push({ id: `artifact_missing:${path}`, observed: null, required: "checked-in evidence artifact" });
    return null;
  }
  try {
    const parsed = JSON.parse(readFileSync(absolute, "utf8")) as Json;
    const record = asRecord(parsed);
    if (record === null) failures.push({ id: `artifact_not_object:${path}`, observed: parsed, required: "JSON object" });
    return record;
  } catch (error) {
    failures.push({ id: `artifact_invalid_json:${path}`, observed: String(error), required: "valid JSON object" });
    return null;
  }
}

function artifactSha(path: string): string | null {
  const absolute = resolve(ROOT, path);
  return existsSync(absolute) ? sha256(readFileSync(absolute)) : null;
}

function check(failures: Failure[], id: string, pass: boolean, observed: Json, required: string): void {
  if (!pass) failures.push({ id, observed, required });
}

function edgeKey(value: Json): string {
  const row = asRecord(value);
  return typeof row?.from === "string" && typeof row?.to === "string" ? `${row.from}->${row.to}` : "";
}

function expectedEdges(): string[] {
  const stable = ["USDC", "USDG", "USDS", "PYUSD"];
  const rwa = ["ONyc", "PRIME", "syrupUSDC", "AUTO", "USDe"];
  return [
    ...stable.flatMap((from) => rwa.map((to) => `${from}->${to}`)),
    ...rwa.flatMap((from) => stable.map((to) => `${from}->${to}`)),
    ...stable.flatMap((from) => stable.filter((to) => to !== from).map((to) => `${from}->${to}`)),
  ];
}

function laneKey(value: Json): string {
  const row = asRecord(value);
  return [row?.market, row?.collateral, row?.debt].every((part) => typeof part === "string")
    ? `${row!.market}/${row!.collateral}/${row!.debt}` : "";
}

function operationKey(value: Json): string {
  const row = asRecord(value);
  const lane = laneKey(value);
  return lane && typeof row?.operation === "string" ? `${lane}/${row.operation}` : "";
}

function validWire(row: RecordJson | null): boolean {
  if (row === null || typeof row.transactionBase64 !== "string" || !isSha256(row.transactionSha256)) return false;
  try {
    const wire = Buffer.from(row.transactionBase64, "base64");
    return wire.length > 0 && wire.toString("base64") === row.transactionBase64
      && sha256(wire) === row.transactionSha256;
  } catch {
    return false;
  }
}

function signedUnsentPositiveBundle(row: Json, compiledSha: string | null, externalBridge = false): boolean {
  const value = asRecord(row);
  const simulation = asRecord(value?.simulation);
  const simulationSlot = simulation?.contextSlot;
  const readbackSlot = value?.confirmedReadbackSlot;
  return value?.broadcast === false && validWire(value)
    && typeof value?.signature === "string" && value.signature.length > 0
    && typeof value?.packetBytes === "number" && value.packetBytes === Buffer.from(String(value.transactionBase64), "base64").length
    && simulation?.err === null && positiveInteger(simulationSlot)
    && value?.signatureAbsentOnChain === true
    && typeof readbackSlot === "number" && typeof simulationSlot === "number" && readbackSlot >= simulationSlot
    && isSha256(value?.chainPreStateSha256) && value?.chainPreStateSha256 === value?.chainPostStateSha256
    && (externalBridge
      ? value?.policyScope === "phase1-external-existing" && value?.compiledArtifactSha256 === null
      : compiledSha !== null && value?.compiledArtifactSha256 === compiledSha);
}

function signedUnsentNegativeBundle(row: Json, compiledSha: string | null): boolean {
  const value = asRecord(row);
  const simulation = asRecord(value?.simulation);
  return value?.broadcast === false && value?.accepted === false && validWire(value)
    && typeof value?.signature === "string" && value.signature.length > 0
    && typeof value?.packetBytes === "number" && value.packetBytes === Buffer.from(String(value.transactionBase64), "base64").length
    && typeof value?.rejectionLayer === "string" && value.rejectionLayer.length > 0
    && ((simulation !== null && simulation.err !== null && positiveInteger(simulation.contextSlot))
      || value?.rejectionLayer === "canonical-go-builder")
    && value?.signatureAbsentOnChain === true
    && isSha256(value?.chainPreStateSha256) && value?.chainPreStateSha256 === value?.chainPostStateSha256
    && (compiledSha === null || value?.compiledArtifactSha256 === compiledSha);
}

const GROUP_COVERAGE: Readonly<Record<(typeof POSITIVE_GROUPS)[number], readonly string[]>> = {
  "three-lane-markets": ["OnRe/ONyc/USDC/deposit", "Prime/PRIME/USDC/deposit", "Maple/syrupUSDC/USDC/deposit"],
  "singleton-markets": ["AUTO/AUTO/PYUSD/deposit", "Ethena/USDe/PYUSD/deposit"],
  "swap-graph": ["USDC->PRIME", "PRIME->USDC", "USDC->USDG"],
  "bridge-lifecycle": ["allocate", "stage-withdrawal", "restore", "nav-refresh"],
};

function positiveSimulationGroup(row: Json, name: (typeof POSITIVE_GROUPS)[number], compiledSha: string | null): boolean {
  const value = asRecord(row);
  const bundles = asArray(value?.bundles);
  const coverage = bundles.map((bundle) => String(asRecord(bundle)?.name ?? ""));
  return value?.name === name && value?.broadcast === false && bundles.length === GROUP_COVERAGE[name].length
    && exactSet(coverage, GROUP_COVERAGE[name])
    && bundles.every((bundle) => signedUnsentPositiveBundle(bundle, compiledSha, name === "bridge-lifecycle"))
    && (name !== "bridge-lifecycle" || bundles.every((bundle) => {
      const item = asRecord(bundle);
      const expected = EXTERNAL_BRIDGE_POLICIES[item?.name as keyof typeof EXTERNAL_BRIDGE_POLICIES];
      const policy = asRecord(item?.externalPolicy);
      return expected !== undefined && policy?.seed === expected.seed && policy?.account === expected.account
        && policy?.dataSha256 === expected.dataSha256;
    }));
}

function negativeSimulationMutation(row: Json, name: (typeof NEGATIVE_MUTATIONS)[number], compiledSha: string | null): boolean {
  const value = asRecord(row);
  const bundles = asArray(value?.bundles);
  return value?.mutation === name && value?.broadcast === false && value?.accepted === false && bundles.length > 0
    && new Set(bundles.map((bundle) => String(asRecord(bundle)?.name ?? ""))).size === bundles.length
    && bundles.every((bundle) => typeof asRecord(bundle)?.name === "string" && String(asRecord(bundle)?.name).length > 0)
    && bundles.every((bundle) => signedUnsentNegativeBundle(bundle, compiledSha));
}

/** Returns a JSON-safe PASS/FAIL document. Missing evidence is FAIL, never BLOCKED. */
export function verifyPhaseTwoAuthority(): RecordJson {
  const failures: Failure[] = [];
  const catalog = readJson(CATALOG, failures);
  const resolution = readJson(RESOLUTION, failures);
  const headers = readJson(JUPITER_HEADERS, failures);
  const compiled = readJson(COMPILED, failures);
  const packets = readJson(PACKETS, failures);
  const simulations = readJson(SIMULATIONS, failures);
  const install = readJson(INSTALL, failures);
  const expectedEdgeKeys = expectedEdges();

  const catalogLanes = asArray(catalog?.lanes).map(laneKey);
  const catalogOperations = asArray(catalog?.operations).map(operationKey);
  const catalogEdges = asArray(catalog?.swapEdges).map(edgeKey);
  const expectedOperationKeys = LANE_KEYS.flatMap((lane) => KAMINO_OPERATIONS.map((operation) => `${lane}/${operation}`));
  check(failures, "catalog_schema", catalog?.schema === "loyal-backyard-rwa-policy-catalog/v1", catalog?.schema ?? null, "loyal-backyard-rwa-policy-catalog/v1");
  check(failures, "lane_bijection", exactSet(catalogLanes, LANE_KEYS), catalogLanes, "exactly the requested 11 lanes once each");
  check(failures, "kamino_operation_bijection", exactSet(catalogOperations, expectedOperationKeys), catalogOperations,
    "Deposit/Withdraw/Borrow/Repay exactly once for each requested lane (44 total)");
  check(failures, "swap_edge_bijection", exactSet(catalogEdges, expectedEdgeKeys), catalogEdges,
    "the exact 52 directed stable/RWA and stable/stable edges once each");

  const resolutionLanes = asArray(resolution?.lanes);
  const resolutionSwap = asRecord(resolution?.swap);
  const seedBefore = typeof resolution?.policySeedBefore === "string" && /^\d+$/.test(resolution.policySeedBefore)
    ? BigInt(resolution.policySeedBefore) : null;
  const resolutionLaneKeys = resolutionLanes.map((row) => String(asRecord(row)?.key ?? ""));
  check(failures, "resolution_schema", resolution?.schema === "loyal-backyard-rwa-policy-resolution/v1", resolution?.schema ?? null,
    "resolver v1 output");
  check(failures, "live_seed_provenance", resolution?.broadcast === false && resolution?.cluster === "mainnet-beta"
    && resolution?.genesisHash === MAINNET_GENESIS && resolution?.commitment === "confirmed"
    && positiveInteger(resolution?.contextSlot) && seedBefore !== null && seedBefore >= 66n
    && isSha256(resolution?.catalogSha256) && resolution?.catalogSha256 === artifactSha(CATALOG),
  resolution === null ? null : { contextSlot: resolution.contextSlot ?? null, policySeedBefore: resolution.policySeedBefore ?? null, catalogSha256: resolution.catalogSha256 ?? null },
  "confirmed mainnet Settings observation with current policySeedBefore and catalog hash");
  check(failures, "live_lane_provenance", resolution?.laneGraphExact === true && resolution?.addressesResolved === true
    && exactSet(resolutionLaneKeys, LANE_KEYS) && resolutionLanes.every((row) => asRecord(row)?.exact === true),
  resolutionLaneKeys, "all 11 decoded current lane graphs, each exact and resolver-marked");
  check(failures, "live_swap_request_bijection", exactSet(asArray(resolutionSwap?.edges).map(edgeKey), expectedEdgeKeys),
  asArray(resolutionSwap?.edges).length, "resolver carries the exact 52 requested swap edges; current headers are proven separately");

  const headerRows = asArray(headers?.rows);
  check(failures, "jupiter_header_schema", headers?.schema === "loyal-backyard-rwa-jupiter-header-evidence/v2"
    && headers?.verdict === "PASS_HEADERS_RESOLVED" && headers?.broadcast === false
    && headers?.requestedEdgeCount === 52 && headers?.passCount === 52,
  headers === null ? null : { schema: headers.schema ?? null, verdict: headers.verdict ?? null, passCount: headers.passCount ?? null },
  "resolved Jupiter header evidence v1 for all 52 edges");
  check(failures, "jupiter_header_edge_bijection", exactSet(headerRows.map((row) => String(asRecord(row)?.key ?? "")), expectedEdgeKeys)
    && headerRows.every((row) => {
      const item = asRecord(row); const quote = asRecord(item?.quote); const header = asRecord(item?.header); const instruction = asRecord(item?.instruction);
      return item?.pass === true && asRecord(item?.source) !== null && asRecord(item?.destination) !== null
        && quote !== null && header !== null && instruction !== null && typeof header.dialect === "string"
        && typeof instruction.dataBase64 === "string"
        && isSha256(instruction.dataSha256) && sha256(Buffer.from(instruction.dataBase64, "base64")) === instruction.dataSha256
        && Array.isArray(instruction.accounts) && instruction.accounts.length > 0 && Array.isArray(item.lookupTables);
    }),
  headerRows.length, "one exact successful current SharedAccountsRoute header for each required edge");

  const compiledPolicies = asArray(compiled?.policies);
  const compiledSha = artifactSha(COMPILED);
  const compiledNames = compiledPolicies.map((row) => String(asRecord(row)?.name ?? ""));
  const expectedSeeds = seedBefore === null ? [] : compiledPolicies.map((_, index) => String(seedBefore + BigInt(index + 1)));
  const compiledOperations = compiledPolicies.flatMap((row) => {
    const item = asRecord(row);
    const logical = String(item?.logicalName ?? "").replace(/^lane\//, "");
    return asArray(item?.operations).map((operation) => `${logical}/${String(operation).replace(/^./, (value) => value.toUpperCase())}`);
  });
  const compiledEdges = compiledPolicies.flatMap((row) => asArray(asRecord(row)?.swapEdges).map(edgeKey));
  const compiledPolicySemanticsExact = compiledPolicies.every((row) => {
    const item = asRecord(row);
    const operations = asArray(item?.operations);
    const edges = asArray(item?.swapEdges);
    const semanticCount = operations.length + edges.length;
    return item !== null && semanticCount > 0 && asArray(item.constraints).length === semanticCount
      && item.semanticEdgeCount === semanticCount && !String(item.name ?? "").toLowerCase().includes("bridge")
      && !String(item.logicalName ?? "").toLowerCase().includes("bridge");
  });
  check(failures, "compiled_artifact_schema", compiled?.schema === "loyal-backyard-rwa-resolved-policy-artifact/v1"
    && compiled?.phase === "phase2" && compiled?.verdict === "COMPILED_SIGNED_SIMULATION_REQUIRED" && compiled?.broadcast === false
    && compiled?.catalogSha256 === artifactSha(CATALOG) && compiled?.resolutionSha256 === artifactSha(RESOLUTION),
  compiled === null ? null : { schema: compiled.schema ?? null, phase: compiled.phase ?? null, verdict: compiled.verdict ?? null },
  "resolver-fed Phase 2 compiler artifact bound to current catalog and resolution bytes");
  check(failures, "compiled_policy_bijection", positiveInteger(compiled?.physicalPolicyCount)
    && compiled?.physicalPolicyCount === compiledPolicies.length
    && compiledNames.every((name) => name.length > 0) && new Set(compiledNames).size === compiledNames.length
    && exactSet(compiledPolicies.map((row) => String(asRecord(row)?.seed ?? "")), expectedSeeds),
  compiledPolicies.map((row) => ({ name: asRecord(row)?.name ?? null, seed: asRecord(row)?.seed ?? null })),
  "the compiler-selected first-safe physical layout at unique contiguous forward seeds");
  check(failures, "compiled_semantic_bijection", exactSet(compiledOperations, expectedOperationKeys)
    && exactSet(compiledEdges, expectedEdgeKeys) && compiledPolicySemanticsExact,
  { operations: compiledOperations.length, swapEdges: compiledEdges.length },
  "compiled policies expand bijectively to the exact 44 Kamino operations and 52 requested swap edges, with no bridge-only policy");

  const measurements = asArray(packets?.measurements);
  const attemptedRungs = asArray(asRecord(compiled?.packing)?.attemptedRungs);
  const selectedRung = asRecord(compiled?.packing)?.selectedRung;
  const selectedMeasurements = compiledPolicies.map((row) => asRecord(asRecord(row)?.createPacket)).filter((row): row is RecordJson => row !== null);
  const attemptedMeasurementKeys = attemptedRungs.flatMap((rung) => asArray(asRecord(rung)?.measurements)
    .map((row) => String(asRecord(row)?.key ?? "")));
  const priorRungsRejectOversize = attemptedRungs.every((rung) => {
    const item = asRecord(rung); const rows = asArray(item?.measurements).map(asRecord);
    return item?.fits === true
      ? rows.length > 0 && rows.every((row) => typeof row?.packetBytes === "number" && row.packetBytes <= PACKET_LIMIT)
      : rows.some((row) => typeof row?.packetBytes === "number" && row.packetBytes > PACKET_LIMIT);
  });
  check(failures, "signed_packet_measurements", packets?.schema === "loyal-backyard-rwa-policy-packet-evidence/v1"
    && packets?.broadcast === false && packets?.compiledArtifactSha256 === compiledSha
    && packets?.signed === true && packets?.cryptographicSignaturesVerified === true
    && packets?.selectedRung === selectedRung
    && exactSet(measurements.map((row) => String(asRecord(row)?.key ?? "")), attemptedMeasurementKeys)
    && measurements.every((row) => {
      const item = asRecord(row);
      if (item === null || !validWire(item) || typeof item.packetBytes !== "number" || typeof item.transactionBase64 !== "string") return false;
      try { return item.signatureVerified === true && item.packetBytes === Buffer.from(item.transactionBase64, "base64").length; } catch { return false; }
    }) && priorRungsRejectOversize
    && selectedMeasurements.length === compiledPolicies.length
    && selectedMeasurements.every((item) => validWire(item) && typeof item.packetBytes === "number" && item.packetBytes <= PACKET_LIMIT),
  { attempted: measurements.length, selected: selectedMeasurements.length, selectedRung: selectedRung ?? null },
  "every attempted signed construction packet is measured; rejected rungs overflow and every selected packet fits 1232 bytes");

  const positiveRows = asArray(simulations?.positiveGroups);
  const negativeRows = asArray(simulations?.negativeMutations);
  check(failures, "grouped_signed_unsent_schema", simulations?.schema === "loyal-backyard-rwa-policy-signed-unsent/v1"
    && simulations?.verdict === "PASS" && simulations?.broadcast === false && simulations?.signedUnsent === true && simulations?.cluster === "mainnet-beta"
    && simulations?.commitment === "confirmed" && simulations?.genesisHash === MAINNET_GENESIS
    && simulations?.compiledArtifactSha256 === compiledSha,
  simulations === null ? null : { schema: simulations.schema ?? null, signedUnsent: simulations.signedUnsent ?? null },
  "final PASS confirmed-mainnet signed-unsent evidence bound to the compiler artifact (partial intermediate aggregates do not qualify)");
  check(failures, "four_positive_groups", exactSet(positiveRows.map((row) => String(asRecord(row)?.name ?? "")), POSITIVE_GROUPS)
    && POSITIVE_GROUPS.every((name) => positiveSimulationGroup(positiveRows.find((row) => asRecord(row)?.name === name) ?? null, name, compiledSha)),
  positiveRows.map((row) => asRecord(row)?.name ?? null), "exactly four successful structural-group signed-unsent simulations with unchanged confirmed chain state");
  check(failures, "seven_negative_mutations", exactSet(negativeRows.map((row) => String(asRecord(row)?.mutation ?? "")), NEGATIVE_MUTATIONS)
    && NEGATIVE_MUTATIONS.every((name) => negativeSimulationMutation(negativeRows.find((row) => asRecord(row)?.mutation === name) ?? null, name, compiledSha)),
  negativeRows.map((row) => asRecord(row)?.mutation ?? null), "exactly the seven named rejected mutations with no broadcast and unchanged confirmed chain state");

  const installs = asArray(install?.operations);
  check(failures, "forward_install_readback_schema", install?.schema === "loyal-backyard-rwa-policy-install-readback/v1"
    && install?.broadcast === true && install?.cluster === "mainnet-beta" && install?.commitment === "confirmed"
    && install?.genesisHash === MAINNET_GENESIS && install?.compiledArtifactSha256 === compiledSha
    && install?.policySeedBefore === resolution?.policySeedBefore && Array.isArray(install?.retiredOrClosedSeeds)
    && install?.retiredOrClosedSeeds.length === 0,
  install === null ? null : { schema: install.schema ?? null, policySeedBefore: install.policySeedBefore ?? null },
  "post-install readback evidence that only creates the compiler-bound Phase 2 policies");
  check(failures, "forward_install_batches", exactSet(installs.map((row) => String(asRecord(row)?.policyName ?? "")), compiledNames)
    && exactSet(installs.map((row) => String(asRecord(row)?.seed ?? "")), expectedSeeds)
    && installs.every((row) => {
      const item = asRecord(row); return item?.action === "create" && typeof item?.transactionSignature === "string"
        && positiveInteger(item?.confirmedSlot) && typeof item?.policyAddress === "string" && isSha256(item?.dataSha256)
        && typeof item?.dataBase64 === "string" && sha256(Buffer.from(item.dataBase64, "base64")) === item.dataSha256
        && item?.active === true && typeof item?.owner === "string" && item.owner.length > 0;
    }),
  installs.map((row) => ({ action: asRecord(row)?.action ?? null, seed: asRecord(row)?.seed ?? null })),
  "one forward-only confirmed PolicyCreate/readback per compiled policy, with no close/remove/update operation");

  const verdict = failures.length === 0 ? "PASS" : "FAIL";
  return {
    schema: "loyal-backyard-rwa-phase2-authority-verifier/v1",
    verdict,
    condition: "All 11 lanes, 44 Kamino permissions, 52 swap edges, exact current resolver provenance, signed packet fit, grouped simulations, negative mutations, and forward-only installed policy readback agree.",
    artifactSha256: {
      catalog: artifactSha(CATALOG), resolution: artifactSha(RESOLUTION), jupiterHeaders: artifactSha(JUPITER_HEADERS),
      compiled: compiledSha, packets: artifactSha(PACKETS), simulations: artifactSha(SIMULATIONS), install: artifactSha(INSTALL),
    },
    requiredEvidence: {
      resolution: RESOLUTION, jupiterHeaders: JUPITER_HEADERS, compiled: COMPILED,
      signedPacketMeasurements: PACKETS, signedUnsentSimulations: SIMULATIONS, forwardInstallReadback: INSTALL,
    },
    checks: {
      lanes: LANE_KEYS.length, kaminoOperations: expectedOperationKeys.length, swapEdges: expectedEdgeKeys.length,
      positiveGroups: [...POSITIVE_GROUPS], negativeMutations: [...NEGATIVE_MUTATIONS], packetLimitBytes: PACKET_LIMIT,
    },
    failures,
  };
}

const output = verifyPhaseTwoAuthority();
console.log(JSON.stringify(output, null, 2));
if (output.verdict !== "PASS") process.exitCode = 1;
