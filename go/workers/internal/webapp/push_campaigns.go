package webapp

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// POST /api/push/mixpanel-cohort   <- web/apps/web/src/app/api/push/mixpanel-cohort/route.ts
// POST /api/solana-week/sweep-notify <- web/apps/web/src/app/api/solana-week/sweep-notify/route.ts
//
// Both claim the send in loyal_yield.push_campaign_sends BEFORE the push, so a
// re-sync, a Mixpanel retry or two overlapping deployments never push twice.

var pushCampaignYieldGrants = []Grant{
	TableGrant("loyal_yield.push_campaign_sends", "SELECT"),
	TableGrant("loyal_yield.push_campaign_sends", "INSERT"),
	TableGrant("loyal_yield.push_campaign_sends", "UPDATE"),
	SequenceGrant("loyal_yield.push_campaign_sends_id_seq", "USAGE"),
}

var earnMixpanelCohortFamily = earnFamily{
	name:        "mixpanel cohort push",
	yieldGrants: pushCampaignYieldGrants,
	ready: func(h earnRoutes) bool {
		return h.YieldDB != nil && h.Push != nil && h.MixpanelCohortSecret != ""
	},
	register: earnRoutes.registerMixpanelCohort,
}

var earnSweepNotifyFamily = earnFamily{
	name:        "sweep notify push",
	yieldGrants: pushCampaignYieldGrants,
	ready: func(h earnRoutes) bool {
		return h.YieldDB != nil && h.Push != nil && h.SweepNotifySecret != ""
	},
	register: earnRoutes.registerSweepNotify,
}

const (
	mixpanelCohortPath      = "/api/push/mixpanel-cohort"
	sweepNotifyPath         = "/api/solana-week/sweep-notify"
	mixpanelCohortPrefix    = "push: "
	mobileDistinctIDPrefix  = "mob:"
	campaignSendConcurrency = 10
	campaignClaimBudget     = 10 * time.Second
	sweepDedupeKeyMax       = 120
)

// ASK-1651 copy, the Next MIXPANEL_COHORT_CAMPAIGNS table.
var mixpanelCohortCampaigns = map[string]autodepositWalletPush{
	"quest-1-nudge": {
		Body:  "Add $5 to your account and it's done.",
		Title: "You're one step from Quest 1.",
	},
	"quest-2-nudge": {
		Body:  "Enable auto-deposit and your USDC earns while it sits. Quest 2, done.",
		Title: "One quest left.",
	},
	"seeker-summer-ending": {
		Body:  "Less than 48 hours left on your quests. Don't leave the rewards on the table.",
		Title: "Seeker's Summer is almost over.",
	},
	"earning-while-away": {
		Body:  "It's been adding up. Come take a look.",
		Title: "Your USDC kept earning while you were out.",
	},
	"still-earning": {
		Body:  "Your USDC has been earning since you last checked in. Pick up where you left off.",
		Title: "Still working for you.",
	},
}

func autodepositSweepFailedPush(amountRaw uint64) autodepositWalletPush {
	body := "We'll retry automatically within the hour."
	if amountRaw > 0 {
		return autodepositWalletPush{Body: body, Title: formatAutodepositUSDC(amountRaw) + " didn't reach EARN."}
	}
	return autodepositWalletPush{Body: body, Title: "Auto-deposit didn't go through."}
}

func claimPushCampaignSend(ctx context.Context, pool *pgxpool.Pool, wallet, campaign string, cohortID *string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, campaignClaimBudget)
	defer cancel()
	var id int64
	err := pool.QueryRow(ctx, `INSERT INTO loyal_yield.push_campaign_sends (wallet_address, campaign, cohort_id)
VALUES ($1, $2, $3)
ON CONFLICT DO NOTHING
RETURNING id`, wallet, campaign, cohortID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

type campaignAck struct {
	Action string `json:"action"`
	Status string `json:"status"`
}

type campaignError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// campaignRejected is the Mixpanel webhook's failure envelope; Mixpanel pauses
// a sync on 400/401 and shows the message in its UI.
type campaignRejected struct {
	Action string        `json:"action"`
	Detail campaignError `json:"error"`
	Status string        `json:"status"`
	status int
}

func (e *campaignRejected) Error() string  { return e.Detail.Message }
func (e *campaignRejected) GetStatus() int { return e.status }

type sweepAck struct {
	Status string `json:"status"`
}

type sweepRejected struct {
	Message string `json:"error"`
	Detail  string `json:"message,omitempty"`
	status  int
}

func (e *sweepRejected) Error() string  { return e.Message }
func (e *sweepRejected) GetStatus() int { return e.status }

type mixpanelCohortInput struct {
	Authorization string `header:"Authorization"`
	Raw           rawJSON
}

type mixpanelCohortOutput struct{ Body campaignAck }

type sweepNotifyInput struct {
	Authorization string `header:"Authorization"`
	Raw           rawJSON
}

type sweepNotifyOutput struct{ Body sweepAck }

func (h earnRoutes) registerMixpanelCohort(api huma.API) {
	responses := errorResponses[campaignRejected](api, 400, 401, 500)
	huma.Register(api, huma.Operation{
		OperationID: "post-mixpanel-cohort",
		Method:      http.MethodPost,
		Path:        mixpanelCohortPath,
		Summary:     "Mixpanel cohort webhook that pushes a campaign to the cohort's mobile wallets",
		Responses:   responses,
	}, h.mixpanelCohort)
	api.OpenAPI().Paths[mixpanelCohortPath].Post.RequestBody = &huma.RequestBody{Content: map[string]*huma.MediaType{
		"application/json": {Schema: &huma.Schema{Type: "object"}},
	}}
}

func (h earnRoutes) registerSweepNotify(api huma.API) {
	responses := errorResponses[sweepRejected](api, 400, 401)
	huma.Register(api, huma.Operation{
		OperationID: "post-sweep-notify",
		Method:      http.MethodPost,
		Path:        sweepNotifyPath,
		Summary:     "Autodeposit worker callback that pushes a failed-sweep notice once per scheduled slot",
		Responses:   responses,
	}, h.sweepNotify)
	api.OpenAPI().Paths[sweepNotifyPath].Post.RequestBody = &huma.RequestBody{Content: map[string]*huma.MediaType{
		"application/json": {Schema: &huma.Schema{Type: "object"}},
	}}
}

func campaignFailure(action string, code int, message string) *campaignRejected {
	return &campaignRejected{Action: action, Detail: campaignError{Code: code, Message: message}, Status: "failure", status: code}
}

func jsonObject(raw []byte) (map[string]any, bool) {
	var body any
	if json.Unmarshal(raw, &body) != nil {
		return nil, false
	}
	record, _ := body.(map[string]any)
	if record == nil {
		record = map[string]any{}
	}
	return record, true
}

func jsonString(record map[string]any, key string) (string, bool) {
	value, ok := record[key].(string)
	return value, ok
}

func jsonObjectField(record map[string]any, key string) map[string]any {
	value, _ := record[key].(map[string]any)
	if value == nil {
		return map[string]any{}
	}
	return value
}

func secretEqual(provided, expected string) bool {
	return len(provided) == len(expected) && subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) == 1
}

// mixpanelAuthorized checks only the Basic password, as Mixpanel's webhook
// sends username:password.
func mixpanelAuthorized(header, secret string) bool {
	if secret == "" || !strings.HasPrefix(header, "Basic ") {
		return false
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(header[len("Basic "):]))
	if err != nil {
		decoded, err = base64.RawStdEncoding.DecodeString(strings.TrimRight(strings.TrimSpace(header[len("Basic "):]), "="))
		if err != nil {
			return false
		}
	}
	text := string(decoded)
	return secretEqual(text[strings.Index(text, ":")+1:], secret)
}

// jsNumberString is String(number) for the ids Mixpanel sends.
func jsNumberString(value float64) string {
	if value == math.Trunc(value) && math.Abs(value) < 1e21 {
		return strconv.FormatFloat(value, 'f', -1, 64)
	}
	return strconv.FormatFloat(value, 'g', -1, 64)
}

func (h earnRoutes) mixpanelCohort(ctx context.Context, in *mixpanelCohortInput) (*mixpanelCohortOutput, error) {
	record, ok := jsonObject(in.Raw.data)
	if !ok {
		return nil, campaignFailure("members", http.StatusBadRequest, "Invalid JSON body.")
	}
	action, _ := jsonString(record, "action")
	if action != "members" && action != "add_members" && action != "remove_members" {
		return nil, campaignFailure("members", http.StatusBadRequest, "Unknown action.")
	}
	if !mixpanelAuthorized(in.Authorization, h.MixpanelCohortSecret) {
		return nil, campaignFailure(action, http.StatusUnauthorized, "Unauthorized.")
	}
	success := &mixpanelCohortOutput{Body: campaignAck{Action: action, Status: "success"}}
	if action == "remove_members" {
		return success, nil
	}

	parameters := jsonObjectField(record, "parameters")
	cohortName, _ := jsonString(parameters, "mixpanel_cohort_name")
	campaign := ""
	known := false
	var payload autodepositWalletPush
	if strings.HasPrefix(cohortName, mixpanelCohortPrefix) {
		campaign = strings.TrimSpace(cohortName[len(mixpanelCohortPrefix):])
		payload, known = mixpanelCohortCampaigns[campaign]
	}
	if campaign == "" || !known {
		return nil, campaignFailure(action, http.StatusBadRequest,
			fmt.Sprintf(`Cohort name must be "push: <campaign>" with a known campaign key (got "%s").`, cohortName))
	}

	members, _ := parameters["members"].([]any)
	seen := map[string]bool{}
	var wallets []string
	for _, member := range members {
		entry, _ := member.(map[string]any)
		distinctID, _ := jsonString(entry, "mixpanel_distinct_id")
		if wallet, found := strings.CutPrefix(distinctID, mobileDistinctIDPrefix); found && !seen[wallet] {
			seen[wallet] = true
			wallets = append(wallets, wallet)
		}
	}
	var cohortID *string
	switch value := parameters["mixpanel_cohort_id"].(type) {
	case string:
		cohortID = &value
	case float64:
		text := jsNumberString(value)
		cohortID = &text
	}

	var sent, deduped atomic.Int64
	var failure atomic.Value
	for start := 0; start < len(wallets) && failure.Load() == nil; start += campaignSendConcurrency {
		chunk := wallets[start:min(start+campaignSendConcurrency, len(wallets))]
		var wg sync.WaitGroup
		for _, wallet := range chunk {
			wg.Go(func() {
				claimed, err := claimPushCampaignSend(ctx, h.YieldDB, wallet, campaign, cohortID)
				switch {
				case err != nil:
					failure.CompareAndSwap(nil, err)
				case !claimed:
					deduped.Add(1)
				default:
					h.Push.send(ctx, wallet, payload)
					sent.Add(1)
				}
			})
		}
		wg.Wait()
	}
	if err, _ := failure.Load().(error); err != nil {
		slog.WarnContext(ctx, "[mixpanel-cohort] claim failed", "campaign", campaign, "error", err)
		return nil, campaignFailure(action, http.StatusInternalServerError, "Push claim failed.")
	}
	slog.InfoContext(ctx, "[mixpanel-cohort] processed", "action", action, "campaign", campaign,
		"deduped", deduped.Load(), "members", len(members), "mobileWallets", len(wallets), "sent", sent.Load())
	return success, nil
}

var amountRawPattern = regexp.MustCompile(`^\d+$`)

func sweepAmountRaw(value any) uint64 {
	text, ok := value.(string)
	if !ok || !amountRawPattern.MatchString(text) {
		return 0
	}
	amount, err := strconv.ParseUint(text, 10, 64)
	if err != nil {
		return 0
	}
	return amount
}

func (h earnRoutes) sweepNotify(ctx context.Context, in *sweepNotifyInput) (*sweepNotifyOutput, error) {
	if !secretEqual(in.Authorization, "Bearer "+h.SweepNotifySecret) {
		return nil, &sweepRejected{Message: "unauthorized", status: http.StatusUnauthorized}
	}
	record, ok := jsonObject(in.Raw.data)
	if !ok {
		return nil, &sweepRejected{Message: "invalid_request", status: http.StatusBadRequest}
	}
	wallet, _ := jsonString(record, "walletAddress")
	if strings.TrimSpace(wallet) == "" {
		return nil, &sweepRejected{Message: "invalid_request", Detail: "walletAddress is required.", status: http.StatusBadRequest}
	}
	if kind, _ := jsonString(record, "kind"); kind != "failed" {
		return nil, &sweepRejected{Message: "invalid_request", Detail: `kind must be "failed".`, status: http.StatusBadRequest}
	}
	amountRaw := sweepAmountRaw(record["amountRaw"])
	if !h.claimFailurePush(ctx, wallet, record["dedupeKey"]) {
		return &sweepNotifyOutput{Body: sweepAck{Status: "deduped"}}, nil
	}
	h.Push.send(ctx, wallet, autodepositSweepFailedPush(amountRaw))
	return &sweepNotifyOutput{Body: sweepAck{Status: "accepted"}}, nil
}

// A failed sweep is retried every cycle, so the slot id is the dedupe key; an
// older worker without one can still wake the user once a day. A claim that
// cannot be written lets the push through, as Next does.
func (h earnRoutes) claimFailurePush(ctx context.Context, wallet string, rawKey any) bool {
	key := time.Now().UTC().Format("2006-01-02")
	if text, ok := rawKey.(string); ok && strings.TrimSpace(text) != "" {
		key = truncateUTF16(strings.TrimSpace(text), sweepDedupeKeyMax)
	}
	claimed, err := claimPushCampaignSend(ctx, h.YieldDB, wallet, "autodeposit-failed:"+key, nil)
	if err != nil {
		slog.WarnContext(ctx, "[sweep-notify] failure sent-log write failed", "errorMessage", err.Error(), "walletAddress", wallet)
		return true
	}
	return claimed
}
