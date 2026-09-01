package backyardrwa

import (
	"context"
	"fmt"
)

// BridgeExecutionEvidence is the complete confirmed input to the exact bridge
// build boundary. ObserveConfirmedBridgeExecutionEvidence produces it only
// from the pinned adaptor config, policy bytes, obligation, and custody set.
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
	rpc *RPCClient,
	operationID string,
	evidence BridgeExecutionEvidence,
) error {
	if database == nil || rpc == nil || operationID == "" {
		return fmt.Errorf("bridge runtime dependencies are required")
	}
	if _, _, _, err := ticketedBridgeInstructions(evidence.Request); err != nil {
		return err
	}
	encodedEffects, err := jsonMarshalExpectedEffects(evidence.ExpectedEffects)
	if err != nil {
		return err
	}
	if _, err := DecodeExpectedEffects(encodedEffects); err != nil {
		return err
	}
	signer, err := loadPinnedPolicySigner()
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
	simulation, err := rpc.SimulateSignedTransaction(ctx, signed.signedWire)
	if err != nil {
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
