package backyardrwa

import (
	"crypto/sha256"
	"encoding/binary"
)

// This unsigned compiler is deliberately NOT registered in phase3BuildInput,
// the signer, or the worker queue. It measures the two exact OnRe/USDC repair
// candidates; it does not authorize their installation or runtime activation.
// Production setup still needs finalized Settings/seed and deployment checks,
// durable admission, and reconciliation/recovery of the partially funded PDA.
type policySetupRequest struct {
	Operation            string
	Seed                 uint64
	RecentBlockhash      string
	LastValidBlockHeight int64
}

func policySetupAddress(seed uint64) (publicKey, error) {
	key, _, err := policySetupAddressAndBump(seed)
	return key, err
}

func policySetupAddressAndBump(seed uint64) (publicKey, byte, error) {
	if seed == 0 {
		return publicKey{}, 0, budgetHold("invalid_policy_setup_seed")
	}
	settings, program := mustKey(bridgeSettings), mustKey(bridgeSquadsProgram)
	var seedBytes [8]byte
	binary.LittleEndian.PutUint64(seedBytes[:], seed)
	for bump := 255; bump >= 0; bump-- {
		h := sha256.New()
		for _, part := range [][]byte{[]byte("smart_account"), []byte("policy"), settings[:], seedBytes[:], {byte(bump)}, program[:], []byte("ProgramDerivedAddress")} {
			_, _ = h.Write(part)
		}
		candidate := h.Sum(nil)
		if !ed25519CompressedPointOnCurve(candidate) {
			return publicKeyFromBytes(candidate), byte(bump), nil
		}
	}
	return publicKey{}, 0, budgetHold("invalid_policy_setup_seed")
}

// Same singleton vectors as the retained catalog, changing ONLY the two farm
// placeholders observed at finalized slot 444525169. Never accepts caller
// supplied constraints, authority, program, vault, destination or amount limits.
func policySetupConstraint(operation string) ([]string, []byte, int, error) {
	const obligation = "4LnCFir7Qc99GhjGHLcwtkfweyAMu37u5QE1zTupKsei"
	const market = "47tfyEG9SsdEnUm9cw5kY9BXngQGqu3LBoop9j5uTAv8"
	const authority = "FsvTiXTUFDc4aLbrov4PrvDTjXCWCniL1dxTUkZ1T2ss"
	const reserve = "AYL4LMc4ZCVyq3Z7XPJGWDM4H9PiWjqXAAuuHBEGVR2Z"
	const supply = "8BkQTZsT8ssKMU643De4iiV5Wf3pENdUFTsdtHPueKjB"
	const feeReceiver = "5iLRav31Y7DJwM6bZ7s92jqvV3zd1wZMcp4mYeKXh8cj"
	const userFarm = "nMqFZFPQsNwot49QAD1B76LxNV7qRG1tnbkXyTjbUAD"
	const farm = "7vNfe1qX8iDxP5p3A4fosrjLqdn1YjmmGcZZkG2b4APF"
	const farmsProgram = "FarmsPZpWu9i7Kky8tPN37rs2TpmMrAZrC7S7vJa91Hr"
	const instructions = "Sysvar1nstructions1111111111111111111111111"
	switch operation {
	case "borrow":
		return []string{bridgeVault, obligation, market, authority, reserve, bridgeUSDC, supply, feeReceiver, bridgeSquadsATA, kaminoProgram, classicTokenProgram, instructions, userFarm, farm, farmsProgram}, []byte{161, 128, 143, 245, 171, 199, 194, 6}, 1400, nil
	case "repay":
		return []string{bridgeVault, obligation, market, reserve, bridgeUSDC, supply, bridgeSquadsATA, classicTokenProgram, instructions, userFarm, farm, authority, farmsProgram}, []byte{116, 174, 213, 76, 180, 53, 210, 144}, 1250, nil
	default:
		return nil, nil, 0, budgetHold("unmapped_policy_setup_candidate")
	}
}

func policySetupCreateInstruction(r policySetupRequest) (compiledInstruction, int, error) {
	accounts, discriminator, allocated, err := policySetupConstraint(r.Operation)
	if err != nil {
		return compiledInstruction{}, 0, err
	}
	policy, err := policySetupAddress(r.Seed)
	if err != nil {
		return compiledInstruction{}, 0, err
	}
	// The deployed legacy ABI, independently checked against the installed SDK
	// and retained catalog in policy_setup_test.go. One action; no hook, memo,
	// spending limit, authority change, expiry, or companion instruction.
	hash := sha256.Sum256([]byte("global:execute_settings_transaction_sync"))
	data := append([]byte(nil), hash[:8]...)
	data = append(data, 1) // num_signers
	data = binary.LittleEndian.AppendUint32(data, 1)
	data = append(data, 7) // PolicyCreate
	data = binary.LittleEndian.AppendUint64(data, r.Seed)
	data = append(data, 3, 0) // LegacyProgramInteraction; vault index zero
	data = binary.LittleEndian.AppendUint32(data, 1)
	program := mustKey(kaminoProgram)
	data = append(data, program[:]...)
	data = binary.LittleEndian.AppendUint32(data, uint32(len(accounts)))
	for index, address := range accounts {
		data = append(data, byte(index), 0) // index, Pubkey constraint
		data = binary.LittleEndian.AppendUint32(data, 1)
		key := mustKey(address)
		data = append(data, key[:]...)
		data = append(data, 0) // owner None
	}
	data = binary.LittleEndian.AppendUint32(data, 2)
	data = binary.LittleEndian.AppendUint64(data, 0)
	data = append(data, 5) // U8Slice
	data = binary.LittleEndian.AppendUint32(data, uint32(len(discriminator)))
	data = append(data, discriminator...)
	data = append(data, 0) // Equals
	data = binary.LittleEndian.AppendUint64(data, 8)
	data = append(data, 3) // U64Le
	data = binary.LittleEndian.AppendUint64(data, bridgeCapRaw)
	data = append(data, 5, 0, 0)                     // LessThanOrEqual; pre/post hooks None
	data = binary.LittleEndian.AppendUint32(data, 0) // no spending limits
	data = binary.LittleEndian.AppendUint32(data, 1) // one policy signer
	delegate := mustKey(bridgeDelegate)
	data = append(data, delegate[:]...)
	data = append(data, 7)
	data = binary.LittleEndian.AppendUint16(data, 1)
	data = binary.LittleEndian.AppendUint32(data, 0)
	data = append(data, 0, 0, 0) // start, expiration, memo None
	admin := mustKey(bridgeSettingsSigner)
	return compiledInstruction{program: mustKey(bridgeSquadsProgram), accounts: []accountMeta{
		{mustKey(bridgeSettings), false, true}, {admin, true, true},
		{mustKey("11111111111111111111111111111111"), false, false},
		{mustKey(bridgeSquadsProgram), false, false}, {admin, true, false}, {policy, false, true},
	}, data: data}, allocated, nil
}

func compilePolicySetupMessages(r policySetupRequest, prefundLamports uint64) ([2][]byte, error) {
	var messages [2][]byte
	if r.LastValidBlockHeight <= 0 || prefundLamports == 0 {
		return messages, budgetHold("invalid_policy_setup_funding")
	}
	blockhash, err := decodeKey(r.RecentBlockhash)
	if err != nil {
		return messages, err
	}
	create, _, err := policySetupCreateInstruction(r)
	if err != nil {
		return messages, err
	}
	policy, _ := policySetupAddress(r.Seed)
	admin := mustKey(bridgeSettingsSigner)
	transferData := binary.LittleEndian.AppendUint32(nil, 2) // System Transfer
	transferData = binary.LittleEndian.AppendUint64(transferData, prefundLamports)
	transfer := compiledInstruction{program: mustKey("11111111111111111111111111111111"), accounts: []accountMeta{{admin, true, true}, {policy, false, true}}, data: transferData}
	for i, instruction := range []compiledInstruction{transfer, create} {
		messages[i], err = compileLegacyMessage(admin, blockhash, []compiledInstruction{instruction})
		if err == nil {
			messages[i], err = checkedUnsignedMessage(messages[i])
		}
		if err != nil {
			return [2][]byte{}, err
		}
	}
	return messages, nil
}

type policySetupCostPlan struct {
	Costs            [2]ValuedTransactionCost `json:"costs"`
	TotalMicros      int64                    `json:"totalMicros"`
	RemainingMicros  int64                    `json:"remainingMicros"`
	ValidThroughSlot int64                    `json:"validThroughSlot"`
}

// Prices both actual payer debits (and each message's own network fee). The
// remainder is the completion reserve required BEFORE funding the empty PDA.
// Rent values must come from the observed rent schedule, not SVM fixture rent.
// This pure measurement does not reserve money or release any existing exit.
func valuePolicySetupPlan(r policySetupRequest, totalRent, emptyAccountRent uint64, fees [2]MessageFeeObservation, nativePrice BudgetPrice, slot int64) (policySetupCostPlan, error) {
	var plan policySetupCostPlan
	first := totalRent / 2
	if emptyAccountRent == 0 || first < emptyAccountRent || first == 0 {
		return plan, budgetHold("policy_prefunding_not_rent_exempt")
	}
	messages, err := compilePolicySetupMessages(r, first)
	if err != nil {
		return plan, err
	}
	for i, lamports := range []uint64{first, totalRent - first} {
		plan.Costs[i], err = ValueTransactionCost(messages[i], ExecutableDebit{}, fees[i], lamports, BudgetPrice{}, nativePrice, slot)
		if err != nil {
			return policySetupCostPlan{}, err
		}
		if plan.Costs[i].TotalMicros > Phase3TransactionCapMicros {
			return policySetupCostPlan{}, budgetHold("transaction_cap_exceeded")
		}
	}
	plan.TotalMicros, err = budgetSum(plan.Costs[0].TotalMicros, plan.Costs[1].TotalMicros)
	plan.RemainingMicros = plan.Costs[1].TotalMicros
	plan.ValidThroughSlot = min(plan.Costs[0].ValidThroughSlot, plan.Costs[1].ValidThroughSlot)
	return plan, err
}
