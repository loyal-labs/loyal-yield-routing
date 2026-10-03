package fleet

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
)

// KaminoIdleDepositRequest intentionally has no source or withdrawal amount.
// The target obligation must already exist and its complete, freshly decoded
// footprint must be empty or target-only. Initializing missing accounts and
// taking durable idle/Autodeposit ownership are not this builder's job.
type KaminoIdleDepositRequest struct {
	Vault                  string                 `json:"vault"`
	Target                 KaminoPositionAccounts `json:"target"`
	DepositLiquidityAmount uint64                 `json:"depositLiquidityAmount"`
}

func (p *KLendProxy) BuildIdleDeposit(ctx context.Context, r KaminoIdleDepositRequest) (KaminoSameMintRoute, error) {
	target := &r.Target
	program, supported := stableTokenProgram(target.LiquidityMint)
	if r.DepositLiquidityAmount == 0 || !supported || program != target.LiquidityTokenProgram || target.Obligation == "" || target.MarketAuthority == "" || len(target.ObligationBorrowReserves) != 0 || len(target.ObligationDepositReserves) > 1 || (target.ReserveFarmState == "") != (target.ObligationFarmUserState == "") {
		return KaminoSameMintRoute{}, fmt.Errorf("invalid idle deposit amount or obligation footprint")
	}
	for _, reserve := range target.ObligationDepositReserves {
		if reserve != target.Reserve {
			return KaminoSameMintRoute{}, fmt.Errorf("idle target obligation contains another reserve")
		}
	}
	ata, err := deriveATA(r.Vault, target.LiquidityMint, target.LiquidityTokenProgram)
	if err != nil || ata != target.VaultLiquidityATA {
		return KaminoSameMintRoute{}, fmt.Errorf("idle liquidity account is not the vault ATA")
	}
	if target.ObligationDepositReserves == nil {
		target.ObligationDepositReserves = []string{}
	}
	if target.ObligationBorrowReserves == nil {
		target.ObligationBorrowReserves = []string{}
	}
	route, err := p.invoke(ctx, proxyRequest{1, "buildIdleDeposit", r})
	if err != nil {
		return KaminoSameMintRoute{}, err
	}
	if err = validateIdleProxyRoute(route, r); err != nil {
		return KaminoSameMintRoute{}, err
	}
	return route, nil
}

// Check every account, privilege, discriminator and amount at the Rust/Go
// boundary. A deposit-only response must not smuggle in a withdrawal, a third
// refresh, a substituted token account, or an unowned obligation.
func validateIdleProxyRoute(route KaminoSameMintRoute, r KaminoIdleDepositRequest) error {
	if len(route.Public) != 2 || len(route.Protected) != 1 {
		return fmt.Errorf("KLend proxy returned incomplete idle route")
	}
	account := func(address string, writable bool) InstructionAccount {
		return InstructionAccount{Address: address, Writable: writable}
	}
	optional := func(address string, writable bool) InstructionAccount {
		if address == "" {
			return account(KLendProgram, false)
		}
		return account(address, writable)
	}
	t := r.Target
	expected := [][]InstructionAccount{
		{account(t.Reserve, true), account(t.Market, false), optional(t.PythOracle, false), optional(t.SwitchboardPriceOracle, false), optional(t.SwitchboardTWAPOracle, false), optional(t.ScopePrices, false)},
		{account(t.Market, false), account(t.Obligation, true)},
		{{Address: r.Vault, Signer: true, Writable: true}, account(t.Obligation, true), account(t.Market, false), account(t.MarketAuthority, false), account(t.Reserve, true), account(t.LiquidityMint, false), account(t.LiquiditySupply, true), account(t.CollateralMint, true), account(t.CollateralSupply, true), account(t.VaultLiquidityATA, true), account(KLendProgram, false), account(tokenProgram, false), account(t.LiquidityTokenProgram, false), account("Sysvar1nstructions1111111111111111111111111", false), optional(t.ObligationFarmUserState, true), optional(t.ReserveFarmState, true), account(farmsProgram, false)},
	}
	for _, reserve := range t.ObligationDepositReserves {
		expected[1] = append(expected[1], account(reserve, true))
	}
	steps := []string{"kamino_refresh_reserve", "kamino_refresh_obligation", "kamino_deposit_reserve_liquidity_and_obligation_collateral_v2"}
	data := []string{"02da8aeb4fc91966", "218493e497c04859", "d8e0bf1bcc9766af"}
	instructions := []RouteInstruction{route.Public[0], route.Public[1], route.Protected[0]}
	for i, ix := range instructions {
		size := 8
		if i == 2 {
			size = 16
		}
		if ix.Program != KLendProgram || ix.Step != steps[i] || len(ix.Data) != size || hex.EncodeToString(ix.Data[:8]) != data[i] || len(ix.Accounts) != len(expected[i]) {
			return fmt.Errorf("KLend proxy returned invalid idle instruction %d", i)
		}
		for j, a := range ix.Accounts {
			if a != expected[i][j] {
				return fmt.Errorf("KLend proxy returned invalid idle account %d/%d", i, j)
			}
		}
	}
	if binary.LittleEndian.Uint64(route.Protected[0].Data[8:]) != r.DepositLiquidityAmount {
		return fmt.Errorf("KLend proxy changed idle amount")
	}
	return nil
}
