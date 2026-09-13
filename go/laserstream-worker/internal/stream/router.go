package stream

import (
	"context"
	"fmt"
	"sort"

	pb "github.com/helius-labs/laserstream-sdk/go/proto"
)

// Router preserves LaserStream's many-to-many filter semantics. An account can
// match several named filters, and every owning durable handler must run before
// the shared stream frontier is allowed to advance.
type Router struct {
	handlers map[string]Handler
}

func NewRouter(handlers map[string]Handler) *Router {
	copy := make(map[string]Handler, len(handlers))
	for filter, handler := range handlers {
		copy[filter] = handler
	}
	return &Router{handlers: copy}
}

func (r *Router) Handle(ctx context.Context, update *pb.SubscribeUpdate) error {
	if update == nil || len(update.Filters) == 0 {
		return fmt.Errorf("LaserStream update had no owning filter")
	}
	filters := append([]string(nil), update.Filters...)
	sort.Strings(filters)
	last := ""
	for _, filter := range filters {
		if filter == last {
			continue
		}
		last = filter
		handler, ok := r.handlers[filter]
		if !ok || handler == nil {
			return fmt.Errorf("LaserStream update matched unowned filter %q", filter)
		}
		if err := handler.Handle(ctx, update); err != nil {
			return fmt.Errorf("process filter %q: %w", filter, err)
		}
	}
	return nil
}
