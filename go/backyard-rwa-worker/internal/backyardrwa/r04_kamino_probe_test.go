package backyardrwa

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func TestPhase3KaminoProbeMatchesProduction(t *testing.T) {
	dir := os.Getenv("PHASE3_KAMINO_PROBE_DIR")
	if dir == "" {
		t.Skip("explicit local probe snapshot required")
	}
	data, err := os.ReadFile(filepath.Join(dir, "plan.json"))
	if err != nil {
		t.Fatal(err)
	}
	var plan struct {
		Lane  string
		Steps []struct {
			Leg, WireBase64, WireSHA256 string
			Request                     KaminoPrimeUSDCRequest
		}
	}
	if err := json.Unmarshal(data, &plan); err != nil {
		t.Fatal(err)
	}
	if plan.Lane != ethenaUSDePYUSD.Lane || len(plan.Steps) != 4 {
		t.Fatal("probe lane or coverage changed")
	}
	for i, name := range []string{"deposit", "borrow", "repay", "withdraw"} {
		s := plan.Steps[i]
		if s.Leg != name || s.Request.RouteLane != plan.Lane {
			t.Fatal("probe action order or identity changed")
		}
		wire, err := base64.StdEncoding.Strict().DecodeString(s.WireBase64)
		if err != nil || len(wire) <= 65 || wire[0] != 1 || !allZero(wire[1:65]) || sha256Bytes(wire) != s.WireSHA256 {
			t.Fatal("probe wire is not an exact unsigned local fixture")
		}
		message, err := CompileKaminoMessage(s.Request)
		if err != nil || !bytes.Equal(message, wire[65:]) {
			t.Fatal("probe message differs from current production compiler", err)
		}
	}
}

// Export only the four existing Ethena production messages for an explicit
// local-SVM probe. Zero signatures and an artificial blockhash make these
// unusable on mainnet. This exporter is not itself lifecycle proof.
func TestExportPhase3KaminoControlledProbe(t *testing.T) {
	path := os.Getenv("PHASE3_KAMINO_PROBE_PLAN")
	if path == "" {
		t.Skip("explicit local probe output required")
	}
	route := ethenaUSDePYUSD
	manifest, err := loadEmbeddedRouteManifest()
	if err != nil {
		t.Fatal(err)
	}
	rows := []any{}
	addresses := map[string]bool{budgetClockAddress: true, bridgeSettings: true}
	policies := map[string]string{}
	for _, step := range []struct {
		name   string
		leg    kaminoPrimeUSDCLeg
		action Action
		amount uint64
	}{
		// USDe has 9 decimals; PYUSD has 6. Use 0.1 collateral against
		// 0.001 debt, not equal raw-unit assumptions across the assets.
		{"deposit", kaminoLegDeposit, OpenRouteStep, 100_000_000},
		{"borrow", kaminoLegBorrow, OpenRouteStep, 1_000},
		{"repay", kaminoLegRepay, DeleverRouteStep, 100_000},
		{"withdraw", kaminoLegWithdraw, DeleverRouteStep, 1_000_000_000},
	} {
		r, err := manifest.kaminoPacketForRoute(step.action, step.leg, step.amount, LatestBlockhash{Blockhash: bridgeVault, LastValidBlockHeight: 99}, route.Lane)
		if err != nil {
			t.Fatal(err)
		}
		message, err := CompileKaminoMessage(r)
		if err != nil {
			t.Fatal(err)
		}
		// The account list is shortvec, but all reviewed legacy messages have
		// fewer than 128 static keys. Reject an encoding change here explicitly.
		if message[3] >= 128 {
			t.Fatal("probe static key decoder needs update")
		}
		for i := 0; i < int(message[3]); i++ {
			addresses[encodeBase58(message[4+32*i:4+32*(i+1)])] = true
		}
		wire := append(make([]byte, 65), message...)
		wire[0] = 1
		rows = append(rows, map[string]any{"leg": step.name, "amount": step.amount, "request": r, "wireBase64": base64.StdEncoding.EncodeToString(wire), "wireSha256": sha256Bytes(wire)})
		policies[r.Policy] = r.PolicyAccountDataSHA256
	}
	keys := []string{}
	for k := range addresses {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	encoded, err := json.MarshalIndent(map[string]any{"schema": "phase3-kamino-controlled-probe/v1", "broadcast": false, "signatureProof": false, "lane": route.Lane,
		"vault": bridgeVault, "delegate": bridgeDelegate, "obligation": route.Kamino.Obligation, "collateralCustody": route.CollateralCustody, "debtCustody": route.DebtCustody,
		"addresses": keys, "policies": policies, "steps": rows}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.Write(encoded); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	t.Log("exported exact four-leg Go messages; not executed lifecycle proof")
}
