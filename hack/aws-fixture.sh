#!/usr/bin/env bash
# The AWS fixture the opt-in conformance tests run against.
#
# It exists because one surface of this repo cannot be tested any other way.
# Every AWS emulator surveyed (LocalStack, Moto, fakecloud) stops at the SSM
# control plane: none implements the session data channel, because that
# protocol terminates at amazon-ssm-agent ON the instance and AWS has never
# specified it. internal/venue/ssmdial is a port written from reading other
# clients, and its own tests use a fake agent written from the same reading —
# a shared misreading passes both sides. Only a real agent settles it.
#
#   hack/aws-fixture.sh up      # create, print the env to export
#   hack/aws-fixture.sh env     # re-print the env for an existing fixture
#   hack/aws-fixture.sh down    # destroy everything it created
#
# Everything is tagged steps-test-fixture=1, and `down` deletes exactly what
# carries that tag. Needs the AWS CLI and credentials for an account you do
# not mind creating and destroying things in.
#
# Cost, which is the reason for the shape: t4g.small is free through
# 2026-12-31 (750h/month, existing accounts included), the SSM agent and
# Session Manager are free, and a spot interruption via FIS is $0.10 per
# action-minute. The instance sits in a PUBLIC subnet with a public IPv4
# (~$0.005/hr) because the alternative — a private subnet — needs three
# interface VPC endpoints at $21+/month. Run `down` when finished and the
# whole exercise costs cents.
set -euo pipefail

NAME=steps-test
TAG_KEY=steps-test-fixture
REGION="${AWS_REGION:-${AWS_DEFAULT_REGION:-us-east-1}}"

# t4g.small: the free-trial size, and arm64 — which also exercises the
# ?binary= path, since this repo is developed on a machine whose binaries a
# Graviton instance cannot run.
INSTANCE_TYPE=t4g.small
AMI_PARAM=/aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-arm64

say() { printf '%s\n' "$*" >&2; }
die() { printf 'aws-fixture: %s\n' "$*" >&2; exit 1; }

aws_() { aws --region "$REGION" "$@"; }

need() { command -v "$1" >/dev/null 2>&1 || die "$1 is required"; }

account_id() { aws_ sts get-caller-identity --query Account --output text; }

# ---------------------------------------------------------------- discovery

# find_instance prints the running fixture instance id, or nothing.
find_instance() {
  aws_ ec2 describe-instances \
    --filters "Name=tag:$TAG_KEY,Values=1" \
              "Name=instance-state-name,Values=pending,running,stopping,stopped" \
    --query 'Reservations[].Instances[].InstanceId' --output text 2>/dev/null | tr -s '[:space:]' '\n' | head -1
}

find_bucket() {
  aws_ s3api list-buckets --query "Buckets[?starts_with(Name, '$NAME-')].Name" --output text 2>/dev/null | tr -s '[:space:]' '\n' | head -1
}

find_vpc() {
  aws_ ec2 describe-vpcs --filters Name=isDefault,Values=true \
    --query 'Vpcs[0].VpcId' --output text 2>/dev/null
}

find_subnet() {
  local vpc="$1"
  # A subnet that hands out public IPs, so the agent can reach the control
  # plane without VPC endpoints.
  aws_ ec2 describe-subnets --filters "Name=vpc-id,Values=$vpc" \
    "Name=map-public-ip-on-launch,Values=true" \
    --query 'Subnets[0].SubnetId' --output text 2>/dev/null
}

find_sg() {
  aws_ ec2 describe-security-groups --filters "Name=tag:$TAG_KEY,Values=1" \
    --query 'SecurityGroups[0].GroupId' --output text 2>/dev/null
}

find_lt() {
  aws_ ec2 describe-launch-templates --launch-template-names "$NAME-lt" \
    --query 'LaunchTemplates[0].LaunchTemplateId' --output text 2>/dev/null
}

# ---------------------------------------------------------------------- up

create_role() {
  local account; account=$(account_id)

  if aws iam get-role --role-name "$NAME-worker" >/dev/null 2>&1; then
    say "  role $NAME-worker exists"
  else
    say "  creating role $NAME-worker"
    aws iam create-role --role-name "$NAME-worker" \
      --tags "Key=$TAG_KEY,Value=1" \
      --assume-role-policy-document '{
        "Version":"2012-10-17",
        "Statement":[{"Effect":"Allow","Principal":{"Service":"ec2.amazonaws.com"},"Action":"sts:AssumeRole"}]
      }' >/dev/null
    # The ONLY policy a worker gets. Artifact bytes reach it through
    # presigned URLs, so it needs no S3 rights of its own — the property the
    # data plane was designed for, asserted here by omission.
    aws iam attach-role-policy --role-name "$NAME-worker" \
      --policy-arn arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore >/dev/null
  fi

  if aws iam get-instance-profile --instance-profile-name "$NAME-worker" >/dev/null 2>&1; then
    say "  instance profile exists"
  else
    say "  creating instance profile"
    aws iam create-instance-profile --instance-profile-name "$NAME-worker" \
      --tags "Key=$TAG_KEY,Value=1" >/dev/null
    aws iam add-role-to-instance-profile --instance-profile-name "$NAME-worker" \
      --role-name "$NAME-worker" >/dev/null
    say "  waiting for the instance profile to propagate"
    sleep 12
  fi

  # The FIS role, for the spot-interruption test. Separate from the worker
  # role: it acts on EC2 from the control plane, not from the instance.
  if aws iam get-role --role-name "$NAME-fis" >/dev/null 2>&1; then
    say "  role $NAME-fis exists"
  else
    say "  creating role $NAME-fis"
    aws iam create-role --role-name "$NAME-fis" \
      --tags "Key=$TAG_KEY,Value=1" \
      --assume-role-policy-document '{
        "Version":"2012-10-17",
        "Statement":[{"Effect":"Allow","Principal":{"Service":"fis.amazonaws.com"},"Action":"sts:AssumeRole"}]
      }' >/dev/null
    aws iam attach-role-policy --role-name "$NAME-fis" \
      --policy-arn arn:aws:iam::aws:policy/service-role/AWSFaultInjectionSimulatorEC2Access >/dev/null
  fi

  printf 'arn:aws:iam::%s:role/%s-fis\n' "$account" "$NAME"
}

create_sg() {
  local vpc="$1" sg
  sg=$(find_sg)

  if [ -n "$sg" ] && [ "$sg" != "None" ]; then
    say "  security group $sg exists"
    printf '%s\n' "$sg"

    return
  fi

  say "  creating security group (NO inbound rules — that is the point)"
  sg=$(aws_ ec2 create-security-group --group-name "$NAME-sg" \
    --description "steps test fixture: egress only, no inbound" \
    --vpc-id "$vpc" \
    --tag-specifications "ResourceType=security-group,Tags=[{Key=$TAG_KEY,Value=1}]" \
    --query GroupId --output text)

  # Deliberately no authorize-security-group-ingress call anywhere in this
  # script. A worker reached over SSM needs no open port, and the fixture
  # proves that by not opening one.
  printf '%s\n' "$sg"
}

create_lt() {
  local sg="$1" subnet="$2" ami lt

  lt=$(find_lt)
  if [ -n "$lt" ] && [ "$lt" != "None" ]; then
    say "  launch template $lt exists"
    printf '%s\n' "$lt"

    return
  fi

  ami=$(aws_ ssm get-parameter --name "$AMI_PARAM" --query 'Parameter.Value' --output text)
  say "  creating launch template (ami $ami)"

  # The launch template owns the entire EC2 vocabulary — steps adds none of
  # its own, which is why the aws://launch/ rung needs only this id.
  lt=$(aws_ ec2 create-launch-template --launch-template-name "$NAME-lt" \
    --tag-specifications "ResourceType=launch-template,Tags=[{Key=$TAG_KEY,Value=1}]" \
    --launch-template-data "{
      \"ImageId\": \"$ami\",
      \"InstanceType\": \"$INSTANCE_TYPE\",
      \"IamInstanceProfile\": {\"Name\": \"$NAME-worker\"},
      \"SecurityGroupIds\": [\"$sg\"],
      \"NetworkInterfaces\": [{\"DeviceIndex\": 0, \"AssociatePublicIpAddress\": true, \"SubnetId\": \"$subnet\", \"Groups\": [\"$sg\"], \"DeleteOnTermination\": true}],
      \"InstanceInitiatedShutdownBehavior\": \"terminate\",
      \"TagSpecifications\": [{\"ResourceType\": \"instance\", \"Tags\": [{\"Key\": \"$TAG_KEY\", \"Value\": \"1\"}]}]
    }" --query 'LaunchTemplate.LaunchTemplateId' --output text)

  printf '%s\n' "$lt"
}

create_instance() {
  local lt="$1" id

  id=$(find_instance)
  if [ -n "$id" ] && [ "$id" != "None" ]; then
    say "  instance $id exists"
    printf '%s\n' "$id"

    return
  fi

  say "  launching the static/parked instance"
  id=$(aws_ ec2 run-instances --launch-template "LaunchTemplateId=$lt" --count 1 \
    --query 'Instances[0].InstanceId' --output text)

  say "  waiting for it to run"
  aws_ ec2 wait instance-running --instance-ids "$id"

  say "  waiting for the SSM agent to register (up to ~3 min)"
  local waited=0
  until [ "$(aws_ ssm describe-instance-information \
        --filters "Key=InstanceIds,Values=$id" \
        --query 'length(InstanceInformationList)' --output text 2>/dev/null)" = "1" ]; do
    [ "$waited" -ge 180 ] && die "the SSM agent never registered — check the instance profile and egress"
    sleep 10
    waited=$((waited + 10))
  done

  printf '%s\n' "$id"
}

create_bucket() {
  local bucket
  bucket=$(find_bucket)

  if [ -n "$bucket" ] && [ "$bucket" != "None" ]; then
    say "  bucket $bucket exists"
    printf '%s\n' "$bucket"

    return
  fi

  bucket="$NAME-$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n')"
  say "  creating bucket $bucket"

  if [ "$REGION" = "us-east-1" ]; then
    aws_ s3api create-bucket --bucket "$bucket" >/dev/null
  else
    aws_ s3api create-bucket --bucket "$bucket" \
      --create-bucket-configuration "LocationConstraint=$REGION" >/dev/null
  fi

  aws_ s3api put-bucket-tagging --bucket "$bucket" \
    --tagging "TagSet=[{Key=$TAG_KEY,Value=1}]" >/dev/null

  printf '%s\n' "$bucket"
}

build_worker_binary() {
  local out="$PWD/.aws-fixture/steps-linux-arm64"
  mkdir -p "$(dirname "$out")"
  say "  cross-compiling the worker binary (linux/arm64)"
  # CGO_ENABLED=0 is what makes this possible at all — the guard the build
  # task documents, load-bearing here.
  ( cd "$PWD" && CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o "$out" . )
  printf '%s\n' "$out"
}

up() {
  need aws
  need go

  say "region $REGION"
  say "identity $(account_id)"

  local vpc subnet sg lt id bucket binary fis
  vpc=$(find_vpc); [ "$vpc" != "None" ] || die "no default VPC in $REGION — create one or pick another region"
  subnet=$(find_subnet "$vpc"); [ "$subnet" != "None" ] || die "no public subnet in $vpc"
  say "vpc $vpc subnet $subnet"

  fis=$(create_role)
  sg=$(create_sg "$vpc")
  lt=$(create_lt "$sg" "$subnet")
  id=$(create_instance "$lt")
  bucket=$(create_bucket)
  binary=$(build_worker_binary)

  cat <<ENV

# ---- steps AWS fixture ready. Export these, then run the opt-in tests. ----
export STEPS_TEST_AWS_REGION=$REGION
export STEPS_TEST_AWS_INSTANCE=$id
export STEPS_TEST_AWS_TEMPLATE=$lt
export STEPS_TEST_AWS_BUCKET=$bucket
export STEPS_TEST_AWS_BINARY=$binary
export STEPS_TEST_AWS_FIS_ROLE=$fis

#   go test ./internal/venue/ssmdial -run TestRealAWS -v      # protocol conformance
#   go test ./internal/venue -run TestRealAWS -v              # dial, acquisition rungs
#   go test . -run TestRealAWS -v                             # a pipeline step, end to end
#   go test ./internal/venue -run TestRealAWSSpotEviction -v  # FIS interruption (costs ~\$0.20)
#
# Tear it all down with: hack/aws-fixture.sh down
ENV
}

# -------------------------------------------------------------------- down

down() {
  need aws
  say "region $REGION — destroying everything tagged $TAG_KEY=1"

  local ids lt sg bucket
  ids=$(aws_ ec2 describe-instances --filters "Name=tag:$TAG_KEY,Values=1" \
    "Name=instance-state-name,Values=pending,running,stopping,stopped" \
    --query 'Reservations[].Instances[].InstanceId' --output text 2>/dev/null | tr -s '[:space:]' ' ')

  if [ -n "${ids// /}" ]; then
    say "  terminating instances: $ids"
    # shellcheck disable=SC2086
    aws_ ec2 terminate-instances --instance-ids $ids >/dev/null
    # shellcheck disable=SC2086
    aws_ ec2 wait instance-terminated --instance-ids $ids
  fi

  lt=$(find_lt)
  if [ -n "$lt" ] && [ "$lt" != "None" ]; then
    say "  deleting launch template $lt"
    aws_ ec2 delete-launch-template --launch-template-id "$lt" >/dev/null
  fi

  sg=$(find_sg)
  if [ -n "$sg" ] && [ "$sg" != "None" ]; then
    say "  deleting security group $sg"
    aws_ ec2 delete-security-group --group-id "$sg" >/dev/null 2>&1 || \
      say "  (security group still in use; re-run down in a minute)"
  fi

  bucket=$(find_bucket)
  if [ -n "$bucket" ] && [ "$bucket" != "None" ]; then
    say "  emptying and deleting bucket $bucket"
    aws_ s3 rm "s3://$bucket" --recursive >/dev/null 2>&1 || true
    aws_ s3api delete-bucket --bucket "$bucket" >/dev/null 2>&1 || true
  fi

  if aws iam get-instance-profile --instance-profile-name "$NAME-worker" >/dev/null 2>&1; then
    say "  deleting instance profile"
    aws iam remove-role-from-instance-profile --instance-profile-name "$NAME-worker" \
      --role-name "$NAME-worker" >/dev/null 2>&1 || true
    aws iam delete-instance-profile --instance-profile-name "$NAME-worker" >/dev/null 2>&1 || true
  fi

  for role in "$NAME-worker:arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore" \
              "$NAME-fis:arn:aws:iam::aws:policy/service-role/AWSFaultInjectionSimulatorEC2Access"; do
    local r="${role%%:*}" p="${role#*:}"
    if aws iam get-role --role-name "$r" >/dev/null 2>&1; then
      say "  deleting role $r"
      aws iam detach-role-policy --role-name "$r" --policy-arn "$p" >/dev/null 2>&1 || true
      aws iam delete-role --role-name "$r" >/dev/null 2>&1 || true
    fi
  done

  rm -rf "$PWD/.aws-fixture"
  say "done"
}

env_only() {
  need aws
  local id lt bucket
  id=$(find_instance); lt=$(find_lt); bucket=$(find_bucket)
  [ -n "$id" ] && [ "$id" != "None" ] || die "no fixture instance found — run: hack/aws-fixture.sh up"

  cat <<ENV
export STEPS_TEST_AWS_REGION=$REGION
export STEPS_TEST_AWS_INSTANCE=$id
export STEPS_TEST_AWS_TEMPLATE=$lt
export STEPS_TEST_AWS_BUCKET=$bucket
export STEPS_TEST_AWS_BINARY=$PWD/.aws-fixture/steps-linux-arm64
export STEPS_TEST_AWS_FIS_ROLE=arn:aws:iam::$(account_id):role/$NAME-fis
ENV
}

case "${1:-}" in
  up) up ;;
  down) down ;;
  env) env_only ;;
  *) die "usage: hack/aws-fixture.sh up|env|down" ;;
esac
