import { describe, expect, test } from "bun:test";
import { exactSet, measuredCondition, localCapTestProof } from "./rwa-phase3-family-activation.js";

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
  test("local cap evidence requires all named behavioral tests and a successful package result", () => {
    const names=["TestProductionBridgeRejectsFreshOverCapCostBeforeSignerOrDatabase",
      "TestProductionKaminoAndJupiterRejectFreshOverCapCostBeforeSigner",
      "TestKnownBuildCostRejectsStaleObservationAndDoesNotGrantAdmission"];
    const events=[...names.map(Test=>({Action:"pass",Test})),{Action:"pass"}];
    const encode=(rows:unknown[])=>rows.map(row=>JSON.stringify(row)).join("\n");
    expect(localCapTestProof(encode(events),0).pass).toBe(true);
    expect(localCapTestProof(encode(events),1).pass).toBe(false);
    expect(localCapTestProof(encode(events.slice(1)),0).pass).toBe(false);
    expect(localCapTestProof(encode(events.slice(0,-1)),0).pass).toBe(false);
    expect(localCapTestProof(encode([...events,{Action:"pass",Test:names[0]}]),0).pass).toBe(false);
    expect(localCapTestProof(encode([{Action:"skip",Test:names[0]},...events.slice(1)]),0).pass).toBe(false);
    expect(localCapTestProof(encode([...events,{Action:"fail",Test:"nested/negative"}]),0).pass).toBe(false);
    expect(localCapTestProof("",0).pass).toBe(false);
  });
});
