-- Preserve positive app-scoped Privy wallet links and the corresponding Loyal
-- user when one exists. The wallet may be seen before its first Loyal signup.
CREATE TABLE loyal_yield.referral_wallet_identities (
    wallet text NOT NULL,
    privy_subject text NOT NULL,
    app_user_id uuid,
    UNIQUE NULLS NOT DISTINCT (wallet, privy_subject, app_user_id)
);
CREATE INDEX referral_wallet_identities_subject ON loyal_yield.referral_wallet_identities (privy_subject, wallet);
CREATE INDEX referral_wallet_identities_user ON loyal_yield.referral_wallet_identities (app_user_id) WHERE app_user_id IS NOT NULL;

ALTER TABLE loyal_yield.referral_bindings ADD COLUMN referee_privy_subject text UNIQUE;

-- A later verified link can reveal an ambiguity between previously independent
-- wallet signups. Preserve both original claims and expose the evidence which
-- holds further money credits on each affected binding.
CREATE VIEW loyal_yield.referral_attribution_conflicts AS
WITH reassigned AS (
    SELECT DISTINCT i.privy_subject
    FROM loyal_yield.referral_wallet_identities i
    JOIN (SELECT wallet FROM loyal_yield.referral_wallet_identities
          GROUP BY wallet HAVING count(DISTINCT privy_subject) > 1) w USING (wallet)
), referees AS (
    SELECT DISTINCT b.referee_app_user_id, i.privy_subject
    FROM loyal_yield.referral_bindings b
    JOIN loyal_yield.referral_wallet_identities i ON i.wallet = b.referee_wallet
), inviters AS (
    SELECT DISTINCT b.referee_app_user_id, i.privy_subject
    FROM loyal_yield.referral_bindings b
    JOIN loyal_yield.referral_wallet_identities i ON i.app_user_id = b.referrer_app_user_id
)
SELECT referee_app_user_id, 'multiple_bindings'::text AS reason
FROM referees JOIN (SELECT privy_subject FROM referees GROUP BY privy_subject HAVING count(*) > 1) d USING (privy_subject)
UNION
SELECT referee_app_user_id, 'self_referral'
FROM referees JOIN inviters USING (referee_app_user_id, privy_subject)
UNION
SELECT referee_app_user_id, 'wallet_reassigned' FROM (
    SELECT * FROM referees UNION SELECT * FROM inviters
) parties JOIN reassigned USING (privy_subject);
