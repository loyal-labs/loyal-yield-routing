package webapp

import (
	"fmt"

	"github.com/solana-foundation/solana-go/v2"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/earncatalog"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
)

// The canonical Earn universe. Key order matters: it is the encoded order of
// the policy's pubkey-list constraints.
var (
	depositKLendProgramID = kamino.ProgramID

	depositMainnetStableMints = catalogMintKeys(earncatalog.Mainnet())
	depositDevnetStableMints  = catalogMintKeys(earncatalog.Devnet())

	depositMainnetSafeMarkets = []solana.PublicKey{
		solana.MustPublicKeyFromBase58("7u3HeHxYDLhnCoErrtycNokbQYbWGzLs6JSDqGAv5PfF"),
		solana.MustPublicKeyFromBase58("CqAoLuqWtavaVE8deBjMKe8ZfSt9ghR6Vb8nfsyabyHA"),
		solana.MustPublicKeyFromBase58("6WEGfej9B9wjxRs6t4BYpb9iCXd8CpTpJ8fVSNzHCC5y"),
		solana.MustPublicKeyFromBase58("47tfyEG9SsdEnUm9cw5kY9BXngQGqu3LBoop9j5uTAv8"),
		solana.MustPublicKeyFromBase58("BJnbcRHqvppTyGesLzWASGKnmnF1wq9jZu6ExrjT7wvF"),
	}
	depositDevnetSafeMarkets = []solana.PublicKey{
		solana.MustPublicKeyFromBase58("27MKCQo5qP7ijrwWSMKX2Jeb3PhK2NZmHQ9befWVRS4J"),
	}

	depositTokenProgramID = solana.MustPublicKeyFromBase58(earncatalog.SPLTokenProgram)

	depositMainnetUSDCReserve = solana.MustPublicKeyFromBase58("D6q6wuQSrifJKZYpR1M8R4YawnLDtDsMmWM1NbBmgJ59")
	depositDevnetUSDCReserve  = solana.MustPublicKeyFromBase58("9uKMtFU9UJ9DfbwzCReGENb31appi79KTEeDGdCnvMjy")

	depositMainnetDepositDiscriminator  = []byte{216, 224, 191, 27, 204, 151, 102, 175}
	depositMainnetWithdrawDiscriminator = []byte{235, 52, 119, 152, 149, 197, 20, 7}
	depositDevnetDepositDiscriminator   = []byte{169, 201, 30, 126, 6, 205, 102, 68}
	depositDevnetWithdrawDiscriminator  = []byte{234, 117, 181, 125, 185, 142, 220, 29}
	depositInitObligationDiscriminator  = []byte{251, 10, 231, 76, 27, 11, 159, 96}
)

type depositEarnTarget struct {
	DepositDiscriminator        []byte
	InitObligationDiscriminator []byte
	LiquidityMint               solana.PublicKey
	LiquidityTokenProgram       solana.PublicKey
	LendProgramID               solana.PublicKey
	Market                      solana.PublicKey
	Reserve                     solana.PublicKey
	WithdrawDiscriminator       []byte
}

func catalogMintKeys(products []earncatalog.Product) []solana.PublicKey {
	keys := make([]solana.PublicKey, len(products))
	for i, product := range products {
		keys[i] = solana.MustPublicKeyFromBase58(product.Mint)
	}
	return keys
}

func depositEarnTargetFor(cluster LoyalCluster) (depositEarnTarget, error) {
	switch cluster {
	case LoyalMainnetBeta:
		return depositEarnTarget{
			DepositDiscriminator:        depositMainnetDepositDiscriminator,
			InitObligationDiscriminator: depositInitObligationDiscriminator,
			LiquidityMint:               depositMainnetStableMints[3],
			LiquidityTokenProgram:       depositTokenProgramID,
			LendProgramID:               depositKLendProgramID,
			Market:                      depositMainnetSafeMarkets[0],
			Reserve:                     depositMainnetUSDCReserve,
			WithdrawDiscriminator:       depositMainnetWithdrawDiscriminator,
		}, nil
	case LoyalDevnet:
		return depositEarnTarget{
			DepositDiscriminator:        depositDevnetDepositDiscriminator,
			InitObligationDiscriminator: depositInitObligationDiscriminator,
			LiquidityMint:               depositDevnetStableMints[0],
			LiquidityTokenProgram:       depositTokenProgramID,
			LendProgramID:               depositKLendProgramID,
			Market:                      depositDevnetSafeMarkets[0],
			Reserve:                     depositDevnetUSDCReserve,
			WithdrawDiscriminator:       depositDevnetWithdrawDiscriminator,
		}, nil
	}
	return depositEarnTarget{}, fmt.Errorf("unsupported Loyal cluster: %s", string(cluster))
}

type depositEarnUniverse struct {
	StableMints []solana.PublicKey
	Markets     []solana.PublicKey
}

func depositEarnUniverseFor(cluster LoyalCluster) (depositEarnUniverse, error) {
	switch cluster {
	case LoyalMainnetBeta:
		return depositEarnUniverse{StableMints: depositMainnetStableMints, Markets: depositMainnetSafeMarkets}, nil
	case LoyalDevnet:
		return depositEarnUniverse{StableMints: depositDevnetStableMints, Markets: depositDevnetSafeMarkets}, nil
	}
	return depositEarnUniverse{}, fmt.Errorf("unsupported Loyal cluster: %s", string(cluster))
}

func depositPubkeyConstraint(index uint8, keys []solana.PublicKey, owner *solana.PublicKey) squads.AccountConstraintView {
	return squads.AccountConstraintView{AccountIndex: index, Pubkeys: keys, Owner: owner}
}

func depositDataEquals(value []byte) squads.DataConstraintView {
	return squads.DataBytes(0, value)
}

// EarnRoutePayload builds the route policy's two instruction constraints:
// KLend withdraw at account 2 over the Safe markets, then KLend deposit at
// account 2 with the liquidity mints at account 5. liquidityMintOwner is nil
// for a fresh creation (policies/prepare) and the token program when matching
// a legacy-generation policy.
func EarnRoutePayload(cluster LoyalCluster, vault solana.PublicKey, liquidityMintOwner *solana.PublicKey) ([]squads.InstructionConstraintView, error) {
	target, err := depositEarnTargetFor(cluster)
	if err != nil {
		return nil, err
	}
	universe, err := depositEarnUniverseFor(cluster)
	if err != nil {
		return nil, err
	}
	withdraw := squads.InstructionConstraintView{
		ProgramID: target.LendProgramID,
		AccountConstraints: []squads.AccountConstraintView{
			depositPubkeyConstraint(0, []solana.PublicKey{vault}, nil),
			depositPubkeyConstraint(2, universe.Markets, nil),
		},
		DataConstraints: []squads.DataConstraintView{depositDataEquals(target.WithdrawDiscriminator)},
	}
	deposit := squads.InstructionConstraintView{
		ProgramID: target.LendProgramID,
		AccountConstraints: []squads.AccountConstraintView{
			depositPubkeyConstraint(0, []solana.PublicKey{vault}, nil),
			depositPubkeyConstraint(2, universe.Markets, nil),
			depositPubkeyConstraint(5, universe.StableMints, liquidityMintOwner),
		},
		DataConstraints: []squads.DataConstraintView{depositDataEquals(target.DepositDiscriminator)},
	}
	return []squads.InstructionConstraintView{withdraw, deposit}, nil
}

// EarnSetupPayload builds the setup policy's one init-obligation constraint:
// one obligation PDA per Safe market, in market order, plus the vault's user
// metadata and the rent/system accounts at positions 7 and 8.
func EarnSetupPayload(cluster LoyalCluster, vault solana.PublicKey) ([]squads.InstructionConstraintView, error) {
	target, err := depositEarnTargetFor(cluster)
	if err != nil {
		return nil, err
	}
	universe, err := depositEarnUniverseFor(cluster)
	if err != nil {
		return nil, err
	}
	obligations := make([]solana.PublicKey, 0, len(universe.Markets))
	for _, market := range universe.Markets {
		obligation, err := kamino.VanillaObligation(vault, market)
		if err != nil {
			return nil, err
		}
		obligations = append(obligations, obligation)
	}
	userMetadata, err := kamino.UserMetadataAddress(vault)
	if err != nil {
		return nil, err
	}
	defaultKey := solana.PublicKey{}
	prefix := append(append([]byte(nil), target.InitObligationDiscriminator...), kamino.VanillaObligationTag, kamino.DefaultObligationID)
	return []squads.InstructionConstraintView{{
		ProgramID: target.LendProgramID,
		AccountConstraints: []squads.AccountConstraintView{
			depositPubkeyConstraint(0, []solana.PublicKey{vault}, nil),
			depositPubkeyConstraint(1, []solana.PublicKey{vault}, nil),
			depositPubkeyConstraint(2, obligations, nil),
			depositPubkeyConstraint(3, universe.Markets, nil),
			depositPubkeyConstraint(4, []solana.PublicKey{defaultKey}, nil),
			depositPubkeyConstraint(5, []solana.PublicKey{defaultKey}, nil),
			depositPubkeyConstraint(6, []solana.PublicKey{userMetadata}, nil),
			depositPubkeyConstraint(7, []solana.PublicKey{solana.SysVarRentPubkey}, nil),
			depositPubkeyConstraint(8, []solana.PublicKey{solana.SystemProgramID}, nil),
		},
		DataConstraints: []squads.DataConstraintView{depositDataEquals(prefix)},
	}}, nil
}
