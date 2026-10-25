package webapp

type WirePreparedInstructionKey struct {
	IsSigner   bool   `json:"isSigner"`
	IsWritable bool   `json:"isWritable"`
	Pubkey     string `json:"pubkey"`
}

type WirePreparedInstruction struct {
	DataBase64 string                       `json:"dataBase64"`
	Keys       []WirePreparedInstructionKey `json:"keys" nullable:"false"`
	ProgramID  string                       `json:"programId"`
}

type WireAddressLookupTable struct {
	Key   string `json:"key"`
	State struct {
		Addresses                  []string `json:"addresses" nullable:"false"`
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
	Instructions         []WirePreparedInstruction `json:"instructions" nullable:"false"`
	LookupTableAccounts  []WireAddressLookupTable  `json:"lookupTableAccounts" nullable:"false"`
	Operation            string                    `json:"operation"`
	Payer                string                    `json:"payer"`
	ProgramID            string                    `json:"programId"`
	RequiresConfirmation bool                      `json:"requiresConfirmation"`
}

type EarnVaultRefundEntry struct {
	Account                 string                        `json:"account"`
	BlockedReason           *string                       `json:"blockedReason"`
	CanRefund               bool                          `json:"canRefund"`
	Lamports                uint64                        `json:"lamports"`
	TokenAccounts           []EarnVaultRefundTokenAccount `json:"tokenAccounts"`
	TotalRefundableLamports uint64                        `json:"totalRefundableLamports"`
}

type PreparedEarnVaultRefund struct {
	EstimatedRefundLamports uint64                `json:"estimatedRefundLamports"`
	Prepared                WirePreparedOperation `json:"prepared"`
	SettingsPda             string                `json:"settingsPda"`
	Vault                   EarnVaultRefundEntry  `json:"vault"`
	VaultIndex              uint8                 `json:"vaultIndex"`
}

type WirePreparedEarnPolicyRefund struct {
	EstimatedRefundLamports uint64                     `json:"estimatedRefundLamports"`
	Policy                  EarnPolicyRefundScanPolicy `json:"policy"`
	Prepared                WirePreparedOperation      `json:"prepared"`
	SettingsPda             string                     `json:"settingsPda"`
	VaultIndex              uint8                      `json:"vaultIndex"`
}

// WirePreparedEarnRecurringDelegationRefund mirrors
// WireSmartAccountPreparedEarnRecurringDelegationRefund
// . The estimated lamports ride the delegation's
// own nullable rent read, so they serialize as null when the account is gone.
type WirePreparedEarnRecurringDelegationRefund struct {
	EstimatedRefundLamports *uint64                             `json:"estimatedRefundLamports"`
	Prepared                WirePreparedOperation               `json:"prepared"`
	RecurringDelegation     EarnPolicyRefundRecurringDelegation `json:"recurringDelegation"`
	SettingsPda             string                              `json:"settingsPda"`
	VaultIndex              uint8                               `json:"vaultIndex"`
}

// EarnPolicyRefundPrepareResponse mirrors EarnPolicyRefundPrepareResponse
// : exactly one branch key per request kind.
type EarnPolicyRefundPrepareResponse struct {
	PreparedVaultRefund               *PreparedEarnVaultRefund                   `json:"preparedVaultRefund,omitempty"`
	PreparedRecurringDelegationRefund *WirePreparedEarnRecurringDelegationRefund `json:"preparedRecurringDelegationRefund,omitempty"`
	PreparedRefund                    *WirePreparedEarnPolicyRefund              `json:"preparedRefund,omitempty"`
}
