package backyard

import (
	"fmt"
)

func compileRequest(m RouteManifest, request any) (message []byte, blockhash string, height int64, err error) {
	delegate, v := mustKey(bridgeDelegate), m.vault
	switch r := request.(type) {
	case BridgeBuildRequest:
		message, err = compileBridgeMessageForDelegate(v, r, delegate)
		blockhash, height = r.RecentBlockhash, r.LastValidBlockHeight
	case KaminoPrimeUSDCRequest:
		message, err = compileKaminoMessageForDelegate(v, r, delegate)
		blockhash, height = r.RecentBlockhash, r.LastValidBlockHeight
	case KaminoInitializationRequest:
		message, err = m.compileKaminoInitializationMessage(r)
		blockhash, height = r.RecentBlockhash, r.LastValidBlockHeight
	case JupiterSwapRequest:
		message, err = compileJupiterMessageForDelegate(v, r, delegate)
		blockhash, height = r.RecentBlockhash, r.LastValidBlockHeight
	case HastraConversionRequest:
		message, err = compileHastraMessageForDelegate(v, r, delegate)
		blockhash, height = r.RecentBlockhash, r.LastValidBlockHeight
	case StepRequest:
		message, err = compileStepMessageForDelegate(v, r, delegate)
		blockhash, height = r.Open.RecentBlockhash, r.Open.LastValidBlockHeight
	default:
		err = fmt.Errorf("request %T has no compiler", request)
	}
	return message, blockhash, height, err
}
