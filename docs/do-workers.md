# Running a pipeline on a DigitalOcean worker

How a `do://` worker works, what to set up on the DigitalOcean side, and a pipeline that uses one.

`do://` only **launches**: each job that needs it gets a droplet (DigitalOcean's word for a virtual machine) that steps creates and deletes. There is no rung for a droplet you already own — that is `docker+ssh://root@<ip>?hostkey=SHA256:…` — and no parked rung, because DigitalOcean bills a powered-off droplet at the full rate, so stopping one saves nothing.

## The defaults, up front

`do://launch/` alone is a complete mapping. Unlike `aws://` and `gcp://`, where a template you wrote owns the machine's shape, `do://` picks one for you unless you say otherwise:

| Option | Default | Change it with |
|---|---|---|
| image | `docker-20-04` | `do://launch/<image slug or id>` |
| region | `nyc3` | `?region=sfo3` |
| size | `s-1vcpu-1gb` | `?size=s-2vcpu-4gb` |

`docker-20-04` is the slug DigitalOcean still publishes its **Docker 1-Click** image under; the name is historical, and the image behind it is built on Ubuntu 22.04. Any other image works if it runs sshd, honors cloud-init, and **already has docker** — steps installs nothing, and a droplet whose docker never answers fails the step after four minutes saying so. A numeric image names a snapshot or custom image by id. `?idle=` keeps a droplet for the next job, exactly as on the other clouds.

## What happens, and what it costs you in trust

```
your laptop ──ssh :22──▶ droplet's public IPv4 ──▶ sshd
                                                    │
                     /var/run/docker.sock ◀── ssh streamlocal
                              │
                           dockerd ──▶ the step's container, its volumes
```

1. steps mints a fresh **host key** for the droplet — the key a server proves its identity with over SSH — and writes it into the create request's `user_data` as cloud-init configuration, together with the public half of this process's login key. No key is uploaded to your DigitalOcean account.
2. It creates the droplet, waits until it is `active` **and** has a public IPv4 address (it is assigned after the create answers), and connects as `root`.
3. The connection accepts **only the host key steps minted**. A droplet that has just booted still offers its image's own host key until cloud-init swaps it, so steps refuses that and retries for up to four minutes; a key it never minted is never accepted.
4. Every placed step runs in a container on the droplet's docker daemon, reached over the ssh connection exactly as `docker+ssh://` reaches one.
5. When the job ends the droplet is deleted, or kept for `?idle=`.

Two honest differences from the other clouds:

- **Port 22 must be reachable from wherever steps runs.** `aws://` needs no inbound port at all and `gcp://` admits only Google's relay range; a droplet is reached at its public address. Put a **Cloud Firewall** on the droplets — steps tags each one `steps`, so a firewall applied to that tag can admit port 22 from your own address only. steps does not manage firewalls.
- **The host's private key travels in `user_data`**, and DigitalOcean serves `user_data` back to anything on the droplet through its metadata service, which a step's container can reach. So a placed step can read the host key of the droplet it runs on. It learns nothing about any other droplet — every droplet's key is new, and it is deleted with the droplet.

Password login is turned off in the same cloud-init configuration. DigitalOcean emails a root password for a droplet created without an account SSH key; that password does not open ssh.

## Setting up DigitalOcean

One API token. Create it under **API → Tokens** with full access, or a custom scope covering droplet create, read and delete and tag create, and export it where steps runs:

```bash
export DIGITALOCEAN_ACCESS_TOKEN=dop_v1_…
```

`DIGITALOCEAN_TOKEN` is read when `DIGITALOCEAN_ACCESS_TOKEN` is not set — the same two names `doctl` and Terraform use.

Then a firewall, so port 22 is open to you rather than the internet (`doctl` shown; the control panel does the same):

```bash
doctl compute firewall create --name steps-workers \
  --inbound-rules "protocol:tcp,ports:22,address:$(curl -s https://ifconfig.me)/32" \
  --outbound-rules "protocol:tcp,ports:all,address:0.0.0.0/0,address:::/0 protocol:udp,ports:all,address:0.0.0.0/0,address:::/0" \
  --tag-names steps
```

Every droplet steps creates carries the bare tag `steps`, which is what this firewall applies to — from the moment the droplet exists. The Docker 1-Click image also runs `ufw` with port 22 open, so the cloud firewall is the layer that narrows it.

## The pipeline

```yaml noexec=credentials
jobs:
- name: burst
  plan:
  - task: make
    outputs: [data]
    run: echo seed > data/seed.txt

  - task: on-a-droplet
    tags: [do]
    image: alpine:3
    inputs: [data]
    outputs: [report]
    run: |
      cat data/seed.txt > report/out
      uname -m >> report/out
      echo "$STEPS_WORKER" >> report/out

  - task: publish
    inputs: [report]
    run: cat report/out
```

The pipeline names a capability (`tags: [do]`); the invocation names the machine:

```bash
steps run --worker "do=do://launch/?region=sfo3" pipeline.yml
steps web --worker "do=do://launch/?idle=10m"
```

Acquisition is **per job**: the first placed step creates the droplet and every later placed step in that job reuses it; a job whose placed steps are all cache hits creates nothing. Under `steps web`, concurrent jobs share one droplet per mapping, as on the other clouds; `do://launch/?exclusive&max=3` gives each job a droplet of its own instead, at most three at once ([infra.md](infra.md#remote-workers-tags)).

## Finding what a crash left behind

Every droplet steps creates is tagged in the create request itself, so no crash can leave an untagged one: `steps` (for the firewall), `steps-worker:<hash>` (which mapping — image, region and size, never any part of the URL), `steps-host:<hostname>` and `steps-pid:<pid>` (which process), and under `steps web`, `steps-owner:<daemon id>-launch-<unix seconds>`.

A restarted `steps web` finds its own: before serving any pipeline it lists the droplets in every region a `do://` mapping names and deletes each one that carries its own `steps-owner` tag, was created within fifteen minutes of that stamp, and has the image and size a mapping there launches. The image and size are recorded by DigitalOcean itself; a tag alone is never proof, since anyone with tag write could put one on any droplet.

Anything else — a one-shot `steps run` killed mid-job, say — is listed by tag:

```bash
doctl compute droplet list --format ID,Name,Region,Tags,Created | grep steps-worker
```

A droplet is a leftover only if no process with that `steps-pid` is running on that `steps-host`.

## When it does not work

**`no DigitalOcean token`** — neither `DIGITALOCEAN_ACCESS_TOKEN` nor `DIGITALOCEAN_TOKEN` is set in the environment steps runs in.

**`waiting 4m0s for droplet …'s sshd: … dial tcp …:22: i/o timeout`** — port 22 is not reachable from here: a firewall that does not admit your address, or one that does not apply to the droplet.

**`… the worker's host key is not the one it was pinned to`** after four minutes — the droplet never installed the host key steps sent: an image without cloud-init, or one whose cloud-init ignores `ssh_keys`. Or something between you and the droplet is answering for it.

**`docker never answered on droplet …`** — the image has no docker. Use the default, or bake docker into your snapshot.

**`do:// only launches droplets`** — the mapping names a droplet, or `stopped/`. Use `do://launch/…`, or `docker+ssh://root@<ip>?hostkey=…` for a droplet you own.

## See also

- [infra.md](infra.md) — `tags:`, the acquisition rungs, `?idle=`, and what a docker+ worker keeps
- [aws-workers.md](aws-workers.md), [gcp-workers.md](gcp-workers.md) — the same walk on the other clouds
- [`hack/do-fixture.sh`](../hack/do-fixture.sh) — a firewall for your address only, for this repo's opt-in real-DigitalOcean tests
