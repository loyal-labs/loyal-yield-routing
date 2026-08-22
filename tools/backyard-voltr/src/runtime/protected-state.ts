import { createHash, webcrypto } from "node:crypto";

import { createSignableMessage, type KeyPairSigner, type SignatureBytes } from "@solana/kit";
import { PublicKey } from "@solana/web3.js";
import { verifySignature } from "@solana/keys";

import {
  PARTNER_FOUR_MARKET_ROUTE,
  PARTNER_FOUR_MARKET_STRATEGIES,
  PARTNER_ROUTE,
} from "../domain/route-spec.js";
import {
  confirmedSnapshots,
  type AccountSnapshot,
} from "../integrations/solana-compat.js";

const POLICY_SEEDS = [17n, 18n, 19n, 20n, 21n, 22n, 23n, 24n] as const;

export type ProtectedStateFingerprint = Readonly<{
  schemaVersion: 1 | 2;
  addressSetSha256: string;
  contextSlot: number;
  stateSha256: string;
  /**
   * The exact account images are optional on the legacy fingerprint surface.
   * New runtime evidence always includes them; keeping this optional lets the
   * old protectedState envelope remain byte-compatible while the richer
   * evidence is introduced alongside it.
   */
  rows?: readonly ProtectedSnapshotRow[];
}>;

export type ProtectedSnapshotRow = Readonly<
  | {
      address: string;
      exists: false;
    }
  | {
      address: string;
      exists: true;
      owner: string;
      lamports: string;
      executable: boolean;
      dataBase64: string;
      dataSha256: string;
    }
>;

/** Exact, replayable account evidence for one ordered protected snapshot. */
export type ProtectedSnapshotEvidence = Readonly<{
  schemaVersion: 1;
  addressSetSha256: string;
  contextSlot: number;
  stateSha256: string;
  rows: readonly ProtectedSnapshotRow[];
}>;

export type ProtectedSnapshotEvidenceEnvelope = Readonly<{
  schemaVersion: 1;
  addressSetSha256: string;
  before: ProtectedSnapshotEvidence;
  after: ProtectedSnapshotEvidence;
}>;

export type ProtectedPreSendAttestationPayload = Readonly<{
  schemaVersion: 1;
  domain: "backyard-voltr-protected-pre-send-v1";
  lifecycleId: string;
  operation: string;
  expectedSignature: string;
  messageSha256: string;
  intentSha256: string;
  addressSetSha256: string;
  preContextSlot: number;
  preStateSha256: string;
}>;

export type ProtectedSettlementAttestationPayload = Readonly<{
  schemaVersion: 1;
  domain: "backyard-voltr-protected-settlement-v1";
  lifecycleId: string;
  operation: string;
  expectedSignature: string;
  confirmedSignature: string;
  messageSha256: string;
  serializedTransactionSha256: string;
  intentSha256: string;
  addressSetSha256: string;
  preAttestationSha256: string;
  preSignatureSha256: string;
  confirmedSlot: number;
  postContextSlot: number;
  postStateSha256: string;
}>;

export type ProtectedPreSendAttestation = Readonly<{
  schemaVersion: 1;
  kind: "pre-send";
  signer: string;
  payload: ProtectedPreSendAttestationPayload;
  payloadSha256: string;
  signatureBase64: string;
  signatureSha256: string;
  attestationSha256: string;
}>;

export type ProtectedSettlementAttestation = Readonly<{
  schemaVersion: 1;
  kind: "confirmed-settlement";
  signer: string;
  payload: ProtectedSettlementAttestationPayload;
  payloadSha256: string;
  signatureBase64: string;
  signatureSha256: string;
  attestationSha256: string;
}>;

export type ProtectedAttestation = ProtectedPreSendAttestation | ProtectedSettlementAttestation;

function sha256(value: Uint8Array | string): string {
  return createHash("sha256").update(value).digest("hex");
}

function canonicalJson(value: unknown): string {
  if (typeof value === "bigint") return JSON.stringify(value.toString());
  if (Array.isArray(value)) return `[${value.map(canonicalJson).join(",")}]`;
  if (value && typeof value === "object") {
    return `{${Object.entries(value)
      .sort(([left], [right]) => left.localeCompare(right))
      .map(([key, entry]) => `${JSON.stringify(key)}:${canonicalJson(entry)}`)
      .join(",")}}`;
  }
  return JSON.stringify(value);
}

/** Canonical bytes used for snapshot and attestation replay. */
export function canonicalProtectedEvidenceJson(value: unknown): string {
  return canonicalJson(value);
}

export function recomputeProtectedAttestationPayloadSha256(payload: unknown): string {
  return sha256(Buffer.from(canonicalJson(payload), "utf8"));
}

export function recomputeProtectedAttestationSha256(
  kind: "pre-send" | "confirmed-settlement",
  payloadSha256: string,
  signatureSha256: string,
): string {
  return sha256(Buffer.from(canonicalJson({ kind, payloadSha256, signatureSha256 }), "utf8"));
}

function canonicalShaField(value: unknown, label: string): string {
  if (typeof value !== "string" || !/^[0-9a-f]{64}$/.test(value)) {
    throw new Error(`${label} must be a lowercase SHA-256 digest`);
  }
  return value;
}

function canonicalAddress(value: unknown, label: string): string {
  if (typeof value !== "string") throw new Error(`${label} must be a Solana address`);
  try {
    return new PublicKey(value).toBase58();
  } catch (error) {
    throw new Error(`${label} must be a valid Solana address`, { cause: error });
  }
}

function canonicalContextSlot(value: unknown, label: string): number {
  if (typeof value !== "number" || !Number.isSafeInteger(value) || value <= 0) {
    throw new Error(`${label} must be a positive safe integer`);
  }
  return value;
}

function canonicalLamports(value: unknown, label: string): string {
  if (typeof value !== "string" || !/^(?:0|[1-9]\d*)$/.test(value)) {
    throw new Error(`${label} must be a canonical non-negative decimal string`);
  }
  return value;
}

function canonicalBase64(value: unknown, label: string): string {
  if (typeof value !== "string") throw new Error(`${label} must be canonical base64`);
  const bytes = Buffer.from(value, "base64");
  if (bytes.toString("base64") !== value) throw new Error(`${label} must be canonical base64`);
  return value;
}

function snapshotRowFromAccount(address: string, account: AccountSnapshot | null): ProtectedSnapshotRow {
  if (account === null) return { address, exists: false };
  if (account.address !== address) throw new Error(`protected snapshot account address mismatch for ${address}`);
  if (!Number.isSafeInteger(account.lamports) || account.lamports < 0) throw new Error(`protected snapshot lamports are not a non-negative safe integer for ${address}`);
  const data = Buffer.from(account.data);
  return {
    address,
    exists: true,
    owner: canonicalAddress(account.owner, `${address}.owner`),
    lamports: canonicalLamports(String(account.lamports), `${address}.lamports`),
    executable: account.executable,
    dataBase64: data.toString("base64"),
    dataSha256: sha256(data),
  };
}

function snapshotRowsStateSha256(rows: readonly ProtectedSnapshotRow[]): string {
  return sha256(canonicalJson(rows));
}

/** Pure state-hash recomputation from the exact ordered snapshot rows. */
export function recomputeProtectedSnapshotStateSha256(
  rows: readonly ProtectedSnapshotRow[],
): string {
  return snapshotRowsStateSha256(rows);
}

/**
 * Build exact ordered evidence from RPC account images. This function does
 * not read the network and is therefore suitable for independent replay.
 */
export function buildProtectedSnapshotEvidence(
  addresses: readonly string[],
  accounts: readonly (AccountSnapshot | null)[],
  contextSlot: number,
): ProtectedSnapshotEvidence {
  const expected = fourMarketProtectedAddresses();
  if (canonicalJson(addresses) !== canonicalJson(expected)) {
    throw new Error("protected snapshot addresses are not the exact four-market set");
  }
  if (accounts.length !== expected.length) throw new Error("protected snapshot account count is not exact");
  const rows = accounts.map((account, index) => snapshotRowFromAccount(expected[index]!, account));
  const evidence: ProtectedSnapshotEvidence = {
    schemaVersion: 1,
    addressSetSha256: fourMarketProtectedAddressSetSha256(),
    contextSlot: canonicalContextSlot(contextSlot, "protected snapshot context slot"),
    stateSha256: snapshotRowsStateSha256(rows),
    rows,
  };
  assertProtectedSnapshotEvidence(evidence);
  return evidence;
}

/** Fail-closed structural and cryptographic validation of persisted evidence. */
export function assertProtectedSnapshotEvidence(
  value: unknown,
  expectedAddresses: readonly string[] = fourMarketProtectedAddresses(),
): asserts value is ProtectedSnapshotEvidence {
  if (!value || typeof value !== "object" || Array.isArray(value)) throw new Error("protected snapshot evidence must be an object");
  const candidate = value as Record<string, unknown>;
  const keys = Object.keys(candidate).sort();
  if (canonicalJson(keys) !== canonicalJson(["addressSetSha256", "contextSlot", "rows", "schemaVersion", "stateSha256"])) {
    throw new Error("protected snapshot evidence keys are not exact");
  }
  if (candidate.schemaVersion !== 1) throw new Error("protected snapshot evidence schemaVersion must be 1");
  if (candidate.addressSetSha256 !== sha256(canonicalJson(expectedAddresses))) throw new Error("protected snapshot evidence address-set hash is not exact");
  const contextSlot = canonicalContextSlot(candidate.contextSlot, "protected snapshot evidence contextSlot");
  if (!Array.isArray(candidate.rows) || candidate.rows.length !== expectedAddresses.length) throw new Error("protected snapshot evidence rows are not exact length");
  const rows: ProtectedSnapshotRow[] = candidate.rows.map((raw, index) => {
    if (!raw || typeof raw !== "object" || Array.isArray(raw)) throw new Error(`protected snapshot row ${index} must be an object`);
    const row = raw as Record<string, unknown>;
    const expectedAddress = expectedAddresses[index]!;
    if (row.address !== expectedAddress) throw new Error(`protected snapshot row ${index} address/order mismatch`);
    if (row.exists === false) {
      if (Object.keys(row).sort().join(",") !== "address,exists") throw new Error(`absent protected snapshot row ${index} has extra fields`);
      return { address: expectedAddress, exists: false };
    }
    if (row.exists !== true || Object.keys(row).sort().join(",") !== "address,dataBase64,dataSha256,executable,exists,lamports,owner") {
      throw new Error(`existing protected snapshot row ${index} keys are not exact`);
    }
    const owner = canonicalAddress(row.owner, `protected snapshot row ${index}.owner`);
    const lamports = canonicalLamports(row.lamports, `protected snapshot row ${index}.lamports`);
    if (typeof row.executable !== "boolean") throw new Error(`protected snapshot row ${index}.executable must be boolean`);
    const dataBase64 = canonicalBase64(row.dataBase64, `protected snapshot row ${index}.dataBase64`);
    const dataSha256 = canonicalShaField(row.dataSha256, `protected snapshot row ${index}.dataSha256`);
    if (sha256(Buffer.from(dataBase64, "base64")) !== dataSha256) throw new Error(`protected snapshot row ${index}.dataSha256 does not match dataBase64`);
    return { address: expectedAddress, exists: true, owner, lamports, executable: row.executable, dataBase64, dataSha256 };
  });
  if (candidate.stateSha256 !== snapshotRowsStateSha256(rows)) throw new Error("protected snapshot evidence state hash does not match rows");
  // Keep the explicit accesses above as part of validation: a malformed
  // context slot must never be accepted merely because the row hash matches.
  void contextSlot;
}

export function validateProtectedSnapshotEvidence(value: unknown, expectedAddresses?: readonly string[]): boolean {
  try {
    assertProtectedSnapshotEvidence(value, expectedAddresses);
    return true;
  } catch {
    return false;
  }
}

export function protectedSnapshotEvidenceEnvelope(
  before: ProtectedSnapshotEvidence,
  after: ProtectedSnapshotEvidence,
): ProtectedSnapshotEvidenceEnvelope {
  assertProtectedSnapshotEvidence(before);
  assertProtectedSnapshotEvidence(after);
  if (before.addressSetSha256 !== after.addressSetSha256) throw new Error("protected evidence address set changed across transaction");
  if (after.contextSlot < before.contextSlot) throw new Error("protected evidence post context precedes pre context");
  return { schemaVersion: 1, addressSetSha256: before.addressSetSha256, before, after };
}

function signatureEnvelope(
  kind: "pre-send" | "confirmed-settlement",
  signer: string,
  payload: ProtectedPreSendAttestationPayload | ProtectedSettlementAttestationPayload,
  signature: Uint8Array,
): ProtectedPreSendAttestation | ProtectedSettlementAttestation {
  if (signature.length !== 64) throw new Error("protected attestation signature must be exactly 64 bytes");
  const payloadBytes = Buffer.from(canonicalJson(payload), "utf8");
  const payloadSha256 = sha256(payloadBytes);
  const signatureBase64 = Buffer.from(signature).toString("base64");
  const signatureSha256 = sha256(signature);
  const attestationSha256 = sha256(Buffer.from(canonicalJson({ kind, payloadSha256, signatureSha256 }), "utf8"));
  return {
    schemaVersion: 1,
    kind,
    signer: canonicalAddress(signer, "protected attestation signer"),
    payload,
    payloadSha256,
    signatureBase64,
    signatureSha256,
    attestationSha256,
  } as ProtectedPreSendAttestation | ProtectedSettlementAttestation;
}

function validateAttestationInput(
  lifecycleId: string,
  operation: string,
  expectedSignature: string,
  messageSha256: string,
  intentSha256: string,
  addressSetSha256: string,
): void {
  canonicalShaField(lifecycleId, "protected attestation lifecycleId");
  if (!operation) throw new Error("protected attestation operation is required");
  if (!expectedSignature) throw new Error("protected attestation expected signature is required");
  canonicalShaField(messageSha256, "protected attestation messageSha256");
  canonicalShaField(intentSha256, "protected attestation intentSha256");
  canonicalShaField(addressSetSha256, "protected attestation addressSetSha256");
}

export async function createProtectedPreSendAttestation(
  signer: KeyPairSigner,
  input: Readonly<{
    lifecycleId: string;
    operation: string;
    expectedSignature: string;
    messageSha256: string;
    intentSha256: string;
    addressSetSha256: string;
    preContextSlot: number;
    preStateSha256: string;
  }>,
): Promise<ProtectedPreSendAttestation> {
  validateAttestationInput(input.lifecycleId, input.operation, input.expectedSignature, input.messageSha256, input.intentSha256, input.addressSetSha256);
  const payload: ProtectedPreSendAttestationPayload = {
    schemaVersion: 1,
    domain: "backyard-voltr-protected-pre-send-v1",
    lifecycleId: input.lifecycleId,
    operation: input.operation,
    expectedSignature: input.expectedSignature,
    messageSha256: canonicalShaField(input.messageSha256, "protected pre-send messageSha256"),
    intentSha256: canonicalShaField(input.intentSha256, "protected pre-send intentSha256"),
    addressSetSha256: canonicalShaField(input.addressSetSha256, "protected pre-send addressSetSha256"),
    preContextSlot: canonicalContextSlot(input.preContextSlot, "protected pre-send preContextSlot"),
    preStateSha256: canonicalShaField(input.preStateSha256, "protected pre-send preStateSha256"),
  };
  const message = createSignableMessage(Buffer.from(canonicalJson(payload), "utf8"));
  let dictionaries: Awaited<ReturnType<KeyPairSigner["signMessages"]>>;
  try {
    dictionaries = await signer.signMessages([message]);
  } catch (error) {
    throw new Error("protected pre-send attestation signing failed", { cause: error });
  }
  const signature = dictionaries[0]?.[signer.address];
  if (!(signature instanceof Uint8Array)) throw new Error("protected pre-send attestation signer returned no signature");
  if (signer.keyPair?.publicKey && !(await verifySignature(signer.keyPair.publicKey, signature, message.content))) {
    throw new Error("protected pre-send attestation signature failed local verification");
  }
  return signatureEnvelope("pre-send", signer.address, payload, signature) as ProtectedPreSendAttestation;
}

export async function createProtectedSettlementAttestation(
  signer: KeyPairSigner,
  input: Readonly<{
    lifecycleId: string;
    operation: string;
    expectedSignature: string;
    confirmedSignature: string;
    messageSha256: string;
    serializedTransactionSha256: string;
    intentSha256: string;
    addressSetSha256: string;
    preAttestation: ProtectedPreSendAttestation;
    confirmedSlot: number;
    postContextSlot: number;
    postStateSha256: string;
  }>,
): Promise<ProtectedSettlementAttestation> {
  assertProtectedPreSendAttestation(input.preAttestation);
  validateAttestationInput(input.lifecycleId, input.operation, input.expectedSignature, input.messageSha256, input.intentSha256, input.addressSetSha256);
  if (input.confirmedSignature !== input.expectedSignature) throw new Error("confirmed settlement signature differs from expected transaction signature");
  if (input.preAttestation.payload.lifecycleId !== input.lifecycleId || input.preAttestation.payload.operation !== input.operation || input.preAttestation.payload.expectedSignature !== input.expectedSignature || input.preAttestation.payload.messageSha256 !== input.messageSha256 || input.preAttestation.payload.intentSha256 !== input.intentSha256 || input.preAttestation.payload.addressSetSha256 !== input.addressSetSha256) throw new Error("confirmed settlement does not bind the exact pre-send attestation");
  const payload: ProtectedSettlementAttestationPayload = {
    schemaVersion: 1,
    domain: "backyard-voltr-protected-settlement-v1",
    lifecycleId: input.lifecycleId,
    operation: input.operation,
    expectedSignature: input.expectedSignature,
    confirmedSignature: input.confirmedSignature,
    messageSha256: canonicalShaField(input.messageSha256, "protected settlement messageSha256"),
    serializedTransactionSha256: canonicalShaField(input.serializedTransactionSha256, "protected settlement serializedTransactionSha256"),
    intentSha256: canonicalShaField(input.intentSha256, "protected settlement intentSha256"),
    addressSetSha256: canonicalShaField(input.addressSetSha256, "protected settlement addressSetSha256"),
    preAttestationSha256: input.preAttestation.attestationSha256,
    preSignatureSha256: input.preAttestation.signatureSha256,
    confirmedSlot: canonicalContextSlot(input.confirmedSlot, "protected settlement confirmedSlot"),
    postContextSlot: canonicalContextSlot(input.postContextSlot, "protected settlement postContextSlot"),
    postStateSha256: canonicalShaField(input.postStateSha256, "protected settlement postStateSha256"),
  };
  if (payload.postContextSlot < payload.confirmedSlot) throw new Error("protected settlement post context precedes confirmed transaction slot");
  const message = createSignableMessage(Buffer.from(canonicalJson(payload), "utf8"));
  let dictionaries: Awaited<ReturnType<KeyPairSigner["signMessages"]>>;
  try {
    dictionaries = await signer.signMessages([message]);
  } catch (error) {
    throw new Error("protected settlement attestation signing failed", { cause: error });
  }
  const signature = dictionaries[0]?.[signer.address];
  if (!(signature instanceof Uint8Array)) throw new Error("protected settlement attestation signer returned no signature");
  if (signer.keyPair?.publicKey && !(await verifySignature(signer.keyPair.publicKey, signature, message.content))) {
    throw new Error("protected settlement attestation signature failed local verification");
  }
  return signatureEnvelope("confirmed-settlement", signer.address, payload, signature) as ProtectedSettlementAttestation;
}

export function assertProtectedPreSendAttestation(value: unknown): asserts value is ProtectedPreSendAttestation {
  assertProtectedAttestationEnvelope(value, "pre-send");
}

export function assertProtectedSettlementAttestation(value: unknown): asserts value is ProtectedSettlementAttestation {
  assertProtectedAttestationEnvelope(value, "confirmed-settlement");
}

export function validateProtectedPreSendAttestation(value: unknown): boolean {
  try {
    assertProtectedPreSendAttestation(value);
    return true;
  } catch {
    return false;
  }
}

export function validateProtectedSettlementAttestation(value: unknown): boolean {
  try {
    assertProtectedSettlementAttestation(value);
    return true;
  } catch {
    return false;
  }
}

/**
 * Verify a detached signature against the fixed signer address.
 *
 * The public key is derived from the expected Solana address internally. A
 * caller cannot select an arbitrary CryptoKey and then accidentally bless an
 * attestation whose `signer` field names a different authority.
 */
export async function verifyProtectedAttestationSignature(
  value: ProtectedAttestation,
  expectedSignerAddress: string,
): Promise<boolean> {
  if (value.kind === "pre-send") assertProtectedPreSendAttestation(value);
  else assertProtectedSettlementAttestation(value);
  const expectedSigner = canonicalAddress(expectedSignerAddress, "expected protected attestation signer");
  if (value.signer !== expectedSigner) return false;
  const publicKey = await webcrypto.subtle.importKey(
    "raw",
    new PublicKey(expectedSigner).toBytes(),
    { name: "Ed25519" },
    false,
    ["verify"],
  );
  const signature = Buffer.from(value.signatureBase64, "base64");
  return verifySignature(publicKey, signature as unknown as SignatureBytes, Buffer.from(canonicalJson(value.payload), "utf8"));
}

function assertProtectedAttestationEnvelope(value: unknown, kind: "pre-send" | "confirmed-settlement"): void {
  if (!value || typeof value !== "object" || Array.isArray(value)) throw new Error("protected attestation must be an object");
  const candidate = value as Record<string, unknown>;
  const expectedKeys = ["attestationSha256", "kind", "payload", "payloadSha256", "schemaVersion", "signatureBase64", "signatureSha256", "signer"];
  if (canonicalJson(Object.keys(candidate).sort()) !== canonicalJson(expectedKeys)) throw new Error("protected attestation keys are not exact");
  if (candidate.schemaVersion !== 1 || candidate.kind !== kind) throw new Error("protected attestation schema/kind is not exact");
  canonicalAddress(candidate.signer, "protected attestation signer");
  const payload = candidate.payload;
  if (!payload || typeof payload !== "object" || Array.isArray(payload)) throw new Error("protected attestation payload must be an object");
  const payloadRecord = payload as Record<string, unknown>;
  const payloadKeys = kind === "pre-send"
    ? ["addressSetSha256", "domain", "expectedSignature", "intentSha256", "lifecycleId", "messageSha256", "operation", "preContextSlot", "preStateSha256", "schemaVersion"]
    : ["addressSetSha256", "confirmedSignature", "confirmedSlot", "domain", "expectedSignature", "intentSha256", "lifecycleId", "messageSha256", "operation", "postContextSlot", "postStateSha256", "preAttestationSha256", "preSignatureSha256", "schemaVersion", "serializedTransactionSha256"];
  if (canonicalJson(Object.keys(payloadRecord).sort()) !== canonicalJson(payloadKeys)) throw new Error("protected attestation payload keys are not exact");
  if (payloadRecord.schemaVersion !== 1 || payloadRecord.domain !== (kind === "pre-send" ? "backyard-voltr-protected-pre-send-v1" : "backyard-voltr-protected-settlement-v1")) throw new Error("protected attestation payload schema/domain is not exact");
  canonicalShaField(payloadRecord.lifecycleId, "protected attestation payload lifecycleId");
  if (typeof payloadRecord.operation !== "string" || payloadRecord.operation.length === 0) throw new Error("protected attestation payload operation is invalid");
  if (typeof payloadRecord.expectedSignature !== "string" || payloadRecord.expectedSignature.length === 0) throw new Error("protected attestation payload expectedSignature is invalid");
  canonicalShaField(payloadRecord.messageSha256, "protected attestation payload messageSha256");
  canonicalShaField(payloadRecord.intentSha256, "protected attestation payload intentSha256");
  canonicalShaField(payloadRecord.addressSetSha256, "protected attestation payload addressSetSha256");
  if (kind === "pre-send") {
    canonicalContextSlot(payloadRecord.preContextSlot, "protected attestation payload preContextSlot");
    canonicalShaField(payloadRecord.preStateSha256, "protected attestation payload preStateSha256");
  } else {
    if (payloadRecord.confirmedSignature !== payloadRecord.expectedSignature) throw new Error("protected settlement confirmedSignature differs from expectedSignature");
    canonicalShaField(payloadRecord.serializedTransactionSha256, "protected settlement payload serializedTransactionSha256");
    canonicalShaField(payloadRecord.preAttestationSha256, "protected settlement payload preAttestationSha256");
    canonicalShaField(payloadRecord.preSignatureSha256, "protected settlement payload preSignatureSha256");
    canonicalContextSlot(payloadRecord.confirmedSlot, "protected settlement payload confirmedSlot");
    canonicalContextSlot(payloadRecord.postContextSlot, "protected settlement payload postContextSlot");
    canonicalShaField(payloadRecord.postStateSha256, "protected settlement payload postStateSha256");
    if ((payloadRecord.postContextSlot as number) < (payloadRecord.confirmedSlot as number)) throw new Error("protected settlement payload post context precedes confirmed slot");
  }
  const payloadBytes = Buffer.from(canonicalJson(payload), "utf8");
  if (candidate.payloadSha256 !== sha256(payloadBytes)) throw new Error("protected attestation payload hash does not match payload");
  const signatureBase64 = canonicalBase64(candidate.signatureBase64, "protected attestation signatureBase64");
  const signature = Buffer.from(signatureBase64, "base64");
  if (signature.length !== 64 || candidate.signatureSha256 !== sha256(signature)) throw new Error("protected attestation signature hash is not exact");
  if (candidate.attestationSha256 !== sha256(Buffer.from(canonicalJson({ kind, payloadSha256: candidate.payloadSha256, signatureSha256: candidate.signatureSha256 }), "utf8"))) throw new Error("protected attestation hash is not exact");
}

function deriveAta(owner: string, mint: string): string {
  return PublicKey.findProgramAddressSync(
    [
      new PublicKey(owner).toBuffer(),
      new PublicKey(PARTNER_ROUTE.programs.token).toBuffer(),
      new PublicKey(mint).toBuffer(),
    ],
    new PublicKey(PARTNER_ROUTE.programs.associatedToken),
  )[0].toBase58();
}

function derivePolicy(seed: bigint): string {
  const seedBytes = Buffer.alloc(8);
  seedBytes.writeBigUInt64LE(seed);
  return PublicKey.findProgramAddressSync(
    [
      Buffer.from("smart_account"),
      Buffer.from("policy"),
      new PublicKey(PARTNER_ROUTE.squads.settings).toBuffer(),
      seedBytes,
    ],
    new PublicKey(PARTNER_ROUTE.squads.program),
  )[0].toBase58();
}

/**
 * One stable account set is used by every transaction in the partner proof.
 *
 * It intentionally includes user token accounts, the Voltr route, the exact
 * Squads manager/policies, and route-specific Kamino positions. The user and guardian
 * system accounts are excluded because their shared fee-payer lamport balances
 * can change in unrelated, independently authorized transactions; every
 * maintained sender already closes their exact transaction fee delta. Shared reserve,
 * oracle, market, collateral-mint, reserve-vault, global farm, and the global
 * USDC mint account are excluded because unrelated users legitimately mutate
 * them. The USDC mint address, owner, token program, and decimals remain pinned
 * by RouteSpec/current-state checks; only its globally changing supply bytes
 * are absent here. Squads Settings is also excluded because unrelated smart-account
 * activity legitimately advances its global transaction counters; every manager
 * preflight and the final policy verifier independently decode Settings and prove
 * the exact manager plus closed policy namespace. A withdrawal receipt/escrow is generation-specific and is
 * bound by requestOrigin instead of being smuggled into this static chain.
 */
export function fourMarketProtectedAddresses(): readonly string[] {
  const user = PARTNER_ROUTE.setupAdmin;
  const values = [
    deriveAta(user, PARTNER_ROUTE.asset.mint),
    deriveAta(user, PARTNER_FOUR_MARKET_ROUTE.commonVoltr.lpMint),
    PARTNER_ROUTE.squads.manager,
    PARTNER_ROUTE.vault,
    PARTNER_FOUR_MARKET_ROUTE.commonVoltr.protocol,
    PARTNER_FOUR_MARKET_ROUTE.commonVoltr.idleAuth,
    PARTNER_FOUR_MARKET_ROUTE.commonVoltr.idleAta,
    PARTNER_FOUR_MARKET_ROUTE.commonVoltr.lpMint,
    PARTNER_FOUR_MARKET_ROUTE.commonVoltr.lpMintAuth,
    PARTNER_FOUR_MARKET_ROUTE.commonVoltr.adaptorAddReceipt,
    ...PARTNER_FOUR_MARKET_STRATEGIES.flatMap(({ graph, voltr }) => [
      voltr.strategyAuth,
      voltr.strategyInitReceipt,
      voltr.strategyAssetAta,
      graph.obligation,
      graph.userMetadata,
      graph.obligationFarm,
    ]),
    ...POLICY_SEEDS.map(derivePolicy),
  ].map(String);
  const unique = [...new Set(values)];
  if (values.length !== 42 || unique.length !== 42) {
    throw new Error(`four-market protected account set must contain exactly 42 unique addresses (got ${values.length}/${unique.length})`);
  }
  if (unique.length !== values.length) {
    throw new Error("four-market protected account set contains a duplicate address");
  }
  return unique;
}

export function fourMarketProtectedAddressSetSha256(): string {
  return sha256(canonicalJson(fourMarketProtectedAddresses()));
}

export function fingerprintProtectedSnapshots(
  addresses: readonly string[],
  accounts: readonly (AccountSnapshot | null)[],
  contextSlot: number,
): ProtectedSnapshotEvidence {
  return buildProtectedSnapshotEvidence(addresses, accounts, contextSlot);
}

export async function loadFourMarketProtectedState(
  rpcUrl: string,
  minimumContextSlot?: number,
): Promise<ProtectedSnapshotEvidence> {
  const addresses = fourMarketProtectedAddresses();
  const response = await confirmedSnapshots(rpcUrl, addresses, minimumContextSlot);
  return fingerprintProtectedSnapshots(addresses, response.accounts, response.contextSlot);
}

export function protectedStateEnvelope(
  before: ProtectedStateFingerprint,
  after: ProtectedStateFingerprint,
) {
  if (before.addressSetSha256 !== after.addressSetSha256) {
    throw new Error("protected state address set changed across transaction");
  }
  if (after.contextSlot < before.contextSlot) {
    throw new Error("protected poststate context precedes prestate");
  }
  return {
    schemaVersion: 1 as const,
    addressSetSha256: before.addressSetSha256,
    beforeContextSlot: before.contextSlot,
    beforeSha256: before.stateSha256,
    afterContextSlot: after.contextSlot,
    afterSha256: after.stateSha256,
  };
}
