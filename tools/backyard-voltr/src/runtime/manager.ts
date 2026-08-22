import { createHash } from "node:crypto";
import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { relative, resolve } from "node:path";

import { generated as squadsGenerated } from "@loyal-labs/loyal-smart-accounts-core";
import { executePolicyPayloadSync } from "@loyal-labs/loyal-smart-accounts-core/internal";
import { Obligation } from "@kamino-finance/klend-sdk";
import { getTokenDecoder } from "@solana-program/token";
import {
  AccountRole,
  address,
  createNoopSigner,
  type Instruction,
  type TransactionSigner,
} from "@solana/kit";
import { getStrategyInitReceiptDecoder, parseTransactionEvents } from "@voltr/vault-sdk";
import bs58 from "bs58";
import {
  AddressLookupTableAccount,
  ComputeBudgetProgram,
  Connection,
  PublicKey,
  TransactionInstruction,
  VersionedTransaction,
} from "@solana/web3.js";

import {
  assertIntentForRouteBinding,
  intentSha256,
  type ManagerRuntimeIntent,
} from "../domain/execution-intent.js";
import {
  PARTNER_FOUR_MARKET_ROUTE,
  PARTNER_ROUTE,
  fourMarketRouteSpecSha256,
  partnerBuilderRoute,
  partnerStrategyGraphSha256,
  type PartnerStrategyId,
} from "../domain/route-spec.js";
import {
  confirmedSnapshots,
  fromWeb3Instruction,
  loadDeploymentIdentities,
  loadMainReserveGraph,
  prepareSignedV0Transaction,
  rentExemptionLamports,
  sendPreparedConfirmedOnce,
  submissionEvidence,
  MAX_IDENTICAL_SUBMISSION_ATTEMPTS,
  type AccountSnapshot,
  type PreparedTransaction,
} from "../integrations/solana-compat.js";
import { signingMaterialFromEnvironment } from "../integrations/signer.js";
import {
  createVoltrRouteBuilder,
  deriveVoltrAccounts,
  type CanonicalInstruction,
  type ReserveGraph,
  type VoltrInstruction,
} from "../integrations/voltr.js";
import { loadRuntimePolicyArtifact, type RuntimePolicyArtifactEntry } from "../policies/compiler.js";
import { loadPolicyCatalogAuthorization, policyCatalogAuthorizationPath } from "../policies/authorization.js";
import { effectiveRouteAuthorizationDigest } from "../policies/authorization.js";
import { verifyExistingRuntimePolicies } from "../policies/commands.js";
import { verifyDeploymentIdentities, verifyVaultCurrentState, type Gate } from "../verify/current.js";
import { verifyNonCatalogSquadsPoliciesIsolated } from "../verify/squads.js";
import {
  assertProtectedPreSendAttestation,
  createProtectedPreSendAttestation,
  createProtectedSettlementAttestation,
  loadFourMarketProtectedState,
  protectedSnapshotEvidenceEnvelope,
  protectedStateEnvelope,
  type ProtectedPreSendAttestation,
  type ProtectedSettlementAttestation,
  type ProtectedSnapshotEvidence,
} from "./protected-state.js";
import {
  confirmRestorationBridge,
  prepareRestorationBridge,
  type RestorationBridgePhaseAResult,
  type RestorationBridgePhaseBResult,
} from "./restoration-bridge.js";

export type ManagerOperation = "deposit" | "withdraw";
const MANAGER_COMPUTE_UNIT_LIMIT = 500_000;
const MANAGER_HEAP_FRAME_BYTES = 256 * 1_024;
const KAMINO_OBLIGATION_DATA_LENGTH = Obligation.layout.span + Obligation.discriminator.length;
const REPOSITORY_ROOT = resolve(fileURLToPath(new URL("../../../..", import.meta.url)));
const MANAGER_INTENT_ROOT = resolve(REPOSITORY_ROOT, "docs/evidence/backyard-voltr-four-market/intents");
const RESTORATION_BRIDGE_ROOT = resolve(REPOSITORY_ROOT, "docs/evidence/backyard-voltr-four-market/restoration-bridge");
type ManagerPreparationApproval = Readonly<{
  confirmArtifactSha256?: string | null;
  confirmWrapperDataSha256?: string | null;
  confirmAuthorizationSha256?: string | null;
  confirmRouteAuthorizationSha256?: string | null;
  authorizationPath?: string | null;
  minimumContextSlot?: number;
  lifecycleId?: string | undefined;
}>;

export type ManagerRestorationBridgeInput = Readonly<{
  originId: string;
  generation: number;
  legId: string;
  owner: string;
  leaseSeconds: number;
  protectedAddressSetSha256: string;
  protectedPrestateSha256: string;
  protectedContextSlot: number;
  evidenceDirectory: string;
  binaryPath?: string | null;
}>;

type ManagerExecutionEnvelope = Readonly<{
  programId: string;
  accounts: readonly Readonly<{ address: string; signer: boolean; writable: boolean }>[];
  dataLength: number;
  dataBase64: string;
  dataSha256: string;
}>;

type RuntimeEntry = RuntimePolicyArtifactEntry & Readonly<{
  managerExecution: ManagerExecutionEnvelope;
}>;

type ManagerWrapper = Readonly<{
  instruction: TransactionInstruction;
  expectedAccounts: readonly Readonly<{ address: string; signer: boolean; writable: boolean }>[];
  dataBase64: string;
  dataSha256: string;
  compiledPayloadSha256: string;
  artifactDataMatches: boolean;
}>;

export type CompatibilityManagerWrapper = Readonly<{
  instruction: TransactionInstruction;
  expectedAccounts: readonly Readonly<{ address: string; signer: boolean; writable: boolean }>[];
  dataBase64: string;
  dataSha256: string;
  compiledPayloadSha256: string;
}>;

type SnapshotSet = Readonly<{
  addresses: readonly string[];
  accounts: readonly (AccountSnapshot | null)[];
  contextSlot: number;
}>;

type ManagerProtectedEvidence = Readonly<{
  before: ProtectedSnapshotEvidence;
  after: ProtectedSnapshotEvidence;
}>;

const SMART_ACCOUNT_SEED = "smart_account";
const POLICY_SEED = "policy";

function rpcUrl(): string {
  const value = process.env.SOLANA_RPC_URL;
  if (!value) throw new Error("SOLANA_RPC_URL is required");
  return value;
}

function requireManagerIntentPath(value: string): string {
  const path = resolve(value);
  const relativePath = relative(MANAGER_INTENT_ROOT, path);
  if (!relativePath || relativePath === ".." || relativePath.startsWith("../") || relativePath.startsWith("/")) {
    throw new Error("manager execute --intent-path must be inside docs/evidence/backyard-voltr-four-market/intents");
  }
  return path;
}

function normalizeRestorationBridgeInput(
  value: ManagerRestorationBridgeInput | null | undefined,
  strategyId: PartnerStrategyId,
  operation: ManagerOperation,
): ManagerRestorationBridgeInput | null {
  if (value === null || value === undefined) return null;
  if (strategyId !== "main" || operation !== "withdraw") throw new Error("restoration bridge is allowed only for the exact Main manager withdrawal");
  const exactSha = (candidate: string, label: string) => {
    if (!/^[0-9a-f]{64}$/.test(candidate)) throw new Error(`${label} must be a lowercase SHA-256 digest`);
  };
  exactSha(value.originId, "restoration origin id");
  exactSha(value.legId, "restoration leg id");
  exactSha(value.protectedAddressSetSha256, "restoration protected address-set hash");
  exactSha(value.protectedPrestateSha256, "restoration protected prestate hash");
  if (!Number.isSafeInteger(value.generation) || value.generation <= 0) throw new Error("restoration generation must be a positive safe integer");
  if (!Number.isSafeInteger(value.leaseSeconds) || value.leaseSeconds < 60 || value.leaseSeconds > 900) throw new Error("restoration lease must be 60..900 seconds");
  if (!Number.isSafeInteger(value.protectedContextSlot) || value.protectedContextSlot <= 0) throw new Error("restoration protected context slot must be a positive safe integer");
  if (!value.owner || value.owner.length > 128) throw new Error("restoration bridge owner must be 1..128 characters");
  const evidenceDirectory = resolve(value.evidenceDirectory);
  const relativePath = relative(RESTORATION_BRIDGE_ROOT, evidenceDirectory);
  if (relativePath === ".." || relativePath.startsWith("../") || relativePath.startsWith("/")) throw new Error("restoration bridge evidence directory must be inside the maintained restoration-bridge root");
  return { ...value, evidenceDirectory };
}

function restorationManagerIntentId(originId: string, generation: number, legId: string): string {
  return sha256(Buffer.from(`backyard-voltr-manager-intent-v1:${originId}:${generation}:${legId}`, "utf8"));
}

function assertIntentNotExpired(intent: ManagerRuntimeIntent): void {
  const now = BigInt(Math.floor(Date.now() / 1_000));
  if (now >= intent.expiresAtUnix) {
    throw new Error(`manager execution intent expired at ${intent.expiresAtUnix}; rebuild and simulate a fresh packet`);
  }
}

function sha256(value: ArrayLike<number>): string {
  return createHash("sha256").update(Uint8Array.from(value)).digest("hex");
}

function assertPreparedWire(prepared: PreparedTransaction): string {
  const wire = Uint8Array.from(prepared.serializedTransaction);
  const transaction = VersionedTransaction.deserialize(wire);
  const roundTrip = transaction.serialize();
  const wireSha256 = sha256(wire);
  const messageSha256 = sha256(transaction.message.serialize());
  const actualSignature = transaction.signatures.length === 1
    ? bs58.encode(transaction.signatures[0]!)
    : null;
  if (wire.length !== prepared.packetBytes
    || !Buffer.from(roundTrip).equals(Buffer.from(wire))
    || messageSha256 !== sha256(prepared.serializedMessage)
    || actualSignature !== prepared.expectedSignature) {
    throw new Error(`prepared manager wire failed exact pre-send validation (packet=${wire.length}/${prepared.packetBytes}, wireSha256=${wireSha256}, messageSha256=${messageSha256}, signature=${actualSignature})`);
  }
  return Buffer.from(wire).toString("base64");
}

function persistManagerIntent(
  intentPath: string,
  preparation: Readonly<{
    strategyId: PartnerStrategyId;
    operation: ManagerOperation;
    intent: ManagerRuntimeIntent;
    intentSha256: string;
    prepared: PreparedTransaction;
    loaded: Readonly<{ path: string; fileSha256: string; artifact: Readonly<{ artifactSha256: string }> }>;
    authorization: Readonly<{ path: string; fileSha256: string; authorization: Readonly<{ authorizationSha256: string }> }>;
    protectedPreSend: ProtectedSnapshotEvidence;
    preSendAttestation: ProtectedPreSendAttestation;
  }>,
  authorizationContextSlot: number,
): Readonly<{ path: string; fileSha256: string; persistenceContract: Readonly<Record<string, unknown>> }> {
  const path = requireManagerIntentPath(intentPath);
  const serializedTransactionBase64 = assertPreparedWire(preparation.prepared);
  const document = JSON.stringify({
    schemaVersion: 1,
    kind: "backyard-voltr-manager-operation-intent",
    strategyId: preparation.strategyId,
    operation: preparation.operation,
    routeId: PARTNER_FOUR_MARKET_ROUTE.id,
    routeSpecSha256: fourMarketRouteSpecSha256(),
    authorizationContextSlot,
    authorizationPath: preparation.authorization.path,
    authorizationFileSha256: preparation.authorization.fileSha256,
    authorizationSha256: preparation.authorization.authorization.authorizationSha256,
    routeAuthorizationSha256: preparation.intent.routeAuthorizationSha256,
    lifecycleId: preparation.intent.lifecycleId,
    protectedPrestateSha256: preparation.intent.protectedPrestateSha256,
    protectedSnapshotEvidence: {
      before: preparation.protectedPreSend,
    },
    protectedPrestateEvidence: preparation.protectedPreSend,
    preSendAttestation: preparation.preSendAttestation,
    artifactPath: preparation.loaded.path,
    artifactFileSha256: preparation.loaded.fileSha256,
    artifactSha256: preparation.loaded.artifact.artifactSha256,
    expectedSignature: preparation.prepared.expectedSignature,
    serializedTransactionSha256: sha256(preparation.prepared.serializedTransaction),
    serializedTransactionBase64,
    canonicalMessageSha256: sha256(preparation.prepared.serializedMessage),
    packetBytes: preparation.prepared.packetBytes,
    feeLamports: preparation.prepared.feeLamports,
    latestBlockhash: preparation.prepared.latestBlockhash,
    persistenceContract: {
      schemaVersion: 2,
      kind: "pre-send-signed-wire",
      persistedBeforeSend: true,
      oneSendOnly: true,
      maxSendAttempts: 1,
      maxSubmissionAttempts: MAX_IDENTICAL_SUBMISSION_ATTEMPTS,
      maxRetries: 0,
      recoveryByExpectedSignature: true,
      recoveryByExpectedSignatureOnly: true,
      expectedSignature: preparation.prepared.expectedSignature,
      serializedTransactionSha256: sha256(preparation.prepared.serializedTransaction),
      serializedMessageSha256: sha256(preparation.prepared.serializedMessage),
      intentSha256: preparation.intentSha256,
      lifecycleId: preparation.intent.lifecycleId,
      routeAuthorizationSha256: preparation.intent.routeAuthorizationSha256,
      protectedPrestateSha256: preparation.intent.protectedPrestateSha256,
      submissionWireSha256: sha256(preparation.prepared.serializedTransaction),
      preSendAttestationSha256: preparation.preSendAttestation.attestationSha256,
      preSendAttestationSignatureSha256: preparation.preSendAttestation.signatureSha256,
    },
    intent: preparation.intent,
    intentSha256: preparation.intentSha256,
  }, (_key, value) => typeof value === "bigint" ? value.toString() : value, 2) + "\n";
  try {
    mkdirSync(MANAGER_INTENT_ROOT, { recursive: true });
    writeFileSync(path, document, { encoding: "utf8", mode: 0o600, flag: "wx" });
  } catch (error) {
    throw new Error(`manager operation could not persist the exact pre-send intent at ${path}`, { cause: error });
  }
  return {
    path,
    fileSha256: sha256(Buffer.from(document, "utf8")),
    persistenceContract: {
      schemaVersion: 2,
      kind: "pre-send-signed-wire",
      persistedBeforeSend: true,
      oneSendOnly: true,
      maxSendAttempts: 1,
      maxSubmissionAttempts: MAX_IDENTICAL_SUBMISSION_ATTEMPTS,
      maxRetries: 0,
      recoveryByExpectedSignature: true,
      recoveryByExpectedSignatureOnly: true,
      expectedSignature: preparation.prepared.expectedSignature,
      serializedTransactionSha256: sha256(preparation.prepared.serializedTransaction),
      serializedMessageSha256: sha256(preparation.prepared.serializedMessage),
      intentSha256: preparation.intentSha256,
      lifecycleId: preparation.intent.lifecycleId,
      routeAuthorizationSha256: preparation.intent.routeAuthorizationSha256,
      protectedPrestateSha256: preparation.intent.protectedPrestateSha256,
      submissionWireSha256: sha256(preparation.prepared.serializedTransaction),
      preSendAttestationSha256: preparation.preSendAttestation.attestationSha256,
      preSendAttestationSignatureSha256: preparation.preSendAttestation.signatureSha256,
    },
  } as const;
}

function verifyPersistedManagerIntent(
  persisted: Readonly<{ path: string; fileSha256: string }>,
  preparation: Readonly<{ intent: ManagerRuntimeIntent; intentSha256: string; prepared: PreparedTransaction; protectedPreSend: ProtectedSnapshotEvidence; preSendAttestation: ProtectedPreSendAttestation }>,
): void {
  const bytes = readFileSync(persisted.path);
  if (sha256(bytes) !== persisted.fileSha256) throw new Error("persisted manager intent file hash changed before send/readback");
  const value = JSON.parse(bytes.toString("utf8")) as {
    routeAuthorizationSha256?: unknown;
    lifecycleId?: unknown;
    protectedPrestateSha256?: unknown;
    persistenceContract?: {
      schemaVersion?: unknown;
      persistedBeforeSend?: unknown;
      maxSendAttempts?: unknown;
      maxSubmissionAttempts?: unknown;
      maxRetries?: unknown;
      recoveryByExpectedSignatureOnly?: unknown;
      expectedSignature?: unknown;
      serializedTransactionSha256?: unknown;
      serializedMessageSha256?: unknown;
      intentSha256?: unknown;
      lifecycleId?: unknown;
      routeAuthorizationSha256?: unknown;
      protectedPrestateSha256?: unknown;
      submissionWireSha256?: unknown;
      preSendAttestationSha256?: unknown;
      preSendAttestationSignatureSha256?: unknown;
    };
    intent?: unknown;
    intentSha256?: unknown;
    expectedSignature?: unknown;
    protectedSnapshotEvidence?: unknown;
    protectedPrestateEvidence?: unknown;
    preSendAttestation?: unknown;
  };
  const protectedEvidence = value.protectedSnapshotEvidence && typeof value.protectedSnapshotEvidence === "object"
    ? (value.protectedSnapshotEvidence as { before?: ProtectedSnapshotEvidence }).before
    : undefined;
  if (!protectedEvidence || !value.protectedPrestateEvidence || protectedEvidence.stateSha256 !== preparation.intent.protectedPrestateSha256 || protectedEvidence.stateSha256 !== preparation.protectedPreSend.stateSha256 || JSON.stringify(value.protectedPrestateEvidence) !== JSON.stringify(preparation.protectedPreSend)) {
    throw new Error("persisted manager intent is missing the exact protected pre-send snapshot evidence");
  }
  if (!value.preSendAttestation || typeof value.preSendAttestation !== "object") {
    throw new Error("persisted manager intent is missing the protected pre-send attestation");
  }
  assertProtectedPreSendAttestation(value.preSendAttestation);
  if ((value.preSendAttestation as ProtectedPreSendAttestation).attestationSha256 !== preparation.preSendAttestation.attestationSha256
    || (value.preSendAttestation as ProtectedPreSendAttestation).signatureSha256 !== preparation.preSendAttestation.signatureSha256) {
    throw new Error("persisted manager intent pre-send attestation changed before send/readback");
  }
  if (value.routeAuthorizationSha256 !== preparation.intent.routeAuthorizationSha256
    || value.lifecycleId !== preparation.intent.lifecycleId
    || value.protectedPrestateSha256 !== preparation.intent.protectedPrestateSha256
    || value.intentSha256 !== preparation.intentSha256
    || value.expectedSignature !== preparation.prepared.expectedSignature
    || value.persistenceContract?.persistedBeforeSend !== true
    || value.persistenceContract?.schemaVersion !== 2
    || value.persistenceContract?.maxSendAttempts !== 1
    || value.persistenceContract?.maxSubmissionAttempts !== MAX_IDENTICAL_SUBMISSION_ATTEMPTS
    || value.persistenceContract?.maxRetries !== 0
    || value.persistenceContract?.recoveryByExpectedSignatureOnly !== true
    || value.persistenceContract?.expectedSignature !== preparation.prepared.expectedSignature
    || value.persistenceContract?.serializedTransactionSha256 !== sha256(preparation.prepared.serializedTransaction)
    || value.persistenceContract?.serializedMessageSha256 !== sha256(preparation.prepared.serializedMessage)
    || value.persistenceContract?.intentSha256 !== preparation.intentSha256
    || value.persistenceContract?.lifecycleId !== preparation.intent.lifecycleId
    || value.persistenceContract?.routeAuthorizationSha256 !== preparation.intent.routeAuthorizationSha256
    || value.persistenceContract?.protectedPrestateSha256 !== preparation.intent.protectedPrestateSha256
    || value.persistenceContract?.submissionWireSha256 !== sha256(preparation.prepared.serializedTransaction)
    || value.persistenceContract?.preSendAttestationSha256 !== (value.preSendAttestation as { attestationSha256?: unknown }).attestationSha256
    || value.persistenceContract?.preSendAttestationSignatureSha256 !== (value.preSendAttestation as { signatureSha256?: unknown }).signatureSha256
    || intentSha256(value.intent as ManagerRuntimeIntent) !== preparation.intentSha256) {
    throw new Error("persisted manager intent is not bound to the exact route authorization/lifecycle/prestate/wire");
  }
}

function add(gates: Gate[], name: string, pass: boolean, observed: unknown, expected: unknown): void {
  gates.push({ name, pass, observed, expected });
}

function strategyEventPayload(logs: readonly string[], operation: ManagerOperation): Readonly<Record<string, unknown>> | null {
  const name = operation === "deposit" ? "DepositStrategyEvent" : "WithdrawStrategyEvent";
  const events = parseTransactionEvents({ logMessages: [...logs] }).filter((event) => event.name === name);
  return events.length === 1 ? events[0]!.payload as unknown as Readonly<Record<string, unknown>> : null;
}

function bigintEventField(event: Readonly<Record<string, unknown>> | null, name: string): bigint | null {
  const value = event?.[name];
  return typeof value === "bigint" ? value : null;
}

function unique<T>(values: readonly T[]): T[] {
  return [...new Set(values)];
}

function derivePolicy(settings: string, seed: bigint): string {
  const seedBytes = Buffer.alloc(8);
  seedBytes.writeBigUInt64LE(seed);
  return PublicKey.findProgramAddressSync(
    [
      Buffer.from(SMART_ACCOUNT_SEED),
      Buffer.from(POLICY_SEED),
      new PublicKey(settings).toBuffer(),
      seedBytes,
    ],
    new PublicKey(PARTNER_ROUTE.squads.program),
  )[0].toBase58();
}

function bytesFromBase64(value: string, label: string): Buffer {
  const decoded = Buffer.from(value, "base64");
  if (decoded.length === 0 || decoded.toString("base64") !== value) {
    throw new Error(`${label} is not canonical base64`);
  }
  return decoded;
}

/**
 * Runtime amounts may vary inside the policy limit. The compiled artifact is
 * still authoritative for every other byte: locate its proof amount exactly
 * once and allow only that eight-byte little-endian field to change.
 */
function dynamicArtifactDataMatches(
  template: ArrayLike<number>,
  candidate: ArrayLike<number>,
  amountRaw: bigint,
): boolean {
  const expected = Buffer.from(Uint8Array.from(template));
  const actual = Buffer.from(Uint8Array.from(candidate));
  if (expected.length !== actual.length || amountRaw < 0n || amountRaw > 0xffff_ffff_ffff_ffffn) return false;
  const offsets: number[] = [];
  for (let offset = 0; offset <= expected.length - 8; offset += 1) {
    if (expected.readBigUInt64LE(offset) === PARTNER_ROUTE.asset.proofAmountRaw) offsets.push(offset);
  }
  if (offsets.length !== 1) return false;
  const amountOffset = offsets[0]!;
  if (actual.readBigUInt64LE(amountOffset) !== amountRaw) return false;
  for (let index = 0; index < expected.length; index += 1) {
    if (index >= amountOffset && index < amountOffset + 8) continue;
    if (expected[index] !== actual[index]) return false;
  }
  return true;
}

function managerEntry(
  operation: ManagerOperation,
  entry: RuntimePolicyArtifactEntry,
): RuntimeEntry {
  const managerExecution = (entry as RuntimeEntry).managerExecution;
  if (!managerExecution || typeof managerExecution !== "object") {
    throw new Error(`${operation} runtime artifact is missing managerExecution`);
  }
  const data = bytesFromBase64(managerExecution.dataBase64, `${operation} managerExecution.dataBase64`);
  if (
    managerExecution.programId !== PARTNER_ROUTE.squads.program
    || managerExecution.dataLength !== data.length
    || managerExecution.dataSha256 !== sha256(data)
    || managerExecution.accounts.length === 0
  ) {
    throw new Error(`${operation} manager execution envelope is not self-consistent`);
  }
  return { ...entry, managerExecution };
}

function deduplicateInnerAccounts(inner: CanonicalInstruction): {
  accounts: { pubkey: PublicKey; isSigner: boolean; isWritable: boolean }[];
  indexes: number[];
  programIndex: number;
} {
  const accounts: { pubkey: PublicKey; isSigner: boolean; isWritable: boolean }[] = [];
  const indexOf = (value: string, signer: boolean, writable: boolean): number => {
    const pubkey = new PublicKey(value);
    const index = accounts.findIndex((candidate) => candidate.pubkey.equals(pubkey));
    if (index >= 0) {
      accounts[index]!.isSigner ||= signer;
      accounts[index]!.isWritable ||= writable;
      return index;
    }
    accounts.push({ pubkey, isSigner: signer, isWritable: writable });
    return accounts.length - 1;
  };
  const indexes = inner.accounts.map((meta) => indexOf(meta.address, false, meta.writable));
  const programIndex = indexOf(inner.programId, false, false);
  return { accounts, indexes, programIndex };
}

function encodeCompiledPayload(inner: CanonicalInstruction): Buffer {
  const deduped = deduplicateInnerAccounts(inner);
  const data = Buffer.from(inner.data);
  if (deduped.accounts.length > 255 || deduped.indexes.length > 255 || data.length > 65_535) {
    throw new Error("inner Voltr instruction exceeds Squads compact payload limits");
  }
  return Buffer.concat([
    Buffer.from([1, deduped.programIndex, deduped.indexes.length]),
    Buffer.from(deduped.indexes),
    (() => { const b = Buffer.alloc(2); b.writeUInt16LE(data.length); return b; })(),
    data,
  ]);
}

function expectedWrapperAccounts(
  policy: string,
  inner: CanonicalInstruction,
): { address: string; signer: boolean; writable: boolean }[] {
  const deduped = deduplicateInnerAccounts(inner);
  return [
    { address: policy, signer: false, writable: true },
    { address: PARTNER_ROUTE.squads.program, signer: false, writable: false },
    { address: PARTNER_ROUTE.squads.guardian, signer: true, writable: false },
    ...deduped.accounts.map(({ pubkey, isWritable }) => ({
      address: pubkey.toBase58(),
      signer: false,
      writable: isWritable,
    })),
  ];
}

/**
 * Pure Squads wrapper construction used by the no-broadcast compatibility
 * probe. The policy address is sizing-only; this function does not claim that
 * the policy authorizes the supplied inner graph.
 */
export function buildManagerWrapperForCompatibility(
  policy: string,
  inner: CanonicalInstruction,
): CompatibilityManagerWrapper {
  const compiled = encodeCompiledPayload(inner);
  const expectedAccounts = expectedWrapperAccounts(policy, inner);
  const guardian = new PublicKey(PARTNER_ROUTE.squads.guardian);
  const transactionAccounts = deduplicateInnerAccounts(inner).accounts.map((account) => ({
    pubkey: account.pubkey,
    isSigner: false,
    isWritable: account.isWritable,
  }));
  const instruction = executePolicyPayloadSync({
    // The wrapper is never submitted by this helper. A real runtime operation
    // separately binds the exact decoded policy artifact before signer use.
    feePayer: guardian,
    policy: new PublicKey(policy),
    accountIndex: PARTNER_ROUTE.squads.vaultIndex,
    numSigners: 1,
    policyPayload: {
      __kind: "ProgramInteraction",
      fields: [{
        instructionConstraintIndices: new Uint8Array([0]),
        transactionPayload: {
          __kind: "SyncTransaction",
          fields: [{
            accountIndex: PARTNER_ROUTE.squads.vaultIndex,
            instructions: compiled,
          }],
        },
      }],
    },
    instruction_accounts: [
      { pubkey: guardian, isSigner: true, isWritable: false },
      ...transactionAccounts,
    ],
    programId: new PublicKey(PARTNER_ROUTE.squads.program),
  });
  const data = Buffer.from(instruction.data);
  return {
    instruction,
    expectedAccounts,
    dataBase64: data.toString("base64"),
    dataSha256: sha256(data),
    compiledPayloadSha256: sha256(compiled),
  };
}

function buildManagerWrapper(
  operation: ManagerOperation,
  entry: RuntimeEntry,
  inner: CanonicalInstruction,
  amountRaw: bigint,
): ManagerWrapper {
  const core = buildManagerWrapperForCompatibility(entry.policy, inner);
  const data = Buffer.from(core.instruction.data);
  const actual = {
    programId: core.instruction.programId.toBase58(),
    accounts: core.instruction.keys.map((meta) => ({
      address: meta.pubkey.toBase58(),
      signer: meta.isSigner,
      writable: meta.isWritable,
    })),
    dataLength: data.length,
    dataBase64: data.toString("base64"),
    dataSha256: sha256(data),
  };
  const expected = entry.managerExecution;
  const expectedData = bytesFromBase64(expected.dataBase64, `${operation} manager artifact data`);
  const artifactDataMatches = dynamicArtifactDataMatches(expectedData, data, amountRaw);
  if (
    actual.programId !== expected.programId
    || actual.dataLength !== expected.dataLength
    || expected.dataSha256 !== sha256(expectedData)
    || !artifactDataMatches
    || JSON.stringify(actual.accounts) !== JSON.stringify(expected.accounts)
  ) {
    throw new Error(`${operation} manager wrapper escaped the verified policy artifact outside its amount field`);
  }
  return {
    ...core,
    dataBase64: actual.dataBase64,
    dataSha256: actual.dataSha256,
    artifactDataMatches,
  };
}

/**
 * Pure canonical wrapper reconstruction for independent finalized verification.
 * It performs no RPC calls and never signs or sends a transaction.
 */
export function buildManagerWrapperForVerification(
  operation: ManagerOperation,
  entry: RuntimePolicyArtifactEntry,
  inner: CanonicalInstruction,
  amountRaw: bigint,
): ManagerWrapper {
  return buildManagerWrapper(operation, managerEntry(operation, entry as RuntimeEntry), inner, amountRaw);
}

function tokenAmount(snapshot: AccountSnapshot | null): bigint | null {
  if (!snapshot) return null;
  try {
    return getTokenDecoder().decode(snapshot.data).amount;
  } catch {
    return null;
  }
}

function strategyPosition(snapshot: AccountSnapshot | null): bigint | null {
  if (!snapshot) return null;
  try {
    return getStrategyInitReceiptDecoder().decode(snapshot.data).positionValue;
  } catch {
    return null;
  }
}

function accountMap(snapshot: SnapshotSet): Map<string, AccountSnapshot | null> {
  return new Map(snapshot.addresses.map((addressValue, index) => [addressValue, snapshot.accounts[index] ?? null]));
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

function finalizedTokenDelta(
  finalized: Awaited<ReturnType<typeof sendPreparedConfirmedOnce>>,
  account: string,
): { delta: bigint | null; mint: string | null } {
  const rows = finalized.tokenDeltas.filter((row) => row.address === account);
  if (rows.length === 0) return { delta: 0n, mint: null };
  if (rows.length !== 1) return { delta: null, mint: null };
  return { delta: BigInt(rows[0]!.deltaRaw), mint: rows[0]!.mint };
}

function finalizedLamportDelta(
  finalized: Awaited<ReturnType<typeof sendPreparedConfirmedOnce>>,
  account: string,
): bigint | null {
  const rows = finalized.lamportDeltas.filter((row) => row.address === account);
  return rows.length === 1 ? BigInt(rows[0]!.deltaRaw) : null;
}

function unexpectedManagerDeltas(
  finalized: Awaited<ReturnType<typeof sendPreparedConfirmedOnce>>,
  route: ReturnType<typeof partnerBuilderRoute>,
  accounts: Awaited<ReturnType<typeof deriveVoltrAccounts>>,
  reserve: ReserveGraph,
  strategyAssetAta: string,
): Readonly<{ token: readonly unknown[]; lamport: readonly unknown[] }> {
  const allowedTokenMints = new Map<string, string>([
    [accounts.idleAta, route.asset.mint],
    [strategyAssetAta, route.asset.mint],
    [reserve.reserveLiquiditySupply, route.asset.mint],
    [reserve.reserveCollateralSupplyVault, reserve.reserveCollateralMint],
  ]);
  const token = finalized.tokenDeltas.filter((row) =>
    allowedTokenMints.get(row.address) !== row.mint,
  );
  const lamport = finalized.lamportDeltas.filter((row) =>
    row.deltaRaw !== "0" && row.address !== route.squads.guardian,
  );
  return { token, lamport } as const;
}

function unexpectedManagerLamportDeltas(
  finalized: Awaited<ReturnType<typeof sendPreparedConfirmedOnce>>,
  route: ReturnType<typeof partnerBuilderRoute>,
  accounts: Awaited<ReturnType<typeof deriveVoltrAccounts>>,
  obligation: string,
  allowObligationTransition: boolean,
): readonly Readonly<{ address: string; deltaRaw: string }>[] {
  return finalized.lamportDeltas.filter((row) => row.deltaRaw !== "0"
    && row.address !== route.squads.guardian
    && !(allowObligationTransition && (row.address === accounts.strategyAuth || row.address === obligation)));
}

function sameJson(left: unknown, right: unknown): boolean {
  return JSON.stringify(left, (_key, value) => typeof value === "bigint" ? value.toString() : value)
    === JSON.stringify(right, (_key, value) => typeof value === "bigint" ? value.toString() : value);
}

function managerErrorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

function isManagerTransportError(error: unknown): boolean {
  const message = managerErrorMessage(error).toLowerCase();
  return message.includes("unable to connect")
    || message.includes("failed to fetch")
    || message.includes("fetch failed")
    || message.includes("econnreset")
    || message.includes("etimedout")
    || message.includes("socket hang up")
    || message.includes("network error")
    || message.includes("typo in the url or port");
}

/**
 * Label every pre-send read boundary. Only explicitly marked transport reads
 * get one bounded retry; semantic failures and all send paths are untouched.
 */
async function managerPreSendStage<T>(
  label: string,
  operation: () => Promise<T>,
  retryTransport = false,
): Promise<T> {
  const attempts = retryTransport ? 5 : 1;
  for (let attempt = 1; attempt <= attempts; attempt += 1) {
    try {
      return await operation();
    } catch (error) {
      if (attempt < attempts && isManagerTransportError(error)) {
        const delayMilliseconds = 150 * (2 ** (attempt - 1));
        await new Promise<void>((resolve) => setTimeout(resolve, delayMilliseconds));
        continue;
      }
      throw new Error(`manager pre-send stage ${label} failed: ${managerErrorMessage(error)}`);
    }
  }
  throw new Error(`manager pre-send stage ${label} failed`);
}

async function loadSettingsSeed(snapshot: AccountSnapshot | null): Promise<bigint | null> {
  if (!snapshot || snapshot.owner !== PARTNER_ROUTE.squads.program) return null;
  const SettingsAccount = (squadsGenerated as unknown as {
    Settings: { fromAccountInfo(account: { data: Buffer; executable: boolean; lamports: number; owner: PublicKey; rentEpoch: number }): readonly [{ policySeed: { toString(): string } | null }] };
  }).Settings;
  const [settings] = SettingsAccount.fromAccountInfo({
    data: Buffer.from(snapshot.data),
    executable: snapshot.executable,
    lamports: snapshot.lamports,
    owner: new PublicKey(snapshot.owner),
    rentEpoch: 0,
  });
  return BigInt(settings.policySeed?.toString() ?? "0");
}

async function verifyMessageEnvelope(
  prepared: PreparedTransaction,
  wrapper: ManagerWrapper,
): Promise<readonly Gate[]> {
  const gates: Gate[] = [];
  const tx = VersionedTransaction.deserialize(Buffer.from(prepared.serializedTransaction));
  const message = tx.message;
  const connection = new Connection(rpcUrl(), "confirmed");
  const altResponse = await connection.getAddressLookupTable(new PublicKey(PARTNER_ROUTE.lookupTable.address), { commitment: "confirmed" });
  const alt = altResponse.value;
  const accountKeys = message.getAccountKeys({ addressLookupTableAccounts: alt ? [alt] : [] });
  const instructions = message.compiledInstructions;
  const computeInstruction = instructions[0] ?? null;
  const heapInstruction = instructions[1] ?? null;
  const instruction = instructions[2] ?? null;
  const computeData = computeInstruction ? Buffer.from(computeInstruction.data) : Buffer.alloc(0);
  const heapData = heapInstruction ? Buffer.from(heapInstruction.data) : Buffer.alloc(0);
  add(gates, "exact compute limit, heap frame, then one manager wrapper", instructions.length === 3, instructions.length, 3);
  add(gates, "manager compute-budget program exact", computeInstruction ? accountKeys.get(computeInstruction.programIdIndex)?.toBase58() === ComputeBudgetProgram.programId.toBase58() : false, computeInstruction ? accountKeys.get(computeInstruction.programIdIndex)?.toBase58() : null, ComputeBudgetProgram.programId.toBase58());
  add(gates, "manager compute-unit limit exact", computeInstruction?.accountKeyIndexes.length === 0 && computeData.equals(Buffer.from(ComputeBudgetProgram.setComputeUnitLimit({ units: MANAGER_COMPUTE_UNIT_LIMIT }).data)), { accounts: computeInstruction?.accountKeyIndexes ?? null, dataHex: computeData.toString("hex") }, { accounts: [], dataHex: Buffer.from(ComputeBudgetProgram.setComputeUnitLimit({ units: MANAGER_COMPUTE_UNIT_LIMIT }).data).toString("hex") });
  add(gates, "manager heap-budget program exact", heapInstruction ? accountKeys.get(heapInstruction.programIdIndex)?.toBase58() === ComputeBudgetProgram.programId.toBase58() : false, heapInstruction ? accountKeys.get(heapInstruction.programIdIndex)?.toBase58() : null, ComputeBudgetProgram.programId.toBase58());
  add(gates, "manager heap frame exact", heapInstruction?.accountKeyIndexes.length === 0 && heapData.equals(Buffer.from(ComputeBudgetProgram.requestHeapFrame({ bytes: MANAGER_HEAP_FRAME_BYTES }).data)), { accounts: heapInstruction?.accountKeyIndexes ?? null, dataHex: heapData.toString("hex") }, { accounts: [], dataHex: Buffer.from(ComputeBudgetProgram.requestHeapFrame({ bytes: MANAGER_HEAP_FRAME_BYTES }).data).toString("hex") });
  add(gates, "one approved address lookup table", message.addressTableLookups.length === 1 && message.addressTableLookups[0]?.accountKey.toBase58() === PARTNER_ROUTE.lookupTable.address, message.addressTableLookups.map((lookup) => lookup.accountKey.toBase58()), [PARTNER_ROUTE.lookupTable.address]);
  add(gates, "approved lookup table exists", alt !== null, alt?.key.toBase58() ?? null, PARTNER_ROUTE.lookupTable.address);
  add(gates, "manager wrapper program exact", instruction ? accountKeys.get(instruction.programIdIndex)?.toBase58() === PARTNER_ROUTE.squads.program : false, instruction ? accountKeys.get(instruction.programIdIndex)?.toBase58() : null, PARTNER_ROUTE.squads.program);
  const observedAccounts = instruction
    ? instruction.accountKeyIndexes.map((index) => accountKeys.get(index)?.toBase58() ?? "<missing>")
    : [];
  const expectedAddresses = wrapper.expectedAccounts.map(({ address: value }) => value);
  add(gates, "manager wrapper account order exact", sameJson(observedAccounts, expectedAddresses), observedAccounts, expectedAddresses);
  const wrapperMetaRoles = wrapper.instruction.keys.map((meta) => ({
    address: meta.pubkey.toBase58(),
    signer: meta.isSigner,
    writable: meta.isWritable,
  }));
  add(
    gates,
    "manager wrapper instruction metas exact",
    sameJson(wrapperMetaRoles, wrapper.expectedAccounts),
    wrapperMetaRoles,
    wrapper.expectedAccounts,
  );
  const observedRoles = instruction
    ? instruction.accountKeyIndexes.map((index) => ({ signer: message.isAccountSigner(index), writable: message.isAccountWritable(index) }))
    : [];
  // The guardian is also the fee payer, so Solana's compiled message marks
  // that account writable even though the policy wrapper requests it readonly.
  // Compare the effective message role while preserving the exact per-ix
  // readonly role in the wrapper/artifact gate above.
  const expectedRoles = wrapper.expectedAccounts.map(({ address: value, signer, writable }) => ({
    signer,
    writable: writable || value === PARTNER_ROUTE.squads.guardian,
  }));
  add(gates, "compiled message account roles exact after fee-payer elevation", sameJson(observedRoles, expectedRoles), observedRoles, expectedRoles);
  const observedData = instruction ? Buffer.from(instruction.data) : Buffer.alloc(0);
  add(gates, "manager wrapper data hash exact", sha256(observedData) === wrapper.dataSha256, sha256(observedData), wrapper.dataSha256);
  add(gates, "sole transaction signer is guardian", message.header.numRequiredSignatures === 1 && message.isAccountSigner(0) && accountKeys.get(0)?.toBase58() === PARTNER_ROUTE.squads.guardian, { required: message.header.numRequiredSignatures, signer0: accountKeys.get(0)?.toBase58() }, { required: 1, signer0: PARTNER_ROUTE.squads.guardian });
  return gates;
}

async function deploymentGates(
  before: Awaited<ReturnType<typeof loadDeploymentIdentities>>,
  after: Awaited<ReturnType<typeof loadDeploymentIdentities>>,
): Promise<readonly Gate[]> {
  const gates: Gate[] = [];
  add(gates, "deployment context does not regress", after.contextSlot >= before.contextSlot, after.contextSlot, `>=${before.contextSlot}`);
  add(gates, "deployment identities unchanged", sameJson(before.identities, after.identities), after.identities, before.identities);
  return gates;
}

function routeInspectionAddresses(
  route: ReturnType<typeof partnerBuilderRoute>,
  accounts: Awaited<ReturnType<typeof deriveVoltrAccounts>>,
  graph: ReserveGraph,
  policy: string,
  strategyAssetAta: string,
  canonical: CanonicalInstruction,
): string[] {
  return unique([
    route.setupAdmin,
    route.squads.guardian,
    route.squads.settings,
    route.squads.manager,
    policy,
    route.vault,
    accounts.lpMint,
    accounts.idleAta,
    accounts.strategyAuth,
    strategyAssetAta,
    accounts.strategyInitReceipt,
    route.asset.mint,
    graph.reserve,
    graph.reserveLiquiditySupply,
    graph.reserveCollateralSupplyVault,
    graph.obligation,
    graph.obligationFarm,
    graph.userMetadata,
    graph.lendingMarket,
    graph.lendingMarketAuthority,
    graph.reserveCollateralMint,
    graph.scope,
    graph.reserveFarmState,
    route.lookupTable.address,
    ...canonical.accounts.map(({ address: account }) => account),
  ]);
}

type ManagerStateFenceClass =
  | "route-owned"
  | "shared-asset-mint"
  | "shared-kamino-reserve"
  | "shared-kamino-market"
  | "shared-scope"
  | "shared-farms";

function managerStateFenceClass(
  route: ReturnType<typeof partnerBuilderRoute>,
  graph: ReserveGraph,
  value: string,
): ManagerStateFenceClass {
  if (value === route.asset.mint) return "shared-asset-mint";
  if (value === graph.reserve || value === graph.reserveLiquiditySupply || value === graph.reserveCollateralSupplyVault) {
    return "shared-kamino-reserve";
  }
  if (value === graph.lendingMarket || value === graph.lendingMarketAuthority || value === graph.reserveCollateralMint) {
    return "shared-kamino-market";
  }
  if (value === graph.scope) return "shared-scope";
  if (value === graph.reserveFarmState) return "shared-farms";
  return "route-owned";
}

function changedManagerState(
  route: ReturnType<typeof partnerBuilderRoute>,
  graph: ReserveGraph,
  addresses: readonly string[],
  before: readonly (AccountSnapshot | null)[],
  after: readonly (AccountSnapshot | null)[],
): readonly Readonly<{ address: string; class: ManagerStateFenceClass }>[] {
  return addresses.flatMap((value, index) => {
    const stateClass = managerStateFenceClass(route, graph, value);
    // Shared Kamino/Scope/Farms accounts are intentionally volatile. Their
    // current graph and protocol semantics are reloaded below; byte fencing
    // them here makes a manager packet impossible to send during normal market
    // activity. Route-owned and policy-owned accounts remain exact byte fences.
    if (stateClass !== "route-owned") return [];
    const left = before[index] ?? null;
    const right = after[index] ?? null;
    const changed = (left === null) !== (right === null)
      || (left !== null && right !== null && !sameJson(fingerprint(left), fingerprint(right)));
    return changed ? [{ address: value, class: stateClass }] : [];
  });
}

function reserveGraphSemanticsEqual(left: ReserveGraph, right: ReserveGraph): boolean {
  return sameJson(left, right);
}

async function prepareManagerOperation(
  strategyId: PartnerStrategyId,
  operation: ManagerOperation,
  amountRaw: bigint,
  artifactPath: string,
  approval?: ManagerPreparationApproval,
) {
  const route = partnerBuilderRoute(strategyId);
  if (amountRaw <= 0n || amountRaw > route.asset.maxManagerOperationRaw) {
    throw new Error(`${strategyId} manager ${operation} amount must be in 1..${route.asset.maxManagerOperationRaw}`);
  }
  // The policy catalog is the only source of policy/account/data identity.
  // Callers provide only strategyId + operation + amount; all other bytes are
  // resolved from this independently hashed authorization envelope.
  const authorization = loadPolicyCatalogAuthorization(
    approval?.authorizationPath ?? policyCatalogAuthorizationPath(),
    artifactPath,
    approval?.confirmAuthorizationSha256,
  );
  const loaded = loadRuntimePolicyArtifact(artifactPath);
  const routeAuthorization = effectiveRouteAuthorizationDigest(loaded, {
    fileSha256: authorization.fileSha256,
    authorization: authorization.authorization,
  });
  if (approval?.confirmRouteAuthorizationSha256 !== undefined
    && approval.confirmRouteAuthorizationSha256 !== null
    && approval.confirmRouteAuthorizationSha256 !== routeAuthorization.sha256) {
    throw new Error(`execute manager operation requires --confirm-route-authorization-sha256 ${routeAuthorization.sha256}`);
  }
  const entryValue = loaded.artifact.policies.find((value) => value.operation === operation && value.strategyId === strategyId);
  if (!entryValue) throw new Error(`policy catalog has no ${strategyId} ${operation} entry`);
  const entry = managerEntry(operation, entryValue);
  const catalogEntry = authorization.authorization.entries.find((value) => value.operation === operation && value.strategyId === strategyId);
  if (!catalogEntry) throw new Error(`policy authorization has no ${strategyId} ${operation} entry`);
  const expectedPolicy = derivePolicy(route.squads.settings, BigInt(catalogEntry.seed));
  if (entry.seed !== catalogEntry.seed
    || entry.policy !== catalogEntry.policy
    || entry.policy !== expectedPolicy
    || catalogEntry.strategyGraphSha256 !== partnerStrategyGraphSha256(strategyId)
    || entry.innerInstructionDataSha256 !== catalogEntry.innerInstructionDataSha256
    || entry.managerExecution.dataSha256 !== catalogEntry.managerExecutionDataSha256) {
    throw new Error(`${operation} policy seed or PDA escaped RouteSpec`);
  }
  if (loaded.artifact.manager !== route.squads.manager || loaded.artifact.routeSpecSha256 !== fourMarketRouteSpecSha256()) {
    throw new Error("runtime policy artifact is not bound to the exact manager route");
  }
  const accounts = await managerPreSendStage("derive route accounts", () => deriveVoltrAccounts(route), true);
  const reserve = await managerPreSendStage("load shared Kamino reserve graph", () => loadMainReserveGraph(rpcUrl(), route, accounts.strategyAuth, "confirmed"), true);
  const builder = await managerPreSendStage("create canonical Voltr route builder", () => createVoltrRouteBuilder(route, reserve.graph), true);
  const manager = createNoopSigner(route.squads.manager);
  const voltr = operation === "deposit"
    ? await managerPreSendStage("build canonical Voltr deposit instruction", () => builder.strategy.deposit(manager, amountRaw), true)
    : await managerPreSendStage("build canonical Voltr withdraw instruction", () => builder.strategy.withdraw(manager, amountRaw), true);
  const sourceManifest = loaded.artifact.sourceManifests?.find((manifest) => manifest.strategyId === strategyId);
  const sourceInstruction = sourceManifest?.instructions?.[operation];
  const sourceData = sourceInstruction
    ? bytesFromBase64(sourceInstruction.dataBase64, `${operation} source manifest data`)
    : null;
  if (
    !sourceInstruction
    || !sourceData
    || sourceManifest?.routeSpecSha256 !== fourMarketRouteSpecSha256()
    || sourceInstruction.programId !== voltr.canonical.programId
    || sourceInstruction.dataLength !== 30
    || voltr.canonical.dataLength !== 30
    || sourceInstruction.dataLength !== sourceData.length
    || sourceInstruction.dataSha256 !== sha256(sourceData)
    || entry.innerInstructionDataSha256 !== sourceInstruction.dataSha256
    || !dynamicArtifactDataMatches(sourceData, voltr.canonical.data, amountRaw)
    || !sameJson(sourceInstruction.accounts, voltr.canonical.accounts)
  ) {
    throw new Error(`${operation} SDK-built Voltr instruction escaped the artifact outside its amount field`);
  }
  const wrapper = buildManagerWrapper(operation, entry, voltr.canonical, amountRaw);
  if (approval?.confirmArtifactSha256 !== undefined) {
    if (approval.confirmArtifactSha256 !== loaded.fileSha256) throw new Error(`execute manager operation requires --confirm-artifact-sha256 ${loaded.fileSha256}`);
  }
  if (approval?.confirmWrapperDataSha256 !== undefined) {
    if (approval.confirmWrapperDataSha256 !== wrapper.dataSha256) throw new Error(`execute manager operation requires --confirm-wrapper-data-sha256 ${wrapper.dataSha256}`);
  }
  const guardian = await managerPreSendStage("load guardian signing material", () => signingMaterialFromEnvironment("YIELD_ROUTER_KEYPAIR"));
  if (guardian.signer.address !== route.squads.guardian) throw new Error("guardian signer does not match RouteSpec");
  const strategyAssetAta = voltr.canonical.accounts.find(({ label }) => label === "vaultStrategyAssetAta")?.address;
  if (!strategyAssetAta) throw new Error(`${operation} SDK instruction is missing vaultStrategyAssetAta`);
  const addresses = routeInspectionAddresses(route, accounts, reserve.graph, entry.policy, strategyAssetAta, voltr.canonical);
  // Solana's `simulateTransaction.accounts` return list is capped at 31
  // addresses. Keep the full list above for exact prestate protection and
  // packet/account identity checks, but request post-state images only for the
  // accounts used by the manager's economic gates.
  const simulatedAddresses = unique([
    accounts.idleAta,
    accounts.strategyInitReceipt,
    strategyAssetAta,
    route.squads.guardian,
    reserve.graph.obligation,
    reserve.graph.reserveLiquiditySupply,
    reserve.graph.reserveCollateralSupplyVault,
  ]);
  const minimumContextSlot = approval?.minimumContextSlot;
  const beforeSnapshot = await managerPreSendStage("load confirmed manager prestate", () => confirmedSnapshots(
    rpcUrl(),
    addresses,
    minimumContextSlot === undefined ? reserve.contextSlot : Math.max(reserve.contextSlot, minimumContextSlot),
  ), true);
  const before: SnapshotSet = { ...beforeSnapshot, addresses };
  const protectedBefore = await managerPreSendStage(
    "load exact four-market protected prestate",
    () => loadFourMarketProtectedState(rpcUrl(), before.contextSlot),
    true,
  );
  const deploymentBefore = await managerPreSendStage("load confirmed prestate deployments", () => loadDeploymentIdentities(rpcUrl(), route, before.contextSlot, "confirmed"), true);
  const settingsIndex = addresses.indexOf(route.squads.settings);
  const policyIndex = addresses.indexOf(entry.policy);
  const vaultIndex = addresses.indexOf(route.vault);
  const idleIndex = addresses.indexOf(accounts.idleAta);
  const receiptIndex = addresses.indexOf(accounts.strategyInitReceipt);
  const settingsSeed = await loadSettingsSeed(before.accounts[settingsIndex] ?? null);
  const catalogFirstSeed = BigInt(loaded.artifact.policies[0]?.seed ?? "0");
  const catalogLastSeed = BigInt(authorization.authorization.terminalPolicySeed);
  const nonCatalogIsolation = await managerPreSendStage(
    "prove every non-catalog Squads policy is isolated from Voltr",
    () => verifyNonCatalogSquadsPoliciesIsolated(rpcUrl(), catalogFirstSeed, catalogLastSeed, before.contextSlot, "confirmed"),
    true,
  );
  const lpMintIndex = addresses.indexOf(accounts.lpMint);
  const vaultSnapshot = verifyVaultCurrentState({ route, accounts, vault: before.accounts[vaultIndex] ?? null, lpMint: before.accounts[lpMintIndex] ?? null, idleAta: before.accounts[idleIndex] ?? null, assetMint: before.accounts[addresses.indexOf(route.asset.mint)] ?? null });
  const gates: Gate[] = [];
  gates.push(...verifyDeploymentIdentities(route, deploymentBefore.identities));
  add(gates, "manager route signer is guardian", guardian.signer.address === route.squads.guardian, guardian.signer.address, route.squads.guardian);
  add(gates, "Settings includes the complete approved Voltr policy catalog", settingsSeed !== null && settingsSeed >= catalogLastSeed, settingsSeed, `>=${catalogLastSeed}`);
  add(gates, "Settings seed matches the stable non-catalog isolation scan", settingsSeed === nonCatalogIsolation.currentSeed, { managerSnapshot: settingsSeed, isolationScan: nonCatalogIsolation.currentSeed }, "equal");
  gates.push(...nonCatalogIsolation.gates.map((gate) => ({ ...gate, name: `Squads isolation: ${gate.name}` })));
  add(gates, "policy PDA exists and is Squads-owned", before.accounts[policyIndex]?.owner === route.squads.program, before.accounts[policyIndex]?.owner ?? null, route.squads.program);
  add(gates, "Voltr current vault manager route", vaultSnapshot.state?.manager === route.squads.manager, vaultSnapshot.state?.manager ?? null, route.squads.manager);
  add(gates, "reserve graph context is fresh", reserve.contextSlot <= before.contextSlot, reserve.contextSlot, `<=${before.contextSlot}`);
  add(gates, "manager inner instruction amount bound", amountRaw <= route.asset.maxManagerOperationRaw, amountRaw, route.asset.maxManagerOperationRaw);
  const idleBefore = tokenAmount(before.accounts[idleIndex] ?? null);
  const positionBefore = strategyPosition(before.accounts[receiptIndex] ?? null);
  const strategyAssetBefore = tokenAmount(before.accounts[addresses.indexOf(strategyAssetAta)] ?? null);
  add(gates, "prestate idle token decodes", idleBefore !== null, idleBefore, "decoded");
  add(gates, "prestate strategy receipt decodes", positionBefore !== null, positionBefore, "decoded");
  if (operation === "deposit") add(gates, "prestate idle covers manager amount", idleBefore !== null && idleBefore >= amountRaw, idleBefore, `>=${amountRaw}`);
  if (operation === "withdraw") add(gates, "prestate strategy plus bounded transient value covers manager amount within one-raw-unit floor", positionBefore !== null && strategyAssetBefore !== null && strategyAssetBefore >= 0n && strategyAssetBefore <= 1n && positionBefore + strategyAssetBefore >= amountRaw - 1n, { positionBefore, strategyAssetBefore }, `position+transient>=${amountRaw - 1n}; transient=0..1`);
  const prepared = await managerPreSendStage("compile and simulate canonical v0 manager packet", () => prepareSignedV0Transaction({
    rpcUrl: rpcUrl(),
    feePayer: guardian,
    instructions: [
      fromWeb3Instruction(ComputeBudgetProgram.setComputeUnitLimit({ units: MANAGER_COMPUTE_UNIT_LIMIT })),
      fromWeb3Instruction(ComputeBudgetProgram.requestHeapFrame({ bytes: MANAGER_HEAP_FRAME_BYTES })),
      fromWeb3Instruction(wrapper.instruction),
    ],
    lookupTableAddresses: [route.lookupTable.address],
    prestateAddresses: addresses,
    inspectedAddresses: simulatedAddresses,
    minimumContextSlot: Math.max(before.contextSlot, protectedBefore.contextSlot),
    commitment: "confirmed",
  }), true);
  const protectedAfter = await managerPreSendStage(
    "load exact four-market protected simulation poststate",
    () => loadFourMarketProtectedState(rpcUrl(), prepared.simulationSlot),
    true,
  );
  const protectedEvidence = protectedSnapshotEvidenceEnvelope(protectedBefore, protectedAfter);
  const deploymentAfter = await managerPreSendStage("load confirmed post-simulation deployments", () => loadDeploymentIdentities(rpcUrl(), route, prepared.simulationSlot, "confirmed"), true);
  gates.push(...await verifyMessageEnvelope(prepared, wrapper));
  gates.push(...await deploymentGates(deploymentBefore, deploymentAfter));
  add(gates, "simulation succeeded", prepared.simulation.err === null, prepared.simulation.err, null);
  add(gates, "simulation context does not regress", prepared.simulationSlot >= before.contextSlot, prepared.simulationSlot, `>=${before.contextSlot}`);
  add(gates, "simulation did not race the common protected state", protectedAfter.addressSetSha256 === protectedBefore.addressSetSha256 && protectedAfter.stateSha256 === protectedBefore.stateSha256, { before: { contextSlot: protectedBefore.contextSlot, addressSetSha256: protectedBefore.addressSetSha256, stateSha256: protectedBefore.stateSha256 }, after: { contextSlot: protectedAfter.contextSlot, addressSetSha256: protectedAfter.addressSetSha256, stateSha256: protectedAfter.stateSha256 } }, "same address set and state hash");
  add(gates, "prepared prestate reaches initial snapshot", prepared.prestateSlot >= before.contextSlot, prepared.prestateSlot, `>=${before.contextSlot}`);
  add(gates, "policy manager execution artifact bytes exact outside amount", wrapper.artifactDataMatches, wrapper.dataSha256, entry.managerExecution.dataSha256);
  add(gates, "compiled packet within Solana limit", prepared.packetBytes <= 1_232, prepared.packetBytes, "<=1232");
  add(gates, "manager SOL fee bounded", prepared.feeLamports > 0 && prepared.feeLamports <= 100_000, prepared.feeLamports, "1..100000 lamports");
  const post = accountMap({ addresses: simulatedAddresses, accounts: prepared.simulation.postAccounts, contextSlot: prepared.simulationSlot });
  const idleAfter = tokenAmount(post.get(accounts.idleAta) ?? null);
  const positionAfter = strategyPosition(post.get(accounts.strategyInitReceipt) ?? null);
  const strategyAssetAfter = tokenAmount(post.get(strategyAssetAta) ?? null);
  const guardianBefore = before.accounts[addresses.indexOf(route.squads.guardian)]?.lamports ?? null;
  const guardianAfter = post.get(route.squads.guardian)?.lamports ?? null;
  const strategyEvent = strategyEventPayload(prepared.simulation.logs, operation);
  add(gates, "simulation guardian pays exact quoted fee", guardianBefore !== null && guardianAfter === guardianBefore - prepared.feeLamports, { before: guardianBefore, after: guardianAfter }, `after=before-${prepared.feeLamports}`);
  if (operation === "deposit") {
    const reserveLog = `Program log: DepositReserveLiquidityAndObligationCollateral Reserve ${reserve.graph.reserve} amount ${amountRaw}`;
    const pnl = prepared.simulation.logs.map((line) => /^Program log: pnl: Deposit reserve liquidity (\d+) and obligation collateral (\d+)$/.exec(line)).filter((value): value is RegExpExecArray => value !== null);
    const depositedRaw = pnl.length === 1 ? BigInt(pnl[0]![1]!) : null;
    add(gates, "simulation idle USDC decreases exactly", idleBefore !== null && idleAfter === idleBefore - amountRaw, { before: idleBefore, after: idleAfter }, `after=before-${amountRaw}`);
    add(gates, `simulation exact ${strategyId} reserve deposit log`, prepared.simulation.logs.filter((line) => line === reserveLog).length === 1, prepared.simulation.logs.filter((line) => line.includes("DepositReserveLiquidityAndObligationCollateral Reserve")), [reserveLog]);
    add(gates, "simulation deposit amount is exactly conserved across reserve and bounded transient dust", depositedRaw !== null && depositedRaw > 0n && BigInt(pnl[0]![2]!) > 0n && strategyAssetBefore === 0n && strategyAssetAfter !== null && strategyAssetAfter >= 0n && strategyAssetAfter <= 1n && depositedRaw + strategyAssetAfter === amountRaw, { pnl: pnl.map((value) => ({ liquidityRaw: value[1], collateralRaw: value[2] })), strategyAssetBefore, strategyAssetAfter }, { liquidityRaw: "requestedRaw-transientDust", collateralRaw: ">0", transientDustRaw: "0..1", conservation: amountRaw });
    add(gates, "simulation strategy position increases", positionBefore !== null && positionAfter !== null && positionAfter >= positionBefore, { before: positionBefore, after: positionAfter }, ">=before");
  } else {
    const pnl = prepared.simulation.logs.map((line) => /^Program log: pnl: Withdraw obligation collateral (\d+) and redeem reserve collateral (\d+)$/.exec(line)).filter((value): value is RegExpExecArray => value !== null);
    const redeemedRaw = pnl.length === 1 ? BigInt(pnl[0]![2]!) : null;
    const transientReleasedRaw = strategyAssetBefore === null || strategyAssetAfter === null ? null : strategyAssetBefore - strategyAssetAfter;
    const eventAmount = bigintEventField(strategyEvent, "vaultAmountAssetWithdrawn");
    const eventIdleBefore = bigintEventField(strategyEvent, "vaultAssetIdleAtaAmountBefore");
    const eventIdleAfter = bigintEventField(strategyEvent, "vaultAssetIdleAtaAmountAfter");
    const eventPositionBefore = bigintEventField(strategyEvent, "strategyPositionValueBefore");
    const eventPositionAfter = bigintEventField(strategyEvent, "strategyPositionValueAfter");
    const eventTotalBefore = bigintEventField(strategyEvent, "vaultAssetTotalValueBefore");
    const eventTotalAfter = bigintEventField(strategyEvent, "vaultAssetTotalValueAfter");
    const idleIncrease = eventIdleBefore === null || eventIdleAfter === null ? null : eventIdleAfter - eventIdleBefore;
    const positionDecrease = eventPositionBefore === null || eventPositionAfter === null ? null : eventPositionBefore - eventPositionAfter;
    const totalValueIncrease = eventTotalBefore === null || eventTotalAfter === null ? null : eventTotalAfter - eventTotalBefore;
    add(gates, "simulation idle USDC exactly equals reserve redemption plus released transient dust", idleBefore !== null && idleAfter !== null && redeemedRaw !== null && transientReleasedRaw !== null && transientReleasedRaw >= 0n && transientReleasedRaw <= 1n && idleAfter === idleBefore + redeemedRaw + transientReleasedRaw, { before: idleBefore, after: idleAfter, redeemedRaw, transientReleasedRaw }, `after=before+redeemedRaw+transientReleasedRaw; transient=0..1`);
    add(gates, "simulation exact requested withdraw amount and positive accrued redemption", pnl.length === 1 && BigInt(pnl[0]![1]!) > 0n && eventAmount !== null && eventAmount >= amountRaw - 1n && eventAmount <= amountRaw && redeemedRaw !== null && redeemedRaw > 0n && transientReleasedRaw !== null && redeemedRaw + transientReleasedRaw >= eventAmount - 1n, { pnl: pnl.map((value) => ({ collateralRaw: value[1], liquidityRaw: value[2] })), eventAmount, transientReleasedRaw }, { collateralRaw: ">0", requestedAmountRaw: `${amountRaw - 1n}..${amountRaw}`, redeemedPlusTransientRaw: ">=requestedAmountRaw-1; accrued yield allowed" });
    add(gates, "simulation exact Voltr withdraw event conservation", strategyEvent !== null
      && strategyEvent.manager === route.squads.manager
      && strategyEvent.vault === route.vault
      && strategyEvent.strategy === reserve.graph.reserve
      && strategyEvent.strategyInitReceipt === accounts.strategyInitReceipt
      && strategyEvent.adaptorProgram === route.programs.kaminoAdaptor
      && strategyEvent.vaultAssetMint === route.asset.mint
      && eventAmount !== null
      && idleIncrease !== null
      && redeemedRaw !== null
      && transientReleasedRaw !== null
      && redeemedRaw + transientReleasedRaw === idleIncrease
      && positionDecrease !== null
      && positionDecrease >= 0n
      && totalValueIncrease === idleIncrease - positionDecrease - transientReleasedRaw
      && strategyEvent.vaultLpSupplyInclFeesBefore === strategyEvent.vaultLpSupplyInclFeesAfter,
    { event: strategyEvent, idleIncrease, positionDecrease, transientReleasedRaw, totalValueIncrease },
    { manager: route.squads.manager, vault: route.vault, strategy: reserve.graph.reserve, strategyInitReceipt: accounts.strategyInitReceipt, adaptorProgram: route.programs.kaminoAdaptor, assetMint: route.asset.mint, idleIncrease: "reserve redemption plus released transient", positionDecrease: ">=0", transientReleasedRaw: "0..1", totalValueIncrease: "idleIncrease-positionDecrease-transientReleased", lpSupply: "unchanged" });
    add(gates, "simulation strategy position decreases", positionBefore !== null && positionAfter !== null && positionAfter <= positionBefore, { before: positionBefore, after: positionAfter }, "<=before");
  }
  add(gates, operation === "deposit" ? "simulation transient strategy USDC ATA is exactly bounded and conserved" : "simulation transient strategy USDC ATA releases at most one raw unit and ends empty", operation === "deposit" ? strategyAssetBefore === 0n && strategyAssetAfter !== null && strategyAssetAfter >= 0n && strategyAssetAfter <= 1n : strategyAssetBefore !== null && strategyAssetBefore >= 0n && strategyAssetBefore <= 1n && strategyAssetAfter === 0n, { before: strategyAssetBefore, after: strategyAssetAfter }, operation === "deposit" ? { before: 0n, after: "0..1; exactly conserved with reserve deposit" } : { before: "0..1", after: 0n });
  add(gates, "simulation manager remains the Squads PDA", vaultSnapshot.state?.manager === route.squads.manager, vaultSnapshot.state?.manager ?? null, route.squads.manager);
  const canonicalMessageSha256 = sha256(prepared.serializedMessage);
  const intent: ManagerRuntimeIntent = {
    schemaVersion: 1,
    kind: "runtime",
    operation: operation === "deposit" ? "manager-deposit" : "manager-withdraw",
    routeId: PARTNER_FOUR_MARKET_ROUTE.id,
    routeSpecSha256: fourMarketRouteSpecSha256(),
    signerRole: "guardian",
    guardian: route.squads.guardian,
    policy: address(entry.policy),
    amountRaw,
    nonce: `${operation}:${entry.policy}:${prepared.expectedSignature}`,
    prestateSlot: BigInt(prepared.prestateSlot),
    expiresAtUnix: BigInt(Math.floor(Date.now() / 1_000) + 300),
    canonicalMessageSha256,
    lifecycleId: approval?.lifecycleId ?? sha256(Buffer.from(`simulation:${PARTNER_FOUR_MARKET_ROUTE.id}:${operation}:${entry.policy}:${prepared.expectedSignature}`)),
    protectedPrestateSha256: protectedBefore.stateSha256,
    routeAuthorizationSha256: routeAuthorization.sha256,
  };
  assertIntentForRouteBinding(intent, {
    routeId: PARTNER_FOUR_MARKET_ROUTE.id,
    routeSpecSha256: fourMarketRouteSpecSha256(),
    maxManagerOperationRaw: route.asset.maxManagerOperationRaw,
    routeAuthorizationSha256: routeAuthorization.sha256,
  });
  const digest = intentSha256(intent);
  const failedGateCount = gates.filter(({ pass }) => !pass).length;
  return {
    operation,
    amountRaw,
    loaded,
    authorization,
    entry,
    reserve,
    accounts,
    simulatedAddresses,
    strategyAssetAta,
    guardian,
    wrapper,
    prepared,
    before,
    deploymentBefore,
    deploymentAfter,
    nonCatalogIsolation,
    routeAuthorization,
    protectedBefore,
    protectedAfter,
    protectedEvidence,
    intent,
    intentSha256: digest,
    report: {
      verdict: failedGateCount === 0 ? "PARTNER_MANAGER_OPERATION_SIMULATION_PASS" : "PARTNER_MANAGER_OPERATION_SIMULATION_FAIL",
      broadcast: false,
      readyForBroadcast: failedGateCount === 0,
      routeSpecSha256: fourMarketRouteSpecSha256(),
      artifact: { path: loaded.path, fileSha256: loaded.fileSha256, artifactSha256: loaded.artifact.artifactSha256, sourceManifestSha256: loaded.artifact.sourceManifestSha256 },
      lifecycleId: intent.lifecycleId,
      routeAuthorizationSha256: routeAuthorization.sha256,
      protectedPrestateSha256: protectedBefore.stateSha256,
      protectedAddressSetSha256: protectedBefore.addressSetSha256,
      protectedSnapshotEvidence: protectedEvidence,
      squadsIsolation: {
        contextSlot: nonCatalogIsolation.contextSlot,
        currentSeed: nonCatalogIsolation.currentSeed,
        catalogFirstSeed: nonCatalogIsolation.catalogFirstSeed,
        catalogLastSeed: nonCatalogIsolation.catalogLastSeed,
        policies: nonCatalogIsolation.policies,
      },
      transaction: {
        operation: `manager-${operation}`,
        amountRaw: amountRaw.toString(),
        policy: entry.policy,
        policySeed: entry.seed,
        guardian: route.squads.guardian,
        manager: route.squads.manager,
        packetBytes: prepared.packetBytes,
        feeLamports: prepared.feeLamports,
        expectedSignature: prepared.expectedSignature,
        wrapperDataSha256: wrapper.dataSha256,
        innerDataSha256: voltr.canonical.dataSha256,
        canonicalMessageSha256,
      },
      simulation: {
        prestateSlot: prepared.prestateSlot,
        contextSlot: prepared.simulationSlot,
        err: prepared.simulation.err,
        unitsConsumed: prepared.simulation.unitsConsumed,
        logs: prepared.simulation.logs,
        logsSha256: sha256(Buffer.from(prepared.simulation.logs.join("\n"))),
        idleBefore,
        idleAfter,
        strategyPositionBefore: positionBefore,
        strategyPositionAfter: positionAfter,
      },
      failedGateCount,
      gates,
    },
  } as const;
}

export async function simulateManagerOperation(
  strategyId: PartnerStrategyId,
  operation: ManagerOperation,
  amountRaw: bigint,
  artifactPath: string,
  authorizationPath?: string | null,
) {
  return (await prepareManagerOperation(strategyId, operation, amountRaw, artifactPath, {
    authorizationPath: authorizationPath ?? null,
  })).report;
}

/**
 * Read-only reconciliation for a confirmed manager signature. This path does
 * not load a signer, rebuild a packet, or resend anything; it exists so a
 * confirmed first-deposit obligation rent transfer can be independently
 * classified after the sender's post-send readback.
 */
export async function reconcileConfirmedManagerOperation(input: Readonly<{
  strategyId: PartnerStrategyId;
  operation: ManagerOperation;
  signature: string;
}>): Promise<Readonly<Record<string, unknown>>> {
  const route = partnerBuilderRoute(input.strategyId);
  if (!input.signature) throw new Error("manager reconciliation requires a confirmed transaction signature");
  const accounts = await deriveVoltrAccounts(route);
  const reserve = await loadMainReserveGraph(rpcUrl(), route, accounts.strategyAuth, "confirmed");
  const connection = new Connection(rpcUrl(), "confirmed");
  const transaction = await connection.getTransaction(input.signature, { commitment: "confirmed", maxSupportedTransactionVersion: 0 });
  const gates: Gate[] = [];
  if (!transaction || !transaction.meta) {
    add(gates, "confirmed manager transaction is readable and successful", false, transaction ? { slot: transaction.slot, err: transaction.meta?.err ?? null } : null, "confirmed transaction with metadata");
    return { verdict: "PARTNER_MANAGER_RECONCILIATION_FAIL", broadcast: false, signature: input.signature, failedGateCount: gates.length, gates };
  }
  add(gates, "confirmed manager transaction succeeded", transaction.meta.err === null, transaction.meta.err, null);
  const keys = [
    ...transaction.transaction.message.staticAccountKeys,
    ...(transaction.meta.loadedAddresses?.writable ?? []),
    ...(transaction.meta.loadedAddresses?.readonly ?? []),
  ].map((key) => key.toBase58());
  const indexOf = (value: string): number => keys.indexOf(value);
  const lamportDelta = (value: string): bigint | null => {
    const index = indexOf(value);
    return index < 0 ? null : BigInt(transaction.meta!.postBalances[index]!) - BigInt(transaction.meta!.preBalances[index]!);
  };
  const obligationIndex = indexOf(reserve.graph.obligation);
  const obligationPreLamports = obligationIndex < 0 ? null : BigInt(transaction.meta.preBalances[obligationIndex]!);
  const post = await confirmedSnapshots(rpcUrl(), [reserve.graph.obligation], transaction.slot);
  const obligationAfter = post.accounts[0] ?? null;
  const obligationRentLamports = await rentExemptionLamports(rpcUrl(), KAMINO_OBLIGATION_DATA_LENGTH);
  const strategyAuthDelta = lamportDelta(accounts.strategyAuth);
  const obligationDelta = lamportDelta(reserve.graph.obligation);
  const guardianDelta = lamportDelta(route.squads.guardian);
  const initializationCandidate = input.operation === "deposit" && obligationPreLamports === 0n && obligationAfter !== null;
  const closureCandidate = input.operation === "withdraw"
    && obligationPreLamports === BigInt(obligationRentLamports)
    && obligationAfter === null;
  let decodedObligation: { owner: string; lendingMarket: string } | null = null;
  if (obligationAfter !== null) {
    try {
      const decoded = Obligation.decode(Buffer.from(obligationAfter.data));
      decodedObligation = { owner: decoded.owner.toString(), lendingMarket: decoded.lendingMarket.toString() };
    } catch {
      decodedObligation = null;
    }
  }
  const exactInitialization = !initializationCandidate || (
    obligationAfter?.address === reserve.graph.obligation
    && obligationAfter.owner === route.programs.klend
    && obligationAfter.data.length === KAMINO_OBLIGATION_DATA_LENGTH
    && obligationAfter.lamports === obligationRentLamports
    && strategyAuthDelta === -BigInt(obligationRentLamports)
    && obligationDelta === BigInt(obligationRentLamports)
    && decodedObligation?.owner === accounts.strategyAuth
    && decodedObligation.lendingMarket === route.strategy.lendingMarket
  );
  // Confirmed RPC does not retain closed account bytes. The live execute
  // path verifies those pre-close bytes; reconciliation proves the exact
  // rent-sized PDA refund and route-bound strategyAuth pair from tx meta.
  const exactClosure = !closureCandidate || (
    obligationPreLamports === BigInt(obligationRentLamports)
    && obligationAfter === null
    && strategyAuthDelta === BigInt(obligationRentLamports)
    && obligationDelta === -BigInt(obligationRentLamports)
  );
  const unexpectedLamports = keys.flatMap((value, index) => {
    const delta = BigInt(transaction.meta!.postBalances[index]!) - BigInt(transaction.meta!.preBalances[index]!);
    if (delta === 0n || value === route.squads.guardian) return [];
    if ((initializationCandidate || closureCandidate) && (value === accounts.strategyAuth || value === reserve.graph.obligation)) return [];
    return [{ address: value, deltaRaw: delta.toString() }];
  });
  add(gates, "confirmed obligation initialization is absent or exact", exactInitialization, { candidate: initializationCandidate, preLamports: obligationPreLamports?.toString() ?? null, post: obligationAfter ? { address: obligationAfter.address, owner: obligationAfter.owner, dataLength: obligationAfter.data.length, lamports: obligationAfter.lamports } : null, obligationRentLamports, strategyAuthDelta, obligationDelta, decodedObligation }, "zero/uninitialized prestate or exact KLend obligation PDA/owner/size/rent and strategyAuth -> obligation transfer");
  add(gates, "confirmed terminal obligation closure/refund is exact", exactClosure, { candidate: closureCandidate, preLamports: obligationPreLamports?.toString() ?? null, post: obligationAfter?.address ?? null, obligationRentLamports, strategyAuthDelta, obligationDelta }, "full withdraw may refund only the exact route obligation rent pair; historical pre-close account bytes are checked by live readback");
  add(gates, "confirmed manager lamport closure exact", unexpectedLamports.length === 0, unexpectedLamports, initializationCandidate ? "guardian fee plus exact obligation rent pair" : closureCandidate ? "guardian fee plus exact terminal-obligation refund pair" : "guardian fee only");
  add(gates, "confirmed guardian fee debit exact", transaction.meta.fee !== null && guardianDelta === -BigInt(transaction.meta.fee), { guardianDelta, feeLamports: transaction.meta.fee }, "guardian transaction fee");
  const failedGateCount = gates.filter(({ pass }) => !pass).length;
  return { verdict: failedGateCount === 0 ? "PARTNER_MANAGER_RECONCILIATION_PASS" : "PARTNER_MANAGER_RECONCILIATION_FAIL", broadcast: false, signature: input.signature, slot: transaction.slot, contextSlot: post.contextSlot, failedGateCount, gates, obligationInitialization: { candidate: initializationCandidate, obligationRentLamports, strategyAuthDelta, obligationDelta, obligationPreLamports, obligationAfter: obligationAfter ? { address: obligationAfter.address, owner: obligationAfter.owner, dataLength: obligationAfter.data.length, lamports: obligationAfter.lamports } : null, decodedObligation }, obligationClosure: { candidate: closureCandidate, exact: exactClosure, obligationRentLamports, strategyAuthDelta, obligationDelta, obligationPreLamports, obligationAfter: obligationAfter?.address ?? null, historicalPrestateBytesChecked: false }, lamportDeltas: keys.map((value, index) => ({ address: value, deltaRaw: (BigInt(transaction.meta!.postBalances[index]!) - BigInt(transaction.meta!.preBalances[index]!)).toString() })) };
}

export async function executeManagerOperation(input: Readonly<{
  strategyId: PartnerStrategyId;
  operation: ManagerOperation;
  amountRaw: bigint;
  artifactPath: string;
  authorizationPath?: string | null;
  confirmAuthorizationSha256: string | null;
  confirmRouteAuthorizationSha256: string | null;
  lifecycleId: string | null;
  confirmVault: string | null;
  confirmArtifactSha256: string | null;
  confirmAmountRaw: string | null;
  confirmWrapperDataSha256: string | null;
  intentPath: string | null;
  restorationBridge?: ManagerRestorationBridgeInput | null;
}>) {
  const route = partnerBuilderRoute(input.strategyId);
  const restorationBridge = normalizeRestorationBridgeInput(input.restorationBridge, input.strategyId, input.operation);
  if (process.env.CONFIRM_MAINNET !== "1") throw new Error("execute manager operation requires CONFIRM_MAINNET=1");
  if (input.confirmVault !== route.vault) throw new Error(`execute manager operation requires --confirm-vault ${route.vault}`);
  if (input.confirmAmountRaw !== input.amountRaw.toString()) throw new Error(`execute manager operation requires --confirm-amount-raw ${input.amountRaw}`);
  if (!input.confirmAuthorizationSha256) throw new Error("execute manager operation requires --confirm-authorization-sha256");
  if (!input.confirmRouteAuthorizationSha256) throw new Error("execute manager operation requires --confirm-route-authorization-sha256");
  if (!input.lifecycleId || !/^[0-9a-f]{64}$/.test(input.lifecycleId)) throw new Error("execute manager operation requires --lifecycle-id as a lowercase SHA-256 digest");
  if (!input.intentPath) throw new Error("execute manager operation requires --intent-path");
  // Authorization is a caller approval boundary, not packet-construction
  // input. Validate it before RPC, then compare the canonical preparation to
  // the same immutable envelope after every rebuild.
  const approvedAuthorization = loadPolicyCatalogAuthorization(
    input.authorizationPath ?? policyCatalogAuthorizationPath(),
    input.artifactPath,
    input.confirmAuthorizationSha256,
  );
  const initialPreparation = await managerPreSendStage("initial canonical preparation and simulation", () => prepareManagerOperation(input.strategyId, input.operation, input.amountRaw, input.artifactPath, {
    authorizationPath: input.authorizationPath ?? null,
    confirmRouteAuthorizationSha256: input.confirmRouteAuthorizationSha256,
    lifecycleId: input.lifecycleId ?? undefined,
  }));
  if (initialPreparation.authorization.fileSha256 !== approvedAuthorization.fileSha256
    || initialPreparation.authorization.authorization.authorizationSha256 !== approvedAuthorization.authorization.authorizationSha256) {
    throw new Error("manager canonical preparation authorization changed after approval");
  }
  if (initialPreparation.routeAuthorization.sha256 !== input.confirmRouteAuthorizationSha256
    || initialPreparation.intent.routeAuthorizationSha256 !== input.confirmRouteAuthorizationSha256
    || initialPreparation.intent.lifecycleId !== input.lifecycleId) {
    throw new Error("manager canonical preparation escaped the explicitly confirmed lifecycle/route authorization");
  }
  if (input.confirmArtifactSha256 !== initialPreparation.loaded.fileSha256) throw new Error(`execute manager operation requires --confirm-artifact-sha256 ${initialPreparation.loaded.fileSha256}`);
  if (input.confirmWrapperDataSha256 !== initialPreparation.wrapper.dataSha256) throw new Error(`execute manager operation requires --confirm-wrapper-data-sha256 ${initialPreparation.wrapper.dataSha256}`);
  if (!initialPreparation.report.readyForBroadcast || initialPreparation.report.failedGateCount !== 0) {
    throw new Error(`manager operation preflight failed with ${initialPreparation.report.verdict}`);
  }
  const refreshed = await managerPreSendStage("initial route-owned state refresh", () => confirmedSnapshots(rpcUrl(), initialPreparation.before.addresses, initialPreparation.prepared.simulationSlot), true);
  const changed = changedManagerState(
    route,
    initialPreparation.reserve.graph,
    initialPreparation.before.addresses,
    initialPreparation.before.accounts,
    refreshed.accounts,
  );
  if (changed.length > 0 || refreshed.contextSlot < initialPreparation.prepared.simulationSlot) {
    throw new Error(`manager route-owned state changed after simulation; refusing send (${changed.map(({ address: value, class: stateClass }) => `${stateClass}:${value}`).join(", ") || `context-slot:${refreshed.contextSlot}<${initialPreparation.prepared.simulationSlot}`})`);
  }
  const refreshedGraph = await managerPreSendStage("initial shared Kamino graph refresh", () => loadMainReserveGraph(rpcUrl(), route, initialPreparation.accounts.strategyAuth, "confirmed"), true);
  if (refreshedGraph.contextSlot < initialPreparation.reserve.contextSlot
    || !reserveGraphSemanticsEqual(initialPreparation.reserve.graph, refreshedGraph.graph)) {
    throw new Error(`manager shared Kamino graph changed after simulation; refusing send (before=${JSON.stringify(initialPreparation.reserve.graph)} after=${JSON.stringify(refreshedGraph.graph)})`);
  }
  const refreshedDeployments = await managerPreSendStage("initial deployment identity refresh", () => loadDeploymentIdentities(rpcUrl(), route, refreshed.contextSlot, "confirmed"), true);
  if (!verifyDeploymentIdentities(route, refreshedDeployments.identities).every(({ pass }) => pass)
    || !sameJson(initialPreparation.deploymentBefore.identities, refreshedDeployments.identities)) {
    throw new Error("manager deployment identity changed after simulation; refusing send");
  }
  const livePolicies = await managerPreSendStage("confirmed runtime-policy semantic proof", () => verifyExistingRuntimePolicies(input.artifactPath, refreshed.contextSlot, "confirmed"), true);
  if (livePolicies.commitment !== "confirmed" || livePolicies.verdict !== "PARTNER_RUNTIME_POLICIES_CONFIRMED_PASS" || livePolicies.failedGateCount !== 0) {
    throw new Error("live manager policy semantics changed after simulation; refusing send");
  }
  const managerAuthorizationContextSlot = Math.max(
    refreshed.contextSlot,
    refreshedDeployments.contextSlot,
    livePolicies.contextSlot,
    livePolicies.deploymentContextSlot,
  );
  // Rebuild and re-simulate after the authorization snapshot. This reruns the
  // full canonical-wrapper and Main-route economic gates against the newest
  // finalized bank, so the packet sent below is the one just authorized.
  const preparation = await managerPreSendStage("authorization-refresh canonical preparation and simulation", () => prepareManagerOperation(input.strategyId, input.operation, input.amountRaw, input.artifactPath, {
    authorizationPath: input.authorizationPath ?? null,
    minimumContextSlot: managerAuthorizationContextSlot,
    confirmRouteAuthorizationSha256: input.confirmRouteAuthorizationSha256,
    lifecycleId: input.lifecycleId ?? undefined,
  }));
  if (preparation.authorization.fileSha256 !== approvedAuthorization.fileSha256
    || preparation.authorization.authorization.authorizationSha256 !== approvedAuthorization.authorization.authorizationSha256
    || preparation.loaded.fileSha256 !== input.confirmArtifactSha256
    || preparation.wrapper.dataSha256 !== input.confirmWrapperDataSha256) {
    throw new Error("manager refreshed canonical preparation escaped the approved authorization or reviewed hashes");
  }
  if (preparation.routeAuthorization.sha256 !== input.confirmRouteAuthorizationSha256
    || preparation.intent.routeAuthorizationSha256 !== input.confirmRouteAuthorizationSha256
    || preparation.intent.lifecycleId !== input.lifecycleId
    || preparation.intent.protectedPrestateSha256 !== preparation.protectedBefore.stateSha256) {
    throw new Error("manager refreshed preparation escaped the explicitly confirmed lifecycle/route authorization/protected prestate");
  }
  if (preparation.prepared.prestateSlot < refreshed.contextSlot
    || !preparation.report.readyForBroadcast
    || preparation.report.failedGateCount !== 0) {
    throw new Error(`manager operation re-preflight failed after authorization refresh: ${preparation.report.verdict}`);
  }
  // Close the last TOCTOU window: route-owned and policy-owned accounts must
  // still be the exact state used by the packet's simulation immediately
  // before send. Shared Kamino/Scope/Farms accounts are deliberately exempted
  // from byte equality because they churn during normal market activity; their
  // current graph and protocol semantics are revalidated below instead.
  const finalAuthorizationState = await managerPreSendStage("final route-owned state refresh", () => confirmedSnapshots(
    rpcUrl(),
    preparation.before.addresses,
    preparation.prepared.simulationSlot,
  ), true);
  const finalChanged = changedManagerState(
    route,
    preparation.reserve.graph,
    preparation.before.addresses,
    preparation.before.accounts,
    finalAuthorizationState.accounts,
  );
  if (finalChanged.length > 0 || finalAuthorizationState.contextSlot < preparation.prepared.prestateSlot) {
    throw new Error(`manager route-owned state changed after final simulation; refusing send (${finalChanged.map(({ address: value, class: stateClass }) => `${stateClass}:${value}`).join(", ") || `context-slot:${finalAuthorizationState.contextSlot}<${preparation.prepared.prestateSlot}`})`);
  }
  const finalSettingsIndex = preparation.before.addresses.indexOf(route.squads.settings);
  const finalSettingsSeed = await loadSettingsSeed(finalAuthorizationState.accounts[finalSettingsIndex] ?? null);
  const finalNonCatalogIsolation = await managerPreSendStage(
    "final non-catalog Squads policy isolation refresh",
    () => verifyNonCatalogSquadsPoliciesIsolated(
      rpcUrl(),
      BigInt(preparation.loaded.artifact.policies[0]?.seed ?? "0"),
      BigInt(preparation.authorization.authorization.terminalPolicySeed),
      finalAuthorizationState.contextSlot,
      "confirmed",
    ),
    true,
  );
  if (finalNonCatalogIsolation.failedGateCount !== 0 || finalNonCatalogIsolation.currentSeed !== finalSettingsSeed) {
    throw new Error("non-catalog Squads policy isolation changed after final simulation; refusing send");
  }
  const finalGraph = await managerPreSendStage("final shared Kamino graph refresh", () => loadMainReserveGraph(rpcUrl(), route, preparation.accounts.strategyAuth, "confirmed"), true);
  if (finalGraph.contextSlot < preparation.reserve.contextSlot
    || !reserveGraphSemanticsEqual(preparation.reserve.graph, finalGraph.graph)) {
    throw new Error(`manager shared Kamino graph changed after final simulation; refusing send (before=${JSON.stringify(preparation.reserve.graph)} after=${JSON.stringify(finalGraph.graph)})`);
  }
  const finalAuthorizationDeployments = await managerPreSendStage("final deployment identity refresh", () => loadDeploymentIdentities(
    rpcUrl(),
    route,
    Math.max(finalAuthorizationState.contextSlot, finalNonCatalogIsolation.contextSlot),
    "confirmed",
  ), true);
  if (!verifyDeploymentIdentities(route, finalAuthorizationDeployments.identities).every(({ pass }) => pass)
    || !sameJson(preparation.deploymentBefore.identities, finalAuthorizationDeployments.identities)) {
    throw new Error("manager packet deployment identity changed after final simulation; refusing send");
  }
  const finalProtectedPrestate = await managerPreSendStage(
    "final common protected-state refresh",
    () => loadFourMarketProtectedState(rpcUrl(), preparation.prepared.simulationSlot),
    true,
  );
  if (finalProtectedPrestate.addressSetSha256 !== preparation.protectedBefore.addressSetSha256
    || finalProtectedPrestate.stateSha256 !== preparation.protectedBefore.stateSha256) {
    throw new Error("manager common protected state changed after final simulation; refusing send");
  }
  const preSendAttestation = await createProtectedPreSendAttestation(preparation.guardian.signer, {
    lifecycleId: preparation.intent.lifecycleId,
    operation: preparation.intent.operation,
    expectedSignature: preparation.prepared.expectedSignature,
    messageSha256: sha256(preparation.prepared.serializedMessage),
    intentSha256: preparation.intentSha256,
    addressSetSha256: finalProtectedPrestate.addressSetSha256,
    preContextSlot: finalProtectedPrestate.contextSlot,
    preStateSha256: finalProtectedPrestate.stateSha256,
  });
  const sendAuthorizationContextSlot = Math.max(
    managerAuthorizationContextSlot,
    preparation.prepared.prestateSlot,
    preparation.prepared.simulationSlot,
    finalAuthorizationState.contextSlot,
    finalNonCatalogIsolation.contextSlot,
    finalAuthorizationDeployments.contextSlot,
    finalProtectedPrestate.contextSlot,
  );
  assertIntentNotExpired(preparation.intent);
  const persistedIntent = persistManagerIntent(
    input.intentPath,
    {
      strategyId: input.strategyId,
      operation: input.operation,
      intent: preparation.intent,
      intentSha256: preparation.intentSha256,
      prepared: preparation.prepared,
      loaded: preparation.loaded,
      authorization: preparation.authorization,
      protectedPreSend: finalProtectedPrestate,
      preSendAttestation,
    },
    sendAuthorizationContextSlot,
  );
  verifyPersistedManagerIntent(persistedIntent, { ...preparation, protectedPreSend: finalProtectedPrestate, preSendAttestation });
  let restorationPhaseA: RestorationBridgePhaseAResult | null = null;
  let restorationPhaseB: RestorationBridgePhaseBResult | null = null;
  let restorationRequiredIdleRaw: bigint | null = null;
  if (restorationBridge) {
    if (restorationBridge.protectedAddressSetSha256 !== finalProtectedPrestate.addressSetSha256
      || restorationBridge.protectedPrestateSha256 !== finalProtectedPrestate.stateSha256
      || restorationBridge.protectedContextSlot > finalProtectedPrestate.contextSlot) {
      throw new Error("restoration bridge checkpoint is not the exact unchanged request poststate authorized by the manager simulation");
    }
    const idleBefore = tokenAmount(accountMap(preparation.before).get(preparation.accounts.idleAta) ?? null);
    if (idleBefore === null) throw new Error("restoration bridge cannot derive the exact pre-send idle balance");
    restorationRequiredIdleRaw = idleBefore + input.amountRaw;
    const managerIntentId = restorationManagerIntentId(restorationBridge.originId, restorationBridge.generation, restorationBridge.legId);
    const writableAccountKeys = unique(preparation.wrapper.expectedAccounts
      .filter(({ address: value, writable }) => writable || value === route.squads.guardian)
      .map(({ address: value }) => value));
    restorationPhaseA = prepareRestorationBridge({
      schemaVersion: 1,
      phase: "prepare",
      cluster: "mainnet-beta",
      routeId: PARTNER_FOUR_MARKET_ROUTE.id,
      routeSpecSha256: fourMarketRouteSpecSha256(),
      vault: PARTNER_ROUTE.vault,
      owner: restorationBridge.owner,
      leaseSeconds: restorationBridge.leaseSeconds,
      originId: restorationBridge.originId,
      generation: restorationBridge.generation,
      legId: restorationBridge.legId,
      signedIntent: {
        managerIntentId,
        lifecycleId: preparation.intent.lifecycleId,
        strategyId: input.strategyId,
        reserve: route.strategy.reserve,
        amountRaw: Number(input.amountRaw),
        routeAuthorizationSha256: preparation.intent.routeAuthorizationSha256,
        protectedPrestateSha256: restorationBridge.protectedPrestateSha256,
        protectedAddressSetSha256: restorationBridge.protectedAddressSetSha256,
        protectedContextSlot: restorationBridge.protectedContextSlot,
        signedTransactionHex: Buffer.from(preparation.prepared.serializedTransaction).toString("hex"),
        signedTransactionSha256: sha256(preparation.prepared.serializedTransaction),
        messageSha256: sha256(preparation.prepared.serializedMessage),
        expectedSignature: preparation.prepared.expectedSignature,
        recentBlockhash: preparation.prepared.latestBlockhash.blockhash,
        lastValidBlockHeight: preparation.prepared.latestBlockhash.lastValidBlockHeight,
        feePayer: route.squads.guardian,
        compiledFeeLamports: preparation.prepared.feeLamports,
        writableAccountKeys,
        logicalConflictKeys: [
          `kamino:reserve:${route.strategy.reserve}`,
          `voltr:vault:${PARTNER_ROUTE.vault}`,
        ],
      },
    }, {
      evidenceDirectory: restorationBridge.evidenceDirectory,
      ...(restorationBridge.binaryPath ? { binaryPath: restorationBridge.binaryPath } : {}),
    });
  }
  let finalized: Awaited<ReturnType<typeof sendPreparedConfirmedOnce>> | null = null;
  try {
    finalized = await sendPreparedConfirmedOnce(rpcUrl(), preparation.prepared, sendAuthorizationContextSlot);
    if (finalized.err !== null) return { verdict: "PARTNER_MANAGER_OPERATION_FINALIZED_WITH_ERROR", broadcast: true, intentPath: persistedIntent.path, intentFileSha256: persistedIntent.fileSha256, authorizationContextSlot: sendAuthorizationContextSlot, protectedSnapshotEvidence: { before: finalProtectedPrestate }, preSendAttestation, preflight: preparation.report, finalized } as const;
    const stateSnapshot = await confirmedSnapshots(rpcUrl(), preparation.before.addresses, finalized.confirmedSlot);
    const state: SnapshotSet = { ...stateSnapshot, addresses: preparation.before.addresses };
    const protectedAfter = await loadFourMarketProtectedState(rpcUrl(), finalized.confirmedSlot);
    const protectedEvidence = protectedSnapshotEvidenceEnvelope(finalProtectedPrestate, protectedAfter);
    const protectedState = protectedStateEnvelope(finalProtectedPrestate, protectedAfter);
    const settlementAttestation = await createProtectedSettlementAttestation(preparation.guardian.signer, {
      lifecycleId: preparation.intent.lifecycleId,
      operation: preparation.intent.operation,
      expectedSignature: preparation.prepared.expectedSignature,
      confirmedSignature: finalized.signature,
      messageSha256: sha256(preparation.prepared.serializedMessage),
      serializedTransactionSha256: sha256(preparation.prepared.serializedTransaction),
      intentSha256: preparation.intentSha256,
      addressSetSha256: finalProtectedPrestate.addressSetSha256,
      preAttestation: preSendAttestation,
      confirmedSlot: finalized.confirmedSlot,
      postContextSlot: protectedAfter.contextSlot,
      postStateSha256: protectedAfter.stateSha256,
    });
    verifyPersistedManagerIntent(persistedIntent, { ...preparation, protectedPreSend: finalProtectedPrestate, preSendAttestation });
    const beforeMap = accountMap(preparation.before);
    const afterMap = accountMap(state);
    const idleBefore = tokenAmount(beforeMap.get(preparation.accounts.idleAta) ?? null);
    const idleAfter = tokenAmount(afterMap.get(preparation.accounts.idleAta) ?? null);
    const positionBefore = strategyPosition(beforeMap.get(preparation.accounts.strategyInitReceipt) ?? null);
    const positionAfter = strategyPosition(afterMap.get(preparation.accounts.strategyInitReceipt) ?? null);
    const idleMeta = finalizedTokenDelta(finalized, preparation.accounts.idleAta);
    const reserveLiquidity = finalizedTokenDelta(finalized, preparation.reserve.graph.reserveLiquiditySupply);
    const reserveCollateral = finalizedTokenDelta(finalized, preparation.reserve.graph.reserveCollateralSupplyVault);
    const strategyAsset = finalizedTokenDelta(finalized, preparation.strategyAssetAta);
    const guardianLamportDelta = finalizedLamportDelta(finalized, route.squads.guardian);
    const obligationBefore = beforeMap.get(preparation.reserve.graph.obligation) ?? null;
    const obligationAfter = afterMap.get(preparation.reserve.graph.obligation) ?? null;
    const obligationRentLamports = input.operation === "deposit" || (input.operation === "withdraw" && obligationBefore !== null)
      ? await rentExemptionLamports(rpcUrl(), KAMINO_OBLIGATION_DATA_LENGTH)
      : null;
    const obligationLamportDelta = finalizedLamportDelta(finalized, preparation.reserve.graph.obligation);
    const strategyAuthLamportDelta = finalizedLamportDelta(finalized, preparation.accounts.strategyAuth);
    const obligationInitializationCandidate = input.operation === "deposit" && obligationBefore === null && obligationAfter !== null;
    const obligationClosureCandidate = input.operation === "withdraw" && obligationBefore !== null && obligationAfter === null;
    let decodedObligationBefore: { owner: string; lendingMarket: string } | null = null;
    let decodedObligationAfter: { owner: string; lendingMarket: string } | null = null;
    if (obligationBefore !== null) {
      try {
        const decoded = Obligation.decode(Buffer.from(obligationBefore.data));
        decodedObligationBefore = { owner: decoded.owner.toString(), lendingMarket: decoded.lendingMarket.toString() };
      } catch {
        decodedObligationBefore = null;
      }
    }
    if (obligationAfter !== null) {
      try {
        const decoded = Obligation.decode(Buffer.from(obligationAfter.data));
        decodedObligationAfter = { owner: decoded.owner.toString(), lendingMarket: decoded.lendingMarket.toString() };
      } catch {
        decodedObligationAfter = null;
      }
    }
    const obligationInitializationExact = !obligationInitializationCandidate || (
      obligationBefore === null
      && obligationAfter !== null
      && obligationAfter.address === preparation.reserve.graph.obligation
      && obligationAfter.owner === route.programs.klend
      && obligationAfter.data.length === KAMINO_OBLIGATION_DATA_LENGTH
      && obligationRentLamports !== null
      && obligationAfter.lamports === obligationRentLamports
      && strategyAuthLamportDelta === -BigInt(obligationRentLamports)
      && obligationLamportDelta === BigInt(obligationRentLamports)
      && decodedObligationAfter?.owner === preparation.accounts.strategyAuth
      && decodedObligationAfter?.lendingMarket === route.strategy.lendingMarket
    );
    const obligationClosureExact = !obligationClosureCandidate || (
      obligationBefore !== null
      && obligationBefore.address === preparation.reserve.graph.obligation
      && obligationBefore.owner === route.programs.klend
      && obligationBefore.data.length === KAMINO_OBLIGATION_DATA_LENGTH
      && obligationRentLamports !== null
      && obligationBefore.lamports === obligationRentLamports
      && obligationAfter === null
      && strategyAuthLamportDelta === BigInt(obligationRentLamports)
      && obligationLamportDelta === -BigInt(obligationRentLamports)
      && decodedObligationBefore?.owner === preparation.accounts.strategyAuth
      && decodedObligationBefore?.lendingMarket === route.strategy.lendingMarket
    );
    const unexpectedDeltas = unexpectedManagerDeltas(
      finalized,
      route,
      preparation.accounts,
      preparation.reserve.graph,
      preparation.strategyAssetAta,
    );
    const unexpectedLamportDeltas = unexpectedManagerLamportDeltas(
      finalized,
      route,
      preparation.accounts,
      preparation.reserve.graph.obligation,
      obligationInitializationCandidate || obligationClosureCandidate,
    );
    const finalizedStrategyEvent = strategyEventPayload(finalized.logs, input.operation);
    const finalizedEventAmount = bigintEventField(finalizedStrategyEvent, input.operation === "deposit" ? "vaultAmountAssetDeposited" : "vaultAmountAssetWithdrawn");
    const finalizedEventIdleBefore = bigintEventField(finalizedStrategyEvent, "vaultAssetIdleAtaAmountBefore");
    const finalizedEventIdleAfter = bigintEventField(finalizedStrategyEvent, "vaultAssetIdleAtaAmountAfter");
    const finalizedEventPositionBefore = bigintEventField(finalizedStrategyEvent, "strategyPositionValueBefore");
    const finalizedEventPositionAfter = bigintEventField(finalizedStrategyEvent, "strategyPositionValueAfter");
    const finalizedEventTotalBefore = bigintEventField(finalizedStrategyEvent, "vaultAssetTotalValueBefore");
    const finalizedEventTotalAfter = bigintEventField(finalizedStrategyEvent, "vaultAssetTotalValueAfter");
    const finalizedIdleEffect = finalizedEventIdleBefore === null || finalizedEventIdleAfter === null
      ? null
      : input.operation === "deposit" ? finalizedEventIdleBefore - finalizedEventIdleAfter : finalizedEventIdleAfter - finalizedEventIdleBefore;
    const finalizedPositionEffect = finalizedEventPositionBefore === null || finalizedEventPositionAfter === null
      ? null
      : input.operation === "deposit" ? finalizedEventPositionAfter - finalizedEventPositionBefore : finalizedEventPositionBefore - finalizedEventPositionAfter;
    const finalizedTotalValueEffect = finalizedEventTotalBefore === null || finalizedEventTotalAfter === null ? null : finalizedEventTotalAfter - finalizedEventTotalBefore;
    const gates: Gate[] = [];
    add(gates, "confirmed context is at or after transaction", state.contextSlot >= finalized.confirmedSlot, state.contextSlot, `>=${finalized.confirmedSlot}`);
    add(gates, "confirmed idle USDC transaction-meta delta exact", idleMeta.delta !== null && (input.operation === "deposit" ? idleMeta.delta === -input.amountRaw : idleMeta.delta > 0n && idleMeta.delta >= input.amountRaw - 1n) && idleMeta.mint === route.asset.mint, idleMeta, input.operation === "deposit" ? { delta: -input.amountRaw, mint: route.asset.mint } : { delta: `>=${input.amountRaw - 1n}; accrued yield allowed`, mint: route.asset.mint });
    add(gates, "confirmed exact Voltr strategy event conservation", finalizedStrategyEvent !== null
      && finalizedStrategyEvent.manager === route.squads.manager
      && finalizedStrategyEvent.vault === route.vault
      && finalizedStrategyEvent.strategy === preparation.reserve.graph.reserve
      && finalizedStrategyEvent.strategyInitReceipt === preparation.accounts.strategyInitReceipt
      && finalizedStrategyEvent.adaptorProgram === route.programs.kaminoAdaptor
      && finalizedStrategyEvent.vaultAssetMint === route.asset.mint
      && finalizedEventAmount !== null
      && (input.operation === "deposit" ? finalizedEventAmount === input.amountRaw : finalizedEventAmount >= input.amountRaw - 1n && finalizedEventAmount <= input.amountRaw)
      && finalizedIdleEffect !== null
      && idleMeta.delta === (input.operation === "deposit" ? -finalizedIdleEffect : finalizedIdleEffect)
      && finalizedPositionEffect !== null
      && finalizedPositionEffect >= 0n
      && strategyAsset.delta !== null
      && finalizedTotalValueEffect === (input.operation === "deposit" ? finalizedPositionEffect + strategyAsset.delta - finalizedIdleEffect : finalizedIdleEffect - finalizedPositionEffect + strategyAsset.delta)
      && finalizedStrategyEvent.vaultLpSupplyInclFeesBefore === finalizedStrategyEvent.vaultLpSupplyInclFeesAfter,
    { event: finalizedStrategyEvent, idleMeta, idleEffect: finalizedIdleEffect, positionEffect: finalizedPositionEffect, transientDelta: strategyAsset.delta, totalValueEffect: finalizedTotalValueEffect },
    { requestedAmountRaw: input.amountRaw, idleMeta: "exact event effect", positionEffect: ">=0", transientDelta: "exact token-meta delta", totalValueEffect: input.operation === "deposit" ? "positionEffect+transientDelta-idleEffect" : "idleEffect-positionEffect+transientDelta", lpSupply: "unchanged" });
    add(gates, "finalized strategy receipt snapshot advisory", true, { before: positionBefore, after: positionAfter, readbackContextSlot: state.contextSlot, transactionSlot: finalized.confirmedSlot }, "informational only; receipt position is not used as exact transaction accounting");
    add(gates, "finalized token delta closure exact", unexpectedDeltas.token.length === 0, unexpectedDeltas.token, "only idle/strategy/reserve USDC and reserve collateral token accounts");
    add(gates, "confirmed first-deposit obligation initialization is exact", obligationInitializationExact, { candidate: obligationInitializationCandidate, obligationBefore: obligationBefore?.address ?? null, obligationAfter: obligationAfter ? { address: obligationAfter.address, owner: obligationAfter.owner, dataLength: obligationAfter.data.length, lamports: obligationAfter.lamports } : null, obligationRentLamports, strategyAuthLamportDelta, obligationLamportDelta, decodedObligation: decodedObligationAfter }, "absent prestate plus exact KLend obligation PDA/owner/size/rent and strategyAuth -> obligation rent transfer; subsequent deposits allow none");
    add(gates, "confirmed terminal obligation closure/refund is exact", obligationClosureExact, { candidate: obligationClosureCandidate, obligationBefore: obligationBefore ? { address: obligationBefore.address, owner: obligationBefore.owner, dataLength: obligationBefore.data.length, lamports: obligationBefore.lamports } : null, obligationAfter: obligationAfter?.address ?? null, obligationRentLamports, strategyAuthLamportDelta, obligationLamportDelta, decodedObligation: decodedObligationBefore }, "full withdraw may close only the exact route KLend obligation and refund its exact rent to strategyAuth; partial withdraws allow no obligation lamports");
    add(gates, "finalized lamport delta closure exact", unexpectedLamportDeltas.length === 0, unexpectedLamportDeltas, obligationInitializationCandidate ? "guardian fee plus the exact first-obligation rent pair only" : obligationClosureCandidate ? "guardian fee plus the exact terminal-obligation refund pair only" : "only guardian transaction fee may have a non-zero lamport delta");
    const expectedReserveLiquidityDelta = input.operation === "deposit"
      ? strategyAsset.delta !== null && strategyAsset.delta >= 0n && strategyAsset.delta <= 1n ? input.amountRaw - strategyAsset.delta : null
      : idleMeta.delta === null || strategyAsset.delta === null ? null : -idleMeta.delta - strategyAsset.delta;
    add(gates, `confirmed ${input.strategyId} reserve liquidity delta exact`, expectedReserveLiquidityDelta !== null && reserveLiquidity.delta === expectedReserveLiquidityDelta && reserveLiquidity.mint === route.asset.mint, reserveLiquidity, { delta: expectedReserveLiquidityDelta, mint: route.asset.mint });
    add(gates, `confirmed ${input.strategyId} collateral supply direction exact`, reserveCollateral.delta !== null && (input.operation === "deposit" ? reserveCollateral.delta > 0n : reserveCollateral.delta < 0n) && reserveCollateral.mint === preparation.reserve.graph.reserveCollateralMint, reserveCollateral, input.operation === "deposit" ? ">0 collateral units" : "<0 collateral units");
    add(gates, input.operation === "deposit" ? "confirmed deposit reserve and transient strategy USDC exactly conserve requested amount" : "confirmed withdrawal reserve and transient release exactly conserve idle increase", input.operation === "deposit"
      ? strategyAsset.delta !== null && strategyAsset.delta >= 0n && strategyAsset.delta <= 1n && reserveLiquidity.delta !== null && reserveLiquidity.delta + strategyAsset.delta === input.amountRaw && (strategyAsset.mint === null || strategyAsset.mint === route.asset.mint)
      : strategyAsset.delta !== null && strategyAsset.delta >= -1n && strategyAsset.delta <= 0n && reserveLiquidity.delta !== null && idleMeta.delta !== null && reserveLiquidity.delta + strategyAsset.delta === -idleMeta.delta && (strategyAsset.mint === null || strategyAsset.mint === route.asset.mint), { strategyAsset, reserveLiquidity, idleMeta }, input.operation === "deposit" ? { transientDelta: "0..1", reservePlusTransient: input.amountRaw, mint: route.asset.mint } : { transientDelta: "-1..0", reservePlusTransient: "-idle increase", mint: route.asset.mint });
    add(gates, "finalized guardian pays exactly the bounded transaction fee", finalized.feeLamports !== null && finalized.feeLamports <= 100_000 && guardianLamportDelta === -BigInt(finalized.feeLamports), { guardianLamportDelta, feeLamports: finalized.feeLamports }, { guardianLamportDelta: finalized.feeLamports === null ? null : -BigInt(finalized.feeLamports), maximumFeeLamports: 100_000 });
    const deploymentsFinal = await loadDeploymentIdentities(rpcUrl(), route, state.contextSlot, "confirmed");
    gates.push(...await deploymentGates(preparation.deploymentBefore, deploymentsFinal));
    let restorationRemainingShortfallRaw: bigint | null = null;
    if (restorationBridge && restorationPhaseA && restorationRequiredIdleRaw !== null) {
      restorationRemainingShortfallRaw = idleAfter === null || restorationRequiredIdleRaw <= idleAfter
        ? 0n
        : restorationRequiredIdleRaw - idleAfter;
      const managerReadbackExact = idleAfter !== null
        && gates.every(({ pass }) => pass)
        && restorationRemainingShortfallRaw === 0n;
      if (managerReadbackExact) {
        const readbackFingerprint = sha256(Buffer.from(JSON.stringify({
          signature: finalized.signature,
          confirmedSlot: finalized.confirmedSlot,
          readbackContextSlot: state.contextSlot,
          idleRawAfter: idleAfter.toString(),
          remainingShortfallRaw: restorationRemainingShortfallRaw.toString(),
          protectedPoststateSha256: protectedAfter.stateSha256,
        }), "utf8"));
        try {
          restorationPhaseB = confirmRestorationBridge(restorationPhaseA.token, {
            managerIntentId: restorationPhaseA.token.managerIntentId,
            lifecycleId: preparation.intent.lifecycleId,
            strategyId: input.strategyId,
            reserve: route.strategy.reserve,
            amountRaw: Number(input.amountRaw),
            routeAuthorizationSha256: preparation.intent.routeAuthorizationSha256,
            signedTransactionSha256: sha256(preparation.prepared.serializedTransaction),
            messageSha256: sha256(preparation.prepared.serializedMessage),
            expectedSignature: finalized.signature,
            confirmedSlot: finalized.confirmedSlot,
            readbackContextSlot: state.contextSlot,
            commitment: "confirmed",
            managerTransactionSignature: finalized.signature,
            idleRawAfter: Number(idleAfter),
            remainingShortfallRaw: Number(restorationRemainingShortfallRaw),
            readbackFingerprint,
          }, {
            evidenceDirectory: restorationBridge.evidenceDirectory,
            ...(restorationBridge.binaryPath ? { binaryPath: restorationBridge.binaryPath } : {}),
          });
          add(gates, "durable restoration fence acknowledged after exact confirmed readback", restorationPhaseB.completion.acknowledged === true, restorationPhaseB, "exact Phase-B acknowledgement of the Phase-A token");
        } catch (error) {
          add(gates, "durable restoration fence acknowledged after exact confirmed readback", false, error instanceof Error ? error.message : String(error), "exact Phase-B acknowledgement of the Phase-A token");
        }
      } else {
        add(gates, "durable restoration fence acknowledged after exact confirmed readback", false, { idleAfter, restorationRequiredIdleRaw, restorationRemainingShortfallRaw, managerReadbackGatesPass: gates.every(({ pass }) => pass) }, { idleAfter: `>=${restorationRequiredIdleRaw}`, remainingShortfallRaw: 0n, managerReadbackGatesPass: true });
      }
    }
    const failedGateCount = gates.filter(({ pass }) => !pass).length;
    return {
      verdict: failedGateCount === 0 ? "PARTNER_MANAGER_OPERATION_FINALIZED_AND_VERIFIED" : "PARTNER_MANAGER_OPERATION_FINALIZED_READBACK_FAIL",
      broadcast: true,
      intent: preparation.intent,
      intentSha256: preparation.intentSha256,
      intentPath: persistedIntent.path,
      intentFileSha256: persistedIntent.fileSha256,
      lifecycleId: preparation.intent.lifecycleId,
      routeAuthorizationSha256: preparation.intent.routeAuthorizationSha256,
      protectedState,
      protectedSnapshotEvidence: protectedEvidence,
      preSendAttestation,
      settlementAttestation,
      senderProof: {
        schemaVersion: 2,
        signerRole: "guardian",
        signer: route.squads.guardian,
        senderSourceSha256: sha256(readFileSync(resolve(REPOSITORY_ROOT, "tools/backyard-voltr/src/runtime/manager.ts"))),
        persistedBeforeSend: true,
        sendAttemptCount: 1,
        submissionAttemptCount: finalized.submissionAttemptCount,
        submissionWireSha256: finalized.submissionWireSha256,
        submissionAttempts: finalized.submissionAttempts,
        maxRetries: 0,
        recoveryByExpectedSignatureOnly: true,
        expectedSignature: finalized.signature,
        serializedTransactionSha256: sha256(preparation.prepared.serializedTransaction),
        serializedMessageSha256: sha256(preparation.prepared.serializedMessage),
        oneSendOnly: true,
        confirmedSlot: finalized.confirmedSlot,
      },
      persistenceContract: persistedIntent.persistenceContract,
      restorationBridge: restorationPhaseA ? {
        phaseA: restorationPhaseA,
        phaseB: restorationPhaseB,
        requiredIdleRaw: restorationRequiredIdleRaw,
        remainingShortfallRaw: restorationRemainingShortfallRaw,
      } : null,
      authorizationContextSlot: sendAuthorizationContextSlot,
      preflight: preparation.report,
      finalized,
      readbackContextSlot: state.contextSlot,
      readback: { failedGateCount, gates, idleBefore, idleAfter, idleMeta, strategyPositionBefore: positionBefore, strategyPositionAfter: positionAfter, reserveLiquidity, reserveCollateral, strategyAsset, guardianLamportDelta, obligationInitialization: { candidate: obligationInitializationCandidate, obligationRentLamports, strategyAuthLamportDelta, obligationLamportDelta, obligationBefore: obligationBefore?.address ?? null, obligationAfter: obligationAfter ? { address: obligationAfter.address, owner: obligationAfter.owner, dataLength: obligationAfter.data.length, lamports: obligationAfter.lamports } : null, decodedObligation: decodedObligationAfter }, obligationClosure: { candidate: obligationClosureCandidate, exact: obligationClosureExact, obligationRentLamports, strategyAuthLamportDelta, obligationLamportDelta, obligationBefore: obligationBefore ? { address: obligationBefore.address, owner: obligationBefore.owner, dataLength: obligationBefore.data.length, lamports: obligationBefore.lamports } : null, obligationAfter: obligationAfter?.address ?? null, decodedObligation: decodedObligationBefore }, unexpectedDeltas: { ...unexpectedDeltas, lamport: unexpectedLamportDeltas }, tokenDeltas: finalized.tokenDeltas, lamportDeltas: finalized.lamportDeltas },
    } as const;
  } catch (error) {
    const failedSubmission = submissionEvidence(error, preparation.prepared);
    if (finalized) {
      return {
        verdict: "PARTNER_MANAGER_OPERATION_FINALIZED_READBACK_ERROR",
        broadcast: true,
        intent: preparation.intent,
        intentSha256: preparation.intentSha256,
        intentPath: persistedIntent.path,
        intentFileSha256: persistedIntent.fileSha256,
        protectedSnapshotEvidence: { before: finalProtectedPrestate },
        preSendAttestation,
        authorizationContextSlot: sendAuthorizationContextSlot,
        preflight: preparation.report,
        finalized,
        senderProof: {
          schemaVersion: 2,
          signerRole: "guardian",
          signer: route.squads.guardian,
          senderSourceSha256: sha256(readFileSync(resolve(REPOSITORY_ROOT, "tools/backyard-voltr/src/runtime/manager.ts"))),
          persistedBeforeSend: true,
          sendAttemptCount: 1,
          submissionAttemptCount: finalized.submissionAttemptCount,
          submissionWireSha256: finalized.submissionWireSha256,
          submissionAttempts: finalized.submissionAttempts,
          maxRetries: 0,
          recoveryByExpectedSignatureOnly: true,
          expectedSignature: finalized.signature,
          serializedTransactionSha256: sha256(preparation.prepared.serializedTransaction),
          serializedMessageSha256: sha256(preparation.prepared.serializedMessage),
          oneSendOnly: true,
          confirmedSlot: finalized.confirmedSlot,
        },
        restorationBridge: restorationPhaseA ? { phaseA: restorationPhaseA, phaseB: restorationPhaseB } : null,
        error: error instanceof Error ? error.message : String(error),
        recoveryInstruction: "Do not resend. The manager transaction is finalized; rerun read-only manager/strategy reconciliation.",
      } as const;
    }
    return {
      verdict: "PARTNER_MANAGER_OPERATION_BROADCAST_STATUS_UNKNOWN",
      broadcast: null,
      expectedSignature: preparation.prepared.expectedSignature,
      intentPath: persistedIntent.path,
      intentFileSha256: persistedIntent.fileSha256,
      intent: preparation.intent,
      intentSha256: preparation.intentSha256,
      authorizationContextSlot: sendAuthorizationContextSlot,
      preflight: preparation.report,
      senderProof: {
        schemaVersion: 2,
        signerRole: "guardian",
        signer: route.squads.guardian,
        senderSourceSha256: sha256(readFileSync(resolve(REPOSITORY_ROOT, "tools/backyard-voltr/src/runtime/manager.ts"))),
        persistedBeforeSend: true,
        sendAttemptCount: 1,
        submissionAttemptCount: failedSubmission.submissionAttemptCount,
        submissionWireSha256: failedSubmission.submissionWireSha256,
        submissionAttempts: failedSubmission.submissionAttempts,
        maxRetries: 0,
        recoveryByExpectedSignatureOnly: true,
        expectedSignature: preparation.prepared.expectedSignature,
        serializedTransactionSha256: sha256(preparation.prepared.serializedTransaction),
        serializedMessageSha256: sha256(preparation.prepared.serializedMessage),
        oneSendOnly: true,
        confirmedSlot: 0,
      },
      restorationBridge: restorationPhaseA ? { phaseA: restorationPhaseA, phaseB: restorationPhaseB } : null,
      error: error instanceof Error ? error.message : String(error),
      recoveryInstruction: "Do not resend. Verify this exact signature and finalized manager/idle/strategy state.",
    } as const;
  }
}
