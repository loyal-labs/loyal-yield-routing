package backyard

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"os"
	"slices"
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

// installedBridgePolicy is one captured bridge policy account.
type installedBridgePolicy struct {
	Address    string `json:"address"`
	Seed       uint64 `json:"seed"`
	Owner      string `json:"owner"`
	Lamports   uint64 `json:"lamports"`
	Executable bool   `json:"executable"`
	Data       string `json:"data"`
	DataSHA256 string `json:"dataSha256"`
}

func installedBridgePolicies(t *testing.T) map[string]installedBridgePolicy {
	t.Helper()
	raw, err := os.ReadFile("testdata/installed-bridge-policies.json")
	if err != nil {
		t.Fatal(err)
	}
	var capture struct {
		Source, Method, Commitment, CapturedAt string
		Slot                                   uint64
		Accounts                               []installedBridgePolicy
	}
	if err := json.Unmarshal(raw, &capture); err != nil {
		t.Fatal(err)
	}
	if capture.Source == "" || capture.Method != "getMultipleAccounts" || capture.Commitment != "finalized" || capture.CapturedAt == "" || capture.Slot == 0 {
		t.Fatal("bridge policy capture has no provenance")
	}
	out := map[string]installedBridgePolicy{}
	for _, account := range capture.Accounts {
		data, err := base64.StdEncoding.Strict().DecodeString(account.Data)
		if err != nil || sha256Bytes(data) != account.DataSHA256 {
			t.Fatalf("bridge policy capture %s data drift: %v", account.Address, err)
		}
		out[account.Address] = account
	}
	return out
}

// Each bridge literal is the policy installed for its action: the captured
// account, at its seed under Backyard's Settings, decodes with its one
// delegate and daily USDC limit to the literal's constraints and limit; the
// builder executes the action through that account, each instruction at its
// leg's constraint, which admits it; and the capture still matches the
// manifest's masked pin the worker checks today.
func TestBridgePoliciesAreTheInstalledAccounts(t *testing.T) {
	installed := installedBridgePolicies(t)
	manifest, err := loadEmbeddedRouteManifest()
	if err != nil {
		t.Fatal(err)
	}
	literals := bridgePolicies()
	for _, tc := range []struct {
		action  Action
		address string
		seed    uint64
		amount  uint64
	}{
		{VoltrAllocateToSquads, bridgeAllocationPolicy, 152, 1_000_000},
		{ReportNAV, bridgeNAVPolicy, 153, 0},
		{StageSquadsToVoltr, bridgeStagePolicy, 154, 1_000_000},
		{VoltrRestoreIdle, bridgeWithdrawPolicy, 155, 1_000_000},
	} {
		t.Run(string(tc.action), func(t *testing.T) {
			account, ok := installed[tc.address]
			if !ok || account.Seed != tc.seed || account.Owner != squads.ProgramID.String() || account.Executable || account.Lamports == 0 {
				t.Fatal("bridge policy capture identity drift")
			}
			data, _ := base64.StdEncoding.DecodeString(account.Data)
			view, err := squads.DecodeLimitedPolicy(&chain.Account{Key: solana.MustPublicKeyFromBase58(tc.address), Owner: squads.ProgramID, Lamports: account.Lamports, Data: data})
			if err != nil || view == nil {
				t.Fatalf("installed bridge policy does not decode: %v", err)
			}
			if view.Settings != solanaKey(bridgeSettings) || view.PolicySeed != tc.seed || view.PolicyAccount.String() != tc.address ||
				view.DelegatedSigner != solanaKey(bridgeDelegate) || view.Payload.VaultIndex != 0 {
				t.Fatal("installed bridge policy is not Backyard's delegate on the vault at its seed")
			}
			literal := literals[tc.action]
			if !squads.ConstraintsEqual(view.Payload.Constraints, literal.constraints) {
				t.Fatal("the bridge literal's constraints are not the installed policy's")
			}
			if !squads.SpendingLimitsEqual(view.Payload.SpendingLimits, literal.limits) {
				t.Fatal("the bridge literal's spending limit is not the installed policy's")
			}

			inner, policy, indexes, err := ticketedBridgeInstructions(bridgeTestRequest(tc.action, tc.amount))
			if err != nil || policy != mustKey(tc.address) || len(indexes) != len(inner) {
				t.Fatalf("the builder does not execute %s through its policy: %v", tc.action, err)
			}
			for i, ix := range inner {
				if int(indexes[i]) >= len(literal.constraints) || !constraintAdmits(literal.constraints[indexes[i]], ix) {
					t.Fatalf("instruction %d is not admitted by its leg's constraint %d", i, indexes[i])
				}
			}

			pin, err := manifest.bridgePolicy(tc.action)
			if err != nil || pin.Account != tc.address || !maskedPolicyDigestMatches(data, pin.MaskedByteRanges, pin.NormalizedDigest) {
				t.Fatalf("the capture no longer matches the manifest's masked pin: %v", err)
			}
		})
	}
	// The admission rule is not vacuous: a NAV report cannot move capital.
	inner, _, _, err := ticketedBridgeInstructions(bridgeTestRequest(VoltrAllocateToSquads, 1_000_000))
	if err != nil || constraintAdmits(literals[ReportNAV].constraints[bridgeArmLeg], inner[0]) || constraintAdmits(literals[ReportNAV].constraints[bridgeCapitalLeg], inner[1]) {
		t.Fatal("the NAV literal admits an allocation")
	}
}

// constraintAdmits is Squads' ProgramInteraction rule for one constraint over
// pinned-key slots and integer or byte data predicates.
func constraintAdmits(c squads.InstructionConstraintView, ix compiledInstruction) bool {
	if c.ProgramID != solana.PublicKey(ix.program) {
		return false
	}
	for _, a := range c.AccountConstraints {
		if len(a.AccountData) > 0 || a.Owner != nil || int(a.AccountIndex) >= len(ix.accounts) || !slices.Contains(a.Pubkeys, solana.PublicKey(ix.accounts[a.AccountIndex].key)) {
			return false
		}
	}
	for _, d := range c.DataConstraints {
		at := int(d.DataOffset)
		var value, bound uint64
		switch d.DataValue.Kind {
		case 0:
			if at+1 > len(ix.data) {
				return false
			}
			value, bound = uint64(ix.data[at]), uint64(d.DataValue.U8)
		case 3:
			if at+8 > len(ix.data) {
				return false
			}
			value, bound = binary.LittleEndian.Uint64(ix.data[at:]), d.DataValue.U64
		case 5:
			if d.Operator != squads.OpEquals || at+len(d.DataValue.Bytes) > len(ix.data) || !bytes.Equal(ix.data[at:at+len(d.DataValue.Bytes)], d.DataValue.Bytes) {
				return false
			}
			continue
		default:
			return false
		}
		if !map[squads.DataOperatorView]bool{squads.OpEquals: value == bound, squads.OpNotEquals: value != bound, squads.OpGreaterThan: value > bound,
			squads.OpGreaterThanOrEqualTo: value >= bound, squads.OpLessThan: value < bound, squads.OpLessThanOrEqualTo: value <= bound}[d.Operator] {
			return false
		}
	}
	return true
}
