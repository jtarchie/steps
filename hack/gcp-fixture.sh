#!/usr/bin/env bash
# The GCP fixture the opt-in conformance tests run against.
#
# It exists for the same reason hack/aws-fixture.sh does: one surface of this
# repo cannot be tested any other way. internal/venue/iapdial is a port of
# the IAP relay protocol written from reading other clients (gcloud's Python,
# a community Go port), and its own tests use a fake relay written from the
# same reading — a shared misreading passes both sides. Only Google's real
# relay settles it. Likewise instances.simulateMaintenanceEvent is the only
# way to see a real preemption travel metadata → draining frame → eviction.
#
#   hack/gcp-fixture.sh up      # create, print the env to export
#   hack/gcp-fixture.sh env     # re-print the env for an existing fixture
#   hack/gcp-fixture.sh down    # destroy everything it created
#
# Instances carry the label steps-test-fixture=1 and `down` deletes exactly
# what carries it; the template and firewall rule are found by their fixed
# names. Needs gcloud, a project with billing, and Application Default
# Credentials (`gcloud auth application-default login`) — the tests sign with
# ADC, not with your gcloud CLI login, and the two are separate logins.
#
# Your account needs roles/compute.instanceAdmin.v1 and
# roles/iap.tunnelResourceAccessor on the project. The IAP API must be
# enabled (iap.googleapis.com); `up` enables compute and iap if it can.
#
# Cost: e2-small spot is roughly a cent an hour, the ephemeral external IP a
# fraction of that, and simulate-maintenance-event is free (rate-limited per
# region). Run `down` when finished and the whole exercise costs cents.
#
# The instance keeps an ephemeral EXTERNAL IP, deliberately: without one a
# --no-address instance has no route out (no docker install, no package
# updates) unless the subnet has Cloud NAT, and a fixture should not leave a
# NAT gateway billing in your project. Ingress is still only what the
# firewall allows; this script creates one rule, for the IAP range, and
# nothing for 0.0.0.0/0. (A default VPC ships its own permissive defaults —
# what this fixture proves is the tunnel, not your VPC's posture.)
set -eEuo pipefail

trap 'st=$?; [ "$BASH_SUBSHELL" -eq 0 ] || exit "$st"; die "failed at line $LINENO (exit $st). expired credentials, an API not enabled, or a permission missing in $PROJECT."' ERR

NAME=steps-test
LABEL_KEY=steps-test-fixture
ZONE="${STEPS_TEST_GCP_ZONE:-${CLOUDSDK_COMPUTE_ZONE:-us-central1-a}}"
PROJECT="${STEPS_TEST_GCP_PROJECT:-$(gcloud config get-value project 2>/dev/null)}"

# e2-small amd64: cheap, available everywhere, and x86 — which still
# exercises the ?binary= path from an arm64 dev machine.
MACHINE_TYPE=e2-small

say() { printf '%s\n' "$*" >&2; }
die() { printf 'gcp-fixture: %s\n' "$*" >&2; exit 1; }

gcloud_() { gcloud --project "$PROJECT" "$@"; }

need() { command -v "$1" >/dev/null 2>&1 || die "$1 is required"; }

# ---------------------------------------------------------------- discovery

find_instance() {
  gcloud_ compute instances list --zones="$ZONE" \
    --filter="labels.$LABEL_KEY=1 AND name=$NAME-worker" \
    --format='value(name)' 2>/dev/null | head -1
}

find_template() {
  gcloud_ compute instance-templates list --filter="name=$NAME-template" \
    --format='value(name)' 2>/dev/null | head -1
}

find_firewall() {
  gcloud_ compute firewall-rules list --filter="name=$NAME-iap" \
    --format='value(name)' 2>/dev/null | head -1
}

# ---------------------------------------------------------------------- up

create_firewall() {
  if [ -n "$(find_firewall)" ]; then
    say "  firewall rule $NAME-iap exists"

    return
  fi

  say "  creating firewall rule (IAP range -> 22, and nothing else)"
  # The one piece of ingress this design needs: Google's own relay range, to
  # sshd, only for instances carrying the fixture's network tag. There is
  # deliberately no 0.0.0.0/0 rule anywhere in this script.
  gcloud_ compute firewall-rules create "$NAME-iap" \
    --direction=INGRESS --action=ALLOW --rules=tcp:22 \
    --source-ranges=35.235.240.0/20 --target-tags="$NAME" >/dev/null
}

create_template() {
  if [ -n "$(find_template)" ]; then
    say "  instance template $NAME-template exists"

    return
  fi

  say "  creating instance template (spot, DELETE on preemption, no service account)"
  # The template owns the entire machine vocabulary — steps adds none of its
  # own, which is why the gcp://launch/ rung needs only this name. The
  # decisions, spelled out:
  #   --provisioning-model=SPOT            what the preemption test needs
  #   --instance-termination-action=DELETE a preempted worker vanishes, so
  #                                        no stopped disk keeps billing
  #   --no-service-account --no-scopes     the worker holds no GCP identity
  #   enable-guest-attributes=TRUE         how the dial verifies host keys
  #   enable-oslogin=FALSE                 metadata SSH keys must work even
  #                                        if the project enforces OS Login
  #   startup-script                       docker, for image: placed steps
  gcloud_ compute instance-templates create "$NAME-template" \
    --machine-type="$MACHINE_TYPE" \
    --image-family=debian-12 --image-project=debian-cloud \
    --no-service-account --no-scopes \
    --tags="$NAME" \
    --labels="$LABEL_KEY=1" \
    --provisioning-model=SPOT --instance-termination-action=DELETE \
    --metadata=enable-guest-attributes=TRUE,enable-oslogin=FALSE,startup-script='#!/bin/bash
apt-get update -q && apt-get install -y -q docker.io' >/dev/null
}

create_instance() {
  local id
  id=$(find_instance)

  if [ -n "$id" ]; then
    say "  instance $id exists"

    return
  fi

  say "  creating the static/parked instance (standard, not spot — other tests need it to stay)"
  # NOT from the template: the static worker must survive the whole test run,
  # and a spot machine can be taken while the parked-rung test is mid-stop —
  # while a template whose termination action is DELETE cannot simply have
  # its provisioning model overridden (the action is only valid for spot).
  # Same shape otherwise, spelled out.
  gcloud_ compute instances create "$NAME-worker" \
    --zone="$ZONE" \
    --machine-type="$MACHINE_TYPE" \
    --image-family=debian-12 --image-project=debian-cloud \
    --no-service-account --no-scopes \
    --tags="$NAME" \
    --labels="$LABEL_KEY=1" \
    --metadata=enable-guest-attributes=TRUE,enable-oslogin=FALSE,startup-script='#!/bin/bash
apt-get update -q && apt-get install -y -q docker.io' >/dev/null

  say "  waiting for the guest agent to publish host keys (up to ~2 min)"
  local waited=0
  until gcloud_ compute instances get-guest-attributes "$NAME-worker" \
      --zone="$ZONE" --query-path=hostkeys/ >/dev/null 2>&1; do
    [ "$waited" -ge 180 ] && die "no host keys in guest attributes — is enable-guest-attributes set?"
    sleep 10
    waited=$((waited + 10))
  done
}

build_worker_binary() {
  local out="$PWD/.gcp-fixture/steps-linux-amd64"
  mkdir -p "$(dirname "$out")"
  say "  cross-compiling the worker binary (linux/amd64)"
  ( cd "$PWD" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$out" . )
  printf '%s\n' "$out"
}

up() {
  need gcloud
  need go

  [ -n "$PROJECT" ] || die "no project — set STEPS_TEST_GCP_PROJECT or gcloud config set project"

  say "project $PROJECT zone $ZONE"

  gcloud_ services enable compute.googleapis.com iap.googleapis.com >/dev/null 2>&1 || \
    say "  (could not enable APIs — fine if they already are)"

  local binary
  create_firewall
  create_template
  create_instance
  binary=$(build_worker_binary)

  cat <<ENV

# ---- steps GCP fixture ready. Export these, then run the opt-in tests. ----
export STEPS_TEST_GCP_PROJECT=$PROJECT
export STEPS_TEST_GCP_ZONE=$ZONE
export STEPS_TEST_GCP_INSTANCE=$NAME-worker
export STEPS_TEST_GCP_TEMPLATE=$NAME-template
export STEPS_TEST_GCP_BINARY=$binary

#   go test ./internal/venue/iapdial -run TestRealGCP -v      # relay protocol conformance
#   go test ./internal/venue -run TestRealGCP -v              # dial, acquisition rungs
#   go test . -run TestRealGCP -v                             # a pipeline step, end to end
#   go test ./internal/venue -run TestRealGCPPreemption -v    # a real preemption (~15 min)
#
# Tear it all down with: hack/gcp-fixture.sh down
ENV
}

# -------------------------------------------------------------------- down

down() {
  need gcloud
  [ -n "$PROJECT" ] || die "no project — set STEPS_TEST_GCP_PROJECT or gcloud config set project"

  say "project $PROJECT — destroying the fixture"

  # Every labeled instance in the zone, not just the static one: the launch
  # rung's steps-* instances are deleted by the tests, but a killed test run
  # can leave one behind, and a leaked instance is the expensive mistake.
  local ids
  ids=$(gcloud_ compute instances list --zones="$ZONE" \
    --filter="labels.$LABEL_KEY=1" --format='value(name)' 2>/dev/null | tr '\n' ' ')

  if [ -n "${ids// /}" ]; then
    say "  deleting instances: $ids"
    # shellcheck disable=SC2086
    gcloud_ compute instances delete $ids --zone="$ZONE" --quiet >/dev/null
  fi

  if [ -n "$(find_template)" ]; then
    say "  deleting instance template"
    gcloud_ compute instance-templates delete "$NAME-template" --quiet >/dev/null
  fi

  if [ -n "$(find_firewall)" ]; then
    say "  deleting firewall rule"
    gcloud_ compute firewall-rules delete "$NAME-iap" --quiet >/dev/null
  fi

  rm -rf "$PWD/.gcp-fixture"
  say "done — check 'gcloud compute instances list' and 'gcloud compute disks list' for orphans"
}

env_only() {
  need gcloud
  [ -n "$PROJECT" ] || die "no project — set STEPS_TEST_GCP_PROJECT or gcloud config set project"

  local id
  id=$(find_instance)
  [ -n "$id" ] || die "no fixture instance found — run: hack/gcp-fixture.sh up"

  cat <<ENV
export STEPS_TEST_GCP_PROJECT=$PROJECT
export STEPS_TEST_GCP_ZONE=$ZONE
export STEPS_TEST_GCP_INSTANCE=$id
export STEPS_TEST_GCP_TEMPLATE=$(find_template)
export STEPS_TEST_GCP_BINARY=$PWD/.gcp-fixture/steps-linux-amd64
ENV
}

case "${1:-}" in
  up) up ;;
  down) down ;;
  env) env_only ;;
  *) die "usage: hack/gcp-fixture.sh up|env|down" ;;
esac
