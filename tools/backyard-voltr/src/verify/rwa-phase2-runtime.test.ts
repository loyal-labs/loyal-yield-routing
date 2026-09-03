import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { fileURLToPath } from "node:url";

import { describe, expect, test } from "bun:test";

const ROOT = resolve(fileURLToPath(new URL("../../../..", import.meta.url)));
const readJson = (relative: string) => JSON.parse(readFileSync(resolve(ROOT, relative), "utf8")) as Record<string, any>;

describe("Backyard RWA Phase 2 runtime activation", () => {
  const manifest = readJson("docs/manifests/backyard-rwa-v1.json");
  const embedded = readJson("crates/loyal-actions/fixtures/backyard_rwa_policy_catalog_v1.json");
  const selection = readJson("docs/evidence/backyard-rwa-go/phase2-runtime/selection-v1.json");
  const compiled = readJson("docs/evidence/backyard-rwa-go/policy-compiled-v1.json");

  test("freezes exactly PRIME plus the selected Maple representative", () => {
    const activation = manifest.runtimeActivation;
    expect(activation.selectedLane).toBe("Maple/syrupUSDC/USDC");
    expect(activation.runtimeRoutes).toEqual(["Prime/PRIME/USDC", "Maple/syrupUSDC/USDC"]);
    expect(activation.selectionEvidence.sha256).toBe(
      createHash("sha256").update(readFileSync(resolve(ROOT, activation.selectionEvidence.path))).digest("hex"),
    );
    expect(selection.selectedLane).toBe(activation.selectedLane);
    expect(selection.verdict).toBe("PASS_SELECTED");
    expect(selection.broadcast).toBe(false);
  });

  test("keeps source and embedded bindings identical and complete", () => {
    const activation = manifest.runtimeActivation;
    expect(embedded.runtimeActivation).toEqual(activation);
    expect(activation.selectedLaneBinding.kaminoPolicies.map((value: any) => value.operation)).toEqual([
      "deposit", "borrow", "repay", "withdraw",
    ]);
    expect(activation.selectedLaneBinding.kaminoPolicies).toHaveLength(4);
    expect(activation.selectedLaneBinding.jupiterEdges).toHaveLength(2);
    for (const policy of activation.selectedLaneBinding.kaminoPolicies) {
      const compiledPolicy = compiled.policies.find((value: any) =>
        value.logicalName === `lane/${activation.selectedLane}` && value.operations.includes(policy.operation));
      expect(compiledPolicy).toBeDefined();
      const constraint = compiledPolicy.constraints[0];
      expect(policy.programId).toBe(constraint.programId);
      expect(policy.accountPubkeys).toEqual(constraint.accountPubkeys.flatMap((value: any) => value.pubkeys));
      expect(policy.data).toEqual(constraint.data);
      expect(policy.packetBytes).toBe(compiledPolicy.createPacketBytes);
    }
  });
});
