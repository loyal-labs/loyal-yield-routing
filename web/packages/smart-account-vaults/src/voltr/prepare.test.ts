import { expect, test } from "bun:test";
import { generated } from "@loyal-labs/loyal-smart-accounts-core";
import {
  AddressLookupTableProgram,
  Connection,
  Keypair,
  SystemProgram,
  TransactionInstruction,
} from "@solana/web3.js";
import { createSmartAccountVaultsClient } from "../client";
import {
  prepareEarnMaxVoltr,
  readEarnMaxVoltrLookupTable,
  voltrWithdrawalFence,
} from "./prepare";
import { VOLTR_PROGRAM_ID } from "./program";
import { VOLTR_VAULT } from "./program";
import { sendPreparedBatchWithWallet } from "../wallet";
import type {
  SendPreparedBatchWithWalletArgs,
  WalletAdapterLike,
} from "../types";
import type { VoltrWithdrawalConsent } from "./plans";

test("all batch strategies reject protected claims before wallet or broadcast", async () => {
  const owner = Keypair.generate().publicKey;
  const settingsPda = Keypair.generate().publicKey;
  const connection = new Connection("http://fixture.invalid");
  const table = Buffer.alloc(56);
  table.writeUInt32LE(1, 0);
  table.writeBigUInt64LE(18446744073709551615n, 4);
  connection.getAccountInfoAndContext = async () => ({
    context: { slot: 11 },
    value: {
      data: table,
      owner: AddressLookupTableProgram.programId,
      executable: false,
      lamports: 1,
      rentEpoch: 0,
    },
  });
  connection.getLatestBlockhash = async () => ({
    blockhash: "11111111111111111111111111111111",
    lastValidBlockHeight: 100,
  });
  connection.getFeeForMessage = async () => ({
    context: { slot: 11 },
    value: 5000,
  });
  let walletCalls = 0;
  let broadcasts = 0;
  connection.sendRawTransaction = async () => {
    broadcasts++;
    return "unexpected";
  };
  const wallet: WalletAdapterLike = {
    publicKey: owner,
    signTransaction: async (transaction) => {
      walletCalls++;
      return transaction;
    },
    signAllTransactions: async (transactions) => {
      walletCalls++;
      return transactions;
    },
  };
  const client = createSmartAccountVaultsClient({ connection });
  client.sdk.smartAccounts.queries.fetchSettings = async () =>
    generated.Settings.fromArgs({
      seed: 0,
      settingsAuthority: owner,
      threshold: 1,
      timeLock: 0,
      transactionIndex: 0,
      staleTransactionIndex: 0,
      archivalAuthority: null,
      archivableAfter: 0,
      bump: 0,
      signers: [{ key: owner, permissions: { mask: 7 } }],
      accountUtilization: 2,
      policySeed: 0,
      reserved2: 0,
    });
  const inner = SystemProgram.transfer({
    fromPubkey: owner,
    toPubkey: settingsPda,
    lamports: 0,
  });
  client.prepareCustomInstructionSync = async () => ({
    operation: "fixture",
    payer: owner,
    programId: generated.PROGRAM_ID,
    requiresConfirmation: true,
    instructions: [inner],
    lookupTableAccounts: [],
  });
  const prepared = await prepareEarnMaxVoltr({
    client,
    connection,
    owner,
    settingsPda,
    programId: generated.PROGRAM_ID,
    build: async ({ authority }) => {
      const consent: VoltrWithdrawalConsent = {
        authority: authority.toBase58(),
        cluster: "mainnet-beta",
        destination: owner.toBase58(),
        expiresAt: "4102444800",
        feeBasisHash: "a".repeat(64),
        lpRaw: "100",
        maxUserFeeLamports: "5000",
        minClaimUsdcRaw: "100",
        nonce: "b".repeat(64),
        owner: owner.toBase58(),
        ownerSignature: "",
        program: VOLTR_PROGRAM_ID.toBase58(),
        referenceSlot: "1",
        referenceUsdcRaw: "100",
        requestInstruction: 0,
        requestSignature: "fixture",
        settings: settingsPda.toBase58(),
        vault: VOLTR_VAULT.toBase58(),
        version: "1",
        policySeed: "0",
      };
      return {
        outer: [],
        vault: [inner],
        claimProtection: {
          consent,
          connection,
          expiresAt: 4102444800n,
          maxFeeLamports: 5000n,
          settings: settingsPda.toBase58(),
          receiptBytes: new Uint8Array(112),
        },
      };
    },
  });
  const modes: NonNullable<SendPreparedBatchWithWalletArgs["sendMode"]>[] = [
    "confirm-each",
    "send-all-before-confirm",
    "sign-and-confirm-each",
  ];
  for (const sendMode of modes)
    await expect(
      sendPreparedBatchWithWallet({
        connection,
        wallet,
        prepared: [prepared],
        sendMode,
      }),
    ).rejects.toThrow("Protected withdrawals must be sent individually");
  expect(walletCalls).toBe(0);
  expect(broadcasts).toBe(0);
});

test("complete outer instructions can overflow an otherwise small prepared execute", async () => {
  const owner = Keypair.generate().publicKey;
  const settingsPda = Keypair.generate().publicKey;
  const connection = new Connection("http://fixture.invalid");
  const client = createSmartAccountVaultsClient({ connection });
  client.sdk.smartAccounts.queries.fetchSettings = async () =>
    generated.Settings.fromArgs({
      seed: 0,
      settingsAuthority: owner,
      threshold: 1,
      timeLock: 0,
      transactionIndex: 0,
      staleTransactionIndex: 0,
      archivalAuthority: null,
      archivableAfter: 0,
      bump: 0,
      signers: [{ key: owner, permissions: { mask: 7 } }],
      accountUtilization: 2,
      policySeed: 0,
      reserved2: 0,
    });
  const inner = SystemProgram.transfer({
    fromPubkey: owner,
    toPubkey: settingsPda,
    lamports: 0,
  });
  client.prepareCustomInstructionSync = async () => ({
    operation: "fixture",
    payer: owner,
    programId: generated.PROGRAM_ID,
    requiresConfirmation: true,
    instructions: [inner],
    lookupTableAccounts: [],
  });
  await expect(
    prepareEarnMaxVoltr({
      connection,
      client,
      owner,
      programId: generated.PROGRAM_ID,
      settingsPda,
      build: async () => ({
        vault: [inner],
        outer: [
          new TransactionInstruction({
            programId: SystemProgram.programId,
            keys: [],
            data: Buffer.alloc(1100),
          }),
        ],
      }),
    }),
  ).rejects.toMatchObject({ code: "transaction_too_large" });
});

test("guard wire binds receipt economics, owner and next policy seed", () => {
  const owner = Keypair.generate().publicKey;
  const authority = Keypair.generate().publicKey;
  const settingsPda = Keypair.generate().publicKey;
  const receipt = Uint8Array.from({ length: 112 }, (_, i) => i);
  const fence = voltrWithdrawalFence({
    authority,
    owner,
    settingsPda,
    programId: generated.PROGRAM_ID,
    currentPolicySeed: 4n,
    receiptBytes: receipt,
  });
  const [decoded] = generated.executeSettingsTransactionSyncStruct.deserialize(
    fence.before[0].data,
  );
  const action = decoded.args.actions[0];
  if (action.__kind !== "PolicyCreate")
    throw new Error("missing generation fence");
  expect(action.seed.toString()).toBe("5");
  expect(action.signers[0].key.equals(owner)).toBe(true);
  const policy = action.policyCreationPayload;
  if (policy.__kind !== "ProgramInteraction")
    throw new Error("missing receipt guard policy");
  const [constraint] =
    policy.fields[0].instructionsConstraints[0].accountConstraints;
  expect(constraint.accountIndex).toBe(2);
  expect(constraint.owner?.equals(VOLTR_PROGRAM_ID)).toBe(true);
  if (constraint.accountConstraint.__kind !== "AccountData")
    throw new Error("missing receipt data guard");
  const [data] = constraint.accountConstraint.fields[0];
  expect(data.dataOffset.toString()).toBe("72");
  if (data.dataValue.__kind !== "U8Slice")
    throw new Error("missing economic slice");
  expect(data.dataValue.fields[0]).toEqual(receipt.slice(72, 104));
  expect(fence.before).toHaveLength(2);
  expect(fence.after).toHaveLength(1);
});

test("lookup evidence must be active, warm and owned by the lookup program", async () => {
  const connection = new Connection("http://fixture.invalid");
  const data = Buffer.alloc(56);
  data.writeUInt32LE(1, 0);
  data.writeBigUInt64LE(18446744073709551615n, 4);
  data.writeBigUInt64LE(10n, 12);
  const snapshot = {
    context: { slot: 11 },
    value: {
      data,
      owner: AddressLookupTableProgram.programId,
      executable: false,
      lamports: 1,
      rentEpoch: 0,
    },
  };
  connection.getAccountInfoAndContext = async () => snapshot;
  expect((await readEarnMaxVoltrLookupTable(connection)).isActive()).toBe(true);
  snapshot.context.slot = 10;
  await expect(readEarnMaxVoltrLookupTable(connection)).rejects.toMatchObject({
    code: "lookup_table_unavailable",
  });
  snapshot.context.slot = 11;
  data.writeBigUInt64LE(9n, 4);
  await expect(readEarnMaxVoltrLookupTable(connection)).rejects.toMatchObject({
    code: "lookup_table_unavailable",
  });
  data.writeBigUInt64LE(18446744073709551615n, 4);
  snapshot.value.owner = SystemProgram.programId;
  await expect(readEarnMaxVoltrLookupTable(connection)).rejects.toMatchObject({
    code: "lookup_table_unavailable",
  });
});
