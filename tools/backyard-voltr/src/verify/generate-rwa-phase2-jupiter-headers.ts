import { existsSync, readFileSync, writeFileSync } from "node:fs";
import { resolve } from "node:path";
import { fileURLToPath } from "node:url";

import { Connection } from "@solana/web3.js";

import { resolveCurrentJupiterHeaders } from "../policies/rwa-multiply-jupiter-headers.js";

const rpcUrl = process.env.SOLANA_RPC_URL?.trim();
if (!rpcUrl) throw new Error("SOLANA_RPC_URL is required");
const repositoryRoot = resolve(fileURLToPath(new URL("../../../..", import.meta.url)));
const output = resolve(repositoryRoot, "docs/evidence/backyard-rwa-go/policy-jupiter-headers-v1.json");
function optionalBoundedInteger(name: string, maximum: number): number | undefined {
  const raw = process.env[name]?.trim();
  if (!raw) return undefined;
  const value = Number(raw);
  if (!Number.isSafeInteger(value) || value < 0 || value > maximum) {
    throw new Error(`${name} must be a non-negative integer no greater than ${maximum}`);
  }
  return value;
}
const cachedEvidenceRaw = existsSync(output) ? JSON.parse(readFileSync(output, "utf8")) : undefined;
const maxNetworkEdges = optionalBoundedInteger("JUPITER_RESUME_MAX_EDGES", 52);
const minRequestIntervalMs = optionalBoundedInteger("JUPITER_MIN_REQUEST_INTERVAL_MS", 60_000);
const targetEdge = process.env.JUPITER_TARGET_EDGE?.trim();
if (targetEdge !== undefined && targetEdge.length === 0) throw new Error("JUPITER_TARGET_EDGE must not be empty");
const forceTargetRefresh = process.env.JUPITER_FORCE_TARGET_REFRESH === "1";
const cachedEvidence = forceTargetRefresh && targetEdge && cachedEvidenceRaw
  ? { ...cachedEvidenceRaw, rows: Array.isArray(cachedEvidenceRaw.rows)
    ? cachedEvidenceRaw.rows.filter((row: { key?: unknown }) => row.key !== targetEdge)
    : cachedEvidenceRaw.rows }
  : cachedEvidenceRaw;
const evidence = await resolveCurrentJupiterHeaders(new Connection(rpcUrl, "confirmed"), {
  cachedEvidence,
  ...(maxNetworkEdges === undefined ? {} : { maxNetworkEdges }),
  ...(minRequestIntervalMs === undefined ? {} : { minRequestIntervalMs }),
  ...(targetEdge === undefined ? {} : { targetEdgeKeys: [targetEdge] }),
});
writeFileSync(output, `${JSON.stringify(evidence, null, 2)}\n`, { flag: "w" });
const failed = evidence.failedEdges;
console.log(JSON.stringify({ output, verdict: evidence.verdict, passCount: evidence.passCount,
  cachedPassCount: evidence.requestBudget.cachedPassCount,
  attemptedEdges: evidence.requestBudget.attemptedEdges, targetEdge: targetEdge ?? null,
  failedCount: failed.length, failed }));
if (evidence.verdict !== "PASS_HEADERS_RESOLVED") process.exitCode = 1;
