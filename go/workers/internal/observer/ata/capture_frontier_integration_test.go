package ata

import (
	"context"
	"testing"
	"time"
)

func TestHeldLowerCaptureIDStillProjectedAfterHigherCommits(t *testing.T) {
	p, ctx := projectorFixture(t)
	a := seedProjectionTarget(t, ctx, p)
	b := seedProjectionTarget(t, ctx, p)
	held, err := p.capture.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Rollback(context.Background())
	var heldID int64
	if err := held.QueryRow(ctx, `INSERT INTO loyal_prod.balance_sweep_wallet_ata_observations
(cluster,target_id,wallet,wallet_usdc_ata,vault_pubkey,vault_usdc_ata,amount_raw,mint,slot,observed_at,source,
source_commitment,account_data_hash,raw_account_data_base64,raw_evidence,received_at)
VALUES($1,$2,$3,$4,$5,$6,100,$7,400,now(),'capture-order-fixture','confirmed','held','', '{}',now()) RETURNING event_id`,
		a.Cluster, a.ID, a.Wallet, a.WalletATA, a.Vault, a.VaultATA, a.Mint).Scan(&heldID); err != nil {
		t.Fatal(err)
	}
	type result struct {
		out Outcome
		err error
	}
	completed := make(chan result, 1)
	workerCtx, stopWorker := context.WithCancel(ctx)
	defer stopWorker()
	go func() {
		out, err := NewHandler(p.capture, nil).persist(workerCtx, observation{target: b, pubkey: b.WalletATA,
			amount: 200, mint: b.Mint, slot: 401, source: laserStreamSource, data: []byte("later capture"), received: time.Now().UTC()})
		completed <- result{out: out, err: err}
	}()
	var later result
	settled := false
	select {
	case later = <-completed:
		settled = true
	case <-time.After(500 * time.Millisecond):
	}
	if out, err := p.Tick(ctx); err != nil || out.InsertedEvents != 0 || out.LastEventID >= heldID {
		t.Fatalf("projector passed an uncommitted lower ID: %+v %v", out, err)
	}
	if err := held.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if !settled {
		select {
		case later = <-completed:
		case <-time.After(5 * time.Second):
			t.Fatal("later capture did not finish after the held transaction committed")
		}
	}
	if later.err != nil || !later.out.Inserted {
		t.Fatalf("later capture failed: %+v %v", later.out, later.err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		var projected int
		if err := p.yield.QueryRow(ctx, `SELECT count(*) FROM loyal_yield.balance_sweep_wallet_balance_events WHERE event_id IN ($1,$2)`,
			heldID, later.out.EventID).Scan(&projected); err != nil {
			t.Fatal(err)
		}
		if projected == 2 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("held lower ID %d and later ID %d not both projected: %d", heldID, later.out.EventID, projected)
		}
		if _, err := p.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
