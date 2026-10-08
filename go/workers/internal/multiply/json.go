package multiply

// JSON helpers shared by the store and worker: persisted documents decode
// with unknown fields denied, byte-compatible with the Rust serde contract.

import (
	"bytes"
	"encoding/json"
	"errors"
)

func jsonMarshal(value any) ([]byte, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return encoded, nil
}

// jsonUnmarshalStrict mirrors serde deny_unknown_fields.
func jsonUnmarshalStrict(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if decoder.More() {
		return errors.New("trailing content after persisted document")
	}
	return nil
}
