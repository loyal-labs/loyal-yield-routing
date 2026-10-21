package backyard

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
)

type KaminoInitializationReceipt struct {
	MessageSHA256    string           `json:"messageSha256"`
	SignedWireSHA256 string           `json:"signedWireSha256"`
	FeeLamports      uint64           `json:"feeLamports"`
	PreBalances      []uint64         `json:"preBalances"`
	PostBalances     []uint64         `json:"postBalances"`
	AccountReadSlot  int64            `json:"accountReadSlot"`
	Obligation       ConfirmedAccount `json:"obligation"`
}

func validateInitializationEffectsShape(e ExpectedEffects) error {
	if e.Schema != "loyal-backyard-rwa-expected-effects/v1" || e.Kind != "kamino-initialize" ||
		!e.Conserved || len(e.Accounts) != 0 || e.Initialization == nil || e.Deposit != nil || e.Repayment != nil || e.ReturnData != nil {
		return fmt.Errorf("invalid native initialization effects")
	}
	return nil
}

func (m RouteManifest) validateInitializationEffects(e ExpectedEffects) error {
	if err := validateInitializationEffectsShape(e); err != nil {
		return err
	}
	_, err := m.compileKaminoInitializationMessage(*e.Initialization)
	return err
}

func (m RouteManifest) reconcileKaminoInitialization(e ExpectedEffects, receipt ConfirmedTransactionEvidence) (Reconciliation, []byte, error) {
	if err := m.validateInitializationEffects(e); err != nil {
		return Reconciliation{}, nil, err
	}
	r, n := *e.Initialization, receipt.Initialization
	if !receipt.Finalized || receipt.Slot <= 0 || receipt.Signature == "" || n == nil ||
		len(receipt.PreTokenBalances) != 0 || len(receipt.PostTokenBalances) != 0 || receipt.ReturnData != nil ||
		n.AccountReadSlot < receipt.Slot || n.FeeLamports == 0 || n.FeeLamports > r.MaximumFeeLamports || !validSHA256(n.SignedWireSHA256) {
		return Reconciliation{}, nil, fmt.Errorf("native initialization receipt is incomplete")
	}
	message, err := m.compileKaminoInitializationMessage(r)
	if err != nil || n.MessageSHA256 != sha256Bytes(message) {
		return Reconciliation{}, nil, fmt.Errorf("native initialization message differs")
	}
	keys, err := messageKeys(message)
	count := len(keys)
	if err != nil || count < 3 || len(n.PreBalances) != count || len(n.PostBalances) != count {
		return Reconciliation{}, nil, fmt.Errorf("native initialization balances are incomplete")
	}
	route, _ := m.route(r.RouteLane)
	farms := map[string]bool{}
	for _, f := range obligationFarms(route) {
		farms[f.accounts.ObligationFarmUserState.String()] = true
	}
	seenPayer, seenVault, seenObligation, seenFarms := false, false, false, 0
	var paid, farmRent uint64
	for i := 0; i < count; i++ {
		address := encodeBase58(keys[i][:])
		before, after := n.PreBalances[i], n.PostBalances[i]
		if farms[address] {
			if before != 0 || after == 0 {
				return Reconciliation{}, nil, fmt.Errorf("initializer farm user state rent credit differs")
			}
			farmRent, seenFarms = farmRent+after, seenFarms+1
			continue
		}
		switch address {
		case backyardDelegate:
			if i != 0 || before < after {
				return Reconciliation{}, nil, fmt.Errorf("initializer network fee differs")
			}
			paid, seenPayer = before-after, true
		case m.vault.squads:
			if before < after || before-after != r.RentLamports {
				return Reconciliation{}, nil, fmt.Errorf("initializer vault rent debit differs")
			}
			seenVault = true
		case route.Kamino.Obligation:
			if before != 0 || after != r.RentLamports {
				return Reconciliation{}, nil, fmt.Errorf("initializer obligation rent credit differs")
			}
			seenObligation = true
		default:
			if before != after {
				return Reconciliation{}, nil, fmt.Errorf("initializer changed unrelated native balance")
			}
		}
	}
	if !seenPayer || !seenVault || !seenObligation || seenFarms != len(farms) {
		return Reconciliation{}, nil, fmt.Errorf("initializer native graph differs")
	}
	if paid != n.FeeLamports+farmRent {
		return Reconciliation{}, nil, fmt.Errorf("initializer network fee differs")
	}
	if err = m.validateInitializedKaminoObligation(r, n.Obligation); err != nil {
		return Reconciliation{}, nil, err
	}
	evidence, err := json.Marshal(map[string]any{"schema": "loyal-backyard-rwa-reconciled-effects/v1", "kind": "kamino-initialize",
		"source": "finalized-transaction-native-balances-and-account", "signature": receipt.Signature, "slot": receipt.Slot, "native": n})
	if err != nil {
		return Reconciliation{}, nil, err
	}
	return Reconciliation{ConfirmedSlot: receipt.Slot, EffectsSHA256: sha256Bytes(evidence), Conserved: true}, evidence, nil
}

func (m RouteManifest) observeFinalizedKaminoInitialization(ctx context.Context, rpc *chain.Client, r KaminoInitializationRequest, op PersistedOperation) (ConfirmedTransactionEvidence, error) {
	var out ConfirmedTransactionEvidence
	if rpc == nil {
		return out, fmt.Errorf("initializer RPC unavailable")
	}
	if err := m.validateInitializerDecision(op.Decision, r); err != nil {
		return out, err
	}
	message, err := m.compileKaminoInitializationMessage(r)
	if err != nil {
		return out, err
	}
	wire := op.SignedWire
	signed, signature, err := splitSignedWire(wire)
	if err != nil || allZero(signature) || !bytes.Equal(signed, message) || sha256Bytes(wire) != op.SignedWireSHA256 || encodeBase58(signature) != op.TransactionSignature ||
		op.RecentBlockhash != r.RecentBlockhash || op.LastValidBlockHeight != r.LastValidBlockHeight {
		return out, fmt.Errorf("initializer persisted wire differs")
	}
	receipt, err := finalizedReceipt(ctx, rpc, op.TransactionSignature)
	if err != nil {
		return out, err
	}
	if receipt.Slot == 0 || int64(receipt.Slot) != op.ConfirmedSlot || receipt.Err != nil {
		return out, fmt.Errorf("initializer finalized receipt differs")
	}
	if len(receipt.Pre) != 0 || len(receipt.Post) != 0 || !receipt.ReturnProgram.IsZero() {
		return out, fmt.Errorf("initializer returned unexpected token or return data")
	}
	if !bytes.Equal(receipt.Wire, wire) {
		return out, fmt.Errorf("initializer finalized wire differs")
	}
	route, _ := m.route(r.RouteLane)
	batch, err := finalizedAccounts(ctx, rpc, []string{route.Kamino.Obligation}, int64(receipt.Slot))
	if err != nil {
		return out, err
	}
	if len(batch.Accounts) != 1 {
		return out, fmt.Errorf("initializer finalized account unavailable")
	}
	out = ConfirmedTransactionEvidence{Finalized: true, Signature: op.TransactionSignature, Slot: int64(receipt.Slot),
		Initialization: &KaminoInitializationReceipt{MessageSHA256: sha256Bytes(message), SignedWireSHA256: op.SignedWireSHA256,
			FeeLamports: receipt.Fee, PreBalances: receipt.PreLamports, PostBalances: receipt.PostLamports, AccountReadSlot: batch.Slot, Obligation: batch.Accounts[0]}}
	return out, nil
}
