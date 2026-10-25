package earncatalog

const (
	SPLTokenProgram  = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"
	Token2022Program = "TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb"

	CASH       = "CASHx9KJUStyftLFWGvEVf59SGeG9sh5FfcnZMVPCASH"
	USDG       = "2u1tszSeqZ3qBWF3uNGPFc8TzMk2tdiwknnRMWGWjGWH"
	PYUSD      = "2b1kV6DkPAnxd5ixfnxCpjxmKwqjjaYmCZfHsFu24GXo"
	USDC       = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
	USDT       = "Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB"
	USDS       = "USDSwr9ApdHk5bvJKMjzff41FfuX8bSxdKcR81vTwcA"
	DevnetUSDC = "4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU"
)

type Product struct {
	Symbol       string
	Mint         string
	TokenProgram string
}

var mainnet = [...]Product{
	{"CASH", CASH, Token2022Program},
	{"USDG", USDG, Token2022Program},
	{"PYUSD", PYUSD, Token2022Program},
	{"USDC", USDC, SPLTokenProgram},
	{"USDT", USDT, SPLTokenProgram},
	{"USDS", USDS, SPLTokenProgram},
}

var devnet = [...]Product{{"USDC", DevnetUSDC, SPLTokenProgram}}

func Mainnet() []Product { return append([]Product(nil), mainnet[:]...) }

func Devnet() []Product { return append([]Product(nil), devnet[:]...) }

func ForCluster(devnetCluster bool) []Product {
	if devnetCluster {
		return Devnet()
	}
	return Mainnet()
}

const (
	SubscriptionsProgram = "De1egAFMkMWZSN5rYXRj9CAdheBamobVNubTsi9avR44"

	SubscriptionAuthoritySeed = "SubscriptionAuthority"
	SubscriptionDelegateSeed  = "delegation"
	SubscriptionEventSeed     = "event_authority"

	TransferRecurringDiscriminator = 5
	TransferDelegatorOffset        = 9
	TransferMintOffset             = 41

	DelegationDiscriminator       = 3
	DelegationDiscriminatorOffset = 0
	DelegationDelegatorOffset     = 3
	DelegationDelegateeOffset     = 35
	DelegationAuthorityOffset     = 107
	DelegationMintOffset          = 139
	DelegationPerPeriodOffset     = 195
	DelegationAmountPulledOffset  = 203
	DelegationDataLen             = 211
)
