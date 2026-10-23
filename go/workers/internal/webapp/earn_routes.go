package webapp

import (
	"net/http"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/solana-foundation/solana-go/v2"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/earnprep"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/jwt"
)

type Earn struct {
	YieldDB      *pgxpool.Pool
	Chain        *chain.Client
	Cluster      SolanaCluster
	PolicySigner solana.PublicKey
	Session      *jwt.Signer
	Sponsor      *earnprep.Sponsor
	SponsorChain SponsorChain
	HTTP         *http.Client
	OTLP         *OTLP
	Privy        *Privy
	Granted      map[string]bool
}

type OTLP struct {
	Endpoint     *url.URL
	IngestionKey string
}

type Privy struct {
	AppID     string
	AppSecret string
}

type earnRoutes struct {
	auth  *Auth
	appDB *pgxpool.Pool
	Earn
}

func newEarnRoutes(auth *Auth, appDB *pgxpool.Pool, earn Earn) earnRoutes {
	if earn.HTTP == nil {
		earn.HTTP = &http.Client{Timeout: 15 * time.Second}
	}
	if earn.SponsorChain == nil && earn.Chain != nil {
		earn.SponsorChain = earn.Chain
	}
	return earnRoutes{auth: auth, appDB: appDB, Earn: earn}
}
