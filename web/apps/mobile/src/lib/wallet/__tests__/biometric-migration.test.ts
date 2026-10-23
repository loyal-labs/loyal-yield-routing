import { Keypair } from "@solana/web3.js";
import * as HardwareKey from "expo-hardware-key";
import * as SecureStore from "expo-secure-store";
import { Platform } from "react-native";

import { authenticateWithBiometrics, enableBiometrics, isBiometricEnabled } from "../biometrics";
import { encryptSecret } from "../crypto";
import { loadKeypair } from "../keypair-storage";

const store = new Map<string, string>();
jest.mock("expo-secure-store", () => ({
  WHEN_UNLOCKED_THIS_DEVICE_ONLY: 0,
  getItemAsync: jest.fn((key: string) => Promise.resolve(store.get(key) ?? null)),
  setItemAsync: jest.fn((key: string, value: string) => {
    store.set(key, value);
    return Promise.resolve();
  }),
  deleteItemAsync: jest.fn((key: string) => {
    store.delete(key);
    return Promise.resolve();
  }),
}));
jest.mock("expo-local-authentication", () => ({
  hasHardwareAsync: async () => true,
  isEnrolledAsync: async () => true,
}));
jest.mock("expo-hardware-key", () => ({
  isAvailable: jest.fn(() => true),
  ensureKey: jest.fn(async () => {}),
  deleteKey: jest.fn(async () => {}),
  wrap: jest.fn(async (alias: string, plaintext: string) =>
    `${alias}|${Buffer.from(plaintext).toString("base64")}`),
  unwrap: jest.fn(async (alias: string, wrapped: string) => {
    const [owner, body] = wrapped.split("|");
    return owner === alias
      ? { ok: true, plaintext: Buffer.from(body, "base64").toString() }
      : { ok: false, reason: "failed" };
  }),
}));

const unwrap = jest.mocked(HardwareKey.unwrap);
const read = jest.mocked(SecureStore.getItemAsync);
const write = jest.mocked(SecureStore.setItemAsync);

beforeEach(() => {
  store.clear();
  jest.clearAllMocks();
  Platform.OS = "android";
});

async function seedLegacy(): Promise<Keypair> {
  const keypair = Keypair.generate();
  store.set("wallet_biometric_enabled", "true");
  store.set("wallet_biometric_password", "1234");
  store.set("wallet_public_key", keypair.publicKey.toBase58());
  store.set("wallet_encrypted_keypair", await encryptSecret(JSON.stringify(Array.from(keypair.secretKey)), "1234"));
  return keypair;
}

test("upgrade detects existing biometric login without prompting at startup", async () => {
  await seedLegacy();
  expect(await isBiometricEnabled()).toBe(true);
  expect(read.mock.calls.some(([key]) => key === "wallet_biometric_password")).toBe(false);
});

test("legacy biometric unlock keeps the old PIN until a later hardware unlock verifies the saved wallet", async () => {
  const keypair = await seedLegacy();
  const secret = await authenticateWithBiometrics();
  expect(Keypair.fromSecretKey(secret!).publicKey.toBase58()).toBe(keypair.publicKey.toBase58());
  expect(read).toHaveBeenCalledWith("wallet_biometric_password", expect.objectContaining({ requireAuthentication: true }));
  expect(store.get("wallet_biometric_password")).toBe("1234");
  expect(store.has("wallet_biometric_keypair")).toBe(true);
  read.mockClear();
  expect(await authenticateWithBiometrics()).toEqual(secret);
  expect(store.has("wallet_biometric_password")).toBe(false);
  expect(read.mock.calls.some(([key]) => key === "wallet_biometric_password")).toBe(false);
});

test.each(["android", "ios"] as const)("%s setup requests one authenticated operation and preserves legacy access", async (platform) => {
  Platform.OS = platform;
  const keypair = await seedLegacy();
  expect(await enableBiometrics(keypair.secretKey)).toBe(true);
  // Android's wrap prompts; iOS wraps without a prompt and authenticates unwrap.
  expect(HardwareKey.wrap).toHaveBeenCalledTimes(1);
  expect(unwrap).toHaveBeenCalledTimes(platform === "android" ? 0 : 1);
  expect(store.get("wallet_biometric_password")).toBe("1234");
  expect(await authenticateWithBiometrics()).toEqual(keypair.secretKey);
  expect(store.has("wallet_biometric_password")).toBe(false);
});

test("canceling Android setup does not enable biometrics or persist a key", async () => {
  jest.mocked(HardwareKey.wrap).mockRejectedValueOnce(new Error("User canceled"));
  await expect(enableBiometrics(Keypair.generate().secretKey)).rejects.toThrow("User canceled");
  expect(await isBiometricEnabled()).toBe(false);
  expect(store.has("wallet_biometric_keypair")).toBe(false);
  expect(unwrap).not.toHaveBeenCalled();
});

test("canceling iOS verification does not enable unauthenticated biometric setup", async () => {
  Platform.OS = "ios";
  unwrap.mockResolvedValueOnce({ ok: false, reason: "canceled" });
  expect(await enableBiometrics(Keypair.generate().secretKey)).toBe(false);
  expect(await isBiometricEnabled()).toBe(false);
});

test("manual PIN migration keeps legacy biometric unlock available", async () => {
  const keypair = await seedLegacy();
  await loadKeypair("1234");
  expect(store.get("wallet_biometric_password")).toBe("1234");
  expect(await authenticateWithBiometrics()).toEqual(keypair.secretKey);
});

test("older native binaries retain legacy biometric login without the hardware module", async () => {
  const keypair = await seedLegacy();
  jest.mocked(HardwareKey.isAvailable).mockReturnValue(false);
  try {
    expect(await authenticateWithBiometrics()).toEqual(keypair.secretKey);
    expect(await isBiometricEnabled()).toBe(true);
    expect(store.get("wallet_biometric_password")).toBe("1234");
    expect(store.has("wallet_encrypted_keypair")).toBe(true);
  } finally {
    jest.mocked(HardwareKey.isAvailable).mockReturnValue(true);
  }
});

test.each(["failed", "canceled", "invalidated"] as const)("%s first hardware unlock retains the legacy credential", async (reason) => {
  const keypair = await seedLegacy();
  expect(await authenticateWithBiometrics()).toEqual(keypair.secretKey);
  unwrap.mockResolvedValueOnce({ ok: false, reason });
  expect(await authenticateWithBiometrics()).toEqual(reason === "canceled" ? null : keypair.secretKey);
  expect(store.get("wallet_biometric_password")).toBe("1234");
  expect(await isBiometricEnabled()).toBe(true);
  expect(await authenticateWithBiometrics()).toEqual(keypair.secretKey);
  expect(store.has("wallet_biometric_password")).toBe(false);
});

test("a changed storage read-back cannot enable biometrics or remove the legacy credential", async () => {
  const keypair = await seedLegacy();
  store.delete("wallet_biometric_enabled");
  write.mockImplementationOnce(async (key) => { store.set(key, "corrupted"); });
  expect(await enableBiometrics(keypair.secretKey)).toBe(false);
  expect(await isBiometricEnabled()).toBe(false);
  expect(store.get("wallet_biometric_password")).toBe("1234");
  expect(unwrap).not.toHaveBeenCalled();
});

test("a hardware key for another wallet cannot unlock or retire the legacy credential", async () => {
  const keypair = await seedLegacy();
  await enableBiometrics(keypair.secretKey);
  unwrap.mockResolvedValueOnce({ ok: true, plaintext: JSON.stringify(Array.from(Keypair.generate().secretKey)) });
  expect(await authenticateWithBiometrics()).toBeNull();
  expect(store.get("wallet_biometric_password")).toBe("1234");
  expect(await authenticateWithBiometrics()).toEqual(keypair.secretKey);
  expect(store.has("wallet_biometric_password")).toBe(false);
});

test("failed legacy cleanup does not block verified hardware unlock and retries later", async () => {
  const keypair = await seedLegacy();
  await enableBiometrics(keypair.secretKey);
  jest.mocked(SecureStore.deleteItemAsync).mockRejectedValueOnce(new Error("SecureStore unavailable"));
  expect(await authenticateWithBiometrics()).toEqual(keypair.secretKey);
  expect(store.get("wallet_biometric_password")).toBe("1234");
  expect(await authenticateWithBiometrics()).toEqual(keypair.secretKey);
  expect(store.has("wallet_biometric_password")).toBe(false);
});

test("a failed biometric write keeps legacy access", async () => {
  const keypair = await seedLegacy();
  await loadKeypair("1234");
  write.mockRejectedValueOnce(new Error("SecureStore unavailable"));
  expect(await authenticateWithBiometrics()).toEqual(keypair.secretKey);
  expect(store.get("wallet_biometric_password")).toBe("1234");
  expect(await isBiometricEnabled()).toBe(true);
});

test("canceling the legacy prompt returns to PIN without changing storage", async () => {
  await seedLegacy();
  const before = new Map(store);
  const normal = read.getMockImplementation()!;
  read.mockImplementationOnce(normal).mockImplementationOnce(normal)
    .mockRejectedValueOnce(new Error("User canceled"));
  expect(await authenticateWithBiometrics()).toBeNull();
  expect(store).toEqual(before);
});

test("a mismatched wallet identity cannot migrate or unlock through the old biometric PIN", async () => {
  await seedLegacy();
  store.set("wallet_public_key", Keypair.generate().publicKey.toBase58());
  expect(await authenticateWithBiometrics()).toBeNull();
  expect(store.get("wallet_biometric_password")).toBe("1234");
  expect(store.has("wallet_biometric_keypair")).toBe(false);
});
