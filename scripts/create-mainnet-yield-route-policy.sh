#!/usr/bin/env bash
set -euo pipefail

USER_KEYPAIR="${USER_KEYPAIR:-/Users/zotho/.config/solana/id.json}"
SETTINGS="${SETTINGS:-4aWMf1dFxviHisBFfi9apgqNDUBH4rLWHQYHUANbLAdi}"
DELEGATED_SIGNER="${DELEGATED_SIGNER:-BAqgbERmvUViqDSx961xpRBHGt68SpACiWL4t9696qZZ}"
POLICY_SEED="${POLICY_SEED:-4}"

if [[ -z "${SOLANA_RPC_URL:-}" ]]; then
  echo "SOLANA_RPC_URL is required. Run through: op run --env-file=.env.1password -- sh -c 'bun run yield-policy:init:mainnet'" >&2
  exit 1
fi

exec bun run yield-policy:init -- \
  --cluster mainnet \
  --keypair "$USER_KEYPAIR" \
  --settings "$SETTINGS" \
  --delegated-signer "$DELEGATED_SIGNER" \
  --topology all-in-one \
  --stable-mint EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v,Es9vMFrzaCERmJfrF4H2FYD4KCoNkY6BvByan1KxLAhB \
  --kamino-liquidity-mint EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v,Es9vMFrzaCERmJfrF4H2FYD4KCoNkY6BvByan1KxLAhB \
  --kamino-market 7u3HeHxYDLhnCoErrtycNokbQYbWGzLs6JSDqGAv5PfF,CqAoLuqWtavaVE8deBjMKe8ZfSt9ghR6Vb8nfsyabyHA \
  --swap-lane jupiter \
  --withdraw-action-seed "$POLICY_SEED" \
  "$@"
