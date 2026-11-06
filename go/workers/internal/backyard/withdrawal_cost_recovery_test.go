package backyard

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/earnmax"
)

func disclosedWithdrawalFixture(t *testing.T, d *Database, id string) (PersistedOperation, SignedTransaction, []byte, finalizedFailureReceipt, publicKey) {
	t.Helper()
	receipt, wire, delegate := failureSettlementFixture(t)
	message, signature, err := splitSignedWire(wire)
	if err != nil {
		t.Fatal(err)
	}
	m, err := decodeV1Message(message)
	if err != nil {
		t.Fatal(err)
	}
	signed := SignedTransaction{message: message, signedWire: wire, messageSHA256: sha256Bytes(message), signedWireSHA256: sha256Bytes(wire), transactionSignature: encodeBase58(signature), recentBlockhash: m.RecentBlockhash.String(), lastValidBlockHeight: 99}
	c := earnmax.WithdrawalConsent{Authority: "authority", WithdrawalGeneration: earnmax.WithdrawalGeneration{RequestSignature: "request", RequestInstruction: 2}, Version: "1", ReferenceUSDC: "1000", MinClaimUSDC: "990", MaxUserFeeLamports: "0", ExpiresAt: fmt.Sprint(time.Now().Add(time.Hour).Unix()), Nonce: strings.Repeat("a", 64)}
	consentID := insertWithdrawalCostConsent(t, d, c)
	p := &withdrawalCostPreparation{EffectiveLP: 100, MaxCost: 6, IncrementalCost: 6, NetworkUSDC: withdrawalTestUSDC(1), NetworkLamports: 5000, MessageSHA256: signed.messageSHA256, Participants: []withdrawalCostParticipant{{Consent: c, ID: consentID, EscrowLP: 100, Payable: 1000, MaxCost: 6, IncrementalCost: 6}}}
	if _, err := d.pool.Exec(context.Background(), `INSERT INTO loyal_yield.multiply_operations(operation_id,route_key,status,expected_effects) VALUES($1,$2,'decided','{}')`, id, rwa.RouteKey); err != nil {
		t.Fatal(err)
	}
	if err := d.reserveWithdrawalCostRecord(context.Background(), id, p); err != nil {
		t.Fatal(err)
	}
	effects, err := json.Marshal(ExpectedEffects{Schema: "loyal-backyard-rwa-expected-effects/v1", Conserved: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.bindWithdrawalCostWire(context.Background(), id, signed, effects); err != nil {
		t.Fatal(err)
	}
	op := PersistedOperation{Operation: Operation{ID: id, RouteKey: rwa.RouteKey}, Status: Decided, ExpectedEffects: []byte(`{}`)}
	return op, signed, effects, receipt, delegate
}

func TestWithdrawalCostDatabaseSimulationDisclosureCrashRetainsExactWire(t *testing.T) {
	d := withdrawalCostTestDB(t)
	ctx := context.Background()
	op, signed, _, _, delegate := disclosedWithdrawalFixture(t, d, "disclosed")
	simulations := 0
	client := newFakeChain(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
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
			result = map[string]any{"absoluteSlot": 77, "blockHeight": 1, "epoch": 1, "slotIndex": 1, "slotsInEpoch": 100}
		case "getSignatureStatuses":
			result = map[string]any{"context": map[string]any{"slot": 77}, "value": []any{nil}}
		case "simulateTransaction":
			var encoded string
			if err := json.Unmarshal(q.Params[0], &encoded); err != nil {
				return nil, err
			}
			wire, err := base64.StdEncoding.DecodeString(encoded)
			if err != nil {
				return nil, err
			}
			var stored []byte
			if err := d.pool.QueryRow(ctx, `SELECT signed_wire FROM loyal_yield.backyard_withdrawal_cost_operations WHERE operation_id=$1`, op.ID).Scan(&stored); err != nil {
				return nil, err
			}
			if !bytes.Equal(wire, signed.signedWire) || !bytes.Equal(stored, wire) {
				return nil, fmt.Errorf("RPC received wire absent from durable journal")
			}
			simulations++
			if simulations == 1 {
				return nil, fmt.Errorf("fixture RPC disconnected after receiving signed wire")
			}
			result = map[string]any{"context": map[string]any{"slot": 77}, "value": map[string]any{"err": nil, "unitsConsumed": 1, "logs": []string{}}}
		default:
			return nil, fmt.Errorf("unexpected disclosure recovery RPC %s", q.Method)
		}
		raw, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": q.ID, "result": result})
		if err != nil {
			return nil, err
		}
		return response(string(raw)), nil
	}))
	if handled, err := d.recoverDisclosedWithdrawalCostForDelegate(ctx, rwa.manifest, client, op, delegate); !handled || err == nil {
		t.Fatal("disclosed RPC failure lost recoverable operation")
	}
	var status, state string
	var opWire []byte
	if err := d.pool.QueryRow(ctx, `SELECT o.status,o.signed_wire,c.state FROM loyal_yield.multiply_operations o JOIN loyal_yield.backyard_withdrawal_cost_operations c USING(operation_id) WHERE operation_id=$1`, op.ID).Scan(&status, &opWire, &state); err != nil {
		t.Fatal(err)
	}
	if status != "decided" || len(opWire) != 0 || state != "reserved" {
		t.Fatalf("disclosure fabricated landing or released expense: %s/%s", status, state)
	}
	if _, err := d.pool.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET status='failed' WHERE operation_id=$1`, op.ID); err != nil {
		t.Fatal(err)
	}
	if err := d.releaseWithdrawalCost(ctx, op.ID); err != nil {
		t.Fatal(err)
	}
	if err := d.pool.QueryRow(ctx, `SELECT state FROM loyal_yield.backyard_withdrawal_cost_operations WHERE operation_id=$1`, op.ID).Scan(&state); err != nil || state != "reserved" {
		t.Fatal("signed RPC disclosure treated as unsigned despite durable hash")
	}
	if _, err := d.pool.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET status='decided' WHERE operation_id=$1`, op.ID); err != nil {
		t.Fatal(err)
	}
	if handled, err := d.recoverDisclosedWithdrawalCostForDelegate(ctx, rwa.manifest, client, op, delegate); !handled || err != nil {
		t.Fatalf("exact stored-wire retry failed: %v", err)
	}
	if err := d.pool.QueryRow(ctx, `SELECT status,signed_wire FROM loyal_yield.multiply_operations WHERE operation_id=$1`, op.ID).Scan(&status, &opWire); err != nil {
		t.Fatal(err)
	}
	if status != "signed" || !bytes.Equal(opWire, signed.signedWire) || simulations != 2 {
		t.Fatal("recovery replaced wire or fabricated simulation")
	}
}

func TestWithdrawalCostDatabaseDisclosedFailureAndManualLandingSatisfyProductionConstraint(t *testing.T) {
	d := withdrawalCostTestDB(t)
	ctx := context.Background()
	op, signed, effects, receipt, delegate := disclosedWithdrawalFixture(t, d, "failed-disclosure")
	if err := d.settleDisclosedWithdrawalFailure(ctx, op, signed, effects, receipt, delegate); err != nil {
		t.Fatalf("real 0113 finalized failure transition rejected: %v", err)
	}
	var status string
	var intent *time.Time
	var fee string
	if err := d.pool.QueryRow(ctx, `SELECT status,broadcast_intent_at,reconciled_effects->>'feeLamports' FROM loyal_yield.multiply_operations WHERE operation_id=$1`, op.ID).Scan(&status, &intent, &fee); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || intent != nil || fee != "5000" {
		t.Fatal("disclosed failure fabricated send intent or lost actual fee")
	}
	client := newFakeChain(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(request.Body)
		var q struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.Unmarshal(body, &q); err != nil {
			return nil, err
		}
		if q.Method != "getTransaction" {
			return nil, fmt.Errorf("unexpected finalized failure RPC %s", q.Method)
		}
		raw, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": q.ID, "result": receipt})
		if err != nil {
			return nil, err
		}
		return response(string(raw)), nil
	}))
	if err := d.settleWithdrawalCost(ctx, client, op.ID); err != nil {
		t.Fatalf("finalized operator fee settlement failed: %v", err)
	}
	var poolCost, incremental, operatorUSD int64
	if err := d.pool.QueryRow(ctx, `SELECT actual_cost_usdc,actual_incremental_cost_usdc,actual_network_usdc FROM loyal_yield.backyard_withdrawal_cost_operations WHERE operation_id=$1`, op.ID).Scan(&poolCost, &incremental, &operatorUSD); err != nil {
		t.Fatal(err)
	}
	if poolCost != 0 || incremental != 0 || operatorUSD != 1 {
		t.Fatalf("operator failure fee consumed user expense budget: pool=%d increment=%d operator=%d", poolCost, incremental, operatorUSD)
	}
	if _, err := d.pool.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET status='decided',signed_wire=NULL,transaction_signature=NULL,simulation_result=NULL WHERE operation_id=$1`, op.ID); err != nil {
		t.Fatal(err)
	}
	if err := d.hydrateDisclosedWithdrawalOperation(ctx, &op, signed, effects, ManualRecovery, receipt.Slot); err != nil {
		t.Fatalf("real 0113 manual unexpected landing rejected: %v", err)
	}
	var reason string
	var simulation []byte
	if err := d.pool.QueryRow(ctx, `SELECT status,broadcast_intent_at,simulation_result,recovery_reason FROM loyal_yield.multiply_operations WHERE operation_id=$1`, op.ID).Scan(&status, &intent, &simulation, &reason); err != nil {
		t.Fatal(err)
	}
	if status != "manual_recovery" || intent != nil || len(simulation) != 0 || reason != "signed_wire_landed_after_simulation_disclosure" {
		t.Fatal("unexpected landing fabricated prior simulation/broadcast intent")
	}
}
