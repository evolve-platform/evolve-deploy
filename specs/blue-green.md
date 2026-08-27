# Blue-green

A spec, not a description. None of this is built yet — including the parts that
say what it deliberately will not do.

## Why

What the tool does today is a direct update: write the image, wait until the
new revision is ready, done. Container Apps and Cloud Run only promote a
revision once its probes pass, so a container that never starts never serves
traffic. That covers the crudest failure and nothing else:

- **A container that starts but is broken still gets traffic.** A probe hitting
  `/health` says "the process is alive", not "checkout works".
- **There is no moment to look.** Between "ready" and "100% of users" there are
  zero seconds and zero requests.
- **Going back means deploying again.** `git revert` plus a pipeline run is
  three to ten minutes with production broken, and it is a cold start of the old
  version.

Blue-green removes all three: the new version runs, warm and reachable, before a
single user reaches it; there is a place in the timeline to check something; and
going back is a label away rather than a deploy away.

It is explicitly **not** the default. Both versions are live for as long as a
release is being staged and checked, and it requires that version N and N-1 can
coexist. That is a decision per environment, not something a deploy tool makes
for you.

## The shape of it

One smoke command, and if it passes the traffic goes over in one write.

```
stage the new version, no traffic  →  run the smoke command  →  100%
```

No percentages, no wait-a-minute-then-20%. That is a deliberate omission and it
is [written up below](#no-gradual-traffic-shifting) — briefly: a canary without
metrics is a stopwatch, and this tool has no metrics.

## The model

One mechanism, and both clouds the tool supports today already have it built in:

> **Two labels on one resource. A label points at a revision, carries a weight,
> and has its own URL.**

| | Container Apps | Cloud Run |
|---|---|---|
| Label | `traffic[].label` | `traffic[].tag` |
| Weight | `traffic[].weight` | `traffic[].percent` |
| URL per label | `https://<app>---<label>.<env-domain>` | `https://<tag>---<service>-<hash>.run.app` |
| Several revisions | needs `activeRevisionsMode: Multiple` | always |

That URL per label is the whole reason this works. A revision with 0% traffic is
reachable through its label — Cloud Run's documentation says so outright, that a
tagged revision stays reachable at 0% allocation — so a smoke test can talk to
the new version with no production traffic anywhere near it. Without that,
"test before switching" would mean testing something that is already live.

We call them `blue` and `green`. Which of the two gets the new version is fixed
nowhere and does not matter: it is whichever one currently has no traffic.

## Which one is active

The question has one answer and it lives in the cloud:

> **Active is the label with 100% of the traffic. If no label has 100%, the tool
> refuses.**

No state, no marker, no assumption about what the previous run did — the same
premise as the rest of the tool. And the refusal is not timidity: an 80/20 split
means someone has been in here by hand, or that a previous run died. There is no
"the active version" then, so there is no idle side to deploy to either.
Carrying on would mean the tool deciding for itself which of the two it may
throw away.

This is a **plan-time check**, so it falls under the all-or-nothing rule: one
service with an odd split and nothing is deployed, for any service.

```
plan failed, nothing was deployed:
  - container-app/evolve-prd-site: traffic is split, so there is no active side:
      blue   evolve-prd-site--a1b2c3   80%   27ec167
      green  evolve-prd-site--d4e5f6   20%   3f9ac21
    resolve it first:
      evolve-deploy traffic deploy/prd.yaml --only site --to blue
```

Exactly what is accepted and what is not:

| Situation | |
|---|---|
| One entry, 100%, with a label | ✅ that is the active side |
| Two entries, 100/0, both labelled | ✅ the normal resting state after a deploy |
| Two entries carrying weight | ❌ split, refuse |
| 100% on an entry with no label | ❌ there is nothing to fall back to |
| `latestRevision: true` / `type: LATEST` | ⚠️ bootstrap, see below |
| `activeRevisionsMode: Single` (Azure) | ❌ labels do not exist there |
| No ingress (a job, an internal worker) | ❌ no traffic to divide |

### Bootstrap

A fresh app is set to "the newest gets everything" — `latestRevision: true` on
Azure, `type: LATEST` on Cloud Run. That is not a valid resting state for
blue-green, but it is not an error either: the label points at exactly one
revision right now, namely the newest one that is ready. The tool converts that
into an explicit reference to that same revision. No change in behaviour, and it
saves a Terraform dance when switching this on.

A traffic entry can name a revision in two ways:

```
{ label: blue, revisionName: "site--a1b2c3", weight: 100 }   ← explicit, fixed
{ label: blue, latestRevision: true,         weight: 100 }   ← "whatever is newest"
```

The second one is not a reference to a revision, it is a *rule*, and that is
where the trap is. The moment STAGE creates a new revision, that revision falls
under the rule and takes 100% of the traffic right there — before the smoke test
exists, before a single weight has been written.

⚠️ **So the order of the writes is the whole story:**

```
1. traffic write    blue → "site--a1b2c3", 100%    pin; replaces latestRevision
2. template write   new revision "site--d4e5f6"    gets 0%, because blue is fixed
3. traffic write    green → "site--d4e5f6", 0%     attach the idle label
4. smoke, switch, settle…
```

Step 1 is functionally a no-op — `a1b2c3` *is* the newest revision — but it
turns the rule into a fact. Do 1 and 2 the other way round and every mechanism
below this is built and none of it does anything.

Cloud Run helps here: once traffic is pinned to a specific revision, every
later deployment keeps that split rather than reverting to latest. So the pin is
written once and stays written. The thing to keep out of any script from then on
is `--to-latest`, which throws the split away again.

The conversion happens once, and it is visible:

```
site  27ec167
  container-app/evolve-prd-site      5367b03 -> 27ec167
                                     blue pinned to evolve-prd-site--a1b2c3 (was: latest)
```

## Config

One block at file level, overridable per service. It is small, and that is the
point.

```yaml
# deploy/prd.yaml
cloud:
  provider: azure
  subscription: bbbf237a-8c9e-492a-b6a3-9b0bd4869690
  resource_group: evolve-prd

strategy:
  type: blue-green
  # One gate for the release. A release has no single address, so a command
  # names what it wants.
  smoke:
    - curl -fsS --retry 5 --retry-delay 2 {{url "purchase"}}/healthz
    - npm run smoke -- --base-url {{url "site"}}

services:
  purchase:
    version: 27ec167
    type: container-app

  site:
    version: 27ec167
    type: container-app
    strategy:
      smoke: []

  # And this one is updated straight, the way everything is today.
  catalog-commercetools:
    version: 27ec167
    type: container-app
    strategy:
      type: direct
```

| Field | | |
|---|---|---|
| `type` | `direct` \| `blue-green` | default `direct` |
| `smoke` | commands run against the idle side | empty = switch straight away |
| `labels` | the two label names | default `[blue, green]` |

A service's block overrides the file's block **field by field**. Lists are
replaced whole, never appended: `smoke: []` means "this service has no smoke
test", and you have to be able to say that without retyping everything else.

`direct` is the default and is today's behaviour: write it, and the traffic
follows as soon as the new revision is ready. It is not called `rolling` because
on Container Apps and Cloud Run nothing rolls — one revision replaces another
whole — and only ECS actually does a rolling update underneath.

`labels` exists only for a repository where Terraform has already bootstrapped
different names. The names do nothing functionally — the tool reads them to know
which two traffic entries are its own, and so it can name them in its output.

Blue-green with no smoke command is still worth having: the new version comes up
warm before it serves anything, and the previous one stays warm behind it. What
you lose is the gate, not the mechanism.

## The choreography

Per target, in this order. Everything above the line is reversible without a
user noticing.

```
0. PIN      bootstrap only: fix the active label onto the revision it currently
            points at implicitly
1. STAGE    create the new revision (image + env), wait until ready, 0% traffic
            — then attach the idle label to it
2. SMOKE    the release's commands, once, over the whole staged side
            exit != 0 → abort
─────────────────────────────────────────────────────────────────────────
3. SWITCH   100% to the idle label, 0% to the other. One write.
4. SETTLE   restore the invariant: two labelled revisions, only the one
            serving still running
```

`before` and `after` stay where they are: `before` is the gate for the whole
release and runs ahead of step 0 of everything, `after` runs once a service has
made it through SETTLE. The smoke test sits exactly in between, which is also
what it is — a gate that cannot exist until there is something running to talk
to.

The switch itself is not a moment of downtime on either platform: Cloud Run lets
in-flight requests finish rather than dropping them when the split changes, and
Container Apps drains the old revision the same way.

Every service runs its own choreography, in parallel with the others. For
services that are not independent `depends_on` already exists, and here it means
the right thing by itself: "done" is now "switched and cleaned up", not "the
revision was created".

## Smoke

The smoke test is a hook like the others: a shell command, exit 0 is approval,
and the tool knows no integration by name. What differs is who it belongs to.

**It gates the release, not a service.** So it lives in the file's `strategy`
block, it is not overridable per service, and it runs exactly once — after
everything has staged and before any traffic moves.

Per target was the first design and it was wrong twice. Practically: the same
suite run once per service runs it fourteen times for one release. And in
principle: the side belongs to the environment, so what is worth checking is a
complete staged side — a request through the staged router reaching the staged
subgraphs — and that belongs to no single service. Testing one app of a side
proves something about that app, not about the release.

**A release has no single URL**, so a command names what it wants:

```yaml
smoke:
  - curl -fsS {{url "purchase"}}/healthz
  - npm run smoke -- --base-url {{url "site"}}
```

`url`, `label` and `revision` each take a service name, or a target label
(`container-app/evolve-prd-site`) for a service that stages more than one side.
Plus `{{.label}}`, `{{.previous_label}}` and `{{.env}}` for the release itself —
a release has one side, so those are unambiguous here.

⚠️ **A function, not a field.** `{{.site.url}}` would read better and it cannot
be offered, because a Go template field has to be an identifier and
`{{.catalog-commercetools.url}}` does not parse — it fails with
`bad character U+002D`, which tells you nothing. Most service names in a deploy
config have a hyphen in them, so that is the common case, not an edge one. One
form that always works beats two where one of them is a trap; the tool
recognises the field mistake and says what to write instead.

A name that stages nothing in this release fails the **plan**, rather than
resolving to an empty string at run time. A gate pointed at nothing would pass,
and a gate that passes when it cannot reach anything is worse than no gate.

Retries belong in the command (`curl --retry`, `npx wait-on`), not in the tool.
A revision has just become ready, so the first call may well return 503 — but
how long you wait for that and how often is exactly the kind of thing a tool
never has the right default for and `curl` has a perfectly good flag for.

A single-revision check has not been lost by moving up a level: it is a line in
the same set, `curl -fsS {{url "purchase"}}/healthz`. What has been gained is
that the end-to-end one is now expressible at all — see
[Knowing your own side](#knowing-your-own-side).

⚠️ The command runs **where the tool runs**, which is CI. The label URL has to
be reachable from there. For an app with internal ingress it is not, and then
`smoke` is unusable — which does not stand in the way of the rest of blue-green:
staging the new version and having a warm previous one both work without it.

## Which side the hooks see

A `before` or `after` hook has to be able to say which side it is deploying to,
because some things downstream are per side. Hive is the case that raised it: a
separate supergraph per side means the variant to publish to is a function of
where this release is going. That example turns out to need more than these two
variables — see [Considerations when using Hive](#considerations-when-using-hive)
— but the variables themselves stand on their own.

Two variables, in `before`, `after` and `smoke` alike:

| | |
|---|---|
| `{{.label}}` | the side this release deploys to |
| `{{.previous_label}}` | the other one: what was serving when the release started, and what a rollback goes back to |

Named after their **role in the release, not after what is live**, because that
is the only naming that stays true from `before` through to `after`. Call it
`active_label` and it would mean one thing before the switch and the opposite
after it.

```yaml
services:
  discover:
    version: 27ec167
    type: container-app
    # Check against what is actually serving right now…
    before: [ hive schema:check   --service discover --target {{.env}}-{{.previous_label}} ]
    # …and publish to the side this release is going to.
    after:  [ hive schema:publish --service discover --target {{.env}}-{{.label}} ]
```

That asymmetry is not a detail. With a supergraph per side, the variant named by
`{{.label}}` currently holds the schema from **two** releases ago — the last time
this side was deployed. So checking against it would check against something
nobody is running. What is live is `{{.previous_label}}`, and that is what a gate
has to be checked against.

Both are known at plan time, before any hook runs, because `Sides` is read
during planning. So `before` gets them just as reliably as `after` does.

Three consequences worth stating.

**Hooks are per service, sides are per target.** So a service whose blue-green
targets are not all on the same side is a plan-time refusal, for the same reason
a service has one version: it releases as a unit, and two targets on opposite
sides means one of them was rolled and the other was not. The error names both.

**On a `direct` service the variables do not exist.** `tmpl.Render` already runs
with `missingkey=error` — a hook that silently became
`--target tst-` would be worse than a failure — so referencing `{{.label}}`
there is an error rather than an empty string.

⚠️ But that error currently surfaces at *apply* time, which is fine for `before`
and wrong for `after`: a typo in a variable name would fail a release that had
already succeeded. So every hook line gets rendered during planning, against the
variables that will be there, and an unknown variable joins the rest of the
plan-time failures. That is a small change to `hooks` and it removes a whole
class of "deployed fine, pipeline red".

**On ECS the identity is the target group.** ECS has no labels, so the first
guess is that there is no stable side there and `{{.label}}` would be a useless
constant. That is wrong. A blue/green service names two target groups —
`targetGroupArn` and `advancedConfiguration.alternateTargetGroupArn` — and the
production listener rule forwards to one of them. After a deployment it forwards
to the *other* one: the roles alternate, exactly like a label moving.

So the mapping needs no convention and no extra config:

| | |
|---|---|
| `blue` | `targetGroupArn`, the primary |
| `green` | `alternateTargetGroupArn` |
| which is serving | whichever one the production listener rule currently forwards to |
| `{{.label}}` | the other one — the side this release stages into |

Two fixed ARNs in the service definition, and one readable pointer that
alternates between them. That is the same shape as "the label with 100% of the
traffic" against a different resource, and it means the hook variables are
portable across all three clouds rather than being an Azure and Cloud Run
feature.

⚠️ One real difference remains: on ECS the non-serving side is *terminated*
after the bake time, so between releases the variant named by `{{.label}}`
describes something that is not running anywhere. The gate still works — a check
against `{{.previous_label}}` is a check against what is live — but "a
supergraph per side" means two live graphs on Container Apps and one live plus
one dormant on ECS.

## Knowing your own side

A smoke command reaching one revision through `{{url "site"}}` reaches exactly
that revision, and nothing more. Go through anything — a GraphQL router, a BFF, a gateway — and that hop
resolves its own downstream addresses, which point at production. The request
lands on the side that is already live, the staged version is never touched, and
the test passes.

The obvious fix is to tag the request and have every hop forward the tag. It is
also the wrong fix: a header only works if the router, the reverse proxy and
every sidecar in the path propagate it, and any one of them dropping it produces
a smoke test that quietly checks the old version and goes green. That is a worse
failure than having no smoke test at all.

The side is a property of the deployment, not of the request. So it belongs in
the environment:

> **Every blue-green target gets `EVOLVE_DEPLOY_SIDE=blue|green` written into its
> environment.**

A service then resolves its own downstream by its own side, with nothing to
propagate and nobody to cooperate.

The side variable alone does not get you there, though: a container environment
is not a shell, so `…/{{.env}}-$EVOLVE_DEPLOY_SIDE` is a literal string rather
than a substitution. Resolving it would mean an entrypoint shim in every image
that needs one. So the *values* are per side as well, in the config:

```yaml
strategy:
  env:
    blue:
      HIVE_CDN_ENDPOINT: …/tst-blue
      HIVE_CDN_KEY: ${secret:hive-cdn-blue}
    green:
      HIVE_CDN_ENDPOINT: …/tst-green
      HIVE_CDN_KEY: ${secret:hive-cdn-green}
```

Additive, like the side variable itself, so a service whose environment Terraform
owns keeps that. Excluded from the diff, because a value that differs by side
would otherwise report a change on every run. And **every side must name the same
variables**: the staged containers are copied from the serving revision, so a
variable only one side sets arrives carrying the other side's value rather than
unset. That is a config error, not a merge.

### Two constraints found while building this

**A revision can carry only one label.** Azure is explicit: "You can only assign
a label to one revision at a time, and a revision can only be assigned one
label." So the idle side cannot be a second label on the revision that is already
serving — occupying it always means a revision of its own, even when the image
did not change.

**Which makes the side a property of the environment, not of a service.** For a
staged side to be a *stack* rather than several unrelated staged apps, every
service needs a revision on that side — including the ones this release does not
change, which therefore get a new revision carrying the same image. Two things
follow, and neither is built yet:

- A release stages every service on the same idle side, so the phases are
  release-wide: stage everything, then smoke, then switch. `applyBlueGreen` does
  them per service today.
- `depends_on` then means nothing between two blue-green services. Ordering the
  staging is pointless because nothing carries traffic, and ordering the switch is
  too, because a staged side addresses its own side by label URL regardless of any
  weight. It should be refused rather than silently ignored — an edge with a
  `direct` service on one end stays meaningful, since there is no other side to
  address there.

The green router fetches the green supergraph, which names the green subgraph
URLs, so it talks to the green subgraphs. Two parallel graphs, no headers — and
an end-to-end smoke test against the router's own label URL then exercises the
entire staged side rather than one container.

There is precedent for the tool writing a variable nobody asked for:
`EVOLVE_DEPLOY_VERSION` already exists on Lambda, for the same kind of reason —
it is information only the deploy has. This one is written on every blue-green
target and is not optional. Opt-in would mean a service that needs it can forget
to declare it, and the failure mode of that is a router silently serving the
wrong graph.

⚠️ **It must be excluded from the environment comparison**, and this is not
cosmetic. The side alternates every release, so the desired value never matches
the value on the revision that is live. Include it in the diff and every plan
reports an environment change, so every run deploys, so the sides flip forever
with no version ever changing. It is written and never compared — the same
treatment `EVOLVE_DEPLOY_VERSION` needs, and the same trap.

## Considerations when using Hive

Everything above treats a service as a thing with an image and an address. A
federated subgraph is also an entry in a composed artifact, and blue-green turns
"which composition" into a question that has to be answered on every release.
This chapter is the answer, and most of it is about a distinction that is easy to
miss.

### A supergraph is a composition, not a pointer

A Hive target holds, per subgraph, both a **schema** and a **routing URL**, and
composes them into one supergraph that the router loads. Two consequences:

- Publishing one subgraph changes the target for every subgraph in it.
- A target that is missing a subgraph does not compose at all. It is not stale,
  it is not a graph.

So a target is only meaningful if every member of the graph has an entry in it,
and those entries describe things that are actually running.

### The instinct that does not work

The obvious move is to give each side its own target and let the deploy pick:

```yaml
after: [ hive schema:publish --service discover --target {{.env}}-{{.label}} ]
```

It fails twice. The first time immediately: `{{.env}}-green` has no entry for any
subgraph that has never been deployed to green, so it does not compose.

The second failure is the one worth understanding. Suppose every subgraph has
been to both sides at least once, so both targets compose. Sides alternate **per
service**, because that is what per-service blue-green means:

| | discover | purchase | live graph is |
|---|---|---|---|
| start | live=blue (S0) | live=blue (P0) | blue |
| release 1: discover → S1 | live=**green** (S1) | live=blue (P0) | ? |
| release 2: purchase → P1 | live=green (S1) | live=**green** (P1) | ? |

After release 1 the sides have desynchronised: discover serves from green,
purchase from blue. There is no longer "the live side", so `{{.env}}-green` names
no set of services that runs together. Release 2 happens to land both on green
and looks fine; release 3 touches discover, sends it to blue, and publishes into
a target where purchase is two generations old.

The reason is one sentence: **the label is a per-service coordinate and a Hive
target is a per-graph artifact.** A per-service coordinate cannot name a
per-graph artifact. It will be right often enough to be trusted and wrong often
enough to hurt.

### Copy, then patch

What works is to build the target rather than accumulate it. At the start of a
release:

1. **Copy** every entry from the live target to the other one.
2. **Patch** the subgraphs this release changes, each with its own staged URL.

Now the target composes, because every member has an entry, and it describes
something real: the changed subgraphs point at their staged revisions, and the
unchanged ones keep the URLs they were copied with — which are where they
actually live. Nothing needs to be deployed to make an unchanged subgraph
present in the target; copying its entry is enough.

That artifact is exactly what production will look like after the switch, which
is what makes an end-to-end smoke test against it worth running.

And keeping two of them is the point rather than an accident: the generation you
are not writing to stays a complete, coherent description of what was running
before. That is the same property as a warm previous revision, one layer up.

### Two things called blue and green

Here is the trap, and it is worth being explicit because both are real and they
are not the same:

| | what it is | where it comes from | when it flips |
|---|---|---|---|
| a service's side | which revision of that service serves | the label with 100% of the traffic, per target | when that service is deployed |
| the graph generation | which of the two targets you are writing | whichever one is not live | once per release |

Trace release 2 above under copy-then-patch: purchase deploys to **green** (its
own idle side), while the live graph is already `green` from release 1 — so the
generation being written is `blue`. The service goes one way and the target the
other, and both are correct.

Which means `{{.label}}` is **not** the variable for a Hive target. What it is
good for here is the staged URL of the service it belongs to, which is what the
patch needs.

### Where each step lives

The copy happens **once per release**; the patch happens once per subgraph. That
maps cleanly onto the split this tool already has, including the rule in
`specs/initial.md` that there are no deploy-level hooks — DB migrations and cache
invalidations stay in the pipeline, and so does this:

```
1. work out the target generation (the one that is not live)   pipeline
2. copy the live target's entries onto it                      pipeline
3. evolve-deploy apply deploy/tst.yaml                         after hooks patch
                                                               their own subgraph
4. point the router at the new generation                      see below
```

Step 3 needs one thing the tool does not have: a way to get a value from the
pipeline into a hook. `{{.env}}` and `{{.label}}` come from the tool; the
generation comes from outside it.

```sh
evolve-deploy apply deploy/tst.yaml --var graph=evolve-tst-blue
```

```yaml
after:
  - hive schema:publish --service discover
      --target {{.graph}}
      --url https://evolve-{{.env}}-discover---{{.label}}.<domain>
```

One generic `--var name=value`, alongside the `--set` that already exists. The
tool learns nothing about Hive, which is the whole point of hooks being shell
commands.

### Pointing the router at a generation

The router loads its supergraph by target, so the target is part of the router's
configuration — and it changes every release. That composes with what already
exists, without a new mechanism: put the generation in the parameter store and
reference it.

```yaml
services:
  router:
    version: 27ec167
    type: container-app
    depends_on: [discover, purchase, catalog-commercetools]
    env:
      HIVE_TARGET: ${param:/evolve/${env}/graph-target}
```

The pipeline writes that parameter as part of step 1. The plan then resolves it,
sees a value that differs from what the running router has, and reports
`~ HIVE_TARGET` — so the router gets a new revision carrying the new generation,
through the ordinary environment-changed path. No extra flag and no special
casing.

⚠️ `depends_on` is not optional here. Every subgraph has to have patched the new
generation before the router stages against it, or the router loads a graph that
is half-copied. "Done" already means switched, settled and `after` completed, so
the ordering is exactly right — it just has to be written down.

### A single-service rollback is not complete

This is the sharp edge, and it is worth knowing before rather than during.

`evolve-deploy traffic --only discover --to blue` puts discover's traffic back in
one write. It does nothing to Hive. So the live generation still advertises
discover's *new* schema while discover serves the *old* one, and the router will
happily route queries for fields that no longer exist.

Two ways out, both of them the pipeline's:

- **Republish** that subgraph's previous schema into the live generation. Precise,
  and it needs the previous schema to be fetchable.
- **Point the router back at the previous generation.** One action, but it rolls
  back every subgraph's schema view while only one service's code went back — so
  it is only correct if the release contained one subgraph.

Neither is something the tool should do on its own: it does not know what a
subgraph is, and inventing a Hive call inside a rollback path is exactly the kind
of integration knowledge this design keeps out. What it should do is **say so** —
a `traffic --to` on a service the config marks as part of a graph deserves a loud
line saying the schema was not rolled back with it.

That also means the honest granularity of rollback for a federated graph is the
generation, not the service. Which is a real argument for keeping releases that
touch subgraphs small.

### What to check before building this

- **Whether Hive can copy a target.** The whole model rests on step 2 being one
  cheap operation. If there is no copy, the pipeline has to fetch each subgraph's
  SDL from the live target and republish it, which is N calls and needs the SDL
  to be retrievable. Scriptable either way, but it decides whether this is five
  lines of pipeline or fifty.
- **Whether a target may hold a different URL for a subgraph than the other
  target does.** Copy-then-patch assumes yes — that targets are independent in
  their routing URLs, not just in their schemas. If URLs are a property of the
  subgraph rather than of the entry, per-side addressing is off the table and
  only the schema half of this chapter survives.
- **Whether the router can be a second instance cheaply.** An end-to-end smoke
  test needs something loading the staged generation. If that is the production
  router, there is nothing to test against before the switch; if it is a small
  internal one, it is a service in the config like any other.
- **What Hive charges for.** Two targets and a copy per release is a different
  usage pattern from one target and a publish per release. Worth knowing before
  it is in every pipeline.


## Cleaning up

After a successful deploy one invariant holds, and that is the entire cleanup:

> **Two revisions carry a label, and only the one serving is running.**

Concretely the traffic block is rewritten to exactly two entries:

```
{ revision: <new>,      label: green, weight: 100 }
{ revision: <previous>, label: blue,  weight: 0   }
```

and every revision that is not the one serving is deactivated — including the
previous one. On Container Apps that scales it to zero and it stops costing
anything; on Cloud Run a revision with no traffic already stops itself.

The label is the rollback target and it stays named. The replicas are not, and
the two are separable: a revision can be deactivated and activated again with
its whole template intact, so nothing about the rollback is lost except its
speed. That is the trade, and it is the right way round by default — a version
nobody is using should not cost anything, and the case where the cold start is
the more expensive half is the exception rather than the rule.

⚠️ This is a **revision of the original design**, which kept the previous
version running so a rollback was one write and no container start. What broke
it is the scale rule: a Container Apps revision that is not deactivated holds on
to its own `minReplicas`, and there is no per-revision update call to lower it —
`ContainerAppsRevisionsClient` has Activate, Deactivate, Get, List and Restart
and nothing else. So on an app with `minReplicas >= 1` "the previous version
stays running" and "the previous version costs nothing" cannot both be true, and
deactivation is the only lever that produces the second. See
[How warm is warm](#how-warm-is-warm).

`strategy.keep_warm: true` restores the original behaviour per file or per
service. It is one of the few knobs in this tool and it earns its place: the
environment where an outage is measured in money is exactly the environment
where paying for an idle side is fine, and the four where it is not are exactly
the ones where a cold rollback is fine. That is a real axis, not a preference.

The label still moves one deploy later than you would expect, and deliberately
so:

```
after deploy 1    blue  → rev-a  100%   ← serving
                  green → –

after deploy 2    blue  → rev-a    0%   ← this is the rollback, stopped
                  green → rev-b  100%   ← serving

after deploy 3    blue  → rev-c  100%   ← serving, the label moved to a new revision
                  green → rev-b    0%   ← the rollback now, stopped
                  rev-a: no label left  → stays deactivated
```

And it is not a cleanup *step* with a retention setting, it is one line
rewritten after **every** successful deploy, even when it already held. So
revisions left behind by a run that died halfway disappear at the next release,
and there is no `--cleanup` flag and no `keep_revisions: 2` knob — the same
idempotence property as the rest of the tool.

### Coming back

A rollback therefore starts a container before it moves a weight, and both
commands that move traffic do it: activate the revision, wait for it to run,
then write. Handing traffic to a deactivated revision would be a label that
resolves and answers nothing, which is the same failure the staging path already
guards against.

`evolve-deploy rollback <config>` is the one to reach for. A release moves every
blue-green service to the same side at once, so undoing it is that move in
reverse and there is nothing to type: it reads which side is not serving, checks
that every target agrees on the answer *and* on the version behind it, and hands
that side everything. Targets that disagree mean a release died between two of
them, and finishing that job in the other direction is not what anyone typed the
command for — so it refuses, prints the moves it worked out, and names
`traffic --to` as the way to resolve it by hand.

`evolve-deploy traffic --to <label>` stays the by-name version: the way out of a
split, and the way onto a side that `rollback` will not pick on its own.

Both then restore the same invariant a deploy does, which is `Tidy`: switch off
every revision that is not serving. `Settle` can name the revision to keep
because it just created it; `Tidy` has no release to ask, so it reads the
traffic block and keeps whatever holds all of it. A split is refused rather than
resolved — switching off the wrong half of one is an outage, not a cleanup.

Without it the accounting only works one way round. `Point` starts what it is
about to serve; nothing would ever stop what it stopped serving, so every manual
switch would leave a side running and the saving would last exactly until the
first rollback. It is also the answer for an app carrying revisions from before
any of this, with no label left on them: `--to` the side it is already on moves
no traffic and tidies.

⚠️ A SETTLE that fails is a **warning, not a failure**. The traffic is on the
new version, the deploy worked; a `deactivateRevision` returning 500 must not
produce a red pipeline. That is the same trade-off as an `after` hook that
fails: removing a working version over a cleanup action is worse than the
problem. It costs money until the next run, and the output says so loudly.

### How warm is warm

"The previous version is still running" was the original claim. It is now
`keep_warm`, off by default, and it needs one qualification per cloud — because
it is the difference between a rollback that is instant and one that is a cold
start.

⚠️ On Cloud Run, **a service-level minimum-instances setting does not apply to a
tagged revision**. So the previous version at 0% traffic can be scaled to zero
despite `min_instances` being set on the service, and rolling back to it means
waiting for a cold start. Getting the warm behaviour requires minimum instances
at the *revision* level, which is a Terraform setting on the template.

On Container Apps a revision that is not deactivated keeps its own scale rules,
so `minReplicas >= 1` gives a warm previous version — and is also exactly what
makes it cost double between releases. `Scale` is part of the template and is
therefore fixed when the revision is created; there is no call that lowers a
running revision's `minReplicas` afterwards. So the choice is binary — running
and paid for, or deactivated and free — and that is what `keep_warm` selects
between. It is off by default, so the answer to "how warm is warm" is: not warm,
unless you said so.

## When it goes wrong

The rollback boundary stays the service — targets of one service share an image
and possibly a contract, so they move together or not at all. What follows from
that, per failure point:

| Fails | What happens | Does a user notice? |
|---|---|---|
| `before` hook | nothing written, whole release stops | no |
| plan checks (split, `Single` mode) | nothing written, whole release stops | no |
| STAGE: revision never becomes ready | revision deactivated, traffic untouched | no |
| SMOKE | revision deactivated, traffic untouched | no |
| SWITCH: the write fails | traffic is where it was; revision deactivated | no |
| a sibling target of the same service | `Revert`: traffic back, revision gone | briefly, if it had already switched |
| SETTLE | warning, deploy succeeded; the old side is still running and still costs money until the next release | no |
| `after` hook | pipeline fails, no rollback (existing rule) | no |
| tool is killed between SWITCH and SETTLE | the new version serves, cleanup is pending | no |

The first four rows are the point of the whole exercise: **a failed blue-green
deploy is a non-event.** Nothing happened that anyone saw, and recovery is
deactivating a revision that never served a request. That is a fundamentally
different kind of pipeline failure from "production was broken for two minutes".

Dropping the gradual shift also removed the one genuinely awkward failure state.
There is no longer a window in which the tool can die halfway through a split,
so the only untidy outcome left is a pending cleanup, which the next run fixes
by itself.

## `evolve-deploy traffic`

```sh
evolve-deploy traffic deploy/prd.yaml                          # where is it now
evolve-deploy traffic deploy/prd.yaml --only site --to blue    # back, in one write
evolve-deploy traffic deploy/prd.yaml --to blue                # everything back
```

Without `--to` it is read-only, and it is the answer to "what is actually
running on production":

```
site        container-app/evolve-prd-site
  blue      evolve-prd-site--a1b2c3    27ec167     0%
  green     evolve-prd-site--d4e5f6    3f9ac21   100%  ← serving

purchase    container-app/evolve-prd-purchase
  blue      evolve-prd-purchase--f0a1   27ec167  100%  ← serving
  green     (none)
```

With `--to` it puts one label on 100% and the other on 0, in one write, waiting
for nothing. That is two things at once: the manual rollback for a release that
went green but still was not right, and the way out of a split someone made by
hand.

No confirmation prompt. A tool that asks for confirmation during an outage is a
tool in the way; what it does do is print what it is about to do before it does
it. Without `--only` it covers every service in the file — "put everything back
to the previous version" is exactly what you mean at that moment.

## `--var name=value`

One flag, added because the Hive chapter needs it and nothing else provides it:
a value the pipeline knows and the tool does not, made available to hooks as
`{{.name}}`.

```sh
evolve-deploy apply deploy/tst.yaml --var graph=evolve-tst-blue
```

It is deliberately generic. The graph generation is the case that forced it, but
"the pipeline knows something the deploy config cannot" is not a Hive problem —
a build number, a change ticket, a release name all have the same shape. Naming
the flag after any of them would have been the mistake.

Repeatable, like `--set`. A name that collides with one the tool provides
(`version`, `name`, `env`, `label`, `previous_label`) is an error rather than an
override: a hook that silently got a different `{{.version}}` than the one being
deployed is not a thing worth being able to do.

## Where "the current version" comes from

This is the subtlety that separates working from "the second deploy does
nothing".

The drivers today read the **template of the resource** to determine what is
running: `app.Properties.Template.Containers` on Azure, `svc.GetTemplate()` on
Cloud Run. In single-revision mode that is correct — the template *is* what
runs. With two live revisions the template is the last one created, and after a
failed deploy that is precisely the revision that was thrown away.

What that gets you if left alone: a deploy fails its smoke test, someone fixes
the code, deploys again at the same version → the tool compares against the
broken green template, sees the same tag, and concludes there is nothing to do.

So, for a blue-green target:

> **`from` and the diff come from the revision the active label points at, not
> from the template of the resource. And the new template is derived from there
> too.**

That second half matters just as much: build the new revision on the resource's
template and the environment of a failed attempt leaks into the next release.
Both clouds return the template of a specific revision
(`RevisionProperties.Template` on Azure, `GetRevision` on Cloud Run), so it is
one extra read and not a new problem.

## Per cloud

### Azure Container Apps

The reference target; this gets built first.

- `activeRevisionsMode` has to be `Multiple`. That is a Terraform setting and
  the tool refuses on `Single` — labels do not exist there and there is nothing
  to divide.
- STAGE is the merge patch that already exists. In `Multiple` mode a new
  revision gets no traffic, so staging is the current behaviour with a different
  resource setting. That is the pleasant part of this choice: STAGE is already
  built and already tested.
- Attaching the label and writing the weights are patches on
  `configuration.ingress.traffic`. Merge patches as well, for the usual reason:
  a read never returns secret values.
- Waiting for readiness stays `waitReady` + `inspectRevision`, unchanged.
- Cleanup is `ContainerAppsRevisionsClient.DeactivateRevision`.
- The label URL is constructed from `ingress.fqdn`: `<app>.<domain>` becomes
  `<app>---<label>.<domain>`. See [still to check](#still-to-check).

### GCP Cloud Run

The same model with fewer preconditions — Cloud Run always allows several
revisions with a split, there is no mode to switch on.

- The field mask today is `template`, which leaves traffic alone. That is exactly
  right: STAGE is the existing write, and the traffic writes are the same call
  with mask `traffic`. Two writes, no new API. In `gcloud` terms the same thing
  is `--no-traffic --tag green`.
- `traffic[].tag` is the label, `percent` the weight,
  `TRAFFIC_TARGET_ALLOCATION_TYPE_REVISION` the explicit reference.
- The tagged URL comes from `traffic_statuses[].uri` — read from the API, not
  glued together.
- The etag protection now on the template write belongs on the traffic writes
  too: two people rolling one service at the same time must not have a silent
  winner.
- Cleanup is only trimming the traffic block back to two entries; a revision
  without traffic scales itself to zero and there is nothing to deactivate. Note
  that a tagged revision at 0% may still be *deleted* — Cloud Run allows that —
  but there is no reason to, and keeping it is the rollback.
- ⚠️ Minimum instances: see [How warm is warm](#how-warm-is-warm). This is the
  one place where the Cloud Run behaviour is a genuine trap.

**Built.** Two decisions the design did not settle, settled by building it:

- **`keep_warm` is refused here.** Everywhere else it is a choice between paying
  for an idle side and paying for a cold start. On Cloud Run there is nothing to
  choose: a revision without traffic scales itself to zero whatever the tool
  does, and keeping one warm is `scaling.min_instance_count` on the template,
  which Terraform owns and the tool never writes. Accepting the field would be
  promising something the driver cannot deliver, so it says so instead — and
  names the service-level trap in the same breath.
- **`Settle` is a no-op and `Tidy` trims the block.** There is nothing to
  deactivate, so the only state worth tidying is the traffic block itself: a
  stray tag from a run that died halfway is a URL that answers, pointing at a
  version nobody chose.

### AWS ECS

ECS has a blue/green engine of its own (`deploymentController: ECS`,
`deploymentConfiguration.strategy`), and it is a better one than this tool could
build: it owns the target groups, the listener rules and the traffic shifting,
and it can roll back on a CloudWatch alarm. So on ECS the tool does not drive
the rollout. It **declares** it, and then supervises.

That sounds like a different feature and it is not, because of one API:

> **A `PAUSE` lifecycle hook stops the deployment and waits for
> `ContinueServiceDeployment`, whose action is `CONTINUE` or `ROLLBACK`.**

That is our smoke gate, natively. The tool puts a pause hook on
`POST_TEST_TRAFFIC_SHIFT` — test traffic fully on green, production traffic
still entirely on blue — polls `DescribeServiceDeployments` until it is paused,
runs the same shell commands from `smoke:`, and answers `CONTINUE` or
`ROLLBACK`. No Lambda, no appspec, and the exit code of a shell command still
decides. A pause hook may wait up to 14 days, so nothing here is racing a
timeout.

The rest maps onto `strategy: BLUE_GREEN`, which shifts production traffic
all-at-once — the same thing this spec does everywhere else. (`type: direct` is
`strategy: ROLLING`, which is what the driver already gets today by setting
nothing at all.) Dropping the
gradual shift made ECS *more* uniform, not less: `LINEAR` and `CANARY` exist and
are now simply not used.

`bakeTimeInMinutes` is required for `BLUE_GREEN` and is the one field with no
counterpart elsewhere, because ECS terminates the old version itself. On
Container Apps and Cloud Run the previous version stays until the next deploy;
on ECS `CLEAN_UP` scales blue to zero once the bake time is up, so that value
*is* the rollback window. The driver sets a default; see [Open](#open).

Two more differences, both of them ECS being fine rather than the tool being
worse:

- **The refusal is a different read.** "Is someone already halfway through this"
  becomes "is there a service deployment in progress or paused", which is one
  `ListServiceDeployments` call. The other half — which side is live — comes off
  the production listener rule; see
  [Which side the hooks see](#which-side-the-hooks-see).
- **There is no `Settle`.** `CLEAN_UP` is ECS's, and the invariant this spec
  maintains by hand elsewhere is maintained by the platform here.

And the one thing this document says the tool will never do, ECS does natively:
`deploymentConfiguration.alarms` rolls back on a CloudWatch alarm during the
shift. That belongs in Terraform next to the alarms themselves, and it composes
with everything above.

The Terraform side is bigger here than elsewhere, because routing is real
infrastructure: two target groups, a production listener rule, a test listener
rule and an IAM role, all named in `loadBalancers[].advancedConfiguration`. The
tool reads that off the service and refuses when it is missing — it never writes
it. `deploymentController` has to be `ECS`; the older `CODE_DEPLOY` controller
is a different mechanism with an appspec, and there is no reason to support
both.

**Built — and not as a second interface.** This chapter proposed a `Delegated`
interface beside `Rollout`, on the grounds that ECS is a different family. It
is, but the two interfaces turned out to have the same shape: `Configure` plus
`Await` *is* `Stage`, `Answer(true)` is `Switch`, `Answer(false)` is `Abandon`,
and `Settle` is the platform's. So ECS implements `Rollout`, `plan.Apply` did
not change, and the choreography really is one piece of code. What the delegated
family costs is not a second orchestrator; it is four honest refusals.

- **`Point` refuses and `Undo` takes its place.** `Pointable` on the interface
  keeps `traffic --to` from offering a side that cannot be named, and a separate
  optional `Undoer` gives `rollback` the shape that is true here. `Routable` and
  `Pointable` are not the same question: a release does move an ECS service's
  traffic, and nothing outside a release can name a side to move it to.

  But "cannot be pointed at" is not "cannot be reversed", and the first draft of
  this conflated them. For as long as the deployment has not finished the
  previous version is still up, and `StopServiceDeployment` with
  `StopType: ROLLBACK` puts the traffic back on it. That covers the bake window
  after a switch and a release whose pipeline died at the gate — the second of
  which nothing else would ever clean up. Once ECS has finished, `CLEAN_UP` has
  terminated the old tasks and `target.ErrNoWindow` turns into advice rather
  than a failure: going back is a deploy.

  Which is what makes `bake_time` more than an accounting setting. It is how
  long `rollback` keeps working, and that is the honest way to describe it to
  someone choosing a value.
- **`test_url` is required on the target.** A listener rule is not an address —
  it may match on a host, a port or a header, and the [still to
  check](#still-to-check) entry about this resolves to "the tool cannot derive
  it". So it is written down, and a blue-green ECS target without one is refused
  rather than staged with nothing to point a gate at. That is the header option
  from that entry declined: one cloud-specific variable to carry one platform's
  routing quirk is a poor trade, and a field the config already has room for is
  not.
- **`strategy.env` per side refuses.** Per-side environment needs the sides to
  alternate and keep their names. They do not here, so the sides are roles in
  one release — the first label is whatever serves, the second is whatever this
  release brings up, every time. `{{.label}}` and `{{.previous_label}}` still
  mean exactly what [Which side the hooks see](#which-side-the-hooks-see) says,
  because those were named after their role in the release and not after what is
  live. That naming was the right call for a reason nobody had in mind at the
  time.
- **`keep_warm` refuses and points at `bake_time`.** They are the same question
  for different platforms: one is a standing state, the other a window. A config
  that sets both has not decided which platform it is talking about, and that is
  a validation error rather than a precedence rule.

`bakeTimeInMinutes` defaults to zero, which is the [Open](#open) question
answered the way this whole feature went: the old side stops costing money as
soon as the new one serves, unless someone asks for otherwise.

### Kubernetes

There, the chart *is* the config, and routing is part of it. Blue-green means
two Deployments with a Service selector that flips, or weights on an
`HTTPRoute`. The tool does `helm upgrade --set image.tag=…` and does not sit
between the routing. So: **the chart owns this, not the tool.** Once Kubernetes
exists in this tool the question is which values a chart has to offer, not which
API the tool has to call.

### Azure Function Apps

Function Apps have deployment slots, and a swap is exactly this: stage into a
slot, check it, swap. That is native blue-green with a different vocabulary.
Function Apps are not built yet, so this is a note: if they arrive, this is the
mechanism, and `strategy` should fit on top of it without a second shape in the
config.

### Jobs and workers

A `container-app-job` has no ingress and therefore no traffic. But it shares its
image with the service next to it, and that is the interesting question:
`discover` is one app plus four jobs on the same tag. If green is not serving
yet, which image does the 03:00 cron run?

> **Jobs move at the switch.** Their template is written in step 3, together
> with the traffic.

Every other choice produces a mixed state nobody intended. Writing earlier means
the new job code talks to the old API; not writing at all means half the release
does not happen. A job that cannot be written fails the service, and the traffic
goes back with it — the rollback boundary is the service, and that stays true.

The same holds for a `lambda` next to an `ecs` service, and for any other target
without ingress in a blue-green service.

## The Terraform side

One line more than is already there. Alongside the `ignore_changes` on the
image:

```hcl
# azure
resource "azurerm_container_app" "this" {
  revision_mode = "Multiple"

  ingress {
    # Bootstrap only: the tool takes this over afterwards.
    traffic_weight {
      label           = "blue"
      latest_revision = true
      percentage      = 100
    }
  }

  lifecycle {
    ignore_changes = [
      template[0].container[0].image,
      ingress[0].traffic_weight,
    ]
  }
}
```

```hcl
# gcp
resource "google_cloud_run_v2_service" "this" {
  template {
    # Leave this out and the idle side costs nothing: a revision with no traffic
    # scales itself to zero, and a rollback is a cold start. That is the default
    # and it is the same trade keep_warm makes on Container Apps — except here
    # it is Terraform's to make, which is why the tool refuses `keep_warm` on
    # Cloud Run rather than pretending it can honour it.
    #
    # ⚠️ If you do want the warm side, it has to be here, on the template. A
    # tagged revision at 0% does not get a *service*-level minimum, so setting
    # it there buys nothing and reads as though it did.
    # scaling { min_instance_count = 1 }
  }

  traffic {
    type    = "TRAFFIC_TARGET_ALLOCATION_TYPE_LATEST"
    percent = 100
    tag     = "blue"
  }

  lifecycle {
    ignore_changes = [
      template[0].containers[0].image,
      traffic,
    ]
  }
}
```

The same figure as with the image: Terraform sets the initial value and then
lets go. From that moment the traffic block belongs to evolve-deploy, which is
also why `latestRevision`/`LATEST` is accepted as a bootstrap — that way the
Terraform side does not have to name a revision it cannot know yet.

⚠️ `ignore_changes` accepts no variables, so this cannot be opt-in in a shared
module — the same problem as with the image
([opentofu#2525](https://github.com/opentofu/opentofu/issues/2525)). In b2b
`app-container` and `app-container-job` are vendored for that reason already;
this adds one line to that same vendored module.

## What it does not do

### No gradual traffic shifting

No 5% for a minute, then 20%. Both platforms can express it and the tool will
not use it, for one reason: **the tool has no metrics, and a canary without
metrics is a stopwatch.**

To hold at 5% and then decide something, you have to know the error rate at 5%.
There is no metrics client in this tool and there will not be one — that way it
becomes half of Argo Rollouts at a tenth of the quality, and the config grows
queries, windows and thresholds. What it could watch is the revision's own
health, which catches a container that crashes under real load and misses every
version of "it answers, with a 500". That is a thin gate for the amount of
machinery and waiting it costs, and it makes every release minutes longer.

What the smoke test does instead is check the new version deliberately, against
a URL, with whatever assertions you feel like writing — before any user reaches
it at all. That is a better gate than 5% of traffic and an unread graph.

The parts of blue-green that survive this are the parts that were doing the
work: staged, warm, checked, and one write to go back.

⚠️ It does mean the switch is all-or-nothing, so a version that only fails under
production load fails for everyone at once. Blue-green does not remove the need
for alerting on the switch — and on ECS, where the platform owns the rollout,
`deploymentConfiguration.alarms` does exactly that natively.

### The rest

- **No compatibility check.** Even with an instant switch, N and N-1 are live
  together during the deploy and for as long as the old side is kept warm. A
  migration that drops a column breaks the version still running next to it.
  That is the familiar expand/contract discipline and the tool can see nothing
  about it — it does not know what a migration is and has no opinion about your
  schema.
- **No state.** Still not. The cloud is the state here too — the label with 100%
  *is* the bookkeeping.
- **No deploy-level blue-green.** There is no "switch every service over at
  once". Services are independent, with `depends_on` for the cases where they
  are not, and one global switch would require the tool to hold every staged
  revision of every service until the slowest is done. That is a second, much
  larger design for a problem nobody has yet.

And the cost: two live revisions with a warm previous side means double the
compute floor for that app, between releases. On tst that is rarely worth it; on
production it is the price of instant rollback.

## How it lands in the code

The choreography is **identical** on Azure and Cloud Run. That is not a
coincidence — they are two implementations of the same labels-and-weights model
— and it is the reason not to put the choice in the driver.

The driver learns five operations, the orchestrator knows the dance:

```go
// Rollout is implemented by drivers that can route traffic by label. A target
// type that cannot is a plan-time error when the config asks for blue-green,
// never a silent direct update.
type Rollout interface {
	// Sides reports the current split: which label serves, which is idle, and
	// what each points at. It fails when there is no single label on 100%.
	Sides(ctx context.Context, t *config.Target) (*Sides, error)

	// Stage creates the new revision, waits until it is ready, and points the
	// idle label at it. Nothing is serving from it yet.
	Stage(ctx context.Context, ch *target.Change) (*Staged, error)

	// Switch hands the staged side 100% of the traffic, in one write.
	Switch(ctx context.Context, ch *target.Change) error

	// Abandon puts the traffic back and deactivates the staged revision.
	Abandon(ctx context.Context, ch *target.Change) error

	// Settle restates the invariant: two labelled revisions, only the one
	// serving still running. A failure here is a warning, not a failed deploy.
	Settle(ctx context.Context, ch *target.Change) error

	// Point puts 100% of the traffic on one label without staging anything,
	// starting the revision first when it is stopped. It is what both rollback
	// and traffic --to are built on.
	Point(ctx context.Context, t *config.Target, label string) error
}
```

`plan.Apply` does the rest: `Stage`, then the smoke hook through the existing
`hooks.Runner` (which already does the shell work, with per-service prefixing),
then `Switch`, then `Settle`. On failure, `Abandon`.

That the smoke test runs there and not in the driver matters: a driver must not
start subprocesses and the hooks runner must know nothing about clouds. `Staged`
carries the URL from the driver to the hook, and that is the only coupling.

`Sides` is called from `Plan`, because that is where the refusals belong. Which
is also why `Change` carries the staged label and the active revision in its
`Payload` — `Apply` then does not have to read the world again, exactly as it
does not today.

**ECS is the other family.** Azure and Cloud Run are *driven*: the tool owns the
traffic, and `Switch` is a write it makes itself. ECS is *delegated*: the
platform owns the traffic, and the tool declares intent and then answers a gate.
Two implementations, not two configs — `strategy` in the file means the same
thing either way, which is the whole reason to write the choreography down
before building any of it.

```go
// Delegated is implemented by drivers whose platform runs the rollout itself.
// The tool translates the strategy into that platform's own configuration, then
// waits at whatever gate it offers and answers it with the same smoke commands
// a driven rollout would have run locally.
type Delegated interface {
	// Configure translates the strategy, and fails when the platform cannot
	// express it — naming the shapes it can.
	Configure(ctx context.Context, ch *target.Change, s *config.Strategy) error

	// Await blocks until the platform is waiting to be told to go on, and
	// reports where the staged version can be reached.
	Await(ctx context.Context, ch *target.Change) (*Staged, error)

	// Answer continues the rollout or rolls it back.
	Answer(ctx context.Context, ch *target.Change, ok bool) error
}
```

Both families produce the same output lines and the same failure modes; what
differs is who writes the weights. And the rest stays either way:
`Driver.Apply` is still the direct path, `Revert` is still the sibling
rollback, and a driver that implements neither interface is not an exception to
handle somewhere — it simply does not get through plan validation.

## What changes in the output

`diff` has to show that this is a blue-green target and which way round it will
go, because that is now a choice and not a given:

```console
$ evolve-deploy diff deploy/prd.yaml

purchase  27ec167
  container-app/evolve-prd-purchase   c2a1950 -> 27ec167
                                      blue-green: stage green, smoke (2), switch
                                      rollback stays on blue c2a1950

site  27ec167
  container-app/evolve-prd-site       5367b03 -> 27ec167
                                      blue-green: stage blue, no smoke, switch
```

And during `apply` every phase is a line, because a tool that is silent for a
minute looks exactly like a tool that has hung:

```console
$ evolve-deploy apply deploy/prd.yaml

purchase  27ec167
  container-app/evolve-prd-purchase   c2a1950 -> 27ec167

  container-app/evolve-prd-purchase   staged green in 1m14s
[purchase] HTTP 200 /healthz
[purchase] 14 passed, 0 failed
  container-app/evolve-prd-purchase   smoke passed in 11s
  container-app/evolve-prd-purchase   green serves 27ec167, blue keeps c2a1950

done in 1m38s
```

That last line is deliberate: after a blue-green deploy the question is not only
"did it work" but also "what do I fall back to", and that is the answer.

## Open

- ~~**ECS bake time.**~~ **Answered**: `strategy.bake_time`, defaulting to zero.
  The knob went in rather than staying a fixed default because it stopped being
  an ECS curiosity — it is the ECS half of the question `keep_warm` asks
  everywhere else, and the two are validated against each other so a config
  cannot set both. Zero by default for the same reason `keep_warm` is off: a
  side nobody is using should not cost anything.
- **Gradual shifting after all.** Written up above as a no. If it ever comes
  back it should arrive with the thing that makes it worth having, which is a
  per-step `check:` command — a shell command that gates each step, so a
  repository with its own metrics CLI sets its own threshold and the tool still
  knows nothing about Prometheus.
- **Blue-green on tst.** Probably pointless — there you want speed and the cost
  is not worth it. But it is the only place to try the choreography out without
  risking a release, so possibly on temporarily while this is being built.
- **Keeping the old side warm for longer than one deploy.** Right now the
  rollback target is exactly one version back, because that is what the
  invariant allows. Two would need a third label and a real question about what
  `--to` then means. Note that "warm" is now `keep_warm` and off by default —
  the *reachable* rollback target is still exactly one version back either way,
  since the label is what makes it reachable and there are two labels.
- **Release groups.** A set of services that stage, verify and switch together
  behind one shared side — environment-level blue-green scoped to a subset. It
  looked necessary for a while, on the assumption that a federated graph could
  only be coherent if its members moved as a unit. Copy-then-patch removes that
  reason: the graph is made coherent in Hive rather than by deploying everything.
  So this is parked, not planned. If it comes back it will be for a different
  reason — a set of services with a runtime contract that a partial release
  genuinely breaks — and that reason should be written down before the feature
  is.

## Still to check

No design decisions, but things that could change the detail:

- **The label FQDN on Container Apps.** We assume
  `<app>---<label>.<env-domain>`, assembled from `ingress.fqdn`. If the API
  returns it somewhere, that beats string construction — Cloud Run does return
  it (`traffic_statuses[].uri`) and then the asymmetry is unnecessary.
- **Whether a deactivated revision may stay in the traffic block** on Container
  Apps, and what happens to a label pointing at a deactivated revision. This is
  no longer a detail — the default cleanup now leaves exactly that state, so the
  rollback pointer depends on it. Verify against a real app before trusting it:

  ```
  az containerapp revision deactivate -g <rg> -n <app> --revision <old>
  az containerapp ingress traffic set -g <rg> -n <app> \
      --revision-weight <new>=100 <old>=0
  ```

  If ARM refuses the second call, the deactivated side cannot keep its label,
  `rollback` and `traffic --to` lose the entry they read, and finding the
  previous revision has to go through `NewListRevisionsPager` instead. That is a
  different design, not a tweak.
- **Whether revision-level minimum instances on Cloud Run behave as the docs
  imply** for a tagged revision at 0%, and what that costs. It is the difference
  between an instant rollback and a cold one.
- **ECS**: whether ECS repoints the *test* listener rule at the incoming side on
  every deployment. It says it "updates the listener rules" plural, but the
  documented example creates the test rule pointing statically at the alternate
  target group — and if it stays static, the test address reaches the wrong side
  on every other release. Decides whether `url` on ECS resolves to a fixed
  address or has to be derived per deployment.
- **ECS**: that the primary/alternate roles really do keep alternating over
  three or more deployments. The documentation states it for the first one
  ("after deployment, forwards traffic to the alternate target group") and the
  rest follows from blue being terminated, but it is an inference — and the whole
  side identity rests on it.
- **ECS with Service Connect** instead of a load balancer. It is the other
  documented way to get managed traffic shifting and it works in DNS names
  inside a namespace, which may be a cheaper route to per-side addresses than
  host-based listener rules. It does not change the lockstep requirement — that
  one comes from the old side being terminated, not from addressing.
- ~~**ECS**: what to do with a header-based test listener rule.~~ **Answered by
  building it**, and neither of the two options in the end. The tool does not
  read the rule at all: `test_url` on the target says where it answers, and a
  blue-green ECS target without one is refused. Which makes the header case
  someone else's problem in the right way — if the rule matches on a header, the
  smoke command carries it, and the tool is not in the middle guessing.
- **ECS**: how to abort a deployment that is already past its pause hook.
  `ContinueServiceDeployment ROLLBACK` only applies while it is paused, and the
  tool should not invent something for the window after that.
