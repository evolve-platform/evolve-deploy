---
title: CLI reference
description: Every command and every flag.
sidebar:
  order: 1
---

```
evolve-deploy <command> <config> [flags]
```

The config path is the only positional argument, and the only thing that decides
which environment is touched.

## Global flags

Accepted by every command.

| Flag | Default | |
|---|---|---|
| `-e`, `--env NAME` | the config filename without its extension | Override what `${env}` and `{{.env}}` expand to. Changes the substitution only, never which file is read |
| `--dir PATH` | `.` | Working directory for hooks |
| `--only a,b` | all services | Limit the run to these services |
| `--set name=version` | | Override a version without editing the file. Repeatable |
| `--var name=value` | | Pass a value to hooks as `{{.name}}`. Repeatable |
| `--workers N` | `16` | How many services to roll out at once |
| `-v`, `--verbose` | off | Log every step and every poll, with timings, to stderr |

`--var` cannot shadow the names the tool uses itself: `version`, `name`, `env`,
`label`, `previous_label`, `url`, `revision`.

## `evolve-deploy diff <config>`

Show what `apply` would do, without doing it.

Runs the same plan `apply` runs: resolves every reference, checks every image
exists, picks the right container, reads the live state, and prints the
difference — including which environment variables would change and why. Also
prints the hooks that would run.

| Flag | |
|---|---|
| `--exit-code` | Exit non-zero when there is anything to apply |

## `evolve-deploy apply <config>`

Roll out the versions named in the config file.

Nothing is touched until the whole plan resolves, and then until every `before`
hook has exited zero.

| Flag | |
|---|---|
| `--allow-env-removal` | Permit removing environment variables that the config does not declare |

## `evolve-deploy traffic <config>`

Blue-green only. Without `--to`, read-only: prints which revision each label
points at and what share of the traffic it gets.

| Flag | |
|---|---|
| `--to LABEL` | Put 100% of the traffic on this label, in one write |

Reads the traffic block directly rather than through the checks `apply` uses,
because the state it has to repair is exactly the state those checks refuse to
interpret.

## `evolve-deploy rollback <config>`

Move all the traffic back to the side that was serving before. Takes the global
flags only; `--only` narrows it to some of the services.

Refuses when the traffic is split, when a side has never served, when targets
disagree on which side is idle, or when the two sides do not hold the same
version everywhere.

## `evolve-deploy version`

```console
$ evolve-deploy version
evolve-deploy 0.5.0 (60b8822)
```

A build from `go install` reports `dev`, because the stamps come from the
release pipeline.

## Exit codes

| | |
|---|---|
| `0` | Success, or `diff` with nothing to do |
| non-zero | Anything failed — and with `diff --exit-code`, also "there are changes to apply" |

## Environment

The tool authenticates through each cloud's standard credential chain, so
nothing here is specific to it:

| Cloud | |
|---|---|
| AWS | The default credential chain: environment, shared config, IMDS, OIDC web identity |
| GCP | Application Default Credentials |
| Azure | The default Azure credential chain: environment, workload identity, managed identity, `az login` |

Hooks inherit the whole environment of the process that ran `evolve-deploy`,
which is how an API key reaches an [action](../../deploying/actions/):

| | Read by |
|---|---|
| `HONEYCOMB_API_KEY` | `uses: honeycomb`, overridable with `key_env` |
| `SENTRY_AUTH_TOKEN` | `uses: sentry`, overridable with `key_env` |

Written by the tool into deployed targets:

| | |
|---|---|
| `EVOLVE_DEPLOY_SIDE` | `blue` or `green`, on every blue-green target |
| `EVOLVE_DEPLOY_VERSION` | On Lambda and Function App targets, which have nothing else recording which package they run |
