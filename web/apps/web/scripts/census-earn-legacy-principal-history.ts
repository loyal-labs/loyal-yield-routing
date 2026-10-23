import { mock } from "bun:test";

import { sql } from "drizzle-orm";

mock.module("server-only", () => ({}));

const { getYieldOptimizationClient } = await import(
  "@/lib/yield-optimization/yield-neon-client.server"
);

const result = await getYieldOptimizationClient().db.execute(sql`
  WITH ledger AS (
    SELECT deposit.settings, deposit.vault_index, deposit.wallet_address,
           EXISTS (
             SELECT 1 FROM loyal_yield.user_yield_position_holding_events holding
             WHERE holding.source_deposit_id = deposit.id
           ) AS linked
    FROM loyal_yield.user_yield_position_deposits deposit
    UNION ALL
    SELECT withdrawal.settings, withdrawal.vault_index, withdrawal.wallet_address,
           EXISTS (
             SELECT 1 FROM loyal_yield.user_yield_position_holding_events holding
             WHERE holding.source_withdrawal_id = withdrawal.id
           ) AS linked
    FROM loyal_yield.user_yield_position_withdrawals withdrawal
  ),
  wallets AS (
    SELECT settings, vault_index, wallet_address,
           count(*) FILTER (WHERE linked) AS linked_rows,
           count(*) FILTER (WHERE NOT linked) AS unlinked_rows
    FROM ledger
    GROUP BY settings, vault_index, wallet_address
  )
  SELECT CASE
           WHEN unlinked_rows = 0 THEN 'position'
           WHEN linked_rows = 0 THEN 'legacy'
           ELSE 'mixed'
         END AS history,
         count(*)::text AS wallets,
         sum(unlinked_rows)::text AS unlinked_rows
  FROM wallets
  GROUP BY 1
  ORDER BY 1
`);

console.table(result.rows);
