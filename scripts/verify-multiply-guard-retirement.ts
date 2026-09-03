import { spawnSync } from "node:child_process";
import { createHash } from "node:crypto";
import { existsSync, readFileSync } from "node:fs";
import { resolve } from "node:path";
import { Connection, PublicKey } from "@solana/web3.js";

type Verdict = "PASS" | "FAIL" | "BLOCKED";

const SOLE_COMMAND = "op run --env-file=.env.1password -- bun run verify:multiply-guard-retirement";
const REPOSITORY_ROOT = resolve(import.meta.dir, "..");
const CONTRACT_PATH = resolve(REPOSITORY_ROOT, "docs/plans/multiply-guard-retirement-verifier.md");
const BASELINE_PATH = resolve(REPOSITORY_ROOT, "docs/evidence/multiply-guard-retirement/blocked-loader-v4-baseline-v1.json");
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
      schema: "loyal.multiply-guard-retirement-verifier.v2",
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
  const guardOwnedAccounts = await connection.getProgramAccounts(
    new PublicKey(ADDRESS_GROUPS.guardProgram[0]),
    { commitment: "confirmed" },
  );
  const guardOwnedAccountAddresses = guardOwnedAccounts
    .map(({ pubkey }) => pubkey.toBase58())
    .sort();
  const exactGuardOwnedSet = guardOwnedAccountAddresses.length === ADDRESS_GROUPS.residualAccounts.length
    && [...ADDRESS_GROUPS.residualAccounts].sort().every((address, index) => guardOwnedAccountAddresses[index] === address);
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
    && program.dataSha256 === "5cbbdaf8e06df9ad669fc80fa2da8f31e1aa61ad0df620a94725cb042b2b5c85"
    && exactGuardOwnedSet
    && residualsExact
    && ADDRESS_GROUPS.programdata.every((address) => !liveByAddress.has(address))
    && ADDRESS_GROUPS.retiredHookPolicies.every((address) => !liveByAddress.has(address));
  const g01Verdict: Verdict = mainnet && exactFrozenPrestate ? "PASS" : "FAIL";

  const checks = [];
  checks.push(check(
    "G01",
    g01Verdict,
    "Mainnet readback exactly matches the user-accepted inert guard residue.",
    { genesisHash, contextSlot: accountRead.context.slot, loaderV4Feature: LOADER_V4_FEATURE, loaderV4Active, exactFrozenPrestate, exactGuardOwnedSet, guardOwnedAccountAddresses, liveAccounts },
    !mainnet
      ? "Use the mainnet-beta RPC endpoint."
      : "Restore or explicitly reclassify the first mismatched pinned residue account; do not infer safety from partial absence or drift.",
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
  const squadsSourcePath = resolve(REPOSITORY_ROOT, "crates/loyal-actions/src/squads.rs");
  const squadsSource = existsSync(squadsSourcePath) ? readFileSync(squadsSourcePath, "utf8") : "";
  const genericHookAbiPreserved = squadsSource.includes("pre_hook: Option<SquadsHook>")
    && squadsSource.includes("post_hook: Option<SquadsHook>")
    && squadsSource.includes("pre_hook: None")
    && squadsSource.includes("post_hook: None");
  checks.push(check(
    "G02",
    forbiddenPaths.length === 0 && forbiddenDeployerFragments.length === 0 && navAdaptorSourcePresent && genericHookAbiPreserved ? "PASS" : "FAIL",
    "No guard/registry/recovery runtime remains in the repository while the NAV adaptor path remains intact.",
    { forbiddenPaths, forbiddenDeployerFragments, navAdaptorSourcePresent, genericHookAbiPreserved },
    "Remove only the obsolete guard/recovery sources and guard-specific deploy target; preserve the NAV-adaptor deploy path and generic Squads ABI.",
  ));

  const phaseOne = runPhaseOneVerifier();
  checks.push(check(
    "G03",
    phaseOne.exitCode === 0 && phaseOne.verdict === "PASS" ? "PASS" : phaseOne.exitCode === 2 ? "BLOCKED" : "FAIL",
    "The current v12 verifier revalidates the live hookless Phase 1 path and installed Phase 2 policy catalog.",
    phaseOne,
    "Resolve the first condition reported by the v12 verifier without replaying identity-valid historical evidence.",
  ));

  const baseline = parseJson(BASELINE_PATH);
  const contract = existsSync(CONTRACT_PATH) ? readFileSync(CONTRACT_PATH, "utf8") : "";
  const closeoutExact = contract.includes("Status: approved close-out contract v2")
    && contract.includes("accepted inert residue")
    && contract.includes("authorizes no transaction")
    && baseline?.schema === "loyal.multiply-guard-retirement-blocked-baseline.v1"
    && baseline?.broadcast === false
    && baseline?.expectedGrossRefundLamports === 136_743_120;
  checks.push(check(
    "G04",
    closeoutExact ? "PASS" : "FAIL",
    "The user-approved v2 contract and sanitized read-only residue baseline are exact.",
    { contractPath: CONTRACT_PATH, baselinePath: BASELINE_PATH, contractV2: contract.includes("Status: approved close-out contract v2"), baselineSchema: baseline?.schema ?? null, broadcast: false },
    "Restore the approved v2 contract or sanitized baseline without adding a mainnet execution path.",
  ));

  const firstFailure = checks.find((entry) => entry.verdict === "FAIL") ?? null;
  const blocker = firstFailure === null ? checks.find((entry) => entry.verdict === "BLOCKED") ?? null : null;
  const verdict: Verdict = firstFailure ? "FAIL" : blocker ? "BLOCKED" : "PASS";
  console.log(JSON.stringify({
    schema: "loyal.multiply-guard-retirement-verifier.v2",
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
    schema: "loyal.multiply-guard-retirement-verifier.v2",
    verdict: "FAIL",
    broadcast: false,
    commitment: "confirmed",
    firstFailure: { id: "VERIFIER_INTERNAL_ERROR", message: error instanceof Error ? error.message : String(error) },
    blocker: null,
    resumeCommand: SOLE_COMMAND,
  }, null, 2));
  process.exitCode = 1;
}
