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
const SQUADS_CREATE_SMART_ACCOUNT_DISCRIMINATOR = [197, 102, 253, 231, 77, 84, 50, 17] as const;
const SQUADS_PROGRAM_CONFIG_DISCRIMINATOR = [196, 210, 90, 231, 144, 149, 140, 63] as const;
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
};

export type SquadsProgramConfig = {
  smartAccountIndex: bigint;
  authority: PublicKey;
  smartAccountCreationFeeLamports: bigint;
  treasury: PublicKey;
};

export function deriveSquadsSettings(config: LoyalClusterConfig, seed: bigint): PublicKey {
  assertPositiveU128(seed, "Squads smart-account seed");
  return PublicKey.findProgramAddressSync(
    [SQUADS_SEED_PREFIX, SQUADS_SEED_SETTINGS, littleEndianBytes(seed, 16)],
    config.squadsSmartAccountProgramId,
  )[0];
}

export function deriveSquadsVault(config: LoyalClusterConfig, settings: PublicKey, vaultIndex: number): PublicKey {
  assertU8(vaultIndex, "vault index");
  return PublicKey.findProgramAddressSync(
    [SQUADS_SEED_PREFIX, settings.toBytes(), SQUADS_SEED_SMART_ACCOUNT, new Uint8Array([vaultIndex])],
    config.squadsSmartAccountProgramId,
  )[0];
}

export function deriveSquadsProgramConfig(config: LoyalClusterConfig): PublicKey {
  return PublicKey.findProgramAddressSync(
    [SQUADS_SEED_PREFIX, SQUADS_PROGRAM_CONFIG_SEED],
    config.squadsSmartAccountProgramId,
  )[0];
}

export function deriveActionAccount(config: LoyalClusterConfig, settings: PublicKey): PublicKey {
  const seedBytes = new Uint8Array(8);
  new DataView(seedBytes.buffer).setBigUint64(0, YIELD_ROUTE_STANDALONE_ACTION_SEED, true);
  return PublicKey.findProgramAddressSync(
    [SQUADS_SEED_PREFIX, SQUADS_SEED_POLICY, settings.toBytes(), seedBytes],
    config.squadsSmartAccountProgramId,
  )[0];
}

export function createSquadsSmartAccountInstruction(
  config: LoyalClusterConfig,
  input: {
    payer: PublicKey;
    verifier: PublicKey;
    seed: bigint;
    treasury: PublicKey;
  },
): TransactionInstruction {
  assertPositiveU128(input.seed, "Squads smart-account seed");
  const settings = deriveSquadsSettings(config, input.seed);

  return new TransactionInstruction({
    programId: config.squadsSmartAccountProgramId,
    keys: [
      { pubkey: deriveSquadsProgramConfig(config), isSigner: false, isWritable: true },
      { pubkey: input.treasury, isSigner: false, isWritable: true },
      { pubkey: input.payer, isSigner: true, isWritable: true },
      { pubkey: SystemProgram.programId, isSigner: false, isWritable: false },
      { pubkey: config.squadsSmartAccountProgramId, isSigner: false, isWritable: false },
      { pubkey: settings, isSigner: false, isWritable: true },
    ],
    data: Buffer.from(serializeSquadsCreateSmartAccountArgs(input.verifier)),
  });
}

export function decodeSquadsProgramConfig(data: Uint8Array): SquadsProgramConfig {
  if (data.length < 96) {
    throw new Error("Squads program config account is too short");
  }
  for (const [index, byte] of SQUADS_PROGRAM_CONFIG_DISCRIMINATOR.entries()) {
    if (data[index] !== byte) {
      throw new Error("account does not match Squads program config discriminator");
    }
  }

  return {
    smartAccountIndex: littleEndianBigInt(data.subarray(8, 24)),
    authority: new PublicKey(data.subarray(24, 56)),
    smartAccountCreationFeeLamports: littleEndianBigInt(data.subarray(56, 64)),
    treasury: new PublicKey(data.subarray(64, 96)),
  };
}

export function createProgramInteractionPolicyInstruction(
  config: LoyalClusterConfig,
  context: SquadsContext,
  constraints: readonly InstructionConstraint[],
): TransactionInstruction {
  const actionAccount = deriveActionAccount(config, context.settings);
  const data = serializeSettingsActions(
    context.delegatedSigner,
    BigInt(YIELD_ROUTE_STANDALONE_ACTION_SEED),
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

function serializeSquadsCreateSmartAccountArgs(verifier: PublicKey): Uint8Array {
  const encoder = new BytesEncoder();
  encoder.pushBytes(SQUADS_CREATE_SMART_ACCOUNT_DISCRIMINATOR);
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

function littleEndianBytes(value: bigint, byteLength: number): Uint8Array {
  if (value < 0n) {
    throw new Error("cannot encode a negative integer");
  }
  const bytes = new Uint8Array(byteLength);
  let remaining = value;
  for (let index = 0; index < byteLength; index += 1) {
    bytes[index] = Number(remaining & 0xffn);
    remaining >>= 8n;
  }
  if (remaining !== 0n) {
    throw new Error(`integer does not fit in ${byteLength} bytes`);
  }
  return bytes;
}

function littleEndianBigInt(bytes: Uint8Array): bigint {
  let value = 0n;
  for (let index = bytes.length - 1; index >= 0; index -= 1) {
    value = (value << 8n) + BigInt(bytes[index] ?? 0);
  }
  return value;
}

function assertPositiveU128(value: bigint, label: string): void {
  if (value <= 0n || value > (1n << 128n) - 1n) {
    throw new Error(`${label} must be in the range 1..=u128::MAX`);
  }
}

function assertU8(value: number, label: string): void {
  if (!Number.isInteger(value) || value < 0 || value > 255) {
    throw new Error(`${label} must be a u8`);
  }
}
