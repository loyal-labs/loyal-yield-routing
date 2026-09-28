package backyardrwa

import "encoding/binary"

// Exact two-group forward rollovers exercised in the connected OnRe SBF run.
// Only constraint zero becomes V2. PRIME and ONyc/USDS sibling constraints stay
// byte-for-byte legacy. These private encoders accept no caller-defined policy.
func onreSwapSetupConstraints(operation string) []byte {
	const onyc = "5Y8NV33Vv7WbnLfq3zBcKSdYPrk7g2KoiQoe7M2tcxp5"
	const custody = "AVX9wxDTk639eZ4KaiMA7LrLhXe7Lg6DaDDVRa1Q7Ji3"
	source, destination, from, to := bridgeSquadsATA, custody, bridgeUSDC, onyc
	siblingSource, siblingDestination, siblingFrom, siblingTo := bridgeSquadsATA, kaminoPrimeCustody, bridgeUSDC, kaminoPrimeUSDCCollateralMint
	siblingAmountOffset := uint64(22)
	if operation == "onre-return-swap" {
		source, destination, from, to = custody, bridgeSquadsATA, onyc, bridgeUSDC
		siblingSource, siblingDestination, siblingFrom, siblingTo = custody, "5LR9AdS7XwJjQXWkKNBXNibGNkFXqe7T2JXU2oBBwknV", onyc, "USDSwr9ApdHk5bvJKMjzff41FfuX8bSxdKcR81vTwcA"
		siblingAmountOffset = 27
	}
	data := binary.LittleEndian.AppendUint32(nil, 2)
	program := mustKey("JUP6LkbZbjS1jKKwapdHNy74zcZ3tLUZoi5QNyVTaV4")
	for i := 0; i < 2; i++ {
		data = append(data, program[:]...)
		indexes := []byte{1, 2, 5, 6, 7, 8, 9}
		accounts := []string{bridgeVault, source, destination, from, to, classicTokenProgram, classicTokenProgram}
		discriminator, amountOffset := []byte{0xd1, 0x98, 0x53, 0x93, 0x7c, 0xfe, 0xd8, 0xe9}, uint64(9)
		if i == 1 {
			indexes = []byte{0, 2, 3, 6, 7, 8}
			accounts = []string{classicTokenProgram, bridgeVault, siblingSource, siblingDestination, siblingFrom, siblingTo}
			discriminator, amountOffset = []byte{0xc1, 0x20, 0x9b, 0x33, 0x41, 0xd6, 0x9c, 0x81}, siblingAmountOffset
		}
		data = binary.LittleEndian.AppendUint32(data, uint32(len(accounts)))
		for n, address := range accounts {
			data = appendSetupPubkey(data, indexes[n], address)
		}
		data = binary.LittleEndian.AppendUint32(data, 4)
		data = appendSetupSlice(data, 0, discriminator)
		data = binary.LittleEndian.AppendUint64(data, amountOffset)
		data = append(data, 3) // U64Le <= original raw bound
		data = binary.LittleEndian.AppendUint64(data, bridgeCapRaw)
		data = append(data, 5)
		data = binary.LittleEndian.AppendUint64(data, amountOffset+16)
		data = append(data, 1) // U16Le <= 50 bps
		data = binary.LittleEndian.AppendUint16(data, 50)
		data = append(data, 5)
		if i == 0 {
			data = appendSetupSlice(data, 27, []byte{0, 0, 0, 0})
		} else {
			data = binary.LittleEndian.AppendUint64(data, amountOffset+18)
			data = append(data, 0, 0, 0) // U8 zero, Equals
		}
	}
	return data
}

func appendSetupPubkey(data []byte, index byte, address string) []byte {
	data = append(data, index, 0) // Pubkey
	data = binary.LittleEndian.AppendUint32(data, 1)
	key := mustKey(address)
	data = append(data, key[:]...)
	return append(data, 0) // owner None
}

func appendSetupSlice(data []byte, offset uint64, value []byte) []byte {
	data = binary.LittleEndian.AppendUint64(data, offset)
	data = append(data, 5) // U8Slice
	data = binary.LittleEndian.AppendUint32(data, uint32(len(value)))
	data = append(data, value...)
	return append(data, 0) // Equals
}
