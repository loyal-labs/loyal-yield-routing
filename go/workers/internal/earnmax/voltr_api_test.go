package earnmax

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/solana-foundation/solana-go/v2"
)

type vaultAPITransport func(*http.Request) (*http.Response, error)

func (f vaultAPITransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestVoltrReadsStayOnSelectedVault(t *testing.T) {
	other := solana.MustPublicKeyFromBase58("GrHZD15bMiUwrVPsmBwz8GRoQeDZN6wNEQVdW8k8SxV1")
	old := voltrClient
	t.Cleanup(func() { voltrClient = old })
	voltrClient = &http.Client{Transport: vaultAPITransport(func(r *http.Request) (*http.Response, error) {
		value := "1000"
		if strings.Contains(r.URL.Path, other.String()) {
			value = "2000"
		}
		body := `{"data":{"sharePrice":` + value + `},"vault":{"dailyStats":{"dateLabels":["2026-11-06"],"tvlData":[` + value + `],"lpData":[1]}}}`
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
	})}
	for _, tc := range []struct {
		key  solana.PublicKey
		want float64
	}{{Vault, 1000}, {other, 2000}, {Vault, 1000}} {
		price, err := FetchVaultSharePrice(context.Background(), tc.key)
		if err != nil || price.PerLP != tc.want {
			t.Fatalf("share price for %s: %+v %v", tc.key, price, err)
		}
		days, err := FetchVaultDailyStats(context.Background(), tc.key)
		if err != nil || len(days) != 1 || days[0].TVLRaw != tc.want {
			t.Fatalf("history for %s: %+v %v", tc.key, days, err)
		}
	}
	price, err := FetchSharePrice(context.Background())
	if err != nil || price.PerLP != 1000 {
		t.Fatalf("public Earn MAX default changed: %+v %v", price, err)
	}
}
