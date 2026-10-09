package fleet

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"time"

	solana "github.com/gagliardetto/solana-go"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/backyard"
)

// FarmsProgram owns Kamino obligation farm user states.
const FarmsProgram = farmsProgram

const (
	farmsProgram = "FarmsPZpWu9i7Kky8tPN37rs2TpmMrAZrC7S7vJa91Hr"
	altProgram   = "AddressLookupTab1e1111111111111111111111111"
)

// revalidationStore is the narrow durable surface Cycle uses.
type revalidationStore interface {
	ClaimRevalidation(ctx context.Context, cluster, owner string, ttl time.Duration, includeReady, crossMintEnabled bool, delegatedSigner ...string) (*RevalidationLease, error)
	CheckRevalidationLease(ctx context.Context, lease RevalidationLease) error
	RefreshTargetCapacity(ctx context.Context, cluster, reserve, mint string, supply, slot int64) error
	LoadReusableLookupTables(ctx context.Context, cluster string, vaultID, minimumSlot int64, requiredAddresses []string) ([]LookupTable, error)
	CommitRevalidation(ctx context.Context, lease RevalidationLease, input RevalidationCommit) error
	EligibleFeePayerShards(ctx context.Context, cluster, policySigner string, mounted []string) ([]FeePayerShard, error)
}

type Revalidator struct {
	store                    revalidationStore
	rpc                      *RPCClient
	owner                    string
	signer                   string
	leaseTTL                 time.Duration
	computeLimit             uint64
	slotDuration             time.Duration
	fusedExecute             bool
	crossMintEnabled         bool
	crossMintMaxValueLossBPS uint16
	crossMintMaxSlippageBPS  uint16
	jupiter                  *JupiterBuildClient
	feeOnlyPayers            []string
}

type RevalidatorConfig struct {
	Owner, DelegatedSigner   string
	LeaseTTL                 time.Duration
	ComputeLimit             uint64
	SlotDuration             time.Duration
	FusedExecute             bool
	CrossMintEnabled         bool
	CrossMintMaxValueLossBPS uint16
	CrossMintMaxSlippageBPS  uint16
	JupiterBuildURL          string
	JupiterAPIKey            string
	// FeeOnlyPayers are the mounted fee-only keys (public); the registry says which are eligible.
	FeeOnlyPayers []string
}

func NewRevalidator(store *Store, rpc *RPCClient, config RevalidatorConfig) (*Revalidator, error) {
	return newRevalidator(store, rpc, config)
}

func newRevalidator(store revalidationStore, rpc *RPCClient, config RevalidatorConfig) (*Revalidator, error) {
	if store == nil || rpc == nil || config.Owner == "" || config.DelegatedSigner == "" || config.LeaseTTL < time.Second {
		return nil, errors.New("store, RPC, owner, signer, and lease TTL are required")
	}
	if _, err := decodePublicKey(config.DelegatedSigner); err != nil {
		return nil, fmt.Errorf("delegated signer: %w", err)
	}
	if config.ComputeLimit == 0 {
		config.ComputeLimit = defaultComputeLimit
	}
	if config.ComputeLimit > defaultComputeLimit {
		return nil, errors.New("compute limit exceeds Solana maximum")
	}
	if config.SlotDuration <= 0 {
		return nil, errors.New("Kamino slot duration is required")
	}
	var jupiter *JupiterBuildClient
	if config.CrossMintEnabled {
		if config.CrossMintMaxValueLossBPS == 0 || config.CrossMintMaxValueLossBPS > 1_000 || config.CrossMintMaxSlippageBPS == 0 || config.CrossMintMaxSlippageBPS > 1_000 {
			return nil, errors.New("cross-mint value-loss or slippage bound is invalid")
		}
		var err error
		jupiter, err = NewJupiterBuildClient(config.JupiterBuildURL, config.JupiterAPIKey)
		if err != nil {
			return nil, err
		}
	}
	return &Revalidator{store: store, rpc: rpc, owner: config.Owner, signer: config.DelegatedSigner, leaseTTL: config.LeaseTTL, computeLimit: config.ComputeLimit, slotDuration: config.SlotDuration, fusedExecute: config.FusedExecute, crossMintEnabled: config.CrossMintEnabled, crossMintMaxValueLossBPS: config.CrossMintMaxValueLossBPS, crossMintMaxSlippageBPS: config.CrossMintMaxSlippageBPS, jupiter: jupiter, feeOnlyPayers: append([]string(nil), config.FeeOnlyPayers...)}, nil
}

// Cycle claims at most one row. Claim, fresh-chain preparation, and commit are
// deliberately separate transactions; CommitRevalidation rechecks every
// mutable identity, lease, epoch, conflict, and capacity fence atomically.
func (r *Revalidator) Cycle(ctx context.Context, cluster string) (bool, error) {
	if r.fusedExecute {
		return false, errors.New("fused execution requires PrepareExecution and atomic signed publication")
	}
	lease, err := r.store.ClaimRevalidation(ctx, cluster, r.owner, r.leaseTTL, r.fusedExecute, r.crossMintEnabled, r.signer)
	if err != nil || lease == nil {
		return false, err
	}
	ctx, cancel := context.WithDeadline(ctx, lease.ExpiresAt.Add(-5*time.Second))
	defer cancel()
	if !contains(lease.DelegatedSigners, r.signer) {
		return true, errors.New("claimed policy no longer delegates to configured signer")
	}
	if lease.RouteKind == "cross_mint_jupiter" {
		return true, r.cycleCrossMint(ctx, *lease)
	}
	prepared, _, err := r.prepareSameMint(ctx, cluster, *lease, func(evidence FreshRouteEvidence) error {
		if r.fusedExecute {
			if err := r.store.RefreshTargetCapacity(ctx, cluster, lease.TargetReserve, lease.LiquidityMint, evidence.TargetObservedSupplyUSDMicros, evidence.Slot); err != nil {
				return fmt.Errorf("refresh fused target capacity: %w", err)
			}
		}
		return r.store.CheckRevalidationLease(ctx, *lease)
	})
	if err != nil {
		return true, err
	}
	if prepared.WaitingALT {
		return true, r.store.CommitRevalidation(ctx, *lease, RevalidationCommit{Disposition: "waiting_alt", Preparation: &prepared.Preparation, MissingAddresses: prepared.Missing, ExpectedEpochFingerprint: lease.OptimizerEpochKey, ExpectedOpportunityKey: lease.IdempotencyKey})
	}
	evidence := prepared.Evidence
	disposition := "ready"
	if r.fusedExecute {
		disposition = "fused_execute"
	}
	return true, r.store.CommitRevalidation(ctx, *lease, RevalidationCommit{Disposition: disposition, Preparation: &prepared.Preparation, ConflictKeys: prepared.Preparation.Transaction.WritableAccounts, ExpectedEpochFingerprint: lease.OptimizerEpochKey, ExpectedOpportunityKey: lease.IdempotencyKey, FreshEconomics: true, ObservedSourceAPYBPS: evidence.ObservedSourceAPYBPS, ObservedTargetAPYBPS: evidence.ObservedTargetAPYBPS, TargetObservedSupplyUSDMicros: evidence.TargetObservedSupplyUSDMicros, TargetObservedSlot: evidence.Slot})
}

// sameMintPreparation is everything Cycle needs to commit.
type sameMintPreparation struct {
	LastValidBlockHeight int64
	Evidence             FreshRouteEvidence
	Preparation          RoutePreparation
	WaitingALT           bool
	Missing              []string
	Compute              uint64
	Fee                  uint64
	PriorityFee          uint64
	Tables               []LookupTable
	Instructions         []RouteInstruction
	// FeePayer pays this route: the policy signer or a fee-only shard.
	FeePayer string
}

// feePayer is Rust's select_same_mint_route_fee_payer: a mature route goes
// to the first healthy shard in the vault's rendezvous order; anything else,
// or no healthy shard, falls back to the policy signer. A route that creates
// accounts is never mature: the payer funds rent, which a fee-only key never
// does (b1ad5b1a). Admission rechecks the chosen shard's budget against a
// fresh balance under its row lock.
func (r *Revalidator) feePayer(ctx context.Context, cluster string, lease RevalidationLease, slot int64, mature bool) string {
	if !mature || len(r.feeOnlyPayers) == 0 {
		return r.signer
	}
	shards, err := r.store.EligibleFeePayerShards(ctx, cluster, r.signer, r.feeOnlyPayers)
	if err != nil || len(shards) == 0 {
		return r.signer
	}
	byPayer := map[string]FeePayerShard{}
	payers := []string{}
	for _, shard := range shards {
		byPayer[shard.Payer] = shard
		payers = append(payers, shard.Payer)
	}
	ranked := RankFeePayers(cluster, lease.VaultPubkey, payers)
	_, accounts, err := r.rpc.ConfirmedAccounts(ctx, ranked, slot)
	if err != nil || len(accounts) != len(ranked) {
		return r.signer
	}
	for i, payer := range ranked {
		if accounts[i].Lamports <= math.MaxInt64 && byPayer[payer].healthy(int64(accounts[i].Lamports), lease.FeeCapLamports) {
			return payer
		}
	}
	return r.signer
}

// prepareSameMint runs the read-only same-mint preparation from fresh chain
// evidence through the final simulated route. afterEvidence runs once between
// loadFreshRoute and proxy build; the durable path uses it for the fused
// capacity refresh and lease check. The returned stage names the failing step.
func (r *Revalidator) prepareSameMint(ctx context.Context, cluster string, lease RevalidationLease, afterEvidence func(FreshRouteEvidence) error) (sameMintPreparation, string, error) {
	var out sameMintPreparation
	fresh, err := r.loadFreshRoute(ctx, lease)
	if err != nil {
		return out, "load_fresh_route", err
	}
	input, evidence := fresh.input, fresh.evidence
	out.Evidence = evidence
	if afterEvidence != nil {
		if err := afterEvidence(evidence); err != nil {
			return out, "lease_check", err
		}
	}
	route, err := BuildSameMintRoute(input)
	if err != nil {
		return out, "klend_build", err
	}
	body, policies, err := wrapSameMintRoute(route, r.signer, lease.VaultIndex, lease.PolicyAccount, fresh.routePolicy, lease.SetupPolicyAccount, fresh.setupPolicy)
	if err != nil {
		return out, "wrap_policy", err
	}
	tables, err := r.store.LoadReusableLookupTables(ctx, cluster, lease.VaultID, evidence.Slot, requiredLookupTableAddresses(body))
	if err != nil {
		return out, "lookup_tables", err
	}
	tables, err = r.verifyLookupTables(ctx, tables, evidence.Slot)
	if err != nil {
		return out, "lookup_tables", err
	}
	out.Tables = tables
	// Finalized, as Rust compiles every route; see finalizedBlockhash. The
	// evidence slot is confirmed, so it cannot floor a finalized read.
	blockhash, lastValidBlockHeight, err := r.rpc.finalizedBlockhash(ctx, 0)
	if err != nil {
		return out, "blockhash", err
	}
	out.LastValidBlockHeight = lastValidBlockHeight
	out.FeePayer = r.feePayer(ctx, cluster, lease, evidence.Slot, !input.setup())
	manifest, err := sameMintRouteALTManifest(input, policies.settings, lease.PolicyAccount, lease.SetupPolicyAccount, out.FeePayer, body)
	if err != nil {
		return out, "alt_manifest", err
	}
	preview, missing, err := compileV0Transaction(out.FeePayer, blockhash, append(computeBudgetInstructions(uint32(r.computeLimit), 0), body...), tables, 1, r.computeLimit)
	if err != nil {
		return out, "compile", err
	}
	if len(missing) > 0 || len(preview.LookupTables) == 0 {
		preparation := waitingALTPreparation(missing, r.computeLimit)
		preparation.RouteFingerprint = retainedSameMintRouteFingerprint(lease)
		preparation.RequirementsFingerprint = manifest.Fingerprint
		preparation.Manifest = &manifest
		if err := preserveCanonicalPlan(lease.ExecutionPlan, &preparation, "alt_readiness"); err != nil {
			return out, "compile", err
		}
		out.WaitingALT, out.Missing, out.Preparation = true, missing, preparation
		return out, "", nil
	}
	baselineSimulation, err := r.rpc.SimulateExactTransaction(ctx, preview.UnsignedWire, evidence.Slot)
	if err != nil {
		return out, "baseline_simulation", fmt.Errorf("baseline exact simulation failed: %w", err)
	}
	if !baselineSimulation.Succeeded {
		return out, "baseline_simulation", fmt.Errorf("baseline exact simulation failed: %s", baselineSimulation.Error)
	}
	compute := paddedComputeUnits(baselineSimulation.UnitsConsumed)
	if compute > r.computeLimit {
		return out, "baseline_simulation", fmt.Errorf("measured compute requirement %d exceeds configured limit %d", compute, r.computeLimit)
	}
	out.Compute = compute
	baselineFee, err := r.rpc.FeeForMessage(ctx, preview.Message, evidence.Slot)
	if err != nil {
		return out, "fee", err
	}
	recentPriority, err := r.rpc.RecentPriorityFee(ctx, preview.WritableAccounts)
	if err != nil {
		return out, "fee", err
	}
	remaining := uint64(0)
	if baselineFee < uint64(lease.FeeCapLamports) {
		remaining = uint64(lease.FeeCapLamports) - baselineFee
	}
	cappedPriority := uint64(0)
	if remaining <= ^uint64(0)/1_000_000 {
		cappedPriority = remaining * 1_000_000 / compute
	} else {
		cappedPriority = ^uint64(0)
	}
	if recentPriority > cappedPriority {
		recentPriority = cappedPriority
	}
	out.PriorityFee = recentPriority
	out.Instructions = append(computeBudgetInstructions(uint32(compute), recentPriority), body...)
	budgetPreview, missing, err := compileV0Transaction(out.FeePayer, blockhash, out.Instructions, tables, 1, compute)
	if err != nil {
		return out, "budgeted_compile", fmt.Errorf("budgeted transaction compilation: %w", err)
	}
	if len(missing) > 0 {
		return out, "budgeted_compile", fmt.Errorf("budgeted ALT compilation changed coverage: %v", missing)
	}
	fee, err := r.rpc.FeeForMessage(ctx, budgetPreview.Message, evidence.Slot)
	if err != nil {
		return out, "fee", err
	}
	if fee > uint64(lease.FeeCapLamports) {
		return out, "fee", fmt.Errorf("budgeted fee %d exceeds opportunity cap %d", fee, lease.FeeCapLamports)
	}
	out.Fee = fee
	preparation, err := prepareRoute(out.Instructions, out.FeePayer, tables, blockhash, fee, compute, func(wire []byte) (SimulationEvidence, error) {
		return r.rpc.SimulateExactTransaction(ctx, wire, evidence.Slot)
	}, "same_mint_kamino_v0")
	if err != nil {
		return out, "prepare_route", err
	}
	preparation.RouteFingerprint = retainedSameMintRouteFingerprint(lease)
	preparation.RequirementsFingerprint = manifest.Fingerprint
	preparation.Manifest = &manifest
	if err := preserveCanonicalPlan(lease.ExecutionPlan, &preparation, "prepared_transaction"); err != nil {
		return out, "prepare_route", err
	}
	out.Preparation = preparation
	return out, "", nil
}

// sameMintPolicies are the policies a wrapped route executes under.
type sameMintPolicies struct {
	settings string
	accounts []string
}

// wrapSameMintRoute replaces each protected instruction, in route order, with
// its own Squads ProgramInteraction execution under an exact constraint:
// withdrawal and deposit under the route policy; init_obligation under the
// route policy when it carries a market-scoped init constraint, else under
// the vault's setup policy (Rust's resolve_init_obligation_policy, ee8715ad).
func wrapSameMintRoute(route []RouteInstruction, signer string, vaultIndex uint8, routePolicy string, routeData []byte, setupPolicy string, setupData []byte) ([]RouteInstruction, sameMintPolicies, error) {
	var used sameMintPolicies
	decode := func(data []byte) (DecodedSquadsPolicy, error) {
		p, err := DecodeSquadsPolicy(data)
		if err == nil && p.AccountIndex != vaultIndex {
			err = errors.New("policy account index differs from managed vault index")
		}
		return p, err
	}
	routeDecoded, err := decode(routeData)
	if err != nil {
		return nil, used, err
	}
	used.settings = routeDecoded.Settings
	var protected []RouteInstruction
	initAt := -1
	for _, ix := range route {
		if ix.Protected {
			if ix.Step == "kamino_init_obligation" {
				initAt = len(protected)
			}
			protected = append(protected, ix)
		}
	}
	accounts := make([]string, len(protected))
	for i := range accounts {
		accounts[i] = routePolicy
	}
	matched, err := validateDelegatedInstructions(routeDecoded, signer, protected)
	indexes := matched.AllowedIndexes
	if err != nil {
		if initAt < 0 {
			return nil, used, err
		}
		rest := append(append([]RouteInstruction{}, protected[:initAt]...), protected[initAt+1:]...)
		if matched, err = validateDelegatedInstructions(routeDecoded, signer, rest); err != nil {
			return nil, used, err
		}
		if len(setupData) == 0 {
			return nil, used, errors.New("target obligation is missing and no route or setup policy authorizes init_obligation")
		}
		setupDecoded, err := decode(setupData)
		if err != nil {
			return nil, used, err
		}
		if setupDecoded, err = validateDelegatedInstructions(setupDecoded, signer, protected[initAt:initAt+1]); err != nil {
			return nil, used, fmt.Errorf("setup policy: %w", err)
		}
		indexes = append(append(append([]uint8{}, matched.AllowedIndexes[:initAt]...), setupDecoded.AllowedIndexes[0]), matched.AllowedIndexes[initAt:]...)
		accounts[initAt] = setupPolicy
	}
	used.accounts = canonicalStrings(accounts)
	body := make([]RouteInstruction, 0, len(route))
	next := 0
	for _, ix := range route {
		if !ix.Protected {
			body = append(body, ix)
			continue
		}
		wrapped, err := wrapSquadsPolicy(accounts[next], signer, vaultIndex, []uint8{indexes[next]}, []RouteInstruction{ix})
		if err != nil {
			return nil, used, err
		}
		body = append(body, wrapped)
		next++
	}
	return body, used, nil
}

// sameMintRouteALTManifest is the requirements manifest Rust records for a
// same-mint route (fleet-worker lib.rs build_route_execution_plan, resolved
// at :8038): the route's own instructions, without the compute-budget
// instructions the final wire adds, paid by the route's selected fee payer,
// and typed by same_mint_outer_lookup_table_requirements (:18844), which
// names the route policy and the vault's setup policy. Membership follows the
// instructions, so the setup policy enters only when init runs under it.
func sameMintRouteALTManifest(input KaminoSameMintRouteRequest, settings, routePolicy, setupPolicy, feePayer string, body []RouteInstruction) (ALTManifest, error) {
	policies := []string{routePolicy}
	if setupPolicy != "" && setupPolicy != routePolicy {
		policies = append(policies, setupPolicy)
	}
	return buildRouteALTManifest(input, settings, policies, feePayer, body, nil, true)
}

func preserveCanonicalPlan(original json.RawMessage, preparation *RoutePreparation, evidenceField string) error {
	var plan, evidence map[string]any
	if len(original) == 0 || json.Unmarshal(original, &plan) != nil {
		return errors.New("canonical execution plan is invalid")
	}
	kind, _ := plan["kind"].(string)
	if kind != "same_mint" && kind != "cross_mint_jupiter" {
		return errors.New("canonical execution plan kind is invalid")
	}
	if json.Unmarshal(preparation.ExecutionPlan, &evidence) != nil {
		return errors.New("prepared route evidence is invalid")
	}
	plan[evidenceField] = evidence
	merged, err := json.Marshal(plan)
	if err != nil {
		return err
	}
	preparation.ExecutionPlan = merged
	return nil
}

func paddedComputeUnits(measured uint64) uint64 {
	scaled := measured
	if measured > ^uint64(0)/115 {
		scaled = ^uint64(0)
	} else {
		scaled *= 115
	}
	padded := scaled / 100
	if scaled%100 != 0 {
		padded++
	}
	if padded > ^uint64(0)-10_000 {
		padded = ^uint64(0)
	} else {
		padded += 10_000
	}
	if padded < 100_000 {
		return 100_000
	}
	if padded > defaultComputeLimit {
		return defaultComputeLimit
	}
	return padded
}

func computeBudgetInstructions(limit uint32, price uint64) []RouteInstruction {
	limitData := make([]byte, 5)
	limitData[0] = 2
	limitData[1] = byte(limit)
	limitData[2] = byte(limit >> 8)
	limitData[3] = byte(limit >> 16)
	limitData[4] = byte(limit >> 24)
	priceData := []byte{3}
	for i := 0; i < 8; i++ {
		priceData = append(priceData, byte(price))
		price >>= 8
	}
	return []RouteInstruction{{Step: "compute_unit_limit", Program: "ComputeBudget111111111111111111111111111111", Data: limitData}, {Step: "compute_unit_price", Program: "ComputeBudget111111111111111111111111111111", Data: priceData}}
}

// Match the retained executor's stable_fingerprint identity contract. Exact
// account requirements are fenced separately by the typed manifest hash.
func retainedSameMintRouteFingerprint(lease RevalidationLease) string {
	hash := sha256.New()
	for _, part := range []string{"same_mint_kamino", lease.Cluster, fmt.Sprint(lease.VaultID), lease.SourceReserve, lease.TargetReserve} {
		var size [8]byte
		binary.LittleEndian.PutUint64(size[:], uint64(len(part)))
		hash.Write(size[:])
		hash.Write([]byte(part))
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func waitingALTPreparation(missing []string, compute uint64) RoutePreparation {
	plan, _ := json.Marshal(struct {
		Kind    string   `json:"kind"`
		Missing []string `json:"missing_alt_addresses"`
		Compute uint64   `json:"compute_unit_limit"`
	}{"same_mint_kamino_waiting_alt", canonicalStrings(missing), compute})
	// Source fingerprints come from the actual route and typed manifest; this
	// metadata helper cannot invent them from missing keys or compute values.
	return RoutePreparation{ExecutionPlan: plan}
}

type decodedRoutePosition struct {
	Position   KaminoPositionAccounts
	Obligation string
	FarmUser   string
}

// freshSameMint is the coherent chain state one same-mint route is built from.
type freshSameMint struct {
	input                    KaminoSameMintRouteRequest
	evidence                 FreshRouteEvidence
	routePolicy, setupPolicy []byte
}

// maxObligationRentLamports bounds the payer's rent top-up to the vault
// (Rust's MAX_KAMINO_OBLIGATION_RENT_LAMPORTS, 515306a9).
const maxObligationRentLamports = 25_000_000

// vaultRentTopUp is the payer's transfer that lets the vault pay its new
// obligation's rent from inside its policy: exactly the vault's deficit.
func vaultRentTopUp(rent, vaultLamports uint64) (uint64, error) {
	if rent > maxObligationRentLamports {
		return 0, fmt.Errorf("route_setup_rent_cap_exceeded: obligation rent %d exceeds %d lamports", rent, maxObligationRentLamports)
	}
	if vaultLamports >= rent {
		return 0, nil
	}
	return rent - vaultLamports, nil
}

func (r *Revalidator) loadFreshRoute(ctx context.Context, lease RevalidationLease) (freshSameMint, error) {
	var f freshSameMint
	minimum, err := r.rpc.ConfirmedSlot(ctx)
	if err != nil {
		return f, err
	}
	_, preliminary, err := r.rpc.ConfirmedAccounts(ctx, []string{lease.SourceReserve, lease.TargetReserve}, minimum)
	if err != nil {
		return f, err
	}
	source, err := decodeRouteReserve(preliminary[0], lease.VaultPubkey)
	if err != nil {
		return f, err
	}
	target, err := decodeRouteReserve(preliminary[1], lease.VaultPubkey)
	if err != nil {
		return f, err
	}
	if source.Position.LiquidityMint != lease.LiquidityMint || target.Position.LiquidityMint != lease.LiquidityMint {
		return f, errors.New("fresh reserve mint differs from opportunity")
	}
	// A missing target obligation or obligation farm user is setup the route
	// performs itself, so those reads may be absent. The vault is a Squads PDA
	// that holds lamports only once funded; its balance decides the rent top-up.
	addresses := []string{lease.SourceReserve, lease.TargetReserve, source.Obligation, target.Obligation, source.Position.VaultLiquidityATA, lease.PolicyAccount, lease.VaultPubkey}
	if lease.SetupPolicyAccount != "" {
		addresses = append(addresses, lease.SetupPolicyAccount)
	}
	farmUsers := len(addresses)
	for _, position := range []*decodedRoutePosition{&source, &target} {
		if position.FarmUser != "" {
			addresses = append(addresses, position.FarmUser)
		}
	}
	minimum, err = r.rpc.ConfirmedSlot(ctx)
	if err != nil {
		return f, err
	}
	slot, accounts, err := r.rpc.accounts(ctx, addresses, minimum, "confirmed", true)
	if err != nil {
		return f, err
	}
	freshSource, err := decodeRouteReserve(accounts[0], lease.VaultPubkey)
	if err != nil {
		return f, err
	}
	freshTarget, err := decodeRouteReserve(accounts[1], lease.VaultPubkey)
	if err != nil {
		return f, err
	}
	if !reflect.DeepEqual(freshSource.Position, source.Position) || !reflect.DeepEqual(freshTarget.Position, target.Position) {
		return f, errors.New("reserve route identities changed during coherent observation")
	}
	sourceCollateral, err := decodeObligation(accounts[2], freshSource.Position.Market, lease.VaultPubkey, lease.SourceReserve, &freshSource.Position)
	if err != nil {
		return f, err
	}
	if lease.SourceCollateralRaw > 0 && sourceCollateral != lease.SourceCollateralRaw {
		return f, errors.New("fresh source collateral amount differs from opportunity")
	}
	targetMissing := absent(accounts[3])
	var targetCollateral uint64
	if !targetMissing {
		if targetCollateral, err = decodeObligation(accounts[3], freshTarget.Position.Market, lease.VaultPubkey, "", &freshTarget.Position); err != nil {
			return f, err
		}
		for i := 0; i < 8; i++ {
			offset := 96 + i*136
			if encodeBase58(accounts[3].Data[offset:offset+32]) == lease.TargetReserve {
				targetCollateral = binary.LittleEndian.Uint64(accounts[3].Data[offset+32 : offset+40])
			}
		}
	}
	if freshSource.Position.LiquidityTokenProgram != freshTarget.Position.LiquidityTokenProgram || accounts[4].Owner != freshSource.Position.LiquidityTokenProgram {
		return f, errors.New("same-mint reserve token programs differ from vault custody")
	}
	if err := validateVaultTokenAccount(accounts[4], lease.LiquidityMint, lease.VaultPubkey); err != nil {
		return f, err
	}
	if accounts[5].Owner != SquadsProgram {
		return f, errors.New("fresh policy account owner mismatch")
	}
	f.routePolicy = accounts[5].Data
	if lease.SetupPolicyAccount != "" {
		f.setupPolicy = accounts[7].Data
	}
	missingFarmUser := []bool{false, false}
	for i, position := range []decodedRoutePosition{freshSource, freshTarget} {
		if position.FarmUser != "" {
			missingFarmUser[i] = absent(accounts[farmUsers])
			farmUsers++
		}
	}
	sourceEconomics, err := DecodeKaminoReserve(accounts[0], ReserveIdentity{Address: lease.SourceReserve, Market: freshSource.Position.Market, Mint: lease.LiquidityMint}, slot, r.slotDuration)
	if err != nil {
		return f, fmt.Errorf("decode fresh source economics: %w", err)
	}
	targetEconomics, err := DecodeKaminoReserve(accounts[1], ReserveIdentity{Address: lease.TargetReserve, Market: freshTarget.Position.Market, Mint: lease.LiquidityMint}, slot, r.slotDuration)
	if err != nil {
		return f, fmt.Errorf("decode fresh target economics: %w", err)
	}
	// The route withdraws all source collateral, so it deposits what that
	// collateral redeems in this bank, not the planning estimate: depositing
	// the estimate left the interest accrued since planning as vault idle that
	// nothing drains (the residue Autodeposit tolerates). KLend redeems at
	// least this exact floor, and its in-transaction refresh only accrues, so
	// the deposit never consumes pre-existing idle custody. The estimate must
	// still be backed: a stale high one means the plan no longer holds.
	redeemable, err := backyard.KaminoRedeemableLiquidity(backyard.ConfirmedAccount{Address: accounts[0].Address, Owner: accounts[0].Owner, Lamports: accounts[0].Lamports, Data: accounts[0].Data, Executable: accounts[0].Executable}, freshSource.Position.Market, lease.LiquidityMint, sourceCollateral)
	if err != nil {
		return f, fmt.Errorf("fresh collateral backing: %w", err)
	}
	if lease.LiquidityAmountRaw == 0 || lease.LiquidityAmountRaw > redeemable || lease.PrincipalUSDMicros <= 0 || uint64(lease.PrincipalUSDMicros) != lease.LiquidityAmountRaw {
		return f, errors.New("planned same-mint deposit exceeds fresh collateral backing or stable principal differs")
	}
	var topUp uint64
	if targetMissing {
		rent, err := r.rpc.MinimumBalanceForRentExemption(ctx, obligationLength)
		if err != nil {
			return f, err
		}
		if topUp, err = vaultRentTopUp(rent, accounts[6].Lamports); err != nil {
			return f, err
		}
	}
	f.evidence = FreshRouteEvidence{ObservedAt: time.Now().UTC(), Slot: slot, ObservedSourceAPYBPS: sourceEconomics.SupplyAPYBPS, ObservedTargetAPYBPS: targetEconomics.SupplyAPYBPS, TargetObservedSupplyUSDMicros: targetEconomics.TotalSupplyUSDMicros, OpportunityID: lease.OpportunityID, OpportunityKey: lease.IdempotencyKey, EpochID: lease.OptimizerEpochID, EpochFingerprint: lease.OptimizerEpochKey}
	f.evidence.Anchors = ExecutionBalanceAnchors{SourceObligation: source.Obligation, TargetObligation: target.Obligation, VaultLiquidityATA: source.Position.VaultLiquidityATA, SourceReserve: lease.SourceReserve, TargetReserve: lease.TargetReserve, SourceMarket: source.Position.Market, TargetMarket: target.Position.Market, SourceCollateralMint: source.Position.CollateralMint, TargetCollateralMint: target.Position.CollateralMint, LiquidityTokenProgram: source.Position.LiquidityTokenProgram, Owner: lease.VaultPubkey, Mint: lease.LiquidityMint, SourceCollateralRaw: sourceCollateral, TargetCollateralRaw: targetCollateral, IdleLiquidityRaw: binary.LittleEndian.Uint64(accounts[4].Data[64:72]), MinimumSlot: slot}
	f.input = KaminoSameMintRouteRequest{Vault: lease.VaultPubkey, Source: freshSource.Position, Target: freshTarget.Position, WithdrawCollateralAmount: sourceCollateral, DepositLiquidityAmount: redeemable,
		TargetObligationMissing: targetMissing, SourceFarmUserMissing: missingFarmUser[0], TargetFarmUserMissing: missingFarmUser[1], Payer: r.signer, VaultRentTopUpLamports: topUp}
	return f, nil
}

// obligationLength is the KLend Obligation account size, discriminator included.
const obligationLength = 3344

// absent reports an account the RPC returned as null.
func absent(a Account) bool { return a.Owner == "" && a.Lamports == 0 }

func decodeRouteReserve(account Account, vault string) (decodedRoutePosition, error) {
	position, err := DecodeReserveIdentity(account)
	if err != nil {
		return decodedRoutePosition{}, err
	}
	if position, err = DeriveVaultReserveAccounts(position, vault); err != nil {
		return decodedRoutePosition{}, err
	}
	return decodedRoutePosition{Position: position, Obligation: position.Obligation, FarmUser: position.ObligationFarmUserState}, nil
}

// DecodeReserveIdentity reads a KLend reserve's account identities: market,
// mints, supplies, token program, oracles and collateral farm (Rust
// decode_kamino_reserve_summary). Vault-derived fields stay empty.
func DecodeReserveIdentity(account Account) (KaminoPositionAccounts, error) {
	if account.Owner != KLendProgram || len(account.Data) != reserveLength || !bytes.Equal(account.Data[:8], reserveDiscriminator[:]) {
		return KaminoPositionAccounts{}, fmt.Errorf("reserve %s has invalid owner or data", account.Address)
	}
	key := func(offset int) string { return encodeBase58(account.Data[offset : offset+32]) }
	position := KaminoPositionAccounts{Reserve: account.Address, Market: key(32), LiquidityMint: key(128), CollateralMint: key(2560), LiquiditySupply: key(160), CollateralSupply: key(2600), LiquidityTokenProgram: key(408), PythOracle: key(5224), SwitchboardPriceOracle: key(5160), SwitchboardTWAPOracle: key(5192), ScopePrices: key(5112), ReserveFarmState: key(64)}
	for _, field := range []*string{&position.PythOracle, &position.SwitchboardPriceOracle, &position.SwitchboardTWAPOracle, &position.ScopePrices, &position.ReserveFarmState} {
		if *field == "11111111111111111111111111111111" {
			*field = ""
		}
	}
	return position, nil
}

// DeriveVaultReserveAccounts fills the vault's accounts in a decoded
// reserve's market: market authority, vanilla obligation, liquidity ATA under
// the reserve's own token program, and the obligation's collateral farm user.
func DeriveVaultReserveAccounts(position KaminoPositionAccounts, vault string) (KaminoPositionAccounts, error) {
	program, _ := solana.PublicKeyFromBase58(KLendProgram)
	marketKey, err := solana.PublicKeyFromBase58(position.Market)
	if err != nil {
		return KaminoPositionAccounts{}, err
	}
	vaultKey, err := solana.PublicKeyFromBase58(vault)
	if err != nil {
		return KaminoPositionAccounts{}, err
	}
	marketAuthority, _, err := solana.FindProgramAddress([][]byte{[]byte("lma"), marketKey[:]}, program)
	if err != nil {
		return KaminoPositionAccounts{}, err
	}
	zero := solana.PublicKey{}
	obligation, _, err := solana.FindProgramAddress([][]byte{{0}, {0}, vaultKey[:], marketKey[:], zero[:], zero[:]}, program)
	if err != nil {
		return KaminoPositionAccounts{}, err
	}
	mint, _ := solana.PublicKeyFromBase58(position.LiquidityMint)
	tokenProgram, _ := solana.PublicKeyFromBase58(position.LiquidityTokenProgram)
	associated, _ := solana.PublicKeyFromBase58("ATokenGPvbdGVxr1b2hvZbsiqW5xWH25efTNsLJA8knL")
	ata, _, err := solana.FindProgramAddress([][]byte{vaultKey[:], tokenProgram[:], mint[:]}, associated)
	if err != nil {
		return KaminoPositionAccounts{}, err
	}
	position.MarketAuthority, position.Obligation, position.VaultLiquidityATA = marketAuthority.String(), obligation.String(), ata.String()
	position.ObligationFarmUserState = ""
	if position.ReserveFarmState != "" {
		farmKey, err := solana.PublicKeyFromBase58(position.ReserveFarmState)
		if err != nil {
			return KaminoPositionAccounts{}, err
		}
		farmsKey, _ := solana.PublicKeyFromBase58(farmsProgram)
		user, _, err := solana.FindProgramAddress([][]byte{[]byte("user"), farmKey[:], obligation[:]}, farmsKey)
		if err != nil {
			return KaminoPositionAccounts{}, err
		}
		position.ObligationFarmUserState = user.String()
	}
	return position, nil
}

func decodeObligation(account Account, expectedMarket, expectedOwner, expectedDeposit string, position *KaminoPositionAccounts) (uint64, error) {
	obligationDiscriminator := [8]byte{168, 206, 141, 106, 88, 76, 172, 167}
	if account.Owner != KLendProgram || len(account.Data) != obligationLength || !bytes.Equal(account.Data[:8], obligationDiscriminator[:]) {
		return 0, fmt.Errorf("obligation %s has invalid owner or data", account.Address)
	}
	key := func(offset int) string { return encodeBase58(account.Data[offset : offset+32]) }
	if key(32) != expectedMarket || key(64) != expectedOwner {
		return 0, fmt.Errorf("obligation %s market or owner mismatch", account.Address)
	}
	var expectedAmount uint64
	for i := 0; i < 8; i++ {
		offset := 96 + i*136
		value := key(offset)
		if value != "11111111111111111111111111111111" {
			position.ObligationDepositReserves = append(position.ObligationDepositReserves, value)
			if value == expectedDeposit {
				expectedAmount = binary.LittleEndian.Uint64(account.Data[offset+32 : offset+40])
			}
		}
	}
	for i := 0; i < 5; i++ {
		value := key(1208 + i*200)
		if value != "11111111111111111111111111111111" {
			position.ObligationBorrowReserves = append(position.ObligationBorrowReserves, value)
		}
	}
	if expectedDeposit != "" && expectedAmount == 0 {
		return 0, errors.New("source obligation no longer contains the planned reserve")
	}
	return expectedAmount, nil
}

func validateVaultTokenAccount(account Account, expectedMint, expectedOwner string) error {
	if account.Executable || account.Lamports == 0 {
		return errors.New("vault token account is not funded token custody")
	}
	return validateStableAccount(account, expectedMint, expectedOwner)
}

func requiredLookupTableAddresses(instructions []RouteInstruction) []string {
	programs := make(map[string]bool, len(instructions))
	signers := map[string]bool{}
	for _, instruction := range instructions {
		programs[instruction.Program] = true
		for _, account := range instruction.Accounts {
			signers[account.Address] = signers[account.Address] || account.Signer
		}
	}
	var required []string
	for _, instruction := range instructions {
		for _, account := range instruction.Accounts {
			if !account.Signer && !programs[account.Address] && !signers[account.Address] {
				required = append(required, account.Address)
			}
		}
	}
	return canonicalStrings(required)
}

func (r *Revalidator) verifyLookupTables(ctx context.Context, tables []LookupTable, minimumSlot int64) ([]LookupTable, error) {
	return r.verifyRouteLookupTables(ctx, tables, minimumSlot, false)
}
func (r *Revalidator) verifyFinalizedLookupTables(ctx context.Context, tables []LookupTable, minimumSlot int64) ([]LookupTable, error) {
	return r.verifyRouteLookupTables(ctx, tables, minimumSlot, true)
}
func (r *Revalidator) verifyRouteLookupTables(ctx context.Context, tables []LookupTable, minimumSlot int64, finalized bool) ([]LookupTable, error) {
	const maximumGetMultipleAccounts = 100
	for start := 0; start < len(tables); start += maximumGetMultipleAccounts {
		end := start + maximumGetMultipleAccounts
		if end > len(tables) {
			end = len(tables)
		}
		addresses := make([]string, end-start)
		for i := start; i < end; i++ {
			addresses[i-start] = tables[i].Address
		}
		read := r.rpc.ConfirmedAccounts
		if finalized {
			read = r.rpc.FinalizedAccounts
		}
		observedSlot, accounts, err := read(ctx, addresses, minimumSlot)
		if err != nil {
			return nil, err
		}
		for offset, account := range accounts {
			table := tables[start+offset]
			if account.Address != table.Address || account.Executable || account.Lamports == 0 || account.Owner != altProgram || len(account.Data) < 56 || (len(account.Data)-56)%32 != 0 || binary.LittleEndian.Uint32(account.Data[:4]) != 1 || binary.LittleEndian.Uint64(account.Data[4:12]) != ^uint64(0) {
				return nil, fmt.Errorf("lookup table %s has invalid or deactivated chain data", account.Address)
			}
			if observedSlot <= 0 || binary.LittleEndian.Uint64(account.Data[12:20]) >= uint64(observedSlot) {
				return nil, fmt.Errorf("lookup table %s is not warm at observed slot", account.Address)
			}
			chain := make([]string, 0, (len(account.Data)-56)/32)
			for offset := 56; offset < len(account.Data); offset += 32 {
				chain = append(chain, encodeBase58(account.Data[offset:offset+32]))
			}
			if len(chain) != len(table.Addresses) {
				return nil, fmt.Errorf("lookup table %s database/chain length mismatch", account.Address)
			}
			for j := range chain {
				if chain[j] != table.Addresses[j] {
					return nil, fmt.Errorf("lookup table %s database/chain member mismatch", account.Address)
				}
			}
		}
	}
	return tables, nil
}
