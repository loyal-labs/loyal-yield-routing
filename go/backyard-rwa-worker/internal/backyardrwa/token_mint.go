package backyardrwa

import "encoding/binary"

// validateExecutionMint checks current transfer semantics, not a stablecoin
// symbol or historical mint snapshot. Authority identities still belong to the
// lane's pinned evidence. Confidential account operations are not supported by
// DecodeTokenCustody even when the mint permits creating such accounts.
func validateExecutionMint(account ConfirmedAccount, program string, decimals uint8) error {
	data := account.Data
	if account.Owner != program || account.Executable || len(data) < 82 || decimals > 18 || data[44] != decimals || data[45] != 1 || binary.LittleEndian.Uint32(data[:4]) > 1 || binary.LittleEndian.Uint32(data[46:50]) > 1 {
		return budgetHold("execution_mint_metadata_mismatch")
	}
	if program == classicTokenProgram {
		if len(data) != 82 {
			return budgetHold("execution_mint_layout_mismatch")
		}
		return nil
	}
	if program != token2022Program {
		return budgetHold("execution_mint_program_mismatch")
	}
	if len(data) == 82 {
		return nil
	}
	if len(data) < 170 || len(data) == 355 || data[165] != 1 {
		return budgetHold("execution_mint_layout_mismatch")
	}
	for _, b := range data[82:165] {
		if b != 0 {
			return budgetHold("execution_mint_layout_mismatch")
		}
	}
	seen := map[uint16]bool{}
	for tail := data[166:]; len(tail) > 0; {
		if len(tail) < 4 {
			return budgetHold("execution_mint_tlv_truncated")
		}
		kind, length := binary.LittleEndian.Uint16(tail[:2]), int(binary.LittleEndian.Uint16(tail[2:4]))
		if kind == 0 {
			for _, b := range tail {
				if b != 0 {
					return budgetHold("execution_mint_padding_invalid")
				}
			}
			return nil
		}
		if seen[kind] || length > len(tail)-4 {
			return budgetHold("execution_mint_tlv_invalid")
		}
		seen[kind] = true
		value := tail[4 : 4+length]
		valid := false
		switch kind {
		case 1: // TransferFeeConfig: reject both current and scheduled fees.
			valid = length == 108
			if valid && (binary.LittleEndian.Uint16(value[88:90]) != 0 || binary.LittleEndian.Uint16(value[106:108]) != 0) {
				return budgetHold("execution_mint_transfer_fee_enabled")
			}
		case 3, 12: // MintCloseAuthority / PermanentDelegate; no authority mutation.
			valid = length == 32
		case 4: // ConfidentialTransferMint, account-side confidential state rejected.
			valid = length == 65 && value[32] <= 1
		case 14: // TransferHook: only the disabled program is supported.
			valid = length == 64
			if valid {
				for _, b := range value[32:] {
					if b != 0 {
						return budgetHold("execution_mint_transfer_hook_enabled")
					}
				}
			}
		case 16: // ConfidentialTransferFeeConfig, SPL Token-2022 POD layout.
			valid = length == 129 && value[64] <= 1
		case 18: // MetadataPointer does not alter raw token transfer accounting.
			valid = length == 64
		case 19: // TokenMetadata; variable strings are irrelevant to raw transfers.
			valid = length >= 80
		default:
			return budgetHold("execution_mint_extension_unsupported")
		}
		if !valid {
			return budgetHold("execution_mint_extension_malformed")
		}
		tail = tail[4+length:]
	}
	return nil
}
