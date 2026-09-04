package stream

import (
	"context"
	"testing"

	pb "github.com/helius-labs/laserstream-sdk/go/proto"
)

func TestRouterFansOneUpdateOutToEveryMatchingFilter(t *testing.T) {
	calls := map[string]int{}
	handler := func(name string) Handler {
		return HandlerFunc(func(context.Context, *pb.SubscribeUpdate) error {
			calls[name]++
			return nil
		})
	}
	router := NewRouter(map[string]Handler{
		"balance": handler("balance"),
		"earn":    handler("earn"),
	})
	if err := router.Handle(context.Background(), &pb.SubscribeUpdate{
		Filters: []string{"earn", "balance", "earn"},
	}); err != nil {
		t.Fatalf("route update: %v", err)
	}
	if calls["balance"] != 1 || calls["earn"] != 1 {
		t.Fatalf("calls = %#v, want each matching durable handler exactly once", calls)
	}
}

func TestRouterRejectsUnownedFilter(t *testing.T) {
	router := NewRouter(nil)
	if err := router.Handle(context.Background(), &pb.SubscribeUpdate{Filters: []string{"unknown"}}); err == nil {
		t.Fatal("unowned filter was silently acknowledged")
	}
}
