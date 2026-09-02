import { readFileSync, writeFileSync } from "node:fs";
import { resolve } from "node:path";
import { fileURLToPath } from "node:url";

import { Connection } from "@solana/web3.js";

import { resolveRepresentativeJupiterFamilies } from "../policies/rwa-multiply-jupiter-headers.js";

const repositoryRoot = resolve(fileURLToPath(new URL("../../../..", import.meta.url)));
const resolutionPath = resolve(repositoryRoot, "docs/evidence/backyard-rwa-go/policy-resolution-v1.json");
const output = resolve(repositoryRoot, "docs/evidence/backyard-rwa-go/policy-family-probes-v1.json");
const resolution = JSON.parse(readFileSync(resolutionPath, "utf8")) as {
  schema: string;
  commitment: string;
  contextSlot: number;
  policySeedBefore: string;
  lanes: readonly Readonly<{ key: string; exact: boolean; resolved: unknown }>[];
};
const rpcUrl = process.env.SOLANA_RPC_URL?.trim();
if (!rpcUrl) throw new Error("SOLANA_RPC_URL is required");
if (resolution.schema !== "loyal-backyard-rwa-policy-resolution/v1"
  || resolution.commitment !== "confirmed" || resolution.policySeedBefore !== "66") {
  throw new Error("confirmed policy resolution artifact is missing or drifted");
}
const representativeLaneKeys = [
  "OnRe/ONyc/USDC",
  "Prime/PRIME/USDC",
  "Maple/syrupUSDC/USDC",
  "AUTO/AUTO/PYUSD",
  "Ethena/USDe/PYUSD",
] as const;
const lanes = representativeLaneKeys.map((key) => {
  const lane = resolution.lanes.find((row) => row.key === key);
  if (!lane?.exact) throw new Error(`representative market lane ${key} is not exact`);
  return lane;
});
const jupiter = await resolveRepresentativeJupiterFamilies(new Connection(rpcUrl, "confirmed"));
if (jupiter.verdict !== "PASS_FAMILIES_PROBED") throw new Error(JSON.stringify(jupiter));
const evidence = {
  schema: "loyal-backyard-rwa-policy-family-probes/v1",
  broadcast: false,
  commitment: "confirmed",
  resolutionContextSlot: resolution.contextSlot,
  policySeedBefore: resolution.policySeedBefore,
  marketFamilies: lanes,
  jupiter,
};
writeFileSync(output, `${JSON.stringify(evidence, null, 2)}\n`, { flag: "w" });
console.log(JSON.stringify({ output, marketFamilyCount: lanes.length,
  jupiterFamilyCount: jupiter.families.length, verdict: "PASS_FAMILY_PROBES" }));
