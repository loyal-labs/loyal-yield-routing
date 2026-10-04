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

// This is a live test-only handoff. The actual C producer owns SVM and RPC
// until D reports verified terminal state; no saved admission replaces C IO.
type connectedGoHandoff struct{ callback, token, database string }

func TestConnectedGoSameMintAdmissionProducer(t *testing.T) {
	callback := os.Getenv("KAMINO_CONNECTED_GO_D_CALLBACK")
	if callback == "" {
		t.Skip("requires the owning Go D connected test")
	}
	u, err := url.Parse(callback)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.User != nil || u.Path != "/handoff" || u.RawQuery != "" || u.Fragment != "" {
		t.Fatal("Go handoff callback must be the scoped loopback fixture")
	}
	token := os.Getenv("KAMINO_CONNECTED_GO_D_TOKEN")
	decoded, err := hex.DecodeString(token)
	if err != nil || len(decoded) != 32 {
		t.Fatal("Go handoff requires an independent scoped capability")
	}
	database := os.Getenv("FLEET_TEST_GO_SAME_MINT_DATABASE_URL")
	dbURL, err := url.Parse(database)
	if err != nil || dbURL.Scheme != "postgresql" || dbURL.Hostname() != "127.0.0.1" || dbURL.Port() != "51913" || dbURL.User == nil || dbURL.User.Username() != "workers_v2" || (dbURL.Path != "/fleet_go_same_mint" && dbURL.Path != "/fleet_go_same_mint_simplify") || dbURL.RawQuery != "" || dbURL.Fragment != "" {
		t.Fatal("Go handoff requires the registered dedicated fixture database")
	}
	if _, hasPassword := dbURL.User.Password(); hasPassword {
		t.Fatal("fixture database URL must not contain a password")
	}
	for _, name := range []string{"KAMINO_TEST_KLEND_PROXY_PATH", "KAMINO_CONNECTED_SVM_PATH", "KAMINO_CONNECTED_WORKER_PATH"} {
		if os.Getenv(name) == "" {
			t.Fatalf("configured Go handoff lacks local artifact %s", name)
		}
	}
	runConnectedLaneWithHandoff(t, true, &connectedGoHandoff{callback, token, database})
}

func postConnectedGoAdmission(t *testing.T, ctx context.Context, handoff connectedGoHandoff, admission ExecutionAdmission, rpcURL string) {
	t.Helper()
	body, err := json.Marshal(struct {
		Admission ExecutionAdmission `json:"admission"`
		RPCURL    string             `json:"rpcUrl"`
	}{admission, rpcURL})
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, handoff.callback, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-Connected-Fixture", handoff.token)
	client := &http.Client{Timeout: 100 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("live Go D handoff failed: %v", err)
	}
	defer response.Body.Close()
	var terminal struct {
		Reconciled bool `json:"reconciled"`
	}
	if response.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(response.Body, 1024)).Decode(&terminal) != nil || !terminal.Reconciled {
		t.Fatal("Go D did not return verified terminal ownership evidence")
	}
}
