package fleetexec

import (
	"context"
	"crypto/sha512"
	"encoding/binary"
	"testing"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
	sdk "github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"
)

// fixtureAccounts is a node holding one bank at slot: an address it does not
// hold is absent, and a minimum slot past its bank is refused as a node does.
type fixtureAccounts struct {
	accounts map[string]chain.Account
	slot     int64
}

func (f fixtureAccounts) Accounts(_ context.Context, keys []sdk.PublicKey, _ rpc.CommitmentType, minContextSlot uint64) (uint64, []*chain.Account, error) {
	if minContextSlot > uint64(f.slot) {
		return 0, nil, chain.ErrBehind
	}
	out := make([]*chain.Account, len(keys))
	for i, key := range keys {
		if a, ok := f.accounts[key.String()]; ok {
			out[i] = &a
		}
	}
	return uint64(f.slot), out, nil
}

// at is address's account, nil when the fixture holds none.
func (f fixtureAccounts) at(address string) *chain.Account {
	if a, ok := f.accounts[address]; ok {
		return &a
	}
	return nil
}

// fixtureAccount is address's account as a test chain holds it.
func fixtureAccount(address, owner string, lamports uint64, data []byte) chain.Account {
	return chain.Account{Key: sdk.MustPublicKeyFromBase58(address), Owner: sdk.MustPublicKeyFromBase58(owner), Lamports: lamports, Data: data}
}

// testSignature is a distinct, valid signature named for the test.
func testSignature(name string) sdk.Signature {
	return sdk.Signature(sha512.Sum512([]byte(name)))
}

func TestObligationAbsenceProofUsesCollateralAndExactIdentity(t *testing.T) {
	fixture := mustSignedFixture(t)
	owner := sdk.MustPublicKeyFromBase58(fixture.FeePayer)
	market := sdk.MustPublicKeyFromBase58(fixture.SecondaryAccount)
	reserve := sdk.MustPublicKeyFromBase58(fixture.RecentBlockhash)
	a := &chain.Account{Owner: sdk.MustPublicKeyFromBase58(fleet.KaminoProgram), Lamports: 1, Data: make([]byte, 3344)}
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
