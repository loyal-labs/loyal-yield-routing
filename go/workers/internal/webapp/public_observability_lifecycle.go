package webapp

import (
	"math"
	"strings"
	"time"
)

var obsLifecycleFlows = []string{
	"auth.sign_in",
	"auth.session_restore",
	"auth.signer_restore",
	"auth.authenticated_action",
	"auth.smart_account_provisioning",
	"earn.deposit",
	"earn.withdrawal",
	"earn.autodeposit.configuration",
	"earn.autodeposit.execute_now",
	"earn.autoswap.configuration",
	"wallet.swap",
	"earn_max.invite",
	"earn_max.vault",
}

var obsLifecycleOutcomes = []string{
	"started", "observed", "retrying", "timed_out", "completed", "failed", "cancelled",
}

var obsLifecycleSources = []string{
	"browser", "next_api", "sse", "fallback", "mobile_app",
}

var obsLifecycleVariants = map[string][]string{
	"auth.authenticated_action":       {"overview"},
	"auth.session_restore":            {"initial", "return", "refresh"},
	"auth.signer_restore":             {"return"},
	"auth.sign_in":                    {"interactive", "auto_reauth", "seed_vault", "wallet_adapter", "import_wallet", "new_wallet"},
	"auth.smart_account_provisioning": {"wallet_onboarding"},
	"earn.deposit":                    {"initial", "resumed", "top_up"},
	"earn.withdrawal":                 {"partial", "full"},
	"earn.autodeposit.configuration":  {"setup", "floor_update", "pause", "resume", "close"},
	"earn.autodeposit.execute_now":    {"execute_now"},
	"earn.autoswap.configuration":     {"setup", "pause", "resume", "delete"},
	"wallet.swap":                     {"wallet_adapter", "smart_account"},
	"earn_max.invite":                 {"redeem"},
	"earn_max.vault":                  {"deposit", "request_withdrawal", "claim"},
}

var obsLifecycleStages = map[string][]string{
	"auth.authenticated_action":       {"intent", "backend_session_valid", "action_completed"},
	"auth.session_restore":            {"intent", "session_read", "backend_session_valid"},
	"auth.signer_restore":             {"intent", "backend_session_valid", "wallet_select", "wallet_connect", "signer_ready"},
	"auth.sign_in":                    {"intent", "privy_authenticated", "backend_session_valid", "signer_ready", "wallet_select", "wallet_connect", "challenge", "wallet_approval", "completion", "session_refresh", "ui_commit"},
	"auth.smart_account_provisioning": {"proof_verify", "user_resolve", "smart_account_lookup", "smart_account_reconcile", "smart_account_reserve", "sponsorship_submit", "sponsorship_finalize", "signer_verify", "completion_persist", "session_issue"},
	"earn.deposit":                    {"intent", "prepare", "review", "policy", "policy_finalize", "preflight", "wallet_submit_confirm", "slot_resolve", "backend_confirm", "ui_commit", "position_visible"},
	"earn.withdrawal":                 {"position_visible", "intent", "prepare", "autodeposit_close", "wallet_submit_confirm", "slot_resolve", "backend_confirm", "full_exit_verify", "cleanup_prepare", "cleanup_wallet_submit_confirm", "cleanup_backend_confirm", "cleanup", "ui_commit"},
	"earn.autodeposit.configuration":  {"intent", "prepare", "wallet_approval", "create_policy", "create_recurring_delegation", "slot_resolve", "backend_confirm", "bootstrap", "ui_commit"},
	"earn.autodeposit.execute_now":    {"intent", "request", "state_observed", "ui_commit"},
	"earn.autoswap.configuration":     {"intent", "prepare", "wallet_approval", "create_policy", "slot_resolve", "backend_confirm", "ui_commit"},
	"wallet.swap":                     {"balance_visible", "intent", "quote_refresh", "build", "wallet_submit_confirm", "ui_commit"},
	"earn_max.invite":                 {"redeem"},
	"earn_max.vault":                  {"intent", "prepare", "wallet_submit_confirm", "ui_commit", "position_visible"},
}

var (
	obsLifecycleErrorCodes = []string{
		"unexpected_error",
		"invalid_request",
		"unauthenticated",
		"captcha_verification_failed",
		"wallet_selection_timeout",
		"wallet_connection_timeout",
		"wallet_connection_failed",
		"wallet_authorization_expired",
		"wallet_account_mismatch",
		"wallet_signing_failed",
		"wallet_signing_unsupported",
		"wallet_rejected",
		"invalid_wallet_origin",
		"invalid_wallet_proof_kind",
		"invalid_wallet_signature",
		"invalid_wallet_signin",
		"wallet_auth_completion_in_progress",
		"smart_account_sponsor_not_configured",
		"smart_account_provisioning_failed",
		"smart_account_reservation_conflict",
		"smart_account_signer_mismatch",
		"smart_account_principal_mismatch",
		"state_not_ready",
		"policy_signer_missing",
		"invalid_source",
		"insufficient_usdc",
		"insufficient_native_sol",
		"earn_max_liquidity_unavailable",
		"instruction_fetch_failed",
		"instruction_validation_failed",
		"transaction_too_large",
		"cluster_mismatch",
		"wallet_unavailable",
		"wallet_mismatch",
		"simulation_failed",
		"earn_withdraw_underfilled",
		"slippage_exceeded",
		"send_failed",
		"chain_confirmation_failed",
		"slot_resolution_failed",
		"backend_confirmation_failed",
		"record_failed",
		"full_exit_verification_retryable",
		"autodeposit_not_found",
		"autodeposit_not_active",
		"earn_position_required",
		"no_scheduled_sweeps",
		"request_failed",
		"progress_read_failed",
		"execution_failed",
		"kamino_top_up_failed",
		"autodeposit_target_closed",
		"unconfirmed_signature",
		"metadata_mismatch",
		"principal_mismatch",
		"slot_mismatch",
		"artifact_missing",
		"token_approval_missing",
		"realtime_unresolved",
	}
	obsLifecycleErrorDetails = []string{
		"mwa_unspecified",
		"mwa_wallet_not_found",
		"mwa_session_timeout",
		"mwa_association_timeout",
		"mwa_session_closed",
		"mwa_end_session_failed",
		"network_unreachable",
		"request_timeout",
		"kamino_upstream_unavailable",
		"rpc_request_failed",
	}
	obsExecuteNowStates      = []string{"requested", "selected", "pull_confirmed", "completed", "failed", "released", "canceled"}
	obsProvisioningOutcomes  = []string{"existing_ready", "delegated_root_signer", "reconciled_ready", "sponsored_existing_record", "sponsored_new_record", "retried_failed_record", "reservation_conflict", "signer_mismatch", "sponsor_unconfigured", "sponsorship_failed"}
	obsProofKinds            = []string{"siws", "message", "transaction"}
	obsExecutionModes        = []string{"batch", "sequential", "single"}
	obsPolicyModes           = []string{"create", "reuse"}
	obsChainStates           = []string{"not_submitted", "submitted", "confirmed", "failed"}
	obsPersistenceStates     = []string{"not_started", "recorded", "failed"}
	obsTransactionVersions   = []string{"legacy", "v0"}
	obsLifecyclePlatforms    = []string{"desktop", "mobile"}
	obsLifecycleProviders    = []string{"backpack", "brave_wallet", "coinbase_wallet", "exodus", "glow", "jupiter", "ledger", "loyal", "magic_eden", "metamask", "nightly", "okx_wallet", "other", "phantom", "solflare", "trust"}
	obsLifecycleRuntimeKinds = []string{"browser", "node", "mobile"}
)

type obsNormalizedLifecycle struct {
	authProofKind            string
	attemptNumber            *int64
	autodepositCloseRequired *bool
	chainState               string
	clientBuildID            string
	clientPlatform           string
	cleanupRequired          *bool
	deploymentEnvironment    string
	deviceID                 string
	devicePlatform           string
	flowID                   string
	flowName                 string
	flowVariant              string
	httpStatus               *int64
	instructionCount         *int64
	journeyID                string
	lookupTableUsed          *bool
	outcome                  string
	pageSessionID            string
	parentSpanID             string
	pathname                 string
	persistenceState         string
	policyMode               string
	provisioningOutcome      string
	recoveryRequired         *bool
	release                  string
	retryCount               *int64
	rumSessionID             string
	runtime                  string
	scheduledSlotID          string
	serviceName              string
	setupRequired            *bool
	spanID                   string
	source                   string
	stage                    string
	stageCount               *int64
	stageIndex               *int64
	durationMS               int64
	elapsedMS                int64
	errorCode                string
	errorDetail              string
	errorMessage             string
	eventID                  string
	executeNowState          string
	executionMode            string
	trafficKind              string
	transactionVersion       string
	traceID                  string
	userID                   string
	timestamp                string
	walletAddress            string
	walletProvider           string
	reviewBypassed           *bool
}

func mapExecuteNowState(state string) (outcome string, stage string) {
	switch state {
	case "completed":
		return "completed", "state_observed"
	case "failed", "released":
		return "failed", "state_observed"
	case "canceled":
		return "cancelled", "state_observed"
	}
	return "observed", "state_observed"
}

func lifecycleFailureCategory(event *obsNormalizedLifecycle) string {
	switch {
	case event.outcome == "timed_out" || event.errorCode == "unconfirmed_signature":
		return "timeout"
	case event.errorCode == "wallet_rejected":
		return "user_rejection"
	case func() bool {
		_, ok := obsStringIn(event.errorCode, []string{
			"unauthenticated",
			"invalid_request",
			"metadata_mismatch",
			"principal_mismatch",
			"cluster_mismatch",
			"autodeposit_not_active",
			"autodeposit_not_found",
			"no_scheduled_sweeps",
			"insufficient_native_sol",
			"insufficient_usdc",
			"invalid_source",
			"state_not_ready",
			"wallet_account_mismatch",
			"wallet_mismatch",
			"earn_position_required",
		})
		return ok
	}():
		return "validation"
	case event.outcome == "cancelled":
		return "user_cancellation"
	case event.outcome == "failed" || event.outcome == "retrying":
		return "technical"
	}
	return "none"
}

var obsExpectedLocalFailures = map[string]bool{
	"insufficient_native_sol":      true,
	"wallet_account_mismatch":      true,
	"wallet_authorization_expired": true,
	"wallet_connection_failed":     true,
	"wallet_connection_timeout":    true,
	"wallet_signing_failed":        true,
	"wallet_unavailable":           true,
}

func isAlertableLifecycleEvent(event *obsNormalizedLifecycle) bool {
	if event.trafficKind == "synthetic" {
		return false
	}
	if event.recoveryRequired != nil && *event.recoveryRequired {
		return true
	}
	if event.outcome != "failed" {
		return false
	}
	if event.persistenceState == "failed" || event.chainState == "failed" ||
		(event.httpStatus != nil && *event.httpStatus >= 500) ||
		event.errorDetail == "kamino_upstream_unavailable" ||
		event.errorDetail == "rpc_request_failed" {
		return true
	}
	if obsExpectedLocalFailures[event.errorCode] {
		return false
	}
	if event.errorCode != "request_failed" {
		return true
	}
	if event.errorDetail == "network_unreachable" || event.errorDetail == "request_timeout" {
		return false
	}
	if event.httpStatus != nil && *event.httpStatus >= 400 && *event.httpStatus < 500 {
		return false
	}
	// Old clients do not carry a cause token. Keep unknown statusless failures
	// alertable until their cause is known rather than silently hiding a bug.
	return true
}

func obsTrimmedToken(value string) string {
	var token strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(value)) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			token.WriteRune(r)
		} else {
			token.WriteRune('_')
		}
	}
	return token.String()
}

// obsOptionalEnumField ports assertOptionalEnum: a present key must hold one
// of the allowed tokens (JSON null counts as present and fails, like TS
// `value !== undefined`).
func obsOptionalEnumField(record map[string]any, key string, allowed []string) (string, error) {
	raw, present := record[key]
	if !present {
		return "", nil
	}
	value, ok := obsStringIn(raw, allowed)
	if !ok {
		return "", errObservabilityEnvelope
	}
	return value, nil
}

func obsOptionalBoolField(record map[string]any, key string) (*bool, error) {
	raw, present := record[key]
	if !present {
		return nil, nil
	}
	value, ok := raw.(bool)
	if !ok {
		return nil, errObservabilityEnvelope
	}
	return &value, nil
}

func obsOptionalIntField(record map[string]any, key string, min, max int64) (*int64, error) {
	raw, present := record[key]
	if !present {
		return nil, nil
	}
	value, ok := raw.(float64)
	if !ok || value != math.Trunc(value) || value < float64(min) || value > float64(max) {
		return nil, errObservabilityEnvelope
	}
	converted := int64(value)
	return &converted, nil
}

func obsRequiredIntField(record map[string]any, key string, min, max int64) (int64, error) {
	value, err := obsOptionalIntField(record, key, min, max)
	if err != nil {
		return 0, err
	}
	if value == nil {
		return 0, errObservabilityEnvelope
	}
	return *value, nil
}

var obsBrowserLifecycleKeys = []string{
	"traceId", "spanId", "parentSpanId", "rumSessionId", "journeyId",
	"attemptNumber", "eventId", "clientBuildId", "pageSessionId", "retryCount",
	"authProofKind", "autodepositCloseRequired", "chainState", "cleanupRequired",
	"clientPlatform", "durationMs", "elapsedMs", "errorCode", "errorDetail",
	"errorMessage", "executeNowState", "executionMode", "flowId", "flowName",
	"flowVariant", "httpStatus", "instructionCount", "lookupTableUsed",
	"outcome", "pathname", "persistenceState", "policyMode",
	"provisioningOutcome", "recoveryRequired", "reviewBypassed", "runtime",
	"scheduledSlotId", "setupRequired", "source", "stage", "stageCount",
	"stageIndex", "timestamp", "transactionVersion", "walletProvider",
}

func parseBrowserLifecycleEnvelope(raw []byte, now time.Time) (obsNormalizedLifecycle, error) {
	record, err := obsObjectBody(raw)
	if err != nil {
		return obsNormalizedLifecycle{}, err
	}
	return parseBrowserLifecycleRecord(record, now)
}

func parseBrowserLifecycleRecord(record map[string]any, now time.Time) (obsNormalizedLifecycle, error) {
	if err := obsAllowedKeys(record, obsBrowserLifecycleKeys...); err != nil {
		return obsNormalizedLifecycle{}, err
	}

	traceID, _ := record["traceId"].(string)
	spanID, _ := record["spanId"].(string)
	parentSpanID, _ := record["parentSpanId"].(string)
	if _, present := record["traceId"]; present && !isTraceID(traceID) {
		return obsNormalizedLifecycle{}, errObservabilityEnvelope
	}
	if _, present := record["spanId"]; present && !isSpanID(spanID) {
		return obsNormalizedLifecycle{}, errObservabilityEnvelope
	}
	parentPresent := false
	if _, present := record["parentSpanId"]; present {
		if !isSpanID(parentSpanID) {
			return obsNormalizedLifecycle{}, errObservabilityEnvelope
		}
		parentPresent = true
	}
	_, tracePresent := record["traceId"]
	_, spanPresent := record["spanId"]
	if tracePresent != spanPresent || parentPresent && !tracePresent {
		return obsNormalizedLifecycle{}, errObservabilityEnvelope
	}

	for _, key := range []string{"journeyId", "eventId", "pageSessionId", "rumSessionId"} {
		if raw, present := record[key]; present && !isCanonicalUUIDV4(raw) {
			return obsNormalizedLifecycle{}, errObservabilityEnvelope
		}
	}
	retryCount, err := obsOptionalIntField(record, "retryCount", 0, 1000)
	if err != nil {
		return obsNormalizedLifecycle{}, err
	}
	attemptNumber, err := obsOptionalIntField(record, "attemptNumber", 1, 1000)
	if err != nil {
		return obsNormalizedLifecycle{}, err
	}
	_, journeyPresent := record["journeyId"]
	if journeyPresent != (attemptNumber != nil) {
		return obsNormalizedLifecycle{}, errObservabilityEnvelope
	}
	clientBuildID, _ := record["clientBuildId"].(string)
	if _, present := record["clientBuildId"]; present && !obsClientBuildID.MatchString(clientBuildID) {
		return obsNormalizedLifecycle{}, errObservabilityEnvelope
	}

	flowID, _ := record["flowId"].(string)
	flowName, ok := obsStringIn(record["flowName"], obsLifecycleFlows)
	if !isCanonicalUUIDV4(flowID) || !ok {
		return obsNormalizedLifecycle{}, errObservabilityEnvelope
	}
	flowVariant, ok := obsStringIn(record["flowVariant"], obsLifecycleVariants[flowName])
	if !ok {
		return obsNormalizedLifecycle{}, errObservabilityEnvelope
	}
	stage, ok := obsStringIn(record["stage"], obsLifecycleStages[flowName])
	if !ok {
		return obsNormalizedLifecycle{}, errObservabilityEnvelope
	}
	outcome, ok := obsStringIn(record["outcome"], obsLifecycleOutcomes)
	if !ok {
		return obsNormalizedLifecycle{}, errObservabilityEnvelope
	}
	source, ok := obsStringIn(record["source"], obsLifecycleSources)
	if !ok {
		return obsNormalizedLifecycle{}, errObservabilityEnvelope
	}
	runtime, ok := obsStringIn(record["runtime"], obsLifecycleRuntimeKinds)
	if !ok {
		return obsNormalizedLifecycle{}, errObservabilityEnvelope
	}
	durationMS, err := obsRequiredIntField(record, "durationMs", 0, 900_000)
	if err != nil {
		return obsNormalizedLifecycle{}, err
	}
	elapsedMS, err := obsRequiredIntField(record, "elapsedMs", 0, 86_400_000)
	if err != nil {
		return obsNormalizedLifecycle{}, err
	}

	rawTimestamp, ok := record["timestamp"].(string)
	if !ok {
		return obsNormalizedLifecycle{}, errObservabilityEnvelope
	}
	timestamp, err := obsTimestampInWindow(rawTimestamp, now)
	if err != nil {
		return obsNormalizedLifecycle{}, err
	}
	rawPathname, ok := record["pathname"].(string)
	if !ok {
		return obsNormalizedLifecycle{}, errObservabilityEnvelope
	}
	pathname := normalizeTelemetryPathname(rawPathname)
	if pathname == "" {
		return obsNormalizedLifecycle{}, errObservabilityEnvelope
	}

	errorCode, err := obsOptionalEnumField(record, "errorCode", obsLifecycleErrorCodes)
	if err != nil {
		return obsNormalizedLifecycle{}, err
	}
	// A detail is dropped, never rejected: it only annotates the event, so an
	// unknown token must not cost the event it describes.
	var errorDetail string
	if raw, present := record["errorDetail"]; present {
		errorDetail, _ = obsStringIn(raw, obsLifecycleErrorDetails)
	}
	// Sanitized on every parse — the client tracker and the ingest route share
	// this function, so a message is redacted before it leaves the browser and
	// again before it reaches the exporter.
	var errorMessage string
	if outcome == "failed" || outcome == "cancelled" {
		if raw, isString := record["errorMessage"].(string); isString {
			errorMessage = sanitizeTelemetryText(raw, obsMaxLifecycleMessageLen)
		}
	}
	clientPlatform, _ := obsStringIn(record["clientPlatform"], obsLifecyclePlatforms)
	walletProvider, _ := obsStringIn(record["walletProvider"], obsLifecycleProviders)
	executeNowState, err := obsOptionalEnumField(record, "executeNowState", obsExecuteNowStates)
	if err != nil {
		return obsNormalizedLifecycle{}, err
	}
	authProofKind, err := obsOptionalEnumField(record, "authProofKind", obsProofKinds)
	if err != nil {
		return obsNormalizedLifecycle{}, err
	}
	executionMode, err := obsOptionalEnumField(record, "executionMode", obsExecutionModes)
	if err != nil {
		return obsNormalizedLifecycle{}, err
	}
	policyMode, err := obsOptionalEnumField(record, "policyMode", obsPolicyModes)
	if err != nil {
		return obsNormalizedLifecycle{}, err
	}
	chainState, err := obsOptionalEnumField(record, "chainState", obsChainStates)
	if err != nil {
		return obsNormalizedLifecycle{}, err
	}
	persistenceState, err := obsOptionalEnumField(record, "persistenceState", obsPersistenceStates)
	if err != nil {
		return obsNormalizedLifecycle{}, err
	}
	transactionVersion, err := obsOptionalEnumField(record, "transactionVersion", obsTransactionVersions)
	if err != nil {
		return obsNormalizedLifecycle{}, err
	}
	provisioningOutcome, err := obsOptionalEnumField(record, "provisioningOutcome", obsProvisioningOutcomes)
	if err != nil {
		return obsNormalizedLifecycle{}, err
	}
	autodepositCloseRequired, err := obsOptionalBoolField(record, "autodepositCloseRequired")
	if err != nil {
		return obsNormalizedLifecycle{}, err
	}
	cleanupRequired, err := obsOptionalBoolField(record, "cleanupRequired")
	if err != nil {
		return obsNormalizedLifecycle{}, err
	}
	lookupTableUsed, err := obsOptionalBoolField(record, "lookupTableUsed")
	if err != nil {
		return obsNormalizedLifecycle{}, err
	}
	recoveryRequired, err := obsOptionalBoolField(record, "recoveryRequired")
	if err != nil {
		return obsNormalizedLifecycle{}, err
	}
	reviewBypassed, err := obsOptionalBoolField(record, "reviewBypassed")
	if err != nil {
		return obsNormalizedLifecycle{}, err
	}
	setupRequired, err := obsOptionalBoolField(record, "setupRequired")
	if err != nil {
		return obsNormalizedLifecycle{}, err
	}
	httpStatus, err := obsOptionalIntField(record, "httpStatus", 100, 599)
	if err != nil {
		return obsNormalizedLifecycle{}, err
	}
	stageIndex, err := obsOptionalIntField(record, "stageIndex", 0, 15)
	if err != nil {
		return obsNormalizedLifecycle{}, err
	}
	stageCount, err := obsOptionalIntField(record, "stageCount", 1, 16)
	if err != nil {
		return obsNormalizedLifecycle{}, err
	}
	instructionCount, err := obsOptionalIntField(record, "instructionCount", 0, 64)
	if err != nil {
		return obsNormalizedLifecycle{}, err
	}
	scheduledSlotID, _ := record["scheduledSlotId"].(string)
	if _, present := record["scheduledSlotId"]; present && !obsScheduledSlot.MatchString(scheduledSlotID) {
		return obsNormalizedLifecycle{}, errObservabilityEnvelope
	}

	if authProofKind != "" && flowName != "auth.sign_in" {
		return obsNormalizedLifecycle{}, errObservabilityEnvelope
	}
	if provisioningOutcome != "" && flowName != "auth.smart_account_provisioning" {
		return obsNormalizedLifecycle{}, errObservabilityEnvelope
	}
	if (executeNowState != "" || scheduledSlotID != "") && flowName != "earn.autodeposit.execute_now" {
		return obsNormalizedLifecycle{}, errObservabilityEnvelope
	}
	isMoneyFlow := flowName == "earn.deposit" || flowName == "earn.withdrawal"
	isTransactionFlow := isMoneyFlow ||
		flowName == "earn.autodeposit.configuration" ||
		flowName == "earn.autoswap.configuration" ||
		flowName == "earn_max.vault" ||
		flowName == "wallet.swap"
	if (chainState != "" || persistenceState != "" || executionMode != "" ||
		transactionVersion != "" || lookupTableUsed != nil || instructionCount != nil) &&
		!isTransactionFlow {
		return obsNormalizedLifecycle{}, errObservabilityEnvelope
	}
	if (policyMode != "" || setupRequired != nil || reviewBypassed != nil) &&
		flowName != "earn.deposit" {
		return obsNormalizedLifecycle{}, errObservabilityEnvelope
	}
	if (autodepositCloseRequired != nil || cleanupRequired != nil) &&
		flowName != "earn.withdrawal" {
		return obsNormalizedLifecycle{}, errObservabilityEnvelope
	}
	if recoveryRequired != nil && *recoveryRequired {
		stageAllowed := stage == "backend_confirm" ||
			(flowName == "earn.withdrawal" && stage == "cleanup_backend_confirm")
		errorCodeAllowed := errorCode == "backend_confirmation_failed" ||
			errorCode == "record_failed"
		if !isMoneyFlow || !stageAllowed || outcome != "observed" ||
			chainState != "confirmed" || persistenceState != "failed" || !errorCodeAllowed {
			return obsNormalizedLifecycle{}, errObservabilityEnvelope
		}
	}
	if flowName == "earn.autodeposit.execute_now" && executeNowState != "" {
		mappedOutcome, mappedStage := mapExecuteNowState(executeNowState)
		if stage == "request" {
			if executeNowState != "requested" || outcome != "observed" {
				return obsNormalizedLifecycle{}, errObservabilityEnvelope
			}
		} else if stage != mappedStage || outcome != mappedOutcome {
			return obsNormalizedLifecycle{}, errObservabilityEnvelope
		}
	}

	return obsNormalizedLifecycle{
		authProofKind:            authProofKind,
		attemptNumber:            attemptNumber,
		autodepositCloseRequired: autodepositCloseRequired,
		chainState:               chainState,
		clientBuildID:            clientBuildID,
		clientPlatform:           clientPlatform,
		cleanupRequired:          cleanupRequired,
		durationMS:               durationMS,
		elapsedMS:                elapsedMS,
		errorCode:                errorCode,
		errorDetail:              errorDetail,
		errorMessage:             errorMessage,
		eventID:                  obsStringField(record, "eventId"),
		executeNowState:          executeNowState,
		executionMode:            executionMode,
		flowID:                   flowID,
		flowName:                 flowName,
		flowVariant:              flowVariant,
		httpStatus:               httpStatus,
		instructionCount:         instructionCount,
		journeyID:                obsStringField(record, "journeyId"),
		lookupTableUsed:          lookupTableUsed,
		outcome:                  outcome,
		pageSessionID:            obsStringField(record, "pageSessionId"),
		parentSpanID:             parentSpanID,
		pathname:                 pathname,
		persistenceState:         persistenceState,
		policyMode:               policyMode,
		provisioningOutcome:      provisioningOutcome,
		recoveryRequired:         recoveryRequired,
		reviewBypassed:           reviewBypassed,
		retryCount:               retryCount,
		rumSessionID:             obsStringField(record, "rumSessionId"),
		runtime:                  runtime,
		scheduledSlotID:          scheduledSlotID,
		setupRequired:            setupRequired,
		spanID:                   spanID,
		source:                   source,
		stage:                    stage,
		stageCount:               stageCount,
		stageIndex:               stageIndex,
		timestamp:                timestamp.UTC().Format("2006-01-02T15:04:05.000Z07:00"),
		traceID:                  traceID,
		transactionVersion:       transactionVersion,
		walletProvider:           walletProvider,
	}, nil
}

func obsStringField(record map[string]any, key string) string {
	s, _ := record[key].(string)
	return s
}

func parseMobileLifecycleEnvelope(raw []byte, now time.Time) (obsNormalizedLifecycle, error) {
	record, err := obsObjectBody(raw)
	if err != nil {
		return obsNormalizedLifecycle{}, err
	}
	environment, err := obsReadResourceValue(record, "environment", obsMaxEnvironmentLength)
	if err != nil {
		return obsNormalizedLifecycle{}, err
	}
	release, err := obsReadResourceValue(record, "release", obsMaxReleaseLength)
	if err != nil {
		return obsNormalizedLifecycle{}, err
	}
	walletAddress, _ := record["walletAddress"].(string)
	if _, present := record["walletAddress"]; present {
		if !obsWalletAddPattern.MatchString(walletAddress) {
			return obsNormalizedLifecycle{}, errObservabilityEnvelope
		}
	}
	deviceID, _ := record["deviceId"].(string)
	if _, present := record["deviceId"]; present && !isCanonicalUUIDV4(deviceID) {
		return obsNormalizedLifecycle{}, errObservabilityEnvelope
	}
	devicePlatform, _ := record["devicePlatform"].(string)
	if _, present := record["devicePlatform"]; present {
		if _, ok := obsStringIn(devicePlatform, obsMobileDevicePlatforms); !ok {
			return obsNormalizedLifecycle{}, errObservabilityEnvelope
		}
	}
	runtime, _ := record["runtime"].(string)
	if runtime != "mobile" {
		return obsNormalizedLifecycle{}, errObservabilityEnvelope
	}
	delete(record, "deviceId")
	delete(record, "devicePlatform")
	delete(record, "environment")
	delete(record, "release")
	delete(record, "walletAddress")
	envelope, err := parseBrowserLifecycleRecord(record, now)
	if err != nil {
		return obsNormalizedLifecycle{}, err
	}
	envelope.deploymentEnvironment = environment
	envelope.release = release
	envelope.walletAddress = walletAddress
	envelope.deviceID = deviceID
	envelope.devicePlatform = devicePlatform
	return envelope, nil
}
