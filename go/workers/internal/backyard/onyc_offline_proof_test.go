package backyard

// Opt-in offline unsigned construction. This test never creates an RPC client,
// loads a signer, persists an operation, or changes the supplied account bytes.
import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"testing"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
)

type onycProofInput struct {
	Operation     string              `json:"operation"`
	Amount        uint64              `json:"amountRaw"`
	Slot          int64               `json:"slot"`
	Accounts      []ConfirmedAccount  `json:"accounts"`
	Swap          *JupiterSwapRequest `json:"swap,omitempty"`
	PartialTarget *int64              `json:"partialTargetLTVBPS,omitempty"`
}

func TestExportONycOfflineProof(t *testing.T) {
	t.Parallel()
	name := os.Getenv("ONYC_PROOF_INPUT")
	if name == "" {
		t.Skip("explicit offline request file required")
	}
	raw, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	var in onycProofInput
	if err = json.Unmarshal(raw, &in); err != nil {
		t.Fatal(err)
	}
	m, err := loadEmbeddedRouteManifest()
	if err != nil {
		t.Fatal(err)
	}
	route, err := runtimeRoute("OnRe/ONyc/USDC")
	if err != nil {
		t.Fatal(err)
	}
	result := map[string]any{"schema": "onyc-offline-native/v1", "broadcast": false, "signatureProof": false, "workerAdmissionProof": false, "manifestSHA256": m.SHA256, "requestSHA256": sha256Bytes(raw), "ticket": reportTicketPDA}
	addresses := map[string]bool{}
	for _, a := range pinnedRouteNAVAddressesForRoute(route) {
		addresses[a] = true
	}
	for _, a := range []string{bridgeDelegate, bridgeSettings, bridgeVault, budgetClockAddress, reportTicketPDA, route.Kamino.CollateralMint, route.Kamino.DebtMint} {
		addresses[a] = true
	}
	// The lane's policies, where today's Settings holds them; the bank must
	// hold each as its literal.
	initializerKey, _ := initializerPolicyLeg(route.Lane)
	required := append([]policyKey{{family: BasicCollateralLifecycle}, {family: BasicDebtLifecycle}, {family: BasicSwapRoutesA},
		{family: BasicSwapRoutesB}, initializerKey}, bridgePolicyKeys...)
	policies := map[string]string{}
	for _, key := range required {
		address := testPolicyAccount(key)
		policies[address] = key.String()
		addresses[address] = true
	}
	initInstruction, err := kaminoMultiplyInitializer(route.Lane)
	if err != nil {
		t.Fatal(err)
	}
	addresses[encodeBase58(initInstruction.program[:])] = true
	for _, a := range initInstruction.accounts {
		addresses[encodeBase58(a.key[:])] = true
	}
	// Discovery uses native builder account vectors, not historical template wires.
	for _, leg := range []kaminoPrimeUSDCLeg{kaminoLegDeposit, kaminoLegBorrow, kaminoLegRepay, kaminoLegWithdraw} {
		action := OpenRouteStep
		if leg == kaminoLegRepay || leg == kaminoLegWithdraw {
			action = DeleverRouteStep
		}
		r, e := m.kaminoPacketForRoute(testPolicies(t), action, leg, 1, LatestBlockhash{Blockhash: bridgeVault, LastValidBlockHeight: 99}, route.Lane)
		if e != nil {
			t.Fatal(e)
		}
		inner, _, e := kaminoResolvedRouteInstruction(r, route)
		if e != nil {
			t.Fatal(e)
		}
		for _, ix := range append(kaminoRefreshInstructionsForResolvedRoute(leg, r, route), inner) {
			addresses[encodeBase58(ix.program[:])] = true
			for _, a := range ix.accounts {
				addresses[encodeBase58(a.key[:])] = true
			}
		}
	}
	for _, action := range []Action{VoltrAllocateToSquads, StageSquadsToVoltr, VoltrRestoreIdle, ReportNAV} {
		amount := uint64(1)
		if action == ReportNAV {
			amount = 0
		}
		r := BridgeBuildRequest{Action: action, AmountRaw: amount, Report: BridgeReport{Sequence: 1, ObservedSlot: 1, NAVAfterRaw: amount, SnapshotDigest: sha256Bytes([]byte("discovery"))}, AdaptorConfig: bridgeStrategy, Settings: bridgeSettings, RecentBlockhash: bridgeVault, LastValidBlockHeight: 99}
		ixs, _, e := ticketedBridgeInstructions(r)
		if e != nil {
			t.Fatal(e)
		}
		for _, ix := range ixs {
			addresses[encodeBase58(ix.program[:])] = true
			for _, a := range ix.accounts {
				addresses[encodeBase58(a.key[:])] = true
			}
		}
	}
	keys := []string{}
	for a := range addresses {
		keys = append(keys, a)
	}
	sort.Strings(keys)
	result["addresses"], result["policies"] = keys, policies
	if in.Operation == "discover" {
		onycProofPrint(t, result)
		return
	}
	if in.Slot <= 0 {
		t.Fatal("positive offline bank slot required")
	}
	seen := map[string]bool{}
	for _, a := range in.Accounts {
		if seen[a.Address] {
			t.Fatal("duplicate bank account")
		}
		seen[a.Address] = true
	}
	var bank []squads.Installed
	for address := range policies {
		view, _ := squads.DecodeCanonicalPolicy(chainAccount(accountAt(in.Accounts, address), address))
		bank = append(bank, squads.Installed{Account: kaminoKey(address), View: view})
	}
	installed, err := findInstalledPolicies(bank)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range required {
		if _, err := installed.account(key); err != nil {
			t.Fatalf("installed policy mismatch: %v", err)
		}
	}
	navAccounts, err := selectRouteNAVAccountsForRoute(in.Accounts, route)
	if err != nil {
		t.Fatal(err)
	}
	nav, err := ComputeRouteNAVForRoute(in.Slot, navAccounts, m, nil, route)
	if err != nil {
		t.Fatal(err)
	}
	result["nav"] = nav
	result["position"] = KaminoPosition{} // KLend closes a fully withdrawn obligation.
	obligationAccount := accountAt(in.Accounts, route.Kamino.Obligation)
	if obligationAccount.Address != route.Kamino.Obligation || (obligationAccount.Lamports == 0 && len(obligationAccount.Data) != 0) {
		t.Fatal("missing or malformed closed obligation evidence")
	}
	if obligationAccount.Lamports != 0 {
		obligation, e := decodeKaminoObligation(obligationAccount, route.Kamino)
		if e != nil {
			t.Fatal(e)
		}
		collateral, e := decodeKaminoReserve(accountAt(in.Accounts, route.Kamino.CollateralReserve), route.Kamino.CollateralMint, route.Kamino)
		if e != nil {
			t.Fatal(e)
		}
		debt, e := decodeKaminoReserve(accountAt(in.Accounts, route.Kamino.DebtReserve), route.Kamino.DebtMint, route.Kamino)
		if e != nil {
			t.Fatal(e)
		}
		underlying, e := collateral.redeemLiquidityRaw(obligation.collateralDepositedRaw)
		if e != nil {
			t.Fatal(e)
		}
		owed, e := obligation.debtAtReserveRate(debt)
		if e != nil {
			t.Fatal(e)
		}
		position := KaminoPosition{Slot: in.Slot, RefreshedSlot: obligation.refreshedSlot, HasPosition: obligation.hasPosition, ObligationPresent: true, LiquidationThresholdBPS: int64(collateral.liquidationThresholdPct) * 100, CollateralDepositedRaw: obligation.collateralDepositedRaw, RedeemablePrimeRaw: underlying, DebtRaw: owed, CollateralDecimals: collateral.mintDecimals, DebtDecimals: debt.mintDecimals, CollateralPriceSF: collateral.marketPriceSF, DebtPriceSF: debt.marketPriceSF}
		result["position"] = position
		snapshot := Snapshot{RouteLane: route.Lane, HasPosition: obligation.hasPosition, PositionCollateralRaw: int64(obligation.collateralDepositedRaw), PositionDebtRaw: int64(owed), PositionCollateralValueRaw: int64(nav.PositionCollateralValue), PositionDebtValueRaw: int64(nav.PositionDebtValue), CollateralIdleRaw: int64(nav.Custodies.SquadsPRIMEraw), CollateralIdleValueRaw: int64(nav.PrimeIdleValueRaw), SquadsIdleRaw: int64(nav.Custodies.SquadsUSDCraw), LeverageTargetLevel: 1.75}
		if in.PartialTarget != nil {
			snapshot.PartialWithdrawalOperationID = "offline-in-memory-not-durable"
			snapshot.PartialWithdrawalLTVBPS = *in.PartialTarget
		}
		target, ok := partialWithdrawalTargetLTVBPS(snapshot)
		if ok {
			result["partialTargetLTVBPS"] = target
			result["partialRepayRaw"] = partialWithdrawalRepayRaw(snapshot, target)
		}
		if in.Operation == "partial" {
			if in.Amount > 100_000_000_000 {
				t.Fatal("partial demand exceeds pilot equity")
			}
			free := int64(in.Amount) + max(int64(in.Amount)/100, partialWithdrawalMinimumBuffer)
			result["partialReceiptsRaw"] = partialWithdrawalReleaseReceipts(snapshot, free)
		}
		for _, level := range []int64{150, 175} {
			n, e := capacitySizedBorrow(position, in.Accounts, route, level)
			if e != nil {
				result[fmt.Sprintf("borrow%dError", level)] = e.Error()
			} else {
				result[fmt.Sprintf("borrow%dRaw", level)] = n
			}
		}
		if owed > 0 {
			bound, e := decodeKaminoPayoffBound(in.Accounts, route, in.Slot)
			if e != nil {
				result["payoffError"] = e.Error()
			} else {
				result["payoff"] = bound
			}
			release, e := decodeKaminoRepaymentReleaseForMode(in.Accounts, route, in.Slot, 5, true)
			if e != nil {
				result["releaseError"] = e.Error()
			} else {
				result["release"] = release
			}
			partial, e := decodeKaminoRepaymentReleaseWithAllowance(in.Accounts, route, in.Slot, 5, true, func(accounts []ConfirmedAccount, route RuntimeRoute, p KaminoPosition, liquidation byte) (uint64, error) {
				native, e := pilotRepaymentLiquidityAllowance(accounts, route, p, liquidation)
				if e != nil {
					return 0, e
				}
				stricter, e := withdrawableUnderlyingAtLTV(releaseValuesForPosition(p), 5000)
				return min(native, stricter), e
			})
			if e != nil {
				result["partialReleaseError"] = e.Error()
			} else {
				result["partialRelease"] = partial
			}
		}
	}
	var message []byte
	switch in.Operation {
	case "observe", "partial":
	case "initialize":
		if obligationAccount.Lamports != 0 {
			t.Fatal("initializer requires absent obligation")
		}
		r, e := m.initializationRequest(installed, route.Lane, LatestBlockhash{Blockhash: bridgeVault, LastValidBlockHeight: 99}, 24_165_120, 5_000)
		if e != nil {
			t.Fatal(e)
		}
		message, err = CompileKaminoInitializationMessage(r)
		result["request"] = r
	case "VOLTR_ALLOCATE_TO_SQUADS", "STAGE_SQUADS_TO_VOLTR", "VOLTR_RESTORE_IDLE", "REPORT_NAV":
		report := nav.Report
		ticket, e := decodeObservedReportTicket(accountAt(in.Accounts, reportTicketPDA))
		if e != nil {
			t.Fatal(e)
		}
		if ticket.Armed {
			t.Fatal("report ticket armed")
		}
		report.Sequence = max(uint64(in.Slot), ticket.LastConsumedSequence+1)
		switch Action(in.Operation) {
		case VoltrAllocateToSquads:
			if nav.StrategyNAVRaw != 0 || nav.Custodies.StrategyUSDCraw != 0 || nav.VaultIdleRaw < in.Amount || in.Amount > 100_000_000_000 {
				t.Fatal("allocation requires flat funded <=100k setup")
			}
			report.NAVAfterRaw += in.Amount
		case StageSquadsToVoltr:
			if in.Amount != nav.Custodies.SquadsUSDCraw || nav.Custodies.StrategyUSDCraw != 0 {
				t.Fatal("stage whole cash custody only")
			}
		case VoltrRestoreIdle:
			if in.Amount != nav.Custodies.StrategyUSDCraw || nav.Custodies.SquadsUSDCraw != 0 {
				t.Fatal("restore whole staged custody only")
			}
		case ReportNAV:
			if in.Amount != 0 {
				t.Fatal("report amount must be zero")
			}
		}
		policy, e := installed.account(policyKey{action: Action(in.Operation)})
		if e != nil {
			t.Fatal(e)
		}
		r := BridgeBuildRequest{Action: Action(in.Operation), AmountRaw: in.Amount, Report: report, Policy: policy, AdaptorConfig: bridgeStrategy, Settings: bridgeSettings, RecentBlockhash: bridgeVault, LastValidBlockHeight: 99}
		message, err = CompileBridgeMessage(r)
		result["request"] = r
	case "swap":
		if in.Swap == nil || in.Swap.RouteLane != route.Lane || in.Swap.AmountRaw != in.Amount {
			t.Fatal("exact ONyc swap request required")
		}
		r := *in.Swap
		if _, ok := policies[r.Policy]; !ok {
			t.Fatal("swap policy is not one of the lane's installed policies")
		}
		_, _, source, _, e := jupiterEdgeForRoute(r.Action, r.RouteLane)
		if e != nil {
			t.Fatal(e)
		}
		custody, e := DecodeTokenCustody(accountAt(in.Accounts, source).Owner, accountAt(in.Accounts, source).Data, mustKey(func() string {
			if source == bridgeSquadsATA {
				return bridgeUSDC
			}
			return route.Kamino.CollateralMint
		}()), mustKey(bridgeVault))
		if e != nil || custody.Raw < in.Amount {
			t.Fatal("insufficient swap custody", e)
		}
		for _, table := range r.LookupTables {
			a := accountAt(in.Accounts, table.Address)
			if a.Owner != table.Owner || a.Lamports != table.Lamports || !bytesEqual(a.Data, table.Data) {
				t.Fatal("ALT differs from bank")
			}
		}
		message, err = CompileJupiterMessage(r)
		result["request"] = r
	case "deposit", "borrow", "repay", "withdraw":
		obligation, e := decodeKaminoObligation(accountAt(in.Accounts, route.Kamino.Obligation), route.Kamino)
		if e != nil {
			t.Fatal(e)
		}
		leg, action := kaminoLegDeposit, OpenRouteStep
		switch in.Operation {
		case "borrow":
			leg = kaminoLegBorrow
		case "repay":
			leg, action = kaminoLegRepay, DeleverRouteStep
		case "withdraw":
			leg, action = kaminoLegWithdraw, DeleverRouteStep
		}
		r, e := m.kaminoPacketForRoute(installed, action, leg, in.Amount, LatestBlockhash{Blockhash: bridgeVault, LastValidBlockHeight: 99}, route.Lane)
		if e != nil {
			t.Fatal(e)
		}
		r.ObligationReserves = []string{}
		if obligation.collateralDepositedRaw > 0 {
			r.ObligationReserves = append(r.ObligationReserves, route.Kamino.CollateralReserve)
		}
		if obligation.debtRaw > 0 {
			r.ObligationReserves = append(r.ObligationReserves, route.Kamino.DebtReserve)
		}
		if leg == kaminoLegBorrow {
			if e := validateCapacityBorrow(in.Accounts, route, in.Amount); e != nil {
				t.Fatal(e)
			}
		}
		if leg == kaminoLegWithdraw && obligation.debtRaw > 0 {
			bound, e := decodeKaminoRepaymentReleaseForMode(in.Accounts, route, in.Slot, 5, true)
			if e != nil || in.Amount > bound.ReceiptRaw {
				t.Fatal("release exceeds native risk bound", e)
			}
			r.RepaymentRelease, r.PilotRepaymentRelease = true, true
		}
		message, err = CompileKaminoMessage(r)
		result["request"] = r
	default:
		t.Fatal("unsupported offline operation")
	}
	if err != nil {
		t.Fatal(err)
	}
	if len(message) > 0 {
		wire := append(make([]byte, 65), message...)
		wire[0] = 1
		if len(wire) > 1232 {
			t.Fatal("packet overflow")
		}
		result["wireBase64"], result["wireSha256"], result["packetBytes"] = base64.StdEncoding.EncodeToString(wire), sha256Bytes(wire), len(wire)
	}
	onycProofPrint(t, result)
}
func onycProofPrint(t *testing.T, result map[string]any) {
	t.Helper()
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Println("ONYC_PROOF_JSON=" + string(data))
}

// Regression from the connected100k proof: receipt sizing must not wrap the
// USDC-value product before dividing. A zero release otherwise sends a modest
// withdrawal into the full-exit fallback at the approved pilot size.
func TestONycPilotSizedPartialWithdrawalDoesNotOverflow(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name                                 string
		collateral, debt, receipts, expected int64
	}{
		{"captured100k1x", 100_131_623_873, 0, 86_951_421_135_998, 8_770_549_397_934},
		{"100k1.75x", 175_000_000_000, 75_000_000_000, 175_000_000_000_000, 17_675_000_000_000},
		{"100kFractional", 120_000_000_000, 20_000_000_000, 120_000_000_000_000, 12_120_000_000_000},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := Snapshot{PositionCollateralValueRaw: c.collateral, PositionDebtValueRaw: c.debt, PositionCollateralRaw: c.receipts}
			got := partialWithdrawalReleaseReceipts(s, 10_100_000_000)
			if got != c.expected {
				t.Fatalf("10k withdrawal plus1%% buffer: receipt release=%d, want%d; must not force full exit", got, c.expected)
			}
		})
	}
}
