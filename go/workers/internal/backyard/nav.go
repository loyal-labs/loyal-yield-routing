package backyard

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"

	"github.com/solana-foundation/solana-go/v2"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/spl"
)

type DecodedTokenCustody struct {
	Raw          uint64
	TokenProgram string
}

func DecodeTokenCustody(programOwner string, data []byte, expectedMint, expectedAuthority [32]byte) (DecodedTokenCustody, error) {
	if bytes.Equal(expectedMint[:], make([]byte, 32)) || bytes.Equal(expectedAuthority[:], make([]byte, 32)) {
		return DecodedTokenCustody{}, fmt.Errorf("zero token identity")
	}
	owner, _ := solana.PublicKeyFromBase58(programOwner)
	held, err := spl.DecodeTokenAccount(&chain.Account{Owner: owner, Data: data})
	if err != nil {
		return DecodedTokenCustody{}, err
	}
	if held.Mint != expectedMint || held.Owner != expectedAuthority {
		return DecodedTokenCustody{}, fmt.Errorf("token custody mint or authority mismatch")
	}
	if held.Frozen {
		return DecodedTokenCustody{}, fmt.Errorf("token custody is not initialized")
	}
	if held.Delegate != nil || held.IsNative || held.HasCloseAuthority {
		return DecodedTokenCustody{}, fmt.Errorf("token custody has unsupported authority state")
	}
	if err := held.UnrestrictedBalance(); err != nil {
		return DecodedTokenCustody{}, err
	}
	return DecodedTokenCustody{Raw: held.Amount, TokenProgram: programOwner}, nil
}

type NAVComponent struct {
	Account, Owner   string
	Raw, Slot        int64
	Known, Liability bool
}
type navDigest string

type NAVReportInput struct {
	Raw            int64
	SnapshotDigest navDigest
}

type NAVSnapshotContext struct {
	Slot               int64
	ReceiptFingerprint string
	ManifestSHA256     string
}

var sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func ComputeNAV(ctx NAVSnapshotContext, cs []NAVComponent) (NAVReportInput, error) {
	if ctx.Slot <= 0 || ctx.ReceiptFingerprint == "" ||
		!sha256Pattern.MatchString(ctx.ManifestSHA256) || len(cs) == 0 {
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
	h := sha256.Sum256([]byte(fmt.Sprintf("%d:%s:%s:%v", ctx.Slot, ctx.ReceiptFingerprint, ctx.ManifestSHA256, canonical)))
	return NAVReportInput{assets - liabilities, navDigest(hex.EncodeToString(h[:]))}, nil
}
