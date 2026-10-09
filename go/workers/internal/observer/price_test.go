package observer

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"math"
	"math/big"
	"testing"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"
)

type priceFixtureRPC struct {
	accounts          map[string]*chain.Account
	slot              uint64
	clock             *int64
	calls, blockCalls int
	batchSizes        []int
	err               error
}

func (r *priceFixtureRPC) Accounts(ctx context.Context, keys []solana.PublicKey, commitment rpc.CommitmentType, minContextSlot uint64) (uint64, []*chain.Account, error) {
	if err := ctx.Err(); err != nil {
		return 0, nil, err
	}
	if r.err != nil {
		return 0, nil, r.err
	}
	r.calls++
	r.batchSizes = append(r.batchSizes, len(keys))
	var out []*chain.Account
	for _, key := range keys {
		out = append(out, r.accounts[key.String()])
	}
	return r.slot, out, nil
}
func (r *priceFixtureRPC) BlockTime(ctx context.Context, slot uint64) (time.Time, error) {
	r.blockCalls++
	if r.clock == nil {
		return time.Time{}, chain.ErrNotFound
	}
	return time.Unix(*r.clock, 0).UTC(), nil
}
func reserveFixture() *chain.Account {
	data := make([]byte, 8624)
	disc := sha256.Sum256([]byte("account:Reserve"))
	copy(data[:8], disc[:8])
	copy(data[32:64], solana.MustPublicKeyFromBase58(benchmarkMarket).Bytes())
	copy(data[128:160], solana.MustPublicKeyFromBase58(USDCMint).Bytes())
	binary.LittleEndian.PutUint64(data[16:24], 900)
	binary.LittleEndian.PutUint64(data[224:232], 100_000_000)
	binary.LittleEndian.PutUint64(data[2592:2600], 100_000_000)
	putSF := func(offset int, raw int64) {
		n := new(big.Int).Lsh(big.NewInt(raw), 60)
		b := n.Bytes()
		for i, v := range b {
			data[offset+len(b)-1-i] = v
		}
	}
	putSF(232, 20_000_000)
	putSF(344, 5_000_000)
	putSF(360, 3_000_000)
	putSF(376, 2_000_000)
	return &chain.Account{Owner: priceProgram, Data: data}
}
func TestIndependentPriceUsesBorrowingFeesAndActualReserveClock(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 30, 0, 0, time.UTC)
	clock := now.Add(-45 * time.Minute).Unix()
	a := reserveFixture()
	other := solana.PublicKey{1}.String()
	rpc := &priceFixtureRPC{slot: 1000, clock: &clock, accounts: map[string]*chain.Account{benchmarkReserve: a, other: reserveFixture()}}
	rows, missing, err := ProbeSharePrices(context.Background(), rpc, []string{benchmarkReserve, other, benchmarkReserve}, now)
	if err != nil || len(missing) != 0 || len(rows) != 2 {
		t.Fatalf("probe = %+v %v %v", rows, missing, err)
	}
	for _, row := range rows {
		if math.Abs(row.Price-1.1) > 1e-12 || row.Slot != 900 || !row.ObservedAt.Equal(time.Unix(clock, 0)) || row.ObservedHour != time.Unix(clock, 0).UTC().Truncate(time.Hour) {
			t.Fatalf("available-only or polling-time price: %+v", row)
		}
	}
	if rpc.calls != 1 || rpc.blockCalls != 1 {
		t.Fatalf("duplicate reads: %+v", rpc)
	}
}
func TestIndependentPriceRefusesUnknownAndStaleEvidence(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 30, 0, 0, time.UTC)
	clock := now.Unix()
	cases := []struct {
		name   string
		mutate func(*chain.Account, *priceFixtureRPC)
	}{
		{"missing", func(a *chain.Account, r *priceFixtureRPC) { r.accounts[benchmarkReserve] = nil }},
		{"owner", func(a *chain.Account, r *priceFixtureRPC) { a.Owner = solana.MustPublicKeyFromBase58(USDCMint) }},
		{"executable", func(a *chain.Account, r *priceFixtureRPC) { a.Executable = true }},
		{"layout", func(a *chain.Account, r *priceFixtureRPC) { a.Data = a.Data[:2600] }},
		{"discriminator", func(a *chain.Account, r *priceFixtureRPC) { a.Data[0] ^= 1 }},
		{"stale", func(a *chain.Account, r *priceFixtureRPC) { a.Data[24] = 1 }},
		{"future-slot", func(a *chain.Account, r *priceFixtureRPC) { binary.LittleEndian.PutUint64(a.Data[16:24], 1001) }},
		{"zero-collateral", func(a *chain.Account, r *priceFixtureRPC) { binary.LittleEndian.PutUint64(a.Data[2592:2600], 0) }},
		{"underwater-fees", func(a *chain.Account, r *priceFixtureRPC) {
			for i := 344; i < 360; i++ {
				a.Data[i] = 255
			}
		}},
		{"no-block-time", func(a *chain.Account, r *priceFixtureRPC) { r.clock = nil }},
		{"future-time", func(a *chain.Account, r *priceFixtureRPC) { c := now.Add(time.Second).Unix(); r.clock = &c }},
		{"old-time", func(a *chain.Account, r *priceFixtureRPC) {
			c := now.Add(-3*time.Hour - time.Second).Unix()
			r.clock = &c
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := reserveFixture()
			r := &priceFixtureRPC{slot: 1000, clock: &clock, accounts: map[string]*chain.Account{benchmarkReserve: a}}
			tc.mutate(a, r)
			rows, missing, err := ProbeSharePrices(context.Background(), r, []string{benchmarkReserve}, now)
			if err != nil || len(rows) != 0 || len(missing) != 1 {
				t.Fatalf("unknown admitted: %v %v %v", rows, missing, err)
			}
		})
	}
}
func TestIndependentPriceBoundedBatchAndTransportFailure(t *testing.T) {
	now := time.Now().UTC()
	clock := now.Unix()
	r := &priceFixtureRPC{slot: 1000, clock: &clock, accounts: map[string]*chain.Account{}}
	var keys []string
	for i := 0; i < 201; i++ {
		key := solana.PublicKey{byte(i), 1}.String()
		keys = append(keys, key)
		r.accounts[key] = reserveFixture()
	}
	rows, _, err := ProbeSharePrices(context.Background(), r, keys, now)
	if err != nil || len(rows) != 201 || len(r.batchSizes) != 3 || r.batchSizes[0] != 100 || r.batchSizes[1] != 100 || r.batchSizes[2] != 1 {
		t.Fatalf("unbounded/incomplete probe: %d %+v %v", len(rows), r.batchSizes, err)
	}
	r.err = errors.New("offline fixture transport failure")
	rows, _, err = ProbeSharePrices(context.Background(), r, keys, now)
	if err == nil || rows != nil {
		t.Fatal("failed RPC invented prices")
	}
}
