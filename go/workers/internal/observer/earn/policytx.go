package earn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gagliardetto/solana-go"
	pb "github.com/helius-labs/laserstream-sdk/go/proto"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/multiply"
	sp "github.com/loyal-labs/loyal-yield-routing/go/workers/internal/squadspolicy"
	"github.com/mr-tron/base58"
)

// Policy transaction decoding, ported from earn_reconciliation.rs
// decode_laserstream_squads_policy_transaction,
// decode_json_squads_policy_transaction, parse_earn_max_intent and
// project_earn_max_memos.

var memoProgram = solana.MustPublicKeyFromBase58("MemoSq4gqABAXKb96qnH8TysNcWxMyWCqXgDLGmfcHr")

// Memo is one Earn MAX memo instruction with its chain location
// (outer index * 256, or group index * 256 + inner index + 1).
type Memo struct {
	SourceIndex uint16
	Accounts    []solana.PublicKey
	Data        []byte
}

// PolicyTransaction is one successful transaction's Squads and
// Subscriptions instructions plus its Earn MAX memos.
type PolicyTransaction struct {
	Signature    string
	Slot         uint64
	Signers      []solana.PublicKey
	Instructions []sp.Instruction
	Memos        []Memo
}

type accountTable struct {
	keys                                               []solana.PublicKey
	staticLen, loadedWritable                          int
	requiredSigners, readonlySigners, readonlyUnsigned int
}

// meta mirrors the account_meta closure: signer and writable flags follow
// the message header and the loaded-address split.
func (t accountTable) meta(index int) (sp.AccountMeta, bool) {
	if index < 0 || index >= len(t.keys) {
		return sp.AccountMeta{}, false
	}
	signer := index < t.requiredSigners
	var writable bool
	switch {
	case signer:
		writable = index < max(t.requiredSigners-t.readonlySigners, 0)
	case index < t.staticLen:
		writable = index < max(t.staticLen-t.readonlyUnsigned, 0)
	default:
		writable = index < t.staticLen+t.loadedWritable
	}
	return sp.AccountMeta{PublicKey: t.keys[index], IsSigner: signer, IsWritable: writable}, true
}

func (t accountTable) metas(indexes []byte) []sp.AccountMeta {
	out := make([]sp.AccountMeta, 0, len(indexes))
	for _, index := range indexes {
		if meta, ok := t.meta(int(index)); ok {
			out = append(out, meta)
		}
	}
	return out
}

func (t accountTable) signers() []solana.PublicKey {
	return append([]solana.PublicKey(nil), t.keys[:min(t.requiredSigners, len(t.keys))]...)
}

func isPolicyProgram(program solana.PublicKey) bool {
	return program == sp.Program || program == subscriptionsProgram
}

func innerMemoIndex(group uint32, inner int) (uint16, error) {
	value := uint64(group)*256 + uint64(inner) + 1
	if group > 0xffff || value > 0xffff {
		return 0, errors.New("Earn MAX memo instruction index overflow")
	}
	return uint16(value), nil
}

// DecodeStreamPolicyTransaction returns nil for a failed transaction.
func DecodeStreamPolicyTransaction(info *pb.SubscribeUpdateTransactionInfo, slot uint64) (*PolicyTransaction, error) {
	if len(info.GetSignature()) != 64 {
		return nil, errors.New("LaserStream policy transaction signature")
	}
	signature := solana.SignatureFromBytes(info.GetSignature()).String()
	meta := info.GetMeta()
	if meta == nil {
		return nil, errors.New("LaserStream policy transaction metadata was missing")
	}
	if meta.GetErr() != nil {
		return nil, nil
	}
	message := info.GetTransaction().GetMessage()
	if message == nil {
		return nil, errors.New("LaserStream policy transaction message was missing")
	}
	header := message.GetHeader()
	if header == nil {
		return nil, errors.New("LaserStream policy transaction header was missing")
	}
	table := accountTable{staticLen: len(message.GetAccountKeys()), loadedWritable: len(meta.GetLoadedWritableAddresses()),
		requiredSigners: int(header.GetNumRequiredSignatures()), readonlySigners: int(header.GetNumReadonlySignedAccounts()),
		readonlyUnsigned: int(header.GetNumReadonlyUnsignedAccounts())}
	for _, group := range [][][]byte{message.GetAccountKeys(), meta.GetLoadedWritableAddresses(), meta.GetLoadedReadonlyAddresses()} {
		for _, key := range group {
			if len(key) != 32 {
				return nil, errors.New("LaserStream account key")
			}
			table.keys = append(table.keys, solana.PublicKeyFromBytes(key))
		}
	}
	out := &PolicyTransaction{Signature: signature, Slot: slot, Signers: table.signers()}
	for outer, compiled := range message.GetInstructions() {
		index := int(compiled.GetProgramIdIndex())
		if index >= len(table.keys) {
			continue
		}
		program := table.keys[index]
		if program == memoProgram {
			if outer > 0xff {
				return nil, errors.New("Earn MAX outer memo instruction index overflow")
			}
			var accounts []solana.PublicKey
			for _, account := range compiled.GetAccounts() {
				if int(account) < len(table.keys) {
					accounts = append(accounts, table.keys[account])
				}
			}
			out.Memos = append(out.Memos, Memo{SourceIndex: uint16(outer) * 256, Accounts: accounts, Data: compiled.GetData()})
			continue
		}
		if isPolicyProgram(program) {
			out.Instructions = append(out.Instructions, sp.Instruction{ProgramID: program, Accounts: table.metas(compiled.GetAccounts()), Data: compiled.GetData()})
		}
	}
	for _, group := range meta.GetInnerInstructions() {
		for inner, instruction := range group.GetInstructions() {
			index := int(instruction.GetProgramIdIndex())
			if index >= len(table.keys) {
				continue
			}
			program := table.keys[index]
			accounts := table.metas(instruction.GetAccounts())
			if isPolicyProgram(program) {
				out.Instructions = append(out.Instructions, sp.Instruction{ProgramID: program, Accounts: accounts, Data: instruction.GetData()})
			}
			if program != memoProgram {
				continue
			}
			source, err := innerMemoIndex(group.GetIndex(), inner)
			if err != nil {
				return nil, err
			}
			out.Memos = append(out.Memos, memoFrom(source, accounts, instruction.GetData()))
		}
	}
	return out, nil
}

func memoFrom(source uint16, accounts []sp.AccountMeta, data []byte) Memo {
	keys := make([]solana.PublicKey, 0, len(accounts))
	for _, account := range accounts {
		keys = append(keys, account.PublicKey)
	}
	return Memo{SourceIndex: source, Accounts: keys, Data: data}
}

type rpcCompiled struct {
	ProgramIDIndex int    `json:"programIdIndex"`
	Accounts       []int  `json:"accounts"`
	Data           string `json:"data"`
}

type rpcPolicyTransaction struct {
	Slot        uint64 `json:"slot"`
	Transaction struct {
		Message *struct {
			Header struct {
				NumRequiredSignatures       int `json:"numRequiredSignatures"`
				NumReadonlySignedAccounts   int `json:"numReadonlySignedAccounts"`
				NumReadonlyUnsignedAccounts int `json:"numReadonlyUnsignedAccounts"`
			} `json:"header"`
			AccountKeys  []string      `json:"accountKeys"`
			Instructions []rpcCompiled `json:"instructions"`
		} `json:"message"`
	} `json:"transaction"`
	Meta *struct {
		Err             json.RawMessage `json:"err"`
		LoadedAddresses *struct {
			Writable []string `json:"writable"`
			Readonly []string `json:"readonly"`
		} `json:"loadedAddresses"`
		InnerInstructions []struct {
			Index        uint32        `json:"index"`
			Instructions []rpcCompiled `json:"instructions"`
		} `json:"innerInstructions"`
	} `json:"meta"`
}

func byteIndexes(values []int) []byte {
	out := make([]byte, 0, len(values))
	for _, value := range values {
		if value >= 0 && value <= 0xff {
			out = append(out, byte(value))
		}
	}
	return out
}

// decodeRPCPolicyTransaction decodes a raw "json" encoded confirmed
// transaction. Only inner Earn MAX memos are collected on this path.
func decodeRPCPolicyTransaction(raw json.RawMessage, signature string, expectedSlot uint64) (*PolicyTransaction, error) {
	var transaction rpcPolicyTransaction
	if err := json.Unmarshal(raw, &transaction); err != nil {
		return nil, err
	}
	if transaction.Slot != expectedSlot {
		return nil, fmt.Errorf("transaction %s landed at slot %d, expected %d", signature, transaction.Slot, expectedSlot)
	}
	meta := transaction.Meta
	if meta == nil {
		return nil, errors.New("policy transaction has no status metadata")
	}
	if len(meta.Err) > 0 && string(meta.Err) != "null" {
		return nil, nil
	}
	message := transaction.Transaction.Message
	if message == nil {
		return nil, errors.New("policy transaction message was not returned raw")
	}
	table := accountTable{staticLen: len(message.AccountKeys), requiredSigners: message.Header.NumRequiredSignatures,
		readonlySigners: message.Header.NumReadonlySignedAccounts, readonlyUnsigned: message.Header.NumReadonlyUnsignedAccounts}
	keys := message.AccountKeys
	if meta.LoadedAddresses != nil {
		table.loadedWritable = len(meta.LoadedAddresses.Writable)
		keys = append(append(append([]string(nil), keys...), meta.LoadedAddresses.Writable...), meta.LoadedAddresses.Readonly...)
	}
	for _, value := range keys {
		key, err := solana.PublicKeyFromBase58(value)
		if err != nil {
			return nil, fmt.Errorf("policy transaction account key: %w", err)
		}
		table.keys = append(table.keys, key)
	}
	out := &PolicyTransaction{Signature: signature, Slot: expectedSlot, Signers: table.signers()}
	for _, compiled := range message.Instructions {
		if compiled.ProgramIDIndex < 0 || compiled.ProgramIDIndex >= len(table.keys) {
			continue
		}
		program := table.keys[compiled.ProgramIDIndex]
		if !isPolicyProgram(program) {
			continue
		}
		data, err := base58.Decode(compiled.Data)
		if err != nil {
			return nil, err
		}
		out.Instructions = append(out.Instructions, sp.Instruction{ProgramID: program, Accounts: table.metas(byteIndexes(compiled.Accounts)), Data: data})
	}
	for _, group := range meta.InnerInstructions {
		for inner, compiled := range group.Instructions {
			if compiled.ProgramIDIndex < 0 || compiled.ProgramIDIndex >= len(table.keys) {
				continue
			}
			program := table.keys[compiled.ProgramIDIndex]
			accounts := table.metas(byteIndexes(compiled.Accounts))
			data, err := base58.Decode(compiled.Data)
			if err != nil {
				return nil, err
			}
			if isPolicyProgram(program) {
				out.Instructions = append(out.Instructions, sp.Instruction{ProgramID: program, Accounts: accounts, Data: data})
			}
			if program != memoProgram {
				continue
			}
			if group.Index > 0xff {
				return nil, errors.New("Earn MAX memo instruction index overflow")
			}
			source, err := innerMemoIndex(group.Index, inner)
			if err != nil {
				return nil, err
			}
			out.Memos = append(out.Memos, memoFrom(source, accounts, data))
		}
	}
	return out, nil
}

func validRequestID(value string) bool {
	if len(value) < 8 || len(value) > 64 {
		return false
	}
	for _, c := range []byte(value) {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// parseEarnMaxIntent is parse_earn_max_intent.
func parseEarnMaxIntent(data []byte) (*EarnMaxIntent, error) {
	if !utf8.Valid(data) {
		return nil, nil
	}
	fields := strings.Split(string(data), ":")
	if len(fields) < 3 || fields[0] != "loyal" || fields[1] != "earn-max" || fields[2] != "v2" {
		return nil, nil
	}
	switch {
	case len(fields) == 7 && fields[3] == "withdraw" && validRequestID(fields[4]):
		if _, err := solana.PublicKeyFromBase58(fields[6]); err != nil {
			return nil, fmt.Errorf("invalid Earn MAX withdrawal destination: %w", err)
		}
		var amount *uint64
		if fields[5] != "max" {
			// Rust u64::from_str accepts one leading '+'.
			parsed, err := strconv.ParseUint(strings.TrimPrefix(fields[5], "+"), 10, 64)
			if err != nil {
				return nil, errors.New("invalid Earn MAX withdrawal amount")
			}
			if parsed == 0 {
				return nil, errors.New("Earn MAX withdrawal amount is zero")
			}
			amount = &parsed
		}
		return &EarnMaxIntent{Withdraw: &EarnMaxWithdrawIntent{RequestID: fields[4], DestinationAccount: fields[6], AmountRaw: amount}}, nil
	case len(fields) == 5 && fields[3] == "cancel" && validRequestID(fields[4]):
		return &EarnMaxIntent{Cancel: &EarnMaxCancelIntent{RequestID: fields[4]}}, nil
	case len(fields) >= 4 && (fields[3] == "deposit" || fields[3] == "claim"):
		return nil, nil
	}
	return nil, errors.New("malformed Earn MAX intent memo")
}

func intentInput(settings string, vaultIndex uint8, transaction *PolicyTransaction, memo Memo, intent *EarnMaxIntent) multiply.IntentInput {
	input := multiply.IntentInput{Settings: settings, VaultIndex: vaultIndex, Signature: transaction.Signature,
		InstructionIndex: memo.SourceIndex, Slot: transaction.Slot, ObservedAt: time.Now().UTC()}
	if intent.Withdraw != nil {
		input.Withdraw = &struct {
			RequestID, DestinationAccount string
			AmountRaw                     *uint64
		}{intent.Withdraw.RequestID, intent.Withdraw.DestinationAccount, intent.Withdraw.AmountRaw}
	} else {
		input.CancelRequestID = &intent.Cancel.RequestID
	}
	return input
}

// projectEarnMaxMemos is project_earn_max_memos: a memo projects only when
// exactly one instruction account is the settings of a memo vault.
func projectEarnMaxMemos(ctx context.Context, store *multiply.Store, transaction *PolicyTransaction) (int, error) {
	applied := 0
	for _, memo := range transaction.Memos {
		intent, err := parseEarnMaxIntent(memo.Data)
		if err != nil {
			return applied, err
		}
		if intent == nil {
			continue
		}
		matches := map[[2]solana.PublicKey]struct{}{}
		for _, vault := range memo.Accounts {
			for _, instruction := range transaction.Instructions {
				for _, account := range instruction.Accounts {
					if squadsVault(account.PublicKey, 0) == vault {
						matches[[2]solana.PublicKey{account.PublicKey, vault}] = struct{}{}
					}
				}
			}
		}
		if len(matches) != 1 {
			continue
		}
		for match := range matches {
			if _, err := store.ProjectIntent(ctx, intentInput(match[0].String(), 0, transaction, memo, intent)); err != nil {
				return applied, err
			}
		}
		applied++
	}
	return applied, nil
}
