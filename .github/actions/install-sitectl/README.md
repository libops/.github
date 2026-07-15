# Install sitectl action

This composite action installs `sitectl` apt packages from
`packages.libops.io`. Supply a one-to-one exact SemVer map for repeatable
release and smoke-test jobs:

```yaml
- uses: libops/.github/.github/actions/install-sitectl@FULL_40_CHARACTER_COMMIT_SHA
  with:
    packages: sitectl sitectl-omeka-s
    package-versions: sitectl=0.39.0 sitectl-omeka-s=0.6.0
    allow-unversioned: false
```

`allow-unversioned` defaults to `true` only for existing callers. Compatibility
installs emit a workflow warning and resolve the newest package available at
run time.

## Archive-key rotation

The action vendors `sitectl-archive-keyring.asc` and checks its complete set of
primary fingerprints before dearmoring it. It never downloads its trust root
from the package endpoint. The currently approved primary fingerprint is:

```text
FBF887BCE093167F499F537BCFB2A9DBD0A2156A
```

The reviewed ASCII key file has SHA-256
`caa22fc0474b2f0934ee0da2749265db297b0e3e74ee0e994c3225900a04ff59`;
the action contract test pins both values.

Rotate the key through a dedicated reviewed change:

1. Verify the replacement fingerprint through an independent LibOps channel.
2. Replace the ASCII keyring, update the approved fingerprint array in
   `install.sh`, and update its contract-test constant together.
3. Keep the prior key in the repository keyring and keep repository metadata
   valid for it during a migration window if SHA-pinned callers still need to
   install newly published packages.
4. Merge the action change and capture the resulting main commit SHA. In a
   separate PR, pin reusable workflows to that merged SHA. Do not pin a commit
   from a squash-merged PR because GitHub discards that commit from main.

Callers pinning older action commits deliberately retain the older trust root;
coordinate package-signing overlap and caller migration before retiring it.
