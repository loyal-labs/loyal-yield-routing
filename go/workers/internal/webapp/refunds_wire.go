package webapp

// Wire shapes for POST /api/smart-accounts/yield-optimization/policy-refunds/prepare.
// They mirror web/apps/web/src/lib/yield-optimization/earn-policy-refund-contracts.shared.ts
// and web/packages/smart-account-vaults/src/wire/prepared-operation-wire.shared.ts:31-94.

type WirePreparedInstructionKey struct {
	IsSigner   bool   `json:"isSigner"`
	IsWritable bool   `json:"isWritable"`
	Pubkey     string `json:"pubkey"`
}

type WirePreparedInstruction struct {
	DataBase64 string                       `json:"dataBase64"`
	Keys       []WirePreparedInstructionKey `json:"keys"`
	ProgramID  string                       `json:"programId"`
}

type WireAddressLookupTable struct {
	Key   string `json:"key"`
	State struct {
		Addresses                  []string `json:"addresses"`
		Authority                  *string  `json:"authority"`
		DeactivationSlot           string   `json:"deactivationSlot"`
		LastExtendedSlot           uint64   `json:"lastExtendedSlot"`
		LastExtendedSlotStartIndex uint8    `json:"lastExtendedSlotStartIndex"`
	} `json:"state"`
}

// WirePreparedOperation mirrors WirePreparedLoyalSmartAccountsOperation. The
// Earn vault refund never carries lookup tables or simulation diagnostics, but
// the fields are part of the response contract and must serialize as present.
type WirePreparedOperation struct {
	Instructions         []WirePreparedInstruction `json:"instructions"`
	LookupTableAccounts  []WireAddressLookupTable  `json:"lookupTableAccounts"`
	Operation            string                    `json:"operation"`
	Payer                string                    `json:"payer"`
	ProgramID            string                    `json:"programId"`
	RequiresConfirmation bool                      `json:"requiresConfirmation"`
}

// EarnVaultRefundEntry mirrors EarnPolicyRefundVaultEntry: the Earn vault's
// refundable rent, chain-derived (contracts.shared.ts:72-83).
type EarnVaultRefundEntry struct {
	Account                 string                        `json:"account"`
	BlockedReason           *string                       `json:"blockedReason"`
	CanRefund               bool                          `json:"canRefund"`
	Lamports                uint64                        `json:"lamports"`
	TokenAccounts           []EarnVaultRefundTokenAccount `json:"tokenAccounts"`
	TotalRefundableLamports uint64                        `json:"totalRefundableLamports"`
}

// PreparedEarnVaultRefund mirrors WireSmartAccountPreparedEarnVaultRefund
// (contracts.shared.ts:123-129).
type PreparedEarnVaultRefund struct {
	EstimatedRefundLamports uint64                `json:"estimatedRefundLamports"`
	Prepared                WirePreparedOperation `json:"prepared"`
	SettingsPda             string                `json:"settingsPda"`
	Vault                   EarnVaultRefundEntry  `json:"vault"`
	VaultIndex              uint8                 `json:"vaultIndex"`
}

// EarnPolicyRefundPrepareResponse mirrors EarnPolicyRefundPrepareResponse
// (contracts.shared.ts:131-135). This unit serves the vault kind; the other
// refund kinds answer 503 earn_refund_unported.
type EarnPolicyRefundPrepareResponse struct {
	PreparedVaultRefund *PreparedEarnVaultRefund `json:"preparedVaultRefund,omitempty"`
}
