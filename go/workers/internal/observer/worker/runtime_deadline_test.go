package worker

import (
	"testing"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
)

func chainClient(t *testing.T, url string, timeout time.Duration) *chain.Client {
	t.Helper()
	client, err := chain.New(url, timeout)
	if err != nil {
		t.Fatal(err)
	}
	return client
}
