import { existsSync, readFileSync, writeFileSync } from "node:fs";
import { resolve } from "node:path";
import { fileURLToPath } from "node:url";

import { Connection } from "@solana/web3.js";

import { diagnoseRejectedJupiterEdge } from "../policies/rwa-multiply-jupiter-headers.js";

const rpcUrl = process.env.SOLANA_RPC_URL?.trim();
if (!rpcUrl) throw new Error("SOLANA_RPC_URL is required");
const edge = process.env.JUPITER_DIAGNOSTIC_EDGE?.trim();
if (!edge) throw new Error("JUPITER_DIAGNOSTIC_EDGE is required");
const repositoryRoot = resolve(fileURLToPath(new URL("../../../..", import.meta.url)));
const headers = resolve(repositoryRoot, "docs/evidence/backyard-rwa-go/policy-jupiter-headers-v1.json");
const output = resolve(repositoryRoot, "docs/evidence/backyard-rwa-go/policy-jupiter-rejected-shapes-v1.json");
const headerEvidence = JSON.parse(readFileSync(headers, "utf8")) as { rows?: unknown[] };
const rejected = headerEvidence.rows?.find((value) => value !== null && typeof value === "object"
  && (value as { key?: unknown; pass?: unknown; code?: unknown }).key === edge
  && (value as { pass?: unknown }).pass === false
  && (value as { code?: unknown }).code === "JUPITER_EDGE_REJECTED");
if (!rejected) throw new Error(`${edge} is not a current Jupiter semantic rejection`);
const prior = existsSync(output) ? JSON.parse(readFileSync(output, "utf8")) as { rows?: unknown[] } : { rows: [] };
const rows = Array.isArray(prior.rows) ? prior.rows : [];
if (rows.some((value) => value !== null && typeof value === "object" && (value as { edge?: unknown }).edge === edge)) {
  throw new Error(`${edge} already has a persisted one-shot rejected-shape diagnostic; refusing a repeat request`);
}
const diagnostic = await diagnoseRejectedJupiterEdge(new Connection(rpcUrl, "confirmed"), edge);
const evidence = {
  schema: "loyal-backyard-rwa-jupiter-rejected-shapes/v1",
  generatedAt: new Date().toISOString(),
  broadcast: false,
  commitment: "confirmed",
  rows: [...rows, diagnostic],
};
writeFileSync(output, `${JSON.stringify(evidence, null, 2)}\n`, { flag: "w" });
console.log(JSON.stringify({ output, edge, rowCount: evidence.rows.length }));
