---
title: Rollback and traffic
description: rollback is the one to reach for. traffic is the way out of a split, and the way onto a side by name.
sidebar:
  order: 4
---

```sh
evolve-deploy rollback deploy/prd.yaml              # put the other side back
evolve-deploy rollback deploy/prd.yaml --only site  # take one service back
evolve-deploy traffic  deploy/prd.yaml              # where is it now
evolve-deploy traffic  deploy/prd.yaml --to blue    # onto a side by name
```

## `rollback`

The one to reach for. It has two shapes because the platforms do, and it picks
the right one from the config rather than from a flag.

**Where the tool owns the sides** — Container Apps, Cloud Run — a release moves
every blue-green service to the same side at once, so going back is that move in
reverse. It works out which side is not serving, checks that every target agrees
on the answer and on the version behind it, prints what it is trading for what,
and hands that side everything.

**Where the platform owns them** — ECS — there is nothing to name, so it asks
the platform to reverse the release it is still running instead. That works for
as long as ECS has not finished; after that, going back is a deploy of the
previous version, which the command says with the line to run rather than
failing.

### It refuses rather than guesses

Every refusal names what it found and the command that resolves it:

| It finds | Because |
|---|---|
| the traffic is already split | there is no serving side to go back *from* |
| a side that has never served | there is nothing behind it to go back *to* |
| targets that disagree on which side is idle | half an environment on the old version is worse than either version everywhere |
| the two sides do not hold the same version everywhere | same reason |

`--only` is how you say that half an environment is exactly what you meant. It
is deliberately something you have to ask for.

### It starts the side first

The previous version is normally stopped after a switch — it keeps its label,
not its replicas — so `rollback` starts it and waits for it before moving
anything.

That is a container start, not the instant flip you get with
[`keep_warm`](../overview/#the-idle-side-is-switched-off), and it is why the
command prints what it is doing rather than returning silently.

## `traffic`

Without `--to` it is read-only and answers "what is actually running":

```console
$ evolve-deploy traffic deploy/prd.yaml

site        container-app/evolve-prd-site
  blue     evolve-prd-site--blue-0001               27ec167    100%  <- serving
  green    evolve-prd-site--green-0002              c2a1950      0%
```

With `--to` it puts one label on 100% and the other on 0, by name. That is the
way out of a split, and the way onto a side that `rollback` will not pick for
you.

It reads the traffic block **directly** rather than through the checks `apply`
uses, because the state it has to repair is exactly the state those checks
refuse to interpret.

### Tidying

Pointing it at the side it is already on moves nothing and switches off whatever
is no longer serving. That is how you clean up an app carrying old revisions
with no label left on them, from before this existed or from a switch made by
hand:

```sh
evolve-deploy traffic deploy/prd.yaml --to blue   # already on blue: moves nothing, tidies
```

## Both of them

**Both switch off what is no longer serving** once the traffic has moved — the
other half of the same thing, and without it a rollback would leave the side it
came from running. A failure there is a warning, never a failed rollback: the
traffic moved, which is what was asked for.

**Neither asks for confirmation.** A tool that asks during an outage is a tool
in the way. So they say what they are about to do before doing it.

**They move traffic only.** Anything published per side — a Hive target, a
schema registry — still describes the version that was serving before, and both
commands say so:

```
This moved traffic only. Anything published per side — a Hive target,
a schema registry — still describes the version that was serving before.
```

## Rolling back a `direct` release

There are no sides, so there is nothing for `rollback` to move. Revert the
config and apply it:

```sh
git revert <the deploy commit>
evolve-deploy apply deploy/prd.yaml
```

Which is the same review, the same audit trail and the same command as any other
release. That is most of the point of the config being a lockfile in git.
