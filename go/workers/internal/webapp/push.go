package webapp

import (
	"context"
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
	Body *PushTokenRequest `required:"false"`
}

type pushOutput struct{ Body PushTokenSaved }

func registerPush(api huma.API, auth *Auth, appDB *pgxpool.Pool) {
	responses := errorResponses[routeError](api, 401, 409)
	maps.Copy(responses, errorResponses[PushError](api, 400, 500))
	pushFailed := &PushError{Message: "Failed to register push token", status: http.StatusInternalServerError}
	huma.Register(api, huma.Operation{
		OperationID: "post-push-token",
		Method:      http.MethodPost,
		Path:        "/api/push-tokens",
		Summary:     "Register the device push token for the signed-in wallet",
		Responses:   responses,
	}, func(ctx context.Context, in *pushRequest) (*pushOutput, error) {
		p, err := auth.Principal(ctx, in.SessionInput)
		if refusal, ok := authRefusal(err); ok {
			return nil, refusal
		}
		if err != nil {
			slog.Warn("push token not saved", "step", "session", "error", err)
			return nil, pushFailed
		}
		if in.Body == nil || in.Body.Token == "" || in.Body.Platform == "" {
			return nil, &PushError{Message: "Missing required fields: token, platform", status: http.StatusBadRequest}
		}
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if _, err := appDB.Exec(ctx, `INSERT INTO push_tokens (token, telegram_user_id, wallet_public_key, platform) VALUES ($1, NULL, $2, $3)
ON CONFLICT (token) DO UPDATE SET telegram_user_id = NULL, wallet_public_key = EXCLUDED.wallet_public_key, platform = EXCLUDED.platform, updated_at = now()`,
			in.Body.Token, p.Wallet.String(), in.Body.Platform); err != nil {
			slog.Warn("push token not saved", "step", "upsert", "error", err)
			return nil, pushFailed
		}
		return &pushOutput{PushTokenSaved{Success: true}}, nil
	})
}
