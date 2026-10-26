package backyard

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/solana-foundation/solana-go/v2"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func response(body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}
}

// fakeNode is a JSON-RPC endpoint that answers through Transport, which a test
// may replace or wrap between calls. A Transport error is an unavailable endpoint.
type fakeNode struct {
	Transport http.RoundTripper
}

var fakeNodes sync.Map

// newFakeChain is a chain client of a fake node answering through transport.
func newFakeChain(t testing.TB, transport http.RoundTripper) *chain.Client {
	t.Helper()
	node := &fakeNode{Transport: transport}
	server := httptest.NewServer(node)
	t.Cleanup(server.Close)
	client, err := chain.New(server.URL, 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	fakeNodes.Store(client, node)
	return client
}

// rpcOf is the fake node behind a client newFakeChain made.
func rpcOf(client *chain.Client) *fakeNode {
	node, _ := fakeNodes.Load(client)
	return node.(*fakeNode)
}

func (n *fakeNode) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var answer *http.Response
	err := errors.New("no transport")
	if n.Transport != nil {
		answer, err = n.Transport.RoundTrip(r)
	}
	if err != nil {
		// The client sees an unavailable endpoint, as for a dropped connection.
		http.Error(w, "transport failure", http.StatusBadGateway)
		return
	}
	defer answer.Body.Close()
	for key, values := range answer.Header {
		w.Header()[key] = values
	}
	w.WriteHeader(answer.StatusCode)
	_, _ = io.Copy(w, answer.Body)
}

// finalizedEpoch is getEpochInfo's answer at a finalized block height.
func finalizedEpoch(height int64) map[string]any {
	return map[string]any{"absoluteSlot": 1000 + height, "blockHeight": height, "epoch": 1, "slotIndex": 1, "slotsInEpoch": 432000, "transactionCount": 1}
}

// testSignature is a well-formed transaction signature.
var testSignature = solana.Signature{5}.String()

// A node that could not answer, a node behind the asked slot and a receipt the
// cluster does not have yet end the tick as a wait. A required account the
// cluster says is absent is a failure.
func TestChainAnswersMapToWaitFailureOrHold(t *testing.T) {
	t.Parallel()
	answer := func(status int, body string) *chain.Client {
		return newFakeChain(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
			answer := response(body)
			answer.StatusCode = status
			return answer, nil
		}))
	}
	nodeError := func(code int) string {
		return fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"error":{"code":%d,"message":"node"}}`, code)
	}
	ctx := context.Background()
	waits := map[string]error{}
	_, _, waits["behind"] = confirmedAccounts(ctx, answer(http.StatusOK, nodeError(-32016)), []string{bridgeVault}, 42)
	_, waits["unhealthy"] = signatureStatus(ctx, answer(http.StatusOK, nodeError(-32005)), testSignature)
	_, waits["rate limited"] = signatureStatus(ctx, answer(http.StatusTooManyRequests, "Too Many Requests"), testSignature)
	_, waits["not landed"] = finalizedTransaction(ctx, answer(http.StatusOK, `{"jsonrpc":"2.0","id":1,"result":null}`), testSignature)
	for name, err := range waits {
		if !errors.Is(err, errConfirmedObservationUnavailable) {
			t.Fatalf("%s: want a wait, got %v", name, err)
		}
	}
	absent := answer(http.StatusOK, `{"jsonrpc":"2.0","id":1,"result":{"context":{"slot":43},"value":[null,null]}}`)
	if _, _, err := confirmedAccounts(ctx, absent, []string{bridgeVault, bridgeStrategy}, 42); err == nil || errors.Is(err, errConfirmedObservationUnavailable) {
		t.Fatalf("an absent required account was accepted or taken for a wait: %v", err)
	}
	slot, accounts, err := confirmedAccounts(ctx, absent, []string{bridgeVault, bridgeStrategy}, 42, bridgeVault, bridgeStrategy)
	if err != nil || slot != 43 || accounts[1].Address != bridgeStrategy || accounts[1].Owner != "" || accounts[1].Data != nil {
		t.Fatalf("pinned optional absence: slot=%d accounts=%+v err=%v", slot, accounts, err)
	}
}
