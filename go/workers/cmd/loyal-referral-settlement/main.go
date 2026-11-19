// loyal-referral-settlement reserves individually funded LP transfers and
// reconciles finalized receipts. Its treasury signer is external.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/db"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/referral"
	"github.com/solana-foundation/solana-go/v2"
)

func run(args []string) error {
	const usage = "usage: loyal-referral-settlement prepare LOT_ID | submit ID SIGNED_WIRE_FILE | register ID SIGNED_WIRE_FILE | reconcile ID | recover ID [AFTER_EXPIRY_TREASURY_SIGNATURE]"
	if len(args) < 2 {
		return errors.New(usage)
	}
	action, id := args[0], args[1]
	var afterExpiry solana.Signature
	var lot int64
	var wire []byte
	var err error
	switch action {
	case "prepare":
		if len(args) != 2 {
			return errors.New(usage)
		}
		lot, err = strconv.ParseInt(id, 10, 64)
		if err != nil || lot <= 0 {
			return errors.New("invalid fee lot ID")
		}
	case "register", "submit", "reconcile", "recover":
		if _, err = uuid.Parse(id); err != nil {
			return errors.New("invalid settlement ID")
		}
		if action == "register" || action == "submit" {
			if len(args) != 3 {
				return errors.New(usage)
			}
			f, err := os.Open(args[2])
			if err != nil {
				return err
			}
			defer f.Close()
			wire, err = io.ReadAll(io.LimitReader(f, 1233))
			if err != nil {
				return err
			}
			if len(wire) == 0 || len(wire) > 1232 {
				return errors.New("invalid signed wire size")
			}
		} else if action == "recover" && len(args) == 3 {
			afterExpiry, err = solana.SignatureFromBase58(args[2])
			if err != nil || afterExpiry.IsZero() {
				return errors.New("invalid recovery cursor")
			}
		} else if len(args) != 2 {
			return errors.New(usage)
		}
	default:
		return errors.New(usage)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	y, err := db.Open(ctx, os.Getenv("REFERRAL_SETTLEMENT_DATABASE_URL"), 2)
	if err != nil {
		return errors.New("settlement database unavailable")
	}
	defer y.Close()
	app, err := db.OpenLazy(ctx, os.Getenv("REFERRAL_APP_DATABASE_URL"), 1)
	if err != nil {
		return errors.New("missing app database configuration")
	}
	defer app.Close()
	c, err := chain.New(os.Getenv("SOLANA_RPC_URL"), 10*time.Second)
	if err != nil {
		return err
	}
	store := referral.NewStore(y, app, nil)
	var plan referral.Settlement
	switch action {
	case "prepare":
		plan, err = store.PrepareSettlement(ctx, c, lot)
	case "register":
		plan, err = store.RegisterSettlement(ctx, c, id, wire)
	case "submit":
		plan, err = store.SubmitSettlement(ctx, c, id, wire)
	case "reconcile":
		plan, err = store.ReconcileSettlement(ctx, c, id)
	case "recover":
		plan, err = store.RecoverSettlement(ctx, c, id, afterExpiry)
	}
	if err == nil || plan.Submission != "" {
		if writeErr := json.NewEncoder(os.Stdout).Encode(plan); writeErr != nil {
			return writeErr
		}
	}
	return err
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, chain.Scrub(err.Error()))
		os.Exit(1)
	}
}
