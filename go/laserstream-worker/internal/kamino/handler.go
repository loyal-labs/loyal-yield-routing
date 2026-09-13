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
	"github.com/loyal-labs/loyal-yield-routing/go/laserstream-worker/internal/solanarpc"
	"github.com/mr-tron/base58"
)

const klendProgram = "KLend2g3cP87fffoy8q1mQqGKjrxjC8boSyAYavgmjD"

type snapshotRank struct {
	slot, writeVersion uint64
	snapshot           Snapshot
}
type Handler struct {
	store          *Store
	rpc            *solanarpc.Client
	logger         *slog.Logger
	slotDurationMS float64
	storeRaw       bool
	mu             sync.RWMutex
	targets        map[string]Target
	snapshots      map[string]snapshotRank
}

type HandleOutcome struct {
	Slot                uint64
	Inserted, Malformed bool
}

func NewHandler(store *Store, rpc *solanarpc.Client, logger *slog.Logger, slotDurationMS float64, storeRaw bool) *Handler {
	return &Handler{store: store, rpc: rpc, logger: logger, slotDurationMS: slotDurationMS, storeRaw: storeRaw, targets: make(map[string]Target), snapshots: make(map[string]snapshotRank)}
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
		h.logger.Error("invalid Kamino stream owner fenced", "reserve", reserve, "slot", accountUpdate.GetSlot(), "owner", owner)
		return HandleOutcome{Slot: accountUpdate.GetSlot(), Malformed: true}, nil
	}
	decodedAt := time.Now().UTC()
	snapshot, err := Decode(target, accountUpdate.GetSlot(), observedAt, account.GetData(), slotDurationMS)
	if err != nil {
		if floorErr := h.store.RecordMalformed(ctx, reserve, accountUpdate.GetSlot(), observedAt); floorErr != nil {
			return HandleOutcome{}, fmt.Errorf("decode reserve: %v; record malformed floor: %w", err, floorErr)
		}
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
	return HandleOutcome{Slot: accountUpdate.GetSlot(), Inserted: outcome.Inserted}, nil
}

func (h *Handler) Seed(ctx context.Context) (uint64, error) { return h.verify(ctx, "http_snapshot") }
func (h *Handler) Verify(ctx context.Context) error {
	_, err := h.verify(ctx, "http_confirmed_refresh")
	return err
}
func (h *Handler) verify(ctx context.Context, source string) (uint64, error) {
	targets := h.Targets()
	h.mu.RLock()
	slotDurationMS := h.slotDurationMS
	h.mu.RUnlock()
	if len(targets) == 0 {
		return 0, fmt.Errorf("kamino target catalog is empty")
	}
	minimum := uint64(^uint64(0))
	for start := 0; start < len(targets); start += 100 {
		end := start + 100
		if end > len(targets) {
			end = len(targets)
		}
		addresses := make([]string, end-start)
		for index := start; index < end; index++ {
			addresses[index-start] = targets[index].Reserve
		}
		response, err := h.rpc.MultipleAccounts(ctx, addresses, "confirmed", nil)
		if err != nil {
			return 0, fmt.Errorf("verify Kamino accounts: %w", err)
		}
		if response.Slot < minimum {
			minimum = response.Slot
		}
		type decodedState struct {
			target     Target
			snapshot   Snapshot
			hash       string
			raw        *string
			observedAt time.Time
		}
		decoded := make(map[string]decodedState, len(response.Accounts))
		verifications := make([]Verification, 0, len(response.Accounts))
		for index, account := range response.Accounts {
			target := targets[start+index]
			observedAt := time.Now().UTC()
			verification := Verification{Reserve: target.Reserve, VerifiedSlot: int64(response.Slot), VerifiedAt: observedAt, Commitment: "confirmed", Source: source}
			if account != nil {
				verification.AccountHash = accountHash(account.Data)
			}
			if account != nil && account.Owner == klendProgram {
				snapshot, decodeErr := Decode(target, response.Slot, observedAt, account.Data, slotDurationMS)
				if decodeErr == nil {
					verification.StateValid = true
					var raw *string
					if h.storeRaw {
						value := base64.StdEncoding.EncodeToString(account.Data)
						raw = &value
					}
					decoded[target.Reserve] = decodedState{target, snapshot, verification.AccountHash, raw, observedAt}
				} else {
					h.logger.Error("confirmed Kamino state was malformed", "reserve", target.Reserve, "error", decodeErr)
				}
			}
			verifications = append(verifications, verification)
		}
		verificationOutcome, err := h.store.VerifyStates(ctx, verifications)
		if err != nil {
			return 0, err
		}
		for reserve, state := range decoded {
			if _, matched := verificationOutcome.Matched[reserve]; matched {
				h.admitSnapshot(reserve, response.Slot, state.snapshot)
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
				value := Compare(previous.snapshot, state.snapshot)
				diff = &value
				if value.Changed {
					summary = joinFields(value.ChangedFields)
				} else {
					summary = "none"
				}
			}
			outcome, insertErr := h.store.Insert(ctx, Record{Target: state.target, Snapshot: state.snapshot, Diff: diff, DiffSummary: summary, Source: source, SourceCommitment: "confirmed", AccountHash: state.hash, RawBase64: state.raw, ReceivedAt: state.observedAt, DecodedAt: time.Now().UTC()})
			if insertErr != nil {
				return 0, insertErr
			}
			if outcome.CurrentStateAdmitted && outcome.VerificationAdmitted {
				h.admitSnapshot(reserve, response.Slot, state.snapshot)
			}
		}
	}
	return minimum, nil
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
