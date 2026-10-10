package backyard

import (
	"encoding/base64"
	"reflect"
	"testing"
)

func TestMultiplyInitializerPinnedTopology(t *testing.T) {
	t.Parallel()
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

// The initializer and capacity offsets match what the Kamino klend SDK built
// and laid out (testdata/sdk-vectors.json).
func TestMultiplyInitializerAndCapacitySDKParity(t *testing.T) {
	t.Parallel()
	vectors := sdkVectors(t).KaminoInitializer
	if len(vectors.Instructions) != len(selectorLanes) {
		t.Fatal("SDK initializer vectors do not cover every selector lane")
	}
	for i, lane := range selectorLanes {
		ix, err := kaminoMultiplyInitializer(lane)
		if err != nil {
			t.Fatal(err)
		}
		want := vectors.Instructions[i]
		if want.Lane != lane || encodeBase58(ix.program[:]) != want.Program || base64.StdEncoding.EncodeToString(ix.data) != want.Data || len(ix.accounts) != len(want.Accounts) {
			t.Fatal("initializer differs from SDK", lane)
		}
		for j, a := range ix.accounts {
			if w := want.Accounts[j]; encodeBase58(a.key[:]) != w.Address || a.signer != w.Signer || a.writable != w.Writable {
				t.Fatal("initializer account differs from SDK", lane, j)
			}
		}
		if encodeBase58(ix.accounts[2].key[:]) != want.Obligation {
			t.Fatal("obligation differs from SDK PDA", lane)
		}
	}
	offsets := map[string]int{
		"elevationGroup": kaminoObligationElevationGroupOffset, "outsideUsed": kaminoOutsideBorrowCounterOffset,
		"outsideLimit": kaminoOutsideBorrowLimitOffset, "disableCross": kaminoDisableCrossCollateralOffset,
		"debtWithdrawalCap": kaminoDebtWithdrawalCapOffset, "borrowFactor": kaminoBorrowFactorOffset,
		"loanToValue": kaminoLoanToValueOffset, "queuedCollateral": kaminoQueuedCollateralOffset,
		"globalBorrowValue":     kaminoGlobalBorrowValueOffset,
		"minimumRemainingValue": kaminoMinRemainingValueOffset,
	}
	if !reflect.DeepEqual(offsets, vectors.Offsets) {
		t.Fatal("capacity offsets differ from SDK layouts", offsets, vectors.Offsets)
	}
}
