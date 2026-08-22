import { createHash } from "node:crypto";

import { findAssociatedTokenPda, getCreateAssociatedTokenIdempotentInstructionAsync, getTokenDecoder } from "@solana-program/token";
import { address, isSignerRole, isWritableRole, type Instruction } from "@solana/kit";

import { assertIntentForRoute, intentSha256, type SetupIntent } from "../domain/execution-intent.js";
import {
  loadBootstrapExecutionAuthorization,
  operationAuthorization,
} from "../domain/bootstrap-execution-authorization.js";
import {
  PARTNER_ROUTE,
  fourMarketRouteSpecSha256,
  partnerBuilderRoute,
  partnerStrategyGraphSha256,
  partnerStrategyIdentity,
  routeSpecSha256,
  type PartnerStrategyId,
} from "../domain/route-spec.js";
import {
  confirmedSnapshots,
  loadDeploymentIdentities,
  prepareSignedV0Transaction,
  rentExemptionLamports,
  sendPreparedConfirmedOnce,
  type AccountSnapshot,
  type PreparedTransaction,
} from "../integrations/solana-compat.js";
import { signingMaterialFromEnvironment } from "../integrations/signer.js";
import { deriveVoltrAccounts } from "../integrations/voltr.js";
import { verifyDeploymentIdentities, verifyStrategyBootstrap, verifyVaultCurrentState, type Gate } from "../verify/current.js";

const TOKEN_ACCOUNT_DATA_LENGTH = 165;
const MAX_STRATEGY_ASSET_ATA_LAMPORTS = 3_000_000;

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

function sameJson(left: unknown, right: unknown): boolean {
  return JSON.stringify(left, (_key, value) => typeof value === "bigint" ? value.toString() : value)
    === JSON.stringify(right, (_key, value) => typeof value === "bigint" ? value.toString() : value);
}

function fingerprint(snapshot: AccountSnapshot | null): unknown {
  return snapshot === null ? null : {
    address: snapshot.address,
    owner: snapshot.owner,
    lamports: snapshot.lamports,
    executable: snapshot.executable,
    dataSha256: sha256(snapshot.data),
  };
}

function snapshotMap(addresses: readonly string[], accounts: readonly (AccountSnapshot | null)[]): Map<string, AccountSnapshot | null> {
  return new Map(addresses.map((value, index) => [value, accounts[index] ?? null]));
}

function ataState(snapshot: AccountSnapshot | null): { mint: string; owner: string; amount: bigint } | null {
  if (!snapshot || snapshot.owner !== PARTNER_ROUTE.programs.token || snapshot.data.length !== TOKEN_ACCOUNT_DATA_LENGTH) return null;
  const decoded = getTokenDecoder().decode(snapshot.data);
  return { mint: decoded.mint, owner: decoded.owner, amount: decoded.amount };
}

function instructionShape(instruction: Instruction, ata: string, owner: string): Gate[] {
  const gates: Gate[] = [];
  const data = instruction.data ?? new Uint8Array();
  const accounts = (instruction.accounts ?? []).map((meta) => ({
    address: meta.address,
    signer: isSignerRole(meta.role),
    writable: isWritableRole(meta.role),
  }));
  const expected = [
    { address: PARTNER_ROUTE.setupAdmin, signer: true, writable: true },
    { address: ata, signer: false, writable: true },
    { address: owner, signer: false, writable: false },
    { address: PARTNER_ROUTE.asset.mint, signer: false, writable: false },
    { address: PARTNER_ROUTE.programs.system, signer: false, writable: false },
    { address: PARTNER_ROUTE.programs.token, signer: false, writable: false },
  ];
  add(gates, "one canonical idempotent ATA instruction", instruction.programAddress === PARTNER_ROUTE.programs.associatedToken && data.length === 1 && data[0] === 1, { programId: instruction.programAddress, dataHex: Buffer.from(data).toString("hex") }, { programId: PARTNER_ROUTE.programs.associatedToken, dataHex: "01" });
  add(gates, "strategy USDC ATA instruction accounts exact", sameJson(accounts, expected), accounts, expected);
  return gates;
}

async function buildPreparation(
  strategyId: PartnerStrategyId,
  minimumContextSlot?: number,
) {
  const route = partnerBuilderRoute(strategyId);
  const identity = partnerStrategyIdentity(strategyId);
  const admin = await signingMaterialFromEnvironment("SOLANA_TESTING_PK");
  if (admin.signer.address !== route.setupAdmin) throw new Error(`SOLANA_TESTING_PK ${admin.signer.address} is not RouteSpec setup admin`);
  const accounts = await deriveVoltrAccounts(route);
  const [strategyAssetAta] = await findAssociatedTokenPda({ owner: accounts.strategyAuth, mint: route.asset.mint, tokenProgram: route.programs.token }, { programAddress: route.programs.associatedToken });
  if (accounts.strategyAuth !== identity.voltr.strategyAuth || accounts.strategyInitReceipt !== identity.voltr.strategyInitReceipt || strategyAssetAta !== identity.voltr.strategyAssetAta) throw new Error(`${strategyId} derived Voltr accounts do not match the frozen catalog`);
  const graph = { reserve: identity.reserve, ...identity.graph } as const;
  const protectedAddresses = [route.setupAdmin, accounts.strategyAuth, strategyAssetAta, route.asset.mint, route.vault, accounts.lpMint, accounts.idleAta, accounts.strategyInitReceipt, graph.userMetadata, graph.obligation, graph.obligationFarm];
  const simulatedAddresses = [route.setupAdmin, strategyAssetAta] as const;
  const beforeResponse = await confirmedSnapshots(rpcUrl(), protectedAddresses, minimumContextSlot);
  const before = snapshotMap(protectedAddresses, beforeResponse.accounts);
  const vault = verifyVaultCurrentState({ route, accounts, vault: before.get(route.vault) ?? null, lpMint: before.get(accounts.lpMint) ?? null, idleAta: before.get(accounts.idleAta) ?? null, assetMint: before.get(route.asset.mint) ?? null });
  const strategyGates = verifyStrategyBootstrap({ route, accounts, graph, strategyReceipt: before.get(accounts.strategyInitReceipt) ?? null, userMetadata: before.get(graph.userMetadata) ?? null, obligation: before.get(graph.obligation) ?? null, obligationFarm: before.get(graph.obligationFarm) ?? null });
  const deploymentsBefore = await loadDeploymentIdentities(rpcUrl(), route, beforeResponse.contextSlot, "confirmed");
  const instruction = await getCreateAssociatedTokenIdempotentInstructionAsync({ payer: admin.signer, ata: address(strategyAssetAta), owner: accounts.strategyAuth, mint: route.asset.mint, systemProgram: route.programs.system, tokenProgram: route.programs.token }, { programAddress: route.programs.associatedToken });
  const instructionData = instruction.data ?? new Uint8Array();
  // Some mainnet RPCs cap simulateTransaction account-return images below the
  // full protected set. The exact ATA packet has only two writable accounts,
  // so simulate those two and retain a separate confirmed snapshot for every
  // protected readback account.
  const prepared = await prepareSignedV0Transaction({ rpcUrl: rpcUrl(), feePayer: admin, instructions: [instruction], inspectedAddresses: simulatedAddresses, minimumContextSlot: beforeResponse.contextSlot, commitment: "confirmed" });
  const post = snapshotMap(simulatedAddresses, prepared.simulation.postAccounts);
  const deploymentsAfter = await loadDeploymentIdentities(rpcUrl(), route, prepared.simulationSlot, "confirmed");
  const ataRentLamports = await rentExemptionLamports(rpcUrl(), TOKEN_ACCOUNT_DATA_LENGTH);
  const ataAfter = ataState(post.get(strategyAssetAta) ?? null);
  const gates: Gate[] = [];
  gates.push(...verifyDeploymentIdentities(route, deploymentsBefore.identities));
  gates.push(...verifyDeploymentIdentities(route, deploymentsAfter.identities).map((gate) => ({ ...gate, name: `simulated deployment: ${gate.name}` })));
  add(gates, "deployment identities unchanged across ATA setup", sameJson(deploymentsBefore.identities, deploymentsAfter.identities), deploymentsAfter.identities, deploymentsBefore.identities);
  gates.push(...vault.gates.map((gate) => ({ ...gate, name: `vault: ${gate.name}` })));
  gates.push(...strategyGates.map((gate) => ({ ...gate, name: `strategy bootstrap: ${gate.name}` })));
  add(gates, "strategy USDC ATA is absent before setup", before.get(strategyAssetAta) === null, before.get(strategyAssetAta)?.address ?? null, null);
  gates.push(...instructionShape(instruction, strategyAssetAta, accounts.strategyAuth));
  add(gates, "simulation succeeds", prepared.simulation.err === null, prepared.simulation.err, null);
  add(gates, "ATA packet within Solana limit", prepared.packetBytes <= 1_232, prepared.packetBytes, "<=1232");
  const adminBefore = before.get(route.setupAdmin)?.lamports ?? null;
  const adminAfter = post.get(route.setupAdmin)?.lamports ?? null;
  const adminSpend = adminBefore !== null && adminAfter !== null ? adminBefore - adminAfter : null;
  add(gates, "simulated ATA has exact owner/mint/zero balance", ataAfter !== null && ataAfter.owner === accounts.strategyAuth && ataAfter.mint === route.asset.mint && ataAfter.amount === 0n, ataAfter, { owner: accounts.strategyAuth, mint: route.asset.mint, amount: 0n });
  add(gates, "simulated ATA has exact rent exemption", post.get(strategyAssetAta)?.lamports === ataRentLamports, post.get(strategyAssetAta)?.lamports ?? null, ataRentLamports);
  add(gates, "setup admin spend is exactly fee plus ATA rent", adminSpend !== null && adminSpend === prepared.feeLamports + ataRentLamports, adminSpend, prepared.feeLamports + ataRentLamports);
  add(gates, "ATA setup fee plus rent is within approved ceiling", prepared.feeLamports + ataRentLamports <= MAX_STRATEGY_ASSET_ATA_LAMPORTS, prepared.feeLamports + ataRentLamports, `<=${MAX_STRATEGY_ASSET_ATA_LAMPORTS}`);
  const canonicalMessageSha256 = sha256(prepared.serializedMessage);
  const intent: SetupIntent = { schemaVersion: 1, kind: "setup", operation: "initialize-strategy-asset-ata", routeId: route.id, routeSpecSha256: routeSpecSha256(route), signer: route.setupAdmin, nonce: `initialize-strategy-asset-ata:${strategyAssetAta}`, prestateSlot: BigInt(prepared.prestateSlot), expiresAtUnix: BigInt(Math.floor(Date.now() / 1_000) + 300), canonicalMessageSha256 };
  assertIntentForRoute(intent, route);
  const digest = intentSha256(intent);
  const failedGateCount = gates.filter(({ pass }) => !pass).length;
  return { route, identity, graph, admin, accounts, strategyAssetAta, inspectedAddresses: protectedAddresses, simulatedAddresses, before, prepared, deploymentsBefore, deploymentsAfter, ataRentLamports, instruction, instructionData, intent, intentSha256: digest, report: { verdict: failedGateCount === 0 ? "PARTNER_STRATEGY_ASSET_ATA_SIMULATION_PASS" : "PARTNER_STRATEGY_ASSET_ATA_SIMULATION_FAIL", broadcast: false, readyForBroadcast: failedGateCount === 0, strategyId, fourMarketRouteSpecSha256: fourMarketRouteSpecSha256(), strategyGraphSha256: partnerStrategyGraphSha256(strategyId), builderRouteSpecSha256: routeSpecSha256(route), transaction: { operation: "initialize-strategy-asset-ata", strategyId, reserve: identity.reserve, setupAdmin: route.setupAdmin, strategyAuth: accounts.strategyAuth, strategyAssetAta, assetMint: route.asset.mint, instructionDataSha256: sha256(instructionData), packetBytes: prepared.packetBytes, feeLamports: prepared.feeLamports, ataRentLamports, maxTotalLamports: MAX_STRATEGY_ASSET_ATA_LAMPORTS, expectedSignature: prepared.expectedSignature, canonicalMessageSha256 }, simulation: { prestateSlot: prepared.prestateSlot, contextSlot: prepared.simulationSlot, inspectedAddresses: simulatedAddresses, err: prepared.simulation.err, unitsConsumed: prepared.simulation.unitsConsumed }, deployments: { before: deploymentsBefore.identities, after: deploymentsAfter.identities }, failedGateCount, gates } } as const;
}

export async function simulateStrategyAssetAta(strategyId: PartnerStrategyId) {
  return (await buildPreparation(strategyId)).report;
}

export async function strategyAssetAtaAuthorizationFacts(strategyId: PartnerStrategyId) {
  const route = partnerBuilderRoute(strategyId);
  const identity = partnerStrategyIdentity(strategyId);
  const accounts = await deriveVoltrAccounts(route);
  const [strategyAssetAta] = await findAssociatedTokenPda(
    {
      owner: accounts.strategyAuth,
      mint: route.asset.mint,
      tokenProgram: route.programs.token,
    },
    { programAddress: route.programs.associatedToken },
  );
  if (
    accounts.strategyAuth !== identity.voltr.strategyAuth
    || accounts.strategyInitReceipt !== identity.voltr.strategyInitReceipt
    || strategyAssetAta !== identity.voltr.strategyAssetAta
  ) throw new Error(`${strategyId} statically derived Voltr ATA accounts do not match the frozen catalog`);
  return {
    route,
    identity,
    accounts,
    strategyAssetAta,
    instructionDataSha256: sha256(Uint8Array.of(1)),
  } as const;
}

export type StrategyAssetAtaExecutionConfirmation = Readonly<{
  strategyId: PartnerStrategyId;
  authorizationPath: string | null;
  confirmAuthorizationSha256: string | null;
  confirmStrategyId: string | null;
  confirmReserve: string | null;
  confirmVault: string | null;
  confirmAta: string | null;
  confirmFourMarketRouteSpecSha256: string | null;
  confirmBuilderRouteSpecSha256: string | null;
  confirmInstructionDataSha256: string | null;
  confirmMaxTotalLamports: string | null;
}>;

export async function executeStrategyAssetAta(input: StrategyAssetAtaExecutionConfirmation) {
  const route = partnerBuilderRoute(input.strategyId);
  const identity = partnerStrategyIdentity(input.strategyId);
  if (process.env.CONFIRM_MAINNET !== "1") throw new Error("execute strategy-asset-ata requires CONFIRM_MAINNET=1");
  if (input.strategyId === "main") throw new Error("Main strategy asset ATA is already initialized and is not in the six-operation bootstrap authorization");
  if (!input.authorizationPath) throw new Error("execute strategy-asset-ata requires --authorization");
  const authorization = loadBootstrapExecutionAuthorization(
    input.authorizationPath,
    input.confirmAuthorizationSha256,
  );
  if (authorization.routeId !== "loyal-backyard-four-market-usdc-v1" || authorization.genesisHash !== PARTNER_ROUTE.genesisHash) throw new Error("bootstrap authorization route or mainnet genesis is not exact");
  const approved = operationAuthorization(
    authorization,
    "initialize-strategy-asset-ata",
    input.strategyId,
  );
  const staticFacts = await strategyAssetAtaAuthorizationFacts(input.strategyId);
  const expectedApproved = {
    reserve: identity.reserve,
    vault: route.vault,
    setupAdmin: route.setupAdmin,
    strategyAuth: staticFacts.accounts.strategyAuth,
    strategyInitReceipt: staticFacts.accounts.strategyInitReceipt,
    strategyAssetAta: staticFacts.strategyAssetAta,
    fourMarketRouteSpecSha256: fourMarketRouteSpecSha256(),
    strategyGraphSha256: partnerStrategyGraphSha256(input.strategyId),
    builderRouteSpecSha256: routeSpecSha256(route),
    instructionDataSha256: { createAta: staticFacts.instructionDataSha256 },
    maxTotalLamports: MAX_STRATEGY_ASSET_ATA_LAMPORTS.toString(),
  };
  if (!sameJson({ ...approved, operation: undefined, strategyId: undefined }, expectedApproved)) throw new Error(`bootstrap authorization semantics do not match initialize-strategy-asset-ata:${input.strategyId}`);
  if (input.confirmStrategyId !== input.strategyId) throw new Error(`execute strategy-asset-ata requires --confirm-strategy-id ${input.strategyId}`);
  if (input.confirmReserve !== identity.reserve) throw new Error(`execute strategy-asset-ata requires --confirm-reserve ${identity.reserve}`);
  if (input.confirmVault !== route.vault) throw new Error(`execute strategy-asset-ata requires --confirm-vault ${route.vault}`);
  if (input.confirmFourMarketRouteSpecSha256 !== fourMarketRouteSpecSha256()) throw new Error(`execute strategy-asset-ata requires --confirm-four-market-route-spec-sha256 ${fourMarketRouteSpecSha256()}`);
  if (input.confirmBuilderRouteSpecSha256 !== routeSpecSha256(route)) throw new Error(`execute strategy-asset-ata requires --confirm-builder-route-spec-sha256 ${routeSpecSha256(route)}`);
  if (input.confirmMaxTotalLamports !== MAX_STRATEGY_ASSET_ATA_LAMPORTS.toString()) throw new Error(`execute strategy-asset-ata requires --confirm-max-total-lamports ${MAX_STRATEGY_ASSET_ATA_LAMPORTS}`);
  const unsigned = staticFacts;
  const unsignedStrategyAssetAta = unsigned.strategyAssetAta;
  const unsignedInstructionDataSha256 = unsigned.instructionDataSha256;
  if (input.confirmAta !== unsignedStrategyAssetAta || unsignedStrategyAssetAta !== identity.voltr.strategyAssetAta) throw new Error(`execute strategy-asset-ata requires --confirm-ata ${identity.voltr.strategyAssetAta}`);
  if (input.confirmInstructionDataSha256 !== unsignedInstructionDataSha256) throw new Error(`execute strategy-asset-ata requires --confirm-instruction-data-sha256 ${unsignedInstructionDataSha256}`);
  const preparation = await buildPreparation(input.strategyId);
  if (input.confirmAta !== preparation.strategyAssetAta || input.confirmInstructionDataSha256 !== sha256(preparation.instructionData)) throw new Error("strategy asset ATA approval changed during signer preparation");
  if (!preparation.report.readyForBroadcast || preparation.report.failedGateCount !== 0) throw new Error(`strategy asset ATA preflight failed with ${preparation.report.verdict}`);
  const refreshed = await confirmedSnapshots(rpcUrl(), preparation.inspectedAddresses, preparation.prepared.simulationSlot);
  const refreshedMap = snapshotMap(preparation.inspectedAddresses, refreshed.accounts);
  const changedAccounts = preparation.inspectedAddresses.filter((account) => account !== route.asset.mint && !sameJson(fingerprint(preparation.before.get(account) ?? null), fingerprint(refreshedMap.get(account) ?? null)));
  if (changedAccounts.length > 0) throw new Error(`strategy asset ATA protected state changed after simulation (${changedAccounts.join(", ")}); refusing send`);
  const refreshedVault = verifyVaultCurrentState({ route, accounts: preparation.accounts, vault: refreshedMap.get(route.vault) ?? null, lpMint: refreshedMap.get(preparation.accounts.lpMint) ?? null, idleAta: refreshedMap.get(preparation.accounts.idleAta) ?? null, assetMint: refreshedMap.get(route.asset.mint) ?? null });
  if (!refreshedVault.gates.every(({ pass }) => pass)) throw new Error("strategy asset ATA refreshed vault or asset-mint semantics changed; refusing send");
  const refreshedStrategy = verifyStrategyBootstrap({ route, accounts: preparation.accounts, graph: preparation.graph, strategyReceipt: refreshedMap.get(preparation.accounts.strategyInitReceipt) ?? null, userMetadata: refreshedMap.get(preparation.graph.userMetadata) ?? null, obligation: refreshedMap.get(preparation.graph.obligation) ?? null, obligationFarm: refreshedMap.get(preparation.graph.obligationFarm) ?? null });
  if (!refreshedStrategy.every(({ pass }) => pass)) throw new Error("strategy bootstrap state changed before ATA setup; refusing send");
  const deployments = await loadDeploymentIdentities(rpcUrl(), route, refreshed.contextSlot, "confirmed");
  if (!verifyDeploymentIdentities(route, deployments.identities).every(({ pass }) => pass) || !sameJson(preparation.deploymentsBefore.identities, deployments.identities)) throw new Error("strategy asset ATA deployment identity changed after simulation; refusing send");
  const authorizationContextSlot = Math.max(preparation.prepared.simulationSlot, refreshed.contextSlot, deployments.contextSlot);
  let confirmed: Awaited<ReturnType<typeof sendPreparedConfirmedOnce>> | null = null;
  try {
    confirmed = await sendPreparedConfirmedOnce(rpcUrl(), preparation.prepared, authorizationContextSlot);
    if (confirmed.err !== null) return { verdict: "PARTNER_STRATEGY_ASSET_ATA_CONFIRMED_WITH_ERROR", broadcast: true, authorizationContextSlot, preflight: preparation.report, confirmed } as const;
    const readback = await confirmedSnapshots(rpcUrl(), preparation.inspectedAddresses, confirmed.confirmedSlot);
    const ata = ataState(readback.accounts[preparation.inspectedAddresses.indexOf(preparation.strategyAssetAta)] ?? null);
    const payerDelta = confirmed.lamportDeltas.find((row) => row.address === route.setupAdmin)?.deltaRaw;
    const ataDelta = confirmed.lamportDeltas.find((row) => row.address === preparation.strategyAssetAta)?.deltaRaw;
    const gates: Gate[] = [];
    add(gates, "confirmed ATA has exact owner/mint/zero balance", ata !== null && ata.owner === preparation.accounts.strategyAuth && ata.mint === route.asset.mint && ata.amount === 0n, ata, { owner: preparation.accounts.strategyAuth, mint: route.asset.mint, amount: 0n });
    add(gates, "confirmed ATA has exact rent", readback.accounts[preparation.inspectedAddresses.indexOf(preparation.strategyAssetAta)]?.lamports === preparation.ataRentLamports, readback.accounts[preparation.inspectedAddresses.indexOf(preparation.strategyAssetAta)]?.lamports ?? null, preparation.ataRentLamports);
    add(gates, "confirmed ATA lamport delta is exact rent", ataDelta === preparation.ataRentLamports.toString(), ataDelta, preparation.ataRentLamports.toString());
    add(gates, "confirmed setup-admin spend is fee plus ATA rent", payerDelta === `-${BigInt(confirmed.feeLamports ?? 0) + BigInt(preparation.ataRentLamports)}`, payerDelta, `-${BigInt(confirmed.feeLamports ?? 0) + BigInt(preparation.ataRentLamports)}`);
    add(gates, "confirmed transaction has no token movement", confirmed.tokenDeltas.every((row) => row.deltaRaw === "0"), confirmed.tokenDeltas, []);
    add(gates, "confirmed context is at or after transaction", readback.contextSlot >= confirmed.confirmedSlot, readback.contextSlot, `>=${confirmed.confirmedSlot}`);
    const finalDeployments = await loadDeploymentIdentities(rpcUrl(), route, readback.contextSlot, "confirmed");
    gates.push(...verifyDeploymentIdentities(route, finalDeployments.identities));
    const failedGateCount = gates.filter(({ pass }) => !pass).length;
    return { verdict: failedGateCount === 0 ? "PARTNER_STRATEGY_ASSET_ATA_CONFIRMED_AND_VERIFIED" : "PARTNER_STRATEGY_ASSET_ATA_CONFIRMED_READBACK_FAIL", broadcast: true, authorizationContextSlot, preflight: preparation.report, confirmed, readbackContextSlot: readback.contextSlot, readback: { failedGateCount, gates, tokenDeltas: confirmed.tokenDeltas, lamportDeltas: confirmed.lamportDeltas } } as const;
  } catch (error) {
    return confirmed ? { verdict: "PARTNER_STRATEGY_ASSET_ATA_CONFIRMED_READBACK_ERROR", broadcast: true, authorizationContextSlot, preflight: preparation.report, confirmed, error: error instanceof Error ? error.message : String(error), recoveryInstruction: "Do not resend. Re-read the exact strategy asset ATA and setup-admin balance." } as const : { verdict: "PARTNER_STRATEGY_ASSET_ATA_BROADCAST_STATUS_UNKNOWN", broadcast: null, authorizationContextSlot, expectedSignature: preparation.prepared.expectedSignature, preflight: preparation.report, error: error instanceof Error ? error.message : String(error), recoveryInstruction: "Do not resend. Verify this exact signature and the strategy asset ATA." } as const;
  }
}
