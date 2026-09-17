package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeRevalidationStore counts every durable call so the shadow path can be
// proven write-free without a database.
type fakeRevalidationStore struct {
	lease                                    *RevalidationLease
	peek, claim, check, refresh, commit, alt int
	lastSeenIDs, lastSeenTokens              []int64
}

func (f *fakeRevalidationStore) ClaimRevalidation(context.Context, string, string, time.Duration, bool, bool, ...string) (*RevalidationLease, error) {
	f.claim++
	return f.lease, nil
}
func (f *fakeRevalidationStore) PeekRevalidation(_ context.Context, _, _ string, _ bool, seenIDs, seenTokens []int64) (*RevalidationLease, error) {
	f.peek++
	f.lastSeenIDs, f.lastSeenTokens = seenIDs, seenTokens
	for i, id := range seenIDs {
		if f.lease != nil && id == f.lease.OpportunityID && seenTokens[i] == f.lease.FencingToken {
			return nil, nil
		}
	}
	return f.lease, nil
}
func (f *fakeRevalidationStore) CheckRevalidationLease(context.Context, RevalidationLease) error {
	f.check++
	return nil
}
func (f *fakeRevalidationStore) RefreshTargetCapacity(context.Context, string, string, string, int64, int64) error {
	f.refresh++
	return nil
}
func (f *fakeRevalidationStore) LoadReusableLookupTables(context.Context, string, int64, int64, []string) ([]LookupTable, error) {
	f.alt++
	return nil, nil
}
func (f *fakeRevalidationStore) CommitRevalidation(context.Context, RevalidationLease, RevalidationCommit) error {
	f.commit++
	return nil
}

func captureShadowEvents(t *testing.T, run func()) []map[string]any {
	t.Helper()
	var buffer bytes.Buffer
	previous, flags := log.Writer(), log.Flags()
	log.SetOutput(&buffer)
	log.SetFlags(0)
	run()
	log.SetOutput(previous)
	log.SetFlags(flags)
	var events []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buffer.String()), "\n") {
		var event map[string]any
		if json.Unmarshal([]byte(line), &event) == nil && event["event"] == "kamino_fleet_revalidation_shadow" {
			events = append(events, event)
		}
	}
	return events
}

func shadowTestRevalidator(t *testing.T, store *fakeRevalidationStore) *Revalidator {
	t.Helper()
	// Every RPC call fails fast and non-retryably, so preparation stops at load_fresh_route.
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		http.Error(writer, "no", http.StatusBadRequest)
	}))
	t.Cleanup(server.Close)
	return &Revalidator{store: store, rpc: NewRPCClient(server.URL), proxy: &KLendProxy{}, owner: "shadow", signer: testIdentity(9), leaseTTL: time.Second, computeLimit: defaultComputeLimit, slotDuration: 400 * time.Millisecond}
}

func TestShadowCyclePerformsNoWrites(t *testing.T) {
	store := &fakeRevalidationStore{lease: &RevalidationLease{OpportunityID: 42, FencingToken: 7, RouteKind: "same_mint", VaultID: 3, SourceReserve: testIdentity(1), TargetReserve: testIdentity(2), DelegatedSigners: []string{testIdentity(9)}, VaultPubkey: testIdentity(4), PolicyAccount: testIdentity(5)}}
	revalidator := shadowTestRevalidator(t, store)
	var seen shadowSeen
	var processed bool
	var err error
	events := captureShadowEvents(t, func() { processed, err = revalidator.ShadowCycle(context.Background(), "mainnet-beta", &seen) })
	if err != nil || !processed {
		t.Fatalf("shadow cycle processed=%v err=%v", processed, err)
	}
	if len(events) != 1 || events[0]["disposition"] != "error" || events[0]["stage"] != "load_fresh_route" || events[0]["mode"] != "shadow" || events[0]["opportunityId"] != float64(42) || events[0]["fencingToken"] != float64(7) {
		t.Fatalf("unexpected shadow events: %v", events)
	}
	if store.peek != 1 || store.claim != 0 || store.check != 0 || store.refresh != 0 || store.commit != 0 {
		t.Fatalf("shadow touched durable state: %+v", store)
	}
	if token, ok := seen.seen[42]; !ok || token != 7 {
		t.Fatalf("seen set missing processed row: %v", seen.seen)
	}
	processed, err = revalidator.ShadowCycle(context.Background(), "mainnet-beta", &seen)
	if err != nil || processed {
		t.Fatalf("second cycle re-processed the same row: processed=%v err=%v", processed, err)
	}
	if store.peek != 2 || len(store.lastSeenIDs) != 1 || store.lastSeenIDs[0] != 42 || store.lastSeenTokens[0] != 7 || store.claim+store.check+store.refresh+store.commit != 0 {
		t.Fatalf("second peek did not exclude the seen row: %+v", store)
	}
	// A durable retry bumps the fencing token; that revision must be shadowed again.
	store.lease.FencingToken = 8
	events = captureShadowEvents(t, func() { processed, err = revalidator.ShadowCycle(context.Background(), "mainnet-beta", &seen) })
	if err != nil || !processed || len(events) != 1 || events[0]["fencingToken"] != float64(8) {
		t.Fatalf("retried revision was not re-shadowed: processed=%v err=%v events=%v", processed, err, events)
	}
	if seen.seen[42] != 8 || store.claim+store.check+store.refresh+store.commit != 0 {
		t.Fatalf("retry shadow touched durable state or did not advance seen: %+v seen=%v", store, seen.seen)
	}
}

func TestShadowCycleSkipsCrossMintWithoutWrites(t *testing.T) {
	store := &fakeRevalidationStore{lease: &RevalidationLease{OpportunityID: 9, FencingToken: 1, RouteKind: "cross_mint_jupiter"}}
	revalidator := shadowTestRevalidator(t, store)
	var seen shadowSeen
	events := captureShadowEvents(t, func() {
		if processed, err := revalidator.ShadowCycle(context.Background(), "mainnet-beta", &seen); err != nil || !processed {
			t.Fatalf("processed=%v err=%v", processed, err)
		}
	})
	if len(events) != 1 || events[0]["disposition"] != "skipped_cross_mint" {
		t.Fatalf("unexpected events: %v", events)
	}
	if store.peek != 1 || store.claim+store.check+store.refresh+store.commit+store.alt != 0 || seen.seen[9] != 1 {
		t.Fatalf("cross-mint shadow touched durable state or missed seen: %+v seen=%v", store, seen.seen)
	}
}
