package earnmax

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/voltr"
)

var ErrClaimWaitingNAVPublication = errors.New("claim_waiting_nav_publication")

type rowQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

// certifiedVaultNAV is the worker's latest certified total NAV for Vault.
type certifiedVaultNAV struct {
	totalRaw               uint64
	certifiedSlot, maxSlot int64
	validUntil             time.Time
}

// readCertifiedVaultNAV returns nil when the worker has certified none.
func readCertifiedVaultNAV(ctx context.Context, q rowQuerier) (*certifiedVaultNAV, error) {
	var total string
	var c certifiedVaultNAV
	err := q.QueryRow(ctx, `SELECT total_nav_raw::text,certified_slot,max_slot,valid_until FROM loyal_yield.backyard_certified_nav WHERE vault=$1`, Vault.String()).Scan(&total, &c.certifiedSlot, &c.maxSlot, &c.validUntil)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if c.totalRaw, err = strconv.ParseUint(total, 10, 64); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *certifiedVaultNAV) validAt(now time.Time, slot uint64) bool {
	return c != nil && now.Before(c.validUntil) && int64(slot) >= c.certifiedSlot && int64(slot) <= c.maxSlot
}

// depositPricePublished bounds the published total value a deposit mints at.
// Below certified NAV the issuance fee left in the vault compensates holders
// for the extra LP, up to the fee itself.
func depositPricePublished(nav uint64, v voltr.Vault) bool {
	return !NAVExceeds(nav, v.TotalValueRaw, uint64(v.IssuanceFeeBPS)) && !NAVExceeds(v.TotalValueRaw, nav, DepositOverpayBPS)
}

// claimPricePublished refuses a claim that a published total value above
// certified NAV would overpay.
func claimPricePublished(nav *certifiedVaultNAV, now time.Time, slot, totalValueRaw uint64) error {
	if !nav.validAt(now, slot) || NAVExceeds(totalValueRaw, nav.totalRaw, NAVPublicationBandBPS) {
		return ErrClaimWaitingNAVPublication
	}
	return nil
}

// CheckNAVPublication prevents claims against a stale, overstated vault price.
func (s *WithdrawalAttemptStore) CheckNAVPublication(ctx context.Context) error {
	if s.chain == nil {
		return ErrWithdrawalClaimWireUnqualified
	}
	slot, accounts, err := s.chain.Accounts(ctx, []solana.PublicKey{Vault}, rpc.CommitmentConfirmed, 0)
	if err != nil {
		return err
	}
	if len(accounts) != 1 || accounts[0] == nil {
		return errors.New("claim vault account unavailable")
	}
	vault, err := voltr.DecodeVault(accounts[0])
	if err != nil {
		return err
	}
	nav, err := readCertifiedVaultNAV(ctx, s.db)
	if err != nil {
		return err
	}
	return claimPricePublished(nav, time.Now(), slot, vault.TotalValueRaw)
}
