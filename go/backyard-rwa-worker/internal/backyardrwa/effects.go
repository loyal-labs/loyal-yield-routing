package backyardrwa

import "encoding/json"

func jsonMarshalExpectedEffects(expected ExpectedEffects) ([]byte, error) {
	return json.Marshal(expected)
}
