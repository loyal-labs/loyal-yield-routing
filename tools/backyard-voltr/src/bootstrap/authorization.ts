import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

import {
  BOOTSTRAP_EXECUTION_AUTHORIZATION_PATH,
  COMPATIBILITY_APPROVAL_PATH,
  COMPATIBILITY_ARTIFACT_PATH,
  bootstrapExecutionSourceBinding,
  type BootstrapExecutionAuthorization,
  type BootstrapExecutionOperation,
} from "../domain/bootstrap-execution-authorization.js";
import {
  PARTNER_FOUR_MARKET_ROUTE,
  PARTNER_ROUTE,
  fourMarketRouteSpecSha256,
  partnerBuilderRoute,
  partnerStrategyGraphSha256,
  partnerStrategyIdentity,
  routeSpecSha256,
} from "../domain/route-spec.js";
import { strategyAssetAtaAuthorizationFacts } from "./strategy-asset.js";
import { strategyBootstrapAuthorizationFacts } from "./strategy.js";

const REPOSITORY_ROOT = resolve(dirname(fileURLToPath(import.meta.url)), "../../../..");
const AUTHORIZED_STRATEGIES = ["onre", "prime", "maple"] as const;
const MAX_STRATEGY_BOOTSTRAP_LAMPORTS = "75000000";
const MAX_STRATEGY_ASSET_ATA_LAMPORTS = "3000000";

function sha256(bytes: Uint8Array): string {
  return createHash("sha256").update(bytes).digest("hex");
}

function compatibilityBinding() {
  return {
    path: COMPATIBILITY_APPROVAL_PATH,
    fileSha256: sha256(readFileSync(resolve(REPOSITORY_ROOT, COMPATIBILITY_APPROVAL_PATH))),
    artifactPath: COMPATIBILITY_ARTIFACT_PATH,
    artifactFileSha256: sha256(readFileSync(resolve(REPOSITORY_ROOT, COMPATIBILITY_ARTIFACT_PATH))),
  } as const;
}

function commonOperation(strategyId: typeof AUTHORIZED_STRATEGIES[number]) {
  const identity = partnerStrategyIdentity(strategyId);
  const route = partnerBuilderRoute(strategyId);
  return {
    strategyId,
    reserve: identity.reserve,
    vault: route.vault,
    setupAdmin: route.setupAdmin,
    strategyAuth: identity.voltr.strategyAuth,
    strategyInitReceipt: identity.voltr.strategyInitReceipt,
    strategyAssetAta: identity.voltr.strategyAssetAta,
    fourMarketRouteSpecSha256: fourMarketRouteSpecSha256(),
    strategyGraphSha256: partnerStrategyGraphSha256(strategyId),
    builderRouteSpecSha256: routeSpecSha256(route),
  } as const;
}

export async function buildBootstrapExecutionAuthorization(
  lifetimeSeconds = 4 * 60 * 60,
): Promise<BootstrapExecutionAuthorization> {
  if (!Number.isSafeInteger(lifetimeSeconds) || lifetimeSeconds <= 0 || lifetimeSeconds > 24 * 60 * 60) {
    throw new Error("bootstrap authorization lifetime must be an integer in 1..86400 seconds");
  }
  const strategyFacts = await Promise.all(
    AUTHORIZED_STRATEGIES.map((strategyId) => strategyBootstrapAuthorizationFacts(strategyId)),
  );
  const ataFacts = await Promise.all(
    AUTHORIZED_STRATEGIES.map((strategyId) => strategyAssetAtaAuthorizationFacts(strategyId)),
  );
  const strategyOperations: BootstrapExecutionOperation[] = AUTHORIZED_STRATEGIES.map((strategyId, index) => ({
    operation: "initialize-strategy",
    ...commonOperation(strategyId),
    instructionDataSha256: {
      setManager: strategyFacts[index]!.instructionDataSha256[0],
      initializeStrategy: strategyFacts[index]!.instructionDataSha256[1],
      restoreManager: strategyFacts[index]!.instructionDataSha256[2],
    },
    maxTotalLamports: MAX_STRATEGY_BOOTSTRAP_LAMPORTS,
  }));
  const ataOperations: BootstrapExecutionOperation[] = AUTHORIZED_STRATEGIES.map((strategyId, index) => ({
    operation: "initialize-strategy-asset-ata",
    ...commonOperation(strategyId),
    instructionDataSha256: { createAta: ataFacts[index]!.instructionDataSha256 },
    maxTotalLamports: MAX_STRATEGY_ASSET_ATA_LAMPORTS,
  }));
  const now = Math.floor(Date.now() / 1_000);
  return {
    schemaVersion: 1,
    evidenceType: "backyard-voltr-four-market-bootstrap-execution-authorization",
    approvalId: `operator-approved-bootstrap-v1-${now}`,
    expiresAtUnix: String(now + lifetimeSeconds),
    routeId: PARTNER_FOUR_MARKET_ROUTE.id,
    cluster: PARTNER_ROUTE.cluster,
    genesisHash: PARTNER_ROUTE.genesisHash,
    compatibilityApproval: compatibilityBinding(),
    sourceBinding: bootstrapExecutionSourceBinding(),
    operations: [...strategyOperations, ...ataOperations],
  };
}

export const bootstrapExecutionAuthorizationPath = BOOTSTRAP_EXECUTION_AUTHORIZATION_PATH;
