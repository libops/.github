# Platform compatibility manifests

Every promoted platform release must publish a manifest conforming to
`platform-release.schema.json`. The manifest is the exact, reviewable set that
was tested together: sitectl and plugin package versions, source commits,
Compose template contract digest, cloud-compose preset commit, container image
digests and source commits, exact shared/caller publisher identities, verified
SPDX SBOMs for both native platforms, verified SLSA v1 provenance, the shared
smoke workflow commit, and links to contract and behavioral test evidence.

Validate a candidate before promotion:

```yaml
- uses: libops/.github/.github/actions/validate-platform-compatibility@FULL_40_CHARACTER_COMMIT_SHA
  with:
    manifest: .libops/platform-release.json
```

Tags without digests, images without exact source and attestation evidence,
movable source commits, incomplete native-platform SBOM coverage, missing
strict verifier checks, and evidence that is not a GitHub Actions run URL are
rejected. Application family and image service names must also be unique. A
candidate may be superseded; a promoted manifest is immutable and must be
marked `revoked` rather than edited if a released tuple proves unsafe.
