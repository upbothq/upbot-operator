# Releasing

Releases are made by the **Release** workflow (`.github/workflows/release.yml`), from `main`, never from a laptop.

## Make a release

From GitHub: Actions → Release → Run workflow, pick the bump (`patch`, `minor` or `major`).

From a terminal, with the GitHub CLI:

```sh
make release              # patch
make release BUMP=minor
gh run watch --repo upbothq/upbot-operator
```

## What the workflow does

1. Runs `make test`.
2. Bumps `VERSION`, sets `version` and `appVersion` in `dist/chart/Chart.yaml`, commits `chore(release): X.Y.Z` and tags `vX.Y.Z`.
3. Builds the image for `linux/amd64` and `linux/arm64` (Go cross-compiles, no emulation), with an SBOM and provenance, pushes `ghcr.io/upbothq/upbot-operator:vX.Y.Z` and signs it with cosign (keyless).
4. Pushes the release commit and the tag to `main`. If `main` moved during the run, the push fails before anything else is published: run the workflow again, it releases the same version.
5. Moves the `latest` image tag to the new image.
6. Pushes the chart to `oci://ghcr.io/upbothq/charts/upbot-operator`.
7. Creates the GitHub release with the chart archive and notes made from the commit subjects since the last tag.
8. Adds the version to `index.yaml` on `gh-pages`, the Helm repository Artifact Hub reads.

Everything runs with the workflow's `GITHUB_TOKEN`, no personal token is needed.

## Install a release

```sh
helm install upbot-operator oci://ghcr.io/upbothq/charts/upbot-operator --version X.Y.Z \
  --namespace upbot-operator-system --create-namespace
```

The gh-pages Helm repository keeps working for existing installs.

## Verify an image

```sh
cosign verify ghcr.io/upbothq/upbot-operator:vX.Y.Z \
  --certificate-identity-regexp '^https://github.com/upbothq/upbot-operator/\.github/workflows/release\.yml@' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

## Versions

- `VERSION` is the version of the last release; only the workflow changes it.
- The Lint workflow fails when `VERSION` and the chart disagree.
- Commit subjects become the release notes, so write them as [conventional commits](https://www.conventionalcommits.org/) (`feat:`, `fix:`, …).

## Test image

To try a build in a cluster without releasing, push it to your own registry:

```sh
make docker-buildx IMG=<registry>/upbot-operator:test
```
