package backyardrwa

import (
	"context"
	"crypto/ed25519"
	"fmt"
)

// This uses the same delegate, reservation, simulation and durable-wire
// pipeline as the ordinary Kamino legs. A compiler alone never authorizes it.
func BuildSimulateAndPersistKaminoInitialization(ctx context.Context, database *Database, rpc *RPCClient, operationID string, manifest RouteManifest, request KaminoInitializationRequest) error {
	if database == nil || rpc == nil || operationID == "" {
		return fmt.Errorf("initializer runtime dependencies are required")
	}
	if err := manifest.validateBindings(); err != nil {
		return err
	}
	if err := manifest.validateInitializationRequest(request); err != nil {
		return err
	}
	effects := ExpectedEffects{Schema: "loyal-backyard-rwa-expected-effects/v1", Kind: "kamino-initialize", Conserved: true, Initialization: &request}
	encoded, err := jsonMarshalExpectedEffects(effects)
	if err != nil {
		return err
	}
	if err = manifest.authorizePhase3ProductionBuild(ctx, database, rpc, operationID, request, effects, encoded); err != nil {
		return err
	}
	signer, err := loadPinnedPolicySigner()
	if err != nil {
		return err
	}
	message, err := manifest.compileKaminoInitializationMessage(request)
	if err != nil {
		return err
	}
	signature := ed25519.Sign(signer, message)
	wire := append(encodeShortVec(1), signature...)
	wire = append(wire, message...)
	signed := SignedKaminoTransaction{message: message, signedWire: wire, messageSHA256: sha256Bytes(message), signedWireSHA256: sha256Bytes(wire), transactionSignature: encodeBase58(signature), recentBlockhash: request.RecentBlockhash, lastValidBlockHeight: request.LastValidBlockHeight}
	if err = database.markBuiltOnManifest(ctx, manifest, operationID, signed.messageSHA256, encoded); err != nil {
		return err
	}
	simulation, err := rpc.SimulateSignedTransaction(ctx, wire)
	if err != nil {
		return err
	}
	if err = database.MarkSimulated(ctx, operationID, simulation); err != nil {
		return err
	}
	build, err := signed.BuildResult(simulation.Slot)
	if err != nil {
		return err
	}
	return database.PersistSigned(ctx, operationID, build)
}
