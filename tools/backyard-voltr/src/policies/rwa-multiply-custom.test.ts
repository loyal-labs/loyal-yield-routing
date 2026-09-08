import assert from "node:assert/strict";
import { test } from "node:test";

import { generated as squadsGenerated } from "@loyal-labs/loyal-smart-accounts-core";
import { Keypair, PublicKey } from "@solana/web3.js";

import {
  rwaMultiplyStrategyTwoTarget,
  STRATEGY_TWO_DAILY_SPENDING_LIMIT_RAW,
  type StrategyTwoIdentity,
} from "../domain/rwa-multiply-strategy2-route-spec.js";
import { RWA_MULTIPLY_ROUTE } from "../domain/rwa-multiply-route-spec.js";
import {
  selectCustomPolicyMutation,
  spendingLimitsMatch,
  type CustomPolicyArtifact,
  type CustomPolicyVerificationRow,
} from "./rwa-multiply-custom.js";

const operations = ["allocation", "nav-refresh", "stage-withdrawal", "withdraw"] as const;

function artifact(): CustomPolicyArtifact {
  return {
    schema: "loyal-voltr-custom-policy-artifact/v5",
    verdict: "VOLTR_CUSTOM_POLICY_ARTIFACT_COMPILED_NOT_DEPLOYED",
    physicalPolicyCount: 4,
    deploymentReady: false,
    sourceSha256: "0".repeat(64),
    compiler: {
      compilerBinarySha256: "1".repeat(64),
      compilerBinaryPath: "/repo/target/backyard-voltr-compilers/debug/compile-voltr-custom-policy",
      compilerTargetDir: "/repo/target/backyard-voltr-compilers",
      compilerSourceTreeSha256: "2".repeat(64),
    },
    policies: operations.map((operation, index) => {
      const seed = String(62 + index);
      const policy = `policy-${seed}`;
      const base = {
        programId: "squads",
        accounts: [],
        dataBase64: operation,
      } as const;
      return {
        operation,
        seed,
        policy,
        constraintIndex: 0 as const,
        constraintIndices: operation === "stage-withdrawal" ? [0] : [0, 1],
        createInstruction: { ...base, dataBase64: `create-${operation}` },
      };
    }),
  };
}

function exactRows(value: CustomPolicyArtifact): CustomPolicyVerificationRow[] {
  return value.policies.map(({ operation, seed, policy }) => ({
    operation,
    seed,
    policy,
    pass: true,
    dataSha256: "1".repeat(64),
  }));
}

test("exact installed custom policies are a no-op", () => {
  const value = artifact();
  assert.deepEqual(selectCustomPolicyMutation({
    policySeedBefore: 65n,
    artifact: value,
    rows: exactRows(value),
  }), { kind: "noop" });
});

test("inexact installed policy fails closed and requires a fresh-seed rollover", () => {
  const value = artifact();
  const rows = exactRows(value);
  rows[1] = { ...rows[1]!, pass: false, reason: "inexact policy payload" };
  assert.throws(
    () => selectCustomPolicyMutation({ policySeedBefore: 65n, artifact: value, rows }),
    /policy seeds are monotonic and require a fresh-seed rollover/,
  );
});

test("absent next seed creates while absent historical seed fails closed", () => {
  const value = artifact();
  const rows = exactRows(value);
  const { dataSha256: _, ...row } = rows[0]!;
  rows[0] = { ...row, pass: false, reason: "absent" };
  const selected = selectCustomPolicyMutation({ policySeedBefore: 61n, artifact: value, rows });
  assert.equal(selected.kind, "create");
  if (selected.kind !== "create") throw new Error("expected create");
  assert.equal(selected.instructions[0].dataBase64, "create-allocation");
  assert.throws(() => selectCustomPolicyMutation({ policySeedBefore: 65n, artifact: value, rows }),
    /not the next finalized Settings seed/);
});

test("inexact policy with a mismatched authority boundary also requires rollover", () => {
  const value = artifact();
  const rows = exactRows(value);
  rows[2] = {
    ...rows[2]!,
    pass: false,
    reason: "authority boundary mismatch",
  };
  assert.throws(() => selectCustomPolicyMutation({ policySeedBefore: 65n, artifact: value, rows }),
    /fresh-seed rollover/);
});

test("finalized Policy account decoding verifies the nested daily spending limit shape", async () => {
  const identity: StrategyTwoIdentity = {
    config: Keypair.generate().publicKey.toBase58() as StrategyTwoIdentity["config"],
    delegatedSigner: Keypair.generate().publicKey.toBase58() as StrategyTwoIdentity["delegatedSigner"],
  };
  const target = await rwaMultiplyStrategyTwoTarget(identity, 140n);
  const policy = (squadsGenerated as unknown as {
    Policy: {
      fromArgs(args: Record<string, unknown>): { serialize(): [Buffer, number] };
      fromAccountInfo(account: {
        data: Buffer;
        executable: boolean;
        lamports: number;
        owner: PublicKey;
        rentEpoch: number;
      }): [Readonly<{ policyState: Readonly<{ fields?: readonly unknown[] }> }>, number];
    };
  }).Policy;
  const policyData = policy.fromArgs({
    settings: new PublicKey(RWA_MULTIPLY_ROUTE.squads.settings),
    seed: 143n,
    bump: 1,
    transactionIndex: 0n,
    staleTransactionIndex: 0n,
    signers: [{
      key: new PublicKey(identity.delegatedSigner),
      permissions: { mask: 7 },
    }],
    threshold: 1,
    timeLock: 0,
    policyState: {
      __kind: "ProgramInteraction",
      fields: [{
        accountIndex: RWA_MULTIPLY_ROUTE.squads.vaultIndex,
        instructionsConstraints: [],
        preHook: null,
        postHook: null,
        spendingLimits: [{
          mint: new PublicKey(target.caps.dailySpendingLimit!.mint),
          timeConstraints: {
            start: 0n,
            expiration: null,
            period: { __kind: "Daily" },
            accumulateUnused: false,
          },
          quantityConstraints: {
            maxPerPeriod: STRATEGY_TWO_DAILY_SPENDING_LIMIT_RAW,
            maxPerUse: 0n,
            enforceExactQuantity: false,
          },
          usage: {
            remainingInPeriod: STRATEGY_TWO_DAILY_SPENDING_LIMIT_RAW,
            lastReset: 0n,
          },
        }],
      }],
    },
    start: 0n,
    expiration: null,
    rentCollector: new PublicKey(RWA_MULTIPLY_ROUTE.setupAdmin),
  }).serialize()[0];
  const [decoded] = policy.fromAccountInfo({
    data: policyData,
    executable: false,
    lamports: 1,
    owner: new PublicKey(RWA_MULTIPLY_ROUTE.squads.program),
    rentEpoch: 0,
  });
  const body = decoded.policyState.fields?.[0] as {
    spendingLimits: readonly unknown[];
  };
  assert.equal(spendingLimitsMatch(body.spendingLimits, target.caps.dailySpendingLimit), true);

  const finalizedLimit = body.spendingLimits[0] as {
    mint: PublicKey;
    timeConstraints: { period: { __kind: string } };
    quantityConstraints: { maxPerPeriod: { toString(): string } };
  };
  assert.equal(spendingLimitsMatch([{
    mint: finalizedLimit.mint,
    period: finalizedLimit.timeConstraints.period,
    quantityConstraints: finalizedLimit.quantityConstraints,
  }], target.caps.dailySpendingLimit), false);
});
