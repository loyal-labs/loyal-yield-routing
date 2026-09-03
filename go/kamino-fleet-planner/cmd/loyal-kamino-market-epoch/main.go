package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"os"

	"github.com/loyal-labs/loyal-yield-routing/go/kamino-fleet-planner/internal/fleet"
)

func main() {
	live := flag.Bool("live", false, "read the retained monitor evidence using TIMESCALE_DATABASE_URL/TIMESCALEDB_URL")
	emitFixture := flag.Bool("emit-fixture", false, "with --live, emit the typed source snapshot instead of the epoch")
	schema := flag.String("schema", "kamino", "Timescale schema used with --live")
	flag.Parse()
	var epoch fleet.ImmutableMarketEpoch
	var err error
	if *live {
		databaseURL := os.Getenv("TIMESCALE_DATABASE_URL")
		if databaseURL == "" {
			databaseURL = os.Getenv("TIMESCALEDB_URL")
		}
		ctx := context.Background()
		store, openErr := fleet.OpenMarketEvidenceStore(ctx, databaseURL, *schema)
		if openErr != nil {
			log.Fatal(openErr)
		}
		defer store.Close()
		if *emitFixture {
			snapshot, loadErr := store.LoadSnapshot(ctx, []string{fleet.USDCMint}, []string{"safe"})
			if loadErr != nil {
				log.Fatal(loadErr)
			}
			fixture := fleet.MarketEpochFixture{SupportedReserveMarketSnapshot: snapshot, EnabledMints: []string{fleet.USDCMint}}
			if encodeErr := json.NewEncoder(os.Stdout).Encode(fixture); encodeErr != nil {
				log.Fatal(encodeErr)
			}
			return
		}
		epoch, err = store.LoadImmutableMarketEpoch(ctx)
	} else {
		var fixture fleet.MarketEpochFixture
		if decodeErr := json.NewDecoder(os.Stdin).Decode(&fixture); decodeErr != nil {
			log.Fatal(decodeErr)
		}
		epoch, err = fleet.BuildImmutableMarketEpoch(fixture.SupportedReserveMarketSnapshot, fixture.EnabledMints)
	}
	if err != nil {
		log.Fatal(err)
	}
	if err := epoch.Validate(); err != nil {
		log.Fatal(err)
	}
	if err := json.NewEncoder(os.Stdout).Encode(epoch.DurableEvidence()); err != nil {
		log.Fatal(err)
	}
}
