package backyard

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"maps"
	"math"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
)

func initializationPrestateFixture(t *testing.T) (KaminoInitializationRequest, map[string]ConfirmedAccount) {
	e, _ := initializationReconcileFixture(t)
	r := *e.Initialization
	inner, err := kaminoMultiplyInitializer(r.RouteLane)
	if err != nil {
		t.Fatal(err)
	}
	route, _ := runtimeRoute(r.RouteLane)
	metadataAddress := encodeBase58(inner.accounts[6].key[:])
	rentAddress := "SysvarRent111111111111111111111111111111111"
	system := "11111111111111111111111111111111"
	accounts := map[string]ConfirmedAccount{}
	for _, meta := range inner.accounts {
		address := encodeBase58(meta.key[:])
		accounts[address] = ConfirmedAccount{Address: address, Owner: system, Lamports: 1}
	}
	delete(accounts, route.Kamino.Obligation)
	accounts[bridgeVault] = ConfirmedAccount{Address: bridgeVault, Owner: system, Lamports: r.RentLamports}
	accounts[bridgeDelegate] = ConfirmedAccount{Address: bridgeDelegate, Owner: system, Lamports: r.MaximumFeeLamports}
	m := ConfirmedAccount{Address: metadataAddress, Owner: kamino.ProgramID.String(), Lamports: 1, Data: make([]byte, 1032)}
	copy(m.Data, kamino.UserMetadataDiscriminator[:])
	putKey(t, m.Data[80:112], bridgeVault)
	accounts[metadataAddress] = m
	accounts[route.Kamino.Market] = marketFixture(t, route.Kamino.Market)
	for _, mint := range []string{route.Kamino.CollateralMint, bridgeUSDC} {
		a := ConfirmedAccount{Address: mint, Owner: classicTokenProgram, Lamports: 1, Data: make([]byte, 82)}
		a.Data[44], a.Data[45] = 6, 1
		accounts[mint] = a
	}
	rent := ConfirmedAccount{Address: rentAddress, Owner: "Sysvar1111111111111111111111111111111111111", Lamports: 1, Data: make([]byte, 17)}
	binary.LittleEndian.PutUint64(rent.Data, 5080)
	binary.LittleEndian.PutUint64(rent.Data[8:16], math.Float64bits(1))
	accounts[rentAddress] = rent
	return r, accounts
}

// Exercise the view's absent-account contract and the native funding gate.
// Only the exact target obligation may be absent; all prerequisites must exist.
func TestInitializationPrestateRequiresAbsentTargetAndFundedExactGraph(t *testing.T) {
	for _, drift := range []string{"", "target_exists", "vault_funding", "delegate_funding", "metadata_owner", "metadata_referrer", "metadata_vault", "market_emergency", "mint_program", "mint_uninitialized", "rent_changed", "rent_nan"} {
		t.Run(drift, func(t *testing.T) {
			r, accounts := initializationPrestateFixture(t)
			inner, _ := kaminoMultiplyInitializer(r.RouteLane)
			route, _ := runtimeRoute(r.RouteLane)
			metadataAddress := encodeBase58(inner.accounts[6].key[:])
			rentAddress := "SysvarRent111111111111111111111111111111111"
			change := func(address string, fn func(*ConfirmedAccount)) {
				a := accounts[address]
				fn(&a)
				accounts[address] = a
			}
			switch drift {
			case "target_exists":
				accounts[route.Kamino.Obligation] = ConfirmedAccount{Address: route.Kamino.Obligation, Owner: kamino.ProgramID.String(), Lamports: 1}
			case "vault_funding":
				change(bridgeVault, func(a *ConfirmedAccount) { a.Lamports-- })
			case "delegate_funding":
				change(bridgeDelegate, func(a *ConfirmedAccount) { a.Lamports-- })
			case "metadata_owner":
				change(metadataAddress, func(a *ConfirmedAccount) { a.Owner = classicTokenProgram })
			case "metadata_referrer":
				change(metadataAddress, func(a *ConfirmedAccount) { a.Data[8] = 1 })
			case "metadata_vault":
				change(metadataAddress, func(a *ConfirmedAccount) { a.Data[80] ^= 1 })
			case "market_emergency":
				change(route.Kamino.Market, func(a *ConfirmedAccount) { a.Data[kaminoMarketEmergencyModeOffset] = 1 })
			case "mint_program":
				change(bridgeUSDC, func(a *ConfirmedAccount) { a.Owner = squads.ProgramID.String() })
			case "mint_uninitialized":
				change(bridgeUSDC, func(a *ConfirmedAccount) { a.Data[45] = 0 })
			case "rent_changed":
				change(rentAddress, func(a *ConfirmedAccount) { binary.LittleEndian.PutUint64(a.Data, 5081) })
			case "rent_nan":
				change(rentAddress, func(a *ConfirmedAccount) { binary.LittleEndian.PutUint64(a.Data[8:16], math.Float64bits(math.NaN())) })
			}
			slot, err := validateKaminoInitializationPrestate(context.Background(), accountView(t, 78, slices.Collect(maps.Values(accounts))), r, 77)
			if (err == nil) != (drift == "") || (err == nil && slot != 78) {
				t.Fatalf("drift=%s slot=%d err=%v", drift, slot, err)
			}
		})
	}
}

func TestInitializationMissingPrerequisiteKeepsValidatedExpiryRecovery(t *testing.T) {
	e, _ := initializationReconcileFixture(t)
	r := *e.Initialization
	raw, _ := json.Marshal(e)
	input, err := encodePhase3BuildInput(r, raw)
	if err != nil {
		t.Fatal(err)
	}
	message, err := CompileKaminoInitializationMessage(r)
	if err != nil {
		t.Fatal(err)
	}
	wire := append([]byte{1}, bytes.Repeat([]byte{9}, 64)...)
	wire = append(wire, message...)
	digest, err := Phase3IntentDigest(r, raw)
	if err != nil {
		t.Fatal(err)
	}
	op := PersistedOperation{Operation: Operation{Decision: Decision{Action: InitializeKaminoObligation, StrategyKey: r.RouteLane, Reason: "multiply_obligation_missing", IdempotencyKey: "initializer-controlled"}}, Status: Signed, SignedWire: wire, SignedWireSHA256: sha256Bytes(wire), TransactionSignature: encodeBase58(wire[1:65]), RecentBlockhash: r.RecentBlockhash, LastValidBlockHeight: r.LastValidBlockHeight}
	auth := phase3OperationAuthorization{IntentSHA256: digest, SignedWireSHA256: op.SignedWireSHA256, BuildInput: input}
	rpc := newFakeChain(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("the prestate read RPC")
		return nil, nil
	}))
	// The final send proves the persisted wire first; only then is a missing
	// policy a prestate hold of that proven wire.
	m := embeddedTestManifest(t)
	request, effects, err := m.validateSignedIdentity(auth, op)
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.validateRequestPrestate(context.Background(), rpc, accountView(t, 78, nil), request, effects)
	assertBudgetHold(t, err, "initializer_prestate_unavailable")
	op.SignedWireSHA256 = sha256Bytes([]byte("other"))
	_, _, err = m.validateSignedIdentity(auth, op)
	assertBudgetHold(t, err, "persisted_signature_or_expiry_mismatch")
}

// A fee read later than the view must not extend the policy/rent read.
func TestInitializationBuildPricesRentAndRetainsPrestateExpiry(t *testing.T) {
	r, accounts := initializationPrestateFixture(t)
	// Controlled lower rent allows the success path under the finite canary cap.
	r.RentLamports = 3_472_000
	rent := accounts["SysvarRent111111111111111111111111111111111"]
	binary.LittleEndian.PutUint64(rent.Data, 1000)
	e := ExpectedEffects{Schema: "loyal-backyard-rwa-expected-effects/v1", Kind: "kamino-initialize", Conserved: true, Initialization: &r}
	rpc := newFakeChain(t, roundTripFunc(func(req *http.Request) (*http.Response, error) {
		var body struct {
			Method string
			ID     any
		}
		_ = json.NewDecoder(req.Body).Decode(&body)
		if body.Method != "getFeeForMessage" {
			t.Fatal("unexpected build RPC", body.Method)
		}
		encoded, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": body.ID, "result": map[string]any{"context": map[string]any{"slot": 60}, "value": 5000}})
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(encoded))), Header: make(http.Header)}, nil
	}))
	cost, err := observePhase3KnownBuildCost(context.Background(), rpc, initializationView(t, accounts), r, e)
	if err != nil {
		t.Fatal(err)
	}
	if cost.SetupLamports != r.RentLamports || cost.SetupLamportsMicros <= 0 || cost.PrincipalMicros != 0 || cost.TotalMicros != cost.SetupLamportsMicros+cost.NetworkFeeMicros || cost.ValidThroughSlot != 74 {
		t.Fatalf("rent or prestate bound lost: %+v", cost)
	}
}
