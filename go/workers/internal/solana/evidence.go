package solana

import (
	"errors"
	"time"
)

// EvidenceRef identifies an observation, not merely the time a worker read it.
type EvidenceRef struct {
	Slot       uint64    `json:"slot"`
	ObservedAt time.Time `json:"observed_at"`
	Hash       string    `json:"hash"`
	Source     string    `json:"source"`
}

func (e EvidenceRef) Validate() error {
	if e.Slot == 0 || e.ObservedAt.IsZero() || e.Hash == "" || e.Source == "" {
		return errors.New("incomplete observation evidence")
	}
	return nil
}
