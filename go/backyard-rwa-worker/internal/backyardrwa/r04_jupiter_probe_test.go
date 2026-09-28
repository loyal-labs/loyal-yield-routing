package backyardrwa

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"regexp"
	"sort"
	"testing"
	"time"
)

type jupiterProbeTransport struct {
	lastInstruction JupiterSwapInstruction
}

func (p *jupiterProbeTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	response, err := http.DefaultTransport.RoundTrip(r)
	if err != nil || r.URL.Path != "/swap/v1/swap-instructions" {
		return response, err
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, jupiterResponseBytes+1))
	response.Body.Close()
	if err != nil {
		return nil, err
	}
	response.Body = io.NopCloser(bytes.NewReader(data))
	var body struct {
		SwapInstruction JupiterSwapInstruction `json:"swapInstruction"`
	}
	if json.Unmarshal(data, &body) == nil {
		p.lastInstruction = body.SwapInstruction
	}
	return response, nil
}

// Fetch real quotes with production construction, but export only zero-signature
// wires with an artificial blockhash for a local deployed-program experiment.
func TestExportPhase3JupiterControlledProbe(t *testing.T) {
	path := os.Getenv("PHASE3_JUPITER_PROBE_PLAN")
	if path == "" {
		t.Skip("explicit local probe output required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	rpc, err := NewRPCClient(os.Getenv("SOLANA_RPC_URL"))
	if err != nil {
		t.Fatal("probe RPC unavailable")
	}
	manifest, err := loadEmbeddedRouteManifest()
	if err != nil {
		t.Fatal(err)
	}
	slot, err := rpc.ConfirmedSlot(ctx)
	if err != nil {
		t.Fatal("probe slot unavailable")
	}
	addresses := map[string]bool{budgetClockAddress: true}
	policies := map[string]string{}
	rows := []any{}
	amount := uint64(900_000) // 0.9 USDC, controlled local input only, not admission proof.
	for _, action := range []Action{SwapStableToCollateralStep, SwapCollateralToDebtStep} {
		d := Decision{Action: action, AmountRaw: int64(amount), StrategyKey: ethenaUSDePYUSD.Lane}
		transport := &jupiterProbeTransport{}
		client := productionJupiterClient()
		client.http.Transport = transport
		e, err := prepareJupiterQuoteEvidence(ctx, rpc, client, manifest, d, amount, 0, slot)
		if err != nil {
			binding, _ := catalogJupiterBindingForRoute(action, ethenaUSDePYUSD.Lane)
			data, _ := base64.StdEncoding.DecodeString(transport.lastInstruction.Data)
			t.Logf("public layout diagnostic: bytes=%d expected=%d instructionData=%x binding=%+v", len(data), binding.FeeOffset+1, data, binding)
			// RPC transport errors can contain endpoint credentials. Keep only
			// local Jupiter validation messages; never print the wrapped error.
			boundary := regexp.MustCompile(`unsigned message does not fit the single-signer packet envelope|fresh Jupiter header does not match the manifest binding|HOLD: [a-z_]+|Jupiter (?:instruction does not match installed edge economics or layout|catalog account boundary [0-9]+ drifted|route requires unapproved companion instructions|message requires unsupported construction|packet is [0-9]+ bytes, exceeds [0-9]+|returned invalid HTTP [0-9]+ response)`).FindString(err.Error())
			if boundary == "" {
				boundary = "unclassified construction failure"
			}
			diagnostic, _ := json.MarshalIndent(map[string]any{"schema": "phase3-jupiter-construction-failure/v1", "broadcast": false, "lane": d.StrategyKey, "action": action, "amountRaw": amount, "slot": slot, "boundary": boundary, "instruction": transport.lastInstruction, "binding": binding}, "", "  ")
			f, writeErr := os.OpenFile(path+".failure.json", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if writeErr != nil {
				t.Fatal(writeErr)
			}
			_, writeErr = f.Write(append(diagnostic, '\n'))
			f.Close()
			if writeErr != nil {
				t.Fatal(writeErr)
			}
			t.Fatal("production quote or packet unavailable for", action, boundary)
		}
		e.Request.RecentBlockhash, e.Request.LastValidBlockHeight = bridgeVault, 99
		message, err := CompileJupiterMessage(e.Request)
		if err != nil {
			t.Fatal(err)
		}
		wire := append(make([]byte, 65), message...)
		wire[0] = 1
		offset := 3
		if message[0] == 0x80 {
			offset++
		}
		if message[offset] >= 128 {
			t.Fatal("probe static key decoder needs update")
		}
		for i := 0; i < int(message[offset]); i++ {
			addresses[encodeBase58(message[offset+1+i*32:offset+1+(i+1)*32])] = true
		}
		for _, a := range e.Request.Instruction.Accounts {
			addresses[a.Pubkey] = true
		}
		for _, table := range e.Request.LookupTables {
			addresses[table.Address] = true
		}
		binding, err := catalogJupiterBindingForRoute(action, ethenaUSDePYUSD.Lane)
		if err != nil {
			t.Fatal(err)
		}
		policies[e.Request.Policy] = e.Request.PolicyAccountDataSHA256
		rows = append(rows, map[string]any{"action": action, "request": e.Request, "wireBase64": base64.StdEncoding.EncodeToString(wire), "wireSha256": sha256Bytes(wire),
			"source": binding.SourceCustody, "destination": binding.DestinationCustody, "amountRaw": amount, "minimumOutputRaw": e.Request.MinimumOutputRaw,
			"instructionDataBase64": e.Request.Instruction.Data, "amountOffset": binding.AmountOffset, "policyMaximumInputRaw": binding.MaxInputRaw})
		amount = e.Request.MinimumOutputRaw
	}
	keys := []string{}
	for a := range addresses {
		keys = append(keys, a)
	}
	sort.Strings(keys)
	if len(keys) > 100 {
		t.Fatal("probe exceeds one coherent account batch")
	}
	plan := map[string]any{"schema": "phase3-jupiter-controlled-probe/v1", "broadcast": false, "lane": ethenaUSDePYUSD.Lane, "quoteObservationSlot": slot,
		"delegate": bridgeDelegate, "inputCustody": bridgeSquadsATA, "collateralCustody": ethenaUSDePYUSD.CollateralCustody, "debtCustody": ethenaUSDePYUSD.DebtCustody,
		"steps": rows, "addresses": keys, "policies": policies}
	data, err := json.MarshalIndent(plan, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err = f.Write(append(data, '\n')); err != nil {
		t.Fatal(err)
	}
	t.Logf("ZERO_SIGNATURE_JUPITER_PROBE steps=2 accounts=%d planSha256=%s", len(keys), sha256Bytes(append(data, '\n')))
}

func TestPhase3JupiterProbeMatchesProduction(t *testing.T) {
	dir := os.Getenv("PHASE3_JUPITER_PROBE_DIR")
	if dir == "" {
		t.Skip("explicit public local probe required")
	}
	data, err := os.ReadFile(dir + "/plan.json")
	if err != nil {
		t.Fatal(err)
	}
	var plan struct {
		Schema, Lane string
		Broadcast    bool
		Steps        []struct {
			Request                JupiterSwapRequest
			WireBase64, WireSHA256 string
		}
	}
	if json.Unmarshal(data, &plan) != nil || plan.Schema != "phase3-jupiter-controlled-probe/v1" || plan.Broadcast || plan.Lane != ethenaUSDePYUSD.Lane || len(plan.Steps) != 2 {
		t.Fatal("invalid Jupiter probe scope")
	}
	for i, action := range []Action{SwapStableToCollateralStep, SwapCollateralToDebtStep} {
		step := plan.Steps[i]
		wire, err := base64.StdEncoding.Strict().DecodeString(step.WireBase64)
		if err != nil || len(wire) <= 65 || wire[0] != 1 || !allZero(wire[1:65]) || sha256Bytes(wire) != step.WireSHA256 || step.Request.RouteLane != plan.Lane || step.Request.Action != action {
			t.Fatal("probe identity or unsigned wire drift")
		}
		message, err := CompileJupiterMessage(step.Request)
		if err != nil || !bytes.Equal(message, wire[65:]) {
			t.Fatal("probe does not match current production compiler", err)
		}
	}
}
