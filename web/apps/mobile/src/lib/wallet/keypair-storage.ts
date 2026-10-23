import { Keypair } from "@solana/web3.js";
import * as HardwareKey from "expo-hardware-key";
import * as SecureStore from "expo-secure-store";

import { decryptSecret, encryptSecret } from "./crypto";
import { isValidWalletPin } from "./pin";

const KEYCHAIN_OPTIONS: SecureStore.SecureStoreOptions = {
  keychainAccessible: SecureStore.WHEN_UNLOCKED_THIS_DEVICE_ONLY,
};

const LEGACY_KEYPAIR_KEY = "wallet_encrypted_keypair";
const HARDWARE_KEYPAIR_KEY = "wallet_hardware_keypair";
const PIN_KEY_ALIAS = "wallet.pin";
const UNWRAP_PROMPT = "Unlock your wallet";
const WALLET_PUBLIC_KEY = "wallet_public_key";

const FAILED_ATTEMPTS_KEY = "wallet_failed_attempts";
const LOCKED_UNTIL_KEY = "wallet_locked_until";

const LOCKOUT_DURATIONS_MS = [
  30_000,
  60_000, // 5th failure  -> 1 min
  300_000, // 6th failure  -> 5 min
  900_000, // 7th failure  -> 15 min
  3_600_000, // 8th failure  -> 1 hour
  14_400_000, // 9th failure  -> 4 hours
  86_400_000, // 10th+ failure -> 24 hours
];

function getLockoutDuration(attempts: number): number {
  if (attempts < 4) return 0;
  const index = Math.min(attempts - 4, LOCKOUT_DURATIONS_MS.length - 1);
  return LOCKOUT_DURATIONS_MS[index];
}

export class WalletKeyUnavailableError extends Error {
  constructor() {
    super("This wallet's hardware key is not usable on this device");
    this.name = "WalletKeyUnavailableError";
  }
}

export class WalletKeyReadError extends Error {
  constructor() {
    super("Could not access your wallet key. Please try unlocking again.");
    this.name = "WalletKeyReadError";
  }
}

export class PinLockedError extends Error {
  remainingMs: number;
  constructor(remainingMs: number) {
    super(`Wallet locked for ${Math.ceil(remainingMs / 1000)}s`);
    this.name = "PinLockedError";
    this.remainingMs = remainingMs;
  }
}

export async function getLockoutRemaining(): Promise<number> {
  const raw = await SecureStore.getItemAsync(LOCKED_UNTIL_KEY);
  if (!raw) return 0;
  return Math.max(0, Number(raw) - Date.now());
}

async function recordFailedAttempt(): Promise<void> {
  const raw = await SecureStore.getItemAsync(FAILED_ATTEMPTS_KEY);
  const attempts = (raw ? Number(raw) : 0) + 1;
  await SecureStore.setItemAsync(FAILED_ATTEMPTS_KEY, String(attempts));
  const duration = getLockoutDuration(attempts);
  if (duration > 0) {
    await SecureStore.setItemAsync(
      LOCKED_UNTIL_KEY,
      String(Date.now() + duration),
    );
  }
}

export async function resetAttempts(): Promise<void> {
  await SecureStore.deleteItemAsync(FAILED_ATTEMPTS_KEY);
  await SecureStore.deleteItemAsync(LOCKED_UNTIL_KEY);
}

async function openPinBlob(blob: string, pin: string): Promise<Keypair | null> {
  const decrypted = await decryptSecret(blob, pin);
  if (!decrypted) return null;
  try {
    return Keypair.fromSecretKey(new Uint8Array(JSON.parse(decrypted)));
  } catch {
    return null;
  }
}

async function opensTo(
  wrapped: string,
  pin: string,
  publicKey: string,
): Promise<boolean> {
  const opened = await HardwareKey.unwrap(PIN_KEY_ALIAS, wrapped, UNWRAP_PROMPT);
  if (!opened.ok) return false;
  const keypair = await openPinBlob(opened.plaintext, pin);
  return keypair?.publicKey.toBase58() === publicKey;
}

async function sealUnderHardwareKey(
  pinBlob: string,
  pin: string,
  publicKey: string,
): Promise<boolean> {
  if (!HardwareKey.isAvailable()) return false;
  try {
    await HardwareKey.ensureKey(PIN_KEY_ALIAS, "device");
    const wrapped = await HardwareKey.wrap(PIN_KEY_ALIAS, pinBlob, UNWRAP_PROMPT);
    if (!(await opensTo(wrapped, pin, publicKey))) return false;
    await SecureStore.setItemAsync(
      HARDWARE_KEYPAIR_KEY,
      wrapped,
      KEYCHAIN_OPTIONS,
    );
    const stored = await SecureStore.getItemAsync(HARDWARE_KEYPAIR_KEY);
    return stored !== null && (await opensTo(stored, pin, publicKey));
  } catch (error) {
    console.warn("[wallet] hardware key seal failed", error);
    return false;
  }
}

export async function storeKeypair(
  keypair: Keypair,
  pin: string,
): Promise<void> {
  if (!isValidWalletPin(pin)) {
    throw new Error("PIN must be 4 digits");
  }
  const serialized = JSON.stringify(Array.from(keypair.secretKey));
  const pinBlob = await encryptSecret(serialized, pin);
  const publicKey = keypair.publicKey.toBase58();
  if (await sealUnderHardwareKey(pinBlob, pin, publicKey)) {
    await SecureStore.deleteItemAsync(LEGACY_KEYPAIR_KEY);
  } else {
    await SecureStore.setItemAsync(
      LEGACY_KEYPAIR_KEY,
      pinBlob,
      KEYCHAIN_OPTIONS,
    );
    await SecureStore.deleteItemAsync(HARDWARE_KEYPAIR_KEY);
  }
  await SecureStore.setItemAsync(WALLET_PUBLIC_KEY, publicKey, KEYCHAIN_OPTIONS);
}

export function generateKeypairInMemory(): Keypair {
  return Keypair.generate();
}

export async function importKeypair(
  secretKey: Uint8Array,
  pin: string,
  expectedPublicKey?: string,
): Promise<Keypair> {
  const keypair = Keypair.fromSecretKey(secretKey);
  if (expectedPublicKey && keypair.publicKey.toBase58() !== expectedPublicKey) {
    throw new Error("This secret key belongs to a different wallet. Use this wallet's secret key.");
  }
  await storeKeypair(keypair, pin);
  if (expectedPublicKey) await resetAttempts();
  return keypair;
}

export async function loadKeypair(pin: string): Promise<Keypair | null> {
  const remaining = await getLockoutRemaining();
  if (remaining > 0) throw new PinLockedError(remaining);

  const wrapped = await SecureStore.getItemAsync(HARDWARE_KEYPAIR_KEY);
  const legacy = await SecureStore.getItemAsync(LEGACY_KEYPAIR_KEY);
  const opened = wrapped
    ? await HardwareKey.unwrap(PIN_KEY_ALIAS, wrapped, UNWRAP_PROMPT)
    : null;
  const pinBlob = opened?.ok ? opened.plaintext : legacy;
  if (!pinBlob) {
    if (wrapped) {
      if (opened && !opened.ok && opened.reason === "invalidated") {
        throw new WalletKeyUnavailableError();
      }
      throw new WalletKeyReadError();
    }
    return null;
  }

  const keypair = await openPinBlob(pinBlob, pin);
  if (!keypair) {
    await recordFailedAttempt();
    return null;
  }
  await resetAttempts();

  if (legacy) {
    const sealed =
      opened?.ok ||
      (await sealUnderHardwareKey(legacy, pin, keypair.publicKey.toBase58()));
    if (sealed) await SecureStore.deleteItemAsync(LEGACY_KEYPAIR_KEY);
  }
  return keypair;
}

export async function hasStoredKeypair(): Promise<boolean> {
  return (
    (await SecureStore.getItemAsync(HARDWARE_KEYPAIR_KEY)) !== null ||
    (await SecureStore.getItemAsync(LEGACY_KEYPAIR_KEY)) !== null
  );
}

export async function getStoredPublicKey(): Promise<string | null> {
  return SecureStore.getItemAsync(WALLET_PUBLIC_KEY);
}

export async function clearStoredKeypair(): Promise<void> {
  await SecureStore.deleteItemAsync(HARDWARE_KEYPAIR_KEY);
  await SecureStore.deleteItemAsync(LEGACY_KEYPAIR_KEY);
  await SecureStore.deleteItemAsync(WALLET_PUBLIC_KEY);
  await HardwareKey.deleteKey(PIN_KEY_ALIAS);
  await resetAttempts();
}

export async function changePin(
  keypair: Keypair,
  newPin: string,
): Promise<void> {
  await storeKeypair(keypair, newPin);
}
