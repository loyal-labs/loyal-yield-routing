package earn

import "errors"

type ClaimRequest struct {
	Full      bool
	AmountRaw uint64
}

var ErrClaimShortfall = errors.New("claim custody holds less than the requested payout")

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
