package earnmax

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/solana-foundation/solana-go/v2"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/voltr"
)

const WithdrawalConsentDomain = "loyal:earn-max:withdrawal-consent:v1"

// RequestInstruction is the ordinal of the request in execution order, including
// inner instructions. A receipt PDA and its version byte are reusable identities.
type WithdrawalGeneration struct {
	RequestSignature   string `json:"requestSignature"`
	RequestInstruction uint32 `json:"requestInstruction"`
}

type WithdrawalConsent struct {
	WithdrawalGeneration
	Version            string `json:"version"`
	PolicySeed         string `json:"policySeed"`
	Owner              string `json:"owner"`
	Cluster            string `json:"cluster"`
	Program            string `json:"program"`
	Vault              string `json:"vault"`
	Settings           string `json:"settings"`
	Authority          string `json:"authority"`
	Destination        string `json:"destination"`
	LPRaw              string `json:"lpRaw"`
	ReferenceUSDC      string `json:"referenceUsdcRaw"`
	ReferenceSlot      string `json:"referenceSlot"`
	FeeBasisHash       string `json:"feeBasisHash"`
	MinClaimUSDC       string `json:"minClaimUsdcRaw"`
	MaxUserFeeLamports string `json:"maxUserFeeLamports"`
	ExpiresAt          string `json:"expiresAt"`
	Nonce              string `json:"nonce"`
	OwnerSignature     string `json:"ownerSignature"`
}

func canonicalUint(raw string) (uint64, error) {
	n, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || strconv.FormatUint(n, 10) != raw {
		return 0, errors.New("noncanonical unsigned integer")
	}
	return n, nil
}

// CanonicalMessage uses a JSON array of strings, in this fixed order, with no
// whitespace. This byte sequence is the owner-signed contract, not arbitrary JSON.
func (c WithdrawalConsent) CanonicalMessage() ([]byte, error) {
	return json.Marshal([]string{WithdrawalConsentDomain, c.Cluster, c.Program, c.Vault,
		c.Settings, c.Owner, c.Authority, c.Destination, c.RequestSignature,
		strconv.FormatUint(uint64(c.RequestInstruction), 10), c.PolicySeed, c.LPRaw, c.ReferenceUSDC,
		c.ReferenceSlot, c.FeeBasisHash, c.MinClaimUSDC, c.MaxUserFeeLamports,
		c.Version, c.Nonce, c.ExpiresAt})
}

func (c WithdrawalConsent) Digest() (string, error) {
	message, err := c.CanonicalMessage()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(message)
	return hex.EncodeToString(sum[:]), nil
}

func (c WithdrawalConsent) Validate(owner, settings solana.PublicKey, now time.Time) error {
	authority, err := AuthorityOf(settings)
	if err != nil {
		return err
	}
	if c.Cluster != Cluster || c.Program != voltr.ProgramID.String() || c.Vault != Vault.String() ||
		c.Owner != owner.String() || c.Destination != owner.String() || c.Settings != settings.String() || c.Authority != authority.Key().String() {
		return errors.New("withdrawal consent does not match the authenticated account")
	}
	if _, err := solana.SignatureFromBase58(c.RequestSignature); err != nil {
		return errors.New("invalid request signature")
	}
	for _, raw := range []string{c.Version, c.PolicySeed, c.LPRaw, c.ReferenceUSDC, c.ReferenceSlot, c.MinClaimUSDC, c.MaxUserFeeLamports, c.ExpiresAt} {
		if _, err := canonicalUint(raw); err != nil {
			return err
		}
	}
	version, _ := canonicalUint(c.Version)
	lp, _ := canonicalUint(c.LPRaw)
	reference, _ := canonicalUint(c.ReferenceUSDC)
	floor, _ := canonicalUint(c.MinClaimUSDC)
	expiry, _ := canonicalUint(c.ExpiresAt)
	if version == 0 || lp == 0 || reference == 0 || floor == 0 || floor > reference || expiry > uint64(^uint64(0)>>1) || int64(expiry) <= now.Unix() {
		return errors.New("invalid or expired withdrawal consent")
	}
	hash, err := hex.DecodeString(c.FeeBasisHash)
	if err != nil || len(hash) != sha256.Size || hex.EncodeToString(hash) != c.FeeBasisHash {
		return errors.New("invalid fee basis hash")
	}
	nonce, err := hex.DecodeString(c.Nonce)
	if err != nil || len(nonce) != 32 || hex.EncodeToString(nonce) != c.Nonce {
		return errors.New("invalid consent nonce")
	}
	signature, err := base64.StdEncoding.Strict().DecodeString(c.OwnerSignature)
	if err != nil || len(signature) != ed25519.SignatureSize {
		return errors.New("invalid owner signature")
	}
	message, err := c.CanonicalMessage()
	if err != nil || !ed25519.Verify(owner[:], message, signature) {
		return errors.New("withdrawal consent signature verification failed")
	}
	return nil
}

type WithdrawalWaitReason string

const (
	WithdrawalNeedsConsent WithdrawalWaitReason = "needs_consent"
	WithdrawalWaitingPrice WithdrawalWaitReason = "waiting_price"
	WithdrawalExpired      WithdrawalWaitReason = "expired"
	WithdrawalUnknownCost  WithdrawalWaitReason = "cost_unavailable"
	WithdrawalFeeBudget    WithdrawalWaitReason = "fee_budget_exhausted"
)

// Payable already includes current NAV and the existing exit allowance. Only
// incremental costs absent from that NAV are deducted here; never charge twice.
type WithdrawalAdmission struct {
	ProjectedPostFeePayable *USDCRaw
	IncrementalReservedCost USDCRaw
	NewIncrementalCost      USDCRaw
	ConsumedUserFeeLamports uint64
	ReservedUserFeeLamports uint64
	NewUserFeeLamports      uint64
}

func (c WithdrawalConsent) Admit(a WithdrawalAdmission, now time.Time) (WithdrawalWaitReason, error) {
	expires, err := canonicalUint(c.ExpiresAt)
	if err != nil || expires > uint64(^uint64(0)>>1) {
		return WithdrawalNeedsConsent, errors.New("invalid consent expiry")
	}
	if int64(expires) <= now.Unix() {
		return WithdrawalExpired, nil
	}
	if a.ProjectedPostFeePayable == nil {
		return WithdrawalUnknownCost, nil
	}
	floor, err := canonicalUint(c.MinClaimUSDC)
	if err != nil {
		return WithdrawalNeedsConsent, err
	}
	if *a.ProjectedPostFeePayable < 0 || a.IncrementalReservedCost < 0 || a.NewIncrementalCost < 0 {
		return WithdrawalUnknownCost, errors.New("negative projected cost")
	}
	payable := uint64(*a.ProjectedPostFeePayable)
	for _, cost := range []uint64{uint64(a.IncrementalReservedCost), uint64(a.NewIncrementalCost)} {
		if cost > payable {
			return WithdrawalWaitingPrice, nil
		}
		payable -= cost
	}
	if payable < floor {
		return WithdrawalWaitingPrice, nil
	}
	budget, err := canonicalUint(c.MaxUserFeeLamports)
	if err != nil {
		return WithdrawalNeedsConsent, err
	}
	for _, fee := range []uint64{a.ConsumedUserFeeLamports, a.ReservedUserFeeLamports, a.NewUserFeeLamports} {
		if fee > budget {
			return WithdrawalFeeBudget, nil
		}
		budget -= fee
	}
	return "", nil
}

func (g WithdrawalGeneration) Key(authority string) string {
	return fmt.Sprintf("%s/%s/%d", authority, g.RequestSignature, g.RequestInstruction)
}
