# Running a pipeline on a GCP worker

How to build, by hand, the GCP side of a `gcp://` worker — and then run a pipeline that uses it.

[`hack/gcp-fixture.sh`](../hack/gcp-fixture.sh) does all of this in one command for this repo's own tests. This page is the same thing typed out, so you can see what each resource is for and adapt it. Every command is `gcloud` with a project already configured. Your own account needs two roles on the project: **`roles/compute.instanceAdmin.v1`** (create, start, stop, delete instances; write their metadata and labels; read guest attributes) and **`roles/iap.tunnelResourceAccessor`** (open the tunnel). A custom role instead needs `compute.instances.setLabels` among its create permissions, because the launch rung labels each machine as it creates it. steps itself authenticates with Application Default Credentials — `gcloud auth application-default login` once, and both the compute calls and the tunnel sign with it.

## What you are building, and why it is almost as small as the AWS one

A worker is **a Compute Engine instance steps can reach through IAP TCP forwarding, with docker on it**. GCP has no SSM-shaped exec channel, so the SSH contract *is* the transport — the tunnel terminates at the instance's own sshd, and steps reaches the docker socket over that ssh connection exactly as `docker+ssh://` does. Every placed step runs in a container on that daemon. That decides the shape of everything below:

- **No public address.** The client opens an outbound websocket to Google's relay (`tunnel.cloudproxy.app`), and the relay reaches the instance's VPC-internal address. The instance needs no external IP.
- **One firewall rule, not zero.** The honest difference from `aws://`: the relay connects to a real TCP port, so the firewall must admit Google's IAP range `35.235.240.0/20` to port 22. Nothing else — no internet-reachable port, and that range is Google's relay infrastructure, not the internet.
- **No keys to manage, no users to create.** steps mints an ephemeral SSH key per orchestrator process, installs its public half for a `steps` account through instance metadata in the expiring `google-ssh` form (the guest agent stops honoring it after 12 hours, and steps prunes expired entries whenever it installs a fresh one), and verifies the host against the SSH host keys the instance itself publishes to **guest attributes** — which is how a machine created moments ago can be verified at all.
- **No GCP credentials on the instance.** The template below attaches no service account; nothing steps does on it needs one.
- **Nothing of steps' installed.** The instance needs sshd (every stock image runs it), the guest agent, and docker — which the startup script below installs.

```
your laptop ──wss──▶ IAP relay (tunnel.cloudproxy.app) ──35.235.240.0/20──▶ sshd :22
                                                                               │
                                           /var/run/docker.sock ◀── ssh streamlocal
                                                    │
                                                 dockerd ──▶ the step's container, its volumes
```

Set these once so the commands below are copy-pasteable:

```bash
export PROJECT=$(gcloud config get-value project)
export ZONE=us-central1-a
export NAME=steps-worker
```

## 1. The firewall rule

```bash
gcloud compute firewall-rules create "$NAME-iap" \
  --direction=INGRESS --action=ALLOW --rules=tcp:22 \
  --source-ranges=35.235.240.0/20 --target-tags="$NAME"
```

The one piece of ingress this design needs: Google's IAP range, to 22, only for instances carrying the `$NAME` network tag. There is deliberately no `0.0.0.0/0` anywhere.

## 2. An instance template

An instance template is **one immutable object** holding a complete machine shape: image, machine type, disks, network, service account, provisioning model, metadata. You never edit one; a different shape is a different template — which is why `gcp://` has no `?version=`.

```bash
gcloud compute instance-templates create "$NAME" \
  --machine-type=e2-small \
  --image-family=debian-12 --image-project=debian-cloud \
  --no-service-account --no-scopes \
  --no-address \
  --tags="$NAME" \
  --provisioning-model=SPOT --instance-termination-action=DELETE \
  --metadata=enable-guest-attributes=TRUE,enable-oslogin=FALSE,startup-script='#!/bin/bash
apt-get update && apt-get install -y docker.io'
```

Each flag is a decision:

- **`--no-service-account --no-scopes`** — the worker holds no GCP identity at all. Nothing steps does on it needs one.
- **`--no-address`** — no external IP. The tunnel is the only way in. (An instance with no external IP also has no route *out* to the internet unless the subnet has Cloud NAT — and both the docker install above and every image pull need one. `gcloud compute routers create` + `nats create` is the two-command version, or bake an image with docker and serve images from Artifact Registry over Private Google Access.)
- **`--provisioning-model=SPOT --instance-termination-action=DELETE`** — spot is 60–91% off, and **DELETE matters**: a preempted spot instance otherwise stops rather than vanishes, and a stopped instance's disk keeps billing with nothing pointing at it.
- **`--metadata=enable-guest-attributes=TRUE`** — how the instance attests its SSH host keys. Without it the dial refuses the worker (it cannot verify who it is talking to) and says so; `?hostkey=` is the manual alternative.
- **`enable-oslogin=FALSE`** — metadata SSH keys are how steps authenticates, and a project that enforces OS Login silently ignores them. This instance-level setting wins over the project's.
- The **startup script installs docker**, and it is **not optional**: every step placed on a `gcp://` worker runs in a container. An image with docker baked in boots faster and can drop the script. Container-Optimized OS ships docker too, but is untested here.

## 3. The static instance

```bash
gcloud compute instances create "$NAME" \
  --zone="$ZONE" \
  --machine-type=e2-small \
  --image-family=debian-12 --image-project=debian-cloud \
  --no-service-account --no-scopes \
  --no-address \
  --tags="$NAME" \
  --metadata=enable-guest-attributes=TRUE,enable-oslogin=FALSE,startup-script='#!/bin/bash
apt-get update && apt-get install -y docker.io'
```

One machine you own and steps merely dials — the **static** worker. Deliberately **not** `--source-instance-template="$NAME"`: that template is `SPOT` with `--instance-termination-action=DELETE`, so a machine built from it can be reclaimed and *destroyed* mid-run — and the static rung acquires nothing, so there is no re-placement to fall back on. Spot belongs on the rungs that own the machine's whole life (`gcp://stopped/`, `gcp://launch/`), where an eviction is a re-placement rather than a worker that stopped existing.

## 4. Docker on the instance

Nothing to build and nothing to push: a `gcp://` worker runs every placed step in a container on the instance's own docker daemon, which the startup script in steps 2 and 3 installs. steps does the rest on its first connection: it waits (up to four minutes) for `docker info` to answer, and where the guest agent created its `steps` account before the `docker` group existed — the usual order on a machine whose startup script is still running — adds the account to the group with the passwordless sudo the guest agent grants every metadata-key account, then reconnects, since only a fresh login picks up a new group.

## 5. The pipeline

```yaml noexec=credentials
jobs:
- name: all-phases
  plan:
  - task: make
    outputs: [big]
    run: dd if=/dev/urandom of=big/blob bs=1M count=64 2>/dev/null

  - task: on-the-static-worker
    tags: [gcp]
    image: alpine:3
    inputs: [big]
    outputs: [r1]
    run: |
      wc -c < big/blob > r1/out
      uname -m >> r1/out

  - task: on-a-launched-machine
    tags: [burst]
    image: alpine:3
    inputs: [big]
    outputs: [r2]
    run: uname -m > r2/out

  - task: publish
    inputs: [r1, r2]
    run: |
      echo "static worker:";    cat r1/out
      echo "launched machine:"; cat r2/out
```

The pipeline names **capabilities** (`tags: [gcp]`), never machines — the same split as everywhere else.

```bash
steps run \
  --worker "gcp=gcp://$NAME?project=$PROJECT&zone=$ZONE" \
  --worker "burst=gcp://launch/$NAME?project=$PROJECT&zone=$ZONE" \
  pipeline.yml
```

The invocation names the **machines**. The parts of that worker URL that matter:

- **`?project=` / `?zone=`** — where the instance lives. Omittable when `GOOGLE_CLOUD_PROJECT`/`CLOUDSDK_COMPUTE_ZONE` (or the ADC credentials' own project) already say.
- **`gcp://launch/…`** — acquires an instance from that template for the job and deletes it at the end. Acquisition is **per job, not per step**; a job whose placed steps are all cache hits acquires nothing. `gcp://stopped/$NAME` is the middle rung: start a parked instance, use it, stop it again (`?idle=` holds it warm between back-to-back jobs).

There is deliberately **no `?capacity=`**: the template decides its own provisioning model, so a spot job names a spot template.

## 6. Tear it down

```bash
gcloud compute instances delete "$NAME" --zone="$ZONE" --quiet
gcloud compute instance-templates delete "$NAME" --quiet
gcloud compute firewall-rules delete "$NAME-iap" --quiet
```

Check for orphans, because a leaked instance is the expensive mistake:

```bash
gcloud compute instances list
gcloud compute disks list --filter="-users:*"
```

The second one matters on its own: a disk that outlives its instance keeps billing with nothing pointing at it — which is exactly what `--instance-termination-action=DELETE` in the template exists to prevent.

Every machine the launch rung creates is labelled at creation with `steps-worker` (a short hash naming the machine — template, project and zone as written in the worker mapping, so ones left to the environment are not part of it — and never any part of the worker URL), `steps-host` and `steps-pid` (the process that launched it: a `steps web` daemon or a one-shot `steps run`/`test`). One the process never gave back — it was killed, it ran out of memory, its host died — is listed by the label:

```bash
gcloud compute instances list --filter="labels.steps-worker:*" \
  --format="table(name,zone,labels.steps-host,labels.steps-pid,creationTimestamp)"
```

An instance is a leftover only if no process with that pid is running on that host: a live one, possibly on another machine, may be mid-job on it, so check before deleting. A crashed run's transcript names the same hash (`creating steps-… from template … (steps-worker=…)`). steps never reads these labels back — a label never makes a machine steps' to reuse or delete.

## When it does not work

**`the IAP relay refused the connection (HTTP 403)`** — your account lacks `iap.tunnelInstances.accessViaIAP` (grant `roles/iap.tunnelResourceAccessor`), or the IAP API is disabled on the project.

**`nothing listening there, or no firewall rule allows the IAP range`** — the relay reached the VPC and nothing answered: the firewall rule from step 1 is missing or its target tag does not match the instance, or sshd is not running. On a machine acquired seconds ago steps waits this out (sshd is still booting); if it never resolves, it is the firewall.

**`ssh: unable to authenticate`** — the project enforces OS Login, which ignores metadata SSH keys. Set `enable-oslogin=FALSE` in the instance (or template) metadata, as step 2 does.

**`guest attributes are disabled … set enable-guest-attributes=TRUE … or pin the key with ?hostkey=`** — the template did not set the metadata key. Either fix the template or pin: `ssh-keyscan` the machine once from somewhere that can reach it, or read the key out of the serial console log, and put its `SHA256:…` fingerprint in the URL (URL-encode it — the base64 can contain `+`).

**The startup script never installs docker** — a `--no-address` instance has no route to the internet without Cloud NAT. Add NAT to the subnet's region, or bake an image with docker preinstalled. steps waits four minutes for docker on a new machine and then fails the step at the daemon: `the docker daemon at /var/run/docker.sock did not answer`.

**`a docker+ worker runs every step in a container, and this step names no image`** — every step placed on a `gcp://` worker needs an `image:`, including a resource type's. Refused before the run starts, so before a machine is paid for.

## See also

- [infra.md](infra.md) — `tags:`, every `gcp://` option, the acquisition rungs, preemption, and what a docker+ worker keeps
- [aws-workers.md](aws-workers.md) — the same walk on AWS, and how the two transports differ
- [`hack/gcp-fixture.sh`](../hack/gcp-fixture.sh) — all of the above as one script, for this repo's opt-in real-GCP tests
