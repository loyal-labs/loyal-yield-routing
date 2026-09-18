/**
 * Offline regressions for the production bridge installer: the journal
 * exact-wire replay barrier and the Settings seed-only byte check. No RPC,
 * no compiler binary, no signer material.
 */
import { describe, expect, it } from "bun:test";
import { createHash } from "node:crypto";

import { generated as squadsGenerated } from "@loyal-labs/loyal-smart-accounts-core";
import { Keypair, MessageV0, PublicKey, TransactionInstruction, TransactionMessage, VersionedTransaction, type AccountInfo } from "@solana/web3.js";
import bs58 from "bs58";
import BN from "bn.js";

import { RWA_MULTIPLY_ROUTE } from "../domain/rwa-multiply-route-spec.js";
import type { CustomPolicyArtifact, CustomPolicyTarget } from "../policies/rwa-multiply-custom.js";
import {
  JOURNAL_SCHEMA,
  assertBridgeStateUnchanged,
  assertSettingsEvolvedFromJournal,
  assertSettingsOnlySeedAdvanced,
  verifyBridgeJournalEntry,
} from "./production-bridge-policies.js";

const Settings = (squadsGenerated as unknown as {
  Settings: { fromArgs(args: Record<string, unknown>): { serialize(): [Buffer, number] } };
}).Settings;

const sha256 = (bytes: Uint8Array) => createHash("sha256").update(bytes).digest("hex");

function paddedSettings(seed: number, mutate?: (args: Record<string, unknown>) => void, pad = Buffer.alloc(24)): Buffer {
  const args: Record<string, unknown> = {
    seed: new BN("0"),
    settingsAuthority: Keypair.fromSeed(new Uint8Array(32).fill(1)).publicKey,
    threshold: 1,
    timeLock: 0,
    transactionIndex: new BN("0"),
    staleTransactionIndex: new BN("0"),
    archivalAuthority: null,
    archivableAfter: new BN("0"),
    bump: 254,
    signers: [{ key: Keypair.fromSeed(new Uint8Array(32).fill(2)).publicKey, permissions: { mask: 7 } }],
    accountUtilization: 0,
    policySeed: new BN(String(seed)),
    reserved2: 0,
  };
  mutate?.(args);
  return Buffer.concat([Settings.fromArgs(args).serialize()[0]!, pad]);
}

function bridgeCreateInstruction(admin: PublicKey, candidate: PublicKey, data: number[]) {
  return {
    programId: RWA_MULTIPLY_ROUTE.squads.program,
    accounts: [
      { address: admin.toBase58(), signer: true, writable: true },
      { address: RWA_MULTIPLY_ROUTE.squads.settings, signer: false, writable: true },
      { address: candidate.toBase58(), signer: false, writable: true },
    ],
    dataBase64: Buffer.from(data).toString("base64"),
  };
}

function signedWire(admin: Keypair, create: ReturnType<typeof bridgeCreateInstruction>, blockhash: string) {
  const transaction = new VersionedTransaction(MessageV0.compile({
    payerKey: admin.publicKey,
    instructions: [new TransactionInstruction({
      programId: new PublicKey(create.programId),
      keys: create.accounts.map((account) => ({
        pubkey: new PublicKey(account.address), isSigner: account.signer, isWritable: account.writable,
      })),
      data: Buffer.from(create.dataBase64, "base64"),
    })],
    addressLookupTableAccounts: [],
    recentBlockhash: blockhash,
  }));
  transaction.sign([admin]);
  return transaction;
}

function journalFixture() {
  const admin = Keypair.fromSeed(new Uint8Array(32).fill(3));
  const candidate = PublicKey.findProgramAddressSync([
    Buffer.from("smart_account"), Buffer.from("policy"),
    new PublicKey(RWA_MULTIPLY_ROUTE.squads.settings).toBuffer(),
    (() => { const seed = Buffer.alloc(8); seed.writeBigUInt64LE(152n); return seed; })(),
  ], new PublicKey(RWA_MULTIPLY_ROUTE.squads.program))[0]!;
  const policy = { operation: "allocation" as const, seed: "152", policy: candidate.toBase58(), constraintIndex: 0, constraintIndices: [0, 1], createInstruction: bridgeCreateInstruction(admin.publicKey, candidate, [1, 2, 3, 4]) };
  const target = { route: { setupAdmin: admin.publicKey.toBase58(), squads: { program: RWA_MULTIPLY_ROUTE.squads.program } } } as unknown as CustomPolicyTarget;
  const artifact = { policies: [policy] } as unknown as CustomPolicyArtifact;
  const transaction = signedWire(admin, policy.createInstruction, "4sJCP76fR9ANKqXS9cY9LZ3uW1rBFRNdqUtoGuTJa9bP");
  const entry = {
    schema: JOURNAL_SCHEMA, index: 0, operation: policy.operation, seed: policy.seed, policy: policy.policy,
    signature: bs58.encode(transaction.signatures[0]!),
    wireBase64: Buffer.from(transaction.serialize()).toString("base64"),
    wireSha256: sha256(transaction.serialize()),
    preSettingsDataBase64: paddedSettings(151).toString("base64"),
  };
  return { admin, target, artifact, entry, transaction };
}

describe("production bridge journal exact-wire barrier", () => {
  it("accepts the exact reviewed wire", () => {
    const { target, artifact, entry } = journalFixture();
    expect(verifyBridgeJournalEntry(entry, target, artifact, 0).policy.seed).toBe("152");
  });

  it("rejects a recompiled message with different instruction data", () => {
    const { target, artifact, entry } = journalFixture();
    const tampered = { ...entry };
    (artifact.policies[0]!.createInstruction as { dataBase64: string }).dataBase64 = Buffer.from([1, 2, 3, 5]).toString("base64");
    expect(() => verifyBridgeJournalEntry(tampered, target, artifact, 0)).toThrow("exact reviewed create transaction");
  });

  it("rejects flipped account writability and dropped accounts", () => {
    const { target, artifact, entry } = journalFixture();
    const flipped = JSON.parse(JSON.stringify(entry));
    artifact.policies[0]!.createInstruction.accounts[1]!.writable = false;
    expect(() => verifyBridgeJournalEntry(flipped, target, artifact, 0)).toThrow("exact reviewed create transaction");
    artifact.policies[0]!.createInstruction.accounts = artifact.policies[0]!.createInstruction.accounts.slice(0, 2);
    expect(() => verifyBridgeJournalEntry(flipped, target, artifact, 0)).toThrow("exact reviewed create transaction");
  });

  it("rejects a signature from any key other than the admin", () => {
    const { target, artifact, entry } = journalFixture();
    const stranger = Keypair.fromSeed(new Uint8Array(32).fill(9));
    const transaction = signedWire(stranger, artifact.policies[0]!.createInstruction, "4sJCP76fR9ANKqXS9cY9LZ3uW1rBFRNdqUtoGuTJa9bP");
    const impostor = { ...entry,
      signature: bs58.encode(transaction.signatures[0]!),
      wireBase64: Buffer.from(transaction.serialize()).toString("base64"),
      wireSha256: sha256(transaction.serialize()) };
    expect(() => verifyBridgeJournalEntry(impostor, target, artifact, 0)).toThrow("signature");
  });

  it("rejects wire hash, identity, and missing pre-settings drift", () => {
    const { target, artifact, entry } = journalFixture();
    expect(() => verifyBridgeJournalEntry({ ...entry, wireBase64: Buffer.from(entry.wireBase64!).toString("base64") }, target, artifact, 0)).toThrow("hash");
    expect(() => verifyBridgeJournalEntry({ ...entry, seed: "153" }, target, artifact, 0)).toThrow("identity");
    expect(() => verifyBridgeJournalEntry({ ...entry, preSettingsDataBase64: undefined }, target, artifact, 0)).toThrow("pre-settings");
  });
});

describe("settings seed-only byte check", () => {
  it("accepts the canonical seed advance with zero padding", () => {
    expect(() => assertSettingsOnlySeedAdvanced(paddedSettings(151), paddedSettings(152), 152n, "t")).not.toThrow();
    expect(() => assertSettingsOnlySeedAdvanced(paddedSettings(154), paddedSettings(155), 155n, "t")).not.toThrow();
  });

  it("rejects authority changes, seed drift, and non-zero trailing bytes", () => {
    expect(() => assertSettingsOnlySeedAdvanced(paddedSettings(151), paddedSettings(152, (args) => { args.threshold = 2; }), 152n, "t")).toThrow("policy seed");
    expect(() => assertSettingsOnlySeedAdvanced(paddedSettings(151), paddedSettings(152, (args) => { (args.signers as { permissions: { mask: number } }[])[0]!.permissions.mask = 3; }), 152n, "t")).toThrow("policy seed");
    expect(() => assertSettingsOnlySeedAdvanced(paddedSettings(151), paddedSettings(152), 153n, "t")).toThrow("policy seed");
    expect(() => assertSettingsOnlySeedAdvanced(paddedSettings(151, undefined, Buffer.alloc(24, 0xff)), paddedSettings(152), 152n, "t")).toThrow("canonical seed-only advance");
    expect(() => assertSettingsOnlySeedAdvanced(paddedSettings(151), paddedSettings(152, undefined, Buffer.alloc(24, 0xff)), 152n, "t")).toThrow("canonical seed-only advance");
    expect(() => assertSettingsOnlySeedAdvanced(paddedSettings(151, (args) => { args.policySeed = null; }), paddedSettings(152), 152n, "t")).toThrow("canonical seed-only advance");
  });
});

describe("settings journal evolution (recovery)", () => {
  const pre = () => paddedSettings(151);
  const recoveredFrom = (current: Buffer, floor = 152n) =>
    assertSettingsEvolvedFromJournal(pre(), current, floor, 155n, "t");

  it("accepts the journaled pre-state with only the seed advanced", () => {
    expect(recoveredFrom(paddedSettings(152))).toBe(152n);
    expect(recoveredFrom(paddedSettings(155))).toBe(155n);
  });

  it("rejects seed windows, authority drift, non-seed bytes, and non-canonical bytes", () => {
    expect(() => recoveredFrom(paddedSettings(151))).toThrow("window");
    expect(() => recoveredFrom(paddedSettings(156))).toThrow("window");
    expect(() => recoveredFrom(paddedSettings(154, (args) => { args.threshold = 2; }))).toThrow("authority");
    expect(() => recoveredFrom(paddedSettings(154, (args) => { args.accountUtilization = 1; }))).toThrow("only the policy seed");
    expect(() => recoveredFrom(paddedSettings(154, undefined, Buffer.alloc(24, 0xff)))).toThrow("only the policy seed");
    expect(() => assertSettingsEvolvedFromJournal(
      paddedSettings(151, undefined, Buffer.alloc(24, 0xff)), paddedSettings(154), 152n, 155n, "t")).toThrow("not canonical");
  });
});

describe("protected-state drift diagnostics", () => {
  const driftAccount = (lamports: number, data: number[]) => ({
    owner: Keypair.fromSeed(new Uint8Array(32).fill(5)).publicKey,
    executable: false,
    lamports,
    data: Buffer.from(data),
    rentEpoch: 0,
  }) as AccountInfo<Buffer>;

  it("reports changed addresses and equality kinds without raw data bytes", () => {
    const key = Keypair.fromSeed(new Uint8Array(32).fill(4)).publicKey;
    let message = "";
    try {
      assertBridgeStateUnchanged([key], [driftAccount(1_000_000, [1, 2, 3])], [driftAccount(990_000, [1, 2, 3])]);
    } catch (error) {
      message = (error as Error).message;
    }
    expect(message).toContain("Bridge protected state changed before journaling");
    expect(message).toContain(key.toBase58());
    expect(message).toContain("lamports 1000000 -> 990000");
    expect(message).not.toContain("1,2,3");
    expect(message).not.toContain(process.env.SOLANA_RPC_URL ?? "https://");
  });

  it("reports existence changes and stays silent on identical accounts", () => {
    const key = Keypair.fromSeed(new Uint8Array(32).fill(6)).publicKey;
    expect(() => assertBridgeStateUnchanged([key], [driftAccount(5, [9])], [null])).toThrow(`existence present -> absent`);
    expect(() => assertBridgeStateUnchanged([key], [driftAccount(5, [9])], [driftAccount(5, [9])])).not.toThrow();
  });
});
