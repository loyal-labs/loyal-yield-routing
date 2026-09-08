import { chmodSync, readFileSync, writeFileSync } from "node:fs";

import { type CustomPolicySeeds } from "../domain/custom-policy-target.js";
import { deriveStrategyTwoPolicySeeds } from "../domain/rwa-multiply-strategy2-route-spec.js";

/** The one-shot repair PolicyCreate must have consumed this counter value. */
export const STRATEGY_TWO_FIRST_POLICY_SEED_BEFORE = 140n;
export const STRATEGY_TWO_REPAIR_POLICY_ADDRESS = "7vqKymJ4RcP9TUR9jT6G2ruuRp3j6rVhTzoYJWYTe2dR";

type StrategyTwoSeedJournalIdentity = Readonly<{
  settingsAddress: string;
  genesisHash: string;
  strategyTwoConfig: string;
  delegatedSigner: string;
  repairPolicy: string;
  repairPolicyDataSha256: string;
  observationSlot: number;
}>;

export type StrategyTwoSeedExpectation = Readonly<{
  policySeedBefore: bigint;
  expectedSeeds: readonly bigint[];
}> & StrategyTwoSeedJournalIdentity;

export type StrategyTwoSeedAnchorObservation = Readonly<{
  settingsAddress: string;
  genesisHash: string;
  strategyTwoConfig: string;
  delegatedSigner: string;
  repairPolicy: string;
  repairPolicyPresent: boolean;
  repairPolicyDataSha256: string | null;
  observationSlot: number;
  policySeedBefore: bigint;
}>;

function invariant(value: unknown, message: string): asserts value {
  if (!value) throw new Error(message);
}

export function strategyTwoSeedStrings(
  seeds: Readonly<CustomPolicySeeds>,
): string[] {
  return [seeds.allocation, seeds.navRefresh, seeds.stageWithdrawal, seeds.withdraw].map(String);
}

export function parseStrategyTwoSeed(value: unknown, label: string): bigint {
  const text = String(value ?? "");
  invariant(/^\d+$/.test(text), `${label} must be a non-negative decimal integer`);
  return BigInt(text);
}

function parseObservationSlot(value: unknown): number {
  invariant(typeof value === "number" && Number.isSafeInteger(value) && value > 0,
    "strategy-two seed journal observationSlot must be a positive safe integer");
  return value;
}

function parseJournalIdentity(value: {
  settingsAddress?: unknown;
  genesisHash?: unknown;
  strategyTwoConfig?: unknown;
  delegatedSigner?: unknown;
  repairPolicy?: unknown;
  repairPolicyDataSha256?: unknown;
  observationSlot?: unknown;
}): StrategyTwoSeedJournalIdentity {
  const identity = {
    settingsAddress: String(value.settingsAddress ?? ""),
    genesisHash: String(value.genesisHash ?? ""),
    strategyTwoConfig: String(value.strategyTwoConfig ?? ""),
    delegatedSigner: String(value.delegatedSigner ?? ""),
    repairPolicy: String(value.repairPolicy ?? ""),
    repairPolicyDataSha256: String(value.repairPolicyDataSha256 ?? ""),
    observationSlot: parseObservationSlot(value.observationSlot),
  };
  invariant(identity.settingsAddress.length > 0
    && identity.genesisHash.length > 0
    && identity.strategyTwoConfig.length > 0
    && identity.delegatedSigner.length > 0
    && identity.repairPolicy === STRATEGY_TWO_REPAIR_POLICY_ADDRESS,
  "strategy-two seed journal anchor identities are incomplete or not pinned to the repair PDA");
  invariant(/^[0-9a-f]{64}$/.test(identity.repairPolicyDataSha256),
    "strategy-two seed journal repair policy data hash is invalid");
  return identity;
}

/** Read and validate the durable seed expectation shared by every invocation. */
export function readStrategyTwoSeedExpectation(path: string): StrategyTwoSeedExpectation {
  const value = JSON.parse(readFileSync(path, "utf8")) as {
    schema?: unknown;
    policySeedBefore?: unknown;
    expectedSeeds?: unknown;
    settingsAddress?: unknown;
    genesisHash?: unknown;
    strategyTwoConfig?: unknown;
    delegatedSigner?: unknown;
    repairPolicy?: unknown;
    repairPolicyDataSha256?: unknown;
    observationSlot?: unknown;
  };
  invariant(value.schema === "loyal-rwa-multiply-strategy-two-seed-expectation/v1",
    "strategy-two seed journal schema is not recognized");
  const policySeedBefore = parseStrategyTwoSeed(value.policySeedBefore, "strategy-two seed journal policySeedBefore");
  invariant(policySeedBefore === STRATEGY_TWO_FIRST_POLICY_SEED_BEFORE,
    `strategy-two seed journal is pinned to policySeedBefore ${STRATEGY_TWO_FIRST_POLICY_SEED_BEFORE}`);
  const expected = deriveStrategyTwoPolicySeeds(policySeedBefore);
  invariant(Array.isArray(value.expectedSeeds)
    && JSON.stringify(value.expectedSeeds) === JSON.stringify(strategyTwoSeedStrings(expected)),
  "strategy-two seed journal expected seeds do not derive from policySeedBefore");
  const identity = parseJournalIdentity(value);
  return {
    policySeedBefore,
    expectedSeeds: [expected.allocation, expected.navRefresh, expected.stageWithdrawal, expected.withdraw],
    ...identity,
  };
}

export function writeStrategyTwoSeedExpectation(
  path: string,
  expectation: StrategyTwoSeedExpectation,
): void {
  const expected = deriveStrategyTwoPolicySeeds(expectation.policySeedBefore);
  invariant(expectation.policySeedBefore === STRATEGY_TWO_FIRST_POLICY_SEED_BEFORE,
    `strategy-two seed journal is pinned to policySeedBefore ${STRATEGY_TWO_FIRST_POLICY_SEED_BEFORE}`);
  invariant(JSON.stringify(expectation.expectedSeeds.map(String)) === JSON.stringify(strategyTwoSeedStrings(expected)),
    "strategy-two seed expectation does not derive from policySeedBefore");
  parseJournalIdentity(expectation);
  writeFileSync(path, `${JSON.stringify({
    schema: "loyal-rwa-multiply-strategy-two-seed-expectation/v1",
    policySeedBefore: expectation.policySeedBefore.toString(),
    expectedSeeds: strategyTwoSeedStrings(expected),
    settingsAddress: expectation.settingsAddress,
    genesisHash: expectation.genesisHash,
    strategyTwoConfig: expectation.strategyTwoConfig,
    delegatedSigner: expectation.delegatedSigner,
    repairPolicy: expectation.repairPolicy,
    repairPolicyDataSha256: expectation.repairPolicyDataSha256,
    observationSlot: expectation.observationSlot,
    note: "The four PolicyCreate wires must consume these fresh sequential seeds; never reuse this expectation after the Settings counter advances beyond the set.",
  }, null, 2)}\n`, { flag: "wx", mode: 0o600 });
  chmodSync(path, 0o600);
}

/**
 * Revalidate the durable journal against the current finalized cluster anchor.
 * The current counter may advance through the four replacement creates, but
 * the Settings identity, cluster, rotated identities, repair PDA, and repair
 * account bytes must remain exactly the values observed when the journal was
 * created.
 */
export function assertStrategyTwoSeedJournalAnchor(
  expectation: StrategyTwoSeedExpectation,
  observation: StrategyTwoSeedAnchorObservation,
): void {
  invariant(observation.policySeedBefore >= expectation.policySeedBefore
    && observation.policySeedBefore <= expectation.policySeedBefore + 4n,
  `finalized Settings counter ${observation.policySeedBefore} escaped the journaled strategy-two seed range ${expectation.policySeedBefore + 1n}-${expectation.policySeedBefore + 4n}`);
  invariant(observation.settingsAddress === expectation.settingsAddress,
    "finalized Settings address differs from the strategy-two seed journal");
  invariant(observation.genesisHash === expectation.genesisHash,
    "RPC genesis hash differs from the strategy-two seed journal");
  invariant(observation.strategyTwoConfig === expectation.strategyTwoConfig,
    "strategy-two config identity differs from the seed journal");
  invariant(observation.delegatedSigner === expectation.delegatedSigner,
    "strategy-two delegated signer differs from the seed journal");
  invariant(observation.repairPolicy === expectation.repairPolicy
    && expectation.repairPolicy === STRATEGY_TWO_REPAIR_POLICY_ADDRESS,
  "strategy-two repair policy PDA differs from the seed journal");
  invariant(observation.repairPolicyPresent,
    `one-shot repair policy at ${STRATEGY_TWO_REPAIR_POLICY_ADDRESS} is absent or has the wrong owner; refusing strategy-two invocation`);
  invariant(observation.repairPolicyDataSha256 === expectation.repairPolicyDataSha256,
    "finalized repair policy data hash differs from the strategy-two seed journal");
  invariant(observation.observationSlot >= expectation.observationSlot,
    "finalized strategy-two anchor observation moved behind the seed journal");
  invariant(Number.isSafeInteger(observation.observationSlot) && observation.observationSlot > 0,
    "finalized strategy-two anchor observation slot is invalid");
}

/**
 * The first installer invocation is pinned to the repair boundary. A missing
 * repair account or a counter drift is an abort, not a new seed allocation.
 */
export function assertStrategyTwoFirstInvocation(
  policySeedBefore: bigint,
  repairPolicyPresent: boolean,
): void {
  invariant(policySeedBefore === STRATEGY_TWO_FIRST_POLICY_SEED_BEFORE,
    `first strategy-two install requires finalized Settings counter ${STRATEGY_TWO_FIRST_POLICY_SEED_BEFORE}; observed ${policySeedBefore}`);
  invariant(repairPolicyPresent,
    `one-shot repair policy at seed ${STRATEGY_TWO_FIRST_POLICY_SEED_BEFORE} (${STRATEGY_TWO_REPAIR_POLICY_ADDRESS}) is absent; refusing strategy-two install`);
}
