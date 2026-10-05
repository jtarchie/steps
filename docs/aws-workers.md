# Running a pipeline on an AWS worker

How to build, by hand, the AWS side of an `aws://` worker — and then run a pipeline that uses it.

[`hack/aws-fixture.sh`](../hack/aws-fixture.sh) does all of this in one command for this repo's own tests. This page is the same thing typed out, so you can see what each resource is for and adapt it. Every command is `aws` CLI v2 with credentials already configured. Those credentials need the EC2 and IAM rights each step below uses, plus the SSM ones steps itself calls (`ssm:StartSession`, `ssm:SendCommand`, `ssm:GetCommandInvocation`, `ssm:DescribeInstanceInformation`) — and, for the `burst` worker in step 6, **`ec2:CreateFleet`**, plus **`ec2:CreateTags`** on `arn:aws:ec2:*:*:instance/*`: the launch rung acquires machines through CreateFleet, never `ec2:RunInstances`, and labels each one in that same request (see step 7). The tag grant can be narrowed to launch time with a condition of `ec2:CreateAction` in `CreateFleet`/`RunInstances` — both listed because which one an instant fleet reports is unconfirmed, and that narrowed form is untested here. [infra.md](infra.md#remote-workers-tags) has the full set.

## What you are building, and why it is so small

A worker is **an EC2 instance steps can reach through SSM, with docker on it**. That is the whole design, and it decides the shape of everything below:

- **No inbound ports.** The instance's own `amazon-ssm-agent` dials *out* to the AWS control plane, and steps opens a session through that — a port-forward to the instance's own sshd on loopback, with steps' ssh riding inside it. The security group has no ingress rules at all — not "port 22 restricted", none.
- **Nothing of steps' to install.** sshd and the SSM agent are already on the AMI; docker comes from the user data below. The first time a `steps` process reaches the instance, it sends one SSM command that creates a `steps` account in the `docker` group and installs that process's own ssh key for it — `restrict,port-forwarding`, expiring after twelve hours — and reports the instance's ssh host key, which steps then pins. No key to distribute, no `known_hosts` to maintain: IAM is the only door.
- **No AWS credentials on the instance.** The instance profile carries exactly one managed policy and nothing else.

```
your laptop ──ssm:StartSession──▶ AWS control plane ──▶ amazon-ssm-agent ──▶ sshd (127.0.0.1:22)
                                                                                  │
                                              /var/run/docker.sock ◀── ssh streamlocal
                                                       │
                                                    dockerd ──▶ the step's container, its volumes
```

Set these once so the commands below are copy-pasteable:

```bash
export AWS_REGION=us-east-1
export NAME=steps-worker
```

## 1. The instance's identity

An EC2 instance cannot have a policy directly; it assumes a **role**, delivered through an **instance profile** attached at launch.

```bash
aws iam create-role --role-name "$NAME" \
  --assume-role-policy-document '{
    "Version":"2012-10-17",
    "Statement":[{"Effect":"Allow","Principal":{"Service":"ec2.amazonaws.com"},"Action":"sts:AssumeRole"}]
  }'
```

Creates the role and says *who may assume it* — the EC2 service, on behalf of an instance. This trust policy grants no permissions; it only names who is allowed to ask.

```bash
aws iam attach-role-policy --role-name "$NAME" \
  --policy-arn arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore
```

The only policy the worker gets. It lets the SSM agent register the instance and carry a session — nothing else. **No S3 access, no ECR access**: steps' trees reach the instance through the session, and a public image needs no credentials. A private image is pulled by the instance's daemon with *your* docker credentials, read from your `~/.docker/config.json` and sent with the pull request, so the instance needs none of its own for that either.

```bash
aws iam create-instance-profile --instance-profile-name "$NAME"
aws iam add-role-to-instance-profile --instance-profile-name "$NAME" --role-name "$NAME"
sleep 12
```

An instance profile is the wrapper EC2 actually accepts at launch; the second command puts the role inside it. The `sleep` is not superstition — IAM is eventually consistent, and a launch that references a profile created a second ago frequently fails.

## 2. A security group with nothing open

```bash
VPC=$(aws ec2 describe-vpcs --filters Name=isDefault,Values=true \
  --query 'Vpcs[0].VpcId' --output text)

SG=$(aws ec2 create-security-group --group-name "$NAME" \
  --description "steps worker: egress only" --vpc-id "$VPC" \
  --query GroupId --output text)
```

Finds your default VPC and makes a group in it. **There is deliberately no `authorize-security-group-ingress` here.** A new group already allows all egress and no ingress, which is exactly what a worker needs — the agent dials out, nothing dials in.

## 3. A launch template

A launch template is a **container of numbered, immutable versions**, each holding a complete machine shape: AMI, instance type, disk, subnet, security groups, instance profile, user data. You never edit a version; you append a new one.

```bash
SUBNET=$(aws ec2 describe-subnets --filters "Name=vpc-id,Values=$VPC" \
  "Name=map-public-ip-on-launch,Values=true" \
  --query 'Subnets[0].SubnetId' --output text)

AMI=$(aws ssm get-parameter \
  --name /aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-arm64 \
  --query 'Parameter.Value' --output text)
```

Picks a subnet that hands out public IPs, and asks AWS for the current Amazon Linux 2023 arm64 AMI id. A public IP is the cheap way for the agent to reach the control plane, and for the daemon to reach the registries its images come from; a private subnet would need three interface VPC endpoints at about $21/month each, plus a route to those registries.

```bash
USERDATA=$(printf '#!/bin/bash\ndnf install -y docker\nsystemctl enable --now docker\n' | base64 | tr -d '\n')

LT=$(aws ec2 create-launch-template --launch-template-name "$NAME" \
  --launch-template-data "{
    \"ImageId\": \"$AMI\",
    \"InstanceType\": \"t4g.small\",
    \"IamInstanceProfile\": {\"Name\": \"$NAME\"},
    \"NetworkInterfaces\": [{\"DeviceIndex\": 0, \"AssociatePublicIpAddress\": true,
      \"SubnetId\": \"$SUBNET\", \"Groups\": [\"$SG\"], \"DeleteOnTermination\": true}],
    \"InstanceInitiatedShutdownBehavior\": \"terminate\",
    \"UserData\": \"$USERDATA\"
  }" --query 'LaunchTemplate.LaunchTemplateId' --output text)
```

The whole machine shape in one object. `tr -d '\n'` on the user data is not decoration: GNU coreutils `base64` wraps at 76 columns, and a newline inside the JSON string makes the whole document invalid. Keep the **id** it prints (`lt-…`) — `aws://launch/` in step 6 takes the id, not the name. The user data installs docker at first boot, and it is **not optional**: every step placed on an `aws://` worker runs in a container on that daemon. A stock AL2023 AMI has no docker; one you bake yourself can skip the user data.

Later, to change the shape, append a version instead of editing:

```bash
aws ec2 create-launch-template-version --launch-template-name "$NAME" --source-version 1 \
  --launch-template-data '{"BlockDeviceMappings":[{"DeviceName":"/dev/xvda",
    "Ebs":{"VolumeSize":200,"VolumeType":"gp3","DeleteOnTermination":true}}]}'
```

Copies version 1 and applies a delta — here a 200GB root volume. `DeviceName` must match the AMI's real root device (`/dev/xvda` on AL2023); get it wrong and you have attached a *second* disk and are paying for both. This is why steps has no `?disk=`: EC2 already models machine shape, and `?version=` names which shape you meant.

## 4. The instance

```bash
ID=$(aws ec2 run-instances --launch-template "LaunchTemplateId=$LT" --count 1 \
  --query 'Instances[0].InstanceId' --output text)

aws ec2 wait instance-running --instance-ids "$ID"
```

Launches one machine from the template and waits for EC2 to call it running. This by-hand `run-instances` is the **static** worker only — a machine you own and steps merely dials. The `burst` worker in step 6 acquires its own machines, and it does that with **`ec2:CreateFleet`** in `instant` mode; steps never calls `RunInstances`. A policy modelled on this command gets you through this section and then fails at the first `tags: [burst]` step. **Running is not reachable** — the SSM agent still has to register, which takes another minute or two:

```bash
until [ "$(aws ssm describe-instance-information \
    --filters "Key=InstanceIds,Values=$ID" \
    --query 'length(InstanceInformationList)' --output text)" = "1" ]; do sleep 10; done
```

Polls until SSM admits it can reach the instance. If this never finishes, the cause is almost always the instance profile (not attached, or missing `AmazonSSMManagedInstanceCore`) or no route out to the internet.

## 5. Docker on the instance

Nothing to build and nothing to upload: an `aws://` worker runs every placed step in a container on the instance's own docker daemon, so the only thing to check is that the user data from step 3 finished installing it.

```bash
CMD=$(aws ssm send-command --instance-ids "$ID" --document-name AWS-RunShellScript \
  --parameters 'commands=["docker info >/dev/null 2>&1 && echo docker-ok"]' \
  --query 'Command.CommandId' --output text)
sleep 5
aws ssm get-command-invocation --command-id "$CMD" --instance-id "$ID" \
  --query StandardOutputContent --output text
```

Prints `docker-ok` once the daemon answers. steps waits for this itself — the first connection to an instance waits up to four minutes for docker before it creates its `steps` account — so this is only a check that the user data worked, not a step steps needs. It is the same `SendCommand` path steps uses for that install, which is also why the credentials above need `ssm:SendCommand` and `ssm:GetCommandInvocation`.

## 6. The pipeline

```yaml noexec=credentials
jobs:
- name: all-phases
  plan:
  - task: make
    outputs: [big]
    run: dd if=/dev/urandom of=big/blob bs=1M count=64 2>/dev/null

  - task: measure
    tags: [aws]
    image: alpine:3
    inputs: [big]
    outputs: [r1]
    run: |
      wc -c < big/blob > r1/out
      uname -m >> r1/out

  - task: measure-again
    tags: [aws]
    image: alpine:3
    inputs: [big]
    outputs: [r2]
    run: |
      wc -c < big/blob > r2/out
      cat /etc/alpine-release >> r2/out

  - task: on-a-launched-machine
    tags: [burst]
    image: alpine:3
    inputs: [big]
    outputs: [r3]
    run: uname -m > r3/out

  - task: publish
    inputs: [r1, r2, r3]
    run: |
      echo "first:";            cat r1/out
      echo "second:";           cat r2/out
      echo "launched machine:"; cat r3/out
```

The pipeline names **capabilities** (`tags: [aws]`), never machines. That is what lets the same file run on somebody else's fleet. `measure-again` sends nothing for `big`: the worker kept the 64MB input from `measure`, found it by its digest, and gave the second step a copy-on-write view of it.

```bash
steps run \
  --worker "aws=aws://$ID?region=$AWS_REGION" \
  --worker "burst=aws://launch/$LT?version=1&region=$AWS_REGION" \
  pipeline.yml
```

The invocation names the **machines**. Two parts of those worker URLs matter:

- **`?region=`** — where the instance lives. It need not match your default region, and on a profile with no default it is the only thing that says.
- **`aws://launch/…?version=1`** — acquires a machine from that template version for the job and terminates it at the end. The path is the template **id** (`lt-…`, captured as `$LT` in step 3); a name is refused before the run starts. Acquisition is **per job, not per step**: the first placed step pays for the machine and the rest reuse it.

## 7. Tear it down

Money stops when the instance does; the rest is tidiness.

```bash
aws ec2 terminate-instances --instance-ids "$ID"
aws ec2 wait instance-terminated --instance-ids "$ID"
aws ec2 delete-launch-template --launch-template-id "$LT"
aws ec2 delete-security-group --group-id "$SG"
aws iam remove-role-from-instance-profile --instance-profile-name "$NAME" --role-name "$NAME"
aws iam delete-instance-profile --instance-profile-name "$NAME"
aws iam detach-role-policy --role-name "$NAME" \
  --policy-arn arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore
aws iam delete-role --role-name "$NAME"
```

In dependency order: the instance holds the security group and the profile, so it goes first. A security group deletion that fails with "in use" usually just means the instance is still terminating — wait a minute and repeat.

Check for orphans, because a leaked instance is the expensive mistake:

```bash
aws ec2 describe-instances --filters "Name=instance-state-name,Values=running,stopped" \
  --query 'Reservations[].Instances[].[InstanceId,State.Name]' --output text
aws ec2 describe-volumes --filters "Name=status,Values=available" --query 'Volumes[].VolumeId' --output text
```

The second one matters on its own: a volume that outlives its instance keeps billing with nothing pointing at it.

Every machine the launch rung creates is tagged at creation with `steps-worker` (a short hash naming the machine — template, version, capacity and region as written in the worker mapping, so a region left to the environment is not part of it — and never any part of the worker URL), `steps-host` and `steps-pid` (the process that launched it: a `steps web` daemon or a one-shot `steps run`/`test`). One the process never gave back — it was killed, it ran out of memory, its host died — is listed by the tag, per region, so run it with each worker mapping's `?region=`:

```bash
aws ec2 describe-instances \
  --filters Name=tag-key,Values=steps-worker Name=instance-state-name,Values=pending,running,stopping,stopped \
  --query 'Reservations[].Instances[].[InstanceId,Tags[?Key==`steps-host`]|[0].Value,Tags[?Key==`steps-pid`]|[0].Value,LaunchTime]' \
  --output text
```

An instance is a leftover only if no process with that pid is running on that host: a live one, possibly on another machine, may be mid-job on it, so check before terminating. A crashed run's transcript names the same hash (`launching from template … (steps-worker=…)`), which is the way from a dead run to its machine. steps never reads these tags back — a tag never makes a machine steps' to reuse or delete.

## When it does not work

**`UnauthorizedOperation … explicit deny in a service control policy`** — an AWS Organizations SCP is refusing the call, and **nothing inside the account can override it**, `AdministratorAccess` included. SCPs are often region-scoped, so try another `AWS_REGION` first; if the deny applies everywhere, it has to be changed from the organization's management account, or you need an account outside that organization.

**`UnauthorizedOperation … CreateFleet` or `… CreateTags`, on a `burst` step** — the launch rung acquires with `ec2:CreateFleet` and tags the machine in the same request with `ec2:CreateTags` (plus `ec2:DescribeInstances` to read the machine back and `ec2:TerminateInstances` to give it back), and a policy written from the `run-instances` command in step 4 grants none of them. **A policy that worked before steps tagged its machines needs `ec2:CreateTags` added.** The refusal can also arrive as the fleet's own error, reading `no capacity for the requested worker: … not authorized … ec2:CreateTags` — that is the same missing grant, not an empty spot pool. Either way no untagged machine is left behind: EC2 refuses the whole launch. The static worker is unaffected: it acquires nothing.

**The SSM agent never registers** — the instance profile is missing or lacks `AmazonSSMManagedInstanceCore`, or the instance has no route to the internet. Check with `aws ssm describe-instance-information`.

**`no docker group after four minutes: docker is not installed on this instance`** — the install step found no docker. The launch template's user data did not run or failed; check `/var/log/cloud-init-output.log` on the instance (through Session Manager), or bake docker into the AMI.

**`a docker+ worker runs every step in a container, and this step names no image`** — every step placed on an `aws://` worker needs an `image:`, including a resource type's. Refused before the run starts, so before a machine is paid for.

**`ssh: unable to authenticate` after an instance was replaced** — steps notices a stale install (an expired key, or a new root volume under the same instance id, which also changes the host key) and installs once more by itself; seeing this anyway means the `steps` account could not be created. Run the install's own check by hand: `id steps` and `getent group docker` through Session Manager.

## See also

- [infra.md](infra.md) — `tags:`, every `aws://` option, the acquisition rungs, spot evictions, and what a docker+ worker keeps
- [`hack/aws-fixture.sh`](../hack/aws-fixture.sh) — all of the above as one script, plus a FIS role for testing spot interruptions
