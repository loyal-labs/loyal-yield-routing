package backyardrwa

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"math"
)

// New Policy state has the same ProgramInteraction body as its legacy create
// payload, with the fixed account envelope and program-assigned start timestamp.
// This layout is independently checked against SDK encoding and retained chain
// readback. It never accepts a caller-supplied constraint or permission vector.
func policySetupExpectedAccount(r policySetupRequest, start int64) ([]byte, error) {
	ix, size, err := policySetupCreateInstruction(r)
	if err != nil {
		return nil, err
	}
	_, bump, err := policySetupAddressAndBump(r.Seed)
	if err != nil || start < 0 || len(ix.data) < 68 {
		return nil, budgetHold("invalid_created_policy")
	}
	data := []byte{222, 135, 7, 163, 235, 177, 33, 68}
	settings := mustKey(bridgeSettings)
	data = append(data, settings[:]...)
	data = binary.LittleEndian.AppendUint64(data, r.Seed)
	data = append(data, bump)
	data = append(data, make([]byte, 16)...)
	data = binary.LittleEndian.AppendUint32(data, 1)
	delegate := mustKey(bridgeDelegate)
	data = append(data, delegate[:]...)
	data = append(data, 7)
	data = binary.LittleEndian.AppendUint16(data, 1)
	data = binary.LittleEndian.AppendUint32(data, 0)
	// Skip instruction prefix/action seed; exclude signers/threshold/time lock
	// and three None arguments at the end. State tag is ProgramInteraction=3.
	data = append(data, ix.data[22:len(ix.data)-46]...)
	data = binary.LittleEndian.AppendUint64(data, uint64(start))
	data = append(data, 0)
	admin := mustKey(bridgeSettingsSigner)
	data = append(data, admin[:]...)
	if len(data) > size {
		return nil, budgetHold("invalid_created_policy")
	}
	return append(data, make([]byte, size-len(data))...), nil
}

func validatePolicySetupCreatedAccount(r policySetupRequest, account ConfirmedAccount, rent uint64, unixTime int64) error {
	ix, size, err := policySetupCreateInstruction(r)
	policy, _, pdaErr := policySetupAddressAndBump(r.Seed)
	if err != nil || pdaErr != nil || account.Address != encodeBase58(policy[:]) || account.Owner != bridgeSquadsProgram || account.Executable || account.Lamports != rent || rent == 0 || len(account.Data) != size || unixTime < 0 {
		return budgetHold("created_policy_account_mismatch")
	}
	startOffset := 108 + len(ix.data) - 22 - 46
	if startOffset+8 > len(account.Data) {
		return budgetHold("created_policy_account_mismatch")
	}
	start := int64(binary.LittleEndian.Uint64(account.Data[startOffset : startOffset+8]))
	if start < 0 || start > unixTime {
		return budgetHold("created_policy_start_invalid")
	}
	expected, err := policySetupExpectedAccount(r, start)
	if err != nil || !bytes.Equal(expected, account.Data) {
		return budgetHold("created_policy_authority_or_constraints_mismatch")
	}
	return nil
}

type policySetupCreatedReceipt struct {
	Signature            string           `json:"signature"`
	WireSHA256           string           `json:"wireSha256"`
	Slot                 int64            `json:"slot"`
	FeeLamports          uint64           `json:"feeLamports"`
	PreBalances          []uint64         `json:"preBalances"`
	PostBalances         []uint64         `json:"postBalances"`
	FinalizedAccountSlot int64            `json:"finalizedAccountSlot"`
	Policy               ConfirmedAccount `json:"policy"`
	SettingsSHA256       string           `json:"settingsSha256"`
	ClockUnixTime        int64            `json:"clockUnixTime"`
}

func policySetupCreationInput(auth phase3OperationAuthorization) (policySetupRequest, ValuedTransactionCost, uint64, error) {
	var request policySetupRequest
	var cost ValuedTransactionCost
	bad := budgetHold("invalid_persisted_setup_creation")
	if auth.GoalID != Phase3GoalID || auth.PolicySetup == nil {
		return request, cost, 0, bad
	}
	parent, err := validatePolicySetupPlan(*auth.PolicySetup)
	if err != nil {
		return request, cost, 0, err
	}
	if auth.PolicySetupCompletion == nil {
		if auth.PolicySetup.Mode != "direct-create" || parent != auth.IntentSHA256 {
			return request, cost, 0, bad
		}
		return auth.PolicySetup.Request, auth.PolicySetup.Payments[0], 0, nil
	}
	digest, err := validatePolicySetupCompletion(*auth.PolicySetup, *auth.PolicySetupCompletion)
	if err != nil || digest != auth.IntentSHA256 {
		return request, cost, 0, bad
	}
	return auth.PolicySetupCompletion.Request, auth.PolicySetupCompletion.Cost, auth.PolicySetupCompletion.Prefund.FundedLamports, nil
}

// No success is inferred from a later funded/allocated PDA. Match the actual
// finalized creation wire and its entire native debit vector first, then check
// the installed state and Settings authority/forward seed at finalized state.
func observePolicySetupCreated(ctx context.Context, rpc *RPCClient, auth phase3OperationAuthorization, op PersistedOperation) (policySetupCreatedReceipt, error) {
	var out policySetupCreatedReceipt
	bad := budgetHold("policy_setup_creation_receipt_mismatch")
	if rpc == nil {
		return out, budgetHold("policy_setup_rpc_missing")
	}
	r, cost, prefund, err := policySetupCreationInput(auth)
	if err != nil {
		return out, err
	}
	if op.Decision.Action != PolicySetupCreate || op.Decision.StrategyKey != "OnRe/ONyc/USDC" || op.RecentBlockhash != r.RecentBlockhash || op.LastValidBlockHeight != r.LastValidBlockHeight {
		return out, bad
	}
	ix, _, err := policySetupCreateInstruction(r)
	if err != nil {
		return out, err
	}
	hash, err := decodeKey(r.RecentBlockhash)
	if err != nil {
		return out, err
	}
	message, err := compileLegacyMessage(mustKey(bridgeSettingsSigner), hash, []compiledInstruction{ix})
	wire := op.SignedWire
	if err != nil || len(wire) <= 65 || wire[0] != 1 || allZero(wire[1:65]) || !bytes.Equal(message, wire[65:]) || sha256Bytes(wire) != op.SignedWireSHA256 || op.SignedWireSHA256 != auth.SignedWireSHA256 || encodeBase58(wire[1:65]) != op.TransactionSignature {
		return out, bad
	}
	var genesis string
	if err = rpc.call(ctx, "getGenesisHash", []any{}, &genesis); err != nil || genesis != "5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d" {
		return out, budgetHold("policy_setup_genesis_mismatch")
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
	if err = rpc.call(ctx, "getTransaction", []any{op.TransactionSignature, map[string]any{"commitment": "finalized", "encoding": "base64", "maxSupportedTransactionVersion": 0}}, &result); err != nil {
		return out, err
	}
	if result.Slot < cost.ObservationSlot || (op.ConfirmedSlot != 0 && result.Slot != op.ConfirmedSlot) || len(result.Transaction) != 2 || result.Transaction[1] != "base64" || result.Meta == nil {
		return out, bad
	}
	actual, err := base64.StdEncoding.DecodeString(result.Transaction[0])
	m := result.Meta
	if err != nil || !bytes.Equal(actual, wire) || string(m.Err) != "null" || m.Fee == nil || *m.Fee == 0 || *m.Fee > cost.Fee.Lamports || len(m.Pre) != 5 || len(m.Post) != 5 || !bytes.Equal(message[:4], []byte{1, 0, 2, 5}) {
		return out, bad
	}
	keys := []string{bridgeSettingsSigner, bridgeSettings, auth.PolicySetup.Policy, "11111111111111111111111111111111", bridgeSquadsProgram}
	for i, address := range keys {
		key := mustKey(address)
		if !bytes.Equal(message[4+32*i:4+32*(i+1)], key[:]) {
			return out, bad
		}
	}
	if cost.SetupLamports > math.MaxUint64-*m.Fee || prefund > math.MaxUint64-cost.SetupLamports || m.Pre[0] < m.Post[0] || m.Pre[0]-m.Post[0] != cost.SetupLamports+*m.Fee || m.Pre[2] != prefund || m.Post[2] != prefund+cost.SetupLamports {
		return out, bad
	}
	for _, i := range []int{1, 3, 4} {
		if m.Pre[i] != m.Post[i] {
			return out, bad
		}
	}
	addresses := []string{bridgeSettings, bridgeSettingsSigner, auth.PolicySetup.Policy, budgetClockAddress}
	slot, accounts, err := rpc.getMultipleAccountsAtCommitment(ctx, addresses, result.Slot, nil, "finalized")
	if err != nil {
		return out, err
	}
	next, err := policySetupNextSeed(accounts[0])
	if err != nil || r.Seed == math.MaxUint64 || next != r.Seed+1 {
		return out, budgetHold("created_policy_settings_mismatch")
	}
	// The deployed creation probe proves only policySeed changes in existing
	// Settings. Normalize that one u64 to its predecessor and compare the
	// frozen prestate hash; membership, archival authority and counters cannot
	// silently drift just because the coarse signer/threshold checks still fit.
	priorSettings:=append([]byte(nil),accounts[0].Data...)
	seedOffset:=127
	if priorSettings[78]==1{seedOffset+=32}
	binary.LittleEndian.PutUint64(priorSettings[seedOffset:seedOffset+8],r.Seed-1)
	if sha256Bytes(priorSettings)!=auth.PolicySetup.SettingsSHA256{return out,budgetHold("created_policy_settings_mismatch")}
	admin := accounts[1]
	if admin.Owner != "11111111111111111111111111111111" || admin.Executable || len(admin.Data) != 0 {
		return out, budgetHold("policy_setup_payer_mismatch")
	}
	clock := accounts[3]
	if clock.Owner != "Sysvar1111111111111111111111111111111111111" || clock.Executable || len(clock.Data) != 40 {
		return out, budgetHold("created_policy_clock_invalid")
	}
	unix := int64(binary.LittleEndian.Uint64(clock.Data[32:40]))
	if err = validatePolicySetupCreatedAccount(r, accounts[2], m.Post[2], unix); err != nil {
		return out, err
	}
	out = policySetupCreatedReceipt{Signature: op.TransactionSignature, WireSHA256: op.SignedWireSHA256, Slot: result.Slot, FeeLamports: *m.Fee, PreBalances: m.Pre, PostBalances: m.Post, FinalizedAccountSlot: slot, Policy: accounts[2], SettingsSHA256: sha256Bytes(accounts[0].Data), ClockUnixTime: unix}
	return out, nil
}
