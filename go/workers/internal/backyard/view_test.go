package backyard

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	pb "github.com/helius-labs/laserstream-sdk/go/proto"
	"github.com/solana-foundation/solana-go/v2"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/observer/stream"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/voltr"
)

// fakeLaserStream is a stream.Connector whose subscriptions deliver only what
// a test sends them.
type fakeLaserStream struct {
	mu       sync.Mutex
	requests []*pb.SubscribeRequest
	streams  []*fakeSubscription
}

type fakeSubscription struct {
	ctx     context.Context
	updates chan *pb.SubscribeUpdate
	fail    chan error
}

func (f *fakeLaserStream) Open(ctx context.Context, request *pb.SubscribeRequest) (stream.OpenStream, error) {
	subscription := &fakeSubscription{ctx: ctx, updates: make(chan *pb.SubscribeUpdate, 64), fail: make(chan error, 1)}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests, f.streams = append(f.requests, request), append(f.streams, subscription)
	return subscription, nil
}

func (f *fakeLaserStream) opened() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.streams)
}

func (f *fakeLaserStream) last() (*pb.SubscribeRequest, *fakeSubscription) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[len(f.requests)-1], f.streams[len(f.streams)-1]
}

func (s *fakeSubscription) Recv() (*pb.SubscribeUpdate, error) {
	select {
	case update := <-s.updates:
		return update, nil
	case err := <-s.fail:
		return nil, err
	case <-s.ctx.Done():
		return nil, context.Canceled
	}
}
func (s *fakeSubscription) Send(*pb.SubscribeRequest) error { return nil }
func (s *fakeSubscription) CloseSend() error                { return nil }
func (s *fakeSubscription) Close() error                    { return nil }

// send delivers frames on the newest subscription and, unless it is a handoff
// candidate, waits until the view has applied them.
func (f *fakeLaserStream) send(t *testing.T, view *View, wait bool, frames ...*pb.SubscribeUpdate) {
	t.Helper()
	_, subscription := f.last()
	var top uint64
	for _, frame := range frames {
		subscription.updates <- frame
		top = max(top, frame.GetSlot().GetSlot(), frame.GetAccount().GetSlot())
	}
	if wait {
		waitFor(t, func() bool {
			view.control.Lock()
			defer view.control.Unlock()
			return view.manager != nil && view.manager.ActiveFrontier() >= top
		})
	}
}

func waitFor(t *testing.T, done func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !done(); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
	}
}

func slotFrame(slot uint64) *pb.SubscribeUpdate {
	return &pb.SubscribeUpdate{Filters: []string{viewSlotsFilter}, UpdateOneof: &pb.SubscribeUpdate_Slot{Slot: &pb.SubscribeUpdateSlot{Slot: slot, Status: pb.SlotStatus_SLOT_CONFIRMED}}}
}

func accountFrame(account ConfirmedAccount, slot, writeVersion uint64, filters ...string) *pb.SubscribeUpdate {
	owner := account.Owner
	if owner == "" {
		owner = solana.SystemProgramID.String()
	}
	return &pb.SubscribeUpdate{Filters: filters, UpdateOneof: &pb.SubscribeUpdate_Account{Account: &pb.SubscribeUpdateAccount{Slot: slot, Account: &pb.SubscribeUpdateAccountInfo{
		Pubkey: solana.MustPublicKeyFromBase58(account.Address).Bytes(), Owner: solana.MustPublicKeyFromBase58(owner).Bytes(),
		Lamports: account.Lamports, Data: account.Data, Executable: account.Executable, WriteVersion: writeVersion,
	}}}}
}

// seedChain answers the view's start-up read at slot: accounts by address
// (any other address is null) and receipts as the vault's withdrawal receipts.
func seedChain(t *testing.T, slot int64, accounts []ConfirmedAccount, receipts ...ConfirmedAccount) *chain.Client {
	t.Helper()
	encode := func(account ConfirmedAccount) string {
		return fmt.Sprintf(`{"owner":%q,"lamports":%d,"executable":%t,"data":[%q,"base64"]}`, account.Owner, account.Lamports, account.Executable, base64.StdEncoding.EncodeToString(account.Data))
	}
	return newFakeChain(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
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
		var values []string
		switch payload.Method {
		case "getMultipleAccounts":
			var addresses []string
			if err := json.Unmarshal(payload.Params[0], &addresses); err != nil {
				return nil, err
			}
			for _, address := range addresses {
				if account := accountAt(accounts, address); account.Address != "" {
					values = append(values, encode(account))
				} else {
					values = append(values, "null")
				}
			}
		case "getProgramAccounts":
			for _, receipt := range receipts {
				values = append(values, fmt.Sprintf(`{"pubkey":%q,"account":%s}`, receipt.Address, encode(receipt)))
			}
		default:
			return nil, fmt.Errorf("unexpected RPC method %s", payload.Method)
		}
		return response(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"result":{"context":{"slot":%d},"value":[%s]}}`, slot, strings.Join(values, ","))), nil
	}))
}

// viewRouteBatch is the production route batch at slot 77 with a neutral
// image for every other account the route observer reads.
func viewRouteBatch(t *testing.T, mutate func([]ConfirmedAccount)) []ConfirmedAccount {
	t.Helper()
	accounts := productionRouteBatchAccounts(t, 77, mutate)
	for _, address := range routeFixedAddresses(readyWorkerManifest(t)) {
		if accountAt(accounts, address).Address == "" && address != bridgeStrategyReceipt {
			accounts = append(accounts, ConfirmedAccount{Address: address, Owner: squads.ProgramID.String(), Lamports: 1, Data: bytes.Repeat([]byte{7}, 96)})
		}
	}
	return accounts
}

// streamView opens a streaming view over viewRouteBatch whose wall clock is
// the fixture Clock's time, so the view is live once it passes slot 77.
func streamView(t *testing.T) (*View, *chain.Client, *fakeLaserStream) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	client, connector := seedChain(t, 77, viewRouteBatch(t, nil)), &fakeLaserStream{}
	view, err := OpenView(ctx, client, connector)
	if err != nil {
		t.Fatal(err)
	}
	view.now = func() time.Time { return time.Unix(kaminoFixtureUnix, 0) }
	return view, client, connector
}

// fixtureView is a view without a stream: the start-up read of a fake chain.
func fixtureView(t *testing.T, rpc *chain.Client) *View {
	t.Helper()
	view, err := OpenView(context.Background(), rpc, nil)
	if err != nil {
		t.Fatal(err)
	}
	return view
}

// accountView is a view without a stream holding accounts at slot.
func accountView(t *testing.T, slot int64, accounts []ConfirmedAccount) *View {
	t.Helper()
	return fixtureView(t, seedChain(t, slot, accounts))
}

// fillAccounts answers account reads through base, taking each account base
// does not hold from accounts.
func fillAccounts(base http.RoundTripper, accounts map[string]ConfirmedAccount) http.RoundTripper {
	return roundTripFunc(func(request *http.Request) (*http.Response, error) {
		raw, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		request.Body = io.NopCloser(bytes.NewReader(raw))
		var call struct {
			ID     any               `json:"id"`
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		res, err := base.RoundTrip(request)
		if err != nil || json.Unmarshal(raw, &call) != nil || call.Method != "getMultipleAccounts" {
			return res, err
		}
		defer res.Body.Close()
		var payload struct {
			Result struct {
				Context any   `json:"context"`
				Value   []any `json:"value"`
			} `json:"result"`
		}
		var addresses []string
		if err := json.NewDecoder(res.Body).Decode(&payload); err != nil || json.Unmarshal(call.Params[0], &addresses) != nil {
			return nil, fmt.Errorf("account read: %v", err)
		}
		for i, address := range addresses {
			if a, ok := accounts[address]; ok && payload.Result.Value[i] == nil {
				payload.Result.Value[i] = map[string]any{"owner": a.Owner, "lamports": a.Lamports, "executable": a.Executable, "data": []string{base64.StdEncoding.EncodeToString(a.Data), "base64"}}
			}
		}
		encoded, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": call.ID, "result": payload.Result})
		if err != nil {
			return nil, err
		}
		return response(string(encoded)), nil
	})
}

// fence is a persisted operation decided at slot 0.
var fence = PersistedOperation{ExpectedEffects: []byte(`{"decision":{"observationSlot":0}}`)}

func usdcAmount(account ConfirmedAccount, raw uint64) ConfirmedAccount {
	account.Data = slices.Clone(account.Data)
	binary.LittleEndian.PutUint64(account.Data[64:72], raw)
	return account
}

// S is the previous confirmed slot status: a slot's account updates are only
// known complete once the next confirmed slot arrives. Within a slot,
// write_version orders one session's writes; across sessions the later
// arrival wins.
func TestViewIsCompleteThroughThePreviousConfirmedSlot(t *testing.T) {
	t.Parallel()
	view := &View{accounts: map[string]viewAccount{}, receipts: map[string]uint64{}, advanced: make(chan struct{})}
	squadsUSDC := ConfirmedAccount{Address: bridgeSquadsATA, Owner: bridgeTokenProgram, Lamports: 1, Data: make([]byte, 165)}
	processed := slotFrame(13)
	processed.GetSlot().Status = pb.SlotStatus_SLOT_PROCESSED
	for _, step := range []struct {
		frame   *pb.SubscribeUpdate
		through uint64
	}{
		{slotFrame(10), 0},
		{slotFrame(11), 10},
		{accountFrame(squadsUSDC, 12, 1, viewAccountsFilter), 10},
		{processed, 10},
		{slotFrame(12), 11},
		{slotFrame(9), 11}, // a replayed status never moves S back
		{slotFrame(14), 12},
	} {
		if err := view.Handle(context.Background(), step.frame); err != nil {
			t.Fatal(err)
		}
		if view.through != step.through {
			t.Fatalf("after %v the view is complete through %d, want %d", step.frame, view.through, step.through)
		}
	}
	replay, cancel := context.WithCancel(context.Background())
	defer cancel()
	for _, write := range []struct {
		ctx      context.Context
		lamports uint64
		version  uint64
	}{{context.Background(), 1, 9}, {context.Background(), 2, 8}, {replay, 3, 1}} {
		squadsUSDC.Lamports = write.lamports
		if err := view.Handle(write.ctx, accountFrame(squadsUSDC, 15, write.version, viewAccountsFilter)); err != nil {
			t.Fatal(err)
		}
	}
	for _, slot := range []uint64{16, 17} {
		if err := view.Handle(replay, slotFrame(slot)); err != nil {
			t.Fatal(err)
		}
	}
	if held := view.accounts[bridgeSquadsATA]; held.slot != 15 || held.Lamports != 3 {
		t.Fatalf("slot 15 resolved to %+v, want the later session's write", held)
	}
}

// A transaction's writes at S+1 are invisible together until S+1 is complete,
// then visible together.
func TestViewShowsATransactionOnlyWhenItsSlotIsComplete(t *testing.T) {
	t.Parallel()
	view, _, connector := streamView(t)
	batch := viewRouteBatch(t, nil)
	connector.send(t, view, true, slotFrame(80), slotFrame(81),
		accountFrame(usdcAmount(accountAt(batch, bridgeIdleATA), 404), 82, 1, viewAccountsFilter),
		accountFrame(usdcAmount(accountAt(batch, bridgeSquadsATA), 606), 82, 2, viewAccountsFilter),
		slotFrame(82))
	before, err := ObserveConfirmedBridgeSnapshot(context.Background(), view, fence)
	if err != nil || before.Snapshot.Slot != 81 || before.Snapshot.VoltrIdleRaw == 404 || before.Snapshot.SquadsIdleRaw == 606 {
		t.Fatalf("slot 82 leaked into the view at %d: idle %d squads %d %v", before.Snapshot.Slot, before.Snapshot.VoltrIdleRaw, before.Snapshot.SquadsIdleRaw, err)
	}
	connector.send(t, view, true, slotFrame(83))
	after, err := ObserveConfirmedBridgeSnapshot(context.Background(), view, fence)
	if err != nil || after.Snapshot.Slot != 82 || after.Snapshot.VoltrIdleRaw != 404 || after.Snapshot.SquadsIdleRaw != 606 {
		t.Fatalf("slot 82 is not whole at %d: idle %d squads %d %v", after.Snapshot.Slot, after.Snapshot.VoltrIdleRaw, after.Snapshot.SquadsIdleRaw, err)
	}
}

// The stream feeds planning at S: nothing before the start-up read is passed,
// stale writes are ignored, the route's own landing at R holds planning until
// S >= R, a view whose chain time is old stops the tick, and a failed stream
// reconnects 32 slots behind S.
func TestViewStreamPlansAtS(t *testing.T) {
	view, client, connector := streamView(t)
	request, _ := connector.last()
	if request.GetFromSlot() != 78 || !slices.Contains(request.Accounts[viewAccountsFilter].Account, bridgeSquadsATA) ||
		request.Accounts[viewReceiptsFilter].Owner[0] != voltr.ProgramID.String() || request.GetCommitment() != pb.CommitmentLevel_CONFIRMED {
		t.Fatalf("subscription does not start after the start-up read: %v", request)
	}
	manifest := readyWorkerManifest(t)
	if _, _, err := ObserveConfirmedRouteSnapshot(context.Background(), client, view, manifest, 0); !errors.Is(err, errConfirmedObservationUnavailable) {
		t.Fatalf("a view without confirmed slots planned: %v", err)
	}
	squadsUSDC := accountAt(viewRouteBatch(t, nil), bridgeSquadsATA)
	connector.send(t, view, true,
		accountFrame(usdcAmount(squadsUSDC, 900), 80, 5, viewAccountsFilter),
		accountFrame(usdcAmount(squadsUSDC, 111), 79, 9, viewAccountsFilter),
		accountFrame(usdcAmount(squadsUSDC, 222), 80, 4, viewAccountsFilter),
		slotFrame(80), slotFrame(81))
	observation, _, err := ObserveConfirmedRouteSnapshot(context.Background(), client, view, manifest, 0)
	if err != nil || observation.Snapshot.Slot != 80 || observation.Snapshot.SquadsIdleRaw != 900 {
		t.Fatalf("observed slot %d squads USDC %d, want 80 and 900: %v", observation.Snapshot.Slot, observation.Snapshot.SquadsIdleRaw, err)
	}

	short, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, _, _, err := view.read(short, nil, 85); !errors.Is(err, errConfirmedObservationUnavailable) {
		t.Fatalf("planned from slot 80 after a landing at 85: %v", err)
	}
	connector.send(t, view, true, slotFrame(200), slotFrame(201))
	if slot, _, _, err := view.read(context.Background(), nil, 85); err != nil || slot != 200 {
		t.Fatalf("view through %d: %v", slot, err)
	}

	view.now = func() time.Time { return time.Unix(kaminoFixtureUnix+60, 0) }
	state := productionObserveState{routeKey: productionRouteKey, manifest: manifest, journal: &stubProductionJournal{journal: reconciledJournal()}, identity: pinnedIdentityObservation,
		batch: func(ctx context.Context) (Observation, error) {
			observation, _, err := ObserveConfirmedRouteSnapshot(ctx, client, view, manifest, 0)
			return observation, err
		}}
	worker := &Worker{routeKey: productionRouteKey, manifest: manifest, runtime: tickRuntime{
		loadNonterminal: func(context.Context, string) (*PersistedOperation, error) { return nil, nil },
		observe:         state.observe,
		recordDecision: func(context.Context, string, Observation, Decision, string) (DecisionRecord, error) {
			t.Fatal("a view a minute behind reached a decision")
			return DecisionRecord{}, nil
		},
	}}
	if err := worker.Tick(context.Background()); !errors.Is(err, errConfirmedObservationUnavailable) {
		t.Fatalf("tick on a view a minute behind: %v", err)
	}

	_, subscription := connector.last()
	subscription.fail <- errors.New("connection reset")
	waitFor(t, func() bool {
		_, _, _, _ = view.read(context.Background(), nil, 0)
		return connector.opened() == 2
	})
	if request, _ := connector.last(); request.GetFromSlot() != 200-32 {
		t.Fatalf("reconnected from %d, want %d", request.GetFromSlot(), 200-32)
	}
}

// A receipt found by the discovery filter is handed off into the by-address
// filter; until promotion the view is incomplete. After promotion its close
// arrives through the by-address filter and the demand is gone.
func TestViewReceiptDiscoveredThenClosed(t *testing.T) {
	t.Parallel()
	view, _, connector := streamView(t)
	address, data := receiptFixture(t, bridgeVoltrVault, testPublicKey(44), 7, 5<<48)
	receipt := ConfirmedAccount{Address: address, Owner: voltr.ProgramID.String(), Lamports: 1, Data: data}
	connector.send(t, view, true, slotFrame(80), slotFrame(81), accountFrame(receipt, 82, 1, viewReceiptsFilter), slotFrame(82), slotFrame(83))
	done := make(chan Observation, 1)
	go func() {
		observation, err := ObserveConfirmedBridgeSnapshot(context.Background(), view, fence)
		if err != nil {
			t.Error(err)
		}
		done <- observation
	}()
	waitFor(t, func() bool { return connector.opened() == 2 })
	if _, err := ObserveConfirmedBridgeSnapshot(context.Background(), view, fence); !errors.Is(err, errConfirmedObservationUnavailable) {
		t.Fatalf("the view planned before the receipt handoff promoted: %v", err)
	}
	if request, _ := connector.last(); !slices.Contains(request.Accounts[viewAccountsFilter].Account, address) || request.GetFromSlot() > 82 {
		t.Fatalf("handoff does not watch the receipt from its discovery slot: from %d", request.GetFromSlot())
	}
	connector.send(t, view, false, slotFrame(84))
	if observation := <-done; observation.Snapshot.WithdrawalDemandRaw != 5 {
		t.Fatalf("discovered receipt demand: %+v", observation.Snapshot)
	}
	connector.send(t, view, true, accountFrame(ConfirmedAccount{Address: address}, 90, 1, viewAccountsFilter), slotFrame(90), slotFrame(91))
	observation, err := ObserveConfirmedBridgeSnapshot(context.Background(), view, fence)
	if err != nil || observation.Snapshot.WithdrawalDemandRaw != 0 || observation.Snapshot.Slot != 90 {
		t.Fatalf("closed receipt still owed: %+v %v", observation.Snapshot, err)
	}
}
