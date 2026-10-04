package autodeposit

import (
	"context"
	"encoding/base64"
	"errors"
	"testing"
)

func TestDesiredCapturedRevisionCannotPublishAfterNewRevisionApplied(t *testing.T) {
	for _, kind := range []string{"pull", "setup"} {
		t.Run(kind, func(t *testing.T) {
			s := integrationStore(t)
			id, r, _ := seedDesiredRuntime(t, s)
			s.EnableDesiredControlAdmission()
			var originalRevision int64
			if err := s.pool.QueryRow(t.Context(), `SELECT desired_revision FROM loyal_yield.balance_sweep_targets WHERE id=$1`, id).Scan(&originalRevision); err != nil {
				t.Fatal(err)
			}
			if _, err := s.pool.Exec(t.Context(), `UPDATE loyal_yield.balance_sweep_targets SET wallet_balance_floor_raw=5000000 WHERE id=$1`, id); err != nil {
				t.Fatal(err)
			}
			if _, err := s.pool.Exec(t.Context(), `UPDATE loyal_yield.balance_sweep_targets SET wallet_balance_floor_raw=4000000 WHERE id=$1`, id); err != nil {
				t.Fatal(err)
			}
			if _, err := s.pool.Exec(t.Context(), `UPDATE loyal_yield.autodeposit_desired_control_requests SET requested_at=now()-interval '2 hours' WHERE target_id=$1`, id); err != nil {
				t.Fatal(err)
			}
			if _, err := r.Tick(t.Context()); err != nil {
				t.Fatal(err)
			}
			assertDesiredApplied(t, s, id, 5_000_000, true)
			fresh, err := s.loadFreshTargets(t.Context(), 1, nil)
			if err != nil || len(fresh) != 1 {
				t.Fatalf("fresh=%v %v", fresh, err)
			}
			claim := "captured-revision-claim"
			selected, err := s.ClaimEligibleLotsOnce(t.Context(), id, claim, &fresh[0].ScheduledSlotID, 9_000_000, 4_000_000, nil, nil)
			if err != nil || selected.Status != ClaimSelected {
				t.Fatalf("selected=%+v %v", selected, err)
			}
			if owned, err := s.AcquireClaimLease(t.Context(), claim, id, "captured-revision-lease"); err != nil || !owned {
				t.Fatalf("lease=%v %v", owned, err)
			}
			current, err := s.LoadTargetExecutionContext(t.Context(), id)
			if err != nil || current.DesiredRevision == originalRevision {
				t.Fatalf("current=%+v %v", current, err)
			}
			if kind == "pull" {
				wire := base64.StdEncoding.EncodeToString([]byte("captured-old-revision-wire"))
				prepared := PreparedAttempt{ClaimToken: claim, TargetID: id, ScheduledSlotID: fresh[0].ScheduledSlotID, OperationKind: OperationPull, AmountRaw: 5_000_000, SourcePreBalanceRaw: 9_000_000, ProtectionFloorRaw: ptrInt64(4_000_000), SourceDesiredRevision: originalRevision, Signature: "captured-revision-wire", SignedTransactionBase64: wire, SignedTransactionSHA256: wireSHA256(wire), RecentBlockhash: "captured-revision-blockhash", LastValidBlockHeight: 500}
				if _, err = s.PersistPreparedAttempt(t.Context(), prepared, "captured-revision-lease"); !errors.Is(err, ErrDesiredControlsPending) {
					t.Fatalf("old pull revision=%v", err)
				}
				prepared.SourceDesiredRevision = current.DesiredRevision
				if _, err = s.PersistPreparedAttempt(t.Context(), prepared, "captured-revision-lease"); err != nil {
					t.Fatal(err)
				}
			} else {
				builder, plan, setup, _ := setupFixture(t, SetupATA)
				plan.Target.ID, plan.Target.ManagedVaultID, plan.AmountRaw = id, current.RoutePolicy.ManagedVaultID, selected.AmountRaw
				if _, err = s.FreezeDepositPlan(t.Context(), claim, "captured-revision-lease", plan); err != nil {
					t.Fatal(err)
				}
				wire, err := builder.BuildDestinationSetup(t.Context(), plan, setup, fixedKey("captured-revision-setup"), 900)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = s.PersistDestinationSetup(t.Context(), claim, "captured-revision-lease", setup, wire, originalRevision); !errors.Is(err, ErrDesiredControlsPending) {
					t.Fatalf("old setup revision=%v", err)
				}
				if _, err = s.PersistDestinationSetup(t.Context(), claim, "captured-revision-lease", setup, wire, current.DesiredRevision); err != nil {
					t.Fatal(err)
				}
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

func TestDesiredControlsABAInsideBuilderCannotPublishNewPull(t *testing.T) {
	for _, kind := range []string{"pause-resume", "floor-restored", "cluster-restored", "delegation-restored", "period-restored"} {
		t.Run(kind, func(t *testing.T) {
			s := integrationStore(t)
			id, r, _ := seedDesiredRuntime(t, s)
			s.EnableDesiredControlAdmission()
			if _, err := s.pool.Exec(t.Context(), `UPDATE loyal_yield.autodeposit_desired_control_requests SET requested_at=now()-interval '2 hours' WHERE target_id=$1`, id); err != nil {
				t.Fatal(err)
			}
			if _, err := r.Tick(t.Context()); err != nil {
				t.Fatal(err)
			}
			target, err := s.LoadArtifactTarget(t.Context(), id)
			if err != nil {
				t.Fatal(err)
			}
			base := &scriptedControllerWires{suffix: "-control-aba"}
			builder := &controlsChangingPullBuilder{scriptedControllerWires: base, change: func() {
				var first, second string
				switch kind {
				case "pause-resume":
					first = `UPDATE loyal_yield.balance_sweep_targets SET desired_active=false WHERE id=$1`
					second = `UPDATE loyal_yield.balance_sweep_targets SET desired_active=true WHERE id=$1`
				case "floor-restored":
					first = `UPDATE loyal_yield.balance_sweep_targets SET wallet_balance_floor_raw=5000000 WHERE id=$1`
					second = `UPDATE loyal_yield.balance_sweep_targets SET wallet_balance_floor_raw=4000000 WHERE id=$1`
				case "cluster-restored":
					first = `UPDATE loyal_yield.balance_sweep_targets SET cluster='devnet' WHERE id=$1`
					second = `UPDATE loyal_yield.balance_sweep_targets SET cluster='mainnet-beta' WHERE id=$1`
				case "delegation-restored":
					first = `UPDATE loyal_yield.balance_sweep_targets SET recurring_delegation=recurring_delegation||'-temporary' WHERE id=$1`
					second = `UPDATE loyal_yield.balance_sweep_targets SET recurring_delegation=replace(recurring_delegation,'-temporary','') WHERE id=$1`
				case "period-restored":
					first = `UPDATE loyal_yield.balance_sweep_targets SET period_length_seconds=period_length_seconds+1 WHERE id=$1`
					second = `UPDATE loyal_yield.balance_sweep_targets SET period_length_seconds=period_length_seconds-1 WHERE id=$1`
				}
				if _, err := s.pool.Exec(t.Context(), first, id); err != nil {
					t.Fatal(err)
				}
				if _, err := s.pool.Exec(t.Context(), second, id); err != nil {
					t.Fatal(err)
				}
			}}
			chain := &scriptedControllerChain{balances: map[string]int64{target.WalletTokenATA: 9_000_000, target.VaultTokenATA: 0}}
			controller, err := NewController(ControllerDependencies{Store: s, Chain: chain, Wires: builder})
			if err != nil {
				t.Fatal(err)
			}
			w, err := NewWorker(WorkerDependencies{Store: s, Executor: controller})
			if err != nil {
				t.Fatal(err)
			}
			report, err := w.Tick(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if len(base.built) != 1 || report.ExecutorErrors != 0 {
				t.Fatalf("builder ABA did not reach durable fence: %v %+v", base.built, report)
			}
			var attempts, selected, pending int64
			if err = s.pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM loyal_yield.balance_sweep_transaction_attempts WHERE target_id=$1),(SELECT count(*) FROM loyal_yield.balance_sweep_lot_claims WHERE target_id=$1 AND status='selected'),requested_generation-processed_generation FROM loyal_yield.autodeposit_desired_control_requests WHERE target_id=$1`, id).Scan(&attempts, &selected, &pending); err != nil {
				t.Fatal(err)
			}
			if attempts != 0 || selected != 0 || pending <= 0 || chain.balances[target.WalletTokenATA] != 9_000_000 {
				t.Fatalf("new ABA packet escaped: attempts=%d selected=%d pending=%d balance=%d", attempts, selected, pending, chain.balances[target.WalletTokenATA])
			}
		})
	}
}

func TestDesiredPendingCannotReplaceExistingPullWire(t *testing.T) {
	s := integrationStore(t)
	target, claim, slot := selectedReleaseClaim(t, s, "desired-old-wire")
	wire := base64.StdEncoding.EncodeToString([]byte("retained-source-pull-wire"))
	prepared := PreparedAttempt{ClaimToken: claim, TargetID: target.TargetID, ScheduledSlotID: slot, OperationKind: OperationPull, AmountRaw: 5_000_000, SourcePreBalanceRaw: 9_000_000, ProtectionFloorRaw: ptrInt64(4_000_000), Signature: "source-wire-signature", SignedTransactionBase64: wire, SignedTransactionSHA256: wireSHA256(wire), RecentBlockhash: "source-wire-hash", LastValidBlockHeight: 500}
	original, err := s.PersistPreparedAttempt(t.Context(), prepared, "lease-current")
	if err != nil {
		t.Fatal(err)
	}
	s.EnableDesiredControlAdmission()
	if _, err = s.pool.Exec(t.Context(), `UPDATE loyal_yield.balance_sweep_targets SET desired_active=false WHERE id=$1`, target.TargetID); err != nil {
		t.Fatal(err)
	}
	prepared.Signature = "replacement-signature"
	adopted, err := s.PersistPreparedAttempt(t.Context(), prepared, "lease-current")
	if err != nil {
		t.Fatal(err)
	}
	if adopted.ID != original.ID || adopted.Signature != original.Signature || adopted.SignedTransactionBase64 != original.SignedTransactionBase64 {
		t.Fatal("pending intent replaced immutable old packet")
	}
}

func TestDesiredSetupPublicationGatePreservesExistingPacket(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "new", true: "retained"}[existing], func(t *testing.T) {
			s := integrationStore(t)
			target, claim, _ := selectedReleaseClaim(t, s, "desired-setup")
			builder, plan, setup, _ := setupFixture(t, SetupATA)
			plan.Target.ID = target.TargetID
			plan.Target.ManagedVaultID = target.ManagedVaultID
			plan.AmountRaw = 5_000_000
			if _, err := s.FreezeDepositPlan(t.Context(), claim, "lease-current", plan); err != nil {
				t.Fatal(err)
			}
			wire, err := builder.BuildDestinationSetup(t.Context(), plan, setup, fixedKey("desired-setup-hash"), 900)
			if err != nil {
				t.Fatal(err)
			}
			var original SetupAttempt
			if existing {
				original, err = s.PersistDestinationSetup(t.Context(), claim, "lease-current", setup, wire, 0)
				if err != nil {
					t.Fatal(err)
				}
			}
			s.EnableDesiredControlAdmission()
			if _, err = s.pool.Exec(t.Context(), `UPDATE loyal_yield.balance_sweep_targets SET desired_active=false WHERE id=$1`, target.TargetID); err != nil {
				t.Fatal(err)
			}
			actual, err := s.PersistDestinationSetup(t.Context(), claim, "lease-current", setup, wire, 0)
			if existing {
				if err != nil || actual.ID != original.ID || actual.Wire != original.Wire {
					t.Fatalf("retained setup adoption: %+v %v", actual, err)
				}
			} else {
				if !errors.Is(err, ErrDesiredControlsPending) {
					t.Fatalf("new setup bypassed desired fence: %v", err)
				}
				var count int
				if err = s.pool.QueryRow(t.Context(), `SELECT count(*) FROM loyal_yield.balance_sweep_destination_setup_attempts WHERE claim_token=$1`, claim).Scan(&count); err != nil {
					t.Fatal(err)
				}
				if count != 0 {
					t.Fatal("rejected setup left a packet")
				}
			}
		})
	}
}
