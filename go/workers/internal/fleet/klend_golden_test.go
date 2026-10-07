package fleet

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// testdata/klend/golden.json is the byte-parity record of the retired Rust
// loyal-klend-proxy (crates/loyal-yield-orchestrator/src/bin/loyal-klend-proxy.rs
// at c22e1094, klend-interface 23b9f2b). Regenerate it only from that binary:
//
//	KLEND_GOLDEN_PROXY=/path/to/loyal-klend-proxy go test ./internal/fleet -run TestKLendGolden
const klendGoldenPath = "../../testdata/klend/golden.json"

type klendGoldenCase struct {
	Name      string          `json:"name"`
	Operation string          `json:"operation"`
	Request   json.RawMessage `json:"request"`
	Output    json.RawMessage `json:"output,omitempty"`
	Error     string          `json:"error,omitempty"`
}

type klendGoldenInstruction struct {
	Step     string               `json:"step"`
	Program  string               `json:"program"`
	Accounts []InstructionAccount `json:"accounts"`
	DataHex  string               `json:"dataHex"`
}

type klendGoldenOutput struct {
	SchemaVersion int    `json:"schemaVersion"`
	Operation     string `json:"operation"`
	Route         struct {
		Public    []klendGoldenInstruction `json:"public"`
		Protected []klendGoldenInstruction `json:"protected"`
	} `json:"route"`
}

func klendGoldenRoute(operation string, route KaminoSameMintRoute) klendGoldenOutput {
	encode := func(list []RouteInstruction) []klendGoldenInstruction {
		out := []klendGoldenInstruction{}
		for _, ix := range list {
			out = append(out, klendGoldenInstruction{ix.Step, ix.Program, ix.Accounts, hex.EncodeToString(ix.Data)})
		}
		return out
	}
	var out klendGoldenOutput
	out.SchemaVersion, out.Operation = 1, operation
	out.Route.Public, out.Route.Protected = encode(route.Public), encode(route.Protected)
	return out
}

// The same-mint route is recorded from the retained worker itself in
// same_mint_golden_test.go; this proxy record keeps the other builders.
//
// klendGoldenRequests spans every builder stage, footprint shape, optional
// account, stable mint/token program and each input the Rust proxy refused.
func klendGoldenRequests(t *testing.T) []klendGoldenCase {
	t.Helper()
	base := routeFixture(t)
	cases := []klendGoldenCase{}
	add := func(name, operation string, request any) {
		raw, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		cases = append(cases, klendGoldenCase{Name: name, Operation: operation, Request: raw})
	}
	ata := func(vault, mint string) string {
		program, ok := stableTokenProgram(mint)
		if !ok {
			t.Fatalf("unsupported mint %s", mint)
		}
		address, err := deriveATA(vault, mint, program)
		if err != nil {
			t.Fatal(err)
		}
		return address
	}
	// Any valid keys serve as farm states: the builders derive, never read.
	targetFarm := base.Target.LiquiditySupply
	withFarm := func(p KaminoPositionAccounts, farmState string) KaminoPositionAccounts {
		p.ReserveFarmState = farmState
		return p
	}
	sameMint := base
	add("same-mint rejected as cross-mint", "buildCrossMintLegs", sameMint)
	for _, mint := range earnStableMints {
		if mint == base.Source.LiquidityMint {
			continue
		}
		cross := base
		cross.Target.LiquidityMint = mint
		cross.Target.LiquidityTokenProgram, _ = stableTokenProgram(mint)
		cross.Target.VaultLiquidityATA = ata(base.Vault, mint)
		add("cross-mint to "+mint, "buildCrossMintLegs", cross)
	}
	crossFarm := base
	crossFarm.Target.LiquidityMint = USDTMint
	crossFarm.Target.VaultLiquidityATA = ata(base.Vault, USDTMint)
	crossFarm.Target = withFarm(crossFarm.Target, targetFarm)
	add("cross-mint farmed target", "buildCrossMintLegs", crossFarm)

	// Idle deposits, through a USDC vault whose ATA the builder derives.
	usdcTarget := base.Target
	usdcTarget.LiquidityMint, usdcTarget.LiquidityTokenProgram = USDCMint, tokenProgram
	usdcTarget.VaultLiquidityATA = ata(base.Vault, USDCMint)
	boundTarget := usdcTarget
	if err := bindKLendPDAsForTest(&boundTarget, base.Vault); err != nil {
		t.Fatal(err)
	}
	idle := KaminoIdleDepositRequest{Vault: base.Vault, Target: boundTarget, DepositLiquidityAmount: 1_000_000}
	add("idle target-only footprint", "buildIdleDeposit", idle)
	idleEmpty := idle
	idleEmpty.Target.ObligationDepositReserves = []string{}
	add("idle empty footprint", "buildIdleDeposit", idleEmpty)
	idleFarm := idle
	idleFarm.Target = withFarm(idle.Target, targetFarm)
	if err := bindKLendPDAsForTest(&idleFarm.Target, base.Vault); err != nil {
		t.Fatal(err)
	}
	add("idle farmed target", "buildIdleDeposit", idleFarm)
	idleForeign := idle
	idleForeign.Target.Obligation = base.Source.Reserve
	add("idle foreign obligation", "buildIdleDeposit", idleForeign)
	idleZero := idle
	idleZero.DepositLiquidityAmount = 0
	add("idle zero amount", "buildIdleDeposit", idleZero)
	idleBorrow := idle
	idleBorrow.Target.ObligationBorrowReserves = []string{base.Source.Reserve}
	add("idle borrow footprint", "buildIdleDeposit", idleBorrow)
	idleOther := idle
	idleOther.Target.ObligationDepositReserves = []string{base.Source.Reserve}
	add("idle other reserve footprint", "buildIdleDeposit", idleOther)
	for _, mint := range earnStableMints {
		stable := idle
		stable.Target.LiquidityMint = mint
		stable.Target.LiquidityTokenProgram, _ = stableTokenProgram(mint)
		stable.Target.VaultLiquidityATA = ata(base.Vault, mint)
		add("idle "+mint, "buildIdleDeposit", stable)
	}

	// Destination setup stages, PDAs derived from empty fields.
	setupTarget := usdcTarget
	setupTarget.ObligationDepositReserves, setupTarget.ObligationBorrowReserves = []string{}, []string{}
	payer := base.Source.Reserve
	for _, stage := range []string{"ata", "metadata", "obligation"} {
		add("setup "+stage, "buildDestinationSetup", DestinationSetupRequest{Stage: stage, Vault: base.Vault, Payer: payer, Target: setupTarget})
	}
	add("setup farm", "buildDestinationSetup", DestinationSetupRequest{Stage: "farm", Vault: base.Vault, Payer: payer, Target: withFarm(setupTarget, targetFarm)})
	add("setup farm without reserve farm", "buildDestinationSetup", DestinationSetupRequest{Stage: "farm", Vault: base.Vault, Payer: payer, Target: setupTarget})
	foreignATA := setupTarget
	foreignATA.VaultLiquidityATA = base.Source.VaultLiquidityATA
	add("setup ata foreign custody", "buildDestinationSetup", DestinationSetupRequest{Stage: "ata", Vault: base.Vault, Payer: payer, Target: foreignATA})
	add("setup unknown stage", "buildDestinationSetup", DestinationSetupRequest{Stage: "policy", Vault: base.Vault, Payer: payer, Target: setupTarget})
	cases = append(cases, canonicalPolicyGoldenRequests(t)...)
	return cases
}

func routeFixture(t *testing.T) KaminoSameMintRouteRequest {
	t.Helper()
	raw, err := os.ReadFile("../../../../docs/verifiers/kamino-fleet-parity/kamino-route-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var request KaminoSameMintRouteRequest
	if err := json.Unmarshal(raw, &request); err != nil {
		t.Fatal(err)
	}
	return request
}

func bindKLendPDAsForTest(p *KaminoPositionAccounts, vault string) error {
	key, err := decodePublicKey(vault)
	if err != nil {
		return err
	}
	return bindKLendPDAs(p, key)
}

func buildKLendGolden(operation string, raw json.RawMessage) (KaminoSameMintRoute, error) {
	decode := func(out any) error {
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		return decoder.Decode(out)
	}
	switch operation {
	case "buildCrossMintLegs":
		var r KaminoSameMintRouteRequest
		if err := decode(&r); err != nil {
			return KaminoSameMintRoute{}, err
		}
		return BuildCrossMintLegs(r)
	case "buildIdleDeposit":
		var r KaminoIdleDepositRequest
		if err := decode(&r); err != nil {
			return KaminoSameMintRoute{}, err
		}
		return BuildIdleDeposit(r)
	case "buildDestinationSetup":
		var r DestinationSetupRequest
		if err := decode(&r); err != nil {
			return KaminoSameMintRoute{}, err
		}
		return BuildDestinationSetup(r)
	}
	panic("unknown golden operation " + operation)
}

func TestKLendGoldenParityWithRustProxy(t *testing.T) {
	if proxy := os.Getenv("KLEND_GOLDEN_PROXY"); proxy != "" {
		writeKLendGolden(t, proxy, klendGoldenRequests(t))
	}
	raw, err := os.ReadFile(klendGoldenPath)
	if err != nil {
		t.Fatal(err)
	}
	var cases []klendGoldenCase
	if err = json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) < 30 {
		t.Fatalf("golden record has only %d cases", len(cases))
	}
	for _, c := range cases {
		if c.Operation == "buildCanonicalSubscriptionPolicy" {
			continue // autodeposit owns this builder; see its golden test.
		}
		t.Run(c.Name, func(t *testing.T) {
			route, err := buildKLendGolden(c.Operation, c.Request)
			if c.Error != "" {
				// Idle preconditions are checked before the official builder
				// and may refuse earlier, never later, than Rust.
				if err == nil {
					t.Fatalf("Rust refused (%s); Go built %+v", c.Error, route)
				}
				return
			}
			if err != nil {
				t.Fatalf("Rust built the route; Go refused: %v", err)
			}
			got, err := json.Marshal(klendGoldenRoute(c.Operation, route))
			if err != nil {
				t.Fatal(err)
			}
			var want bytes.Buffer
			if err = json.Compact(&want, c.Output); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want.Bytes()) {
				t.Fatalf("Go wire differs from Rust proxy\n go: %s\nrust: %s", got, want.Bytes())
			}
		})
	}
}

// writeKLendGolden runs every request through the Rust proxy binary and
// records its exact stdout, or that it refused.
func writeKLendGolden(t *testing.T, proxy string, cases []klendGoldenCase) {
	t.Helper()
	for i := range cases {
		cases[i].Output, cases[i].Error = runKLendProxy(t, proxy, cases[i].Operation, cases[i].Request)
	}
	out, err := json.MarshalIndent(cases, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(klendGoldenPath, append(out, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

func runKLendProxy(t *testing.T, proxy, operation string, request json.RawMessage) (json.RawMessage, string) {
	t.Helper()
	input, err := json.Marshal(map[string]any{"schemaVersion": 1, "operation": operation, "request": request})
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(proxy)
	command.Stdin = bytes.NewReader(input)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		return nil, message
	}
	return json.RawMessage(bytes.TrimSpace(stdout.Bytes())), ""
}

// canonicalPolicyGoldenRequests records the Squads policy creator and the
// current-account proof from the independent Solita SDK fixture.
func canonicalPolicyGoldenRequests(t *testing.T) []klendGoldenCase {
	t.Helper()
	raw, err := os.ReadFile("../../testdata/autodeposit/canonical-policy-creator.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Settings, RootAuthority, Payer, DelegatedSigner, Wallet, Vault string
		PolicySeed, MaxAmountPerPeriod                                 uint64
		PolicyDataHex, WeakenedPolicyDataHex                           string
	}
	if err = json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	type request struct {
		Settings           string `json:"settings"`
		RootAuthority      string `json:"rootAuthority"`
		Payer              string `json:"payer"`
		DelegatedSigner    string `json:"delegatedSigner"`
		PolicySeed         uint64 `json:"policySeed"`
		Wallet             string `json:"wallet"`
		Vault              string `json:"vault"`
		MaxAmountPerPeriod uint64 `json:"maxAmountPerPeriod"`
		PolicyDataHex      string `json:"policyDataHex"`
	}
	base := request{f.Settings, f.RootAuthority, f.Payer, f.DelegatedSigner, f.PolicySeed, f.Wallet, f.Vault, f.MaxAmountPerPeriod, ""}
	var cases []klendGoldenCase
	add := func(name string, r request) {
		raw, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		cases = append(cases, klendGoldenCase{Name: name, Operation: "buildCanonicalSubscriptionPolicy", Request: raw})
	}
	add("policy creator", base)
	other := base
	other.Payer = f.DelegatedSigner
	add("policy creator separate payer", other)
	current := base
	current.PolicyDataHex = f.PolicyDataHex
	add("policy current account", current)
	weakened := base
	weakened.PolicyDataHex = f.WeakenedPolicyDataHex
	add("policy weakened account", weakened)
	data, err := hex.DecodeString(f.PolicyDataHex)
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func([]byte){
		"threshold":          func(d []byte) { d[102] = 2 },
		"signer permissions": func(d []byte) { d[101] = 3 },
		"signer":             func(d []byte) { d[69] ^= 1 },
		"seed":               func(d []byte) { d[40] ^= 1 },
		"payload tail byte":  func(d []byte) { d[len(d)-40] ^= 1 },
	} {
		mutated := append([]byte(nil), data...)
		mutate(mutated)
		r := base
		r.PolicyDataHex = hex.EncodeToString(mutated)
		add("policy mutated "+name, r)
	}
	zeroSeed := base
	zeroSeed.PolicySeed = 0
	add("policy zero seed", zeroSeed)
	zeroMax := base
	zeroMax.MaxAmountPerPeriod = 0
	add("policy zero budget", zeroMax)
	foreignVault := base
	foreignVault.Vault = f.Wallet
	add("policy vault not index 1", foreignVault)
	return cases
}
