import { Platform } from "react-native";

import ExpoSeedVault, { type NativeVaultAccount } from "./ExpoSeedVault";
import { DEFAULT_SOLANA_DERIVATION_PATH, type VaultAccount } from "./types";

export { DEFAULT_SOLANA_DERIVATION_PATH } from "./types";
export type { VaultAccount } from "./types";

const BASE58_ALPHABET =
  "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz";

function uint8ToBase64(bytes: Uint8Array): string {
  let binary = "";
  for (let i = 0; i < bytes.length; i++) {
    binary += String.fromCharCode(bytes[i]);
  }
  return globalThis.btoa(binary);
}

function base64ToUint8(b64: string): Uint8Array {
  const binary = globalThis.atob(b64);
  const out = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i++) {
    out[i] = binary.charCodeAt(i);
  }
  return out;
}

function encodeBase58(bytes: Uint8Array): string {
  if (bytes.length === 0) return "";
  let value = 0n;
  for (const byte of bytes) {
    value = (value << 8n) + BigInt(byte);
  }
  let encoded = "";
  const base = 58n;
  while (value > 0n) {
    const remainder = Number(value % base);
    encoded = BASE58_ALPHABET[remainder] + encoded;
    value /= base;
  }
  for (const byte of bytes) {
    if (byte !== 0) break;
    encoded = `1${encoded}`;
  }
  return encoded || "1";
}

function toVaultAccount(native: NativeVaultAccount): VaultAccount {
  return {
    // Legacy binaries return the token as a number; normalize to string.
    authToken: String(native.authToken),
    derivationPath: native.derivationPath,
    publicKey: encodeBase58(base64ToUint8(native.publicKey)),
  };
}

/**
 * Pick the wire format the installed binary understands. Binaries built
 * before the string-token bridge (reachable via OTA updates) expect a JS
 * number — safe there in practice because real vault tokens are small
 * database row IDs, far below 2^53.
 */
function nativeAuthToken(authToken: string): string | number {
  return ExpoSeedVault.stringAuthTokens === true
    ? authToken
    : Number(authToken);
}

export async function isAvailable(): Promise<boolean> {
  if (Platform.OS !== "android") return false;
  try {
    return await ExpoSeedVault.isAvailable();
  } catch {
    return false;
  }
}

/**
 * Prompt the user for the dangerous-level Seed Vault permission. Must be
 * called and granted before any auth/sign flow — without it the SDK throws
 * `IllegalStateException("No access to Seed Vault…")`.
 *
 * Returns `false` on iOS, on devices without Seed Vault, and on user denial.
 * Safe to call repeatedly; resolves immediately if the permission is already
 * held.
 */
export async function requestPermission(): Promise<boolean> {
  if (Platform.OS !== "android") return false;
  try {
    return await ExpoSeedVault.requestPermission();
  } catch {
    return false;
  }
}

export async function authorizeExistingSeed(
  derivationPath: string = DEFAULT_SOLANA_DERIVATION_PATH,
): Promise<VaultAccount> {
  const native = await ExpoSeedVault.authorizeExistingSeed(derivationPath);
  return toVaultAccount(native);
}

export async function listAuthorizedSeeds(
  derivationPath: string = DEFAULT_SOLANA_DERIVATION_PATH,
): Promise<VaultAccount[]> {
  if (Platform.OS !== "android") return [];
  try {
    const natives = await ExpoSeedVault.listAuthorizedSeeds(derivationPath);
    return natives.map(toVaultAccount);
  } catch {
    return [];
  }
}

/**
 * Prompt the user to generate a new seed inside the vault and authorize it
 * for this app. The fresh 24-word seed never leaves the vault.
 */
export async function createNewSeed(
  derivationPath: string = DEFAULT_SOLANA_DERIVATION_PATH,
): Promise<VaultAccount> {
  const native = await ExpoSeedVault.createNewSeed(derivationPath);
  return toVaultAccount(native);
}

export async function importSeed(
  derivationPath: string = DEFAULT_SOLANA_DERIVATION_PATH,
): Promise<VaultAccount> {
  const native = await ExpoSeedVault.importSeed(derivationPath);
  return toVaultAccount(native);
}

/**
 * Release this app's authorization for the given seed. The seed itself stays
 * in the vault and can be re-authorized later.
 */
export async function deauthorize(authToken: string): Promise<void> {
  await ExpoSeedVault.deauthorize(nativeAuthToken(authToken));
}

export async function signTransaction(args: {
  authToken: string;
  derivationPath: string;
  txBytes: Uint8Array;
}): Promise<Uint8Array> {
  const sigB64 = await ExpoSeedVault.signTransaction(
    nativeAuthToken(args.authToken),
    args.derivationPath,
    uint8ToBase64(args.txBytes),
  );
  return base64ToUint8(sigB64);
}

/**
 * Ask the vault to sign a batch of transactions in a single authorization —
 * one biometric/PIN prompt for the whole batch. Signatures come back in
 * input order. Callers must respect the vault's per-prompt request limit
 * (3 on current Saga/Seeker devices).
 */
export async function signTransactions(args: {
  authToken: string;
  derivationPath: string;
  txs: Uint8Array[];
}): Promise<Uint8Array[]> {
  // Dev clients built before the batch API lack the native function — fall
  // back to one prompt per transaction rather than crashing.
  if (typeof ExpoSeedVault.signTransactions !== "function") {
    const signatures: Uint8Array[] = [];
    for (const tx of args.txs) {
      signatures.push(
        await signTransaction({
          authToken: args.authToken,
          derivationPath: args.derivationPath,
          txBytes: tx,
        }),
      );
    }
    return signatures;
  }
  const sigsB64 = await ExpoSeedVault.signTransactions(
    nativeAuthToken(args.authToken),
    args.derivationPath,
    args.txs.map(uint8ToBase64),
  );
  return sigsB64.map(base64ToUint8);
}

export async function signMessage(args: {
  authToken: string;
  derivationPath: string;
  message: Uint8Array;
}): Promise<Uint8Array> {
  const sigB64 = await ExpoSeedVault.signMessage(
    nativeAuthToken(args.authToken),
    args.derivationPath,
    uint8ToBase64(args.message),
  );
  return base64ToUint8(sigB64);
}

/**
 * Look up the public key for an already-authorized derivation path without
 * prompting the user. Only works for paths the vault has pre-derived at
 * authorization time (the Solana defaults are pre-derived).
 */
export async function getPublicKey(args: {
  authToken: string;
  derivationPath: string;
}): Promise<string> {
  const b64 = await ExpoSeedVault.getPublicKey(
    nativeAuthToken(args.authToken),
    args.derivationPath,
  );
  return encodeBase58(base64ToUint8(b64));
}

export const __test__ = { encodeBase58, uint8ToBase64, base64ToUint8 };
