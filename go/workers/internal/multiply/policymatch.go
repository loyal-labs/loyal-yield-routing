package multiply

// Faithful port of the Squads ProgramInteraction policy account decoder and
// the canonical Earn MAX constraint builder:
//   crates/loyal-actions/src/detection.rs
//     decode_program_interaction_policy_account and its readers,
//   crates/loyal-actions/src/earn_max.rs
//     earn_max_policy_constraints,
//   crates/loyal-actions/src/squads.rs
//     semantic_program_interaction_constraints,
//   crates/loyal-fleet-worker/src/multiply/policy.rs
//     current_policy_matches.
// The comparison is structural over the decoded payload view, exactly like
// canonical_policy_payload_matches in the Rust worker.

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/gagliardetto/solana-go"
)

const squadsFullPermissionsMask = uint8(7)

// DataOperatorView mirrors SquadsDataOperatorView.
type DataOperatorView uint8

const (
	OpEquals DataOperatorView = iota
	OpNotEquals
	OpGreaterThan
	OpGreaterThanOrEqualTo
	OpLessThan
	OpLessThanOrEqualTo
)

// DataValueView mirrors SquadsDataValueView.
type DataValueView struct {
	Kind  uint8 // 0 u8, 1 u16le, 2 u32le, 3 u64le, 4 u128le, 5 bytes
	U8    uint8
	U16   uint16
	U32   uint32
	U64   uint64
	U128  [16]byte
	Bytes []byte
}

// DataConstraintView mirrors SquadsDataConstraintView.
type DataConstraintView struct {
	DataOffset uint64
	DataValue  DataValueView
	Operator   DataOperatorView
}

// AccountConstraintView mirrors SquadsAccountConstraintView.
type AccountConstraintView struct {
	AccountIndex uint8
	// Pubkeys is set when the constraint pins literal accounts; AccountData
	// is set when it constrains an account's own data.
	Pubkeys     []solana.PublicKey
	AccountData []DataConstraintView
	Owner       *solana.PublicKey
}

// InstructionConstraintView mirrors SquadsInstructionConstraintView.
type InstructionConstraintView struct {
	ProgramID          solana.PublicKey
	AccountConstraints []AccountConstraintView
	DataConstraints    []DataConstraintView
}

// SpendingLimitView mirrors SquadsLimitedSpendingLimitView.
type SpendingLimitView struct {
	Mint         solana.PublicKey
	Start        int64
	Expiration   *int64
	Period       uint8 // 0 one-time, 1 daily, 2 weekly, 3 monthly, 4 custom
	CustomPeriod int64
	MaxPerPeriod uint64
}

// PolicyPayloadView mirrors SquadsProgramInteractionPolicyView.
type PolicyPayloadView struct {
	VaultIndex     uint8
	PubkeyTable    []solana.PublicKey
	Constraints    []InstructionConstraintView
	SpendingLimits []SpendingLimitView
}

// PolicyAccountView mirrors SquadsProgramInteractionPolicyAccountView.
type PolicyAccountView struct {
	Settings        solana.PublicKey
	PolicySeed      uint64
	PolicyAccount   solana.PublicKey
	DelegatedSigner solana.PublicKey
	Threshold       uint16
	Payload         PolicyPayloadView
}

type borshCursor struct {
	data   []byte
	offset int
}

func (c *borshCursor) take(count int) ([]byte, error) {
	if count < 0 || count > len(c.data)-c.offset {
		return nil, errors.New("borsh cursor is truncated")
	}
	out := c.data[c.offset : c.offset+count]
	c.offset += count
	return out, nil
}

func (c *borshCursor) u8() (uint8, error) {
	raw, err := c.take(1)
	if err != nil {
		return 0, err
	}
	return raw[0], nil
}

func (c *borshCursor) u16() (uint16, error) {
	raw, err := c.take(2)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint16(raw), nil
}

func (c *borshCursor) u32() (uint32, error) {
	raw, err := c.take(4)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(raw), nil
}

func (c *borshCursor) u64() (uint64, error) {
	raw, err := c.take(8)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint64(raw), nil
}

func (c *borshCursor) u128() ([16]byte, error) {
	var value [16]byte
	raw, err := c.take(16)
	if err != nil {
		return value, err
	}
	copy(value[:], raw)
	return value, nil
}

func (c *borshCursor) i64() (int64, error) {
	value, err := c.u64()
	return int64(value), err
}

func (c *borshCursor) pubkey() (solana.PublicKey, error) {
	var key solana.PublicKey
	raw, err := c.take(32)
	if err != nil {
		return key, err
	}
	copy(key[:], raw)
	return key, nil
}

func (c *borshCursor) bool() (bool, error) {
	value, err := c.u8()
	if err != nil {
		return false, err
	}
	if value > 1 {
		return false, errors.New("invalid boolean byte")
	}
	return value == 1, nil
}

func (c *borshCursor) u32Len() (int, error) {
	value, err := c.u32()
	if err != nil {
		return 0, err
	}
	if value > 4096 || int(value) > len(c.data)-c.offset {
		return 0, errors.New("policy vector length exceeds bounded payload")
	}
	return int(value), nil
}

func (c *borshCursor) u8Len() (int, error) {
	value, err := c.u8()
	if err != nil {
		return 0, err
	}
	return int(value), nil
}

func (c *borshCursor) pubkeys(count int) ([]solana.PublicKey, error) {
	keys := make([]solana.PublicKey, 0, count)
	for i := 0; i < count; i++ {
		key, err := c.pubkey()
		if err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	return keys, nil
}

func (c *borshCursor) option(readSome func() error) (bool, error) {
	tag, err := c.u8()
	if err != nil {
		return false, err
	}
	if tag == 0 {
		return false, nil
	}
	if tag != 1 {
		return false, errors.New("invalid option tag")
	}
	return true, readSome()
}

func indexedKey(table []solana.PublicKey, index uint8) (solana.PublicKey, error) {
	if int(index) >= len(table) {
		return solana.PublicKey{}, errors.New("pubkey table index out of bounds")
	}
	return table[index], nil
}

// anchorAccountDiscriminator mirrors anchor_account_discriminator("Policy")
// (sha256 of "account:<name>"). The Squads Policy discriminator is fixed.
var squadsPolicyAccountDiscriminator = [8]byte{155, 23, 24, 219, 22, 33, 43, 240}

// DecodeProgramInteractionPolicyAccount is the Go port of
// decode_program_interaction_policy_account: nil means "not a canonical
// hookless ProgramInteraction policy" (which the executor treats as
// mismatch, never as authority to proceed).
func DecodeProgramInteractionPolicyAccount(data []byte) (*PolicyAccountView, error) {
	if len(data) > 64<<10 {
		return nil, errors.New("policy account exceeds payload limit")
	}
	cursor := &borshCursor{data: data}
	discriminator, err := cursor.take(8)
	if err != nil {
		return nil, err
	}
	if !equalBytes(discriminator, squadsPolicyAccountDiscriminator[:]) {
		return nil, errors.New("account discriminator is not a Squads Policy account")
	}
	settings, err := cursor.pubkey()
	if err != nil {
		return nil, err
	}
	policySeed, err := cursor.u64()
	if err != nil {
		return nil, err
	}
	policyBump, err := cursor.u8()
	if err != nil {
		return nil, err
	}
	transactionIndex, err := cursor.u64()
	if err != nil {
		return nil, err
	}
	staleTransactionIndex, err := cursor.u64()
	if err != nil {
		return nil, err
	}
	signerCount, err := cursor.u32()
	if err != nil {
		return nil, err
	}
	if signerCount > 32 {
		return nil, errors.New("too many policy signers")
	}
	signers := make([]solana.PublicKey, 0, signerCount)
	permissions := make([]uint8, 0, signerCount)
	for i := uint32(0); i < signerCount; i++ {
		key, err := cursor.pubkey()
		if err != nil {
			return nil, err
		}
		mask, err := cursor.u8()
		if err != nil {
			return nil, err
		}
		signers = append(signers, key)
		permissions = append(permissions, mask)
	}
	threshold, err := cursor.u16()
	if err != nil {
		return nil, err
	}
	timeLock, err := cursor.u32()
	if err != nil {
		return nil, err
	}
	kind, err := cursor.u8()
	if err != nil {
		return nil, err
	}
	if kind != 3 {
		return nil, nil
	}
	accountIndex, err := cursor.u8()
	if err != nil {
		return nil, err
	}
	var candidates []payloadCandidate
	if legacy, legacyErr := readLegacyPayload(cursor, accountIndex); legacyErr == nil {
		candidates = append(candidates, legacy)
	}
	compactCursor := &borshCursor{data: data}
	if _, skipErr := skipToPayload(compactCursor); skipErr != nil {
		return nil, skipErr
	}
	if compact, compactErr := readCompactPayload(compactCursor, accountIndex); compactErr == nil {
		candidates = append(candidates, compact)
	}
	// The Squads action-account PDA does not expose its bump separately here;
	// the Rust reader re-derives it. FindProgramAddress returns the bump, so
	// recompute it explicitly.
	policyAccount, expectedBump := deriveActionAccountWithBump(settings, policySeed)
	if len(signers) != 1 || permissions[0] != squadsFullPermissionsMask ||
		threshold != 1 || timeLock != 0 || staleTransactionIndex > transactionIndex ||
		policyBump != expectedBump {
		return nil, nil
	}
	var valid []PolicyPayloadView
	for _, candidate := range candidates {
		if !candidate.preHook && !candidate.postHook && candidate.exactSpendingLimits &&
			len(candidate.payload.SpendingLimits) == 0 &&
			candidate.start >= 0 && !candidate.hasExpiration &&
			compactPubkeyTableIsTight(candidate.payload) {
			valid = append(valid, candidate.payload)
		}
	}
	if len(valid) == 0 {
		return nil, nil
	}
	for _, candidate := range valid[1:] {
		if !payloadEqual(candidate, valid[0]) {
			return nil, errors.New("ambiguous ProgramInteraction account encoding")
		}
	}
	return &PolicyAccountView{
		Settings:        settings,
		PolicySeed:      policySeed,
		PolicyAccount:   policyAccount,
		DelegatedSigner: signers[0],
		Threshold:       threshold,
		Payload:         valid[0],
	}, nil
}

// payloadCandidate carries the hook/spending-limit/validity state the Rust
// reader tracks alongside each parsed encoding.
type payloadCandidate struct {
	payload             PolicyPayloadView
	preHook, postHook   bool
	exactSpendingLimits bool
	start               int64
	hasExpiration       bool
}

func skipToPayload(cursor *borshCursor) (uint8, error) {
	if _, err := cursor.take(8); err != nil {
		return 0, err
	}
	if _, err := cursor.pubkey(); err != nil {
		return 0, err
	}
	if _, err := cursor.u64(); err != nil {
		return 0, err
	}
	if _, err := cursor.u8(); err != nil {
		return 0, err
	}
	if _, err := cursor.u64(); err != nil {
		return 0, err
	}
	if _, err := cursor.u64(); err != nil {
		return 0, err
	}
	signerCount, err := cursor.u32()
	if err != nil {
		return 0, err
	}
	for i := uint32(0); i < signerCount; i++ {
		if _, err := cursor.take(33); err != nil {
			return 0, err
		}
	}
	if _, err := cursor.u16(); err != nil {
		return 0, err
	}
	if _, err := cursor.u32(); err != nil {
		return 0, err
	}
	return cursor.u8()
}

func readLegacyPayload(cursor *borshCursor, accountIndex uint8) (payloadCandidate, error) {
	constraintCount, err := cursor.u32Len()
	if err != nil {
		return payloadCandidate{}, err
	}
	constraints := make([]InstructionConstraintView, 0, constraintCount)
	for i := 0; i < constraintCount; i++ {
		constraint, err := readRawInstructionConstraint(cursor)
		if err != nil {
			return payloadCandidate{}, err
		}
		constraints = append(constraints, constraint)
	}
	preHook, err := skipHook(cursor, false, nil)
	if err != nil {
		return payloadCandidate{}, err
	}
	postHook, err := skipHook(cursor, false, nil)
	if err != nil {
		return payloadCandidate{}, err
	}
	limitCount, err := cursor.u32Len()
	if err != nil || limitCount > 128 {
		return payloadCandidate{}, errors.New("too many ProgramInteraction spending limits")
	}
	limits := make([]SpendingLimitView, 0, limitCount)
	exact := true
	for i := 0; i < limitCount; i++ {
		limit, limitExact, err := readLegacySpendingLimit(cursor)
		if err != nil {
			return payloadCandidate{}, err
		}
		limits = append(limits, limit)
		exact = exact && limitExact
	}
	start, hasExpiration, err := readPolicyAccountTail(cursor)
	if err != nil {
		return payloadCandidate{}, err
	}
	return payloadCandidate{
		payload: PolicyPayloadView{
			VaultIndex: accountIndex, Constraints: constraints, SpendingLimits: limits,
		},
		preHook: preHook, postHook: postHook, exactSpendingLimits: exact,
		start: start, hasExpiration: hasExpiration,
	}, nil
}

func readCompactPayload(cursor *borshCursor, accountIndex uint8) (payloadCandidate, error) {
	tableLen, err := cursor.u8Len()
	if err != nil {
		return payloadCandidate{}, err
	}
	if tableLen > 240 {
		return payloadCandidate{}, errors.New("ProgramInteraction pubkey table exceeds Squads limit")
	}
	table, err := cursor.pubkeys(tableLen)
	if err != nil {
		return payloadCandidate{}, err
	}
	constraintCount, err := cursor.u8Len()
	if err != nil {
		return payloadCandidate{}, err
	}
	constraints := make([]InstructionConstraintView, 0, constraintCount)
	for i := 0; i < constraintCount; i++ {
		constraint, err := readCompactInstructionConstraint(cursor, table)
		if err != nil {
			return payloadCandidate{}, err
		}
		constraints = append(constraints, constraint)
	}
	preHook, err := skipHook(cursor, true, table)
	if err != nil {
		return payloadCandidate{}, err
	}
	postHook, err := skipHook(cursor, true, table)
	if err != nil {
		return payloadCandidate{}, err
	}
	limitCount, err := cursor.u8Len()
	if err != nil {
		return payloadCandidate{}, err
	}
	limits := make([]SpendingLimitView, 0, limitCount)
	for i := 0; i < limitCount; i++ {
		limit, err := readCompactSpendingLimit(cursor, table)
		if err != nil {
			return payloadCandidate{}, err
		}
		limits = append(limits, limit)
	}
	start, hasExpiration, err := readPolicyAccountTail(cursor)
	if err != nil {
		return payloadCandidate{}, err
	}
	return payloadCandidate{
		payload: PolicyPayloadView{
			VaultIndex: accountIndex, PubkeyTable: table, Constraints: constraints,
			SpendingLimits: limits,
		},
		preHook: preHook, postHook: postHook, exactSpendingLimits: true,
		start: start, hasExpiration: hasExpiration,
	}, nil
}

func readRawInstructionConstraint(cursor *borshCursor) (InstructionConstraintView, error) {
	programID, err := cursor.pubkey()
	if err != nil {
		return InstructionConstraintView{}, err
	}
	accountCount, err := cursor.u32Len()
	if err != nil {
		return InstructionConstraintView{}, err
	}
	accounts := make([]AccountConstraintView, 0, accountCount)
	for i := 0; i < accountCount; i++ {
		account, err := readRawAccountConstraint(cursor)
		if err != nil {
			return InstructionConstraintView{}, err
		}
		accounts = append(accounts, account)
	}
	data, err := readDataConstraints(cursor, u32LenReader(cursor))
	if err != nil {
		return InstructionConstraintView{}, err
	}
	return InstructionConstraintView{ProgramID: programID, AccountConstraints: accounts, DataConstraints: data}, nil
}

func readRawAccountConstraint(cursor *borshCursor) (AccountConstraintView, error) {
	accountIndex, err := cursor.u8()
	if err != nil {
		return AccountConstraintView{}, err
	}
	kind, err := cursor.u8()
	if err != nil {
		return AccountConstraintView{}, err
	}
	view := AccountConstraintView{AccountIndex: accountIndex}
	switch kind {
	case 0:
		count, err := cursor.u32Len()
		if err != nil {
			return AccountConstraintView{}, err
		}
		if view.Pubkeys, err = cursor.pubkeys(count); err != nil {
			return AccountConstraintView{}, err
		}
	case 1:
		if view.AccountData, err = readDataConstraints(cursor, u32LenReader(cursor)); err != nil {
			return AccountConstraintView{}, err
		}
	default:
		return AccountConstraintView{}, errors.New("unknown account constraint kind")
	}
	hasOwner, err := cursor.option(func() error {
		key, err := cursor.pubkey()
		if err != nil {
			return err
		}
		view.Owner = &key
		return nil
	})
	if err != nil || !hasOwner {
		return view, err
	}
	return view, nil
}

func readCompactInstructionConstraint(cursor *borshCursor, table []solana.PublicKey) (InstructionConstraintView, error) {
	programIndex, err := cursor.u8()
	if err != nil {
		return InstructionConstraintView{}, err
	}
	programID, err := indexedKey(table, programIndex)
	if err != nil {
		return InstructionConstraintView{}, err
	}
	accountCount, err := cursor.u8Len()
	if err != nil {
		return InstructionConstraintView{}, err
	}
	accounts := make([]AccountConstraintView, 0, accountCount)
	for i := 0; i < accountCount; i++ {
		account, err := readCompactAccountConstraint(cursor, table)
		if err != nil {
			return InstructionConstraintView{}, err
		}
		accounts = append(accounts, account)
	}
	dataCount, err := cursor.u8Len()
	if err != nil {
		return InstructionConstraintView{}, err
	}
	data := make([]DataConstraintView, 0, dataCount)
	for i := 0; i < dataCount; i++ {
		constraint, err := readDataConstraint(cursor)
		if err != nil {
			return InstructionConstraintView{}, err
		}
		data = append(data, constraint)
	}
	return InstructionConstraintView{ProgramID: programID, AccountConstraints: accounts, DataConstraints: data}, nil
}

func readCompactAccountConstraint(cursor *borshCursor, table []solana.PublicKey) (AccountConstraintView, error) {
	accountIndex, err := cursor.u8()
	if err != nil {
		return AccountConstraintView{}, err
	}
	kind, err := cursor.u8()
	if err != nil {
		return AccountConstraintView{}, err
	}
	view := AccountConstraintView{AccountIndex: accountIndex}
	switch kind {
	case 0:
		count, err := cursor.u8Len()
		if err != nil {
			return AccountConstraintView{}, err
		}
		for i := 0; i < count; i++ {
			index, err := cursor.u8()
			if err != nil {
				return AccountConstraintView{}, err
			}
			key, err := indexedKey(table, index)
			if err != nil {
				return AccountConstraintView{}, err
			}
			view.Pubkeys = append(view.Pubkeys, key)
		}
	case 1:
		dataCount, err := cursor.u8Len()
		if err != nil {
			return AccountConstraintView{}, err
		}
		for i := 0; i < dataCount; i++ {
			constraint, err := readDataConstraint(cursor)
			if err != nil {
				return AccountConstraintView{}, err
			}
			view.AccountData = append(view.AccountData, constraint)
		}
	default:
		return AccountConstraintView{}, errors.New("unknown account constraint kind")
	}
	_, err = cursor.option(func() error {
		index, err := cursor.u8()
		if err != nil {
			return err
		}
		key, err := indexedKey(table, index)
		if err != nil {
			return err
		}
		view.Owner = &key
		return nil
	})
	return view, err
}

func u32LenReader(cursor *borshCursor) func() (int, error) {
	return cursor.u32Len
}

func readDataConstraints(cursor *borshCursor, length func() (int, error)) ([]DataConstraintView, error) {
	count, err := length()
	if err != nil {
		return nil, err
	}
	constraints := make([]DataConstraintView, 0, count)
	for i := 0; i < count; i++ {
		constraint, err := readDataConstraint(cursor)
		if err != nil {
			return nil, err
		}
		constraints = append(constraints, constraint)
	}
	return constraints, nil
}

func readDataConstraint(cursor *borshCursor) (DataConstraintView, error) {
	offset, err := cursor.u64()
	if err != nil {
		return DataConstraintView{}, err
	}
	kind, err := cursor.u8()
	if err != nil {
		return DataConstraintView{}, err
	}
	view := DataConstraintView{DataOffset: offset, Operator: OpEquals}
	switch kind {
	case 0:
		if view.DataValue.U8, err = cursor.u8(); err != nil {
			return DataConstraintView{}, err
		}
	case 1:
		if view.DataValue.U16, err = cursor.u16(); err != nil {
			return DataConstraintView{}, err
		}
	case 2:
		if view.DataValue.U32, err = cursor.u32(); err != nil {
			return DataConstraintView{}, err
		}
	case 3:
		if view.DataValue.U64, err = cursor.u64(); err != nil {
			return DataConstraintView{}, err
		}
	case 4:
		if view.DataValue.U128, err = cursor.u128(); err != nil {
			return DataConstraintView{}, err
		}
	case 5:
		length, err := cursor.u32Len()
		if err != nil {
			return DataConstraintView{}, err
		}
		if view.DataValue.Bytes, err = cursor.take(length); err != nil {
			return DataConstraintView{}, err
		}
	default:
		return DataConstraintView{}, errors.New("unknown data value kind")
	}
	view.DataValue.Kind = kind
	operator, err := cursor.u8()
	if err != nil {
		return DataConstraintView{}, err
	}
	if operator > 5 {
		return DataConstraintView{}, errors.New("unknown data operator")
	}
	view.Operator = DataOperatorView(operator)
	return view, nil
}

func readLegacySpendingLimit(cursor *borshCursor) (SpendingLimitView, bool, error) {
	mint, err := cursor.pubkey()
	if err != nil {
		return SpendingLimitView{}, false, err
	}
	start, err := cursor.i64()
	if err != nil {
		return SpendingLimitView{}, false, err
	}
	var expiration *int64
	_, err = cursor.option(func() error {
		value, err := cursor.i64()
		if err != nil {
			return err
		}
		expiration = &value
		return nil
	})
	if err != nil {
		return SpendingLimitView{}, false, err
	}
	period, custom, err := readPeriodV2(cursor)
	if err != nil {
		return SpendingLimitView{}, false, err
	}
	accumulateUnused, err := cursor.bool()
	if err != nil {
		return SpendingLimitView{}, false, err
	}
	maxPerPeriod, err := cursor.u64()
	if err != nil {
		return SpendingLimitView{}, false, err
	}
	maxPerUse, err := cursor.u64()
	if err != nil {
		return SpendingLimitView{}, false, err
	}
	enforceExactQuantity, err := cursor.bool()
	if err != nil {
		return SpendingLimitView{}, false, err
	}
	remainingInPeriod, err := cursor.u64()
	if err != nil {
		return SpendingLimitView{}, false, err
	}
	lastReset, err := cursor.i64()
	if err != nil {
		return SpendingLimitView{}, false, err
	}
	exact := !accumulateUnused && maxPerUse == 0 && !enforceExactQuantity &&
		remainingInPeriod <= maxPerPeriod && lastReset >= start
	return SpendingLimitView{
		Mint: mint, Start: start, Expiration: expiration, Period: period,
		CustomPeriod: custom, MaxPerPeriod: maxPerPeriod,
	}, exact, nil
}

func readCompactSpendingLimit(cursor *borshCursor, table []solana.PublicKey) (SpendingLimitView, error) {
	mintIndex, err := cursor.u8()
	if err != nil {
		return SpendingLimitView{}, err
	}
	mint, err := indexedKey(table, mintIndex)
	if err != nil {
		return SpendingLimitView{}, err
	}
	start, err := cursor.i64()
	if err != nil {
		return SpendingLimitView{}, err
	}
	var expiration *int64
	_, err = cursor.option(func() error {
		value, err := cursor.i64()
		if err != nil {
			return err
		}
		expiration = &value
		return nil
	})
	if err != nil {
		return SpendingLimitView{}, err
	}
	period, custom, err := readPeriodV2(cursor)
	if err != nil {
		return SpendingLimitView{}, err
	}
	maxPerPeriod, err := cursor.u64()
	if err != nil {
		return SpendingLimitView{}, err
	}
	return SpendingLimitView{
		Mint: mint, Start: start, Expiration: expiration, Period: period,
		CustomPeriod: custom, MaxPerPeriod: maxPerPeriod,
	}, nil
}

func readPeriodV2(cursor *borshCursor) (uint8, int64, error) {
	period, err := cursor.u8()
	if err != nil {
		return 0, 0, err
	}
	if period > 4 {
		return 0, 0, errors.New("unknown period v2")
	}
	var custom int64
	if period == 4 {
		if custom, err = cursor.i64(); err != nil {
			return 0, 0, err
		}
	}
	return period, custom, nil
}

func readPolicyAccountTail(cursor *borshCursor) (start int64, hasExpiration bool, err error) {
	if start, err = cursor.i64(); err != nil {
		return 0, false, err
	}
	present, err := cursor.option(func() error {
		kind, err := cursor.u8()
		if err != nil {
			return err
		}
		switch kind {
		case 0:
			_, err = cursor.take(8)
			return err
		case 1:
			_, err = cursor.take(32)
			return err
		default:
			return errors.New("unknown policy expiration")
		}
	})
	if err != nil {
		return 0, false, err
	}
	if _, err = cursor.pubkey(); err != nil {
		return 0, false, err
	}
	return start, present, nil
}

func skipHook(cursor *borshCursor, compact bool, table []solana.PublicKey) (bool, error) {
	return cursor.option(func() error {
		if _, err := cursor.u8(); err != nil {
			return err
		}
		if compact {
			count, err := cursor.u8Len()
			if err != nil {
				return err
			}
			for i := 0; i < count; i++ {
				if _, err := readCompactAccountConstraint(cursor, table); err != nil {
					return err
				}
			}
			if _, err := cursor.u8Len(); err != nil {
				return err
			}
			index, err := cursor.u8()
			if err != nil {
				return err
			}
			if _, err := indexedKey(table, index); err != nil {
				return err
			}
			_, err = cursor.bool()
			return err
		}
		count, err := cursor.u32Len()
		if err != nil {
			return err
		}
		for i := 0; i < count; i++ {
			if _, err := readRawAccountConstraint(cursor); err != nil {
				return err
			}
		}
		byteCount, err := cursor.u32Len()
		if err != nil {
			return err
		}
		if _, err := cursor.take(byteCount); err != nil {
			return err
		}
		if _, err := cursor.take(32); err != nil {
			return err
		}
		_, err = cursor.bool()
		return err
	})
}

func compactPubkeyTableIsTight(payload PolicyPayloadView) bool {
	if len(payload.PubkeyTable) == 0 {
		return true
	}
	seen := map[solana.PublicKey]struct{}{}
	for _, key := range payload.PubkeyTable {
		seen[key] = struct{}{}
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
	return len(referenced) == len(seen)
}

func payloadEqual(left, right PolicyPayloadView) bool {
	return left.VaultIndex == right.VaultIndex &&
		constraintsEqual(left.Constraints, right.Constraints)
}

func constraintsEqual(left, right []InstructionConstraintView) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index].ProgramID != right[index].ProgramID ||
			!accountConstraintsEqual(left[index].AccountConstraints, right[index].AccountConstraints) ||
			!dataConstraintsEqual(left[index].DataConstraints, right[index].DataConstraints) {
			return false
		}
	}
	return true
}

func accountConstraintsEqual(left, right []AccountConstraintView) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index].AccountIndex != right[index].AccountIndex {
			return false
		}
		if (left[index].Owner == nil) != (right[index].Owner == nil) {
			return false
		}
		if left[index].Owner != nil && *left[index].Owner != *right[index].Owner {
			return false
		}
		if !keysEqual(left[index].Pubkeys, right[index].Pubkeys) ||
			!dataConstraintsEqual(left[index].AccountData, right[index].AccountData) {
			return false
		}
	}
	return true
}

func keysEqual(left, right []solana.PublicKey) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func dataConstraintsEqual(left, right []DataConstraintView) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index].DataOffset != right[index].DataOffset ||
			left[index].Operator != right[index].Operator ||
			!dataValueEqual(left[index].DataValue, right[index].DataValue) {
			return false
		}
	}
	return true
}

func dataValueEqual(left, right DataValueView) bool {
	if left.Kind != right.Kind {
		return false
	}
	switch left.Kind {
	case 0:
		return left.U8 == right.U8
	case 1:
		return left.U16 == right.U16
	case 2:
		return left.U32 == right.U32
	case 3:
		return left.U64 == right.U64
	case 4:
		return left.U128 == right.U128
	case 5:
		return equalBytes(left.Bytes, right.Bytes)
	}
	return false
}

// CanonicalConstraints is the Go port of earn_max_policy_constraints over the
// semantic contract: literal KLend lane pinning for collateral/debt families
// and Jupiter SharedAccountsRoute lane pinning for swaps. Constraint indexes
// line up with ConstraintIndexes in policy.go.
func CanonicalConstraints(topology *EarnMaxTopology, family PolicyFamily) ([]InstructionConstraintView, error) {
	strategies := topology.StrategyCatalog()
	if len(strategies) == 0 {
		return nil, errors.New("earn max policy boundary has no lanes")
	}
	onyc, err := topology.Strategy(OnycUsdc)
	if err != nil {
		return nil, err
	}
	prime, err := topology.Strategy(PrimeUsdc)
	if err != nil {
		return nil, err
	}
	syrupUsdc, err := topology.Strategy(SyrupUsdcUsdc)
	if err != nil {
		return nil, err
	}
	usds, err := topology.Strategy(OnycUsds)
	if err != nil {
		return nil, err
	}
	pyusd, err := topology.Strategy(PrimePyusd)
	if err != nil {
		return nil, err
	}
	boundary := policyBoundary{
		vault:            topology.Vault,
		klendProgram:     mustKey(KlendProgram),
		jupiterProgram:   mustKey(JupiterProgram),
		usdcCustody:      onyc.DebtCustody,
		pyusdCustody:     pyusd.DebtCustody,
		usdsCustody:      usds.DebtCustody,
		onycCustody:      onyc.CollateralCustody,
		primeCustody:     prime.CollateralCustody,
		syrupUsdcCustody: syrupUsdc.CollateralCustody,
	}
	for _, strategy := range strategies {
		boundary.lanes = append(boundary.lanes, policyLane{
			market: strategy.Market, obligation: strategy.Obligation,
			collateralReserve: strategy.CollateralReserve, collateralCustody: strategy.CollateralCustody,
			debtReserve: strategy.DebtReserve, debtCustody: strategy.DebtCustody,
			debtTokenProgram: strategy.DebtTokenProgram,
		})
	}
	switch family {
	case FamilyCollateral:
		return []InstructionConstraintView{
			collateralConstraint(boundary, DiscriminatorDepositCollateral),
			collateralConstraint(boundary, DiscriminatorWithdrawCollateral),
		}, nil
	case FamilyDebt:
		return []InstructionConstraintView{
			{
				ProgramID: boundary.klendProgram,
				AccountConstraints: []AccountConstraintView{
					pinned(0, boundary.vault),
					pinned(2, uniqueKeys(laneKeys(boundary.lanes, func(lane policyLane) solana.PublicKey { return lane.market }))...),
					pinned(8, uniqueKeys(laneKeys(boundary.lanes, func(lane policyLane) solana.PublicKey { return lane.debtCustody }))...),
					obligationOwnedByVault(boundary),
				},
				DataConstraints: []DataConstraintView{sliceEquals(DiscriminatorBorrowDebt)},
			},
			{
				ProgramID: boundary.klendProgram,
				AccountConstraints: []AccountConstraintView{
					pinned(0, boundary.vault),
					pinned(2, uniqueKeys(laneKeys(boundary.lanes, func(lane policyLane) solana.PublicKey { return lane.market }))...),
					pinned(6, uniqueKeys(laneKeys(boundary.lanes, func(lane policyLane) solana.PublicKey { return lane.debtCustody }))...),
					obligationOwnedByVault(boundary),
				},
				DataConstraints: []DataConstraintView{sliceEquals(DiscriminatorRepayDebt)},
			},
		}, nil
	case FamilySwap:
		return []InstructionConstraintView{
			swapConstraint(boundary,
				[]solana.PublicKey{boundary.usdcCustody, boundary.usdsCustody},
				[]solana.PublicKey{boundary.onycCustody, boundary.primeCustody}),
			swapConstraint(boundary,
				[]solana.PublicKey{boundary.usdcCustody, boundary.pyusdCustody},
				[]solana.PublicKey{boundary.primeCustody, boundary.syrupUsdcCustody}),
			swapConstraint(boundary,
				[]solana.PublicKey{boundary.onycCustody, boundary.primeCustody},
				[]solana.PublicKey{boundary.usdcCustody, boundary.usdsCustody}),
			swapConstraint(boundary,
				[]solana.PublicKey{boundary.primeCustody, boundary.syrupUsdcCustody},
				[]solana.PublicKey{boundary.usdcCustody, boundary.pyusdCustody}),
		}, nil
	}
	return nil, fmt.Errorf("unknown policy family %q", family)
}

type policyLane struct {
	market, obligation, collateralReserve, collateralCustody,
	debtReserve, debtCustody, debtTokenProgram solana.PublicKey
}

type policyBoundary struct {
	vault, klendProgram, jupiterProgram,
	usdcCustody, pyusdCustody, usdsCustody,
	onycCustody, primeCustody, syrupUsdcCustody solana.PublicKey
	lanes []policyLane
}

func collateralConstraint(boundary policyBoundary, discriminator [8]byte) InstructionConstraintView {
	return InstructionConstraintView{
		ProgramID: boundary.klendProgram,
		AccountConstraints: []AccountConstraintView{
			pinned(0, boundary.vault),
			pinned(4, uniqueKeys(laneKeys(boundary.lanes, func(lane policyLane) solana.PublicKey { return lane.collateralReserve }))...),
			pinned(9, uniqueKeys(laneKeys(boundary.lanes, func(lane policyLane) solana.PublicKey { return lane.collateralCustody }))...),
			obligationOwnedByVault(boundary),
		},
		DataConstraints: []DataConstraintView{sliceEquals(discriminator)},
	}
}

func swapConstraint(boundary policyBoundary, sources, destinations []solana.PublicKey) InstructionConstraintView {
	return InstructionConstraintView{
		ProgramID: boundary.jupiterProgram,
		AccountConstraints: []AccountConstraintView{
			pinned(2, boundary.vault),
			pinned(3, sources...),
			pinned(6, destinations...),
		},
		DataConstraints: []DataConstraintView{{
			DataOffset: 0,
			DataValue: DataValueView{Kind: 1, U16: binary.LittleEndian.Uint16(
				JupiterSharedAccountsRouteDiscriminator[:2])},
			Operator: OpEquals,
		}},
	}
}

func obligationOwnedByVault(boundary policyBoundary) AccountConstraintView {
	vault := boundary.vault
	return AccountConstraintView{
		AccountIndex: 1,
		Owner:        &boundary.klendProgram,
		AccountData: []DataConstraintView{{
			DataOffset: 64,
			DataValue:  DataValueView{Kind: 5, Bytes: append([]byte(nil), vault[:]...)},
			Operator:   OpEquals,
		}},
	}
}

func pinned(accountIndex uint8, keys ...solana.PublicKey) AccountConstraintView {
	return AccountConstraintView{AccountIndex: accountIndex, Pubkeys: keys}
}

func sliceEquals(discriminator [8]byte) DataConstraintView {
	return DataConstraintView{
		DataOffset: 0,
		DataValue:  DataValueView{Kind: 5, Bytes: append([]byte(nil), discriminator[:]...)},
		Operator:   OpEquals,
	}
}

func laneKeys(lanes []policyLane, pick func(policyLane) solana.PublicKey) []solana.PublicKey {
	keys := make([]solana.PublicKey, 0, len(lanes))
	for _, lane := range lanes {
		keys = append(keys, pick(lane))
	}
	return keys
}

func uniqueKeys(keys []solana.PublicKey) []solana.PublicKey {
	unique := make([]solana.PublicKey, 0, len(keys))
	for _, key := range keys {
		duplicate := false
		for _, existing := range unique {
			if existing == key {
				duplicate = true
				break
			}
		}
		if !duplicate {
			unique = append(unique, key)
		}
	}
	return unique
}

// CurrentPolicyMatches is the Go port of current_policy_matches: the on-chain
// policy account must be a canonical hookless ProgramInteraction policy under
// the expected PDA, delegating to exactly this signer with threshold 1, and
// its payload must equal the canonical constraint set.
func CurrentPolicyMatches(data []byte, policy PolicyConfig, delegate solana.PublicKey, expected []InstructionConstraintView, expectedVaultIndex uint8) (bool, error) {
	current, err := DecodeProgramInteractionPolicyAccount(data)
	if err != nil {
		return false, err
	}
	if current == nil {
		return false, nil
	}
	return current.PolicySeed == policy.Seed &&
		current.PolicyAccount == policy.Account &&
		current.DelegatedSigner == delegate &&
		current.Threshold == 1 &&
		current.Payload.VaultIndex == expectedVaultIndex &&
		len(current.Payload.SpendingLimits) == 0 &&
		constraintsEqual(current.Payload.Constraints, expected), nil
}

// PolicyDataHash hashes the policy account bytes for the persisted binding.
func PolicyDataHash(data []byte) string {
	digest := sha256.Sum256(data)
	return fmt.Sprintf("%x", digest[:])
}
