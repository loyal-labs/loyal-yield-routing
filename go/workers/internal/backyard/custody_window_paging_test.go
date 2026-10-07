package backyard

import (
	"fmt"
	"testing"
)

// An inert reconciled row (a NAV-report-like record): strict canonical
// evidence and readable expected effects, neither naming the custody.
func custodyAttributionInertRow(t *testing.T, opID string, slot int64) custodyAttributionRow {
	t.Helper()
	expected := ExpectedEffects{Schema: "loyal-backyard-rwa-expected-effects/v1", Conserved: true, Accounts: []ExpectedAccountEffect{
		{Address: autoAUTOPYUSD.DebtLiquiditySupply, Owner: token2022Program, Mint: autoAUTOPYUSD.Kamino.DebtMint, Authority: autoAUTOPYUSD.Kamino.MarketAuthority, BeforeRaw: 1_000, AfterRaw: 2_000},
		{Address: autoAUTOPYUSD.DebtFeeReceiver, Owner: token2022Program, Mint: autoAUTOPYUSD.Kamino.DebtMint, Authority: autoAUTOPYUSD.Kamino.MarketAuthority, BeforeRaw: 2_000, AfterRaw: 1_000},
	}}
	return custodyAttributionRowFrom(t, custodyAttributionParams{
		OperationID: opID, Action: string(ReportNAV), Signature: "sig-" + opID, Slot: slot, Expected: expected,
		Pre:  []TransactionTokenBalance{custodyAttributionBalance(autoAUTOPYUSD.DebtLiquiditySupply, token2022Program, autoAUTOPYUSD.Kamino.DebtMint, autoAUTOPYUSD.Kamino.MarketAuthority, 1_000), custodyAttributionBalance(autoAUTOPYUSD.DebtFeeReceiver, token2022Program, autoAUTOPYUSD.Kamino.DebtMint, autoAUTOPYUSD.Kamino.MarketAuthority, 2_000)},
		Post: []TransactionTokenBalance{custodyAttributionBalance(autoAUTOPYUSD.DebtLiquiditySupply, token2022Program, autoAUTOPYUSD.Kamino.DebtMint, autoAUTOPYUSD.Kamino.MarketAuthority, 2_000), custodyAttributionBalance(autoAUTOPYUSD.DebtFeeReceiver, token2022Program, autoAUTOPYUSD.Kamino.DebtMint, autoAUTOPYUSD.Kamino.MarketAuthority, 1_000)},
	})
}

// journal: tip repay, then inert rows, then the funding origin, newest first.
func custodyPagedJournal(t *testing.T, inert int, corruptAt int) []custodyAttributionRow {
	t.Helper()
	nav := custodyAttributionInertRow(t, "nav", 150)
	journal := []custodyAttributionRow{custodyAttributionRepayRow(t, "auto-repay", "sig-repay", 200)}
	for i := 0; i < inert; i++ {
		row := nav
		row.OperationID = fmt.Sprintf("nav-%d", i)
		if i == corruptAt {
			row = custodyAttributionJournalOnlyRow("nav-corrupt", "sig-nav-corrupt", 150, `{}`, sha256Bytes([]byte(`{}`)))
		}
		journal = append(journal, row)
	}
	journal = append(journal, custodyAttributionFundingRow(t, "auto-fund", "sig-fund", 100))
	return append(journal, custodyAttributionInertRow(t, "behind-origin", 50))
}

func pagedCustodyEvidence(t *testing.T, journal []custodyAttributionRow, pageSize, bound int) (sharedCustodyAttributionEvidence, int) {
	t.Helper()
	fetches := 0
	rows, exhausted, err := pageSharedCustodyWindow(custodyAttributionConfig(), pageSize, bound, func(offset, size int) ([]custodyAttributionRow, error) {
		fetches++
		if offset >= len(journal) {
			return nil, nil
		}
		return journal[offset:min(len(journal), offset+size)], nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return sharedCustodyAttributionEvidence{Rows: rows, WindowExhausted: exhausted}, fetches
}

func TestSharedCustodyWindowPagesBackToTheOrigin(t *testing.T) {
	cfg := custodyAttributionConfig()
	// Live shape: the origin sits behind 98 inert reports, past one 64-row page.
	journal := custodyPagedJournal(t, 98, -1)
	evidence, fetches := pagedCustodyEvidence(t, journal, 64, sharedCustodyAttributionWindowBound)
	if fetches != 2 || evidence.WindowExhausted || len(evidence.Rows) != len(journal) {
		t.Fatalf("paging did not stop at the page holding the origin: fetches=%d rows=%d", fetches, len(evidence.Rows))
	}
	proof, err := validateSharedCustodyAttribution(3_100_000_000, 300, cfg, evidence, 0)
	if err != nil || proof.Origin.Signature != "sig-fund" || len(proof.Steps) != 2 {
		t.Fatalf("origin at row 100 not proven: %v %+v", err, proof)
	}
	// The old fixed 64-row window could not reach it.
	short := sharedCustodyAttributionEvidence{Rows: journal[:64]}
	if _, err := validateSharedCustodyAttribution(3_100_000_000, 300, cfg, short, 0); custodyAttributionHoldReason(t, err) != "custody_attribution_origin_unproven" {
		t.Fatal("fixture does not reproduce the live window miss")
	}
}

func TestSharedCustodyWindowBoundRefusesAnOriginBeyondIt(t *testing.T) {
	cfg := custodyAttributionConfig()
	journal := custodyPagedJournal(t, 300, -1)
	evidence, _ := pagedCustodyEvidence(t, journal, 64, 256)
	if !evidence.WindowExhausted || len(evidence.Rows) != 256 {
		t.Fatalf("bound not enforced: exhausted=%t rows=%d", evidence.WindowExhausted, len(evidence.Rows))
	}
	if _, err := validateSharedCustodyAttribution(3_100_000_000, 300, cfg, evidence, 0); custodyAttributionHoldReason(t, err) != "custody_attribution_bound_exhausted" {
		t.Fatalf("origin beyond the bound accepted: %v", err)
	}
	// The same journal inside the bound is proven.
	evidence, _ = pagedCustodyEvidence(t, journal, 64, sharedCustodyAttributionWindowBound)
	if _, err := validateSharedCustodyAttribution(3_100_000_000, 300, cfg, evidence, 0); err != nil {
		t.Fatalf("origin inside the bound refused: %v", err)
	}
}

func TestSharedCustodyWindowStillChecksRowsInLaterPages(t *testing.T) {
	cfg := custodyAttributionConfig()
	// A corrupt no-custody record on the second page, between tip and origin.
	journal := custodyPagedJournal(t, 98, 80)
	evidence, _ := pagedCustodyEvidence(t, journal, 64, sharedCustodyAttributionWindowBound)
	if _, err := validateSharedCustodyAttribution(3_100_000_000, 300, cfg, evidence, 0); custodyAttributionHoldReason(t, err) != "custody_attribution_malformed_record" {
		t.Fatalf("malformed row on a later page skipped: %v", err)
	}
	// A journal with no origin at all ends without exhausting the bound.
	noOrigin := custodyPagedJournal(t, 10, -1)[:11]
	evidence, _ = pagedCustodyEvidence(t, noOrigin, 64, sharedCustodyAttributionWindowBound)
	if evidence.WindowExhausted {
		t.Fatal("short journal reported as exhausted")
	}
	if _, err := validateSharedCustodyAttribution(3_100_000_000, 300, cfg, evidence, 0); custodyAttributionHoldReason(t, err) != "custody_attribution_origin_unproven" {
		t.Fatalf("journal without origin accepted: %v", err)
	}
}

// A funding row without a positive confirmed slot is not a paging origin:
// paging continues past it and the route-wide malformed-identity gate holds.
func TestSharedCustodyWindowNeverStopsAtAnUnorderedOrigin(t *testing.T) {
	cfg := custodyAttributionConfig()
	unordered := custodyAttributionFundingRow(t, "auto-fund-null", "sig-fund-null", 100)
	if !custodyRowIsZeroStartOrigin(unordered, cfg) {
		t.Fatal("fixture is not an origin with a slot")
	}
	unordered.ConfirmedSlot = 0
	if custodyRowIsZeroStartOrigin(unordered, cfg) {
		t.Fatal("NULL-slot funding row ended paging")
	}
	journal := []custodyAttributionRow{custodyAttributionRepayRow(t, "auto-repay", "sig-repay", 200), unordered, custodyAttributionInertRow(t, "after", 50)}
	evidence, fetches := pagedCustodyEvidence(t, journal, 2, sharedCustodyAttributionWindowBound)
	if fetches != 2 || len(evidence.Rows) != 3 || evidence.WindowExhausted {
		t.Fatalf("paging stopped at the unordered row: fetches=%d rows=%d", fetches, len(evidence.Rows))
	}
	evidence.MalformedIdentity = true // what the reader's route-wide EXISTS reports for this row
	if _, err := validateSharedCustodyAttribution(3_100_000_000, 300, cfg, evidence, 0); custodyAttributionHoldReason(t, err) != "custody_attribution_malformed_identity" {
		t.Fatalf("unordered origin not held: %v", err)
	}
}
