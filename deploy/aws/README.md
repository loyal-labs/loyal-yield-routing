# deploy/aws

## box.sh

`deploy/aws/box.sh <box> [--dry-run]` builds one AWS box from `deploy/aws/boxes/<box>.conf` and converges on rerun. A second run prints `<box>: 0 change(s)`. Boxes: `robinx-main`, `loyal-main`, `xstock-live`, `weather-live`.

Per box it ensures, each found again by its Name tag: a key pair imported from the conf's public key, a security group (tcp 22 from anywhere, nothing else), the instance (Ubuntu 24.04 for the conf's architecture, AMI read from Canonical's SSM parameter, IMDSv2 required, termination protection on), its gp3 volumes with size, IOPS and throughput from `VOLUMES`, attached and tagged, and one Elastic IP associated. It never deletes, stops or terminates. A type, size or extra-rule mismatch on an existing box is a `warning:` line, not a change. Formatting and mounting the data volumes is not its job.

Conf fields, all required, none with a default: `PROFILE` (or `AWS_PROFILE`), `REGION` (or `AWS_REGION`), `INSTANCE_TYPE`, `ARCH` (`arm64` or `x86_64`), `PUBKEY` (or `BOX_PUBKEY`; a path to a `.pub` file, never a private key), `VOLUMES` (`name:GB[:iops[:MB/s]]`, with `root` first). `ACCOUNT` is optional and, when set, must match the profile's account. `PROFILE` is empty for `robinx-main` and `xstock-live` until the vCPU quota read picks `main` or the new account, and `REGION` is empty for `weather-live` until the Polymarket probe picks one.

`--dry-run` prints every aws command it would run and changes nothing. Its reads (`sts get-caller-identity` first) need a live session; without one it says so in one line and lists every step as missing.
