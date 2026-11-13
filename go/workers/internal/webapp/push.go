package webapp

import (
	"context"
	"encoding/json"
	"log/slog"
	"maps"
	"net/http"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type PushTokenRequest struct {
	_        struct{} `additionalProperties:"true"`
	Token    string   `json:"token,omitempty"`
	Platform string   `json:"platform,omitempty"`
	// Provider is "onesignal" for a OneSignal push subscription ID; omitted means a legacy Expo token.
	Provider string `json:"provider,omitempty"`
}

type PushTokenSaved struct {
	Success bool `json:"success"`
}

type PushError struct {
	Message string `json:"error"`
	status  int
}

func (e *PushError) Error() string  { return e.Message }
func (e *PushError) GetStatus() int { return e.status }

type pushRequest struct {
	SessionInput
	Raw rawJSON
}

const pushPath = "/api/push-tokens"

const (
	pushProviderExpo      = "expo"
	pushProviderOneSignal = "onesignal"
)

var (
	errPushFields = &PushError{Message: "Missing required fields: token, platform", status: http.StatusBadRequest}
	errPushToken  = &PushError{Message: "Invalid push token for provider", status: http.StatusBadRequest}
	errPushFailed = &PushError{Message: "Failed to register push token", status: http.StatusInternalServerError}
)

func pushFields(raw []byte) (PushTokenRequest, *PushError) {
	var body PushTokenRequest
	if !json.Valid(raw) {
		return body, errPushFailed
	}
	if json.Unmarshal(raw, &body) != nil || body.Token == "" || (body.Platform != "ios" && body.Platform != "android") {
		return body, errPushFields
	}
	switch body.Provider {
	case "", pushProviderExpo:
		body.Provider = pushProviderExpo
		if !validExpoToken(body.Token) {
			return body, errPushToken
		}
	case pushProviderOneSignal:
		// The server sends to these IDs, so only a canonical subscription UUID is stored.
		id, err := uuid.Parse(body.Token)
		if err != nil || id.String() != strings.ToLower(body.Token) {
			return body, errPushToken
		}
		body.Token = id.String()
	default:
		return body, errPushToken
	}
	return body, nil
}

type pushOutput struct{ Body PushTokenSaved }

func registerPush(api huma.API, auth *Auth, appDB *pgxpool.Pool) {
	responses := errorResponses[routeError](api, 401)
	maps.Copy(responses, errorResponses[PushError](api, 400, 500))
	huma.Register(api, huma.Operation{
		OperationID: "post-push-token",
		Method:      http.MethodPost,
		Path:        pushPath,
		Summary:     "Register the device push token for the signed-in wallet",
		Responses:   responses,
	}, func(ctx context.Context, in *pushRequest) (*pushOutput, error) {
		session, err := auth.Session(in.SessionInput)
		if refusal, ok := authRefusal(err); ok {
			return nil, refusal
		}
		if err != nil {
			slog.Warn("push token not saved", "step", "session", "error", err)
			return nil, errPushFailed
		}
		body, refused := pushFields(in.Raw.data)
		if refused != nil {
			return nil, refused
		}
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if _, err := appDB.Exec(ctx, `INSERT INTO push_tokens (token, telegram_user_id, wallet_public_key, platform, provider) VALUES ($1, NULL, $2, $3, $4)
ON CONFLICT (token) DO UPDATE SET telegram_user_id = CASE WHEN push_tokens.wallet_public_key = EXCLUDED.wallet_public_key THEN push_tokens.telegram_user_id END, wallet_public_key = EXCLUDED.wallet_public_key, platform = EXCLUDED.platform, provider = EXCLUDED.provider, updated_at = now()`,
			body.Token, session.Wallet.String(), body.Platform, body.Provider); err != nil {
			slog.Warn("push token not saved", "step", "upsert", "error", err)
			return nil, errPushFailed
		}
		return &pushOutput{PushTokenSaved{Success: true}}, nil
	})
	documentJSONBody[PushTokenRequest](api, pushPath)
}
