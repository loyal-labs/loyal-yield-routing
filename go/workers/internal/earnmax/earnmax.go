package earnmax

import "github.com/solana-foundation/solana-go/v2"

const (
	squadsVault = "ST999VUTo5QExYEX9bz1oDDoKGkjXG9zpphy4Hj7VWh"
	RouteKey    = "rwa-multiply:" + squadsVault
)

var (
	SquadsVault   = solana.MustPublicKeyFromBase58(squadsVault)
	Vault         = solana.MustPublicKeyFromBase58("HXtk15EA5pBg3rSKxBm8sWPExScPkTknSRp37fXNHgNA")
	LPMint        = solana.MustPublicKeyFromBase58("6tNheTBYSpQkfMLhcczKgmTLSGffK54npKMG1WQR2tvb")
	IdleATA       = solana.MustPublicKeyFromBase58("6LATwaB4yRwGURCBDyFeJGqofaXxb6xXws9wBGbr3RBh")
	CustodyDebt   = solana.MustPublicKeyFromBase58("J4YFQzxhQ3pht2RRYes5yv1spPYBqvHzxn4zMX7iriHn")
	CustodyCollat = solana.MustPublicKeyFromBase58("9tDh95ofQ7B83bHAou1XJGJNnfRTs3hLfX8uyKW6u97G")
)
