import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";

import { buildPhaseTwoKaminoLaneOperations, hasConfiguredKaminoOracle, resolutionLanes } from "./rwa-multiply-phase2-kamino.js";

test("Phase-2 Kamino deposit preserves a positive quoted execution amount", () => {
  const root = resolve(fileURLToPath(new URL("../../../..", import.meta.url)));
  const resolution = JSON.parse(readFileSync(resolve(root, "docs/evidence/backyard-rwa-go/policy-resolution-v1.json"), "utf8")) as Record<string, unknown>;
  const lane = resolutionLanes(resolution).find((entry) => entry.key === "AUTO/AUTO/PYUSD");
  assert.ok(lane);
  const amount = 980_476n;
  const deposit = buildPhaseTwoKaminoLaneOperations(lane, amount).find((entry) => entry.operation === "deposit");
  assert.ok(deposit);
  const data = Buffer.from(deposit.dataBase64, "base64");
  assert.equal(data.readBigUInt64LE(8), amount);
  assert.throws(() => buildPhaseTwoKaminoLaneOperations(lane, 0n), /outside the constrained policy cap/);
});

test("K-Lend null oracle sentinel is omitted rather than passed to RefreshReserve", () => {
  assert.equal(hasConfiguredKaminoOracle("nu11111111111111111111111111111111111111111"), false);
  assert.equal(hasConfiguredKaminoOracle("11111111111111111111111111111111"), false);
  assert.equal(hasConfiguredKaminoOracle("3t4JZcueEzTbVP6kLxXrL3VpWx45jDer4eqysweBchNH"), true);
});
