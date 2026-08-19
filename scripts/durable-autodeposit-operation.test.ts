import { describe, expect, test } from "bun:test";

import {
  canCompleteDurableAutodeposit,
  durableDepositRetryEffect,
  initialDurableAutodepositState,
  nextDurableAutodepositAction,
  operationalAlertForDurableAutodeposit,
  reduceDurableAutodepositState,
} from "./durable-autodeposit-operation";

describe("durable autodeposit operation", () => {
  test("preflights deposit before pull and resumes deposit after a crash", () => {
    const initial = initialDurableAutodepositState();
    expect(nextDurableAutodepositAction(initial)).toBe("preflight_deposit");
    const ready = reduceDurableAutodepositState(initial, {
      type: "deposit_preflight_ready",
    });
    expect(nextDurableAutodepositAction(ready)).toBe("execute_pull");
    const pulled = reduceDurableAutodepositState(ready, {
      type: "pull_confirmed",
      signature: "pull",
    });
    expect(nextDurableAutodepositAction(pulled)).toBe("execute_deposit");
    expect(canCompleteDurableAutodeposit(pulled)).toBe(false);
  });

  test("retries ordinary deposit failures without paging", () => {
    const state = reduceDurableAutodepositState(
      reduceDurableAutodepositState(
        reduceDurableAutodepositState(initialDurableAutodepositState(), {
          type: "deposit_preflight_ready",
        }),
        { type: "pull_confirmed", signature: "pull" },
      ),
      { type: "deposit_retryable_failure" },
    );
    expect(nextDurableAutodepositAction(state)).toBe("execute_deposit");
    expect(operationalAlertForDurableAutodeposit(state)).toBeNull();
  });

  test("pages only for ambiguous effect or lost durable ownership", () => {
    const ambiguous = reduceDurableAutodepositState(
      initialDurableAutodepositState(),
      { type: "deposit_ambiguous" },
    );
    expect(operationalAlertForDurableAutodeposit(ambiguous)).toBe(
      "transaction_effect_ambiguous",
    );
    const lost = reduceDurableAutodepositState(
      initialDurableAutodepositState(),
      { type: "durable_ownership_lost" },
    );
    expect(operationalAlertForDurableAutodeposit(lost)).toBe(
      "durable_ownership_lost",
    );
  });

  test("does not retry after the durable post-pull source balance decreased", () => {
    expect(
      durableDepositRetryEffect({
        currentSourceBalanceRaw: 105n,
        postPullSourceBalanceRaw: 110n,
      }),
    ).toBe("ambiguous_prior_effect");
    expect(
      durableDepositRetryEffect({
        currentSourceBalanceRaw: 110n,
        postPullSourceBalanceRaw: 110n,
      }),
    ).toBe("safe_to_retry");
  });
});
