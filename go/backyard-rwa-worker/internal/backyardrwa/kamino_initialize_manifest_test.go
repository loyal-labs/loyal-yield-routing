package backyardrwa

import (
	"context"
	"net/http"
	"testing"
)

func initializerManifestFixture(t *testing.T) RouteManifest {
	t.Helper()
	m, err := loadEmbeddedRouteManifest()
	if err != nil {
		t.Fatal(err)
	}
	for i, lane := range []string{PhaseOneLaneID, SelectedRouteID, "OnRe/ONyc/USDC"} {
		seed := uint64(151 + i)
		p, err := policySetupAddress(seed)
		if err != nil {
			t.Fatal(err)
		}
		m.RuntimeBindings.MultiplyInitializers = append(m.RuntimeBindings.MultiplyInitializers, KaminoInitializerBinding{Lane: lane, PolicySeed: seed, Policy: encodeBase58(p[:]), AccountDataSHA256: sha256Bytes([]byte(lane))})
	}
	return m
}

func TestInitializationManifestRejectsPartialAndChangedAuthority(t *testing.T) {
	legacy, err := loadEmbeddedRouteManifest()
	if err != nil {
		t.Fatal(err)
	}
	if legacy.validateBindings() != nil {
		t.Fatal("legacy manifest cannot load")
	}
	if _, err = legacy.initializerBinding(SelectedRouteID); err == nil {
		t.Fatal("legacy manifest silently enabled initializer")
	}
	for _, drift := range []string{"", "partial", "foreign_lane", "duplicate_lane", "duplicate_seed", "policy_address", "account_hash"} {
		m := initializerManifestFixture(t)
		switch drift {
		case "partial":
			m.RuntimeBindings.MultiplyInitializers = m.RuntimeBindings.MultiplyInitializers[:2]
		case "foreign_lane":
			m.RuntimeBindings.MultiplyInitializers[0].Lane = "OnRe/ONyc/USDS"
		case "duplicate_lane":
			m.RuntimeBindings.MultiplyInitializers[0].Lane = SelectedRouteID
		case "duplicate_seed":
			m.RuntimeBindings.MultiplyInitializers[0].PolicySeed = m.RuntimeBindings.MultiplyInitializers[1].PolicySeed
			m.RuntimeBindings.MultiplyInitializers[0].Policy = m.RuntimeBindings.MultiplyInitializers[1].Policy
		case "policy_address":
			m.RuntimeBindings.MultiplyInitializers[0].Policy = bridgeVault
		case "account_hash":
			m.RuntimeBindings.MultiplyInitializers[0].AccountDataSHA256 = "invalid"
		}
		if err := m.validateBindings(); (err == nil) != (drift == "") {
			t.Fatalf("drift=%s err=%v", drift, err)
		}
	}
	m := initializerManifestFixture(t)
	r, err := m.initializationRequest(SelectedRouteID, LatestBlockhash{Blockhash: bridgeVault, LastValidBlockHeight: 100}, 17637760, 5000)
	if err != nil || m.validateInitializationRequest(r) != nil {
		t.Fatal("bound request", err)
	}
	r.PolicySeed++
	if m.validateInitializationRequest(r) == nil {
		t.Fatal("unreviewed policy seed admitted")
	}
	r.PolicySeed--
	r.PolicyAccountDataSHA256 = sha256Bytes([]byte("different policy"))
	if m.validateInitializationRequest(r) == nil {
		t.Fatal("unreviewed policy bytes admitted")
	}
}

func TestInitializationBuilderRejectsUnactivatedPolicyBeforeSigner(t *testing.T) {
	t.Setenv("POLICY_KEYPAIR", "must never be parsed")
	legacy, err := loadEmbeddedRouteManifest()
	if err != nil {
		t.Fatal(err)
	}
	r, _ := initializationReconcileFixture(t)
	rpc, _ := NewRPCClient("https://rpc.invalid")
	rpc.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("unactivated initializer reached RPC")
		return nil, nil
	})
	err = BuildSimulateAndPersistKaminoInitialization(context.Background(), &Database{}, rpc, "controlled-op", legacy, *r.Initialization)
	assertBudgetHold(t, err, "initializer_policy_not_activated")
	m := initializerManifestFixture(t)
	err = BuildSimulateAndPersistKaminoInitialization(context.Background(), &Database{}, rpc, "controlled-op", m, *r.Initialization)
	assertBudgetHold(t, err, "initializer_request_manifest_mismatch")
}
