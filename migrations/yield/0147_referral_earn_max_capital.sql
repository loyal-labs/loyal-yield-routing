-- The effective launch is supplied explicitly by the operator after approval.
-- Installing the schema alone cannot start accruing rewards.
CREATE TABLE loyal_yield.referral_earn_max_program (
  singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
  launch_slot bigint NOT NULL CHECK (launch_slot > 0),
  launched_at timestamptz NOT NULL
);

CREATE FUNCTION loyal_yield.protect_referral_earn_max_launch() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION 'Earn Max referral launch is immutable';
END;
$$;
CREATE TRIGGER protect_referral_earn_max_launch BEFORE UPDATE OR DELETE ON loyal_yield.referral_earn_max_program
FOR EACH ROW EXECUTE FUNCTION loyal_yield.protect_referral_earn_max_launch();

CREATE TABLE loyal_yield.referral_earn_max_capital_facts (
  referee_app_user_id uuid NOT NULL REFERENCES loyal_yield.referral_bindings(referee_app_user_id),
  authority text NOT NULL,
  signature text NOT NULL,
  slot bigint NOT NULL CHECK (slot > 0),
  block_time timestamptz NOT NULL,
  movement jsonb NOT NULL,
  PRIMARY KEY (referee_app_user_id, authority, signature)
);

CREATE TABLE loyal_yield.referral_earn_max_capital_status (
  referee_app_user_id uuid PRIMARY KEY REFERENCES loyal_yield.referral_bindings(referee_app_user_id),
  authority text NOT NULL,
  as_of_slot bigint NOT NULL CHECK (as_of_slot > 0),
  as_of_at timestamptz NOT NULL,
  coverage text NOT NULL CHECK (coverage IN ('complete','incomplete')),
  reason text,
  earned_micro_usdc numeric(39,0) CHECK (earned_micro_usdc >= 0),
  earned_through_slot bigint CHECK (earned_through_slot > 0 AND earned_through_slot <= as_of_slot),
  earned_through_at timestamptz,
  replay jsonb,
  CHECK ((coverage = 'incomplete' AND reason IS NOT NULL) OR
         (coverage = 'complete' AND reason IS NULL AND earned_through_slot = as_of_slot AND earned_through_slot IS NOT NULL)),
  CHECK ((earned_micro_usdc IS NULL) = (replay IS NULL) AND
         (earned_through_slot IS NULL) = (replay IS NULL) AND (earned_through_at IS NULL) = (replay IS NULL))
);
