package fleetexec

import (
	"context"
	"encoding/binary"
	"errors"
	"testing"
	"time"

	sdk "github.com/gagliardetto/solana-go"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
)

type fixtureAccounts struct {
	accounts map[string]fleet.Account
	slot     int64
}

func (f fixtureAccounts) FinalizedAccounts(_ context.Context, addresses []string, floor int64) (int64, []fleet.Account, error) {
	out := make([]fleet.Account, 0, len(addresses))
	for _, address := range addresses {
		a, ok := f.accounts[address]
		if !ok {
			return 0, nil, errors.New("account missing")
		}
		out = append(out, a)
	}
	return f.slot, out, nil
}
func TestObligationAbsenceProofUsesCollateralAndExactIdentity(t *testing.T) {
	fixture := mustSignedFixture(t)
	owner := sdk.MustPublicKeyFromBase58(fixture.FeePayer)
	market := sdk.MustPublicKeyFromBase58(fixture.SecondaryAccount)
	reserve := sdk.MustPublicKeyFromBase58(fixture.RecentBlockhash)
	a := fleet.Account{Owner: fleet.KaminoProgram, Lamports: 1, Data: make([]byte, 3344)}
	copy(a.Data[:8], []byte{168, 206, 141, 106, 88, 76, 172, 167})
	copy(a.Data[32:64], market[:])
	copy(a.Data[64:96], owner[:])
	copy(a.Data[96:128], reserve[:])
	binary.LittleEndian.PutUint64(a.Data[128:136], 42)
	got, err := obligationCollateral(a, market.String(), owner.String(), reserve.String())
	if err != nil || got != 42 {
		t.Fatalf("collateral read %d: %v", got, err)
	}
	if _, err := obligationCollateral(a, market.String(), reserve.String(), reserve.String()); err == nil {
		t.Fatal("wrong custody owner accepted")
	}
	copy(a.Data[232:264], reserve[:])
	if _, err := obligationCollateral(a, market.String(), owner.String(), reserve.String()); err == nil {
		t.Fatal("duplicate deposit accepted")
	}
}
func TestLeaseWorkDeadlineUsesDatabaseExpiryWithMargin(t *testing.T) {
	expires := time.Date(2026, 10, 2, 0, 0, 30, 0, time.UTC)
	if got := leaseWorkDeadline(expires, time.Minute); !got.Equal(expires.Add(-5 * time.Second)) {
		t.Fatalf("deadline %v outlives bounded database fence", got)
	}
	if got := leaseWorkDeadline(expires, time.Second); !got.Equal(expires.Add(-100 * time.Millisecond)) {
		t.Fatalf("short lease margin %v", got)
	}
}
