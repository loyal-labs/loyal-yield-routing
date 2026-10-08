ALTER TYPE loyal_yield.decision_reason
    ADD VALUE IF NOT EXISTS 'voltr_manager_operation';

ALTER TABLE loyal_yield.rebalance_opportunities
    ADD COLUMN IF NOT EXISTS operation_class TEXT NOT NULL
        DEFAULT 'yield_optimization',
    ADD COLUMN IF NOT EXISTS service_deadline_at TIMESTAMPTZ;

ALTER TABLE loyal_yield.rebalance_opportunities
    DROP CONSTRAINT IF EXISTS rebalance_opportunities_operation_class_check;
ALTER TABLE loyal_yield.rebalance_opportunities
    ADD CONSTRAINT rebalance_opportunities_operation_class_check CHECK (
        operation_class IN (
            'yield_optimization',
            'idle_allocation',
            'withdrawal_restoration'
        )
    );

ALTER TABLE loyal_yield.rebalance_opportunities
    DROP CONSTRAINT IF EXISTS rebalance_opportunities_value_check;
ALTER TABLE loyal_yield.rebalance_opportunities
    ADD CONSTRAINT rebalance_opportunities_value_check CHECK (
        amount_raw > 0
        AND principal_usd_micros > 0
        AND estimated_cost_lamports >= 0
        AND jsonb_typeof(execution_plan) = 'object'
        AND (
            (
                operation_class IN ('yield_optimization', 'idle_allocation')
                AND estimated_edge_bps > 0
                AND annual_yield_gain_usd_micros > 0
                AND expected_net_gain_usd_micros > 0
                AND economic_priority > 0
                AND service_deadline_at IS NULL
            )
            OR (
                operation_class = 'withdrawal_restoration'
                AND estimated_edge_bps = 0
                AND annual_yield_gain_usd_micros = 0
                AND expected_net_gain_usd_micros = 0
                AND economic_priority = 0
                AND service_deadline_at IS NOT NULL
            )
        )
    );

CREATE INDEX IF NOT EXISTS rebalance_opportunities_operation_priority_idx
ON loyal_yield.rebalance_opportunities (
    cluster,
    operation_class,
    service_deadline_at,
    scheduler_priority_anchor DESC,
    id
)
WHERE opportunity_state IN ('revalidate', 'ready');

COMMENT ON COLUMN loyal_yield.rebalance_opportunities.operation_class IS
    'Closed semantic class. Withdrawal restoration is urgent service work and must not fabricate APY or gain.';
COMMENT ON COLUMN loyal_yield.rebalance_opportunities.service_deadline_at IS
    'Required only for withdrawal restoration; derived from the confirmed Voltr receipt deadline.';
