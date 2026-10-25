package webapp

import (
	"encoding/json"
	"net/url"
	"regexp"
	"strings"
	"time"
)

var obsBrowserErrorOperations = []string{
	"browser.window.error",
	"browser.unhandled_rejection",
	"react.error_boundary",
	"react.global_error_boundary",
	"earn.deposit.confirmation",
	"earn.deposit.execute",
	"earn.deposit_batch.execute",
	"earn.deposit_policy_stage.execute",
	"earn.policy_setup.execute",
	"earn.withdrawal.execute",
	"earn.cleanup.execute",
	"earn.autodeposit_setup.execute",
	"earn.autodeposit_floor_update.execute",
	"earn.autodeposit_toggle.execute",
	"earn.autodeposit_close.execute",
	"earn.autoswap_setup.execute",
	"earn.autoswap_toggle.execute",
	"earn.autoswap_delete.execute",
	"vault.transfer.execute",
	"vault.swap.execute",
}

// The ambient window listeners observe wallet extensions too; their
// extension-only crashes never reach the error alert.
var obsAmbientErrorOperations = map[string]bool{
	"browser.window.error":        true,
	"browser.unhandled_rejection": true,
}

var obsMobileErrorOperations = []string{
	"mobile.global_error",
	"mobile.fatal_error",
	"mobile.unhandled_rejection",
}

var obsMobileDevicePlatforms = []string{"android", "ios"}

type obsChunkDiagnostics struct {
	chunkURL                string
	connectionEffectiveType string
	connectionRTTMS         *float64
	networkOnline           bool
	recoveryAction          string
	resourceDurationMS      *float64
	resourceResponseStatus  *float64
	resourceTransferSize    *float64
}

type obsErrorEnvelope struct {
	clientBuildID  string
	diagnostics    *obsChunkDiagnostics
	deviceID       string
	devicePlatform string
	environment    string
	message        string
	name           string
	operation      string
	pageSessionID  string
	pathname       string
	release        string
	stack          string
	timestamp      string
}

func obsRequiredString(record map[string]any, key string) (string, error) {
	value, ok := record[key].(string)
	if !ok || value == "" || utf16Len(value) > obsMaxRawFieldLength {
		return "", errObservabilityEnvelope
	}
	return value, nil
}

func obsOptionalStack(record map[string]any) (string, error) {
	raw, present := record["stack"]
	if !present || raw == nil {
		return "", nil
	}
	stack, ok := raw.(string)
	if !ok || utf16Len(stack) > obsMaxRawFieldLength {
		return "", errObservabilityEnvelope
	}
	return stack, nil
}

// parseIsoTimestamp enforces the TS `new Date(ms).toISOString() === raw`
// round-trip: exactly RFC3339 with millisecond precision in UTC.
func parseIsoTimestamp(raw string) (time.Time, bool) {
	parsed, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}, false
	}
	normalized := parsed.UTC().Format("2006-01-02T15:04:05.000Z07:00")
	if normalized != raw {
		return time.Time{}, false
	}
	return parsed, true
}

func obsTimestampInWindow(raw string, now time.Time) (time.Time, error) {
	parsed, ok := parseIsoTimestamp(raw)
	if !ok {
		return time.Time{}, errObservabilityEnvelope
	}
	ms := parsed.UnixMilli()
	if ms < now.UnixMilli()-obsMaxEventAgeMS || ms > now.UnixMilli()+obsMaxEventClockSkewMS {
		return time.Time{}, errObservabilityEnvelope
	}
	return parsed, nil
}

func obsCommonErrorFields(record map[string]any, now time.Time) (obsErrorEnvelope, error) {
	var envelope obsErrorEnvelope
	rawTimestamp, err := obsRequiredString(record, "timestamp")
	if err != nil {
		return envelope, err
	}
	timestamp, err := obsTimestampInWindow(rawTimestamp, now)
	if err != nil {
		return envelope, err
	}
	rawPathname, err := obsRequiredString(record, "pathname")
	if err != nil {
		return envelope, err
	}
	pathname := normalizeTelemetryPathname(rawPathname)
	if pathname == "" {
		return envelope, errObservabilityEnvelope
	}
	stack, err := obsOptionalStack(record)
	if err != nil {
		return envelope, err
	}
	rawName, err := obsRequiredString(record, "name")
	if err != nil {
		return envelope, err
	}
	rawMessage, err := obsRequiredString(record, "message")
	if err != nil {
		return envelope, err
	}
	envelope.timestamp = timestamp.UTC().Format("2006-01-02T15:04:05.000Z07:00")
	envelope.pathname = pathname
	envelope.stack = sanitizeTelemetryText(stack, obsMaxErrorStackLength)
	envelope.name = sanitizeTelemetryText(rawName, obsMaxErrorNameLength)
	envelope.message = sanitizeTelemetryText(rawMessage, obsMaxErrorMessageLength)
	return envelope, nil
}

func obsObjectBody(raw []byte) (map[string]any, error) {
	var parsed any
	if json.Unmarshal(raw, &parsed) != nil {
		return nil, errObservabilityEnvelope
	}
	record, ok := parsed.(map[string]any)
	if !ok {
		return nil, errObservabilityEnvelope
	}
	return record, nil
}

func obsAllowedKeys(record map[string]any, allowed ...string) error {
	for key := range record {
		found := false
		for _, candidate := range allowed {
			if key == candidate {
				found = true
				break
			}
		}
		if !found {
			return errObservabilityEnvelope
		}
	}
	return nil
}

func obsStringIn(value any, allowed []string) (string, bool) {
	s, ok := value.(string)
	if !ok {
		return "", false
	}
	for _, candidate := range allowed {
		if s == candidate {
			return s, true
		}
	}
	return "", false
}

func obsReadResourceValue(record map[string]any, key string, maxLength int) (string, error) {
	raw, err := obsRequiredString(record, key)
	if err != nil {
		return "", err
	}
	normalized := normalizeResourceValue(raw, maxLength)
	if normalized == "" {
		return "", errObservabilityEnvelope
	}
	return normalized, nil
}

func parseBrowserErrorEnvelope(raw []byte, expectedChunkOrigin string, now time.Time) (obsErrorEnvelope, error) {
	record, err := obsObjectBody(raw)
	if err != nil {
		return obsErrorEnvelope{}, err
	}
	if err := obsAllowedKeys(record, "clientBuildId", "diagnostics", "message", "name",
		"operation", "pageSessionId", "pathname", "stack", "timestamp"); err != nil {
		return obsErrorEnvelope{}, err
	}
	operation, ok := obsStringIn(record["operation"], obsBrowserErrorOperations)
	if !ok {
		return obsErrorEnvelope{}, errObservabilityEnvelope
	}
	envelope, err := obsCommonErrorFields(record, now)
	if err != nil {
		return obsErrorEnvelope{}, err
	}
	envelope.operation = operation
	// Chunk context is diagnostic enrichment, never the report itself: a
	// partial match is dropped and the error still exports
	buildID, _ := record["clientBuildId"].(string)
	pageSession, _ := record["pageSessionId"].(string)
	if obsClientBuildID.MatchString(buildID) && obsPageSessionID.MatchString(pageSession) {
		envelope.clientBuildID = buildID
		envelope.pageSessionID = pageSession
		if rawDiagnostics, present := record["diagnostics"]; present {
			diagnostics := normalizeChunkDiagnostics(rawDiagnostics)
			if diagnostics != nil && obsChunkURLFromOrigin(diagnostics.chunkURL, expectedChunkOrigin) {
				envelope.diagnostics = diagnostics
			}
		}
	}
	return envelope, nil
}

func parseMobileErrorEnvelope(raw []byte, now time.Time) (obsErrorEnvelope, error) {
	record, err := obsObjectBody(raw)
	if err != nil {
		return obsErrorEnvelope{}, err
	}
	if err := obsAllowedKeys(record, "deviceId", "devicePlatform", "environment", "message",
		"name", "operation", "pathname", "release", "stack", "timestamp"); err != nil {
		return obsErrorEnvelope{}, err
	}
	operation, ok := obsStringIn(record["operation"], obsMobileErrorOperations)
	if !ok {
		return obsErrorEnvelope{}, errObservabilityEnvelope
	}
	envelope, err := obsCommonErrorFields(record, now)
	if err != nil {
		return obsErrorEnvelope{}, err
	}
	if rawDeviceID, present := record["deviceId"]; present && !isCanonicalUUIDV4(rawDeviceID) {
		return obsErrorEnvelope{}, errObservabilityEnvelope
	}
	if rawPlatform, present := record["devicePlatform"]; present {
		platform, ok := obsStringIn(rawPlatform, obsMobileDevicePlatforms)
		if !ok {
			return obsErrorEnvelope{}, errObservabilityEnvelope
		}
		envelope.devicePlatform = platform
	}
	if rawDeviceID, present := record["deviceId"]; present {
		envelope.deviceID, _ = rawDeviceID.(string)
	}
	environment, err := obsReadResourceValue(record, "environment", obsMaxEnvironmentLength)
	if err != nil {
		return obsErrorEnvelope{}, err
	}
	release, err := obsReadResourceValue(record, "release", obsMaxReleaseLength)
	if err != nil {
		return obsErrorEnvelope{}, err
	}
	envelope.environment = environment
	envelope.release = release
	envelope.operation = operation
	return envelope, nil
}

// isThirdPartyExtensionError ports error-contract.ts:273: an ambient report
// whose stack holds only extension frames never saw Loyal code run.
var (
	obsExtensionFramePattern  = regexp.MustCompile(`(?i)\b(?:chrome|moz|safari-web|safari|ms-browser)-extension:\/\//`)
	obsFirstPartyFramePattern = regexp.MustCompile(`\bhttps?:\/\//`)
	obsCapWidgetPattern       = regexp.MustCompile(`\b_ensureSize\b`)
)

func isThirdPartyExtensionError(operation, stack string) bool {
	if stack == "" || !obsAmbientErrorOperations[operation] {
		return false
	}
	return obsExtensionFramePattern.MatchString(stack) && !obsFirstPartyFramePattern.MatchString(stack)
}

// isCapWidgetInternalError ports error-contract.ts:294: the Cap captcha widget
// tearing down mid-solve is not a Loyal failure.
func isCapWidgetInternalError(operation, message string) bool {
	return obsAmbientErrorOperations[operation] && obsCapWidgetPattern.MatchString(message)
}

type obsNormalizedError struct {
	browserDiagnostics    *obsChunkDiagnostics
	clientBuildID         string
	deploymentEnvironment string
	deviceID              string
	devicePlatform        string
	exceptionMessage      string
	exceptionName         string
	exceptionStack        string
	method                string
	operation             string
	pageSessionID         string
	pathname              string
	release               string
	runtime               string
	serviceName           string
	severity              string // "" means ERROR
	timestamp             string
}

func createNormalizedBrowserErrorEvent(envelope obsErrorEnvelope, deploymentEnvironment, serverRelease string) obsNormalizedError {
	return obsNormalizedError{
		browserDiagnostics:    envelope.diagnostics,
		clientBuildID:         envelope.clientBuildID,
		deploymentEnvironment: deploymentEnvironment,
		exceptionMessage:      envelope.message,
		exceptionName:         envelope.name,
		exceptionStack:        envelope.stack,
		operation:             envelope.operation,
		pageSessionID:         envelope.pageSessionID,
		pathname:              envelope.pathname,
		release:               serverRelease,
		runtime:               "browser",
		serviceName:           "loyal-frontend",
		timestamp:             envelope.timestamp,
	}
}

func normalizedMobileErrorEvent(envelope obsErrorEnvelope) obsNormalizedError {
	return obsNormalizedError{
		deploymentEnvironment: envelope.environment,
		deviceID:              envelope.deviceID,
		devicePlatform:        envelope.devicePlatform,
		exceptionMessage:      envelope.message,
		exceptionName:         envelope.name,
		exceptionStack:        envelope.stack,
		operation:             envelope.operation,
		pathname:              envelope.pathname,
		release:               envelope.release,
		runtime:               "mobile",
		serviceName:           "loyal-mobile",
		timestamp:             envelope.timestamp,
	}
}

func normalizeChunkDiagnostics(value any) *obsChunkDiagnostics {
	record, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	for key := range record {
		switch key {
		case "chunkUrl", "connectionEffectiveType", "connectionRttMs", "networkOnline",
			"recoveryAction", "resourceDurationMs", "resourceResponseStatus", "resourceTransferSize":
		default:
			return nil
		}
	}
	chunkURL, _ := record["chunkUrl"].(string)
	chunkURL = normalizeChunkURL(chunkURL)
	online, ok := record["networkOnline"].(bool)
	if chunkURL == "" || !ok {
		return nil
	}
	diagnostics := &obsChunkDiagnostics{chunkURL: chunkURL, networkOnline: online}
	if raw, present := record["connectionEffectiveType"]; present && raw != nil {
		s, ok := raw.(string)
		if !ok {
			return nil
		}
		switch s {
		case "slow-2g", "2g", "3g", "4g":
		default:
			return nil
		}
		diagnostics.connectionEffectiveType = s
	}
	if raw, present := record["recoveryAction"]; present && raw != nil {
		s, ok := raw.(string)
		if !ok {
			return nil
		}
		switch s {
		case "guarded", "offline", "reload", "unavailable":
		default:
			return nil
		}
		diagnostics.recoveryAction = s
	}
	if v, err := obsOptionalNumber(record, "connectionRttMs", 10*60*1000, true); err != nil {
		return nil
	} else if v != nil {
		diagnostics.connectionRTTMS = v
	}
	if v, err := obsOptionalNumber(record, "resourceDurationMs", 60*60*1000, false); err != nil {
		return nil
	} else if v != nil {
		diagnostics.resourceDurationMS = v
	}
	if v, err := obsOptionalNumber(record, "resourceResponseStatus", 599, true); err != nil {
		return nil
	} else if v != nil {
		diagnostics.resourceResponseStatus = v
	}
	if v, err := obsOptionalNumber(record, "resourceTransferSize", 2147483647, true); err != nil {
		return nil
	} else if v != nil {
		diagnostics.resourceTransferSize = v
	}
	return diagnostics
}

func obsOptionalNumber(record map[string]any, key string, max float64, integer bool) (*float64, error) {
	raw, present := record[key]
	if !present || raw == nil {
		return nil, nil
	}
	value, ok := raw.(float64)
	if !ok || value != value || value < 0 || value > max || (integer && value != float64(int64(value))) {
		return nil, errObservabilityEnvelope
	}
	return &value, nil
}

func normalizeChunkURL(value string) string {
	if value == "" || utf16Len(value) > 1024 {
		return ""
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.RawFragment != "" {
		return ""
	}
	if !obsURLOriginLocal(parsed) {
		return ""
	}
	if !strings.HasPrefix(parsed.EscapedPath(), "/_next/static/chunks/") || !strings.HasSuffix(parsed.EscapedPath(), ".js") {
		return ""
	}
	normalized := parsed.String()
	if utf16Len(normalized) > 1024 {
		return ""
	}
	return normalized
}

// obsURLOriginLocal mirrors the TS local-or-https guard: https anywhere, http
// only on loopback hosts.
func obsURLOriginLocal(u *url.URL) bool {
	if u.Scheme == "https" {
		return true
	}
	if u.Scheme != "http" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return host == "127.0.0.1" || host == "localhost"
}

// obsURLOrigin renders a URL's origin the way WHATWG URL.origin does: scheme
// and host lowercased, default ports dropped, "null" for non-hierarchical
// schemes.
func obsURLOrigin(u *url.URL) string {
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" && scheme != "ws" && scheme != "wss" && scheme != "ftp" {
		return "null"
	}
	host := strings.ToLower(u.Host)
	if port := u.Port(); port != "" {
		if (scheme == "https" || scheme == "wss") && port == "443" || (scheme == "http" || scheme == "ws") && port == "80" || scheme == "ftp" && port == "21" {
			host = strings.ToLower(u.Hostname())
		}
	}
	return scheme + "://" + host
}

func obsChunkURLFromOrigin(value, expectedOrigin string) bool {
	expected, err := url.Parse(expectedOrigin)
	if err != nil || obsURLOrigin(expected) == "null" {
		return false
	}
	chunk, err := url.Parse(value)
	if err != nil {
		return false
	}
	return obsURLOrigin(chunk) == obsURLOrigin(expected)
}
