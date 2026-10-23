import { Keypair } from "@solana/web3.js";
import * as HardwareKey from "expo-hardware-key";

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
