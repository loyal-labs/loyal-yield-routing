package webapp

// Wire shapes for the cleanup routes. They mirror serializePreparedEarnUsdcCleanup
// and the parse/serialize pair in
// web/apps/web/src/lib/yield-optimization/earn-withdraw-cleanup-contracts.shared.ts,
// with the prepared operation from wire/prepared-operation-wire.shared.ts:31-94
// (WirePreparedOperation in refunds_wire.go).

// EarnCleanupPrepareRequest documents the prepare body for OpenAPI; the
// handler parses the raw bytes so the TS error messages carry over verbatim.
// minContextSlot is a decimal string when present.
type EarnCleanupPrepareRequest struct {
	MinContextSlot *string `json:"minContextSlot,omitempty"`
}

// EarnCleanupConfirmRequest documents the confirm body for OpenAPI; the
// handler parses the raw bytes so the TS error messages carry over verbatim.
type EarnCleanupConfirmRequest struct {
	AutodepositCloseConfirmedSlot *string         `json:"autodepositCloseConfirmedSlot,omitempty"`
	AutodepositCloseSignature     *string         `json:"autodepositCloseSignature,omitempty"`
	CleanupSignature              *string         `json:"cleanupSignature,omitempty"`
	ConfirmedSlot                 *string         `json:"confirmedSlot,omitempty"`
	PreparedCleanup               *map[string]any `json:"preparedCleanup,omitempty"`
}

// EarnCleanupPrepareResponse wraps the wire cleanup one level, like the Next
// route's { preparedCleanup: ... }.
type EarnCleanupPrepareResponse struct {
	PreparedCleanup EarnCleanupPrepareWire `json:"preparedCleanup"`
}

// EarnCleanupPrepareWire is WireSmartAccountPreparedEarnUsdcCleanup
// (contracts.shared.ts:13-33). autodepositClosePrepared and
// estimatedRefundLamports are always present, null when unknown; setupPolicy
// is dropped when the vault has none.
type EarnCleanupPrepareWire struct {
	AutodepositClosePrepared *EarnCleanupAutodepositCloseWire `json:"autodepositClosePrepared"`
	EstimatedRefundLamports  *uint64                          `json:"estimatedRefundLamports"`
	Persistence              EarnCleanupPersistenceWire       `json:"persistence"`
	Policy                   EarnCleanupPolicyWire            `json:"policy"`
	SetupPolicy              *EarnCleanupPolicyWire           `json:"setupPolicy,omitempty"`
	Prepared                 WirePreparedOperation            `json:"prepared"`
	Vault                    EarnCleanupVaultWire             `json:"vault"`
}

// EarnCleanupPolicyWire is the {account, id, seed} triple; the TS ids are
// BigInt seed values serialized as decimal strings.
type EarnCleanupPolicyWire struct {
	Account string `json:"account"`
	ID      string `json:"id"`
	Seed    string `json:"seed"`
}

type EarnCleanupVaultWire struct {
	AccountIndex uint8  `json:"accountIndex"`
	Pubkey       string `json:"pubkey"`
	UsdcAta      string `json:"usdcAta"`
}

// EarnCleanupPersistenceWire is SmartAccountEarnUsdcCleanupMetadata
// (client.ts:10287-10310). setupPolicy* ride only when a setup policy exists;
// autodepositClose is the literal null when the vault has none.
type EarnCleanupPersistenceWire struct {
	Cluster               string                           `json:"cluster"`
	WalletAddress         string                           `json:"walletAddress"`
	DelegatedSigner       string                           `json:"delegatedSigner"`
	Settings              string                           `json:"settings"`
	VaultIndex            uint8                            `json:"vaultIndex"`
	VaultPubkey           string                           `json:"vaultPubkey"`
	PolicyID              string                           `json:"policyId"`
	PolicyAccount         string                           `json:"policyAccount"`
	PolicySeed            string                           `json:"policySeed"`
	SetupPolicyID         string                           `json:"setupPolicyId,omitempty"`
	SetupPolicyAccount    string                           `json:"setupPolicyAccount,omitempty"`
	SetupPolicySeed       string                           `json:"setupPolicySeed,omitempty"`
	IdleTransferAmountRaw string                           `json:"idleTransferAmountRaw"`
	ClosedVaultUsdcAta    bool                             `json:"closedVaultUsdcAta"`
	ClosedCollateralAtas  []string                         `json:"closedCollateralAtas"`
	AutodepositClose      *EarnCleanupClosePersistenceWire `json:"autodepositClose"`
}

// EarnCleanupClosePersistenceWire is SmartAccountEarnUsdcAutodepositCloseMetadata.
type EarnCleanupClosePersistenceWire struct {
	Cluster             string `json:"cluster"`
	WalletAddress       string `json:"walletAddress"`
	DelegatedSigner     string `json:"delegatedSigner"`
	Settings            string `json:"settings"`
	VaultIndex          uint8  `json:"vaultIndex"`
	VaultPubkey         string `json:"vaultPubkey"`
	PolicyAccount       string `json:"policyAccount"`
	RecurringDelegation string `json:"recurringDelegation"`
}

// EarnCleanupAutodepositCloseWire is WireSmartAccountPreparedEarnUsdcAutodepositClose
// (wire/earn-prepare-wire.shared.ts:47-60).
type EarnCleanupAutodepositCloseWire struct {
	Persistence EarnCleanupClosePersistenceWire `json:"persistence"`
	Policy      struct {
		Account string `json:"account"`
	} `json:"policy"`
	Prepared     WirePreparedOperation `json:"prepared"`
	Subscription struct {
		RecurringDelegation string `json:"recurringDelegation"`
	} `json:"subscription"`
	Vault struct {
		AccountIndex uint8  `json:"accountIndex"`
		Pubkey       string `json:"pubkey"`
	} `json:"vault"`
}

// EarnCleanupConfirmResponse is the confirm route's verified answer.
type EarnCleanupConfirmResponse struct {
	OK     bool   `json:"ok"`
	Status string `json:"status"`
}
