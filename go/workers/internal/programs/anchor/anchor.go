// Package anchor derives Anchor's 8-byte account and instruction
// discriminators from their names and frames Borsh Options.
package anchor

import (
	"crypto/sha256"
	"encoding/binary"
)

const (
	OptionNone          byte = 0
	OptionSomeTag       byte = 1
	OptionSomeHeaderLen      = 1 + 4
)

func OptionSomeHeader(length uint32) []byte {
	return binary.LittleEndian.AppendUint32([]byte{OptionSomeTag}, length)
}

func OptionSome(value []byte) []byte {
	return append(OptionSomeHeader(uint32(len(value))), value...)
}

func Account(name string) [8]byte { return discriminator("account:" + name) }

func Instruction(name string) [8]byte { return discriminator("global:" + name) }

func discriminator(preimage string) [8]byte {
	sum := sha256.Sum256([]byte(preimage))
	return [8]byte(sum[:8])
}
