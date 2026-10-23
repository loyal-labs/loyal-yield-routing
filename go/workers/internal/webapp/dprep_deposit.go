package webapp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/earnprep"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/spl"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
)

const (
	dprepDepositOperation          = "earnUsdcDeposit"
	dprepDepositDecimals           = 6
	dprepSetupRentBufferLamports   = 39_532_800
	dprepLamportsPerSignature      = 5_000
	dprepRentOverheadBytes         = 128
	dprepMainnetLamportsPerByte    = 6_960
	dprepTokenAccountSpace         = 165
	dprepInsufficientWalletBalance = "Main wallet does not have enough of the selected stablecoin for this Earn deposit."
)

var dprepSameMintConstraintIndexes = []int{0, 1}

var dprepKaminoSetupDiscriminators = [][8]byte{
	kamino.InitUserMetadataDiscriminator,
	kamino.InitObligationDiscriminator,
	kamino.InitObligationFarmsForReserveDiscriminator,
}

type dprepTarget struct {
	reserve, market, liquidityMint, tokenProgram solana.PublicKey
	supplyApyBps                                 *int64
}

func dprepTargetOf(target dtargetEarnReserveTarget) (dprepTarget, error) {
	var out dprepTarget
	for _, field := range []struct {
		text string
		key  *solana.PublicKey
	}{{target.Reserve, &out.reserve}, {target.Market, &out.market}, {target.LiquidityMint, &out.liquidityMint}, {target.LiquidityTokenProgram, &out.tokenProgram}} {
		key, err := solana.PublicKeyFromBase58(field.text)
		if err != nil {
			return dprepTarget{}, err
		}
		*field.key = key
	}
	out.supplyApyBps = target.SupplyApyBps
	return out, nil
}

func (h dprepRoutes) prepareDeposit(ctx context.Context, account EarnAccount, amount uint64, product dtargetProductMint) (*dprepPreparedDeposit, error) {
	if account.Cluster != LoyalMainnetBeta {
		return nil, errors.New("Earn deposit prepare is served by Go on mainnet-beta only.")
	}
	pair, err := h.activePolicyPair(ctx, account)
	if err != nil {
		return nil, err
	}
	position, err := h.reconciledActiveYieldPosition(ctx, account)
	if err != nil {
		return nil, err
	}
	if position != nil && position.InitialLiquidityMint != product.Mint {
		position = nil
	}
	known, hasPolicy, err := h.knownRoute(ctx, account, pair)
	if err != nil {
		return nil, err
	}

	var target *dtargetEarnReserveTarget
	if hasPolicy && position != nil {
		target, err = dtargetResolveEligibleEarnDepositTarget(ctx, h.reserves, h.blockTime, account.Cluster, product.Mint, "earn-deposit-prepare",
			dtargetActivePosition{CurrentLiquidityMint: position.CurrentLiquidityMint, CurrentMarket: position.CurrentMarket, CurrentReserve: position.CurrentReserve})
		if err != nil {
			return nil, err
		}
	}
	if target == nil {
		target, err = dtargetFindBestSafeEarnReserveTargetWithRetry(ctx, h.reserves, h.blockTime, account.Cluster, product)
		if err != nil {
			return nil, err
		}
	}
	if target == nil {
		return nil, earnError(http.StatusConflict, "no_eligible_reserve", "No eligible Safe Kamino reserve is available for this Earn mint.")
	}
	resolved, err := dprepTargetOf(*target)
	if err != nil {
		return nil, err
	}
	return h.prepareEarnDeposit(ctx, account, amount, resolved, known)
}

func (h dprepRoutes) knownRoute(ctx context.Context, account EarnAccount, pair *RoutePolicyPair) (*dprepKnownRoute, bool, error) {
	if pair != nil {
		route, err := dprepKnownRouteOf(pair.Route.PolicyAccount, pair.Route.PolicySeed)
		return route, true, err
	}
	attempt, err := h.dprepOnboardingAttempt(ctx, account)
	if err != nil || attempt == nil || attempt.routeSignature == nil || attempt.routeConfirmedSlot == nil {
		return nil, false, err
	}
	if attempt.setupSignature == nil || attempt.setupAccount == nil || attempt.setupSeed == nil {
		return nil, true, nil
	}
	route, err := dprepKnownRouteOf(attempt.policyAccount, attempt.policySeed)
	return route, true, err
}

func dprepKnownRouteOf(account string, seed int64) (*dprepKnownRoute, error) {
	key, err := solana.PublicKeyFromBase58(account)
	if err != nil {
		return nil, err
	}
	if seed < 0 {
		return nil, fmt.Errorf("route policy seed %d is negative", seed)
	}
	return &dprepKnownRoute{account: key, seed: uint64(seed)}, nil
}

func (h dprepRoutes) accountAt(ctx context.Context, key solana.PublicKey) (*chain.Account, error) {
	_, read, err := h.Chain.Accounts(ctx, []solana.PublicKey{key}, rpc.CommitmentConfirmed, 0)
	if err != nil {
		return nil, err
	}
	return read[0], nil
}

func (h dprepRoutes) lamportsAt(ctx context.Context, key solana.PublicKey) (uint64, error) {
	account, err := h.accountAt(ctx, key)
	if err != nil || account == nil {
		return 0, err
	}
	return account.Lamports, nil
}

func (h dprepRoutes) assertWalletCovers(ctx context.Context, walletAta solana.PublicKey, amount uint64) error {
	account, err := h.accountAt(ctx, walletAta)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "could not find account") {
			return errors.New(dprepInsufficientWalletBalance)
		}
		return err
	}
	if account == nil {
		return errors.New(dprepInsufficientWalletBalance)
	}
	token, err := spl.DecodeTokenAccount(account)
	if err != nil {
		return err
	}
	if token.Amount < amount {
		return errors.New(dprepInsufficientWalletBalance)
	}
	return nil
}

func (h dprepRoutes) prepareEarnDeposit(ctx context.Context, account EarnAccount, amount uint64, target dprepTarget, known *dprepKnownRoute) (*dprepPreparedDeposit, error) {
	wallet, vault := account.Wallet, account.Vault
	obligation, err := kamino.VanillaObligation(vault, target.market)
	if err != nil {
		return nil, err
	}
	vaultAta, err := spl.AssociatedTokenAddress(vault, target.liquidityMint, target.tokenProgram)
	if err != nil {
		return nil, err
	}
	walletAta, err := spl.AssociatedTokenAddress(wallet, target.liquidityMint, target.tokenProgram)
	if err != nil {
		return nil, err
	}

	var (
		group                 sync.WaitGroup
		balanceErr, policyErr error
		reserveErr            error
		policy                dprepResolvedPolicy
		reserve               kamino.Reserve
	)
	group.Add(3)
	go func() { defer group.Done(); balanceErr = h.assertWalletCovers(ctx, walletAta, amount) }()
	go func() {
		defer group.Done()
		policy, policyErr = h.resolvePolicy(ctx, account, target.tokenProgram, known)
	}()
	go func() {
		defer group.Done()
		read, err := h.accountAt(ctx, target.reserve)
		if err != nil {
			reserveErr = err
			return
		}
		reserve, reserveErr = earnprep.ValidateKaminoEarnReserveAccount(read, earnprep.KaminoEarnReserveExpectation{
			LendProgramID: depositKLendProgramID, Market: target.market, LiquidityMint: target.liquidityMint, LiquidityTokenProgram: target.tokenProgram,
		})
	}()
	group.Wait()
	for _, err := range []error{balanceErr, policyErr, reserveErr} {
		if err != nil {
			return nil, err
		}
	}

	bundle, err := earnprep.KaminoDepositInstructions(ctx, h.HTTP, earnprep.KaminoDepositInstructionsURL, earnprep.KaminoDepositInstructionArgs{
		AmountRaw: amount, DepositDiscriminator: kamino.DepositV2Discriminator, LendProgramID: depositKLendProgramID,
		Market: target.market, Reserve: target.reserve, Wallet: vault,
	})
	if err != nil {
		return nil, err
	}
	instructions, deposit, err := h.kaminoDepositInstructions(ctx, bundle, target, reserve, obligation, vault, vaultAta, amount)
	if err != nil {
		return nil, err
	}

	collateral := dprepInferCollateral(deposit, vault, vaultAta)
	setupCount := 0
	for _, ix := range instructions {
		if dprepStartsWithAny(ix.Data, dprepKaminoSetupDiscriminators) {
			setupCount++
		}
	}
	var topUp uint64
	if setupCount > 0 {
		vaultLamports, err := h.lamportsAt(ctx, vault)
		if err != nil {
			return nil, err
		}
		if vaultLamports < dprepSetupRentBufferLamports {
			topUp = dprepSetupRentBufferLamports - vaultLamports
		}
	}
	var topUpIx *squads.Instruction
	if topUp > 0 {
		ix, err := squads.InstructionOf(spl.SystemTransfer(wallet, vault, topUp))
		if err != nil {
			return nil, err
		}
		topUpIx = &ix
	}

	finalize := policy.finalize
	if finalize != nil && topUpIx != nil {
		extended := *finalize
		extended.Instructions = append(append([]DepositWireInstruction{}, finalize.Instructions...), dprepWireInstruction(*topUpIx))
		finalize = &extended
	}

	sync, err := earnprep.ExecuteMemberSync(earnprep.MemberSync{
		Settings: account.Settings, Vault: vault, AccountIndex: earnVaultIndex, Members: []solana.PublicKey{wallet}, Inner: instructions,
	})
	if err != nil {
		return nil, err
	}
	var prepared []squads.Instruction
	ata, err := squads.InstructionOf(spl.CreateIdempotentATA(wallet, vault, target.liquidityMint, target.tokenProgram))
	if err != nil {
		return nil, err
	}
	prepared = append(prepared, ata)
	if collateral != nil {
		collateralIx, err := squads.InstructionOf(spl.CreateIdempotentATA(wallet, vault, collateral.mint, solana.TokenProgramID))
		if err != nil {
			return nil, err
		}
		prepared = append(prepared, collateralIx)
	}
	if topUpIx != nil && finalize == nil {
		prepared = append(prepared, *topUpIx)
	}
	transfer, err := squads.InstructionOf(spl.TransferChecked(target.tokenProgram, walletAta, target.liquidityMint, vaultAta, wallet, amount, dprepDepositDecimals))
	if err != nil {
		return nil, err
	}
	prepared = append(prepared, transfer, sync)

	depositOp := DepositWirePreparedOperation{
		LookupTableAccounts:  []DepositWireLookupTableAccount{},
		Operation:            dprepDepositOperation,
		Payer:                wallet.String(),
		ProgramID:            squads.ProgramID.String(),
		RequiresConfirmation: true,
	}
	for _, ix := range prepared {
		depositOp.Instructions = append(depositOp.Instructions, dprepWireInstruction(ix))
	}
	if length, err := dprepPacketLength(wallet, prepared); err != nil || depositPacketOverLimit(length) {
		return nil, errors.New("Earn deposit transaction is too large to fit in a Solana packet. Split Kamino setup from the deposit and try again.")
	}
	var stages []DepositWirePreparedOperation
	if policy.operation != nil {
		stages = append(stages, *policy.operation)
	}
	if finalize != nil {
		stages = append(stages, *finalize)
	}
	for _, stage := range stages {
		ixs, err := dprepInstructionsOf(stage)
		if err != nil {
			return nil, err
		}
		if length, err := dprepPacketLength(wallet, ixs); err != nil || depositPacketOverLimit(length) {
			return nil, errors.New("Earn policy setup transaction is too large to fit in a Solana packet.")
		}
	}

	rent := append([]dprepRentCandidate{}, policy.rent...)
	rent = append(rent, dprepRentCandidate{account: vaultAta, kind: "token_account_rent", label: "Earn vault token account rent", space: dprepTokenAccountSpace, stage: "deposit"})
	if collateral != nil {
		rent = append(rent, dprepRentCandidate{account: collateral.ata, kind: "token_account_rent", label: "Earn vault collateral token account rent", space: dprepTokenAccountSpace, stage: "deposit"})
	}
	var fixed []dprepNativeSolItem
	if topUp > 0 {
		vaultText := vault.String()
		fixed = append(fixed, dprepNativeSolItem{Account: &vaultText, Kind: "kamino_setup_top_up", Label: "Kamino setup account rent top-up", Lamports: strconv.FormatUint(topUp, 10), Stage: "deposit"})
	}
	nativeSol, err := h.nativeSolRequirement(ctx, account.Cluster, wallet, fixed, rent, append(stages, depositOp))
	if err != nil {
		return nil, err
	}

	return h.preparedDeposit(account, amount, target, obligation, vaultAta, collateral, policy, finalize, depositOp, setupCount, topUp, nativeSol)
}

func (h dprepRoutes) preparedDeposit(account EarnAccount, amount uint64, target dprepTarget, obligation, vaultAta solana.PublicKey, collateral *dprepCollateral,
	policy dprepResolvedPolicy, finalize *DepositWirePreparedOperation, depositOp DepositWirePreparedOperation, setupCount int, topUp uint64, nativeSol dprepNativeSolRequirement,
) (*dprepPreparedDeposit, error) {
	seedText := strconv.FormatUint(policy.seed, 10)
	var supplyApy *string
	if target.supplyApyBps != nil {
		text := strconv.FormatInt(*target.supplyApyBps, 10)
		supplyApy = &text
	}
	persistence := dprepPersistence{
		Cluster:              string(account.Cluster),
		WalletAddress:        account.Wallet.String(),
		DelegatedSigner:      h.PolicySigner.String(),
		Settings:             account.Settings.String(),
		VaultIndex:           earnVaultIndex,
		VaultPubkey:          account.Vault.String(),
		PolicyID:             seedText,
		PolicyAccount:        policy.account.String(),
		PolicySeed:           seedText,
		TargetReserve:        target.reserve.String(),
		Market:               target.market.String(),
		LiquidityMint:        target.liquidityMint.String(),
		DepositMint:          target.liquidityMint.String(),
		PrincipalAmountRaw:   strconv.FormatUint(amount, 10),
		PolicyInitialization: "reuse",
		TargetSupplyApyBps:   supplyApy,
	}
	if policy.operation != nil {
		persistence.PolicyInitialization = "create"
	}
	if plan := policy.persistence; plan != nil {
		swapLanes, threshold := plan.SwapLanes, plan.Threshold
		persistence.RiskProfile, persistence.UniversePreset, persistence.RouteModes = plan.RiskProfile, plan.UniversePreset, plan.RouteModes
		persistence.StableMints, persistence.KaminoMarkets, persistence.KaminoLiquidityMints = plan.StableMints, plan.KaminoMarkets, plan.KaminoLiquidityMints
		persistence.SwapLanes, persistence.Threshold = &swapLanes, &threshold
	} else {
		universe, err := depositEarnUniverseFor(account.Cluster)
		if err != nil {
			return nil, err
		}
		persistence.RiskProfile, persistence.UniversePreset, persistence.RouteModes = depositEarnRiskProfile, depositEarnUniversePreset, []string{"same_mint_kamino"}
		persistence.StableMints, persistence.KaminoMarkets = depositKeyStrings(universe.StableMints), depositKeyStrings(universe.Markets)
		persistence.KaminoLiquidityMints = depositKeyStrings(universe.StableMints)
	}
	var setupRef *EarnSetupPolicyRef
	if policy.setupAccount != nil {
		setupSeedText, setupAccountText := strconv.FormatUint(policy.setupSeed, 10), policy.setupAccount.String()
		persistence.SetupPolicyID, persistence.SetupPolicyAccount, persistence.SetupPolicySeed = &setupSeedText, &setupAccountText, &setupSeedText
		setupRef = &EarnSetupPolicyRef{Account: setupAccountText, ID: setupSeedText, InitObligationInstructionConstraintIndex: 0, Seed: setupSeedText}
	}
	var collateralAta *string
	if collateral != nil {
		text := collateral.ata.String()
		collateralAta = &text
	}
	return &dprepPreparedDeposit{
		KaminoSetupAccountCount: setupCount,
		KaminoSetupRentLamports: strconv.FormatUint(topUp, 10),
		KaminoSetupRequired:     setupCount > 0,
		NativeSolRequirement:    nativeSol,
		Persistence:             persistence,
		PolicyFinalizePrepared:  finalize,
		Policy:                  dprepPolicyRef{Account: policy.account.String(), ID: seedText, SameMintInstructionConstraintIndexes: dprepSameMintConstraintIndexes, Seed: seedText},
		SetupPolicy:             setupRef,
		PolicySetupPrepared:     policy.operation,
		Prepared:                depositOp,
		TargetReserve: dprepTargetReserve{
			LiquidityMint: target.liquidityMint.String(), LiquidityTokenProgram: target.tokenProgram.String(), Market: target.market.String(),
			Obligation: obligation.String(), Reserve: target.reserve.String(), SupplyApyBps: supplyApy,
		},
		Vault: dprepVault{AccountIndex: earnVaultIndex, CollateralAta: collateralAta, Pubkey: account.Vault.String(), UsdcAta: vaultAta.String()},
	}, nil
}

func (h dprepRoutes) kaminoDepositInstructions(ctx context.Context, bundle earnprep.KaminoInstructionBundle, target dprepTarget, reserve kamino.Reserve,
	obligation, vault, vaultAta solana.PublicKey, amount uint64,
) ([]squads.Instruction, squads.Instruction, error) {
	instructions, deposit := bundle.Instructions, bundle.Instruction
	accounts := deposit.Accounts
	if len(accounts) > 4 && accounts[2].PublicKey == target.market && accounts[4].PublicKey != target.reserve {
		local, err := dprepLocalDeposit(target, reserve, obligation, vault, vaultAta, amount)
		if err != nil {
			return nil, squads.Instruction{}, err
		}
		var kept []squads.Instruction
		for _, ix := range instructions {
			if !bytes.HasPrefix(ix.Data, kamino.DepositV2Discriminator[:]) {
				kept = append(kept, ix)
			}
		}
		instructions, deposit = append(kept, local), local
	}
	if reserve.FarmCollateral.IsZero() {
		return instructions, deposit, nil
	}
	farmUser, err := kamino.ObligationFarmUserState(reserve.FarmCollateral, obligation)
	if err != nil {
		return nil, squads.Instruction{}, err
	}
	existing, err := h.accountAt(ctx, farmUser)
	if err != nil {
		return nil, squads.Instruction{}, err
	}
	if existing != nil || dprepAnyStartsWith(instructions, kamino.InitObligationFarmsForReserveDiscriminator) {
		return instructions, deposit, nil
	}
	authority, err := kamino.LendingMarketAuthority(target.market)
	if err != nil {
		return nil, squads.Instruction{}, err
	}
	farmInit, err := squads.InstructionOf(kamino.InitObligationFarmsForReserve(kamino.ObligationFarmsInitAccounts{
		Payer: vault, Owner: vault, Obligation: obligation, LendingMarketAuthority: authority, Reserve: target.reserve,
		ReserveFarmState: reserve.FarmCollateral, ObligationFarmUserState: farmUser, LendingMarket: target.market,
	}, kamino.FarmCollateral))
	if err != nil {
		return nil, squads.Instruction{}, err
	}
	return dprepInsertSetup(instructions, farmInit), deposit, nil
}

func dprepAnyStartsWith(instructions []squads.Instruction, discriminator [8]byte) bool {
	for _, ix := range instructions {
		if bytes.HasPrefix(ix.Data, discriminator[:]) {
			return true
		}
	}
	return false
}

func dprepStartsWithAny(data []byte, discriminators [][8]byte) bool {
	for _, discriminator := range discriminators {
		if bytes.HasPrefix(data, discriminator[:]) {
			return true
		}
	}
	return false
}

func dprepInsertSetup(instructions []squads.Instruction, setup squads.Instruction) []squads.Instruction {
	at := -1
	for i, ix := range instructions {
		if ix.ProgramID == setup.ProgramID && bytes.HasPrefix(ix.Data, kamino.RefreshObligationDiscriminator[:]) {
			at = i
			break
		}
	}
	if at < 0 {
		for i, ix := range instructions {
			if ix.ProgramID == setup.ProgramID && !dprepStartsWithAny(ix.Data, dprepKaminoSetupDiscriminators) {
				at = i
				break
			}
		}
	}
	if at < 0 {
		return append(append([]squads.Instruction{}, instructions...), setup)
	}
	out := append([]squads.Instruction{}, instructions[:at]...)
	out = append(out, setup)
	return append(out, instructions[at:]...)
}

func dprepLocalDeposit(target dprepTarget, reserve kamino.Reserve, obligation, vault, vaultAta solana.PublicKey, amount uint64) (squads.Instruction, error) {
	authority, err := kamino.LendingMarketAuthority(target.market)
	if err != nil {
		return squads.Instruction{}, err
	}
	accounts := kamino.CollateralAccounts{
		Owner: vault, Obligation: obligation, LendingMarket: target.market, LendingMarketAuthority: authority, Reserve: target.reserve,
		LiquidityMint: reserve.LiquidityMint, LiquiditySupply: reserve.LiquiditySupply, CollateralMint: reserve.CollateralMint,
		CollateralSupply: reserve.CollateralSupply, UserLiquidity: vaultAta, LiquidityTokenProgram: target.tokenProgram,
	}
	if !reserve.FarmCollateral.IsZero() {
		farmUser, err := kamino.ObligationFarmUserState(reserve.FarmCollateral, obligation)
		if err != nil {
			return squads.Instruction{}, err
		}
		accounts.ObligationFarmUserState, accounts.ReserveFarmState = farmUser, reserve.FarmCollateral
	}
	return squads.InstructionOf(kamino.DepositV2(accounts, amount))
}

type dprepCollateral struct {
	mint, ata solana.PublicKey
}

func dprepInferCollateral(deposit squads.Instruction, vault, vaultAta solana.PublicKey) *dprepCollateral {
	var writable []solana.PublicKey
	for _, meta := range deposit.Accounts {
		if meta.IsWritable && !meta.IsSigner && meta.PublicKey != vault && meta.PublicKey != vaultAta {
			writable = append(writable, meta.PublicKey)
		}
	}
	for _, meta := range deposit.Accounts {
		derived, err := spl.AssociatedTokenAddress(vault, meta.PublicKey, solana.TokenProgramID)
		if err != nil {
			continue
		}
		for _, candidate := range writable {
			if candidate == derived {
				return &dprepCollateral{mint: meta.PublicKey, ata: candidate}
			}
		}
	}
	return nil
}

func dprepPacketLength(payer solana.PublicKey, instructions []squads.Instruction) (int, error) {
	tx, err := earnprep.CompileV0(payer, depositPacketBlockhash, instructions, nil)
	if err != nil {
		return 0, err
	}
	wire, err := tx.MarshalBinary()
	if err != nil {
		return 0, err
	}
	return len(wire), nil
}

func (h dprepRoutes) nativeSolRequirement(ctx context.Context, cluster LoyalCluster, payer solana.PublicKey, fixed []dprepNativeSolItem, rent []dprepRentCandidate, ops []DepositWirePreparedOperation) (dprepNativeSolRequirement, error) {
	var unknown []solana.PublicKey
	for _, candidate := range rent {
		if !candidate.known {
			unknown = append(unknown, candidate.account)
		}
	}
	exists := map[solana.PublicKey]bool{}
	if len(unknown) > 0 {
		_, read, err := h.Chain.Accounts(ctx, unknown, rpc.CommitmentConfirmed, 0)
		if err != nil {
			return dprepNativeSolRequirement{}, err
		}
		for i, account := range read {
			if account != nil {
				exists[unknown[i]] = true
			}
		}
	}
	balance, err := h.lamportsAt(ctx, payer)
	if err != nil {
		return dprepNativeSolRequirement{}, err
	}

	items := append([]dprepNativeSolItem{}, fixed...)
	for _, candidate := range rent {
		if !candidate.known && exists[candidate.account] {
			continue
		}
		lamports := (candidate.space + dprepRentOverheadBytes) * dprepMainnetLamportsPerByte
		if cluster != LoyalMainnetBeta {
			if lamports, err = h.Chain.RentExempt(ctx, candidate.space); err != nil {
				return dprepNativeSolRequirement{}, err
			}
		}
		if lamports == 0 {
			continue
		}
		accountText := candidate.account.String()
		items = append(items, dprepNativeSolItem{Account: &accountText, Kind: candidate.kind, Label: candidate.label, Lamports: strconv.FormatUint(lamports, 10), Stage: candidate.stage})
	}
	for i, op := range ops {
		signatures := uint64(1)
		if ixs, err := dprepInstructionsOf(op); err == nil {
			if tx, err := earnprep.CompileV0(payer, depositPacketBlockhash, ixs, nil); err == nil && tx.Message.Header.NumRequiredSignatures > 1 {
				signatures = uint64(tx.Message.Header.NumRequiredSignatures)
			}
		}
		stage := op.Operation
		if stage == "" {
			stage = fmt.Sprintf("transaction_%d", i+1)
		}
		items = append(items, dprepNativeSolItem{Kind: "transaction_fee", Label: "Estimated transaction fee", Lamports: strconv.FormatUint(signatures*dprepLamportsPerSignature, 10), Stage: stage})
	}

	var required uint64
	for _, item := range items {
		lamports, err := strconv.ParseUint(item.Lamports, 10, 64)
		if err != nil {
			return dprepNativeSolRequirement{}, err
		}
		required += lamports
	}
	var deficit uint64
	if required > balance {
		deficit = required - balance
	}
	return dprepNativeSolRequirement{
		BalanceLamports:  strconv.FormatUint(balance, 10),
		BalanceSource:    "queried",
		CanProceed:       deficit == 0,
		DeficitLamports:  strconv.FormatUint(deficit, 10),
		Items:            items,
		Payer:            payer.String(),
		RequiredLamports: strconv.FormatUint(required, 10),
	}, nil
}
