package chain

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

func TestBlockTimeWithoutATimeIsNotFound(t *testing.T) {
	var value any = 1_700_000_000
	client := serve(t, func(request) (int, any) { return http.StatusOK, result(value) })
	at, err := client.BlockTime(context.Background(), 9)
	if err != nil || at.Unix() != 1_700_000_000 {
		t.Fatalf("block time %v, %v", at, err)
	}
	value = nil
	if _, err := client.BlockTime(context.Background(), 9); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a block without a time = %v, want ErrNotFound", err)
	}
}
