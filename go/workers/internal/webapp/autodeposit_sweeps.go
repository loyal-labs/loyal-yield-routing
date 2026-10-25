package webapp

// POST and GET /api/smart-accounts/yield-optimization/autodeposit/sweeps/execute,
// ported from web/apps/web/src/app/api/smart-accounts/yield-optimization/autodeposit/sweeps/execute/route.ts.
// POST flips the wallet's oldest eligible scheduled slot to "requested" so the
// sweep worker executes it now (execute-now); GET reports one slot's progress
// for the client fallback when the realtime stream is down. Identity is the
// Principal; the mobile twin's walletAddress query param never reaches here.

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/earnprep"
)

const autodepositSweepsExecutePath = "/api/smart-accounts/yield-optimization/autodeposit/sweeps/execute"

// EARN_REALTIME_EVENT_TYPES.autodeposit and EARN_REALTIME_SCHEMA_VERSION
// (features/earn-realtime/types.ts).
const autodepositRealtimeEventType = "earn.autodeposit.execution.changed"
const autodepositRealtimeSchemaVersion = 1

// AutodepositSweepsExecuteRequest is the documented body: the optional slot id
// arrives as a decimal string, as the web client sends it.
type AutodepositSweepsExecuteRequest struct {
	SlotID *string `json:"slotId,omitempty"`
}

// AutodepositSweepRequestResult mirrors serializeRequestResult
// (route.ts:52-63); every id and amount renders as a decimal string.
type AutodepositSweepRequestResult struct {
	AcceleratedAmountRaw string `json:"acceleratedAmountRaw"`
	AcceleratedLotCount  int64  `json:"acceleratedLotCount"`
	EligibleAfter        string `json:"eligibleAfter"`
	SlotID               string `json:"slotId"`
	Status               string `json:"status"`
	TargetID             string `json:"targetId"`
}

// AutodepositSweepsExecuteResponse is NextResponse.json({ status,
// sweepRequest, target }) — key order matches the route's object literal.
type AutodepositSweepsExecuteResponse struct {
	Status       string                         `json:"status"`
	SweepRequest AutodepositSweepRequestResult  `json:"sweepRequest"`
	Target       *AutodepositSetupConfirmTarget `json:"target"`
}

// AutodepositSweepsProgressResponse is the GET body (route.ts:160-170). eventId
// and failureCode are undefined in TS when the column is null, so they are
// omitted here too; the remaining key order matches the route's literal.
type AutodepositSweepsProgressResponse struct {
	EventID         *string `json:"eventId,omitempty"`
	EventType       string  `json:"eventType"`
	FailureCode     *string `json:"failureCode,omitempty"`
	OccurredAt      string  `json:"occurredAt"`
	ScheduledSlotID string  `json:"scheduledSlotId"`
	SchemaVersion   int     `json:"schemaVersion"`
	Scope           string  `json:"scope"`
	State           string  `json:"state"`
}

// resolveAutodepositTargetStatus ports resolveEarnAutodepositStatus
// (earn-autodeposit-repository.server.ts:232): active flag and lifecycle
// collapse to active, paused or pending.
func resolveAutodepositTargetStatus(target autodepositTargetRow) string {
	switch target.LifecycleStatus {
	case "active":
		if target.Active {
			return "active"
		}
		return "paused"
	case "paused_missing_position":
		return "paused"
	}
	return "pending"
}

// findCurrentAutodepositTarget ports findCurrentEarnAutodepositState's target
// read (:635): the wallet's most relevant non-closed target for the Earn
// vault, preferring the active row, newest last-seen slot first.
func (h earnRoutes) findCurrentAutodepositTarget(ctx context.Context, settings, wallet string) (*autodepositTargetRow, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	row := h.YieldDB.QueryRow(ctx,
		`SELECT `+autodepositTargetColumns+` FROM loyal_yield.balance_sweep_targets
WHERE wallet = $1 AND settings = $2 AND vault_index = $3 AND chain_status <> 'closed'
ORDER BY CASE WHEN desired_active = TRUE AND chain_status = 'active' THEN 0 ELSE 1 END,
  last_seen_slot DESC
LIMIT 1`, wallet, settings, int16(earnprep.EarnVaultIndex))
	target, err := scanAutodepositTarget(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &target, nil
}

// autodepositSlotIDPattern is /^\d+$/ — both parsers accept decimal strings
// only, whatever their length.
var autodepositSlotIDPattern = regexp.MustCompile(`^\d+$`)

// parseAutodepositSlotID ports the shared slot-id shape check
// (route.ts:65-87): anything but a non-empty decimal string fails with the
// route's message.
func parseAutodepositSlotID(value string) (int64, error) {
	if !autodepositSlotIDPattern.MatchString(value) {
		return 0, errors.New("Invalid Autodeposit scheduled slot.")
	}
	slotID, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, errors.New("Invalid Autodeposit scheduled slot.")
	}
	return slotID, nil
}

// parseAutodepositOptionalSlotID ports parseOptionalSlotId (route.ts:65-79):
// only an application/json body carries a slot; a missing key, null, an empty
// string or a non-object body means "oldest eligible slot".
func parseAutodepositOptionalSlotID(raw []byte) (*int64, error) {
	parsed, err := parseJSONBody(raw)
	if err != nil {
		return nil, errors.New("Invalid Autodeposit scheduled slot.")
	}
	object, ok := parsed.(map[string]any)
	if !ok {
		return nil, nil
	}
	value, present := object["slotId"]
	if !present {
		return nil, nil
	}
	switch slotID := value.(type) {
	case nil:
		return nil, nil
	case string:
		if slotID == "" {
			return nil, nil
		}
		parsed, err := parseAutodepositSlotID(slotID)
		if err != nil {
			return nil, err
		}
		return &parsed, nil
	}
	return nil, errors.New("Invalid Autodeposit scheduled slot.")
}

// autodepositSweepRequestResult is ImmediateEarnAutodepositScheduledSweepRequestResult.
type autodepositSweepRequestResult struct {
	AcceleratedAmountRaw uint64
	AcceleratedLotCount  int64
	EligibleAfter        time.Time
	SlotID               int64
	Status               string
	TargetID             int64
}

// requestImmediateAutodepositSweep ports
// requestImmediateEarnAutodepositScheduledSweep (:903-987): one statement
// locks the oldest eligible slot FOR UPDATE, flips it to "requested" and
// re-labels its open lots, returning the accelerated totals. A second call
// finds nothing left in ('scheduled','failed','released') with open lots and
// returns nil — the caller renders 409 no_scheduled_sweeps. The eligible-after
// bump to max(now, delegation-ready) is what moves the worker's pick-up forward.
func (h earnRoutes) requestImmediateAutodepositSweep(ctx context.Context, target autodepositTargetRow, slotID *int64) (*autodepositSweepRequestResult, error) {
	now := time.Now()
	eligibleAfter := now
	if ready := resolveEarnAutodepositDelegationReadyAt(target.StartTimestamp); ready != nil && ready.After(now) {
		eligibleAfter = *ready
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	tx, err := h.YieldDB.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var result autodepositSweepRequestResult
	err = tx.QueryRow(ctx, `WITH selected_slot AS (
  SELECT slot.id
  FROM loyal_yield.balance_sweep_scheduled_slots AS slot
  WHERE slot.target_id = $1
    AND ($2::bigint IS NULL OR slot.id = $2)
    AND slot.status IN ('scheduled', 'failed', 'released')
    AND EXISTS (
      SELECT 1
      FROM loyal_yield.balance_sweep_surplus_lots AS lot
      WHERE lot.scheduled_slot_id = slot.id
        AND lot.status = 'open'
        AND lot.remaining_amount_raw > 0
    )
  ORDER BY slot.eligible_after ASC, slot.id ASC
  LIMIT 1
  FOR UPDATE
),
updated_slot AS (
  UPDATE loyal_yield.balance_sweep_scheduled_slots AS slot
  SET status = 'requested',
      request_source = 'web_execute_now',
      requested_at = $3,
      eligible_after = $4,
      last_error = NULL,
      updated_at = $3
  WHERE slot.id IN (SELECT id FROM selected_slot)
  RETURNING slot.id, slot.eligible_after, slot.status::text AS status
),
updated_lots AS (
  UPDATE loyal_yield.balance_sweep_surplus_lots AS lot
  SET confidence = 'user_requested',
      eligible_after = $4,
      reason = CASE
        WHEN lot.reason LIKE 'user requested immediate autodeposit sweep;%'
          THEN lot.reason
        ELSE concat('user requested immediate autodeposit sweep; ', lot.reason)
      END,
      updated_at = $3
  WHERE lot.scheduled_slot_id IN (SELECT id FROM updated_slot)
    AND lot.status = 'open'
    AND lot.remaining_amount_raw > 0
  RETURNING lot.remaining_amount_raw
)
SELECT
  updated_slot.id,
  updated_slot.eligible_after,
  updated_slot.status,
  COALESCE(SUM(updated_lots.remaining_amount_raw), 0)::bigint,
  COUNT(updated_lots.remaining_amount_raw)::bigint
FROM updated_slot
LEFT JOIN updated_lots ON true
GROUP BY updated_slot.id, updated_slot.eligible_after, updated_slot.status`,
		target.ID, slotID, now, eligibleAfter).
		Scan(&result.SlotID, &result.EligibleAfter, &result.Status, &result.AcceleratedAmountRaw, &result.AcceleratedLotCount)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	result.TargetID = target.ID
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &result, nil
}

type autodepositSweepsExecuteInput struct {
	SessionInput
	ContentType string `header:"Content-Type"`
	Raw         rawJSON
}

// autodepositSweepProgressRecord is EarnAutodepositScheduledSweepProgressRecord.
type autodepositSweepProgressRecord struct {
	SlotID                int64
	Status                string
	CompletedAt           *time.Time
	CompletionFailureCode *string
	EventID               *int64
	OccurredAt            time.Time
}

// findAutodepositSweepProgress ports
// findEarnAutodepositScheduledSweepProgress (:848-901): the slot's execution
// completion columns plus the newest autodeposit realtime event for the slot;
// occurredAt falls back to the slot's updated_at when no event exists.
func (h earnRoutes) findAutodepositSweepProgress(ctx context.Context, targetID, slotID int64) (*autodepositSweepProgressRecord, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var progress autodepositSweepProgressRecord
	err := h.YieldDB.QueryRow(ctx, `SELECT
  slot.id,
  slot.status::text,
  execution.completed_at,
  execution.completion_failure_code,
  latest_event.id,
  COALESCE(latest_event.created_at, slot.updated_at)
FROM loyal_yield.balance_sweep_scheduled_slots AS slot
LEFT JOIN loyal_yield.balance_sweep_executions AS execution
  ON execution.id = slot.execution_id
LEFT JOIN LATERAL (
  SELECT event.id, event.created_at
  FROM loyal_yield.realtime_events AS event
  WHERE event.scheduled_slot_id = slot.id
    AND event.scope = 'autodeposit'
  ORDER BY event.id DESC
  LIMIT 1
) AS latest_event ON true
WHERE slot.target_id = $1
  AND slot.id = $2
LIMIT 1`, targetID, slotID).
		Scan(&progress.SlotID, &progress.Status, &progress.CompletedAt,
			&progress.CompletionFailureCode, &progress.EventID, &progress.OccurredAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &progress, nil
}

// resolveAutodepositProgressState ports resolveProgressState (route.ts:89-112):
// the execution outcome wins, then the executed status, then the live slot
// statuses; an unrecognized status has no renderable state.
func resolveAutodepositProgressState(progress autodepositSweepProgressRecord) string {
	if progress.CompletedAt != nil {
		return "completed"
	}
	if progress.CompletionFailureCode != nil {
		return "failed"
	}
	if progress.Status == "executed" {
		return "pull_confirmed"
	}
	switch progress.Status {
	case "scheduled", "requested", "selected", "failed", "released", "canceled":
		return progress.Status
	}
	return ""
}

type autodepositSweepsProgressInput struct {
	SessionInput
	SlotID string `query:"slotId" required:"false"`
}

// autodepositSweepsProgressOutput wraps the body, same as the execute output.
type autodepositSweepsProgressOutput struct {
	Body AutodepositSweepsProgressResponse
}

func (h earnRoutes) autodepositSweepsProgress(ctx context.Context, in *autodepositSweepsProgressInput) (*autodepositSweepsProgressOutput, error) {
	// route.ts:115-118.
	account, err := h.account(ctx, in.SessionInput)
	if err != nil {
		return nil, earnUnavailable("load Earn account", err, "account_unavailable", "Earn account is unavailable.")
	}

	// route.ts:120-131.
	slotID, err := parseAutodepositSlotID(in.SlotID)
	if err != nil {
		return nil, earnError(http.StatusBadRequest, "invalid_request", err.Error())
	}

	// route.ts:133-145.
	target, err := h.findCurrentAutodepositTarget(ctx, account.Settings.String(), account.Wallet.String())
	if err != nil {
		return nil, earnError(http.StatusInternalServerError, "progress_read_failed",
			"Failed to read Autodeposit sweep progress.")
	}
	if target == nil {
		return nil, earnError(http.StatusNotFound, "autodeposit_not_found",
			"No Earn Autodeposit policy is available for this wallet.")
	}

	// route.ts:147-158.
	progress, err := h.findAutodepositSweepProgress(ctx, target.ID, slotID)
	if err != nil {
		return nil, earnError(http.StatusInternalServerError, "progress_read_failed",
			"Failed to read Autodeposit sweep progress.")
	}
	if progress == nil {
		return nil, earnError(http.StatusNotFound, "scheduled_sweep_not_found",
			"No matching Autodeposit scheduled sweep is available.")
	}
	state := resolveAutodepositProgressState(*progress)
	if state == "" {
		return nil, earnError(http.StatusNotFound, "scheduled_sweep_not_found",
			"No matching Autodeposit scheduled sweep is available.")
	}

	response := &autodepositSweepsProgressOutput{Body: AutodepositSweepsProgressResponse{
		EventType:       autodepositRealtimeEventType,
		OccurredAt:      isoMilliseconds(progress.OccurredAt),
		ScheduledSlotID: strconv.FormatInt(progress.SlotID, 10),
		SchemaVersion:   autodepositRealtimeSchemaVersion,
		Scope:           "autodeposit",
		State:           state,
	}}
	if progress.EventID != nil {
		eventID := strconv.FormatInt(*progress.EventID, 10)
		response.Body.EventID = &eventID
	}
	if progress.CompletionFailureCode != nil {
		response.Body.FailureCode = progress.CompletionFailureCode
	}
	return response, nil
}

func (h earnRoutes) registerAutodepositSweeps(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "post-autodeposit-sweeps-execute",
		Method:      http.MethodPost,
		Path:        autodepositSweepsExecutePath,
		Summary:     "Request immediate execution of a pending Earn autodeposit sweep",
		Responses:   errorResponses[routeError](api, 400, 401, 404, 409, 500, 503),
	}, h.autodepositSweepsExecute)
	documentJSONBody[AutodepositSweepsExecuteRequest](api, autodepositSweepsExecutePath)
	huma.Register(api, huma.Operation{
		OperationID: "get-autodeposit-sweeps-execute",
		Method:      http.MethodGet,
		Path:        autodepositSweepsExecutePath,
		Summary:     "Read the progress of one Earn autodeposit scheduled sweep",
		Responses:   errorResponses[routeError](api, 400, 401, 404, 500, 503),
	}, h.autodepositSweepsProgress)
}

// autodepositSweepsExecuteOutput wraps the body: huma reads a top-level Status
// field as the response status code, so the route's "status" key must live one
// level down.
type autodepositSweepsExecuteOutput struct {
	Body AutodepositSweepsExecuteResponse
}

func (h earnRoutes) autodepositSweepsExecute(ctx context.Context, in *autodepositSweepsExecuteInput) (*autodepositSweepsExecuteOutput, error) {
	// route.ts:206-210.
	account, err := h.account(ctx, in.SessionInput)
	if err != nil {
		return nil, earnUnavailable("load Earn account", err, "account_unavailable", "Earn account is unavailable.")
	}

	// route.ts:213-227 — only an application/json body carries a slot id.
	var slotID *int64
	if strings.Contains(strings.ToLower(in.ContentType), "application/json") {
		slotID, err = parseAutodepositOptionalSlotID(in.Raw.data)
		if err != nil {
			return nil, earnError(http.StatusBadRequest, "invalid_request", err.Error())
		}
	}

	// route.ts:229-241.
	target, err := h.findCurrentAutodepositTarget(ctx, account.Settings.String(), account.Wallet.String())
	if err != nil {
		return nil, earnError(http.StatusInternalServerError, "request_failed", err.Error())
	}
	if target == nil {
		return nil, earnError(http.StatusNotFound, "autodeposit_not_found",
			"No Earn Autodeposit policy is available for this wallet.")
	}

	// route.ts:243-249.
	if resolveAutodepositTargetStatus(*target) != "active" {
		return nil, earnError(http.StatusConflict, "autodeposit_not_active",
			"Earn Autodeposit must be active before a scheduled sweep can be executed now.")
	}

	// route.ts:257-281 — a withdrawn-away position has no route policy left and
	// its sweeps would strand on "Executing…" forever; lookup errors fail open.
	pair, gateErr := h.activePolicyPair(ctx, account)
	if gateErr != nil {
		slog.WarnContext(ctx, "[autodeposit-sweeps-execute] earn position gate skipped",
			"errorMessage", gateErr.Error(), "walletAddress", account.Wallet.String())
	} else if pair == nil {
		return nil, earnError(http.StatusConflict, "earn_position_required",
			"Autodeposit needs an active Earn account to deposit into. Make a deposit first.")
	}

	// route.ts:283-294 — nil result means nothing was eligible.
	result, err := h.requestImmediateAutodepositSweep(ctx, *target, slotID)
	if err != nil {
		return nil, earnError(http.StatusInternalServerError, "request_failed", err.Error())
	}
	if result == nil {
		return nil, earnError(http.StatusConflict, "no_scheduled_sweeps",
			"There are no pending scheduled Autodeposit sweeps to execute now.")
	}

	return &autodepositSweepsExecuteOutput{Body: AutodepositSweepsExecuteResponse{
		Status: "requested",
		SweepRequest: AutodepositSweepRequestResult{
			AcceleratedAmountRaw: strconv.FormatUint(result.AcceleratedAmountRaw, 10),
			AcceleratedLotCount:  result.AcceleratedLotCount,
			EligibleAfter:        isoMilliseconds(result.EligibleAfter),
			SlotID:               strconv.FormatInt(result.SlotID, 10),
			Status:               result.Status,
			TargetID:             strconv.FormatInt(result.TargetID, 10),
		},
		Target: serializeAutodepositTarget(*target),
	}}, nil
}
