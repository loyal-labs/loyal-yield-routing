CREATE TABLE loyal_yield.referral_codes (
    app_user_id uuid PRIMARY KEY,
    code text NOT NULL UNIQUE CHECK (code ~ '^[0-9A-HJKMNP-TV-Z]{7}$'),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE TABLE loyal_yield.referral_bindings (
    referee_app_user_id uuid PRIMARY KEY,
    referee_wallet text NOT NULL UNIQUE,
    referrer_app_user_id uuid NOT NULL,
    source text NOT NULL CHECK (source IN ('link', 'typed', 'install_referrer')),
    referee_signed_up_at timestamptz NOT NULL,
    bound_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    CHECK (referee_app_user_id <> referrer_app_user_id)
);

CREATE INDEX referral_bindings_referrer ON loyal_yield.referral_bindings (referrer_app_user_id);
