import { describe, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";

const root = resolve(import.meta.dir, "../../../..");
const read = (path: string) => readFileSync(resolve(root, path), "utf8");

describe("Backyard close-out standing contract", () => {
  test("action vocabulary and partner boundary remain explicit", () => {
    const plan = read("docs/plans/backyard-voltr-orchestrator-verifier.md");
    const handoff = read("docs/backyard-rwa-partner-handoff.md");
    for (const action of ["SWAP_USDC_TO_PRIME_STEP", "SWAP_PRIME_TO_USDC_STEP"]) {
      expect(plan).toContain(action);
    }
    for (const boundary of ["fixed `PRIME/USDC`", "600-second", "There is no optimizer", "consumer Earn Max"]) {
      expect(handoff).toContain(boundary);
    }
  });

  test("archived evidence is not an execution replay gate", () => {
    const verifier = read("tools/backyard-voltr/src/verify/rwa-multiply-custom-lifecycle.ts");
    const adaptorStart = verifier.indexOf("async function adaptorCheck");
    const adaptorEnd = verifier.indexOf("async function legacyPolicyCatalogCheck");
    const activeAdaptor = verifier.slice(adaptorStart, adaptorEnd);
    expect(activeAdaptor).not.toContain("independentSignedSimulations(");
    expect(activeAdaptor).toContain("independentSignedUnsentAudit(");
    expect(activeAdaptor).toContain("historicalReplayRetired: true");
  });

  test("policy capability is set-exact and packet bounded", () => {
    const catalog = JSON.parse(read("crates/loyal-actions/fixtures/backyard_rwa_policy_catalog_v1.json"));
    const install = JSON.parse(read("docs/evidence/backyard-rwa-go/policy-install-readback-v1.json"));
    const packets = JSON.parse(read("docs/evidence/backyard-rwa-go/policy-packets-v1.json"));
    expect(catalog.lanes).toHaveLength(11);
    expect(catalog.operations).toHaveLength(44);
    expect(catalog.swapEdges).toHaveLength(52);
    expect(install.operations).toHaveLength(70);
    expect(new Set(install.operations.map((row: { policyAddress: string }) => row.policyAddress)).size).toBe(70);
    const selected = packets.measurements.filter((row: { rung: string }) =>
      row.rung === "kamino/partition-1+1+1+1" || row.rung === "swap/byte-optimal-best-fit-size");
    expect(selected).toHaveLength(70);
    expect(selected.every((row: { packetBytes: number }) => row.packetBytes <= 1232)).toBe(true);
  });
});
