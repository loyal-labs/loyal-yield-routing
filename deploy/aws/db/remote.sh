#!/usr/bin/env bash
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
# PGMOVE_DEST: a second install beside a running one (/opt/pgmove-next while /opt/pgmove runs a backfill).
DEST=${PGMOVE_DEST:-/opt/pgmove}
[[ $DEST =~ ^/opt/pgmove[a-z0-9-]*$ ]] || { echo "remote: PGMOVE_DEST=$DEST is not /opt/pgmove or /opt/pgmove-<name>" >&2; exit 2; }

die() { echo "remote: $*" >&2; exit 1; }

conf_get() { # conf_get <cluster> <VAR>
  [[ -f $HERE/clusters/$1.conf ]] || { echo "remote: no cluster $1" >&2; exit 2; }
  # shellcheck disable=SC2016
  bash -c 'eval "$2="; source "$1"; echo "${!2}"' _ "$HERE/clusters/$1.conf" "$2"
}

conf_box() { conf_get "$1" BOX; }

ipv4_of() { # the literal IPv4 this machine's ssh config resolves <alias> to
  local ip
  ip=$(ssh -G "$1" | awk '$1 == "hostname" { print $2 }')
  [[ $ip =~ ^[0-9]+(\.[0-9]+){3}$ ]] || die "ssh -G $1 gives '$ip', not an IPv4 address"
  echo "$ip"
}

# route <cluster>: the forward-only user on FWD_HOP and the box's ssh alias FWD_SSH to it. The key
# is made on the box and never leaves it; the hop accepts it from the box's address only, for a
# -L to FWD_TARGET only, with no shell, pty, agent, X11, listener or unix socket.
route_on() {
  local c=$1 box hop user alias target box_ip hop_ip pub hostkey v listen='' rev_host rev_port
  box=$(conf_box "$c") hop=$(conf_get "$c" FWD_HOP) user=$(conf_get "$c" FWD_USER) alias=$(conf_get "$c" FWD_SSH) target=$(conf_get "$c" FWD_TARGET)
  rev_host=$(conf_get "$c" REV_HOST) rev_port=$(conf_get "$c" REV_PORT)
  # The reverse: pg-forward@ binds REV_HOST:REV_PORT on the hop (-R), where the source reaches the target.
  if [[ -n $rev_port ]]; then
    [[ ${rev_host:=127.0.0.1} =~ ^[0-9.]+$ && $rev_port =~ ^[0-9]+$ ]] || die "$c.conf: REV_HOST or REV_PORT has an unexpected shape"
    listen=$rev_host:$rev_port
  fi
  for v in hop user alias target; do [[ -n ${!v} ]] || die "$c.conf: route needs FWD_HOP, FWD_USER, FWD_SSH and FWD_TARGET"; done
  [[ $user =~ ^[a-z][a-z0-9-]*$ && $alias =~ ^[a-z][a-z0-9-]*$ && $target =~ ^[0-9.]+:[0-9]+$ ]] || die "$c.conf: FWD_USER, FWD_SSH or FWD_TARGET has an unexpected shape"
  box_ip=$(ipv4_of "$box") hop_ip=$(ipv4_of "$hop")
  pub=$(ssh -o BatchMode=yes "$box" "sudo -n bash -c 'f=/etc/pgmove/$c/forward.key; test -s \$f || { install -d -m 0700 /etc/pgmove/$c && ssh-keygen -q -t ed25519 -N \"\" -C pgmove-$c-forward -f \$f && echo \"changed: \$f made on $box\" >&2; }; cat \$f.pub'")
  [[ $pub =~ ^ssh-ed25519\ [A-Za-z0-9+/]+=*\ pgmove-$c-forward$ ]] || die "the box's forward key is not one ed25519 public key"
  hostkey=$(ssh -o BatchMode=yes "$hop" 'cat /etc/ssh/ssh_host_ed25519_key.pub' | awk '{ print $1 " " $2 }')
  [[ $hostkey =~ ^ssh-ed25519\ [A-Za-z0-9+/]+=*$ ]] || die "$hop: no ed25519 host key"
  { printf 'user=%q box_ip=%q target=%q pub=%q listen=%q\n' "$user" "$box_ip" "$target" "$pub" "$listen"; cat <<'EOF'
set -euo pipefail
ts=$(date -u +%Y%m%dT%H%M%SZ) n=0
changed() { n=$((n + 1)); echo "changed: $(hostname -s): $*"; }
# Backups stay out of sshd_config.d and authorized_keys.d, so nothing stray sits where sshd reads.
bak=/root/pgmove-backup/route-$ts
if ! id -u "$user" >/dev/null 2>&1; then
  useradd --system --user-group --shell /usr/sbin/nologin --home-dir /nonexistent --no-create-home "$user"
  changed "user $user: system, shell /usr/sbin/nologin, no home"
fi
[[ $(passwd -S "$user" | awk '{ print $2 }') == L ]] || { usermod -L "$user"; changed "$user password locked"; }
keys=/etc/ssh/authorized_keys.d/$user
want="from=\"$box_ip\",restrict,port-forwarding,permitopen=\"$target\"${listen:+,permitlisten=\"$listen\"},command=\"/usr/sbin/nologin\" $pub"
if [[ $(cat "$keys" 2>/dev/null || true) != "$want" ]]; then
  install -d -m 0755 -o root -g root /etc/ssh/authorized_keys.d
  [[ ! -e $keys ]] || { install -d -m 0700 "$bak"; cp -a "$keys" "$bak/"; }
  printf '%s\n' "$want" >"$keys.new" && chown root:root "$keys.new" && chmod 0644 "$keys.new" && mv -f "$keys.new" "$keys"
  changed "$keys: one key, from the box only, restrict, -L to $target only, command nologin (root-owned)"
fi
# One name per user: 60-pgmove-fwd.conf for pgmove-fwd. Earlier runs wrote 60-pgmove-$user.conf.
conf=/etc/ssh/sshd_config.d/60-pgmove-${user#pgmove-}.conf
old=/etc/ssh/sshd_config.d/60-pgmove-$user.conf
[[ $old != "$conf" && -e $old ]] || old=
wantc="# pgmove forward-only user (loyal-yield-routing deploy/aws/db/remote.sh route)
Match User $user
  AuthorizedKeysFile /etc/ssh/authorized_keys.d/%u
  AuthenticationMethods publickey
  PasswordAuthentication no
  KbdInteractiveAuthentication no
  AllowTcpForwarding ${listen:+yes}${listen:-local}
  AllowStreamLocalForwarding no
  PermitOpen $target
  PermitListen ${listen:-none}
  GatewayPorts ${listen:+clientspecified}${listen:-no}
  PermitTunnel no
  X11Forwarding no
  AllowAgentForwarding no
  PermitTTY no
  ForceCommand /usr/sbin/nologin"
if [[ -n $old || $(cat "$conf" 2>/dev/null || true) != "$wantc" ]]; then
  probe="host=pgmove-check,addr=192.0.2.1"
  before=$(sshd -T -C "user=root,$probe")
  install -d -m 0700 "$bak"
  for f in "$conf" ${old:+"$old"}; do [[ ! -e $f ]] || cp -a "$f" "$bak/"; done
  revert() {
    for f in "$conf" ${old:+"$old"}; do if [[ -e $bak/${f##*/} ]]; then cp -a "$bak/${f##*/}" "$f"; else rm -f "$f"; fi; done
    echo "route: $1; sshd_config.d restored from $bak, sshd not reloaded" >&2; exit 1
  }
  printf '%s\n' "$wantc" >"$conf"
  chmod 0644 "$conf"
  [[ -z $old ]] || rm -f "$old"
  sshd -t || revert "sshd -t failed"
  [[ $(sshd -T -C "user=root,$probe") == "$before" ]] || revert "root's effective sshd settings would change"
  systemctl reload ssh
  changed "$conf: Match User $user, publickey only, -L to $target only, ${listen:+-R on $listen only, }no shell, pty, agent, X11${listen:-, listener} or unix socket${old:+; replaced $old}; sshd -t ok, root's settings unchanged, ssh reloaded"
fi
echo "route: $(hostname -s) effective for $user from the box:"
sshd -T -C "user=$user,host=pgmove-check,addr=$box_ip" | grep -E '^(authorizedkeysfile|authenticationmethods|passwordauthentication|allowtcpforwarding|allowstreamlocalforwarding|permitopen|permitlisten|gatewayports|permittty|permittunnel|x11forwarding|allowagentforwarding|forcecommand) ' | sed 's/^/  /'
echo "route: $(hostname -s) $n change(s)"
EOF
  } | ssh -o BatchMode=yes "$hop" bash -s
  { printf 'c=%q alias=%q hop_ip=%q user=%q hostkey=%q\n' "$c" "$alias" "$hop_ip" "$user" "$hostkey"; cat <<'EOF'
set -euo pipefail
n=0
changed() { n=$((n + 1)); echo "changed: $(hostname -s): $*"; }
put() { [[ $(cat "$1" 2>/dev/null || true) == "$2" ]] && return 0; printf '%s\n' "$2" >"$1.new" && chmod "$3" "$1.new" && mv -f "$1.new" "$1"; changed "$1: $4"; }
put "/etc/pgmove/$c/known_hosts" "$hop_ip $hostkey" 0600 "the hop's ed25519 host key, read over the operator's own root session"
put "/etc/ssh/ssh_config.d/pgmove-$c.conf" "Host $alias
  HostName $hop_ip
  User $user
  IdentityFile /etc/pgmove/$c/forward.key
  IdentitiesOnly yes
  UserKnownHostsFile /etc/pgmove/$c/known_hosts
  StrictHostKeyChecking yes
  ExitOnForwardFailure yes" 0644 "ssh alias $alias for pg-forward@$c"
echo "route: $(hostname -s) $n change(s)"
EOF
  } | ssh -o BatchMode=yes "$box" sudo -n bash -s
}

# source <cluster>: what needs a superuser on the source, from an operator machine. PGMOVE_SRC_ADMIN
# names a command that runs its one argument as root on the source; the script comes from
# pgmove.sh here, and the box only answers whether the source's verifier for pgmove is current.
source_on() {
  local c=$1 admin cur res
  admin=$(admin_cmd)
  # The conf's OS user and socket: on robinx-prod the default socket is production's, not the test cluster's.
  cur=$("$admin" "cd / && sudo -n -u $(printf %q "$(conf_get "$c" SRC_OSUSER)") psql -h $(printf %q "$(conf_get "$c" SRC_SOCKET)") -p $(printf %q "$(conf_get "$c" SRC_PORT)") -d postgres -XAtqc \"select coalesce(rolpassword, '') from pg_authid where rolname = 'pgmove'\"")
  res=$(printf '%s\n' "$cur" | box_pgmove verifier "$c")
  bash "$HERE/pgmove.sh" source "$c" "$res" | "$admin" "bash -s"
}

# The target holds no key for the source's root: the source side of each run rides an agent that
# holds only that key (ssh-agent -a <socket>, then ssh-add the key), named in PGMOVE_AGENT and
# forwarded for the length of the run.
# SRC_VIA=forward needs no agent: the box reads the source through its forward as pgmove.
box_pgmove() { local box; box=$(conf_box "$2"); ssh -o BatchMode=yes "$box" "sudo -n $DEST/pgmove.sh $(printf '%q ' "$@")"; }

admin_cmd() {
  [[ -n ${PGMOVE_SRC_ADMIN:-} ]] || die "set PGMOVE_SRC_ADMIN to a command that runs its one argument as root on the source"
  echo "$PGMOVE_SRC_ADMIN"
}

# src_run <cluster> <need lines>: the root script srcexec builds here from the box's need lines,
# run on the source through PGMOVE_SRC_ADMIN. Catchup rows go box to source over a pipe.
# forward_loop calls it in an OR-list, where errexit is off: each step returns 1 itself.
src_run() {
  local c=$1 needs=$2 admin script t f moved rc=0
  admin=$(admin_cmd) || return 1
  while read -r _ _ t f; do
    script=$(printf 'need: catchin %s %s\n' "$t" "$f" | bash "$HERE/pgmove.sh" srcexec "$c") || { echo "remote: srcexec catchin $t $f failed" >&2; return 1; }
    moved=$(box_pgmove catchout "$c" "$t" "$f" | "$admin" "bash -c $(printf %q "$script")") || { echo "remote: catchup $t above $f failed" >&2; return 1; }
    [[ $moved =~ ^[0-9]+$ ]] || { echo "remote: catchup $t above $f answered '$moved', not a row count" >&2; return 1; }
    echo "remote: catchup $t above $f, target to source: +$moved rows"
  done < <(awk '/^need: catchup /' <<<"$needs")
  if grep -q '^need: revsub ' <<<"$needs"; then
    PGMOVE_REV_PW=$(ssh -o BatchMode=yes "$(conf_box "$c")" "sudo -n cat /etc/pgmove/$c/repl.password") || die "cannot read the replication password on the box"
    export PGMOVE_REV_PW
  fi
  printf '%s\n' "$needs" | bash "$HERE/pgmove.sh" srcexec "$c" | "$admin" "bash -s" || rc=1
  unset PGMOVE_REV_PW
  return "$rc"
}

# The box step prints need: lines and exits 3 when root on the source must act; this runs them and
# reruns the step, which is idempotent, until it exits 0. The same needs twice is a stop.
forward_loop() {
  local c=$2 out rc prev='' needs round dry=0 a
  for a in "$@"; do [[ $a != --dry-run ]] || dry=1; done
  if [[ $1 == switch && " $* " != *" --reverse "* ]] && ((dry == 0)); then
    passwords_on "$c" || return
  fi
  out=$(mktemp)
  # Four rounds of source steps, each followed by the box step again: the last run is never one
  # whose needs ran unchecked.
  for round in 1 2 3 4 5; do
    set +e
    box_pgmove "$@" | tee "$out"
    rc=${PIPESTATUS[0]}
    set -e
    ((rc == 3)) || break
    ((round < 5)) || { rm -f "$out"; die "$1 still asks for source steps after 4 rounds; stopping"; }
    needs=$(awk '/^need: /' "$out")
    [[ -n $needs ]] || die "$1 exited 3 with no need: lines"
    [[ $needs != "$prev" ]] || die "$1 asked for the same source steps after they ran; stopping"
    prev=$needs
    echo "remote: round $round: $(grep -c . <<<"$needs") source step(s) through PGMOVE_SRC_ADMIN"
    src_run "$c" "$needs" || { rm -f "$out"; die "the source steps failed; $1 not rerun"; }
  done
  rm -f "$out"
  if ((rc == 0 && dry == 0)) && [[ $1 == retire ]]; then src_run "$c" "need: rmmarker"; fi
  if [[ $1 == preflight ]]; then preflight_admin "$c" || { ((rc)) || rc=1; }; fi
  return "$rc"
}

# passwords <cluster> [--dry-run]: the source's verifiers, read as root there, piped to the box.
passwords_on() {
  local c=$1 admin
  shift
  admin=$(admin_cmd)
  bash "$HERE/pgmove.sh" srcexec "$c" verifiers </dev/null | "$admin" "bash -s" | box_pgmove passwords "$c" "$@"
}

# The checks that need root on the source: the freeze marker is there, the verifiers are equal.
preflight_admin() {
  local c=$1 admin out bad=0
  admin=$(admin_cmd)
  bash "$HERE/pgmove.sh" srcexec "$c" marker </dev/null | "$admin" "bash -s" || { echo "DIFF marker: absent on the source"; bad=1; }
  out=$(passwords_on "$c" --dry-run) || bad=1
  sed 's/^/  passwords: /' <<<"$out"
  grep -q '^passwords: 0 change(s) in the dry run' <<<"$out" || { echo "DIFF passwords: the target's verifiers differ from the source's; run remote.sh run passwords $c"; bad=1; }
  echo "preflight: source-side checks: $bad DIFF"
  return "$bad"
}

pgmove_on() {
  local box
  box=$(conf_box "$2")
  if [[ $(conf_get "$2" SRC_VIA) == forward ]]; then
    case $1 in
      passwords) shift; passwords_on "$@" ;;
      verifier | catchout | srcexec) box_pgmove "$@" ;;
      *) forward_loop "$@" ;;
    esac
    return
  fi
  [[ -S ${PGMOVE_AGENT:-} ]] || { echo "remote: PGMOVE_AGENT must name the socket of an agent holding the source key" >&2; exit 2; }
  ssh -o ForwardAgent="$PGMOVE_AGENT" -o BatchMode=yes "$box" "sudo -n --preserve-env=SSH_AUTH_SOCK $DEST/pgmove.sh $(printf '%q ' "$@")"
}

install_on() {
  local box
  box=$(conf_box "$1")
  COPYFILE_DISABLE=1 tar --no-xattrs -C "$HERE" -cf - pgmove.sh parsers-copy.sh pg-forward@.service clusters |
    ssh -o BatchMode=yes "$box" "sudo -n install -d -m 0755 $DEST && sudo -n tar -C $DEST --no-same-owner -xf - && sudo -n chown -R root:root $DEST"
  echo "remote: $DEST on $box holds pgmove.sh at $(git -C "$HERE" rev-parse --short HEAD 2>/dev/null || echo '?')"
}

[[ ${BASH_SOURCE[0]} == "$0" ]] || return 0

case ${1:-} in
  install)
    [[ -n ${2:-} ]] || { echo "usage: $0 install <cluster>" >&2; exit 2; }
    install_on "$2"
    ;;
  run)
    [[ -n ${2:-} && -n ${3:-} ]] || { echo "usage: $0 run <pgmove subcommand> <cluster> [flags]" >&2; exit 2; }
    shift
    pgmove_on "$@"
    ;;
  route)
    [[ -n ${2:-} ]] || { echo "usage: $0 route <cluster>" >&2; exit 2; }
    route_on "$2"
    ;;
  source)
    [[ -n ${2:-} ]] || { echo "usage: PGMOVE_SRC_ADMIN=<cmd> $0 source <cluster>" >&2; exit 2; }
    source_on "$2"
    ;;
  *)
    echo "usage: $0 install <cluster> | run <pgmove subcommand> <cluster> [flags]  (on the cluster's BOX)" >&2
    echo "       $0 route <cluster> | PGMOVE_SRC_ADMIN=<cmd> $0 source <cluster>  (SRC_VIA=forward)" >&2
    exit 2
    ;;
esac
