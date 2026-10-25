package webapp

// OTLP/JSON payload builders, porting
// web/apps/web/src/features/observability/otlp.ts. Field order in the wire
// structs follows the TS object literals because the payload bytes are
// compared against the Next exporter's output.

import (
	"strconv"
	"strings"
	"time"
)

type otlpAttributeValue struct {
	BoolValue   *bool    `json:"boolValue,omitempty"`
	DoubleValue *float64 `json:"doubleValue,omitempty"`
	IntValue    *string  `json:"intValue,omitempty"`
	StringValue *string  `json:"stringValue,omitempty"`
}

type otlpAttribute struct {
	Key   string             `json:"key"`
	Value otlpAttributeValue `json:"value"`
}

func obsStringAttribute(key, value string) otlpAttribute {
	return otlpAttribute{Key: key, Value: otlpAttributeValue{StringValue: &value}}
}

func obsIntAttribute(key string, value int64) otlpAttribute {
	encoded := strconv.FormatInt(value, 10)
	return otlpAttribute{Key: key, Value: otlpAttributeValue{IntValue: &encoded}}
}

func obsDoubleAttribute(key string, value float64) otlpAttribute {
	return otlpAttribute{Key: key, Value: otlpAttributeValue{DoubleValue: &value}}
}

func obsBoolAttribute(key string, value bool) otlpAttribute {
	return otlpAttribute{Key: key, Value: otlpAttributeValue{BoolValue: &value}}
}

// obsToUnixNano ports toUnixNano: milliseconds since epoch
// shifted, never a wall-clock nanosecond read.
func obsToUnixNano(timestamp string) string {
	parsed, err := time.Parse(time.RFC3339Nano, timestamp)
	if err != nil {
		return "0"
	}
	return strconv.FormatInt(parsed.UnixMilli()*1_000_000, 10)
}

func obsUnixNanoFromMillis(ms int64) string {
	return strconv.FormatInt(ms*1_000_000, 10)
}

type otlpLogBody struct {
	StringValue string `json:"stringValue"`
}

type otlpScope struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type otlpLogRecord struct {
	Attributes           []otlpAttribute `json:"attributes"`
	TraceID              string          `json:"traceId,omitempty"`
	SpanID               string          `json:"spanId,omitempty"`
	Body                 otlpLogBody     `json:"body"`
	ObservedTimeUnixNano string          `json:"observedTimeUnixNano"`
	SeverityNumber       int             `json:"severityNumber"`
	SeverityText         string          `json:"severityText"`
	TimeUnixNano         string          `json:"timeUnixNano"`
}

type otlpScopeLogs struct {
	LogRecords []otlpLogRecord `json:"logRecords"`
	Scope      otlpScope       `json:"scope"`
}

type otlpResource struct {
	Attributes []otlpAttribute `json:"attributes"`
}

type otlpResourceLogs struct {
	Resource  otlpResource    `json:"resource"`
	ScopeLogs []otlpScopeLogs `json:"scopeLogs"`
}

type otlpLogsPayload struct {
	ResourceLogs []otlpResourceLogs `json:"resourceLogs"`
}

type otlpSpanStatus struct {
	Code int `json:"code"`
}

type otlpSpan struct {
	TraceID           string          `json:"traceId"`
	SpanID            string          `json:"spanId"`
	ParentSpanID      *string         `json:"parentSpanId,omitempty"`
	Name              string          `json:"name"`
	Kind              int             `json:"kind"`
	StartTimeUnixNano string          `json:"startTimeUnixNano"`
	EndTimeUnixNano   string          `json:"endTimeUnixNano"`
	Attributes        []otlpAttribute `json:"attributes"`
	Status            otlpSpanStatus  `json:"status"`
}

type otlpScopeSpans struct {
	Scope otlpScope  `json:"scope"`
	Spans []otlpSpan `json:"spans"`
}

type otlpResourceSpans struct {
	Resource   otlpResource     `json:"resource"`
	ScopeSpans []otlpScopeSpans `json:"scopeSpans"`
}

type otlpTracesPayload struct {
	ResourceSpans []otlpResourceSpans `json:"resourceSpans"`
}

type otlpDataPoint struct {
	AsDouble     float64         `json:"asDouble"`
	Attributes   []otlpAttribute `json:"attributes"`
	TimeUnixNano string          `json:"timeUnixNano"`
}

type otlpGauge struct {
	DataPoints []otlpDataPoint `json:"dataPoints"`
}

type otlpMetric struct {
	Description string    `json:"description"`
	Gauge       otlpGauge `json:"gauge"`
	Name        string    `json:"name"`
	Unit        string    `json:"unit"`
}

type otlpScopeMetrics struct {
	Metrics []otlpMetric `json:"metrics"`
	Scope   otlpScope    `json:"scope"`
}

type otlpResourceMetrics struct {
	Resource     otlpResource       `json:"resource"`
	ScopeMetrics []otlpScopeMetrics `json:"scopeMetrics"`
}

type otlpMetricsPayload struct {
	ResourceMetrics []otlpResourceMetrics `json:"resourceMetrics"`
}

func obsObservedResource(serviceName, release, deploymentEnvironment string) otlpResource {
	return otlpResource{Attributes: []otlpAttribute{
		obsStringAttribute("service.name", serviceName),
		obsStringAttribute("service.version", release),
		obsStringAttribute("deployment.environment.name", deploymentEnvironment),
	}}
}

// obsEventSpanID ports the `eventId.replaceAll("-", "").slice(0, 16)`
// derivation shared by the lifecycle log record and the journey spans.
// slice(0, 16) never panics on short ids, so neither may the port.
func obsEventSpanID(eventID string) string {
	hex := strings.ReplaceAll(eventID, "-", "")
	if len(hex) > 16 {
		return hex[:16]
	}
	return hex
}

func buildOtlpErrorPayload(event *obsNormalizedError) otlpLogsPayload {
	attributes := []otlpAttribute{
		obsStringAttribute("loyal.runtime", event.runtime),
		obsStringAttribute("loyal.operation", event.operation),
		obsStringAttribute("url.path", event.pathname),
		obsStringAttribute("exception.type", event.exceptionName),
		obsStringAttribute("exception.message", event.exceptionMessage),
	}
	if event.exceptionStack != "" {
		attributes = append(attributes, obsStringAttribute("exception.stacktrace", event.exceptionStack))
	}
	if event.method != "" {
		attributes = append(attributes, obsStringAttribute("http.request.method", event.method))
	}
	if event.clientBuildID != "" {
		attributes = append(attributes, obsStringAttribute("loyal.client.build_id", event.clientBuildID))
	}
	if event.pageSessionID != "" {
		attributes = append(attributes, obsStringAttribute("loyal.page_session.id", event.pageSessionID))
	}
	if event.deviceID != "" {
		attributes = append(attributes, obsStringAttribute("loyal.device.id", event.deviceID))
	}
	if event.devicePlatform != "" {
		attributes = append(attributes, obsStringAttribute("loyal.device.platform", event.devicePlatform))
	}
	if diagnostics := event.browserDiagnostics; diagnostics != nil {
		attributes = append(attributes,
			obsStringAttribute("loyal.chunk.url", diagnostics.chunkURL),
			obsBoolAttribute("network.online", diagnostics.networkOnline),
		)
		if diagnostics.recoveryAction != "" {
			attributes = append(attributes, obsStringAttribute("loyal.chunk.recovery_action", diagnostics.recoveryAction))
		}
		if diagnostics.connectionEffectiveType != "" {
			attributes = append(attributes, obsStringAttribute("network.connection.effective_type", diagnostics.connectionEffectiveType))
		}
		if diagnostics.connectionRTTMS != nil {
			attributes = append(attributes, obsIntAttribute("network.connection.rtt_ms", int64(*diagnostics.connectionRTTMS)))
		}
		if diagnostics.resourceResponseStatus != nil {
			attributes = append(attributes, obsIntAttribute("loyal.resource.response_status", int64(*diagnostics.resourceResponseStatus)))
		}
		if diagnostics.resourceTransferSize != nil {
			attributes = append(attributes, obsIntAttribute("loyal.resource.transfer_size", int64(*diagnostics.resourceTransferSize)))
		}
		if diagnostics.resourceDurationMS != nil {
			attributes = append(attributes, obsDoubleAttribute("loyal.resource.duration_ms", *diagnostics.resourceDurationMS))
		}
	}

	timeUnixNano := obsToUnixNano(event.timestamp)
	severityNumber, severityText := 17, "ERROR"
	scopeName := "loyal.frontend.errors"
	if event.serviceName == "loyal-mobile" {
		scopeName = "loyal.mobile.errors"
	}
	return otlpLogsPayload{ResourceLogs: []otlpResourceLogs{{
		Resource: obsObservedResource(event.serviceName, event.release, event.deploymentEnvironment),
		ScopeLogs: []otlpScopeLogs{{
			LogRecords: []otlpLogRecord{{
				Attributes:           attributes,
				Body:                 otlpLogBody{StringValue: event.exceptionMessage},
				ObservedTimeUnixNano: timeUnixNano,
				SeverityNumber:       severityNumber,
				SeverityText:         severityText,
				TimeUnixNano:         timeUnixNano,
			}},
			Scope: otlpScope{Name: scopeName, Version: "1"},
		}},
	}}}
}

func buildOtlpLifecycleLogRecord(event *obsNormalizedLifecycle) otlpLogRecord {
	attributes := []otlpAttribute{
		obsStringAttribute("loyal.event.kind", "flow_lifecycle"),
		obsStringAttribute("loyal.flow.id", event.flowID),
		obsStringAttribute("loyal.flow.name", event.flowName),
		obsStringAttribute("loyal.flow.variant", event.flowVariant),
		obsStringAttribute("loyal.flow.stage", event.stage),
		obsStringAttribute("loyal.flow.outcome", event.outcome),
		obsStringAttribute("loyal.flow.source", event.source),
		obsIntAttribute("loyal.duration_ms", event.durationMS),
		obsIntAttribute("loyal.elapsed_ms", event.elapsedMS),
		obsStringAttribute("url.path", event.pathname),
		obsStringAttribute("loyal.runtime", event.runtime),
	}
	optionalStrings := [][2]string{
		{"loyal.journey.id", event.journeyID},
		{"loyal.traffic.kind", event.trafficKind},
		{"loyal.user.id", event.userID},
		{"loyal.rum.session_id", event.rumSessionID},
		{"loyal.event.id", event.eventID},
		{"loyal.client.build_id", event.clientBuildID},
		{"loyal.page_session.id", event.pageSessionID},
		{"loyal.device.id", event.deviceID},
		{"loyal.device.platform", event.devicePlatform},
		{"loyal.wallet.address", event.walletAddress},
		{"loyal.wallet.provider", event.walletProvider},
		{"loyal.error.category", lifecycleFailureCategory(event)},
		{"loyal.error.code", event.errorCode},
		{"loyal.error.detail", event.errorDetail},
		{"loyal.error.message", event.errorMessage},
		{"loyal.client.platform", event.clientPlatform},
		{"loyal.execute_now.state", event.executeNowState},
		{"loyal.chain.state", event.chainState},
		{"loyal.persistence.state", event.persistenceState},
		{"loyal.transaction.version", event.transactionVersion},
		{"loyal.policy.mode", event.policyMode},
		{"loyal.auth.proof_kind", event.authProofKind},
		{"loyal.execution.mode", event.executionMode},
		{"loyal.provisioning.outcome", event.provisioningOutcome},
		{"loyal.scheduled_slot.id", event.scheduledSlotID},
	}
	for _, pair := range optionalStrings {
		if pair[1] != "" {
			attributes = append(attributes, obsStringAttribute(pair[0], pair[1]))
		}
	}
	optionalInts := []struct {
		key   string
		value *int64
	}{
		{"loyal.attempt.number", event.attemptNumber},
		{"loyal.retry.count", event.retryCount},
	}
	for _, item := range optionalInts {
		if item.value != nil {
			attributes = append(attributes, obsIntAttribute(item.key, *item.value))
		}
	}
	flowVersion := int64(1)
	if event.journeyID != "" {
		flowVersion = 2
	}
	attributes = append(attributes, obsIntAttribute("loyal.flow.version", flowVersion))
	if event.httpStatus != nil {
		attributes = append(attributes, obsIntAttribute("http.response.status_code", *event.httpStatus))
	}
	optionalInts2 := []struct {
		key   string
		value *int64
	}{
		{"loyal.stage.index", event.stageIndex},
		{"loyal.stage.count", event.stageCount},
		{"loyal.instruction.count", event.instructionCount},
	}
	for _, item := range optionalInts2 {
		if item.value != nil {
			attributes = append(attributes, obsIntAttribute(item.key, *item.value))
		}
	}
	optionalBools := []struct {
		key   string
		value *bool
	}{
		{"loyal.transaction.lookup_table_used", event.lookupTableUsed},
		{"loyal.setup.required", event.setupRequired},
		{"loyal.review.bypassed", event.reviewBypassed},
		{"loyal.autodeposit_close.required", event.autodepositCloseRequired},
		{"loyal.cleanup.required", event.cleanupRequired},
		{"loyal.recovery.required", event.recoveryRequired},
	}
	for _, item := range optionalBools {
		if item.value != nil {
			attributes = append(attributes, obsBoolAttribute(item.key, *item.value))
		}
	}

	timeUnixNano := obsToUnixNano(event.timestamp)
	isError := isAlertableLifecycleEvent(event)
	severityNumber, severityText := 9, "INFO"
	if isError {
		severityNumber, severityText = 17, "ERROR"
	}
	record := otlpLogRecord{
		Attributes:           attributes,
		Body:                 otlpLogBody{StringValue: event.flowName + "." + event.stage + "." + event.outcome},
		ObservedTimeUnixNano: timeUnixNano,
		SeverityNumber:       severityNumber,
		SeverityText:         severityText,
		TimeUnixNano:         timeUnixNano,
	}
	if event.traceID != "" && event.spanID != "" {
		record.TraceID = event.traceID
		if event.eventID != "" {
			record.SpanID = obsEventSpanID(event.eventID)
		} else {
			record.SpanID = event.spanID
		}
	}
	return record
}

func buildOtlpLifecyclePayload(event *obsNormalizedLifecycle) otlpLogsPayload {
	scopeName := "loyal.frontend.lifecycle"
	if event.serviceName == "loyal-mobile" {
		scopeName = "loyal.mobile.lifecycle"
	}
	return otlpLogsPayload{ResourceLogs: []otlpResourceLogs{{
		Resource: obsObservedResource(event.serviceName, event.release, event.deploymentEnvironment),
		ScopeLogs: []otlpScopeLogs{{
			LogRecords: []otlpLogRecord{buildOtlpLifecycleLogRecord(event)},
			Scope:      otlpScope{Name: scopeName, Version: "1"},
		}},
	}}}
}

func buildOtlpJourneyTracePayload(event *obsNormalizedLifecycle) *otlpTracesPayload {
	if event.traceID == "" || event.spanID == "" || event.eventID == "" {
		return nil
	}
	logs := buildOtlpLifecyclePayload(event)
	resource := logs.ResourceLogs[0].Resource
	attributes := logs.ResourceLogs[0].ScopeLogs[0].LogRecords[0].Attributes
	end, err := time.Parse(time.RFC3339Nano, event.timestamp)
	if err != nil {
		return nil
	}
	endMS := end.UnixMilli()
	parentSpanID := event.spanID
	forcedExternal := *event
	forcedExternal.trafficKind = "external"
	statusCode := 0
	if isAlertableLifecycleEvent(&forcedExternal) {
		statusCode = 2
	}
	span := otlpSpan{
		TraceID:           event.traceID,
		SpanID:            obsEventSpanID(event.eventID),
		ParentSpanID:      &parentSpanID,
		Name:              event.flowName + "." + event.stage,
		Kind:              1,
		StartTimeUnixNano: obsUnixNanoFromMillis(endMS - event.durationMS),
		EndTimeUnixNano:   obsToUnixNano(event.timestamp),
		Attributes:        attributes,
		Status:            otlpSpanStatus{Code: statusCode},
	}
	spans := []otlpSpan{span}
	switch event.outcome {
	case "completed", "failed", "cancelled":
		terminal := span
		terminal.SpanID = event.spanID
		if event.parentSpanID != "" {
			terminal.ParentSpanID = &event.parentSpanID
		} else {
			terminal.ParentSpanID = nil
		}
		terminal.Name = event.flowName + "." + event.source
		if event.source == "next_api" {
			terminal.Kind = 2
		} else {
			terminal.Kind = 1
		}
		terminal.StartTimeUnixNano = obsUnixNanoFromMillis(endMS - event.elapsedMS)
		spans = append(spans, terminal)
	}
	return &otlpTracesPayload{ResourceSpans: []otlpResourceSpans{{
		Resource: resource,
		ScopeSpans: []otlpScopeSpans{{
			Scope: otlpScope{Name: "loyal.frontend.journeys", Version: "2"},
			Spans: spans,
		}},
	}}}
}

func buildOtlpLoadingMetricPayload(event *obsNormalizedMetric) otlpMetricsPayload {
	attributes := []otlpAttribute{
		obsStringAttribute("loyal.operation", event.operation),
		obsStringAttribute("loyal.phase", event.phase),
		obsStringAttribute("loyal.outcome", event.outcome),
		obsStringAttribute("url.path", event.pathname),
	}
	if event.flowID != "" {
		attributes = append(attributes, obsStringAttribute("loyal.flow.id", event.flowID))
	}
	if event.pageSessionID != "" {
		attributes = append(attributes, obsStringAttribute("loyal.page_session.id", event.pageSessionID))
	}
	if event.appSessionID != "" {
		attributes = append(attributes, obsStringAttribute("loyal.app_session.id", event.appSessionID))
	}
	if event.deviceID != "" {
		attributes = append(attributes, obsStringAttribute("loyal.device.id", event.deviceID))
	}
	if event.platform != "" {
		attributes = append(attributes, obsStringAttribute("loyal.platform", event.platform))
	}
	if event.dependency != "" {
		attributes = append(attributes, obsStringAttribute("loyal.dependency", event.dependency))
	}
	if event.presentation != "" {
		attributes = append(attributes, obsStringAttribute("loyal.presentation", event.presentation))
	}
	if event.requestCount != nil {
		attributes = append(attributes, obsIntAttribute("loyal.request.count", *event.requestCount))
	}

	description := "Browser loading latency from a user-visible Loyal flow boundary."
	scopeName := "loyal.frontend.loading"
	if event.serviceName == "loyal-mobile" {
		description = "Mobile loading latency from a user-visible Loyal flow boundary."
		scopeName = "loyal.mobile.loading"
	}
	return otlpMetricsPayload{ResourceMetrics: []otlpResourceMetrics{{
		Resource: obsObservedResource(event.serviceName, event.release, event.deploymentEnvironment),
		ScopeMetrics: []otlpScopeMetrics{{
			Metrics: []otlpMetric{{
				Description: description,
				Gauge: otlpGauge{DataPoints: []otlpDataPoint{{
					AsDouble:     event.durationMS,
					Attributes:   attributes,
					TimeUnixNano: obsToUnixNano(event.timestamp),
				}}},
				Name: event.metricName,
				Unit: "ms",
			}},
			Scope: otlpScope{Name: scopeName, Version: "1"},
		}},
	}}}
}
