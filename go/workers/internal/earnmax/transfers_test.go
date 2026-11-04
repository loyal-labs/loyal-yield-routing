package earnmax

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/solana-foundation/solana-go/v2"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/voltr"
)

func transferPriceEvents(t *testing.T) map[string]string {
	t.Helper()
	raw, err := os.ReadFile("testdata/transfer-prices.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Events []struct{ Event, Data string }
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, event := range fixture.Events {
		out[event.Event] = event.Data
	}
	return out
}

func vaultPriceLogs(encoded string) []string {
	program := voltr.ProgramID.String()
	return []string{"Program " + program + " invoke [1]", programData + encoded, "Program " + program + " success"}
}

func TestTransferPricesUseVaultEventsAndRejectMissingEvidence(t *testing.T) {
	for name, data := range transferPriceEvents(t) {
		t.Run(name, func(t *testing.T) {
			value, supply, ok := transferShareValue(vaultPriceLogs(data))
			if !ok || value != 980_000_000 || supply != 100 {
				t.Fatalf("price=%d/%d known=%v", value, supply, ok)
			}
			raw, _ := base64.StdEncoding.DecodeString(data)
			raw[8] ^= 1
			// Deposit/withdraw user events carry the vault at byte 56;
			// strategy events at 40, request events at 8.
			vaultAt := 56
			if name == "RequestWithdrawVaultEvent" {
				vaultAt = 8
			} else if name == "DepositStrategyEvent" || name == "WithdrawStrategyEvent" {
				vaultAt = 40
			}
			raw[vaultAt] ^= 2
			cases := [][]string{
				{programData + data},
				{"Program " + solana.TokenProgramID.String() + " invoke [1]", programData + data, "Program " + solana.TokenProgramID.String() + " success"},
				vaultPriceLogs(base64.StdEncoding.EncodeToString(raw)),
				vaultPriceLogs(base64.StdEncoding.EncodeToString(raw[:len(raw)-1])),
				vaultPriceLogs("invalid base64"),
				vaultPriceLogs(data)[:2],
				{"Program " + voltr.ProgramID.String() + " invoke [1]", programData + data, "Program " + voltr.ProgramID.String() + " failed: rejected"},
			}
			for _, logs := range cases {
				if _, _, ok := transferShareValue(logs); ok {
					t.Fatal("accepted an unbound, malformed or incomplete price event")
				}
			}
		})
	}
}

func TestHistoryFindsIncomingSharesAndRetriesUnknownPrice(t *testing.T) {
	user, err := voltr.UserAccounts(Vault, LPMint, recordedAuthority.key)
	if err != nil {
		t.Fatal(err)
	}
	transferSig, priceSig := solana.Signature{1}, solana.Signature{2}
	sender, source := solana.PublicKey{3}, solana.PublicKey{4}
	data := append([]byte{splTransferChecked}, binary.LittleEndian.AppendUint64(nil, 20)...)
	data = append(data, 9)
	transfer := solana.Transaction{Signatures: []solana.Signature{transferSig}, Message: solana.Message{
		Header: solana.MessageHeader{NumRequiredSignatures: 1}, AccountKeys: solana.PublicKeySlice{sender, source, LPMint, user.LPATA, solana.TokenProgramID},
		Instructions: []solana.CompiledInstruction{{ProgramIDIndex: 4, Accounts: []uint16{1, 2, 3, 0}, Data: data}},
	}}
	price := solana.Transaction{Signatures: []solana.Signature{priceSig}, Message: solana.Message{
		Header: solana.MessageHeader{NumRequiredSignatures: 1}, AccountKeys: solana.PublicKeySlice{sender, Vault, voltr.ProgramID},
		Instructions: []solana.CompiledInstruction{{ProgramIDIndex: 2, Accounts: []uint16{0, 1}, Data: voltr.DepositStrategyDiscriminator[:]}},
	}}
	balance := func(index int, owner solana.PublicKey, amount int) map[string]any {
		return map[string]any{"accountIndex": index, "owner": owner.String(), "mint": LPMint.String(), "programId": solana.TokenProgramID.String(), "uiTokenAmount": map[string]any{"amount": strconv.Itoa(amount), "decimals": 9}}
	}
	transaction := func(tx solana.Transaction, slot uint64, at int64, pre, post []map[string]any, logs []string) map[string]any {
		wire, err := tx.MarshalBinary()
		if err != nil {
			t.Fatal(err)
		}
		return map[string]any{"slot": slot, "blockTime": at, "transaction": []string{base64.StdEncoding.EncodeToString(wire), "base64"},
			"meta": map[string]any{"err": nil, "fee": 5000, "preBalances": []int{}, "postBalances": []int{}, "innerInstructions": []any{}, "logMessages": logs, "preTokenBalances": pre, "postTokenBalances": post}}
	}
	at := earningsTime(2).Unix()
	transferTx := transaction(transfer, 200, at, []map[string]any{balance(1, sender, 20)}, []map[string]any{balance(1, sender, 0), balance(3, recordedAuthority.key, 20)}, []string{})
	priceTx := transaction(price, 190, at-10, nil, nil, vaultPriceLogs(transferPriceEvents(t)["DepositStrategyEvent"]))
	var priceAvailable atomic.Bool
	var priceReads, transactionReads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage
			Method string
			Params []json.RawMessage
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		var address string
		_ = json.Unmarshal(req.Params[0], &address)
		var result any
		switch req.Method {
		case "getSignaturesForAddress":
			result = []any{}
			if address == user.LPATA.String() {
				result = []any{map[string]any{"signature": transferSig.String(), "slot": 200, "blockTime": at}}
			} else if address == Vault.String() {
				priceReads.Add(1)
				var opts struct{ Before string }
				_ = json.Unmarshal(req.Params[1], &opts)
				if opts.Before != transferSig.String() {
					t.Error("price lookup must be anchored before the transfer")
				}
				if priceAvailable.Load() {
					result = []any{map[string]any{"signature": priceSig.String(), "slot": 190, "blockTime": at - 10}}
				}
			}
		case "getTransaction":
			transactionReads.Add(1)
			if address == transferSig.String() {
				result = transferTx
			} else if address == priceSig.String() {
				result = priceTx
			}
		default:
			t.Errorf("unexpected RPC %s", req.Method)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}))
	defer server.Close()
	client, err := chain.New(server.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	reader := NewReader(client, nil)
	read := func() Transfer {
		t.Helper()
		h, err := reader.History(context.Background(), recordedAuthority, 200)
		if err != nil || !h.Complete() || len(h.Entries) != 1 {
			t.Fatalf("history=%+v error=%v", h, err)
		}
		transfer, ok := h.Entries[0].(Transfer)
		if !ok || transfer.LPDelta != 20 {
			t.Fatalf("missing token-account transfer: %+v", h.Entries)
		}
		return transfer
	}
	if read().Amount != nil {
		t.Fatal("missing historical price was guessed")
	}
	priceAvailable.Store(true)
	got := read()
	if got.Amount == nil || *got.Amount != 196_000_000 || got.PoolLPBefore != 100 {
		t.Fatalf("transfer value=%+v", got)
	}
	read()
	if priceReads.Load() != 2 || transactionReads.Load() != 3 {
		t.Fatal("a verified transfer was not cached, or an unknown price was cached permanently")
	}
}

func TestMergedHistoryPreservesOrderWithoutDoubleCounting(t *testing.T) {
	a := chain.Signed{Signature: solana.Signature{1}, Slot: 10}
	b := chain.Signed{Signature: solana.Signature{2}, Slot: 10}
	c := chain.Signed{Signature: solana.Signature{3}, Slot: 20}
	rows, complete := mergeHistory([][]chain.Signed{{a}, {c, b, a}})
	if !complete || len(rows) != 3 || rows[0] != c || rows[1] != b || rows[2] != a {
		t.Fatalf("merged history lost RPC order or duplicated a deposit: %+v", rows)
	}
	if _, complete := mergeHistory([][]chain.Signed{{a}, {b}}); complete {
		t.Fatal("invented ordering between unrelated same-slot flows")
	}
}
