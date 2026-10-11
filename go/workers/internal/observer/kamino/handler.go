package kamino

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	pb "github.com/helius-labs/laserstream-sdk/go/proto"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	klend "github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
	"github.com/mr-tron/base58"
	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"
)

type snapshotRank struct {
	slot, writeVersion uint64
	snapshot           Snapshot
}
type Handler struct {
	store     *Store
	rpc       *chain.Client
	logger    *slog.Logger
	storeRaw  bool
	mu        sync.RWMutex
	targets   map[string]Target
	snapshots map[string]snapshotRank
	// slotsPerSecond is the chain's last measured slot rate, set by the
	// runtime once a sample period; zero until one is read.
	slotsPerSecond float64
	skipped        map[string]bool // reserve and reason already logged
}

type HandleOutcome struct {
	Slot                uint64
	Inserted, Malformed bool
}

func NewHandler(store *Store, rpc *chain.Client, logger *slog.Logger, storeRaw bool) *Handler {
	return &Handler{store: store, rpc: rpc, logger: logger, storeRaw: storeRaw, targets: make(map[string]Target), snapshots: make(map[string]snapshotRank), skipped: make(map[string]bool)}
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
	h.mu.RUnlock()
	if !ok {
		return HandleOutcome{Slot: accountUpdate.GetSlot()}, fmt.Errorf("kamino update for unknown reserve %s", reserve)
	}
	observedAt := time.Now().UTC()
	owner, ownerErr := bytesKey(account.GetOwner())
	if ownerErr != nil || owner != klend.ProgramID.String() {
		h.logger.Error("invalid Kamino stream owner skipped", "reserve", reserve, "slot", accountUpdate.GetSlot(), "owner", owner)
		return HandleOutcome{Slot: accountUpdate.GetSlot(), Malformed: true}, nil
	}
	snapshot, err := h.decode(target, accountUpdate.GetSlot(), observedAt, &chain.Account{Key: solana.PublicKeyFromBytes(account.GetPubkey()), Owner: solana.PublicKeyFromBytes(account.GetOwner()), Lamports: account.GetLamports(), Data: account.GetData(), Executable: account.GetExecutable()})
	if err != nil {
		return HandleOutcome{Slot: accountUpdate.GetSlot(), Malformed: h.skip(reserve, accountUpdate.GetSlot(), err)}, nil
	}
	outcome, err := h.record(ctx, target, snapshot, "laserstream_grpc", account.GetData(), accountUpdate.GetSlot(), account.GetWriteVersion(), observedAt)
	if err != nil {
		return HandleOutcome{}, err
	}
	return HandleOutcome{Slot: accountUpdate.GetSlot(), Inserted: outcome.Inserted}, nil
}

// Seed reads targets once at confirmed commitment and inserts each as an
// http_snapshot row, the state the stream continues from: every target at
// start-up, each added one when the watched set changes. It returns the
// lowest slot read.
func (h *Handler) Seed(ctx context.Context, targets []Target) (uint64, error) {
	if len(targets) == 0 {
		return 0, fmt.Errorf("kamino target catalog is empty")
	}
	minimum := uint64(^uint64(0))
	for start := 0; start < len(targets); start += 100 {
		batch := targets[start:min(start+100, len(targets))]
		addresses := make([]solana.PublicKey, len(batch))
		for index, target := range batch {
			address, err := solana.PublicKeyFromBase58(target.Reserve)
			if err != nil {
				return 0, fmt.Errorf("seed Kamino reserve %s: %w", target.Reserve, err)
			}
			addresses[index] = address
		}
		slot, accounts, err := h.rpc.Accounts(ctx, addresses, rpc.CommitmentConfirmed, 0)
		if err != nil {
			return 0, fmt.Errorf("seed Kamino accounts: %w", err)
		}
		minimum = min(minimum, slot)
		observedAt := time.Now().UTC()
		for index, account := range accounts {
			target := batch[index]
			if account == nil || account.Owner != klend.ProgramID {
				h.logger.Error("confirmed Kamino seed account is missing or foreign", "reserve", target.Reserve)
				continue
			}
			snapshot, err := h.decode(target, slot, observedAt, account)
			if err != nil {
				h.skip(target.Reserve, slot, err)
				continue
			}
			if _, err := h.record(ctx, target, snapshot, "http_snapshot", account.Data, slot, 0, observedAt); err != nil {
				return 0, err
			}
		}
	}
	return minimum, nil
}

// SetSlotRate gives the handler the chain's measured slot rate.
func (h *Handler) SetSlotRate(slotsPerSecond float64) {
	h.mu.Lock()
	h.slotsPerSecond = slotsPerSecond
	h.mu.Unlock()
}

// decode is Decode at the last measured slot rate.
func (h *Handler) decode(target Target, slot uint64, observedAt time.Time, account *chain.Account) (Snapshot, error) {
	h.mu.RLock()
	slotsPerSecond := h.slotsPerSecond
	h.mu.RUnlock()
	return Decode(target, slot, observedAt, account, slotsPerSecond)
}

// skip drops a reserve row the observer cannot store and reports whether its
// data was malformed. A row it cannot price yet (a Legacy-basis reserve before
// any slot rate is measured) or at all (interest terms with no model) is
// logged once per reserve and reason; malformed data, every time.
func (h *Handler) skip(reserve string, slot uint64, err error) bool {
	reason := "unsupported_interest_terms"
	switch {
	case errors.Is(err, klend.ErrSlotRateUnavailable):
		reason = "slot_rate_unavailable"
	case !errors.Is(err, klend.ErrUnsupportedInterestTerms):
		h.logger.Error("invalid Kamino reserve data skipped", "reserve", reserve, "slot", slot, "error", err)
		return true
	}
	h.mu.Lock()
	first := !h.skipped[reserve+"\x00"+reason]
	h.skipped[reserve+"\x00"+reason] = true
	h.mu.Unlock()
	if first {
		h.logger.Warn("Kamino reserve rows skipped", "reserve", reserve, "reason", reason, "error", err)
	}
	return false
}

// record inserts one confirmed reserve state with its diff against the newest
// state this handler holds, then holds it when it is newer.
func (h *Handler) record(ctx context.Context, target Target, snapshot Snapshot, source string, data []byte, slot, writeVersion uint64, receivedAt time.Time) (InsertOutcome, error) {
	decodedAt := time.Now().UTC()
	h.mu.RLock()
	previous, hasPrevious := h.snapshots[target.Reserve]
	h.mu.RUnlock()
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
	var raw *string
	if h.storeRaw {
		value := base64.StdEncoding.EncodeToString(data)
		raw = &value
	}
	outcome, err := h.store.Insert(ctx, Record{Target: target, Snapshot: snapshot, Diff: diff, DiffSummary: summary, Source: source, SourceCommitment: "confirmed", AccountHash: accountHash(data), RawBase64: raw, ReceivedAt: receivedAt, DecodedAt: decodedAt, ReceiveToDecodeMS: decodedAt.Sub(receivedAt).Milliseconds()})
	if err != nil {
		return InsertOutcome{}, err
	}
	h.mu.Lock()
	current, exists := h.snapshots[target.Reserve]
	if !exists || slot > current.slot || (slot == current.slot && writeVersion >= current.writeVersion) {
		h.snapshots[target.Reserve] = snapshotRank{slot, writeVersion, snapshot}
	}
	h.mu.Unlock()
	return outcome, nil
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
