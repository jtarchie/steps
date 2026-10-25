# Real-cloud fixtures

For the opt-in real-cloud tests and `task bench:artifacts`. Each script's `up` prints `export` lines to paste, `env` prints them again, and `down` deletes everything its `up` made. The fixtures cost cents an hour, so run `down` when you're finished.

## One-time setup

**AWS.** Use `aws login` only to create a long-lived key. Its sessions last 15 minutes, and the Go SDK can't refresh them.

```sh
aws login
aws iam create-user --user-name steps-bench
aws iam attach-user-policy --user-name steps-bench --policy-arn arn:aws:iam::aws:policy/AdministratorAccess
aws iam create-access-key --user-name steps-bench   # copy AccessKeyId + SecretAccessKey
aws configure --profile cli                        # paste both; region us-east-2
AWS_PROFILE=cli aws sts get-caller-identity        # must print the steps-bench user
```

A new account's first instance in a region triggers an AWS validation email. That's expected.

**GCP.** The project needs billing linked.

```sh
gcloud auth login
gcloud config set project <project>
gcloud services enable compute.googleapis.com iap.googleapis.com
gcloud auth application-default login              # what steps and the tests sign with
```

**DigitalOcean.**

```sh
doctl auth init
export DIGITALOCEAN_ACCESS_TOKEN=<token>
```

## Up

```sh
AWS_PROFILE=cli AWS_REGION=us-east-2 hack/aws-fixture.sh up
hack/gcp-fixture.sh up
hack/do-fixture.sh up
```

Paste the `export` lines each one prints.

## Run

```sh
export AWS_PROFILE=cli AWS_REGION=us-east-2
eval "$(aws configure export-credentials --profile cli --format env)"   # the bench hands these keys to its workers

PAYLOAD=64M task bench:artifacts      # both clouds; drop PAYLOAD for the 256M default
go test ./internal/venue -run 'TestRealAWS|TestRealGCP|TestRealDO' -v
go test ./e2e -run 'TestRealAWS|TestRealGCP' -v
```

## Down

```sh
AWS_PROFILE=cli AWS_REGION=us-east-2 hack/aws-fixture.sh down
hack/gcp-fixture.sh down
hack/do-fixture.sh down

# nothing left billing:
AWS_PROFILE=cli aws ec2 describe-instances --region us-east-2 \
  --filters Name=instance-state-name,Values=pending,running,stopping,stopped \
  --query 'Reservations[].Instances[].InstanceId' --output text
gcloud compute instances list; gcloud compute disks list
doctl compute droplet list --tag-name steps
```

To retire the AWS key later: `aws iam delete-access-key --user-name steps-bench --access-key-id <id>`, `aws iam detach-user-policy --user-name steps-bench --policy-arn arn:aws:iam::aws:policy/AdministratorAccess`, then `aws iam delete-user --user-name steps-bench`.
