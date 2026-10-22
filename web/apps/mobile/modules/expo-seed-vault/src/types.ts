/**
 * A vault-resident account that a wallet has been authorized to use.
 *
 * `authToken` identifies the seed authorization granted to this app. It is a
 * native Kotlin Long carried as a decimal string — a JS number would corrupt
 * values above 2^53. `derivationPath` picks the specific derived account;
 * `publicKey` is the resolved base58 Solana address.
 */
export type VaultAccount = {
  authToken: string;
  derivationPath: string;
  publicKey: string;
};

export const DEFAULT_SOLANA_DERIVATION_PATH = "m/44'/501'/0'/0'";
