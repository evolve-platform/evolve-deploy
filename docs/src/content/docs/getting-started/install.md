---
title: Install
description: Install evolve-deploy from a release binary, with go install, or on a CI runner.
sidebar:
  order: 3
---

One binary, no dependencies. It needs nothing else installed: no cloud CLI, no
Terraform, no runtime.

## From a release

Binaries are published for Linux and macOS on `amd64` and `arm64`. Take one
from the [releases page](https://github.com/evolve-platform/evolve-deploy/releases),
or fetch it directly:

```sh
VERSION=0.6.0
OS=$(uname -s | tr '[:upper:]' '[:lower:]')      # linux | darwin
ARCH=$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')

curl -fsSL -o evolve-deploy.tar.gz \
  "https://github.com/evolve-platform/evolve-deploy/releases/download/v${VERSION}/evolve-deploy_${VERSION}_${OS}_${ARCH}.tar.gz"

tar -xzf evolve-deploy.tar.gz evolve-deploy
sudo mv evolve-deploy /usr/local/bin/
```

Every release also publishes `evolve-deploy_<version>_SHA256SUMS`. Verifying
against it is worth the two lines — the thing you are downloading then runs with
credentials to a live environment:

```sh
curl -fsSL -O "https://github.com/evolve-platform/evolve-deploy/releases/download/v${VERSION}/evolve-deploy_${VERSION}_SHA256SUMS"
grep " evolve-deploy_${VERSION}_${OS}_${ARCH}.tar.gz\$" "evolve-deploy_${VERSION}_SHA256SUMS" | shasum -a 256 -c -
```

## With Go

```sh
go install github.com/evolve-platform/evolve-deploy@latest
```

A version built this way reports `dev` from `evolve-deploy version`, because the
build stamps come from the release pipeline.

## On a CI runner

Don't script the above. There is an action that does it, verifies the checksum
and puts the binary on `PATH`:

```yaml
- uses: evolve-platform/setup-evolve-deploy@v1
  with:
    version: v0.6.0
```

See [GitHub Actions](../../ci/github-actions/) for the full setup, including
cloud authentication.

## Check it

```console
$ evolve-deploy version
evolve-deploy 0.6.0 (d6af824)
```

## Cloud credentials

The tool authenticates the way every other tool for that cloud does — it reads
the standard credential chain, so whatever already works for the CLI or for
Terraform works here.

| Cloud | What it reads |
|---|---|
| AWS | The default credential chain: environment, shared config, IMDS, or an OIDC web identity token |
| GCP | Application Default Credentials |
| Azure | The default Azure credential chain: environment, workload identity, managed identity, or `az login` |

On AWS the `account` in your config is checked against `sts:GetCallerIdentity`
and a mismatch is refused. The account is implicit in the credentials, so
without that check the one thing in the file that says *where* it points would
not be reviewable.

## Permissions

Beyond the obvious read and update on the resources you deploy to, two are easy
to miss:

- **Azure Function Apps** need **Storage Blob Data Reader** on the artifacts
  storage account. The package is fetched over the data plane, not through ARM,
  so the roles that let you manage the storage account are not enough to read
  from it.
- **Registry read access** is needed wherever the plan checks that an image
  exists — which is every container target.
