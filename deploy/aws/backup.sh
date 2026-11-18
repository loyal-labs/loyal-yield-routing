#!/usr/bin/env bash
# Ensures the pgBackRest backup storage and access for one box from boxes/<box>.conf, then its stanzas on the box, and converges on rerun.
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
export AWS_PAGER='' AWS_EC2_METADATA_DISABLED=true

usage() {
  cat >&2 <<EOF
usage: $0 <box> [--dry-run] [--full]
boxes: $(for f in "$HERE"/boxes/*.conf; do if grep -q '^BACKUP_BUCKET=.' "$f"; then basename "$f" .conf; fi; done | paste -sd' ' -)
Ensures, each step found again by name: the S3 bucket BACKUP_BUCKET from the conf in the box's account and region (all public
access blocked, SSE-S3 default encryption, versioning on, bucket-owner-enforced ownership, a policy denying non-TLS requests,
a lifecycle rule expiring noncurrent versions after 30 days and aborting incomplete multipart uploads after 7), the IAM role
<box>-pgbackrest with one inline policy (list on the bucket, get, put and delete on its objects), the instance profile of the
same name holding that role, and the profile attached to the instance (found by its Name tag).
Never deletes a bucket, object, role or policy and sets no object lock or retention. Prints "changed:" lines and ends
"<box>: <n> change(s)". --dry-run prints each aws command it would run. Reads need a live session; a dry run without one lists
every step as missing and says so.
Then, over ssh to BOX_SSH, for a conf that lists STANZAS: the pgBackRest config (S3 repo from the instance role, no key on disk),
a cipher passphrase generated on the box once and kept root:postgres 0640 in /etc/pgbackrest/conf.d/cipher.conf, the stanza,
archive_command of the cluster (reload only; a box whose archive_mode is off is reported and left alone), pgbackrest check, the
backup unit files left disabled, and with --full the first full as a transient niced, CPU- and read-capped unit that saves
subscription lag before, during and after. Never restarts Postgres, enables or starts a timer, or prints the passphrase.
EOF
  exit 2
}

BOX=${1:-}
[[ -n $BOX ]] || usage
shift
DRY=0 FULL=0
for a in "$@"; do
  case $a in
    --dry-run) DRY=1 ;;
    --full) FULL=1 ;;
    *) usage ;;
  esac
done
[[ $BOX =~ ^[a-z0-9-]+$ && -f $HERE/boxes/$BOX.conf ]] || { echo "backup.sh: no box $BOX ($HERE/boxes/$BOX.conf)" >&2; usage; }

die() { echo "backup.sh: $*" >&2; exit 1; }
command -v aws >/dev/null || die "aws CLI not found"
command -v jq >/dev/null || die "jq not found"

PROFILE='' ACCOUNT='' REGION='' BACKUP_BUCKET='' BOX_SSH='' PG_MAJOR=18
STANZAS=() UNITS=() VERIFY_UNITS=()
RETENTION_FULL=2 RETENTION_FULL_TYPE=count RETENTION_DIFF=7 BACKUP_PROCESS_MAX=2 BACKUP_READ_MBPS=60
# shellcheck source=/dev/null
source "$HERE/boxes/$BOX.conf"
PROFILE=${PROFILE:-${AWS_PROFILE:-}}
REGION=${REGION:-${AWS_REGION:-}}
[[ -n $PROFILE ]] || die "no AWS profile: set PROFILE in boxes/$BOX.conf or AWS_PROFILE"
[[ -n $REGION ]] || die "no region: set REGION in boxes/$BOX.conf or AWS_REGION"
[[ -n $ACCOUNT ]] || die "ACCOUNT missing in boxes/$BOX.conf"
[[ -n $BACKUP_BUCKET ]] || die "BACKUP_BUCKET missing in boxes/$BOX.conf"
[[ $BACKUP_BUCKET =~ ^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$ ]] || die "BACKUP_BUCKET '$BACKUP_BUCKET' is not a valid bucket name"
[[ $BACKUP_BUCKET == *"$ACCOUNT"* ]] || die "BACKUP_BUCKET '$BACKUP_BUCKET' must carry the account id $ACCOUNT"

B=$BACKUP_BUCKET
ROLE=$BOX-pgbackrest
IPROFILE=$BOX-pgbackrest
POLICY=pgbackrest-s3

A() { aws --profile "$PROFILE" --region "$REGION" "$@"; }

changes=0 OFFLINE=0 FRESH=0 OUT=''
change() { changes=$((changes + 1)); if ((DRY)); then echo "would change: $*"; else echo "changed: $*"; fi; }
warn() { echo "warning: $*"; }

get() {
  local re=$1 rc=0
  shift
  OUT=''
  if ((OFFLINE)); then return 0; fi
  OUT=$(A "$@" 2>&1) || rc=$?
  if ((rc)); then
    if grep -q -E "$re" <<<"$OUT"; then OUT=''; return 0; fi
    die "aws $* failed: $(grep -m1 . <<<"$OUT")"
  fi
}

sub() {
  local re=$1
  shift
  if ((DRY && FRESH)); then OUT=''; else get "$re" "$@"; fi
}

mut() {
  if ((DRY)); then
    printf '  aws --profile %q --region %q' "$PROFILE" "$REGION"
    printf ' %q' "$@"
    echo
    OUT=''
    return 0
  fi
  OUT=$(A "$@")
}

pick() { jq -r "$1" <<<"${OUT:-null}" 2>/dev/null | grep -v '^null$' || true; }
canon() { jq -S -c . 2>/dev/null <<<"${1:-null}" || echo null; }

if ! ident=$(A sts get-caller-identity --output json 2>&1); then
  if ((DRY)); then
    OFFLINE=1
    echo "dry run offline: sts get-caller-identity failed for profile $PROFILE ($(grep -m1 . <<<"$ident")); no state is read, every step is listed as missing"
  else
    die "no live session for profile $PROFILE: sts get-caller-identity failed: $(grep -m1 . <<<"$ident")"
  fi
else
  acct=$(jq -r .Account <<<"$ident")
  [[ $ACCOUNT == "$acct" ]] || die "profile $PROFILE is account $acct, boxes/$BOX.conf wants $ACCOUNT"
  echo "$BOX: account $acct, region $REGION, bucket $B"
fi

# bucket
head_rc=0
if ((OFFLINE)); then
  head_rc=1
  head_out='404 Not Found'
else
  head_out=$(A s3api head-bucket --bucket "$B" --expected-bucket-owner "$ACCOUNT" 2>&1) || head_rc=$?
fi
if ((head_rc)); then
  grep -q -E '\(404\)|NoSuchBucket|Not Found' <<<"$head_out" || die "bucket $B is not reachable in account $ACCOUNT: $(grep -m1 . <<<"$head_out")"
  create=(s3api create-bucket --bucket "$B" --object-ownership BucketOwnerEnforced --output json)
  if [[ $REGION != us-east-1 ]]; then create+=(--create-bucket-configuration "LocationConstraint=$REGION"); fi
  change "bucket $B created (ownership BucketOwnerEnforced)"
  mut "${create[@]}"
  FRESH=1
fi

sub 'NoSuchPublicAccessBlockConfiguration' s3api get-public-access-block --bucket "$B" --output json
if [[ $(pick '.PublicAccessBlockConfiguration | [.BlockPublicAcls, .IgnorePublicAcls, .BlockPublicPolicy, .RestrictPublicBuckets] | all') != true ]]; then
  change "bucket $B blocks all public access"
  mut s3api put-public-access-block --bucket "$B" \
    --public-access-block-configuration BlockPublicAcls=true,IgnorePublicAcls=true,BlockPublicPolicy=true,RestrictPublicBuckets=true
fi

sub 'ServerSideEncryptionConfigurationNotFoundError' s3api get-bucket-encryption --bucket "$B" --output json
if [[ $(pick '.ServerSideEncryptionConfiguration.Rules | map(.ApplyServerSideEncryptionByDefault.SSEAlgorithm) | index("AES256") != null') != true ]]; then
  change "bucket $B default encryption SSE-S3"
  mut s3api put-bucket-encryption --bucket "$B" \
    --server-side-encryption-configuration '{"Rules":[{"ApplyServerSideEncryptionByDefault":{"SSEAlgorithm":"AES256"},"BucketKeyEnabled":true}]}'
fi

sub 'NoSuchBucket' s3api get-bucket-versioning --bucket "$B" --output json
if [[ $(pick .Status) != Enabled ]]; then
  change "bucket $B versioning on"
  mut s3api put-bucket-versioning --bucket "$B" --versioning-configuration Status=Enabled
fi

sub 'OwnershipControlsNotFoundError' s3api get-bucket-ownership-controls --bucket "$B" --output json
if [[ $(pick '.OwnershipControls.Rules[0].ObjectOwnership') != BucketOwnerEnforced ]]; then
  change "bucket $B ownership BucketOwnerEnforced"
  mut s3api put-bucket-ownership-controls --bucket "$B" --ownership-controls 'Rules=[{ObjectOwnership=BucketOwnerEnforced}]'
fi

# Statements and rules with other ids are kept; only ours are set.
TLS_STMT=$(jq -n -c --arg b "$B" '{Sid:"DenyInsecureTransport",Effect:"Deny",Principal:"*",Action:"s3:*",Resource:["arn:aws:s3:::\($b)","arn:aws:s3:::\($b)/*"],Condition:{Bool:{"aws:SecureTransport":"false"}}}')
sub 'NoSuchBucketPolicy' s3api get-bucket-policy --bucket "$B" --output json
have_policy=$(pick .Policy)
[[ -n $have_policy ]] || have_policy='{"Version":"2012-10-17","Statement":[]}'
want_policy=$(jq -S -c --argjson s "$TLS_STMT" '{Version:"2012-10-17",Statement:([.Statement[]? | select(.Sid != "DenyInsecureTransport")] + [$s])}' <<<"$have_policy")
mine=$(jq -S -c '.Statement[]? | select(.Sid == "DenyInsecureTransport")' <<<"$have_policy")
if [[ $mine != "$(canon "$TLS_STMT")" ]]; then
  change "bucket $B policy denies non-TLS requests"
  mut s3api put-bucket-policy --bucket "$B" --policy "$want_policy"
fi

RULE=$(jq -n -c '{ID:"pgbackrest-housekeeping",Status:"Enabled",Filter:{Prefix:""},NoncurrentVersionExpiration:{NoncurrentDays:30},AbortIncompleteMultipartUpload:{DaysAfterInitiation:7}}')
sub 'NoSuchLifecycleConfiguration' s3api get-bucket-lifecycle-configuration --bucket "$B" --output json
have_life=${OUT:-null}
lifeview='.Rules[]? | select(.ID == "pgbackrest-housekeeping") | {Status, NoncurrentDays: .NoncurrentVersionExpiration.NoncurrentDays, Abort: .AbortIncompleteMultipartUpload.DaysAfterInitiation, Prefix: (.Filter.Prefix // .Prefix // ""), Other: (del(.ID, .Status, .Filter, .Prefix, .NoncurrentVersionExpiration, .AbortIncompleteMultipartUpload) | keys)}'
have_rule=$(jq -S -c "$lifeview" <<<"$have_life" 2>/dev/null || true)
want_rule=$(jq -S -c "$lifeview" <<<"{\"Rules\":[$RULE]}")
if [[ $have_rule != "$want_rule" ]]; then
  change "bucket $B lifecycle: noncurrent versions expire after 30 days, incomplete multipart uploads abort after 7"
  lifecycle=$(jq -c --argjson r "$RULE" '{Rules: ([.Rules[]? | select(.ID != "pgbackrest-housekeeping")] + [$r])}' <<<"$have_life")
  mut s3api put-bucket-lifecycle-configuration --bucket "$B" --lifecycle-configuration "$lifecycle"
fi

# role, inline policy, instance profile
TRUST=$(jq -n -c '{Version:"2012-10-17",Statement:[{Effect:"Allow",Principal:{Service:"ec2.amazonaws.com"},Action:"sts:AssumeRole"}]}')
ROLE_POLICY=$(jq -n -c --arg b "$B" '{Version:"2012-10-17",Statement:[{Sid:"ListBucket",Effect:"Allow",Action:"s3:ListBucket",Resource:"arn:aws:s3:::\($b)"},{Sid:"Objects",Effect:"Allow",Action:["s3:GetObject","s3:PutObject","s3:DeleteObject"],Resource:"arn:aws:s3:::\($b)/*"}]}')

get 'NoSuchEntity' iam get-role --role-name "$ROLE" --output json
if [[ -z $OUT ]]; then
  change "role $ROLE created (trusted by ec2.amazonaws.com)"
  mut iam create-role --role-name "$ROLE" --assume-role-policy-document "$TRUST" \
    --description "$BOX pgBackRest access to s3://$B only" --output json
  ROLE_FRESH=1
else
  ROLE_FRESH=0
  if [[ $(canon "$(pick .Role.AssumeRolePolicyDocument)") != "$(canon "$TRUST")" ]]; then
    change "role $ROLE trust policy set to ec2.amazonaws.com"
    mut iam update-assume-role-policy --role-name "$ROLE" --policy-document "$TRUST"
  fi
fi

if ((ROLE_FRESH)); then OUT=''; else get 'NoSuchEntity' iam get-role-policy --role-name "$ROLE" --policy-name "$POLICY" --output json; fi
if [[ $(canon "$(pick .PolicyDocument)") != "$(canon "$ROLE_POLICY")" ]]; then
  change "role $ROLE inline policy $POLICY: list on $B, get/put/delete on its objects"
  mut iam put-role-policy --role-name "$ROLE" --policy-name "$POLICY" --policy-document "$ROLE_POLICY"
fi
if ((!ROLE_FRESH)); then
  get 'NoSuchEntity' iam list-role-policies --role-name "$ROLE" --output json
  others=$(pick "[.PolicyNames[] | select(. != \"$POLICY\")] | length")
  if ((${others:-0} > 0)); then warn "role $ROLE has $others other inline policy(ies); left as is, remove by hand"; fi
  get 'NoSuchEntity' iam list-attached-role-policies --role-name "$ROLE" --output json
  attached=$(pick '.AttachedPolicies | length')
  if ((${attached:-0} > 0)); then warn "role $ROLE has $attached managed policy(ies) attached; left as is, remove by hand"; fi
fi

get 'NoSuchEntity' iam get-instance-profile --instance-profile-name "$IPROFILE" --output json
if [[ -z $OUT ]]; then
  change "instance profile $IPROFILE created"
  mut iam create-instance-profile --instance-profile-name "$IPROFILE" --output json
  PROFILE_ROLES=''
else
  PROFILE_ROLES=$(pick '.InstanceProfile.Roles[].RoleName')
fi
if [[ -z $PROFILE_ROLES ]]; then
  change "instance profile $IPROFILE holds role $ROLE"
  mut iam add-role-to-instance-profile --instance-profile-name "$IPROFILE" --role-name "$ROLE"
elif [[ $PROFILE_ROLES != "$ROLE" ]]; then
  warn "instance profile $IPROFILE holds $PROFILE_ROLES, not $ROLE; left as is"
fi

# attach to the instance
get '^$' ec2 describe-instances --filters "Name=tag:Name,Values=$BOX" Name=instance-state-name,Values=pending,running,stopping,stopped --output json
n=$(pick '[.Reservations[].Instances[]] | length')
((${n:-0} <= 1)) || die "$n live instances are tagged Name=$BOX; leave one"
IID=$(pick '.Reservations[0].Instances[0].InstanceId')
if [[ -z $IID ]]; then
  if ((OFFLINE)); then IID=instance-id; else warn "no live instance tagged Name=$BOX; profile $IPROFILE not attached"; fi
fi
if [[ -n $IID ]]; then
  get '^$' ec2 describe-iam-instance-profile-associations --filters "Name=instance-id,Values=$IID" Name=state,Values=associated,associating --output json
  have_arn=$(pick '.IamInstanceProfileAssociations[0].IamInstanceProfile.Arn')
  if [[ -z $have_arn ]]; then
    change "instance profile $IPROFILE associated with $IID"
    if ((DRY)); then
      mut ec2 associate-iam-instance-profile --instance-id "$IID" --iam-instance-profile "Name=$IPROFILE" --output json
    else
      try=1
      while ! OUT=$(A ec2 associate-iam-instance-profile --instance-id "$IID" --iam-instance-profile "Name=$IPROFILE" --output json 2>&1); do
        if ((try < 10)) && grep -q 'Invalid IAM Instance Profile' <<<"$OUT"; then
          try=$((try + 1))
          sleep 6
        else
          die "associate-iam-instance-profile failed: $(grep -m1 . <<<"$OUT")"
        fi
      done
    fi
  elif [[ $have_arn != */"$IPROFILE" ]]; then
    warn "instance $IID has profile $have_arn, not $IPROFILE; left as is, replace by hand"
  fi
fi

# stanzas on the box
if ((${#STANZAS[@]} > 0)); then
  [[ -n $BOX_SSH ]] || BOX_SSH=$BOX
  blobs=''
  for u in ${UNITS[@]+"${UNITS[@]}"}; do
    [[ -f $HERE/boxes/$BOX.units/$u ]] || die "unit file $HERE/boxes/$BOX.units/$u missing"
    blobs+="$u:$(base64 <"$HERE/boxes/$BOX.units/$u" | tr -d '\n')"$'\n'
  done
  stanza_lines=$(printf '%s\n' "${STANZAS[@]}")
  verify_lines=$(printf '%s\n' ${VERIFY_UNITS[@]+"${VERIFY_UNITS[@]}"})
  unit_lines=$(printf '%s\n' ${UNITS[@]+"${UNITS[@]}"})
  remote_vars=$(
    printf '%s=%q\n' DRY "$DRY" FULL "$FULL" BUCKET "$B" REGION "$REGION" PG_MAJOR "$PG_MAJOR" \
      RET_FULL "$RETENTION_FULL" RET_TYPE "$RETENTION_FULL_TYPE" RET_DIFF "$RETENTION_DIFF" \
      PROCESS_MAX "$BACKUP_PROCESS_MAX" READ_MBPS "$BACKUP_READ_MBPS" \
      STANZA_LINES "$stanza_lines" UNIT_BLOBS "$blobs" UNIT_LINES "$unit_lines" VERIFY_LINES "$verify_lines"
  )
  read -r -d '' remote_body <<'REMOTE' || true
set -euo pipefail
set +x
changes=0 bad=0
change() { changes=$((changes + 1)); if ((DRY)); then echo "would change: $*"; else echo "changed: $*"; fi; }
warn() { echo "warning: $*"; }
as_pg() { runuser -u postgres -- "$@" </dev/null; }
pgq() { as_pg psql -p "$1" -d postgres -Atqc "$2"; }

put_text() { # put_text <path> <mode> <owner:group> <text> <what>
  local path=$1 mode=$2 owner=$3 text=$4 what=$5 tmp
  if [[ -f $path && $(cat "$path") == "$text" && $(stat -c '%a %U:%G' "$path") == "${mode#0} $owner" ]]; then return 0; fi
  change "$what"
  if ((DRY)); then return 0; fi
  tmp=$(mktemp "$path.XXXXXX")
  chmod "$mode" "$tmp"
  chown "$owner" "$tmp"
  printf '%s\n' "$text" >"$tmp"
  mv -f "$tmp" "$path"
}
ensure_dir() { # ensure_dir <path> <mode> <owner:group>
  if [[ -d $1 && $(stat -c '%a %U:%G' "$1") == "${2#0} $3" ]]; then return 0; fi
  change "directory $1 $2 $3"
  if ((DRY)); then return 0; fi
  install -d -m "$2" -o "${3%%:*}" -g "${3##*:}" "$1"
}

ensure_dir /etc/pgbackrest 0750 root:postgres
ensure_dir /etc/pgbackrest/conf.d 0750 root:postgres
for d in /var/lib/pgbackrest /var/log/pgbackrest /var/spool/pgbackrest; do ensure_dir "$d" 0750 postgres:postgres; done

conf="[global]
repo1-type=s3
repo1-s3-bucket=$BUCKET
repo1-s3-endpoint=s3.$REGION.amazonaws.com
repo1-s3-region=$REGION
repo1-s3-key-type=auto
repo1-cipher-type=aes-256-cbc
repo1-retention-full-type=$RET_TYPE
repo1-retention-full=$RET_FULL
repo1-retention-diff=$RET_DIFF
process-max=$PROCESS_MAX
compress-type=zst
compress-level=3
start-fast=y
resume=n
archive-timeout=120
archive-async=y
spool-path=/var/spool/pgbackrest
log-path=/var/log/pgbackrest
log-level-console=warn
log-level-file=info

[global:archive-push]
process-max=2"
while IFS='|' read -r st rpath cluster port pgdata sock; do
  [[ -n $st ]] || continue
  conf+="

[$st]
repo1-path=$rpath
pg1-path=$pgdata
pg1-port=$port
pg1-socket-path=$sock"
done <<<"$STANZA_LINES"
put_text /etc/pgbackrest/pgbackrest.conf 0640 root:postgres "$conf" "/etc/pgbackrest/pgbackrest.conf: S3 repo $BUCKET, instance-role credentials, retention full=$RET_FULL ($RET_TYPE) diff=$RET_DIFF"

CIPHER=/etc/pgbackrest/conf.d/cipher.conf
if ! grep -q '^repo1-cipher-pass=.' "$CIPHER" 2>/dev/null; then
  change "$CIPHER: new cipher passphrase generated here, root:postgres 0640, never printed"
  if ((!DRY)); then
    umask 077
    tmp=$(mktemp /etc/pgbackrest/conf.d/.cipher.XXXXXX)
    { printf '[global]\nrepo1-cipher-pass='; openssl rand -base64 48 | tr -d '\n'; printf '\n'; } >"$tmp"
    chown root:postgres "$tmp"
    chmod 0640 "$tmp"
    mv -f "$tmp" "$CIPHER"
  fi
fi
if [[ -f $CIPHER && $(stat -c '%a %U:%G' "$CIPHER") != "640 root:postgres" ]]; then warn "$CIPHER is not root:postgres 0640"; bad=1; fi

LAGGED=/usr/local/sbin/pgbackrest-lagged-full
read -r -d '' lagged <<'WRAP' || true
#!/bin/sh
# usage: pgbackrest-lagged-full <stanza> <port> [full|diff]. One backup, with subscription lag saved before, every 60 s during and after.
set -u
S=$1 P=$2 T=${3:-full}
L=/var/log/pgbackrest/$S.$T
Q="select to_char(now() at time zone 'utc', 'YYYY-MM-DD HH24:MI:SS'), subname, worker_type, pg_wal_lsn_diff(latest_end_lsn, received_lsn), date_trunc('second', now() - last_msg_receipt_time) from pg_stat_subscription"
lag() { psql -p "$P" -d postgres -AtF'|' -c "$Q"; }
date -u +%FT%TZ >"$L.started"
lag >"$L.lag-before.txt"
: >"$L.lag-during.txt"
(while :; do lag >>"$L.lag-during.txt"; sleep 60; done) &
W=$!
pgbackrest --stanza="$S" --type="$T" backup
rc=$?
kill "$W" 2>/dev/null
lag >"$L.lag-after.txt"
date -u +%FT%TZ >"$L.finished"
echo "$rc" >"$L.rc"
exit "$rc"
WRAP
put_text "$LAGGED" 0755 root:root "$lagged" "$LAGGED: one backup with subscription lag saved before, during and after"

while IFS='|' read -r st rpath cluster port pgdata sock; do
  [[ -n $st ]] || continue
  echo "-- stanza $st: cluster $PG_MAJOR/$cluster port $port, repo path $rpath"
  amode=$(pgq "$port" 'show archive_mode')
  pend=$(pgq "$port" "select pending_restart from pg_settings where name = 'archive_mode'")
  if [[ $amode != on || $pend != f ]]; then
    warn "stanza $st: archive_mode is $amode (pending_restart $pend); STOP, a restart is the main session's call. The rest is done, archive_command is not touched."
    bad=1
    archive_ok=0
  else
    archive_ok=1
  fi

  code=99
  if [[ -s $CIPHER ]]; then
    code=$(as_pg pgbackrest --stanza="$st" --output=json info | jq -r '.[0].status.code')
  fi
  if [[ $code == 1 || ($code == 99 && ! -s $CIPHER) ]]; then
    change "stanza $st created in $BUCKET$rpath (encrypted repo)"
    if ((!DRY)); then as_pg pgbackrest --stanza="$st" stanza-create; fi
  elif [[ $code == 99 ]]; then
    echo "stanza $st: info answered code 99:"
    as_pg pgbackrest --stanza="$st" info | sed -n '1,12p' || true
    exit 1
  fi

  afile=/etc/postgresql/$PG_MAJOR/$cluster/conf.d/zz-pgmove-archive.conf
  acmd="pgbackrest --stanza=$st archive-push %p"
  if ((archive_ok)); then
    want="archive_mode = on
archive_command = '$acmd'"
    live=$(pgq "$port" 'show archive_command')
    if [[ $live != "$acmd" ]]; then
      put_text "$afile" 0644 postgres:postgres "$want" "$afile: archive_command pgbackrest archive-push for $st, applied by reload"
      if ((!DRY)); then
        systemctl reload "postgresql@$PG_MAJOR-$cluster"
        for _ in $(seq 20); do
          if [[ $(pgq "$port" 'show archive_command') == "$acmd" ]]; then break; fi
          sleep 1
        done
        row=$(pgq "$port" "select setting || ' ' || coalesce(sourcefile, '') || ' ' || pending_restart from pg_settings where name = 'archive_command'")
        [[ $row == "$acmd $afile false" ]] || { echo "archive_command is not live from $afile: $row"; exit 1; }
      fi
    fi
  fi

  if ((DRY)); then
    echo "dry run: pgbackrest check skipped"
  else
    rc=0
    as_pg pgbackrest --stanza="$st" check || rc=$?
    echo "check $st: rc=$rc"
    if ((rc)); then bad=1; fi
  fi

  has_full=0
  if ((!DRY)) || [[ -s $CIPHER && $code != 1 ]]; then
    has_full=$(as_pg pgbackrest --stanza="$st" --output=json info | jq -r '[.[0].backup[]? | select(.type == "full" and .timestamp.stop != null)] | length')
  fi
  if ((has_full > 0)); then
    echo "stanza $st: $has_full full backup(s) listed"
  elif ((FULL)) && ((!bad)); then
    unit=pgbackrest-first-full-$st.service
    sub=$(systemctl show -p SubState --value "$unit" 2>/dev/null || true)
    if [[ $sub == running ]]; then
      echo "stanza $st: first full running as $unit"
    else
      dev=$(findmnt -no SOURCE --target "$pgdata")
      change "first full of $st started as transient unit $unit (Nice 10, CPUQuota 200%, IOWeight 10, read cap ${READ_MBPS}M on $dev)"
      if ((!DRY)); then
        systemctl reset-failed "$unit" 2>/dev/null || true
        systemctl stop "$unit" 2>/dev/null || true
        systemd-run --unit="${unit%.service}" --description="first pgBackRest full of stanza $st" \
          -p User=postgres -p Group=postgres -p Nice=10 -p CPUQuota=200% -p CPUWeight=10 -p IOWeight=10 \
          -p IOSchedulingClass=best-effort -p IOSchedulingPriority=7 -p MemoryMax=4G -p RemainAfterExit=yes \
          -p "IOReadBandwidthMax=$dev ${READ_MBPS}M" \
          "$LAGGED" "$st" "$port" full
      fi
    fi
  else
    echo "stanza $st: no full listed; pass --full to start one"
  fi
done <<<"$STANZA_LINES"

reload=0
while IFS=: read -r name b64; do
  [[ -n $name ]] || continue
  text=$(printf '%s' "$b64" | base64 -d)
  before=$changes
  put_text "/etc/systemd/system/$name" 0644 root:root "$text" "unit file $name installed, not enabled"
  if ((changes != before)); then reload=1; fi
done <<<"$UNIT_BLOBS"
if ((reload)) && ((!DRY)); then systemctl daemon-reload; fi
for name in $UNIT_LINES $VERIFY_LINES; do
  en=$(systemctl is-enabled "$name" 2>&1 || true)
  case $name in *@.service) ac=template ;; *) ac=$(systemctl is-active "$name" 2>&1 || true) ;; esac
  echo "unit $name: $en, $ac"
  case $name in
    *.timer) if [[ $en == enabled || $ac == active ]]; then warn "timer $name is $en and $ac; it switches with its box, nothing here changes it"; fi ;;
  esac
  if [[ $en == *"No such file"* ]] && ((!DRY)); then warn "unit $name does not exist"; fi
done

echo "remote: $changes change(s)"
exit "$bad"
REMOTE
  script=$(printf '%s\n%s\n' "$remote_vars" "$remote_body" | base64 | tr -d '\n')
  rc=0
  box_out=$(ssh -o BatchMode=yes -o ConnectTimeout=15 "$BOX_SSH" 'sudo -n bash -c "$(base64 -d)"' <<<"$script" 2>&1) || rc=$?
  printf '%s\n' "$box_out"
  n=$(grep -c -E '^(changed|would change):' <<<"$box_out" || true)
  changes=$((changes + n))
  if ((rc)); then
    if ((DRY)) && ! grep -q '^remote:' <<<"$box_out"; then
      echo "dry run: the box part could not read $BOX_SSH (rc=$rc), its steps are not listed"
    else
      die "box part on $BOX_SSH exited $rc"
    fi
  fi
fi

if ((DRY)); then echo "dry run: nothing ran"; fi
echo "$BOX: $changes change(s)"
