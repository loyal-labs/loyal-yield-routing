package ata

import (
	"context"
	"testing"
	"time"
)

func TestCapturePublicationLockOrdersSequenceBeforeCommit(t *testing.T) {
	p, ctx := projectorFixture(t)
	a := seedProjectionTarget(t, ctx, p)
	b := seedProjectionTarget(t, ctx, p)
	tx, err := p.capture.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	// Hold exactly the production transaction's two publication locks, before
	// allocating a sequence ID. This first transaction models a writer paused
	// immediately before commit; the second is the actual Handler.persist path.
	if _, err := tx.Exec(ctx, `LOCK TABLE loyal_prod.balance_sweep_wallet_ata_observation_dedupe,
loyal_prod.balance_sweep_wallet_ata_observations IN SHARE ROW EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	var firstID int64
	if err := tx.QueryRow(ctx, `INSERT INTO loyal_prod.balance_sweep_wallet_ata_observations
(cluster,target_id,wallet,wallet_usdc_ata,vault_pubkey,vault_usdc_ata,amount_raw,mint,slot,observed_at,source,
source_commitment,account_data_hash,raw_account_data_base64,raw_evidence,received_at)
VALUES($1,$2,$3,$4,$5,$6,100,$7,400,now(),'capture-order-fixture','confirmed','first','', '{}',now()) RETURNING event_id`,
		a.Cluster, a.ID, a.Wallet, a.WalletATA, a.Vault, a.VaultATA, a.Mint).Scan(&firstID); err != nil {
		t.Fatal(err)
	}
	type result struct {
		out Outcome
		err error
	}
	completed := make(chan result, 1)
	workerCtx, stopWorker := context.WithCancel(ctx)
	joined := false
	defer func() {
		stopWorker()
		if !joined {
			select {
			case <-completed:
			case <-time.After(time.Second):
				t.Error("capture worker failed to join after cancellation")
			}
		}
	}()
	go func() {
		out, err := NewHandler(p.capture, "production", nil).persist(workerCtx, observation{target: b, pubkey: b.WalletATA,
			amount: 200, mint: b.Mint, slot: 401, source: laserStreamSource, data: []byte("second capture"), received: time.Now().UTC()})
		completed <- result{out: out, err: err}
	}()
	// Wait for observable lock contention rather than relying on sleep timing.
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var waiting bool
		if err := p.capture.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_locks
WHERE relation='loyal_prod.balance_sweep_wallet_ata_observation_dedupe'::regclass
AND mode='ShareRowExclusiveLock' AND NOT granted)`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case early := <-completed:
			joined = true
			t.Fatalf("capture passed an uncommitted earlier publisher: %+v %v", early.out, early.err)
		case <-deadline.C:
			t.Fatal("actual capture did not acquire the publication lock")
		case <-ticker.C:
		}
	}
	var allocated int64
	if err := p.capture.QueryRow(ctx, `SELECT last_value FROM loyal_prod.balance_sweep_wallet_ata_observation_event_id_seq`).Scan(&allocated); err != nil || allocated != firstID {
		t.Fatalf("waiting capture allocated an ID before lock/commit: allocated=%d first=%d err=%v", allocated, firstID, err)
	}
	if out, err := p.Tick(ctx); err != nil || out.LastEventID >= firstID {
		t.Fatalf("projector passed unpublished earlier ID: %+v %v", out, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	second := <-completed
	joined = true
	if second.err != nil || !second.out.Inserted || second.out.EventID <= firstID {
		t.Fatalf("ordered capture did not commit after first: %+v %v", second.out, second.err)
	}
	if out, err := p.Tick(ctx); err != nil || out.LastEventID != second.out.EventID || out.InsertedEvents != 2 {
		t.Fatalf("ordered commits did not project both exact IDs: %+v %v", out, err)
	}
}
