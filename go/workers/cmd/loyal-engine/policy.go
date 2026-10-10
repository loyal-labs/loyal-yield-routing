package main

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/engine"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/policy"
	"github.com/solana-foundation/solana-go/v2"
)

const policyUsage = `usage: loyal-engine policy apply|check klend --settings <settings> --reserve <reserve> [flags]
  apply: install the product's policy (simulates; --send lands it). Needs --delegate and
         credential POLICY_SETTINGS_SIGNER, the Settings' one signer, who pays.
  check: run each op (or --op N) through the installed policy (simulates; --send lands them in order).
         Needs credential POLICY_DELEGATE, the policy's delegate, who pays.
  Both read credential SOLANA_RPC_URL.`

// runPolicy is the developer loop for a policy: apply it, check it, both
// against the real chain.
func runPolicy(ctx context.Context, args []string, out io.Writer) error {
	if len(args) < 2 || args[1] != "klend" {
		return errors.New(policyUsage)
	}
	act := args[0]
	flags := flag.NewFlagSet("policy", flag.ContinueOnError)
	flags.SetOutput(out)
	settingsFlag := flags.String("settings", "", "Squads Settings account")
	reserveFlag := flags.String("reserve", "", "KLend reserve")
	vaultIndex := flags.Uint("vault-index", 0, "smart account vault index")
	amount := flags.Uint64("amount", 1_000_000, "deposit amount, raw liquidity units")
	delegateFlag := flags.String("delegate", "", "apply: the policy's delegated signer")
	replaceFlag := flags.String("replace", "", "apply: comma-separated policies the new one replaces")
	only := flags.Int("op", -1, "check: run only this op, by its policy position")
	send := flags.Bool("send", false, "land the transactions instead of only simulating")
	if err := flags.Parse(args[2:]); err != nil {
		return err
	}
	settings, err := solana.PublicKeyFromBase58(*settingsFlag)
	if err != nil {
		return fmt.Errorf("--settings: %w", err)
	}
	reserve, err := solana.PublicKeyFromBase58(*reserveFlag)
	if err != nil {
		return fmt.Errorf("--reserve: %w", err)
	}
	if *vaultIndex > 255 {
		return errors.New("--vault-index must be at most 255")
	}
	endpoint, err := engine.Credential("SOLANA_RPC_URL")
	if err != nil {
		return err
	}
	c, err := chain.New(endpoint, 30*time.Second)
	if err != nil {
		return err
	}
	switch act {
	case "apply":
		delegate, err := solana.PublicKeyFromBase58(*delegateFlag)
		if err != nil {
			return fmt.Errorf("--delegate: %w", err)
		}
		var replace []solana.PublicKey
		for _, value := range strings.Split(*replaceFlag, ",") {
			if value == "" {
				continue
			}
			key, err := solana.PublicKeyFromBase58(value)
			if err != nil {
				return fmt.Errorf("--replace: %w", err)
			}
			replace = append(replace, key)
		}
		signer, err := credentialKeypair("POLICY_SETTINGS_SIGNER")
		if err != nil {
			return err
		}
		build := policy.KLend(c, settings, uint8(*vaultIndex), reserve, delegate, *amount)
		return policy.Apply(ctx, c, out, settings, build, signer, delegate, replace, *send)
	case "check":
		delegate, err := credentialKeypair("POLICY_DELEGATE")
		if err != nil {
			return err
		}
		build := policy.KLend(c, settings, uint8(*vaultIndex), reserve, delegate.PublicKey(), *amount)
		return policy.Check(ctx, c, out, settings, build, delegate, *only, *send)
	}
	return errors.New(policyUsage)
}

// credentialKeypair reads a solana-keygen JSON keypair from a systemd
// credential and checks its public half against its seed.
func credentialKeypair(name string) (solana.PrivateKey, error) {
	value, err := engine.Credential(name)
	if err != nil {
		return nil, err
	}
	var raw []byte
	if err := json.Unmarshal([]byte(value), &raw); err != nil || len(raw) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("credential %s is not a keypair", name)
	}
	if !ed25519.NewKeyFromSeed(raw[:ed25519.SeedSize]).Equal(ed25519.PrivateKey(raw)) {
		return nil, fmt.Errorf("credential %s public key does not match its seed", name)
	}
	return solana.PrivateKey(raw), nil
}
