package fleet

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"

	solana "github.com/solana-foundation/solana-go/v2"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
)

func TestFusedCycleCannotBypassTypedAdmission(t *testing.T) {
	r := &Revalidator{fusedExecute: true}
	if claimed, err := r.Cycle(context.Background(), "mainnet-beta"); err == nil || claimed {
		t.Fatalf("untyped fused path accepted: claimed=%v error=%v", claimed, err)
	}
}

func TestFreshVaultTokenCustodyChecksProgramAndState(t *testing.T) {
	owner := testIdentity(60)
	for _, mint := range []string{USDCMint, PYUSDMint} {
		program, ok := stableTokenProgram(mint)
		if !ok {
			t.Fatal("fixture mint unsupported")
		}
		data := make([]byte, 165)
		if program == token2022Program {
			data = append(make([]byte, 165), 2, 7, 0, 0, 0)
		}
		fixtureKey(t, data, 0, mint)
		fixtureKey(t, data, 32, owner)
		data[108] = 1
		a := &chain.Account{Owner: solana.MustPublicKeyFromBase58(program), Lamports: 1, Data: data}
		if _, err := validateVaultTokenAccount(a, mint, owner); err != nil {
			t.Fatalf("valid %s custody rejected: %v", mint, err)
		}
		for _, state := range []byte{0, 2} {
			a.Data[108] = state
			if _, err := validateVaultTokenAccount(a, mint, owner); err == nil {
				t.Fatal("uninitialized or frozen custody accepted")
			}
		}
		a.Data[108] = 1
		a.Owner = solana.MustPublicKeyFromBase58(SquadsProgram)
		if _, err := validateVaultTokenAccount(a, mint, owner); err == nil {
			t.Fatal("foreign token program accepted")
		}
	}
}

func TestFreshLookupTableRejectsCurrentSlotExtension(t *testing.T) {
	table := testIdentity(63)
	member := testIdentity(64)
	for _, extended := range []uint64{499, 500, 501} {
		t.Run(fmt.Sprint(extended), func(t *testing.T) {
			data := make([]byte, 88)
			binary.LittleEndian.PutUint32(data[:4], 1)
			binary.LittleEndian.PutUint64(data[4:12], ^uint64(0))
			binary.LittleEndian.PutUint64(data[12:20], extended)
			fixtureKey(t, data, 56, member)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					ID uint64 `json:"id"`
				}
				_ = json.NewDecoder(r.Body).Decode(&req)
				_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"context": map[string]any{"slot": 500}, "value": []any{map[string]any{"owner": altProgram, "lamports": 1, "executable": false, "data": []string{base64.StdEncoding.EncodeToString(data), "base64"}}}}})
			}))
			defer srv.Close()
			r := &Revalidator{rpc: testChain(t, srv.URL)}
			_, err := r.verifyLookupTables(context.Background(), []LookupTable{{Address: table, Addresses: []string{member}}}, 499)
			if (err == nil) != (extended < 500) {
				t.Fatalf("extended=%d error=%v", extended, err)
			}
		})
	}
}

func TestCrossMintFutureValueUsesWideIntermediates(t *testing.T) {
	plan := json.RawMessage(`{"holding_horizon_seconds":2592000,"estimated_execution_costs":{"kind":"cross_mint_jupiter","jupiter_swap_usd_micros":100000,"deposit_usd_micros":100000}}`)
	amount, err := minimumProfitableCrossMintOutput(plan, 10_000_000_000, 1000, 2000)
	if err != nil || amount == 0 || amount >= 10_000_000_000 {
		t.Fatalf("representable economics rejected: amount=%d error=%v", amount, err)
	}
	// Independent closed-form minimum: floor(P * (D + targetYield) / D)
	// must exceed sourceFuture + swapCost. The deposit cost cancels on both sides.
	den := big.NewInt(365 * 24 * 60 * 60 * 10000)
	sourceGain := new(big.Int).Mul(big.NewInt(10_000_000_000), big.NewInt(1000*2592000))
	sourceGain.Quo(sourceGain, den)
	required := sourceGain.Add(sourceGain, big.NewInt(10_000_000_000+100000+1))
	required.Mul(required, den)
	divisor := new(big.Int).Add(new(big.Int).Set(den), big.NewInt(2000*2592000))
	required.Add(required, new(big.Int).Sub(new(big.Int).Set(divisor), big.NewInt(1)))
	want := required.Quo(required, divisor).Uint64()
	if amount != want {
		t.Fatalf("binary-search minimum=%d closed-form=%d", amount, want)
	}
}
