import { afterAll, beforeAll, beforeEach, describe, expect, test } from "bun:test";
import { SQL } from "bun";
import {
  AutodepositIdleVaultBalanceError,
  assertEmptyVaultBeforeDirectAutodeposit,
  deferIdleVaultScheduledSlot,
} from "./execute-autodeposit-policy";

const usdc = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v";

for (const amount of [25000001n, 30000000n, 885010000n]) {
  test(`idle custody remains guarded at ${amount} raw`, () => {
    expect(() => assertEmptyVaultBeforeDirectAutodeposit(amount)).toThrow(
      AutodepositIdleVaultBalanceError
    );
  });
}

for (const amount of [0n, 1n, 3n, 91989n, 305209n, 1000000n, 10336170n, 25000000n]) {
  test(`tolerated idle (${amount} raw) does not touch the scheduled slot`, async () => {
    expect(await deferIdleVaultScheduledSlot({
      neon: (() => { throw new Error("must not query"); }) as never,
      databaseUrl: "unused",
      targetId: 7068n,
      scheduledSlotId: 1n,
      vaultBalanceRaw: amount,
    })).toBe(false);
  });
}

// Explicit disposable LOCAL database only; never point fixture DDL at production.
const databaseUrl = process.env.AUTODEPOSIT_IDLE_TEST_DATABASE_URL;
if (databaseUrl && !["localhost", "127.0.0.1", "[::1]"].includes(new URL(databaseUrl).hostname)) {
  throw new Error("idle deferral fixtures require a disposable localhost database");
}

describe.skipIf(!databaseUrl)("idle deferral PostgreSQL contract", () => {
  let sql: SQL;
  beforeAll(async () => {
    sql = new SQL(databaseUrl!);
    await sql`CREATE SCHEMA loyal_yield`;
    await sql`CREATE TABLE loyal_yield.balance_sweep_scheduled_slots (
      id bigint PRIMARY KEY, target_id bigint NOT NULL, token_mint text NOT NULL,
      status text NOT NULL, claim_token text, execution_id bigint,
      eligible_after timestamptz NOT NULL, last_error text,
      updated_at timestamptz NOT NULL DEFAULT now()
    )`;
    await sql`CREATE TABLE loyal_yield.balance_sweep_surplus_lots (
      scheduled_slot_id bigint NOT NULL, target_id bigint NOT NULL,
      status text NOT NULL, remaining_amount_raw bigint NOT NULL
    )`;
    await sql`CREATE TABLE loyal_yield.balance_sweep_transaction_attempts (
      scheduled_slot_id bigint NOT NULL, attempt_state text NOT NULL
    )`;
  });
  afterAll(async () => { if (sql) await sql.close(); });
  beforeEach(async () => {
    await sql`TRUNCATE loyal_yield.balance_sweep_scheduled_slots,
      loyal_yield.balance_sweep_transaction_attempts,
      loyal_yield.balance_sweep_surplus_lots`;
    await sql`INSERT INTO loyal_yield.balance_sweep_scheduled_slots
      (id,target_id,token_mint,status,eligible_after)
      VALUES (1,7068,${usdc},'scheduled',now()-interval '1 minute')`;
    await sql`INSERT INTO loyal_yield.balance_sweep_surplus_lots VALUES (1,7068,'open',1000000)`;
  });

  const defer = (amount = 30000000n, targetId = 7068n) => deferIdleVaultScheduledSlot({
    neon: (() => sql) as never,
    databaseUrl: databaseUrl!,
    targetId,
    scheduledSlotId: 1n,
    vaultBalanceRaw: amount,
  });

  for (const amount of [25000001n, 30000000n]) {
    test(`retries ${amount} raw on the same slot without creating a claim`, async () => {
      let firstBlocked: string | undefined;
      for (let retry = 0; retry < 3; retry++) {
        expect(await defer(amount)).toBe(true);
        const [row] = await sql`SELECT *,
          eligible_after > now()+interval '4 minutes' AS delayed
          FROM loyal_yield.balance_sweep_scheduled_slots`;
        expect(row.status).toBe("scheduled");
        expect(row.claim_token).toBeNull();
        expect(row.execution_id).toBeNull();
        expect(row.delayed).toBe(true);
        const marker = row.last_error.match(/\[idle_blocked_since=([0-9]+); idle_deferrals=([0-9]+)\]$/);
        expect(marker).not.toBeNull();
        firstBlocked ??= marker[1];
        expect(marker[1]).toBe(firstBlocked);
        expect(marker[2]).toBe(String(retry + 1));
        expect(row.last_error).toStartWith(`existing idle vault balance must drain before direct autodeposit: ${amount} [`);
        // A duplicate process cannot repeatedly postpone already-deferred work.
        expect(await defer(amount)).toBe(false);
        await sql`UPDATE loyal_yield.balance_sweep_scheduled_slots
          SET eligible_after=now()-interval '1 second' WHERE id=1`;
      }
      const [count] = await sql`SELECT count(*)::int AS n FROM loyal_yield.balance_sweep_scheduled_slots`;
      expect(count.n).toBe(1);
      const [lot] = await sql`SELECT remaining_amount_raw::text AS amount, status
        FROM loyal_yield.balance_sweep_surplus_lots`;
      expect(lot.amount).toBe("1000000");
      expect(lot.status).toBe("open");
    });
  }

  test("durable idle age survives balance changes and caps the count", async () => {
    await sql`UPDATE loyal_yield.balance_sweep_scheduled_slots
      SET last_error='existing idle vault balance must drain before direct autodeposit: 30000000 [idle_blocked_since=1700000000; idle_deferrals=1000000]'`;
    expect(await defer(25000001n)).toBe(true);
    const [row] = await sql`SELECT last_error FROM loyal_yield.balance_sweep_scheduled_slots`;
    expect(row.last_error).toBe('existing idle vault balance must drain before direct autodeposit: 25000001 [idle_blocked_since=1700000000; idle_deferrals=1000000]');
  });

  for (const previous of [
    'unrelated failure [idle_blocked_since=1700000000; idle_deferrals=40]',
    'existing idle vault balance must drain before direct autodeposit: 3 [idle_blocked_since=oops; idle_deferrals=40]',
    'existing idle vault balance must drain before direct autodeposit: 3 [idle_blocked_since=999999999999999999999; idle_deferrals=40]',
  ]) {
    test(`unrelated/malformed idle history resets safely: ${previous}`, async () => {
      await sql`UPDATE loyal_yield.balance_sweep_scheduled_slots SET last_error=${previous}`;
      expect(await defer()).toBe(true);
      const [row] = await sql`SELECT last_error FROM loyal_yield.balance_sweep_scheduled_slots`;
      expect(row.last_error).toMatch(/\[idle_blocked_since=[0-9]{10}; idle_deferrals=1\]$/);
    });
  }

  test("concurrent deferrals persist only one deadline transition", async () => {
    const results = await Promise.all([defer(), defer()]);
    expect(results.sort()).toEqual([false, true]);
  });

  test("a concurrent selection wins without losing its claim", async () => {
    let deferred: Promise<boolean> | undefined;
    await sql.begin(async (transaction) => {
      await transaction`UPDATE loyal_yield.balance_sweep_scheduled_slots
        SET status='selected',claim_token='concurrent-owner' WHERE id=1`;
      deferred = defer();
      // Hold selection's row lock while the deferral query starts.
      await Bun.sleep(20);
    });
    expect(await deferred).toBe(false);
    const [row] = await sql`SELECT status,claim_token,last_error
      FROM loyal_yield.balance_sweep_scheduled_slots WHERE id=1`;
    expect(row.status).toBe("selected");
    expect(row.claim_token).toBe("concurrent-owner");
    expect(row.last_error).toBeNull();
  });

  test("historical empty slots are not turned into actionable deferrals", async () => {
    await sql`DELETE FROM loyal_yield.balance_sweep_surplus_lots`;
    expect(await defer()).toBe(false);
    await sql`INSERT INTO loyal_yield.balance_sweep_surplus_lots VALUES (1,7068,'consumed',0)`;
    expect(await defer()).toBe(false);
  });

  test("requested work is deferred without changing request status", async () => {
    await sql`UPDATE loyal_yield.balance_sweep_scheduled_slots SET status='requested'`;
    expect(await defer()).toBe(true);
    const [row] = await sql`SELECT status FROM loyal_yield.balance_sweep_scheduled_slots`;
    expect(row.status).toBe("requested");
  });

  test("wrong target and mint cannot move another slot's deadline", async () => {
    expect(await defer(1n, 7070n)).toBe(false);
    await sql`UPDATE loyal_yield.balance_sweep_scheduled_slots SET token_mint='other-mint'`;
    expect(await defer()).toBe(false);
  });

  for (const status of ["selected", "completed", "failed"]) {
    test(`${status} ownership remains untouched`, async () => {
      await sql`UPDATE loyal_yield.balance_sweep_scheduled_slots SET status=${status}`;
      expect(await defer()).toBe(false);
    });
  }
  test("claim and execution ownership remain untouched even on a scheduled row", async () => {
    await sql`UPDATE loyal_yield.balance_sweep_scheduled_slots SET claim_token='owned'`;
    expect(await defer()).toBe(false);
    await sql`UPDATE loyal_yield.balance_sweep_scheduled_slots SET claim_token=NULL,execution_id=42`;
    expect(await defer()).toBe(false);
  });
  for (const state of ["prepared", "submitted", "confirmed", "unknown", "ambiguous"]) {
    test(`${state} transaction evidence retains recovery priority`, async () => {
      await sql`INSERT INTO loyal_yield.balance_sweep_transaction_attempts VALUES (1,${state})`;
      expect(await defer()).toBe(false);
      const [row] = await sql`SELECT last_error FROM loyal_yield.balance_sweep_scheduled_slots`;
      expect(row.last_error).toBeNull();
    });
  }
});
