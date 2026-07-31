#!/usr/bin/env bun
//
// End-to-end verifier for the closed-route-policy autodeposit failure.
//
// Reproduces the production incident on a throwaway local Postgres cluster: a
// vault whose Squads route policy was closed on chain while Neon still lists it
// active. Before the fix, the trigger's DB-only guard let the slot through, the
// executor died on `AccountNotFound: pubkey=<policy_account>`, and the residual
// mover immediately rescheduled the lot with an `eligible_after` in the past,
// looping forever.
//
// Requires local Postgres binaries (`initdb`, `postgres`) and cargo. Nothing
// here touches production: the cluster lives in a temp directory and the Solana
// RPC is a local stub.
//
//   bun scripts/verify-autodeposit-closed-route-policy.ts

import { mkdtempSync, rmSync, writeFileSync, readFileSync, existsSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { SQL } from "bun";

import {
  ClosedOnChainRoutePolicyError,
  cancelAutodepositLotClaimForClosedRoutePolicy,
  closedRoutePolicyAccountFromStderr,
  releaseAutodepositLotClaim,
} from "./execute-autodeposit-policy";

const USDC_MINT = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v";
// Mirrors MAX_AUTODEPOSIT_LOT_ATTEMPTS in the trigger.
const MAX_ATTEMPTS = 6;

const BROKEN = {
  targetId: 9001n,
  settings: "BrokenSettings111111111111111111111111111",
  vaultPubkey: "BrokenVault11111111111111111111111111111",
  policyAccount: "BrokenRoutePoxicy111111111111111111111111",
  wallet: "BrokenWaxxet1111111111111111111111111111",
};
const HEALTHY = {
  targetId: 9002n,
  settings: "HeaxthySettings11111111111111111111111111",
  vaultPubkey: "HeaxthyVauxt1111111111111111111111111111",
  policyAccount: "HeaxthyRoutePoxicy1111111111111111111111",
  wallet: "HeaxthyWaxxet111111111111111111111111111",
};

let failures = 0;
let checks = 0;

function check(label: string, condition: boolean, detail?: unknown) {
  checks += 1;
  if (condition) {
    console.log(`  ok   ${label}`);
    return;
  }
  failures += 1;
  console.log(`  FAIL ${label}`);
  if (detail !== undefined) {
    console.log(`       ${JSON.stringify(detail)}`);
  }
}

type RunResult = { exitCode: number; stdout: string; stderr: string };

// Async on purpose: the stub Solana RPC below is served from this process's
// event loop, so a blocking spawnSync would deadlock the trigger's chain check.
async function run(
  command: string[],
  options: { cwd?: string; env?: Record<string, string>; allowFailure?: boolean } = {}
): Promise<RunResult> {
  const child = Bun.spawn(command, {
    cwd: options.cwd ?? process.cwd(),
    env: { ...process.env, ...(options.env ?? {}) },
    stdout: "pipe",
    stderr: "pipe",
  });
  const [stdout, stderr, exitCode] = await Promise.all([
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
    child.exited,
  ]);
  if (exitCode !== 0 && !options.allowFailure) {
    throw new Error(
      `command failed (${exitCode}): ${command.join(" ")}\n${stdout}\n${stderr}`
    );
  }
  return { exitCode, stdout, stderr };
}

async function freePort(): Promise<number> {
  const server = Bun.listen({
    hostname: "127.0.0.1",
    port: 0,
    socket: { data() {} },
  });
  const port = server.port;
  server.stop(true);
  return port;
}

// ---------------------------------------------------------------- postgres --

type Cluster = { url: string; dataDir: string; stop: () => Promise<void> };

async function startPostgres(root: string): Promise<Cluster> {
  const dataDir = join(root, "pgdata");
  const socketDir = join(root, "sock");
  await run(["mkdir", "-p", socketDir]);
  const pgBin = ["/opt/homebrew/opt/postgresql@17/bin", "/usr/local/opt/postgresql@17/bin"].find(
    (candidate) => existsSync(join(candidate, "initdb"))
  );
  const initdb = pgBin ? join(pgBin, "initdb") : "initdb";
  const pgCtl = pgBin ? join(pgBin, "pg_ctl") : "pg_ctl";
  const createdb = pgBin ? join(pgBin, "createdb") : "createdb";

  await run([initdb, "-D", dataDir, "-U", "postgres", "--auth=trust", "-E", "UTF8"]);
  const port = await freePort();
  await run([
    pgCtl,
    "-D",
    dataDir,
    "-o",
    `-p ${port} -k ${socketDir} -c listen_addresses=127.0.0.1 -c lc_messages=C -c fsync=off -c full_page_writes=off`,
    "-w",
    "-l",
    join(root, "postgres.log"),
    "start",
  ]);
  await run([createdb, "-h", "127.0.0.1", "-p", String(port), "-U", "postgres", "loyal_yield_verifier"]);

  return {
    url: `postgres://postgres@127.0.0.1:${port}/loyal_yield_verifier`,
    dataDir,
    stop: async () => {
      await run([pgCtl, "-D", dataDir, "-m", "immediate", "-w", "stop"], { allowFailure: true });
    },
  };
}

// --------------------------------------------------------------- stub rpc ---

type StubRpc = {
  url: string;
  requests: number;
  setMissing: (accounts: string[]) => void;
  setOutage: (outage: boolean) => void;
  stop: () => void;
};

function startStubRpc(missing: string[]): StubRpc {
  let missingAccounts = new Set(missing);
  let outage = false;
  const state = { requests: 0 };

  const server = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    async fetch(request) {
      state.requests += 1;
      if (outage) {
        return new Response("stub rpc outage", { status: 503 });
      }
      const body = (await request.json()) as {
        method?: string;
        params?: [string[], unknown];
      };
      if (body.method !== "getMultipleAccounts") {
        return Response.json({ jsonrpc: "2.0", id: 1, error: { message: "unsupported" } });
      }
      const requested = body.params?.[0] ?? [];
      return Response.json({
        jsonrpc: "2.0",
        id: 1,
        result: {
          context: { slot: 500_000_000 },
          value: requested.map((account) =>
            missingAccounts.has(account)
              ? null
              : {
                  // Squads policy program; the guard only cares about existence.
                  owner: "SMRTzfY6DfH5ik3TKiyLFfXexV8uSG3d2UksSCYdunG",
                  lamports: 2_060_160,
                  data: ["", "base64"],
                  executable: false,
                  rentEpoch: 0,
                  space: 0,
                }
          ),
        },
      });
    },
  });

  return {
    url: `http://127.0.0.1:${server.port}`,
    get requests() {
      return state.requests;
    },
    setMissing: (accounts: string[]) => {
      missingAccounts = new Set(accounts);
    },
    setOutage: (value: boolean) => {
      outage = value;
    },
    stop: () => server.stop(true),
  };
}

// ------------------------------------------------------------------- seed ---

async function seedVault(
  sql: SQL,
  fixture: typeof BROKEN,
  options: { policySeed: number }
): Promise<void> {
  const [policy] = await sql`
    INSERT INTO loyal_yield.route_policies
      (settings, authority, policy_seed, policy_account, vault_index, vault_pubkey,
       delegated_signers, threshold, route_modes, stable_mints, kamino_markets,
       kamino_liquidity_mints, active, last_seen_slot, last_seen_signature)
    VALUES
      (${fixture.settings}, ${fixture.wallet}, ${options.policySeed}, ${fixture.policyAccount},
       1, ${fixture.vaultPubkey}, ARRAY['DexegateSigner111111111111111111111111111'], 1,
       ARRAY['same_mint_kamino'], ARRAY[${USDC_MINT}], ARRAY['KaminoMarket1111111111111111111111111111'],
       ARRAY[${USDC_MINT}], true, 1, 'seed-policy-signature')
    RETURNING id
  `;

  await sql`
    INSERT INTO loyal_yield.managed_vaults
      (settings, vault_index, vault_pubkey, active_policy_id, active)
    VALUES (${fixture.settings}, 1, ${fixture.vaultPubkey}, ${policy.id}, true)
  `;

  await sql`
    INSERT INTO loyal_yield.balance_sweep_targets
      (id, settings, authority, policy_seed, policy_account, vault_index, vault_pubkey,
       wallet, wallet_usdc_ata, vault_usdc_ata, token_mint, wallet_token_ata, vault_token_ata,
       delegated_signers, threshold, max_amount_per_period, active, lifecycle_status,
       first_seen_at, last_seen_at, last_seen_slot, last_seen_signature,
       recurring_delegation, wallet_balance_floor_raw)
    VALUES
      (${fixture.targetId}, ${fixture.settings}, ${fixture.wallet}, ${options.policySeed},
       ${`sweep-${fixture.policyAccount}`}, 1, ${fixture.vaultPubkey}, ${fixture.wallet},
       ${`${fixture.wallet}-wata`}, ${`${fixture.vaultPubkey}-vata`}, ${USDC_MINT},
       ${`${fixture.wallet}-wata`}, ${`${fixture.vaultPubkey}-vata`},
       ARRAY['DexegateSigner111111111111111111111111111'], 1, 1000000000, true, 'active',
       now(), now(), 1, 'seed-target-signature',
       ${`${fixture.wallet}-recurring`}, 1000)
  `;

  await sql`
    INSERT INTO loyal_yield.balance_sweep_wallet_balances_current
      (target_id, wallet, wallet_token_ata, mint, amount_raw, observed_slot, source, source_commitment, updated_at)
    VALUES (${fixture.targetId}, ${fixture.wallet}, ${`${fixture.wallet}-wata`}, ${USDC_MINT},
            50000000, 1, 'verifier', 'confirmed', now())
  `;
}

async function seedLotAndSlot(
  sql: SQL,
  fixture: typeof BROKEN,
  options: { eventId: number; amountRaw: number; attemptCount?: number }
): Promise<{ slotId: bigint; lotId: bigint }> {
  await sql`
    INSERT INTO loyal_yield.balance_sweep_wallet_balance_events
      (event_id, target_id, wallet, wallet_token_ata, mint, amount_raw, observed_slot, observed_at, source, source_commitment)
    VALUES (${options.eventId}, ${fixture.targetId}, ${fixture.wallet}, ${`${fixture.wallet}-wata`},
            ${USDC_MINT}, ${options.amountRaw}, ${options.eventId}, now() - interval '3 hours', 'verifier', 'confirmed')
  `;
  const [slot] = await sql`
    INSERT INTO loyal_yield.balance_sweep_scheduled_slots (target_id, token_mint, eligible_after, status)
    VALUES (${fixture.targetId}, ${USDC_MINT}, now() - interval '1 hour', 'scheduled')
    RETURNING id
  `;
  const [lot] = await sql`
    INSERT INTO loyal_yield.balance_sweep_surplus_lots
      (target_id, source_event_id, original_amount_raw, remaining_amount_raw, classification,
       eligible_after, status, reason, scheduled_slot_id, autodeposit_attempt_count)
    VALUES (${fixture.targetId}, ${options.eventId}, ${options.amountRaw}, ${options.amountRaw},
            'simple_inbound', now() - interval '1 hour', 'open', 'verifier seed', ${slot.id},
            ${options.attemptCount ?? 0})
    RETURNING id
  `;
  return { slotId: slot.id, lotId: lot.id };
}

async function seedExtraLot(
  sql: SQL,
  fixture: typeof BROKEN,
  options: { eventId: number; amountRaw: number; slotId: bigint; attemptCount: number }
): Promise<{ lotId: bigint }> {
  await sql`
    INSERT INTO loyal_yield.balance_sweep_wallet_balance_events
      (event_id, target_id, wallet, wallet_token_ata, mint, amount_raw, observed_slot, observed_at, source, source_commitment)
    VALUES (${options.eventId}, ${fixture.targetId}, ${fixture.wallet}, ${`${fixture.wallet}-wata`},
            ${USDC_MINT}, ${options.amountRaw}, ${options.eventId}, now() - interval '3 hours', 'verifier', 'confirmed')
  `;
  const [lot] = await sql`
    INSERT INTO loyal_yield.balance_sweep_surplus_lots
      (target_id, source_event_id, original_amount_raw, remaining_amount_raw, classification,
       eligible_after, status, reason, scheduled_slot_id, autodeposit_attempt_count)
    VALUES (${fixture.targetId}, ${options.eventId}, ${options.amountRaw}, ${options.amountRaw},
            'simple_inbound', now() - interval '30 minutes', 'open', 'verifier seed',
            ${options.slotId}, ${options.attemptCount})
    RETURNING id
  `;
  return { lotId: lot.id };
}

// ------------------------------------------------------------------- main ---

const root = mkdtempSync(join(tmpdir(), "autodeposit-verifier-"));
console.log(`workspace: ${root}`);

const executorLog = join(root, "executor-invocations.log");
const executorScript = join(root, "stub-executor.sh");
// Emulates `same-mint-reserve-swap` dying on a closed route policy: the exact
// stderr shape the production executor reported.
writeFileSync(
  executorScript,
  `#!/bin/sh
echo "$@" >> ${executorLog}
for arg in "$@"; do
  case "$arg" in
    ${BROKEN.targetId})
      echo 'Error: Error { request: None, kind: RpcError(ForUser("AccountNotFound: pubkey=${BROKEN.policyAccount}")) }' >&2
      exit 1
      ;;
  esac
done
exit 0
`,
  { mode: 0o755 }
);

function executorInvocations(): string[] {
  if (!existsSync(executorLog)) {
    return [];
  }
  return readFileSync(executorLog, "utf8").trim().split("\n").filter(Boolean);
}

let cluster: Cluster | null = null;
const rpc = startStubRpc([BROKEN.policyAccount]);

try {
  console.log("\n[1/7] starting throwaway postgres");
  cluster = await startPostgres(root);
  console.log(`  ${cluster.url}`);

  console.log("\n[2/7] applying loyal_yield migrations");
  await run(["cargo", "run", "--quiet", "-p", "loyal-yield-orchestrator", "--bin", "yield-migrations", "--", "--apply"], {
    env: { NEON_DATABASE_URL: cluster.url },
  });

  console.log("\n[3/7] building the autodeposit trigger");
  await run(["cargo", "build", "--quiet", "-p", "balance-sweep-autodeposit-trigger"]);
  const triggerBin = join(process.cwd(), "target", "debug", "balance-sweep-autodeposit-trigger");

  const sql = new SQL(cluster.url);

  const [{ exists: hasAttemptColumn }] = await sql`
    SELECT EXISTS (
      SELECT 1 FROM information_schema.columns
      WHERE table_schema = 'loyal_yield'
        AND table_name = 'balance_sweep_surplus_lots'
        AND column_name = 'autodeposit_attempt_count'
    ) AS exists
  `;
  check("migration 0033 added autodeposit_attempt_count", hasAttemptColumn === true);

  await seedVault(sql, BROKEN, { policySeed: 1 });
  await seedVault(sql, HEALTHY, { policySeed: 1 });
  const broken = await seedLotAndSlot(sql, BROKEN, { eventId: 1, amountRaw: 5_000_000 });
  const healthy = await seedLotAndSlot(sql, HEALTHY, { eventId: 2, amountRaw: 5_000_000 });

  const runTrigger = (env: Record<string, string> = {}) =>
    run(
      [
        triggerBin,
        "--once",
        "--execute-eligible",
        "--disable-realtime-listen",
        "--postgres-url",
        cluster!.url,
        "--executor-command",
        executorScript,
        "--solana-rpc-url",
        rpc.url,
      ],
      { env: { RUST_LOG: "info", ...env }, allowFailure: true }
    );

  // tracing colourises its key=value pairs, so strip ANSI before matching.
  const triggerLog = (result: RunResult) =>
    `${result.stdout}\n${result.stderr}`.replace(/\u001b\[[0-9;]*m/g, "");

  const debugTrigger = (label: string, result: RunResult) => {
    if (!process.env.VERIFIER_DEBUG) {
      return;
    }
    console.log(`  --- ${label} exit=${result.exitCode}`);
    console.log(result.stdout.trim());
    console.log(result.stderr.trim());
  };

  console.log("\n[4/7] scenario: route policy closed on chain");
  if (process.env.VERIFIER_DEBUG) {
    console.log("  targets:", await sql`SELECT id, active, lifecycle_status, token_mint, wallet_balance_floor_raw, settings, vault_index, vault_pubkey, authority FROM loyal_yield.balance_sweep_targets`);
    console.log("  slots:", await sql`SELECT id, target_id, token_mint, status::text, eligible_after FROM loyal_yield.balance_sweep_scheduled_slots`);
    console.log("  lots:", await sql`SELECT id, target_id, source_event_id, status::text, remaining_amount_raw, scheduled_slot_id FROM loyal_yield.balance_sweep_surplus_lots`);
    console.log("  vaults:", await sql`SELECT id, settings, vault_index, vault_pubkey, active_policy_id, active FROM loyal_yield.managed_vaults`);
    console.log("  policies:", await sql`SELECT id, settings, authority, vault_index, vault_pubkey, active, route_modes FROM loyal_yield.route_policies`);
    console.log("  balances:", await sql`SELECT target_id, mint, amount_raw FROM loyal_yield.balance_sweep_wallet_balances_current`);
  }
  const first = await runTrigger();
  debugTrigger("trigger pass 1", first);
  const [brokenSlot] = await sql`
    SELECT status::text AS status, last_error FROM loyal_yield.balance_sweep_scheduled_slots WHERE id = ${broken.slotId}
  `;
  const [brokenLot] = await sql`
    SELECT status::text AS status FROM loyal_yield.balance_sweep_surplus_lots WHERE id = ${broken.lotId}
  `;
  const [brokenTarget] = await sql`
    SELECT active, lifecycle_status FROM loyal_yield.balance_sweep_targets WHERE id = ${BROKEN.targetId}
  `;
  check("broken slot is canceled, not failed-and-retried", brokenSlot.status === "canceled", brokenSlot);
  check(
    "cancel reason names the missing policy account",
    String(brokenSlot.last_error ?? "").includes(BROKEN.policyAccount),
    brokenSlot.last_error
  );
  check("broken lot is dead-lettered", brokenLot.status === "suppressed", brokenLot);
  check(
    "broken target is paused for a missing policy",
    brokenTarget.active === false && brokenTarget.lifecycle_status === "pending_policy",
    brokenTarget
  );
  check(
    "executor was never spawned for the broken target",
    !executorInvocations().some((line) => line.includes(String(BROKEN.targetId))),
    executorInvocations()
  );
  check(
    "trigger reported the cancellation",
    triggerLog(first).includes("closed_onchain_route_policy_slots_canceled=1"),
    triggerLog(first)
      .split("\n")
      .filter((line) => line.includes("scanned eligible"))
  );

  console.log("\n[5/7] scenario: healthy vault still executes");
  check(
    "executor ran for the healthy target",
    executorInvocations().some((line) => line.includes(String(HEALTHY.targetId))),
    executorInvocations()
  );
  const [healthySlot] = await sql`
    SELECT status::text AS status FROM loyal_yield.balance_sweep_scheduled_slots WHERE id = ${healthy.slotId}
  `;
  check("healthy slot was left for the executor", healthySlot.status === "scheduled", healthySlot);

  const invocationsAfterFirst = executorInvocations().length;
  await runTrigger();
  check(
    "second pass does not re-spawn anything for the broken target",
    !executorInvocations()
      .slice(invocationsAfterFirst)
      .some((line) => line.includes(String(BROKEN.targetId))),
    executorInvocations().slice(invocationsAfterFirst)
  );
  const [{ count: brokenOpenSlots }] = await sql`
    SELECT COUNT(*)::int AS count FROM loyal_yield.balance_sweep_scheduled_slots
    WHERE target_id = ${BROKEN.targetId} AND status IN ('scheduled', 'requested')
  `;
  check("no replacement slot was minted for the broken target", brokenOpenSlots === 0, brokenOpenSlots);

  console.log("\n[6/7] scenario: RPC outage must not cancel healthy work");
  const outageFixture = { ...HEALTHY, targetId: 9003n, settings: "OutageSettings11111111111111111111111111", vaultPubkey: "OutageVauxt11111111111111111111111111111", policyAccount: "OutageRoutePoxicy111111111111111111111111", wallet: "OutageWaxxet1111111111111111111111111111" };
  await seedVault(sql, outageFixture, { policySeed: 1 });
  const outage = await seedLotAndSlot(sql, outageFixture, { eventId: 3, amountRaw: 5_000_000 });
  rpc.setOutage(true);
  const outageRun = await runTrigger();
  rpc.setOutage(false);
  const [outageSlot] = await sql`
    SELECT status::text AS status FROM loyal_yield.balance_sweep_scheduled_slots WHERE id = ${outage.slotId}
  `;
  check("slot survives an RPC outage", outageSlot.status === "scheduled", outageSlot);
  check(
    "outage is reported rather than swallowed",
    triggerLog(outageRun).includes("route policy chain check failed"),
    triggerLog(outageRun).split("\n").slice(-6)
  );

  console.log("\n[7/7] scenario: executor-side classification, cancel, and attempt cap");
  check(
    "AccountNotFound for the bound policy is classified",
    closedRoutePolicyAccountFromStderr(
      `Error: Error { request: None, kind: RpcError(ForUser("AccountNotFound: pubkey=${BROKEN.policyAccount}")) }`,
      BROKEN.policyAccount
    ) === BROKEN.policyAccount
  );
  check(
    "AccountNotFound for some other account stays retryable",
    closedRoutePolicyAccountFromStderr(
      'AccountNotFound: pubkey=SomeOtherAccount1111111111111111111111111',
      BROKEN.policyAccount
    ) === null
  );
  check(
    "the terminal error carries the policy account",
    new ClosedOnChainRoutePolicyError(1n, BROKEN.policyAccount).policyAccount === BROKEN.policyAccount
  );

  // Drive the executor's own claim-resolution SQL against the local database.
  const neonShim = ((url: string) => new SQL(url)) as never;
  const claimFixture = { ...BROKEN, targetId: 9004n, settings: "CxaimSettings111111111111111111111111111", vaultPubkey: "CxaimVauxt11111111111111111111111111111", policyAccount: "CxaimRoutePoxicy11111111111111111111111", wallet: "CxaimWaxxet111111111111111111111111111" };
  await seedVault(sql, claimFixture, { policySeed: 1 });
  const claimed = await seedLotAndSlot(sql, claimFixture, { eventId: 4, amountRaw: 4_000_000 });
  const claimToken = "verifier-claim-closed-policy";
  await sql`
    INSERT INTO loyal_yield.balance_sweep_lot_claims (claim_token, target_id, amount_raw, status)
    VALUES (${claimToken}, ${claimFixture.targetId}, 4000000, 'selected')
  `;
  await sql`
    INSERT INTO loyal_yield.balance_sweep_lot_claim_items (claim_token, lot_id, amount_raw)
    VALUES (${claimToken}, ${claimed.lotId}, 4000000)
  `;
  await sql`
    UPDATE loyal_yield.balance_sweep_surplus_lots
    SET remaining_amount_raw = 0, status = 'consumed' WHERE id = ${claimed.lotId}
  `;
  await sql`
    UPDATE loyal_yield.balance_sweep_scheduled_slots
    SET status = 'selected', claim_token = ${claimToken} WHERE id = ${claimed.slotId}
  `;

  await cancelAutodepositLotClaimForClosedRoutePolicy({
    neon: neonShim,
    databaseUrl: cluster.url,
    claimToken,
    lastError: `Autodeposit route policy ${claimFixture.policyAccount} is not present on chain.`,
  });
  const [canceledSlot] = await sql`
    SELECT status::text AS status FROM loyal_yield.balance_sweep_scheduled_slots WHERE id = ${claimed.slotId}
  `;
  const [canceledLot] = await sql`
    SELECT status::text AS status, remaining_amount_raw, original_amount_raw
    FROM loyal_yield.balance_sweep_surplus_lots WHERE id = ${claimed.lotId}
  `;
  const [canceledTarget] = await sql`
    SELECT active, lifecycle_status FROM loyal_yield.balance_sweep_targets WHERE id = ${claimFixture.targetId}
  `;
  check("executor cancel marks the slot canceled", canceledSlot.status === "canceled", canceledSlot);
  check("executor cancel suppresses the lot", canceledLot.status === "suppressed", canceledLot);
  check(
    "executor cancel hands the amount back without exceeding the original",
    BigInt(canceledLot.remaining_amount_raw) === BigInt(canceledLot.original_amount_raw),
    canceledLot
  );
  check(
    "executor cancel pauses the target",
    canceledTarget.active === false && canceledTarget.lifecycle_status === "pending_policy",
    canceledTarget
  );

  // A retryable failure must back off instead of rescheduling immediately.
  const retryFixture = { ...BROKEN, targetId: 9005n, settings: "RetrySettings111111111111111111111111111", vaultPubkey: "RetryVauxt11111111111111111111111111111", policyAccount: "RetryRoutePoxicy11111111111111111111111", wallet: "RetryWaxxet111111111111111111111111111" };
  await seedVault(sql, retryFixture, { policySeed: 1 });
  const retried = await seedLotAndSlot(sql, retryFixture, { eventId: 5, amountRaw: 3_000_000 });
  const retryToken = "verifier-claim-retryable";
  await sql`
    INSERT INTO loyal_yield.balance_sweep_lot_claims (claim_token, target_id, amount_raw, status)
    VALUES (${retryToken}, ${retryFixture.targetId}, 3000000, 'selected')
  `;
  await sql`
    INSERT INTO loyal_yield.balance_sweep_lot_claim_items (claim_token, lot_id, amount_raw)
    VALUES (${retryToken}, ${retried.lotId}, 3000000)
  `;
  await sql`
    UPDATE loyal_yield.balance_sweep_surplus_lots
    SET remaining_amount_raw = 0, status = 'consumed' WHERE id = ${retried.lotId}
  `;
  await sql`
    UPDATE loyal_yield.balance_sweep_scheduled_slots
    SET status = 'selected', claim_token = ${retryToken} WHERE id = ${retried.slotId}
  `;
  await releaseAutodepositLotClaim({
    neon: neonShim,
    databaseUrl: cluster.url,
    claimToken: retryToken,
    lastError: "transient RPC failure",
    pauseTargetForMissingDelegate: false,
  });
  const [releasedLot] = await sql`
    SELECT status::text AS status, autodeposit_attempt_count,
           eligible_after > now() + interval '4 minutes' AS backed_off
    FROM loyal_yield.balance_sweep_surplus_lots WHERE id = ${retried.lotId}
  `;
  check("retryable failure reopens the lot", releasedLot.status === "open", releasedLot);
  check("retryable failure records an attempt", releasedLot.autodeposit_attempt_count === 1, releasedLot);
  check("retryable failure backs the lot off into the future", releasedLot.backed_off === true, releasedLot);

  // Residual mover: over-budget lots are dead-lettered, in-budget lots keep a
  // future eligibility instead of an already-elapsed one.
  const capFixture = { ...BROKEN, targetId: 9006n, settings: "CapSettings1111111111111111111111111111", vaultPubkey: "CapVauxt1111111111111111111111111111111", policyAccount: "CapRoutePoxicy111111111111111111111111", wallet: "CapWaxxet1111111111111111111111111111" };
  await seedVault(sql, capFixture, { policySeed: 1 });
  // Three lots on one slot: the claim consumes the cheapest, leaving one lot
  // inside the attempt budget and one past it as residuals for the mover.
  const claimable = await seedLotAndSlot(sql, capFixture, {
    eventId: 6,
    amountRaw: 500_000,
  });
  const exhausted = await seedExtraLot(sql, capFixture, {
    eventId: 7,
    amountRaw: 2_000_000,
    slotId: claimable.slotId,
    attemptCount: MAX_ATTEMPTS,
  });
  const withinBudget = await seedExtraLot(sql, capFixture, {
    eventId: 8,
    amountRaw: 1_000_000,
    slotId: claimable.slotId,
    attemptCount: 1,
  });

  // The claim path refuses to act on a target with wallet events the projector
  // has not consumed yet; advance the offset instead of re-running projection,
  // which would mint unrelated lots and slots.
  await sql`
    INSERT INTO loyal_yield.projection_offsets (consumer_name, last_event_id, updated_at)
    VALUES ('balance_sweep_autodeposit_trigger',
            (SELECT MAX(event_id) FROM loyal_yield.balance_sweep_wallet_balance_events), now())
    ON CONFLICT (consumer_name) DO UPDATE
      SET last_event_id = EXCLUDED.last_event_id, updated_at = now()
  `;

  const capClaim = await run(
    [
      triggerBin,
      "--postgres-url",
      cluster.url,
      "--disable-realtime-listen",
      "--claim-target-id",
      String(capFixture.targetId),
      "--scheduled-slot-id",
      String(claimable.slotId),
      "--claim-token",
      "verifier-claim-cap",
      "--claim-wallet-balance-raw",
      "50000000",
      "--claim-wallet-balance-floor-raw",
      "1000",
      "--claim-max-amount-raw",
      "500000",
    ],
    { env: { RUST_LOG: "info" }, allowFailure: true }
  );
  debugTrigger("cap claim", capClaim);
  const [exhaustedLot] = await sql`
    SELECT status::text AS status FROM loyal_yield.balance_sweep_surplus_lots WHERE id = ${exhausted.lotId}
  `;
  const [movedLot] = await sql`
    SELECT status::text AS status, scheduled_slot_id FROM loyal_yield.balance_sweep_surplus_lots WHERE id = ${withinBudget.lotId}
  `;
  const [{ count: pastDueSlots }] = await sql`
    SELECT COUNT(*)::int AS count
    FROM loyal_yield.balance_sweep_scheduled_slots
    WHERE target_id = ${capFixture.targetId}
      AND status = 'scheduled'
      AND eligible_after < now() - interval '1 second'
  `;
  check(
    "lot past the attempt budget is dead-lettered instead of rescheduled",
    exhaustedLot.status === "suppressed",
    exhaustedLot
  );
  check(
    "lot inside the attempt budget is still carried forward",
    movedLot.status === "open" && String(movedLot.scheduled_slot_id) !== String(claimable.slotId),
    movedLot
  );
  check(
    "no replacement slot is created already past due",
    pastDueSlots === 0,
    pastDueSlots
  );

  await sql.end();
} finally {
  rpc.stop();
  await cluster?.stop();
  rmSync(root, { recursive: true, force: true });
}

console.log(`\n${checks - failures}/${checks} checks passed`);
if (failures > 0) {
  console.log(`${failures} check(s) FAILED`);
  process.exit(1);
}
console.log("autodeposit closed-route-policy recovery verified");
