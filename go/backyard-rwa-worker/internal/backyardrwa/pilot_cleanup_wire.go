package backyardrwa

// Operator preparation CLI for the one bounded historical-custody cleanup:
// the attributed residual cash is reported, staged from Squads custody into
// the Voltr strategy custody, and restored to vault idle, one wire at a time.
// Every invocation re-reads finalized state, classifies the next single step,
// and emits exactly one UNSIGNED legacy message through the production
// CompileBridgeMessage path. It never loads a signer, never broadcasts, never
// touches the database, and never relaxes validatePilotFlatEvidence or budget
// activation: signing, sending, and reconciliation stay with the coordinator.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Fixed bound for this one cleanup. The attributed residue is ~214,921 raw
// USDC; any larger cash balance is not this cleanup and refuses.
const pilotCleanupMaxRaw uint64 = 220_000

type cleanupCustodyState struct {
	IdleRaw                  uint64
	StrategyRaw              uint64
	SquadsRaw                uint64
	ReceiptPositionValueRaw  uint64
	ReceiptCustodyTrackedRaw uint64
	TicketArmed              bool
}

type cleanupStep struct {
	Action    Action
	Order     int
	AmountRaw uint64
	Reason    string
}

// classifyCleanupStep is the pure state machine over finalized custody
// evidence. The order is fixed: report the attributed cash first (so the
// strategy value explains the Squads balance before anything moves), then
// stage, then restore. Intermediate states are the real states Voltr leaves
// behind after each wire - they are not the strict pilot-flat poststate and
// must not be refused here.
func classifyCleanupStep(state cleanupCustodyState, maxRaw uint64) (cleanupStep, error) {
	if state.TicketArmed {
		return cleanupStep{}, budgetHold("cleanup_ticket_armed")
	}
	if state.SquadsRaw > 0 && state.StrategyRaw > 0 {
		return cleanupStep{}, budgetHold("cleanup_cash_split_across_custodies")
	}
	for _, raw := range []uint64{state.SquadsRaw, state.StrategyRaw, state.ReceiptPositionValueRaw, state.ReceiptCustodyTrackedRaw} {
		if raw > maxRaw {
			return cleanupStep{}, budgetHold("cleanup_bound_exceeded")
		}
	}
	if state.SquadsRaw == 0 && state.StrategyRaw == 0 {
		if state.ReceiptPositionValueRaw != 0 || state.ReceiptCustodyTrackedRaw != 0 {
			return cleanupStep{}, budgetHold("cleanup_state_incoherent")
		}
		return cleanupStep{Reason: "cleanup_cash_flat"}, nil
	}
	amount := state.SquadsRaw
	if amount == 0 {
		amount = state.StrategyRaw
	}
	switch {
	case state.StrategyRaw == 0 && state.ReceiptCustodyTrackedRaw == 0 && state.ReceiptPositionValueRaw == 0:
		// Cash sits in Squads custody and no report explains it yet. The
		// first recovery report may legitimately precede any book
		// reconciliation; requiring a reconciled book here would deadlock.
		return cleanupStep{Action: ReportNAV, Order: 1, Reason: "cleanup_recovery_report_due"}, nil
	case state.StrategyRaw == 0 && state.ReceiptCustodyTrackedRaw == 0 && state.ReceiptPositionValueRaw == amount:
		return cleanupStep{Action: StageSquadsToVoltr, Order: 2, AmountRaw: amount, Reason: "cleanup_stage_reported_cash"}, nil
	case state.SquadsRaw == 0 && state.StrategyRaw == amount && state.ReceiptPositionValueRaw == amount && state.ReceiptCustodyTrackedRaw == 0:
		return cleanupStep{Action: VoltrRestoreIdle, Order: 3, AmountRaw: amount, Reason: "cleanup_restore_staged_cash"}, nil
	default:
		return cleanupStep{}, budgetHold("cleanup_state_incoherent")
	}
}

// cleanupVaultBookHold enforces the same exact accounting identity as M1.
// Staging is a plain SPL transfer and does not update the receipt or book.
func cleanupVaultBookHold(accounts []ConfirmedAccount) error {
	idle, err := decodePinnedUSDC(accountAt(accounts, bridgeIdleATA), bridgeIdleAuthority)
	if err != nil {
		return err
	}
	book, err := decodeVoltrVaultBook(accountAt(accounts, bridgeVoltrVault))
	if err != nil {
		return err
	}
	supply, err := decodeVoltrLPSupply(accountAt(accounts, bridgeLPMint))
	if err != nil {
		return err
	}
	receipt, err := decodeStrategyReceipt(accountAt(accounts, bridgeStrategyReceipt))
	if err != nil {
		return err
	}
	if receipt.PositionValueRaw > uint64(PilotDepositCapRaw) || receipt.CustodyTrackedRaw > uint64(PilotDepositCapRaw) || idle.Raw > uint64(PilotDepositCapRaw) || book.TotalValueRaw != idle.Raw+receipt.PositionValueRaw+receipt.CustodyTrackedRaw {
		return budgetHold("cleanup_vault_book_unexplained")
	}
	if idle.Raw > uint64(PilotDepositCapRaw) || supply != 0 ||
		book.FeeAccumulatorManagerRaw != 0 || book.FeeAccumulatorAdminRaw != 0 || book.FeeAccumulatorProtocolRaw != 0 {
		return budgetHold("cleanup_vault_book_not_empty")
	}
	return nil
}

// cleanupLaneResidueHold refuses any position or lane residue outside the
// three bridge USDC custodies, mirroring the lane checks inside
// validatePilotFlatEvidence. Absent optional accounts stay flat-absent.
func cleanupLaneResidueHold(accounts []ConfirmedAccount) error {
	authority, err := decodeBase58PublicKey(bridgeVault)
	if err != nil {
		return err
	}
	for _, lane := range pilotTransitionLanes {
		route, err := runtimeRoute(lane)
		if err != nil {
			return err
		}
		for _, custody := range []struct{ address, mint, program string }{
			{route.CollateralCustody, route.Kamino.CollateralMint, route.CollateralTokenProgram},
			{route.DebtCustody, route.Kamino.DebtMint, route.DebtTokenProgram},
		} {
			if custody.address == bridgeSquadsATA {
				continue // Independently decoded and bounded as bridge cash.
			}
			account := accountAt(accounts, custody.address)
			if account.Lamports == 0 {
				if account.Owner != "" || len(account.Data) != 0 || account.Executable {
					return budgetHold("cleanup_lane_custody_integrity")
				}
				continue
			}
			mint, err := decodeBase58PublicKey(custody.mint)
			if err != nil {
				return err
			}
			if account.Owner != custody.program || account.Executable {
				return budgetHold("cleanup_lane_custody_identity")
			}
			token, err := DecodeTokenCustody(account.Owner, account.Data, mint, authority)
			if err != nil {
				return err
			}
			if token.Raw != 0 {
				return budgetHold("cleanup_lane_custody_not_flat")
			}
		}
		obligation := accountAt(accounts, route.Kamino.Obligation)
		if obligation.Lamports == 0 {
			if obligation.Owner != "" || len(obligation.Data) != 0 || obligation.Executable {
				return budgetHold("cleanup_lane_obligation_integrity")
			}
			continue
		}
		if _, err := decodeKaminoObligation(obligation, route.Kamino); err != nil {
			return err
		}
		// Same raw-word pins as validatePilotFlatEvidence.
		for i := 0; i < 8; i++ {
			if binary.LittleEndian.Uint64(obligation.Data[128+i*136:136+i*136]) != 0 {
				return budgetHold("cleanup_lane_position_not_flat")
			}
		}
		for i := 0; i < 5; i++ {
			if !allZero(obligation.Data[1296+i*200 : 1312+i*200]) {
				return budgetHold("cleanup_lane_debt_not_flat")
			}
		}
	}
	return nil
}

type cleanupWireOutput struct {
	Schema          string                  `json:"schema"`
	Broadcast       bool                    `json:"broadcast"`
	SignerLoaded    bool                    `json:"signerLoaded"`
	Status          string                  `json:"status"`
	ObservedSlot    int64                   `json:"observedSlot"`
	FinalizedSlot   int64                   `json:"finalizedSlot"`
	CleanupBoundRaw uint64                  `json:"cleanupBoundRaw"`
	Hold            *BudgetHold             `json:"hold,omitempty"`
	Wire            *cleanupWirePreparation `json:"wire,omitempty"`
	Evidence        cleanupEvidence         `json:"evidence"`
}

type cleanupEvidence struct {
	IdleRaw                  uint64 `json:"idleRaw"`
	StrategyRaw              uint64 `json:"strategyRaw"`
	SquadsRaw                uint64 `json:"squadsRaw"`
	ReceiptPositionValueRaw  uint64 `json:"receiptPositionValueRaw"`
	ReceiptCustodyTrackedRaw uint64 `json:"receiptCustodyTrackedRaw"`
	TicketArmed              bool   `json:"ticketArmed"`
	TicketLastConsumedSeq    uint64 `json:"ticketLastConsumedSequence"`
	BookTotalValueRaw        uint64 `json:"bookTotalValueRaw"`
}

type cleanupWirePreparation struct {
	Order                           int    `json:"order"`
	Action                          string `json:"action"`
	Reason                          string `json:"reason"`
	AmountRaw                       uint64 `json:"amountRaw"`
	ReportSequence                  uint64 `json:"reportSequence"`
	ReportObservedSlot              uint64 `json:"reportObservedSlot"`
	NAVAfterRaw                     uint64 `json:"navAfterRaw"`
	SnapshotDigest                  string `json:"snapshotDigest"`
	RecentBlockhash                 string `json:"recentBlockhash"`
	LastValidBlockHeight            int64  `json:"lastValidBlockHeight"`
	UnsignedMessageBase64           string `json:"unsignedMessageBase64"`
	UnsignedMessageSHA256           string `json:"unsignedMessageSHA256"`
	PostIdleRaw                     uint64 `json:"postIdleRaw"`
	PostStrategyRaw                 uint64 `json:"postStrategyRaw"`
	PostSquadsRaw                   uint64 `json:"postSquadsRaw"`
	ExpectedAdaptorReturnDataBase64 string `json:"expectedAdaptorReturnDataBase64,omitempty"`
}

// CompilePilotCleanupWire is read-only end to end: finalized custody plus an unchanged confirmed snapshot,
// one blockhash read, and the production message compiler. It never signs,
// sends, or persists. The coordinator signs the returned legacy message with
// the pinned delegated executor and broadcasts it out of band; the next
// invocation re-reads finalized state and prepares the following wire.
func CompilePilotCleanupWire(ctx context.Context, rpcURL string, out io.Writer) error {
	if out == nil {
		return fmt.Errorf("missing output writer")
	}
	rpc, err := NewRPCClient(rpcURL)
	if err != nil {
		return budgetHold("cleanup_rpc_unavailable")
	}
	manifest, err := loadEmbeddedRouteManifest()
	if err != nil {
		return err
	}
	if blocker := manifest.executionBlocker(); blocker != nil {
		return blocker
	}
	identity, err := newProgramIdentityWatcher(rpc).observe(ctx)
	if err != nil {
		return budgetHold("cleanup_identity_read_failed")
	}
	if !identity.Verified {
		return budgetHold("program_identity_unverified")
	}
	genesis, err := rpc.GenesisHash(ctx)
	if err != nil || genesis != mainnetGenesisHash {
		return budgetHold("cleanup_wrong_chain")
	}
	minimum, err := rpc.FinalizedSlot(ctx)
	if err != nil {
		return err
	}
	route, err := manifest.activeRuntimeRoute()
	if err != nil {
		return err
	}
	flatAddresses, optional, err := pilotFlatAddresses()
	if err != nil {
		return err
	}
	addresses := uniqueNonzero(append(append([]string{}, flatAddresses...), pinnedRouteNAVAddressesForRoute(route)...))
	slot, accounts, err := rpc.getMultipleAccountsAtCommitment(ctx, addresses, minimum, optional, "finalized")
	if err != nil {
		return budgetHold("cleanup_finalized_read_failed")
	}
	finalizedSlot := slot
	finalizedAccounts := accounts
	slot, accounts, err = rpc.getMultipleAccountsAtCommitment(ctx, addresses, finalizedSlot, optional, "confirmed")
	if err != nil {
		return budgetHold("cleanup_confirmed_read_failed")
	}
	// Finalized custody is authoritative; a fresh report may use the confirmed
	// slot only after every protected custody/position/book account is unchanged.
	for _, address := range flatAddresses {
		a, b := accountAt(finalizedAccounts, address), accountAt(accounts, address)
		if a.Owner != b.Owner || a.Lamports != b.Lamports || a.Executable != b.Executable || !bytes.Equal(a.Data, b.Data) {
			return budgetHold("cleanup_state_changed_after_finality")
		}
	}
	evidence, state, holdErr := cleanupEvidenceFromAccounts(accounts)
	output := cleanupWireOutput{Schema: "pilot-cleanup-wire/v1", Broadcast: false, SignerLoaded: false,
		FinalizedSlot: finalizedSlot, ObservedSlot: slot, CleanupBoundRaw: pilotCleanupMaxRaw, Evidence: evidence}
	if holdErr == nil {
		var step cleanupStep
		step, holdErr = classifyCleanupStep(state, pilotCleanupMaxRaw)
		if holdErr == nil && step.Action != "" {
			var wire *cleanupWirePreparation
			wire, holdErr = compileCleanupWire(ctx, rpc, manifest, route, accounts, slot, state, step)
			output.Wire = wire
		}
		if holdErr == nil {
			if step.Action == "" {
				output.Status = "CLEANUP_CASH_FLAT"
			} else {
				output.Status = "WIRE_PREPARED"
			}
		}
	}
	if holdErr != nil {
		var hold *BudgetHold
		if !errors.As(holdErr, &hold) {
			return holdErr
		}
		output.Hold = hold
	}
	if encodeErr := json.NewEncoder(out).Encode(output); encodeErr != nil {
		return encodeErr
	}
	return holdErr
}

func cleanupEvidenceFromAccounts(accounts []ConfirmedAccount) (cleanupEvidence, cleanupCustodyState, error) {
	idle, err := decodePinnedUSDC(accountAt(accounts, bridgeIdleATA), bridgeIdleAuthority)
	if err != nil {
		return cleanupEvidence{}, cleanupCustodyState{}, err
	}
	strategy, err := decodePinnedUSDC(accountAt(accounts, bridgeStrategyATA), bridgeStrategyAuth)
	if err != nil {
		return cleanupEvidence{}, cleanupCustodyState{}, err
	}
	squads, err := decodePinnedUSDC(accountAt(accounts, bridgeSquadsATA), bridgeVault)
	if err != nil {
		return cleanupEvidence{}, cleanupCustodyState{}, err
	}
	receipt, err := decodeStrategyReceipt(accountAt(accounts, bridgeStrategyReceipt))
	if err != nil {
		return cleanupEvidence{}, cleanupCustodyState{}, err
	}
	ticket, err := decodeObservedReportTicket(accountAt(accounts, reportTicketPDA))
	if err != nil {
		return cleanupEvidence{}, cleanupCustodyState{}, err
	}
	book, err := decodeVoltrVaultBook(accountAt(accounts, bridgeVoltrVault))
	if err != nil {
		return cleanupEvidence{}, cleanupCustodyState{}, err
	}
	evidence := cleanupEvidence{IdleRaw: idle.Raw, StrategyRaw: strategy.Raw, SquadsRaw: squads.Raw,
		ReceiptPositionValueRaw: receipt.PositionValueRaw, ReceiptCustodyTrackedRaw: receipt.CustodyTrackedRaw,
		TicketArmed: ticket.Armed, TicketLastConsumedSeq: ticket.LastConsumedSequence, BookTotalValueRaw: book.TotalValueRaw}
	state := cleanupCustodyState{IdleRaw: idle.Raw, StrategyRaw: strategy.Raw, SquadsRaw: squads.Raw,
		ReceiptPositionValueRaw: receipt.PositionValueRaw, ReceiptCustodyTrackedRaw: receipt.CustodyTrackedRaw,
		TicketArmed: ticket.Armed}
	if err := cleanupVaultBookHold(accounts); err != nil {
		return evidence, state, err
	}
	if err := cleanupLaneResidueHold(accounts); err != nil {
		return evidence, state, err
	}
	return evidence, state, nil
}

// compileCleanupWire reuses the production construction path end to end:
// bridgeExpectedEffects for the custody poststate, ComputeRouteNAVForRoute for
// the report, and CompileBridgeMessage for the exact unsigned legacy message.
func compileCleanupWire(ctx context.Context, rpc *RPCClient, manifest RouteManifest, route RuntimeRoute,
	accounts []ConfirmedAccount, slot int64, state cleanupCustodyState, step cleanupStep) (*cleanupWirePreparation, error) {
	if err := cleanupVaultBookHold(accounts); err != nil {
		return nil, err
	}
	if err := cleanupLaneResidueHold(accounts); err != nil {
		return nil, err
	}
	decision := Decision{Action: step.Action, Reason: step.Reason, AmountRaw: int64(step.AmountRaw),
		StrategyKey: route.Lane, IdempotencyKey: "manual-pilot-cleanup-wire"}
	custodies, err := decodeRouteNAVCustodiesForRoute(accounts, route)
	if err != nil {
		return nil, err
	}
	if custodies.VoltrIdleRaw != state.IdleRaw || custodies.StrategyUSDCraw != state.StrategyRaw || custodies.SquadsUSDCraw != state.SquadsRaw {
		return nil, budgetHold("cleanup_custody_decode_disagreement")
	}
	post := custodies
	if step.Action != ReportNAV {
		effects, strategyAfter, squadsAfter, err := bridgeExpectedEffects(decision, custodies.VoltrIdleRaw, custodies.StrategyUSDCraw, custodies.SquadsUSDCraw)
		if err != nil {
			return nil, err
		}
		_ = effects
		post.StrategyUSDCraw = strategyAfter
		post.SquadsUSDCraw = squadsAfter
		if step.Action == VoltrRestoreIdle {
			post.VoltrIdleRaw += step.AmountRaw
		}
	}
	navAccounts, err := selectRouteNAVAccountsForRoute(accounts, route)
	if err != nil {
		return nil, err
	}
	nav, err := ComputeRouteNAVForRoute(slot, navAccounts, manifest, &post, route)
	if err != nil {
		return nil, err
	}
	ticket, err := decodeObservedReportTicket(accountAt(accounts, reportTicketPDA))
	if err != nil {
		return nil, err
	}
	if nav.Report.Sequence <= ticket.LastConsumedSequence {
		return nil, budgetHold("cleanup_ticket_sequence_stale")
	}
	blockhash, err := rpc.LatestBlockhash(ctx)
	if err != nil {
		return nil, budgetHold("cleanup_blockhash_unavailable")
	}
	request := BridgeBuildRequest{Action: step.Action, AmountRaw: step.AmountRaw, Report: nav.Report,
		AdaptorConfig: bridgeStrategy, Settings: bridgeSettings,
		RecentBlockhash: blockhash.Blockhash, LastValidBlockHeight: blockhash.LastValidBlockHeight}
	message, err := CompileBridgeMessage(request)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(message)
	wire := &cleanupWirePreparation{Order: step.Order, Action: string(step.Action), Reason: step.Reason,
		AmountRaw: step.AmountRaw, ReportSequence: nav.Report.Sequence, ReportObservedSlot: nav.Report.ObservedSlot,
		NAVAfterRaw: nav.Report.NAVAfterRaw, SnapshotDigest: nav.Report.SnapshotDigest,
		RecentBlockhash: request.RecentBlockhash, LastValidBlockHeight: request.LastValidBlockHeight,
		UnsignedMessageBase64: base64.StdEncoding.EncodeToString(message), UnsignedMessageSHA256: hex.EncodeToString(digest[:]),
		PostIdleRaw: post.VoltrIdleRaw, PostStrategyRaw: post.StrategyUSDCraw, PostSquadsRaw: post.SquadsUSDCraw}
	if step.Action != StageSquadsToVoltr {
		if returnData := expectedAdaptorReturnData(nav.Report.NAVAfterRaw); returnData != nil {
			wire.ExpectedAdaptorReturnDataBase64 = returnData.DataBase64
		}
	}
	return wire, nil
}
