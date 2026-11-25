package backyard

import (
	"bytes"
	"context"
	"fmt"

	"sort"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"
)

type feeCapitalReader interface {
	History(context.Context, solana.PublicKey, int, solana.Signature, rpc.CommitmentType, uint64) ([]chain.Signed, error)
	Execution(context.Context, solana.Signature, rpc.CommitmentType) (chain.Execution, error)
	FirstAvailableBlock(context.Context) (uint64, error)
}

// Outside collateral or a third-party debt repayment has no agreed USDC cost
// basis. Keep publishing net NAV and serving withdrawals, and suspend new fees.
func checkFeeCapital(ctx context.Context, c feeCapitalReader, addresses map[solana.PublicKey]bool, from, through uint64, known map[string]bool) (string, error) {
	if through < from || from == 0 {
		return "", fmt.Errorf("invalid Looping capital history interval")
	}
	if len(addresses) == 0 {
		return "", nil
	}
	first, err := c.FirstAvailableBlock(ctx)
	if err != nil || first > from {
		return "", fmt.Errorf("Looping capital history unavailable from slot %d", from)
	}
	keys := make([]solana.PublicKey, 0, len(addresses))
	for k := range addresses {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return bytes.Compare(keys[i][:], keys[j][:]) < 0 })
	seen := map[solana.Signature]bool{}
	for _, key := range keys {
		var before solana.Signature
		const pageSize = 128
		for page := 0; ; page++ {
			if page == 4 {
				return "", fmt.Errorf("Looping capital history exceeds bounded window")
			}
			rows, err := c.History(ctx, key, pageSize, before, rpc.CommitmentConfirmed, through)
			if err != nil {
				return "", err
			}
			for _, row := range rows {
				if row.Failed || row.Slot > through || row.Slot < from || seen[row.Signature] || known[row.Signature.String()] {
					continue
				}
				seen[row.Signature] = true
				x, err := c.Execution(ctx, row.Signature, rpc.CommitmentConfirmed)
				if err != nil {
					return "", err
				}
				if x.Transaction == nil || x.Err != nil || x.Slot != row.Slot || len(x.Transaction.Signatures) == 0 || x.Transaction.Signatures[0] != row.Signature {
					return "", fmt.Errorf("incomplete Looping capital receipt")
				}
				if !neutralFeeCapital(x, addresses) {
					// A confirmed fork can disappear. Only retain an incident
					// an operator can subsequently verify at finality.
					landed, err := c.Execution(ctx, row.Signature, rpc.CommitmentFinalized)
					if err != nil || landed.Err != nil || landed.Slot != x.Slot || landed.Transaction == nil || len(landed.Transaction.Signatures) == 0 || landed.Transaction.Signatures[0] != row.Signature || neutralFeeCapital(landed, addresses) {
						return "", fmt.Errorf("outside Looping capital not yet finalized")
					}
					return row.Signature.String(), nil
				}
			}
			if len(rows) < pageSize || rows[len(rows)-1].Slot < from {
				break
			}
			before = rows[len(rows)-1].Signature
		}
	}
	return "", nil
}

func neutralFeeCapital(x chain.Execution, watched map[solana.PublicKey]bool) bool {
	for key, obligation := range watched {
		if !obligation && x.Pre[key].Amount != x.Post[key].Amount {
			return false
		}
	}
	check := func(program uint16, accounts []uint16, data []byte) bool {
		if int(program) >= len(x.Keys) {
			return false
		}

		for _, i := range accounts {
			if int(i) >= len(x.Keys) {
				return false
			}
			if !watched[x.Keys[i]] {
				continue
			}
			if x.Keys[program] != kamino.ProgramID {
				// Without CPI metadata a wrapper could conceal a principal mutation.
				if x.Inner == nil {
					return false
				}
				continue
			}
			if !bytes.Equal(data, kamino.RefreshObligationDiscriminator[:]) {
				return false
			}
		}
		return true
	}
	for _, outer := range x.Transaction.Message.Instructions {
		if !check(outer.ProgramIDIndex, outer.Accounts, outer.Data) {
			return false
		}
	}
	for _, group := range x.Inner {
		for _, inner := range group.Instructions {
			if !check(inner.ProgramIDIndex, inner.Accounts, inner.Data) {
				return false
			}
		}
	}
	return true
}

func (d *Database) observeFeeCapital(ctx context.Context, c feeCapitalReader, view *View, s *Snapshot) {
	f := s.performanceFee
	var resolution feeCapitalResolution
	if f.CapitalHold != "" && f.CapitalHold != "history_unavailable" {
		resolution.Signature = f.CapitalHold
		err := d.pool.QueryRow(ctx, `SELECT finalized_slot,principal_usdc_raw,effective_lp_raw FROM `+resolutionTable+` WHERE signature=$1 AND applied_operation_id IS NULL`, f.CapitalHold).Scan(&resolution.Slot, &resolution.Principal, &resolution.Supply)
		if err != nil {
			s.feeCapitalHold = f.CapitalHold
			return
		}
	}
	// The coherent view retains the last write to each address. After a
	// restart its newer seed forces a fresh history read for those accounts.
	watches := feeCapitalAccounts()
	view.mu.Lock()
	for key := range watches {
		if held, ok := view.accounts[key.String()]; ok && held.slot < uint64(f.CapitalSlot) {
			delete(watches, key)
		}
	}
	view.mu.Unlock()
	s.feeCapitalHold = "history_unavailable"
	if resolution.Signature != "" {
		s.feeCapitalHold = resolution.Signature
	}
	known := map[string]bool{}
	if resolution.Signature != "" {
		known[resolution.Signature] = true
	}
	if len(watches) > 0 {
		rows, err := d.pool.Query(ctx, `SELECT transaction_signature FROM loyal_yield.multiply_operations WHERE route_key=$1 AND status='reconciled' AND confirmed_slot >= $2 AND transaction_signature IS NOT NULL UNION ALL SELECT signature FROM `+resolutionTable+` WHERE applied_operation_id IS NOT NULL AND finalized_slot >= $2`, looping.RouteKey, f.CapitalSlot)
		if err != nil {
			return
		}
		for rows.Next() {
			var signature string
			if err = rows.Scan(&signature); err != nil {
				break
			}
			known[signature] = true
		}
		rows.Close()
		if err != nil || rows.Err() != nil {
			return
		}
	}
	reason, err := checkFeeCapital(ctx, c, watches, uint64(f.CapitalSlot), uint64(s.Slot), known)
	if err != nil {
		return
	}
	s.feeResolution = resolution
	s.feeCapitalHold = reason
	s.feeCapitalSlot = s.Slot
}

func feeCapitalAccounts() map[solana.PublicKey]bool {
	watched := map[solana.PublicKey]bool{}
	for _, r := range looping.routes {
		watched[solanaKey(r.CollateralCustody)] = false
		watched[solanaKey(r.DebtCustody)] = false
		watched[solanaKey(r.Kamino.Obligation)] = true
	}
	delete(watched, solanaKey(looping.cash)) // Native USDC uses the exact finalized cash ledger.
	return watched
}
