#!/usr/bin/env bash
# Prepares the arm64 AWS host loyal-main to match loyal-workers-1 (services) and loyal-db-primary (Postgres).
# Run on the host as root: ssh loyal-main 'sudo bash -s' < deploy/aws/loyal-main-prepare.sh
# Package installs run with SYSTEMD_OFFLINE=1 and a policy-rc.d that refuses starts, because some postinst scripts call systemctl start themselves.
# Idempotent. Starts and enables nothing, creates no Postgres cluster, copies no unit, release, secret or data.
set -euo pipefail

DB_GB=${DB_GB:-300}
SVC_GB=${SVC_GB:-150}
WANT_HOST=${WANT_HOST:-loyal-main}

PG_VER=18.6-1.pgdg24.04+2
TS_VER=2.24.0~ubuntu24.04-1803
PGBACKREST_VER=2.59.1-1.pgdg24.04+1
PGBACKREST_URL=https://apt-archive.postgresql.org/pub/repos/apt/pool/main/p/pgbackrest/pgbackrest_${PGBACKREST_VER}_arm64.deb
PGBACKREST_SHA256=58c24e70b8520503262b755dd4fc7891650b8916ed3c84d3ed2ab118fbf40ab6
CLOUDFLARED_VER=2026.10.2
CLOUDFLARED_URL=https://github.com/cloudflare/cloudflared/releases/download/${CLOUDFLARED_VER}/cloudflared-linux-arm64.deb
CLOUDFLARED_SHA256=2e5335ea0ef2fa98e5354357e77aaf453f3756d1f9023cf5b063409e1fb3baca
OTELCOL_VER=0.162.0
OTELCOL_URL=https://github.com/open-telemetry/opentelemetry-collector-releases/releases/download/v${OTELCOL_VER}/otelcol-contrib_${OTELCOL_VER}_linux_arm64.deb
OTELCOL_SHA256=b6163e6639989ac608ae2ba58b891128f2f31bba1b0af31a937ac5d0de7b0c23
CADDY_VER=2.11.7

changes=0
change() { changes=$((changes + 1)); echo "changed: $*"; }
die() { echo "loyal-main-prepare.sh: $*" >&2; exit 1; }

[[ $(id -u) -eq 0 ]] || die "run as root"
[[ $(uname -m) == aarch64 ]] || die "this script prepares an arm64 host, found $(uname -m)"
export DEBIAN_FRONTEND=noninteractive NEEDRESTART_SUSPEND=1

put() { # put <mode> <path>: file from stdin, written only when different
  local mode=$1 path=$2 tmp
  tmp=$(mktemp "$path.XXXXXX")
  cat >"$tmp"
  chmod "$mode" "$tmp"
  if [[ -f $path && $(cmp -s "$tmp" "$path" && echo same) == same && $(stat -c %a "$path") == "${mode#0}" ]]; then
    rm "$tmp"
  else
    mv "$tmp" "$path"
    change "$path"
  fi
}

dir() { # dir <mode> <owner:group> <path>
  local mode=$1 own=$2 path=$3
  if [[ ! -d $path ]]; then
    install -d -m "$mode" -o "${own%%:*}" -g "${own##*:}" "$path"
    change "$path created $mode $own"
    return
  fi
  local have
  have=$(stat -c '%a %u:%g' "$path")
  local want_uid want_gid
  want_uid=$(id -u "${own%%:*}" 2>/dev/null || echo "${own%%:*}")
  want_gid=$(getent group "${own##*:}" | cut -d: -f3 || true)
  want_gid=${want_gid:-${own##*:}}
  if [[ $have != "${mode#0} $want_uid:$want_gid" ]]; then
    chown "$own" "$path"
    chmod "$mode" "$path"
    change "$path set to $mode $own"
  fi
}

hostname_step() {
  if [[ $(hostnamectl --static) != "$WANT_HOST" ]]; then
    hostnamectl set-hostname "$WANT_HOST"
    change "hostname $WANT_HOST"
  fi
  put 0644 /etc/cloud/cloud.cfg.d/99-loyal-hostname.cfg <<<'preserve_hostname: true'
  if ! grep -qE "^127\.0\.1\.1[[:space:]]+$WANT_HOST\$" /etc/hosts; then
    echo "127.0.1.1 $WANT_HOST" >>/etc/hosts
    change "/etc/hosts 127.0.1.1 $WANT_HOST"
  fi
}

pick_disk() { # pick_disk <gb>: the one blank whole disk of exactly that size
  local want=$(($1 * 1073741824)) d found=()
  while read -r d; do
    if [[ -z $(blkid -p -o value -s TYPE "$d" 2>/dev/null) && -z $(lsblk -nro NAME "$d" | tail -n +2) ]]; then
      found+=("$d")
    fi
  done < <(lsblk -bdnpro NAME,SIZE,TYPE | awk -v w="$want" '$3 == "disk" && $2 == w {print $1}')
  ((${#found[@]} == 1)) || die "expected one blank $1 GB disk, found ${#found[@]}"
  echo "${found[0]}"
}

volume() { # volume <gb> <mountpoint>: ext4, mounted by UUID
  local gb=$1 mp=$2 dev uuid
  if ! awk -v m="$mp" '$1 !~ /^#/ && $2 == m {f = 1} END {exit !f}' /etc/fstab; then
    dev=$(pick_disk "$gb")
    mkfs.ext4 -q "$dev"
    uuid=$(blkid -s UUID -o value "$dev")
    echo "UUID=$uuid $mp ext4 defaults,noatime,nofail,x-systemd.device-timeout=60 0 2" >>/etc/fstab
    systemctl daemon-reload
    change "$dev formatted ext4, fstab UUID=$uuid $mp"
  fi
  mkdir -p "$mp"
  mountpoint -q "$mp" || { mount "$mp"; change "mounted $mp"; }
}

bind() { # bind <source> <target>
  local src=$1 dst=$2
  if ! awk -v m="$dst" '$1 !~ /^#/ && $2 == m {f = 1} END {exit !f}' /etc/fstab; then
    echo "$src $dst none bind,nofail 0 0" >>/etc/fstab
    systemctl daemon-reload
    change "fstab bind $src $dst"
  fi
  mkdir -p "$dst"
  mountpoint -q "$dst" || { mount "$dst"; change "mounted $dst"; }
}

disks_step() {
  volume "$DB_GB" /srv/postgresql
  dir 0755 root:root /srv/postgresql
  volume "$SVC_GB" /srv/loyal-services
  dir 0755 root:root /srv/loyal-services
  dir 0755 root:root /srv/loyal-services/opt-loyal
  dir 2775 101:101 /srv/loyal-services/loyal-clickstack
  bind /srv/loyal-services/opt-loyal /opt/loyal
  bind /srv/loyal-services/loyal-clickstack /srv/loyal-clickstack
}

tree_step() {
  local d
  dir 0755 root:root /opt/loyal-hetzner
  for d in assets assets/web bin diagnostics dryrun ops releases \
    releases/admin releases/backyard releases/earn-max releases/observer releases/ops \
    releases/realtime releases/retail releases/stats releases/telegram releases/web; do
    dir 0755 root:root "/opt/loyal/$d"
  done
  dir 0755 root:root /etc/loyal
  dir 0700 root:root /etc/credstore.encrypted
  for d in alertmanager loyal-backyard loyal-clickstack loyal-clickstack-relay loyal-earn-max \
    loyal-observer loyal-realtime loyal-retail loyal-telegram otelcol; do
    dir 0700 root:root "/etc/credstore.encrypted/$d"
  done
  dir 0755 root:root /etc/loyal-db
  dir 0700 root:root /etc/loyal-db/replica
  dir 0755 root:root /var/lib/prometheus/node-exporter
}

policy_rc_d=''
no_start() {
  if [[ ! -e /usr/sbin/policy-rc.d ]]; then
    printf '#!/bin/sh\nexit 101\n' >/usr/sbin/policy-rc.d
    chmod 755 /usr/sbin/policy-rc.d
    policy_rc_d=1
  fi
}
trap '[[ -z $policy_rc_d ]] || rm -f /usr/sbin/policy-rc.d' EXIT

installed() { dpkg-query -W -f '${db:Status-Abbrev}${Version}' "$1" 2>/dev/null | sed -n 's/^[ih]i //p' || true; }

apt_pin() { # apt_pin <pkg=ver>...: installs the ones not at that version
  local todo=() spec
  for spec in "$@"; do
    [[ $(installed "${spec%%=*}") == "${spec#*=}" ]] || todo+=("$spec")
  done
  ((${#todo[@]} == 0)) && return 0
  SYSTEMD_OFFLINE=1 apt-get install -y --no-install-recommends --allow-downgrades -o Dpkg::Options::=--force-confold "${todo[@]}" >/dev/null
  change "apt ${todo[*]}"
}

apt_any() { # apt_any <pkg>...: installs the ones that are missing, any version
  local todo=() p
  for p in "$@"; do [[ -n $(installed "$p") ]] || todo+=("$p"); done
  ((${#todo[@]} == 0)) && return 0
  SYSTEMD_OFFLINE=1 apt-get install -y --no-install-recommends -o Dpkg::Options::=--force-confold "${todo[@]}" >/dev/null
  change "apt ${todo[*]}"
}

fetch_deb() { # fetch_deb <pkg> <version> <url> <sha256>
  local pkg=$1 ver=$2 url=$3 sha=$4 tmp
  [[ $(installed "$pkg") == "$ver" ]] && return 0
  tmp=$(mktemp -d)
  curl -fsSL "$url" -o "$tmp/$pkg.deb"
  [[ $(sha256sum "$tmp/$pkg.deb" | cut -d' ' -f1) == "$sha" ]] || die "$pkg: sha256 mismatch for $url"
  SYSTEMD_OFFLINE=1 apt-get install -y --no-install-recommends --allow-downgrades -o Dpkg::Options::=--force-confold "$tmp/$pkg.deb" >/dev/null
  rm -f "$tmp/$pkg.deb"
  rmdir "$tmp"
  change "dpkg $pkg $ver"
}

repo() { # repo <name> <key-url> <armored|binary> <keyring> <list line>
  local name=$1 keyurl=$2 kind=$3 ring=$4 line=$5 tmp
  if [[ ! -s $ring ]]; then
    tmp=$(mktemp)
    curl -fsSL "$keyurl" -o "$tmp"
    if [[ $kind == armored ]]; then install -m 0644 "$tmp" "$ring"; else gpg --dearmor <"$tmp" >"$ring"; chmod 0644 "$ring"; fi
    rm -f "$tmp"
    change "$ring"
  fi
  put 0644 "/etc/apt/sources.list.d/$name.list" <<<"$line"
}

packages_step() {
  apt-get update -qq
  apt_any ca-certificates curl gnupg
  repo pgdg https://www.postgresql.org/media/keys/ACCC4CF8.asc armored /usr/share/keyrings/pgdg.asc \
    'deb [signed-by=/usr/share/keyrings/pgdg.asc] https://apt.postgresql.org/pub/repos/apt noble-pgdg main'
  repo timescale https://packagecloud.io/timescale/timescaledb/gpgkey binary /usr/share/keyrings/timescale.gpg \
    'deb [signed-by=/usr/share/keyrings/timescale.gpg] https://packagecloud.io/timescale/timescaledb/ubuntu/ noble main'
  repo caddy-stable https://dl.cloudsmith.io/public/caddy/stable/gpg.key binary /usr/share/keyrings/caddy-stable-archive-keyring.gpg \
    'deb [signed-by=/usr/share/keyrings/caddy-stable-archive-keyring.gpg] https://dl.cloudsmith.io/public/caddy/stable/deb/debian any-version main'
  apt-get update -qq

  no_start
  put 0644 /etc/default/prometheus <<<'ARGS="--web.listen-address=127.0.0.1:9090 --storage.tsdb.retention.time=30d"'
  put 0644 /etc/default/prometheus-alertmanager <<<'ARGS="--web.listen-address=127.0.0.1:9093 --cluster.listen-address="'
  put 0644 /etc/default/prometheus-node-exporter <<<'ARGS="--web.listen-address=127.0.0.1:9100 --collector.systemd --collector.systemd.unit-include=(loyal|xstock)-.+\.service --collector.systemd.enable-restarts-metrics --collector.textfile.directory=/var/lib/prometheus/node-exporter"'

  apt_any curl git jq rsync nftables iptables wireguard-tools sqlite3 apt-transport-https fio openssl wget
  apt_pin \
    postgresql-common=293.pgdg24.04+1 postgresql-client-common=293.pgdg24.04+1 \
    libpq5="$PG_VER" postgresql-client-18="$PG_VER"
  if ! grep -q '^create_main_cluster = false' /etc/postgresql-common/createcluster.conf; then
    sed -i 's/^#\?create_main_cluster.*/create_main_cluster = false/' /etc/postgresql-common/createcluster.conf
    change "/etc/postgresql-common/createcluster.conf create_main_cluster = false"
  fi
  apt_pin \
    postgresql-18="$PG_VER" postgresql-18-repack=1.5.3-1.pgdg24.04+2 \
    timescaledb-2-loader-postgresql-18="$TS_VER" timescaledb-2-postgresql-18="$TS_VER" \
    pgbouncer=1.26.0-1.pgdg24.04+1 python3-psycopg2=2.9.10-1.pgdg24.04+1
  apt-mark hold postgresql-18 postgresql-18-repack postgresql-client-18 \
    timescaledb-2-loader-postgresql-18 timescaledb-2-postgresql-18 >/dev/null
  fetch_deb pgbackrest "$PGBACKREST_VER" "$PGBACKREST_URL" "$PGBACKREST_SHA256"
  apt-mark hold pgbackrest >/dev/null

  apt_pin \
    containerd=2.2.1-0ubuntu1~24.04.3 runc=1.3.4-0ubuntu1~24.04.1 docker.io=29.1.3-0ubuntu3~24.04.2 \
    docker-compose-v2=2.40.3+ds1-0ubuntu1~24.04.1 \
    prometheus=2.45.3+ds-2ubuntu0.3 prometheus-alertmanager=0.26.0+ds-1ubuntu0.3 \
    prometheus-node-exporter=1.7.0-1ubuntu0.3 prometheus-node-exporter-collectors=0.0~git20231018.f5c56e7-1 \
    caddy="$CADDY_VER"
  fetch_deb cloudflared "$CLOUDFLARED_VER" "$CLOUDFLARED_URL" "$CLOUDFLARED_SHA256"
  fetch_deb otelcol-contrib "$OTELCOL_VER" "$OTELCOL_URL" "$OTELCOL_SHA256"
}

postgres_dirs_step() {
  dir 0700 postgres:postgres /srv/postgresql/18
  dir 0750 postgres:postgres /var/lib/pgbackrest
  dir 0750 postgres:postgres /var/log/pgbackrest
  dir 0750 root:postgres /etc/pgbackrest
}

quiet_step() {
  local pkg unit state
  for pkg in postgresql-common postgresql-18 pgbouncer pgbackrest docker.io containerd prometheus \
    prometheus-alertmanager prometheus-node-exporter prometheus-node-exporter-collectors caddy cloudflared otelcol-contrib; do
    while read -r unit; do
      [[ $unit == *@.* ]] && continue
      state=$(systemctl is-enabled "$unit" 2>/dev/null || true)
      if [[ $state == enabled ]]; then
        systemctl disable "$unit" >/dev/null 2>&1
        change "disabled $unit"
      fi
      if systemctl is-active --quiet "$unit"; then
        systemctl stop "$unit"
        change "stopped $unit"
      fi
    done < <(dpkg -L "$pkg" 2>/dev/null | sed -n 's#^/\(usr/\)\?lib/systemd/system/\([^/]*\.\(service\|socket\|timer\|path\)\)$#\2#p' | sort -u)
  done
}

hostname_step
disks_step
tree_step
packages_step
postgres_dirs_step
quiet_step

echo "loyal-main: $changes change(s)"
