import { createHash } from "node:crypto";
import { mkdirSync, writeFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { resolve } from "node:path";

import { AddressLookupTableAccount, Connection, PublicKey } from "@solana/web3.js";

import { PARTNER_FOUR_MARKET_ROUTE, PARTNER_ROUTE, fourMarketRouteSpecSha256 } from "../domain/route-spec.js";
import { loadPolicyCatalogAuthorization, policyCatalogAuthorizationPath } from "../policies/authorization.js";
import { loadFourMarketProtectedState } from "./protected-state.js";
import {
  negativeMutationGeneratorSourceSha256,
  localCanonicalMutationRejection,
  produceNegativeMutationArtifact,
  SOLANA_PACKET_LIMIT_BYTES,
  assertExactNegativeMutationLookupTable,
  classifyNegativeMutationSimulationError,
  type NegativeMutationArtifact,
  type NegativeMutationSimulation,
  type NegativeMutationSimulationRequest,
} from "../verify/negative-mutations.js";

const REPOSITORY_ROOT = resolve(fileURLToPath(new URL("../../../..", import.meta.url)));
const DEFAULT_CATALOG_PATH = resolve(REPOSITORY_ROOT, "docs/evidence/backyard-voltr-four-market/runtime-policy-catalog-v1.json");
const DEFAULT_OUTPUT_PATH = resolve(REPOSITORY_ROOT, "docs/evidence/backyard-voltr-four-market/negative-mutations-v1.json");

function sha256(value: ArrayLike<number> | string): string {
  return createHash("sha256").update(typeof value === "string" ? value : Uint8Array.from(value)).digest("hex");
}

function rpcUrl(input?: string): string {
  const value = input ?? process.env.SOLANA_RPC_URL;
  if (!value) throw new Error("SOLANA_RPC_URL is required for confirmed negative-mutation simulation");
  return value;
}

async function loadExactAuthorizedCatalog(input: Readonly<{ catalogPath?: string; authorizationPath?: string; confirmAuthorizationSha256?: string | null }>) {
  const catalogPath = resolve(input.catalogPath ?? DEFAULT_CATALOG_PATH);
  const authorizationPath = resolve(input.authorizationPath ?? policyCatalogAuthorizationPath());
  const authorized = loadPolicyCatalogAuthorization(authorizationPath, catalogPath, input.confirmAuthorizationSha256);
  if (authorized.artifact.routeId !== PARTNER_FOUR_MARKET_ROUTE.id || authorized.artifact.routeSpecSha256 !== fourMarketRouteSpecSha256()) throw new Error("authorized policy catalog is not bound to the four-market route");
  if (authorized.artifact.runtimePolicyCount !== 8 || authorized.artifact.policies.length !== 8) throw new Error("authorized policy catalog must contain exactly eight runtime policies");
  return { catalogPath, authorizationPath, authorized } as const;
}

async function loadExactAlt(connection: Connection, minimumContextSlot: number): Promise<AddressLookupTableAccount> {
  const response = await connection.getAddressLookupTable(new PublicKey(PARTNER_ROUTE.lookupTable.address), { commitment: "confirmed", minContextSlot: minimumContextSlot });
  const table = response.value;
  if (!table || response.context.slot < minimumContextSlot) throw new Error("approved manager ALT is absent or predates the confirmed simulation context");
  assertExactNegativeMutationLookupTable(table);
  return table;
}

async function simulateRejectedMutation(
  connection: Connection,
  request: NegativeMutationSimulationRequest,
  preProtectedStateSha256: string,
  minimumContextSlot: number,
): Promise<NegativeMutationSimulation> {
  if (request.transaction.signatures.some((signature) => signature.some((byte) => byte !== 0))) throw new Error(`${request.id} mutation packet unexpectedly contains a signer signature`);
  const simulation = await connection.simulateTransaction(request.transaction, {
    commitment: "confirmed",
    sigVerify: false,
    replaceRecentBlockhash: false,
    minContextSlot: minimumContextSlot,
  });
  if (!Number.isSafeInteger(simulation.context.slot) || simulation.context.slot < minimumContextSlot || simulation.context.slot <= 0) throw new Error(`${request.id} simulation context predates protected prestate or is invalid`);
  const logs = simulation.value.logs ?? [];
  if (simulation.value.err === null || simulation.value.err === undefined) throw new Error(`${request.id} unexpectedly simulated successfully; negative case is not rejected`);
  if (!Array.isArray(logs) || !logs.every((line) => typeof line === "string")) throw new Error(`${request.id} simulation logs are not a string array`);
  if (simulation.value.unitsConsumed !== null && simulation.value.unitsConsumed !== undefined && (typeof simulation.value.unitsConsumed !== "number" || !Number.isFinite(simulation.value.unitsConsumed) || simulation.value.unitsConsumed < 0)) throw new Error(`${request.id} simulation unitsConsumed is not number|null`);
  const postProtected = await loadFourMarketProtectedState(connection.rpcEndpoint, simulation.context.slot);
  if (!Number.isSafeInteger(postProtected.contextSlot) || postProtected.contextSlot < simulation.context.slot || postProtected.stateSha256 !== preProtectedStateSha256) throw new Error(`${request.id} simulation changed the protected state or returned a stale post context`);
  const simulationError = {
    kind: "confirmed-simulation-error" as const,
    observation: "producer-observed-confirmed-rpc" as const,
    classification: classifyNegativeMutationSimulationError(simulation.value.err, logs),
    err: simulation.value.err,
    logs,
    logsSha256: sha256(logs.join("\n")),
    unitsConsumed: simulation.value.unitsConsumed ?? null,
    contextSlot: simulation.context.slot,
  };
  return { simulationError, preProtectedStateSha256, postProtectedStateSha256: postProtected.stateSha256, preProtectedContextSlot: minimumContextSlot, postProtectedContextSlot: postProtected.contextSlot };
}

async function simulateOrRejectMutation(
  connection: Connection,
  request: NegativeMutationSimulationRequest,
  preProtectedStateSha256: string,
  minimumContextSlot: number,
): Promise<NegativeMutationSimulation> {
  if (request.enforcementLayer === "canonical pre-send verifier") return localCanonicalMutationRejection(request, preProtectedStateSha256);
  const packetBytes = Buffer.from(request.transaction.serialize()).length;
  if (packetBytes > SOLANA_PACKET_LIMIT_BYTES) {
    throw new Error(`${request.id} mutation packet is oversized before RPC (${packetBytes} > ${SOLANA_PACKET_LIMIT_BYTES} bytes)`);
  }
  return simulateRejectedMutation(connection, request, preProtectedStateSha256, minimumContextSlot);
}

export type ConfirmedNegativeMutationProducerResult = Readonly<{
  artifact: NegativeMutationArtifact;
  path: string;
  fileSha256: string;
  catalogPath: string;
  authorizationPath: string;
  authorizationSha256: string;
  routeSpecSha256: string;
  protectedPrestateSha256: string;
  protectedContextSlot: number;
  simulationContextSlots: readonly number[];
  broadcast: false;
}>;

/**
 * Confirmed-mainnet, no-signer, no-broadcast producer for the complete matrix.
 * It intentionally performs one simulation at a time so a provider rate limit
 * or a protected-state drift fails closed instead of producing a partial file.
 */
export async function produceConfirmedNegativeMutationArtifact(input: Readonly<{
  rpcUrl?: string;
  catalogPath?: string;
  authorizationPath?: string;
  confirmAuthorizationSha256?: string | null;
  amountRaw: bigint;
  outputPath?: string;
}>): Promise<ConfirmedNegativeMutationProducerResult> {
  const { catalogPath, authorizationPath, authorized } = await loadExactAuthorizedCatalog(input);
  const connection = new Connection(rpcUrl(input.rpcUrl), "confirmed");
  const latest = await connection.getLatestBlockhashAndContext("confirmed");
  const protectedBefore = await loadFourMarketProtectedState(connection.rpcEndpoint, latest.context.slot);
  const lookupTable = await loadExactAlt(connection, Math.max(latest.context.slot, protectedBefore.contextSlot));
  const simulationContextSlots: number[] = [];
  const artifact = await produceNegativeMutationArtifact({
    artifact: authorized.artifact,
    amountRaw: input.amountRaw,
    recentBlockhash: latest.value.blockhash,
    lookupTable,
    protectedStateSha256: protectedBefore.stateSha256,
    protectedContextSlot: protectedBefore.contextSlot,
    simulate: async (request) => {
      const result = await simulateOrRejectMutation(connection, request, protectedBefore.stateSha256, protectedBefore.contextSlot);
      simulationContextSlots.push(result.postProtectedContextSlot);
      return result;
    },
  });
  const protectedAfter = await loadFourMarketProtectedState(connection.rpcEndpoint, Math.max(...simulationContextSlots, protectedBefore.contextSlot));
  if (protectedAfter.stateSha256 !== protectedBefore.stateSha256) throw new Error("protected state changed after the complete negative simulation matrix");
  const outputPath = resolve(input.outputPath ?? DEFAULT_OUTPUT_PATH);
  mkdirSync(resolve(outputPath, ".."), { recursive: true });
  const serialized = `${JSON.stringify(artifact, null, 2)}\n`;
  writeFileSync(outputPath, serialized, { encoding: "utf8", mode: 0o600 });
  return {
    artifact,
    path: outputPath,
    fileSha256: sha256(serialized),
    catalogPath,
    authorizationPath,
    authorizationSha256: authorized.authorization.authorizationSha256,
    routeSpecSha256: artifact.routeSpecSha256,
    protectedPrestateSha256: protectedBefore.stateSha256,
    protectedContextSlot: protectedBefore.contextSlot,
    simulationContextSlots,
    broadcast: false,
  };
}

export { DEFAULT_CATALOG_PATH, DEFAULT_OUTPUT_PATH, negativeMutationGeneratorSourceSha256 };
