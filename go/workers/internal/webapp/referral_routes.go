package webapp

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/referral"
)

var earnReferralsFamily = earnFamily{
	name:      "referrals",
	appGrants: []Grant{TableGrant("app_users", "SELECT")},
	yieldGrants: []Grant{
		TableGrant("loyal_yield.referral_codes", "SELECT"),
		TableGrant("loyal_yield.referral_codes", "INSERT"),
		TableGrant("loyal_yield.referral_bindings", "SELECT"),
		TableGrant("loyal_yield.referral_bindings", "INSERT"),
		TableGrant("loyal_yield.user_yield_positions", "SELECT"),
		TableGrant("loyal_yield.earn_max_cash_flows", "SELECT"),
	},
	ready:    func(h earnRoutes) bool { return h.signedIn() && h.YieldDB != nil },
	register: earnRoutes.registerReferrals,
}

var referralLookupBudget = budget{every: 10 * time.Second, burst: 60}

const (
	referralsPath       = "/api/referrals"
	referralClaimPath   = "/api/referrals/claim"
	defaultInviteBase   = "https://next.askloyal.com"
	referralsDownCode   = "referrals_unavailable"
	referralsDownReason = "Referrals are temporarily unavailable."
)

type ReferralFriend struct {
	Label          string                `json:"label"`
	Status         referral.FriendStatus `json:"status" enum:"joined,deposited,withdrawn"`
	DepositedMicro int64                 `json:"deposited_micro"`
	BoundAt        time.Time             `json:"bound_at"`
}

type ReferralCounts struct {
	Joined    int `json:"joined"`
	Deposited int `json:"deposited"`
	Withdrawn int `json:"withdrawn"`
}

type Referrals struct {
	Code           string           `json:"code"`
	Link           string           `json:"link"`
	CanClaim       bool             `json:"can_claim"`
	ClosesAt       time.Time        `json:"closes_at"`
	Counts         ReferralCounts   `json:"counts"`
	DepositedMicro int64            `json:"deposited_micro"`
	Friends        []ReferralFriend `json:"friends" nullable:"false"`
}

type ReferralClaimRequest struct {
	Code   string          `json:"code"`
	Source referral.Source `json:"source" enum:"link,typed,install_referrer"`
}

type ReferralClaim struct {
	Bound   bool            `json:"bound,omitempty"`
	Refusal referral.Result `json:"refusal,omitempty" enum:"unknown_code,own_code,already_bound,referrer_newer,window_closed"`
}

type ReferralCode struct {
	Valid bool `json:"valid"`
}

type referralsOutput struct{ Body Referrals }

type referralClaimOutput struct{ Body ReferralClaim }

type referralCodeInput struct {
	Code   string `path:"code"`
	client string
}

func (i *referralCodeInput) Resolve(ctx huma.Context) []error {
	r, _ := humachi.Unwrap(ctx)
	i.client = clientIP(r)
	return nil
}

type referralCodeOutput struct{ Body ReferralCode }

type referralRoutes struct {
	auth       *Auth
	store      *referral.Store
	inviteBase string
	tries      *inviteAttempts
	lookups    *limiters
}

func (h earnRoutes) registerReferrals(api huma.API) {
	routes := referralRoutes{
		auth:       h.auth,
		store:      referral.NewStore(h.YieldDB, h.appDB),
		inviteBase: strings.TrimRight(cmp.Or(os.Getenv("LOYAL_INVITE_BASE_URL"), defaultInviteBase), "/"),
		tries:      newInviteAttempts(),
		lookups:    newLimiters(),
	}
	huma.Register(api, huma.Operation{
		OperationID: "get-referrals",
		Method:      http.MethodGet,
		Path:        referralsPath,
		Summary:     "The signed-in user's referral code, link, claim window and invited friends",
		Responses:   errorResponses[routeError](api, 401, 403, 503),
	}, routes.referrals)
	huma.Register(api, huma.Operation{
		OperationID: "post-referrals-claim",
		Method:      http.MethodPost,
		Path:        referralClaimPath,
		Summary:     "Bind the signed-in user to the inviter who owns a referral code",
		Responses:   errorResponses[routeError](api, 400, 401, 403, 429, 503),
	}, routes.claim)
	documentJSONBody[ReferralClaimRequest](api, referralClaimPath)
	huma.Register(api, huma.Operation{
		OperationID: "get-referral-code",
		Method:      http.MethodGet,
		Path:        "/api/referrals/codes/{code}",
		Summary:     "Whether a referral code exists",
		Responses:   errorResponses[routeError](api, 400, 429, 503),
	}, routes.code)
}

func (h referralRoutes) user(ctx context.Context, in SessionInput) (referral.User, error) {
	session, err := h.auth.Session(in)
	if refusal, ok := authRefusal(err); ok {
		return referral.User{}, refusal
	}
	if err != nil {
		return referral.User{}, earnUnavailable("referrals session", err, referralsDownCode, referralsDownReason)
	}
	if err := admitTestWallet(session.Wallet); err != nil {
		return referral.User{}, err
	}
	user, err := h.store.User(ctx, session.Wallet.String())
	if errors.Is(err, referral.ErrNoUser) {
		return referral.User{}, earnError(Unauthenticated.Status(), Unauthenticated.Code(), authMessage(Unauthenticated))
	}
	if err != nil {
		return referral.User{}, earnUnavailable("referrals user", err, referralsDownCode, referralsDownReason)
	}
	return user, nil
}

func (h referralRoutes) referrals(ctx context.Context, in *sessionRequest) (*referralsOutput, error) {
	user, err := h.user(ctx, in.SessionInput)
	if err != nil {
		return nil, err
	}
	code, err := h.store.EnsureCode(ctx, user.ID)
	if err != nil {
		return nil, earnUnavailable("referrals code", err, referralsDownCode, referralsDownReason)
	}
	bound, err := h.store.Bound(ctx, user)
	if err != nil {
		return nil, earnUnavailable("referrals binding", err, referralsDownCode, referralsDownReason)
	}
	friends, err := h.store.Friends(ctx, user.ID)
	if err != nil {
		return nil, earnUnavailable("referrals friends", err, referralsDownCode, referralsDownReason)
	}
	body := Referrals{
		Code:     string(code),
		Link:     h.inviteBase + "/i/" + string(code),
		CanClaim: !bound && !time.Now().After(user.ClaimClosesAt()),
		ClosesAt: user.ClaimClosesAt(),
		Friends:  make([]ReferralFriend, len(friends)),
	}
	for i, f := range friends {
		body.Friends[i] = ReferralFriend{Label: f.Label, Status: f.Status, DepositedMicro: f.DepositedMicro, BoundAt: f.BoundAt}
		body.DepositedMicro += f.DepositedMicro
		switch f.Status {
		case referral.Joined:
			body.Counts.Joined++
		case referral.Deposited:
			body.Counts.Deposited++
		case referral.Withdrawn:
			body.Counts.Withdrawn++
		}
	}
	return &referralsOutput{body}, nil
}

func (h referralRoutes) claim(ctx context.Context, in *redeemRequest) (*referralClaimOutput, error) {
	user, err := h.user(ctx, in.SessionInput)
	if err != nil {
		return nil, err
	}
	if !h.tries.consume(user.Wallet, time.Now()) {
		return nil, earnError(http.StatusTooManyRequests, "rate_limited", "")
	}
	var body ReferralClaimRequest
	if json.Unmarshal(in.Raw.data, &body) != nil {
		return nil, earnError(http.StatusBadRequest, "invalid_body", "")
	}
	code, err := referral.ParseCode(body.Code)
	if err != nil {
		return nil, earnError(http.StatusBadRequest, "invalid_code", "")
	}
	source, err := referral.ParseSource(string(body.Source))
	if err != nil {
		return nil, earnError(http.StatusBadRequest, "invalid_source", "")
	}
	result, err := h.store.Bind(ctx, user, code, source)
	if err != nil {
		return nil, earnUnavailable("referrals bind", err, referralsDownCode, referralsDownReason)
	}
	if result == referral.Bound {
		return &referralClaimOutput{ReferralClaim{Bound: true}}, nil
	}
	return &referralClaimOutput{ReferralClaim{Refusal: result}}, nil
}

func (h referralRoutes) code(ctx context.Context, in *referralCodeInput) (*referralCodeOutput, error) {
	now := time.Now()
	if !h.lookups.get(limiterKey{"referral-code", "client", in.client}, referralLookupBudget, now).AllowN(now, 1) {
		return nil, earnError(http.StatusTooManyRequests, "rate_limited", "")
	}
	code, err := referral.ParseCode(in.Code)
	if err != nil {
		return nil, earnError(http.StatusBadRequest, "invalid_code", "")
	}
	valid, err := h.store.CodeExists(ctx, code)
	if err != nil {
		return nil, earnUnavailable("referrals lookup", err, referralsDownCode, referralsDownReason)
	}
	return &referralCodeOutput{ReferralCode{Valid: valid}}, nil
}
