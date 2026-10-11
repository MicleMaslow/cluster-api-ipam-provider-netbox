# Release

Goal: publish a release: the image on ghcr.io and the manifests clusterctl installs, as a GitHub release.

## Steps

1. **A new minor needs its release series** in `metadata.yaml`, with the Cluster API contract it follows; `make release` refuses a tag without one. Update the compatibility table of the README.

2. **Build it locally first.** `RELEASE_TAG` is `vX.Y.Z`, or `vX.Y.Z-{alpha,beta,rc}.N`:

   ```bash
   make release RELEASE_TAG=v0.1.0-rc.0
   ```

   It writes `out/ipam-components.yaml` (the manifests with the image `<REGISTRY>/cluster-api-ipam-provider-netbox:<tag>`), `out/metadata.yaml`, and a local clusterctl repository in `out/repo`. Installing from it is [A build of your own](../getting-started/install.md#a-build-of-your-own); on the dev stand see [Dev stand](dev-stand.md#run-inside-kind).

3. **Push a tag** `vX.Y.Z` or `vX.Y.Z-rc.N` on a commit of `main` whose workflows are all green, both NetBox versions and the instance leg included: the `release` workflow runs no tests. It:

   - builds the manifests with `REGISTRY=ghcr.io/<owner>` (lower case);
   - pushes the image `ghcr.io/<owner>/cluster-api-ipam-provider-netbox:<tag>` for linux/amd64 and linux/arm64 (`make docker-push-all`), with the tag as the version baked into the binary;
   - creates a draft GitHub release with `ipam-components.yaml`, `metadata.yaml` and generated notes, marked as a pre-release when the tag has a suffix.

4. **After the first push of the image,** set the package `cluster-api-ipam-provider-netbox` to Public in its GitHub package settings; new ghcr packages are private.

5. **Edit the notes and publish the draft.** The generated notes list merged pull requests only: write the alpha status, the compatibility table of the README and a link to [Install](../getting-started/install.md) by hand. The repository must be public before the draft is published: clusterctl and the operator download the assets without a token, and they see only published releases.

| Variable | Default | Meaning |
|---|---|---|
| `REGISTRY` | `ghcr.io/miclemaslow` | the image registry and path |
| `IMAGE_NAME` | `cluster-api-ipam-provider-netbox` | the image name |
| `TAG` | `RELEASE_TAG`, else `dev` | the image tag |
| `IMG` | `<REGISTRY>/<IMAGE_NAME>:<TAG>` | the whole image reference |
| `PLATFORMS` | `linux/amd64,linux/arm64` | for `docker-build-all` and `docker-push-all` |

`make release-manifests IMG=...` writes the manifests for any image without the tag checks. The kustomization in `config/default` keeps the default image; the Makefile sets the image on a copy.

## Check

```bash
docker buildx imagetools inspect ghcr.io/<owner>/cluster-api-ipam-provider-netbox:<tag>
clusterctl init --ipam netbox
```

The image lists both platforms, and `clusterctl init --ipam netbox` (with the provider registered as in [Install](../getting-started/install.md#steps)) installs the new release; the log line `starting manager` of the provider shows the version.

## Next

- [Upgrade](../operations/upgrade.md): what your users do with a new release.
- [Testing](testing.md).
