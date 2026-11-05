package webapp

import (
	"cmp"
	"context"
	"fmt"
	"math/big"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
)

type overviewSigner struct {
	Address          string   `json:"address"`
	Scope            string   `json:"scope"`
	ConsensusAddress string   `json:"consensusAddress"`
	Permissions      []string `json:"permissions"`
	PermissionMask   uint8    `json:"permissionMask"`
	Lamports         *uint64  `json:"lamports"`
	CanInitiate      bool     `json:"canInitiate"`
	CanVote          bool     `json:"canVote"`
	CanExecute       bool     `json:"canExecute"`
	Threshold        uint16   `json:"threshold"`
	TimeLock         uint32   `json:"timeLock"`
	PolicyAddress    *string  `json:"policyAddress"`
	PolicySeed       *string  `json:"policySeed"`
}

type overviewVaultBase struct {
	AccountIndex int    `json:"accountIndex"`
	Address      string `json:"address"`
}

type overviewBase struct {
	ProgramID             string              `json:"programId"`
	SettingsPda           string              `json:"settingsPda"`
	Threshold             uint16              `json:"threshold"`
	TimeLock              uint32              `json:"timeLock"`
	TransactionIndex      string              `json:"transactionIndex"`
	StaleTransactionIndex string              `json:"staleTransactionIndex"`
	CanonicalVaultAddress string              `json:"canonicalVaultAddress"`
	AccountUtilization    uint8               `json:"accountUtilization"`
	Signers               []overviewSigner    `json:"signers"`
	Vaults                []overviewVaultBase `json:"vaults"`
	FetchedAt             int64               `json:"fetchedAt"`
}

type overviewPolicy struct {
	Address               string           `json:"address"`
	SettingsPda           string           `json:"settingsPda"`
	Seed                  string           `json:"seed"`
	Threshold             uint16           `json:"threshold"`
	TimeLock              uint32           `json:"timeLock"`
	TransactionIndex      string           `json:"transactionIndex"`
	StaleTransactionIndex string           `json:"staleTransactionIndex"`
	State                 string           `json:"state"`
	AccountIndex          *uint8           `json:"accountIndex"`
	Mint                  *string          `json:"mint"`
	Signers               []overviewSigner `json:"signers"`
}

type overviewSpendingLimit struct {
	Address                     string   `json:"address"`
	SettingsPda                 string   `json:"settingsPda"`
	Seed                        string   `json:"seed"`
	AccountIndex                uint8    `json:"accountIndex"`
	Mint                        string   `json:"mint"`
	Symbol                      string   `json:"symbol"`
	Decimals                    int      `json:"decimals"`
	AmountRaw                   string   `json:"amountRaw"`
	RemainingAmountRaw          string   `json:"remainingAmountRaw"`
	EffectiveRemainingAmountRaw string   `json:"effectiveRemainingAmountRaw"`
	MaxPerUseRaw                string   `json:"maxPerUseRaw"`
	AmountUi                    string   `json:"amountUi"`
	RemainingAmountUi           string   `json:"remainingAmountUi"`
	AmountUsd                   *float64 `json:"amountUsd"`
	RemainingAmountUsd          *float64 `json:"remainingAmountUsd"`
	Period                      string   `json:"period"`
	PeriodSeconds               *int64   `json:"periodSeconds"`
	PeriodLabel                 string   `json:"periodLabel"`
	AccumulateUnused            bool     `json:"accumulateUnused"`
	LastReset                   int64    `json:"lastReset"`
	NextReset                   *int64   `json:"nextReset"`
	Expiration                  *int64   `json:"expiration"`
	IsExpired                   bool     `json:"isExpired"`
	Signers                     []string `json:"signers"`
	Destinations                []string `json:"destinations"`
}

type overviewPolicies struct {
	Signers        []overviewSigner        `json:"signers"`
	Policies       []overviewPolicy        `json:"policies"`
	SpendingLimits []overviewSpendingLimit `json:"spendingLimits"`
}

func (o *overviewRoutes) settingsAccount(ctx context.Context, settings solana.PublicKey) (squads.Settings, error) {
	_, accounts, err := o.Chain.Accounts(ctx, []solana.PublicKey{settings}, rpc.CommitmentConfirmed, 0)
	if err != nil {
		return squads.Settings{}, fmt.Errorf("read smart account settings: %w", err)
	}
	if accounts[0] == nil {
		return squads.Settings{}, errOverviewSettingsMissing
	}
	return squads.DecodeSettings(accounts[0])
}

func (o *overviewRoutes) base(ctx context.Context, settingsPda solana.PublicKey) (overviewBase, error) {
	settings, err := o.settingsAccount(ctx, settingsPda)
	if err != nil {
		return overviewBase{}, err
	}
	vaults := make([]overviewVaultBase, 0, int(settings.AccountUtilization)+1)
	for index := 0; index <= int(settings.AccountUtilization); index++ {
		vault, _, err := squads.SmartAccountAddress(settingsPda, uint8(index))
		if err != nil {
			return overviewBase{}, err
		}
		vaults = append(vaults, overviewVaultBase{AccountIndex: index, Address: vault.String()})
	}
	return overviewBase{
		ProgramID:             squads.ProgramID.String(),
		SettingsPda:           settingsPda.String(),
		Threshold:             settings.Threshold,
		TimeLock:              settings.TimeLock,
		TransactionIndex:      strconv.FormatUint(settings.TransactionIndex, 10),
		StaleTransactionIndex: strconv.FormatUint(settings.StaleTransactionIndex, 10),
		CanonicalVaultAddress: vaults[0].Address,
		AccountUtilization:    settings.AccountUtilization,
		Signers:               overviewSigners(settings.Signers, "settings", settingsPda, settings.Threshold, settings.TimeLock, nil, nil),
		Vaults:                vaults,
		FetchedAt:             time.Now().UnixMilli(),
	}, nil
}

func (o *overviewRoutes) policies(ctx context.Context, settingsPda solana.PublicKey) (overviewPolicies, error) {
	settings, err := o.settingsAccount(ctx, settingsPda)
	if err != nil {
		return overviewPolicies{}, err
	}
	var accounts []squadsPolicyEntry
	if settings.TransactionIndex > 0 {
		if accounts, err = o.policyAccounts(ctx, settingsPda); err != nil {
			return overviewPolicies{}, err
		}
	}
	now := time.Now().Unix()
	out := overviewPolicies{
		Signers:        overviewSigners(settings.Signers, "settings", settingsPda, settings.Threshold, settings.TimeLock, nil, nil),
		Policies:       make([]overviewPolicy, 0, len(accounts)),
		SpendingLimits: []overviewSpendingLimit{},
	}
	for _, entry := range accounts {
		out.Policies = append(out.Policies, overviewPolicySnapshot(entry))
		if entry.policy.SpendingLimit != nil {
			out.SpendingLimits = append(out.SpendingLimits, overviewSpendingLimitSnapshot(entry, now))
		}
	}
	slices.SortFunc(out.Policies, func(a, b overviewPolicy) int {
		left, _ := strconv.ParseUint(a.Seed, 10, 64)
		right, _ := strconv.ParseUint(b.Seed, 10, 64)
		return cmp.Compare(left, right)
	})
	slices.SortFunc(out.SpendingLimits, func(a, b overviewSpendingLimit) int { return overviewLocaleCompare(a.Address, b.Address) })
	if len(out.Policies) == 0 {
		return out, nil
	}
	var addresses []solana.PublicKey
	signers := slices.Clone(settings.Signers)
	for _, entry := range accounts {
		signers = append(signers, entry.policy.Signers...)
	}
	for _, signer := range signers {
		if !slices.Contains(addresses, signer.Key) {
			addresses = append(addresses, signer.Key)
		}
	}
	_, balances, err := o.Chain.Accounts(ctx, addresses, rpc.CommitmentConfirmed, 0)
	if err != nil {
		return overviewPolicies{}, fmt.Errorf("read policy signer lamports: %w", err)
	}
	lamports := make(map[string]uint64, len(addresses))
	for i, account := range balances {
		if account != nil {
			lamports[addresses[i].String()] = account.Lamports
		}
	}
	setLamports := func(signers []overviewSigner) {
		for i := range signers {
			balance := lamports[signers[i].Address]
			signers[i].Lamports = &balance
		}
	}
	setLamports(out.Signers)
	for i := range out.Policies {
		setLamports(out.Policies[i].Signers)
	}
	return out, nil
}

type squadsPolicyEntry struct {
	address solana.PublicKey
	policy  squads.PolicyAccount
}

func (o *overviewRoutes) policyAccounts(ctx context.Context, settings solana.PublicKey) ([]squadsPolicyEntry, error) {
	_, accounts, err := o.Chain.ProgramAccounts(ctx, squads.ProgramID, []rpc.RPCFilter{
		{Memcmp: &rpc.RPCFilterMemcmp{Offset: 0, Bytes: squads.PolicyDiscriminator[:]}},
		{Memcmp: &rpc.RPCFilterMemcmp{Offset: 8, Bytes: settings[:]}},
	}, rpc.CommitmentConfirmed, 0)
	if err != nil {
		return nil, fmt.Errorf("scan smart account policies: %w", err)
	}
	entries := make([]squadsPolicyEntry, 0, len(accounts))
	for i := range accounts {
		policy, err := squads.DecodePolicyAccount(&accounts[i])
		if err != nil {
			return nil, fmt.Errorf("decode policy %s: %w", accounts[i].Key, err)
		}
		entries = append(entries, squadsPolicyEntry{address: accounts[i].Key, policy: policy})
	}
	return entries, nil
}

func overviewSigners(signers []squads.Signer, scope string, consensus solana.PublicKey, threshold uint16, timeLock uint32, policy *string, seed *string) []overviewSigner {
	out := make([]overviewSigner, 0, len(signers))
	for _, signer := range signers {
		permissions := []string{}
		for _, p := range [...]struct {
			bit  uint8
			name string
		}{{squads.PermissionInitiate, "initiate"}, {squads.PermissionVote, "vote"}, {squads.PermissionExecute, "execute"}} {
			if signer.Permissions&p.bit == p.bit {
				permissions = append(permissions, p.name)
			}
		}
		out = append(out, overviewSigner{
			Address:          signer.Key.String(),
			Scope:            scope,
			ConsensusAddress: consensus.String(),
			Permissions:      permissions,
			PermissionMask:   signer.Permissions,
			CanInitiate:      slices.Contains(permissions, "initiate"),
			CanVote:          slices.Contains(permissions, "vote"),
			CanExecute:       slices.Contains(permissions, "execute"),
			Threshold:        threshold,
			TimeLock:         timeLock,
			PolicyAddress:    policy,
			PolicySeed:       seed,
		})
	}
	return out
}

func overviewPolicySnapshot(entry squadsPolicyEntry) overviewPolicy {
	p := entry.policy
	address := entry.address.String()
	seed := strconv.FormatUint(p.Seed, 10)
	var mint *string
	if p.SpendingLimit != nil {
		value := p.SpendingLimit.Limit.Mint.String()
		mint = &value
	}
	return overviewPolicy{
		Address:               address,
		SettingsPda:           p.Settings.String(),
		Seed:                  seed,
		Threshold:             p.Threshold,
		TimeLock:              p.TimeLock,
		TransactionIndex:      strconv.FormatUint(p.TransactionIndex, 10),
		StaleTransactionIndex: strconv.FormatUint(p.StaleTransactionIndex, 10),
		State:                 p.Kind.String(),
		AccountIndex:          p.AccountIndex,
		Mint:                  mint,
		Signers:               overviewSigners(p.Signers, "policy", entry.address, p.Threshold, p.TimeLock, &address, &seed),
	}
}

const overviewDaySeconds = 24 * 60 * 60

var overviewPeriods = map[uint8]struct {
	name    string
	seconds int64
}{
	squads.PeriodOneTime: {"one_time", 0},
	squads.PeriodDaily:   {"day", overviewDaySeconds},
	squads.PeriodWeekly:  {"week", 7 * overviewDaySeconds},
	squads.PeriodMonthly: {"month", 30 * overviewDaySeconds},
}

func overviewSpendingLimitPeriod(limit squads.SpendingLimitView) (string, *int64) {
	if period, ok := overviewPeriods[limit.Period]; ok {
		if period.seconds == 0 {
			return period.name, nil
		}
		return period.name, &period.seconds
	}
	for _, period := range []uint8{squads.PeriodDaily, squads.PeriodWeekly, squads.PeriodMonthly} {
		if overviewPeriods[period].seconds == limit.CustomPeriod {
			return overviewPeriods[period].name, &limit.CustomPeriod
		}
	}
	if limit.CustomPeriod > 0 {
		return "custom", &limit.CustomPeriod
	}
	return "custom", nil
}

func overviewSpendingLimitSnapshot(entry squadsPolicyEntry, now int64) overviewSpendingLimit {
	p, sl := entry.policy, entry.policy.SpendingLimit
	amount := new(big.Int).SetUint64(sl.Limit.MaxPerPeriod)
	remaining := new(big.Int).SetUint64(sl.Usage.RemainingInPeriod)
	lastReset := sl.Usage.LastReset
	period, periodSeconds := overviewSpendingLimitPeriod(sl.Limit)
	effective := remaining
	var nextReset *int64
	if periodSeconds != nil {
		reset := *periodSeconds
		if elapsed := now - lastReset; elapsed > reset {
			if sl.Limit.Accumulate {
				effective = new(big.Int).Add(remaining, new(big.Int).Mul(amount, big.NewInt(elapsed/reset)))
			} else {
				effective = amount
			}
		}
		next := lastReset + (max(0, now-lastReset)/reset+1)*reset
		nextReset = &next
	}
	mint := sl.Limit.Mint.String()
	symbol, decimals := "TOKEN", 0
	if sl.Limit.Mint.IsZero() {
		symbol, decimals = "SOL", 9
	}
	label := period
	switch period {
	case "one_time":
		label = "one-time"
	case "custom":
		if periodSeconds != nil && *periodSeconds%overviewDaySeconds == 0 {
			days := *periodSeconds / overviewDaySeconds
			label = fmt.Sprintf("%d day%s", days, map[bool]string{true: "", false: "s"}[days == 1])
		}
	}
	signers := make([]string, 0, len(p.Signers))
	for _, signer := range p.Signers {
		signers = append(signers, signer.Key.String())
	}
	destinations := make([]string, 0, len(sl.Destinations))
	for _, destination := range sl.Destinations {
		destinations = append(destinations, destination.String())
	}
	return overviewSpendingLimit{
		Address:                     entry.address.String(),
		SettingsPda:                 p.Settings.String(),
		Seed:                        strconv.FormatUint(p.Seed, 10),
		AccountIndex:                *p.AccountIndex,
		Mint:                        mint,
		Symbol:                      symbol,
		Decimals:                    decimals,
		AmountRaw:                   amount.String(),
		RemainingAmountRaw:          remaining.String(),
		EffectiveRemainingAmountRaw: effective.String(),
		MaxPerUseRaw:                strconv.FormatUint(sl.Limit.MaxPerUse, 10),
		AmountUi:                    overviewTokenAmount(amount, decimals),
		RemainingAmountUi:           overviewTokenAmount(effective, decimals),
		Period:                      period,
		PeriodSeconds:               periodSeconds,
		PeriodLabel:                 label,
		AccumulateUnused:            sl.Limit.Accumulate,
		LastReset:                   lastReset,
		NextReset:                   nextReset,
		Expiration:                  sl.Limit.Expiration,
		IsExpired:                   sl.Limit.Expiration != nil && *sl.Limit.Expiration <= now,
		Signers:                     signers,
		Destinations:                destinations,
	}
}

func overviewTokenAmount(raw *big.Int, decimals int) string {
	if decimals == 0 {
		return raw.String()
	}
	base := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)
	whole, fraction := new(big.Int).QuoRem(raw, base, new(big.Int))
	if fraction.Sign() == 0 {
		return whole.String()
	}
	digits := fmt.Sprintf("%0*s", decimals, fraction.String())
	return whole.String() + "." + strings.TrimRight(digits, "0")
}

func overviewLocaleCompare(a, b string) int {
	primary := func(r byte) int {
		switch {
		case r >= '0' && r <= '9':
			return int(r - '0')
		case r >= 'a' && r <= 'z':
			return 10 + int(r-'a')*2
		case r >= 'A' && r <= 'Z':
			return 10 + int(r-'A')*2
		}
		return 100 + int(r)
	}
	for i := 0; i < min(len(a), len(b)); i++ {
		if c := cmp.Compare(primary(a[i]), primary(b[i])); c != 0 {
			return c
		}
	}
	if c := cmp.Compare(len(a), len(b)); c != 0 {
		return c
	}
	for i := range len(a) {
		if a[i] != b[i] {
			if a[i] >= 'a' && a[i] <= 'z' {
				return -1
			}
			return 1
		}
	}
	return 0
}
