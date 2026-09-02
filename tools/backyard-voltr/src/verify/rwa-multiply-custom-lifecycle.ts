import { createHash } from "node:crypto";
import { existsSync, readFileSync, readdirSync } from "node:fs";
import { homedir } from "node:os";
import { dirname, join, relative, resolve } from "node:path";
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";
import { Connection, PublicKey, VersionedTransaction } from "@solana/web3.js";
import bs58 from "bs58";

import { RWA_MULTIPLY_ROUTE, rwaMultiplyRouteSpecSha256 } from "../domain/rwa-multiply-route-spec.js";
import { verifyInstalledCustomPolicies } from "../policies/rwa-multiply-custom.js";
import {
  validateV06Lifecycle,
  type V06ChainRead,
  type V06ChainTransaction,
  type V06DatabaseRead,
  type V06FinalAccountEvidence,
  type V06LifecycleEvidence,
  type V06RouteBindings,
  type V06TokenBalance,
} from "./rwa-multiply-custom-lifecycle-v06.js";

const REPOSITORY_ROOT = resolve(fileURLToPath(new URL("../../../..", import.meta.url)));
const APPS_ROOT = resolve(REPOSITORY_ROOT, "../loyal-apps");
const SCHEMA = "loyal-backyard-rwa-go-lifecycle/v3";
const PLAN_PATH = "docs/plans/backyard-voltr-orchestrator-verifier.md";
const PLAN_SHA256 = "5665423e9b9003bc34e4d6f8389eeb8fabf5ec140596271a787abc2e00486b0c";
const MANIFEST_PATH = "docs/manifests/backyard-rwa-v1.json";
const POLICY_CATALOG_PATH = "crates/loyal-actions/fixtures/backyard_rwa_policy_catalog_v1.json";
const ADAPTOR_MANIFEST = "crates/loyal-voltr-rwa-nav-adaptor/Cargo.toml";
const ADAPTOR_PROCESSOR = "crates/loyal-voltr-rwa-nav-adaptor/src/processor.rs";
const ADAPTOR_CONFIG = "crates/loyal-voltr-rwa-nav-adaptor/src/config.rs";
const GO_ROOT = "go/backyard-rwa-worker";
const DEPLOYMENT_EVIDENCE = "docs/evidence/backyard-rwa-go/deployment-v1.json";
const LIFECYCLE_EVIDENCE = "docs/evidence/backyard-rwa-go/lifecycle-v1.json";
const ADAPTOR_SIMULATION_EVIDENCE = "docs/evidence/backyard-rwa-go/adaptor-v2-ticket-simulation-v2.json";
const POLICY_SIMULATION_EVIDENCE = "docs/evidence/backyard-rwa-go/policy-catalog-simulation-v1.json";
const SOLE_COMMAND = "bun run --cwd tools/backyard-voltr verify:rwa-multiply-custom-lifecycle";
const BRIDGE_POLICY_ROUTE_SPEC_SHA256 = "6482b284172cd2b2da0317f9b33db737688d60cfe61f6b28c68da5ddbfc19550";
const BRIDGE_POLICY_ROLLOVER = [
  { action: "VOLTR_ALLOCATE_TO_SQUADS", operation: "allocation", seed: 62n,
    account: "HoDV7mtsb2u1VARZLYuGByW7cCsGWL9NFxHZs7WHjdzz",
    dataSha256: "bda72932f474064fa3cd60ce91633acba35b2730e86b82f4352aa96a6738e2f4" },
  { action: "REPORT_NAV", operation: "nav-refresh", seed: 63n,
    account: "41nzu42c3KPgJfWhnV5jbfxjHbvVU6HXaiJmzzYNqvBP",
    dataSha256: "bf34a3e9c9c635c79a0d30e096b639a86d52e300ad113c81161e3486832d97ca" },
  { action: "STAGE_SQUADS_TO_VOLTR", operation: "stage-withdrawal", seed: 64n,
    account: "ALz5Wkt82GhGFH1LfzbnAovkZ6t85ErovbxHUH3yY1wY",
    dataSha256: "ef8c231497fb2620b5930cfe5d329c871f103db6512781eb5487534db8b1291b" },
  { action: "VOLTR_RESTORE_IDLE", operation: "withdraw", seed: 65n,
    account: "DjYYkQWb4zYbySfEndjVdg2NwZ8i77Fb9P1UFVbebc5t",
    dataSha256: "84e8f6f881758cff1714ef743603c016024104f9834392c6fba693c3651b719c" },
] as const;
const RETIRED_BRIDGE_POLICY_SEEDS = [53n, 54n, 55n, 56n] as const;

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

type JsonCommandResult = Readonly<{
  attempted: boolean;
  exitCode: number | null;
  value: unknown;
  error: string | null;
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

function runJson(command: string, args: readonly string[], cwd = REPOSITORY_ROOT): JsonCommandResult {
  const result = spawnSync(command, [...args], {
    cwd,
    env: process.env,
    encoding: "utf8",
    maxBuffer: 16 * 1024 * 1024,
    timeout: 60_000,
  });
  if (result.error) {
    return { attempted: false, exitCode: null, value: null, error: redact(result.error.message) };
  }
  if (result.status !== 0) {
    return {
      attempted: true,
      exitCode: result.status,
      value: null,
      error: redact(result.stderr ?? `${command} exited nonzero`),
    };
  }
  try {
    return {
      attempted: true,
      exitCode: 0,
      value: JSON.parse(result.stdout || "null") as unknown,
      error: null,
    };
  } catch {
    return { attempted: true, exitCode: 0, value: null, error: `${command} returned non-JSON output` };
  }
}

function databaseEnvironment(): NodeJS.ProcessEnv | null {
  const databaseUrl = process.env.NEON_DATABASE_URL;
  if (!databaseUrl) return null;
  try {
    const parsed = new URL(databaseUrl);
    if (parsed.protocol !== "postgres:" && parsed.protocol !== "postgresql:") return null;
    const environment: NodeJS.ProcessEnv = {
      ...process.env,
      PGHOST: parsed.hostname,
      PGPORT: parsed.port || "5432",
      PGUSER: decodeURIComponent(parsed.username),
      PGPASSWORD: decodeURIComponent(parsed.password),
      PGDATABASE: decodeURIComponent(parsed.pathname.replace(/^\//, "")),
      PGSSLMODE: parsed.searchParams.get("sslmode") || "require",
    };
    const channelBinding = parsed.searchParams.get("channel_binding");
    if (channelBinding) environment.PGCHANNELBINDING = channelBinding;
    return environment;
  } catch {
    return null;
  }
}

function readOnlyDatabaseJson(query: string): JsonCommandResult {
  const environment = databaseEnvironment();
  if (!environment) {
    return { attempted: false, exitCode: null, value: null, error: "NEON_DATABASE_URL unavailable or invalid" };
  }
  const result = spawnSync("psql", ["-X", "-A", "-t", "-v", "ON_ERROR_STOP=1"], {
    cwd: REPOSITORY_ROOT,
    env: environment,
    input: `BEGIN READ ONLY;\n${query}\nCOMMIT;\n`,
    encoding: "utf8",
    maxBuffer: 2 * 1024 * 1024,
    timeout: 30_000,
  });
  if (result.error) {
    return { attempted: false, exitCode: null, value: null, error: redact(result.error.message) };
  }
  if (result.status !== 0) {
    return { attempted: true, exitCode: result.status, value: null, error: redact(result.stderr ?? "psql read failed") };
  }
  const payload = (result.stdout ?? "")
    .split("\n")
    .map((value) => value.trim())
    .find((value) => value.startsWith("{") && value.endsWith("}"));
  if (!payload) return { attempted: true, exitCode: 0, value: null, error: "database read returned no JSON row" };
  try {
    return { attempted: true, exitCode: 0, value: JSON.parse(payload) as unknown, error: null };
  } catch {
    return { attempted: true, exitCode: 0, value: null, error: "database read returned malformed JSON" };
  }
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

function signedTransactionSignature(value: unknown): string | null {
  const wire = canonicalBase64(value);
  if (wire === null) return null;
  try {
    const signature = VersionedTransaction.deserialize(wire).signatures[0];
    return signature && signature.some((byte) => byte !== 0) ? bs58.encode(signature) : null;
  } catch {
    return null;
  }
}

type SimulationExpectation = "success" | "failure" | "arm-only-success";

type ExpectedArmOnlyTransition = Readonly<{
  ticketAddress: string;
  configAddress: string;
  lastConsumedSequence: string;
  activeSequence: string;
  activeWireSha256: string;
}>;

type SignedSimulationRow = Readonly<{
  name: string;
  expectation: SimulationExpectation;
  transactionBase64: unknown;
  transactionSha256: unknown;
  inspectedAddresses: unknown;
  logsSha256: unknown;
  expectedArmOnlyTransition?: ExpectedArmOnlyTransition | undefined;
}>;

type IndependentSimulationResult = Readonly<{
  name: string;
  expectation: SimulationExpectation;
  transactionSha256: string | null;
  validInput: boolean;
  contextSlot: number | null;
  errIsNull: boolean | null;
  blockhashExpired: boolean;
  logsSha256: string | null;
  logsMatchEvidence: boolean;
  simulationPostAccountsAvailable: boolean | null;
  simulationNullAddresses: readonly string[];
  simulationChangedAddresses: readonly string[];
  simulationStateUnchanged: boolean | null;
  armOnlyTicketTransitionExact: boolean | null;
  chainReadbackContextSlot: number | null;
  chainReadbackStateSha256: string | null;
  signatureStatusIsNull: boolean | null;
  stateUnchanged: boolean | null;
  passed: boolean;
}>;

type IndependentSimulationEvidence = Readonly<{
  attempted: boolean;
  reason: string | null;
  genesisHash: string | null;
  currentSlot: number | null;
  results: readonly IndependentSimulationResult[];
}>;

function simulationAddresses(value: unknown): string[] | null {
  if (!Array.isArray(value) || value.length === 0 || !value.every((entry) => typeof entry === "string")) {
    return null;
  }
  try {
    const addresses = value.map((entry) => new PublicKey(entry).toBase58());
    return new Set(addresses).size === addresses.length ? addresses : null;
  } catch {
    return null;
  }
}

function normalizedAccount(value: unknown): JsonRecord | null {
  const account = record(value);
  if (account === null) return null;
  const data = account.data;
  if (!Array.isArray(data) || data.length !== 2 || typeof data[0] !== "string" || data[1] !== "base64") {
    return null;
  }
  return {
    data,
    executable: account.executable,
    lamports: account.lamports,
    owner: account.owner,
    rentEpoch: account.rentEpoch,
    space: account.space,
  };
}

function accountSetSha256(value: unknown): string | null {
  if (!Array.isArray(value)) return null;
  const accounts = value.map((entry) => entry === null ? null : normalizedAccount(entry));
  if (accounts.some((entry, index) => value[index] !== null && entry === null)) return null;
  return sha256(JSON.stringify(accounts));
}

function exactNormalizedAccount(left: unknown, right: unknown): boolean {
  const leftAccount = left === null ? null : normalizedAccount(left);
  const rightAccount = right === null ? null : normalizedAccount(right);
  return leftAccount !== null || left === null
    ? JSON.stringify(leftAccount) === JSON.stringify(rightAccount)
    : false;
}

function reportTicketState(value: unknown, expectedConfig: string): Readonly<{
  armed: boolean;
  lastConsumedSequence: string;
  activeSequence: string;
  activeWireSha256: string;
}> | null {
  const account = normalizedAccount(value);
  const dataField = account?.data;
  if (!Array.isArray(dataField) || dataField.length !== 2 || dataField[1] !== "base64") return null;
  const data = canonicalBase64(dataField[0]);
  if (data === null || data.length !== 96
    || !data.subarray(0, 8).equals(Buffer.from("f568b6c53ae774ed", "hex"))
    || data[8] !== 1 || data[9] !== 254 || !data.subarray(11, 16).every((byte) => byte === 0)
    || new PublicKey(data.subarray(16, 48)).toBase58() !== expectedConfig
    || (data[10] !== 0 && data[10] !== 1)) return null;
  return {
    armed: data[10] === 1,
    lastConsumedSequence: data.readBigUInt64LE(48).toString(),
    activeSequence: data.readBigUInt64LE(56).toString(),
    activeWireSha256: data.subarray(64, 96).toString("hex"),
  };
}

async function independentSignedSimulations(
  rows: readonly SignedSimulationRow[],
): Promise<IndependentSimulationEvidence> {
  const rpcUrl = process.env.SOLANA_RPC_URL?.trim();
  if (!rpcUrl) {
    return {
      attempted: false,
      reason: "SOLANA_RPC_URL unavailable",
      genesisHash: null,
      currentSlot: null,
      results: [],
    };
  }

  let requestId = 0;
  const rpc = async (method: string, params: readonly unknown[]): Promise<JsonRecord> => {
    for (let attempt = 0; attempt < 24; attempt += 1) {
      const response = await fetch(rpcUrl, {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({ jsonrpc: "2.0", id: ++requestId, method, params }),
      });
      const payload = record(await response.json());
      const error = record(payload?.error);
      const message = typeof error?.message === "string" ? error.message : "";
      const retryable = response.status === 429
        || message.includes("Minimum context slot has not been reached")
        || message.includes("minimum context slot");
      if (response.ok && payload !== null && payload.error === undefined) return payload;
      if (!retryable || attempt === 23) {
        throw new Error(response.ok
          ? `RPC ${method} returned an error`
          : `RPC ${method} returned HTTP ${response.status}`);
      }
      await new Promise((resolve) => setTimeout(resolve, 250));
    }
    throw new Error(`RPC ${method} retry budget exhausted`);
  };

  try {
    const genesisResponse = await rpc("getGenesisHash", []);
    const genesisHash = typeof genesisResponse.result === "string" ? genesisResponse.result : null;
    const slotResponse = await rpc("getSlot", [{ commitment: "confirmed" }]);
    const currentSlot = nonnegativeInteger(slotResponse.result) ? slotResponse.result : null;
    if (genesisHash === null || currentSlot === null) {
      return { attempted: true, reason: "RPC returned invalid cluster identity or slot", genesisHash, currentSlot, results: [] };
    }

    const results: IndependentSimulationResult[] = [];
    for (const row of rows) {
      const wire = canonicalBase64(row.transactionBase64);
      const addresses = simulationAddresses(row.inspectedAddresses);
      const transactionSha256 = sha256Hex(row.transactionSha256) ? row.transactionSha256 : null;
      const armOnlyExpected = row.expectation !== "arm-only-success"
        || (row.expectedArmOnlyTransition !== undefined
          && addresses?.includes(row.expectedArmOnlyTransition.ticketAddress) === true
          && sha256Hex(row.expectedArmOnlyTransition.activeWireSha256));
      const validInput = wire !== null
        && transactionSha256 !== null
        && sha256(wire) === transactionSha256
        && addresses !== null
        && armOnlyExpected
        && sha256Hex(row.logsSha256);
      if (!validInput || wire === null || addresses === null) {
        results.push({
          name: row.name,
          expectation: row.expectation,
          transactionSha256,
          validInput: false,
          contextSlot: null,
          errIsNull: null,
          blockhashExpired: false,
          logsSha256: null,
          logsMatchEvidence: false,
          simulationPostAccountsAvailable: null,
          simulationNullAddresses: [],
          simulationChangedAddresses: [],
          simulationStateUnchanged: null,
          armOnlyTicketTransitionExact: null,
          chainReadbackContextSlot: null,
          chainReadbackStateSha256: null,
          signatureStatusIsNull: null,
          stateUnchanged: null,
          passed: false,
        });
        continue;
      }

      const preResponse = await rpc("getMultipleAccounts", [addresses, {
        commitment: "confirmed",
        encoding: "base64",
        minContextSlot: currentSlot,
      }]);
      const preResult = record(preResponse.result);
      const preContext = record(preResult?.context);
      const preSlot = nonnegativeInteger(preContext?.slot) ? preContext.slot : null;
      const preStateSha256 = accountSetSha256(preResult?.value);
      const preAccounts = Array.isArray(preResult?.value)
        && preResult.value.length === addresses.length ? preResult.value : null;
      const simulatedResponse = await rpc("simulateTransaction", [row.transactionBase64, {
        accounts: { addresses, encoding: "base64" },
        commitment: "confirmed",
        encoding: "base64",
        minContextSlot: preSlot ?? currentSlot,
        replaceRecentBlockhash: false,
        sigVerify: true,
      }]);
      const simulationResult = record(simulatedResponse.result);
      const simulationContext = record(simulationResult?.context);
      const contextSlot = nonnegativeInteger(simulationContext?.slot) ? simulationContext.slot : null;
      const value = record(simulationResult?.value);
      const logs = Array.isArray(value?.logs) && value.logs.every((line) => typeof line === "string")
        ? value.logs as string[]
        : null;
      const observedLogsSha256 = logs === null ? null : sha256(logs.join("\n"));
      const errIsNull = value === null ? null : value.err === null;
      const blockhashExpired = JSON.stringify(value?.err) === JSON.stringify("BlockhashNotFound");
      const simulationAccounts = Array.isArray(value?.accounts)
        && value.accounts.length === addresses.length ? value.accounts : null;
      const simulationNullAddresses = simulationAccounts === null ? [] : addresses.filter(
        (_, index) => simulationAccounts[index] === null,
      );
      const simulationPostAccountsAvailable = simulationAccounts === null ? null
        : simulationNullAddresses.length === 0;
      const simulationAccountShapeExact = simulationAccounts !== null
        && (simulationPostAccountsAvailable === true || simulationNullAddresses.length === addresses.length);
      const simulationPostStateSha256 = simulationAccounts === null ? null : accountSetSha256(simulationAccounts);
      const simulationStateUnchanged = simulationPostAccountsAvailable === true
        && preStateSha256 !== null && simulationPostStateSha256 !== null
        ? preStateSha256 === simulationPostStateSha256
        : null;
      const simulationChangedAddresses = preAccounts === null || simulationAccounts === null
        ? [] : addresses.filter((_, index) => !exactNormalizedAccount(
          preAccounts[index], simulationAccounts[index],
        ));
      let armOnlyTicketTransitionExact: boolean | null = null;
      if (row.expectation === "arm-only-success" && row.expectedArmOnlyTransition !== undefined
        && simulationPostAccountsAvailable === true) {
        const ticketIndex = addresses.indexOf(row.expectedArmOnlyTransition.ticketAddress);
        const ticketState = ticketIndex < 0 ? null : reportTicketState(
          simulationAccounts?.[ticketIndex], row.expectedArmOnlyTransition.configAddress,
        );
        armOnlyTicketTransitionExact = simulationChangedAddresses.length === 1
          && simulationChangedAddresses[0] === row.expectedArmOnlyTransition.ticketAddress
          && ticketState?.armed === true
          && ticketState.lastConsumedSequence === row.expectedArmOnlyTransition.lastConsumedSequence
          && ticketState.activeSequence === row.expectedArmOnlyTransition.activeSequence
          && ticketState.activeWireSha256 === row.expectedArmOnlyTransition.activeWireSha256;
      }
      const chainReadbackResponse = await rpc("getMultipleAccounts", [addresses, {
        commitment: "confirmed",
        encoding: "base64",
        minContextSlot: contextSlot ?? preSlot ?? currentSlot,
      }]);
      const chainReadbackResult = record(chainReadbackResponse.result);
      const chainReadbackContext = record(chainReadbackResult?.context);
      const chainReadbackContextSlot = nonnegativeInteger(chainReadbackContext?.slot)
        ? chainReadbackContext.slot : null;
      const chainReadbackStateSha256 = accountSetSha256(chainReadbackResult?.value);
      const stateUnchanged = preStateSha256 !== null && chainReadbackStateSha256 !== null
        ? preStateSha256 === chainReadbackStateSha256 : null;
      const signature = signedTransactionSignature(row.transactionBase64);
      const signatureStatusResponse = signature === null ? null : await rpc("getSignatureStatuses", [
        [signature], { searchTransactionHistory: true },
      ]);
      const signatureStatusResult = record(signatureStatusResponse?.result);
      const signatureStatuses = Array.isArray(signatureStatusResult?.value) ? signatureStatusResult.value : null;
      const signatureStatusIsNull = signatureStatuses?.length === 1
        ? signatureStatuses[0] === null : null;
      const logsMatchEvidence = observedLogsSha256 !== null && observedLogsSha256 === row.logsSha256;
      const expiredWireProof = blockhashExpired
        && signatureStatusIsNull === true
        && stateUnchanged === true;
      const passed = contextSlot !== null
        && chainReadbackContextSlot !== null
        && stateUnchanged === true
        && signatureStatusIsNull === true
        && (expiredWireProof
          || (logsMatchEvidence
            && (row.expectation === "success"
              ? errIsNull === true && simulationPostAccountsAvailable === true
              : row.expectation === "arm-only-success"
                ? errIsNull === true
                  && simulationPostAccountsAvailable === true
                  && armOnlyTicketTransitionExact === true
                : errIsNull === false
                && simulationAccountShapeExact
                && (simulationPostAccountsAvailable === false || simulationStateUnchanged === true))));
      results.push({
        name: row.name,
        expectation: row.expectation,
        transactionSha256,
        validInput: true,
        contextSlot,
        errIsNull,
        blockhashExpired,
        logsSha256: observedLogsSha256,
        logsMatchEvidence,
        simulationPostAccountsAvailable,
        simulationNullAddresses,
        simulationChangedAddresses,
        simulationStateUnchanged,
        armOnlyTicketTransitionExact,
        chainReadbackContextSlot,
        chainReadbackStateSha256,
        signatureStatusIsNull,
        stateUnchanged,
        passed,
      });
    }
    return { attempted: true, reason: null, genesisHash, currentSlot, results };
  } catch (error) {
    const reason = error instanceof Error
      ? error.message.replace(/https?:\/\/\S+/g, "<redacted>")
      : "read-only RPC verification failed";
    return { attempted: true, reason, genesisHash: null, currentSlot: null, results: [] };
  }
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

function derivedSquadsPolicy(seed: bigint): string | null {
  const seedBytes = Buffer.alloc(8);
  seedBytes.writeBigUInt64LE(seed);
  return derivedPda(RWA_MULTIPLY_ROUTE.squads.program.toString(), [
    Buffer.from("smart_account"),
    Buffer.from("policy"),
    new PublicKey(RWA_MULTIPLY_ROUTE.squads.settings.toString()).toBuffer(),
    seedBytes,
  ]);
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
  const manifestRuntimeBindings = record(manifest?.runtimeBindings);
  const manifestBridgePolicies = Array.isArray(manifestRuntimeBindings?.bridgePolicies)
    ? manifestRuntimeBindings.bridgePolicies.map(record).filter((value): value is JsonRecord => value !== null)
    : [];
  const manifestPrimeUsdc = record(manifestRuntimeBindings?.primeUsdc);
  const manifestBridgeRollover = manifestBridgePolicies.map((binding) => ({
    action: binding.action,
    account: binding.account,
    dataSha256: binding.dataSha256,
  }));
  const expectedBridgeRollover = BRIDGE_POLICY_ROLLOVER.map(({ action, account, dataSha256 }) => ({
    action, account, dataSha256,
  }));
  const checks = {
    planHashExact: planHash === PLAN_SHA256,
    bridgePolicyRouteSpecExact: rwaMultiplyRouteSpecSha256() === BRIDGE_POLICY_ROUTE_SPEC_SHA256,
    manifestPresentAndV1: manifest?.schema === "loyal-backyard-rwa-manifest/v1",
    manifestPhaseOneBindingsDeclared: typeof manifestIdentities?.v2StrategyConfig === "string"
      && manifestBridgePolicies.length === 4
      && manifestBridgePolicies.every((binding) => typeof binding.action === "string"
        && typeof binding.account === "string"
        && sha256Hex(binding.dataSha256))
      && typeof manifestPrimeUsdc?.program === "string"
      && typeof manifestPrimeUsdc?.market === "string"
      && typeof manifestPrimeUsdc?.obligation === "string"
      && typeof manifestPrimeUsdc?.collateralReserve === "string"
      && typeof manifestPrimeUsdc?.debtReserve === "string"
      && Array.isArray(manifestPrimeUsdc?.packets)
      && manifestPrimeUsdc.packets.length >= 4,
    manifestBridgePolicyRolloverExact: JSON.stringify(manifestBridgeRollover) === JSON.stringify(expectedBridgeRollover)
      && BRIDGE_POLICY_ROLLOVER.every(({ seed, account }) => derivedSquadsPolicy(seed) === account),
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
    ? pass("V01_contract_and_forbidden_surface", "Frozen v10 Phase 1 contract, manifest, Go source, and forbidden deployed-writer surface are exact.", evidence)
    : fail("V01_contract_and_forbidden_surface", "Frozen v10 Phase 1 contract, manifest, Go source, and forbidden deployed-writer surface are exact.", evidence,
      "Add/fix the checked-in v1 manifest and Go worker source without changing the frozen plan; remove any legacy deployed writer wiring.");
}

async function adaptorCheck(): Promise<Check> {
  const manifest = parseJson(MANIFEST_PATH);
  const identities = record(manifest?.identities);
  const source = sourceText([ADAPTOR_PROCESSOR, ADAPTOR_CONFIG]);
  const bridgeSource = sourceText([
    "tools/backyard-voltr/src/integrations/rwa-multiply-voltr.ts",
    "crates/loyal-actions/src/autonomous_vaults/voltr_custom.rs",
    "go/backyard-rwa-worker/internal/backyardrwa/report_ticket.go",
    "go/backyard-rwa-worker/internal/backyardrwa/build.go",
  ]);
  const simulation = parseJson(ADAPTOR_SIMULATION_EVIDENCE);
  const mutationRows = Array.isArray(simulation?.mutations) ? simulation.mutations : [];
  const mutationNames = mutationRows
    .map((value) => value !== null && typeof value === "object" && !Array.isArray(value)
      ? String((value as JsonRecord).name ?? "")
      : "")
  ;
  const requiredMutations = [
    "direct_voltr_without_ticket", "consume_before_arm", "arm_only_payload",
    "reversed_instruction_order", "extra_third_instruction", "different_second_instruction",
    "second_consume", "same_sequence_rearm", "lower_sequence_rearm", "arm_while_active",
    "nonsigner_squads", "wrong_squads_vault", "wrong_settings_owner",
    "wrong_settings_or_index", "address_only_lookalike", "wrong_delegated_executor",
    "wrong_policy", "wrong_voltr_authority", "wrong_ticket_pda", "wrong_ticket_owner",
    "wrong_ticket_config", "wrong_ticket_index", "readonly_ticket", "wrong_operation",
    "wrong_amount", "wrong_wire_hash", "zero_sequence", "sequence_below_observed_slot",
    "sequence_above_observed_slot", "stale_slot", "future_slot", "oversized_amount",
    "oversized_nav", "trailing_bytes", "wrong_vault_or_strategy",
    "wrong_mint_or_token_program", "wrong_ata", "duplicate_writable_alias",
    "voltr_failure_rolls_back_ticket_and_capital",
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
  const canonicalReturnData = record(canonicalSimulation?.returnData);
  const canonicalReport = record(simulation?.report);
  const topology = record(simulation?.topology);
  const ticket = record(simulation?.ticket);
  const ticketBefore = record(ticket?.before);
  const ticketAfter = record(ticket?.after);
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
  const expectedTicket = typeof identities?.adaptorProgram === "string" && typeof v2StrategyConfig === "string"
    ? derivePda(identities.adaptorProgram, [Buffer.from("report_ticket"), new PublicKey(v2StrategyConfig).toBuffer()])
    : null;
  const manifestRuntimeBindings = record(manifest?.runtimeBindings);
  const manifestBridgePolicies = Array.isArray(manifestRuntimeBindings?.bridgePolicies)
    ? manifestRuntimeBindings.bridgePolicies.map(record).filter((value): value is JsonRecord => value !== null)
    : [];
  let bridgePolicyReadback: Readonly<{
    attempted: boolean;
    error: string | null;
    contextSlot: number | null;
    current: readonly JsonRecord[];
    retired: readonly Readonly<{ seed: string; account: string | null; exists: boolean | null }>[];
  }> = { attempted: false, error: "SOLANA_RPC_URL unavailable", contextSlot: null, current: [], retired: [] };
  const rpcUrl = process.env.SOLANA_RPC_URL?.trim();
  if (rpcUrl) {
    try {
      const connection = new Connection(rpcUrl, "finalized");
      const installed = await verifyInstalledCustomPolicies(connection);
      const retiredAddresses = RETIRED_BRIDGE_POLICY_SEEDS.map(derivedSquadsPolicy);
      if (retiredAddresses.some((account) => account === null)) throw new Error("retired bridge policy PDA derivation failed");
      const coherentAddresses = [
        ...BRIDGE_POLICY_ROLLOVER.map(({ account }) => account),
        ...retiredAddresses.map((account) => account!),
      ];
      const coherentResponse = await connection.getMultipleAccountsInfoAndContext(
        coherentAddresses.map((account) => new PublicKey(account)),
        { commitment: "finalized", minContextSlot: installed.contextSlot },
      );
      const coherentCurrent = coherentResponse.value.slice(0, BRIDGE_POLICY_ROLLOVER.length);
      const coherentRetired = coherentResponse.value.slice(BRIDGE_POLICY_ROLLOVER.length);
      bridgePolicyReadback = {
        attempted: true,
        error: null,
        contextSlot: coherentResponse.context.slot,
        current: installed.rows.map((row, index) => ({
          ...row,
          coherentOwner: coherentCurrent[index]?.owner.toBase58() ?? null,
          coherentDataSha256: coherentCurrent[index] == null ? null : sha256(coherentCurrent[index]!.data),
        })),
        retired: RETIRED_BRIDGE_POLICY_SEEDS.map((seed, index) => ({
          seed: seed.toString(),
          account: retiredAddresses[index]!,
          exists: coherentRetired[index] !== null,
        })),
      };
    } catch (error) {
      bridgePolicyReadback = {
        attempted: true,
        error: error instanceof Error ? error.message.replace(/https?:\/\/\S+/g, "<redacted>") : "bridge policy readback failed",
        contextSlot: null,
        current: [],
        retired: [],
      };
    }
  }
  const currentBridgePoliciesExact = bridgePolicyReadback.current.length === BRIDGE_POLICY_ROLLOVER.length
    && BRIDGE_POLICY_ROLLOVER.every((expected, index) => {
      const current = bridgePolicyReadback.current[index];
      const manifestBinding = manifestBridgePolicies[index];
      return current?.operation === expected.operation
        && current.seed === expected.seed.toString()
        && current.policy === expected.account
        && current.pass === true
        && current.dataSha256 === expected.dataSha256
        && current.coherentOwner === RWA_MULTIPLY_ROUTE.squads.program.toString()
        && current.coherentDataSha256 === expected.dataSha256
        && manifestBinding?.action === expected.action
        && manifestBinding.account === expected.account
        && manifestBinding.dataSha256 === expected.dataSha256;
    });
  const retiredBridgePoliciesAbsent = bridgePolicyReadback.retired.length === RETIRED_BRIDGE_POLICY_SEEDS.length
    && bridgePolicyReadback.retired.every(({ exists }) => exists === false);
  const armSignerMetas = Array.isArray(simulation?.armSignerMetas)
    ? simulation.armSignerMetas.map(record).filter((row): row is JsonRecord => row !== null)
    : [];
  const consumeSignerMetas = Array.isArray(simulation?.consumeSignerMetas)
    ? simulation.consumeSignerMetas.map(record).filter((row): row is JsonRecord => row !== null)
    : [];
  const mutationTransactionHashes = mutationRows.map((value) => record(value)?.transactionSha256 ?? null);
  const canonicalReturnBytes = typeof canonicalReturnData?.dataBase64 === "string"
    ? canonicalBase64(canonicalReturnData.dataBase64)
    : null;
  const mutationProofsExact = mutationRows.length === requiredMutations.length
    && mutationRows.every((value) => {
      const row = record(value);
      const rpc = record(row?.simulation);
      const inspectedAddresses = simulationAddresses(row?.inspectedAddresses);
      const simulationNullAddresses = Array.isArray(row?.simulationNullAddresses)
        && row.simulationNullAddresses.every((entry) => typeof entry === "string")
        ? row.simulationNullAddresses as string[] : null;
      const simulationPostAccountsAvailable = row?.simulationPostAccountsAvailable;
      const simulationChangedAddresses = Array.isArray(row?.simulationChangedAddresses)
        && row.simulationChangedAddresses.every((entry) => typeof entry === "string")
        ? row.simulationChangedAddresses as string[] : null;
      const armOnlyTicketTransition = record(row?.armOnlyTicketTransition);
      const isArmOnly = row?.name === "arm_only_payload";
      const outcomeExact = isArmOnly
        ? row?.expectation === "arm-only-success"
          && simulationPostAccountsAvailable === true
          && simulationNullAddresses?.length === 0
          && simulationChangedAddresses !== null
          && ticket?.address !== undefined
          && exactStringSet(simulationChangedAddresses, [String(ticket.address)])
          && armOnlyTicketTransition?.armed === true
          && bigintOrNull(armOnlyTicketTransition.lastConsumedSequence)
            === bigintOrNull(ticketBefore?.lastConsumedSequence)
          && bigintOrNull(armOnlyTicketTransition.activeSequence)
            === bigintOrNull(canonicalReport?.sequence)
          && armOnlyTicketTransition.activeWireSha256 === topology?.capitalWireSha256
          && row?.error === null
          && row?.rejectedBeforeMutation === false
        : row?.expectation === "rejection"
          && (simulationPostAccountsAvailable === true
            ? simulationNullAddresses?.length === 0
              && simulationChangedAddresses?.length === 0
            : simulationPostAccountsAvailable === false
              && inspectedAddresses !== null
              && simulationNullAddresses !== null
              && exactStringSet(simulationNullAddresses, inspectedAddresses)
            )
          && typeof row?.error === "string"
          && row.error.length > 0
          && row.rejectedBeforeMutation === true;
      return row !== null
        && requiredMutations.includes(String(row.name))
        && base64MatchesSha256(row.transactionBase64, row.transactionSha256)
        && signedTransactionSignature(row.transactionBase64) !== null
        && sha256Hex(row.logsSha256)
        && sha256Hex(row.simulationStateSha256)
        && sha256Hex(row.preStateSha256)
        && row.preStateSha256 === row.postStateSha256
        && row.preStateSha256 === row.chainReadbackStateSha256
        && inspectedAddresses !== null
        && simulationNullAddresses !== null
        && simulationChangedAddresses !== null
        && outcomeExact
        && nonnegativeInteger(row.chainReadbackContextSlot)
        && nonnegativeInteger(rpc?.contextSlot)
        && row.chainReadbackContextSlot >= rpc.contextSlot
        && row.signatureStatus === null
        && rpc?.sigVerify === true
        && rpc?.replaceRecentBlockhash === false;
    })
    && mutationRows.filter((value) => record(value)?.expectation === "rejection").length === 38
    && mutationRows.filter((value) => record(value)?.expectation === "arm-only-success").length === 1
    && new Set(mutationTransactionHashes).size === mutationTransactionHashes.length;
  const independentSimulation = await independentSignedSimulations([
    {
      name: "canonical",
      expectation: "success",
      transactionBase64: simulation?.transactionBase64,
      transactionSha256: simulation?.transactionSha256,
      inspectedAddresses: simulation?.inspectedAddresses,
      logsSha256: canonicalSimulation?.logsSha256,
    },
    ...mutationRows.map((value): SignedSimulationRow => {
      const row = record(value);
      return {
        name: String(row?.name ?? ""),
        expectation: row?.name === "arm_only_payload" ? "arm-only-success" : "failure",
        transactionBase64: row?.transactionBase64,
        transactionSha256: row?.transactionSha256,
        inspectedAddresses: row?.inspectedAddresses,
        logsSha256: row?.logsSha256,
        expectedArmOnlyTransition: row?.name === "arm_only_payload"
          && typeof ticket?.address === "string"
          && typeof ticket?.config === "string"
          && typeof ticketBefore?.lastConsumedSequence === "string"
          && typeof canonicalReport?.sequence === "string"
          && typeof topology?.capitalWireSha256 === "string"
          ? {
            ticketAddress: ticket.address,
            configAddress: ticket.config,
            lastConsumedSequence: ticketBefore.lastConsumedSequence,
            activeSequence: canonicalReport.sequence,
            activeWireSha256: topology.capitalWireSha256,
          } : undefined,
      };
    }),
  ]);
  const expectedIndependentNames = ["canonical", ...requiredMutations];
  const independentSimulationExact = independentSimulation.genesisHash === manifest?.genesisHash
    && exactStringSet(independentSimulation.results.map((result) => result.name), expectedIndependentNames)
    && independentSimulation.results.length === expectedIndependentNames.length
    && independentSimulation.results.every((result) => result.passed);
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
    exactTicketAbi: source.includes("REPORT_TICKET_SEED")
      && source.includes("REPORT_TICKET_DISCRIMINATOR")
      && source.includes("REPORT_TICKET_VERSION")
      && source.includes("REPORT_TICKET_LEN")
      && source.includes("INITIALIZE_REPORT_TICKET_DISCRIMINATOR")
      && source.includes("ARM_REPORT_DISCRIMINATOR")
      && source.includes("ReportTicket")
      && source.includes("process_initialize_report_ticket")
      && source.includes("process_arm_report")
      && source.includes("load_ticket")
      && source.includes("capital_wire_hash")
      && source.includes("validate_ticket_for_capital")
      && source.includes("consume_ticket"),
    requiresSquadsSignerAtArm: source.includes("fn process_arm_report")
      && source.includes("!accounts[3].is_signer")
      && source.includes("validate_squads_binding(&config, &accounts[2], &accounts[3], &accounts[4])"),
    requiresVoltrStrategySigner: source.includes("accounts[0].is_signer")
      && source.includes("strategy_authority_signer")
      && source.includes("if !strategy_authority_signer"),
    derivesSquadsVault: source.includes("SQUADS_PREFIX") && source.includes("Pubkey::find_program_address"),
    exactSettingsType: source.includes("SQUADS_SETTINGS_DISCRIMINATOR")
      && source.includes("valid_settings_authority_graph")
      && source.includes("signer_count != 1"),
    exactVoltrProgram: source.includes("VOLTR_PROGRAM_ID")
      && source.includes("voltr_program.key != &VOLTR_PROGRAM_ID"),
    sequenceEqualsObservedSlot: source.includes("report.sequence != report.observed_slot"),
    observedSlotFreshness: source.includes("Clock::get()")
      && source.includes("checked_sub(report.observed_slot)")
      && source.includes("max_report_age_slots"),
    staleActiveTicketRecovery: source.includes("age > max_report_age_slots")
      && source.includes("report.sequence <= ticket.active_sequence")
      && source.includes("AdaptorError::TicketAlreadyArmed"),
    hasNavAfterRaw: source.includes("nav_after_raw"),
    hasSnapshotDigest: source.includes("snapshot_digest"),
    returnsExactNAV: source.includes("set_return_data(&report.nav_after_raw.to_le_bytes())"),
    rejectsTrailingBytes: source.includes("data.len() != REPORT_OFFSET + REPORT_V1_LEN")
      && source.includes("data[OPTION_TAG_OFFSET] != 1")
      && source.includes("!= REPORT_V1_LEN as u32")
      && source.includes("input.len() != REPORT_V1_LEN")
      && source.includes("data.len() == 9")
      && source.includes("data[8] == 0"),
    noEconomicRouteSurface: forbidden.length === 0,
    immutableConfigWithOneTicket: source.includes("report.sequence != report.observed_slot")
      && bridgeSource.includes("reportTicketPDA")
      && bridgeSource.includes("ticketedBridgeInstructions")
      && bridgeSource.includes("len(inner) != len(constraintIndexes)"),
    canonicalSignedUnsentSimulation: simulation?.schema === "loyal-backyard-rwa-adaptor-simulation/v2"
      && simulation?.broadcast === false
      && simulation?.signedUnsent === true
      && simulation?.path === "Squads->[ArmReport,Voltr->adaptor]"
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
      && bigintOrNull(canonicalReport?.sequence) !== null
      && bigintOrNull(canonicalReport?.sequence) === bigintOrNull(canonicalReport?.observedSlot)
      && (canonicalReturnData === null
        ? canonicalSimulation?.returnData === null
        : [identities?.adaptorProgram, identities?.voltrProgram].includes(canonicalReturnData.programId)
          && canonicalReturnBytes !== null
          && canonicalReturnBytes.length === 8
          && canonicalReturnBytes.readBigUInt64LE(0) === bigintOrNull(canonicalReport?.navAfterRaw))
      && nonnegativeInteger(canonicalSimulation?.contextSlot),
    exactAtomicTicketTopology: topology?.squadsInnerInstructionCount === 2
      && Array.isArray(topology?.orderedInstructions)
      && JSON.stringify(topology.orderedInstructions) === JSON.stringify(["ArmReport", "VoltrCapital"])
      && topology?.voltrRemainingTicketIndex === 17
      && topology?.adaptorTicketIndex === 8
      && topology?.ticketWritable === true
      && topology?.threeInstructionFallback === false,
    exactOneUseTicketTransition: expectedTicket !== null
      && ticket?.address === expectedTicket && ticket?.bump === 254
      && ticket?.config === v2StrategyConfig
      && ticketBefore?.armed === false && ticketAfter?.armed === false
      && bigintOrNull(ticketAfter?.lastConsumedSequence) === bigintOrNull(canonicalReport?.sequence)
      && bigintOrNull(ticketBefore?.lastConsumedSequence) !== null
      && bigintOrNull(ticketAfter?.lastConsumedSequence)! > bigintOrNull(ticketBefore?.lastConsumedSequence)!
      && bigintOrNull(ticketAfter?.activeSequence) === 0n
      && ticketAfter?.activeWireSha256 === "0".repeat(64)
      && canonicalSimulation?.configPreStateSha256 === canonicalSimulation?.configPostStateSha256,
    bindingsMatchFrozenManifest: expectedStrategyAuthority !== null
      && expectedSquadsVault !== null
      && expectedTicket !== null
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
      && simulationBindings?.squadsAssetAta === identities?.squadsUsdcAta
      && simulationBindings?.reportTicket === expectedTicket,
    separatedExactSignerProofs: armSignerMetas.length === 1
      && armSignerMetas[0]?.address === expectedSquadsVault
      && armSignerMetas[0]?.isSigner === true
      && consumeSignerMetas.length === 1
      && consumeSignerMetas[0]?.address === expectedStrategyAuthority
      && consumeSignerMetas[0]?.isSigner === true,
    finalizedBridgePoliciesExact: currentBridgePoliciesExact,
    retiredBridgePoliciesAbsent,
    exactV10Matrix: exactStringSet(mutationNames, requiredMutations)
      && mutationProofsExact,
    independentCurrentSimulation: independentSimulationExact,
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
    independentSimulation,
    bridgePolicyReadback,
  };
  const condition = "Adaptor v2 uses one exact reusable ticket and finalized bridge policies are exactly seeds 62-65 while retired seeds 53-56 are closed.";
  if (Object.values(checks).every(Boolean)) return pass("V02_adaptor_identity_and_signer", condition, evidence);
  const checksWithoutIndependentRpc = Object.entries(checks)
    .filter(([name]) => !["independentCurrentSimulation", "finalizedBridgePoliciesExact", "retiredBridgePoliciesAbsent"].includes(name))
    .every(([, value]) => value);
  return checksWithoutIndependentRpc && !independentSimulation.attempted && !bridgePolicyReadback.attempted
    ? blocked("V02_adaptor_identity_and_signer", condition, evidence,
      "Provide SOLANA_RPC_URL and rerun while the checked-in signed-unsent ticket transactions are fresh; the verifier will also prove exact policies 62-65 and absence of retired policies 53-56.")
    : fail("V02_adaptor_identity_and_signer", condition, evidence,
      "Repair the first adaptor, signed-unsent simulation, current policy 62-65, or retired policy 53-56 mismatch; never weaken the exact policy boundary.");
}

async function policyCatalogCheck(): Promise<Check> {
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
  const groupTransactions = Array.isArray(simulation?.groupTransactions) ? simulation.groupTransactions : [];
  const negativeTransactions = Array.isArray(simulation?.negativeTransactions) ? simulation.negativeTransactions : [];
  const independentRows = [
    ...groupTransactions.map((value): SignedSimulationRow => {
      const row = record(value);
      return {
        name: String(row?.name ?? ""),
        expectation: "success",
        transactionBase64: row?.transactionBase64,
        transactionSha256: row?.transactionSha256,
        inspectedAddresses: row?.inspectedAddresses,
        logsSha256: row?.logsSha256,
      };
    }),
    ...negativeTransactions.map((value): SignedSimulationRow => {
      const row = record(value);
      return {
        name: String(row?.name ?? ""),
        expectation: "failure",
        transactionBase64: row?.transactionBase64,
        transactionSha256: row?.transactionSha256,
        inspectedAddresses: row?.inspectedAddresses,
        logsSha256: row?.logsSha256,
      };
    }),
  ];
  const independentSimulation = await independentSignedSimulations(independentRows);
  const expectedGroupNames = [
    "three-lane-markets", "singleton-markets", "swap-graph", "bridge-lifecycle",
  ];
  const expectedNegativeNames = [
    "same-mint-wrong-reserve", "cross-lane-obligation", "unapproved-edge",
    "extra-instruction", "amount-cap-breach", "signer-substitution",
    "writable-role-substitution",
  ];
  const signedWireRowsExact = exactStringSet(
    groupTransactions.map((value) => String(record(value)?.name ?? "")),
    expectedGroupNames,
  ) && exactStringSet(
    negativeTransactions.map((value) => String(record(value)?.name ?? "")),
    expectedNegativeNames,
  ) && independentRows.every((row) => base64MatchesSha256(row.transactionBase64, row.transactionSha256)
    && simulationAddresses(row.inspectedAddresses) !== null
    && sha256Hex(row.logsSha256));
  const independentSimulationExact = independentSimulation.genesisHash === simulation?.genesisHash
    && simulation?.genesisHash === parseJson(MANIFEST_PATH)?.genesisHash
    && exactStringSet(
      independentSimulation.results.map((result) => result.name),
      [...expectedGroupNames, ...expectedNegativeNames],
    )
    && independentSimulation.results.length === expectedGroupNames.length + expectedNegativeNames.length
    && independentSimulation.results.every((result) => result.passed);
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
      && simulation?.cluster === "mainnet-beta"
      && simulation?.commitment === "confirmed"
      && exactStringSet(simulation?.groups, expectedGroupNames),
    negativeCasesExact: exactStringSet(simulation?.negativeCases, expectedNegativeNames),
    signedWireRowsExact,
    independentCurrentSimulation: independentSimulationExact,
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
    independentSimulation,
  };
  const condition = "Phase 2 catalog is the exact 11-lane, 44-operation, 52-edge, first-safe-packet-fitting authority set.";
  if (Object.values(checks).every(Boolean)) return pass("P2_catalog_semantics_and_packing", condition, evidence);
  const checksWithoutIndependentRpc = Object.entries(checks)
    .filter(([name]) => name !== "independentCurrentSimulation")
    .every(([, value]) => value);
  return checksWithoutIndependentRpc && !independentSimulation.attempted
    ? blocked("P2_catalog_semantics_and_packing", condition, evidence,
      "Provide SOLANA_RPC_URL and rerun while all eleven checked-in signed-unsent group and negative wires are fresh.")
    : fail("P2_catalog_semantics_and_packing", condition, evidence,
      "Resolve current confirmed route identities, compile the exact correlated lane constraints, and attach fresh signed-unsent group and negative wires with inspected account sets.");
}

function goWorkerCheck(): Check {
  const manifest = parseJson(MANIFEST_PATH);
  const runtimeBindings = record(manifest?.runtimeBindings);
  const primeUsdcBinding = record(runtimeBindings?.primeUsdc);
  const primeUsdcPackets = Array.isArray(primeUsdcBinding?.packets) ? primeUsdcBinding.packets : [];
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
  const jupiterBuildSource = sourceText([join(GO_ROOT, "internal/backyardrwa/jupiter.go")]);
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
  const migrationSource = sourceText(migrations);
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
    "SWAP_USDC_TO_PRIME_STEP",
    "SWAP_PRIME_TO_USDC_STEP",
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
    migrationReusesExistingTables: migrations.length === 2
      && migrations[0] === "crates/loyal-yield-store/migrations/0070_backyard_rwa_worker.sql"
      && migrations[1] === "crates/loyal-yield-store/migrations/0071_backyard_rwa_phase1_activation.sql"
      && migrationSource.includes("multiply_route_states")
      && migrationSource.includes("multiply_operations")
      && migrationSource.includes("multiply_route_states_schema_v8_v9_or_backyard_v1")
      && migrationSource.includes("earn_max_v2")
      && migrationSource.includes("request_withdrawal")
      && migrationSource.includes("cancel_withdrawal")
      && migrationSource.includes("source_instruction_index IS NOT NULL")
      && migrationSource.includes("source_instruction_index IS NULL")
      && migrationSource.includes("SWAP_USDC_TO_PRIME_STEP")
      && migrationSource.includes("SWAP_PRIME_TO_USDC_STEP")
      && migrationSource.includes("prestate_sha256")
      && migrationSource.includes("poststate_sha256")
      && migrationSource.includes("state_version = 817")
      && migrationSource.includes("state_version = 818")
      && migrationSource.includes("UPDATE loyal_yield.multiply_route_states")
      && !migrationSource.includes("INSERT INTO loyal_yield.multiply_route_states")
      && source.includes("multiply_position_snapshots"),
    runtimeNotDisabled: !source.includes("disabled until concrete deployment wiring"),
    tickRunsObservationDecisionAndBuild: workerSource.includes("Observe")
      && workerSource.includes("Decide(")
      && workerSource.includes("RecordDecision")
      && workerSource.includes("BuildSimulateAndPersistBridge")
      && workerSource.includes("BuildSimulateAndPersistKamino")
      && workerSource.includes("BuildSimulateAndPersistJupiter"),
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
      && kaminoBuildSource.includes("BuildSimulateAndPersistKamino")
      && /func\s+\w*(Build|build)\w*Jupiter\w*\s*\(/.test(jupiterBuildSource)
      && jupiterBuildSource.includes("BuildSimulateAndPersistJupiter"),
    primeUsdcExecutionEvidenceReady: primeUsdcPackets.length >= 4
      && !kaminoObserveSource.includes("reviewed manifest intentionally has no packet vectors yet"),
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
    migrationSha256: migrations.length === 2 ? migrations.map((path) => ({ path, sha256: sha256File(path) })) : null,
    schemaReadback,
    missingConcreteCapabilities: {
      observationDecisionBuild: !(workerSource.includes("BuildSimulateAndPersistBridge")
        && workerSource.includes("BuildSimulateAndPersistKamino")
        && workerSource.includes("BuildSimulateAndPersistJupiter")),
      receiptKaminoObservation: kaminoObserveSource.length === 0,
      primeUsdcExecutionEvidence: !checks.primeUsdcExecutionEvidenceReady,
      actionTransactionConstruction: kaminoBuildSource.length === 0,
      signing: !(source.includes("crypto/ed25519") && source.includes("ed25519.Sign")),
      deployedSchemaIntrospection: !checks.independentSchemaIntrospection,
    },
    forbidden,
  };
  const condition = "One concrete serialized Go worker and narrow existing-table migration pass focused tests.";
  if (Object.values(checks).every(Boolean)) return pass("V04_go_state_machine_and_store", condition, evidence);
  const localChecksPass = Object.entries(checks)
    .filter(([name]) => name !== "independentSchemaIntrospection")
    .every(([, value]) => value);
  return localChecksPass && !schemaReadback.attempted
    ? blocked("V04_go_state_machine_and_store", condition, evidence,
      "Provide NEON_DATABASE_URL and rerun to prove the applied 0070->0071 schema readback.")
    : fail("V04_go_state_machine_and_store", condition, evidence,
      "Implement/fix the fixed-route Go state machine, persistence ordering, NAV logic, existing-table migration, and focused tests.");
}

function unwrapRenderRow(value: unknown, key: "service" | "deploy"): JsonRecord | null {
  const outer = record(value);
  return record(outer?.[key]) ?? outer;
}

function renderDeploymentRead(expectedImage: string): JsonRecord {
  const hasCredential = Boolean(process.env.RENDER_API_KEY?.trim())
    || existsSync(join(homedir(), ".render", "cli.yaml"));
  if (!hasCredential) {
    return { attempted: false, available: false, reason: "Render CLI credential unavailable" };
  }
  const listed = runJson("render", ["services", "--output", "json"]);
  if (listed.exitCode !== 0 || !Array.isArray(listed.value)) {
    return {
      attempted: listed.attempted,
      available: false,
      reason: listed.error ?? "Render service listing unavailable",
    };
  }
  const services = listed.value
    .map((value) => unwrapRenderRow(value, "service"))
    .filter((value): value is JsonRecord => value !== null);
  const matches = services.filter((service) => service.name === "loyal-backyard-rwa-worker");
  const service = matches.length === 1 ? matches[0]! : null;
  if (!service) {
    return {
      attempted: true,
      available: true,
      serviceCount: matches.length,
      exact: false,
      reason: "expected exactly one live Render service named loyal-backyard-rwa-worker",
    };
  }
  const serviceId = typeof service.id === "string" ? service.id : null;
  if (!serviceId) {
    return { attempted: true, available: true, serviceCount: 1, exact: false, reason: "Render service has no id" };
  }
  const details = record(service.serviceDetails);
  const envDetails = record(details?.envSpecificDetails);
  const registry = record(service.registryCredential);
  const deployRows = runJson("render", ["deploys", "list", serviceId, "--output", "json"]);
  if (deployRows.exitCode !== 0 || !Array.isArray(deployRows.value)) {
    return {
      attempted: true,
      available: false,
      serviceId,
      reason: deployRows.error ?? "Render deploy listing unavailable",
    };
  }
  const deploys = deployRows.value
    .map((value) => unwrapRenderRow(value, "deploy"))
    .filter((value): value is JsonRecord => value !== null);
  const liveDeploy = deploys.find((deploy) => deploy.status === "live") ?? null;
  if (!liveDeploy) {
    return { attempted: true, available: true, serviceId, exact: false, reason: "Render has no live deploy" };
  }
  const image = record(liveDeploy.image);
  const deployedImage = typeof image?.ref === "string"
    ? image.ref
    : typeof service.imagePath === "string" ? service.imagePath : null;
  const imageDigest = typeof image?.sha === "string" ? image.sha : null;
  // Start at deploy creation, before the process startup identity line. Using
  // finishedAt can exclude the very runtime proof this read is looking for.
  const deployedAt = typeof liveDeploy.createdAt === "string"
    ? liveDeploy.createdAt
    : typeof liveDeploy.finishedAt === "string" ? liveDeploy.finishedAt : null;
  const logArgs = [
    "logs", "--resources", serviceId,
    ...(deployedAt ? ["--start", deployedAt] : ["--start", "24h"]),
    "--text", "backyard-rwa-worker: starting serialized confirmed lifecycle",
    "--limit", "20", "--output", "json",
  ];
  const logs = runJson("render", logArgs);
  if (logs.exitCode !== 0) {
    return {
      attempted: true,
      available: false,
      serviceId,
      deployId: liveDeploy.id ?? null,
      reason: logs.error ?? "Render runtime log read unavailable",
    };
  }
  const logText = JSON.stringify(logs.value);
  const imageTag = /:sha-([0-9a-f]{40})$/.exec(expectedImage)?.[1] ?? null;
  const serviceExact = service.type === "background_worker"
    && details?.runtime === "image"
    && envDetails?.dockerCommand === "/usr/local/bin/backyard-rwa-worker"
    && details?.numInstances === 1
    && (service.suspended === undefined || service.suspended === "not_suspended")
    && (registry?.name === undefined || registry.name === "loyal-ghcr");
  const deployExact = deployedImage === expectedImage
    && typeof imageDigest === "string" && /^sha256:[0-9a-f]{64}$/.test(imageDigest);
  const runtimeExact = logText.includes("backyard-rwa-worker: starting serialized confirmed lifecycle")
    && logText.includes("route=rwa-multiply:ST999VUTo5QExYEX9bz1oDDoKGkjXG9zpphy4Hj7VWh")
    && imageTag !== null && logText.includes(`image=sha-${imageTag}`)
    && /manifest_sha256=[0-9a-f]{64}/.test(logText);
  return {
    attempted: true,
    available: true,
    exact: serviceExact && deployExact && runtimeExact,
    serviceExact,
    deployExact,
    runtimeExact,
    serviceCount: 1,
    serviceId,
    expectedLeaseOwner: imageTag === null ? null : `render:${serviceId}:sha-${imageTag}`,
    deployId: liveDeploy.id ?? null,
    deployStatus: liveDeploy.status ?? null,
    deployedAt,
    deployedImage,
    imageDigest,
  };
}

function deploymentLeaseRead(): JsonCommandResult {
  return readOnlyDatabaseJson(`
SELECT json_build_object(
  'migration70', EXISTS (
    SELECT 1 FROM loyal_yield.schema_migrations
    WHERE version = 70 AND name = 'backyard_rwa_worker'
  ),
  'routeCount', (SELECT count(*) FROM loyal_yield.multiply_route_states
    WHERE route_key = 'rwa-multiply:ST999VUTo5QExYEX9bz1oDDoKGkjXG9zpphy4Hj7VWh'),
  'engineVersion', (SELECT state ->> 'engineVersion' FROM loyal_yield.multiply_route_states
    WHERE route_key = 'rwa-multiply:ST999VUTo5QExYEX9bz1oDDoKGkjXG9zpphy4Hj7VWh'),
  'routeKind', (SELECT state ->> 'routeKind' FROM loyal_yield.multiply_route_states
    WHERE route_key = 'rwa-multiply:ST999VUTo5QExYEX9bz1oDDoKGkjXG9zpphy4Hj7VWh'),
  'leaseActive', COALESCE((SELECT lease_owner IS NOT NULL AND lease_expires_at > now()
    FROM loyal_yield.multiply_route_states
    WHERE route_key = 'rwa-multiply:ST999VUTo5QExYEX9bz1oDDoKGkjXG9zpphy4Hj7VWh'), false),
  'leaseOwner', (SELECT lease_owner FROM loyal_yield.multiply_route_states
    WHERE route_key = 'rwa-multiply:ST999VUTo5QExYEX9bz1oDDoKGkjXG9zpphy4Hj7VWh'),
  'leaseExpiresAt', (SELECT lease_expires_at FROM loyal_yield.multiply_route_states
    WHERE route_key = 'rwa-multiply:ST999VUTo5QExYEX9bz1oDDoKGkjXG9zpphy4Hj7VWh'),
  'routeUpdatedAt', (SELECT updated_at FROM loyal_yield.multiply_route_states
    WHERE route_key = 'rwa-multiply:ST999VUTo5QExYEX9bz1oDDoKGkjXG9zpphy4Hj7VWh'),
  'nonterminalCount', (SELECT count(*) FROM loyal_yield.multiply_operations
    WHERE route_key = 'rwa-multiply:ST999VUTo5QExYEX9bz1oDDoKGkjXG9zpphy4Hj7VWh'
      AND status IN ('prepared','signed_persisted','broadcast_intent','confirmed',
        'reconciliation_pending','decided','built','simulated','signed','submitted','reconciling')),
  'recentBackyardWrites', (SELECT count(*) FROM loyal_yield.multiply_operations
    WHERE route_key = 'rwa-multiply:ST999VUTo5QExYEX9bz1oDDoKGkjXG9zpphy4Hj7VWh'
      AND engine_version = 'backyard_rwa_v1' AND updated_at >= now() - interval '30 minutes'),
  'recentCompetingWrites', (SELECT count(*) FROM loyal_yield.multiply_operations
    WHERE route_key = 'rwa-multiply:ST999VUTo5QExYEX9bz1oDDoKGkjXG9zpphy4Hj7VWh'
      AND engine_version <> 'backyard_rwa_v1' AND updated_at >= now() - interval '24 hours')
)::text;
`);
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
  const expectedImage = pinnedImagePattern.exec(backyardService)?.[0]?.trim() ?? "";
  const live = expectedImage ? renderDeploymentRead(expectedImage) : {
    attempted: false, available: false, reason: "checked-in immutable image unavailable",
  };
  const database = deploymentLeaseRead();
  const lease = record(database.value);
  const leaseOwner = typeof lease?.leaseOwner === "string" ? lease.leaseOwner : null;
  const expectedLeaseOwner = typeof live.expectedLeaseOwner === "string" ? live.expectedLeaseOwner : null;
  const liveUnavailable = live.available !== true;
  const databaseUnavailable = database.exitCode !== 0 || lease === null;
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
    independentRenderRead: live.exact === true,
    migrationApplied: lease?.migration70 === true,
    exactRouteLease: lease?.routeCount === 1
      && lease?.engineVersion === "backyard_rwa_v1"
      && lease?.routeKind === "backyard_rwa_v1"
      && lease?.leaseActive === true
      && leaseOwner !== null
      && leaseOwner === expectedLeaseOwner,
    serializedNonterminal: lease?.nonterminalCount === 0 || lease?.nonterminalCount === 1,
    noCompetingRecentWriter: lease?.recentCompetingWrites === 0,
    runtimeRecentlyObserved: typeof lease?.recentBackyardWrites === "number"
      && lease.recentBackyardWrites > 0,
  };
  const row = {
    checks,
    renderServiceCount: backyardServices.length,
    renderServiceSha256: backyardService.length > 0 ? sha256(backyardService) : null,
    imageBuildSourceSha256: imageBuildSource.length > 0 ? sha256(imageBuildSource) : null,
    deploymentEvidencePath: DEPLOYMENT_EVIDENCE,
    deploymentEvidenceSha256: sha256File(DEPLOYMENT_EVIDENCE),
    checkedInEvidence: evidence,
    live,
    database: {
      attempted: database.attempted,
      exitCode: database.exitCode,
      error: database.error,
      value: lease === null ? null : {
        ...lease,
        leaseOwner: leaseOwner === null ? null : `sha256:${sha256(leaseOwner)}`,
      },
    },
  };
  const staticChecks = [checks.imageBuildWired, checks.exactlyOneGoService, checks.goServiceWired,
    checks.goCommandDirect, checks.immutableGhcrImage, checks.noLegacyWriter];
  if (!prerequisitesPass || !staticChecks.every(Boolean)) {
    return fail("V05_deployed_single_writer", "Exactly one immutable Go deployment owns the route and no old writer can claim it.", row,
      "Complete the Phase 1 bridge/NAV and Go prerequisites, add the pinned service wiring, and remove legacy route ownership before deployment.");
  }
  if (liveUnavailable || databaseUnavailable) {
    return blocked("V05_deployed_single_writer", "Exactly one immutable Go deployment owns the route and no old writer can claim it.", row,
      "Provide working read-only Render CLI credentials and NEON_DATABASE_URL, then rerun the sole verifier.");
  }
  return Object.values(checks).every(Boolean)
    ? pass("V05_deployed_single_writer", "Exactly one immutable Go deployment owns the route and no old writer can claim it.", row)
    : fail("V05_deployed_single_writer", "Exactly one immutable Go deployment owns the route and no old writer can claim it.", row,
      "Make the one live Render service/image/startup identity and active database lease exactly match, then stop all competing writers and rerun.");
}

function derivePda(program: string, seeds: readonly Buffer[]): string {
  return PublicKey.findProgramAddressSync([...seeds], new PublicKey(program))[0].toBase58();
}

function deriveAta(owner: string, mint: string, tokenProgram: string, associatedTokenProgram: string): string {
  return derivePda(associatedTokenProgram, [
    new PublicKey(owner).toBuffer(),
    new PublicKey(tokenProgram).toBuffer(),
    new PublicKey(mint).toBuffer(),
  ]);
}

function v06RouteBindings(): V06RouteBindings {
  const route = RWA_MULTIPLY_ROUTE;
  const voltr = route.programs.voltr.toString();
  const vault = route.vault.address.toString();
  const strategy = route.customAdaptor.strategyConfig.toString();
  const token = route.assets.tokenProgram.toString();
  const associatedToken = route.assets.associatedTokenProgram.toString();
  const usdc = route.assets.assetMint.toString();
  const idleAuthority = derivePda(voltr, [Buffer.from("vault_asset_idle_auth"), new PublicKey(vault).toBuffer()]);
  const strategyAuthority = derivePda(voltr, [
    Buffer.from("vault_strategy_auth"),
    new PublicKey(vault).toBuffer(),
    new PublicKey(strategy).toBuffer(),
  ]);
  return {
    routeKey: route.id,
    genesisHash: route.genesisHash,
    withdrawalWaitSeconds: Number(route.vault.withdrawalWaitingPeriodSeconds),
    targetLtvBps: 5_000,
    maxReportAgeSlots: Number(route.customAdaptor.maxReportAgeSlots),
    programs: {
      voltr,
      adaptor: route.customAdaptor.program.toString(),
      squads: route.squads.program.toString(),
      kamino: route.kamino.program.toString(),
      jupiter: route.programs.jupiter.toString(),
      token,
      associatedToken,
    },
    accounts: {
      voltrVault: vault,
      strategy,
      strategyReceipt: derivePda(voltr, [
        Buffer.from("strategy_init_receipt"),
        new PublicKey(vault).toBuffer(),
        new PublicKey(strategy).toBuffer(),
      ]),
      voltrIdleAta: deriveAta(idleAuthority, usdc, token, associatedToken),
      strategyAta: deriveAta(strategyAuthority, usdc, token, associatedToken),
      squadsUsdcAta: route.squads.assetAta.toString(),
      squadsPrimeAta: route.squads.collateralAta.toString(),
      obligation: route.kamino.obligation.toString(),
      collateralReserve: route.kamino.collateralReserve.toString(),
      debtReserve: route.kamino.debtReserve.toString(),
      squadsSettings: route.squads.settings.toString(),
      squadsVault: route.squads.vault.toString(),
      reportTicket: derivePda(route.customAdaptor.program.toString(), [
        Buffer.from("report_ticket"),
        new PublicKey(route.customAdaptor.strategyConfig.toString()).toBuffer(),
      ]),
    },
    mints: { usdc, prime: route.assets.collateralMint.toString() },
  };
}

function lifecycleSignatures(evidence: JsonRecord | null): string[] | null {
  if (!evidence || !Array.isArray(evidence.steps)) return null;
  const signatures: string[] = [];
  for (const value of evidence.steps) {
    const step = record(value);
    if (!step || !Array.isArray(step.transactions)) return null;
    for (const input of step.transactions) {
      const transaction = record(input);
      if (typeof transaction?.signature !== "string" || !/^[1-9A-HJ-NP-Za-km-z]{80,90}$/.test(transaction.signature)) return null;
      try {
        if (bs58.decode(transaction.signature).length !== 64) return null;
      } catch {
        return null;
      }
      signatures.push(transaction.signature);
    }
  }
  return signatures.length > 0 && signatures.length <= 64 && new Set(signatures).size === signatures.length
    ? signatures
    : null;
}

function parsedTokenBalances(meta: JsonRecord, accountKeys: readonly string[]): V06TokenBalance[] | null {
  type PartialBalance = { mint: string; owner: string; beforeRaw?: string; afterRaw?: string };
  const balances = new Map<string, PartialBalance>();
  const readSide = (value: unknown, side: "beforeRaw" | "afterRaw"): boolean => {
    if (!Array.isArray(value)) return false;
    for (const input of value) {
      const row = record(input);
      const amount = record(row?.uiTokenAmount)?.amount;
      const index = row?.accountIndex;
      const address = typeof index === "number" && Number.isSafeInteger(index) ? accountKeys[index] : undefined;
      if (!address || typeof row?.mint !== "string" || typeof row.owner !== "string"
        || typeof amount !== "string" || !/^(0|[1-9][0-9]*)$/.test(amount)) return false;
      const existing = balances.get(address);
      if (existing && (existing.mint !== row.mint || existing.owner !== row.owner || existing[side] !== undefined)) return false;
      balances.set(address, { ...existing, mint: row.mint, owner: row.owner, [side]: amount });
    }
    return true;
  };
  if (!readSide(meta.preTokenBalances, "beforeRaw") || !readSide(meta.postTokenBalances, "afterRaw")) return null;
  return [...balances.entries()].map(([address, balance]) => ({
    address,
    mint: balance.mint,
    owner: balance.owner,
    beforeRaw: balance.beforeRaw ?? "0",
    afterRaw: balance.afterRaw ?? "0",
  })).sort((left, right) => left.address.localeCompare(right.address));
}

async function lifecycleChainRead(evidence: JsonRecord | null, route: V06RouteBindings): Promise<V06ChainRead> {
  const rpcUrl = process.env.SOLANA_RPC_URL?.trim();
  const signatures = lifecycleSignatures(evidence);
  if (!rpcUrl || signatures === null) {
    return {
      attempted: false,
      error: !rpcUrl ? "SOLANA_RPC_URL unavailable" : "lifecycle evidence has no exact signature set",
      genesisHash: null,
      transactions: [],
      finalContextSlot: null,
      finalAccounts: [],
      finalAccountData: {},
    };
  }
  let requestId = 0;
  const rpc = async (method: string, params: readonly unknown[]): Promise<unknown> => {
    const response = await fetch(rpcUrl, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ jsonrpc: "2.0", id: ++requestId, method, params }),
    });
    if (!response.ok) throw new Error(`RPC ${method} returned HTTP ${response.status}`);
    const payload = record(await response.json());
    if (!payload || payload.error !== undefined) throw new Error(`RPC ${method} returned an error`);
    return payload.result;
  };
  try {
    const genesisHash = await rpc("getGenesisHash", []);
    if (genesisHash !== route.genesisHash) throw new Error("RPC cluster is not pinned mainnet-beta");
    const transactions: V06ChainTransaction[] = [];
    for (const signature of signatures) {
      const value = record(await rpc("getTransaction", [signature, {
        commitment: "confirmed",
        encoding: "base64",
        maxSupportedTransactionVersion: 0,
      }]));
      const encoded = value?.transaction;
      const meta = record(value?.meta);
      if (!Array.isArray(encoded) || encoded.length !== 2 || typeof encoded[0] !== "string" || encoded[1] !== "base64"
        || !meta || typeof value?.slot !== "number" || !Number.isSafeInteger(value.slot) || value.slot <= 0
        || typeof value.blockTime !== "number" || !Number.isSafeInteger(value.blockTime) || value.blockTime <= 0) {
        throw new Error("confirmed lifecycle transaction is absent or malformed");
      }
      const wire = Buffer.from(encoded[0], "base64");
      if (wire.length === 0 || wire.toString("base64") !== encoded[0]) throw new Error("lifecycle wire is not canonical base64");
      const transaction = VersionedTransaction.deserialize(wire);
      if (bs58.encode(transaction.signatures[0]!) !== signature) throw new Error("RPC wire signature differs from requested signature");
      const loaded = record(meta.loadedAddresses);
      const writable = Array.isArray(loaded?.writable) && loaded.writable.every((entry) => typeof entry === "string")
        ? loaded.writable as string[] : [];
      const readonly = Array.isArray(loaded?.readonly) && loaded.readonly.every((entry) => typeof entry === "string")
        ? loaded.readonly as string[] : [];
      const accountKeys = [...transaction.message.staticAccountKeys.map((key) => key.toBase58()), ...writable, ...readonly];
      const topLevelInstructions = transaction.message.compiledInstructions.map((instruction, index) => ({
        groupIndex: index,
        position: index,
        stackHeight: 1,
        programId: accountKeys[instruction.programIdIndex]!,
        accounts: [...instruction.accountKeyIndexes].map((accountIndex) => accountKeys[accountIndex]!),
        dataBase64: Buffer.from(instruction.data).toString("base64"),
      }));
      const innerInstructions: Array<V06ChainTransaction["innerInstructions"][number]> = [];
      const programIndexes = transaction.message.compiledInstructions.map(({ programIdIndex }) => programIdIndex);
      if (Array.isArray(meta.innerInstructions)) {
        for (const group of meta.innerInstructions) {
          const inner = record(group);
          if (!Array.isArray(inner?.instructions)) throw new Error("inner instruction group is malformed");
          for (const [position, input] of inner.instructions.entries()) {
            const instruction = record(input);
            if (typeof instruction?.programIdIndex !== "number" || !Number.isSafeInteger(instruction.programIdIndex)
              || !Array.isArray(instruction.accounts) || !instruction.accounts.every((entry) => typeof entry === "number" && Number.isSafeInteger(entry))
              || typeof instruction.data !== "string") {
              throw new Error("inner instruction program index is malformed");
            }
            programIndexes.push(instruction.programIdIndex);
            const decodedData = Buffer.from(bs58.decode(instruction.data));
            innerInstructions.push({
              groupIndex: typeof inner.index === "number" && Number.isSafeInteger(inner.index) ? inner.index : -1,
              position,
              stackHeight: typeof instruction.stackHeight === "number" && Number.isSafeInteger(instruction.stackHeight)
                ? instruction.stackHeight : null,
              programId: accountKeys[instruction.programIdIndex]!,
              accounts: (instruction.accounts as number[]).map((accountIndex) => accountKeys[accountIndex]!),
              dataBase64: decodedData.toString("base64"),
            });
          }
        }
      }
      const programIds = [...new Set(programIndexes.map((index) => accountKeys[index]).filter((entry): entry is string => typeof entry === "string"))];
      const tokenBalances = parsedTokenBalances(meta, accountKeys);
      if (tokenBalances === null) throw new Error("transaction token balances are malformed");
      const rawReturnData = record(meta.returnData);
      const returnDataValue = rawReturnData?.data;
      const returnData = rawReturnData === null ? null
        : typeof rawReturnData.programId === "string" && Array.isArray(returnDataValue)
          && returnDataValue.length === 2 && typeof returnDataValue[0] === "string" && returnDataValue[1] === "base64"
          && Buffer.from(returnDataValue[0], "base64").toString("base64") === returnDataValue[0]
          ? { programId: rawReturnData.programId, dataBase64: returnDataValue[0] }
          : undefined;
      if (returnData === undefined) throw new Error("transaction return data is malformed");
      transactions.push({
        signature,
        slot: value.slot,
        blockTime: value.blockTime,
        success: meta.err === null,
        wireBase64: encoded[0],
        accountKeys,
        programIds,
        tokenBalances,
        returnData,
        topLevelInstructions,
        innerInstructions,
      });
    }
    const lifecycle = evidence as unknown as V06LifecycleEvidence;
    const expectedAddresses = [
      route.accounts.voltrVault, route.accounts.strategy, route.accounts.strategyReceipt,
      route.accounts.voltrIdleAta, route.accounts.strategyAta, route.accounts.squadsUsdcAta,
      route.accounts.squadsPrimeAta, route.accounts.obligation, route.accounts.collateralReserve,
      route.accounts.debtReserve, lifecycle.withdrawalReceipt,
      route.accounts.reportTicket,
    ];
    const minimumSlot = Math.max(...transactions.map(({ slot }) => slot));
    const response = record(await rpc("getMultipleAccounts", [expectedAddresses, {
      commitment: "confirmed",
      encoding: "base64",
      minContextSlot: minimumSlot,
    }]));
    const context = record(response?.context);
    if (typeof context?.slot !== "number" || !Number.isSafeInteger(context.slot) || context.slot < minimumSlot
      || !Array.isArray(response?.value) || response.value.length !== expectedAddresses.length) {
      throw new Error("final account read is malformed or regressed");
    }
    const finalAccounts: V06FinalAccountEvidence[] = [];
    const finalAccountData: Record<string, string | null> = {};
    response.value.forEach((input, index) => {
      const address = expectedAddresses[index]!;
      if (input === null) {
        finalAccounts.push({ address, owner: null, dataSha256: null });
        finalAccountData[address] = null;
        return;
      }
      const account = record(input);
      const data = account?.data;
      if (!account || typeof account.owner !== "string" || !Array.isArray(data) || data.length !== 2
        || typeof data[0] !== "string" || data[1] !== "base64") throw new Error("final account envelope is malformed");
      const bytes = Buffer.from(data[0], "base64");
      if (bytes.toString("base64") !== data[0]) throw new Error("final account data is not canonical base64");
      finalAccounts.push({ address, owner: account.owner, dataSha256: sha256(bytes) });
      finalAccountData[address] = data[0];
    });
    return {
      attempted: true,
      error: null,
      genesisHash,
      transactions,
      finalContextSlot: context.slot,
      finalAccounts,
      finalAccountData,
    };
  } catch (error) {
    return {
      attempted: true,
      error: redact(error instanceof Error ? error.message : String(error)),
      genesisHash: null,
      transactions: [],
      finalContextSlot: null,
      finalAccounts: [],
      finalAccountData: {},
    };
  }
}

function lifecycleDatabaseRead(signatures: readonly string[], route: V06RouteBindings, chain: V06ChainRead): V06DatabaseRead {
  if (signatures.length === 0 || chain.transactions.length !== signatures.length) {
    return { attempted: false, error: "exact confirmed lifecycle signatures unavailable", rows: [], position: null, nonterminalCount: null };
  }
  const literal = (value: string) => `'${value.replaceAll("'", "''")}'`;
  const signatureArray = `ARRAY[${signatures.map(literal).join(",")}]::text[]`;
  const minimumSlot = Math.min(...chain.transactions.map(({ slot }) => slot));
  const maximumSlot = Math.max(...chain.transactions.map(({ slot }) => slot));
  const result = readOnlyDatabaseJson(`
SELECT json_build_object(
  'rows', COALESCE((SELECT json_agg(json_build_object(
      'operationId', operation_id,
      'cycle', cycle,
      'action', action,
      'status', status,
      'transactionSignature', transaction_signature,
      'confirmedSlot', confirmed_slot,
      'confirmationStatus', confirmation_status,
      'signedWireBase64', encode(signed_wire, 'base64'),
      'signedWireSha256', signed_wire_sha256,
      'expectedEffects', expected_effects,
      'reconciledEffects', reconciled_effects,
      'reconciliationSha256', reconciliation_sha256,
      'createdAt', created_at::text,
      'broadcastIntentAt', broadcast_intent_at::text
    ) ORDER BY cycle, confirmed_slot, operation_id)
    FROM loyal_yield.multiply_operations
    WHERE route_key = ${literal(route.routeKey)}
      AND engine_version = 'backyard_rwa_v1'
      AND transaction_signature = ANY(${signatureArray})), '[]'::json),
  'position', (SELECT json_build_object(
      'observedSlot', observed_slot,
      'collateralRaw', collateral_raw::text,
      'debtRaw', debt_raw::text,
      'ltvBps', ltv_bps,
      'valuationSource', valuation_source)
    FROM loyal_yield.multiply_position_snapshots
    WHERE route_key = ${literal(route.routeKey)}
      AND observed_slot BETWEEN ${minimumSlot} AND ${maximumSlot}
      AND collateral_raw > 0 AND debt_raw > 0
    ORDER BY observed_slot DESC LIMIT 1),
  'nonterminalCount', (SELECT count(*)
    FROM loyal_yield.multiply_operations
    WHERE route_key = ${literal(route.routeKey)}
      AND status IN ('prepared','signed_persisted','broadcast_intent','confirmed',
        'reconciliation_pending','decided','built','simulated','signed','submitted','reconciling'))
)::text;
`);
  const value = record(result.value);
  return {
    attempted: result.attempted,
    error: result.error,
    rows: Array.isArray(value?.rows) ? value.rows as unknown as V06DatabaseRead["rows"] : [],
    position: record(value?.position) as unknown as V06DatabaseRead["position"],
    nonterminalCount: typeof value?.nonterminalCount === "number" ? value.nonterminalCount : null,
  };
}

async function lifecycleCheck(deploymentPass: boolean): Promise<Check> {
  const evidence = parseJson(LIFECYCLE_EVIDENCE);
  const route = v06RouteBindings();
  const chain = await lifecycleChainRead(evidence, route);
  const signatures = lifecycleSignatures(evidence) ?? [];
  const database = lifecycleDatabaseRead(signatures, route, chain);
  const validation = validateV06Lifecycle(evidence, route, chain, database);
  const row = {
    checks: validation.checks,
    details: validation.details,
    evidencePath: LIFECYCLE_EVIDENCE,
    evidenceSha256: sha256File(LIFECYCLE_EVIDENCE),
    chain: {
      attempted: chain.attempted,
      error: chain.error,
      genesisHash: chain.genesisHash,
      transactionCount: chain.transactions.length,
      signatures: chain.transactions.map(({ signature, slot, blockTime, success }) => ({ signature, slot, blockTime, success })),
      finalContextSlot: chain.finalContextSlot,
      finalAccounts: chain.finalAccounts,
    },
    database: {
      attempted: database.attempted,
      error: database.error,
      rowCount: database.rows.length,
      operations: database.rows.map(({ operationId, action, status, transactionSignature, confirmedSlot, confirmationStatus }) => ({
        operationId, action, status, transactionSignature, confirmedSlot, confirmationStatus,
      })),
      position: database.position,
      nonterminalCount: database.nonterminalCount,
    },
  };
  if (!deploymentPass) {
    return fail("V06_live_internal_lifecycle", "One real confirmed internal deposit-to-claim lifecycle is independently reconciled.", row,
      "Complete V05 before requesting approval for the real internal lifecycle.");
  }
  if (evidence === null || !chain.attempted || database.attempted === false) {
    return blocked("V06_live_internal_lifecycle", "One real confirmed internal deposit-to-claim lifecycle is independently reconciled.", row,
      "Provide the exact lifecycle-v1 evidence plus working read-only SOLANA_RPC_URL and NEON_DATABASE_URL, then rerun the sole verifier.");
  }
  return validation.pass
    ? pass("V06_live_internal_lifecycle", "One real confirmed internal deposit-to-claim lifecycle is independently reconciled.", row)
    : fail("V06_live_internal_lifecycle", "One real confirmed internal deposit-to-claim lifecycle is independently reconciled.", row,
      "Run and reconcile the complete approved lifecycle, including a target-LTV onchain position snapshot, a fresh explicit external total-route-yield attestation, and exact NAV report sequence; then regenerate evidence from the real signatures and rerun.");
}

function nestedProjection(state: JsonRecord | null, keys: readonly string[]): unknown {
  if (!state) return null;
  const groups = [state, record(state.snapshot), record(state.observation), record(state.balances), record(state.report)]
    .filter((value): value is JsonRecord => value !== null);
  for (const group of groups) {
    for (const key of keys) {
      const value = group[key];
      if (["string", "number", "bigint", "boolean"].includes(typeof value)) return value;
    }
  }
  return null;
}

function bigintOrNull(value: unknown): bigint | null {
  try {
    return value === null || value === undefined ? null : BigInt(value as string | number | bigint);
  } catch {
    return null;
  }
}

function formatAdminRaw(value: bigint | null): string {
  if (value === null) return "Unavailable";
  const whole = value / 1_000_000n;
  const fraction = ((value % 1_000_000n) / 10_000n).toString().padStart(2, "0");
  return `${whole.toString().replace(/\B(?=(\d{3})+(?!\d))/g, ",")}.${fraction}`;
}

function formatAdminUsdMicros(value: bigint | null): string {
  if (value === null) return "—";
  const cents = (value + 5_000n) / 10_000n;
  const whole = cents / 100n;
  return `$${whole.toString().replace(/\B(?=(\d{3})+(?!\d))/g, ",")}.${(cents % 100n).toString().padStart(2, "0")}`;
}

function formatAdminBps(value: bigint | null): string {
  if (value === null) return "—";
  return `${(Number(value) / 100).toFixed(2)}%`;
}

function adminDatabaseRead(): JsonCommandResult {
  return readOnlyDatabaseJson(`
SELECT json_build_object(
  'route', (SELECT json_build_object(
      'routeKey', route_key, 'state', state, 'updatedAt', updated_at)
    FROM loyal_yield.multiply_route_states
    WHERE route_key = 'rwa-multiply:ST999VUTo5QExYEX9bz1oDDoKGkjXG9zpphy4Hj7VWh'),
  'snapshot', (SELECT json_build_object(
      'strategyKey', strategy_key,
      'collateralRaw', collateral_raw::text,
      'debtRaw', debt_raw::text,
      'equityUsdMicros', equity_usd_micros::text,
      'ltvBps', ltv_bps::text,
      'forecastApyBps', forecast_apy_bps::text,
      'observedAt', observed_at)
    FROM loyal_yield.multiply_position_snapshots
    WHERE route_key = 'rwa-multiply:ST999VUTo5QExYEX9bz1oDDoKGkjXG9zpphy4Hj7VWh'
    ORDER BY observed_at DESC, id DESC LIMIT 1),
  'history', COALESCE((SELECT json_agg(row_to_json(history_row)) FROM (
    SELECT action, status,
      expected_effects -> 'decision' ->> 'amountRaw' AS amount_raw,
      transaction_signature, created_at
    FROM loyal_yield.multiply_operations
    WHERE route_key = 'rwa-multiply:ST999VUTo5QExYEX9bz1oDDoKGkjXG9zpphy4Hj7VWh'
      AND engine_version = 'backyard_rwa_v1'
    ORDER BY created_at DESC, operation_id DESC LIMIT 20
  ) AS history_row), '[]'::json)
)::text;
`);
}

async function vercelDeploymentRead(adminUrl: string, sourceCommit: string | null): Promise<JsonRecord> {
  const token = process.env.VERCEL_TOKEN?.trim();
  if (!token) return { attempted: false, available: false, reason: "VERCEL_TOKEN unavailable" };
  let host: string;
  try {
    host = new URL(adminUrl).host;
  } catch {
    return { attempted: false, available: false, reason: "admin deployment URL is invalid" };
  }
  const team = process.env.VERCEL_TEAM_ID?.trim();
  const endpoint = `https://api.vercel.com/v13/deployments/${encodeURIComponent(host)}${team ? `?teamId=${encodeURIComponent(team)}` : ""}`;
  try {
    const response = await fetch(endpoint, {
      headers: { Authorization: `Bearer ${token}`, Accept: "application/json" },
      signal: AbortSignal.timeout(20_000),
    });
    if (response.status === 401 || response.status === 403) {
      return { attempted: true, available: false, reason: `Vercel deployment read returned ${response.status}` };
    }
    if (!response.ok) {
      return { attempted: true, available: true, exact: false, reason: `Vercel deployment read returned ${response.status}` };
    }
    const deployment = record(await response.json());
    const meta = record(deployment?.meta);
    const gitSource = record(deployment?.gitSource);
    const deployedCommit = typeof meta?.githubCommitSha === "string"
      ? meta.githubCommitSha
      : typeof meta?.gitCommitSha === "string" ? meta.gitCommitSha : null;
    const exact = deployment?.readyState === "READY"
      && deployment?.target === "production"
      && deployedCommit !== null && deployedCommit === sourceCommit
      && (gitSource?.ref === undefined || gitSource.ref === "main");
    return {
      attempted: true,
      available: true,
      exact,
      deploymentId: deployment?.id ?? null,
      readyState: deployment?.readyState ?? null,
      target: deployment?.target ?? null,
      deployedCommit,
      sourceCommit,
      gitRef: gitSource?.ref ?? null,
    };
  } catch (error) {
    return { attempted: true, available: false, reason: redact(error instanceof Error ? error.message : String(error)) };
  }
}

async function adminPageRead(adminUrl: string): Promise<JsonRecord> {
  const login = process.env.ADMIN_USER;
  const password = process.env.ADMIN_PASSWORD;
  if (!login || !password) {
    return { attempted: false, available: false, reason: "ADMIN_USER or ADMIN_PASSWORD unavailable" };
  }
  try {
    const loginResponse = await fetch(new URL("/auth/login", adminUrl), {
      method: "POST",
      body: new URLSearchParams({ login, password, next: "/vault-integrations/backyard" }),
      redirect: "manual",
      signal: AbortSignal.timeout(20_000),
    });
    const cookieHeader = loginResponse.headers.get("set-cookie") ?? "";
    const session = /(?:^|,\s*)([^=;,]*session[^=;,]*)=([^;]+)/i.exec(cookieHeader);
    if (![302, 303].includes(loginResponse.status) || !session) {
      return {
        attempted: true,
        available: false,
        reason: `admin login did not issue a session (${loginResponse.status})`,
      };
    }
    const pageResponse = await fetch(new URL("/vault-integrations/backyard", adminUrl), {
      headers: { Cookie: `${session[1]}=${session[2]}` },
      redirect: "manual",
      signal: AbortSignal.timeout(20_000),
    });
    if (!pageResponse.ok) {
      return {
        attempted: true,
        available: pageResponse.status !== 401 && pageResponse.status !== 403,
        exact: false,
        reason: `admin Backyard page returned ${pageResponse.status}`,
      };
    }
    const html = await pageResponse.text();
    return {
      attempted: true,
      available: true,
      status: pageResponse.status,
      html,
      htmlSha256: sha256(html),
    };
  } catch (error) {
    return { attempted: true, available: false, reason: redact(error instanceof Error ? error.message : String(error)) };
  }
}

function tokenAmountFromRpcAccount(value: unknown, expectedOwner: string): bigint | null {
  const account = record(value);
  const data = account?.data;
  if (account?.owner !== "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"
    || !Array.isArray(data) || data[1] !== "base64" || typeof data[0] !== "string") return null;
  const bytes = Buffer.from(data[0], "base64");
  if (bytes.length < 165) return null;
  try {
    const authority = new PublicKey(bytes.subarray(32, 64)).toBase58();
    return authority === expectedOwner ? bytes.readBigUInt64LE(64) : null;
  } catch {
    return null;
  }
}

async function adminRpcRead(state: JsonRecord | null, history: readonly JsonRecord[]): Promise<JsonRecord> {
  const rpcUrl = process.env.SOLANA_RPC_URL?.trim();
  if (!rpcUrl) return { attempted: false, available: false, reason: "SOLANA_RPC_URL unavailable" };
  let requestId = 0;
  const rpc = async (method: string, params: readonly unknown[]): Promise<unknown> => {
    const response = await fetch(rpcUrl, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ jsonrpc: "2.0", id: ++requestId, method, params }),
      signal: AbortSignal.timeout(20_000),
    });
    if (!response.ok) throw new Error(`RPC ${method} returned ${response.status}`);
    const body = record(await response.json());
    if (body === null || body.error !== undefined) throw new Error(`RPC ${method} returned an error`);
    return body.result;
  };
  try {
    const genesis = await rpc("getGenesisHash", []);
    if (genesis !== "5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d") {
      return { attempted: true, available: true, exact: false, reason: "RPC is not mainnet-beta" };
    }
    const slot = await rpc("getSlot", [{ commitment: "confirmed" }]);
    if (!nonnegativeInteger(slot)) return { attempted: true, available: true, exact: false, reason: "RPC slot is invalid" };
    const addresses = [
      "6LATwaB4yRwGURCBDyFeJGqofaXxb6xXws9wBGbr3RBh",
      "FTDWN5Ay8tzYPJBJT4s2oZaHRQ7jKPo8XP2ZRWb5GP3M",
      "EBG2iYrcXttDy9FpWDeNVL8uaCLRCkevrpRyrAhvVYKe",
      "HXtk15EA5pBg3rSKxBm8sWPExScPkTknSRp37fXNHgNA",
    ];
    const result = record(await rpc("getMultipleAccounts", [addresses, {
      commitment: "confirmed", encoding: "base64", minContextSlot: slot,
    }]));
    const context = record(result?.context);
    const values = Array.isArray(result?.value) ? result.value : [];
    const voltrIdle = tokenAmountFromRpcAccount(values[0], "EoHz6FHTL34F6HjuJmb5EceaRqxRG1RMYwYWKtWkGBFb");
    const strategyIdle = tokenAmountFromRpcAccount(values[1], "8fLTf2ufePttZW3Es1xVoW3ows3WjXcuHQkkBCVvHsdH");
    const squadsIdle = tokenAmountFromRpcAccount(values[2], "ST999VUTo5QExYEX9bz1oDDoKGkjXG9zpphy4Hj7VWh");
    const vault = record(values[3]);
    const balancesExact = voltrIdle !== null && strategyIdle !== null && squadsIdle !== null
      && voltrIdle === bigintOrNull(nestedProjection(state, ["voltrIdleRaw", "voltr_idle_raw"]))
      && strategyIdle === bigintOrNull(nestedProjection(state, ["voltrStrategyIdleRaw", "voltr_strategy_idle_raw"]))
      && squadsIdle === bigintOrNull(nestedProjection(state, ["squadsIdleRaw", "squads_idle_raw"]));
    const signatures = history
      .map((row) => typeof row.transaction_signature === "string" ? row.transaction_signature : null)
      .filter((value): value is string => value !== null)
      .slice(0, 20);
    const statuses = signatures.length === 0 ? [] : record(await rpc("getSignatureStatuses", [signatures, {
      searchTransactionHistory: true,
    }]))?.value;
    const signatureRows = Array.isArray(statuses) ? statuses : [];
    const signaturesExact = signatureRows.length === signatures.length
      && signatureRows.every((value) => {
        const status = record(value);
        return status?.err === null && ["confirmed", "finalized"].includes(String(status.confirmationStatus));
      });
    const vaultExact = vault?.owner === "vVoLTRjQmtFpiYoegx285Ze4gsLJ8ZxgFKVcuvmG1a8";
    return {
      attempted: true,
      available: true,
      exact: nonnegativeInteger(context?.slot) && context.slot >= slot
        && balancesExact && signaturesExact && vaultExact,
      contextSlot: context?.slot ?? null,
      balancesExact,
      signaturesChecked: signatures.length,
      signaturesExact,
      vaultExact,
    };
  } catch (error) {
    return { attempted: true, available: false, reason: redact(error instanceof Error ? error.message : String(error)) };
  }
}

async function adminCheck(): Promise<Check> {
  const main = existsSync(APPS_ROOT)
    ? gitFilesAtRef(APPS_ROOT, "refs/remotes/origin/main", /vault.*integration|backyard.*vault/i)
    : { commit: null, files: [], source: "" };
  const source = main.source;
  const required = [
    "AUM", "NAV", "APY", "Voltr", "Squads", "LTV", "withdraw",
    "strategy idle", "receipt", "transaction_signature",
  ];
  const adminUrl = process.env.BACKYARD_ADMIN_URL?.trim() || "https://loyal-admin-zeta.vercel.app";
  const [deployment, page] = await Promise.all([
    vercelDeploymentRead(adminUrl, main.commit),
    adminPageRead(adminUrl),
  ]);
  const database = adminDatabaseRead();
  const databaseValue = record(database.value);
  const route = record(databaseValue?.route);
  const state = record(route?.state);
  const snapshot = record(databaseValue?.snapshot);
  const history = Array.isArray(databaseValue?.history)
    ? databaseValue.history.map(record).filter((value): value is JsonRecord => value !== null)
    : [];
  const rpc = await adminRpcRead(state, history);
  const html = typeof page.html === "string" ? page.html : "";
  const requiredRenderedValues = state && snapshot ? [
    formatAdminUsdMicros(bigintOrNull(nestedProjection(state, ["aumUsdMicros", "aum_usd_micros", "aumRaw"]))),
    formatAdminUsdMicros(bigintOrNull(nestedProjection(state, ["navUsdMicros", "nav_usd_micros", "reportedNavRaw", "navRaw"]))),
    formatAdminBps(bigintOrNull(snapshot.forecastApyBps)),
    `${formatAdminRaw(bigintOrNull(snapshot.collateralRaw))} PRIME`,
    `${formatAdminRaw(bigintOrNull(snapshot.debtRaw))} USDC`,
    formatAdminBps(bigintOrNull(snapshot.ltvBps)),
    `${formatAdminRaw(bigintOrNull(nestedProjection(state, ["voltrIdleRaw", "voltr_idle_raw"]))) } USDC`,
    `${formatAdminRaw(bigintOrNull(nestedProjection(state, ["squadsIdleRaw", "squads_idle_raw"]))) } USDC`,
    "600s",
    "HXtk15EA5pBg3rSKxBm8sWPExScPkTknSRp37fXNHgNA",
    "ST999VUTo5QExYEX9bz1oDDoKGkjXG9zpphy4Hj7VWh",
  ] : [];
  const renderedHistoryExact = history.slice(0, 20).every((row) =>
    typeof row.status === "string" && html.includes(row.status)
      && (typeof row.transaction_signature !== "string" || html.includes(row.transaction_signature)));
  const pageMatchesDatabase = page.available === true
    && route?.routeKey === "rwa-multiply:ST999VUTo5QExYEX9bz1oDDoKGkjXG9zpphy4Hj7VWh"
    && state !== null && snapshot !== null
    && requiredRenderedValues.every((value) => value !== "—" && !value.includes("Unavailable") && html.includes(value))
    && renderedHistoryExact;
  const checks = {
    appsRepositoryPresent: existsSync(APPS_ROOT),
    originMainResolved: typeof main.commit === "string" && /^[0-9a-f]{40}$/.test(main.commit),
    pagePresentOnOriginMain: main.files.some((path) => /page\.(tsx|ts)$/.test(path)),
    requiredFields: required.every((value) => source.toLowerCase().includes(value.toLowerCase())),
    readOnly: !/onClick[^\n]{0,100}(submit|execute|withdraw|deposit)|useMutation|mutationFn/s.test(source),
    deployedOriginMainCommit: deployment.exact === true,
    independentDeployedTruthComparison: pageMatchesDatabase && rpc.exact === true,
  };
  const evidence = {
    checks,
    appsRoot: APPS_ROOT,
    sourceRef: "refs/remotes/origin/main",
    sourceCommit: main.commit,
    candidateFiles: main.files,
    deployment,
    page: {
      attempted: page.attempted,
      available: page.available,
      status: page.status ?? null,
      htmlSha256: page.htmlSha256 ?? null,
      reason: page.reason ?? null,
    },
    database: {
      attempted: database.attempted,
      exitCode: database.exitCode,
      error: database.error,
      routePresent: route !== null,
      snapshotPresent: snapshot !== null,
      historyCount: history.length,
    },
    rpc,
    renderedValueCount: requiredRenderedValues.length,
    renderedHistoryExact,
  };
  const staticChecks = [checks.appsRepositoryPresent, checks.originMainResolved,
    checks.pagePresentOnOriginMain, checks.requiredFields, checks.readOnly];
  if (!staticChecks.every(Boolean)) {
    return fail("V07_admin_macroview_truth", "The thin read-only Vault integrations page exposes all required operating fields.", evidence,
      "Land the minimum read-only page on loyal-apps origin/main with the complete operating fields and no mutation surface.");
  }
  if (deployment.available !== true || page.available !== true || database.exitCode !== 0
    || databaseValue === null || rpc.available !== true) {
    return blocked("V07_admin_macroview_truth", "The thin read-only Vault integrations page exposes all required operating fields.", evidence,
      "Provide working VERCEL_TOKEN, ADMIN_USER, ADMIN_PASSWORD, NEON_DATABASE_URL, and SOLANA_RPC_URL read access, then rerun the sole verifier.");
  }
  return Object.values(checks).every(Boolean)
    ? pass("V07_admin_macroview_truth", "The thin read-only Vault integrations page exposes all required operating fields.", evidence)
    : fail("V07_admin_macroview_truth", "The thin read-only Vault integrations page exposes all required operating fields.", evidence,
      "Deploy the exact loyal-apps origin/main commit and make the authenticated page values equal the independent database readback.");
}

async function main() {
  const sourceCommitResult = run("git", ["rev-parse", "HEAD"]);
  const checks: Check[] = [];
  const v01 = localContractCheck();
  checks.push(v01);
  const v02 = await adaptorCheck();
  checks.push(v02);
  const phase2Catalog = await policyCatalogCheck();
  checks.push(phase2Catalog);
  const v04 = goWorkerCheck();
  checks.push(v04);
  const phaseOneLocalPass = [v01, v02, v04].every((check) => check.verdict === "PASS");
  const v05 = deploymentCheck(phaseOneLocalPass);
  checks.push(v05);
  const v06 = await lifecycleCheck(v05.verdict === "PASS");
  checks.push(v06);
  const v07 = await adminCheck();
  checks.push(v07);

  // Phase 1 is deliberately narrower than the future catalog expansion: it
  // is the authenticated bridge/NAV path, one deployed serialized Go writer,
  // and one complete reconciled PRIME/USDC lifecycle.
  const phaseOneChecks = [v01, v02, v04, v05, v06];
  const firstFailure = phaseOneChecks.find((check) => check.verdict === "FAIL") ?? null;
  const blocker = firstFailure === null
    ? phaseOneChecks.find((check) => check.verdict === "BLOCKED") ?? null
    : null;
  const verdict: Verdict = firstFailure !== null ? "FAIL" : blocker !== null ? "BLOCKED" : "PASS";
  const manifest = parseJson(MANIFEST_PATH);
  const policyCatalog = parseJson(POLICY_CATALOG_PATH);
  const liveDeployment = record(v05.evidence.live);
  const output = {
    schema: SCHEMA,
    verdict,
    releasePhase: "phase1",
    broadcast: false,
    commitment: "confirmed",
    sourceCommit: sourceCommitResult.exitCode === 0 ? sourceCommitResult.stdoutTail.trim() : null,
    deployedImageDigest: liveDeployment?.imageDigest ?? null,
    manifestSha256: sha256File(MANIFEST_PATH),
    policyCatalogSha256: sha256File(POLICY_CATALOG_PATH),
    manifestSchema: manifest?.schema ?? null,
    policyCatalogSchema: policyCatalog?.schema ?? null,
    evidenceLayers: {
      static: ["V01", "V02", "V04", "P2"],
      simulation: ["V02", "P2"],
      submission: ["V06"],
      confirmation: ["V06"],
      deployment: ["V05", "V07"],
      reconciliation: ["V06", "V07"],
      live: ["V06"],
    },
    phase1: {
      verdict,
      condition: "Authenticated bridge/NAV plus one deployed serialized Go writer plus one complete reconciled PRIME/USDC lifecycle.",
      checkIds: phaseOneChecks.map((check) => check.id),
      firstFailure,
      blocker,
    },
    phase2: {
      verdict: phase2Catalog.verdict,
      condition: "Full eleven-lane, 44-operation, 52-edge policy catalog.",
      checkIds: [phase2Catalog.id],
      firstFailure: phase2Catalog.verdict === "FAIL" ? phase2Catalog : null,
      blocker: phase2Catalog.verdict === "BLOCKED" ? phase2Catalog : null,
    },
    supplemental: {
      adminMacroview: v07.verdict,
      checkIds: [v07.id],
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
  await main();
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
