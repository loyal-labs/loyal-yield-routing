package backyardrwa

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math/big"
	"regexp"
	"sort"
)

type DecodedTokenCustody struct {
	Raw          uint64
	TokenProgram string
}

// DecodeTokenCustody handles the common 165-byte token-account base for both
// programs and validates Token-2022's account-type marker before accepting TLV
// extensions. The exact mint and custody authority are caller-pinned bytes.
func DecodeTokenCustody(programOwner string, data []byte, expectedMint, expectedAuthority [32]byte) (DecodedTokenCustody, error) {
	if bytes.Equal(expectedMint[:], make([]byte, 32)) || bytes.Equal(expectedAuthority[:], make([]byte, 32)) {
		return DecodedTokenCustody{}, fmt.Errorf("zero token identity")
	}
	switch programOwner {
	case classicTokenProgram:
		if len(data) != 165 {
			return DecodedTokenCustody{}, fmt.Errorf("classic SPL token account has extensions or truncation")
		}
	case token2022Program:
		if len(data) != 165 {
			return DecodedTokenCustody{}, fmt.Errorf("Token-2022 extensions are not supported by the plain custody MVP")
		}
	default:
		return DecodedTokenCustody{}, fmt.Errorf("unknown token program owner")
	}
	if !bytes.Equal(data[:32], expectedMint[:]) || !bytes.Equal(data[32:64], expectedAuthority[:]) {
		return DecodedTokenCustody{}, fmt.Errorf("token custody mint or authority mismatch")
	}
	// Frozen accounts (state=2) are readable but cannot safely participate in
	// a money-moving lifecycle.
	if data[108] != 1 {
		return DecodedTokenCustody{}, fmt.Errorf("token custody is not initialized")
	}
	// The MVP pins plain custody accounts. Extra authority state is not present
	// in the manifest and therefore fails closed.
	if binary.LittleEndian.Uint32(data[72:76]) != 0 ||
		binary.LittleEndian.Uint32(data[109:113]) != 0 ||
		binary.LittleEndian.Uint32(data[129:133]) != 0 {
		return DecodedTokenCustody{}, fmt.Errorf("token custody has unsupported authority state")
	}
	return DecodedTokenCustody{Raw: binary.LittleEndian.Uint64(data[64:72]), TokenProgram: programOwner}, nil
}

// ValueRawUSDC converts token raw units at a micro-dollar price. Assets round
// down and liabilities round up, so rounding can never inflate NAV.
func ValueRawUSDC(raw uint64, tokenDecimals uint8, priceMicros uint64, liability bool) (int64, error) {
	if tokenDecimals > 18 || priceMicros == 0 {
		return 0, fmt.Errorf("invalid valuation input")
	}
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(tokenDecimals)), nil)
	numerator := new(big.Int).Mul(new(big.Int).SetUint64(raw), new(big.Int).SetUint64(priceMicros))
	if liability && numerator.Sign() > 0 {
		numerator.Add(numerator, new(big.Int).Sub(scale, big.NewInt(1)))
	}
	value := numerator.Div(numerator, scale)
	if !value.IsInt64() || value.Sign() < 0 {
		return 0, fmt.Errorf("valuation overflow")
	}
	return value.Int64(), nil
}

type ObligationNAV struct {
	Address    string
	Recognized bool
	Nonzero    bool
}

func ValidateSingleObligation(obligations []ObligationNAV) error {
	active := 0
	for _, obligation := range obligations {
		if obligation.Nonzero {
			if !obligation.Recognized || obligation.Address == "" {
				return fmt.Errorf("unknown active obligation")
			}
			active++
		}
	}
	if active > 1 {
		return fmt.Errorf("multiple active obligations")
	}
	return nil
}

type NAVComponent struct {
	Account, Owner   string
	Raw, Slot        int64
	Known, Liability bool
}
type NAVReportInput struct {
	Raw            int64
	SnapshotDigest string
}

type NAVSnapshotContext struct {
	Slot                int64
	ReceiptFingerprint  string
	ManifestSHA256      string
	PolicyCatalogSHA256 string
}

var sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func ComputeNAV(ctx NAVSnapshotContext, cs []NAVComponent) (NAVReportInput, error) {
	if ctx.Slot <= 0 || ctx.ReceiptFingerprint == "" ||
		!sha256Pattern.MatchString(ctx.ManifestSHA256) ||
		!sha256Pattern.MatchString(ctx.PolicyCatalogSHA256) || len(cs) == 0 {
		return NAVReportInput{}, fmt.Errorf("invalid slot")
	}
	seen := map[string]string{}
	var assets, liabilities int64
	canonical := []string{}
	for _, c := range cs {
		if !c.Known || c.Owner == "" || c.Account == "" || c.Slot != ctx.Slot || c.Raw < 0 {
			return NAVReportInput{}, fmt.Errorf("invalid NAV component")
		}
		encoded := fmt.Sprintf("%s:%s:%d:%t", c.Account, c.Owner, c.Raw, c.Liability)
		if previous, exists := seen[c.Account]; exists {
			if previous != encoded {
				return NAVReportInput{}, fmt.Errorf("conflicting duplicate NAV component")
			}
			continue
		}
		seen[c.Account] = encoded
		if c.Liability {
			if c.Raw > 0 && liabilities > int64(^uint64(0)>>1)-c.Raw {
				return NAVReportInput{}, fmt.Errorf("NAV overflow")
			}
			liabilities += c.Raw
		} else {
			if c.Raw > 0 && assets > int64(^uint64(0)>>1)-c.Raw {
				return NAVReportInput{}, fmt.Errorf("NAV overflow")
			}
			assets += c.Raw
		}
		canonical = append(canonical, encoded)
	}
	if liabilities > assets {
		return NAVReportInput{}, fmt.Errorf("NAV underflow")
	}
	sort.Strings(canonical)
	h := sha256.Sum256([]byte(fmt.Sprintf("%d:%s:%s:%s:%v", ctx.Slot, ctx.ReceiptFingerprint, ctx.ManifestSHA256, ctx.PolicyCatalogSHA256, canonical)))
	return NAVReportInput{assets - liabilities, hex.EncodeToString(h[:])}, nil
}
