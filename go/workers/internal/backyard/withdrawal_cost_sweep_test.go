package backyard

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/earnmax"
)

func withdrawalSweepRPC(t *testing.T, receipt finalizedFailureReceipt, mode func(string) string) *chain.Client {
	t.Helper()
	return newFakeChain(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(request.Body)
		var q struct {
			ID     json.RawMessage   `json:"id"`
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(body, &q); err != nil {
			return nil, err
		}
		var result any
		switch q.Method {
		case "getEpochInfo":
			result = map[string]any{"absoluteSlot": 80, "blockHeight": 1000, "epoch": 1, "slotIndex": 1, "slotsInEpoch": 100}
		case "getSignatureStatuses":
			var sigs []string
			if err := json.Unmarshal(q.Params[0], &sigs); err != nil {
				return nil, err
			}
			var status any
			switch mode(sigs[0]) {
			case "blocked":
				<-request.Context().Done()
				return nil, request.Context().Err()
			case "failed":
				status = map[string]any{"slot": receipt.Slot, "confirmationStatus": "finalized", "confirmations": nil, "err": map[string]any{"InstructionError": []any{0, map[string]any{"Custom": 9}}}}
			}
			result = map[string]any{"context": map[string]any{"slot": 80}, "value": []any{status}}
		case "getTransaction":
			result = receipt
		case "isBlockhashValid":
			result = map[string]any{"context": map[string]any{"slot": 80}, "value": false}
		default:
			return nil, fmt.Errorf("unexpected settlement RPC %s", q.Method)
		}
		raw, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": q.ID, "result": result})
		if err != nil {
			return nil, err
		}
		return response(string(raw)), nil
	}))
}

func insertSweepClaim(t *testing.T, d *Database, consentID string, signed SignedTransaction) string {
	t.Helper()
	id := sha256Bytes(signed.signedWire)
	if _, err := d.pool.Exec(context.Background(), `INSERT INTO loyal_yield.earn_max_withdrawal_claim_attempts(attempt_id,consent_id,authority,request_signature,request_instruction,signed_wire,transaction_signature,blockhash,last_valid_block_height,reserved_fee_lamports) SELECT $1,consent_id,authority,request_signature,request_instruction,$3,$4,$5,99,5000 FROM loyal_yield.earn_max_withdrawal_consents WHERE consent_id=$2`, id, consentID, signed.signedWire, signed.transactionSignature, signed.recentBlockhash); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestWithdrawalCostDatabaseEngineSweepsWithoutDemandOrNewSigning(t *testing.T) {
	d := withdrawalCostTestDB(t)
	ctx := context.Background()
	op, signed, effects, receipt, delegate := disclosedWithdrawalFixture(t, d, "terminal-without-demand")
	if err := d.settleDisclosedWithdrawalFailure(ctx, op, signed, effects, receipt, delegate); err != nil {
		t.Fatal(err)
	}
	var consentID string
	if err := d.pool.QueryRow(ctx, `SELECT consent_id FROM loyal_yield.earn_max_withdrawal_consents LIMIT 1`).Scan(&consentID); err != nil {
		t.Fatal(err)
	}
	attemptID := insertSweepClaim(t, d, consentID, signed)
	var beforeClaim, beforeCost, afterClaim, afterCost string
	read := func(claim, cost *string) {
		t.Helper()
		if err := d.pool.QueryRow(ctx, `SELECT a.state,c.state FROM loyal_yield.earn_max_withdrawal_claim_attempts a CROSS JOIN loyal_yield.backyard_withdrawal_cost_operations c WHERE a.attempt_id=$1 AND c.operation_id=$2`, attemptID, op.ID).Scan(claim, cost); err != nil {
			t.Fatal(err)
		}
	}
	read(&beforeClaim, &beforeCost)
	newWithdrawalSettlementWorker(d, newFakeChain(t, nil), nil, looping).sweep(ctx)
	read(&afterClaim, &afterCost)
	if beforeClaim != afterClaim || beforeCost != afterCost {
		t.Fatal("Backyard settled Loyal's pending claim or cost")
	}
	client := withdrawalSweepRPC(t, receipt, func(string) string { return "failed" })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	events := newEvents(log, nil, "rwa")
	var ticks atomic.Int64
	w := &Worker{routeKey: rwa.RouteKey, manifest: rwa.manifest, interval: time.Millisecond, wake: make(chan struct{}, 1), events: events, holds: holdLog{events: events}, runtime: tickRuntime{loadLatch: func(context.Context, string) (ManualRecoveryLatch, bool, error) {
		ticks.Add(1)
		return ManualRecoveryLatch{Reason: "fixture_no_new_demand"}, true, nil
	}}}
	e := &Engine{workers: []*Worker{w}, states: []*vaultState{{events: events}}, database: d, rpc: client, out: io.Discard, runtime: EngineConfig{Logger: log, Selector: SelectorOff}}
	run, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- e.Run(run) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("engine settlement lane did not join")
		}
	})
	waitFor(t, func() bool {
		var claim, cost string
		err := d.pool.QueryRow(ctx, `SELECT a.state,c.state FROM loyal_yield.earn_max_withdrawal_claim_attempts a CROSS JOIN loyal_yield.backyard_withdrawal_cost_operations c WHERE a.attempt_id=$1 AND c.operation_id=$2`, attemptID, op.ID).Scan(&claim, &cost)
		return err == nil && claim == "settled" && cost == "settled"
	})
	if ticks.Load() == 0 {
		t.Fatal("background settlement blocked serialized vault worker")
	}
	var fee string
	if err := d.pool.QueryRow(ctx, `SELECT actual_fee_lamports::text FROM loyal_yield.earn_max_withdrawal_claim_attempts WHERE attempt_id=$1`, attemptID).Scan(&fee); err != nil || fee != "5000" {
		t.Fatal("idle sweep lost finalized wallet fee")
	}
}

func TestWithdrawalCostDatabaseSweepTimeoutDoesNotStarveLaterWireAndJoins(t *testing.T) {
	d := withdrawalCostTestDB(t)
	ctx := context.Background()
	c := earnmax.WithdrawalConsent{Authority: "authority", WithdrawalGeneration: earnmax.WithdrawalGeneration{RequestSignature: "request", RequestInstruction: 2}, Version: "1", ReferenceUSDC: "1000", MinClaimUSDC: "990", MaxUserFeeLamports: "10000", ExpiresAt: fmt.Sprint(time.Now().Add(time.Hour).Unix()), Nonce: strings.Repeat("a", 64)}
	consentID := insertWithdrawalCostConsent(t, d, c)
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{37}, ed25519.SeedSize))
	delegate := publicKeyFromBytes(key.Public().(ed25519.PublicKey))
	var candidates []SignedTransaction
	for _, hash := range []string{bridgeVault, backyardSettings} {
		r := bridgeTestRequest(ReportNAV, 0)
		r.RecentBlockhash = hash
		r = bindBridgeRequest(r)
		signed, err := buildAndSignBridgeTransactionForDelegate(r, key, delegate)
		if err != nil {
			t.Fatal(err)
		}
		candidates = append(candidates, signed)
	}
	sort.Slice(candidates, func(i, j int) bool {
		return sha256Bytes(candidates[i].signedWire) < sha256Bytes(candidates[j].signedWire)
	})
	c.RequestSignature = "other-request"
	c.Nonce = strings.Repeat("b", 64)
	otherConsentID := insertWithdrawalCostConsent(t, d, c)
	ids := []string{insertSweepClaim(t, d, consentID, candidates[0]), insertSweepClaim(t, d, otherConsentID, candidates[1])}
	client := withdrawalSweepRPC(t, finalizedFailureReceipt{}, func(sig string) string {
		if sig == candidates[0].transactionSignature {
			return "blocked"
		}
		return "expired"
	})
	w := newWithdrawalSettlementWorker(d, client, slog.New(slog.NewTextHandler(io.Discard, nil)), rwa)
	w.batchSize = 1
	w.attemptTimeout = 20 * time.Millisecond
	w.interval = time.Millisecond
	start := time.Now()
	w.sweep(ctx)
	if time.Since(start) > time.Second {
		t.Fatal("attempt timeout did not bound settlement")
	}
	w.sweep(ctx)
	var state string
	if err := d.pool.QueryRow(ctx, `SELECT state FROM loyal_yield.earn_max_withdrawal_claim_attempts WHERE attempt_id=$1`, ids[1]).Scan(&state); err != nil || state != "released" {
		t.Fatal("stuck older attempt starved later proven expiry")
	}
	run, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- w.Run(run) }()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("settlement worker ignored cancellation")
	}
}
