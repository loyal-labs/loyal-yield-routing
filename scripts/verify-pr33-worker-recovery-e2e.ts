#!/usr/bin/env bun
/**
 * Complete isolated E2E verifier for PR #33.
 *
 * It starts the real fleet planner binary twice: once through its production
 * 10,000-vault benchmark path and once through the exact cycle-recovery
 * classifier at 10,000 transient cycles. It then runs the autodeposit worker
 * verifier, which scans a 2,500-target fleet and starts 25 concurrent child
 * workers matching the production incident shape.
 *
 * All production endpoints and signer variables are removed from child
 * environments. The verifier uses only deterministic in-memory stores,
 * finalized-RPC stubs, and throwaway worker files.
 */

export {};

type CommandCheck = {
  name: string;
  command: string[];
  requiredOutput: string[];
};

const isolatedEnv = { ...process.env };
for (const name of [
  "NEON_DATABASE_URL",
  "TIMESCALEDB_URL",
  "SOLANA_RPC_URL",
  "POLICY_KEYPAIR",
  "YIELD_ROUTER_KEYPAIR",
  "SOLANA_TESTING_PK",
  "YIELD_ROUTE_FEE_PAYER_KEYPAIRS",
]) {
  delete isolatedEnv[name];
}

const checks: CommandCheck[] = [
  {
    name: "fleet planner recovery supervision",
    command: [
      "cargo",
      "run",
      "--quiet",
      "-p",
      "loyal-yield-orchestrator",
      "--bin",
      "fleet-opportunity-planner",
      "--",
      "--recovery-verification-probe",
    ],
    requiredOutput: [
      '"status":"pass"',
      '"simulatedCycleCount":10000',
      '"fatalInvariantCount":1',
    ],
  },
  {
    name: "fleet planner production-load benchmark",
    command: [
      "cargo",
      "run",
      "--quiet",
      "-p",
      "loyal-yield-orchestrator",
      "--bin",
      "fleet-opportunity-planner",
      "--",
      "--once",
      "--dry-run",
      "--benchmark",
      "--json",
      "--count",
      "10000",
      "--rounds",
      "7",
    ],
    requiredOutput: ['"status":"pass"', '"inputCount":10000'],
  },
  {
    name: "autodeposit closed-policy worker fleet",
    command: ["bun", "scripts/verify-autodeposit-closed-route-policy.ts"],
    requiredOutput: [
      "production-shaped load: 2500 targets, 25 concurrent closed-policy workers",
      "each closed target spawns exactly one worker",
      "PASSED",
    ],
  },
];

let failed = false;
for (const check of checks) {
  console.log(`\n==> ${check.name}`);
  const child = Bun.spawn(check.command, {
    cwd: process.cwd(),
    env: isolatedEnv,
    stdout: "pipe",
    stderr: "pipe",
  });
  const [stdout, stderr, exitCode] = await Promise.all([
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
    child.exited,
  ]);
  if (stdout.trim()) {
    console.log(stdout.trimEnd());
  }
  if (stderr.trim()) {
    console.error(stderr.trimEnd());
  }
  const missing = check.requiredOutput.filter(
    (value) => !stdout.includes(value)
  );
  if (exitCode !== 0 || missing.length > 0) {
    failed = true;
    console.error(
      `FAILED ${check.name}: exit=${exitCode} missing=${JSON.stringify(
        missing
      )}`
    );
  } else {
    console.log(`PASS ${check.name}`);
  }
}

if (failed) {
  process.exitCode = 1;
} else {
  console.log("\nPR #33 isolated worker E2E verification passed");
}
