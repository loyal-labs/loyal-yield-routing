#!/usr/bin/env bash
# Prepares the arm64 AWS host weather-live to carry the live weather bot (switch S3): the w20 user, directories, packages, persistent journal.
# Run on the host as root: ssh weather-live 'sudo bash -s' < deploy/aws/weather-live-prepare.sh
# Idempotent: a rerun prints "weather-live-prepare: 0 change(s)". Starts and enables nothing, copies no unit, binary, secret or data (s3-weather.sh prepare does that from the old box).
set -euo pipefail

W20_UID=999
W20_GID=988
PKGS=(tzdata rsync xz-utils python3 curl jq)

changes=0
change() { changes=$((changes + 1)); echo "changed: $*"; }
die() { echo "weather-live-prepare: $*" >&2; exit 1; }

[[ $(id -u) -eq 0 ]] || die "run as root"
[[ $(uname -m) == aarch64 ]] || die "this script prepares an arm64 host, found $(uname -m)"
export DEBIAN_FRONTEND=noninteractive NEEDRESTART_SUSPEND=1

dir() { # dir <mode> <owner:group> <path>
  local mode=$1 own=$2 path=$3 want
  if [[ ! -d $path ]]; then
    install -d -m "$mode" -o "${own%%:*}" -g "${own##*:}" "$path"
    change "$path created $mode $own"
    return
  fi
  want="$mode $(id -u "${own%%:*}"):$(getent group "${own##*:}" | cut -d: -f3)"
  if [[ $(stat -c '%a %u:%g' "$path") != "${want#0}" ]]; then
    chown "$own" "$path"
    chmod "$mode" "$path"
    change "$path set to $mode $own"
  fi
}

group_user_step() {
  if ! getent group w20 >/dev/null; then
    groupadd --gid "$W20_GID" w20
    change "group w20 gid $W20_GID"
  fi
  if ! getent passwd w20 >/dev/null; then
    useradd --system --uid "$W20_UID" --gid "$W20_GID" --home-dir /srv/w20 --no-create-home --shell /usr/sbin/nologin w20
    change "user w20 uid $W20_UID"
  fi
  [[ $(id -u w20) == "$W20_UID" && $(id -g w20) == "$W20_GID" ]] || die "w20 exists with another uid or gid; the data copy keeps numeric ids"
}

packages_step() {
  local p missing=()
  for p in "${PKGS[@]}"; do
    dpkg -s "$p" >/dev/null 2>&1 || missing+=("$p")
  done
  if ((${#missing[@]})); then
    apt-get install -y -qq "${missing[@]}" >/dev/null
    change "apt install ${missing[*]}"
  fi
}

tree_step() {
  dir 0755 root:root /srv/w20
  dir 0755 root:root /srv/w20/bin
  dir 0755 root:root /srv/w20/policy
  dir 0755 root:root /srv/w20/ref
  dir 0755 root:root /srv/w20/band
  dir 0755 root:root /srv/w20/reward-pilot
  dir 0755 w20:w20 /srv/w20/scorecard
  dir 0755 w20:w20 /srv/w20/data
  dir 0755 w20:w20 /srv/w20/out
  dir 0755 root:root /srv/w20/w2-weather
  dir 0755 w20:w20 /srv/w20/w2-weather/data
  dir 0700 root:root /etc/w20
  dir 0700 root:root /etc/w20/reward-pilot
  dir 0700 root:root /var/lib/private
  if [[ ! -e /srv/w20/w2-weather/recorder.log ]]; then
    install -m 0644 -o w20 -g w20 /dev/null /srv/w20/w2-weather/recorder.log
    change "/srv/w20/w2-weather/recorder.log created"
  fi
}

journal_step() {
  if [[ ! -d /var/log/journal ]]; then
    install -d -m 2755 -o root -g systemd-journal /var/log/journal
    systemd-tmpfiles --create --prefix /var/log/journal
    change "persistent journal enabled (the switch reads quotes and fills from it)"
  fi
}

group_user_step
packages_step
tree_step
journal_step

echo "weather-live-prepare: $changes change(s)"
