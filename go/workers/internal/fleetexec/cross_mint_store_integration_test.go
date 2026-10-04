package fleetexec

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/gagliardetto/solana-go"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
)

// SQL scenarios use the existing integrationStore URL guard and registered
// schema. They exercise database custody/fencing, not chain execution proof.
func seedCrossMintMovement(t *testing.T, ctx context.Context, pool *pgxpool.Pool) seededBaseline {
	t.Helper()
	b := seedBaseline(t, ctx, pool, fmt.Sprint(time.Now().UnixNano()))
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `UPDATE loyal_yield.rebalance_opportunities SET source_reserve='source',source_liquidity_mint='source-mint',target_liquidity_mint=liquidity_mint,estimated_cost_lamports=50000,execution_plan='{"kind":"cross_mint_jupiter","route_kind":"cross_mint_jupiter","source_kind":"reserve_position"}' WHERE id=$1`, b.OpportunityID); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `UPDATE loyal_yield.rebalance_decisions SET status='confirming',source_reserve='source',target_reserve=$2,active_target_reserve=$2,source_liquidity_mint='source-mint',target_liquidity_mint=$3,amount_raw=1000,custody_mint='source-mint',custody_amount_raw=1000,custody_account='source',cross_mint_activation_control_generation=1,cross_mint_preflight_certification='{"fixture":"sql-only"}',continuation_available_at=clock_timestamp() WHERE id=$1`, b.DecisionID, b.Reserve, b.Mint); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO loyal_yield.cross_mint_movement_controls(cluster,start_new_movements,continue_or_recover_existing,generation) VALUES($1,true,true,1)`, b.Cluster); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return b
}

func seedCrossMintActivation(t *testing.T, ctx context.Context, pool *pgxpool.Pool) (fleet.ExecutionAdmission, DelegateSigner, CrossMintActivation) {
	return seedCrossMintActivationWithControlGeneration(t, ctx, pool, 1)
}

func seedCrossMintActivationWithControlGeneration(t *testing.T, ctx context.Context, pool *pgxpool.Pool, controlGeneration int64) (fleet.ExecutionAdmission, DelegateSigner, CrossMintActivation) {
	t.Helper()
	a, signer := seedFresh(t, ctx, pool)
	l := &a.Lease
	var settings, authority, policy string
	if err := pool.QueryRow(ctx, `SELECT v.settings,p.authority,p.policy_account FROM loyal_yield.managed_vaults v JOIN loyal_yield.route_policies p ON p.id=v.active_policy_id WHERE v.id=$1`, l.VaultID).Scan(&settings, &authority, &policy); err != nil {
		t.Fatal(err)
	}
	// Baseline string labels are sufficient for old SQL-only tests, but source
	// certificate identities must be actual public keys even in this fixture.
	settings = sdk.PublicKey(sha256.Sum256([]byte(settings))).String()
	policy = sdk.PublicKey(sha256.Sum256([]byte(policy))).String()
	l.SourceLiquidityMint, l.TargetLiquidityMint, l.LiquidityMint = fleet.USDCMint, fleet.USDTMint, fleet.USDTMint
	l.LiquidityAmountRaw = 1_000_000_000
	l.PrincipalUSDMicros = 1_000_000_000
	l.SourceAPYBPS = 100
	l.TargetAPYBPS = 900
	l.FeeCapLamports = 50000
	derive := func(seed uint64) string {
		var raw [8]byte
		binary.LittleEndian.PutUint64(raw[:], seed)
		key := sdk.MustPublicKeyFromBase58(settings)
		account, _, err := sdk.FindProgramAddress([][]byte{[]byte("smart_account"), []byte("policy"), key[:], raw[:]}, sdk.MustPublicKeyFromBase58(fleet.SquadsProgram))
		if err != nil {
			t.Fatal(err)
		}
		return account.String()
	}
	swap, sibling := derive(1), derive(2)
	manifest := strings.Repeat("a", 64)
	bindings := fleet.CrossMintPolicyBindings{Settings: settings, VaultPubkey: l.VaultPubkey, DelegatedSigner: signerPublic(signer), Withdraw: fleet.CrossMintEarnPolicyBinding{PolicyAccount: policy, ObservedSlot: 999, SourceCommitment: "finalized", ConstraintIndex: 0}, Deposit: fleet.CrossMintEarnPolicyBinding{PolicyAccount: policy, ObservedSlot: 999, SourceCommitment: "finalized", ConstraintIndex: 1}, Swap: fleet.CrossMintSwapPolicyBinding{PolicyAccount: swap, ObservedSlot: 999, SourceCommitment: "finalized", SourceShard: "classic", EnrollmentGeneration: 1, MaxSlippageBPS: 10, DailySourceMintSpendingCap: 10_000_000_000, ManifestFingerprint: manifest}}
	plan, _ := json.Marshal(map[string]any{"kind": "cross_mint_jupiter", "route_kind": "cross_mint_jupiter", "source_kind": "reserve_position", "policy_bindings": bindings, "source_liquidity_mint": l.SourceLiquidityMint, "target_liquidity_mint": l.TargetLiquidityMint, "amount_raw": l.LiquidityAmountRaw, "redeemable_source_liquidity_amount_raw": l.LiquidityAmountRaw, "cross_mint_maximum_value_loss_bps": 10, "observed_target_apy_bps": 900, "source_apy_bps": 100, "confidence_ppm": 1000000, "holding_horizon_seconds": 2592000, "estimated_execution_cost_usd_micros": 0})
	l.ExecutionPlan = plan
	a.Preparation.ExecutionPlan = plan
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `UPDATE loyal_yield.managed_vaults SET settings=$2 WHERE id=$1`, l.VaultID, settings); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `DELETE FROM loyal_yield.target_capacity_reservations WHERE opportunity_id=$1 AND decision_id IS NULL AND signed_submission_id IS NULL`, l.OpportunityID); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `UPDATE loyal_yield.target_capacity_frontiers SET liquidity_mint=$2,observed_supply_usd_micros=1000000000000,maximum_inflight_usd_micros=20000000000 WHERE cluster=$1`, l.Cluster, l.TargetLiquidityMint); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `UPDATE loyal_yield.route_policies SET cluster=$2,source_commitment='finalized',finalized_eligible=true,stable_mints=ARRAY[$3,$4]::text[],kamino_liquidity_mints=ARRAY[$3,$4]::text[],settings=$5,policy_account=$6 WHERE id=(SELECT active_policy_id FROM loyal_yield.managed_vaults WHERE id=$1)`, l.VaultID, l.Cluster, l.SourceLiquidityMint, l.TargetLiquidityMint, settings, policy); err != nil {
		t.Fatal(err)
	}
	for i, shard := range []string{"classic", "token_2022"} {
		account := swap
		if i == 1 {
			account = sibling
		}
		if _, err = tx.Exec(ctx, `INSERT INTO loyal_yield.cross_mint_swap_policies(cluster,settings,authority,policy_account,vault_index,vault_pubkey,delegated_signer,source_shard,max_slippage_bps,daily_source_mint_spending_cap,manifest_fingerprint,start_eligible,last_mutation,source_commitment,last_seen_slot,last_seen_signature) VALUES($1,$2,$3,$4,0,$5,$6,$7,10,10000000000,$8,true,'create','finalized',999,'fixture-signature')`, l.Cluster, settings, authority, account, l.VaultPubkey, bindings.DelegatedSigner, shard, manifest); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = tx.Exec(ctx, `INSERT INTO loyal_yield.cross_mint_vault_opt_ins(cluster,settings,vault_index,vault_pubkey,enabled,classic_policy_account,classic_policy_seed,token_2022_policy_account,token_2022_policy_seed,max_slippage_bps,daily_source_mint_spending_cap) VALUES($1,$2,0,$3,true,$4,1,$5,2,10,10000000000)`, l.Cluster, settings, l.VaultPubkey, swap, sibling); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO loyal_yield.cross_mint_movement_controls(cluster,start_new_movements,continue_or_recover_existing,generation) VALUES($1,true,true,$2)`, l.Cluster, controlGeneration); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `UPDATE loyal_yield.rebalance_opportunities SET source_liquidity_mint=$2,target_liquidity_mint=$3,liquidity_mint=$3,amount_raw=$4,principal_usd_micros=$4,source_apy_bps=100,target_apy_bps=900,estimated_edge_bps=800,estimated_cost_lamports=50000,execution_plan=$5 WHERE id=$1`, l.OpportunityID, l.SourceLiquidityMint, l.TargetLiquidityMint, int64(l.LiquidityAmountRaw), plan); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var projection CrossMintCapacityProjection
	projection.Cluster, projection.TargetReserve, projection.LiquidityMint = l.Cluster, l.TargetReserve, l.TargetLiquidityMint
	if err = pool.QueryRow(ctx, `SELECT observed_supply_usd_micros,observed_slot,maximum_inflight_usd_micros,telemetry_version FROM loyal_yield.target_capacity_frontiers WHERE cluster=$1 AND target_reserve=$2 AND liquidity_mint=$3`, projection.Cluster, projection.TargetReserve, projection.LiquidityMint).Scan(&projection.ObservedSupplyUSDMicros, &projection.ObservedSlot, &projection.MaximumInflightUSDMicros, &projection.TelemetryVersion); err != nil {
		t.Fatal(err)
	}
	// The structurally complete certificate is deliberately fixture evidence.
	// Hashes and simulation fields are not represented as chain verification.
	policyProof := fleet.CrossMintCertificatePolicy{PolicyAccount: policy, ContextSlot: 999, DataSHA256: strings.Repeat("b", 64)}
	certificate := fleet.CrossMintPreflightCertificate{Kind: "cross_mint_preflight", CertifiedAt: time.Now(), Cluster: l.Cluster, SourceMint: l.SourceLiquidityMint, TargetMint: l.TargetLiquidityMint, InputAmountRaw: "1000000000", MinimumOutputAmountRaw: "999000000", EffectiveSlippageBPS: 10, EffectiveMaximumValueLossBPS: 10,
		FinalizedPolicyReadbacks: fleet.CrossMintCertificatePolicies{Withdraw: policyProof, Deposit: policyProof, Swap: fleet.CrossMintCertificateSwapPolicy{CrossMintCertificatePolicy: fleet.CrossMintCertificatePolicy{PolicyAccount: swap, ContextSlot: 999, DataSHA256: strings.Repeat("c", 64)}, PolicySeed: "1", SourceShard: "classic", ManifestFingerprint: manifest, Dialect: "route_v2", ConstraintIndex: 0, DailySourceMintSpendingCap: "10000000000"}},
		JupiterBuild:             fleet.CrossMintCertificateJupiter{ResponseSHA256: strings.Repeat("d", 64), MessageSHA256: strings.Repeat("e", 64), RouteStepCount: 1, QuotedOutputAmountRaw: "1000000000", ComputeUnitLimit: 200000, PacketSizeBytes: 500, PacketDataSizeBytes: 1232, FitsPacketDataSize: true, LastValidBlockHeight: 1000, ObservedBlockHeight: 999, InputPreBalanceRaw: "50", OutputPreBalanceRaw: "0", SimulationAttempted: true, SimulationUnits: 100000, SimulationTopology: "withdraw_then_swap_atomic_preflight_only", SimulationLookupTables: []string{signerPublic(signer)}, TargetDepositPolicyValidated: true, TargetReserve: l.TargetReserve, TargetObligation: signerPublic(signer)},
	}
	certJSON, err := json.Marshal(certificate)
	if err != nil {
		t.Fatal(err)
	}
	return a, signer, CrossMintActivation{Capacity: projection, InitialWithdrawCompiledFeeLamports: 5000, PreflightCertification: certJSON, SourceControlGeneration: controlGeneration}
}

func signerPublic(s DelegateSigner) string { return sdk.PublicKeyFromBytes(s.FeePayer[32:]).String() }

func TestCrossMintActivationAndSignedPublicationRetainExistingFences(t *testing.T) {
	store, pool := integrationStore(t)
	ctx := context.Background()
	if err := store.RequireCrossMintSchema(ctx); err != nil {
		t.Fatal(err)
	}
	a, signer, input := seedCrossMintActivation(t, ctx, pool)
	// The synthetic message protects exact-wire/database publication contracts.
	// It is not a KLend/Jupiter chain execution or a preflight simulation proof.
	badCertificate := input
	badCertificate.PreflightCertification = json.RawMessage(`{"fixture":"old-generic-object"}`)
	if _, err := store.ActivateCrossMintMovement(ctx, a.Lease, badCertificate); err == nil {
		t.Fatal("activation accepted an incomplete certificate")
	}
	var premature int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM loyal_yield.target_capacity_reservations WHERE opportunity_id=$1`, a.Lease.OpportunityID).Scan(&premature); err != nil || premature != 0 {
		t.Fatalf("failed certification reserved capacity: %d %v", premature, err)
	}
	m, err := store.ActivateCrossMintMovement(ctx, a.Lease, input)
	if err != nil {
		t.Fatal(err)
	}
	if m.Phase != CrossMintSourceReserve || m.CustodyVersion != 0 || m.CustodyAmountRaw != int64(a.Lease.LiquidityAmountRaw) {
		t.Fatalf("activation moved funds before withdrawal: %+v", m)
	}
	again, err := store.ActivateCrossMintMovement(ctx, a.Lease, input)
	if err != nil || again.DecisionID != m.DecisionID {
		t.Fatalf("activation duplicated movement: %+v %v", again, err)
	}
	l, err := store.ClaimCrossMintContinuation(ctx, m.Cluster, a.Lease.Owner, time.Minute)
	if err != nil || l == nil {
		t.Fatalf("activation crash not recoverable: %v", err)
	}
	r := CrossMintLegRequest{Movement: l.Movement, Leg: LegWithdraw, Purpose: PurposeOptimizeYield, Generation: 1, RemainingFeeLamports: 50000}
	b, err := crossMintBindings(m.ExecutionPlan)
	if err != nil {
		t.Fatal(err)
	}
	e := CrossMintExpectedEffect{CreditMint: crossString(m.SourceMint), CreditTokenAccount: crossString("source-ata"), MinimumCreditAmountRaw: crossInt(m.PlannedAmountRaw - 10)}
	anchors := CrossMintBalanceAnchors{Credit: &CrossMintTokenAmount{Mint: m.SourceMint, TokenAccount: "source-ata", AmountRaw: 50}, Position: &CrossMintPositionAnchor{Reserve: m.SourceReserve, Market: "market", Obligation: "source-obligation", ObligationExists: true, CollateralRaw: 1001}}
	p := CrossMintPreparedLeg{Preparation: a.Preparation, LastValidBlockHeight: a.LastValidBlockHeight, PolicyAccount: b.Withdraw.PolicyAccount, ExpectedEffect: e, BalanceAnchors: anchors, ConflictKeys: a.ConflictKeys, SelectedALTs: a.SelectedALTs, AltSelectionFingerprint: a.AltSelectionFingerprint}
	wire, err := signer.SignPreparedRoute(p.Preparation.Transaction, p.LastValidBlockHeight)
	if err != nil {
		t.Fatal(err)
	}
	old := *l
	old.FencingToken++
	if _, err = store.AppendCrossMintLeg(ctx, old, r, p, wire); !errors.Is(err, ErrStaleOwner) {
		t.Fatalf("stale continuation signed publication: %v", err)
	}
	id, err := store.AppendCrossMintLeg(ctx, *l, r, p, wire)
	if err != nil {
		t.Fatal(err)
	}
	var saved []byte
	var leg, required, reservation string
	var owner *string
	if err = pool.QueryRow(ctx, `SELECT s.signed_transaction,s.movement_leg,s.required_commitment,r.reservation_state,d.continuation_lease_owner FROM loyal_yield.signed_route_submissions s JOIN loyal_yield.rebalance_decisions d ON d.id=s.decision_id JOIN loyal_yield.target_capacity_reservations r ON r.decision_id=d.id WHERE s.id=$1`, id).Scan(&saved, &leg, &required, &reservation, &owner); err != nil {
		t.Fatal(err)
	}
	if string(saved) != string(wire.SignedTransaction) || leg != LegWithdraw || required != "finalized" || reservation != "active" || owner != nil {
		t.Fatal("signed movement publication lost bytes, finality or ownership")
	}
	if _, err = store.AppendCrossMintLeg(ctx, *l, r, p, wire); !errors.Is(err, ErrStaleOwner) {
		t.Fatalf("same continuation appended twice: %v", err)
	}
}

func TestCrossMintSignedAppendRejectsChangedAdmissionWithoutPartialPublication(t *testing.T) {
	for _, change := range []string{"fee", "wire", "alt_epoch", "policy", "control_generation", "foreign_conflict"} {
		t.Run(change, func(t *testing.T) {
			store, pool := integrationStore(t)
			ctx := context.Background()
			a, signer, input := seedCrossMintActivation(t, ctx, pool)
			m, err := store.ActivateCrossMintMovement(ctx, a.Lease, input)
			if err != nil {
				t.Fatal(err)
			}
			l, err := store.ClaimCrossMintContinuation(ctx, m.Cluster, a.Lease.Owner, time.Minute)
			if err != nil || l == nil {
				t.Fatalf("claim: %v", err)
			}
			b, err := crossMintBindings(m.ExecutionPlan)
			if err != nil {
				t.Fatal(err)
			}
			r := CrossMintLegRequest{Movement: l.Movement, Leg: LegWithdraw, Purpose: PurposeOptimizeYield, Generation: 1, RemainingFeeLamports: 50000}
			p := CrossMintPreparedLeg{Preparation: a.Preparation, LastValidBlockHeight: a.LastValidBlockHeight, PolicyAccount: b.Withdraw.PolicyAccount, ExpectedEffect: CrossMintExpectedEffect{CreditMint: crossString(m.SourceMint), CreditTokenAccount: crossString("source-ata"), MinimumCreditAmountRaw: crossInt(m.PlannedAmountRaw - 10)}, BalanceAnchors: CrossMintBalanceAnchors{Credit: &CrossMintTokenAmount{Mint: m.SourceMint, TokenAccount: "source-ata", AmountRaw: 50}, Position: &CrossMintPositionAnchor{Reserve: m.SourceReserve, Market: "market", Obligation: "obligation", ObligationExists: true, CollateralRaw: 1001}}, ConflictKeys: a.ConflictKeys, SelectedALTs: a.SelectedALTs, AltSelectionFingerprint: a.AltSelectionFingerprint}
			wire, err := signer.SignPreparedRoute(p.Preparation.Transaction, p.LastValidBlockHeight)
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "fee":
				p.Preparation.Transaction.FeeLamports = 50001
			case "wire":
				wire.SignedTransaction = append([]byte(nil), wire.SignedTransaction...)
				wire.SignedTransaction[len(wire.SignedTransaction)-1] ^= 1
			case "alt_epoch":
				_, err = pool.Exec(ctx, `UPDATE loyal_yield.route_lookup_tables SET mutation_epoch=mutation_epoch+1 WHERE id=$1`, a.SelectedALTs[0].TableID)
			case "policy":
				_, err = pool.Exec(ctx, `UPDATE loyal_yield.cross_mint_swap_policies SET start_eligible=false WHERE cluster=$1 AND policy_account=$2`, m.Cluster, b.Swap.PolicyAccount)
			case "control_generation":
				_, err = pool.Exec(ctx, `UPDATE loyal_yield.cross_mint_movement_controls SET generation=generation+1 WHERE cluster=$1`, m.Cluster)
			case "foreign_conflict":
				_, err = pool.Exec(ctx, `UPDATE loyal_yield.route_account_conflict_leases SET lease_owner='rust-owner',fencing_token=fencing_token+1 WHERE cluster=$1 AND writable_account_key=$2`, m.Cluster, a.ConflictKeys[0])
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err = store.AppendCrossMintLeg(ctx, *l, r, p, wire); err == nil {
				t.Fatalf("changed %s published", change)
			}
			var count, version int64
			var state string
			if err = pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM loyal_yield.signed_route_submissions WHERE decision_id=d.id),d.custody_version,r.reservation_state FROM loyal_yield.rebalance_decisions d JOIN loyal_yield.target_capacity_reservations r ON r.decision_id=d.id WHERE d.id=$1`, m.DecisionID).Scan(&count, &version, &state); err != nil {
				t.Fatal(err)
			}
			if count != 0 || version != 0 || state != "active" {
				t.Fatalf("partial failed append: signed=%d custody=%d capacity=%s", count, version, state)
			}
			if err = pool.QueryRow(ctx, `SELECT count(*) FROM loyal_yield.lookup_table_usage_leases WHERE reference_key=$1`, crossMintSemantic(m.DecisionID, LegWithdraw, 1)).Scan(&count); err != nil || count != 0 {
				t.Fatalf("failed append retained a new prepared ALT lease: %d %v", count, err)
			}
		})
	}
}

func seedCrossMintReceiptAttempt(t *testing.T, ctx context.Context, pool *pgxpool.Pool, b seededBaseline, leg, purpose string, e CrossMintExpectedEffect, pre CrossMintBalanceAnchors, slot int64) SubmissionLease {
	t.Helper()
	wire, _ := integrationWire(t, 123)
	var bound PersistRouteInput
	var attempts int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM loyal_yield.signed_route_submissions WHERE decision_id=$1`, b.DecisionID).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if attempts == 0 {
		bound = fixturePersistInput(t, ctx, pool, b, wire)
	} else {
		if err := pool.QueryRow(ctx, `SELECT alt_requirements_fingerprint,alt_selection_fingerprint,alt_mutation_epochs,fee_payer FROM loyal_yield.signed_route_submissions WHERE decision_id=$1 ORDER BY id LIMIT 1`, b.DecisionID).Scan(&bound.AltRequirementsFingerprint, &bound.AltSelectionFingerprint, &bound.AltMutationEpochs, &bound.FeePayer); err != nil {
			t.Fatal(err)
		}
	}
	effect, _ := json.Marshal(e)
	anchors, _ := json.Marshal(pre)
	var id int64
	err := pool.QueryRow(ctx, `INSERT INTO loyal_yield.signed_route_submissions(cluster,semantic_key,opportunity_id,decision_id,signed_transaction,signed_transaction_hash,message_hash,transaction_signature,recent_blockhash,last_valid_block_height,optimizer_epoch_id,alt_requirements_fingerprint,alt_selection_fingerprint,alt_mutation_epochs,fee_payer,fee_payer_kind,compiled_fee_lamports,writable_account_keys,conflict_account_keys,executor_owner,executor_fencing_token,movement_leg,leg_purpose,leg_generation,required_commitment,policy_account,expected_effect,expected_balance_anchors,submission_state,confirmed_slot,finalized_slot,finalized_at,confirmation_lease_owner,confirmation_fencing_token,confirmation_lease_expires_at)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$17,$18,$19,$20,'policy',5000,ARRAY[$20]::text[],ARRAY['vault-write:test','fleet-shared-write-lane:test'],'owner',$21,$12,$13,1,'finalized','policy',$14,$15,'reconciliation_pending',$16,$16,clock_timestamp(),'receipt-owner',1,clock_timestamp()+interval '1 minute') RETURNING id`, b.Cluster, crossMintSemantic(b.DecisionID, leg, 1), b.OpportunityID, b.DecisionID, wire.SignedTransaction, wire.SignedTransactionHash, wire.MessageHash, wire.TransactionSignature, wire.RecentBlockhash, wire.LastValidBlockHeight, b.EpochID, leg, purpose, effect, anchors, slot, bound.AltRequirementsFingerprint, bound.AltSelectionFingerprint, bound.AltMutationEpochs, bound.FeePayer, attempts+1).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return SubmissionLease{Submission: SubmissionRecord{ID: id, DecisionID: &b.DecisionID, OpportunityID: b.OpportunityID, MovementLeg: leg, LegPurpose: purpose, Signature: wire.TransactionSignature}, Owner: "receipt-owner", FencingToken: 1}
}

func TestCrossMintContinuationClaimsOneOwnerAndLeavesHoldingAttempt(t *testing.T) {
	store, pool := integrationStore(t)
	ctx := context.Background()
	b := seedCrossMintMovement(t, ctx, pool)
	var wg sync.WaitGroup
	leases := make(chan *CrossMintContinuationLease, 2)
	failures := make(chan error, 2)
	for _, owner := range []string{"rust-owner", "go-owner"} {
		wg.Add(1)
		go func(owner string) {
			defer wg.Done()
			l, err := store.ClaimCrossMintContinuation(ctx, b.Cluster, owner, time.Minute)
			leases <- l
			failures <- err
		}(owner)
	}
	wg.Wait()
	close(leases)
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	var winner *CrossMintContinuationLease
	for l := range leases {
		if l != nil {
			if winner != nil {
				t.Fatal("two live continuation owners")
			}
			winner = l
		}
	}
	if winner == nil {
		t.Fatal("neither writer claimed untouched movement")
	}
	if winner.Movement.Phase != CrossMintSourceReserve || winner.Movement.CustodyVersion != 0 {
		t.Fatal("crash-before-first-publication custody changed")
	}
	e, pre, _, _ := crossWithdrawContract()
	seedCrossMintReceiptAttempt(t, ctx, pool, b, LegWithdraw, PurposeOptimizeYield, e, pre, 10)
	if _, err := pool.Exec(ctx, `UPDATE loyal_yield.rebalance_decisions SET continuation_lease_owner=NULL,continuation_lease_expires_at=NULL WHERE id=$1`, b.DecisionID); err != nil {
		t.Fatal(err)
	}
	if l, err := store.ClaimCrossMintContinuation(ctx, b.Cluster, "next-owner", time.Minute); err != nil || l != nil {
		t.Fatalf("holding signed attempt permitted next leg: %v %v", l, err)
	}
}

func TestCrossMintUntouchedCancellationRequiresRevocationAndCannotReleaseSignedCustody(t *testing.T) {
	for _, tc := range []struct {
		name          string
		start         bool
		generation    int64
		signed, allow bool
	}{
		{"live activation remains authorized", true, 1, false, false},
		{"start revoked", false, 2, false, true},
		{"new generation does not revive old activation", true, 2, false, true},
		{"signed bytes retain capacity even before broadcast", false, 2, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, pool := integrationStore(t)
			ctx := context.Background()
			b := seedCrossMintMovement(t, ctx, pool)
			if tc.generation != 1 {
				if _, err := pool.Exec(ctx, `UPDATE loyal_yield.cross_mint_movement_controls SET start_new_movements=$2,generation=$3 WHERE cluster=$1`, b.Cluster, tc.start, tc.generation); err != nil {
					t.Fatal(err)
				}
			}
			l, err := store.ClaimCrossMintContinuation(ctx, b.Cluster, "cancel-owner", time.Minute)
			if err != nil || l == nil {
				t.Fatalf("claim: %v", err)
			}
			if authorized, err := store.crossMintInitialAuthority(ctx, *l); err != nil || authorized != (tc.generation == 1) {
				t.Fatalf("activation generation: %v %v", authorized, err)
			}
			if tc.signed {
				e, pre, _, _ := crossWithdrawContract()
				seedCrossMintReceiptAttempt(t, ctx, pool, b, LegWithdraw, PurposeOptimizeYield, e, pre, 500)
			}
			err = store.CancelUntouchedCrossMint(ctx, *l, 501)
			if (err == nil) != tc.allow {
				t.Fatalf("allow=%v err=%v", tc.allow, err)
			}
			var reservation, opportunity string
			var outcome *string
			if err := pool.QueryRow(ctx, `SELECT r.reservation_state,o.opportunity_state,d.terminal_outcome FROM loyal_yield.rebalance_decisions d JOIN loyal_yield.rebalance_opportunities o ON o.decision_id=d.id JOIN loyal_yield.target_capacity_reservations r ON r.decision_id=d.id WHERE d.id=$1`, b.DecisionID).Scan(&reservation, &opportunity, &outcome); err != nil {
				t.Fatal(err)
			}
			if tc.allow {
				if reservation != "released" || opportunity != "completed" || outcome == nil || *outcome != "cancelled_before_withdraw" {
					t.Fatalf("incomplete atomic cancellation: %s %s %v", reservation, opportunity, outcome)
				}
			} else if reservation != "active" || opportunity != "decision_created" || outcome != nil {
				t.Fatalf("failed cancellation released authority: %s %s %v", reservation, opportunity, outcome)
			}
		})
	}
}

func TestCrossMintFinalizedReceiptAdvancesCustodyAtomicallyAndRejectsStaleOwner(t *testing.T) {
	store, pool := integrationStore(t)
	ctx := context.Background()
	b := seedCrossMintMovement(t, ctx, pool)
	e, pre, actual, post := crossWithdrawContract()
	l := seedCrossMintReceiptAttempt(t, ctx, pool, b, LegWithdraw, PurposeOptimizeYield, e, pre, 10)
	bad := post
	bad.Credit = &CrossMintTokenAmount{Mint: "source-mint", TokenAccount: "source-ata", AmountRaw: 1046}
	if _, err := store.ReconcileCrossMintLeg(ctx, l, CrossMintReconciliation{FinalizedSlot: 10, Effect: actual, BalanceAnchors: bad}); err == nil {
		t.Fatal("incorrect balance receipt committed")
	}
	m, err := store.CrossMintMovement(ctx, b.DecisionID)
	if err != nil || m.CustodyVersion != 0 || m.CustodyAmountRaw != 1000 {
		t.Fatalf("failed receipt partly changed custody: %+v %v", m, err)
	}
	stale := l
	stale.FencingToken++
	if _, err := store.ReconcileCrossMintLeg(ctx, stale, CrossMintReconciliation{FinalizedSlot: 10, Effect: actual, BalanceAnchors: post}); !errors.Is(err, ErrStaleOwner) {
		t.Fatalf("stale receipt owner: %v", err)
	}
	m, err = store.ReconcileCrossMintLeg(ctx, l, CrossMintReconciliation{FinalizedSlot: 10, Effect: actual, BalanceAnchors: post})
	if err != nil {
		t.Fatal(err)
	}
	if m.Phase != CrossMintSourceIdle || m.CustodyVersion != 1 || m.CustodyAmountRaw != 995 || m.CustodyObservedBalanceRaw == nil || *m.CustodyObservedBalanceRaw != 1045 {
		t.Fatalf("receipt attribution mismatch: %+v", m)
	}
	var state, reservation string
	if err = pool.QueryRow(ctx, `SELECT s.submission_state,r.reservation_state FROM loyal_yield.signed_route_submissions s JOIN loyal_yield.target_capacity_reservations r ON r.decision_id=s.decision_id WHERE s.id=$1`, l.Submission.ID).Scan(&state, &reservation); err != nil {
		t.Fatal(err)
	}
	if state != "reconciled" || reservation != "active" {
		t.Fatalf("intermediate receipt released custody capacity: %s %s", state, reservation)
	}
	if next, err := store.ClaimCrossMintContinuation(ctx, b.Cluster, "swap-owner", time.Minute); err != nil || next == nil || next.Movement.CustodyAmountRaw != 995 {
		t.Fatalf("committed receipt did not expose actual next custody: %+v %v", next, err)
	}
}

func TestCrossMintLeaseExpiryDuringRowLockCannotPublish(t *testing.T) {
	store, pool := integrationStore(t)
	ctx := context.Background()
	b := seedCrossMintMovement(t, ctx, pool)
	l, err := store.ClaimCrossMintContinuation(ctx, b.Cluster, "blocked-owner", time.Minute)
	if err != nil || l == nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err = pool.Exec(ctx, `UPDATE loyal_yield.rebalance_decisions SET continuation_lease_expires_at=clock_timestamp()+interval '100 milliseconds' WHERE id=$1`, b.DecisionID); err != nil {
		t.Fatal(err)
	}
	blocker, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(ctx)
	if _, err = blocker.Exec(ctx, `SELECT id FROM loyal_yield.rebalance_decisions WHERE id=$1 FOR UPDATE`, b.DecisionID); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- func() error {
			tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
			if err != nil {
				return err
			}
			defer tx.Rollback(ctx)
			close(started)
			_, err = lockCrossMintLease(ctx, tx, *l)
			return err
		}()
	}()
	<-started
	time.Sleep(150 * time.Millisecond)
	if err = blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-done; !errors.Is(err, ErrStaleOwner) {
		t.Fatalf("expired blocked lease became authorization: %v", err)
	}
}

func advanceCrossMintFixtureToTargetIdle(t *testing.T, ctx context.Context, store *Store, pool *pgxpool.Pool, b seededBaseline) CrossMintMovement {
	t.Helper()
	e, pre, actual, post := crossWithdrawContract()
	l := seedCrossMintReceiptAttempt(t, ctx, pool, b, LegWithdraw, PurposeOptimizeYield, e, pre, 1000)
	m, err := store.ReconcileCrossMintLeg(ctx, l, CrossMintReconciliation{FinalizedSlot: 1000, Effect: actual, BalanceAnchors: post})
	if err != nil {
		t.Fatal(err)
	}
	e = CrossMintExpectedEffect{Debit: &CrossMintTokenAmount{Mint: m.SourceMint, TokenAccount: m.CustodyAccount, AmountRaw: m.CustodyAmountRaw}, CreditMint: crossString(m.TargetMint), CreditTokenAccount: crossString("target-ata"), MinimumCreditAmountRaw: crossInt(990)}
	pre = CrossMintBalanceAnchors{Debit: &CrossMintTokenAmount{Mint: m.SourceMint, TokenAccount: m.CustodyAccount, AmountRaw: *m.CustodyObservedBalanceRaw}, Credit: &CrossMintTokenAmount{Mint: m.TargetMint, TokenAccount: "target-ata", AmountRaw: 7}}
	actual = CrossMintEffect{Debit: e.Debit, Credit: &CrossMintTokenAmount{Mint: m.TargetMint, TokenAccount: "target-ata", AmountRaw: 990}}
	post = CrossMintBalanceAnchors{Debit: &CrossMintTokenAmount{Mint: m.SourceMint, TokenAccount: m.CustodyAccount, AmountRaw: 50}, Credit: &CrossMintTokenAmount{Mint: m.TargetMint, TokenAccount: "target-ata", AmountRaw: 997}}
	l = seedCrossMintReceiptAttempt(t, ctx, pool, b, LegSwap, PurposeOptimizeYield, e, pre, 1010)
	m, err = store.ReconcileCrossMintLeg(ctx, l, CrossMintReconciliation{FinalizedSlot: 1010, Effect: actual, BalanceAnchors: post})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestCrossMintThreeFinalizedLegsKeepCapacityUntilNewerTargetTelemetry(t *testing.T) {
	store, pool := integrationStore(t)
	ctx := context.Background()
	b := seedCrossMintMovement(t, ctx, pool)
	m := advanceCrossMintFixtureToTargetIdle(t, ctx, store, pool, b)
	e := CrossMintExpectedEffect{Debit: &CrossMintTokenAmount{Mint: m.TargetMint, TokenAccount: m.CustodyAccount, AmountRaw: m.CustodyAmountRaw}}
	pre := CrossMintBalanceAnchors{Debit: &CrossMintTokenAmount{Mint: m.TargetMint, TokenAccount: m.CustodyAccount, AmountRaw: 997}, Position: &CrossMintPositionAnchor{Reserve: m.ActiveTargetReserve, Market: "market", Obligation: "target-obligation", ObligationExists: true, CollateralRaw: 10}}
	actual := CrossMintEffect{Debit: e.Debit}
	post := CrossMintBalanceAnchors{Debit: &CrossMintTokenAmount{Mint: m.TargetMint, TokenAccount: m.CustodyAccount, AmountRaw: 7}, Position: &CrossMintPositionAnchor{Reserve: m.ActiveTargetReserve, Market: "market", Obligation: "target-obligation", ObligationExists: true, CollateralRaw: 1000}}
	l := seedCrossMintReceiptAttempt(t, ctx, pool, b, LegDeposit, PurposeOptimizeYield, e, pre, 1020)
	m, err := store.ReconcileCrossMintLeg(ctx, l, CrossMintReconciliation{FinalizedSlot: 1020, Effect: actual, BalanceAnchors: post})
	if err != nil {
		t.Fatal(err)
	}
	if m.CustodyVersion != 3 || m.Phase != CrossMintTargetReserve || m.CustodyAmountRaw != 0 || m.CustodyAccount != b.Reserve || m.TerminalOutcome == nil || *m.TerminalOutcome != "completed_target" {
		t.Fatalf("three receipts did not complete target: %+v", m)
	}
	var state string
	if err = pool.QueryRow(ctx, `SELECT reservation_state FROM loyal_yield.target_capacity_reservations WHERE decision_id=$1`, b.DecisionID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "awaiting_telemetry" {
		t.Fatalf("terminal movement released capacity before reflected observation: %s", state)
	}
	if next, err := store.ClaimCrossMintContinuation(ctx, b.Cluster, "late-owner", time.Minute); err != nil || next != nil {
		t.Fatalf("terminal movement reactivated: %+v %v", next, err)
	}
}

func TestCrossMintFallbackIncludesUnattachedCapacityAndInvalidatesOldLease(t *testing.T) {
	store, pool := integrationStore(t)
	ctx := context.Background()
	b := seedCrossMintMovement(t, ctx, pool)
	advanceCrossMintFixtureToTargetIdle(t, ctx, store, pool, b)
	l, err := store.ClaimCrossMintContinuation(ctx, b.Cluster, "fallback-owner", time.Minute)
	if err != nil || l == nil {
		t.Fatalf("claim: %v", err)
	}
	other := seedBaseline(t, ctx, pool, fmt.Sprint(time.Now().UnixNano()), b.Cluster)
	if _, err = pool.Exec(ctx, `INSERT INTO loyal_yield.target_capacity_frontiers(cluster,target_reserve,liquidity_mint,observed_supply_usd_micros,observed_slot,maximum_inflight_usd_micros) VALUES($1,$2,$3,1000000000,500,900000000)`, b.Cluster, other.Reserve, b.Mint); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `UPDATE loyal_yield.target_capacity_reservations SET decision_id=NULL,liquidity_mint=$2,principal_usd_micros=900000000 WHERE opportunity_id=$1`, other.OpportunityID, b.Mint); err != nil {
		t.Fatal(err)
	}
	p := CrossMintCapacityProjection{Cluster: b.Cluster, TargetReserve: other.Reserve, LiquidityMint: b.Mint, ObservedSupplyUSDMicros: 1000000000, ObservedSlot: 500, MaximumInflightUSDMicros: 900000000, TelemetryVersion: 1}
	if err = pool.QueryRow(ctx, `SELECT telemetry_version FROM loyal_yield.target_capacity_frontiers WHERE cluster=$1 AND target_reserve=$2 AND liquidity_mint=$3`, p.Cluster, p.TargetReserve, p.LiquidityMint).Scan(&p.TelemetryVersion); err != nil {
		t.Fatal(err)
	}
	if _, err = store.RebindCrossMintFallbackCapacity(ctx, *l, p); err == nil || !strings.Contains(err.Error(), "capacity exhausted") {
		t.Fatalf("NULL decision capacity was not counted: %v", err)
	}
	if _, err = pool.Exec(ctx, `UPDATE loyal_yield.target_capacity_reservations SET reservation_state='released',released_at=clock_timestamp(),release_reason='fixture-no-spend' WHERE opportunity_id=$1`, other.OpportunityID); err != nil {
		t.Fatal(err)
	}
	if _, err = store.RebindCrossMintFallbackCapacity(ctx, *l, p); err != nil {
		t.Fatal(err)
	}
	if _, _, err = store.CrossMintLegBudget(ctx, *l, LegDeposit); !errors.Is(err, ErrStaleOwner) {
		t.Fatalf("fallback retained old wire authority: %v", err)
	}
	newLease, err := store.ClaimCrossMintContinuation(ctx, b.Cluster, "new-fallback-owner", time.Minute)
	if err != nil || newLease == nil || newLease.Movement.ActiveTargetReserve != other.Reserve {
		t.Fatalf("fallback did not expose new binding: %+v %v", newLease, err)
	}
	if _, err = store.RebindCrossMintFallbackCapacity(ctx, *newLease, p); err == nil {
		t.Fatal("fallback rebound repeatedly")
	}
}

func TestCrossMintALTQueueSealsFullDemandAndPreservesMovement(t *testing.T) {
	store, pool := integrationStore(t)
	ctx := context.Background()
	b := seedCrossMintMovement(t, ctx, pool)
	l, err := store.ClaimCrossMintContinuation(ctx, b.Cluster, "alt-owner", time.Minute)
	if err != nil || l == nil {
		t.Fatalf("claim: %v", err)
	}
	key := mustSignedFixture(t).SecondaryAccount
	p := CrossMintPreparedLeg{WaitingALT: true, MissingAddresses: []string{key}, VaultAddresses: []CrossMintALTAddress{{Address: key, SemanticClass: "vault", AccountRole: "vault_ata", Ordinal: 0, Writable: true}}, Preparation: fleet.RoutePreparation{RouteFingerprint: "route-fp", RequirementsFingerprint: "requirement-fp", ExecutionPlan: l.Movement.ExecutionPlan}}
	shared := CrossMintALTAddress{Address: mustSignedFixture(t).LookupTable, SemanticClass: "shared_market", AccountRole: "infrastructure", Ordinal: 0}
	p.SharedAddresses = []CrossMintALTAddress{shared}
	_, catalogTable := seedCrossMintCatalog(t, ctx, pool, b.Cluster, p.SharedAddresses)
	r := CrossMintLegRequest{Movement: l.Movement, Leg: LegWithdraw, Purpose: PurposeOptimizeYield}
	// Physical append remnants must not certify a current sealed catalog.
	if _, err = pool.Exec(ctx, `INSERT INTO loyal_yield.lookup_table_addresses(route_lookup_table_id,address,ordinal,added_slot,usable_after_slot,last_verified_slot,last_verified_at) VALUES($1,$2,1,100,101,101,clock_timestamp())`, catalogTable, key); err != nil {
		t.Fatal(err)
	}
	if _, err = store.QueueCrossMintALT(ctx, *l, r, p); err == nil {
		t.Fatal("catalog accepted an extra physical member")
	}
	var queued int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM loyal_yield.lookup_table_provisioning_requests WHERE cluster=$1`, b.Cluster).Scan(&queued); err != nil || queued != 0 {
		t.Fatalf("failed catalog validation persisted demand: %d %v", queued, err)
	}
	if _, err = pool.Exec(ctx, `DELETE FROM loyal_yield.lookup_table_addresses WHERE route_lookup_table_id=$1 AND address=$2`, catalogTable, key); err != nil {
		t.Fatal(err)
	}
	id, err := store.QueueCrossMintALT(ctx, *l, r, p)
	if err != nil {
		t.Fatal(err)
	}
	var sealed bool
	var writable bool
	var role string
	var count int
	if err = pool.QueryRow(ctx, `SELECT r.sealed_at IS NOT NULL,r.desired_vault_address_count,a.account_role,a.is_writable FROM loyal_yield.lookup_table_provisioning_requests r JOIN loyal_yield.lookup_table_provisioning_request_addresses a ON a.request_id=r.id WHERE r.id=$1 AND a.semantic_class='vault'`, id).Scan(&sealed, &count, &role, &writable); err != nil {
		t.Fatal(err)
	}
	if !sealed || count != 1 || !writable || role != "vault_ata" {
		t.Fatal("ALT queue discarded complete role/access manifest")
	}
	if _, err = store.QueueCrossMintALT(ctx, *l, r, p); !errors.Is(err, ErrStaleOwner) {
		t.Fatalf("old movement lease reused after enqueue: %v", err)
	}
	if _, err = pool.Exec(ctx, `UPDATE loyal_yield.rebalance_decisions SET continuation_available_at=clock_timestamp() WHERE id=$1`, b.DecisionID); err != nil {
		t.Fatal(err)
	}
	l, err = store.ClaimCrossMintContinuation(ctx, b.Cluster, "next-alt-owner", time.Minute)
	if err != nil || l == nil {
		t.Fatalf("reclaim: %v", err)
	}
	r.Movement = l.Movement
	changed := p
	changed.VaultAddresses = append([]CrossMintALTAddress(nil), p.VaultAddresses...)
	changed.VaultAddresses[0].Writable = false
	if _, err = store.QueueCrossMintALT(ctx, *l, r, changed); err == nil {
		t.Fatal("same requirement accepted changed sealed access")
	}
	idAgain, err := store.QueueCrossMintALT(ctx, *l, r, p)
	if err != nil || idAgain != id {
		t.Fatalf("identical demand allocated twice: %d %d %v", id, idAgain, err)
	}
	var plan json.RawMessage
	var state string
	if err = pool.QueryRow(ctx, `SELECT execution_plan,opportunity_state FROM loyal_yield.rebalance_opportunities WHERE id=$1`, b.OpportunityID).Scan(&plan, &state); err != nil {
		t.Fatal(err)
	}
	if !sameJSON(plan, l.Movement.ExecutionPlan) || state != "decision_created" {
		t.Fatal("continuation ALT demand rewrote immutable opportunity")
	}
}

// Retained catalog graph from fleet's registered waiting-ALT fixture. It is
// source-table evidence, not a bypass or a replacement schema.
func seedCrossMintCatalog(t *testing.T, ctx context.Context, pool *pgxpool.Pool, cluster string, addresses []CrossMintALTAddress) (int64, int64) {
	t.Helper()
	var family, manifest, revision, table int64
	authority := mustSignedFixture(t).FeePayer
	if err := pool.QueryRow(ctx, `INSERT INTO loyal_yield.lookup_table_families(cluster,logical_name,kind,planner_version,catalog_version,active_generation,provisioning_authority,payer,hard_capacity,largest_atomic_expansion,safety_margin,allocation_high_water) VALUES($1,'cross-mint-catalog','shared_market','v1','v1',1,$2,$2,256,239,1,16) RETURNING id`, cluster, authority).Scan(&family); err != nil {
		t.Fatal(err)
	}
	hash := crossMintALTHash(addresses)
	if err := pool.QueryRow(ctx, `INSERT INTO loyal_yield.lookup_table_manifests(family_id,subject_kind,subject_key,desired_set_hash,address_count,source_slot,planner_version,catalog_version) VALUES($1,'shared_market','cross-mint-catalog',$2,$3,100,'v1','v1') RETURNING id`, family, hash, len(addresses)).Scan(&manifest); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, len(addresses))
	for i, a := range addresses {
		keys[i] = a.Address
		if _, err := pool.Exec(ctx, `INSERT INTO loyal_yield.lookup_table_manifest_addresses(manifest_id,address,ordinal,semantic_class,account_role,is_writable) VALUES($1,$2,$3,$4,$5,$6)`, manifest, a.Address, a.Ordinal, a.SemanticClass, a.AccountRole, a.Writable); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE loyal_yield.lookup_table_manifests SET sealed_at=clock_timestamp() WHERE id=$1`, manifest); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO loyal_yield.lookup_table_shared_market_catalog_revisions(family_id,manifest_id,catalog_revision,catalog_version,desired_set_hash,enabled_mints_hash,reserve_set_hash,address_count,source_slot,reason,updated_by) VALUES($1,$2,1,'v1',$3,$3,$3,$4,100,'fixture','fixture') RETURNING id`, family, manifest, hash, len(addresses)).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO loyal_yield.lookup_table_shared_market_catalog_heads(family_id,catalog_revision_id,target_generation,readiness_state,activated_at) VALUES($1,$2,1,'active',clock_timestamp())`, family, revision); err != nil {
		t.Fatal(err)
	}
	key := sha256.Sum256([]byte("cross-mint-catalog:" + cluster))
	raw, _ := json.Marshal(keys)
	if err := pool.QueryRow(ctx, `INSERT INTO loyal_yield.route_lookup_tables(cluster,scope,table_address,authority,payer,status,durable,address_count,addresses,family_id,allocation_kind,generation,shard_ordinal,desired_state,accepting_allocations,allocation_high_water,reserved_address_count,usable_address_count,last_verified_slot,mutation_epoch) VALUES($1,'cross-mint-catalog',$2,$3,$3,'active',true,$4,$5::jsonb,$6,'shared_market',1,0,'active',true,16,0,$4,101,0) RETURNING id`, cluster, sdk.PublicKeyFromBytes(key[:]).String(), authority, len(keys), raw, family).Scan(&table); err != nil {
		t.Fatal(err)
	}
	for i, a := range addresses {
		if _, err := pool.Exec(ctx, `INSERT INTO loyal_yield.lookup_table_addresses(route_lookup_table_id,address,ordinal,added_slot,usable_after_slot,last_verified_slot,last_verified_at) VALUES($1,$2,$3,100,101,101,clock_timestamp())`, table, a.Address, i); err != nil {
			t.Fatal(err)
		}
	}
	return family, table
}
