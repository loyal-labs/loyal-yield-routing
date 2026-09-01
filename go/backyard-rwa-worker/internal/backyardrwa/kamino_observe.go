package backyardrwa

// This file deliberately decodes only the frozen PRIME/USDC Kamino graph. It
// is not a general KLend decoder: an unfamiliar account layout or topology is
// an observation failure, not an opportunity to guess.

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"sort"
)

const (
	kaminoProgram             = "KLend2g3cP87fffoy8q1mQqGKjrxjC8boSyAYavgmjD"
	kaminoMarket              = "CqAoLuqWtavaVE8deBjMKe8ZfSt9ghR6Vb8nfsyabyHA"
	kaminoCollateralReserve   = "BUTND9T7Ux4KR8RAEgd4WoZwnP7xA279oA1y3iPVcvSh"
	kaminoDebtReserve         = "9GJ9GBRwCp4pHmWrQ43L5xpc9Vykg7jnfwcFGN8FoHYu"
	kaminoPrimeMint           = "3b8X44fLF9ooXaUm3hhSgjpmVs6rZZ3pPoGnGahc3Uu7"
	kaminoUSDCMint            = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
	kaminoObligationLength    = 3344
	kaminoReserveLength       = 8624
	kaminoRequiredPriceStatus = 0x3f
)

var (
	kaminoObligationDiscriminator = [8]byte{168, 206, 141, 106, 88, 76, 172, 167}
	kaminoReserveDiscriminator    = [8]byte{43, 242, 204, 202, 26, 247, 59, 127}
)

type KaminoObservationConfig struct {
	Program, Market, Obligation, CollateralReserve, DebtReserve string
	Vault, CollateralMint, DebtMint                             string
}

func pinnedKaminoObservationConfig() (KaminoObservationConfig, error) {
	// The new Squads-owned PRIME obligation does not exist yet. Never reuse the
	// older Maple obligation simply to make the graph appear complete.
	return KaminoObservationConfig{}, ErrKaminoTransactionConstructionUnavailable
}

type KaminoPosition struct {
	Slot, RefreshedSlot int64
	HasPosition         bool
	CollateralPriceSF   [16]byte
	DebtPriceSF         [16]byte
	Oracles             []string
}

// ObserveKaminoPrimeUSDC reads the obligation, both reserves, and every
// configured oracle at confirmed commitment. Two RPC batches are necessary
// because oracle identities are encoded in the reserve. Their context slots
// must therefore match exactly; retries advance minContextSlot rather than
// mixing a newer oracle with older reserve bytes.
func (c *RPCClient) ObserveKaminoPrimeUSDC(ctx context.Context) (KaminoPosition, error) {
	if c == nil {
		return KaminoPosition{}, fmt.Errorf("RPC client is required")
	}
	config, err := pinnedKaminoObservationConfig()
	if err != nil {
		return KaminoPosition{}, err
	}
	return c.observeKaminoPrimeUSDC(ctx, config)
}

func (c *RPCClient) observeKaminoPrimeUSDC(ctx context.Context, config KaminoObservationConfig) (KaminoPosition, error) {
	minSlot, err := c.ConfirmedSlot(ctx)
	if err != nil {
		return KaminoPosition{}, err
	}
	for attempt := 0; attempt < maxConfirmedObservationAttempts; attempt++ {
		baseSlot, accounts, err := c.GetMultipleAccounts(ctx, []string{config.Obligation, config.CollateralReserve, config.DebtReserve}, minSlot)
		if err != nil {
			return KaminoPosition{}, err
		}
		obligation, err := decodeKaminoObligation(accountAt(accounts, config.Obligation), config)
		if err != nil {
			return KaminoPosition{}, err
		}
		collateral, err := decodeKaminoReserve(accountAt(accounts, config.CollateralReserve), config.CollateralMint, config)
		if err != nil {
			return KaminoPosition{}, err
		}
		debt, err := decodeKaminoReserve(accountAt(accounts, config.DebtReserve), config.DebtMint, config)
		if err != nil {
			return KaminoPosition{}, err
		}
		if err := validateKaminoRefresh(obligation, collateral, debt); err != nil {
			return KaminoPosition{}, err
		}
		oracles := uniqueNonzero(append(collateral.oracles, debt.oracles...))
		if len(oracles) == 0 {
			return KaminoPosition{}, fmt.Errorf("Kamino reserve has no configured oracle")
		}
		oracleSlot, oracleAccounts, err := c.GetMultipleAccounts(ctx, oracles, baseSlot)
		if err != nil {
			return KaminoPosition{}, err
		}
		if oracleSlot != baseSlot {
			minSlot = maxSlot(baseSlot, oracleSlot)
			continue
		}
		for _, oracle := range oracleAccounts {
			if oracle.Executable || oracle.Lamports == 0 || len(oracle.Data) == 0 {
				return KaminoPosition{}, fmt.Errorf("invalid configured oracle %s", oracle.Address)
			}
		}
		return KaminoPosition{Slot: baseSlot, RefreshedSlot: obligation.refreshedSlot, HasPosition: obligation.hasPosition, CollateralPriceSF: collateral.marketPriceSF, DebtPriceSF: debt.marketPriceSF, Oracles: oracles}, nil
	}
	return KaminoPosition{}, fmt.Errorf("confirmed Kamino reserve and oracle reads did not align after %d attempts", maxConfirmedObservationAttempts)
}

type decodedKaminoObligation struct {
	refreshedSlot      int64
	stale, priceStatus byte
	hasPosition        bool
}
type decodedKaminoReserve struct {
	refreshedSlot      int64
	stale, priceStatus byte
	marketPriceSF      [16]byte
	oracles            []string
}

func decodeKaminoObligation(account ConfirmedAccount, c KaminoObservationConfig) (decodedKaminoObligation, error) {
	if err := kaminoEnvelope(account, c.Obligation, kaminoObligationLength, kaminoObligationDiscriminator, c.Program); err != nil {
		return decodedKaminoObligation{}, err
	}
	if !sameKey(account.Data[32:64], c.Market) || !sameKey(account.Data[64:96], c.Vault) {
		return decodedKaminoObligation{}, fmt.Errorf("Kamino obligation market or owner drifted")
	}
	deposits, borrows := 0, 0
	for i := 0; i < 8; i++ {
		off := 96 + i*136
		if !zeroKey(account.Data[off:off+32]) && binary.LittleEndian.Uint64(account.Data[off+32:off+40]) > 0 {
			if !sameKey(account.Data[off:off+32], c.CollateralReserve) {
				return decodedKaminoObligation{}, fmt.Errorf("unsupported Kamino collateral reserve")
			}
			deposits++
		}
	}
	for i := 0; i < 5; i++ {
		off := 1208 + i*200
		if !zeroKey(account.Data[off:off+32]) && !allZero(account.Data[off+88:off+104]) {
			if !sameKey(account.Data[off:off+32], c.DebtReserve) {
				return decodedKaminoObligation{}, fmt.Errorf("unsupported Kamino debt reserve")
			}
			borrows++
		}
	}
	flat := deposits == 0 && borrows == 0 && allZero(account.Data[1192:1208]) && allZero(account.Data[2224:2240])
	if !flat && (deposits != 1 || borrows != 1) {
		return decodedKaminoObligation{}, fmt.Errorf("Kamino obligation is intermediate or unsupported")
	}
	return decodedKaminoObligation{int64(binary.LittleEndian.Uint64(account.Data[16:24])), account.Data[24], account.Data[25], !flat}, nil
}

func decodeKaminoReserve(account ConfirmedAccount, mint string, c KaminoObservationConfig) (decodedKaminoReserve, error) {
	if err := kaminoEnvelope(account, account.Address, kaminoReserveLength, kaminoReserveDiscriminator, c.Program); err != nil {
		return decodedKaminoReserve{}, err
	}
	if binary.LittleEndian.Uint64(account.Data[8:16]) != 1 || !sameKey(account.Data[32:64], c.Market) || !sameKey(account.Data[128:160], mint) {
		return decodedKaminoReserve{}, fmt.Errorf("Kamino reserve identity drifted")
	}
	var price [16]byte
	copy(price[:], account.Data[248:264])
	// KLend ReserveConfig.tokenInfo begins at 5008 in the pinned 8624-byte
	// layout. These are Scope, Switchboard price/TWAP, and Pyth price fields.
	oracles := []string{keyString(account.Data[5088:5120]), keyString(account.Data[5136:5168]), keyString(account.Data[5168:5200]), keyString(account.Data[5200:5232])}
	return decodedKaminoReserve{int64(binary.LittleEndian.Uint64(account.Data[16:24])), account.Data[24], account.Data[25], price, oracles}, nil
}

func validateKaminoRefresh(o decodedKaminoObligation, reserves ...decodedKaminoReserve) error {
	if o.stale != 0 || o.priceStatus&kaminoRequiredPriceStatus != kaminoRequiredPriceStatus {
		return fmt.Errorf("Kamino obligation valuation is stale or invalid")
	}
	for _, r := range reserves {
		if r.stale != 0 || r.priceStatus&kaminoRequiredPriceStatus != kaminoRequiredPriceStatus || r.refreshedSlot != o.refreshedSlot {
			return fmt.Errorf("Kamino reserve valuation is stale, invalid, or incoherent")
		}
	}
	return nil
}

func kaminoEnvelope(a ConfirmedAccount, address string, length int, discriminator [8]byte, program string) error {
	if a.Address != address || a.Owner != program || a.Executable || a.Lamports == 0 || len(a.Data) != length || !bytes.Equal(a.Data[:8], discriminator[:]) {
		return fmt.Errorf("Kamino account envelope or layout drifted")
	}
	return nil
}
func sameKey(data []byte, address string) bool {
	want, err := decodeBase58PublicKey(address)
	return err == nil && bytes.Equal(data, want[:])
}
func zeroKey(data []byte) bool { return allZero(data) }
func keyString(data []byte) string {
	if zeroKey(data) {
		return ""
	}
	return encodeBase58(data)
}
func uniqueNonzero(values []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value != "" {
			seen[value] = struct{}{}
		}
	}
	for value := range seen {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}
func maxSlot(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
