package backyard

import (
	"fmt"
	"time"
)

func compileRequest(m RouteManifest, request any) (message []byte, blockhash string, height int64, err error) {
	delegate, v := mustKey(backyardDelegate), m.vault
	if a, required := requestAddAdmission(request); required {
		if err := validateAddAdmission(m, request, a, time.Now(), func() int64 {
			if a == nil {
				return 0
			}
			return a.net.slot
		}()); err != nil {
			return nil, "", 0, err
		}
	}
	switch r := request.(type) {
	case BridgeBuildRequest:
		if (r.Action != bridgeStageSquadsToVoltr || v.trustful()) && (r.net == nil || r.net.manifest != m.SHA256) {
			return nil, "", 0, staleHold("certified_nav_manifest_mismatch")
		}
		message, err = compileBridgeMessageForDelegate(v, r, delegate)
		blockhash, height = r.RecentBlockhash, r.LastValidBlockHeight
	case KaminoLegRequest:
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
