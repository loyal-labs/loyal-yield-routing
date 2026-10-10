package backyard

import (
	"encoding/hex"
	"testing"
)

func TestManifestIdentitiesMatchStrategyTwo(t *testing.T) {
	t.Parallel()
	manifest, err := loadEmbeddedRouteManifest()
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Identities.V2StrategyConfig != bridgeStrategy ||
		manifest.Identities.ReportTicket != reportTicketPDA ||
		manifest.Identities.ReportTicketBump != int64(reportTicketBump) {
		t.Fatalf("manifest identities do not match the pinned strategy-two worker consts: config=%s ticket=%s bump=%d",
			manifest.Identities.V2StrategyConfig, manifest.Identities.ReportTicket, manifest.Identities.ReportTicketBump)
	}
}

func TestStrategyTwoBridgeLegsRespectPerExecutionCap(t *testing.T) {
	t.Parallel()
	digest := make([]byte, 32)
	digest[0] = 1
	request := func(action Action, amountRaw uint64) BridgeBuildRequest {
		return BridgeBuildRequest{
			Action: action, AmountRaw: amountRaw, Policy: testPolicyAccount(policyKey{action: action}),
			Report:        BridgeReport{Sequence: 100, ObservedSlot: 100, NAVAfterRaw: 1_000_000, SnapshotDigest: hex.EncodeToString(digest)},
			AdaptorConfig: bridgeStrategy, Settings: bridgeSettings,
			RecentBlockhash: bridgeVault, LastValidBlockHeight: 99,
		}
	}
	for _, action := range []Action{VoltrAllocateToSquads, StageSquadsToVoltr, VoltrRestoreIdle} {
		if _, err := CompileBridgeMessage(request(action, strategyTwoBridgeLegCapRaw)); err != nil {
			t.Fatalf("%s at the exact strategy-two cap must compile: %v", action, err)
		}
		if _, err := CompileBridgeMessage(request(action, strategyTwoBridgeLegCapRaw+1)); err == nil {
			t.Fatalf("%s above the strategy-two cap must be rejected", action)
		}
		if _, err := CompileBridgeMessage(request(action, 1_000_000)); err != nil {
			t.Fatalf("%s canary amount must compile: %v", action, err)
		}
	}
	if _, err := CompileBridgeMessage(request(VoltrAllocateToSquads, strategyTwoBridgeLegCapRaw)); err != nil {
		t.Fatalf("an allocation at the strategy-two cap must compile: %v", err)
	}
}

func TestReportTicketIsDerivedFromStrategyTwoConfig(t *testing.T) {
	t.Parallel()
	config, err := decodeKey(bridgeStrategy)
	if err != nil {
		t.Fatal(err)
	}
	// The ticket is the "report_ticket" PDA of the strategy-two config under
	// the adaptor program (REPORT_TICKET_SEED in
	// crates/loyal-voltr-rwa-nav-adaptor/src/processor.rs). The same
	// derivation cross-checks against tools/backyard-voltr's
	// PublicKey.findProgramAddressSync, which returned this exact address and
	// bump 255 for the pinned config.
	program := mustKey(bridgeAdaptorProgram)
	derived, err := findProgramDerivedAddress([]byte("report_ticket"), program[:], config[:])
	if err != nil {
		t.Fatal(err)
	}
	if derived != reportTicketPDA || reportTicketBump != 255 {
		t.Fatalf("strategy-two ticket drift: derived %s bump %d, pinned %s bump %d",
			derived, reportTicketBump, reportTicketPDA, reportTicketBump)
	}
}
