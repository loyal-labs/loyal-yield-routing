package webapp

import "errors"

// Confirm ops for the remaining autodeposit writes, ported from
// web/apps/web/src/app/api/smart-accounts/yield-optimization/autodeposit/{close,floor,toggle}/confirm/route.ts.
// Mobile's wallet pushes stay inside the ops (pathmap.md:15-16): floor pushes
// the scheduled-sweep copy when a rebaseline sweep was scheduled, toggle
// pushes the enabled copy when the target turns active.

const (
	autodepositCloseConfirmPath  = "/api/smart-accounts/yield-optimization/autodeposit/close/confirm"
	autodepositFloorConfirmPath  = "/api/smart-accounts/yield-optimization/autodeposit/floor/confirm"
	autodepositToggleConfirmPath = "/api/smart-accounts/yield-optimization/autodeposit/toggle/confirm"
)

// AutodepositCloseConfirmRequest is the documented body: every unsigned
// integer arrives as a decimal string, as the web client sends it.
type AutodepositCloseConfirmRequest struct {
	Cluster             string `json:"cluster"`
	CloseSignature      string `json:"closeSignature"`
	ConfirmedSlot       string `json:"confirmedSlot"`
	DelegatedSigner     string `json:"delegatedSigner"`
	PolicyAccount       string `json:"policyAccount"`
	RecurringDelegation string `json:"recurringDelegation"`
	Settings            string `json:"settings"`
	VaultIndex          int    `json:"vaultIndex"`
	VaultPubkey         string `json:"vaultPubkey"`
	WalletAddress       string `json:"walletAddress"`
}

type AutodepositFloorConfirmRequest struct {
	PolicyAccount         string `json:"policyAccount"`
	RecurringDelegation   string `json:"recurringDelegation"`
	VaultIndex            int    `json:"vaultIndex"`
	WalletBalanceFloorRaw string `json:"walletBalanceFloorRaw"`
}

type AutodepositToggleConfirmRequest struct {
	Active              bool   `json:"active"`
	PolicyAccount       string `json:"policyAccount"`
	RecurringDelegation string `json:"recurringDelegation"`
	VaultIndex          int    `json:"vaultIndex"`
}

// autodepositCloseInput is the parsed close body, mirroring
// ConfirmedEarnAutodepositCloseInput (BigInt fields become uint64).
type autodepositCloseInput struct {
	Cluster             string
	CloseSignature      string
	ConfirmedSlot       uint64
	DelegatedSigner     string
	PolicyAccount       string
	RecurringDelegation string
	Settings            string
	VaultIndex          int
	VaultPubkey         string
	WalletAddress       string
}

type autodepositFloorInput struct {
	PolicyAccount         string
	RecurringDelegation   string
	Settings              string
	VaultIndex            int
	WalletAddress         string
	WalletBalanceFloorRaw uint64
}

type autodepositToggleInput struct {
	Active              bool
	PolicyAccount       string
	RecurringDelegation string
	Settings            string
	VaultIndex          int
	WalletAddress       string
}

// autodepositBodyObject ports assertRequestObject
// (prepare-contracts.shared.ts:190): anything but a JSON object fails with
// the same message.
func autodepositBodyObject(raw []byte) (map[string]any, error) {
	parsed, err := parseJSONBody(raw)
	if err != nil {
		return nil, errors.New("Request body must be an object.")
	}
	object, ok := parsed.(map[string]any)
	if !ok {
		return nil, errors.New("Request body must be an object.")
	}
	return object, nil
}

// readAutodepositVaultIndex ports readVaultIndex
// (prepare-contracts.shared.ts:252): the JSON number must be exactly 1.
func readAutodepositVaultIndex(object map[string]any, into *int) error {
	if value, ok := object["vaultIndex"]; !ok || value != float64(1) {
		return errors.New("vaultIndex must be 1 for Earn autodeposit.")
	}
	*into = 1
	return nil
}

// parseAutodepositCloseConfirmBody ports
// parseEarnAutodepositCloseConfirmRequestBody
// (earn-autodeposit-prepare-contracts.shared.ts:500): fields evaluate in the
// TS object-literal order and every failure names its key, so the first bad
// field matches Next's error message exactly.
func parseAutodepositCloseConfirmBody(raw []byte) (autodepositCloseInput, error) {
	object, err := autodepositBodyObject(raw)
	if err != nil {
		return autodepositCloseInput{}, err
	}
	in := autodepositCloseInput{}
	read := func() error {
		if err := readAutodepositString(object, "cluster", &in.Cluster); err != nil {
			return err
		}
		if err := readAutodepositString(object, "closeSignature", &in.CloseSignature); err != nil {
			return err
		}
		if err := readAutodepositUint(object, "confirmedSlot", &in.ConfirmedSlot); err != nil {
			return err
		}
		if err := readAutodepositString(object, "delegatedSigner", &in.DelegatedSigner); err != nil {
			return err
		}
		if err := readAutodepositString(object, "policyAccount", &in.PolicyAccount); err != nil {
			return err
		}
		if err := readAutodepositString(object, "recurringDelegation", &in.RecurringDelegation); err != nil {
			return err
		}
		if err := readAutodepositString(object, "settings", &in.Settings); err != nil {
			return err
		}
		if err := readAutodepositVaultIndex(object, &in.VaultIndex); err != nil {
			return err
		}
		if err := readAutodepositString(object, "vaultPubkey", &in.VaultPubkey); err != nil {
			return err
		}
		return readAutodepositString(object, "walletAddress", &in.WalletAddress)
	}
	if err := read(); err != nil {
		return autodepositCloseInput{}, err
	}
	return in, nil
}

// parseAutodepositFloorConfirmBody ports
// parseEarnAutodepositFloorUpdateConfirmRequestBody
// (earn-autodeposit-prepare-contracts.shared.ts:370).
func parseAutodepositFloorConfirmBody(raw []byte) (autodepositFloorInput, error) {
	object, err := autodepositBodyObject(raw)
	if err != nil {
		return autodepositFloorInput{}, err
	}
	in := autodepositFloorInput{}
	read := func() error {
		if err := readAutodepositString(object, "policyAccount", &in.PolicyAccount); err != nil {
			return err
		}
		if err := readAutodepositString(object, "recurringDelegation", &in.RecurringDelegation); err != nil {
			return err
		}
		if err := readAutodepositVaultIndex(object, &in.VaultIndex); err != nil {
			return err
		}
		return readAutodepositUint(object, "walletBalanceFloorRaw", &in.WalletBalanceFloorRaw)
	}
	if err := read(); err != nil {
		return autodepositFloorInput{}, err
	}
	return in, nil
}

// parseAutodepositToggleConfirmBody ports
// parseEarnAutodepositToggleConfirmRequestBody
// (earn-autodeposit-prepare-contracts.shared.ts:387).
func parseAutodepositToggleConfirmBody(raw []byte) (autodepositToggleInput, error) {
	object, err := autodepositBodyObject(raw)
	if err != nil {
		return autodepositToggleInput{}, err
	}
	in := autodepositToggleInput{}
	read := func() error {
		// readRequiredBoolean (prepare-contracts.shared.ts:241).
		active, isBool := object["active"].(bool)
		if !isBool {
			return errors.New("active must be a boolean.")
		}
		in.Active = active
		if err := readAutodepositString(object, "policyAccount", &in.PolicyAccount); err != nil {
			return err
		}
		if err := readAutodepositString(object, "recurringDelegation", &in.RecurringDelegation); err != nil {
			return err
		}
		return readAutodepositVaultIndex(object, &in.VaultIndex)
	}
	if err := read(); err != nil {
		return autodepositToggleInput{}, err
	}
	return in, nil
}
