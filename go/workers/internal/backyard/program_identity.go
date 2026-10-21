package backyard

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/voltr"
)

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

	voltrProgramDataAddress   = "3fiAyUjktZkZf6hcbBPy6U6UdkMdEFoToS4sjtzAd5az"
	voltrProgramDataSHA256    = "bf1c1831b3d6350f4340badb942bd2e7bfaca4aa89276cb65e8480aa30d44c56"
	voltrProgramDeploySlot    = int64(445223838)
	adaptorProgramDataAddress = "DrvzixaVmAuPVVJPtP5wykb9mvgDWqZbvZau9oiCUpHu"
	adaptorProgramDataSHA256  = "8361a469833fa17df8f62f9c4b8055aa859552fe8db7610b8fcb3af6ac6eb6d5"
	adaptorProgramDeploySlot  = int64(443528877)
)

type pinnedProgramIdentity struct {
	program, programData, dataSHA256 string
	deploySlot                       int64
}

var pinnedProgramIdentities = []pinnedProgramIdentity{
	{program: voltr.ProgramID.String(), programData: voltrProgramDataAddress,
		deploySlot: voltrProgramDeploySlot, dataSHA256: voltrProgramDataSHA256},
	{program: fsj27Program, programData: adaptorProgramDataAddress,
		deploySlot: adaptorProgramDeploySlot, dataSHA256: adaptorProgramDataSHA256},
}

// programIdentityReader returns the pinned program and ProgramData accounts;
// an absent one carries its address alone.
type programIdentityReader func(ctx context.Context) ([]ConfirmedAccount, error)

func viewProgramIdentity(view *View) programIdentityReader {
	return func(ctx context.Context) ([]ConfirmedAccount, error) {
		addresses := []string{voltr.ProgramID.String(), fsj27Program, voltrProgramDataAddress, adaptorProgramDataAddress}
		_, accounts, err := view.read(ctx, addresses, 0, addresses...)
		return accounts, err
	}
}

type programIdentityObservation struct {
	Verified                 bool
	VoltrProgramDeploySlot   int64
	AdaptorProgramDeploySlot int64
}

type programIdentityWatcher struct {
	reader       programIdentityReader
	lastVerified map[string]int64
}

func newProgramIdentityWatcher(reader programIdentityReader) *programIdentityWatcher {
	return &programIdentityWatcher{reader: reader, lastVerified: map[string]int64{}}
}

// observe verifies the pinned program identity. A ProgramData image is hashed
// whenever its deploy slot was not already verified. Absence, a wrong owner,
// a wrong discriminant, a moved slot, or a hash mismatch returns
// Verified=false and no error, so the caller records a durable
// program_identity_unverified hold instead of failing the tick; only an
// unavailable view is an error.
func (w *programIdentityWatcher) observe(ctx context.Context) (programIdentityObservation, error) {
	observation := programIdentityObservation{}
	images, err := w.reader(ctx)
	if err != nil {
		return observation, err
	}
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
		case voltr.ProgramID.String():
			observation.VoltrProgramDeploySlot = slot
		case fsj27Program:
			observation.AdaptorProgramDeploySlot = slot
		}
		if w.lastVerified[pin.program] != slot {
			if _, err := verifyProgramDataImage(data, pin); err != nil {
				return observation, nil
			}
			w.lastVerified[pin.program] = slot
		}
	}
	observation.Verified = true
	return observation, nil
}

func identityImage(images []ConfirmedAccount, address string) (ConfirmedAccount, error) {
	for _, image := range images {
		if image.Address == address {
			return image, nil
		}
	}
	return ConfirmedAccount{}, fmt.Errorf("program identity account %s is absent", address)
}

func decodeProgramIdentityHeader(image ConfirmedAccount) (string, error) {
	if image.Owner != bpfLoaderProgramOwner || !image.Executable || image.Lamports == 0 ||
		len(image.Data) != programHeaderLength {
		return "", fmt.Errorf("account %s is not a live upgradeable program", image.Address)
	}
	if binary.LittleEndian.Uint32(image.Data[0:4]) != programAccountDiscriminant {
		return "", fmt.Errorf("account %s is not an upgradeable program header", image.Address)
	}
	return encodeBase58(image.Data[4:36]), nil
}

func decodeProgramDataSlot(image ConfirmedAccount) (int64, error) {
	if err := validateProgramDataAccount(image, programDataHeaderLength); err != nil {
		return 0, err
	}
	slot := int64(binary.LittleEndian.Uint64(image.Data[4:12]))
	if slot <= 0 {
		return 0, fmt.Errorf("program data account %s has an invalid deploy slot", image.Address)
	}
	return slot, nil
}

func verifyProgramDataImage(image ConfirmedAccount, pin pinnedProgramIdentity) (int64, error) {
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

func validateProgramDataAccount(image ConfirmedAccount, minLength int) error {
	if image.Owner != bpfLoaderProgramOwner || image.Lamports == 0 || len(image.Data) < minLength {
		return fmt.Errorf("account %s is not an upgradeable program data account", image.Address)
	}
	if binary.LittleEndian.Uint32(image.Data[0:4]) != programDataDiscriminant {
		return fmt.Errorf("account %s is not an upgradeable program data header", image.Address)
	}
	return nil
}
