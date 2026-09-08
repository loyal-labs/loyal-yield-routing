package backyardrwa

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"math"
	"time"
)

type policySetupPayment struct {
	Request        policySetupRequest
	Message        []byte
	Cost           ValuedTransactionCost
	CompletionCost *ValuedTransactionCost
}

func policySetupPaymentInput(auth phase3OperationAuthorization, action Action) (policySetupPayment, error) {
	var out policySetupPayment
	if auth.GoalID != Phase3GoalID || auth.PolicySetup == nil || auth.ReservationReleased || auth.BookedSpentMicros != 0 {
		return out, budgetHold("invalid_setup_payment_intent")
	}
	if action == PolicySetupCreate {
		r, cost, _, err := policySetupCreationInput(auth)
		if err != nil {
			return out, err
		}
		ix, _, err := policySetupCreateInstruction(r)
		if err != nil {
			return out, err
		}
		key, err := decodeKey(r.RecentBlockhash)
		if err != nil {
			return out, err
		}
		message, err := compileLegacyMessage(mustKey(bridgeSettingsSigner), key, []compiledInstruction{ix})
		if err != nil {
			return out, err
		}
		return policySetupPayment{Request: r, Message: message, Cost: cost}, nil
	}
	digest, err := validatePolicySetupPlan(*auth.PolicySetup)
	if err != nil {
		return out, err
	}
	if action != PolicySetupPrefund || auth.PolicySetup.Mode != "prefund-then-create" || auth.PolicySetupCompletion != nil || digest != auth.IntentSHA256 {
		return out, budgetHold("invalid_setup_payment_intent")
	}
	pair, err := compilePolicySetupMessages(auth.PolicySetup.Request, auth.PolicySetup.Payments[0].SetupLamports)
	if err != nil {
		return out, err
	}
	return policySetupPayment{Request: auth.PolicySetup.Request, Message: pair[0], Cost: auth.PolicySetup.Payments[0], CompletionCost: &auth.PolicySetup.Payments[1]}, nil
}

// Refresh exact-message fees/native valuation and the complete remaining setup
// before signer access and again before send. No new blockhash, policy seed,
// request, wire or reservation is substituted here. Unsigned expired requests
// need an explicit refresh; a possibly signed request needs expiry reconciliation.
// This measures cost/prestate, not repaired-farm or deployed-worker readiness.
func observePolicySetupPayment(ctx context.Context, rpc *RPCClient, auth phase3OperationAuthorization, action Action) (policySetupPayment, error) {
	out, err := policySetupPaymentInput(auth, action)
	if err != nil {
		return out, err
	}
	if rpc == nil {
		return out, budgetHold("policy_setup_rpc_missing")
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	var genesis string
	if err = rpc.call(ctx, "getGenesisHash", []any{}, &genesis); err != nil || genesis != "5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d" {
		return out, budgetHold("policy_setup_genesis_mismatch")
	}
	plan := auth.PolicySetup
	minimum := plan.FinalizedSettingsSlot
	if auth.PolicySetupCompletion != nil {
		minimum = auth.PolicySetupCompletion.Prefund.Slot
	}
	addresses := []string{bridgeSettings, bridgeSettingsSigner, plan.Policy}
	optional := map[string]struct{}{plan.Policy: {}}
	validate := func(accounts []ConfirmedAccount) error {
		if auth.PolicySetupCompletion != nil {
			return validatePolicySetupFundedPrestate(*plan, accounts)
		}
		return validatePolicySetupPrestate(plan.SettingsSHA256, out.Request.Seed, accounts)
	}
	slot, accounts, err := rpc.getMultipleAccountsAtCommitment(ctx, addresses, minimum, optional, "finalized")
	if err != nil {
		return out, err
	}
	if err = validate(accounts); err != nil {
		return out, err
	}
	price, err := ObserveNativeSOLBudgetPrice(ctx, rpc, slot)
	if err != nil {
		return out, err
	}
	var rent uint64
	if err = rpc.call(ctx, "getMinimumBalanceForRentExemption", []any{plan.AllocatedBytes, map[string]string{"commitment": "confirmed"}}, &rent); err != nil || rent == 0 {
		return out, budgetHold("policy_setup_rent_unavailable")
	}
	expectedRent := plan.RentLamports
	if auth.PolicySetupCompletion != nil {
		expectedRent = auth.PolicySetupCompletion.RentLamports
	}
	if rent != expectedRent {
		return out, budgetHold("policy_setup_rent_changed")
	}
	fee, err := rpc.ObserveMessageFee(ctx, out.Message, price.ObservedSlot)
	if err != nil {
		return out, err
	}
	if fee.Lamports > out.Cost.Fee.Lamports {
		return out, budgetHold("policy_setup_fee_bound_changed")
	}
	out.Cost, err = ValueTransactionCost(out.Message, ExecutableDebit{}, fee, out.Cost.SetupLamports, BudgetPrice{}, price, max(price.ObservedSlot, fee.Slot))
	if err != nil {
		return out, err
	}
	if out.Cost.TotalMicros > Phase3TransactionCapMicros {
		return out, budgetHold("transaction_cap_exceeded")
	}
	if out.CompletionCost != nil {
		pair, err := compilePolicySetupMessages(out.Request, out.Cost.SetupLamports)
		if err != nil {
			return out, err
		}
		completionFee, err := rpc.ObserveMessageFee(ctx, pair[1], out.Cost.ObservationSlot)
		if err != nil {
			return out, err
		}
		remaining, err := ValueTransactionCost(pair[1], ExecutableDebit{}, completionFee, rent-out.Cost.SetupLamports, BudgetPrice{}, price, max(out.Cost.ObservationSlot, completionFee.Slot))
		if err != nil {
			return out, err
		}
		if remaining.TotalMicros > Phase3TransactionCapMicros {
			return out, budgetHold("setup_completion_exceeds_reserved_cap")
		}
		out.CompletionCost = &remaining
	}
	height, err := rpc.ConfirmedBlockHeight(ctx)
	if err != nil {
		return out, err
	}
	if height > out.Request.LastValidBlockHeight {
		return out, budgetHold("policy_setup_blockhash_expired")
	}
	minimum = out.Cost.ObservationSlot
	validThrough := out.Cost.ValidThroughSlot
	debit := out.Cost.SetupLamports + out.Cost.Fee.Lamports
	if out.Cost.SetupLamports > math.MaxUint64-out.Cost.Fee.Lamports {
		return out, budgetHold("invalid_budget_accounting")
	}
	if out.CompletionCost != nil {
		minimum = max(minimum, out.CompletionCost.ObservationSlot)
		validThrough = min(validThrough, out.CompletionCost.ValidThroughSlot)
		remaining := out.CompletionCost
		if remaining.SetupLamports > math.MaxUint64-remaining.Fee.Lamports || debit > math.MaxUint64-remaining.SetupLamports-remaining.Fee.Lamports {
			return out, budgetHold("invalid_budget_accounting")
		}
		debit += remaining.SetupLamports + remaining.Fee.Lamports
	}
	slot, accounts, err = rpc.getMultipleAccounts(ctx, addresses, minimum, optional)
	if err != nil {
		return out, err
	}
	if err = validate(accounts); err != nil {
		return out, err
	}
	if slot > validThrough {
		return out, budgetHold("policy_setup_valuation_expired")
	}
	if accounts[1].Lamports < debit {
		return out, budgetHold("policy_setup_payer_underfunded")
	}
	// Preserve each independently priced cost's original observation fields.
	// The locked journal boundary rechecks the shared earliest expiry again.
	return out, nil
}

func validatePolicySetupSignedPayment(auth phase3OperationAuthorization, op PersistedOperation) error {
	input, err := policySetupPaymentInput(auth, op.Decision.Action)
	if err != nil {
		return err
	}
	if op.Decision.StrategyKey != "OnRe/ONyc/USDC" || op.RecentBlockhash != input.Request.RecentBlockhash || op.LastValidBlockHeight != input.Request.LastValidBlockHeight {
		return budgetHold("setup_signed_intent_mismatch")
	}
	wire := op.SignedWire
	if len(wire) <= 65 || wire[0] != 1 || !bytes.Equal(wire[65:], input.Message) || sha256Bytes(wire) != auth.SignedWireSHA256 || op.SignedWireSHA256 != auth.SignedWireSHA256 || op.TransactionSignature != encodeBase58(wire[1:65]) {
		return budgetHold("setup_signed_intent_mismatch")
	}
	admin := mustKey(bridgeSettingsSigner)
	if !ed25519.Verify(admin[:], input.Message, wire[1:65]) {
		return budgetHold("setup_signature_invalid")
	}
	return nil
}
