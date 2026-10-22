package stats

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestVaultListSurvivesHungDatabase(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var accepted atomic.Int32
	var held []net.Conn
	var heldMu sync.Mutex
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			heldMu.Lock()
			held = append(held, conn)
			heldMu.Unlock()
		}
	}()
	pool, err := pgxpool.New(context.Background(), "postgres://u@"+ln.Addr().String()+"/d?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ln.Close()
		heldMu.Lock()
		for _, conn := range held {
			conn.Close()
		}
		heldMu.Unlock()
		pool.Close()
	})
	vaults := newVaultList(pool)

	get := func(timeout time.Duration) (int, string, time.Duration) {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		req := httptest.NewRequest(http.MethodGet, "/api/earn/vaults", nil).WithContext(ctx)
		rec := httptest.NewRecorder()
		start := time.Now()
		vaults.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String(), time.Since(start)
	}

	code, body, took := get(time.Second)
	if code != http.StatusServiceUnavailable || body != `{"error":"vault list unavailable"}` || took > 3*time.Second {
		t.Errorf("empty cache, hung database: got %d %q after %s, want 503 within the client's 1s", code, body, took)
	}

	vaults.mu.Lock()
	vaults.last = vaultsPage{body: []byte(`{"vaults":["v"],"count":1,"updatedAt":"old"}`), fetched: time.Now().Add(-5*time.Minute - 30*time.Second)}
	vaults.mu.Unlock()

	var wg sync.WaitGroup
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, body, took := get(2 * time.Second)
			if code != http.StatusOK || body != `{"vaults":["v"],"count":1,"updatedAt":"old"}` || took > time.Second {
				t.Errorf("5m30s-old cache, hung database: got %d %q after %s, want the cached 200 within 1s", code, body, took)
			}
		}()
	}
	wg.Wait()

	if n := accepted.Load(); n != 1 {
		t.Fatalf("database connections opened: got %d, want 1", n)
	}
}
