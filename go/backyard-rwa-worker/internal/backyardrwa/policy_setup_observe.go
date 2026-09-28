package backyardrwa

import (
	"bytes"
	"context"
	"encoding/binary"
	"math"
)

// Decode only the deployed Settings envelope required by the pinned admin's
// synchronous PolicyCreate. This is not a membership-management decoder. The
// variable options and signer vector follow the installed generated SDK layout.
func policySetupNextSeed(account ConfirmedAccount) (uint64, error) {
	bad := budgetHold("policy_setup_settings_envelope_mismatch")
	d := account.Data
	if account.Address != bridgeSettings || account.Owner != bridgeSquadsProgram || account.Executable || account.Lamports == 0 || len(d) < 79 || !bytes.Equal(d[:8], []byte{223, 179, 163, 190, 177, 224, 67, 173}) {
		return 0, bad
	}
	// Zero Settings authority means the installed signer/threshold flow, not a
	// different authority able to bypass it. Never silently adapt membership.
	if !allZero(d[24:56]) || binary.LittleEndian.Uint16(d[56:58]) != 1 || binary.LittleEndian.Uint32(d[58:62]) != 0 {
		return 0, bad
	}
	offset := 79
	switch d[78] { // archivalAuthority Option<Pubkey>
	case 0:
	case 1:
		offset += 32
	default:
		return 0, bad
	}
	offset += 8 + 1 // archivableAfter, bump
	if len(d) < offset+4 || binary.LittleEndian.Uint32(d[offset:offset+4]) != 1 {
		return 0, bad
	}
	offset += 4
	admin := mustKey(bridgeSettingsSigner)
	if len(d) < offset+33+1+1+8+1 || !bytes.Equal(d[offset:offset+32], admin[:]) || d[offset+32] != 7 {
		return 0, bad
	}
	offset += 33 + 1    // signer and accountUtilization
	if d[offset] != 1 { // policySeed must already exist for forward repair
		return 0, bad
	}
	seed := binary.LittleEndian.Uint64(d[offset+1 : offset+9])
	if seed < 139 || seed == math.MaxUint64 || !allZero(d[offset+9:]) {
		return 0, bad
	}
	return seed + 1, nil
}

type policySetupObservation struct {
	Request                  policySetupRequest      `json:"request"`
	Policy                   string                  `json:"policy"`
	SettingsSHA256           string                  `json:"settingsSha256"`
	FinalizedSettingsSlot    int64                   `json:"finalizedSettingsSlot"`
	ObservationSlot          int64                   `json:"observationSlot"`
	Mode                     string                  `json:"mode"`
	AllocatedBytes           int                     `json:"allocatedBytes"`
	RentLamports             uint64                  `json:"rentLamports"`
	PayerBalanceLamports     uint64                  `json:"payerBalanceLamports"`
	Payments                 []ValuedTransactionCost `json:"payments"`
	TotalMicros              int64                   `json:"totalMicros"`
	CompletionReserveMicros  int64                   `json:"completionReserveMicros"`
	ValidThroughSlot         int64                   `json:"validThroughSlot"`
	ProductionSetupAdmission bool                    `json:"productionSetupAdmission"`
}

// Read-only candidate pricing, not a send gate. In particular, this does not
// validate all repaired farm relationships, deployment identity or the durable
// journal. A nonempty target is an unfinished/conflicting setup, never adopted.
func observePolicySetup(ctx context.Context, rpc *RPCClient, operation string) (policySetupObservation, error) {
	var out policySetupObservation
	if _, _, err := policySetupConstraints(operation); err != nil {
		return out, err
	}
	if rpc == nil {
		return out, budgetHold("policy_setup_rpc_missing")
	}
	var genesis string
	if err := rpc.call(ctx, "getGenesisHash", []any{}, &genesis); err != nil || genesis != "5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d" {
		return out, budgetHold("policy_setup_genesis_mismatch")
	}
	var slot int64
	if err := rpc.call(ctx, "getSlot", []any{map[string]string{"commitment": "finalized"}}, &slot); err != nil || slot <= 0 {
		return out, budgetHold("policy_setup_finalized_slot_unavailable")
	}
	slot, accounts, err := rpc.getMultipleAccountsAtCommitment(ctx, []string{bridgeSettings}, slot, nil, "finalized")
	if err != nil {
		return out, err
	}
	seed, err := policySetupNextSeed(accounts[0])
	if err != nil {
		return out, err
	}
	out.FinalizedSettingsSlot, out.SettingsSHA256 = slot, sha256Bytes(accounts[0].Data)
	policy, err := policySetupAddress(seed)
	if err != nil {
		return out, err
	}
	out.Policy = encodeBase58(policy[:])
	addresses := []string{bridgeSettings, bridgeSettingsSigner, out.Policy}
	optional := map[string]struct{}{out.Policy: {}}
	slot, accounts, err = rpc.getMultipleAccountsAtCommitment(ctx, addresses, slot, optional, "finalized")
	if err != nil {
		return out, err
	}
	if err = validatePolicySetupPrestate(out.SettingsSHA256, seed, accounts); err != nil {
		return out, err
	}
	out.FinalizedSettingsSlot = slot
	price, err := ObserveNativeSOLBudgetPrice(ctx, rpc, slot)
	if err != nil {
		return out, err
	}
	blockhash, err := rpc.LatestBlockhash(ctx)
	if err != nil {
		return out, err
	}
	out.Request = policySetupRequest{operation, seed, blockhash.Blockhash, blockhash.LastValidBlockHeight}
	create, allocated, err := policySetupCreateInstruction(out.Request)
	if err != nil {
		return out, err
	}
	out.AllocatedBytes = allocated
	if err = rpc.call(ctx, "getMinimumBalanceForRentExemption", []any{allocated, map[string]string{"commitment": "confirmed"}}, &out.RentLamports); err != nil || out.RentLamports == 0 {
		return out, budgetHold("policy_setup_rent_unavailable")
	}
	blockhashKey, err := decodeKey(blockhash.Blockhash)
	if err != nil {
		return out, budgetHold("policy_setup_blockhash_invalid")
	}
	message, err := compileLegacyMessage(mustKey(bridgeSettingsSigner), blockhashKey, []compiledInstruction{create})
	if err != nil {
		return out, err
	}
	fee, err := rpc.ObserveMessageFee(ctx, message, price.ObservedSlot)
	if err != nil {
		return out, err
	}
	slot = max(price.ObservedSlot, fee.Slot)
	cost, err := ValueTransactionCost(message, ExecutableDebit{}, fee, out.RentLamports, BudgetPrice{}, price, slot)
	if err != nil {
		return out, err
	}
	out.Mode, out.Payments = "direct-create", []ValuedTransactionCost{cost}
	out.TotalMicros, out.ValidThroughSlot = cost.TotalMicros, cost.ValidThroughSlot
	if cost.TotalMicros > Phase3TransactionCapMicros {
		var emptyRent uint64
		if err = rpc.call(ctx, "getMinimumBalanceForRentExemption", []any{0, map[string]string{"commitment": "confirmed"}}, &emptyRent); err != nil {
			return out, budgetHold("policy_setup_rent_unavailable")
		}
		messages, err := compilePolicySetupMessages(out.Request, out.RentLamports/2)
		if err != nil {
			return out, err
		}
		firstFee, err := rpc.ObserveMessageFee(ctx, messages[0], slot)
		if err != nil {
			return out, err
		}
		slot = max(slot, firstFee.Slot)
		plan, err := valuePolicySetupPlan(out.Request, out.RentLamports, emptyRent, [2]MessageFeeObservation{firstFee, fee}, price, slot)
		if err != nil {
			return out, err
		}
		out.Mode, out.Payments = "prefund-then-create", plan.Costs[:]
		out.TotalMicros, out.CompletionReserveMicros, out.ValidThroughSlot = plan.TotalMicros, plan.RemainingMicros, plan.ValidThroughSlot
	}
	// Confirmed guard catches Settings changes newer than the finalized seed
	// read. It does not upgrade the original authority observation's commitment.
	slot, accounts, err = rpc.getMultipleAccounts(ctx, addresses, slot, optional)
	if err != nil {
		return out, err
	}
	if err = validatePolicySetupPrestate(out.SettingsSHA256, seed, accounts); err != nil {
		return out, err
	}
	if slot > out.ValidThroughSlot {
		return out, budgetHold("policy_setup_valuation_expired")
	}
	out.ObservationSlot, out.PayerBalanceLamports = slot, accounts[1].Lamports
	var debit uint64
	for _, payment := range out.Payments {
		if payment.SetupLamports > math.MaxUint64-payment.Fee.Lamports || debit > math.MaxUint64-payment.SetupLamports-payment.Fee.Lamports {
			return out, budgetHold("invalid_budget_accounting")
		}
		debit += payment.SetupLamports + payment.Fee.Lamports
	}
	if debit > out.PayerBalanceLamports {
		return out, budgetHold("policy_setup_payer_underfunded")
	}
	return out, nil
}

func validatePolicySetupPrestate(settingsHash string, seed uint64, accounts []ConfirmedAccount) error {
	if len(accounts) != 3 {
		return budgetHold("policy_setup_prestate_mismatch")
	}
	next, err := policySetupNextSeed(accounts[0])
	if err != nil || next != seed || sha256Bytes(accounts[0].Data) != settingsHash {
		return budgetHold("policy_setup_settings_changed")
	}
	admin := accounts[1]
	if admin.Address != bridgeSettingsSigner || admin.Owner != "11111111111111111111111111111111" || admin.Executable || len(admin.Data) != 0 || admin.Lamports == 0 {
		return budgetHold("policy_setup_payer_mismatch")
	}
	target, err := policySetupAddress(seed)
	a := accounts[2]
	if err != nil || a.Address != encodeBase58(target[:]) || a.Owner != "" || a.Lamports != 0 || len(a.Data) != 0 || a.Executable {
		return budgetHold("policy_setup_target_not_absent")
	}
	return nil
}
