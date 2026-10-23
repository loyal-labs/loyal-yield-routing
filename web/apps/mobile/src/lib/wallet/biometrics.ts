import * as HardwareKey from "expo-hardware-key";
import * as LocalAuthentication from "expo-local-authentication";
import * as SecureStore from "expo-secure-store";

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
  if (!available) {
    await disableBiometrics();
    return false;
  }

  await HardwareKey.replaceKey(BIOMETRIC_KEY_ALIAS, "biometry");
  const wrapped = await HardwareKey.wrap(
    BIOMETRIC_KEY_ALIAS,
    JSON.stringify(Array.from(secretKey)),
    "Enable biometric unlock",
  );
  await SecureStore.setItemAsync(BIOMETRIC_KEYPAIR_KEY, wrapped, KEYCHAIN_OPTIONS);
  await SecureStore.deleteItemAsync(LEGACY_BIOMETRIC_PIN_KEY);
  await SecureStore.setItemAsync(BIOMETRIC_ENABLED_KEY, "true");
  return true;
}

export async function disableBiometrics(): Promise<void> {
  await SecureStore.deleteItemAsync(BIOMETRIC_KEYPAIR_KEY);
  await SecureStore.deleteItemAsync(LEGACY_BIOMETRIC_PIN_KEY);
  await SecureStore.deleteItemAsync(BIOMETRIC_ENABLED_KEY);
  await HardwareKey.deleteKey(BIOMETRIC_KEY_ALIAS);
}

export async function isBiometricEnabled(): Promise<boolean> {
  const value = await SecureStore.getItemAsync(BIOMETRIC_ENABLED_KEY);
  if (value !== "true") return false;
  return (await SecureStore.getItemAsync(BIOMETRIC_KEYPAIR_KEY)) !== null;
}

export async function deleteLegacyBiometricPin(): Promise<void> {
  await SecureStore.deleteItemAsync(LEGACY_BIOMETRIC_PIN_KEY);
}

export async function authenticateWithBiometrics(): Promise<Uint8Array | null> {
  const wrapped = await SecureStore.getItemAsync(BIOMETRIC_KEYPAIR_KEY);
  if (!wrapped) return null;
  const opened = await HardwareKey.unwrap(
    BIOMETRIC_KEY_ALIAS,
    wrapped,
    "Unlock your wallet",
  );
  if (opened.ok) {
    try {
      return new Uint8Array(JSON.parse(opened.plaintext));
    } catch {
      return null;
    }
  }
  if (opened.reason === "invalidated") await disableBiometrics();
  return null;
}
