package autodeposit

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"

	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/backyard"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
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
	// ConfirmedLamports reads one account's lamports; an absent account has 0.
	ConfirmedLamports(ctx context.Context, address string) (uint64, error)
	// RemainingDelegationAllowanceRaw reads the recurring delegation's unused
	// authorization against the frozen identity. ErrAllowanceUnknown
	// distinguishes "cannot read it" from "it is exhausted"; unknown never
	// becomes a zero cap.
	RemainingDelegationAllowanceRaw(ctx context.Context, delegation string, identity DelegationIdentity) (int64, error)
	// LatestBlockhash returns a fresh confirmed blockhash and its validity
	// window, required before any wire is built.
	LatestBlockhash(ctx context.Context) (string, int64, error)
	// LandChain is the shared send path persisted wires land through.
	chain.LandChain
	// ConfirmedReceipt reads the immutable transaction receipt for the exact
	// signature, for integer effect verification.
	ConfirmedReceipt(ctx context.Context, signature string) (ReceiptEvidence, error)
	// SimulateExact simulates the persisted wire's exact bytes with signature
	// verification, before the family records anything as settled. It never
	// replaces the blockhash.
	SimulateExact(ctx context.Context, attempt DurableAttempt) error
	// ConfirmedVaultPositionRaw reads the vault's deposited liquidity in the
	// frozen reserve from confirmed chain state, with the slot it was read at.
	// It is the post-confirm position amount finalization publishes.
	// The read is no older than minSlot, the top-up's confirmed slot.
	ConfirmedVaultPositionRaw(ctx context.Context, plan DepositPlan, route TopUpRoute, minSlot int64) (int64, int64, error)
}

// ErrTokenAccountAbsent reports a token account that does not exist (yet).
var ErrTokenAccountAbsent = errors.New("required autodeposit token account is absent")

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

// RPCChain is the production Chain: the shared chain client plus the
// family's decoders. It adds no family behavior: reads, broadcast, receipts,
// simulation.
type RPCChain struct {
	*chain.Client
}

// NewRPCChain builds the production chain adapter over the shared client.
// The recurring-delegation allowance is decoded with the official
// loyal-actions byte layout (delegation.go); no decoder is injected.
func NewRPCChain(client *chain.Client) *RPCChain {
	return &RPCChain{Client: client}
}

func (c *RPCChain) ConfirmedTokenBalanceRaw(ctx context.Context, tokenAccount, authority string) (int64, error) {
	if tokenAccount == "" {
		return 0, errors.New("token account address is required")
	}
	_, accounts, err := c.ReadAccounts(ctx, 0, []string{tokenAccount}, tokenAccount)
	if err != nil {
		return 0, fmt.Errorf("read autodeposit token account %s: %w", tokenAccount, err)
	}
	account := accounts[0]
	if account == nil {
		return 0, ErrTokenAccountAbsent
	}
	return usdcTokenAccount(account, authority)
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
	_, accounts, err := c.ReadAccounts(ctx, 0, []string{delegation, identity.WalletTokenAccount})
	if err != nil {
		return 0, fmt.Errorf("%w: read delegation %s: %v", ErrAllowanceUnknown, delegation, err)
	}
	if len(accounts) != 2 {
		return 0, ErrAllowanceUnknown
	}
	account := accounts[0]
	allowance, decodeErr := RemainingDelegationAllowance(account.Owner.String(), account.Data, identity)
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
	if _, err := usdcTokenAccount(wallet, identity.Delegator); err != nil || binary.LittleEndian.Uint32(wallet.Data[72:76]) != 1 || base58Key(wallet.Data[76:108]) != base58Key(authority[:]) {
		return 0, fmt.Errorf("%w: wallet token approval does not authorize the subscription authority", ErrAllowanceUnknown)
	}
	tokenAllowance := binary.LittleEndian.Uint64(wallet.Data[121:129])
	if tokenAllowance < uint64(allowance) {
		allowance = int64(tokenAllowance)
	}
	return allowance, nil
}

func (c *RPCChain) LatestBlockhash(ctx context.Context) (string, int64, error) {
	hash, lastValid, err := c.Blockhash(ctx)
	if err != nil {
		return "", 0, err
	}
	return hash.String(), int64(lastValid), nil
}

func (c *RPCChain) MinimumBalanceForRentExemption(ctx context.Context, size int) (uint64, error) {
	return c.RentExempt(ctx, uint64(size))
}

func (c *RPCChain) ConfirmedReceipt(ctx context.Context, signature string) (ReceiptEvidence, error) {
	sig, err := solana.SignatureFromBase58(signature)
	if err != nil {
		return ReceiptEvidence{}, fmt.Errorf("autodeposit receipt signature: %w", err)
	}
	receipt, err := c.Receipt(ctx, sig, rpc.CommitmentConfirmed)
	if err != nil {
		return ReceiptEvidence{}, fmt.Errorf("read autodeposit receipt %s: %w", signature, err)
	}
	return receiptEvidence(signature, receipt)
}

// ReadAccounts reads addresses at one confirmed slot no older than minSlot.
// Only the optional addresses may be absent; they come back nil. A node
// behind minSlot answers chain.ErrBehind.
func (c *RPCChain) ReadAccounts(ctx context.Context, minSlot int64, addresses []string, optional ...string) (int64, []*chain.Account, error) {
	keys := make([]solana.PublicKey, len(addresses))
	for i, address := range addresses {
		key, err := solana.PublicKeyFromBase58(address)
		if err != nil {
			return 0, nil, fmt.Errorf("account address %q: %w", address, err)
		}
		keys[i] = key
	}
	slot, accounts, err := c.Accounts(ctx, keys, rpc.CommitmentConfirmed, uint64(max(minSlot, 0)))
	if err != nil {
		return 0, nil, err
	}
	for i, account := range accounts {
		if account == nil && !slices.Contains(optional, addresses[i]) {
			return 0, nil, fmt.Errorf("required account %s is absent", addresses[i])
		}
	}
	return int64(slot), accounts, nil
}

func (c *RPCChain) ConfirmedLamports(ctx context.Context, address string) (uint64, error) {
	_, accounts, err := c.ReadAccounts(ctx, 0, []string{address}, address)
	if err != nil || accounts[0] == nil {
		return 0, err
	}
	return accounts[0].Lamports, nil
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
	if _, err := chain.OwnSignedWire(wire, attempt.SignedTransactionSHA256); err != nil {
		return fmt.Errorf("persisted %s wire failed the shared packet contract: %w", attempt.OperationKind, err)
	}
	if _, _, err := c.Simulate(ctx, wire); err != nil {
		return fmt.Errorf("simulate persisted %s wire %s: %w", attempt.OperationKind, attempt.Signature, err)
	}
	return nil
}

// ConfirmedVaultPositionRaw reads the obligation's deposited collateral for
// the frozen reserve and converts it to liquidity through the reserve's
// collateral exchange value, at one coherent confirmed slot. Borrowed liquidity
// and fee liabilities remain in the 60-bit scaled-fraction domain until the
// final flooring through the shared KLend decoder.
func (c *RPCChain) ConfirmedVaultPositionRaw(ctx context.Context, plan DepositPlan, route TopUpRoute, minSlot int64) (int64, int64, error) {
	observedSlot, accounts, err := c.ReadAccounts(ctx, minSlot, []string{route.Obligation, plan.Reserve})
	if err != nil {
		return 0, 0, err
	}
	obligationAccount, reserveAccount := accounts[0], accounts[1]
	if obligationAccount.Owner.String() != KLendProgramID || len(obligationAccount.Data) != obligationDataLength ||
		hex.EncodeToString(obligationAccount.Data[:8]) != hex.EncodeToString(obligationDiscriminator[:]) {
		return 0, 0, fmt.Errorf("obligation %s is not a confirmed KLend obligation", route.Obligation)
	}
	if reserveAccount.Owner.String() != KLendProgramID || len(reserveAccount.Data) != reserveDataLength ||
		reserveDiscriminator != hex.EncodeToString(reserveAccount.Data[:8]) {
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
	liquidity, err := backyard.KaminoRedeemableLiquidity(backyard.ConfirmedAccount{Address: plan.Reserve, Owner: reserveAccount.Owner.String(), Lamports: reserveAccount.Lamports, Executable: reserveAccount.Executable, Data: reserveAccount.Data}, plan.Market, plan.LiquidityMint, collateralRaw)
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

// receiptEvidence keeps the token accounts the receipt reports on both sides.
// An account absent from one side is unknown there; it cannot prove a delta.
func receiptEvidence(signature string, receipt chain.Receipt) (ReceiptEvidence, error) {
	evidence := ReceiptEvidence{Signature: signature, Slot: int64(receipt.Slot)}
	if receipt.Err != nil {
		return evidence, fmt.Errorf("transaction %s failed on chain: %v", signature, receipt.Err)
	}
	for account, before := range receipt.Pre {
		after, known := receipt.Post[account]
		if !known {
			continue
		}
		if before.Mint != after.Mint {
			return evidence, errors.New("receipt changed token-account mint")
		}
		if before.Amount > 1<<63-1 || after.Amount > 1<<63-1 {
			return evidence, errors.New("receipt token amount exceeds BIGINT")
		}
		evidence.Effects = append(evidence.Effects, ReceiptEffect{TokenAccount: account.String(), Mint: before.Mint.String(), PreRaw: int64(before.Amount), PostRaw: int64(after.Amount)})
	}
	return evidence, nil
}
