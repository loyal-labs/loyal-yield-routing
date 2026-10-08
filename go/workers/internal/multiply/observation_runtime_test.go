package multiply

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gagliardetto/solana-go"
)

type observationTransport func(*http.Request) (*http.Response, error)

func (f observationTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func testLiveReader(t *testing.T, transport observationTransport) *LiveObservationReader {
	t.Helper()
	rpc := NewLiveRPCSurface("http://disposable-rpc.invalid")
	rpc.HTTP = &http.Client{Transport: transport, Timeout: time.Second}
	reader, err := NewLiveObservationReader(rpc)
	if err != nil {
		t.Fatal(err)
	}
	return reader
}

func TestLiveObservationPreservesKnownAbsenceAndAccountIdentity(t *testing.T) {
	key := fixtureKey(19)
	reader := testLiveReader(t, func(request *http.Request) (*http.Response, error) {
		var call struct {
			ID     int64             `json:"id"`
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(request.Body).Decode(&call); err != nil {
			return nil, err
		}
		var keys []string
		var options map[string]string
		if err := json.Unmarshal(call.Params[0], &keys); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(call.Params[1], &options); err != nil {
			return nil, err
		}
		if call.Method != "getMultipleAccounts" || len(keys) != 2 || keys[0] != key.String() || options["commitment"] != "confirmed" || options["encoding"] != "base64" {
			t.Error("observation transport changed confirmed account scope")
		}
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": call.ID, "result": map[string]any{
			"context": map[string]any{"slot": uint64(700)}, "value": []any{map[string]any{"owner": TokenProgram, "lamports": 1, "executable": false, "data": []any{base64.StdEncoding.EncodeToString([]byte{7, 8}), "base64"}}, nil},
		}})
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
	})
	slot, accounts, err := reader.GetMultipleAccounts(context.Background(), []solana.PublicKey{key, fixtureKey(20)})
	if err != nil {
		t.Fatal(err)
	}
	if slot != 700 || accounts[0].Address != key.String() || string(accounts[0].Data) != string([]byte{7, 8}) || accounts[1] != nil {
		t.Fatal("bank observation lost identity or explicit absence")
	}
}

func TestLiveObservationCancellationJoinsTransport(t *testing.T) {
	started, returned := make(chan struct{}), make(chan struct{})
	reader := testLiveReader(t, func(request *http.Request) (*http.Response, error) {
		close(started)
		<-request.Context().Done()
		close(returned)
		return nil, request.Context().Err()
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, _, err := reader.GetMultipleAccounts(ctx, []solana.PublicKey{fixtureKey(19)}); done <- err }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("read did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("read did not join cancelled transport")
	}
	select {
	case <-returned:
	default:
		t.Fatal("IO remained owned after read returned")
	}
}

type bankObservationReader struct {
	accounts map[string]*Account
	slot     uint64
	batches  int
}

func (r *bankObservationReader) GetMultipleAccounts(_ context.Context, keys []solana.PublicKey) (uint64, []*Account, error) {
	r.batches++
	accounts := make([]*Account, len(keys))
	for i, key := range keys {
		accounts[i] = r.accounts[key.String()]
	}
	return r.slot, accounts, nil
}
func (*bankObservationReader) GetAccount(context.Context, solana.PublicKey) (*Account, error) {
	return nil, errors.New("slotless financial read forbidden")
}

func observedToken(key solana.PublicKey, mint string, owner solana.PublicKey, amount uint64) *Account {
	data := make([]byte, 165)
	mintKey := mustKey(mint)
	copy(data[:32], mintKey[:])
	copy(data[32:64], owner[:])
	binary.LittleEndian.PutUint64(data[64:72], amount)
	data[108] = 1
	return &Account{Address: key.String(), Owner: TokenProgram, Lamports: 1, Data: data}
}

func emptyBankReader(topology *EarnMaxTopology) *bankObservationReader {
	reader := &bankObservationReader{accounts: map[string]*Account{}, slot: 500}
	reader.accounts[topology.ClaimCustody.String()] = observedToken(topology.ClaimCustody, USDCMint, topology.Vault, 4_000_000)
	for _, config := range topology.StrategyCatalog() {
		reader.accounts[config.CollateralReserve.String()] = reviewReserveAccount(config, false)
		reader.accounts[config.DebtReserve.String()] = reviewReserveAccount(config, true)
	}
	return reader
}

func TestReceiptAndRouteCustodyShareOneBankSnapshot(t *testing.T) {
	topology := testTopology(t)
	reader := emptyBankReader(topology)
	destination := fixtureKey(211)
	reader.accounts[destination.String()] = observedToken(destination, USDCMint, fixtureKey(210), 2_000_000)
	extra := []TokenBalance{{Account: destination.String(), Mint: USDCMint, TokenProgram: TokenProgram}}
	observed, err := ObserveConfirmed(context.Background(), reader, topology, extra)
	if err != nil {
		t.Fatal(err)
	}
	if reader.batches != 1 || observed.Slot != 500 || observed.Claim.AmountRaw != 4_000_000 || observed.ExternalCustody[0].AmountRaw != 2_000_000 {
		t.Fatal("financial receipt was not bound to a single bank snapshot")
	}
	// Nil entries supplied by a complete RPC snapshot are known absent, not
	// transport omissions; undeployed obligations contain no debt or principal.
	for _, position := range observed.Strategies {
		if position == nil || position.DebtRaw != 0 || position.CollateralDepositedRaw != 0 {
			t.Fatal("known absent obligation fabricated exposure")
		}
	}
	delete(reader.accounts, destination.String())
	if _, err := ObserveConfirmed(context.Background(), reader, topology, extra); err == nil {
		t.Fatal("missing receipt destination became a zero-balance proof")
	}
}

func TestObservationRefusesFutureAccountAndUnknownPlannerCustody(t *testing.T) {
	topology := testTopology(t)
	reader := emptyBankReader(topology)
	config := topology.Strategies[SyrupUsdcUsdc]
	binary.LittleEndian.PutUint64(reader.accounts[config.CollateralReserve.String()].Data[16:24], 501)
	if _, err := ObserveConfirmed(context.Background(), reader, topology, nil); err == nil {
		t.Fatal("future reserve escaped bank-slot validation")
	}
	observed := idleObserved(topology)
	observed.DebtCustodies = nil
	route := testRouteState(t, topology)
	route.Goal = GoalWithdraw
	if decision := NextAction(route, observed, topology); decision.Kind != "invalid_observation" {
		t.Fatal("missing custody completed withdrawal")
	}
	operationID := "immutable-attempt"
	route.CurrentOperationID = &operationID
	if decision := NextAction(route, nil, topology); decision.Kind != "resume" {
		t.Fatal("unknown fresh observation hid an already owned recovery")
	}
	if decision := NextAction(route, observed, topology); decision.Kind != "resume" {
		t.Fatal("missing fresh admission evidence hid owned recovery")
	}
}

func TestWithdrawalExactPayoutRequiresEnoughConfirmedLiquidity(t *testing.T) {
	// Exact-payout semantics are independently established by the App's
	// persisted request and wallet-signed transfer_checked, not planner defaults.
	topology := testTopology(t)
	destination := fixtureKey(212).String()
	for _, test := range []struct {
		name  string
		claim uint64
		want  string
	}{
		{"fee_loss", 9_990_000, "withdrawal_liquidity_shortfall"},
		{"nav_loss", 8_000_000, "withdrawal_liquidity_shortfall"},
		{"exact", 10_000_000, "withdrawal_claimable"},
		{"surplus", 11_000_000, "withdrawal_claimable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			route := testRouteState(t, topology)
			route.Goal = GoalWithdraw
			now := time.Now().UTC()
			route.Withdrawal = &Withdrawal{RequestID: "exact-user-request", DestinationAccount: destination, AmountRaw: 10_000_000, Status: WithdrawalRequested, RequestedAt: now, ReadyBy: now.Add(10 * time.Minute)}
			observed := idleObserved(topology)
			observed.Claim.AmountRaw = test.claim
			observed.ExternalCustody = []TokenBalance{{Account: destination, Mint: USDCMint, TokenProgram: TokenProgram}}
			condition, err := withdrawalClaimCondition(route, observed, topology)
			if err != nil || condition != test.want {
				t.Fatalf("condition %s %v", condition, err)
			}
			if route.Withdrawal.AmountRaw != 10_000_000 || route.Withdrawal.Status != WithdrawalRequested {
				t.Fatal("eligibility rewrote exact user intent")
			}
			serialized, err := jsonMarshal(route)
			if err != nil {
				t.Fatal(err)
			}
			var restarted RouteState
			if err := jsonUnmarshalStrict(serialized, &restarted); err != nil {
				t.Fatal(err)
			}
			if condition, err := withdrawalClaimCondition(&restarted, observed, topology); err != nil || condition != test.want {
				t.Fatalf("restart changed payout evidence: %s %v", condition, err)
			}
			observed.ExternalCustody = nil
			if _, err := withdrawalClaimCondition(route, observed, topology); err == nil {
				t.Fatal("missing destination became claimable")
			}
			if _, err := withdrawalClaimCondition(route, nil, topology); err == nil {
				t.Fatal("missing latest snapshot became claimable")
			}
		})
	}
}
