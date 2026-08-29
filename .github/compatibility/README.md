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

`platform-release.owners.json` assigns every leaf field in the schema to one
accountable owner. The validator derives the leaf paths from the schema and
fails if the owner map is missing a field, names an extra field, duplicates a
path, or uses an unrecognized owner. `application-family-owner` resolves through
the required `family` value to exactly one specialist skill in the same file.
Schema changes and ownership changes therefore cannot drift independently.

## Signing and approval ownership

- `libops-devsecops` produces candidate manifests, owns the keyless promoted-
  manifest signer, and verifies its signature.
- `libops-platform-coo` approves promotion after the release gates are met.
- The resolved application-family owner approves that application's source,
  package, template, image, and contract evidence.
- `libops-site-reliability` approves smoke, recovery, and hosted-canary
  evidence.

These are accountable roles, not long-lived signing keys. The promoted-manifest
signer must use a short-lived GitHub Actions OIDC identity bound to a reviewed,
SHA-pinned workflow. A candidate manifest is not promoted merely because it
validates. Until that signer and its verification receipt are present, retain
the manifest as `candidate`; do not represent it as an immutable signed release.
