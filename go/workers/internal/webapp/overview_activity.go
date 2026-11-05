package webapp

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/big"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"encoding/json"

	"github.com/danielgtaylor/huma/v2"
	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"
	"golang.org/x/sync/errgroup"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/jupiter"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
)

const (
	overviewActivityLimit      = 10
	overviewActivityTTL        = 15 * time.Second
	overviewActivityMinBatch   = 30
	overviewActivityPages      = 3
	overviewActivityReads      = 10
	overviewDustLamports       = 100_000
	overviewDustTokenThreshold = 0.0001
)

var (
	overviewInvalidAccountIndex     = []byte(`{"error":{"code":"invalid_account_index","message":"A valid vault account index is required."}}`)
	overviewActivityRateLimitedBody = []byte(`{"error":{"code":"rpc_rate_limited","message":"Vault activity is temporarily rate limited. Please wait a moment and try again."}}`)
	overviewJSDecimal               = regexp.MustCompile(`^[+-]?(\d+\.?\d*|\.\d+)([eE][+-]?\d+)?$`)
	overviewEarnPrograms            = []solana.PublicKey{squads.ProgramID, kamino.ProgramID, kamino.FarmsProgramID}
)

type overviewActivityInput struct {
	SessionInput
	AccountIndex    string `query:"accountIndex"`
	ForceRefresh    string `query:"forceRefresh"`
	hasAccountIndex bool
}

func (i *overviewActivityInput) Resolve(ctx huma.Context) []error {
	u := ctx.URL()
	_, i.hasAccountIndex = u.Query()["accountIndex"]
	return i.SessionInput.Resolve(ctx)
}

type overviewActivityPage struct {
	Activities []any  `json:"activities"`
	NextCursor string `json:"nextCursor,omitempty"`
}

type overviewActivityToken struct {
	Mint     string `json:"mint"`
	Amount   string `json:"amount"`
	Decimals int    `json:"decimals"`
}

type overviewSolTransfer struct {
	Type              string  `json:"type"`
	Signature         string  `json:"signature"`
	Slot              uint64  `json:"slot"`
	Timestamp         *int64  `json:"timestamp"`
	Direction         string  `json:"direction"`
	AmountLamports    int64   `json:"amountLamports"`
	NetChangeLamports int64   `json:"netChangeLamports"`
	FeeLamports       uint64  `json:"feeLamports"`
	Status            string  `json:"status"`
	Counterparty      *string `json:"counterparty,omitempty"`
}

type overviewTokenTransfer struct {
	Type         string                `json:"type"`
	Signature    string                `json:"signature"`
	Slot         uint64                `json:"slot"`
	Timestamp    *int64                `json:"timestamp"`
	Direction    string                `json:"direction"`
	Token        overviewActivityToken `json:"token"`
	FeeLamports  uint64                `json:"feeLamports"`
	Status       string                `json:"status"`
	Counterparty *string               `json:"counterparty,omitempty"`
}

type overviewSwap struct {
	Type           string                `json:"type"`
	Signature      string                `json:"signature"`
	Slot           uint64                `json:"slot"`
	Timestamp      *int64                `json:"timestamp"`
	Direction      string                `json:"direction"`
	FromToken      overviewActivityToken `json:"fromToken"`
	ToToken        overviewActivityToken `json:"toToken"`
	AmountLamports int64                 `json:"amountLamports"`
	FeeLamports    uint64                `json:"feeLamports"`
	Status         string                `json:"status"`
	Counterparty   string                `json:"counterparty"`
}

type overviewProgramAction struct {
	Type              string                 `json:"type"`
	Action            string                 `json:"action"`
	Signature         string                 `json:"signature"`
	Slot              uint64                 `json:"slot"`
	Timestamp         *int64                 `json:"timestamp"`
	Direction         string                 `json:"direction"`
	AmountLamports    int64                  `json:"amountLamports"`
	NetChangeLamports int64                  `json:"netChangeLamports"`
	FeeLamports       uint64                 `json:"feeLamports"`
	Status            string                 `json:"status"`
	Counterparty      string                 `json:"counterparty"`
	Token             *overviewActivityToken `json:"token,omitempty"`
}

func overviewAccountIndex(in *overviewActivityInput) (int, bool) {
	if !in.hasAccountIndex {
		return 0, false
	}
	value, ok := overviewJSNumber(in.AccountIndex)
	if !ok || math.IsInf(value, 0) || value != math.Trunc(value) || value < 0 || value > 255 {
		return 0, false
	}
	return int(value), true
}

func overviewJSNumber(raw string) (float64, bool) {
	s := jsTrim(raw)
	if s == "" {
		return 0, true
	}
	if len(s) > 2 && s[0] == '0' {
		if base, ok := map[byte]int{'x': 16, 'X': 16, 'o': 8, 'O': 8, 'b': 2, 'B': 2}[s[1]]; ok {
			value, err := strconv.ParseUint(s[2:], base, 64)
			if errors.Is(err, strconv.ErrRange) {
				return math.Inf(1), true
			}
			return float64(value), err == nil
		}
	}
	switch s {
	case "Infinity", "+Infinity":
		return math.Inf(1), true
	case "-Infinity":
		return math.Inf(-1), true
	}
	if !overviewJSDecimal.MatchString(s) {
		return 0, false
	}
	value, err := strconv.ParseFloat(s, 64)
	return value, err == nil || errors.Is(err, strconv.ErrRange)
}

func (o *overviewRoutes) serveActivity(ctx context.Context, in *overviewActivityInput) *overviewOutput {
	settings, refused := o.principal(ctx, in.SessionInput, "smart-account-vault-activity")
	if refused != nil {
		return refused
	}
	index, ok := overviewAccountIndex(in)
	if !ok {
		return overviewJSON(http.StatusBadRequest, overviewInvalidAccountIndex)
	}
	vault, _, err := squads.SmartAccountAddress(settings, uint8(index))
	if err != nil {
		return overviewFailed()
	}
	key := fmt.Sprintf("%s:vault-activity:%d", settings, index)
	page, err := overviewLoad(ctx, o.cache, key, in.ForceRefresh == "1", overviewActivityTTL, func(ctx context.Context) (overviewActivityPage, error) {
		return o.vaultActivity(ctx, vault, overviewActivityLimit)
	})
	var limited *overviewRateLimited
	if errors.As(err, &limited) {
		out := overviewJSON(http.StatusTooManyRequests, overviewActivityRateLimitedBody)
		out.RetryAfter = strconv.Itoa(limited.retryAfterSeconds)
		return out
	}
	if err != nil {
		slog.Warn("[smart-account-overview] smart-account-vault-activity.failed", "settingsPda", settings.String(), "accountIndex", index, "error", chain.Scrub(err.Error()))
		return overviewFailed()
	}
	body, err := json.Marshal(struct {
		AccountIndex int                  `json:"accountIndex"`
		Activity     overviewActivityPage `json:"activity"`
	}{index, page})
	if err != nil {
		slog.Error("[smart-account-overview] encode vault activity", "error", err)
		return overviewFailed()
	}
	return overviewJSON(http.StatusOK, body)
}

func (o *overviewRoutes) vaultActivity(ctx context.Context, vault solana.PublicKey, limit int) (overviewActivityPage, error) {
	page := overviewActivityPage{Activities: []any{}}
	var before solana.Signature
	for range overviewActivityPages {
		signatures, err := o.Chain.History(ctx, vault, max(limit*2, overviewActivityMinBatch), before, rpc.CommitmentConfirmed, 0)
		if err != nil {
			return overviewActivityPage{}, fmt.Errorf("read vault signatures: %w", err)
		}
		if len(signatures) == 0 {
			break
		}
		executions := make([]*chain.Execution, len(signatures))
		reads, readCtx := errgroup.WithContext(ctx)
		reads.SetLimit(overviewActivityReads)
		for i, signed := range signatures {
			reads.Go(func() error {
				execution, err := o.Chain.Execution(readCtx, signed.Signature, rpc.CommitmentConfirmed)
				if errors.Is(err, chain.ErrNotFound) {
					return nil
				}
				if err != nil {
					return fmt.Errorf("read vault transaction: %w", err)
				}
				executions[i] = &execution
				return nil
			})
		}
		if err := reads.Wait(); err != nil {
			return overviewActivityPage{}, err
		}
		for i, execution := range executions {
			if execution == nil {
				continue
			}
			if activity := overviewNormalize(*execution, signatures[i].Signature.String(), vault); activity != nil {
				page.Activities = append(page.Activities, activity)
			}
		}
		before = signatures[len(signatures)-1].Signature
		page.NextCursor = before.String()
		if len(page.Activities) >= limit {
			break
		}
	}
	page.Activities = page.Activities[:min(limit, len(page.Activities))]
	return page, nil
}

type overviewInstruction struct {
	program  solana.PublicKey
	accounts []solana.PublicKey
	data     []byte
}

type overviewParsed struct {
	program, kind string
	info          map[string]solana.PublicKey
}

func overviewInstructions(execution chain.Execution) []overviewInstruction {
	keys := execution.Keys
	compiled := func(programIndex uint16, accountIndexes []uint16, data []byte) (overviewInstruction, bool) {
		if int(programIndex) >= len(keys) {
			return overviewInstruction{}, false
		}
		ix := overviewInstruction{program: keys[programIndex], data: data}
		for _, index := range accountIndexes {
			if int(index) >= len(keys) {
				return overviewInstruction{}, false
			}
			ix.accounts = append(ix.accounts, keys[index])
		}
		return ix, true
	}
	var out []overviewInstruction
	for _, ix := range execution.Transaction.Message.Instructions {
		if parsed, ok := compiled(ix.ProgramIDIndex, ix.Accounts, ix.Data); ok {
			out = append(out, parsed)
		}
	}
	for _, group := range execution.Inner {
		for _, ix := range group.Instructions {
			if parsed, ok := compiled(ix.ProgramIDIndex, ix.Accounts, ix.Data); ok {
				out = append(out, parsed)
			}
		}
	}
	return out
}

func (ix overviewInstruction) parsed() overviewParsed {
	a, d := ix.accounts, ix.data
	switch ix.program {
	case solana.SystemProgramID:
		if len(d) < 4 || len(a) < 2 {
			return overviewParsed{}
		}
		switch tag := binary.LittleEndian.Uint32(d); {
		case tag == 0 && len(d) >= 52:
			return overviewParsed{"system", "createAccount", map[string]solana.PublicKey{"source": a[0], "newAccount": a[1]}}
		case tag == 2 && len(d) >= 12:
			return overviewParsed{"system", "transfer", map[string]solana.PublicKey{"source": a[0], "destination": a[1]}}
		}
		return overviewParsed{program: "system"}
	case solana.TokenProgramID, solana.Token2022ProgramID:
		program := map[bool]string{true: "spl-token", false: "spl-token-2022"}[ix.program == solana.TokenProgramID]
		switch {
		case len(d) >= 9 && d[0] == 3 && len(a) >= 3:
			return overviewParsed{program, "transfer", map[string]solana.PublicKey{"source": a[0], "destination": a[1]}}
		case len(d) >= 10 && d[0] == 12 && len(a) >= 4:
			return overviewParsed{program, "transferChecked", map[string]solana.PublicKey{"source": a[0], "destination": a[2]}}
		}
		return overviewParsed{program: program}
	case solana.SPLAssociatedTokenAccountProgramID:
		if len(a) < 6 || len(d) > 1 || len(d) == 1 && d[0] > 1 {
			return overviewParsed{}
		}
		kind := "create"
		if len(d) == 1 && d[0] == 1 {
			kind = "createIdempotent"
		}
		return overviewParsed{"spl-associated-token-account", kind, map[string]solana.PublicKey{"source": a[0], "account": a[1], "wallet": a[2], "mint": a[3]}}
	}
	return overviewParsed{}
}

type overviewTokenChange struct {
	mint      string
	decimals  int
	abs       *big.Int
	direction string
}

func overviewTokenChanges(execution chain.Execution, wallet solana.PublicKey) []overviewTokenChange {
	type sum struct {
		raw      *big.Int
		decimals int
	}
	var mints []string
	sums := [2]map[string]*sum{{}, {}}
	for side, balances := range [2]map[solana.PublicKey]chain.TokenBalance{execution.Pre, execution.Post} {
		for _, key := range execution.Keys {
			balance, ok := balances[key]
			if !ok || balance.Owner != wallet {
				continue
			}
			mint := balance.Mint.String()
			if !slices.Contains(mints, mint) {
				mints = append(mints, mint)
			}
			if s := sums[side][mint]; s != nil {
				s.raw.Add(s.raw, new(big.Int).SetUint64(balance.Amount))
				continue
			}
			sums[side][mint] = &sum{new(big.Int).SetUint64(balance.Amount), int(balance.Decimals)}
		}
	}
	var changes []overviewTokenChange
	for _, mint := range mints {
		pre, post := sums[0][mint], sums[1][mint]
		delta, decimals := new(big.Int), 0
		if post != nil {
			delta.Set(post.raw)
			decimals = post.decimals
		} else {
			decimals = pre.decimals
		}
		if pre != nil {
			delta.Sub(delta, pre.raw)
		}
		if delta.Sign() == 0 {
			continue
		}
		direction := "in"
		if delta.Sign() < 0 {
			direction = "out"
		}
		changes = append(changes, overviewTokenChange{mint: mint, decimals: decimals, abs: delta.Abs(delta), direction: direction})
	}
	return changes
}

func overviewActivityAmount(abs *big.Int, decimals, maxFraction int) string {
	if decimals <= 0 {
		return abs.String()
	}
	digits := min(decimals, maxFraction)
	base := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)
	integer, remainder := new(big.Int).QuoRem(abs, base, new(big.Int))
	fraction := new(big.Int).Quo(remainder, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals-digits)), nil))
	text := strings.TrimRight(fmt.Sprintf("%0*s", digits, fraction.String()), "0")
	if text == "" {
		return integer.String()
	}
	return integer.String() + "." + text
}

func (c overviewTokenChange) amount(maxFraction int) overviewActivityToken {
	return overviewActivityToken{Mint: c.mint, Amount: overviewActivityAmount(c.abs, c.decimals, maxFraction), Decimals: c.decimals}
}

func overviewNormalize(execution chain.Execution, signature string, wallet solana.PublicKey) any {
	walletIndex := slices.Index(execution.Keys, wallet)
	isSigner := walletIndex >= 0 && walletIndex < int(execution.Transaction.Message.Header.NumRequiredSignatures)
	instructions := overviewInstructions(execution)
	parsed := make([]overviewParsed, len(instructions))
	for i, ix := range instructions {
		parsed[i] = ix.parsed()
	}

	var system *overviewParsed
	for i, p := range parsed {
		destination, ok := p.info["destination"]
		if !ok {
			destination = p.info["newAccount"]
		}
		if p.program == "system" && (p.kind == "transfer" || p.kind == "createAccount") && (p.info["source"] == wallet || destination == wallet) {
			system = &parsed[i]
			break
		}
	}

	changes := overviewTokenChanges(execution, wallet)
	var change *overviewTokenChange
	for i := range changes {
		if change == nil || changes[i].abs.Cmp(change.abs) > 0 {
			change = &changes[i]
		}
	}
	if walletIndex < 0 && change == nil {
		return nil
	}
	var net int64
	if walletIndex >= 0 {
		net = int64(execution.PostLamports[walletIndex]) - int64(execution.PreLamports[walletIndex])
	}
	if !isSigner && change == nil && abs64(net) < overviewDustLamports {
		return nil
	}
	if change != nil && !isSigner && change.direction == "in" && change.abs.Sign() > 0 {
		raw, _ := new(big.Float).SetInt(change.abs).Float64()
		if raw/math.Pow(10, float64(max(0, change.decimals))) < overviewDustTokenThreshold {
			return nil
		}
	}
	if net == 0 && change == nil {
		return nil
	}

	var timestamp *int64
	if execution.BlockTime != nil && execution.BlockTime.Unix() != 0 {
		ms := execution.BlockTime.Unix() * 1000
		timestamp = &ms
	}
	status := "success"
	if execution.Err != nil {
		status = "failed"
	}
	direction := "out"
	if net > 0 {
		direction = "in"
	}
	amountLamports := abs64(net)
	var counterparty *string
	if system != nil {
		field := map[string]string{"in": "source", "out": "destination"}[direction]
		if key, ok := system.info[field]; ok {
			text := key.String()
			counterparty = &text
		}
	}

	jupiterSwap := len(changes) > 0 && slices.ContainsFunc(instructions, func(ix overviewInstruction) bool { return ix.program == jupiter.ProgramID })
	if jupiterSwap || isSigner && len(changes) > 1 {
		in := slices.IndexFunc(changes, func(c overviewTokenChange) bool { return c.direction == "in" })
		out := slices.IndexFunc(changes, func(c overviewTokenChange) bool { return c.direction == "out" })
		swap := overviewSwap{Type: "swap", Signature: signature, Slot: execution.Slot, Timestamp: timestamp, Direction: "out", AmountLamports: amountLamports, FeeLamports: execution.Fee, Status: status, Counterparty: "Swap"}
		sol := func(lamports int64) overviewActivityToken {
			return overviewActivityToken{Mint: overviewSOLMint, Amount: overviewActivityAmount(big.NewInt(abs64(lamports)), overviewSOLDecimals, 6), Decimals: overviewSOLDecimals}
		}
		switch {
		case in >= 0 && out >= 0:
			swap.FromToken, swap.ToToken = changes[out].amount(4), changes[in].amount(4)
			return swap
		case net < -overviewDustLamports && in >= 0:
			swap.FromToken, swap.ToToken = sol(net), changes[in].amount(6)
			return swap
		case out >= 0 && net > overviewDustLamports:
			swap.FromToken, swap.ToToken = changes[out].amount(6), sol(net)
			return swap
		}
	}

	if action, counter, ok := overviewEarn(instructions, parsed, execution.Logs, wallet, change); ok {
		activity := overviewProgramAction{Type: "program_action", Action: action, Signature: signature, Slot: execution.Slot, Timestamp: timestamp, Direction: direction, AmountLamports: amountLamports, NetChangeLamports: net, FeeLamports: execution.Fee, Status: status, Counterparty: counter}
		if change != nil {
			token := change.amount(4)
			activity.Direction, activity.AmountLamports, activity.Token = change.direction, 0, &token
		}
		return activity
	}

	if change != nil {
		if counter := overviewTokenCounterparty(execution, parsed, wallet, *change); counter != "" {
			counterparty = &counter
		}
		return overviewTokenTransfer{Type: "token_transfer", Signature: signature, Slot: execution.Slot, Timestamp: timestamp, Direction: change.direction, Token: change.amount(4), FeeLamports: execution.Fee, Status: status, Counterparty: counterparty}
	}
	return overviewSolTransfer{Type: "sol_transfer", Signature: signature, Slot: execution.Slot, Timestamp: timestamp, Direction: direction, AmountLamports: amountLamports, NetChangeLamports: net, FeeLamports: execution.Fee, Status: status, Counterparty: counterparty}
}

func overviewEarn(instructions []overviewInstruction, parsed []overviewParsed, logs []string, wallet solana.PublicKey, change *overviewTokenChange) (action, counterparty string, ok bool) {
	earn, kaminoLend := false, false
	for _, ix := range instructions {
		if slices.Contains(overviewEarnPrograms, ix.program) {
			earn = true
			kaminoLend = kaminoLend || ix.program == kamino.ProgramID
		}
	}
	if !earn && !slices.ContainsFunc(parsed, func(p overviewParsed) bool {
		owner, hasOwner := p.info["wallet"]
		return p.program == "spl-associated-token-account" && hasOwner && owner != wallet && p.info["source"] == wallet
	}) {
		return "", "", false
	}
	switch {
	case change != nil && change.direction == "in":
		action = "earn_withdraw"
	case change != nil:
		action = "earn_deposit"
	case slices.ContainsFunc(logs, func(log string) bool { return strings.Contains(log, "Withdraw") }):
		action = "earn_withdraw"
	default:
		action = "earn_deposit"
	}
	if kaminoLend {
		return action, "Kamino Lend", true
	}
	return action, "Earn", true
}

func overviewTokenCounterparty(execution chain.Execution, parsed []overviewParsed, wallet solana.PublicKey, change overviewTokenChange) string {
	owners := map[solana.PublicKey]solana.PublicKey{}
	walletAccounts := map[solana.PublicKey]bool{}
	for _, balances := range []map[solana.PublicKey]chain.TokenBalance{execution.Pre, execution.Post} {
		for account, balance := range balances {
			if balance.Mint.String() != change.mint {
				continue
			}
			owners[account] = balance.Owner
			if balance.Owner == wallet {
				walletAccounts[account] = true
			}
		}
	}
	for _, p := range parsed {
		if p.program != "spl-token" && p.program != "spl-token-2022" || p.kind != "transfer" && p.kind != "transferChecked" {
			continue
		}
		source, destination := p.info["source"], p.info["destination"]
		switch {
		case change.direction == "out" && walletAccounts[source]:
			return overviewOwnerOr(owners[destination], destination).String()
		case change.direction == "in" && walletAccounts[destination]:
			return overviewOwnerOr(owners[source], source).String()
		}
	}
	return ""
}

func overviewOwnerOr(owner, fallback solana.PublicKey) solana.PublicKey {
	if owner.IsZero() {
		return fallback
	}
	return owner
}
