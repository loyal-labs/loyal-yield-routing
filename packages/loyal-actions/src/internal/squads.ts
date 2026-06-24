import { PublicKey, SystemProgram, TransactionInstruction } from "@solana/web3.js";
import { YIELD_ROUTE_STANDALONE_ACTION_SEED } from "../constants.js";
import type { LoyalClusterConfig } from "../cluster.js";
import { BytesEncoder } from "./bytes.js";

const SQUADS_SEED_PREFIX = new TextEncoder().encode("smart_account");
const SQUADS_SEED_SETTINGS = new TextEncoder().encode("settings");
const SQUADS_SEED_SMART_ACCOUNT = new TextEncoder().encode("smart_account");
const SQUADS_SEED_POLICY = new TextEncoder().encode("policy");
const SQUADS_PROGRAM_CONFIG_SEED = new TextEncoder().encode("program_config");
const SQUADS_FULL_PERMISSIONS_MASK = 7;
const SQUADS_SYNC_SIGNER_COUNT = 1;
const CREATE_SMART_ACCOUNT_DISCRIMINATOR = [197, 102, 253, 231, 77, 84, 50, 17] as const;
const EXECUTE_TRANSACTION_SYNC_V2_DISCRIMINATOR = [90, 81, 187, 81, 39, 70, 128, 78] as const;
const EXECUTE_SETTINGS_TRANSACTION_SYNC_DISCRIMINATOR = [138, 209, 64, 163, 79, 67, 233, 76] as const;

export type DataConstraint = {
  dataOffset: bigint;
  dataValue:
    | { type: "u8"; value: number }
    | { type: "u16Le"; value: number }
    | { type: "u32Le"; value: number }
    | { type: "u64Le"; value: bigint }
    | { type: "u128Le"; value: bigint }
    | { type: "u8Slice"; value: readonly number[] };
  operator:
    | "equals"
    | "notEquals"
    | "greaterThan"
    | "greaterThanOrEqualTo"
    | "lessThan"
    | "lessThanOrEqualTo";
};

export type AccountConstraint = {
  accountIndex: number;
  kind:
    | { type: "pubkey"; pubkeys: readonly PublicKey[] }
    | { type: "accountData"; dataConstraints: readonly DataConstraint[] };
  owner?: PublicKey;
};

export type InstructionConstraint = {
  programId: PublicKey;
  accountConstraints: readonly AccountConstraint[];
  dataConstraints: readonly DataConstraint[];
};

export type SquadsContext = {
  settings: PublicKey;
  authority: PublicKey;
  delegatedSigner: PublicKey;
  accountIndex: number;
  vault: PublicKey;
  policySeed?: bigint;
};

export type SquadsPda = {
  address: PublicKey;
  bump: number;
};

export type CreateSquadsSmartAccountInput = {
  payer: PublicKey;
  verifier: PublicKey;
  seed: bigint;
  treasury: PublicKey;
  programConfig?: PublicKey;
};

export type CompiledSquadsInstruction = {
  programIdIndex: number;
  accounts: number[];
  data: Uint8Array;
};

export type CompiledSquadsTransaction = {
  compiledInstructions: CompiledSquadsInstruction[];
  transactionAccounts: TransactionInstruction["keys"];
};

export type SquadsSyncTransactionInput = {
  settings: PublicKey;
  signer: PublicKey;
  accountIndex: number;
  instructions: readonly TransactionInstruction[];
  extraAccounts?: TransactionInstruction["keys"];
};

export type SquadsProgramInteractionExecutionInput = {
  policy: PublicKey;
  signer: PublicKey;
  accountIndex: number;
  instructions: readonly TransactionInstruction[];
  instructionConstraintIndexes: readonly number[];
  extraAccounts?: TransactionInstruction["keys"];
};

export type PlannedLaneRebalance = {
  fromLaneId: number;
  toLaneId: number;
};

export function deriveSquadsSettings(config: LoyalClusterConfig, seed: bigint): SquadsPda {
  assertU128(seed, "seed");
  const seedBytes = new Uint8Array(16);
  new DataView(seedBytes.buffer).setBigUint64(0, seed & 0xffffffffffffffffn, true);
  new DataView(seedBytes.buffer).setBigUint64(8, seed >> 64n, true);
  const [address, bump] = PublicKey.findProgramAddressSync(
    [SQUADS_SEED_PREFIX, SQUADS_SEED_SETTINGS, seedBytes],
    config.squadsSmartAccountProgramId,
  );
  return { address, bump };
}

export function deriveSquadsVault(config: LoyalClusterConfig, settings: PublicKey, vaultIndex: number): SquadsPda {
  assertU8(vaultIndex, "vaultIndex");
  const [address, bump] = PublicKey.findProgramAddressSync(
    [SQUADS_SEED_PREFIX, settings.toBytes(), SQUADS_SEED_SMART_ACCOUNT, Uint8Array.of(vaultIndex)],
    config.squadsSmartAccountProgramId,
  );
  return { address, bump };
}

export function deriveSquadsPolicy(config: LoyalClusterConfig, settings: PublicKey, policySeed: bigint): SquadsPda {
  assertU64(policySeed, "policySeed");
  const seedBytes = new Uint8Array(8);
  new DataView(seedBytes.buffer).setBigUint64(0, policySeed, true);
  const [address, bump] = PublicKey.findProgramAddressSync(
    [SQUADS_SEED_PREFIX, SQUADS_SEED_POLICY, settings.toBytes(), seedBytes],
    config.squadsSmartAccountProgramId,
  );
  return { address, bump };
}

export function deriveActionAccount(
  config: LoyalClusterConfig,
  settings: PublicKey,
  policySeed = YIELD_ROUTE_STANDALONE_ACTION_SEED,
): PublicKey {
  return deriveSquadsPolicy(config, settings, policySeed).address;
}

export function deriveSquadsProgramConfig(config: LoyalClusterConfig): PublicKey {
  return PublicKey.findProgramAddressSync(
    [SQUADS_SEED_PREFIX, SQUADS_PROGRAM_CONFIG_SEED],
    config.squadsSmartAccountProgramId,
  )[0];
}

export function createSquadsSmartAccountInstruction(
  config: LoyalClusterConfig,
  input: CreateSquadsSmartAccountInput,
): TransactionInstruction {
  if (input.seed <= 0n) {
    throw new Error("Squads smart-account seed starts at 1");
  }
  const settings = deriveSquadsSettings(config, input.seed).address;
  const programConfig = input.programConfig ?? deriveSquadsProgramConfig(config);

  return new TransactionInstruction({
    programId: config.squadsSmartAccountProgramId,
    keys: [
      { pubkey: programConfig, isSigner: false, isWritable: true },
      { pubkey: input.treasury, isSigner: false, isWritable: true },
      { pubkey: input.payer, isSigner: true, isWritable: true },
      { pubkey: SystemProgram.programId, isSigner: false, isWritable: false },
      { pubkey: config.squadsSmartAccountProgramId, isSigner: false, isWritable: false },
      { pubkey: settings, isSigner: false, isWritable: true },
    ],
    data: Buffer.from(serializeCreateSmartAccountArgs(input.verifier)),
  });
}

export function createProgramInteractionPolicyInstruction(
  config: LoyalClusterConfig,
  context: SquadsContext,
  constraints: readonly InstructionConstraint[],
): TransactionInstruction {
  const policySeed = context.policySeed ?? YIELD_ROUTE_STANDALONE_ACTION_SEED;
  const actionAccount = deriveActionAccount(config, context.settings, policySeed);
  const data = serializeSettingsActions(
    context.delegatedSigner,
    policySeed,
    compileProgramInteractionPayload(context.accountIndex, constraints),
  );

  return new TransactionInstruction({
    programId: config.squadsSmartAccountProgramId,
    keys: [
      { pubkey: context.settings, isSigner: false, isWritable: true },
      { pubkey: context.authority, isSigner: true, isWritable: true },
      { pubkey: SystemProgram.programId, isSigner: false, isWritable: false },
      { pubkey: config.squadsSmartAccountProgramId, isSigner: false, isWritable: false },
      { pubkey: context.authority, isSigner: true, isWritable: false },
      { pubkey: actionAccount, isSigner: false, isWritable: true },
    ],
    data: Buffer.from(data),
  });
}

export function createProgramInteractionPolicyUpdateInstruction(
  config: LoyalClusterConfig,
  context: Omit<SquadsContext, "vault" | "policySeed">,
  policy: PublicKey,
  constraints: readonly InstructionConstraint[],
): TransactionInstruction {
  const data = serializePolicyUpdateActions(
    policy,
    context.delegatedSigner,
    compileProgramInteractionPayload(context.accountIndex, constraints),
  );

  return new TransactionInstruction({
    programId: config.squadsSmartAccountProgramId,
    keys: [
      { pubkey: context.settings, isSigner: false, isWritable: true },
      { pubkey: context.authority, isSigner: true, isWritable: true },
      { pubkey: SystemProgram.programId, isSigner: false, isWritable: false },
      { pubkey: config.squadsSmartAccountProgramId, isSigner: false, isWritable: false },
      { pubkey: context.authority, isSigner: true, isWritable: false },
      { pubkey: policy, isSigner: false, isWritable: true },
    ],
    data: Buffer.from(data),
  });
}

export function compileSquadsTransactionInstructions(
  instructions: readonly TransactionInstruction[],
  extraAccounts: TransactionInstruction["keys"] = [],
): CompiledSquadsTransaction {
  if (instructions.length > 255) {
    throw new Error("Squads sync payload supports up to 255 instructions");
  }
  const transactionAccounts: TransactionInstruction["keys"] = [];
  for (const account of extraAccounts) {
    pushOrUpdateAccountMeta(transactionAccounts, account);
  }

  const compiledInstructions = instructions.map((instruction) => {
    const accountIndexes = instruction.keys.map((account) => pushOrUpdateAccountMeta(transactionAccounts, account));
    const programIdIndex = pushOrUpdateAccountMeta(transactionAccounts, {
      pubkey: instruction.programId,
      isSigner: false,
      isWritable: false,
    });
    return {
      programIdIndex,
      accounts: accountIndexes,
      data: new Uint8Array(instruction.data),
    };
  });

  return { compiledInstructions, transactionAccounts };
}

export function createSquadsSyncTransactionInstruction(
  config: LoyalClusterConfig,
  input: SquadsSyncTransactionInput,
): TransactionInstruction {
  const compiled = compileSquadsTransactionInstructions(input.instructions, input.extraAccounts);
  return createSquadsSyncTransactionInstructionFromCompiled(config, {
    settings: input.settings,
    signer: input.signer,
    accountIndex: input.accountIndex,
    ...compiled,
  });
}

export function createSquadsSyncTransactionInstructionFromCompiled(
  config: LoyalClusterConfig,
  input: {
    settings: PublicKey;
    signer: PublicKey;
    accountIndex: number;
    compiledInstructions: readonly CompiledSquadsInstruction[];
    transactionAccounts: TransactionInstruction["keys"];
  },
): TransactionInstruction {
  const data = serializeSyncTransactionArgs(
    input.accountIndex,
    squadsCompiledInstructionPayload(input.compiledInstructions),
  );
  return new TransactionInstruction({
    programId: config.squadsSmartAccountProgramId,
    keys: [
      { pubkey: input.settings, isSigner: false, isWritable: true },
      { pubkey: config.squadsSmartAccountProgramId, isSigner: false, isWritable: false },
      { pubkey: input.signer, isSigner: true, isWritable: false },
      ...input.transactionAccounts,
    ],
    data: Buffer.from(data),
  });
}

export function createSquadsProgramInteractionExecutionInstruction(
  config: LoyalClusterConfig,
  input: SquadsProgramInteractionExecutionInput,
): TransactionInstruction {
  const compiled = compileSquadsTransactionInstructions(input.instructions, input.extraAccounts);
  return createSquadsProgramInteractionExecutionInstructionFromCompiled(config, {
    policy: input.policy,
    signer: input.signer,
    accountIndex: input.accountIndex,
    instructionConstraintIndexes: input.instructionConstraintIndexes,
    ...compiled,
  });
}

export function createSquadsProgramInteractionExecutionInstructionFromCompiled(
  config: LoyalClusterConfig,
  input: {
    policy: PublicKey;
    signer: PublicKey;
    accountIndex: number;
    compiledInstructions: readonly CompiledSquadsInstruction[];
    instructionConstraintIndexes: readonly number[];
    transactionAccounts: TransactionInstruction["keys"];
  },
): TransactionInstruction {
  const data = serializeProgramInteractionExecutionArgs(
    input.accountIndex,
    input.instructionConstraintIndexes,
    squadsCompiledInstructionPayload(input.compiledInstructions),
  );
  return new TransactionInstruction({
    programId: config.squadsSmartAccountProgramId,
    keys: [
      { pubkey: input.policy, isSigner: false, isWritable: true },
      { pubkey: config.squadsSmartAccountProgramId, isSigner: false, isWritable: false },
      { pubkey: input.signer, isSigner: true, isWritable: false },
      ...input.transactionAccounts,
    ],
    data: Buffer.from(data),
  });
}

export function assertRebalanceAvoidsActiveLanes(
  activeLanes: readonly number[],
  transfers: readonly PlannedLaneRebalance[],
): void {
  const active = new Set(activeLanes.map((lane) => normalizeLaneId(lane, "active lane")));
  for (const transfer of transfers) {
    const fromLaneId = normalizeLaneId(transfer.fromLaneId, "fromLaneId");
    const toLaneId = normalizeLaneId(transfer.toLaneId, "toLaneId");
    if (active.has(fromLaneId) || active.has(toLaneId)) {
      throw new Error(`rebalance touches active lane ${fromLaneId} -> ${toLaneId}`);
    }
  }
}

type CompiledPayload = {
  accountIndex: number;
  pubkeyTable: PublicKey[];
  instructionConstraints: CompiledInstructionConstraint[];
};

type CompiledInstructionConstraint = {
  programIdIndex: number;
  accountConstraints: CompiledAccountConstraint[];
  dataConstraints: readonly DataConstraint[];
};

type CompiledAccountConstraint = {
  accountIndex: number;
  kind: { type: "pubkey"; pubkeyIndexes: number[] } | { type: "accountData"; dataConstraints: readonly DataConstraint[] };
  ownerIndex?: number;
};

function compileProgramInteractionPayload(
  accountIndex: number,
  constraints: readonly InstructionConstraint[],
): CompiledPayload {
  const pubkeyTable: PublicKey[] = [];
  return {
    accountIndex,
    pubkeyTable,
    instructionConstraints: constraints.map((constraint) => compileInstructionConstraint(constraint, pubkeyTable)),
  };
}

function compileInstructionConstraint(
  constraint: InstructionConstraint,
  pubkeyTable: PublicKey[],
): CompiledInstructionConstraint {
  return {
    programIdIndex: pubkeyTableIndex(pubkeyTable, constraint.programId),
    accountConstraints: constraint.accountConstraints.map((accountConstraint) =>
      compileAccountConstraint(accountConstraint, pubkeyTable),
    ),
    dataConstraints: constraint.dataConstraints,
  };
}

function compileAccountConstraint(
  constraint: AccountConstraint,
  pubkeyTable: PublicKey[],
): CompiledAccountConstraint {
  return {
    accountIndex: constraint.accountIndex,
    kind:
      constraint.kind.type === "pubkey"
        ? {
            type: "pubkey",
            pubkeyIndexes: constraint.kind.pubkeys.map((pubkey) => pubkeyTableIndex(pubkeyTable, pubkey)),
          }
        : { type: "accountData", dataConstraints: constraint.kind.dataConstraints },
    ownerIndex: constraint.owner ? pubkeyTableIndex(pubkeyTable, constraint.owner) : undefined,
  };
}

function pubkeyTableIndex(pubkeyTable: PublicKey[], pubkey: PublicKey): number {
  const key = pubkey.toBase58();
  const existing = pubkeyTable.findIndex((candidate) => candidate.toBase58() === key);
  if (existing !== -1) {
    return existing;
  }
  if (pubkeyTable.length >= 240) {
    throw new Error("Squads ProgramInteraction pubkey table overflow");
  }
  pubkeyTable.push(pubkey);
  return pubkeyTable.length - 1;
}

function serializeSettingsActions(
  delegatedSigner: PublicKey,
  seed: bigint,
  payload: CompiledPayload,
): Uint8Array {
  const encoder = new BytesEncoder();
  encoder.pushBytes(EXECUTE_SETTINGS_TRANSACTION_SYNC_DISCRIMINATOR);
  encoder.pushU8(SQUADS_SYNC_SIGNER_COUNT);
  encoder.pushVec([undefined], () => encodePolicyCreateAction(encoder, delegatedSigner, seed, payload));
  encoder.pushOption<string>(undefined, (memo) => {
    const bytes = new TextEncoder().encode(memo);
    encoder.pushU32(bytes.length);
    encoder.pushBytes(bytes);
  });
  return encoder.finish();
}

function serializePolicyUpdateActions(
  policy: PublicKey,
  delegatedSigner: PublicKey,
  payload: CompiledPayload,
): Uint8Array {
  const encoder = new BytesEncoder();
  encoder.pushBytes(EXECUTE_SETTINGS_TRANSACTION_SYNC_DISCRIMINATOR);
  encoder.pushU8(SQUADS_SYNC_SIGNER_COUNT);
  encoder.pushVec([undefined], () => encodePolicyUpdateAction(encoder, policy, delegatedSigner, payload));
  encoder.pushOption<string>(undefined, (memo) => {
    const bytes = new TextEncoder().encode(memo);
    encoder.pushU32(bytes.length);
    encoder.pushBytes(bytes);
  });
  return encoder.finish();
}

function serializeCreateSmartAccountArgs(verifier: PublicKey): Uint8Array {
  const encoder = new BytesEncoder();
  encoder.pushBytes(CREATE_SMART_ACCOUNT_DISCRIMINATOR);
  encoder.pushOption<PublicKey>(undefined, (pubkey) => encoder.pushPubkey(pubkey));
  encoder.pushU16(1);
  encoder.pushU32(1);
  encoder.pushPubkey(verifier);
  encoder.pushU8(SQUADS_FULL_PERMISSIONS_MASK);
  encoder.pushU32(0);
  encoder.pushOption<PublicKey>(undefined, (pubkey) => encoder.pushPubkey(pubkey));
  encoder.pushOption<string>(undefined, (memo) => {
    const bytes = new TextEncoder().encode(memo);
    encoder.pushU32(bytes.length);
    encoder.pushBytes(bytes);
  });
  return encoder.finish();
}

function serializeSyncTransactionArgs(accountIndex: number, payload: Uint8Array): Uint8Array {
  assertU8(accountIndex, "accountIndex");
  const encoder = new BytesEncoder();
  encoder.pushBytes(EXECUTE_TRANSACTION_SYNC_V2_DISCRIMINATOR);
  encoder.pushU8(accountIndex);
  encoder.pushU8(SQUADS_SYNC_SIGNER_COUNT);
  encoder.pushU8(0);
  encoder.pushVec([...payload], (byte) => encoder.pushU8(byte));
  return encoder.finish();
}

function serializeProgramInteractionExecutionArgs(
  accountIndex: number,
  instructionConstraintIndexes: readonly number[],
  instructionsPayload: Uint8Array,
): Uint8Array {
  assertU8(accountIndex, "accountIndex");
  const encoder = new BytesEncoder();
  encoder.pushBytes(EXECUTE_TRANSACTION_SYNC_V2_DISCRIMINATOR);
  encoder.pushU8(accountIndex);
  encoder.pushU8(SQUADS_SYNC_SIGNER_COUNT);
  encoder.pushU8(1);
  encoder.pushU8(1);
  encoder.pushOption(instructionConstraintIndexes, (indexes) =>
    encoder.pushVec(indexes, (index) => {
      assertU8(index, "instructionConstraintIndex");
      encoder.pushU8(index);
    }),
  );
  encoder.pushU8(1);
  encoder.pushU8(accountIndex);
  encoder.pushVec([...instructionsPayload], (byte) => encoder.pushU8(byte));
  return encoder.finish();
}

function encodePolicyCreateAction(
  encoder: BytesEncoder,
  delegatedSigner: PublicKey,
  seed: bigint,
  payload: CompiledPayload,
): void {
  encoder.pushU8(7);
  encoder.pushU64(seed);
  encoder.pushU8(4);
  encodeProgramInteractionPayload(encoder, payload);
  encoder.pushVec([delegatedSigner], (signer) => {
    encoder.pushPubkey(signer);
    encoder.pushU8(SQUADS_FULL_PERMISSIONS_MASK);
  });
  encoder.pushU16(1);
  encoder.pushU32(0);
  encoder.pushOption<bigint>(undefined, (timestamp) => encoder.pushU64(timestamp));
  encoder.pushOption<never>(undefined, () => undefined);
}

function encodePolicyUpdateAction(
  encoder: BytesEncoder,
  policy: PublicKey,
  delegatedSigner: PublicKey,
  payload: CompiledPayload,
): void {
  encoder.pushU8(8);
  encoder.pushPubkey(policy);
  encoder.pushVec([delegatedSigner], (signer) => {
    encoder.pushPubkey(signer);
    encoder.pushU8(SQUADS_FULL_PERMISSIONS_MASK);
  });
  encoder.pushU16(1);
  encoder.pushU32(0);
  encoder.pushU8(4);
  encodeProgramInteractionPayload(encoder, payload);
  encoder.pushOption<never>(undefined, () => undefined);
}

function encodeProgramInteractionPayload(encoder: BytesEncoder, payload: CompiledPayload): void {
  encoder.pushU8(payload.accountIndex);
  encoder.pushSmallVec(payload.pubkeyTable, (pubkey) => encoder.pushPubkey(pubkey));
  encoder.pushSmallVec(payload.instructionConstraints, (constraint) => {
    encoder.pushU8(constraint.programIdIndex);
    encoder.pushSmallVec(constraint.accountConstraints, (accountConstraint) => {
      encoder.pushU8(accountConstraint.accountIndex);
      if (accountConstraint.kind.type === "pubkey") {
        encoder.pushU8(0);
        encoder.pushSmallVec(accountConstraint.kind.pubkeyIndexes, (index) => encoder.pushU8(index));
      } else {
        encoder.pushU8(1);
        encoder.pushSmallVec(accountConstraint.kind.dataConstraints, (dataConstraint) =>
          encodeDataConstraint(encoder, dataConstraint),
        );
      }
      encoder.pushOption(accountConstraint.ownerIndex, (ownerIndex) => encoder.pushU8(ownerIndex));
    });
    encoder.pushSmallVec(constraint.dataConstraints, (dataConstraint) => encodeDataConstraint(encoder, dataConstraint));
  });
  encoder.pushOption<never>(undefined, () => undefined);
  encoder.pushOption<never>(undefined, () => undefined);
  encoder.pushSmallVec([], () => undefined);
}

function encodeDataConstraint(encoder: BytesEncoder, constraint: DataConstraint): void {
  encoder.pushU64(constraint.dataOffset);
  switch (constraint.dataValue.type) {
    case "u8":
      encoder.pushU8(0);
      encoder.pushU8(constraint.dataValue.value);
      break;
    case "u16Le":
      encoder.pushU8(1);
      encoder.pushU16(constraint.dataValue.value);
      break;
    case "u32Le":
      encoder.pushU8(2);
      encoder.pushU32(constraint.dataValue.value);
      break;
    case "u64Le":
      encoder.pushU8(3);
      encoder.pushU64(constraint.dataValue.value);
      break;
    case "u128Le":
      encoder.pushU8(4);
      encoder.pushU64(constraint.dataValue.value & 0xffffffffffffffffn);
      encoder.pushU64(constraint.dataValue.value >> 64n);
      break;
    case "u8Slice":
      encoder.pushU8(5);
      encoder.pushVec(constraint.dataValue.value, (byte) => encoder.pushU8(byte));
      break;
  }
  encoder.pushU8(operatorTag(constraint.operator));
}

function squadsCompiledInstructionPayload(instructions: readonly CompiledSquadsInstruction[]): Uint8Array {
  if (instructions.length > 255) {
    throw new Error("Squads sync payload supports up to 255 instructions");
  }
  const encoder = new BytesEncoder();
  encoder.pushU8(instructions.length);
  for (const instruction of instructions) {
    assertU8(instruction.programIdIndex, "programIdIndex");
    encoder.pushU8(instruction.programIdIndex);
    encoder.pushSmallVec(instruction.accounts, (accountIndex) => {
      assertU8(accountIndex, "accountIndex");
      encoder.pushU8(accountIndex);
    });
    if (instruction.data.length > 65535) {
      throw new Error("Squads compiled instruction data overflow");
    }
    encoder.pushU16(instruction.data.length);
    encoder.pushBytes(instruction.data);
  }
  return encoder.finish();
}

function pushOrUpdateAccountMeta(accounts: TransactionInstruction["keys"], meta: TransactionInstruction["keys"][number]): number {
  const existingIndex = accounts.findIndex((existing) => existing.pubkey.equals(meta.pubkey));
  if (existingIndex !== -1) {
    const existing = accounts[existingIndex];
    accounts[existingIndex] = {
      pubkey: existing.pubkey,
      isSigner: existing.isSigner || meta.isSigner,
      isWritable: existing.isWritable || meta.isWritable,
    };
    return existingIndex;
  }
  if (accounts.length >= 256) {
    throw new Error("Squads transaction account table overflow");
  }
  accounts.push({ ...meta });
  return accounts.length - 1;
}

function normalizeLaneId(value: number, field: string): number {
  assertU8(value, field);
  return value;
}

function assertU8(value: number, field: string): void {
  if (!Number.isInteger(value) || value < 0 || value > 255) {
    throw new Error(`${field} must be a u8`);
  }
}

function assertU64(value: bigint, field: string): void {
  if (value < 0n || value > 0xffffffffffffffffn) {
    throw new Error(`${field} must be a u64`);
  }
}

function assertU128(value: bigint, field: string): void {
  if (value < 0n || value > 0xffffffffffffffffffffffffffffffffn) {
    throw new Error(`${field} must be a u128`);
  }
}

function operatorTag(operator: DataConstraint["operator"]): number {
  switch (operator) {
    case "equals":
      return 0;
    case "notEquals":
      return 1;
    case "greaterThan":
      return 2;
    case "greaterThanOrEqualTo":
      return 3;
    case "lessThan":
      return 4;
    case "lessThanOrEqualTo":
      return 5;
  }
}
