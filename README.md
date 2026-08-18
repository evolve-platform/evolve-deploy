# evolve-deploy

Stateless deployments to AWS, GCP, Azure and Kubernetes.

```console
$ evolve-deploy apply deploy/tst.yaml

purchase  27ec167
  container-app/evolve-tst-purchase                  c2a1950 -> 27ec167

site  27ec167
  container-app/evolve-tst-site                      5367b03 -> 27ec167

  container-app/evolve-tst-purchase                  27ec167 in 1m21s
  container-app/evolve-tst-site                      27ec167 in 1m27s

done in 1m45s
```

It reads a config file, compares it against what is actually running, and rolls
out the difference. There is no state file and no lock — the cloud already knows
what it runs — so running it twice does nothing the second time.

## Install

```sh
go install github.com/evolve-platform/evolve-deploy@latest
```

Or take a binary from the releases. It needs nothing else installed: no cloud
CLI, no Terraform, no runtime.

## Commands

```sh
evolve-deploy diff  deploy/tst.yaml              # what would happen, changes nothing
evolve-deploy diff  deploy/prd.yaml --exit-code  # non-zero if production has drifted
evolve-deploy apply deploy/tst.yaml              # roll it out
```

The config path is the only positional argument, and the only thing that decides
which environment is touched. Useful flags:

| | |
|---|---|
| `--set name=version` | override a version without editing the file; how CI deploys a test build |
| `--only a,b` | limit the run to these services |
| `-v`, `--verbose` | log every API call and every poll, to stderr |
| `--workers N` | services rolled out at once (default 16) |
| `--env NAME` | override what `${env}` and `{{.env}}` expand to (default: the filename) |

`diff` is read-only. It resolves every reference, checks that every image
exists, picks the right container and compares the full desired state — so a
broken reference or an image that was never pushed is found without touching
anything.

## Config

One file per environment, one cloud per file.

```yaml
cloud:
  provider: azure
  subscription: bbbf237a-8c9e-492a-b6a3-9b0bd4869690
  resource_group: evolve-tst

services:
  # The common case: one deployable, named after the service.
  purchase:
    version: 27ec167
    targets:
      - { type: container-app, name: evolve-tst-purchase }

  # One image, five deployables. The version is written once, so a job can never
  # drift away from the service it shares an image with.
  catalog-commercetools:
    version: 27ec167
    env:
      SERVICE_NAME: catalog-commercetools                 # shared by all five
    targets:
      - { type: container-app, name: evolve-tst-catalog-commercetools }
      - type: container-app-job
        name: evolve-tst-catalog-ct-products
        env:
          SERVICE_ENTRYPOINT: sync-products/index.mjs     # only this target
      - type: container-app-job
        name: evolve-tst-catalog-ct-categories
        env:
          SERVICE_ENTRYPOINT: sync-categories/index.mjs
```

This is a deployment lockfile: desired state in git, actual state in the cloud.
`git log` is the deploy history and `git revert` is a rollback.

A service has one version and one or more targets. `env`, `envFrom`, `before`
and `after` apply to every target; a target may add to or override `env`.

There is a short form for a service with exactly one target named after it:

```yaml
  site:
    version: abc1234
    type: ecs
    cluster: platform
```

### Where it deploys to

The `cloud` block is a tagged union: `provider` selects which fields apply and
the rest are rejected, so the file has the same shape whichever cloud it targets.

| `provider` | `type` | Rest of the block |
|---|---|---|
| `aws` | `ecs`, `lambda` | `account` + `region` |
| `gcp` | `cloud-run` | `project` + `region` |
| `azure` | `container-app`, `container-app-job`, `function-app` | `subscription` + `resource_group`, optionally `app_config` |
| `kubernetes` | `helm` | `context` + `namespace` |

On AWS the `account` is a guard rather than an address: the account is implicit
in the credentials, so the tool compares it against `sts:GetCallerIdentity` and
refuses on a mismatch. Everything else in the file is reviewable; without this,
where it points would not be.

## Two ways to use it

### Image only

Leave `env` and `envFrom` out, and the tool sets the image tag and touches
nothing else. Every environment variable stays exactly as Terraform left it.
This is the whole file:

```yaml
cloud:
  provider: azure
  subscription: bbbf237a-8c9e-492a-b6a3-9b0bd4869690
  resource_group: evolve-tst

services:
  purchase:
    version: 27ec167
    targets:
      - { type: container-app, name: evolve-tst-purchase }
```

### Environment under deploy control

Declare `env`, and the tool owns the whole environment for that service:
anything not listed is removed. That is the point — it is what makes removing a
variable possible — but it means listing all of them, not just the interesting
ones.

```yaml
services:
  purchase:
    version: 27ec167
    env:
      LOG_LEVEL:         info
      CTP_API_URL:       https://api.europe-west1.gcp.commercetools.com
      CTP_CLIENT_SECRET: ${secret:purchase-server-token-commercetools}
    targets:
      - { type: container-app, name: evolve-tst-purchase }
```

`apply` refuses when it would delete a variable the config does not mention,
which is what stops "I added one variable" from becoming "I deleted the other
thirty-one, secrets included":

```console
$ evolve-deploy apply deploy/tst.yaml
error: this would delete 3 environment variable(s):
  - container-app/evolve-tst-purchase: API_EXTENSION_SECRET
  - container-app/evolve-tst-purchase: OTEL_EXPORTER_OTLP_HEADERS
  - container-app/evolve-tst-purchase: SENTRY_DSN

Declaring `env:` for a service means owning all of it, so anything not listed
there is removed. If the variables above come from Terraform, add them to the
config or leave `env:` out entirely. Pass --allow-env-removal if you meant it.
```

## References

Values are either literals or references. **The config never contains a secret.**

```yaml
env:
  LOG_LEVEL:         info                            # literal
  REDIS_URL:         ${param:/platform/redis-url}    # configuration store
  CTP_CLIENT_SECRET: ${secret:purchase-ctp-secret}   # secret store
```

`${env}` expands to the environment name, so a reference can be written once for
every environment: `${param:/evolve/${env}/purchase/setup}`.

A reference is handed to the platform where it can resolve one, and read by the
tool only where it cannot:

| Target | `${secret:}` | `${param:}` |
|---|---|---|
| `ecs` | `valueFrom` | `valueFrom` |
| `cloud-run` | `secretKeyRef` | read from Secret Manager |
| `container-app`, `container-app-job` | `secretRef` | read from App Configuration |
| `lambda` | read by the tool | read by the tool |
| `function-app` | not managed | not managed |

Lambda is the exception because its environment variables are literal strings
with no reference mechanism at all. A project that will not accept any value
passing through CI can say so, at the cost of not being able to deploy Lambda
targets that carry references:

```yaml
refs:
  resolve: deny
```

**What a `${secret:}` names differs per cloud, and it has to.** On AWS it is a
Secrets Manager ARN or SSM parameter name, which a task definition carries
directly. On Azure a secret must be *declared* on the resource — with a Key
Vault URL and the identity allowed to read it — and referred to by name;
declaring it is Terraform's job, so `${secret:ctp-secret}` means "the secret
named ctp-secret that Terraform declared on this app". A name that is not
declared fails the plan rather than producing a revision that cannot start.

Function apps manage no environment at all: their app settings hold platform
wiring — `AzureWebJobsStorage`, the deployment connection string, Application
Insights — alongside application config, and owning that map would mean
reproducing secrets the platform manages. Declaring `env` on a `function-app`
target is an error rather than a silent no-op.

Deploying one is a **one deploy**, the only technology Flex Consumption
supports: the package is fetched from blob storage and posted to the app's
publish endpoint. Because nothing in the app then records which package it is
running, the tool writes an `EVOLVE_DEPLOY_VERSION` app setting — the same
marker Lambda needs, for the same reason.

```yaml
services:
  purchase:
    version: abc1234
    code:
      url: https://artifacts.blob.core.windows.net/functions/purchase/purchase-sha-{{.version}}.zip
    targets:
      - { type: function-app, name: evolve-tst-purchase-events }
```

Whoever runs this needs **Storage Blob Data Reader** on the artifacts account:
the package is fetched over the data plane, not through ARM, so the roles that
let you manage the storage account are not enough to read from it.

`envFrom` expands a JSON object into the environment — what Terraform writes
with `jsonencode(local.env_vars)`. It must point at a parameter store, never a
secret store, so bulk expansion never means reading a secret. Anything in `env`
wins over it.

## Hooks

Shell commands, run once per service. The tool knows nothing about Hive or
anything else: deploy-time gates belong next to the deploy rather than in
Terraform, but they do not belong *inside* the tool.

```yaml
services:
  purchase:
    version: abc1234
    before:
      - hive schema:check   --service purchase --commit {{.version}}
    after:
      - hive schema:publish --service purchase --commit {{.version}}
    targets:
      - { type: container-app, name: evolve-tst-purchase }
```

`before` is the gate for the whole release. Every `before` hook runs first, all
of them, concurrently; only if every one exits zero does anything get written.
One failure and nothing is deployed at all — a schema check that goes red means
the release is already lost, and rolling the other services out anyway leaves an
environment half a version ahead. You also get every message at once rather than
one broken schema per run.

`after` is per service and runs only once that service's targets all succeeded,
so a release where one service failed still publishes for the ones that worked.
A failure in `after` rolls nothing back — removing a working version because a
registration call failed is worse than the missing registration.

So the order is: all `before` hooks → all deploys → each service's `after`.
Nothing runs when there is nothing to deploy, and a service with no work has
neither hook run.

## Order

Services roll out concurrently by default, which is right until one of them
reads from another. `depends_on` says which way round:

```yaml
services:
  discover:
    version: abc1234
    type: container-app
    name: evolve-tst-discover
  site:
    version: abc1234
    depends_on: [discover, purchase]
    type: container-app
    name: evolve-tst-site
```

`site` now starts only once both backends have finished. Everything else still
runs at once — this is an ordering constraint, not a queue, so each service
waits for exactly what it named and nothing else.

Two things it deliberately does not do. It does not pull a service into a
release: a dependency that is not part of this run — filtered out by `--only`,
or already at its version — is simply satisfied, which is what makes it usable
in CI where the pipeline deploys only what it rebuilt. And it does not deploy a
service whose dependency failed; that one is reported as not deployed rather
than counted as a failure of its own, because nothing was written for it.

A `depends_on` naming a service that is not in the file, or a cycle between
services, fails while reading the config — before anything is deployed.

## Waiting, failure and rollback

The tool waits until a target is healthy: ECS `services-stable`, a Cloud Run
ready condition, a Container Apps revision becoming the ready one. Services roll
out concurrently, so a release takes about as long as its slowest service rather
than the sum of all of them.

That makes the platform's own timings the floor. A readiness probe with
`initialDelaySeconds: 60` means every deploy of that service takes at least a
minute however fast the tool is; `-v` shows exactly where the time goes.

Failures are contained differently depending on when they happen:

- **Anything found while planning stops everything.** A mistyped reference, a
  missing image, a secret that is not declared — nothing is deployed at all.
- **A failing `before` hook stops everything too.** It runs ahead of every
  write, so calling the release off there costs nothing and leaves nothing half
  applied.
- **A failure during rollout stays with its service.** Its targets go back
  together, because they share an image and may have a contract with each other.
  Services that already succeeded are left alone.

A rollout that fails does not wait out the clock. On Container Apps the tool
reads the revision itself, so an image that cannot be pulled or a container that
crash loops fails in seconds with the platform's own message, rather than after
ten minutes of nothing happening. The rollback that follows gets a much shorter
budget than the deploy did: it is restoring containers that were serving a
moment ago, and if that does not come back quickly, waiting will not fix it.

## What it does not do

Cpu, memory, probes, scaling, networking, IAM, load balancers, queues and event
source mappings stay with Terraform. The contract is one sentence:

> I set the image and the environment on the running resource, and leave
> everything else alone.

Your IaC needs one change so it does not roll the image back:

```hcl
# azure
lifecycle { ignore_changes = [template[0].container[0].image] }

# gcp
lifecycle { ignore_changes = [template[0].containers[0].image] }

# aws lambda
lifecycle { ignore_changes = [s3_key] }
```

Add the environment to that list once a service's `env` moves into the deploy
config.

Azure and GCP need nothing else: updates carry only the template — a merge patch
on Azure, a `template` field mask on GCP — so ingress, identity, IAM, traffic
split and declared secrets are untouched. On Azure that is necessity rather than
tidiness: a full write-back would blank every secret, because a read never
returns their values.

ECS is the exception. Image, environment, cpu and probes all live in one
immutable `container_definitions` blob, so field-level `ignore_changes` is
impossible. Instead each owner gets its own task definition family: Terraform
registers the shape into `<name>-base`, nothing points at it, and evolve-deploy
derives the running family from it.

```yaml
services:
  purchase:
    version: abc1234
    type: ecs
    cluster: platform
    base: purchase-base    # defaults to <name>-base
```

A memory change in Terraform then lands on the next deploy, and Terraform can
never roll the image back.

## Development

```sh
task test       # run the tests
task format     # gofmt + go mod tidy, before committing
task lint       # golangci-lint
task build      # a local binary
```

## Status

Exercised against a real Azure subscription: reading, planning, the diff in both
directions, skipping when nothing changed, the full write, concurrency, sidecars
left untouched, environments reproduced exactly, and `--set`.

Azure **function apps** are half proven: planning works against real apps —
deployment mode detected off the resource, package found, settings read — but
nothing has been deployed to one yet, so one deploy, the trigger sync and the
rollback are untested.

Built but never run against a real account: the **AWS** and **GCP** drivers.

Not built: Kubernetes/Helm, and `${version}` in environment values. Anything
unimplemented reports an explicit error rather than silently doing nothing.
