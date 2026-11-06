package earnmax

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/spl"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/voltr"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/releasecontrol"
)

// Deposit refusal reasons are API contract names.
const (
	DepositNAVUncertified        = "nav_uncertified"
	DepositWaitingNAVPublication = "deposit_waiting_nav_publication"
)

type DepositRefusal struct{ Reason string }

func (r *DepositRefusal) Error() string { return r.Reason }

type VaultPin struct {
	Offset int    `json:"offset"`
	Hex    string `json:"hex"`
}

// depositPinnedRanges are the vault bytes the LP bounds depend on; the client
// refuses a quote that leaves any of its VOLTR_DEPOSIT_PRICING_RANGES unpinned.
var depositPinnedRanges = [][2]int{{168, 176}, {448, 456}, {512, 528}, {568, 600}, {616, 640}, {672, 688}}

type DepositQuote struct {
	Vault         string     `json:"vault"`
	LPMint        string     `json:"lpMint"`
	Authority     string     `json:"authority"`
	LPATA         string     `json:"lpAta"`
	AmountRaw     string     `json:"amountRaw"`
	CertifiedSlot string     `json:"certifiedSlot"`
	NotAfterSlot  string     `json:"notAfterSlot"`
	ExpiresAt     string     `json:"expiresAt"`
	VaultPins     []VaultPin `json:"vaultPins"`
	LPSupplyRaw   string     `json:"lpSupplyRaw"`
	LPBaselineRaw string     `json:"lpBaselineRaw"`
	LPMinRaw      string     `json:"lpMinRaw"`
	LPMaxRaw      string     `json:"lpMaxRaw"`
	SupplyMaxRaw  string     `json:"supplyMaxRaw"`
	CertificateID string     `json:"certificateId"`
	expiresUnix   int64
}

type DepositQuoter struct {
	db    WithdrawalConsentDB
	chain *chain.Client
}

func NewDepositQuoter(db WithdrawalConsentDB, client *chain.Client) *DepositQuoter {
	return &DepositQuoter{db: db, chain: client}
}

// Quote certifies LP bounds for one deposit. The release control, the
// worker's certified NAV and its last confirmed publication are read in one
// transaction, and the issued quote is stored under that control version.
func (q *DepositQuoter) Quote(ctx context.Context, authority solana.PublicKey, amountRaw uint64) (DepositQuote, error) {
	if q == nil || q.db == nil || q.chain == nil {
		return DepositQuote{}, errors.New("deposit quote unavailable")
	}
	if amountRaw == 0 || amountRaw > 1<<63-1 {
		return DepositQuote{}, errors.New("deposit amount must be a positive raw integer")
	}
	lpATA, err := spl.AssociatedTokenAddress(authority, LPMint, solana.TokenProgramID)
	if err != nil {
		return DepositQuote{}, err
	}
	slot, accounts, err := q.chain.Accounts(ctx, []solana.PublicKey{Vault, LPMint, AssetMint, lpATA, solana.SysVarClockPubkey}, rpc.CommitmentConfirmed, 0)
	if err != nil {
		return DepositQuote{}, err
	}
	if accounts[0] == nil || accounts[1] == nil || accounts[2] == nil || accounts[4] == nil || len(accounts[1].Data) < 82 || len(accounts[2].Data) < 82 || len(accounts[4].Data) < 40 {
		return DepositQuote{}, errors.New("deposit accounts unavailable")
	}
	vault, err := voltr.DecodeVault(accounts[0])
	if err != nil {
		return DepositQuote{}, err
	}
	if vault.LPMint != LPMint || vault.AssetMint != AssetMint {
		return DepositQuote{}, errors.New("deposit vault mints changed")
	}
	supply := binary.LittleEndian.Uint64(accounts[1].Data[36:44])
	lpDecimals, assetDecimals := accounts[1].Data[44], accounts[2].Data[44]
	baseline := uint64(0)
	if a := accounts[3]; a != nil {
		if a.Owner != solana.TokenProgramID || len(a.Data) < 72 || solana.PublicKeyFromBytes(a.Data[0:32]) != LPMint || solana.PublicKeyFromBytes(a.Data[32:64]) != authority {
			return DepositQuote{}, errors.New("deposit LP account is not the owner's LP token account")
		}
		baseline = binary.LittleEndian.Uint64(a.Data[64:72])
	}
	chainNow := time.Unix(int64(binary.LittleEndian.Uint64(accounts[4].Data[32:40])), 0)

	tx, err := q.db.Begin(ctx)
	if err != nil {
		return DepositQuote{}, err
	}
	defer tx.Rollback(ctx)
	admission, err := releasecontrol.Check(ctx, tx, Vault.String(), releasecontrol.OfficialDeposit, []string{authority.String()}, &amountRaw)
	if err != nil {
		return DepositQuote{}, err
	}
	nav, err := readCertifiedVaultNAV(ctx, tx)
	if err != nil {
		return DepositQuote{}, err
	}
	if !nav.depositValidAt(time.Now(), slot) {
		return DepositQuote{}, &DepositRefusal{DepositNAVUncertified}
	}
	var lockedProfit, lastReport string
	err = tx.QueryRow(ctx, `SELECT locked_profit_raw::text,last_report_unix::text FROM loyal_yield.backyard_confirmed_nav_publications WHERE vault=$1`, Vault.String()).Scan(&lockedProfit, &lastReport)
	if errors.Is(err, pgx.ErrNoRows) {
		return DepositQuote{}, &DepositRefusal{DepositWaitingNAVPublication}
	}
	if err != nil {
		return DepositQuote{}, err
	}
	if lockedProfit != strconv.FormatUint(vault.LastUpdatedLockedProfitRaw, 10) || lastReport != strconv.FormatUint(vault.LastLockedProfitReportUnix, 10) || !depositPricePublished(nav.totalRaw, vault) {
		return DepositQuote{}, &DepositRefusal{DepositWaitingNAVPublication}
	}
	quote, err := newDepositQuote(depositState{Authority: authority, LPATA: lpATA, VaultData: accounts[0].Data, Vault: vault, SupplyRaw: supply, AssetDecimals: assetDecimals, LPDecimals: lpDecimals, BaselineRaw: baseline}, amountRaw, chainNow, nav.validUntil, nav.certifiedSlot, nav.maxSlot)
	if err != nil {
		return DepositQuote{}, err
	}
	expires := time.Unix(quote.expiresUnix, 0)
	body, err := json.Marshal(quote)
	if err != nil {
		return DepositQuote{}, err
	}
	sum := sha256.Sum256(append([]byte(fmt.Sprintf("earn-max-deposit-quote/v1|%d|", admission.Version)), body...))
	quote.CertificateID = hex.EncodeToString(sum[:])
	stored, err := json.Marshal(quote)
	if err != nil {
		return DepositQuote{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO loyal_yield.earn_max_deposit_quotes(certificate_id,vault,authority,amount_raw,lp_min_raw,lp_max_raw,certified_slot,not_after_slot,release_control_version,expires_at,quote) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) ON CONFLICT (certificate_id) DO NOTHING`, quote.CertificateID, quote.Vault, quote.Authority, quote.AmountRaw, quote.LPMinRaw, quote.LPMaxRaw, nav.certifiedSlot, nav.maxSlot, admission.Version, expires, stored); err != nil {
		return DepositQuote{}, err
	}
	return quote, tx.Commit(ctx)
}

type depositState struct {
	Authority, LPATA          solana.PublicKey
	VaultData                 []byte
	Vault                     voltr.Vault
	SupplyRaw, BaselineRaw    uint64
	AssetDecimals, LPDecimals uint8
}

// newDepositQuote prices one deposit against the observed vault state. Fee LP
// accrues with time and raises the LP each asset unit buys, so the mint is
// smallest at the observed chain time and largest when the quote expires.
func newDepositQuote(st depositState, amountRaw uint64, chainNow, validUntil time.Time, certifiedSlot, maxSlot int64) (DepositQuote, error) {
	if len(st.VaultData) < depositPinnedRanges[len(depositPinnedRanges)-1][1] {
		return DepositQuote{}, errors.New("deposit vault data too short")
	}
	pins := make([]VaultPin, len(depositPinnedRanges))
	for i, r := range depositPinnedRanges {
		pins[i] = VaultPin{Offset: r[0], Hex: hex.EncodeToString(st.VaultData[r[0]:r[1]])}
	}
	expires := validUntil.Truncate(time.Second)
	lpMin, err := st.Vault.DepositLP(st.SupplyRaw, st.AssetDecimals, st.LPDecimals, amountRaw, chainNow)
	if err != nil {
		return DepositQuote{}, err
	}
	lpMax, err := st.Vault.DepositLP(st.SupplyRaw, st.AssetDecimals, st.LPDecimals, amountRaw, chainNow.Add(max(expires.Sub(chainNow), 0)+time.Second))
	if err != nil {
		return DepositQuote{}, err
	}
	if st.SupplyRaw > ^uint64(0)-lpMax || st.BaselineRaw > ^uint64(0)-lpMax {
		return DepositQuote{}, errors.New("deposit LP bounds overflow")
	}
	return DepositQuote{
		Vault: Vault.String(), LPMint: LPMint.String(), Authority: st.Authority.String(), LPATA: st.LPATA.String(),
		AmountRaw: strconv.FormatUint(amountRaw, 10), CertifiedSlot: strconv.FormatInt(certifiedSlot, 10), NotAfterSlot: strconv.FormatInt(maxSlot, 10),
		ExpiresAt: strconv.FormatInt(expires.Unix(), 10), VaultPins: pins, LPSupplyRaw: strconv.FormatUint(st.SupplyRaw, 10), LPBaselineRaw: strconv.FormatUint(st.BaselineRaw, 10),
		LPMinRaw: strconv.FormatUint(lpMin, 10), LPMaxRaw: strconv.FormatUint(lpMax, 10), SupplyMaxRaw: strconv.FormatUint(st.SupplyRaw+lpMax, 10),
		expiresUnix: expires.Unix(),
	}, nil
}
