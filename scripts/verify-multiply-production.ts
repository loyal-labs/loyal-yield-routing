import { createHash } from "node:crypto";
import { existsSync, readFileSync, readdirSync, statSync } from "node:fs";
import { resolve } from "node:path";

import { neon } from "@neondatabase/serverless";
import { Connection, PublicKey } from "@solana/web3.js";

const CONTRACT_VERSION = "earn-max-v4";
const POLICY_MANIFEST_VERSION = "earn-max-v2";
const CONTRACT_SHA256 = "945e64c1e3afc8f66b7dd9a4a4a3ac317dd9583f6ba28d4bef54ab61eafc947a";
const PASS = "PASS_EARN_MAX_THREE_POLICY_PRODUCTION_READY";
const FAIL = "FAIL_EARN_MAX_THREE_POLICY_PRODUCTION_READY";
const BLOCKED = "BLOCKED_EARN_MAX_THREE_POLICY_PRODUCTION_READY";
const MAINNET_GENESIS = "5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d";
const VERIFY_SETTINGS = "6jgkucnbz1RuHq6NULqACQY3r2XegHaWhgPpaCEGPCA3";
const APP_URL = "https://askloyal.com";
const POLICY_MONITOR_SERVICE = "srv-d8j87m6q1p3s73ff8n8g";
const MULTIPLY_WORKER_SERVICE = "srv-da56asrncjis73fu9psg";
const ROOT = resolve(import.meta.dir, "..");
const APPS_ROOT = resolve(ROOT, "../loyal-apps");
const CONTRACT = "docs/plans/multiply-rwa-looping-policy-architecture.md";
const CONFIG = "crates/loyal-fleet-worker/src/multiply/config.rs";
const POLICY = "crates/loyal-fleet-worker/src/multiply/policy.rs";
const RUST_EARN_MAX_POLICY = "crates/loyal-actions/src/earn_max.rs";
const BUILDER = "crates/loyal-fleet-worker/src/multiply/builder.rs";
const EXECUTOR = "crates/loyal-fleet-worker/src/multiply/executor.rs";
const POLICY_MONITOR = "crates/loyal-squads-policy-monitor/src/lib.rs";
const POLICY_MONITOR_MANIFEST = "crates/loyal-squads-policy-monitor/Cargo.toml";
const LASERSTREAM_MONITOR = "crates/balance-sweep-ata-monitor/src/main.rs";
const LASERSTREAM_SOURCE = "crates/balance-sweep-ata-monitor/src/lib.rs";
const LASERSTREAM_RECONCILIATION = "crates/balance-sweep-ata-monitor/src/earn_reconciliation.rs";
const WORKER = "crates/loyal-fleet-worker/src/bin/multiply-route-worker.rs";
const MIGRATIONS = "crates/loyal-yield-store/migrations";
const RENDER_BLUEPRINT = "render.yaml";
const APP_API_ROOT = "apps/web/src/app/api/smart-accounts/earn-max";
const APP_FEATURE_ROOT = "apps/web/src/features/earn-max";
const APP_ACTIONS = "packages/loyal-actions/src/earn-max.ts";
const APP_UI = "apps/web/src/components/wallet-workspace/facelift/earn-max-pane.tsx";
const APP_SHELL = "apps/web/src/components/wallet-workspace/facelift/shell.tsx";
const MULTIPLY_STORE = "crates/loyal-yield-store/src/multiply_state_store.rs";
const MIGRATION_RUNNER = "crates/loyal-yield-orchestrator/src/bin/yield-migrations.rs";
const KLEND_PROGRAM = new PublicKey("KLend2g3cP87fffoy8q1mQqGKjrxjC8boSyAYavgmjD");
const TOKEN_PROGRAM = new PublicKey("TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA");
const TOKEN_2022_PROGRAM = new PublicKey("TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb");
const SQUADS_PROGRAM = new PublicKey("SMRTzfY6DfH5ik3TKiyLFfXexV8uSG3d2UksSCYdunG");
const BPF_UPGRADEABLE_LOADER = new PublicKey("BPFLoaderUpgradeab1e11111111111111111111111");
const SUPPORTED_LEGACY_POLICY_PROGRAM_DATA_HASHES = new Set<string>([
  "4242cc6453644e9d76622181800800c20a62b36c53466a8b052777c60bb14db2",
]);

const MAINNET_POOL_CATALOG = [
  {
    key: "onyc_usdc", market: "47tfyEG9SsdEnUm9cw5kY9BXngQGqu3LBoop9j5uTAv8",
    collateralReserve: "6ZxkBSJEqsXA3Kdm2PDAzHLUdPTPUK93Lf4bAezec1UQ", collateralMint: "5Y8NV33Vv7WbnLfq3zBcKSdYPrk7g2KoiQoe7M2tcxp5",
    collateralSupply: "9YuHgsPVGgWrkpsaRZmeZCV2uXweMEn6TEAcusQKRjgG", collateralReceiptMint: "CtzvqjvpxJDXyraDjP2QrEr8b1xvGvxADRV7w29qrmxd", collateralReceiptSupply: "2c42iUaea3QVLvSPQHUBZBwqdvpiQo5vmeMePq9qx8eo",
    debtReserve: "AYL4LMc4ZCVyq3Z7XPJGWDM4H9PiWjqXAAuuHBEGVR2Z", debtMint: "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v", debtTokenProgram: TOKEN_PROGRAM,
    debtSupply: "8BkQTZsT8ssKMU643De4iiV5Wf3pENdUFTsdtHPueKjB", debtFee: "5iLRav31Y7DJwM6bZ7s92jqvV3zd1wZMcp4mYeKXh8cj",
  },
  {
    key: "onyc_usds", market: "47tfyEG9SsdEnUm9cw5kY9BXngQGqu3LBoop9j5uTAv8",
    collateralReserve: "6ZxkBSJEqsXA3Kdm2PDAzHLUdPTPUK93Lf4bAezec1UQ", collateralMint: "5Y8NV33Vv7WbnLfq3zBcKSdYPrk7g2KoiQoe7M2tcxp5",
    collateralSupply: "9YuHgsPVGgWrkpsaRZmeZCV2uXweMEn6TEAcusQKRjgG", collateralReceiptMint: "CtzvqjvpxJDXyraDjP2QrEr8b1xvGvxADRV7w29qrmxd", collateralReceiptSupply: "2c42iUaea3QVLvSPQHUBZBwqdvpiQo5vmeMePq9qx8eo",
    debtReserve: "3yDc9ARvtPLhYxZLgucZGuBtZ9bHshBvXTwHxGe3nhmC", debtMint: "USDSwr9ApdHk5bvJKMjzff41FfuX8bSxdKcR81vTwcA", debtTokenProgram: TOKEN_PROGRAM,
    debtSupply: "21Skwocv5cJoftyejSTtXVaHJWTg88xcWGQtnRvUyKLx", debtFee: "CmMAn2UtLWHsQhwv31Trz4BZwVravs2jgxZYK2daTHaK",
  },
  {
    key: "prime_usdc", market: "CqAoLuqWtavaVE8deBjMKe8ZfSt9ghR6Vb8nfsyabyHA",
    collateralReserve: "BUTND9T7Ux4KR8RAEgd4WoZwnP7xA279oA1y3iPVcvSh", collateralMint: "3b8X44fLF9ooXaUm3hhSgjpmVs6rZZ3pPoGnGahc3Uu7",
    collateralSupply: "FkSkbRU5A6JXRXo5uaFwCS7jQ6jHYa1DxFtfpXfTz352", collateralReceiptMint: "FMKBCGqipyj5dm9C58Rb9ZWYeneDzrxd3YaL6amgZ8gW", collateralReceiptSupply: "Eg4wKFWc8aGfAqrcmYu3paz2afY5VqJMo17K95Y4VqFN",
    debtReserve: "9GJ9GBRwCp4pHmWrQ43L5xpc9Vykg7jnfwcFGN8FoHYu", debtMint: "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v", debtTokenProgram: TOKEN_PROGRAM,
    debtSupply: "H6JUwz8c61eQnYUx8avGXydKztKPyGvgWAUjmZUPS3BC", debtFee: "BzSw9sWTxUumr2wHhDiezkaLy3QZQS1KT4a9Fz8GvAQ6",
  },
  {
    key: "prime_pyusd", market: "CqAoLuqWtavaVE8deBjMKe8ZfSt9ghR6Vb8nfsyabyHA",
    collateralReserve: "BUTND9T7Ux4KR8RAEgd4WoZwnP7xA279oA1y3iPVcvSh", collateralMint: "3b8X44fLF9ooXaUm3hhSgjpmVs6rZZ3pPoGnGahc3Uu7",
    collateralSupply: "FkSkbRU5A6JXRXo5uaFwCS7jQ6jHYa1DxFtfpXfTz352", collateralReceiptMint: "FMKBCGqipyj5dm9C58Rb9ZWYeneDzrxd3YaL6amgZ8gW", collateralReceiptSupply: "Eg4wKFWc8aGfAqrcmYu3paz2afY5VqJMo17K95Y4VqFN",
    debtReserve: "3ZUAwhEtK8XWfK4fy98z4yoptm4GeyeAu21L11HPXaZ5", debtMint: "2b1kV6DkPAnxd5ixfnxCpjxmKwqjjaYmCZfHsFu24GXo", debtTokenProgram: TOKEN_2022_PROGRAM,
    debtSupply: "4LF3i8grZPRbk8d6gXvzRux4rYjGd5AmqrpLLYFpPKKt", debtFee: "4b9U55muKtwx9RimJSuztvyZaKWkmaoferVexgvxrYJr",
  },
  {
    key: "prime_usds", market: "CqAoLuqWtavaVE8deBjMKe8ZfSt9ghR6Vb8nfsyabyHA",
    collateralReserve: "BUTND9T7Ux4KR8RAEgd4WoZwnP7xA279oA1y3iPVcvSh", collateralMint: "3b8X44fLF9ooXaUm3hhSgjpmVs6rZZ3pPoGnGahc3Uu7",
    collateralSupply: "FkSkbRU5A6JXRXo5uaFwCS7jQ6jHYa1DxFtfpXfTz352", collateralReceiptMint: "FMKBCGqipyj5dm9C58Rb9ZWYeneDzrxd3YaL6amgZ8gW", collateralReceiptSupply: "Eg4wKFWc8aGfAqrcmYu3paz2afY5VqJMo17K95Y4VqFN",
    debtReserve: "7SzMWArC8WAenndXFmRyfvcvrNPodqUFkmPrmmoRZvn4", debtMint: "USDSwr9ApdHk5bvJKMjzff41FfuX8bSxdKcR81vTwcA", debtTokenProgram: TOKEN_PROGRAM,
    debtSupply: "5tP1kDJBYnjtrpUaRQhsrU1Y28ahiJVjz8p9mbqJFpz5", debtFee: "DjmdtvsvctUXCZ32y6UGdCEvXPTds6Ci7LFnVhw5HaQY",
  },
  {
    key: "syrup_usdc_usdc", market: "6WEGfej9B9wjxRs6t4BYpb9iCXd8CpTpJ8fVSNzHCC5y",
    collateralReserve: "AwCyCPZYJSZ93xcVKNK7jR8e1BHzJXq1D4bReNuh9woY", collateralMint: "AvZZF1YaZDziPY2RCK4oJrRVrbN3mTD9NL24hPeaZeUj",
    collateralSupply: "8Se5SK1Tty2bH4EQVrKW8hwr9Lc9E2cEbkaN59DpcB6i", collateralReceiptMint: "9gQ8M4WiFepY9skYntJZ5N3joa3RByiPqao61gMfmGMu", collateralReceiptSupply: "21GK6yHS3MKhTnF5pN5FuSmnpLiyPXTDrpxxbqMEoX58",
    debtReserve: "Atj6UREVWa7WxbF2EMKNyfmYUY1U1txughe2gjhcPDCo", debtMint: "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v", debtTokenProgram: TOKEN_PROGRAM,
    debtSupply: "BBcwMNSMyhhBnYE9pevEvkxKHGzTafMP9v3j7Kk7nAWM", debtFee: "HH7GLnRcGHJrdkEueVVj7mccNUjnSeWobDmtu9cHLkJV",
  },
  {
    key: "syrup_usdc_pyusd", market: "6WEGfej9B9wjxRs6t4BYpb9iCXd8CpTpJ8fVSNzHCC5y",
    collateralReserve: "AwCyCPZYJSZ93xcVKNK7jR8e1BHzJXq1D4bReNuh9woY", collateralMint: "AvZZF1YaZDziPY2RCK4oJrRVrbN3mTD9NL24hPeaZeUj",
    collateralSupply: "8Se5SK1Tty2bH4EQVrKW8hwr9Lc9E2cEbkaN59DpcB6i", collateralReceiptMint: "9gQ8M4WiFepY9skYntJZ5N3joa3RByiPqao61gMfmGMu", collateralReceiptSupply: "21GK6yHS3MKhTnF5pN5FuSmnpLiyPXTDrpxxbqMEoX58",
    debtReserve: "92qeAka3ZzCGPfJriDXrE7tiNqfATVCAM6ZjjctR3TrS", debtMint: "2b1kV6DkPAnxd5ixfnxCpjxmKwqjjaYmCZfHsFu24GXo", debtTokenProgram: TOKEN_2022_PROGRAM,
    debtSupply: "GUENeLN1ufX4K5622DbyYoQFhaWxMKoCFycvLSEYsykN", debtFee: "AwnzukUiajn7b3T9hXcwy19RLPZcHmLANUeqZnzXT6dU",
  },
] as const;

type Json = Record<string, unknown>;

function emit(verdict: string, condition: string, evidence: Json, exitCode: number): never {
  process.stdout.write(`${JSON.stringify({
    contractVersion: CONTRACT_VERSION,
    verdict,
    condition,
    evidence,
  }, null, 2)}\n`);
  process.stdout.write(`${verdict} ${condition}\n`);
  process.exit(exitCode);
}

function fail(condition: string, evidence: Json = {}): never {
  return emit(FAIL, condition, evidence, 2);
}

function blocked(condition: string, evidence: Json = {}): never {
  return emit(BLOCKED, condition, evidence, 2);
}

function sha256(value: string | Uint8Array): string {
  return createHash("sha256").update(value).digest("hex");
}

function record(value: unknown): Record<string, unknown> | null {
  return typeof value === "object" && value !== null && !Array.isArray(value)
    ? value as Record<string, unknown>
    : null;
}

function array(value: unknown): unknown[] {
  return Array.isArray(value) ? value : [];
}

function integer(value: unknown): bigint | null {
  if (typeof value === "bigint") return value;
  if (typeof value === "number" && Number.isSafeInteger(value)) return BigInt(value);
  if (typeof value === "string" && /^-?\d+$/.test(value)) return BigInt(value);
  return null;
}

function timestamp(value: unknown): number | null {
  if (typeof value !== "string" && !(value instanceof Date)) return null;
  const parsed = new Date(value).getTime();
  return Number.isFinite(parsed) ? parsed : null;
}

async function commandJson(command: string[], cwd = ROOT): Promise<unknown> {
  const child = Bun.spawn(command, { cwd, stdout: "pipe", stderr: "pipe" });
  const [exitCode, stdout, stderr] = await Promise.all([
    child.exited,
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
  ]);
  if (exitCode !== 0) {
    fail("read_only_command_failed", {
      command: command.join(" "),
      exitCode,
      stderrTail: stderr.split(/\r?\n/).slice(-12).join("\n"),
    });
  }
  try {
    return JSON.parse(stdout);
  } catch {
    fail("read_only_command_returned_invalid_json", {
      command: command.join(" "),
      stdoutSha256: sha256(stdout),
    });
  }
}

async function commandText(command: string[], cwd = ROOT): Promise<string> {
  const child = Bun.spawn(command, { cwd, stdout: "pipe", stderr: "pipe" });
  const [exitCode, stdout, stderr] = await Promise.all([
    child.exited,
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
  ]);
  if (exitCode !== 0) {
    fail("read_only_command_failed", {
      command: command.join(" "),
      exitCode,
      stderrTail: stderr.split(/\r?\n/).slice(-12).join("\n"),
    });
  }
  return stdout.trim();
}

function file(root: string, relative: string): string {
  const path = resolve(root, relative);
  if (!existsSync(path) || !statSync(path).isFile()) {
    fail("required_source_missing", { path });
  }
  return readFileSync(path, "utf8");
}

function relativeFiles(root: string): string[] {
  if (!existsSync(root)) return [];
  const result: string[] = [];
  const visit = (directory: string) => {
    for (const entry of readdirSync(directory, { withFileTypes: true })) {
      const path = resolve(directory, entry.name);
      if (entry.isDirectory()) visit(path);
      else if (entry.isFile()) result.push(path.slice(root.length + 1));
    }
  };
  visit(root);
  return result.sort();
}

function requireText(source: string, expected: string, condition: string, path: string): void {
  if (!source.includes(expected)) fail(condition, { path, expected });
}

function rejectText(source: string, forbidden: string, condition: string, path: string): void {
  if (source.includes(forbidden)) fail(condition, { path, forbidden });
}

function checkContractIdentity(): Json {
  const contract = file(ROOT, CONTRACT);
  if (sha256(contract) !== CONTRACT_SHA256) {
    fail("authoritative_contract_hash_drift", {
      expected: CONTRACT_SHA256,
      actual: sha256(contract),
      path: CONTRACT,
    });
  }
  for (const expected of [
    `**Version:** \`${CONTRACT_VERSION}\``,
    "op run --env-file=.env.1password -- bun run verify:earn-max:production",
    PASS,
    FAIL,
    BLOCKED,
    "the only product-readiness authority",
  ]) {
    requireText(contract, expected, "authoritative_contract_drift", CONTRACT);
  }
  const obsoleteMarkers = ["PASS", "FAIL", "BLOCKED"].map(
    (prefix) => `${prefix}_RWA_${"MULTIPLY_RELEASE_CANDIDATE"}`,
  );
  for (const obsolete of [...obsoleteMarkers, `One fixed ${"pooled Squads vault"}`]) {
    rejectText(contract, obsolete, "obsolete_product_contract_survived", CONTRACT);
  }

  const packageJson = file(ROOT, "package.json");
  const packageData = JSON.parse(packageJson) as { scripts?: Record<string, string> };
  const scripts = packageData.scripts ?? {};
  if (scripts["verify:earn-max:production"] !== "bun scripts/verify-multiply-production.ts") {
    fail("authoritative_verifier_entrypoint_missing", { path: "package.json" });
  }
  if (scripts[`verify:${"multiply:production"}`]) {
    fail("competing_product_verifier_entrypoint_survived", { path: "package.json" });
  }

  return { contract: CONTRACT, version: CONTRACT_VERSION, sha256: sha256(contract) };
}

function checkPerUserTopology(): Json {
  const config = file(ROOT, CONFIG);
  const fixedIdentityPatterns = [
    /pub const SETTINGS\s*:/,
    /pub const VAULT\s*:/,
    /pub const DELEGATE\s*:/,
    /pub const (?:SYRUP|USDC|PYUSD)_CUSTODY\s*:/,
    /pub const CLAIM_POLICY\s*:/,
    /obligation:\s*"[1-9A-HJ-NP-Za-km-z]+"/,
    /account:\s*"[1-9A-HJ-NP-Za-km-z]+"/,
  ];
  const matches = fixedIdentityPatterns
    .map((pattern) => config.match(pattern)?.[0])
    .filter((value): value is string => Boolean(value));
  if (matches.length > 0) {
    fail("multiply_topology_still_fixed", {
      path: CONFIG,
      matches,
      resume: "derive user-owned accounts and exactly three policy PDAs from Settings plus the earn-max-v2 manifest",
    });
  }
  for (const required of [
    "EarnMaxTopology",
    "derive_earn_max_topology",
    "manifest_version",
    'MANIFEST_VERSION: &str = "earn-max-v2"',
    "collateral_policy",
    "debt_policy",
    "swap_policy",
    "PYUSD_MINT",
    "USDS_MINT",
    "StrategyKey::OnycUsdc",
    "StrategyKey::OnycUsds",
    "StrategyKey::PrimeUsdc",
    "StrategyKey::PrimePyusd",
    "StrategyKey::PrimeUsds",
    "StrategyKey::SyrupUsdcUsdc",
    "StrategyKey::SyrupUsdcPyusd",
  ] as const) {
    requireText(config, required, "deterministic_per_user_topology_missing", CONFIG);
  }
  for (const forbidden of [
    "deposit_policy",
    "borrow_policy",
    "claim_to_collateral_policy",
    "debt_to_collateral_policy",
    "collateral_to_debt_policy",
    "collateral_to_claim_policy",
    "repay_policy",
    "withdraw_policy",
    "USX",
    "CASH_MINT",
    "USDG_MINT",
    "guard",
    "flashBorrow",
    "flash_borrow",
  ] as const) {
    rejectText(config, forbidden, "forbidden_strategy_or_program_survived", CONFIG);
  }
  return { path: CONFIG, sha256: sha256(config) };
}

function checkThreePolicySourceContract(): Json {
  const policy = file(ROOT, POLICY);
  const rustPolicy = file(ROOT, RUST_EARN_MAX_POLICY);
  const builder = file(ROOT, BUILDER);
  const executor = file(ROOT, EXECUTOR);
  const appActions = file(APPS_ROOT, APP_ACTIONS);
  const monitor = file(ROOT, POLICY_MONITOR);

  for (const required of [
    "PolicyFamily::Collateral",
    "PolicyFamily::Debt",
    "PolicyFamily::Swap",
    "CollateralLifecycle",
    "DebtLifecycle",
    "SwapRoutes",
  ]) {
    requireText(`${policy}\n${rustPolicy}\n${monitor}`, required, "three_policy_family_missing", `${POLICY} + ${RUST_EARN_MAX_POLICY} + ${POLICY_MONITOR}`);
  }
  for (const required of [
    "EarnMaxPolicyBoundary",
    "EarnMaxPolicyLane",
    "earn_max_policy_constraints",
    "validate_earn_max_jupiter_route",
    "three_policy_wire_contract_matches_the_typescript_sdk",
    "jupiter_mutation_boundary_rejects_value_redirection_and_quote_drift",
  ]) {
    requireText(rustPolicy, required, "rust_policy_byte_or_mutation_contract_missing", RUST_EARN_MAX_POLICY);
  }
  for (const required of [
    "pre_instructions",
    "policy_instructions",
  ]) {
    requireText(`${builder}\n${executor}`, required, "top_level_refresh_split_missing", `${BUILDER} + ${EXECUTOR}`);
  }
  requireText(
    executor,
    "policy_instructions.len() != 1",
    "single_terminal_execution_gate_missing",
    EXECUTOR,
  );
  for (const required of [
    'EARN_MAX_MANIFEST_VERSION = "earn-max-v2"',
    '"collateral"',
    '"debt"',
    '"swap"',
    "PYUSD_MINT",
    "USDS_MINT",
    '"onyc_usdc"',
    '"onyc_usds"',
    '"prime_usdc"',
    '"prime_pyusd"',
    '"prime_usds"',
    '"syrup_usdc_usdc"',
    '"syrup_usdc_pyusd"',
    "strategy.debtCustody",
    "strategy.collateralCustody",
    "accountDataPubkey",
    "swapBiclique",
    "AddressLookupTableProgram.createLookupTable",
    '"legacy"',
  ]) {
    requireText(appActions, required, "three_policy_client_manifest_missing", APP_ACTIONS);
  }
  for (const forbidden of [
    '"forward_swap"',
    '"reverse_swap"',
    "PolicyFamily::Deposit",
    "PolicyFamily::Borrow",
    "PolicyFamily::Repay",
    "PolicyFamily::Withdraw",
    "updateProgramInteractionPolicyInstruction",
    "updateInstruction",
  ]) {
    rejectText(`${policy}\n${appActions}`, forbidden, "six_policy_manifest_survived", `${POLICY} + ${APP_ACTIONS}`);
  }

  return {
    policySha256: sha256(policy),
    builderSha256: sha256(builder),
    executorSha256: sha256(executor),
    rustPolicySha256: sha256(rustPolicy),
    appActionsSha256: sha256(appActions),
    monitorSha256: sha256(monitor),
  };
}

function checkMinimalSchemaSource(): Json {
  const migrationRoot = resolve(ROOT, MIGRATIONS);
  const migrations = relativeFiles(migrationRoot).filter((path) => path.endsWith(".sql"));
  const source = migrations.map((path) => file(migrationRoot, path)).join("\n");
  const runner = file(ROOT, MIGRATION_RUNNER);
  for (const table of [
    "earn_max_policy_sets",
    "multiply_route_states",
    "multiply_operations",
    "multiply_position_snapshots",
  ]) {
    requireText(source, table, "earn_max_schema_table_missing", MIGRATIONS);
  }
  requireText(
    source,
    "multiply_operations_one_nonterminal_per_route",
    "multiply_one_nonterminal_constraint_missing",
    MIGRATIONS,
  );
  for (const required of [
    "0064_earn_max_partial_lifecycle.sql",
    "0067_earn_max_three_policy_v2.sql",
    "source_instruction_index",
    "state - 'frontend' - 'targetStrategyKey'",
    "request_withdrawal",
    "cancel_withdrawal",
  ]) {
    requireText(
      `${migrations.join("\n")}\n${source}`,
      required,
      "earn_max_partial_lifecycle_schema_missing",
      MIGRATIONS,
    );
  }
  for (const forbidden of [
    "earn_max_policy_events",
    "earn_max_decisions",
    "earn_max_commands",
    "earn_max_jobs",
    "earn_max_sagas",
    "earn_max_outbox",
    "earn_max_registry",
    "earn_max_confirmations",
  ]) {
    rejectText(source, forbidden, "forbidden_earn_max_table_survived", MIGRATIONS);
  }
  for (const required of [
    'version: 67',
    'name: "earn_max_three_policy_v2"',
    '0067_earn_max_three_policy_v2.sql',
  ]) {
    requireText(
      runner,
      required,
      "earn_max_production_migration_registry_missing",
      MIGRATION_RUNNER,
    );
  }
  return {
    migrations,
    sha256: sha256(source),
    runnerSha256: sha256(runner),
  };
}

function checkLaserStreamSource(): Json {
  const monitor = file(ROOT, POLICY_MONITOR);
  const manifest = file(ROOT, POLICY_MONITOR_MANIFEST);
  const laserstream = file(ROOT, LASERSTREAM_MONITOR);
  const laserstreamSource = file(ROOT, LASERSTREAM_SOURCE);
  const reconciliation = file(ROOT, LASERSTREAM_RECONCILIATION);
  requireText(manifest, "loyal-fleet-worker", "policy_projection_manifest_contract_missing", POLICY_MONITOR_MANIFEST);
  for (const required of [
    "UpdateSourceKind::Laserstream",
    "with_earn_max_projection",
    "PolicyCommitment::Confirmed",
    "EARN_MAX_DELEGATE",
    "LaserstreamPolicyUpdateSource",
    "earn_max_policy_replay_start_slot",
  ]) {
    requireText(laserstream, required, "earn_max_projection_not_owned_by_existing_laserstream", LASERSTREAM_MONITOR);
  }
  for (const required of [
    "SubscribeRequestFilterTransactions",
    "CommitmentLevel::Confirmed",
    "SQUADS_SMART_ACCOUNT_PROGRAM_ID.to_string()",
    "process_earn_max_policy_update",
  ]) {
    requireText(laserstreamSource, required, "earn_max_confirmed_transaction_stream_missing", LASERSTREAM_SOURCE);
  }
  requireText(
    reconciliation,
    "read_confirmed_squads_policy_transaction",
    "laserstream_policy_reconciliation_bridge_missing",
    LASERSTREAM_RECONCILIATION,
  );
  for (const required of [
    "parse_earn_max_intent",
    "project_earn_max_intent",
    "project_earn_max_cash_flow",
    "earn_max_cash_flows",
    "source_instruction_index",
    "CommitmentConfig::confirmed()",
    "memo.accounts.contains(&vault_pubkey)",
    "has_squads_execution",
    "transaction.signers.as_slice() != [*wallet]",
    "memo.source_instruction_index % 256 != 0",
    "memo.source_instruction_index % 256 == 0",
    "let [wallet] = memo.accounts.as_slice()",
    "let [vault] = memo.accounts.as_slice()",
    "derive_associated_token_account(*wallet, USDC_MINT, spl_token::ID)",
    "read_confirmed_earn_max_transfer",
    ".destination_post",
    ".saturating_sub(transfer.destination_pre)",
    "source_instruction_index: Some",
    "signed_wire_sha256: None",
    '["loyal", "earn-max", "v2"]',
  ]) {
    requireText(
      reconciliation,
      required,
      "earn_max_intent_projection_missing",
      LASERSTREAM_RECONCILIATION,
    );
  }
  for (const required of [
    "project_earn_max_policy_set",
    "current_policy_matches",
    "get_multiple_accounts_with_commitment",
  ]) {
    requireText(monitor, required, "policy_projection_contract_missing", POLICY_MONITOR);
  }
  return {
    policyMonitorSha256: sha256(monitor),
    laserstreamOwnerSha256: sha256(laserstream),
    laserstreamSourceSha256: sha256(laserstreamSource),
    reconciliationSha256: sha256(reconciliation),
  };
}

function checkAppSource(): Json {
  if (!existsSync(APPS_ROOT)) fail("loyal_apps_checkout_missing", { path: APPS_ROOT });
  const apiRoot = resolve(APPS_ROOT, APP_API_ROOT);
  const files = relativeFiles(apiRoot).filter((path) => path.endsWith("route.ts"));
  const expected = ["activity/route.ts", "summary/route.ts"];
  if (JSON.stringify(files) !== JSON.stringify(expected)) {
    fail("earn_max_endpoint_inventory_drift", { expected, actual: files });
  }
  const featureRoot = resolve(APPS_ROOT, APP_FEATURE_ROOT);
  const featureFiles = relativeFiles(featureRoot).filter((path) => /\.tsx?$/.test(path));
  const source = [
    ...files.map((path) => file(apiRoot, path)),
    ...featureFiles.map((path) => file(featureRoot, path)),
    file(APPS_ROOT, APP_ACTIONS),
    file(APPS_ROOT, APP_UI),
  ].join("\n");
  for (const forbidden of [
    "programId", "policySeed", "claimDestination", "/confirm",
    "prepareTransaction", "requestWithdrawal", "requestEarnMaxWithdrawal",
  ] as const) {
    rejectText(files.map((path) => file(apiRoot, path)).join("\n"), forbidden, "earn_max_arbitrary_or_confirmation_surface", APP_API_ROOT);
  }
  for (const required of [
    "buildEarnMaxInstallInstructions",
    "buildEarnMaxDepositInstructions",
    "buildEarnMaxWithdrawalRequestInstructions",
    "buildEarnMaxWithdrawalCancelInstructions",
    "buildEarnMaxClaimInstructions",
    "buildEarnMaxCloseInstructions",
    "createEarnMaxPolicyManifest",
    "resolveEarnMaxInstallSeedBase",
    "EarnMaxViewModel",
    "EarnMaxSummaryResponse",
    "EarnMaxActivityResponse",
    "history_incomplete",
    "realized_apy_bps",
    "forecast_apy_bps",
    "x-loyal-deployment-revision",
    "loyal:earn-max:v2:withdraw:",
    "loyal:earn-max:v2:cancel:",
    "confirm: true",
  ]) {
    requireText(source, required, "earn_max_application_contract_missing", APP_FEATURE_ROOT);
  }
  for (const forbidden of [
    "preparation_pending", "SOLANA_TESTING_PK", "flashBorrow", "flash_borrow",
    "guard", "hook", "EARN_MAX_BALANCE_USD", "EARN_MAX_APY_LABEL",
    "NOOP_EXECUTE_NOW", "mock no-op", "Mocked Earn MAX", "Promise<unknown>",
    "useState<unknown>", 'readJson("/api/smart-accounts/earn-max/performance")',
  ]) {
    rejectText(source, forbidden, "earn_max_application_placeholder_or_forbidden_graph", APP_FEATURE_ROOT);
  }
  requireText(
    file(APPS_ROOT, "apps/web/tsconfig.earn-max.json"),
    "src/features/earn-max/**/*.ts",
    "earn_max_scoped_typecheck_missing",
    "apps/web/tsconfig.earn-max.json",
  );
  requireText(
    file(APPS_ROOT, APP_SHELL),
    "const EARN_MAX_VISIBLE = false",
    "earn_max_not_hidden_before_release",
    APP_SHELL,
  );
  return { root: APP_API_ROOT, files, featureFiles, sha256: sha256(source) };
}

function checkWorkerAndStoreSource(): Json {
  const worker = file(ROOT, "crates/loyal-fleet-worker/src/multiply/mod.rs");
  const state = file(ROOT, "crates/loyal-yield-store/src/fleet_orchestration/multiply.rs");
  const planner = file(ROOT, "crates/loyal-fleet-worker/src/multiply/planner.rs");
  const store = file(ROOT, MULTIPLY_STORE);
  for (const required of [
    "bootstrap_ready_route",
    "record_multiply_position_snapshot",
    "confirmed_kamino_reserve_curve_500ms",
    "forecast_apy_bps",
  ]) {
    requireText(worker, required, "earn_max_worker_bridge_missing", "crates/loyal-fleet-worker/src/multiply/mod.rs");
  }
  for (const required of [
    "load_unbootstrapped_earn_max_policy_set",
    "confirmed_claim_transfer",
    "AND NOT COALESCE((",
  ]) {
    requireText(store, required, "earn_max_store_contract_missing", MULTIPLY_STORE);
  }
  for (const forbidden of [
    "find_confirmed_deposit",
    "admit_next_confirmed_deposit",
    "admit_next_confirmed_claim",
    "get_signatures_for_address_with_config",
  ]) {
    rejectText(worker, forbidden, "duplicate_worker_chain_ingestion_survived", "crates/loyal-fleet-worker/src/multiply/mod.rs");
  }
  for (const forbidden of ["MultiplyFrontendView", "pub frontend:", "RouteGoal::Move", "request_move"] as const) {
    rejectText(`${state}\n${planner}`, forbidden, "unproven_or_duplicated_route_state_survived", "crates/loyal-yield-store/src/fleet_orchestration/multiply.rs");
  }
  rejectText(worker, "build_operation(.*MultiplyAction::Claim", "delegate_claim_execution_survived", "crates/loyal-fleet-worker/src/multiply/mod.rs");
  requireText(planner, "if observed.claim.amount_raw > 0", "active_position_top_up_not_deployed", "crates/loyal-fleet-worker/src/multiply/planner.rs");
  requireText(state, "ready_by", "withdrawal_sla_not_explicit", "crates/loyal-yield-store/src/fleet_orchestration/multiply.rs");
  requireText(state, "cancel_withdrawal", "withdrawal_cancellation_state_missing", "crates/loyal-yield-store/src/fleet_orchestration/multiply.rs");
  requireText(state, "roll_terminal_policy_seed_base", "repeated_policy_install_state_transition_missing", "crates/loyal-yield-store/src/fleet_orchestration/multiply.rs");
  requireText(store, "interval '30 seconds'", "withdrawal_cancel_grace_missing", MULTIPLY_STORE);
  return {
    workerSha256: sha256(worker),
    stateSha256: sha256(state),
    plannerSha256: sha256(planner),
    storeSha256: sha256(store),
  };
}

async function targetedChecks(): Promise<Json> {
  const commands: Array<{ command: string[]; cwd: string }> = [
    { command: ["cargo", "test", "-q", "-p", "loyal-actions", "earn_max::tests"], cwd: ROOT },
    { command: ["cargo", "check", "-q", "-p", "loyal-squads-policy-monitor"], cwd: ROOT },
    { command: ["cargo", "check", "-q", "-p", "balance-sweep-ata-monitor", "--bin", "balance-sweep-ata-monitor"], cwd: ROOT },
    { command: ["cargo", "check", "-q", "-p", "loyal-fleet-worker", "--bin", "multiply-route-worker"], cwd: ROOT },
    {
      command: [
        "bunx", "turbo", "run", "build",
        "--filter=@loyal-labs/smart-account-vaults...",
        "--filter=@loyal-labs/actions...",
        "--filter=@loyal-labs/auth-core...",
        "--filter=@loyal-labs/db-adapter-neon...",
      ],
      cwd: APPS_ROOT,
    },
    { command: ["bunx", "tsc", "-p", "apps/web/tsconfig.earn-max.json", "--pretty", "false"], cwd: APPS_ROOT },
    { command: ["bun", "test", "packages/loyal-actions/test/sdk.test.ts"], cwd: APPS_ROOT },
  ];
  const results: Json[] = [];
  for (const check of commands) {
    const child = Bun.spawn(check.command, { cwd: check.cwd, stdout: "pipe", stderr: "pipe" });
    const [exitCode, stdout, stderr] = await Promise.all([
      child.exited,
      new Response(child.stdout).text(),
      new Response(child.stderr).text(),
    ]);
    if (exitCode !== 0) {
      fail("targeted_compile_failed", {
        command: check.command.join(" "),
        cwd: check.cwd,
        exitCode,
        stdoutSha256: sha256(stdout),
        stderrSha256: sha256(stderr),
        stdoutTail: stdout.split(/\r?\n/).slice(-20).join("\n"),
        stderrTail: stderr.split(/\r?\n/).slice(-20).join("\n"),
      });
    }
    results.push({ command: check.command.join(" "), cwd: check.cwd, exitCode });
  }
  return { results };
}

function requiredEnv(name: string): string {
  const value = process.env[name]?.trim();
  if (!value) {
    blocked("terminal_environment_missing", {
      variable: name,
      resume: "run the sole verifier through op run --env-file=.env.1password",
    });
  }
  return value;
}

function reservePubkey(data: Buffer, offset: number): string {
  if (data.length !== 8_624 || offset < 0 || offset + 32 > data.length) {
    fail("mainnet_kamino_reserve_layout_drift", { dataLength: data.length, offset });
  }
  return new PublicKey(data.subarray(offset, offset + 32)).toBase58();
}

async function checkMainnetPoolCatalog(): Promise<Json> {
  const connection = new Connection(requiredEnv("SOLANA_RPC_URL"), {
    commitment: "confirmed",
    httpAgent: false,
  });
  const keys = [...new Set(MAINNET_POOL_CATALOG.flatMap((pool) => [
    pool.market,
    pool.collateralReserve,
    pool.collateralMint,
    pool.collateralSupply,
    pool.collateralReceiptMint,
    pool.collateralReceiptSupply,
    pool.debtReserve,
    pool.debtMint,
    pool.debtSupply,
    pool.debtFee,
  ]))].map((value) => new PublicKey(value));
  const response = await connection.getMultipleAccountsInfoAndContext(keys, {
    commitment: "confirmed",
  });
  const accounts = new Map(keys.map((key, index) => [key.toBase58(), response.value[index]]));
  const owner = (address: string, expected: PublicKey, field: string, pool: string) => {
    const account = accounts.get(address);
    if (!account || !account.owner.equals(expected)) {
      fail("mainnet_pool_catalog_account_drift", {
        pool,
        field,
        address,
        expectedOwner: expected.toBase58(),
        actualOwner: account?.owner.toBase58() ?? null,
      });
    }
    return account;
  };
  const checkedReserves = new Set<string>();
  for (const pool of MAINNET_POOL_CATALOG) {
    owner(pool.market, KLEND_PROGRAM, "market", pool.key);
    owner(pool.collateralMint, TOKEN_PROGRAM, "collateralMint", pool.key);
    owner(pool.debtMint, pool.debtTokenProgram, "debtMint", pool.key);
    for (const [kind, reserveAddress, expected] of [
      ["collateral", pool.collateralReserve, {
        market: pool.market,
        mint: pool.collateralMint,
        tokenProgram: TOKEN_PROGRAM.toBase58(),
        supply: pool.collateralSupply,
        collateralMint: pool.collateralReceiptMint,
        collateralSupply: pool.collateralReceiptSupply,
      }],
      ["debt", pool.debtReserve, {
        market: pool.market,
        mint: pool.debtMint,
        tokenProgram: pool.debtTokenProgram.toBase58(),
        supply: pool.debtSupply,
        fee: pool.debtFee,
      }],
    ] as const) {
      if (checkedReserves.has(reserveAddress)) continue;
      const reserve = owner(reserveAddress, KLEND_PROGRAM, `${kind}Reserve`, pool.key);
      const data = Buffer.from(reserve.data);
      const actual = {
        market: reservePubkey(data, 32),
        mint: reservePubkey(data, 128),
        supply: reservePubkey(data, 160),
        fee: reservePubkey(data, 192),
        tokenProgram: reservePubkey(data, 408),
        collateralMint: reservePubkey(data, 2_560),
        collateralSupply: reservePubkey(data, 2_600),
      };
      for (const [field, value] of Object.entries(expected)) {
        if (actual[field as keyof typeof actual] !== value) {
          fail("mainnet_pool_catalog_reserve_identity_drift", {
            pool: pool.key,
            kind,
            reserve: reserveAddress,
            field,
            expected: value,
            actual: actual[field as keyof typeof actual],
            slot: response.context.slot,
          });
        }
      }
      const derivedAuthority = PublicKey.findProgramAddressSync(
        [Buffer.from("lma"), new PublicKey(pool.market).toBytes()],
        KLEND_PROGRAM,
      )[0];
      const configuredAuthority = kind === "collateral"
        ? file(ROOT, CONFIG).includes(derivedAuthority.toBase58())
        : true;
      if (!configuredAuthority) {
        fail("mainnet_pool_catalog_market_authority_drift", {
          pool: pool.key,
          market: pool.market,
          derivedAuthority: derivedAuthority.toBase58(),
        });
      }
      checkedReserves.add(reserveAddress);
    }
  }
  return {
    slot: response.context.slot,
    pools: MAINNET_POOL_CATALOG.map((pool) => pool.key),
    uniqueReserves: checkedReserves.size,
    accountCount: keys.length,
  };
}

async function checkSquadsLegacyPolicyCompatibility(): Promise<Json> {
  const connection = new Connection(requiredEnv("SOLANA_RPC_URL"), {
    commitment: "confirmed",
    httpAgent: false,
  });
  const program = await connection.getAccountInfo(SQUADS_PROGRAM, "confirmed");
  if (
    !program ||
    !program.executable ||
    !program.owner.equals(BPF_UPGRADEABLE_LOADER) ||
    program.data.length !== 36 ||
    program.data.readUInt32LE(0) !== 2
  ) {
    fail("deployed_squads_program_account_invalid", {
      program: SQUADS_PROGRAM.toBase58(),
      exists: Boolean(program),
      executable: program?.executable ?? null,
      owner: program?.owner.toBase58() ?? null,
      dataLength: program?.data.length ?? null,
      stateTag: program && program.data.length >= 4 ? program.data.readUInt32LE(0) : null,
    });
  }

  const programDataAddress = new PublicKey(program.data.subarray(4, 36));
  const programData = await connection.getAccountInfo(programDataAddress, "confirmed");
  if (
    !programData ||
    !programData.owner.equals(BPF_UPGRADEABLE_LOADER) ||
    programData.data.length < 13 ||
    programData.data.readUInt32LE(0) !== 3
  ) {
    fail("deployed_squads_program_data_invalid", {
      program: SQUADS_PROGRAM.toBase58(),
      programData: programDataAddress.toBase58(),
      exists: Boolean(programData),
      owner: programData?.owner.toBase58() ?? null,
      dataLength: programData?.data.length ?? null,
      stateTag: programData && programData.data.length >= 4
        ? programData.data.readUInt32LE(0)
        : null,
    });
  }

  const deployedSlot = Number(programData.data.readBigUInt64LE(4));
  const programDataSha256 = sha256(programData.data);
  if (!SUPPORTED_LEGACY_POLICY_PROGRAM_DATA_HASHES.has(programDataSha256)) {
    blocked("deployed_squads_legacy_policy_payload_unverified", {
      program: SQUADS_PROGRAM.toBase58(),
      programData: programDataAddress.toBase58(),
      deployedSlot,
      programDataSha256,
      expectedInstallPackets: { collateral: 1_138, debt: 1_138, swapV0: 1_227 },
      resume: "independently prove direct legacy PolicyCreate deserialization for this exact ProgramData hash before allowlisting it",
    });
  }

  return {
    program: SQUADS_PROGRAM.toBase58(),
    programData: programDataAddress.toBase58(),
    deployedSlot,
    programDataSha256,
  };
}

async function checkLivePrerequisites(): Promise<Json> {
  const rpcUrl = requiredEnv("SOLANA_RPC_URL");
  const databaseUrl = requiredEnv("NEON_DATABASE_URL");

  const connection = new Connection(rpcUrl, { commitment: "confirmed", httpAgent: false });
  const genesisHash = await connection.getGenesisHash();
  if (genesisHash !== MAINNET_GENESIS) fail("rpc_not_mainnet_beta", { genesisHash });

  const sql = neon(databaseUrl);
  const rows = await sql`
    SELECT table_name
    FROM information_schema.tables
    WHERE table_schema = 'loyal_yield'
      AND table_name IN (
        'earn_max_policy_sets',
        'multiply_route_states',
        'multiply_operations',
        'multiply_position_snapshots',
        'projection_offsets'
      )
    ORDER BY table_name
  `;
  const tables = rows.map((row) => String(row.table_name));
  const expected = [
    "earn_max_policy_sets",
    "multiply_operations",
    "multiply_position_snapshots",
    "multiply_route_states",
    "projection_offsets",
  ];
  if (JSON.stringify(tables) !== JSON.stringify(expected)) {
    fail("deployed_earn_max_schema_incomplete", { expected, actual: tables });
  }
  const migrations = await sql`
    SELECT version, name, checksum
    FROM loyal_yield.schema_migrations
    WHERE version IN (54, 55, 56, 64, 66, 67)
    ORDER BY version
  `;
  if (
    migrations.length !== 6 ||
    String(migrations[0]?.name) !== "earn_max_per_user" ||
    String(migrations[1]?.name) !== "earn_max_repeated_lifecycle" ||
    String(migrations[2]?.name) !== "earn_max_dynamic_policy_seeds" ||
    String(migrations[3]?.name) !== "earn_max_partial_lifecycle" ||
    String(migrations[4]?.name) !== "earn_max_single_owner_state" ||
    String(migrations[5]?.name) !== "earn_max_three_policy_v2"
  ) {
    fail("deployed_earn_max_migration_missing", { migrations });
  }
  const liveRoutes = ["summary", "activity"];
  const routeEvidence: Json[] = [];
  let deployedRevision: string | null = null;
  for (const route of liveRoutes) {
    const response = await fetch(`${APP_URL}/api/smart-accounts/earn-max/${route}`, {
      redirect: "manual",
    });
    const contractHeader = response.headers.get("x-loyal-earn-max-contract");
    const revision = response.headers.get("x-loyal-deployment-revision");
    if (
      response.status !== 401 ||
      contractHeader !== CONTRACT_VERSION ||
      !revision?.match(/^[0-9a-f]{40}$/) ||
      (deployedRevision !== null && revision !== deployedRevision)
    ) {
      fail("deployed_earn_max_application_identity_missing", {
        route,
        status: response.status,
        contractHeader,
        revision,
        deployedRevision,
      });
    }
    deployedRevision = revision;
    routeEvidence.push({ route, status: response.status, contractHeader, revision });
  }
  for (const removed of ["state", "performance", "history", "withdrawals", "transactions/prepare"]) {
    const response = await fetch(`${APP_URL}/api/smart-accounts/earn-max/${removed}`, {
      redirect: "manual",
    });
    if (response.status !== 404) {
      fail("deployed_earn_max_mutation_or_obsolete_endpoint_survived", {
        route: removed,
        status: response.status,
      });
    }
  }
  const localAppTree = await commandText(["git", "rev-parse", "HEAD^{tree}"], APPS_ROOT);
  const deployedAppTree = await commandText([
    "gh", "api", `repos/loyal-labs/loyal-app/git/commits/${deployedRevision}`, "--jq", ".tree.sha",
  ]);
  if (deployedAppTree !== localAppTree) {
    fail("deployed_earn_max_application_revision_drift", {
      deployedRevision,
      deployedAppTree,
      localAppTree,
    });
  }
  return {
    genesisHash,
    tables,
    migrations: migrations.map((migration) => ({
      version: migration.version,
      name: migration.name,
      checksum: migration.checksum,
    })),
    deployedRevision,
    routes: routeEvidence,
  };
}

async function checkDeployedWorkers(
  monitorImageRevision: string,
  workerImageRevision: string,
): Promise<Json> {
  const expected = {
    [POLICY_MONITOR_SERVICE]: `ghcr.io/loyal-labs/loyal-yield-routing/laserstream-workers:sha-${monitorImageRevision}`,
    [MULTIPLY_WORKER_SERVICE]: `ghcr.io/loyal-labs/loyal-yield-routing/light-workers:sha-${workerImageRevision}`,
  };
  const evidence: Json[] = [];
  for (const [service, image] of Object.entries(expected)) {
    const deploys = array(await commandJson(["render", "deploys", "list", service, "-o", "json"]));
    const latest = record(deploys[0]);
    const deployedImage = record(latest?.image)?.ref;
    if (latest?.status !== "live" || deployedImage !== image) {
      fail("earn_max_worker_deployment_drift", {
        service,
        expectedImage: image,
        actualStatus: latest?.status,
        actualImage: deployedImage,
      });
    }
    evidence.push({
      service,
      deployId: latest.id,
      image: deployedImage,
      status: latest.status,
      createdAt: latest.createdAt,
      finishedAt: latest.finishedAt,
    });
  }
  return { services: evidence };
}

async function confirmedSignatures(
  connection: Connection,
  signatures: string[],
): Promise<Json> {
  const invalidFormat = signatures.filter((value) => !/^[1-9A-HJ-NP-Za-km-z]{80,90}$/.test(value));
  const unique = [...new Set(signatures.filter((value) => /^[1-9A-HJ-NP-Za-km-z]{80,90}$/.test(value)))];
  if (invalidFormat.length > 0 || unique.length === 0) {
    fail("earn_max_lifecycle_signature_inventory_invalid", {
      supplied: signatures.length,
      unique: unique.length,
      invalidFormatCount: invalidFormat.length,
    });
  }
  const statuses = await connection.getSignatureStatuses(unique, { searchTransactionHistory: true });
  const invalid = unique.flatMap((signature, index) => {
    const status = statuses.value[index];
    return status && status.err === null && ["confirmed", "finalized"].includes(status.confirmationStatus ?? "")
      ? []
      : [{ signature, status }];
  });
  if (invalid.length > 0) fail("earn_max_lifecycle_signature_not_confirmed", { invalid });
  return { count: unique.length, slots: statuses.value.map((status) => status?.slot ?? null) };
}

async function checkFreshLifecycle(): Promise<Json> {
  const rpcUrl = requiredEnv("SOLANA_RPC_URL");
  const databaseUrl = requiredEnv("NEON_DATABASE_URL");
  const sql = neon(databaseUrl);
  const connection = new Connection(rpcUrl, { commitment: "confirmed", httpAgent: false });
  const policies = await sql`
    SELECT * FROM loyal_yield.earn_max_policy_sets
    WHERE settings = ${VERIFY_SETTINGS} AND vault_index = 0
    LIMIT 1
  `;
  const policy = record(policies[0]);
  const bindings = array(policy?.policy_accounts).map(record).filter((value): value is Record<string, unknown> => value !== null);
  const seeds = bindings.map((binding) => integer(binding.seed));
  const accounts = bindings.map((binding) => String(binding.account ?? ""));
  const base = integer(policy?.policy_seed_base);
  const actualSeeds = seeds.filter((seed): seed is bigint => seed !== null);
  const expectedSeeds = base === null
    ? []
    : Array.from({ length: 3 }, (_, index) => base + BigInt(index));
  if (
    policy?.manifest_version !== POLICY_MANIFEST_VERSION ||
    policy?.status !== "removed" ||
    base === null || base <= 0n ||
    bindings.length !== 3 ||
    actualSeeds.length !== 3 ||
    new Set(actualSeeds).size !== 3 ||
    expectedSeeds.some((seed) => !actualSeeds.includes(seed)) ||
    new Set(accounts).size !== 3 ||
    bindings.some((binding) => binding.matches !== false)
  ) {
    blocked("fresh_laserstream_policy_removal_missing", {
      settings: VERIFY_SETTINGS,
      policyStatus: policy?.status,
      policySeedBase: policy?.policy_seed_base,
      policySeeds: actualSeeds.map(String),
      policyCount: bindings.length,
      resume: "complete the fresh confirmed install-to-removal product lifecycle",
    });
  }

  const routes = await sql`
    SELECT route_key, settings, vault_index, vault, state, state_version, updated_at
    FROM loyal_yield.multiply_route_states
    WHERE settings = ${VERIFY_SETTINGS} AND vault_index = 0
    LIMIT 1
  `;
  const route = record(routes[0]);
  const state = record(route?.state);
  const deposit = record(state?.deposit);
  const withdrawal = record(state?.withdrawal);
  const depositAmount = integer(deposit?.amountRaw);
  const walletPre = integer(deposit?.walletPreAmountRaw);
  const walletPost = integer(deposit?.walletPostAmountRaw);
  const vaultPre = integer(deposit?.vaultPreAmountRaw);
  const vaultPost = integer(deposit?.vaultPostAmountRaw);
  const requestedAt = timestamp(withdrawal?.requestedAt);
  const readyBy = timestamp(withdrawal?.readyBy);
  const unwindAt = timestamp(withdrawal?.unwindCompletedAt);
  if (
    state?.schemaVersion !== 9 ||
    state?.engineVersion !== "earn_max_v2" ||
    integer(state?.policySeedBase) !== base ||
    state?.goal !== "claimed" ||
    state?.currentOperationId !== null ||
    state?.manualRecoveryReason !== null ||
    withdrawal?.status !== "claimed" ||
    depositAmount === null || depositAmount <= 0n ||
    walletPre === null || walletPost === null || walletPre - walletPost !== depositAmount ||
    vaultPre === null || vaultPost === null || vaultPost - vaultPre !== depositAmount ||
    requestedAt === null || readyBy === null || unwindAt === null ||
    readyBy - requestedAt !== 600_000 || unwindAt < requestedAt || unwindAt > readyBy
  ) {
    blocked("fresh_claimed_route_reconciliation_missing", {
      settings: VERIFY_SETTINGS,
      goal: state?.goal,
      withdrawalStatus: withdrawal?.status,
      readyBy,
      unwindMilliseconds: requestedAt !== null && unwindAt !== null ? unwindAt - requestedAt : null,
      resume: "complete and reconcile the fresh deposit, unwind, and claim lifecycle within 600 seconds",
    });
  }

  const operations = await sql`
    SELECT operation_id, action, status, transaction_signature,
           source_instruction_index, confirmed_slot, expected_effects,
           created_at, updated_at
    FROM loyal_yield.multiply_operations
    WHERE route_key = ${String(route?.route_key ?? "")}
    ORDER BY created_at, operation_id
  `;
  const requiredActions = [
    "request_withdrawal",
    "cancel_withdrawal",
    "deposit_claim_asset",
    "swap_claim_to_collateral",
    "deposit_collateral",
    "borrow_debt",
    "withdraw_collateral",
    "repay_debt",
    "withdraw_remaining_collateral",
    "swap_collateral_to_claim",
    "claim",
  ];
  const reconciledOperations = operations.filter((operation) => operation.status === "reconciled");
  const expiredOperations = operations.filter((operation) => operation.status === "expired");
  const unexpectedOperations = operations.filter(
    (operation) => operation.status !== "reconciled" && operation.status !== "expired",
  );
  const expiredSignatures = expiredOperations.map((operation) => String(operation.transaction_signature ?? ""));
  const expiredStatuses = expiredSignatures.length === 0
    ? []
    : (await connection.getSignatureStatuses(expiredSignatures, { searchTransactionHistory: true })).value;
  const unsafeExpiredOperations = expiredOperations.filter((operation, index) =>
    operation.confirmed_slot !== null ||
    operation.source_instruction_index !== null ||
    !/^[1-9A-HJ-NP-Za-km-z]{80,90}$/.test(expiredSignatures[index] ?? "") ||
    expiredStatuses[index] !== null
  );
  const actionSet = new Set(reconciledOperations.map((operation) => String(operation.action)));
  const actionCount = (action: string) =>
    reconciledOperations.filter((operation) => operation.action === action).length;
  const chainLocations = reconciledOperations
    .filter((operation) => operation.source_instruction_index !== null)
    .map((operation) => `${operation.transaction_signature}:${operation.source_instruction_index}`);
  const intentLocationsUnique = new Set(chainLocations).size === chainLocations.length;
  if (
    operations.length === 0 ||
    requiredActions.some((action) => !actionSet.has(action)) ||
    unexpectedOperations.length > 0 ||
    unsafeExpiredOperations.length > 0 ||
    actionCount("deposit_claim_asset") < 2 ||
    actionCount("request_withdrawal") < 3 ||
    actionCount("cancel_withdrawal") < 1 ||
    actionCount("claim") < 2 ||
    !intentLocationsUnique
  ) {
    blocked("fresh_hookless_operation_graph_incomplete", {
      actions: [...actionSet].sort(),
      counts: Object.fromEntries(requiredActions.map((action) => [action, actionCount(action)])),
      intentLocationsUnique,
      unexpected: unexpectedOperations.map((operation) => ({ id: operation.operation_id, status: operation.status })),
      unsafeExpired: unsafeExpiredOperations.map((operation) => operation.operation_id),
      resume: "complete the confirmed deposit, top-up, cancel, partial/full claim, and hookless open/unwind graph",
    });
  }

  const operationSlot = (operation: Record<string, unknown>) => integer(operation.confirmed_slot);
  const intent = (operation: Record<string, unknown>) =>
    record(record(operation.expected_effects)?.intent);
  const claimSourcePost = (operation: Record<string, unknown>): bigint | null => {
    const effects = record(operation.expected_effects);
    const before = array(effects?.tokenAmountsBefore)
      .map(record)
      .filter((value): value is Record<string, unknown> => value !== null);
    const deltas = array(effects?.tokenDeltas)
      .map(record)
      .filter((value): value is Record<string, unknown> => value !== null);
    const sourceDelta = deltas.find((delta) => (integer(delta.rawDelta) ?? 0n) < 0n);
    const sourceBefore = before.find((amount) => amount.account === sourceDelta?.account);
    const amount = integer(sourceBefore?.amountRaw);
    const delta = integer(sourceDelta?.rawDelta);
    return amount === null || delta === null ? null : amount + delta;
  };
  const bySlot = (left: Record<string, unknown>, right: Record<string, unknown>) =>
    Number((operationSlot(left) ?? 0n) - (operationSlot(right) ?? 0n));
  const deposits = reconciledOperations.filter((operation) => operation.action === "deposit_claim_asset").sort(bySlot);
  const borrows = reconciledOperations.filter((operation) => operation.action === "borrow_debt").sort(bySlot);
  const requests = reconciledOperations.filter((operation) => operation.action === "request_withdrawal").sort(bySlot);
  const cancels = reconciledOperations.filter((operation) => operation.action === "cancel_withdrawal").sort(bySlot);
  const claims = reconciledOperations.filter((operation) => operation.action === "claim").sort(bySlot);
  const partialClaim = claims.find((operation) => (claimSourcePost(operation) ?? 0n) > 0n);
  const fullClaim = [...claims].reverse().find((operation) => claimSourcePost(operation) === 0n);
  const cancel = cancels.find((candidate) => requests.some((request) =>
    intent(request)?.requestId === intent(candidate)?.requestId &&
    (operationSlot(request) ?? 0n) <= (operationSlot(candidate) ?? 0n)
  ));
  const partialClaimSlot = partialClaim ? operationSlot(partialClaim) : null;
  const fullClaimSlot = fullClaim ? operationSlot(fullClaim) : null;
  const cancelSlot = cancel ? operationSlot(cancel) : null;
  const partialRequest = partialClaimSlot === null ? undefined : [...requests].reverse().find((request) =>
    intent(request)?.requestId !== intent(cancel ?? {})?.requestId &&
    (operationSlot(request) ?? 0n) <= partialClaimSlot
  );
  const redeploy = partialClaimSlot === null ? undefined : reconciledOperations.find((operation) =>
    operation.action === "swap_claim_to_collateral" &&
    (operationSlot(operation) ?? 0n) > partialClaimSlot
  );
  const redeploySlot = redeploy ? operationSlot(redeploy) : null;
  const fullRequest = fullClaimSlot === null || redeploySlot === null ? undefined : requests.find((request) =>
    (operationSlot(request) ?? 0n) > redeploySlot &&
    (operationSlot(request) ?? 0n) <= fullClaimSlot
  );
  const depositPair = deposits.slice(0, -1).flatMap((deposit, index) => {
    const topUp = deposits[index + 1];
    const depositSlot = operationSlot(deposit);
    const topUpSlot = operationSlot(topUp ?? {});
    const borrow = depositSlot === null || topUpSlot === null || cancelSlot === null || topUpSlot > cancelSlot
      ? undefined
      : borrows.find((candidate) =>
        (operationSlot(candidate) ?? 0n) > depositSlot &&
        (operationSlot(candidate) ?? 0n) < topUpSlot
      );
    return borrow ? [{ deposit, topUp, borrow }] : [];
  }).at(-1);
  const firstDepositSlot = operationSlot(depositPair?.deposit ?? {});
  const topUpSlot = operationSlot(depositPair?.topUp ?? {});
  const initialBorrow = depositPair?.borrow;
  const lifecycleSlots = [
    firstDepositSlot,
    initialBorrow ? operationSlot(initialBorrow) : null,
    topUpSlot,
    cancelSlot,
    partialRequest ? operationSlot(partialRequest) : null,
    partialClaimSlot,
    redeploySlot,
    fullRequest ? operationSlot(fullRequest) : null,
    fullClaimSlot,
  ];
  const lifecycleOrdered = lifecycleSlots.every((slot) => slot !== null) &&
    lifecycleSlots.every((slot, index) =>
      index === 0 || (slot ?? 0n) >= (lifecycleSlots[index - 1] ?? 0n)
    );
  if (!lifecycleOrdered) {
    blocked("fresh_partial_and_repeated_lifecycle_missing", {
      firstDepositSlot: firstDepositSlot?.toString(),
      initialBorrowSlot: initialBorrow ? operationSlot(initialBorrow)?.toString() : null,
      topUpSlot: topUpSlot?.toString(),
      cancelSlot: cancelSlot?.toString(),
      partialRequestSlot: partialRequest ? operationSlot(partialRequest)?.toString() : null,
      partialClaimSlot: partialClaimSlot?.toString(),
      partialClaimSourcePost: partialClaim ? claimSourcePost(partialClaim)?.toString() : null,
      redeploySlot: redeploySlot?.toString(),
      fullRequestSlot: fullRequest ? operationSlot(fullRequest)?.toString() : null,
      fullClaimSlot: fullClaimSlot?.toString(),
      resume: "complete the ordered initial-open, top-up, cancel, partial-claim/redeploy, and full-claim lifecycle",
    });
  }

  const snapshots = await sql`
    SELECT * FROM loyal_yield.multiply_position_snapshots
    WHERE route_key = ${String(route?.route_key ?? "")}
    ORDER BY observed_slot, id
  `;
  const open = snapshots.find((snapshot) => (integer(snapshot.collateral_raw) ?? 0n) > 0n && (integer(snapshot.debt_raw) ?? 0n) > 0n);
  const finalSnapshot = snapshots.at(-1);
  if (
    !open || !finalSnapshot ||
    integer(finalSnapshot.claim_raw) !== 0n ||
    integer(finalSnapshot.collateral_raw) !== 0n ||
    integer(finalSnapshot.debt_raw) !== 0n ||
    integer(finalSnapshot.equity_usd_micros) !== 0n ||
    !finalSnapshot.valuation_source || !finalSnapshot.valuation_slot
  ) {
    blocked("fresh_position_history_or_final_zero_missing", {
      snapshotCount: snapshots.length,
      hasRealOpen: Boolean(open),
      final: finalSnapshot ? {
        claimRaw: finalSnapshot.claim_raw,
        collateralRaw: finalSnapshot.collateral_raw,
        debtRaw: finalSnapshot.debt_raw,
        equityUsdMicros: finalSnapshot.equity_usd_micros,
      } : null,
      resume: "observe one nonzero real position and a reconciled zero final position",
    });
  }

  const policyAccounts = await connection.getMultipleAccountsInfo(
    accounts.map((account) => new PublicKey(account)),
    { commitment: "confirmed" },
  );
  if (policyAccounts.some(Boolean)) fail("removed_earn_max_policy_still_exists_on_chain", { accounts });
  const signatures = [
    String(policy?.observed_signature ?? ""),
    String(deposit?.transactionSignature ?? ""),
    String(withdrawal?.claimSignature ?? ""),
    ...reconciledOperations.map((operation) => String(operation.transaction_signature ?? "")),
  ];
  const chain = await confirmedSignatures(connection, signatures);
  const latestOperationUpdatedAt = Math.max(
    ...operations.map((operation) => timestamp(operation.updated_at) ?? 0),
  );
  return {
    settings: VERIFY_SETTINGS,
    routeKey: route?.route_key,
    vault: route?.vault,
    policySeedBase: base.toString(),
    policyAccounts: accounts,
    operationCount: reconciledOperations.length,
    inertExpiredOperationCount: expiredOperations.length,
    actionCounts: Object.fromEntries(requiredActions.map((action) => [action, actionCount(action)])),
    intentLocationCount: chainLocations.length,
    partialClaimSourcePost: partialClaim ? claimSourcePost(partialClaim)?.toString() : null,
    latestOperationUpdatedAt: new Date(latestOperationUpdatedAt).toISOString(),
    snapshotCount: snapshots.length,
    openSnapshotSlot: open.observed_slot,
    finalSnapshotSlot: finalSnapshot.observed_slot,
    unwindMilliseconds: unwindAt - requestedAt,
    chain,
  };
}

function checkReplayEvidence(deployedWorkers: Json, lifecycle: Json): Json {
  const projector = array(deployedWorkers.services)
    .map(record)
    .find((service) => service?.service === POLICY_MONITOR_SERVICE);
  const replayStartedAt = timestamp(projector?.createdAt);
  const latestOperationAt = timestamp(lifecycle.latestOperationUpdatedAt);
  if (
    replayStartedAt === null ||
    latestOperationAt === null ||
    replayStartedAt <= latestOperationAt ||
    projector?.status !== "live"
  ) {
    blocked("post_lifecycle_projector_replay_missing", {
      service: POLICY_MONITOR_SERVICE,
      replayStartedAt: projector?.createdAt,
      latestOperationUpdatedAt: lifecycle.latestOperationUpdatedAt,
      status: projector?.status,
      resume: "restart the exact pinned LaserStream worker after the lifecycle and rerun the read-only verifier",
    });
  }
  return {
    service: POLICY_MONITOR_SERVICE,
    deployId: projector?.deployId,
    replayStartedAt: projector?.createdAt,
    latestOperationUpdatedAt: lifecycle.latestOperationUpdatedAt,
    operationCountAfterReplay: lifecycle.operationCount,
    intentLocationCountAfterReplay: lifecycle.intentLocationCount,
  };
}

function checkReleaseSource(): Json {
  const worker = file(ROOT, WORKER);
  const render = file(ROOT, RENDER_BLUEPRINT);
  for (const forbidden of ["SOLANA_TESTING_PK", "guard", "flashBorrow", "flash_borrow"] as const) {
    rejectText(worker, forbidden, "multiply_runtime_authority_or_graph_drift", WORKER);
  }
  requireText(worker, "CommitmentConfig::confirmed()", "multiply_runtime_not_confirmed", WORKER);
  for (const required of ["loyal-multiply-route-worker", "multiply-route-worker run", "POLICY_KEYPAIR"] as const) {
    requireText(render, required, "earn_max_release_topology_missing", RENDER_BLUEPRINT);
  }
  const monitorImage = render.match(/name: loyal-balance-sweep-ata-monitor[\s\S]*?laserstream-workers:sha-([0-9a-f]{40})/)?.[1];
  const workerImage = render.match(/name: loyal-multiply-route-worker[\s\S]*?light-workers:sha-([0-9a-f]{40})/)?.[1];
  if (!monitorImage || !workerImage) {
    fail("earn_max_worker_image_pin_missing", { monitorImage, workerImage });
  }
  const imageBuild = file(ROOT, "scripts/build-rust-image-binaries.sh");
  requireText(imageBuild, "laserstream-workers)\n    packages=(balance-sweep-ata-monitor kamino-reserve-monitor", "policy_projection_wrong_image_family", "scripts/build-rust-image-binaries.sh");
  requireText(imageBuild, "multiply-route-worker", "multiply_worker_missing_from_image", "scripts/build-rust-image-binaries.sh");
  return {
    workerSha256: sha256(worker),
    renderSha256: sha256(render),
    monitorImageRevision: monitorImage,
    workerImageRevision: workerImage,
  };
}

const contract = checkContractIdentity();
const laserStream = checkLaserStreamSource();
const topology = checkPerUserTopology();
const threePolicy = checkThreePolicySourceContract();
const schema = checkMinimalSchemaSource();
const app = checkAppSource();
const engine = checkWorkerAndStoreSource();
const release = checkReleaseSource();
const targeted = await targetedChecks();
const mainnetCatalog = await checkMainnetPoolCatalog();
const squadsPolicyDialect = await checkSquadsLegacyPolicyCompatibility();
const live = await checkLivePrerequisites();
const deployedWorkers = await checkDeployedWorkers(
  String(release.monitorImageRevision),
  String(release.workerImageRevision),
);
const lifecycle = await checkFreshLifecycle();
const replay = checkReplayEvidence(deployedWorkers, lifecycle);

emit(PASS, "earn_max_three_policy_production_ready", {
  contract,
  topology,
  threePolicy,
  schema,
  laserStream,
  app,
  engine,
  release,
  targeted,
  mainnetCatalog,
  squadsPolicyDialect,
  live,
  deployedWorkers,
  lifecycle,
  replay,
}, 0);
