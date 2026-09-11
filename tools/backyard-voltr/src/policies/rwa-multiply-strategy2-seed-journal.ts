import { createHash } from "node:crypto";
import { chmodSync, readFileSync, writeFileSync } from "node:fs";

import { PublicKey, type Connection } from "@solana/web3.js";

import { type CustomPolicySeeds } from "../domain/custom-policy-target.js";
import { deriveStrategyTwoPolicySeeds } from "../domain/rwa-multiply-strategy2-route-spec.js";
import { customPolicyAddress } from "./rwa-multiply-legacy-retirement.js";

/** The installed basic policy set must have consumed this counter value. */
export const STRATEGY_TWO_FIRST_POLICY_SEED_BEFORE = 144n;

/**
 * The continuity anchor is the last basic policy of the installed set, at the
 * seed the Settings counter stopped on. `docs/evidence/backyard-rwa-basic/
 * policy-install-readback-v1.json` records its finalized address and
 * `accountDataSha256`; the installer compares the live account bytes against
 * that readback instead of the removed one-shot repair policy.
 */
export const STRATEGY_TWO_ANCHOR_POLICY_ADDRESS = customPolicyAddress(STRATEGY_TWO_FIRST_POLICY_SEED_BEFORE);
export const STRATEGY_TWO_ANCHOR_POLICY_DATA_SHA256 =
  "43d09b3cbdd63f1c775f7a660ac75ac76f699b18bd1298ec1bf87d088e6d535a";

type StrategyTwoSeedJournalIdentity = Readonly<{
  settingsAddress: string;
  genesisHash: string;
  strategyTwoConfig: string;
  delegatedSigner: string;
  anchorPolicy: string;
  anchorPolicyDataSha256: string;
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
  anchorPolicy: string;
  anchorPolicyPresent: boolean;
  anchorPolicyDataSha256: string | null;
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
  anchorPolicy?: unknown;
  anchorPolicyDataSha256?: unknown;
  observationSlot?: unknown;
}): StrategyTwoSeedJournalIdentity {
  const identity = {
    settingsAddress: String(value.settingsAddress ?? ""),
    genesisHash: String(value.genesisHash ?? ""),
    strategyTwoConfig: String(value.strategyTwoConfig ?? ""),
    delegatedSigner: String(value.delegatedSigner ?? ""),
    anchorPolicy: String(value.anchorPolicy ?? ""),
    anchorPolicyDataSha256: String(value.anchorPolicyDataSha256 ?? ""),
    observationSlot: parseObservationSlot(value.observationSlot),
  };
  invariant(identity.settingsAddress.length > 0
    && identity.genesisHash.length > 0
    && identity.strategyTwoConfig.length > 0
    && identity.delegatedSigner.length > 0
    && identity.anchorPolicy === STRATEGY_TWO_ANCHOR_POLICY_ADDRESS,
  "strategy-two seed journal anchor identities are incomplete or not pinned to the seed-144 basic policy");
  invariant(identity.anchorPolicyDataSha256 === STRATEGY_TWO_ANCHOR_POLICY_DATA_SHA256,
    "strategy-two seed journal anchor policy data hash is not the finalized seed-144 install readback");
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
    anchorPolicy?: unknown;
    anchorPolicyDataSha256?: unknown;
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
    anchorPolicy: expectation.anchorPolicy,
    anchorPolicyDataSha256: expectation.anchorPolicyDataSha256,
    observationSlot: expectation.observationSlot,
    note: "The four PolicyCreate wires must consume these fresh sequential seeds; never reuse this expectation after the Settings counter advances beyond the set.",
  }, null, 2)}\n`, { flag: "wx", mode: 0o600 });
  chmodSync(path, 0o600);
}

/**
 * Revalidate the durable journal against the current finalized cluster anchor.
 * The current counter may advance through the four replacement creates, but
 * the Settings identity, cluster, rotated identities, seed-144 basic policy
 * anchor, and its install-readback bytes must remain exactly the pinned
 * values.
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
  invariant(observation.anchorPolicy === expectation.anchorPolicy
    && expectation.anchorPolicy === STRATEGY_TWO_ANCHOR_POLICY_ADDRESS,
  "strategy-two anchor policy account differs from the seed journal");
  invariant(observation.anchorPolicyPresent,
    `basic policy anchor at seed ${STRATEGY_TWO_FIRST_POLICY_SEED_BEFORE} (${STRATEGY_TWO_ANCHOR_POLICY_ADDRESS}) is absent or has the wrong owner; refusing strategy-two invocation`);
  invariant(observation.anchorPolicyDataSha256 === expectation.anchorPolicyDataSha256,
    "finalized anchor policy data hash differs from the strategy-two seed journal");
  invariant(observation.anchorPolicyDataSha256 === STRATEGY_TWO_ANCHOR_POLICY_DATA_SHA256,
    "finalized anchor policy bytes differ from the seed-144 install readback");
  invariant(observation.observationSlot >= expectation.observationSlot,
    "finalized strategy-two anchor observation moved behind the seed journal");
  invariant(Number.isSafeInteger(observation.observationSlot) && observation.observationSlot > 0,
    "finalized strategy-two anchor observation slot is invalid");
}

/**
 * The first installer invocation is pinned to the basic policy set boundary. A
 * missing anchor account or a counter drift is an abort, not a new seed
 * allocation.
 */
export function assertStrategyTwoFirstInvocation(
  policySeedBefore: bigint,
  anchorPolicyPresent: boolean,
): void {
  invariant(policySeedBefore === STRATEGY_TWO_FIRST_POLICY_SEED_BEFORE,
    `first strategy-two install requires finalized Settings counter ${STRATEGY_TWO_FIRST_POLICY_SEED_BEFORE}; observed ${policySeedBefore}`);
  invariant(anchorPolicyPresent,
    `basic policy anchor at seed ${STRATEGY_TWO_FIRST_POLICY_SEED_BEFORE} (${STRATEGY_TWO_ANCHOR_POLICY_ADDRESS}) is absent; refusing strategy-two install`);
}

export type StrategyTwoLiveAnchorObservation = Readonly<{
  anchorPolicyPresent: boolean;
  anchorPolicyOwnerMatches: boolean;
  anchorPolicyDataSha256: string | null;
}>;

/**
 * Re-observe the pinned seed-144 anchor on live finalized state so journal
 * consumers can confirm the account bytes still match the install readback.
 * Callers with no connection in scope pass `null` to
 * `assertStrategyTwoAnchorObservation` instead and must say so in their output.
 */
export async function observeStrategyTwoSeedAnchor(
  connection: Connection,
  squadsProgram: string,
): Promise<StrategyTwoLiveAnchorObservation> {
  const info = await connection.getAccountInfo(
    new PublicKey(STRATEGY_TWO_ANCHOR_POLICY_ADDRESS), "finalized");
  return {
    anchorPolicyPresent: info !== null,
    anchorPolicyOwnerMatches: info !== null && info.owner.toBase58() === squadsProgram,
    anchorPolicyDataSha256: info === null
      ? null
      : createHash("sha256").update(info.data).digest("hex"),
  };
}

/**
 * Shared anchor gate. `null` means the caller had no connection available: the
 * gate is skipped and reported as such rather than silently passing.
 */
export function assertStrategyTwoAnchorObservation(
  observed: StrategyTwoLiveAnchorObservation | null,
): Readonly<{ observed: "pass" | "skipped-no-connection" }> {
  if (observed === null) return { observed: "skipped-no-connection" };
  invariant(observed.anchorPolicyPresent && observed.anchorPolicyOwnerMatches,
    `basic policy anchor at seed ${STRATEGY_TWO_FIRST_POLICY_SEED_BEFORE} (${STRATEGY_TWO_ANCHOR_POLICY_ADDRESS}) is absent or has the wrong owner`);
  invariant(observed.anchorPolicyDataSha256 === STRATEGY_TWO_ANCHOR_POLICY_DATA_SHA256,
    "live anchor policy bytes differ from the seed-144 install readback");
  return { observed: "pass" };
}

/**
 * Floor for a program-assigned spending-limit window start when no landing
 * wire is known: the block time of the seed journal's anchor observation slot.
 * No policy derived from that observation can have landed before it. This
 * refuses instead of returning null so a read-only readback can never drop
 * the floor silently and accept an arbitrarily old start.
 */
export async function strategyTwoAnchorObservationFloor(
  connection: Pick<Connection, "getBlockTime">,
  observationSlot: number,
): Promise<number> {
  const blockTime = await connection.getBlockTime(observationSlot);
  invariant(typeof blockTime === "number",
    `strategy-two seed journal anchor observation slot ${observationSlot} has no resolvable block time; refusing to read back the installed policies without a window-start floor`);
  return blockTime;
}
