package watch

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/gagliardetto/solana-go"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	BalanceSweepWalletATAs      = "balance_sweep_wallet_atas"
	EarnSmartAccounts           = "earn_smart_accounts"
	EarnPolicyAccounts          = "earn_policy_accounts"
	EarnVaultAccounts           = "earn_vault_accounts"
	EarnIdleTokenAccounts       = "earn_idle_token_accounts"
	EarnWalletTokenAccounts     = "earn_wallet_token_accounts"
	EarnObligations             = "earn_obligations"
	EarnAutodepositWalletATAs   = "earn_autodeposit_wallet_atas"
	EarnSubscriptionAuthorities = "earn_subscription_authorities"
	EarnRecurringDelegations    = "earn_recurring_delegations"
	EarnWallets                 = "earn_wallets"
)

var (
	squadsProgram          = solana.MustPublicKeyFromBase58("SMRTzfY6DfH5ik3TKiyLFfXexV8uSG3d2UksSCYdunG")
	associatedTokenProgram = solana.MustPublicKeyFromBase58("ATokenGPvbdGVxr1b2hvZbsiqW5xWH25efTNsLJA8knL")
	tokenProgram           = solana.TokenProgramID
	token2022Program       = solana.MustPublicKeyFromBase58("TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb")
	kaminoProgram          = solana.MustPublicKeyFromBase58("KLend2g3cP87fffoy8q1mQqGKjrxjC8boSyAYavgmjD")
)

type Stablecoin struct{ Mint, TokenProgram solana.PublicKey }

var stablecoins = []Stablecoin{
	{solana.MustPublicKeyFromBase58("CASHx9KJUStyftLFWGvEVf59SGeG9sh5FfcnZMVPCASH"), token2022Program},
	{solana.MustPublicKeyFromBase58("2u1tszSeqZ3qBWF3uNGPFc8TzMk2tdiwknnRMWGWjGWH"), token2022Program},
	{solana.MustPublicKeyFromBase58("2b1kV6DkPAnxd5ixfnxCpjxmKwqjjaYmCZfHsFu24GXo"), token2022Program},
	{solana.MustPublicKeyFromBase58("EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"), tokenProgram},
	{solana.MustPublicKeyFromBase58("Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB"), tokenProgram},
	{solana.MustPublicKeyFromBase58("USDSwr9ApdHk5bvJKMjzff41FfuX8bSxdKcR81vTwcA"), tokenProgram},
}

var safeMarkets = []solana.PublicKey{
	solana.MustPublicKeyFromBase58("7u3HeHxYDLhnCoErrtycNokbQYbWGzLs6JSDqGAv5PfF"),
	solana.MustPublicKeyFromBase58("CqAoLuqWtavaVE8deBjMKe8ZfSt9ghR6Vb8nfsyabyHA"),
	solana.MustPublicKeyFromBase58("6WEGfej9B9wjxRs6t4BYpb9iCXd8CpTpJ8fVSNzHCC5y"),
	solana.MustPublicKeyFromBase58("47tfyEG9SsdEnUm9cw5kY9BXngQGqu3LBoop9j5uTAv8"),
	solana.MustPublicKeyFromBase58("BJnbcRHqvppTyGesLzWASGKnmnF1wq9jZu6ExrjT7wvF"),
}

type ATATarget struct {
	ID                                                int64
	Cluster, Wallet, WalletATA, Vault, VaultATA, Mint string
}

type Account struct {
	Pubkey string `json:"pubkey"`
	Role   string `json:"role"`
}
type Vault struct {
	Environment          string    `json:"environment"`
	Settings             string    `json:"settings"`
	Wallet               string    `json:"wallet"`
	EarnMax              bool      `json:"earn_max"`
	Vault                string    `json:"vault"`
	VaultIndex           uint8     `json:"vault_index"`
	Accounts             []Account `json:"accounts"`
	ObservationStartSlot *uint64   `json:"-"`
}

type Set struct {
	ATAs                 map[string]ATATarget
	Vaults               []Vault
	Channels             map[string][]string
	BindingStartSlots    map[string]uint64
	ObservationStartSlot *uint64
}

func (s *Set) AffectedVaults(account string) []Vault {
	var affected []Vault
	for _, vault := range s.Vaults {
		for _, watched := range vault.Accounts {
			if watched.Pubkey == account {
				affected = append(affected, vault)
				break
			}
		}
	}
	return affected
}

func (s *Set) Fingerprint() string {
	encoded, _ := json.Marshal(struct {
		Channels map[string][]string
		Vaults   []Vault
	}{s.Channels, s.Vaults})
	return string(encoded)
}

type Loader struct {
	pool    *pgxpool.Pool
	cluster string
}

func NewLoader(pool *pgxpool.Pool, cluster string) *Loader {
	return &Loader{pool: pool, cluster: cluster}
}

func (l *Loader) Load(ctx context.Context) (*Set, error) {
	atas, err := l.loadATATargets(ctx)
	if err != nil {
		return nil, err
	}
	set := &Set{ATAs: atas, Channels: make(map[string][]string), BindingStartSlots: make(map[string]uint64)}
	targets, err := l.loadEarnTargets(ctx)
	if err != nil {
		return nil, err
	}
	vaults := make(map[string]*Vault)
	for _, target := range targets {
		vault, err := buildVault(target)
		if err != nil {
			return nil, err
		}
		if target.ObservationStartSlot != nil && *target.ObservationStartSlot > 0 {
			for _, account := range vault.Accounts {
				set.recordBindingStart(vault, account, *target.ObservationStartSlot)
			}
		}
		key := vault.Environment + ":" + vault.Vault
		if current := vaults[key]; current != nil {
			if current.Settings != vault.Settings || current.VaultIndex != vault.VaultIndex {
				return nil, fmt.Errorf("conflicting Earn identity for vault %s", vault.Vault)
			}
			current.EarnMax = current.EarnMax || vault.EarnMax
			if current.Wallet == "" {
				current.Wallet = vault.Wallet
			}
			current.Accounts = append(current.Accounts, vault.Accounts...)
			current.ObservationStartSlot = minimumSlot(current.ObservationStartSlot, vault.ObservationStartSlot)
		} else {
			copy := vault
			vaults[key] = &copy
		}
		set.ObservationStartSlot = minimumSlot(set.ObservationStartSlot, target.ObservationStartSlot)
	}
	for _, vault := range vaults {
		sort.Slice(vault.Accounts, func(i, j int) bool {
			if vault.Accounts[i].Role == vault.Accounts[j].Role {
				return vault.Accounts[i].Pubkey < vault.Accounts[j].Pubkey
			}
			return vault.Accounts[i].Role < vault.Accounts[j].Role
		})
		vault.Accounts = compactAccounts(vault.Accounts)
		set.Vaults = append(set.Vaults, *vault)
	}
	sort.Slice(set.Vaults, func(i, j int) bool { return vaultKey(set.Vaults[i]) < vaultKey(set.Vaults[j]) })
	set.rebuildChannels()
	return set, nil
}

func (s *Set) AnchorNewEarnBindings(previous *Set, fallback uint64) error {
	existing := earnBindingSet(previous)
	if s.BindingStartSlots == nil {
		s.BindingStartSlots = make(map[string]uint64)
	}
	for _, vault := range s.Vaults {
		for _, account := range vault.Accounts {
			key := earnBindingKey(vault, account)
			if _, ok := existing[key]; ok {
				continue
			}
			if slot := s.BindingStartSlots[key]; slot > 0 {
				continue
			}
			if fallback == 0 {
				return fmt.Errorf("new Earn binding %s has no replay anchor", key)
			}
			s.BindingStartSlots[key] = fallback
		}
	}
	return nil
}

func (s *Set) NewEarnBindingStart(previous *Set) (*uint64, error) {
	existing := earnBindingSet(previous)
	var result *uint64
	for _, vault := range s.Vaults {
		for _, account := range vault.Accounts {
			key := earnBindingKey(vault, account)
			if _, ok := existing[key]; ok {
				continue
			}
			slot := s.BindingStartSlots[key]
			if slot == 0 {
				return nil, fmt.Errorf("new Earn binding %s has no replay anchor", key)
			}
			result = minimumSlot(result, &slot)
		}
	}
	return result, nil
}

// RetainPreviousEarnBindings keeps Earn routing monotonic for this process.
// An update for a just-removed account may still be in flight on the old
// subscription during handoff. A restart rebuilds the compact current set.
func (s *Set) RetainPreviousEarnBindings(previous *Set) error {
	if previous == nil {
		return nil
	}
	current := make(map[string]Vault, len(s.Vaults)+len(previous.Vaults))
	for _, vault := range s.Vaults {
		current[vaultKey(vault)] = vault
	}
	for _, old := range previous.Vaults {
		key := vaultKey(old)
		next, ok := current[key]
		if !ok {
			current[key] = old
			continue
		}
		if next.Settings != old.Settings || next.VaultIndex != old.VaultIndex || (next.Wallet != "" && old.Wallet != "" && next.Wallet != old.Wallet) {
			return fmt.Errorf("conflicting retained Earn identity for vault %s", old.Vault)
		}
		if next.Wallet == "" {
			next.Wallet = old.Wallet
		}
		next.EarnMax = next.EarnMax || old.EarnMax
		next.ObservationStartSlot = minimumSlot(next.ObservationStartSlot, old.ObservationStartSlot)
		next.Accounts = append(next.Accounts, old.Accounts...)
		sort.Slice(next.Accounts, func(i, j int) bool {
			if next.Accounts[i].Role == next.Accounts[j].Role {
				return next.Accounts[i].Pubkey < next.Accounts[j].Pubkey
			}
			return next.Accounts[i].Role < next.Accounts[j].Role
		})
		next.Accounts = compactAccounts(next.Accounts)
		current[key] = next
	}
	s.Vaults = s.Vaults[:0]
	for _, vault := range current {
		s.Vaults = append(s.Vaults, vault)
	}
	if s.BindingStartSlots == nil {
		s.BindingStartSlots = make(map[string]uint64)
	}
	for key, slot := range previous.BindingStartSlots {
		if current, ok := s.BindingStartSlots[key]; !ok || slot < current {
			s.BindingStartSlots[key] = slot
		}
	}
	s.ObservationStartSlot = minimumSlot(s.ObservationStartSlot, previous.ObservationStartSlot)
	sort.Slice(s.Vaults, func(i, j int) bool { return vaultKey(s.Vaults[i]) < vaultKey(s.Vaults[j]) })
	s.rebuildChannels()
	return nil
}

func (s *Set) rebuildChannels() {
	s.Channels = make(map[string][]string)
	for address := range s.ATAs {
		s.Channels[BalanceSweepWalletATAs] = append(s.Channels[BalanceSweepWalletATAs], address)
	}
	for _, vault := range s.Vaults {
		for _, account := range vault.Accounts {
			if channel := roleChannel(account.Role); channel != "" {
				s.Channels[channel] = append(s.Channels[channel], account.Pubkey)
			}
		}
	}
	for channel, addresses := range s.Channels {
		sort.Strings(addresses)
		s.Channels[channel] = compactStrings(addresses)
	}
}

func (s *Set) recordBindingStart(vault Vault, account Account, slot uint64) {
	key := earnBindingKey(vault, account)
	if current, ok := s.BindingStartSlots[key]; !ok || slot < current {
		s.BindingStartSlots[key] = slot
	}
}

func earnBindingSet(set *Set) map[string]struct{} {
	result := make(map[string]struct{})
	if set == nil {
		return result
	}
	for _, vault := range set.Vaults {
		for _, account := range vault.Accounts {
			result[earnBindingKey(vault, account)] = struct{}{}
		}
	}
	return result
}

func earnBindingKey(vault Vault, account Account) string {
	return vault.Environment + ":" + vault.Vault + ":" + account.Role + ":" + account.Pubkey
}

func vaultKey(vault Vault) string { return vault.Environment + ":" + vault.Vault }

func (l *Loader) loadATATargets(ctx context.Context) (map[string]ATATarget, error) {
	rows, err := l.pool.Query(ctx, `
		SELECT target.id, target.cluster, target.wallet, target.wallet_token_ata,
		       target.vault_pubkey, target.vault_token_ata, target.token_mint
		FROM loyal_yield.balance_sweep_targets AS target
		WHERE target.cluster = $1
		  AND target.desired_active
		  AND target.chain_status = 'active'
		  AND target.token_mint = $2
		ORDER BY target.id`, l.cluster, stablecoins[3].Mint.String())
	if err != nil {
		return nil, fmt.Errorf("load active ATA targets: %w", err)
	}
	defer rows.Close()
	targets := make(map[string]ATATarget)
	for rows.Next() {
		var target ATATarget
		if err := rows.Scan(&target.ID, &target.Cluster, &target.Wallet, &target.WalletATA, &target.Vault, &target.VaultATA, &target.Mint); err != nil {
			return nil, err
		}
		targets[target.WalletATA] = target
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return targets, nil
}

type earnTarget struct {
	Environment, Settings, Wallet, Vault         string
	VaultIndex                                   int16
	EarnMax                                      bool
	PolicyAccounts, Markets, AutodepositAccounts []string
	ObservationStartSlot                         *uint64
}

func appSettingsFilter(enabled bool, qualifiedColumn string) string {
	if !enabled {
		return ""
	}
	return fmt.Sprintf(`
			  AND %s IN (
			      SELECT app_smart.settings_pda
			      FROM app_user_smart_accounts AS app_smart
			      WHERE app_smart.solana_env = $1
			  )`, qualifiedColumn)
}

func (l *Loader) loadEarnTargets(ctx context.Context) ([]earnTarget, error) {
	type watchQuery struct {
		sql  string
		scan func(rowScanner) (earnTarget, error)
	}
	var queries []watchQuery
	appReady, err := l.relationsExist(ctx, "app_user_smart_accounts", "app_users")
	if err != nil {
		return nil, err
	}
	if appReady {
		queries = append(queries, watchQuery{`SELECT smart.solana_env, smart.settings_pda, app.subject_address FROM app_user_smart_accounts smart JOIN app_users app ON app.id=smart.user_id WHERE smart.solana_env=$1 AND smart.state='ready'`, scanAppTarget})
	}
	onboardingFilter := appSettingsFilter(appReady, "onboarding.settings")
	positionFilter := appSettingsFilter(appReady, "position.settings")
	managedVaultFilter := appSettingsFilter(appReady, "vault.settings")
	crossMintFilter := appSettingsFilter(appReady, "cross_mint_swap_policies.settings")
	earnMaxFilter := appSettingsFilter(appReady, "route.settings")
	managedVaultsReady, err := l.relationsExist(ctx, "loyal_yield.managed_vaults", "loyal_yield.route_policies")
	if err != nil {
		return nil, err
	}
	if managedVaultsReady {
		queries = append(queries, watchQuery{`
			SELECT $1::text, active_policy.authority, vault.settings,
			       vault.vault_index, vault.vault_pubkey,
			       active_policy.policy_account,
			       setup_policy.policy_account,
			       active_policy.kamino_markets,
			       LEAST(active_policy.last_seen_slot,
			             setup_policy.last_seen_slot)
			FROM loyal_yield.managed_vaults AS vault
			JOIN loyal_yield.route_policies AS active_policy
			  ON active_policy.id = vault.active_policy_id
			LEFT JOIN loyal_yield.route_policies AS setup_policy
			  ON setup_policy.id = vault.setup_policy_id
			WHERE vault.active AND active_policy.active` + managedVaultFilter, scanManagedVaultTarget})
	}
	optional := []struct {
		relation, sql string
		scan          func(rowScanner) (earnTarget, error)
	}{
		{"loyal_yield.earn_deposit_onboarding_attempts", `
			SELECT $1::text, onboarding.wallet_address, onboarding.settings,
			       onboarding.vault_index, onboarding.vault_pubkey,
			       onboarding.policy_account, onboarding.setup_policy_account,
			       onboarding.market
			FROM loyal_yield.earn_deposit_onboarding_attempts AS onboarding
			WHERE onboarding.status <> 'complete'` + onboardingFilter, scanOnboardingTarget},
		{"loyal_yield.user_yield_positions", `
			SELECT $1::text, position.wallet_address, position.settings,
			       position.vault_index, position.vault_pubkey,
			       position.policy_account, position.current_market,
			       active_policy.policy_account, setup_policy.policy_account
			FROM loyal_yield.user_yield_positions AS position
			LEFT JOIN loyal_yield.managed_vaults AS vault
			  ON vault.settings = position.settings
			 AND vault.vault_index = position.vault_index
			 AND vault.vault_pubkey = position.vault_pubkey
			LEFT JOIN loyal_yield.route_policies AS active_policy
			  ON active_policy.id = vault.active_policy_id
			LEFT JOIN loyal_yield.route_policies AS setup_policy
			  ON setup_policy.id = vault.setup_policy_id
			WHERE position.status = 'active'` + positionFilter, scanPositionTarget},
		{"loyal_yield.cross_mint_swap_policies", `
			SELECT $1::text, cross_mint_swap_policies.authority,
			       cross_mint_swap_policies.settings,
			       cross_mint_swap_policies.vault_index,
			       cross_mint_swap_policies.vault_pubkey,
			       array_agg(DISTINCT cross_mint_swap_policies.policy_account
			                 ORDER BY cross_mint_swap_policies.policy_account)
			FROM loyal_yield.cross_mint_swap_policies AS cross_mint_swap_policies
			WHERE cross_mint_swap_policies.cluster = $1
			  AND cross_mint_swap_policies.active
			  AND cross_mint_swap_policies.source_shard IN ('classic', 'token_2022')` + crossMintFilter + `
			GROUP BY cross_mint_swap_policies.authority,
			         cross_mint_swap_policies.settings,
			         cross_mint_swap_policies.vault_index,
			         cross_mint_swap_policies.vault_pubkey`, scanCrossMintTarget},
		{"loyal_yield.balance_sweep_targets", `
			SELECT $1::text, target.settings, target.wallet, target.vault_index,
			       target.vault_pubkey, target.policy_account,
			       target.subscription_authority, target.recurring_delegation
			FROM loyal_yield.balance_sweep_targets AS target
			WHERE target.cluster = $1
			  AND target.chain_status <> 'closed'`, scanAutodepositTarget},
		{"loyal_yield.multiply_route_states", `
			SELECT $1::text, route.settings, route.vault_index, route.vault,
			       ARRAY(
			           SELECT item ->> 'account'
			           FROM jsonb_array_elements(policy.policy_accounts) AS item
			           WHERE item ->> 'account' IS NOT NULL
			           ORDER BY (item ->> 'seed')::bigint
			       ),
			       (route.state ->> 'observedSlot')::bigint
			FROM loyal_yield.multiply_route_states AS route
			JOIN loyal_yield.earn_max_policy_sets AS policy
			  ON policy.settings = route.settings
			 AND policy.vault_index = route.vault_index
			 AND policy.vault = route.vault
			WHERE route.state ->> 'engineVersion' = 'earn_max_v2'
			  AND policy.manifest_version = 'earn-max-v2'
			  AND policy.status = 'ready'` + earnMaxFilter, scanEarnMaxTarget},
	}
	for _, item := range optional {
		exists, err := l.relationsExist(ctx, item.relation)
		if err != nil {
			return nil, err
		}
		if exists {
			queries = append(queries, watchQuery{item.sql, item.scan})
		}
	}
	var result []earnTarget
	for _, query := range queries {
		rows, err := l.pool.Query(ctx, query.sql, l.cluster)
		if err != nil {
			return nil, fmt.Errorf("load Earn watch targets: %w", err)
		}
		for rows.Next() {
			target, err := query.scan(rows)
			if err != nil {
				rows.Close()
				return nil, err
			}
			result = append(result, target)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return result, nil
}

func (l *Loader) relationsExist(ctx context.Context, relations ...string) (bool, error) {
	for _, relation := range relations {
		var exists bool
		if err := l.pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, relation).Scan(&exists); err != nil {
			return false, err
		}
		if !exists {
			return false, nil
		}
	}
	return true, nil
}

type rowScanner interface{ Scan(...any) error }

func scanAppTarget(row rowScanner) (earnTarget, error) {
	var t earnTarget
	t.VaultIndex = 1
	err := row.Scan(&t.Environment, &t.Settings, &t.Wallet)
	return t, err
}
func scanOnboardingTarget(row rowScanner) (earnTarget, error) {
	var t earnTarget
	var policy, setup, market *string
	err := row.Scan(&t.Environment, &t.Wallet, &t.Settings, &t.VaultIndex, &t.Vault, &policy, &setup, &market)
	t.PolicyAccounts = nonNil(policy, setup)
	t.Markets = nonNil(market)
	return t, err
}
func scanPositionTarget(row rowScanner) (earnTarget, error) {
	var t earnTarget
	var policy, market, active, setup *string
	err := row.Scan(&t.Environment, &t.Wallet, &t.Settings, &t.VaultIndex, &t.Vault, &policy, &market, &active, &setup)
	t.PolicyAccounts = nonNil(policy, active, setup)
	t.Markets = nonNil(market)
	return t, err
}
func scanManagedVaultTarget(row rowScanner) (earnTarget, error) {
	var t earnTarget
	var active, setup *string
	var slot *int64
	if err := row.Scan(&t.Environment, &t.Wallet, &t.Settings, &t.VaultIndex, &t.Vault, &active, &setup, &t.Markets, &slot); err != nil {
		return t, err
	}
	if slot != nil {
		if *slot < 0 {
			return t, fmt.Errorf("Earn observation start slot is negative")
		}
		if *slot > 0 {
			value := uint64(*slot)
			t.ObservationStartSlot = &value
		}
	}
	t.PolicyAccounts = nonNil(active, setup)
	return t, nil
}
func scanCrossMintTarget(row rowScanner) (earnTarget, error) {
	var t earnTarget
	err := row.Scan(&t.Environment, &t.Wallet, &t.Settings, &t.VaultIndex, &t.Vault, &t.PolicyAccounts)
	return t, err
}
func scanAutodepositTarget(row rowScanner) (earnTarget, error) {
	var t earnTarget
	var authority, delegation *string
	var policy string
	err := row.Scan(&t.Environment, &t.Settings, &t.Wallet, &t.VaultIndex, &t.Vault, &policy, &authority, &delegation)
	t.PolicyAccounts = []string{policy}
	t.AutodepositAccounts = nonNil(authority, delegation)
	return t, err
}
func scanEarnMaxTarget(row rowScanner) (earnTarget, error) {
	var t earnTarget
	var slot *int64
	err := row.Scan(&t.Environment, &t.Settings, &t.VaultIndex, &t.Vault, &t.PolicyAccounts, &slot)
	t.EarnMax = true
	if slot != nil && *slot > 0 {
		v := uint64(*slot)
		t.ObservationStartSlot = &v
	}
	return t, err
}

func buildVault(target earnTarget) (Vault, error) {
	if target.VaultIndex < 0 || target.VaultIndex > 255 {
		return Vault{}, fmt.Errorf("invalid vault index %d", target.VaultIndex)
	}
	settings, err := solana.PublicKeyFromBase58(target.Settings)
	if err != nil {
		return Vault{}, fmt.Errorf("invalid settings %q: %w", target.Settings, err)
	}
	index := uint8(target.VaultIndex)
	derived, _, err := solana.FindProgramAddress([][]byte{[]byte("smart_account"), settings[:], []byte("smart_account"), {index}}, squadsProgram)
	if err != nil {
		return Vault{}, err
	}
	vault := target.Vault
	if vault == "" {
		vault = derived.String()
	} else if vault != derived.String() {
		return Vault{}, fmt.Errorf("recorded vault %s does not match derived %s", vault, derived)
	}
	result := Vault{Environment: target.Environment, Settings: target.Settings, Wallet: target.Wallet, Vault: vault, VaultIndex: index, EarnMax: target.EarnMax, ObservationStartSlot: target.ObservationStartSlot}
	result.Accounts = append(result.Accounts, Account{target.Settings, "smart_account"}, Account{vault, "vault"})
	vaultKey := derived
	if target.Wallet != "" {
		wallet, err := solana.PublicKeyFromBase58(target.Wallet)
		if err != nil {
			return Vault{}, err
		}
		result.Accounts = append(result.Accounts, Account{target.Wallet, "wallet"}, Account{associatedToken(wallet, stablecoins[3]), "autodeposit_wallet_ata"})
		for _, coin := range stablecoins {
			result.Accounts = append(result.Accounts, Account{associatedToken(wallet, coin), "wallet_token"})
		}
	}
	for _, coin := range stablecoins {
		result.Accounts = append(result.Accounts, Account{associatedToken(vaultKey, coin), "idle_token"})
	}
	markets := append([]string(nil), target.Markets...)
	for _, market := range safeMarkets {
		markets = append(markets, market.String())
	}
	markets = compactStringsSorted(markets)
	for _, value := range markets {
		market, err := solana.PublicKeyFromBase58(value)
		if err != nil {
			continue
		}
		obligation, _, err := solana.FindProgramAddress([][]byte{{0}, {0}, vaultKey[:], market[:], make([]byte, 32), make([]byte, 32)}, kaminoProgram)
		if err != nil {
			return Vault{}, err
		}
		result.Accounts = append(result.Accounts, Account{obligation.String(), "obligation"})
	}
	for _, policy := range target.PolicyAccounts {
		if strings.TrimSpace(policy) != "" {
			result.Accounts = append(result.Accounts, Account{policy, "policy"})
		}
	}
	for index, address := range target.AutodepositAccounts {
		role := "recurring_delegation"
		if index == 0 {
			role = "subscription_authority"
		}
		result.Accounts = append(result.Accounts, Account{address, role})
	}
	return result, nil
}

func USDCATA(owner string) (string, error) {
	pubkey, err := solana.PublicKeyFromBase58(owner)
	if err != nil {
		return "", err
	}
	return associatedToken(pubkey, stablecoins[3]), nil
}

func associatedToken(owner solana.PublicKey, coin Stablecoin) string {
	address, _, _ := solana.FindProgramAddress([][]byte{owner[:], coin.TokenProgram[:], coin.Mint[:]}, associatedTokenProgram)
	return address.String()
}
func ChannelForRole(role string) string {
	return map[string]string{"wallet": EarnWallets, "smart_account": EarnSmartAccounts, "policy": EarnPolicyAccounts, "vault": EarnVaultAccounts, "idle_token": EarnIdleTokenAccounts, "wallet_token": EarnWalletTokenAccounts, "obligation": EarnObligations, "autodeposit_wallet_ata": EarnAutodepositWalletATAs, "subscription_authority": EarnSubscriptionAuthorities, "recurring_delegation": EarnRecurringDelegations}[role]
}
func roleChannel(role string) string { return ChannelForRole(role) }
func minimumSlot(a, b *uint64) *uint64 {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	v := *a
	if *b < v {
		v = *b
	}
	return &v
}
func nonNil(values ...*string) []string {
	var out []string
	for _, v := range values {
		if v != nil && *v != "" {
			out = append(out, *v)
		}
	}
	return out
}
func compactStringsSorted(values []string) []string {
	sort.Strings(values)
	return compactStrings(values)
}
func compactStrings(values []string) []string {
	if len(values) < 2 {
		return values
	}
	out := values[:1]
	for _, v := range values[1:] {
		if v != out[len(out)-1] {
			out = append(out, v)
		}
	}
	return out
}
func compactAccounts(values []Account) []Account {
	if len(values) < 2 {
		return values
	}
	out := values[:1]
	for _, v := range values[1:] {
		if v != out[len(out)-1] {
			out = append(out, v)
		}
	}
	return out
}
