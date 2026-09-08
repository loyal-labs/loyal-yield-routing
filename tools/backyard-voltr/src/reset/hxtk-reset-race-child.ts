import { appendFileSync, existsSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { Keypair, TransactionMessage, VersionedTransaction } from "@solana/web3.js";
import bs58 from "bs58";

import { runJournaledStepForTest } from "./hxtk-reset.js";

function requiredEnv(name: string): string {
  const value = process.env[name];
  if (!value) throw new Error(`race child is missing ${name}`);
  return value;
}

const stateRoot = requiredEnv("HXTK_RACE_STATE_ROOT");
const journal = requiredEnv("HXTK_RACE_JOURNAL");
const startFile = requiredEnv("HXTK_RACE_START_FILE");
const sendCounter = requiredEnv("HXTK_RACE_SEND_COUNTER");
const raceDirectory = requiredEnv("HXTK_RACE_DIRECTORY");
const childId = process.env.HXTK_RACE_CHILD_ID ?? "unknown";

if (process.env.HXTK_FAKE_RPC !== "1") throw new Error("race child requires HXTK_FAKE_RPC=1");

async function waitForStart(): Promise<void> {
  for (let attempt = 0; attempt < 200; attempt += 1) {
    if (existsSync(startFile)) return;
    await new Promise((resolve) => setTimeout(resolve, 10));
  }
  throw new Error("race child start barrier timed out");
}

async function waitForPeers(prefix: string): Promise<void> {
  const peerPaths = ["0", "1"].map((peerId) => join(raceDirectory, `${prefix}-${peerId}`));
  for (let attempt = 0; attempt < 2_000; attempt += 1) {
    if (peerPaths.every((path) => existsSync(path))) return;
    await new Promise((resolve) => setTimeout(resolve, 10));
  }
  throw new Error(`race child ${childId} timed out waiting for ${prefix} peers`);
}

async function waitForOtherChildDone(): Promise<void> {
  const otherId = childId === "0" ? "1" : "0";
  const otherPath = join(raceDirectory, `done-${otherId}`);
  for (let attempt = 0; attempt < 2_000; attempt += 1) {
    if (existsSync(otherPath)) return;
    await new Promise((resolve) => setTimeout(resolve, 10));
  }
  throw new Error(`race child ${childId} timed out waiting for done-${otherId}`);
}

async function main(): Promise<void> {
  await waitForStart();
  await new Promise((resolve) => setTimeout(resolve, 25));
  const signer = Keypair.generate();
  const message = new TransactionMessage({
    payerKey: signer.publicKey,
    recentBlockhash: signer.publicKey.toBase58(),
    instructions: [],
  }).compileToV0Message();
  const transaction = new VersionedTransaction(message);
  transaction.sign([signer]);
  const serializedTransaction = Uint8Array.from(transaction.serialize());
  const serializedMessage = Uint8Array.from(transaction.message.serialize());
  const signature = bs58.encode(transaction.signatures[0]!);
  const prepared = {
    cluster: "mainnet-beta" as const,
    genesisHash: "genesis",
    commitment: "finalized" as const,
    serializedTransaction,
    serializedMessage,
    expectedSignature: signature,
    latestBlockhash: { blockhash: signer.publicKey.toBase58(), lastValidBlockHeight: 1 },
    prestateSlot: 1,
    simulationSlot: 1,
    packetBytes: serializedTransaction.length,
    feeLamports: 0,
    simulation: { err: null, unitsConsumed: 1, logs: [], returnData: null, postAccounts: [] },
  };
  const finalized = {
    slot: 2,
    blockTime: 2,
    transaction: {
      signatures: [signature],
      message: { serialize: () => serializedMessage },
    },
    meta: { err: null },
  } as never;
  const fakeClaim = {
    path: join(stateRoot, "race-test.claim"),
    tokenPath: join(stateRoot, "race-test.claim." + childId),
    inode: 1,
    pid: process.pid,
    startedAtUnixMs: Date.now(),
    processStartTime: "test-" + process.pid,
    journal,
    hostname: "race-test",
    token: (childId + "00000000000000000000000000000000").slice(0, 32),
  } as never;
  const input = {
    mode: "execute" as const,
    step: "race-test",
    schema: "race-test-schema",
    journal,
    rpcUrl: "fake://rpc",
    build: async () => {
      // The barrier is inside build so both children have already read the
      // same canonical generation before either attempts its first election.
      writeFileSync(join(raceDirectory, `build-ready-${childId}`), "ready\n", { mode: 0o600 });
      await waitForPeers("build-ready");
      return {
        prepared,
        plan: { transaction: { kind: "race-test", childId } },
      };
    },
    reconcile: async () => ({ reconciled: true }),
  };
  const result = await runJournaledStepForTest(input, {
    stateRoot,
    claim: fakeClaim,
    sendPreparedOnce: (async () => {
      await waitForOtherChildDone();
      appendFileSync(sendCounter, String(process.pid) + "\n");
      return { signature, err: null, confirmationSlot: 2 };
    }) as never,
    finalizedTransaction: (async () => finalized) as never,
  });
  console.log("RACE_RESULT " + JSON.stringify({ childId, ok: true, result }));
}

try {
  await main();
} catch (error) {
  writeFileSync(join(raceDirectory, `done-${childId}`), "done\n", { mode: 0o600 });
  console.log("RACE_RESULT " + JSON.stringify({
    childId,
    ok: false,
    error: error instanceof Error ? error.message : String(error),
  }));
}
