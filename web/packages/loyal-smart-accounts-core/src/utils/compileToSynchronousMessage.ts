import {
  AccountMeta,
  PublicKey,
  TransactionInstruction,
} from "@solana/web3.js";
import {
  assembleSyncAccounts,
  compileSyncTail,
  serializeSyncInstructions,
} from "./compileToSynchronousMessageV2";

export function compileToSynchronousMessageAndAccounts({
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
  const accounts = assembleSyncAccounts(
    members,
    [],
    compileSyncTail(vaultPda, instructions)
  );
  return {
    instructions: serializeSyncInstructions(instructions, accounts),
    accounts,
  };
}
