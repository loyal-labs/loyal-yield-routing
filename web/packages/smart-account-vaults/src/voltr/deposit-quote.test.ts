import { describe, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { Keypair } from "@solana/web3.js";

import { parseEarnMaxDepositQuote } from "./deposit-quote";
import { voltrDepositPlan } from "./plans";

const golden = JSON.parse(
  readFileSync(
    join(
      import.meta.dir,
      "../../../../../crates/squads-test-harness/fixtures/voltr/deposit-quote-go.json",
    ),
    "utf8",
  ),
);

describe("Go deposit quote contract", () => {
  test("the quote the Go API issues builds a guarded deposit", () => {
    const quote = parseEarnMaxDepositQuote(golden);
    const plan = voltrDepositPlan(
      { authority: quote.authority, owner: Keypair.generate().publicKey },
      quote,
      quote.expiresAt - BigInt(10),
    );
    expect(plan.vault.length).toBe(1);
    expect(plan.outer.length).toBeGreaterThan(3);
  });
});
