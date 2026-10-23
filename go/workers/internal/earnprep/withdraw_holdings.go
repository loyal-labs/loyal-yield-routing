package earnprep

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/spl"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
)

// The RPC holdings snapshot, a mirror of fetchEarnRpcHoldingsSnapshot
// (web/apps/web/src/lib/yield-optimization/earn-rpc-holdings.client.ts:422-662).
// Stage 1 reads the Earn vault's idle product ATAs and its vanilla obligation
// per allowed market; stage 2 reads the reserves behind every discovered
// deposit. Kamino amounts are redeemable liquidity, not raw collateral. The
// function only throws; callers map failures to their own envelopes.

// RPC is the chain surface the withdraw family reads through; *chain.Client
// satisfies it.
type RPC interface {
	Accounts(ctx context.Context, keys []solana.PublicKey, commitment rpc.CommitmentType, minContextSlot uint64) (uint64, []*chain.Account, error)
}

// WithdrawVaultIndex is the Earn vault's account index (EARN_VAULT_INDEX,
// loyal-actions/src/constants.ts:49).
const WithdrawVaultIndex uint8 = 1

// withdrawCommitment is SOURCE_COMMITMENT (earn-rpc-holdings.client.ts:44).
var withdrawCommitment = rpc.CommitmentConfirmed

const withdrawGetMultipleAccountsLimit = 100

// WithdrawPolicyUniverse is the policy metadata the snapshot narrows to
// (EarnRpcPolicyMetadata, earn-rpc-holdings.client.ts:47): the vault index,
// the policy's stable mints, its Kamino liquidity mints and its markets.
type WithdrawPolicyUniverse struct {
	VaultIndex           uint8
	StableMints          []string
	KaminoLiquidityMints []string
	KaminoMarkets        []string
}

// withdrawProductAsset mirrors EarnProductAsset (earn-product-mints.shared.ts:21).
// The token program follows the stablecoin, not the cluster
// (loyal-actions/src/constants.ts:253): CASH, USDG and PYUSD are Token-2022.
type withdrawProductAsset struct {
	Mint         solana.PublicKey
	Symbol       string
	TokenProgram solana.PublicKey
}

var (
	withdrawDevnetUSDCMint = solana.MustPublicKeyFromBase58("4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU")
	withdrawTokenProgram   = solana.TokenProgramID
	withdrawToken2022      = solana.Token2022ProgramID
)

// withdrawProductAssets mirrors getEarnProductAssetsForCluster
// (earn-product-mints.shared.ts:29): the six canonical stablecoins in policy
// order on mainnet, the devnet USDC mint only elsewhere.
func withdrawProductAssets(cluster string) []withdrawProductAsset {
	if cluster == "devnet" {
		return []withdrawProductAsset{{Mint: withdrawDevnetUSDCMint, Symbol: "USDC", TokenProgram: withdrawTokenProgram}}
	}
	return []withdrawProductAsset{
		{Mint: solana.MustPublicKeyFromBase58("CASHx9KJUStyftLFWGvEVf59SGeG9sh5FfcnZMVPCASH"), Symbol: "CASH", TokenProgram: withdrawToken2022},
		{Mint: solana.MustPublicKeyFromBase58("2u1tszSeqZ3qBWF3uNGPFc8TzMk2tdiwknnRMWGWjGWH"), Symbol: "USDG", TokenProgram: withdrawToken2022},
		{Mint: solana.MustPublicKeyFromBase58("2b1kV6DkPAnxd5ixfnxCpjxmKwqjjaYmCZfHsFu24GXo"), Symbol: "PYUSD", TokenProgram: withdrawToken2022},
		{Mint: solana.MustPublicKeyFromBase58("EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"), Symbol: "USDC", TokenProgram: withdrawTokenProgram},
		{Mint: solana.MustPublicKeyFromBase58("Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB"), Symbol: "USDT", TokenProgram: withdrawTokenProgram},
		{Mint: solana.MustPublicKeyFromBase58("USDSwr9ApdHk5bvJKMjzff41FfuX8bSxdKcR81vTwcA"), Symbol: "USDS", TokenProgram: withdrawTokenProgram},
	}
}

// withdrawSafeMarkets mirrors getRiskBasketMarketsForCluster(cluster, Safe)
// (loyal-actions/src/constants.ts:97-134).
func withdrawSafeMarkets(cluster string) []solana.PublicKey {
	if cluster == "devnet" {
		return []solana.PublicKey{solana.MustPublicKeyFromBase58("27MKCQo5qP7ijrwWSMKX2Jeb3PhK2NZmHQ9befWVRS4J")}
	}
	return []solana.PublicKey{
		solana.MustPublicKeyFromBase58("7u3HeHxYDLhnCoErrtycNokbQYbWGzLs6JSDqGAv5PfF"),
		solana.MustPublicKeyFromBase58("CqAoLuqWtavaVE8deBjMKe8ZfSt9ghR6Vb8nfsyabyHA"),
		solana.MustPublicKeyFromBase58("6WEGfej9B9wjxRs6t4BYpb9iCXd8CpTpJ8fVSNzHCC5y"),
		solana.MustPublicKeyFromBase58("47tfyEG9SsdEnUm9cw5kY9BXngQGqu3LBoop9j5uTAv8"),
		solana.MustPublicKeyFromBase58("BJnbcRHqvppTyGesLzWASGKnmnF1wq9jZu6ExrjT7wvF"),
	}
}

// assertWithdrawPolicyUniverse mirrors assertPolicyUniverse
// (earn-rpc-holdings.client.ts:188): the product assets the policy lists in
// both its stable mints and its Kamino liquidity mints, and the policy's
// markets that are in the Safe basket, in policy order.
func assertWithdrawPolicyUniverse(cluster string, policy *WithdrawPolicyUniverse) ([]withdrawProductAsset, []solana.PublicKey, error) {
	if policy == nil {
		return nil, nil, errors.New("Active Earn policy metadata is required for RPC holdings.")
	}
	if policy.VaultIndex != WithdrawVaultIndex {
		return nil, nil, errors.New("Active Earn policy is not for the Earn vault.")
	}
	var assets []withdrawProductAsset
	for _, asset := range withdrawProductAssets(cluster) {
		mint := asset.Mint.String()
		if slices.Contains(policy.StableMints, mint) && slices.Contains(policy.KaminoLiquidityMints, mint) {
			assets = append(assets, asset)
		}
	}
	if len(assets) == 0 {
		return nil, nil, errors.New("Active Earn policy includes no supported product mint.")
	}
	var markets []solana.PublicKey
	for _, market := range policy.KaminoMarkets {
		key, err := solana.PublicKeyFromBase58(market)
		if err != nil {
			return nil, nil, err
		}
		if slices.ContainsFunc(withdrawSafeMarkets(cluster), func(safe solana.PublicKey) bool { return safe == key }) {
			markets = append(markets, key)
		}
	}
	if len(markets) == 0 {
		return nil, nil, errors.New("Active Earn policy has no Safe Kamino markets.")
	}
	return assets, markets, nil
}

// withdrawPositionDisplay mirrors resolveEarnPositionDisplay
// (earn-position-display.ts:81): known Safe-basket market names, known mint
// symbols, shortened addresses otherwise.
func withdrawPositionDisplay(liquidityMint string, market *solana.PublicKey) (label, marketName, mintSymbol string) {
	if market == nil {
		marketName = "Unknown Market"
	} else {
		marketName = withdrawKnownMarketName(market.String())
	}
	mintSymbol = withdrawKnownMintSymbol(liquidityMint)
	return marketName + " · " + mintSymbol, marketName, mintSymbol
}

func withdrawKnownMarketName(market string) string {
	switch market {
	case "7u3HeHxYDLhnCoErrtycNokbQYbWGzLs6JSDqGAv5PfF", "27MKCQo5qP7ijrwWSMKX2Jeb3PhK2NZmHQ9befWVRS4J":
		return "Main Kamino"
	case "CqAoLuqWtavaVE8deBjMKe8ZfSt9ghR6Vb8nfsyabyHA":
		return "Prime Market"
	case "6WEGfej9B9wjxRs6t4BYpb9iCXd8CpTpJ8fVSNzHCC5y":
		return "Maple Market"
	case "47tfyEG9SsdEnUm9cw5kY9BXngQGqu3LBoop9j5uTAv8":
		return "OnRe Market"
	case "BJnbcRHqvppTyGesLzWASGKnmnF1wq9jZu6ExrjT7wvF":
		return "Ethena Market"
	}
	return withdrawShortenAddress(market)
}

func withdrawKnownMintSymbol(mint string) string {
	for _, asset := range withdrawProductAssets("mainnet-beta") {
		if asset.Mint.String() == mint {
			return asset.Symbol
		}
	}
	if mint == withdrawDevnetUSDCMint.String() {
		return "USDC"
	}
	return withdrawShortenAddress(mint)
}

// withdrawShortenAddress mirrors shortenAddress
// (earn-position-display.ts:73): ten characters or fewer stay whole, anything
// longer is head 4 + "..." + tail 4.
func withdrawShortenAddress(address string) string {
	if len(address) <= 10 {
		return address
	}
	return address[:4] + "..." + address[len(address)-4:]
}

// withdrawObservedAt formats like Date.prototype.toISOString: millisecond
// precision with a Z suffix.
func withdrawObservedAt(now func() time.Time) string {
	if now == nil {
		now = time.Now
	}
	return now().UTC().Format("2006-01-02T15:04:05.000Z")
}

// WithdrawHolding mirrors EarnRpcHolding (earn-rpc-holdings.client.ts:68): one
// withdrawable position, the amount in raw integer units. Market, reserve and
// the keys stay public keys here; the route serializes them as base58.
type WithdrawHolding struct {
	AmountRaw     uint64
	Kind          string // "idle" or "kamino"
	Label         string
	LiquidityMint solana.PublicKey
	Market        *solana.PublicKey
	MarketName    string
	Provenance    map[string]string
	Reserve       *solana.PublicKey
	SourceID      string
	SupplyApyBps  *string // always nil: the read never prices a position
	TokenProgram  solana.PublicKey
}

// WithdrawWatchedAccount mirrors EarnRpcWatchedAccount (:84).
type WithdrawWatchedAccount struct {
	Kind   string // "idle", "obligation" or "reserve"
	Pubkey string
}

// WithdrawSnapshotProvenance mirrors the provenance block (:96-102).
type WithdrawSnapshotProvenance struct {
	AccountCount    int
	ChunkCount      int
	Commitment      string
	Source          string
	WatchedAccounts []WithdrawWatchedAccount
}

// WithdrawSnapshot mirrors EarnRpcHoldingsSnapshot (:89-103).
type WithdrawSnapshot struct {
	Completeness          string // always "complete"
	CurrentTotalAmountRaw uint64
	Holdings              []WithdrawHolding
	ObservedAt            string
	ObservedSlot          uint64
	Provenance            WithdrawSnapshotProvenance
}

// WithdrawHoldingsArgs carries the read's inputs
// (fetchEarnRpcHoldingsSnapshot's args, :422-439).
type WithdrawHoldingsArgs struct {
	Cluster string
	// MinContextSlot, when set, demands a node at or past this slot on every
	// underlying read, so no stage answers with pre-deposit state (:425-430).
	MinContextSlot uint64
	Now            func() time.Time
	Policy         *WithdrawPolicyUniverse
	SettingsPda    solana.PublicKey
	// RequireCompleteReserveReads fails closed when a reserve behind a
	// positive obligation deposit is unreadable (:433-436).
	RequireCompleteReserveReads bool
}

type withdrawAccountRole struct {
	kind   string // "idle" or "obligation"
	asset  withdrawProductAsset
	market solana.PublicKey
	pubkey solana.PublicKey
}

type withdrawDiscoveredDeposit struct {
	collateralAmountRaw uint64
	market              solana.PublicKey
	obligation          solana.PublicKey
	reserve             solana.PublicKey
	slotIndex           int
}

// FetchWithdrawHoldingsSnapshot mirrors fetchEarnRpcHoldingsSnapshot
// (:422-662). The vault is the settings' account at WithdrawVaultIndex.
func FetchWithdrawHoldingsSnapshot(ctx context.Context, rpc RPC, args WithdrawHoldingsArgs) (WithdrawSnapshot, error) {
	assets, markets, err := assertWithdrawPolicyUniverse(args.Cluster, args.Policy)
	if err != nil {
		return WithdrawSnapshot{}, err
	}
	vaultPda, _, err := squads.SmartAccountAddress(args.SettingsPda, WithdrawVaultIndex)
	if err != nil {
		return WithdrawSnapshot{}, err
	}
	// Stage 1 (:454-478): the idle ATA per asset, then the vanilla obligation
	// per allowed market. The KLend program is the same address on both
	// clusters (constants.ts:207-212).
	firstStageRoles := make([]withdrawAccountRole, 0, len(assets)+len(markets))
	for _, asset := range assets {
		ata, err := spl.AssociatedTokenAddress(vaultPda, asset.Mint, asset.TokenProgram)
		if err != nil {
			return WithdrawSnapshot{}, err
		}
		firstStageRoles = append(firstStageRoles, withdrawAccountRole{kind: "idle", asset: asset, pubkey: ata})
	}
	for _, market := range markets {
		obligation, err := kamino.VanillaObligation(vaultPda, market)
		if err != nil {
			return WithdrawSnapshot{}, err
		}
		firstStageRoles = append(firstStageRoles, withdrawAccountRole{kind: "obligation", market: market, pubkey: obligation})
	}
	firstKeys := make([]solana.PublicKey, len(firstStageRoles))
	for i, role := range firstStageRoles {
		firstKeys[i] = role.pubkey
	}
	firstStage, err := readWithdrawAccountsInChunks(ctx, rpc, firstKeys, args.MinContextSlot)
	if err != nil {
		return WithdrawSnapshot{}, err
	}
	var deposits []withdrawDiscoveredDeposit
	for index, role := range firstStageRoles {
		if role.kind != "obligation" {
			continue
		}
		account := firstStage.valueAt(index)
		if account == nil {
			continue
		}
		if account.Owner != kamino.ProgramID {
			return WithdrawSnapshot{}, errors.New("Kamino obligation account has an unexpected owner.")
		}
		parsed, err := ParseWithdrawObligation(account.Data)
		if err != nil {
			return WithdrawSnapshot{}, err
		}
		if parsed.Owner != vaultPda {
			return WithdrawSnapshot{}, errors.New("Kamino obligation owner is not the Earn vault.")
		}
		if parsed.LendingMarket != role.market {
			return WithdrawSnapshot{}, errors.New("Kamino obligation lending market mismatch.")
		}
		for _, deposit := range parsed.Deposits {
			deposits = append(deposits, withdrawDiscoveredDeposit{
				collateralAmountRaw: deposit.DepositedAmountRaw,
				market:              parsed.LendingMarket,
				obligation:          role.pubkey,
				reserve:             deposit.Reserve,
				slotIndex:           deposit.SlotIndex,
			})
		}
	}
	// Stage 2 (:516-535): one read per discovered reserve, in discovery order;
	// with no deposits the stage reuses stage 1's observed slot.
	reserveKeys := make([]solana.PublicKey, 0, len(deposits))
	for _, deposit := range deposits {
		reserveKeys = append(reserveKeys, deposit.reserve)
	}
	reserveStage := withdrawBatchedRead{MaxObservedSlot: firstStage.MaxObservedSlot}
	if len(reserveKeys) > 0 {
		reserveStage, err = readWithdrawAccountsInChunks(ctx, rpc, reserveKeys, args.MinContextSlot)
		if err != nil {
			return WithdrawSnapshot{}, err
		}
	}
	type reconciledDeposit struct {
		withdrawDiscoveredDeposit
		asset          withdrawProductAsset
		liquidityMint  solana.PublicKey
		reserveAccount *chain.Account
		collateralMint solana.PublicKey
	}
	var candidates []reconciledDeposit
	for index, deposit := range deposits {
		if !slices.ContainsFunc(markets, func(allowed solana.PublicKey) bool { return allowed == deposit.market }) {
			return WithdrawSnapshot{}, errors.New("Kamino obligation deposit is outside the Safe policy.")
		}
		account := reserveStage.valueAt(index)
		if account == nil {
			if args.RequireCompleteReserveReads && deposit.collateralAmountRaw > 0 {
				return WithdrawSnapshot{}, errors.New("Kamino reserve account is unavailable for a positive Earn obligation.")
			}
			continue
		}
		if account.Owner != kamino.ProgramID {
			return WithdrawSnapshot{}, errors.New("Kamino reserve account has an unexpected owner.")
		}
		accounts, err := ParseWithdrawReserveTokenAccounts(account.Data)
		if err != nil {
			return WithdrawSnapshot{}, err
		}
		if accounts.LendingMarket != deposit.market {
			return WithdrawSnapshot{}, errors.New("Kamino reserve lending market mismatch.")
		}
		assetIndex := slices.IndexFunc(assets, func(asset withdrawProductAsset) bool { return asset.Mint == accounts.ReserveLiquidityMint })
		if assetIndex < 0 {
			return WithdrawSnapshot{}, errors.New("Kamino reserve liquidity mint is outside the policy.")
		}
		if accounts.ReserveLiquidityTokenProgram != assets[assetIndex].TokenProgram {
			return WithdrawSnapshot{}, errors.New("Kamino reserve liquidity token program mismatch.")
		}
		candidates = append(candidates, reconciledDeposit{
			withdrawDiscoveredDeposit: deposit,
			asset:                     assets[assetIndex],
			liquidityMint:             accounts.ReserveLiquidityMint,
			reserveAccount:            account,
			collateralMint:            accounts.ReserveCollateralMint,
		})
	}
	observedSlot := max(firstStage.MaxObservedSlot, reserveStage.MaxObservedSlot)
	observedAt := withdrawObservedAt(args.Now)
	// Holdings (:606-644): Kamino positions first, then one idle holding per
	// asset even when it holds nothing.
	holdings := make([]WithdrawHolding, 0, len(candidates)+len(assets))
	for _, candidate := range candidates {
		snapshot, err := ParseWithdrawReserveSnapshot(candidate.reserveAccount.Data)
		if err != nil {
			return WithdrawSnapshot{}, err
		}
		amountRaw, err := WithdrawRedeemableLiquidityRaw(candidate.collateralAmountRaw, snapshot)
		if err != nil {
			return WithdrawSnapshot{}, err
		}
		if amountRaw == 0 {
			continue
		}
		holdings = append(holdings, withdrawKaminoHolding(amountRaw, candidate.asset, candidate.collateralAmountRaw,
			candidate.collateralMint, candidate.liquidityMint, candidate.market, candidate.obligation,
			candidate.reserve, observedAt, observedSlot, candidate.slotIndex))
	}
	for index, role := range firstStageRoles {
		if role.kind != "idle" {
			continue
		}
		amountRaw, err := withdrawIdleTokenAmount(firstStage.valueAt(index), role.asset, vaultPda)
		if err != nil {
			return WithdrawSnapshot{}, err
		}
		holdings = append(holdings, withdrawIdleHolding(amountRaw, role.asset, observedAt, observedSlot,
			firstStage.MaxObservedSlot, vaultPda, role.pubkey))
	}
	var total uint64
	for _, holding := range holdings {
		total += holding.AmountRaw
	}
	return WithdrawSnapshot{
		Completeness:          "complete",
		CurrentTotalAmountRaw: total,
		Holdings:              holdings,
		ObservedAt:            observedAt,
		ObservedSlot:          observedSlot,
		Provenance: WithdrawSnapshotProvenance{
			AccountCount:    firstStage.AccountCount + reserveStage.AccountCount,
			ChunkCount:      firstStage.ChunkCount + reserveStage.ChunkCount,
			Commitment:      "confirmed",
			Source:          "rpc_getMultipleAccounts",
			WatchedAccounts: withdrawWatchedAccounts(firstStageRoles, reserveKeys),
		},
	}, nil
}

// withdrawKaminoHolding mirrors toKaminoHolding (:347); the caller drops zero
// amounts (:354).
func withdrawKaminoHolding(amountRaw uint64, asset withdrawProductAsset, collateralAmountRaw uint64,
	collateralMint, liquidityMint, market, obligation, reserve solana.PublicKey,
	observedAt string, observedSlot uint64, slotIndex int) WithdrawHolding {
	label, marketName, _ := withdrawPositionDisplay(liquidityMint.String(), &market)
	return WithdrawHolding{
		AmountRaw:     amountRaw,
		Kind:          "kamino",
		Label:         label,
		LiquidityMint: liquidityMint,
		Market:        &market,
		MarketName:    marketName,
		Provenance: map[string]string{
			"amountSemantics":               "kamino_redeemable_liquidity",
			"obligation":                    obligation.String(),
			"obligationCollateralAmountRaw": strconv.FormatUint(collateralAmountRaw, 10),
			"reserveCollateralMint":         collateralMint.String(),
			"slotIndex":                     strconv.Itoa(slotIndex),
			"source":                        "rpc_getMultipleAccounts",
			"sourceCommitment":              "confirmed",
			"sourceSlot":                    strconv.FormatUint(observedSlot, 10),
		},
		Reserve:      &reserve,
		SourceID:     "reserve:" + reserve.String(),
		SupplyApyBps: nil,
		TokenProgram: asset.TokenProgram,
	}
}

// withdrawIdleHolding mirrors toIdleHolding (:390): one holding per product
// asset, present even at zero.
func withdrawIdleHolding(amountRaw uint64, asset withdrawProductAsset, observedAt string,
	observedSlot, sourceSlot uint64, vaultPda, tokenAccount solana.PublicKey) WithdrawHolding {
	return WithdrawHolding{
		AmountRaw:     amountRaw,
		Kind:          "idle",
		Label:         "Idle Balance",
		LiquidityMint: asset.Mint,
		Market:        nil,
		MarketName:    asset.Symbol,
		Provenance: map[string]string{
			"owner":            vaultPda.String(),
			"source":           "rpc_getMultipleAccounts",
			"sourceCommitment": "confirmed",
			"sourceSlot":       strconv.FormatUint(sourceSlot, 10),
			"tokenAccount":     tokenAccount.String(),
		},
		Reserve:      nil,
		SourceID:     "idle:" + tokenAccount.String(),
		SupplyApyBps: nil,
		TokenProgram: asset.TokenProgram,
	}
}

// withdrawIdleTokenAmount mirrors validateTokenAccountAmount (:274): an
// absent account counts as zero; a present one must be the asset's token
// program, carry the asset's mint, and be owned by the vault.
func withdrawIdleTokenAmount(account *chain.Account, asset withdrawProductAsset, vaultPda solana.PublicKey) (uint64, error) {
	if account == nil {
		return 0, nil
	}
	label := "Earn vault " + asset.Symbol + " ATA"
	if account.Owner != asset.TokenProgram {
		return 0, fmt.Errorf("%s is not owned by the token program.", label)
	}
	if len(account.Data) < 72 {
		return 0, fmt.Errorf("%s is shorter than a token account.", label)
	}
	if solana.PublicKeyFromBytes(account.Data[0:32]) != asset.Mint {
		return 0, fmt.Errorf("%s has an unexpected mint.", label)
	}
	if solana.PublicKeyFromBytes(account.Data[32:64]) != vaultPda {
		return 0, fmt.Errorf("%s is not owned by the Earn vault.", label)
	}
	return binary.LittleEndian.Uint64(account.Data[64:72]), nil
}

// withdrawBatchedRead mirrors BatchedAccountRead (:107).
type withdrawBatchedRead struct {
	AccountCount    int
	ChunkCount      int
	MaxObservedSlot uint64
	Values          []*chain.Account
}

func (r withdrawBatchedRead) valueAt(index int) *chain.Account {
	if index < len(r.Values) {
		return r.Values[index]
	}
	return nil
}

// readWithdrawAccountsInChunks mirrors readAccountsInChunks (:234): chunks of
// 100 at confirmed commitment, the max context slot across calls kept.
func readWithdrawAccountsInChunks(ctx context.Context, rpc RPC, keys []solana.PublicKey, minContextSlot uint64) (withdrawBatchedRead, error) {
	read := withdrawBatchedRead{AccountCount: len(keys)}
	for start := 0; start < len(keys); start += withdrawGetMultipleAccountsLimit {
		chunk := keys[start:min(start+withdrawGetMultipleAccountsLimit, len(keys))]
		slot, accounts, err := rpc.Accounts(ctx, chunk, withdrawCommitment, minContextSlot)
		if err != nil {
			return withdrawBatchedRead{}, err
		}
		read.ChunkCount++
		read.MaxObservedSlot = max(read.MaxObservedSlot, slot)
		read.Values = append(read.Values, accounts...)
	}
	return read, nil
}

// withdrawWatchedAccounts mirrors toWatchedAccounts (:328): first-seen order,
// deduped by kind and pubkey.
func withdrawWatchedAccounts(roles []withdrawAccountRole, reserves []solana.PublicKey) []WithdrawWatchedAccount {
	seen := map[string]bool{}
	var accounts []WithdrawWatchedAccount
	add := func(kind string, pubkey solana.PublicKey) {
		key := kind + ":" + pubkey.String()
		if seen[key] {
			return
		}
		seen[key] = true
		accounts = append(accounts, WithdrawWatchedAccount{Kind: kind, Pubkey: pubkey.String()})
	}
	for _, role := range roles {
		add(role.kind, role.pubkey)
	}
	for _, reserve := range reserves {
		add("reserve", reserve)
	}
	return accounts
}
