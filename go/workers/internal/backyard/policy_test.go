package backyard

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"testing"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
	"github.com/solana-foundation/solana-go/v2"
)

// The AUTO literal is the policy installed on Backyard's Settings, so the
// worker finds that account by equality and needs no binding.
func TestAutoPolicyIsTheInstalledAccount(t *testing.T) {
	installed := installedAutoPolicyAccount(t)
	view, err := squads.DecodeCanonicalPolicy(&chain.Account{Key: solana.MustPublicKeyFromBase58(installed.Address),
		Owner: squads.ProgramID, Lamports: installed.Lamports, Data: installed.Data})
	if err != nil || view == nil {
		t.Fatalf("installed AUTO policy does not decode: %v", err)
	}
	literal, err := autoPolicy(autoAUTOPYUSD)
	if err != nil {
		t.Fatal(err)
	}
	if !squads.ConstraintsEqual(view.Payload.Constraints, literal[:]) {
		t.Fatal("the AUTO literal is not the installed policy")
	}
}

// Every policy the worker pins by (address, digest) today is installed with
// exactly those bytes, and its literal is that account's constraints, with
// the leg the worker executes at the index the literal names.
func TestPolicyLiteralsAreTheInstalledAccounts(t *testing.T) {
	installed := installedBackyardPolicies(t)
	manifest, err := loadEmbeddedRouteManifest()
	if err != nil {
		t.Fatal(err)
	}
	type row struct {
		name, policy, digest string
		literal              []squads.InstructionConstraintView
		err                  error
		// index the worker executes the leg under today -> the literal's
		used map[byte]int
	}
	var rows []row
	add := func(name, policy, digest string, literal []squads.InstructionConstraintView, err error, used map[byte]int) {
		rows = append(rows, row{name, policy, digest, literal, err, used})
	}

	for _, family := range []BasicPolicyFamily{BasicCollateralLifecycle, BasicDebtLifecycle, BasicSwapRoutesA, BasicSwapRoutesB} {
		binding, digest, err := manifest.basicPolicyBinding(family)
		if err != nil {
			t.Fatal(err)
		}
		used := map[byte]int{}
		for name, index := range binding.Index {
			used[index] = map[string]int{"deposit": basicDeposit, "withdraw": basicWithdraw, "borrow": basicBorrow, "repay": basicRepay,
				"route0": basicSwapONycPrime, "route1": basicSwapPrimeSyrup}[name]
		}
		literal, err := basicPolicy(family)
		add(string(family), binding.Policy, digest, literal, err, used)
	}

	for _, b := range manifest.RuntimeBindings.MultiplyInitializers {
		route, err := runtimeRoute(b.Lane)
		if err != nil {
			t.Fatal(err)
		}
		literal, err := initializerPolicy(route)
		add("initialize "+b.Lane, b.Policy, b.AccountDataSHA256, literal[:], err, nil)
	}

	// The split KLend legs have no literal yet (see splitLeg): their rows prove
	// only that the worker pins installed bytes.
	for _, p := range manifest.RuntimeBindings.PrimeUSDC.Packets {
		add(RouteID+" "+string(p.Action), p.Policy, p.PolicyAccountDataSHA256, nil, nil, map[byte]int{p.PolicyConstraintIndex: splitLeg})
	}
	for _, route := range []RuntimeRoute{autoAUTOPYUSD, ethenaUSDePYUSD, primePRIMEPYUSD, primePRIMEUSDS, mapleSyrupUSDCUSDC} {
		for leg, b := range route.KaminoPolicies {
			var used map[byte]int
			if route.Lane != autoAUTOPYUSD.Lane { // AUTO's split legs are observation pins; it executes under H6X87
				used = map[byte]int{kaminoConstraintIndexForRoute(route, leg): splitLeg}
			}
			add(route.Lane+" split KLend leg", b.Policy, b.DataSHA256, nil, nil, used)
		}
	}

	forward, swaps, err := primeUSDCSwapPolicies()
	for _, b := range manifest.RuntimeBindings.PrimeUSDC.SwapPolicies {
		if b.Action == SwapUSDCToPrimeStep {
			add(RouteID+" forward swap", b.Policy, b.PolicyAccountDataSHA256, forward[:], err,
				map[byte]int{b.ConstraintBindings[0].PolicyConstraintIndex: primeForwardPlanID1, b.ConstraintBindings[1].PolicyConstraintIndex: primeForwardPlanID2})
		} else {
			add(RouteID+" swap", b.Policy, b.PolicyAccountDataSHA256, swaps[:], err, map[byte]int{b.PolicyConstraintIndex: primeSwapOut})
		}
	}
	mapleOut, err := mapleSwapOutPolicy()
	add("Maple swap out", mapleSyrupUSDCUSDC.PolicyAccounts[SwapCollateralToStableStep], mapleSyrupUSDCUSDC.PolicyHashes[SwapCollateralToStableStep], mapleOut[:], err, nil)
	mapleIn, _, err := catalogSwapPolicy("USDC", mapleSyrupUSDCUSDC.CollateralSymbol)
	add("Maple swap in", mapleSyrupUSDCUSDC.PolicyAccounts[SwapStableToCollateralStep], mapleSyrupUSDCUSDC.PolicyHashes[SwapStableToCollateralStep], mapleIn, err, nil)

	var catalog []catalogJupiterBinding
	if err := json.Unmarshal(catalogJupiterJSON, &catalog); err != nil {
		t.Fatal(err)
	}
	for _, b := range catalog {
		literal, index, err := catalogSwapPolicy(b.From, b.To)
		add("catalog "+b.From+" into "+b.To, b.Policy, b.PolicySHA256, literal, err, map[byte]int{b.ConstraintIndex: int(index)})
	}

	covered := map[string]bool{}
	for _, r := range rows {
		account, ok := installed[r.policy]
		switch {
		case r.err != nil:
			t.Errorf("%s: %v", r.name, r.err)
		case !ok:
			t.Errorf("%s: the worker pins %s, which the capture does not hold", r.name, r.policy)
		case sha256Bytes(account.Data) != r.digest:
			t.Errorf("%s: the worker pins %s at %s, but %s is installed", r.name, r.policy, r.digest, sha256Bytes(account.Data))
		default:
			covered[r.policy] = true
			view, err := squads.DecodeCanonicalPolicy(&account)
			if err != nil || view == nil || view.Settings != kaminoKey(bridgeSettings) || view.DelegatedSigner != kaminoKey(bridgeDelegate) || view.Payload.VaultIndex != 0 {
				t.Errorf("%s: %s is not a canonical policy of the vault's delegate: %v", r.name, r.policy, err)
			} else if r.literal != nil && !squads.ConstraintsEqual(view.Payload.Constraints, r.literal) {
				t.Errorf("%s: the literal is not the installed policy %s", r.name, r.policy)
			}
			for index, leg := range r.used {
				if int(index) != leg {
					t.Errorf("%s: the worker executes under constraint %d, the literal names %d", r.name, index, leg)
				}
			}
		}
	}
	for address := range installed {
		if !covered[address] {
			t.Errorf("the capture holds %s, which the worker does not pin", address)
		}
	}
}

// installedBackyardPolicies is the checked-in capture of the policies the
// worker pins, by address, each checked against its recorded digest.
func installedBackyardPolicies(t *testing.T) map[string]chain.Account {
	t.Helper()
	raw, err := os.ReadFile("testdata/installed-backyard-policies.json")
	if err != nil {
		t.Fatal(err)
	}
	var capture struct {
		Settings string `json:"settings"`
		Accounts []struct {
			Address    string `json:"address"`
			Owner      string `json:"owner"`
			Lamports   uint64 `json:"lamports"`
			Executable bool   `json:"executable"`
			DataSHA256 string `json:"dataSha256"`
			DataBase64 string `json:"dataBase64"`
		} `json:"accounts"`
	}
	if err = json.Unmarshal(raw, &capture); err != nil {
		t.Fatal(err)
	}
	if capture.Settings != bridgeSettings || len(capture.Accounts) == 0 {
		t.Fatal("capture is not of Backyard's Settings")
	}
	out := make(map[string]chain.Account, len(capture.Accounts))
	for _, a := range capture.Accounts {
		data, err := base64.StdEncoding.Strict().DecodeString(a.DataBase64)
		if err != nil || sha256Bytes(data) != a.DataSHA256 || a.Owner != squads.ProgramID.String() || a.Executable || a.Lamports == 0 {
			t.Fatalf("capture of %s drifted: %v", a.Address, err)
		}
		out[a.Address] = chain.Account{Key: kaminoKey(a.Address), Owner: squads.ProgramID, Lamports: a.Lamports, Data: data}
	}
	return out
}
