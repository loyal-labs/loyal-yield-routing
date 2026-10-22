import {
  AccountMeta,
  PublicKey,
  TransactionInstruction,
} from "@solana/web3.js";
import { CompiledKeys } from "./compiled-keys";

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
  const compiledKeys = CompiledKeys.compileWithoutPayer(instructions);

  const [header, staticAccountKeys] = compiledKeys.getMessageComponents();

  const remainingAccounts: AccountMeta[] = [];

  staticAccountKeys.forEach((key, index) => {
    remainingAccounts.push({
      pubkey: key,
      isSigner: index < header.numRequiredSignatures,
      isWritable:
        index <
          header.numRequiredSignatures - header.numReadonlySignedAccounts ||
        (index >= header.numRequiredSignatures &&
          index <
            staticAccountKeys.length - header.numReadonlyUnsignedAccounts),
    });
  });

  if (remainingAccounts.find((acc) => acc.pubkey.equals(vaultPda))) {
    remainingAccounts.find((acc) => acc.pubkey.equals(vaultPda))!.isSigner =
      false;
  }
  let args_buffer = Buffer.alloc(0);

  args_buffer = Buffer.concat([Buffer.from([instructions.length])]);

  instructions.forEach((ix) => {
    const accounts = ix.keys.map((key) => {
      const index = remainingAccounts.findIndex((acc) =>
        acc.pubkey.equals(key.pubkey)
      );
      if (index === -1) {
        throw new Error(
          `Account ${key.pubkey.toBase58()} not found in remaining accounts`
        );
      }
      return index;
    });

    const programIdIndex = remainingAccounts.findIndex((id) =>
      id.pubkey.equals(ix.programId)
    );
    if (programIdIndex === -1) {
      throw new Error(
        `ProgramId ${ix.programId.toBase58()} not found in remaining accounts`
      );
    }

    const serialized_ix = serializeCompiledInstruction({
      programIdIndex,
      accountIndexes: accounts,
      data: ix.data,
    });

    args_buffer = Buffer.concat([args_buffer, serialized_ix]);
  });

  // Add the members as signers after instruction indexes have been serialized.
  members.forEach((member) => {
    remainingAccounts.unshift({
      pubkey: member,
      isSigner: true,
      isWritable: false,
    });
  });

  return {
    instructions: args_buffer,
    accounts: remainingAccounts,
  };
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
  const compiledKeys = CompiledKeys.compileWithoutPayer(instructions);

  const [header, staticAccountKeys] = compiledKeys.getMessageComponents();

  const remainingAccounts: AccountMeta[] = [];

  staticAccountKeys.forEach((key, index) => {
    remainingAccounts.push({
      pubkey: key,
      isSigner: index < header.numRequiredSignatures,
      isWritable:
        index <
          header.numRequiredSignatures - header.numReadonlySignedAccounts ||
        (index >= header.numRequiredSignatures &&
          index <
            staticAccountKeys.length - header.numReadonlyUnsignedAccounts),
    });
  });

  if (remainingAccounts.find((acc) => acc.pubkey.equals(vaultPda))) {
    remainingAccounts.find((acc) => acc.pubkey.equals(vaultPda))!.isSigner =
      false;
  }
  let args_buffer = Buffer.alloc(0);

  args_buffer = Buffer.concat([Buffer.from([instructions.length])]);

  instructions.forEach((ix) => {
    const accounts = ix.keys.map((key) => {
      const index = remainingAccounts.findIndex((acc) =>
        acc.pubkey.equals(key.pubkey)
      );
      if (index === -1) {
        throw new Error(
          `Account ${key.pubkey.toBase58()} not found in remaining accounts`
        );
      }
      return index;
    });

    const programIdIndex = remainingAccounts.findIndex((id) =>
      id.pubkey.equals(ix.programId)
    );
    if (programIdIndex === -1) {
      throw new Error(
        `ProgramId ${ix.programId.toBase58()} not found in remaining accounts`
      );
    }

    const serialized_ix = serializeCompiledInstruction({
      programIdIndex,
      accountIndexes: accounts,
      data: ix.data,
    });

    args_buffer = Buffer.concat([args_buffer, serialized_ix]);
  });

  // Add signing members and hook accounts after instruction indexes have been
  // serialized, because the on-chain program treats them as execution context.
  members.forEach((member) => {
    remainingAccounts.unshift({
      pubkey: member,
      isSigner: true,
      isWritable: false,
    });
  });
  remainingAccounts.splice(members.length, 0, ...preHookAccounts);
  remainingAccounts.splice(
    members.length + preHookAccounts.length,
    0,
    ...postHookAccounts
  );

  return {
    instructions: args_buffer,
    accounts: remainingAccounts,
  };
}

function serializeCompiledInstruction(ix: {
  programIdIndex: number;
  accountIndexes: number[];
  data: Buffer | Uint8Array;
}) {
  let buffer = Buffer.alloc(0);

  buffer = Buffer.concat([buffer, Buffer.from([ix.programIdIndex])]);

  // SmallVec<u8, u8> for account indexes
  buffer = Buffer.concat([
    buffer,
    Buffer.from([ix.accountIndexes.length]),
    Buffer.from(ix.accountIndexes),
  ]);

  // SmallVec<u16, u8> for data
  buffer = Buffer.concat([
    buffer,
    // Length prefix as u16 (little endian)
    Buffer.from(new Uint16Array([ix.data.length]).buffer),
    ix.data,
  ]);

  return Uint8Array.from(buffer);
}
