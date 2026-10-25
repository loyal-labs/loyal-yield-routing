package webapp

import (
	"math"
	"strings"
	"time"
)

const (
	obsFrontendLoadingMetricName = "loyal.frontend.loading.duration"
	obsMobileLoadingMetricName   = "loyal.mobile.loading.duration"
)

var (
	obsFrontendLoadingOperations = []string{
		"page_load",
		"earn.deposit",
		"earn.withdrawal",
		"earn.close",
		"earn.autodeposit.setup",
		"earn.autodeposit.close",
	}
	obsFrontendLoadingPhases = []string{
		"balances_ready",
		"interaction_to_preview",
		"wallet_confirmation_to_ui",
		"dependency",
	}
	obsFrontendLoadingDependencies  = []string{"loyal_api", "solana_rpc", "third_party_api"}
	obsFrontendLoadingOutcomes      = []string{"completed", "failed"}
	obsFrontendLoadingPresentations = []string{"in_app", "wallet"}
	obsMobileLoadingOperations      = []string{
		"app_load",
		"earn.deposit",
		"earn.withdrawal",
		"earn.refund",
		"earn.autodeposit.setup",
		"earn.autodeposit.floor_update",
		"earn.autodeposit.pause",
		"earn.autodeposit.resume",
		"earn.autodeposit.close",
		"earn.autodeposit.execute_now",
	}
	obsMobileMetricPhases = []string{"app_ready", "interaction_to_ui"}
)

type obsNormalizedMetric struct {
	appSessionID          string
	dependency            string
	deploymentEnvironment string
	deviceID              string
	durationMS            float64
	flowID                string
	metricName            string
	operation             string
	outcome               string
	pageSessionID         string
	pathname              string
	phase                 string
	platform              string
	presentation          string
	release               string
	requestCount          *int64
	serviceName           string
	timestamp             string
}

// obsRoundMillis ports the `Math.round(durationMs * 1000) / 1000` midpoint
// rounding; durations are non-negative so Go math.Round matches.
func obsRoundMillis(value float64) float64 {
	return math.Round(value*1000) / 1000
}

func parseBrowserLoadingMetricEnvelope(raw []byte, now time.Time) (obsNormalizedMetric, error) {
	record, err := obsObjectBody(raw)
	if err != nil {
		return obsNormalizedMetric{}, err
	}
	if err := obsAllowedKeys(record, "dependency", "durationMs", "flowId", "metricName",
		"operation", "outcome", "pageSessionId", "pathname", "phase",
		"presentation", "requestCount", "timestamp"); err != nil {
		return obsNormalizedMetric{}, err
	}
	if record["metricName"] != obsFrontendLoadingMetricName {
		return obsNormalizedMetric{}, errObservabilityEnvelope
	}
	operation, ok := obsStringIn(record["operation"], obsFrontendLoadingOperations)
	if !ok {
		return obsNormalizedMetric{}, errObservabilityEnvelope
	}
	phase, ok := obsStringIn(record["phase"], obsFrontendLoadingPhases)
	if !ok {
		return obsNormalizedMetric{}, errObservabilityEnvelope
	}
	outcome, ok := obsStringIn(record["outcome"], obsFrontendLoadingOutcomes)
	if !ok {
		return obsNormalizedMetric{}, errObservabilityEnvelope
	}
	durationRaw, ok := record["durationMs"].(float64)
	if !ok || math.IsNaN(durationRaw) || math.IsInf(durationRaw, 0) ||
		durationRaw < 0 || durationRaw > 24*60*60*1000 {
		return obsNormalizedMetric{}, errObservabilityEnvelope
	}

	rawPathname, ok := record["pathname"].(string)
	if !ok {
		return obsNormalizedMetric{}, errObservabilityEnvelope
	}
	pathname := normalizeTelemetryPathname(rawPathname)
	if pathname == "" {
		return obsNormalizedMetric{}, errObservabilityEnvelope
	}
	rawTimestamp, ok := record["timestamp"].(string)
	if !ok {
		return obsNormalizedMetric{}, errObservabilityEnvelope
	}
	timestamp, err := obsTimestampInWindow(rawTimestamp, now)
	if err != nil {
		return obsNormalizedMetric{}, err
	}

	pageSessionID, _ := record["pageSessionId"].(string)
	if _, present := record["pageSessionId"]; present && !obsPageSessionID.MatchString(pageSessionID) {
		return obsNormalizedMetric{}, errObservabilityEnvelope
	}

	isPageLoad := operation == "page_load"
	flowID, _ := record["flowId"].(string)
	_, flowPresent := record["flowId"]
	if isPageLoad && phase != "balances_ready" ||
		!isPageLoad && phase == "balances_ready" ||
		!isPageLoad && !isCanonicalUUIDV4(flowID) ||
		isPageLoad && flowPresent {
		return obsNormalizedMetric{}, errObservabilityEnvelope
	}

	isDependency := phase == "dependency"
	var dependency string
	var requestCount *int64
	if isDependency {
		dependency, ok = obsStringIn(record["dependency"], obsFrontendLoadingDependencies)
		if !ok {
			return obsNormalizedMetric{}, errObservabilityEnvelope
		}
		requestCount, err = obsOptionalIntField(record, "requestCount", 0, 128)
		if err != nil || requestCount == nil {
			return obsNormalizedMetric{}, errObservabilityEnvelope
		}
	} else if _, present := record["dependency"]; present {
		return obsNormalizedMetric{}, errObservabilityEnvelope
	} else if _, present := record["requestCount"]; present {
		return obsNormalizedMetric{}, errObservabilityEnvelope
	}

	isPreview := phase == "interaction_to_preview"
	presentation, _ := obsStringIn(record["presentation"], obsFrontendLoadingPresentations)
	if isPreview && presentation == "" {
		return obsNormalizedMetric{}, errObservabilityEnvelope
	}
	if !isPreview {
		if _, present := record["presentation"]; present {
			return obsNormalizedMetric{}, errObservabilityEnvelope
		}
	}

	return obsNormalizedMetric{
		dependency:    dependency,
		durationMS:    obsRoundMillis(durationRaw),
		flowID:        flowID,
		metricName:    obsFrontendLoadingMetricName,
		operation:     operation,
		outcome:       outcome,
		pageSessionID: pageSessionID,
		pathname:      pathname,
		phase:         phase,
		presentation:  presentation,
		requestCount:  requestCount,
		timestamp:     timestamp.UTC().Format("2006-01-02T15:04:05.000Z07:00"),
	}, nil
}

func parseMobileLoadingMetricEnvelope(raw []byte, now time.Time) (obsNormalizedMetric, error) {
	record, err := obsObjectBody(raw)
	if err != nil {
		return obsNormalizedMetric{}, err
	}
	if err := obsAllowedKeys(record, "appSessionId", "deviceId", "durationMs", "environment",
		"flowId", "metricName", "operation", "outcome", "pathname", "phase",
		"platform", "release", "timestamp"); err != nil {
		return obsNormalizedMetric{}, err
	}
	if record["metricName"] != obsMobileLoadingMetricName {
		return obsNormalizedMetric{}, errObservabilityEnvelope
	}
	operation, ok := obsStringIn(record["operation"], obsMobileLoadingOperations)
	if !ok {
		return obsNormalizedMetric{}, errObservabilityEnvelope
	}
	outcome, ok := obsStringIn(record["outcome"], obsFrontendLoadingOutcomes)
	if !ok {
		return obsNormalizedMetric{}, errObservabilityEnvelope
	}
	phase, ok := obsStringIn(record["phase"], obsMobileMetricPhases)
	if !ok {
		return obsNormalizedMetric{}, errObservabilityEnvelope
	}
	platform, ok := obsStringIn(record["platform"], obsMobileDevicePlatforms)
	if !ok {
		return obsNormalizedMetric{}, errObservabilityEnvelope
	}
	durationRaw, ok := record["durationMs"].(float64)
	if !ok || math.IsNaN(durationRaw) || math.IsInf(durationRaw, 0) ||
		durationRaw < 0 || durationRaw > 24*60*60*1000 {
		return obsNormalizedMetric{}, errObservabilityEnvelope
	}
	appSessionID, _ := record["appSessionId"].(string)
	if !isCanonicalUUIDV4(appSessionID) {
		return obsNormalizedMetric{}, errObservabilityEnvelope
	}
	deviceID, _ := record["deviceId"].(string)
	if _, present := record["deviceId"]; present && !isCanonicalUUIDV4(deviceID) {
		return obsNormalizedMetric{}, errObservabilityEnvelope
	}

	pathname, ok := record["pathname"].(string)
	if !ok || !strings.HasPrefix(pathname, "/") ||
		utf16Len(pathname) > 256 ||
		strings.Contains(pathname, "?") || strings.Contains(pathname, "#") {
		return obsNormalizedMetric{}, errObservabilityEnvelope
	}

	isAppLoad := operation == "app_load"
	flowID, _ := record["flowId"].(string)
	_, flowPresent := record["flowId"]
	if isAppLoad && (phase != "app_ready" || flowPresent) ||
		!isAppLoad && (phase != "interaction_to_ui" || !isCanonicalUUIDV4(flowID)) {
		return obsNormalizedMetric{}, errObservabilityEnvelope
	}

	environment, isString := record["environment"].(string)
	release, isReleaseString := record["release"].(string)
	if !isString || normalizeResourceValue(environment, obsMaxEnvironmentLength) != environment ||
		!isReleaseString || normalizeResourceValue(release, obsMaxReleaseLength) != release {
		return obsNormalizedMetric{}, errObservabilityEnvelope
	}
	rawTimestamp, ok := record["timestamp"].(string)
	if !ok {
		return obsNormalizedMetric{}, errObservabilityEnvelope
	}
	timestamp, err := obsTimestampInWindow(rawTimestamp, now)
	if err != nil {
		return obsNormalizedMetric{}, err
	}

	return obsNormalizedMetric{
		appSessionID:          appSessionID,
		deploymentEnvironment: environment,
		deviceID:              deviceID,
		durationMS:            obsRoundMillis(durationRaw),
		flowID:                flowID,
		metricName:            obsMobileLoadingMetricName,
		operation:             operation,
		outcome:               outcome,
		pathname:              pathname,
		phase:                 phase,
		platform:              platform,
		release:               release,
		timestamp:             timestamp.UTC().Format("2006-01-02T15:04:05.000Z07:00"),
	}, nil
}
