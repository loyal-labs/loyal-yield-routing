import { createHash } from "node:crypto";

import { getInitializeVaultInstructionDataDecoder } from "@voltr/vault-sdk";
import { createNoopSigner } from "@solana/kit";

import {
  assertIntentForRoute,
  intentSha256,
  type SetupIntent,
} from "../domain/execution-intent.js";
import { PARTNER_ROUTE, routeSpecSha256 } from "../domain/route-spec.js";
import {
  finalizedSnapshots,
  loadDeploymentIdentities,
  loadMainReserveGraph,
  prepareSignedV0Transaction,
  sendPreparedOnce,
  type PreparedTransaction,
} from "../integrations/solana-compat.js";
import {
  derivePartnerVaultSigningMaterial,
  signingMaterialFromEnvironment,
} from "../integrations/signer.js";
import {
  createVoltrRouteBuilder,
  deriveVoltrAccounts,
} from "../integrations/voltr.js";
import {
  verifyAdaptorReceipt,
  verifyDeploymentIdentities,
  verifyVaultCurrentState,
  type Gate,
} from "../verify/current.js";
import { verifyPrecreatedSquadsIsolation } from "../verify/squads.js";

// Transaction approval is semantic and survives a blockhash refresh, but its
// SOL exposure must not. This is just above the observed 12,809,440-lamport
// fee-plus-rent requirement for the four-account atomic bootstrap.
const MAX_INIT_ADAPTOR_LAMPORTS = 12_900_000;

export type InitializeVaultPreparation = Readonly<{
  intent: SetupIntent;
  intentSha256: string;
  prepared: PreparedTransaction;
  report: Readonly<{
    verdict: "PARTNER_VAULT_INITIALIZE_SIMULATION_PASS" | "PARTNER_VAULT_INITIALIZE_SIMULATION_FAIL";
    broadcast: false;
    readyForBroadcast: boolean;
    routeSpecSha256: string;
    intentSha256: string;
    transaction: Readonly<{
      cluster: "mainnet-beta";
      operation: "initialize-vault";
      vault: string;
      manager: string;
      adminAndFeePayer: string;
      assetMint: string;
      waitingPeriodSeconds: string;
      expectedAssetMovementRaw: "0";
      packetBytes: number;
      feeLamports: number;
      createdAccountRentLamports: number;
      expectedSignature: string;
      instructionDataSha256: string;
      canonicalMessageSha256: string;
    }>;
    simulation: Readonly<{
      prestateSlot: number;
      contextSlot: number;
      err: unknown;
      unitsConsumed: number | null;
    }>;
    failedGateCount: number;
    gates: readonly Gate[];
  }>;
}>;

function add(
  gates: Gate[],
  name: string,
  pass: boolean,
  observed: unknown,
  expected: unknown,
): void {
  gates.push({ name, pass, observed, expected });
}

function rpcUrl(): string {
  const value = process.env.SOLANA_RPC_URL;
  if (!value) throw new Error("SOLANA_RPC_URL is required");
  return value;
}

async function loadSetupContext() {
  const route = PARTNER_ROUTE;
  const admin = await signingMaterialFromEnvironment("SOLANA_TESTING_PK");
  if (admin.signer.address !== route.setupAdmin) {
    throw new Error(`SOLANA_TESTING_PK ${admin.signer.address} is not RouteSpec setup admin`);
  }
  const vault = await derivePartnerVaultSigningMaterial(admin, route);
  const accounts = await deriveVoltrAccounts(route);
  const reserve = await loadMainReserveGraph(rpcUrl(), route, accounts.strategyAuth);
  const builder = await createVoltrRouteBuilder(route, reserve.graph);
  return { route, admin, vault, accounts, reserve, builder } as const;
}

async function unsignedInitializeAndAdaptorApproval() {
  const route = PARTNER_ROUTE;
  const accounts = await deriveVoltrAccounts(route);
  const reserve = await loadMainReserveGraph(rpcUrl(), route, accounts.strategyAuth);
  const builder = await createVoltrRouteBuilder(route, reserve.graph);
  const admin = createNoopSigner(route.setupAdmin);
  const vault = createNoopSigner(route.vault);
  const signers = { payer: admin, admin, vault };
  const initialize = await builder.setup.initializeVault(signers);
  const addAdaptor = await builder.setup.addAdaptor(signers);
  return {
    initializeDataSha256: initialize.canonical.dataSha256,
    addAdaptorDataSha256: addAdaptor.canonical.dataSha256,
  } as const;
}

/** Public-safe semantic envelope. It performs no signing or transaction send. */
export async function summarizeInitializeAndAdaptorApproval() {
  const approval = await unsignedInitializeAndAdaptorApproval();
  return {
    verdict: "PARTNER_VAULT_INIT_ADAPTOR_APPROVAL_SUMMARY",
    broadcast: false,
    signerLoaded: false,
    cluster: "mainnet-beta",
    operation: "initialize-vault-and-adaptor",
    routeSpecSha256: routeSpecSha256(PARTNER_ROUTE),
    vault: PARTNER_ROUTE.vault,
    manager: PARTNER_ROUTE.squads.manager,
    setupAdmin: PARTNER_ROUTE.setupAdmin,
    waitingPeriodSeconds: PARTNER_ROUTE.vaultConfiguration.withdrawalWaitingPeriodSeconds,
    expectedAssetMovementRaw: 0,
    maxTotalLamports: MAX_INIT_ADAPTOR_LAMPORTS,
    initializeDataSha256: approval.initializeDataSha256,
    addAdaptorDataSha256: approval.addAdaptorDataSha256,
    approvalPhrase: `I approve the exact ${PARTNER_ROUTE.vaultConfiguration.withdrawalWaitingPeriodSeconds}-second init+adaptor transaction with a ${MAX_INIT_ADAPTOR_LAMPORTS.toLocaleString("en-US")}-lamport maximum.`,
  } as const;
}

async function prepareInitializeVault(): Promise<InitializeVaultPreparation> {
  const { route, admin, vault, accounts, builder } = await loadSetupContext();
  const instruction = await builder.setup.initializeVault({
    payer: admin.signer,
    admin: admin.signer,
    vault: vault.signer,
  });
  const inspectedAddresses = [
    route.setupAdmin,
    route.vault,
    accounts.lpMint,
    accounts.idleAta,
    route.asset.mint,
    accounts.adaptorAddReceipt,
  ];
  const before = await finalizedSnapshots(rpcUrl(), inspectedAddresses);
  const prepared = await prepareSignedV0Transaction({
    rpcUrl: rpcUrl(),
    feePayer: admin,
    additionalSigners: [vault],
    instructions: [instruction.raw],
    inspectedAddresses,
  });
  const post = new Map(
    inspectedAddresses.map((account, index) => [account, prepared.simulation.postAccounts[index] ?? null]),
  );
  const vaultState = verifyVaultCurrentState({
    route,
    accounts,
    vault: post.get(route.vault) ?? null,
    lpMint: post.get(accounts.lpMint) ?? null,
    idleAta: post.get(accounts.idleAta) ?? null,
    assetMint: post.get(route.asset.mint) ?? null,
    requireEmpty: true,
  });
  const decodedInstruction = getInitializeVaultInstructionDataDecoder().decode(
    instruction.canonical.data,
  );
  const createdAccountRentLamports = [route.vault, accounts.lpMint, accounts.idleAta]
    .reduce((sum, account) => sum + (post.get(account)?.lamports ?? 0), 0);
  const gates: Gate[] = [];
  add(gates, "candidate vault and derived accounts are absent", before.accounts.slice(1, 4).every((account) => account === null), before.accounts.slice(1, 4).map((account) => account?.address ?? null), [null, null, null]);
  add(gates, "adaptor receipt is absent", before.accounts[5] === null, before.accounts[5]?.address ?? null, null);
  add(gates, "one canonical Voltr instruction", instruction.canonical.programId === route.programs.voltrVault && instruction.canonical.accounts.length === 16, { programId: instruction.canonical.programId, accountCount: instruction.canonical.accounts.length }, { programId: route.programs.voltrVault, accountCount: 16 });
  add(gates, "withdrawal waiting period encoded from RouteSpec", decodedInstruction.withdrawalWaitingPeriod === route.vaultConfiguration.withdrawalWaitingPeriodSeconds, decodedInstruction.withdrawalWaitingPeriod, route.vaultConfiguration.withdrawalWaitingPeriodSeconds);
  add(gates, "simulation succeeded", prepared.simulation.err === null, prepared.simulation.err, null);
  add(gates, "simulated initialized vault is exact and empty", vaultState.verdict === "PARTNER_VAULT_CURRENT_PASS", { verdict: vaultState.verdict, failedGateCount: vaultState.failedGateCount }, { verdict: "PARTNER_VAULT_CURRENT_PASS", failedGateCount: 0 });
  add(gates, "zero USDC movement", vaultState.state?.idleRaw === 0n && vaultState.state.totalValueRaw === 0n, vaultState.state ? { idleRaw: vaultState.state.idleRaw, totalValueRaw: vaultState.state.totalValueRaw } : null, { idleRaw: 0n, totalValueRaw: 0n });
  add(gates, "rent plus fee bounded", createdAccountRentLamports + prepared.feeLamports <= 20_000_000, createdAccountRentLamports + prepared.feeLamports, "<= 20000000");
  gates.push(...vaultState.gates.map((gate) => ({ ...gate, name: `simulated vault: ${gate.name}` })));
  const canonicalMessageSha256 = createHash("sha256")
    .update(prepared.serializedMessage)
    .digest("hex");
  const intent: SetupIntent = {
    schemaVersion: 1,
    kind: "setup",
    operation: "initialize-vault",
    routeId: route.id,
    routeSpecSha256: routeSpecSha256(route),
    signer: route.setupAdmin,
    nonce: `initialize-vault:${route.vault}`,
    prestateSlot: BigInt(prepared.prestateSlot),
    expiresAtUnix: BigInt(Math.floor(Date.now() / 1_000) + 300),
    canonicalMessageSha256,
  };
  assertIntentForRoute(intent, route);
  const intentDigest = intentSha256(intent);
  const failedGateCount = gates.filter(({ pass }) => !pass).length;
  return {
    intent,
    intentSha256: intentDigest,
    prepared,
    report: {
      verdict: failedGateCount === 0
        ? "PARTNER_VAULT_INITIALIZE_SIMULATION_PASS"
        : "PARTNER_VAULT_INITIALIZE_SIMULATION_FAIL",
      broadcast: false,
      readyForBroadcast: failedGateCount === 0,
      routeSpecSha256: routeSpecSha256(route),
      intentSha256: intentDigest,
      transaction: {
        cluster: "mainnet-beta",
        operation: "initialize-vault",
        vault: route.vault,
        manager: route.squads.manager,
        adminAndFeePayer: route.setupAdmin,
        assetMint: route.asset.mint,
        waitingPeriodSeconds: route.vaultConfiguration.withdrawalWaitingPeriodSeconds.toString(),
        expectedAssetMovementRaw: "0",
        packetBytes: prepared.packetBytes,
        feeLamports: prepared.feeLamports,
        createdAccountRentLamports,
        expectedSignature: prepared.expectedSignature,
        instructionDataSha256: instruction.canonical.dataSha256,
        canonicalMessageSha256,
      },
      simulation: {
        prestateSlot: prepared.prestateSlot,
        contextSlot: prepared.simulationSlot,
        err: prepared.simulation.err,
        unitsConsumed: prepared.simulation.unitsConsumed,
      },
      failedGateCount,
      gates,
    },
  };
}

export async function simulateInitializeVault() {
  return (await prepareInitializeVault()).report;
}

async function prepareAddAdaptor() {
  const { route, admin, vault, accounts, builder } = await loadSetupContext();
  const instruction = await builder.setup.addAdaptor({
    payer: admin.signer,
    admin: admin.signer,
    vault: vault.signer,
  });
  const inspectedAddresses = [
    route.vault,
    accounts.lpMint,
    accounts.idleAta,
    route.asset.mint,
    accounts.adaptorAddReceipt,
  ];
  const before = await finalizedSnapshots(rpcUrl(), inspectedAddresses);
  const current = verifyVaultCurrentState({
    route,
    accounts,
    vault: before.accounts[0] ?? null,
    lpMint: before.accounts[1] ?? null,
    idleAta: before.accounts[2] ?? null,
    assetMint: before.accounts[3] ?? null,
    requireEmpty: true,
  });
  const prepared = await prepareSignedV0Transaction({
    rpcUrl: rpcUrl(),
    feePayer: admin,
    instructions: [instruction.raw],
    inspectedAddresses,
  });
  const post = new Map(
    inspectedAddresses.map((account, index) => [account, prepared.simulation.postAccounts[index] ?? null]),
  );
  const simulatedVault = verifyVaultCurrentState({
    route,
    accounts,
    vault: post.get(route.vault) ?? null,
    lpMint: post.get(accounts.lpMint) ?? null,
    idleAta: post.get(accounts.idleAta) ?? null,
    assetMint: post.get(route.asset.mint) ?? null,
    requireEmpty: true,
  });
  const receiptGates = verifyAdaptorReceipt(
    route,
    accounts.adaptorAddReceipt,
    post.get(accounts.adaptorAddReceipt) ?? null,
  );
  const gates: Gate[] = [];
  add(gates, "finalized vault is exact and empty", current.verdict === "PARTNER_VAULT_CURRENT_PASS", current.verdict, "PARTNER_VAULT_CURRENT_PASS");
  add(gates, "adaptor receipt is absent", before.accounts[4] === null, before.accounts[4]?.address ?? null, null);
  add(gates, "one canonical add-adaptor instruction", instruction.canonical.programId === route.programs.voltrVault && instruction.canonical.accounts.length === 7, { programId: instruction.canonical.programId, accountCount: instruction.canonical.accounts.length }, { programId: route.programs.voltrVault, accountCount: 7 });
  add(gates, "simulation succeeded", prepared.simulation.err === null, prepared.simulation.err, null);
  add(gates, "simulated vault remains exact and empty", simulatedVault.verdict === "PARTNER_VAULT_CURRENT_PASS", simulatedVault.verdict, "PARTNER_VAULT_CURRENT_PASS");
  add(gates, "vault bytes unchanged", simulatedVault.state?.vaultDataSha256 === current.state?.vaultDataSha256, simulatedVault.state?.vaultDataSha256 ?? null, current.state?.vaultDataSha256 ?? null);
  gates.push(...receiptGates.map((gate) => ({ ...gate, name: `simulated adaptor: ${gate.name}` })));
  const receiptRentLamports = post.get(accounts.adaptorAddReceipt)?.lamports ?? 0;
  add(gates, "rent plus fee bounded", receiptRentLamports + prepared.feeLamports <= 5_000_000, receiptRentLamports + prepared.feeLamports, "<= 5000000");
  const canonicalMessageSha256 = createHash("sha256").update(prepared.serializedMessage).digest("hex");
  const intent: SetupIntent = {
    schemaVersion: 1,
    kind: "setup",
    operation: "add-adaptor",
    routeId: route.id,
    routeSpecSha256: routeSpecSha256(route),
    signer: route.setupAdmin,
    nonce: `add-adaptor:${accounts.adaptorAddReceipt}`,
    prestateSlot: BigInt(prepared.prestateSlot),
    expiresAtUnix: BigInt(Math.floor(Date.now() / 1_000) + 300),
    canonicalMessageSha256,
  };
  assertIntentForRoute(intent, route);
  const intentDigest = intentSha256(intent);
  const failedGateCount = gates.filter(({ pass }) => !pass).length;
  return {
    intent,
    intentSha256: intentDigest,
    prepared,
    report: {
      verdict: failedGateCount === 0 ? "PARTNER_ADAPTOR_SIMULATION_PASS" : "PARTNER_ADAPTOR_SIMULATION_FAIL",
      broadcast: false,
      readyForBroadcast: failedGateCount === 0,
      routeSpecSha256: routeSpecSha256(route),
      intentSha256: intentDigest,
      transaction: {
        cluster: "mainnet-beta",
        operation: "add-adaptor",
        vault: route.vault,
        adaptor: route.programs.kaminoAdaptor,
        adaptorReceipt: accounts.adaptorAddReceipt,
        adminAndFeePayer: route.setupAdmin,
        expectedAssetMovementRaw: "0",
        packetBytes: prepared.packetBytes,
        feeLamports: prepared.feeLamports,
        createdAccountRentLamports: receiptRentLamports,
        expectedSignature: prepared.expectedSignature,
        instructionDataSha256: instruction.canonical.dataSha256,
        canonicalMessageSha256,
        prestateVaultDataSha256: current.state?.vaultDataSha256 ?? null,
      },
      simulation: {
        prestateSlot: prepared.prestateSlot,
        contextSlot: prepared.simulationSlot,
        err: prepared.simulation.err,
        unitsConsumed: prepared.simulation.unitsConsumed,
      },
      failedGateCount,
      gates,
    },
  } as const;
}

export async function simulateAddAdaptor() {
  return (await prepareAddAdaptor()).report;
}

async function prepareInitializeAndAdaptor() {
  const { route, admin, vault, accounts, builder } = await loadSetupContext();
  const signers = { payer: admin.signer, admin: admin.signer, vault: vault.signer };
  const initialize = await builder.setup.initializeVault(signers);
  const addAdaptor = await builder.setup.addAdaptor(signers);
  const inspectedAddresses = [
    route.setupAdmin,
    route.vault,
    accounts.lpMint,
    accounts.idleAta,
    accounts.adaptorAddReceipt,
    route.asset.mint,
  ];
  const before = await finalizedSnapshots(rpcUrl(), inspectedAddresses);
  const deployments = await loadDeploymentIdentities(rpcUrl(), route, before.contextSlot);
  const squadsIsolation = await verifyPrecreatedSquadsIsolation(rpcUrl(), route.squads.policySeedBefore);
  const prepared = await prepareSignedV0Transaction({
    rpcUrl: rpcUrl(),
    feePayer: admin,
    additionalSigners: [vault],
    instructions: [initialize.raw, addAdaptor.raw],
    inspectedAddresses,
  });
  const post = new Map(
    inspectedAddresses.map((account, index) => [account, prepared.simulation.postAccounts[index] ?? null]),
  );
  const vaultState = verifyVaultCurrentState({
    route,
    accounts,
    vault: post.get(route.vault) ?? null,
    lpMint: post.get(accounts.lpMint) ?? null,
    idleAta: post.get(accounts.idleAta) ?? null,
    assetMint: post.get(route.asset.mint) ?? null,
    requireEmpty: true,
  });
  const gates: Gate[] = [];
  add(gates, "all setup accounts absent before diagnostic", before.accounts.slice(1, 5).every((account) => account === null), before.accounts.slice(1, 5).map((account) => account?.address ?? null), [null, null, null, null]);
  add(gates, "pre-created Squads boundary is isolated", squadsIsolation.verdict === "PARTNER_PRECREATED_SQUADS_ISOLATION_PASS", { verdict: squadsIsolation.verdict, failedGateCount: squadsIsolation.failedGateCount }, { verdict: "PARTNER_PRECREATED_SQUADS_ISOLATION_PASS", failedGateCount: 0 });
  gates.push(...verifyDeploymentIdentities(route, deployments.identities, [route.programs.voltrVault, route.programs.kaminoAdaptor]).map((gate) => ({ ...gate, name: `setup deployment: ${gate.name}` })));
  add(gates, "two ordered SDK-built instructions", initialize.canonical.programId === route.programs.voltrVault && addAdaptor.canonical.programId === route.programs.voltrVault, [initialize.canonical.programId, addAdaptor.canonical.programId], [route.programs.voltrVault, route.programs.voltrVault]);
  add(gates, "diagnostic simulation succeeded", prepared.simulation.err === null, prepared.simulation.err, null);
  add(gates, "simulated vault remains exact and empty", vaultState.verdict === "PARTNER_VAULT_CURRENT_PASS", vaultState.verdict, "PARTNER_VAULT_CURRENT_PASS");
  gates.push(...verifyAdaptorReceipt(route, accounts.adaptorAddReceipt, post.get(accounts.adaptorAddReceipt) ?? null));
  gates.push(...vaultState.gates.map((gate) => ({ ...gate, name: `simulated vault: ${gate.name}` })));
  const createdAccountRentLamports = [route.vault, accounts.lpMint, accounts.idleAta, accounts.adaptorAddReceipt]
    .reduce((sum, account) => sum + (post.get(account)?.lamports ?? 0), 0);
  add(gates, "rent plus fee bounded by approved atomic-bootstrap ceiling", createdAccountRentLamports + prepared.feeLamports <= MAX_INIT_ADAPTOR_LAMPORTS, createdAccountRentLamports + prepared.feeLamports, `<= ${MAX_INIT_ADAPTOR_LAMPORTS}`);
  const canonicalMessageSha256 = createHash("sha256").update(prepared.serializedMessage).digest("hex");
  const intent: SetupIntent = {
    schemaVersion: 1,
    kind: "setup",
    operation: "initialize-vault-and-adaptor",
    routeId: route.id,
    routeSpecSha256: routeSpecSha256(route),
    signer: route.setupAdmin,
    nonce: `initialize-vault-and-adaptor:${route.vault}`,
    prestateSlot: BigInt(prepared.prestateSlot),
    expiresAtUnix: BigInt(Math.floor(Date.now() / 1_000) + 300),
    canonicalMessageSha256,
  };
  assertIntentForRoute(intent, route);
  const intentDigest = intentSha256(intent);
  const failedGateCount = gates.filter(({ pass }) => !pass).length;
  const report = {
    verdict: failedGateCount === 0
      ? "PARTNER_VAULT_INIT_ADAPTOR_SIMULATION_PASS"
      : "PARTNER_VAULT_INIT_ADAPTOR_SIMULATION_FAIL",
    broadcast: false,
    readyForBroadcast: failedGateCount === 0,
    routeSpecSha256: routeSpecSha256(route),
    intentSha256: intentDigest,
    transaction: {
      instructionSequence: ["initialize-vault", "add-adaptor"],
      vault: route.vault,
      manager: route.squads.manager,
      adminAndFeePayer: route.setupAdmin,
      adaptor: route.programs.kaminoAdaptor,
      adaptorReceipt: accounts.adaptorAddReceipt,
      waitingPeriodSeconds: route.vaultConfiguration.withdrawalWaitingPeriodSeconds,
      packetBytes: prepared.packetBytes,
      feeLamports: prepared.feeLamports,
      createdAccountRentLamports,
      maxTotalLamports: MAX_INIT_ADAPTOR_LAMPORTS,
      expectedAssetMovementRaw: 0,
      expectedSignature: prepared.expectedSignature,
      initializeDataSha256: initialize.canonical.dataSha256,
      addAdaptorDataSha256: addAdaptor.canonical.dataSha256,
      canonicalMessageSha256,
    },
    simulation: {
      prestateSlot: prepared.prestateSlot,
      contextSlot: prepared.simulationSlot,
      err: prepared.simulation.err,
      unitsConsumed: prepared.simulation.unitsConsumed,
    },
    deployments: deployments.identities.filter(({ programId }) => programId === route.programs.voltrVault || programId === route.programs.kaminoAdaptor),
    squadsIsolation: { contextSlot: squadsIsolation.contextSlot, activeLegacyPolicies: squadsIsolation.activeLegacyPolicies },
    failedGateCount,
    gates,
  } as const;
  return { intent, intentSha256: intentDigest, prepared, report } as const;
}

export async function simulateInitializeAndAdaptorDiagnostic() {
  return (await prepareInitializeAndAdaptor()).report;
}

export async function executeInitializeAndAdaptor(input: Readonly<{
  confirmVault: string | null;
  confirmRouteSpecSha256: string | null;
  confirmInitializeDataSha256: string | null;
  confirmAddAdaptorDataSha256: string | null;
  confirmMaxTotalLamports: string | null;
}>) {
  if (process.env.CONFIRM_MAINNET !== "1") {
    throw new Error("execute initialize-vault-and-adaptor requires CONFIRM_MAINNET=1");
  }
  if (input.confirmVault !== PARTNER_ROUTE.vault) {
    throw new Error(`execute initialize-vault-and-adaptor requires --confirm-vault ${PARTNER_ROUTE.vault}`);
  }
  const expectedRouteSha256 = routeSpecSha256(PARTNER_ROUTE);
  if (input.confirmRouteSpecSha256 !== expectedRouteSha256) {
    throw new Error(`execute initialize-vault-and-adaptor requires --confirm-route-spec-sha256 ${expectedRouteSha256}`);
  }
  if (input.confirmMaxTotalLamports !== MAX_INIT_ADAPTOR_LAMPORTS.toString()) {
    throw new Error(`execute initialize-vault-and-adaptor requires --confirm-max-total-lamports ${MAX_INIT_ADAPTOR_LAMPORTS}`);
  }
  const unsignedApproval = await unsignedInitializeAndAdaptorApproval();
  if (
    input.confirmInitializeDataSha256 !== unsignedApproval.initializeDataSha256
    || input.confirmAddAdaptorDataSha256 !== unsignedApproval.addAdaptorDataSha256
  ) {
    throw new Error(`execute initialize-vault-and-adaptor requires --confirm-initialize-data-sha256 ${unsignedApproval.initializeDataSha256} --confirm-add-adaptor-data-sha256 ${unsignedApproval.addAdaptorDataSha256}`);
  }
  const preparation = await prepareInitializeAndAdaptor();
  if (
    input.confirmInitializeDataSha256 !== preparation.report.transaction.initializeDataSha256
    || input.confirmAddAdaptorDataSha256 !== preparation.report.transaction.addAdaptorDataSha256
  ) {
    throw new Error(`execute initialize-vault-and-adaptor requires --confirm-initialize-data-sha256 ${preparation.report.transaction.initializeDataSha256} --confirm-add-adaptor-data-sha256 ${preparation.report.transaction.addAdaptorDataSha256}`);
  }
  if (!preparation.report.readyForBroadcast || preparation.report.failedGateCount !== 0) {
    throw new Error(`atomic initialization preflight failed with ${preparation.report.verdict}`);
  }
  const accounts = await deriveVoltrAccounts(PARTNER_ROUTE);
  const protectedAddresses = [
    PARTNER_ROUTE.vault,
    accounts.lpMint,
    accounts.idleAta,
    accounts.adaptorAddReceipt,
  ];
  const refreshed = await finalizedSnapshots(
    rpcUrl(),
    protectedAddresses,
    preparation.prepared.simulationSlot,
  );
  if (refreshed.accounts.some((account) => account !== null)) {
    throw new Error("atomic initialization protected accounts changed after simulation; refusing send");
  }
  const deployments = await loadDeploymentIdentities(rpcUrl(), PARTNER_ROUTE, refreshed.contextSlot);
  if (!verifyDeploymentIdentities(PARTNER_ROUTE, deployments.identities, [PARTNER_ROUTE.programs.voltrVault, PARTNER_ROUTE.programs.kaminoAdaptor]).every(({ pass }) => pass)) {
    throw new Error("atomic initialization deployment identity changed after simulation; refusing send");
  }
  const squadsIsolation = await verifyPrecreatedSquadsIsolation(rpcUrl(), PARTNER_ROUTE.squads.policySeedBefore, refreshed.contextSlot);
  if (squadsIsolation.verdict !== "PARTNER_PRECREATED_SQUADS_ISOLATION_PASS") {
    throw new Error("pre-created Squads authority changed after simulation; refusing send");
  }
  const authorizationContextSlot = Math.max(
    preparation.prepared.simulationSlot,
    refreshed.contextSlot,
    deployments.contextSlot,
    squadsIsolation.contextSlot,
  );
  let finalized: Awaited<ReturnType<typeof sendPreparedOnce>> | null = null;
  try {
    finalized = await sendPreparedOnce(rpcUrl(), preparation.prepared, authorizationContextSlot);
    if (finalized.err !== null) {
      return {
        verdict: "PARTNER_VAULT_INIT_ADAPTOR_FINALIZED_WITH_ERROR",
        broadcast: true,
        intent: preparation.intent,
        intentSha256: preparation.intentSha256,
        preflight: preparation.report,
        finalized,
      } as const;
    }
    const state = await finalizedSnapshots(
      rpcUrl(),
      [PARTNER_ROUTE.vault, accounts.lpMint, accounts.idleAta, PARTNER_ROUTE.asset.mint, accounts.adaptorAddReceipt],
      finalized.finalizedSlot,
    );
    const vaultReadback = verifyVaultCurrentState({
      route: PARTNER_ROUTE,
      accounts,
      vault: state.accounts[0] ?? null,
      lpMint: state.accounts[1] ?? null,
      idleAta: state.accounts[2] ?? null,
      assetMint: state.accounts[3] ?? null,
      requireEmpty: true,
    });
    const receiptGates = verifyAdaptorReceipt(PARTNER_ROUTE, accounts.adaptorAddReceipt, state.accounts[4] ?? null);
    const failedGateCount = vaultReadback.failedGateCount + receiptGates.filter(({ pass }) => !pass).length;
    return {
      verdict: failedGateCount === 0
        ? "PARTNER_VAULT_INIT_ADAPTOR_FINALIZED_AND_VERIFIED"
        : "PARTNER_VAULT_INIT_ADAPTOR_FINALIZED_READBACK_FAIL",
      broadcast: true,
      intent: preparation.intent,
      intentSha256: preparation.intentSha256,
      preflight: preparation.report,
      finalized,
      readbackContextSlot: state.contextSlot,
      readback: { vault: vaultReadback, adaptorReceipt: receiptGates, failedGateCount },
    } as const;
  } catch (error) {
    if (finalized) {
      return {
        verdict: "PARTNER_VAULT_INIT_ADAPTOR_FINALIZED_READBACK_ERROR",
        broadcast: true,
        intent: preparation.intent,
        intentSha256: preparation.intentSha256,
        preflight: preparation.report,
        finalized,
        error: error instanceof Error ? error.message : String(error),
        recoveryInstruction: "Do not resend. The transaction is finalized; rerun read-only vault/adaptor reconciliation.",
      } as const;
    }
    return {
      verdict: "PARTNER_VAULT_INIT_ADAPTOR_BROADCAST_STATUS_UNKNOWN",
      broadcast: null,
      expectedSignature: preparation.prepared.expectedSignature,
      intent: preparation.intent,
      intentSha256: preparation.intentSha256,
      preflight: preparation.report,
      error: error instanceof Error ? error.message : String(error),
      recoveryInstruction: "Do not resend. Verify this exact signature and reload all candidate vault accounts.",
    } as const;
  }
}
