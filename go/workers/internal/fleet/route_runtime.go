package fleet

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"slices"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
	solana "github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/spl"
)

const altProgram = "AddressLookupTab1e1111111111111111111111111"

type revalidationStore interface {
	ClaimRevalidation(ctx context.Context, cluster, owner string, ttl time.Duration, includeReady, crossMintEnabled bool, delegatedSigner ...string) (*RevalidationLease, error)
	CheckRevalidationLease(ctx context.Context, lease RevalidationLease) error
	LoadReusableLookupTables(ctx context.Context, cluster string, vaultID, minimumSlot int64, requiredAddresses []string) ([]LookupTable, error)
	CommitRevalidation(ctx context.Context, lease RevalidationLease, input RevalidationCommit) error
	EligibleFeePayerShards(ctx context.Context, cluster, policySigner string, mounted []string) ([]FeePayerShard, error)
}

type Revalidator struct {
	store                    revalidationStore
	rpc                      *chain.Client
	owner                    string
	signer                   string
	leaseTTL                 time.Duration
	computeLimit             uint64
	crossMintEnabled         bool
	crossMintMaxValueLossBPS uint16
	crossMintMaxSlippageBPS  uint16
	jupiter                  *JupiterBuildClient
	feeOnlyPayers            []string
	metrics                  ExecutionMetrics
}

type RevalidatorConfig struct {
	Owner, DelegatedSigner   string
	LeaseTTL                 time.Duration
	ComputeLimit             uint64
	CrossMintEnabled         bool
	CrossMintMaxValueLossBPS uint16
	CrossMintMaxSlippageBPS  uint16
	JupiterBuildURL          string
	JupiterAPIKey            string
	FeeOnlyPayers            []string
	Metrics                  ExecutionMetrics
}

func NewRevalidator(store *Store, client *chain.Client, config RevalidatorConfig) (*Revalidator, error) {
	return newRevalidator(store, client, config)
}

func newRevalidator(store revalidationStore, client *chain.Client, config RevalidatorConfig) (*Revalidator, error) {
	if store == nil || client == nil || config.Owner == "" || config.DelegatedSigner == "" || config.LeaseTTL < time.Second || config.Metrics == nil {
		return nil, errors.New("store, RPC, owner, signer, lease TTL and metrics are required")
	}
	if _, err := decodePublicKey(config.DelegatedSigner); err != nil {
		return nil, fmt.Errorf("delegated signer: %w", err)
	}
	if config.ComputeLimit == 0 {
		config.ComputeLimit = DefaultComputeLimit
	}
	if config.ComputeLimit > DefaultComputeLimit {
		return nil, errors.New("compute limit exceeds Solana maximum")
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
	return &Revalidator{store: store, rpc: client, owner: config.Owner, signer: config.DelegatedSigner, leaseTTL: config.LeaseTTL, computeLimit: config.ComputeLimit, crossMintEnabled: config.CrossMintEnabled, crossMintMaxValueLossBPS: config.CrossMintMaxValueLossBPS, crossMintMaxSlippageBPS: config.CrossMintMaxSlippageBPS, jupiter: jupiter, feeOnlyPayers: append([]string(nil), config.FeeOnlyPayers...), metrics: config.Metrics}, nil
}

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
	FeePayer             string
}

func (r *Revalidator) feePayer(ctx context.Context, cluster string, lease RevalidationLease, mature bool) string {
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
	_, accounts, err := ReadAccounts(ctx, r.rpc, ranked, rpc.CommitmentConfirmed, 0)
	if err != nil || slices.Contains(accounts, nil) {
		return r.signer
	}
	for i, payer := range ranked {
		if accounts[i].Lamports <= math.MaxInt64 && byPayer[payer].healthy(int64(accounts[i].Lamports), lease.FeeCapLamports) {
			return payer
		}
	}
	return r.signer
}

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
	tables, err = r.verifyRouteLookupTables(ctx, tables, evidence.Slot, false)
	if err != nil {
		return out, "lookup_tables", err
	}
	out.Tables = tables
	hash, lastValidBlockHeight, _, err := r.rpc.Blockhash(ctx, rpc.CommitmentFinalized, 0)
	if err != nil {
		return out, "blockhash", err
	}
	blockhash := hash.String()
	out.LastValidBlockHeight = int64(lastValidBlockHeight)
	out.FeePayer = r.feePayer(ctx, cluster, lease, !input.setup())
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
		preparation.RouteFingerprint, preparation.RequirementsFingerprint = manifest.Fingerprint, manifest.Fingerprint
		preparation.Manifest = &manifest
		if err := preserveCanonicalPlan(lease.ExecutionPlan, &preparation, "alt_readiness"); err != nil {
			return out, "compile", err
		}
		out.WaitingALT, out.Missing, out.Preparation = true, missing, preparation
		return out, "", nil
	}
	baselineSimulation, err := SimulateExact(ctx, r.rpc, preview.UnsignedWire, rpc.CommitmentConfirmed, evidence.Slot)
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
	baselineFee, err := chain.MessageFee(preview.Message)
	if err != nil {
		return out, "fee", err
	}
	recentPriority, err := recentPriorityFee(ctx, r.rpc, preview.WritableAccounts)
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
	fee, err := chain.MessageFee(budgetPreview.Message)
	if err != nil {
		return out, "fee", err
	}
	if fee > uint64(lease.FeeCapLamports) {
		return out, "fee", fmt.Errorf("budgeted fee %d exceeds opportunity cap %d", fee, lease.FeeCapLamports)
	}
	out.Fee = fee
	preparation, err := prepareRoute(out.Instructions, out.FeePayer, tables, blockhash, fee, compute, func(wire []byte) (SimulationEvidence, error) {
		return SimulateExact(ctx, r.rpc, wire, rpc.CommitmentConfirmed, evidence.Slot)
	}, "same_mint_kamino_v0")
	if err != nil {
		return out, "prepare_route", err
	}
	preparation.RouteFingerprint, preparation.RequirementsFingerprint = manifest.Fingerprint, manifest.Fingerprint
	preparation.Manifest = &manifest
	if err := preserveCanonicalPlan(lease.ExecutionPlan, &preparation, "prepared_transaction"); err != nil {
		return out, "prepare_route", err
	}
	out.Preparation = preparation
	return out, "", nil
}

type sameMintPolicies struct {
	settings string
	accounts []string
}

func wrapSameMintRoute(route []RouteInstruction, signer string, vaultIndex uint8, routePolicy string, routeAccount *chain.Account, setupPolicy string, setupAccount *chain.Account) ([]RouteInstruction, sameMintPolicies, error) {
	var used sameMintPolicies
	decode := func(account *chain.Account) (DecodedSquadsPolicy, error) {
		p, err := DecodeSquadsPolicy(account)
		if err == nil && p.AccountIndex != vaultIndex {
			err = errors.New("policy account index differs from managed vault index")
		}
		return p, err
	}
	routeDecoded, err := decode(routeAccount)
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
		if setupAccount == nil || len(setupAccount.Data) == 0 {
			return nil, used, errors.New("target obligation is missing and no route or setup policy authorizes init_obligation")
		}
		setupDecoded, err := decode(setupAccount)
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
	if padded > DefaultComputeLimit {
		return DefaultComputeLimit
	}
	return padded
}

func computeBudgetInstructions(limit uint32, price uint64) []RouteInstruction {
	return []RouteInstruction{RouteInstructionOf("compute_unit_limit", spl.SetComputeUnitLimit(limit)), RouteInstructionOf("compute_unit_price", spl.SetComputeUnitPrice(price))}
}

func RouteInstructionOf(step string, ix *solana.GenericInstruction) RouteInstruction {
	out := RouteInstruction{Step: step, Program: ix.ProgID.String(), Data: ix.DataBytes}
	for _, account := range ix.AccountValues {
		out.Accounts = append(out.Accounts, InstructionAccount{account.PublicKey.String(), account.IsSigner, account.IsWritable})
	}
	return out
}

func waitingALTPreparation(missing []string, compute uint64) RoutePreparation {
	plan, _ := json.Marshal(struct {
		Kind    string   `json:"kind"`
		Missing []string `json:"missing_alt_addresses"`
		Compute uint64   `json:"compute_unit_limit"`
	}{"same_mint_kamino_waiting_alt", canonicalStrings(missing), compute})
	return RoutePreparation{ExecutionPlan: plan}
}

type decodedRoutePosition struct {
	Position   KaminoPositionAccounts
	Obligation string
	FarmUser   string
}

type freshSameMint struct {
	input                    KaminoSameMintRouteRequest
	evidence                 FreshRouteEvidence
	routePolicy, setupPolicy *chain.Account
}

const maxObligationRentLamports = 25_000_000

// vaultRentTopUp is the payer's transfer that lets the vault pay its new
// obligation's rent from inside its policy: exactly what leaves the vault,
// a system account with no data, at 0 or at its rent-exempt minimum
// emptyRent or more. Between those the runtime refuses the transaction.
func vaultRentTopUp(rent, emptyRent, vaultLamports uint64) (uint64, error) {
	if rent > maxObligationRentLamports {
		return 0, fmt.Errorf("route_setup_rent_cap_exceeded: obligation rent %d exceeds %d lamports", rent, maxObligationRentLamports)
	}
	if vaultLamports < rent {
		return rent - vaultLamports, nil
	}
	if left := vaultLamports - rent; left > 0 && left < emptyRent {
		return emptyRent - left, nil
	}
	return 0, nil
}

func (r *Revalidator) loadFreshRoute(ctx context.Context, lease RevalidationLease) (freshSameMint, error) {
	var f freshSameMint
	_, preliminary, err := ReadAccounts(ctx, r.rpc, []string{lease.SourceReserve, lease.TargetReserve}, rpc.CommitmentConfirmed, 0)
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
	slot, accounts, err := ReadAccounts(ctx, r.rpc, addresses, rpc.CommitmentConfirmed, 0)
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
	sourceObligation, err := decodeObligation(accounts[2], freshSource.Position.Market, lease.VaultPubkey, &freshSource.Position)
	if err != nil {
		return f, err
	}
	sourceCollateral := sourceObligation.Collateral(accounts[0].Key)
	if sourceCollateral == 0 {
		return f, errors.New("source obligation no longer contains the planned reserve")
	}
	if lease.SourceCollateralRaw > 0 && sourceCollateral != lease.SourceCollateralRaw {
		return f, errors.New("fresh source collateral amount differs from opportunity")
	}
	targetMissing := accounts[3] == nil
	var targetCollateral uint64
	if !targetMissing {
		targetObligation, err := decodeObligation(accounts[3], freshTarget.Position.Market, lease.VaultPubkey, &freshTarget.Position)
		if err != nil {
			return f, err
		}
		targetCollateral = targetObligation.Collateral(accounts[1].Key)
	}
	if freshSource.Position.LiquidityTokenProgram != freshTarget.Position.LiquidityTokenProgram || accounts[4] == nil || accounts[4].Owner.String() != freshSource.Position.LiquidityTokenProgram {
		return f, errors.New("same-mint reserve token programs differ from vault custody")
	}
	idle, err := validateVaultTokenAccount(accounts[4], lease.LiquidityMint, lease.VaultPubkey)
	if err != nil {
		return f, err
	}
	if accounts[5] == nil || accounts[5].Owner != squads.ProgramID {
		return f, errors.New("fresh policy account owner mismatch")
	}
	f.routePolicy = accounts[5]
	if lease.SetupPolicyAccount != "" && accounts[7] != nil {
		f.setupPolicy = accounts[7]
	}
	missingFarmUser := []bool{false, false}
	for i, position := range []decodedRoutePosition{freshSource, freshTarget} {
		if position.FarmUser != "" {
			missingFarmUser[i] = accounts[farmUsers] == nil
			farmUsers++
		}
	}
	slotsPerSecond, err := r.rpc.SlotsPerSecond(ctx)
	if err != nil {
		return f, fmt.Errorf("measured slot rate: %w", err)
	}
	sourceEconomics, err := DecodeKaminoReserve(accounts[0], ReserveIdentity{Address: lease.SourceReserve, Market: freshSource.Position.Market, Mint: lease.LiquidityMint}, slot, slotsPerSecond)
	if err != nil {
		return f, fmt.Errorf("decode fresh source economics: %w", err)
	}
	targetEconomics, err := DecodeKaminoReserve(accounts[1], ReserveIdentity{Address: lease.TargetReserve, Market: freshTarget.Position.Market, Mint: lease.LiquidityMint}, slot, slotsPerSecond)
	if err != nil {
		return f, fmt.Errorf("decode fresh target economics: %w", err)
	}
	// KLend redeems at least this exact floor, and its in-transaction refresh
	// only accrues, so the deposit never consumes pre-existing idle custody.
	redeemable, err := redeemableLiquidity(accounts[0], freshSource.Position.Market, lease.LiquidityMint, sourceCollateral)
	if err != nil {
		return f, fmt.Errorf("fresh collateral backing: %w", err)
	}
	if lease.LiquidityAmountRaw == 0 || lease.LiquidityAmountRaw > redeemable || lease.PrincipalUSDMicros <= 0 || uint64(lease.PrincipalUSDMicros) != lease.LiquidityAmountRaw {
		return f, errors.New("planned same-mint deposit exceeds fresh collateral backing or stable principal differs")
	}
	// The whole redeemed holding must fit both reserves as the chain stands
	// now, before anything is built or simulated: KLend pays withdrawals only
	// from available liquidity and refuses deposits past the deposit limit.
	targetRoom, roomKnown := targetEconomics.DepositRoomRaw()
	sourceAvailable := sourceEconomics.AvailableLiquidityUSDMicros()
	if sourceAvailable == nil || !roomKnown || redeemable > math.MaxInt64 {
		return f, errors.New("fresh reserve capacity is unreadable")
	}
	if err := reserveCapacityShortfall(int64(redeemable), *sourceAvailable, 0, targetRoom, 0); err != nil {
		return f, err
	}
	var topUp uint64
	if targetMissing {
		rent, err := r.rpc.RentExempt(ctx, kamino.ObligationSize)
		if err != nil {
			return f, err
		}
		emptyRent, err := r.rpc.RentExempt(ctx, 0)
		if err != nil {
			return f, err
		}
		vaultLamports := uint64(0)
		if accounts[6] != nil {
			vaultLamports = accounts[6].Lamports
		}
		// KLend's withdraw closes the obligation it leaves with no active
		// deposit and no borrow and pays its lamports to the owner, the vault,
		// before init_obligation runs in this transaction.
		if deposits := sourceObligation.DepositReserves(); len(deposits) == 1 && deposits[0] == accounts[0].Key && len(sourceObligation.BorrowReserves()) == 0 {
			vaultLamports += accounts[2].Lamports
		}
		if topUp, err = vaultRentTopUp(rent, emptyRent, vaultLamports); err != nil {
			return f, err
		}
	}
	f.evidence = FreshRouteEvidence{ObservedAt: time.Now().UTC(), Slot: slot, ObservedSourceAPYBPS: sourceEconomics.SupplyAPYBPS, ObservedTargetAPYBPS: targetEconomics.SupplyAPYBPS, TargetObservedSupplyUSDMicros: targetEconomics.SupplyUSDMicros(), SourceAvailableLiquidityRaw: *sourceEconomics.AvailableLiquidityRaw, TargetDepositRoomRaw: targetRoom, OpportunityID: lease.OpportunityID, OpportunityKey: lease.IdempotencyKey, EpochID: lease.OptimizerEpochID, EpochFingerprint: lease.OptimizerEpochKey}
	f.evidence.Anchors = ExecutionBalanceAnchors{SourceObligation: source.Obligation, TargetObligation: target.Obligation, VaultLiquidityATA: source.Position.VaultLiquidityATA, SourceReserve: lease.SourceReserve, TargetReserve: lease.TargetReserve, SourceMarket: source.Position.Market, TargetMarket: target.Position.Market, SourceCollateralMint: source.Position.CollateralMint, TargetCollateralMint: target.Position.CollateralMint, LiquidityTokenProgram: source.Position.LiquidityTokenProgram, Owner: lease.VaultPubkey, Mint: lease.LiquidityMint, SourceCollateralRaw: sourceCollateral, TargetCollateralRaw: targetCollateral, IdleLiquidityRaw: idle.Amount, MinimumSlot: slot}
	f.input = KaminoSameMintRouteRequest{Vault: lease.VaultPubkey, Source: freshSource.Position, Target: freshTarget.Position, WithdrawCollateralAmount: sourceCollateral, DepositLiquidityAmount: redeemable,
		TargetObligationMissing: targetMissing, SourceFarmUserMissing: missingFarmUser[0], TargetFarmUserMissing: missingFarmUser[1], Payer: r.signer, VaultRentTopUpLamports: topUp}
	return f, nil
}

func decodeRouteReserve(account *chain.Account, vault string) (decodedRoutePosition, error) {
	position, err := DecodeReserveIdentity(account)
	if err != nil {
		return decodedRoutePosition{}, err
	}
	if position, err = DeriveVaultReserveAccounts(position, vault); err != nil {
		return decodedRoutePosition{}, err
	}
	return decodedRoutePosition{Position: position, Obligation: position.Obligation, FarmUser: position.ObligationFarmUserState}, nil
}

func DecodeReserveIdentity(account *chain.Account) (KaminoPositionAccounts, error) {
	reserve, err := kamino.DecodeReserve(account)
	if err != nil {
		return KaminoPositionAccounts{}, err
	}
	optional := func(key solana.PublicKey) string {
		if key.IsZero() {
			return ""
		}
		return key.String()
	}
	return KaminoPositionAccounts{Reserve: account.Key.String(), Market: reserve.LendingMarket.String(), LiquidityMint: reserve.LiquidityMint.String(),
		CollateralMint: reserve.CollateralMint.String(), LiquiditySupply: reserve.LiquiditySupply.String(), CollateralSupply: reserve.CollateralSupply.String(),
		LiquidityTokenProgram: reserve.LiquidityTokenProgram.String(), PythOracle: optional(reserve.PythPrice), SwitchboardPriceOracle: optional(reserve.SwitchboardPriceAggregator),
		SwitchboardTWAPOracle: optional(reserve.SwitchboardTWAPAggregator), ScopePrices: optional(reserve.ScopePriceFeed), ReserveFarmState: optional(reserve.FarmCollateral)}, nil
}

func DeriveVaultReserveAccounts(position KaminoPositionAccounts, vault string) (KaminoPositionAccounts, error) {
	marketKey, err := solana.PublicKeyFromBase58(position.Market)
	if err != nil {
		return KaminoPositionAccounts{}, err
	}
	vaultKey, err := solana.PublicKeyFromBase58(vault)
	if err != nil {
		return KaminoPositionAccounts{}, err
	}
	marketAuthority, err := kamino.LendingMarketAuthority(marketKey)
	if err != nil {
		return KaminoPositionAccounts{}, err
	}
	obligation, err := kamino.VanillaObligation(vaultKey, marketKey)
	if err != nil {
		return KaminoPositionAccounts{}, err
	}
	mint, _ := solana.PublicKeyFromBase58(position.LiquidityMint)
	tokenProgram, _ := solana.PublicKeyFromBase58(position.LiquidityTokenProgram)
	ata, err := spl.AssociatedTokenAddress(vaultKey, mint, tokenProgram)
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
		user, err := kamino.ObligationFarmUserState(farmKey, obligation)
		if err != nil {
			return KaminoPositionAccounts{}, err
		}
		position.ObligationFarmUserState = user.String()
	}
	return position, nil
}

func decodeObligation(account *chain.Account, expectedMarket, expectedOwner string, position *KaminoPositionAccounts) (kamino.Obligation, error) {
	obligation, err := kamino.DecodeObligation(account)
	if err != nil {
		return obligation, err
	}
	if obligation.LendingMarket.String() != expectedMarket || obligation.Owner.String() != expectedOwner {
		return obligation, fmt.Errorf("obligation %s market or owner mismatch", account.Key)
	}
	for _, reserve := range obligation.DepositReserves() {
		position.ObligationDepositReserves = append(position.ObligationDepositReserves, reserve.String())
	}
	for _, reserve := range obligation.BorrowReserves() {
		position.ObligationBorrowReserves = append(position.ObligationBorrowReserves, reserve.String())
	}
	return obligation, nil
}

func redeemableLiquidity(account *chain.Account, market, mint string, collateral uint64) (uint64, error) {
	reserve, err := kamino.DecodeReserve(account)
	if err != nil {
		return 0, err
	}
	if reserve.LendingMarket.String() != market || reserve.LiquidityMint.String() != mint {
		return 0, errors.New("Kamino reserve identity drifted")
	}
	return reserve.CollateralToLiquidity(collateral)
}

func validateVaultTokenAccount(account *chain.Account, expectedMint, expectedOwner string) (spl.TokenAccount, error) {
	if account == nil || account.Lamports == 0 {
		return spl.TokenAccount{}, errors.New("vault token account is not funded token custody")
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
		commitment := rpc.CommitmentConfirmed
		if finalized {
			commitment = rpc.CommitmentFinalized
		}
		observedSlot, accounts, err := ReadAccounts(ctx, r.rpc, addresses, commitment, minimumSlot)
		if err != nil {
			return nil, err
		}
		for offset, account := range accounts {
			table := tables[start+offset]
			if account == nil || account.Executable || account.Lamports == 0 || account.Owner.String() != altProgram || len(account.Data) < 56 || (len(account.Data)-56)%32 != 0 || binary.LittleEndian.Uint32(account.Data[:4]) != 1 || binary.LittleEndian.Uint64(account.Data[4:12]) != ^uint64(0) {
				return nil, fmt.Errorf("lookup table %s has invalid or deactivated chain data", table.Address)
			}
			if observedSlot <= 0 || binary.LittleEndian.Uint64(account.Data[12:20]) >= uint64(observedSlot) {
				return nil, fmt.Errorf("lookup table %s is not warm at observed slot", table.Address)
			}
			members := make([]string, 0, (len(account.Data)-56)/32)
			for offset := 56; offset < len(account.Data); offset += 32 {
				members = append(members, encodeBase58(account.Data[offset:offset+32]))
			}
			if len(members) != len(table.Addresses) {
				return nil, fmt.Errorf("lookup table %s database/chain length mismatch", table.Address)
			}
			for j := range members {
				if members[j] != table.Addresses[j] {
					return nil, fmt.Errorf("lookup table %s database/chain member mismatch", table.Address)
				}
			}
		}
	}
	return tables, nil
}
