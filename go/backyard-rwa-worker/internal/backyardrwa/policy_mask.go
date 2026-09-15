package backyardrwa

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// maxMaskedPolicyBytes caps how much of a pinned policy account the manifest
// may exclude from the digest. The volatile region of one embedded spending
// limit - timeConstraints.start, usage.remainingInPeriod, usage.lastReset -
// is 24 bytes, so anything dramatically larger would mean the manifest is
// unpinning policy semantics rather than program-owned counters.
const maxMaskedPolicyBytes = 64

// observedPolicyPin is one pinned policy account: the digest to compare and
// the volatile byte span excluded from it. Bridge policies carry their masked
// digest and the manifest's byte mask; policies without a mask compare as the
// raw account digest.
type observedPolicyPin struct {
	digest string
	mask   [][2]int64
}

// maskedPolicyDigestMatches reports whether live policy account bytes match
// the manifest's normalized digest: the sha256 of the raw account data with
// every masked byte range zeroed. The ranges are derived offline from the
// generated Squads layout (tools/backyard-voltr
// src/activation/rwa-multiply-strategy2-policy-mask.ts) and cover only the
// bytes the program rewrites during normal operation, so a charged or
// re-windowed spending limit no longer fails the pin while any drift in the
// constraints, programs, executor, mint, period, quantity, or padding does.
// A structural mask fault fails closed as a mismatch.
func maskedPolicyDigestMatches(data []byte, maskedByteRanges [][2]int64, pinned string) bool {
	if len(data) == 0 || !sha256Pattern.MatchString(pinned) {
		return false
	}
	if validatePolicyByteMask(maskedByteRanges) != nil {
		return false
	}
	masked := append([]byte(nil), data...)
	total := 0
	for _, bounds := range maskedByteRanges {
		if int(bounds[1]) > len(masked) {
			return false
		}
		total += int(bounds[1] - bounds[0])
		for offset := int(bounds[0]); offset < int(bounds[1]); offset++ {
			masked[offset] = 0
		}
	}
	digest := sha256.Sum256(masked)
	return hex.EncodeToString(digest[:]) == pinned
}

// validatePolicyByteMask rejects masks that could silently unpin policy
// semantics: unsorted, overlapping, empty, or over the cap. Range bounds are
// additionally checked against the live account length at compare time,
// because only the observed account knows how long it is.
func validatePolicyByteMask(maskedByteRanges [][2]int64) error {
	total := 0
	previousEnd := int64(0)
	for index, bounds := range maskedByteRanges {
		start, end := bounds[0], bounds[1]
		if start < 0 || end <= start {
			return fmt.Errorf("masked byte range %d is empty or negative", index)
		}
		if start < previousEnd {
			return fmt.Errorf("masked byte range %d overlaps or precedes range %d", index, index-1)
		}
		total += int(end - start)
		previousEnd = end
	}
	if total > maxMaskedPolicyBytes {
		return fmt.Errorf("mask excludes %d bytes, cap is %d", total, maxMaskedPolicyBytes)
	}
	return nil
}
