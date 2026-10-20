package webapp

import (
	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/webauth"
)

// Deps is everything an /api route may use: the workers' own stores and
// clients, never a second copy of their SQL.
type Deps struct {
	Neon *pgxpool.Pool
	Auth *webauth.Verifier
}

// RegisterAPI declares every /api route Go owns. A route registered here
// answers its path; any other /api path is forwarded to the Next deployment.
// Each family lives in its own api_<family>.go and is registered below.
func RegisterAPI(api huma.API, d Deps) {
	_ = api
	_ = d
}
