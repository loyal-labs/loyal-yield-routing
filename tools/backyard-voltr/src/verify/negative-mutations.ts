import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { address, type Address } from "@solana/kit";
import {
  AddressLookupTableAccount,
  ComputeBudgetProgram,
  PublicKey,
  TransactionInstruction,
  TransactionMessage,
  VersionedTransaction,
} from "@solana/web3.js";

import { PARTNER_FOUR_MARKET_ROUTE, PARTNER_ROUTE, fourMarketRouteSpecSha256, type PartnerStrategyId } from "../domain/route-spec.js";
import { buildManagerWrapperForCompatibility, buildManagerWrapperForVerification } from "../runtime/manager.js";
import type { CanonicalAccount, CanonicalInstruction } from "../integrations/voltr.js";
import type { RuntimePolicyArtifact, RuntimePolicyArtifactEntry } from "../policies/compiler.js";

const MANAGER_COMPUTE_UNIT_LIMIT = 500_000;
const MANAGER_HEAP_FRAME_BYTES = 256 * 1_024;
export const SOLANA_PACKET_LIMIT_BYTES = 1_232;
const REJECTED_MUTATIONS = [
  "wrong-guardian", "wrong-manager", "wrong-vault", "wrong-strategy", "wrong-reserve",
  "wrong-market", "wrong-farm", "wrong-receipt", "wrong-obligation", "wrong-mint",
  "wrong-program", "account-order", "account-role", "wrong-discriminator", "adaptor-tail",
  "zero-amount", "over-limit-amount", "mixed-graph", "extra-instruction", "reordered-instruction",
] as const;
const DEPOSIT_CONSTRAINED_INDEXES = [0, 1, 2, 3, 4, 5, 6, 7, 8, 10, 11, 12, 13, 14, 15, 17, 21, 29, 30] as const;
const WITHDRAW_CONSTRAINED_INDEXES = [0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 11, 12, 13, 14, 15, 17, 21, 26, 27] as const;
const ZERO_ADDRESS = "11111111111111111111111111111111";

type Operation = "deposit" | "withdraw";
type MutationName = (typeof REJECTED_MUTATIONS)[number] | `omitted-index-${number}`;
type Strategy = (typeof PARTNER_FOUR_MARKET_ROUTE.strategies)[number];
export type NegativeMutationEnforcementLayer = "Squads policy" | "on-chain validator" | "canonical pre-send verifier";

type NegativeMutationLocalRejection = Readonly<{
  kind: "local-canonical-rejection";
  classification: string;
  messageSha256: string;
  canonicalMessageSha256: string;
  instructionCount: number;
}>;

type NegativeMutationConfirmedSimulationError = Readonly<{
  kind: "confirmed-simulation-error";
  /** This is an RPC observation persisted by the producer; the verifier does not replay it. */
  observation: "producer-observed-confirmed-rpc";
  classification: string;
  err: unknown;
  logs: readonly string[];
  logsSha256: string;
  unitsConsumed: number | null;
  contextSlot: number;
}>;

type NegativeMutationSimulationError = NegativeMutationLocalRejection | NegativeMutationConfirmedSimulationError;

export type NegativeMutationSimulation = Readonly<{
  simulationError: NegativeMutationSimulationError;
  preProtectedStateSha256: string;
  postProtectedStateSha256: string;
  preProtectedContextSlot: number;
  postProtectedContextSlot: number;
}>;

export type NegativeMutationSimulationRequest = Readonly<{
  id: string;
  strategyId: PartnerStrategyId;
  operation: Operation;
  mutation: MutationName;
  enforcementLayer: NegativeMutationEnforcementLayer;
  canonicalMessageSha256: string;
  serializedMessageBase64: string;
  serializedMessageSha256: string;
  preProtectedContextSlot: number;
  transaction: VersionedTransaction;
}>;

export type NegativeMutation = Readonly<{
  id: string;
  enforcementLayer: NegativeMutationEnforcementLayer;
  recentBlockhash: string;
  serializedMessageBase64: string;
  serializedMessageSha256: string;
  accepted: false;
  broadcast: false;
  simulationError: NegativeMutationSimulationError;
  preProtectedStateSha256: string;
  postProtectedStateSha256: string;
  preProtectedContextSlot: number;
  postProtectedContextSlot: number;
}>;

export type NegativeMutationLookupTable = Readonly<{
  address: string;
  authority: string;
  addressCount: number;
  orderedAddressesSha256: string;
  fullOrderedAddressesSha256: string;
  addresses: readonly string[];
}>;

export type NegativeMutationArtifact = Readonly<{
  schemaVersion: 1;
  evidenceType: "backyard-voltr-negative-mutations-confirmed";
  broadcast: false;
  generatorSourceSha256: string;
  routeId: string;
  routeSpecSha256: string;
  lookupTable: NegativeMutationLookupTable;
  mutations: readonly NegativeMutation[];
}>;

type GeneratedMutation = Readonly<{
  id: string;
  strategyId: PartnerStrategyId;
  operation: Operation;
  mutation: MutationName;
  enforcementLayer: string;
  transaction: VersionedTransaction;
  serializedMessageBase64: string;
  serializedMessageSha256: string;
}>;

function sha256(value: ArrayLike<number> | string): string {
  return createHash("sha256").update(typeof value === "string" ? value : Uint8Array.from(value)).digest("hex");
}

function canonicalJson(value: unknown): string {
  return JSON.stringify(value, (_key, item) => typeof item === "bigint" ? item.toString() : item);
}

const HASH_PATTERN = /^[0-9a-f]{64}$/;

function hasExactKeys(value: Record<string, unknown>, keys: readonly string[]): boolean {
  const observed = Object.keys(value).sort();
  const expected = [...keys].sort();
  return observed.length === expected.length && observed.every((key, index) => key === expected[index]);
}

function isSha256(value: unknown): value is string {
  return typeof value === "string" && HASH_PATTERN.test(value);
}

function isPositiveContextSlot(value: unknown): value is number {
  return typeof value === "number" && Number.isSafeInteger(value) && value > 0;
}

function isUnitsConsumed(value: unknown): value is number | null {
  return value === null || (typeof value === "number" && Number.isFinite(value) && value >= 0);
}

/**
 * Keep classification deterministic and independently checkable. This is
 * deliberately shared by the producer and offline verifier so a report
 * cannot replace the RPC error with a self-authored label.
 */
export function classifyNegativeMutationSimulationError(error: unknown, logs: readonly string[]): string {
  if (error && typeof error === "object" && "InstructionError" in error) {
    const instructionError = (error as { InstructionError?: unknown }).InstructionError;
    if (Array.isArray(instructionError) && instructionError.length === 2 && typeof instructionError[0] === "number") {
      const detail = instructionError[1];
      if (detail && typeof detail === "object" && "Custom" in detail && typeof (detail as { Custom?: unknown }).Custom === "number") {
        return `instruction-${instructionError[0]}-custom-${(detail as { Custom: number }).Custom}`;
      }
      if (typeof detail === "string") return `instruction-${instructionError[0]}-${detail}`;
    }
  }
  const failedProgram = logs.find((line) => line.includes(" failed:"));
  return failedProgram ? "program-failure-log" : "simulation-error";
}

function lookupTableIdentity(table: AddressLookupTableAccount): NegativeMutationLookupTable {
  const addresses = table.state.addresses.map((item) => item.toBase58());
  const fullOrderedAddressesSha256 = sha256(addresses.join("\n"));
  return {
    address: table.key.toBase58(),
    authority: table.state.authority?.toBase58() ?? "",
    addressCount: addresses.length,
    // Keep the route's established name and also expose the explicit full
    // list hash. Both values must cover the entire ordered address vector.
    orderedAddressesSha256: fullOrderedAddressesSha256,
    fullOrderedAddressesSha256,
    addresses,
  };
}

function exactRouteLookupTableIdentity(identity: NegativeMutationLookupTable): boolean {
  return identity.address === PARTNER_ROUTE.lookupTable.address
    && identity.authority === PARTNER_FOUR_MARKET_ROUTE.lookupTable.authority
    && identity.addressCount === PARTNER_FOUR_MARKET_ROUTE.lookupTable.addressCount
    && identity.orderedAddressesSha256 === PARTNER_FOUR_MARKET_ROUTE.lookupTable.orderedAddressesSha256
    && identity.fullOrderedAddressesSha256 === PARTNER_FOUR_MARKET_ROUTE.lookupTable.orderedAddressesSha256
    && identity.orderedAddressesSha256 === identity.fullOrderedAddressesSha256;
}

/** Reject an ALT unless every immutable route identity is exact. */
export function assertExactNegativeMutationLookupTable(table: AddressLookupTableAccount): NegativeMutationLookupTable {
  const identity = lookupTableIdentity(table);
  if (!exactRouteLookupTableIdentity(identity)) {
    throw new Error("negative mutation ALT identity/order does not match the immutable four-market route");
  }
  return identity;
}

function sourceInstruction(artifact: RuntimePolicyArtifact, strategyId: PartnerStrategyId, operation: Operation, amountRaw: bigint): CanonicalInstruction {
  const source = artifact.sourceManifests?.find((candidate) => candidate.strategyId === strategyId) ?? (artifact.sourceManifest.strategyId === strategyId ? artifact.sourceManifest : null);
  const raw = source?.instructions[operation];
  if (!raw) throw new Error(`negative mutation source manifest missing ${strategyId}/${operation}`);
  const data = Buffer.from(raw.dataBase64, "base64");
  if (data.toString("base64") !== raw.dataBase64 || data.length !== raw.dataLength || sha256(data) !== raw.dataSha256) throw new Error(`negative mutation source data is not canonical for ${strategyId}/${operation}`);
  if (data.length < 16) throw new Error(`negative mutation source data has no amount field for ${strategyId}/${operation}`);
  data.writeBigUInt64LE(amountRaw, 8);
  const accounts: CanonicalAccount[] = raw.accounts.map((account) => ({
    index: account.index,
    label: account.label,
    address: address(account.address),
    signer: account.signer,
    writable: account.writable,
  }));
  return {
    programId: address(raw.programId),
    data,
    dataBase64: data.toString("base64"),
    dataSha256: sha256(data),
    dataLength: data.length,
    accounts,
  };
}

function cloneInner(inner: CanonicalInstruction): { programId: Address; data: Buffer; accounts: CanonicalAccount[] } {
  return { programId: inner.programId, data: Buffer.from(inner.data), accounts: inner.accounts.map((account) => ({ ...account })) };
}

function finishInner(inner: ReturnType<typeof cloneInner>): CanonicalInstruction {
  return { ...inner, dataBase64: inner.data.toString("base64"), dataSha256: sha256(inner.data), dataLength: inner.data.length };
}

function accountIndex(inner: CanonicalInstruction, label: string): number {
  const index = inner.accounts.findIndex((account) => account.label === label);
  if (index < 0) throw new Error(`negative mutation canonical inner vector lacks ${label}`);
  return index;
}

/**
 * Choose a key that is already present in the canonical inner packet.  A
 * negative-policy case must not introduce ZERO_ADDRESS (or any other new
 * static key): doing so changes the wire size and can make the RPC reject the
 * packet before Squads or the downstream validator sees the intended bad
 * graph.  Keeping the original meta's role/index while swapping only its
 * address still violates the constrained account identity.
 */
function existingCanonicalReplacement(inner: CanonicalInstruction, targetIndex: number, targetAddress = inner.accounts[targetIndex]?.address): Address {
  const candidates = [
    ...inner.accounts.map((account) => account.address),
    inner.programId,
    PARTNER_ROUTE.squads.program,
    PARTNER_ROUTE.squads.guardian,
  ];
  const replacement = candidates.find((candidate) => candidate !== targetAddress);
  if (!replacement) throw new Error(`negative mutation cannot find an existing packet key distinct from index ${targetIndex}`);
  return replacement;
}

function mutateInner(inner: CanonicalInstruction, mutation: MutationName, strategyId: PartnerStrategyId, operation: Operation, amountRaw: bigint): { inner: CanonicalInstruction; enforcementLayer: string } {
  const next = cloneInner(inner);
  const wrong = (index: number, layer = "Squads policy") => { next.accounts[index] = { ...next.accounts[index]!, address: existingCanonicalReplacement(inner, index) }; return layer; };
  let enforcementLayer = "canonical pre-send verifier";
  switch (mutation) {
    case "wrong-guardian":
    case "extra-instruction":
    case "reordered-instruction":
      // Applied to the outer wrapper/top-level packet after the inner graph
      // is reconstructed.
      break;
    case "wrong-manager": enforcementLayer = wrong(accountIndex(inner, "manager")); break;
    case "wrong-vault": enforcementLayer = wrong(accountIndex(inner, "vault")); break;
    case "wrong-strategy": enforcementLayer = wrong(accountIndex(inner, "strategy")); break;
    case "wrong-reserve": enforcementLayer = wrong(accountIndex(inner, "reserve")); break;
    case "wrong-market": enforcementLayer = wrong(accountIndex(inner, "lendingMarket")); break;
    case "wrong-farm": {
      const index = accountIndex(inner, operation === "deposit" ? "reserveFarmState" : "obligationFarm");
      // The alternate must already be in this exact packet. A different
      // strategy's farm is not necessarily in the pinned ALT and can turn a
      // policy-level negative test into an oversized/local transport error.
      const vaultAddress = inner.accounts[accountIndex(inner, "vault")]!.address;
      if (vaultAddress === inner.accounts[index]!.address) throw new Error("negative mutation farm and vault addresses unexpectedly match");
      next.accounts[index] = { ...next.accounts[index]!, address: vaultAddress };
      enforcementLayer = "on-chain validator";
      break;
    }
    case "wrong-receipt": enforcementLayer = wrong(accountIndex(inner, "strategyInitReceipt")); break;
    case "wrong-obligation": enforcementLayer = wrong(accountIndex(inner, "kaminoObligation")); break;
    case "wrong-mint": enforcementLayer = wrong(accountIndex(inner, "vaultAssetMint")); break;
    case "wrong-program": next.programId = existingCanonicalReplacement(inner, -1, inner.programId); enforcementLayer = "Squads policy"; break;
    case "account-order": {
      const left = accountIndex(inner, "vault");
      const right = accountIndex(inner, "strategy");
      [next.accounts[left], next.accounts[right]] = [next.accounts[right]!, next.accounts[left]!];
      next.accounts = next.accounts.map((account, index) => ({ ...account, index }));
      break;
    }
    case "account-role": {
      const index = accountIndex(inner, "vault");
      next.accounts[index] = { ...next.accounts[index]!, writable: !next.accounts[index]!.writable };
      break;
    }
    case "wrong-discriminator": next.data[0] = (next.data[0] ?? 0) ^ 0xff; enforcementLayer = "canonical pre-send verifier"; break;
    case "adaptor-tail": next.data = Buffer.concat([next.data, Buffer.from([0])]); break;
    case "zero-amount": next.data.writeBigUInt64LE(0n, 8); enforcementLayer = "Squads policy"; break;
    case "over-limit-amount": next.data.writeBigUInt64LE(PARTNER_ROUTE.asset.maxManagerOperationRaw + 1n, 8); enforcementLayer = "Squads policy"; break;
    case "mixed-graph": {
      // Keep both substituted keys in the canonical packet. Using another
      // strategy's reserve can introduce a new static key when that reserve
      // is not in the manager ALT, making the mutation oversized before
      // Squads can inspect it. Swapping these two constrained addresses still
      // violates the graph while preserving the original key set and roles.
      const strategyIndex = accountIndex(inner, "strategy");
      const marketIndex = accountIndex(inner, "lendingMarket");
      const strategyAddress = next.accounts[strategyIndex]!.address;
      next.accounts[strategyIndex] = { ...next.accounts[strategyIndex]!, address: next.accounts[marketIndex]!.address };
      next.accounts[marketIndex] = { ...next.accounts[marketIndex]!, address: strategyAddress };
      break;
    }
    default: {
      const prefix = "omitted-index-";
      if (mutation.startsWith(prefix)) {
        const index = Number(mutation.slice(prefix.length));
        if (!Number.isSafeInteger(index) || index < 0 || index >= next.accounts.length) throw new Error(`omitted index ${mutation} escapes ${operation} vector`);
        enforcementLayer = "on-chain validator";
        next.accounts[index] = { ...next.accounts[index]!, address: existingCanonicalReplacement(inner, index) };
      }
      else throw new Error(`unsupported negative mutation ${mutation}`);
    }
  }
  return { inner: finishInner(next), enforcementLayer };
}

function expectedLayer(mutation: MutationName): NegativeMutationEnforcementLayer {
  if (mutation === "wrong-guardian" || mutation === "account-order" || mutation === "account-role" || mutation === "wrong-discriminator" || mutation === "adaptor-tail" || mutation === "extra-instruction" || mutation === "reordered-instruction") return "canonical pre-send verifier";
  if (mutation === "wrong-farm" || mutation.startsWith("omitted-index-")) return "on-chain validator";
  return "Squads policy";
}

function wrapperInstruction(entry: RuntimePolicyArtifactEntry, operation: Operation, inner: CanonicalInstruction, amountRaw: bigint, canonical: boolean): TransactionInstruction {
  const wrapper = canonical
    ? buildManagerWrapperForVerification(operation, entry, inner, amountRaw).instruction
    : buildManagerWrapperForCompatibility(entry.policy, inner).instruction;
  return new TransactionInstruction({
    programId: wrapper.programId,
    keys: wrapper.keys.map((key) => ({ pubkey: key.pubkey, isSigner: key.isSigner, isWritable: key.isWritable })),
    data: Buffer.from(wrapper.data),
  });
}

function cloneInstruction(instruction: TransactionInstruction): TransactionInstruction {
  return new TransactionInstruction({
    programId: instruction.programId,
    keys: instruction.keys.map((key) => ({ pubkey: key.pubkey, isSigner: key.isSigner, isWritable: key.isWritable })),
    data: Buffer.from(instruction.data),
  });
}

function mutationTransaction(entry: RuntimePolicyArtifactEntry, operation: Operation, inner: CanonicalInstruction, mutation: MutationName, amountRaw: bigint, recentBlockhash: string, lookupTable: AddressLookupTableAccount | null): TransactionInstruction[] {
  const base = wrapperInstruction(entry, operation, inner, amountRaw, mutation === "wrong-guardian" ? true : false);
  if (mutation === "wrong-guardian") {
    base.keys[2] = { ...base.keys[2]!, pubkey: new PublicKey(ZERO_ADDRESS) };
  }
  if (mutation === "extra-instruction") {
    return [ComputeBudgetProgram.setComputeUnitLimit({ units: MANAGER_COMPUTE_UNIT_LIMIT }), ComputeBudgetProgram.requestHeapFrame({ bytes: MANAGER_HEAP_FRAME_BYTES }), base, ComputeBudgetProgram.setComputeUnitLimit({ units: MANAGER_COMPUTE_UNIT_LIMIT })];
  }
  if (mutation === "reordered-instruction") {
    return [base, ComputeBudgetProgram.setComputeUnitLimit({ units: MANAGER_COMPUTE_UNIT_LIMIT }), ComputeBudgetProgram.requestHeapFrame({ bytes: MANAGER_HEAP_FRAME_BYTES })];
  }
  return [ComputeBudgetProgram.setComputeUnitLimit({ units: MANAGER_COMPUTE_UNIT_LIMIT }), ComputeBudgetProgram.requestHeapFrame({ bytes: MANAGER_HEAP_FRAME_BYTES }), base];
}

function compilePacket(instructions: readonly TransactionInstruction[], recentBlockhash: string, lookupTable: AddressLookupTableAccount | null): { transaction: VersionedTransaction; messageBase64: string; messageSha256: string } {
  const message = new TransactionMessage({ payerKey: new PublicKey(PARTNER_ROUTE.squads.guardian), recentBlockhash, instructions: [...instructions] }).compileToV0Message(lookupTable ? [lookupTable] : []);
  const transaction = new VersionedTransaction(message);
  const serialized = Buffer.from(message.serialize());
  return { transaction, messageBase64: serialized.toString("base64"), messageSha256: sha256(serialized) };
}

function compileCanonical(entry: RuntimePolicyArtifactEntry, operation: Operation, inner: CanonicalInstruction, amountRaw: bigint, recentBlockhash: string, lookupTable: AddressLookupTableAccount | null) {
  return compilePacket([
    ComputeBudgetProgram.setComputeUnitLimit({ units: MANAGER_COMPUTE_UNIT_LIMIT }),
    ComputeBudgetProgram.requestHeapFrame({ bytes: MANAGER_HEAP_FRAME_BYTES }),
    wrapperInstruction(entry, operation, inner, amountRaw, true),
  ], recentBlockhash, lookupTable);
}

function compileMutation(entry: RuntimePolicyArtifactEntry, operation: Operation, inner: CanonicalInstruction, mutation: MutationName, amountRaw: bigint, recentBlockhash: string, lookupTable: AddressLookupTableAccount | null) {
  return compilePacket(mutationTransaction(entry, operation, inner, mutation, amountRaw, recentBlockhash, lookupTable), recentBlockhash, lookupTable);
}

function omittedIndexes(operation: Operation, accountCount: number): number[] {
  const constrained = new Set<number>(operation === "deposit" ? DEPOSIT_CONSTRAINED_INDEXES : WITHDRAW_CONSTRAINED_INDEXES);
  return Array.from({ length: accountCount }, (_, index) => index).filter((index) => !constrained.has(index));
}

function mutationNames(operation: Operation, accountCount: number): MutationName[] {
  return [...REJECTED_MUTATIONS, ...omittedIndexes(operation, accountCount).map((index) => `omitted-index-${index}` as const)];
}

function entryFor(artifact: RuntimePolicyArtifact, strategyId: PartnerStrategyId, operation: Operation): RuntimePolicyArtifactEntry {
  const entry = artifact.policies.find((candidate) => candidate.strategyId === strategyId && candidate.operation === operation);
  if (!entry) throw new Error(`negative mutation policy artifact missing ${strategyId}/${operation}`);
  return entry;
}

function fallbackLookupTable(artifact: RuntimePolicyArtifact): AddressLookupTableAccount {
  const values = new Set<string>([
    PARTNER_ROUTE.squads.program,
    PARTNER_ROUTE.squads.guardian,
    PARTNER_ROUTE.squads.manager,
    PARTNER_ROUTE.asset.mint,
    PARTNER_FOUR_MARKET_ROUTE.commonVoltr.protocol,
    PARTNER_FOUR_MARKET_ROUTE.commonVoltr.idleAta,
    PARTNER_FOUR_MARKET_ROUTE.commonVoltr.lpMint,
  ]);
  for (const manifest of artifact.sourceManifests ?? [artifact.sourceManifest]) {
    for (const operation of ["deposit", "withdraw"] as const) {
      const instruction = manifest.instructions[operation];
      values.add(instruction.programId);
      for (const account of instruction.accounts) values.add(account.address);
    }
  }
  for (const entry of artifact.policies) values.add(entry.policy);
  return new AddressLookupTableAccount({
    key: new PublicKey(PARTNER_ROUTE.lookupTable.address),
    state: {
      deactivationSlot: BigInt("18446744073709551615"),
      lastExtendedSlot: 0,
      lastExtendedSlotStartIndex: 0,
      authority: new PublicKey(PARTNER_FOUR_MARKET_ROUTE.lookupTable.authority),
      addresses: [...values].map((item) => new PublicKey(item)),
    },
  });
}

/**
 * Build every rejected packet without RPC, a signer, or a send. The caller's
 * simulation callback is the only place allowed to attach a live simulation
 * error and protected-state observations.
 */
export async function produceNegativeMutationArtifact(input: Readonly<{
  artifact: RuntimePolicyArtifact;
  amountRaw: bigint;
  recentBlockhash: string;
  lookupTable?: AddressLookupTableAccount | null;
  protectedStateSha256: string;
  protectedContextSlot: number;
  simulate?: (request: NegativeMutationSimulationRequest) => Promise<NegativeMutationSimulation>;
}>): Promise<NegativeMutationArtifact> {
  const mutations: NegativeMutation[] = [];
  if (!isPositiveContextSlot(input.protectedContextSlot)) throw new Error("negative mutation protected context slot must be a positive safe integer");
  if (!isSha256(input.protectedStateSha256)) throw new Error("negative mutation protected state fingerprint must be a lowercase SHA-256 digest");
  const lookupTable = input.lookupTable ?? fallbackLookupTable(input.artifact);
  // The offline generator can still use a synthetic table to inspect packet
  // construction, but any producer that attaches RPC observations must use
  // the exact immutable route ALT. The verifier below rejects synthetic ALT
  // identities, so they cannot become lifecycle evidence accidentally.
  if (input.simulate) assertExactNegativeMutationLookupTable(lookupTable);
  let previousPostProtectedContextSlot = input.protectedContextSlot;
  for (const strategy of PARTNER_FOUR_MARKET_ROUTE.strategies) {
    for (const operation of ["deposit", "withdraw"] as const) {
      const entry = entryFor(input.artifact, strategy.id, operation);
      const baseline = sourceInstruction(input.artifact, strategy.id, operation, input.amountRaw);
      for (const mutation of mutationNames(operation, baseline.accounts.length)) {
        const generated = mutation === "extra-instruction" || mutation === "reordered-instruction"
          ? { inner: baseline, enforcementLayer: "canonical pre-send verifier" }
          : mutateInner(baseline, mutation, strategy.id, operation, input.amountRaw);
        const compiled = compileMutation(entry, operation, generated.inner, mutation, input.amountRaw, input.recentBlockhash, lookupTable);
        const canonical = compileCanonical(entry, operation, baseline, input.amountRaw, input.recentBlockhash, lookupTable);
        const id = `${strategy.id}:${operation}:${mutation}`;
        const enforcementLayer = expectedLayer(mutation);
        const packetBytes = Buffer.from(compiled.transaction.serialize()).length;
        if (enforcementLayer !== "canonical pre-send verifier" && packetBytes > SOLANA_PACKET_LIMIT_BYTES) throw new Error(`${id} mutation packet exceeds Solana's ${SOLANA_PACKET_LIMIT_BYTES}-byte limit before evidence production (${packetBytes} bytes)`);
        const simulation = input.simulate
          ? await input.simulate({ id, strategyId: strategy.id, operation, mutation, enforcementLayer, canonicalMessageSha256: canonical.messageSha256, serializedMessageBase64: compiled.messageBase64, serializedMessageSha256: compiled.messageSha256, preProtectedContextSlot: input.protectedContextSlot, transaction: compiled.transaction })
          : {
            simulationError: { kind: "local-canonical-rejection", classification: "offline-simulation-not-run", messageSha256: compiled.messageSha256, canonicalMessageSha256: canonical.messageSha256, instructionCount: compiled.transaction.message.compiledInstructions.length } as NegativeMutationLocalRejection,
            preProtectedStateSha256: input.protectedStateSha256,
            postProtectedStateSha256: input.protectedStateSha256,
            preProtectedContextSlot: input.protectedContextSlot,
            postProtectedContextSlot: input.protectedContextSlot,
        };
        if (simulation.preProtectedStateSha256 !== simulation.postProtectedStateSha256) throw new Error(`${id} simulation changed protected state`);
        if (!isPositiveContextSlot(simulation.preProtectedContextSlot) || !isPositiveContextSlot(simulation.postProtectedContextSlot) || simulation.preProtectedContextSlot !== input.protectedContextSlot || simulation.postProtectedContextSlot < simulation.preProtectedContextSlot) throw new Error(`${id} simulation protected context is not bound to the prestate`);
        if (enforcementLayer === "canonical pre-send verifier") {
          if (simulation.postProtectedContextSlot !== input.protectedContextSlot) throw new Error(`${id} local rejection must retain the common protected context`);
        } else {
          if (simulation.postProtectedContextSlot < previousPostProtectedContextSlot) throw new Error(`${id} RPC simulation protected context is not monotonic`);
          previousPostProtectedContextSlot = simulation.postProtectedContextSlot;
        }
        if (input.simulate && !verifySimulationError(simulation.simulationError, enforcementLayer, mutation, compiled.messageSha256, canonical.messageSha256, compiled.transaction.message.compiledInstructions.length, simulation.preProtectedContextSlot, simulation.postProtectedContextSlot)) throw new Error(`${id} simulation callback returned a non-canonical negative-mutation error envelope`);
        mutations.push({ id, enforcementLayer, recentBlockhash: input.recentBlockhash, serializedMessageBase64: compiled.messageBase64, serializedMessageSha256: compiled.messageSha256, accepted: false, broadcast: false, simulationError: simulation.simulationError, preProtectedStateSha256: simulation.preProtectedStateSha256, postProtectedStateSha256: simulation.postProtectedStateSha256, preProtectedContextSlot: simulation.preProtectedContextSlot, postProtectedContextSlot: simulation.postProtectedContextSlot });
      }
    }
  }
  const lookup = lookupTableIdentity(lookupTable);
  return { schemaVersion: 1, evidenceType: "backyard-voltr-negative-mutations-confirmed", broadcast: false, generatorSourceSha256: negativeMutationGeneratorSourceSha256(), routeId: PARTNER_FOUR_MARKET_ROUTE.id, routeSpecSha256: fourMarketRouteSpecSha256(), lookupTable: lookup, mutations };
}

/** Reject a mutation before RPC when its failure is a local canonical-wire invariant. */
export function localCanonicalMutationRejection(request: NegativeMutationSimulationRequest, preProtectedStateSha256: string): NegativeMutationSimulation {
  if (request.enforcementLayer !== "canonical pre-send verifier") throw new Error(`${request.id} is not a local canonical mutation`);
  if (!isPositiveContextSlot(request.preProtectedContextSlot)) throw new Error(`${request.id} local canonical prestate context slot is invalid`);
  if (request.serializedMessageSha256 === request.canonicalMessageSha256) throw new Error(`${request.id} local validator could not distinguish the mutation from the canonical message`);
  const decoded = request.transaction;
  const serializedMessage = Buffer.from(decoded.message.serialize());
  if (serializedMessage.toString("base64") !== request.serializedMessageBase64 || sha256(serializedMessage) !== request.serializedMessageSha256 || decoded.signatures.some((signature) => signature.some((byte) => byte !== 0))) throw new Error(`${request.id} local canonical packet envelope is not exact unsigned wire data`);
  const instructionCount = decoded.message.compiledInstructions.length;
  const structuralCheck = request.mutation === "extra-instruction" ? instructionCount === 4 : request.mutation === "reordered-instruction" ? instructionCount === 3 && decoded.message.compiledInstructions[0]?.programIdIndex !== decoded.message.compiledInstructions[2]?.programIdIndex : instructionCount === 3;
  if (!structuralCheck) throw new Error(`${request.id} local canonical validator did not observe the expected packet mutation`);
  return { simulationError: { kind: "local-canonical-rejection", classification: `canonical-${request.mutation}`, messageSha256: request.serializedMessageSha256, canonicalMessageSha256: request.canonicalMessageSha256, instructionCount }, preProtectedStateSha256, postProtectedStateSha256: preProtectedStateSha256, preProtectedContextSlot: request.preProtectedContextSlot, postProtectedContextSlot: request.preProtectedContextSlot };
}

export function negativeMutationGeneratorSourceSha256(): string {
  // Bind the evidence to the checked-out generator bytes, not to a caller
  // supplied label or a hand-authored verdict string.
  return sha256(readFileSync(fileURLToPath(import.meta.url)));
}

function isRecord(value: unknown): value is Record<string, unknown> { return value !== null && typeof value === "object" && !Array.isArray(value); }

function parseLookupTable(value: unknown): Readonly<{ table: AddressLookupTableAccount; identity: NegativeMutationLookupTable }> | null {
  if (!isRecord(value) || !hasExactKeys(value, ["address", "authority", "addressCount", "orderedAddressesSha256", "fullOrderedAddressesSha256", "addresses"])) return null;
  const addressCount = typeof value.addressCount === "number" ? value.addressCount : null;
  if (typeof value.address !== "string" || typeof value.authority !== "string" || addressCount === null || !Number.isSafeInteger(addressCount) || addressCount <= 0 || !isSha256(value.orderedAddressesSha256) || !isSha256(value.fullOrderedAddressesSha256) || !Array.isArray(value.addresses) || value.addresses.length !== addressCount || !value.addresses.every((item) => typeof item === "string")) return null;
  try {
    const key = new PublicKey(value.address);
    const authority = new PublicKey(value.authority);
    const addresses = value.addresses.map((item) => new PublicKey(item));
    const table = new AddressLookupTableAccount({ key, state: { deactivationSlot: BigInt("18446744073709551615"), lastExtendedSlot: 0, lastExtendedSlotStartIndex: 0, authority, addresses } });
    const identity = lookupTableIdentity(table);
    if (identity.address !== value.address || identity.authority !== value.authority || identity.addressCount !== addressCount || identity.orderedAddressesSha256 !== value.orderedAddressesSha256 || identity.fullOrderedAddressesSha256 !== value.fullOrderedAddressesSha256 || canonicalJson(identity.addresses) !== canonicalJson(value.addresses) || !exactRouteLookupTableIdentity(identity)) return null;
    return { table, identity };
  } catch {
    return null;
  }
}

function verifySimulationError(
  value: unknown,
  layer: NegativeMutationEnforcementLayer,
  mutation: MutationName,
  compiledMessageSha256: string,
  canonicalMessageSha256: string,
  instructionCount: number,
  preContextSlot: number,
  postContextSlot: number,
): boolean {
  if (!isRecord(value)) return false;
  if (layer === "canonical pre-send verifier") {
    if (!hasExactKeys(value, ["kind", "classification", "messageSha256", "canonicalMessageSha256", "instructionCount"])) return false;
    return value.kind === "local-canonical-rejection"
      && value.classification === `canonical-${mutation}`
      && value.messageSha256 === compiledMessageSha256
      && value.canonicalMessageSha256 === canonicalMessageSha256
      && value.instructionCount === instructionCount;
  }
  if (!hasExactKeys(value, ["kind", "observation", "classification", "err", "logs", "logsSha256", "unitsConsumed", "contextSlot"])) return false;
  if (value.kind !== "confirmed-simulation-error" || value.observation !== "producer-observed-confirmed-rpc" || value.err === null || value.err === undefined || typeof value.classification !== "string" || value.classification.length === 0 || !Array.isArray(value.logs) || !value.logs.every((line) => typeof line === "string") || !isSha256(value.logsSha256) || !isUnitsConsumed(value.unitsConsumed) || !isPositiveContextSlot(value.contextSlot)) return false;
  const logs = value.logs as readonly string[];
  return value.logsSha256 === sha256(logs.join("\n"))
    && value.classification === classifyNegativeMutationSimulationError(value.err, logs)
    && value.contextSlot >= preContextSlot
    && value.contextSlot <= postContextSlot;
}

/** Independently rebuild each row's message and reject self-consistent lies. */
export function verifyNegativeMutationArtifact(value: unknown, artifact: RuntimePolicyArtifact, amountRaw: bigint): Readonly<{ pass: boolean; observed: unknown; expected: unknown }> {
  const expectedRoot = {
    schemaVersion: 1,
    evidenceType: "backyard-voltr-negative-mutations-confirmed",
    broadcast: false,
    generatorSourceSha256: negativeMutationGeneratorSourceSha256(),
    routeId: PARTNER_FOUR_MARKET_ROUTE.id,
    routeSpecSha256: fourMarketRouteSpecSha256(),
    lookupTable: "exact immutable PARTNER_ROUTE ALT identity and full ordered address list",
    mutations: "exact ordered matrix",
  };
  if (!isRecord(value) || !hasExactKeys(value, ["schemaVersion", "evidenceType", "broadcast", "generatorSourceSha256", "routeId", "routeSpecSha256", "lookupTable", "mutations"]) || value.schemaVersion !== 1 || value.evidenceType !== "backyard-voltr-negative-mutations-confirmed" || value.broadcast !== false || value.generatorSourceSha256 !== expectedRoot.generatorSourceSha256 || value.routeId !== PARTNER_FOUR_MARKET_ROUTE.id || value.routeSpecSha256 !== expectedRoot.routeSpecSha256) return { pass: false, observed: value, expected: expectedRoot };
  if (!Array.isArray(value.mutations)) return { pass: false, observed: { mutations: null }, expected: expectedRoot };
  const parsedLookup = parseLookupTable(value.lookupTable);
  if (!parsedLookup) return { pass: false, observed: { lookupTable: value.lookupTable }, expected: { lookupTable: expectedRoot.lookupTable, address: PARTNER_ROUTE.lookupTable.address, authority: PARTNER_FOUR_MARKET_ROUTE.lookupTable.authority, addressCount: PARTNER_FOUR_MARKET_ROUTE.lookupTable.addressCount, orderedAddressesSha256: PARTNER_FOUR_MARKET_ROUTE.lookupTable.orderedAddressesSha256 } };
  const lookupTable = parsedLookup.table;
  const rows = value.mutations;
  const expectedIds: string[] = [];
  const expectedHashes: string[] = [];
  let preContextSlotObserved: number | null = null;
  let postContextSlotObserved: number | null = null;
  let cursor = 0;
  try {
    for (const strategy of PARTNER_FOUR_MARKET_ROUTE.strategies) {
      for (const operation of ["deposit", "withdraw"] as const) {
        const entry = entryFor(artifact, strategy.id, operation);
        const baseline = sourceInstruction(artifact, strategy.id, operation, amountRaw);
        for (const mutation of mutationNames(operation, baseline.accounts.length)) {
          const generated = mutation === "extra-instruction" || mutation === "reordered-instruction" ? { inner: baseline, enforcementLayer: "canonical pre-send verifier" } : mutateInner(baseline, mutation, strategy.id, operation, amountRaw);
          const layer = expectedLayer(mutation);
          const row = rows[cursor++];
          const id = `${strategy.id}:${operation}:${mutation}`;
          expectedIds.push(id);
          if (!isRecord(row) || !hasExactKeys(row, ["id", "enforcementLayer", "recentBlockhash", "serializedMessageBase64", "serializedMessageSha256", "accepted", "broadcast", "simulationError", "preProtectedStateSha256", "postProtectedStateSha256", "preProtectedContextSlot", "postProtectedContextSlot"])) return { pass: false, observed: { index: cursor - 1, row }, expected: { id, schema: "exact negative mutation row" } };
          const recentBlockhash = typeof row.recentBlockhash === "string" ? row.recentBlockhash : "";
          if (!recentBlockhash || typeof row.serializedMessageBase64 !== "string" || row.serializedMessageBase64.length === 0 || !isSha256(row.serializedMessageSha256) || row.accepted !== false || row.broadcast !== false || !isSha256(row.preProtectedStateSha256) || !isSha256(row.postProtectedStateSha256) || row.preProtectedStateSha256 !== row.postProtectedStateSha256 || !isPositiveContextSlot(row.preProtectedContextSlot) || !isPositiveContextSlot(row.postProtectedContextSlot) || row.postProtectedContextSlot < row.preProtectedContextSlot || row.id !== id || row.enforcementLayer !== layer) return { pass: false, observed: { index: cursor - 1, row }, expected: { id, enforcementLayer: layer, accepted: false, broadcast: false, simulationError: "strict union error", protectedState: "unchanged", context: "post >= pre" } };
          const bytes = Buffer.from(row.serializedMessageBase64, "base64");
          if (preContextSlotObserved === null) preContextSlotObserved = row.preProtectedContextSlot;
          else if (preContextSlotObserved !== row.preProtectedContextSlot) return { pass: false, observed: { id, preProtectedContextSlot: row.preProtectedContextSlot }, expected: { id, preProtectedContextSlot: preContextSlotObserved } };
          if (layer === "canonical pre-send verifier") {
            if (row.postProtectedContextSlot !== row.preProtectedContextSlot) return { pass: false, observed: { id, postProtectedContextSlot: row.postProtectedContextSlot }, expected: { id, postProtectedContextSlot: row.preProtectedContextSlot } };
          } else {
            if (postContextSlotObserved !== null && row.postProtectedContextSlot < postContextSlotObserved) return { pass: false, observed: { id, postProtectedContextSlot: row.postProtectedContextSlot }, expected: { id, postProtectedContextSlot: `>=${postContextSlotObserved}` } };
            postContextSlotObserved = row.postProtectedContextSlot;
          }
          if (bytes.toString("base64") !== row.serializedMessageBase64 || sha256(bytes) !== row.serializedMessageSha256) return { pass: false, observed: { id, serializedMessageSha256: row.serializedMessageSha256 }, expected: { id, serializedMessageSha256: "SHA-256(serialized message bytes)" } };
          const compiled = compileMutation(entry, operation, generated.inner, mutation, amountRaw, recentBlockhash, lookupTable);
          const compiledPacketBytes = Buffer.from(compiled.transaction.serialize()).length;
          if (layer !== "canonical pre-send verifier" && compiledPacketBytes > SOLANA_PACKET_LIMIT_BYTES) return { pass: false, observed: { id, packetBytes: compiledPacketBytes }, expected: { id, packetBytes: `<=${SOLANA_PACKET_LIMIT_BYTES}` } };
          if (compiled.messageSha256 !== row.serializedMessageSha256 || compiled.messageBase64 !== row.serializedMessageBase64) return { pass: false, observed: { id, serializedMessageSha256: row.serializedMessageSha256 }, expected: { id, serializedMessageSha256: compiled.messageSha256 } };
          const canonical = compileCanonical(entry, operation, baseline, amountRaw, recentBlockhash, lookupTable);
          if (!verifySimulationError(row.simulationError, layer, mutation, compiled.messageSha256, canonical.messageSha256, compiled.transaction.message.compiledInstructions.length, row.preProtectedContextSlot, row.postProtectedContextSlot)) return { pass: false, observed: { id, simulationError: row.simulationError }, expected: { id, simulationError: layer === "canonical pre-send verifier" ? "exact local-canonical-rejection union" : "exact producer-observed-confirmed-rpc union" } };
          expectedHashes.push(compiled.messageSha256);
        }
      }
    }
  } catch (error) {
    return { pass: false, observed: error instanceof Error ? error.message : String(error), expected: "all canonical mutation packets reconstruct" };
  }
  const countExact = cursor === rows.length && rows.every((row, index) => isRecord(row) && row.id === expectedIds[index]);
  return { pass: countExact && new Set(expectedHashes).size === expectedHashes.length, observed: { count: rows.length, expectedCount: expectedIds.length, uniqueMessageHashes: new Set(expectedHashes).size, lookupTable: parsedLookup.identity }, expected: { count: expectedIds.length, orderedIds: expectedIds, uniqueMessageHashes: true, lookupTable: expectedRoot.lookupTable } };
}
