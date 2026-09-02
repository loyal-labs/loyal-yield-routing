import { mkdirSync, writeFileSync } from "node:fs";
import { resolve } from "node:path";
import { fileURLToPath } from "node:url";

import { resolveCurrentRwaMultiplyCatalogFromEnvironment } from "../policies/rwa-multiply-catalog-resolver.js";

const repositoryRoot = resolve(fileURLToPath(new URL("../../../..", import.meta.url)));
const output = resolve(repositoryRoot, "docs/evidence/backyard-rwa-go/policy-resolution-v1.json");
const resolution = await resolveCurrentRwaMultiplyCatalogFromEnvironment();

if (!resolution.laneGraphExact) {
  throw new Error("confirmed 11-lane account snapshot is incomplete");
}

mkdirSync(resolve(output, ".."), { recursive: true });
writeFileSync(output, `${JSON.stringify(resolution, null, 2)}\n`, { flag: "w" });
console.log(JSON.stringify({
  output,
  contextSlot: resolution.contextSlot,
  policySeedBefore: resolution.policySeedBefore,
  laneCount: resolution.lanes.length,
  laneGraphExact: resolution.laneGraphExact,
  requestedSwapEdgeCount: resolution.swap.edges.length,
}));
