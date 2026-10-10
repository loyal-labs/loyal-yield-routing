package squads

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/solana-foundation/solana-go/v2"
)

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

// SpendingLimitView mirrors SquadsLimitedSpendingLimitView, with the
// allowance rules of the legacy account layout (the compact layout has none:
// they read zero there). Its usage counters are Squads' running state, not
// what the policy allows, so it does not carry them.
type SpendingLimitView struct {
	Mint          solana.PublicKey
	Start         int64
	Expiration    *int64
	Period        uint8 // 0 one-time, 1 daily, 2 weekly, 3 monthly, 4 custom
	CustomPeriod  int64
	MaxPerPeriod  uint64
	Accumulate    bool   // an unspent allowance carries into the next period
	MaxPerUse     uint64 // 0 when one use may spend the whole allowance
	ExactQuantity bool   // a use spends exactly MaxPerUse
}

// Policy is what a ProgramInteraction policy authorizes: the smart account
// (vault index) it executes as, its constraints in index order, and its
// spending limits. A worker writes the policy it executes through as one, and
// finds the installed account by Equal (FindPolicy).
type Policy struct {
	VaultIndex     uint8
	Constraints    []InstructionConstraintView
	SpendingLimits []SpendingLimitView
}

// Equal reports whether p and q authorize the same: vault index, constraints
// and spending limits.
func (p Policy) Equal(q Policy) bool {
	return p.VaultIndex == q.VaultIndex && ConstraintsEqual(p.Constraints, q.Constraints) && SpendingLimitsEqual(p.SpendingLimits, q.SpendingLimits)
}

// PolicyPayloadView mirrors SquadsProgramInteractionPolicyView: the policy and,
// in the compact layout, the pubkey table its keys index.
type PolicyPayloadView struct {
	Policy
	PubkeyTable []solana.PublicKey
}

// FullPayload is a ProgramInteraction payload decoded through its tail.
type FullPayload struct {
	Payload           PolicyPayloadView
	PreHook, PostHook bool
	Start             int64
	HasExpiration     bool
}

type borshCursor struct {
	data                []byte
	offset              int
	maxVector, maxBytes int
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
	if value > uint32(c.maxVector) || int(value) > len(c.data)-c.offset {
		return 0, errors.New("policy vector length exceeds bounded payload")
	}
	return int(value), nil
}

func (c *borshCursor) u8Len() (int, error) {
	value, err := c.u8()
	if err != nil {
		return 0, err
	}
	if int(value) > c.maxVector || int(value) > len(c.data)-c.offset {
		return 0, errors.New("policy vector length exceeds bounded payload")
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

type header struct {
	Settings                                solana.PublicKey
	PolicySeed                              uint64
	Bump                                    uint8
	TransactionIndex, StaleTransactionIndex uint64
	Signers                                 []solana.PublicKey
	Permissions                             []uint8
	Threshold                               uint16
	TimeLock                                uint32
	Kind, VaultIndex                        uint8
}

func decodeHeader(data []byte) (header, int, error) {
	cursor := &borshCursor{data: data}
	discriminator, err := cursor.take(8)
	if err != nil {
		return header{}, 0, err
	}
	if !bytes.Equal(discriminator, PolicyDiscriminator[:]) {
		return header{}, 0, errors.New("account discriminator is not a Squads Policy account")
	}
	settings, err := cursor.pubkey()
	if err != nil {
		return header{}, 0, err
	}
	policySeed, err := cursor.u64()
	if err != nil {
		return header{}, 0, err
	}
	policyBump, err := cursor.u8()
	if err != nil {
		return header{}, 0, err
	}
	transactionIndex, err := cursor.u64()
	if err != nil {
		return header{}, 0, err
	}
	staleTransactionIndex, err := cursor.u64()
	if err != nil {
		return header{}, 0, err
	}
	signerCount, err := cursor.u32()
	if err != nil {
		return header{}, 0, err
	}
	if signerCount > 32 {
		return header{}, 0, errors.New("too many policy signers")
	}
	signers := make([]solana.PublicKey, 0, signerCount)
	permissions := make([]uint8, 0, signerCount)
	for i := uint32(0); i < signerCount; i++ {
		key, err := cursor.pubkey()
		if err != nil {
			return header{}, 0, err
		}
		mask, err := cursor.u8()
		if err != nil {
			return header{}, 0, err
		}
		signers = append(signers, key)
		permissions = append(permissions, mask)
	}
	threshold, err := cursor.u16()
	if err != nil {
		return header{}, 0, err
	}
	timeLock, err := cursor.u32()
	if err != nil {
		return header{}, 0, err
	}
	kind, err := cursor.u8()
	if err != nil {
		return header{}, 0, err
	}
	// Non-ProgramInteraction kinds have no ProgramInteraction vault-index byte.
	if kind != 3 {
		return header{Settings: settings, PolicySeed: policySeed, Bump: policyBump, TransactionIndex: transactionIndex, StaleTransactionIndex: staleTransactionIndex, Signers: signers, Permissions: permissions, Threshold: threshold, TimeLock: timeLock, Kind: kind}, cursor.offset, nil
	}
	accountIndex, err := cursor.u8()
	if err != nil {
		return header{}, 0, err
	}
	return header{settings, policySeed, policyBump, transactionIndex, staleTransactionIndex, signers, permissions, threshold, timeLock, kind, accountIndex}, cursor.offset, nil
}

// DecodedPolicy is a ProgramInteraction Policy account: its header and
// instruction constraints. Full is the payload decoded through hooks, spending
// limits and the account tail in the same layout; nil when that tail does not
// decode.
type DecodedPolicy struct {
	Settings                                solana.PublicKey
	Seed                                    uint64
	Bump                                    uint8
	TransactionIndex, StaleTransactionIndex uint64
	Signers                                 []solana.PublicKey
	Permissions                             []uint8
	Threshold                               uint16
	TimeLock                                uint32
	VaultIndex                              uint8
	Constraints                             []InstructionConstraintView
	PubkeyTable                             []solana.PublicKey
	Full                                    *FullPayload
}

// DecodeProgramInteractionPolicy decodes the constraint prefix in the legacy
// Borsh layout, or in the compact pubkey-table layout when the legacy read
// fails or is empty; it never grants authority on its own.
func DecodeProgramInteractionPolicy(account *chain.Account) (DecodedPolicy, error) {
	if account == nil || account.Owner != ProgramID || account.Executable {
		return DecodedPolicy{}, errors.New("policy account is absent or not owned by Squads")
	}
	data := account.Data
	h, offset, err := decodeHeader(data)
	if err != nil {
		return DecodedPolicy{}, err
	}
	if h.Kind != 3 {
		return DecodedPolicy{}, errors.New("policy is not ProgramInteraction")
	}
	p := DecodedPolicy{Settings: h.Settings, Seed: h.PolicySeed, Bump: h.Bump, TransactionIndex: h.TransactionIndex, StaleTransactionIndex: h.StaleTransactionIndex, Signers: h.Signers, Permissions: h.Permissions, Threshold: h.Threshold, TimeLock: h.TimeLock, VaultIndex: h.VaultIndex}
	var payload PolicyPayloadView
	compact := false
	for _, layout := range []bool{false, true} {
		compact = layout
		cursor := &borshCursor{data: data, offset: offset, maxVector: 128, maxBytes: 256}
		if payload, err = readConstraints(cursor, h.VaultIndex, compact); err == nil && len(payload.Constraints) > 0 {
			break
		}
	}
	if err != nil || len(payload.Constraints) == 0 {
		return p, fmt.Errorf("decode ProgramInteraction constraints: %w", err)
	}
	p.Constraints, p.PubkeyTable = payload.Constraints, payload.PubkeyTable
	if full, err := decodePayload(data, offset, h.VaultIndex, compact); err == nil {
		p.Full = &full
	}
	return p, nil
}

func decodePayload(data []byte, offset int, vaultIndex uint8, compact bool) (FullPayload, error) {
	if len(data) > 64<<10 || offset < 0 || offset > len(data) {
		return FullPayload{}, errors.New("policy account exceeds payload bounds")
	}
	cursor := &borshCursor{data: data, offset: offset, maxVector: 4096, maxBytes: 4096}
	if compact {
		return readCompactPayload(cursor, vaultIndex)
	}
	return readLegacyPayload(cursor, vaultIndex)
}

// readConstraints decodes only the constraint prefix. Fleet intentionally accepts
// a prefix without hooks, spending limits or account tail; Multiply requires the
// complete payload. Neither decoding mode grants execution authority.
func readConstraints(cursor *borshCursor, accountIndex uint8, compact bool) (PolicyPayloadView, error) {
	if !compact {
		constraintCount, err := cursor.u32Len()
		if err != nil {
			return PolicyPayloadView{}, err
		}
		constraints := make([]InstructionConstraintView, 0, constraintCount)
		for i := 0; i < constraintCount; i++ {
			constraint, err := readRawInstructionConstraint(cursor)
			if err != nil {
				return PolicyPayloadView{}, err
			}
			constraints = append(constraints, constraint)
		}
		return PolicyPayloadView{Policy: Policy{VaultIndex: accountIndex, Constraints: constraints}}, nil
	}
	tableCount, err := cursor.u8()
	tableLen := int(tableCount)
	if err != nil {
		return PolicyPayloadView{}, err
	}
	if tableLen > 240 {
		return PolicyPayloadView{}, errors.New("ProgramInteraction pubkey table exceeds Squads limit")
	}
	table, err := cursor.pubkeys(tableLen)
	if err != nil {
		return PolicyPayloadView{}, err
	}
	constraintCount, err := cursor.u8Len()
	if err != nil {
		return PolicyPayloadView{}, err
	}
	constraints := make([]InstructionConstraintView, 0, constraintCount)
	for i := 0; i < constraintCount; i++ {
		constraint, err := readCompactInstructionConstraint(cursor, table)
		if err != nil {
			return PolicyPayloadView{}, err
		}
		constraints = append(constraints, constraint)
	}
	return PolicyPayloadView{Policy: Policy{VaultIndex: accountIndex, Constraints: constraints}, PubkeyTable: table}, nil
}
func readLegacyPayload(cursor *borshCursor, accountIndex uint8) (FullPayload, error) {
	payload, err := readConstraints(cursor, accountIndex, false)
	if err != nil {
		return FullPayload{}, err
	}
	preHook, err := skipHook(cursor, false, nil)
	if err != nil {
		return FullPayload{}, err
	}
	postHook, err := skipHook(cursor, false, nil)
	if err != nil {
		return FullPayload{}, err
	}
	limitCount, err := cursor.u32Len()
	if err != nil || limitCount > 128 {
		return FullPayload{}, errors.New("too many ProgramInteraction spending limits")
	}
	limits := make([]SpendingLimitView, 0, limitCount)
	for i := 0; i < limitCount; i++ {
		limit, err := readLegacySpendingLimit(cursor)
		if err != nil {
			return FullPayload{}, err
		}
		limits = append(limits, limit)
	}
	start, hasExpiration, err := readPolicyAccountTail(cursor)
	if err != nil {
		return FullPayload{}, err
	}
	payload.SpendingLimits = limits
	return FullPayload{Payload: payload, PreHook: preHook, PostHook: postHook, Start: start, HasExpiration: hasExpiration}, nil
}

func readCompactPayload(cursor *borshCursor, accountIndex uint8) (FullPayload, error) {
	payload, err := readConstraints(cursor, accountIndex, true)
	if err != nil {
		return FullPayload{}, err
	}
	preHook, err := skipHook(cursor, true, payload.PubkeyTable)
	if err != nil {
		return FullPayload{}, err
	}
	postHook, err := skipHook(cursor, true, payload.PubkeyTable)
	if err != nil {
		return FullPayload{}, err
	}
	limitCount, err := cursor.u8Len()
	if err != nil {
		return FullPayload{}, err
	}
	limits := make([]SpendingLimitView, 0, limitCount)
	for i := 0; i < limitCount; i++ {
		limit, err := readCompactSpendingLimit(cursor, payload.PubkeyTable)
		if err != nil {
			return FullPayload{}, err
		}
		limits = append(limits, limit)
	}
	start, hasExpiration, err := readPolicyAccountTail(cursor)
	if err != nil {
		return FullPayload{}, err
	}
	payload.SpendingLimits = limits
	return FullPayload{Payload: payload, PreHook: preHook, PostHook: postHook, Start: start, HasExpiration: hasExpiration}, nil
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
		// A pinned-key constraint stays a pinned-key constraint when empty.
		view.Pubkeys = make([]solana.PublicKey, 0, count)
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
		lengthRaw, err := cursor.u32()
		length := int(lengthRaw)
		if err != nil {
			return DataConstraintView{}, err
		}
		if lengthRaw > uint32(cursor.maxBytes) {
			return DataConstraintView{}, errors.New("policy byte constraint exceeds limit")
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

// readLegacySpendingLimit reads one legacy account spending limit; its usage
// counters (remaining in period, last reset) are skipped.
func readLegacySpendingLimit(cursor *borshCursor) (SpendingLimitView, error) {
	var limit SpendingLimitView
	var err error
	if limit.Mint, err = cursor.pubkey(); err != nil {
		return SpendingLimitView{}, err
	}
	if limit.Start, err = cursor.i64(); err != nil {
		return SpendingLimitView{}, err
	}
	if _, err = cursor.option(func() error {
		value, err := cursor.i64()
		limit.Expiration = &value
		return err
	}); err != nil {
		return SpendingLimitView{}, err
	}
	if limit.Period, limit.CustomPeriod, err = readPeriodV2(cursor); err != nil {
		return SpendingLimitView{}, err
	}
	if limit.Accumulate, err = cursor.bool(); err != nil {
		return SpendingLimitView{}, err
	}
	if limit.MaxPerPeriod, err = cursor.u64(); err != nil {
		return SpendingLimitView{}, err
	}
	if limit.MaxPerUse, err = cursor.u64(); err != nil {
		return SpendingLimitView{}, err
	}
	if limit.ExactQuantity, err = cursor.bool(); err != nil {
		return SpendingLimitView{}, err
	}
	if _, err = cursor.take(8 + 8); err != nil { // remaining_in_period, last_reset
		return SpendingLimitView{}, err
	}
	return limit, nil
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
