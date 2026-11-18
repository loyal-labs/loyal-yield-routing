#!/bin/bash
# robinx-main host prep: hostname, data and mnemonic disks, Postgres tools.
# Run as root on robinx-main with wen's deploy/postgres/prepare-host.sh beside it. Rerun prints "host-prep: 0 change(s)".
set -euo pipefail
[[ $EUID = 0 ]] || { echo 'host-prep: run as root' >&2; exit 1; }
HERE=$(cd "$(dirname "$0")" && pwd)
changes=0
changed() { changes=$((changes + 1)); echo "changed: $*"; }

NAME=robinx-main
DATA_DEV=/dev/disk/by-id/nvme-Amazon_Elastic_Block_Store_vol0a554a834b3e7230c
DATA_DIR=/srv/robinx-data
DATA_OPTS=defaults,prjquota,noatime,X-fstrim.notrim,nofail
MNEMONIC_DEV=/dev/disk/by-id/nvme-Amazon_Elastic_Block_Store_vol05817a5160105c2a9
MNEMONIC_DIR=/var/lib/mnemonic
MNEMONIC_OPTS=defaults,noatime,nofail

if [[ ! -f /etc/cloud/cloud.cfg.d/99-robinx-hostname.cfg ]]; then
  echo 'preserve_hostname: true' >/etc/cloud/cloud.cfg.d/99-robinx-hostname.cfg
  changed "cloud-init keeps the hostname across boots"
fi
if [[ $(hostname) != "$NAME" ]]; then
  hostnamectl set-hostname "$NAME"
  changed "hostname $NAME"
fi
if ! grep -qE "^127\.0\.1\.1[[:space:]]+$NAME\$" /etc/hosts; then
  sed -i "/^127\.0\.1\.1[[:space:]]/d" /etc/hosts
  sed -i "1a 127.0.1.1 $NAME" /etc/hosts
  changed "/etc/hosts resolves $NAME"
fi

disk() {
  local dev=$1 dir=$2 fs=$3 opts=$4 label=$5 have uuid
  dev=$(readlink -f "$dev")
  [[ -b $dev ]] || { echo "host-prep: $1 is not a block device" >&2; exit 1; }
  have=$(blkid -s TYPE -o value "$dev" || true)
  if [[ -z $have ]]; then
    [[ -z $(lsblk -no NAME "$dev" | tail -n +2) ]] || { echo "host-prep: $dev has partitions, refusing to format" >&2; exit 1; }
    if [[ $fs == xfs ]]; then mkfs.xfs -q -L "$label" "$dev"; else mkfs.ext4 -q -L "$label" "$dev"; fi
    changed "$dev formatted $fs ($label)"
  elif [[ $have != "$fs" ]]; then
    echo "host-prep: $dev holds $have, expected $fs; refusing" >&2; exit 1
  fi
  uuid=$(blkid -s UUID -o value "$dev")
  if ! grep -qE "^UUID=$uuid[[:space:]]+$dir[[:space:]]+$fs[[:space:]]+$opts[[:space:]]" /etc/fstab; then
    grep -qE "[[:space:]]$dir[[:space:]]" /etc/fstab && { echo "host-prep: /etc/fstab already mounts something else at $dir" >&2; exit 1; }
    printf 'UUID=%s %s %s %s 0 0\n' "$uuid" "$dir" "$fs" "$opts" >>/etc/fstab
    systemctl daemon-reload
    changed "/etc/fstab mounts UUID=$uuid at $dir ($opts)"
  fi
  if [[ $(findmnt -no SOURCE "$dir" 2>/dev/null) != "$dev" ]]; then
    install -d -m 0755 "$dir"
    mount "$dir"
    changed "$dir mounted"
  fi
}
disk "$DATA_DEV" "$DATA_DIR" xfs "$DATA_OPTS" robinx-data
disk "$MNEMONIC_DEV" "$MNEMONIC_DIR" ext4 "$MNEMONIC_OPTS" mnemonic

installed=$(dpkg-query -W -f='${Status}\n' postgresql-18 pgbackrest 2>/dev/null | grep -c 'install ok installed' || true)
if [[ $installed != 2 ]]; then
  bash "$HERE/prepare-host.sh"
  changed "Postgres 18 and pgbackrest installed from PGDG $(dpkg --print-architecture) by wen prepare-host.sh"
fi
if [[ $(stat -c %U "$DATA_DIR") != postgres ]]; then
  chown postgres:postgres "$DATA_DIR"
  chmod 0750 "$DATA_DIR"
  changed "$DATA_DIR owned by postgres"
fi
if ! grep -qxF "data_directory = '$DATA_DIR/%c'" /etc/postgresql-common/createcluster.conf; then
  sed -i '/^[[:space:]]*data_directory[[:space:]]*=/d' /etc/postgresql-common/createcluster.conf
  printf "data_directory = '%s/%%c'\n" "$DATA_DIR" >>/etc/postgresql-common/createcluster.conf
  changed "pg_createcluster puts each cluster in $DATA_DIR/<name>"
fi
/usr/lib/postgresql/18/bin/postgres --version
pgbackrest version
echo "host-prep: $changes change(s)"
