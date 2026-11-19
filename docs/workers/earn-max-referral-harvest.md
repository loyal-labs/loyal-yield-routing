# Earn MAX referral fee harvest

Voltr accrues the vault’s 15% performance fee in Earn MAX shares. This job pays the Solana transaction fee to mint accrued shares to the configured admin token account once referral fees qualify. The [fee collector](earn-max-referral-fees.md) verifies the finalized mint and records each friend’s portion. Paying the inviter is a separate settlement step.

## Install a verified release

Run on one designated host with persistent storage. The **Earn MAX referral harvest** GitHub Actions workflow builds a separate Linux binary for amd64 and arm64. A successful main-branch run publishes the executable, both systemd units and a checksum manifest. Installing this job requires no change to the lending services.

Download the artifact from the successful workflow run for the reviewed main commit. Set `run` to its run ID and `sha` to the full approved commit SHA. Run these commands on the authenticated download machine.

```sh
set -eu
repo=loyal-labs/loyal-yield-routing
test "$(gh run view "$run" -R "$repo" --json event,workflowName,conclusion,headBranch,headSha \
  --jq 'select(.event=="push" or .event=="workflow_dispatch") | [.workflowName,.conclusion,.headBranch,.headSha] | join(" ")')" = "Earn MAX referral harvest success main $sha"
gh run download "$run" -R "$repo" --name "referral-harvest-$sha" --dir harvest-artifact
```

Transfer that directory to the designated host. As root, in the transferred directory, set the same approved `sha` and install the release. On an upgrade, first disable the timer with `systemctl disable --now loyal-referral-harvest.timer`, let the current service invocation finish and inspect its last result. Keep the state directory and prior release.

```sh
set -eu
test "$(cat COMMIT)" = "$sha"
sha256sum --check SHA256SUMS
case "$(uname -m)" in x86_64) arch=amd64 ;; aarch64) arch=arm64 ;; *) exit 1 ;; esac
base=/opt/loyal/releases/referral-harvest
install -d -m 0755 "$base"
if test -d "$base/$sha"; then
  cmp "$arch/loyal-referral-harvest" "$base/$sha/loyal-referral-harvest"
else
  stage=$(mktemp -d "$base/.install.XXXXXXXX")
  install -m 0555 "$arch/loyal-referral-harvest" "$stage/loyal-referral-harvest"
  chmod 0755 "$stage"
  mv -T "$stage" "$base/$sha"
fi
ln -sfnT "$sha" "$base/.next"
mv -Tf "$base/.next" "$base/current"
install -m 0644 loyal-referral-harvest.service loyal-referral-harvest.timer /etc/systemd/system/
systemctl daemon-reload
```

## Simulate and enable collection

1. Confirm the referral program launch and opening fee checkpoint with the fee collector. Give the job a read-only Yield database credential and mainnet RPC with finalized transaction history. Use a dedicated, SOL-funded transaction fee payer that holds no vault role or referral assets.
2. In `/etc/loyal/loyal-referral-harvest.env`, set `REFERRAL_HARVEST_PAYER` to that public address. Under `/etc/credstore.encrypted/loyal-referral-harvest/`, install host-encrypted systemd credentials named `REFERRAL_HARVEST_DATABASE_URL` and `REFERRAL_HARVEST_RPC_URL`. Use each filename as the systemd credential name. Stream each value directly from the secret manager to `systemd-creds encrypt --with-key=host --name=NAME - /etc/credstore.encrypted/loyal-referral-harvest/NAME`. The directory should be root-owned and mode 0700.
3. For a first run, leave the execution drop-in absent. For an upgrade, temporarily rename the installed `50-execute.conf` to `50-execute.conf.off` and run `systemctl daemon-reload`. Run `systemctl start loyal-referral-harvest.service` and inspect `journalctl -u loyal-referral-harvest.service -n 20 --no-pager`. A `simulated` result verifies an eligible collection without signing. `no_eligible_fees` is an idle result. `not_launched` or `opening_checkpoint_required` identifies the missing prerequisite.
4. To activate, install the dedicated payer’s Solana keygen JSON array as a third encrypted credential, `REFERRAL_HARVEST_KEYPAIR`. Create the following drop-in at `/etc/systemd/system/loyal-referral-harvest.service.d/50-execute.conf`, or restore it after an upgrade.

```ini
[Service]
Environment=HARVEST_EXECUTION_ARGUMENT=--execute
LoadCredentialEncrypted=REFERRAL_HARVEST_KEYPAIR:/etc/credstore.encrypted/loyal-referral-harvest/REFERRAL_HARVEST_KEYPAIR
```

5. Run `systemctl daemon-reload`, then `systemctl start loyal-referral-harvest.service` for the first execution. Read the journal result. After a `submitted` result, running the same service again checks that signature. Verify the first finalized fee collection against the fee collector. A healthy idle result also allows scheduling. Enable the recurring job with `systemctl enable --now loyal-referral-harvest.timer`. Check the next run with `systemctl list-timers loyal-referral-harvest.timer`. The timer fires hourly, at minute 36 UTC with up to 30 seconds of jitter.

## Recovery

On `submission_unknown`, `held`, or a failed run, stop the timer and inspect the journal and fee collector before resuming. `pending_finality` waits for Solana. `awaiting_fee_observer` waits for the finalized receipt to reach the collector. `reconciled` means the observer accounted for the harvest. `expired` means the previous signature was proved absent past expiry. In execution mode, the job checks a persisted attempt before preparing another.

The service owns its private state at `/var/lib/loyal-referral-harvest`, including `harvest-attempt.json` while an attempt is outstanding. Keep this directory across upgrades and rollbacks. To move the writer, stop its timer, wait for its invocation to finish and preserve its state on the destination before enabling that host. If the journal is missing or corrupt during recovery, reconcile the outstanding signature and vault history before enabling execution. To roll back the binary, stop scheduling, point `current` at the previously verified release, and run its simulation and execution checks with the same state.
