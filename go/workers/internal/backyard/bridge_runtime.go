package backyard

import (
	"context"
	"errors"
	"fmt"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
)

// BridgeExecutionEvidence is the complete confirmed input to the exact bridge
// build boundary. The production bridge observer produces it only from the
// enriched snapshot, pinned adaptor config, policy bytes, obligation, and
// custody set.
type BridgeExecutionEvidence struct {
	Request         BridgeBuildRequest
	ExpectedEffects ExpectedEffects
}

// BuildSimulateAndPersistBridge is the only bridge construction sequence. It
// creates exact signed bytes, journals the message before simulation, then
// journals simulation and the same signed bytes before broadcast intent. Send,
// confirmation, and reconciliation continue through AdvanceNonterminal.
func BuildSimulateAndPersistBridge(
	ctx context.Context,
	database *Database,
	rpc *chain.Client,
	view *View,
	operationID string,
	evidence BridgeExecutionEvidence,
	credentials Credentials,
) error {
	if database == nil || rpc == nil || operationID == "" {
		return fmt.Errorf("bridge runtime dependencies are required")
	}
	if _, _, err := ticketedBridgeInstructions(evidence.Request); err != nil {
		return err
	}
	encodedEffects, err := jsonMarshalExpectedEffects(evidence.ExpectedEffects)
	if err != nil {
		return err
	}
	if _, err := DecodeExpectedEffects(encodedEffects); err != nil {
		return err
	}
	if err := database.requireBoundIntent(ctx, operationID, evidence.Request, encodedEffects); err != nil {
		return err
	}
	if err := validateBuildPrestate(ctx, rpc, view, evidence.Request, evidence.ExpectedEffects); err != nil {
		return err
	}
	signer, err := credentials.signer()
	if err != nil {
		return err
	}
	signed, err := BuildAndSignBridgeTransaction(evidence.Request, signer)
	if err != nil {
		return err
	}
	if err := database.MarkBuilt(ctx, operationID, signed.messageSHA256, encodedEffects); err != nil {
		return err
	}
	simulation, err := simulateSigned(ctx, rpc, signed.signedWire)
	if err != nil {
		var limitErr *SquadsSpendingLimitError
		if errors.As(err, &limitErr) {
			// The Squads policy refused the wire before broadcast because its
			// embedded spending limit is exhausted: nothing moved, and the
			// limit self-heals at its period boundary. Journal the refusal
			// under its own reason and hand the tick a named hold so the leg
			// is skipped and retried after the interval instead of failing
			// the process on a generic simulation error.
			if markErr := database.MarkPreBroadcastFailed(ctx, operationID, Built, squadsSpendingLimitReason); markErr != nil {
				return errors.Join(err, markErr)
			}
			return journaledBudgetHold(squadsSpendingLimitReason)
		}
		var slotErr *ReportSlotSimulationError
		if errors.As(err, &slotErr) {
			// The adaptor refused the report's slot: past its age limit, or
			// ahead of a lagging simulation node's clock. Nothing moved, and a
			// fresh report is built next tick.
			if markErr := database.MarkPreBroadcastFailed(ctx, operationID, Built, reportExpiredInSimulationReason); markErr != nil {
				return errors.Join(err, markErr)
			}
			return journaledBudgetHold(reportExpiredInSimulationReason)
		}
		return err
	}
	if err := database.MarkSimulated(ctx, operationID, simulation); err != nil {
		return err
	}
	build, err := signed.BuildResult(simulation.Slot)
	if err != nil {
		return err
	}
	return database.PersistSigned(ctx, operationID, build)
}
