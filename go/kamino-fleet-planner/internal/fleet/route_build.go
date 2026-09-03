package fleet

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
)

const KLendProgram = "KLend2g3cP87fffoy8q1mQqGKjrxjC8boSyAYavgmjD"

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
}
type KaminoSameMintRoute struct {
	Public    []RouteInstruction `json:"public"`
	Protected []RouteInstruction `json:"protected"`
}

type KLendProxy struct{ executable string }

func NewKLendProxy(executable string) (*KLendProxy, error) {
	if executable == "" {
		return nil, fmt.Errorf("KLend proxy executable is required")
	}
	resolved, err := exec.LookPath(executable)
	if err != nil {
		return nil, fmt.Errorf("find KLend proxy: %w", err)
	}
	return &KLendProxy{executable: resolved}, nil
}

type proxyInstruction struct {
	Step     string               `json:"step"`
	Program  string               `json:"program"`
	Accounts []InstructionAccount `json:"accounts"`
	DataHex  string               `json:"dataHex"`
}
type proxyOutput struct {
	Public    []proxyInstruction `json:"public"`
	Protected []proxyInstruction `json:"protected"`
}

// Build asks the small Rust KLend proxy to invoke the official KLend builders.
// The proxy is deterministic: it receives one JSON request and has no RPC,
// database, signer, or transaction-broadcast capability.
func (p *KLendProxy) Build(ctx context.Context, request KaminoSameMintRouteRequest) (KaminoSameMintRoute, error) {
	if p == nil || p.executable == "" {
		return KaminoSameMintRoute{}, fmt.Errorf("KLend proxy is not configured")
	}
	if request.WithdrawCollateralAmount == 0 || request.DepositLiquidityAmount == 0 || request.Source.LiquidityMint == "" || request.Source.LiquidityMint != request.Target.LiquidityMint || request.Source.VaultLiquidityATA != request.Target.VaultLiquidityATA {
		return KaminoSameMintRoute{}, fmt.Errorf("invalid same-mint route request")
	}
	input, err := json.Marshal(request)
	if err != nil {
		return KaminoSameMintRoute{}, err
	}
	command := exec.CommandContext(ctx, p.executable)
	command.Stdin = bytes.NewReader(input)
	stdout := &limitedBuffer{limit: 1 << 20}
	stderr := &limitedBuffer{limit: 64 << 10}
	command.Stdout = stdout
	command.Stderr = stderr
	if err := command.Run(); err != nil {
		return KaminoSameMintRoute{}, fmt.Errorf("KLend proxy failed: %w: %s", err, stderr.String())
	}
	var raw proxyOutput
	decoder := json.NewDecoder(bytes.NewReader(stdout.Bytes()))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&raw); err != nil {
		return KaminoSameMintRoute{}, fmt.Errorf("decode KLend proxy: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return KaminoSameMintRoute{}, err
	}
	convert := func(values []proxyInstruction) ([]RouteInstruction, error) {
		out := make([]RouteInstruction, 0, len(values))
		for _, value := range values {
			data, err := hex.DecodeString(value.DataHex)
			if err != nil {
				return nil, fmt.Errorf("invalid proxy instruction data")
			}
			for _, account := range value.Accounts {
				if _, err := decodePublicKey(account.Address); err != nil {
					return nil, fmt.Errorf("invalid proxy account")
				}
			}
			out = append(out, RouteInstruction{value.Step, value.Program, value.Accounts, data})
		}
		return out, nil
	}
	public, err := convert(raw.Public)
	if err != nil {
		return KaminoSameMintRoute{}, err
	}
	protected, err := convert(raw.Protected)
	if err != nil {
		return KaminoSameMintRoute{}, err
	}
	route := KaminoSameMintRoute{public, protected}
	if err := validateProxyRoute(route, request); err != nil {
		return KaminoSameMintRoute{}, err
	}
	return route, nil
}
func validateProxyRoute(route KaminoSameMintRoute, r KaminoSameMintRouteRequest) error {
	publicCount := 4
	if r.Source.Reserve == r.Target.Reserve {
		publicCount = 3
	}
	if len(route.Public) != publicCount || len(route.Protected) != 2 {
		return fmt.Errorf("KLend proxy returned incomplete route")
	}
	for _, instruction := range append(append([]RouteInstruction{}, route.Public...), route.Protected...) {
		if instruction.Program != KLendProgram {
			return fmt.Errorf("KLend proxy returned foreign program")
		}
	}
	refreshReserveCount := 2
	if r.Source.Reserve == r.Target.Reserve {
		refreshReserveCount = 1
	}
	for index := 0; index < refreshReserveCount; index++ {
		if route.Public[index].Step != "kamino_refresh_reserve" || len(route.Public[index].Accounts) != 6 || hex.EncodeToString(route.Public[index].Data) != "02da8aeb4fc91966" {
			return fmt.Errorf("KLend proxy returned invalid reserve refresh")
		}
	}
	for _, index := range []int{refreshReserveCount, publicCount - 1} {
		if route.Public[index].Step != "kamino_refresh_obligation" || len(route.Public[index].Data) != 8 || hex.EncodeToString(route.Public[index].Data) != "218493e497c04859" {
			return fmt.Errorf("KLend proxy returned invalid obligation refresh")
		}
	}
	if route.Protected[0].Step != "kamino_withdraw_obligation_collateral_and_redeem_reserve_collateral_v2" || len(route.Protected[0].Accounts) != 17 || len(route.Protected[0].Data) != 16 || hex.EncodeToString(route.Protected[0].Data[:8]) != "eb34779895c51407" || binary.LittleEndian.Uint64(route.Protected[0].Data[8:]) != r.WithdrawCollateralAmount {
		return fmt.Errorf("KLend proxy returned invalid withdrawal")
	}
	if route.Protected[1].Step != "kamino_deposit_reserve_liquidity_and_obligation_collateral_v2" || len(route.Protected[1].Accounts) != 17 || len(route.Protected[1].Data) != 16 || hex.EncodeToString(route.Protected[1].Data[:8]) != "d8e0bf1bcc9766af" || binary.LittleEndian.Uint64(route.Protected[1].Data[8:]) != r.DepositLiquidityAmount {
		return fmt.Errorf("KLend proxy returned invalid deposit")
	}
	for _, instruction := range route.Protected {
		if instruction.Accounts[0].Address != r.Vault || !instruction.Accounts[0].Signer || !instruction.Accounts[0].Writable {
			return fmt.Errorf("KLend proxy returned invalid owner account")
		}
	}
	return nil
}
func requireJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return fmt.Errorf("KLend proxy returned trailing output")
	}
	return nil
}

type limitedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *limitedBuffer) Write(value []byte) (int, error) {
	if b.Len()+len(value) > b.limit {
		return 0, fmt.Errorf("output limit exceeded")
	}
	return b.Buffer.Write(value)
}
