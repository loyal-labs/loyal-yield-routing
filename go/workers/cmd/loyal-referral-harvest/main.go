// loyal-referral-harvest collects Earn MAX performance fees into the vault's configured treasury.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/db"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/engine"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/referral"
	"github.com/solana-foundation/solana-go/v2"
)

func run(ctx context.Context, args []string, out io.Writer) error {
	flags := flag.NewFlagSet("loyal-referral-harvest", flag.ContinueOnError)
	execute := flags.Bool("execute", false, "submit and reconcile eligible fee collections")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected arguments")
	}
	payer, err := solana.PublicKeyFromBase58(os.Getenv("REFERRAL_HARVEST_PAYER"))
	if err != nil || payer.IsZero() || !payer.IsOnCurve() {
		return errors.New("REFERRAL_HARVEST_PAYER must be the dedicated fee payer's public key")
	}
	var signer solana.PrivateKey
	if *execute {
		raw, err := engine.Credential("REFERRAL_HARVEST_KEYPAIR")
		if err != nil {
			return err
		}
		signer, err = solana.PrivateKeyFromSolanaKeygenFileBytes([]byte(raw))
		if err != nil || signer.PublicKey() != payer {
			return errors.New("REFERRAL_HARVEST_KEYPAIR must match REFERRAL_HARVEST_PAYER")
		}
	}
	dsn, err := engine.Credential("REFERRAL_HARVEST_DATABASE_URL")
	if err != nil {
		return err
	}
	endpoint, err := engine.Credential("REFERRAL_HARVEST_RPC_URL")
	if err != nil {
		return err
	}
	client, err := chain.New(endpoint, 10*time.Second)
	if err != nil {
		return errors.New("invalid harvest RPC URL")
	}
	pool, err := db.Open(ctx, dsn, 1)
	if err != nil {
		return errors.New("harvest database unavailable")
	}
	defer pool.Close()
	report, err := referral.NewStore(pool, nil, nil).RunHarvest(ctx, client, payer, signer, os.Getenv("STATE_DIRECTORY"), *execute)
	if writeErr := json.NewEncoder(out).Encode(report); writeErr != nil {
		return writeErr
	}
	if err == nil && report.Status == "held" {
		return errors.New("harvest held; inspect the reported reason")
	}
	return err
}

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := run(ctx, os.Args[1:], os.Stdout); err != nil && !errors.Is(err, flag.ErrHelp) {
		fmt.Fprintln(os.Stderr, chain.Scrub(err.Error()))
		os.Exit(1)
	}
}
