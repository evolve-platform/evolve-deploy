---
title: Commands
description: diff, apply, traffic, rollback and version — what each one is for.
sidebar:
  order: 1
---

Five commands. The config path is the only positional argument, and the only
thing that decides which environment is touched.

```sh
evolve-deploy diff     deploy/tst.yaml              # what would happen, changes nothing
evolve-deploy diff     deploy/prd.yaml --exit-code  # non-zero if production has drifted
evolve-deploy apply    deploy/tst.yaml              # roll it out
evolve-deploy traffic  deploy/prd.yaml              # which side is serving
evolve-deploy rollback deploy/prd.yaml              # put the other side back
evolve-deploy version
```

## `diff`

Read-only. Runs exactly the same plan `apply` runs and prints it.

```console
$ evolve-deploy diff deploy/tst.yaml

purchase  27ec167
  container-app/evolve-tst-purchase                  c2a1950 -> 27ec167
    ~ LOG_LEVEL  debug -> info
    + SENTRY_DSN

site  27ec167
  container-app/evolve-tst-site                      5367b03 -> 27ec167

2 services, 2 targets to deploy
```

This is what fills the hole left by not having `terraform plan`. It resolves
every reference, checks every image exists, picks the right container, and shows
which environment variables would change and why — including a rollout caused by
Terraform moving a base task definition rather than by anything in the file.

It also prints the hooks that would run, since they are part of what a deploy
does and are easy to forget.

### `--exit-code`

Exits non-zero when there is anything to apply. Which makes drift a thing a
pipeline can gate on:

```sh
evolve-deploy diff deploy/prd.yaml --exit-code   # "is production what the file says?"
```

See [Workflow recipes](../../ci/recipes/) for a scheduled drift check.

## `apply`

Compares the config against what is running and deploys the difference.

Nothing is touched until the whole plan resolves: every reference is looked up
and every image checked first, so a typo in one service cannot leave half a
release deployed. Then every `before` hook runs, all of them, and only if every
one exits zero does anything get written.

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

Run it again and it does nothing.

The tool waits until each target is actually healthy — ECS `services-stable`, a
Cloud Run ready condition, a Container Apps revision becoming the ready one —
rather than returning as soon as the API accepted the write.

`--allow-env-removal` is the one flag it adds, and it is covered in
[Environment variables](../../configuration/environment/#refusing-to-delete).

## `traffic`

Blue-green only.

Without `--to` it is read-only and answers "what is actually running": per
service, which revision each label points at and what share of the traffic it
gets.

```console
$ evolve-deploy traffic deploy/prd.yaml

site
  blue   00000000-0000  27ec167   100%
  green  00000000-0001  c2a1950     0%
```

With `--to` it puts one label on 100% and the other on 0, by name:

```sh
evolve-deploy traffic deploy/prd.yaml --to blue
```

That is the way out of a split, and the way onto a side that `rollback` will not
pick for you. It reads the traffic block directly rather than going through the
checks `apply` uses, because the state it has to repair is exactly the state
those checks refuse to interpret.

It is also how you tidy an app carrying old unlabelled revisions, from before
this existed or from a switch made by hand — point it at the side it is already
on:

```sh
evolve-deploy traffic deploy/prd.yaml --to blue   # already on blue: moves nothing, tidies
```

## `rollback`

Undoes the last release. Reach for this one rather than `traffic --to`: it does
not need you to know which label to name, and it refuses when the targets do not
agree on the answer.

```sh
evolve-deploy rollback deploy/prd.yaml              # the whole environment
evolve-deploy rollback deploy/prd.yaml --only site  # one service
```

It has two shapes because the platforms do, and it picks the right one from the
config rather than from a flag. Both are covered in [Rollback and
traffic](../../blue-green/rollback/).

## Global flags

Every command takes these.

| Flag | | Default |
|---|---|---|
| `--set name=version` | override a version without editing the file (repeatable) | |
| `--only a,b` | limit the run to these services | all |
| `--var name=value` | pass a value to hooks as `{{.name}}` (repeatable) | |
| `-e`, `--env NAME` | override what `${env}` and `{{.env}}` expand to | the filename |
| `--dir PATH` | working directory for hooks | `.` |
| `--workers N` | how many services to roll out at once | `16` |
| `-v`, `--verbose` | log every step and every poll, with timings, to stderr | off |

`--verbose` is the account of a release: each API call, each hook and each
staged revision, with how long it took, streamed as it happens. A release that
feels slow can then be read rather than guessed at. It goes to stderr, so it
does not get in the way of the output.

The full reference, with every flag and which command takes it, is in [CLI
reference](../../reference/cli/).
