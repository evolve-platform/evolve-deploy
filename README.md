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
evolve-deploy diff    deploy/tst.yaml              # what would happen, changes nothing
evolve-deploy diff    deploy/prd.yaml --exit-code  # non-zero if production has drifted
evolve-deploy apply   deploy/tst.yaml              # roll it out
evolve-deploy traffic deploy/prd.yaml              # which side is serving
evolve-deploy rollback deploy/prd.yaml            # put the other side back
```

The config path is the only positional argument, and the only thing that decides
which environment is touched. Useful flags:

| | |
|---|---|
| `--set name=version` | override a version without editing the file; how CI deploys a test build |
| `--only a,b` | limit the run to these services |
| `-v`, `--verbose` | log every step with how long it took, and stream what the hooks print, to stderr |
| `--workers N` | services rolled out at once (default 16) |
| `--env NAME` | override what `${env}` and `{{.env}}` expand to (default: the filename) |
| `--var name=value` | pass a value from the pipeline to hooks as `{{.name}}` (repeatable) |

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
nothing else. Every environment variable stays exactly as Terraform left it —
which needs no mode of its own, since a config that sets nothing merges nothing.
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

Declare `env`, and those variables are laid over the ones the target already
carries. Terraform declares the environment; the config refines it. So listing
one variable sets one variable, and the other thirty keep the values Terraform
gave them.

The trade is that the config sets but never removes. Deleting a variable means
deleting it where it is declared, which is Terraform — the next release then
carries that through.

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

A variable can still disappear — when Terraform stops declaring one that a
running target has. `apply` refuses to be the release that carries that through
unless it is confirmed, secrets being among the things that go this way:

```console
$ evolve-deploy apply deploy/tst.yaml
error: this would delete 3 environment variable(s):
  - container-app/evolve-tst-purchase: API_EXTENSION_SECRET
  - container-app/evolve-tst-purchase: OTEL_EXPORTER_OTLP_HEADERS
  - container-app/evolve-tst-purchase: SENTRY_DSN

The deploy config only ever sets variables, so these are gone from what
Terraform declares. Put them back there if that was not intended.
Pass --allow-env-removal if you meant it.
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
wins over it, and both are laid over what the target already carries.

So the environment a target ends up with is four layers, each with the last
word over the one before it: what Terraform declares, then `envFrom`, then
`env`, then a blue-green service's `strategy.env` for the side being staged —
plus `EVOLVE_DEPLOY_SIDE`, which the tool writes itself.

## Hooks

Steps run once per service, before and after its deploy. A hook is either a
command line or one of a small set of named actions.

The command line came first and is still the whole of the contract: deploy-time
gates belong next to the deploy rather than in Terraform, but they do not belong
*inside* the tool, and it has no business knowing what `hive schema:publish` is.

A hook that succeeds prints nothing. There are three of them per service on a
normal release and each is a CLI with plenty to say, none of which is the answer
to what was deployed. A hook that *fails* prints everything it printed, because
that is the diagnosis — and `--verbose` streams all of it as it happens, tagged
per service, with how long each hook took.

```yaml
services:
  purchase:
    version: abc1234
    before:
      - hive schema:check   --service purchase --commit {{.version}}
    after:
      - hive schema:publish --service purchase --commit {{.version}}
      - uses: honeycomb
        with: { dataset: purchase }
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

### Actions

Three forms, and the first two are the same thing:

```yaml
after:
  - hive schema:publish --commit {{.version}}       # a command line
  - cmd: hive schema:publish --commit {{.version}}  # the same, written out
  - uses: honeycomb                                 # a named action
    with: { dataset: purchase }
```

The actions exist for the hooks that were never really commands. A Honeycomb
marker written as curl is six lines of flags, a hand-built JSON body, a header
out of an environment variable and a `|| echo` on the end so a failed annotation
does not fail the release — and every value in it is something the tool already
knows: the version, the service, the environment, the side. So the set stays
small and stays about what a deploy *is* — say a version went out, ask whether
something answers — and `cmd` covers everything else.

An action can also refuse while there is still a plan to refuse. A marker whose
API key is nowhere in the environment fails the plan, with the name of the
variable, rather than turning up as a 401 from an `after` hook on a release that
already succeeded.

`uses: honeycomb` marks the deploy on a dataset. `dataset` is required — `__all__`
marks the whole environment — and everything else has a default.

| Option | | |
|---|---|---|
| `dataset` | the dataset to mark | required |
| `message` | | default `{{.name}} {{.version}}` |
| `type` | groups markers so they share a colour | default `deploy` |
| `url` | what the marker links to, usually the commit | default none |
| `endpoint` | | default `https://api.honeycomb.io`, EU is `https://api.eu1.honeycomb.io` |
| `key_env` | which variable holds the key | default `HONEYCOMB_API_KEY` |

`uses: sentry` registers the release and then the deploy of it — two things
Sentry knows, because the same release is deployed to tst and later to prd, and
only the second call differs.

| Option | | |
|---|---|---|
| `org` | | required |
| `project` / `projects` | one, or several | default `{{.name}}` |
| `version` | what Sentry calls a release | default `{{.version}}` |
| `environment` | | default `{{.env}}` |
| `repository` + `commit` | associates the release with what is in it | default none |
| `endpoint` | | default `https://sentry.io/api/0` |
| `key_env` | | default `SENTRY_AUTH_TOKEN` |

Neither of those two can fail a release. An annotation that did not arrive is
reported and forgiven: an `after` hook runs on a deploy that has already
succeeded, and pulling a working version because a note about it went missing
costs more than the missing note. That is what the `|| echo` on the end of every
curl line was already doing, in one place instead of fourteen.

`uses: http` asks for one url and says whether the answer was the expected one.
It replaces the row of flags this was written as — `--fail --silent --show-error
--max-time --retry --retry-delay --retry-connrefused` — where every one had to be
remembered and leaving off `--fail` meant a 500 walked through the gate.

| Option | | |
|---|---|---|
| `url` | | required |
| `method` | | default `GET` |
| `headers` / `body` | | default none |
| `status` | the one status that counts as healthy | default any 2xx |
| `timeout` | bounds one attempt | default `10s` |
| `retry` | further attempts after the first | default `0` |
| `delay` | between attempts | default `3s` |

Retrying is what makes it usable as a smoke test: a side that has staged is not
always answering the instant staging returns, and the first refused connection is
nearly always that rather than a broken deploy. A failure reports the status and
what the body said, which is where a health route explains itself.

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

## Blue-green

By default a new version starts serving as soon as it is ready. That covers the
crudest failure — a container that never starts never gets traffic — and nothing
else: a container that starts but is broken serves everyone, there is no moment
to check anything, and going back means another pipeline run.

`strategy: blue-green` stages the new version on the side that carries no
traffic, runs a command against it, and switches in one write:

```yaml
strategy:
  type: blue-green
  smoke:
    - uses: http
      with: { url: '{{url "site"}}/healthz', retry: 5, delay: 2s }

services:
  site:
    version: 27ec167
    type: container-app

  # No gate for this one: it stages and switches straight away.
  catalog-commercetools:
    version: 27ec167
    type: container-app
    strategy:
      smoke: []

  # And this one is updated straight, the way everything is by default.
  purchase:
    version: 27ec167
    type: container-app
    strategy:
      type: direct
```

| Field | | |
|---|---|---|
| `type` | `direct` \| `blue-green` | default `direct` |
| `smoke` | hooks run against the staged side | empty = switch straight away |
| `labels` | the two side names | default `[blue, green]` |
| `env` | environment per side | see below |
| `keep_warm` | leave the previous version running after the switch | default `false`, Container Apps only |
| `bake_time` | how long before ECS terminates the old side | default `0`, ECS only |

The file's block is the default and a service overrides it field by field. Lists
and maps are replaced whole, never merged — `smoke: []` has to be sayable without
restating everything else.

### Addressing the staged side

A smoke test against one staged container is worth something. A smoke test
against the whole staged stack is worth much more, and for that a service has to
reach *its own* side's downstream rather than whatever is live. Tagging the
request is the wrong way round: a header only survives if the router, the reverse
proxy and every sidecar forward it, and one that drops it produces a smoke test
that quietly checks the old version and passes.

The side is a property of the deployment, so it goes in the environment.
`strategy.env` gives each side its own values:

```yaml
services:
  graphql-gateway:
    version: 27ec167
    type: container-app
    strategy:
      env:
        blue:
          HIVE_CDN_ENDPOINT: https://cdn.graphql-hive.com/artifacts/v1/<tst-blue>
          HIVE_CDN_KEY: ${secret:hive-cdn-blue}
        green:
          HIVE_CDN_ENDPOINT: https://cdn.graphql-hive.com/artifacts/v1/<tst-green>
          HIVE_CDN_KEY: ${secret:hive-cdn-green}
```

The green router then reads the green graph, which names the green subgraph URLs,
and the storefront staged on green calls the green router — with nothing to
propagate and nobody to cooperate.

This is **not** the same as declaring `env` on a service. That means the deploy
owns the whole environment; this writes the variables named here over whatever
the staged revision inherited and leaves the rest to Terraform, exactly like
`EVOLVE_DEPLOY_SIDE` itself. References work as they do anywhere else, so the
per-side secret is named rather than carried.

Two properties worth knowing:

- **The values are excluded from the environment diff.** They differ by side by
  definition, so comparing the staged side's against the serving side's would
  report a change on every run — which would deploy on every run and flip the
  sides forever with no version ever changing. The plan prints which variables
  the side sets instead.
- **Every side must name the same variables.** The staged containers are copied
  from the serving revision, so a variable only one side sets does not arrive
  unset — it arrives carrying the other side's value. That is refused while
  reading the config rather than resolved by picking a behaviour for it.

**A release is one release.** Every staged service is staged first, then every
smoke step runs, then all of the traffic moves — not stage-smoke-switch per
service. Which means a service with nothing to change is staged as well, at the
version it already runs: the side is a property of the environment, and a side
missing an app is not a stack you can point a smoke test at. (A revision can
carry only one label, so the serving revision cannot lend the idle one its own.)

Two things follow. `depends_on` between two blue-green services is refused —
staging carries no traffic, and a staged side reaches its own side by label URL
whatever the weights say, so there is nothing left to order. And if nothing in
the release has a real change, nothing is staged at all: a second `apply` is
still a no-op.

**Active is the label with 100% of the traffic.** There is no state and no
marker. If no label has all of it, the tool refuses: a split means someone is in
there by hand or a previous run died, and there is then no active side to deploy
away from. That check runs while planning, so one odd split stops the whole
release before anything is written.

The same holds across the environment: the apps have to agree on which side is
idle, because "green" must mean the same thing everywhere for the staged side to
be a stack. A release where they disagree is refused, naming the
`evolve-deploy traffic <config> --to <label>` that aligns them.

A failed blue-green deploy is a non-event. The staged side never served a
request, so recovery is switching it off:

| Fails | What happens | Does a user notice? |
|---|---|---|
| the staged revision never becomes ready | switched off, traffic untouched | no |
| `smoke` | switched off, traffic untouched | no |
| the switch itself | traffic is where it was | no |
| the cleanup afterwards | a warning; the deploy succeeded | no |

The previous version keeps its label at 0% and that is the rollback target, but
after the switch it is switched off. A Container Apps revision that is not
deactivated holds on to its own scale rules, so with `minReplicas >= 1` the side
nobody is using goes on costing money for as long as the pair stands — and a
version nobody is using should not cost anything. Rolling back starts it again,
which is a container start rather than the one write it would otherwise be.

`keep_warm: true` buys that write back:

```yaml
strategy:
  type: blue-green
  keep_warm: true      # prd: an outage is measured in money, a cold start is not free
```

Set it per file or per service, which is the axis production and test actually
differ on. Either way this is one line rewritten after every successful deploy,
even when it already held, so a run that died halfway is tidied by the next
release — there is no retention setting and no cleanup command.

⚠️ Version N and N-1 are live at the same time against the same database while a
release is being staged and checked. That is the familiar expand/contract
discipline and the tool can see nothing about it. With `keep_warm` they also both
hold replicas between releases, which doubles the compute floor for that app.

There is **no gradual traffic shifting** — no 5% for a minute, then 20%. Both
platforms can express it and this deliberately does not use it: to hold at 5%
and then decide something you need to know the error rate at 5%, and there is no
metrics client here. The smoke test checks the new version deliberately, against
a URL, before anyone reaches it — a better gate than 5% of traffic and an unread
graph.

### Per cloud

Implemented for Azure Container Apps, GCP Cloud Run and AWS ECS. Kubernetes has
no implementation and asking for it there is an explicit "not implemented" while
planning, never a silent direct update.

The choreography is the same everywhere — stage, gate, switch — and so is the
config. What differs is who owns the traffic, and that changes what a rollback
is. It is worth knowing before you pick a cloud to be brave on.

| | Container Apps | Cloud Run | ECS |
|---|---|---|---|
| who moves the traffic | the tool | the tool | ECS |
| the sides | labels, alternating | tags, alternating | roles in one release |
| the staged address | `<app>---<label>.<domain>` | from the API | `test_url`, written down |
| after the switch | previous stopped | previous scales to zero | previous terminated |
| cost of the idle side | zero, or `keep_warm` | zero | zero, or `bake_time` |
| `traffic --to <label>` | yes | yes | no — no side to name |
| `rollback` | any time | any time | until the release finishes |
| `strategy.env` per side | yes | yes | no |

**Azure Container Apps** is the reference. The tool owns `ingress.traffic`, the
sides are labels it writes, and everything above works.

**Cloud Run** is the same model with fewer preconditions — there is no revision
mode to switch on, and the tagged URL comes back from the API rather than being
assembled. Two differences worth knowing. A revision with no traffic scales
itself to zero, so the idle side costs nothing without being switched off; and
`keep_warm` is refused there, because keeping a revision warm is
`scaling.min_instance_count` on the template, which Terraform owns. ⚠️ Note that
a *service*-level minimum does not apply to a tagged revision, which is the one
genuine trap on Cloud Run.

**ECS is the other family.** ECS has a blue/green engine of its own and it is
better than one this tool could build: it owns the target groups, the listener
rules and the shifting, and it can roll back on a CloudWatch alarm. So the tool
does not drive the rollout, it declares it and then answers the gate — a `PAUSE`
lifecycle hook at `POST_TEST_TRAFFIC_SHIFT`, which is test traffic fully on the
new side and production traffic still entirely on the old one. That is exactly
where a smoke test belongs. No Lambda and no appspec: the exit code of a shell
command still decides.

Three consequences, all from ECS owning the sides rather than the tool:

- **`test_url` is required.** The staged side is reached through the test
  listener rule, and a rule is not an address — it may match on a host, a port
  or a header. So it is written down on the target, and a blue-green ECS target
  without one is refused rather than staged with nothing to point a gate at.
- **`strategy.env` per side is refused.** Per-side environment exists so green
  calls green, which needs the two sides to alternate and keep their names. ECS
  swaps its own target groups, so the sides are roles in one release rather than
  two standing environments.
- **`rollback` works on a window, not on a side.** `traffic --to` does not
  apply — there is no side to name — but `rollback` does, and it takes the other
  shape: for as long as the deployment has not finished, the previous version is
  still running and ECS can put the traffic back on it. That covers the
  `bake_time` window after a switch, and a release whose pipeline died while
  paused at the smoke gate. Once ECS has finished, `CLEAN_UP` has terminated the
  old tasks and going back is a deploy of the previous version — which the
  command says, with the line to run, rather than failing.

  So `bake_time` is not just an accounting setting: it is how long `rollback`
  keeps working. Zero means the switch is the end of it.

```yaml
strategy:
  type: blue-green
  bake_time: 10m        # ECS only: the window before the old side is terminated
  smoke: [ 'curl -fsS {{url "site"}}/healthz' ]

services:
  site:
    version: 27ec167
    type: ecs
    cluster: platform
    test_url: https://site-test.internal.example
```

### Which side, and telling anything about it

Hooks get two more variables, named after their role in the release so they mean
the same thing in `before` and in `after`:

| | |
|---|---|
| `{{.label}}` | the side this release is going to |
| `{{.previous_label}}` | the other one: what was serving, and what a rollback returns to |

`smoke` is different: it gates the release rather than a service, so it runs
once and has no single service to take a URL from. It names one instead —
`{{url "site"}}`, and `{{label "site"}}` / `{{revision "site"}}` — as a function
rather than a field, because a template field has to be an identifier and
`{{.catalog-commercetools.url}}` does not even parse. A name that stages nothing
in this release fails the plan rather than resolving to an empty string: a gate
pointed at nothing would pass. On a `direct`
service the side variables are absent rather than empty, so a hook naming one
fails loudly instead of publishing to `tst-`. Every hook is rendered while planning,
so a typo in a variable name cannot fail a release that already succeeded.

The service itself is told too: every blue-green target gets
`EVOLVE_DEPLOY_SIDE=blue|green` in its environment. A request cannot carry the
side — a header only arrives if every hop forwards it — but a service can resolve
its own downstream by its own side with nothing to propagate. It is written and
never compared: the side alternates every release, so diffing it would report a
change on every run.

### `evolve-deploy rollback` and `evolve-deploy traffic`

```sh
evolve-deploy rollback deploy/prd.yaml              # put the other side back
evolve-deploy rollback deploy/prd.yaml --only site  # take one service back
evolve-deploy traffic  deploy/prd.yaml              # where is it now
evolve-deploy traffic  deploy/prd.yaml --to blue    # onto a side by name
```

`rollback` is the one to reach for, and it has two shapes because the platforms
do. Where the tool owns the sides, a release moves every blue-green service to
the same side at once, so going back is that move in reverse: it works out which
side is not serving, checks that every target agrees on the answer and on the
version behind it, prints what it is trading for what, and hands that side
everything. Where the platform owns them — ECS — there is nothing to name, so it
asks the platform to reverse the release it is still running instead. It refuses rather than guesses — a split, a side that has never
served, targets that disagree — and every refusal names what it found and the
command that resolves it. Half an environment on the old version is worse than
either version everywhere — and `--only` is how you say that is what you meant.

`traffic` without `--to` is read-only and answers "what is actually running".
With `--to` it puts one label on 100% and the other on 0 by name, which is the
way out of a split and the way onto a side `rollback` will not pick for you. It
reads the traffic block directly rather than through the checks `apply` uses,
because the state it has to repair is exactly the state those checks refuse to
interpret.

Both start the revision they are about to hand traffic to, and wait for it,
because without `keep_warm` the side being rolled back to is stopped. Both also
switch off what is no longer serving once the traffic has moved — the other half
of the same thing, and without it a rollback would leave the side it came from
running. A failure there is a warning, never a failed rollback: the traffic
moved, which is what was asked for.

That is also the way to clean up an app that has old revisions running with no
label left on them, from before this existed or from a switch made by hand:
point it at the side it is already on.

```sh
evolve-deploy traffic deploy/prd.yaml --to blue   # already on blue: moves nothing, tidies
```

Neither command asks for confirmation — a tool that asks during an outage is a
tool in the way — so they say what they are about to do before doing it.

They move traffic only. Anything published per side — a Hive target, a schema
registry — still describes the version that was serving before, and both commands
say so.

The full design, including how this interacts with a federated GraphQL graph,
is in [specs/blue-green.md](specs/blue-green.md).

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
