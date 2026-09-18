package backyardrwa

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func redepositProbeRequest(t *testing.T, dir string) (KaminoPrimeUSDCRequest, []byte) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "plan.json"))
	var plan struct {
		LendingPrelude struct {
			Steps []struct{ Request KaminoPrimeUSDCRequest }
		}
	}
	if err != nil || json.Unmarshal(data, &plan) != nil || len(plan.LendingPrelude.Steps) != 4 {
		t.Fatal("missing linked plan", err)
	}
	r := plan.LendingPrelude.Steps[0].Request
	if r.RouteLane != ethenaUSDePYUSD.Lane {
		t.Fatal("wrong redeposit lane")
	}
	r.AmountRaw, r.Data = 1_000_000, append([]byte(nil), r.Data...)
	binary.LittleEndian.PutUint64(r.Data[8:], r.AmountRaw)
	r.ObligationReserves = []string{ethenaUSDePYUSD.Kamino.CollateralReserve, ethenaUSDePYUSD.Kamino.DebtReserve}
	return r, data
}

func TestExportPhase3RedepositProbe(t *testing.T) {
	dir, name := os.Getenv("PHASE3_JUPITER_RETURN_PROBE_DIR"), os.Getenv("PHASE3_REDEPOSIT_PLAN")
	if dir == "" || name == "" {
		t.Skip("explicit local probe path required")
	}
	if filepath.Base(name) != name {
		t.Fatal("probe must be a local filename")
	}
	r, data := redepositProbeRequest(t, dir)
	message, err := CompileKaminoMessage(r)
	if err != nil {
		t.Fatal(err)
	}
	wire := append(make([]byte, 65), message...)
	wire[0] = 1
	if len(wire) > 1232 {
		t.Fatal("redeposit does not fit")
	}
	encoded, err := json.Marshal(map[string]any{"request": r, "wireBase64": base64.StdEncoding.EncodeToString(wire), "wireSha256": sha256Bytes(wire), "planSha256": sha256Bytes(data), "broadcast": false, "signatureProof": false})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), encoded, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestPhase3RedepositMatchesProduction(t *testing.T) {
	dir, name := os.Getenv("PHASE3_JUPITER_RETURN_PROBE_DIR"), os.Getenv("PHASE3_JUPITER_PROBE_RESULT")
	if dir == "" || name == "" {
		t.Skip("explicit deployed-program probe required")
	}
	if filepath.Base(name) != name {
		t.Fatal("result must be a local filename")
	}
	r, plan := redepositProbeRequest(t, dir)
	type captured struct {
		Address, Owner, DataBase64, DataSHA256 string
		Lamports                               uint64
		Present                                bool
	}
	var result struct {
		Slot                       int64
		PlanSHA256, SnapshotSHA256 string
		RedepositProbe             struct {
			WireBase64, WireSHA256                      string
			Before, After                               []captured
			ActualDebitRaw, ReceiptBefore, ReceiptAfter uint64
			ComputeUnits                                uint64
			DebtBefore, DebtAfter                       string
			Overrides                                   []struct {
				Address             string
				BeforeRaw, AfterRaw uint64
			}
		}
	}
	data, err := os.ReadFile(filepath.Join(dir, name))
	snapshot, snapshotErr := os.ReadFile(filepath.Join(dir, "snapshot.json"))
	if err != nil || snapshotErr != nil || json.Unmarshal(data, &result) != nil || result.PlanSHA256 != sha256Bytes(plan) || result.SnapshotSHA256 != sha256Bytes(snapshot) {
		t.Fatal("redeposit witness identity mismatch", err)
	}
	p := result.RedepositProbe
	message, err := CompileKaminoMessage(r)
	wire, wireErr := base64.StdEncoding.Strict().DecodeString(p.WireBase64)
	if err != nil || wireErr != nil || len(wire) <= 65 || len(wire) > 1232 || wire[0] != 1 || !allZero(wire[1:65]) || sha256Bytes(wire) != p.WireSHA256 || !bytes.Equal(message, wire[65:]) {
		t.Fatal("redeposit did not execute current Go wire", err)
	}
	route := ethenaUSDePYUSD
	if len(p.Overrides) != 2 || p.Overrides[0].Address != route.CollateralCustody || p.Overrides[0].AfterRaw != 1_000_000 || p.Overrides[1].Address != route.DebtCustody || p.Overrides[1].AfterRaw != 0 {
		t.Fatal("unrecognized local custody overrides")
	}
	decode := func(rows []captured) []ConfirmedAccount {
		var accounts []ConfirmedAccount
		for _, a := range rows {
			if !a.Present {
				continue
			}
			data, err := base64.StdEncoding.Strict().DecodeString(a.DataBase64)
			if err != nil || sha256Bytes(data) != a.DataSHA256 {
				t.Fatal("invalid captured account")
			}
			accounts = append(accounts, ConfirmedAccount{Address: a.Address, Owner: a.Owner, Lamports: a.Lamports, Data: data})
		}
		return accounts
	}
	before, after := decode(p.Before), decode(p.After)
	effects, err := boundedKaminoDepositEffects(before, route, result.Slot, r.AmountRaw)
	if err != nil {
		t.Fatal(err)
	}
	projection := phase3KaminoProjection{Slot: result.Slot, MessageSHA256: sha256Bytes(message), UnitsConsumed: p.ComputeUnits, Accounts: after}
	if err := validateRedepositProjection(r, effects, before, projection); err != nil {
		t.Fatal("real redeposit rejected by admission", err)
	}
	old, err := decodeKaminoObligation(accountAt(before, route.Kamino.Obligation), route.Kamino)
	if err != nil {
		t.Fatal(err)
	}
	position, err := decodeKaminoObligation(accountAt(after, route.Kamino.Obligation), route.Kamino)
	if err != nil || old.collateralDepositedRaw != p.ReceiptBefore || position.collateralDepositedRaw != p.ReceiptAfter || p.ReceiptAfter <= p.ReceiptBefore || p.DebtBefore == "0" || p.DebtAfter != p.DebtBefore || p.ActualDebitRaw != 999_999 {
		t.Fatal("missing debt-bearing rounded redeposit witness", err)
	}
	bad := projection
	bad.Accounts = append([]ConfirmedAccount(nil), after...)
	for i, a := range bad.Accounts {
		if a.Address == route.Kamino.Obligation {
			bad.Accounts[i].Data = append([]byte(nil), a.Data...)
			binary.LittleEndian.PutUint64(bad.Accounts[i].Data[128:136], old.collateralDepositedRaw)
		}
	}
	if err := validateRedepositProjection(r, effects, before, bad); err == nil {
		t.Fatal("unchanged receipts passed real-poststate negative control")
	}
	t.Logf("PHASE3_REDEPOSIT requested=%d actual=%d receipts=%d->%d debtSF=%s", r.AmountRaw, p.ActualDebitRaw, p.ReceiptBefore, p.ReceiptAfter, p.DebtAfter)
}
