import { existsSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { afterEach, describe, expect, test } from "bun:test";
import { Keypair, TransactionMessage, VersionedTransaction } from "@solana/web3.js";
import bs58 from "bs58";

import {
  JournalTransitionFault,
  buildRepairRecoveryCommands,
  runJournaledStepForTest,
  type JournalTransitionStep,
} from "./hxtk-reset.js";
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

function deps(fx: ReturnType<typeof fixture>, options: Readonly<{
  send?: () => Promise<unknown>;
  finalize?: () => Promise<unknown>;
  faultAfterTransitionStep?: (step: JournalTransitionStep) => void | Promise<void>;
}> = {}) {
  return {
    stateRoot: fx.stateRoot,
    sendPreparedOnce: (options.send ?? (async () => ({ signature: fx.prepared.expectedSignature, err: null, confirmationSlot: 2 }))) as never,
    finalizedTransaction: (options.finalize ?? (async () => fx.finalized)) as never,
    faultAfterTransitionStep: options.faultAfterTransitionStep,
  };
}

describe("HXtk journaled flow", () => {
  test("repair recovery commands carry the complete parser-visible provenance", () => {
    const base = {
      repairJournal: "/tmp/hxtk-repair.json",
      policyJournal: "/tmp/hxtk-repair-policy.json",
      policyRemoveJournal: "/tmp/hxtk-repair.policy-remove.json",
    } as const;
    for (const [status, mode] of [
      ["pending", "reconcile"],
      ["attempted", "reconcile"],
      ["aborted-pre-send", "execute"],
      [null, "execute"],
    ] as const) {
      const commands = buildRepairRecoveryCommands({ ...base, removalStatus: status });
      expect(commands).toHaveLength(2);
      const repair = parseHxtkCli(commands[0]!.split(/\s+/));
      expect(repair).toMatchObject({ step: "repair", mode: "reconcile" });
      expect(repair.value("--policy-journal")).toBe(base.policyJournal);
      expect(repair.value("--journal")).toBe(base.repairJournal);
      const removal = parseHxtkCli(commands[1]!.split(/\s+/));
      expect(removal).toMatchObject({ step: "repair-policy-remove", mode });
      expect(removal.value("--expect-seed")).toBe("140");
      expect(removal.value("--policy-journal")).toBe(base.policyJournal);
      expect(removal.value("--repair-journal")).toBe(base.repairJournal);
      expect(removal.value("--journal")).toBe(base.policyRemoveJournal);
      if (mode === "execute") {
        expect(commands[1]).toStartWith("op run --env-file=.env.1password -- env CONFIRM_MAINNET=1 bun run reset:hxtk ");
      }
    }
    const finalized = buildRepairRecoveryCommands({ ...base, removalStatus: "finalized" });
    expect(finalized[1]).toBe("repair-policy-remove already finalized; verify with bun run reset:hxtk verify");
    expect(parseHxtkCli(finalized[1]!.split("verify with ")[1]!.split(/\s+/))).toMatchObject({
      step: "verify",
      mode: null,
    });
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

  test("fresh execution resumes every finalized transition boundary without a second send", async () => {
    for (const transitionStep of ["final-journal", "sent-wire", "finalized-state"] as const) {
      const fx = fixture();
      let sends = 0;
      await expect(runJournaledStepForTest(fx.input(fx.journal, "execute"), deps(fx, {
        send: async () => {
          sends += 1;
          return { signature: fx.prepared.expectedSignature, err: null, confirmationSlot: 2 };
        },
        faultAfterTransitionStep: (step) => {
          if (step === transitionStep) throw new JournalTransitionFault(step);
        },
      }))).rejects.toThrow("HXTK_TEST_INTERRUPTED: " + transitionStep);
      expect(sends).toBe(1);
      expect(await runJournaledStepForTest(fx.input(fx.journal, "execute"), deps(fx, {
        send: async () => {
          sends += 1;
          throw new Error("must not send twice");
        },
      }))).toBe(0);
      expect(sends).toBe(1);
      expect(JSON.parse(readFileSync(join(fx.stateRoot, "flow-test.state"), "utf8")).status).toBe("finalized");
      expect(existsSync(`${fx.journal}.pending`)).toBe(false);
      expect(existsSync(`${fx.journal}.sent-wire`)).toBe(true);
    }
  });

  test("fresh execution resumes every aborted-pre-send transition boundary and re-arms", async () => {
    for (const transitionStep of ["aborted-pending", "aborted-journal", "aborted-state"] as const) {
      const fx = fixture();
      let sends = 0;
      const aborting = {
        ...fx.input(fx.journal, "execute"),
        build: async () => ({
          prepared: fx.prepared,
          plan: { transaction: { kind: "test" } },
          beforeSend: async () => { throw new Error("prestate changed"); },
        }),
      };
      await expect(runJournaledStepForTest(aborting, deps(fx, {
        faultAfterTransitionStep: (step) => {
          if (step === transitionStep) throw new JournalTransitionFault(step);
        },
      }))).rejects.toThrow("HXTK_TEST_INTERRUPTED: " + transitionStep);
      expect(sends).toBe(0);
      expect(await runJournaledStepForTest(fx.input(fx.journal, "execute"), deps(fx, {
        send: async () => {
          sends += 1;
          return { signature: fx.prepared.expectedSignature, err: null, confirmationSlot: 2 };
        },
      }))).toBe(0);
      expect(sends).toBe(1);
      expect(JSON.parse(readFileSync(join(fx.stateRoot, "flow-test.state"), "utf8")).status).toBe("finalized");
      expect(existsSync(`${fx.journal}.pending`)).toBe(false);
    }
  });

  test("two Bun processes racing one leg elect one generation and send once", async () => {
    const root = mkdtempSync(join("/tmp", "hxtk-race-"));
    roots.push(root);
    const stateRoot = resolveCanonicalStateRoot({ vault: "HXtk", homeDir: root, create: true });
    const startFile = join(root, "start");
    const sendCounter = join(root, "sends");
    writeFileSync(sendCounter, "", { mode: 0o600 });
    const childPath = join(import.meta.dir, "hxtk-reset-race-child.ts");
    const children = [0, 1].map((childId) => Bun.spawn([
      process.execPath,
      "run",
      childPath,
    ], {
      cwd: join(import.meta.dir, "../.."),
      env: {
        PATH: process.env.PATH ?? "",
        HXTK_FAKE_RPC: "1",
        HXTK_RACE_STATE_ROOT: stateRoot,
        HXTK_RACE_START_FILE: startFile,
        HXTK_RACE_SEND_COUNTER: sendCounter,
        HXTK_RACE_DIRECTORY: root,
        HXTK_RACE_CHILD_ID: String(childId),
        HXTK_RACE_JOURNAL: join(root, "race-" + childId + ".json"),
      },
      stdout: "pipe",
      stderr: "pipe",
    }));
    await new Promise((resolve) => setTimeout(resolve, 50));
    writeFileSync(startFile, "go\n", { mode: 0o600 });
    const results = await Promise.all(children.map(async (child) => {
      const stdout = await new Response(child.stdout).text();
      const stderr = await new Response(child.stderr).text();
      const exitCode = await child.exited;
      const line = stdout.split(/\r?\n/).filter((entry) => entry.startsWith("RACE_RESULT ")).pop();
      return {
        exitCode,
        stdout,
        stderr,
        result: line ? JSON.parse(line.slice("RACE_RESULT ".length)) as {
          ok: boolean;
          error?: string;
        } : null,
      };
    }));
    const sentPids = readFileSync(sendCounter, "utf8").trim().split(/\r?\n/).filter(Boolean);
    expect(sentPids).toHaveLength(1);
    expect(results.filter((result) => result.result?.ok)).toHaveLength(1);
    expect(results.filter((result) => result.result?.error?.includes("STATE_GENERATION_CONFLICT"))).toHaveLength(1);
    expect(results.every((result) => result.exitCode === 0)).toBe(true);
  });
});
