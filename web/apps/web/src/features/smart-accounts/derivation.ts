import { PublicKey } from "@solana/web3.js";
import { pda } from "@loyal-labs/loyal-smart-accounts";

export const DEFAULT_CANONICAL_SMART_ACCOUNT_INDEX = 0;

export const EARN_VAULT_INDEX = 1 as const;

export function deriveEarnVaultAddress(args: {
  programId?: string;
  settingsPda: string;
}): string {
  const [earnVaultPda] = pda.getSmartAccountPda({
    accountIndex: EARN_VAULT_INDEX,
    ...(args.programId === undefined
      ? {}
      : { programId: new PublicKey(args.programId) }),
    settingsPda: new PublicKey(args.settingsPda),
  });

  return earnVaultPda.toBase58();
}

export function deriveSettingsPdaAddress(args: {
  programId: string;
  accountIndex: bigint;
}): string {
  const [settingsPda] = pda.getSettingsPda({
    accountIndex: args.accountIndex,
    programId: new PublicKey(args.programId),
  });

  return settingsPda.toBase58();
}

export function deriveCanonicalSmartAccountAddress(args: {
  programId: string;
  settingsPda: string;
  accountIndex?: number;
}): string {
  const [smartAccountPda] = pda.getSmartAccountPda({
    settingsPda: new PublicKey(args.settingsPda),
    accountIndex: args.accountIndex ?? DEFAULT_CANONICAL_SMART_ACCOUNT_INDEX,
    programId: new PublicKey(args.programId),
  });

  return smartAccountPda.toBase58();
}
