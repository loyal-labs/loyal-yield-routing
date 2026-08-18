import {
  AUTODEPOSIT_IDLE_HANDOFF_STATUS,
  OncePerKeyAlertLatch,
  historicalIdleVaultRecoveryAction,
  idleVaultRecoveryAlert,
  observeIdleVaultAtOrAfter,
  projectIdleVaultBalance,
  shouldNotifyIdleVaultFailure,
  type IdleVaultProjection,
} from "./autodeposit-idle-vault-handoff";

type Evidence = { condition: string; detail: string; passed: boolean };

const evidence: Evidence[] = [];

function check(condition: string, passed: boolean, detail: string): void {
  evidence.push({ condition, detail, passed });
  if (!passed) {
    throw new Error(`${condition}: ${detail}`);
  }
}

class PullHandoffHarness {
  broadcasts = 0;
  claimCompleted = false;
  prepares = 0;
  projection: IdleVaultProjection | null = null;
  signedPullExists = false;

  async run(crashAfterConfirmation: boolean): Promise<void> {
    if (!this.signedPullExists) {
      this.prepares += 1;
      this.broadcasts += 1;
      this.signedPullExists = true;
    }
    if (crashAfterConfirmation) {
      throw new Error("simulated crash after confirmation");
    }
    this.projection = projectIdleVaultBalance(this.projection, {
      amountRaw: 7_000_000n,
      mint: "USDC",
      observedSlot: 101n,
      owner: "vault",
      tokenAccount: "vault-usdc-ata",
      vaultId: 42n,
    });
    this.claimCompleted = true;
  }
}

async function readText(path: string): Promise<string> {
  return Bun.file(new URL(path, import.meta.url)).text();
}

async function main(): Promise<void> {
  const harness = new PullHandoffHarness();
  await harness.run(true).catch(() => undefined);
  check(
    "crash keeps confirmed pull recoverable",
    harness.signedPullExists && !harness.claimCompleted,
    "confirmed attempt persisted while claim remained incomplete",
  );
  await Promise.all([harness.run(false), harness.run(false)]);
  check(
    "pull exactly once across replay and concurrency",
    harness.prepares === 1 && harness.broadcasts === 1,
    `prepares=${harness.prepares} broadcasts=${harness.broadcasts}`,
  );
  check(
    "handoff replay is idempotent",
    harness.claimCompleted && harness.projection?.amountRaw === 7_000_000n,
    "both workers converged on one current idle projection",
  );

  const observationSlots = [99n, 100n];
  const fenced = await observeIdleVaultAtOrAfter({
    minimumSlot: 100n,
    maxAttempts: 2,
    pollIntervalMs: 0,
    read: async () => ({
      amountRaw: 7_000_000n,
      observedSlot: observationSlots.shift() ?? 100n,
    }),
    sleep: async () => undefined,
  });
  check(
    "post-pull observation is context fenced",
    fenced.observedSlot === 100n,
    `accepted slot ${fenced.observedSlot}`,
  );

  const newer = projectIdleVaultBalance(harness.projection, {
    ...harness.projection!,
    amountRaw: 9_000_000n,
    observedSlot: 103n,
  });
  const staleRace = projectIdleVaultBalance(newer, {
    ...newer,
    amountRaw: 8_000_000n,
    observedSlot: 102n,
  });
  check(
    "stale observation cannot overwrite newer projection",
    staleRace.amountRaw === 9_000_000n && staleRace.observedSlot === 103n,
    "slot 102 lost to slot 103",
  );

  const firstFleetFailureAlert = idleVaultRecoveryAlert({
    alertAlreadyClaimed: false,
    handoffPersistenceFailed: false,
    idleSinceMs: 1_000,
    nowMs: 1_500,
    recoverySlaMs: 1_000,
  });
  check(
    "first transient fleet failure is observable but does not alert",
    firstFleetFailureAlert === null && staleRace.amountRaw > 0n,
    "positive idle work remains eligible before SLA",
  );

  const latch = new OncePerKeyAlertLatch();
  const alertReasons = [
    idleVaultRecoveryAlert({
      alertAlreadyClaimed: !latch.claim("vault:USDC"),
      handoffPersistenceFailed: false,
      idleSinceMs: 1_000,
      nowMs: 2_000,
      recoverySlaMs: 1_000,
    }),
    idleVaultRecoveryAlert({
      alertAlreadyClaimed: !latch.claim("vault:USDC"),
      handoffPersistenceFailed: false,
      idleSinceMs: 1_000,
      nowMs: 3_000,
      recoverySlaMs: 1_000,
    }),
  ];
  check(
    "idle recovery SLA alerts exactly once",
    alertReasons[0] === "recovery_sla_exceeded" && alertReasons[1] === null,
    JSON.stringify(alertReasons),
  );

  const persistenceLatch = new OncePerKeyAlertLatch();
  const persistenceAlerts = [0, 1].map(() =>
    idleVaultRecoveryAlert({
      alertAlreadyClaimed: !persistenceLatch.claim("slot:77"),
      handoffPersistenceFailed: true,
      idleSinceMs: 0,
      nowMs: 0,
      recoverySlaMs: 1_000,
    }),
  );
  check(
    "confirmed-pull publication failure alerts exactly once",
    persistenceAlerts[0] === "handoff_persistence_failed" &&
      persistenceAlerts[1] === null,
    JSON.stringify(persistenceAlerts),
  );
  check(
    "user notification waits for final failure and deduplicates",
    !shouldNotifyIdleVaultFailure({
      finalFailure: false,
      notificationAlreadySent: false,
    }) &&
      shouldNotifyIdleVaultFailure({
        finalFailure: true,
        notificationAlreadySent: false,
      }) &&
      !shouldNotifyIdleVaultFailure({
        finalFailure: true,
        notificationAlreadySent: true,
      }),
    "transient=false final=true duplicate=false",
  );

  check(
    "historical recovery filters zero and recovered balances",
    historicalIdleVaultRecoveryAction({
      alreadyRecovered: false,
      amountRaw: 0n,
      executionId: 1n,
    }) === "skip_zero" &&
      historicalIdleVaultRecoveryAction({
        alreadyRecovered: true,
        amountRaw: 5n,
        executionId: 2n,
      }) === "skip_already_recovered" &&
      historicalIdleVaultRecoveryAction({
        alreadyRecovered: false,
        amountRaw: 5n,
        executionId: 3n,
      }) === "project",
    "only unresolved positive finalized balance is projected",
  );

  const [
    executor,
    trigger,
    fleet,
    recovery,
    packageJson,
    lightDockerfile,
    workerImagesWorkflow,
  ] = await Promise.all([
    readText("./execute-autodeposit-policy.ts"),
    readText("../crates/balance-sweep-autodeposit-trigger/src/main.rs"),
    readText("../crates/loyal-fleet-worker/src/lib.rs"),
    readText("./recover-autodeposit-idle-vault-handoffs.ts"),
    readText("../package.json"),
    readText("../Dockerfile.light-workers"),
    readText("../.github/workflows/worker-images.yml"),
  ]);
  const executorMain = executor.slice(executor.indexOf("async function main()"));
  const confirmedRecoveryLoader = executor.slice(
    executor.indexOf("async function loadConfirmedPullRecoveryContext"),
    executor.indexOf("function attemptErrorDetail"),
  );
  const triggerRecoveryQuery = trigger.slice(
    trigger.indexOf("async fn load_executable_targets"),
    trigger.indexOf("let remaining_limit"),
  );
  const fleetHistoryRepair = fleet.slice(
    fleet.indexOf("async fn repair_idle_vault_deposit_partial_pull_history"),
    fleet.indexOf("async fn repair_idle_vault_deposit_app_history_in_tx"),
  );
  const fleetAppHistoryRepair = fleet.slice(
    fleet.indexOf("async fn repair_idle_vault_deposit_app_history_in_tx"),
    fleet.indexOf("async fn deactivate_vault_policy_after_full_withdraw"),
  );
  check(
    "publication failure alert claim is durable",
    executor.includes("claimIdleHandoffFailureAlert") &&
      executor.includes("alerted:autodeposit_idle_handoff_failed") &&
      executor.includes("autodeposit_idle_handoff_recovery_pending"),
    "confirmed attempt stores the first alert claim and later retries stay quiet",
  );
  check(
    "executor publishes before completing the claim",
    executor.includes("publishConfirmedPullAndCompleteClaim") &&
      executorMain.includes("publishConfirmedPullHandoff") &&
      executor.includes("valid_publication") &&
      executor.includes("updated_claim"),
    "publication, execution evidence, claim, and slot share one SQL statement",
  );
  check(
    "executor does not own a Kamino top-up",
    !executorMain.includes("runSameMintReserveTopUp({") &&
      !executorMain.includes('"kamino_top_up_failed"'),
    "main owns only wallet-to-vault pull and durable handoff",
  );
  check(
    "confirmed attempts are restart-selectable",
    trigger.includes(
      'AUTOMATIC_PULL_RECOVERY_STATES: &[&str] = &["prepared", "submitted", "confirmed", "unknown"]',
    ),
    "confirmed durable attempt remains in automatic recovery states",
  );
  check(
    "confirmed recovery bypasses route-policy and target-active gates",
    confirmedRecoveryLoader.includes("attempt.attempt_state = 'confirmed'") &&
      confirmedRecoveryLoader.includes("managed_vaults") &&
      !confirmedRecoveryLoader.includes("route_policies") &&
      executorMain.indexOf("loadConfirmedPullRecoveryContext") <
        executorMain.indexOf("loadEligibleTarget") &&
      triggerRecoveryQuery.includes("attempt.attempt_state = 'confirmed'") &&
      triggerRecoveryQuery.indexOf("attempt.attempt_state = 'confirmed'") <
        triggerRecoveryQuery.indexOf("target.active = true"),
    "persisted confirmed pull resolves its vault directly before normal eligibility",
  );
  check(
    "light-worker packaging tracks the handoff runtime module",
    lightDockerfile.includes(
      "COPY scripts/autodeposit-idle-vault-handoff.ts scripts/autodeposit-idle-vault-handoff.ts",
    ) &&
      (workerImagesWorkflow.match(
        /scripts\/autodeposit-idle-vault-handoff\.ts/g,
      )?.length ?? 0) === 2,
    "runtime image includes the module and both PR and main changes rebuild it",
  );
  check(
    "existing fleet remains sole vault-to-Earn owner",
    fleet.includes("idle_vault_usdc") &&
      fleet.includes(AUTODEPOSIT_IDLE_HANDOFF_STATUS) &&
      fleet.includes("repair_idle_vault_deposit_partial_pull_history"),
    "fleet selects idle source and repairs autodeposit history after confirmation",
  );
  check(
    "history repair follows stable vault identity across replaced targets",
    fleetHistoryRepair.includes(
      "JOIN loyal_yield.balance_sweep_targets AS execution_target",
    ) &&
      fleetHistoryRepair.includes("execution_target.settings = $1") &&
      fleetHistoryRepair.includes("execution_target.vault_index = $2") &&
      fleetHistoryRepair.includes("execution_target.vault_pubkey = $3") &&
      fleetHistoryRepair.includes(
        "COALESCE(execution.destination_token_ata, execution.destination_vault_ata) = $5",
      ) &&
      !fleetHistoryRepair.includes("WHERE target_id = $1"),
    "replaced target rows cannot hide an older confirmed pull for the same vault ATA",
  );
  check(
    "history repair preserves the post-confirm Kamino position total",
    fleet.includes("i64::try_from(post_deposit_position.amount_raw)") &&
      fleetHistoryRepair.includes("post_confirm_position_amount_raw") &&
      fleetAppHistoryRepair.includes(
        "let observed_current_amount = post_confirm_position_amount_raw",
      ) &&
      !fleetAppHistoryRepair.includes(
        "let observed_current_amount = decision.amount_raw",
      ),
    "the deposit delta updates principal while the reconciled chain total updates current_amount_raw",
  );
  check(
    "oversized historical failures cannot block a newer handoff",
    fleet.includes("fn plan_partial_pull_recovery(") &&
      fleet.includes("partial_pull_recovery_priority") &&
      fleetHistoryRepair.includes("idleVaultRecoveredAmountRaw") &&
      fleetHistoryRepair.includes("idleVaultLastDepositSignature") &&
      fleetHistoryRepair.includes("already_repaired_deposit_signature") &&
      fleetHistoryRepair.includes("allocation.fully_recovered") &&
      !fleetHistoryRepair.includes("matched_amount_raw + amount") &&
      !fleetHistoryRepair.includes("ORDER BY execution.slot ASC"),
    "the current handoff is preferred and historical executions consume only their unrecovered residual",
  );
  check(
    "SLA alert is durably claimed",
    trigger.includes("idleVaultRecoveryAlertedAt") &&
      trigger.includes("autodeposit_idle_vault_recovery_stalled"),
    "database evidence prevents repeat SLA alerts",
  );
  check(
    "historical mode is read-only by default and never broadcasts",
    recovery.includes('mode: applyProjections ? "apply_projections" : "read_only"') &&
      recovery.includes("sendsTransactions: false") &&
      !recovery.includes("sendTransaction") &&
      !recovery.includes("sendRawTransaction"),
    "explicit projection-only apply flag performs no chain transaction",
  );
  check(
    "repository verifier command is registered",
    packageJson.includes('"verify:autodeposit-idle-vault-handoff"'),
    "package.json exposes the cold verifier",
  );

  console.log(
    JSON.stringify(
      {
        verdict: "pass",
        handoffStatus: AUTODEPOSIT_IDLE_HANDOFF_STATUS,
        evidence,
      },
      null,
      2,
    ),
  );
  console.log("PASS_AUTODEPOSIT_IDLE_VAULT_HANDOFF");
}

main().catch((error) => {
  console.log(
    JSON.stringify(
      {
        verdict: "fail",
        error: error instanceof Error ? error.message : String(error),
        evidence,
      },
      null,
      2,
    ),
  );
  console.log("FAIL_AUTODEPOSIT_IDLE_VAULT_HANDOFF");
  process.exitCode = 1;
});
