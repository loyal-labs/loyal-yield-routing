package backyardrwa

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
)

// The upgradeable-loader identity this worker accepts. Any other owner, a
// different account discriminant, a moved deploy slot, or a ProgramData image
// that no longer hashes to its pin leaves the program identity unverified and
// stops the route for manual recovery.
const (
	bpfLoaderProgramOwner      = "BPFLoaderUpgradeab1e11111111111111111111111"
	programAccountDiscriminant = uint32(2)
	programDataDiscriminant    = uint32(3)
	programHeaderLength        = 36
	programDataHeaderLength    = 12
	// programDataExecutableOffset is where the executable bytes start: a u32
	// discriminant, a u64 deploy slot, a u8 option byte, and a 32-byte upgrade
	// authority. The hash pin covers only what follows, so changing the
	// authority changes nothing about the reviewed binary and every external
	// tool can reproduce the digest with sha256(data[45:]).
	programDataExecutableOffset = 45
)

// pinnedProgramIdentity is the reviewed binary of one upgradeable program: the
// ProgramData address it must point at, the slot that data deployed at, and the
// sha256 of the executable bytes ProgramData.data[45:].
type pinnedProgramIdentity struct {
	program, programData, dataSHA256 string
	deploySlot                       int64
}

var pinnedProgramIdentities = []pinnedProgramIdentity{
	{program: bridgeVoltrProgram, programData: voltrProgramDataAddress,
		deploySlot: voltrProgramDeploySlot, dataSHA256: voltrProgramDataSHA256},
	{program: bridgeAdaptorProgram, programData: adaptorProgramDataAddress,
		deploySlot: adaptorProgramDeploySlot, dataSHA256: adaptorProgramDataSHA256},
}

// programIdentityImage is one raw loader account read.
type programIdentityImage struct {
	Address    string
	Owner      string
	Lamports   uint64
	Executable bool
	Data       []byte
}

// programIdentityReader is the confirmed-state seam behind M6.
type programIdentityReader interface {
	// programIdentityAccounts returns the pinned program header accounts and
	// their ProgramData accounts: 12 bytes each, or the full image when full is
	// set. An absent account is simply missing from the result.
	programIdentityAccounts(ctx context.Context, full bool) ([]programIdentityImage, error)
}

// programIdentityObservation is what the monitors arm M6 from.
type programIdentityObservation struct {
	Verified                 bool
	VoltrProgramDeploySlot   int64
	AdaptorProgramDeploySlot int64
}

// programIdentityWatcher remembers which deploy slots were already verified
// against the full ProgramData hash, so a tick pays for a full image read only
// at worker start and after an observed slot move.
type programIdentityWatcher struct {
	reader       programIdentityReader
	lastVerified map[string]int64
}

func newProgramIdentityWatcher(reader programIdentityReader) *programIdentityWatcher {
	return &programIdentityWatcher{reader: reader, lastVerified: map[string]int64{}}
}

// observe verifies the pinned program identity. Per tick it reads only the
// program and ProgramData headers; the full ProgramData is fetched and hashed
// whenever the observed slot was not already verified. Absence, a wrong owner,
// a wrong discriminant, a moved slot, or a hash mismatch returns Verified=false
// and no error, so the caller records a durable program_identity_unverified
// hold instead of failing the tick; only a transport failure is an error.
func (w *programIdentityWatcher) observe(ctx context.Context) (programIdentityObservation, error) {
	observation := programIdentityObservation{}
	images, err := w.reader.programIdentityAccounts(ctx, false)
	if err != nil {
		return observation, err
	}
	rehash := false
	for _, pin := range pinnedProgramIdentities {
		program, err := identityImage(images, pin.program)
		if err != nil {
			return observation, nil
		}
		programDataAddress, err := decodeProgramIdentityHeader(program)
		if err != nil || programDataAddress != pin.programData {
			return observation, nil
		}
		data, err := identityImage(images, pin.programData)
		if err != nil {
			return observation, nil
		}
		slot, err := decodeProgramDataSlot(data)
		if err != nil {
			return observation, nil
		}
		switch pin.program {
		case bridgeVoltrProgram:
			observation.VoltrProgramDeploySlot = slot
		case bridgeAdaptorProgram:
			observation.AdaptorProgramDeploySlot = slot
		}
		if w.lastVerified[pin.program] != slot {
			rehash = true
		}
	}
	if rehash {
		images, err = w.reader.programIdentityAccounts(ctx, true)
		if err != nil {
			return observation, err
		}
		for _, pin := range pinnedProgramIdentities {
			data, err := identityImage(images, pin.programData)
			if err != nil {
				return observation, nil
			}
			slot, err := verifyProgramDataImage(data, pin)
			if err != nil {
				return observation, nil
			}
			w.lastVerified[pin.program] = slot
		}
	}
	observation.Verified = true
	return observation, nil
}

func identityImage(images []programIdentityImage, address string) (programIdentityImage, error) {
	for _, image := range images {
		if image.Address == address {
			return image, nil
		}
	}
	return programIdentityImage{}, fmt.Errorf("program identity account %s is absent", address)
}

// decodeProgramIdentityHeader validates a 36-byte upgradeable program account:
// a u32 variant of 2 followed by the 32-byte ProgramData address.
func decodeProgramIdentityHeader(image programIdentityImage) (string, error) {
	if image.Owner != bpfLoaderProgramOwner || !image.Executable || image.Lamports == 0 ||
		len(image.Data) != programHeaderLength {
		return "", fmt.Errorf("account %s is not a live upgradeable program", image.Address)
	}
	if binary.LittleEndian.Uint32(image.Data[0:4]) != programAccountDiscriminant {
		return "", fmt.Errorf("account %s is not an upgradeable program header", image.Address)
	}
	return encodeBase58(image.Data[4:36]), nil
}

// decodeProgramDataSlot validates a 12-byte ProgramData header: a u32 variant
// of 3 followed by the u64 last-deploy slot.
func decodeProgramDataSlot(image programIdentityImage) (int64, error) {
	if err := validateProgramDataAccount(image, programDataHeaderLength); err != nil {
		return 0, err
	}
	slot := int64(binary.LittleEndian.Uint64(image.Data[4:12]))
	if slot <= 0 {
		return 0, fmt.Errorf("program data account %s has an invalid deploy slot", image.Address)
	}
	return slot, nil
}

// verifyProgramDataImage hashes the executable ProgramData bytes (data[45:])
// against its pin. The 45-byte loader header is excluded: it carries the
// upgrade authority, which can rotate without the binary changing.
func verifyProgramDataImage(image programIdentityImage, pin pinnedProgramIdentity) (int64, error) {
	if err := validateProgramDataAccount(image, programDataExecutableOffset); err != nil {
		return 0, err
	}
	slot := int64(binary.LittleEndian.Uint64(image.Data[4:12]))
	if slot != pin.deploySlot {
		return 0, fmt.Errorf("program data account %s deployed at slot %d, pinned %d", image.Address, slot, pin.deploySlot)
	}
	digest := sha256.Sum256(image.Data[programDataExecutableOffset:])
	if hex.EncodeToString(digest[:]) != pin.dataSHA256 {
		return 0, fmt.Errorf("program data account %s does not hash to its pinned sha256", image.Address)
	}
	return slot, nil
}

func validateProgramDataAccount(image programIdentityImage, minLength int) error {
	if image.Owner != bpfLoaderProgramOwner || image.Lamports == 0 || len(image.Data) < minLength {
		return fmt.Errorf("account %s is not an upgradeable program data account", image.Address)
	}
	if binary.LittleEndian.Uint32(image.Data[0:4]) != programDataDiscriminant {
		return fmt.Errorf("account %s is not an upgradeable program data header", image.Address)
	}
	return nil
}
