import { createHash } from "node:crypto";
import { strict as assert } from "node:assert";
import { readFileSync } from "node:fs";
import { test } from "node:test";
import { resolve } from "node:path";

test("the sole verifier pins the exact frozen Phase-1 v9 plan", () => {
  const repository = resolve(import.meta.dirname, "../../../..");
  const plan = readFileSync(resolve(repository, "docs/plans/backyard-voltr-orchestrator-verifier.md"));
  const verifier = readFileSync(resolve(repository,
    "tools/backyard-voltr/src/verify/rwa-multiply-custom-lifecycle.ts"), "utf8");
  const pinned = verifier.match(/const PLAN_SHA256 = "([0-9a-f]{64})";/)?.[1];
  assert.equal(pinned, createHash("sha256").update(plan).digest("hex"));
});
