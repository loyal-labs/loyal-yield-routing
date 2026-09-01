import { createHash } from "node:crypto";
import { existsSync, readFileSync, readdirSync } from "node:fs";
import { dirname, join, relative, resolve } from "node:path";
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";
import { PublicKey } from "@solana/web3.js";

const REPOSITORY_ROOT = resolve(fileURLToPath(new URL("../../../..", import.meta.url)));
const APPS_ROOT = resolve(REPOSITORY_ROOT, "../loyal-apps");
const SCHEMA = "loyal-backyard-rwa-go-lifecycle/v2";
const PLAN_PATH = "docs/plans/backyard-voltr-orchestrator-verifier.md";
const PLAN_SHA256 = "5caa613216b182fd12db44be9d372f32b709b7d972ac9b1415cee6f8ce0a4fdc";
const MANIFEST_PATH = "docs/manifests/backyard-rwa-v1.json";
const POLICY_CATALOG_PATH = "crates/loyal-actions/fixtures/backyard_rwa_policy_catalog_v1.json";
const ADAPTOR_MANIFEST = "crates/loyal-voltr-rwa-nav-adaptor/Cargo.toml";
const ADAPTOR_PROCESSOR = "crates/loyal-voltr-rwa-nav-adaptor/src/processor.rs";
const ADAPTOR_CONFIG = "crates/loyal-voltr-rwa-nav-adaptor/src/config.rs";
const GO_ROOT = "go/backyard-rwa-worker";
const DEPLOYMENT_EVIDENCE = "docs/evidence/backyard-rwa-go/deployment-v1.json";
const LIFECYCLE_EVIDENCE = "docs/evidence/backyard-rwa-go/lifecycle-v1.json";
const ADAPTOR_SIMULATION_EVIDENCE = "docs/evidence/backyard-rwa-go/adaptor-v2-signer-simulation-v1.json";
const POLICY_SIMULATION_EVIDENCE = "docs/evidence/backyard-rwa-go/policy-catalog-simulation-v1.json";
const SOLE_COMMAND = "bun run --cwd tools/backyard-voltr verify:rwa-multiply-custom-lifecycle";

type Verdict = "PASS" | "FAIL" | "BLOCKED";
type JsonRecord = Record<string, unknown>;

type Check = Readonly<{
  id: string;
  verdict: Verdict;
  condition: string;
  evidence: JsonRecord;
  resumeCondition: string | null;
}>;

type CommandResult = Readonly<{
  command: string;
  exitCode: number;
  stdoutTail: string;
  stderrTail: string;
}>;

function absolute(path: string): string {
  return resolve(REPOSITORY_ROOT, path);
}

function read(path: string): string {
  return readFileSync(absolute(path), "utf8");
}

function sha256(value: string | Uint8Array): string {
  return createHash("sha256").update(value).digest("hex");
}

function sha256File(path: string): string | null {
  return existsSync(absolute(path)) ? sha256(readFileSync(absolute(path))) : null;
}

function parseJson(path: string): JsonRecord | null {
  if (!existsSync(absolute(path))) return null;
  try {
    const value = JSON.parse(read(path)) as unknown;
    return value !== null && typeof value === "object" && !Array.isArray(value)
      ? value as JsonRecord
      : null;
  } catch {
    return null;
  }
}

function redact(value: string): string {
  return value
    .replace(/(postgres(?:ql)?:\/\/)[^\s"']+/gi, "$1<redacted>")
    .replace(/(https?:\/\/)[^\s"']+/gi, "$1<redacted>")
    .replace(/[A-Za-z0-9_-]{80,}/g, "<redacted-token>")
    .slice(-2_000);
}

function run(
  command: string,
  args: readonly string[],
  cwd = REPOSITORY_ROOT,
  envOverrides: Readonly<Record<string, string>> = {},
): CommandResult {
  const result = spawnSync(command, [...args], {
    cwd,
    env: { ...process.env, ...envOverrides },
    encoding: "utf8",
    maxBuffer: 16 * 1024 * 1024,
    timeout: 120_000,
  });
  return {
    command: [command, ...args].join(" "),
    exitCode: result.status ?? 1,
    stdoutTail: redact(result.stdout ?? ""),
    stderrTail: redact(result.stderr ?? ""),
  };
}

function readBackyardSchema(): Readonly<{
  attempted: boolean;
  exitCode: number | null;
  names: string[];
  error: string | null;
}> {
  const databaseUrl = process.env.NEON_DATABASE_URL;
  if (!databaseUrl) {
    return { attempted: false, exitCode: null, names: [], error: "NEON_DATABASE_URL unavailable" };
  }
  let databaseEnvironment: NodeJS.ProcessEnv;
  try {
    const parsed = new URL(databaseUrl);
    if (parsed.protocol !== "postgres:" && parsed.protocol !== "postgresql:") {
      throw new Error("unsupported database URL protocol");
    }
    databaseEnvironment = {
      ...process.env,
      PGHOST: parsed.hostname,
      PGPORT: parsed.port || "5432",
      PGUSER: decodeURIComponent(parsed.username),
      PGPASSWORD: decodeURIComponent(parsed.password),
      PGDATABASE: decodeURIComponent(parsed.pathname.replace(/^\//, "")),
      PGSSLMODE: parsed.searchParams.get("sslmode") || "require",
    };
    const channelBinding = parsed.searchParams.get("channel_binding");
    if (channelBinding) databaseEnvironment.PGCHANNELBINDING = channelBinding;
  } catch {
    return { attempted: false, exitCode: null, names: [], error: "NEON_DATABASE_URL is invalid" };
  }
  const query = `
BEGIN READ ONLY;
SELECT 'constraint|' || constraint_name
FROM information_schema.table_constraints
WHERE table_schema = 'loyal_yield'
  AND table_name = 'multiply_operations'
  AND constraint_name IN (
    'multiply_operations_backyard_action_scope',
    'multiply_operations_backyard_lifecycle',
    'multiply_operations_backyard_submission_evidence',
    'multiply_operations_backyard_confirmation_evidence'
  )
UNION ALL
SELECT 'index|' || indexname
FROM pg_indexes
WHERE schemaname = 'loyal_yield'
  AND tablename = 'multiply_operations'
  AND indexname = 'multiply_operations_one_nonterminal_per_route';
COMMIT;
`;
  const result = spawnSync("psql", ["-X", "-A", "-t", "-v", "ON_ERROR_STOP=1"], {
    cwd: REPOSITORY_ROOT,
    // Keep credentials out of argv and verifier output. libpq reads them only
    // from this child environment.
    env: databaseEnvironment,
    input: query,
    encoding: "utf8",
    maxBuffer: 1024 * 1024,
    timeout: 30_000,
  });
  const names = (result.stdout ?? "")
    .split("\n")
    .map((value) => value.trim())
    .filter((value) => value.startsWith("constraint|") || value.startsWith("index|"))
    .sort();
  return {
    attempted: true,
    exitCode: result.status,
    names,
    error: result.status === 0 ? null : redact(result.stderr ?? "psql schema read failed"),
  };
}

function gitOutput(repository: string, args: readonly string[]): string | null {
  const result = spawnSync("git", [...args], {
    cwd: repository,
    env: process.env,
    encoding: "utf8",
    maxBuffer: 16 * 1024 * 1024,
    timeout: 30_000,
  });
  return result.status === 0 ? result.stdout : null;
}

function gitFilesAtRef(repository: string, ref: string, matches: RegExp): Readonly<{
  commit: string | null;
  files: string[];
  source: string;
}> {
  const commit = gitOutput(repository, ["rev-parse", "--verify", `${ref}^{commit}`])?.trim() ?? null;
  if (commit === null) return { commit: null, files: [], source: "" };
  const files = (gitOutput(repository, ["ls-tree", "-r", "--name-only", commit]) ?? "")
    .split("\n")
    .filter((path) => path.length > 0 && matches.test(path));
  const source = files.map((path) => gitOutput(repository, ["show", `${commit}:${path}`]) ?? "").join("\n");
  return { commit, files, source };
}

function filesUnder(root: string): string[] {
  const start = absolute(root);
  if (!existsSync(start)) return [];
  const rows: string[] = [];
  const visit = (directory: string) => {
    for (const entry of readdirSync(directory, { withFileTypes: true })) {
      if (entry.name === ".git" || entry.name === "node_modules" || entry.name === "target") continue;
      const path = join(directory, entry.name);
      if (entry.isDirectory()) visit(path);
      else if (entry.isFile()) rows.push(relative(REPOSITORY_ROOT, path));
    }
  };
  visit(start);
  return rows.sort();
}

function sourceText(paths: readonly string[]): string {
  return paths.filter((path) => existsSync(absolute(path))).map(read).join("\n");
}

function renderServiceBlocks(source: string, serviceName: string): string[] {
  const lines = source.split("\n");
  const blocks: string[] = [];
  for (let index = 0; index < lines.length; index += 1) {
    if (lines[index]?.trim() !== `name: ${serviceName}`) continue;
    let start = index;
    while (start >= 0 && !/^\s*- type:\s*/.test(lines[start] ?? "")) start -= 1;
    if (start < 0) continue;
    const indent = (lines[start]?.match(/^(\s*)/)?.[1] ?? "").length;
    let end = index + 1;
    while (end < lines.length) {
      const line = lines[end] ?? "";
      const lineIndent = line.match(/^(\s*)/)?.[1]?.length ?? 0;
      if (line.trim().length > 0 && lineIndent === indent && /^\s*- type:\s*/.test(line)) break;
      if (line.trim().length > 0 && lineIndent < indent) break;
      end += 1;
    }
    blocks.push(lines.slice(start, end).join("\n"));
  }
  return blocks;
}

function pass(id: string, condition: string, evidence: JsonRecord): Check {
  return { id, verdict: "PASS", condition, evidence, resumeCondition: null };
}

function fail(id: string, condition: string, evidence: JsonRecord, resumeCondition: string): Check {
  return { id, verdict: "FAIL", condition, evidence, resumeCondition };
}

function blocked(id: string, condition: string, evidence: JsonRecord, resumeCondition: string): Check {
  return { id, verdict: "BLOCKED", condition, evidence, resumeCondition };
}

function exactStringSet(value: unknown, expected: readonly string[]): boolean {
  if (!Array.isArray(value) || !value.every((entry) => typeof entry === "string")) return false;
  const observed = [...new Set(value)].sort();
  return observed.length === expected.length
    && observed.every((entry, index) => entry === [...expected].sort()[index]);
}

function record(value: unknown): JsonRecord | null {
  return value !== null && typeof value === "object" && !Array.isArray(value)
    ? value as JsonRecord
    : null;
}

function sha256Hex(value: unknown): value is string {
  return typeof value === "string" && /^[0-9a-f]{64}$/.test(value);
}

function nonnegativeInteger(value: unknown): value is number {
  return typeof value === "number" && Number.isSafeInteger(value) && value >= 0;
}

function canonicalBase64(value: unknown): Buffer | null {
  if (typeof value !== "string" || value.length === 0) return null;
  const decoded = Buffer.from(value, "base64");
  return decoded.length > 0 && decoded.toString("base64") === value ? decoded : null;
}

function base64MatchesSha256(value: unknown, expectedSha256: unknown): boolean {
  const decoded = canonicalBase64(value);
  return decoded !== null && sha256Hex(expectedSha256) && sha256(decoded) === expectedSha256;
}

function derivedPda(program: unknown, seeds: Buffer[]): string | null {
  if (typeof program !== "string") return null;
  try {
    return PublicKey.findProgramAddressSync(seeds, new PublicKey(program))[0].toBase58();
  } catch {
    return null;
  }
}

function derivedStrategyAuthority(program: unknown, vault: unknown, strategy: unknown): string | null {
  if (typeof vault !== "string" || typeof strategy !== "string") return null;
  try {
    return derivedPda(program, [
      Buffer.from("vault_strategy_auth"),
      new PublicKey(vault).toBuffer(),
      new PublicKey(strategy).toBuffer(),
    ]);
  } catch {
    return null;
  }
}

function derivedSquadsVault(program: unknown, settings: unknown, vaultIndex: unknown): string | null {
  if (typeof settings !== "string" || !nonnegativeInteger(vaultIndex) || vaultIndex > 255) return null;
  try {
    return derivedPda(program, [
      Buffer.from("smart_account"),
      new PublicKey(settings).toBuffer(),
      Buffer.from("smart_account"),
      Buffer.from([vaultIndex]),
    ]);
  } catch {
    return null;
  }
}

function localContractCheck(): Check {
  const planHash = sha256File(PLAN_PATH);
  const manifest = parseJson(MANIFEST_PATH);
  const verifierSource = read(relative(REPOSITORY_ROOT, fileURLToPath(import.meta.url)));
  const renderSource = sourceText([
    "render.yaml",
    "Dockerfile.laserstream-workers",
    "Dockerfile.light-workers",
    "Dockerfile.backyard-rwa-worker",
    ".github/workflows/backyard-rwa-worker-image.yml",
  ]);
  const goFiles = filesUnder(GO_ROOT).filter((path) => path.endsWith(".go"));
  const oldWriterCommands = [
    "backyard-voltr-earn-replay",
    "backyard-voltr-restoration-bridge",
    "backyard-voltr-restoration-readback",
    "tools/backyard-voltr/src/runtime/manager.ts",
  ].filter((needle) => renderSource.includes(needle));
  const manifestIdentities = record(manifest?.identities);
  const manifestPolicyCatalog = record(manifest?.policyCatalog);
  const manifestDeployment = record(manifest?.deployment);
  const checks = {
    planHashExact: planHash === PLAN_SHA256,
    manifestPresentAndV1: manifest?.schema === "loyal-backyard-rwa-manifest/v1",
    manifestNotProvisional: Array.isArray(manifest?.unresolved)
      && manifest.unresolved.length === 0
      && typeof manifestIdentities?.v2StrategyConfig === "string"
      && sha256Hex(manifestPolicyCatalog?.sha256)
      && manifestPolicyCatalog?.addressesResolved === true
      && typeof manifestDeployment?.sourceCommit === "string"
      && /^[0-9a-f]{40}$/.test(manifestDeployment.sourceCommit)
      && typeof manifestDeployment?.imageDigest === "string"
      && /^sha256:[0-9a-f]{64}$/.test(manifestDeployment.imageDigest)
      && typeof manifestDeployment?.singleWriterService === "string"
      && manifestDeployment.singleWriterService.length > 0,
    verifierSchemaExact: verifierSource.includes(SCHEMA),
    verifierBroadcastFalse: verifierSource.includes("broadcast: false"),
    goWorkerSourcePresent: goFiles.length > 0,
    noDeployedLegacyWriterCommand: oldWriterCommands.length === 0,
    fixedRoute: manifest?.mvpRoute === "PRIME/USDC",
    withdrawalWaitSeconds: manifest?.withdrawalWaitSeconds === 600,
  };
  const evidence = {
    checks,
    planPath: PLAN_PATH,
    planSha256: planHash,
    expectedPlanSha256: PLAN_SHA256,
    manifestPath: MANIFEST_PATH,
    manifestSha256: sha256File(MANIFEST_PATH),
    goFiles,
    deployedLegacyWriterMatches: oldWriterCommands,
  };
  return Object.values(checks).every(Boolean)
    ? pass("V01_contract_and_forbidden_surface", "Frozen v4 contract, manifest, Go source, and forbidden deployed-writer surface are exact.", evidence)
    : fail("V01_contract_and_forbidden_surface", "Frozen v4 contract, manifest, Go source, and forbidden deployed-writer surface are exact.", evidence,
      "Add/fix the checked-in v1 manifest and Go worker source without changing the frozen plan; remove any legacy deployed writer wiring.");
}

function adaptorCheck(): Check {
  const manifest = parseJson(MANIFEST_PATH);
  const identities = record(manifest?.identities);
  const source = sourceText([ADAPTOR_PROCESSOR, ADAPTOR_CONFIG]);
  const bridgeSource = sourceText([
    "tools/backyard-voltr/src/integrations/rwa-multiply-voltr.ts",
    "crates/loyal-actions/src/autonomous_vaults/voltr_custom.rs",
  ]);
  const simulation = parseJson(ADAPTOR_SIMULATION_EVIDENCE);
  const mutationRows = Array.isArray(simulation?.mutations) ? simulation.mutations : [];
  const mutationNames = mutationRows
    .map((value) => value !== null && typeof value === "object" && !Array.isArray(value)
      ? String((value as JsonRecord).name ?? "")
      : "")
  ;
  const requiredMutations = [
    "nonsigner_squads", "wrong_settings_or_index", "address_only_lookalike",
    "wrong_voltr_authority", "replayed_sequence", "skipped_sequence", "stale_slot",
    "future_slot", "oversized_nav", "trailing_bytes", "wrong_ata",
  ];
  const cargo = existsSync(absolute(ADAPTOR_MANIFEST))
    ? run("cargo", ["check", "--offline", "--manifest-path", ADAPTOR_MANIFEST], REPOSITORY_ROOT, {
      CARGO_TARGET_DIR: "/private/tmp/backyard-rwa-verifier-adaptor-target",
    })
    : null;
  const cargoTest = existsSync(absolute(ADAPTOR_MANIFEST))
    ? run("cargo", ["test", "--offline", "--manifest-path", ADAPTOR_MANIFEST, "--lib"], REPOSITORY_ROOT, {
      CARGO_TARGET_DIR: "/private/tmp/backyard-rwa-verifier-adaptor-target",
    })
    : null;
  const simulationBindings = record(simulation?.bindings);
  const canonicalSimulation = record(simulation?.simulation);
  const v2StrategyConfig = identities?.v2StrategyConfig;
  const expectedStrategyAuthority = derivedStrategyAuthority(
    identities?.voltrProgram,
    identities?.voltrVault,
    v2StrategyConfig,
  );
  const expectedSquadsVault = derivedSquadsVault(
    identities?.squadsProgram,
    identities?.squadsSettings,
    identities?.squadsVaultIndex,
  );
  const signerMetas = Array.isArray(simulation?.adaptorSignerMetas)
    ? simulation.adaptorSignerMetas.map(record).filter((row): row is JsonRecord => row !== null)
    : [];
  const expectedSignerMetaAddresses = [expectedStrategyAuthority, expectedSquadsVault]
    .filter((value): value is string => value !== null);
  const mutationTransactionHashes = mutationRows.map((value) => record(value)?.transactionSha256 ?? null);
  const mutationProofsExact = mutationRows.length === requiredMutations.length
    && mutationRows.every((value) => {
      const row = record(value);
      const rpc = record(row?.simulation);
      return row !== null
        && requiredMutations.includes(String(row.name))
        && base64MatchesSha256(row.transactionBase64, row.transactionSha256)
        && sha256Hex(row.logsSha256)
        && sha256Hex(row.preStateSha256)
        && row.preStateSha256 === row.postStateSha256
        && typeof row.error === "string"
        && row.error.length > 0
        && row.rejectedBeforeMutation === true
        && rpc?.sigVerify === true
        && rpc?.replaceRecentBlockhash === false
        && nonnegativeInteger(rpc?.contextSlot);
    })
    && new Set(mutationTransactionHashes).size === mutationTransactionHashes.length;
  const forbidden = [
    "refresh_reserve",
    "refresh_obligation",
    "flash_borrow",
    "flash_repay",
    "nav_reporter",
  ].filter((needle) => source.includes(needle));
  const checks = {
    sourcePresent: source.length > 0,
    reportV1: source.includes("ReportV1"),
    requiresSquadsSigner: source.includes("!accounts[6].is_signer"),
    requiresVoltrStrategySigner: source.includes("!accounts[0].is_signer"),
    derivesSquadsVault: source.includes("SQUADS_PREFIX") && source.includes("Pubkey::find_program_address"),
    exactSettingsType: source.includes("SQUADS_SETTINGS_DISCRIMINATOR")
      && source.includes("valid_settings_authority_graph")
      && source.includes("signer_count != 1"),
    exactVoltrProgram: source.includes("VOLTR_PROGRAM_ID")
      && source.includes("voltr_program.key != &VOLTR_PROGRAM_ID"),
    hasSequence: source.includes("sequence"),
    hasObservedSlot: source.includes("observed_slot"),
    hasNavAfterRaw: source.includes("nav_after_raw"),
    hasSnapshotDigest: source.includes("snapshot_digest"),
    rejectsTrailingBytes: source.includes("data.len() != 8 + REPORT_V1_LEN")
      && source.includes("input.len() != REPORT_V1_LEN"),
    noEconomicRouteSurface: forbidden.length === 0,
    writableConfigForwarded: bridgeSource.includes("writableStrategy")
      && bridgeSource.includes("readonlySigner(route.squads.vault)"),
    canonicalSignedUnsentSimulation: simulation?.schema === "loyal-backyard-rwa-adaptor-simulation/v1"
      && simulation?.broadcast === false
      && simulation?.signedUnsent === true
      && simulation?.path === "Squads->Voltr->adaptor"
      && simulation?.success === true
      && simulation?.cluster === "mainnet-beta"
      && simulation?.genesisHash === manifest?.genesisHash
      && simulation?.commitment === "confirmed"
      && simulation?.manifestSha256 === sha256File(MANIFEST_PATH)
      && sha256Hex(simulation?.programElfSha256)
      && sha256Hex(simulation?.programDataSha256)
      && sha256Hex(simulation?.configDataSha256)
      && base64MatchesSha256(simulation?.transactionBase64, simulation?.transactionSha256)
      && sha256Hex(simulation?.messageSha256)
      && canonicalSimulation?.sigVerify === true
      && canonicalSimulation?.replaceRecentBlockhash === false
      && canonicalSimulation?.err === null
      && nonnegativeInteger(canonicalSimulation?.contextSlot),
    bindingsMatchFrozenManifest: expectedStrategyAuthority !== null
      && expectedSquadsVault !== null
      && expectedSquadsVault === identities?.squadsVault
      && simulationBindings?.voltrProgram === identities?.voltrProgram
      && simulationBindings?.voltrVault === identities?.voltrVault
      && simulationBindings?.strategyConfig === v2StrategyConfig
      && simulationBindings?.strategyAuthority === expectedStrategyAuthority
      && simulationBindings?.adaptorProgram === identities?.adaptorProgram
      && simulationBindings?.squadsProgram === identities?.squadsProgram
      && simulationBindings?.squadsSettings === identities?.squadsSettings
      && simulationBindings?.squadsVaultIndex === identities?.squadsVaultIndex
      && simulationBindings?.squadsVault === expectedSquadsVault
      && simulationBindings?.delegatedExecutor === identities?.delegatedExecutor
      && simulationBindings?.squadsAssetAta === identities?.squadsUsdcAta,
    exactAdaptorSignerMetas: exactStringSet(
      signerMetas.filter((row) => row.isSigner === true).map((row) => String(row.address ?? "")),
      expectedSignerMetaAddresses,
    ) && signerMetas.length === 2,
    allNegativeMutations: exactStringSet(mutationNames, requiredMutations)
      && mutationProofsExact,
    // Checked-in RPC responses are caller-authored evidence. A future PASS
    // must re-decode and re-simulate these exact fresh signed bytes through a
    // read-only RPC call made by this command.
    independentCurrentSimulation: false,
    cargoCheck: cargo?.exitCode === 0,
    cargoTest: cargoTest?.exitCode === 0,
  };
  const evidence = {
    checks,
    processorSha256: sha256File(ADAPTOR_PROCESSOR),
    configSha256: sha256File(ADAPTOR_CONFIG),
    forbidden,
    cargo,
    cargoTest,
    signerTopologySimulationPath: ADAPTOR_SIMULATION_EVIDENCE,
    signerTopologySimulationSha256: sha256File(ADAPTOR_SIMULATION_EVIDENCE),
  };
  return Object.values(checks).every(Boolean)
    ? pass("V02_adaptor_identity_and_signer", "Adaptor v2 source/bytes require exact Squads and Voltr PDA signer authority with replay-safe ReportV1.", evidence)
    : fail("V02_adaptor_identity_and_signer", "Adaptor v2 source/bytes require exact Squads and Voltr PDA signer authority with replay-safe ReportV1.", evidence,
      "Implement the smallest compatible adaptor v2 contract, pass cargo check, then attach the canonical signed-unsent Squads -> Voltr -> adaptor simulation.");
}

function policyCatalogCheck(): Check {
  const catalog = parseJson(POLICY_CATALOG_PATH);
  const simulation = parseJson(POLICY_SIMULATION_EVIDENCE);
  const lanes = Array.isArray(catalog?.lanes) ? catalog.lanes : [];
  const operations = Array.isArray(catalog?.operations) ? catalog.operations : [];
  const swapEdges = Array.isArray(catalog?.swapEdges) ? catalog.swapEdges : [];
  const packing = catalog?.packing !== null && typeof catalog?.packing === "object"
    ? catalog.packing as JsonRecord
    : null;
  const laneKeys = lanes.map((lane) => {
    if (lane === null || typeof lane !== "object" || Array.isArray(lane)) return "";
    const row = lane as JsonRecord;
    return [row.market, row.collateral, row.debt].join("/");
  });
  const expectedLanes = [
    "OnRe/ONyc/USDC", "OnRe/ONyc/USDG", "OnRe/ONyc/USDS",
    "Prime/PRIME/USDC", "Prime/PRIME/PYUSD", "Prime/PRIME/USDS",
    "Maple/syrupUSDC/USDC", "Maple/syrupUSDC/USDG", "Maple/syrupUSDC/PYUSD",
    "AUTO/AUTO/PYUSD", "Ethena/USDe/PYUSD",
  ];
  const packetRows = Array.isArray(packing?.packets) ? packing?.packets : [];
  const semanticSourcePath = "crates/loyal-actions/src/backyard_policy_catalog.rs";
  const semanticSource = existsSync(absolute(semanticSourcePath)) ? read(semanticSourcePath) : "";
  const compilerSourcePath = "crates/loyal-actions/src/bin/compile_backyard_rwa_policy_catalog.rs";
  const compilerSource = existsSync(absolute(compilerSourcePath)) ? read(compilerSourcePath) : "";
  const checks = {
    semanticSourcePresent: semanticSource.includes("create_market_policies")
      && semanticSource.includes("create_swap_policy")
      && semanticSource.includes("BEST_CASE_PHYSICAL_POLICY_COUNT"),
    noFalseReadyCompiler: compilerSource.length > 0
      && !compilerSource.includes("READY_FOR_POLICY_INSTALLATION"),
    catalogSchema: catalog?.schema === "loyal-backyard-rwa-policy-catalog/v1",
    lanesExact: exactStringSet(laneKeys, expectedLanes),
    operationsExact: operations.length === 44,
    swapEdgesExact: swapEdges.length === 52,
    noDuplicateLanes: new Set(laneKeys).size === laneKeys.length,
    noDuplicateSwapEdges: new Set(swapEdges.map((edge) => JSON.stringify(edge))).size === swapEdges.length,
    packetMeasurementsPresent: packetRows.length >= 8,
    everyPacketFits: packetRows.length > 0 && packetRows.every((row) => row !== null
      && typeof row === "object"
      && !Array.isArray(row)
      && typeof (row as JsonRecord).bytes === "number"
      && ((row as JsonRecord).bytes as number) <= 1_232),
    currentAddressesResolved: catalog?.addressesResolved === true,
    groupedSignedUnsentSimulation: simulation?.schema === "loyal-backyard-rwa-policy-simulation/v1"
      && simulation?.broadcast === false
      && simulation?.signedUnsent === true
      && exactStringSet(simulation?.groups, [
        "three-lane-markets", "singleton-markets", "swap-graph", "bridge-lifecycle",
      ]),
    negativeCasesExact: exactStringSet(simulation?.negativeCases, [
      "same-mint-wrong-reserve", "cross-lane-obligation", "unapproved-edge",
      "extra-instruction", "amount-cap-breach", "signer-substitution",
      "writable-role-substitution",
    ]),
    // As in V02, files can carry the wires but cannot attest their own RPC
    // results. The sole verifier must re-simulate fresh signed group/mutation
    // wires before this gate can pass.
    independentCurrentSimulation: false,
  };
  const evidence = {
    checks,
    catalogPath: POLICY_CATALOG_PATH,
    catalogSha256: sha256File(POLICY_CATALOG_PATH),
    observedLaneKeys: laneKeys,
    operationCount: operations.length,
    swapEdgeCount: swapEdges.length,
    packetCount: packetRows.length,
    unresolved: catalog?.unresolved ?? ["catalog missing"],
    semanticSourceSha256: sha256File(semanticSourcePath),
    compilerSourceSha256: sha256File(compilerSourcePath),
    simulationPath: POLICY_SIMULATION_EVIDENCE,
    simulationSha256: sha256File(POLICY_SIMULATION_EVIDENCE),
  };
  return Object.values(checks).every(Boolean)
    ? pass("V03_catalog_semantics_and_packing", "Catalog is the exact 11-lane, 44-operation, 52-edge, first-safe-packet-fitting authority set.", evidence)
    : fail("V03_catalog_semantics_and_packing", "Catalog is the exact 11-lane, 44-operation, 52-edge, first-safe-packet-fitting authority set.", evidence,
      "Resolve current confirmed route identities, compile the exact correlated lane constraints, and measure the first safe packing rung with full signed packets.");
}

function goWorkerCheck(): Check {
  const goFiles = filesUnder(GO_ROOT).filter((path) => path.endsWith(".go"));
  const source = sourceText(goFiles);
  const workerSource = sourceText([join(GO_ROOT, "internal/backyardrwa/worker.go")]);
  const receiptObserveSource = sourceText([
    join(GO_ROOT, "internal/backyardrwa/observe.go"),
    join(GO_ROOT, "internal/backyardrwa/voltr_observe.go"),
  ]);
  const kaminoObserveSource = sourceText([join(GO_ROOT, "internal/backyardrwa/kamino_observe.go")]);
  const bridgeBuildSource = sourceText([
    join(GO_ROOT, "internal/backyardrwa/build.go"),
    join(GO_ROOT, "internal/backyardrwa/bridge_runtime.go"),
  ]);
  const kaminoBuildSource = sourceText([join(GO_ROOT, "internal/backyardrwa/kamino_build.go")]);
  const goTest = existsSync(absolute(join(GO_ROOT, "go.mod")))
    ? run("go", ["test", "./..."], absolute(GO_ROOT), {
      GOCACHE: "/private/tmp/backyard-rwa-verifier-go-cache",
    })
    : null;
  const goVet = existsSync(absolute(join(GO_ROOT, "go.mod")))
    ? run("go", ["vet", "./..."], absolute(GO_ROOT), {
      GOCACHE: "/private/tmp/backyard-rwa-verifier-go-cache",
    })
    : null;
  const migrations = filesUnder("crates/loyal-yield-store/migrations")
    .filter((path) => /backyard.*rwa|rwa.*backyard/i.test(path));
  const schemaReadback = readBackyardSchema();
  const expectedSchemaNames = [
    "constraint|multiply_operations_backyard_action_scope",
    "constraint|multiply_operations_backyard_confirmation_evidence",
    "constraint|multiply_operations_backyard_lifecycle",
    "constraint|multiply_operations_backyard_submission_evidence",
    "index|multiply_operations_one_nonterminal_per_route",
  ];
  const forbidden = [
    "optimizer",
    "route switch",
    "RouteSwitch",
    "saga",
    "outbox",
  ].filter((needle) => source.toLowerCase().includes(needle.toLowerCase()));
  const requiredActions = [
    "HOLD",
    "RECOVER_TRANSACTION",
    "VOLTR_ALLOCATE_TO_SQUADS",
    "OPEN_PRIME_USDC_STEP",
    "DELEVER_PRIME_USDC_STEP",
    "STAGE_SQUADS_TO_VOLTR",
    "VOLTR_RESTORE_IDLE",
    "REPORT_NAV",
  ];
  const checks = {
    modulePresent: existsSync(absolute(join(GO_ROOT, "go.mod"))),
    oneCommand: filesUnder(join(GO_ROOT, "cmd")).filter((path) => path.endsWith("main.go")).length === 1,
    fixedPrimeUsdc: source.includes("PRIME/USDC"),
    requiredActions: requiredActions.every((action) => source.includes(action)),
    oneNonterminalInvariant: source.includes("one nonterminal") || source.includes("one_nonterminal") || source.includes("oneNonterminal"),
    persistedBeforeSend: source.includes("broadcast_intent") || source.includes("broadcast intent"),
    noForbiddenRuntimeSurface: forbidden.length === 0,
    migrationReusesExistingTables: migrations.length === 1
      && sourceText(migrations).includes("multiply_route_states")
      && sourceText(migrations).includes("multiply_operations")
      && source.includes("multiply_position_snapshots"),
    runtimeNotDisabled: !source.includes("disabled until concrete deployment wiring"),
    tickRunsObservationDecisionAndBuild: workerSource.includes("Observe")
      && workerSource.includes("Decide(")
      && workerSource.includes("RecordDecision")
      && workerSource.includes("BuildSimulateAndPersistBridge")
      && workerSource.includes("BuildSimulateAndPersistKamino"),
    liveReceiptAndKaminoObserver: /func\s+\w*(Observe|observe|Scan)\w*\s*\(/.test(receiptObserveSource)
      && /withdraw(al)?\s*(receipt|request)/i.test(receiptObserveSource)
      && /GetMultipleAccounts|minContextSlot/.test(receiptObserveSource)
      && /func\s+\w*(Observe|observe|Decode|decode)Kamino\w*\s*\(/.test(kaminoObserveSource)
      && /obligation/i.test(kaminoObserveSource)
      && /reserve/i.test(kaminoObserveSource)
      && /oracle/i.test(kaminoObserveSource)
      && /GetMultipleAccounts|minContextSlot/.test(kaminoObserveSource),
    concreteActionTransactionBuilder: /func\s+\w*(Build|build)\w*Transaction\w*\s*\(/.test(bridgeBuildSource)
      && bridgeBuildSource.includes("BuildSimulateAndPersistBridge")
      && /func\s+\w*(Build|build)\w*(Kamino|Prime)\w*\s*\(/.test(kaminoBuildSource)
      && kaminoBuildSource.includes("BuildSimulateAndPersistKamino"),
    concreteSigner: source.includes("crypto/ed25519")
      && source.includes("ed25519.Sign")
      && source.includes("SignedWire"),
    concreteLifecycleContracts: source.includes("type Observation")
      && source.includes("type BuildResult")
      && source.includes("type Reconciliation"),
    noAbstractRuntimeLayer: !/type\s+\w*(Runtime|Store|RPC|Signer)\s+interface\s*\{/.test(source),
    concreteDatabase: source.includes("github.com/jackc/pgx")
      && source.includes("multiply_operations")
      && source.includes("FOR UPDATE"),
    concreteConfirmedRpc: source.includes("getMultipleAccounts")
      && source.includes("minContextSlot")
      && source.includes("confirmed"),
    concreteTransactionLifecycle: source.includes("simulateTransaction")
      && source.includes("sendTransaction")
      && source.includes("getSignatureStatuses"),
    independentSchemaIntrospection: schemaReadback.exitCode === 0
      && JSON.stringify(schemaReadback.names) === JSON.stringify(expectedSchemaNames),
    goTest: goTest?.exitCode === 0,
    goVet: goVet?.exitCode === 0,
  };
  const evidence = {
    checks,
    goFiles,
    goTest,
    goVet,
    migrations,
    migrationSha256: migrations.length === 1 ? sha256File(migrations[0]!) : null,
    schemaReadback,
    missingConcreteCapabilities: {
      observationDecisionBuild: !(workerSource.includes("BuildSimulateAndPersistBridge")
        && workerSource.includes("BuildSimulateAndPersistKamino")),
      receiptKaminoObservation: kaminoObserveSource.length === 0,
      actionTransactionConstruction: kaminoBuildSource.length === 0,
      signing: !(source.includes("crypto/ed25519") && source.includes("ed25519.Sign")),
      deployedSchemaIntrospection: true,
    },
    forbidden,
  };
  return Object.values(checks).every(Boolean)
    ? pass("V04_go_state_machine_and_store", "One concrete serialized Go worker and narrow existing-table migration pass focused tests.", evidence)
    : fail("V04_go_state_machine_and_store", "One concrete serialized Go worker and narrow existing-table migration pass focused tests.", evidence,
      "Implement/fix the fixed-route Go state machine, persistence ordering, NAV logic, existing-table migration, and focused tests.");
}

function deploymentCheck(prerequisitesPass: boolean): Check {
  const renderSource = sourceText(["render.yaml"]);
  const imageBuildSource = sourceText([
    "Dockerfile.backyard-rwa-worker",
    ".github/workflows/backyard-rwa-worker-image.yml",
  ]);
  const backyardServices = renderServiceBlocks(renderSource, "loyal-backyard-rwa-worker");
  const backyardService = backyardServices.length === 1 ? backyardServices[0] ?? "" : "";
  const pinnedImagePattern = /ghcr\.io\/loyal-labs\/loyal-yield-routing\/backyard-rwa-worker:sha-[0-9a-f]{40}(?:\s|$)/;
  const evidence = parseJson(DEPLOYMENT_EVIDENCE);
  const checks = {
    imageBuildWired: imageBuildSource.includes("Dockerfile.backyard-rwa-worker")
      && imageBuildSource.includes("backyard-rwa-worker:sha-${{ github.sha }}")
      && imageBuildSource.includes("/usr/local/bin/backyard-rwa-worker"),
    exactlyOneGoService: backyardServices.length === 1,
    goServiceWired: /^\s*- type:\s*worker\s*$/m.test(backyardService)
      && /^\s*runtime:\s*image\s*$/m.test(backyardService),
    goCommandDirect: /^\s*dockerCommand:\s*\/usr\/local\/bin\/backyard-rwa-worker\s*$/m.test(backyardService),
    immutableGhcrImage: pinnedImagePattern.test(backyardService),
    noLegacyWriter: !renderSource.includes("backyard-voltr-earn-replay")
      && !renderSource.includes("backyard-voltr-restoration-bridge")
      && !renderSource.includes("backyard-voltr-restoration-readback"),
    deploymentEvidence: evidence?.schema === "loyal-backyard-rwa-deployment/v1"
      && evidence?.singleWriter === true
      && typeof evidence?.imageDigest === "string"
      && typeof evidence?.sourceCommit === "string",
    // A checked-in JSON assertion is not deployment truth. This becomes true
    // only when the verifier performs the Render/service and database-lease
    // reads itself and binds them to the service block and image digest above.
    independentDeploymentRead: false,
  };
  const row = {
    checks,
    renderServiceCount: backyardServices.length,
    renderServiceSha256: backyardService.length > 0 ? sha256(backyardService) : null,
    imageBuildSourceSha256: imageBuildSource.length > 0 ? sha256(imageBuildSource) : null,
    deploymentEvidencePath: DEPLOYMENT_EVIDENCE,
    deploymentEvidenceSha256: sha256File(DEPLOYMENT_EVIDENCE),
  };
  if (prerequisitesPass && checks.imageBuildWired && checks.exactlyOneGoService
    && checks.goServiceWired && checks.goCommandDirect && checks.immutableGhcrImage && checks.noLegacyWriter) {
    return blocked("V05_deployed_single_writer", "Exactly one immutable Go deployment owns the route and no old writer can claim it.", row,
      "Query Render and the database lease read-only inside this verifier, bind the live service/image/route owner to the checked-in source, and prove no competing recent writer.");
  }
  return fail("V05_deployed_single_writer", "Exactly one immutable Go deployment owns the route and no old writer can claim it.", row,
    "Add the pinned Go service/image wiring and remove legacy route ownership before requesting deployment approval.");
}

function lifecycleCheck(deploymentPass: boolean): Check {
  const evidence = parseJson(LIFECYCLE_EVIDENCE);
  const requiredSteps = [
    "deposit", "allocate", "open", "nav", "withdraw_request",
    "unwind", "restore", "predeadline_rejection", "claim", "conservation",
  ];
  const steps = Array.isArray(evidence?.steps) ? evidence.steps : [];
  const observedNames = steps.map((step) => step !== null && typeof step === "object" && !Array.isArray(step)
    ? String((step as JsonRecord).name ?? "")
    : "");
  const checks = {
    schema: evidence?.schema === "loyal-backyard-rwa-live-lifecycle/v1",
    confirmed: evidence?.commitment === "confirmed",
    realBroadcast: evidence?.broadcast === true,
    exactWait: evidence?.withdrawalWaitSeconds === 600,
    allSteps: exactStringSet(observedNames, requiredSteps),
    independentReconciliation: evidence?.independentReconciliation === true,
    conserved: evidence?.conserved === true,
    // Lifecycle JSON is an input to comparison, never the authority. This
    // becomes true only after this command independently fetches and decodes
    // every signature, account delta, receipt, NAV config, and operation row.
    independentChainAndDatabaseRead: false,
  };
  const row = {
    checks,
    evidencePath: LIFECYCLE_EVIDENCE,
    evidenceSha256: sha256File(LIFECYCLE_EVIDENCE),
    observedSteps: observedNames,
  };
  if (deploymentPass) {
    return blocked("V06_live_internal_lifecycle", "One real confirmed internal deposit-to-claim lifecycle is independently reconciled.", row,
      "Obtain transaction-specific approval for one real internal Backyard lifecycle, then make this verifier independently fetch its confirmed signatures, raw account/receipt deltas, operation rows, and conservation equation.");
  }
  return fail("V06_live_internal_lifecycle", "One real confirmed internal deposit-to-claim lifecycle is independently reconciled.", row,
    "Complete V05 before requesting approval for the real internal lifecycle.");
}

function adminCheck(): Check {
  const main = existsSync(APPS_ROOT)
    ? gitFilesAtRef(APPS_ROOT, "refs/remotes/origin/main", /vault.*integration|backyard.*vault/i)
    : { commit: null, files: [], source: "" };
  const source = main.source;
  const required = ["AUM", "NAV", "APY", "Voltr", "Squads", "LTV", "withdraw"];
  const checks = {
    appsRepositoryPresent: existsSync(APPS_ROOT),
    originMainResolved: typeof main.commit === "string" && /^[0-9a-f]{40}$/.test(main.commit),
    pagePresentOnOriginMain: main.files.some((path) => /page\.(tsx|ts)$/.test(path)),
    requiredFields: required.every((value) => source.toLowerCase().includes(value.toLowerCase())),
    readOnly: !/onClick[^\n]{0,100}(submit|execute|withdraw|deposit)|useMutation|mutationFn/s.test(source),
    // Source presence is not display truth. A PASS additionally requires this
    // verifier to read the deployed page plus its RPC/database sources at one
    // explicit freshness boundary and compare their raw values itself.
    independentDeployedTruthComparison: false,
  };
  const evidence = {
    checks,
    appsRoot: APPS_ROOT,
    sourceRef: "refs/remotes/origin/main",
    sourceCommit: main.commit,
    candidateFiles: main.files,
  };
  return fail("V07_admin_macroview_truth", "The thin read-only Vault integrations page exposes all required operating fields.", evidence,
    "Land the minimum read-only page on loyal-apps origin/main, deploy that exact commit, then compare displayed values with independent RPC/database reads inside this verifier.");
}

function main() {
  const sourceCommitResult = run("git", ["rev-parse", "HEAD"]);
  const checks: Check[] = [];
  const v01 = localContractCheck();
  checks.push(v01);
  checks.push(adaptorCheck());
  checks.push(policyCatalogCheck());
  checks.push(goWorkerCheck());
  const localPass = checks.every((check) => check.verdict === "PASS");
  const v05 = deploymentCheck(localPass);
  checks.push(v05);
  checks.push(lifecycleCheck(v05.verdict === "PASS"));
  checks.push(adminCheck());

  const firstFailure = checks.find((check) => check.verdict === "FAIL") ?? null;
  const blocker = firstFailure === null
    ? checks.find((check) => check.verdict === "BLOCKED") ?? null
    : null;
  const verdict: Verdict = firstFailure !== null ? "FAIL" : blocker !== null ? "BLOCKED" : "PASS";
  const manifest = parseJson(MANIFEST_PATH);
  const policyCatalog = parseJson(POLICY_CATALOG_PATH);
  const output = {
    schema: SCHEMA,
    verdict,
    broadcast: false,
    commitment: "confirmed",
    sourceCommit: sourceCommitResult.exitCode === 0 ? sourceCommitResult.stdoutTail.trim() : null,
    deployedImageDigest: parseJson(DEPLOYMENT_EVIDENCE)?.imageDigest ?? null,
    manifestSha256: sha256File(MANIFEST_PATH),
    policyCatalogSha256: sha256File(POLICY_CATALOG_PATH),
    manifestSchema: manifest?.schema ?? null,
    policyCatalogSchema: policyCatalog?.schema ?? null,
    evidenceLayers: {
      static: ["V01", "V02", "V03", "V04"],
      simulation: ["V02", "V03"],
      submission: ["V06"],
      confirmation: ["V06"],
      deployment: ["V05", "V07"],
      reconciliation: ["V06", "V07"],
      live: ["V06"],
    },
    checks,
    firstFailure,
    blocker,
    resumeCommand: SOLE_COMMAND,
  };
  console.log(JSON.stringify(output, null, 2));
  process.exitCode = verdict === "PASS" ? 0 : verdict === "FAIL" ? 1 : 2;
}

try {
  main();
} catch (error) {
  const message = error instanceof Error ? redact(error.message) : redact(String(error));
  console.log(JSON.stringify({
    schema: SCHEMA,
    verdict: "FAIL",
    broadcast: false,
    commitment: "confirmed",
    firstFailure: {
      id: "VERIFIER_INTERNAL_ERROR",
      verdict: "FAIL",
      condition: "The sole verifier must evaluate the contract without hidden temporary prerequisites.",
      evidence: { message },
      resumeCondition: "Fix the verifier defect and rerun the sole command.",
    },
    blocker: null,
    resumeCommand: SOLE_COMMAND,
  }, null, 2));
  process.exitCode = 1;
}
