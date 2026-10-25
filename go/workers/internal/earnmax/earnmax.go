package earnmax

import (
	"github.com/solana-foundation/solana-go/v2"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/earncatalog"
)

const (
	squadsVault  = "ST999VUTo5QExYEX9bz1oDDoKGkjXG9zpphy4Hj7VWh"
	RouteKey     = "rwa-multiply:" + squadsVault
	Cluster      = "mainnet-beta"
	AccountIndex = 2
)

var (
	SquadsVault   = solana.MustPublicKeyFromBase58(squadsVault)
	Vault         = solana.MustPublicKeyFromBase58("HXtk15EA5pBg3rSKxBm8sWPExScPkTknSRp37fXNHgNA")
	AssetMint     = solana.MustPublicKeyFromBase58(earncatalog.USDC)
	LPMint        = solana.MustPublicKeyFromBase58("6tNheTBYSpQkfMLhcczKgmTLSGffK54npKMG1WQR2tvb")
	IdleAuth      = solana.MustPublicKeyFromBase58("EoHz6FHTL34F6HjuJmb5EceaRqxRG1RMYwYWKtWkGBFb")
	IdleATA       = solana.MustPublicKeyFromBase58("6LATwaB4yRwGURCBDyFeJGqofaXxb6xXws9wBGbr3RBh")
	CustodyDebt   = solana.MustPublicKeyFromBase58("J4YFQzxhQ3pht2RRYes5yv1spPYBqvHzxn4zMX7iriHn")
	CustodyCollat = solana.MustPublicKeyFromBase58("9tDh95ofQ7B83bHAou1XJGJNnfRTs3hLfX8uyKW6u97G")
)

type (
	USDCRaw int64
	LPRaw   int64
)
