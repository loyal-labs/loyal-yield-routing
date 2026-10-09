package fleetexec

import (
	"errors"
	"math"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
	sdk "github.com/solana-foundation/solana-go/v2"
)

// sameMintRecovery reconciles a landed same-mint route against finalized
// account state. A full withdrawal makes KLend close the obligation, so an
// absent obligation proves zero collateral (Rust parity:
// decode_kamino_obligation_summary(None) is exists=false, amount 0).
type sameMintRecovery struct {
	slotDuration time.Duration
	store        *Store
	accounts     fleet.AccountReader
}

func obligationCollateral(a *chain.Account, market, owner, reserve string) (int64, error) {
	obligation, err := kamino.DecodeObligation(a)
	if err != nil || obligation.LendingMarket.String() != market || obligation.Owner.String() != owner {
		return 0, errors.New("obligation envelope or owner/market changed")
	}
	seen := map[sdk.PublicKey]bool{}
	var amount uint64
	for _, deposit := range obligation.Deposits {
		if deposit.Reserve.IsZero() {
			continue
		}
		if seen[deposit.Reserve] {
			return 0, errors.New("duplicate obligation deposit")
		}
		seen[deposit.Reserve] = true
		if deposit.Reserve.String() == reserve {
			amount = deposit.DepositedAmount
		}
	}
	if amount > math.MaxInt64 {
		return 0, errors.New("collateral amount exceeds BIGINT")
	}
	return int64(amount), nil
}
