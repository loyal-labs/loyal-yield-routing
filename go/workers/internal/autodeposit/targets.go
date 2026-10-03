package autodeposit

// ExecutableTarget is one unit of dispatch for a scan: either a fresh
// scheduled slot or an existing claim whose pull already has a durable attempt.
type ExecutableTarget struct {
	TargetID        int64
	ScheduledSlotID int64
	// ClaimToken is non-empty for a recovery row: an existing claim to resume.
	ClaimToken string
}

func (t ExecutableTarget) isRecovery() bool { return t.ClaimToken != "" }

// PrioritizeExecutableTargets orders a scan's dispatch list.
//
// Recovery rows come first: each is an existing claim whose pull already moved
// funds, and dropping one would starve it forever because the sort order is
// fixed. Realtime hints rank next, newest requested slot first. Fresh targets
// then keep their SQL ordering.
//
// Only one dispatch per target per scan: the SQL guard against a selected claim
// is evaluated before any executor runs, so a target with several eligible slots
// would otherwise spawn several pulls in flight against one vault and break the
// exclusive-custody assumption in deposit recovery. Recovery rows are never
// dropped, and seeding the seen-set with them makes a fresh slot lose to a
// recovery claim on the same target.
func PrioritizeExecutableTargets(targets []ExecutableTarget, hintedSlotIDs []int64, limit int) []ExecutableTarget {
	hintRank := make(map[int64]int, len(hintedSlotIDs))
	for index, slotID := range hintedSlotIDs {
		if _, seen := hintRank[slotID]; !seen {
			hintRank[slotID] = index
		}
	}
	stableSortStable(targets, func(a, b ExecutableTarget) bool {
		if a.isRecovery() != b.isRecovery() {
			return a.isRecovery()
		}
		return hintPosition(hintRank, a.ScheduledSlotID) < hintPosition(hintRank, b.ScheduledSlotID)
	})

	seenTargets := make(map[int64]struct{}, len(targets))
	for _, target := range targets {
		if target.isRecovery() {
			seenTargets[target.TargetID] = struct{}{}
		}
	}
	kept := targets[:0]
	for _, target := range targets {
		if target.isRecovery() {
			kept = append(kept, target)
			continue
		}
		if _, seen := seenTargets[target.TargetID]; seen {
			continue
		}
		seenTargets[target.TargetID] = struct{}{}
		kept = append(kept, target)
	}
	if limit >= 0 && len(kept) > limit {
		kept = kept[:limit]
	}
	return kept
}

// stableSortStable is insertion sort with equality stability, keeping the port
// free of extra dependencies while preserving input order for equal keys.
func stableSortStable[T any](items []T, less func(a, b T) bool) {
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && less(items[j], items[j-1]); j-- {
			items[j], items[j-1] = items[j-1], items[j]
		}
	}
}

// hintPosition ranks a hinted slot by its arrival position and puts every
// unhinted slot behind all hinted ones instead of in front of them.
func hintPosition(hintRank map[int64]int, scheduledSlotID int64) int {
	if position, hinted := hintRank[scheduledSlotID]; hinted {
		return position
	}
	return int(^uint(0) >> 1)
}

// SlotHintQueue is the bounded, deduplicated FIFO of realtime requested-slot
// wakeups. Capacity overflow evicts the oldest hint; durable polling still
// covers everything it drops.
type SlotHintQueue struct {
	capacity int
	queued   map[int64]struct{}
	slotIDs  []int64
	head     int
}

func NewSlotHintQueue(capacity int) *SlotHintQueue {
	if capacity < 1 {
		capacity = 1
	}
	return &SlotHintQueue{capacity: capacity, queued: make(map[int64]struct{}, capacity)}
}

// Push enqueues a slot wakeup. Non-positive ids and duplicates are ignored.
func (q *SlotHintQueue) Push(slotID int64) {
	if slotID <= 0 {
		return
	}
	if _, seen := q.queued[slotID]; seen {
		return
	}
	if len(q.slotIDs)-q.head == q.capacity {
		evicted := q.slotIDs[q.head]
		delete(q.queued, evicted)
		q.head++
		if q.head == len(q.slotIDs) {
			q.slotIDs = q.slotIDs[:0]
			q.head = 0
		}
	}
	q.slotIDs = append(q.slotIDs, slotID)
	q.queued[slotID] = struct{}{}
}

// Drain removes and returns up to limit hints in arrival order.
func (q *SlotHintQueue) Drain(limit int) []int64 {
	available := len(q.slotIDs) - q.head
	if limit > available {
		limit = available
	}
	drained := make([]int64, 0, limit)
	for i := 0; i < limit; i++ {
		slotID := q.slotIDs[q.head]
		q.head++
		delete(q.queued, slotID)
		drained = append(drained, slotID)
	}
	if q.head == len(q.slotIDs) {
		q.slotIDs = q.slotIDs[:0]
		q.head = 0
	}
	return drained
}

// Len reports queued hint count.
func (q *SlotHintQueue) Len() int { return len(q.slotIDs) - q.head }
