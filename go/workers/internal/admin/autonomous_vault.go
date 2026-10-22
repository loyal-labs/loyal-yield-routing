package admin

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"
	"math/big"
	"os"
	"strconv"
	"time"

	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/meteora"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/spl"
)

const autonomousVaultDefaultRPC = "https://api.mainnet-beta.solana.com"

var autonomousVaultManifest = struct {
	kaminoMarket, kaminoObligation, kaminoReserve solana.PublicKey
	loyalMint, usdcMint                           solana.PublicKey
	meteoraPool, meteoraPosition                  solana.PublicKey
	vault                                         solana.PublicKey
}{
	kaminoMarket:     solana.MustPublicKeyFromBase58("7u3HeHxYDLhnCoErrtycNokbQYbWGzLs6JSDqGAv5PfF"),
	kaminoObligation: solana.MustPublicKeyFromBase58("4HhTdUm6Z1GLTjW9bvv3B9X5tHedJC8zHwdGtvu3yqKU"),
	kaminoReserve:    solana.MustPublicKeyFromBase58("D6q6wuQSrifJKZYpR1M8R4YawnLDtDsMmWM1NbBmgJ59"),
	loyalMint:        solana.MustPublicKeyFromBase58("LYLikzBQtpa9ZgVrJsqYGQpR3cC1WMJrBHaXGrQmeta"),
	usdcMint:         solana.MustPublicKeyFromBase58("EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"),
	meteoraPool:      solana.MustPublicKeyFromBase58("c29DVknA5DZUCH6U5ujo1EGfiKKXZrUk6yk56yJxLrm"),
	meteoraPosition:  solana.MustPublicKeyFromBase58("3SBxJxpCG2EbfLrvD5DgjKUarLgn5i643WYNnUdTYgCB"),
	vault:            solana.MustPublicKeyFromBase58("F7zuL14omw4JJfS1cvsWXVb3wh48dvsonMJgoc9tYu3e"),
}

type AutonomousVaultPage struct {
	ObservedAt string                   `json:"observedAt"`
	Error      string                   `json:"error"`
	Snapshot   *AutonomousVaultSnapshot `json:"snapshot,omitempty"`
}

type AutonomousVaultSnapshot struct {
	DeployedUsdcRaw string                 `json:"deployedUsdcRaw"`
	Idle            AutonomousVaultIdle    `json:"idle"`
	Kamino          AutonomousVaultKamino  `json:"kamino"`
	Meteora         AutonomousVaultMeteora `json:"meteora"`
	ObservedSlot    uint64                 `json:"observedSlot"`
	Status          string                 `json:"status" enum:"attention,healthy"`
	TotalLoyalRaw   string                 `json:"totalLoyalRaw"`
	TotalUsdcRaw    string                 `json:"totalUsdcRaw"`
	Vault           string                 `json:"vault"`
}

type AutonomousVaultIdle struct {
	LoyalRaw string `json:"loyalRaw"`
	UsdcRaw  string `json:"usdcRaw"`
}

type AutonomousVaultKamino struct {
	Reserve      string `json:"reserve"`
	ValueUsdcRaw string `json:"valueUsdcRaw"`
}

type AutonomousVaultMeteora struct {
	ActiveBin         int32   `json:"activeBin"`
	LoyalRaw          string  `json:"loyalRaw"`
	Position          string  `json:"position"`
	PriceUsdcPerLoyal float64 `json:"priceUsdcPerLoyal"`
	UsdcRaw           string  `json:"usdcRaw"`
}

func autonomousVaultRPCURL() string {
	return cmp.Or(os.Getenv("LOYAL_ADMIN_SOLANA_RPC_URL"), autonomousVaultDefaultRPC)
}

func registerAutonomousVault(m *Mux) {
	client, err := chain.New(autonomousVaultRPCURL(), readTimeout)
	Page(m, "/earn/autonomous-vault", func(ctx context.Context, _ *Deps, _ PageRequest) (AutonomousVaultPage, error) {
		if err != nil {
			return AutonomousVaultPage{}, err
		}
		page := AutonomousVaultPage{ObservedAt: time.Now().UTC().Format("2006-01-02T15:04:05.000Z07:00")}
		ctx, cancel := context.WithTimeout(ctx, readTimeout)
		defer cancel()
		snapshot, err := autonomousVaultRead(ctx, client)
		if err != nil {
			page.Error = chain.Scrub(err.Error())
			return page, nil
		}
		page.Snapshot = &snapshot
		return page, nil
	})
}

func autonomousVaultRead(ctx context.Context, client *chain.Client) (AutonomousVaultSnapshot, error) {
	m := autonomousVaultManifest
	vaultUsdc, err := spl.AssociatedTokenAddress(m.vault, m.usdcMint, solana.TokenProgramID)
	if err != nil {
		return AutonomousVaultSnapshot{}, err
	}
	vaultLoyal, err := spl.AssociatedTokenAddress(m.vault, m.loyalMint, solana.TokenProgramID)
	if err != nil {
		return AutonomousVaultSnapshot{}, err
	}
	slot, accounts, err := client.Accounts(ctx, []solana.PublicKey{
		m.kaminoObligation, m.kaminoReserve, vaultUsdc, vaultLoyal, m.meteoraPosition, m.meteoraPool, m.loyalMint, m.usdcMint,
	}, rpc.CommitmentFinalized, 0)
	if err != nil {
		return AutonomousVaultSnapshot{}, err
	}
	obligationAccount, reserveAccount, usdcAccount, loyalAccount, positionAccount, poolAccount, loyalMintAccount, usdcMintAccount := accounts[0], accounts[1], accounts[2], accounts[3], accounts[4], accounts[5], accounts[6], accounts[7]

	position, err := meteora.DecodePosition(positionAccount)
	if err != nil {
		return AutonomousVaultSnapshot{}, err
	}
	if position.LbPair != m.meteoraPool {
		return AutonomousVaultSnapshot{}, errors.New("Meteora position belongs to a different pool.")
	}
	if position.Owner != m.vault {
		return AutonomousVaultSnapshot{}, errors.New("Meteora position is not owned by the autonomous vault.")
	}
	indexes := position.BinArrayIndexes()
	binArrayKeys := make([]solana.PublicKey, len(indexes))
	for i, index := range indexes {
		if binArrayKeys[i], err = meteora.BinArrayAddress(m.meteoraPool, index); err != nil {
			return AutonomousVaultSnapshot{}, err
		}
	}
	_, binArrayAccounts, err := client.Accounts(ctx, binArrayKeys, rpc.CommitmentFinalized, slot)
	if err != nil {
		return AutonomousVaultSnapshot{}, err
	}
	binArrays := map[int64]meteora.BinArray{}
	for i, account := range binArrayAccounts {
		if account == nil {
			continue
		}
		array, err := meteora.DecodeBinArray(account)
		if err != nil {
			return AutonomousVaultSnapshot{}, err
		}
		if array.Index != indexes[i] || array.LbPair != m.meteoraPool {
			return AutonomousVaultSnapshot{}, fmt.Errorf("Meteora bin array %s does not match the pool.", account.Key)
		}
		binArrays[array.Index] = array
	}

	idleUsdc, err := autonomousVaultTokenAmount(usdcAccount, "Vault USDC account", m.usdcMint)
	if err != nil {
		return AutonomousVaultSnapshot{}, err
	}
	idleLoyal, err := autonomousVaultTokenAmount(loyalAccount, "Vault LOYAL account", m.loyalMint)
	if err != nil {
		return AutonomousVaultSnapshot{}, err
	}

	obligation, err := kamino.DecodeObligation(obligationAccount)
	if err != nil {
		return AutonomousVaultSnapshot{}, err
	}
	if obligation.Owner != m.vault || obligation.LendingMarket != m.kaminoMarket {
		return AutonomousVaultSnapshot{}, errors.New("Kamino obligation identity differs from the manifest.")
	}
	reserve, err := kamino.DecodeReserve(reserveAccount)
	if err != nil {
		return AutonomousVaultSnapshot{}, err
	}
	if reserve.LendingMarket != m.kaminoMarket || reserve.LiquidityMint != m.usdcMint {
		return AutonomousVaultSnapshot{}, errors.New("Kamino reserve graph differs from the manifest.")
	}
	collateral := obligation.Collateral(m.kaminoReserve)
	if collateral == 0 {
		return AutonomousVaultSnapshot{}, errors.New("Kamino has no funded autonomous-vault position.")
	}
	kaminoUsdc, err := reserve.CollateralToLiquidity(collateral)
	if err != nil {
		return AutonomousVaultSnapshot{}, err
	}

	pool, err := meteora.DecodeLbPair(poolAccount)
	if err != nil {
		return AutonomousVaultSnapshot{}, err
	}
	if pool.TokenXMint != m.loyalMint || pool.TokenYMint != m.usdcMint {
		return AutonomousVaultSnapshot{}, errors.New("Meteora pool mints differ from the manifest.")
	}
	price, err := autonomousVaultPoolPrice(pool, loyalMintAccount, usdcMintAccount)
	if err != nil {
		return AutonomousVaultSnapshot{}, err
	}
	bins, err := position.Amounts(binArrays)
	if err != nil {
		return AutonomousVaultSnapshot{}, err
	}
	meteoraLoyal, meteoraUsdc := new(big.Int), new(big.Int)
	funded := false
	minFunded, maxFunded := int32(math.MaxInt32), int32(math.MinInt32)
	for _, bin := range bins {
		meteoraLoyal.Add(meteoraLoyal, bin.AmountX).Add(meteoraLoyal, bin.FeeX)
		meteoraUsdc.Add(meteoraUsdc, bin.AmountY).Add(meteoraUsdc, bin.FeeY)
		if bin.LiquidityShare.Sign() > 0 || bin.AmountX.Sign() > 0 || bin.AmountY.Sign() > 0 {
			funded = true
			minFunded, maxFunded = min(minFunded, bin.BinID), max(maxFunded, bin.BinID)
		}
	}
	status := "attention"
	if pool.Status == 0 && funded && pool.ActiveID >= minFunded && pool.ActiveID <= maxFunded {
		status = "healthy"
	}

	deployedUsdc := new(big.Int).Add(new(big.Int).SetUint64(kaminoUsdc), meteoraUsdc)
	return AutonomousVaultSnapshot{
		DeployedUsdcRaw: deployedUsdc.String(),
		Idle:            AutonomousVaultIdle{LoyalRaw: strconv.FormatUint(idleLoyal, 10), UsdcRaw: strconv.FormatUint(idleUsdc, 10)},
		Kamino:          AutonomousVaultKamino{Reserve: m.kaminoReserve.String(), ValueUsdcRaw: strconv.FormatUint(kaminoUsdc, 10)},
		Meteora: AutonomousVaultMeteora{
			ActiveBin: pool.ActiveID, LoyalRaw: meteoraLoyal.String(), Position: m.meteoraPosition.String(),
			PriceUsdcPerLoyal: price, UsdcRaw: meteoraUsdc.String(),
		},
		ObservedSlot:  slot,
		Status:        status,
		TotalLoyalRaw: new(big.Int).Add(meteoraLoyal, new(big.Int).SetUint64(idleLoyal)).String(),
		TotalUsdcRaw:  new(big.Int).Add(deployedUsdc, new(big.Int).SetUint64(idleUsdc)).String(),
		Vault:         m.vault.String(),
	}, nil
}

func autonomousVaultTokenAmount(account *chain.Account, label string, mint solana.PublicKey) (uint64, error) {
	if account == nil {
		return 0, fmt.Errorf("%s is missing from finalized mainnet state.", label)
	}
	if account.Owner != solana.TokenProgramID {
		return 0, fmt.Errorf("%s has an unexpected program owner.", label)
	}
	token, err := spl.DecodeTokenAccount(account)
	if err != nil {
		return 0, err
	}
	if token.Owner != autonomousVaultManifest.vault || token.Mint != mint {
		return 0, fmt.Errorf("%s does not match the autonomous vault manifest.", label)
	}
	return token.Amount, nil
}

func autonomousVaultPoolPrice(pool meteora.LbPair, loyalMintAccount, usdcMintAccount *chain.Account) (float64, error) {
	if loyalMintAccount == nil || usdcMintAccount == nil {
		return 0, errors.New("Meteora pool mints are missing from finalized mainnet state.")
	}
	loyal, err := spl.DecodeMint(loyalMintAccount)
	if err != nil {
		return 0, err
	}
	usdc, err := spl.DecodeMint(usdcMintAccount)
	if err != nil {
		return 0, err
	}
	price := meteora.PriceOfBin(pool.ActiveID, pool.BinStep) * math.Pow10(int(loyal.Decimals)-int(usdc.Decimals))
	if math.IsNaN(price) || math.IsInf(price, 0) || price <= 0 {
		return 0, errors.New("Meteora returned an invalid active-bin price.")
	}
	return price, nil
}
