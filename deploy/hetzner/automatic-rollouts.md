# Automatic production rollouts

The `Production rollout` workflow checks the exact merged main commit, then
deploys Backyard, Earn MAX, retail, observer, Telegram, web, admin, stats and
realtime sequentially. It requires a merged main PR with passing checks, the
worker/database/SVM and monitoring checks, web/mobile checks, browser journeys,
and the realtime image probe. Deployment uses binaries and assets from that
same workflow run. PR jobs receive no production SSH credentials.

## Activate once

Review and merge the workflow before enabling production access. Publishing,
merging and the first production rollout need the owner's authorization.

1. Create the GitHub `production` environment and restrict deployment branches
   to `main`. For unattended deployment, do not require an environment reviewer.
   Repository branch rules and the workflow's merged-PR gate still apply.
2. Provision a dedicated deployment SSH key on the workers host. Store its
   private half as environment secret `LOYAL_WORKERS_DEPLOY_KEY`; do not copy a
   developer's personal key. This identity has privileged deployment access.
   Use the `restrict` authorized-key option to disable SSH forwarding and PTYs.
3. Store the independently verified host public key in environment secret
   `LOYAL_WORKERS_KNOWN_HOSTS`, in OpenSSH known_hosts format, using the hostname
   or IP in environment variable `LOYAL_WORKERS_HOST`. Host key checking is
   strict; a changed/missing key fails before deployment. Configure the private
   network below before enabling CI; `LOYAL_WORKERS_HOST` remains the public
   endpoint, while SSH uses the tunnel and pins that endpoint with `HostKeyAlias`.
4. Set repository variable `NEXT_PUBLIC_PRIVY_APP_ID`. Copy the production
   application's other public build settings into the matching repository
   variables listed in `.github/workflows/web-go.yml`; their allowlist lives in
   `web/apps/loyal/vite/public-env.ts`. Unset optional variables keep application
   defaults. These values are compiled into the browser bundle and must be
   public. Signing keys and database URLs stay in the host's encrypted systemd
   credential store.
5. Reconcile the existing releases before the first run. All nine services must
   have a known main-branch release, executable previous binary, healthy process
   and supported readiness/metrics endpoints. Migration and systemd changes
   since any running release require an explicit reviewed rollout first. The
   automation does not apply migrations or install service units. Host tools
   include Python 3, PostgreSQL's `psql`, systemd, curl, sha256sum and ldd.
6. From a clean checkout of the current merged main SHA, with the existing
   `loyal-workers` SSH alias, run the read-only gate:

   ```sh
   python3 scripts/production-rollout.py "$(git rev-parse HEAD)" --preflight-only
   ```

   It creates/removes a temporary helper directory and transient read-only
   credential processes. It never activates a service. Success ends with
   `Read-only preflight passed for all services. No deployment performed.`
7. Run `Production rollout` on `main`, then inspect every service's verification
   line in the GitHub job summary. Subsequent runtime changes merged into main
   run it automatically. Changes outside the workflow's runtime/deployment
   paths do not trigger a rollout.

## Private CI network

Keep Hetzner's TCP 22 source restriction. Add a separate inbound **UDP 51820**
rule from `0.0.0.0/0` to the workers firewall. Standard GitHub runners have
changing source addresses; WireGuard authenticates the dedicated peer before
accepting tunnel traffic. Only the protected production job receives its key.

The host uses `10.203.0.1/32`, the CI peer `10.203.0.2/32`, and the interface is
`loyal-ci`. Check for route/address and UDP port conflicts before installation.
No default route, DNS change, NAT or forwarding is needed. The dedicated
`inet loyal_ci` nftables table permits the peer to reach SSH and ping on the
tunnel address, drops every other tunnel input, and drops all forwarded tunnel
traffic. It does not change Docker's rules or the public SSH restriction.

One-time host provisioning, with owner authorization:

1. Install `wireguard-tools` and `nftables`. Generate separate host and CI keys
   with `wg genkey`, holding private values in memory. Derive public keys using
   `wg pubkey` on stdin. Send the CI private key directly to
   `gh secret set LOYAL_WORKERS_WIREGUARD_KEY --env production` on stdin; never
   save it in a config file or pass it as an argument. Set production variable
   `LOYAL_WORKERS_WIREGUARD_PUBLIC_KEY` to the host's public key.
2. Encrypt the host private key on the host by piping it to
   `systemd-creds encrypt --name=private-key --with-key=host - /etc/credstore.encrypted/loyal-ci-wireguard.cred`.
   Set mode `0600`. The host private key never leaves the host.
3. Install `ci-network/loyal-ci.conf` as `/etc/wireguard/loyal-ci.conf`, replacing
   `CI_PUBLIC_KEY` with the CI public key, and install `ci-network/loyal-ci.nft`
   as `/etc/wireguard/loyal-ci.nft`, both mode `0600`. Install
   `ci-network/credentials.conf` under
   `/etc/systemd/system/wg-quick@loyal-ci.service.d/credentials.conf`.
   `LoadCredentialEncrypted` decrypts the host key into systemd's runtime
   credential directory; the interface reads it there, never from a plaintext
   persistent config. `SaveConfig` must remain disabled.
4. Run `nft --check -f /etc/wireguard/loyal-ci.nft`, then
   `systemctl daemon-reload` and `systemctl enable --now wg-quick@loyal-ci`.
   Verify `systemctl is-active wg-quick@loyal-ci`, `wg show loyal-ci public-key`,
   `wg show loyal-ci allowed-ips` and `nft list table inet loyal_ci`.
   Never use `wg showconf` or `wg show ... dump` in logs; those expose private keys.
5. Run the production workflow. `Private workers tunnel connected` proves the
   authenticated tunnel works. The following SSH preflight also verifies the
   existing host-key pin and deployment identity. A tunnel timeout means UDP
   access, key pairing or the host unit is wrong; it happens before activation.

The packaged `wg-quick@loyal-ci` unit restores the interface at boot and removes
its firewall table after a stop or failed start. CI removes its interface after
every run, including failures. Rotate the CI peer and its GitHub secret together
while no production job is active. Disable access with
`systemctl disable --now wg-quick@loyal-ci` and remove the UDP rule; existing
public SSH access remains available. Worker signing and database credentials
are not part of this network setup.

`sudo bash scripts/ci-worker-network_test.sh` verifies real handshakes, SSH
access, blocked ports/forwarding and failed-connection cleanup in temporary
network namespaces using generated test keys. It needs Linux, WireGuard,
nftables and Python; CI runs it without production credentials.

## What the gates mean

`Configure repository variable NEXT_PUBLIC_PRIVY_APP_ID` or `Configure the
production environment SSH key...` means setup is incomplete. No service has
been activated. `ROLLOUT BLOCKED` or `ROLLOUT HALTED` means the configuration was
loaded but a safety/health gate refused progress; inspect the named service.
Missing evidence is a failure, not a successful empty check.

Before the first mutation, all services pass identity/health and migration/unit
checks. Before each activation, the workflow rechecks main's SHA and that
service's current release. The host lock and expected-release comparison prevent
overlapping or unexpected replacements. The vaults drain using the existing
scoped in-flight gates; stopped services are never started implicitly.
The observed executable checksum must match the verified artifact. If an already
running SHA has different binary bytes, the workflow requires a new commit
instead of silently replacing or accepting that release.

After each activation, a new worker has up to 60 seconds to publish its first
lane observation, with all failure, restart and vault guards active throughout.
The full 120-second observation then starts, polling every five seconds.
It requires fresh progress and vault position observations,
unchanged persisted target levels, exit requests and release permissions, no
recovery latch, no newly failed/held operation, no new error counter/journal
error, and no exit/restart. Selector sampling timestamps and transient economic
approvals may advance normally. Web/admin/stats also serve the expected release
header; Telegram and realtime pass their own health endpoints. The watch covers
each service's observation window; ongoing monitoring remains responsible after
that window. Fresh observations do not prove that every money-moving route was
exercised. New transaction shapes still need real-wire preparation and
persist/sign validation tests before merge.

## Failure, retries and recovery

A failed activation or watch stops the sequence immediately. The summary names
each service already verified and the service where it stopped; remaining
services have not been verified on the target release. The workflow never
cancels an active rollout when another merge arrives. It refuses an obsolete SHA
before starting the next service.

The activation scripts retain their guarded restoration behavior. A failed
post-deploy watch requests restoration of the recorded previous binary through
the same host lock and in-flight gates. If pending/signed work prevents safe
restoration, the job reports that and stops. It never clears a recovery latch,
changes permissions/targets, drops signed work or repeatedly restarts a worker.
An SSH interruption can leave the host activation running to completion; inspect
the host before retrying, because the CI failure does not establish its outcome.

Host logs are `/var/log/loyal-deploy/<family>-<sha>.log`; the GitHub job summary
records completed observation gates. Inspect `systemctl status loyal-<family>`
and its journal for the failing service. Use the existing reviewed manual
deployment procedure when intervention is needed. After resolving the cause,
rerun the whole production workflow on the current main SHA. A previous rollout
failure does not invalidate the successful main verification marker. Each retry
still reruns the verification graph and repeats preflight/observation.

To suspend automation, disable `Production rollout` in GitHub Actions. An
activation already started on the host must still be inspected to completion.
