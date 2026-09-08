import { appendFileSync, existsSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { Keypair, TransactionMessage, VersionedTransaction } from "@solana/web3.js";
import bs58 from "bs58";

import { runJournaledStepForTest } from "./hxtk-reset.js";
import { acquireCanonicalLegClaim, releaseCanonicalLegClaim } from "./hxtk-fence.js";

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

async function main(): Promise<void> {
  await waitForStart();
  // Both processes reach the real hard-link claim election together. The
  // loser never reaches build or raw submission.
  writeFileSync(join(raceDirectory, `claim-ready-${childId}`), "ready\n", { mode: 0o600 });
  await waitForPeers("claim-ready");
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
  const input = {
    mode: "execute" as const,
    step: "race-test",
    schema: "race-test-schema",
    journal,
    rpcUrl: "fake://rpc",
    build: async () => {
      return {
        prepared,
        plan: { transaction: { kind: "race-test", childId } },
      };
    },
    reconcile: async () => ({ reconciled: true }),
  };
  let claim;
  try {
    claim = acquireCanonicalLegClaim({
      stateRoot,
      step: "race-test",
      journal,
      ...(process.env.HXTK_RACE_BREAK_CLAIM === "1" ? { breakClaim: true } : {}),
    });
    writeFileSync(join(raceDirectory, `claim-result-${childId}`), "ok\n", { mode: 0o600 });
  } catch (error) {
    writeFileSync(join(raceDirectory, `claim-result-${childId}`), "error\n", { mode: 0o600 });
    throw error;
  }
  await waitForPeers("claim-result");
  try {
    const result = await runJournaledStepForTest(input, {
      stateRoot,
      claim,
      sendPreparedOnce: (async () => {
        appendFileSync(sendCounter, String(process.pid) + "\n");
        return { signature, err: null, confirmationSlot: 2 };
      }) as never,
      finalizedTransaction: (async () => finalized) as never,
    });
    console.log("RACE_RESULT " + JSON.stringify({ childId, ok: true, result }));
  } finally {
    releaseCanonicalLegClaim(claim);
  }
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
