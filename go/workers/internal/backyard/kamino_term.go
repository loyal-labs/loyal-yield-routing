package backyard

// A term reserve (KLend v1.25 term debt; the wYLDS reserve 34GJ… lends for
// 30 days) admits a borrow only while two deadlines are ahead: its market-wide
// DebtMaturityTimestamp, when it has one, and each obligation's own term end,
// its last borrow plus DebtTermSeconds. Every borrow on the reserve starts the
// obligation's term again. Past its end a borrow can borrow no more
// (DebtReachedReserveDebtTerm) and becomes liquidatable.

// kaminoReserveTermOpen reports whether reserve admits borrowing at chain
// time now, reserve-wide: interest terms the workers price
// (SupportedInterestTerms: no early-repay interest) and no market-wide
// maturity, or one still ahead.
func kaminoReserveTermOpen(reserve decodedKaminoReserve, now int64) bool {
	t := reserve.terms
	return t.SupportedInterestTerms() == nil && (t.DebtMaturityTimestamp == 0 || now >= 0 && uint64(now) < t.DebtMaturityTimestamp)
}

// kaminoDebtTermEnd is the unix time obligation's debt term on reserve
// ends: its last borrow plus the reserve's debt term (kamino's DebtTermEnd).
// It is absent when the reserve has no term or the obligation no borrow.
func kaminoDebtTermEnd(obligation decodedKaminoObligation, reserve decodedKaminoReserve) (uint64, bool) {
	if obligation.debtRaw == 0 {
		return 0, false
	}
	return obligation.debtBorrow.DebtTermEnd(reserve.terms)
}

// kaminoObligationTermOpen reports whether obligation may still borrow from
// reserve at chain time now: the reserve admits it and the obligation's own
// term, when it has one, has not ended.
func kaminoObligationTermOpen(obligation decodedKaminoObligation, reserve decodedKaminoReserve, now int64) bool {
	if !kaminoReserveTermOpen(reserve, now) {
		return false
	}
	end, ok := kaminoDebtTermEnd(obligation, reserve)
	return !ok || now >= 0 && uint64(now) < end
}

// wyldsTermEnd is the unix time route's obligation's debt term ends, read
// from one account batch holding the obligation and the debt reserve: what
// the keep-alive and maturity decisions read. It is absent when the
// reserve has no term or the obligation no debt (or no account).
func wyldsTermEnd(accounts []ConfirmedAccount, route RuntimeRoute) (uint64, bool, error) {
	reserve, err := decodeKaminoReserve(accountAt(accounts, route.Kamino.DebtReserve), route.Kamino.DebtMint, route.Kamino)
	if err != nil {
		return 0, false, err
	}
	account := accountAt(accounts, route.Kamino.Obligation)
	if account.Address != route.Kamino.Obligation {
		return 0, false, budgetHold("term_obligation_missing")
	}
	if account.Lamports == 0 {
		return 0, false, nil
	}
	obligation, err := decodeKaminoObligation(account, route.Kamino)
	if err != nil {
		return 0, false, err
	}
	end, ok := kaminoDebtTermEnd(obligation, reserve)
	return end, ok, nil
}
