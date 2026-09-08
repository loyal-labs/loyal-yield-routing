import { existsSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { afterEach, describe, expect, test } from "bun:test";
import { Keypair, TransactionMessage, VersionedTransaction } from "@solana/web3.js";
import bs58 from "bs58";

import { buildRepairRecoveryCommands, runJournaledStepForTest } from "./hxtk-reset.js";
import { parseHxtkCli } from "./hxtk-cli.js";
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
    const parsed = commands.map((command) => parseHxtkCli(command.split(/\s+/)));
    expect(parsed[0]).toMatchObject({ step: "repair", mode: "reconcile" });
    expect(parsed[0]!.has("--expect-seed")).toBe(true);
    expect(parsed[0]!.value("--expect-seed")).toBe("140");
    expect(parsed[0]!.value("--policy-journal")).toBe("/tmp/hxtk-repair-policy.json");
    expect(parsed[0]!.value("--journal")).toBe("/tmp/hxtk-repair.json");
    expect(parsed[1]).toMatchObject({ step: "repair-policy-remove", mode: "reconcile" });
    expect(parsed[1]!.value("--expect-seed")).toBe("140");
    expect(parsed[1]!.value("--policy-journal")).toBe("/tmp/hxtk-repair-policy.json");
    expect(parsed[1]!.value("--repair-journal")).toBe("/tmp/hxtk-repair.json");
    expect(parsed[1]!.value("--journal")).toBe("/tmp/hxtk-repair.policy-remove.json");
    const executeCommands = buildRepairRecoveryCommands({
      repairJournal: "/tmp/hxtk-repair.json",
      policyJournal: "/tmp/hxtk-repair-policy.json",
      policyRemoveJournal: "/tmp/hxtk-repair.policy-remove.json",
      removalStatus: null,
    });
    const executeParsed = parseHxtkCli(executeCommands[1]!.split(/\s+/));
    expect(executeCommands[1]).toStartWith("op run --env-file=.env.1password -- env CONFIRM_MAINNET=1 bun run reset:hxtk ");
    expect(executeParsed).toMatchObject({ step: "repair-policy-remove", mode: "execute" });
    expect(executeParsed.value("--repair-journal")).toBe("/tmp/hxtk-repair.json");
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
    expect(existsSync(join(fx.stateRoot, "flow-test.claim"))).toBe(false);
    await expect(runJournaledStepForTest(fx.input(fx.journal, "reconcile"), failing)).rejects.toThrow("not finalized");
    expect(JSON.parse(readFileSync(join(fx.stateRoot, "flow-test.state"), "utf8")).status).toBe("attempted");
    expect(existsSync(join(fx.stateRoot, "flow-test.claim"))).toBe(false);

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

    let releaseRecoveryBuild!: () => void;
    const recoveryBuildGate = new Promise<void>((resolve) => { releaseRecoveryBuild = resolve; });
    const recovered = deps(fx, { send: async () => {
      sends += 1;
      return { signature: fx.prepared.expectedSignature, err: null, confirmationSlot: 2 };
    }});
    const recoveredInput = fx.input(join(fx.root, "recovered.json"), "execute");
    recoveredInput.build = async () => {
      await recoveryBuildGate;
      return { prepared: fx.prepared, plan: { transaction: { kind: "test" } } };
    };
    const firstRecovery = runJournaledStepForTest(recoveredInput, recovered);
    await new Promise((resolve) => setTimeout(resolve, 0));
    await expect(runJournaledStepForTest(fx.input(join(fx.root, "second-recovery.json"), "execute"), recovered))
      .rejects.toThrow("another hxtk-reset process holds the flow-test claim");
    releaseRecoveryBuild();
    expect(await firstRecovery).toBe(0);
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

  test("canonical generation conflict aborts before raw send", async () => {
    const fx = fixture();
    let sends = 0;
    const input = fx.input(join(fx.root, "generation-conflict.json"), "execute");
    input.build = async () => ({
      prepared: fx.prepared,
      plan: { transaction: { kind: "test" } },
      beforeSend: async () => {
        const statePath = join(fx.stateRoot, "flow-test.state");
        const current = JSON.parse(readFileSync(statePath, "utf8")) as Record<string, unknown>;
        writeFileSync(statePath, `${JSON.stringify({
          ...current,
          generation: Number(current.generation) + 1,
        })}\n`);
      },
    });
    await expect(runJournaledStepForTest(input, {
      ...deps(fx, {
        send: async () => {
          sends += 1;
          return { signature: fx.prepared.expectedSignature, err: null, confirmationSlot: 2 };
        },
      }),
    })).rejects.toThrow("STATE_GENERATION_CONFLICT");
    expect(sends).toBe(0);
    expect(existsSync(join(fx.stateRoot, "flow-test.claim"))).toBe(false);
  });
});
