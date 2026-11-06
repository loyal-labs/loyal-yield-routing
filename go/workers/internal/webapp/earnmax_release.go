package webapp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/danielgtaylor/huma/v2"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/earnmax"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/releasecontrol"
)

const (
	depositQuotePath     = "/api/smart-accounts/earn-max/deposit-quote"
	releaseControlsPath  = "/api/smart-accounts/earn-max/release-controls"
	depositQuoteMaxBytes = 4 << 10
)

type DepositQuoteRequest struct {
	AmountRaw string `json:"amountRaw" pattern:"^[1-9][0-9]{0,19}$"`
}

type depositQuoteInput struct {
	SessionInput
	Raw rawJSON
}

type depositQuoteOutput struct {
	EarnMaxHeaders
	Body earnmax.DepositQuote
}

type DepositRefusal struct {
	Reason string `json:"reason" enum:"release_control_held,owner_not_admitted,amount_over_canary_limit,nav_uncertified,deposit_waiting_nav_publication"`
}

func (e *DepositRefusal) Error() string  { return e.Reason }
func (e *DepositRefusal) GetStatus() int { return http.StatusConflict }

type ReleaseCapability struct {
	Action       string  `json:"action"`
	State        string  `json:"state"`
	Admitted     bool    `json:"admitted"`
	Reason       string  `json:"reason,omitempty"`
	MaxAmountRaw *string `json:"maxAmountRaw"`
}

type ReleaseControlsView struct {
	Vault     string              `json:"vault"`
	Authority string              `json:"authority"`
	Actions   []ReleaseCapability `json:"actions"`
}

type releaseControlsOutput struct {
	EarnMaxHeaders
	Body ReleaseControlsView
}

func (h earnMaxRoutes) registerRelease(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "get-earn-max-release-controls",
		Method:      http.MethodGet,
		Path:        releaseControlsPath,
		Summary:     "Which Earn MAX money paths are open to the signed-in wallet now",
		Responses:   errorResponses[routeError](api, 401, 409, 503),
	}, func(ctx context.Context, in *sessionRequest) (*releaseControlsOutput, error) {
		p, err := h.auth.Principal(ctx, in.SessionInput)
		if err != nil {
			return nil, h.refused(err)
		}
		if err := h.admit(); err != nil {
			return nil, err
		}
		if h.cfg.Releases == nil {
			return nil, h.fail(http.StatusServiceUnavailable, "release_controls_unavailable", "Earn MAX availability is temporarily unknown.")
		}
		authority, err := earnmax.AuthorityOf(p.Settings)
		if err != nil {
			return nil, h.unavailable("release authority", err)
		}
		view := ReleaseControlsView{Vault: earnmax.Vault.String(), Authority: authority.Key().String()}
		for _, a := range releasecontrol.Actions {
			if !a.Owners {
				continue
			}
			c, reason, err := releasecontrol.Capability(ctx, h.cfg.Releases, view.Vault, a.Action, view.Authority, nil)
			if err != nil {
				return nil, h.unavailable("release controls", err)
			}
			if reason == releasecontrol.RefusedLimit {
				reason = ""
			}
			capability := ReleaseCapability{Action: string(a.Action), State: string(c.State), Admitted: reason == "", Reason: reason}
			if c.MaxAmountRaw != nil {
				raw := strconv.FormatUint(*c.MaxAmountRaw, 10)
				capability.MaxAmountRaw = &raw
			}
			view.Actions = append(view.Actions, capability)
		}
		return &releaseControlsOutput{h.headers, view}, nil
	})
	huma.Register(api, huma.Operation{
		OperationID: "post-earn-max-deposit-quote",
		Method:      http.MethodPost,
		Path:        depositQuotePath,
		Summary:     "Certified LP bounds for one official Earn MAX deposit",
		Responses: func() map[string]*huma.Response {
			out := errorResponses[routeError](api, 400, 401, 503)
			for code, r := range errorResponses[DepositRefusal](api, 409) {
				out[code] = r
			}
			return out
		}(),
	}, func(ctx context.Context, in *depositQuoteInput) (*depositQuoteOutput, error) {
		p, err := h.auth.Principal(ctx, in.SessionInput)
		if err != nil {
			return nil, h.refused(err)
		}
		if err := h.admit(); err != nil {
			return nil, err
		}
		var body DepositQuoteRequest
		if len(in.Raw.data) > depositQuoteMaxBytes || json.Unmarshal(in.Raw.data, &body) != nil {
			return nil, h.fail(http.StatusBadRequest, "invalid_amount", "Enter a deposit amount.")
		}
		amount, err := strconv.ParseUint(body.AmountRaw, 10, 64)
		if err != nil || amount == 0 || strconv.FormatUint(amount, 10) != body.AmountRaw {
			return nil, h.fail(http.StatusBadRequest, "invalid_amount", "Enter a deposit amount.")
		}
		if h.cfg.Deposits == nil {
			return nil, h.fail(http.StatusServiceUnavailable, "deposit_quote_unavailable", "Deposits are temporarily unavailable.")
		}
		authority, err := earnmax.AuthorityOf(p.Settings)
		if err != nil {
			return nil, h.unavailable("deposit authority", err)
		}
		quote, err := h.cfg.Deposits.Quote(ctx, authority.Key(), amount)
		var held *releasecontrol.Refusal
		var nav *earnmax.DepositRefusal
		switch {
		case errors.As(err, &held):
			return nil, h.refuseDeposit(held.Reason)
		case errors.As(err, &nav):
			return nil, h.refuseDeposit(nav.Reason)
		case err != nil:
			return nil, h.unavailable("deposit quote", err)
		}
		return &depositQuoteOutput{h.headers, quote}, nil
	})
	documentJSONBody[DepositQuoteRequest](api, depositQuotePath)
}

func (h earnMaxRoutes) refuseDeposit(reason string) error {
	return huma.ErrorWithHeaders(&DepositRefusal{Reason: reason}, http.Header{
		"X-Loyal-Earn-Max-Contract":   {h.headers.Contract},
		"X-Loyal-Deployment-Revision": {h.headers.Revision},
	})
}
