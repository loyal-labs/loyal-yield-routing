package backyard

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"slices"
	"testing"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/engine"
)

func TestVaultWriterIsolationAndLegacyExclusion(t *testing.T) {
	control := withdrawalCostTestDB(t)
	ctx := context.Background()
	dsn := os.Getenv("BACKYARD_WITHDRAWAL_TEST_DSN")
	legacy, err := control.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer legacy.Release()
	family := string(engine.FamilyBackyard)
	if _, err := legacy.Exec(ctx, `SELECT pg_advisory_lock(`+writerLockKey+`)`, family); err != nil {
		t.Fatal(err)
	}
	for _, v := range vaults {
		if d, err := openOperatorWriter(ctx, dsn, v.Name); !errors.Is(err, engine.ErrFamilyHeld) {
			if d != nil {
				d.Close()
			}
			t.Fatalf("legacy writer did not exclude %s: %v", v.Name, err)
		}
	}
	if _, err := legacy.Exec(ctx, `SELECT pg_advisory_unlock(`+writerLockKey+`)`, family); err != nil {
		t.Fatal(err)
	}
	open := func(v *Vault) *Database {
		t.Helper()
		d, err := OpenDatabase(ctx, engine.DirectDSN(dsn), v.Name)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(d.Close)
		return d
	}
	a, b := open(rwa), open(looping)
	for _, v := range vaults {
		if d, err := openOperatorWriter(ctx, dsn, v.Name); !errors.Is(err, engine.ErrFamilyHeld) {
			if d != nil {
				d.Close()
			}
			t.Fatalf("duplicate writer accepted for %s: %v", v.Name, err)
		}
	}
	var held bool
	if err := legacy.QueryRow(ctx, `SELECT pg_try_advisory_lock(`+writerLockKey+`)`, family).Scan(&held); err != nil || held {
		t.Fatalf("old combined worker could overlap split workers: held=%v err=%v", held, err)
	}
	a.Close()
	a = open(rwa)
	var pid uint32
	if err := a.pool.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(ctx, `SELECT pg_terminate_backend($1)`, pid); err != nil {
		t.Fatal(err)
	}
	_, _ = a.pool.Exec(ctx, `SELECT 1`)
	if _, err := a.pool.Exec(ctx, `SELECT 1`); !errors.Is(err, errWriterSessionLost) {
		t.Fatalf("lost writer session silently reconnected: %v", err)
	}
	if _, err := b.pool.Exec(ctx, `SELECT 1`); err != nil {
		t.Fatalf("losing Loyal writer interrupted Backyard: %v", err)
	}
}

func TestVaultViewSubscribesOnlySelectedCustodyAndReceipts(t *testing.T) {
	for _, selected := range vaults {
		t.Run(selected.Name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			connector := &fakeLaserStream{}
			view, err := OpenView(ctx, seedChain(t, 77, nil), connector, selected.Name)
			if err != nil {
				t.Fatal(err)
			}
			request, _ := connector.last()
			for _, v := range vaults {
				_, receipts := request.Accounts[viewReceiptsFilter(v)]
				custody := slices.Contains(request.Accounts[viewAccountsFilter].Account, v.squads)
				_, seeded := view.accounts[v.squads]
				if receipts != (v == selected) || custody != (v == selected) || seeded != (v == selected) {
					t.Fatalf("%s view includes wrong vault %s: receipts=%v custody=%v seed=%v", selected.Name, v.Name, receipts, custody, seeded)
				}
			}
		})
	}
}

func TestVaultSettlementReleasesOnlyOwnedOperations(t *testing.T) {
	d := withdrawalCostTestDB(t)
	ctx := context.Background()
	for _, v := range vaults {
		if _, err := d.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_operations(operation_id,route_key,status,expected_effects,recovery_reason) VALUES($1,$2,'failed','{}','simulation_failed')`, v.Name, v.RouteKey); err != nil {
			t.Fatal(err)
		}
		if _, err := d.pool.Exec(ctx, `INSERT INTO loyal_yield.backyard_withdrawal_cost_operations(operation_id,evidence_digest,message_sha256,effective_lp,max_cost_usdc,other_holders_usdc,max_incremental_cost_usdc,incremental_other_holders_usdc,network_usdc,network_lamports,evidence) VALUES($1,repeat('a',64),repeat('b',64),1,0,0,0,0,0,0,'{}')`, v.Name); err != nil {
			t.Fatal(err)
		}
	}
	for _, selected := range []*Vault{looping, rwa} {
		w := newWithdrawalSettlementWorker(d, newFakeChain(t, nil), slog.New(slog.NewTextHandler(io.Discard, nil)), selected)
		w.sweep(ctx)
		for _, v := range vaults {
			want := "released"
			if selected == looping && v == rwa {
				want = "reserved"
			}
			var state string
			if err := d.pool.QueryRow(ctx, `SELECT state FROM loyal_yield.backyard_withdrawal_cost_operations WHERE operation_id=$1`, v.Name).Scan(&state); err != nil || state != want {
				t.Fatalf("%s sweep: %s state=%s want=%s err=%v", selected.Name, v.Name, state, want, err)
			}
		}
	}
}
