package backyard

import (
	"context"
	"fmt"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
)

// This uses the same delegate, bind, simulation and durable-wire pipeline as
// the ordinary Kamino legs. A compiler alone never authorizes it.
func BuildSimulateAndPersistKaminoInitialization(ctx context.Context, database *Database, rpc *chain.Client, view *View, operationID string, manifest RouteManifest, request KaminoInitializationRequest, credentials Credentials) error {
	if database == nil || rpc == nil || operationID == "" {
		return fmt.Errorf("initializer runtime dependencies are required")
	}
	if err := manifest.validateBindings(); err != nil {
		return err
	}
	if err := manifest.validateInitializationRequest(request); err != nil {
		return err
	}
	effects := kaminoInitializationEffects(request)
	encoded, err := jsonMarshalExpectedEffects(effects)
	if err != nil {
		return err
	}
	if err = database.requireBoundIntent(ctx, operationID, request, encoded); err != nil {
		return err
	}
	if _, err = manifest.validateRequestPrestate(ctx, rpc, view, request, effects); err != nil {
		return err
	}
	signer, err := credentials.signer()
	if err != nil {
		return err
	}
	message, err := manifest.compileKaminoInitializationMessage(request)
	if err != nil {
		return err
	}
	signed, err := signMessage(signer, mustKey(bridgeDelegate), message, request.RecentBlockhash, request.LastValidBlockHeight)
	if err != nil {
		return err
	}
	if err = database.markBuiltOnManifest(ctx, manifest, operationID, signed.messageSHA256, encoded); err != nil {
		return err
	}
	simulation, err := simulateSigned(ctx, rpc, signed.signedWire)
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

// kaminoInitializationEffects is the initializer's whole effect graph: it
// creates the obligation and moves no token.
func kaminoInitializationEffects(r KaminoInitializationRequest) ExpectedEffects {
	return ExpectedEffects{Schema: "loyal-backyard-rwa-expected-effects/v1", Kind: "kamino-initialize", Conserved: true, Initialization: &r}
}
