package webapp

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"net/http"
	"slices"
	"strconv"

	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/earnprep"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/spl"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
)

// prepareEarnUsdcWithdraw (web/packages/smart-account-vaults/src/client.ts,
// CL:8648-10166) for the two shapes the route can reach on mainnet: an idle
// source (CL:8793-8950) and a partial reserve source (CL:8956-10052). The
// route always passes closePoliciesOnFullWithdrawal:false, so no final exit,
// cleanup, sweep, policy close or autodeposit close is ever built here.

const (
	wprepDecimals        = 6
	wprepMaxLegsPerBatch = 2
	wprepMaxAdjustments  = 2
)

// wprepSource is SelectedEarnWithdrawSource
// (earn-withdraw-input-resolution.server.ts:99-118).
type wprepSource struct {
	AmountRaw    uint64
	ID           string
	Mint         solana.PublicKey
	TokenProgram solana.PublicKey
	TokenAccount solana.PublicKey
	Market       solana.PublicKey
	Reserve      solana.PublicKey
}

func (s wprepSource) idle() bool { return s.Reserve.IsZero() }

type wprepPolicy struct {
	Account      solana.PublicKey
	Seed         int64
	SetupAccount solana.PublicKey
	SetupSeed    int64
}

func (p wprepPolicy) hasSetup() bool { return !p.SetupAccount.IsZero() && p.SetupSeed != 0 }

type wprepInput struct {
	Account      EarnAccount
	AmountRaw    uint64
	PolicySigner solana.PublicKey
	Policy       wprepPolicy
	Source       wprepSource
}

type wprepDeps struct {
	Chain     earnprep.WithdrawChain
	HTTP      *http.Client
	KaminoURL string
}

// wprepLeg is ProvisionalReserveWithdrawal (CL:9061-9077).
type wprepLeg struct {
	AmountRaw               uint64
	CollateralAta           solana.PublicKey
	CollateralMint          solana.PublicKey
	ExecutionMarket         solana.PublicKey
	ExecutionReserve        solana.PublicKey
	ExpectedRedeemedRaw     uint64
	Instruction             squads.Instruction
	Instructions            []squads.Instruction
	KaminoWithdrawAmountRaw uint64
	LookupTables            []solana.PublicKey
	Snapshot                *earnprep.WithdrawReserveSnapshot
}

var errWprepUnderfilled = errors.New("earn_withdraw_underfilled")

func (in wprepInput) persistenceBase() EarnWithdrawPersistence {
	p := EarnWithdrawPersistence{
		Cluster:         string(in.Account.Cluster),
		WalletAddress:   in.Account.Wallet.String(),
		DelegatedSigner: in.PolicySigner.String(),
		Settings:        in.Account.Settings.String(),
		VaultIndex:      earnVaultIndex,
		VaultPubkey:     in.Account.Vault.String(),
		PolicyID:        strconv.FormatInt(in.Policy.Seed, 10),
		PolicyAccount:   in.Policy.Account.String(),
		PolicySeed:      strconv.FormatInt(in.Policy.Seed, 10),
		SourceAmountRaw: strconv.FormatUint(in.Source.AmountRaw, 10),
		SourceID:        in.Source.ID,
		SourceMint:      in.Source.Mint.String(),
		SourceType:      "reserve",
	}
	if in.Policy.hasSetup() {
		seed := strconv.FormatInt(in.Policy.SetupSeed, 10)
		p.SetupPolicyID, p.SetupPolicyAccount, p.SetupPolicySeed = seed, in.Policy.SetupAccount.String(), seed
	}
	if in.Source.idle() {
		p.SourceType = "idle"
		p.SourceTokenAccount = in.Source.TokenAccount.String()
		p.SourceMetadata = EarnWithdrawSourceMetadata{Mint: in.Source.Mint.String(), TokenAccount: in.Source.TokenAccount.String()}
	} else {
		p.SourceMetadata = EarnWithdrawSourceMetadata{LiquidityMint: in.Source.Mint.String(), Market: in.Source.Market.String(), Reserve: in.Source.Reserve.String()}
	}
	return p
}

func (in wprepInput) envelope(amountRaw uint64, persistence EarnWithdrawPersistence, prepared DepositWirePreparedOperation, steps []EarnWithdrawStepWire) EarnWithdrawPrepareWire {
	seed := strconv.FormatInt(in.Policy.Seed, 10)
	wire := EarnWithdrawPrepareWire{
		AmountRaw:   strconv.FormatUint(amountRaw, 10),
		Mode:        "partial",
		Persistence: persistence,
		Policy: EarnWithdrawPolicyRef{
			Account:                              in.Policy.Account.String(),
			ID:                                   seed,
			SameMintInstructionConstraintIndexes: [2]uint8{0, 1},
			Seed:                                 seed,
		},
		Prepared:      prepared,
		WithdrawSteps: steps,
	}
	if in.Policy.hasSetup() {
		setupSeed := strconv.FormatInt(in.Policy.SetupSeed, 10)
		wire.SetupPolicy = &EarnWithdrawSetupPolicyRef{Account: in.Policy.SetupAccount.String(), ID: setupSeed, Seed: setupSeed}
	}
	return wire
}

func wprepIx(ix solana.Instruction) squads.Instruction {
	out, err := squads.InstructionOf(ix)
	if err != nil {
		panic(err)
	}
	return out
}

// wprepVaultTransfer is makeSignerWritable(createTransferCheckedInstruction(…))
// (CL:2984): the vault signs the inner transfer and must be writable.
func wprepVaultTransfer(tokenProgram, vaultAta, mint, walletAta, vault solana.PublicKey, amountRaw uint64) squads.Instruction {
	ix := wprepIx(spl.TransferChecked(tokenProgram, vaultAta, mint, walletAta, vault, amountRaw, wprepDecimals))
	for i := range ix.Accounts {
		if ix.Accounts[i].PublicKey == vault && ix.Accounts[i].IsSigner {
			ix.Accounts[i].IsWritable = true
		}
	}
	return ix
}

func (in wprepInput) memberSync(inner []squads.Instruction) (squads.Instruction, error) {
	return earnprep.ExecuteMemberSync(earnprep.MemberSync{
		Settings:     in.Account.Settings,
		Vault:        in.Account.Vault,
		AccountIndex: earnVaultIndex,
		Members:      []solana.PublicKey{in.Account.Wallet},
		Inner:        inner,
	})
}

// prepareIdle is the idle-source branch (CL:8710-8726, 8793-8950).
func (in wprepInput) prepareIdle() (EarnWithdrawPrepareWire, error) {
	source, vault, wallet := in.Source, in.Account.Vault, in.Account.Wallet
	vaultAta, err := spl.AssociatedTokenAddress(vault, source.Mint, source.TokenProgram)
	if err != nil {
		return EarnWithdrawPrepareWire{}, err
	}
	walletAta, err := spl.AssociatedTokenAddress(wallet, source.Mint, source.TokenProgram)
	if err != nil {
		return EarnWithdrawPrepareWire{}, err
	}
	if source.TokenAccount != vaultAta {
		return EarnWithdrawPrepareWire{}, errors.New("Earn idle withdrawal source token account does not match the selected vault mint account.")
	}
	if in.AmountRaw > source.AmountRaw {
		return EarnWithdrawPrepareWire{}, errors.New("Earn withdraw amount exceeds the selected idle source amount.")
	}
	execute, err := in.memberSync([]squads.Instruction{wprepVaultTransfer(source.TokenProgram, vaultAta, source.Mint, walletAta, vault, in.AmountRaw)})
	if err != nil {
		return EarnWithdrawPrepareWire{}, err
	}
	prepared := wprepWireOperation(wallet, []squads.Instruction{
		wprepIx(spl.CreateIdempotentATA(wallet, wallet, source.Mint, source.TokenProgram)),
		execute,
	}, nil)
	amount := strconv.FormatUint(in.AmountRaw, 10)
	persistence := in.persistenceBase()
	persistence.LiquidityMint = source.Mint.String()
	persistence.TargetReserve = source.TokenAccount.String()
	persistence.RequestedWithdrawAmountRaw = amount
	persistence.WithdrawnAmountRaw = amount
	persistence.Mode = "partial"
	persistence.WalletTransferAmountRaw = amount
	persistence.VaultUsdcRemainderRaw = "0"
	persistence.AutodepositClose = wprepNull
	step := persistence
	final, zero, one := true, 0, 1
	step.IsFinalStep, step.StepCount, step.StepIndex = &final, &one, &zero
	wire := in.envelope(in.AmountRaw, persistence, prepared, []EarnWithdrawStepWire{{
		AmountRaw:          amount,
		Mode:               "partial",
		Persistence:        step,
		Prepared:           prepared,
		ReserveWithdrawals: []EarnWithdrawReserveMetadata{},
		StepCount:          1,
		StepIndex:          0,
	}})
	wire.Vault = EarnWithdrawVault{AccountIndex: earnVaultIndex, Pubkey: vault.String(), UsdcAta: vaultAta.String()}
	return wire, nil
}

// prepareReserve is the mainnet partial reserve branch (CL:8956-10052): one
// plan for the source reserve, legs from Kamino's withdraw instructions,
// batches of at most two legs, each simulated to size the wallet transfer.
func (in wprepInput) prepareReserve(ctx context.Context, deps wprepDeps) (EarnWithdrawPrepareWire, error) {
	source, vault := in.Source, in.Account.Vault
	vaultAta, err := spl.AssociatedTokenAddress(vault, source.Mint, source.TokenProgram)
	if err != nil {
		return EarnWithdrawPrepareWire{}, err
	}
	obligation, err := kamino.VanillaObligation(vault, source.Market)
	if err != nil {
		return EarnWithdrawPrepareWire{}, err
	}
	legs, err := in.collectLegs(ctx, deps, vaultAta, obligation)
	if err != nil {
		return EarnWithdrawPrepareWire{}, err
	}
	batches := wprepChunk(legs)
	steps := make([]EarnWithdrawStepWire, len(batches))
	for index, batch := range batches {
		step, err := in.buildBatch(ctx, deps, batch, vaultAta, obligation)
		if err != nil {
			return EarnWithdrawPrepareWire{}, err
		}
		final := index == len(batches)-1
		count, at := len(batches), index
		step.StepCount, step.StepIndex = count, at
		step.Persistence.AutodepositClose = wprepNull
		step.Persistence.IsFinalStep, step.Persistence.StepCount, step.Persistence.StepIndex = &final, &count, &at
		steps[index] = step
	}
	first, last := steps[0], steps[len(steps)-1]
	top := in.persistenceBase()
	top.TargetReserve = first.AccountingReserve.Reserve
	top.Market = first.AccountingReserve.Market
	top.LiquidityMint = source.Mint.String()
	top.RequestedWithdrawAmountRaw = strconv.FormatUint(in.AmountRaw, 10)
	top.WithdrawnAmountRaw = last.Persistence.WithdrawnAmountRaw
	top.Mode = "partial"
	stepCount := len(steps)
	top.StepCount = &stepCount
	top.KaminoWithdrawAmountRaw = last.Persistence.KaminoWithdrawAmountRaw
	top.ReserveWithdrawals = last.Persistence.ReserveWithdrawals
	top.VaultUsdcRemainderRaw = last.Persistence.VaultUsdcRemainderRaw
	top.WalletTransferAmountRaw = last.Persistence.WalletTransferAmountRaw
	wire := in.envelope(in.AmountRaw, top, first.Prepared, steps)
	wire.TargetReserve = &EarnPolicyTargetReserve{
		LiquidityMint:         source.Mint.String(),
		LiquidityTokenProgram: source.TokenProgram.String(),
		Market:                first.AccountingReserve.Market,
		Obligation:            first.AccountingReserve.Obligation,
		Reserve:               first.AccountingReserve.Reserve,
	}
	wire.Vault = EarnWithdrawVault{AccountIndex: earnVaultIndex, CollateralAta: first.CollateralAta, Pubkey: vault.String(), UsdcAta: vaultAta.String()}
	return wire, nil
}

// collectLegs is collectReserveWithdrawalsForPlan (CL:9178-9580) on mainnet
// partial: no local amount resolution, no collateral reconcile (the target
// names no collateral mint, so localCollateralAta is null), no snapshot.
func (in wprepInput) collectLegs(ctx context.Context, deps wprepDeps, vaultAta, obligation solana.PublicKey) ([]wprepLeg, error) {
	source := in.Source
	bundle, err := earnprep.KaminoWithdrawInstructions(ctx, deps.HTTP, deps.KaminoURL, in.Account.Vault, source.Market, source.Reserve, in.AmountRaw)
	if err != nil {
		return nil, err
	}
	kaminoAmount := wprepWithdrawAmount(bundle.Instruction, in.AmountRaw)
	validated, err := in.validateWithdraw(bundle.Instruction, vaultAta)
	if err != nil {
		return nil, err
	}
	if validated.ExecutionReserve != source.Reserve {
		bundle, err = in.rebuildForReserve(ctx, deps, bundle, obligation, vaultAta, kaminoAmount)
		if err != nil {
			return nil, err
		}
		if _, err := in.validateWithdraw(bundle.Instruction, vaultAta); err != nil {
			return nil, err
		}
	}
	matching := bundle.MatchingInstructions
	if len(matching) == 0 {
		matching = []squads.Instruction{bundle.Instruction}
	}
	legs := make([]wprepLeg, 0, len(matching))
	for _, ix := range matching {
		leg, err := in.validateWithdraw(ix, vaultAta)
		if err != nil {
			return nil, err
		}
		leg.AmountRaw = in.AmountRaw
		leg.ExpectedRedeemedRaw = in.AmountRaw
		leg.Instruction = ix
		leg.Instructions = []squads.Instruction{ix}
		if len(matching) == 1 {
			leg.Instructions = bundle.Instructions
		}
		leg.KaminoWithdrawAmountRaw = wprepWithdrawAmount(ix, kaminoAmount)
		leg.LookupTables = bundle.LookupTables
		legs = append(legs, leg)
	}
	if len(legs) == 0 {
		return nil, errors.New("Kamino did not return any Earn withdraw steps.")
	}
	return legs, nil
}

// rebuildForReserve is the executionReserve != reserve branch (CL:9273-9332):
// Kamino routed the withdraw through another reserve, so the withdraw is
// rebuilt locally against the selected reserve behind Kamino's refresh prefix.
func (in wprepInput) rebuildForReserve(ctx context.Context, deps wprepDeps, bundle earnprep.KaminoWithdrawBundle, obligation, vaultAta solana.PublicKey, kaminoAmount uint64) (earnprep.KaminoWithdrawBundle, error) {
	source, vault := in.Source, in.Account.Vault
	reserve, _, err := wprepReadReserve(ctx, deps.Chain, source, "Selected Kamino reserve account was not found.")
	if err != nil {
		return earnprep.KaminoWithdrawBundle{}, err
	}
	authority, err := kamino.LendingMarketAuthority(source.Market)
	if err != nil {
		return earnprep.KaminoWithdrawBundle{}, err
	}
	accounts := kamino.CollateralAccounts{
		Owner: vault, Obligation: obligation, LendingMarket: source.Market, LendingMarketAuthority: authority,
		Reserve: source.Reserve, LiquidityMint: reserve.ReserveLiquidityMint, LiquiditySupply: reserve.ReserveLiquiditySupply,
		CollateralMint: reserve.ReserveCollateralMint, CollateralSupply: reserve.ReserveCollateralSupply,
		UserLiquidity: vaultAta, LiquidityTokenProgram: source.TokenProgram,
	}
	if !reserve.FarmCollateral.IsZero() {
		accounts.ReserveFarmState = reserve.FarmCollateral
		accounts.ObligationFarmUserState, err = kamino.ObligationFarmUserState(reserve.FarmCollateral, obligation)
		if err != nil {
			return earnprep.KaminoWithdrawBundle{}, err
		}
	}
	local := wprepIx(kamino.WithdrawV2(accounts, kaminoAmount))
	var prefix []squads.Instruction
	for _, ix := range bundle.Instructions {
		if !bytes.HasPrefix(ix.Data, kamino.WithdrawV2Discriminator[:]) {
			prefix = append(prefix, ix)
		}
	}
	bundle.Instruction = local
	bundle.Instructions = append(prefix, local)
	bundle.MatchingInstructions = []squads.Instruction{local}
	return bundle, nil
}

// wprepReadReserve is getAccountInfo + validateKaminoEarnReserveAccount
// (CL:1141-1182) against the source's market, mint and token program.
func wprepReadReserve(ctx context.Context, chain earnprep.WithdrawChain, source wprepSource, missing string) (earnprep.WithdrawReserveTokenAccounts, []byte, error) {
	_, accounts, err := chain.Accounts(ctx, []solana.PublicKey{source.Reserve}, rpc.CommitmentConfirmed, 0)
	if err != nil {
		return earnprep.WithdrawReserveTokenAccounts{}, nil, err
	}
	if accounts[0] == nil {
		return earnprep.WithdrawReserveTokenAccounts{}, nil, errors.New(missing)
	}
	if accounts[0].Owner != kamino.ProgramID {
		return earnprep.WithdrawReserveTokenAccounts{}, nil, errors.New("Selected Kamino reserve is not owned by the selected lending program.")
	}
	parsed, err := earnprep.ParseWithdrawReserveTokenAccounts(accounts[0].Data)
	if err != nil {
		return earnprep.WithdrawReserveTokenAccounts{}, nil, err
	}
	for _, check := range []struct {
		actual, expected solana.PublicKey
		label            string
	}{
		{parsed.LendingMarket, source.Market, "lending market"},
		{parsed.ReserveLiquidityMint, source.Mint, "liquidity mint"},
		{parsed.ReserveLiquidityTokenProgram, source.TokenProgram, "liquidity token program"},
	} {
		if check.actual != check.expected {
			return earnprep.WithdrawReserveTokenAccounts{}, nil, errors.New("Selected Kamino reserve has an unexpected " + check.label + ".")
		}
	}
	return parsed, accounts[0].Data, nil
}

// validateWithdraw is validateKaminoWithdrawInstruction (CL:2817-2945),
// returning the leg's execution and collateral accounts.
func (in wprepInput) validateWithdraw(ix squads.Instruction, vaultAta solana.PublicKey) (wprepLeg, error) {
	unexpected := func(label string) error {
		return errors.New("Kamino withdraw instruction has an unexpected " + label + ".")
	}
	key := func(index int, label string) (solana.PublicKey, error) {
		if index >= len(ix.Accounts) {
			return solana.PublicKey{}, errors.New("Kamino withdraw instruction is missing " + label + ".")
		}
		return ix.Accounts[index].PublicKey, nil
	}
	expect := func(index int, label string, expected solana.PublicKey) error {
		actual, err := key(index, label)
		if err != nil {
			return err
		}
		if actual != expected {
			return unexpected(label)
		}
		return nil
	}
	if !bytes.HasPrefix(ix.Data, kamino.WithdrawV2Discriminator[:]) {
		return wprepLeg{}, unexpected("withdraw discriminator")
	}
	if ix.ProgramID != kamino.ProgramID {
		return wprepLeg{}, unexpected("program")
	}
	if err := expect(0, "vault", in.Account.Vault); err != nil {
		return wprepLeg{}, err
	}
	at2, err := key(2, "market")
	if err != nil {
		return wprepLeg{}, err
	}
	current := at2 == in.Source.Market
	marketAt, reserveAt, mintAt, collateralAt, collateralMintAt, vaultAtaAt, programAt := 1, 2, 4, 7, 5, 8, 10
	if current {
		marketAt, reserveAt, mintAt, collateralAt, collateralMintAt, vaultAtaAt, programAt = 2, 4, 5, 6, 7, 9, 12
	}
	if err := expect(marketAt, "market", in.Source.Market); err != nil {
		return wprepLeg{}, err
	}
	if !slices.Contains(dtargetSafeMarkets[in.Account.Cluster], in.Source.Market.String()) {
		return wprepLeg{}, errors.New("Kamino withdraw instruction has an unsafe market.")
	}
	reserve, err := key(reserveAt, "reserve")
	if err != nil {
		return wprepLeg{}, err
	}
	if err := expect(mintAt, "liquidity mint", in.Source.Mint); err != nil {
		return wprepLeg{}, err
	}
	collateralMint, err := key(collateralMintAt, "reserve collateral mint")
	if err != nil {
		return wprepLeg{}, err
	}
	collateralAta, err := key(collateralAt, "vault collateral account")
	if err != nil {
		return wprepLeg{}, err
	}
	if err := expect(vaultAtaAt, "vault USDC account", vaultAta); err != nil {
		return wprepLeg{}, err
	}
	if err := expect(programAt, "liquidity token program", in.Source.TokenProgram); err != nil {
		return wprepLeg{}, err
	}
	if current {
		if err := expect(11, "collateral token program", solana.TokenProgramID); err != nil {
			return wprepLeg{}, err
		}
	}
	return wprepLeg{CollateralAta: collateralAta, CollateralMint: collateralMint, ExecutionMarket: in.Source.Market, ExecutionReserve: reserve}, nil
}

func wprepWithdrawAmount(ix squads.Instruction, fallback uint64) uint64 {
	if len(ix.Data) < 16 {
		return fallback
	}
	return binary.LittleEndian.Uint64(ix.Data[8:16])
}

// wprepChunk is chunkReserveWithdrawals (CL:9122-9174) for one mint: every
// leg here shares the source mint, so the groups collapse to one, chunked.
func wprepChunk(legs []wprepLeg) [][]wprepLeg {
	var batches [][]wprepLeg
	for start := 0; start < len(legs); start += wprepMaxLegsPerBatch {
		batches = append(batches, legs[start:min(start+wprepMaxLegsPerBatch, len(legs))])
	}
	return batches
}

// buildBatch is buildWithdrawBatch (CL:9608-10052) for a partial withdraw.
func (in wprepInput) buildBatch(ctx context.Context, deps wprepDeps, batch []wprepLeg, vaultAta, obligation solana.PublicKey) (EarnWithdrawStepWire, error) {
	source, vault, wallet := in.Source, in.Account.Vault, in.Account.Wallet
	walletAta, err := spl.AssociatedTokenAddress(wallet, source.Mint, source.TokenProgram)
	if err != nil {
		return EarnWithdrawStepWire{}, err
	}
	var batchAmount uint64
	for _, leg := range batch {
		batchAmount += leg.AmountRaw
	}
	setup := []squads.Instruction{wprepIx(spl.CreateIdempotentATA(wallet, vault, source.Mint, source.TokenProgram))}
	seen := map[solana.PublicKey]bool{}
	var tableKeys []solana.PublicKey
	for _, leg := range batch {
		tableKeys = append(tableKeys, leg.LookupTables...)
		if seen[leg.CollateralAta] {
			continue
		}
		seen[leg.CollateralAta] = true
		derived, err := spl.AssociatedTokenAddress(vault, leg.CollateralMint, solana.TokenProgramID)
		if err != nil {
			return EarnWithdrawStepWire{}, err
		}
		if derived == leg.CollateralAta {
			setup = append(setup, wprepIx(spl.CreateIdempotentATA(wallet, vault, leg.CollateralMint, solana.TokenProgramID)))
		}
	}
	tables := earnprep.WithdrawLookupTables(ctx, deps.Chain, tableKeys)
	current, err := earnprep.WithdrawTokenAmountOrZero(ctx, deps.Chain, vaultAta)
	if err != nil {
		return EarnWithdrawStepWire{}, err
	}
	walletAtaCreate := wprepIx(spl.CreateIdempotentATA(wallet, wallet, source.Mint, source.TokenProgram))
	legInstructions := func() []squads.Instruction {
		var out []squads.Instruction
		for _, leg := range batch {
			out = append(out, leg.Instructions...)
		}
		return out
	}
	simulateRedeemed := func() (uint64, error) {
		execute, err := in.memberSync(legInstructions())
		if err != nil {
			return 0, err
		}
		instructions := slices.Concat([]squads.Instruction{walletAtaCreate}, setup, []squads.Instruction{execute})
		simulated, err := earnprep.SimulateWithdrawTokenAmount(ctx, deps.Chain, wallet, instructions, tables, vaultAta)
		if err != nil {
			return 0, err
		}
		return wprepSimulatedRedeemed(current, simulated), nil
	}
	redeemed, err := simulateRedeemed()
	if err != nil {
		return EarnWithdrawStepWire{}, err
	}
	for attempt := 0; redeemed < batchAmount && attempt < wprepMaxAdjustments; attempt++ {
		if len(batch) != 1 {
			return EarnWithdrawStepWire{}, errWprepUnderfilled
		}
		if err := in.adjustLeg(ctx, deps, &batch[0], batchAmount-redeemed); err != nil {
			return EarnWithdrawStepWire{}, err
		}
		if redeemed, err = simulateRedeemed(); err != nil {
			return EarnWithdrawStepWire{}, err
		}
	}
	if redeemed < batchAmount {
		return EarnWithdrawStepWire{}, errWprepUnderfilled
	}
	transfer := wprepVaultTransfer(source.TokenProgram, vaultAta, source.Mint, walletAta, vault, batchAmount)
	execute, err := in.memberSync(append(legInstructions(), transfer))
	if err != nil {
		return EarnWithdrawStepWire{}, err
	}
	prepared := wprepWireOperation(wallet, slices.Concat([]squads.Instruction{walletAtaCreate}, setup, []squads.Instruction{execute}), tables)

	var kaminoTotal uint64
	metadata := make([]EarnWithdrawReserveMetadata, 0, len(batch))
	for _, leg := range batch {
		kaminoTotal += leg.KaminoWithdrawAmountRaw
		metadata = append(metadata, EarnWithdrawReserveMetadata{
			AccountingReserve:       source.Reserve.String(),
			CollateralAta:           leg.CollateralAta.String(),
			ExecutionMarket:         leg.ExecutionMarket.String(),
			ExecutionReserve:        leg.ExecutionReserve.String(),
			KaminoWithdrawAmountRaw: strconv.FormatUint(leg.KaminoWithdrawAmountRaw, 10),
			LiquidityMint:           source.Mint.String(),
			Market:                  source.Market.String(),
			Reserve:                 source.Reserve.String(),
			WithdrawnAmountRaw:      strconv.FormatUint(leg.ExpectedRedeemedRaw, 10),
		})
	}
	first := batch[0]
	amount := strconv.FormatUint(batchAmount, 10)
	persistence := in.persistenceBase()
	persistence.AccountingReserve = source.Reserve.String()
	persistence.ExecutionReserve = first.ExecutionReserve.String()
	persistence.TargetReserve = source.Reserve.String()
	persistence.Market = source.Market.String()
	persistence.LiquidityMint = source.Mint.String()
	persistence.RequestedWithdrawAmountRaw = amount
	persistence.WithdrawnAmountRaw = amount
	persistence.Mode = "partial"
	persistence.KaminoWithdrawAmountRaw = strconv.FormatUint(kaminoTotal, 10)
	persistence.ReserveWithdrawals = metadata
	persistence.VaultUsdcRemainderRaw = "0"
	persistence.WalletTransferAmountRaw = amount
	collateralAta := first.CollateralAta.String()
	return EarnWithdrawStepWire{
		AccountingReserve: &EarnWithdrawAccountingReserve{
			LiquidityMint: source.Mint.String(),
			Market:        source.Market.String(),
			Obligation:    obligation.String(),
			Reserve:       source.Reserve.String(),
		},
		AmountRaw:     amount,
		CollateralAta: &collateralAta,
		ExecutionReserve: &EarnWithdrawExecutionReserve{
			LiquidityMint: source.Mint.String(),
			Market:        first.ExecutionMarket.String(),
			Reserve:       first.ExecutionReserve.String(),
		},
		Mode:               "partial",
		Persistence:        persistence,
		Prepared:           prepared,
		ReserveWithdrawals: metadata,
	}, nil
}

// adjustLeg is one rounding adjustment (CL:9763-9824): add the collateral the
// shortfall needs plus one to the leg's single withdraw instruction.
func (in wprepInput) adjustLeg(ctx context.Context, deps wprepDeps, leg *wprepLeg, shortfall uint64) error {
	if leg.Snapshot == nil {
		_, data, err := wprepReadReserve(ctx, deps.Chain, in.Source, "Kamino reserve account was not found.")
		if err != nil {
			return err
		}
		snapshot, err := earnprep.ParseWithdrawReserveSnapshot(data)
		if err != nil {
			return err
		}
		leg.Snapshot = &snapshot
	}
	collateral, err := earnprep.WithdrawCollateralForRedeemableLiquidityRaw(shortfall, *leg.Snapshot)
	if err != nil {
		return err
	}
	adjusted := leg.KaminoWithdrawAmountRaw + collateral + 1
	replaced := 0
	for i, ix := range leg.Instructions {
		if ix.ProgramID == leg.Instruction.ProgramID && bytes.HasPrefix(ix.Data, kamino.WithdrawV2Discriminator[:]) {
			if len(ix.Data) < 16 {
				return errWprepUnderfilled
			}
			leg.Instructions[i] = wprepWithAmount(ix, adjusted)
			replaced++
		}
	}
	if replaced != 1 {
		return errWprepUnderfilled
	}
	leg.Instruction = wprepWithAmount(leg.Instruction, adjusted)
	leg.KaminoWithdrawAmountRaw = adjusted
	leg.ExpectedRedeemedRaw, err = earnprep.WithdrawRedeemableLiquidityRaw(adjusted, *leg.Snapshot)
	return err
}

func wprepWithAmount(ix squads.Instruction, amountRaw uint64) squads.Instruction {
	data := slices.Clone(ix.Data)
	binary.LittleEndian.PutUint64(data[8:16], amountRaw)
	ix.Data = data
	return ix
}

// wprepSimulatedRedeemed is resolveSimulatedRedeemedAmountRaw (CL:3291-3304).
func wprepSimulatedRedeemed(current, simulated uint64) uint64 {
	if current == 0 || simulated < current {
		return simulated
	}
	return simulated - current
}
