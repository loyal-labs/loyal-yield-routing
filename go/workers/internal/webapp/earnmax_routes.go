package webapp

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
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

type InviteCodes [][sha256.Size]byte

var (
	inviteCodePattern = regexp.MustCompile(`^[A-Z0-9]{6}$`)
	inviteHashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

func ParseInviteCodes(raw string) (InviteCodes, error) {
	var codes InviteCodes
	for entry := range strings.SplitSeq(raw, ",") {
		hash := strings.ToLower(strings.TrimSpace(entry))
		if hash == "" {
			continue
		}
		if !inviteHashPattern.MatchString(hash) {
			return nil, errors.New("EARN_MAX_INVITE_CODE_HASHES has a malformed entry")
		}
		var sum [sha256.Size]byte
		hex.Decode(sum[:], []byte(hash))
		codes = append(codes, sum)
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
	ok := 0
	for _, allowed := range c {
		ok |= subtle.ConstantTimeCompare(sum[:], allowed[:])
	}
	return hex.EncodeToString(sum[:]), ok == 1
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

func (h earnMaxRoutes) admit() error {
	if h.cfg.Cluster != Mainnet {
		return h.fail(http.StatusConflict, "unsupported_cluster", "Earn MAX requires mainnet.")
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

const maxJSONBody = 1 << 20

type rawJSON struct{ data []byte }

func (r *rawJSON) Resolve(ctx huma.Context) []error {
	r.data, _ = io.ReadAll(io.LimitReader(ctx.BodyReader(), maxJSONBody))
	return nil
}

type redeemRequest struct {
	SessionInput
	Raw rawJSON
}

func documentJSONBody[T any](api huma.API, path string) {
	api.OpenAPI().Paths[path].Post.RequestBody = &huma.RequestBody{Content: map[string]*huma.MediaType{
		"application/json": {Schema: api.OpenAPI().Components.Schemas.Schema(reflect.TypeFor[T](), true, "")},
	}}
}

func inviteCode(raw []byte) *string {
	var body struct {
		Code *string `json:"code"`
	}
	if json.Unmarshal(raw, &body) != nil {
		return nil
	}
	return body.Code
}

func newEarnMaxRoutes(auth *Auth, appDB *pgxpool.Pool, cfg EarnMax, release string) earnMaxRoutes {
	if release == "" {
		release = "unknown"
	}
	return earnMaxRoutes{auth: auth, appDB: appDB, cfg: cfg, headers: EarnMaxHeaders{earnMaxContract, release}, tries: newInviteAttempts()}
}

func (h earnMaxRoutes) registerReads(api huma.API) {
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
		if err := h.admit(); err != nil {
			return nil, err
		}
		summary, err := h.cfg.Reader.Summary(ctx, p.Settings)
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
		if err := h.admit(); err != nil {
			return nil, err
		}
		activity, err := h.cfg.Reader.Activity(ctx, p.Settings)
		if err != nil {
			return nil, h.unavailable("activity", err)
		}
		return &activityOutput{h.headers, EarnMaxActivity(activity)}, nil
	})
	huma.Register(api, huma.Operation{
		OperationID: "get-earn-max-invite",
		Method:      http.MethodGet,
		Path:        invitePath,
		Summary:     "Whether the signed-in wallet has redeemed an Earn MAX invite",
		Responses:   errs(401, 409, 503),
	}, func(ctx context.Context, in *sessionRequest) (*inviteOutput, error) {
		_, err := h.auth.Principal(ctx, in.SessionInput)
		if err != nil {
			return nil, h.refused(err)
		}
		return &inviteOutput{h.headers, InviteStatus{Redeemed: true}}, nil
	})
}

type apyOutput struct {
	Body earnmax.EarnMaxApy
}

func (h earnMaxRoutes) registerPublicAPY(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "get-earn-max-apy",
		Method:      http.MethodGet,
		Path:        "/api/earn-max/apy",
		Summary:     "Earn MAX trailing 7-day realized APY and its history",
	}, func(ctx context.Context, _ *struct{}) (*apyOutput, error) {
		return &apyOutput{h.cfg.Reader.PublicAPY()}, nil
	})
}

const invitePath = "/api/smart-accounts/earn-max/invite"

func (h earnMaxRoutes) registerRedeem(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "post-earn-max-invite",
		Method:      http.MethodPost,
		Path:        invitePath,
		Summary:     "Redeem an Earn MAX invite code for the signed-in wallet",
		Responses:   errorResponses[routeError](api, 400, 401, 409, 429, 503),
	}, func(ctx context.Context, in *redeemRequest) (*inviteOutput, error) {
		p, err := h.auth.Principal(ctx, in.SessionInput)
		if err != nil {
			return nil, h.refused(err)
		}
		if !h.tries.consume(p.Wallet.String(), time.Now()) {
			return nil, h.fail(http.StatusTooManyRequests, "rate_limited", "")
		}
		hash, ok := h.cfg.Invites.admit(inviteCode(in.Raw.data))
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
	documentJSONBody[InviteRequest](api, invitePath)
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
	inviteSweepEvery  = time.Minute
)

type inviteAttempts struct {
	mu      sync.Mutex
	seen    map[string]inviteWindowCount
	sweepAt time.Time
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
	if !now.Before(a.sweepAt) {
		for k, v := range a.seen {
			if !v.resetAt.After(now) {
				delete(a.seen, k)
			}
		}
		a.sweepAt = now.Add(inviteSweepEvery)
	}
	a.seen[key] = inviteWindowCount{count: 1, resetAt: now.Add(inviteWindow)}
	return true
}
