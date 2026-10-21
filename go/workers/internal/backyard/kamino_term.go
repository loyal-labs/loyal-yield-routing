package backyard

// A term reserve (KLend v1.25 term debt; the wYLDS reserve 34GJ… lends for
// 30 days) admits a borrow only while two deadlines are ahead: its market-wide
// DebtMaturityTimestamp, when it has one, and each obligation's own term end,
// its last borrow plus DebtTermSeconds. Every borrow on the reserve starts the
// obligation's term again. Past its end a borrow can borrow no more
// (DebtReachedReserveDebtTerm) and becomes liquidatable.

func kaminoReserveTermOpen(reserve decodedKaminoReserve, now int64) bool {
	t := reserve.terms
	return t.SupportedInterestTerms() == nil && (t.DebtMaturityTimestamp == 0 || now >= 0 && uint64(now) < t.DebtMaturityTimestamp)
}

// debtTermEnd is the one derivation of an obligation's borrow maturity on
// reserve: its own term end (last borrow plus the reserve's debt term), or
// the reserve's market-wide maturity when that is earlier. Zero when it owes
// nothing there or the reserve has no term; termMaturityUnknown when term
// debt carries no borrow time.
func debtTermEnd(obligation decodedKaminoObligation, reserve decodedKaminoReserve) int64 {
	terms := reserve.terms
	if obligation.debtRaw == 0 || terms.DebtTermSeconds == 0 && terms.DebtMaturityTimestamp == 0 {
		return 0
	}
	end, ok := obligation.debtBorrow.DebtTermEnd(terms)
	if terms.DebtTermSeconds != 0 && (!ok || end > 1<<62) {
		return termMaturityUnknown
	}
	if market := terms.DebtMaturityTimestamp; market != 0 && market <= 1<<62 && (!ok || market < end) {
		end = market
	}
	return int64(end)
}

func kaminoObligationTermOpen(obligation decodedKaminoObligation, reserve decodedKaminoReserve, now int64) bool {
	end := debtTermEnd(obligation, reserve)
	return kaminoReserveTermOpen(reserve, now) && end != termMaturityUnknown && (end == 0 || now < end)
}

func observedDebtTermEnd(accounts []ConfirmedAccount, route RuntimeRoute) (int64, error) {
	account := accountAt(accounts, route.Kamino.Obligation)
	if account.Address != route.Kamino.Obligation || account.Lamports == 0 {
		return 0, nil
	}
	reserve, err := decodeKaminoReserve(accountAt(accounts, route.Kamino.DebtReserve), route.Kamino.DebtMint, route.Kamino)
	if err != nil {
		return 0, err
	}
	obligation, err := decodeKaminoObligation(account, route.Kamino)
	if err != nil {
		return 0, err
	}
	return debtTermEnd(obligation, reserve), nil
}
