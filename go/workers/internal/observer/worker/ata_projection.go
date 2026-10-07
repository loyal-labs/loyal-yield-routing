package worker

import (
	"context"
	"errors"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/engine"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/observer/ata"
)

// NewATAProjector borrows the capture and Yield pools already owned by the
// observer. Progress uses the retained stream consumer and its atomic cursor;
// the observer owns and joins this lane alongside capture and read models.
func (r *Runtime) NewATAProjector(ctx context.Context) (*ata.Projector, error) {
	if r == nil || r.neon == nil || r.timescale == nil || r.facts == nil {
		return nil, errors.New("ATA projection requires initialized observer pools and facts")
	}
	projector, err := ata.NewProjector(r.timescale, r.neon, ata.ProjectorConfig{
		Stream: r.cfg.ATAStream, Cluster: r.cfg.Cluster, BatchLimit: 500, PollInterval: 250 * time.Millisecond,
		IOTimeout: 10 * time.Second,
		OnError:   func(err error) { r.ataProjectionFailed(ctx, err) },
	})
	if err != nil {
		return nil, err
	}
	if err := projector.RequireSchema(ctx); err != nil {
		return nil, err
	}
	return projector, nil
}

func (r *Runtime) ataProjectionFailed(ctx context.Context, err error) {
	r.facts.Failed(engine.FamilyObserver, "ata_projection")
	r.logger.WarnContext(ctx, "ATA projection unavailable", "stream", r.cfg.ATAStream, "error", err)
}
