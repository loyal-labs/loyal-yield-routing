use loyal_yield_store::{
    AutodepositChainObservation, BalanceSweepTargetId, NeonSqlClient, NeonSqlConfig,
};
const DATABASE_URL_ENV: &str = "ASK_2211_VERIFY_DATABASE_URL";
const MIGRATION_0069: &str = include_str!("../migrations/0069_autodeposit_event_id_ranges.sql");
const APP_EVENT_ID_MIN: i64 = -999_999_999_999;
const FLOOR_EVENT_ID_MIN: i64 = -1_999_999_999_999;
const FLOOR_EVENT_ID_MAX: i64 = -1_000_000_000_000;
const ACTIVATION_EVENT_ID_MIN: i64 = -2_999_999_999_999;
const ACTIVATION_EVENT_ID_MAX: i64 = -2_000_000_000_000;

#[tokio::test]
#[ignore = "requires ASK_2211_VERIFY_DATABASE_URL pointing at a throwaway database"]
async fn synthetic_event_ranges_do_not_collide_and_activation_retries_immediately() {
    let database_url = std::env::var(DATABASE_URL_ENV).expect("throwaway database URL");
    let client = NeonSqlClient::connect(NeonSqlConfig::new(database_url))
        .await
        .expect("connect to throwaway database");

    let legacy_target_id = insert_target(&client, "legacy-activation-owner", 68).await;
    assert_eq!(legacy_target_id, 1);
    sqlx::query(
        r#"
        INSERT INTO loyal_yield.balance_sweep_wallet_balance_events
            (event_id, target_id, wallet, wallet_usdc_ata, wallet_token_ata,
             mint, previous_amount_raw, amount_raw, delta_amount_raw,
             observed_slot, observed_at, source, source_commitment,
             raw_evidence, projected_at)
        VALUES
            (-1,$1,'wallet-legacy-activation-owner','wallet-ata-legacy-activation-owner',
             'wallet-ata-legacy-activation-owner','mint-legacy-activation-owner',NULL,3000000,NULL,
             680,now(),'laserstream_autodeposit_activation','finalized','{}'::jsonb,now()),
            (-2,$1,'wallet-legacy-activation-owner','wallet-ata-legacy-activation-owner',
             'wallet-ata-legacy-activation-owner','mint-legacy-activation-owner',NULL,3000000,NULL,
             681,now(),'laserstream_autodeposit_activation','finalized','{}'::jsonb,now())
        "#,
    )
    .bind(legacy_target_id)
    .execute(client.pool())
    .await
    .expect("seed retained activation events in the App range");
    sqlx::query(
        r#"
        INSERT INTO loyal_yield.balance_sweep_surplus_lots
            (target_id, source_event_id, original_amount_raw, remaining_amount_raw,
             classification, eligible_after, status, confidence, reason)
        VALUES
            ($1,-2,1000000,1000000,'initial_surplus',now() + interval '1 hour',
             'open','confirmed_snapshot','retained activation event fixture')
        "#,
    )
    .bind(legacy_target_id)
    .execute(client.pool())
    .await
    .expect("seed lot linked to a retained activation event");

    sqlx::query(
        "SELECT setval('loyal_yield.balance_sweep_floor_rebaseline_event_id_seq', -1000000004008, true)",
    )
    .execute(client.pool())
    .await
    .expect("seed the production-like floor sequence position");

    sqlx::raw_sql(MIGRATION_0069)
        .execute(client.pool())
        .await
        .expect("relocate retained activation events");

    let retained_activation_count: i64 = sqlx::query_scalar(
        r#"
        SELECT count(*)
        FROM loyal_yield.balance_sweep_wallet_balance_events
        WHERE source = 'laserstream_autodeposit_activation'
          AND event_id NOT BETWEEN $1 AND $2
        "#,
    )
    .bind(ACTIVATION_EVENT_ID_MIN)
    .bind(ACTIVATION_EVENT_ID_MAX)
    .fetch_one(client.pool())
    .await
    .expect("count activation events outside their reserved range");
    assert_eq!(retained_activation_count, 0);

    let migrated_lot_event_id: i64 = sqlx::query_scalar(
        r#"
        SELECT lot.source_event_id
        FROM loyal_yield.balance_sweep_surplus_lots AS lot
        JOIN loyal_yield.balance_sweep_wallet_balance_events AS event
          ON event.event_id = lot.source_event_id
        WHERE lot.target_id = $1
          AND lot.reason = 'retained activation event fixture'
          AND event.source = 'laserstream_autodeposit_activation'
        "#,
    )
    .bind(legacy_target_id)
    .fetch_one(client.pool())
    .await
    .expect("load migrated activation lot reference");
    assert!((ACTIVATION_EVENT_ID_MIN..=ACTIVATION_EVENT_ID_MAX).contains(&migrated_lot_event_id));

    // This is the cross-repository contract: target 2 maps to App event -2,
    // which is free after migration even though routing previously retained -2.
    let target_id = insert_target(&client, "event-ranges", 69).await;
    assert_eq!(target_id, 2);
    let app_event_id = -target_id;
    assert!((APP_EVENT_ID_MIN..=-1).contains(&app_event_id));
    sqlx::query(
        r#"
        INSERT INTO loyal_yield.balance_sweep_wallet_balance_events
            (event_id, target_id, wallet, wallet_usdc_ata, wallet_token_ata,
             mint, previous_amount_raw, amount_raw, delta_amount_raw,
             observed_slot, observed_at, source, source_commitment,
             raw_evidence, projected_at)
        VALUES
            ($1,$2,'wallet-event-ranges','wallet-ata-event-ranges',
             'wallet-ata-event-ranges','mint-event-ranges',NULL,3000000,NULL,
             690,now(),'app_autodeposit_setup_confirm','finalized',
             '{"bootstrap":true}'::jsonb,now())
        "#,
    )
    .bind(app_event_id)
    .bind(target_id)
    .execute(client.pool())
    .await
    .expect("insert App target-derived event after legacy activation relocation");

    // Occupy the next activation sequence value and verify reconciliation
    // allocates another ID immediately in the same transaction.
    sqlx::query("SELECT setval('loyal_yield.autodeposit_bootstrap_event_id_seq', $1, false)")
        .bind(ACTIVATION_EVENT_ID_MAX - 2)
        .execute(client.pool())
        .await
        .expect("position activation sequence at collision fixture");
    sqlx::query(
        r#"
        INSERT INTO loyal_yield.balance_sweep_wallet_balance_events
            (event_id, target_id, wallet, wallet_usdc_ata, wallet_token_ata,
             mint, previous_amount_raw, amount_raw, delta_amount_raw,
             observed_slot, observed_at, source, source_commitment,
             raw_evidence, projected_at)
        VALUES
            ($1,$2,'wallet-legacy-activation-owner','wallet-ata-legacy-activation-owner',
             'wallet-ata-legacy-activation-owner','mint-legacy-activation-owner',NULL,3000000,NULL,
             682,now(),'laserstream_autodeposit_activation','finalized','{}'::jsonb,now())
        "#,
    )
    .bind(ACTIVATION_EVENT_ID_MAX - 2)
    .bind(legacy_target_id)
    .execute(client.pool())
    .await
    .expect("occupy the next activation ID");

    let result = client
        .reconcile_autodeposit_chain_observation(AutodepositChainObservation {
            target_id: BalanceSweepTargetId(target_id),
            observation_slot: 691,
            observation_complete: true,
            policy_valid: true,
            subscription_authority_valid: true,
            recurring_delegation_valid: true,
            token_delegate_valid: true,
            wallet_balance_raw: 3_000_000,
        })
        .await
        .expect("allocate the next activation ID in the same reconciliation");
    assert_eq!(result.bootstrap_generation, Some(1));

    let activation_event_id: i64 = sqlx::query_scalar(
        r#"
        SELECT event_id
        FROM loyal_yield.balance_sweep_wallet_balance_events
        WHERE target_id = $1
          AND source = 'laserstream_autodeposit_activation'
        "#,
    )
    .bind(target_id)
    .fetch_one(client.pool())
    .await
    .expect("load activation event");
    assert_eq!(activation_event_id, ACTIVATION_EVENT_ID_MAX - 3);
    assert!((ACTIVATION_EVENT_ID_MIN..=ACTIVATION_EVENT_ID_MAX).contains(&activation_event_id));

    let lot_source_event_id: i64 = sqlx::query_scalar(
        r#"
        SELECT source_event_id
        FROM loyal_yield.balance_sweep_surplus_lots
        WHERE target_id = $1
          AND classification = 'initial_surplus'
        "#,
    )
    .bind(target_id)
    .fetch_one(client.pool())
    .await
    .expect("load activation surplus lot");
    assert_eq!(lot_source_event_id, activation_event_id);

    let sequence_ranges: Vec<(String, i64, i64)> = sqlx::query_as(
        r#"
        SELECT sequencename, min_value, max_value
        FROM pg_sequences
        WHERE schemaname = 'loyal_yield'
          AND sequencename IN (
              'autodeposit_bootstrap_event_id_seq',
              'balance_sweep_floor_rebaseline_event_id_seq'
          )
        ORDER BY sequencename
        "#,
    )
    .fetch_all(client.pool())
    .await
    .expect("load synthetic event sequence ranges");
    assert_eq!(
        sequence_ranges,
        vec![
            (
                "autodeposit_bootstrap_event_id_seq".to_owned(),
                ACTIVATION_EVENT_ID_MIN,
                ACTIVATION_EVENT_ID_MAX,
            ),
            (
                "balance_sweep_floor_rebaseline_event_id_seq".to_owned(),
                FLOOR_EVENT_ID_MIN,
                FLOOR_EVENT_ID_MAX,
            ),
        ]
    );
    assert!(ACTIVATION_EVENT_ID_MAX < FLOOR_EVENT_ID_MIN);
    assert!(FLOOR_EVENT_ID_MAX < APP_EVENT_ID_MIN);

    let floor_event_id: i64 = sqlx::query_scalar(
        "SELECT nextval('loyal_yield.balance_sweep_floor_rebaseline_event_id_seq')",
    )
    .fetch_one(client.pool())
    .await
    .expect("allocate floor-rebaseline event ID");
    assert_eq!(floor_event_id, -1_000_000_004_009);
    assert!((FLOOR_EVENT_ID_MIN..=FLOOR_EVENT_ID_MAX).contains(&floor_event_id));
}

async fn insert_target(client: &NeonSqlClient, suffix: &str, policy_seed: i64) -> i64 {
    sqlx::query_scalar(
        r#"
        INSERT INTO loyal_yield.balance_sweep_targets
            (settings, authority, policy_seed, policy_account, vault_index,
             vault_pubkey, wallet, wallet_usdc_ata, vault_usdc_ata, token_mint,
             wallet_token_ata, vault_token_ata, delegated_signers, threshold,
             max_amount_per_period, desired_active, chain_status,
             chain_observation_slot, wallet_balance_floor_raw, last_seen_slot,
             last_seen_signature, cluster)
        VALUES
            ('settings-' || $1, 'wallet-' || $1, $2,
             'policy-' || $1, 1, 'vault-' || $1,
             'wallet-' || $1, 'wallet-ata-' || $1,
             'vault-ata-' || $1, 'mint-' || $1,
             'wallet-ata-' || $1, 'vault-ata-' || $1,
             ARRAY['signer-' || $1], 1, 10000000, TRUE, 'pending',
             690, 1000000, 690, 'policy-signature-' || $1,
             'mainnet-beta')
        RETURNING id
        "#,
    )
    .bind(suffix)
    .bind(policy_seed)
    .fetch_one(client.pool())
    .await
    .expect("insert Autodeposit target")
}
