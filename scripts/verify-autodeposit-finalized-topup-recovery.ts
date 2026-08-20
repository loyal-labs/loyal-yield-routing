#!/usr/bin/env bun

import { readFile } from "node:fs/promises";

import {
  classifyDirectTopUpRecovery,
} from "./execute-autodeposit-policy";
import {
  operationalAlertForAttempt,
  settleDurableAutodepositAttempt,
  type AutodepositAttemptState,
  type DurableAutodepositAttempt,
} from "./durable-autodeposit-confirmation";

type Check = { name: string; pass: boolean; detail?: string };

const checks: Check[] = [];

function check(name: string, pass: boolean, detail?: string): void {
  checks.push({ name, pass, detail });
}

function classify(args: {
  state: AutodepositAttemptState | null;
  vaultAmountRaw: bigint;
  plannedAmountRaw?: bigint;
  persistedSourcePreBalanceRaw?: bigint;
}) {
  return classifyDirectTopUpRecovery({
    existingAttemptState: args.state,
    vaultAmountRaw: args.vaultAmountRaw,
    plannedAmountRaw: args.plannedAmountRaw ?? 100n,
    persistedSourcePreBalanceRaw:
      args.persistedSourcePreBalanceRaw ?? null,
  });
}

function persistedUnknownTopUp(): DurableAutodepositAttempt {
  return {
    id: "production-top-up",
    claimToken: "production-claim",
    operationKind: "top_up",
    executionId: "production-execution",
    amountRaw: 100n,
    sourcePreBalanceRaw: 100n,
    destinationPreBalanceRaw: 0n,
    signature: "persisted-signature",
    signedTransactionBase64: "cGVyc2lzdGVkLWJ5dGVz",
    signedTransactionSha256: "persisted-sha256",
    blockhash: "persisted-blockhash",
    lastValidBlockHeight: 1n,
    state: "unknown",
    broadcastCount: 1,
    confirmedSlot: null,
  };
}

async function main(): Promise<void> {
  for (const state of [
    "prepared",
    "submitted",
    "confirmed",
    "unknown",
    "ambiguous",
  ] as const) {
    check(
      `a ${state} persisted top-up is reconciled before inspecting the drained vault`,
      classify({ state, vaultAmountRaw: 0n }) === "reconcile_persisted",
    );
  }

  check(
    "a missing attempt with insufficient vault funds remains fail-closed",
    classify({ state: null, vaultAmountRaw: 0n }) === "effect_ambiguous",
  );
  check(
    "an expired attempt with effect evidence remains fail-closed",
    classify({
      state: "expired",
      vaultAmountRaw: 0n,
      persistedSourcePreBalanceRaw: 100n,
    }) === "effect_ambiguous",
  );
  check(
    "a conclusively expired attempt may be replaced only while its funds remain",
    classify({
      state: "expired",
      vaultAmountRaw: 100n,
      persistedSourcePreBalanceRaw: 100n,
    }) === "prepare_or_requeue",
  );
  check(
    "a new top-up may be prepared while the planned funds remain",
    classify({ state: null, vaultAmountRaw: 100n }) === "prepare_or_requeue",
  );

  let broadcasts = 0;
  const settled = await settleDurableAutodepositAttempt({
    attempt: persistedUnknownTopUp(),
    dependencies: {
      observe: async () => ({
        state: "confirmed" as const,
        confirmedSlot: 440_441_023n,
        error: null,
      }),
      broadcastExact: async () => {
        broadcasts += 1;
        return "persisted-signature";
      },
      recordBroadcast: async (attempt) => attempt,
      recordObservation: async (attempt, observation) => ({
        ...attempt,
        state: observation.state,
        confirmedSlot: observation.confirmedSlot,
      }),
    },
  });
  check(
    "a finalized stored signature becomes confirmed",
    settled.attempt.state === "confirmed" &&
      settled.attempt.confirmedSlot === 440_441_023n,
  );
  check(
    "finalized recovery never broadcasts another transaction",
    broadcasts === 0 && settled.broadcasted === false,
  );
  check(
    "confirmed recovery emits no ambiguity alert",
    operationalAlertForAttempt(settled.attempt.state) === null,
  );

  const executor = await readFile(
    new URL("./execute-autodeposit-policy.ts", import.meta.url),
    "utf8",
  );
  const resumeStart = executor.indexOf(
    "async function resumeDirectKaminoDeposit",
  );
  const resumeEnd = executor.indexOf(
    "async function recoverAutodepositClaim",
    resumeStart,
  );
  const resume = executor.slice(resumeStart, resumeEnd);
  const classification = resume.indexOf("classifyDirectTopUpRecovery({");
  const ambiguity = resume.indexOf(
    'topUpRecovery === "effect_ambiguous"',
  );
  const settlement = resume.indexOf("sendPreparedTopUpOperation({");
  check(
    "production recovery classifies the persisted attempt before the ambiguity gate",
    resumeStart >= 0 &&
      resumeEnd > resumeStart &&
      classification >= 0 &&
      ambiguity > classification &&
      settlement > ambiguity,
  );
}

await main();

for (const result of checks) {
  console.log(
    `${result.pass ? "ok" : "not ok"} - ${result.name}${
      result.detail ? `: ${result.detail}` : ""
    }`,
  );
}

if (checks.length === 0 || checks.some((result) => !result.pass)) {
  console.log("FAIL_AUTODEPOSIT_FINALIZED_TOPUP_RECOVERY");
  process.exit(1);
}

console.log("PASS_AUTODEPOSIT_FINALIZED_TOPUP_RECOVERY");
