// Package anchor derives Anchor's 8-byte account and instruction
// discriminators from their names.
package anchor

import "crypto/sha256"

func Account(name string) [8]byte { return discriminator("account:" + name) }

func Instruction(name string) [8]byte { return discriminator("global:" + name) }

func discriminator(preimage string) [8]byte {
	sum := sha256.Sum256([]byte(preimage))
	return [8]byte(sum[:8])
}
