ALTER TYPE loyal_yield.balance_sweep_scheduled_slot_status
    ADD VALUE IF NOT EXISTS 'blocked';

ALTER TABLE loyal_yield.balance_sweep_targets
    ADD COLUMN IF NOT EXISTS execution_blocked_reason TEXT
        CHECK (
            execution_blocked_reason IS NULL
            OR execution_blocked_reason = 'account_not_found'
        ),
    ADD COLUMN IF NOT EXISTS execution_block_evidence JSONB
        NOT NULL DEFAULT '{}'::jsonb,
    ADD COLUMN IF NOT EXISTS execution_blocked_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS execution_block_last_checked_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS execution_block_last_check_error TEXT,
    ADD COLUMN IF NOT EXISTS execution_block_recovered_at TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS balance_sweep_targets_execution_block_scan_idx
    ON loyal_yield.balance_sweep_targets (
        execution_blocked_at,
        id
    )
    WHERE execution_blocked_reason = 'account_not_found';

CREATE OR REPLACE VIEW loyal_yield.balance_sweep_execution_block_metrics AS
WITH target_blocks AS (
    SELECT
        wallet,
        COALESCE(execution_block_evidence ->> 'accountRole', 'unknown') AS account_role,
        execution_blocked_reason,
        execution_blocked_at,
        execution_block_recovered_at
    FROM loyal_yield.balance_sweep_targets
    WHERE execution_blocked_at IS NOT NULL
),
role_metrics AS (
    SELECT
        account_role,
        COUNT(*) FILTER (
            WHERE execution_blocked_at >= now() - interval '24 hours'
        )::BIGINT AS new_blocks_24h,
        COUNT(DISTINCT wallet) FILTER (
            WHERE execution_blocked_at >= now() - interval '24 hours'
        )::BIGINT AS new_unique_wallets_24h,
        COUNT(*) FILTER (
            WHERE execution_blocked_reason = 'account_not_found'
        )::BIGINT AS active_blocks,
        COUNT(DISTINCT wallet) FILTER (
            WHERE execution_blocked_reason = 'account_not_found'
        )::BIGINT AS active_unique_wallets,
        COUNT(*) FILTER (
            WHERE execution_blocked_reason IS NULL
              AND execution_block_recovered_at >= now() - interval '24 hours'
        )::BIGINT AS recovered_blocks_24h,
        COUNT(DISTINCT wallet) FILTER (
            WHERE execution_blocked_reason IS NULL
              AND execution_block_recovered_at >= now() - interval '24 hours'
        )::BIGINT AS recovered_unique_wallets_24h,
        MIN(execution_blocked_at) FILTER (
            WHERE execution_blocked_reason = 'account_not_found'
        ) AS oldest_active_block_at
    FROM target_blocks
    GROUP BY account_role
),
all_metrics AS (
    SELECT
        'all'::TEXT AS account_role,
        COUNT(*) FILTER (
            WHERE execution_blocked_at >= now() - interval '24 hours'
        )::BIGINT AS new_blocks_24h,
        COUNT(DISTINCT wallet) FILTER (
            WHERE execution_blocked_at >= now() - interval '24 hours'
        )::BIGINT AS new_unique_wallets_24h,
        COUNT(*) FILTER (
            WHERE execution_blocked_reason = 'account_not_found'
        )::BIGINT AS active_blocks,
        COUNT(DISTINCT wallet) FILTER (
            WHERE execution_blocked_reason = 'account_not_found'
        )::BIGINT AS active_unique_wallets,
        COUNT(*) FILTER (
            WHERE execution_blocked_reason IS NULL
              AND execution_block_recovered_at >= now() - interval '24 hours'
        )::BIGINT AS recovered_blocks_24h,
        COUNT(DISTINCT wallet) FILTER (
            WHERE execution_blocked_reason IS NULL
              AND execution_block_recovered_at >= now() - interval '24 hours'
        )::BIGINT AS recovered_unique_wallets_24h,
        MIN(execution_blocked_at) FILTER (
            WHERE execution_blocked_reason = 'account_not_found'
        ) AS oldest_active_block_at
    FROM target_blocks
)
SELECT * FROM all_metrics
UNION ALL
SELECT * FROM role_metrics;
