package autodeposit

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"

	"github.com/gagliardetto/solana-go"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/backyard"
	solwire "github.com/loyal-labs/loyal-yield-routing/go/workers/internal/solana"
)

var base64Std = base64.StdEncoding

// Chain is the chain-effect capability an execution needs. It is deliberately
// narrower than an RPC client: no account writes, no policy decisions. The
// controller owns the durable order; the adapter owns transport.
type Chain interface {
	// ConfirmedTokenBalanceRaw reads one token account's current raw balance. A
	// missing or malformed account is an error; zero requires an observed
	// initialized token account with the expected wallet or vault authority.
	ConfirmedTokenBalanceRaw(ctx context.Context, tokenAccount, authority string) (int64, error)
	// RemainingDelegationAllowanceRaw reads the recurring delegation's unused
	// authorization against the frozen identity. ErrAllowanceUnknown
	// distinguishes "cannot read it" from "it is exhausted"; unknown never
	// becomes a zero cap.
	RemainingDelegationAllowanceRaw(ctx context.Context, delegation string, identity DelegationIdentity) (int64, error)
	// LatestBlockhash returns a fresh confirmed blockhash and its validity
	// window, required before any wire is built.
	LatestBlockhash(ctx context.Context) (string, int64, error)
	// Observe reads the chain state of one persisted signature.
	Observe(ctx context.Context, attempt DurableAttempt) (AttemptObservation, error)
	// BroadcastExact submits the persisted bytes once, without RPC-side retry,
	// and returns the acknowledged signature.
	BroadcastExact(ctx context.Context, attempt DurableAttempt) (string, error)
	// ConfirmedReceipt reads the immutable transaction receipt for the exact
	// signature, for integer effect verification.
	ConfirmedReceipt(ctx context.Context, signature string) (ReceiptEvidence, error)
	// ReadAccounts reads one coherent confirmed account set. Every requested
	// address must exist with a program owner, or the read fails.
	ReadAccounts(ctx context.Context, addresses []string) (int64, []backyard.ConfirmedAccount, error)
	// SimulateExact simulates the persisted wire's exact bytes with signature
	// verification, before the family records anything as settled. It never
	// replaces the blockhash.
	SimulateExact(ctx context.Context, attempt DurableAttempt) error
	// ConfirmedVaultPositionRaw reads the vault's deposited liquidity in the
	// frozen reserve from confirmed chain state, with the slot it was read at.
	// It is the post-confirm position amount finalization publishes.
	ConfirmedVaultPositionRaw(ctx context.Context, plan DepositPlan, route TopUpRoute) (int64, int64, error)
}

// ErrAllowanceUnknown reports that the delegated allowance could not be read.
// The controller defers instead of treating the unknown as exhaustion.
var ErrAllowanceUnknown = errors.New("autodeposit recurring delegation allowance is unreadable")

// ReceiptEffect is one token account's movement inside a confirmed transaction.
type ReceiptEffect struct {
	TokenAccount string
	Mint         string
	PreRaw       int64
	PostRaw      int64
}

// ReceiptEvidence is the confirmed receipt of one signature with the token
// movements this family verifies its effects against.
type ReceiptEvidence struct {
	Signature string
	Slot      int64
	Effects   []ReceiptEffect
}

// EffectFor returns the receipt's movement for one token account.
func (e ReceiptEvidence) EffectFor(tokenAccount string) (ReceiptEffect, bool) {
	for _, effect := range e.Effects {
		if effect.TokenAccount == tokenAccount {
			return effect, true
		}
	}
	return ReceiptEffect{}, false
}

// RPCChain is the production Chain over the real HTTP JSON-RPC adapter the
// Backyard family already runs. It adds no family behavior: reads, broadcast,
// receipts, simulation.
type RPCChain struct {
	rpc *backyard.RPCClient
}

// ConfirmedSlot probes the chain independently of business dispatch. An empty
// queue cannot manufacture chain progress for runtime readiness.
func (c *RPCChain) ConfirmedSlot(ctx context.Context) (int64, error) {
	if c == nil || c.rpc == nil {
		return 0, errors.New("autodeposit confirmed RPC frontier unavailable")
	}
	return c.rpc.ConfirmedSlot(ctx)
}

// NewRPCChain builds the production chain adapter over an RPC endpoint URL.
// The recurring-delegation allowance is decoded with the official
// loyal-actions byte layout (delegation.go); no decoder is injected.
func NewRPCChain(rpcURL string) (*RPCChain, error) {
	if strings.TrimSpace(rpcURL) == "" {
		return nil, errors.New("autodeposit chain requires an RPC endpoint URL")
	}
	client, err := backyard.NewRPCClient(rpcURL)
	if err != nil {
		return nil, fmt.Errorf("build autodeposit chain RPC: %w", err)
	}
	return &RPCChain{rpc: client}, nil
}

func (c *RPCChain) ConfirmedTokenBalanceRaw(ctx context.Context, tokenAccount, authority string) (int64, error) {
	if tokenAccount == "" {
		return 0, errors.New("token account address is required")
	}
	_, accounts, err := c.rpc.GetMultipleAccounts(ctx, []string{tokenAccount}, 1)
	if err != nil {
		return 0, fmt.Errorf("read autodeposit token account %s: %w", tokenAccount, err)
	}
	account := accounts[0]
	if account.Owner == "" {
		return 0, errors.New("required autodeposit token account is absent")
	}

	if account.Owner != splTokenID || account.Executable || len(account.Data) != splTokenAccountLength || base58Key(account.Data[:32]) != USDCMint || base58Key(account.Data[32:64]) != authority || authority == "" || account.Data[108] != 1 {
		return 0, errors.New("autodeposit balance requires an initialized USDC account owned by SPL Token")
	}
	if len(account.Data) < 72 {
		return 0, fmt.Errorf("token account %s data is too short for a spl-token v2 account", tokenAccount)
	}
	// SPL token account layout: mint(32) owner(32) amount(u64 LE) at offset 64.
	var amount uint64
	for i := 0; i < 8; i++ {
		amount |= uint64(account.Data[64+i]) << (8 * i)
	}
	if amount > 1<<63-1 {
		return 0, fmt.Errorf("token account %s balance exceeds the family's int64 range", tokenAccount)
	}
	return int64(amount), nil
}

// RemainingDelegationAllowanceRaw reads the delegation account and decodes its
// unused authorization with the official loyal-actions layout. The account
// owner is checked before any byte is read, and the embedded delegator,
// delegatee and mint must equal the frozen identity: an unrelated account can
// never authorize this family's pull. A mismatching or corrupt account is
// unknown, never a zero cap.
func (c *RPCChain) RemainingDelegationAllowanceRaw(ctx context.Context, delegation string, identity DelegationIdentity) (int64, error) {
	if _, err := solana.PublicKeyFromBase58(delegation); err != nil {
		return 0, fmt.Errorf("%w: delegation address %q is not a public key", ErrAllowanceUnknown, delegation)
	}
	identity.Account = delegation
	if identity.Nonce == nil || identity.WalletTokenAccount == "" {
		return 0, fmt.Errorf("%w: confirmed delegation nonce and wallet token account required", ErrAllowanceUnknown)
	}
	_, accounts, err := c.ReadAccounts(ctx, []string{delegation, identity.WalletTokenAccount})
	if err != nil {
		return 0, fmt.Errorf("%w: read delegation %s: %v", ErrAllowanceUnknown, delegation, err)
	}
	if len(accounts) != 2 {
		return 0, ErrAllowanceUnknown
	}
	account := accounts[0]
	allowance, decodeErr := RemainingDelegationAllowance(account.Owner, account.Data, identity)
	if decodeErr != nil {
		return 0, fmt.Errorf("%w: %v", ErrAllowanceUnknown, decodeErr)
	}
	wallet := accounts[1]
	walletKey, keyErr := solana.PublicKeyFromBase58(identity.Delegator)
	if keyErr != nil {
		return 0, ErrAllowanceUnknown
	}
	mintKey, keyErr := solana.PublicKeyFromBase58(identity.Mint)
	if keyErr != nil {
		return 0, ErrAllowanceUnknown
	}
	authority, err := subscriptionAuthorityKey(walletKey[:], mintKey[:])
	if err != nil {
		return 0, err
	}
	if wallet.Address != identity.WalletTokenAccount || wallet.Owner != splTokenID || wallet.Executable || len(wallet.Data) != splTokenAccountLength || base58Key(wallet.Data[:32]) != identity.Mint || base58Key(wallet.Data[32:64]) != identity.Delegator || wallet.Data[108] != 1 || binary.LittleEndian.Uint32(wallet.Data[72:76]) != 1 || base58Key(wallet.Data[76:108]) != base58Key(authority[:]) {
		return 0, fmt.Errorf("%w: wallet token approval does not authorize the subscription authority", ErrAllowanceUnknown)
	}
	tokenAllowance := binary.LittleEndian.Uint64(wallet.Data[121:129])
	if tokenAllowance < uint64(allowance) {
		allowance = int64(tokenAllowance)
	}
	return allowance, nil
}

func (c *RPCChain) LatestBlockhash(ctx context.Context) (string, int64, error) {
	blockhash, err := c.rpc.LatestBlockhash(ctx)
	if err != nil {
		return "", 0, err
	}
	return blockhash.Blockhash, blockhash.LastValidBlockHeight, nil
}

func (c *RPCChain) MinimumBalanceForRentExemption(ctx context.Context, size int) (uint64, error) {
	return c.rpc.MinimumBalanceForRentExemption(ctx, size)
}

func (c *RPCChain) Observe(ctx context.Context, attempt DurableAttempt) (AttemptObservation, error) {
	if _, err := persistedWireTransaction(attempt); err != nil {
		return AttemptObservation{}, err
	}
	status, err := c.rpc.SignatureStatus(ctx, attempt.Signature)
	if err != nil {
		return AttemptObservation{}, fmt.Errorf("observe autodeposit %s signature: %w", attempt.OperationKind, err)
	}
	switch {
	case !status.Found:
		if attempt.LastValidBlockHeight <= 0 {
			return AttemptObservation{State: AttemptUnknown}, nil
		}
		height, err := c.rpc.FinalizedBlockHeight(ctx)
		if err != nil {
			return AttemptObservation{}, err
		}
		if height > attempt.LastValidBlockHeight {
			return AttemptObservation{State: AttemptExpired}, nil
		}
		return AttemptObservation{State: AttemptUnknown}, nil
	case status.Failed:
		return AttemptObservation{State: AttemptFailed, Err: errors.New("transaction failed on chain")}, nil
	case status.Confirmed:
		slot := status.ConfirmationSlot
		return AttemptObservation{State: AttemptConfirmed, ConfirmedSlot: &slot}, nil
	default:
		return AttemptObservation{State: AttemptUnknown}, nil
	}
}

func (c *RPCChain) BroadcastExact(ctx context.Context, attempt DurableAttempt) (string, error) {
	if _, err := persistedWireTransaction(attempt); err != nil {
		return "", err
	}
	wire, err := base64StdDecode(attempt.SignedTransactionBase64)
	if err != nil {
		return "", fmt.Errorf("decode persisted %s wire: %w", attempt.OperationKind, err)
	}
	return c.rpc.SendSignedTransactionOnce(ctx, wire, attempt.Signature)
}

func (c *RPCChain) ConfirmedReceipt(ctx context.Context, signature string) (ReceiptEvidence, error) {
	evidence, err := c.rpc.ConfirmedTransaction(ctx, signature)
	if err != nil {
		return ReceiptEvidence{}, fmt.Errorf("read autodeposit receipt %s: %w", signature, err)
	}
	return receiptFromEvidence(evidence)
}

// ReadAccounts reads one coherent confirmed account set: every address must
// exist with a program owner, and the response must be coherent at one
// confirmed slot.
func (c *RPCChain) ReadAccounts(ctx context.Context, addresses []string) (int64, []backyard.ConfirmedAccount, error) {
	if len(addresses) == 0 {
		return 0, nil, errors.New("autodeposit account read needs at least one address")
	}
	slot, err := c.rpc.ConfirmedSlot(ctx)
	if err != nil {
		return 0, nil, err
	}
	return c.rpc.GetMultipleAccounts(ctx, addresses, slot)
}

func (c *RPCChain) ReadAccountsWithOptional(ctx context.Context, addresses []string, optional ...string) (int64, []backyard.ConfirmedAccount, error) {
	if len(optional) == 0 {
		return c.ReadAccounts(ctx, addresses)
	}
	slot, err := c.rpc.ConfirmedSlot(ctx)
	if err != nil {
		return 0, nil, err
	}
	return c.rpc.GetMultipleAccountsWithOptional(ctx, addresses, slot, optional...)
}

// SimulateExact simulates the persisted wire's exact bytes with signature
// verification before the family records anything as settled.
func (c *RPCChain) SimulateExact(ctx context.Context, attempt DurableAttempt) error {
	if _, err := persistedWireTransaction(attempt); err != nil {
		return err
	}
	wire, err := base64StdDecode(attempt.SignedTransactionBase64)
	if err != nil {
		return fmt.Errorf("decode persisted %s wire: %w", attempt.OperationKind, err)
	}
	if _, err := solwire.OwnSignedWire(wire, attempt.SignedTransactionSHA256); err != nil {
		return fmt.Errorf("persisted %s wire failed the shared packet contract: %w", attempt.OperationKind, err)
	}
	if _, err := c.rpc.SimulateSignedTransaction(ctx, wire); err != nil {
		return fmt.Errorf("simulate persisted %s wire %s: %w", attempt.OperationKind, attempt.Signature, err)
	}
	return nil
}

// ConfirmedVaultPositionRaw reads the obligation's deposited collateral for
// the frozen reserve and converts it to liquidity through the reserve's
// collateral exchange value, at one coherent confirmed slot. Borrowed liquidity
// and fee liabilities remain in the 60-bit scaled-fraction domain until the
// final flooring through the shared KLend decoder.
func (c *RPCChain) ConfirmedVaultPositionRaw(ctx context.Context, plan DepositPlan, route TopUpRoute) (int64, int64, error) {
	observedSlot, accounts, err := c.ReadAccounts(ctx, []string{route.Obligation, plan.Reserve})
	if err != nil {
		return 0, 0, err
	}
	var obligationAccount, reserveAccount backyard.ConfirmedAccount
	for _, account := range accounts {
		switch account.Address {
		case route.Obligation:
			obligationAccount = account
		case plan.Reserve:
			reserveAccount = account
		}
	}
	if obligationAccount.Owner != KLendProgramID || len(obligationAccount.Data) != obligationDataLength ||
		hexPrefix(obligationAccount.Data[:8]) != hexPrefix(obligationDiscriminator[:]) {
		return 0, 0, fmt.Errorf("obligation %s is not a confirmed KLend obligation", route.Obligation)
	}
	if reserveAccount.Owner != KLendProgramID || len(reserveAccount.Data) != reserveDataLength ||
		reserveDiscriminator != hexPrefix(reserveAccount.Data[:8]) {
		return 0, 0, fmt.Errorf("reserve %s is not a confirmed KLend reserve", plan.Reserve)
	}
	var collateralRaw uint64
	for i := 0; i < obligationDepositCount; i++ {
		offset := obligationDepositsOffset + i*obligationDepositStride
		if base58Key(obligationAccount.Data[offset:offset+32]) != plan.Reserve {
			continue
		}
		amount := binary.LittleEndian.Uint64(obligationAccount.Data[offset+32 : offset+40])
		if amount > 1<<63-1 {
			return 0, 0, fmt.Errorf("obligation %s collateral amount exceeds the family's int64 range", route.Obligation)
		}
		collateralRaw = amount
		break
	}
	if collateralRaw == 0 {
		return 0, 0, fmt.Errorf("obligation %s carries no confirmed deposit in the frozen reserve %s", route.Obligation, plan.Reserve)
	}
	if base58Key(obligationAccount.Data[32:64]) != plan.Market || base58Key(obligationAccount.Data[64:96]) != plan.Target.VaultPubkey {
		return 0, 0, errors.New("confirmed obligation identity differs from frozen plan")
	}
	obligationSlot := binary.LittleEndian.Uint64(obligationAccount.Data[16:24])
	reserveSlot := binary.LittleEndian.Uint64(reserveAccount.Data[16:24])
	if observedSlot <= 0 || obligationSlot == 0 || reserveSlot < obligationSlot || reserveSlot > uint64(observedSlot) || obligationSlot > uint64(observedSlot) {
		return 0, 0, errors.New("confirmed collateral conversion snapshot is stale or incoherent")
	}
	liquidity, err := backyard.KaminoRedeemableLiquidity(reserveAccount, plan.Market, plan.LiquidityMint, collateralRaw)
	if err != nil {
		return 0, 0, err
	}
	if liquidity > 1<<63-1 {
		return 0, 0, errors.New("redeemable liquidity exceeds SQL BIGINT")
	}
	return int64(liquidity), observedSlot, nil
}

// base64StdDecode decodes the persisted wire bytes.
func base64StdDecode(encoded string) ([]byte, error) {
	return base64Std.DecodeString(encoded)
}

func receiptFromEvidence(evidence backyard.ConfirmedTransactionEvidence) (ReceiptEvidence, error) {
	receipt := ReceiptEvidence{Signature: evidence.Signature, Slot: evidence.Slot}
	if evidence.Slot <= 0 {
		return receipt, errors.New("receipt slot unavailable")
	}
	pre := map[string]backyard.TransactionTokenBalance{}
	post := map[string]backyard.TransactionTokenBalance{}
	for _, set := range []struct {
		balances []backyard.TransactionTokenBalance
		into     map[string]backyard.TransactionTokenBalance
	}{{evidence.PreTokenBalances, pre}, {evidence.PostTokenBalances, post}} {
		for _, balance := range set.balances {
			if balance.Address == "" || balance.Mint == "" || balance.Raw > 1<<63-1 {
				return receipt, errors.New("receipt token identity or BIGINT amount invalid")
			}
			if _, exists := set.into[balance.Address]; exists {
				return receipt, errors.New("duplicate token account receipt evidence")
			}
			set.into[balance.Address] = balance
		}
	}
	for address, before := range pre {
		after, known := post[address]
		if !known {
			continue
		} // An absent side is unknown; it cannot prove a delta.
		if before.Mint != after.Mint {
			return receipt, errors.New("receipt changed token-account mint")
		}
		receipt.Effects = append(receipt.Effects, ReceiptEffect{TokenAccount: address, Mint: before.Mint, PreRaw: int64(before.Raw), PostRaw: int64(after.Raw)})
	}
	return receipt, nil
}
