# LibOps shared GitHub Actions

This repository owns the reusable delivery workflows used by LibOps repositories. Callers must pin reusable workflows to a full commit SHA so a reviewed workflow—not a movable branch or tag—defines every privileged publication.

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
