package fleetexec

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"math"
	"time"

	sdk "github.com/gagliardetto/solana-go"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
)

type finalizedAccountReader interface {
	FinalizedAccounts(context.Context, []string, int64) (int64, []fleet.Account, error)
	// Post-landing proofs read obligations with this: a full withdrawal makes
	// KLend close the obligation, which proves zero collateral (Rust parity:
	// decode_kamino_obligation_summary(None) is exists=false, amount 0).
	FinalizedAccountsAllowingAbsent(context.Context, []string, int64) (int64, []fleet.Account, error)
}

type confirmedAccountReader interface {
	ConfirmedAccounts(context.Context, []string, int64) (int64, []fleet.Account, error)
}

// sameMintRecovery reconciles a landed same-mint route against finalized
// account state.
type sameMintRecovery struct {
	slotDuration time.Duration
	store        *Store
	accounts     finalizedAccountReader
}

func obligationCollateral(a fleet.Account, market, owner, reserve string) (int64, error) {
	if a.Owner != fleet.KaminoProgram || a.Executable || a.Lamports == 0 || len(a.Data) != 3344 || !bytes.Equal(a.Data[:8], []byte{168, 206, 141, 106, 88, 76, 172, 167}) || sdk.PublicKeyFromBytes(a.Data[32:64]).String() != market || sdk.PublicKeyFromBytes(a.Data[64:96]).String() != owner {
		return 0, errors.New("obligation envelope or owner/market changed")
	}
	seen := map[string]bool{}
	var amount uint64
	for i := 0; i < 8; i++ {
		offset := 96 + i*136
		key := sdk.PublicKeyFromBytes(a.Data[offset : offset+32])
		if key.IsZero() {
			continue
		}
		name := key.String()
		if seen[name] {
			return 0, errors.New("duplicate obligation deposit")
		}
		seen[name] = true
		if name == reserve {
			amount = binary.LittleEndian.Uint64(a.Data[offset+32 : offset+40])
		}
	}
	if amount > math.MaxInt64 {
		return 0, errors.New("collateral amount exceeds BIGINT")
	}
	return int64(amount), nil
}
