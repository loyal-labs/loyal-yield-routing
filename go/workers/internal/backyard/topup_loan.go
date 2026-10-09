package backyard

import (
	"encoding/binary"
	"math"
	"math/big"
)

// Immutable actual finalized principal, not a perpetual execution budget.
// Fresh per-attempt phase3 payoff/rate/freshness/cost guards remain separate.
type topupLoan struct {
	CollateralRaw        uint64   `json:"collateralRaw"`
	DebtAmountSF         [16]byte `json:"debtAmountSf"`
	CumulativeBorrowRate [32]byte `json:"cumulativeBorrowRate"`
	BorrowedAtUnix       uint64   `json:"borrowedAtUnix"`
	ObservedSlot         int64    `json:"observedSlot"`
	ClockSlot            int64    `json:"clockSlot"`
	ChainUnix            int64    `json:"chainUnix"`
	AccountsSHA256       string   `json:"accountsSha256"`
}

// Decoding grants no finality authority. The production origin must come from
// observeTopupLoanOrigin, then be bound once in the authoritative journal.
func captureTopupLoan(accounts []ConfirmedAccount, route RuntimeRoute, slot int64) (topupLoan, error) {
	if err := validateTopupLoanCapture(accounts, route, slot); err != nil {
		return topupLoan{}, err
	}
	bound, err := decodeKaminoPayoffBound(accounts, route, slot)
	if err != nil {
		return topupLoan{}, err
	}
	a := accountAt(accounts, route.Kamino.Obligation)
	o, err := decodeKaminoObligation(a, route.Kamino)
	if err != nil || o.collateralDepositedRaw == 0 || o.debtRaw == 0 {
		return topupLoan{}, budgetHold("topup_loan_unavailable")
	}
	marker, err := topupBorrowMarker(a, route.Kamino)
	if err != nil {
		return topupLoan{}, err
	}
	loan := topupLoan{CollateralRaw: o.collateralDepositedRaw, DebtAmountSF: o.debtAmountSF, CumulativeBorrowRate: o.cumulativeBorrowRate,
		BorrowedAtUnix: marker, ObservedSlot: slot, ClockSlot: int64(binary.LittleEndian.Uint64(accountAt(accounts, budgetClockAddress).Data[:8])),
		ChainUnix: bound.ChainUnix, AccountsSHA256: hashConfirmedAccounts(accounts)}
	if err := loan.validatePrincipal(accounts, route, slot); err != nil {
		return topupLoan{}, err
	}
	return loan, nil
}

func validateTopupLoanCapture(accounts []ConfirmedAccount, route RuntimeRoute, slot int64) error {
	if route.Lane != autoAUTOPYUSD.Lane || route.Kamino != autoAUTOPYUSD.Kamino || route.DebtCustody != autoAUTOPYUSD.DebtCustody || route.DebtTokenProgram != autoAUTOPYUSD.DebtTokenProgram {
		return budgetHold("topup_loan_route_changed")
	}
	seen := make(map[string]bool, len(accounts))
	for _, a := range accounts {
		if a.Address == "" || seen[a.Address] || (a.ValuationSource != "" && a.ValuationSource != "confirmed") || (a.ValuationSlot != 0 && a.ValuationSlot != slot) {
			return budgetHold("topup_loan_provenance_changed")
		}
		seen[a.Address] = true
	}
	return nil
}

// Reviewed a087609 KLend: reserve32 + BigFractionBytes48, then borrow timestamp.
// SDK 7.3.9 calls this u64 padding. The top-up program-identity gate is required.
func topupBorrowMarker(a ConfirmedAccount, c KaminoObservationConfig) (uint64, error) {
	if _, err := decodeKaminoObligation(a, c); err != nil {
		return 0, err
	}
	for i := 0; i < 5; i++ {
		offset := 1208 + i*200
		if sameKey(a.Data[offset:offset+32], c.DebtReserve) && !allZero(a.Data[offset+88:offset+104]) {
			return binary.LittleEndian.Uint64(a.Data[offset+80 : offset+88]), nil
		}
	}
	return 0, budgetHold("topup_loan_borrow_marker_unavailable")
}

func (loan topupLoan) valid() bool {
	return loan.CollateralRaw > 0 && loan.CollateralRaw <= math.MaxInt64 && !allZero(loan.DebtAmountSF[:]) && !allZero(loan.CumulativeBorrowRate[:]) &&
		loan.ObservedSlot > 0 && loan.ObservedSlot < math.MaxInt64 && loan.ClockSlot >= loan.ObservedSlot && loan.ClockSlot <= loan.ObservedSlot+1 &&
		loan.ChainUnix > 0 && uint64(loan.ChainUnix) > loan.BorrowedAtUnix && sha256Pattern.MatchString(loan.AccountsSHA256)
}

// This proves principal consistency only. It neither admits an executable
// attempt nor renews any old payoff window, reservation, signature or consent.
// Finalized origin + coherent descendant reads and ancestor-monotonic Clock
// make the strict origin time/unchanged-marker rule exclude later borrowing.
func (loan topupLoan) validatePrincipal(accounts []ConfirmedAccount, route RuntimeRoute, slot int64) error {
	if err := validateTopupLoanCapture(accounts, route, slot); err != nil {
		return err
	}
	current, err := decodeKaminoPayoffBound(accounts, route, slot)
	if err != nil {
		return err
	}
	clockSlot := int64(binary.LittleEndian.Uint64(accountAt(accounts, budgetClockAddress).Data[:8]))
	if !loan.valid() || current.ObservedSlot < loan.ObservedSlot || current.ChainUnix < loan.ChainUnix || clockSlot < loan.ClockSlot {
		return budgetHold("topup_loan_origin_changed")
	}
	a := accountAt(accounts, route.Kamino.Obligation)
	o, err := decodeKaminoObligation(a, route.Kamino)
	if err != nil || o.collateralDepositedRaw != loan.CollateralRaw || o.debtRaw == 0 {
		return budgetHold("topup_loan_position_changed")
	}
	marker, err := topupBorrowMarker(a, route.Kamino)
	if err != nil || marker != loan.BorrowedAtUnix {
		return budgetHold("topup_loan_borrow_marker_changed")
	}
	r, err := decodeKaminoReserve(accountAt(accounts, route.Kamino.DebtReserve), route.Kamino.DebtMint, route.Kamino)
	if err != nil {
		return err
	}
	former, refreshed := littleInt(loan.CumulativeBorrowRate[:]), littleInt(o.cumulativeBorrowRate[:])
	if former.Sign() <= 0 || refreshed.Cmp(former) < 0 || refreshed.Cmp(littleInt(r.cumulativeBorrowRate[:])) > 0 {
		return budgetHold("topup_loan_rate_changed")
	}
	upper := new(big.Int).Mul(littleInt(loan.DebtAmountSF[:]), refreshed)
	if upper.BitLen() > 256 {
		return budgetHold("topup_loan_rate_changed")
	}
	upper.Quo(upper, former)
	gap := new(big.Int)
	if refreshed.Cmp(former) > 0 {
		// One pending old-rate refresh may occur in the origin's own slot.
		// Later distinct reserve rates require later slots. Each floor loss
		// is <1 SF, amplified at most R_final/R_origin, so integer gap is
		// <=ceil(N*R_final/R_origin)-1. Same rate has EXACTLY zero allowance.
		count := big.NewInt(clockSlot - loan.ClockSlot + 1)
		numerator := new(big.Int).Mul(count, refreshed)
		oneRawSF := new(big.Int).Lsh(big.NewInt(1), 60)
		if numerator.Cmp(new(big.Int).Mul(oneRawSF, former)) > 0 {
			return budgetHold("topup_loan_rounding_proof_unavailable")
		}
		gap.Quo(new(big.Int).Sub(numerator, big.NewInt(1)), former)
	}
	actual := littleInt(o.debtAmountSF[:])
	if actual.Cmp(upper) > 0 || actual.Cmp(new(big.Int).Sub(upper, gap)) < 0 {
		return budgetHold("topup_loan_principal_changed")
	}
	// Do not add raw-ceil equality: one lawful SF floor can cross a raw ceil.
	cashAccount := accountAt(accounts, route.DebtCustody)
	mint, _ := decodeBase58PublicKey(route.Kamino.DebtMint)
	owner, _ := decodeBase58PublicKey(bridgeVault)
	cash, err := DecodeTokenCustody(cashAccount.Owner, cashAccount.Data, mint, owner)
	if err != nil || cashAccount.Owner != route.DebtTokenProgram || cashAccount.Executable || cashAccount.Lamports == 0 || cash.Raw != 0 {
		return budgetHold("topup_loan_debt_custody_changed")
	}
	return nil
}
