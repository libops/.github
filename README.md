# LibOps shared GitHub Actions

This repository owns the reusable delivery workflows used by LibOps repositories. Callers must pin reusable workflows to a full commit SHA so a reviewed workflow—not a movable branch or tag—defines every privileged publication.

## sitectl releases

`.github/workflows/sitectl-plugin-goreleaser.yaml` has two release jobs:

- `release` validates one exact release tag, checks out that tag, and runs
  GoReleaser. GoReleaser publishes both the GitHub release and the Homebrew
  formula described by the caller's `.goreleaser.yaml`. It receives the caller
  repository's automatic token plus the separately scoped `HOMEBREW_REPO`
  credential as `HOMEBREW_REPO_TOKEN`.
- `publish-linux-packages` receives the GCP identity permission. It is the only
  custom post-release publisher. Before requesting a cloud identity, it checks
  out the package publisher at an exact reviewed commit, verifies that checkout,
  applies the publisher-mandatory exclusions, and builds a commit-named local
  tools image. Plugin callers cannot add exclusions. Credentialed publication
  invokes the publisher's validated environment wrapper directly and verifies
  the local image ID instead of resolving a mutable image or passing inputs
  through Make.

`full` runs both jobs either from a `vMAJOR.MINOR.PATCH` tag with no
`release-version` input or from the source default branch with an exact
`release-version`. The latter is the secure recovery path for a tag whose
GitHub release was published before its assets were uploaded. Tag-triggered
publication may create an absent release, while default-branch recovery requires
the exact release to already exist. The preflight rejects draft, prerelease, or
immutable releases and any release that already has assets. Both paths check out
`refs/tags/<version>`, verify the checkout resolves to that tag's commit, and
require the commit to be reachable from the source default branch.
`packages-only` republishes an existing stable release to the Linux package
repository and is subject to the same default-branch gate.

Callers must pass `HOMEBREW_REPO` as a fine-grained token limited to the
contents operations GoReleaser needs in `libops/homebrew`, plus pull-request
access when the caller enables GoReleaser's pull-request mode. Continue to pin
this reusable workflow to a full commit SHA. Force every tag dispatch to `full`
in the reusable-workflow call; this prevents a no-input dispatch created by the
release bump workflow from inheriting the manual package-recovery default:

```yaml
on:
  workflow_dispatch:
    inputs:
      release-mode:
        description: Full release or Linux-package recovery
        required: true
        type: choice
        default: packages-only
        options:
          - full
          - packages-only
      release-version:
        description: Exact vMAJOR.MINOR.PATCH tag for recovery
        required: false
        type: string
        default: ""

jobs:
  release:
    uses: libops/.github/.github/workflows/sitectl-plugin-goreleaser.yaml@FULL_40_CHARACTER_COMMIT_SHA
    permissions:
      contents: write
      id-token: write
    secrets:
      HOMEBREW_REPO: ${{ secrets.HOMEBREW_REPO }}
    with:
      package-name: sitectl-example
      release-mode: ${{ github.ref_type == 'tag' && 'full' || inputs.release-mode }}
      release-version: ${{ inputs.release-version || '' }}
      sitectl-ref: 65cfde137a58ba14aaa9a1512d88b943888872f3
```

The shared default uses that same immutable sitectl v1 commit. Callers should
still pass it explicitly so dependency upgrades remain visible in their own
review history; never point a release build at `main`.

## sitectl create smoke tests

`.github/workflows/sitectl-create-smoke-test.yaml` exercises a template with the
published `sitectl` host and plugin packages. Release and compatibility tests
should pass an exact SemVer for every package so a rerun exercises the same CLI
bits:

```yaml
jobs:
  smoke:
    uses: libops/.github/.github/workflows/sitectl-create-smoke-test.yaml@FULL_40_CHARACTER_COMMIT_SHA
    with:
      plugin: omeka-s
      packages: sitectl sitectl-omeka-s
      package-versions: sitectl=0.39.0 sitectl-omeka-s=0.6.0
      allow-unversioned-packages: false
```

`package-versions` is a one-to-one `package=version` map: missing, duplicate,
extra, or non-SemVer assignments fail before apt runs. The reusable workflow
retains `allow-unversioned-packages: true` only for existing callers. That mode
emits a warning and installs whatever version is newest in the apt repository at
run time; it is unsuitable for a release gate.

The workflow invokes the repository's composite install action through an
immutable commit SHA. Because this repository squash-merges pull requests, an
action contract change and its reusable-workflow adoption require two green
PRs. Merge the action first, capture the resulting main SHA, then pin the
workflow to that merged SHA in the second PR. Only advertise the second PR's
merged workflow SHA to callers.

## Pull request status aggregation

`.github/workflows/pr-status.yaml` provides one credential-free branch-protection
check for a caller's real pull-request jobs. Give the caller job the ID and name
`run`, make it depend on every required job, run it with `always()`, and pass the
complete `toJSON(needs)` value:

```yaml
jobs:
  lint:
    runs-on: ubuntu-24.04
    steps:
      - run: make lint

  test:
    runs-on: ubuntu-24.04
    steps:
      - run: make test

  run:
    name: run
    if: ${{ always() }}
    needs: [lint, test]
    permissions: {}
    uses: libops/.github/.github/workflows/pr-status.yaml@FULL_40_CHARACTER_COMMIT_SHA
    with:
      needs-json: ${{ toJSON(needs) }}
```

The reusable job is named `merge`, so this caller receives the exact check name
`run / merge`. Require that check in branch protection. It rejects malformed or
empty input and fails unless every dependency result is exactly `success`; a
failed, cancelled, or skipped dependency therefore blocks the merge. Do not put
secrets in job outputs because `toJSON(needs)` includes those outputs, even
though this workflow neither prints nor publishes the input.

## Container publication

`.github/workflows/build-push.yaml` builds native architecture images and assembles one multi-platform manifest. The primary registry defaults to GHCR. Set `additional-gar-registry` when the same image also needs to run in Google Cloud. Both registries receive manifests assembled from the exact native digests produced by the same workflow run.

Set `additional-image-names` to a JSON array such as `["dash"]` when multiple package names are aliases of the same Dockerfile and context. The workflow builds once, scans once when scanning is enabled, fans the native content out to every primary/GAR repository, and fails unless every final alias resolves to the same manifest digest. Aliases therefore do not need duplicate reusable-workflow jobs.

The publication modes are:

- The compatibility mode (`scan: false` and no `expected-main-sha`) may write a BuildKit registry cache while it pushes an attempt-specific native image.
- A scanned or main-tip-guarded build first loads the image locally. It may read `cache-from`, but it cannot export a registry cache or image until the scan and current-main check pass.
- `expected-main-sha` is checked immediately before each native image, final manifest, and signature write. This narrows the race but does not make two independent registries transactional.

Digest artifact names are stable for a workflow run and architecture. A failed-job rerun can therefore combine an untouched digest from attempt 1 with a repaired digest from attempt 2. Rerun jobs overwrite only their own artifact. Attempt numbers appear only in temporary native registry tags; the merge consumes immutable digests.

An always-run cleanup job retains all run-scoped staging tags when a build or merge fails so a failed-job rerun can reuse the successful native digest. After a verified merge, cleanup is best effort and cannot change the publication result. For GAR, the workflow calls only the Artifact Registry tag resource's delete method. The publisher identity therefore needs a repository-scoped custom role containing only `artifactregistry.tags.delete`; `roles/artifactregistry.writer` does not include that permission. Artifact Registry does not expose supported resource-name IAM condition attributes for restricting this permission to a tag prefix, so the custom role can delete any tag in that repository. GHCR and registries without a conditional tag-only delete retain the exact `ci-*` tags and emit a workflow warning rather than risking a published native version.

Builds always run on the fixed `ubuntu-24.04` and `ubuntu-24.04-arm` GitHub-hosted runners, and the merge always consumes the fixed `amd64` and `arm64` platforms. The deprecated `runners` input remains for caller compatibility, but only its default pair is accepted; it cannot select a self-hosted or arbitrary runner with publisher credentials.

### Caller secrets

The reusable workflow declares four optional secrets so callers can pass only what their registry needs. GHCR uses the caller's automatic `GITHUB_TOKEN` and needs none of these secrets. GAR callers explicitly map `GCLOUD_OIDC_POOL` and `GSA`; Docker Hub callers explicitly map `DOCKERHUB_USER` and `DOCKERHUB_PASSWORD`. Do not use `secrets: inherit`.

```yaml
jobs:
  image:
    uses: libops/.github/.github/workflows/build-push.yaml@FULL_40_CHARACTER_COMMIT_SHA
    secrets:
      GCLOUD_OIDC_POOL: ${{ secrets.GCLOUD_OIDC_POOL }}
      GSA: ${{ secrets.GSA }}
```

### Keyless signatures

Signing is backward compatible and disabled by default. To enable it, the caller grants `id-token: write`, sets `sign: true`, and supplies the exact Fulcio URI for the SHA-pinned reusable workflow in `certificate-identity`:

```yaml
permissions:
  contents: read
  id-token: write
  packages: write

jobs:
  image:
    uses: libops/.github/.github/workflows/build-push.yaml@FULL_40_CHARACTER_COMMIT_SHA
    with:
      image: example
      additional-image-names: '["example-dashboard"]'
      scan: true
      sign: true
      certificate-identity: https://github.com/libops/.github/.github/workflows/build-push.yaml@FULL_40_CHARACTER_COMMIT_SHA
```

The pinned official Cosign installer signs each final manifest by digest in every configured registry, then verifies it before the job can succeed. Before writing a signature, the workflow validates the live GitHub OIDC token's issuer, audience, called workflow, caller workflow, repository, ref, and commit claims. Fulcio's certificate identity represents the called reusable workflow (`job_workflow_ref`). The signed `caller-workflow-ref` annotation records the exact caller workflow path and ref; verification also requires the certificate's caller repository, ref, and commit extensions. This preserves both trust boundaries instead of treating the shared builder as the caller.

For example, verify an API image with the immutable digest and the values from its publication run:

```bash
cosign verify "ghcr.io/libops/api@sha256:IMAGE_DIGEST" \
  --certificate-identity "https://github.com/libops/.github/.github/workflows/build-push.yaml@SHARED_WORKFLOW_COMMIT" \
  --certificate-oidc-issuer "https://token.actions.githubusercontent.com" \
  --certificate-github-workflow-repository "libops/api" \
  --certificate-github-workflow-ref "refs/heads/main" \
  --certificate-github-workflow-sha "CALLER_COMMIT" \
  -a "caller-workflow-ref=libops/api/.github/workflows/images.yml@refs/heads/main"
```

These are image signatures, not provenance attestations. A consumer that requires build provenance must add and verify a separately defined attestation policy.
