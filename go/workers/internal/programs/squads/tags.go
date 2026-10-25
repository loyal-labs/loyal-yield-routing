package squads

const (
	ValueU8 uint8 = iota
	ValueU16
	ValueU32
	ValueU64
	ValueU128
	ValueBytes
)

const (
	PeriodOneTime uint8 = iota
	PeriodDaily
	PeriodWeekly
	PeriodMonthly
	PeriodCustom
)

const (
	PermissionInitiate uint8 = 1 << iota
	PermissionVote
	PermissionExecute
)

const (
	settingsActionPolicyCreate byte = 7
	settingsActionPolicyRemove byte = 9

	policyCreationLegacyProgramInteraction byte  = 3
	policyKindProgramInteraction           uint8 = 3

	syncPayloadPolicy                 byte = 1
	policyPayloadProgramInteraction   byte = 1
	transactionPayloadSyncTransaction byte = 1

	noMemo           byte = 0
	noPreHook        byte = 0
	noPostHook       byte = 0
	noSpendingLimits byte = 0
	noStartTimestamp byte = 0
	noExpirationArgs byte = 0

	legacyPolicySignerCount = 1
	legacyPolicyThreshold   = 1
	legacyPolicyTimeLock    = 0

	archivalAuthorityLen  = 32
	archivableAfterLen    = 8
	accountUtilizationLen = 1
)
