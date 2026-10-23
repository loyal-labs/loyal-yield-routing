import { Keypair } from "@solana/web3.js";
import * as HardwareKey from "expo-hardware-key";
import * as LocalAuthentication from "expo-local-authentication";
import * as SecureStore from "expo-secure-store";

import { getStoredPublicKey, loadKeypair } from "./keypair-storage";

const BIOMETRIC_KEY_ALIAS = "wallet.biometric";
const BIOMETRIC_KEYPAIR_KEY = "wallet_biometric_keypair";
const LEGACY_BIOMETRIC_PIN_KEY = "wallet_biometric_password";
const BIOMETRIC_ENABLED_KEY = "wallet_biometric_enabled";

const KEYCHAIN_OPTIONS: SecureStore.SecureStoreOptions = {
  keychainAccessible: SecureStore.WHEN_UNLOCKED_THIS_DEVICE_ONLY,
};

export async function isBiometricAvailable(): Promise<boolean> {
  if (!HardwareKey.isAvailable()) return false;
  const compatible = await LocalAuthentication.hasHardwareAsync();
  if (!compatible) return false;
  const enrolled = await LocalAuthentication.isEnrolledAsync();
  return enrolled;
}

export async function getBiometricType(): Promise<"faceid" | "fingerprint" | "none"> {
  const types = await LocalAuthentication.supportedAuthenticationTypesAsync();
  if (types.includes(LocalAuthentication.AuthenticationType.FACIAL_RECOGNITION))
    return "faceid";
  if (types.includes(LocalAuthentication.AuthenticationType.FINGERPRINT))
    return "fingerprint";
  return "none";
}

export async function enableBiometrics(secretKey: Uint8Array): Promise<boolean> {
  const available = await isBiometricAvailable();
  if (!available) return false;

  await HardwareKey.ensureKey(BIOMETRIC_KEY_ALIAS, "biometry");
  const wrapped = await HardwareKey.wrap(
    BIOMETRIC_KEY_ALIAS,
    JSON.stringify(Array.from(secretKey)),
    "Enable biometric unlock",
  );
  const publicKey = Keypair.fromSecretKey(secretKey).publicKey.toBase58();
  if (!(await opensTo(wrapped, publicKey))) return false;
  await SecureStore.setItemAsync(BIOMETRIC_KEYPAIR_KEY, wrapped, KEYCHAIN_OPTIONS);
  const stored = await SecureStore.getItemAsync(BIOMETRIC_KEYPAIR_KEY);
  if (!stored || !(await opensTo(stored, publicKey))) return false;
  await SecureStore.setItemAsync(BIOMETRIC_ENABLED_KEY, "true");
  // Keep the old authenticated PIN until the persisted replacement opens.
  await SecureStore.deleteItemAsync(LEGACY_BIOMETRIC_PIN_KEY);
  return true;
}

export async function disableBiometrics(): Promise<void> {
  await SecureStore.deleteItemAsync(BIOMETRIC_KEYPAIR_KEY);
  await SecureStore.deleteItemAsync(LEGACY_BIOMETRIC_PIN_KEY);
  await SecureStore.deleteItemAsync(BIOMETRIC_ENABLED_KEY);
  await HardwareKey.deleteKey(BIOMETRIC_KEY_ALIAS);
}

export async function isBiometricEnabled(): Promise<boolean> {
  // Older installations have the flag and a biometric-protected PIN, but no
  // wrapped key. Reading that PIN here would prompt during startup.
  return (await SecureStore.getItemAsync(BIOMETRIC_ENABLED_KEY)) === "true";
}

async function opensTo(wrapped: string, publicKey: string): Promise<boolean> {
  const opened = await HardwareKey.unwrap(
    BIOMETRIC_KEY_ALIAS,
    wrapped,
    "Unlock your wallet",
  );
  if (!opened.ok) return false;
  try {
    return Keypair.fromSecretKey(new Uint8Array(JSON.parse(opened.plaintext)))
      .publicKey.toBase58() === publicKey;
  } catch {
    return false;
  }
}

export async function authenticateWithBiometrics(): Promise<Uint8Array | null> {
  try {
    const publicKey = await getStoredPublicKey();
    const wrapped = await SecureStore.getItemAsync(BIOMETRIC_KEYPAIR_KEY);
    if (wrapped) {
      const opened = await HardwareKey.unwrap(
        BIOMETRIC_KEY_ALIAS,
        wrapped,
        "Unlock your wallet",
      );
      if (opened.ok) {
        const keypair = Keypair.fromSecretKey(new Uint8Array(JSON.parse(opened.plaintext)));
        return keypair.publicKey.toBase58() === publicKey ? keypair.secretKey : null;
      }
      if (opened.reason === "canceled") return null;
    }

    const pin = await SecureStore.getItemAsync(LEGACY_BIOMETRIC_PIN_KEY, {
      requireAuthentication: true,
      authenticationPrompt: "Unlock your wallet",
    });
    if (!pin) return null;
    const keypair = await loadKeypair(pin);
    if (!keypair || keypair.publicKey.toBase58() !== publicKey) return null;
    try {
      await enableBiometrics(keypair.secretKey);
    } catch {
      // Migration is best effort. The authenticated legacy credential remains
      // usable when the native module, prompt, or secure-store write fails.
    }
    return keypair.secretKey;
  } catch {
    // SecureStore rejects when the user cancels its legacy biometric prompt.
    return null;
  }
}
