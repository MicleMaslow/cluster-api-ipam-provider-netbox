# Contributing

Issues and pull requests are welcome. For a larger change or a new API field, open an issue first; design changes go to [`docs/proposals`](docs/proposals) in the [Cluster API proposal format](https://github.com/kubernetes-sigs/cluster-api/blob/main/docs/proposals/YYYYMMDD-template.md).

Contributions are licensed under the [Apache License 2.0](LICENSE). Participation is governed by the [Code of Conduct](CODE_OF_CONDUCT.md); report vulnerabilities as described in [SECURITY.md](SECURITY.md).

## Setup

Needs Go (version in `go.mod`), docker, kind, kubectl and jq (and yq for the CAPV cluster). Tools (controller-gen, golangci-lint, kustomize, setup-envtest, clusterctl) are installed by `make` into `hack/tools/bin`.

```bash
make dev-up      # NetBox 4.6.7 on 127.0.0.1:18000 and a kind management cluster with Cluster API
make run         # the provider against both
make smoke       # sample cluster, network, pool and claim
make dev-down
```

`make dev-all` brings up the whole stand, with the provider inside kind and two CAPV clusters on a simulated vCenter. The dev stand writes its kubeconfig to `hack/dev/kubeconfig` and the NetBox token to `hack/dev/.env`; the Makefile uses both. Its passwords, keys and tokens are for local development only. Everything about the stand, a second NetBox version and Tilt: [Dev stand](docs/development/dev-stand.md).

## Make targets

| Target | |
|---|---|
| `make generate` | deepcopy, CRDs and RBAC |
| `make lint` | golangci-lint, and kube-api-linter on `api/` |
| `make verify` | generated files, `go.mod`/`go.sum` and license headers are up to date, and the relative links and anchors of README.md and `docs` resolve (`hack/verify-doc-links.sh`) |
| `make test` | unit tests and CRD validation on envtest, and the shell test of the control-plane VIP command (`hack/test-control-plane-vip.sh`); no NetBox needed |
| `make test-integration` | `go test ./...` with the controller tests against the dev NetBox (`make netbox-up` first); `TEST_LOG=1` prints the provider logs ([Testing](docs/development/testing.md)) |
| `make deploy-dev` | build the image and run the provider inside kind through clusterctl |
| `make release-manifests` | `out/ipam-components.yaml` with the image `IMG`, and `out/metadata.yaml` |
| `make release RELEASE_TAG=vX.Y.Z` | the release manifests and a local clusterctl repository in `out/repo` |

`make help` lists all targets.

## Before a pull request

```bash
make generate lint verify test test-integration
```

- Every behaviour change comes with a test. Controller tests run against a real NetBox; CEL rules are tested in `api/v1alpha1/validation_test.go`.
- Update the documentation in `docs/` when behaviour visible to users changes, and the README when the landing page does; `make verify` checks the links. Pages follow the goal, steps, check, next shape; the reference pages are tables.
- Follow the Cluster API conventions: conditions are `metav1.Condition`, logging uses `ctrl.LoggerFrom(ctx)` with `klog.KObj`, API fields follow the [Kubernetes API conventions](https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md) and kube-api-linter.
- New Go, shell, Makefile and Dockerfile files start with the license header from `hack/boilerplate` (`Copyright The cluster-api-ipam-provider-netbox Authors.`, without a year).
- Keep comments short; say why, not what.
- Keep the code and the examples generic: no installation-specific names, private addresses from RFC 1918 or documentation ranges, NetBox fields and tags set by users rather than hard-coded.

## Releasing

A new minor needs its release series in `metadata.yaml`; push a tag `vX.Y.Z` or `vX.Y.Z-rc.N` and the `release` workflow pushes the image and creates a draft GitHub release. The steps, with what to check, are in [Release](docs/development/release.md).

## Pull request titles

As in Cluster API, the title starts with an icon for the kind of change:

| Icon | Alias | Kind |
|---|---|---|
| ⚠️ | `:warning:` | breaking change |
| ✨ | `:sparkles:` | feature |
| 🐛 | `:bug:` | bug fix |
| 📖 | `:book:` | documentation or proposal |
| 🌱 | `:seedling:` | other: tests, build, dependencies |

Example: `🐛 Keep the old range bounds while a shrink waits`. One topic per pull request; squash its commits before merge.
