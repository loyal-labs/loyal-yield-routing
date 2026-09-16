package backyardrwa

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"testing"
	"time"
)

// The production observe path is what arms the fail-closed monitors, so the
// review findings are proven through it: the stubs below replace only the three
// readers (confirmed batch, reconciled journal, pinned program identity) and
// productionObserveState.observe performs the real merge before Decide.
type stubProductionJournal struct {
	postMutation           bool
	journal                ReconciledBridgeJournalState
	positionSnapshotWrites int
}

func (s *stubProductionJournal) PostMutationNAVRequired(context.Context, string) (bool, error) {
	return s.postMutation, nil
}

func (s *stubProductionJournal) ReconciledBridgeJournal(context.Context, string) (ReconciledBridgeJournalState, error) {
	return s.journal, nil
}

func (s *stubProductionJournal) RecordPositionSnapshot(_ context.Context, _ string, _ Observation) error {
	s.positionSnapshotWrites++
	return nil
}

func productionConfirmedBatch(t *testing.T, slot int64, mutate func([]ConfirmedAccount)) func(context.Context) (Observation, error) {
	t.Helper()
	return productionConfirmedBatchForManifest(t, readyWorkerManifest(t), slot, mutate)
}

// productionConfirmedBatchForManifest pins the manifest too, so a test can
// observe a non-cutover lane through the identical production merge.
// productionRouteBatchAccounts builds the confirmed account images the
// production observe path decodes. Both the in-memory runtime and the raw
// JSON-RPC stub feed the same images, so tests cannot drift from the wire.
func productionRouteBatchAccounts(t *testing.T, slot int64, mutate func([]ConfirmedAccount)) []ConfirmedAccount {
	t.Helper()
	accounts := routeNAVFixture(t, slot)
	accounts = append(accounts, exactReportTicketAccount(t, 4))
	binary.LittleEndian.PutUint64(accountAt(accounts, bridgeStrategyATA).Data[64:72], 0)
	binary.LittleEndian.PutUint64(accountAt(accounts, bridgeVoltrVault).Data[168:176], 53)
	// The full observation path validates the Kamino reserve oracles that the
	// NAV-only path ignores, so the batch carries one configured oracle for
	// both reserves plus the oracle account itself.
	putKey(t, accountAt(accounts, kaminoCollateralReserve).Data[5112:5144], kaminoPrimeMint)
	putKey(t, accountAt(accounts, kaminoDebtReserve).Data[5112:5144], kaminoPrimeMint)
	accounts = append(accounts, ConfirmedAccount{Address: kaminoPrimeMint, Owner: kaminoProgram, Lamports: 1, Data: []byte{1}})
	// The production observer validates oracle age against chain time from the
	// same confirmed batch, so keep the Clock image beside the reserve images.
	accounts = append(accounts, clockFixture())
	// A live reserve carries an 80% liquidation threshold; the decision engine
	// refuses to plan around a position without one.
	accountAt(accounts, kaminoCollateralReserve).Data[kaminoReserveConfigOffset+17] = 80
	if mutate != nil {
		mutate(accounts)
	}
	return accounts
}

// fixtureBatchRuntime supplies confirmed and finalized readers over one account
// list. A removed account is absent from both commitments, which is exactly
// what the finalized gate needs to see before absence arms the hold.
func fixtureBatchRuntime(slot int64, accounts []ConfirmedAccount) (func(context.Context, []string, int64) (int64, []ConfirmedAccount, error), func(context.Context, int64) (int64, []ConfirmedAccount, error)) {
	lookup := func(addresses []string) []ConfirmedAccount {
		observed := make([]ConfirmedAccount, 0, len(addresses))
		for _, address := range addresses {
			for _, account := range accounts {
				if account.Address == address {
					observed = append(observed, account)
					break
				}
			}
		}
		return observed
	}
	return func(_ context.Context, addresses []string, _ int64) (int64, []ConfirmedAccount, error) {
			return slot, lookup(addresses), nil
		}, func(_ context.Context, _ int64) (int64, []ConfirmedAccount, error) {
			return slot, lookup([]string{bridgeStrategyReceipt}), nil
		}
}

func productionConfirmedBatchForManifest(t *testing.T, manifest RouteManifest, slot int64, mutate func([]ConfirmedAccount)) func(context.Context) (Observation, error) {
	t.Helper()
	return func(ctx context.Context) (Observation, error) {
		accounts := productionRouteBatchAccounts(t, slot, mutate)
		accountsReader, finalizedReceipt := fixtureBatchRuntime(slot, accounts)
		observation, _, err := observeConfirmedRouteSnapshotWithAccounts(ctx, manifest, routeObservationRuntime{
			confirmedSlot: func(context.Context) (int64, error) { return slot, nil },
			receipts: func(_ context.Context, _ int64) (int64, []programAccount, error) {
				// No open withdrawal receipts, so the queue demand is zero.
				return slot, nil, nil
			},
			accounts:         accountsReader,
			finalizedReceipt: finalizedReceipt,
			now:              func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
		})
		if err != nil {
			return Observation{}, err
		}
		return observation, nil
	}
}

func pinnedIdentityObservation(context.Context) (programIdentityObservation, error) {
	return programIdentityObservation{Verified: true,
		VoltrProgramDeploySlot: voltrProgramDeploySlot, AdaptorProgramDeploySlot: adaptorProgramDeploySlot}, nil
}

// replaceAccount swaps one batch account in place, so a test can remove or
// truncate an account the production readers will request.
func replaceAccount(accounts []ConfirmedAccount, address string, account ConfirmedAccount) {
	for i := range accounts {
		if accounts[i].Address == address {
			accounts[i] = account
			return
		}
	}
}

func productionDecision(t *testing.T, journal ReconciledBridgeJournalState, postMutation bool,
	mutate func([]ConfirmedAccount), identity func(context.Context) (programIdentityObservation, error)) Decision {
	t.Helper()
	return productionDecisionForManifest(t, readyWorkerManifest(t), journal, postMutation, mutate, identity)
}

func productionDecisionForManifest(t *testing.T, manifest RouteManifest, journal ReconciledBridgeJournalState, postMutation bool,
	mutate func([]ConfirmedAccount), identity func(context.Context) (programIdentityObservation, error)) Decision {
	t.Helper()
	state := productionObserveState{
		routeKey: "rwa-multiply:test",
		journal:  &stubProductionJournal{postMutation: postMutation, journal: journal},
		batch:    productionConfirmedBatchForManifest(t, manifest, 77, mutate),
		identity: identity,
	}
	observation, err := state.observe(context.Background())
	if err != nil {
		t.Fatalf("production observe failed: %v", err)
	}
	return Decide(observation.Snapshot)
}

// reconciledJournal is a route at rest: the last ticket-consuming operation was
// this worker's own report, it carried the adaptor return data that armed the
// receipt, and nothing reconciled after it.
func reconciledJournal() ReconciledBridgeJournalState {
	return ReconciledBridgeJournalState{
		TicketSequenceKnown: true, TicketSequenceRaw: 4,
		ArmedNAVKnown: true, ArmedNAVRaw: 42,
	}
}

// TestProductionStageTransientIsJournaledNotTracked is the blocker 1 proof
// through the production path: a stage moves Squads cash into the custody ATA
// without invoking Voltr, so the batch still shows tv 53 and a zero tracked
// custody while the ATA holds the staged amount.
func TestProductionStageTransientIsJournaledNotTracked(t *testing.T) {
	staged := reconciledJournal()
	staged.StagedAmountKnown, staged.StagedAmountRaw, staged.StageAfterTicket = true, 7, true
	staged.MutationAfterReport = true
	stageCustody := func(accounts []ConfirmedAccount) {
		binary.LittleEndian.PutUint64(accountAt(accounts, bridgeStrategyATA).Data[64:72], 7)
	}
	inFlight := productionDecision(t, staged, false, stageCustody, pinnedIdentityObservation)
	if inFlight.Reason == "custody_mismatch" || inFlight.Reason == "custody_transient_mismatch" {
		t.Fatalf("a journaled stage transient was booked as a custody fault: %+v", inFlight)
	}
	// The same custody balance is unexplainable without the journal row. Custody
	// discipline runs before the monitors, so it names the mismatch, while the
	// monitor's at-rest residue variant stays covered by the monitor tests.
	if got := productionDecision(t, reconciledJournal(), false, stageCustody, pinnedIdentityObservation); got.Action != HoldManualRecovery || got.Reason != "custody_mismatch" {
		t.Fatalf("custody without a journaled stage did not hold: %+v", got)
	}
}

// TestProductionArmedNAVRequiresReconciledReturnData is the major 4 proof: the
// armed NAV is the adaptor return data of the latest reconciled ticket-consuming
// operation, so a row without it holds durably instead of disarming the receipt
// monitor.
func TestProductionArmedNAVRequiresReconciledReturnData(t *testing.T) {
	missing := reconciledJournal()
	missing.ArmedNAVKnown, missing.ArmedNAVReturnDataMissing = false, true
	if got := productionDecision(t, missing, false, nil, pinnedIdentityObservation); got.Action != HoldManualRecovery || got.Reason != "armed_nav_missing" {
		t.Fatalf("a reconciled operation without adaptor return data did not hold: %+v", got)
	}
	malformed := reconciledJournal()
	malformed.ArmedNAVKnown, malformed.ArmedNAVMalformed = false, true
	if got := productionDecision(t, malformed, false, nil, pinnedIdentityObservation); got.Action != HoldManualRecovery || got.Reason != "armed_nav_malformed" {
		t.Fatalf("an unreadable armed NAV did not hold: %+v", got)
	}
}

// TestProductionStageIsNotTicketConsumption is the blocker 3 proof: a
// reconciled stage is newer than the last ticket-consuming operation, and the
// ticket monitor must not read that stage as the consumed sequence.
func TestProductionStageIsNotTicketConsumption(t *testing.T) {
	staged := reconciledJournal()
	staged.StagedAmountKnown, staged.StagedAmountRaw, staged.StageAfterTicket = true, 7, true
	got := productionDecision(t, staged, false, func(accounts []ConfirmedAccount) {
		binary.LittleEndian.PutUint64(accountAt(accounts, bridgeStrategyATA).Data[64:72], 7)
	}, pinnedIdentityObservation)
	if got.Reason == "out_of_band_crank" {
		t.Fatalf("a reconciled stage was counted as ticket consumption: %+v", got)
	}
	outOfBand := reconciledJournal()
	outOfBand.TicketSequenceRaw = 5
	if got := productionDecision(t, outOfBand, false, nil, pinnedIdentityObservation); got.Action != HoldManualRecovery || got.Reason != "out_of_band_crank" {
		t.Fatalf("a ticket consumed outside the journal did not hold: %+v", got)
	}
}

// TestProductionUnverifiedProgramIdentityHolds is the major 5 proof: an absent
// or incoherent identity read reaches Decide as an unverified program and holds
// the route durably instead of failing the tick.
func TestProductionUnverifiedProgramIdentityHolds(t *testing.T) {
	unverified := func(context.Context) (programIdentityObservation, error) {
		return programIdentityObservation{Verified: false, VoltrProgramDeploySlot: voltrProgramDeploySlot + 1}, nil
	}
	if got := productionDecision(t, reconciledJournal(), false, nil, unverified); got.Action != HoldManualRecovery || got.Reason != "program_identity_unverified" {
		t.Fatalf("an unverified program identity did not hold the route: %+v", got)
	}
}

// TestProductionStrategyReceiptIntegrityHoldsDurably proves the receipt
// integrity rule on the production observe path: a confirmed batch whose
// strategy receipt is absent or the wrong length persists a durable
// strategy_receipt_integrity hold, while a transport failure stays a tick
// error and never becomes a decision.
func TestProductionStrategyReceiptIntegrityHoldsDurably(t *testing.T) {
	absent := func(accounts []ConfirmedAccount) {
		replaceAccount(accounts, bridgeStrategyReceipt, ConfirmedAccount{})
	}
	if got := productionDecision(t, reconciledJournal(), false, absent, pinnedIdentityObservation); got.Action != HoldManualRecovery || got.Reason != "strategy_receipt_integrity" {
		t.Fatalf("an absent strategy receipt did not hold durably: %+v", got)
	}
	truncated := func(accounts []ConfirmedAccount) {
		replaceAccount(accounts, bridgeStrategyReceipt, ConfirmedAccount{
			Address: bridgeStrategyReceipt, Owner: bridgeVoltrProgram, Lamports: 1, Data: make([]byte, 100)})
	}
	if got := productionDecision(t, reconciledJournal(), false, truncated, pinnedIdentityObservation); got.Action != HoldManualRecovery || got.Reason != "strategy_receipt_integrity" {
		t.Fatalf("a truncated strategy receipt did not hold durably: %+v", got)
	}
	state := productionObserveState{
		routeKey: "rwa-multiply:test",
		journal:  &stubProductionJournal{journal: reconciledJournal()},
		batch: func(context.Context) (Observation, error) {
			return Observation{}, fmt.Errorf("getMultipleAccounts: transport unavailable")
		},
		identity: pinnedIdentityObservation,
	}
	if _, err := state.observe(context.Background()); err == nil {
		t.Fatal("a transport failure must stay a tick error, not a hold")
	}
}

// TestProductionUnexplainedDriftHoldsThroughObserve is the S1 acceptance proof
// on the production observe path. The batch observes the PRIME/USDC lane
// directly instead of the selected cutover lane, and carries no PRIME
// position, so no unwind leg outranks the drift gate: an unexplained NAV move
// holds, and the same healthy batch without the move does not.
func TestProductionUnexplainedDriftHoldsThroughObserve(t *testing.T) {
	manifest := readyWorkerManifest(t)
	manifest.RuntimeActivation.SelectedLane = ""
	nonCutover := func(accounts []ConfirmedAccount) {
		config, err := pinnedKaminoObservationConfig()
		if err != nil {
			t.Fatal(err)
		}
		obligation := accountAt(accounts, config.Obligation)
		for i := 96; i < 136; i++ {
			obligation.Data[i] = 0
		}
		for i := 1208; i < 1272; i++ {
			obligation.Data[i] = 0
		}
	}
	unexplained := func(accounts []ConfirmedAccount) {
		nonCutover(accounts)
		binary.LittleEndian.PutUint64(accountAt(accounts, bridgeSquadsATA).Data[64:72], 20_000)
	}
	got := productionDecisionForManifest(t, manifest, reconciledJournal(), false, unexplained, pinnedIdentityObservation)
	if got.Action != HoldManualRecovery || got.Reason != "nav_drift_unexplained" {
		t.Fatalf("unexplained NAV drift was reported instead of held: %+v", got)
	}
	healthy := productionDecisionForManifest(t, manifest, reconciledJournal(), false, nonCutover, pinnedIdentityObservation)
	if healthy.Action == HoldManualRecovery {
		t.Fatalf("the flat non-cutover batch held without a drift: %+v", healthy)
	}
}

// TestProductionPerformanceFeeTermsHold is the major 6 proof: the decoded
// performance-fee bps fields reach the snapshot, and either one switched on
// stops the route until the terms are reviewed.
func TestProductionPerformanceFeeTermsHold(t *testing.T) {
	for _, tc := range []struct {
		name   string
		offset int
	}{
		{"manager performance fee", 512},
		{"admin performance fee", 514},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := productionDecision(t, reconciledJournal(), false, func(accounts []ConfirmedAccount) {
				binary.LittleEndian.PutUint16(accountAt(accounts, bridgeVoltrVault).Data[tc.offset:tc.offset+2], 250)
			}, pinnedIdentityObservation)
			if got.Action != HoldManualRecovery || got.Reason != "performance_fee_enabled" {
				t.Fatalf("%s did not hold the route: %+v", tc.name, got)
			}
		})
	}
}

type stubIdentityReader struct {
	images    []programIdentityImage
	fullReads int
}

func (s *stubIdentityReader) programIdentityAccounts(_ context.Context, full bool) ([]programIdentityImage, error) {
	if full {
		s.fullReads++
	}
	return s.images, nil
}

func pinnedIdentityHeaders(t *testing.T, slots map[string]int64) []programIdentityImage {
	t.Helper()
	images := make([]programIdentityImage, 0, 2*len(pinnedProgramIdentities))
	for _, pin := range pinnedProgramIdentities {
		programData, err := decodeBase58PublicKey(pin.programData)
		if err != nil {
			t.Fatal(err)
		}
		program := make([]byte, programHeaderLength)
		binary.LittleEndian.PutUint32(program[0:4], programAccountDiscriminant)
		copy(program[4:36], programData[:])
		slot := slots[pin.program]
		if slot == 0 {
			slot = pin.deploySlot
		}
		// A full ProgramData image: header (discriminant, slot, option,
		// authority) followed by executable bytes that do not hash to the
		// reviewed pin, so a full read can only fail on the digest.
		data := make([]byte, programDataExecutableOffset+16)
		binary.LittleEndian.PutUint32(data[0:4], programDataDiscriminant)
		binary.LittleEndian.PutUint64(data[4:12], uint64(slot))
		data[12] = 1
		for i := programDataExecutableOffset; i < len(data); i++ {
			data[i] = byte(i)
		}
		images = append(images,
			programIdentityImage{Address: pin.program, Owner: bpfLoaderProgramOwner, Lamports: 1, Executable: true, Data: program},
			programIdentityImage{Address: pin.programData, Owner: bpfLoaderProgramOwner, Lamports: 1, Data: data})
	}
	return images
}

// TestProgramIdentityWatcherVerifiesFullImageOnSlotMove covers the major 5
// watcher: headers alone never verify, an observed slot forces exactly one full
// image read per attempt, and a hash mismatch never advances the verified slot,
// so the route keeps holding instead of silently trusting the new binary.
func TestProgramIdentityWatcherVerifiesFullImageOnSlotMove(t *testing.T) {
	reader := &stubIdentityReader{images: pinnedIdentityHeaders(t, nil)}
	watcher := newProgramIdentityWatcher(reader)
	observation, err := watcher.observe(context.Background())
	if err != nil || observation.Verified || observation.VoltrProgramDeploySlot != voltrProgramDeploySlot {
		t.Fatalf("pinned headers must force a full verification: observation=%+v err=%v", observation, err)
	}
	if reader.fullReads != 1 {
		t.Fatalf("the watcher read %d full images, want 1", reader.fullReads)
	}
	// A full image that does not hash to its pin never counts as verified, so
	// the next tick pays for the verification again instead of trusting it.
	if _, err := watcher.observe(context.Background()); err != nil {
		t.Fatal(err)
	}
	if reader.fullReads != 2 {
		t.Fatalf("an unverified slot must be re-verified: full reads=%d", reader.fullReads)
	}

	absent := &stubIdentityReader{images: pinnedIdentityHeaders(t, nil)[:2]}
	if observation, err := newProgramIdentityWatcher(absent).observe(context.Background()); err != nil || observation.Verified {
		t.Fatalf("an absent program identity must stay unverified: observation=%+v err=%v", observation, err)
	}
	if absent.fullReads != 0 {
		t.Fatal("an unreadable program header must not spend a full image read")
	}

	moved := &stubIdentityReader{images: pinnedIdentityHeaders(t, map[string]int64{bridgeVoltrProgram: voltrProgramDeploySlot + 1})}
	if observation, err := newProgramIdentityWatcher(moved).observe(context.Background()); err != nil || observation.Verified || observation.VoltrProgramDeploySlot != voltrProgramDeploySlot+1 {
		t.Fatalf("a moved deploy slot must stay unverified: observation=%+v err=%v", observation, err)
	}
}

// TestProgramDataImageVerificationChecksOwnerDiscriminatorSlotAndHash pins the
// byte-level acceptance rule of a ProgramData image: the digest covers only
// the executable bytes past the 45-byte loader header, so rotating the upgrade
// authority cannot look like a new binary while changing one executable byte
// must.
func TestProgramDataImageVerificationChecksOwnerDiscriminatorSlotAndHash(t *testing.T) {
	data := make([]byte, 512)
	binary.LittleEndian.PutUint32(data[0:4], programDataDiscriminant)
	binary.LittleEndian.PutUint64(data[4:12], 445223838)
	data[12] = 1
	for i := programDataExecutableOffset; i < len(data); i++ {
		data[i] = byte(i)
	}
	digest := sha256.Sum256(data[programDataExecutableOffset:])
	pin := pinnedProgramIdentity{program: "program", programData: "data",
		deploySlot: 445223838, dataSHA256: hex.EncodeToString(digest[:])}
	image := programIdentityImage{Address: "data", Owner: bpfLoaderProgramOwner, Lamports: 1, Data: data}
	if slot, err := verifyProgramDataImage(image, pin); err != nil || slot != pin.deploySlot {
		t.Fatalf("the pinned image was rejected: slot=%d err=%v", slot, err)
	}
	authorityRotated := image
	authorityRotated.Data = append([]byte(nil), data...)
	authorityRotated.Data[20] ^= 0xff
	if slot, err := verifyProgramDataImage(authorityRotated, pin); err != nil || slot != pin.deploySlot {
		t.Fatalf("an upgrade-authority rotation changed the executable digest: slot=%d err=%v", slot, err)
	}
	executableChanged := image
	executableChanged.Data = append([]byte(nil), data...)
	executableChanged.Data[100] ^= 0xff
	if _, err := verifyProgramDataImage(executableChanged, pin); err == nil {
		t.Fatal("a changed executable byte was accepted")
	}
	if _, err := verifyProgramDataImage(image, pinnedProgramIdentity{program: "p", programData: "d",
		deploySlot: 445223839, dataSHA256: pin.dataSHA256}); err == nil {
		t.Fatal("a foreign deploy slot was accepted")
	}
	if _, err := verifyProgramDataImage(image, pinnedProgramIdentity{program: "p", programData: "d",
		deploySlot: 445223838, dataSHA256: hex.EncodeToString(digest[:]) + "00"}); err == nil {
		t.Fatal("a foreign image hash was accepted")
	}
	wrongOwner := image
	wrongOwner.Owner = bridgeTokenProgram
	if _, err := verifyProgramDataImage(wrongOwner, pin); err == nil {
		t.Fatal("a non-loader owner was accepted")
	}
	truncated := image
	truncated.Data = data[:8]
	if _, err := verifyProgramDataImage(truncated, pin); err == nil {
		t.Fatal("a truncated ProgramData header was accepted")
	}
}

func TestProductionHealthHoldPreservesLastGoodPositionProjection(t *testing.T) {
	journal := &stubProductionJournal{}
	state := productionObserveState{routeKey: productionRouteKey, journal: journal, identity: pinnedIdentityObservation, batch: func(context.Context) (Observation, error) {
		held, ok := KaminoHealthHoldObservation(errKaminoReserveStale, 42, time.Now().UTC())
		if !ok {
			t.Fatal("health hold unavailable")
		}
		return held, nil
	}}
	observation, err := state.observe(context.Background())
	if err != nil || observation.Snapshot.ManualReason != "kamino_stale" || journal.positionSnapshotWrites != 0 {
		t.Fatalf("unpriced hold tried to overwrite holdings: %+v %v", observation, err)
	}
	if got := Decide(observation.Snapshot); got.Action != HoldManualRecovery {
		t.Fatal(got)
	}
}
