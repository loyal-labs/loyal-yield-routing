package webapp

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
)

// Canonical JSON for the sha256 fingerprints and revisions the TS readers
// derive with JSON.stringify. JS object key order is insertion order, so every
// writer below spells the field order out; Go map iteration would not.

func fingerprintHex(canonical string) string {
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:])
}

// writeEarningsEventJSON emits one ledger event the way JSON.stringify sees a
// findYieldPositionEvents row: bigint fields as strings, Date as ISO, and
// undefined-valued keys skipped. One key order covers deposits, withdrawals
// and settlements because each row only sets the keys its origin sets.
func writeEarningsEventJSON(w *jsonWriter, e earningsEvent) {
	w.raw(`{"amountRaw":`)
	w.bigint(e.amountRaw)
	w.raw(`,"confirmedAt":`)
	w.str(isoMS(e.confirmedAt))
	w.raw(`,"confirmedSlot":`)
	w.bigint(e.confirmedSlot)
	w.raw(`,"liquidityMint":`)
	w.str(e.liquidityMint)
	if e.holdingEventID != nil {
		w.raw(`,"holdingEventId":`)
		w.optBigint(e.holdingEventID)
	}
	if e.positionID != nil {
		w.raw(`,"positionId":`)
		w.optBigint(e.positionID)
	}
	if e.initializesPosition != nil {
		w.raw(`,"initializesPosition":`)
		w.optBool(e.initializesPosition)
	}
	if e.principalDeltaRaw != nil {
		w.raw(`,"principalDeltaRaw":`)
		w.optBigint(e.principalDeltaRaw)
	}
	if e.settlesPosition != nil {
		w.raw(`,"settlesPosition":`)
		w.optBool(e.settlesPosition)
	}
	w.raw(`,"type":`)
	w.str(string(e.eventType))
	w.raw("}")
}

func earningsLedgerFingerprint(sorted []earningsEvent, before int64) string {
	var w jsonWriter
	w.raw("[")
	first := true
	for _, event := range sorted {
		if event.confirmedAt.UnixMilli() >= before {
			continue
		}
		if !first {
			w.raw(",")
		}
		first = false
		writeEarningsEventJSON(&w, event)
	}
	w.raw("]")
	return fingerprintHex(w.String())
}

func getMaterialEarningsHistoryRevision(pathEvents []earningsPathEvent) string {
	var w jsonWriter
	w.raw("[")
	for i, event := range pathEvents {
		if i > 0 {
			w.raw(",")
		}
		w.raw(`[`)
		w.str(string(event.eventType))
		w.raw(",")
		w.str(isoMS(event.confirmedAt))
		w.raw(",")
		w.str(event.reserve)
		w.raw(",")
		w.optStr(event.market)
		w.raw(",")
		w.str(event.liquidityMint)
		w.raw(",")
		w.str(formatInt64(event.principalAmountRaw))
		w.raw(`]`)
	}
	w.raw("]")
	return fingerprintHex(w.String())
}

// getPortfolioEarningsHistoryRevision ports read-service:395. Undefined
// optional entries sit inside arrays, so they serialize as null here (unlike
// object keys, which are skipped).
func getPortfolioEarningsHistoryRevision(sorted []earningsEvent, snapshots []earningsPortfolioSnapshot) string {
	var w jsonWriter
	w.raw(`{"events":[`)
	for i, event := range sorted {
		if i > 0 {
			w.raw(",")
		}
		w.raw(`[`)
		w.str(string(event.eventType))
		w.raw(",")
		w.str(isoMS(event.confirmedAt))
		w.raw(",")
		w.str(event.liquidityMint)
		w.raw(",")
		w.str(formatInt64(event.amountRaw))
		w.raw(",")
		w.raw(eventArrayBigInt(event.confirmedSlot))
		w.raw(",")
		w.raw(eventArrayBigIntOpt(event.holdingEventID))
		w.raw(",")
		w.raw(eventArrayBigIntOpt(event.positionID))
		w.raw(",")
		w.raw(eventArrayBool(event.initializesPosition))
		w.raw(",")
		w.raw(eventArrayBigIntOpt(event.principalDeltaRaw))
		w.raw(",")
		w.raw(eventArrayBool(event.settlesPosition))
		w.raw(`]`)
	}
	w.raw(`],"snapshots":[`)
	for i, snapshot := range snapshots {
		if i > 0 {
			w.raw(",")
		}
		w.raw(`[`)
		w.str(formatInt64(snapshot.observedSlot))
		w.raw(",")
		w.str(isoMS(snapshot.observedAt))
		w.raw(",[")
		for j, exposure := range snapshot.exposures {
			if j > 0 {
				w.raw(",")
			}
			w.raw(`[`)
			w.str(exposure.sourceID)
			w.raw(",")
			w.str(string(exposure.kind))
			w.raw(",")
			w.str(exposure.liquidityMint)
			w.raw(",")
			w.optStr(exposure.reserve)
			w.raw(",")
			w.str(formatInt64(exposure.amountRaw))
			w.raw(`]`)
		}
		w.raw(`]]`)
	}
	w.raw("]}")
	return fingerprintHex(w.String())
}

func eventArrayBigInt(v int64) string {
	var w jsonWriter
	w.bigint(v)
	return w.String()
}

func eventArrayBigIntOpt(v *int64) string {
	var w jsonWriter
	if v == nil {
		return "null"
	}
	w.bigint(*v)
	return w.String()
}

func eventArrayBool(v *bool) string {
	var w jsonWriter
	w.optBool(v)
	return w.String()
}

func earningsApyFingerprint(samples []reserveApySample, before int64, reserves []string) string {
	var w jsonWriter
	w.raw("[")
	first := true
	for _, sample := range samples {
		if sample.observedAt.UnixMilli() >= before {
			continue
		}
		if reserves != nil {
			allowed := false
			if sample.reserve != nil {
				for _, reserve := range reserves {
					if *sample.reserve == reserve {
						allowed = true
						break
					}
				}
			}
			if !allowed {
				continue
			}
		}
		if !first {
			w.raw(",")
		}
		first = false
		w.raw(`{"observedAt":`)
		w.str(isoMS(sample.observedAt))
		w.raw(`,"reserve":`)
		if sample.reserve == nil {
			w.raw("null")
		} else {
			w.str(*sample.reserve)
		}
		w.raw(`,"supplyApy":`)
		w.number(sample.supplyApy)
		w.raw("}")
	}
	w.raw("]")
	return fingerprintHex(w.String())
}

type earningsSourceRevisionRow struct {
	ID           int64
	Status       string
	Principal    int64
	LastEvent    *int64
	UpdatedAtISO string
}

func (r earningsSourceRevisionRow) write(w *jsonWriter) {
	w.raw(`{"id":`)
	w.bigint(r.ID)
	w.raw(`,"status":`)
	w.str(r.Status)
	w.raw(`,"principal":`)
	w.bigint(r.Principal)
	w.raw(`,"lastEvent":`)
	w.optBigint(r.LastEvent)
	w.raw(`,"updatedAt":`)
	w.str(r.UpdatedAtISO)
	w.raw("}")
}

func getEarnEarningsSourceRevision(unsorted []earningsSourceRevisionRow) string {
	rows := make([]earningsSourceRevisionRow, len(unsorted))
	copy(rows, unsorted)
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	var w jsonWriter
	w.raw("[")
	for i, row := range rows {
		if i > 0 {
			w.raw(",")
		}
		row.write(&w)
	}
	w.raw("]")
	return "earnings-v1:" + fingerprintHex(w.String())
}

func isServerVerifiedEarnEarningsPayload(payload *EarningsRangeSet) bool {
	if payload == nil {
		return false
	}
	if payload.Outcome != "empty" && payload.Outcome != "ready" {
		return false
	}
	if payload.Freshness != "fresh" && payload.Freshness != "stale" {
		return false
	}
	if !payload.PrincipalMatchesHistory || payload.SourcePrincipalAmountRaw == "" {
		return false
	}
	for _, rangeID := range earningsRangeIDs {
		r := payload.Ranges.at(rangeID)
		if r.Bars == nil || r.PrincipalAmountRaw == "" {
			return false
		}
	}
	return true
}
