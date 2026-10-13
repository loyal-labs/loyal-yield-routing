package backyard

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
)

// The builders below sign with deterministic test keys, so codec tests can
// prove the exact wire the production build signs (compileRequest, then
// signMessage) without the pinned delegate's secret.

func buildAndSignBridgeTransactionForDelegate(request BridgeBuildRequest, executor ed25519.PrivateKey, expectedDelegate publicKey) (SignedTransaction, error) {
	message, err := compileBridgeMessageForDelegate(rwa, request, expectedDelegate)
	if err != nil {
		return SignedTransaction{}, err
	}
	return signMessage(executor, expectedDelegate, message, request.RecentBlockhash, request.LastValidBlockHeight)
}

func buildAndSignJupiterTransactionForDelegate(request JupiterSwapRequest, executor ed25519.PrivateKey, expectedDelegate publicKey) (SignedTransaction, error) {
	message, err := compileJupiterMessageForDelegate(rwa, request, expectedDelegate)
	if err != nil {
		return SignedTransaction{}, err
	}
	return signMessage(executor, expectedDelegate, message, request.RecentBlockhash, request.LastValidBlockHeight)
}

func buildAndSignKaminoPrimeUSDCTransactionForDelegate(request KaminoPrimeUSDCRequest, executor ed25519.PrivateKey, expectedDelegate publicKey) (SignedTransaction, error) {
	message, err := compileKaminoMessageForDelegate(rwa, request, expectedDelegate)
	if err != nil {
		return SignedTransaction{}, err
	}
	return signMessage(executor, expectedDelegate, message, request.RecentBlockhash, request.LastValidBlockHeight)
}

// BuildResult is the exact signed transaction that passed simulation. It must
// be persisted before broadcast intent is recorded or the wire is submitted.
type BuildResult struct {
	MessageSHA256        string
	SignedWire           []byte
	SignedWireSHA256     string
	TransactionSignature string
	RecentBlockhash      string
	LastValidBlockHeight int64
	SimulationSlot       int64
}

// BuildResult is the signed wire with the slot its simulation returned.
func (s SignedTransaction) BuildResult(simulationSlot int64) (BuildResult, error) {
	if simulationSlot <= 0 || len(s.signedWire) == 0 || s.transactionSignature == "" {
		return BuildResult{}, fmt.Errorf("exact signed transaction was not simulated")
	}
	result := BuildResult{MessageSHA256: s.messageSHA256, SignedWire: append([]byte(nil), s.signedWire...), SignedWireSHA256: s.signedWireSHA256,
		TransactionSignature: s.transactionSignature, RecentBlockhash: s.recentBlockhash, LastValidBlockHeight: s.lastValidBlockHeight, SimulationSlot: simulationSlot}
	return result, result.validateForDelegate(publicKey{})
}

// validateForDelegate decodes a signed wire as the cluster would: one v1
// message, its signature by the (pinned, when nonzero) delegate, and hashes
// that name these bytes.
func (b BuildResult) validateForDelegate(expectedDelegate publicKey) error {
	if len(b.SignedWire) == 0 || b.TransactionSignature == "" || b.RecentBlockhash == "" || b.LastValidBlockHeight <= 0 || b.SimulationSlot <= 0 {
		return fmt.Errorf("incomplete simulated signed transaction")
	}
	wireHash := sha256.Sum256(b.SignedWire)
	if b.SignedWireSHA256 != hex.EncodeToString(wireHash[:]) || len(b.MessageSHA256) != 64 {
		return fmt.Errorf("transaction hash mismatch")
	}
	signature, message, recentBlockhash, signer, err := decodeExactWire(b.SignedWire)
	if err != nil || !ed25519.Verify(signer[:], message, signature) ||
		b.TransactionSignature != encodeBase58(signature) || b.RecentBlockhash != encodeBase58(recentBlockhash[:]) {
		return fmt.Errorf("signed transaction wire does not match persisted evidence")
	}
	if expectedDelegate != (publicKey{}) && signer != expectedDelegate {
		return fmt.Errorf("signed transaction does not use the pinned delegated executor")
	}
	messageHash := sha256.Sum256(message)
	if b.MessageSHA256 != hex.EncodeToString(messageHash[:]) {
		return fmt.Errorf("message hash does not match signed wire")
	}
	return nil
}

// kaminoBorrowFee is KLend's origination fee for receive at the debt
// reserve's configured rate, for fixtures that build borrow effects.
func kaminoBorrowFee(accounts []ConfirmedAccount, route RuntimeRoute, receive uint64) (uint64, error) {
	reserve := accountAt(accounts, route.Kamino.DebtReserve)
	if _, err := decodeKaminoReserve(reserve, route.Kamino.DebtMint, route.Kamino); err != nil {
		return 0, err
	}
	rate := binary.LittleEndian.Uint64(reserve.Data[kaminoReserveConfigOffset+40:])
	return kaminoBorrowFeeAtRate(rate, receive)
}
