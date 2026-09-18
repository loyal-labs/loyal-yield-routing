import {
  assertStrategyTwoAnchorObservation,
  readStrategyTwoSeedExpectation,
  STRATEGY_TWO_ANCHOR_POLICY_ADDRESS,
  STRATEGY_TWO_ANCHOR_POLICY_DATA_SHA256,
  STRATEGY_TWO_FIRST_POLICY_SEED_BEFORE,
  type StrategyTwoLiveAnchorObservation,
} from "./rwa-multiply-strategy2-seed-journal.js";
import {
  deriveStrategyTwoVoltrAccounts,
  rwaMultiplyStrategyTwoTarget,
  type StrategyTwoIdentity,
} from "../domain/rwa-multiply-strategy2-route-spec.js";
import { customPolicyAddress } from "./rwa-multiply-legacy-retirement.js";

export type StrategyTwoWorkerPolicyConfig = Readonly<{
  schema: "loyal-rwa-multiply-strategy-two-worker-config/v1";
  source: Readonly<{
    seedJournal: string;
    policySeedBefore: string;
  }>;
  identities: Readonly<{
    strategyConfig: string;
    strategyAuth: string;
    strategyInitReceipt: string;
    strategyAssetAta: string;
    reportTicket: string;
    delegatedExecutor: string;
  }>;
  policies: readonly Readonly<{
    operation: "allocation" | "nav-refresh" | "stage-withdrawal" | "withdraw";
    seed: string;
    policy: string;
  }>[];
  legacyGate: Readonly<{
    policySeeds: readonly string[];
    requirement: "absent-before-worker-start";
  }>;
  anchorGate: Readonly<{
    seed: string;
    policy: string;
    dataSha256: string;
    observed: "pass" | "skipped-no-connection";
  }>;
}>;

/**
 * Render the strategy-two worker bindings from the durable seed journal. The
 * worker handoff never derives a seed from a live counter or a source-code
 * literal; changing the journal changes every generated policy binding.
 *
 * Callers with an RPC connection in scope should pass the observation from
 * `observeStrategyTwoSeedAnchor` so the pinned seed-144 anchor is re-checked
 * against the finalized install readback; passing `null` (the default) skips
 * that re-observation and the rendered `anchorGate` says so explicitly.
 */
export async function buildStrategyTwoWorkerPolicyConfig(
  seedJournal: string,
  identity: StrategyTwoIdentity,
  liveAnchor: StrategyTwoLiveAnchorObservation | null = null,
): Promise<StrategyTwoWorkerPolicyConfig> {
  const expectation = readStrategyTwoSeedExpectation(seedJournal);
  const target = await rwaMultiplyStrategyTwoTarget(identity, expectation.policySeedBefore);
  const accounts = await deriveStrategyTwoVoltrAccounts(identity.config);
  const operations = ["allocation", "nav-refresh", "stage-withdrawal", "withdraw"] as const;
  const seeds = [target.seeds.allocation, target.seeds.navRefresh, target.seeds.stageWithdrawal, target.seeds.withdraw];
  return {
    schema: "loyal-rwa-multiply-strategy-two-worker-config/v1",
    source: {
      seedJournal,
      policySeedBefore: expectation.policySeedBefore.toString(),
    },
    identities: {
      strategyConfig: identity.config,
      strategyAuth: accounts.strategyAuth,
      strategyInitReceipt: accounts.strategyInitReceipt,
      strategyAssetAta: accounts.strategyAssetAta,
      reportTicket: accounts.reportTicket,
      delegatedExecutor: identity.delegatedSigner,
    },
    policies: operations.map((operation, index) => ({
      operation,
      seed: seeds[index]!.toString(),
      policy: customPolicyAddress(seeds[index]!),
    })),
    legacyGate: {
      policySeeds: [62n, 63n, 64n, 65n].map(String),
      requirement: "absent-before-worker-start",
    },
    anchorGate: {
      seed: STRATEGY_TWO_FIRST_POLICY_SEED_BEFORE.toString(),
      policy: STRATEGY_TWO_ANCHOR_POLICY_ADDRESS,
      dataSha256: STRATEGY_TWO_ANCHOR_POLICY_DATA_SHA256,
      ...assertStrategyTwoAnchorObservation(liveAnchor),
    },
  };
}
