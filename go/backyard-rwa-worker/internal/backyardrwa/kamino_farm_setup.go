package backyardrwa

import "fmt"

const kaminoFarmSetupMode byte = 0

var kaminoInitObligationFarmsForReserve = []byte{136, 63, 15, 186, 211, 152, 168, 164}

// KaminoFarmSetupAccount is the exact account meta emitted for the one-time
// KLend farm registration leg. The payer is the only signer in the KLend IDL;
// the owner is the vault but is deliberately read-only and non-signer.
type KaminoFarmSetupAccount struct {
	Address  string
	Signer   bool
	Writable bool
}

type KaminoFarmSetupRequest struct {
	Payer                   string
	Owner                   string
	Obligation              string
	LendingMarket           string
	LendingMarketAuthority  string
	Reserve                 string
	ReserveFarmState        string
	ObligationFarmUserState string
}

type KaminoFarmSetupInstruction struct {
	Program  string
	Accounts []KaminoFarmSetupAccount
	Data     []byte
}

// KaminoFarmSetupHold is a typed, manual-only state for a farm user account
// that has not yet been observed. The normal worker tick never constructs or
// submits this leg; an operator workflow may use the request after reviewing
// the hold and then wait for finalized account existence.
type KaminoFarmSetupHold struct {
	Lane      string
	UserState string
}

func (h *KaminoFarmSetupHold) Error() string {
	return fmt.Sprintf("HOLD: Kamino farm user state for %s is not finalized", h.Lane)
}

func (h *KaminoFarmSetupHold) ResumeCondition() string {
	return "manually run the payer-signed farm setup leg, then observe the obligation farm user state at finalized commitment"
}

func kaminoFarmSetupHold(lane, userState string, exists bool) error {
	if exists {
		return nil
	}
	if lane == "" || userState == "" {
		return fmt.Errorf("invalid Kamino farm setup hold identity")
	}
	return &KaminoFarmSetupHold{Lane: lane, UserState: userState}
}

// BuildKaminoFarmSetupInstruction builds the exact KLend inner instruction.
// It is an explicit setup seam for a future operator-controlled workflow, not
// part of the ordinary four-instruction Kamino money-moving transaction.
func BuildKaminoFarmSetupInstruction(request KaminoFarmSetupRequest) (KaminoFarmSetupInstruction, error) {
	keys := []struct {
		name     string
		address  string
		signer   bool
		writable bool
	}{
		{"payer", request.Payer, true, true},
		{"owner", request.Owner, false, false},
		{"obligation", request.Obligation, false, true},
		{"lendingMarketAuthority", request.LendingMarketAuthority, false, false},
		{"reserve", request.Reserve, false, true},
		{"reserveFarmState", request.ReserveFarmState, false, true},
		{"obligationFarmUserState", request.ObligationFarmUserState, false, true},
		{"lendingMarket", request.LendingMarket, false, false},
		{"farmsProgram", kaminoFarmsProgram, false, false},
		{"rent", "SysvarRent111111111111111111111111111111111", false, false},
		{"systemProgram", "11111111111111111111111111111111", false, false},
	}
	accounts := make([]KaminoFarmSetupAccount, len(keys))
	for index, key := range keys {
		if _, err := decodeKey(key.address); err != nil {
			return KaminoFarmSetupInstruction{}, fmt.Errorf("invalid Kamino farm setup %s account: %w", key.name, err)
		}
		accounts[index] = KaminoFarmSetupAccount{Address: key.address, Signer: key.signer, Writable: key.writable}
	}
	derived, err := deriveKaminoObligationFarmUserState(request.ReserveFarmState, request.Obligation)
	if err != nil {
		return KaminoFarmSetupInstruction{}, err
	}
	if derived != request.ObligationFarmUserState {
		return KaminoFarmSetupInstruction{}, fmt.Errorf("Kamino farm setup user state is not derived from reserve farm and obligation")
	}
	market, err := decodeKey(request.LendingMarket)
	if err != nil {
		return KaminoFarmSetupInstruction{}, fmt.Errorf("decode lending market: %w", err)
	}
	klend, err := decodeKey(kaminoProgram)
	if err != nil {
		return KaminoFarmSetupInstruction{}, fmt.Errorf("decode KLend program: %w", err)
	}
	marketAuthority, err := findProgramDerivedAddress([]byte("lma"), klend[:], market[:])
	if err != nil || marketAuthority != request.LendingMarketAuthority {
		return KaminoFarmSetupInstruction{}, fmt.Errorf("Kamino farm setup lending market authority is not derived from the market")
	}
	data := append([]byte(nil), kaminoInitObligationFarmsForReserve...)
	data = append(data, kaminoFarmSetupMode)
	return KaminoFarmSetupInstruction{Program: kaminoProgram, Accounts: accounts, Data: data}, nil
}
