package backyardrwa

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestMultiplyInitializerPinnedTopology(t *testing.T) {
	for _, lane := range selectorLanes {
		ix, err := kaminoMultiplyInitializer(lane)
		if err != nil {
			t.Fatal(err)
		}
		route, _ := runtimeRoute(lane)
		if len(ix.accounts) != 9 || ix.accounts[2].key != mustKey(route.Kamino.Obligation) ||
			ix.accounts[6].key != mustKey("78e2ZY7pcpQjhimGg9DUn8cipXaFPdpHEkd2YkMDNEr1") {
			t.Fatal("PDA differs from captured initializer", lane)
		}
		outer, err := wrapSquadsKaminoPolicy(mustKey(bridgeSettings), mustKey(bridgeDelegate), mustKey(bridgeDelegate), 0, ix)
		if err != nil {
			t.Fatal(err)
		}
		message, err := compileLegacyMessage(mustKey(bridgeDelegate), mustKey(bridgeVault), []compiledInstruction{outer})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = checkedUnsignedMessage(message); err != nil {
			t.Fatal("initializer exceeds wire boundary", err)
		}
	}
	if _, err := kaminoMultiplyInitializer("Ethena/USDe/PYUSD"); err == nil {
		t.Fatal("unreviewed lane admitted")
	}
}

// Run explicitly after tools/backyard-voltr dependencies are installed. Ordinary
// worker tests do not acquire that operator tool's separate SDK dependency tree.
func TestMultiplyInitializerAndCapacitySDKParity(t *testing.T) {
	if os.Getenv("KLEND_SDK_ORACLE") != "1" {
		t.Skip("set KLEND_SDK_ORACLE=1 with operator SDK installed")
	}
	var instructions []map[string]any
	for _, lane := range selectorLanes {
		ix, err := kaminoMultiplyInitializer(lane)
		if err != nil {
			t.Fatal(err)
		}
		var accounts []map[string]any
		for _, a := range ix.accounts {
			accounts = append(accounts, map[string]any{"address": encodeBase58(a.key[:]), "signer": a.signer, "writable": a.writable})
		}
		instructions = append(instructions, map[string]any{"lane": lane, "program": encodeBase58(ix.program[:]), "accounts": accounts, "data": base64.StdEncoding.EncodeToString(ix.data)})
	}
	input, _ := json.Marshal(map[string]any{"instructions": instructions, "offsets": map[string]int{
		"elevationGroup": kaminoObligationElevationGroupOffset, "outsideUsed": kaminoOutsideBorrowCounterOffset,
		"outsideLimit": kaminoOutsideBorrowLimitOffset, "disableCross": kaminoDisableCrossCollateralOffset,
		"debtWithdrawalCap": kaminoDebtWithdrawalCapOffset, "borrowFactor": kaminoBorrowFactorOffset,
		"loanToValue": kaminoLoanToValueOffset, "queuedCollateral": kaminoQueuedCollateralOffset,
	}})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bun", "testdata/kamino-selector-oracle.mjs")
	cmd.Stdin = bytes.NewReader(input)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("SDK parity: %v %s", err, output)
	}
}
