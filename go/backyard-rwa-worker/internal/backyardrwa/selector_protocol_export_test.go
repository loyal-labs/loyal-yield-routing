package backyardrwa

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"os"
	"sort"
	"testing"
	"time"
)

// Produces unsigned current-compiler input for a captured-program experiment.
// The 15-USDC collateral / 5-USDC debt position is a sizing witness only:
// seeding that collateral is not a deposit/swap or financed lifecycle proof.
func TestExportSelectorProtocolPosition(t *testing.T) {
	path := os.Getenv("SELECTOR_PROTOCOL_PLAN")
	if path == "" {
		t.Skip("explicit public-RPC capture plan required")
	}
	rpc, err := NewRPCClient(os.Getenv("SOLANA_RPC_URL"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	route, err := runtimeRoute(SelectedRouteID)
	if err != nil {
		t.Fatal("selected route absent")
	}
	minimumSlot, err := rpc.FinalizedSlot(ctx)
	if err != nil {
		t.Fatal("finalized slot unavailable")
	}
	slot, accounts, err := rpc.getMultipleAccountsAtCommitment(ctx, []string{route.Kamino.CollateralReserve, route.Kamino.DebtReserve}, minimumSlot, nil, "finalized")
	if err != nil {
		t.Fatal("reserve capture unavailable")
	}
	collateral, err := decodeKaminoReserve(accountAt(accounts, route.Kamino.CollateralReserve), route.Kamino.CollateralMint, route.Kamino)
	if err != nil {
		t.Fatal(err)
	}
	debt, err := decodeKaminoReserve(accountAt(accounts, route.Kamino.DebtReserve), route.Kamino.DebtMint, route.Kamino)
	if err != nil {
		t.Fatal(err)
	}
	amount, err := valueBetweenTokenRaw(15_000_000, debt.mintDecimals, collateral.mintDecimals, debt.marketPriceSF, collateral.marketPriceSF, false)
	if err != nil || amount == 0 {
		t.Fatal("collateral sizing unavailable", err)
	}
	manifest, err := loadEmbeddedRouteManifest()
	if err != nil {
		t.Fatal(err)
	}
	addresses := map[string]bool{budgetClockAddress: true, bridgeSettings: true}
	policies := map[string]string{}
	rows := []any{}
	for _, step := range []struct {
		name   string
		leg    kaminoPrimeUSDCLeg
		action Action
		amount uint64
	}{
		{"deposit", kaminoLegDeposit, OpenRouteStep, amount},
		{"borrow", kaminoLegBorrow, OpenRouteStep, 5_000_000},
		{"repay", kaminoLegRepay, DeleverRouteStep, 5_100_000},
		{"withdraw", kaminoLegWithdraw, DeleverRouteStep, 1_000_000_000_000},
	} {
		r, err := manifest.kaminoPacketForRoute(step.action, step.leg, step.amount, LatestBlockhash{Blockhash: bridgeVault, LastValidBlockHeight: 99}, route.Lane)
		if err != nil {
			t.Fatal(err)
		}
		message, err := CompileKaminoMessage(r)
		if err != nil {
			t.Fatal(err)
		}
		if len(message) < 4 || message[3] >= 128 {
			t.Fatal("unreviewed static account encoding")
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
	for key := range addresses {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	output, err := json.MarshalIndent(map[string]any{"schema": "selector-kamino-position-probe/v1", "broadcast": false, "signatureProof": false,
		"lane": route.Lane, "sizingSlot": slot, "sizingAccountsSHA256": hashConfirmedAccounts(accounts), "collateralSeedRaw": amount,
		"debtSeedRaw": 100_000, "vault": bridgeVault, "delegate": bridgeDelegate, "obligation": route.Kamino.Obligation,
		"collateralCustody": route.CollateralCustody, "debtCustody": route.DebtCustody, "addresses": keys, "policies": policies, "steps": rows}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.Write(output); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	t.Log("exported unsigned protocol-position experiment; no lifecycle proof")
}

func TestExportSelectorProtocolRelease(t *testing.T) {
	dir := os.Getenv("SELECTOR_PROTOCOL_DIR")
	if dir == "" {
		t.Skip("executed position witness required")
	}
	raw, err := os.ReadFile(dir + "/position-result.json")
	if err != nil {
		t.Fatal(err)
	}
	var report struct {
		Slot           int64
		FourLegsPassed bool
		Steps          []struct {
			After []struct {
				Address, Owner, DataBase64, DataSHA256 string
				Lamports                               uint64
				Present                                bool
			}
		}
	}
	if json.Unmarshal(raw, &report) != nil || !report.FourLegsPassed || len(report.Steps) != 4 {
		t.Fatal("incomplete position witness")
	}
	accounts := []ConfirmedAccount{}
	for _, row := range report.Steps[1].After {
		if !row.Present {
			continue
		}
		data, err := base64.StdEncoding.Strict().DecodeString(row.DataBase64)
		if err != nil || sha256Bytes(data) != row.DataSHA256 {
			t.Fatal("witness account hash mismatch")
		}
		accounts = append(accounts, ConfirmedAccount{Address: row.Address, Owner: row.Owner, Lamports: row.Lamports, Data: data})
	}
	route, err := runtimeRoute(SelectedRouteID)
	if err != nil {
		t.Fatal(err)
	}
	bound, err := decodeKaminoRepaymentReleaseForMode(accounts, route, report.Slot, 5, true)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := loadEmbeddedRouteManifest()
	if err != nil {
		t.Fatal(err)
	}
	r, err := manifest.kaminoPacketForRoute(DeleverRouteStep, kaminoLegWithdraw, bound.ReceiptRaw, LatestBlockhash{Blockhash: bridgeVault, LastValidBlockHeight: 99}, route.Lane)
	if err != nil {
		t.Fatal(err)
	}
	r.RepaymentRelease = true
	r.PilotRepaymentRelease = true
	r.ObligationReserves = []string{route.Kamino.CollateralReserve, route.Kamino.DebtReserve}
	r.ReleaseDebtIdleRaw = binary.LittleEndian.Uint64(accountAt(accounts, route.DebtCustody).Data[64:72])
	message, err := CompileKaminoMessage(r)
	if err != nil {
		t.Fatal(err)
	}
	wire := append(make([]byte, 65), message...)
	wire[0] = 1
	out, err := json.MarshalIndent(map[string]any{"schema": "selector-kamino-release-probe/v1", "lane": route.Lane, "sourceResultSHA256": sha256Bytes(raw), "bound": bound, "request": r, "wireBase64": base64.StdEncoding.EncodeToString(wire), "wireSha256": sha256Bytes(wire)}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(dir+"/release-plan.json", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.Write(out); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
}

// Retained SBF witnesses must remain byte-identical to today's compiler.
func TestSelectorProtocolMessagesMatchCurrentCompiler(t *testing.T) {
	dir := os.Getenv("SELECTOR_PROTOCOL_DIR")
	if dir == "" {
		t.Skip("explicit retained protocol evidence required")
	}
	type record struct {
		Request                KaminoPrimeUSDCRequest
		WireBase64, WireSHA256 string
	}
	verify := func(r record) {
		t.Helper()
		wire, err := base64.StdEncoding.Strict().DecodeString(r.WireBase64)
		if err != nil || len(wire) <= 65 || wire[0] != 1 || !allZero(wire[1:65]) || sha256Bytes(wire) != r.WireSHA256 {
			t.Fatal("invalid retained unsigned wire")
		}
		message, err := CompileKaminoMessage(r.Request)
		if err != nil || !bytes.Equal(wire[65:], message) {
			t.Fatal("retained wire differs from current compiler", err)
		}
	}
	var plan struct {
		Lane  string
		Steps []record
	}
	data, err := os.ReadFile(dir + "/plan.json")
	if err != nil || json.Unmarshal(data, &plan) != nil || plan.Lane != SelectedRouteID || len(plan.Steps) != 4 {
		t.Fatal("invalid retained position plan", err)
	}
	for _, r := range plan.Steps {
		if r.Request.RouteLane != SelectedRouteID {
			t.Fatal("unexpected lane")
		}
		verify(r)
	}
	var release record
	data, err = os.ReadFile(dir + "/release-plan.json")
	if err != nil || json.Unmarshal(data, &release) != nil {
		t.Fatal("invalid retained release", err)
	}
	if !release.Request.PilotRepaymentRelease || !release.Request.RepaymentRelease || release.Request.RouteLane != SelectedRouteID {
		t.Fatal("release mode drift")
	}
	verify(release)
	if data, err = os.ReadFile(dir + "/partial-plan.json"); err == nil {
		var partial record
		if json.Unmarshal(data, &partial) != nil || partial.Request.RouteLane != SelectedRouteID || partial.Request.FullPayoff || partial.Request.RepaymentRelease || partial.Request.Action != DeleverRouteStep || partial.Request.AmountRaw != 1_000_000 {
			t.Fatal("partial repayment mode drift")
		}
		_, leg, err := kaminoPrimeUSDCInstruction(partial.Request)
		if err != nil || leg != kaminoLegRepay {
			t.Fatal("partial repayment leg drift")
		}
		verify(partial)
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

// A bounded debt-reduction branch, not production admission authorization.
func TestExportSelectorProtocolPartialRepayment(t *testing.T) {
	dir := os.Getenv("SELECTOR_PROTOCOL_DIR")
	if dir == "" {
		t.Skip("explicit retained protocol evidence required")
	}
	raw, err := os.ReadFile(dir + "/position-result.json")
	if err != nil {
		t.Fatal(err)
	}
	var report struct{ FourLegsPassed bool }
	if json.Unmarshal(raw, &report) != nil || !report.FourLegsPassed {
		t.Fatal("incomplete position witness")
	}
	m, err := loadEmbeddedRouteManifest()
	if err != nil {
		t.Fatal(err)
	}
	route, err := runtimeRoute(SelectedRouteID)
	if err != nil {
		t.Fatal(err)
	}
	decision := Decision{Action: DeleverRouteStep, Reason: "hard_ltv_repay", StrategyKey: SelectedRouteID, AmountRaw: 1_000_000}
	leg, amount, effect, err := selectKaminoLeg(true, decision, KaminoPosition{DebtRaw: 5_000_000})
	if err != nil || leg != kaminoLegRepay || amount != 1_000_000 || effect != amount {
		t.Fatal("partial repayment selection", err)
	}
	r, err := m.kaminoPacketForRoute(decision.Action, leg, amount, LatestBlockhash{Blockhash: bridgeVault, LastValidBlockHeight: 99}, SelectedRouteID)
	if err != nil {
		t.Fatal(err)
	}
	r.ObligationReserves = []string{route.Kamino.CollateralReserve, route.Kamino.DebtReserve}
	message, err := CompileKaminoMessage(r)
	if err != nil {
		t.Fatal(err)
	}
	wire := append(make([]byte, 65), message...)
	wire[0] = 1
	output, err := json.MarshalIndent(map[string]any{"schema": "selector-kamino-partial-probe/v1", "sourceResultSHA256": sha256Bytes(raw), "request": r, "amountRaw": amount, "wireBase64": base64.StdEncoding.EncodeToString(wire), "wireSha256": sha256Bytes(wire)}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(dir+"/partial-plan.json", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.Write(output); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
}
