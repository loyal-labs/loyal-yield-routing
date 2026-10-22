import { describe, expect, test } from "bun:test";
import type { SmartAccountPreparedEarnUsdcDeposit } from "@loyal-labs/smart-account-vaults";
import { PublicKey } from "@solana/web3.js";

import { getEarnDepositReviewStages } from "./earn-deposit-flow.shared";

const POLICY_ACCOUNT = new PublicKey("11111111111111111111111111111111");

function createPreparedDeposit(args: {
  finalize?: boolean;
  policyInitialization: "create" | "reuse";
  setup?: boolean;
}): SmartAccountPreparedEarnUsdcDeposit {
  return {
    persistence: {
      policyInitialization: args.policyInitialization,
    },
    policy: {
      account: POLICY_ACCOUNT,
      seed: BigInt(7),
    },
    policyFinalizePrepared: args.finalize ? ({} as never) : null,
    policySetupPrepared: args.setup ? ({} as never) : null,
  } as SmartAccountPreparedEarnUsdcDeposit;
}

describe("Earn deposit flow helpers", () => {
  test("first deposit without finalize requires setup then deposit", () => {
    const preparedDeposit = createPreparedDeposit({
      policyInitialization: "create",
      setup: true,
    });

    expect(getEarnDepositReviewStages({ preparedDeposit }).join(">")).toBe(
      "policy>deposit"
    );
  });

  test("first deposit with finalize requires setup, finalize, then deposit", () => {
    const preparedDeposit = createPreparedDeposit({
      finalize: true,
      policyInitialization: "create",
      setup: true,
    });

    expect(getEarnDepositReviewStages({ preparedDeposit }).join(">")).toBe(
      "policy>policy-finalize>deposit"
    );
  });

  test("top-up is a single deposit stage", () => {
    const preparedDeposit = createPreparedDeposit({
      policyInitialization: "reuse",
    });

    expect(getEarnDepositReviewStages({ preparedDeposit }).join(">")).toBe(
      "deposit"
    );
  });
});
