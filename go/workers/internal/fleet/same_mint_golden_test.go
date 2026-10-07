package fleet

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"os"
	"os/exec"
	"testing"
)

// testdata/klend/same-mint-route.golden.json records the retained Rust fleet
// worker's build_route_execution_plan output (crates/loyal-fleet-worker
// klend_golden) for every same-mint route shape: mature, farm-user setup and
// in-route target obligation setup under the route or setup policy. Regenerate
// it only from that code:
//
//	cd testdata/klend/golden-generator && cp ../../../../../Cargo.lock . && cargo build --offline
//	KLEND_ROUTE_GOLDEN_GENERATOR=$PWD/target/debug/klend-golden-generator go test ./internal/fleet -run TestSameMintRouteGolden
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

func sameMintGoldenRequests(t *testing.T) []sameMintGoldenCase {
	t.Helper()
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, 32))
	signer := encodeBase58(key.Public().(ed25519.PublicKey))
	base := routeFixture(t)
	settings := testIdentity(12)
	vault := base.Vault
	sourceFarm, targetFarm := base.Source.LiquiditySupply, base.Target.LiquiditySupply
	bind := func(p KaminoPositionAccounts, farm string) KaminoPositionAccounts {
		p.ReserveFarmState, p.ObligationFarmUserState = farm, ""
		ata, err := deriveATA(vault, p.LiquidityMint, p.LiquidityTokenProgram)
		if err != nil {
			t.Fatal(err)
		}
		p.VaultLiquidityATA = ata
		if err := bindKLendPDAsForTest(&p, vault); err != nil {
			t.Fatal(err)
		}
		return p
	}
	position := func(p KaminoPositionAccounts, exists, farmUser bool, amount uint64) sameMintGoldenPosition {
		if !exists {
			p.ObligationDepositReserves, p.ObligationBorrowReserves = []string{}, []string{}
		}
		redeemable := uint64(0)
		if amount > 0 {
			redeemable = 120_000_000
		}
		return sameMintGoldenPosition{p, exists, farmUser, amount, redeemable}
	}
	// Policies are the deployed Squads account layout; constraints pin every
	// instruction account, with both markets accepted wherever a market is.
	policyData := func(seed uint64, positions []KaminoPositionAccounts, instructions ...RouteInstruction) (string, []byte) {
		data, err := connectedEarnPolicyDataForIndex(settings, signer, 1, instructions, positions)
		if err != nil {
			t.Fatal(err)
		}
		_, address := connectedPolicyHeader(t, settings, signer, seed)
		_, bump, _ := derivePolicyAccount(settings, seed)
		putPolicySeed(data, seed, bump)
		return address, append(data, make([]byte, 2+4+8+1+32)...)
	}
	const amount, collateral = 120_000_000, 123_456_789
	var cases []sameMintGoldenCase
	add := func(name string, source, target sameMintGoldenPosition, withInit, setupPolicy bool, vaultLamports uint64) {
		positions := []KaminoPositionAccounts{source.KaminoPositionAccounts, target.KaminoPositionAccounts}
		withdraw := withdrawV2(vault, source.KaminoPositionAccounts, collateral)
		deposit := depositV2(vault, target.KaminoPositionAccounts, amount)
		metadata, err := findProgramAddress(KLendProgram, []byte("user_meta"), mustKey(t, vault))
		if err != nil {
			t.Fatal(err)
		}
		init := initObligation(vault, target.KaminoPositionAccounts, metadata)
		r := sameMintGoldenRequest{PolicyKeypair: encodeBase58(key), Vault: vault, Settings: settings, VaultIndex: 1, FeePayer: signer, Source: source, Target: target, AmountRaw: amount, RentLamports: 23_942_400, VaultLamports: vaultLamports, PayerLamports: 1_000_000_000}
		route := []RouteInstruction{withdraw, deposit}
		if withInit && !setupPolicy {
			route = append(route, init)
		}
		r.RoutePolicy, r.RoutePolicyData = policyData(1, positions, route...)
		if setupPolicy {
			r.SetupPolicy, r.SetupPolicyData = policyData(2, positions, init)
		}
		cases = append(cases, sameMintGoldenCase{Name: name, Request: r})
	}
	plain := func(p KaminoPositionAccounts) KaminoPositionAccounts { return bind(p, "") }
	add("mature", position(plain(base.Source), true, false, collateral), position(plain(base.Target), true, false, 0), false, false, 0)
	add("mature with ready farms", position(bind(base.Source, sourceFarm), true, true, collateral), position(bind(base.Target, targetFarm), true, true, 0), false, false, 0)
	add("farm users missing on both sides", position(bind(base.Source, sourceFarm), true, false, collateral), position(bind(base.Target, targetFarm), true, false, 0), false, false, 0)
	add("target obligation missing, route policy init, rent top-up", position(plain(base.Source), true, false, collateral), position(plain(base.Target), false, false, 0), true, false, 1_000_000)
	add("target obligation missing, setup policy init, funded vault", position(plain(base.Source), true, false, collateral), position(plain(base.Target), false, false, 0), true, true, 30_000_000)
	add("target obligation and farm user missing, setup policy, unfunded vault", position(bind(base.Source, sourceFarm), true, true, collateral), position(bind(base.Target, targetFarm), false, false, 0), true, true, 0)
	oracles := plain(base.Source)
	oracles.SwitchboardPriceOracle, oracles.SwitchboardTWAPOracle = base.Target.PythOracle, base.Target.ScopePrices
	emptyTarget := plain(base.Target)
	emptyTarget.ObligationDepositReserves = []string{}
	add("every oracle, empty target footprint", position(oracles, true, false, collateral), position(emptyTarget, true, false, 0), false, false, 0)
	add("source farm user missing", position(bind(base.Source, sourceFarm), true, false, collateral), position(bind(base.Target, targetFarm), true, true, 0), false, false, 0)
	add("target obligation missing without init authority", position(plain(base.Source), true, false, collateral), position(plain(base.Target), false, false, 0), false, false, 0)
	capped := position(plain(base.Target), false, false, 0)
	add("target obligation missing, rent above cap", position(plain(base.Source), true, false, collateral), capped, true, false, 0)
	cases[len(cases)-1].Request.RentLamports = 25_000_001
	return cases
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
	if generator := os.Getenv("KLEND_ROUTE_GOLDEN_GENERATOR"); generator != "" {
		input, err := json.Marshal(sameMintGoldenRequests(t))
		if err != nil {
			t.Fatal(err)
		}
		command := exec.Command(generator)
		command.Stdin = bytes.NewReader(input)
		var stderr bytes.Buffer
		command.Stderr = &stderr
		output, err := command.Output()
		if err != nil {
			t.Fatalf("generator: %v\n%s", err, stderr.String())
		}
		if err = os.WriteFile(sameMintGoldenPath, output, 0o644); err != nil {
			t.Fatal(err)
		}
	}
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
