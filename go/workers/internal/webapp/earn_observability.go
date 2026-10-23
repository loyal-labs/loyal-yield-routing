package webapp

import "github.com/danielgtaylor/huma/v2"

// POST /api/observability/errors                                   <- web/apps/web/src/app/api/observability/errors/route.ts (mobile twin web/apps/web/src/app/api/observability/mobile/errors)
// POST /api/observability/events                                   <- web/apps/web/src/app/api/observability/events/route.ts (mobile twin web/apps/web/src/app/api/observability/mobile/events)
// POST /api/observability/metrics                                  <- web/apps/web/src/app/api/observability/metrics/route.ts (mobile twin web/apps/web/src/app/api/observability/mobile/metrics)
var earnObservabilityFamily = earnFamily{
	name:     "observability ingest",
	ready:    notPorted,
	register: earnRoutes.registerObservability,
}

func (h earnRoutes) registerObservability(api huma.API) {}
