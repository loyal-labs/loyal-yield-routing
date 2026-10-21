package webapp

import (
	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/webauth"
)

type Deps struct {
	Neon *pgxpool.Pool
	Auth *webauth.Verifier
}

func RegisterAPI(api huma.API, d Deps) {
	_ = api
	_ = d
}
