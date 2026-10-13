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

Terraform owns the infrastructure; this owns the version. The contract is one
sentence:

> I set the image and the environment on the running resource, and leave
> everything else alone.

## Documentation

**[deploy.evolve-platform.com](https://deploy.evolve-platform.com)** — install,
the config format, references and secrets, hooks, blue-green across three
clouds, what Terraform has to declare, and the CLI reference.

Worth starting at:

- [Introduction](https://deploy.evolve-platform.com/getting-started/introduction/)
  — what it is, and what it deliberately is not
- [How it works](https://deploy.evolve-platform.com/getting-started/how-it-works/)
  — the three ideas the whole thing rests on
- [Your first deploy](https://deploy.evolve-platform.com/getting-started/quickstart/)
- [GitHub Actions](https://deploy.evolve-platform.com/ci/github-actions/)

## Install

```sh
go install github.com/evolve-platform/evolve-deploy@latest
```

Or take a binary from the [releases](https://github.com/evolve-platform/evolve-deploy/releases).
It needs nothing else installed: no cloud CLI, no Terraform, no runtime. On a
GitHub runner, use
[setup-evolve-deploy](https://github.com/evolve-platform/setup-evolve-deploy).

## Pin runtime configuration on GCP

```yaml
services:
  purchase:
    version: abc1234
    env:
      PARAMETER_MANAGER_PROJECT: my-project
      PARAMETER_MANAGER_PARAMETER: purchase
      PARAMETER_MANAGER_VERSION: ${parameter-version:purchase}
      API_TOKEN: ${secret:purchase-api-token}
    targets:
      - { type: cloud-run, name: purchase }
      - { type: cloud-run-job, name: purchase-sync }
```

`${parameter-version:purchase}` selects the newest Parameter Manager version by
creation time once during planning and writes its concrete ID as a literal. The
same selection applies to `/versions/latest`. This lists metadata across all
pages: the metadata Get API does not resolve the render API's `latest` selector. Use
`purchase/versions/config-a` to select a specific snapshot, or a full
`projects/PROJECT/locations/global/parameters/NAME/versions/VERSION` resource.
Bare names use the configured project. Only global parameters are supported. The app must
pass this ID to its Parameter Manager loader; reading `latest` at boot defeats
pinning. A missing or disabled version fails planning.

GCP `${secret:NAME}` references are also resolved to numeric versions during
planning, without accessing their payloads. Cloud Run receives the secret name
and numeric version separately. `/versions/NUMBER` explicitly selects an older
secret. `${param:...}` keeps its existing meaning: reading a value or `envFrom`
JSON document from Secret Manager, with optional `/versions/NUMBER` support.

Identical selectors are captured once per plan across services, jobs and side
environments. A new plan resolves them again; changing only a version still
produces an environment change when declared in `env`. Metadata resolution works with `refs.resolve:
deny`; it does not read a parameter payload or a secret value. Other clouds
refuse `parameter-version` references. Declare them in `env`, not `envFrom`.

The deploy identity needs `parametermanager.parameterVersions.list` for floating
selectors, `parametermanager.parameterVersions.get` for explicit pins, and
`secretmanager.versions.get` in addition to its existing permissions. The runtime
identity still reads the parameter payload; Parameter Manager resolves its
secret references. Publish those references with numeric versions and retain
both parameter and secret versions for the supported rollback window. Parent
resources, IAM access and upstream credentials must remain valid too.

A traffic rollback reuses the previous revision's stored pins, including on a
cold start. It does not restore job templates. Redeploying an old image with an
unpinned selector chooses the latest configuration again; explicitly select the
old version for a replay. Revisions deployed before pinning retain their old
behavior. Sidecar environments remain infrastructure-owned.

## Development

```sh
task test       # run the tests
task format     # gofmt + go mod tidy, before committing
task lint       # golangci-lint
task build      # a local binary
```

CI runs formatting, `go test -race`, golangci-lint and a full goreleaser build.
Running the four above before pushing means a green run here is a green run
there — `go vet` alone is not enough, since the lint job adds staticcheck and
revive on top of it.

An opt-in live check verifies Parameter Manager lookup using application default
credentials and an existing global parameter. It reads only metadata:

```sh
EVOLVE_GCP_PIN_PROJECT=my-project EVOLVE_GCP_PIN_PARAMETER=purchase \
  go test -tags=integration ./internal/drivers/gcp -run TestPinParameterVersionLive -count=1 -v
```

User-visible changes need a [changie](https://changie.dev) entry in
`.changes/unreleased/`. While a feature is still unreleased, amend its existing
entry rather than adding a second one for the same thing.

The docs site lives in [`docs/`](docs/) and is a separate build:

```sh
cd docs
pnpm install
pnpm dev         # http://localhost:4321
pnpm build       # astro check, build, and a link check over the output
```

Dependencies are held back until they are a fortnight old (`minimumReleaseAge`
in `pnpm-workspace.yaml`), so ranges there are deliberately loose enough to
leave a mature version to resolve to.

## License

[MIT](LICENSE)
