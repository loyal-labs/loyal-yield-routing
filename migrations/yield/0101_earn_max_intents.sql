-- Earn Max withdraw and cancel memos (loyal:earn-max:v2:...) naming one of our
-- routes, one row per memo instruction. The observer inserts each as pending;
-- the multiply worker, which owns multiply_route_states, applies or refuses it
-- under its route lease and records the decision. amount_raw NULL is a "max"
-- withdrawal, resolved from the route's equity when applied. Intents apply
-- in chain order: slot, transaction_index (the transaction's index in its
-- block) and instruction_index. transaction_index is NULL only on the intents
-- backfilled below, which applied before it was recorded.
CREATE TABLE loyal_yield.earn_max_intents (
    signature TEXT NOT NULL,
    instruction_index INTEGER NOT NULL CHECK (instruction_index >= 0),
    settings TEXT NOT NULL,
    vault TEXT NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('withdraw', 'cancel')),
    request_id TEXT NOT NULL,
    amount_raw NUMERIC(20, 0) CHECK (amount_raw > 0),
    destination TEXT,
    slot BIGINT NOT NULL CHECK (slot > 0),
    transaction_index BIGINT CHECK (transaction_index >= 0),
    status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'applied', 'refused')),
    refusal_reason TEXT,
    observed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    decided_at TIMESTAMPTZ,
    PRIMARY KEY (signature, instruction_index),
    CHECK ((kind = 'withdraw') = (destination IS NOT NULL)),
    CHECK (kind = 'withdraw' OR amount_raw IS NULL),
    CHECK ((status = 'refused') = (refusal_reason IS NOT NULL)),
    CHECK ((status = 'pending') = (decided_at IS NULL)),
    CHECK (transaction_index IS NOT NULL OR status = 'applied')
);

CREATE INDEX earn_max_intents_pending_idx
    ON loyal_yield.earn_max_intents (settings, slot, transaction_index, instruction_index)
    WHERE status = 'pending';

-- Intents applied before this table existed become applied rows, so a memo
-- the stream replays is not decided twice.
INSERT INTO loyal_yield.earn_max_intents
    (signature, instruction_index, settings, vault, kind, request_id, amount_raw,
     destination, slot, status, observed_at, decided_at)
SELECT operation.transaction_signature, operation.source_instruction_index, route.settings, route.vault,
       operation.expected_effects #>> '{intent,kind}', operation.expected_effects #>> '{intent,requestId}',
       (operation.expected_effects #>> '{intent,amountRaw}')::NUMERIC,
       operation.expected_effects #>> '{intent,destinationAccount}',
       operation.confirmed_slot, 'applied', operation.created_at, operation.created_at
FROM loyal_yield.multiply_operations operation
JOIN loyal_yield.multiply_route_states route ON route.route_key = operation.route_key
WHERE operation.action IN ('request_withdrawal', 'cancel_withdrawal')
  AND operation.idempotency_key LIKE '%:intent:%'
  AND operation.transaction_signature IS NOT NULL
  AND operation.source_instruction_index >= 0
  AND operation.confirmed_slot > 0
  AND operation.expected_effects #>> '{intent,requestId}' IS NOT NULL
  AND (
    (operation.expected_effects #>> '{intent,kind}' = 'withdraw'
     AND operation.expected_effects #>> '{intent,destinationAccount}' IS NOT NULL
     AND (operation.expected_effects #>> '{intent,amountRaw}')::NUMERIC > 0)
    OR (operation.expected_effects #>> '{intent,kind}' = 'cancel'
        AND operation.expected_effects #> '{intent,amountRaw}' IS NULL
        AND operation.expected_effects #> '{intent,destinationAccount}' IS NULL)
  )
ON CONFLICT DO NOTHING;
