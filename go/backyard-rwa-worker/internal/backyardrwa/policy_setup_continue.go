package backyardrwa

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"math"
	"reflect"
)

type policySetupPrefundReceipt struct {
	Signature      string   `json:"signature"`
	WireSHA256     string   `json:"wireSha256"`
	Slot           int64    `json:"slot"`
	FeeLamports    uint64   `json:"feeLamports"`
	FundedLamports uint64   `json:"fundedLamports"`
	PreBalances    []uint64 `json:"preBalances"`
	PostBalances   []uint64 `json:"postBalances"`
}

// Continuation keeps the original policy identity/constraints and prefund
// receipt, but prices only the still-unpaid creation using a fresh blockhash.
// It is neither a new setup budget nor a second prefunding instruction.
type policySetupCompletion struct {
	PrefundOperationID   string                    `json:"prefundOperationId"`
	Prefund              policySetupPrefundReceipt `json:"prefund"`
	Request              policySetupRequest        `json:"request"`
	Cost                 ValuedTransactionCost     `json:"cost"`
	RentLamports         uint64                    `json:"rentLamports"`
	FinalizedAccountSlot int64                     `json:"finalizedAccountSlot"`
	ObservationSlot      int64                     `json:"observationSlot"`
}

// Exact base64 wire plus transaction-scoped native deltas prove this payment.
// The RPC is asked for finalized commitment; missing/failed/malformed receipts
// are not absence proofs and cannot free the reservation or authorize a resend.
func finalizedPolicySetupPrefund(ctx context.Context, rpc *RPCClient, plan policySetupObservation, op PersistedOperation) (policySetupPrefundReceipt, error) {
	var out policySetupPrefundReceipt
	bad := budgetHold("policy_setup_prefund_receipt_mismatch")
	if rpc == nil {
		return out, budgetHold("policy_setup_rpc_missing")
	}
	if _, err := validatePolicySetupPlan(plan); err != nil {
		return out, err
	}
	if plan.Mode != "prefund-then-create" || op.ID == "" || op.Decision.Action != PolicySetupPrefund || op.Decision.StrategyKey != "OnRe/ONyc/USDC" {
		return out, bad
	}
	pair, err := compilePolicySetupMessages(plan.Request, plan.RentLamports/2)
	wire := op.SignedWire
	if err != nil || len(wire) <= 65 || wire[0] != 1 || allZero(wire[1:65]) || !bytes.Equal(wire[65:], pair[0]) || sha256Bytes(wire) != op.SignedWireSHA256 || encodeBase58(wire[1:65]) != op.TransactionSignature || op.RecentBlockhash != plan.Request.RecentBlockhash || op.LastValidBlockHeight != plan.Request.LastValidBlockHeight {
		return out, bad
	}
	var result struct {
		Slot        int64    `json:"slot"`
		Transaction []string `json:"transaction"`
		Meta        *struct {
			Err  json.RawMessage `json:"err"`
			Fee  *uint64         `json:"fee"`
			Pre  []uint64        `json:"preBalances"`
			Post []uint64        `json:"postBalances"`
		} `json:"meta"`
	}
	if err := rpc.call(ctx, "getTransaction", []any{op.TransactionSignature, map[string]any{"commitment": "finalized", "encoding": "base64", "maxSupportedTransactionVersion": 0}}, &result); err != nil {
		return out, err
	}
	if result.Slot < plan.ObservationSlot || (op.ConfirmedSlot != 0 && op.ConfirmedSlot != result.Slot) || len(result.Transaction) != 2 || result.Transaction[1] != "base64" || result.Meta == nil {
		return out, bad
	}
	got, err := base64.StdEncoding.DecodeString(result.Transaction[0])
	m := result.Meta
	if err != nil || !bytes.Equal(got, wire) || string(m.Err) != "null" || m.Fee == nil || *m.Fee == 0 || *m.Fee > plan.Payments[0].Fee.Lamports || len(m.Pre) != 3 || len(m.Post) != 3 {
		return out, bad
	}
	// The reconstructed, single-instruction transfer has exactly these three
	// static accounts: pinned admin payer, exact PDA, System Program. Check the
	// compiler's keys explicitly before interpreting positional receipt values.
	keys := []string{bridgeSettingsSigner, plan.Policy, "11111111111111111111111111111111"}
	if len(pair[0]) < 100 || !bytes.Equal(pair[0][:4], []byte{1, 0, 1, 3}) {
		return out, bad
	}
	for i, key := range keys {
		k := mustKey(key)
		if !bytes.Equal(pair[0][4+i*32:4+(i+1)*32], k[:]) {
			return out, bad
		}
	}
	funded := plan.Payments[0].SetupLamports
	if funded > math.MaxUint64-*m.Fee || m.Pre[0] < m.Post[0] || m.Pre[0]-m.Post[0] != funded+*m.Fee || m.Pre[1] != 0 || m.Post[1] != funded || m.Pre[2] != m.Post[2] {
		return out, bad
	}
	return policySetupPrefundReceipt{Signature: op.TransactionSignature, WireSHA256: op.SignedWireSHA256, Slot: result.Slot, FeeLamports: *m.Fee, FundedLamports: funded, PreBalances: m.Pre, PostBalances: m.Post}, nil
}

func validatePolicySetupCompletion(plan policySetupObservation, completion policySetupCompletion) (string, error) {
	parent, err := validatePolicySetupPlan(plan)
	if err != nil {
		return "", err
	}
	bad := budgetHold("invalid_persisted_setup_continuation")
	if plan.Mode != "prefund-then-create" || completion.PrefundOperationID == "" || completion.Request.Operation != plan.Request.Operation || completion.Request.Seed != plan.Request.Seed || completion.Request.LastValidBlockHeight <= 0 || completion.Prefund.Slot < plan.ObservationSlot || completion.FinalizedAccountSlot < completion.Prefund.Slot || completion.Cost.ObservationSlot < completion.FinalizedAccountSlot || completion.ObservationSlot < completion.Cost.ObservationSlot || completion.ObservationSlot > completion.Cost.ValidThroughSlot || completion.Prefund.FundedLamports != plan.Payments[0].SetupLamports || completion.RentLamports <= completion.Prefund.FundedLamports || completion.Cost.SetupLamports != completion.RentLamports-completion.Prefund.FundedLamports || completion.Cost.SetupLamports > math.MaxInt64 || completion.Cost.TotalMicros > Phase3TransactionCapMicros {
		return "", bad
	}
	create, _, err := policySetupCreateInstruction(completion.Request)
	if err != nil {
		return "", bad
	}
	key, err := decodeKey(completion.Request.RecentBlockhash)
	if err != nil {
		return "", bad
	}
	message, err := compileLegacyMessage(mustKey(bridgeSettingsSigner), key, []compiledInstruction{create})
	if err != nil {
		return "", bad
	}
	cost, err := ValueTransactionCost(message, ExecutableDebit{}, completion.Cost.Fee, completion.Cost.SetupLamports, BudgetPrice{}, completion.Cost.NativePrice, completion.Cost.ObservationSlot)
	if err != nil || !reflect.DeepEqual(cost, completion.Cost) {
		return "", bad
	}
	encoded, _ := json.Marshal(struct {
		ParentIntent string
		Completion   policySetupCompletion
	}{parent, completion})
	return sha256Bytes(encoded), nil
}

func validatePolicySetupFundedPrestate(plan policySetupObservation, accounts []ConfirmedAccount) error {
	if len(accounts) != 3 {
		return budgetHold("policy_setup_prestate_mismatch")
	}
	// Reuse the full Settings/admin envelope without pretending the real target
	// is absent. Only a system-owned, empty, exact original prefund is resumable.
	absent := append([]ConfirmedAccount(nil), accounts...)
	absent[2] = ConfirmedAccount{Address: plan.Policy}
	if err := validatePolicySetupPrestate(plan.SettingsSHA256, plan.Request.Seed, absent); err != nil {
		return err
	}
	a := accounts[2]
	if a.Address != plan.Policy || a.Owner != "11111111111111111111111111111111" || a.Executable || len(a.Data) != 0 || a.Lamports != plan.Payments[0].SetupLamports {
		return budgetHold("policy_setup_prefund_account_changed")
	}
	return nil
}

func observePolicySetupCompletion(ctx context.Context, rpc *RPCClient, plan policySetupObservation, op PersistedOperation) (policySetupCompletion, error) {
	var out policySetupCompletion
	if rpc == nil {
		return out, budgetHold("policy_setup_rpc_missing")
	}
	var genesis string
	if err := rpc.call(ctx, "getGenesisHash", []any{}, &genesis); err != nil || genesis != "5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d" {
		return out, budgetHold("policy_setup_genesis_mismatch")
	}
	receipt, err := finalizedPolicySetupPrefund(ctx, rpc, plan, op)
	if err != nil {
		return out, err
	}
	addresses := []string{bridgeSettings, bridgeSettingsSigner, plan.Policy}
	slot, accounts, err := rpc.getMultipleAccountsAtCommitment(ctx, addresses, receipt.Slot, nil, "finalized")
	if err != nil {
		return out, err
	}
	if err = validatePolicySetupFundedPrestate(plan, accounts); err != nil {
		return out, err
	}
	out.PrefundOperationID, out.Prefund, out.FinalizedAccountSlot = op.ID, receipt, slot
	price, err := ObserveNativeSOLBudgetPrice(ctx, rpc, slot)
	if err != nil {
		return out, err
	}
	blockhash, err := rpc.LatestBlockhash(ctx)
	if err != nil {
		return out, err
	}
	out.Request = plan.Request
	out.Request.RecentBlockhash, out.Request.LastValidBlockHeight = blockhash.Blockhash, blockhash.LastValidBlockHeight
	create, size, err := policySetupCreateInstruction(out.Request)
	if err != nil {
		return out, err
	}
	key, err := decodeKey(blockhash.Blockhash)
	if err != nil || blockhash.LastValidBlockHeight <= 0 {
		return out, budgetHold("policy_setup_blockhash_invalid")
	}
	message, err := compileLegacyMessage(mustKey(bridgeSettingsSigner), key, []compiledInstruction{create})
	if err != nil {
		return out, err
	}
	if err = rpc.call(ctx, "getMinimumBalanceForRentExemption", []any{size, map[string]string{"commitment": "confirmed"}}, &out.RentLamports); err != nil || out.RentLamports <= receipt.FundedLamports {
		return out, budgetHold("policy_setup_rent_unavailable")
	}
	fee, err := rpc.ObserveMessageFee(ctx, message, price.ObservedSlot)
	if err != nil {
		return out, err
	}
	out.Cost, err = ValueTransactionCost(message, ExecutableDebit{}, fee, out.RentLamports-receipt.FundedLamports, BudgetPrice{}, price, max(price.ObservedSlot, fee.Slot))
	if err != nil {
		return out, err
	}
	if out.Cost.TotalMicros > Phase3TransactionCapMicros {
		return out, budgetHold("transaction_cap_exceeded")
	}
	slot, accounts, err = rpc.getMultipleAccounts(ctx, addresses, out.Cost.ObservationSlot, nil)
	if err != nil {
		return out, err
	}
	if err = validatePolicySetupFundedPrestate(plan, accounts); err != nil {
		return out, err
	}
	if slot > out.Cost.ValidThroughSlot {
		return out, budgetHold("policy_setup_valuation_expired")
	}
	if out.Cost.SetupLamports > math.MaxUint64-fee.Lamports || accounts[1].Lamports < out.Cost.SetupLamports+fee.Lamports {
		return out, budgetHold("policy_setup_payer_underfunded")
	}
	out.ObservationSlot = slot
	return out, nil
}
