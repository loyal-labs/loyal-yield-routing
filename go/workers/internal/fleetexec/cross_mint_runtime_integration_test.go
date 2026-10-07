package fleetexec

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/gagliardetto/solana-go"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/solana"
)

type runtimeStatus struct {
	status   SignatureStatus
	receipt  *TransactionReceipt
	calls    int
	sequence []SignatureStatus
}

func (s *runtimeStatus) SignatureStatus(context.Context, string) (SignatureStatus, error) {
	s.calls++
	if len(s.sequence) > 0 {
		next := s.sequence[0]
		s.sequence = s.sequence[1:]
		return next, nil
	}
	return s.status, nil
}
func (s *runtimeStatus) FinalizedTransaction(context.Context, string) (*TransactionReceipt, error) {
	return s.receipt, nil
}

// runtimeSend is the landing chain over the same scripted status. A send
// numbered landOn finalizes the signature, like a forward that arrives.
type runtimeSend struct {
	status *runtimeStatus
	wire   []byte
	calls  int
	landOn int
	err    error
}

func (s *runtimeSend) SendWire(_ context.Context, wire []byte, _ bool) error {
	s.calls++
	s.wire = append([]byte(nil), wire...)
	if s.calls == s.landOn {
		s.status.status = SignatureStatus{Found: true, Confirmed: true, Finalized: true, Slot: 1010, ContextSlot: 1010, BlockHeight: s.status.status.BlockHeight}
	}
	return s.err
}

func (s *runtimeSend) FinalizedBlockHeight(context.Context) (uint64, error) {
	return uint64(s.status.status.BlockHeight), nil
}

func (s *runtimeSend) SignatureState(context.Context, string) (solana.SignatureState, error) {
	st := s.status.status
	out := solana.SignatureState{Found: st.Found, Slot: uint64(st.Slot), Err: st.Err, ContextSlot: uint64(st.ContextSlot), Commitment: solana.Processed}
	if st.Confirmed {
		out.Commitment = solana.Confirmed
	}
	if st.Finalized {
		out.Commitment = solana.Finalized
	}
	return out, nil
}

type runtimeVerifier struct {
	calls    int
	evidence CrossMintFirstSendRequest
	err      error
}

func (v *runtimeVerifier) VerifyCrossMintFirstSend(_ context.Context, q CrossMintFirstSendRequest) error {
	v.calls++
	v.evidence = q
	return v.err
}

// These are deterministic SQL lifecycle/bank-decoder fixtures. The synthetic
// protected instruction and verifier below do NOT prove Squads/KLend/Jupiter
// execution or production C first-send policy validation.
func crossMintRuntimeFixture(t *testing.T) (*CrossMintRuntime, *pgxpool.Pool, int64) {
	t.Helper()
	ctx := context.Background()
	store, pool := integrationStore(t)
	a, signer, input := seedCrossMintActivation(t, ctx, pool)
	key := func(label string) string {
		return sdk.PublicKey(sha256.Sum256([]byte(a.Lease.Cluster + label))).String()
	}
	a.Lease.VaultPubkey = key("vault")
	a.Lease.SourceReserve = key("source")
	a.Lease.TargetReserve = key("target")
	b, err := crossMintBindings(a.Lease.ExecutionPlan)
	if err != nil {
		t.Fatal(err)
	}
	b.VaultPubkey = a.Lease.VaultPubkey
	var plan map[string]any
	if err = json.Unmarshal(a.Lease.ExecutionPlan, &plan); err != nil {
		t.Fatal(err)
	}
	plan["policy_bindings"] = b
	a.Lease.ExecutionPlan, err = json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	a.Preparation.ExecutionPlan = a.Lease.ExecutionPlan
	var cert fleet.CrossMintPreflightCertificate
	if err = json.Unmarshal(input.PreflightCertification, &cert); err != nil {
		t.Fatal(err)
	}
	cert.JupiterBuild.TargetReserve = a.Lease.TargetReserve
	input.PreflightCertification, err = json.Marshal(cert)
	if err != nil {
		t.Fatal(err)
	}
	oldConflict := a.ConflictKeys[0]
	a.ConflictKeys[0] = "vault-write:" + a.Lease.VaultPubkey
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`UPDATE loyal_yield.managed_vaults SET vault_pubkey=$2 WHERE id=$1`, []any{a.Lease.VaultID, a.Lease.VaultPubkey}},
		{`UPDATE loyal_yield.route_policies SET vault_pubkey=$2 WHERE id=(SELECT active_policy_id FROM loyal_yield.managed_vaults WHERE id=$1)`, []any{a.Lease.VaultID, a.Lease.VaultPubkey}},
		{`UPDATE loyal_yield.cross_mint_swap_policies SET vault_pubkey=$2 WHERE cluster=$1`, []any{a.Lease.Cluster, a.Lease.VaultPubkey}},
		{`UPDATE loyal_yield.cross_mint_vault_opt_ins SET vault_pubkey=$2 WHERE cluster=$1`, []any{a.Lease.Cluster, a.Lease.VaultPubkey}},
		{`UPDATE loyal_yield.target_capacity_frontiers SET target_reserve=$2 WHERE cluster=$1`, []any{a.Lease.Cluster, a.Lease.TargetReserve}},
		{`UPDATE loyal_yield.rebalance_opportunities SET source_reserve=$2,target_reserve=$3,execution_plan=$4 WHERE id=$1`, []any{a.Lease.OpportunityID, a.Lease.SourceReserve, a.Lease.TargetReserve, a.Lease.ExecutionPlan}},
		{`UPDATE loyal_yield.route_account_conflict_leases SET writable_account_key=$3 WHERE cluster=$1 AND writable_account_key=$2`, []any{a.Lease.Cluster, oldConflict, a.ConflictKeys[0]}},
	} {
		if _, err = pool.Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	input.Capacity.TargetReserve = a.Lease.TargetReserve
	c := sameMintPostContract{vault: a.Lease.VaultPubkey, source: a.Lease.SourceReserve, target: a.Lease.TargetReserve, mint: a.Lease.SourceLiquidityMint, minimumSlot: 1000, sourceKind: "reserve_position"}
	reader := postFixture(t, c, 1005, 1001, 50)
	market, obligation, _, program, err := reservePostIdentity(reader.accounts[c.source], c.mint, c.vault)
	if err != nil {
		t.Fatal(err)
	}
	ata, err := associatedCustodyAccount(c.vault, c.mint, program)
	if err != nil {
		t.Fatal(err)
	}
	members := []string{ata, c.source, obligation}
	table := &a.SelectedALTs[0]
	table.Addresses = members
	if _, err = pool.Exec(ctx, `DELETE FROM loyal_yield.lookup_table_addresses WHERE route_lookup_table_id=$1`, table.TableID); err != nil {
		t.Fatal(err)
	}
	for i, member := range members {
		if _, err = pool.Exec(ctx, `INSERT INTO loyal_yield.lookup_table_addresses(route_lookup_table_id,address,ordinal,added_slot,usable_after_slot,last_verified_slot,last_verified_at) VALUES($1,$2,$3,990,991,1000,clock_timestamp())`, table.TableID, member, i); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = pool.Exec(ctx, `UPDATE loyal_yield.route_lookup_tables SET address_count=3,usable_address_count=3,reserved_address_count=3 WHERE id=$1`, table.TableID); err != nil {
		t.Fatal(err)
	}
	tx, err := sdk.TransactionFromBytes(a.Preparation.Transaction.UnsignedWire)
	if err != nil {
		t.Fatal(err)
	}
	tx.Message.AddressTableLookups[0].WritableIndexes = []uint8{0, 1, 2}
	message, err := tx.Message.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	unsigned, err := tx.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	mh, wh := sha256.Sum256(message), sha256.Sum256(unsigned)
	a.Preparation.Transaction.Message = message
	a.Preparation.Transaction.UnsignedWire = unsigned
	a.Preparation.Transaction.MessageSHA256 = hex.EncodeToString(mh[:])
	a.Preparation.Transaction.WireSHA256 = hex.EncodeToString(wh[:])
	a.Preparation.Transaction.PacketBytes = len(unsigned)
	a.Preparation.Transaction.WritableAccounts = append([]string{signerPublic(signer)}, members...)
	a.Preparation.Simulation.WireSHA256 = a.Preparation.Transaction.WireSHA256
	selected, err := json.Marshal(a.SelectedALTs)
	if err != nil {
		t.Fatal(err)
	}
	sh := sha256.Sum256(selected)
	a.AltSelectionFingerprint = hex.EncodeToString(sh[:])
	m, err := store.ActivateCrossMintMovement(ctx, a.Lease, input)
	if err != nil {
		t.Fatal(err)
	}
	l, err := store.ClaimCrossMintContinuation(ctx, m.Cluster, a.Lease.Owner, time.Minute)
	if err != nil || l == nil {
		t.Fatalf("claim continuation: %v", err)
	}
	prepared := CrossMintPreparedLeg{Preparation: a.Preparation, LastValidBlockHeight: a.LastValidBlockHeight, PolicyAccount: b.Withdraw.PolicyAccount, ExpectedEffect: CrossMintExpectedEffect{CreditMint: crossString(m.SourceMint), CreditTokenAccount: crossString(ata), MinimumCreditAmountRaw: crossInt(m.PlannedAmountRaw - 10)}, BalanceAnchors: CrossMintBalanceAnchors{Credit: &CrossMintTokenAmount{Mint: m.SourceMint, TokenAccount: ata, AmountRaw: 50}, Position: &CrossMintPositionAnchor{Reserve: m.SourceReserve, Market: market, Obligation: obligation, ObligationExists: true, CollateralRaw: 1001}}, ConflictKeys: a.ConflictKeys, SelectedALTs: a.SelectedALTs, AltSelectionFingerprint: a.AltSelectionFingerprint}
	wire, err := signer.SignPreparedRoute(prepared.Preparation.Transaction, prepared.LastValidBlockHeight)
	if err != nil {
		t.Fatal(err)
	}
	id, err := store.AppendCrossMintLeg(ctx, *l, CrossMintLegRequest{Movement: m, Leg: LegWithdraw, Purpose: PurposeOptimizeYield, Generation: 1, RemainingFeeLamports: 50000}, prepared, wire)
	if err != nil {
		t.Fatal(err)
	}
	status := &runtimeStatus{status: SignatureStatus{ContextSlot: 1005, BlockHeight: 3999}}
	return &CrossMintRuntime{config: Config{Cluster: m.Cluster, Owner: a.Lease.Owner, LeaseTTL: time.Minute, BatchSize: 1, TickInterval: time.Second, Facts: testFacts()}, store: store, accounts: reader, history: addressHistory{pages: map[string][]finalizedAddressSignature{}}, status: status, chain: &runtimeSend{status: status, landOn: 1}, verifier: &runtimeVerifier{}}, pool, id
}

func runtimeClaim(t *testing.T, r *CrossMintRuntime, pool *pgxpool.Pool) SubmissionLease {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `UPDATE loyal_yield.signed_route_submissions SET confirmation_available_at=clock_timestamp() WHERE cluster=$1 AND confirmation_lease_owner IS NULL`, r.config.Cluster); err != nil {
		t.Fatal(err)
	}
	leases, err := r.store.ClaimCrossMintRecoveryWork(context.Background(), r.config.Cluster, r.config.Owner, time.Minute, 1)
	if err != nil || len(leases) != 1 {
		t.Fatalf("claim family: %d %v", len(leases), err)
	}
	return leases[0]
}

func TestCrossMintRuntimeLandsExactWireAcrossDroppedForwards(t *testing.T) {
	// The Go runtime used to send a leg once and then wait for its blockhash
	// to expire when the forward was dropped; the Rust confirmer rebroadcast.
	r, pool, id := crossMintRuntimeFixture(t)
	l := runtimeClaim(t, r, pool)
	sender := r.chain.(*runtimeSend)
	sender.err = errors.New("transport timeout")
	sender.landOn = 3
	if err := r.handle(context.Background(), l); err != nil {
		t.Fatal(err)
	}
	if sender.calls != 3 || string(sender.wire) != string(l.Submission.SignedTransaction) {
		t.Fatalf("sent %d times; want the exact persisted bytes until they land", sender.calls)
	}
	verifier := r.verifier.(*runtimeVerifier)
	if verifier.calls != 1 || len(verifier.evidence.SelectedALTs) != 1 || verifier.evidence.MinimumSlot < 1000 {
		t.Fatal("first send omitted actual compiled ALT/bank evidence or was re-verified on resend")
	}
	l = runtimeClaim(t, r, pool)
	if l.Submission.BroadcastCount != 3 || l.Submission.State != StateReconciliationPending {
		t.Fatalf("landing state %s count %d", l.Submission.State, l.Submission.BroadcastCount)
	}
	var reservation string
	if err := pool.QueryRow(context.Background(), `SELECT reservation_state FROM loyal_yield.target_capacity_reservations WHERE signed_submission_id=$1 OR decision_id=(SELECT decision_id FROM loyal_yield.signed_route_submissions WHERE id=$1)`, id).Scan(&reservation); err != nil || reservation != "active" {
		t.Fatalf("landing released capacity before the receipt: %s %v", reservation, err)
	}
}

func TestCrossMintRuntimeInitialExpiryWithoutHistoryAnchorRetainsManualHold(t *testing.T) {
	r, pool, _ := crossMintRuntimeFixture(t)
	status := r.status.(*runtimeStatus)
	status.status.BlockHeight = 4001
	l := runtimeClaim(t, r, pool)
	if err := r.handle(context.Background(), l); err != nil {
		t.Fatal(err)
	}
	l = runtimeClaim(t, r, pool)
	if l.Submission.EffectCheckSlot == nil || l.Submission.ExpiryObservedBlockHeight == nil {
		t.Fatal("expiry did not bind clocks")
	}
	if err := r.handle(context.Background(), l); err == nil {
		t.Fatal("unknown first-withdraw anchor became no-effect proof")
	}
	l = runtimeClaim(t, r, pool)
	if l.Submission.State != StateEffectAmbiguous || r.chain.(*runtimeSend).calls != 0 {
		t.Fatal("unknown expiry became publish or retry authority")
	}
	var receipts int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM loyal_yield.cross_mint_no_effect_receipts WHERE submission_id=$1`, l.Submission.ID).Scan(&receipts); err != nil || receipts != 0 {
		t.Fatalf("fabricated initial no-effect receipt: %d %v", receipts, err)
	}
}

func TestCrossMintRuntimeFinalityCannotAdvanceCustodyBeforeExactReceipt(t *testing.T) {
	r, pool, id := crossMintRuntimeFixture(t)
	status := r.status.(*runtimeStatus)
	status.status = SignatureStatus{Found: true, Confirmed: true, Finalized: true, Slot: 1010, ContextSlot: 1010, BlockHeight: 4000}
	l := runtimeClaim(t, r, pool)
	if err := r.handle(context.Background(), l); err != nil {
		t.Fatal(err)
	}
	l = runtimeClaim(t, r, pool)
	if l.Submission.State != StateReconciliationPending || l.Submission.ConfirmedSlot == nil || *l.Submission.ConfirmedSlot != 1010 {
		t.Fatal("finality did not hand off receipt owner")
	}
	if err := r.handle(context.Background(), l); err != nil {
		t.Fatal(err)
	} // Receipt is unknown, not zero effect.
	var version int64
	if err := pool.QueryRow(context.Background(), `SELECT custody_version FROM loyal_yield.rebalance_decisions WHERE id=(SELECT decision_id FROM loyal_yield.signed_route_submissions WHERE id=$1)`, id).Scan(&version); err != nil || version != 0 {
		t.Fatalf("signature finality inferred custody: %d %v", version, err)
	}
	if r.chain.(*runtimeSend).calls != 0 {
		t.Fatal("observed finalized signature was broadcast again")
	}
}

func TestCrossMintRuntimeFinalizedReceiptAdvancesExactCreditAndHistoricalCustody(t *testing.T) {
	r, pool, id := crossMintRuntimeFixture(t)
	runtimeFinalizeWithdrawal(t, r, pool, id)
}

func runtimeFinalizeWithdrawal(t *testing.T, r *CrossMintRuntime, pool *pgxpool.Pool, id int64) CrossMintMovement {
	t.Helper()
	ctx := context.Background()
	l := runtimeClaim(t, r, pool)
	m, err := r.store.CrossMintMovement(ctx, *l.Submission.DecisionID)
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := r.store.crossMintFirstSendEvidence(ctx, l, m)
	if err != nil {
		t.Fatal(err)
	}
	r.verifier.(*runtimeVerifier).evidence = evidence
	status := r.status.(*runtimeStatus)
	status.status = SignatureStatus{Found: true, Confirmed: true, Finalized: true, Slot: 1010, ContextSlot: 1010, BlockHeight: 4000}
	if err = r.handle(ctx, l); err != nil {
		t.Fatal(err)
	}
	l = runtimeClaim(t, r, pool)
	pre, err := parseCrossMintAnchors(l.Submission.ExpectedBalanceAnchors)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := sdk.TransactionFromBytes(l.Submission.SignedTransaction)
	if err != nil {
		t.Fatal(err)
	}
	message, err := tx.Message.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	keys := []string{}
	for _, key := range tx.Message.AccountKeys {
		keys = append(keys, key.String())
	}
	keys = append(keys, evidence.SelectedALTs[0].Addresses...)
	before, after := uint64(50), uint64(m.PlannedAmountRaw+50)
	status.receipt = &TransactionReceipt{Slot: 1010, Signature: l.Submission.Signature, MessageB64: base64.StdEncoding.EncodeToString(message), SignedTransaction: l.Submission.SignedTransaction, accountAddresses: keys, TokenDeltas: []TokenDelta{{Account: pre.Credit.TokenAccount, Mint: m.SourceMint, PreRaw: &before, PostRaw: &after, PreOwner: m.VaultPubkey, PostOwner: m.VaultPubkey, PreProgram: sdk.TokenProgramID.String(), PostProgram: sdk.TokenProgramID.String()}}}
	c := sameMintPostContract{vault: m.VaultPubkey, source: m.SourceReserve, target: m.ActiveTargetReserve, mint: m.SourceMint, minimumSlot: 1010, sourceKind: "reserve_position"}
	r.accounts = postFixture(t, c, 1011, 1, int64(after))
	r.history = addressHistory{pages: map[string][]finalizedAddressSignature{pre.Credit.TokenAccount: {{Signature: l.Submission.Signature, Slot: 1010, ConfirmationStatus: "finalized"}}, pre.Position.Obligation: {{Signature: l.Submission.Signature, Slot: 1010, ConfirmationStatus: "finalized"}}}}
	if err = r.handle(ctx, l); err != nil {
		t.Fatal(err)
	}
	actual, err := r.store.CrossMintMovement(ctx, m.DecisionID)
	if err != nil {
		t.Fatal(err)
	}
	if actual.Phase != CrossMintSourceIdle || actual.CustodyVersion != 1 || actual.CustodyAmountRaw != m.PlannedAmountRaw || actual.CustodyObservedBalanceRaw == nil || *actual.CustodyObservedBalanceRaw != int64(after) || actual.CustodyReconciledSlot == nil || *actual.CustodyReconciledSlot != 1010 {
		t.Fatalf("receipt did not atomically advance attributable custody: %+v", actual)
	}
	var state, reservation string
	if err = pool.QueryRow(ctx, `SELECT s.submission_state,r.reservation_state FROM loyal_yield.signed_route_submissions s JOIN loyal_yield.target_capacity_reservations r ON r.decision_id=s.decision_id WHERE s.id=$1`, id).Scan(&state, &reservation); err != nil || state != "reconciled" || reservation != "active" {
		t.Fatalf("intermediate receipt released capacity: %s %s %v", state, reservation, err)
	}
	if r.chain.(*runtimeSend).calls != 0 {
		t.Fatal("receipt recovery rebuilt or resent finalized wire")
	}
	return actual
}

func TestCrossMintRuntimeCrashAfterIntentResendsSameBytesWithoutNewFirstSend(t *testing.T) {
	r, pool, _ := crossMintRuntimeFixture(t)
	ctx := context.Background()
	l := runtimeClaim(t, r, pool)
	m, err := r.store.CrossMintMovement(ctx, *l.Submission.DecisionID)
	if err != nil {
		t.Fatal(err)
	}
	if err = r.store.recordCrossMintBroadcastIntent(ctx, l, m); err != nil {
		t.Fatal(err)
	}
	// Test-only expire the claimant as if its process had exited after journaling.
	if _, err = pool.Exec(ctx, `UPDATE loyal_yield.signed_route_submissions SET confirmation_lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, l.Submission.ID); err != nil {
		t.Fatal(err)
	}
	l = runtimeClaim(t, r, pool)
	if l.Submission.BroadcastCount != 1 || l.Submission.State != StateSigned {
		t.Fatal("intent crash did not retain exact signed state")
	}
	if err = r.handle(ctx, l); err != nil {
		t.Fatal(err)
	}
	sender := r.chain.(*runtimeSend)
	if sender.calls != 1 || string(sender.wire) != string(l.Submission.SignedTransaction) || r.verifier.(*runtimeVerifier).calls != 0 {
		t.Fatal("restart must resend the same bytes without a new first-send verification")
	}
	l = runtimeClaim(t, r, pool)
	if l.Submission.BroadcastCount != 2 || l.Submission.State != StateReconciliationPending {
		t.Fatalf("restart landing state %s count %d", l.Submission.State, l.Submission.BroadcastCount)
	}
}

func runtimePublishSourceRecovery(t *testing.T, r *CrossMintRuntime, pool *pgxpool.Pool, m CrossMintMovement) int64 {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `UPDATE loyal_yield.cross_mint_movement_controls SET generation=generation+1,start_new_movements=false WHERE cluster=$1`, m.Cluster); err != nil {
		t.Fatal(err)
	}
	l, err := r.store.ClaimCrossMintContinuation(ctx, m.Cluster, r.config.Owner, time.Minute)
	if err != nil || l == nil {
		t.Fatalf("existing custody stopped with new starts disabled: %v", err)
	}
	evidence := r.verifier.(*runtimeVerifier).evidence
	transaction, err := sdk.TransactionFromBytes(evidence.Submission.SignedTransaction)
	if err != nil {
		t.Fatal(err)
	}
	transaction.Signatures[0] = sdk.Signature{}
	transaction.Message.RecentBlockhash = sdk.Hash(sha256.Sum256([]byte(m.Cluster + "source-recovery")))
	message, err := transaction.Message.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	unsigned, err := transaction.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	mh, wh := sha256.Sum256(message), sha256.Sum256(unsigned)
	var requirements, route string
	if err = pool.QueryRow(ctx, `SELECT requirements_fingerprint,route_fingerprint FROM loyal_yield.rebalance_opportunities WHERE id=$1`, m.OpportunityID).Scan(&requirements, &route); err != nil {
		t.Fatal(err)
	}
	_, key := integrationWire(t, 112)
	signer := DelegateSigner{FeePayer: ed25519.PrivateKey(key)}
	reader := r.accounts.(fixtureAccounts)
	market, obligation, _, _, err := reservePostIdentity(reader.accounts[m.SourceReserve], m.SourceMint, m.VaultPubkey)
	if err != nil {
		t.Fatal(err)
	}
	selectedJSON, err := json.Marshal(evidence.SelectedALTs)
	if err != nil {
		t.Fatal(err)
	}
	selection := sha256.Sum256(selectedJSON)
	prepared := CrossMintPreparedLeg{Preparation: fleet.RoutePreparation{RouteFingerprint: route, RequirementsFingerprint: requirements, ExecutionPlan: m.ExecutionPlan, Transaction: fleet.PreparedTransaction{Message: message, UnsignedWire: unsigned, MessageSHA256: hex.EncodeToString(mh[:]), WireSHA256: hex.EncodeToString(wh[:]), PacketBytes: len(unsigned), LookupTables: []string{evidence.SelectedALTs[0].Address}, WritableAccounts: append([]string{signerPublic(signer)}, evidence.SelectedALTs[0].Addresses...), FeeLamports: 5000, ComputeLimit: 200000}, Simulation: fleet.SimulationEvidence{Succeeded: true, Slot: 1011, WireSHA256: hex.EncodeToString(wh[:]), UnitsConsumed: 10000}}, LastValidBlockHeight: 5000, PolicyAccount: evidence.PolicyAccount, ExpectedEffect: CrossMintExpectedEffect{Debit: &CrossMintTokenAmount{Mint: m.SourceMint, TokenAccount: m.CustodyAccount, AmountRaw: m.CustodyAmountRaw}}, BalanceAnchors: CrossMintBalanceAnchors{Debit: &CrossMintTokenAmount{Mint: m.SourceMint, TokenAccount: m.CustodyAccount, AmountRaw: *m.CustodyObservedBalanceRaw}, Position: &CrossMintPositionAnchor{Reserve: m.SourceReserve, Market: market, Obligation: obligation, ObligationExists: true, CollateralRaw: 1}}, SelectedALTs: evidence.SelectedALTs, AltSelectionFingerprint: hex.EncodeToString(selection[:]), ConflictKeys: []string{"vault-write:" + m.VaultPubkey, fmt.Sprintf("fleet-shared-write-lane:%02d", m.VaultID%64)}}
	wire, err := signer.SignPreparedRoute(prepared.Preparation.Transaction, prepared.LastValidBlockHeight)
	if err != nil {
		t.Fatal(err)
	}
	id, err := r.store.AppendCrossMintLeg(ctx, *l, CrossMintLegRequest{Movement: m, Leg: LegDeposit, Purpose: PurposeRecoverSource, Generation: 1, RemainingFeeLamports: 45000}, prepared, wire)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestCrossMintRuntimeExpiryPublishesConcreteNoEffectOnlyAfterKnownAnchorAndLateStatusRecheck(t *testing.T) {
	for _, mode := range []string{"known_absent", "late_processed", "external_restoration"} {
		t.Run(mode, func(t *testing.T) {
			r, pool, withdrawID := crossMintRuntimeFixture(t)
			m := runtimeFinalizeWithdrawal(t, r, pool, withdrawID)
			id := runtimePublishSourceRecovery(t, r, pool, m)
			status := r.status.(*runtimeStatus)
			status.status = SignatureStatus{ContextSlot: 1019, BlockHeight: 5001}
			status.receipt = nil
			reader := r.accounts.(fixtureAccounts)
			reader.slot = 1019
			r.accounts = reader
			l := runtimeClaim(t, r, pool)
			if err := r.handle(context.Background(), l); err != nil {
				t.Fatal(err)
			}
			l = runtimeClaim(t, r, pool)
			if mode == "late_processed" {
				status.sequence = []SignatureStatus{status.status, {Found: true, Slot: 1018, ContextSlot: 1019, BlockHeight: 5001, Err: "processed error"}}
			}
			if mode == "external_restoration" {
				history := r.history.(addressHistory)
				history.pages[m.CustodyAccount] = append([]finalizedAddressSignature{{Signature: "external-restored", Slot: 1015, ConfirmationStatus: "finalized"}}, history.pages[m.CustodyAccount]...)
				r.history = history
			}
			err := r.handle(context.Background(), l)
			if (err == nil) != (mode == "known_absent") {
				t.Fatalf("mode=%s error=%v", mode, err)
			}
			var receipts, version int64
			var state, reservation string
			if err = pool.QueryRow(context.Background(), `SELECT s.submission_state,(SELECT count(*) FROM loyal_yield.cross_mint_no_effect_receipts WHERE submission_id=s.id),d.custody_version,r.reservation_state FROM loyal_yield.signed_route_submissions s JOIN loyal_yield.rebalance_decisions d ON d.id=s.decision_id JOIN loyal_yield.target_capacity_reservations r ON r.decision_id=d.id WHERE s.id=$1`, id).Scan(&state, &receipts, &version, &reservation); err != nil {
				t.Fatal(err)
			}
			if version != 1 || reservation != "active" || r.chain.(*runtimeSend).calls != 0 {
				t.Fatal("expiry changed custody, released capacity or broadcast")
			}
			if mode == "known_absent" {
				if state != "expired" || receipts != 1 {
					t.Fatalf("concrete no-effect not atomically terminal: %s %d", state, receipts)
				}
			} else if state != "effect_ambiguous" || receipts != 0 {
				t.Fatalf("unknown history produced replaceable generation: %s %d", state, receipts)
			}
		})
	}
}

type rolloutFactory struct{ calls int }

func (f *rolloutFactory) PrepareCrossMintLeg(context.Context, CrossMintLegRequest) (CrossMintPreparedLeg, error) {
	f.calls++
	return CrossMintPreparedLeg{}, errors.New("disabled rollout cannot prepare new leg")
}

type rolloutAdmission struct{ calls int }

func (a *rolloutAdmission) PrepareCrossMintActivation(context.Context, string) (*CrossMintActivationAdmission, error) {
	a.calls++
	return nil, nil
}

func TestCrossMintRuntimeRecoveryPrecedesAdmission(t *testing.T) {
	r, _, _ := crossMintRuntimeFixture(t)
	admission := &rolloutAdmission{}
	r.SetActivationSource(admission)
	// A pending signed family must prevent even consulting fresh admission.
	// The fixture intentionally has no controller: recovery must return first.
	if n, err := r.Tick(context.Background()); err != nil || n != 1 || admission.calls != 0 {
		t.Fatalf("recovery priority: n=%d admission=%d err=%v", n, admission.calls, err)
	}
}

func TestCrossMintRuntimeDisabledRolloutCancelsOnlyUnsignedSourceCustodyAndSkipsActivation(t *testing.T) {
	store, pool := integrationStore(t)
	ctx := context.Background()
	b := seedCrossMintMovement(t, ctx, pool) // DB start_new=true, generation1.
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		var q rpcRequest
		if err := json.NewDecoder(request.Body).Decode(&q); err != nil {
			t.Error(err)
			return
		}
		if q.Method != "getSlot" {
			t.Errorf("unexpected rollout RPC %s", q.Method)
			return
		}
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","result":777,"id":1}`))
	}))
	defer server.Close()
	adapter, err := NewRPCAdapter(server.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_, key := integrationWire(t, 112)
	factory := &rolloutFactory{}
	controller, err := NewCrossMintController(store, factory, DelegateSigner{FeePayer: ed25519.PrivateKey(key)}, adapter, b.Cluster, "rollout-owner", time.Minute, false)
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := NewCrossMintRuntime(ctx, Config{Cluster: b.Cluster, Owner: "rollout-owner", LeaseTTL: time.Minute, BatchSize: 1, TickInterval: time.Second, Facts: testFacts()}, store, controller, adapter, &runtimeVerifier{})
	if err != nil {
		t.Fatal(err)
	}
	admission := &rolloutAdmission{}
	runtime.SetActivationSource(admission)
	if n, err := runtime.Tick(ctx); err != nil || n != 1 {
		t.Fatalf("rollout cancellation: %d %v", n, err)
	}
	if n, err := runtime.Tick(ctx); err != nil || n != 0 {
		t.Fatalf("disabled idle tick: %d %v", n, err)
	}
	if factory.calls != 0 || admission.calls != 0 || calls.Load() != 1 {
		t.Fatalf("disabled configuration invoked financial work: factory=%d admission=%d slot=%d", factory.calls, admission.calls, calls.Load())
	}
	var reason, basis, state string
	if err = pool.QueryRow(ctx, `SELECT d.terminal_reason,d.terminal_evidence->>'basis',r.reservation_state FROM loyal_yield.rebalance_decisions d JOIN loyal_yield.target_capacity_reservations r ON r.decision_id=d.id WHERE d.id=$1`, b.DecisionID).Scan(&reason, &basis, &state); err != nil || reason != "start_authority_revoked_before_withdraw" || basis != "rollout_disabled" || state != "released" {
		t.Fatalf("rollout reason or release incorrect: %s %s %s %v", reason, basis, state, err)
	}
	// ANY durable signed attempt vetoes the same local configuration authority.
	b = seedCrossMintMovement(t, ctx, pool)
	l, err := store.ClaimCrossMintContinuation(ctx, b.Cluster, "rollout-owner", time.Minute)
	if err != nil || l == nil {
		t.Fatalf("claim: %v", err)
	}
	e, pre, _, _ := crossWithdrawContract()
	seedCrossMintReceiptAttempt(t, ctx, pool, b, LegWithdraw, PurposeOptimizeYield, e, pre, 778)
	if err = store.cancelUntouchedCrossMint(ctx, *l, 779, crossMintRolloutDisabled); err == nil {
		t.Fatal("local disabled config revoked existing signed bytes")
	}
	var outcome *string
	if err = pool.QueryRow(ctx, `SELECT d.terminal_outcome,r.reservation_state FROM loyal_yield.rebalance_decisions d JOIN loyal_yield.target_capacity_reservations r ON r.decision_id=d.id WHERE d.id=$1`, b.DecisionID).Scan(&outcome, &state); err != nil || outcome != nil || state != "active" {
		t.Fatalf("signed custody release: %v %s %v", outcome, state, err)
	}
}
