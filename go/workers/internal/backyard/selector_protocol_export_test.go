package backyard

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
)

// Produces unsigned current-compiler input for a captured-program experiment.
// The 15-USDC collateral / 5-USDC debt position is a sizing witness only:
// seeding that collateral is not a deposit/swap or financed lifecycle proof.
func TestExportSelectorProtocolPosition(t *testing.T) {
	path := os.Getenv("SELECTOR_PROTOCOL_PLAN")
	if path == "" {
		t.Skip("explicit public-RPC capture plan required")
	}
	rpc, err := chain.New(os.Getenv("SOLANA_RPC_URL"), 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	route, err := runtimeRoute(SelectedRouteID)
	if err != nil {
		t.Fatal("selected route absent")
	}
	batch, err := finalizedAccounts(ctx, rpc, []string{route.Kamino.CollateralReserve, route.Kamino.DebtReserve}, 0)
	slot, accounts := batch.Slot, batch.Accounts
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
	addresses := map[string]bool{budgetClockAddress: true, backyardSettings: true}
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
		r, err := manifest.kaminoPacketForRoute(testPolicies(t), step.action, step.leg, step.amount, LatestBlockhash{Blockhash: bridgeVault, LastValidBlockHeight: 99}, route.Lane)
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
		wire := signedWire(message, make([]byte, 64))
		rows = append(rows, map[string]any{"leg": step.name, "amount": step.amount, "request": r, "wireBase64": base64.StdEncoding.EncodeToString(wire), "wireSha256": sha256Bytes(wire)})
		key, _ := kaminoPolicyLeg(route, step.leg)
		policies[r.Policy] = key.String()
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

// Retained SBF witnesses must remain byte-identical to today's compiler.
func TestSelectorProtocolMessagesMatchCurrentCompiler(t *testing.T) {
	t.Parallel()
	dir := os.Getenv("SELECTOR_PROTOCOL_DIR")
	if dir == "" {
		t.Skip("explicit retained protocol evidence required")
	}
	type record struct {
		Request                KaminoLegRequest
		WireBase64, WireSHA256 string
	}
	verify := func(r record) {
		t.Helper()
		wire, err := base64.StdEncoding.Strict().DecodeString(r.WireBase64)
		if err != nil || len(wire) <= 65 || wire[0] != 0x81 || !allZero(wireSig(wire)) || sha256Bytes(wire) != r.WireSHA256 {
			t.Fatal("invalid retained unsigned wire")
		}
		message, err := CompileKaminoMessage(r.Request)
		if err != nil || !bytes.Equal(wireMsg(wire), message) {
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
	if data, err = os.ReadFile(dir + "/partial-plan.json"); err == nil {
		var partial record
		if json.Unmarshal(data, &partial) != nil || partial.Request.RouteLane != SelectedRouteID || partial.Request.Action != DeleverRouteStep || partial.Request.AmountRaw != 1_000_000 {
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
