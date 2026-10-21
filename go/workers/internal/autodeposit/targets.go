package autodeposit

type ExecutableTarget struct {
	TargetID        int64
	ScheduledSlotID int64
	ClaimToken      string
	Wallet          string
}

func (t ExecutableTarget) isRecovery() bool { return t.ClaimToken != "" }
