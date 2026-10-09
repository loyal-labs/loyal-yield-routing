package fleet

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"

	"github.com/gagliardetto/solana-go"
)

// KLend instruction builders. This is the Go port of the official
// klend-interface builders (Kamino-Finance/klend 23b9f2b, libs/klend-interface)
// that the retired loyal-klend-proxy child invoked. Byte parity with that Rust
// proxy is pinned by testdata/klend/golden.json.

const (
	KLendProgram = "KLend2g3cP87fffoy8q1mQqGKjrxjC8boSyAYavgmjD"

	systemProgram          = "11111111111111111111111111111111"
	rentSysvar             = "SysvarRent111111111111111111111111111111111"
	associatedTokenProgram = "ATokenGPvbdGVxr1b2hvZbsiqW5xWH25efTNsLJA8knL"
)

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
	s, t := &r.Source, &r.Target
	if s.LiquidityMint != t.LiquidityMint || s.VaultLiquidityATA != t.VaultLiquidityATA || s.Reserve == t.Reserve || r.WithdrawCollateralAmount == 0 || r.DepositLiquidityAmount == 0 {
		return nil, fmt.Errorf("invalid same-mint route lane or amount")
	}
	vault, err := solana.PublicKeyFromBase58(r.Vault)
	if err != nil {
		return nil, err
	}
	if err = bindKLendPDAs(s, vault); err != nil {
		return nil, err
	}
	if err = bindKLendPDAs(t, vault); err != nil {
		return nil, err
	}
	// The retained worker refreshes every footprint reserve; this route reads
	// and refreshes only its own two.
	for _, reserve := range append(append(append(append([]string{}, s.ObligationDepositReserves...), s.ObligationBorrowReserves...), t.ObligationDepositReserves...), t.ObligationBorrowReserves...) {
		if reserve != s.Reserve && reserve != t.Reserve {
			return nil, fmt.Errorf("obligation footprint reserve %s is outside the route", reserve)
		}
	}
	owner := vault.String()
	protect := func(ix RouteInstruction) RouteInstruction { ix.Protected = true; return ix }
	route := []RouteInstruction{refreshReserve(*s), refreshReserve(*t)}
	if r.SourceFarmUserMissing {
		route = append(route, initObligationFarm(r.Payer, owner, *s))
	}
	route = append(route, refreshObligation(*s, false))
	withdraw := protect(withdrawV2(owner, *s, r.WithdrawCollateralAmount))
	deposit := protect(depositV2(owner, *t, r.DepositLiquidityAmount))
	if !r.TargetObligationMissing {
		if r.TargetFarmUserMissing {
			route = append(route, initObligationFarm(r.Payer, owner, *t))
		}
		route = append(route, refreshObligation(*t, false), withdraw, refreshObligation(*t, true), deposit)
		return route, canonicalAccounts(KaminoSameMintRoute{Public: route})
	}
	route = append(route, withdraw)
	if r.VaultRentTopUpLamports > 0 {
		route = append(route, RouteInstruction{Step: "system_transfer_vault_rent_top_up", Program: systemProgram, Accounts: []InstructionAccount{{r.Payer, true, true}, {owner, false, true}},
			Data: binary.LittleEndian.AppendUint64([]byte{2, 0, 0, 0}, r.VaultRentTopUpLamports)})
	}
	metadata, err := findProgramAddress(KLendProgram, []byte("user_meta"), vault[:])
	if err != nil {
		return nil, err
	}
	route = append(route, protect(initObligation(owner, *t, metadata)))
	if r.TargetFarmUserMissing {
		route = append(route, initObligationFarm(r.Payer, owner, *t))
	}
	route = append(route, refreshObligation(*t, false), deposit)
	return route, canonicalAccounts(KaminoSameMintRoute{Public: route})
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
	if err = bindKLendPDAs(&r.Source, vault); err != nil {
		return KaminoSameMintRoute{}, err
	}
	if err = bindKLendPDAs(&r.Target, vault); err != nil {
		return KaminoSameMintRoute{}, err
	}
	route := KaminoSameMintRoute{Public: []RouteInstruction{refreshReserve(r.Source), refreshReserve(r.Target), refreshObligation(r.Source, false), refreshObligation(r.Target, true)}, Protected: []RouteInstruction{
		withdrawV2(vault.String(), r.Source, r.WithdrawCollateralAmount),
		depositV2(vault.String(), r.Target, r.DepositLiquidityAmount),
	}}
	return route, canonicalAccounts(route)
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
	if err = bindKLendPDAs(t, vault); err != nil {
		return KaminoSameMintRoute{}, err
	}
	route := KaminoSameMintRoute{
		Public:    []RouteInstruction{refreshReserve(*t), refreshObligation(*t, false)},
		Protected: []RouteInstruction{depositV2(vault.String(), *t, r.DepositLiquidityAmount)},
	}
	return route, canonicalAccounts(route)
}

// BuildDestinationSetup builds one destination account-creation stage. The
// vault ATA and farm stages are public; KLend metadata and obligation creation
// need the vault's signature and are protected.
func BuildDestinationSetup(r DestinationSetupRequest) (KaminoSameMintRoute, error) {
	vault, err := solana.PublicKeyFromBase58(r.Vault)
	if err != nil {
		return KaminoSameMintRoute{}, err
	}
	if err = bindKLendPDAs(&r.Target, vault); err != nil {
		return KaminoSameMintRoute{}, err
	}
	owner := vault.String()
	metadata, err := findProgramAddress(KLendProgram, []byte("user_meta"), vault[:])
	if err != nil {
		return KaminoSameMintRoute{}, err
	}
	t := r.Target
	var ix RouteInstruction
	switch r.Stage {
	case "ata":
		// The source-owned idempotent ATA instruction
		// (c1aebfc0:crates/autonomous-vaults/src/kamino.rs) with the executor
		// paying rent.
		ata, err := deriveATA(owner, t.LiquidityMint, t.LiquidityTokenProgram)
		if err != nil || ata != t.VaultLiquidityATA {
			return KaminoSameMintRoute{}, fmt.Errorf("setup custody is not vault ATA")
		}
		ix = RouteInstruction{Program: associatedTokenProgram, Data: []byte{1}, Accounts: []InstructionAccount{
			{r.Payer, true, true}, {ata, false, true}, {owner, false, false}, {t.LiquidityMint, false, false}, {systemProgram, false, false}, {t.LiquidityTokenProgram, false, false},
		}}
	case "metadata":
		ix = klendInstruction("init_user_metadata", make([]byte, 32), []InstructionAccount{
			{owner, true, false}, {owner, true, true}, {metadata, false, true}, {KLendProgram, false, false}, {rentSysvar, false, false}, {systemProgram, false, false},
		})
	case "obligation":
		ix = initObligation(owner, t, metadata)
	case "farm":
		ix = initObligationFarm(r.Payer, owner, t)
	default:
		return KaminoSameMintRoute{}, fmt.Errorf("unsupported destination setup stage")
	}
	ix.Step = "kamino_setup_" + r.Stage
	route := KaminoSameMintRoute{Public: []RouteInstruction{ix}}
	if r.Stage == "metadata" || r.Stage == "obligation" {
		route = KaminoSameMintRoute{Protected: []RouteInstruction{ix}}
	}
	return route, canonicalAccounts(route)
}

// initObligation creates the vault's vanilla (tag 0, id 0) obligation. The
// vault is owner and rent payer: inside a Squads policy only the vault signs.
func initObligation(owner string, t KaminoPositionAccounts, metadata string) RouteInstruction {
	return klendInstruction("init_obligation", []byte{0, 0}, []InstructionAccount{
		{owner, true, false}, {owner, true, true}, {t.Obligation, false, true}, {t.Market, false, false}, {systemProgram, false, false}, {systemProgram, false, false}, {metadata, false, false}, {rentSysvar, false, false}, {systemProgram, false, false},
	})
}

// initObligationFarm creates the obligation's collateral farm user; the payer
// funds it, so it stays outside the vault's policy.
func initObligationFarm(payer, owner string, t KaminoPositionAccounts) RouteInstruction {
	return klendInstruction("init_obligation_farms_for_reserve", []byte{0}, []InstructionAccount{
		{payer, true, true}, {owner, false, false}, {t.Obligation, false, true}, {t.MarketAuthority, false, false}, {t.Reserve, false, true}, {t.ReserveFarmState, false, true}, {t.ObligationFarmUserState, false, true}, {t.Market, false, false}, {farmsProgram, false, false}, {rentSysvar, false, false}, {systemProgram, false, false},
	})
}

// bindKLendPDAs derives the market authority, the vanilla (tag 0, id 0)
// obligation and the obligation farm user state. Supplied values must equal
// the derivation; empty values are filled from it.
func bindKLendPDAs(p *KaminoPositionAccounts, vault solana.PublicKey) error {
	market, err := solana.PublicKeyFromBase58(p.Market)
	if err != nil {
		return err
	}
	authority, err := findProgramAddress(KLendProgram, []byte("lma"), market[:])
	if err != nil {
		return err
	}
	var zero solana.PublicKey
	obligation, err := findProgramAddress(KLendProgram, []byte{0}, []byte{0}, vault[:], market[:], zero[:], zero[:])
	if err != nil {
		return err
	}
	if p.MarketAuthority != "" && p.MarketAuthority != authority {
		return fmt.Errorf("market authority does not match KLend PDA")
	}
	if p.Obligation != "" && p.Obligation != obligation {
		return fmt.Errorf("obligation does not match vanilla KLend PDA")
	}
	p.MarketAuthority, p.Obligation = authority, obligation
	if p.ReserveFarmState == "" {
		if p.ObligationFarmUserState != "" {
			return fmt.Errorf("farm user state has no reserve farm")
		}
		return nil
	}
	farm, err := solana.PublicKeyFromBase58(p.ReserveFarmState)
	if err != nil {
		return err
	}
	obligationKey := solana.MustPublicKeyFromBase58(obligation)
	user, err := findProgramAddress(farmsProgram, []byte("user"), farm[:], obligationKey[:])
	if err != nil {
		return err
	}
	if p.ObligationFarmUserState != "" && p.ObligationFarmUserState != user {
		return fmt.Errorf("farm user state does not match Farms PDA")
	}
	p.ObligationFarmUserState = user
	return nil
}

func refreshReserve(p KaminoPositionAccounts) RouteInstruction {
	return klendInstruction("refresh_reserve", nil, []InstructionAccount{
		{p.Reserve, false, true}, {p.Market, false, false}, optionalKLendAccount(p.PythOracle, false), optionalKLendAccount(p.SwitchboardPriceOracle, false), optionalKLendAccount(p.SwitchboardTWAPOracle, false), optionalKLendAccount(p.ScopePrices, false),
	})
}

// refreshObligation lists the obligation's complete reserve footprint, or only
// the target reserve after a deposit into it.
func refreshObligation(p KaminoPositionAccounts, targetOnly bool) RouteInstruction {
	accounts := []InstructionAccount{{p.Market, false, false}, {p.Obligation, false, true}}
	reserves := append(append([]string{}, p.ObligationDepositReserves...), p.ObligationBorrowReserves...)
	if targetOnly {
		reserves = []string{p.Reserve}
	}
	for _, reserve := range reserves {
		accounts = append(accounts, InstructionAccount{reserve, false, true})
	}
	return klendInstruction("refresh_obligation", nil, accounts)
}

func withdrawV2(owner string, p KaminoPositionAccounts, collateralAmount uint64) RouteInstruction {
	return klendInstruction("withdraw_obligation_collateral_and_redeem_reserve_collateral_v2", binary.LittleEndian.AppendUint64(nil, collateralAmount), []InstructionAccount{
		{owner, true, true}, {p.Obligation, false, true}, {p.Market, false, false}, {p.MarketAuthority, false, false}, {p.Reserve, false, true}, {p.LiquidityMint, false, false}, {p.CollateralSupply, false, true}, {p.CollateralMint, false, true}, {p.LiquiditySupply, false, true}, {p.VaultLiquidityATA, false, true}, optionalKLendAccount("", false), {tokenProgram, false, false}, {p.LiquidityTokenProgram, false, false}, {instructionsSysvar, false, false}, optionalKLendAccount(p.ObligationFarmUserState, true), optionalKLendAccount(p.ReserveFarmState, true), {farmsProgram, false, false},
	})
}

func depositV2(owner string, p KaminoPositionAccounts, liquidityAmount uint64) RouteInstruction {
	return klendInstruction("deposit_reserve_liquidity_and_obligation_collateral_v2", binary.LittleEndian.AppendUint64(nil, liquidityAmount), []InstructionAccount{
		{owner, true, true}, {p.Obligation, false, true}, {p.Market, false, false}, {p.MarketAuthority, false, false}, {p.Reserve, false, true}, {p.LiquidityMint, false, false}, {p.LiquiditySupply, false, true}, {p.CollateralMint, false, true}, {p.CollateralSupply, false, true}, {p.VaultLiquidityATA, false, true}, optionalKLendAccount("", false), {tokenProgram, false, false}, {p.LiquidityTokenProgram, false, false}, {instructionsSysvar, false, false}, optionalKLendAccount(p.ObligationFarmUserState, true), optionalKLendAccount(p.ReserveFarmState, true), {farmsProgram, false, false},
	})
}

// optionalKLendAccount is klend-interface's optional_account: an absent
// account is passed as the KLend program id, read-only.
func optionalKLendAccount(address string, writable bool) InstructionAccount {
	if address == "" {
		return InstructionAccount{KLendProgram, false, false}
	}
	return InstructionAccount{address, false, writable}
}

func klendInstruction(name string, args []byte, accounts []InstructionAccount) RouteInstruction {
	return RouteInstruction{Step: "kamino_" + name, Program: KLendProgram, Accounts: accounts, Data: append(anchorDiscriminator(name), args...)}
}

func anchorDiscriminator(name string) []byte {
	digest := sha256.Sum256([]byte("global:" + name))
	return append([]byte(nil), digest[:8]...)
}

func findProgramAddress(program string, seeds ...[]byte) (string, error) {
	address, _, err := solana.FindProgramAddress(seeds, solana.MustPublicKeyFromBase58(program))
	if err != nil {
		return "", err
	}
	return address.String(), nil
}

// canonicalAccounts parses every emitted account as a public key and rewrites
// it in canonical base58, exactly as the official builders' Pubkey values
// serialize.
func canonicalAccounts(route KaminoSameMintRoute) error {
	for _, list := range [][]RouteInstruction{route.Public, route.Protected} {
		for i := range list {
			for j := range list[i].Accounts {
				key, err := solana.PublicKeyFromBase58(list[i].Accounts[j].Address)
				if err != nil {
					return fmt.Errorf("invalid KLend account %q: %w", list[i].Accounts[j].Address, err)
				}
				list[i].Accounts[j].Address = key.String()
			}
		}
	}
	return nil
}
