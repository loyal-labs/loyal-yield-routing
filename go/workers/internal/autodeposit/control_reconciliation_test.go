package autodeposit

import (
	"bytes"
	"context"
	"testing"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/earncatalog"
)

func TestControlSnapshotProvesActualArtifacts(t *testing.T) {
	ctx := context.Background()
	f, target, builder := controlFixture(t)
	accounts := installControlSnapshot(t, f, builder, uint64(f.MaxAmountPerPeriod))
	observation, err := builder.ObserveControl(ctx, target, 200)
	if err != nil || observation.status() != "active" || observation.WalletBalanceRaw != 9_000_000 {
		t.Fatalf("actual artifacts observation=%+v err=%v", observation, err)
	}
	if _, err = builder.ObserveControl(ctx, target, 201); err == nil {
		t.Fatal("RPC below minimum slot accepted")
	}
	accounts[f.WalletATA].Data[76] ^= 1
	observation, err = builder.ObserveControl(ctx, target, 200)
	if err != nil || observation.status() != "inconsistent" {
		t.Fatalf("foreign token delegate observation=%+v err=%v", observation, err)
	}
	accounts[f.WalletATA].Data[76] ^= 1
	// A policy whose constraints no longer allow the pull is observed, not
	// retried forever as an unprovable target.
	subscriptions := mustKey(earncatalog.SubscriptionsProgram)
	at := bytes.Index(accounts[f.Policy].Data, subscriptions[:])
	if at < 0 {
		t.Fatal("fixture policy lacks the subscriptions program constraint")
	}
	accounts[f.Policy].Data[at] ^= 1
	observation, err = builder.ObserveControl(ctx, target, 200)
	if err != nil || observation.PolicyValid || observation.status() != "inconsistent" {
		t.Fatalf("policy without the pull observation=%+v err=%v", observation, err)
	}
	accounts[f.Policy].Data[at] ^= 1
	policy := accounts[f.Policy]
	policy.Owner = systemProgramZero
	accounts[f.Policy] = policy
	if _, err = builder.ObserveControl(ctx, target, 200); err == nil {
		t.Fatal("foreign policy activated target")
	}
}
