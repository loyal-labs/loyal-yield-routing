package backyardrwa

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
)

// The strategy receipt's absence is judged by the real RPC decoder and the
// finalized gate, so these tests feed raw JSON-RPC responses to RPCClient and
// drive the same batch function the production observe path wires.

type jsonRPCAccounts struct {
	slot      int64
	confirmed map[string]ConfirmedAccount
	finalized map[string]ConfirmedAccount
}

func (f *jsonRPCAccounts) transport() roundTripFunc {
	return roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		var payload struct {
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			return nil, err
		}
		var result string
		switch payload.Method {
		case "getSlot":
			result = fmt.Sprintf(`%d`, f.slot)
		case "getProgramAccounts":
			// No open withdrawal receipts, so the queue demand is zero.
			result = fmt.Sprintf(`{"context":{"slot":%d},"value":[]}`, f.slot)
		case "getMultipleAccounts":
			var addresses []string
			if err := json.Unmarshal(payload.Params[0], &addresses); err != nil {
				return nil, err
			}
			var config struct {
				Commitment string `json:"commitment"`
			}
			if err := json.Unmarshal(payload.Params[1], &config); err != nil {
				return nil, err
			}
			source := f.confirmed
			if config.Commitment == "finalized" {
				source = f.finalized
			}
			values := make([]string, 0, len(addresses))
			for _, address := range addresses {
				account, found := source[address]
				if !found {
					values = append(values, "null")
					continue
				}
				values = append(values, fmt.Sprintf(`{"owner":%q,"lamports":%d,"executable":false,"data":[%q,"base64"]}`,
					account.Owner, account.Lamports, base64.StdEncoding.EncodeToString(account.Data)))
			}
			result = fmt.Sprintf(`{"context":{"slot":%d},"value":[%s]}`, f.slot, strings.Join(values, ","))
		default:
			return nil, fmt.Errorf("unexpected RPC method %s", payload.Method)
		}
		return response(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"result":%s}`, result)), nil
	})
}

// rawJSONRouteBatch serves the production batch accounts as real JSON-RPC. The
// request's own address list drives the response, so an account that is absent
// from the maps is answered with a genuine null entry.
func rawJSONRouteBatch(t *testing.T, mutate func(map[string]ConfirmedAccount, map[string]ConfirmedAccount)) *RPCClient {
	t.Helper()
	manifest := readyWorkerManifest(t)
	fixture := &jsonRPCAccounts{slot: 77, confirmed: map[string]ConfirmedAccount{}, finalized: map[string]ConfirmedAccount{}}
	// Every pinned address the batch requests must exist at both commitments
	// unless a test removes it: the neutral envelopes keep undecoded helper
	// accounts present while the fixture provides the real images.
	for _, address := range routeFixedAddresses(manifest) {
		neutral := ConfirmedAccount{Address: address, Owner: bridgeSquadsProgram, Lamports: 1, Data: bytes.Repeat([]byte{7}, 96)}
		fixture.confirmed[address] = neutral
		fixture.finalized[address] = neutral
	}
	for _, account := range productionRouteBatchAccounts(t, 77, nil) {
		fixture.confirmed[account.Address] = account
		fixture.finalized[account.Address] = account
	}
	if mutate != nil {
		mutate(fixture.confirmed, fixture.finalized)
	}
	client, err := NewRPCClient("https://rpc.invalid")
	if err != nil {
		t.Fatal(err)
	}
	client.client.Transport = fixture.transport()
	return client
}

func rawJSONProductionObserve(t *testing.T, client *RPCClient) (Observation, error) {
	t.Helper()
	state := productionObserveState{
		routeKey: "rwa-multiply:test",
		journal:  &stubProductionJournal{journal: reconciledJournal()},
		batch: func(ctx context.Context) (Observation, error) {
			return ObserveConfirmedRouteSnapshot(ctx, client, readyWorkerManifest(t))
		},
		identity: pinnedIdentityObservation,
	}
	return state.observe(context.Background())
}

func TestConstructionRefreshUsesFullyMergedVerifiedSnapshot(t *testing.T) {
	client := rawJSONRouteBatch(t, nil)
	manifest := readyWorkerManifest(t)
	state := productionObserveState{
		routeKey: productionRouteKey,
		journal:  &stubProductionJournal{journal: reconciledJournal()},
		identity: pinnedIdentityObservation,
	}
	observation, _, err := observeConfirmedRouteSnapshotWithRPCAccountsAndEnrichment(
		context.Background(), client, manifest, state.enrich,
	)
	if err != nil {
		t.Fatalf("construction refresh snapshot failed: %v", err)
	}
	if !observation.Snapshot.ProgramIdentityKnown ||
		observation.Snapshot.VoltrProgramDeploySlot != voltrProgramDeploySlot ||
		observation.Snapshot.AdaptorProgramDeploySlot != adaptorProgramDeploySlot {
		t.Fatalf("verified program identity was not merged into the refresh: %+v", observation.Snapshot)
	}
	decision := Decide(observation.Snapshot)
	if decision.Action == HoldManualRecovery {
		t.Fatalf("a healthy verified construction refresh held: %+v", decision)
	}
	if decision.Action != DeleverPrimeUSDCStep {
		t.Fatalf("the healthy construction refresh did not retain the actionable decision: %+v", decision)
	}
}

func TestKaminoConstructionReturnsMergedRefreshHoldBeforeConstruction(t *testing.T) {
	client := rawJSONRouteBatch(t, nil)
	manifest := readyWorkerManifest(t)
	unverified := func(context.Context) (programIdentityObservation, error) {
		return programIdentityObservation{}, nil
	}
	state := productionObserveState{
		routeKey: productionRouteKey,
		journal:  &stubProductionJournal{journal: reconciledJournal()},
		identity: unverified,
	}
	decision := Decision{
		Action:         DeleverPrimeUSDCStep,
		Reason:         "hard_ltv_repay",
		AmountRaw:      1,
		IdempotencyKey: "kamino-refresh-regression",
		StrategyKey:    RouteID,
	}
	observation, evidence, err := observeConfirmedKaminoExecutionEvidenceWithEnrichment(
		context.Background(), client, manifest, decision, state.enrich,
	)
	if err != nil {
		t.Fatalf("Kamino construction discarded the refreshed hold: %v", err)
	}
	if evidence.Request.Action != "" {
		t.Fatalf("Kamino construction produced wire evidence after a hold: %+v", evidence)
	}
	refreshedDecision := Decide(observation.Snapshot)
	if refreshedDecision.Action != HoldManualRecovery || refreshedDecision.Reason != "program_identity_unverified" {
		t.Fatalf("the refreshed Kamino snapshot did not hold on identity: %+v", refreshedDecision)
	}
}

func TestStrategyReceiptIntegrityThroughRawJSONRPC(t *testing.T) {
	// A confirmed null with a finalized receipt is a replication artifact: the
	// tick errors and no hold is produced.
	client := rawJSONRouteBatch(t, func(confirmed, _ map[string]ConfirmedAccount) {
		delete(confirmed, bridgeStrategyReceipt)
	})
	if _, err := rawJSONProductionObserve(t, client); err == nil || !strings.Contains(err.Error(), "present at finalized") {
		t.Fatalf("a confirmed null with a finalized receipt must stay a tick error: %v", err)
	}

	// A null at both commitments is settled ledger state: the integrity fault
	// reaches Decide and becomes the durable hold.
	client = rawJSONRouteBatch(t, func(confirmed, finalized map[string]ConfirmedAccount) {
		delete(confirmed, bridgeStrategyReceipt)
		delete(finalized, bridgeStrategyReceipt)
	})
	observation, err := rawJSONProductionObserve(t, client)
	if err != nil {
		t.Fatalf("a receipt absent from both commitments must observe: %v", err)
	}
	decision := Decide(observation.Snapshot)
	if decision.Action != HoldManualRecovery || decision.Reason != "strategy_receipt_integrity" {
		t.Fatalf("a finalized absent receipt did not hold: %+v", decision)
	}

	// A receipt of the wrong length is broken regardless of commitment.
	client = rawJSONRouteBatch(t, func(confirmed, _ map[string]ConfirmedAccount) {
		receipt := confirmed[bridgeStrategyReceipt]
		receipt.Data = receipt.Data[:100]
		confirmed[bridgeStrategyReceipt] = receipt
	})
	observation, err = rawJSONProductionObserve(t, client)
	if err != nil {
		t.Fatalf("a truncated receipt must observe: %v", err)
	}
	if decision = Decide(observation.Snapshot); decision.Action != HoldManualRecovery || decision.Reason != "strategy_receipt_integrity" {
		t.Fatalf("a truncated receipt did not hold: %+v", decision)
	}

	// A receipt owned by another program is foreign evidence.
	client = rawJSONRouteBatch(t, func(confirmed, _ map[string]ConfirmedAccount) {
		receipt := confirmed[bridgeStrategyReceipt]
		receipt.Owner = kaminoProgram
		confirmed[bridgeStrategyReceipt] = receipt
	})
	observation, err = rawJSONProductionObserve(t, client)
	if err != nil {
		t.Fatalf("a foreign-owned receipt must observe: %v", err)
	}
	if decision = Decide(observation.Snapshot); decision.Action != HoldManualRecovery || decision.Reason != "strategy_receipt_integrity" {
		t.Fatalf("a foreign-owned receipt did not hold: %+v", decision)
	}

	// The untouched fixture stays decodable and holds for no integrity reason.
	client = rawJSONRouteBatch(t, nil)
	observation, err = rawJSONProductionObserve(t, client)
	if err != nil {
		t.Fatalf("a healthy raw JSON-RPC batch must observe: %v", err)
	}
	if observation.Snapshot.StrategyReceiptIntegrityFault {
		t.Fatalf("a healthy batch was classified as an integrity fault: %+v", observation.Snapshot)
	}
}

func TestAbsentConfirmedReceiptNeedsFinalizedConfirmation(t *testing.T) {
	manifest := readyWorkerManifest(t)
	accounts := productionRouteBatchAccounts(t, 77, func(batch []ConfirmedAccount) {
		replaceAccount(batch, bridgeStrategyReceipt, ConfirmedAccount{})
	})
	accountsReader, _ := fixtureBatchRuntime(77, accounts)
	receipt := strategyReceiptFixture(t, 42)
	observation, _, err := observeConfirmedRouteSnapshotWithAccounts(context.Background(), manifest, routeObservationRuntime{
		confirmedSlot: func(context.Context) (int64, error) { return 77, nil },
		receipts: func(_ context.Context, _ int64) (int64, []programAccount, error) {
			return 77, nil, nil
		},
		accounts: accountsReader,
		finalizedReceipt: func(_ context.Context, _ int64) (int64, []ConfirmedAccount, error) {
			return 78, []ConfirmedAccount{receipt}, nil
		},
		now: func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
	})
	if err == nil || !strings.Contains(err.Error(), "present at finalized") {
		t.Fatalf("a transient confirmed absence must stay a tick error: %+v %v", observation, err)
	}
}
