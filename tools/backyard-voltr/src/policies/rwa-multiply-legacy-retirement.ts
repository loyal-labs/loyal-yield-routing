import { generated as squadsGenerated } from "@loyal-labs/loyal-smart-accounts-core";
import { executeSettingsTransactionSync } from "@loyal-labs/loyal-smart-accounts-core/internal";
import { AccountRole, type Instruction } from "@solana/kit";
import { PublicKey, SystemProgram } from "@solana/web3.js";

import { RWA_MULTIPLY_ROUTE } from "../domain/rwa-multiply-route-spec.js";
import { deriveStrategyTwoPolicySeeds } from "../domain/rwa-multiply-strategy2-route-spec.js";
import { fromWeb3Instruction } from "../integrations/solana-compat.js";

/**
 * The v2 bridge policies (seeds 62-65) are the retirement target once the
 * strategy-two policies derived from the finalized Settings counter verify on
 * chain. Their frozen data
 * hashes are what the worker manifest pinned, so a removal transaction is only
 * built against exactly these bytes.
 */
export const LEGACY_CUSTOM_POLICY_SEEDS = [62n, 63n, 64n, 65n] as const;
export const LEGACY_CUSTOM_POLICY_DATA_SHA256 = [
  "bda72932f474064fa3cd60ce91633acba35b2730e86b82f4352aa96a6738e2f4",
  "bf34a3e9c9c635c79a0d30e096b639a86d52e300ad113c81161e3486832d97ca",
  "ef8c231497fb2620b5930cfe5d329c871f103db6512781eb5487534db8b1291b",
  "84e8f6f881758cff1714ef743603c016024104f9834392c6fba693c3651b719c",
] as const;

/** Strategy-two replacement seeds: allocation, NAV refresh, staging, withdraw. */
export function deriveStrategyTwoReplacementSeeds(policySeedBefore: bigint): readonly bigint[] {
  const seeds = deriveStrategyTwoPolicySeeds(policySeedBefore);
  return [seeds.allocation, seeds.navRefresh, seeds.stageWithdrawal, seeds.withdraw];
}

export function customPolicyAddress(seed: bigint): string {
  const seedBytes = Buffer.alloc(8);
  seedBytes.writeBigUInt64LE(seed);
  return PublicKey.findProgramAddressSync([
    Buffer.from("smart_account"),
    Buffer.from("policy"),
    new PublicKey(RWA_MULTIPLY_ROUTE.squads.settings).toBuffer(),
    seedBytes,
  ], new PublicKey(RWA_MULTIPLY_ROUTE.squads.program))[0].toBase58();
}

export const LEGACY_CUSTOM_POLICY_ADDRESSES = LEGACY_CUSTOM_POLICY_SEEDS.map(customPolicyAddress);

function invariant(value: unknown, message: string): asserts value {
  if (!value) throw new Error(message);
}

/**
 * Strategy-two installation is intentionally allowed to overlap the legacy
 * set. Retirement is a later, separately gated operation, so presence here
 * is an expected cutover state rather than a failure.
 */
export function assertStrategyTwoInstallMayCoexist(
  legacyPolicyAccounts: readonly unknown[],
): void {
  invariant(legacyPolicyAccounts.length === LEGACY_CUSTOM_POLICY_ADDRESSES.length,
    "strategy-two install legacy coexistence read is incomplete");
}

/** The retirement wire is refused until all four derived replacements pass. */
export function assertStrategyTwoReplacementPoliciesFinalized(
  installed: Readonly<{ pass: boolean; rows: readonly Readonly<{ seed: string }>[] }>,
  expectedSeeds: readonly bigint[],
): void {
  const expected = expectedSeeds.map(String);
  invariant(installed.pass && JSON.stringify(installed.rows.map(({ seed }) => seed)) === JSON.stringify(expected),
    `four finalized strategy-two replacement policies are required before legacy retirement; expected seeds ${expected.join(",")}`);
}

/** One Settings instruction removes only the four superseded bridge policies. */
export function buildLegacyCustomPolicyRetirementInstruction(): Instruction {
  const route = RWA_MULTIPLY_ROUTE;
  const policies = LEGACY_CUSTOM_POLICY_ADDRESSES.map((value) => new PublicKey(value));
  const web3Instruction = executeSettingsTransactionSync({
    settingsPda: new PublicKey(route.squads.settings),
    signers: [new PublicKey(route.setupAdmin)],
    actions: policies.map((policy) => ({ __kind: "PolicyRemove" as const, policy })),
    feePayer: new PublicKey(route.setupAdmin),
    programId: new PublicKey(route.squads.program),
    remainingAccounts: policies.map((pubkey) => ({ pubkey, isSigner: false, isWritable: true })),
  });

  invariant(web3Instruction.programId.toBase58() === route.squads.program,
    "legacy policy retirement escaped the Squads program");
  const expectedAccounts = [
    [route.squads.settings, false, true],
    [route.setupAdmin, true, true],
    [SystemProgram.programId.toBase58(), false, false],
    [route.squads.program, false, false],
    [route.setupAdmin, true, false],
    ...LEGACY_CUSTOM_POLICY_ADDRESSES.map((policy) => [policy, false, true] as const),
  ] as const;
  invariant(web3Instruction.keys.length === expectedAccounts.length
    && web3Instruction.keys.every((meta, index) => {
      const expected = expectedAccounts[index]!;
      return meta.pubkey.toBase58() === expected[0]
        && meta.isSigner === expected[1]
        && meta.isWritable === expected[2];
    }), "legacy policy retirement account graph drifted");

  const SyncSettingsArgs = (squadsGenerated as unknown as {
    syncSettingsTransactionArgsBeet: {
      deserialize(data: Buffer): readonly [{
        numSigners: number;
        actions: readonly Readonly<{ __kind: string; policy?: PublicKey }>[];
        memo: string | null;
      }, number];
    };
  }).syncSettingsTransactionArgsBeet;
  const [decoded] = SyncSettingsArgs.deserialize(web3Instruction.data.subarray(8));
  invariant(decoded.numSigners === 1
    && decoded.memo === null
    && decoded.actions.length === LEGACY_CUSTOM_POLICY_ADDRESSES.length
    && decoded.actions.every((action, index) => action.__kind === "PolicyRemove"
      && action.policy?.toBase58() === LEGACY_CUSTOM_POLICY_ADDRESSES[index]),
  "legacy policy retirement action list drifted");

  const instruction = fromWeb3Instruction(web3Instruction);
  invariant(instruction.accounts?.[0]?.role === AccountRole.WRITABLE
    && instruction.accounts?.[1]?.role === AccountRole.WRITABLE_SIGNER
    && instruction.accounts?.[4]?.role === AccountRole.READONLY_SIGNER
    && instruction.accounts.slice(5).every(({ role }) => role === AccountRole.WRITABLE),
  "legacy policy retirement signer or writable roles drifted");
  return instruction;
}
