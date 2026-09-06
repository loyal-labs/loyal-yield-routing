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
	"reflect"
	"sort"
	"time"

	solana "github.com/gagliardetto/solana-go"
)

const (
	farmsProgram = "FarmsPZpWu9i7Kky8tPN37rs2TpmMrAZrC7S7vJa91Hr"
	altProgram   = "AddressLookupTab1e1111111111111111111111111"
)

type Revalidator struct {
	store                    *Store
	rpc                      *RPCClient
	proxy                    *KLendProxy
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
}

func NewRevalidator(store *Store, rpc *RPCClient, proxy *KLendProxy, config RevalidatorConfig) (*Revalidator, error) {
	if store == nil || rpc == nil || proxy == nil || config.Owner == "" || config.DelegatedSigner == "" || config.LeaseTTL < time.Second {
		return nil, errors.New("store, RPC, proxy, owner, signer, and lease TTL are required")
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
	return &Revalidator{store: store, rpc: rpc, proxy: proxy, owner: config.Owner, signer: config.DelegatedSigner, leaseTTL: config.LeaseTTL, computeLimit: config.ComputeLimit, slotDuration: config.SlotDuration, fusedExecute: config.FusedExecute, crossMintEnabled: config.CrossMintEnabled, crossMintMaxValueLossBPS: config.CrossMintMaxValueLossBPS, crossMintMaxSlippageBPS: config.CrossMintMaxSlippageBPS, jupiter: jupiter}, nil
}

// Cycle claims at most one row. Claim, fresh-chain preparation, and commit are
// deliberately separate transactions; CommitRevalidation rechecks every
// mutable identity, lease, epoch, conflict, and capacity fence atomically.
func (r *Revalidator) Cycle(ctx context.Context, cluster string) (bool, error) {
	lease, err := r.store.ClaimRevalidation(ctx, cluster, r.owner, r.leaseTTL, r.fusedExecute, r.crossMintEnabled, r.signer)
	if err != nil || lease == nil {
		return false, err
	}
	if !contains(lease.DelegatedSigners, r.signer) {
		return true, errors.New("claimed policy no longer delegates to configured signer")
	}
	if lease.RouteKind == "cross_mint_jupiter" {
		return true, r.cycleCrossMint(ctx, *lease)
	}
	input, evidence, err := r.loadFreshRoute(ctx, *lease)
	if err != nil {
		return true, err
	}
	if r.fusedExecute {
		if err := r.store.RefreshTargetCapacity(ctx, cluster, lease.TargetReserve, lease.LiquidityMint, evidence.TargetObservedSupplyUSDMicros, evidence.Slot); err != nil {
			return true, fmt.Errorf("refresh fused target capacity: %w", err)
		}
	}
	if err := r.store.CheckRevalidationLease(ctx, *lease); err != nil {
		return true, err
	}
	route, err := r.proxy.Build(ctx, input)
	if err != nil {
		return true, err
	}
	policy, err := ValidateFreshRouteEvidence(evidence, time.Now().UTC(), lease.IdempotencyKey, lease.OptimizerEpochKey, r.signer, route.Protected)
	if err != nil {
		return true, err
	}
	if policy.AccountIndex != lease.VaultIndex {
		return true, errors.New("fresh policy account index differs from managed vault index")
	}
	wrapped := make([]RouteInstruction, len(route.Protected))
	for i := range route.Protected {
		wrapped[i], err = wrapSquadsPolicy(lease.PolicyAccount, r.signer, lease.VaultIndex, []uint8{policy.AllowedIndexes[i]}, []RouteInstruction{route.Protected[i]})
		if err != nil {
			return true, err
		}
	}
	instructions, err := interleaveMatureSameMintRoute(route.Public, wrapped)
	if err != nil {
		return true, err
	}
	requiredAddresses := requiredLookupTableAddresses(instructions)
	tables, err := r.store.LoadReusableLookupTables(ctx, cluster, lease.VaultID, evidence.Slot, requiredAddresses)
	if err != nil {
		return true, err
	}
	tables, err = r.verifyLookupTables(ctx, tables, evidence.Slot)
	if err != nil {
		return true, err
	}
	blockhash, _, err := r.rpc.LatestBlockhash(ctx, evidence.Slot)
	if err != nil {
		return true, err
	}
	preview, missing, err := compileV0Transaction(r.signer, blockhash, instructions, tables, 1, r.computeLimit)
	if err != nil {
		return true, err
	}
	if len(missing) > 0 || len(preview.LookupTables) == 0 {
		preparation := waitingALTPreparation(route, missing, r.computeLimit)
		if err := preserveCanonicalPlan(lease.ExecutionPlan, &preparation, "alt_readiness"); err != nil {
			return true, err
		}
		return true, r.store.CommitRevalidation(ctx, *lease, RevalidationCommit{Disposition: "waiting_alt", Preparation: &preparation, MissingAddresses: missing, ExpectedEpochFingerprint: lease.OptimizerEpochKey, ExpectedOpportunityKey: lease.IdempotencyKey})
	}
	baselineSimulation, err := r.rpc.SimulateExactTransaction(ctx, preview.UnsignedWire, evidence.Slot)
	if err != nil {
		return true, fmt.Errorf("baseline exact simulation failed: %w", err)
	}
	if !baselineSimulation.Succeeded {
		return true, fmt.Errorf("baseline exact simulation failed: %s", baselineSimulation.Error)
	}
	compute := paddedComputeUnits(baselineSimulation.UnitsConsumed)
	if compute > r.computeLimit {
		return true, fmt.Errorf("measured compute requirement %d exceeds configured limit %d", compute, r.computeLimit)
	}
	baselineFee, err := r.rpc.FeeForMessage(ctx, preview.Message, evidence.Slot)
	if err != nil {
		return true, err
	}
	recentPriority, err := r.rpc.RecentPriorityFee(ctx, preview.WritableAccounts)
	if err != nil {
		return true, err
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
	budgeted := route
	budgeted.Public = append(computeBudgetInstructions(uint32(compute), recentPriority), route.Public...)
	budgetWrapped := make([]RouteInstruction, len(budgeted.Protected))
	for i := range budgeted.Protected {
		budgetWrapped[i], err = wrapSquadsPolicy(lease.PolicyAccount, r.signer, lease.VaultIndex, []uint8{policy.AllowedIndexes[i]}, []RouteInstruction{budgeted.Protected[i]})
		if err != nil {
			return true, err
		}
	}
	budgetInstructions, err := interleaveMatureSameMintRoute(budgeted.Public, budgetWrapped)
	if err != nil {
		return true, err
	}
	budgetPreview, missing, err := compileV0Transaction(r.signer, blockhash, budgetInstructions, tables, 1, compute)
	if err != nil {
		return true, fmt.Errorf("budgeted transaction compilation: %w", err)
	}
	if len(missing) > 0 {
		return true, fmt.Errorf("budgeted ALT compilation changed coverage: %v", missing)
	}
	fee, err := r.rpc.FeeForMessage(ctx, budgetPreview.Message, evidence.Slot)
	if err != nil {
		return true, err
	}
	if fee > uint64(lease.FeeCapLamports) {
		return true, fmt.Errorf("budgeted fee %d exceeds opportunity cap %d", fee, lease.FeeCapLamports)
	}
	preparation, err := PrepareRoute(budgeted, lease.PolicyAccount, r.signer, lease.VaultIndex, policy.AllowedIndexes, tables, blockhash, fee, compute, func(wire []byte) (SimulationEvidence, error) {
		return r.rpc.SimulateExactTransaction(ctx, wire, evidence.Slot)
	})
	if err == nil {
		preparation.RouteFingerprint = retainedSameMintRouteFingerprint(*lease)
		preparation.RequirementsFingerprint, err = retainedSameMintRequirementsFingerprint(input, lease.PolicyAccount, r.signer, instructions)
	}
	if err != nil {
		return true, err
	}
	if err := preserveCanonicalPlan(lease.ExecutionPlan, &preparation, "prepared_transaction"); err != nil {
		return true, err
	}
	disposition := "ready"
	if r.fusedExecute {
		disposition = "fused_execute"
	}
	return true, r.store.CommitRevalidation(ctx, *lease, RevalidationCommit{Disposition: disposition, Preparation: &preparation, ConflictKeys: preparation.Transaction.WritableAccounts, ExpectedEpochFingerprint: lease.OptimizerEpochKey, ExpectedOpportunityKey: lease.IdempotencyKey, FreshEconomics: true, ObservedSourceAPYBPS: evidence.ObservedSourceAPYBPS, ObservedTargetAPYBPS: evidence.ObservedTargetAPYBPS, TargetObservedSupplyUSDMicros: evidence.TargetObservedSupplyUSDMicros, TargetObservedSlot: evidence.Slot})
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

func waitingALTPreparation(route KaminoSameMintRoute, missing []string, compute uint64) RoutePreparation {
	routeBytes, _ := json.Marshal(route)
	requirements, _ := json.Marshal(struct {
		Missing []string `json:"missing"`
		Compute uint64   `json:"compute"`
	}{canonicalStrings(missing), compute})
	routeHash, requirementHash := sha256.Sum256(routeBytes), sha256.Sum256(requirements)
	plan, _ := json.Marshal(struct {
		Kind    string   `json:"kind"`
		Missing []string `json:"missing_alt_addresses"`
		Compute uint64   `json:"compute_unit_limit"`
	}{"same_mint_kamino_waiting_alt", canonicalStrings(missing), compute})
	return RoutePreparation{RouteFingerprint: hex.EncodeToString(routeHash[:]), RequirementsFingerprint: hex.EncodeToString(requirementHash[:]), ExecutionPlan: plan}
}

type decodedRoutePosition struct {
	Position   KaminoPositionAccounts
	Obligation string
	FarmUser   string
}

func (r *Revalidator) loadFreshRoute(ctx context.Context, lease RevalidationLease) (KaminoSameMintRouteRequest, FreshRouteEvidence, error) {
	minimum, err := r.rpc.ConfirmedSlot(ctx)
	if err != nil {
		return KaminoSameMintRouteRequest{}, FreshRouteEvidence{}, err
	}
	_, preliminary, err := r.rpc.ConfirmedAccounts(ctx, []string{lease.SourceReserve, lease.TargetReserve}, minimum)
	if err != nil {
		return KaminoSameMintRouteRequest{}, FreshRouteEvidence{}, err
	}
	source, err := decodeRouteReserve(preliminary[0], lease.VaultPubkey)
	if err != nil {
		return KaminoSameMintRouteRequest{}, FreshRouteEvidence{}, err
	}
	target, err := decodeRouteReserve(preliminary[1], lease.VaultPubkey)
	if err != nil {
		return KaminoSameMintRouteRequest{}, FreshRouteEvidence{}, err
	}
	if source.Position.LiquidityMint != lease.LiquidityMint || target.Position.LiquidityMint != lease.LiquidityMint {
		return KaminoSameMintRouteRequest{}, FreshRouteEvidence{}, errors.New("fresh reserve mint differs from opportunity")
	}
	addresses := []string{lease.VaultPubkey, lease.SourceReserve, lease.TargetReserve, source.Obligation, target.Obligation, source.Position.VaultLiquidityATA, lease.PolicyAccount}
	kinds := []string{"vault", "reserve", "reserve", "obligation", "obligation", "token_account", "policy"}
	for _, position := range []*decodedRoutePosition{&source, &target} {
		if position.Position.ReserveFarmState != "" {
			addresses = append(addresses, position.Position.ReserveFarmState, position.FarmUser)
			kinds = append(kinds, "farm", "farm")
		}
	}
	minimum, err = r.rpc.ConfirmedSlot(ctx)
	if err != nil {
		return KaminoSameMintRouteRequest{}, FreshRouteEvidence{}, err
	}
	slot, accounts, err := r.rpc.ConfirmedAccounts(ctx, addresses, minimum)
	if err != nil {
		return KaminoSameMintRouteRequest{}, FreshRouteEvidence{}, err
	}
	freshSource, err := decodeRouteReserve(accounts[1], lease.VaultPubkey)
	if err != nil {
		return KaminoSameMintRouteRequest{}, FreshRouteEvidence{}, err
	}
	freshTarget, err := decodeRouteReserve(accounts[2], lease.VaultPubkey)
	if err != nil {
		return KaminoSameMintRouteRequest{}, FreshRouteEvidence{}, err
	}
	if !reflect.DeepEqual(freshSource.Position, source.Position) || !reflect.DeepEqual(freshTarget.Position, target.Position) {
		return KaminoSameMintRouteRequest{}, FreshRouteEvidence{}, errors.New("reserve route identities changed during coherent observation")
	}
	sourceCollateral, err := decodeObligation(accounts[3], freshSource.Position.Market, lease.VaultPubkey, lease.SourceReserve, &freshSource.Position)
	if err != nil {
		return KaminoSameMintRouteRequest{}, FreshRouteEvidence{}, err
	}
	if lease.SourceCollateralRaw > 0 && sourceCollateral != lease.SourceCollateralRaw {
		return KaminoSameMintRouteRequest{}, FreshRouteEvidence{}, errors.New("fresh source collateral amount differs from opportunity")
	}
	if _, err := decodeObligation(accounts[4], freshTarget.Position.Market, lease.VaultPubkey, "", &freshTarget.Position); err != nil {
		return KaminoSameMintRouteRequest{}, FreshRouteEvidence{}, err
	}
	if err := validateVaultTokenAccount(accounts[5], lease.LiquidityMint, lease.VaultPubkey); err != nil {
		return KaminoSameMintRouteRequest{}, FreshRouteEvidence{}, err
	}
	if accounts[6].Address != lease.PolicyAccount || accounts[6].Owner != SquadsProgram {
		return KaminoSameMintRouteRequest{}, FreshRouteEvidence{}, errors.New("fresh policy account identity or owner mismatch")
	}
	sourceEconomics, err := DecodeKaminoReserve(accounts[1], ReserveIdentity{Address: lease.SourceReserve, Market: freshSource.Position.Market, Mint: lease.LiquidityMint}, slot, r.slotDuration)
	if err != nil {
		return KaminoSameMintRouteRequest{}, FreshRouteEvidence{}, fmt.Errorf("decode fresh source economics: %w", err)
	}
	targetEconomics, err := DecodeKaminoReserve(accounts[2], ReserveIdentity{Address: lease.TargetReserve, Market: freshTarget.Position.Market, Mint: lease.LiquidityMint}, slot, r.slotDuration)
	if err != nil {
		return KaminoSameMintRouteRequest{}, FreshRouteEvidence{}, fmt.Errorf("decode fresh target economics: %w", err)
	}
	evidence := FreshRouteEvidence{ObservedAt: time.Now().UTC(), Slot: slot, ObservedSourceAPYBPS: sourceEconomics.SupplyAPYBPS, ObservedTargetAPYBPS: targetEconomics.SupplyAPYBPS, TargetObservedSupplyUSDMicros: targetEconomics.TotalSupplyUSDMicros, OpportunityID: lease.OpportunityID, OpportunityKey: lease.IdempotencyKey, EpochID: lease.OptimizerEpochID, EpochFingerprint: lease.OptimizerEpochKey, PolicyData: append([]byte(nil), accounts[6].Data...)}
	for index, account := range accounts {
		hash := sha256.Sum256(account.Data)
		evidence.Accounts = append(evidence.Accounts, FreshAccount{Kind: kinds[index], Address: account.Address, Owner: account.Owner, DataSHA256: hex.EncodeToString(hash[:]), Slot: slot, Executable: account.Executable, Exists: true})
	}
	return KaminoSameMintRouteRequest{Vault: lease.VaultPubkey, Source: freshSource.Position, Target: freshTarget.Position, WithdrawCollateralAmount: sourceCollateral, DepositLiquidityAmount: lease.LiquidityAmountRaw}, evidence, nil
}

func decodeRouteReserve(account Account, vault string) (decodedRoutePosition, error) {
	if account.Owner != KLendProgram || len(account.Data) != reserveLength || !bytes.Equal(account.Data[:8], reserveDiscriminator[:]) {
		return decodedRoutePosition{}, fmt.Errorf("reserve %s has invalid owner or data", account.Address)
	}
	key := func(offset int) string { return encodeBase58(account.Data[offset : offset+32]) }
	market := key(32)
	farm := key(64)
	if farm == "11111111111111111111111111111111" {
		farm = ""
	}
	position := KaminoPositionAccounts{Reserve: account.Address, Market: market, LiquidityMint: key(128), CollateralMint: key(2560), LiquiditySupply: key(160), CollateralSupply: key(2600), LiquidityTokenProgram: key(408), PythOracle: key(5224), SwitchboardPriceOracle: key(5160), SwitchboardTWAPOracle: key(5192), ScopePrices: key(5112), ReserveFarmState: farm}
	for _, field := range []*string{&position.PythOracle, &position.SwitchboardPriceOracle, &position.SwitchboardTWAPOracle, &position.ScopePrices} {
		if *field == "11111111111111111111111111111111" {
			*field = ""
		}
	}
	program, _ := solana.PublicKeyFromBase58(KLendProgram)
	marketKey, _ := solana.PublicKeyFromBase58(market)
	vaultKey, err := solana.PublicKeyFromBase58(vault)
	if err != nil {
		return decodedRoutePosition{}, err
	}
	marketAuthority, _, err := solana.FindProgramAddress([][]byte{[]byte("lma"), marketKey[:]}, program)
	if err != nil {
		return decodedRoutePosition{}, err
	}
	zero := solana.PublicKey{}
	obligation, _, err := solana.FindProgramAddress([][]byte{{0}, {0}, vaultKey[:], marketKey[:], zero[:], zero[:]}, program)
	if err != nil {
		return decodedRoutePosition{}, err
	}
	mint, _ := solana.PublicKeyFromBase58(position.LiquidityMint)
	tokenProgram, _ := solana.PublicKeyFromBase58(position.LiquidityTokenProgram)
	associated, _ := solana.PublicKeyFromBase58("ATokenGPvbdGVxr1b2hvZbsiqW5xWH25efTNsLJA8knL")
	ata, _, err := solana.FindProgramAddress([][]byte{vaultKey[:], tokenProgram[:], mint[:]}, associated)
	if err != nil {
		return decodedRoutePosition{}, err
	}
	position.MarketAuthority, position.Obligation, position.VaultLiquidityATA = marketAuthority.String(), obligation.String(), ata.String()
	result := decodedRoutePosition{Position: position, Obligation: obligation.String()}
	if farm != "" {
		farmKey, _ := solana.PublicKeyFromBase58(farm)
		farmsKey, _ := solana.PublicKeyFromBase58(farmsProgram)
		user, _, e := solana.FindProgramAddress([][]byte{[]byte("user"), farmKey[:], obligation[:]}, farmsKey)
		if e != nil {
			return decodedRoutePosition{}, e
		}
		result.FarmUser, result.Position.ObligationFarmUserState = user.String(), user.String()
	}
	return result, nil
}

func decodeObligation(account Account, expectedMarket, expectedOwner, expectedDeposit string, position *KaminoPositionAccounts) (uint64, error) {
	obligationDiscriminator := [8]byte{168, 206, 141, 106, 88, 76, 172, 167}
	if account.Owner != KLendProgram || len(account.Data) != 3344 || !bytes.Equal(account.Data[:8], obligationDiscriminator[:]) {
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
	sort.Strings(position.ObligationDepositReserves)
	sort.Strings(position.ObligationBorrowReserves)
	if expectedDeposit != "" && expectedAmount == 0 {
		return 0, errors.New("source obligation no longer contains the planned reserve")
	}
	return expectedAmount, nil
}

func validateVaultTokenAccount(account Account, expectedMint, expectedOwner string) error {
	if account.Owner != "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA" && account.Owner != "TokenzQdYhDYEzV8znWVkuxHcQKoZbYGWvVGg9Lzc" {
		return errors.New("vault token account has invalid program owner")
	}
	if len(account.Data) < 165 || encodeBase58(account.Data[:32]) != expectedMint || encodeBase58(account.Data[32:64]) != expectedOwner {
		return errors.New("vault token account mint or authority mismatch")
	}
	return nil
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
		_, accounts, err := r.rpc.ConfirmedAccounts(ctx, addresses, minimumSlot)
		if err != nil {
			return nil, err
		}
		for offset, account := range accounts {
			table := tables[start+offset]
			if account.Owner != altProgram || len(account.Data) < 56 || (len(account.Data)-56)%32 != 0 || binary.LittleEndian.Uint32(account.Data[:4]) != 1 || binary.LittleEndian.Uint64(account.Data[4:12]) != ^uint64(0) {
				return nil, fmt.Errorf("lookup table %s has invalid or deactivated chain data", account.Address)
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
