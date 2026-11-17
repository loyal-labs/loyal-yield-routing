#!/usr/bin/env bash
# Creates one AWS box from boxes/<box>.conf and converges on rerun. Formatting and mounting the data volumes is not this script's job.
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
export AWS_PAGER='' AWS_EC2_METADATA_DISABLED=true

usage() {
  cat >&2 <<EOF
usage: $0 <box> [--dry-run]
boxes: $(for f in "$HERE"/boxes/*.conf; do basename "$f" .conf; done | paste -sd' ' -)
Ensures, each step found again by its Name tag: key pair, security group (tcp 22 in, nothing else),
instance (Ubuntu 24.04 from Canonical's SSM parameter, IMDSv2 required, termination protection on),
gp3 volumes attached and tagged, one Elastic IP associated. Never deletes, stops or terminates.
Prints "changed:" lines and ends "<box>: <n> change(s)". --dry-run prints each aws command it would run.
Reads need a live session; a dry run without one lists every step as missing and says so.
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
[[ $BOX =~ ^[a-z0-9-]+$ && -f $HERE/boxes/$BOX.conf ]] || { echo "box.sh: no box $BOX ($HERE/boxes/$BOX.conf)" >&2; usage; }

die() { echo "box.sh: $*" >&2; exit 1; }
command -v aws >/dev/null || die "aws CLI not found"
command -v jq >/dev/null || die "jq not found"

PROFILE='' ACCOUNT='' REGION='' INSTANCE_TYPE='' ARCH='' PUBKEY='' VOLUMES=()
# shellcheck source=/dev/null
source "$HERE/boxes/$BOX.conf"
PROFILE=${PROFILE:-${AWS_PROFILE:-}}
REGION=${REGION:-${AWS_REGION:-}}
PUBKEY=${PUBKEY:-${BOX_PUBKEY:-}}
[[ -n $PROFILE ]] || die "no AWS profile: set PROFILE in boxes/$BOX.conf or AWS_PROFILE"
[[ -n $REGION ]] || die "no region: set REGION in boxes/$BOX.conf or AWS_REGION"
[[ -n $INSTANCE_TYPE ]] || die "INSTANCE_TYPE missing in boxes/$BOX.conf"
case $ARCH in
  arm64) CANON_ARCH=arm64 ;;
  x86_64) CANON_ARCH=amd64 ;;
  *) die "ARCH in boxes/$BOX.conf must be arm64 or x86_64" ;;
esac
[[ -n $PUBKEY ]] || die "no public key: set PUBKEY in boxes/$BOX.conf or BOX_PUBKEY (path to a .pub file)"
PUBKEY=${PUBKEY/#\~/$HOME}
[[ -f $PUBKEY ]] || die "public key $PUBKEY does not exist"
case $(cut -d' ' -f1 "$PUBKEY") in
  ssh-ed25519 | ssh-rsa | ecdsa-sha2-*) ;;
  *) die "$PUBKEY is not an ssh public key" ;;
esac
((${#VOLUMES[@]} > 0)) || die "VOLUMES missing in boxes/$BOX.conf"

A() { aws --profile "$PROFILE" --region "$REGION" "$@"; }

changes=0 OFFLINE=0 OUT=''
change() { changes=$((changes + 1)); if ((DRY)); then echo "would change: $*"; else echo "changed: $*"; fi; }
warn() { echo "warning: $*"; }

rd() {
  if ((OFFLINE)); then OUT=''; return 0; fi
  OUT=$(A "$@")
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

if ! ident=$(A sts get-caller-identity --output json 2>&1); then
  if ((DRY)); then
    OFFLINE=1
    echo "dry run offline: sts get-caller-identity failed for profile $PROFILE ($(grep -m1 . <<<"$ident")); no state is read, every step is listed as missing"
  else
    die "no live session for profile $PROFILE: sts get-caller-identity failed: $(grep -m1 . <<<"$ident")"
  fi
else
  acct=$(jq -r .Account <<<"$ident")
  [[ -z $ACCOUNT || $ACCOUNT == "$acct" ]] || die "profile $PROFILE is account $acct, boxes/$BOX.conf wants $ACCOUNT"
  echo "$BOX: account $acct, region $REGION, $INSTANCE_TYPE ($ARCH)"
fi

parse_vol() { IFS=: read -r V_NAME V_SIZE V_IOPS V_TP <<<"$1"; }
vol_flags() {
  VF=(--size "$V_SIZE")
  if [[ -n ${V_IOPS:-} ]]; then VF+=(--iops "$V_IOPS"); fi
  if [[ -n ${V_TP:-} ]]; then VF+=(--throughput "$V_TP"); fi
}
root_mapping() {
  local ebs="VolumeSize=$V_SIZE,VolumeType=gp3,DeleteOnTermination=true"
  if [[ -n ${V_IOPS:-} ]]; then ebs+=",Iops=$V_IOPS"; fi
  if [[ -n ${V_TP:-} ]]; then ebs+=",Throughput=$V_TP"; fi
  echo "DeviceName=/dev/sda1,Ebs={$ebs}"
}

ROOT_SPEC=''
DATA_SPECS=()
for spec in "${VOLUMES[@]}"; do
  [[ $spec =~ ^[a-z0-9]+:[0-9]+(:[0-9]*){0,2}$ ]] || die "VOLUMES entry '$spec' must be name:sizeGB[:iops[:throughputMBps]]"
  if [[ $spec == root:* ]]; then ROOT_SPEC=$spec; else DATA_SPECS+=("$spec"); fi
done
[[ -n $ROOT_SPEC ]] || die "VOLUMES in boxes/$BOX.conf needs a root:<GB> entry"

# key pair
rd ec2 describe-key-pairs --filters "Name=tag:Name,Values=$BOX" --output json
KEY=$(pick '.KeyPairs[0].KeyName')
if [[ -z $KEY ]]; then
  change "key pair $BOX imported from $PUBKEY"
  mut ec2 import-key-pair --key-name "$BOX" --public-key-material "fileb://$PUBKEY" \
    --tag-specifications "ResourceType=key-pair,Tags=[{Key=Name,Value=$BOX}]" --output json
  KEY=$BOX
fi

# security group
rd ec2 describe-vpcs --filters Name=is-default,Values=true --output json
VPC=$(pick '.Vpcs[0].VpcId')
if [[ -z $VPC ]]; then
  if ((OFFLINE)); then VPC=default-vpc-id; else die "no default VPC in $REGION on profile $PROFILE"; fi
fi
SGNAME=$BOX-ssh
rd ec2 describe-security-groups --filters "Name=tag:Name,Values=$SGNAME" "Name=vpc-id,Values=$VPC" --output json
SG=$(pick '.SecurityGroups[0].GroupId')
SGPERMS=$(pick '.SecurityGroups[0].IpPermissions // []' | jq -c . 2>/dev/null || true)
[[ -n $SGPERMS ]] || SGPERMS='[]'
if [[ -z $SG ]]; then
  change "security group $SGNAME in $VPC"
  mut ec2 create-security-group --group-name "$SGNAME" --description "$BOX: key-only ssh" --vpc-id "$VPC" \
    --tag-specifications "ResourceType=security-group,Tags=[{Key=Name,Value=$SGNAME}]" --output json
  SG=$(pick .GroupId)
  if ((DRY)); then SG=new-security-group-id; fi
fi
has22=$(jq '[.[] | select(.IpProtocol=="tcp" and .FromPort==22 and .ToPort==22 and ([.IpRanges[].CidrIp] | index("0.0.0.0/0")))] | length' <<<"$SGPERMS")
if ((has22 == 0)); then
  change "security group $SGNAME allows tcp 22 from 0.0.0.0/0"
  mut ec2 authorize-security-group-ingress --group-id "$SG" --protocol tcp --port 22 --cidr 0.0.0.0/0 --output json
fi
extra=$(jq '[.[] | select((.IpProtocol=="tcp" and .FromPort==22 and .ToPort==22 and ([.IpRanges[].CidrIp] | index("0.0.0.0/0")) and (.IpRanges | length)==1 and (.Ipv6Ranges | length)==0 and (.UserIdGroupPairs | length)==0 and (.PrefixListIds | length)==0) | not)] | length' <<<"$SGPERMS")
if ((extra > 0)); then warn "security group $SGNAME has $extra inbound rule(s) besides tcp 22 from 0.0.0.0/0; left as is, remove by hand"; fi

# instance
rd ec2 describe-instances --filters "Name=tag:Name,Values=$BOX" Name=instance-state-name,Values=pending,running,stopping,stopped --output json
n=$(pick '[.Reservations[].Instances[]] | length')
((${n:-0} <= 1)) || die "$n live instances are tagged Name=$BOX; leave one"
IID=$(pick '.Reservations[0].Instances[0].InstanceId')
AZ=$(pick '.Reservations[0].Instances[0].Placement.AvailabilityZone')
if [[ -z $IID ]]; then
  SSMPARAM=/aws/service/canonical/ubuntu/server/24.04/stable/current/$CANON_ARCH/hvm/ebs-gp3/ami-id
  rd ssm get-parameter --name "$SSMPARAM" --output json
  AMI=$(pick .Parameter.Value)
  [[ -n $AMI ]] || { if ((OFFLINE)); then AMI=ami-from-ssm-parameter; else die "ssm parameter $SSMPARAM returned no AMI"; fi; }
  parse_vol "$ROOT_SPEC"
  change "instance $BOX ($INSTANCE_TYPE, $AMI)"
  mut ec2 run-instances --image-id "$AMI" --instance-type "$INSTANCE_TYPE" --count 1 --key-name "$KEY" \
    --security-group-ids "$SG" --block-device-mappings "$(root_mapping)" \
    --metadata-options HttpTokens=required,HttpEndpoint=enabled,HttpPutResponseHopLimit=1 \
    --disable-api-termination \
    --tag-specifications "ResourceType=instance,Tags=[{Key=Name,Value=$BOX}]" "ResourceType=volume,Tags=[{Key=Name,Value=$BOX-root}]" \
    --output json
  IID=$(pick '.Instances[0].InstanceId')
  if ((DRY)); then IID=new-instance-id; AZ=new-instance-az; fi
  mut ec2 wait instance-running --instance-ids "$IID"
  if ((!DRY)); then
    rd ec2 describe-instances --instance-ids "$IID" --output json
    AZ=$(pick '.Reservations[0].Instances[0].Placement.AvailabilityZone')
  fi
else
  have_type=$(pick '.Reservations[0].Instances[0].InstanceType')
  [[ $have_type == "$INSTANCE_TYPE" ]] || warn "instance $BOX is $have_type, conf says $INSTANCE_TYPE; resize is a stop, modify, start and is not done here"
  tokens=$(pick '.Reservations[0].Instances[0].MetadataOptions.HttpTokens')
  if [[ $tokens != required ]]; then
    change "instance $BOX requires IMDSv2"
    mut ec2 modify-instance-metadata-options --instance-id "$IID" --http-tokens required --http-endpoint enabled --output json
  fi
  rd ec2 describe-instance-attribute --instance-id "$IID" --attribute disableApiTermination --output json
  if [[ $(pick '.DisableApiTermination.Value') != true ]]; then
    change "instance $BOX termination protection on"
    mut ec2 modify-instance-attribute --instance-id "$IID" --disable-api-termination
  fi
fi

# root volume: converge size, IOPS and throughput
fix_volume() {
  local vol=$1 have_size have_iops have_tp
  have_size=$(pick '.Volumes[0].Size')
  have_iops=$(pick '.Volumes[0].Iops')
  have_tp=$(pick '.Volumes[0].Throughput')
  [[ -z $have_size ]] && return 0
  if ((have_size > V_SIZE)); then warn "volume $V_NAME is $have_size GB, conf says $V_SIZE; gp3 cannot shrink"; fi
  local args=()
  if ((have_size < V_SIZE)); then args+=(--size "$V_SIZE"); fi
  if [[ -n ${V_IOPS:-} && $have_iops != "$V_IOPS" ]]; then args+=(--iops "$V_IOPS"); fi
  if [[ -n ${V_TP:-} && $have_tp != "$V_TP" ]]; then args+=(--throughput "$V_TP"); fi
  if ((${#args[@]} > 0)); then
    change "volume $BOX-$V_NAME $vol: ${args[*]}"
    mut ec2 modify-volume --volume-id "$vol" "${args[@]}" --output json
  fi
}
if ((!OFFLINE)); then
  parse_vol "$ROOT_SPEC"
  rd ec2 describe-volumes --filters "Name=attachment.instance-id,Values=$IID" Name=attachment.device,Values=/dev/sda1 --output json
  rvol=$(pick '.Volumes[0].VolumeId')
  if [[ -n $rvol ]]; then fix_volume "$rvol"; fi
fi

# data volumes
letters=(f g h i j k l m n o p)
i=0
for spec in ${DATA_SPECS[@]+"${DATA_SPECS[@]}"}; do
  parse_vol "$spec"
  dev=/dev/sd${letters[$i]}
  i=$((i + 1))
  rd ec2 describe-volumes --filters "Name=tag:Name,Values=$BOX-$V_NAME" --output json
  VOL=$(pick '.Volumes[0].VolumeId')
  attached=$(pick '.Volumes[0].Attachments[0].InstanceId')
  if [[ -z $VOL ]]; then
    vol_flags
    change "volume $BOX-$V_NAME ($V_SIZE GB gp3${V_IOPS:+, $V_IOPS IOPS}${V_TP:+, $V_TP MB/s})"
    mut ec2 create-volume --availability-zone "$AZ" --volume-type gp3 "${VF[@]}" \
      --tag-specifications "ResourceType=volume,Tags=[{Key=Name,Value=$BOX-$V_NAME}]" --output json
    VOL=$(pick .VolumeId)
    if ((DRY)); then VOL=new-volume-id-$V_NAME; fi
    mut ec2 wait volume-available --volume-ids "$VOL"
    attached=''
  else
    fix_volume "$VOL"
  fi
  if [[ -z $attached ]]; then
    change "volume $BOX-$V_NAME attached to $BOX as $dev"
    mut ec2 attach-volume --volume-id "$VOL" --instance-id "$IID" --device "$dev" --output json
  elif [[ $attached != "$IID" ]]; then
    warn "volume $BOX-$V_NAME is attached to $attached, not $IID; left as is"
  fi
done

# Elastic IP
rd ec2 describe-addresses --filters "Name=tag:Name,Values=$BOX" --output json
ALLOC=$(pick '.Addresses[0].AllocationId')
IP=$(pick '.Addresses[0].PublicIp')
assoc=$(pick '.Addresses[0].InstanceId')
if [[ -z $ALLOC ]]; then
  change "elastic ip $BOX allocated"
  mut ec2 allocate-address --domain vpc --tag-specifications "ResourceType=elastic-ip,Tags=[{Key=Name,Value=$BOX}]" --output json
  ALLOC=$(pick .AllocationId)
  IP=$(pick .PublicIp)
  if ((DRY)); then ALLOC=new-allocation-id; IP=new-address; fi
  assoc=''
fi
if [[ -z $assoc ]]; then
  change "elastic ip $BOX ($IP) associated with $IID"
  mut ec2 associate-address --allocation-id "$ALLOC" --instance-id "$IID" --no-allow-reassociation --output json
elif [[ $assoc != "$IID" ]]; then
  warn "elastic ip $BOX is associated with $assoc, not $IID; left as is"
fi

if ((DRY)); then echo "dry run: nothing ran"; fi
echo "address: ${IP:-unknown}"
echo "$BOX: $changes change(s)"
