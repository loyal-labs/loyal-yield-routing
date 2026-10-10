package earn

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	pb "github.com/helius-labs/laserstream-sdk/go/proto"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/engine"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/multiply"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/observer/watch"
	"github.com/solana-foundation/solana-go/v2"
)

// Application is the Go Earn domain application that replaced the Rust
// earn-domain-bridge child: policy transaction projection on the stream path
// and the durable Earn reconciliation job consumers.
type Application struct {
	store    *Store
	multiply *multiply.Store
	rpc      *chain.Client
	monitor  *PolicyMonitor
	consumer string
	facts    *engine.Facts
	logger   *slog.Logger
	// streamAlive reports whether the LaserStream session delivered a slot
	// recently; observer progress requires both a live stream and a caught-up
	// Earn application.
	streamAlive func() bool
}

const (
	jobLeaseSeconds      = 120
	jobRetryBaseSeconds  = 15
	deadLetterAttempt    = 50
	proofStaleAttempt    = 6
	earnBacklogHorizon   = 60 * time.Second
	consumerIdleInterval = time.Second
)

// NewApplication binds the Earn application to the observer's Neon pool.
func NewApplication(ctx context.Context, pool *pgxpool.Pool, rpc *chain.Client, cluster string, delegate solana.PublicKey, facts *engine.Facts, logger *slog.Logger, streamAlive func() bool) (*Application, error) {
	store := NewStore(pool)
	multiplyStore, err := multiply.NewStoreFromPool(ctx, pool)
	if err != nil {
		return nil, err
	}
	monitor, err := NewPolicyMonitor(store, rpc, cluster, delegate)
	if err != nil {
		return nil, err
	}
	return &Application{store: store, multiply: multiplyStore, rpc: rpc, monitor: monitor, consumer: ConsumerName(cluster),
		facts: facts, logger: logger, streamAlive: streamAlive}, nil
}

// HandlePolicyTransaction projects one confirmed policy transaction from the
// stream and advances the policy projection cursor past its slot.
func (a *Application) HandlePolicyTransaction(ctx context.Context, update *pb.SubscribeUpdate) error {
	transaction := update.GetTransaction()
	if transaction == nil {
		return errors.New("Earn policy projection received a non-transaction update")
	}
	if transaction.GetTransaction() == nil {
		return errors.New("policy transaction payload was missing")
	}
	slot := transaction.GetSlot()
	decoded, err := DecodeStreamPolicyTransaction(transaction.GetTransaction(), slot)
	if err != nil {
		return err
	}
	if decoded != nil {
		if len(decoded.Instructions) > 0 {
			if _, err := a.monitor.ProcessPolicyInstructions(ctx, decoded.Signature, decoded.Slot, decoded.Instructions); err != nil {
				return err
			}
		}
		if _, err := projectEarnMaxMemos(ctx, a.multiply, a.logger, decoded); err != nil {
			return err
		}
		if err := a.recordRecurringDelegations(ctx, decoded); err != nil {
			return err
		}
	}
	return a.store.AdvanceProjectionCursor(ctx, PolicyProjectionConsumer, slot)
}

// recordRecurringDelegations records each Subscriptions create_recurring.
// The program is public: bytes that are not ours are logged and skipped.
func (a *Application) recordRecurringDelegations(ctx context.Context, transaction *PolicyTransaction) error {
	for _, instruction := range transaction.Instructions {
		accounts, data := instruction.Accounts, instruction.Data
		if instruction.ProgramID != subscriptionsProgram || len(data) == 0 || data[0] != subscriptionsCreateRecurring {
			continue
		}
		skipped := "the instruction is shorter than create_recurring"
		if len(data) >= 41 && len(accounts) >= 4 {
			var err error
			if skipped, err = a.store.RecordRecurringDelegation(ctx, RecurringDelegationObserved{
				Wallet: accounts[0].PublicKey.String(), VaultPubkey: accounts[3].PublicKey.String(),
				SubscriptionAuthority: accounts[1].PublicKey.String(), RecurringDelegation: accounts[2].PublicKey.String(),
				Nonce: binary.LittleEndian.Uint64(data[1:9]), AmountPerPeriod: binary.LittleEndian.Uint64(data[9:17]),
				PeriodLengthSeconds: binary.LittleEndian.Uint64(data[17:25]), StartTimestamp: int64(binary.LittleEndian.Uint64(data[25:33])),
				ExpiryTimestamp: int64(binary.LittleEndian.Uint64(data[33:41])), Signature: transaction.Signature, Slot: transaction.Slot,
			}); err != nil {
				return err
			}
		}
		if skipped != "" {
			a.logger.Info("skipped a recurring delegation", "event", "recurring_delegation_skipped", "reason", skipped, "signature", transaction.Signature)
		}
	}
	return nil
}

func touchesPolicyIdentity(update NormalizedUpdate, vault watch.Vault) bool {
	policyFilter := false
	for _, filter := range update.Filters {
		policyFilter = policyFilter || filter == watch.EarnSmartAccounts || filter == watch.EarnPolicyAccounts || filter == watch.EarnWallets
	}
	if !policyFilter || update.AccountPubkey == nil {
		return false
	}
	account := *update.AccountPubkey
	if account == vault.Settings || account == vault.Wallet {
		return true
	}
	for _, binding := range vault.Accounts {
		if binding.Pubkey == account && (binding.Role == "smart_account" || binding.Role == "policy") {
			return true
		}
	}
	return false
}

type deferral int

const (
	deferProofPending deferral = iota
	deferRPCBehind
	deferFailure
)

func deferralKind(err error) deferral {
	switch {
	case errors.Is(err, errProofPending):
		return deferProofPending
	case errors.Is(err, chain.ErrBehind):
		return deferRPCBehind
	}
	return deferFailure
}

// jobOutcome is EarnReconciliationProcessOutcome.
type jobOutcome struct {
	idle, applied, deferred, deadLettered bool
	jobID                                 int64
	attempt                               int32
	kind                                  deferral
	err                                   error
}

func (a *Application) decideMutation(ctx context.Context, update NormalizedUpdate, vault watch.Vault) (EarnMutation, error) {
	if vault.EarnMax {
		return EarnMutation{}, a.projectEarnMaxAccountUpdate(ctx, update, vault)
	}
	// The stream's transaction filter projects every Squads and Subscriptions
	// instruction, so a policy identity's own update moves no position: cash
	// flow changes a watched token account or obligation in the same
	// transaction. A legacy policy deletion still needs the zero-balance and
	// closed-policy proof.
	if update.EventKind != "refund_cleanup_repair" && touchesPolicyIdentity(update, vault) && !isPolicyDeletion(update, vault) {
		return EarnMutation{}, nil
	}
	context, err := a.store.LoadContext(ctx, vault.Settings, vault.VaultIndex, vault.Vault)
	if err != nil {
		return EarnMutation{}, err
	}
	return resolveMutation(ctx, a.rpc, update, vault, context)
}

// processNextJob is process_next_earn_reconciliation_job_with_policy_monitor.
func (a *Application) processNextJob(ctx context.Context, owner string) (jobOutcome, error) {
	job, err := a.store.ClaimJob(ctx, a.consumer, owner, jobLeaseSeconds)
	if err != nil {
		return jobOutcome{}, err
	}
	if job == nil {
		return jobOutcome{idle: true}, nil
	}
	retryAfter := int64(jobRetryBaseSeconds) << min(max(job.AttemptCount-1, 0), 5)
	var update NormalizedUpdate
	var vault watch.Vault
	mutation := EarnMutation{}
	if err = json.Unmarshal(job.EventPayload, &update); err != nil {
		err = fmt.Errorf("decode durable Earn event: %w", err)
	} else if err = json.Unmarshal(job.VaultPayload, &vault); err != nil {
		err = fmt.Errorf("decode durable Earn vault: %w", err)
	} else if mutation, err = a.decideMutation(ctx, update, vault); err == nil {
		applied, completeErr := a.store.CompleteJob(ctx, job.ID, owner, mutation)
		if completeErr == nil {
			return jobOutcome{applied: applied > 0, jobID: job.ID}, nil
		}
		err = completeErr
	}
	kind := deferralKind(err)
	message := err.Error()
	if kind == deferFailure && job.AttemptCount >= deadLetterAttempt {
		if err := a.store.DeadLetterJob(ctx, job.ID, owner, message); err != nil {
			return jobOutcome{}, err
		}
		return jobOutcome{deadLettered: true, jobID: job.ID, attempt: job.AttemptCount, kind: kind, err: errors.New(message)}, nil
	}
	if err := a.store.RetryJob(ctx, job.ID, owner, message, retryAfter); err != nil {
		return jobOutcome{}, err
	}
	return jobOutcome{deferred: true, jobID: job.ID, attempt: job.AttemptCount, kind: kind, err: errors.New(message)}, nil
}

// caughtUp reports whether no claimable Earn job has waited longer than the
// backlog horizon. A job deferred to a later attempt, or queued behind an
// earlier pending job of its vault, is not claimable: the head's failure is
// reported through Failed at the alert thresholds, while consumers that fall
// behind the claimable queue stop observer progress.
func (a *Application) caughtUp(ctx context.Context) (bool, error) {
	var oldest *time.Time
	if err := a.store.pool.QueryRow(ctx, `
        SELECT MIN(next_attempt_at)
        FROM loyal_yield.earn_reconciliation_jobs
        WHERE `+claimableJob, a.consumer).Scan(&oldest); err != nil {
		return false, err
	}
	return oldest == nil || time.Since(*oldest) < earnBacklogHorizon, nil
}

func (a *Application) reportCycle(ctx context.Context) {
	ok, err := a.caughtUp(ctx)
	if err != nil {
		a.facts.Failed(engine.FamilyObserver, "earn_backlog_read")
		return
	}
	if ok && a.streamAlive() {
		a.facts.Progress(engine.FamilyObserver)
	}
}

// RunConsumers runs the durable Earn reconciliation consumers until ctx ends.
func (a *Application) RunConsumers(ctx context.Context, workers int) {
	var wait sync.WaitGroup
	for worker := range workers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			a.runConsumer(ctx, fmt.Sprintf("observer-earn:%d:%d", os.Getpid(), worker), worker == 0)
		}()
	}
	wait.Wait()
}

func (a *Application) runConsumer(ctx context.Context, owner string, reports bool) {
	for ctx.Err() == nil {
		outcome, err := a.processNextJob(ctx, owner)
		switch {
		case err != nil:
			a.facts.Failed(engine.FamilyObserver, "earn_consumer")
			a.logger.Error("durable Earn reconciliation consumer failed", "event", "earn_consumer_failed", "error", err)
		case outcome.deadLettered:
			a.facts.Failed(engine.FamilyObserver, "earn_job_dead_lettered")
			a.logger.Error("Earn reconciliation job exhausted retries and was dead-lettered", "event", "earn_job_dead_lettered", "job_id", outcome.jobID, "attempt_count", outcome.attempt, "error", outcome.err)
		case outcome.deferred:
			alert := outcome.kind == deferFailure && outcome.attempt == 1 || outcome.kind != deferFailure && outcome.attempt == proofStaleAttempt
			reason := map[deferral]string{deferProofPending: "proof_pending", deferRPCBehind: "rpc_behind", deferFailure: "failure"}[outcome.kind]
			level := slog.LevelWarn
			if alert {
				// Rust alerted on a first failure and on a proof stale at attempt 6.
				a.facts.Failed(engine.FamilyObserver, "earn_job_"+reason)
				level = slog.LevelError
			}
			a.logger.Log(ctx, level, "Earn reconciliation job deferred", "event", "earn_job_deferred", "reason", reason, "job_id", outcome.jobID, "attempt_count", outcome.attempt, "error", outcome.err)
		case outcome.applied:
			a.logger.Info("completed durable Earn reconciliation job", "job_id", outcome.jobID)
		}
		if reports {
			a.reportCycle(ctx)
		}
		if err != nil || outcome.idle {
			select {
			case <-ctx.Done():
			case <-time.After(consumerIdleInterval):
			}
		}
	}
}
