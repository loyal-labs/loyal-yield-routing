package backyard

import (
	"testing"
	"time"
)

func TestNAVContractExactCoverage(t *testing.T) {
	q := ExitCost{Source: exitCostQuote, QuotedCollateralRaw: 11, MarkSF: kaminoOne()}
	if q.fresh(10, kaminoOne()) {
		t.Fatal("larger quote certified a different holding size")
	}
}
func TestNAVContractHistoryCannotPriceCurrentHoldings(t *testing.T) {
	c := ExitCost{Lane: onreONycUSDC, Source: exitCostRealized, BPS: 2, QuotedCollateralRaw: 10, MarkSF: kaminoOne(), Slot: 1, At: time.Now()}
	if _, known := (exitCosts{c.Lane: {Realized: &c}}).at(c.Lane, 10, c.MarkSF); known {
		t.Fatal("historical receipt certified current holdings")
	}
}
func TestNAVContractFreeExitIsKnownZero(t *testing.T) {
	c, err := exitCostOf(onreONycUSDC, exitCostQuote, 10, 10, 10, kaminoOne(), 1, time.Now())
	if err != nil || c.BPS != 0 {
		t.Fatal("free exit was replaced by nonzero sentinel", c.BPS, err)
	}
}
func TestNAVContractNumericReportCannotSign(t *testing.T) {
	r := BridgeBuildRequest{Action: bridgeReportNAV, Report: BridgeReport{Sequence: 1, ObservedSlot: 1, NAVAfterRaw: 0, SnapshotDigest: navDigest(sha256Bytes([]byte("book")))}, Policy: testPolicyAccount(policyKey{action: ReportNAV}), AdaptorConfig: bridgeStrategy, Settings: backyardSettings, RecentBlockhash: bridgeVault, LastValidBlockHeight: 99}
	if _, err := compileBridgeMessageForDelegate(rwa, r, mustKey(backyardDelegate)); err == nil {
		t.Fatal("raw numeric NAV report compiled without certificate")
	}
}
