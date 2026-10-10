package backyard

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"testing"
)

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
