package backyard

import (
	"encoding/json"
	"fmt"
)

// decodeExpectedEffectsWithManifest delegates every non-initializer shape to
// the public decoder unchanged; initializer effects revalidate through the
// SAME explicit manifest that compiled the request.
func decodeExpectedEffectsWithManifest(m RouteManifest, data []byte) (ExpectedEffects, error) {
	var envelope struct {
		Schema          string          `json:"schema"`
		ExpectedEffects json.RawMessage `json:"expectedEffects"`
	}
	raw := data
	if json.Unmarshal(data, &envelope) == nil && envelope.Schema == "loyal-backyard-rwa-operation-evidence/v1" {
		if len(envelope.ExpectedEffects) == 0 || string(envelope.ExpectedEffects) == "null" {
			return ExpectedEffects{}, fmt.Errorf("operation has no built expected effects")
		}
		raw = envelope.ExpectedEffects
	}
	var expected ExpectedEffects
	if json.Unmarshal(raw, &expected) != nil {
		return DecodeExpectedEffects(data)
	}
	if expected.Kind != "kamino-initialize" && expected.Initialization == nil {
		return DecodeExpectedEffects(data)
	}
	if err := m.validateInitializationEffects(expected); err != nil {
		return ExpectedEffects{}, err
	}
	return expected, nil
}

func compileRequest(m RouteManifest, request any) (message []byte, blockhash string, height int64, err error) {
	delegate := mustKey(bridgeDelegate)
	switch r := request.(type) {
	case BridgeBuildRequest:
		message, err = compileBridgeMessageForDelegate(r, delegate)
		blockhash, height = r.RecentBlockhash, r.LastValidBlockHeight
	case KaminoPrimeUSDCRequest:
		message, err = compileKaminoMessageForDelegate(r, delegate)
		blockhash, height = r.RecentBlockhash, r.LastValidBlockHeight
	case KaminoInitializationRequest:
		message, err = m.compileKaminoInitializationMessage(r)
		blockhash, height = r.RecentBlockhash, r.LastValidBlockHeight
	case JupiterSwapRequest:
		message, err = compileJupiterMessageForDelegate(r, delegate)
		blockhash, height = r.RecentBlockhash, r.LastValidBlockHeight
	case HastraConversionRequest:
		message, err = compileHastraMessageForDelegate(r, delegate)
		blockhash, height = r.RecentBlockhash, r.LastValidBlockHeight
	case StepRequest:
		message, err = compileStepMessageForDelegate(r, delegate)
		blockhash, height = r.Open.RecentBlockhash, r.Open.LastValidBlockHeight
	default:
		err = fmt.Errorf("request %T has no compiler", request)
	}
	return message, blockhash, height, err
}
