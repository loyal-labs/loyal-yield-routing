package fleet

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"testing"
	"time"
)

// The bank stays alive while the actual retail adapter and Go D consume it.
// This transport describes fixture endpoints; it conveys no financial proof,
// signing capability, admission, or caller-certified receipt.
type connectedCrossMintBankHandoff struct{ callback, token, database string }

type connectedCrossMintBank struct {
	RPCURL, BuildURL, Cluster string
	TLSCA                     []byte
	OpportunityID, EpochID    int64
	VaultID                   int64
	VaultIndex                uint8
	Vault, Settings, Signer   string
	Source, Target            KaminoPositionAccounts
}

func TestConnectedGoCrossMintBankProducer(t *testing.T) {
	handoff := connectedCrossMintBankRequest(t)
	if handoff == nil {
		t.Skip("requires the owning current Go cross-mint test")
	}
	t.Setenv("FLEET_TEST_DATABASE_URL", handoff.database)
	runConnectedLane(t, false)
}

func connectedCrossMintBankRequest(t *testing.T) *connectedCrossMintBankHandoff {
	t.Helper()
	callback := os.Getenv("KAMINO_CONNECTED_GO_CROSS_MINT_CALLBACK")
	if callback == "" {
		return nil
	}
	u, err := url.Parse(callback)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.User != nil || u.Path != "/bank" || u.RawQuery != "" || u.Fragment != "" {
		t.Fatal("cross-mint callback escaped the scoped loopback fixture")
	}
	token := os.Getenv("KAMINO_CONNECTED_GO_CROSS_MINT_TOKEN")
	decoded, err := hex.DecodeString(token)
	if err != nil || len(decoded) != 32 {
		t.Fatal("cross-mint bank requires an independent scoped capability")
	}
	database := os.Getenv("FLEET_TEST_GO_CROSS_MINT_DATABASE_URL")
	u, err = url.Parse(database)
	if err != nil || u.Scheme != "postgresql" || u.Hostname() != "127.0.0.1" || u.Port() != "51913" || u.User == nil || u.User.Username() != "workers_v2" || u.Path != "/fleet_go_cross_mint" || u.RawQuery != "" || u.Fragment != "" {
		t.Fatal("cross-mint bank requires its registered dedicated database")
	}
	return &connectedCrossMintBankHandoff{callback, token, database}
}

func postConnectedCrossMintBank(t *testing.T, ctx context.Context, handoff connectedCrossMintBankHandoff, bank connectedCrossMintBank) {
	t.Helper()
	body, err := json.Marshal(bank)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, handoff.callback, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-Connected-Fixture", handoff.token)
	client := &http.Client{Timeout: 110 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("live retail cross-mint proof failed: %v", err)
	}
	defer response.Body.Close()
	var terminal struct{ Reconciled bool }
	if response.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(response.Body, 1024)).Decode(&terminal) != nil || !terminal.Reconciled {
		t.Fatal("current Go cross-mint owner did not return verified terminal evidence")
	}
}
