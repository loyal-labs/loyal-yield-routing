package backyard

import "context"

// Reviewed KLend release 1.25.0, verified source a08760976f51a3a58c4a0c6ea27b4a0e565bca79.
// This pin hashes FULL ProgramData.data[45:], including allocation padding.
// The verified build's zero-trimmed b1344d... hash is a different hash domain.
// This is a top-up capability only; global Voltr/adaptor pins are unchanged.
func reviewedTopupKaminoIdentity() pinnedProgramIdentity {
	return pinnedProgramIdentity{program: kaminoProgram, programData: "9uSbGW1y9H5Av6H5TKxQ1wnFApSq2t3oEpfF2YfjDQGA",
		deploySlot: 440486775, dataSHA256: "9db16dd4b7bbfe4f13df850bf880bfc4522fcece06717c0626d625746a3cc85b"}
}

// No cache yet: verify the reviewed executable and actual loan accounts in ONE
// finalized RPC batch. Identity is checked before decoding the loan, and a
// program upgrade cannot race between an identity read and an origin read.
// Admission must bind the returned origin once under the existing route lock.
func observeTopupLoanOrigin(ctx context.Context, rpc *RPCClient) (topupLoan, error) {
	if rpc == nil {
		return topupLoan{}, budgetHold("topup_finalized_origin_unavailable")
	}
	slot, err := rpc.FinalizedSlot(ctx)
	if err != nil {
		return topupLoan{}, err
	}
	pin := reviewedTopupKaminoIdentity()
	addresses := append([]string{pin.program, pin.programData}, payoffWindowAddresses(autoAUTOPYUSD)...)
	observed, accounts, err := rpc.getMultipleAccountsAtCommitment(ctx, addresses, slot, nil, "finalized")
	if err != nil {
		return topupLoan{}, err
	}
	if err := validateTopupKaminoCapability(accounts, observed); err != nil {
		return topupLoan{}, err
	}
	return captureTopupLoan(accounts, autoAUTOPYUSD, observed)
}

// Reuse at fresh top-up admission/build/send boundaries without recapturing or
// rebasing the immutable origin. Requires the program and full ProgramData
// from the same account batch whose context slot is supplied.
func validateTopupKaminoCapability(accounts []ConfirmedAccount, slot int64) error {
	if err := validateTopupLoanCapture(accounts, autoAUTOPYUSD, slot); err != nil {
		return err
	}
	pin := reviewedTopupKaminoIdentity()
	image := func(a ConfirmedAccount) programIdentityImage {
		return programIdentityImage{Address: a.Address, Owner: a.Owner, Lamports: a.Lamports, Executable: a.Executable, Data: a.Data}
	}
	programData, err := decodeProgramIdentityHeader(image(accountAt(accounts, pin.program)))
	if err != nil || programData != pin.programData || slot < pin.deploySlot {
		return budgetHold("topup_kamino_program_identity_changed")
	}
	data := image(accountAt(accounts, pin.programData))
	if data.Executable {
		return budgetHold("topup_kamino_program_identity_changed")
	}
	if _, err = verifyProgramDataImage(data, pin); err != nil {
		return budgetHold("topup_kamino_program_identity_changed")
	}
	return nil
}
