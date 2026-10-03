package backyard

import (
	"context"
	"fmt"
	"io"
)

// EngineConfig is the explicit injected runtime for one separately credentialed
// Backyard engine instance. The loyal-engine command constructs it; nothing in
// this package reads the environment to build one. Database, RPC, and signing
// credentials are all Backyard-scoped: a retail engine instance composes its
// own and neither instance can reach the other's dependencies.
type EngineConfig struct {
	Database *Database
	RPC      *RPCClient
	// Credentials is the Backyard delegated executor capability. It stays
	// inside the engine instance; observers and planners never receive it.
	Credentials Credentials
	RouteKey    string
	Config      Config
	// Owner is the platform-neutral scope+instance+release lease identity.
	Owner string
	// Out is optional; when set it receives the startup banner.
	Out io.Writer
	// ImageVersion is optional banner provenance, never a lease identity.
	ImageVersion string
}

// Engine is the concrete runtime adapter for process composition. It satisfies
// the shared process-lane contract structurally (`Run(context.Context) error`)
// without this domain package importing the runtime package.
type Engine struct {
	worker       *Worker
	leases       routeLeaser
	owner        string
	config       Config
	out          io.Writer
	imageVersion string
}

// NewEngine validates the complete injected runtime or fails closed. A missing
// dependency, an unowned lease identity, or a capability that does not match
// the pinned delegated executor is a startup failure, not a degraded mode.
func NewEngine(config EngineConfig) (*Engine, error) {
	worker, err := NewWorker(config.Database, config.RPC, config.RouteKey, config.Config, config.Credentials)
	if err != nil {
		return nil, err
	}
	if !ValidLeaseOwner(config.Owner) {
		return nil, fmt.Errorf("Backyard engine requires a platform-neutral lease owner")
	}
	return &Engine{
		worker: worker, leases: config.Database, owner: config.Owner, config: config.Config,
		out: config.Out, imageVersion: config.ImageVersion,
	}, nil
}

// Run acquires the route fence, serializes lifecycle ticks, and releases the
// exact fencing token on shutdown. Cancellation surfaces as context.Canceled
// so the owning process runtime can join the lane without recording a failure.
func (e *Engine) Run(ctx context.Context) error {
	if e == nil || e.worker == nil || e.leases == nil {
		return fmt.Errorf("Backyard engine is not constructed")
	}
	if e.out != nil {
		if _, err := fmt.Fprintf(e.out,
			"backyard-rwa-worker: starting serialized confirmed lifecycle route=%s image=%s lease_owner=%s manifest_sha256=%s\n",
			e.worker.routeKey, e.imageVersion, e.owner, e.worker.manifest.SHA256,
		); err != nil {
			return err
		}
	}
	return e.worker.Run(ctx, e.leases, e.owner, e.config)
}

// RunWithConfig runs one credentialed Backyard engine instance on an explicit
// injected runtime. It is the v2 entrypoint for process composition; the
// environment-reading bootstrap remains separately in Run.
func RunWithConfig(ctx context.Context, config EngineConfig) error {
	engine, err := NewEngine(config)
	if err != nil {
		return err
	}
	return engine.Run(ctx)
}
