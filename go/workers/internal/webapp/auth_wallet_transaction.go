package webapp

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"net/http"
	"regexp"
	"strings"

	"github.com/solana-foundation/solana-go/v2"
)

var walletBase64Pattern = regexp.MustCompile(`^[A-Za-z0-9+/]+={0,2}$`)

var errWalletTransactionShape = errors.New("wallet proof transaction is malformed")

type walletProofInstruction struct {
	programID solana.PublicKey
	keys      []solana.PublicKey
	signers   []bool
	data      []byte
}

type walletProofMessage struct {
	header       [3]byte
	accountKeys  []solana.PublicKey
	blockhash    solana.PublicKey
	programIdx   []byte
	accountIdx   [][]byte
	instructions []walletProofInstruction
	signatures   [][]byte
}

func appendShortvec(out []byte, n int) []byte {
	for {
		elem := byte(n & 0x7f)
		n >>= 7
		if n == 0 {
			return append(out, elem)
		}
		out = append(out, elem|0x80)
	}
}

func walletProofTransaction(wallet solana.PublicKey, memo string) string {
	header, keys, program := []byte{1, 0, 1}, []solana.PublicKey{wallet, walletMemoProgram}, byte(1)
	if wallet == walletMemoProgram {
		header, keys, program = []byte{1, 0, 0}, []solana.PublicKey{wallet}, 0
	}
	wire := appendShortvec(nil, 1)
	wire = append(wire, make([]byte, 64)...)
	wire = append(wire, header...)
	wire = appendShortvec(wire, len(keys))
	for _, key := range keys {
		wire = append(wire, key[:]...)
	}
	wire = append(wire, make([]byte, 32)...)
	wire = appendShortvec(wire, 1)
	wire = append(wire, program)
	wire = appendShortvec(wire, 1)
	wire = append(wire, 0)
	wire = appendShortvec(wire, len(memo))
	wire = append(wire, memo...)
	return base64.StdEncoding.EncodeToString(wire)
}

type walletWireReader struct{ rest []byte }

func (r *walletWireReader) shortvec() int {
	var n int32
	for size := 0; ; size++ {
		var elem byte
		if len(r.rest) > 0 {
			elem, r.rest = r.rest[0], r.rest[1:]
		}
		n |= int32(elem&0x7f) << ((size * 7) & 31)
		if elem&0x80 == 0 {
			return max(0, int(n))
		}
	}
}

func (r *walletWireReader) take(n int) ([]byte, error) {
	if n > len(r.rest) {
		return nil, errWalletTransactionShape
	}
	out := r.rest[:n:n]
	r.rest = r.rest[n:]
	return out, nil
}

func parseWalletProofTransaction(wire []byte) (walletProofMessage, error) {
	r := &walletWireReader{rest: wire}
	var m walletProofMessage
	for count := r.shortvec(); len(m.signatures) < count; {
		signature, err := r.take(64)
		if err != nil {
			return m, err
		}
		m.signatures = append(m.signatures, signature)
	}
	header, err := r.take(3)
	if err != nil {
		return m, err
	}
	if header[0]&0x80 != 0 {
		return m, errWalletTransactionShape
	}
	copy(m.header[:], header)
	for count := r.shortvec(); len(m.accountKeys) < count; {
		key, err := r.take(32)
		if err != nil {
			return m, err
		}
		m.accountKeys = append(m.accountKeys, solana.PublicKeyFromBytes(key))
	}
	blockhash, err := r.take(32)
	if err != nil {
		return m, err
	}
	m.blockhash = solana.PublicKeyFromBytes(blockhash)
	for count, i := r.shortvec(), 0; i < count; i++ {
		program, err := r.take(1)
		if err != nil {
			return m, err
		}
		accounts, err := r.take(r.shortvec())
		if err != nil {
			return m, err
		}
		data, err := r.take(r.shortvec())
		if err != nil {
			return m, err
		}
		m.programIdx = append(m.programIdx, program[0])
		m.accountIdx = append(m.accountIdx, accounts)
		m.instructions = append(m.instructions, walletProofInstruction{data: data})
	}
	if len(m.signatures) > len(m.accountKeys) {
		return m, errWalletTransactionShape
	}
	for i := range m.instructions {
		if int(m.programIdx[i]) >= len(m.accountKeys) {
			return m, errWalletTransactionShape
		}
		m.instructions[i].programID = m.accountKeys[m.programIdx[i]]
		for _, account := range m.accountIdx[i] {
			if int(account) >= len(m.accountKeys) {
				return m, errWalletTransactionShape
			}
			key := m.accountKeys[account]
			signer := int(account) < int(m.header[0])
			for _, signed := range m.accountKeys[:len(m.signatures)] {
				signer = signer || signed == key
			}
			m.instructions[i].keys = append(m.instructions[i].keys, key)
			m.instructions[i].signers = append(m.instructions[i].signers, signer)
		}
	}
	return m, nil
}

func (m walletProofMessage) serialize() []byte {
	out := append([]byte(nil), m.header[:]...)
	out = appendShortvec(out, len(m.accountKeys))
	for _, key := range m.accountKeys {
		out = append(out, key[:]...)
	}
	out = append(out, m.blockhash[:]...)
	out = appendShortvec(out, len(m.instructions))
	for i, instruction := range m.instructions {
		out = append(out, m.programIdx[i])
		out = appendShortvec(out, len(m.accountIdx[i]))
		out = append(out, m.accountIdx[i]...)
		out = appendShortvec(out, len(instruction.data))
		out = append(out, instruction.data...)
	}
	return out
}

func decodeWalletTransaction(value string) ([]byte, error) {
	normalized := strings.TrimSpace(value)
	if normalized == "" || len(normalized)%4 == 1 || !walletBase64Pattern.MatchString(normalized) {
		return nil, walletRefusal(http.StatusBadRequest, "invalid_wallet_transaction_encoding", "Wallet transaction is invalid.")
	}
	unpadded := strings.TrimRight(normalized, "=")
	if len(unpadded)%4 == 1 {
		unpadded = unpadded[:len(unpadded)-1]
	}
	wire, err := base64.RawStdEncoding.DecodeString(unpadded)
	if err != nil {
		return nil, walletRefusal(http.StatusBadRequest, "invalid_wallet_transaction_encoding", "Wallet transaction is invalid.")
	}
	return wire, nil
}

func verifyWalletProofTransaction(memo, signedTransaction, walletAddress string) (string, error) {
	wire, err := decodeWalletTransaction(signedTransaction)
	if err != nil {
		return "", err
	}
	m, err := parseWalletProofTransaction(wire)
	if err != nil {
		return "", walletRefusal(http.StatusBadRequest, "invalid_wallet_transaction", "Wallet transaction is invalid.")
	}
	refuse := func(code, message string) (string, error) {
		return "", walletRefusal(http.StatusUnauthorized, code, message)
	}
	if m.header[0] == 0 || len(m.accountKeys) == 0 || m.accountKeys[0].String() != walletAddress {
		return refuse("wallet_transaction_fee_payer_mismatch", "Wallet transaction fee payer is invalid.")
	}
	feePayer := m.accountKeys[0]
	if m.blockhash.String() != walletNonBroadcastBlockhash {
		return refuse("wallet_transaction_blockhash_mismatch", "Wallet transaction blockhash is invalid.")
	}
	var memoInstructions []walletProofInstruction
	for _, instruction := range m.instructions {
		if instruction.programID == walletMemoProgram {
			memoInstructions = append(memoInstructions, instruction)
		}
	}
	if len(memoInstructions) != 1 {
		return refuse("wallet_transaction_instruction_mismatch", "Wallet transaction instructions are invalid.")
	}
	instruction := memoInstructions[0]
	validKeys := len(instruction.keys) == 0 ||
		len(instruction.keys) == 1 && instruction.keys[0].String() == walletAddress && instruction.signers[0]
	if !validKeys {
		return refuse("wallet_transaction_accounts_not_allowed", "Wallet transaction accounts are invalid.")
	}
	if string(instruction.data) != memo {
		return refuse("wallet_transaction_memo_mismatch", "Wallet transaction memo is invalid.")
	}
	zero := make([]byte, 64)
	if len(m.signatures) == 0 || bytes.Equal(m.signatures[0], zero) {
		return refuse("wallet_transaction_signature_missing", "Wallet transaction signature is missing.")
	}
	required := min(int(m.header[0]), len(m.accountKeys))
	valid := len(m.signatures) == required
	message := m.serialize()
	for i := 0; valid && i < len(m.signatures); i++ {
		valid = !bytes.Equal(m.signatures[i], zero) && ed25519.Verify(m.accountKeys[i][:], message, m.signatures[i])
	}
	if !valid {
		return refuse("invalid_wallet_transaction_signature", "Wallet transaction signature is invalid.")
	}
	return feePayer.String(), nil
}
