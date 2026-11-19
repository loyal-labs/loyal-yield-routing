-- One reservation draws from one verified harvested lot and its own referee.
CREATE TABLE loyal_yield.referral_earn_max_settlements (
  settlement_id uuid PRIMARY KEY,
  lot_id bigint NOT NULL REFERENCES loyal_yield.referral_earn_max_fee_lots(lot_id),
  referee_app_user_id uuid NOT NULL REFERENCES loyal_yield.referral_bindings(referee_app_user_id),
  referrer_app_user_id uuid NOT NULL,
  state text NOT NULL CHECK (state IN ('prepared','signed','delivered','failed','expired')),
  wallet text NOT NULL,
  settings text NOT NULL,
  recipient_owner text NOT NULL,
  recipient_ata text NOT NULL,
  treasury_owner text NOT NULL,
  treasury_ata text NOT NULL CHECK (treasury_ata <> recipient_ata),
  lp_mint text NOT NULL,
  amount_lp_raw numeric(20,0) NOT NULL CHECK (amount_lp_raw > 0 AND amount_lp_raw <= 18446744073709551615),
  quoted_micro_usdc numeric(39,0) NOT NULL CHECK (quoted_micro_usdc > 0),
  quote_asset_micro numeric(20,0) NOT NULL CHECK (quote_asset_micro > 0),
  quote_supply_raw numeric(39,0) NOT NULL CHECK (quote_supply_raw > 0),
  quote_slot bigint NOT NULL CHECK (quote_slot > 0),
  quote_at timestamptz NOT NULL,
  nav_report_at timestamptz NOT NULL,
  earned_through_slot bigint NOT NULL CHECK (earned_through_slot > 0 AND earned_through_slot <= quote_slot),
  last_valid_height bigint NOT NULL CHECK (last_valid_height > 0),
  unsigned_wire bytea NOT NULL CHECK (octet_length(unsigned_wire) BETWEEN 1 AND 1232),
  signed_wire bytea CHECK (octet_length(signed_wire) BETWEEN 1 AND 1232),
  signature text UNIQUE,
  finalized_slot bigint CHECK (finalized_slot > quote_slot),
  finalized_at timestamptz,
  receipt jsonb,
  CHECK (quoted_micro_usdc = div(amount_lp_raw * quote_asset_micro + quote_supply_raw - 1, quote_supply_raw)),
  CHECK ((state<>'prepared' OR signed_wire IS NULL) AND (state NOT IN ('signed','delivered','failed') OR signed_wire IS NOT NULL) AND (signed_wire IS NULL) = (signature IS NULL)),
  CHECK ((state IN ('delivered','failed','expired')) = (finalized_slot IS NOT NULL) AND
         (finalized_slot IS NULL) = (finalized_at IS NULL) AND (finalized_slot IS NULL) = (receipt IS NULL))
);
CREATE UNIQUE INDEX referral_earn_max_referee_open ON loyal_yield.referral_earn_max_settlements(referee_app_user_id)
  WHERE state IN ('prepared','signed');
CREATE INDEX referral_earn_max_settlements_lot ON loyal_yield.referral_earn_max_settlements(lot_id);
CREATE INDEX referral_earn_max_settlements_treasury_open ON loyal_yield.referral_earn_max_settlements(treasury_ata)
  WHERE state IN ('prepared','signed');

CREATE FUNCTION loyal_yield.guard_referral_earn_max_settlement() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
  verified_lot numeric;
  earned numeric;
BEGIN
  IF TG_OP = 'INSERT' THEN
    PERFORM pg_advisory_xact_lock_shared(hashtextextended('referral-identity-evidence',0));
    PERFORM 1 FROM loyal_yield.referral_bindings WHERE referee_app_user_id=NEW.referee_app_user_id FOR UPDATE;
    SELECT amount_lp_raw INTO verified_lot FROM loyal_yield.referral_earn_max_fee_lots WHERE lot_id=NEW.lot_id FOR UPDATE;
    SELECT earned_micro_usdc INTO earned FROM loyal_yield.referral_earn_max_capital_status
      WHERE referee_app_user_id=NEW.referee_app_user_id AND earned_micro_usdc IS NOT NULL AND earned_through_slot=NEW.earned_through_slot FOR SHARE;
    IF earned IS NULL OR EXISTS (SELECT 1 FROM loyal_yield.referral_attribution_conflicts WHERE referee_app_user_id=NEW.referee_app_user_id)
      OR verified_lot IS NULL OR NEW.amount_lp_raw + (SELECT COALESCE(sum(amount_lp_raw),0) FROM loyal_yield.referral_earn_max_settlements
        WHERE lot_id=NEW.lot_id AND state IN ('prepared','signed','delivered')) > verified_lot
      OR NEW.quoted_micro_usdc + (SELECT COALESCE(sum(quoted_micro_usdc),0) FROM loyal_yield.referral_earn_max_settlements
        WHERE referee_app_user_id=NEW.referee_app_user_id AND state IN ('prepared','signed','delivered')) > earned THEN
      RAISE EXCEPTION 'settlement exceeds proved funding or earned rewards';
    END IF;
    IF NEW.state <> 'prepared' OR NOT EXISTS (
      SELECT 1 FROM loyal_yield.referral_earn_max_fee_lots l
      WHERE l.lot_id=NEW.lot_id AND l.referee_app_user_id=NEW.referee_app_user_id AND l.referrer_app_user_id=NEW.referrer_app_user_id
        AND l.treasury_lp_account=NEW.treasury_ata AND l.lp_mint=NEW.lp_mint AND l.harvest_slot<=NEW.earned_through_slot
    ) THEN RAISE EXCEPTION 'settlement does not match verified fee lot'; END IF;
    RETURN NEW;
  END IF;
  IF TG_OP = 'DELETE' OR (to_jsonb(OLD) - ARRAY['state','signed_wire','signature','finalized_slot','finalized_at','receipt'])
                       <> (to_jsonb(NEW) - ARRAY['state','signed_wire','signature','finalized_slot','finalized_at','receipt']) THEN
    RAISE EXCEPTION 'settlement terms are immutable';
  END IF;
  IF OLD.state IN ('prepared','signed') AND NEW.state='expired' AND OLD.signed_wire IS NOT DISTINCT FROM NEW.signed_wire AND OLD.signature IS NOT DISTINCT FROM NEW.signature THEN RETURN NEW; END IF;
  IF OLD.state='prepared' AND NEW.state='signed' THEN RETURN NEW; END IF;
  IF OLD.state='signed' AND NEW.state IN ('delivered','failed') AND OLD.signed_wire=NEW.signed_wire AND OLD.signature=NEW.signature THEN RETURN NEW; END IF;
  RAISE EXCEPTION 'invalid settlement transition';
END;
$$;
CREATE TRIGGER guard_referral_earn_max_settlement BEFORE INSERT OR UPDATE OR DELETE ON loyal_yield.referral_earn_max_settlements
FOR EACH ROW EXECUTE FUNCTION loyal_yield.guard_referral_earn_max_settlement();

CREATE VIEW loyal_yield.referral_earn_max_paid AS
SELECT b.referee_app_user_id,
  COALESCE(sum(s.quoted_micro_usdc) FILTER (WHERE s.state='delivered'),0) AS paid_micro_usdc,
  max(s.finalized_slot) FILTER (WHERE s.state='delivered') AS last_paid_slot,
  COALESCE(bool_or(s.state IN ('prepared','signed')),false) AS has_open_settlement
FROM loyal_yield.referral_bindings b LEFT JOIN loyal_yield.referral_earn_max_settlements s USING(referee_app_user_id)
GROUP BY b.referee_app_user_id;
