package multiply

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
