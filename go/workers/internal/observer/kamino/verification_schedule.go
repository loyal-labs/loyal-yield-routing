package kamino

import (
	"sort"
	"sync"
)

// verificationSchedule is Rust's DirtyReserveVerificationSchedule
// (kamino-reserve-monitor verification_schedule.rs). A durable stream write
// marks its reserve dirty, the periodic timer only requests a safety sweep for
// quiet reserves, and at most one confirmed refresh batch is in flight. A
// reserve marked dirty again while its batch was in flight keeps its newer
// generation: the older confirmed read is discarded and the reserve stays
// pending, so the latest stream state is the one that gets verified.
type verificationSchedule struct {
	mu          sync.Mutex
	generations map[string]uint64
	pending     map[string]struct{}
	inFlight    bool
}

type verificationBatch struct{ generations map[string]uint64 }

func newVerificationSchedule() *verificationSchedule {
	return &verificationSchedule{generations: make(map[string]uint64), pending: make(map[string]struct{})}
}

func (b verificationBatch) reserves() []string {
	reserves := make([]string, 0, len(b.generations))
	for reserve := range b.generations {
		reserves = append(reserves, reserve)
	}
	sort.Strings(reserves)
	return reserves
}

func (s *verificationSchedule) markDirty(reserve string) {
	s.mu.Lock()
	s.generations[reserve]++
	s.pending[reserve] = struct{}{}
	s.mu.Unlock()
}

func (s *verificationSchedule) requestSafetySweep(reserves []string) {
	s.mu.Lock()
	for _, reserve := range reserves {
		if _, exists := s.generations[reserve]; !exists {
			s.generations[reserve] = 0
		}
		s.pending[reserve] = struct{}{}
	}
	s.mu.Unlock()
}

func (s *verificationSchedule) begin() (verificationBatch, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inFlight || len(s.pending) == 0 {
		return verificationBatch{}, false
	}
	batch := verificationBatch{generations: make(map[string]uint64, len(s.pending))}
	for reserve := range s.pending {
		batch.generations[reserve] = s.generations[reserve]
	}
	s.pending = make(map[string]struct{})
	s.inFlight = true
	return batch, true
}

// completeSuccess returns the reserves whose confirmed read is still current.
func (s *verificationSchedule) completeSuccess(batch verificationBatch) map[string]struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inFlight = false
	accepted := make(map[string]struct{}, len(batch.generations))
	for reserve, requested := range batch.generations {
		if s.generations[reserve] != requested {
			continue
		}
		delete(s.pending, reserve)
		accepted[reserve] = struct{}{}
	}
	return accepted
}

func (s *verificationSchedule) completeFailure(batch verificationBatch) {
	s.mu.Lock()
	s.inFlight = false
	for reserve := range batch.generations {
		s.pending[reserve] = struct{}{}
	}
	s.mu.Unlock()
}

// retry re-queues accepted reserves whose persistence failed. Rust exits the
// process there and re-seeds; the Go runtime keeps running, so the reserves
// must not be forgotten until the next safety sweep.
func (s *verificationSchedule) retry(reserves map[string]struct{}) {
	s.mu.Lock()
	for reserve := range reserves {
		s.pending[reserve] = struct{}{}
	}
	s.mu.Unlock()
}
