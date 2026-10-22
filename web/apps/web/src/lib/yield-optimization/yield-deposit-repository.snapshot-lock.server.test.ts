import { expect, mock, test } from "bun:test";
import { drizzle } from "drizzle-orm/postgres-js";
import postgres from "postgres";

mock.module("server-only", () => ({}));

const { recordConfirmedYieldWithdrawal } = await import(
  "./yield-deposit-repository.server"
);
const { yieldOptimizationSchema } = await import("./yield-neon-client.server");

const databaseUrl = process.env.OBSERVER_TEST_DATABASE_URL;

type Arrival = { count: number; waiters: Array<() => void> };

function lockstepWriter(url: string, arrivals: Map<number, Arrival>) {
  const connection = postgres(url, { max: 1, onnotice: () => {} });
  const db = drizzle(connection, { schema: yieldOptimizationSchema });
  let calls = 0;
  const batch = async (queries: PromiseLike<unknown>[]) => {
    const index = calls++;
    const arrival = arrivals.get(index) ?? { count: 0, waiters: [] };
    arrivals.set(index, arrival);
    arrival.count += 1;
    if (arrival.count === 2) {
      for (const release of arrival.waiters) release();
    } else {
      await Promise.race([
        new Promise<void>((resolve) => arrival.waiters.push(resolve)),
        new Promise((resolve) => setTimeout(resolve, 2_000)),
      ]);
    }
    await connection`begin`;
    try {
      const results: unknown[] = [];
      for (const query of queries) results.push(await query);
      await connection`commit`;
      return results;
    } catch (error) {
      await connection`rollback`;
      throw error;
    }
  };
  Object.assign(db, { batch });
  return {
    close: () => connection.end(),
    dependencies: {
      client: { db } as never,
      now: () => new Date("2026-10-22T00:00:00Z"),
    },
  };
}

test.skipIf(!databaseUrl)(
  "concurrent confirmed withdrawals both debit the current reserve row",
  async () => {
    const admin = postgres(databaseUrl!, { max: 1, onnotice: () => {} });
    const suffix = `${Date.now()}`;
    const settings = `settings-${suffix}`;
    const vaultPubkey = `vault-${suffix}`;
    const wallet = `wallet-${suffix}`;
    const reserve = `reserve-${suffix}`;
    const mint = "usdc-mint";
    const [policy] = await admin`
      INSERT INTO loyal_yield.route_policies
        (settings, authority, policy_seed, policy_account, vault_index, vault_pubkey, threshold, last_seen_slot, last_seen_signature)
      VALUES (${settings}, 'authority', 1, ${`policy-${suffix}`}, 0, ${vaultPubkey}, 1, 100, 'signature') RETURNING id`;
    const [vault] = await admin`
      INSERT INTO loyal_yield.managed_vaults (settings, vault_index, vault_pubkey, active_policy_id)
      VALUES (${settings}, 0, ${vaultPubkey}, ${policy.id}) RETURNING id`;
    const [snapshot] = await admin`
      INSERT INTO loyal_yield.vault_position_snapshots (vault_id, policy_id, observed_slot)
      VALUES (${vault.id}, ${policy.id}, 100) RETURNING id`;
    await admin`
      INSERT INTO loyal_yield.vault_reserve_positions_current
        (vault_id, reserve, market, liquidity_mint, amount_raw, has_value, snapshot_id, observed_slot, observed_at, planning_metadata)
      VALUES (${vault.id}, ${reserve}, 'market', ${mint}, 1000, true, ${snapshot.id}, 100, now(),
        ${admin.json({ amountSemantics: "kamino_redeemable_liquidity" })})`;
    await admin`
      INSERT INTO loyal_yield.user_yield_positions
        (wallet_address, smart_account_address, settings, vault_index, vault_pubkey, policy_id, policy_account,
         policy_seed, initial_reserve, initial_liquidity_mint, deposit_mint, principal_amount_raw,
         first_deposit_signature, last_deposit_signature, last_confirmed_slot, status, created_at, updated_at,
         current_reserve, current_market, current_liquidity_mint, current_amount_raw, current_observed_slot, current_observed_at)
      VALUES (${wallet}, ${settings}, ${settings}, 0, ${vaultPubkey}, ${policy.id}, ${`policy-${suffix}`}, 1, ${reserve}, ${mint}, ${mint},
        1000, ${`deposit-${suffix}`}, ${`deposit-${suffix}`}, 100, 'active', now(), now(),
        ${reserve}, 'market', ${mint}, 1000, 100, now())`;

    const arrivals = new Map<number, Arrival>();
    const writers = [
      lockstepWriter(databaseUrl!, arrivals),
      lockstepWriter(databaseUrl!, arrivals),
    ];
    const withdraw = (index: number, amountRaw: bigint) =>
      recordConfirmedYieldWithdrawal(
        {
          cluster: "mainnet",
          confirmedReserveDebitAmountRaw: amountRaw,
          confirmedSlot: BigInt(200 + index),
          delegatedSigner: "signer",
          liquidityMint: mint,
          market: "market",
          mode: "partial",
          policyAccount: `policy-${suffix}`,
          policyId: BigInt(policy.id),
          policySeed: BigInt(1),
          settings,
          smartAccountAddress: settings,
          sourceAmountRaw: BigInt(1000),
          sourceId: reserve,
          sourceType: "reserve",
          targetReserve: reserve,
          vaultIndex: 0,
          vaultPubkey,
          walletAddress: wallet,
          withdrawalSignature: `withdrawal-${suffix}-${index}`,
          withdrawnAmountRaw: amountRaw,
        },
        writers[index].dependencies
      );

    try {
      await Promise.all([withdraw(0, BigInt(300)), withdraw(1, BigInt(400))]);
      const [row] = await admin`
        SELECT amount_raw::text AS amount FROM loyal_yield.vault_reserve_positions_current
        WHERE vault_id = ${vault.id} AND reserve = ${reserve}`;
      expect(row.amount).toBe("300");
    } finally {
      await Promise.all(writers.map((writer) => writer.close()));
      await admin.end();
    }
  }
);
