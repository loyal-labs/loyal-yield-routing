package kamino

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sync"
	"time"

	pb "github.com/helius-labs/laserstream-sdk/go/proto"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/mr-tron/base58"
	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"
)

const klendProgram = "KLend2g3cP87fffoy8q1mQqGKjrxjC8boSyAYavgmjD"

type snapshotRank struct {
	slot, writeVersion uint64
	snapshot           Snapshot
}
type Handler struct {
	store          *Store
	rpc            *chain.Client
	logger         *slog.Logger
	slotDurationMS float64
	storeRaw       bool
	mu             sync.RWMutex
	targets        map[string]Target
	snapshots      map[string]snapshotRank
	schedule       *verificationSchedule
}

type HandleOutcome struct {
	Slot                uint64
	Inserted, Malformed bool
}

func NewHandler(store *Store, rpc *chain.Client, logger *slog.Logger, slotDurationMS float64, storeRaw bool) *Handler {
	return &Handler{store: store, rpc: rpc, logger: logger, slotDurationMS: slotDurationMS, storeRaw: storeRaw, targets: make(map[string]Target), snapshots: make(map[string]snapshotRank), schedule: newVerificationSchedule()}
}
func (h *Handler) SetSlotDuration(value float64) {
	if value <= 0 {
		return
	}
	h.mu.Lock()
	h.slotDurationMS = value
	h.mu.Unlock()
}
func (h *Handler) SetTargets(targets []Target) {
	h.mu.Lock()
	defer h.mu.Unlock()
	next := make(map[string]Target, len(targets))
	for _, target := range targets {
		next[target.Reserve] = target
	}
	h.targets = next
}
func (h *Handler) Targets() []Target {
	h.mu.RLock()
	defer h.mu.RUnlock()
	targets := make([]Target, 0, len(h.targets))
	for _, target := range h.targets {
		targets = append(targets, target)
	}
	return targets
}

func (h *Handler) HandleAccount(ctx context.Context, update *pb.SubscribeUpdate) (HandleOutcome, error) {
	accountUpdate := update.GetAccount()
	if accountUpdate == nil || accountUpdate.GetAccount() == nil {
		return HandleOutcome{}, fmt.Errorf("kamino update omitted account payload")
	}
	account := accountUpdate.GetAccount()
	reserve, err := bytesKey(account.GetPubkey())
	if err != nil {
		return HandleOutcome{}, err
	}
	h.mu.RLock()
	target, ok := h.targets[reserve]
	previous, hasPrevious := h.snapshots[reserve]
	slotDurationMS := h.slotDurationMS
	h.mu.RUnlock()
	if !ok {
		return HandleOutcome{Slot: accountUpdate.GetSlot()}, fmt.Errorf("kamino update for unknown reserve %s", reserve)
	}
	observedAt := time.Now().UTC()
	owner, ownerErr := bytesKey(account.GetOwner())
	if ownerErr != nil || owner != klendProgram {
		if err := h.store.RecordMalformed(ctx, reserve, accountUpdate.GetSlot(), observedAt); err != nil {
			return HandleOutcome{}, err
		}
		h.schedule.markDirty(reserve)
		h.logger.Error("invalid Kamino stream owner fenced", "reserve", reserve, "slot", accountUpdate.GetSlot(), "owner", owner)
		return HandleOutcome{Slot: accountUpdate.GetSlot(), Malformed: true}, nil
	}
	decodedAt := time.Now().UTC()
	snapshot, err := Decode(target, accountUpdate.GetSlot(), observedAt, account.GetData(), slotDurationMS)
	if err != nil {
		if floorErr := h.store.RecordMalformed(ctx, reserve, accountUpdate.GetSlot(), observedAt); floorErr != nil {
			return HandleOutcome{}, fmt.Errorf("decode reserve: %v; record malformed floor: %w", err, floorErr)
		}
		h.schedule.markDirty(reserve)
		h.logger.Error("invalid Kamino stream data fenced", "reserve", reserve, "slot", accountUpdate.GetSlot(), "error", err)
		return HandleOutcome{Slot: accountUpdate.GetSlot(), Malformed: true}, nil
	}
	var diff *Diff
	summary := "initial_snapshot"
	if hasPrevious {
		value := Compare(previous.snapshot, snapshot)
		diff = &value
		if value.Changed {
			summary = joinFields(value.ChangedFields)
		} else {
			summary = "none"
		}
	}
	hash := accountHash(account.GetData())
	var raw *string
	if h.storeRaw {
		value := base64.StdEncoding.EncodeToString(account.GetData())
		raw = &value
	}
	outcome, err := h.store.Insert(ctx, Record{Target: target, Snapshot: snapshot, Diff: diff, DiffSummary: summary, Source: "laserstream_grpc", SourceCommitment: "confirmed", AccountHash: hash, RawBase64: raw, ReceivedAt: observedAt, DecodedAt: decodedAt, ReceiveToDecodeMS: decodedAt.Sub(observedAt).Milliseconds()})
	if err != nil {
		return HandleOutcome{}, err
	}
	h.mu.Lock()
	current, exists := h.snapshots[reserve]
	if !exists || accountUpdate.GetSlot() > current.slot || (accountUpdate.GetSlot() == current.slot && account.GetWriteVersion() > current.writeVersion) {
		h.snapshots[reserve] = snapshotRank{accountUpdate.GetSlot(), account.GetWriteVersion(), snapshot}
	}
	h.mu.Unlock()
	// Rust marks every durably persisted stream state dirty so a confirmed
	// read follows within one batch tick. That read inserts the
	// http_confirmed_refresh state the planner's verified view requires;
	// without it the stream floor evicts the reserve's verification.
	h.schedule.markDirty(reserve)
	return HandleOutcome{Slot: accountUpdate.GetSlot(), Inserted: outcome.Inserted}, nil
}

func (h *Handler) Seed(ctx context.Context) (uint64, error) {
	return h.verify(ctx, "http_snapshot", h.Targets(), nil)
}

// RequestSafetySweep queues every watched reserve for a confirmed read. It is
// Rust's confirmed_refresh_tick: only a sweep for quiet reserves, since every
// stream write already queued its own reserve.
func (h *Handler) RequestSafetySweep() {
	targets := h.Targets()
	reserves := make([]string, len(targets))
	for index, target := range targets {
		reserves[index] = target.Reserve
	}
	h.schedule.requestSafetySweep(reserves)
}

// VerifyDirty runs one confirmed refresh batch over the pending reserves, as
// Rust's dirty_verification_tick does. It reports false when nothing was due.
func (h *Handler) VerifyDirty(ctx context.Context) (bool, error) {
	batch, ok := h.schedule.begin()
	if !ok {
		return false, nil
	}
	h.mu.RLock()
	targets := make([]Target, 0, len(batch.generations))
	for _, reserve := range batch.reserves() {
		if target, watched := h.targets[reserve]; watched {
			targets = append(targets, target)
		}
	}
	h.mu.RUnlock()
	if len(targets) == 0 {
		h.schedule.completeSuccess(batch)
		return true, nil
	}
	_, err := h.verify(ctx, "http_confirmed_refresh", targets, &batch)
	return true, err
}

type confirmedState struct {
	target     Target
	account    *chain.Account
	slot       uint64
	observedAt time.Time
}

// verify is Rust's ConfirmedReserveVerifier::fetch followed by
// refresh_confirmed_snapshots. A batch from the dirty schedule admits only the
// reserves not marked dirty again while their read was in flight.
func (h *Handler) verify(ctx context.Context, source string, targets []Target, batch *verificationBatch) (uint64, error) {
	h.mu.RLock()
	slotDurationMS := h.slotDurationMS
	h.mu.RUnlock()
	if len(targets) == 0 {
		return 0, fmt.Errorf("kamino target catalog is empty")
	}
	minimum := uint64(^uint64(0))
	states := make([]confirmedState, 0, len(targets))
	for start := 0; start < len(targets); start += 100 {
		end := min(start+100, len(targets))
		var err error
		addresses := make([]solana.PublicKey, end-start)
		for index := start; index < end && err == nil; index++ {
			addresses[index-start], err = solana.PublicKeyFromBase58(targets[index].Reserve)
		}
		var slot uint64
		var accounts []*chain.Account
		if err == nil {
			slot, accounts, err = h.rpc.Accounts(ctx, addresses, rpc.CommitmentConfirmed, 0)
		}
		if err != nil {
			if batch != nil {
				h.schedule.completeFailure(*batch)
			}
			return 0, fmt.Errorf("verify Kamino accounts: %w", err)
		}
		minimum = min(minimum, slot)
		observedAt := time.Now().UTC()
		for index, account := range accounts {
			states = append(states, confirmedState{target: targets[start+index], account: account, slot: slot, observedAt: observedAt})
		}
	}
	var accepted map[string]struct{}
	if batch != nil {
		accepted = h.schedule.completeSuccess(*batch)
		current := states[:0]
		for _, state := range states {
			if _, ok := accepted[state.target.Reserve]; ok {
				current = append(current, state)
			}
		}
		states = current
	}
	if err := h.persistConfirmed(ctx, source, states, slotDurationMS); err != nil {
		if batch != nil {
			h.schedule.retry(accepted)
		}
		return 0, err
	}
	return minimum, nil
}

func (h *Handler) persistConfirmed(ctx context.Context, source string, states []confirmedState, slotDurationMS float64) error {
	if len(states) == 0 {
		return nil
	}
	type decodedState struct {
		state    confirmedState
		snapshot Snapshot
		hash     string
		raw      *string
	}
	decoded := make(map[string]decodedState, len(states))
	verifications := make([]Verification, 0, len(states))
	for _, state := range states {
		target, account := state.target, state.account
		verification := Verification{Reserve: target.Reserve, VerifiedSlot: int64(state.slot), VerifiedAt: state.observedAt, Commitment: "confirmed", Source: source}
		if account != nil {
			verification.AccountHash = accountHash(account.Data)
		}
		if account != nil && account.Owner.String() == klendProgram {
			snapshot, decodeErr := Decode(target, state.slot, state.observedAt, account.Data, slotDurationMS)
			if decodeErr == nil {
				verification.StateValid = true
				var raw *string
				if h.storeRaw {
					value := base64.StdEncoding.EncodeToString(account.Data)
					raw = &value
				}
				decoded[target.Reserve] = decodedState{state, snapshot, verification.AccountHash, raw}
			} else {
				h.logger.Error("confirmed Kamino state was malformed", "reserve", target.Reserve, "error", decodeErr)
			}
		}
		verifications = append(verifications, verification)
	}
	verificationOutcome, err := h.store.VerifyStates(ctx, verifications)
	if err != nil {
		return err
	}
	for reserve, value := range decoded {
		if _, matched := verificationOutcome.Matched[reserve]; matched {
			h.admitSnapshot(reserve, value.state.slot, value.snapshot)
			continue
		}
		if _, deferred := verificationOutcome.Deferred[reserve]; deferred {
			continue
		}
		h.mu.RLock()
		previous, hasPrevious := h.snapshots[reserve]
		h.mu.RUnlock()
		var diff *Diff
		summary := "initial_snapshot"
		if hasPrevious {
			compared := Compare(previous.snapshot, value.snapshot)
			diff = &compared
			if compared.Changed {
				summary = joinFields(compared.ChangedFields)
			} else {
				summary = "none"
			}
		}
		outcome, insertErr := h.store.Insert(ctx, Record{Target: value.state.target, Snapshot: value.snapshot, Diff: diff, DiffSummary: summary, Source: source, SourceCommitment: "confirmed", AccountHash: value.hash, RawBase64: value.raw, ReceivedAt: value.state.observedAt, DecodedAt: time.Now().UTC()})
		if insertErr != nil {
			return insertErr
		}
		if outcome.CurrentStateAdmitted && outcome.VerificationAdmitted {
			h.admitSnapshot(reserve, value.state.slot, value.snapshot)
		}
	}
	return nil
}

func (h *Handler) admitSnapshot(reserve string, slot uint64, snapshot Snapshot) {
	h.mu.Lock()
	current, exists := h.snapshots[reserve]
	if !exists || slot >= current.slot {
		h.snapshots[reserve] = snapshotRank{slot: slot, snapshot: snapshot}
	}
	h.mu.Unlock()
}

func bytesKey(data []byte) (string, error) {
	if len(data) != 32 {
		return "", fmt.Errorf("public key has %d bytes", len(data))
	}
	var key [32]byte
	copy(key[:], data)
	return base58.Encode(key[:]), nil
}
func accountHash(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }
func joinFields(fields []string) string {
	if len(fields) == 0 {
		return "none"
	}
	result := fields[0]
	for _, field := range fields[1:] {
		result += "," + field
	}
	return result
}
