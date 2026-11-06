package backyard

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/solana-foundation/solana-go/v2"
)

func TestForeignHistoryWorkerRecoversWithoutDuplicateCredit(t *testing.T) {
	for _, scenario := range []string{"same-slot observation", "checkpoint ahead", "confirmed receipt missing", "finalized receipt missing", "bounded catchup"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			const slot = foreignInflowAnchorSlot + 2
			batch := productionConfirmedBatch(t, slot, nil)
			initial, err := batch(ctx)
			if err != nil {
				t.Fatal(err)
			}
			address := rwa.idleATA
			base := uint64(initial.Snapshot.VoltrIdleRaw)
			anchor := foreignCheckpoint{Signature: foreignInflowAnchor, Slot: foreignInflowAnchorSlot, BalanceRaw: base}
			db := &foreignMemoryStore{checkpoints: map[string]foreignCheckpoint{
				address: anchor, rwa.strategyATA: {Signature: foreignInflowAnchor, Slot: foreignInflowAnchorSlot, BalanceRaw: uint64(initial.Snapshot.VoltrStrategyIdleRaw)},
			}}
			anchorRow := chain.Signed{Signature: solana.MustSignatureFromBase58(foreignInflowAnchor), Slot: foreignInflowAnchorSlot}
			c := &foreignChainFixture{finalized: slot, executions: map[solana.Signature]chain.Execution{}, history: map[solana.PublicKey][]chain.Signed{
				solanaKey(address): {anchorRow}, solanaKey(rwa.strategyATA): {anchorRow},
			}}
			gifts := 1
			if scenario == "bounded catchup" {
				gifts = 70
			}
			if scenario == "checkpoint ahead" {
				gifts = 0
				checkpoint := anchor
				checkpoint.Signature, checkpoint.Slot = (solana.Signature{2}).String(), slot+1
				db.checkpoints[address] = checkpoint
				c.history[solanaKey(address)] = []chain.Signed{{Signature: solana.Signature{2}, Slot: slot + 1}}
			}
			for i := 0; i < gifts; i++ {
				x := foreignFixture(address, base+uint64(i), 1)
				sig := solana.Signature{byte(i + 1)}
				x.Transaction.Signatures[0] = sig
				c.executions[sig] = x
				c.history[solanaKey(address)] = append([]chain.Signed{{Signature: sig, Slot: x.Slot}}, c.history[solanaKey(address)]...)
			}
			missing := c.executions[solana.Signature{1}]
			if scenario == "confirmed receipt missing" || scenario == "finalized receipt missing" {
				delete(c.executions, solana.Signature{1})
			}
			if scenario == "confirmed receipt missing" {
				c.finalized = foreignInflowAnchorSlot
			}
			journal := &stubProductionJournal{journal: reconciledJournal()}
			reads := 0
			state := productionObserveState{routeKey: productionRouteKey, journal: journal, identity: pinnedIdentityObservation,
				batch: func(ctx context.Context) (Observation, error) {
					o, err := batch(ctx)
					if scenario == "checkpoint ahead" && reads > 1 {
						o.Snapshot.Slot++
					}
					if scenario != "same-slot observation" || reads > 1 {
						o.Snapshot.VoltrIdleRaw += int64(gifts)
					}
					return o, err
				},
				foreignInflows: func(ctx context.Context, s *Snapshot) error { return observeForeignInflows(ctx, db, c, rwa, s) },
			}
			created, sent := 0, 0
			worker := &Worker{routeKey: productionRouteKey, interval: time.Millisecond, runtime: tickRuntime{
				loadNonterminal: func(context.Context, string) (*PersistedOperation, error) { return nil, nil },
				recordDecision: func(context.Context, string, Observation, Decision, string, *releaseRequest) (DecisionRecord, error) {
					created++
					return DecisionRecord{}, errors.New("unexpected decision")
				},
				advance: func(context.Context, PersistedOperation) error { sent++; return errors.New("unexpected send") },
			}}
			worker.runtime.observe = func(ctx context.Context) (Observation, error) {
				reads++
				o, err := state.observe(ctx)
				if reads == 1 {
					if !errors.Is(err, errConfirmedObservationUnavailable) || journal.positionSnapshotWrites != 0 {
						t.Fatalf("uncertain evidence did not defer before publication: %v", err)
					}
					if scenario == "bounded catchup" && (db.checkpoints[address].ForeignRaw != 64 || len(db.writes) != 64) {
						t.Fatal("bounded replay did not persist its 64-receipt prefix")
					}
					if scenario == "confirmed receipt missing" || scenario == "finalized receipt missing" {
						c.executions[solana.Signature{1}] = missing
						c.finalized = slot
					}
					return o, err
				}
				if err != nil || o.Snapshot.AcknowledgedForeignIdleRaw != int64(gifts) {
					t.Fatalf("later evidence did not converge: credit=%d err=%v", o.Snapshot.AcknowledgedForeignIdleRaw, err)
				}
				cancel()
				return Observation{}, ctx.Err()
			}
			if err := worker.Run(ctx); !errors.Is(err, context.Canceled) || reads != 2 || created != 0 || sent != 0 {
				t.Fatalf("worker exited or executed during recovery: reads=%d created=%d sent=%d err=%v", reads, created, sent, err)
			}
			if journal.positionSnapshotWrites != 1 || db.checkpoints[address].ForeignRaw != uint64(gifts) || len(db.writes) != gifts {
				t.Fatalf("checkpoint did not converge exactly once: checkpoint=%+v writes=%d snapshots=%d", db.checkpoints[address], len(db.writes), journal.positionSnapshotWrites)
			}
			if _, err := state.observe(context.Background()); err != nil || len(db.writes) != gifts {
				t.Fatalf("restart repeated credit: writes=%d err=%v", len(db.writes), err)
			}
		})
	}
}

type foreignFailingStore struct {
	*foreignMemoryStore
	readErr, writeErr error
}

func (s foreignFailingStore) foreignCheckpoint(ctx context.Context, route, address string) (foreignCheckpoint, bool, error) {
	if s.readErr != nil {
		return foreignCheckpoint{}, false, s.readErr
	}
	return s.foreignMemoryStore.foreignCheckpoint(ctx, route, address)
}
func (s foreignFailingStore) saveForeignEvidence(ctx context.Context, route, address string, e foreignEvidence) error {
	if s.writeErr != nil {
		return s.writeErr
	}
	return s.foreignMemoryStore.saveForeignEvidence(ctx, route, address, e)
}

func TestForeignHistoryHardFaultsStillStopWorker(t *testing.T) {
	for _, scenario := range []string{"database read", "database write", "malformed receipt", "changed slot", "missing anchor", "lost checkpoint", "negative balance", "sweep checkpoint ahead"} {
		t.Run(scenario, func(t *testing.T) {
			x := foreignFixture(rwa.idleATA, 17, 1)
			sig := x.Transaction.Signatures[0]
			anchor := foreignCheckpoint{Signature: foreignInflowAnchor, Slot: foreignInflowAnchorSlot, BalanceRaw: 17}
			db := foreignFailingStore{foreignMemoryStore: &foreignMemoryStore{checkpoints: map[string]foreignCheckpoint{rwa.idleATA: anchor}}}
			c := &foreignChainFixture{finalized: x.Slot, executions: map[solana.Signature]chain.Execution{sig: x}, history: map[solana.PublicKey][]chain.Signed{solanaKey(rwa.idleATA): {{Signature: sig, Slot: x.Slot}, {Signature: solana.MustSignatureFromBase58(foreignInflowAnchor), Slot: anchor.Slot}}}}
			stopBefore := ""
			switch scenario {
			case "database read":
				db.readErr = errors.New("database read failed")
			case "database write":
				db.writeErr = errors.New("database write failed")
			case "malformed receipt":
				x.Transaction = nil
				c.executions[sig] = x
			case "changed slot":
				x.Slot++
				c.finalized = x.Slot
				c.executions[sig] = x
			case "missing anchor":
				delete(db.checkpoints, rwa.idleATA)
			case "negative balance":
				c.history[solanaKey(rwa.idleATA)] = c.history[solanaKey(rwa.idleATA)][1:]
			case "lost checkpoint":
				c.history[solanaKey(rwa.idleATA)] = nil
			case "sweep checkpoint ahead":
				anchor.Slot = x.Slot + 1
				db.checkpoints[rwa.idleATA] = anchor
				stopBefore = sig.String()
			}
			reads := 0
			worker := &Worker{interval: time.Millisecond}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			err := worker.runTicks(ctx, func(ctx context.Context) error {
				reads++
				if scenario == "negative balance" {
					s := Snapshot{Slot: int64(x.Slot), VoltrIdleRaw: -1}
					return observeForeignInflows(ctx, db, c, rwa, &s)
				}
				_, err := replayForeignInflows(ctx, db, c, rwa, rwa.idleATA, x.Slot, c.finalized, stopBefore)
				return err
			})
			if err == nil || errors.Is(err, errConfirmedObservationUnavailable) || reads != 1 || len(db.writes) != 0 {
				t.Fatalf("hard fault hidden or evidence advanced: reads=%d writes=%d err=%v", reads, len(db.writes), err)
			}
		})
	}
}
