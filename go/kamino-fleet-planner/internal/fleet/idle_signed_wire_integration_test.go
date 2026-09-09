package fleet

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Runs an independent SVM from the immutable same-mint fixture. Only initial
// idle funding is seeded. It is a real signed Squads/SPL execution and replay
// test, NOT an idle publication/revalidation/retained recovery lifecycle proof.
func verifyIdleDepositSignedWire(t *testing.T, ctx context.Context, proxy *KLendProxy, seed map[string]Account, positions []KaminoPositionAccounts, policy, signer, vault string, tableNames []string) {
	t.Helper()
	const amount = uint64(1_000_000_000)
	accounts := make(map[string]Account, len(seed))
	for key, account := range seed {
		account.Data = bytes.Clone(account.Data)
		accounts[key] = account
	}
	target := positions[1]
	idle := accounts[target.VaultLiquidityATA]
	binary.LittleEndian.PutUint64(idle.Data[64:72], amount)
	accounts[idle.Address] = idle
	svm := startConnectedSVM(t, ctx, accounts)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			http.Error(w, "invalid request", 400)
			return
		}
		response, err := svm.call(json.RawMessage(raw))
		if err != nil {
			http.Error(w, "SVM unavailable", 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(response)
	}))
	defer server.Close()
	rpc := NewRPCClient(server.URL)
	request := KaminoIdleDepositRequest{Vault: vault, Target: target, DepositLiquidityAmount: amount}
	route, err := proxy.BuildIdleDeposit(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	tables := []LookupTable{}
	for _, key := range tableNames {
		table, err := decodeLookupTable(accounts[key], 1000)
		if err != nil {
			t.Fatal(err)
		}
		tables = append(tables, table)
	}
	simulate := func(wire []byte) (SimulationEvidence, error) { return rpc.SimulateExactTransaction(ctx, wire, 1000) }
	preparation, err := PrepareIdleDepositRoute(route, request, policy, signer, 0, 1, tables, svm.blockhash, 5000, defaultComputeLimit, simulate)
	if err != nil {
		t.Fatal(err)
	}
	if !preparation.Simulation.Succeeded || preparation.Simulation.UnitsConsumed == 0 || preparation.Transaction.PacketBytes > SolanaPacketLimit {
		t.Fatal("idle did not execute an exact bounded simulation")
	}
	readBalances := func() (uint64, uint64, uint64) {
		_, current, err := rpc.ConfirmedAccounts(ctx, []string{positions[0].Obligation, target.Obligation, target.VaultLiquidityATA}, 1000)
		if err != nil {
			t.Fatal(err)
		}
		sourcePosition, targetPosition := positions[0], target
		source, err := decodeObligation(current[0], sourcePosition.Market, vault, sourcePosition.Reserve, &sourcePosition)
		if err != nil {
			t.Fatal(err)
		}
		expectedTarget := ""
		if binary.LittleEndian.Uint64(current[1].Data[128:136]) != 0 {
			expectedTarget = target.Reserve
		}
		deposited, err := decodeObligation(current[1], target.Market, vault, expectedTarget, &targetPosition)
		if err != nil {
			t.Fatal(err)
		}
		return source, deposited, binary.LittleEndian.Uint64(current[2].Data[64:72])
	}
	assertBalances := func(wantTarget, wantIdle uint64) {
		source, deposited, idle := readBalances()
		if source != amount || deposited != wantTarget || idle != wantIdle {
			t.Fatalf("idle wire balances: source=%d target=%d idle=%d", source, deposited, idle)
		}
	}
	assertBalances(0, amount) // Simulation must not mutate chain state.
	for _, index := range []uint8{0, 2} {
		wrapped, err := wrapSquadsPolicy(policy, signer, 0, []uint8{index}, route.Protected)
		if err != nil {
			t.Fatal(err)
		}
		instructions := append(append([]RouteInstruction{}, route.Public...), wrapped)
		invalid, _, err := compileV0Transaction(signer, svm.blockhash, instructions, tables, 5000, defaultComputeLimit)
		if err != nil {
			t.Fatal(err)
		}
		result, err := simulate(invalid.UnsignedWire)
		if err != nil {
			t.Fatal(err)
		}
		if result.Succeeded {
			t.Fatal("Squads accepted wrong deposit constraint index")
		}
	}
	assertBalances(0, amount)
	private := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, 32))
	signature := ed25519.Sign(private, preparation.Transaction.Message)
	wire := append([]byte{1}, signature...)
	wire = append(wire, preparation.Transaction.Message...)
	for attempt := 0; attempt < 2; attempt++ {
		var returned string
		if err := rpc.call(ctx, "sendTransaction", []any{base64.StdEncoding.EncodeToString(wire), map[string]any{"encoding": "base64"}}, &returned); err != nil {
			t.Fatal(err)
		}
		if returned != encodeBase58(signature) {
			t.Fatal("SVM returned another signed transaction")
		}
		assertBalances(amount, 0)
	}
	var evidence struct {
		SubmissionAttempts int `json:"submissionAttempts"`
		Receipts           map[string]struct {
			Err  json.RawMessage `json:"err"`
			Slot int64           `json:"slot"`
		} `json:"receipts"`
		Transactions map[string]struct {
			Transaction []json.RawMessage `json:"transaction"`
		} `json:"transactions"`
	}
	if err := rpc.call(ctx, "executionEvidence", []any{}, &evidence); err != nil {
		t.Fatal(err)
	}
	receipt, ok := evidence.Receipts[encodeBase58(signature)]
	if !ok || string(receipt.Err) != "null" || receipt.Slot <= 0 || len(evidence.Receipts) != 1 || len(evidence.Transactions) != 1 || evidence.SubmissionAttempts != 2 {
		t.Fatal("idle replay did not retain exactly one successful execution")
	}
	var persistedWire string
	if err := json.Unmarshal(evidence.Transactions[encodeBase58(signature)].Transaction[0], &persistedWire); err != nil {
		t.Fatal(err)
	}
	if persistedWire != base64.StdEncoding.EncodeToString(wire) {
		t.Fatal("idle execution receipt lost its exact signed bytes")
	}
	t.Log("idle signed wire verified: simulation, Squads constraint rejection, unchanged reserve, exact deposit and replay; no durable idle lifecycle claim")
}
