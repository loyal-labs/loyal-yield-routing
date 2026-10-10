package main

import (
	"context"
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
       loyal-engine policy remove --settings <settings> --policies <a,b,...> [--send]
  apply: install the product's policy (simulates; --send lands it). Needs --delegate and
         credential POLICY_SETTINGS_SIGNER, the Settings' one signer, who pays.
  check: run each op (or --op N) through the installed policy (simulates; --send lands them in order).
         Needs credential POLICY_DELEGATE, the policy's delegate, who pays.
  remove: remove the policies in one settings transaction (simulates; --send lands it).
         Needs credential POLICY_SETTINGS_SIGNER.
  All read credential SOLANA_RPC_URL.`

// runPolicy is the developer loop for a policy: apply it, check it, remove
// it, against the real chain.
func runPolicy(ctx context.Context, args []string, out io.Writer) error {
	if len(args) < 1 || (args[0] != "remove" && (len(args) < 2 || args[1] != "klend")) {
		return errors.New(policyUsage)
	}
	act, rest := args[0], args[1:]
	if act != "remove" {
		rest = args[2:]
	}
	flags := flag.NewFlagSet("policy", flag.ContinueOnError)
	flags.SetOutput(out)
	settingsFlag := flags.String("settings", "", "Squads Settings account")
	reserveFlag := flags.String("reserve", "", "KLend reserve")
	vaultIndex := flags.Uint("vault-index", 0, "smart account vault index")
	amount := flags.Uint64("amount", 1_000_000, "deposit amount, raw liquidity units")
	delegateFlag := flags.String("delegate", "", "apply: the policy's delegated signer")
	replaceFlag := flags.String("replace", "", "apply: comma-separated policies the new one replaces")
	policiesFlag := flags.String("policies", "", "remove: comma-separated policies to remove")
	only := flags.Int("op", -1, "check: run only this op, by its policy position")
	send := flags.Bool("send", false, "land the transactions instead of only simulating")
	if err := flags.Parse(rest); err != nil {
		return err
	}
	settings, err := solana.PublicKeyFromBase58(*settingsFlag)
	if err != nil {
		return fmt.Errorf("--settings: %w", err)
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
	if act == "remove" {
		policies, err := publicKeyList("--policies", *policiesFlag)
		if err != nil {
			return err
		}
		signer, err := credentialKeypair("POLICY_SETTINGS_SIGNER")
		if err != nil {
			return err
		}
		return policy.Remove(ctx, c, out, settings, signer, policies, *send)
	}
	reserve, err := solana.PublicKeyFromBase58(*reserveFlag)
	if err != nil {
		return fmt.Errorf("--reserve: %w", err)
	}
	switch act {
	case "apply":
		delegate, err := solana.PublicKeyFromBase58(*delegateFlag)
		if err != nil {
			return fmt.Errorf("--delegate: %w", err)
		}
		replace, err := publicKeyList("--replace", *replaceFlag)
		if err != nil {
			return err
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

// publicKeyList parses a comma-separated flag of public keys.
func publicKeyList(name, value string) ([]solana.PublicKey, error) {
	var out []solana.PublicKey
	for _, part := range strings.Split(value, ",") {
		if part == "" {
			continue
		}
		key, err := solana.PublicKeyFromBase58(part)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		out = append(out, key)
	}
	return out, nil
}

// credentialKeypair reads a keypair from a systemd credential.
func credentialKeypair(name string) (solana.PrivateKey, error) {
	value, err := engine.Credential(name)
	if err != nil {
		return nil, err
	}
	key, err := parseRetailKey(value)
	if err != nil {
		return nil, fmt.Errorf("credential %s: %w", name, err)
	}
	return solana.PrivateKey(key), nil
}
