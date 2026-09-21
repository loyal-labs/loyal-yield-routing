package backyardrwa

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"strings"
	"testing"
)

// The initializer-enabled candidate request carries the binding identity
// verbatim: seed and account digest are retained from the reviewed manifest,
// never invented here.
func autoInitializerRequestFixture(t *testing.T) (RouteManifest, KaminoInitializationRequest) {
	t.Helper()
	manifest := autoInitializerFixtureManifest(t)
	binding := autoInitializerFixtureBinding(t)
	r := KaminoInitializationRequest{RouteLane: autoAUTOPYUSD.Lane, PolicySeed: binding.PolicySeed,
		PolicyAccountDataSHA256: binding.AccountDataSHA256, RecentBlockhash: bridgeVault,
		LastValidBlockHeight: 100, RentLamports: 17_637_760, MaximumFeeLamports: 5000}
	return manifest, r
}

// autoInitializerPrestateAccounts mirrors initializationPrestateFixture for the
// candidate AUTO lane: same settings seed gate, same metadata image, same rent
// arithmetic — with the policy account carrying the exact synthetic bytes the
// fixture binding digests, and the PYUSD debt mint under Token-2022.
func autoInitializerPrestateAccounts(t *testing.T, r KaminoInitializationRequest) map[string]ConfirmedAccount {
	t.Helper()
	route, err := runtimeRoute(r.RouteLane)
	if err != nil {
		t.Fatal(err)
	}
	inner, err := kaminoRouteInitializer(route)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := policySetupAddress(r.PolicySeed)
	if err != nil {
		t.Fatal(err)
	}
	policyAddress := encodeBase58(policy[:])
	metadataAddress := encodeBase58(inner.accounts[6].key[:])
	rentAddress := "SysvarRent111111111111111111111111111111111"
	system := "11111111111111111111111111111111"
	accounts := map[string]ConfirmedAccount{}
	for _, meta := range inner.accounts {
		address := encodeBase58(meta.key[:])
		accounts[address] = ConfirmedAccount{Address: address, Owner: system, Lamports: 1}
	}
	delete(accounts, route.Kamino.Obligation)
	settings := setupSettingsAccount(t)
	// Captured Settings seed counter is exactly the candidate seed: the next
	// derived seed must stay strictly ahead of the bound policy seed.
	binary.LittleEndian.PutUint64(settings.Data[159:167], r.PolicySeed)
	accounts[bridgeSettings] = settings
	accounts[policyAddress] = ConfirmedAccount{Address: policyAddress, Owner: bridgeSquadsProgram, Lamports: 1,
		Data: []byte(autoInitializerFixtureSyntheticAccountData)}
	accounts[bridgeVault] = ConfirmedAccount{Address: bridgeVault, Owner: system, Lamports: r.RentLamports}
	accounts[bridgeDelegate] = ConfirmedAccount{Address: bridgeDelegate, Owner: system, Lamports: r.MaximumFeeLamports}
	m := ConfirmedAccount{Address: metadataAddress, Owner: kaminoProgram, Lamports: 1, Data: make([]byte, 1032)}
	copy(m.Data, []byte{157, 214, 220, 235, 98, 135, 171, 28})
	putKey(t, m.Data[80:112], bridgeVault)
	accounts[metadataAddress] = m
	accounts[route.Kamino.Market] = marketFixture(t, route.Kamino.Market)
	collateral := ConfirmedAccount{Address: route.Kamino.CollateralMint, Owner: classicTokenProgram, Lamports: 1, Data: make([]byte, 82)}
	collateral.Data[45] = 1
	accounts[route.Kamino.CollateralMint] = collateral
	accounts[route.Kamino.DebtMint] = ConfirmedAccount{Address: route.Kamino.DebtMint, Owner: token2022Program, Lamports: 1,
		Data: token2022DebtMintImage("valid")}
	rent := ConfirmedAccount{Address: rentAddress, Owner: "Sysvar1111111111111111111111111111111111111", Lamports: 1, Data: make([]byte, 17)}
	binary.LittleEndian.PutUint64(rent.Data, 5080)
	binary.LittleEndian.PutUint64(rent.Data[8:16], math.Float64bits(1))
	accounts[rentAddress] = rent
	return accounts
}

// token2022DebtMintImage builds the reviewed PYUSD-shaped Token-2022 mint:
// classic base layout plus the supported extension set (zeroed transfer fee,
// disabled transfer hook). Variants exercise the refusals.
func token2022DebtMintImage(variant string) []byte {
	data := make([]byte, 166)
	data[44], data[45], data[165] = 6, 1, 1
	appendExtension := func(kind uint16, value []byte) {
		header := make([]byte, 4)
		binary.LittleEndian.PutUint16(header, kind)
		binary.LittleEndian.PutUint16(header[2:], uint16(len(value)))
		data = append(data, header...)
		data = append(data, value...)
	}
	appendExtension(1, make([]byte, 108)) // TransferFeeConfig, both fee windows off.
	hook := make([]byte, 64)              // TransferHook: disabled program.
	vault := mustKey(bridgeVault)
	copy(hook[:32], vault[:])
	appendExtension(14, hook)
	switch variant {
	case "unsupported":
		data[166] = 99
	case "fee_enabled":
		data[170+88] = 1
	case "truncated":
		data[168] = 200 // Declared length exceeds the remaining tail.
	}
	return data
}

func autoInitializerTransport(t *testing.T, accounts map[string]ConfirmedAccount) *RPCClient {
	t.Helper()
	rpc, _ := NewRPCClient("https://rpc.invalid")
	rpc.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		var body struct {
			Method string
			Params []json.RawMessage
			ID     any
		}
		_ = json.NewDecoder(req.Body).Decode(&body)
		if body.Method != "getMultipleAccounts" {
			t.Fatal("unexpected RPC method", body.Method)
		}
		var addresses []string
		var config map[string]any
		_ = json.Unmarshal(body.Params[0], &addresses)
		_ = json.Unmarshal(body.Params[1], &config)
		if config["commitment"] != "confirmed" || config["minContextSlot"] != float64(77) {
			t.Fatal("unanchored prestate")
		}
		values := make([]any, len(addresses))
		for i, address := range addresses {
			if a, ok := accounts[address]; ok {
				values[i] = map[string]any{"owner": a.Owner, "lamports": a.Lamports, "executable": a.Executable,
					"data": []string{base64.StdEncoding.EncodeToString(a.Data), "base64"}}
			}
		}
		encoded, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": body.ID,
			"result": map[string]any{"context": map[string]any{"slot": 78}, "value": values}})
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(encoded))), Header: make(http.Header)}, nil
	})
	return rpc
}

// autoInitializerFundedObligation returns the fixture accounts with the AUTO
// obligation funded at the exact raw amounts given (debt is a u64-by-2^60
// scaled fraction, little endian over 128 bits).
func autoInitializerFundedObligation(t *testing.T, r KaminoInitializationRequest, collateral, debt uint64) map[string]ConfirmedAccount {
	t.Helper()
	route, err := runtimeRoute(r.RouteLane)
	if err != nil {
		t.Fatal(err)
	}
	accounts := autoInitializerPrestateAccounts(t, r)
	data := make([]byte, kaminoObligationLength)
	copy(data, kaminoObligationDiscriminator[:])
	binary.LittleEndian.PutUint64(data[8:16], 1)
	putKey(t, data[32:64], route.Kamino.Market)
	putKey(t, data[64:96], route.Kamino.Vault)
	putKey(t, data[96:128], route.Kamino.CollateralReserve)
	binary.LittleEndian.PutUint64(data[128:136], collateral)
	putKey(t, data[1208:1240], route.Kamino.DebtReserve)
	binary.LittleEndian.PutUint64(data[1296:1304], debt<<60)
	binary.LittleEndian.PutUint64(data[1304:1312], debt>>4)
	accounts[route.Kamino.Obligation] = ConfirmedAccount{Address: route.Kamino.Obligation, Owner: kaminoProgram, Lamports: 1, Data: data}
	return accounts
}

// The candidate message must wrap the initializer at exactly the appended
// eighth index against exactly the reviewed policy identity — and the public
// compiler must still refuse the lane outright.
func TestAutoInitializerCandidateCompilesOnlyAgainstReviewedBinding(t *testing.T) {
	manifest, r := autoInitializerRequestFixture(t)
	binding := autoInitializerFixtureBinding(t)
	message, err := manifest.compileKaminoInitializationMessage(r)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := policySetupAddress(binding.PolicySeed)
	if err != nil {
		t.Fatal(err)
	}
	route, err := runtimeRoute(autoAUTOPYUSD.Lane)
	if err != nil {
		t.Fatal(err)
	}
	obligation, err := decodeBase58PublicKey(route.Kamino.Obligation)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(message, policy[:]) || !bytes.Contains(message, obligation[:]) {
		t.Fatal("candidate initializer message lost the reviewed policy or obligation identity")
	}
	// The Squads execute-sync payload pins the constraint index at a fixed
	// offset behind the discriminator: exactly the appended eighth index.
	wrapped := bytes.Index(message, squadsExecuteSyncDiscriminator)
	if wrapped < 0 || wrapped+17 >= len(message) || message[wrapped+17] != autoInitializerConstraintIndex {
		t.Fatalf("initializer not wrapped at the appended index %d", autoInitializerConstraintIndex)
	}
	repeated, err := manifest.compileKaminoInitializationMessage(r)
	if err != nil || !bytes.Equal(message, repeated) {
		t.Fatal("candidate initializer compilation is not deterministic", err)
	}
	// Identity comes from the manifest, never from the request.
	drifted := r
	drifted.PolicySeed = autoFixtureSeed
	_, err = manifest.compileKaminoInitializationMessage(drifted)
	assertBudgetHold(t, err, "initializer_request_manifest_mismatch")
	drifted = r
	drifted.PolicyAccountDataSHA256 = sha256Bytes([]byte("other candidate bytes"))
	_, err = manifest.compileKaminoInitializationMessage(drifted)
	assertBudgetHold(t, err, "initializer_request_manifest_mismatch")
	// Seven proven constraints never imply the eighth.
	seven := autoFixtureManifest(t)
	_, err = seven.compileKaminoInitializationMessage(r)
	assertBudgetHold(t, err, "auto_initializer_constraint_not_reviewed")
	// No reviewed binding at all keeps every AUTO initializer path held: the
	// explicit absent fixture (the shipped pre-install state) holds with the
	// shipped token, while the embedded manifest's installed binding refuses
	// the candidate identity with the exact typed mismatch.
	_, absentErr := autoAbsentBindingManifest(t).compileKaminoInitializationMessage(r)
	assertBudgetHold(t, absentErr, "auto_policy_not_activated")
	embedded := requireEmbeddedInstalledBinding(t)
	_, embeddedErr := embedded.compileKaminoInitializationMessage(r)
	assertBudgetHold(t, embeddedErr, "initializer_request_manifest_mismatch")
	// The installed state compiles end to end: the request built from the
	// embedded manifest itself resolves exactly the installed identity.
	installedRequest, installedErr := embedded.initializationRequest(autoAUTOPYUSD.Lane, LatestBlockhash{Blockhash: bridgeVault, LastValidBlockHeight: 100}, r.RentLamports, r.MaximumFeeLamports)
	if installedErr != nil {
		t.Fatal(installedErr)
	}
	if installedRequest.PolicySeed != installedAutoPolicySeed || installedRequest.PolicyAccountDataSHA256 != installedAutoPolicyDigest {
		t.Fatalf("installed initializer request lost the installed identity: %+v", installedRequest)
	}
	if _, err := embedded.compileKaminoInitializationMessage(installedRequest); err != nil {
		t.Fatalf("installed initializer did not compile against the embedded manifest: %v", err)
	}
	// Any other non-selector lane stays unreviewed.
	foreign := r
	foreign.RouteLane = "Ethena/USDe/PYUSD"
	_, err = manifest.compileKaminoInitializationMessage(foreign)
	assertBudgetHold(t, err, "initializer_lane_unreviewed")
	// The public production compiler and post-state validator refuse AUTO.
	if _, err := CompileKaminoInitializationMessage(r); err == nil || !strings.Contains(err.Error(), "unreviewed Multiply initializer lane") {
		t.Fatalf("public compiler admitted AUTO: %v", err)
	}
	funded := ConfirmedAccount{Lamports: r.RentLamports, Data: make([]byte, kaminoObligationLength)}
	if err := validateInitializedKaminoObligation(r, funded); err == nil || !strings.Contains(err.Error(), "unreviewed initialized obligation") {
		t.Fatalf("public initialized validator admitted AUTO: %v", err)
	}
	// The manifest-aware request builder resolves the same binding identity.
	resolved, err := manifest.initializationRequest(autoAUTOPYUSD.Lane, LatestBlockhash{Blockhash: bridgeVault, LastValidBlockHeight: 100}, r.RentLamports, r.MaximumFeeLamports)
	if err != nil || resolved.PolicySeed != binding.PolicySeed || resolved.PolicyAccountDataSHA256 != binding.AccountDataSHA256 || manifest.validateInitializationRequest(resolved) != nil {
		t.Fatalf("bound request builder drifted: %v %+v", err, resolved)
	}
}

// Absent-only admission for the candidate lane, with the Token-2022 debt mint
// admitted through the reviewed extension parser and every malformed or
// unsupported variant refused.
func TestAutoInitializerPrestateAbsentObligationAndToken2022Mint(t *testing.T) {
	for _, drift := range []string{"", "target_exists", "settings_seed", "policy_hash", "policy_bytes", "mint_program", "mint_uninitialized", "mint_unsupported_extension", "mint_transfer_fee_enabled", "mint_truncated_tlv", "classic_extensionless_debt", "rent_changed"} {
		t.Run(drift, func(t *testing.T) {
			manifest, r := autoInitializerRequestFixture(t)
			accounts := autoInitializerPrestateAccounts(t, r)
			route, err := runtimeRoute(r.RouteLane)
			if err != nil {
				t.Fatal(err)
			}
			policy, err := policySetupAddress(r.PolicySeed)
			if err != nil {
				t.Fatal(err)
			}
			policyAddress := encodeBase58(policy[:])
			change := func(address string, fn func(*ConfirmedAccount)) {
				a := accounts[address]
				fn(&a)
				accounts[address] = a
			}
			switch drift {
			case "target_exists":
				accounts[route.Kamino.Obligation] = ConfirmedAccount{Address: route.Kamino.Obligation, Owner: "11111111111111111111111111111111", Lamports: 1}
			case "settings_seed":
				change(bridgeSettings, func(a *ConfirmedAccount) { binary.LittleEndian.PutUint64(a.Data[159:167], r.PolicySeed-1) })
			case "policy_hash":
				change(policyAddress, func(a *ConfirmedAccount) { a.Data[0] ^= 1 })
			case "policy_bytes":
				change(policyAddress, func(a *ConfirmedAccount) { a.Data = []byte("different candidate account bytes") })
			case "mint_program":
				change(route.Kamino.DebtMint, func(a *ConfirmedAccount) { a.Owner = classicTokenProgram })
			case "mint_uninitialized":
				change(route.Kamino.DebtMint, func(a *ConfirmedAccount) { a.Data[45] = 0 })
			case "mint_unsupported_extension":
				change(route.Kamino.DebtMint, func(a *ConfirmedAccount) { a.Data = token2022DebtMintImage("unsupported") })
			case "mint_transfer_fee_enabled":
				change(route.Kamino.DebtMint, func(a *ConfirmedAccount) { a.Data = token2022DebtMintImage("fee_enabled") })
			case "mint_truncated_tlv":
				change(route.Kamino.DebtMint, func(a *ConfirmedAccount) { a.Data = token2022DebtMintImage("truncated") })
			case "classic_extensionless_debt":
				// An extensionless 82-byte Token-2022 mint is admitted by the
				// reviewed parser exactly as on the execution path; the
				// extension-bearing "valid" fixture above is what proves the
				// candidate PYUSD is not waved through a synthetic shortcut.
			case "rent_changed":
				change("SysvarRent111111111111111111111111111111111", func(a *ConfirmedAccount) { binary.LittleEndian.PutUint64(a.Data, 5081) })
			}
			slot, err := manifest.validateKaminoInitializationPrestate(context.Background(), autoInitializerTransport(t, accounts), r, 77)
			// An extensionless 82-byte Token-2022 mint is admitted by the same
			// reviewed parser the execution path uses; only real drift holds.
			pass := drift == "" || drift == "classic_extensionless_debt"
			if (err == nil) != pass || (err == nil && slot != 78) {
				t.Fatalf("drift=%s slot=%d err=%v", drift, slot, err)
			}
			if drift == "target_exists" {
				assertBudgetHold(t, err, "initializer_obligation_already_present")
			}
			if strings.HasPrefix(drift, "mint_") && drift != "classic_extensionless_debt" {
				assertBudgetHold(t, err, "initializer_seed_mint_unavailable")
			}
			// The public prestate still refuses the candidate lane outright —
			// no manifest, no admission — with the installed typed hold, so
			// retry-vs-fatal handling never changes per lane.
			public, _ := NewRPCClient("https://rpc.invalid")
			public.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
				t.Fatal("public prestate reached RPC for the candidate lane")
				return nil, nil
			})
			_, publicErr := validateKaminoInitializationPrestate(context.Background(), public, r, 77)
			assertBudgetHold(t, publicErr, "initializer_prestate_unavailable")
		})
	}
}

// The reentry forecast prices recreation of the exact obligation the validated
// source exit is forecast to close: same identity, observed amounts, bounded.
func TestAutoInitializerReentryForecastIsBounded(t *testing.T) {
	manifest, r := autoInitializerRequestFixture(t)
	const collateralRaw, debtRaw = uint64(2_000_000), uint64(1_000_000)
	// The exact observed funded lane at the exact exit bound is admitted.
	slot, err := manifest.validateKaminoReentryForecastPrestate(context.Background(),
		autoInitializerTransport(t, autoInitializerFundedObligation(t, r, collateralRaw, debtRaw)), r, 77,
		selectorExitBound{MaxCollateralRaw: int64(collateralRaw), MaxDebtRaw: int64(debtRaw)})
	if err != nil || slot != 78 {
		t.Fatalf("exact observed reentry position: slot=%d err=%v", slot, err)
	}
	// A position above either bound is not the observed lane.
	assertBudgetHold(t, holdFor(func() error {
		_, err := manifest.validateKaminoReentryForecastPrestate(context.Background(),
			autoInitializerTransport(t, autoInitializerFundedObligation(t, r, collateralRaw+1, debtRaw)), r, 77,
			selectorExitBound{MaxCollateralRaw: int64(collateralRaw), MaxDebtRaw: int64(debtRaw)})
		return err
	}), "initializer_reentry_obligation_unobserved")
	assertBudgetHold(t, holdFor(func() error {
		_, err := manifest.validateKaminoReentryForecastPrestate(context.Background(),
			autoInitializerTransport(t, autoInitializerFundedObligation(t, r, collateralRaw, debtRaw+1)), r, 77,
			selectorExitBound{MaxCollateralRaw: int64(collateralRaw), MaxDebtRaw: int64(debtRaw)})
		return err
	}), "initializer_reentry_obligation_unobserved")
	// A negative exit bound is invalid before any observation.
	assertBudgetHold(t, holdFor(func() error {
		_, err := manifest.validateKaminoReentryForecastPrestate(context.Background(), nil, r, 77,
			selectorExitBound{MaxCollateralRaw: -1, MaxDebtRaw: int64(debtRaw)})
		return err
	}), "initializer_reentry_bound_invalid")
	// The public reentry forecast still refuses the candidate lane outright,
	// with the same typed hold as every unreviewed lane.
	_, publicErr := validateKaminoReentryForecastPrestate(context.Background(), nil, r, 77,
		selectorExitBound{MaxCollateralRaw: int64(collateralRaw), MaxDebtRaw: int64(debtRaw)})
	assertBudgetHold(t, publicErr, "initializer_prestate_unavailable")
}

func holdFor(run func() error) error { return run() }

// The post-state validator keeps its exact installed empty-state checks on the
// candidate lane once the binding resolved.
func TestAutoInitializerEmptyObligationValidatedThroughBinding(t *testing.T) {
	manifest, r := autoInitializerRequestFixture(t)
	route, err := runtimeRoute(r.RouteLane)
	if err != nil {
		t.Fatal(err)
	}
	empty := ConfirmedAccount{Address: route.Kamino.Obligation, Owner: kaminoProgram, Lamports: r.RentLamports, Data: make([]byte, kaminoObligationLength)}
	copy(empty.Data, kaminoObligationDiscriminator[:])
	binary.LittleEndian.PutUint64(empty.Data[8:16], 1)
	putKey(t, empty.Data[32:64], route.Kamino.Market)
	putKey(t, empty.Data[64:96], route.Kamino.Vault)
	if err := manifest.validateInitializedKaminoObligation(r, empty); err != nil {
		t.Fatal(err)
	}
	funded := append([]byte(nil), empty.Data...)
	putKey(t, funded[96:128], route.Kamino.CollateralReserve)
	binary.LittleEndian.PutUint64(funded[128:136], 1)
	if err := manifest.validateInitializedKaminoObligation(r, ConfirmedAccount{Address: route.Kamino.Obligation, Owner: kaminoProgram, Lamports: r.RentLamports, Data: funded}); err == nil {
		t.Fatal("funded obligation validated as empty")
	}
	if err := manifest.validateInitializedKaminoObligation(r, ConfirmedAccount{Address: route.Kamino.Obligation, Owner: kaminoProgram, Lamports: r.RentLamports + 1, Data: append([]byte(nil), empty.Data...)}); err == nil {
		t.Fatal("changed rent validated")
	}
	// The post-state validator enforces the same request identity as compile
	// and prestate: seed and account digest come from the reviewed binding.
	driftedSeed := r
	driftedSeed.PolicySeed = autoFixtureSeed
	assertBudgetHold(t, manifest.validateInitializedKaminoObligation(driftedSeed, empty), "initializer_request_manifest_mismatch")
	driftedHash := r
	driftedHash.PolicyAccountDataSHA256 = sha256Bytes([]byte("other candidate bytes"))
	assertBudgetHold(t, manifest.validateInitializedKaminoObligation(driftedHash, empty), "initializer_request_manifest_mismatch")
	// Both binding states: the explicit absent fixture (the shipped pre-install
	// state) holds with the shipped token; the embedded manifest's installed
	// binding refuses the candidate identity with the exact typed mismatch.
	assertBudgetHold(t, autoAbsentBindingManifest(t).validateInitializedKaminoObligation(r, empty), "auto_policy_not_activated")
	assertBudgetHold(t, requireEmbeddedInstalledBinding(t).validateInitializedKaminoObligation(r, empty), "initializer_request_manifest_mismatch")
}

// The build pipeline refuses the candidate before any authority is loaded or
// RPC is reached, in both binding states: the explicit absent fixture (the
// shipped pre-install state) holds with the shipped token, and the embedded
// manifest's installed binding refuses the candidate identity with the exact
// typed mismatch.
func TestAutoInitializerBuildRequiresReviewedBinding(t *testing.T) {
	_, r := autoInitializerRequestFixture(t)
	embedded := requireEmbeddedInstalledBinding(t)
	t.Setenv("POLICY_KEYPAIR", "must never be parsed")
	rpc, _ := NewRPCClient("https://rpc.invalid")
	rpc.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("unactivated candidate initializer reached RPC")
		return nil, nil
	})
	err := BuildSimulateAndPersistKaminoInitialization(context.Background(), &Database{}, rpc, "controlled-op", embedded, r)
	assertBudgetHold(t, err, "initializer_request_manifest_mismatch")
	err = BuildSimulateAndPersistKaminoInitialization(context.Background(), &Database{}, rpc, "controlled-op", autoAbsentBindingManifest(t), r)
	assertBudgetHold(t, err, "auto_policy_not_activated")
	seven := autoFixtureManifest(t)
	err = BuildSimulateAndPersistKaminoInitialization(context.Background(), &Database{}, rpc, "controlled-op", seven, r)
	assertBudgetHold(t, err, "auto_initializer_constraint_not_reviewed")
	manifest, bound := autoInitializerRequestFixture(t)
	drifted := bound
	drifted.PolicySeed = autoFixtureSeed
	err = BuildSimulateAndPersistKaminoInitialization(context.Background(), &Database{}, rpc, "controlled-op", manifest, drifted)
	assertBudgetHold(t, err, "initializer_request_manifest_mismatch")
}

// Expected-effects validation is the seam the cost and decode paths consume.
// The manifest-aware form shares the exact structural shape check and
// recompiles the embedded admission through the manifest compiler, so an AUTO
// effect only validates when its request matches the reviewed binding; the
// public form keeps the exact installed behavior and refuses the candidate
// lane outright.
func TestAutoInitializationEffectsValidateThroughTheManifestBinding(t *testing.T) {
	manifest, r := autoInitializerRequestFixture(t)
	valid := ExpectedEffects{Schema: "loyal-backyard-rwa-expected-effects/v1", Kind: "kamino-initialize", Conserved: true, Initialization: &r}
	if err := manifest.validateInitializationEffects(valid); err != nil {
		t.Fatalf("bound AUTO effect refused by the manifest validator: %v", err)
	}
	// Identity retention: a request that drifts from the binding is a typed
	// hold, never compiled from request-supplied values.
	driftedSeed := r
	driftedSeed.PolicySeed = autoFixtureSeed
	assertBudgetHold(t, manifest.validateInitializationEffects(ExpectedEffects{Schema: "loyal-backyard-rwa-expected-effects/v1", Kind: "kamino-initialize", Conserved: true, Initialization: &driftedSeed}), "initializer_request_manifest_mismatch")
	driftedHash := r
	driftedHash.PolicyAccountDataSHA256 = sha256Bytes([]byte("other candidate bytes"))
	assertBudgetHold(t, manifest.validateInitializationEffects(ExpectedEffects{Schema: "loyal-backyard-rwa-expected-effects/v1", Kind: "kamino-initialize", Conserved: true, Initialization: &driftedHash}), "initializer_request_manifest_mismatch")

	// The public form refuses the candidate lane outright and stays byte-identical
	// for installed selector effects.
	if err := validateInitializationEffects(valid); err == nil || !strings.Contains(err.Error(), "unreviewed Multiply initializer lane") {
		t.Fatalf("public effects validator did not plainly refuse the AUTO candidate: %v", err)
	}
	installed := r
	installed.RouteLane = PhaseOneLaneID
	installed.PolicySeed = 141
	installed.PolicyAccountDataSHA256 = sha256Bytes([]byte("local candidate policy; hash does not enter wire"))
	if err := validateInitializationEffects(ExpectedEffects{Schema: "loyal-backyard-rwa-expected-effects/v1", Kind: "kamino-initialize", Conserved: true, Initialization: &installed}); err != nil {
		t.Fatalf("installed selector effect refused after the shared-shape refactor: %v", err)
	}

	// Malformed shapes fail identically in both forms before any compilation.
	malformed := []ExpectedEffects{
		{Schema: "loyal-backyard-rwa-expected-effects/v2", Kind: "kamino-initialize", Conserved: true, Initialization: &r},
		{Schema: "loyal-backyard-rwa-expected-effects/v1", Kind: "kamino-deposit", Conserved: true, Initialization: &r},
		{Schema: "loyal-backyard-rwa-expected-effects/v1", Kind: "kamino-initialize", Conserved: false, Initialization: &r},
		{Schema: "loyal-backyard-rwa-expected-effects/v1", Kind: "kamino-initialize", Conserved: true, Accounts: []ExpectedAccountEffect{{}}, Initialization: &r},
		{Schema: "loyal-backyard-rwa-expected-effects/v1", Kind: "kamino-initialize", Conserved: true},
		{Schema: "loyal-backyard-rwa-expected-effects/v1", Kind: "kamino-initialize", Conserved: true, Initialization: &r, Deposit: &ExpectedDeposit{MinimumDebitRaw: 1}},
		{Schema: "loyal-backyard-rwa-expected-effects/v1", Kind: "kamino-initialize", Conserved: true, Initialization: &r, ReturnData: &ExpectedReturnData{}},
	}
	for index, effects := range malformed {
		if err := validateInitializationEffects(effects); err == nil || !strings.Contains(err.Error(), "invalid native initialization effects") {
			t.Fatalf("public form accepted malformed effect %d as %v", index, err)
		}
		if err := manifest.validateInitializationEffects(effects); err == nil || !strings.Contains(err.Error(), "invalid native initialization effects") {
			t.Fatalf("manifest form accepted malformed effect %d as %v", index, err)
		}
	}
}
