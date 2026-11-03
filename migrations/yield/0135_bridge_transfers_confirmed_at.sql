ALTER TABLE loyal_yield.bridge_transfers ADD COLUMN confirmed_at timestamptz;

CREATE INDEX bridge_transfers_confirmed_at ON loyal_yield.bridge_transfers (confirmed_at DESC) WHERE state = 'quoted' AND confirmed_at IS NOT NULL;
CREATE INDEX bridge_transfers_unconfirmed_quoted_at ON loyal_yield.bridge_transfers (quoted_at) WHERE state = 'quoted' AND confirmed_at IS NULL;
