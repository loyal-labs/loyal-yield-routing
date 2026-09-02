import { createHash } from "node:crypto";
import { readFileSync, writeFileSync } from "node:fs";
import { resolve } from "node:path";
import { fileURLToPath } from "node:url";

import { RWA_MULTIPLY_ROUTE } from "../domain/rwa-multiply-route-spec.js";

type Json = Record<string, unknown>;
const ROOT = resolve(fileURLToPath(new URL("../../../..", import.meta.url)));
const EVIDENCE = resolve(ROOT, "docs/evidence/backyard-rwa-go");
const OUT = resolve(EVIDENCE, "policy-signed-unsent-v1.json");
const COMPILED = resolve(EVIDENCE, "policy-compiled-v1.json");
const sha256 = (value: Uint8Array | string) => createHash("sha256").update(value).digest("hex");
function object(value: unknown, label: string): Json {
  if (value === null || typeof value !== "object" || Array.isArray(value)) throw new Error(`${label} is not an object`);
  return value as Json;
}
function source(file: string): { value: Json; bytes: Buffer } {
  const bytes = readFileSync(resolve(EVIDENCE, file));
  return { value: object(JSON.parse(bytes.toString("utf8")), file), bytes };
}
function positive(name: string, file: string, compiledSha: string): Json {
  const { value, bytes } = source(file);
  if (value.verdict !== "PASS" || value.broadcast !== false || value.signedUnsent !== true
    || value.compiledArtifactSha256 !== compiledSha || !Array.isArray(value.transactions) || value.transactions.length === 0) {
    throw new Error(`${file} is not current passing signed-unsent evidence`);
  }
  const wire = object(value.transactions.at(-1), `${file} final transaction`);
  const simulation = object(value.simulation, `${file} simulation`);
  return { name, source: `docs/evidence/backyard-rwa-go/${file}`, sourceSha256: sha256(bytes), broadcast: false,
    ...wire, simulation: { err: simulation.err ?? null, contextSlot: simulation.contextSlot },
    signatureAbsentOnChain: value.signatureAbsentOnChain, chainPreStateSha256: value.chainPreStateSha256,
    chainPostStateSha256: value.chainPostStateSha256, confirmedReadbackSlot: value.confirmedReadbackSlot,
    compiledArtifactSha256: value.compiledArtifactSha256 };
}

const compiledSha = sha256(readFileSync(COMPILED));
const markets = [
  ["OnRe/ONyc/USDC/deposit", "policy-helius-market-OnRe-ONyc-USDC-v15.json"],
  ["Prime/PRIME/USDC/deposit", "policy-helius-market-Prime-PRIME-USDC-v15.json"],
  ["Maple/syrupUSDC/USDC/deposit", "policy-helius-market-Maple-syrupUSDC-USDC-v15.json"],
  ["AUTO/AUTO/PYUSD/deposit", "policy-helius-market-AUTO-AUTO-PYUSD-v15.json"],
  ["Ethena/USDe/PYUSD/deposit", "policy-helius-market-Ethena-USDe-PYUSD-v15.json"],
] as const;
const swaps = [
  ["USDC->PRIME", "policy-helius-prefix-USDC--PRIME-v2.json"],
  ["PRIME->USDC", "policy-helius-prefix-PRIME--USDC-v9.json"],
  ["USDC->USDG", "policy-helius-prefix-USDC--USDG-v11.json"],
] as const;
const bridgeSource = source("policy-helius-bridge-lifecycle-v2.json");
if (bridgeSource.value.verdict !== "PASS" || bridgeSource.value.policyScope !== "phase1-external-existing"
  || !Array.isArray(bridgeSource.value.bundles) || bridgeSource.value.bundles.length !== 4) {
  throw new Error("bridge lifecycle evidence is not complete");
}
const bridge = bridgeSource.value.bundles.map((row) => ({ ...object(row, "bridge bundle"),
  source: "docs/evidence/backyard-rwa-go/policy-helius-bridge-lifecycle-v2.json", sourceSha256: sha256(bridgeSource.bytes) }));

const negativeSource = source("policy-helius-negative-bundles-v3.json");
if (negativeSource.value.verdict !== "PASS" || negativeSource.value.compiledArtifactSha256 !== compiledSha
  || !Array.isArray(negativeSource.value.cases) || negativeSource.value.cases.length !== 7) {
  throw new Error("negative mutation evidence is not current and complete");
}
const negativeMutations = negativeSource.value.cases.map((row, index) => {
  const item = object(row, `negative case ${index}`);
  if (!Array.isArray(item.transactions) || item.transactions.length === 0 || item.accepted !== false) throw new Error(`negative case ${index} lacks a rejected wire`);
  const wire = object(item.transactions.at(-1), `negative case ${index} final transaction`);
  return { mutation: item.name, broadcast: false, accepted: false, bundles: [{ name: `${String(item.name)}-exact-mutation`,
    broadcast: false, accepted: false, rejectionLayer: item.rejectionLayer, ...wire, simulation: item.simulation,
    signatureAbsentOnChain: item.signatureAbsentOnChain, chainPreStateSha256: item.chainPreStateSha256,
    chainPostStateSha256: item.chainPostStateSha256, confirmedReadbackSlot: item.confirmedReadbackSlot,
    compiledArtifactSha256: compiledSha, source: "docs/evidence/backyard-rwa-go/policy-helius-negative-bundles-v3.json",
    sourceSha256: sha256(negativeSource.bytes) }] };
});

const result = { schema: "loyal-backyard-rwa-policy-signed-unsent/v1", verdict: "PASS", broadcast: false,
  signedUnsent: true, cluster: "mainnet-beta", commitment: "confirmed", genesisHash: RWA_MULTIPLY_ROUTE.genesisHash,
  compiledArtifactSha256: compiledSha, positiveGroups: [
    { name: "three-lane-markets", broadcast: false, bundles: markets.slice(0, 3).map(([name, file]) => positive(name, file, compiledSha)) },
    { name: "singleton-markets", broadcast: false, bundles: markets.slice(3).map(([name, file]) => positive(name, file, compiledSha)) },
    { name: "swap-graph", broadcast: false, bundles: swaps.map(([name, file]) => positive(name, file, compiledSha)) },
    { name: "bridge-lifecycle", broadcast: false, bundles: bridge },
  ], negativeMutations };
writeFileSync(OUT, `${JSON.stringify(result, null, 2)}\n`, { flag: "w", mode: 0o600 });
console.log(JSON.stringify({ verdict: "PASS", output: OUT, compiledArtifactSha256: compiledSha,
  positiveGroups: result.positiveGroups.map((row) => row.name), negativeMutations: negativeMutations.map((row) => row.mutation) }));
