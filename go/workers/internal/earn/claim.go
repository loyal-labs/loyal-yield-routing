// Package earn holds Earn MAX facts shared by the claim builder and the bridge
// that accepts a confirmed Claim.
package earn

import "errors"

// ClaimRequest is what a withdrawal memo asked to be paid: everything ("max")
// or an explicit raw USDC amount.
type ClaimRequest struct {
	Full      bool
	AmountRaw uint64
}

// ErrClaimShortfall means the claim custody holds less than an explicit
// request; the user asked for more than the unwind realized.
var ErrClaimShortfall = errors.New("claim custody holds less than the requested payout")

// ClaimPayout is the claim's transfer amount, computed from the claim
// custody's balance when the Claim transaction is built.
//
// Root cause it replaces (52c2815a, store.rs request admission): a "max"
// withdrawal saved the last equity snapshot as its payout before the unwind
// ran, so swap and repay fees left the custody below the saved number and
// Rust accepted min(saved, source_pre). A full withdrawal pays the custody
// balance the transaction is built against; an explicit request pays exactly
// its amount. There is no min.
func ClaimPayout(request ClaimRequest, custodyBalanceRaw uint64) (uint64, error) {
	if request.Full {
		if custodyBalanceRaw == 0 {
			return 0, errors.New("claim custody is empty")
		}
		return custodyBalanceRaw, nil
	}
	if request.AmountRaw == 0 {
		return 0, errors.New("claim request amount is zero")
	}
	if custodyBalanceRaw < request.AmountRaw {
		return 0, ErrClaimShortfall
	}
	return request.AmountRaw, nil
}

// VerifyClaimTransfer is what the bridge accepts from a confirmed Claim: the
// custody's pre-transaction balance is the balance the payout was built from,
// and the transfer moved exactly that payout out of it.
func VerifyClaimTransfer(request ClaimRequest, sourcePreRaw, sourcePostRaw, transferredRaw uint64) error {
	payout, err := ClaimPayout(request, sourcePreRaw)
	if err != nil {
		return err
	}
	if transferredRaw != payout || sourcePostRaw != sourcePreRaw-payout {
		return errors.New("claim transfer differs from the payout built from its source balance")
	}
	return nil
}
