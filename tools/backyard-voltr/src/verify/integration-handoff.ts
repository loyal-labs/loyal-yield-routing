import { fileURLToPath } from "node:url";
import { resolve } from "node:path";
import { createHash } from "node:crypto";

import { getMintDecoder, getTokenDecoder } from "@solana-program/token";
import { Connection } from "@solana/web3.js";
import { getVaultDecoder } from "@voltr/vault-sdk";

import {
  PARTNER_FOUR_MARKET_ROUTE,
  PARTNER_FOUR_MARKET_STRATEGIES,
  PARTNER_ROUTE,
  fourMarketRouteSpecSha256,
  partnerBuilderRoute,
} from "../domain/route-spec.js";
import {
  confirmedSnapshots,
  loadDeploymentIdentities,
} from "../integrations/solana-compat.js";
import {
  deriveVoltrAccounts,
  deriveVoltrAccountsForStrategy,
} from "../integrations/voltr.js";
import {
  loadRuntimePolicyArtifact,
} from "../policies/compiler.js";
import { verifyExistingRuntimePolicies } from "../policies/commands.js";
import {
  verifyAdaptorReceipt,
  verifyDeploymentIdentities,
  verifyStrategyBootstrap,
  type Gate,
} from "./current.js";
import { verifyLegacyVoltrPolicyCatalog } from "./squads.js";

const REPOSITORY_ROOT = resolve(fileURLToPath(new URL("../../../..", import.meta.url)));
const POLICY_ARTIFACT = resolve(
  REPOSITORY_ROOT,
  "docs/evidence/backyard-voltr-four-market/runtime-policy-catalog-v2.json",
);

export const BACKYARD_TESTING_HANDOFF_TARGET = {
  vault: String(PARTNER_ROUTE.vault),
  lpMint: String(PARTNER_FOUR_MARKET_ROUTE.commonVoltr.lpMint),
  vaultCapRaw: 1_000_000_000_000n,
  policyMaxOperationRaw: 200_000_000_000n,
  normalOptimizationMaxPrincipalRaw: 100_000_000_000n,
  withdrawalWaitingPeriodSeconds: 600n,
  normalOptimizationIntervalSeconds: 3_600n,
  lockedProfitDegradationDurationSeconds: 86_400n,
  managerPerformanceFeeBps: 0,
  adminPerformanceFeeBps: 500,
  managerManagementFeeBps: 0,
  adminManagementFeeBps: 0,
  redemptionFeeBps: 0,
  issuanceFeeBps: 0,
  disabledOperations: 0,
  allowAnyAdaptor: 0,
} as const;

function add(
  gates: Gate[],
  name: string,
  pass: boolean,
  observed: unknown,
  expected: unknown,
): void {
  gates.push({ name, pass, observed, expected });
}

function errorText(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

export async function verifyBackyardTestingHandoff() {
  const rpcUrl = process.env.SOLANA_RPC_URL;
  if (!rpcUrl) {
    return {
      schemaVersion: 1,
      verifier: "backyard-voltr-testing-handoff",
      verdict: "BLOCKED",
      broadcast: false,
      signerLoaded: false,
      blocker: "SOLANA_RPC_URL is unavailable",
      resumeCondition: "run through the mounted .env.1password environment",
      gates: [],
    } as const;
  }

  const connection = new Connection(rpcUrl, "confirmed");
  let genesisHash: string;
  try {
    genesisHash = await connection.getGenesisHash();
  } catch (error) {
    return {
      schemaVersion: 1,
      verifier: "backyard-voltr-testing-handoff",
      verdict: "BLOCKED",
      broadcast: false,
      signerLoaded: false,
      blocker: `confirmed Solana RPC is unavailable: ${errorText(error)}`,
      resumeCondition: "restore the mounted RPC connection and rerun the same verifier",
      gates: [],
    } as const;
  }

  const gates: Gate[] = [];
  const target = BACKYARD_TESTING_HANDOFF_TARGET;
  add(gates, "mainnet genesis exact", genesisHash === PARTNER_ROUTE.genesisHash, genesisHash, PARTNER_ROUTE.genesisHash);
  add(gates, "stable vault address exact", String(PARTNER_ROUTE.vault) === target.vault, PARTNER_ROUTE.vault, target.vault);
  add(gates, "stable LP mint address exact", String(PARTNER_FOUR_MARKET_ROUTE.commonVoltr.lpMint) === target.lpMint, PARTNER_FOUR_MARKET_ROUTE.commonVoltr.lpMint, target.lpMint);
  add(gates, "checked-in vault cap is testing target", BigInt(PARTNER_ROUTE.asset.vaultCapRaw) === target.vaultCapRaw, PARTNER_ROUTE.asset.vaultCapRaw, target.vaultCapRaw);
  add(gates, "checked-in policy ceiling is testing target", BigInt(PARTNER_ROUTE.asset.maxManagerOperationRaw) === target.policyMaxOperationRaw, PARTNER_ROUTE.asset.maxManagerOperationRaw, target.policyMaxOperationRaw);
  add(gates, "checked-in normal optimization interval is hourly", BigInt(PARTNER_FOUR_MARKET_ROUTE.normalOptimizationIntervalSeconds) === target.normalOptimizationIntervalSeconds, PARTNER_FOUR_MARKET_ROUTE.normalOptimizationIntervalSeconds, target.normalOptimizationIntervalSeconds);
  add(gates, "checked-in withdrawal wait remains ten minutes", PARTNER_ROUTE.vaultConfiguration.withdrawalWaitingPeriodSeconds === target.withdrawalWaitingPeriodSeconds && PARTNER_FOUR_MARKET_ROUTE.withdrawalWaitingPeriodSeconds === target.withdrawalWaitingPeriodSeconds, { vault: PARTNER_ROUTE.vaultConfiguration.withdrawalWaitingPeriodSeconds, route: PARTNER_FOUR_MARKET_ROUTE.withdrawalWaitingPeriodSeconds }, target.withdrawalWaitingPeriodSeconds);
  add(gates, "checked-in locked-profit duration remains one day", PARTNER_ROUTE.vaultConfiguration.lockedProfitDegradationDurationSeconds === target.lockedProfitDegradationDurationSeconds, PARTNER_ROUTE.vaultConfiguration.lockedProfitDegradationDurationSeconds, target.lockedProfitDegradationDurationSeconds);
  add(gates, "checked-in fee split is exact five-percent admin performance fee", Number(PARTNER_ROUTE.vaultConfiguration.managerPerformanceFeeBps) === target.managerPerformanceFeeBps && Number(PARTNER_ROUTE.vaultConfiguration.adminPerformanceFeeBps) === target.adminPerformanceFeeBps && Number(PARTNER_ROUTE.vaultConfiguration.managerManagementFeeBps) === target.managerManagementFeeBps && Number(PARTNER_ROUTE.vaultConfiguration.adminManagementFeeBps) === target.adminManagementFeeBps && Number(PARTNER_ROUTE.vaultConfiguration.redemptionFeeBps) === target.redemptionFeeBps && Number(PARTNER_ROUTE.vaultConfiguration.issuanceFeeBps) === target.issuanceFeeBps, PARTNER_ROUTE.vaultConfiguration, { managerPerformanceFeeBps: 0, adminPerformanceFeeBps: 500, allOtherFeesBps: 0 });

  let contextSlot = 0;
  let vaultDataSha256: string | null = null;
  let highWaterMark: null | Readonly<{
    highestAssetPerLpDecimalBits: bigint;
    lastUpdatedTs: bigint;
    lastPerformanceFeeUpdateTs: bigint;
  }> = null;
  try {
    const accounts = await deriveVoltrAccounts(PARTNER_ROUTE);
    const state = await confirmedSnapshots(rpcUrl, [
      PARTNER_ROUTE.vault,
      accounts.lpMint,
      accounts.idleAta,
      PARTNER_ROUTE.asset.mint,
      accounts.adaptorAddReceipt,
    ]);
    contextSlot = state.contextSlot;
    const vaultAccount = state.accounts[0] ?? null;
    const lpAccount = state.accounts[1] ?? null;
    const idleAccount = state.accounts[2] ?? null;
    const assetMintAccount = state.accounts[3] ?? null;
    const adaptorAccount = state.accounts[4] ?? null;
    add(gates, "live vault account exists", vaultAccount !== null, vaultAccount?.address ?? null, target.vault);
    add(gates, "live LP mint exists", lpAccount !== null, lpAccount?.address ?? null, target.lpMint);
    add(gates, "live idle ATA exists", idleAccount !== null, idleAccount?.address ?? null, String(accounts.idleAta));
    add(gates, "live USDC mint exists", assetMintAccount !== null, assetMintAccount?.address ?? null, String(PARTNER_ROUTE.asset.mint));
    if (vaultAccount && lpAccount && idleAccount && assetMintAccount) {
      const vault = getVaultDecoder().decode(vaultAccount.data);
      const lpMint = getMintDecoder().decode(lpAccount.data);
      const idle = getTokenDecoder().decode(idleAccount.data);
      const assetMint = getMintDecoder().decode(assetMintAccount.data);
      vaultDataSha256 = createHash("sha256").update(vaultAccount.data).digest("hex");
      add(gates, "live vault owner is Voltr", vaultAccount.owner === PARTNER_ROUTE.programs.voltrVault, vaultAccount.owner, PARTNER_ROUTE.programs.voltrVault);
      add(gates, "live vault and LP addresses remain stable", vault.lp.mint === target.lpMint && lpAccount.address === target.lpMint, { vaultLpMint: vault.lp.mint, account: lpAccount.address }, target.lpMint);
      add(gates, "live admin boundary exact", vault.admin === PARTNER_ROUTE.setupAdmin && vault.pendingAdmin === PARTNER_ROUTE.setupAdmin, { admin: vault.admin, pendingAdmin: vault.pendingAdmin }, PARTNER_ROUTE.setupAdmin);
      add(gates, "live manager remains Squads PDA", vault.manager === PARTNER_ROUTE.squads.manager, vault.manager, PARTNER_ROUTE.squads.manager);
      add(gates, "live vault cap is one million USDC", vault.vaultConfiguration.maxCap === target.vaultCapRaw, vault.vaultConfiguration.maxCap, target.vaultCapRaw);
      add(gates, "live withdrawal and locked-profit durations exact", vault.vaultConfiguration.withdrawalWaitingPeriod === target.withdrawalWaitingPeriodSeconds && vault.vaultConfiguration.lockedProfitDegradationDuration === target.lockedProfitDegradationDurationSeconds, { withdrawalWaitingPeriod: vault.vaultConfiguration.withdrawalWaitingPeriod, lockedProfitDegradationDuration: vault.vaultConfiguration.lockedProfitDegradationDuration }, { withdrawalWaitingPeriod: target.withdrawalWaitingPeriodSeconds, lockedProfitDegradationDuration: target.lockedProfitDegradationDurationSeconds });
      add(gates, "live operation and adaptor bypass settings exact", vault.vaultConfiguration.disabledOperations === target.disabledOperations && vault.allowAnyAdaptor === target.allowAnyAdaptor, { disabledOperations: vault.vaultConfiguration.disabledOperations, allowAnyAdaptor: vault.allowAnyAdaptor }, { disabledOperations: target.disabledOperations, allowAnyAdaptor: target.allowAnyAdaptor });
      add(gates, "live five-percent fee split exact", vault.feeConfiguration.managerPerformanceFee === target.managerPerformanceFeeBps && vault.feeConfiguration.adminPerformanceFee === target.adminPerformanceFeeBps && vault.feeConfiguration.managerManagementFee === target.managerManagementFeeBps && vault.feeConfiguration.adminManagementFee === target.adminManagementFeeBps && vault.feeConfiguration.redemptionFee === target.redemptionFeeBps && vault.feeConfiguration.issuanceFee === target.issuanceFeeBps, vault.feeConfiguration, { managerPerformanceFee: 0, adminPerformanceFee: 500, allOtherFees: 0 });
      highWaterMark = {
        highestAssetPerLpDecimalBits: vault.highWaterMark.highestAssetPerLpDecimalBits,
        lastUpdatedTs: vault.highWaterMark.lastUpdatedTs,
        lastPerformanceFeeUpdateTs: vault.feeUpdate.lastPerformanceFeeUpdateTs,
      };
      const highWaterMarkReady = highWaterMark.highestAssetPerLpDecimalBits > 0n
        && highWaterMark.lastUpdatedTs > 0n
        && (lpMint.supply === 0n
          ? highWaterMark.lastPerformanceFeeUpdateTs === 0n
          : highWaterMark.lastPerformanceFeeUpdateTs > 0n && highWaterMark.lastUpdatedTs >= highWaterMark.lastPerformanceFeeUpdateTs);
      add(gates, "high-water mark state matches LP lifecycle", highWaterMarkReady, { ...highWaterMark, lpSupplyRaw: lpMint.supply }, "initialized HWM; fee update timestamp is zero before first LP and positive thereafter");
      add(gates, "live LP mint authority exact", lpMint.mintAuthority.__option === "Some" && lpMint.mintAuthority.value === accounts.lpMintAuth, lpMint.mintAuthority, accounts.lpMintAuth);
      add(gates, "live LP and asset decimals exact", lpMint.decimals === 9 && assetMint.decimals === PARTNER_ROUTE.asset.decimals, { lp: lpMint.decimals, asset: assetMint.decimals }, { lp: 9, asset: PARTNER_ROUTE.asset.decimals });
      add(gates, "live idle ATA exact", idle.owner === accounts.idleAuth && idle.mint === PARTNER_ROUTE.asset.mint, { owner: idle.owner, mint: idle.mint }, { owner: accounts.idleAuth, mint: PARTNER_ROUTE.asset.mint });
    }
    gates.push(...verifyAdaptorReceipt(PARTNER_ROUTE, accounts.adaptorAddReceipt, adaptorAccount).map((gate) => ({ ...gate, name: `adaptor: ${gate.name}` })));
  } catch (error) {
    add(gates, "live vault/configuration readback", false, errorText(error), "exact confirmed vault configuration");
  }

  const strategyReports: Array<Readonly<{ strategyId: string; contextSlot: number; failedGateCount: number }>> = [];
  for (const expected of PARTNER_FOUR_MARKET_STRATEGIES) {
    const strategyGates: Gate[] = [];
    let strategySlot = 0;
    try {
      const route = partnerBuilderRoute(expected.id);
      const accounts = await deriveVoltrAccountsForStrategy(route, expected.reserve);
      add(strategyGates, "derived strategy identities exact", accounts.strategyAuth === expected.voltr.strategyAuth && accounts.strategyInitReceipt === expected.voltr.strategyInitReceipt, { strategyAuth: accounts.strategyAuth, strategyInitReceipt: accounts.strategyInitReceipt }, expected.voltr);
      const state = await confirmedSnapshots(rpcUrl, [
        accounts.strategyInitReceipt,
        expected.graph.userMetadata,
        expected.graph.obligation,
        expected.graph.obligationFarm,
        expected.voltr.strategyAssetAta,
      ], contextSlot);
      strategySlot = state.contextSlot;
      strategyGates.push(...verifyStrategyBootstrap({
        route,
        accounts,
        graph: { reserve: expected.reserve, ...expected.graph },
        strategyReceipt: state.accounts[0] ?? null,
        userMetadata: state.accounts[1] ?? null,
        obligation: state.accounts[2] ?? null,
        obligationFarm: state.accounts[3] ?? null,
      }));
      const ataAccount = state.accounts[4] ?? null;
      let ata: ReturnType<ReturnType<typeof getTokenDecoder>["decode"]> | null = null;
      try { ata = ataAccount ? getTokenDecoder().decode(ataAccount.data) : null; } catch { ata = null; }
      add(strategyGates, "strategy USDC ATA exact", ataAccount?.address === expected.voltr.strategyAssetAta && ataAccount.owner === PARTNER_ROUTE.programs.token && ata?.mint === PARTNER_ROUTE.asset.mint && ata.owner === expected.voltr.strategyAuth, ataAccount ? { address: ataAccount.address, ownerProgram: ataAccount.owner, mint: ata?.mint ?? null, authority: ata?.owner ?? null } : null, { address: expected.voltr.strategyAssetAta, ownerProgram: PARTNER_ROUTE.programs.token, mint: PARTNER_ROUTE.asset.mint, authority: expected.voltr.strategyAuth });
    } catch (error) {
      add(strategyGates, "strategy readback", false, errorText(error), "exact initialized native-Kamino strategy");
    }
    gates.push(...strategyGates.map((gate) => ({ ...gate, name: `${expected.id}: ${gate.name}` })));
    strategyReports.push({ strategyId: expected.id, contextSlot: strategySlot, failedGateCount: strategyGates.filter(({ pass }) => !pass).length });
    contextSlot = Math.max(contextSlot, strategySlot);
  }

  let policyEvidence: Awaited<ReturnType<typeof verifyExistingRuntimePolicies>> | null = null;
  try {
    const loaded = loadRuntimePolicyArtifact(POLICY_ARTIFACT);
    const manifestLimits = loaded.artifact.sourceManifests?.map(({ strategyId, limits }) => ({ strategyId, maxPerOperationRaw: limits.maxPerOperationRaw })) ?? [];
    add(gates, "policy artifact is exact current route bundle", loaded.artifact.runtimePolicyCount === 8 && loaded.artifact.routeSpecSha256 === fourMarketRouteSpecSha256(), { count: loaded.artifact.runtimePolicyCount, routeSpecSha256: loaded.artifact.routeSpecSha256 }, { count: 8, routeSpecSha256: fourMarketRouteSpecSha256() });
    add(gates, "all eight policy manifests use the 200k ceiling", manifestLimits.length === 4 && manifestLimits.every(({ maxPerOperationRaw }) => BigInt(maxPerOperationRaw) === target.policyMaxOperationRaw), manifestLimits, target.policyMaxOperationRaw);
    const legacyEvidence = await verifyLegacyVoltrPolicyCatalog(rpcUrl, contextSlot, "confirmed");
    add(gates, "immutable one-raw legacy policy generation remains exactly classified", legacyEvidence.verdict === "PARTNER_LEGACY_VOLTR_POLICIES_CONFIRMED_PASS" && legacyEvidence.failedGateCount === 0, { verdict: legacyEvidence.verdict, failedGateCount: legacyEvidence.failedGateCount, policyCount: legacyEvidence.policies.length, contextSlot: legacyEvidence.contextSlot }, { verdict: "PARTNER_LEGACY_VOLTR_POLICIES_CONFIRMED_PASS", failedGateCount: 0, policyCount: 8 });
    policyEvidence = await verifyExistingRuntimePolicies(POLICY_ARTIFACT, Math.max(contextSlot, legacyEvidence.contextSlot), "confirmed", [{ firstSeed: 17n, lastSeed: 24n }]);
    add(gates, "eight exact current live policies and origins pass", policyEvidence.verdict === "PARTNER_RUNTIME_POLICIES_CONFIRMED_PASS" && policyEvidence.failedGateCount === 0 && policyEvidence.policies.length === 8, { verdict: policyEvidence.verdict, failedGateCount: policyEvidence.failedGateCount, policyCount: policyEvidence.policies.length, contextSlot: policyEvidence.contextSlot }, { verdict: "PARTNER_RUNTIME_POLICIES_CONFIRMED_PASS", failedGateCount: 0, policyCount: 8 });
    contextSlot = Math.max(contextSlot, policyEvidence.contextSlot);
  } catch (error) {
    add(gates, "policy artifact and live readback", false, errorText(error), "exact checked-in 200k eight-policy catalog installed on-chain");
  }

  try {
    const deployments = await loadDeploymentIdentities(rpcUrl, PARTNER_ROUTE, contextSlot, "confirmed");
    gates.push(...verifyDeploymentIdentities(PARTNER_ROUTE, deployments.identities).map((gate) => ({ ...gate, name: `deployment: ${gate.name}` })));
    add(gates, "deployment read reaches all route state", deployments.contextSlot >= contextSlot, deployments.contextSlot, `>=${contextSlot}`);
    contextSlot = Math.max(contextSlot, deployments.contextSlot);
  } catch (error) {
    add(gates, "deployment identity readback", false, errorText(error), "exact approved executable identities");
  }

  const failed = gates.filter(({ pass }) => !pass);
  return {
    schemaVersion: 1,
    verifier: "backyard-voltr-testing-handoff",
    verdict: failed.length === 0 ? "PASS" : "FAIL",
    broadcast: false,
    signerLoaded: false,
    commitment: "confirmed",
    contextSlot,
    routeSpecSha256: fourMarketRouteSpecSha256(),
    policyArtifactPath: POLICY_ARTIFACT,
    addresses: { vault: target.vault, lpMint: target.lpMint },
    target,
    current: { vaultDataSha256, highWaterMark, strategies: strategyReports, policyContextSlot: policyEvidence?.contextSlot ?? null },
    failedGateCount: failed.length,
    firstFailure: failed[0]?.name ?? null,
    resumeCondition: failed.length === 0 ? null : failed[0]?.expected ?? null,
    gates,
  } as const;
}
