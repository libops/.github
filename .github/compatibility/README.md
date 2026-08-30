# Platform compatibility manifests

Every promoted platform release must publish a manifest conforming to
`platform-release.schema.json`. Version 2 is the first-customer release
contract. The manifest is the exact, reviewable set that was tested together:

- the Terraform source commit;
- all 17 required API, controller, runner, Vault, edge, PPB, Task Agent, and
  sandbox images, each with source, digest, managed publisher identity, exact
  runtime-resolved builder commit, SBOM, provenance, and contract-test evidence;
- the canonical skills source commit and embedded-manifest digest;
- hosted onboarding, GitHub-install, Slack-install, Vault-recovery,
  edge-routing, Task Agent, MariaDB-recovery, and rollback runs;
- sitectl and plugin package versions, source commits, Compose template
  contract digests, cloud-compose preset commits, and the shared smoke workflow
  commit.

Validate a candidate before promotion:

```yaml
- uses: libops/.github/.github/actions/validate-platform-compatibility@main
  with:
    manifest: .libops/platform-release.json
```

Tags without digests, missing first-customer images, images without exact
source and attestation evidence, mutable skills sources, incomplete hosted
evidence, movable source commits, incomplete native-platform SBOM coverage,
missing strict verifier checks, and evidence that is not a GitHub Actions run
URL are rejected. Application family and image service names must also be unique. A
candidate may be superseded; a promoted manifest is immutable and must be
marked `revoked` rather than edited if a released tuple proves unsafe.

`platform-release.owners.json` assigns every leaf field in the schema to one
accountable owner. The validator derives the leaf paths from the schema and
fails if the owner map is missing a field, names an extra field, duplicates a
path, or uses an unrecognized owner. `application-family-owner` and
`platform-component-owner` resolve through the required family or component to
exactly one specialist skill in the same file. Schema changes and ownership
changes therefore cannot drift independently.

## Signing and approval ownership

- `libops-devsecops` produces candidate manifests, owns the keyless promoted-
  manifest signer, and verifies its signature.
- `libops-platform-coo` approves promotion after the release gates are met.
- The resolved application-family owner approves that application's source,
  package, template, image, and contract evidence.
- The resolved platform-component owner approves that runtime image's source
  and contract evidence.
- `libops-site-reliability` approves smoke, recovery, and hosted-canary
  evidence.

These are accountable roles, not long-lived signing keys. The promoted-manifest
signer must use a short-lived GitHub Actions OIDC identity bound to the reviewed
managed workflow ref and record the exact resolved workflow commit. A candidate
manifest is not promoted merely because it validates. Until that signer and its
verification receipt are present, retain the manifest as `candidate`; do not
represent it as an immutable signed release.

## Publication evidence input

Every signed invocation of the shared `build-push` workflow uploads a
`publication-record-...` artifact for 90 days and exposes its artifact ID,
GitHub-reported artifact digest, and authenticated URL as reusable-workflow
outputs. The JSON record contains the exact caller source commit, managed
publisher identity, resolved publisher commit, caller workflow, hosted run,
native image digests, final tag-plus-digest references, and verified SBOM and
provenance evidence for every registry alias.

The record is generated only after every final image has the same digest and
its signature and attestations verify. Manifest producers should consume these
records, add component-owned contract evidence, and validate the assembled
candidate. Do not copy the resolved publisher commit back into a caller's
`uses:` reference: LibOps callers stay on the managed `@main` channel, while
the generated record preserves the immutable identity used for release and
rollback evidence.
