package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/db"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/engine"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/releasecontrol"
)

const releaseControlUsage = `usage: loyal-engine backyard release-control show --vault <voltr-vault>
       loyal-engine backyard release-control set --vault <voltr-vault> --action optional_addition|ordinary_funding|official_deposit|claim_attempt --state open|held --expect-version <n> --reason "<text>" --operator <name> [--allow-owner <authority>...] [--max-amount-raw <n>]`

func runReleaseControl(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 || (args[0] != "show" && args[0] != "set") {
		return errors.New(releaseControlUsage)
	}
	change, err := parseReleaseControlFlags(args[0], args[1:])
	if err != nil {
		return err
	}
	url, err := engine.Credential("BACKYARD_DATABASE_URL")
	if err != nil {
		return err
	}
	startup, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	pool, err := db.Open(startup, url, 2)
	if err != nil {
		return err
	}
	defer pool.Close()
	var result any
	if args[0] == "show" {
		result, err = releasecontrol.Show(ctx, pool, change.Vault)
	} else {
		result, err = releasecontrol.Set(ctx, pool, change)
	}
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(result)
}

func parseReleaseControlFlags(command string, args []string) (releasecontrol.Change, error) {
	var c releasecontrol.Change
	seen := map[string]bool{}
	for i := 0; i < len(args); i++ {
		flag := args[i]
		if i+1 >= len(args) {
			return c, fmt.Errorf("release-control: %s requires a value\n%s", flag, releaseControlUsage)
		}
		value := args[i+1]
		i++
		if seen[flag] && flag != "--allow-owner" {
			return c, fmt.Errorf("release-control: %s given twice", flag)
		}
		seen[flag] = true
		switch flag {
		case "--vault":
			c.Vault = value
		case "--action":
			action, err := releasecontrol.ParseAction(value)
			if err != nil {
				return c, err
			}
			c.Action = action
		case "--state":
			c.State = releasecontrol.State(value)
		case "--expect-version":
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil || n < 0 {
				return c, fmt.Errorf("release-control: --expect-version must be a version from show, 0 to create")
			}
			c.ExpectVersion = n
		case "--reason":
			c.Reason = value
		case "--operator":
			c.Operator = value
		case "--allow-owner":
			c.AllowedOwners = append(c.AllowedOwners, value)
		case "--max-amount-raw":
			n, err := strconv.ParseUint(value, 10, 64)
			if err != nil {
				return c, fmt.Errorf("release-control: --max-amount-raw must be a raw integer")
			}
			c.MaxAmountRaw = &n
		default:
			return c, fmt.Errorf("release-control: unexpected argument %q\n%s", flag, releaseControlUsage)
		}
	}
	if c.Vault == "" {
		return c, errors.New(releaseControlUsage)
	}
	if command == "show" && len(seen) != 1 {
		return c, errors.New(releaseControlUsage)
	}
	if command == "set" && (!seen["--action"] || !seen["--state"] || !seen["--expect-version"] || !seen["--reason"] || !seen["--operator"]) {
		return c, fmt.Errorf("release-control set: --action, --state, --expect-version, --reason and --operator are required\n%s", releaseControlUsage)
	}
	return c, nil
}
