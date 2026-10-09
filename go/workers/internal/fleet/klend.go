package fleet

import (
	"fmt"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
	"github.com/solana-foundation/solana-go/v2"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/spl"
)

// KLend routes. This is the Go port of the official klend-interface builders
// (Kamino-Finance/klend 23b9f2b, libs/klend-interface) that the retired
// loyal-klend-proxy child invoked. Byte parity with that Rust proxy is pinned
// by testdata/klend/golden.json.

type InstructionAccount struct {
	Address  string `json:"address"`
	Signer   bool   `json:"signer"`
	Writable bool   `json:"writable"`
}
type RouteInstruction struct {
	Step     string               `json:"step"`
	Program  string               `json:"program"`
	Accounts []InstructionAccount `json:"accounts"`
	Data     []byte               `json:"-"`
	// Protected instructions execute as the vault through a Squads policy.
	Protected bool `json:"-"`
}
type KaminoPositionAccounts struct {
	Reserve                   string   `json:"reserve"`
	Market                    string   `json:"market"`
	MarketAuthority           string   `json:"marketAuthority"`
	LiquidityMint             string   `json:"liquidityMint"`
	CollateralMint            string   `json:"collateralMint"`
	LiquiditySupply           string   `json:"liquiditySupply"`
	CollateralSupply          string   `json:"collateralSupply"`
	LiquidityTokenProgram     string   `json:"liquidityTokenProgram"`
	Obligation                string   `json:"obligation"`
	VaultLiquidityATA         string   `json:"vaultLiquidityAta"`
	PythOracle                string   `json:"pythOracle"`
	SwitchboardPriceOracle    string   `json:"switchboardPriceOracle"`
	SwitchboardTWAPOracle     string   `json:"switchboardTwapOracle"`
	ScopePrices               string   `json:"scopePrices"`
	ObligationFarmUserState   string   `json:"obligationFarmUserState"`
	ReserveFarmState          string   `json:"reserveFarmState"`
	ObligationDepositReserves []string `json:"obligationDepositReserves"`
	ObligationBorrowReserves  []string `json:"obligationBorrowReserves"`
}
type KaminoSameMintRouteRequest struct {
	Vault                    string                 `json:"vault"`
	Source                   KaminoPositionAccounts `json:"source"`
	Target                   KaminoPositionAccounts `json:"target"`
	WithdrawCollateralAmount uint64                 `json:"withdrawCollateralAmount"`
	DepositLiquidityAmount   uint64                 `json:"depositLiquidityAmount"`
	// Same-mint setup facts read from chain with the route. The route itself
	// initializes a missing target obligation and missing obligation farm
	// users, paid by Payer; VaultRentTopUpLamports funds the vault, which pays
	// the obligation's rent from inside its policy.
	TargetObligationMissing bool   `json:"targetObligationMissing,omitempty"`
	SourceFarmUserMissing   bool   `json:"sourceFarmUserMissing,omitempty"`
	TargetFarmUserMissing   bool   `json:"targetFarmUserMissing,omitempty"`
	Payer                   string `json:"payer,omitempty"`
	VaultRentTopUpLamports  uint64 `json:"vaultRentTopUpLamports,omitempty"`
}

// KaminoSameMintRoute splits a route into instructions the vault may run
// directly (Public) and instructions that must be wrapped by its policy
// (Protected).
type KaminoSameMintRoute struct {
	Public    []RouteInstruction `json:"public"`
	Protected []RouteInstruction `json:"protected"`
}

// KaminoIdleDepositRequest intentionally has no source or withdrawal amount.
// The target obligation must already exist and its complete, freshly decoded
// footprint must be empty or target-only. Initializing missing accounts and
// taking durable idle/Autodeposit ownership are not this builder's job.
type KaminoIdleDepositRequest struct {
	Vault                  string                 `json:"vault"`
	Target                 KaminoPositionAccounts `json:"target"`
	DepositLiquidityAmount uint64                 `json:"depositLiquidityAmount"`
}

// DestinationSetupRequest builds one account-creation stage. It cannot move
// wallet liquidity or create/update policy permissions.
type DestinationSetupRequest struct {
	Stage  string                 `json:"stage"`
	Vault  string                 `json:"vault"`
	Payer  string                 `json:"payer"`
	Target KaminoPositionAccounts `json:"target"`
}

// setup reports a route that creates accounts, and so pays rent.
func (r KaminoSameMintRouteRequest) setup() bool {
	return r.TargetObligationMissing || r.SourceFarmUserMissing || r.TargetFarmUserMissing
}

// BuildSameMintRoute builds one atomic same-mint move in the retained
// worker's order (build_route_execution_plan): refresh both reserves, create a
// missing source farm user, refresh the source obligation and, for an existing
// target, create its missing farm user and refresh it; withdraw; then either
// refresh the target for its reserve or, for a missing target obligation, fund
// the vault's obligation rent, initialize the obligation and its farm user and
// refresh it; deposit. Setup is part of the move: withdrawal and deposit share
// one transaction, so funds never sit idle between transactions (ee8715ad).
func BuildSameMintRoute(r KaminoSameMintRouteRequest) ([]RouteInstruction, error) {
	if r.Source.LiquidityMint != r.Target.LiquidityMint || r.Source.VaultLiquidityATA != r.Target.VaultLiquidityATA || r.Source.Reserve == r.Target.Reserve || r.WithdrawCollateralAmount == 0 || r.DepositLiquidityAmount == 0 {
		return nil, fmt.Errorf("invalid same-mint route lane or amount")
	}
	vault, err := solana.PublicKeyFromBase58(r.Vault)
	if err != nil {
		return nil, err
	}
	s, err := bindKLendPDAs(&r.Source, vault)
	if err != nil {
		return nil, err
	}
	t, err := bindKLendPDAs(&r.Target, vault)
	if err != nil {
		return nil, err
	}
	// The retained worker refreshes every footprint reserve; this route reads
	// and refreshes only its own two.
	for _, reserve := range append(append([]solana.PublicKey{}, s.reserves...), t.reserves...) {
		if reserve != s.Reserve && reserve != t.Reserve {
			return nil, fmt.Errorf("obligation footprint reserve %s is outside the route", reserve)
		}
	}
	protect := func(ix RouteInstruction) RouteInstruction { ix.Protected = true; return ix }
	route := []RouteInstruction{refreshReserve(s), refreshReserve(t)}
	var sourceFarm, targetFarm RouteInstruction
	if r.SourceFarmUserMissing {
		if sourceFarm, err = initObligationFarm(r.Payer, s); err != nil {
			return nil, err
		}
		route = append(route, sourceFarm)
	}
	if r.TargetFarmUserMissing {
		if targetFarm, err = initObligationFarm(r.Payer, t); err != nil {
			return nil, err
		}
	}
	route = append(route, refreshObligation(s, false))
	withdraw := protect(withdrawV2(s, r.WithdrawCollateralAmount))
	deposit := protect(depositV2(t, r.DepositLiquidityAmount))
	if !r.TargetObligationMissing {
		if r.TargetFarmUserMissing {
			route = append(route, targetFarm)
		}
		route = append(route, refreshObligation(t, false), withdraw, refreshObligation(t, true), deposit)
		return route, nil
	}
	route = append(route, withdraw)
	if r.VaultRentTopUpLamports > 0 {
		payer, err := solana.PublicKeyFromBase58(r.Payer)
		if err != nil {
			return nil, err
		}
		route = append(route, RouteInstructionOf("system_transfer_vault_rent_top_up", spl.SystemTransfer(payer, vault, r.VaultRentTopUpLamports)))
	}
	metadata, err := kamino.UserMetadataAddress(vault)
	if err != nil {
		return nil, err
	}
	route = append(route, protect(initObligation(vault, t, metadata)))
	if r.TargetFarmUserMissing {
		route = append(route, targetFarm)
	}
	route = append(route, refreshObligation(t, false), deposit)
	return route, nil
}

// BuildCrossMintLegs builds the independent KLend withdrawal and deposit legs.
// It does not combine withdrawal, Jupiter swap, and deposit into a transaction.
func BuildCrossMintLegs(r KaminoSameMintRouteRequest) (KaminoSameMintRoute, error) {
	if r.Source.LiquidityMint == r.Target.LiquidityMint || r.Source.VaultLiquidityATA == r.Target.VaultLiquidityATA || r.WithdrawCollateralAmount == 0 || r.DepositLiquidityAmount == 0 ||
		r.TargetObligationMissing || r.SourceFarmUserMissing || r.TargetFarmUserMissing || r.VaultRentTopUpLamports > 0 {
		return KaminoSameMintRoute{}, fmt.Errorf("invalid KLend route lane or amount")
	}
	vault, err := solana.PublicKeyFromBase58(r.Vault)
	if err != nil {
		return KaminoSameMintRoute{}, err
	}
	s, err := bindKLendPDAs(&r.Source, vault)
	if err != nil {
		return KaminoSameMintRoute{}, err
	}
	t, err := bindKLendPDAs(&r.Target, vault)
	if err != nil {
		return KaminoSameMintRoute{}, err
	}
	return KaminoSameMintRoute{
		Public:    []RouteInstruction{refreshReserve(s), refreshReserve(t), refreshObligation(s, false), refreshObligation(t, true)},
		Protected: []RouteInstruction{withdrawV2(s, r.WithdrawCollateralAmount), depositV2(t, r.DepositLiquidityAmount)},
	}, nil
}

// BuildIdleDeposit deposits vault-ATA liquidity into an existing obligation
// whose footprint is empty or holds only the target reserve.
func BuildIdleDeposit(r KaminoIdleDepositRequest) (KaminoSameMintRoute, error) {
	t := &r.Target
	program, supported := stableTokenProgram(t.LiquidityMint)
	if r.DepositLiquidityAmount == 0 || !supported || program != t.LiquidityTokenProgram || t.Obligation == "" || t.MarketAuthority == "" || len(t.ObligationBorrowReserves) != 0 || len(t.ObligationDepositReserves) > 1 || (t.ReserveFarmState == "") != (t.ObligationFarmUserState == "") {
		return KaminoSameMintRoute{}, fmt.Errorf("invalid idle deposit amount or obligation footprint")
	}
	for _, reserve := range t.ObligationDepositReserves {
		if reserve != t.Reserve {
			return KaminoSameMintRoute{}, fmt.Errorf("idle target obligation contains another reserve")
		}
	}
	ata, err := deriveATA(r.Vault, t.LiquidityMint, t.LiquidityTokenProgram)
	if err != nil || ata != t.VaultLiquidityATA {
		return KaminoSameMintRoute{}, fmt.Errorf("idle liquidity account is not the vault ATA")
	}
	vault, err := solana.PublicKeyFromBase58(r.Vault)
	if err != nil {
		return KaminoSameMintRoute{}, err
	}
	target, err := bindKLendPDAs(t, vault)
	if err != nil {
		return KaminoSameMintRoute{}, err
	}
	return KaminoSameMintRoute{
		Public:    []RouteInstruction{refreshReserve(target), refreshObligation(target, false)},
		Protected: []RouteInstruction{depositV2(target, r.DepositLiquidityAmount)},
	}, nil
}

// BuildDestinationSetup builds one destination account-creation stage. The
// vault ATA and farm stages are public; KLend metadata and obligation creation
// need the vault's signature and are protected.
func BuildDestinationSetup(r DestinationSetupRequest) (KaminoSameMintRoute, error) {
	vault, err := solana.PublicKeyFromBase58(r.Vault)
	if err != nil {
		return KaminoSameMintRoute{}, err
	}
	t, err := bindKLendPDAs(&r.Target, vault)
	if err != nil {
		return KaminoSameMintRoute{}, err
	}
	metadata, err := kamino.UserMetadataAddress(vault)
	if err != nil {
		return KaminoSameMintRoute{}, err
	}
	var ix RouteInstruction
	switch r.Stage {
	case "ata":
		// The source-owned idempotent ATA instruction
		// (c1aebfc0:crates/autonomous-vaults/src/kamino.rs) with the executor
		// paying rent.
		ata, err := deriveATA(r.Vault, r.Target.LiquidityMint, r.Target.LiquidityTokenProgram)
		payer, payerErr := solana.PublicKeyFromBase58(r.Payer)
		if err != nil || payerErr != nil || ata != r.Target.VaultLiquidityATA {
			return KaminoSameMintRoute{}, fmt.Errorf("setup custody is not vault ATA")
		}
		ix = RouteInstructionOf("", spl.CreateIdempotentATA(payer, vault, t.LiquidityMint, t.LiquidityTokenProgram))
	case "metadata":
		ix = RouteInstructionOf("", kamino.InitUserMetadata(vault, vault, metadata, solana.PublicKey{}))
	case "obligation":
		ix = initObligation(vault, t, metadata)
	case "farm":
		if ix, err = initObligationFarm(r.Payer, t); err != nil {
			return KaminoSameMintRoute{}, err
		}
	default:
		return KaminoSameMintRoute{}, fmt.Errorf("unsupported destination setup stage")
	}
	ix.Step = "kamino_setup_" + r.Stage
	route := KaminoSameMintRoute{Public: []RouteInstruction{ix}}
	if r.Stage == "metadata" || r.Stage == "obligation" {
		route = KaminoSameMintRoute{Protected: []RouteInstruction{ix}}
	}
	return route, nil
}

// initObligation creates the vault's vanilla (tag 0, id 0) obligation. The
// vault is owner and rent payer: inside a Squads policy only the vault signs.
func initObligation(owner solana.PublicKey, t klendPosition, metadata solana.PublicKey) RouteInstruction {
	return RouteInstructionOf("kamino_init_obligation", kamino.InitObligation(owner, owner, t.Obligation, t.LendingMarket, solana.PublicKey{}, solana.PublicKey{}, metadata, 0, 0))
}

// initObligationFarm creates the obligation's collateral farm user; the payer
// funds it, so it stays outside the vault's policy. The reserve must have a
// collateral farm.
func initObligationFarm(payer string, t klendPosition) (RouteInstruction, error) {
	key, err := solana.PublicKeyFromBase58(payer)
	if err != nil || t.ReserveFarmState.IsZero() {
		return RouteInstruction{}, fmt.Errorf("invalid KLend farm setup payer %q or reserve farm", payer)
	}
	return RouteInstructionOf("kamino_init_obligation_farms_for_reserve", kamino.InitObligationFarmsForReserve(kamino.InitObligationFarmsAccounts{
		Payer: key, Owner: t.Owner, Obligation: t.Obligation, LendingMarketAuthority: t.LendingMarketAuthority, Reserve: t.Reserve,
		ReserveFarmState: t.ReserveFarmState, ObligationFarmUserState: t.ObligationFarmUserState, LendingMarket: t.LendingMarket,
	}, 0)), nil
}

// klendPosition is a position's KLend accounts, parsed for the vault.
type klendPosition struct {
	kamino.CollateralAccounts
	oracles  kamino.RefreshReserveAccounts
	reserves []solana.PublicKey // the obligation footprint, deposits then borrows
}

// bindKLendPDAs derives the market authority, the vanilla (tag 0, id 0)
// obligation and the obligation farm user state. Supplied values must equal
// the derivation; empty values are filled from it. It returns the bound
// position's parsed accounts.
func bindKLendPDAs(p *KaminoPositionAccounts, vault solana.PublicKey) (klendPosition, error) {
	market, err := solana.PublicKeyFromBase58(p.Market)
	if err != nil {
		return klendPosition{}, err
	}
	authority, err := kamino.LendingMarketAuthority(market)
	if err != nil {
		return klendPosition{}, err
	}
	obligation, err := kamino.VanillaObligation(vault, market)
	if err != nil {
		return klendPosition{}, err
	}
	if p.MarketAuthority != "" && p.MarketAuthority != authority.String() {
		return klendPosition{}, fmt.Errorf("market authority does not match KLend PDA")
	}
	if p.Obligation != "" && p.Obligation != obligation.String() {
		return klendPosition{}, fmt.Errorf("obligation does not match vanilla KLend PDA")
	}
	p.MarketAuthority, p.Obligation = authority.String(), obligation.String()
	if p.ReserveFarmState == "" {
		if p.ObligationFarmUserState != "" {
			return klendPosition{}, fmt.Errorf("farm user state has no reserve farm")
		}
		return p.klend(vault)
	}
	farm, err := solana.PublicKeyFromBase58(p.ReserveFarmState)
	if err != nil {
		return klendPosition{}, err
	}
	user, err := kamino.ObligationFarmUserState(farm, obligation)
	if err != nil {
		return klendPosition{}, err
	}
	if p.ObligationFarmUserState != "" && p.ObligationFarmUserState != user.String() {
		return klendPosition{}, fmt.Errorf("farm user state does not match Farms PDA")
	}
	p.ObligationFarmUserState = user.String()
	return p.klend(vault)
}

// klend parses the position's addresses; an empty optional one stays zero.
func (p KaminoPositionAccounts) klend(owner solana.PublicKey) (klendPosition, error) {
	var err error
	parse := func(value string, optional bool) solana.PublicKey {
		if err != nil || optional && value == "" {
			return solana.PublicKey{}
		}
		key, parseErr := solana.PublicKeyFromBase58(value)
		if parseErr != nil {
			err = fmt.Errorf("invalid KLend account %q: %w", value, parseErr)
		}
		return key
	}
	out := klendPosition{CollateralAccounts: kamino.CollateralAccounts{
		Owner: owner, Obligation: parse(p.Obligation, false), LendingMarket: parse(p.Market, false), LendingMarketAuthority: parse(p.MarketAuthority, false),
		Reserve: parse(p.Reserve, false), LiquidityMint: parse(p.LiquidityMint, false), LiquiditySupply: parse(p.LiquiditySupply, false),
		CollateralMint: parse(p.CollateralMint, false), CollateralSupply: parse(p.CollateralSupply, false), UserLiquidity: parse(p.VaultLiquidityATA, false),
		LiquidityTokenProgram: parse(p.LiquidityTokenProgram, false), ObligationFarmUserState: parse(p.ObligationFarmUserState, true), ReserveFarmState: parse(p.ReserveFarmState, true),
	}}
	out.oracles = kamino.RefreshReserveAccounts{Reserve: out.Reserve, LendingMarket: out.LendingMarket, Pyth: parse(p.PythOracle, true),
		SwitchboardPrice: parse(p.SwitchboardPriceOracle, true), SwitchboardTWAP: parse(p.SwitchboardTWAPOracle, true), Scope: parse(p.ScopePrices, true)}
	for _, reserve := range append(append([]string{}, p.ObligationDepositReserves...), p.ObligationBorrowReserves...) {
		out.reserves = append(out.reserves, parse(reserve, false))
	}
	return out, err
}

func refreshReserve(p klendPosition) RouteInstruction {
	return RouteInstructionOf("kamino_refresh_reserve", kamino.RefreshReserve(p.oracles))
}

// refreshObligation lists the obligation's complete reserve footprint, or only
// the target reserve after a deposit into it.
func refreshObligation(p klendPosition, targetOnly bool) RouteInstruction {
	reserves := p.reserves
	if targetOnly {
		reserves = []solana.PublicKey{p.Reserve}
	}
	return RouteInstructionOf("kamino_refresh_obligation", kamino.RefreshObligation(p.LendingMarket, p.Obligation, reserves...))
}

func withdrawV2(p klendPosition, collateralAmount uint64) RouteInstruction {
	return RouteInstructionOf("kamino_withdraw_obligation_collateral_and_redeem_reserve_collateral_v2", kamino.WithdrawV2(p.CollateralAccounts, collateralAmount))
}

func depositV2(p klendPosition, liquidityAmount uint64) RouteInstruction {
	return RouteInstructionOf("kamino_deposit_reserve_liquidity_and_obligation_collateral_v2", kamino.DepositV2(p.CollateralAccounts, liquidityAmount))
}
