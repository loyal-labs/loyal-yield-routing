import { Keypair } from "@solana/web3.js";
import * as HardwareKey from "expo-hardware-key";

import { encryptSecret } from "../crypto";
import { importKeypair, loadKeypair, WalletKeyReadError, WalletKeyUnavailableError } from "../keypair-storage";

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

jest.mock("expo-hardware-key", () => ({
  isAvailable: () => true,
  ensureKey: jest.fn(() => Promise.resolve()),
  deleteKey: jest.fn(() => Promise.resolve()),
  wrap: jest.fn((alias: string, plaintext: string) =>
    Promise.resolve(`${alias}|${Buffer.from(plaintext).toString("base64")}`),
  ),
  unwrap: jest.fn((alias: string, wrapped: string) => {
    const [owner, body] = wrapped.split("|");
    return Promise.resolve(
      owner === alias
        ? { ok: true, plaintext: Buffer.from(body, "base64").toString() }
        : { ok: false, reason: "failed" },
    );
  }),
}));

const unwrap = HardwareKey.unwrap as jest.MockedFunction<typeof HardwareKey.unwrap>;

async function seedLegacyWallet(pin: string): Promise<Keypair> {
  const keypair = Keypair.generate();
  store.set(
    "wallet_encrypted_keypair",
    await encryptSecret(JSON.stringify(Array.from(keypair.secretKey)), pin),
  );
  store.set("wallet_public_key", keypair.publicKey.toBase58());
  return keypair;
}

beforeEach(() => store.clear());

test("a PIN unlock rewraps the legacy blob under the hardware key, then deletes it", async () => {
  const keypair = await seedLegacyWallet("1234");

  const unlocked = await loadKeypair("1234");

  expect(unlocked?.publicKey.toBase58()).toBe(keypair.publicKey.toBase58());
  expect(store.has("wallet_encrypted_keypair")).toBe(false);
  expect(store.get("wallet_hardware_keypair")?.startsWith("wallet.pin|")).toBe(true);
  const again = await loadKeypair("1234");
  expect(again?.publicKey.toBase58()).toBe(keypair.publicKey.toBase58());
});

test("a rewrap whose round-trip fails keeps the legacy blob", async () => {
  const keypair = await seedLegacyWallet("1234");
  const legacy = store.get("wallet_encrypted_keypair");
  unwrap.mockResolvedValueOnce({ ok: false, reason: "failed" });

  const unlocked = await loadKeypair("1234");

  expect(unlocked?.publicKey.toBase58()).toBe(keypair.publicKey.toBase58());
  expect(store.get("wallet_encrypted_keypair")).toBe(legacy);
  expect(store.has("wallet_hardware_keypair")).toBe(false);
});

test.each(["failed", "canceled"] as const)("%s hardware access stays retryable without changing credentials or PIN attempts", async (reason) => {
  const keypair = await seedLegacyWallet("1234");
  await loadKeypair("1234");
  const before = new Map(store);
  unwrap.mockResolvedValueOnce({ ok: false, reason });
  await expect(loadKeypair("1234")).rejects.toBeInstanceOf(WalletKeyReadError);
  expect(store).toEqual(before);
  expect((await loadKeypair("1234"))?.publicKey.toBase58()).toBe(keypair.publicKey.toBase58());
});

test("an invalidated hardware key offers recovery while keeping all wallet records", async () => {
  await seedLegacyWallet("1234");
  await loadKeypair("1234");
  const before = new Map(store);
  unwrap.mockResolvedValueOnce({ ok: false, reason: "invalidated" });
  await expect(loadKeypair("1234")).rejects.toBeInstanceOf(WalletKeyUnavailableError);
  expect(store).toEqual(before);
});

test("recovery rejects another wallet or malformed key before touching stored credentials", async () => {
  const keypair = await seedLegacyWallet("1234");
  const before = new Map(store);
  await expect(importKeypair(Keypair.generate().secretKey, "5678", keypair.publicKey.toBase58())).rejects.toThrow("different wallet");
  await expect(importKeypair(new Uint8Array(64), "5678", keypair.publicKey.toBase58())).rejects.toThrow();
  expect(store).toEqual(before);
});

test("recovery of the same wallet replaces an unusable hardware blob and clears PIN lockout", async () => {
  const keypair = await seedLegacyWallet("1234");
  await loadKeypair("1234");
  store.set("wallet_failed_attempts", "4");
  store.set("wallet_locked_until", String(Date.now() + 30_000));
  unwrap.mockResolvedValueOnce({ ok: false, reason: "invalidated" });
  await importKeypair(keypair.secretKey, "5678", keypair.publicKey.toBase58());
  expect(store.has("wallet_locked_until")).toBe(false);
  expect((await loadKeypair("5678"))?.publicKey.toBase58()).toBe(keypair.publicKey.toBase58());
});
