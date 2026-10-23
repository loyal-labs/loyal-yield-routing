package webapp

import (
	"context"
	"encoding/json"
	"log/slog"
	"maps"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5/pgxpool"
)

type PushTokenRequest struct {
	_        struct{} `additionalProperties:"true"`
	Token    string   `json:"token,omitempty"`
	Platform string   `json:"platform,omitempty"`
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

var (
	errPushFields = &PushError{Message: "Missing required fields: token, platform", status: http.StatusBadRequest}
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
		if _, err := appDB.Exec(ctx, `INSERT INTO push_tokens (token, telegram_user_id, wallet_public_key, platform) VALUES ($1, NULL, $2, $3)
ON CONFLICT (token) DO UPDATE SET telegram_user_id = CASE WHEN push_tokens.wallet_public_key = EXCLUDED.wallet_public_key THEN push_tokens.telegram_user_id END, wallet_public_key = EXCLUDED.wallet_public_key, platform = EXCLUDED.platform, updated_at = now()`,
			body.Token, session.Wallet.String(), body.Platform); err != nil {
			slog.Warn("push token not saved", "step", "upsert", "error", err)
			return nil, errPushFailed
		}
		return &pushOutput{PushTokenSaved{Success: true}}, nil
	})
	documentJSONBody[PushTokenRequest](api, pushPath)
}
