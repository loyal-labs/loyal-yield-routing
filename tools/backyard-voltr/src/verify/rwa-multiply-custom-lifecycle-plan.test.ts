import { createHash } from "node:crypto";
import { strict as assert } from "node:assert";
import { readFileSync } from "node:fs";
import { test } from "node:test";
import { resolve } from "node:path";

test("the sole verifier pins the exact frozen Phase-1 v10 plan", () => {
  const repository = resolve(import.meta.dirname, "../../../..");
  const plan = readFileSync(resolve(repository, "docs/plans/backyard-voltr-orchestrator-verifier.md"));
  const verifier = readFileSync(resolve(repository,
    "tools/backyard-voltr/src/verify/rwa-multiply-custom-lifecycle.ts"), "utf8");
  const pinned = verifier.match(/const PLAN_SHA256 = "([0-9a-f]{64})";/)?.[1];
  assert.equal(pinned, createHash("sha256").update(plan).digest("hex"));
});

test("Phase-1 manifest and verifier pin only the finalized bridge policy rollover", () => {
  const repository = resolve(import.meta.dirname, "../../../..");
  const manifestPath = resolve(repository, "docs/manifests/backyard-rwa-v1.json");
  const embeddedPath = resolve(repository,
    "go/backyard-rwa-worker/internal/backyardrwa/manifest/backyard-rwa-v1.json");
  const manifestBytes = readFileSync(manifestPath);
  assert.deepEqual(manifestBytes, readFileSync(embeddedPath));
  const manifest = JSON.parse(manifestBytes.toString("utf8")) as {
    runtimeBindings: { bridgePolicies: unknown[] };
    deployment: { sourceCommit: unknown; imageDigest: unknown; singleWriterService: unknown };
  };
  assert.deepEqual(manifest.runtimeBindings.bridgePolicies, [
    { action: "VOLTR_ALLOCATE_TO_SQUADS", account: "HoDV7mtsb2u1VARZLYuGByW7cCsGWL9NFxHZs7WHjdzz", dataSha256: "bda72932f474064fa3cd60ce91633acba35b2730e86b82f4352aa96a6738e2f4" },
    { action: "REPORT_NAV", account: "41nzu42c3KPgJfWhnV5jbfxjHbvVU6HXaiJmzzYNqvBP", dataSha256: "bf34a3e9c9c635c79a0d30e096b639a86d52e300ad113c81161e3486832d97ca" },
    { action: "STAGE_SQUADS_TO_VOLTR", account: "ALz5Wkt82GhGFH1LfzbnAovkZ6t85ErovbxHUH3yY1wY", dataSha256: "ef8c231497fb2620b5930cfe5d329c871f103db6512781eb5487534db8b1291b" },
    { action: "VOLTR_RESTORE_IDLE", account: "DjYYkQWb4zYbySfEndjVdg2NwZ8i77Fb9P1UFVbebc5t", dataSha256: "84e8f6f881758cff1714ef743603c016024104f9834392c6fba693c3651b719c" },
  ]);
  assert.deepEqual(manifest.deployment,
    { sourceCommit: null, imageDigest: null, singleWriterService: null });
  const verifier = readFileSync(resolve(repository,
    "tools/backyard-voltr/src/verify/rwa-multiply-custom-lifecycle.ts"), "utf8");
  assert.match(verifier, /const RETIRED_BRIDGE_POLICY_SEEDS = \[53n, 54n, 55n, 56n\]/);
  assert.match(verifier, /const BRIDGE_POLICY_ROUTE_SPEC_SHA256 = "6482b284172cd2b2da0317f9b33db737688d60cfe61f6b28c68da5ddbfc19550"/);
  assert.match(verifier, /const coherentAddresses = \[/);
  assert.match(verifier, /\.\.\.BRIDGE_POLICY_ROLLOVER\.map\(\(\{ account \}\) => account\)/);
  assert.match(verifier, /\.\.\.retiredAddresses\.map\(\(account\) => account!\)/);
  assert.match(verifier, /commitment: "finalized", minContextSlot: installed\.contextSlot/);
  assert.match(verifier, /current\.coherentDataSha256 === expected\.dataSha256/);
});

test("failed simulation evidence separates unavailable overlays from confirmed rollback proof", () => {
  const repository = resolve(import.meta.dirname, "../../../..");
  const generator = readFileSync(resolve(repository,
    "tools/backyard-voltr/src/verify/rwa-multiply-adaptor-simulation.ts"), "utf8");
  const verifier = readFileSync(resolve(repository,
    "tools/backyard-voltr/src/verify/rwa-multiply-custom-lifecycle.ts"), "utf8");

  for (const field of [
    "simulationPostAccountsAvailable",
    "simulationNullAddresses",
    "chainReadbackContextSlot",
    "chainReadbackStateSha256",
    "signatureStatus",
  ]) {
    assert.match(generator, new RegExp(`\\b${field}\\b`));
    assert.match(verifier, new RegExp(`\\b${field}\\b`));
  }
  assert.match(generator, /simulationNullAddresses\.length === inspectedAddresses\.length/);
  assert.match(generator, /confirmedAccountsAtOrAfter\(\s*connection, inspectedAddresses, prepared\.simulationSlot/);
  assert.match(generator, /minContextSlot: minimumContextSlot/);
  assert.match(generator, /getSignatureStatuses\(\s*\[prepared\.expectedSignature\]/);
  assert.match(verifier, /VersionedTransaction\.deserialize\(wire\)\.signatures\[0\]/);
  assert.match(verifier, /getSignatureStatuses/);
  assert.match(verifier, /row\.signatureStatus === null/);
  assert.doesNotMatch(generator,
    /Voltr post-arm failure did not return account images needed to prove in-transaction rollback/);
  assert.doesNotMatch(verifier,
    /row\.name !== "voltr_failure_rolls_back_ticket_and_capital"/);
});

test("v10 treats Arm-only as bounded signed-unsent success and keeps 38 rejections", () => {
  const repository = resolve(import.meta.dirname, "../../../..");
  const plan = readFileSync(resolve(repository,
    "docs/plans/backyard-voltr-orchestrator-verifier.md"), "utf8");
  const generator = readFileSync(resolve(repository,
    "tools/backyard-voltr/src/verify/rwa-multiply-adaptor-simulation.ts"), "utf8");
  const verifier = readFileSync(resolve(repository,
    "tools/backyard-voltr/src/verify/rwa-multiply-custom-lifecycle.ts"), "utf8");
  const processor = readFileSync(resolve(repository,
    "crates/loyal-voltr-rwa-nav-adaptor/src/processor.rs"), "utf8");

  assert.match(plan, /38 rejections plus one bounded expected-success/);
  assert.match(plan, /only the ticket simulation image\s+changes/);
  assert.match(plan, /active ticket\s+whose active_sequence is older than the\s+configured max report age may be replaced/);
  assert.match(generator, /expectation: armOnlyExpectedSuccess \? "arm-only-success" : "rejection"/);
  assert.match(generator, /simulationChangedAddresses\.length === 1/);
  assert.match(generator, /armedTicket\.lastConsumedSequence === ticketBefore\.lastConsumedSequence/);
  assert.match(generator, /signatureStatuses\.value\[0\] === null/);
  assert.match(verifier, /row\.expectation === "arm-only-success"/);
  assert.match(verifier, /armOnlyTicketTransitionExact === true/);
  assert.match(verifier, /expectation === "rejection"\)\.length === 38/);
  assert.match(processor, /age > max_report_age_slots/);
  assert.match(processor, /Err\(AdaptorError::TicketAlreadyArmed\)/);
});
