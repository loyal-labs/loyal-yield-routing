package fleet

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"sync"
	"time"
)

type SnapshotOwner struct {
	mu       sync.RWMutex
	expected map[string]struct{}
	current  *MarketSnapshot
}

func NewSnapshotOwner(identities ...ReserveIdentity) (*SnapshotOwner, error) {
	if len(identities) != 2 {
		return nil, fmt.Errorf("phase 1 requires exactly two reserves")
	}
	expected := make(map[string]struct{}, len(identities))
	for _, identity := range identities {
		if identity.Address == "" {
			return nil, fmt.Errorf("reserve address is required")
		}
		if _, duplicate := expected[identity.Address]; duplicate {
			return nil, fmt.Errorf("reserve identity is duplicated")
		}
		expected[identity.Address] = struct{}{}
	}
	return &SnapshotOwner{expected: expected}, nil
}

func (o *SnapshotOwner) Apply(slot int64, observedAt time.Time, states []ReserveState) (MarketSnapshot, bool, error) {
	if slot <= 0 || observedAt.IsZero() || len(states) != len(o.expected) {
		return MarketSnapshot{}, false, fmt.Errorf("complete confirmed reserve batch is required")
	}
	reserves := make(map[string]ReserveState, len(states))
	addresses := make([]string, 0, len(states))
	hasher := sha256.New()
	for _, state := range states {
		if state.Slot != slot {
			return MarketSnapshot{}, false, fmt.Errorf("mixed-slot reserve batch")
		}
		if _, ok := o.expected[state.Address]; !ok {
			return MarketSnapshot{}, false, fmt.Errorf("unexpected reserve %s", state.Address)
		}
		if _, duplicate := reserves[state.Address]; duplicate {
			return MarketSnapshot{}, false, fmt.Errorf("duplicate reserve %s", state.Address)
		}
		reserves[state.Address] = state
		addresses = append(addresses, state.Address)
	}
	sort.Strings(addresses)
	for _, address := range addresses {
		state := reserves[address]
		for _, value := range []string{state.Address, state.Market, state.Mint, state.DataHash} {
			hasher.Write([]byte{byte(len(value)), byte(len(value) >> 8)})
			hasher.Write([]byte(value))
		}
	}
	hash := hex.EncodeToString(hasher.Sum(nil))
	next := MarketSnapshot{Slot: slot, ObservedAt: observedAt.UTC(), Hash: hash, Reserves: reserves}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.current != nil {
		if slot < o.current.Slot {
			return cloneSnapshot(*o.current), false, fmt.Errorf("out-of-order confirmed slot %d behind %d", slot, o.current.Slot)
		}
		if slot == o.current.Slot {
			if hash != o.current.Hash {
				return cloneSnapshot(*o.current), false, fmt.Errorf("conflicting confirmed evidence at slot %d", slot)
			}
			return cloneSnapshot(*o.current), false, nil
		}
	}
	o.current = &next
	return cloneSnapshot(next), true, nil
}

func (o *SnapshotOwner) Current() (MarketSnapshot, bool) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	if o.current == nil {
		return MarketSnapshot{}, false
	}
	return cloneSnapshot(*o.current), true
}

func cloneSnapshot(input MarketSnapshot) MarketSnapshot {
	output := input
	output.Reserves = make(map[string]ReserveState, len(input.Reserves))
	for key, value := range input.Reserves {
		output.Reserves[key] = value
	}
	return output
}
