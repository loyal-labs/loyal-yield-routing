#!/usr/bin/env bun
import { readFile } from "node:fs/promises";
import {
  causalEvidenceFailures,
  type CausalEvidence,
} from "./fleet-local-load-lab/causal-evidence";

const positive: CausalEvidence = {
  raw: {
    rpcTotalRequests: 12,
    rpcSyntheticDriverRequests: 10,
    outboxTotalRowsAdded: 7,
    localUserOutboxRowsAdded: 7,
  },
  attribution: {
    realWorker: { rpcRequests: 2, completed: 0, outboxRows: 0 },
    syntheticSql: { outboxRows: 7 },
    syntheticRpc: { requests: 10 },
  },
  checks: {
    allStartedProcessesSurvived: true,
    realWorkerProgress: { passes: false },
  },
  verdicts: { fullChainE2e: "NOT_RUN" },
};

const negativeControls: Array<[string, CausalEvidence]> = [
  ["synthetic RPC relabelled as real", {
    ...structuredClone(positive),
    attribution: {
      ...structuredClone(positive.attribution),
      realWorker: { ...positive.attribution.realWorker, rpcRequests: 12 },
      syntheticRpc: { requests: 0 },
    },
  }],
  ["local_user_load relabelled as worker outbox", {
    ...structuredClone(positive),
    attribution: {
      ...structuredClone(positive.attribution),
      realWorker: { ...positive.attribution.realWorker, outboxRows: 7 },
      syntheticSql: { outboxRows: 0 },
    },
  }],
  ["full-chain PASS without execution evidence", {
    ...structuredClone(positive),
    verdicts: { fullChainE2e: "PASS" },
  }],
  ["liveness presented as progress", {
    ...structuredClone(positive),
    checks: {
      allStartedProcessesSurvived: true,
      realWorkerProgress: { passes: true },
    },
  }],
];

const fixtureIndex = process.argv.indexOf("--fixture");
if (fixtureIndex !== -1) {
  const path = process.argv[fixtureIndex + 1];
  if (!path) throw new Error("--fixture requires a path");
  const fixture = JSON.parse(await readFile(path, "utf8")) as CausalEvidence;
  const failures = causalEvidenceFailures(fixture);
  if (failures.length) {
    console.error(failures.join("\n"));
    process.exit(1);
  }
  console.log("PASS: causal evidence fixture");
  process.exit(0);
}

const positiveFailures = causalEvidenceFailures(positive);
if (positiveFailures.length) {
  throw new Error(`positive fixture failed: ${positiveFailures.join("; ")}`);
}
for (const [name, fixture] of negativeControls) {
  if (causalEvidenceFailures(fixture).length === 0) {
    throw new Error(`negative control unexpectedly passed: ${name}`);
  }
  console.log(`PASS negative control: ${name}`);
}
console.log("PASS: fleet local load lab causal evidence checker");
