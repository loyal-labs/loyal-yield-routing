package backyardrwa

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
)

// Test-local bindings only: installed JSON and production activation stay intact.
func candidateV2Catalog(t *testing.T, edges ...string) {
	t.Helper()
	if len(edges) == 0 {
		edges = []string{"USDC->USDe", "USDe->PYUSD"}
	}
	original := catalogJupiterJSON
	t.Cleanup(func() { catalogJupiterJSON = original })
	var entries []catalogJupiterBinding
	if err := json.Unmarshal(original, &entries); err != nil {
		t.Fatal(err)
	}
	for i := range entries {
		b := &entries[i]
		selected := false
		for _, edge := range edges {
			selected = selected || edge == b.From+"->"+b.To
		}
		if !selected {
			continue
		}
		b.DiscriminatorHex = "d19853937cfed8e9"
		b.AmountOffset, b.SlippageOffset, b.FeeOffset = 9, 25, 27
		b.AuthorityIndex, b.SourceIndex, b.DestinationIndex = 1, 2, 5
		b.SourceMintIndex, b.DestinationMintIndex = 6, 7
		b.SourceTokenProgramIndex, b.DestinationTokenProgramIndex = 8, 9
	}
	var err error
	catalogJupiterJSON, err = json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
}

func TestCandidateJupiterV2FixedPrefixAndClient(t *testing.T) {
	var fixture struct {
		Rows []struct {
			Quote        JupiterQuote
			Instruction  JupiterSwapInstruction
			LookupTables []string
		}
	}
	data, err := os.ReadFile("../../../../docs/evidence/backyard-rwa-go/phase3/jupiter-v2-public-quotes-2026-09-04.json")
	if err != nil || json.Unmarshal(data, &fixture) != nil || len(fixture.Rows) != 2 {
		t.Fatal("missing public V2 vectors", err)
	}
	// The actual installed catalog MUST reject V2, including after support lands.
	for i, action := range []Action{SwapStableToCollateralStep, SwapCollateralToDebtStep} {
		r := fixture.Rows[i]
		amount, _ := strconv.ParseUint(r.Quote.InAmount, 10, 64)
		out, minimum, err := validateJupiterQuoteForRoute(r.Quote, action, amount, ethenaUSDePYUSD.Lane)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = validateCatalogJupiterInstruction(r.Instruction, action, amount, out, minimum, ethenaUSDePYUSD.Lane); err == nil {
			t.Fatal("uninstalled V2 accepted")
		}
	}
	candidateV2Catalog(t)
	manifest, err := loadEmbeddedRouteManifest()
	if err != nil {
		t.Fatal(err)
	}
	for i, action := range []Action{SwapStableToCollateralStep, SwapCollateralToDebtStep} {
		r := fixture.Rows[i]
		amount, _ := strconv.ParseUint(r.Quote.InAmount, 10, 64)
		out, minimum, err := validateJupiterQuoteForRoute(r.Quote, action, amount, ethenaUSDePYUSD.Lane)
		if err != nil {
			t.Fatal(err)
		}
		binding, err := manifest.jupiterPolicyForRoute(action, ethenaUSDePYUSD.Lane)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = binding.constraintIndex(r.Instruction); err != nil {
			t.Fatal(err)
		}
		if _, err = validateCatalogJupiterInstruction(r.Instruction, action, amount, out, minimum, ethenaUSDePYUSD.Lane); err != nil {
			t.Fatal(err)
		}
		original, _ := base64.StdEncoding.Strict().DecodeString(r.Instruction.Data)
		for _, offset := range []int{0, 9, 17, 25, 27, 28, 29, 30, 31} {
			bad := r.Instruction
			mutated := append([]byte(nil), original...)
			switch offset {
			case 25:
				binary.LittleEndian.PutUint16(mutated[25:], 51)
			case 31:
				binary.LittleEndian.PutUint32(mutated[31:], 5)
			default:
				mutated[offset] ^= 1
			}
			bad.Data = base64.StdEncoding.EncodeToString(mutated)
			if _, err = validateCatalogJupiterInstruction(bad, action, amount, out, minimum, ethenaUSDePYUSD.Lane); err == nil {
				t.Fatalf("accepted data mutation %d", offset)
			}
		}
		for n := 0; n < 40; n++ {
			bad := r.Instruction
			bad.Data = base64.StdEncoding.EncodeToString(original[:n])
			if _, err = validateCatalogJupiterInstruction(bad, action, amount, out, minimum, ethenaUSDePYUSD.Lane); err == nil {
				t.Fatal("accepted truncated prefix", n)
			}
		}
		for _, index := range []int{1, 2, 5, 6, 7, 8, 9} {
			bad := r.Instruction
			bad.Accounts = append([]JupiterInstructionAccount(nil), r.Instruction.Accounts...)
			bad.Accounts[index].Pubkey = previousBackyardVault
			if _, err = validateCatalogJupiterInstruction(bad, action, amount, out, minimum, ethenaUSDePYUSD.Lane); err == nil {
				t.Fatal("accepted authority/custody mutation", index)
			}
		}
		client, _ := newJupiterClient("https://jupiter.invalid", &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			var payload any
			if req.Method == "GET" {
				if req.URL.Query().Get("instructionVersion") != "V2" {
					t.Fatal("missing explicit V2 quote selection")
				}
				payload = r.Quote
			} else {
				var body struct {
					UseSharedAccounts bool
					UserPublicKey     string
				}
				if json.NewDecoder(req.Body).Decode(&body) != nil || !body.UseSharedAccounts || body.UserPublicKey != bridgeVault {
					t.Fatal("wrong V2 request authority/dialect")
				}
				payload = map[string]any{"swapInstruction": r.Instruction, "addressLookupTableAddresses": r.LookupTables}
			}
			encoded, _ := json.Marshal(payload)
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(encoded))}, nil
		})})
		_, got, err := client.freshSwapForRoute(context.Background(), ethenaUSDePYUSD.Lane, action, amount)
		if err != nil || strings.Join(got.LookupTableAddresses, ",") != strings.Join(r.LookupTables, ",") {
			t.Fatal("V2 client failed", err)
		}
	}
}

func TestPhase3JupiterCandidateMatchesGo(t *testing.T) {
	checkPhase3JupiterCandidateMatchesGo(t, false)
}

func TestPhase3JupiterReturnMatchesGo(t *testing.T) {
	checkPhase3JupiterCandidateMatchesGo(t, true)
}

func TestPhase3LinkedLendingMessagesMatchGo(t *testing.T) {
	dir, name := os.Getenv("PHASE3_JUPITER_RETURN_PROBE_DIR"), os.Getenv("PHASE3_JUPITER_PROBE_RESULT")
	if dir == "" || name == "" {
		t.Skip("explicit linked execution required")
	}
	var plan struct {
		LendingPrelude *struct {
			Schema, Lane              string
			Broadcast, SignatureProof bool
			Steps                     []struct {
				Leg, WireBase64, WireSHA256 string
				Request                     KaminoPrimeUSDCRequest
			}
		}
	}
	data, err := os.ReadFile(dir + "/plan.json")
	if err != nil || json.Unmarshal(data, &plan) != nil {
		t.Fatal("linked plan unavailable", err)
	}
	if plan.LendingPrelude == nil {
		t.Fatal("linked lending plan required")
	}
	var result struct {
		PlanSHA256   string
		LendingSteps []struct{ Leg, WireSHA256 string }
	}
	resultData, err := os.ReadFile(dir + "/" + name)
	if err != nil || json.Unmarshal(resultData, &result) != nil || result.PlanSHA256 != sha256Bytes(data) {
		t.Fatal("linked execution identity mismatch", err)
	}
	p := plan.LendingPrelude
	if p.Schema != "phase3-kamino-controlled-probe/v1" || p.Lane != ethenaUSDePYUSD.Lane || p.Broadcast || p.SignatureProof || len(p.Steps) != 4 || len(result.LendingSteps) != 4 {
		t.Fatal("linked lending scope mismatch")
	}
	for i, step := range p.Steps {
		if step.Leg != []string{"deposit", "borrow", "repay", "withdraw"}[i] || result.LendingSteps[i].Leg != step.Leg || result.LendingSteps[i].WireSHA256 != step.WireSHA256 {
			t.Fatal("executed lending order/wire mismatch")
		}
		message, err := CompileKaminoMessage(step.Request)
		wire, decodeErr := base64.StdEncoding.Strict().DecodeString(step.WireBase64)
		if err != nil || decodeErr != nil || len(wire) <= 65 || len(wire) > 1232 || wire[0] != 1 || !allZero(wire[1:65]) || sha256Bytes(wire) != step.WireSHA256 || !bytes.Equal(message, wire[65:]) {
			t.Fatal("executed lending wire differs from production Go compiler", err, decodeErr)
		}
	}
}

func checkPhase3JupiterCandidateMatchesGo(t *testing.T, returning bool) {
	t.Helper()
	dir := os.Getenv("PHASE3_JUPITER_CANDIDATE_PROBE_DIR")
	schema := "phase3-jupiter-candidate-controlled-result/v1"
	edges := []string{"USDC->USDe", "USDe->PYUSD"}
	actions := []Action{SwapStableToCollateralStep, SwapCollateralToDebtStep}
	if returning {
		dir = os.Getenv("PHASE3_JUPITER_RETURN_PROBE_DIR")
		schema = "phase3-jupiter-return-controlled-result/v1"
		edges = []string{"USDe->USDC"}
		actions = []Action{SwapCollateralToStableStep, SwapDebtToUSDCStep}
	}
	if dir == "" {
		t.Skip("explicit public candidate snapshot required")
	}
	read := func(name string, v any) []byte {
		t.Helper()
		b, e := os.ReadFile(dir + "/" + name)
		if e != nil || json.Unmarshal(b, v) != nil {
			t.Fatal("candidate input unavailable", name, e)
		}
		return b
	}
	var plan struct {
		Compiler, Lane, Profile string
		Broadcast               bool
		Steps                   []struct {
			Action                      Action
			AmountRaw, MinimumOutputRaw uint64
			WireBase64, WireSHA256      string
			HeaderRow                   struct {
				Instruction  JupiterSwapInstruction
				LookupTables []string
				Quote        struct{ OutAmountRaw string }
			}
		}
	}
	planBytes := read("plan.json", &plan)
	var snapshot struct {
		Slot     int64
		Accounts []struct {
			Address, Owner, DataBase64, DataSHA256 string
			Present, Executable                    bool
			Lamports                               uint64
		}
	}
	snapshotBytes := read("snapshot.json", &snapshot)
	var result struct {
		Schema, PlanSHA256, SnapshotSHA256 string
		Broadcast, InstalledPolicyProof    bool
		CandidateCreation                  []struct {
			Policy string
			After  []struct{ Address, DataBase64, DataSHA256 string }
		}
	}
	name := os.Getenv("PHASE3_JUPITER_PROBE_RESULT")
	if name == "" {
		name = "result-negatives.json"
		if returning {
			name = "result-return.json"
		}
	}
	read(name, &result)
	if plan.Compiler != "TYPESCRIPT_CANDIDATE_NOT_INSTALLED_GO" || plan.Broadcast || plan.Lane != ethenaUSDePYUSD.Lane || (plan.Profile == "RETURN_CONVERSIONS") != returning || len(plan.Steps) != 2 || result.Schema != schema || result.Broadcast || result.InstalledPolicyProof || len(result.CandidateCreation) != len(edges) || result.PlanSHA256 != sha256Bytes(planBytes) || result.SnapshotSHA256 != sha256Bytes(snapshotBytes) {
		t.Fatal("candidate scope/identity mismatch")
	}
	candidateV2Catalog(t, edges...)
	var bindings []catalogJupiterBinding
	if json.Unmarshal(catalogJupiterJSON, &bindings) != nil {
		t.Fatal("catalog decode")
	}
	for i, edge := range edges {
		creation := result.CandidateCreation[i]
		hash := ""
		for _, a := range creation.After {
			if a.Address == creation.Policy {
				data, e := base64.StdEncoding.Strict().DecodeString(a.DataBase64)
				if e != nil || sha256Bytes(data) != a.DataSHA256 {
					t.Fatal("candidate policy hash mismatch")
				}
				hash = a.DataSHA256
			}
		}
		if !validSHA256(hash) {
			t.Fatal("candidate creation absent")
		}
		for j := range bindings {
			if bindings[j].From+"->"+bindings[j].To == edge {
				bindings[j].Policy = creation.Policy
				bindings[j].PolicySHA256 = hash
			}
		}
	}
	catalogJupiterJSON, _ = json.Marshal(bindings)
	for i, action := range actions {
		s := plan.Steps[i]
		if s.Action != action {
			t.Fatal("step order drift")
		}
		b, err := catalogJupiterBindingForRoute(action, plan.Lane)
		if err != nil {
			t.Fatal(err)
		}
		if returning && i == 1 {
			found := false
			for _, account := range snapshot.Accounts {
				if account.Address == b.Policy && account.Present && !account.Executable && account.Owner == bridgeSquadsProgram {
					data, err := base64.StdEncoding.Strict().DecodeString(account.DataBase64)
					found = err == nil && sha256Bytes(data) == b.PolicySHA256 && account.DataSHA256 == b.PolicySHA256
				}
			}
			if !found {
				t.Fatal("return legacy policy does not match installed snapshot")
			}
		}
		out, err := strconv.ParseUint(s.HeaderRow.Quote.OutAmountRaw, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		r := JupiterSwapRequest{Action: action, AmountRaw: s.AmountRaw, QuotedOutputRaw: out, MinimumOutputRaw: s.MinimumOutputRaw, Policy: b.Policy, PolicyAccountDataSHA256: b.PolicySHA256, PolicyConstraintIndex: b.ConstraintIndex, Instruction: s.HeaderRow.Instruction, RecentBlockhash: bridgeVault, LastValidBlockHeight: 99, RouteLane: plan.Lane}
		wire, err := base64.StdEncoding.Strict().DecodeString(s.WireBase64)
		if err != nil || len(wire) <= 65 || wire[0] != 1 || !allZero(wire[1:65]) || sha256Bytes(wire) != s.WireSHA256 {
			t.Fatal("unsigned SDK wire drift")
		}
		if acceptsJupiterLookupHints(plan.Lane, action) {
			r.Instruction.LookupTableAddresses = s.HeaderRow.LookupTables
		}
		var lookups []string
		if wire[65]&0x80 != 0 {
			lookups = s.HeaderRow.LookupTables
		}
		for _, address := range lookups {
			found := false
			for _, a := range snapshot.Accounts {
				if a.Address == address && a.Present {
					data, e := base64.StdEncoding.Strict().DecodeString(a.DataBase64)
					if e != nil || sha256Bytes(data) != a.DataSHA256 {
						t.Fatal("lookup hash drift")
					}
					r.LookupTables = append(r.LookupTables, LookupTableSnapshot{Address: address, Owner: a.Owner, Data: data, Lamports: a.Lamports, Executable: a.Executable, ObservedSlot: snapshot.Slot})
					found = true
				}
			}
			if !found {
				t.Fatal("lookup missing")
			}
		}
		message, err := CompileJupiterMessage(r)
		if err != nil || !bytes.Equal(message, wire[65:]) {
			t.Fatal("candidate Go/SDK byte mismatch", i, err, fmt.Sprint(len(message), len(wire)-65))
		}
	}
}
