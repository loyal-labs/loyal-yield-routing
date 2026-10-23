package webapp

import (
	"bytes"
	"fmt"
	"math"
	"strconv"
	"time"
	"unicode/utf8"
)

// Wire types for GET /api/smart-accounts/yield-optimization/earnings, mirroring
// web/apps/web/src/lib/yield-optimization/earnings.shared.ts field for field.

type EarningsBar struct {
	APYBps             *int64  `json:"apyBps"`
	AvgPrincipalUsd    float64 `json:"avgPrincipalUsd"`
	EarnedUsd          float64 `json:"earnedUsd"`
	EndAt              string  `json:"endAt"`
	IsCurrent          bool    `json:"isCurrent"`
	Label              string  `json:"label"`
	PrincipalAmountRaw string  `json:"principalAmountRaw"`
	PrincipalUsd       float64 `json:"principalUsd"`
	StartAt            string  `json:"startAt"`
}

type EarningsRange struct {
	Bars                      []EarningsBar `json:"bars"`
	CurrentApyBps             *int64        `json:"currentApyBps"`
	LastDepositAt             *string       `json:"lastDepositAt"`
	LifetimeEarnedUsd         float64       `json:"lifetimeEarnedUsd"`
	PrincipalAmountRaw        string        `json:"principalAmountRaw"`
	PrincipalUsd              float64       `json:"principalUsd"`
	RangeEarnedUsd            float64       `json:"rangeEarnedUsd"`
	SinceLastDepositEarnedUsd float64       `json:"sinceLastDepositEarnedUsd"`
	TodayEarnedUsd            float64       `json:"todayEarnedUsd"`
}

// EarningsRanges keeps the TS insertion order (EARNINGS_RANGE_IDS) instead of a
// Go map, whose marshal order would differ.
type EarningsRanges struct {
	SevenD  EarningsRange `json:"7D"`
	ThirtyD EarningsRange `json:"30D"`
	OneY    EarningsRange `json:"1Y"`
	All     EarningsRange `json:"ALL"`
}

func (r EarningsRanges) at(rangeID string) EarningsRange {
	switch rangeID {
	case "7D":
		return r.SevenD
	case "30D":
		return r.ThirtyD
	case "1Y":
		return r.OneY
	case "ALL":
		return r.All
	}
	return EarningsRange{}
}

func (r *EarningsRanges) set(rangeID string, v EarningsRange) {
	switch rangeID {
	case "7D":
		r.SevenD = v
	case "30D":
		r.ThirtyD = v
	case "1Y":
		r.OneY = v
	case "ALL":
		r.All = v
	}
}

type EarningsCoverage struct {
	CurrentReserveSampleAgeMs *int64   `json:"currentReserveSampleAgeMs"`
	EventCount                int      `json:"eventCount"`
	GappedReserves            []string `json:"gappedReserves"`
	MaxSampleGapMs            *int64   `json:"maxSampleGapMs"`
	MissingReserves           []string `json:"missingReserves"`
	ReserveCount              int      `json:"reserveCount"`
	SampleCount               int      `json:"sampleCount"`
	SampledReserveCount       int      `json:"sampledReserveCount"`
	StaleReserves             []string `json:"staleReserves"`
}

type EarningsRangeSet struct {
	Coverage                 EarningsCoverage `json:"coverage"`
	Freshness                string           `json:"freshness"`
	GeneratedAt              string           `json:"generatedAt"`
	HistoryRevision          string           `json:"historyRevision"`
	Outcome                  string           `json:"outcome"`
	PrincipalMatchesHistory  bool             `json:"principalMatchesHistory"`
	Ranges                   EarningsRanges   `json:"ranges"`
	SnapshotAgeMs            *int64           `json:"snapshotAgeMs"`
	SourcePrincipalAmountRaw string           `json:"sourcePrincipalAmountRaw"`
	StaleReason              *string          `json:"staleReason"`
}

// storedEarningsSnapshot is the JSONB of earn_earnings_snapshots.payload: the
// response plus the two private keys loadEarnEarningsSnapshot strips before a
// snapshot is served (earnings-read-service.server.ts:680).
type storedEarningsSnapshot struct {
	EarningsRangeSet
	SourceRevision *string                 `json:"_sourceRevision,omitempty"`
	Checkpoint     *earningsCheckpointWire `json:"_checkpoint,omitempty"`
}

// earningsCheckpointWire mirrors EarningsCheckpoint
// (earnings-checkpoint.server.ts:10) field for field so snapshots written by
// Next stay verifiable here and the other way around.
type earningsCheckpointWire struct {
	Version            int                    `json:"version"`
	At                 string                 `json:"at"`
	FirstDepositAt     string                 `json:"firstDepositAt"`
	HistoryFingerprint string                 `json:"historyFingerprint"`
	LedgerFingerprint  string                 `json:"ledgerFingerprint"`
	ApyFingerprint     string                 `json:"apyFingerprint"`
	Reserves           []string               `json:"reserves"`
	Ranges             EarningsRanges         `json:"ranges"`
	Seed               earningsCheckpointSeed `json:"seed"`
}

type earningsCheckpointSeedExposure struct {
	AmountRaw     string  `json:"amountRaw"`
	Kind          string  `json:"kind"`
	LiquidityMint string  `json:"liquidityMint"`
	Reserve       *string `json:"reserve"`
	SourceID      string  `json:"sourceId"`
}

type earningsCheckpointSeed struct {
	ObservedSlot string                           `json:"observedSlot"`
	Exposures    []earningsCheckpointSeedExposure `json:"exposures"`
}

// EarningsUnavailableBody is the 503 envelope of the Next earnings route: an
// error object with an optional detailCode plus freshness/outcome markers.
type EarningsUnavailableBody struct {
	Error struct {
		Code       string  `json:"code"`
		DetailCode *string `json:"detailCode,omitempty"`
		Message    string  `json:"message"`
	} `json:"error"`
	Freshness string `json:"freshness"`
	Outcome   string `json:"outcome"`
}

type earningsError struct {
	body EarningsUnavailableBody
}

func (e *earningsError) Error() string  { return e.body.Error.Code }
func (e *earningsError) GetStatus() int { return 503 }

// newEarningsError builds the route's 503 body the way
// yield-optimization/earnings/route.ts does: message from the code alone, and
// detailCode present exactly when the failure is a classified unavailable
// error.
func newEarningsError(code string, detailCode *string) *earningsError {
	body := EarningsUnavailableBody{Freshness: "unavailable", Outcome: "unavailable"}
	body.Error.Code = code
	body.Error.DetailCode = detailCode
	if code == "history_incomplete" {
		body.Error.Message = "Earn history is still updating."
	} else {
		body.Error.Message = "Earn earnings are unavailable."
	}
	return &earningsError{body: body}
}

// Earnings unavailable classifications from earnings-read-service.server.ts.
const (
	earningsUnavailableCode = "earnings_unavailable"
	earningsIncompleteCode  = "history_incomplete"

	detailAPYCoverageIncomplete = "apy_coverage_incomplete"
	detailDepositIncomplete     = "deposit_history_incomplete"
	detailPrincipalAmbiguous    = "principal_history_ambiguous"
	detailPrincipalMismatch     = "principal_history_mismatch"
)

type earningsUnavailable struct {
	code       string
	message    string
	detailCode string
	coverage   *EarningsCoverage
}

func (e *earningsUnavailable) Error() string { return e.message }

func unavailableErr(detailCode, message string) *earningsUnavailable {
	return &earningsUnavailable{code: earningsIncompleteCode, message: message, detailCode: detailCode}
}

// ---- JS-compatible canonical JSON (fingerprints and revisions) ----

type jsonWriter struct{ buf bytes.Buffer }

func (w *jsonWriter) String() string { return w.buf.String() }

func (w *jsonWriter) raw(s string) { w.buf.WriteString(s) }

func (w *jsonWriter) str(s string) { appendJSONString(&w.buf, s) }

func (w *jsonWriter) optStr(s *string) {
	if s == nil {
		w.raw("null")
		return
	}
	w.str(*s)
}

func (w *jsonWriter) bigint(v int64) { w.str(strconv.FormatInt(v, 10)) }

func (w *jsonWriter) optBigint(v *int64) {
	if v == nil {
		w.raw("null")
		return
	}
	w.bigint(*v)
}

func (w *jsonWriter) boolean(b bool) {
	if b {
		w.raw("true")
		return
	}
	w.raw("false")
}

func (w *jsonWriter) optBool(b *bool) {
	if b == nil {
		w.raw("null")
		return
	}
	w.boolean(*b)
}

// number writes a float64 exactly like JSON.stringify: shortest round-trip
// form, exponent notation below 1e-6 and from 1e21 (single exponent digit).
func (w *jsonWriter) number(f float64) {
	abs := math.Abs(f)
	format := byte('f')
	if abs != 0 && (abs < 1e-6 || abs >= 1e21) {
		format = 'e'
	}
	b := strconv.AppendFloat(w.buf.AvailableBuffer()[:0], f, format, -1, 64)
	if format == 'e' {
		if n := len(b); n >= 4 && b[n-4] == 'e' && b[n-3] == '-' && b[n-2] == '0' {
			b[n-2] = b[n-1]
			b = b[:n-1]
		}
	}
	w.buf.Write(b)
}

// appendJSONString quotes like JSON.stringify: no HTML escaping, raw UTF-8.
func appendJSONString(buf *bytes.Buffer, s string) {
	buf.WriteByte('"')
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			switch {
			case c == '"':
				buf.WriteString(`\"`)
			case c == '\\':
				buf.WriteString(`\\`)
			case c == '\b':
				buf.WriteString(`\b`)
			case c == '\f':
				buf.WriteString(`\f`)
			case c == '\n':
				buf.WriteString(`\n`)
			case c == '\r':
				buf.WriteString(`\r`)
			case c == '\t':
				buf.WriteString(`\t`)
			case c < 0x20:
				fmt.Fprintf(buf, `\u%04x`, c)
			default:
				buf.WriteByte(c)
			}
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			buf.WriteString("�")
			i++
			continue
		}
		buf.WriteString(s[i : i+size])
		i += size
	}
	buf.WriteByte('"')
}

// isoMS formats a time like JS Date.toISOString (millisecond UTC).
func isoMS(t time.Time) string {
	return time.UnixMilli(t.UnixMilli()).UTC().Format("2006-01-02T15:04:05.000Z07:00")
}

// msTime truncates to millisecond precision, what a JS Date keeps. Postgres
// timestamps carry microseconds the TS readers never see.
func msTime(t time.Time) time.Time {
	if _, off := t.Zone(); off == 0 {
		return time.UnixMilli(t.UnixMilli())
	}
	return time.UnixMilli(t.UnixMilli()).UTC()
}

func msTimeMillis(ms int64) time.Time { return time.UnixMilli(ms).UTC() }

func jsonFloat(f float64) string {
	var w jsonWriter
	w.number(f)
	return w.String()
}
