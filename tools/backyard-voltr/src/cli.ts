import { createHash } from "node:crypto";
import { readFileSync, writeFileSync } from "node:fs";
import { resolve } from "node:path";

import { PARTNER_FOUR_MARKET_ROUTE, PARTNER_ROUTE, fourMarketRouteSpecSha256, routeSpecSha256, type PartnerStrategyId } from "./domain/route-spec.js";
import {
  executeInitializeAndAdaptor,
  simulateAddAdaptor,
  simulateInitializeAndAdaptorDiagnostic,
  simulateInitializeVault,
  summarizeInitializeAndAdaptorApproval,
} from "./bootstrap/commands.js";
import {
  executeStrategyBootstrap,
  simulateStrategyBootstrap,
} from "./bootstrap/strategy.js";
import {
  executeStrategyAssetAta,
  simulateStrategyAssetAta,
} from "./bootstrap/strategy-asset.js";
import { buildBootstrapExecutionAuthorization } from "./bootstrap/authorization.js";
import {
  finalizedSnapshots,
  loadDeploymentIdentities,
  loadMainReserveGraph,
} from "./integrations/solana-compat.js";
import { deriveVoltrAccounts } from "./integrations/voltr.js";
import {
  verifyAdaptorReceipt,
  verifyDeploymentIdentities,
  verifyStrategyBootstrap,
  verifyVaultCurrentState,
} from "./verify/current.js";
import {
  compileRuntimePolicyArtifact,
  verifyRuntimePolicyArtifact,
} from "./policies/compiler.js";
import {
  executeRuntimePolicyInstall,
  simulateRuntimePolicyInstall,
  verifyExistingRuntimePolicies,
  type RuntimePolicyOperation,
} from "./policies/commands.js";
import { buildPolicyCatalogAuthorization } from "./policies/authorization.js";
import { verifyPartnerStructure } from "./verify/structure.js";
import { verifyFinalizedLifecycle } from "./verify/finalized.js";
import { verifyPrecreatedSquadsIsolation } from "./verify/squads.js";
import {
  buildCompatibilityApproval,
  verifyFourMarketCompatibility,
} from "./verify/compatibility.js";
import {
  executeManagerOperation,
  reconcileConfirmedManagerOperation,
  simulateManagerOperation,
  type ManagerRestorationBridgeInput,
  type ManagerOperation,
} from "./runtime/manager.js";
import {
  executePostDeadlineWithdrawClaim,
  executeUserDeposit,
  executeWithdrawRequest,
  simulatePostDeadlineWithdrawClaim,
  simulateInstantWithdrawRejection,
  simulatePrematureWithdrawClaim,
  simulateUserDeposit,
  simulateWithdrawRequest,
} from "./runtime/commands.js";
import { loadFourMarketRestorationSources, scanWithdrawalDemand } from "./runtime/withdrawal-scanner.js";
import { loadFourMarketProtectedState } from "./runtime/protected-state.js";
import { reconcileConfirmedFinalConservation } from "./runtime/final-reconciliation.js";
import { produceConfirmedNegativeMutationArtifact } from "./runtime/negative-mutations-mainnet.js";
import { parseEarnAdapterProducerInput, produceEarnAdapterEvidence } from "./runtime/earn-adapter.js";
import { assembleRestorationEvidenceFromFiles } from "./runtime/restoration-evidence.js";
import {
  parseFourMarketPositionEvidenceFile,
  parseWithdrawalRestorationScanFile,
  planWithdrawalRestoration,
  restorationPlanAsOutboxInput,
  verifyWithdrawalRestorationPlanner,
} from "./runtime/withdrawal-restoration.js";
import { buildFourMarketManifestFromInputsFile, verifyFourMarketLifecycle } from "./verify/four-market.js";
import { verifyBackyardTestingHandoff } from "./verify/integration-handoff.js";
import {
  executeVaultConfigActivation,
  simulateVaultConfigActivation,
} from "./activation/config.js";

function json(value: unknown): string {
  return `${JSON.stringify(value, (_key, entry) => {
    if (typeof entry === "bigint") return entry.toString();
    if (entry instanceof Uint8Array) return Buffer.from(entry).toString("base64");
    return entry;
  }, 2)}\n`;
}

function valueAfter(flag: string): string | null {
  const index = process.argv.indexOf(flag);
  return index < 0 ? null : process.argv[index + 1] ?? null;
}

function runtimePolicyOperation(): RuntimePolicyOperation {
  const operation = valueAfter("--operation");
  if (operation !== "deposit" && operation !== "withdraw") {
    throw new Error("policy install requires --operation deposit|withdraw");
  }
  return operation;
}

function managerOperation(): ManagerOperation {
  const operation = valueAfter("--operation");
  if (operation !== "deposit" && operation !== "withdraw") {
    throw new Error("manager operation requires --operation deposit|withdraw");
  }
  return operation;
}

function managerStrategyId(): PartnerStrategyId {
  return strategyId();
}

function managerAmount(): bigint {
  const value = valueAfter("--amount-raw");
  if (value === null) return PARTNER_ROUTE.asset.proofAmountRaw;
  try {
    return BigInt(value);
  } catch {
    throw new Error("manager operation --amount-raw must be an integer");
  }
}

function strategyId(): PartnerStrategyId {
  const value = valueAfter("--strategy-id");
  if (value !== "main" && value !== "onre" && value !== "prime" && value !== "maple") {
    throw new Error("--strategy-id must be main|onre|prime|maple");
  }
  return value;
}

function bigintFlag(flag: string): bigint | undefined {
  const value = valueAfter(flag);
  if (value === null) return undefined;
  try {
    return BigInt(value);
  } catch {
    throw new Error(`${flag} must be an integer`);
  }
}

function positiveIntegerFlag(flag: string): number | undefined {
  const value = valueAfter(flag);
  if (value === null) return undefined;
  const parsed = Number(value);
  if (!Number.isSafeInteger(parsed) || parsed <= 0) throw new Error(`${flag} must be a positive safe integer`);
  return parsed;
}

function nonNegativeIntegerFlag(flag: string): number | undefined {
  const value = valueAfter(flag);
  if (value === null) return undefined;
  const parsed = Number(value);
  if (!Number.isSafeInteger(parsed) || parsed < 0) throw new Error(`${flag} must be a non-negative safe integer`);
  return parsed;
}

function requiredSha256Flag(flag: string): string {
  const value = valueAfter(flag);
  if (value === null || !/^[0-9a-f]{64}$/.test(value)) throw new Error(`${flag} requires a lowercase SHA-256 digest`);
  return value;
}

function managerRestorationBridgeInput(): ManagerRestorationBridgeInput | null {
  const originId = valueAfter("--restoration-origin-id");
  const requiredFlags = [
    "--restoration-generation",
    "--restoration-leg-id",
    "--restoration-owner",
    "--restoration-protected-address-set-sha256",
    "--restoration-protected-prestate-sha256",
    "--restoration-protected-context-slot",
    "--restoration-evidence-directory",
  ] as const;
  const supplied = requiredFlags.filter((flag) => valueAfter(flag) !== null);
  if (originId === null && supplied.length === 0) return null;
  if (originId === null || supplied.length !== requiredFlags.length) throw new Error("restoration manager execution requires the complete origin/generation/leg/owner/protected-checkpoint/evidence-directory flag set");
  const generation = positiveIntegerFlag("--restoration-generation")!;
  const protectedContextSlot = positiveIntegerFlag("--restoration-protected-context-slot")!;
  const leaseSecondsRaw = valueAfter("--restoration-lease-seconds");
  const leaseSeconds = leaseSecondsRaw === null ? 600 : positiveIntegerFlag("--restoration-lease-seconds")!;
  return {
    originId: requiredSha256Flag("--restoration-origin-id"),
    generation,
    legId: requiredSha256Flag("--restoration-leg-id"),
    owner: valueAfter("--restoration-owner")!,
    leaseSeconds,
    protectedAddressSetSha256: requiredSha256Flag("--restoration-protected-address-set-sha256"),
    protectedPrestateSha256: requiredSha256Flag("--restoration-protected-prestate-sha256"),
    protectedContextSlot,
    evidenceDirectory: valueAfter("--restoration-evidence-directory")!,
    binaryPath: valueAfter("--restoration-bridge-bin"),
  };
}

async function verifyCurrent() {
  const rpcUrl = process.env.SOLANA_RPC_URL;
  if (!rpcUrl) throw new Error("SOLANA_RPC_URL is required");
  const accounts = await deriveVoltrAccounts(PARTNER_ROUTE);
  const reserve = await loadMainReserveGraph(rpcUrl, PARTNER_ROUTE, accounts.strategyAuth);
  const addresses = [
    PARTNER_ROUTE.vault,
    accounts.lpMint,
    accounts.idleAta,
    PARTNER_ROUTE.asset.mint,
    accounts.adaptorAddReceipt,
    accounts.strategyInitReceipt,
    reserve.graph.userMetadata,
    reserve.graph.obligation,
    reserve.graph.obligationFarm,
  ];
  const state = await finalizedSnapshots(rpcUrl, addresses, reserve.contextSlot);
  const deployments = await loadDeploymentIdentities(rpcUrl, PARTNER_ROUTE, state.contextSlot);
  const vault = verifyVaultCurrentState({
    route: PARTNER_ROUTE,
    accounts,
    vault: state.accounts[0] ?? null,
    lpMint: state.accounts[1] ?? null,
    idleAta: state.accounts[2] ?? null,
    assetMint: state.accounts[3] ?? null,
  });
  const adaptor = verifyAdaptorReceipt(PARTNER_ROUTE, accounts.adaptorAddReceipt, state.accounts[4] ?? null);
  const strategy = verifyStrategyBootstrap({
    route: PARTNER_ROUTE,
    accounts,
    graph: reserve.graph,
    strategyReceipt: state.accounts[5] ?? null,
    userMetadata: state.accounts[6] ?? null,
    obligation: state.accounts[7] ?? null,
    obligationFarm: state.accounts[8] ?? null,
  });
  const deploymentGates = verifyDeploymentIdentities(PARTNER_ROUTE, deployments.identities);
  const failedGateCount = vault.failedGateCount
    + adaptor.filter(({ pass }) => !pass).length
    + strategy.filter(({ pass }) => !pass).length
    + deploymentGates.filter(({ pass }) => !pass).length;
  return {
    verdict: failedGateCount === 0 ? "PARTNER_CURRENT_PASS" : "PARTNER_CURRENT_FAIL",
    broadcast: false,
    contextSlot: Math.max(state.contextSlot, deployments.contextSlot),
    routeSpecSha256: routeSpecSha256(),
    failedGateCount,
    vault,
    adaptor,
    strategy,
    deployments: { observed: deployments.identities, gates: deploymentGates },
  };
}

async function main() {
  const [group, operation] = process.argv.slice(2).filter((value) => !value.startsWith("--"));
  let result: unknown;
  if (group === "bootstrap" && operation === "simulate-initialize") {
    result = await simulateInitializeVault();
  } else if (group === "bootstrap" && operation === "approval-init-adaptor") {
    result = await summarizeInitializeAndAdaptorApproval();
  } else if (group === "bootstrap" && operation === "simulate-init-adaptor") {
    result = await simulateInitializeAndAdaptorDiagnostic();
  } else if (group === "bootstrap" && operation === "simulate-add-adaptor") {
    result = await simulateAddAdaptor();
  } else if (group === "bootstrap" && operation === "execute-init-adaptor") {
    result = await executeInitializeAndAdaptor({
      confirmVault: valueAfter("--confirm-vault"),
      confirmRouteSpecSha256: valueAfter("--confirm-route-spec-sha256"),
      confirmInitializeDataSha256: valueAfter("--confirm-initialize-data-sha256"),
      confirmAddAdaptorDataSha256: valueAfter("--confirm-add-adaptor-data-sha256"),
      confirmMaxTotalLamports: valueAfter("--confirm-max-total-lamports"),
    });
  } else if (group === "bootstrap" && operation === "simulate-strategy") {
    result = await simulateStrategyBootstrap(strategyId());
  } else if (group === "bootstrap" && operation === "simulate-strategy-asset-ata") {
    result = await simulateStrategyAssetAta(strategyId());
  } else if (group === "bootstrap" && operation === "authorization") {
    result = await buildBootstrapExecutionAuthorization(positiveIntegerFlag("--lifetime-seconds"));
  } else if (group === "bootstrap" && operation === "execute-strategy") {
    const selectedStrategyId = strategyId();
    result = await executeStrategyBootstrap({
      strategyId: selectedStrategyId,
      authorizationPath: valueAfter("--authorization"),
      confirmAuthorizationSha256: valueAfter("--confirm-authorization-sha256"),
      confirmStrategyId: valueAfter("--confirm-strategy-id"),
      confirmReserve: valueAfter("--confirm-reserve"),
      confirmVault: valueAfter("--confirm-vault"),
      confirmFourMarketRouteSpecSha256: valueAfter("--confirm-four-market-route-spec-sha256"),
      confirmBuilderRouteSpecSha256: valueAfter("--confirm-builder-route-spec-sha256"),
      confirmSetManagerDataSha256: valueAfter("--confirm-set-manager-data-sha256"),
      confirmInitializeStrategyDataSha256: valueAfter("--confirm-initialize-strategy-data-sha256"),
      confirmRestoreManagerDataSha256: valueAfter("--confirm-restore-manager-data-sha256"),
      confirmMaxTotalLamports: valueAfter("--confirm-max-total-lamports"),
    });
  } else if (group === "bootstrap" && operation === "execute-strategy-asset-ata") {
    const selectedStrategyId = strategyId();
    result = await executeStrategyAssetAta({
      strategyId: selectedStrategyId,
      authorizationPath: valueAfter("--authorization"),
      confirmAuthorizationSha256: valueAfter("--confirm-authorization-sha256"),
      confirmStrategyId: valueAfter("--confirm-strategy-id"),
      confirmReserve: valueAfter("--confirm-reserve"),
      confirmVault: valueAfter("--confirm-vault"),
      confirmAta: valueAfter("--confirm-ata"),
      confirmFourMarketRouteSpecSha256: valueAfter("--confirm-four-market-route-spec-sha256"),
      confirmBuilderRouteSpecSha256: valueAfter("--confirm-builder-route-spec-sha256"),
      confirmInstructionDataSha256: valueAfter("--confirm-instruction-data-sha256"),
      confirmMaxTotalLamports: valueAfter("--confirm-max-total-lamports"),
    });
  } else if (group === "verify" && operation === "current") {
    result = await verifyCurrent();
  } else if (group === "verify" && operation === "testing-handoff") {
    const commitment = valueAfter("--commitment") ?? "confirmed";
    if (commitment !== "confirmed") throw new Error("verify testing-handoff requires --commitment confirmed");
    result = await verifyBackyardTestingHandoff();
  } else if (group === "activation" && operation === "simulate-vault-config") {
    result = await simulateVaultConfigActivation();
  } else if (group === "activation" && operation === "execute-vault-config") {
    result = await executeVaultConfigActivation({
      confirmVault: valueAfter("--confirm-vault"),
      confirmVaultCapRaw: valueAfter("--confirm-vault-cap-raw"),
      confirmAdminPerformanceFeeBps: valueAfter("--confirm-admin-performance-fee-bps"),
      confirmRouteSpecSha256: valueAfter("--confirm-route-spec-sha256"),
      confirmMaxTotalLamports: valueAfter("--confirm-max-total-lamports"),
    });
  } else if (group === "verify" && operation === "compatibility") {
    const commitment = valueAfter("--commitment") ?? "confirmed";
    if (commitment !== "confirmed") {
      throw new Error("verify compatibility requires --commitment confirmed");
    }
    result = await verifyFourMarketCompatibility(
      "confirmed",
      valueAfter("--approval"),
      valueAfter("--confirm-approval-sha256"),
    );
  } else if (group === "verify" && operation === "compatibility-approval") {
    result = buildCompatibilityApproval();
  } else if (group === "verify" && operation === "four-market") {
    const evidence = valueAfter("--evidence");
    if (!evidence) throw new Error("verify four-market requires --evidence");
    const commitment = valueAfter("--commitment") ?? "confirmed";
    if (commitment !== "confirmed") throw new Error("verify four-market requires --commitment confirmed");
    result = await verifyFourMarketLifecycle(evidence, "confirmed");
  } else if (group === "verify" && operation === "four-market-manifest") {
    const inputs = valueAfter("--inputs");
    const out = valueAfter("--out");
    if (!inputs || !out) throw new Error("verify four-market-manifest requires --inputs and --out");
    result = buildFourMarketManifestFromInputsFile(inputs, out);
  } else if (group === "verify" && operation === "negative-mutations") {
    const artifact = valueAfter("--artifact");
    const authorization = valueAfter("--authorization");
    const confirmAuthorizationSha256 = valueAfter("--confirm-authorization-sha256");
    const artifactOut = valueAfter("--artifact-out");
    const amountRaw = bigintFlag("--amount-raw");
    if (!artifact || !authorization || !confirmAuthorizationSha256 || !artifactOut || amountRaw === undefined) throw new Error("verify negative-mutations requires --artifact, --authorization, --confirm-authorization-sha256, --amount-raw, and --artifact-out");
    const produced = await produceConfirmedNegativeMutationArtifact({
      catalogPath: artifact,
      authorizationPath: authorization,
      confirmAuthorizationSha256,
      amountRaw,
      outputPath: artifactOut,
    });
    result = {
      verdict: "BACKYARD_VOLTR_NEGATIVE_MUTATIONS_CONFIRMED_PASS",
      broadcast: false,
      signerLoaded: false,
      path: produced.path,
      fileSha256: produced.fileSha256,
      authorizationSha256: produced.authorizationSha256,
      routeSpecSha256: produced.routeSpecSha256,
      protectedPrestateSha256: produced.protectedPrestateSha256,
      protectedContextSlot: produced.protectedContextSlot,
      mutationCount: produced.artifact.mutations.length,
      simulationContextSlots: produced.simulationContextSlots,
    };
  } else if (group === "verify" && operation === "earn-adapter") {
    const inputPath = valueAfter("--input");
    const artifactOut = valueAfter("--artifact-out");
    if (!inputPath || !artifactOut) throw new Error("verify earn-adapter requires --input and --artifact-out");
    const artifact = produceEarnAdapterEvidence(parseEarnAdapterProducerInput(JSON.parse(readFileSync(resolve(inputPath), "utf8"))));
    writeFileSync(resolve(artifactOut), json(artifact));
    result = {
      evidenceType: artifact.evidenceType,
      artifactPath: resolve(artifactOut),
      movementId: artifact.movement.movementId,
      sourceStrategyId: artifact.movement.sourceStrategyId,
      destinationStrategyId: artifact.movement.destinationStrategyId,
      amountRaw: artifact.movement.amountRaw,
      broadcast: artifact.broadcast,
    };
  } else if (group === "policies" && operation === "compile") {
    result = await compileRuntimePolicyArtifact();
  } else if (group === "policies" && operation === "authorization") {
    const artifact = valueAfter("--artifact");
    if (!artifact) throw new Error("policies authorization requires --artifact");
    result = buildPolicyCatalogAuthorization(artifact, valueAfter("--authorization-out") ?? "docs/evidence/backyard-voltr-four-market/policy-catalog-authorization-v7.json");
  } else if (group === "policies" && operation === "verify") {
    const artifact = valueAfter("--artifact");
    if (!artifact) throw new Error("policies verify requires --artifact");
    result = verifyRuntimePolicyArtifact(artifact);
  } else if (group === "policies" && operation === "simulate-install") {
    const artifact = valueAfter("--artifact");
    if (!artifact) throw new Error("policies simulate-install requires --artifact");
    result = await simulateRuntimePolicyInstall(strategyId(), runtimePolicyOperation(), artifact);
  } else if (group === "policies" && operation === "execute-install") {
    const artifact = valueAfter("--artifact");
    if (!artifact) throw new Error("policies execute-install requires --artifact");
    result = await executeRuntimePolicyInstall({
      strategyId: strategyId(),
      operation: runtimePolicyOperation(),
      artifactPath: artifact,
      authorizationPath: valueAfter("--authorization"),
      confirmAuthorizationSha256: valueAfter("--confirm-authorization-sha256"),
      confirmVault: valueAfter("--confirm-vault"),
      confirmArtifactSha256: valueAfter("--confirm-artifact-sha256"),
      confirmPolicyCreateDataSha256: valueAfter("--confirm-policy-create-data-sha256"),
      confirmMaxTotalLamports: valueAfter("--confirm-max-total-lamports"),
      intentPath: valueAfter("--intent-path"),
    });
  } else if (group === "policies" && operation === "verify-current") {
    const artifact = valueAfter("--artifact");
    if (!artifact) throw new Error("policies verify-current requires --artifact");
    result = await verifyExistingRuntimePolicies(artifact);
  } else if (group === "runtime" && (operation === "simulate-manager" || operation === "simulate-manager-operation")) {
    const artifact = valueAfter("--artifact");
    if (!artifact) throw new Error("runtime simulate-manager requires --artifact");
    result = await simulateManagerOperation(
      managerStrategyId(),
      managerOperation(),
      managerAmount(),
      artifact,
      valueAfter("--authorization"),
    );
  } else if (group === "runtime" && (operation === "execute-manager" || operation === "execute-manager-operation")) {
    const artifact = valueAfter("--artifact");
    if (!artifact) throw new Error("runtime execute-manager requires --artifact");
    result = await executeManagerOperation({
      strategyId: managerStrategyId(),
      operation: managerOperation(),
      amountRaw: managerAmount(),
      artifactPath: artifact,
      authorizationPath: valueAfter("--authorization"),
      confirmAuthorizationSha256: valueAfter("--confirm-authorization-sha256"),
      confirmVault: valueAfter("--confirm-vault"),
      confirmArtifactSha256: valueAfter("--confirm-artifact-sha256"),
      confirmAmountRaw: valueAfter("--confirm-amount-raw"),
      confirmWrapperDataSha256: valueAfter("--confirm-wrapper-data-sha256"),
      confirmRouteAuthorizationSha256: valueAfter("--confirm-route-authorization-sha256"),
      lifecycleId: valueAfter("--lifecycle-id"),
      intentPath: valueAfter("--intent-path"),
      restorationBridge: managerRestorationBridgeInput(),
    });
  } else if (group === "runtime" && (operation === "reconcile-manager" || operation === "reconcile-manager-operation")) {
    result = await reconcileConfirmedManagerOperation({
      strategyId: managerStrategyId(),
      operation: managerOperation(),
      signature: valueAfter("--signature") ?? valueAfter("--transaction-signature") ?? "",
    });
  } else if (group === "runtime" && (operation === "reconcile-final" || operation === "reconcile-final-conservation")) {
    const signature = valueAfter("--claim-signature") ?? valueAfter("--signature");
    const slotRaw = valueAfter("--claim-slot") ?? valueAfter("--slot");
    if (!signature || !slotRaw) throw new Error("runtime reconcile-final requires --claim-signature <signature> --claim-slot <confirmed slot>");
    const slot = Number(slotRaw);
    if (!Number.isSafeInteger(slot) || slot <= 0) throw new Error("runtime reconcile-final --claim-slot must be a positive safe integer");
    result = await reconcileConfirmedFinalConservation({ claimSignature: signature, claimSlot: slot });
  } else if (group === "runtime" && operation === "simulate-user-deposit") {
    result = await simulateUserDeposit(bigintFlag("--amount-raw"));
  } else if (group === "runtime" && operation === "execute-user-deposit") {
    result = await executeUserDeposit(valueAfter("--confirm-vault"), valueAfter("--confirm-amount-raw"), valueAfter("--confirm-user"), valueAfter("--confirm-max-total-lamports"), valueAfter("--intent-path"), valueAfter("--confirm-lifecycle-id"), valueAfter("--confirm-protected-prestate-sha256"), valueAfter("--confirm-protected-address-set-sha256"));
  } else if (group === "runtime" && (operation === "simulate-instant-withdraw-rejection" || operation === "simulate-instant-withdraw")) {
    result = await simulateInstantWithdrawRejection(bigintFlag("--amount-lp"));
  } else if (group === "runtime" && operation === "simulate-withdraw-request") {
    result = await simulateWithdrawRequest(bigintFlag("--amount-lp"));
  } else if (group === "runtime" && operation === "execute-withdraw-request") {
    result = await executeWithdrawRequest(valueAfter("--confirm-vault"), valueAfter("--confirm-amount-lp"), valueAfter("--confirm-receipt"), valueAfter("--confirm-user"), valueAfter("--confirm-max-total-lamports"), valueAfter("--intent-path"), valueAfter("--confirm-lifecycle-id"), valueAfter("--confirm-protected-prestate-sha256"), valueAfter("--confirm-protected-address-set-sha256"));
  } else if (group === "runtime" && operation === "scan-withdrawals") {
    const requestSignature = valueAfter("--request-signature");
    const requestEventIndexRaw = valueAfter("--request-event-index");
    const requestReceipt = valueAfter("--request-receipt");
    const requestFlagsSupplied = [requestSignature, requestEventIndexRaw, requestReceipt].filter((value) => value !== null).length;
    if (requestFlagsSupplied !== 0 && requestFlagsSupplied !== 3) {
      throw new Error("runtime scan-withdrawals request binding requires --request-signature, --request-event-index, and --request-receipt together");
    }
    result = await scanWithdrawalDemand(
      requestSignature ?? undefined,
      requestFlagsSupplied === 0 ? 0 : nonNegativeIntegerFlag("--request-event-index")!,
      requestReceipt ?? undefined,
    );
  } else if (group === "runtime" && (operation === "plan-withdrawal-restoration" || operation === "prepare-withdrawal-restoration")) {
    const scanPath = valueAfter("--scan");
    if (!scanPath) throw new Error("runtime plan-withdrawal-restoration requires --scan <confirmed-scan.json>");
    const scan = parseWithdrawalRestorationScanFile(scanPath);
    const generationRaw = valueAfter("--generation");
    const generation = generationRaw === null ? 1 : Number(generationRaw);
    if (!Number.isSafeInteger(generation) || generation <= 0) throw new Error("--generation must be a positive safe integer");
    const positionsPath = valueAfter("--positions");
    const positionEvidenceRaw = positionsPath
      ? null
      : await loadFourMarketRestorationSources(scan.observationContextSlot);
    const sources = positionsPath ? parseFourMarketPositionEvidenceFile(positionsPath, scan.observationContextSlot) : positionEvidenceRaw!.sources;
    const positionEvidence = positionsPath
      ? { verdict: "PARTNER_FOUR_MARKET_POSITION_EVIDENCE_ACCEPTED" as const, broadcast: false, signerLoaded: false, sourcePath: positionsPath, routeId: PARTNER_FOUR_MARKET_ROUTE.id, routeSpecSha256: fourMarketRouteSpecSha256(), vault: PARTNER_ROUTE.vault, minimumContextSlot: scan.observationContextSlot, sources }
      : positionEvidenceRaw;
    const restorationRpcUrl = process.env.SOLANA_RPC_URL;
    if (!restorationRpcUrl) throw new Error("SOLANA_RPC_URL is required for the restoration protected checkpoint");
    const protectedCheckpoint = {
      addressSetSha256: requiredSha256Flag("--protected-address-set-sha256"),
      stateSha256: requiredSha256Flag("--protected-state-sha256"),
      contextSlot: positiveIntegerFlag("--protected-context-slot") ?? (() => { throw new Error("runtime plan-withdrawal-restoration requires --protected-context-slot"); })(),
    };
    if (protectedCheckpoint.contextSlot > scan.observationContextSlot) throw new Error("restoration request checkpoint cannot postdate the confirmed scanner observation");
    const currentProtectedState = await loadFourMarketProtectedState(restorationRpcUrl, scan.observationContextSlot);
    if (currentProtectedState.addressSetSha256 !== protectedCheckpoint.addressSetSha256
      || currentProtectedState.stateSha256 !== protectedCheckpoint.stateSha256
      || currentProtectedState.contextSlot < scan.observationContextSlot) {
      throw new Error("restoration protected state changed between the exact request poststate and confirmed scanner observation");
    }
    const plan = planWithdrawalRestoration(scan, sources, generation, {
      lifecycleId: requiredSha256Flag("--lifecycle-id"),
      routeAuthorizationSha256: requiredSha256Flag("--route-authorization-sha256"),
      requestOrigin: scan.requestOrigin,
      protectedCheckpoint: {
        addressSetSha256: protectedCheckpoint.addressSetSha256,
        stateSha256: protectedCheckpoint.stateSha256,
        contextSlot: protectedCheckpoint.contextSlot,
      },
    });
    const outboxInput = restorationPlanAsOutboxInput(plan, PARTNER_ROUTE.cluster);
    const outboxInputOut = valueAfter("--outbox-input-out");
    const outboxInputFile = outboxInputOut === null ? null : (() => {
      const path = resolve(outboxInputOut);
      const serialized = json(outboxInput);
      writeFileSync(path, serialized, { mode: 0o600 });
      return { path, fileSha256: createHash("sha256").update(serialized, "utf8").digest("hex") };
    })();
    result = {
      verdict: "BACKYARD_VOLTR_WITHDRAWAL_RESTORATION_PLAN_PASS",
      broadcast: false,
      signerLoaded: false,
      scan,
      positionEvidence,
      plan,
      outboxInput,
      outboxInputFile,
      execution: {
        state: "PLANNED_NOT_ENQUEUED",
        blocker: "operator_must_run_the_maintained_rust_enqueue_then_the_manager_handoff_worker",
        durableBoundary: "existing Neon orchestration_outbox via enqueue_voltr_withdrawal_restoration",
        enqueueCommand: "fleet-opportunity-planner --json --enqueue-voltr-restoration-json <this outboxInput JSON>",
      },
    };
  } else if (group === "runtime" && operation === "enqueue-withdrawal-restoration") {
    result = {
      verdict: "BACKYARD_VOLTR_WITHDRAWAL_RESTORATION_ENQUEUE_BLOCKED",
      broadcast: false,
      signerLoaded: false,
      state: "NOT_EXECUTED",
      blocker: "typescript_cli_intentionally_has_no_database_writer; use fleet-opportunity-planner --json --enqueue-voltr-restoration-json with the emitted outboxInput",
      requiredInput: "runtime plan-withdrawal-restoration --scan <scan.json> --positions <position-evidence.json> --vault-id <database-vault-id> --out <plan.json>",
      noSecondScheduler: true,
    };
  } else if (group === "runtime" && operation === "assemble-restoration-evidence") {
    const scanPath = valueAfter("--scan");
    const planPath = valueAfter("--plan");
    const managerPath = valueAfter("--manager");
    const durableReadbackPath = valueAfter("--durable-readback");
    const manifestPath = valueAfter("--manifest-path");
    if (!scanPath || !planPath || !managerPath || !durableReadbackPath || !manifestPath) throw new Error("runtime assemble-restoration-evidence requires --scan, --plan, --manager, --durable-readback, and --manifest-path");
    result = assembleRestorationEvidenceFromFiles({ scanPath, planPath, managerPath, durableReadbackPath, manifestPath });
  } else if (group === "runtime" && (operation === "simulate-withdraw-claim-premature" || operation === "simulate-claim-premature")) {
    result = await simulatePrematureWithdrawClaim(valueAfter("--request-signature") ?? undefined);
  } else if (group === "runtime" && (operation === "simulate-withdraw-claim-post-deadline" || operation === "simulate-claim-post-deadline")) {
    result = await simulatePostDeadlineWithdrawClaim(valueAfter("--request-signature") ?? undefined);
  } else if (group === "runtime" && (operation === "execute-withdraw-claim" || operation === "execute-claim")) {
    result = await executePostDeadlineWithdrawClaim(valueAfter("--confirm-receipt"), valueAfter("--confirm-deadline"), valueAfter("--request-signature"), valueAfter("--confirm-user"), valueAfter("--intent-path"), valueAfter("--confirm-lifecycle-id"), valueAfter("--confirm-protected-prestate-sha256"), valueAfter("--confirm-protected-address-set-sha256"), valueAfter("--confirm-request-event-index"), valueAfter("--confirm-request-raw-account-sha256"), valueAfter("--confirm-request-generation-fingerprint"));
  } else if (group === "verify" && operation === "structure") {
    result = verifyPartnerStructure();
  } else if (group === "verify" && operation === "restoration-planner") {
    result = verifyWithdrawalRestorationPlanner();
  } else if (group === "verify" && operation === "squads") {
    const rpcUrl = process.env.SOLANA_RPC_URL;
    if (!rpcUrl) throw new Error("SOLANA_RPC_URL is required");
    result = await verifyPrecreatedSquadsIsolation(rpcUrl, PARTNER_ROUTE.squads.policySeedBefore);
  } else if (group === "verify" && operation === "lifecycle") {
    const evidence = valueAfter("--evidence");
    if (!evidence) throw new Error("verify lifecycle requires --evidence");
    result = await verifyFinalizedLifecycle(evidence);
  } else {
    throw new Error(`unsupported command ${group ?? "<missing>"} ${operation ?? "<missing>"}`);
  }
  try {
    const serialized = json(result);
    const out = valueAfter("--out");
    if (out) {
      const path = resolve(out);
      writeFileSync(path, serialized, { mode: 0o600 });
      process.stdout.write(json({ wrote: path, verdict: (result as { verdict?: string }).verdict ?? "OUTPUT_WRITTEN" }));
    } else {
      process.stdout.write(serialized);
    }
  } catch (error) {
    const execution = operation?.startsWith("execute-") === true;
    const envelope = result && typeof result === "object" ? result as Record<string, unknown> : {};
    const finalized = envelope.finalized && typeof envelope.finalized === "object" ? envelope.finalized as Record<string, unknown> : null;
    const expectedSignature = typeof finalized?.signature === "string"
      ? finalized.signature
      : typeof envelope.expectedSignature === "string"
        ? envelope.expectedSignature
        : null;
    process.stderr.write(json({
      verdict: "OUTPUT_ERROR",
      broadcast: execution ? envelope.broadcast === true ? true : null : false,
      expectedSignature,
      error: error instanceof Error ? error.message : String(error),
      recoveryInstruction: execution ? "Do not resend. Recover by the exact signature and rerun read-only reconciliation/output." : "Rerun the read-only command or choose a writable output path.",
    }));
    process.exitCode = 1;
  }
}

main().catch((error: unknown) => {
  process.stderr.write(json({
    verdict: "ERROR",
    broadcast: false,
    error: error instanceof Error ? error.message : String(error),
  }));
  process.exitCode = 1;
});
