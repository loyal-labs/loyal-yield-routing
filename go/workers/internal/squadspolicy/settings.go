package squadspolicy

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"

	"github.com/solana-foundation/solana-go/v2"
)

// Squads settings-instruction decoding, ported from loyal-actions
// detection.rs (decode_squads_policy_create_actions_from_parts,
// detect_squads_policy_removals, detect_squads_policy_update_identity and the
// strict settings envelope) and squads.rs (compact PolicyUpdate encoding).

// AccountMeta is one resolved instruction account.
type AccountMeta struct {
	PublicKey            solana.PublicKey
	IsSigner, IsWritable bool
}

// Instruction is one resolved Squads or Subscriptions instruction.
type Instruction struct {
	ProgramID solana.PublicKey
	Accounts  []AccountMeta
	Data      []byte
}

// SettingsAction mirrors SquadsSettingsActionView: one PolicyCreate (seeded)
// or PolicyUpdate (policy_seed 0) carrying a ProgramInteraction payload.
type SettingsAction struct {
	Settings, Authority solana.PublicKey
	PolicySeed          uint64
	PolicyAccount       solana.PublicKey
	DelegatedSigners    []solana.PublicKey
	Threshold           uint16
	Payload             PolicyPayloadView
}

// PolicyIdentity mirrors DetectedPolicyRemoval.
type PolicyIdentity struct {
	Settings, Authority, PolicyAccount solana.PublicKey
}

// ErrUnsupportedSettingsInstruction is PolicyDetectionError::UnsupportedSettingsInstruction.
var ErrUnsupportedSettingsInstruction = errors.New("instruction is not a supported Squads settings instruction")

var settingsDiscriminator = func() [8]byte {
	digest := sha256.Sum256([]byte("global:execute_settings_transaction_sync"))
	var out [8]byte
	copy(out[:], digest[:8])
	return out
}()

const syncSignerCount = 1

func settingsCursor(data []byte) *borshCursor {
	return &borshCursor{data: data, maxVector: len(data) + 1, maxBytes: len(data) + 1}
}

func (c *borshCursor) requireSettingsDiscriminator() error {
	value, err := c.take(8)
	if err != nil {
		return err
	}
	if [8]byte(value) != settingsDiscriminator {
		return ErrUnsupportedSettingsInstruction
	}
	return nil
}

func (c *borshCursor) remaining() int { return len(c.data) - c.offset }

// DecodeSettingsActions is decode_squads_policy_create_actions.
func DecodeSettingsActions(instruction Instruction) ([]SettingsAction, error) {
	if instruction.ProgramID != Program {
		return nil, nil
	}
	if len(instruction.Accounts) < 1 {
		return nil, errors.New("missing Squads settings account")
	}
	if len(instruction.Accounts) < 2 {
		return nil, errors.New("missing Squads authority account")
	}
	var hint *solana.PublicKey
	if len(instruction.Accounts) > 5 {
		hint = &instruction.Accounts[5].PublicKey
	}
	return decodeSettingsActions(instruction.Accounts[0].PublicKey, instruction.Accounts[1].PublicKey, hint, instruction.Data)
}

func decodeSettingsActions(settings, authority solana.PublicKey, hint *solana.PublicKey, data []byte) ([]SettingsAction, error) {
	c := settingsCursor(data)
	if err := c.requireSettingsDiscriminator(); err != nil {
		return nil, err
	}
	if _, err := c.u8(); err != nil {
		return nil, err
	}
	count, err := c.u32()
	if err != nil {
		return nil, err
	}
	var actions []SettingsAction
	for i := uint32(0); i < count; i++ {
		tag, err := c.u8()
		if err != nil {
			return nil, err
		}
		switch tag {
		case 7:
			seed, err := c.u64()
			if err != nil {
				return nil, err
			}
			payload, err := c.creationPayload()
			if err != nil {
				return nil, err
			}
			if payload == nil {
				if err := c.skipPolicyCreateTail(); err != nil {
					return nil, err
				}
				continue
			}
			signers, err := c.signers()
			if err != nil {
				return nil, err
			}
			threshold, err := c.u16()
			if err != nil {
				return nil, err
			}
			if _, err := c.u32(); err != nil {
				return nil, err
			}
			if err := c.skipOption(8); err != nil {
				return nil, err
			}
			if err := c.skipExpiration(); err != nil {
				return nil, err
			}
			account := solana.PublicKey{}
			if hint != nil {
				account = *hint
			} else if account, _, err = ActionAccount(settings, seed); err != nil {
				return nil, err
			}
			actions = append(actions, SettingsAction{Settings: settings, Authority: authority, PolicySeed: seed, PolicyAccount: account, DelegatedSigners: signers, Threshold: threshold, Payload: *payload})
		case 8:
			account, err := c.pubkey()
			if err != nil {
				return nil, err
			}
			signers, err := c.signers()
			if err != nil {
				return nil, err
			}
			threshold, err := c.u16()
			if err != nil {
				return nil, err
			}
			if _, err := c.u32(); err != nil {
				return nil, err
			}
			payload, err := c.creationPayload()
			if err != nil {
				return nil, err
			}
			if err := c.skipExpiration(); err != nil {
				return nil, err
			}
			if payload != nil {
				actions = append(actions, SettingsAction{Settings: settings, Authority: authority, PolicyAccount: account, DelegatedSigners: signers, Threshold: threshold, Payload: *payload})
			}
		default:
			if err := c.skipSettingsAction(tag); err != nil {
				return nil, err
			}
		}
	}
	if err := c.skipOptionString(); err != nil {
		return nil, err
	}
	return actions, nil
}

// creationPayload is read_policy_payload: tags 3 (legacy) and 4 (compact)
// decode a ProgramInteraction view; any other payload is skipped as nil.
func (c *borshCursor) creationPayload() (*PolicyPayloadView, error) {
	tag, err := c.u8()
	if err != nil {
		return nil, err
	}
	switch tag {
	case 3:
		vaultIndex, err := c.u8()
		if err != nil {
			return nil, err
		}
		payload, err := readConstraints(c, vaultIndex, false)
		if err != nil {
			return nil, err
		}
		for range 2 {
			if err := c.skipRawHook(); err != nil {
				return nil, err
			}
		}
		count, err := c.u32Len()
		if err != nil {
			return nil, err
		}
		for range count {
			limit, err := c.creationSpendingLimit(nil)
			if err != nil {
				return nil, err
			}
			payload.SpendingLimits = append(payload.SpendingLimits, limit)
		}
		return &payload, nil
	case 4:
		vaultIndex, err := c.u8()
		if err != nil {
			return nil, err
		}
		payload, err := readConstraints(c, vaultIndex, true)
		if err != nil {
			return nil, err
		}
		for range 2 {
			if err := c.skipCompiledHook(); err != nil {
				return nil, err
			}
		}
		count, err := c.u8()
		if err != nil {
			return nil, err
		}
		for range int(count) {
			limit, err := c.creationSpendingLimit(payload.PubkeyTable)
			if err != nil {
				return nil, err
			}
			payload.SpendingLimits = append(payload.SpendingLimits, limit)
		}
		return &payload, nil
	default:
		return nil, c.skipPolicyPayloadBody(tag)
	}
}

// creationSpendingLimit reads a LimitedSpendingLimit (raw when table is nil).
func (c *borshCursor) creationSpendingLimit(table []solana.PublicKey) (SpendingLimitView, error) {
	var limit SpendingLimitView
	var err error
	if table == nil {
		limit.Mint, err = c.pubkey()
	} else {
		var index uint8
		if index, err = c.u8(); err == nil {
			limit.Mint, err = indexedKey(table, index)
		}
	}
	if err != nil {
		return limit, err
	}
	if limit.Start, err = c.i64(); err != nil {
		return limit, err
	}
	if _, err = c.option(func() error {
		value, err := c.i64()
		limit.Expiration = &value
		return err
	}); err != nil {
		return limit, err
	}
	if limit.Period, limit.CustomPeriod, err = readPeriodV2(c); err != nil {
		return limit, err
	}
	limit.MaxPerPeriod, err = c.u64()
	return limit, err
}

func (c *borshCursor) signers() ([]solana.PublicKey, error) {
	count, err := c.u32()
	if err != nil {
		return nil, err
	}
	signers := make([]solana.PublicKey, 0, min(int(count), c.remaining()))
	for range count {
		key, err := c.pubkey()
		if err != nil {
			return nil, err
		}
		if _, err := c.u8(); err != nil {
			return nil, err
		}
		signers = append(signers, key)
	}
	return signers, nil
}

// delegatedSigner is read_delegated_signer: exactly one full-permission signer.
func (c *borshCursor) delegatedSigner() (*solana.PublicKey, error) {
	count, err := c.u32()
	if err != nil {
		return nil, err
	}
	var signer *solana.PublicKey
	exact := count == 1
	for i := range count {
		key, err := c.pubkey()
		if err != nil {
			return nil, err
		}
		permissions, err := c.u8()
		if err != nil {
			return nil, err
		}
		if i == 0 {
			signer = &key
		}
		exact = exact && permissions == FullPermissionsMask
	}
	if !exact {
		return nil, nil
	}
	return signer, nil
}

func (c *borshCursor) skip(n int) error { _, err := c.take(n); return err }

func (c *borshCursor) skipOption(n int) error {
	_, err := c.option(func() error { return c.skip(n) })
	return err
}

func (c *borshCursor) skipOptionString() error {
	_, err := c.option(func() error {
		n, err := c.u32()
		if err != nil {
			return err
		}
		return c.skip(int(n))
	})
	return err
}

// expiration reads Option<PolicyExpirationArgs>; true when present.
func (c *borshCursor) expiration() (bool, error) {
	return c.option(func() error {
		kind, err := c.u8()
		if err != nil {
			return err
		}
		switch kind {
		case 0:
			return c.skip(8)
		case 1:
			return nil
		}
		return errors.New("unknown policy expiration args")
	})
}

func (c *borshCursor) skipExpiration() error { _, err := c.expiration(); return err }

func (c *borshCursor) skipPolicyCreateTail() error {
	if _, err := c.signers(); err != nil {
		return err
	}
	if err := c.skip(6); err != nil {
		return err
	}
	if err := c.skipOption(8); err != nil {
		return err
	}
	return c.skipExpiration()
}

func (c *borshCursor) skipSettingsAction(tag uint8) error {
	switch tag {
	case 0:
		return c.skip(33)
	case 1, 5, 9:
		return c.skip(32)
	case 2:
		return c.skip(2)
	case 3:
		return c.skip(4)
	case 4:
		if err := c.skip(32 + 1 + 32 + 8); err != nil {
			return err
		}
		period, err := c.u8()
		if err != nil {
			return err
		}
		if period > 3 {
			return errors.New("unknown legacy period")
		}
		for range 2 {
			n, err := c.u32()
			if err != nil {
				return err
			}
			if err := c.skip(32 * int(n)); err != nil {
				return err
			}
		}
		return c.skip(8)
	case 6:
		return c.skipOption(32)
	case 8:
		if err := c.skip(32); err != nil {
			return err
		}
		if _, err := c.signers(); err != nil {
			return err
		}
		if err := c.skip(6); err != nil {
			return err
		}
		payloadTag, err := c.u8()
		if err != nil {
			return err
		}
		if err := c.skipPolicyPayloadBody(payloadTag); err != nil {
			return err
		}
		return c.skipExpiration()
	}
	return errors.New("unknown settings action")
}

func (c *borshCursor) skipPolicyPayloadBody(tag uint8) error {
	switch tag {
	case 0, 2:
		n, err := c.u32()
		if err != nil {
			return err
		}
		return c.skip(int(n))
	case 1:
		if err := c.skip(32 + 1 + 8); err != nil {
			return err
		}
		if err := c.skipOption(8); err != nil {
			return err
		}
		if _, _, err := readPeriodV2(c); err != nil {
			return err
		}
		if _, err := c.u8(); err != nil {
			return err
		}
		if err := c.skip(8 + 8 + 1); err != nil {
			return err
		}
		if err := c.skipOption(16); err != nil {
			return err
		}
		n, err := c.u32()
		if err != nil {
			return err
		}
		return c.skip(32 * int(n))
	case 3, 4:
		if _, err := c.u8(); err != nil {
			return err
		}
		if _, err := readConstraints(c, 0, tag == 4); err != nil {
			return err
		}
		if tag == 3 {
			for range 2 {
				if err := c.skipRawHook(); err != nil {
					return err
				}
			}
			n, err := c.u32Len()
			if err != nil {
				return err
			}
			for range n {
				if _, err := c.creationSpendingLimit(nil); err != nil {
					return err
				}
			}
			return nil
		}
		for range 2 {
			if err := c.skipCompiledHook(); err != nil {
				return err
			}
		}
		n, err := c.u8()
		if err != nil {
			return err
		}
		for range int(n) {
			if err := c.skip(1 + 8); err != nil {
				return err
			}
			if err := c.skipOption(8); err != nil {
				return err
			}
			if _, _, err := readPeriodV2(c); err != nil {
				return err
			}
			if err := c.skip(8); err != nil {
				return err
			}
		}
		return nil
	}
	return errors.New("unknown policy payload")
}

func (c *borshCursor) skipRawHook() error {
	_, err := c.option(func() error {
		if _, err := c.u8(); err != nil {
			return err
		}
		n, err := c.u32Len()
		if err != nil {
			return err
		}
		for range n {
			if _, err := readRawAccountConstraint(c); err != nil {
				return err
			}
		}
		bytes, err := c.u32()
		if err != nil {
			return err
		}
		if err := c.skip(int(bytes)); err != nil {
			return err
		}
		return c.skip(33)
	})
	return err
}

// skipCompiledHook mirrors detection.rs exactly, including its empty pubkey
// table and u8-prefixed instruction data.
func (c *borshCursor) skipCompiledHook() error {
	_, err := c.option(func() error {
		if _, err := c.u8(); err != nil {
			return err
		}
		n, err := c.u8()
		if err != nil {
			return err
		}
		for range int(n) {
			if _, err := readCompactAccountConstraint(c, nil); err != nil {
				return err
			}
		}
		data, err := c.u8()
		if err != nil {
			return err
		}
		return c.skip(int(data) + 2)
	})
	return err
}

// StrictEnvelope is strict_settings_envelope: the six-account canonical
// settings instruction with a writable policy account.
func StrictEnvelope(instruction Instruction) (PolicyIdentity, bool) {
	a := instruction.Accounts
	if instruction.ProgramID != Program || len(a) != 6 || a[2].PublicKey != solana.SystemProgramID || a[3].PublicKey != Program ||
		a[4].PublicKey != a[1].PublicKey || !a[1].IsSigner || !a[4].IsSigner || !a[5].IsWritable {
		return PolicyIdentity{}, false
	}
	return PolicyIdentity{Settings: a[0].PublicKey, Authority: a[1].PublicKey, PolicyAccount: a[5].PublicKey}, true
}

// DetectPolicyRemovals is detect_squads_policy_removals.
func DetectPolicyRemovals(instruction Instruction) ([]PolicyIdentity, error) {
	a := instruction.Accounts
	if instruction.ProgramID != Program || len(a) < 6 || a[2].PublicKey != solana.SystemProgramID || a[3].PublicKey != Program ||
		a[4].PublicKey != a[1].PublicKey || !a[1].IsSigner || !a[4].IsSigner {
		return nil, nil
	}
	remaining := a[5:]
	c := settingsCursor(instruction.Data)
	if err := c.requireSettingsDiscriminator(); err != nil {
		return nil, err
	}
	signers, err := c.u8()
	if err != nil {
		return nil, err
	}
	if signers != syncSignerCount {
		return nil, nil
	}
	count, err := c.u32()
	if err != nil {
		return nil, err
	}
	if count == 0 || count > 32 || int(count) != len(remaining) {
		return nil, nil
	}
	removals := make([]PolicyIdentity, 0, count)
	for _, account := range remaining {
		tag, err := c.u8()
		if err != nil {
			return nil, err
		}
		if tag != 9 || !account.IsWritable {
			return nil, nil
		}
		policy, err := c.pubkey()
		if err != nil {
			return nil, err
		}
		if policy != account.PublicKey {
			return nil, nil
		}
		removals = append(removals, PolicyIdentity{Settings: a[0].PublicKey, Authority: a[1].PublicKey, PolicyAccount: policy})
	}
	if err := c.skipOptionString(); err != nil {
		return nil, err
	}
	if c.remaining() != 0 {
		return nil, nil
	}
	return removals, nil
}

// DetectPolicyUpdateIdentity is detect_squads_policy_update_identity.
func DetectPolicyUpdateIdentity(instruction Instruction) (*PolicyIdentity, error) {
	envelope, ok := StrictEnvelope(instruction)
	if !ok {
		return nil, nil
	}
	c := settingsCursor(instruction.Data)
	if err := c.requireSettingsDiscriminator(); err != nil {
		return nil, err
	}
	signers, err := c.u8()
	if err != nil {
		return nil, err
	}
	if signers != syncSignerCount {
		return nil, nil
	}
	count, err := c.u32()
	if err != nil {
		return nil, err
	}
	if count != 1 {
		return nil, nil
	}
	tag, err := c.u8()
	if err != nil {
		return nil, err
	}
	if tag != 8 {
		return nil, nil
	}
	policy, err := c.pubkey()
	if err != nil {
		return nil, err
	}
	if policy != envelope.PolicyAccount {
		return nil, nil
	}
	return &envelope, nil
}

// StrictPolicyAction is the one-action strict create/update decoded by
// detect_jupiter_cross_mint_policy_action before classification.
type StrictPolicyAction struct {
	Envelope        PolicyIdentity
	Create          bool
	PolicySeed      uint64
	PolicyAccount   solana.PublicKey
	DelegatedSigner solana.PublicKey
	Payload         PolicyPayloadView
}

// DecodeStrictPolicyAction returns nil unless the instruction is exactly one
// legacy-payload PolicyCreate/PolicyUpdate with canonical metadata.
func DecodeStrictPolicyAction(instruction Instruction) (*StrictPolicyAction, error) {
	envelope, ok := StrictEnvelope(instruction)
	if !ok {
		return nil, nil
	}
	c := settingsCursor(instruction.Data)
	if err := c.requireSettingsDiscriminator(); err != nil {
		return nil, err
	}
	signers, err := c.u8()
	if err != nil {
		return nil, err
	}
	count, err := c.u32()
	if err != nil {
		return nil, err
	}
	if signers != syncSignerCount || count != 1 {
		return nil, nil
	}
	tag, err := c.u8()
	if err != nil {
		return nil, err
	}
	out := StrictPolicyAction{Envelope: envelope}
	var payload *PolicyPayloadView
	var delegate *solana.PublicKey
	exact := false
	switch tag {
	case 7:
		out.Create = true
		if out.PolicySeed, err = c.u64(); err != nil {
			return nil, err
		}
		if payload, err = c.legacyInteractionPayload(); err != nil {
			return nil, err
		}
		if delegate, err = c.delegatedSigner(); err != nil {
			return nil, err
		}
		threshold, err := c.u16()
		if err != nil {
			return nil, err
		}
		timeLock, err := c.u32()
		if err != nil {
			return nil, err
		}
		start, err := c.option(func() error { return c.skip(8) })
		if err != nil {
			return nil, err
		}
		expiration, err := c.expiration()
		if err != nil {
			return nil, err
		}
		if out.PolicyAccount, _, err = ActionAccount(envelope.Settings, out.PolicySeed); err != nil {
			return nil, err
		}
		exact = threshold == 1 && timeLock == 0 && !start && !expiration
	case 8:
		if out.PolicyAccount, err = c.pubkey(); err != nil {
			return nil, err
		}
		if delegate, err = c.delegatedSigner(); err != nil {
			return nil, err
		}
		threshold, err := c.u16()
		if err != nil {
			return nil, err
		}
		timeLock, err := c.u32()
		if err != nil {
			return nil, err
		}
		if payload, err = c.legacyInteractionPayload(); err != nil {
			return nil, err
		}
		expiration, err := c.expiration()
		if err != nil {
			return nil, err
		}
		exact = threshold == 1 && timeLock == 0 && !expiration
	default:
		return nil, nil
	}
	memo, err := c.option(func() error {
		n, err := c.u32()
		if err != nil {
			return err
		}
		return c.skip(int(n))
	})
	if err != nil {
		return nil, err
	}
	if !exact || memo || c.remaining() != 0 || out.PolicyAccount != envelope.PolicyAccount || payload == nil || delegate == nil {
		return nil, nil
	}
	out.Payload, out.DelegatedSigner = *payload, *delegate
	return &out, nil
}

// legacyInteractionPayload is read_program_interaction_payload: only the
// hookless legacy tag 3 is classified.
func (c *borshCursor) legacyInteractionPayload() (*PolicyPayloadView, error) {
	tag, err := c.u8()
	if err != nil {
		return nil, err
	}
	if tag != 3 {
		return nil, c.skipPolicyPayloadBody(tag)
	}
	vaultIndex, err := c.u8()
	if err != nil {
		return nil, err
	}
	payload, err := readConstraints(c, vaultIndex, false)
	if err != nil {
		return nil, err
	}
	hooked := false
	for range 2 {
		present, err := c.option(func() error {
			if _, err := c.u8(); err != nil {
				return err
			}
			n, err := c.u32Len()
			if err != nil {
				return err
			}
			for range n {
				if _, err := readRawAccountConstraint(c); err != nil {
					return err
				}
			}
			bytes, err := c.u32()
			if err != nil {
				return err
			}
			if err := c.skip(int(bytes)); err != nil {
				return err
			}
			return c.skip(33)
		})
		if err != nil {
			return nil, err
		}
		hooked = hooked || present
	}
	n, err := c.u32Len()
	if err != nil {
		return nil, err
	}
	for range n {
		limit, err := c.creationSpendingLimit(nil)
		if err != nil {
			return nil, err
		}
		payload.SpendingLimits = append(payload.SpendingLimits, limit)
	}
	if hooked {
		return nil, nil
	}
	return &payload, nil
}

// CreationTableIsTight is compact_pubkey_table_is_tight for instruction
// payloads, which also reference spending-limit mints.
func CreationTableIsTight(payload PolicyPayloadView) bool {
	if len(payload.PubkeyTable) == 0 {
		return true
	}
	table := map[solana.PublicKey]struct{}{}
	for _, key := range payload.PubkeyTable {
		table[key] = struct{}{}
	}
	if len(table) != len(payload.PubkeyTable) {
		return false
	}
	referenced := map[solana.PublicKey]struct{}{}
	for _, constraint := range payload.Constraints {
		referenced[constraint.ProgramID] = struct{}{}
		for _, account := range constraint.AccountConstraints {
			if account.Owner != nil {
				referenced[*account.Owner] = struct{}{}
			}
			for _, key := range account.Pubkeys {
				referenced[key] = struct{}{}
			}
		}
	}
	for _, limit := range payload.SpendingLimits {
		referenced[limit.Mint] = struct{}{}
	}
	if len(referenced) != len(table) {
		return false
	}
	for key := range referenced {
		if _, ok := table[key]; !ok {
			return false
		}
	}
	return true
}

// EncodeCompactPolicyUpdate is the instruction data of
// update_semantic_program_interaction_policy_instruction: one PolicyUpdate
// with the compact ProgramInteraction payload (enum index 4), no hooks or
// spending limits, one full-permission delegate, threshold 1.
func EncodeCompactPolicyUpdate(policy, delegate solana.PublicKey, accountIndex uint8, constraints []InstructionConstraintView) ([]byte, error) {
	if len(constraints) > 20 {
		return nil, errors.New("compact policy payload overflow")
	}
	var table []solana.PublicKey
	index := func(key solana.PublicKey) (uint8, error) {
		for i, existing := range table {
			if existing == key {
				return uint8(i), nil
			}
		}
		if len(table) >= 240 {
			return 0, errors.New("pubkey table overflow")
		}
		table = append(table, key)
		return uint8(len(table) - 1), nil
	}
	var body []byte
	body = append(body, uint8(len(constraints)))
	for _, constraint := range constraints {
		if len(constraint.AccountConstraints) > 255 || len(constraint.DataConstraints) > 255 {
			return nil, errors.New("compact policy payload overflow")
		}
		program, err := index(constraint.ProgramID)
		if err != nil {
			return nil, err
		}
		body = append(body, program, uint8(len(constraint.AccountConstraints)))
		for _, account := range constraint.AccountConstraints {
			body = append(body, account.AccountIndex)
			if account.Pubkeys != nil {
				if len(account.Pubkeys) > 255 {
					return nil, errors.New("compact policy payload overflow")
				}
				body = append(body, 0, uint8(len(account.Pubkeys)))
				for _, key := range account.Pubkeys {
					i, err := index(key)
					if err != nil {
						return nil, err
					}
					body = append(body, i)
				}
			} else {
				if len(account.AccountData) > 255 {
					return nil, errors.New("compact policy payload overflow")
				}
				body = append(body, 1, uint8(len(account.AccountData)))
				if body, err = appendDataConstraintValues(body, account.AccountData); err != nil {
					return nil, err
				}
			}
			if account.Owner == nil {
				body = append(body, 0)
			} else {
				i, err := index(*account.Owner)
				if err != nil {
					return nil, err
				}
				body = append(body, 1, i)
			}
		}
		body = append(body, uint8(len(constraint.DataConstraints)))
		if body, err = appendDataConstraintValues(body, constraint.DataConstraints); err != nil {
			return nil, err
		}
	}
	body = append(body, 0, 0, 0) // pre_hook, post_hook, spending_limits
	out := append([]byte(nil), settingsDiscriminator[:]...)
	out = append(out, syncSignerCount)
	out = binary.LittleEndian.AppendUint32(out, 1)
	out = append(out, 8)
	out = append(out, policy[:]...)
	out = binary.LittleEndian.AppendUint32(out, 1)
	out = append(append(out, delegate[:]...), FullPermissionsMask)
	out = binary.LittleEndian.AppendUint16(out, 1)
	out = binary.LittleEndian.AppendUint32(out, 0)
	out = append(out, 4, accountIndex, uint8(len(table)))
	for _, key := range table {
		out = append(out, key[:]...)
	}
	out = append(out, body...)
	out = append(out, 0) // expiration_args
	return append(out, 0), nil
}

// appendDataConstraintValues writes data constraints without a length prefix.
func appendDataConstraintValues(out []byte, constraints []DataConstraintView) ([]byte, error) {
	encoded, err := appendDataConstraints(nil, constraints)
	if err != nil {
		return nil, err
	}
	return append(out, encoded[4:]...), nil
}
