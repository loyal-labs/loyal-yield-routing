import { findRequestWithdrawVaultReceiptPda, getStrategyInitReceiptDecoder } from "@voltr/vault-sdk";
import { address } from "@solana/kit";

import {
  PARTNER_FOUR_MARKET_ROUTE,
  PARTNER_ROUTE,
  fourMarketRouteSpecSha256,
  partnerStrategyIdentity,
} from "../domain/route-spec.js";
import {
  confirmedSnapshots,
  confirmedTransaction,
  loadDeploymentIdentities,
} from "../integrations/solana-compat.js";
import { deriveVoltrAccountsForStrategy } from "../integrations/voltr.js";
import { verifyDeploymentIdentities, verifyVaultCurrentState } from "../verify/current.js";
import { scanWithdrawalDemand } from "./withdrawal-scanner.js";

const STRATEGIES = ["main", "onre", "prime", "maple"] as const;
type StrategyId = (typeof STRATEGIES)[number];

function positiveSlot(value: number, label: string): number {
  if (!Number.isSafeInteger(value) || value <= 0) throw new Error(`${label} must be a positive safe integer`);
  return value;
}

function assertClaimRoute(transaction: Awaited<ReturnType<typeof confirmedTransaction>>): void {
  const keys = [
    ...transaction.transaction.message.staticAccountKeys,
    ...(transaction.meta?.loadedAddresses?.writable ?? []),
    ...(transaction.meta?.loadedAddresses?.readonly ?? []),
  ].map((key) => key.toBase58());
  for (const expected of [PARTNER_ROUTE.vault, PARTNER_ROUTE.setupAdmin, PARTNER_ROUTE.programs.voltrVault]) {
    if (!keys.includes(expected)) throw new Error(`claim transaction does not contain the frozen route account ${expected}`);
  }
  const logs = transaction.meta?.logMessages ?? [];
  if (!logs.some((line) => line === `Program ${PARTNER_ROUTE.programs.voltrVault} invoke [1]`)) {
    throw new Error("claim transaction does not invoke the frozen Voltr vault program");
  }
}

/**
 * Produce the signer-free final conservation envelope after a successful
 * confirmed claim. The envelope intentionally contains only the schema that
 * the four-market verifier consumes; all identities and expected addresses
 * come from the frozen route catalog, never from CLI input.
 */
export async function reconcileConfirmedFinalConservation(input: Readonly<{
  claimSignature: string;
  claimSlot: number;
}>): Promise<Readonly<{
  schemaVersion: 1;
  evidenceType: "backyard-voltr-final-current-conservation";
  broadcast: false;
  routeId: string;
  routeSpecSha256: string;
  finalContextSlot: number;
  activeReceipts: readonly string[];
  conservation: Readonly<{
    idleRaw: string;
    strategyPositionsRaw: Readonly<Record<StrategyId, string>>;
    lpSupplyRaw: string;
    vaultTotalValueRaw: string;
    accountingDifferenceRaw: string;
  }>;
}>> {
  const claimSignature = input.claimSignature.trim();
  if (claimSignature.length === 0) throw new Error("final reconciliation requires a non-empty confirmed claim signature");
  const claimSlot = positiveSlot(input.claimSlot, "final reconciliation claim slot");
  const rpcUrl = process.env.SOLANA_RPC_URL;
  if (!rpcUrl) throw new Error("SOLANA_RPC_URL is required");

  // Read the exact claim first. This prevents a caller from anchoring a fresh
  // conservation read to an arbitrary slot or to a failed/non-Voltr tx.
  const claim = await confirmedTransaction(rpcUrl, claimSignature, claimSlot);
  if (claim.slot !== claimSlot) throw new Error(`claim signature landed at slot ${claim.slot}, expected ${claimSlot}`);
  assertClaimRoute(claim);

  // Deployment identity is not emitted in the intentionally minimal artifact,
  // but a changed approved deployment must prevent artifact production.
  const deployments = await loadDeploymentIdentities(rpcUrl, PARTNER_ROUTE, claimSlot, "confirmed");
  const deploymentGates = verifyDeploymentIdentities(PARTNER_ROUTE, deployments.identities);
  if (!deploymentGates.every(({ pass }) => pass)) throw new Error("approved deployment identity changed at or after the claim slot");

  const baseAccounts = await deriveVoltrAccountsForStrategy(PARTNER_ROUTE, PARTNER_ROUTE.strategy.reserve);
  const [lifecycleReceipt] = await findRequestWithdrawVaultReceiptPda(
    { vault: PARTNER_ROUTE.vault, userTransferAuthority: address(PARTNER_ROUTE.setupAdmin) },
    { programAddress: PARTNER_ROUTE.programs.voltrVault },
  );

  // Scanner provides the authoritative active-receipt set. It uses the same
  // process-injected RPC endpoint as the claim read and is itself fenced at the
  // claim slot, so an older provider snapshot cannot be mixed into the final
  // conservation envelope.
  const scan = await scanWithdrawalDemand(undefined, 0, undefined, claimSlot);
  const minimumContextSlot = Math.max(claimSlot, scan.observationContextSlot, deployments.contextSlot);
  const addresses = [
    PARTNER_ROUTE.vault,
    baseAccounts.lpMint,
    baseAccounts.idleAta,
    PARTNER_ROUTE.asset.mint,
    ...STRATEGIES.map((id) => partnerStrategyIdentity(id).voltr.strategyInitReceipt),
    lifecycleReceipt,
  ];
  const state = await confirmedSnapshots(rpcUrl, addresses, minimumContextSlot);
  const vault = verifyVaultCurrentState({
    route: PARTNER_ROUTE,
    accounts: baseAccounts,
    vault: state.accounts[0] ?? null,
    lpMint: state.accounts[1] ?? null,
    idleAta: state.accounts[2] ?? null,
    assetMint: state.accounts[3] ?? null,
  });
  if (!vault.state || vault.failedGateCount !== 0) throw new Error("final confirmed vault state does not pass the frozen RouteSpec decoder");
  if (state.accounts[8] !== null) throw new Error("the lifecycle withdrawal receipt is still active after the claim");

  const strategyPositions = Object.fromEntries(STRATEGIES.map((id, index) => {
    const snapshot = state.accounts[4 + index];
    if (!snapshot) throw new Error(`final ${id} strategy receipt is absent`);
    const receipt = getStrategyInitReceiptDecoder().decode(snapshot.data);
    const expected = partnerStrategyIdentity(id);
    if (snapshot.owner !== PARTNER_ROUTE.programs.voltrVault || receipt.vault !== PARTNER_ROUTE.vault || receipt.strategy !== expected.reserve || receipt.adaptorProgram !== PARTNER_ROUTE.programs.kaminoAdaptor) {
      throw new Error(`final ${id} strategy receipt identity changed`);
    }
    if (receipt.positionValue < 0n) throw new Error(`final ${id} strategy position is negative`);
    return [id, receipt.positionValue] as const;
  })) as Record<StrategyId, bigint>;
  const idleRaw = vault.state.idleRaw;
  const sumPositions = STRATEGIES.reduce((sum, id) => sum + strategyPositions[id], 0n);
  const accountingDifferenceRaw = vault.state.totalValueRaw - idleRaw - sumPositions;
  if (accountingDifferenceRaw !== 0n) throw new Error(`final conservation mismatch: ${accountingDifferenceRaw.toString()} raw units`);
  const activeReceipts = scan.receipts.map(({ receipt }) => receipt).slice().sort();
  if (activeReceipts.includes(lifecycleReceipt)) throw new Error("scanner still reports the claimed lifecycle receipt as active");
  return {
    schemaVersion: 1,
    evidenceType: "backyard-voltr-final-current-conservation",
    broadcast: false,
    routeId: PARTNER_FOUR_MARKET_ROUTE.id,
    routeSpecSha256: fourMarketRouteSpecSha256(),
    finalContextSlot: state.contextSlot,
    activeReceipts,
    conservation: {
      idleRaw: idleRaw.toString(),
      strategyPositionsRaw: Object.fromEntries(STRATEGIES.map((id) => [id, strategyPositions[id].toString()])) as Record<StrategyId, string>,
      lpSupplyRaw: vault.state.lpSupplyRaw.toString(),
      vaultTotalValueRaw: vault.state.totalValueRaw.toString(),
      accountingDifferenceRaw: accountingDifferenceRaw.toString(),
    },
  };
}
