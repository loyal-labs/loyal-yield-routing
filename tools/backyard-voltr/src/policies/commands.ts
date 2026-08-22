import { createHash } from "node:crypto";
import { mkdirSync, writeFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { relative, resolve } from "node:path";

import { generated as squadsGenerated } from "@loyal-labs/loyal-smart-accounts-core";
import { AccountRole, address, createNoopSigner, type Instruction } from "@solana/kit";
import { Connection, PublicKey } from "@solana/web3.js";

import { assertIntentForRouteBinding, intentSha256, type SetupIntent } from "../domain/execution-intent.js";
import { PARTNER_FOUR_MARKET_ROUTE, PARTNER_ROUTE, fourMarketRouteSpecSha256, partnerBuilderRoute, type PartnerStrategyId } from "../domain/route-spec.js";
import { confirmedSnapshots, finalizedSnapshots, loadDeploymentIdentities, loadMainReserveGraph, prepareSignedV0Transaction, sendPreparedConfirmedOnce, type AccountSnapshot } from "../integrations/solana-compat.js";
import { signingMaterialFromEnvironment } from "../integrations/signer.js";
import { createVoltrRouteBuilder, deriveVoltrAccounts, type CanonicalInstruction } from "../integrations/voltr.js";
import { verifyDeploymentIdentities, type Gate } from "../verify/current.js";
import { verifyLegacyVoltrPolicyCatalog, verifyNonCatalogSquadsPoliciesIsolated } from "../verify/squads.js";
import { loadRuntimePolicyArtifact, type RuntimePolicyArtifactEntry } from "./compiler.js";
import { loadPolicyCatalogAuthorization } from "./authorization.js";

export type RuntimePolicyOperation = "deposit" | "withdraw";

const REPOSITORY_ROOT = resolve(fileURLToPath(new URL("../../../..", import.meta.url)));
const POLICY_INTENT_ROOT = resolve(REPOSITORY_ROOT, "docs/evidence/backyard-voltr-four-market/intents");

function requirePolicyIntentPath(value: string): string {
  const path = resolve(value);
  const relativePath = relative(POLICY_INTENT_ROOT, path);
  if (!relativePath || relativePath === ".." || relativePath.startsWith("../") || relativePath.startsWith("/")) {
    throw new Error(`policy install --intent-path must be inside ${relative(POLICY_INTENT_ROOT, POLICY_INTENT_ROOT) || "docs/evidence/backyard-voltr-four-market/intents"}`);
  }
  return path;
}

type DecodedSettings = Readonly<{ policySeed: { toString(): string } | null }>;
type DecodedPolicy = Readonly<{
  settings: PublicKey;
  seed: unknown;
  bump: number;
  transactionIndex: unknown;
  staleTransactionIndex: unknown;
  signers: readonly Readonly<{ key: PublicKey; permissions: Readonly<{ mask: number }> }>[];
  threshold: number;
  timeLock: number;
  policyState: unknown;
  start: unknown;
  expiration: unknown | null;
  rentCollector: PublicKey;
}>;

const POLICY_DISCRIMINATOR = Uint8Array.from([222, 135, 7, 163, 235, 177, 33, 68]);
const MAX_POLICY_INSTALL_LAMPORTS = 20_000_000;
const APPROVED_CONSTRAINED_ACCOUNT_INDEXES = {
  deposit: [0, 1, 2, 3, 4, 5, 6, 7, 8, 10, 11, 12, 13, 14, 15, 17, 21, 29, 30],
  withdraw: [0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 11, 12, 13, 14, 15, 17, 21, 26, 27],
} as const;
const SettingsAccount = (squadsGenerated as unknown as {
  Settings: { fromAccountInfo(account: ReturnType<typeof web3AccountInfo>): readonly [DecodedSettings, number] };
}).Settings;
const PolicyAccount = (squadsGenerated as unknown as {
  Policy: { fromAccountInfo(account: ReturnType<typeof web3AccountInfo>): readonly [DecodedPolicy, number] };
}).Policy;

function rpcUrl(): string {
  const value = process.env.SOLANA_RPC_URL;
  if (!value) throw new Error("SOLANA_RPC_URL is required");
  return value;
}

function add(gates: Gate[], name: string, pass: boolean, observed: unknown, expected: unknown): void {
  gates.push({ name, pass, observed, expected });
}

function sha256(data: ArrayLike<number>): string {
  return createHash("sha256").update(Uint8Array.from(data)).digest("hex");
}

function web3AccountInfo(snapshot: AccountSnapshot) {
  return {
    data: Buffer.from(snapshot.data),
    executable: snapshot.executable,
    lamports: snapshot.lamports,
    owner: new PublicKey(snapshot.owner),
    rentEpoch: 0,
  };
}

function settingsSeed(snapshot: AccountSnapshot | null): bigint | null {
  if (!snapshot || snapshot.owner !== PARTNER_ROUTE.squads.program) return null;
  const [settings] = SettingsAccount.fromAccountInfo(web3AccountInfo(snapshot));
  return BigInt(settings.policySeed?.toString() ?? "0");
}

function unsignedInteger(value: unknown, label: string): bigint {
  if (typeof value === "bigint") return value;
  if (typeof value === "number" && Number.isSafeInteger(value) && value >= 0) return BigInt(value);
  if (typeof value === "string" && /^[0-9]+$/.test(value)) return BigInt(value);
  if (value && typeof value === "object" && "toString" in value) {
    const encoded = value.toString();
    if (/^[0-9]+$/.test(encoded)) return BigInt(encoded);
  }
  throw new Error(`${label} is not an unsigned integer`);
}

function signedInteger(value: unknown, label: string): bigint {
  if (typeof value === "bigint") return value;
  if (typeof value === "number" && Number.isSafeInteger(value)) return BigInt(value);
  if (typeof value === "string" && /^-?[0-9]+$/.test(value)) return BigInt(value);
  if (value && typeof value === "object" && "toString" in value) {
    const encoded = value.toString();
    if (/^-?[0-9]+$/.test(encoded)) return BigInt(encoded);
  }
  throw new Error(`${label} is not a signed integer`);
}

function record(value: unknown, label: string): Record<string, unknown> {
  if (!value || typeof value !== "object" || Array.isArray(value)) {
    throw new Error(`${label} is not an object`);
  }
  return value as Record<string, unknown>;
}

function generatedPublicKey(value: unknown, label: string): string {
  if (typeof value === "string") return new PublicKey(value).toBase58();
  if (value && typeof value === "object") {
    const candidate = value as { toBase58?: () => string; toString?: () => string };
    if (candidate.toBase58) return new PublicKey(candidate.toBase58()).toBase58();
    if (candidate.toString) return new PublicKey(candidate.toString()).toBase58();
  }
  throw new Error(`${label} is not a public key`);
}

function generatedBytes(value: unknown, label: string): Buffer {
  if (value instanceof Uint8Array) return Buffer.from(value);
  if (Array.isArray(value) && value.every((entry) => Number.isInteger(entry) && Number(entry) >= 0 && Number(entry) <= 255)) {
    return Buffer.from(value as number[]);
  }
  if (value && typeof value === "object") {
    const entries = Object.entries(value as Record<string, unknown>)
      .filter(([key]) => /^[0-9]+$/.test(key))
      .sort(([left], [right]) => Number(left) - Number(right));
    if (entries.length > 0 && entries.every(([, entry]) => Number.isInteger(entry) && Number(entry) >= 0 && Number(entry) <= 255)) {
      return Buffer.from(entries.map(([, entry]) => Number(entry)));
    }
  }
  throw new Error(`${label} is not a byte sequence`);
}

function derivePolicyPda(seed: bigint): readonly [string, number] {
  const encodedSeed = Buffer.alloc(8);
  encodedSeed.writeBigUInt64LE(seed);
  const [policy, bump] = PublicKey.findProgramAddressSync(
    [
      Buffer.from("smart_account"),
      Buffer.from("policy"),
      new PublicKey(PARTNER_ROUTE.squads.settings).toBuffer(),
      encodedSeed,
    ],
    new PublicKey(PARTNER_ROUTE.squads.program),
  );
  return [policy.toBase58(), bump] as const;
}

type ExpectedInnerInstruction = Readonly<{
  programId: string;
  dataHex?: string;
  dataBase64: string;
  dataSha256: string;
  dataLength: number;
  accounts: readonly Readonly<{
    index: number;
    label?: string;
    address: string;
    signer?: boolean;
    writable?: boolean;
  }>[];
}>;

type PolicySemanticReadback = Readonly<{
  gates: readonly Gate[];
  decodedLength: number | null;
  transactionIndex: bigint | null;
  staleTransactionIndex: bigint | null;
}>;

function sameJson(left: unknown, right: unknown): boolean {
  return JSON.stringify(left, (_key, value) => typeof value === "bigint" ? value.toString() : value)
    === JSON.stringify(right, (_key, value) => typeof value === "bigint" ? value.toString() : value);
}

function strategyForEntry(entry: RuntimePolicyArtifactEntry): PartnerStrategyId {
  if (entry.strategyId === "main" || entry.strategyId === "onre" || entry.strategyId === "prime" || entry.strategyId === "maple") return entry.strategyId;
  throw new Error(`policy ${entry.operation} is missing its exact strategy id`);
}

function sourceManifestForEntry(artifact: ReturnType<typeof loadRuntimePolicyArtifact>["artifact"], strategyId: PartnerStrategyId) {
  if (Array.isArray(artifact.sourceManifests)) {
    const manifest = artifact.sourceManifests.find((candidate) => candidate.strategyId === strategyId);
    if (manifest) return manifest;
  }
  if (strategyId === "main" && (artifact.sourceManifest.strategyId === "main" || artifact.runtimePolicyCount === 2)) return artifact.sourceManifest;
  throw new Error(`policy artifact has no source manifest for ${strategyId}`);
}

async function canonicalRuntimeInstructions(strategyId: PartnerStrategyId): Promise<Readonly<Record<RuntimePolicyOperation, CanonicalInstruction>>> {
  const strategyRoute = partnerBuilderRoute(strategyId);
  const accounts = await deriveVoltrAccounts(strategyRoute);
  const reserve = await loadMainReserveGraph(rpcUrl(), strategyRoute, accounts.strategyAuth);
  const builder = await createVoltrRouteBuilder(strategyRoute, reserve.graph);
  const manager = createNoopSigner(strategyRoute.squads.manager);
  const [deposit, withdraw] = await Promise.all([
    builder.strategy.deposit(manager, strategyRoute.asset.proofAmountRaw),
    builder.strategy.withdraw(manager, strategyRoute.asset.proofAmountRaw),
  ]);
  return { deposit: deposit.canonical, withdraw: withdraw.canonical };
}

function canonicalInstructionView(instruction: CanonicalInstruction): Readonly<{
  programId: string;
  dataHex: string;
  dataBase64: string;
  dataSha256: string;
  dataLength: number;
  accounts: readonly Readonly<{
    index: number;
    label: string;
    address: string;
    signer: boolean;
    writable: boolean;
  }>[];
}> {
  return {
    programId: instruction.programId,
    dataHex: Buffer.from(instruction.data).toString("hex"),
    dataBase64: instruction.dataBase64,
    dataSha256: instruction.dataSha256,
    dataLength: instruction.dataLength,
    accounts: instruction.accounts.map(({ index, label, address: accountAddress, signer, writable }) => ({
      index,
      label,
      address: accountAddress,
      signer,
      writable,
    })),
  };
}

function artifactLinkageGates(input: Readonly<{
  operation: RuntimePolicyOperation;
  entry: RuntimePolicyArtifactEntry;
  source: ExpectedInnerInstruction;
  canonical: CanonicalInstruction;
}>): readonly Gate[] {
  const { operation, entry, source, canonical } = input;
  const gates: Gate[] = [];
  const approvedIndexes = [...APPROVED_CONSTRAINED_ACCOUNT_INDEXES[operation]];
  const sourceView = {
    programId: source.programId,
    dataHex: source.dataHex,
    dataBase64: source.dataBase64,
    dataSha256: source.dataSha256,
    dataLength: source.dataLength,
    accounts: source.accounts,
  };
  const expectedView = canonicalInstructionView(canonical);
  add(gates, "artifact constrained indexes are exact, ordered, and unique", sameJson(entry.constrainedAccountIndexes, approvedIndexes) && new Set(entry.constrainedAccountIndexes).size === approvedIndexes.length, entry.constrainedAccountIndexes, approvedIndexes);
  add(gates, "source manifest instruction equals current SDK route", sameJson(sourceView, expectedView), sourceView, expectedView);
  add(gates, "artifact inner hash equals current SDK instruction", entry.innerInstructionDataSha256 === canonical.dataSha256, entry.innerInstructionDataSha256, canonical.dataSha256);
  add(gates, "runtime instruction has exact 30-byte bounded-amount layout", canonical.dataLength === 30 && canonical.data.length === 30, canonical.dataLength, 30);

  const expectedPolicyCreateAccounts = [
    { address: PARTNER_ROUTE.squads.settings, signer: false, writable: true },
    { address: PARTNER_ROUTE.setupAdmin, signer: true, writable: true },
    { address: PARTNER_ROUTE.programs.system, signer: false, writable: false },
    { address: PARTNER_ROUTE.squads.program, signer: false, writable: false },
    { address: PARTNER_ROUTE.setupAdmin, signer: true, writable: false },
    { address: entry.policy, signer: false, writable: true },
  ];
  add(gates, "policy-create account addresses and instruction roles are exact", sameJson(entry.policyCreate.accounts, expectedPolicyCreateAccounts), entry.policyCreate.accounts, expectedPolicyCreateAccounts);

  const wrapperData = Buffer.from(entry.managerExecution.dataBase64, "base64");
  const wrapperAccounts = entry.managerExecution.accounts;
  add(
    gates,
    "manager wrapper bytes are self-consistent",
    wrapperData.length === entry.managerExecution.dataLength
      && wrapperData.toString("base64") === entry.managerExecution.dataBase64
      && sha256(wrapperData) === entry.managerExecution.dataSha256,
    { dataLength: wrapperData.length, dataSha256: sha256(wrapperData) },
    { dataLength: entry.managerExecution.dataLength, dataSha256: entry.managerExecution.dataSha256 },
  );
  add(gates, "manager wrapper program is Squads", entry.managerExecution.programId === PARTNER_ROUTE.squads.program, entry.managerExecution.programId, PARTNER_ROUTE.squads.program);

  const transactionAccounts: { address: string; signer: false; writable: boolean }[] = [];
  const appendTransactionAccount = (accountAddress: string, writable: boolean) => {
    const existing = transactionAccounts.find((account) => account.address === accountAddress);
    if (existing) {
      existing.writable ||= writable;
    } else {
      transactionAccounts.push({ address: accountAddress, signer: false, writable });
    }
  };
  canonical.accounts.forEach((account) => appendTransactionAccount(account.address, account.writable));
  appendTransactionAccount(canonical.programId, false);
  const expectedWrapperAccounts = [
    { address: entry.policy, signer: false, writable: true },
    { address: PARTNER_ROUTE.squads.program, signer: false, writable: false },
    { address: PARTNER_ROUTE.squads.guardian, signer: true, writable: false },
    ...transactionAccounts,
  ];
  add(
    gates,
    "manager wrapper accounts and instruction roles equal current SDK graph",
    sameJson(wrapperAccounts, expectedWrapperAccounts),
    wrapperAccounts,
    expectedWrapperAccounts,
  );
  const wrapperInnerOffset = wrapperData.length - canonical.dataLength;
  add(
    gates,
    "manager wrapper embeds the exact current inner instruction",
    wrapperInnerOffset >= 2
      && wrapperData.readUInt16LE(wrapperInnerOffset - 2) === canonical.dataLength
      && wrapperData.subarray(wrapperInnerOffset).equals(Buffer.from(canonical.data)),
    wrapperInnerOffset >= 0
      ? { encodedLength: wrapperInnerOffset >= 2 ? wrapperData.readUInt16LE(wrapperInnerOffset - 2) : null, innerDataSha256: sha256(wrapperData.subarray(Math.max(0, wrapperInnerOffset))) }
      : null,
    { encodedLength: canonical.dataLength, innerDataSha256: canonical.dataSha256 },
  );
  return gates;
}

/**
 * Decode the Squads account and compare policy meaning, not mutable raw bytes.
 * Creation readbacks require zero counters; durable current-state checks only
 * require Squads' mutable staleTransactionIndex <= transactionIndex invariant.
 */
function semanticPolicyReadback(input: Readonly<{
  operation: RuntimePolicyOperation;
  entry: RuntimePolicyArtifactEntry;
  inner: ExpectedInnerInstruction;
  account: AccountSnapshot | null;
  requireZeroCounters: boolean;
}>): PolicySemanticReadback {
  const { operation, entry, inner, account, requireZeroCounters } = input;
  const gates: Gate[] = [];
  add(gates, "policy exists", account !== null, account?.address ?? null, entry.policy);
  if (!account) return { gates, decodedLength: null, transactionIndex: null, staleTransactionIndex: null };
  add(gates, "policy address", account.address === entry.policy, account.address, entry.policy);
  add(gates, "policy owner", account.owner === PARTNER_ROUTE.squads.program, account.owner, PARTNER_ROUTE.squads.program);
  add(
    gates,
    "policy discriminator",
    Buffer.from(account.data.subarray(0, POLICY_DISCRIMINATOR.length)).equals(Buffer.from(POLICY_DISCRIMINATOR)),
    Buffer.from(account.data.subarray(0, POLICY_DISCRIMINATOR.length)).toString("hex"),
    Buffer.from(POLICY_DISCRIMINATOR).toString("hex"),
  );

  let decoded: DecodedPolicy;
  let decodedLength: number;
  try {
    [decoded, decodedLength] = PolicyAccount.fromAccountInfo(web3AccountInfo(account));
    add(gates, "policy generated decoder", true, { decodedLength, accountDataLength: account.data.length }, "decoded");
  } catch (error) {
    add(gates, "policy generated decoder", false, error instanceof Error ? error.message : String(error), "decoded");
    return { gates, decodedLength: null, transactionIndex: null, staleTransactionIndex: null };
  }

  const seed = unsignedInteger(decoded.seed, "policy seed");
  const transactionIndex = unsignedInteger(decoded.transactionIndex, "policy transaction index");
  const staleTransactionIndex = unsignedInteger(decoded.staleTransactionIndex, "policy stale transaction index");
  const expectedSeed = BigInt(entry.seed);
  const [derivedPolicy, expectedBump] = derivePolicyPda(expectedSeed);
  add(gates, "policy Settings", decoded.settings.toBase58() === PARTNER_ROUTE.squads.settings, decoded.settings.toBase58(), PARTNER_ROUTE.squads.settings);
  add(gates, "policy seed", seed === expectedSeed, seed, expectedSeed);
  add(gates, "policy PDA derived from Settings and seed", derivedPolicy === entry.policy, derivedPolicy, entry.policy);
  add(gates, "policy bump", decoded.bump === expectedBump, decoded.bump, expectedBump);
  if (requireZeroCounters) {
    add(gates, "new policy transaction index", transactionIndex === 0n, transactionIndex, 0n);
    add(gates, "new policy stale transaction index", staleTransactionIndex === 0n, staleTransactionIndex, 0n);
  } else {
    add(
      gates,
      "policy mutable counters remain ordered",
      staleTransactionIndex <= transactionIndex,
      { transactionIndex, staleTransactionIndex },
      "staleTransactionIndex <= transactionIndex",
    );
  }
  add(gates, "policy signer count", decoded.signers.length === 1, decoded.signers.length, 1);
  const guardian = decoded.signers[0];
  add(gates, "policy delegated guardian", guardian?.key.toBase58() === PARTNER_ROUTE.squads.guardian, guardian?.key.toBase58() ?? null, PARTNER_ROUTE.squads.guardian);
  add(gates, "policy guardian permissions", guardian?.permissions.mask === PARTNER_ROUTE.squads.guardianPermissionsMask, guardian?.permissions.mask ?? null, PARTNER_ROUTE.squads.guardianPermissionsMask);
  add(gates, "policy threshold", decoded.threshold === PARTNER_ROUTE.squads.threshold, decoded.threshold, PARTNER_ROUTE.squads.threshold);
  add(gates, "policy timelock", decoded.timeLock === 0, decoded.timeLock, 0);
  add(gates, "policy start initialized", signedInteger(decoded.start, "policy start") > 0n, signedInteger(decoded.start, "policy start"), "> 0");
  add(gates, "policy has no expiration", decoded.expiration === null, decoded.expiration, null);
  add(gates, "policy rent collector", decoded.rentCollector.toBase58() === PARTNER_ROUTE.setupAdmin, decoded.rentCollector.toBase58(), PARTNER_ROUTE.setupAdmin);
  add(
    gates,
    "policy unused allocation is zeroed",
    decodedLength <= account.data.length && account.data.subarray(decodedLength).every((byte) => byte === 0),
    { decodedLength, accountDataLength: account.data.length },
    "all trailing allocation bytes are zero",
  );

  try {
    const state = record(decoded.policyState, "policy state");
    add(gates, "policy state kind", state.__kind === "ProgramInteraction", state.__kind, "ProgramInteraction");
    const stateFields = Array.isArray(state.fields) ? state.fields : [];
    add(gates, "policy state payload count", stateFields.length === 1, stateFields.length, 1);
    const interaction = record(stateFields[0], "ProgramInteraction policy");
    add(gates, "policy vault index", interaction.accountIndex === PARTNER_ROUTE.squads.vaultIndex, interaction.accountIndex, PARTNER_ROUTE.squads.vaultIndex);
    add(gates, "policy pre-hook disabled", interaction.preHook === null, interaction.preHook, null);
    add(gates, "policy post-hook disabled", interaction.postHook === null, interaction.postHook, null);
    add(gates, "policy spending limits empty", Array.isArray(interaction.spendingLimits) && interaction.spendingLimits.length === 0, interaction.spendingLimits, []);

    const instructionConstraints = Array.isArray(interaction.instructionsConstraints) ? interaction.instructionsConstraints : [];
    add(gates, "policy instruction constraint count", instructionConstraints.length === 1, instructionConstraints.length, 1);
    const instructionConstraint = record(instructionConstraints[0], `${operation} instruction constraint`);
    const outerProgram = generatedPublicKey(instructionConstraint.programId, "policy outer program");
    add(gates, "policy outer program is Voltr", outerProgram === PARTNER_ROUTE.programs.voltrVault && outerProgram === inner.programId, outerProgram, inner.programId);

    const accountConstraints = Array.isArray(instructionConstraint.accountConstraints) ? instructionConstraint.accountConstraints : [];
    add(gates, "policy account constraint count", accountConstraints.length === entry.constrainedAccountIndexes.length, accountConstraints.length, entry.constrainedAccountIndexes.length);
    for (const [position, expectedIndex] of entry.constrainedAccountIndexes.entries()) {
      const constraint = record(accountConstraints[position], `${operation} account constraint ${position}`);
      const payload = record(constraint.accountConstraint, `${operation} account constraint ${position} payload`);
      const fields = Array.isArray(payload.fields) ? payload.fields : [];
      const allowed = Array.isArray(fields[0]) ? fields[0] : [];
      const observedKeys = allowed.map((value, index) => generatedPublicKey(value, `${operation} account constraint ${position} pubkey ${index}`));
      const expectedAddress = inner.accounts[expectedIndex]?.address ?? null;
      add(
        gates,
        `policy account constraint ${position}`,
        constraint.accountIndex === expectedIndex
          && payload.__kind === "Pubkey"
          && constraint.owner === null
          && fields.length === 1
          && observedKeys.length === 1
          && expectedAddress !== null
          && observedKeys[0] === expectedAddress,
        { accountIndex: constraint.accountIndex, kind: payload.__kind, owner: constraint.owner, pubkeys: observedKeys },
        { accountIndex: expectedIndex, kind: "Pubkey", owner: null, pubkeys: [expectedAddress] },
      );
    }

    const expectedData = Buffer.from(inner.dataBase64, "base64");
    add(gates, "inner instruction encoding", expectedData.length === inner.dataLength && sha256(expectedData) === inner.dataSha256 && expectedData.length >= 16, { dataLength: expectedData.length, dataSha256: sha256(expectedData) }, { dataLength: inner.dataLength, dataSha256: inner.dataSha256 });
    const expectedConstraints = [
      { offset: 0n, kind: "U8Slice", operator: 0, value: expectedData.subarray(0, 8) },
      { offset: 8n, kind: "U64Le", operator: 2, value: 0n },
      { offset: 8n, kind: "U64Le", operator: 5, value: PARTNER_ROUTE.asset.maxManagerOperationRaw },
      { offset: 16n, kind: "U8Slice", operator: 0, value: expectedData.subarray(16) },
    ] as const;
    const dataConstraints = Array.isArray(instructionConstraint.dataConstraints) ? instructionConstraint.dataConstraints : [];
    add(gates, "policy bounded data constraint count", dataConstraints.length === expectedConstraints.length, dataConstraints.length, expectedConstraints.length);
    for (const [index, expected] of expectedConstraints.entries()) {
      const constraint = record(dataConstraints[index], `${operation} data constraint ${index}`);
      const dataValue = record(constraint.dataValue, `${operation} data value ${index}`);
      const fields = Array.isArray(dataValue.fields) ? dataValue.fields : [];
      const offset = unsignedInteger(constraint.dataOffset, `${operation} data offset ${index}`);
      const observed = expected.kind === "U8Slice"
        ? generatedBytes(fields[0], `${operation} constrained bytes ${index}`)
        : unsignedInteger(fields[0], `${operation} constrained integer ${index}`);
      const valueMatches = Buffer.isBuffer(expected.value)
        ? Buffer.isBuffer(observed) && observed.equals(expected.value)
        : typeof observed === "bigint" && observed === expected.value;
      add(
        gates,
        `policy data constraint ${index}`,
        offset === expected.offset
          && dataValue.__kind === expected.kind
          && fields.length === 1
          && constraint.operator === expected.operator
          && valueMatches,
        { offset, kind: dataValue.__kind, operator: constraint.operator, value: Buffer.isBuffer(observed) ? { dataLength: observed.length, dataSha256: sha256(observed) } : observed },
        { offset: expected.offset, kind: expected.kind, operator: expected.operator, value: Buffer.isBuffer(expected.value) ? { dataLength: expected.value.length, dataSha256: sha256(expected.value) } : expected.value },
      );
    }
  } catch (error) {
    add(gates, "ProgramInteraction semantic decode", false, error instanceof Error ? error.message : String(error), "exact approved ProgramInteraction policy");
  }

  return { gates, decodedLength, transactionIndex, staleTransactionIndex };
}

function artifactInstruction(entry: RuntimePolicyArtifactEntry): Instruction {
  const data = Buffer.from(entry.policyCreate.dataBase64, "base64");
  if (data.length !== entry.policyCreate.dataLength || sha256(data) !== entry.policyCreate.dataSha256) {
    throw new Error("policy-create bytes do not match the verified artifact");
  }
  return {
    programAddress: address(entry.policyCreate.programId),
    accounts: entry.policyCreate.accounts.map((account) => ({
      address: address(account.address),
      role: account.signer
        ? account.writable ? AccountRole.WRITABLE_SIGNER : AccountRole.READONLY_SIGNER
        : account.writable ? AccountRole.WRITABLE : AccountRole.READONLY,
    })),
    data,
  };
}

async function catalogPrefixSuffixGates(input: Readonly<{
  artifact: ReturnType<typeof loadRuntimePolicyArtifact>["artifact"];
  canonical: ReadonlyMap<PartnerStrategyId, Readonly<Record<RuntimePolicyOperation, CanonicalInstruction>>>;
  state: Readonly<{ contextSlot: number; accounts: readonly (AccountSnapshot | null)[] }>;
  targetIndex: number;
  expectedBefore: bigint;
}>): Promise<readonly Gate[]> {
  const gates: Gate[] = [];
  const settings = input.state.accounts[0] ?? null;
  add(gates, "policy catalog Settings seed is the exact predecessor", settingsSeed(settings) === input.expectedBefore, settingsSeed(settings), input.expectedBefore);
  for (const [index, entry] of input.artifact.policies.entries()) {
    const account = input.state.accounts[index + 1] ?? null;
    const strategyId = strategyForEntry(entry);
    const canonical = input.canonical.get(strategyId)?.[entry.operation];
    if (!canonical) throw new Error(`missing canonical ${strategyId} ${entry.operation} instruction`);
    if (index < input.targetIndex) {
      const semantic = semanticPolicyReadback({
        operation: entry.operation,
        entry,
        inner: canonicalInstructionView(canonical),
        account,
        requireZeroCounters: false,
      });
      gates.push(...semantic.gates.map((gate) => ({ ...gate, name: `catalog prefix ${index} ${strategyId} ${entry.operation}: ${gate.name}` })));
    } else if (index === input.targetIndex) {
      add(gates, `catalog target ${index} is absent before send`, account === null, account?.address ?? null, null);
    } else {
      add(gates, `catalog suffix ${index} remains absent before send`, account === null, account?.address ?? null, null);
    }
  }
  return gates;
}

async function prepareRuntimePolicyInstall(
  strategyId: PartnerStrategyId,
  operation: RuntimePolicyOperation,
  artifactPath: string,
) {
  const route = PARTNER_ROUTE;
  const loaded = loadRuntimePolicyArtifact(artifactPath);
  const entry = loaded.artifact.policies.find((policy) => strategyForEntry(policy) === strategyId && policy.operation === operation);
  if (!entry) throw new Error(`verified artifact has no ${strategyId} ${operation} policy`);
  const targetIndex = loaded.artifact.policies.indexOf(entry);
  const expectedSeed = BigInt(entry.seed);
  const expectedBefore = expectedSeed - 1n;
  const canonicalEntries = await Promise.all((["main", "onre", "prime", "maple"] as const).map(async (id) => [id, await canonicalRuntimeInstructions(id)] as const));
  const canonical = new Map<PartnerStrategyId, Readonly<Record<RuntimePolicyOperation, CanonicalInstruction>>>(canonicalEntries);
  const canonicalInner = canonical.get(strategyId)![operation];
  const sourceInner = sourceManifestForEntry(loaded.artifact, strategyId).instructions[operation];
  const linkageGates = artifactLinkageGates({ operation, entry, source: sourceInner, canonical: canonicalInner });
  const allPolicyAddresses = loaded.artifact.policies.map(({ policy }) => policy);
  const before = await confirmedSnapshots(rpcUrl(), [route.squads.settings, ...allPolicyAddresses, route.setupAdmin]);
  const catalogGates = await catalogPrefixSuffixGates({ artifact: loaded.artifact, canonical, state: before, targetIndex, expectedBefore });
  const [legacyCatalog, squadsIsolation] = await Promise.all([
    verifyLegacyVoltrPolicyCatalog(rpcUrl(), before.contextSlot, "confirmed"),
    verifyNonCatalogSquadsPoliciesIsolated(rpcUrl(), BigInt(loaded.artifact.policies[0]!.seed), BigInt(loaded.artifact.policies.at(-1)!.seed), before.contextSlot, "confirmed", [{ firstSeed: 17n, lastSeed: 24n }], false),
  ]);
  const admin = await signingMaterialFromEnvironment("SOLANA_TESTING_PK");
  if (admin.signer.address !== route.setupAdmin) throw new Error("policy installer is not the RouteSpec setup admin");
  const instruction = artifactInstruction(entry);
  const inspectedAddresses = [route.squads.settings, entry.policy, route.setupAdmin];
  const prepared = await prepareSignedV0Transaction({
    rpcUrl: rpcUrl(),
    feePayer: admin,
    instructions: [instruction],
    inspectedAddresses,
    minimumContextSlot: before.contextSlot,
    commitment: "confirmed",
  });
  const settingsAfter = prepared.simulation.postAccounts[0] ?? null;
  const policyAfter = prepared.simulation.postAccounts[1] ?? null;
  const adminAfter = prepared.simulation.postAccounts[2] ?? null;
  const simulatedPolicy = semanticPolicyReadback({
    operation,
    entry,
    inner: canonicalInstructionView(canonicalInner),
    account: policyAfter,
    requireZeroCounters: true,
  });
  const gates: Gate[] = [];
  gates.push(...catalogGates);
  gates.push(...linkageGates.map((gate) => ({ ...gate, name: `artifact linkage: ${gate.name}` })));
  add(gates, "artifact file hash is fixed", /^[0-9a-f]{64}$/.test(loaded.fileSha256), loaded.fileSha256, "lowercase SHA-256");
  add(gates, "immutable legacy Voltr catalog remains exact", legacyCatalog.verdict === "PARTNER_LEGACY_VOLTR_POLICIES_CONFIRMED_PASS", { verdict: legacyCatalog.verdict, failedGateCount: legacyCatalog.failedGateCount }, { verdict: "PARTNER_LEGACY_VOLTR_POLICIES_CONFIRMED_PASS", failedGateCount: 0 });
  add(gates, "all policies outside legacy and current catalogs remain isolated", squadsIsolation.verdict === "PARTNER_NON_CATALOG_SQUADS_ISOLATION_PASS", { verdict: squadsIsolation.verdict, failedGateCount: squadsIsolation.failedGateCount }, { verdict: "PARTNER_NON_CATALOG_SQUADS_ISOLATION_PASS", failedGateCount: 0 });
  add(gates, "settings owner and current seed exact", settingsSeed(before.accounts[0] ?? null) === expectedBefore, settingsSeed(before.accounts[0] ?? null), expectedBefore);
  add(gates, "target policy account is absent", before.accounts[targetIndex + 1] === null, before.accounts[targetIndex + 1]?.address ?? null, null);
  add(gates, "one exact Squads policy-create instruction", instruction.programAddress === route.squads.program && instruction.accounts?.length === 6 && sha256(instruction.data ?? new Uint8Array()) === entry.policyCreate.dataSha256, { programId: instruction.programAddress, accountCount: instruction.accounts?.length ?? 0, dataSha256: sha256(instruction.data ?? new Uint8Array()) }, { programId: route.squads.program, accountCount: 6, dataSha256: entry.policyCreate.dataSha256 });
  add(gates, "simulation succeeded", prepared.simulation.err === null, prepared.simulation.err, null);
  add(gates, "settings advanced by exactly one", settingsSeed(settingsAfter) === expectedSeed, settingsSeed(settingsAfter), expectedSeed);
  gates.push(...simulatedPolicy.gates.map((gate) => ({ ...gate, name: `simulated policy: ${gate.name}` })));
  const simulatedPolicyDataSha256 = policyAfter ? sha256(policyAfter.data) : null;
  const adminBefore = before.accounts[before.accounts.length - 1] ?? null;
  const adminSpend = adminBefore && adminAfter ? adminBefore.lamports - adminAfter.lamports : null;
  add(gates, "policy rent plus fee bounded", adminSpend !== null && adminSpend >= prepared.feeLamports && adminSpend <= MAX_POLICY_INSTALL_LAMPORTS, adminSpend, `${prepared.feeLamports}..${MAX_POLICY_INSTALL_LAMPORTS}`);
  add(gates, "compiled packet matches artifact bound", prepared.packetBytes === entry.policyCreatePacketBytes, prepared.packetBytes, entry.policyCreatePacketBytes);
  const canonicalMessageSha256 = sha256(prepared.serializedMessage);
  const intent: SetupIntent = {
    schemaVersion: 1,
    kind: "setup",
    operation: operation === "deposit" ? "install-deposit-policy" : "install-withdraw-policy",
    routeId: PARTNER_FOUR_MARKET_ROUTE.id,
    routeSpecSha256: fourMarketRouteSpecSha256(),
    signer: route.setupAdmin,
    nonce: `install-${operation}-policy:${entry.policy}`,
    prestateSlot: BigInt(prepared.prestateSlot),
    expiresAtUnix: BigInt(Math.floor(Date.now() / 1_000) + 300),
    canonicalMessageSha256,
  };
  assertIntentForRouteBinding(intent, {
    routeId: PARTNER_FOUR_MARKET_ROUTE.id,
    routeSpecSha256: fourMarketRouteSpecSha256(),
    maxManagerOperationRaw: PARTNER_ROUTE.asset.maxManagerOperationRaw,
  });
  const intentDigest = intentSha256(intent);
  const failedGateCount = gates.filter(({ pass }) => !pass).length;
  return {
    loaded,
    entry,
    expectedBefore,
    expectedSeed,
    canonicalInner,
    simulatedPolicyDataSha256,
    prestateSettingsDataSha256: before.accounts[0] ? sha256(before.accounts[0].data) : null,
    intent,
    intentSha256: intentDigest,
    prepared,
    report: {
      verdict: failedGateCount === 0 ? "PARTNER_RUNTIME_POLICY_INSTALL_SIMULATION_PASS" : "PARTNER_RUNTIME_POLICY_INSTALL_SIMULATION_FAIL",
      broadcast: false,
      readyForBroadcast: failedGateCount === 0,
      routeSpecSha256: fourMarketRouteSpecSha256(),
      artifact: { path: loaded.path, fileSha256: loaded.fileSha256, artifactSha256: loaded.artifact.artifactSha256, sourceManifestSha256: loaded.artifact.sourceManifestSha256 },
      transaction: { operation: `install-${operation}-policy`, seed: entry.seed, policy: entry.policy, packetBytes: prepared.packetBytes, feeLamports: prepared.feeLamports, expectedRentLamports: adminSpend === null ? null : adminSpend - prepared.feeLamports, maxTotalLamports: MAX_POLICY_INSTALL_LAMPORTS, expectedSignature: prepared.expectedSignature, policyCreateDataSha256: entry.policyCreate.dataSha256, simulatedPolicyDataSha256, canonicalMessageSha256 },
      simulation: { prestateSlot: prepared.prestateSlot, contextSlot: prepared.simulationSlot, err: prepared.simulation.err, unitsConsumed: prepared.simulation.unitsConsumed },
      squadsIsolation: { contextSlot: squadsIsolation.contextSlot, nonCatalogPolicies: squadsIsolation.policies, legacyCatalogContextSlot: legacyCatalog.contextSlot },
      failedGateCount,
      gates,
    },
    targetIndex,
    canonicalByStrategy: canonical,
    prestatePolicyAddresses: allPolicyAddresses,
  } as const;
}

export async function simulateRuntimePolicyInstall(strategyId: PartnerStrategyId, operation: RuntimePolicyOperation, artifactPath: string) {
  return (await prepareRuntimePolicyInstall(strategyId, operation, artifactPath)).report;
}

export async function verifyExistingRuntimePolicies(
  artifactPath: string,
  minContextSlot?: number,
  commitment: "confirmed" | "finalized" = "finalized",
  additionalCatalogRanges: readonly Readonly<{ firstSeed: bigint; lastSeed: bigint }>[] = [],
) {
  if (minContextSlot !== undefined && (!Number.isSafeInteger(minContextSlot) || minContextSlot < 0)) {
    throw new Error(`runtime policy minimum context slot must be a non-negative safe integer: ${minContextSlot}`);
  }
  const snapshotAtCommitment = async (addresses: readonly string[]) => {
    for (let attempt = 0; attempt < 5; attempt += 1) {
      try {
        return commitment === "confirmed"
          ? await confirmedSnapshots(rpcUrl(), addresses, minContextSlot)
          : await finalizedSnapshots(rpcUrl(), addresses, minContextSlot);
      } catch (error) {
        const message = error instanceof Error ? error.message : String(error);
        if (!message.toLowerCase().includes("minimum context slot") || attempt === 4) throw error;
        await new Promise((resolve) => setTimeout(resolve, 200));
      }
    }
    throw new Error("unreachable policy snapshot retry state");
  };
  const loaded = loadRuntimePolicyArtifact(artifactPath);
  const rpc = rpcUrl();
  const connection = new Connection(rpc, commitment);
  const policyAddresses = loaded.artifact.policies.map(({ policy }) => new PublicKey(policy));
  const canonicalEntries = await Promise.all((["main", "onre", "prime", "maple"] as const).map(async (id) => [id, await canonicalRuntimeInstructions(id)] as const));
  const canonical = new Map<PartnerStrategyId, Readonly<Record<RuntimePolicyOperation, CanonicalInstruction>>>(canonicalEntries);
  const [genesisHash, state] = await Promise.all([
    connection.getGenesisHash(),
    snapshotAtCommitment([PARTNER_ROUTE.squads.settings, ...loaded.artifact.policies.map(({ policy }) => policy)]),
  ]);
  if (genesisHash !== PARTNER_ROUTE.genesisHash) {
    throw new Error(`refusing non-mainnet genesis ${genesisHash}`);
  }
  const deployments = await loadDeploymentIdentities(rpc, PARTNER_ROUTE, state.contextSlot, commitment);
  const terminalSeed = BigInt(loaded.artifact.policies.at(-1)?.seed ?? "0");
  const catalogFirstSeed = BigInt(loaded.artifact.policies[0]?.seed ?? "0");
  const effectiveAdditionalCatalogRanges = additionalCatalogRanges.length > 0
    ? additionalCatalogRanges
    : catalogFirstSeed === 43n ? [{ firstSeed: 17n, lastSeed: 24n }] : [];
  const nonCatalogIsolation = await verifyNonCatalogSquadsPoliciesIsolated(rpc, catalogFirstSeed, terminalSeed, state.contextSlot, commitment, effectiveAdditionalCatalogRanges);
  const gates: Gate[] = [];
  add(gates, "runtime policy state context reaches requested minimum", minContextSlot === undefined || state.contextSlot >= minContextSlot, state.contextSlot, minContextSlot === undefined ? "current finalized" : `>=${minContextSlot}`);
  add(gates, "runtime policy deployment context reaches policy state", deployments.contextSlot >= state.contextSlot, deployments.contextSlot, `>=${state.contextSlot}`);
  gates.push(...verifyDeploymentIdentities(PARTNER_ROUTE, deployments.identities).map((gate) => ({ ...gate, name: `runtime policy deployment: ${gate.name}` })));
  add(gates, "Settings includes the complete runtime-policy catalog", settingsSeed(state.accounts[0] ?? null) !== null && settingsSeed(state.accounts[0] ?? null)! >= terminalSeed, settingsSeed(state.accounts[0] ?? null), `>=${terminalSeed}`);
  add(gates, "Settings seed matches the stable non-catalog isolation scan", settingsSeed(state.accounts[0] ?? null) === nonCatalogIsolation.currentSeed, { catalogSnapshot: settingsSeed(state.accounts[0] ?? null), isolationScan: nonCatalogIsolation.currentSeed }, "equal");
  gates.push(...nonCatalogIsolation.gates.map((gate) => ({ ...gate, name: `Squads isolation: ${gate.name}` })));
  const policies = [];
  for (let index = 0; index < loaded.artifact.policies.length; index += 1) {
    const entry = loaded.artifact.policies[index]!;
    const account = state.accounts[index + 1] ?? null;
    const strategyId = strategyForEntry(entry);
    const canonicalInstruction = canonical.get(strategyId)?.[entry.operation];
    if (!canonicalInstruction) throw new Error(`missing canonical ${strategyId} ${entry.operation} instruction`);
    const linkage = artifactLinkageGates({
      operation: entry.operation,
      entry,
      source: sourceManifestForEntry(loaded.artifact, strategyId).instructions[entry.operation],
      canonical: canonicalInstruction,
    });
    gates.push(...linkage.map((gate) => ({ ...gate, name: `${entry.operation} artifact linkage: ${gate.name}` })));
    const semantic = semanticPolicyReadback({
      operation: entry.operation,
      entry,
      inner: canonicalInstructionView(canonicalInstruction),
      account,
      requireZeroCounters: false,
    });
    gates.push(...semantic.gates.map((gate) => ({ ...gate, name: `${entry.operation} current policy: ${gate.name}` })));
    const signatures = [];
    let before: string | undefined;
    for (let pageIndex = 0; pageIndex < 10; pageIndex += 1) {
      const page = await connection.getSignaturesForAddress(
        policyAddresses[index]!,
        { limit: 1_000, ...(before === undefined ? {} : { before }) },
        commitment,
      );
      signatures.push(...page);
      if (page.length < 1_000) break;
      before = page[page.length - 1]!.signature;
      if (pageIndex === 9) throw new Error(`policy ${entry.policy} origin is older than the bounded 10,000-signature history scan`);
    }
    let origin: null | Readonly<{
      signature: string;
      slot: number;
      dataSha256: string;
      accounts: readonly string[];
      requiredSigners: readonly string[];
      transactionVersion: number | "legacy";
      signatureMatches: boolean;
      accountKeySetExact: boolean;
      accountRolesExact: boolean;
      instructionRolesExact: boolean;
      topLevelInstructionCount: number;
      lookupTableCount: number;
    }> = null;
    for (const candidate of signatures) {
      if (candidate.err !== null) continue;
      const transaction = await connection.getTransaction(candidate.signature, { commitment, maxSupportedTransactionVersion: 0 });
      if (!transaction || transaction.meta?.err) continue;
      const message = transaction.transaction.message;
      const keys = [
        ...message.staticAccountKeys,
        ...(transaction.meta?.loadedAddresses?.writable ?? []),
        ...(transaction.meta?.loadedAddresses?.readonly ?? []),
      ];
      for (const instruction of message.compiledInstructions) {
        const programId = keys[instruction.programIdIndex]?.toBase58() ?? null;
        const dataHash = sha256(instruction.data);
        const instructionAccounts = [...instruction.accountKeyIndexes].map((accountIndex) => keys[accountIndex]?.toBase58() ?? "<missing>");
        if (programId === entry.policyCreate.programId && dataHash === entry.policyCreate.dataSha256 && instructionAccounts.join(",") === entry.policyCreate.accounts.map(({ address }) => address).join(",")) {
          const expectedRoles = new Map<string, { signer: boolean; writable: boolean }>();
          const mergeRole = (accountAddress: string, signer: boolean, writable: boolean) => {
            const previous = expectedRoles.get(accountAddress);
            expectedRoles.set(accountAddress, {
              signer: signer || previous?.signer === true,
              writable: writable || previous?.writable === true,
            });
          };
          mergeRole(entry.policyCreate.programId, false, false);
          entry.policyCreate.accounts.forEach(({ address: accountAddress, signer, writable }) => mergeRole(accountAddress, signer, writable));
          // The fee payer is globally signer+writable even when one duplicate
          // instruction meta requests it readonly.
          mergeRole(PARTNER_ROUTE.setupAdmin, true, true);
          const observedKeyAddresses = keys.map((key) => key.toBase58());
          const expectedKeyAddresses = [...expectedRoles.keys()];
          const accountKeySetExact = observedKeyAddresses.length === expectedKeyAddresses.length
            && sameJson([...observedKeyAddresses].sort(), [...expectedKeyAddresses].sort());
          const accountRolesExact = accountKeySetExact && keys.every((key, accountIndex) => {
            const expectedRole = expectedRoles.get(key.toBase58());
            return expectedRole !== undefined
              && message.isAccountSigner(accountIndex) === expectedRole.signer
              && message.isAccountWritable(accountIndex) === expectedRole.writable;
          });
          const instructionRolesExact = [...instruction.accountKeyIndexes].every((accountIndex, accountPosition) => {
            const expectedRole = expectedRoles.get(entry.policyCreate.accounts[accountPosition]!.address);
            return expectedRole !== undefined
              && message.isAccountSigner(accountIndex) === expectedRole.signer
              && message.isAccountWritable(accountIndex) === expectedRole.writable;
          });
          const requiredSigners = message.staticAccountKeys
            .slice(0, message.header.numRequiredSignatures)
            .map((key) => key.toBase58());
          origin = {
            signature: candidate.signature,
            slot: transaction.slot,
            dataSha256: dataHash,
            accounts: instructionAccounts,
            requiredSigners,
            transactionVersion: transaction.version ?? "legacy",
            signatureMatches: transaction.transaction.signatures.length === 1
              && transaction.transaction.signatures[0] === candidate.signature,
            accountKeySetExact,
            accountRolesExact,
            instructionRolesExact,
            topLevelInstructionCount: message.compiledInstructions.length,
            lookupTableCount: message.addressTableLookups.length,
          };
          break;
        }
      }
      if (origin) break;
    }
    add(gates, `${entry.operation} exact policy-create origin found`, origin !== null, origin, { programId: entry.policyCreate.programId, dataSha256: entry.policyCreate.dataSha256, accounts: entry.policyCreate.accounts.map(({ address }) => address) });
    add(
      gates,
      `${entry.operation} origin has exact v0 signer, roles, keys, and one instruction`,
      origin !== null
        && sameJson(origin.requiredSigners, [PARTNER_ROUTE.setupAdmin])
        && origin.transactionVersion === 0
        && origin.signatureMatches
        && origin.accountKeySetExact
        && origin.accountRolesExact
        && origin.instructionRolesExact
        && origin.topLevelInstructionCount === 1
        && origin.lookupTableCount === 0,
      origin,
      {
        requiredSigners: [PARTNER_ROUTE.setupAdmin],
        transactionVersion: 0,
        signatureMatches: true,
        accountKeySetExact: true,
        accountRolesExact: true,
        instructionRolesExact: true,
        topLevelInstructionCount: 1,
        lookupTableCount: 0,
      },
    );
    policies.push({
      operation: entry.operation,
      seed: entry.seed,
      policy: entry.policy,
      currentDataSha256: account ? sha256(account.data) : null,
      decodedLength: semantic.decodedLength,
      transactionIndex: semantic.transactionIndex,
      staleTransactionIndex: semantic.staleTransactionIndex,
      origin,
    });
  }
  const failedGateCount = gates.filter(({ pass }) => !pass).length;
  return {
    verdict: failedGateCount === 0
      ? commitment === "confirmed" ? "PARTNER_RUNTIME_POLICIES_CONFIRMED_PASS" : "PARTNER_RUNTIME_POLICIES_FINALIZED_PASS"
      : commitment === "confirmed" ? "PARTNER_RUNTIME_POLICIES_CONFIRMED_FAIL" : "PARTNER_RUNTIME_POLICIES_FINALIZED_FAIL",
    broadcast: false,
    routeSpecSha256: loaded.artifact.routeSpecSha256,
    artifact: { path: loaded.path, fileSha256: loaded.fileSha256, artifactSha256: loaded.artifact.artifactSha256 },
    requestedMinContextSlot: minContextSlot ?? null,
    commitment,
    contextSlot: state.contextSlot,
    deploymentContextSlot: deployments.contextSlot,
    deployments: deployments.identities,
    nonCatalogIsolation,
    policies,
    failedGateCount,
    gates,
  } as const;
}

export async function executeRuntimePolicyInstall(input: Readonly<{
  strategyId: PartnerStrategyId;
  operation: RuntimePolicyOperation;
  artifactPath: string;
  authorizationPath: string | null;
  confirmAuthorizationSha256: string | null;
  confirmVault: string | null;
  confirmArtifactSha256: string | null;
  confirmPolicyCreateDataSha256: string | null;
  confirmMaxTotalLamports: string | null;
  intentPath: string | null;
}>) {
  if (process.env.CONFIRM_MAINNET !== "1") throw new Error("execute policy install requires CONFIRM_MAINNET=1");
  if (!input.authorizationPath) throw new Error("policy install requires --authorization");
  if (!input.confirmAuthorizationSha256) throw new Error("policy install requires --confirm-authorization-sha256");
  if (!input.intentPath) throw new Error("policy install requires an explicit --intent-path");
  const intentPath = requirePolicyIntentPath(input.intentPath);
  if (input.confirmVault !== PARTNER_ROUTE.vault) throw new Error(`execute policy install requires --confirm-vault ${PARTNER_ROUTE.vault}`);
  if (input.confirmMaxTotalLamports !== MAX_POLICY_INSTALL_LAMPORTS.toString()) throw new Error(`execute policy install requires --confirm-max-total-lamports ${MAX_POLICY_INSTALL_LAMPORTS}`);
  const authorization = loadPolicyCatalogAuthorization(input.authorizationPath, input.artifactPath, input.confirmAuthorizationSha256);
  if (input.confirmArtifactSha256 !== authorization.authorization.artifactFileSha256) throw new Error(`execute policy install requires --confirm-artifact-sha256 ${authorization.authorization.artifactFileSha256}`);
  const authorizedEntry = authorization.artifact.policies.find((entry) => strategyForEntry(entry) === input.strategyId && entry.operation === input.operation);
  if (!authorizedEntry) throw new Error(`policy authorization has no ${input.strategyId} ${input.operation} entry`);
  if (input.confirmPolicyCreateDataSha256 !== authorizedEntry.policyCreate.dataSha256) throw new Error(`execute policy install requires --confirm-policy-create-data-sha256 ${authorizedEntry.policyCreate.dataSha256}`);
  const preparation = await prepareRuntimePolicyInstall(input.strategyId, input.operation, input.artifactPath);
  if (input.confirmArtifactSha256 !== preparation.loaded.fileSha256) throw new Error(`execute policy install requires --confirm-artifact-sha256 ${preparation.loaded.fileSha256}`);
  if (input.confirmPolicyCreateDataSha256 !== preparation.entry.policyCreate.dataSha256) throw new Error(`execute policy install requires --confirm-policy-create-data-sha256 ${preparation.entry.policyCreate.dataSha256}`);
  if (!preparation.report.readyForBroadcast || preparation.report.failedGateCount !== 0) throw new Error(`policy install preflight failed with ${preparation.report.verdict}`);
  const refreshed = await confirmedSnapshots(rpcUrl(), [PARTNER_ROUTE.squads.settings, ...preparation.prestatePolicyAddresses, PARTNER_ROUTE.setupAdmin], preparation.prepared.simulationSlot);
  const refreshedCanonical = preparation.canonicalByStrategy;
  const refreshedCatalogGates = await catalogPrefixSuffixGates({
    artifact: preparation.loaded.artifact,
    canonical: refreshedCanonical,
    state: refreshed,
    targetIndex: preparation.targetIndex,
    expectedBefore: preparation.expectedBefore,
  });
  if (refreshedCatalogGates.some(({ pass }) => !pass) || !refreshed.accounts[0] || sha256(refreshed.accounts[0].data) !== preparation.prestateSettingsDataSha256) {
    throw new Error("policy install protected state changed after simulation; refusing send");
  }
  const refreshedDeployments = await loadDeploymentIdentities(rpcUrl(), PARTNER_ROUTE, refreshed.contextSlot, "confirmed");
  if (!verifyDeploymentIdentities(PARTNER_ROUTE, refreshedDeployments.identities, [PARTNER_ROUTE.squads.program]).every(({ pass }) => pass)) {
    throw new Error("Squads deployment identity changed after policy simulation; refusing send");
  }
  const [legacyCatalog, squadsIsolation] = await Promise.all([
    verifyLegacyVoltrPolicyCatalog(rpcUrl(), refreshed.contextSlot, "confirmed"),
    verifyNonCatalogSquadsPoliciesIsolated(rpcUrl(), BigInt(preparation.loaded.artifact.policies[0]!.seed), BigInt(preparation.loaded.artifact.policies.at(-1)!.seed), refreshed.contextSlot, "confirmed", [{ firstSeed: 17n, lastSeed: 24n }], false),
  ]);
  if (legacyCatalog.verdict !== "PARTNER_LEGACY_VOLTR_POLICIES_CONFIRMED_PASS" || squadsIsolation.verdict !== "PARTNER_NON_CATALOG_SQUADS_ISOLATION_PASS") {
    throw new Error("legacy or non-catalog Squads policy boundary changed after simulation; refusing send");
  }
  const authorizationContextSlot = Math.max(
    preparation.prepared.simulationSlot,
    refreshed.contextSlot,
    refreshedDeployments.contextSlot,
    legacyCatalog.contextSlot,
    squadsIsolation.contextSlot,
  );
  const intentDocument = JSON.stringify({
    schemaVersion: 1,
    kind: "backyard-voltr-policy-install-intent",
    strategyId: input.strategyId,
    operation: input.operation,
    artifactPath: authorization.authorization.artifactPath,
    artifactFileSha256: authorization.authorization.artifactFileSha256,
    artifactSha256: authorization.authorization.artifactSha256,
    authorizationFileSha256: authorization.fileSha256,
    authorizationSha256: authorization.authorization.authorizationSha256,
    catalogPolicySeedBefore: authorization.authorization.catalogPolicySeedBefore,
    terminalPolicySeed: authorization.authorization.terminalPolicySeed,
    expectedSignature: preparation.prepared.expectedSignature,
    serializedTransactionSha256: sha256(preparation.prepared.serializedTransaction),
    packetBytes: preparation.prepared.packetBytes,
    canonicalMessageSha256: sha256(preparation.prepared.serializedMessage),
    intent: preparation.intent,
  }, (_key, value) => typeof value === "bigint" ? value.toString() : value, 2) + "\n";
  try {
    mkdirSync(POLICY_INTENT_ROOT, { recursive: true });
    writeFileSync(intentPath, intentDocument, { encoding: "utf8", mode: 0o600, flag: "wx" });
  } catch (error) {
    throw new Error(`policy install could not persist the exact signed intent at ${intentPath}`, { cause: error });
  }
  let finalized: Awaited<ReturnType<typeof sendPreparedConfirmedOnce>> | null = null;
  try {
    finalized = await sendPreparedConfirmedOnce(rpcUrl(), preparation.prepared, authorizationContextSlot);
    if (finalized.err !== null) return { verdict: "PARTNER_RUNTIME_POLICY_INSTALL_FINALIZED_WITH_ERROR", broadcast: true, preflight: preparation.report, finalized } as const;
    const state = await confirmedSnapshots(rpcUrl(), [PARTNER_ROUTE.squads.settings, ...preparation.prestatePolicyAddresses], finalized.confirmedSlot);
    const policy = state.accounts[preparation.targetIndex + 1] ?? null;
    const semantic = semanticPolicyReadback({
      operation: input.operation,
      entry: preparation.entry,
      inner: canonicalInstructionView(preparation.canonicalInner),
      account: policy,
      requireZeroCounters: true,
    });
    const gates: Gate[] = [];
    add(gates, "confirmed settings seed", settingsSeed(state.accounts[0] ?? null) === preparation.expectedSeed, settingsSeed(state.accounts[0] ?? null), preparation.expectedSeed);
    gates.push(...semantic.gates.map((gate) => ({ ...gate, name: `finalized policy: ${gate.name}` })));
    const failedGateCount = gates.filter(({ pass }) => !pass).length;
    return { verdict: failedGateCount === 0 ? "PARTNER_RUNTIME_POLICY_INSTALL_CONFIRMED_AND_VERIFIED" : "PARTNER_RUNTIME_POLICY_INSTALL_CONFIRMED_READBACK_FAIL", broadcast: true, intentPath, intent: preparation.intent, intentSha256: preparation.intentSha256, preflight: preparation.report, finalized, readbackContextSlot: state.contextSlot, readback: { failedGateCount, gates } } as const;
  } catch (error) {
    if (finalized) return { verdict: "PARTNER_RUNTIME_POLICY_INSTALL_CONFIRMED_READBACK_ERROR", broadcast: true, intentPath, intent: preparation.intent, intentSha256: preparation.intentSha256, preflight: preparation.report, finalized, error: error instanceof Error ? error.message : String(error), recoveryInstruction: "Do not resend. The policy transaction is confirmed; rerun read-only Settings/policy reconciliation." } as const;
    return { verdict: "PARTNER_RUNTIME_POLICY_INSTALL_BROADCAST_STATUS_UNKNOWN", broadcast: null, intentPath, expectedSignature: preparation.prepared.expectedSignature, intent: preparation.intent, intentSha256: preparation.intentSha256, preflight: preparation.report, error: error instanceof Error ? error.message : String(error), recoveryInstruction: "Do not resend. Verify this exact signature, Settings seed, and policy PDA." } as const;
  }
}
