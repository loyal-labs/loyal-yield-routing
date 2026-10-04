package autodeposit

import (
	"context"
	"errors"
	"testing"
	"time"
)

type desiredControlReader struct {
	reader ControlReader
	after  func()
	calls  int
}

func (r *desiredControlReader) ObserveControl(ctx context.Context, t ControlTarget, slot int64) (ControlObservation, error) {
	r.calls++
	o, err := r.reader.ObserveControl(ctx, t, slot)
	if r.after != nil {
		r.after()
	}
	return o, err
}

type desiredCreatorHistory struct {
	entries  map[string]ArtifactHistoryEntry
	receipts map[string]ArtifactReceipt
}

func (h desiredCreatorHistory) ArtifactHistory(_ context.Context, address string, _ int) ([]ArtifactHistoryEntry, error) {
	return []ArtifactHistoryEntry{h.entries[address]}, nil
}
func (h desiredCreatorHistory) ArtifactReceipt(_ context.Context, signature string) (ArtifactReceipt, error) {
	receipt, ok := h.receipts[signature]
	if !ok {
		return receipt, ErrArtifactCreationProofPending
	}
	return receipt, nil
}

func seedDesiredRuntime(t *testing.T, s *Store) (int64, *DesiredReconciler, *desiredControlReader) {
	t.Helper()
	f, target, b := artifactFixture(t)
	installArtifactSnapshot(t, f, b)
	seeded := seedIntegrationTarget(t, s, "desired")
	target.TargetID = seeded.TargetID
	seedProjectedSurplus(t, s, seeded, 1, 9_000_000)
	_, err := s.pool.Exec(t.Context(), `UPDATE loyal_yield.balance_sweep_targets SET settings=$2,authority=$3,policy_seed=$4,policy_account=$5,wallet=$6,wallet_usdc_ata=$7,wallet_token_ata=$7,vault_pubkey=$8,vault_usdc_ata=$9,vault_token_ata=$9,subscription_authority=$10,recurring_delegation=$11,recurring_delegation_nonce=$12,max_amount_per_period=$13,period_length_seconds=$14,start_timestamp=$15,recurring_delegation_expiry_timestamp=$16,setup_generation=$17 WHERE id=$1`, target.TargetID, target.Settings, target.RootAuthority, target.PolicySeed, target.Policy, target.Wallet, target.WalletTokenATA, target.Vault, target.VaultTokenATA, target.SubscriptionAuthority, target.RecurringDelegation, *target.Nonce, *target.MaxAmountPerPeriod, *target.PeriodLength, *target.StartTimestamp, *target.ExpiryTimestamp, target.SetupGeneration)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(t.Context(), `UPDATE loyal_yield.managed_vaults SET settings=$2,vault_pubkey=$3 WHERE id=$1`, seeded.ManagedVaultID, target.Settings, target.Vault); err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(t.Context(), `UPDATE loyal_yield.route_policies SET settings=$2,authority=$3,vault_pubkey=$4 WHERE managed_vault_id=$1 OR id=(SELECT active_policy_id FROM loyal_yield.managed_vaults WHERE id=$1)`, seeded.ManagedVaultID, target.Settings, target.RootAuthority, target.Vault); err != nil {
		t.Fatal(err)
	}
	seedRepairPosition(t, s, target.TargetID, USDCMint)
	policy := goldenCreatorReceipt(t, f)
	f.WireBase64 = f.DelegationWireBase64
	f.Policy = f.RecurringDelegation
	delegation := goldenCreatorReceipt(t, f)
	history := desiredCreatorHistory{entries: map[string]ArtifactHistoryEntry{target.Policy: {Signature: policy.Signature, Slot: policy.Slot}, target.RecurringDelegation: {Signature: delegation.Signature, Slot: delegation.Slot}}, receipts: map[string]ArtifactReceipt{policy.Signature: policy, delegation.Signature: delegation}}
	reader := &desiredControlReader{reader: b}
	r := &DesiredReconciler{Store: s, Reader: reader, Artifacts: &ArtifactProofReader{Wires: b, History: history}}
	return target.TargetID, r, reader
}

func assertDesiredApplied(t *testing.T, s *Store, id int64, wantAmount int64, eligible bool) {
	t.Helper()
	var desired, applied, pending, amount int64
	var actualEligible bool
	err := s.pool.QueryRow(t.Context(), `SELECT target.desired_revision,target.applied_desired_revision,target.applied_scheduling_eligible,request.requested_generation-request.processed_generation,COALESCE((SELECT sum(remaining_amount_raw) FROM loyal_yield.balance_sweep_surplus_lots WHERE target_id=target.id AND status='open'),0)::bigint FROM loyal_yield.balance_sweep_targets target JOIN loyal_yield.autodeposit_desired_control_requests request ON request.target_id=target.id WHERE target.id=$1`, id).Scan(&desired, &applied, &actualEligible, &pending, &amount)
	if err != nil {
		t.Fatal(err)
	}
	if desired != applied || pending != 0 || actualEligible != eligible || amount != wantAmount {
		t.Fatalf("desired state rev=%d/%d pending=%d eligible=%v lots=%d", desired, applied, pending, actualEligible, amount)
	}
}

func TestDesiredFloorRebaselineWithoutAppReadsUsesDemandClock(t *testing.T) {
	s := integrationStore(t)
	id, r, _ := seedDesiredRuntime(t, s)
	if err := s.RequireDesiredSchema(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(t.Context(), `UPDATE loyal_yield.balance_sweep_targets SET wallet_balance_floor_raw=2000000 WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	var requestTime time.Time
	if err := s.pool.QueryRow(t.Context(), `SELECT requested_at FROM loyal_yield.autodeposit_desired_control_requests WHERE target_id=$1`, id).Scan(&requestTime); err != nil {
		t.Fatal(err)
	}
	coalesced := requestTime.Add(2 * time.Hour)
	if _, err := s.pool.Exec(t.Context(), `UPDATE loyal_yield.balance_sweep_scheduled_slots SET eligible_after=$2,status='requested',requested_at=$3,request_source='mobile_execute_now' WHERE target_id=$1`, id, coalesced, requestTime); err != nil {
		t.Fatal(err)
	}
	if worked, err := r.Tick(t.Context()); err != nil || !worked {
		t.Fatalf("desired tick: %v %v", worked, err)
	}
	assertDesiredApplied(t, s, id, 7_000_000, true)
	var due, clock time.Time
	var source, state string
	var slot int64
	if err := s.pool.QueryRow(t.Context(), `SELECT lot.eligible_after,event.observed_at,event.source,event.observed_slot,slot.status::text FROM loyal_yield.balance_sweep_surplus_lots lot JOIN loyal_yield.balance_sweep_wallet_balance_events event ON event.event_id=lot.source_event_id JOIN loyal_yield.balance_sweep_scheduled_slots slot ON slot.id=lot.scheduled_slot_id WHERE lot.target_id=$1 AND lot.status='open'`, id).Scan(&due, &clock, &source, &slot, &state); err != nil {
		t.Fatal(err)
	}
	if !due.Equal(coalesced) || clock.Before(requestTime) || source != "go_autodeposit_desired_rebaseline" || slot != 200 || state != "requested" {
		t.Fatalf("lost clocks/request/deadline: %s %s %s %d %s", due, clock, source, slot, state)
	}
	if worked, err := r.Tick(t.Context()); err != nil || worked {
		t.Fatalf("replayed desired work: %v %v", worked, err)
	}
	assertDesiredApplied(t, s, id, 7_000_000, true)
}

func TestDesiredRevisionRaisedDuringProofRemainsPending(t *testing.T) {
	s := integrationStore(t)
	id, r, reader := seedDesiredRuntime(t, s)
	reader.after = func() {
		if _, err := s.pool.Exec(t.Context(), `UPDATE loyal_yield.balance_sweep_targets SET wallet_balance_floor_raw=3000000 WHERE id=$1`, id); err != nil {
			t.Fatal(err)
		}
	}
	if worked, err := r.Tick(t.Context()); err == nil || !worked {
		t.Fatalf("stale desired proof accepted: %v %v", worked, err)
	}
	var pending, lots int64
	if err := s.pool.QueryRow(t.Context(), `SELECT requested_generation-processed_generation FROM loyal_yield.autodeposit_desired_control_requests WHERE target_id=$1`, id).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(t.Context(), `SELECT sum(remaining_amount_raw)::bigint FROM loyal_yield.balance_sweep_surplus_lots WHERE target_id=$1 AND status='open'`, id).Scan(&lots); err != nil {
		t.Fatal(err)
	}
	if pending <= 0 || lots != 5_000_000 {
		t.Fatalf("stale proof mutated/acknowledged new demand: %d %d", pending, lots)
	}
	reader.after = nil
	if worked, err := r.Tick(t.Context()); err != nil || !worked {
		t.Fatalf("new revision did not converge: %v %v", worked, err)
	}
	assertDesiredApplied(t, s, id, 6_000_000, true)
}

func TestDesiredPauseAndResumeIncludingFundingWithoutClients(t *testing.T) {
	s := integrationStore(t)
	id, r, reader := seedDesiredRuntime(t, s)
	if _, err := r.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(t.Context(), `UPDATE loyal_yield.balance_sweep_targets SET desired_active=false WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	calls := reader.calls
	if _, err := r.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if reader.calls != calls {
		t.Fatal("pause required an active bank")
	}
	assertDesiredApplied(t, s, id, 0, false)
	// Resume's confirmed bank contains the existing funded balance. No new
	// wallet event or GET is needed to re-establish its unreserved surplus.
	if _, err := s.pool.Exec(t.Context(), `UPDATE loyal_yield.balance_sweep_targets SET desired_active=true WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertDesiredApplied(t, s, id, 5_000_000, true)
}

func TestDesiredMissingPositionReturnAndUnknownFloorHold(t *testing.T) {
	s := integrationStore(t)
	id, r, _ := seedDesiredRuntime(t, s)
	if _, err := s.pool.Exec(t.Context(), `UPDATE loyal_yield.user_yield_positions SET status='closed'`); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Tick(t.Context()); err == nil {
		t.Fatal("missing position acknowledged")
	}
	if _, err := s.pool.Exec(t.Context(), `UPDATE loyal_yield.user_yield_positions SET status='active'; UPDATE loyal_yield.autodeposit_desired_control_requests SET next_attempt_at=now()`); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertDesiredApplied(t, s, id, 5_000_000, true)
	if _, err := s.pool.Exec(t.Context(), `UPDATE loyal_yield.balance_sweep_targets SET wallet_balance_floor_raw=NULL WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Tick(t.Context()); err == nil {
		t.Fatal("unknown floor acknowledged as zero")
	}
}

func TestDesiredActiveRebaselineCannotReleaseSelectedCustody(t *testing.T) {
	s := integrationStore(t)
	id, r, _ := seedDesiredRuntime(t, s)
	var slot int64
	if err := s.pool.QueryRow(t.Context(), `SELECT id FROM loyal_yield.balance_sweep_scheduled_slots WHERE target_id=$1`, id).Scan(&slot); err != nil {
		t.Fatal(err)
	}
	cap := int64(100_000_000)
	if claim, err := s.ClaimEligibleLotsOnce(t.Context(), id, "desired-held", &slot, 9_000_000, 4_000_000, &cap, &cap); err != nil || claim.Status != ClaimSelected {
		t.Fatalf("claim: %+v %v", claim, err)
	}
	if _, err := r.Tick(t.Context()); !errors.Is(err, ErrClaimCustodyHeld) {
		t.Fatalf("selected custody rebaselined: %v", err)
	}
	assertReleaseCustodyUnchanged(t, s, "desired-held")
	if _, err := s.pool.Exec(t.Context(), `UPDATE loyal_yield.balance_sweep_targets SET desired_active=false WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertReleaseCustodyUnchanged(t, s, "desired-held")
}

func TestDesiredAdmissionWaitsForRebaselineThenCompletes(t *testing.T) {
	s := integrationStore(t)
	id, r, _ := seedDesiredRuntime(t, s)
	s.EnableDesiredControlAdmission()
	target, err := s.LoadArtifactTarget(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	pull, topup := "itest-controller-pull-sig-desired-gate", "itest-controller-topup-sig-desired-gate"
	wallet, custody := target.WalletTokenATA, target.VaultTokenATA
	chain := &scriptedControllerChain{balances: map[string]int64{wallet: 9_000_000, custody: 0}, observations: map[string]AttemptObservation{pull: {State: AttemptConfirmed, ConfirmedSlot: ptrInt64(870001)}, topup: {State: AttemptConfirmed, ConfirmedSlot: ptrInt64(870002)}}, receipts: map[string]ReceiptEvidence{
		pull:  {Signature: pull, Slot: 870001, Effects: []ReceiptEffect{{TokenAccount: wallet, Mint: USDCMint, PreRaw: 9_000_000, PostRaw: 4_000_000}, {TokenAccount: custody, Mint: USDCMint, PreRaw: 0, PostRaw: 5_000_000}}},
		topup: {Signature: topup, Slot: 870002, Effects: []ReceiptEffect{{TokenAccount: custody, Mint: USDCMint, PreRaw: 5_000_000, PostRaw: 0}, {TokenAccount: "itest-liquidity-supply", Mint: USDCMint, PreRaw: 10, PostRaw: 5_000_010}}}}, positions: map[string][2]int64{"repair-reserve": {5_000_001, 870002}}}
	wires := &scriptedControllerWires{suffix: "-desired-gate"}
	controller, err := NewController(ControllerDependencies{Store: s, Chain: chain, Wires: wires})
	if err != nil {
		t.Fatal(err)
	}
	w, err := NewWorker(WorkerDependencies{Store: s, Executor: controller})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = w.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(wires.built) != 0 {
		t.Fatal("pending intent signed a pull")
	}
	// Retained demand fixture is old enough to be due. The engine must use
	// this source demand time, rather than restarting its hour at processing.
	if _, err = s.pool.Exec(t.Context(), `UPDATE loyal_yield.autodeposit_desired_control_requests SET requested_at=now()-interval '2 hours' WHERE target_id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err = r.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	report, err := w.Tick(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if report.Outcome.ExecutionsCompleted != 1 || len(wires.built) != 2 {
		t.Fatalf("applied mature intent failed to complete: %+v %v", report, wires.built)
	}
}

func TestDesiredSameRevisionNewGenerationInvalidatesOldProof(t *testing.T) {
	s := integrationStore(t)
	id, r, reader := seedDesiredRuntime(t, s)
	reader.after = func() {
		if _, err := s.pool.Exec(t.Context(), `UPDATE loyal_yield.autodeposit_desired_control_requests SET requested_generation=requested_generation+1,requested_at=clock_timestamp() WHERE target_id=$1`, id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.Tick(t.Context()); err == nil {
		t.Fatal("old generation proof applied")
	}
	var applied, processed, events int64
	if err := s.pool.QueryRow(t.Context(), `SELECT applied_desired_revision,(SELECT processed_generation FROM loyal_yield.autodeposit_desired_control_requests WHERE target_id=$1),(SELECT count(*) FROM loyal_yield.balance_sweep_wallet_balance_events WHERE target_id=$1 AND source='go_autodeposit_desired_rebaseline') FROM loyal_yield.balance_sweep_targets WHERE id=$1`, id).Scan(&applied, &processed, &events); err != nil {
		t.Fatal(err)
	}
	if applied != 0 || processed != 0 || events != 0 {
		t.Fatalf("old demand clock published: applied=%d processed=%d events=%d", applied, processed, events)
	}
	reader.after = nil
	if _, err := r.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertDesiredApplied(t, s, id, 5_000_000, true)
}

func TestDesiredPolicyReturnWakesReconciliationWithoutClient(t *testing.T) {
	s := integrationStore(t)
	id, r, reader := seedDesiredRuntime(t, s)
	var policy, revision int64
	if err := s.pool.QueryRow(t.Context(), `SELECT mv.active_policy_id,target.desired_revision FROM loyal_yield.balance_sweep_targets target JOIN loyal_yield.managed_vaults mv ON mv.settings=target.settings AND mv.vault_index=target.vault_index AND mv.vault_pubkey=target.vault_pubkey WHERE target.id=$1`, id).Scan(&policy, &revision); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(t.Context(), `UPDATE loyal_yield.route_policies SET active=false WHERE id=$1`, policy); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Tick(t.Context()); err == nil {
		t.Fatal("missing active policy acknowledged")
	}
	if reader.calls != 0 {
		t.Fatal("ineligible policy caused an active-bank read")
	}
	if _, err := s.pool.Exec(t.Context(), `UPDATE loyal_yield.autodeposit_desired_control_requests SET next_attempt_at=now()+interval '1 day' WHERE target_id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(t.Context(), `UPDATE loyal_yield.route_policies SET active=true,last_seen_at=clock_timestamp() WHERE id=$1`, policy); err != nil {
		t.Fatal(err)
	}
	if worked, err := r.Tick(t.Context()); err != nil || !worked {
		t.Fatalf("policy return stayed on old backoff: %v %v", worked, err)
	}
	assertDesiredApplied(t, s, id, 5_000_000, true)
	var current int64
	if err := s.pool.QueryRow(t.Context(), `SELECT desired_revision FROM loyal_yield.balance_sweep_targets WHERE id=$1`, id).Scan(&current); err != nil {
		t.Fatal(err)
	}
	if current != revision || reader.calls != 1 {
		t.Fatalf("policy return fabricated intent or proof: revision=%d/%d reads=%d", current, revision, reader.calls)
	}
}

func TestDesiredProofRechecksLeaseAndWalletProjection(t *testing.T) {
	for _, kind := range []string{"displaced", "expired", "newer-wallet", "finalized-contradiction", "confirmed-contradiction", "confirmed-hash-contradiction"} {
		t.Run(kind, func(t *testing.T) {
			s := integrationStore(t)
			id, r, reader := seedDesiredRuntime(t, s)
			reader.after = func() {
				var err error
				switch kind {
				case "displaced":
					_, err = s.pool.Exec(t.Context(), `UPDATE loyal_yield.autodeposit_desired_control_requests SET claim_owner='other-reader' WHERE target_id=$1`, id)
				case "expired":
					_, err = s.pool.Exec(t.Context(), `UPDATE loyal_yield.autodeposit_desired_control_requests SET claim_expires_at=now()-interval '1 second' WHERE target_id=$1`, id)
				case "newer-wallet":
					_, err = s.pool.Exec(t.Context(), `UPDATE loyal_yield.balance_sweep_wallet_balances_current SET observed_slot=201 WHERE target_id=$1`, id)
				case "confirmed-contradiction":
					_, err = s.pool.Exec(t.Context(), `UPDATE loyal_yield.balance_sweep_wallet_balances_current SET observed_slot=200,amount_raw=8000000,source_commitment='confirmed' WHERE target_id=$1`, id)
				case "confirmed-hash-contradiction":
					_, err = s.pool.Exec(t.Context(), `UPDATE loyal_yield.balance_sweep_wallet_balances_current SET observed_slot=200,amount_raw=9000000,account_data_hash=repeat('0',64),source_commitment='confirmed' WHERE target_id=$1`, id)
				case "finalized-contradiction":
					_, err = s.pool.Exec(t.Context(), `UPDATE loyal_yield.balance_sweep_wallet_balances_current SET observed_slot=200,amount_raw=8000000,source_commitment='finalized' WHERE target_id=$1`, id)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, err := r.Tick(t.Context()); err == nil {
				t.Fatal("stale proof applied")
			}
			var amount, applied int64
			if err := s.pool.QueryRow(t.Context(), `SELECT (SELECT sum(remaining_amount_raw)::bigint FROM loyal_yield.balance_sweep_surplus_lots WHERE target_id=$1 AND status='open'),applied_desired_revision FROM loyal_yield.balance_sweep_targets WHERE id=$1`, id).Scan(&amount, &applied); err != nil {
				t.Fatal(err)
			}
			if amount != 5_000_000 || applied != 0 {
				t.Fatalf("failed proof changed scheduling: %d applied=%d", amount, applied)
			}
		})
	}
}

func TestDesiredConfirmedCloseRetainsIntentAndCustody(t *testing.T) {
	s := integrationStore(t)
	id, r, reader := seedDesiredRuntime(t, s)
	if _, err := s.pool.Exec(t.Context(), `UPDATE loyal_yield.balance_sweep_targets SET chain_status='closed',chain_observation_slot=200 WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if reader.calls != 0 {
		t.Fatal("verified closed state required an active bank")
	}
	assertDesiredApplied(t, s, id, 0, false)
	var enabled bool
	if err := s.pool.QueryRow(t.Context(), `SELECT desired_active FROM loyal_yield.balance_sweep_targets WHERE id=$1`, id).Scan(&enabled); err != nil {
		t.Fatal(err)
	}
	if !enabled {
		t.Fatal("chain close rewrote desired intent")
	}
}

func TestDesiredEligibilityReturnsAtSameRevisionWithoutClientOrBackoffWait(t *testing.T) {
	s := integrationStore(t)
	id, r, _ := seedDesiredRuntime(t, s)
	if _, err := r.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	var revision int64
	if err := s.pool.QueryRow(t.Context(), `SELECT desired_revision FROM loyal_yield.balance_sweep_targets WHERE id=$1`, id).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(t.Context(), `UPDATE loyal_yield.user_yield_positions SET status='closed',updated_at=clock_timestamp()`); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Tick(t.Context()); err == nil {
		t.Fatal("missing position acknowledged")
	}
	if _, err := s.pool.Exec(t.Context(), `UPDATE loyal_yield.user_yield_positions SET status='active',updated_at=clock_timestamp()`); err != nil {
		t.Fatal(err)
	}
	if worked, err := r.Tick(t.Context()); err != nil || !worked {
		t.Fatalf("actual position return did not wake pending generation: %v %v", worked, err)
	}
	assertDesiredApplied(t, s, id, 5_000_000, true)
	var actual int64
	if err := s.pool.QueryRow(t.Context(), `SELECT desired_revision FROM loyal_yield.balance_sweep_targets WHERE id=$1`, id).Scan(&actual); err != nil {
		t.Fatal(err)
	}
	if actual != revision {
		t.Fatal("position recovery rewrote user intent revision")
	}
}
