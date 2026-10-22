package earnmax

import (
	"context"
	"encoding/binary"
	"errors"
	"time"

	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/spl"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/voltr"
)

const (
	mintLen         = 82
	mintSupplyAt    = 36
	tokenAccountLen = 165
)

type PendingWithdrawal struct {
	Receipt          solana.PublicKey
	EscrowedLP       LPRaw
	Payout           USDCRaw
	WithdrawableFrom time.Time
}

type Position struct {
	ObservedSlot uint64
	LP           LPRaw
	Value        USDCRaw
	Idle         *USDCRaw
	Withdrawal   *PendingWithdrawal
}

type ClaimBlock string

const (
	ClaimOpen                  ClaimBlock = ""
	ClaimNotReady              ClaimBlock = "not_ready"
	ClaimInsufficientLiquidity ClaimBlock = "insufficient_liquidity"
	ClaimLiquidityUnavailable  ClaimBlock = "liquidity_unavailable"
)

func (p Position) ClaimBlock(now time.Time) ClaimBlock {
	switch w := p.Withdrawal; {
	case w == nil || now.Before(w.WithdrawableFrom):
		return ClaimNotReady
	case p.Idle == nil:
		return ClaimLiquidityUnavailable
	case *p.Idle < w.Payout:
		return ClaimInsufficientLiquidity
	}
	return ClaimOpen
}

func (r *Reader) Position(ctx context.Context, a Authority, now time.Time) (Position, error) {
	user, err := voltr.UserAccounts(Vault, LPMint, a.key)
	if err != nil {
		return Position{}, err
	}
	keys := []solana.PublicKey{Vault, LPMint, user.LPATA, user.Receipt, IdleATA}
	var slot uint64
	accounts, err := retryRateLimited(ctx, func() ([]*chain.Account, error) {
		var accounts []*chain.Account
		var err error
		slot, accounts, err = r.chain.Accounts(ctx, keys, rpc.CommitmentConfirmed, 0)
		return accounts, err
	})
	if err != nil {
		return Position{}, err
	}
	vaultAccount, mint, lpAccount, receipt, idle := accounts[0], accounts[1], accounts[2], accounts[3], accounts[4]
	if vaultAccount == nil || mint == nil {
		return Position{}, errors.New("Voltr vault is not available on this cluster.")
	}
	vault, err := voltr.DecodeVault(vaultAccount)
	if err != nil {
		return Position{}, err
	}
	if len(mint.Data) < mintLen {
		return Position{}, errors.New("the Voltr LP mint is unreadable")
	}
	supply := binary.LittleEndian.Uint64(mint.Data[mintSupplyAt:])
	toAssets := func(lp uint64) (USDCRaw, error) {
		assets, err := vault.LPToAssets(lp, supply, now)
		if err != nil {
			return 0, err
		}
		return signed[USDCRaw](assets)
	}
	position := Position{ObservedSlot: slot, Idle: idleAssets(idle)}
	if lpAccount != nil {
		token, err := spl.DecodeTokenAccount(lpAccount)
		if err != nil {
			return Position{}, err
		}
		if position.LP, err = signed[LPRaw](token.Amount); err != nil {
			return Position{}, err
		}
	}
	if position.Value, err = toAssets(uint64(position.LP)); err != nil {
		return Position{}, err
	}
	if receipt != nil {
		decoded, err := voltr.DecodeWithdrawalReceipt(receipt)
		if err != nil {
			return Position{}, err
		}
		requested, fits := decoded.AssetToWithdraw.FloorRaw()
		if !fits {
			return Position{}, errors.New("voltr withdrawal receipt amount overflows u64")
		}
		atRequest, err := signed[USDCRaw](requested)
		if err != nil {
			return Position{}, err
		}
		atPresent, err := toAssets(decoded.LPEscrowedRaw)
		if err != nil {
			return Position{}, err
		}
		escrowed, err := signed[LPRaw](decoded.LPEscrowedRaw)
		if err != nil {
			return Position{}, err
		}
		position.Withdrawal = &PendingWithdrawal{
			Receipt: user.Receipt, EscrowedLP: escrowed, Payout: min(atPresent, atRequest),
			WithdrawableFrom: time.Unix(int64(decoded.WithdrawableFromTS), 0).UTC(),
		}
	}
	return position, nil
}

func idleAssets(idle *chain.Account) *USDCRaw {
	if idle == nil || idle.Executable || idle.Owner != solana.TokenProgramID || len(idle.Data) != tokenAccountLen {
		return nil
	}
	token, err := spl.DecodeTokenAccount(idle)
	if err != nil || token.Mint != AssetMint || token.Owner != IdleAuth || token.Frozen {
		return nil
	}
	amount, err := signed[USDCRaw](token.Amount)
	if err != nil {
		return nil
	}
	return &amount
}
