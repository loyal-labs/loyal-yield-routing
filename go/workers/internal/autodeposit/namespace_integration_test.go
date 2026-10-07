package autodeposit

import (
	"context"
	"encoding/base64"
	"errors"
	"testing"
	"time"
)

// A nil chain can implement no useful RPC. Any accidental mainnet read in
// these tests fails immediately instead of returning permissive fake evidence.
type namespaceNoRPC struct{ Chain }

func TestNamespaceRefusesFreshControlWork(t *testing.T) {
	for _, kind := range []string{"devnet-target", "null-target", "devnet-policy", "unknown-policy"} {
		t.Run(kind, func(t *testing.T) {
			s := integrationStore(t)
			id, reader, _ := seedControlRuntime(t, s)
			var value any = "devnet"
			if kind == "null-target" {
				value = nil
			}
			query := `UPDATE loyal_yield.balance_sweep_targets SET cluster=$2 WHERE id=$1`
			if kind == "devnet-policy" || kind == "unknown-policy" {
				query = `UPDATE loyal_yield.route_policies SET cluster=$2 WHERE id=(SELECT active_policy_id FROM loyal_yield.managed_vaults mv JOIN loyal_yield.balance_sweep_targets target ON target.settings=mv.settings AND target.vault_index=mv.vault_index AND target.vault_pubkey=mv.vault_pubkey WHERE target.id=$1)`
				if kind == "unknown-policy" {
					value = "unknown"
				}
			}
			if _, err := s.pool.Exec(t.Context(), query, id, value); err != nil {
				t.Fatal(err)
			}
			if reader.calls != 0 {
				t.Fatal("unscoped control proof reached mainnet reader")
			}
			fresh, err := s.loadFreshTargets(t.Context(), 100, nil)
			if err != nil || len(fresh) != 0 {
				t.Fatalf("foreign fresh selection=%v err=%v", fresh, err)
			}
			claim, err := s.ClaimEligibleLotsOnce(t.Context(), id, "namespace-new", nil, 9_000_000, 4_000_000, nil, nil)
			if err != nil || claim.Status != ClaimNoopStatus() {
				t.Fatalf("unscoped claim=%+v err=%v", claim, err)
			}
			if kind == "devnet-target" || kind == "null-target" {
				control, err := s.LoadControlTarget(t.Context(), id)
				if err != nil || control != nil {
					t.Fatal("foreign control target loaded")
				}
				artifact, err := s.LoadArtifactTarget(t.Context(), id)
				if err != nil || artifact != nil {
					t.Fatal("foreign artifact target loaded")
				}
				controller, err := NewController(ControllerDependencies{Store: s, Chain: namespaceNoRPC{}, Wires: &scriptedControllerWires{}})
				if err != nil {
					t.Fatal(err)
				}
				if _, err = controller.Execute(t.Context(), ExecutableTarget{TargetID: id}); !errors.Is(err, ErrChainNamespace) {
					t.Fatalf("foreign execution=%v", err)
				}
			}
		})
	}
}

func TestNamespaceSignedForeignCustodyStaysHeldWithoutRPC(t *testing.T) {
	for _, value := range []any{"devnet", nil} {
		t.Run(map[bool]string{true: "null", false: "devnet"}[value == nil], func(t *testing.T) {
			s := integrationStore(t)
			target, claim, slot := selectedReleaseClaim(t, s, "namespace-signed")
			wire := base64.StdEncoding.EncodeToString([]byte("existing-source-pull-packet"))
			prepared := PreparedAttempt{ClaimToken: claim, TargetID: target.TargetID, ScheduledSlotID: slot, OperationKind: OperationPull, AmountRaw: 5_000_000, SourcePreBalanceRaw: 9_000_000, ProtectionFloorRaw: ptrInt64(4_000_000), Signature: "namespace-existing-signature", SignedTransactionBase64: wire, SignedTransactionSHA256: wireSHA256(wire), RecentBlockhash: "namespace-blockhash", LastValidBlockHeight: 500}
			original, err := s.PersistPreparedAttempt(t.Context(), prepared, "lease-current")
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.pool.Exec(t.Context(), `UPDATE loyal_yield.balance_sweep_targets SET cluster=$2 WHERE id=$1`, target.TargetID, value); err != nil {
				t.Fatal(err)
			}
			controller, err := NewController(ControllerDependencies{Store: s, Chain: namespaceNoRPC{}, Wires: &scriptedControllerWires{}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = controller.Execute(t.Context(), ExecutableTarget{TargetID: target.TargetID, ScheduledSlotID: slot, ClaimToken: claim}); !errors.Is(err, ErrChainNamespace) {
				t.Fatalf("foreign recovery=%v", err)
			}
			adapter := durableSettlement{store: s, chain: namespaceNoRPC{}, leaseToken: "lease-current"}
			if _, err = adapter.Observe(t.Context(), original); !errors.Is(err, ErrChainNamespace) {
				t.Fatalf("foreign observe=%v", err)
			}
			if _, err = adapter.BroadcastExact(t.Context(), original); !errors.Is(err, ErrChainNamespace) {
				t.Fatalf("foreign send=%v", err)
			}
			saved, err := s.LoadLatestAttempt(t.Context(), claim, OperationPull)
			if err != nil || saved.ID != original.ID || saved.State != AttemptPrepared || saved.BroadcastCount != 0 || saved.SignedTransactionBase64 != wire {
				t.Fatalf("foreign wire changed: %+v %v", saved, err)
			}
			var state string
			if err = s.pool.QueryRow(t.Context(), `SELECT status::text FROM loyal_yield.balance_sweep_lot_claims WHERE claim_token=$1`, claim).Scan(&state); err != nil || state != "selected" {
				t.Fatalf("foreign custody released: %s %v", state, err)
			}
		})
	}
}

func TestMainnetControlIgnoresForeignNamespacesAndClosedBaseline(t *testing.T) {
	s := integrationStore(t)
	id, reader, artifacts := seedControlRuntime(t, s)
	control := &ControlReconciler{Store: s, Reader: reader, Artifacts: &ArtifactReconciler{Store: s, Reader: artifacts}}
	// Request real bootstrap proof.
	if _, err := s.EnqueueAutodepositReconciliationRequest(t.Context(), id, 200); err != nil {
		t.Fatal(err)
	}
	if worked, err := control.Tick(t.Context()); err != nil || !worked {
		t.Fatalf("control tick worked=%v err=%v", worked, err)
	}
	// Control reconciliation can publish confirmed mainnet wallet events.
	if _, err := s.ProjectSurplusLotsOnce(t.Context(), 1000); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"closed", "devnet", "null"} {
		other := seedIntegrationTarget(t, s, "health-unrelated-"+kind)
		if kind == "closed" {
			if _, err := s.pool.Exec(t.Context(), `UPDATE loyal_yield.balance_sweep_targets SET chain_status='closed' WHERE id=$1`, other.TargetID); err != nil {
				t.Fatal(err)
			}
			if _, err := s.pool.Exec(t.Context(), `DELETE FROM loyal_yield.autodeposit_reconciliation_requests WHERE target_id=$1`, other.TargetID); err != nil {
				t.Fatal(err)
			}
		} else {
			var cluster any = "devnet"
			if kind == "null" {
				cluster = nil
			}
			if _, err := s.pool.Exec(t.Context(), `UPDATE loyal_yield.balance_sweep_targets SET cluster=$2 WHERE id=$1`, other.TargetID, cluster); err != nil {
				t.Fatal(err)
			}
			if _, err := s.EnqueueAutodepositReconciliationRequest(t.Context(), other.TargetID, 999); err != nil {
				t.Fatal(err)
			}
			s.insertIntegrationEvent(t, other.TargetID, 1000+other.TargetID, 9_000_000, nil, time.Now())
		}
	}
	if request, err := s.ClaimAutodepositReconciliationRequest(t.Context(), "mainnet-health-reader", 30); err != nil || request != nil {
		t.Fatalf("foreign request claimed=%v %v", request, err)
	}
}

func TestNamespaceChangedInsideBuilderCannotPublishPull(t *testing.T) {
	for _, value := range []any{"devnet", nil} {
		t.Run(map[bool]string{true: "null", false: "devnet"}[value == nil], func(t *testing.T) {
			s := integrationStore(t)
			id, _, _ := seedControlRuntime(t, s)
			target, err := s.LoadArtifactTarget(t.Context(), id)
			if err != nil {
				t.Fatal(err)
			}
			builder := &controlsChangingPullBuilder{scriptedControllerWires: &scriptedControllerWires{suffix: "-namespace"}, change: func() {
				if _, err := s.pool.Exec(t.Context(), `UPDATE loyal_yield.balance_sweep_targets SET cluster=$2 WHERE id=$1`, id, value); err != nil {
					t.Fatal(err)
				}
			}}
			chain := &scriptedControllerChain{balances: map[string]int64{target.WalletTokenATA: 9_000_000, target.VaultTokenATA: 0}}
			controller, err := NewController(ControllerDependencies{Store: s, Chain: chain, Wires: builder})
			if err != nil {
				t.Fatal(err)
			}
			fresh, err := s.loadFreshTargets(t.Context(), 1, nil)
			if err != nil || len(fresh) != 1 {
				t.Fatalf("fresh=%v %v", fresh, err)
			}
			if _, err = controller.Execute(t.Context(), fresh[0]); !errors.Is(err, ErrChainNamespace) {
				t.Fatalf("stale builder namespace=%v", err)
			}
			var attempts int
			if err = s.pool.QueryRow(t.Context(), `SELECT count(*) FROM loyal_yield.balance_sweep_transaction_attempts WHERE target_id=$1`, id).Scan(&attempts); err != nil {
				t.Fatal(err)
			}
			if attempts != 0 || chain.balances[target.WalletTokenATA] != 9_000_000 {
				t.Fatal("foreign builder packet escaped atomic namespace gate")
			}
		})
	}
}

func TestNamespaceChangedBeforeSetupPersistenceHoldsNewPacket(t *testing.T) {
	for _, value := range []any{"devnet", nil} {
		t.Run(map[bool]string{true: "null", false: "devnet"}[value == nil], func(t *testing.T) {
			s := integrationStore(t)
			target, claim, _ := selectedReleaseClaim(t, s, "namespace-setup")
			builder, plan, setup, _ := setupFixture(t, SetupATA)
			plan.Target.ID, plan.Target.ManagedVaultID, plan.AmountRaw = target.TargetID, target.ManagedVaultID, 5_000_000
			if _, err := s.FreezeDepositPlan(t.Context(), claim, "lease-current", plan); err != nil {
				t.Fatal(err)
			}
			wire, err := builder.BuildDestinationSetup(t.Context(), plan, setup, fixedKey("namespace-setup-hash"), 900)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.pool.Exec(t.Context(), `UPDATE loyal_yield.balance_sweep_targets SET cluster=$2 WHERE id=$1`, target.TargetID, value); err != nil {
				t.Fatal(err)
			}
			if _, err = s.PersistDestinationSetup(t.Context(), claim, "lease-current", setup, wire); !errors.Is(err, ErrChainNamespace) {
				t.Fatalf("foreign setup publication=%v", err)
			}
			var attempts int
			if err = s.pool.QueryRow(t.Context(), `SELECT count(*) FROM loyal_yield.balance_sweep_destination_setup_attempts WHERE claim_token=$1`, claim).Scan(&attempts); err != nil || attempts != 0 {
				t.Fatalf("setup journal=%d %v", attempts, err)
			}
		})
	}
}

func TestControlConfirmedSameSlotConflictDoesNotOverwriteWallet(t *testing.T) {
	s := integrationStore(t)
	id, reader, _ := seedControlRuntime(t, s)
	target, err := s.LoadControlTarget(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	o, err := reader.ObserveControl(t.Context(), *target, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(t.Context(), `UPDATE loyal_yield.balance_sweep_wallet_balances_current SET observed_slot=$2,amount_raw=8000000,source_commitment='confirmed' WHERE target_id=$1`, id, o.ObservedSlot); err != nil {
		t.Fatal(err)
	}
	if _, err = s.EnqueueAutodepositReconciliationRequest(t.Context(), id, o.ObservedSlot); err != nil {
		t.Fatal(err)
	}
	request, err := s.ClaimAutodepositReconciliationRequest(t.Context(), "same-bank-reader", 30)
	if err != nil || request == nil {
		t.Fatalf("claim=%v %v", request, err)
	}
	if err = s.ApplyControlObservation(t.Context(), *request, "same-bank-reader", o); err == nil {
		t.Fatal("contradictory confirmed bank applied")
	}
	var amount, processed int64
	if err = s.pool.QueryRow(t.Context(), `SELECT wallet.amount_raw,request.processed_slot FROM loyal_yield.balance_sweep_wallet_balances_current wallet JOIN loyal_yield.autodeposit_reconciliation_requests request ON request.target_id=wallet.target_id WHERE wallet.target_id=$1`, id).Scan(&amount, &processed); err != nil {
		t.Fatal(err)
	}
	if amount != 8_000_000 || processed != 0 {
		t.Fatalf("confirmed contradiction overwritten: amount=%d processed=%d", amount, processed)
	}
}

var _ Chain = namespaceNoRPC{}

func TestMainnetStaleSchedulingDoesNotMutateForeignUnsignedWork(t *testing.T) {
	for _, value := range []any{"devnet", nil} {
		t.Run(map[bool]string{true: "null", false: "devnet"}[value == nil], func(t *testing.T) {
			s := integrationStore(t)
			target, claim, _ := selectedReleaseClaim(t, s, "foreign-unsigned-schedule")
			if _, err := s.pool.Exec(t.Context(), `UPDATE loyal_yield.balance_sweep_targets SET cluster=$2 WHERE id=$1`, target.TargetID, value); err != nil {
				t.Fatal(err)
			}
			if _, err := s.pool.Exec(t.Context(), `UPDATE loyal_yield.balance_sweep_lot_claims SET updated_at=now()-interval '1 day',autodeposit_executor_lease_expires_at=now()-interval '1 second' WHERE claim_token=$1`, claim); err != nil {
				t.Fatal(err)
			}
			var requested int64
			if err := s.pool.QueryRow(t.Context(), `INSERT INTO loyal_yield.balance_sweep_scheduled_slots(target_id,token_mint,status,eligible_after,requested_at) VALUES($1,$2,'requested',now()-interval '1 day',now()-interval '1 day') RETURNING id`, target.TargetID, USDCMint).Scan(&requested); err != nil {
				t.Fatal(err)
			}
			if affected, err := s.FailStaleRequestedSlots(t.Context(), 100); err != nil || affected != 0 {
				t.Fatalf("foreign requested rows failed=%d %v", affected, err)
			}
			if affected, err := s.ReleaseStaleSelectedClaims(t.Context(), 60, 100); err != nil || affected != 0 {
				t.Fatalf("foreign claims released=%d %v", affected, err)
			}
			var requestedState, claimState string
			if err := s.pool.QueryRow(t.Context(), `SELECT slot.status::text,claim.status::text FROM loyal_yield.balance_sweep_scheduled_slots slot CROSS JOIN loyal_yield.balance_sweep_lot_claims claim WHERE slot.id=$1 AND claim.claim_token=$2`, requested, claim).Scan(&requestedState, &claimState); err != nil {
				t.Fatal(err)
			}
			if requestedState != "requested" || claimState != "selected" {
				t.Fatalf("foreign scheduling touched: %s %s", requestedState, claimState)
			}
		})
	}
}

type controlsChangingPullBuilder struct {
	*scriptedControllerWires
	change func()
}

func (b *controlsChangingPullBuilder) BuildPull(ctx context.Context, request PullWireRequest) (BuiltWire, error) {
	wire, err := b.scriptedControllerWires.BuildPull(ctx, request)
	b.change()
	return wire, err
}
