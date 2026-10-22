package webapp

import (
	"testing"

	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestStatsRouteIsRegisteredOnlyWhenTheRoleHoldsItsGrants(t *testing.T) {
	pool, err := pgxpool.New(t.Context(), "postgres://u:p@127.0.0.1:1/db?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	for _, granted := range []bool{false, true} {
		api := humachi.New(chi.NewRouter(), apiConfig())
		registerAPI(api, Config{AppDB: pool, StatsDB: granted})
		if _, registered := api.OpenAPI().Paths["/api/earn/stats"]; registered != granted {
			t.Errorf("StatsDB=%v: /api/earn/stats registered=%v", granted, registered)
		}
	}
}
