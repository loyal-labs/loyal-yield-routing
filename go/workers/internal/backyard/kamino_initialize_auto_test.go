package backyard

import (
	"bytes"
	"encoding/binary"
	"math"
	"strings"
	"testing"

	lanepolicy "github.com/loyal-labs/loyal-yield-routing/go/workers/internal/policy"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
)

// The AUTO initializer request executes through the account the AUTO policy
// is installed on.
func autoInitializerRequestFixture(t *testing.T) (RouteManifest, KaminoInitializationRequest) {
	t.Helper()
	manifest := embeddedTestManifest(t)
	r := KaminoInitializationRequest{RouteLane: autoAUTOPYUSD.Lane, Policy: installedAutoPolicyKey(),
		RecentBlockhash:      bridgeVault,
		LastValidBlockHeight: 100, RentLamports: 17_637_760, MaximumFeeLamports: 5000}
	return manifest, r
}

// autoInitializerPrestateAccounts mirrors initializationPrestateFixture for the
// candidate AUTO lane: same Settings, same metadata image, same rent
// arithmetic — with the PYUSD debt mint under Token-2022.
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
	accounts[backyardDelegate] = ConfirmedAccount{Address: backyardDelegate, Owner: system, Lamports: r.MaximumFeeLamports}
	m := ConfirmedAccount{Address: metadataAddress, Owner: kamino.ProgramID.String(), Lamports: 1, Data: make([]byte, 1032)}
	copy(m.Data, kamino.UserMetadataDiscriminator[:])
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

// The AUTO initializer message wraps the initializer at the AUTO policy's
// initializer leg, through the installed AUTO policy — and the public
// compiler must still refuse the lane outright.
func TestAutoInitializerCompilesAtItsLeg(t *testing.T) {
	t.Parallel()
	manifest, r := autoInitializerRequestFixture(t)
	message, err := manifest.compileKaminoInitializationMessage(r)
	if err != nil {
		t.Fatal(err)
	}
	policy := mustKey(r.Policy)
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
	// offset behind the discriminator: the AUTO policy's initializer leg.
	wrapped := bytes.Index(message, squads.ExecuteTransactionSyncV2Discriminator[:])
	if wrapped < 0 || wrapped+17 >= len(message) || message[wrapped+17] != lanepolicy.LoopInitialize {
		t.Fatalf("initializer not wrapped at its leg %d", lanepolicy.LoopInitialize)
	}
	repeated, err := manifest.compileKaminoInitializationMessage(r)
	if err != nil || !bytes.Equal(message, repeated) {
		t.Fatal("candidate initializer compilation is not deterministic", err)
	}
	if policy != mustKey(installedAutoPolicyKey()) {
		t.Fatal("the AUTO initializer does not execute through the installed AUTO policy")
	}
	// Any other non-selector lane stays unreviewed.
	foreign := r
	foreign.RouteLane = "Ethena/USDe/PYUSD"
	_, err = manifest.compileKaminoInitializationMessage(foreign)
	assertBudgetHold(t, err, "initializer_lane_unreviewed")
	// The request builder resolves the installed AUTO policy's account.
	resolved, err := manifest.initializationRequest(testPolicies(t), autoAUTOPYUSD.Lane, LatestBlockhash{Blockhash: bridgeVault, LastValidBlockHeight: 100}, r.RentLamports, r.MaximumFeeLamports)
	if err != nil || resolved.Policy != installedAutoPolicyKey() || manifest.validateInitializationRequest(resolved) != nil {
		t.Fatalf("bound request builder drifted: %v %+v", err, resolved)
	}
}

// Expected-effects validation is the seam the cost and decode paths consume.
// The manifest-aware form shares the exact structural shape check and
// recompiles the embedded admission through the manifest compiler, so an AUTO
// effect validates through it; the public form keeps the exact installed
// behavior and refuses the candidate lane outright.
func TestAutoInitializationEffectsValidateThroughTheManifest(t *testing.T) {
	t.Parallel()
	manifest, r := autoInitializerRequestFixture(t)
	valid := ExpectedEffects{Schema: "loyal-backyard-rwa-expected-effects/v1", Kind: "kamino-initialize", Conserved: true, Initialization: &r}
	if err := manifest.validateInitializationEffects(valid); err != nil {
		t.Fatalf("bound AUTO effect refused by the manifest validator: %v", err)
	}
	installed := r
	installed.RouteLane = PrimeUSDCLaneID
	installed.Policy = testPolicyAccount(policyKey{lane: PrimeUSDCLaneID, action: InitializeKaminoObligation})
	if err := manifest.validateInitializationEffects(ExpectedEffects{Schema: "loyal-backyard-rwa-expected-effects/v1", Kind: "kamino-initialize", Conserved: true, Initialization: &installed}); err != nil {
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
		if err := manifest.validateInitializationEffects(effects); err == nil || !strings.Contains(err.Error(), "invalid native initialization effects") {
			t.Fatalf("manifest form accepted malformed effect %d as %v", index, err)
		}
	}
}
