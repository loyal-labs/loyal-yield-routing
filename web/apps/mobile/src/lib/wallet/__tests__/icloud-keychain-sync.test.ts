/**
 * iCloud Keychain mirror (ASK-2162) — storage-boundary contract tests.
 *
 * Protects: the synced keychain only ever holds a copy when the user opted
 * in, reset always removes cloud copies, and a wallet restored from the
 * synced keychain unlocks with the original PIN (full encrypt/decrypt
 * round-trip through the real crypto).
 */
/* eslint-disable import/first -- the stores must exist before the jest.mock
   factories run, which happens when the mocked modules are first imported */
import { Keypair } from "@solana/web3.js";

const localStore = new Map<string, string>();
const syncedStore = new Map<string, string>();

jest.mock("expo-secure-store", () => ({
  WHEN_UNLOCKED_THIS_DEVICE_ONLY: "WHEN_UNLOCKED_THIS_DEVICE_ONLY",
  setItemAsync: jest.fn(async (k: string, v: string) => {
    localStore.set(k, v);
  }),
  getItemAsync: jest.fn(async (k: string) => localStore.get(k) ?? null),
  deleteItemAsync: jest.fn(async (k: string) => {
    localStore.delete(k);
  }),
}));

jest.mock("expo-synced-keychain", () => ({
  isAvailable: jest.fn(() => true),
  setItem: jest.fn(async (k: string, v: string) => {
    syncedStore.set(k, v);
  }),
  getItem: jest.fn(async (k: string) => syncedStore.get(k) ?? null),
  deleteItem: jest.fn(async (k: string) => {
    syncedStore.delete(k);
  }),
}));

const mockCloudWriteFile = jest.fn(async () => {});

jest.mock("react-native", () => ({ Platform: { OS: "ios" } }));

jest.mock("react-native-cloud-storage", () => ({
  CloudStorageScope: { AppData: "AppData" },
  CloudStorage: {
    isCloudAvailable: jest.fn(async () => true),
    writeFile: (...args: unknown[]) => mockCloudWriteFile(...(args as [])),
    exists: jest.fn(async () => false),
  },
}));

import { mmkv } from "@/lib/storage";

import { refreshCloudBackupIfEnabled } from "../icloud-backup";

import {
  clearStoredKeypair,
  getStoredPublicKey,
  hasStoredKeypair,
  isICloudSyncEnabled,
  keepLegacyICloudSyncOn,
  loadKeypair,
  restoreFromSyncedKeychain,
  setICloudSyncEnabled,
  storeKeypair,
} from "../keypair-storage";

const PIN = "4821";
const keypair = Keypair.fromSeed(new Uint8Array(32).fill(7));

beforeEach(async () => {
  localStore.clear();
  syncedStore.clear();
  await setICloudSyncEnabled(false);
});

test("a new wallet with no opt-in writes no iCloud Keychain or Drive copy, before or after a restart", async () => {
  mmkv.delete("settings.icloudKeychainSync");
  await storeKeypair(keypair, PIN);
  keepLegacyICloudSyncOn();
  await refreshCloudBackupIfEnabled();
  expect(syncedStore.size).toBe(0);
  expect(mockCloudWriteFile).not.toHaveBeenCalled();
  expect(isICloudSyncEnabled()).toBe(false);
});

test("storeKeypair does not touch the synced keychain when sync is off", async () => {
  await storeKeypair(keypair, PIN);
  expect(localStore.size).toBeGreaterThan(0);
  expect(syncedStore.size).toBe(0);
});

test("storeKeypair mirrors when sync is on (PIN change keeps the copy fresh)", async () => {
  await setICloudSyncEnabled(true);
  await storeKeypair(keypair, PIN);
  expect(syncedStore.get("wallet_encrypted_keypair")).toBe(
    localStore.get("wallet_encrypted_keypair"),
  );
});

test("clearStoredKeypair removes the synced copies even when sync is on", async () => {
  await setICloudSyncEnabled(true);
  await storeKeypair(keypair, PIN);
  await clearStoredKeypair();
  expect(localStore.has("wallet_encrypted_keypair")).toBe(false);
  expect(syncedStore.size).toBe(0);
});

// ASK-2206: "Remove from this device" must leave the iCloud Keychain mirror
// restorable; only "Delete everywhere" wipes it.
test("device-only clear keeps the synced copies restorable; full clear wipes them", async () => {
  await setICloudSyncEnabled(true);
  await storeKeypair(keypair, PIN);

  await clearStoredKeypair({ keepSyncedKeychain: true });
  expect(localStore.has("wallet_encrypted_keypair")).toBe(false);
  expect(localStore.has("wallet_public_key")).toBe(false);
  expect(syncedStore.has("wallet_encrypted_keypair")).toBe(true);

  // A later install can still restore and unlock with the original PIN.
  expect(await restoreFromSyncedKeychain()).toBe(true);
  const unlocked = await loadKeypair(PIN);
  expect(unlocked?.publicKey.toBase58()).toBe(keypair.publicKey.toBase58());

  await clearStoredKeypair();
  expect(syncedStore.size).toBe(0);
});

test("restore round-trip: synced blob adopts locally and unlocks with the original PIN", async () => {
  // Device A: wallet exists and syncs.
  await setICloudSyncEnabled(true);
  await storeKeypair(keypair, PIN);

  // Device B: fresh install — local empty, flag never set, synced keychain
  // carried over by iCloud. (Not setICloudSyncEnabled(false) — that is the
  // explicit opt-out and deletes the cloud copies by design.)
  localStore.clear();
  mmkv.delete("settings.icloudKeychainSync");
  expect(await hasStoredKeypair()).toBe(false);

  const restored = await restoreFromSyncedKeychain();
  expect(restored).toBe(true);
  expect(isICloudSyncEnabled()).toBe(true); // stays mirrored on the new device
  expect(await getStoredPublicKey()).toBe(keypair.publicKey.toBase58());

  const unlocked = await loadKeypair(PIN);
  expect(unlocked?.publicKey.toBase58()).toBe(keypair.publicKey.toBase58());
});

test("restore returns false when nothing is synced", async () => {
  expect(await restoreFromSyncedKeychain()).toBe(false);
  expect(await hasStoredKeypair()).toBe(false);
});
