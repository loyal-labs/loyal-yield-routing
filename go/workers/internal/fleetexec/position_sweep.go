package fleetexec

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/big"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/engine"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/spl"
	sdk "github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"
)

// The vault-position sweep is Rust's fleet reconciler position sweep
// (loyal-fleet-worker lib.rs advance_fleet_position_sweep and
// reconcile_fleet_position_sweep_vault): every managed Kamino vault's reserve
// positions and idle balances are re-read from RPC on a fixed cadence and
// published as one complete product snapshot. Planning and Autodeposit reserve
// resolution read those rows; without the sweep they age out between this
// family's own submissions.
const (
	positionSweepSameMintMode     = "same_mint_kamino"
	positionSweepFixedKaminoMode  = "fixed_kamino_main"
	positionSweepFailureRetry     = 5 * time.Second
	positionSweepAccountsPerBatch = 100
	// positionSweepDatabaseSlots bounds the connections the sweep holds at once
	// on the shared yield pool (16). Rust ran the sweep in its own process
	// with its own pool; here a 64-vault RPC wave queues for these slots, so
	// at least pool size minus four connections always remain for the other
	// lanes.
	positionSweepDatabaseSlots    = 4
	positionSweepAmountSemantics  = "kamino_obligation_collateral_deposited_amount"
	positionSweepKind             = "fleet_position_sweep"
	positionSweepCompleteScope    = "complete_product_vault"
	positionSweepObligationLength = 3344
	positionSweepUserMetadataSeed = "user_meta"
	positionSweepSourceCommitment = "confirmed"
)

var positionSweepObligationDiscriminator = []byte{168, 206, 141, 106, 88, 76, 172, 167}

// PositionSweepConfig is the explicit sweep configuration. Interval is
// Rust's --position-sweep-interval-seconds and Concurrency its reconciler
// --concurrency, the vaults refreshed in one wave.
type PositionSweepConfig struct {
	Cluster string
	// DelegatedSigner is the standard policy authority a swept policy names.
	DelegatedSigner string
	// EnabledMints is the planner's routing universe; a vault is swept when
	// its policy holds one of them.
	EnabledMints []string
	Interval     time.Duration
	Concurrency  int
	Facts        *engine.Facts
}

// PositionSweep is the reconciler lane that keeps vault positions current.
// It reads with confirmed commitment and never holds a signing capability.
type PositionSweep struct {
	config PositionSweepConfig
	store  *Store
	rpc    fleet.AccountReader
	nextID uint64
	// summaries is Rust's shared reserve-summary cache: an address-derivation
	// accelerator refreshed by every coherent vault read.
	mu        sync.Mutex
	summaries map[string]fleet.KaminoPositionAccounts
	// database holds one token per sweep database statement or transaction in
	// flight; RPC reads never hold one.
	database chan struct{}
}

func NewPositionSweep(config PositionSweepConfig, store *Store, rpc fleet.AccountReader) (*PositionSweep, error) {
	if config.Interval <= 0 || config.Interval > 24*time.Hour || config.Concurrency <= 0 || config.Concurrency > 256 || config.Cluster == "" || config.Facts == nil {
		return nil, errors.New("incomplete position sweep configuration")
	}
	if _, err := sdk.PublicKeyFromBase58(config.DelegatedSigner); err != nil {
		return nil, fmt.Errorf("position sweep delegated signer: %w", err)
	}
	if _, err := positionSweepCatalogMintsHash(config.EnabledMints); err != nil {
		return nil, err
	}
	if store == nil || rpc == nil {
		return nil, errors.New("position sweep requires store and RPC")
	}
	return &PositionSweep{config: config, store: store, rpc: rpc, nextID: 1, summaries: map[string]fleet.KaminoPositionAccounts{}, database: make(chan struct{}, positionSweepDatabaseSlots)}, nil
}

type positionSweepFailureKind string

const (
	positionSweepTransport positionSweepFailureKind = "transport"
	positionSweepInvariant positionSweepFailureKind = "invariant"
)

type positionSweepError struct {
	kind positionSweepFailureKind
	err  error
}

func (e *positionSweepError) Error() string { return e.err.Error() }
func (e *positionSweepError) Unwrap() error { return e.err }

func sweepTransport(err error) error { return &positionSweepError{positionSweepTransport, err} }
func sweepInvariant(format string, args ...any) error {
	return &positionSweepError{positionSweepInvariant, fmt.Errorf(format, args...)}
}

// sweepKind classifies a per-vault failure. Database and RPC faults clear on
// retry; anything this code decided is an identity or policy invariant.
func sweepKind(err error) positionSweepFailureKind {
	var typed *positionSweepError
	if errors.As(err, &typed) {
		return typed.kind
	}
	return positionSweepTransport
}

type positionSweepOutcome string

const (
	positionSweepRefreshed  positionSweepOutcome = "refreshed"
	positionSweepStale      positionSweepOutcome = "stale"
	positionSweepSuperseded positionSweepOutcome = "superseded"
	positionSweepFailed     positionSweepOutcome = "failed"
)

type positionSweepVault struct {
	id         int64
	settings   string
	vaultIndex int16
}

type positionSweepReserve struct {
	reserve, market, mint string
}

type positionSweepUniverse struct {
	revisionID int64
	sourceSlot *int64
	reserves   []positionSweepReserve
}

// positionSweepMetrics mirrors Rust FleetPositionSweepMetrics.
type positionSweepMetrics struct {
	SweepID           uint64 `json:"sweepId"`
	CatalogRevisionID int64  `json:"catalogRevisionId"`
	ReserveCount      int    `json:"reserveCount"`
	Eligible          int    `json:"eligible"`
	Processed         int    `json:"processed"`
	Refreshed         int    `json:"refreshed"`
	Failed            int    `json:"failed"`
	Stale             int    `json:"stale"`
	Superseded        int    `json:"superseded"`
	DurationMS        int64  `json:"durationMilliseconds"`
}

// Run sweeps every Interval from each sweep's start; a failed initialization
// retries after min(Interval, 5s), as Rust does.
func (p *PositionSweep) Run(ctx context.Context) error {
	next := time.Now()
	for {
		if wait := time.Until(next); wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil
			case <-timer.C:
			}
		}
		if ctx.Err() != nil {
			return nil
		}
		started := time.Now()
		_, err := p.sweep(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			next = time.Now().Add(min(p.config.Interval, positionSweepFailureRetry))
			continue
		}
		next = started.Add(p.config.Interval)
	}
}

// sweep runs one complete sweep: it freezes the ordered cohort and catalog
// universe, then refreshes the cohort in waves of Concurrency vaults.
func (p *PositionSweep) sweep(ctx context.Context) (positionSweepMetrics, error) {
	started := time.Now()
	startedAt := started.UTC()
	metrics := positionSweepMetrics{SweepID: p.nextID}
	p.nextID++
	vaults, universe, err := p.initialize(ctx)
	if err != nil {
		slog.Error("fleet_position_sweep_initialization_failed", "sweepId", metrics.SweepID, "kind", string(sweepKind(err)), "error", fleet.LogErrorText(err), "signerLoaded", false, "transactionsSent", false)
		return metrics, err
	}
	metrics.CatalogRevisionID, metrics.ReserveCount, metrics.Eligible = universe.revisionID, len(universe.reserves), len(vaults)
	for start := 0; start < len(vaults); start += p.config.Concurrency {
		if ctx.Err() != nil {
			return metrics, ctx.Err()
		}
		wave := vaults[start:min(start+p.config.Concurrency, len(vaults))]
		outcomes := make([]positionSweepOutcome, len(wave))
		var group sync.WaitGroup
		for i := range wave {
			group.Add(1)
			go func(i int) {
				defer group.Done()
				outcome, reason, err := p.reconcileVault(ctx, wave[i], universe, metrics.SweepID, startedAt)
				switch {
				case err != nil && ctx.Err() != nil:
					outcome = positionSweepFailed
				case err != nil:
					outcome = positionSweepFailed
					slog.Error("fleet_position_sweep_vault_failed", "sweepId", metrics.SweepID, "vaultId", wave[i].id, "kind", string(sweepKind(err)), "error", fleet.LogErrorText(err), "stateChanged", false, "signerLoaded", false, "transactionsSent", false)
				case outcome == positionSweepSuperseded:
					slog.Info("fleet_position_sweep_vault_superseded", "sweepId", metrics.SweepID, "vaultId", wave[i].id, "reason", reason, "stateChanged", false)
				}
				outcomes[i] = outcome
			}(i)
		}
		group.Wait()
		waveFailed := 0
		for _, outcome := range outcomes {
			if outcome == positionSweepFailed {
				waveFailed++
			}
		}
		slog.Info("fleet_position_sweep_wave", "sweepId", metrics.SweepID, "firstVault", start, "vaults", len(wave), "failed", waveFailed, "eligible", len(vaults))
		for _, outcome := range outcomes {
			metrics.Processed++
			switch outcome {
			case positionSweepRefreshed:
				metrics.Refreshed++
			case positionSweepStale:
				metrics.Stale++
			case positionSweepSuperseded:
				metrics.Superseded++
			default:
				metrics.Failed++
			}
		}
	}
	metrics.DurationMS = time.Since(started).Milliseconds()
	// A vault-specific invariant is logged per vault; the lane itself has
	// failed only when no vault could be refreshed at all.
	if metrics.Processed == 0 || metrics.Failed < metrics.Processed {
		p.config.Facts.LaneSucceeded(engine.FamilyFleet, "position_sweep")
	}
	slog.Info("fleet_position_sweep_complete", "metrics", metrics, "signerLoaded", false, "transactionsSent", false)
	return metrics, nil
}

// withDatabase runs one sweep statement or transaction under a database slot.
func (p *PositionSweep) withDatabase(ctx context.Context, work func() error) error {
	select {
	case p.database <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-p.database }()
	return work()
}

func (p *PositionSweep) initialize(ctx context.Context) ([]positionSweepVault, positionSweepUniverse, error) {
	var vaults []positionSweepVault
	err := p.withDatabase(ctx, func() (err error) {
		vaults, err = p.store.loadPositionSweepCohort(ctx, p.config.DelegatedSigner, p.config.EnabledMints)
		return err
	})
	if err != nil {
		return nil, positionSweepUniverse{}, sweepTransport(err)
	}
	universe, err := p.loadUniverse(ctx)
	return vaults, universe, err
}

// positionSweepCatalogMintsHash is Rust position_sweep_catalog_mints_hash:
// the runtime allowlist must be a supported subset, while the shared catalog
// must carry the complete supported universe.
func positionSweepCatalogMintsHash(enabled []string) (string, error) {
	supported := map[string]bool{}
	for _, mint := range fleet.EarnStableMints() {
		supported[mint] = true
	}
	if len(enabled) == 0 {
		return "", sweepInvariant("fleet position sweep runtime mints must be a non-empty supported subset")
	}
	for _, mint := range enabled {
		if !supported[mint] {
			return "", sweepInvariant("fleet position sweep runtime mints must be a non-empty supported subset")
		}
	}
	hasher := sha256.New()
	for _, mint := range fleet.EarnStableMints() {
		var length [8]byte
		binary.LittleEndian.PutUint64(length[:], uint64(len(mint)))
		hasher.Write(length[:])
		hasher.Write([]byte(mint))
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

// loadUniverse is Rust load_fleet_position_sweep_universe: the reserve-role
// addresses of the exact active shared-market catalog, each decoded once.
func (p *PositionSweep) loadUniverse(ctx context.Context) (positionSweepUniverse, error) {
	var universe positionSweepUniverse
	var head positionSweepCatalogHead
	err := p.withDatabase(ctx, func() (err error) {
		head, err = p.store.loadPositionSweepCatalogHead(ctx, p.config.Cluster)
		return err
	})
	if err != nil {
		return universe, err
	}
	if head.readiness != "active" || head.activeGeneration == nil || head.targetGeneration == nil || *head.activeGeneration != *head.targetGeneration {
		return universe, sweepInvariant("fleet position sweep requires the exact active shared-market catalog generation")
	}
	expected, err := positionSweepCatalogMintsHash(p.config.EnabledMints)
	if err != nil {
		return universe, err
	}
	if head.enabledMintsHash != expected {
		return universe, sweepInvariant("fleet position sweep requires the complete supported stable-mint shared-market catalog")
	}
	seen := map[string]bool{}
	var reserves []string
	for _, address := range head.addresses {
		if address.semanticClass != "shared_market" {
			return universe, sweepInvariant("shared-market catalog head contains a non-shared semantic row")
		}
		parts := strings.Split(address.accountRole, ",")
		roles := map[string]bool{}
		for _, role := range parts {
			if role == "" || strings.TrimSpace(role) != role || roles[role] {
				return universe, sweepInvariant("shared-market catalog contains malformed account roles")
			}
			roles[role] = true
		}
		if !roles["reserve"] {
			continue
		}
		if !address.writable || seen[address.address] {
			return universe, sweepInvariant("shared-market catalog reserve roles must be unique and writable")
		}
		if _, err := sdk.PublicKeyFromBase58(address.address); err != nil {
			return universe, sweepInvariant("shared-market catalog reserve role contains an invalid public key")
		}
		seen[address.address] = true
		reserves = append(reserves, address.address)
	}
	if len(reserves) == 0 {
		return universe, sweepInvariant("active shared-market catalog contains no reserve-role addresses")
	}
	decoded := map[string]fleet.KaminoPositionAccounts{}
	for start := 0; start < len(reserves); start += positionSweepAccountsPerBatch {
		chunk := reserves[start:min(start+positionSweepAccountsPerBatch, len(reserves))]
		_, accounts, err := fleet.ReadAccounts(ctx, p.rpc, chunk, rpc.CommitmentConfirmed, 1)
		if err != nil {
			return universe, sweepTransport(err)
		}
		for i, account := range accounts {
			if account == nil {
				return universe, sweepTransport(fmt.Errorf("reserve account %s does not exist", chunk[i]))
			}
			summary, err := fleet.DecodeReserveIdentity(account)
			if err != nil {
				return universe, sweepTransport(err)
			}
			decoded[chunk[i]] = summary
		}
	}
	p.mu.Lock()
	for reserve, summary := range decoded {
		p.summaries[reserve] = summary
	}
	p.mu.Unlock()
	for _, reserve := range reserves {
		universe.reserves = append(universe.reserves, positionSweepReserve{reserve: reserve, market: decoded[reserve].Market, mint: decoded[reserve].LiquidityMint})
	}
	sort.Slice(universe.reserves, func(i, j int) bool { return universe.reserves[i].reserve < universe.reserves[j].reserve })
	universe.revisionID, universe.sourceSlot = head.revisionID, head.sourceSlot
	return universe, nil
}

type positionSweepPolicyVault struct {
	id, policyID                                      int64
	vaultPubkey                                       string
	signers, modes, stableMints, markets, kaminoMints []string
}

func sweepContains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func sweepIntersects(values, wanted []string) bool {
	for _, value := range values {
		if sweepContains(wanted, value) {
			return true
		}
	}
	return false
}

// reconcileVault is Rust reconcile_fleet_position_sweep_vault. Cohort
// predicates are re-read before RPC work; one that no longer holds means the
// row changed under the frozen cohort and is reported as superseded.
func (p *PositionSweep) reconcileVault(ctx context.Context, entry positionSweepVault, universe positionSweepUniverse, sweepID uint64, startedAt time.Time) (positionSweepOutcome, string, error) {
	var vault *positionSweepPolicyVault
	err := p.withDatabase(ctx, func() (err error) {
		vault, err = p.store.loadPositionSweepVault(ctx, entry.settings, entry.vaultIndex)
		return err
	})
	if err != nil {
		return "", "", sweepTransport(err)
	}
	switch {
	case vault == nil:
		return positionSweepSuperseded, "sweep vault is no longer active under an active policy", nil
	case vault.id != entry.id:
		return positionSweepSuperseded, "active sweep vault identity changed during the full sweep", nil
	case !sweepContains(vault.modes, positionSweepSameMintMode) && !sweepContains(vault.modes, positionSweepFixedKaminoMode):
		return positionSweepSuperseded, "active policy is no longer in a tracked Kamino route mode", nil
	case !sweepContains(vault.signers, p.config.DelegatedSigner):
		return positionSweepSuperseded, "active policy no longer contains the standard delegated signer", nil
	case !sweepIntersects(vault.stableMints, p.config.EnabledMints) || !sweepIntersects(vault.kaminoMints, p.config.EnabledMints):
		return positionSweepSuperseded, "active policy is no longer in the enabled stable-mint cohort", nil
	case len(vault.markets) == 0:
		return positionSweepSuperseded, "active policy no longer declares a Kamino market", nil
	}
	var reserves []positionSweepReserve
	for _, reserve := range universe.reserves {
		if sweepContains(vault.markets, reserve.market) && sweepContains(vault.stableMints, reserve.mint) && sweepContains(vault.kaminoMints, reserve.mint) {
			reserves = append(reserves, reserve)
		}
	}
	if len(reserves) == 0 {
		return "", "", sweepInvariant("active policy has no role-validated shared-catalog reserve")
	}
	var held []string
	err = p.withDatabase(ctx, func() (err error) {
		held, err = p.store.heldPositionReserves(ctx, vault.id)
		return err
	})
	if err != nil {
		return "", "", sweepTransport(err)
	}
	for _, reserve := range held {
		if !containsReserve(reserves, reserve) {
			return "", "", sweepInvariant("a held current reserve is outside the active policy's role-validated stable universe")
		}
	}
	observation, err := p.observeVault(ctx, vault.vaultPubkey, reserves)
	if err != nil {
		return "", "", err
	}
	observation.context = map[string]any{
		"kind": positionSweepKind, "cluster": p.config.Cluster, "sweep_id": sweepID, "sweep_started_at": startedAt,
		"catalog_revision_id": universe.revisionID, "catalog_source_slot": universe.sourceSlot,
		"amount_semantics": positionSweepAmountSemantics, "idle_vault_liquidity_amount_raw": observation.idleTotal,
		"signer_loaded": false, "transactions_sent": false, "publication_scope": positionSweepCompleteScope,
	}
	var outcome positionSweepOutcome
	var reason string
	err = p.withDatabase(ctx, func() (err error) {
		outcome, reason, err = p.store.publishSweptVault(ctx, vault.id, vault.policyID, vault.vaultPubkey, observation)
		return err
	})
	return outcome, reason, err
}

func containsReserve(reserves []positionSweepReserve, reserve string) bool {
	for _, candidate := range reserves {
		if candidate.reserve == reserve {
			return true
		}
	}
	return false
}

type positionSweepPosition struct {
	reserve, market, mint              string
	amount, redeemable, vaultLiquidity int64
	obligation, ata                    string
	obligationExists, ataExists        bool
}

type positionSweepIdle struct {
	mint, tokenAccount string
	amount             int64
}

type positionSweepObservation struct {
	slot       int64
	observedAt time.Time
	positions  []positionSweepPosition
	idle       []positionSweepIdle
	idleTotal  *big.Int
	context    map[string]any
}

// sameDerivation is Rust KaminoReserveSummary::derivation_identity_matches.
func sameDerivation(a, b fleet.KaminoPositionAccounts) bool {
	return a.Market == b.Market && a.LiquidityMint == b.LiquidityMint && a.LiquidityTokenProgram == b.LiquidityTokenProgram &&
		a.LiquiditySupply == b.LiquiditySupply && a.CollateralMint == b.CollateralMint && a.CollateralSupply == b.CollateralSupply &&
		a.ReserveFarmState == b.ReserveFarmState && a.PythOracle == b.PythOracle && a.SwitchboardPriceOracle == b.SwitchboardPriceOracle &&
		a.SwitchboardTWAPOracle == b.SwitchboardTWAPOracle && a.ScopePrices == b.ScopePrices
}

// observeVault is Rust load_chain_reconcile_preview_from_runtime for the
// sweep: one confirmed getMultipleAccounts context holds the vault's KLend
// user metadata, its six Earn idle accounts and, per reserve, the reserve,
// the vault ATA, the vanilla obligation and its collateral farm user. The
// reserve is re-read in that batch; the cached summary only derives addresses.
func (p *PositionSweep) observeVault(ctx context.Context, vault string, reserves []positionSweepReserve) (positionSweepObservation, error) {
	var out positionSweepObservation
	vaultKey, err := sdk.PublicKeyFromBase58(vault)
	if err != nil {
		return out, sweepInvariant("vault %s is not a public key", vault)
	}
	userMetadata, _, err := sdk.FindProgramAddress([][]byte{[]byte(positionSweepUserMetadataSeed), vaultKey[:]}, sdk.MustPublicKeyFromBase58(fleet.KLendProgram))
	if err != nil {
		return out, sweepInvariant("derive user metadata: %v", err)
	}
	type idleAccount struct{ mint, program, account string }
	var idleAccounts []idleAccount
	for _, mint := range fleet.EarnStableMints() {
		program, err := canonicalCustodyTokenProgram(mint)
		if err != nil {
			return out, sweepInvariant("%v", err)
		}
		account, err := associatedCustodyAccount(vault, mint, program)
		if err != nil {
			return out, sweepInvariant("derive idle account: %v", err)
		}
		idleAccounts = append(idleAccounts, idleAccount{mint, program, account})
	}
	derived := make([]fleet.KaminoPositionAccounts, len(reserves))
	keys := map[string]bool{userMetadata.String(): true}
	for _, idle := range idleAccounts {
		keys[idle.account] = true
	}
	p.mu.Lock()
	for i, reserve := range reserves {
		summary, ok := p.summaries[reserve.reserve]
		if !ok {
			p.mu.Unlock()
			return out, sweepInvariant("validated reserve summary omitted catalog reserve %s", reserve.reserve)
		}
		derived[i] = summary
	}
	p.mu.Unlock()
	for i := range derived {
		if derived[i], err = fleet.DeriveVaultReserveAccounts(derived[i], vault); err != nil {
			return out, sweepInvariant("derive reserve accounts: %v", err)
		}
		keys[derived[i].Reserve], keys[derived[i].VaultLiquidityATA], keys[derived[i].Obligation] = true, true, true
		if derived[i].ObligationFarmUserState != "" {
			keys[derived[i].ObligationFarmUserState] = true
		}
	}
	if len(keys) > positionSweepAccountsPerBatch {
		return out, sweepInvariant("chain reconciliation dependent accounts exceed one getMultipleAccounts context")
	}
	ordered := make([]string, 0, len(keys))
	for key := range keys {
		ordered = append(ordered, key)
	}
	sort.Strings(ordered)
	slot, values, err := fleet.ReadAccounts(ctx, p.rpc, ordered, rpc.CommitmentConfirmed, 1)
	if err != nil {
		return out, sweepTransport(err)
	}
	accounts := make(map[string]*chain.Account, len(values))
	for i, value := range values {
		accounts[ordered[i]] = value
	}
	out.slot, out.observedAt = slot, time.Now().UTC()
	if a := accounts[userMetadata.String()]; a != nil && a.Owner.String() != fleet.KLendProgram {
		return out, sweepInvariant("account %s is owned by %s, expected %s", a.Key, a.Owner, fleet.KLendProgram)
	}
	out.idleTotal = new(big.Int)
	for _, idle := range idleAccounts {
		amount, _, err := positionSweepTokenAmount(accounts[idle.account], idle.mint, idle.program)
		if err != nil {
			return out, err
		}
		out.idle = append(out.idle, positionSweepIdle{mint: idle.mint, tokenAccount: idle.account, amount: amount})
		out.idleTotal.Add(out.idleTotal, big.NewInt(amount))
	}
	// A refreshed reserve whose derivation identity drifted invalidates the
	// addresses this batch read. The cache takes the refreshed summary first,
	// so the next sweep derives correctly (Rust TransientChainReadError).
	refreshed := make([]fleet.KaminoPositionAccounts, len(derived))
	drift := ""
	for i, position := range derived {
		reserve := accounts[position.Reserve]
		if reserve == nil {
			return out, sweepInvariant("reserve account %s does not exist", position.Reserve)
		}
		if refreshed[i], err = fleet.DecodeReserveIdentity(reserve); err != nil {
			return out, sweepInvariant("%v", err)
		}
		if !sameDerivation(position, refreshed[i]) {
			drift = position.Reserve
		}
	}
	p.mu.Lock()
	for _, summary := range refreshed {
		p.summaries[summary.Reserve] = summary
	}
	p.mu.Unlock()
	if drift != "" {
		return out, sweepTransport(fmt.Errorf("reserve %s address-derivation identity changed during coherent account read; retry with the refreshed cache", drift))
	}
	for i, position := range derived {
		vaultLiquidity, ataExists, err := positionSweepTokenAmount(accounts[position.VaultLiquidityATA], position.LiquidityMint, position.LiquidityTokenProgram)
		if err != nil {
			return out, err
		}
		obligation, err := positionSweepObligation(accounts[position.Obligation], vault, position.Market, position.Reserve)
		if err != nil {
			return out, err
		}
		for _, referenced := range append(append([]string(nil), obligation.deposits...), obligation.borrows...) {
			if !containsReserve(reserves, referenced) {
				return out, sweepInvariant("chain obligation %s references reserve %s outside the active policy's stable reserve universe", position.Obligation, referenced)
			}
		}
		if position.ObligationFarmUserState != "" {
			if a := accounts[position.ObligationFarmUserState]; a != nil && a.Owner.String() != fleet.FarmsProgram {
				return out, sweepInvariant("account %s is owned by %s, expected %s", a.Key, a.Owner, fleet.FarmsProgram)
			}
		}
		redeemable, err := redeemableCollateral(accounts[position.Reserve].Data, obligation.amount)
		if err != nil {
			return out, sweepInvariant("%v", err)
		}
		if refreshed[i].Market != reserves[i].market || refreshed[i].LiquidityMint != reserves[i].mint {
			return out, sweepInvariant("chain position identity falls outside the active policy's stable reserve universe")
		}
		out.positions = append(out.positions, positionSweepPosition{reserve: position.Reserve, market: position.Market, mint: position.LiquidityMint, amount: obligation.amount, redeemable: redeemable, vaultLiquidity: vaultLiquidity, obligation: position.Obligation, ata: position.VaultLiquidityATA, obligationExists: obligation.exists, ataExists: ataExists})
	}
	return out, nil
}

// positionSweepTokenAmount reads a vault token account: a missing account is
// zero; an existing one must be a valid account of the expected token
// program and mint.
func positionSweepTokenAmount(a *chain.Account, mint, program string) (int64, bool, error) {
	if a == nil {
		return 0, false, nil
	}
	held, err := spl.DecodeTokenAccount(a)
	if err != nil || held.Program.String() != program || held.Mint.String() != mint {
		return 0, false, sweepInvariant("token account %s is not a %s account of mint %s: %v", a.Key, program, mint, err)
	}
	if held.Amount > math.MaxInt64 {
		return 0, false, sweepInvariant("token account %s balance does not fit Postgres BIGINT", a.Key)
	}
	return int64(held.Amount), true, nil
}

type positionSweepObligationSummary struct {
	exists            bool
	amount            int64
	deposits, borrows []string
}

// positionSweepObligation is Rust decode_kamino_obligation_summary.
func positionSweepObligation(a *chain.Account, owner, market, reserve string) (positionSweepObligationSummary, error) {
	var out positionSweepObligationSummary
	if a == nil {
		return out, nil
	}
	if a.Owner.String() != fleet.KLendProgram {
		return out, sweepInvariant("obligation account %s is owned by %s, expected %s", a.Key, a.Owner, fleet.KLendProgram)
	}
	if len(a.Data) != positionSweepObligationLength || !bytes.Equal(a.Data[:8], positionSweepObligationDiscriminator) {
		return out, sweepInvariant("obligation account %s has invalid layout", a.Key)
	}
	if sdk.PublicKeyFromBytes(a.Data[64:96]).String() != owner {
		return out, sweepInvariant("obligation account %s owner does not match vault %s", a.Key, owner)
	}
	if sdk.PublicKeyFromBytes(a.Data[32:64]).String() != market {
		return out, sweepInvariant("obligation account %s market does not match reserve market %s", a.Key, market)
	}
	out.exists = true
	found := false
	for i := 0; i < 8; i++ {
		offset := 96 + i*136
		key := sdk.PublicKeyFromBytes(a.Data[offset : offset+32])
		if key.IsZero() {
			continue
		}
		out.deposits = append(out.deposits, key.String())
		if !found && key.String() == reserve {
			found = true
			amount := binary.LittleEndian.Uint64(a.Data[offset+32 : offset+40])
			if amount > math.MaxInt64 {
				return out, sweepInvariant("obligation collateral does not fit Postgres BIGINT")
			}
			out.amount = int64(amount)
		}
	}
	for i := 0; i < 5; i++ {
		offset := 1208 + i*200
		key := sdk.PublicKeyFromBytes(a.Data[offset : offset+32])
		if !key.IsZero() {
			out.borrows = append(out.borrows, key.String())
		}
	}
	return out, nil
}

func (o positionSweepObservation) contextJSON() ([]byte, error) { return json.Marshal(o.context) }
