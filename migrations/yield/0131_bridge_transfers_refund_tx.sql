ALTER TABLE loyal_yield.bridge_transfers ADD COLUMN refund_tx text;

DROP INDEX loyal_yield.bridge_transfers_open_updated_at;
CREATE INDEX bridge_transfers_open_submitted_at ON loyal_yield.bridge_transfers (submitted_at)
WHERE state IN ('submitted', 'failed') OR (state = 'filled' AND destination_tx IS NULL) OR (state = 'refunded' AND refund_tx IS NULL);
