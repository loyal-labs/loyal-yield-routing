package fleetexec

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
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
	if a == nil || a.Owner.String() != fleet.KaminoProgram || a.Executable || a.Lamports == 0 || len(a.Data) != 3344 || !bytes.Equal(a.Data[:8], []byte{168, 206, 141, 106, 88, 76, 172, 167}) || sdk.PublicKeyFromBytes(a.Data[32:64]).String() != market || sdk.PublicKeyFromBytes(a.Data[64:96]).String() != owner {
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
