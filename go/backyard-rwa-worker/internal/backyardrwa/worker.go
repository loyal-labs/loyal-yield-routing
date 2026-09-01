package backyardrwa

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"
)

type Worker struct {
	database *Database
	rpc      *RPCClient
	routeKey string
	interval time.Duration
	manifest RouteManifest
}

func NewWorker(database *Database, rpc *RPCClient, routeKey string, config Config) (*Worker, error) {
	if database == nil || rpc == nil || routeKey == "" || config.PollInterval <= 0 {
		return nil, fmt.Errorf("invalid concrete worker configuration")
	}
	manifest, err := loadEmbeddedRouteManifest()
	if err != nil {
		return nil, err
	}
	return &Worker{database: database, rpc: rpc, routeKey: routeKey, interval: config.PollInterval, manifest: manifest}, nil
}

// Tick resumes durable work first. New observation/transaction construction is
// intentionally fail-closed until the deployed adaptor-v2 identity and complete
// policy catalog exist; it cannot fabricate builders or signatures.
func (w *Worker) Tick(ctx context.Context) error {
	operation, err := w.database.LoadNonterminal(ctx, w.routeKey)
	if err != nil {
		return err
	}
	if operation != nil {
		return AdvanceNonterminal(ctx, w.database, w.rpc, *operation)
	}
	observation, err := ObserveConfirmedBridgeSnapshot(ctx, w.rpc)
	if err != nil {
		return err
	}
	decision := Decide(observation.Snapshot)
	if err := decision.Validate(); err != nil {
		return err
	}
	if blocker := w.manifest.executionBlocker(); blocker != nil {
		return blocker
	}
	// Never create a nonterminal operation that cannot be built from exact,
	// confirmed inputs. Terminal holds still use the shared decision journal.
	if blocker := buildBlocker(decision.Action); blocker != nil {
		return blocker
	}
	_, err = w.database.RecordDecision(ctx, w.routeKey, observation, decision, w.manifest.SHA256, *w.manifest.PolicyCatalog.SHA256)
	return err
}

func buildBlocker(action Action) *RuntimeBlocker {
	switch action {
	case VoltrAllocateToSquads, StageSquadsToVoltr, VoltrRestoreIdle, ReportNAV:
		return ErrBridgePrerequisitesUnavailable
	case OpenPrimeUSDCStep, DeleverPrimeUSDCStep:
		return ErrKaminoTransactionConstructionUnavailable
	default:
		return nil
	}
}

func (w *Worker) Run(ctx context.Context) error {
	for {
		if err := w.Tick(ctx); err != nil {
			return err
		}
		timer := time.NewTimer(w.interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// Run wires the single direct pgx/RPC process. Missing deployment artifacts are
// an explicit startup failure, not a read-only mode or an alternate executor.
func Run(ctx context.Context, out io.Writer) error {
	if ctx == nil || out == nil {
		return fmt.Errorf("missing runtime dependency")
	}
	runtimeConfig := RuntimeConfigFromEnvironment()
	if err := runtimeConfig.Validate(); err != nil {
		return err
	}
	database, err := OpenDatabase(ctx, runtimeConfig.DatabaseURL)
	if err != nil {
		return err
	}
	defer database.Close()
	rpc, err := NewRPCClient(runtimeConfig.RPCURL)
	if err != nil {
		return err
	}
	worker, err := NewWorker(database, rpc, runtimeConfig.RouteKey, DefaultConfig())
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintln(out, "backyard-rwa-worker: starting serialized confirmed lifecycle"); err != nil {
		return err
	}
	err = worker.Run(ctx)
	if errors.Is(err, ErrTransactionConstructionUnavailable) {
		return ErrTransactionConstructionUnavailable
	}
	return err
}
