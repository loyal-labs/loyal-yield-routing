import { existsSync, mkdtempSync, readFileSync, rmSync } from "node:fs";
import { join } from "node:path";
import { afterEach, describe, expect, test } from "bun:test";
import { Keypair, TransactionMessage, VersionedTransaction } from "@solana/web3.js";
import bs58 from "bs58";

import { buildRepairRecoveryCommands, runJournaledStepForTest } from "./hxtk-reset.js";
import { resolveCanonicalStateRoot } from "./hxtk-fence.js";

const roots: string[] = [];

afterEach(() => {
  for (const root of roots.splice(0)) rmSync(root, { recursive: true, force: true });
});

function fixture() {
  const root = mkdtempSync(join("/tmp", "hxtk-flow-"));
  roots.push(root);
  const stateRoot = resolveCanonicalStateRoot({ vault: "HXtk", homeDir: root, create: true });
  const journal = join(root, "flow.json");
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
  const input = (path: string, mode: "execute" | "reconcile") => ({
    mode,
    step: "flow-test",
    schema: "flow-test-schema",
    journal: path,
    rpcUrl: "offline",
    build: async () => ({
      prepared,
      plan: {
        before: { fixed: "before" },
        postState: { fixed: "after" },
        transaction: { kind: "test" },
      },
    }),
    reconcile: async () => ({ reconciled: true }),
  });
  return { root, stateRoot, journal, prepared, finalized, input };
}

function deps(fx: ReturnType<typeof fixture>, options: Readonly<{ send?: () => Promise<unknown>; finalize?: () => Promise<unknown> }> = {}) {
  return {
    stateRoot: fx.stateRoot,
    sendPreparedOnce: (options.send ?? (async () => ({ signature: fx.prepared.expectedSignature, err: null, confirmationSlot: 2 }))) as never,
    finalizedTransaction: (options.finalize ?? (async () => fx.finalized)) as never,
  };
}

describe("HXtk journaled flow", () => {
  test("repair recovery commands carry the complete parser-visible provenance", () => {
    const commands = buildRepairRecoveryCommands({
      repairJournal: "/tmp/hxtk-repair.json",
      policyJournal: "/tmp/hxtk-repair-policy.json",
      policyRemoveJournal: "/tmp/hxtk-repair.policy-remove.json",
      removalStatus: "attempted",
    });
    expect(commands).toHaveLength(2);
    for (const command of commands) {
      const args = command.split(/\s+/);
      expect(args[0]).toBe("reset:hxtk");
      expect(args).toContain("--expect-seed");
      expect(args).toContain("140");
      expect(args).toContain("--policy-journal");
      expect(args).toContain("/tmp/hxtk-repair-policy.json");
      expect(args).toContain("--journal");
    }
    expect(commands[0]).toContain("repair --reconcile");
    expect(commands[1]).toContain("repair-policy-remove --reconcile");
    expect(commands[1]).toContain("--repair-journal /tmp/hxtk-repair.json");
    expect(commands[1]).toContain("--journal /tmp/hxtk-repair.policy-remove.json");
    expect(buildRepairRecoveryCommands({
      repairJournal: "/tmp/hxtk-repair.json",
      policyJournal: "/tmp/hxtk-repair-policy.json",
      policyRemoveJournal: "/tmp/hxtk-repair.policy-remove.json",
      removalStatus: null,
    })[1]).toContain("repair-policy-remove --execute");
  });

  test("send failure leaves attempted state and reconcile finalizes without resend", async () => {
    const fx = fixture();
    let sends = 0;
    const failing = deps(fx, {
      send: async () => {
        sends += 1;
        throw new Error("submitted but response was lost");
      },
      finalize: async () => { throw new Error("not finalized"); },
    });
    await expect(runJournaledStepForTest(fx.input(fx.journal, "execute"), failing)).rejects.toThrow("response was lost");
    expect(sends).toBe(1);
    expect(JSON.parse(readFileSync(join(fx.stateRoot, "flow-test.state"), "utf8")).status).toBe("attempted");
    await expect(runJournaledStepForTest(fx.input(fx.journal, "reconcile"), failing)).rejects.toThrow("not finalized");
    expect(JSON.parse(readFileSync(join(fx.stateRoot, "flow-test.state"), "utf8")).status).toBe("attempted");

    const finalized = deps(fx, { send: async () => { sends += 1; throw new Error("must not send"); } });
    expect(await runJournaledStepForTest(fx.input(fx.journal, "reconcile"), finalized)).toBe(0);
    expect(sends).toBe(1);
    expect(existsSync(fx.journal)).toBe(true);
  });

  test("pre-send abort re-arms and concurrent executes produce one send", async () => {
    const fx = fixture();
    let sends = 0;
    const abort = { ...fx.input(fx.journal, "execute"), build: async () => ({
      prepared: fx.prepared,
      plan: { transaction: { kind: "test" } },
      beforeSend: async () => { throw new Error("prestate changed"); },
    }) };
    await expect(runJournaledStepForTest(abort, deps(fx))).rejects.toThrow("prestate changed");
    expect(sends).toBe(0);
    expect(JSON.parse(readFileSync(join(fx.stateRoot, "flow-test.state"), "utf8")).status).toBe("aborted-pre-send");

    const recovered = deps(fx, { send: async () => {
      sends += 1;
      return { signature: fx.prepared.expectedSignature, err: null, confirmationSlot: 2 };
    }});
    const recoveredInput = fx.input(join(fx.root, "recovered.json"), "execute");
    expect(await runJournaledStepForTest(recoveredInput, recovered)).toBe(0);
    expect(sends).toBe(1);

    const concurrent = fixture();
    let releaseBuild!: () => void;
    const buildGate = new Promise<void>((resolve) => { releaseBuild = resolve; });
    const first = concurrent.input(concurrent.journal, "execute");
    first.build = async () => {
      await buildGate;
      return { prepared: concurrent.prepared, plan: { transaction: { kind: "test" } } };
    };
    const send = async () => { sends += 1; return { signature: concurrent.prepared.expectedSignature, err: null, confirmationSlot: 2 }; };
    const firstRun = runJournaledStepForTest(first, deps(concurrent, { send }));
    await new Promise((resolve) => setTimeout(resolve, 0));
    await expect(runJournaledStepForTest(concurrent.input(join(concurrent.root, "other.json"), "execute"), deps(concurrent, { send })))
      .rejects.toThrow("another hxtk-reset process holds the flow-test claim");
    releaseBuild();
    expect(await firstRun).toBe(0);
  });
});
