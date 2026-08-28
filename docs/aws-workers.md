# Running a pipeline on an AWS worker

How to build, by hand, the AWS side of an `aws://` worker — and then run a pipeline that uses it.

[`hack/aws-fixture.sh`](../hack/aws-fixture.sh) does all of this in one command for this repo's own tests. This page is the same thing typed out, so you can see what each resource is for and adapt it. Every command is `aws` CLI v2 with credentials already configured. Those credentials need the EC2, IAM and S3 rights each step below uses, plus the SSM ones steps itself calls (`ssm:StartSession`, `ssm:SendCommand`, `ssm:GetCommandInvocation`, `ssm:DescribeInstanceInformation`) — and, for the `burst` worker in step 6, **`ec2:CreateFleet`**: the launch rung acquires machines through CreateFleet, never `ec2:RunInstances`. [infra.md](infra.md#remote-workers-tags) has the full set.

## What you are building, and why it is so small

A worker is **an EC2 instance steps can reach through SSM**. That is the whole design, and it decides the shape of everything below:

- **No inbound ports.** The instance's own `amazon-ssm-agent` dials *out* to the AWS control plane, and steps opens a session through that. The security group has no ingress rules at all — not "port 22 restricted", none.
- **No sshd, no agent to install.** steps pushes its own binary and starts it for one session.
- **No AWS credentials on the instance.** Artifact bytes arrive over presigned URLs the orchestrator mints per transfer, so the instance profile carries exactly one managed policy and nothing else.

```
your laptop ──ssm:StartSession──▶ AWS control plane ──▶ amazon-ssm-agent ──▶ steps _shim
     │                                                                            │
     └────────────────── presigned URLs ──▶ S3 ◀── artifact bytes ────────────────┘
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

The only policy the worker gets. It lets the SSM agent register the instance and carry a session — nothing else. **No S3 access**: the worker reads and writes artifacts through presigned URLs, so it never needs bucket rights of its own.

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

Picks a subnet that hands out public IPs, and asks AWS for the current Amazon Linux 2023 arm64 AMI id. A public IP is the cheap way for the agent to reach the control plane; a private subnet would need three interface VPC endpoints at about $21/month each.

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

The whole machine shape in one object. `tr -d '\n'` on the user data is not decoration: GNU coreutils `base64` wraps at 76 columns, and a newline inside the JSON string makes the whole document invalid. Keep the **id** it prints (`lt-…`) — `aws://launch/` in step 6 takes the id, not the name. The user data installs docker at first boot — **only needed if you want `image:` on a placed step**, which runs the command in a container on the worker. Omit it and everything else still works.

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

## 5. A bucket, and the worker's binary

```bash
BUCKET="$NAME-$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n')"
aws s3api create-bucket --bucket "$BUCKET"   # add --create-bucket-configuration LocationConstraint=$AWS_REGION outside us-east-1
```

Bucket names are globally unique, hence the random suffix. This is the artifact store: step trees and outputs move through it, and the worker reaches it with presigned URLs rather than credentials.

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o /tmp/steps-linux-arm64 .
```

Cross-compiles the binary the worker will run. `CGO_ENABLED=0` is what makes it a single static file that can be pushed and executed anywhere. Match `GOARCH` to your instance type — `arm64` for `t4g`/Graviton, `amd64` for `t3`/`m5`.

## 6. The pipeline

```yaml noexec=credentials
jobs:
- name: all-phases
  plan:
  - task: make
    outputs: [big]
    run: dd if=/dev/urandom of=big/blob bs=1M count=64 2>/dev/null

  - task: on-host
    tags: [aws]
    inputs: [big]
    outputs: [r1]
    run: |
      wc -c < big/blob > r1/out
      uname -m >> r1/out

  - task: in-container
    tags: [aws]
    image: alpine:3
    inputs: [big]
    outputs: [r2]
    run: |
      wc -c < big/blob > r2/out
      cat /etc/alpine-release >> r2/out

  - task: on-a-launched-machine
    tags: [burst]
    inputs: [big]
    outputs: [r3]
    run: uname -m > r3/out

  - task: publish
    inputs: [r1, r2, r3]
    run: |
      echo "host-placed:";      cat r1/out
      echo "containerized:";    cat r2/out
      echo "launched machine:"; cat r3/out
```

The pipeline names **capabilities** (`tags: [aws]`), never machines. That is what lets the same file run on somebody else's fleet.

```bash
steps run \
  --worker "aws=aws://$ID/var/tmp/steps?binary=/tmp/steps-linux-arm64&region=$AWS_REGION" \
  --worker "burst=aws://launch/$LT?version=1&binary=/tmp/steps-linux-arm64&region=$AWS_REGION" \
  --artifact-store "s3://$BUCKET/runs?region=$AWS_REGION" \
  pipeline.yml
```

The invocation names the **machines**. Three parts of that worker URL matter:

- **`/var/tmp/steps`** — the path picks a disk on the worker. Leave it off and you get the worker's temp directory, which on Amazon Linux 2023 is **tmpfs: memory, capped near half the machine's RAM, and cleared on reboot**. steps warns when it detects this, because a build tree competing with the build for RAM is a confusing way to fail.
- **`?binary=`** — pushes your cross-compiled binary, keyed by its content hash so it uploads once. This **requires `--artifact-store`**, checked before the run starts rather than after a machine has been acquired. Use `?shim=/usr/local/bin/steps` instead if your AMI already bakes one in.
- **`aws://launch/…?version=1`** — acquires a machine from that template version for the job and terminates it at the end. The path is the template **id** (`lt-…`, captured as `$LT` in step 3); a name is refused before the run starts. Acquisition is **per job, not per step**: the first placed step pays for the machine and the rest reuse it.

## 7. Tear it down

Money stops when the instance does; the rest is tidiness.

```bash
aws ec2 terminate-instances --instance-ids "$ID"
aws ec2 wait instance-terminated --instance-ids "$ID"
aws ec2 delete-launch-template --launch-template-id "$LT"
aws ec2 delete-security-group --group-id "$SG"
aws s3 rm "s3://$BUCKET" --recursive && aws s3api delete-bucket --bucket "$BUCKET"
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

## When it does not work

**`UnauthorizedOperation … explicit deny in a service control policy`** — an AWS Organizations SCP is refusing the call, and **nothing inside the account can override it**, `AdministratorAccess` included. SCPs are often region-scoped, so try another `AWS_REGION` first; if the deny applies everywhere, it has to be changed from the organization's management account, or you need an account outside that organization.

**`UnauthorizedOperation … CreateFleet`, on a `burst` step** — the launch rung acquires with `ec2:CreateFleet` (plus `ec2:DescribeInstances` to read the machine back and `ec2:TerminateInstances` to give it back), and a policy written from the `run-instances` command in step 4 grants none of them. The static worker is unaffected: it acquires nothing.

**The SSM agent never registers** — the instance profile is missing or lacks `AmazonSSMManagedInstanceCore`, or the instance has no route to the internet. Check with `aws ssm describe-instance-information`.

**A step succeeds and produces nothing, on a containerized placed step** — the daemon bind-mounts the step's tree, so the tree must be somewhere that daemon can see. Docker answers an unshared mount by silently mounting an **empty directory**. Name a real path in the worker URL.

**`is on tmpfs (… free) — that is memory, not disk`** — the warning above. Add a path to the worker URL.

**`?binary=` requires `--artifact-store`** — the binary is uploaded to the store and fetched by the bootstrap from a presigned URL. Checked before the run so you do not discover it after paying for a machine.

## See also

- [infra.md](infra.md) — `tags:`, every `aws://` option, the acquisition rungs, spot evictions, and the artifact store
- [`hack/aws-fixture.sh`](../hack/aws-fixture.sh) — all of the above as one script, plus a FIS role for testing spot interruptions
