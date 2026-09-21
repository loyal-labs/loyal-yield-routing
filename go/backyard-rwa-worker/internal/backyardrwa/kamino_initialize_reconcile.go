package backyardrwa

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
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

// validateInitializationEffectsShape is the exact structural check both effect
// validators share: only a conserved native kamino-initialize effect with no
// side accounts, deposits, repayments or return data is admissible.
func validateInitializationEffectsShape(e ExpectedEffects) error {
	if e.Schema != "loyal-backyard-rwa-expected-effects/v1" || e.Kind != "kamino-initialize" ||
		!e.Conserved || len(e.Accounts) != 0 || e.Initialization == nil || e.Deposit != nil || e.Repayment != nil || e.ReturnData != nil {
		return fmt.Errorf("invalid native initialization effects")
	}
	return nil
}

func validateInitializationEffects(e ExpectedEffects) error {
	if err := validateInitializationEffectsShape(e); err != nil {
		return err
	}
	_, err := CompileKaminoInitializationMessage(*e.Initialization)
	return err
}

// validateInitializationEffectsOnRoute is the manifest-aware form: identical
// structural checks, but the embedded admission request is recompiled through
// the manifest compiler, so an AUTO effect only validates when its request
// matches the reviewed binding. Installed selector lanes keep the exact public
// path; reconcile.go keeps calling the public function unchanged.
func (m RouteManifest) validateInitializationEffects(e ExpectedEffects) error {
	if err := validateInitializationEffectsShape(e); err != nil {
		return err
	}
	_, err := m.compileKaminoInitializationMessage(*e.Initialization)
	return err
}

func reconcileKaminoInitialization(e ExpectedEffects, receipt ConfirmedTransactionEvidence) (Reconciliation, []byte, error) {
	return reconcileKaminoInitializationAdmission(e, receipt, validateInitializationEffects, CompileKaminoInitializationMessage, validateInitializedKaminoObligation)
}

// reconcileKaminoInitialization is the manifest-aware variant: the exact
// installed reconciler body with only the three admission calls swapped to the
// explicit reviewed manifest, so a candidate AUTO effect reconciles against the
// same binding that compiled it. The public form above is unchanged.
func (m RouteManifest) reconcileKaminoInitialization(e ExpectedEffects, receipt ConfirmedTransactionEvidence) (Reconciliation, []byte, error) {
	return reconcileKaminoInitializationAdmission(e, receipt, m.validateInitializationEffects, m.compileKaminoInitializationMessage, m.validateInitializedKaminoObligation)
}

// reconcileKaminoInitializationAdmission is the shared initializer reconciler
// body; the installed and manifest forms differ only in the three functions
// that revalidate the effect, recompile the message and check the poststate.
func reconcileKaminoInitializationAdmission(e ExpectedEffects, receipt ConfirmedTransactionEvidence,
	validate func(ExpectedEffects) error,
	compile func(KaminoInitializationRequest) ([]byte, error),
	validateObligation func(KaminoInitializationRequest, ConfirmedAccount) error,
) (Reconciliation, []byte, error) {
	if err := validate(e); err != nil {
		return Reconciliation{}, nil, err
	}
	r, n := *e.Initialization, receipt.Initialization
	if !receipt.Finalized || receipt.Slot <= 0 || receipt.Signature == "" || n == nil ||
		len(receipt.PreTokenBalances) != 0 || len(receipt.PostTokenBalances) != 0 || receipt.ReturnData != nil ||
		n.AccountReadSlot < receipt.Slot || n.FeeLamports == 0 || n.FeeLamports > r.MaximumFeeLamports || !validSHA256(n.SignedWireSHA256) {
		return Reconciliation{}, nil, fmt.Errorf("native initialization receipt is incomplete")
	}
	message, err := compile(r)
	if err != nil || n.MessageSHA256 != sha256Bytes(message) {
		return Reconciliation{}, nil, fmt.Errorf("native initialization message differs")
	}
	offset := 3
	count, err := decodeShortVec(message, &offset)
	if err != nil || count < 3 || offset+count*32 > len(message) || len(n.PreBalances) != count || len(n.PostBalances) != count {
		return Reconciliation{}, nil, fmt.Errorf("native initialization balances are incomplete")
	}
	route, _ := runtimeRoute(r.RouteLane)
	seenPayer, seenVault, seenObligation := false, false, false
	for i := 0; i < count; i++ {
		address := keyString(message[offset+i*32 : offset+(i+1)*32])
		before, after := n.PreBalances[i], n.PostBalances[i]
		switch address {
		case bridgeDelegate:
			if i != 0 || before < after || before-after != n.FeeLamports {
				return Reconciliation{}, nil, fmt.Errorf("initializer network fee differs")
			}
			seenPayer = true
		case bridgeVault:
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
	if !seenPayer || !seenVault || !seenObligation {
		return Reconciliation{}, nil, fmt.Errorf("initializer native graph differs")
	}
	if err = validateObligation(r, n.Obligation); err != nil {
		return Reconciliation{}, nil, err
	}
	evidence, err := json.Marshal(map[string]any{"schema": "loyal-backyard-rwa-reconciled-effects/v1", "kind": "kamino-initialize",
		"source": "finalized-transaction-native-balances-and-account", "signature": receipt.Signature, "slot": receipt.Slot, "native": n})
	if err != nil {
		return Reconciliation{}, nil, err
	}
	return Reconciliation{ConfirmedSlot: receipt.Slot, EffectsSHA256: sha256Bytes(evidence), Conserved: true}, evidence, nil
}

// Recovery reads the immutable receipt for the persisted wire. Account presence
// alone never establishes which submission created it or authorizes a retry.
func observeFinalizedKaminoInitialization(ctx context.Context, rpc *RPCClient, r KaminoInitializationRequest, op PersistedOperation) (ConfirmedTransactionEvidence, error) {
	return observeFinalizedKaminoInitializationAdmission(ctx, rpc, r, op, CompileKaminoInitializationMessage, observeInitializerDecisionInstalled)
}

// observeInitializerDecisionInstalled is the exact installed journal-decision
// check the public observer has always applied.
func observeInitializerDecisionInstalled(d Decision, r KaminoInitializationRequest) error {
	if d.Action != InitializeKaminoObligation || d.StrategyKey != r.RouteLane || d.Validate() != nil {
		return fmt.Errorf("initializer journal decision differs")
	}
	return nil
}

// observeFinalizedKaminoInitialization is the manifest-aware variant: the same
// receipt observer with the persisted-wire recompilation and the journal
// decision validation resolved through the explicit reviewed manifest, so
// recovery re-derives the candidate AUTO wire from the binding that produced
// it. The public form above is unchanged.
func (m RouteManifest) observeFinalizedKaminoInitialization(ctx context.Context, rpc *RPCClient, r KaminoInitializationRequest, op PersistedOperation) (ConfirmedTransactionEvidence, error) {
	return observeFinalizedKaminoInitializationAdmission(ctx, rpc, r, op, m.compileKaminoInitializationMessage, m.validateInitializerDecision)
}

// observeFinalizedKaminoInitializationAdmission is the shared observer body;
// the installed and manifest forms differ only in the compiler that must
// reproduce the exact persisted wire and in the decision validator applied to
// the journal decision.
func observeFinalizedKaminoInitializationAdmission(ctx context.Context, rpc *RPCClient, r KaminoInitializationRequest, op PersistedOperation, compile func(KaminoInitializationRequest) ([]byte, error), validateDecision func(Decision, KaminoInitializationRequest) error) (ConfirmedTransactionEvidence, error) {
	var out ConfirmedTransactionEvidence
	if rpc == nil {
		return out, fmt.Errorf("initializer RPC unavailable")
	}
	if err := validateDecision(op.Decision, r); err != nil {
		return out, err
	}
	message, err := compile(r)
	if err != nil {
		return out, err
	}
	wire := op.SignedWire
	if len(wire) <= 65 || wire[0] != 1 || allZero(wire[1:65]) || !bytes.Equal(wire[65:], message) || sha256Bytes(wire) != op.SignedWireSHA256 || encodeBase58(wire[1:65]) != op.TransactionSignature ||
		op.RecentBlockhash != r.RecentBlockhash || op.LastValidBlockHeight != r.LastValidBlockHeight {
		return out, fmt.Errorf("initializer persisted wire differs")
	}
	var result struct {
		Slot        int64    `json:"slot"`
		Transaction []string `json:"transaction"`
		Meta        *struct {
			Err        json.RawMessage   `json:"err"`
			Fee        *uint64           `json:"fee"`
			Pre        []uint64          `json:"preBalances"`
			Post       []uint64          `json:"postBalances"`
			PreTokens  []json.RawMessage `json:"preTokenBalances"`
			PostTokens []json.RawMessage `json:"postTokenBalances"`
			ReturnData json.RawMessage   `json:"returnData"`
		} `json:"meta"`
	}
	if err = rpc.call(ctx, "getTransaction", []any{op.TransactionSignature, map[string]any{"commitment": "finalized", "encoding": "base64", "maxSupportedTransactionVersion": 0}}, &result); err != nil {
		return out, err
	}
	if result.Slot <= 0 || result.Slot != op.ConfirmedSlot || len(result.Transaction) != 2 || result.Transaction[1] != "base64" || result.Meta == nil || result.Meta.Fee == nil || string(result.Meta.Err) != "null" {
		return out, fmt.Errorf("initializer finalized receipt differs")
	}
	if len(result.Meta.PreTokens) != 0 || len(result.Meta.PostTokens) != 0 || (len(result.Meta.ReturnData) > 0 && string(result.Meta.ReturnData) != "null") {
		return out, fmt.Errorf("initializer returned unexpected token or return data")
	}
	actual, err := base64.StdEncoding.Strict().DecodeString(result.Transaction[0])
	if err != nil || !bytes.Equal(actual, wire) {
		return out, fmt.Errorf("initializer finalized wire differs")
	}
	route, _ := runtimeRoute(r.RouteLane)
	slot, accounts, err := rpc.getMultipleAccountsAtCommitment(ctx, []string{route.Kamino.Obligation}, result.Slot, nil, "finalized")
	if err != nil {
		return out, err
	}
	if len(accounts) != 1 {
		return out, fmt.Errorf("initializer finalized account unavailable")
	}
	out = ConfirmedTransactionEvidence{Finalized: true, Signature: op.TransactionSignature, Slot: result.Slot,
		Initialization: &KaminoInitializationReceipt{MessageSHA256: sha256Bytes(message), SignedWireSHA256: op.SignedWireSHA256,
			FeeLamports: *result.Meta.Fee, PreBalances: result.Meta.Pre, PostBalances: result.Meta.Post, AccountReadSlot: slot, Obligation: accounts[0]}}
	return out, nil
}
