---
title: Hooks
description: before gates the release, after runs per service once its targets succeeded. Three forms, and what a hook prints.
sidebar:
  order: 3
---

Steps that run once per service, before and after its deploy. A hook is either a
command line or one of a small set of [named actions](../actions/).

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

Deploy-time gates belong next to the deploy rather than in Terraform, but they
do not belong *inside* the tool, and it has no business knowing what
`hive schema:publish` is. The command line came first and is still the whole of
the contract.

## `before` gates the whole release

Every `before` hook runs first, all of them, concurrently. Only if every one
exits zero does anything get written.

One failure and **nothing is deployed at all** — not the service that failed,
not any other. A schema check that goes red means the release is already lost,
and rolling the other services out anyway leaves an environment half a version
ahead. You also get every message at once rather than one broken schema per run.

Because it runs ahead of every write, calling the release off there costs
nothing and leaves nothing half applied.

## `after` is per service

`after` runs only once that service's targets have all succeeded, so a release
where one service failed still publishes for the ones that worked.

A failure in `after` **rolls nothing back**. Removing a working version because
a registration call failed is worse than the missing registration.

## The order, in full

1. Plan — everything resolved, nothing written
2. All `before` hooks, every service, concurrently
3. All deploys, concurrently, respecting [`depends_on`](../ordering/)
4. Each service's `after` hooks, once its own targets succeeded

Nothing runs when there is nothing to deploy, and a service with no work has
neither hook run.

For a [blue-green](../../blue-green/overview/) release there is one more step:
`strategy.smoke` runs after everything has staged and before any traffic moves.

## The three forms

The first two are the same thing:

```yaml
after:
  - hive schema:publish --commit {{.version}}       # a command line
  - cmd: hive schema:publish --commit {{.version}}  # the same, written out
  - uses: honeycomb                                 # a named action
    with: { dataset: purchase }
```

Use `cmd:` when you want the list to read consistently next to a `uses:` entry.
A plain string means what it always did.

`strategy.smoke` takes the same three forms.

## What a hook prints

**A hook that succeeds prints nothing.** There are three of them per service on
a normal release and each is a CLI with plenty to say, none of which is the
answer to what was deployed.

**A hook that fails prints everything it printed**, because that is the
diagnosis.

**`--verbose` streams all of it as it happens**, tagged per service, with how
long each hook took.

## Variables

Hooks are rendered as Go templates with what the tool knows about the release:
`{{.version}}`, `{{.name}}`, `{{.env}}`, and for blue-green `{{.label}}` and
`{{.previous_label}}`. `--var` adds your own. See
[Templating](../../configuration/templating/).

Every hook is rendered while planning, so a typo in a variable name cannot fail
a release that already succeeded — and `diff` prints the rendered hooks that
would run.

## Where they run

In the current directory, or wherever `--dir` points. The hook inherits the
environment of the process that ran `evolve-deploy`, which is how an API key
reaches it in CI.
