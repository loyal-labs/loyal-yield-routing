import { createHash } from "node:crypto";
import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, relative, resolve } from "node:path";

import {
  getCreateAssociatedTokenIdempotentInstructionAsync,
  getMintDecoder,
  getTokenDecoder,
} from "@solana-program/token";
import { address, createNoopSigner, type Address, type Instruction } from "@solana/kit";
import {
  calculateAssetsForWithdrawAmount,
  calculateLpForDepositAmount,
  findRequestWithdrawVaultReceiptPda,
  getVaultDecoder,
  parseTransactionEvents,
} from "@voltr/vault-sdk";
import bs58 from "bs58";
import {
  PublicKey,
  TransactionMessage,
  VersionedTransaction,
} from "@solana/web3.js";

import {
  assertIntentForRouteBinding,
  intentSha256,
  type UserRuntimeIntent,
} from "../domain/execution-intent.js";
import {
  PARTNER_FOUR_MARKET_ROUTE,
  PARTNER_ROUTE,
  fourMarketRouteSpecSha256,
} from "../domain/route-spec.js";
import {
  confirmedBlockTime,
  confirmedTransaction,
  finalizedTransaction,
  confirmedSnapshots,
  loadDeploymentIdentities,
  loadMainReserveGraph,
  prepareSignedV0Transaction,
  rentExemptionLamports,
  sendPreparedConfirmedOnce,
  submissionEvidence,
  MAX_IDENTICAL_SUBMISSION_ATTEMPTS,
  toWeb3Instruction,
  type AccountSnapshot,
  type PreparedTransaction,
} from "../integrations/solana-compat.js";
import { signingMaterialFromEnvironment, type SigningMaterial } from "../integrations/signer.js";
import {
  createVoltrRouteBuilder,
  deriveVoltrAccounts,
  type UserVoltrAccounts,
  type VoltrInstruction,
} from "../integrations/voltr.js";
import { verifyDeploymentIdentities, verifyVaultCurrentState, type Gate } from "../verify/current.js";
import { decodeReceipt, RECEIPT_DATA_LENGTH } from "./receipt.js";
import {
  assertProtectedPreSendAttestation,
  createProtectedPreSendAttestation,
  createProtectedSettlementAttestation,
  fourMarketProtectedAddresses,
  loadFourMarketProtectedState,
  protectedSnapshotEvidenceEnvelope,
  protectedStateEnvelope,
  type ProtectedPreSendAttestation,
  type ProtectedSettlementAttestation,
  type ProtectedSnapshotEvidence,
} from "./protected-state.js";

// Keep the existing guardian manager surface stable while user operations
// remain in this file. The manager implementation has its own policy/artifact
// boundary and is intentionally not mixed into the user signer flow below.
export {
  executeManagerOperation,
  reconcileConfirmedManagerOperation,
  simulateManagerOperation,
  type ManagerOperation,
} from "./manager.js";

/** Recurring operations deliberately have no setup-only authority. */
export const RUNTIME_OPERATIONS = [
  "user-deposit",
  "manager-deposit",
  "manager-withdraw",
  "withdraw-request",
  "withdraw-claim",
] as const;

const U80F48_FRACTION_BITS = 48n;
const MAX_U64_RAW = (1n << 64n) - 1n;
const MAX_USER_DEPOSIT_TOTAL_LAMPORTS = 3_000_000;
const MAX_WITHDRAW_REQUEST_TOTAL_LAMPORTS = 5_000_000;
const REPOSITORY_ROOT = resolve(fileURLToPath(new URL("../../../..", import.meta.url)));
const RUNTIME_INTENT_ROOT = resolve(REPOSITORY_ROOT, "docs/evidence/backyard-voltr-four-market/intents");

type RuntimeContext = Readonly<{
  route: typeof PARTNER_ROUTE;
  user: SigningMaterial;
  accounts: Awaited<ReturnType<typeof deriveVoltrAccounts>>;
  userAccounts: UserVoltrAccounts;
  builder: Awaited<ReturnType<typeof createVoltrRouteBuilder>>;
}>;

type TokenState = Readonly<{
  mint: Address;
  owner: Address;
  amount: bigint;
}>;

type ProtectedState = Readonly<{
  schemaVersion: 1;
  addressSetSha256: string;
  beforeContextSlot: number;
  beforeSha256: string;
  afterContextSlot: number;
  afterSha256: string;
}>;

type UserLifecycleAuthorization = Readonly<{
  lifecycleId: string;
  protectedPrestateSha256: string;
  protectedAddressSetSha256: string;
}>;

type RequestOrigin = Readonly<{
  signature: string;
  eventIndex: number;
  receipt: string;
  rawAccountSha256: string;
  generationFingerprint: string;
}>;

function rpcUrl(): string {
  const value = process.env.SOLANA_RPC_URL;
  if (!value) throw new Error("SOLANA_RPC_URL is required");
  return value;
}

function requireRuntimeIntentPath(value: string | null, operation: string): string {
  if (!value) throw new Error(`execute ${operation} requires an explicit --intent-path`);
  const path = resolve(value);
  const relativePath = relative(RUNTIME_INTENT_ROOT, path);
  if (!relativePath || relativePath === ".." || relativePath.startsWith("../") || relativePath.startsWith("/")) {
    throw new Error(`execute ${operation} --intent-path must be inside docs/evidence/backyard-voltr-four-market/intents`);
  }
  return path;
}

function persistRuntimeIntent(path: string, document: Readonly<Record<string, unknown>>): string {
  const serialized = `${JSON.stringify(document, (_key, value) => typeof value === "bigint" ? value.toString() : value, 2)}\n`;
  try {
    mkdirSync(dirname(path), { recursive: true });
    writeFileSync(path, serialized, { encoding: "utf8", mode: 0o600, flag: "wx" });
  } catch (error) {
    throw new Error(`could not persist the exact signed runtime intent at ${path}`, { cause: error });
  }
  return sha256(Buffer.from(serialized, "utf8"));
}

function verifyPersistedRuntimeIntent(
  path: string,
  fileSha256: string,
  preparation: Readonly<{ intent: UserRuntimeIntent; intentSha256: string; prepared: PreparedTransaction; protectedPreSend: ProtectedSnapshotEvidence; preSendAttestation: ProtectedPreSendAttestation }>,
  expectedPersistenceContract: Readonly<Record<string, unknown>>,
  authorizationContextSlot: number,
): void {
  const bytes = readFileSync(path);
  if (sha256(bytes) !== fileSha256) throw new Error("persisted user runtime intent file hash changed before send");
  const value = JSON.parse(bytes.toString("utf8")) as Record<string, unknown>;
  const transactionBase64 = value.serializedTransactionBase64;
  if (typeof transactionBase64 !== "string") throw new Error("persisted user runtime intent has no signed wire");
  const wire = Buffer.from(transactionBase64, "base64");
  if (wire.toString("base64") !== transactionBase64) throw new Error("persisted user runtime signed wire is not canonical base64");
  const transaction = VersionedTransaction.deserialize(wire);
  const signature = transaction.signatures.length === 1 ? bs58.encode(transaction.signatures[0]!) : null;
  const messageSha256 = sha256(transaction.message.serialize());
  const protectedEvidence = value.protectedSnapshotEvidence && typeof value.protectedSnapshotEvidence === "object"
    ? (value.protectedSnapshotEvidence as { before?: ProtectedSnapshotEvidence }).before
    : undefined;
  const protectedPrestateEvidence = value.protectedPrestateEvidence;
  if (!protectedEvidence || !protectedPrestateEvidence || protectedEvidence.stateSha256 !== preparation.protectedPreSend.stateSha256 || protectedEvidence.addressSetSha256 !== preparation.protectedPreSend.addressSetSha256 || canonicalProtectedJson(protectedPrestateEvidence) !== canonicalProtectedJson(preparation.protectedPreSend)) {
    throw new Error("persisted user runtime intent is missing the exact protected pre-send snapshot evidence");
  }
  const preSendAttestation = value.preSendAttestation;
  if (!preSendAttestation || typeof preSendAttestation !== "object") throw new Error("persisted user runtime intent is missing the protected pre-send attestation");
  assertProtectedPreSendAttestation(preSendAttestation);
  if (preSendAttestation.attestationSha256 !== preparation.preSendAttestation.attestationSha256 || preSendAttestation.signatureSha256 !== preparation.preSendAttestation.signatureSha256) {
    throw new Error("persisted user runtime intent pre-send attestation changed before send/readback");
  }
  if (value.expectedSignature !== preparation.prepared.expectedSignature
    || signature !== preparation.prepared.expectedSignature
    || value.serializedTransactionSha256 !== sha256(wire)
    || value.serializedTransactionSha256 !== sha256(preparation.prepared.serializedTransaction)
    || value.serializedMessageSha256 !== messageSha256
    || messageSha256 !== sha256(preparation.prepared.serializedMessage)
    || value.authorizationContextSlot !== authorizationContextSlot
    || intentSha256(value.intent as UserRuntimeIntent) !== preparation.intentSha256
    || canonicalProtectedJson(value.intent) !== canonicalProtectedJson(preparation.intent)
    || canonicalProtectedJson(value.persistenceContract) !== canonicalProtectedJson(expectedPersistenceContract)) {
    throw new Error("persisted user runtime intent is not bound to the exact lifecycle/prestate/wire");
  }
}

function add(gates: Gate[], name: string, pass: boolean, observed: unknown, expected: unknown): void {
  gates.push({ name, pass, observed, expected });
}

function sha256(data: ArrayLike<number>): string {
  return createHash("sha256").update(Uint8Array.from(data)).digest("hex");
}

function canonicalProtectedJson(value: unknown): string {
  if (typeof value === "bigint") return JSON.stringify(value.toString());
  if (Array.isArray(value)) return `[${value.map(canonicalProtectedJson).join(",")}]`;
  if (value && typeof value === "object") {
    return `{${Object.entries(value)
      .sort(([left], [right]) => left.localeCompare(right))
      .map(([key, entry]) => `${JSON.stringify(key)}:${canonicalProtectedJson(entry)}`)
      .join(",")}}`;
  }
  return JSON.stringify(value);
}

function requestOriginFingerprint(origin: Omit<RequestOrigin, "generationFingerprint">): string {
  return sha256(Buffer.from(canonicalProtectedJson(origin), "utf8"));
}

function authorizedSha256(value: string | null, label: string): string {
  if (value === null || !/^[0-9a-f]{64}$/.test(value)) throw new Error(`${label} requires a lowercase SHA-256 digest`);
  return value;
}

function authorizedEventIndex(value: string | null, label: string): number {
  if (value === null || !/^\d+$/.test(value)) throw new Error(`${label} requires a non-negative event index`);
  const parsed = Number(value);
  if (!Number.isSafeInteger(parsed) || parsed < 0) throw new Error(`${label} requires a non-negative safe event index`);
  return parsed;
}

function simulationCustomErrorCode(error: unknown, logs: readonly string[]): number | null {
  if (error && typeof error === "object") {
    const instructionError = (error as { InstructionError?: unknown }).InstructionError;
    if (Array.isArray(instructionError)) {
      const detail = instructionError[1];
      if (detail && typeof detail === "object" && typeof (detail as { Custom?: unknown }).Custom === "number") {
        return (detail as { Custom: number }).Custom;
      }
    }
  }
  const serialized = (() => {
    try { return JSON.stringify(error); } catch { return ""; }
  })();
  const decimal = serialized.match(/(?:Custom|custom)\s*[:=]\s*(\d+)/)?.[1];
  if (decimal !== undefined) return Number(decimal);
  const hexadecimal = logs.join("\n").match(/custom program error:\s*0x([0-9a-f]+)/i)?.[1];
  return hexadecimal === undefined ? null : Number.parseInt(hexadecimal, 16);
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
    throw new Error(`prepared user wire failed exact pre-send validation (packet=${wire.length}/${prepared.packetBytes}, wireSha256=${wireSha256}, messageSha256=${messageSha256}, signature=${actualSignature})`);
  }
  return Buffer.from(wire).toString("base64");
}

function equalSnapshot(left: AccountSnapshot | null, right: AccountSnapshot | null): boolean {
  if (left === null || right === null) return left === right;
  return left.address === right.address
    && left.owner === right.owner
    && left.lamports === right.lamports
    && left.executable === right.executable
    && Buffer.from(left.data).equals(Buffer.from(right.data));
}

function snapshotMap(addresses: readonly string[], snapshots: readonly (AccountSnapshot | null)[]): Map<string, AccountSnapshot | null> {
  return new Map(addresses.map((account, index) => [account, snapshots[index] ?? null]));
}

type FinalizedTransaction = Awaited<ReturnType<typeof finalizedTransaction>>;

function finalizedMessageKeys(transaction: FinalizedTransaction): readonly string[] {
  return [
    ...transaction.transaction.message.staticAccountKeys,
    ...(transaction.meta?.loadedAddresses?.writable ?? []),
    ...(transaction.meta?.loadedAddresses?.readonly ?? []),
  ].map((key) => key.toBase58());
}

function finalizedLamportDelta(transaction: FinalizedTransaction, account: string): bigint | null {
  const meta = transaction.meta;
  if (!meta) return null;
  const index = finalizedMessageKeys(transaction).indexOf(account);
  if (index < 0 || index >= meta.preBalances.length || index >= meta.postBalances.length) return null;
  return BigInt(meta.postBalances[index]!) - BigInt(meta.preBalances[index]!);
}

function decodeToken(snapshot: AccountSnapshot | null, expectedMint: Address, expectedOwner: Address): TokenState | null {
  if (!snapshot || snapshot.owner !== PARTNER_ROUTE.asset.tokenProgram || snapshot.data.length !== 165) return null;
  const decoded = getTokenDecoder().decode(snapshot.data);
  if (decoded.mint !== expectedMint || decoded.owner !== expectedOwner) return null;
  return { mint: decoded.mint, owner: decoded.owner, amount: decoded.amount };
}


function authorizedAddress(value: string | null, label: string): Address {
  if (value === null || value.length === 0) throw new Error(`${label} is required before loading the user signer`);
  try {
    return address(value);
  } catch (error) {
    throw new Error(`${label} is not a valid Solana address`, { cause: error });
  }
}

function authorizedPositiveAmount(value: string | null, label: string, maximum: bigint): bigint {
  if (value === null || !/^[0-9]+$/.test(value)) throw new Error(`${label} must be an unsigned integer before loading the user signer`);
  const amount = BigInt(value);
  if (amount <= 0n || amount > maximum) throw new Error(`${label} must be in the range 1..${maximum} before loading the user signer`);
  return amount;
}

function authorizedSignature(value: string | null, label: string): string {
  if (value === null || value.length === 0) throw new Error(`${label} is required before loading the user signer`);
  try {
    if (bs58.decode(value).length !== 64) throw new Error("signature is not 64 bytes");
  } catch (error) {
    throw new Error(`${label} is not a valid Solana signature`, { cause: error });
  }
  return value;
}

function authorizedMaximum(value: string | null, label: string, expected: number): number {
  if (value !== expected.toString()) throw new Error(`${label} must equal the fixed machine limit ${expected} before loading the user signer`);
  return expected;
}

async function expectedReceiptForUser(user: Address): Promise<Address> {
  const [receipt] = await findRequestWithdrawVaultReceiptPda({
    vault: PARTNER_ROUTE.vault,
    userTransferAuthority: user,
  }, { programAddress: PARTNER_ROUTE.programs.voltrVault });
  return receipt;
}

async function authorizeWithdrawRequestBeforeSigner(
  confirmVault: string | null,
  confirmAmountLpRaw: string | null,
  confirmReceipt: string | null,
  confirmUser: string | null,
  confirmMaxTotalLamports: string | null,
): Promise<Readonly<{ user: Address; receipt: Address; amountLpRaw: bigint }>> {
  if (confirmVault !== PARTNER_ROUTE.vault) throw new Error(`execute withdraw-request requires --confirm-vault ${PARTNER_ROUTE.vault}`);
  const user = authorizedAddress(confirmUser, "execute withdraw-request --confirm-user");
  if (user !== PARTNER_ROUTE.setupAdmin) throw new Error(`execute withdraw-request POC user must equal ${PARTNER_ROUTE.setupAdmin}`);
  const receipt = authorizedAddress(confirmReceipt, "execute withdraw-request --confirm-receipt");
  // LP raw units use the LP mint's decimals and are not bounded by the
  // underlying asset's raw-unit vault cap. The finalized user balance remains
  // the economic bound; this pre-signer check only rejects non-u64 input.
  const amountLpRaw = authorizedPositiveAmount(confirmAmountLpRaw, "execute withdraw-request --confirm-amount-lp", MAX_U64_RAW);
  authorizedMaximum(confirmMaxTotalLamports, "execute withdraw-request --confirm-max-total-lamports", MAX_WITHDRAW_REQUEST_TOTAL_LAMPORTS);
  const expectedReceipt = await expectedReceiptForUser(user);
  if (receipt !== expectedReceipt) throw new Error(`execute withdraw-request requires the exact receipt PDA ${expectedReceipt} for --confirm-user ${user}`);
  return { user, receipt, amountLpRaw };
}

function sameNumbers(left: readonly number[], right: readonly number[]): boolean {
  return left.length === right.length && left.every((value, index) => value === right[index]);
}

function sameStrings(left: readonly string[], right: readonly string[]): boolean {
  return left.length === right.length && left.every((value, index) => value === right[index]);
}

function messageAccountRole(
  index: number,
  total: number,
  header: Readonly<{ numRequiredSignatures: number; numReadonlySignedAccounts: number; numReadonlyUnsignedAccounts: number }>,
): Readonly<{ signer: boolean; writable: boolean }> {
  const signer = index < header.numRequiredSignatures;
  const writable = signer
    ? index < header.numRequiredSignatures - header.numReadonlySignedAccounts
    : index < total - header.numReadonlyUnsignedAccounts;
  return { signer, writable };
}

/**
 * Compare the finalized request packet to the same SDK-built instructions.
 * The blockhash and signatures are intentionally excluded; every message
 * account, role, program index, and instruction byte remains exact.
 */
async function assertFinalizedWithdrawRequestPacket(
  transaction: Awaited<ReturnType<typeof finalizedTransaction>>,
  builder: Awaited<ReturnType<typeof createVoltrRouteBuilder>>,
  user: Address,
  amountLpRaw: bigint,
): Promise<void> {
  if (transaction.version !== 0) throw new Error("withdrawal request origin must be a version-0 transaction");
  const noopUser = createNoopSigner(user);
  const userAccounts = await builder.userAccounts(user);
  const createEscrowAta = await getCreateAssociatedTokenIdempotentInstructionAsync({
    payer: noopUser,
    ata: userAccounts.requestWithdrawLpAta,
    owner: userAccounts.requestWithdrawVaultReceipt,
    mint: builder.accounts.lpMint,
    systemProgram: PARTNER_ROUTE.programs.system,
    tokenProgram: PARTNER_ROUTE.programs.token,
  }, { programAddress: PARTNER_ROUTE.programs.associatedToken });
  const request = await builder.user.requestWithdraw({ user: noopUser, payer: noopUser }, amountLpRaw, true);
  const expected = new TransactionMessage({
    payerKey: new PublicKey(user),
    recentBlockhash: new PublicKey(new Uint8Array(32)).toBase58(),
    instructions: [createEscrowAta, request.raw].map(toWeb3Instruction),
  }).compileToV0Message([]);
  const actual = transaction.transaction.message;
  if (!("addressTableLookups" in actual) || actual.addressTableLookups.length !== 0) {
    throw new Error("withdrawal request origin must not use an address lookup table");
  }
  if (actual.header.numRequiredSignatures !== 1 || actual.header.numReadonlySignedAccounts !== 0) {
    throw new Error("withdrawal request origin must have exactly one writable signer");
  }
  const expectedKeys = expected.staticAccountKeys.map((key) => key.toBase58());
  const actualKeys = actual.staticAccountKeys.map((key) => key.toBase58());
  if (!sameStrings(actualKeys, expectedKeys)) throw new Error("withdrawal request origin static account keys are not the canonical SDK packet");
  if (actual.header.numReadonlyUnsignedAccounts !== expected.header.numReadonlyUnsignedAccounts) throw new Error("withdrawal request origin account roles are not canonical");
  for (let index = 0; index < expectedKeys.length; index += 1) {
    const expectedRole = messageAccountRole(index, expectedKeys.length, expected.header);
    const actualRole = messageAccountRole(index, actualKeys.length, actual.header);
    if (expectedRole.signer !== actualRole.signer || expectedRole.writable !== actualRole.writable) throw new Error("withdrawal request origin signer/writable roles are not canonical");
  }
  if (actual.compiledInstructions.length !== expected.compiledInstructions.length) throw new Error("withdrawal request origin must contain exactly the ATA-create and Voltr request instructions");
  for (let index = 0; index < expected.compiledInstructions.length; index += 1) {
    const expectedInstruction = expected.compiledInstructions[index]!;
    const actualInstruction = actual.compiledInstructions[index]!;
    if (actualInstruction.programIdIndex !== expectedInstruction.programIdIndex
      || !sameNumbers(actualInstruction.accountKeyIndexes, expectedInstruction.accountKeyIndexes)
      || !Buffer.from(actualInstruction.data).equals(Buffer.from(expectedInstruction.data))) {
      throw new Error(`withdrawal request origin instruction ${index} is not the canonical SDK packet`);
    }
  }
  if (transaction.transaction.signatures.length !== 1 || actualKeys[0] !== user) throw new Error("withdrawal request origin signer set is not exactly the confirmed user");
}

async function authorizeWithdrawClaimBeforeSigner(
  confirmReceipt: string | null,
  confirmDeadline: string | null,
  requestSignatureInput: string | null,
  confirmUser: string | null,
): Promise<Readonly<{ user: Address; receipt: Address; deadline: bigint; requestSignature: string }>> {
  const user = authorizedAddress(confirmUser, "execute withdraw-claim --confirm-user");
  if (user !== PARTNER_ROUTE.setupAdmin) throw new Error(`execute withdraw-claim POC user must equal ${PARTNER_ROUTE.setupAdmin}`);
  const receipt = authorizedAddress(confirmReceipt, "execute withdraw-claim --confirm-receipt");
  const deadline = authorizedPositiveAmount(confirmDeadline, "execute withdraw-claim --confirm-deadline", 9_999_999_999n);
  const requestSignature = authorizedSignature(requestSignatureInput, "execute withdraw-claim --request-signature");
  const expectedReceipt = await expectedReceiptForUser(user);
  if (receipt !== expectedReceipt) throw new Error(`execute withdraw-claim requires the exact receipt PDA ${expectedReceipt} for --confirm-user ${user}`);
  const accounts = await deriveVoltrAccounts(PARTNER_ROUTE);
  const receiptState = await confirmedSnapshots(rpcUrl(), [receipt]);
  const decodedReceipt = decodeReceipt(receiptState.accounts[0] ?? null);
  const [, expectedReceiptBump] = await findRequestWithdrawVaultReceiptPda({
    vault: PARTNER_ROUTE.vault,
    userTransferAuthority: user,
  }, { programAddress: PARTNER_ROUTE.programs.voltrVault });
  if (!decodedReceipt || decodedReceipt.vault !== PARTNER_ROUTE.vault || decodedReceipt.user !== user || decodedReceipt.withdrawableFromTs !== deadline || decodedReceipt.bump !== expectedReceiptBump || decodedReceipt.version !== 0) {
    throw new Error("execute withdraw-claim authorization does not match the finalized receipt state");
  }
  const reserve = await loadMainReserveGraph(rpcUrl(), PARTNER_ROUTE, accounts.strategyAuth, "confirmed", receiptState.contextSlot);
  const builder = await createVoltrRouteBuilder(PARTNER_ROUTE, reserve.graph);
  const requestTransaction = await confirmedTransaction(rpcUrl(), requestSignature);
  if (requestTransaction.slot > receiptState.contextSlot) {
    throw new Error(`withdrawal request origin slot ${requestTransaction.slot} postdates receipt snapshot ${receiptState.contextSlot}`);
  }
  await assertFinalizedWithdrawRequestPacket(requestTransaction, builder, user, decodedReceipt.amountLpEscrowed);
  const requestEvents = parseTransactionEvents({ logMessages: requestTransaction.meta?.logMessages ?? [] }).filter((event) => event.name === "RequestWithdrawVaultEvent");
  const requestEvent = requestEvents.length === 1 ? requestEvents[0]!.payload : null;
  if (!requestEvent || requestEvent.vault !== PARTNER_ROUTE.vault || requestEvent.user !== user || requestEvent.requestWithdrawVaultReceipt !== receipt || requestEvent.amountLpEscrowed !== decodedReceipt.amountLpEscrowed || requestEvent.withdrawableFromTs !== deadline || requestEvent.withdrawableFromTs - requestEvent.requestedTs !== PARTNER_ROUTE.vaultConfiguration.withdrawalWaitingPeriodSeconds) {
    throw new Error("execute withdraw-claim authorization does not match the finalized request event");
  }
  return { user, receipt, deadline, requestSignature };
}

function makeIntent(
  operation: UserRuntimeIntent["operation"],
  user: Address,
  amountRaw: bigint,
  prepared: PreparedTransaction,
  nonce: string,
  lifecycleId: string,
  protectedPrestateSha256: string,
): { intent: UserRuntimeIntent; intentSha256: string } {
  const intent: UserRuntimeIntent = {
    schemaVersion: 1,
    kind: "runtime",
    operation,
    signerRole: "user",
    user,
    amountRaw,
    lifecycleId,
    protectedPrestateSha256,
    routeId: PARTNER_FOUR_MARKET_ROUTE.id,
    routeSpecSha256: fourMarketRouteSpecSha256(),
    nonce,
    prestateSlot: BigInt(prepared.prestateSlot),
    expiresAtUnix: BigInt(Math.floor(Date.now() / 1_000) + 300),
    canonicalMessageSha256: sha256(prepared.serializedMessage),
  };
  assertIntentForRouteBinding(intent, {
    routeId: PARTNER_FOUR_MARKET_ROUTE.id,
    routeSpecSha256: fourMarketRouteSpecSha256(),
    maxManagerOperationRaw: PARTNER_ROUTE.asset.maxManagerOperationRaw,
  });
  return { intent, intentSha256: intentSha256(intent) };
}

async function loadContext(): Promise<RuntimeContext> {
  const user = await signingMaterialFromEnvironment("SOLANA_TESTING_PK");
  const accounts = await deriveVoltrAccounts(PARTNER_ROUTE);
  const reserve = await loadMainReserveGraph(rpcUrl(), PARTNER_ROUTE, accounts.strategyAuth, "confirmed");
  const builder = await createVoltrRouteBuilder(PARTNER_ROUTE, reserve.graph);
  const userAccounts = await builder.userAccounts(user.signer.address);
  return { route: PARTNER_ROUTE, user, accounts, userAccounts, builder };
}

function baseInspectedAddresses(context: RuntimeContext): string[] {
  const { route, accounts, userAccounts, user } = context;
  return [route.vault, accounts.lpMint, accounts.idleAta, route.asset.mint, userAccounts.userAssetAta, userAccounts.userLpAta, userAccounts.requestWithdrawLpAta, userAccounts.requestWithdrawVaultReceipt, user.signer.address];
}

function instructionSummary(instruction: VoltrInstruction): Readonly<Record<string, unknown>> {
  return { programId: instruction.canonical.programId, accountCount: instruction.canonical.accounts.length, dataLength: instruction.canonical.dataLength, dataSha256: instruction.canonical.dataSha256 };
}

function currentVaultGate(context: RuntimeContext, state: Map<string, AccountSnapshot | null>): ReturnType<typeof verifyVaultCurrentState> {
  return verifyVaultCurrentState({ route: context.route, accounts: context.accounts, vault: state.get(context.route.vault) ?? null, lpMint: state.get(context.accounts.lpMint) ?? null, idleAta: state.get(context.accounts.idleAta) ?? null, assetMint: state.get(context.route.asset.mint) ?? null });
}

function userTokenGate(gates: Gate[], label: string, snapshot: AccountSnapshot | null, expectedMint: Address, expectedOwner: Address): TokenState | null {
  const token = decodeToken(snapshot, expectedMint, expectedOwner);
  add(gates, `${label} exact token account`, token !== null, token ? { address: snapshot?.address, mint: token.mint, owner: token.owner, amount: token.amount } : null, { mint: expectedMint, owner: expectedOwner });
  return token;
}

function quoteAssetsForWithdraw(
  vaultSnapshot: AccountSnapshot | null,
  lpMintSnapshot: AccountSnapshot | null,
  lpAmount: bigint,
  currentTimeSec: bigint = BigInt(Math.floor(Date.now() / 1_000)),
): bigint {
  if (!vaultSnapshot || !lpMintSnapshot) throw new Error("instant withdrawal quote requires the vault and LP mint accounts");
  const vault = getVaultDecoder().decode(vaultSnapshot.data);
  const lpMint = getMintDecoder().decode(lpMintSnapshot.data);
  return calculateAssetsForWithdrawAmount({
    vaultTotalValue: vault.asset.totalValue,
    vaultLastUpdatedLockedProfit: vault.lockedProfitState.lastUpdatedLockedProfit,
    vaultLastReport: vault.lockedProfitState.lastReport,
    vaultLockedProfitDegradationDuration: vault.vaultConfiguration.lockedProfitDegradationDuration,
    vaultAccumulatedLpAdminFees: vault.feeState.accumulatedLpAdminFees,
    vaultAccumulatedLpManagerFees: vault.feeState.accumulatedLpManagerFees,
    vaultAccumulatedLpProtocolFees: vault.feeState.accumulatedLpProtocolFees,
    vaultDeadWeight: vault.deadWeight,
    vaultRedemptionFeeBps: vault.feeConfiguration.redemptionFee,
    vaultManagementFeeBps: vault.feeConfiguration.managerManagementFee + vault.feeConfiguration.adminManagementFee,
    vaultLastManagementFeeUpdateTs: vault.feeUpdate.lastManagementFeeUpdateTs,
    lpSupply: lpMint.supply,
    lpAmount,
    currentTimeSec,
  });
}

function instantPacketGate(prepared: PreparedTransaction, instruction: VoltrInstruction, user: Address): Gate {
  try {
    const transaction = VersionedTransaction.deserialize(Buffer.from(prepared.serializedTransaction));
    const message = transaction.message;
    const expectedMessage = new TransactionMessage({
      payerKey: new PublicKey(user),
      recentBlockhash: prepared.latestBlockhash.blockhash,
      instructions: [toWeb3Instruction(instruction.raw)],
    }).compileToV0Message([]);
    const keys = message.staticAccountKeys.map((key) => key.toBase58());
    const expectedKeys = expectedMessage.staticAccountKeys.map((key) => key.toBase58());
    const compiled = message.compiledInstructions;
    const expectedCompiled = expectedMessage.compiledInstructions;
    const actualInstruction = compiled.length === 1 ? compiled[0]! : null;
    const expectedInstruction = expectedCompiled.length === 1 ? expectedCompiled[0]! : null;
    const actualAccounts = actualInstruction === null ? [] : actualInstruction.accountKeyIndexes.map((index) => keys[index] ?? "");
    const expectedAccounts = expectedInstruction === null ? [] : expectedInstruction.accountKeyIndexes.map((index) => expectedKeys[index] ?? "");
    const actualRoles = actualInstruction === null ? [] : actualInstruction.accountKeyIndexes.map((index) => messageAccountRole(index, keys.length, message.header));
    const expectedRoles = expectedInstruction === null ? [] : expectedInstruction.accountKeyIndexes.map((index) => messageAccountRole(index, expectedKeys.length, expectedMessage.header));
    const actualProgram = actualInstruction === null ? null : keys[actualInstruction.programIdIndex] ?? null;
    const expectedProgram = expectedInstruction === null ? null : expectedKeys[expectedInstruction.programIdIndex] ?? null;
    const actualData = actualInstruction === null ? Buffer.alloc(0) : Buffer.from(actualInstruction.data);
    const expectedData = expectedInstruction === null ? Buffer.alloc(0) : Buffer.from(expectedInstruction.data);
    const canonicalAccounts = instruction.canonical.accounts.map((meta) => meta.address);
    const canonicalData = Buffer.from(instruction.canonical.data ?? []);
    const signerKeys = keys.slice(0, message.header.numRequiredSignatures);
    const headerExact = message.header.numRequiredSignatures === expectedMessage.header.numRequiredSignatures
      && message.header.numReadonlySignedAccounts === expectedMessage.header.numReadonlySignedAccounts
      && message.header.numReadonlyUnsignedAccounts === expectedMessage.header.numReadonlyUnsignedAccounts;
    const instructionExact = actualInstruction !== null
      && expectedInstruction !== null
      && actualInstruction.programIdIndex === expectedInstruction.programIdIndex
      && sameNumbers([...actualInstruction.accountKeyIndexes], [...expectedInstruction.accountKeyIndexes])
      && actualData.equals(expectedData);
    const canonicalInstructionExact = expectedProgram === instruction.canonical.programId
      && sameStrings(expectedAccounts, canonicalAccounts)
      && expectedData.equals(canonicalData);
    const feePayerRole = keys[0] === user
      && message.header.numRequiredSignatures >= 1
      && messageAccountRole(0, keys.length, message.header).signer
      && messageAccountRole(0, keys.length, message.header).writable;
    const pass = message.version === 0
      && expectedMessage.version === 0
      && message.addressTableLookups.length === 0
      && expectedMessage.addressTableLookups.length === 0
      && signerKeys.length === 1
      && signerKeys[0] === user
      && headerExact
      && sameStrings(keys, expectedKeys)
      && instructionExact
      && canonicalInstructionExact
      && actualProgram === expectedProgram
      && sameStrings(actualAccounts, expectedAccounts)
      && JSON.stringify(actualRoles) === JSON.stringify(expectedRoles)
      && message.recentBlockhash === expectedMessage.recentBlockhash
      && Buffer.from(message.serialize()).equals(Buffer.from(expectedMessage.serialize()))
      && feePayerRole;
    return { name: "instant withdrawal packet is exact and user-only", pass, observed: { version: message.version, addressTableLookups: message.addressTableLookups.length, header: message.header, staticAccountKeys: keys, signers: signerKeys, programId: actualProgram, accounts: actualAccounts, accountKeyIndexes: actualInstruction?.accountKeyIndexes ?? [], roles: actualRoles, dataSha256: sha256(actualData) }, expected: { version: 0, addressTableLookups: 0, header: expectedMessage.header, staticAccountKeys: expectedKeys, signers: [user], programId: expectedProgram, accounts: expectedAccounts, accountKeyIndexes: expectedInstruction?.accountKeyIndexes ?? [], roles: expectedRoles, dataSha256: sha256(expectedData) } };
  } catch (error) {
    return { name: "instant withdrawal packet is exact and user-only", pass: false, observed: error instanceof Error ? error.message : String(error), expected: "one v0 instruction, no ALT, exactly the confirmed user signer" };
  }
}

function exactConfirmedTokenDeltas(
  deltas: readonly Readonly<{ address: string; mint: string; deltaRaw: string }>[],
  expected: readonly Readonly<{ address: string; mint: string; deltaRaw: string }>[],
): boolean {
  const normalize = (rows: readonly Readonly<{ address: string; mint: string; deltaRaw: string }>[]) => rows
    .map((row) => `${row.address}|${row.mint}|${row.deltaRaw}`)
    .sort();
  return sameStrings(normalize(deltas), normalize(expected));
}

function simulatedPost(addresses: readonly string[], prepared: PreparedTransaction): Map<string, AccountSnapshot | null> {
  const post = snapshotMap(addresses, prepared.simulation.postAccounts);
  // RPC simulation represents a closed account as a zero-lamport, empty-data
  // account image, while finalized getMultipleAccounts returns null. Normalize
  // the two representations so close-account gates have identical semantics.
  for (const [account, snapshot] of post) {
    if (snapshot !== null && snapshot.lamports === 0 && snapshot.data.length === 0) post.set(account, null);
  }
  return post;
}

function reportEnvelope(verdict: string, prepared: PreparedTransaction, intentDigest: string, gates: readonly Gate[], transaction: Readonly<Record<string, unknown>>, extra: Readonly<Record<string, unknown>> = {}): Readonly<Record<string, unknown>> {
  const failedGateCount = gates.filter(({ pass }) => !pass).length;
  const failedVerdict = verdict.endsWith("_PASS")
    ? `${verdict.slice(0, -"_PASS".length)}_FAIL`
    : `${verdict}_FAIL`;
  return { verdict: failedGateCount === 0 ? verdict : failedVerdict, broadcast: false, readyForBroadcast: failedGateCount === 0, routeId: PARTNER_FOUR_MARKET_ROUTE.id, routeSpecSha256: fourMarketRouteSpecSha256(), intentSha256: intentDigest, transaction, simulation: { prestateSlot: prepared.prestateSlot, contextSlot: prepared.simulationSlot, err: prepared.simulation.err, unitsConsumed: prepared.simulation.unitsConsumed, logsSha256: sha256(Buffer.from(prepared.simulation.logs.join("\n"), "utf8")) }, failedGateCount, gates, ...extra };
}

function senderProof(
  signer: Address,
  expectedSignature: string,
  messageSha256: string,
  serializedTransactionSha256: string,
  confirmedSlot = 0,
  submission: Readonly<{
    submissionAttemptCount: number;
    submissionWireSha256: string;
    submissionAttempts: readonly Readonly<Record<string, unknown>>[];
  }> = {
    submissionAttemptCount: 0,
    submissionWireSha256: serializedTransactionSha256,
    submissionAttempts: [],
  },
): Readonly<Record<string, unknown>> {
  return {
    schemaVersion: 2,
    signerRole: "user",
    signer,
    senderSourceSha256: sha256(readFileSync(resolve(REPOSITORY_ROOT, "tools/backyard-voltr/src/runtime/commands.ts"))),
    persistedBeforeSend: true,
    sendAttemptCount: 1,
    submissionAttemptCount: submission.submissionAttemptCount,
    submissionWireSha256: submission.submissionWireSha256,
    submissionAttempts: submission.submissionAttempts,
    maxRetries: 0,
    recoveryByExpectedSignatureOnly: true,
    expectedSignature,
    serializedTransactionSha256,
    serializedMessageSha256: messageSha256,
    oneSendOnly: true,
    confirmedSlot,
  };
}

function persistenceContract(
  intentPath: string,
  intentFileSha256: string,
  expectedSignature: string,
  serializedTransactionSha256: string,
  serializedMessageSha256 = "",
  intentSha256 = "",
  lifecycleId = "",
  protectedPrestateSha256 = "",
): Readonly<Record<string, unknown>> {
  return {
    schemaVersion: 2,
    kind: "pre-send-signed-wire",
    serializedTransactionSha256,
    serializedMessageSha256,
    intentSha256,
    lifecycleId,
    protectedPrestateSha256,
    expectedSignature,
    persistedBeforeSend: true,
    recoveryByExpectedSignature: true,
    maxSendAttempts: 1,
    maxSubmissionAttempts: MAX_IDENTICAL_SUBMISSION_ATTEMPTS,
    oneSendOnly: true,
    maxRetries: 0,
    recoveryByExpectedSignatureOnly: true,
    submissionWireSha256: serializedTransactionSha256,
  };
}

async function refreshUserProtectedPreSend(
  preparation: Readonly<{ intent: UserRuntimeIntent; protectedState: ProtectedState }>,
  minimumContextSlot: number,
): Promise<Awaited<ReturnType<typeof loadFourMarketProtectedState>>> {
  const observed = await loadFourMarketProtectedState(rpcUrl(), minimumContextSlot);
  if (observed.addressSetSha256 !== preparation.protectedState.addressSetSha256
    || observed.stateSha256 !== preparation.intent.protectedPrestateSha256) {
    throw new Error("user protected prestate changed immediately before persistence/send; refusing broadcast");
  }
  return observed;
}

type DeploymentSnapshot = Awaited<ReturnType<typeof loadDeploymentIdentities>>;

function deploymentFingerprint(value: DeploymentSnapshot["identities"]): string {
  return JSON.stringify(value, (_key, entry) => typeof entry === "bigint" ? entry.toString() : entry);
}

function addDeploymentGates(gates: Gate[], before: DeploymentSnapshot, after: DeploymentSnapshot): void {
  gates.push(...verifyDeploymentIdentities(PARTNER_ROUTE, before.identities).map((gate) => ({ ...gate, name: `prestate deployment: ${gate.name}` })));
  gates.push(...verifyDeploymentIdentities(PARTNER_ROUTE, after.identities).map((gate) => ({ ...gate, name: `simulation deployment: ${gate.name}` })));
  add(gates, "privileged deployments unchanged across simulation", deploymentFingerprint(before.identities) === deploymentFingerprint(after.identities), after.identities, before.identities);
}

function addFinalDeploymentGates(gates: Gate[], before: DeploymentSnapshot, after: DeploymentSnapshot): void {
  gates.push(...verifyDeploymentIdentities(PARTNER_ROUTE, after.identities).map((gate) => ({ ...gate, name: `confirmed final deployment: ${gate.name}` })));
  add(gates, "privileged deployments unchanged through confirmed settlement", deploymentFingerprint(before.identities) === deploymentFingerprint(after.identities), after.identities, before.identities);
}

function addLamportDeltaGate(
  gates: Gate[],
  name: string,
  payerBefore: AccountSnapshot | null,
  payerAfter: AccountSnapshot | null,
  feeLamports: number,
  createdRentLamports: number,
  refundRentLamports = 0,
): void {
  const actual = payerBefore && payerAfter ? payerBefore.lamports - payerAfter.lamports : null;
  const simulationDebit = createdRentLamports - refundRentLamports;
  const finalizedDebit = feeLamports + createdRentLamports - refundRentLamports;
  add(gates, name, actual !== null && (actual === simulationDebit || actual === finalizedDebit), actual, {
    simulationBank: simulationDebit,
    finalizedTransaction: finalizedDebit,
    feeLamports,
    createdRentLamports,
    refundRentLamports,
  });
}

function addExactSpendGate(
  gates: Gate[],
  name: string,
  payerBefore: AccountSnapshot | null,
  payerAfter: AccountSnapshot | null,
  quotedFeeLamports: number,
  createdRentLamports: number,
  maximumTotalLamports: number,
  finalized: boolean,
  observedDebit?: bigint | null,
  refundRentLamports = 0,
): void {
  const actual = observedDebit === undefined
    ? payerBefore && payerAfter ? BigInt(payerBefore.lamports - payerAfter.lamports) : null
    : observedDebit;
  const grossSpend = quotedFeeLamports + createdRentLamports;
  const totalSpend = grossSpend - refundRentLamports;
  // The RPC account snapshots used by this runtime include the transaction
  // fee even for simulation. Keep simulation and finalized readback on the
  // same exact fee-plus-rent debit; the fixed cap still bounds the total.
  const expectedDebit = BigInt(totalSpend);
  add(gates, name, actual === expectedDebit && totalSpend <= maximumTotalLamports, {
    actual: actual?.toString() ?? null,
    quotedFeeLamports,
    createdRentLamports,
    expectedDebit,
    totalSpend,
    maximumTotalLamports,
    refundRentLamports,
    grossSpend,
  }, { expectedDebit, totalSpend, maximumTotalLamports });
}

function addRentGate(
  gates: Gate[],
  name: string,
  snapshot: AccountSnapshot | null,
  expectedLamports: number,
  shouldExist: boolean,
): void {
  add(
    gates,
    name,
    shouldExist ? snapshot !== null && snapshot.lamports === expectedLamports : snapshot === null,
    snapshot?.lamports ?? null,
    shouldExist ? expectedLamports : null,
  );
}

export type UserDepositPreparation = Readonly<{ context: RuntimeContext; amountRaw: bigint; prepared: PreparedTransaction; intent: UserRuntimeIntent; intentSha256: string; report: Readonly<Record<string, unknown>>; inspectedAddresses: readonly string[]; before: Map<string, AccountSnapshot | null>; deploymentsBefore: DeploymentSnapshot; protectedState: ProtectedState; protectedAddresses: readonly string[]; requestOrigin?: RequestOrigin }>;

async function prepareUserDeposit(amountRaw: bigint = PARTNER_ROUTE.asset.proofAmountRaw, lifecycle?: UserLifecycleAuthorization): Promise<UserDepositPreparation> {
  if (amountRaw <= 0n || amountRaw > PARTNER_ROUTE.asset.vaultCapRaw) throw new Error(`user deposit amount must be in the range 1..${PARTNER_ROUTE.asset.vaultCapRaw}`);
  const context = await loadContext();
  const instruction = await context.builder.user.deposit({ user: context.user.signer }, amountRaw);
  const createUserLpAta = await getCreateAssociatedTokenIdempotentInstructionAsync({
    payer: context.user.signer,
    ata: context.userAccounts.userLpAta,
    owner: context.user.signer.address,
    mint: context.accounts.lpMint,
    systemProgram: context.route.programs.system,
    tokenProgram: context.route.programs.token,
  }, { programAddress: context.route.programs.associatedToken });
  const protectedAddresses = fourMarketProtectedAddresses();
  const inspectedAddresses = baseInspectedAddresses(context);
  const beforeResponse = await confirmedSnapshots(rpcUrl(), inspectedAddresses);
  const before = snapshotMap(inspectedAddresses, beforeResponse.accounts);
  const protectedBefore = await loadFourMarketProtectedState(rpcUrl(), beforeResponse.contextSlot);
  const current = currentVaultGate(context, before);
  const deploymentsBefore = await loadDeploymentIdentities(rpcUrl(), context.route, beforeResponse.contextSlot, "confirmed");
  const sourcePrestate = decodeToken(
    before.get(context.userAccounts.userAssetAta) ?? null,
    context.route.asset.mint,
    context.user.signer.address,
  );
  if (!sourcePrestate) throw new Error("user deposit requires an exact finalized user USDC ATA");
  if (sourcePrestate.amount < amountRaw) {
    throw new Error(`user deposit exceeds finalized user USDC balance (${sourcePrestate.amount} < ${amountRaw})`);
  }
  const prepared = await prepareSignedV0Transaction({ rpcUrl: rpcUrl(), feePayer: context.user, instructions: [createUserLpAta, instruction.raw], prestateAddresses: protectedAddresses, inspectedAddresses, commitment: "confirmed" });
  const post = simulatedPost(inspectedAddresses, prepared);
  const protectedAfter = await loadFourMarketProtectedState(rpcUrl(), prepared.simulationSlot);
  if (lifecycle && (lifecycle.protectedPrestateSha256 !== protectedBefore.stateSha256 || lifecycle.protectedAddressSetSha256 !== protectedBefore.addressSetSha256)) throw new Error("user deposit protected-state confirmation does not match the exact confirmed prestate");
  const protectedState = protectedStateEnvelope(protectedBefore, protectedAfter);
  const after = currentVaultGate(context, post);
  const deploymentsAfter = await loadDeploymentIdentities(rpcUrl(), context.route, prepared.simulationSlot, "confirmed");
  const lpAtaRentLamports = await rentExemptionLamports(rpcUrl(), 165);
  const gates: Gate[] = [];
  gates.push(...current.gates.map((gate) => ({ ...gate, name: `prestate: ${gate.name}` })));
  gates.push(...after.gates.map((gate) => ({ ...gate, name: `simulation: ${gate.name}` })));
  addDeploymentGates(gates, deploymentsBefore, deploymentsAfter);
  const sourceBefore = userTokenGate(gates, "user asset prestate", before.get(context.userAccounts.userAssetAta) ?? null, context.route.asset.mint, context.user.signer.address);
  const sourceAfter = userTokenGate(gates, "user asset simulation", post.get(context.userAccounts.userAssetAta) ?? null, context.route.asset.mint, context.user.signer.address);
  const userLpBeforeSnapshot = before.get(context.userAccounts.userLpAta) ?? null;
  const userLpBefore = userLpBeforeSnapshot === null ? { mint: context.accounts.lpMint, owner: context.user.signer.address, amount: 0n } : userTokenGate(gates, "user LP prestate", userLpBeforeSnapshot, context.accounts.lpMint, context.user.signer.address);
  const userLpAfter = userTokenGate(gates, "user LP simulation", post.get(context.userAccounts.userLpAta) ?? null, context.accounts.lpMint, context.user.signer.address);
  const beforeVault = before.get(context.route.vault);
  const beforeLpMint = before.get(context.accounts.lpMint);
  let expectedLp = 0n;
  try {
    if (!beforeVault || !beforeLpMint) throw new Error("vault or LP mint absent");
    const vault = getVaultDecoder().decode(beforeVault.data);
    const lpMint = getMintDecoder().decode(beforeLpMint.data);
    expectedLp = calculateLpForDepositAmount({ vaultTotalValue: vault.asset.totalValue, vaultAccumulatedLpAdminFees: vault.feeState.accumulatedLpAdminFees, vaultAccumulatedLpManagerFees: vault.feeState.accumulatedLpManagerFees, vaultAccumulatedLpProtocolFees: vault.feeState.accumulatedLpProtocolFees, vaultDeadWeight: vault.deadWeight, vaultIssuanceFeeBps: vault.feeConfiguration.issuanceFee, vaultManagementFeeBps: vault.feeConfiguration.managerManagementFee + vault.feeConfiguration.adminManagementFee, vaultLastManagementFeeUpdateTs: vault.feeUpdate.lastManagementFeeUpdateTs, lpSupply: lpMint.supply, assetAmount: amountRaw, assetDecimals: context.route.asset.decimals, lpDecimals: lpMint.decimals, currentTimeSec: BigInt(Math.floor(Date.now() / 1_000)) });
  } catch (error) {
    add(gates, "LP mint quote decodes", false, error instanceof Error ? error.message : String(error), "exact SDK quote");
  }
  add(gates, "one idempotent user LP ATA instruction", createUserLpAta.programAddress === context.route.programs.associatedToken && createUserLpAta.accounts?.length === 6 && createUserLpAta.data?.length === 1 && createUserLpAta.data[0] === 1, { programId: createUserLpAta.programAddress, accountCount: createUserLpAta.accounts?.length ?? 0, data: Buffer.from(createUserLpAta.data ?? []).toString("hex") }, { programId: context.route.programs.associatedToken, accountCount: 6, discriminator: "01" });
  add(gates, "one canonical user-deposit instruction", instruction.canonical.programId === context.route.programs.voltrVault && instruction.canonical.accounts.length === 13, instructionSummary(instruction), { programId: context.route.programs.voltrVault, accountCount: 13 });
  add(gates, "simulation succeeded", prepared.simulation.err === null, prepared.simulation.err, null);
  add(gates, "source USDC delta", sourceBefore !== null && sourceAfter !== null && sourceBefore.amount - sourceAfter.amount === amountRaw, sourceBefore && sourceAfter ? sourceAfter.amount - sourceBefore.amount : null, -amountRaw);
  const idleBefore = decodeToken(before.get(context.accounts.idleAta) ?? null, context.route.asset.mint, context.accounts.idleAuth);
  const idleAfter = decodeToken(post.get(context.accounts.idleAta) ?? null, context.route.asset.mint, context.accounts.idleAuth);
  add(gates, "idle USDC delta", idleBefore !== null && idleAfter !== null && idleAfter.amount - idleBefore.amount === amountRaw, idleBefore && idleAfter ? idleAfter.amount - idleBefore.amount : null, amountRaw);
  const createdLpAtaRentLamports = userLpBeforeSnapshot === null ? post.get(context.userAccounts.userLpAta)?.lamports ?? 0 : 0;
  addRentGate(gates, "user LP ATA has exact rent exemption", post.get(context.userAccounts.userLpAta) ?? null, lpAtaRentLamports, true);
  addExactSpendGate(gates, "user deposit total SOL spend is quoted fee plus exact new LP ATA rent", before.get(context.user.signer.address) ?? null, post.get(context.user.signer.address) ?? null, prepared.feeLamports, createdLpAtaRentLamports, MAX_USER_DEPOSIT_TOTAL_LAMPORTS, false);
  add(gates, "LP minted exactly by SDK economics", userLpBefore !== null && userLpAfter !== null && userLpAfter.amount - userLpBefore.amount === expectedLp, userLpBefore && userLpAfter ? userLpAfter.amount - userLpBefore.amount : null, expectedLp);
  if (after.state && current.state) {
    add(gates, "vault total value delta", after.state.totalValueRaw - current.state.totalValueRaw === amountRaw, after.state.totalValueRaw - current.state.totalValueRaw, amountRaw);
    add(gates, "LP supply delta", after.state.lpSupplyRaw - current.state.lpSupplyRaw === expectedLp, after.state.lpSupplyRaw - current.state.lpSupplyRaw, expectedLp);
  }
  const protectedLifecycleId = lifecycle?.lifecycleId ?? sha256(Buffer.from(`simulation:user-deposit:${context.user.signer.address}:${protectedBefore.stateSha256}`, "utf8"));
  const { intent, intentSha256: digest } = makeIntent("user-deposit", context.user.signer.address, amountRaw, prepared, `user-deposit:${context.user.signer.address}:${amountRaw}`, protectedLifecycleId, protectedBefore.stateSha256);
  const report = reportEnvelope("PARTNER_USER_DEPOSIT_SIMULATION_PASS", prepared, digest, gates, { operation: "user-deposit", user: context.user.signer.address, vault: context.route.vault, amountRaw: amountRaw.toString(), expectedLpRaw: expectedLp.toString(), packetBytes: prepared.packetBytes, feeLamports: prepared.feeLamports, createdLpAtaRentLamports, expectedSignature: prepared.expectedSignature, instructions: [createUserLpAta.programAddress, instruction.canonical], canonicalMessageSha256: sha256(prepared.serializedMessage) }, { prestateContextSlot: beforeResponse.contextSlot, protectedState, protectedSnapshotEvidence: protectedSnapshotEvidenceEnvelope(protectedBefore, protectedAfter), deployments: { before: deploymentsBefore.identities, after: deploymentsAfter.identities } });
  return { context, amountRaw, prepared, intent, intentSha256: digest, report, inspectedAddresses, before, deploymentsBefore, protectedState, protectedAddresses };
}

export async function simulateUserDeposit(amountRaw: bigint = PARTNER_ROUTE.asset.proofAmountRaw) {
  return (await prepareUserDeposit(amountRaw)).report;
}

export async function executeUserDeposit(confirmVault: string | null, confirmAmountRaw: string | null, confirmUser: string | null, confirmMaxTotalLamports: string | null, intentPathInput: string | null, confirmLifecycleId: string | null, confirmProtectedPrestateSha256: string | null, confirmProtectedAddressSetSha256: string | null) {
  if (process.env.CONFIRM_MAINNET !== "1") throw new Error("execute user-deposit requires CONFIRM_MAINNET=1");
  const intentPath = requireRuntimeIntentPath(intentPathInput, "user-deposit");
  if (confirmVault !== PARTNER_ROUTE.vault) throw new Error(`execute user-deposit requires --confirm-vault ${PARTNER_ROUTE.vault}`);
  const authorizedUser = authorizedAddress(confirmUser, "execute user-deposit --confirm-user");
  if (authorizedUser !== PARTNER_ROUTE.setupAdmin) throw new Error(`execute user-deposit POC user must equal ${PARTNER_ROUTE.setupAdmin}`);
  const amountRaw = authorizedPositiveAmount(confirmAmountRaw, "execute user-deposit --confirm-amount-raw", PARTNER_ROUTE.asset.vaultCapRaw);
  authorizedMaximum(confirmMaxTotalLamports, "execute user-deposit --confirm-max-total-lamports", MAX_USER_DEPOSIT_TOTAL_LAMPORTS);
  const lifecycle: UserLifecycleAuthorization = {
    lifecycleId: authorizedSha256(confirmLifecycleId, "execute user-deposit --confirm-lifecycle-id"),
    protectedPrestateSha256: authorizedSha256(confirmProtectedPrestateSha256, "execute user-deposit --confirm-protected-prestate-sha256"),
    protectedAddressSetSha256: authorizedSha256(confirmProtectedAddressSetSha256, "execute user-deposit --confirm-protected-address-set-sha256"),
  };
  const preparation = await prepareUserDeposit(amountRaw, lifecycle);
  if (preparation.context.user.signer.address !== authorizedUser) throw new Error("user deposit signer does not match the pre-authorized user");
  if (preparation.report.readyForBroadcast !== true || preparation.report.failedGateCount !== 0) throw new Error("user deposit preflight failed; refusing broadcast");
  const refreshed = await confirmedSnapshots(rpcUrl(), preparation.inspectedAddresses, preparation.prepared.simulationSlot);
  const refreshedMap = snapshotMap(preparation.inspectedAddresses, refreshed.accounts);
  // USDC's global mint supply can change because of unrelated Circle activity.
  // Re-decode its route-critical semantics below, but do not bind a user deposit
  // to byte-for-byte equality of that unrelated volatile supply field.
  const changedAccounts = [...preparation.before.keys()].filter((account) => account !== preparation.context.route.asset.mint && !equalSnapshot(refreshedMap.get(account) ?? null, preparation.before.get(account) ?? null));
  if (changedAccounts.length > 0) throw new Error(`user deposit protected prestate changed after simulation (${changedAccounts.join(", ")}); refusing broadcast`);
  if (!currentVaultGate(preparation.context, refreshedMap).gates.every(({ pass }) => pass)) throw new Error("user deposit refreshed vault or asset-mint semantics changed; refusing broadcast");
  const refreshedDeployments = await loadDeploymentIdentities(rpcUrl(), PARTNER_ROUTE, refreshed.contextSlot, "confirmed");
  if (!verifyDeploymentIdentities(PARTNER_ROUTE, refreshedDeployments.identities).every(({ pass }) => pass)) throw new Error("user deposit approved deployment identity changed; refusing broadcast");
  const preSendProtected = await refreshUserProtectedPreSend(
    preparation,
    Math.max(preparation.prepared.simulationSlot, refreshed.contextSlot, refreshedDeployments.contextSlot),
  );
  const preSendAttestation = await createProtectedPreSendAttestation(preparation.context.user.signer, {
    lifecycleId: preparation.intent.lifecycleId,
    operation: preparation.intent.operation,
    expectedSignature: preparation.prepared.expectedSignature,
    messageSha256: sha256(preparation.prepared.serializedMessage),
    intentSha256: preparation.intentSha256,
    addressSetSha256: preSendProtected.addressSetSha256,
    preContextSlot: preSendProtected.contextSlot,
    preStateSha256: preSendProtected.stateSha256,
  });
  const authorizationContextSlot = Math.max(
    preparation.prepared.simulationSlot,
    refreshed.contextSlot,
    refreshedDeployments.contextSlot,
    preSendProtected.contextSlot,
  );
  const preSendPersistence = persistenceContract("", "", preparation.prepared.expectedSignature, sha256(preparation.prepared.serializedTransaction), sha256(preparation.prepared.serializedMessage), preparation.intentSha256, preparation.intent.lifecycleId, preparation.intent.protectedPrestateSha256);
  const createdLpAtaRentLamports = preparation.before.get(preparation.context.userAccounts.userLpAta) === null
    ? preparation.report.transaction && typeof preparation.report.transaction === "object" && "createdLpAtaRentLamports" in preparation.report.transaction
      ? Number((preparation.report.transaction as { createdLpAtaRentLamports?: string }).createdLpAtaRentLamports ?? 0)
      : 0
    : 0;
  const serializedTransactionBase64 = assertPreparedWire(preparation.prepared);
  const intentFileSha256 = persistRuntimeIntent(intentPath, {
    schemaVersion: 1,
    kind: "backyard-voltr-user-runtime-intent",
    operation: "user-deposit",
    routeId: PARTNER_FOUR_MARKET_ROUTE.id,
    routeSpecSha256: fourMarketRouteSpecSha256(),
    user: preparation.context.user.signer.address,
    vault: PARTNER_ROUTE.vault,
    amountRaw,
    expectedSignature: preparation.prepared.expectedSignature,
    serializedTransactionSha256: sha256(preparation.prepared.serializedTransaction),
    serializedTransactionBase64,
    serializedMessageSha256: sha256(preparation.prepared.serializedMessage),
    packetBytes: preparation.prepared.packetBytes,
    authorizationContextSlot,
    feeLamports: preparation.prepared.feeLamports,
    createdRentLamports: createdLpAtaRentLamports,
    maxTotalLamports: MAX_USER_DEPOSIT_TOTAL_LAMPORTS,
    persistenceContract: preSendPersistence,
    protectedSnapshotEvidence: { before: preSendProtected },
    protectedPrestateEvidence: preSendProtected,
    preSendAttestation,
    intent: preparation.intent,
  });
  verifyPersistedRuntimeIntent(intentPath, intentFileSha256, { ...preparation, protectedPreSend: preSendProtected, preSendAttestation }, preSendPersistence, authorizationContextSlot);
  let finalized: Awaited<ReturnType<typeof sendPreparedConfirmedOnce>> | null = null;
  try {
    finalized = await sendPreparedConfirmedOnce(rpcUrl(), preparation.prepared, authorizationContextSlot);
    if (finalized.err !== null) return { verdict: "PARTNER_USER_DEPOSIT_FINALIZED_WITH_ERROR", broadcast: true, intentPath, intentFileSha256, preflight: preparation.report, finalized } as const;
    const state = await confirmedSnapshots(rpcUrl(), preparation.inspectedAddresses, finalized.confirmedSlot);
    const finalizedDeployments = await loadDeploymentIdentities(rpcUrl(), PARTNER_ROUTE, state.contextSlot, "confirmed");
    const post = snapshotMap(preparation.inspectedAddresses, state.accounts);
    const expectedLp = BigInt(String((preparation.report.transaction as { expectedLpRaw?: string }).expectedLpRaw ?? "0"));
    const lpAtaRentLamports = await rentExemptionLamports(rpcUrl(), 165);
    const gates: Gate[] = [];
    gates.push(...verifyDeploymentIdentities(PARTNER_ROUTE, finalizedDeployments.identities).map((gate) => ({ ...gate, name: `finalized deployment: ${gate.name}` })));
    addRentGate(gates, "finalized user LP ATA has exact rent exemption", post.get(preparation.context.userAccounts.userLpAta) ?? null, lpAtaRentLamports, true);
    const finalizedFeeLamports = finalized.feeLamports ?? preparation.prepared.feeLamports;
    const createdLpAtaRentLamports = preparation.before.get(preparation.context.userAccounts.userLpAta) === null ? post.get(preparation.context.userAccounts.userLpAta)?.lamports ?? 0 : 0;
    const finalizedUserLamportDelta = finalized.lamportDeltas.find(({ address: value }) => value === preparation.context.user.signer.address)?.deltaRaw ?? null;
    addExactSpendGate(gates, "finalized user total SOL spend is quoted fee plus exact new LP ATA rent", preparation.before.get(preparation.context.user.signer.address) ?? null, post.get(preparation.context.user.signer.address) ?? null, finalizedFeeLamports, createdLpAtaRentLamports, MAX_USER_DEPOSIT_TOTAL_LAMPORTS, true, finalizedUserLamportDelta === null ? null : -BigInt(finalizedUserLamportDelta));
    const expectedTokenDeltas = [
      { address: preparation.context.userAccounts.userAssetAta, mint: preparation.context.route.asset.mint, deltaRaw: (-amountRaw).toString() },
      { address: preparation.context.accounts.idleAta, mint: preparation.context.route.asset.mint, deltaRaw: amountRaw.toString() },
      { address: preparation.context.userAccounts.userLpAta, mint: preparation.context.accounts.lpMint, deltaRaw: expectedLp.toString() },
    ];
    add(gates, "confirmed user deposit token deltas are exact and closed", exactConfirmedTokenDeltas(finalized.tokenDeltas, expectedTokenDeltas), finalized.tokenDeltas, expectedTokenDeltas);
    const vaultBefore = currentVaultGate(preparation.context, preparation.before).state;
    const vaultAfter = currentVaultGate(preparation.context, post).state;
    add(gates, "confirmed deposit vault accounting exact", vaultBefore !== null && vaultAfter !== null && vaultAfter.totalValueRaw - vaultBefore.totalValueRaw === amountRaw && vaultAfter.lpSupplyRaw - vaultBefore.lpSupplyRaw === expectedLp && vaultAfter.idleRaw - vaultBefore.idleRaw === amountRaw, vaultBefore && vaultAfter ? { totalValueDelta: vaultAfter.totalValueRaw - vaultBefore.totalValueRaw, lpSupplyDelta: vaultAfter.lpSupplyRaw - vaultBefore.lpSupplyRaw, idleDelta: vaultAfter.idleRaw - vaultBefore.idleRaw } : null, { totalValueDelta: amountRaw, lpSupplyDelta: expectedLp, idleDelta: amountRaw });
    addFinalDeploymentGates(gates, preparation.deploymentsBefore, finalizedDeployments);
    const failedGateCount = gates.filter(({ pass }) => !pass).length;
    const protectedAfter = await loadFourMarketProtectedState(rpcUrl(), state.contextSlot);
    const finalProtectedState = protectedStateEnvelope({ schemaVersion: 1, addressSetSha256: preSendProtected.addressSetSha256, contextSlot: preSendProtected.contextSlot, stateSha256: preSendProtected.stateSha256 }, protectedAfter);
    const protectedEvidence = protectedSnapshotEvidenceEnvelope(preSendProtected, protectedAfter);
    const settlementAttestation = await createProtectedSettlementAttestation(preparation.context.user.signer, {
      lifecycleId: preparation.intent.lifecycleId,
      operation: preparation.intent.operation,
      expectedSignature: preparation.prepared.expectedSignature,
      confirmedSignature: finalized.signature,
      messageSha256: sha256(preparation.prepared.serializedMessage),
      serializedTransactionSha256: sha256(preparation.prepared.serializedTransaction),
      intentSha256: preparation.intentSha256,
      addressSetSha256: preSendProtected.addressSetSha256,
      preAttestation: preSendAttestation,
      confirmedSlot: finalized.confirmedSlot,
      postContextSlot: protectedAfter.contextSlot,
      postStateSha256: protectedAfter.stateSha256,
    });
    return { verdict: failedGateCount === 0 ? "PARTNER_USER_DEPOSIT_FINALIZED_AND_VERIFIED" : "PARTNER_USER_DEPOSIT_FINALIZED_READBACK_FAIL", broadcast: true, intentPath, intentFileSha256, lifecycleId: preparation.intent.lifecycleId, protectedState: finalProtectedState, protectedSnapshotEvidence: protectedEvidence, preSendAttestation, settlementAttestation, senderProof: senderProof(preparation.context.user.signer.address, finalized.signature, sha256(preparation.prepared.serializedMessage), sha256(preparation.prepared.serializedTransaction), finalized.confirmedSlot, finalized), persistenceContract: persistenceContract(intentPath, intentFileSha256, finalized.signature, sha256(preparation.prepared.serializedTransaction), sha256(preparation.prepared.serializedMessage), preparation.intentSha256, preparation.intent.lifecycleId, preparation.intent.protectedPrestateSha256), intent: preparation.intent, intentSha256: preparation.intentSha256, preflight: preparation.report, finalized, readbackContextSlot: state.contextSlot, readback: { failedGateCount, gates } } as const;
  } catch (error) {
    if (finalized) return { verdict: "PARTNER_USER_DEPOSIT_FINALIZED_READBACK_ERROR", broadcast: true, intentPath, intentFileSha256, lifecycleId: preparation.intent.lifecycleId, protectedState: preparation.protectedState, senderProof: senderProof(preparation.context.user.signer.address, finalized.signature, sha256(preparation.prepared.serializedMessage), sha256(preparation.prepared.serializedTransaction), 0, finalized), persistenceContract: persistenceContract(intentPath, intentFileSha256, finalized.signature, sha256(preparation.prepared.serializedTransaction), sha256(preparation.prepared.serializedMessage), preparation.intentSha256, preparation.intent.lifecycleId, preparation.intent.protectedPrestateSha256), intent: preparation.intent, intentSha256: preparation.intentSha256, preflight: preparation.report, finalized, error: error instanceof Error ? error.message : String(error), recoveryInstruction: "Do not resend. The deposit is confirmed; rerun read-only vault/user reconciliation." } as const;
    const failedSubmission = submissionEvidence(error, preparation.prepared);
    return { verdict: "PARTNER_USER_DEPOSIT_BROADCAST_STATUS_UNKNOWN", broadcast: null, intentPath, intentFileSha256, lifecycleId: preparation.intent.lifecycleId, protectedState: preparation.protectedState, senderProof: senderProof(preparation.context.user.signer.address, preparation.prepared.expectedSignature, sha256(preparation.prepared.serializedMessage), sha256(preparation.prepared.serializedTransaction), 0, failedSubmission), persistenceContract: persistenceContract(intentPath, intentFileSha256, preparation.prepared.expectedSignature, sha256(preparation.prepared.serializedTransaction), sha256(preparation.prepared.serializedMessage), preparation.intentSha256, preparation.intent.lifecycleId, preparation.intent.protectedPrestateSha256), expectedSignature: preparation.prepared.expectedSignature, intent: preparation.intent, intentSha256: preparation.intentSha256, preflight: preparation.report, error: error instanceof Error ? error.message : String(error), recoveryInstruction: "Do not resend. Verify this exact signature and reload the vault, idle ATA, source ATA, LP ATA, and LP mint." } as const;
  }
}

export type InstantWithdrawPreparation = Readonly<{
  context: RuntimeContext;
  amountLpRaw: bigint;
  quoteAssetRaw: bigint;
  prepared: PreparedTransaction;
  intent: UserRuntimeIntent;
  intentSha256: string;
  report: Readonly<Record<string, unknown>>;
  inspectedAddresses: readonly string[];
  before: Map<string, AccountSnapshot | null>;
  deploymentsBefore: DeploymentSnapshot;
  protectedState: ProtectedState;
  protectedAddresses: readonly string[];
}>;

async function prepareInstantWithdrawRejection(amountLpRaw?: bigint, lifecycle?: UserLifecycleAuthorization): Promise<InstantWithdrawPreparation> {
  const context = await loadContext();
  const protectedAddresses = fourMarketProtectedAddresses();
  const inspectedAddresses = baseInspectedAddresses(context);
  const beforeResponse = await confirmedSnapshots(rpcUrl(), inspectedAddresses);
  const before = snapshotMap(inspectedAddresses, beforeResponse.accounts);
  const protectedBefore = await loadFourMarketProtectedState(rpcUrl(), beforeResponse.contextSlot);
  const current = currentVaultGate(context, before);
  const deploymentsBefore = await loadDeploymentIdentities(rpcUrl(), context.route, beforeResponse.contextSlot, "confirmed");
  const userLp = decodeToken(before.get(context.userAccounts.userLpAta) ?? null, context.accounts.lpMint, context.user.signer.address);
  if (!userLp) throw new Error("instant withdrawal requires an exact user LP ATA");
  const amount = amountLpRaw ?? userLp.amount;
  if (amount <= 0n || amount > userLp.amount) throw new Error(`instant withdrawal LP amount must be in range 1..${userLp.amount}`);
  const quoteAssetRaw = quoteAssetsForWithdraw(before.get(context.route.vault) ?? null, before.get(context.accounts.lpMint) ?? null, amount);
  if (quoteAssetRaw <= 0n) throw new Error("instant withdrawal quote must be positive");
  const idleBefore = decodeToken(before.get(context.accounts.idleAta) ?? null, context.route.asset.mint, context.accounts.idleAuth);
  if (!idleBefore || idleBefore.amount < quoteAssetRaw) throw new Error(`instant withdrawal requires confirmed idle USDC >= quoted payout (${idleBefore?.amount ?? 0n} < ${quoteAssetRaw})`);
  const instruction = await context.builder.user.instantWithdraw({ user: context.user.signer }, amount, true);
  const prepared = await prepareSignedV0Transaction({
    rpcUrl: rpcUrl(),
    feePayer: context.user,
    instructions: [instruction.raw],
    prestateAddresses: protectedAddresses,
    inspectedAddresses,
    minimumContextSlot: beforeResponse.contextSlot,
    commitment: "confirmed",
  });
  const protectedAfter = await loadFourMarketProtectedState(rpcUrl(), prepared.simulationSlot);
  if (lifecycle && (lifecycle.protectedPrestateSha256 !== protectedBefore.stateSha256 || lifecycle.protectedAddressSetSha256 !== protectedBefore.addressSetSha256)) throw new Error("instant withdrawal protected-state confirmation does not match the exact confirmed prestate");
  const protectedState = protectedStateEnvelope(protectedBefore, protectedAfter);
  const rejectionReadback = await confirmedSnapshots(rpcUrl(), inspectedAddresses, prepared.simulationSlot);
  const rejectionPost = snapshotMap(inspectedAddresses, rejectionReadback.accounts);
  const vaultSnapshot = before.get(context.route.vault) ?? null;
  const vaultConfiguration = vaultSnapshot ? getVaultDecoder().decode(vaultSnapshot.data).vaultConfiguration : null;
  const gates: Gate[] = [];
  gates.push(...current.gates.map((gate) => ({ ...gate, name: `prestate: ${gate.name}` })));
  gates.push(instantPacketGate(prepared, instruction, context.user.signer.address));
  const simulationErrorCode = simulationCustomErrorCode(prepared.simulation.err, prepared.simulation.logs);
  const exactInstructionError = (() => {
    const value = prepared.simulation.err;
    if (!value || typeof value !== "object") return false;
    const error = (value as { InstructionError?: unknown }).InstructionError;
    if (!Array.isArray(error) || error.length !== 2 || error[0] !== 0) return false;
    const detail = error[1];
    return !!detail && typeof detail === "object" && (detail as { Custom?: unknown }).Custom === 6015;
  })();
  const rejectionLogs = prepared.simulation.logs.filter((line) => line.includes("InstantWithdrawNotAllowed") || line.includes("custom program error: 0x177f") || line.includes("6015"));
  const events = parseTransactionEvents({ logMessages: prepared.simulation.logs }).filter((event) => event.name === "InstantWithdrawVaultEvent");
  const rejectionAddresses = [context.route.vault, context.accounts.lpMint, context.accounts.idleAta, context.userAccounts.userAssetAta, context.userAccounts.userLpAta, context.user.signer.address];
  const changedAccounts = rejectionAddresses.filter((account) => !equalSnapshot(before.get(account) ?? null, rejectionPost.get(account) ?? null));
  const protectedReadback = await loadFourMarketProtectedState(rpcUrl(), rejectionReadback.contextSlot);
  const deploymentsReadback = await loadDeploymentIdentities(rpcUrl(), context.route, rejectionReadback.contextSlot, "confirmed");
  add(gates, "one canonical instant withdrawal instruction", instruction.canonical.programId === context.route.programs.voltrVault && instruction.canonical.accounts.length === 12, instructionSummary(instruction), { programId: context.route.programs.voltrVault, accountCount: 12 });
  add(gates, "instant withdrawal rejects with exact Custom 6015", exactInstructionError && simulationErrorCode === 6015, { err: prepared.simulation.err, code: simulationErrorCode }, { InstructionError: [0, { Custom: 6015 }] });
  add(gates, "simulation logs identify InstantWithdrawNotAllowed", rejectionLogs.length > 0, prepared.simulation.logs, "InstantWithdrawNotAllowed");
  add(gates, "instant withdrawal rejection emits no vault event", events.length === 0, events, []);
  add(gates, "instant withdrawal rejection readback is context-ordered", rejectionReadback.contextSlot >= prepared.simulationSlot, { simulationSlot: prepared.simulationSlot, readbackContextSlot: rejectionReadback.contextSlot }, { minimumReadbackContextSlot: prepared.simulationSlot });
  add(gates, "instant withdrawal route wait is exactly 600 seconds", vaultConfiguration?.withdrawalWaitingPeriod === 600n, vaultConfiguration?.withdrawalWaitingPeriod ?? null, 600n);
  add(gates, "instant withdrawal disabled operations are zero", vaultConfiguration?.disabledOperations === 0, vaultConfiguration?.disabledOperations ?? null, 0);
  add(gates, "confirmed idle covers exact quote", idleBefore.amount >= quoteAssetRaw, idleBefore.amount, `>=${quoteAssetRaw}`);
  addDeploymentGates(gates, deploymentsBefore, deploymentsReadback);
  const protectedLifecycleId = lifecycle?.lifecycleId ?? sha256(Buffer.from(`simulation:instant-withdraw-rejection:${context.user.signer.address}:${protectedBefore.stateSha256}`, "utf8"));
  const { intent, intentSha256: digest } = makeIntent("instant-withdraw", context.user.signer.address, amount, prepared, `instant-withdraw:${context.user.signer.address}:${amount}`, protectedLifecycleId, protectedBefore.stateSha256);
  const report = reportEnvelope("PARTNER_INSTANT_WITHDRAW_REJECTION_PASS", prepared, digest, gates, { operation: "instant-withdraw", mode: "rejection", user: context.user.signer.address, vault: context.route.vault, amountLpRaw: amount.toString(), quoteAssetRaw: quoteAssetRaw.toString(), withdrawalWaitingPeriodSeconds: "600", disabledOperations: 0, withdrawAll: true, packetBytes: prepared.packetBytes, feeLamports: prepared.feeLamports, expectedSignature: prepared.expectedSignature, instruction: instruction.canonical, canonicalMessageSha256: sha256(prepared.serializedMessage), serializedTransactionBase64: Buffer.from(prepared.serializedTransaction).toString("base64"), serializedTransactionSha256: sha256(prepared.serializedTransaction), serializedMessageBase64: Buffer.from(prepared.serializedMessage).toString("base64"), serializedMessageSha256: sha256(prepared.serializedMessage), simulationErrorCode, rejectionLogs, noEventCount: events.length, rejectionReadback: { contextSlot: rejectionReadback.contextSlot, changedAccounts, protectedState: protectedStateEnvelope(protectedBefore, protectedReadback), protectedSnapshotEvidence: protectedSnapshotEvidenceEnvelope(protectedBefore, protectedReadback), deployments: deploymentsReadback.identities } }, { broadcast: false, readyForBroadcast: false, intent, prestateContextSlot: beforeResponse.contextSlot, protectedState, protectedSnapshotEvidence: protectedSnapshotEvidenceEnvelope(protectedBefore, protectedAfter), confirmationCommitment: "confirmed", deployments: { before: deploymentsBefore.identities, after: deploymentsReadback.identities }, rejectionReadbackContextSlot: rejectionReadback.contextSlot });
  return { context, amountLpRaw: amount, quoteAssetRaw, prepared, intent, intentSha256: digest, report, inspectedAddresses, before, deploymentsBefore, protectedState, protectedAddresses };
}

export async function simulateInstantWithdrawRejection(amountLpRaw?: bigint) {
  return (await prepareInstantWithdrawRejection(amountLpRaw)).report;
}

export type WithdrawRequestPreparation = Readonly<{ context: RuntimeContext; amountLpRaw: bigint; prepared: PreparedTransaction; intent: UserRuntimeIntent; intentSha256: string; report: Readonly<Record<string, unknown>>; inspectedAddresses: readonly string[]; before: Map<string, AccountSnapshot | null>; deploymentsBefore: DeploymentSnapshot; protectedState: ProtectedState; protectedAddresses: readonly string[] }>;

async function prepareWithdrawRequest(amountLpRaw?: bigint, lifecycle?: UserLifecycleAuthorization): Promise<WithdrawRequestPreparation> {
  const context = await loadContext();
  const protectedAddresses = fourMarketProtectedAddresses();
  const inspectedAddresses = baseInspectedAddresses(context);
  const beforeResponse = await confirmedSnapshots(rpcUrl(), inspectedAddresses);
  const before = snapshotMap(inspectedAddresses, beforeResponse.accounts);
  const protectedBefore = await loadFourMarketProtectedState(rpcUrl(), beforeResponse.contextSlot);
  const current = currentVaultGate(context, before);
  const deploymentsBefore = await loadDeploymentIdentities(rpcUrl(), context.route, beforeResponse.contextSlot, "confirmed");
  const userLp = decodeToken(before.get(context.userAccounts.userLpAta) ?? null, context.accounts.lpMint, context.user.signer.address);
  if (!userLp) throw new Error("withdrawal request requires an exact user LP ATA");
  const amount = amountLpRaw ?? userLp.amount;
  if (amount <= 0n || amount > userLp.amount) throw new Error(`withdrawal LP amount must be in range 1..${userLp.amount}`);
  if (before.get(context.userAccounts.requestWithdrawVaultReceipt) !== null) throw new Error("withdrawal request receipt already exists; refusing replacement");
  const createEscrowAta = await getCreateAssociatedTokenIdempotentInstructionAsync({ payer: context.user.signer, ata: context.userAccounts.requestWithdrawLpAta, owner: context.userAccounts.requestWithdrawVaultReceipt, mint: context.accounts.lpMint, systemProgram: context.route.programs.system, tokenProgram: context.route.programs.token }, { programAddress: context.route.programs.associatedToken });
  const request = await context.builder.user.requestWithdraw({ user: context.user.signer, payer: context.user.signer }, amount, true);
  const prepared = await prepareSignedV0Transaction({ rpcUrl: rpcUrl(), feePayer: context.user, instructions: [createEscrowAta, request.raw], prestateAddresses: protectedAddresses, inspectedAddresses, commitment: "confirmed" });
  const post = simulatedPost(inspectedAddresses, prepared);
  const protectedAfter = await loadFourMarketProtectedState(rpcUrl(), prepared.simulationSlot);
  if (lifecycle && (lifecycle.protectedPrestateSha256 !== protectedBefore.stateSha256 || lifecycle.protectedAddressSetSha256 !== protectedBefore.addressSetSha256)) throw new Error("withdrawal request protected-state confirmation does not match the exact confirmed prestate");
  const protectedState = protectedStateEnvelope(protectedBefore, protectedAfter);
  const after = currentVaultGate(context, post);
  const deploymentsAfter = await loadDeploymentIdentities(rpcUrl(), context.route, prepared.simulationSlot, "confirmed");
  const tokenAtaRentLamports = await rentExemptionLamports(rpcUrl(), 165);
  const receiptRentLamports = await rentExemptionLamports(rpcUrl(), RECEIPT_DATA_LENGTH);
  const gates: Gate[] = [];
  gates.push(...current.gates.map((gate) => ({ ...gate, name: `prestate: ${gate.name}` })));
  gates.push(...after.gates.map((gate) => ({ ...gate, name: `simulation: ${gate.name}` })));
  addDeploymentGates(gates, deploymentsBefore, deploymentsAfter);
  const lpAfter = userTokenGate(gates, "user LP simulation", post.get(context.userAccounts.userLpAta) ?? null, context.accounts.lpMint, context.user.signer.address);
  const escrowAfter = userTokenGate(gates, "escrow LP simulation", post.get(context.userAccounts.requestWithdrawLpAta) ?? null, context.accounts.lpMint, context.userAccounts.requestWithdrawVaultReceipt);
  const receipt = decodeReceipt(post.get(context.userAccounts.requestWithdrawVaultReceipt) ?? null);
  const [, expectedReceiptBump] = await findRequestWithdrawVaultReceiptPda({
    vault: context.route.vault,
    userTransferAuthority: context.user.signer.address,
  }, { programAddress: context.route.programs.voltrVault });
  const events = parseTransactionEvents({ logMessages: prepared.simulation.logs }).filter((event) => event.name === "RequestWithdrawVaultEvent");
  const event = events.length === 1 ? events[0]!.payload : null;
  add(gates, "one escrow ATA instruction", createEscrowAta.programAddress === context.route.programs.associatedToken && createEscrowAta.accounts?.length === 6 && createEscrowAta.data?.length === 1 && createEscrowAta.data[0] === 1, { programId: createEscrowAta.programAddress, accountCount: createEscrowAta.accounts?.length ?? 0, data: Buffer.from(createEscrowAta.data ?? []).toString("hex") }, { programId: context.route.programs.associatedToken, accountCount: 6, discriminator: "01" });
  add(gates, "one canonical Voltr withdrawal request", request.canonical.programId === context.route.programs.voltrVault && request.canonical.accounts.length === 10, instructionSummary(request), { programId: context.route.programs.voltrVault, accountCount: 10 });
  add(gates, "simulation succeeded", prepared.simulation.err === null, prepared.simulation.err, null);
  add(gates, "user LP delta", userLp.amount - (lpAfter?.amount ?? -1n) === amount, lpAfter ? lpAfter.amount - userLp.amount : null, -amount);
  add(gates, "escrow LP exact", escrowAfter?.amount === amount, escrowAfter?.amount ?? null, amount);
  add(gates, "one exact request event", event !== null, event, "RequestWithdrawVaultEvent");
  add(gates, "receipt exact and linked", receipt !== null && receipt.vault === context.route.vault && receipt.user === context.user.signer.address && receipt.amountLpEscrowed === amount && event !== null && receipt.withdrawableFromTs === event.withdrawableFromTs, receipt, { vault: context.route.vault, user: context.user.signer.address, amountLpEscrowed: amount.toString() });
  add(gates, "receipt canonical bump and version", receipt?.bump === expectedReceiptBump && receipt.version === 0, receipt ? { bump: receipt.bump, version: receipt.version } : null, { bump: expectedReceiptBump, version: 0 });
  add(gates, "exact 600-second receipt", receipt !== null && event !== null && receipt.withdrawableFromTs - event.requestedTs === context.route.vaultConfiguration.withdrawalWaitingPeriodSeconds, receipt && event ? receipt.withdrawableFromTs - event.requestedTs : null, context.route.vaultConfiguration.withdrawalWaitingPeriodSeconds);
  add(gates, "request uses LP amount and withdraw-all", event !== null && event.requestedAmount === amount && event.isAmountInLp === true && event.isWithdrawAll === true, event ? { requestedAmount: event.requestedAmount, isAmountInLp: event.isAmountInLp, isWithdrawAll: event.isWithdrawAll } : null, { requestedAmount: amount, isAmountInLp: true, isWithdrawAll: true });
  add(gates, "vault accounting unchanged", current.state !== null && after.state !== null && current.state.totalValueRaw === after.state.totalValueRaw && current.state.lpSupplyRaw === after.state.lpSupplyRaw && current.state.idleRaw === after.state.idleRaw, current.state && after.state ? { before: current.state, after: after.state } : null, "unchanged");
  const createdEscrowAtaRentLamports = before.get(context.userAccounts.requestWithdrawLpAta) === null ? post.get(context.userAccounts.requestWithdrawLpAta)?.lamports ?? 0 : 0;
  const createdReceiptRentLamports = before.get(context.userAccounts.requestWithdrawVaultReceipt) === null ? post.get(context.userAccounts.requestWithdrawVaultReceipt)?.lamports ?? 0 : 0;
  addRentGate(gates, "escrow LP ATA has exact rent exemption", post.get(context.userAccounts.requestWithdrawLpAta) ?? null, tokenAtaRentLamports, true);
  addRentGate(gates, "withdrawal receipt has exact rent exemption", post.get(context.userAccounts.requestWithdrawVaultReceipt) ?? null, receiptRentLamports, true);
  addExactSpendGate(gates, "withdraw request total SOL spend is quoted fee plus exact new ATA/receipt rent", before.get(context.user.signer.address) ?? null, post.get(context.user.signer.address) ?? null, prepared.feeLamports, createdEscrowAtaRentLamports + createdReceiptRentLamports, MAX_WITHDRAW_REQUEST_TOTAL_LAMPORTS, false);
  const protectedLifecycleId = lifecycle?.lifecycleId ?? sha256(Buffer.from(`simulation:withdraw-request:${context.user.signer.address}:${protectedBefore.stateSha256}`, "utf8"));
  const { intent, intentSha256: digest } = makeIntent("withdraw-request", context.user.signer.address, amount, prepared, `withdraw-request:${context.user.signer.address}:${amount}`, protectedLifecycleId, protectedBefore.stateSha256);
  const report = reportEnvelope("PARTNER_WITHDRAW_REQUEST_SIMULATION_PASS", prepared, digest, gates, { operation: "withdraw-request", user: context.user.signer.address, vault: context.route.vault, amountLpRaw: amount.toString(), withdrawAll: true, receipt: receipt ? { address: context.userAccounts.requestWithdrawVaultReceipt, withdrawableFromTs: receipt.withdrawableFromTs.toString(), amountLpEscrowed: receipt.amountLpEscrowed.toString() } : null, event, packetBytes: prepared.packetBytes, feeLamports: prepared.feeLamports, createdEscrowAtaRentLamports, createdReceiptRentLamports, expectedSignature: prepared.expectedSignature, instructions: [createEscrowAta.programAddress, request.canonical], canonicalMessageSha256: sha256(prepared.serializedMessage) }, { prestateContextSlot: beforeResponse.contextSlot, protectedState, protectedSnapshotEvidence: protectedSnapshotEvidenceEnvelope(protectedBefore, protectedAfter), deployments: { before: deploymentsBefore.identities, after: deploymentsAfter.identities } });
  return { context, amountLpRaw: amount, prepared, intent, intentSha256: digest, report, inspectedAddresses, before, deploymentsBefore, protectedState, protectedAddresses };
}

export async function simulateWithdrawRequest(amountLpRaw?: bigint) {
  return (await prepareWithdrawRequest(amountLpRaw)).report;
}

export async function executeWithdrawRequest(confirmVault: string | null, confirmAmountLpRaw: string | null, confirmReceipt: string | null, confirmUser: string | null, confirmMaxTotalLamports: string | null, intentPathInput: string | null, confirmLifecycleId: string | null, confirmProtectedPrestateSha256: string | null, confirmProtectedAddressSetSha256: string | null) {
  if (process.env.CONFIRM_MAINNET !== "1") throw new Error("execute withdraw-request requires CONFIRM_MAINNET=1");
  const intentPath = requireRuntimeIntentPath(intentPathInput, "withdraw-request");
  const authorization = await authorizeWithdrawRequestBeforeSigner(confirmVault, confirmAmountLpRaw, confirmReceipt, confirmUser, confirmMaxTotalLamports);
  const amount = authorization.amountLpRaw;
  const lifecycle: UserLifecycleAuthorization = {
    lifecycleId: authorizedSha256(confirmLifecycleId, "execute withdraw-request --confirm-lifecycle-id"),
    protectedPrestateSha256: authorizedSha256(confirmProtectedPrestateSha256, "execute withdraw-request --confirm-protected-prestate-sha256"),
    protectedAddressSetSha256: authorizedSha256(confirmProtectedAddressSetSha256, "execute withdraw-request --confirm-protected-address-set-sha256"),
  };
  const preparation = await prepareWithdrawRequest(amount, lifecycle);
  if (preparation.context.user.signer.address !== authorization.user || preparation.context.userAccounts.requestWithdrawVaultReceipt !== authorization.receipt) throw new Error("withdrawal request signer does not match the pre-authorized user and receipt");
  if (preparation.report.readyForBroadcast !== true || preparation.report.failedGateCount !== 0) throw new Error("withdrawal request preflight failed; refusing broadcast");
  const refreshed = await confirmedSnapshots(rpcUrl(), preparation.inspectedAddresses, preparation.prepared.simulationSlot);
  const refreshedMap = snapshotMap(preparation.inspectedAddresses, refreshed.accounts);
  const changedAccounts = [...preparation.before.keys()].filter((account) => account !== preparation.context.route.asset.mint && !equalSnapshot(refreshedMap.get(account) ?? null, preparation.before.get(account) ?? null));
  if (changedAccounts.length > 0) throw new Error(`withdrawal request protected prestate changed after simulation (${changedAccounts.join(", ")}); refusing broadcast`);
  if (!currentVaultGate(preparation.context, refreshedMap).gates.every(({ pass }) => pass)) throw new Error("withdrawal request refreshed vault or asset-mint semantics changed; refusing broadcast");
  const refreshedDeployments = await loadDeploymentIdentities(rpcUrl(), PARTNER_ROUTE, refreshed.contextSlot, "confirmed");
  if (!verifyDeploymentIdentities(PARTNER_ROUTE, refreshedDeployments.identities).every(({ pass }) => pass)) throw new Error("withdrawal request approved deployment identity changed; refusing broadcast");
  const preSendProtected = await refreshUserProtectedPreSend(
    preparation,
    Math.max(preparation.prepared.simulationSlot, refreshed.contextSlot, refreshedDeployments.contextSlot),
  );
  const preSendAttestation = await createProtectedPreSendAttestation(preparation.context.user.signer, {
    lifecycleId: preparation.intent.lifecycleId,
    operation: preparation.intent.operation,
    expectedSignature: preparation.prepared.expectedSignature,
    messageSha256: sha256(preparation.prepared.serializedMessage),
    intentSha256: preparation.intentSha256,
    addressSetSha256: preSendProtected.addressSetSha256,
    preContextSlot: preSendProtected.contextSlot,
    preStateSha256: preSendProtected.stateSha256,
  });
  const authorizationContextSlot = Math.max(
    preparation.prepared.simulationSlot,
    refreshed.contextSlot,
    refreshedDeployments.contextSlot,
    preSendProtected.contextSlot,
  );
  const preSendPersistence = persistenceContract("", "", preparation.prepared.expectedSignature, sha256(preparation.prepared.serializedTransaction), sha256(preparation.prepared.serializedMessage), preparation.intentSha256, preparation.intent.lifecycleId, preparation.intent.protectedPrestateSha256);
  const serializedTransactionBase64 = assertPreparedWire(preparation.prepared);
  const intentFileSha256 = persistRuntimeIntent(intentPath, {
    schemaVersion: 1,
    kind: "backyard-voltr-user-runtime-intent",
    operation: "withdraw-request",
    routeId: PARTNER_FOUR_MARKET_ROUTE.id,
    routeSpecSha256: fourMarketRouteSpecSha256(),
    user: preparation.context.user.signer.address,
    vault: PARTNER_ROUTE.vault,
    receipt: preparation.context.userAccounts.requestWithdrawVaultReceipt,
    amountLpRaw: preparation.amountLpRaw,
    expectedSignature: preparation.prepared.expectedSignature,
    serializedTransactionSha256: sha256(preparation.prepared.serializedTransaction),
    serializedTransactionBase64,
    serializedMessageSha256: sha256(preparation.prepared.serializedMessage),
    packetBytes: preparation.prepared.packetBytes,
    authorizationContextSlot,
    feeLamports: preparation.prepared.feeLamports,
    createdRentLamports: Number((preparation.report.transaction as { createdEscrowAtaRentLamports?: string }).createdEscrowAtaRentLamports ?? 0) + Number((preparation.report.transaction as { createdReceiptRentLamports?: string }).createdReceiptRentLamports ?? 0),
    maxTotalLamports: MAX_WITHDRAW_REQUEST_TOTAL_LAMPORTS,
    withdrawalWaitingPeriodSeconds: PARTNER_ROUTE.vaultConfiguration.withdrawalWaitingPeriodSeconds,
    persistenceContract: preSendPersistence,
    protectedSnapshotEvidence: { before: preSendProtected },
    protectedPrestateEvidence: preSendProtected,
    preSendAttestation,
    intent: preparation.intent,
  });
  verifyPersistedRuntimeIntent(intentPath, intentFileSha256, { ...preparation, protectedPreSend: preSendProtected, preSendAttestation }, preSendPersistence, authorizationContextSlot);
  let finalized: Awaited<ReturnType<typeof sendPreparedConfirmedOnce>> | null = null;
  try {
    finalized = await sendPreparedConfirmedOnce(rpcUrl(), preparation.prepared, authorizationContextSlot);
    if (finalized.err !== null) return { verdict: "PARTNER_WITHDRAW_REQUEST_FINALIZED_WITH_ERROR", broadcast: true, intentPath, intentFileSha256, preflight: preparation.report, finalized } as const;
    const state = await confirmedSnapshots(rpcUrl(), preparation.inspectedAddresses, finalized.confirmedSlot);
    const finalizedDeployments = await loadDeploymentIdentities(rpcUrl(), PARTNER_ROUTE, state.contextSlot, "confirmed");
    const post = snapshotMap(preparation.inspectedAddresses, state.accounts);
    const receipt = decodeReceipt(post.get(preparation.context.userAccounts.requestWithdrawVaultReceipt) ?? null);
    const [, expectedReceiptBump] = await findRequestWithdrawVaultReceiptPda({
      vault: PARTNER_ROUTE.vault,
      userTransferAuthority: preparation.context.user.signer.address,
    }, { programAddress: PARTNER_ROUTE.programs.voltrVault });
    const escrow = decodeToken(post.get(preparation.context.userAccounts.requestWithdrawLpAta) ?? null, preparation.context.accounts.lpMint, preparation.context.userAccounts.requestWithdrawVaultReceipt);
    const events = parseTransactionEvents({ logMessages: finalized.logs }).filter((event) => event.name === "RequestWithdrawVaultEvent");
    const event = events.length === 1 ? events[0]!.payload : null;
    const tokenAtaRentLamports = await rentExemptionLamports(rpcUrl(), 165);
    const receiptRentLamports = await rentExemptionLamports(rpcUrl(), RECEIPT_DATA_LENGTH);
    const gates: Gate[] = [];
    gates.push(...verifyDeploymentIdentities(PARTNER_ROUTE, finalizedDeployments.identities).map((gate) => ({ ...gate, name: `finalized deployment: ${gate.name}` })));
    addRentGate(gates, "finalized escrow LP ATA has exact rent exemption", post.get(preparation.context.userAccounts.requestWithdrawLpAta) ?? null, tokenAtaRentLamports, true);
    addRentGate(gates, "finalized withdrawal receipt has exact rent exemption", post.get(preparation.context.userAccounts.requestWithdrawVaultReceipt) ?? null, receiptRentLamports, true);
    const finalizedFeeLamports = finalized.feeLamports ?? preparation.prepared.feeLamports;
    const createdEscrowAtaRentLamports = preparation.before.get(preparation.context.userAccounts.requestWithdrawLpAta) === null ? post.get(preparation.context.userAccounts.requestWithdrawLpAta)?.lamports ?? 0 : 0;
    const createdReceiptRentLamports = preparation.before.get(preparation.context.userAccounts.requestWithdrawVaultReceipt) === null ? post.get(preparation.context.userAccounts.requestWithdrawVaultReceipt)?.lamports ?? 0 : 0;
    const finalizedRequestLamportDelta = finalized.lamportDeltas.find(({ address: value }) => value === preparation.context.user.signer.address)?.deltaRaw ?? null;
    addExactSpendGate(gates, "confirmed withdraw request total SOL spend is quoted fee plus exact new ATA/receipt rent", preparation.before.get(preparation.context.user.signer.address) ?? null, post.get(preparation.context.user.signer.address) ?? null, finalizedFeeLamports, createdEscrowAtaRentLamports + createdReceiptRentLamports, MAX_WITHDRAW_REQUEST_TOTAL_LAMPORTS, true, finalizedRequestLamportDelta === null ? null : -BigInt(finalizedRequestLamportDelta));
    add(gates, "finalized receipt exact", receipt !== null && receipt.vault === PARTNER_ROUTE.vault && receipt.user === preparation.context.user.signer.address && receipt.amountLpEscrowed === amount && receipt.bump === expectedReceiptBump && receipt.version === 0, receipt, { vault: PARTNER_ROUTE.vault, user: preparation.context.user.signer.address, amountLpEscrowed: amount, bump: expectedReceiptBump, version: 0 });
    add(gates, "finalized escrow exact", escrow?.amount === amount, escrow?.amount ?? null, amount);
    const expectedTokenDeltas = [
      { address: preparation.context.userAccounts.userLpAta, mint: preparation.context.accounts.lpMint, deltaRaw: (-amount).toString() },
      { address: preparation.context.userAccounts.requestWithdrawLpAta, mint: preparation.context.accounts.lpMint, deltaRaw: amount.toString() },
    ];
    add(gates, "confirmed withdraw request token deltas are exact and closed", exactConfirmedTokenDeltas(finalized.tokenDeltas, expectedTokenDeltas), finalized.tokenDeltas, expectedTokenDeltas);
    const vaultBefore = currentVaultGate(preparation.context, preparation.before).state;
    const vaultAfter = currentVaultGate(preparation.context, post).state;
    add(gates, "confirmed withdrawal request leaves vault accounting unchanged", vaultBefore !== null && vaultAfter !== null && vaultAfter.totalValueRaw === vaultBefore.totalValueRaw && vaultAfter.lpSupplyRaw === vaultBefore.lpSupplyRaw && vaultAfter.idleRaw === vaultBefore.idleRaw, vaultBefore && vaultAfter ? { before: vaultBefore, after: vaultAfter } : null, "unchanged");
    add(gates, "confirmed request event amount, flags, receipt, and 600-second deadline exact", event !== null && event.requestWithdrawVaultReceipt === preparation.context.userAccounts.requestWithdrawVaultReceipt && event.requestedAmount === amount && event.isAmountInLp === true && event.isWithdrawAll === true && event.withdrawableFromTs === receipt?.withdrawableFromTs && event.withdrawableFromTs - event.requestedTs === PARTNER_ROUTE.vaultConfiguration.withdrawalWaitingPeriodSeconds, event, { amount, isAmountInLp: true, isWithdrawAll: true, waitingPeriodSeconds: PARTNER_ROUTE.vaultConfiguration.withdrawalWaitingPeriodSeconds });
    addFinalDeploymentGates(gates, preparation.deploymentsBefore, finalizedDeployments);
    const failedGateCount = gates.filter(({ pass }) => !pass).length;
    const allEvents = parseTransactionEvents({ logMessages: finalized.logs });
    const eventIndex = event === null ? -1 : allEvents.findIndex((candidate) => candidate.name === "RequestWithdrawVaultEvent");
    const receiptSnapshot = post.get(preparation.context.userAccounts.requestWithdrawVaultReceipt) ?? null;
    const originBase = event !== null && receipt !== null && receiptSnapshot !== null
      ? { signature: finalized.signature, eventIndex, receipt: preparation.context.userAccounts.requestWithdrawVaultReceipt, rawAccountSha256: sha256(receiptSnapshot.data) }
      : null;
    const requestOrigin = originBase ? { ...originBase, generationFingerprint: requestOriginFingerprint(originBase) } : null;
    const protectedAfter = await loadFourMarketProtectedState(rpcUrl(), state.contextSlot);
    const finalProtectedState = protectedStateEnvelope({ schemaVersion: 1, addressSetSha256: preSendProtected.addressSetSha256, contextSlot: preSendProtected.contextSlot, stateSha256: preSendProtected.stateSha256 }, protectedAfter);
    const protectedEvidence = protectedSnapshotEvidenceEnvelope(preSendProtected, protectedAfter);
    const settlementAttestation = await createProtectedSettlementAttestation(preparation.context.user.signer, {
      lifecycleId: preparation.intent.lifecycleId,
      operation: preparation.intent.operation,
      expectedSignature: preparation.prepared.expectedSignature,
      confirmedSignature: finalized.signature,
      messageSha256: sha256(preparation.prepared.serializedMessage),
      serializedTransactionSha256: sha256(preparation.prepared.serializedTransaction),
      intentSha256: preparation.intentSha256,
      addressSetSha256: preSendProtected.addressSetSha256,
      preAttestation: preSendAttestation,
      confirmedSlot: finalized.confirmedSlot,
      postContextSlot: protectedAfter.contextSlot,
      postStateSha256: protectedAfter.stateSha256,
    });
    return { verdict: failedGateCount === 0 ? "PARTNER_WITHDRAW_REQUEST_FINALIZED_AND_VERIFIED" : "PARTNER_WITHDRAW_REQUEST_FINALIZED_READBACK_FAIL", broadcast: true, intentPath, intentFileSha256, lifecycleId: preparation.intent.lifecycleId, protectedState: finalProtectedState, protectedSnapshotEvidence: protectedEvidence, preSendAttestation, settlementAttestation, requestOrigin, senderProof: senderProof(preparation.context.user.signer.address, finalized.signature, sha256(preparation.prepared.serializedMessage), sha256(preparation.prepared.serializedTransaction), finalized.confirmedSlot, finalized), persistenceContract: persistenceContract(intentPath, intentFileSha256, finalized.signature, sha256(preparation.prepared.serializedTransaction), sha256(preparation.prepared.serializedMessage), preparation.intentSha256, preparation.intent.lifecycleId, preparation.intent.protectedPrestateSha256), intent: preparation.intent, intentSha256: preparation.intentSha256, preflight: preparation.report, finalized, readbackContextSlot: state.contextSlot, readback: { failedGateCount, gates } } as const;
  } catch (error) {
    if (finalized) return { verdict: "PARTNER_WITHDRAW_REQUEST_FINALIZED_READBACK_ERROR", broadcast: true, intentPath, intentFileSha256, lifecycleId: preparation.intent.lifecycleId, protectedState: preparation.protectedState, senderProof: senderProof(preparation.context.user.signer.address, finalized.signature, sha256(preparation.prepared.serializedMessage), sha256(preparation.prepared.serializedTransaction), 0, finalized), persistenceContract: persistenceContract(intentPath, intentFileSha256, finalized.signature, sha256(preparation.prepared.serializedTransaction), sha256(preparation.prepared.serializedMessage), preparation.intentSha256, preparation.intent.lifecycleId, preparation.intent.protectedPrestateSha256), intent: preparation.intent, intentSha256: preparation.intentSha256, preflight: preparation.report, finalized, error: error instanceof Error ? error.message : String(error), recoveryInstruction: "Do not resend. The request is confirmed; rerun read-only receipt/escrow reconciliation." } as const;
    const failedSubmission = submissionEvidence(error, preparation.prepared);
    return { verdict: "PARTNER_WITHDRAW_REQUEST_BROADCAST_STATUS_UNKNOWN", broadcast: null, intentPath, intentFileSha256, lifecycleId: preparation.intent.lifecycleId, protectedState: preparation.protectedState, senderProof: senderProof(preparation.context.user.signer.address, preparation.prepared.expectedSignature, sha256(preparation.prepared.serializedMessage), sha256(preparation.prepared.serializedTransaction), 0, failedSubmission), persistenceContract: persistenceContract(intentPath, intentFileSha256, preparation.prepared.expectedSignature, sha256(preparation.prepared.serializedTransaction), sha256(preparation.prepared.serializedMessage), preparation.intentSha256, preparation.intent.lifecycleId, preparation.intent.protectedPrestateSha256), expectedSignature: preparation.prepared.expectedSignature, intent: preparation.intent, intentSha256: preparation.intentSha256, preflight: preparation.report, error: error instanceof Error ? error.message : String(error), recoveryInstruction: "Do not resend. Verify this exact signature and reload the request receipt and escrow ATA." } as const;
  }
}

function claimGates(context: RuntimeContext, before: Map<string, AccountSnapshot | null>, post: Map<string, AccountSnapshot | null>, simulationError: unknown, logs: readonly string[], premature: boolean, blockTime: number, receipt: NonNullable<ReturnType<typeof decodeReceipt>>, feeLamports: number): Gate[] {
  const gates: Gate[] = [];
  add(gates, "withdraw claim SOL fee bounded", feeLamports > 0 && feeLamports <= 100_000, feeLamports, "1..100000 lamports");
  const beforeVault = before.get(context.route.vault);
  const afterVault = post.get(context.route.vault);
  const beforeLp = before.get(context.accounts.lpMint);
  const afterLp = post.get(context.accounts.lpMint);
  const idleBefore = decodeToken(before.get(context.accounts.idleAta) ?? null, context.route.asset.mint, context.accounts.idleAuth);
  const idleAfter = decodeToken(post.get(context.accounts.idleAta) ?? null, context.route.asset.mint, context.accounts.idleAuth);
  const userBefore = decodeToken(before.get(context.userAccounts.userAssetAta) ?? null, context.route.asset.mint, context.user.signer.address);
  const userAfter = decodeToken(post.get(context.userAccounts.userAssetAta) ?? null, context.route.asset.mint, context.user.signer.address);
  const escrowBefore = decodeToken(before.get(context.userAccounts.requestWithdrawLpAta) ?? null, context.accounts.lpMint, context.userAccounts.requestWithdrawVaultReceipt);
  const escrowAfter = post.get(context.userAccounts.requestWithdrawLpAta) ? decodeToken(post.get(context.userAccounts.requestWithdrawLpAta)!, context.accounts.lpMint, context.userAccounts.requestWithdrawVaultReceipt) : null;
  // Voltr closes the receipt but deliberately retains the canonical escrow ATA
  // at zero balance for the next request. Only receipt rent is refunded.
  const refundRentLamports = before.get(context.userAccounts.requestWithdrawVaultReceipt)?.lamports ?? 0;
  addLamportDeltaGate(gates, premature ? "premature claim has no SOL mutation beyond possible simulated fee" : "claim SOL delta is fee net receipt rent refund", before.get(context.user.signer.address) ?? null, post.get(context.user.signer.address) ?? null, feeLamports, 0, premature ? 0 : refundRentLamports);
  add(gates, "claim bank time is before/after receipt deadline", premature ? BigInt(blockTime) < receipt.withdrawableFromTs : BigInt(blockTime) >= receipt.withdrawableFromTs, blockTime, premature ? `< ${receipt.withdrawableFromTs}` : `>= ${receipt.withdrawableFromTs}`);
  if (premature) {
    const custom6012 = typeof simulationError === "object" && simulationError !== null && JSON.stringify(simulationError).includes("6012");
    add(gates, "premature claim rejected with Custom 6012", custom6012, simulationError, { InstructionError: [0, { Custom: 6012 }] });
    add(gates, "premature claim logs identify WithdrawalNotYetAvailable", logs.some((line) => line.includes("WithdrawalNotYetAvailable") || line.includes("6012")), logs, "WithdrawalNotYetAvailable");
    const protectedAccounts = [...before.keys()].filter((account) => account !== context.route.asset.mint);
    const changedAccounts = protectedAccounts.filter((account) => !equalSnapshot(before.get(account) ?? null, post.get(account) ?? null));
    add(gates, "premature claim has no finalized protected-account mutation", changedAccounts.length === 0, changedAccounts, []);
    return gates;
  }
  const events = parseTransactionEvents({ logMessages: logs }).filter((event) => event.name === "WithdrawVaultEvent");
  const event = events.length === 1 ? events[0]!.payload : null;
  const quoteRaw = receipt.amountAssetToWithdrawDecimalBits >> U80F48_FRACTION_BITS;
  add(gates, "claim simulation succeeded", simulationError === null, simulationError, null);
  add(gates, "one exact withdraw event", event !== null, event, "WithdrawVaultEvent");
  add(gates, "withdraw event exact route and amount", event !== null && event.user === context.user.signer.address && event.vault === context.route.vault && event.vaultAssetMint === context.route.asset.mint && event.userAmountLpBurned === receipt.amountLpEscrowed && event.userAmountAssetWithdrawn === quoteRaw, event, { user: context.user.signer.address, vault: context.route.vault, amountLpBurned: receipt.amountLpEscrowed, amountAssetWithdrawn: quoteRaw });
  add(gates, "receipt closed", post.get(context.userAccounts.requestWithdrawVaultReceipt) === null, post.get(context.userAccounts.requestWithdrawVaultReceipt)?.address ?? null, null);
  add(gates, "escrow LP retained empty for reuse", escrowAfter?.amount === 0n, escrowAfter?.amount ?? null, 0n);
  add(gates, "user receives exact quoted USDC", userBefore !== null && userAfter !== null && userAfter.amount - userBefore.amount === quoteRaw, userBefore && userAfter ? userAfter.amount - userBefore.amount : null, quoteRaw);
  add(gates, "idle USDC pays exact quote", idleBefore !== null && idleAfter !== null && idleBefore.amount - idleAfter.amount === quoteRaw, idleBefore && idleAfter ? idleAfter.amount - idleBefore.amount : null, -quoteRaw);
  if (beforeVault && afterVault) {
    try {
      const vaultBefore = getVaultDecoder().decode(beforeVault.data);
      const vaultAfter = getVaultDecoder().decode(afterVault.data);
      add(gates, "vault total value decreases by quote", vaultBefore.asset.totalValue - vaultAfter.asset.totalValue === quoteRaw, vaultAfter.asset.totalValue - vaultBefore.asset.totalValue, -quoteRaw);
    } catch (error) {
      add(gates, "claim vault accounting decodes", false, error instanceof Error ? error.message : String(error), "decoded");
    }
  }
  if (beforeLp && afterLp) {
    try {
      const supplyBefore = getMintDecoder().decode(beforeLp.data).supply;
      const supplyAfter = getMintDecoder().decode(afterLp.data).supply;
      add(gates, "LP supply burns escrow amount", supplyBefore - supplyAfter === receipt.amountLpEscrowed, supplyAfter - supplyBefore, -receipt.amountLpEscrowed);
    } catch (error) {
      add(gates, "claim LP accounting decodes", false, error instanceof Error ? error.message : String(error), "decoded");
    }
  }
  add(gates, "unrelated user LP account unchanged", equalSnapshot(before.get(context.userAccounts.userLpAta) ?? null, post.get(context.userAccounts.userLpAta) ?? null), null, null);
  add(gates, "receipt prestate escrow burns to retained empty ATA", escrowBefore?.amount === receipt.amountLpEscrowed && escrowAfter?.amount === 0n, { before: escrowBefore?.amount ?? null, after: escrowAfter?.amount ?? null }, { before: receipt.amountLpEscrowed, after: 0n });
  return gates;
}

export type WithdrawClaimPreparation = Readonly<{ context: RuntimeContext; prepared: PreparedTransaction; intent: UserRuntimeIntent; intentSha256: string; report: Readonly<Record<string, unknown>>; inspectedAddresses: readonly string[]; before: Map<string, AccountSnapshot | null>; receipt: NonNullable<ReturnType<typeof decodeReceipt>>; deploymentsBefore: DeploymentSnapshot; protectedState: ProtectedState; protectedAddresses: readonly string[]; requestOrigin: RequestOrigin }>;

async function prepareWithdrawClaim(mode: "premature" | "post-deadline" = "post-deadline", requestSignature?: string, lifecycle?: UserLifecycleAuthorization): Promise<WithdrawClaimPreparation> {
  const context = await loadContext();
  const protectedAddresses = fourMarketProtectedAddresses();
  const inspectedAddresses = baseInspectedAddresses(context);
  const beforeResponse = await confirmedSnapshots(rpcUrl(), inspectedAddresses);
  const before = snapshotMap(inspectedAddresses, beforeResponse.accounts);
  const protectedBefore = await loadFourMarketProtectedState(rpcUrl(), beforeResponse.contextSlot);
  const deploymentsBefore = await loadDeploymentIdentities(rpcUrl(), context.route, beforeResponse.contextSlot, "confirmed");
  const receipt = decodeReceipt(before.get(context.userAccounts.requestWithdrawVaultReceipt) ?? null);
  if (!receipt) throw new Error("withdrawal claim requires the exact finalized 112-byte Voltr receipt");
  const [, expectedReceiptBump] = await findRequestWithdrawVaultReceiptPda({
    vault: context.route.vault,
    userTransferAuthority: context.user.signer.address,
  }, { programAddress: context.route.programs.voltrVault });
  if (receipt.vault !== context.route.vault || receipt.user !== context.user.signer.address || receipt.withdrawableFromTs <= 0n || receipt.bump !== expectedReceiptBump || receipt.version !== 0) throw new Error("withdrawal receipt is not the canonical route/user PDA and version");
  if (!requestSignature) throw new Error("withdrawal claim requires the finalized request signature");
  const requestOrigin = await confirmedTransaction(rpcUrl(), requestSignature);
  if (requestOrigin.slot > beforeResponse.contextSlot) {
    throw new Error(`withdrawal request origin slot ${requestOrigin.slot} postdates claim prestate ${beforeResponse.contextSlot}`);
  }
  await assertFinalizedWithdrawRequestPacket(requestOrigin, context.builder, context.user.signer.address, receipt.amountLpEscrowed);
  const requestEvents = parseTransactionEvents({ logMessages: requestOrigin.meta?.logMessages ?? [] }).filter((event) => event.name === "RequestWithdrawVaultEvent");
  const requestEvent = requestEvents.length === 1 ? requestEvents[0]!.payload : null;
  if (!requestEvent || requestEvent.vault !== context.route.vault || requestEvent.user !== context.user.signer.address || requestEvent.requestWithdrawVaultReceipt !== context.userAccounts.requestWithdrawVaultReceipt || requestEvent.amountLpEscrowed !== receipt.amountLpEscrowed || requestEvent.withdrawableFromTs !== receipt.withdrawableFromTs || requestEvent.withdrawableFromTs - requestEvent.requestedTs !== context.route.vaultConfiguration.withdrawalWaitingPeriodSeconds) throw new Error("withdrawal claim request origin is not an exact 600-second Voltr request for this route");
  const requestAllEvents = parseTransactionEvents({ logMessages: requestOrigin.meta?.logMessages ?? [] });
  const requestEventIndex = requestAllEvents.findIndex((event) => event.name === "RequestWithdrawVaultEvent");
  const requestReceiptSnapshot = before.get(context.userAccounts.requestWithdrawVaultReceipt) ?? null;
  if (requestEventIndex < 0 || requestReceiptSnapshot === null) throw new Error("withdrawal claim request origin has no exact receipt account image");
  const requestOriginBase = { signature: requestSignature, eventIndex: requestEventIndex, receipt: context.userAccounts.requestWithdrawVaultReceipt, rawAccountSha256: sha256(requestReceiptSnapshot.data) };
  const requestOriginProof: RequestOrigin = { ...requestOriginBase, generationFingerprint: requestOriginFingerprint(requestOriginBase) };
  const blockTime = await confirmedBlockTime(rpcUrl(), beforeResponse.contextSlot);
  if (mode === "post-deadline" && BigInt(blockTime) < receipt.withdrawableFromTs) throw new Error(`withdrawal claim is not yet available; deadline is ${receipt.withdrawableFromTs}`);
  if (mode === "premature" && BigInt(blockTime) >= receipt.withdrawableFromTs) throw new Error("premature claim mode requires a finalized bank time before the receipt deadline");
  const instruction = await context.builder.user.claimWithdraw(context.user.signer);
  const prepared = await prepareSignedV0Transaction({ rpcUrl: rpcUrl(), feePayer: context.user, instructions: [instruction.raw], prestateAddresses: protectedAddresses, inspectedAddresses, commitment: "confirmed" });
  // Failed simulations do not return usable requested account images. For the
  // premature-claim proof, reload finalized state at/after the simulation bank
  // and prove that the rejected packet committed no protected-account change.
  const prematureReadback = mode === "premature"
    ? await confirmedSnapshots(rpcUrl(), inspectedAddresses, prepared.simulationSlot)
    : null;
  const post = prematureReadback
    ? snapshotMap(inspectedAddresses, prematureReadback.accounts)
    : simulatedPost(inspectedAddresses, prepared);
  const deploymentsAfter = await loadDeploymentIdentities(rpcUrl(), context.route, prematureReadback?.contextSlot ?? prepared.simulationSlot, "confirmed");
  const protectedAfter = await loadFourMarketProtectedState(rpcUrl(), prematureReadback?.contextSlot ?? prepared.simulationSlot);
  if (lifecycle && (lifecycle.protectedPrestateSha256 !== protectedBefore.stateSha256 || lifecycle.protectedAddressSetSha256 !== protectedBefore.addressSetSha256)) throw new Error("withdrawal claim protected-state confirmation does not match the exact confirmed prestate");
  const protectedState = protectedStateEnvelope(protectedBefore, protectedAfter);
  const tokenAtaRentLamports = await rentExemptionLamports(rpcUrl(), 165);
  const receiptRentLamports = await rentExemptionLamports(rpcUrl(), RECEIPT_DATA_LENGTH);
  const simulationErrorCode = mode === "premature" ? simulationCustomErrorCode(prepared.simulation.err, prepared.simulation.logs) : null;
  const gates = claimGates(context, before, post, prepared.simulation.err, prepared.simulation.logs, mode === "premature", await confirmedBlockTime(rpcUrl(), prepared.simulationSlot), receipt, prepared.feeLamports);
  addRentGate(gates, "claim escrow LP rent prestate is exact", before.get(context.userAccounts.requestWithdrawLpAta) ?? null, tokenAtaRentLamports, true);
  addRentGate(gates, "claim receipt rent prestate is exact", before.get(context.userAccounts.requestWithdrawVaultReceipt) ?? null, receiptRentLamports, true);
  if (mode === "post-deadline") {
    addRentGate(gates, "claim escrow LP account retains exact rent for reuse", post.get(context.userAccounts.requestWithdrawLpAta) ?? null, tokenAtaRentLamports, true);
    addRentGate(gates, "claim receipt account closes", post.get(context.userAccounts.requestWithdrawVaultReceipt) ?? null, receiptRentLamports, false);
  }
  addDeploymentGates(gates, deploymentsBefore, deploymentsAfter);
  add(gates, "one canonical Voltr claim instruction", instruction.canonical.programId === context.route.programs.voltrVault && instruction.canonical.accounts.length === 13, instructionSummary(instruction), { programId: context.route.programs.voltrVault, accountCount: 13 });
  const protectedLifecycleId = lifecycle?.lifecycleId ?? sha256(Buffer.from(`simulation:withdraw-claim:${context.user.signer.address}:${protectedBefore.stateSha256}`, "utf8"));
  const { intent, intentSha256: digest } = makeIntent("withdraw-claim", context.user.signer.address, receipt.amountLpEscrowed, prepared, `withdraw-claim:${context.userAccounts.requestWithdrawVaultReceipt}:${mode}`, protectedLifecycleId, protectedBefore.stateSha256);
  const passVerdict = mode === "premature" ? "PARTNER_WITHDRAW_CLAIM_PREMATURE_REJECTION_PASS" : "PARTNER_WITHDRAW_CLAIM_SIMULATION_PASS";
  add(gates, "request origin is exact 600-second Voltr request", requestOrigin.slot > 0 && requestEvent !== null, { signature: requestSignature, slot: requestOrigin.slot, event: requestEvent }, "finalized request event bound to receipt");
  const baseReport = reportEnvelope(passVerdict, prepared, digest, gates, { operation: "withdraw-claim", mode, user: context.user.signer.address, vault: context.route.vault, receipt: context.userAccounts.requestWithdrawVaultReceipt, requestSignature, requestSlot: requestOrigin.slot, withdrawableFromTs: receipt.withdrawableFromTs.toString(), amountLpEscrowed: receipt.amountLpEscrowed.toString(), amountAssetToWithdrawDecimalBits: receipt.amountAssetToWithdrawDecimalBits.toString(), packetBytes: prepared.packetBytes, serializedPacketBase64: Buffer.from(prepared.serializedTransaction).toString("base64"), serializedPacketSha256: sha256(prepared.serializedTransaction), feeLamports: prepared.feeLamports, expectedSignature: prepared.expectedSignature, instruction: instruction.canonical, canonicalMessageSha256: sha256(prepared.serializedMessage) }, { prestateContextSlot: beforeResponse.contextSlot, protectedState, protectedSnapshotEvidence: protectedSnapshotEvidenceEnvelope(protectedBefore, protectedAfter), requestOrigin: requestOriginProof, bankBlockTime: blockTime, errorCode: simulationErrorCode, deployments: { before: deploymentsBefore.identities, after: deploymentsAfter.identities }, intent });
  const report = mode === "premature" ? { ...baseReport, readyForBroadcast: false } : baseReport;
  return { context, prepared, intent, intentSha256: digest, report, inspectedAddresses, before, receipt, deploymentsBefore, protectedState, protectedAddresses, requestOrigin: requestOriginProof };
}

export async function simulatePrematureWithdrawClaim(requestSignature?: string) {
  return (await prepareWithdrawClaim("premature", requestSignature)).report;
}

export async function simulatePostDeadlineWithdrawClaim(requestSignature?: string) {
  return (await prepareWithdrawClaim("post-deadline", requestSignature)).report;
}

/** Explicit, one-send path. Simulation commands never call this function. */
export async function executePostDeadlineWithdrawClaim(confirmReceipt: string | null, confirmDeadline: string | null, requestSignature: string | null, confirmUser: string | null, intentPathInput: string | null, confirmLifecycleId: string | null, confirmProtectedPrestateSha256: string | null, confirmProtectedAddressSetSha256: string | null, confirmRequestEventIndex: string | null, confirmRequestRawAccountSha256: string | null, confirmRequestGenerationFingerprint: string | null) {
  if (process.env.CONFIRM_MAINNET !== "1") throw new Error("execute withdraw-claim requires CONFIRM_MAINNET=1");
  const intentPath = requireRuntimeIntentPath(intentPathInput, "withdraw-claim");
  const authorization = await authorizeWithdrawClaimBeforeSigner(confirmReceipt, confirmDeadline, requestSignature, confirmUser);
  const lifecycle: UserLifecycleAuthorization = {
    lifecycleId: authorizedSha256(confirmLifecycleId, "execute withdraw-claim --confirm-lifecycle-id"),
    protectedPrestateSha256: authorizedSha256(confirmProtectedPrestateSha256, "execute withdraw-claim --confirm-protected-prestate-sha256"),
    protectedAddressSetSha256: authorizedSha256(confirmProtectedAddressSetSha256, "execute withdraw-claim --confirm-protected-address-set-sha256"),
  };
  const confirmedRequestOrigin: RequestOrigin = {
    signature: authorization.requestSignature,
    eventIndex: authorizedEventIndex(confirmRequestEventIndex, "execute withdraw-claim --confirm-request-event-index"),
    receipt: authorization.receipt,
    rawAccountSha256: authorizedSha256(confirmRequestRawAccountSha256, "execute withdraw-claim --confirm-request-raw-account-sha256"),
    generationFingerprint: authorizedSha256(confirmRequestGenerationFingerprint, "execute withdraw-claim --confirm-request-generation-fingerprint"),
  };
  const preparation = await prepareWithdrawClaim("post-deadline", authorization.requestSignature, lifecycle);
  if (JSON.stringify(preparation.requestOrigin) !== JSON.stringify(confirmedRequestOrigin)) throw new Error("withdrawal claim request-origin confirmation does not match the exact confirmed request generation");
  const receiptAddress = preparation.context.userAccounts.requestWithdrawVaultReceipt;
  if (preparation.context.user.signer.address !== authorization.user || receiptAddress !== authorization.receipt || preparation.receipt.withdrawableFromTs !== authorization.deadline) throw new Error("withdrawal claim signer, receipt, or deadline changed after authorization");
  if (preparation.report.readyForBroadcast !== true || preparation.report.failedGateCount !== 0) throw new Error("withdrawal claim preflight failed; refusing broadcast");
  const refreshed = await confirmedSnapshots(rpcUrl(), preparation.inspectedAddresses, preparation.prepared.simulationSlot);
  const refreshedMap = snapshotMap(preparation.inspectedAddresses, refreshed.accounts);
  const changedAccounts = [...preparation.before.keys()].filter((account) => account !== preparation.context.route.asset.mint && !equalSnapshot(refreshedMap.get(account) ?? null, preparation.before.get(account) ?? null));
  if (changedAccounts.length > 0) throw new Error(`withdrawal claim protected state changed after simulation (${changedAccounts.join(", ")}); refusing broadcast`);
  if (!currentVaultGate(preparation.context, refreshedMap).gates.every(({ pass }) => pass)) throw new Error("withdrawal claim refreshed vault or asset-mint semantics changed; refusing broadcast");
  const refreshedDeployments = await loadDeploymentIdentities(rpcUrl(), PARTNER_ROUTE, refreshed.contextSlot, "confirmed");
  if (!verifyDeploymentIdentities(PARTNER_ROUTE, refreshedDeployments.identities).every(({ pass }) => pass)) throw new Error("withdrawal claim approved deployment identity changed; refusing broadcast");
  const preSendProtected = await refreshUserProtectedPreSend(
    preparation,
    Math.max(preparation.prepared.simulationSlot, refreshed.contextSlot, refreshedDeployments.contextSlot),
  );
  const preSendAttestation = await createProtectedPreSendAttestation(preparation.context.user.signer, {
    lifecycleId: preparation.intent.lifecycleId,
    operation: preparation.intent.operation,
    expectedSignature: preparation.prepared.expectedSignature,
    messageSha256: sha256(preparation.prepared.serializedMessage),
    intentSha256: preparation.intentSha256,
    addressSetSha256: preSendProtected.addressSetSha256,
    preContextSlot: preSendProtected.contextSlot,
    preStateSha256: preSendProtected.stateSha256,
  });
  const authorizationContextSlot = Math.max(
    preparation.prepared.simulationSlot,
    refreshed.contextSlot,
    refreshedDeployments.contextSlot,
    preSendProtected.contextSlot,
  );
  const preSendPersistence = persistenceContract("", "", preparation.prepared.expectedSignature, sha256(preparation.prepared.serializedTransaction), sha256(preparation.prepared.serializedMessage), preparation.intentSha256, preparation.intent.lifecycleId, preparation.intent.protectedPrestateSha256);
  const serializedTransactionBase64 = assertPreparedWire(preparation.prepared);
  const intentFileSha256 = persistRuntimeIntent(intentPath, {
    schemaVersion: 1,
    kind: "backyard-voltr-user-runtime-intent",
    operation: "withdraw-claim",
    routeId: PARTNER_FOUR_MARKET_ROUTE.id,
    routeSpecSha256: fourMarketRouteSpecSha256(),
    user: preparation.context.user.signer.address,
    vault: PARTNER_ROUTE.vault,
    receipt: preparation.context.userAccounts.requestWithdrawVaultReceipt,
    deadline: preparation.receipt.withdrawableFromTs,
    amountLpRaw: preparation.receipt.amountLpEscrowed,
    requestSignature: authorization.requestSignature,
    requestOrigin: preparation.requestOrigin,
    expectedSignature: preparation.prepared.expectedSignature,
    serializedTransactionSha256: sha256(preparation.prepared.serializedTransaction),
    serializedTransactionBase64,
    serializedMessageSha256: sha256(preparation.prepared.serializedMessage),
    packetBytes: preparation.prepared.packetBytes,
    authorizationContextSlot,
    feeLamports: preparation.prepared.feeLamports,
    receiptRentRefundLamports: preparation.before.get(preparation.context.userAccounts.requestWithdrawVaultReceipt)?.lamports ?? 0,
    maxTotalLamports: 100_000,
    withdrawalWaitingPeriodSeconds: PARTNER_ROUTE.vaultConfiguration.withdrawalWaitingPeriodSeconds,
    persistenceContract: preSendPersistence,
    protectedSnapshotEvidence: { before: preSendProtected },
    protectedPrestateEvidence: preSendProtected,
    preSendAttestation,
    intent: preparation.intent,
  });
  verifyPersistedRuntimeIntent(intentPath, intentFileSha256, { ...preparation, protectedPreSend: preSendProtected, preSendAttestation }, preSendPersistence, authorizationContextSlot);
  let finalized: Awaited<ReturnType<typeof sendPreparedConfirmedOnce>> | null = null;
  try {
    finalized = await sendPreparedConfirmedOnce(rpcUrl(), preparation.prepared, authorizationContextSlot);
    if (finalized.err !== null) return { verdict: "PARTNER_WITHDRAW_CLAIM_FINALIZED_WITH_ERROR", broadcast: true, intentPath, intentFileSha256, preflight: preparation.report, finalized } as const;
    const state = await confirmedSnapshots(rpcUrl(), preparation.inspectedAddresses, finalized.confirmedSlot);
    const finalizedDeployments = await loadDeploymentIdentities(rpcUrl(), PARTNER_ROUTE, state.contextSlot, "confirmed");
    const readback = snapshotMap(preparation.inspectedAddresses, state.accounts);
    const gates = claimGates(preparation.context, preparation.before, readback, finalized.err, finalized.logs, false, await confirmedBlockTime(rpcUrl(), finalized.confirmedSlot), preparation.receipt, finalized.feeLamports ?? preparation.prepared.feeLamports);
    const quoteRaw = preparation.receipt.amountAssetToWithdrawDecimalBits >> U80F48_FRACTION_BITS;
    const expectedTokenDeltas = [
      { address: preparation.context.userAccounts.requestWithdrawLpAta, mint: preparation.context.accounts.lpMint, deltaRaw: (-preparation.receipt.amountLpEscrowed).toString() },
      { address: preparation.context.userAccounts.userAssetAta, mint: preparation.context.route.asset.mint, deltaRaw: quoteRaw.toString() },
      { address: preparation.context.accounts.idleAta, mint: preparation.context.route.asset.mint, deltaRaw: (-quoteRaw).toString() },
    ];
    add(gates, "confirmed claim token deltas are exact and closed", exactConfirmedTokenDeltas(finalized.tokenDeltas, expectedTokenDeltas), finalized.tokenDeltas, expectedTokenDeltas);
    const claimPayerDelta = finalized.lamportDeltas.find(({ address: value }) => value === preparation.context.user.signer.address)?.deltaRaw ?? null;
    const receiptRentRefund = preparation.before.get(preparation.context.userAccounts.requestWithdrawVaultReceipt)?.lamports ?? 0;
    addExactSpendGate(gates, "confirmed claim SOL debit is exact fee net receipt rent refund", preparation.before.get(preparation.context.user.signer.address) ?? null, readback.get(preparation.context.user.signer.address) ?? null, finalized.feeLamports ?? preparation.prepared.feeLamports, 0, 100_000, true, claimPayerDelta === null ? null : -BigInt(claimPayerDelta), receiptRentRefund);
    addFinalDeploymentGates(gates, preparation.deploymentsBefore, finalizedDeployments);
    addRentGate(gates, "finalized claim escrow LP account retains exact rent for reuse", readback.get(preparation.context.userAccounts.requestWithdrawLpAta) ?? null, await rentExemptionLamports(rpcUrl(), 165), true);
    addRentGate(gates, "finalized claim receipt account closes", readback.get(preparation.context.userAccounts.requestWithdrawVaultReceipt) ?? null, await rentExemptionLamports(rpcUrl(), RECEIPT_DATA_LENGTH), false);
    gates.push(...verifyDeploymentIdentities(PARTNER_ROUTE, finalizedDeployments.identities).map((gate) => ({ ...gate, name: `finalized deployment: ${gate.name}` })));
    const failedGateCount = gates.filter(({ pass }) => !pass).length;
    const protectedAfter = await loadFourMarketProtectedState(rpcUrl(), state.contextSlot);
    const finalProtectedState = protectedStateEnvelope({ schemaVersion: 1, addressSetSha256: preSendProtected.addressSetSha256, contextSlot: preSendProtected.contextSlot, stateSha256: preSendProtected.stateSha256 }, protectedAfter);
    const protectedEvidence = protectedSnapshotEvidenceEnvelope(preSendProtected, protectedAfter);
    const settlementAttestation = await createProtectedSettlementAttestation(preparation.context.user.signer, {
      lifecycleId: preparation.intent.lifecycleId,
      operation: preparation.intent.operation,
      expectedSignature: preparation.prepared.expectedSignature,
      confirmedSignature: finalized.signature,
      messageSha256: sha256(preparation.prepared.serializedMessage),
      serializedTransactionSha256: sha256(preparation.prepared.serializedTransaction),
      intentSha256: preparation.intentSha256,
      addressSetSha256: preSendProtected.addressSetSha256,
      preAttestation: preSendAttestation,
      confirmedSlot: finalized.confirmedSlot,
      postContextSlot: protectedAfter.contextSlot,
      postStateSha256: protectedAfter.stateSha256,
    });
    return { verdict: failedGateCount === 0 ? "PARTNER_WITHDRAW_CLAIM_FINALIZED_AND_VERIFIED" : "PARTNER_WITHDRAW_CLAIM_FINALIZED_READBACK_FAIL", broadcast: true, intentPath, intentFileSha256, lifecycleId: preparation.intent.lifecycleId, protectedState: finalProtectedState, protectedSnapshotEvidence: protectedEvidence, preSendAttestation, settlementAttestation, requestOrigin: preparation.requestOrigin, senderProof: senderProof(preparation.context.user.signer.address, finalized.signature, sha256(preparation.prepared.serializedMessage), sha256(preparation.prepared.serializedTransaction), finalized.confirmedSlot, finalized), persistenceContract: persistenceContract(intentPath, intentFileSha256, finalized.signature, sha256(preparation.prepared.serializedTransaction), sha256(preparation.prepared.serializedMessage), preparation.intentSha256, preparation.intent.lifecycleId, preparation.intent.protectedPrestateSha256), intent: preparation.intent, intentSha256: preparation.intentSha256, preflight: preparation.report, finalized, readbackContextSlot: state.contextSlot, readback: { failedGateCount, gates } } as const;
  } catch (error) {
    if (finalized) return { verdict: "PARTNER_WITHDRAW_CLAIM_FINALIZED_READBACK_ERROR", broadcast: true, intentPath, intentFileSha256, lifecycleId: preparation.intent.lifecycleId, protectedState: preparation.protectedState, requestOrigin: preparation.requestOrigin, senderProof: senderProof(preparation.context.user.signer.address, finalized.signature, sha256(preparation.prepared.serializedMessage), sha256(preparation.prepared.serializedTransaction), 0, finalized), persistenceContract: persistenceContract(intentPath, intentFileSha256, finalized.signature, sha256(preparation.prepared.serializedTransaction), sha256(preparation.prepared.serializedMessage), preparation.intentSha256, preparation.intent.lifecycleId, preparation.intent.protectedPrestateSha256), intent: preparation.intent, intentSha256: preparation.intentSha256, preflight: preparation.report, finalized, error: error instanceof Error ? error.message : String(error), recoveryInstruction: "Do not resend. The claim is confirmed; rerun read-only receipt/vault/user reconciliation." } as const;
    const failedSubmission = submissionEvidence(error, preparation.prepared);
    return { verdict: "PARTNER_WITHDRAW_CLAIM_BROADCAST_STATUS_UNKNOWN", broadcast: null, intentPath, intentFileSha256, lifecycleId: preparation.intent.lifecycleId, protectedState: preparation.protectedState, requestOrigin: preparation.requestOrigin, senderProof: senderProof(preparation.context.user.signer.address, preparation.prepared.expectedSignature, sha256(preparation.prepared.serializedMessage), sha256(preparation.prepared.serializedTransaction), 0, failedSubmission), persistenceContract: persistenceContract(intentPath, intentFileSha256, preparation.prepared.expectedSignature, sha256(preparation.prepared.serializedTransaction), sha256(preparation.prepared.serializedMessage), preparation.intentSha256, preparation.intent.lifecycleId, preparation.intent.protectedPrestateSha256), expectedSignature: preparation.prepared.expectedSignature, intent: preparation.intent, intentSha256: preparation.intentSha256, preflight: preparation.report, error: error instanceof Error ? error.message : String(error), recoveryInstruction: "Do not resend. Verify this exact signature and reload the receipt, escrow, idle ATA, user ATA, and LP mint." } as const;
  }
}
