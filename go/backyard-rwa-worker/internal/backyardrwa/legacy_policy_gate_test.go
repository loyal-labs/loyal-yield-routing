package backyardrwa

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// The gate derives seed 62-65 policy PDAs from the bridge Settings constant at
// startup. This pins that derivation to the addresses the lifecycle evidence
// recorded when the four policies were actually created on mainnet.
func TestLegacyPolicyGateDerivesTheRecordedSeed6265Addresses(t *testing.T) {
	derived, err := legacyCustomPolicyAddresses()
	if err != nil {
		t.Fatalf("derive legacy policy addresses: %v", err)
	}
	if len(derived) != len(legacyCustomPolicySeeds) {
		t.Fatalf("derived %d addresses for %d seeds", len(derived), len(legacyCustomPolicySeeds))
	}
	seen := map[string]bool{}
	for index, address := range derived {
		if len(address) < 32 || len(address) > 44 {
			t.Fatalf("seed %d derived %q, not a base58 pubkey", legacyCustomPolicySeeds[index], address)
		}
		if seen[address] {
			t.Fatalf("seed %d collided with an earlier derivation: %s", legacyCustomPolicySeeds[index], address)
		}
		seen[address] = true
	}
	expected := []string{
		"HoDV7mtsb2u1VARZLYuGByW7cCsGWL9NFxHZs7WHjdzz",
		"41nzu42c3KPgJfWhnV5jbfxjHbvVU6HXaiJmzzYNqvBP",
		"ALz5Wkt82GhGFH1LfzbnAovkZ6t85ErovbxHUH3yY1wY",
		"DjYYkQWb4zYbySfEndjVdg2NwZ8i77Fb9P1UFVbebc5t",
	}
	for index, address := range expected {
		if derived[index] != address {
			t.Fatalf("seed %d derived %s, evidence records %s", legacyCustomPolicySeeds[index], derived[index], address)
		}
	}
	evidence, err := os.ReadFile("../../../../docs/evidence/backyard-rwa-go/policy-helius-bridge-lifecycle-v2.json")
	if err != nil {
		t.Fatalf("read lifecycle evidence: %v", err)
	}
	for _, address := range expected {
		if !strings.Contains(string(evidence), address) {
			t.Fatalf("evidence file does not record legacy policy %s", address)
		}
	}
}

func TestIsOnCurveRejectsCurvePointsAndAcceptsHashes(t *testing.T) {
	// RFC 8032 test-vector public keys (TEST 1, TEST 2): real curve points that
	// must decompress, so a base/sign parsing error in the decompression math
	// fails here instead of silently skipping the bump Solana chose for a PDA.
	onCurveVectors := []string{
		"d75a980182b10ab7d54bfed3c964073a0ee172f3daa62325af021a68f707511a",
		"3d4017c3e843895a92b70aa74d1b7ebc9c982ccf2ec4968cc0cd55f12af4660c",
	}
	for _, vector := range onCurveVectors {
		raw, err := hex.DecodeString(vector)
		if err != nil {
			t.Fatalf("decode test vector: %v", err)
		}
		if !isOnCurve(raw) {
			t.Fatalf("ed25519 test vector %s classified off-curve", vector)
		}
	}
	identityOnCurve := make([]byte, 32)
	identityOnCurve[0] = 1 // y = 1 is the identity point of ed25519
	if !isOnCurve(identityOnCurve) {
		t.Fatalf("ed25519 identity classified off-curve")
	}
	offCurve := make([]byte, 32)
	for attempt := 0; attempt < 256; attempt++ {
		offCurve[0] = byte(attempt)
		if !isOnCurve(offCurve) {
			return
		}
	}
	t.Fatalf("no small y landed off the curve; on-curve check is broken")
}

type stubLegacyRPC struct {
	surviving map[string]bool
	genesis   string
	anchor    bool
}

func (s stubLegacyRPC) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	var payload struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
		http.Error(writer, err.Error(), http.StatusBadRequest)
		return
	}
	writer.Header().Set("content-type", "application/json")
	switch payload.Method {
	case "getGenesisHash":
		genesis := s.genesis
		if genesis == "" {
			genesis = mainnetGenesisHash
		}
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"jsonrpc": "2.0", "id": 1, "result": genesis,
		})
	case "getSlot":
		_, _ = writer.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":1000}`))
	case "getMultipleAccounts":
		var params []json.RawMessage
		_ = json.Unmarshal(payload.Params, &params)
		var addresses []string
		_ = json.Unmarshal(params[0], &addresses)
		values := make([]any, len(addresses))
		for index, address := range addresses {
			if index == 0 && address == bridgeSettings && s.anchor {
				data := append(append([]byte{}, squadsSettingsDiscriminator[:]...), 1)
				values[index] = map[string]any{
					"owner": bridgeSquadsProgram, "lamports": 2_000_000,
					"data": []string{base64.StdEncoding.EncodeToString(data), "base64"}, "executable": false,
				}
			} else if s.surviving[address] {
				values[index] = map[string]any{
					"owner": bridgeSquadsProgram, "lamports": 2_000_000,
					"data": []string{"AAAAAA==", "base64"}, "executable": false,
				}
			}
		}
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"jsonrpc": "2.0", "id": 1,
			"result": map[string]any{
				"context": map[string]any{"slot": 1001},
				"value":   values,
			},
		})
	default:
		_, _ = writer.Write([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-1,"message":"unexpected"}}`))
	}
}

func TestAssertLegacyPoliciesRetiredFailsClosedOnSurvivors(t *testing.T) {
	addresses, err := legacyCustomPolicyAddresses()
	if err != nil {
		t.Fatalf("derive legacy policy addresses: %v", err)
	}
	server := httptest.NewServer(stubLegacyRPC{
		surviving: map[string]bool{addresses[2]: true}, anchor: true,
	})
	defer server.Close()
	surviving, err := AssertLegacyPoliciesRetired(context.Background(), server.URL)
	if err == nil || len(surviving) != 1 || surviving[0] != addresses[2] {
		t.Fatalf("expected a refusal naming %s, got %v, %v", addresses[2], surviving, err)
	}
	if !strings.Contains(err.Error(), "seeds 62-65") {
		t.Fatalf("refusal must name the legacy seeds: %v", err)
	}

	clear := httptest.NewServer(stubLegacyRPC{anchor: true})
	defer clear.Close()
	surviving, err = AssertLegacyPoliciesRetired(context.Background(), clear.URL)
	if err != nil || surviving != nil {
		t.Fatalf("expected a clear retirement read, got %v, %v", surviving, err)
	}
}

func TestAssertLegacyPoliciesRetiredRequiresAnchorForAllNullPolicies(t *testing.T) {
	server := httptest.NewServer(stubLegacyRPC{anchor: true})
	defer server.Close()
	if surviving, err := AssertLegacyPoliciesRetired(context.Background(), server.URL); err != nil || surviving != nil {
		t.Fatalf("valid finalized Settings anchor should permit all-null legacy policies: %v, %v", surviving, err)
	}

	withoutAnchor := httptest.NewServer(stubLegacyRPC{})
	defer withoutAnchor.Close()
	if surviving, err := AssertLegacyPoliciesRetired(context.Background(), withoutAnchor.URL); err == nil || surviving != nil {
		t.Fatalf("all-null policies without the mandatory anchor must refuse startup: %v, %v", surviving, err)
	}
}

func TestAssertLegacyPoliciesRetiredRefusesWrongGenesis(t *testing.T) {
	server := httptest.NewServer(stubLegacyRPC{
		genesis: "EtWTRABZaYq6iMfeYKouRu166VU2xqa1", anchor: true,
	})
	defer server.Close()
	if surviving, err := AssertLegacyPoliciesRetired(context.Background(), server.URL); err == nil || surviving != nil {
		t.Fatalf("wrong genesis must refuse startup before trusting null policy values: %v, %v", surviving, err)
	}
}

type fakeLegacyPolicyGateClock struct {
	now    time.Time
	sleeps []time.Duration
	reads  int
}

func (clock *fakeLegacyPolicyGateClock) Now() time.Time {
	return clock.now
}

func TestLegacyPolicyGateEnforcesOneGlobalDeadline(t *testing.T) {
	start := time.Unix(1_000_000, 0)
	deadline := start.Add(legacyPolicyGateTimeout)
	clock := &fakeLegacyPolicyGateClock{now: start}
	clientFactory := func(rpcURL string) (*RPCClient, error) {
		client, err := NewRPCClient(rpcURL)
		if err != nil {
			return nil, err
		}
		client.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
			clock.reads++
			remaining := deadline.Sub(clock.Now())
			if remaining > 0 {
				requestBudget := 15 * time.Second
				if remaining < requestBudget {
					requestBudget = remaining
				}
				clock.now = clock.now.Add(requestBudget)
			}
			return nil, errors.New("simulated RPC timeout")
		})
		client.sleep = func(ctx context.Context, duration time.Duration) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			clock.sleeps = append(clock.sleeps, duration)
			clock.now = clock.now.Add(duration)
			return nil
		}
		return client, nil
	}

	if surviving, err := assertLegacyPoliciesRetiredWithDeadline(
		context.Background(), "https://rpc.example", legacyPolicyGateTimeout, clock.Now, clientFactory,
	); err == nil || surviving != nil {
		t.Fatalf("timed-out gate unexpectedly passed: surviving=%v err=%v", surviving, err)
	} else if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timed-out gate returned the wrong error: %v", err)
	}
	if elapsed := clock.Now().Sub(start); elapsed > legacyPolicyGateTimeout {
		t.Fatalf("global gate deadline exceeded: elapsed=%s deadline=%s", elapsed, legacyPolicyGateTimeout)
	}
	if clock.reads != 4 {
		t.Fatalf("expected the global deadline to stop the fifth request, got %d reads", clock.reads)
	}
	for index, sleep := range clock.sleeps {
		if sleep != legacyPolicyGateRetryInterval {
			t.Fatalf("retry %d used %s instead of fixed %s", index+1, sleep, legacyPolicyGateRetryInterval)
		}
	}
}
