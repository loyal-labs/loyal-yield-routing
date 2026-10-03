package stream

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/helius-labs/laserstream-sdk/go/proto"
	_ "github.com/jackc/pgx/v5/stdlib"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
)

const testBufferSize = 4 * 1024 * 1024

func TestParallelSubscriptionHandoffE2E(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	listener := bufconn.Listen(testBufferSize)
	fake := newFakeLaserStream()
	server := grpc.NewServer()
	pb.RegisterGeyserServer(server, fake)
	go func() {
		_ = server.Serve(listener)
	}()
	defer server.Stop()

	connector := GRPCConnector{
		Endpoint: "passthrough:///laserstream-e2e",
		DialOptions: []grpc.DialOption{
			grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
				return listener.Dial()
			}),
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		},
	}
	sink := newIdempotentSink()
	var durableHandler Handler = sink
	var postgresSink *postgresDedupeSink
	if databaseURL := os.Getenv("TEST_DATABASE_URL"); databaseURL != "" {
		postgresSink = newPostgresDedupeSink(t, ctx, databaseURL)
		durableHandler = HandlerFunc(func(ctx context.Context, update *pb.SubscribeUpdate) error {
			if err := postgresSink.Handle(ctx, update); err != nil {
				return err
			}
			return sink.Handle(ctx, update)
		})
	}
	manager := NewManager(connector, durableHandler, Config{
		ReplayOverlapSlots: 5,
		HandoffTimeout:     5 * time.Second,
	})
	defer manager.Close()

	initial := combinedRequest(100, []string{"balance_sweep_wallet_atas"})
	if err := manager.Start(ctx, initial); err != nil {
		t.Fatalf("start combined subscription: %v", err)
	}
	waitFor(t, ctx, func() bool { return manager.ActiveFrontier() >= 110 }, "initial stream frontier")

	// A newly discovered Earn binding asks for a deeper replay than the normal
	// five-slot handoff overlap. The candidate must honor the smaller start.
	replacement := combinedRequest(103, []string{
		"balance_sweep_wallet_atas",
		"kamino_reserves",
		"earn_vault_accounts",
	})
	if err := manager.Handoff(ctx, replacement); err != nil {
		t.Fatalf("parallel handoff: %v", err)
	}

	waitFor(t, ctx, func() bool {
		return fake.activeSubscriptions() == 1 &&
			sink.maxSlot("balance_sweep_wallet_atas") >= 115 &&
			sink.maxSlot("earn_vault_accounts") >= 115 &&
			sink.maxSlot("kamino_reserves") >= 115
	}, "candidate promotion and old stream cancellation")

	requests := fake.requestsSnapshot()
	if len(requests) != 2 {
		t.Fatalf("requests = %d, want exactly initial plus handoff candidate", len(requests))
	}
	if got := requests[1].GetFromSlot(); got != 103 {
		t.Fatalf("candidate from_slot = %d, want deeper new-binding replay slot 103", got)
	}
	if fake.maxConcurrentSubscriptions() != 2 {
		t.Fatalf("max concurrent subscriptions = %d, want 2", fake.maxConcurrentSubscriptions())
	}
	if sink.maxConcurrentHandlers() != 1 {
		t.Fatalf("max concurrent durable handlers = %d, want 1", sink.maxConcurrentHandlers())
	}
	if !requestHasAllChannels(requests[1]) {
		t.Fatalf("candidate was not one combined accounts+transactions+slots request")
	}

	for _, filter := range []string{"kamino_reserves", "earn_vault_accounts"} {
		for slot := uint64(103); slot <= 115; slot++ {
			if !sink.has(filter, slot) {
				t.Fatalf("missing replayed %s event at slot %d", filter, slot)
			}
		}
	}
	if duplicates := sink.duplicateCount(); duplicates == 0 {
		t.Fatal("expected overlap duplicates to exercise idempotency")
	}
	if sink.uniqueCount("balance_sweep_wallet_atas", 103, 115) != 13 {
		t.Fatalf("overlap replay produced a gap or duplicate durable balance row")
	}
	if postgresSink != nil {
		for _, filter := range []string{"balance_sweep_wallet_atas", "kamino_reserves", "earn_vault_accounts"} {
			if count := postgresSink.count(t, ctx, filter, 103, 115); count != 13 {
				t.Fatalf("PostgreSQL rows for %s = %d, want 13 exact-once durable rows", filter, count)
			}
		}
		if postgresSink.duplicates.Load() == 0 {
			t.Fatal("PostgreSQL sink did not exercise ON CONFLICT replay deduplication")
		}
	}
}

func TestFailedCandidateRollsBackToOldStreamE2E(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	listener := bufconn.Listen(testBufferSize)
	fake := newFakeLaserStream()
	server := grpc.NewServer()
	pb.RegisterGeyserServer(server, fake)
	go func() { _ = server.Serve(listener) }()
	defer server.Stop()

	manager := NewManager(GRPCConnector{
		Endpoint: "passthrough:///laserstream-rollback-e2e",
		DialOptions: []grpc.DialOption{
			grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }),
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		},
	}, newIdempotentSink(), Config{ReplayOverlapSlots: 5, HandoffTimeout: 5 * time.Second})
	defer manager.Close()

	if err := manager.Start(ctx, combinedRequest(100, []string{"balance_sweep_wallet_atas"})); err != nil {
		t.Fatalf("start initial stream: %v", err)
	}
	waitFor(t, ctx, func() bool { return manager.ActiveFrontier() >= 110 }, "initial rollback frontier")
	before := manager.ActiveFrontier()
	err := manager.Handoff(ctx, combinedRequest(105, []string{
		"balance_sweep_wallet_atas", "fail_candidate",
	}))
	if err == nil {
		t.Fatal("candidate failure unexpectedly promoted")
	}
	waitFor(t, ctx, func() bool {
		return fake.activeSubscriptions() == 1 && manager.ActiveFrontier() > before
	}, "old stream resume after candidate rollback")
}

func combinedRequest(fromSlot uint64, accountFilters []string) *pb.SubscribeRequest {
	accounts := make(map[string]*pb.SubscribeRequestFilterAccounts, len(accountFilters))
	for _, filter := range accountFilters {
		accounts[filter] = &pb.SubscribeRequestFilterAccounts{
			Account:              []string{filter + "-pubkey"},
			NonemptyTxnSignature: boolPointer(true),
		}
	}
	confirmed := pb.CommitmentLevel_CONFIRMED
	filterByCommitment := true
	vote := false
	failed := false
	return &pb.SubscribeRequest{
		Accounts: accounts,
		Transactions: map[string]*pb.SubscribeRequestFilterTransactions{
			"earn_max_policy_transactions": {
				Vote:           &vote,
				Failed:         &failed,
				AccountInclude: []string{"squads-program"},
			},
		},
		Slots: map[string]*pb.SubscribeRequestFilterSlots{
			"stream_progress": {FilterByCommitment: &filterByCommitment},
		},
		Commitment: &confirmed,
		FromSlot:   uint64Pointer(fromSlot),
	}
}

func requestHasAllChannels(request *pb.SubscribeRequest) bool {
	return len(request.Accounts) == 3 &&
		len(request.Transactions) == 1 &&
		len(request.Slots) == 1 &&
		request.GetCommitment() == pb.CommitmentLevel_CONFIRMED
}

type fakeLaserStream struct {
	pb.UnimplementedGeyserServer

	mu            sync.Mutex
	requests      []*pb.SubscribeRequest
	active        int
	maxConcurrent int
}

func newFakeLaserStream() *fakeLaserStream { return &fakeLaserStream{} }

func (s *fakeLaserStream) Subscribe(stream grpc.BidiStreamingServer[pb.SubscribeRequest, pb.SubscribeUpdate]) error {
	request, err := stream.Recv()
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.requests = append(s.requests, proto.Clone(request).(*pb.SubscribeRequest))
	s.active++
	if s.active > s.maxConcurrent {
		s.maxConcurrent = s.active
	}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.active--
		s.mu.Unlock()
	}()

	fromSlot := request.GetFromSlot()
	if fromSlot == 0 {
		fromSlot = 1
	}
	_, failCandidate := request.Accounts["fail_candidate"]
	for slot := fromSlot; ; slot++ {
		if failCandidate && slot >= fromSlot+2 {
			return errors.New("injected candidate stream failure")
		}
		select {
		case <-stream.Context().Done():
			return stream.Context().Err()
		case <-time.After(time.Millisecond):
		}

		filters := make([]string, 0, len(request.Accounts))
		for filter := range request.Accounts {
			filters = append(filters, filter)
		}
		sort.Strings(filters)
		for _, filter := range filters {
			if err := stream.Send(&pb.SubscribeUpdate{
				Filters: []string{filter},
				UpdateOneof: &pb.SubscribeUpdate_Account{
					Account: &pb.SubscribeUpdateAccount{Slot: slot},
				},
			}); err != nil {
				return err
			}
		}
		if err := stream.Send(&pb.SubscribeUpdate{
			Filters: []string{"earn_max_policy_transactions"},
			UpdateOneof: &pb.SubscribeUpdate_Transaction{
				Transaction: &pb.SubscribeUpdateTransaction{Slot: slot},
			},
		}); err != nil {
			return err
		}
		if err := stream.Send(&pb.SubscribeUpdate{
			Filters: []string{"stream_progress"},
			UpdateOneof: &pb.SubscribeUpdate_Slot{
				Slot: &pb.SubscribeUpdateSlot{Slot: slot},
			},
		}); err != nil {
			return err
		}
	}
}

func (s *fakeLaserStream) requestsSnapshot() []*pb.SubscribeRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]*pb.SubscribeRequest, len(s.requests))
	for index, request := range s.requests {
		result[index] = proto.Clone(request).(*pb.SubscribeRequest)
	}
	return result
}

func (s *fakeLaserStream) activeSubscriptions() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active
}

func (s *fakeLaserStream) maxConcurrentSubscriptions() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.maxConcurrent
}

type postgresDedupeSink struct {
	db         *sql.DB
	duplicates atomic.Int64
}

func newPostgresDedupeSink(t *testing.T, ctx context.Context, databaseURL string) *postgresDedupeSink {
	t.Helper()
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		t.Fatalf("open isolated PostgreSQL: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("ping isolated PostgreSQL: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		DROP TABLE IF EXISTS laserstream_handoff_events;
		CREATE TABLE laserstream_handoff_events (
			filter_name text NOT NULL,
			slot bigint NOT NULL,
			PRIMARY KEY (filter_name, slot)
		)
	`); err != nil {
		t.Fatalf("initialize isolated PostgreSQL dedupe table: %v", err)
	}
	return &postgresDedupeSink{db: db}
}

func (s *postgresDedupeSink) Handle(ctx context.Context, update *pb.SubscribeUpdate) error {
	slot, ok := updateSlot(update)
	if !ok {
		return nil
	}
	for _, filter := range update.Filters {
		result, err := s.db.ExecContext(ctx, `
			INSERT INTO laserstream_handoff_events (filter_name, slot)
			VALUES ($1, $2)
			ON CONFLICT (filter_name, slot) DO NOTHING
		`, filter, slot)
		if err != nil {
			return fmt.Errorf("insert durable handoff event: %w", err)
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("read durable handoff insert result: %w", err)
		}
		if rows == 0 {
			s.duplicates.Add(1)
		}
	}
	return nil
}

func (s *postgresDedupeSink) count(t *testing.T, ctx context.Context, filter string, from, to uint64) int {
	t.Helper()
	var count int
	if err := s.db.QueryRowContext(ctx, `
		SELECT count(*)
		FROM laserstream_handoff_events
		WHERE filter_name = $1 AND slot BETWEEN $2 AND $3
	`, filter, from, to).Scan(&count); err != nil {
		t.Fatalf("count isolated PostgreSQL rows: %v", err)
	}
	return count
}

type idempotentSink struct {
	mu             sync.Mutex
	seen           map[string]struct{}
	duplicates     int
	max            map[string]uint64
	inflight       int
	maxConcurrency int
}

func newIdempotentSink() *idempotentSink {
	return &idempotentSink{seen: make(map[string]struct{}), max: make(map[string]uint64)}
}

func (s *idempotentSink) Handle(_ context.Context, update *pb.SubscribeUpdate) error {
	s.mu.Lock()
	s.inflight++
	if s.inflight > s.maxConcurrency {
		s.maxConcurrency = s.inflight
	}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.inflight--
		s.mu.Unlock()
	}()
	// Widen the race window so the E2E would reliably catch old/candidate
	// delivery running concurrently.
	time.Sleep(50 * time.Microsecond)

	slot, ok := updateSlot(update)
	if !ok {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, filter := range update.Filters {
		key := filter + ":" + formatSlot(slot)
		if _, exists := s.seen[key]; exists {
			s.duplicates++
			continue
		}
		s.seen[key] = struct{}{}
		if slot > s.max[filter] {
			s.max[filter] = slot
		}
	}
	return nil
}

func (s *idempotentSink) has(filter string, slot uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.seen[filter+":"+formatSlot(slot)]
	return ok
}

func (s *idempotentSink) maxSlot(filter string) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.max[filter]
}

func (s *idempotentSink) duplicateCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.duplicates
}

func (s *idempotentSink) maxConcurrentHandlers() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.maxConcurrency
}

func (s *idempotentSink) uniqueCount(filter string, from, to uint64) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	count := 0
	for slot := from; slot <= to; slot++ {
		if _, ok := s.seen[filter+":"+formatSlot(slot)]; ok {
			count++
		}
	}
	return count
}

func formatSlot(slot uint64) string {
	if slot == 0 {
		return "0"
	}
	var buffer [20]byte
	index := len(buffer)
	for slot > 0 {
		index--
		buffer[index] = byte('0' + slot%10)
		slot /= 10
	}
	return string(buffer[index:])
}

func waitFor(t *testing.T, ctx context.Context, predicate func() bool, label string) {
	t.Helper()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		if predicate() {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for %s: %v", label, ctx.Err())
		case <-ticker.C:
		}
	}
}

func boolPointer(value bool) *bool { return &value }
