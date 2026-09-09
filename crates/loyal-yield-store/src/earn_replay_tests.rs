//! Real PostgreSQL invariants; only run through verify-earn-replay-repair.sh.
use super::*;

fn policy(slot: u64) -> PolicyMatchInput {
    PolicyMatchInput {
        signature: format!("policy-{slot}"),
        slot,
        cluster: "mainnet".into(),
        source_commitment: "finalized".into(),
        settings: "replay-settings".into(),
        authority: "replay-wallet".into(),
        policy_seed: slot,
        policy_account: format!("replay-policy-{slot}"),
        vault_index: 1,
        vault_pubkey: "replay-vault".into(),
        delegated_signers: vec!["replay-wallet".into()],
        threshold: 1,
        route_modes: vec!["kamino_deposit".into()],
        stable_mints: vec!["mint".into()],
        kamino_markets: vec!["market".into()],
        kamino_liquidity_mints: vec!["mint".into()],
        universe_preset: None,
        risk_profile: None,
        swap_lanes: json!([]),
    }
}

fn deposit(slot: u64, amount: u64, route: PolicyMatchInput) -> EarnDirectMutation {
    EarnDirectMutation::Deposit(EarnDepositMutation {
        route_policy: route,
        setup_policy: None,
        deposit_signature: format!("deposit-{slot}"),
        deposit_slot: slot,
        observed_slot: slot,
        deposit_mint: "mint".into(),
        principal_amount_raw: amount,
        target_reserve: "reserve".into(),
        market: Some("market".into()),
        liquidity_mint: "mint".into(),
        target_supply_apy_bps: None,
        wallet: "replay-wallet".into(),
        smart_account_address: "replay-vault".into(),
        reserve_state: vec![EarnReserveMutation {
            reserve: "reserve".into(),
            market: Some("market".into()),
            liquidity_mint: "mint".into(),
            amount_raw: amount,
            has_value: true,
            supply_apy_bps: None,
            borrow_apy_bps: None,
            planning_metadata: json!({}),
        }],
        idle_state: vec![],
        observed_at: None,
    })
}

async fn complete(
    store: &OrchestratorStore,
    key: &str,
    slot: i64,
    mutation: &EarnDirectMutation,
) -> Result<EarnReconciliationCompletionOutcome, OrchestratorError> {
    let id: i64 = sqlx::query_scalar(
        "INSERT INTO loyal_yield.earn_reconciliation_jobs (consumer_name,event_key,durable_slot,settings,vault_index,vault_pubkey,event_payload,vault_payload,claim_owner,claim_expires_at) VALUES ('replay-test',$1,$2,'replay-settings',1,'replay-vault','{}','{}','test',now()+interval '1 minute') RETURNING id",
    ).bind(key).bind(slot).fetch_one(store.pool()).await?;
    store
        .complete_earn_reconciliation_job(id, "test", mutation)
        .await
}

#[tokio::test]
#[ignore = "requires disposable PostgreSQL from scripts/verify-earn-replay-repair.sh"]
async fn refund_cleanup_and_historical_withdrawal_replay() {
    let url = std::env::var("EARN_REPLAY_TEST_DATABASE_URL").expect("isolated DB required");
    let pool = PgPoolOptions::new()
        .max_connections(2)
        .connect(&url)
        .await
        .unwrap();
    let db: String = sqlx::query_scalar("SELECT current_database()")
        .fetch_one(&pool)
        .await
        .unwrap();
    assert_eq!(
        db, "earn_replay_verify",
        "never run against a product database"
    );
    let store = OrchestratorStore::from_pool(pool);
    complete(
        &store,
        "deposit-old",
        100,
        &deposit(100, 5_000_000, policy(90)),
    )
    .await
    .unwrap();
    let mut refund = EarnRefundMutation {
        cluster: "mainnet".into(),
        full_cleanup: false,
        settings: "replay-settings".into(),
        vault_index: 1,
        vault_pubkey: "replay-vault".into(),
        wallet: "replay-wallet".into(),
        refund_signature: "refund-old".into(),
        confirmed_slot: 120,
        refund_kind: "vault_account".into(),
        observed_at: None,
    };
    complete(
        &store,
        "refund-only",
        120,
        &EarnDirectMutation::Refund(refund.clone()),
    )
    .await
    .unwrap();
    refund.full_cleanup = true;
    assert_eq!(
        complete(
            &store,
            "repair-refund",
            120,
            &EarnDirectMutation::Refund(refund.clone())
        )
        .await
        .unwrap()
        .applied_mutations,
        1
    );
    assert_eq!(
        complete(
            &store,
            "repair-refund-twice",
            120,
            &EarnDirectMutation::Refund(refund)
        )
        .await
        .unwrap()
        .applied_mutations,
        0
    );
    let closed: i64 = sqlx::query_scalar("SELECT count(*) FROM loyal_yield.user_yield_positions WHERE status::text='closed' AND current_amount_raw=0 AND principal_amount_raw=0").fetch_one(store.pool()).await.unwrap();
    assert_eq!(closed, 1);
    let refund_rows: i64 = sqlx::query_scalar("SELECT count(*) FROM loyal_yield.earn_chain_refund_events WHERE refund_signature='refund-old'").fetch_one(store.pool()).await.unwrap();
    assert_eq!(refund_rows, 1);
    let cleanup_rows: i64 = sqlx::query_scalar("SELECT count(*) FROM loyal_yield.earn_chain_mutations WHERE chain_signature='refund-old' AND mutation_kind='cleanup'").fetch_one(store.pool()).await.unwrap();
    assert_eq!(cleanup_rows, 1);

    complete(
        &store,
        "deposit-new",
        200,
        &deposit(200, 7_000_000, policy(190)),
    )
    .await
    .unwrap();
    let withdrawal = EarnDirectMutation::Withdrawal(EarnWithdrawalMutation {
        route_policy: policy(90),
        withdrawal_signature: "withdraw-old".into(),
        confirmed_slot: 110,
        // A historical RPC proof is read today, after the new deposit.
        observed_slot: 210,
        wallet: "replay-wallet".into(),
        vault_pubkey: "replay-vault".into(),
        target_reserve: "reserve".into(),
        market: Some("market".into()),
        liquidity_mint: "mint".into(),
        withdrawn_amount_raw: 5_000_000,
        remaining_amount_raw: 0,
        reserve_state: vec![],
        idle_state: vec![],
        observed_at: None,
    });
    complete(&store, "backfill-withdraw", 110, &withdrawal)
        .await
        .unwrap();
    assert_eq!(
        complete(&store, "backfill-withdraw-twice", 110, &withdrawal)
            .await
            .unwrap()
            .applied_mutations,
        0
    );
    let old_close = EarnDirectMutation::Cleanup(EarnCleanupMutation {
        settings: "replay-settings".into(),
        vault_index: 1,
        vault_pubkey: "replay-vault".into(),
        cleanup_signature: "late-old-close".into(),
        confirmed_slot: 125,
        observed_at: None,
    });
    complete(&store, "old-close-after-new-deposit", 125, &old_close)
        .await
        .unwrap();
    let current: (i64,i64,String) = sqlx::query_as("SELECT principal_amount_raw,current_amount_raw,status::text FROM loyal_yield.user_yield_positions WHERE last_deposit_signature='deposit-200'").fetch_one(store.pool()).await.unwrap();
    assert_eq!(current, (7_000_000, 7_000_000, "active".into()));
    let policy_active: bool = sqlx::query_scalar(
        "SELECT active FROM loyal_yield.route_policies WHERE policy_account='replay-policy-190'",
    )
    .fetch_one(store.pool())
    .await
    .unwrap();
    assert!(policy_active);
    let old_policy_active: bool = sqlx::query_scalar(
        "SELECT active FROM loyal_yield.route_policies WHERE policy_account='replay-policy-90'",
    )
    .fetch_one(store.pool())
    .await
    .unwrap();
    assert!(!old_policy_active);
    let recorded_policy: String = sqlx::query_scalar("SELECT policy_account FROM loyal_yield.user_yield_position_withdrawals WHERE withdrawal_signature='withdraw-old'").fetch_one(store.pool()).await.unwrap();
    assert_eq!(recorded_policy, "replay-policy-90");

    let EarnDirectMutation::Withdrawal(mut unknown) = withdrawal else {
        unreachable!()
    };
    // No owning deposit, an unproven later lifecycle, and same-slot ordering
    // ambiguity must all fail without marking the repair complete.
    for slot in [80_u64, 150, 200] {
        unknown.confirmed_slot = slot;
        unknown.withdrawal_signature = format!("unproven-withdraw-{slot}");
        assert!(complete(
            &store,
            &format!("unknown-lifecycle-{slot}"),
            slot as i64,
            &EarnDirectMutation::Withdrawal(unknown.clone())
        )
        .await
        .is_err());
        let leaked: i64 = sqlx::query_scalar(
            "SELECT count(*) FROM loyal_yield.earn_chain_mutations WHERE chain_signature=$1",
        )
        .bind(&unknown.withdrawal_signature)
        .fetch_one(store.pool())
        .await
        .unwrap();
        assert_eq!(
            leaked, 0,
            "rejected repair must roll back its completion marker"
        );
    }

    let EarnDirectMutation::Deposit(current) = deposit(220, 5_000_000, policy(190)) else {
        unreachable!()
    };
    let partial = EarnDirectMutation::Withdrawal(EarnWithdrawalMutation {
        route_policy: policy(190),
        withdrawal_signature: "withdraw-current".into(),
        confirmed_slot: 220,
        observed_slot: 220,
        wallet: "replay-wallet".into(),
        vault_pubkey: "replay-vault".into(),
        target_reserve: "reserve".into(),
        market: Some("market".into()),
        liquidity_mint: "mint".into(),
        withdrawn_amount_raw: 2_000_000,
        remaining_amount_raw: 5_000_000,
        reserve_state: current.reserve_state,
        idle_state: vec![],
        observed_at: None,
    });
    complete(&store, "current-partial", 220, &partial)
        .await
        .unwrap();
    complete(&store, "current-partial-twice", 220, &partial)
        .await
        .unwrap();
    let current: (i64, i64) = sqlx::query_as("SELECT principal_amount_raw,current_amount_raw FROM loyal_yield.user_yield_positions WHERE last_deposit_signature='deposit-200'").fetch_one(store.pool()).await.unwrap();
    assert_eq!(current, (5_000_000, 5_000_000));
}
