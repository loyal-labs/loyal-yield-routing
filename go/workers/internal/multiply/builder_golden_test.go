package multiply

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"

	"github.com/gagliardetto/solana-go"
)

func TestKLendRecipesMatchOfficialSDKGolden(t *testing.T) {
	raw, err := os.ReadFile("testdata/rust-klend-recipes.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Provenance struct {
			GeneratorSHA256 string `json:"generatorSha256"`
			SDKRevision     string `json:"sdkRevision"`
			CargoLockSHA256 string `json:"cargoLockSha256"`
		} `json:"provenance"`
		Cases []struct {
			Input struct {
				Vault  solana.PublicKey
				Amount uint64
				Config StrategyConfig
			}
			Action                         string
			IncludeCollateral, IncludeDebt bool
			Instructions                   []goldenInstruction
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	source, err := os.ReadFile("../../../../crates/squads-test-harness/tests/workers_v2_multiply/abi.rs")
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(source)
	if fixture.Provenance.GeneratorSHA256 != hex.EncodeToString(digest[:]) || fixture.Provenance.SDKRevision != "23b9f2b54530784ef1d9d0e08c5256e1eafe4a04" || len(fixture.Cases) != 44 {
		t.Fatal("official SDK fixture provenance drifted")
	}
	lock, err := os.ReadFile("../../../../Cargo.lock")
	if err != nil {
		t.Fatal(err)
	}
	lockHash := sha256.Sum256(lock)
	if fixture.Provenance.CargoLockSHA256 != hex.EncodeToString(lockHash[:]) {
		t.Fatal("official SDK lock provenance drifted")
	}
	for i, tc := range fixture.Cases {
		var actual []Instruction
		switch tc.Action {
		case "deposit":
			actual, err = depositInstructions(tc.Input.Config, tc.Input.Vault, tc.Input.Amount, tc.IncludeCollateral, tc.IncludeDebt)
		case "borrow":
			actual, err = borrowInstructions(tc.Input.Config, tc.Input.Vault, tc.Input.Amount, tc.IncludeCollateral, tc.IncludeDebt)
		case "withdraw":
			actual, err = withdrawInstructions(tc.Input.Config, tc.Input.Vault, tc.Input.Amount, tc.IncludeDebt)
		case "repay":
			actual, err = repayInstructions(tc.Input.Config, tc.Input.Vault, tc.Input.Amount)
		default:
			t.Fatalf("unknown Rust fixture action %q", tc.Action)
		}
		if err != nil {
			t.Fatalf("case%d %s: %v", i, tc.Action, err)
		}
		if len(actual) != len(tc.Instructions) {
			t.Fatalf("case%d %s instruction count got%d SDK%d", i, tc.Action, len(actual), len(tc.Instructions))
		}
		for j, expected := range tc.Instructions {
			a, _ := json.Marshal(actual[j])
			e, _ := json.Marshal(expected.instruction(t))
			if string(a) != string(e) {
				t.Fatalf("case%d %s instruction%d differs from official SDK:\nGo %s\nSDK %s", i, tc.Action, j, a, e)
			}
		}
	}
}
