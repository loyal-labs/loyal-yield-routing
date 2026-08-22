import { createHash } from "node:crypto";

import {
  findAssociatedTokenPda,
  getCreateAssociatedTokenIdempotentInstructionAsync,
  getMintDecoder,
} from "@solana-program/token";
import {
  address,
  getU16Encoder,
  getU64Encoder,
  type Instruction,
} from "@solana/kit";
import {
  getUpdateVaultConfigInstructionAsync,
  getVaultDecoder,
  VaultConfigField,
} from "@voltr/vault-sdk";

import { PARTNER_ROUTE, fourMarketRouteSpecSha256 } from "../domain/route-spec.js";
import {
  confirmedSnapshots,
  loadDeploymentIdentities,
  prepareSignedV0Transaction,
  sendPreparedConfirmedOnce,
  type AccountSnapshot,
} from "../integrations/solana-compat.js";
import { signingMaterialFromEnvironment } from "../integrations/signer.js";
import { deriveVoltrAccounts } from "../integrations/voltr.js";
import { verifyDeploymentIdentities, type Gate } from "../verify/current.js";

const TARGET_CAP_RAW = 1_000_000_000_000n;
const TARGET_ADMIN_PERFORMANCE_FEE_BPS = 500;
const MAX_TOTAL_LAMPORTS = 5_000_000;

function rpcUrl(): string {
  const value = process.env.SOLANA_RPC_URL;
  if (!value) throw new Error("SOLANA_RPC_URL is required");
  return value;
}

function sha256(data: ArrayLike<number>): string {
  return createHash("sha256").update(Uint8Array.from(data)).digest("hex");
}

function add(gates: Gate[], name: string, pass: boolean, observed: unknown, expected: unknown): void {
  gates.push({ name, pass, observed, expected });
}

function snapshotFingerprint(snapshot: AccountSnapshot | null): unknown {
  return snapshot === null ? null : {
    address: snapshot.address,
    owner: snapshot.owner,
    lamports: snapshot.lamports,
    executable: snapshot.executable,
    dataSha256: sha256(snapshot.data),
  };
}

function targetVaultState(snapshot: AccountSnapshot | null): Readonly<{
  capRaw: bigint;
  adminPerformanceFeeBps: number;
  highWaterMarkBits: bigint;
  highWaterMarkUpdatedTs: bigint;
  lastPerformanceFeeUpdateTs: bigint;
  totalValueRaw: bigint;
}> | null {
  if (!snapshot || snapshot.owner !== PARTNER_ROUTE.programs.voltrVault) return null;
  const vault = getVaultDecoder().decode(snapshot.data);
  return {
    capRaw: vault.vaultConfiguration.maxCap,
    adminPerformanceFeeBps: vault.feeConfiguration.adminPerformanceFee,
    highWaterMarkBits: vault.highWaterMark.highestAssetPerLpDecimalBits,
    highWaterMarkUpdatedTs: vault.highWaterMark.lastUpdatedTs,
    lastPerformanceFeeUpdateTs: vault.feeUpdate.lastPerformanceFeeUpdateTs,
    totalValueRaw: vault.asset.totalValue,
  };
}

function isActivated(state: ReturnType<typeof targetVaultState>, lpSupplyRaw: bigint): boolean {
  return state !== null
    && state.capRaw === TARGET_CAP_RAW
    && state.adminPerformanceFeeBps === TARGET_ADMIN_PERFORMANCE_FEE_BPS
    && state.highWaterMarkBits > 0n
    && state.highWaterMarkUpdatedTs > 0n
    && (lpSupplyRaw === 0n
      ? state.lastPerformanceFeeUpdateTs === 0n
      : state.lastPerformanceFeeUpdateTs > 0n && state.highWaterMarkUpdatedTs >= state.lastPerformanceFeeUpdateTs);
}

async function buildPreparation() {
  const admin = await signingMaterialFromEnvironment("SOLANA_TESTING_PK");
  if (admin.signer.address !== PARTNER_ROUTE.setupAdmin) {
    throw new Error(`SOLANA_TESTING_PK ${admin.signer.address} is not RouteSpec setup admin`);
  }
  const accounts = await deriveVoltrAccounts(PARTNER_ROUTE);
  const [adminLpAta] = await findAssociatedTokenPda(
    { owner: address(PARTNER_ROUTE.setupAdmin), mint: accounts.lpMint, tokenProgram: PARTNER_ROUTE.programs.token },
    { programAddress: PARTNER_ROUTE.programs.associatedToken },
  );
  const protectedAddresses = [
    PARTNER_ROUTE.setupAdmin,
    PARTNER_ROUTE.vault,
    accounts.lpMint,
    accounts.idleAta,
    PARTNER_ROUTE.asset.mint,
    adminLpAta,
  ] as const;
  const before = await confirmedSnapshots(rpcUrl(), protectedAddresses);
  const beforeVault = before.accounts[1] ?? null;
  const beforeLpMint = before.accounts[2] ?? null;
  const beforeState = targetVaultState(beforeVault);
  if (!beforeState) throw new Error("live vault is absent or not owned by the approved Voltr program");
  if (!beforeLpMint || beforeLpMint.owner !== PARTNER_ROUTE.programs.token) throw new Error("live LP mint is absent or has the wrong token program");
  const beforeLpSupply = getMintDecoder().decode(beforeLpMint.data).supply;
  if (beforeLpSupply !== 0n || beforeState.totalValueRaw > 1n) {
    throw new Error(`unsafe HWM calibration is permitted only for the empty test vault (LP supply ${beforeLpSupply}, total value ${beforeState.totalValueRaw})`);
  }
  if (isActivated(beforeState, beforeLpSupply)) {
    return { alreadyActivated: true, beforeState, beforeContextSlot: before.contextSlot } as const;
  }

  const createAdminLpAta = await getCreateAssociatedTokenIdempotentInstructionAsync({
    payer: admin.signer,
    ata: adminLpAta,
    owner: address(PARTNER_ROUTE.setupAdmin),
    mint: accounts.lpMint,
    systemProgram: PARTNER_ROUTE.programs.system,
    tokenProgram: PARTNER_ROUTE.programs.token,
  }, { programAddress: PARTNER_ROUTE.programs.associatedToken });
  const updateCap = await getUpdateVaultConfigInstructionAsync({
    admin: admin.signer,
    vault: PARTNER_ROUTE.vault,
    field: VaultConfigField.MaxCap,
    data: getU64Encoder().encode(TARGET_CAP_RAW),
  }, { programAddress: PARTNER_ROUTE.programs.voltrVault });
  const updateAdminPerformanceFee = await getUpdateVaultConfigInstructionAsync({
    admin: admin.signer,
    vault: PARTNER_ROUTE.vault,
    field: VaultConfigField.AdminPerformanceFee,
    data: getU16Encoder().encode(TARGET_ADMIN_PERFORMANCE_FEE_BPS),
  }, { programAddress: PARTNER_ROUTE.programs.voltrVault });
  const instructions: readonly Instruction[] = [
    createAdminLpAta,
    updateCap,
    updateAdminPerformanceFee,
  ];
  const deploymentsBefore = await loadDeploymentIdentities(rpcUrl(), PARTNER_ROUTE, before.contextSlot, "confirmed");
  const prepared = await prepareSignedV0Transaction({
    rpcUrl: rpcUrl(),
    feePayer: admin,
    instructions,
    prestateAddresses: protectedAddresses,
    inspectedAddresses: protectedAddresses,
    minimumContextSlot: Math.max(before.contextSlot, deploymentsBefore.contextSlot),
    commitment: "confirmed",
  });
  const postVault = prepared.simulation.postAccounts[1] ?? null;
  const postState = targetVaultState(postVault);
  const gates: Gate[] = [];
  add(gates, "approved admin signer exact", admin.signer.address === PARTNER_ROUTE.setupAdmin, admin.signer.address, PARTNER_ROUTE.setupAdmin);
  add(gates, "route hash exact", fourMarketRouteSpecSha256() === "df6547aeaba99f6bf32a0f56d63c50d30f84d7dc1d3df801266b97bd9811e8f4", fourMarketRouteSpecSha256(), "df6547aeaba99f6bf32a0f56d63c50d30f84d7dc1d3df801266b97bd9811e8f4");
  add(gates, "atomic activation has exactly three instructions", instructions.length === 3, instructions.length, 3);
  add(gates, "pre-activation vault is empty and HWM is initialized", beforeLpSupply === 0n && beforeState.totalValueRaw <= 1n && beforeState.highWaterMarkBits > 0n && beforeState.highWaterMarkUpdatedTs > 0n, { lpSupplyRaw: beforeLpSupply, totalValueRaw: beforeState.totalValueRaw, highWaterMarkBits: beforeState.highWaterMarkBits, highWaterMarkUpdatedTs: beforeState.highWaterMarkUpdatedTs }, { lpSupplyRaw: 0n, totalValueRaw: "<=1", initializedHighWaterMark: true });
  add(gates, "simulation succeeds", prepared.simulation.err === null, prepared.simulation.err, null);
  add(gates, "simulated vault reaches exact cap fee and pre-deposit HWM state", isActivated(postState, beforeLpSupply), postState, { capRaw: TARGET_CAP_RAW, adminPerformanceFeeBps: TARGET_ADMIN_PERFORMANCE_FEE_BPS, initializedHighWaterMark: true, lpSupplyRaw: 0n });
  add(gates, "packet is within Solana limit", prepared.packetBytes <= 1_232, prepared.packetBytes, "<=1232");
  add(gates, "quoted fee is within activation ceiling", prepared.feeLamports <= MAX_TOTAL_LAMPORTS, prepared.feeLamports, `<=${MAX_TOTAL_LAMPORTS}`);
  const deploymentsAfter = await loadDeploymentIdentities(rpcUrl(), PARTNER_ROUTE, prepared.simulationSlot, "confirmed");
  gates.push(...verifyDeploymentIdentities(PARTNER_ROUTE, deploymentsBefore.identities).map((gate) => ({ ...gate, name: `pre-activation deployment: ${gate.name}` })));
  gates.push(...verifyDeploymentIdentities(PARTNER_ROUTE, deploymentsAfter.identities).map((gate) => ({ ...gate, name: `post-simulation deployment: ${gate.name}` })));
  add(gates, "deployment identities remain exact", JSON.stringify(deploymentsBefore.identities, (_key, value) => typeof value === "bigint" ? value.toString() : value) === JSON.stringify(deploymentsAfter.identities, (_key, value) => typeof value === "bigint" ? value.toString() : value), deploymentsAfter.identities, deploymentsBefore.identities);
  const failedGateCount = gates.filter(({ pass }) => !pass).length;
  return {
    alreadyActivated: false,
    accounts,
    adminLpAta,
    admin,
    protectedAddresses,
    before,
    beforeState,
    postState,
    deploymentsBefore,
    prepared,
    report: {
      schemaVersion: 1,
      verdict: failedGateCount === 0 ? "PARTNER_VAULT_CONFIG_ACTIVATION_SIMULATION_PASS" : "PARTNER_VAULT_CONFIG_ACTIVATION_SIMULATION_FAIL",
      broadcast: false,
      readyForBroadcast: failedGateCount === 0,
      routeSpecSha256: fourMarketRouteSpecSha256(),
      vault: PARTNER_ROUTE.vault,
      lpMint: accounts.lpMint,
      adminLpAta,
      target: { vaultCapRaw: TARGET_CAP_RAW, adminPerformanceFeeBps: TARGET_ADMIN_PERFORMANCE_FEE_BPS },
      transaction: {
        instructionDataSha256: instructions.map((instruction) => sha256(instruction.data ?? new Uint8Array())),
        canonicalMessageSha256: sha256(prepared.serializedMessage),
        expectedSignature: prepared.expectedSignature,
        packetBytes: prepared.packetBytes,
        feeLamports: prepared.feeLamports,
        maxTotalLamports: MAX_TOTAL_LAMPORTS,
      },
      simulation: { prestateSlot: prepared.prestateSlot, contextSlot: prepared.simulationSlot, err: prepared.simulation.err, unitsConsumed: prepared.simulation.unitsConsumed, logs: prepared.simulation.logs },
      failedGateCount,
      gates,
    },
  } as const;
}

export async function simulateVaultConfigActivation() {
  const preparation = await buildPreparation();
  if (preparation.alreadyActivated) {
    return { schemaVersion: 1, verdict: "PARTNER_VAULT_CONFIG_ALREADY_ACTIVE", broadcast: false, readyForBroadcast: false, state: preparation.beforeState, contextSlot: preparation.beforeContextSlot } as const;
  }
  return preparation.report;
}

export async function executeVaultConfigActivation(input: Readonly<{
  confirmVault: string | null;
  confirmVaultCapRaw: string | null;
  confirmAdminPerformanceFeeBps: string | null;
  confirmRouteSpecSha256: string | null;
  confirmMaxTotalLamports: string | null;
}>) {
  if (process.env.CONFIRM_MAINNET !== "1") throw new Error("execute vault-config activation requires CONFIRM_MAINNET=1");
  if (input.confirmVault !== PARTNER_ROUTE.vault) throw new Error(`execute vault-config activation requires --confirm-vault ${PARTNER_ROUTE.vault}`);
  if (input.confirmVaultCapRaw !== TARGET_CAP_RAW.toString()) throw new Error(`execute vault-config activation requires --confirm-vault-cap-raw ${TARGET_CAP_RAW}`);
  if (input.confirmAdminPerformanceFeeBps !== TARGET_ADMIN_PERFORMANCE_FEE_BPS.toString()) throw new Error(`execute vault-config activation requires --confirm-admin-performance-fee-bps ${TARGET_ADMIN_PERFORMANCE_FEE_BPS}`);
  if (input.confirmRouteSpecSha256 !== fourMarketRouteSpecSha256()) throw new Error(`execute vault-config activation requires --confirm-route-spec-sha256 ${fourMarketRouteSpecSha256()}`);
  if (input.confirmMaxTotalLamports !== MAX_TOTAL_LAMPORTS.toString()) throw new Error(`execute vault-config activation requires --confirm-max-total-lamports ${MAX_TOTAL_LAMPORTS}`);
  const preparation = await buildPreparation();
  if (preparation.alreadyActivated) {
    return { schemaVersion: 1, verdict: "PARTNER_VAULT_CONFIG_ALREADY_ACTIVE", broadcast: false, state: preparation.beforeState, contextSlot: preparation.beforeContextSlot } as const;
  }
  if (!preparation.report.readyForBroadcast || preparation.report.failedGateCount !== 0) {
    throw new Error(`vault config activation preflight failed with ${preparation.report.verdict}`);
  }
  const refreshed = await confirmedSnapshots(rpcUrl(), preparation.protectedAddresses, preparation.prepared.simulationSlot);
  const changedExact = preparation.protectedAddresses.filter((_account, index) => JSON.stringify(snapshotFingerprint(refreshed.accounts[index] ?? null)) !== JSON.stringify(snapshotFingerprint(preparation.before.accounts[index] ?? null)));
  if (changedExact.length > 0) throw new Error(`vault activation protected state changed after simulation (${changedExact.join(", ")}); refusing send`);
  const deployments = await loadDeploymentIdentities(rpcUrl(), PARTNER_ROUTE, refreshed.contextSlot, "confirmed");
  if (!verifyDeploymentIdentities(PARTNER_ROUTE, deployments.identities).every(({ pass }) => pass)) throw new Error("deployment identity changed after activation simulation; refusing send");
  const authorizationContextSlot = Math.max(preparation.prepared.simulationSlot, refreshed.contextSlot, deployments.contextSlot);
  const confirmed = await sendPreparedConfirmedOnce(rpcUrl(), preparation.prepared, authorizationContextSlot);
  const readback = await confirmedSnapshots(rpcUrl(), preparation.protectedAddresses, confirmed.confirmedSlot);
  const finalState = targetVaultState(readback.accounts[1] ?? null);
  const finalLpSupply = readback.accounts[2] ? getMintDecoder().decode(readback.accounts[2]!.data).supply : -1n;
  const gates: Gate[] = [];
  add(gates, "confirmed transaction succeeded", confirmed.err === null, confirmed.err, null);
  add(gates, "confirmed vault reaches exact cap fee and pre-deposit HWM state", isActivated(finalState, finalLpSupply), { state: finalState, lpSupplyRaw: finalLpSupply }, { capRaw: TARGET_CAP_RAW, adminPerformanceFeeBps: TARGET_ADMIN_PERFORMANCE_FEE_BPS, initializedHighWaterMark: true, lpSupplyRaw: 0n });
  const payerDelta = confirmed.lamportDeltas.find(({ address: account }) => account === PARTNER_ROUTE.setupAdmin)?.deltaRaw ?? null;
  add(gates, "confirmed admin total spend is bounded", payerDelta !== null && -BigInt(payerDelta) >= 0n && -BigInt(payerDelta) <= BigInt(MAX_TOTAL_LAMPORTS), payerDelta, `debit <=${MAX_TOTAL_LAMPORTS}`);
  const failedGateCount = gates.filter(({ pass }) => !pass).length;
  return {
    schemaVersion: 1,
    verdict: failedGateCount === 0 ? "PARTNER_VAULT_CONFIG_ACTIVATION_CONFIRMED_AND_VERIFIED" : "PARTNER_VAULT_CONFIG_ACTIVATION_CONFIRMED_READBACK_FAIL",
    broadcast: true,
    authorizationContextSlot,
    preflight: preparation.report,
    confirmed,
    readback: { contextSlot: readback.contextSlot, state: finalState, failedGateCount, gates },
  } as const;
}
