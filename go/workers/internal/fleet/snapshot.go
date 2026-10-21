package fleet

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	solana "github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/spl"
)

const opportunityLifetime = 2 * time.Minute

const snapshotReadConcurrency = 8

type CatalogStore struct {
	pool         *pgxpool.Pool
	schema       string
	enabledMints []string
}

func NewCatalogStoreFromPool(pool *pgxpool.Pool, schema string) (*CatalogStore, error) {
	if pool == nil || !validSQLIdentifier(schema) {
		return nil, errors.New("reserve catalog requires a scoped pool and a valid schema")
	}
	return &CatalogStore{pool: pool, schema: schema, enabledMints: EarnStableMints()}, nil
}

func validSQLIdentifier(value string) bool {
	if value == "" {
		return false
	}
	for index, r := range value {
		if !(r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || index > 0 && r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

func (s *CatalogStore) LoadCatalog(ctx context.Context) ([]ReserveIdentity, error) {
	rows, err := s.pool.Query(ctx, fmt.Sprintf(`
SELECT reserve, market, liquidity_mint FROM %s.supported_reserves
WHERE active AND source='kamino-api' AND liquidity_mint=ANY($1) AND 'safe'=ANY(risk_baskets)
ORDER BY reserve, market, liquidity_mint`, s.schema), s.enabledMints)
	if err != nil {
		return nil, fmt.Errorf("load reserve catalog: %w", err)
	}
	defer rows.Close()
	var catalog []ReserveIdentity
	duplicate := map[string]bool{}
	for rows.Next() {
		var identity ReserveIdentity
		if err := rows.Scan(&identity.Address, &identity.Market, &identity.Mint); err != nil {
			return nil, err
		}
		if n := len(catalog); n > 0 && catalog[n-1].Address == identity.Address {
			duplicate[identity.Address] = true
			continue
		}
		catalog = append(catalog, identity)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	routable := catalog[:0]
	for _, identity := range catalog {
		if !duplicate[identity.Address] {
			routable = append(routable, identity)
		}
	}
	return routable, nil
}

type SnapshotVault struct {
	Pubkey        string
	PolicyAccount string
	Markets       []string
}

type Holding struct {
	Reserve, Market, Mint       string
	CollateralRaw, LiquidityRaw uint64
}

type VaultFacts struct {
	Policy    *DecodedSquadsPolicy
	PolicyErr string
	Holdings  []Holding
	Idle      map[string]uint64
}

type Snapshot struct {
	Slot       int64
	ObservedAt time.Time
	Reserves   map[string]ReserveState
	Unroutable map[string]string
	Vaults     map[string]VaultFacts
}

type SnapshotRequest struct {
	Catalog []ReserveIdentity
	Vaults  []SnapshotVault
	// SlotsPerSecond is the measured slot rate; nil reads holdings only.
	SlotsPerSecond *float64
	Idle           bool
}

func ReadSnapshot(ctx context.Context, reader AccountReader, request SnapshotRequest) (Snapshot, error) {
	catalog, vaults, idle := request.Catalog, request.Vaults, request.Idle
	out := Snapshot{Reserves: map[string]ReserveState{}, Unroutable: map[string]string{}, Vaults: map[string]VaultFacts{}}
	if len(catalog) == 0 {
		return out, errors.New("snapshot requires a reserve catalog")
	}
	keys := make([]solana.PublicKey, len(catalog))
	for i, identity := range catalog {
		key, err := solana.PublicKeyFromBase58(identity.Address)
		if err != nil {
			return out, fmt.Errorf("catalog reserve %q: %w", identity.Address, err)
		}
		keys[i] = key
	}
	slot, accounts, err := reader.Accounts(ctx, keys, rpc.CommitmentConfirmed, 0)
	if err != nil {
		return out, fmt.Errorf("read catalog reserves: %w", err)
	}
	out.Slot, out.ObservedAt = int64(slot), time.Now().UTC()
	decoded := map[string]kamino.Reserve{}
	for i, identity := range catalog {
		account := accounts[i]
		if account == nil {
			out.Unroutable[identity.Address] = "absent"
			continue
		}
		reserve, err := kamino.DecodeReserve(account)
		if err != nil || reserve.LendingMarket.String() != identity.Market || reserve.LiquidityMint.String() != identity.Mint {
			out.Unroutable[identity.Address] = "identity_or_layout_drift"
			continue
		}
		decoded[identity.Address] = reserve
		if request.SlotsPerSecond == nil {
			continue
		}
		state, err := DecodeKaminoReserve(account, identity, out.Slot, *request.SlotsPerSecond)
		if err != nil {
			out.Unroutable[identity.Address] = err.Error()
			continue
		}
		state.ObservedAt = out.ObservedAt
		out.Reserves[identity.Address] = state
	}
	markets := map[string]bool{}
	for _, identity := range catalog {
		markets[identity.Market] = true
	}
	type vaultKeys struct {
		policy      int
		obligations []int
		idle        map[string]int
	}
	var reads []solana.PublicKey
	add := func(key solana.PublicKey) int {
		reads = append(reads, key)
		return len(reads) - 1
	}
	plan := make([]vaultKeys, len(vaults))
	for i, vault := range vaults {
		owner, err := solana.PublicKeyFromBase58(vault.Pubkey)
		if err != nil {
			return out, fmt.Errorf("vault %q: %w", vault.Pubkey, err)
		}
		plan[i].policy = -1
		if vault.PolicyAccount != "" {
			policy, err := solana.PublicKeyFromBase58(vault.PolicyAccount)
			if err != nil {
				return out, fmt.Errorf("vault %s policy %q: %w", vault.Pubkey, vault.PolicyAccount, err)
			}
			plan[i].policy = add(policy)
		}
		for _, market := range canonicalStrings(vault.Markets) {
			if !markets[market] {
				continue
			}
			marketKey, err := solana.PublicKeyFromBase58(market)
			if err != nil {
				return out, fmt.Errorf("vault %s market %q: %w", vault.Pubkey, market, err)
			}
			obligation, err := kamino.VanillaObligation(owner, marketKey)
			if err != nil {
				return out, err
			}
			plan[i].obligations = append(plan[i].obligations, add(obligation))
		}
		if idle {
			plan[i].idle = map[string]int{}
			for _, mint := range earnStableMints {
				account, err := spl.AssociatedTokenAddress(owner, solana.MustPublicKeyFromBase58(mint), earnTokenProgram(mint))
				if err != nil {
					return out, err
				}
				plan[i].idle[mint] = add(account)
			}
		}
	}
	values, err := readAccountsAt(ctx, reader, reads, slot)
	if err != nil {
		return out, fmt.Errorf("read vault accounts: %w", err)
	}
	for i, vault := range vaults {
		facts := VaultFacts{}
		if plan[i].policy >= 0 {
			if policy, err := DecodeSquadsPolicy(values[plan[i].policy]); err != nil {
				facts.PolicyErr = err.Error()
			} else {
				facts.Policy = &policy
			}
		}
		for _, index := range plan[i].obligations {
			holdings, err := obligationHoldings(values[index], vault.Pubkey, decoded)
			if err != nil {
				return out, fmt.Errorf("vault %s obligation %s: %w", vault.Pubkey, reads[index], err)
			}
			facts.Holdings = append(facts.Holdings, holdings...)
		}
		sort.Slice(facts.Holdings, func(a, b int) bool { return facts.Holdings[a].Reserve < facts.Holdings[b].Reserve })
		if idle {
			facts.Idle = map[string]uint64{}
			for mint, index := range plan[i].idle {
				amount, err := idleAmount(values[index], mint, vault.Pubkey)
				if err != nil {
					return out, fmt.Errorf("vault %s idle %s: %w", vault.Pubkey, mint, err)
				}
				facts.Idle[mint] = amount
			}
		}
		out.Vaults[vault.Pubkey] = facts
	}
	return out, nil
}

func obligationHoldings(account *chain.Account, owner string, reserves map[string]kamino.Reserve) ([]Holding, error) {
	if account == nil {
		return nil, nil
	}
	obligation, err := kamino.DecodeObligation(account)
	if err != nil {
		return nil, err
	}
	if obligation.Owner.String() != owner {
		return nil, fmt.Errorf("obligation owner %s is not the vault", obligation.Owner)
	}
	var out []Holding
	for _, deposit := range obligation.Deposits {
		if deposit.Reserve.IsZero() || deposit.DepositedAmount == 0 {
			continue
		}
		reserve, known := reserves[deposit.Reserve.String()]
		if !known || reserve.LendingMarket != obligation.LendingMarket {
			continue
		}
		liquidity, err := reserve.CollateralToLiquidity(deposit.DepositedAmount)
		if err != nil {
			return nil, fmt.Errorf("reserve %s collateral value: %w", deposit.Reserve, err)
		}
		out = append(out, Holding{Reserve: deposit.Reserve.String(), Market: obligation.LendingMarket.String(), Mint: reserve.LiquidityMint.String(), CollateralRaw: deposit.DepositedAmount, LiquidityRaw: liquidity})
	}
	return out, nil
}

func idleAmount(account *chain.Account, mint, owner string) (uint64, error) {
	if account == nil {
		return 0, nil
	}
	held, err := spl.DecodeTokenAccount(account)
	if err != nil {
		return 0, err
	}
	if held.Mint.String() != mint || held.Owner.String() != owner {
		return 0, errors.New("token account is not the vault's account of the mint")
	}
	return held.Amount, nil
}

func earnTokenProgram(mint string) solana.PublicKey {
	switch mint {
	case CashMint, USDGMint, PYUSDMint:
		return solana.Token2022ProgramID
	}
	return solana.TokenProgramID
}

func readAccountsAt(ctx context.Context, reader AccountReader, keys []solana.PublicKey, minSlot uint64) ([]*chain.Account, error) {
	out := make([]*chain.Account, len(keys))
	const batch = 100
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	slots := make(chan struct{}, snapshotReadConcurrency)
	var wg sync.WaitGroup
	var once sync.Once
	var first error
	for start := 0; start < len(keys); start += batch {
		end := min(start+batch, len(keys))
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func(start, end int) {
			defer wg.Done()
			defer func() { <-slots }()
			_, accounts, err := reader.Accounts(ctx, keys[start:end], rpc.CommitmentConfirmed, minSlot)
			if err == nil && len(accounts) != end-start {
				err = errors.New("getMultipleAccounts returned a different account count")
			}
			if err != nil {
				once.Do(func() { first = err; cancel() })
				return
			}
			copy(out[start:end], accounts)
		}(start, end)
	}
	wg.Wait()
	if first != nil {
		return nil, first
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

type snapshotRecord struct {
	Key                   string
	Slot                  int64
	ObservedAt, ExpiresAt time.Time
	State                 json.RawMessage
}

type recordedReserve struct {
	Reserve               string    `json:"reserve"`
	Market                string    `json:"market"`
	LiquidityMint         string    `json:"liquidityMint"`
	Slot                  int64     `json:"slot"`
	ObservedAt            time.Time `json:"observedAt"`
	LastUpdateSlot        int64     `json:"lastUpdateSlot"`
	LastUpdateStale       bool      `json:"lastUpdateStale"`
	SupplyAPYBPS          int64     `json:"supplyApyBps"`
	TotalSupplyRaw        int64     `json:"totalSupplyUsdMicros"`
	AvailableLiquidityRaw *int64    `json:"availableLiquidityRaw"`
	DepositLimitRaw       *uint64   `json:"depositLimitRaw"`
}

func recordOf(snapshot Snapshot) (snapshotRecord, error) {
	reserves := make([]recordedReserve, 0, len(snapshot.Reserves))
	for _, r := range snapshot.Reserves {
		reserves = append(reserves, recordedReserve{r.Address, r.Market, r.Mint, r.Slot, r.ObservedAt, r.LastUpdateSlot, r.LastUpdateStale, r.SupplyAPYBPS, r.TotalSupplyRaw, r.AvailableLiquidityRaw, r.DepositLimitRaw})
	}
	sort.Slice(reserves, func(i, j int) bool { return reserves[i].Reserve < reserves[j].Reserve })
	state, err := json.Marshal(struct {
		Slot       int64             `json:"slot"`
		ObservedAt time.Time         `json:"observedAt"`
		Reserves   []recordedReserve `json:"reserves"`
	}{snapshot.Slot, snapshot.ObservedAt, reserves})
	if err != nil {
		return snapshotRecord{}, err
	}
	sum := sha256.Sum256(state)
	return snapshotRecord{Key: hex.EncodeToString(sum[:]), Slot: snapshot.Slot, ObservedAt: snapshot.ObservedAt, ExpiresAt: snapshot.ObservedAt.Add(opportunityLifetime), State: state}, nil
}

func (s Snapshot) Market(hash string) MarketSnapshot {
	return MarketSnapshot{Slot: s.Slot, ObservedAt: s.ObservedAt, Hash: hash, Reserves: s.Reserves}
}

type ReserveSource interface {
	ReadReserves(context.Context) (MarketSnapshot, error)
}

type ChainReserves struct {
	Catalog ReserveCatalog
	RPC     *chain.Client
}

func (c ChainReserves) ReadReserves(ctx context.Context) (MarketSnapshot, error) {
	if c.Catalog == nil || c.RPC == nil {
		return MarketSnapshot{}, errors.New("reserve source requires a catalog and RPC")
	}
	catalog, err := c.Catalog.LoadCatalog(ctx)
	if err != nil {
		return MarketSnapshot{}, err
	}
	slotsPerSecond, err := c.RPC.SlotsPerSecond(ctx)
	if err != nil {
		return MarketSnapshot{}, fmt.Errorf("measured slot rate: %w", err)
	}
	snapshot, err := ReadSnapshot(ctx, c.RPC, SnapshotRequest{Catalog: catalog, SlotsPerSecond: &slotsPerSecond})
	if err != nil {
		return MarketSnapshot{}, err
	}
	return snapshot.Market(""), nil
}
