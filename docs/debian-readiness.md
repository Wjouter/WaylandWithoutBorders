# Debian Readiness

This document tracks what remains before `mwb-linux` is a credible candidate
for Debian upload and later Ubuntu sync.

## Current status

- Upstream release exists: `v0.4.1`.
- License is MIT.
- Go module has one binary and two external Go dependencies.
- The module now builds on Go 1.22 with Debian/Ubuntu-packaged dependency
  versions:
  - `github.com/BurntSushi/toml v1.3.2`
  - `golang.org/x/crypto v0.19.0`
- `debian/` packaging is present but deliberately marked `UNRELEASED`.
- The package name is `mwb-linux`; binary installed in `$PATH` is `mwb`.
- Current readiness: binary and source packages build from the Debian packaging
  branch. Source upload remains blocked on external Debian process steps:
  ITP, final Debian unstable review, and sponsor feedback.

## Recommended next move

Use an upstream-first path:

1. Use the `debian/sid` branch for Debian packaging work.
2. File the ITP bug against `wnpp`.
3. Add the real ITP close line to `debian/changelog`.
4. Move `debian/changelog` from `UNRELEASED` to `unstable`.
5. Upload the source package to mentors.debian.net and file RFS.

## Packaging choices

- Source format: `3.0 (quilt)`.
- Build helper: `dh-golang`.
- Runtime package installs:
  - `/usr/bin/mwb`
  - systemd user service
  - udev rules for `/dev/uinput` and `mwb-mouse` libinput flat profile
  - man page, README, architecture docs, and example config
- Autopkgtest currently performs a superficial CLI smoke test.
- Debian source package local artifacts are ignored via `debian/source/options`
  so `dist/`, `graphify-out/`, and local assistant state do not enter source
  diffs.
- The `debian/sid` branch repacks the `v0.4.1` upstream tarball as `+ds` and
  excludes the upstream `debian/` directory from the orig source.

## Verification ledger

2026-06-21 on an Ubuntu Noble x86_64 test host:

- `go test ./...` passed with `/usr/lib/go-1.22/bin/go`.
- `dpkg-checkbuilddeps` passed against Ubuntu-packaged build dependencies.
- `dpkg-buildpackage -us -uc -b` produced
  `mwb-linux_0.4.1-1_amd64.deb`.
- The Debian build ran package tests through `dh_auto_test`.
- The built binary is PIE and dynamically linked with generated `libc6`
  dependency.
- Package contents include `/usr/bin/mwb`, user systemd unit, udev rules,
  README/architecture docs, example config, copyright, changelog, and man page.
- Extracted-package smoke check passed: `mwb -h` prints the expected CLI flags.
- `v0.4.1` GitHub release assets were published and verified:
  - `checksums.txt`
  - `mwb-linux-amd64`
  - `mwb-linux-arm64`
  - `mwb-linux_0.4.1_amd64.deb`
  - `mwb-linux_0.4.1_arm64.deb`
- Published asset checksums passed, the amd64 binary printed expected `mwb -h`
  output, and both release `.deb` files use the public GitHub noreply
  maintainer address.

2026-06-21 on Debian unstable via container, using clean `debian/sid` clone:

- `uscan --download-current-version --force-download` repacked the `v0.4.1`
  upstream tarball as `mwb-linux_0.4.1+ds.orig.tar.xz`, deleting upstream
  `debian/` files from the orig source.
- `dpkg-buildpackage -S -us -uc` produced
  `mwb-linux_0.4.1+ds-1_source.changes`.
- `lintian mwb-linux_0.4.1+ds-1_source.changes` completed with no findings.
- Debian unstable binary build from the source package completed and ran package
  tests through `dh_auto_test`.
- `lintian mwb-linux_0.4.1+ds-1_amd64.changes` completed with one expected
  warning: `initial-upload-closes-no-bugs`.

Known verification limits:

- Ubuntu Noble lintian warns `unknown-field Static-Built-Using`; Debian
  unstable lintian accepts the field.
- Debian unstable lintian warns `initial-upload-closes-no-bugs` until a real
  ITP bug exists.

## Acceptance blockers to close before upload

- File an ITP bug and add its real `Closes: #nnnnnn` entry to
  `debian/changelog`.
- Decide whether to maintain under the Debian Go Packaging Team on Salsa.
- Repeat the Debian unstable build in the final maintainer environment
  (`sbuild`, `pbuilder`, or a sponsor-preferred equivalent) before upload.
- Re-run `lintian` after adding the real ITP bug; fix any new findings.
- Review trademark-safe wording around Microsoft PowerToys Mouse Without
  Borders compatibility.
- Verify source provenance for `docs/assets/banner.png` and `docs/assets/logo.png`
  or exclude nonessential generated artwork from the upstream tarball if a
  sponsor objects.
- Add a stronger autopkgtest if practical. Full bidirectional behavior needs a
  graphical/input-device environment, so the first Debian upload may reasonably
  keep the smoke test superficial.

## Sponsor path

1. Publish the packaging branch and make it build reproducibly.
2. Create a clean source package from a matching upstream release tarball.
3. Open an ITP bug against `wnpp`.
4. Add the real ITP close line and move `debian/changelog` from `UNRELEASED`
   to `unstable`.
5. Upload the source package to mentors.debian.net.
6. File an RFS bug against `sponsorship-requests`.
7. Respond to sponsor review, especially around naming, udev rules, service
   behavior, and testability.
