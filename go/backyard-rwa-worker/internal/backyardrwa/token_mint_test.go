package backyardrwa

import (
	"encoding/binary"
	"testing"
)

func TestExecutionMintRejectsChangedTransferSemantics(t *testing.T) {
	data := make([]byte, 166)
	data[44], data[45], data[165] = 6, 1, 1
	appendExtension := func(kind uint16, value []byte) {
		header := make([]byte, 4)
		binary.LittleEndian.PutUint16(header, kind)
		binary.LittleEndian.PutUint16(header[2:], uint16(len(value)))
		data = append(data, header...)
		data = append(data, value...)
	}
	appendExtension(1, make([]byte, 108))
	appendExtension(14, make([]byte, 64))
	appendExtension(16, make([]byte, 129))
	account := ConfirmedAccount{Owner: token2022Program, Data: data}
	if err := validateExecutionMint(account, token2022Program, 6); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		offset int
		reason string
	}{
		{"current-fee", 170 + 88, "execution_mint_transfer_fee_enabled"},
		{"scheduled-fee", 170 + 106, "execution_mint_transfer_fee_enabled"},
		{"enabled-hook", 282 + 32, "execution_mint_transfer_hook_enabled"},
		{"unknown-extension", 166, "execution_mint_extension_unsupported"},
		{"mint-padding", 100, "execution_mint_layout_mismatch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := account
			changed.Data = append([]byte(nil), data...)
			if tc.name == "unknown-extension" {
				changed.Data[tc.offset] = 99
			} else {
				changed.Data[tc.offset] = 1
			}
			assertBudgetHold(t, validateExecutionMint(changed, token2022Program, 6), tc.reason)
		})
	}
	for size := 0; size < len(data); size++ {
		// Exercise every truncation without allowing a bounds panic. Some
		// exact boundaries form valid shorter mints; their result is not fixed.
		short := account
		short.Data = data[:size]
		_ = validateExecutionMint(short, token2022Program, 6)
	}
}
