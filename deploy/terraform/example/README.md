# Yumdrake on ECS: an example Terraform configuration

**This is an example to adapt, not a module to apply as it is.** Read every file,
change what does not fit your organisation, and have it reviewed (see `REVIEW.md`
for the IAM review that came with it) before you use any part of it. It has been
checked with `terraform fmt` and `terraform validate` only. It has never been
planned or applied by its authors.

It names no account, region, host name, certificate, VPC or identity provider.
Every such value is a variable with a description and a type; the ones with no
default are required.

## What it creates

- A KMS key for stored secrets and a second KMS key for config-file copies.
- A PostgreSQL database on RDS: encrypted, private, automated backups with
  point-in-time recovery, deletion protection, TLS required. RDS generates the
  password and keeps it in Secrets Manager.
- A Valkey replication group on ElastiCache, encrypted at rest and in transit.
- Two private, encrypted, versioned S3 buckets: config copies (objects expire
  after 30 days) and the log archive (expiry set by `log_archive_retention_days`).
- Security groups. The runner has no inbound rule. The database and the cache
  accept connections from the two services only.
- Task roles for the orchestrator and the runner, and a task execution role.
- An ECS cluster on Fargate with an orchestrator service behind an internal HTTPS
  load balancer, and a runner service that scales on the `QueueDepth` metric.
- CloudWatch log groups and four alarms to an SNS topic you provide.

## What it assumes already exists

- A VPC with at least two private subnets in different availability zones and a
  way out to the internet or to AWS endpoints (NAT or VPC endpoints), because the
  services call GitHub, Slack and AWS APIs.
- An ACM certificate in this region for the host name users will type.
- An SNS topic for alarms.
- The two container images, pulled from the registry in `image_repository`. A
  private registry needs `registry_credentials_secret_arn`. Both images are
  published for `linux/amd64` and `linux/arm64` by the release workflow.
- DNS: you create the record for `orchestrator_hostname` yourself and point it at
  the `load_balancer_dns_name` output.
- An identity provider, if you use SAML. It is configured in Yumdrake's Admin
  pages, not here.

## What to change first

1. Copy `terraform.tfvars.example` to `terraform.tfvars` (git-ignored) and replace
   every placeholder. Never put a secret in it.
2. Configure your own remote state in a backend block of your own (this example
   declares none on purpose).
3. Decide on the settings you may want different: `client_cidr_blocks` (who may
   reach the load balancer), the database and cache sizes, `cpu_architecture`,
   the retention values, and the egress rules in `network.tf`.
4. Review `iam.tf` and `kms.tf` against your own policies.

## Order of use together with `target-account/`

The two modules depend on each other in a circle: the roles in `target-account/`
must trust the Yumdrake task roles (which this module creates), and the task
roles may assume only the roles listed in `discovery_role_arns` and
`deployment_role_arns` (which `target-account/` creates). Break it like this:

1. Apply this module with `discovery_role_arns = []` and `deployment_role_arns = []`.
   The task roles then cannot assume any role, which is the safe state.
2. Take the outputs `orchestrator_task_role_arn` and `runner_task_role_arn`, and
   apply `target-account/` in each AWS account Yumdrake manages.
3. Put the new `discovery_role_arn` and `deployment_role_arn` values into the two
   lists here and apply this module again.
4. Enter the same role ARNs and external ids when you connect each account in
   Yumdrake's Admin pages.

## Settings the application reads from later plans

`ecs.tf` sets some variables that only later versions of the application read.
Each is marked in a comment as "read from plan N onward". Older versions ignore
them. They are: `YUMDRAKE_ENV` and `YUMDRAKE_RUNNER_CONCURRENCY` (plan 4);
`YUMDRAKE_SECRETS_MODE=kms`, `YUMDRAKE_KMS_KEY_ID`, `YUMDRAKE_AWS_ENDPOINT_URL`
(optional), `YUMDRAKE_ARTIFACT_BUCKET`, `YUMDRAKE_ARTIFACT_KMS_KEY_ID` and
`YUMDRAKE_LOG_BUCKET` (plan 5); `YUMDRAKE_METRICS` (plan 11, task 4); `YUMDRAKE_VALKEY_TLS` (plan 11, task 7).
Until the version you run reads them, the services refuse to start in `kms` mode
or do not emit metrics, so match `image_version` to the plan you have.

## Things to check against the version you run

- **Valkey and TLS.** Encryption in transit is on, so the client must use TLS.
  `ecs.tf` sets `YUMDRAKE_VALKEY_TLS=true` for both services; the application
  reads it from plan 11 onward (an older version ignores it and cannot reach the
  cache).
- **Database password.** The database URL carries no password. The task gets
  `PGPASSWORD` from the Secrets Manager secret, which the PostgreSQL driver reads
  when the URL has none (the application tests this). The URL uses
  `sslmode=require`; use `verify-full` if you add the RDS certificate bundle to
  the image.
- **Encryption purposes.** The purposes in `locals.tf` (`aws-discovery`,
  `aws-deploy`, `github-connection`, `slack`, `saml-sp-key`) are the ones the
  plans name. If a later version adds a purpose, add it to the right list there.
- **Load balancer.** The idle timeout is 300 seconds so the live event stream
  (Server-Sent Events) is not cut between events. Keep it above the heartbeat
  interval of the version you run.

## Files

| File | Contents |
|---|---|
| `versions.tf`, `providers.tf`, `locals.tf` | Version pins, provider, shared names and settings |
| `variables.tf`, `terraform.tfvars.example`, `outputs.tf` | Inputs, placeholders, outputs |
| `network.tf` | Security groups |
| `database.tf`, `cache.tf`, `storage.tf` | RDS, ElastiCache, S3 |
| `kms.tf`, `iam.tf` | Keys and roles |
| `ecs.tf`, `load-balancer.tf`, `alarms.tf` | Services, load balancer, alarms |
| `REVIEW.md` | The hand review: every IAM statement and the checklist of rules |

## Checks

`make tf-check` from the repository root runs `terraform fmt -check -recursive`
and, in this directory and in `target-account/`, `terraform init -backend=false`
and `terraform validate`. No credentials are needed. Nothing is planned or applied.
Terraform 1.5.7 or later 1.x, and the AWS provider 5.100.x, are what it was
checked with.
