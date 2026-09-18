package backyardrwa

import (
	"encoding/hex"
	"strings"
	"testing"
)

// strategyTwoBridgePolicyDigests are the finalized digests of the four
// strategy-two Squads bridge policies (seeds 152-155), verified against
// mainnet at finalized commitment. The raw digests pin the exact account
// bytes as captured; the normalized digests are what the worker pins at
// runtime: the sha256 of the account bytes with the embedded limit's volatile
// region (timeConstraints.start, usage.remainingInPeriod, usage.lastReset)
// zeroed, so the program's own charging and re-windowing never breaks the pin.
var strategyTwoBridgePolicyRawDigests = map[Action]string{
	VoltrAllocateToSquads: "57043335df88ac51cbdbe2a6f4bcb2bedaab4b4ea1ca02f2f9a1c9ec06669c32",
	ReportNAV:             "13aadb4368399dcccdddc95c312c9a3088346cef6d5ead4001fc330bac5df64a",
	StageSquadsToVoltr:    "cd33348d492b9a5d2ffc7b0e1fedbb401e56374bb6b644f3a49a87ffe6bdc69d",
	VoltrRestoreIdle:      "ad7d9f26dd6ef2d0444142ef77ac40778eb72c793de50b6dafbebce1213fee1b",
}

var strategyTwoBridgePolicyNormalizedDigests = map[Action]string{
	VoltrAllocateToSquads: "453ba26c074cb35366f6d097d8818210604b4972b654de313a9954da445972bb",
	ReportNAV:             "8215849592af46928ae42273cbf65248d351db96e16cb2f3eabb38bfd237b1e5",
	StageSquadsToVoltr:    "66618b0a47eeed818ea38521aedf26b89494616d79643f4e9e50f0df81281708",
	VoltrRestoreIdle:      "563a0624b874235a78817f44f7563c4629b6b03892bd012b5eaf734cadd66834",
}

func TestManifestBridgeBindingsMatchStrategyTwoPolicies(t *testing.T) {
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
	expectedAccounts := map[Action]string{
		VoltrAllocateToSquads: bridgeAllocationPolicy,
		ReportNAV:             bridgeNAVPolicy,
		StageSquadsToVoltr:    bridgeStagePolicy,
		VoltrRestoreIdle:      bridgeWithdrawPolicy,
	}
	if len(manifest.RuntimeBindings.BridgePolicies) != len(expectedAccounts) {
		t.Fatalf("bridge policy set has %d entries, want %d", len(manifest.RuntimeBindings.BridgePolicies), len(expectedAccounts))
	}
	seenDigests := make(map[string]bool, len(expectedAccounts))
	for _, binding := range manifest.RuntimeBindings.BridgePolicies {
		if binding.Account != expectedAccounts[binding.Action] {
			t.Fatalf("bridge policy for %s drifted: %s", binding.Action, binding.Account)
		}
		for name, digest := range map[string]string{
			"normalizedDigest": binding.NormalizedDigest,
			"dataSha256Raw":    binding.DataSHA256Raw,
		} {
			if len(digest) != 64 || strings.ToLower(digest) != digest {
				t.Fatalf("bridge policy %s for %s is not 64 lowercase hex chars: %q", name, binding.Action, digest)
			}
			if _, err := hex.DecodeString(digest); err != nil {
				t.Fatalf("bridge policy %s for %s is not hex: %v", name, binding.Action, err)
			}
		}
		if binding.NormalizedDigest != strategyTwoBridgePolicyNormalizedDigests[binding.Action] {
			t.Fatalf("bridge policy normalized digest for %s does not match the finalized strategy-two policy", binding.Action)
		}
		if binding.DataSHA256Raw != strategyTwoBridgePolicyRawDigests[binding.Action] {
			t.Fatalf("bridge policy raw digest for %s does not match the finalized strategy-two policy", binding.Action)
		}
		if seenDigests[binding.NormalizedDigest] {
			t.Fatalf("bridge policy normalized digest for %s is not distinct", binding.Action)
		}
		seenDigests[binding.NormalizedDigest] = true
		if err := validatePolicyByteMask(binding.MaskedByteRanges); err != nil {
			t.Fatalf("bridge policy mask for %s is invalid: %v", binding.Action, err)
		}
		if len(binding.MaskedByteRanges) == 0 {
			t.Fatalf("bridge policy for %s has an empty mask but every strategy-two policy embeds a spending limit", binding.Action)
		}
	}
}

func TestStrategyTwoBridgeLegsRespectPerExecutionCap(t *testing.T) {
	digest := make([]byte, 32)
	digest[0] = 1
	request := func(action Action, amountRaw uint64) BridgeBuildRequest {
		return BridgeBuildRequest{
			Action: action, AmountRaw: amountRaw,
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
	if _, err := CompileBridgeMessage(request(VoltrAllocateToSquads, strategyTwoBridgeLegCapRaw)); err != nil ||
		strategyTwoBridgeLegCapRaw > strategyTwoDailyAllocationCapRaw {
		t.Fatal("strategy-two caps must keep every bridge leg inside the daily allocation bound")
	}
}

func TestReportTicketIsDerivedFromStrategyTwoConfig(t *testing.T) {
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
