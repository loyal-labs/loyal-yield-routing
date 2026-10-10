package backyard

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"testing"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
)

// Every policy Backyard executes through (backyardPolicies) is installed as
// its literal on exactly one account of the capture, and every captured
// account is some literal's. Which leg each
// instruction executes at is TestEveryRuntimeLegExecutesAtItsOwnConstraint.
func TestPolicyLiteralsAreTheInstalledAccounts(t *testing.T) {
	literals, err := backyardPolicies()
	if err != nil {
		t.Fatal(err)
	}
	captured := capturedInstalled(t)
	installed, err := findInstalledPolicies(captured)
	if err != nil {
		t.Fatal(err)
	}
	claimed := map[string]bool{}
	for key := range literals {
		account, err := installed.account(key)
		if err != nil {
			t.Errorf("%s: %v", key, err)
			continue
		}
		claimed[account] = true
	}
	for _, policy := range captured {
		if address := policy.Account.String(); !claimed[address] {
			t.Errorf("the capture holds %s (seed %d), which no literal claims", address, policy.View.PolicySeed)
		}
	}

	// The admission rule is not vacuous: a NAV report cannot move capital.
	nav := literals[policyKey{action: ReportNAV}]
	inner, _, err := ticketedBridgeInstructions(bridgeTestRequest(VoltrAllocateToSquads, 1_000_000))
	if err != nil || squads.Admits(nav.Constraints[bridgeArmLeg], inner[0].squads(), nil) || squads.Admits(nav.Constraints[bridgeCapitalLeg], inner[1].squads(), nil) {
		t.Fatal("the NAV literal admits an allocation")
	}
}

// No two literals are equal, and no literal decodes from two captured
// accounts: while a replaced policy is still installed beside its successors
// (an apply removes it only in its last create), each literal still finds one
// account.
func TestNoLiteralMatchesTwoAccounts(t *testing.T) {
	literals, err := backyardPolicies()
	if err != nil {
		t.Fatal(err)
	}
	for key, literal := range literals {
		for other, policy := range literals {
			if key != other && literal.Equal(policy) {
				t.Errorf("%s and %s are the same policy", key, other)
			}
		}
		if _, matches := squads.FindPolicy(capturedInstalled(t), solanaKey(bridgeSettings), solanaKey(bridgeDelegate), literal); matches > 1 {
			t.Errorf("%s matches %d captured accounts", key, matches)
		}
	}
}

// A literal installed on no account, or on two, holds by name, and nothing
// else stands in for it.
func TestUninstalledLiteralHoldsByName(t *testing.T) {
	policies := testPolicies(t)
	route, err := runtimeRoute(SelectedRouteID)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := loadEmbeddedRouteManifest()
	if err != nil {
		t.Fatal(err)
	}
	blockhash := LatestBlockhash{Blockhash: bridgeVault, LastValidBlockHeight: 99}
	if _, err := manifest.kaminoPacketForRoute(policies, OpenRouteStep, kaminoLegBorrow, 1, blockhash, route.Lane); err != nil {
		t.Fatal(err)
	}
	key := policyKey{family: BasicDebtLifecycle}
	for matches, reason := range map[int]string{0: "DebtLifecycle policy not installed", 2: "DebtLifecycle policy installed twice"} {
		changed := installedPolicies{}
		for k, v := range policies {
			changed[k] = v
		}
		changed[key] = installedPolicy{matches: matches}
		_, err = manifest.kaminoPacketForRoute(changed, OpenRouteStep, kaminoLegBorrow, 1, blockhash, route.Lane)
		if hold, ok := err.(*BudgetHold); !ok || hold.Reason != reason {
			t.Fatalf("%d installed accounts must hold by name, got %v", matches, err)
		}
	}
}

// installedAutoPolicyKey is the AUTO policy's installed account.
func installedAutoPolicyKey() string { return testPolicyAccount(policyKey{lane: autoAUTOPYUSD.Lane}) }

// policyCapture is testdata/installed-policies.json: one finalized
// getProgramAccounts of every policy on Backyard's Settings (the policy
// discriminator at 0, the Settings at 8), as public mainnet RPC answered it.
type policyCapture struct {
	Source     string            `json:"source"`
	CapturedAt string            `json:"capturedAt"`
	Method     string            `json:"method"`
	Params     []json.RawMessage `json:"params"`
	Result     struct {
		Context struct {
			Slot uint64 `json:"slot"`
		} `json:"context"`
		Value []struct {
			Pubkey  string `json:"pubkey"`
			Account struct {
				Data       []string `json:"data"`
				Owner      string   `json:"owner"`
				Lamports   uint64   `json:"lamports"`
				Executable bool     `json:"executable"`
				RentEpoch  uint64   `json:"rentEpoch"`
				Space      int      `json:"space"`
			} `json:"account"`
		} `json:"value"`
	} `json:"result"`
}

func readPolicyCapture() (policyCapture, error) {
	var capture policyCapture
	raw, err := os.ReadFile("testdata/installed-policies.json")
	if err != nil {
		return capture, err
	}
	if err = json.Unmarshal(raw, &capture); err != nil {
		return capture, err
	}
	var config struct {
		Commitment string `json:"commitment"`
		Filters    []struct {
			Memcmp struct {
				Offset int    `json:"offset"`
				Bytes  string `json:"bytes"`
			} `json:"memcmp"`
		} `json:"filters"`
	}
	if capture.Method != "getProgramAccounts" || len(capture.Params) != 2 || string(capture.Params[0]) != `"`+squads.ProgramID.String()+`"` ||
		json.Unmarshal(capture.Params[1], &config) != nil || config.Commitment != "finalized" || len(config.Filters) != 2 ||
		config.Filters[0].Memcmp.Offset != 0 || config.Filters[0].Memcmp.Bytes != encodeBase58(squads.PolicyDiscriminator[:]) ||
		config.Filters[1].Memcmp.Offset != 8 || config.Filters[1].Memcmp.Bytes != bridgeSettings || capture.Result.Context.Slot == 0 {
		return capture, fmt.Errorf("testdata/installed-policies.json is not a finalized read of every policy on Backyard's Settings")
	}
	return capture, nil
}

// capturedInstalled is the capture as squads.Policies lists it: every
// account a canonical policy of Backyard's Settings.
func capturedInstalled(t testing.TB) []squads.Installed {
	t.Helper()
	out, err := readCapturedInstalled()
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func readCapturedInstalled() ([]squads.Installed, error) {
	capture, err := readPolicyCapture()
	if err != nil {
		return nil, err
	}
	var out []squads.Installed
	for _, a := range capture.Result.Value {
		data, err := base64.StdEncoding.Strict().DecodeString(a.Account.Data[0])
		if err != nil || len(a.Account.Data) != 2 || a.Account.Data[1] != "base64" {
			return nil, fmt.Errorf("capture of %s is not base64", a.Pubkey)
		}
		account := chain.Account{Key: kaminoKey(a.Pubkey), Owner: kaminoKey(a.Account.Owner), Lamports: a.Account.Lamports, Executable: a.Account.Executable, Data: data}
		view, err := squads.DecodeCanonicalPolicy(&account)
		if err != nil || view == nil || view.Settings != kaminoKey(bridgeSettings) || view.PolicyAccount != account.Key {
			return nil, fmt.Errorf("%s is not a canonical policy of Backyard's Settings: %v", a.Pubkey, err)
		}
		out = append(out, squads.Installed{Account: account.Key, View: view})
	}
	return out, nil
}

var capturedLiterals struct {
	once     sync.Once
	policies installedPolicies
	err      error
}

// loadTestPolicies is where each literal is installed in the capture: what
// a build reading today's Settings finds.
func loadTestPolicies() (installedPolicies, error) {
	capturedLiterals.once.Do(func() {
		installed, err := readCapturedInstalled()
		if err == nil {
			capturedLiterals.policies, err = findInstalledPolicies(installed)
		}
		capturedLiterals.err = err
	})
	return capturedLiterals.policies, capturedLiterals.err
}

func testPolicies(t testing.TB) installedPolicies {
	t.Helper()
	policies, err := loadTestPolicies()
	if err != nil {
		t.Fatal(err)
	}
	return policies
}

// capturedTestPolicies is loadTestPolicies for fixtures without a testing.T.
func capturedTestPolicies() installedPolicies {
	policies, err := loadTestPolicies()
	if err != nil {
		panic(err)
	}
	return policies
}

// testPolicyAccount is the captured account key's literal is installed at.
func testPolicyAccount(key policyKey) string {
	account, err := capturedTestPolicies().account(key)
	if err != nil {
		panic(err)
	}
	return account
}

// capturedPolicyProgramAccounts is getProgramAccounts' answer for the
// policies on Backyard's Settings: the capture, at slot.
func capturedPolicyProgramAccounts(slot int64) map[string]any {
	capture, err := readPolicyCapture()
	if err != nil {
		panic(err)
	}
	return map[string]any{"context": map[string]int64{"slot": slot}, "value": capture.Result.Value}
}

// squadsProgramAccounts reports whether a getProgramAccounts request reads
// the Squads program: the policy read, not a Voltr receipt read.
func squadsProgramAccounts(params []json.RawMessage) bool {
	var program string
	return len(params) > 0 && json.Unmarshal(params[0], &program) == nil && program == squads.ProgramID.String()
}
