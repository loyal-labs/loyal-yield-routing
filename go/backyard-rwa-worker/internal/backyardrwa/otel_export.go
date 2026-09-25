package backyardrwa

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// OTLP/HTTP JSON log export for ClickStack alerts, the same wire shape as the
// web app's features/observability/otlp.ts. Export is best-effort and never
// on the money path: emit only enqueues, a full queue drops, and one goroutine
// batches and posts with a bounded timeout. Unset env disables it entirely.
const (
	otelQueueSize     = 1024
	otelBatchSize     = 100
	otelFlushInterval = 2 * time.Second
	otelPostTimeout   = 3 * time.Second
	otelDetailLimit   = 300
)

// otelLogs is the process exporter, set once by Run before any goroutine
// starts. Every method is nil-safe, so tests and one-shot commands export
// nothing.
var otelLogs *otelExporter

type otelAttr struct {
	Key   string         `json:"key"`
	Value map[string]any `json:"value"`
}

func otelStr(key, value string) otelAttr {
	return otelAttr{key, map[string]any{"stringValue": value}}
}

func otelInt(key string, value int64) otelAttr {
	return otelAttr{key, map[string]any{"intValue": strconv.FormatInt(value, 10)}}
}

func otelBool(key string, value bool) otelAttr {
	return otelAttr{key, map[string]any{"boolValue": value}}
}

type otelRecord struct {
	Attributes     []otelAttr     `json:"attributes"`
	Body           map[string]any `json:"body"`
	ObservedTime   string         `json:"observedTimeUnixNano"`
	SeverityNumber int            `json:"severityNumber"`
	SeverityText   string         `json:"severityText"`
	TimeUnixNano   string         `json:"timeUnixNano"`
}

type otelExporter struct {
	endpoint, key, version string
	client                 *http.Client
	queue                  chan otelRecord
	stop, done             chan struct{}
	stopOnce               sync.Once
	dropped                atomic.Int64
	now                    func() time.Time

	mu            sync.Mutex
	last          map[string]time.Time // per-kind throttle
	failKey       string               // repeated_failure: current action|reason streak
	failCount     int
	action        Action // action of the current tick, if decided
	lane          string
	ltvBPS, nav   int64
	manual        bool
	latchReason   string
	withdrawSince time.Time
	selectorSince time.Time
	cause         string
	causeAt       time.Time
}

// newOtelExporterFromEnv returns nil (export disabled) unless both env vars
// hold a usable endpoint and key, mirroring the web getTelemetryConfig.
func newOtelExporterFromEnv(version string) *otelExporter {
	raw := strings.TrimSpace(os.Getenv("OBSERVABILITY_OTLP_ENDPOINT"))
	key := strings.TrimSpace(os.Getenv("OBSERVABILITY_INGESTION_API_KEY"))
	if raw == "" || key == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return nil
	}
	local := u.Scheme == "http" && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost")
	if u.Scheme != "https" && !local {
		return nil
	}
	u.Path, u.RawQuery, u.Fragment = "/v1/logs", "", ""
	e := newOtelExporter(u.String(), key, version, otelQueueSize)
	go e.run()
	return e
}

// newOtelExporter builds an exporter without starting its sender.
func newOtelExporter(endpoint, key, version string, queueSize int) *otelExporter {
	return &otelExporter{
		endpoint: endpoint, key: key, version: version,
		client: &http.Client{Timeout: otelPostTimeout},
		queue:  make(chan otelRecord, queueSize),
		stop:   make(chan struct{}), done: make(chan struct{}),
		now:  time.Now,
		last: map[string]time.Time{},
	}
}

func (e *otelExporter) run() {
	defer close(e.done)
	ticker := time.NewTicker(otelFlushInterval)
	defer ticker.Stop()
	batch := make([]otelRecord, 0, otelBatchSize)
	flush := func(ctx context.Context) {
		if len(batch) > 0 {
			e.post(ctx, batch)
			batch = batch[:0]
		}
	}
	for {
		select {
		case r := <-e.queue:
			if batch = append(batch, r); len(batch) >= otelBatchSize {
				flush(context.Background())
			}
		case <-ticker.C:
			flush(context.Background())
		case <-e.stop:
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			for {
				select {
				case r := <-e.queue:
					if batch = append(batch, r); len(batch) >= otelBatchSize {
						flush(ctx)
					}
				default:
					flush(ctx)
					return
				}
			}
		}
	}
}

// shutdown flushes best-effort and waits at most timeout.
func (e *otelExporter) shutdown(timeout time.Duration) {
	if e == nil {
		return
	}
	e.stopOnce.Do(func() { close(e.stop) })
	select {
	case <-e.done:
	case <-time.After(timeout):
	}
}

func (e *otelExporter) post(ctx context.Context, records []otelRecord) {
	body, err := json.Marshal(e.payload(records))
	if err != nil {
		return
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, e.endpoint, bytes.NewReader(body))
	if err != nil {
		return
	}
	request.Header.Set("authorization", e.key)
	request.Header.Set("content-type", "application/json")
	if response, err := e.client.Do(request); err == nil {
		_ = response.Body.Close()
	}
}

func (e *otelExporter) payload(records []otelRecord) map[string]any {
	return map[string]any{"resourceLogs": []any{map[string]any{
		"resource": map[string]any{"attributes": []otelAttr{
			otelStr("service.name", "backyard-rwa-worker"),
			otelStr("service.version", e.version),
			otelStr("deployment.environment.name", "production"),
		}},
		"scopeLogs": []any{map[string]any{
			"logRecords": records,
			"scope":      map[string]any{"name": "loyal.backyard_rwa.worker", "version": "1"},
		}},
	}}}
}

var otelSeverityNumbers = map[string]int{"ERROR": 17, "WARN": 13, "INFO": 9}

// emit enqueues one record and never blocks. code is the loyal.error.code
// column; it is omitted when empty.
func (e *otelExporter) emit(severity, kind, code string, attrs ...otelAttr) {
	if e == nil {
		return
	}
	e.mu.Lock()
	lane := e.lane
	e.mu.Unlock()
	base := []otelAttr{
		otelStr("loyal.flow.name", "backyard_rwa.worker"),
		otelStr("loyal.flow.stage", kind),
		otelStr("loyal.route.key", productionRouteKey),
	}
	if lane != "" {
		base = append(base, otelStr("loyal.route.lane", lane))
	}
	if code != "" {
		base = append(base, otelStr("loyal.error.code", code))
	}
	at := strconv.FormatInt(e.now().UnixNano(), 10)
	record := otelRecord{
		Attributes: append(base, attrs...), Body: map[string]any{"stringValue": "backyard_rwa." + kind},
		ObservedTime: at, TimeUnixNano: at, SeverityText: severity, SeverityNumber: otelSeverityNumbers[severity],
	}
	select {
	case e.queue <- record:
	default:
		e.dropped.Add(1)
	}
}

// due is the per-kind throttle; callers hold e.mu.
func (e *otelExporter) due(kind string, every time.Duration) bool {
	now := e.now()
	if last, ok := e.last[kind]; ok && now.Sub(last) < every {
		return false
	}
	e.last[kind] = now
	return true
}

var (
	otelURLPattern    = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://\S+`)
	otelSecretPattern = regexp.MustCompile(`(?i)(password|api[-_]?key|token|secret)=\S+`)
)

// otelDetail removes anything URL- or credential-shaped (RPC/DB URLs can
// carry credentials) and bounds the text.
func otelDetail(text string) string {
	text = otelURLPattern.ReplaceAllString(text, "<url>")
	text = otelSecretPattern.ReplaceAllString(text, "$1=<redacted>")
	if len(text) > otelDetailLimit {
		text = text[:otelDetailLimit]
	}
	return text
}

// holdTransactionError is a hold's bounded simulation error, if any.
func holdTransactionError(err error) string {
	var hold *BudgetHold
	if !errors.As(err, &hold) || hold.Details["transactionError"] == "" {
		return ""
	}
	return otelDetail(hold.Details["transactionError"])
}

// holdDetail is a BudgetHold's reason plus its transaction error, if any.
func holdDetail(err error) string {
	var hold *BudgetHold
	if !errors.As(err, &hold) {
		return ""
	}
	if te := holdTransactionError(err); te != "" {
		return otelDetail(hold.Reason + " transactionError=" + te)
	}
	return hold.Reason
}

// otelErrorCode maps a tick error to a closed, grouping-safe code.
func otelErrorCode(err error) string {
	var hold *BudgetHold
	var blocker *RuntimeBlocker
	var pgErr *pgconn.PgError
	switch {
	case errors.As(err, &hold):
		return hold.Reason
	case errors.As(err, &blocker):
		return blocker.Code
	case errors.Is(err, errConfirmedObservationUnavailable):
		return "confirmed_observation_unavailable"
	case errors.Is(err, ErrRouteLeaseLost):
		return "route_lease_lost"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.As(err, &pgErr):
		return "database_" + pgErr.Code
	}
	return "worker_fault"
}

func (e *otelExporter) workerStart() {
	e.emit("INFO", "worker_start", "")
}

// noteCause remembers the latest underlying failure so the latch it causes
// can carry it. ponytail: one slot, last cause wins within 10 min.
func (e *otelExporter) noteCause(detail string) {
	if e == nil || detail == "" {
		return
	}
	e.mu.Lock()
	e.cause, e.causeAt = otelDetail(detail), e.now()
	e.mu.Unlock()
}

// latched reports a newly recorded manual recovery stop.
func (e *otelExporter) latched(reason string) {
	if e == nil {
		return
	}
	e.mu.Lock()
	cause := ""
	if e.cause != "" && e.now().Sub(e.causeAt) < 10*time.Minute {
		cause = e.cause
	}
	e.cause = ""
	e.manual, e.latchReason = true, reason
	e.mu.Unlock()
	if cause != "" {
		e.emit("ERROR", "latched", reason, otelStr("loyal.error.detail", cause))
		return
	}
	e.emit("ERROR", "latched", reason)
}

// noteLatch is the tick's latch read. Only the operator clear-hold command
// (HOLD_CLEARED) lifts a latch, so a latched -> clear edge reports it here,
// from the process that has the export env.
func (e *otelExporter) noteLatch(latched bool, reason string) {
	if e == nil {
		return
	}
	e.mu.Lock()
	cleared, previous := e.manual && !latched, e.latchReason
	e.manual = latched
	if latched {
		e.latchReason = reason
	}
	e.mu.Unlock()
	if cleared {
		e.emit("INFO", "latch_cleared", previous)
	}
}

func (e *otelExporter) noteAction(action Action) {
	if e == nil {
		return
	}
	e.mu.Lock()
	e.action = action
	e.mu.Unlock()
}

// noteSnapshot drives the LTV, withdrawal and heartbeat state from a full
// confirmed observation. Health-hold snapshots carry no valuation and are skipped.
func (e *otelExporter) noteSnapshot(s Snapshot) {
	if e == nil || !s.Fresh || s.ManualReason != "" {
		return
	}
	e.mu.Lock()
	now := e.now()
	e.lane, e.ltvBPS, e.nav = s.RouteLane, s.LTVBPS, s.StrategyNAVRaw
	urgent := s.HasPosition && s.LTVBPS >= 5500 && e.due("ltv_urgent", 10*time.Minute)
	warning := s.HasPosition && s.LTVBPS >= 4500 && s.LTVBPS < 5500 && e.due("ltv_warning", 30*time.Minute)
	waiting := int64(0)
	if s.WithdrawalDemandRaw > s.VoltrIdleRaw {
		if e.withdrawSince.IsZero() {
			e.withdrawSince = now
		}
		if age := now.Sub(e.withdrawSince); age > 30*time.Minute && e.due("withdrawal_waiting", 30*time.Minute) {
			waiting = int64(age.Seconds())
		}
	} else {
		e.withdrawSince = time.Time{}
	}
	e.mu.Unlock()
	if urgent {
		e.emit("ERROR", "ltv_urgent", "ltv_urgent", otelInt("loyal.ltv_bps", s.LTVBPS))
	}
	if warning {
		e.emit("WARN", "ltv_warning", "ltv_warning", otelInt("loyal.ltv_bps", s.LTVBPS))
	}
	if waiting > 0 {
		e.emit("WARN", "withdrawal_waiting", "withdrawal_waiting",
			otelInt("loyal.amount_raw", s.WithdrawalDemandRaw), otelInt("loyal.age_seconds", waiting))
	}
}

// selectorSample tracks the live selector loop; code is empty on success.
func (e *otelExporter) selectorSample(code string) {
	if e == nil {
		return
	}
	e.mu.Lock()
	age := int64(0)
	if code == "" {
		e.selectorSince = time.Time{}
	} else {
		if e.selectorSince.IsZero() {
			e.selectorSince = e.now()
		}
		if since := e.now().Sub(e.selectorSince); since >= 30*time.Minute && e.due("selector_unavailable", 30*time.Minute) {
			age = int64(since.Seconds())
		}
	}
	e.mu.Unlock()
	if age > 0 {
		e.emit("WARN", "selector_unavailable", code, otelInt("loyal.age_seconds", age))
	}
}

// tickResult runs after every tick: heartbeat, repeated-failure counting,
// custody attribution alerts and the fatal exit record.
func (e *otelExporter) tickResult(err error, fatal bool) {
	if e == nil {
		return
	}
	e.mu.Lock()
	heartbeat := e.due("heartbeat", time.Minute)
	ltv, nav, manual := e.ltvBPS, e.nav, e.manual
	action := e.action
	e.action = ""
	custody := err != nil && strings.HasPrefix(otelErrorCode(err), "custody_attribution_unknown") && e.due("custody_unexplained", 10*time.Minute)
	repeated, attempts := false, 0
	if err == nil {
		e.failKey, e.failCount = "", 0
	} else if !fatal {
		if key := string(action) + "|" + otelErrorCode(err); key == e.failKey {
			e.failCount++
		} else {
			e.failKey, e.failCount = key, 1
		}
		attempts = e.failCount
		repeated = attempts == 10 || (attempts > 10 && (attempts-10)%50 == 0)
	}
	e.mu.Unlock()
	if heartbeat {
		e.emit("INFO", "heartbeat", "", otelInt("loyal.ltv_bps", ltv), otelInt("loyal.nav_raw", nav),
			otelBool("loyal.manual", manual), otelInt("loyal.otel.dropped", e.dropped.Load()))
	}
	if err == nil {
		return
	}
	code := otelErrorCode(err)
	if custody {
		e.emit("ERROR", "custody_unexplained", code)
	}
	if repeated {
		e.emit("WARN", "repeated_failure", code, otelStr("loyal.action", string(action)), otelInt("loyal.attempts", int64(attempts)))
	}
	if fatal {
		attrs := []otelAttr{otelStr("loyal.error.detail", otelDetail(err.Error()))}
		if action != "" {
			attrs = append(attrs, otelStr("loyal.action", string(action)))
		}
		if hold := holdDetail(err); hold != "" {
			attrs = append(attrs, otelStr("loyal.hold.reason", hold))
		}
		e.emit("ERROR", "worker_exit", code, attrs...)
	}
}

// operationFailedAfterSend reports a broadcast money operation that ended
// failed or in manual recovery.
func (e *otelExporter) operationFailedAfterSend(action Action, reason, signature string) {
	attrs := []otelAttr{otelStr("loyal.action", string(action))}
	if signature != "" {
		attrs = append(attrs, otelStr("loyal.tx.signature", signature))
	}
	// A failed REPORT_NAV is retried by the next tick (~30/48h in production,
	// mostly blockhash expiry). The nav_reported absence alert pages if the
	// retries stop working, so only fund-moving steps page here.
	severity := "ERROR"
	if action == ReportNAV {
		severity = "INFO"
	}
	e.emit(severity, "operation_failed_after_send", reason, attrs...)
}

// operationReconciled reports one finalized, reconciled money step; a
// reconciled REPORT_NAV additionally reports the NAV it armed.
func (e *otelExporter) operationReconciled(action Action, amountRaw int64, signature string, navRaw int64, navKnown bool) {
	if e == nil {
		return
	}
	e.emit("INFO", "operation_reconciled", "", otelStr("loyal.action", string(action)),
		otelInt("loyal.amount_raw", amountRaw), otelStr("loyal.tx.signature", signature))
	if action == ReportNAV && navKnown {
		e.emit("INFO", "nav_reported", "", otelInt("loyal.nav_raw", navRaw), otelStr("loyal.tx.signature", signature))
	}
}

// otelOperation reads the facts an operation record carries, only when export
// is on. Best-effort and read-only: it runs after the state change committed.
func (d *Database) otelOperation(ctx context.Context, operationID string) (action Action, amountRaw int64, signature, navBase64 string, broadcast, ok bool) {
	if otelLogs == nil || d == nil || d.pool == nil {
		return "", 0, "", "", false, false
	}
	var name string
	err := d.pool.QueryRow(ctx, `SELECT action, COALESCE(transaction_signature,''), broadcast_intent_at IS NOT NULL,
		COALESCE(CASE WHEN expected_effects->'decision'->>'amountRaw' ~ '^[0-9]+$' THEN (expected_effects->'decision'->>'amountRaw')::bigint END, 0),
		COALESCE(expected_effects->'expectedEffects'->'returnData'->>'dataBase64','')
		FROM loyal_yield.multiply_operations WHERE operation_id=$1`, operationID).Scan(&name, &signature, &broadcast, &amountRaw, &navBase64)
	return Action(name), amountRaw, signature, navBase64, broadcast, err == nil
}

// otelFailedAfterSend reports an operation that ended failed or in manual
// recovery, only if it was broadcast; pre-send refusals are not reported.
func (d *Database) otelFailedAfterSend(ctx context.Context, operationID, reason string) {
	if action, _, signature, _, broadcast, ok := d.otelOperation(ctx, operationID); ok && broadcast {
		otelLogs.operationFailedAfterSend(action, reason, signature)
	}
}

// otelReconciled reports a reconciled money step and, for REPORT_NAV, the NAV
// its adaptor return data armed.
func (d *Database) otelReconciled(ctx context.Context, operationID string) {
	action, amountRaw, signature, navBase64, _, ok := d.otelOperation(ctx, operationID)
	if !ok {
		return
	}
	raw, err := base64.StdEncoding.DecodeString(navBase64)
	navKnown := err == nil && len(raw) == 8 && int64(binary.LittleEndian.Uint64(raw)) >= 0
	nav := int64(0)
	if navKnown {
		nav = int64(binary.LittleEndian.Uint64(raw))
	}
	otelLogs.operationReconciled(action, amountRaw, signature, nav, navKnown)
}
