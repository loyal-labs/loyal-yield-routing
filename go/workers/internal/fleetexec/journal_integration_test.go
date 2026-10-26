package fleetexec

import (
	"github.com/jackc/pgx/v5"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
)

func rotateSeed(seed byte, offset byte) []byte {
	out := make([]byte, 32)
	for i := range out {
		out[i] = seed + byte(i) + offset
	}
	return out
}

var _ = fleet.PreparedTransaction{}
var _ = pgx.ErrNoRows
