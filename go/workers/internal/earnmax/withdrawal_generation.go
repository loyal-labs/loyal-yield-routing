package earnmax

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"strconv"
	"time"

	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/spl"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/voltr"
)

type WithdrawalGenerationSource interface {
	History(context.Context, solana.PublicKey, int, solana.Signature, rpc.CommitmentType, uint64) ([]chain.Signed, error)
	Execution(context.Context, solana.Signature, rpc.CommitmentType) (chain.Execution, error)
}

type CurrentWithdrawalRequest struct {
	WithdrawalGeneration
	Slot      uint64
	Execution chain.Execution
}

// ReadWithdrawalRequest proves only the current receipt lifecycle. Historical
// PnL coverage and account birth are unrelated to permission to withdraw.
func ReadWithdrawalRequest(ctx context.Context, source WithdrawalGenerationSource, authority solana.PublicKey, atLeast uint64) (CurrentWithdrawalRequest, error) {
	user, err := voltr.UserAccounts(Vault, LPMint, authority)
	if err != nil {
		return CurrentWithdrawalRequest{}, err
	}
	const pageSize = 100
	const maxPages = 4
	var before solana.Signature
	for page := 0; page < maxPages; page++ {
		rows, err := source.History(ctx, user.Receipt, pageSize, before, rpc.CommitmentConfirmed, atLeast)
		if err != nil {
			return CurrentWithdrawalRequest{}, err
		}
		for _, row := range rows {
			if row.Failed {
				continue
			}
			ex, err := source.Execution(ctx, row.Signature, rpc.CommitmentConfirmed)
			if err != nil {
				return CurrentWithdrawalRequest{}, err
			}
			if ex.Err != nil || ex.Transaction == nil || ex.Slot != row.Slot || len(ex.Transaction.Signatures) == 0 || ex.Transaction.Signatures[0] != row.Signature {
				return CurrentWithdrawalRequest{}, errors.New("withdrawal history execution mismatch")
			}
			calls := invocations(ex)
			for i := len(calls) - 1; i >= 0; i-- {
				ix := calls[i]
				if int(ix.program) >= len(ex.Keys) || ex.Keys[ix.program] != voltr.ProgramID {
					continue
				}
				touches := false
				for _, index := range ix.accounts {
					if int(index) >= len(ex.Keys) {
						return CurrentWithdrawalRequest{}, errors.New("withdrawal history account index out of bounds")
					}
					touches = touches || ex.Keys[index] == user.Receipt
				}
				if !touches {
					continue
				}
				if bytes.HasPrefix(ix.data, voltr.RequestWithdrawVaultDiscriminator[:]) {
					return CurrentWithdrawalRequest{WithdrawalGeneration: WithdrawalGeneration{RequestSignature: row.Signature.String(), RequestInstruction: uint32(i)}, Slot: row.Slot, Execution: ex}, nil
				}
				if bytes.HasPrefix(ix.data, voltr.WithdrawVaultDiscriminator[:]) || bytes.HasPrefix(ix.data, voltr.CancelRequestWithdrawVaultDiscriminator[:]) {
					return CurrentWithdrawalRequest{}, errors.New("withdrawal request is already settled")
				}
				return CurrentWithdrawalRequest{}, errors.New("unrecognized withdrawal receipt mutation")
			}
		}
		if len(rows) < pageSize || rows[len(rows)-1].Signature == before {
			break
		}
		before = rows[len(rows)-1].Signature
	}
	return CurrentWithdrawalRequest{}, errors.New("current withdrawal request is unavailable within bounded history")
}

func (r *Reader) WithdrawalConsentDraft(ctx context.Context, owner, settings solana.PublicKey) (WithdrawalConsent, error) {
	a, err := AuthorityOf(settings)
	if err != nil {
		return WithdrawalConsent{}, err
	}
	p, err := r.Position(ctx, a, r.now())
	if err != nil {
		return WithdrawalConsent{}, err
	}
	if p.Withdrawal == nil {
		return WithdrawalConsent{}, errors.New("no active withdrawal receipt")
	}
	request, err := ReadWithdrawalRequest(ctx, r.chain, a.Key(), p.ObservedSlot)
	if err != nil {
		return WithdrawalConsent{}, err
	}
	c := WithdrawalConsent{WithdrawalGeneration: request.WithdrawalGeneration, Version: "1", Owner: owner.String(), Cluster: Cluster, Program: voltr.ProgramID.String(), Vault: Vault.String(), Settings: settings.String(), Authority: a.Key().String(), Destination: owner.String(), LPRaw: strconv.FormatInt(int64(p.Withdrawal.EscrowedLP), 10), ReferenceUSDC: strconv.FormatInt(int64(p.Withdrawal.Payout), 10), ReferenceSlot: strconv.FormatUint(p.ObservedSlot, 10), FeeBasisHash: p.FeeBasisHash, ExpiresAt: strconv.FormatInt(r.now().Add(15*time.Minute).Unix(), 10)}
	if err := verifyRequestInstruction(request.Execution, c, a.Key()); err != nil {
		return WithdrawalConsent{}, err
	}
	seed, err := r.withdrawalPolicySeed(ctx, settings, owner, p.ObservedSlot)
	if err != nil {
		return WithdrawalConsent{}, err
	}
	c.PolicySeed = strconv.FormatUint(seed, 10)
	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		return WithdrawalConsent{}, err
	}
	c.Nonce = hex.EncodeToString(nonce)
	return c, nil
}

func (r *Reader) VerifyWithdrawalGeneration(ctx context.Context, c WithdrawalConsent) error {
	settings, err := solana.PublicKeyFromBase58(c.Settings)
	if err != nil {
		return err
	}
	a, err := AuthorityOf(settings)
	if err != nil {
		return err
	}
	owner, err := solana.PublicKeyFromBase58(c.Owner)
	if err != nil {
		return err
	}
	p, err := r.Position(ctx, a, r.now())
	if err != nil {
		return err
	}
	if p.Withdrawal == nil {
		return errors.New("no active withdrawal receipt")
	}
	seed, err := r.withdrawalPolicySeed(ctx, settings, owner, p.ObservedSlot)
	if err != nil {
		return err
	}
	if strconv.FormatUint(seed, 10) != c.PolicySeed {
		return errors.New("withdrawal policy generation changed")
	}
	request, err := ReadWithdrawalRequest(ctx, r.chain, a.Key(), p.ObservedSlot)
	if err != nil {
		return err
	}
	if request.WithdrawalGeneration != c.WithdrawalGeneration {
		return errors.New("withdrawal consent belongs to another request generation")
	}
	lp, err := canonicalUint(c.LPRaw)
	if err != nil || lp != uint64(p.Withdrawal.EscrowedLP) {
		return errors.New("withdrawal consent LP differs from active receipt")
	}
	if err := verifyRequestInstruction(request.Execution, c, a.Key()); err != nil {
		return err
	}
	slot, err := canonicalUint(c.ReferenceSlot)
	if err != nil || slot < request.Slot || slot > p.ObservedSlot {
		return errors.New("invalid withdrawal reference slot")
	}
	return nil
}

func (r *Reader) withdrawalPolicySeed(ctx context.Context, settings, owner solana.PublicKey, atLeast uint64) (uint64, error) {
	_, accounts, err := r.chain.Accounts(ctx, []solana.PublicKey{settings}, rpc.CommitmentConfirmed, atLeast)
	if err != nil {
		return 0, err
	}
	if len(accounts) != 1 || accounts[0] == nil {
		return 0, errors.New("withdrawal settings unavailable")
	}
	decoded, err := squads.DecodeSettings(accounts[0])
	if err != nil {
		return 0, err
	}
	return authorizedWithdrawalPolicySeed(decoded, settings, owner)
}

func authorizedWithdrawalPolicySeed(decoded squads.Settings, settings, owner solana.PublicKey) (uint64, error) {
	derived, bump, err := squads.SettingsAddress(decoded.Seed)
	if err != nil || derived != settings || bump != decoded.Bump || decoded.Threshold != 1 || decoded.TimeLock != 0 {
		return 0, errors.New("withdrawal settings ownership changed")
	}
	authorized := false
	for _, signer := range decoded.Signers {
		if signer.Key == owner && signer.Permissions&squads.FullPermissions == squads.FullPermissions {
			authorized = true
		}
	}
	if !authorized {
		return 0, errors.New("withdrawal owner permissions changed")
	}
	if decoded.PolicySeed == nil {
		return 0, nil
	}
	return *decoded.PolicySeed, nil
}

func verifyRequestInstruction(ex chain.Execution, c WithdrawalConsent, authority solana.PublicKey) error {
	if ex.Err != nil || ex.Transaction == nil || len(ex.Transaction.Signatures) == 0 || ex.Transaction.Signatures[0].String() != c.RequestSignature {
		return errors.New("withdrawal request transaction is not confirmed successful")
	}
	calls := invocations(ex)
	if uint64(c.RequestInstruction) >= uint64(len(calls)) {
		return errors.New("withdrawal request instruction is absent")
	}
	ix := calls[c.RequestInstruction]
	if int(ix.program) >= len(ex.Keys) || ex.Keys[ix.program] != voltr.ProgramID || len(ix.data) != 18 ||
		!bytes.Equal(ix.data[:8], voltr.RequestWithdrawVaultDiscriminator[:]) || ix.data[16] != 1 || ix.data[17] > 1 || len(ix.accounts) != 10 {
		return errors.New("withdrawal request instruction ABI mismatch")
	}
	user, err := voltr.UserAccounts(Vault, LPMint, authority)
	if err != nil {
		return err
	}
	escrow, err := spl.AssociatedTokenAddress(user.Receipt, LPMint, solana.TokenProgramID)
	if err != nil {
		return err
	}
	for _, item := range []struct {
		index int
		key   solana.PublicKey
	}{{1, authority}, {3, Vault}, {4, LPMint}, {5, user.LPATA}, {6, escrow}, {7, user.Receipt}} {
		index := ix.accounts[item.index]
		if int(index) >= len(ex.Keys) || ex.Keys[index] != item.key {
			return errors.New("withdrawal request accounts mismatch")
		}
	}
	for i := int(c.RequestInstruction) + 1; i < len(calls); i++ {
		next := calls[i]
		if int(next.program) >= len(ex.Keys) || ex.Keys[next.program] != voltr.ProgramID {
			continue
		}
		if !bytes.HasPrefix(next.data, voltr.RequestWithdrawVaultDiscriminator[:]) && !bytes.HasPrefix(next.data, voltr.WithdrawVaultDiscriminator[:]) && !bytes.HasPrefix(next.data, voltr.CancelRequestWithdrawVaultDiscriminator[:]) {
			continue
		}
		for _, index := range next.accounts {
			if int(index) < len(ex.Keys) && ex.Keys[index] == user.Receipt {
				return errors.New("request generation was superseded in its transaction")
			}
		}
	}
	lp, err := canonicalUint(c.LPRaw)
	if err != nil || (ix.data[17] == 0 && binary.LittleEndian.Uint64(ix.data[8:16]) != lp) {
		return errors.New("withdrawal request amount mismatch")
	}
	return nil
}
