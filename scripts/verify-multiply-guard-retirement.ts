import { spawnSync } from "node:child_process";
import { createHash } from "node:crypto";
import { existsSync, readFileSync } from "node:fs";
import { resolve } from "node:path";
import { Connection, PublicKey } from "@solana/web3.js";

type Verdict = "PASS" | "FAIL" | "BLOCKED";

const SOLE_COMMAND = "op run --env-file=.env.1password -- bun run verify:multiply-guard-retirement";
const REPOSITORY_ROOT = resolve(import.meta.dir, "..");
const EVIDENCE_PATH = resolve(REPOSITORY_ROOT, "docs/evidence/multiply-guard-retirement/finalized-recovery-v1.json");
const LOADER_V4_FEATURE = "2aQJYqER2aKyb3cZw22v4SL2xMX7vwXBRWfvS4pTrtED";
const EXPECTED_RESIDUALS = new Map([
  ["3GZpBrXGjCKELRwoK5VERYZeyKPJn7WiJAoUvkTFibU4", { lamports: 22_230_240, dataLength: 3_066, dataSha256: "3a3dceee31e52abefbf591d220ac5907bd9ad8829a0ce1b9de6c07e1bd52a44f" }],
  ["J1VEo6YTmMNfRRrZGjpkU8ZF8z2t3x5xHQySqYa2kMN2", { lamports: 11_296_080, dataLength: 1_495, dataSha256: "a11b4bbbc1b8d83b87995c84875d5699af9fde3499aa617e1acb992d30cf6ef4" }],
  ["4bSQzxkXKmezQTUyvNkMMgthsF4Wdc1J7eZr64QbxjAp", { lamports: 90_779_280, dataLength: 12_915, dataSha256: "69364145d54baf5997fd6b0e6444e9cc2b866387acd0a390276b7fdc69f9120c" }],
  ["GdMMMAQCGihyN6tTbJiXjt8zZVmbmhNeS4yCRSXhJsnT", { lamports: 11_296_080, dataLength: 1_495, dataSha256: "a11b4bbbc1b8d83b87995c84875d5699af9fde3499aa617e1acb992d30cf6ef4" }],
]);
const ADDRESS_GROUPS = {
  guardProgram: ["8moAa3vXstMPop9FtEnhTDRmcyo9HPn1CsywGMZ9K9n8"],
  programdata: ["Hke6Nd6i5PkAEpGZGbjLf7sEc1TM48NGGjTjRQhnqX1G"],
  residualAccounts: [
    "3GZpBrXGjCKELRwoK5VERYZeyKPJn7WiJAoUvkTFibU4",
    "J1VEo6YTmMNfRRrZGjpkU8ZF8z2t3x5xHQySqYa2kMN2",
    "4bSQzxkXKmezQTUyvNkMMgthsF4Wdc1J7eZr64QbxjAp",
    "GdMMMAQCGihyN6tTbJiXjt8zZVmbmhNeS4yCRSXhJsnT",
  ],
  retiredHookPolicies: [
    "633UHSciFmPCr2dysjEEHq1pG1kx1E3Kk6W9d9JQSL5g",
    "GUGvmxsqAvneNJoxx1FJJPpou9hckGhkiwSQse7ijqzx",
  ],
} as const;

function check(id: string, verdict: Verdict, condition: string, evidence: unknown, resumeCondition: string | null = null) {
  return { id, verdict, condition, evidence, resumeCondition };
}

function parseJson(path: string): Record<string, unknown> | null {
  if (!existsSync(path)) return null;
  try {
    return JSON.parse(readFileSync(path, "utf8")) as Record<string, unknown>;
  } catch {
    return null;
  }
}

function runPhaseOneVerifier() {
  const result = spawnSync("bun", ["run", "verify:rwa-multiply-custom-lifecycle"], {
    cwd: resolve(REPOSITORY_ROOT, "tools/backyard-voltr"),
    env: process.env,
    encoding: "utf8",
    maxBuffer: 32 * 1024 * 1024,
    timeout: 120_000,
  });
  let output: Record<string, unknown> | null = null;
  try {
    output = JSON.parse(result.stdout || "null") as Record<string, unknown>;
  } catch {
    output = null;
  }
  return {
    exitCode: result.status,
    verdict: typeof output?.verdict === "string" ? output.verdict : null,
    sourceCommit: output?.sourceCommit ?? null,
    phase1: output?.phase1 ?? null,
    phase2: output?.phase2 ?? null,
    error: result.error?.message ?? (output === null ? "v12 verifier returned non-JSON output" : null),
  };
}

async function main() {
  const rpcUrl = process.env.SOLANA_RPC_URL;
  if (!rpcUrl) {
    console.log(JSON.stringify({
      schema: "loyal.multiply-guard-retirement-verifier.v1",
      verdict: "BLOCKED",
      broadcast: false,
      commitment: "confirmed",
      firstFailure: null,
      blocker: { id: "ENV", resumeCondition: "Inject SOLANA_RPC_URL through the existing 1Password environment." },
      resumeCommand: SOLE_COMMAND,
    }, null, 2));
    process.exitCode = 2;
    return;
  }

  const connection = new Connection(rpcUrl, "confirmed");
  const genesisHash = await connection.getGenesisHash();
  const mainnet = genesisHash === "5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d";
  const addresses = Object.values(ADDRESS_GROUPS).flat();
  const accountRead = await connection.getMultipleAccountsInfoAndContext(
    [...addresses, LOADER_V4_FEATURE].map((value) => new PublicKey(value)),
    { commitment: "confirmed" },
  );
  const liveAccounts = addresses.flatMap((address, index) => {
    const account = accountRead.value[index];
    return account ? [{
      address,
      owner: account.owner.toBase58(),
      lamports: account.lamports,
      dataLength: account.data.length,
      dataSha256: createHash("sha256").update(account.data).digest("hex"),
    }] : [];
  });
  const loaderV4Active = accountRead.value[addresses.length] !== null;
  const liveByAddress = new Map(liveAccounts.map((entry) => [entry.address, entry]));
  const program = liveByAddress.get(ADDRESS_GROUPS.guardProgram[0]);
  const residualsExact = ADDRESS_GROUPS.residualAccounts.every((address) => {
    const actual = liveByAddress.get(address);
    const expected = EXPECTED_RESIDUALS.get(address);
    if (!actual || !expected) return false;
    return actual.owner === ADDRESS_GROUPS.guardProgram[0]
      && actual.lamports === expected?.lamports
      && actual.dataLength === expected.dataLength
      && actual.dataSha256 === expected.dataSha256;
  });
  const exactFrozenPrestate = program?.owner === "BPFLoaderUpgradeab1e11111111111111111111111"
    && program.lamports === 1_141_440
    && program.dataLength === 36
    && residualsExact
    && ADDRESS_GROUPS.programdata.every((address) => !liveByAddress.has(address))
    && ADDRESS_GROUPS.retiredHookPolicies.every((address) => !liveByAddress.has(address));
  const g01Verdict: Verdict = mainnet && liveAccounts.length === 0
    ? "PASS"
    : mainnet && exactFrozenPrestate && !loaderV4Active
      ? "BLOCKED"
      : "FAIL";

  const checks = [];
  checks.push(check(
    "G01",
    g01Verdict,
    "The exact retired onchain surface is absent on mainnet-beta.",
    { genesisHash, contextSlot: accountRead.context.slot, loaderV4Feature: LOADER_V4_FEATURE, loaderV4Active, exactFrozenPrestate, liveAccounts },
    !mainnet
      ? "Use the mainnet-beta RPC endpoint."
      : g01Verdict === "BLOCKED"
        ? "Resume the exact signed migration and recovery ladder only after loader-v4 activates on mainnet-beta."
        : "Complete and reconcile the exact allowlisted recovery stages; do not close any other account.",
  ));

  const recoveryEvidence = parseJson(EVIDENCE_PATH);
  const evidencePass = recoveryEvidence?.schema === "loyal.multiply-guard-retirement-evidence.v1"
    && recoveryEvidence?.verdict === "PASS"
    && recoveryEvidence?.grossRefundLamports === 136_743_120
    && recoveryEvidence?.refundDestination === "BAqgbERmvUViqDSx961xpRBHGt68SpACiWL4t9696qZZ";
  checks.push(check(
    "G02",
    evidencePass ? "PASS" : g01Verdict === "BLOCKED" ? "BLOCKED" : "FAIL",
    "Finalized recovery evidence reconciles the exact allowlist, refund, fees, and authority balance equation.",
    { path: EVIDENCE_PATH, present: recoveryEvidence !== null, schema: recoveryEvidence?.schema ?? null, verdict: recoveryEvidence?.verdict ?? null },
    "Generate sanitized finalized evidence from the durable recovery journal after every exact target is absent.",
  ));

  const forbiddenPaths = [
    "crates/loyal-multiply-guard-deployer",
    "crates/loyal-multiply-guard-program",
    "crates/loyal-multiply-guard-recovery",
    "scripts/recover-retired-multiply-guard-state.ts",
    "scripts/retire-multiply-guard.ts",
  ].filter((path) => existsSync(resolve(REPOSITORY_ROOT, path)));
  const deployerPath = resolve(REPOSITORY_ROOT, "crates/loyal-voltr-rwa-nav-adaptor-deployer/src/main.rs");
  const deployerSource = existsSync(deployerPath) ? readFileSync(deployerPath, "utf8") : "";
  const forbiddenDeployerFragments = [
    "GUARD_SPEC",
    "Target::Guard",
    "loyal_multiply_guard_program.so",
    "8moAa3vXstMPop9FtEnhTDRmcyo9HPn1CsywGMZ9K9n8",
  ].filter((fragment) => deployerSource.includes(fragment));
  const navAdaptorSourcePresent = deployerSource.includes("ADAPTOR_SPEC")
    && deployerSource.includes("FSj27QT2PtP7365pQRtgSAwSwk5h2m2ATCBoXQjwTSxW");
  checks.push(check(
    "G03",
    forbiddenPaths.length === 0 && forbiddenDeployerFragments.length === 0 && navAdaptorSourcePresent ? "PASS" : "FAIL",
    "No guard/registry/recovery runtime remains in the repository while the NAV adaptor path remains intact.",
    { forbiddenPaths, forbiddenDeployerFragments, navAdaptorSourcePresent },
    "Remove only the obsolete guard/recovery sources and guard-specific deploy target; preserve the NAV-adaptor deploy path and generic Squads ABI.",
  ));

  const phaseOne = runPhaseOneVerifier();
  checks.push(check(
    "G04",
    phaseOne.exitCode === 0 && phaseOne.verdict === "PASS" ? "PASS" : phaseOne.exitCode === 2 ? "BLOCKED" : "FAIL",
    "The current v12 verifier revalidates the live hookless Phase 1 path and installed Phase 2 policy catalog.",
    phaseOne,
    "Resolve the first condition reported by the v12 verifier without replaying identity-valid historical evidence.",
  ));

  const firstFailure = checks.find((entry) => entry.verdict === "FAIL") ?? null;
  const blocker = firstFailure === null ? checks.find((entry) => entry.verdict === "BLOCKED") ?? null : null;
  const verdict: Verdict = firstFailure ? "FAIL" : blocker ? "BLOCKED" : "PASS";
  console.log(JSON.stringify({
    schema: "loyal.multiply-guard-retirement-verifier.v1",
    verdict,
    broadcast: false,
    commitment: "confirmed",
    contractPath: "docs/plans/multiply-guard-retirement-verifier.md",
    checks,
    firstFailure,
    blocker,
    resumeCommand: SOLE_COMMAND,
  }, null, 2));
  process.exitCode = verdict === "PASS" ? 0 : verdict === "FAIL" ? 1 : 2;
}

try {
  await main();
} catch (error) {
  console.log(JSON.stringify({
    schema: "loyal.multiply-guard-retirement-verifier.v1",
    verdict: "FAIL",
    broadcast: false,
    commitment: "confirmed",
    firstFailure: { id: "VERIFIER_INTERNAL_ERROR", message: error instanceof Error ? error.message : String(error) },
    blocker: null,
    resumeCommand: SOLE_COMMAND,
  }, null, 2));
  process.exitCode = 1;
}
