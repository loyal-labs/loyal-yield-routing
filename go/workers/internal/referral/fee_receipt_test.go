package referral

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/earnmax"
)

func TestFeeReceiptFinalizedNAVWire(t *testing.T) {
	fixture, err := os.ReadFile("testdata/fee-nav-471419563.json")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Method != "getTransaction" {
			t.Error("unexpected RPC request")
		}
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": json.RawMessage(fixture)})
	}))
	defer server.Close()
	c, err := chain.New(server.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	sig := solana.MustSignatureFromBase58("pFTyRHueuVM41inJoKttMGNYj6aGB2Y7NRu5qGgATyXrbCG8KTHK2Kj1jQW3Pg6PbGicabxBZJmnCPWnJxBgZHE")
	e, err := c.Execution(context.Background(), sig, rpc.CommitmentFinalized)
	if err != nil {
		t.Fatal(err)
	}
	point := FeePoint{Signature: sig.String(), Slot: 471419563, Index: 202, At: *e.BlockTime}
	treasury, err := feeTreasury()
	if err != nil {
		t.Fatal(err)
	}
	event, err := decodeFeeEvent(e, point, treasury)
	if err != nil || event.kind != "strategy" || event.fee != 0 || event.supplyBefore != 210456986 {
		t.Fatalf("finalized NAV: %+v %v", event, err)
	}
	var eventIx int
	for i, line := range e.Logs {
		if strings.HasPrefix(line, "Program data: ") {
			eventIx = i
			break
		}
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(e.Logs[eventIx], "Program data: "))
	if err != nil {
		t.Fatal(err)
	}
	// A synthetic isolated assessment changes the effective supply and HWM on
	// the same real deployed wire. A forged/corrupted event frame stays held.
	binary.LittleEndian.PutUint64(raw[232:], 210457086)
	binary.LittleEndian.PutUint64(raw[256:], binary.LittleEndian.Uint64(raw[240:])+5)
	e.Logs[eventIx] = "Program data: " + base64.StdEncoding.EncodeToString(raw)
	event, err = decodeFeeEvent(e, point, treasury)
	if err != nil || event.fee != 100 {
		t.Fatalf("isolated assessment: %+v %v", event, err)
	}
	e.Logs[eventIx] = "Program data: " + base64.StdEncoding.EncodeToString(raw[:49])
	if _, err := decodeFeeEvent(e, point, treasury); err == nil {
		t.Fatal("truncated event accepted")
	}
}

func TestFeeHarvestRequiresActualTreasuryMint(t *testing.T) {
	treasury, err := feeTreasury()
	if err != nil {
		t.Fatal(err)
	}
	mint := make([]byte, 9)
	mint[0] = 7
	binary.LittleEndian.PutUint64(mint[1:], 35)
	token := chain.TokenBalance{Program: solana.TokenProgramID, Mint: earnmax.LPMint, Owner: feeAdmin, Amount: 35}
	e := chain.Execution{Receipt: chain.Receipt{Keys: []solana.PublicKey{earnmax.LPMint, treasury}, Post: map[solana.PublicKey]chain.TokenBalance{treasury: token}}}
	calls := []feeCall{{program: solana.TokenProgramID, accounts: []uint16{0, 1}, data: mint}}
	if err := verifyFeeMint(e, calls, treasury, 35); err != nil {
		t.Fatal(err)
	}
	if err := verifyFeeMint(e, nil, treasury, 35); err == nil {
		t.Fatal("receipt without a mint accepted")
	}
	token.Amount = 34
	e.Post[treasury] = token
	if err := verifyFeeMint(e, calls, treasury, 35); err == nil {
		t.Fatal("short treasury credit accepted")
	}
}

func TestFeeReceiptIgnoresUnrelatedWritableVaultReference(t *testing.T) {
	sig := solana.Signature{1}
	program := solana.SystemProgramID
	e := chain.Execution{Receipt: chain.Receipt{Slot: 5, Keys: []solana.PublicKey{program, earnmax.Vault},
		Logs: []string{"Program " + program.String() + " invoke [1]", "Program " + program.String() + " success"}},
		Transaction: &solana.Transaction{Signatures: []solana.Signature{sig}, Message: solana.Message{
			Instructions: []solana.CompiledInstruction{{ProgramIDIndex: 0, Accounts: []uint16{1}}}}}}
	out, err := decodeFeeEvent(e, FeePoint{Signature: sig.String(), Slot: 5}, solana.PublicKey{})
	if err != nil || out.kind != "unrelated" {
		t.Fatalf("unrelated wallet reference = %+v, %v", out, err)
	}
}
