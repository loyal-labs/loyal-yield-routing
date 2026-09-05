import { describe, expect, test } from "bun:test";
import { exactSet, measuredCondition } from "./rwa-phase3-family-activation.js";

describe("Phase 3 measured verifier", () => {
  test("rejects missing, duplicate, extra and substituted lane identities", () => {
    expect(exactSet(["a","b"],["b","a"])).toBe(true);
    for (const candidate of [[],["a"],["a","a"],["a","b","c"],["a","c"],null])
      expect(exactSet(candidate,["a","b"])).toBe(false);
  });
  test("passing observations cannot conceal missing behavioral proof", () => {
    const result = measuredCondition("R01","cap gate",[{claim:"compiled caps",verdict:"PASS",evidence:[1,20,60]}],["production admission"]);
    expect(result.verdict).toBe("FAIL");
    expect(result.measurementStatus).toBe("MEASURED");
    expect(result.missingProof).toEqual(["production admission"]);
  });
  test("distinguishes unavailable observation from a falsified condition", () => {
    const unavailable = {claim:"current state",verdict:"BLOCKED" as const,evidence:"credentials unavailable"};
    expect(measuredCondition("R05","live state",[unavailable],[]).verdict).toBe("BLOCKED");
    expect(measuredCondition("R05","live state",[unavailable,{claim:"image",verdict:"FAIL",evidence:"wrong source"}],[]).verdict).toBe("FAIL");
  });
  test("requires nonempty complete passing measurements", () => {
    expect(measuredCondition("R04","lifecycle",[],[]).verdict).toBe("FAIL");
    expect(measuredCondition("R04","lifecycle",[{claim:"execution",verdict:"PASS",evidence:{}}],[]).verdict).toBe("PASS");
  });
});
