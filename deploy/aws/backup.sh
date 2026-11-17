#!/usr/bin/env bash
# Ensures the pgBackRest backup storage and its access for one box from boxes/<box>.conf and converges on rerun.
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
export AWS_PAGER='' AWS_EC2_METADATA_DISABLED=true

usage() {
  cat >&2 <<EOF
usage: $0 <box> [--dry-run]
boxes: $(for f in "$HERE"/boxes/*.conf; do if grep -q '^BACKUP_BUCKET=.' "$f"; then basename "$f" .conf; fi; done | paste -sd' ' -)
Ensures, each step found again by name: the S3 bucket BACKUP_BUCKET from the conf in the box's account and region (all public
access blocked, SSE-S3 default encryption, versioning on, bucket-owner-enforced ownership, a policy denying non-TLS requests,
a lifecycle rule expiring noncurrent versions after 30 days and aborting incomplete multipart uploads after 7), the IAM role
<box>-pgbackrest with one inline policy (list on the bucket, get, put and delete on its objects), the instance profile of the
same name holding that role, and the profile attached to the instance (found by its Name tag).
Never deletes a bucket, object, role or policy and sets no object lock or retention. Prints "changed:" lines and ends
"<box>: <n> change(s)". --dry-run prints each aws command it would run. Reads need a live session; a dry run without one lists
every step as missing and says so.
EOF
  exit 2
}

BOX=${1:-}
[[ -n $BOX ]] || usage
shift
DRY=0
for a in "$@"; do
  case $a in
    --dry-run) DRY=1 ;;
    *) usage ;;
  esac
done
[[ $BOX =~ ^[a-z0-9-]+$ && -f $HERE/boxes/$BOX.conf ]] || { echo "backup.sh: no box $BOX ($HERE/boxes/$BOX.conf)" >&2; usage; }

die() { echo "backup.sh: $*" >&2; exit 1; }
command -v aws >/dev/null || die "aws CLI not found"
command -v jq >/dev/null || die "jq not found"

PROFILE='' ACCOUNT='' REGION='' BACKUP_BUCKET=''
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

if ((DRY)); then echo "dry run: nothing ran"; fi
echo "$BOX: $changes change(s)"
