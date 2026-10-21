package earn

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"unicode/utf8"

	pb "github.com/helius-labs/laserstream-sdk/go/proto"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
	"github.com/solana-foundation/solana-go/v2"
)

var memoProgram = solana.MustPublicKeyFromBase58("MemoSq4gqABAXKb96qnH8TysNcWxMyWCqXgDLGmfcHr")

type ChainLocation uint16

func OuterLocation(outer int) (ChainLocation, error) {
	if outer > 0xff {
		return 0, errors.New("Earn MAX outer memo instruction index overflow")
	}
	return ChainLocation(outer * 256), nil
}

func InnerLocation(group uint32, inner int) (ChainLocation, error) {
	value := uint64(group)*256 + uint64(inner) + 1
	if group > 0xffff || value > 0xffff {
		return 0, errors.New("Earn MAX memo instruction index overflow")
	}
	return ChainLocation(value), nil
}

type Memo struct {
	SourceIndex ChainLocation
	Accounts    []solana.PublicKey
	Data        []byte
}

type PolicyTransaction struct {
	Signature    string
	Slot         uint64
	Index        uint64
	Signers      []solana.PublicKey
	Instructions []squads.Instruction
	Memos        []Memo
}

type accountTable struct {
	keys                                               []solana.PublicKey
	staticLen, loadedWritable                          int
	requiredSigners, readonlySigners, readonlyUnsigned int
}

func (t accountTable) meta(index int) (solana.AccountMeta, bool) {
	if index < 0 || index >= len(t.keys) {
		return solana.AccountMeta{}, false
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
	return solana.AccountMeta{PublicKey: t.keys[index], IsSigner: signer, IsWritable: writable}, true
}

func (t accountTable) metas(indexes []byte) []solana.AccountMeta {
	out := make([]solana.AccountMeta, 0, len(indexes))
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
	return program == squads.ProgramID || program == subscriptionsProgram
}

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
	out := &PolicyTransaction{Signature: signature, Slot: slot, Index: info.GetIndex(), Signers: table.signers()}
	for outer, compiled := range message.GetInstructions() {
		index := int(compiled.GetProgramIdIndex())
		if index >= len(table.keys) {
			continue
		}
		program := table.keys[index]
		if program == memoProgram {
			location, err := OuterLocation(outer)
			if err != nil {
				return nil, err
			}
			var accounts []solana.PublicKey
			for _, account := range compiled.GetAccounts() {
				if int(account) < len(table.keys) {
					accounts = append(accounts, table.keys[account])
				}
			}
			out.Memos = append(out.Memos, Memo{SourceIndex: location, Accounts: accounts, Data: compiled.GetData()})
			continue
		}
		if isPolicyProgram(program) {
			out.Instructions = append(out.Instructions, squads.Instruction{ProgramID: program, Accounts: table.metas(compiled.GetAccounts()), Data: compiled.GetData()})
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
				out.Instructions = append(out.Instructions, squads.Instruction{ProgramID: program, Accounts: accounts, Data: instruction.GetData()})
			}
			if program != memoProgram {
				continue
			}
			source, err := InnerLocation(group.GetIndex(), inner)
			if err != nil {
				return nil, err
			}
			out.Memos = append(out.Memos, memoFrom(source, accounts, instruction.GetData()))
		}
	}
	return out, nil
}

func memoFrom(source ChainLocation, accounts []solana.AccountMeta, data []byte) Memo {
	keys := make([]solana.PublicKey, 0, len(accounts))
	for _, account := range accounts {
		keys = append(keys, account.PublicKey)
	}
	return Memo{SourceIndex: source, Accounts: keys, Data: data}
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

func projectEarnMaxMemos(ctx context.Context, store *Store, logger *slog.Logger, transaction *PolicyTransaction) error {
	for _, memo := range transaction.Memos {
		// The SPL Memo program fails unless every account it lists signed, and
		// the vault PDA signs only through its Squads settings (the wallet or a
		// policy), so the settings of the listed vault authorised this intent.
		intent, err := parseEarnMaxIntent(memo.Data)
		if err != nil {
			logger.Info("skipped an Earn MAX memo", "event", "earn_max_memo_skipped", "reason", err.Error(), "signature", transaction.Signature)
			continue
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
			logger.Info("skipped an Earn MAX memo", "event", "earn_max_memo_skipped", "reason", "the memo names no single vault", "signature", transaction.Signature)
			continue
		}
		for match := range matches {
			ours, err := store.RecordEarnMaxIntent(ctx, match[0].String(), match[1].String(), transaction, memo.SourceIndex, intent)
			if err != nil {
				return err
			}
			if !ours {
				logger.Info("skipped an Earn MAX memo", "event", "earn_max_memo_skipped", "reason", "the settings have no Earn MAX route", "signature", transaction.Signature)
			}
		}
	}
	return nil
}

func (s *Store) RecordEarnMaxIntent(ctx context.Context, settings, vault string, transaction *PolicyTransaction, index ChainLocation, intent *EarnMaxIntent) (bool, error) {
	var ours bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM loyal_yield.multiply_route_states WHERE settings = $1 AND vault = $2)`, settings, vault).Scan(&ours); err != nil || !ours {
		return false, err
	}
	kind, requestID, amount, destination := "cancel", "", (*string)(nil), (*string)(nil)
	if withdraw := intent.Withdraw; withdraw != nil {
		kind, requestID, destination = "withdraw", withdraw.RequestID, &withdraw.DestinationAccount
		if withdraw.AmountRaw != nil {
			value := strconv.FormatUint(*withdraw.AmountRaw, 10)
			amount = &value
		}
	} else {
		requestID = intent.Cancel.RequestID
	}
	_, err := s.pool.Exec(ctx, `
        INSERT INTO loyal_yield.earn_max_intents
            (signature, instruction_index, settings, vault, kind, request_id, amount_raw, destination, slot, transaction_index)
        VALUES ($1, $2, $3, $4, $5, $6, $7::numeric, $8, $9, $10)
        ON CONFLICT DO NOTHING`,
		transaction.Signature, int32(index), settings, vault, kind, requestID, amount, destination, int64(transaction.Slot), int64(transaction.Index))
	return true, err
}
