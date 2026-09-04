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
  const rollovers = readJson("docs/evidence/backyard-rwa-go/phase2-runtime/current-policy-rollovers-v1.json");
  const restoreIncident = readJson("docs/evidence/backyard-rwa-go/phase2-runtime/voltr-restore-incident-v1.json");
  const signedUnsent = readJson("docs/evidence/backyard-rwa-go/phase2-runtime/signed-unsent-v1.json");

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
      const rollover = rollovers.policies.find((value: any) => JSON.stringify(value.binding) === JSON.stringify(policy));
      expect(compiledPolicy !== undefined || rollover !== undefined).toBe(true);
      if (rollover !== undefined) {
        expect(rollover.owner).toBe(manifest.identities.squadsProgram);
        expect(rollover.liveAccountDataSha256).toBe(policy.liveAccountDataSha256);
      } else {
        const constraint = compiledPolicy.constraints[0];
        expect(policy.programId).toBe(constraint.programId);
        expect(policy.accountPubkeys).toEqual(constraint.accountPubkeys.flatMap((value: any) => value.pubkeys));
        expect(policy.data).toEqual(constraint.data);
        expect(policy.packetBytes).toBe(compiledPolicy.createPacketBytes);
      }
    }
    expect(rollovers.settings.policySeed).toBe("139");
    expect(rollovers.commitment).toBe("finalized");
  });

  test("keeps the sole restore exception exact and outside reconciled lifecycle operations", () => {
    expect(restoreIncident.operationId).toBe("fe45a0369bf950da3ea311a4c493377cf9720a92c359c0bfbe739a3d9f699cbe");
    expect(restoreIncident.transactionSignature).toBe("46UBvSw1zjtZyDVUVaissm9SEXsKFKnYCQYKd23njb1NS1Ktkzsup5ic9XA55FxyTCpkoYuuM8hhn4MioGU2X7Wz");
    expect(restoreIncident.requestedAmountRaw).toBe("1000000");
    expect(restoreIncident.actualAmountRaw).toBe("3793417");
    expect(restoreIncident.durableStatus).toBe("manual_recovery");
    expect(restoreIncident.conservation).toEqual({
      usdcDeltaRaw: "0",
      capitalLost: false,
      destinationChanged: false,
    });
    expect(restoreIncident.operatorAuthorization.authorized).toBe(true);
    expect(restoreIncident.exceptionScope).toMatchObject({
      ordinaryCapsRemainUnchanged: true,
      additionalExceptionsAuthorized: false,
      qualifiesAsReconciledLifecycleOperation: false,
      satisfiesTerminalRestoration: true,
    });
  });

  test("keeps the R03 lifecycle signed-unsent and removes every broadcast surface", () => {
    const producer = readFileSync(resolve(ROOT, "tools/backyard-voltr/src/verify/generate-rwa-phase2-r03-plan.ts"), "utf8");
    expect(producer).not.toMatch(/\.send(?:Raw)?Transaction\s*\(/);
    expect(producer).toContain("this producer has no broadcast mode");
    expect(signedUnsent).toMatchObject({
      verdict: "PASS",
      broadcast: false,
      signedUnsent: true,
      selectedLane: "Maple/syrupUSDC/USDC",
      signatureAbsentOnChain: true,
    });
    expect(signedUnsent.chainPreStateSha256).toBe(signedUnsent.chainPostStateSha256);
    expect(signedUnsent.transactions).toHaveLength(9);
    expect(signedUnsent.transactions.every((row: any) => row.simulationPassed === true && row.packetBytes <= 1232)).toBe(true);
  });
});
