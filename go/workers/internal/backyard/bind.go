package backyard

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// phase3BuildInput is one priced request of a selector recipe. Byte strings
// retain the exact canonical request/effects encoding through JSONB storage;
// JSON objects would lose their original key ordering and digest.
type phase3BuildInput struct {
	Kind    string `json:"kind"`
	Request []byte `json:"request"`
	Effects []byte `json:"effects"`
}

func encodePhase3BuildInput(request any, effects []byte) (*phase3BuildInput, error) {
	input := &phase3BuildInput{Effects: append([]byte(nil), effects...)}
	switch request.(type) {
	case BridgeBuildRequest:
		input.Kind = "bridge"
	case KaminoPrimeUSDCRequest:
		input.Kind = "kamino"
	case KaminoInitializationRequest:
		input.Kind = "kamino-initialize"
	case JupiterSwapRequest:
		input.Kind = "jupiter"
	case HastraConversionRequest:
		input.Kind = "hastra"
	case StepRequest:
		input.Kind = "step"
	default:
		return nil, budgetHold("unmapped_economic_action")
	}
	var err error
	input.Request, err = json.Marshal(request)
	if err != nil {
		return nil, err
	}
	return input, nil
}

// decodeWithManifest decodes a recipe request and compiles it as the build
// would (compileRequest).
func (input *phase3BuildInput) decodeWithManifest(m RouteManifest) (any, ExpectedEffects, []byte, error) {
	if input == nil {
		return nil, ExpectedEffects{}, nil, budgetHold("missing_persisted_build_input")
	}
	if !json.Valid(input.Request) {
		return nil, ExpectedEffects{}, nil, budgetHold("invalid_persisted_build_input")
	}
	var target any
	switch input.Kind {
	case "bridge":
		target = &BridgeBuildRequest{}
	case "kamino":
		target = &KaminoPrimeUSDCRequest{}
	case "kamino-initialize":
		target = &KaminoInitializationRequest{}
	case "jupiter":
		target = &JupiterSwapRequest{}
	case "hastra":
		target = &HastraConversionRequest{}
	case "step":
		target = &StepRequest{}
	default:
		return nil, ExpectedEffects{}, nil, budgetHold("invalid_persisted_build_input")
	}
	decoder := json.NewDecoder(bytes.NewReader(input.Request))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return nil, ExpectedEffects{}, nil, budgetHoldFrom(err, "invalid_persisted_build_input")
	}
	request := requestValue(target)
	message, err := compileRequestMessage(m, request)
	if err != nil {
		return nil, ExpectedEffects{}, nil, budgetHoldFrom(err, "persisted_build_no_longer_compiles")
	}
	effects, err := decodeExpectedEffectsWithManifest(m, input.Effects)
	if err != nil {
		return nil, ExpectedEffects{}, nil, budgetHoldFrom(err, "invalid_persisted_build_effects")
	}
	return request, effects, message, nil
}

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

// requestValue is the request a decode target points at.
func requestValue(target any) any {
	switch r := target.(type) {
	case *BridgeBuildRequest:
		return *r
	case *KaminoPrimeUSDCRequest:
		return *r
	case *KaminoInitializationRequest:
		return *r
	case *JupiterSwapRequest:
		return *r
	case *HastraConversionRequest:
		return *r
	case *StepRequest:
		return *r
	}
	return nil
}

// compileRequestMessage compiles one prepared request to the unsigned v1
// message the pinned delegate signs, with the blockhash and expiry it carries.
// Kamino, initializer, Jupiter, Hastra and step requests compile with the
// fixed bridge delegate; the delegate is never request-supplied.
func compileRequestMessage(m RouteManifest, request any) ([]byte, error) {
	message, _, _, err := compileRequest(m, request)
	return message, err
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
