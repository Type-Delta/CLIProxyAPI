# Management center packaging

CPA pins the Type-Delta CPAMC fork as the required `web/management-center` submodule. The gitlink, not a branch name, selects the panel source used by local, CI, Docker, and release builds.

## Clone and verify

```bash
git clone --recurse-submodules https://github.com/Type-Delta/CLIProxyAPI.git
cd CLIProxyAPI
git submodule update --init --recursive
scripts/verify-submodules.sh
```

To repair an uninitialized checkout without changing the pinned commit:

```bash
git submodule sync --recursive
git submodule update --init --recursive
scripts/verify-submodules.sh
```

## Update the pinned panel

Make and validate CPAMC changes inside `web/management-center`, commit and push them to the Type-Delta CPAMC fork, and only then advance the CPA gitlink. Never commit a gitlink that refers only to an unpushed local commit.

The canonical single-file artifact is generated in the digest-pinned Bun 1.3.14
builder image so native Rolldown differences on the host cannot change release
bytes:

```bash
scripts/build-management-center.sh
```

That command replaces `internal/managementasset/bundled/management.html` and its
compatibility manifest. Commit those bytes with the later CPA gitlink commit.
Docker builds rebuild the submodule with the same pinned builder and compare the
result byte-for-byte with the bundled artifact. After the package cache is
primed, `Dockerfile.management` can rebuild with BuildKit networking disabled.

## Runtime selection and rollback

CPA serves a mutable `management.html` only when the adjacent `management-artifact.json` has a compatible Management API range and its SHA-256 matches. Otherwise, CPA serves the immutable panel embedded in the binary. The updater accepts the same verified pair and has no undigested network fallback.

To roll back an installed mutable panel, disable panel auto-updates, install a previously released `management.html` and its matching manifest together in the active `WRITABLE_PATH/static` or config-relative `static` directory, verify the digest, and restart CPA. Removing only those two named mutable files through the deployment platform also returns service to the embedded panel; do not delete the static directory or analytics data.
