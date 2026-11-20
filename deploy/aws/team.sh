#!/usr/bin/env bash
# Team access to the Loyal box on AWS (loyal-main). Reads the account, region and box name from boxes/loyal-main.conf. Guide: deploy/aws/TEAM.md.
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
export AWS_PAGER='' AWS_EC2_METADATA_DISABLED=true

usage() {
  cat >&2 <<EOF
usage: $0 <check|status|ssh [command...]|run '<command>'|deploy <family> [sha] [flags]>
  check    proves your AWS session is in the Loyal account and that the box answers. Run it first.
  status   says whether production runs on loyal-main yet, and lists every Loyal unit there.
  ssh      opens a shell as ubuntu (sudo without a password), or runs one command. Your public key is valid on the box for 60 s per call.
  run      runs one command on the box as root through Session Manager. Needs no ssh key and no open port.
  deploy   runs scripts/deploy-workers.sh (bun run deploy) against loyal-main as ubuntu with sudo, over one ssh connection.
Environment: LOYAL_AWS_PROFILE (default loyal-team), LOYAL_SSH_PUBKEY (default ~/.ssh/id_ed25519.pub).
EOF
  exit 2
}

die() { echo "team.sh: $*" >&2; exit 1; }

ACCOUNT='' REGION='' BOX_SSH=''
# shellcheck source=/dev/null
source "$HERE/boxes/loyal-main.conf"
BOX=$BOX_SSH
export AWS_PROFILE=${LOYAL_AWS_PROFILE:-loyal-team} AWS_REGION=$REGION
KNOWN_HOSTS=$HOME/.ssh/known_hosts.$BOX

command -v aws >/dev/null || die "aws CLI not found. Install version 2: brew install awscli"

session() {
  local got
  got=$(aws sts get-caller-identity --query Account --output text 2>/dev/null) ||
    die "no AWS session for profile $AWS_PROFILE. Run: aws configure set region $REGION --profile $AWS_PROFILE && aws login --profile $AWS_PROFILE"
  [[ $got == "$ACCOUNT" ]] ||
    die "profile $AWS_PROFILE is signed in to account $got, not the Loyal account $ACCOUNT. Run aws login --profile $AWS_PROFILE again and pick the role loyal-team-admin"
}

instance() {
  local out
  out=$(aws ec2 describe-instances --filters "Name=tag:Name,Values=$BOX" Name=instance-state-name,Values=running \
    --query 'Reservations[].Instances[].[InstanceId,PublicIpAddress]' --output text) || die "describe-instances failed"
  [[ $(grep -c . <<<"$out") == 1 ]] || die "expected one running instance named $BOX in account $ACCOUNT, found: ${out:-none}"
  read -r INSTANCE ADDRESS <<<"$out"
  [[ $INSTANCE == i-* && $ADDRESS =~ ^[0-9.]+$ ]] || die "could not read the instance id and address of $BOX: $out"
}

on_box() {
  local params id state='' i
  params=$(python3 -c 'import json,sys; print(json.dumps({"commands": [sys.argv[1]]}))' "$1") || die "python3 is needed to quote the command"
  id=$(aws ssm send-command --instance-ids "$INSTANCE" --document-name AWS-RunShellScript --parameters "$params" \
    --query Command.CommandId --output text) || die "Session Manager refused the command"
  for ((i = 0; i < 150; i++)); do
    state=$(aws ssm get-command-invocation --command-id "$id" --instance-id "$INSTANCE" --query Status --output text 2>/dev/null || true)
    case $state in
      Success | Failed | Cancelled | TimedOut) break ;;
    esac
    sleep 2
  done
  aws ssm get-command-invocation --command-id "$id" --instance-id "$INSTANCE" --query '[StandardOutputContent,StandardErrorContent]' --output json |
    python3 -c 'import json,sys; out,err=json.load(sys.stdin); sys.stdout.write(out); sys.stderr.write(err)'
  [[ $state == Success ]] || die "the command on $BOX ended $state (command id $id)"
}

cmd_check() {
  local ping
  session
  echo "ok session: account $ACCOUNT as $(aws sts get-caller-identity --query Arn --output text)"
  instance
  echo "ok box: $BOX is $INSTANCE at $ADDRESS"
  ping=$(aws ssm describe-instance-information --filters "Key=InstanceIds,Values=$INSTANCE" \
    --query 'InstanceInformationList[0].PingStatus' --output text) || die "could not read the Session Manager state"
  [[ $ping == Online ]] || die "Session Manager shows $BOX as $ping, want Online"
  echo "ok session manager: Online"
  [[ $(on_box hostname) == "$BOX" ]] || die "the box did not answer with the hostname $BOX"
  echo "ok command: $BOX answers"
}

cmd_status() {
  session
  instance
  # shellcheck disable=SC2016
  on_box '
live=$(systemctl is-active cloudflared 2>/dev/null || true)
if [ "$live" = active ]; then
  echo "loyal-main is LIVE: production runs here."
else
  echo "loyal-main is STAGED: production still runs on Hetzner. Start nothing here. A unit started by hand is a second writer."
fi
{
  systemctl list-unit-files "loyal-*" caddy.service cloudflared.service prometheus.service prometheus-alertmanager.service otelcol-contrib.service --no-legend --no-pager
  for unit in postgresql@18-main.service wg-quick@loyal-ci.service; do echo "$unit $(systemctl is-enabled "$unit" 2>/dev/null || true)"; done
} |
  while read -r unit enabled _; do
    case $unit in *@.service | *@.timer) continue ;; esac
    printf "%-52s %-10s %s\n" "$unit" "$enabled" "$(systemctl is-active "$unit" 2>/dev/null || true)"
  done'
}

PUB=${LOYAL_SSH_PUBKEY:-$HOME/.ssh/id_ed25519.pub}

# Pins the box's host key and lets $PUB log in as ubuntu for the next 60 s.
admit() {
  local key
  [[ -f $PUB ]] || die "no public key at $PUB. Create one with: ssh-keygen -t ed25519   (or set LOYAL_SSH_PUBKEY)"
  session
  instance
  key=$(on_box 'cat /etc/ssh/ssh_host_ed25519_key.pub')
  [[ $key == ssh-ed25519\ * ]] || die "could not read the host key of $BOX through Session Manager"
  mkdir -p "$HOME/.ssh"
  printf '%s %s\n' "$ADDRESS" "$(cut -d' ' -f1,2 <<<"$key")" >"$KNOWN_HOSTS"
  aws ec2-instance-connect send-ssh-public-key --instance-id "$INSTANCE" --instance-os-user ubuntu \
    --ssh-public-key "file://$PUB" --query Success --output text >/dev/null || die "the box refused the public key $PUB"
}

cmd_ssh() {
  admit
  exec ssh -i "${PUB%.pub}" -o IdentitiesOnly=yes -o StrictHostKeyChecking=yes -o UserKnownHostsFile="$KNOWN_HOSTS" "ubuntu@$ADDRESS" "$@"
}

# A deploy makes ssh and scp calls for minutes, past the 60 s the key is valid, so they share one connection opened
# inside that window. Every other host keeps the user's own ssh config.
cmd_deploy() {
  local real_ssh real_scp status=0
  # dir stays global: the EXIT trap runs after this function has returned.
  admit
  dir=$(mktemp -d "${TMPDIR:-/tmp}/loyal-deploy-ssh.XXXXXX")
  trap 'ssh -F "$dir/config" -O exit "$BOX" 2>/dev/null || true; rm -rf "$dir"' EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM
  real_ssh=$(command -v ssh) real_scp=$(command -v scp)
  cat >"$dir/config" <<EOF
Host $BOX
  HostName $ADDRESS
  User ubuntu
  IdentityFile "${PUB%.pub}"
  IdentitiesOnly yes
  UserKnownHostsFile $KNOWN_HOSTS
  StrictHostKeyChecking yes
  ControlMaster auto
  ControlPath $dir/control
  ControlPersist yes
  ServerAliveInterval 15
  ServerAliveCountMax 3
Match all
  Include ~/.ssh/config
EOF
  mkdir "$dir/bin"
  printf '#!/bin/sh\nexec %q -F %q "$@"\n' "$real_ssh" "$dir/config" >"$dir/bin/ssh"
  printf '#!/bin/sh\nexec %q -F %q "$@"\n' "$real_scp" "$dir/config" >"$dir/bin/scp"
  chmod 755 "$dir/bin/ssh" "$dir/bin/scp"
  if "$dir/bin/ssh" -fN "$BOX"; then
    LOYAL_WORKERS_SSH=$BOX PATH="$dir/bin:$PATH" bash "$HERE/../../scripts/deploy-workers.sh" "$@" || status=$?
  else
    echo "team.sh: could not open the ssh connection to $BOX" >&2
    status=1
  fi
  return "$status"
}

cmd_run() {
  [[ $# == 1 ]] || usage
  session
  instance
  on_box "$1"
}

case ${1:-} in
  check | status) [[ $# == 1 ]] || usage; "cmd_$1" ;;
  ssh | run | deploy) c=$1; shift; "cmd_$c" "$@" ;;
  *) usage ;;
esac
