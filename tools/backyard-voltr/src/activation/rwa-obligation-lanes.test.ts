import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { fileURLToPath } from "node:url";

import { describe, expect, test } from "bun:test";

import { assertJournalLaneFilter, journalPathForTag, LANE_FILTER_ENV, JOURNAL_TAG_ENV, parseJournalTag, parseLaneFilter, selectLanes } from "./rwa-obligation-lanes.js";

const ROOT = resolve(fileURLToPath(new URL("../../../..", import.meta.url)));
const LANE_KEYS = ["Maple/syrupUSDC/USDC", "Prime/PRIME/USDC", "OnRe/ONyc/USDC", "AUTO/AUTO/PYUSD"] as const;
const LANES = LANE_KEYS.map((key) => ({ key, resolved: { obligation: `obligation-${key}` } }));
const DEFAULT_JOURNAL = "docs/evidence/backyard-rwa-go/policy-phase2-obligation-init-v1.json";

describe("parseLaneFilter", () => {
  test("accepts listed lanes, trims surrounding whitespace, and preserves operator order", () => {
    const filter = parseLaneFilter("  AUTO/AUTO/PYUSD , Maple/syrupUSDC/USDC ,OnRe/ONyc/USDC", LANE_KEYS);
    expect(filter.keys).toEqual(["AUTO/AUTO/PYUSD", "Maple/syrupUSDC/USDC", "OnRe/ONyc/USDC"]);
  });

  test("accepts a single lane", () => {
    expect(parseLaneFilter("Prime/PRIME/USDC", LANE_KEYS).keys).toEqual(["Prime/PRIME/USDC"]);
  });

  test("refuses an unset or blank value", () => {
    for (const value of [undefined, "", "   ", "\n\t"]) {
      expect(() => parseLaneFilter(value, LANE_KEYS)).toThrow(new RegExp(`^${LANE_FILTER_ENV} is required`));
    }
  });

  test("refuses an empty entry", () => {
    expect(() => parseLaneFilter("Maple/syrupUSDC/USDC,,Prime/PRIME/USDC", LANE_KEYS)).toThrow(/empty lane key/);
  });

  test("refuses a lane that is not one of the resolution lanes", () => {
    expect(() => parseLaneFilter("Maple/syrupUSDC/USDC,NotALane/NOPE/USDC", LANE_KEYS)).toThrow(/unknown lane "NotALane\/NOPE\/USDC"/);
  });

  test("refuses a duplicate lane", () => {
    expect(() => parseLaneFilter("Maple/syrupUSDC/USDC,Prime/PRIME/USDC,Maple/syrupUSDC/USDC", LANE_KEYS)).toThrow(/repeats lane "Maple\/syrupUSDC\/USDC"/);
  });
});

describe("selectLanes", () => {
  test("returns only the named lanes, in the filter's order", () => {
    const filter = parseLaneFilter("OnRe/ONyc/USDC,Maple/syrupUSDC/USDC", LANE_KEYS);
    expect(selectLanes(LANES, filter).map((lane) => lane.key)).toEqual(["OnRe/ONyc/USDC", "Maple/syrupUSDC/USDC"]);
  });

  test("refuses a lane the resolution artifact does not contain", () => {
    expect(() => selectLanes(LANES, { keys: ["Maple/syrupUSDC/USDC", "Ghost/GHOST/USDC"] })).toThrow(/"Ghost\/GHOST\/USDC" is absent from the resolution artifact/);
  });
});

describe("assertJournalLaneFilter", () => {
  const filter = parseLaneFilter("Maple/syrupUSDC/USDC,Prime/PRIME/USDC", LANE_KEYS);

  test("accepts a journal recorded under the same filter", () => {
    assertJournalLaneFilter({ laneFilter: [...filter.keys] }, filter, "pending obligation journal");
  });

  test("refuses a journal recorded under a different filter", () => {
    expect(() => assertJournalLaneFilter({ laneFilter: ["Maple/syrupUSDC/USDC"] }, filter, "pending obligation journal"))
      .toThrow(/pending obligation journal laneFilter \["Maple\/syrupUSDC\/USDC"\] does not match the current RWA_OBLIGATION_LANES/);
  });

  test("refuses the same lanes listed in a different order", () => {
    expect(() => assertJournalLaneFilter({ laneFilter: ["Prime/PRIME/USDC", "Maple/syrupUSDC/USDC"] }, filter, "pending obligation journal"))
      .toThrow(/refusing to resume across a lane-filter change/);
  });

  test("refuses a journal written before the filter existed", () => {
    expect(() => assertJournalLaneFilter({ verdict: "SIGNED_SIMULATION_PASS_PENDING_SEND" }, filter, "pending obligation journal"))
      .toThrow(/no laneFilter array/);
    expect(() => assertJournalLaneFilter({ laneFilter: ["Maple/syrupUSDC/USDC", 7] }, filter, "pending obligation journal"))
      .toThrow(/no laneFilter array/);
  });
});

describe("parseJournalTag", () => {
  test("accepts a filename-safe tag", () => {
    expect(parseJournalTag("maple-prime_2026.09.09")).toBe("maple-prime_2026.09.09");
    expect(parseJournalTag("Tag.09_x-Y")).toBe("Tag.09_x-Y");
  });

  test("keeps the default v1 journal when unset", () => {
    expect(parseJournalTag(undefined)).toBe(null);
  });

  test("refuses a blank value instead of silently falling back to v1", () => {
    for (const value of ["", "   ", "\n\t"]) {
      expect(() => parseJournalTag(value)).toThrow(new RegExp(`^${JOURNAL_TAG_ENV} is set but empty`));
    }
  });

  test("refuses characters outside letters, digits, dot, underscore, and dash", () => {
    for (const value of ["maple/prime", "bad tag", "../escape", "tag:v1", "tag\\x"]) {
      expect(() => parseJournalTag(value)).toThrow(/may only contain letters, digits/);
    }
    let message = "";
    try { parseJournalTag("maple/prime"); } catch (error) { message = (error as Error).message; }
    expect(message).toContain(`RWA_OBLIGATION_JOURNAL_TAG "maple/prime"`);
  });

  test("refuses a tag longer than 40 characters", () => {
    expect(parseJournalTag("a".repeat(40))).toBe("a".repeat(40));
    expect(() => parseJournalTag("a".repeat(41))).toThrow(new RegExp(`^${JOURNAL_TAG_ENV} "a{41}" exceeds 40 characters`));
  });

  test("refuses the reserved v1 tag", () => {
    expect(() => parseJournalTag("v1")).toThrow(new RegExp(`^${JOURNAL_TAG_ENV} "v1" is reserved for the existing journal`));
  });
});

describe("journalPathForTag", () => {
  test("keeps the committed v1 journal path when the tag is unset", () => {
    expect(journalPathForTag(DEFAULT_JOURNAL, null)).toBe(DEFAULT_JOURNAL);
  });

  test("derives the tagged journal path and its pending path", () => {
    const journalPath = journalPathForTag(DEFAULT_JOURNAL, parseJournalTag("maple-prime-2026-09-09"));
    expect(journalPath).toBe("docs/evidence/backyard-rwa-go/policy-phase2-obligation-init-maple-prime-2026-09-09.json");
    expect(`${journalPath}.pending`).toBe("docs/evidence/backyard-rwa-go/policy-phase2-obligation-init-maple-prime-2026-09-09.json.pending");
  });

  test("never retargets onto the committed v1 journal", () => {
    expect(journalPathForTag(DEFAULT_JOURNAL, parseJournalTag("v1-2026-09-09"))).not.toBe(DEFAULT_JOURNAL);
  });
});

describe("operator lane filter for the Maple and Prime run", () => {
  const resolution = JSON.parse(readFileSync(resolve(ROOT, "docs/evidence/backyard-rwa-go/policy-resolution-v1.json"), "utf8")) as Record<string, unknown>;
  const lanes = resolution.lanes as ReadonlyArray<{ key: string; resolved: { obligation: string } }>;
  const filter = parseLaneFilter("OnRe/ONyc/USDC,Maple/syrupUSDC/USDC,Prime/PRIME/USDC", lanes.map((lane) => lane.key));

  test("resolves the three listed lanes to their exact obligations and skips the other eight", () => {
    expect(lanes).toHaveLength(11);
    expect(selectLanes(lanes, filter).map((lane) => [lane.key, lane.resolved.obligation])).toEqual([
      ["OnRe/ONyc/USDC", "4LnCFir7Qc99GhjGHLcwtkfweyAMu37u5QE1zTupKsei"],
      ["Maple/syrupUSDC/USDC", "Gtwj2FNuiPoV2mGLC5SpHZ9PCmDrHHKaHXtacRaqm8vT"],
      ["Prime/PRIME/USDC", "9suFBUhW7D7jN141mKR49Hn1WYDHEsRnPiGhxxm7RFkv"],
    ]);
    expect(lanes.length - filter.keys.length).toBe(8);
  });
});
