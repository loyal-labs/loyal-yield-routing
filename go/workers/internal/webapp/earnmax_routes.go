package webapp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/earnmax"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
)

const earnMaxContract = "earn-max-v4"

type SolanaCluster string

const (
	Mainnet SolanaCluster = "mainnet"
	Devnet  SolanaCluster = "devnet"
)

func ParseSolanaCluster(raw string) (SolanaCluster, error) {
	switch cluster := SolanaCluster(strings.TrimSpace(raw)); cluster {
	case "":
		return Mainnet, nil
	case Mainnet, Devnet:
		return cluster, nil
	}
	return "", errors.New("LOYAL_WEB_SOLANA_CLUSTER: want mainnet or devnet")
}

type InviteCodes map[string]struct{}

var (
	inviteCodePattern = regexp.MustCompile(`^[A-Z0-9]{6}$`)
	inviteHashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

func ParseInviteCodes(raw string) (InviteCodes, error) {
	codes := InviteCodes{}
	for entry := range strings.SplitSeq(raw, ",") {
		hash := strings.ToLower(strings.TrimSpace(entry))
		if hash == "" {
			continue
		}
		if !inviteHashPattern.MatchString(hash) {
			return nil, errors.New("EARN_MAX_INVITE_CODE_HASHES has a malformed entry")
		}
		codes[hash] = struct{}{}
	}
	return codes, nil
}

func (c InviteCodes) admit(code *string) (string, bool) {
	if code == nil {
		return "", false
	}
	normalized := strings.ToUpper(strings.TrimSpace(*code))
	if !inviteCodePattern.MatchString(normalized) {
		return "", false
	}
	sum := sha256.Sum256([]byte(normalized))
	hash := hex.EncodeToString(sum[:])
	_, ok := c[hash]
	return hash, ok
}

type EarnMax struct {
	Reader  *earnmax.Reader
	Cluster SolanaCluster
	Invites InviteCodes
}

type EarnMaxHeaders struct {
	Contract string `header:"X-Loyal-Earn-Max-Contract"`
	Revision string `header:"X-Loyal-Deployment-Revision"`
}

type EarnMaxConfig struct {
	DelegatedSigner string `json:"delegatedSigner"`
	ProgramID       string `json:"programId"`
	SettingsPda     string `json:"settingsPda"`
	WalletAddress   string `json:"walletAddress"`
}

type (
	EarnMaxSummary  earnmax.Summary
	EarnMaxActivity earnmax.Activity
)

type EarnMaxSummaryResponse struct {
	Config  EarnMaxConfig  `json:"config"`
	Summary EarnMaxSummary `json:"summary"`
}

type InviteStatus struct {
	Redeemed bool `json:"redeemed"`
}

type InviteRequest struct {
	_    struct{} `additionalProperties:"true"`
	Code *string  `json:"code,omitempty"`
}

type EarnMaxErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message,omitempty"`
}

type routeError struct {
	Detail EarnMaxErrorDetail `json:"error"`
	status int
}

func (e *routeError) Error() string  { return e.Detail.Code }
func (e *routeError) GetStatus() int { return e.status }

func authRefusal(err error) (*routeError, bool) {
	var refused *AuthError
	if !errors.As(err, &refused) {
		return nil, false
	}
	return &routeError{Detail: EarnMaxErrorDetail{Code: refused.Failure.Code(), Message: authMessage(refused.Failure)}, status: refused.Failure.Status()}, true
}

func authMessage(failure AuthFailure) string {
	switch failure {
	case Unauthenticated:
		return "No active auth session."
	case InvalidMobileSession:
		return "Mobile Earn session is invalid or expired."
	case WalletRequired:
		return "Sign in with a Solana wallet."
	case SmartAccountRequired:
		return "Sign in to set up your smart account."
	}
	panic("webapp: unknown auth failure")
}

type earnMaxRoutes struct {
	auth    *Auth
	appDB   *pgxpool.Pool
	cfg     EarnMax
	headers EarnMaxHeaders
	tries   *inviteAttempts
}

func (h earnMaxRoutes) fail(status int, code, message string) error {
	return huma.ErrorWithHeaders(&routeError{Detail: EarnMaxErrorDetail{Code: code, Message: message}, status: status}, http.Header{
		"X-Loyal-Earn-Max-Contract":   {h.headers.Contract},
		"X-Loyal-Deployment-Revision": {h.headers.Revision},
	})
}

func (h earnMaxRoutes) unavailable(step string, err error) error {
	slog.Warn("earn max unavailable", "step", step, "error", err)
	return h.fail(http.StatusServiceUnavailable, "earn_max_unavailable", "Earn MAX is temporarily unavailable.")
}

func (h earnMaxRoutes) refused(err error) error {
	refusal, ok := authRefusal(err)
	if !ok {
		return h.unavailable("session", err)
	}
	return h.fail(refusal.status, refusal.Detail.Code, refusal.Detail.Message)
}

func (h earnMaxRoutes) redeemed(ctx context.Context, wallet string) (bool, error) {
	var redeemed bool
	err := readContent(ctx, h.appDB, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM earn_max_invite_redemptions WHERE wallet_address = $1)`, wallet).Scan(&redeemed)
	})
	return redeemed, err
}

func (h earnMaxRoutes) admit(ctx context.Context, p Principal) error {
	if h.cfg.Cluster != Mainnet {
		return h.fail(http.StatusConflict, "unsupported_cluster", "Earn MAX requires mainnet.")
	}
	redeemed, err := h.redeemed(ctx, p.Wallet.String())
	if err != nil {
		return h.unavailable("invite", err)
	}
	if !redeemed {
		return h.fail(http.StatusForbidden, "invite_required", "")
	}
	return nil
}

func (h earnMaxRoutes) redeem(ctx context.Context, p Principal, hash string) (bool, error) {
	insertCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	tag, err := h.appDB.Exec(insertCtx, `INSERT INTO earn_max_invite_redemptions (code_hash, settings_pda, wallet_address) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`,
		hash, p.Settings.String(), p.Wallet.String())
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() > 0 {
		return true, nil
	}
	return h.redeemed(ctx, p.Wallet.String())
}

type summaryOutput struct {
	EarnMaxHeaders
	Body EarnMaxSummaryResponse
}

type activityOutput struct {
	EarnMaxHeaders
	Body EarnMaxActivity
}

type inviteOutput struct {
	EarnMaxHeaders
	Body InviteStatus
}

type sessionRequest struct{ SessionInput }

type redeemRequest struct {
	SessionInput
	Body *InviteRequest `required:"false"`
}

func registerEarnMax(api huma.API, auth *Auth, appDB *pgxpool.Pool, cfg EarnMax, release string) {
	if release == "" {
		release = "unknown"
	}
	h := earnMaxRoutes{auth: auth, appDB: appDB, cfg: cfg, headers: EarnMaxHeaders{earnMaxContract, release}, tries: newInviteAttempts()}
	errs := func(codes ...int) map[string]*huma.Response {
		return errorResponses[routeError](api, codes...)
	}
	huma.Register(api, huma.Operation{
		OperationID: "get-earn-max-summary",
		Method:      http.MethodGet,
		Path:        "/api/smart-accounts/earn-max/summary",
		Summary:     "Earn MAX position for the signed-in wallet",
		Responses:   errs(401, 403, 409, 503),
	}, func(ctx context.Context, in *sessionRequest) (*summaryOutput, error) {
		p, err := h.auth.Principal(ctx, in.SessionInput)
		if err != nil {
			return nil, h.refused(err)
		}
		if err := h.admit(ctx, p); err != nil {
			return nil, err
		}
		summary, err := cfg.Reader.Summary(ctx, p.Settings)
		if err != nil {
			return nil, h.unavailable("summary", err)
		}
		return &summaryOutput{h.headers, EarnMaxSummaryResponse{
			Config:  EarnMaxConfig{ProgramID: squads.ProgramID.String(), SettingsPda: p.Settings.String(), WalletAddress: p.Wallet.String()},
			Summary: EarnMaxSummary(summary),
		}}, nil
	})
	huma.Register(api, huma.Operation{
		OperationID: "get-earn-max-activity",
		Method:      http.MethodGet,
		Path:        "/api/smart-accounts/earn-max/activity",
		Summary:     "Earn MAX operations and performance for the signed-in wallet",
		Responses:   errs(401, 403, 409, 503),
	}, func(ctx context.Context, in *sessionRequest) (*activityOutput, error) {
		p, err := h.auth.Principal(ctx, in.SessionInput)
		if err != nil {
			return nil, h.refused(err)
		}
		if err := h.admit(ctx, p); err != nil {
			return nil, err
		}
		activity, err := cfg.Reader.Activity(ctx, p.Settings)
		if err != nil {
			return nil, h.unavailable("activity", err)
		}
		return &activityOutput{h.headers, EarnMaxActivity(activity)}, nil
	})
	huma.Register(api, huma.Operation{
		OperationID: "get-earn-max-invite",
		Method:      http.MethodGet,
		Path:        "/api/smart-accounts/earn-max/invite",
		Summary:     "Whether the signed-in wallet has redeemed an Earn MAX invite",
		Responses:   errs(401, 409, 503),
	}, func(ctx context.Context, in *sessionRequest) (*inviteOutput, error) {
		p, err := h.auth.Principal(ctx, in.SessionInput)
		if err != nil {
			return nil, h.refused(err)
		}
		redeemed, err := h.redeemed(ctx, p.Wallet.String())
		if err != nil {
			return nil, h.unavailable("invite", err)
		}
		return &inviteOutput{h.headers, InviteStatus{Redeemed: redeemed}}, nil
	})
	huma.Register(api, huma.Operation{
		OperationID: "post-earn-max-invite",
		Method:      http.MethodPost,
		Path:        "/api/smart-accounts/earn-max/invite",
		Summary:     "Redeem an Earn MAX invite code for the signed-in wallet",
		Responses:   errs(400, 401, 409, 429, 503),
	}, func(ctx context.Context, in *redeemRequest) (*inviteOutput, error) {
		p, err := h.auth.Principal(ctx, in.SessionInput)
		if err != nil {
			return nil, h.refused(err)
		}
		if !h.tries.consume(p.Wallet.String(), time.Now()) {
			return nil, h.fail(http.StatusTooManyRequests, "rate_limited", "")
		}
		var code *string
		if in.Body != nil {
			code = in.Body.Code
		}
		hash, ok := cfg.Invites.admit(code)
		if !ok {
			return nil, h.fail(http.StatusBadRequest, "invalid_code", "")
		}
		redeemed, err := h.redeem(ctx, p, hash)
		if err != nil {
			return nil, h.unavailable("redeem", err)
		}
		if !redeemed {
			return nil, h.fail(http.StatusConflict, "code_used", "")
		}
		return &inviteOutput{h.headers, InviteStatus{Redeemed: true}}, nil
	})
}

func errorResponses[T any](api huma.API, codes ...int) map[string]*huma.Response {
	schema := api.OpenAPI().Components.Schemas.Schema(reflect.TypeFor[T](), true, "")
	out := map[string]*huma.Response{}
	for _, code := range codes {
		out[strconv.Itoa(code)] = &huma.Response{
			Description: http.StatusText(code),
			Content:     map[string]*huma.MediaType{"application/json": {Schema: schema}},
		}
	}
	return out
}

const (
	inviteWindow      = 10 * time.Minute
	inviteMaxAttempts = 10
	inviteMaxTracked  = 4096
)

type inviteAttempts struct {
	mu   sync.Mutex
	seen map[string]inviteWindowCount
}

type inviteWindowCount struct {
	count   int
	resetAt time.Time
}

func newInviteAttempts() *inviteAttempts {
	return &inviteAttempts{seen: map[string]inviteWindowCount{}}
}

func (a *inviteAttempts) consume(key string, now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if current, ok := a.seen[key]; ok && current.resetAt.After(now) {
		if current.count >= inviteMaxAttempts {
			return false
		}
		current.count++
		a.seen[key] = current
		return true
	}
	if len(a.seen) >= inviteMaxTracked {
		for k, v := range a.seen {
			if !v.resetAt.After(now) {
				delete(a.seen, k)
			}
		}
		for k := range a.seen {
			if len(a.seen) < inviteMaxTracked {
				break
			}
			delete(a.seen, k)
		}
	}
	a.seen[key] = inviteWindowCount{count: 1, resetAt: now.Add(inviteWindow)}
	return true
}
