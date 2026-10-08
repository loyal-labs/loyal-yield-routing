package fleet

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
)

// testdata/klend/same-mint-route.golden.json is the recorded output of the
// retired Rust fleet worker's build_route_execution_plan for every same-mint
// route shape: mature, farm-user setup and in-route target obligation setup
// under the route or setup policy. The generator was deleted with the Rust
// workers; the record is fixed.
const sameMintGoldenPath = "../../testdata/klend/same-mint-route.golden.json"

type sameMintGoldenPosition struct {
	KaminoPositionAccounts
	ObligationExists bool   `json:"obligationExists"`
	FarmUserExists   bool   `json:"farmUserExists"`
	AmountRaw        uint64 `json:"amountRaw"`
	RedeemableRaw    uint64 `json:"redeemableAmountRaw"`
}

type sameMintGoldenRequest struct {
	PolicyKeypair   string                 `json:"policyKeypair"`
	Vault           string                 `json:"vault"`
	Settings        string                 `json:"settings"`
	VaultIndex      uint8                  `json:"vaultIndex"`
	RoutePolicy     string                 `json:"routePolicy"`
	RoutePolicyData []byte                 `json:"routePolicyData"`
	SetupPolicy     string                 `json:"setupPolicy,omitempty"`
	SetupPolicyData []byte                 `json:"setupPolicyData,omitempty"`
	FeePayer        string                 `json:"feePayer"`
	Source          sameMintGoldenPosition `json:"source"`
	Target          sameMintGoldenPosition `json:"target"`
	AmountRaw       uint64                 `json:"amountRaw"`
	RentLamports    uint64                 `json:"rentLamports"`
	VaultLamports   uint64                 `json:"vaultLamports"`
	PayerLamports   uint64                 `json:"payerLamports"`
}

type sameMintGoldenCase struct {
	Name    string                `json:"name"`
	Request sameMintGoldenRequest `json:"request"`
	Output  *struct {
		Instructions []struct {
			Program  string               `json:"program"`
			Accounts []InstructionAccount `json:"accounts"`
			Data     []byte               `json:"data"`
		} `json:"instructions"`
		RouteSteps []string `json:"routeSteps"`
	} `json:"output,omitempty"`
	Error string `json:"error,omitempty"`
}

func putPolicySeed(data []byte, seed uint64, bump uint8) {
	for i := 0; i < 8; i++ {
		data[40+i] = byte(seed >> (8 * i))
	}
	data[48] = bump
}

func mustKey(t *testing.T, address string) []byte {
	t.Helper()
	key, err := decodePublicKey(address)
	if err != nil {
		t.Fatal(err)
	}
	return key[:]
}

func TestSameMintRouteGoldenParityWithRustWorker(t *testing.T) {
	raw, err := os.ReadFile(sameMintGoldenPath)
	if err != nil {
		t.Fatal(err)
	}
	var cases []sameMintGoldenCase
	if err = json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) < 10 {
		t.Fatalf("golden record has only %d cases", len(cases))
	}
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			got, err := goldenSameMintBody(c.Request)
			if c.Error != "" {
				if err == nil {
					t.Fatalf("Rust refused (%s); Go built %d instructions", c.Error, len(got))
				}
				return
			}
			if err != nil {
				t.Fatalf("Rust built the route; Go refused: %v", err)
			}
			want := c.Output.Instructions
			if len(got) != len(want) {
				t.Fatalf("Go built %d instructions, Rust %d (%v)", len(got), len(want), c.Output.RouteSteps)
			}
			for i := range want {
				if got[i].Program != want[i].Program || !bytes.Equal(got[i].Data, want[i].Data) || len(got[i].Accounts) != len(want[i].Accounts) {
					t.Fatalf("instruction %d differs\n go: %s %x %v\nrust: %s %x %v", i, got[i].Program, got[i].Data, got[i].Accounts, want[i].Program, want[i].Data, want[i].Accounts)
				}
				for j := range want[i].Accounts {
					if got[i].Accounts[j] != want[i].Accounts[j] {
						t.Fatalf("instruction %d account %d: go %+v rust %+v", i, j, got[i].Accounts[j], want[i].Accounts[j])
					}
				}
			}
		})
	}
}

// goldenSameMintBody runs one golden request through the production path:
// rent top-up, builder and policy wrapping.
func goldenSameMintBody(r sameMintGoldenRequest) ([]RouteInstruction, error) {
	key, err := decodeBase58(r.PolicyKeypair)
	if err != nil {
		return nil, err
	}
	signer := encodeBase58(key[32:])
	var topUp uint64
	if !r.Target.ObligationExists {
		if topUp, err = vaultRentTopUp(r.RentLamports, r.VaultLamports); err != nil {
			return nil, err
		}
	}
	route, err := BuildSameMintRoute(KaminoSameMintRouteRequest{Vault: r.Vault, Source: r.Source.KaminoPositionAccounts, Target: r.Target.KaminoPositionAccounts,
		WithdrawCollateralAmount: r.Source.AmountRaw, DepositLiquidityAmount: r.AmountRaw, TargetObligationMissing: !r.Target.ObligationExists,
		SourceFarmUserMissing: r.Source.ReserveFarmState != "" && !r.Source.FarmUserExists, TargetFarmUserMissing: r.Target.ReserveFarmState != "" && !r.Target.FarmUserExists,
		Payer: r.FeePayer, VaultRentTopUpLamports: topUp})
	if err != nil {
		return nil, err
	}
	body, _, err := wrapSameMintRoute(route, signer, r.VaultIndex, r.RoutePolicy, r.RoutePolicyData, r.SetupPolicy, r.SetupPolicyData)
	return body, err
}
