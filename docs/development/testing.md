# Testing

Goal: run the checks of the repository and know what each one covers, including the two key modes of the integration tests: without an instance name and with one.

## Steps

```bash
make generate lint verify test          # no NetBox needed
make dev-up                             # or make netbox-up, for the controller tests
make test-integration
NETBOX_TEST_MODE=instance make test-integration
```

| Target | What it runs |
|---|---|
| `make test` | unit tests, the CRD validation on envtest, and the shell test of the control-plane VIP command (`hack/test-control-plane-vip.sh`, which runs the command of [`docs/examples/control-plane-vip.yaml`](../examples/control-plane-vip.yaml) on kubeadm configs shaped as Cluster API v1.14 renders them). No NetBox needed: the controller tests skip with `NETBOX_URL not set` |
| `make test-integration` | `go test ./...` with the controller tests running against the NetBox of `DEV_ENV_FILE` (`hack/dev/.env`, `make netbox-up` first). It does not run the VIP command test, which is part of `make test`. `TEST_LOG=1` prints the provider logs |
| `make verify` | generated files and `go.mod`/`go.sum` are up to date, license headers, and the relative links and anchors of README.md and `docs` (`hack/verify-doc-links.sh`) |
| `make lint` | golangci-lint, and kube-api-linter on `api/` |
| `make generate` | deepcopy, CRDs and RBAC |

Every behaviour change comes with a test. Controller tests run against a real NetBox with envtest for the Kubernetes API; CEL rules are tested in `api/v1alpha1/validation_test.go`; the flags in `main_test.go`. The test environment is installed by `make`: `setup-envtest` with Kubernetes `ENVTEST_K8S_VERSION` (1.37.0), and the Cluster API CRDs and the `InClusterIPPool` CRD of in-cluster IPAM from the module cache. envtest runs no in-cluster controller or webhook: the tests that need its finalizer or its refusals set them up themselves.

To run one test, source the stand's env file and set what the Makefile sets:

```bash
set -a; source hack/dev/.env; set +a
KUBEBUILDER_ASSETS="$(hack/tools/bin/setup-envtest-v0.25.0 use 1.37.0 -p path)" \
  CAPI_CRDS="$(go list -m -f '{{.Dir}}' sigs.k8s.io/cluster-api)/core/config/crd/bases" \
  INCLUSTER_CRDS="$(go list -m -f '{{.Dir}}' sigs.k8s.io/cluster-api-ipam-provider-in-cluster)/config/crd/bases" \
  go test ./internal/controllers -run TestNetworkSyncsAttrs -count=1
```

The tests share one NetBox and its parent prefixes: run one integration run at a time, and make sure no earlier test binary or envtest process is left.

## The two key modes

The controller suite runs the provider in the mode `NETBOX_TEST_MODE` names. It is a variable of its own and not part of `hack/dev/.env`: the Makefile sets it after the env file is sourced, so a value in the file is ignored. The suite does not read `NETBOX_INSTANCE`: the `dev/` keys of the dev stand stay apart from those of the tests.

| `NETBOX_TEST_MODE` | Keys |
|---|---|
| empty (default) | no instance name: `NetBoxNetwork/<ns>/<name>`, in the text field `capi_ref` |
| `instance` | the instance `ci`: `ci/NetBoxNetwork/<ns>/<name>`, in the same field |

The tests build their keys from the suite's settings, so the whole suite runs in both modes. The suite prints the mode it runs in (`NetBox test mode: instance "ci", ref capi_ref`). Any other value stops the run. Owners and tenants are made by the tests themselves, one zone per test; the seed gives the operator token what they need ([Dev stand](dev-stand.md#what-the-seed-creates)).

## CI

| Workflow | Runs |
|---|---|
| `pr-test` | `make test` |
| `pr-verify` | `make verify` |
| `pr-golangci-lint` | `make lint` |
| `pr-integration` | `make netbox-up` and `make test-integration` on NetBox `v4.6.7-5.0.2` and `v4.7.2-5.1.1` in the default mode, and on `v4.6.7-5.0.2` in the `instance` mode |
| `image` | `make docker-build-all`: validates the linux/amd64 and linux/arm64 build |
| `weekly-govulncheck` | `govulncheck` every Monday |

A second NetBox version on your machine: [Dev stand](dev-stand.md#a-second-netbox-472).

## Link check

`hack/verify-doc-links.sh` checks that every relative link and anchor in README.md, CONTRIBUTING.md and `docs/**/*.md` resolves, using the rules of GitHub: an anchor is the heading in lower case, spaces as `-`, and everything but letters, digits, `-` and `_` dropped. A link to a directory is accepted, external links are not checked, files ignored by git are left out. It needs bash, git, awk and grep.

## Check

`make verify lint test` is green, and `make test-integration` is green in both modes on the dev NetBox. Before a pull request: `make generate lint verify test test-integration` ([CONTRIBUTING](../../CONTRIBUTING.md)).

## Next

- [Dev stand](dev-stand.md) and [Release](release.md).
