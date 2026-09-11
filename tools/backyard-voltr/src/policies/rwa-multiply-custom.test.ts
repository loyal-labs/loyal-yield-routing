import assert from "node:assert/strict";
import { Buffer } from "node:buffer";
import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
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
  buildInstalledRowExpectations,
  installedPolicyRow,
  type InstalledReadbackOptions,
  selectCustomPolicyMutation,
  spendingLimitsMatch,
  type CustomPolicyArtifact,
  type CustomPolicyVerificationRow,
} from "./rwa-multiply-custom.js";

const operations = ["allocation", "nav-refresh", "stage-withdrawal", "withdraw"] as const;

/** The block time the Squads program stamps into a freshly created window start. */
const PROGRAM_ASSIGNED_START = BigInt(Math.floor(Date.now() / 1000) - 30);

function artifact(): CustomPolicyArtifact {
  return {
    schema: "loyal-voltr-custom-policy-artifact/v5",
    verdict: "VOLTR_CUSTOM_POLICY_ARTIFACT_COMPILED_NOT_DEPLOYED",
    physicalPolicyCount: 4,
    deploymentReady: false,
    sourceSha256: "0".repeat(64),
    compiler: {
      compilerBinarySha256: "1".repeat(64),
      compilerBinarySha256AtExec: "1".repeat(64),
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
  const target = await rwaMultiplyStrategyTwoTarget(identity, 144n);
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
    seed: 147n,
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
            start: PROGRAM_ASSIGNED_START,
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
            lastReset: PROGRAM_ASSIGNED_START,
          },
        }],
      }],
    },
    start: PROGRAM_ASSIGNED_START,
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

  const limit = body.spendingLimits[0] as {
    mint: PublicKey;
    timeConstraints: { start: bigint; expiration: null; period: { __kind: string }; accumulateUnused: boolean };
    quantityConstraints: { maxPerPeriod: bigint; maxPerUse: bigint; enforceExactQuantity: boolean };
    usage: { remainingInPeriod: bigint; lastReset: bigint };
  };
  const withStart = (start: bigint) => [{
    ...limit,
    timeConstraints: { ...limit.timeConstraints, start },
  }];
  // PolicyCreate leaves the window start to the program, which stamps the
  // landing block time into it: a zero or future start is not a state the
  // program produces, while a landed start inside the acceptance window is.
  assert.equal(spendingLimitsMatch(withStart(0n), target.caps.dailySpendingLimit), false);
  assert.equal(spendingLimitsMatch(withStart(BigInt(Math.floor(Date.now() / 1000)) + 3600n),
    target.caps.dailySpendingLimit), false);
  // A different per-period amount is a real contract difference.
  assert.equal(spendingLimitsMatch([{
    ...limit,
    quantityConstraints: { ...limit.quantityConstraints, maxPerPeriod: limit.quantityConstraints.maxPerPeriod + 1n },
  }], target.caps.dailySpendingLimit), false);

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

/**
 * The finalized seed-145 strategy-two allocation policy account bytes
 * (8Nd646MD6H6hQrXZuP6utG5QZjRZ44GrRmdZJGShmhnh, owner SMRTzfY6DfH5ik3TKiyLF
 * fXexV8uSG3d2UksSCYdunG, landing slot 446095740). This is the account whose
 * readback refused before the program-assigned spending-limit start was
 * normalized, so it pins the whole compare against real chain state: the row
 * must pass as landed, and a single mutated constraint byte must still fail.
 */
const LIVE_SEED_145_POLICY = "8Nd646MD6H6hQrXZuP6utG5QZjRZ44GrRmdZJGShmhnh";
const LIVE_SEED_145_OWNER = "SMRTzfY6DfH5ik3TKiyLFfXexV8uSG3d2UksSCYdunG";
const LIVE_SEED_145_DATA_SHA256 = "e89bdc6e5b09922c0c32078d579c3263020e6796d8415b804dd1b5ed263a7b55";
/** The landing block time the Squads program stamped into the window start. */
const LIVE_SEED_145_START = 1789110293;
const LIVE_SEED_145_ACCOUNT_HEX = "de8707a3ebb121444379e0e134dbd2b80aa2aa8199739496b43794a41cdbb2a2217411e6d8efcae59100000000000000ff00000000000000000000000000000000010000004a9f9f54e872666d7c3d8dd6e06de67ec5f8953c0c6461fd593553d48d48fb4007010000000000030002000000d69aa5fd31edd7807c9b6602a74e09c229e70bcc5c62617458ca337793d1f38702000000000001000000b5533e4a117b87a7334998bc752855f8b58d005289d39352794e393e237eb6e90001000100000076c53dfd30a712428451510f7a2b9190a4a63ad618c390aee2edd817c4929c8e000500000000000000000000000509000000a4aff629b28c2303000009000000000000000300000000000000000209000000000000000300e1f50500000000052700000000000000030010a5d4e80000000511000000000000000506000000013900000001000db4588c00282e3906bc90f790d4706811e8cc24b29cca991b4d70db3aa58d390b000000000001000000068513c48cf2381aa83b47182c9841a91f0f337d03cf1f887c1c308c1653c2fa00020001000000f5a4eb5618d09fd728fb93f7e0cd62d27104d8e3cad915c47d5b5658117d6f5700030001000000b5533e4a117b87a7334998bc752855f8b58d005289d39352794e393e237eb6e900080001000000c6fa7af3bedbad3a3d65f36aabc97431b1bbe4c2d2f6e0e47ca60203452f5d61000b0001000000c6d7b1fbdbf758db76f540a3aa9a48779b269a36fa9b5c4d9cef4155a3c575af000c000100000006ddf6e1d765a193d9cbe146ceeb79ac1cb485ed5f5b37913a8cf5857eff00a9000d0001000000d69aa5fd31edd7807c9b6602a74e09c229e70bcc5c62617458ca337793d1f387000e00010000004379e0e134dbd2b80aa2aa8199739496b43794a41cdbb2a2217411e6d8efcae5000f0001000000068513c48cf2381aa83b47182c9841a91f0f337d03cf1f887c1c308c1653c2fa00100001000000c3c8bb6e0945061658ed1241d10f3625b81bf0af8a1fca6990709eecc9fe44050011000100000076c53dfd30a712428451510f7a2b9190a4a63ad618c390aee2edd817c4929c8e000500000000000000000000000508000000f65239e283defdf90008000000000000000300000000000000000208000000000000000300e1f50500000000053300000000000000030010a5d4e800000005100000000000000005130000000108000000f223c68952e1f2b601390000000100000001000000c6fa7af3bedbad3a3d65f36aabc97431b1bbe4c2d2f6e0e47ca60203452f5d6115a8a36a0000000000010000a3e1110000000000000000000000000000a3e1110000000015a8a36a0000000015a8a36a0000000000971a24624a17f59e77b372ad6dcac49bc6c9b0fc0637028f5270205fd4977c080000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000";


const POLICY_DECODER = (squadsGenerated as unknown as {
  Policy: {
    fromAccountInfo(account: {
      data: Buffer;
      executable: boolean;
      lamports: number;
      owner: PublicKey;
      rentEpoch: number;
    }): readonly [Readonly<{ policyState: Readonly<{ fields?: readonly unknown[] }> }>, number];
  };
}).Policy;

function decodeLimit(accountData: Buffer): {
  quantityConstraints: { maxPerPeriod: { toString(): string } };
  usage: { remainingInPeriod: { toString(): string }; lastReset: { toString(): string } };
} {
  const [decoded] = POLICY_DECODER.fromAccountInfo({
    data: accountData, executable: false, lamports: 1,
    owner: new PublicKey(LIVE_SEED_145_OWNER), rentEpoch: 0,
  });
  const body = decoded.policyState.fields?.[0] as {
    spendingLimits: readonly {
      quantityConstraints: { maxPerPeriod: { toString(): string } };
      usage: { remainingInPeriod: { toString(): string }; lastReset: { toString(): string } };
    }[];
  };
  return body.spendingLimits[0]!;
}

test("the finalized seed-145 strategy-two row passes with its program-assigned limit start",
  { timeout: 60_000 },
  async () => {
  const identity = {
    config: "DCpR24Eb6xCWxDyaZvCBTkadkxCB2vkqJN1EfYNWtLxY",
    delegatedSigner: "62JLkPeE4oG65LRB3W3m52RVicmYq3xFHdv7TecCsPj5",
  } as StrategyTwoIdentity;
  const target = await rwaMultiplyStrategyTwoTarget(identity, 144n);
  const expected = {
    operation: "allocation",
    seed: "145",
    policy: LIVE_SEED_145_POLICY,
    constraintIndex: 0 as const,
    constraintIndices: [0, 1],
    createInstruction: { programId: identity.config, accounts: [], dataBase64: "" },
  } as const;
  const data = Buffer.from(LIVE_SEED_145_ACCOUNT_HEX, "hex");
  assert.equal(createHash("sha256").update(data).digest("hex"), LIVE_SEED_145_DATA_SHA256);
  const info = { owner: new PublicKey(LIVE_SEED_145_OWNER), data };
  const rowFor = async (options: InstalledReadbackOptions) => installedPolicyRow({
    target, expected, index: 0, info,
    expectations: await buildInstalledRowExpectations(target, options),
  });

  // With the landing wire known, the program-assigned window start is pinned
  // to that block time within the program's clock jitter; without it, the
  // start is only bounded below by the anchor observation floor and above by
  // the near-future ceiling.
  assert.equal((await rowFor({})).pass, true);
  assert.equal((await rowFor({ landingBlockTime: LIVE_SEED_145_START })).pass, true);
  assert.equal((await rowFor({ landingBlockTime: LIVE_SEED_145_START + 2 })).pass, true);
  assert.equal((await rowFor({ landingBlockTime: LIVE_SEED_145_START + 10 })).pass, false);
  assert.equal((await rowFor({ earliestStart: LIVE_SEED_145_START - 1000 })).pass, true);
  assert.equal((await rowFor({ earliestStart: LIVE_SEED_145_START + 5 })).pass, false);
  // A freshly installed limit must be full and never reset.
  assert.equal((await rowFor({ landingBlockTime: LIVE_SEED_145_START, requireUntouchedUsage: true })).pass, true);

  // One flipped byte inside the per-instruction amount cap the constraints
  // compare must still refuse the policy: pinning the window start never
  // loosens the byte-exact constraint check.
  const capBytes = Buffer.alloc(8);
  capBytes.writeBigUInt64LE(100_000_000n);
  const capOffset = data.indexOf(capBytes);
  assert.ok(capOffset > 0, "fixture lacks the encoded amount cap");
  const mutated = Buffer.from(data);
  mutated[capOffset] ^= 0x01;
  const mutatedRow = installedPolicyRow({
    target, expected, index: 0,
    info: { owner: new PublicKey(LIVE_SEED_145_OWNER), data: mutated },
    expectations: await buildInstalledRowExpectations(target, { landingBlockTime: LIVE_SEED_145_START }),
  });
  assert.equal(mutatedRow.pass, false);
  assert.equal(mutatedRow.reason, "inexact policy payload");

  // A limit the account has already drawn on is a legitimate state for the
  // routine readback of an installed seed, but not for a fresh install.
  const budgetBytes = Buffer.alloc(8);
  budgetBytes.writeBigUInt64LE(STRATEGY_TWO_DAILY_SPENDING_LIMIT_RAW);
  const budgetOffset = data.lastIndexOf(budgetBytes);
  assert.ok(budgetOffset > data.indexOf(budgetBytes), "fixture lacks a distinct usage entry");
  const charged = Buffer.from(data);
  charged[budgetOffset] ^= 0x01;
  const chargedLimit = decodeLimit(charged);
  assert.equal(chargedLimit.quantityConstraints.maxPerPeriod.toString(),
    STRATEGY_TWO_DAILY_SPENDING_LIMIT_RAW.toString());
  assert.notEqual(chargedLimit.usage.remainingInPeriod.toString(),
    STRATEGY_TWO_DAILY_SPENDING_LIMIT_RAW.toString());
  const chargedRowFor = async (options: InstalledReadbackOptions) => installedPolicyRow({
    target, expected, index: 0,
    info: { owner: new PublicKey(LIVE_SEED_145_OWNER), data: charged },
    expectations: await buildInstalledRowExpectations(target, options),
  });
  assert.equal((await chargedRowFor({ landingBlockTime: LIVE_SEED_145_START })).pass, true);
  assert.equal((await chargedRowFor({
    landingBlockTime: LIVE_SEED_145_START, requireUntouchedUsage: true,
  })).pass, false);
  assert.equal((await chargedRowFor({
    landingBlockTime: LIVE_SEED_145_START, requireUntouchedUsage: true,
  })).reason, "inexact policy payload");
});
