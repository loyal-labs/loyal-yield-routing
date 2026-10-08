package fleet

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

// Golden values printed by loyal-actions embedded_backyard_voltr_route_bundle
// (route_bundle_sha256, requirements_fingerprint, manager_instruction at
// 12_345_678 raw, manager_intent_sha256).
const voltrGoldenBundle = "f02a29551493737f69c0e8a9e401d52c4ef5e6a660de601cf6a232becf0f2ccf"

var voltrGoldenRequirements = map[string]string{
	"main deposit": "b231373bc122150f3d1ce7732c2a64eff2943f3cef93a7f86ab4e8d0d1aeafe3", "main withdraw": "8cced72e83f8ed175399ff65507c1750e55c67d97390b5c4798c43f53461a6eb",
	"onre deposit": "e2aef8a0d7e08947f1d6329691e984ecf97ff0ee8be620786cb08be3af03d43b", "onre withdraw": "96e5a48b9dba94fbdf5f5473925bd6d01fe806347af9e98ab47d297ac62ce311",
	"prime deposit": "85c676dfc151c0ae70ff12fe56e462622d8c12e47ec90c28585af8fddb180d22", "prime withdraw": "5e187938a7d0d3a7c1e6971805b0272ca8f0601160c80222728178a5eff24d8d",
	"maple deposit": "8c20b9767c9df9b6792319ed33d55b12de79498d66e9a3f6035e1c7fc56a91b4", "maple withdraw": "590951ad8e79979004dfb7dcef4139cb4c987e40be5af0c2bab398a6875a8e99",
}

var voltrGoldenData = map[string]string{
	"deposit":  "5a51bb512746804e01010101010100000000010142000000011d1f000102030405060708090a0b0c0d0e0f10031112130c1415161718191a1b1c1e00f65239e283defdf94e61bc00000000000108000000d435bac193358f7b00",
	"withdraw": "5a51bb512746804e0101010101010000000001013f000000011a1c000102030405060708090a0b0c0d0e0f10051112130d1415161718191e001f2da205c1d986bc4e61bc000000000001080000007b6df50f9630cb7100",
}

func TestVoltrRouteMatchesRustBundle(t *testing.T) {
	r, err := LoadVoltrRoute()
	if err != nil {
		t.Fatal(err)
	}
	if r.BundleSHA256 != voltrGoldenBundle {
		t.Fatalf("bundle %s", r.BundleSHA256)
	}
	for i, id := range voltrStrategyIDs {
		for _, op := range []string{"deposit", "withdraw"} {
			req, err := r.RequirementsFingerprint(i, op)
			if err != nil || req != voltrGoldenRequirements[id+" "+op] {
				t.Fatalf("%s %s requirements %s %v", id, op, req, err)
			}
			ix, err := r.ManagerInstruction(i, op, 12_345_678)
			if err != nil || hex.EncodeToString(ix.Data) != voltrGoldenData[op] {
				t.Fatalf("%s %s manager data %x %v", id, op, ix.Data, err)
			}
		}
	}
	if got := r.IntentSHA256(2, "withdraw", 5_000_000, 444, "r", "s", "a"); got != "fdb6b563d004c58fa68ff0b48358e2581efdc10b16d1b30ff8834b0c3e24e738" {
		t.Fatalf("intent %s", got)
	}
	if _, err := r.ManagerInstruction(0, "deposit", r.MaxOperationRaw+1); err == nil {
		t.Fatal("over-cap manager amount was accepted")
	}
}

func TestVoltrRouteIsThePinnedCatalog(t *testing.T) {
	// The embedded subset must be the catalog Rust pins by file hash, and the
	// wrapper Go builds must be the SDK-generated canonical instruction.
	raw, err := os.ReadFile("../../../../docs/evidence/backyard-voltr-four-market/runtime-policy-catalog-v2.json")
	if err != nil {
		t.Skip("catalog is outside this checkout")
	}
	if sum := sha256.Sum256(raw); hex.EncodeToString(sum[:]) != voltrCatalogFileSHA256 {
		t.Fatal("catalog drifted from the Rust pin")
	}
	var catalog struct {
		Policies []struct {
			StrategyID, Operation, Policy string
			ManagerExecution              struct {
				ProgramID  string `json:"programId"`
				DataBase64 string `json:"dataBase64"`
				Accounts   []InstructionAccount
			} `json:"managerExecution"`
		} `json:"policies"`
	}
	if err := json.Unmarshal(raw, &catalog); err != nil {
		t.Fatal(err)
	}
	r, err := LoadVoltrRoute()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range catalog.Policies {
		i, _ := r.StrategyIndex(p.StrategyID)
		canonical, _ := base64.StdEncoding.DecodeString(p.ManagerExecution.DataBase64)
		inner, _ := r.instruction(i, p.Operation)
		data, _ := base64.StdEncoding.DecodeString(inner.Data)
		amount := uint64(0)
		for b := 15; b >= 8; b-- {
			amount = amount<<8 | uint64(data[b])
		}
		ix, err := r.ManagerInstruction(i, p.Operation, amount)
		if err != nil || inner.Policy != p.Policy || ix.Program != p.ManagerExecution.ProgramID || hex.EncodeToString(ix.Data) != hex.EncodeToString(canonical) || !reflect.DeepEqual(ix.Accounts, p.ManagerExecution.Accounts) {
			t.Fatalf("%s %s wrapper differs from the canonical SDK instruction: %v", p.StrategyID, p.Operation, err)
		}
	}
}

func TestVoltrControllerMatchesRustOneLegOracle(t *testing.T) {
	// backyard_voltr_one_leg_oracle: restoration withdraws one capped leg per
	// confirmed observation, cheapest yield first, and stops when receipts are
	// covered; it never pre-plans a sibling leg.
	positions := []voltrPosition{{0, 80e9, 80e9, 80e9, 200}, {1, 0, 0, 0, 400}, {2, 80e9, 80e9, 80e9, 300}, {3, 0, 0, 0, 500}}
	idle, demand := uint64(0), uint64(120e9)
	got := []voltrLeg{}
	for {
		leg, err := nextVoltrLeg(idle, demand, demand, 50e9, positions, true)
		if err != nil {
			t.Fatal(err)
		}
		if leg == nil {
			break
		}
		got = append(got, *leg)
		idle += leg.amount
		p := &positions[leg.strategy]
		p.value, p.redeemable = p.value-leg.amount, p.redeemable-leg.amount
	}
	want := []voltrLeg{{"withdrawal_restoration", "withdraw", 0, 50e9}, {"withdrawal_restoration", "withdraw", 2, 50e9}, {"withdrawal_restoration", "withdraw", 0, 20e9}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("legs %+v", got)
	}
}
