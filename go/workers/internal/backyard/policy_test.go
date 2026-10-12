package backyard

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
	"github.com/solana-foundation/solana-go/v2"
)

// No two literals are equal (each finds its own installed account), and the
// NAV report literal admits no capital movement.
func TestLiteralsAreDistinctAndNAVMovesNoCapital(t *testing.T) {
	t.Parallel()
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
	}
	nav := literals[policyKey{action: ReportNAV}]
	inner, _, err := ticketedBridgeInstructions(bridgeTestRequest(VoltrAllocateToSquads, 1_000_000))
	if err != nil || squads.Admits(nav.Constraints[bridgeArmLeg], inner[0].squads(), nil) || squads.Admits(nav.Constraints[bridgeCapitalLeg], inner[1].squads(), nil) {
		t.Fatal("the NAV literal admits an allocation")
	}
}

// A literal installed on no account, or on two, holds by name, and nothing
// else stands in for it.
func TestUninstalledLiteralHoldsByName(t *testing.T) {
	t.Parallel()
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
		if hold, ok := err.(*BudgetHold); !ok || holdCause(hold) != reason {
			t.Fatalf("%d installed accounts must hold by name, got %v", matches, err)
		}
	}
}

// installedAutoPolicyKey is the AUTO policy's installed account.
func installedAutoPolicyKey() string { return testPolicyAccount(policyKey{lane: autoAUTOPYUSD.Lane}) }

// testInstalled is every Backyard literal installed once on Backyard's
// Settings, delegated to its executor, at a seed of its own, encoded as a
// Squads policy account (encodeTestPolicy): what a build reading a Settings
// that holds exactly the literals finds. The chain's own policies are the
// live read's (observeInstalledPolicies), never a checked-in copy.
var testInstalled = sync.OnceValues(func() ([]testPolicyAccountData, error) {
	literals, err := backyardPolicies()
	if err != nil {
		return nil, err
	}
	keys := make([]policyKey, 0, len(literals))
	for key := range literals {
		keys = append(keys, key)
	}
	slices.SortFunc(keys, func(a, b policyKey) int { return strings.Compare(a.String(), b.String()) })
	out := make([]testPolicyAccountData, 0, len(keys))
	for i, key := range keys {
		account, err := encodeTestPolicy(uint64(100+i), literals[key])
		if err != nil {
			return nil, fmt.Errorf("%s: %w", key, err)
		}
		out = append(out, account)
	}
	return out, nil
})

// readCapturedInstalled is testInstalled as squads.Policies lists it.
func readCapturedInstalled() ([]squads.Installed, error) {
	accounts, err := testInstalled()
	if err != nil {
		return nil, err
	}
	var out []squads.Installed
	for _, a := range accounts {
		account := chain.Account{Key: a.address, Owner: squads.ProgramID, Lamports: 1, Data: a.data}
		view, err := squads.DecodeCanonicalPolicy(&account)
		if err != nil || view == nil {
			return nil, fmt.Errorf("%s is not a canonical policy: %v", a.address, err)
		}
		out = append(out, squads.Installed{Account: account.Key, View: view})
	}
	return out, nil
}

var loadTestPolicies = sync.OnceValues(func() (installedPolicies, error) {
	installed, err := readCapturedInstalled()
	if err != nil {
		return nil, err
	}
	return findInstalledPolicies(installed)
})

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

// testPolicyAccount is the account key's literal is installed at.
func testPolicyAccount(key policyKey) string {
	account, err := capturedTestPolicies().account(key)
	if err != nil {
		panic(err)
	}
	return account
}

// capturedPolicyProgramAccounts is getProgramAccounts' answer for the
// policies on Backyard's Settings: testInstalled, at slot.
func capturedPolicyProgramAccounts(slot int64) map[string]any {
	accounts, err := testInstalled()
	if err != nil {
		panic(err)
	}
	values := make([]any, 0, len(accounts))
	for _, a := range accounts {
		values = append(values, map[string]any{"pubkey": a.address.String(), "account": map[string]any{
			"data": []string{base64.StdEncoding.EncodeToString(a.data), "base64"}, "owner": squads.ProgramID.String(),
			"lamports": 1, "executable": false, "rentEpoch": 0, "space": len(a.data)}})
	}
	return map[string]any{"context": map[string]int64{"slot": slot}, "value": values}
}

// squadsProgramAccounts reports whether a getProgramAccounts request reads
// the Squads program: the policy read, not a Voltr receipt read.
func squadsProgramAccounts(params []json.RawMessage) bool {
	var program string
	return len(params) > 0 && json.Unmarshal(params[0], &program) == nil && program == squads.ProgramID.String()
}

type testPolicyAccountData struct {
	address solana.PublicKey
	data    []byte
}

// encodeTestPolicy is p as a canonical ProgramInteraction policy account of
// Backyard's Settings at seed, in the legacy Borsh layout the decoder reads:
// one full-permission signer (the executor), threshold 1, no time lock,
// hooks or expiration.
func encodeTestPolicy(seed uint64, p squads.Policy) (testPolicyAccountData, error) {
	settings := solanaKey(bridgeSettings)
	address, bump, err := squads.PolicyAddress(settings, seed)
	if err != nil {
		return testPolicyAccountData{}, err
	}
	var b bytes.Buffer
	le := func(v any) { _ = binary.Write(&b, binary.LittleEndian, v) }
	key := func(k solana.PublicKey) { b.Write(k[:]) }
	option := func(present bool) {
		if present {
			b.WriteByte(1)
		} else {
			b.WriteByte(0)
		}
	}
	data := func(cs []squads.DataConstraintView) {
		le(uint32(len(cs)))
		for _, c := range cs {
			le(c.DataOffset)
			b.WriteByte(c.DataValue.Kind)
			switch c.DataValue.Kind {
			case 0:
				b.WriteByte(c.DataValue.U8)
			case 1:
				le(c.DataValue.U16)
			case 2:
				le(c.DataValue.U32)
			case 3:
				le(c.DataValue.U64)
			case 4:
				b.Write(c.DataValue.U128[:])
			case 5:
				le(uint32(len(c.DataValue.Bytes)))
				b.Write(c.DataValue.Bytes)
			}
			b.WriteByte(byte(c.Operator))
		}
	}
	b.Write(squads.PolicyDiscriminator[:])
	key(settings)
	le(seed)
	b.WriteByte(bump)
	le(uint64(0))
	le(uint64(0))
	le(uint32(1))
	key(solanaKey(bridgeDelegate))
	b.WriteByte(squads.FullPermissions)
	le(uint16(1))
	le(uint32(0))
	b.WriteByte(3)
	b.WriteByte(p.VaultIndex)
	le(uint32(len(p.Constraints)))
	for _, c := range p.Constraints {
		key(c.ProgramID)
		le(uint32(len(c.AccountConstraints)))
		for _, a := range c.AccountConstraints {
			b.WriteByte(a.AccountIndex)
			if a.AccountData != nil {
				b.WriteByte(1)
				data(a.AccountData)
			} else {
				b.WriteByte(0)
				le(uint32(len(a.Pubkeys)))
				for _, k := range a.Pubkeys {
					key(k)
				}
			}
			option(a.Owner != nil)
			if a.Owner != nil {
				key(*a.Owner)
			}
		}
		data(c.DataConstraints)
	}
	option(false)
	option(false)
	le(uint32(len(p.SpendingLimits)))
	for _, l := range p.SpendingLimits {
		key(l.Mint)
		le(l.Start)
		option(l.Expiration != nil)
		if l.Expiration != nil {
			le(*l.Expiration)
		}
		b.WriteByte(l.Period)
		if l.Period == 4 {
			le(l.CustomPeriod)
		}
		option(l.Accumulate)
		le(l.MaxPerPeriod)
		le(l.MaxPerUse)
		option(l.ExactQuantity)
		le(uint64(0))
		le(int64(0))
	}
	le(int64(0))
	option(false)
	key(solanaKey(bridgeDelegate))
	return testPolicyAccountData{address: address, data: b.Bytes()}, nil
}
