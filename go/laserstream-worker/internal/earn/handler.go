package earn

import (
	"context"
	"fmt"
	"math"
	"sort"
	"sync"

	pb "github.com/helius-labs/laserstream-sdk/go/proto"
	"github.com/loyal-labs/loyal-yield-routing/go/laserstream-worker/internal/watch"
	"github.com/mr-tron/base58"
)

const ConsumerNamePrefix = "earn-smart-account:"

var earnFilters = map[string]struct{}{watch.EarnSmartAccounts: {}, watch.EarnPolicyAccounts: {}, watch.EarnVaultAccounts: {}, watch.EarnIdleTokenAccounts: {}, watch.EarnWalletTokenAccounts: {}, watch.EarnObligations: {}, watch.EarnAutodepositWalletATAs: {}, watch.EarnSubscriptionAuthorities: {}, watch.EarnRecurringDelegations: {}, watch.EarnWallets: {}}

type NormalizedUpdate struct {
	EventKey      *string  `json:"event_key"`
	Filters       []string `json:"filters"`
	EventKind     string   `json:"event_kind"`
	AccountPubkey *string  `json:"account_pubkey"`
	Slot          uint64   `json:"slot"`
	Signature     *string  `json:"signature"`
}
type Handler struct {
	store    *Store
	consumer string
	mu       sync.RWMutex
	watch    *watch.Set
}

func NewHandler(store *Store, cluster string) *Handler {
	return &Handler{store: store, consumer: ConsumerNamePrefix + cluster}
}
func (h *Handler) SetWatchSet(set *watch.Set) { h.mu.Lock(); h.watch = set; h.mu.Unlock() }
func (h *Handler) ConsumerName() string       { return h.consumer }

func (h *Handler) HandleAccount(ctx context.Context, update *pb.SubscribeUpdate) (EnqueueOutcome, error) {
	accountUpdate := update.GetAccount()
	if accountUpdate == nil {
		return EnqueueOutcome{}, fmt.Errorf("earn update omitted account wrapper")
	}
	filters := make([]string, 0, len(update.Filters))
	for _, filter := range update.Filters {
		if _, ok := earnFilters[filter]; ok {
			filters = append(filters, filter)
		}
	}
	if len(filters) == 0 {
		return EnqueueOutcome{}, fmt.Errorf("earn handler received no Earn filters")
	}
	sort.Strings(filters)
	var accountPubkey, signature *string
	kind := "account_deleted"
	if account := accountUpdate.GetAccount(); account != nil {
		if len(account.GetPubkey()) != 32 {
			return EnqueueOutcome{}, fmt.Errorf("earn account pubkey has %d bytes", len(account.GetPubkey()))
		}
		value := base58.Encode(account.GetPubkey())
		accountPubkey = &value
		if account.GetLamports() > 0 {
			kind = "account"
		}
		if len(account.GetTxnSignature()) > 0 {
			value := base58.Encode(account.GetTxnSignature())
			signature = &value
		}
	}
	if accountUpdate.GetSlot() == 0 || accountUpdate.GetSlot() > math.MaxInt64 {
		return EnqueueOutcome{}, fmt.Errorf("earn update slot is invalid")
	}
	normalized := NormalizedUpdate{Filters: filters, EventKind: kind, AccountPubkey: accountPubkey, Slot: accountUpdate.GetSlot(), Signature: signature}
	h.mu.RLock()
	set := h.watch
	h.mu.RUnlock()
	if set == nil {
		return EnqueueOutcome{}, fmt.Errorf("earn watch set is not initialized")
	}
	account := ""
	if accountPubkey != nil {
		account = *accountPubkey
	}
	affected := set.AffectedVaults(account)
	if len(affected) == 0 {
		return EnqueueOutcome{}, fmt.Errorf("earn update at slot %d matched %v but no watched vault", normalized.Slot, filters)
	}
	eventKey := durableEventKey(normalized, affected)
	return h.store.Enqueue(ctx, h.consumer, eventKey, normalized.Slot, normalized, affected, account)
}

func durableEventKey(update NormalizedUpdate, vaults []watch.Vault) string {
	policyFilter := false
	for _, filter := range update.Filters {
		if filter == watch.EarnSmartAccounts || filter == watch.EarnPolicyAccounts || filter == watch.EarnWallets {
			policyFilter = true
		}
	}
	policyAccount := false
	if update.AccountPubkey != nil {
		for _, vault := range vaults {
			if *update.AccountPubkey == vault.Settings || *update.AccountPubkey == vault.Wallet {
				policyAccount = true
			}
			for _, account := range vault.Accounts {
				if account.Pubkey == *update.AccountPubkey && (account.Role == "smart_account" || account.Role == "policy") {
					policyAccount = true
				}
			}
		}
	}
	if policyFilter && policyAccount && update.Signature != nil {
		return fmt.Sprintf("policy-discovery:%d:%s", update.Slot, *update.Signature)
	}
	if update.EventKey != nil {
		return *update.EventKey
	}
	signature := "missing-signature"
	if update.Signature != nil {
		signature = *update.Signature
	}
	account := "missing-account"
	if update.AccountPubkey != nil {
		account = *update.AccountPubkey
	}
	return fmt.Sprintf("%s:%d:%s:%s", update.EventKind, update.Slot, signature, account)
}
