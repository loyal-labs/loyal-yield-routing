import { afterAll, beforeAll, beforeEach, describe, expect, test } from "bun:test";
import { SQL } from "bun";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir, userInfo } from "node:os";
import { join } from "node:path";
import { spawnSync } from "node:child_process";
import type { Connection } from "@solana/web3.js";
import {
  AccountedAutodepositConflictError,
  reconcileAccountedAutodeposit,
} from "./autodeposit-accounted-recovery";
import { resumeDirectKaminoDeposit } from "./execute-autodeposit-policy";

// Real PostgreSQL contract test: never accepts a supplied/production database URL.
const postgresAvailable = Bun.which("initdb") !== null && Bun.which("pg_ctl") !== null;
describe.skipIf(!postgresAvailable)("already-accounted autodeposit recovery (local PostgreSQL)", () => {
  let scratch: string;
  let sql: SQL;
  let started = false;
  const args = {
    claimToken: "claim", leaseToken: "lease", targetId: 4356n,
    scheduledSlotId: 238750n, pullAttemptId: "84",
  };
  function command(name: string, argv: string[]) {
    const result = spawnSync(name, argv, { encoding: "utf8" });
    if (result.status !== 0) throw new Error(`${name}: ${result.stderr}`);
  }
  beforeAll(async () => {
    scratch = mkdtempSync(join(tmpdir(), "autodeposit-accounted-"));
    const port = 59000 + Math.floor(Math.random() * 700);
    command("initdb", ["-D", join(scratch, "pg"), "-A", "trust", "--no-locale", "-E", "UTF8"]);
    command("pg_ctl", ["-D", join(scratch, "pg"), "-l", join(scratch, "postgres.log"),
      "-o", `-F -k '${scratch}' -p ${port} -c listen_addresses=127.0.0.1`, "-w", "start"]);
    started = true;
    sql = new SQL(`postgresql://${userInfo().username}@127.0.0.1:${port}/postgres`);
    await sql.unsafe(`
      CREATE SCHEMA loyal_yield;
      CREATE TABLE loyal_yield.balance_sweep_lot_claims (
        claim_token text PRIMARY KEY, target_id bigint, amount_raw bigint, status text,
        execution_id bigint, autodeposit_executor_lease_token text,
        autodeposit_executor_lease_expires_at timestamptz, autodeposit_deposit_plan jsonb,
        updated_at timestamptz DEFAULT now());
      CREATE TABLE loyal_yield.balance_sweep_scheduled_slots (
        id bigint PRIMARY KEY, target_id bigint, claim_token text, status text,
        execution_id bigint, last_error text, updated_at timestamptz DEFAULT now());
      CREATE TABLE loyal_yield.balance_sweep_transaction_attempts (
        id bigint PRIMARY KEY, claim_token text, target_id bigint, scheduled_slot_id bigint,
        operation_kind text, attempt_state text, confirmed_slot bigint, amount_raw bigint,
        execution_id bigint, signature text, attempt_number int);
      CREATE TABLE loyal_yield.balance_sweep_executions (
        id bigint PRIMARY KEY, target_id bigint, signature text, slot bigint, amount_raw bigint,
        dedupe_key text, source_post_balance_raw bigint, destination_post_balance_raw bigint,
        yield_deposit_id bigint, yield_position_id bigint, kamino_deposit_signature text,
        completed_at timestamptz, completion_failure_code text, decoded_evidence jsonb, decoded_at timestamptz);
      CREATE TABLE loyal_yield.user_yield_position_deposits (
        id bigint PRIMARY KEY, deposit_signature text, confirmed_slot bigint,
        balance_sweep_execution_id bigint, balance_sweep_scheduled_slot_id bigint,
        principal_amount_raw bigint, settings text, vault_index smallint, vault_pubkey text,
        wallet_address text, target_reserve text, liquidity_mint text);
      CREATE TABLE loyal_yield.user_yield_position_holding_events (
        id bigint PRIMARY KEY, source_signature text, source_deposit_id bigint, event_type text,
        principal_delta_raw bigint, observed_slot bigint, position_id bigint);
      CREATE TABLE loyal_yield.user_yield_positions (
        id bigint PRIMARY KEY, settings text, vault_index smallint, vault_pubkey text,
        wallet_address text, initial_reserve text, current_reserve text, status text,
        principal_amount_raw bigint, current_amount_raw bigint, last_confirmed_slot bigint,
        last_holding_event_id bigint, current_observed_slot bigint);
      CREATE TABLE loyal_yield.balance_sweep_lot_claim_items (claim_token text, lot_id bigint, amount_raw bigint);
      CREATE TABLE loyal_yield.balance_sweep_execution_lots (
        execution_id bigint, lot_id bigint, amount_raw bigint, PRIMARY KEY(execution_id, lot_id));
    `);
  }, 60000);
  afterAll(async () => {
    if (sql) await sql.close();
    if (started) command("pg_ctl", ["-D", join(scratch, "pg"), "-m", "immediate", "-w", "stop"]);
    if (scratch) rmSync(scratch, { recursive: true, force: true });
  });
  beforeEach(async () => {
    await sql.unsafe(`
      TRUNCATE loyal_yield.balance_sweep_lot_claims, loyal_yield.balance_sweep_scheduled_slots,
        loyal_yield.balance_sweep_transaction_attempts, loyal_yield.balance_sweep_executions,
        loyal_yield.user_yield_position_deposits, loyal_yield.user_yield_position_holding_events,
        loyal_yield.user_yield_positions, loyal_yield.balance_sweep_lot_claim_items,
        loyal_yield.balance_sweep_execution_lots;
      INSERT INTO loyal_yield.balance_sweep_lot_claims VALUES ('claim',4356,8467048,'selected',NULL,'lease',
        now()+interval '10 minutes','{"amountRaw":"8467048","reserve":"reserve","liquidityMint":"mint",
        "target":{"id":"4356","settings":"settings","vaultIndex":1,"vaultPubkey":"vault","wallet":"wallet"}}',now());
      INSERT INTO loyal_yield.balance_sweep_scheduled_slots VALUES (238750,4356,'claim','selected',NULL,'stale',now());
      INSERT INTO loyal_yield.balance_sweep_transaction_attempts VALUES
        (84,'claim',4356,238750,'pull','confirmed',440440976,8467048,NULL,'pull-sig',1),
        (85,'claim',4356,238750,'top_up','confirmed',440441023,8467048,10677,'deposit-sig',1);
      INSERT INTO loyal_yield.balance_sweep_executions
        (id,target_id,signature,slot,amount_raw,dedupe_key,source_post_balance_raw,destination_post_balance_raw)
        VALUES (10677,4356,'pull-sig',440440976,8467048,'executor-pull',1000000,8467048),
               (10678,4356,'pull-sig',440440976,8467048,'indexer-pull',1000000,8467048);
      INSERT INTO loyal_yield.user_yield_position_deposits VALUES
        (20980,'deposit-sig',440441023,10677,238750,8467048,'settings',1,'vault','wallet','reserve','mint');
      INSERT INTO loyal_yield.user_yield_position_holding_events VALUES
        (1037717,'deposit-sig',20980,'deposit_top_up',8467048,440467178,4361),
        (1041861,'withdrawal-sig',NULL,'withdrawal_full',-8467048,440477231,4361);
      INSERT INTO loyal_yield.user_yield_positions VALUES
        (4361,'settings',1,'vault','wallet','reserve','reserve','closed',0,0,440477231,1041861,440477260);
      INSERT INTO loyal_yield.balance_sweep_lot_claim_items VALUES ('claim',42,8467048);
    `);
  });
  const recover = () => reconcileAccountedAutodeposit({ ...args, sql });
  const accounting = async () => JSON.stringify(await sql.unsafe(`
    SELECT (SELECT jsonb_agg(p) FROM loyal_yield.user_yield_positions p) AS positions,
           (SELECT jsonb_agg(d) FROM loyal_yield.user_yield_position_deposits d) AS deposits,
           (SELECT jsonb_agg(e) FROM loyal_yield.user_yield_position_holding_events e) AS events`));
  async function expectSelected() {
    expect((await sql`SELECT status FROM loyal_yield.balance_sweep_lot_claims`)[0].status).toBe("selected");
    expect((await sql`SELECT count(*)::int AS n FROM loyal_yield.balance_sweep_executions WHERE completed_at IS NOT NULL`)[0].n).toBe(0);
  }
  function resume(connection: Connection) {
    const target = { id: 4356n, managedVaultId: 4837n, settings: "settings", vaultIndex: 1,
      wallet: "wallet", walletUsdcAta: "11111111111111111111111111111111", walletTokenAta: "wallet-ata",
      vaultPubkey: "vault", vaultUsdcAta: "11111111111111111111111111111111", vaultTokenAta: "vault-ata",
      tokenMint: "mint", routePolicyAccount: "policy", routePolicySeed: 1n,
      currentReserve: "reserve", currentMarket: "market", currentLiquidityMint: "mint" };
    const neon = () => Object.assign((strings: TemplateStringsArray, ...values: unknown[]) => sql(strings, ...values),
      { transaction: async (queries: Promise<unknown[]>[]) => Promise.all(queries) });
    return resumeDirectKaminoDeposit({
      ...args, connection, databaseUrl: "local-test-only", neon, rpcUrl: "unused",
      target, plan: { version: 1, amountRaw: 8467048n, reserve: "reserve", market: "market", liquidityMint: "mint", target },
      attempt: { id: "84", claimToken: "claim", operationKind: "pull", executionId: null,
        amountRaw: 8467048n, sourcePreBalanceRaw: 9467048n, destinationPreBalanceRaw: 0n,
        signature: "pull-sig", signedTransactionBase64: "unused", signedTransactionSha256: "unused",
        blockhash: "unused", lastValidBlockHeight: 1n, state: "confirmed", broadcastCount: 1, confirmedSlot: 440440976n },
    });
  }
  test("closed ATA recovery links the exact execution without reads, sends, or position resurrection", async () => {
    const before = await accounting();
    const connection = new Proxy({}, { get() { throw new Error("unexpected chain access"); } }) as Connection;
    const result = await resume(connection);
    expect(result.status).toBe("completed");
    expect(result.executionRecord.executionId).toBe("10677");
    expect(result.walletPostPullRaw).toBe(1000000n);
    expect(result.vaultObservation.amountRaw).toBe(8467048n);
    expect(await accounting()).toBe(before);
    expect((await sql`SELECT status,execution_id FROM loyal_yield.balance_sweep_scheduled_slots`)[0]).toMatchObject({ status: "executed", execution_id: "10677" });
    expect((await sql`SELECT execution_id FROM loyal_yield.balance_sweep_execution_lots`)[0].execution_id).toBe("10677");
    expect((await sql`SELECT completed_at FROM loyal_yield.balance_sweep_executions WHERE id=10678`)[0].completed_at).toBeNull();
    expect(await recover()).toBeNull();
    expect(await accounting()).toBe(before);
  });
  test("a later rebalanced active position is left byte-for-byte unchanged", async () => {
    await sql`UPDATE loyal_yield.user_yield_positions SET status='active',current_reserve='new-reserve',principal_amount_raw=100,current_amount_raw=101`;
    const before = await accounting();
    expect(await recover()).not.toBeNull();
    expect(await accounting()).toBe(before);
  });
  test("a lost or expired lease cannot finalize", async () => {
    expect(await reconcileAccountedAutodeposit({ ...args, sql, leaseToken: "other" })).toBeNull();
    await sql`UPDATE loyal_yield.balance_sweep_lot_claims SET autodeposit_executor_lease_expires_at=now()-interval '1 second'`;
    expect(await recover()).toBeNull();
    await expectSelected();
  });
  for (const [name, mutation] of [
    ["unconfirmed pull", "UPDATE loyal_yield.balance_sweep_transaction_attempts SET attempt_state='unknown' WHERE id=84"],
    ["wrong execution signature", "UPDATE loyal_yield.balance_sweep_executions SET signature='wrong' WHERE id=10677"],
    ["wrong deposit amount", "UPDATE loyal_yield.user_yield_position_deposits SET principal_amount_raw=1"],
    ["wrong deposit identity", "UPDATE loyal_yield.user_yield_position_deposits SET settings='other'"],
    ["wrong execution binding", "UPDATE loyal_yield.user_yield_position_deposits SET balance_sweep_execution_id=10678"],
    ["wrong event binding", "UPDATE loyal_yield.user_yield_position_holding_events SET source_deposit_id=1 WHERE id=1037717"],
    ["missing event", "DELETE FROM loyal_yield.user_yield_position_holding_events WHERE id=1037717"],
    ["missing accounting after withdrawal", "DELETE FROM loyal_yield.user_yield_position_deposits; DELETE FROM loyal_yield.user_yield_position_holding_events WHERE id=1037717"],
    ["conflicting slot", "UPDATE loyal_yield.balance_sweep_scheduled_slots SET execution_id=10678"],
    ["conflicting lots", "INSERT INTO loyal_yield.balance_sweep_execution_lots VALUES (10677,42,1)"],
    ["missing claim items", "DELETE FROM loyal_yield.balance_sweep_lot_claim_items"],
    ["newer topup", "INSERT INTO loyal_yield.balance_sweep_transaction_attempts VALUES (86,'claim',4356,238750,'top_up','unknown',NULL,8467048,10677,'other',2)"],
  ]) {
    test(`${name} fails closed and preserves recovery ownership`, async () => {
      await sql.unsafe(mutation!);
      const before = await accounting();
      await expect(recover()).rejects.toBeInstanceOf(AccountedAutodepositConflictError);
      await expectSelected();
      expect(await accounting()).toBe(before);
    });
  }
  test("unconfirmed topup with existing accounting cannot use either finalizer", async () => {
    await sql`UPDATE loyal_yield.balance_sweep_transaction_attempts SET attempt_state='unknown' WHERE id=85`;
    await expect(recover()).rejects.toBeInstanceOf(AccountedAutodepositConflictError);
    await expectSelected();
  });
  test("a position closed before this new deposit does not falsely block normal reconciliation", async () => {
    await sql`DELETE FROM loyal_yield.user_yield_position_deposits`;
    await sql`DELETE FROM loyal_yield.user_yield_position_holding_events`;
    await sql`UPDATE loyal_yield.user_yield_positions SET last_confirmed_slot=440374330,current_observed_slot=440374331`;
    expect(await recover()).toBeNull();
    await expectSelected();
  });
  test("missing ATA without confirmed accounting is not treated as zero", async () => {
    await sql`DELETE FROM loyal_yield.balance_sweep_transaction_attempts WHERE id=85`;
    let reads = 0;
    const connection = { getTokenAccountBalance: async () => {
      reads += 1;
      if (reads === 1) return { value: { amount: "1000000" }, context: { slot: 440477231 } };
      throw new Error("could not find account");
    } } as unknown as Connection;
    await expect(resume(connection)).rejects.toThrow("could not find account");
    expect(reads).toBe(2);
    await expectSelected();
  });
  test("slot write failure rolls back execution and lots atomically", async () => {
    await sql`ALTER TABLE loyal_yield.balance_sweep_scheduled_slots ADD CONSTRAINT reject_completion CHECK (status <> 'executed')`;
    try {
      await expect(recover()).rejects.toThrow();
      await expectSelected();
      expect((await sql`SELECT count(*)::int AS n FROM loyal_yield.balance_sweep_execution_lots`)[0].n).toBe(0);
    } finally {
      await sql`ALTER TABLE loyal_yield.balance_sweep_scheduled_slots DROP CONSTRAINT reject_completion`;
    }
  });
  test("concurrent recovery completes once", async () => {
    const results = await Promise.all([recover(), recover()]);
    expect(results.filter(Boolean)).toHaveLength(1);
    expect((await sql`SELECT count(*)::int AS n FROM loyal_yield.balance_sweep_execution_lots`)[0].n).toBe(1);
  });
});

test("child outcome protocol is opt-in and keeps completion distinct from pending", () => {
  const env = { ...process.env };
  const outcomes = ["completed", "deferred", "recovery_pending", "noop"] as const;
  for (const outcome of outcomes) delete env[`AUTODEPOSIT_${outcome.toUpperCase()}_EXIT_CODE`];
  function exitFor(outcome: typeof outcomes[number]) {
    return Bun.spawnSync([process.execPath, "--eval", `
      import { setAutodepositExecutorOutcome } from ${JSON.stringify(join(import.meta.dir, "autodeposit-accounted-recovery.ts"))};
      setAutodepositExecutorOutcome(${JSON.stringify(outcome)});
    `], { env, stdout: "ignore", stderr: "pipe" }).exitCode;
  }
  expect(exitFor("recovery_pending")).toBe(0);
  for (const [index, outcome] of outcomes.entries()) {
    env[`AUTODEPOSIT_${outcome.toUpperCase()}_EXIT_CODE`] = String(28 + index);
    expect(exitFor(outcome)).toBe(28 + index);
  }
});
