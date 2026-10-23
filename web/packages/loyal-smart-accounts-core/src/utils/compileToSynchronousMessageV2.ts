import {
  AccountMeta,
  PublicKey,
  TransactionInstruction,
} from "@solana/web3.js";
import { CompiledKeys } from "./compiled-keys";

export function compileSyncTail(
  vaultPda: PublicKey,
  instructions: TransactionInstruction[]
): AccountMeta[] {
  const [header, staticAccountKeys] =
    CompiledKeys.compileWithoutPayer(instructions).getMessageComponents();
  const signers = header.numRequiredSignatures;
  const writableSigners = signers - header.numReadonlySignedAccounts;
  const writableEnd =
    staticAccountKeys.length - header.numReadonlyUnsignedAccounts;
  return staticAccountKeys.map((pubkey, index) => ({
    pubkey,
    isSigner: index < signers && !pubkey.equals(vaultPda),
    isWritable:
      index < writableSigners || (index >= signers && index < writableEnd),
  }));
}

export function assembleSyncAccounts(
  members: PublicKey[],
  hooks: AccountMeta[],
  tail: AccountMeta[]
): AccountMeta[] {
  return [
    ...[...members]
      .reverse()
      .map((pubkey) => ({ pubkey, isSigner: true, isWritable: false })),
    ...hooks,
    ...tail,
  ];
}

export function serializeSyncInstructions(
  instructions: TransactionInstruction[],
  accounts: AccountMeta[]
): Uint8Array {
  const indexOf = (pubkey: PublicKey, kind: string) => {
    const index = accounts.findIndex((acc) => acc.pubkey.equals(pubkey));
    if (index === -1) {
      throw new Error(
        `${kind} ${pubkey.toBase58()} not found in remaining accounts`
      );
    }
    return index;
  };
  return Buffer.concat([
    Buffer.from([instructions.length]),
    ...instructions.map((ix) =>
      serializeCompiledInstruction({
        accountIndexes: ix.keys.map((key) => indexOf(key.pubkey, "Account")),
        programIdIndex: indexOf(ix.programId, "ProgramId"),
        data: ix.data,
      })
    ),
  ]);
}

export function compileToSynchronousMessageAndAccountsV2({
  vaultPda,
  members,
  instructions,
}: {
  vaultPda: PublicKey;
  members: PublicKey[];
  instructions: TransactionInstruction[];
}): {
  instructions: Uint8Array;
  accounts: AccountMeta[];
} {
  return compileToSynchronousMessageAndAccountsV2WithHooks({
    vaultPda,
    members,
    preHookAccounts: [],
    postHookAccounts: [],
    instructions,
  });
}

export function compileToSynchronousMessageAndAccountsV2WithHooks({
  vaultPda,
  members,
  preHookAccounts,
  postHookAccounts,
  instructions,
}: {
  vaultPda: PublicKey;
  members: PublicKey[];
  preHookAccounts: AccountMeta[];
  postHookAccounts: AccountMeta[];
  instructions: TransactionInstruction[];
}): {
  instructions: Uint8Array;
  accounts: AccountMeta[];
} {
  const tail = compileSyncTail(vaultPda, instructions);
  return {
    instructions: serializeSyncInstructions(instructions, tail),
    accounts: assembleSyncAccounts(
      members,
      [...preHookAccounts, ...postHookAccounts],
      tail
    ),
  };
}

function serializeCompiledInstruction(ix: {
  accountIndexes: number[];
  programIdIndex: number;
  data: Buffer | Uint8Array;
}) {
  return Buffer.concat([
    Buffer.from([ix.programIdIndex]),
    // SmallVec<u8, u8> for account indexes
    Buffer.from([ix.accountIndexes.length]),
    Buffer.from(ix.accountIndexes),
    Buffer.from(new Uint16Array([ix.data.length]).buffer),
    ix.data,
  ]);
}
