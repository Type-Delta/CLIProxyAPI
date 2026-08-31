# Dependency notices

CLIProxyAPI releases contain code from the Go modules recorded in
`go-modules.txt` and the CPAMC packages locked in `cpamc-bun.lock`. The release
workflow also publishes SPDX SBOMs for each binary archive and the CPAMC source
tree so a release can be matched to exact package versions and license data.

The CPA license is distributed as `LICENSE`. The maintained CPAMC fork's
license is distributed as `CPAMC-LICENSE`, and the upstream CPA Usage Keeper
license covering the ported analytics contracts is distributed as
`CPAUK-LICENSE`. Dependency-specific copyright and license terms remain in the
source packages identified by the module inventory and SBOMs.

Build provenance is published as `source-provenance.json`. Container releases
carry BuildKit provenance and SBOM attestations and use digest-pinned base
images.
